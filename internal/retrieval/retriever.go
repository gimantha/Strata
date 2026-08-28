package retrieval

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding"
	"github.com/gimantha/strata/internal/llm"
	"github.com/gimantha/strata/internal/store/index"
)

// TraceSink persists query-time explainability, when the deployment wants it
// (AGENTS.md section 6.12).
type TraceSink interface {
	RecordTrace(ctx context.Context, trace domain.RetrievalTrace) (domain.RetrievalTrace, error)
}

// Store is the projection surface retrieval reads, declared by its consumer.
// Ledger is the canonical material retrieval reads directly, declared by its consumer.
//
// One method, and it is not a mistake that it is small. Resolving a name to an identity is
// a canonical lookup — it joins entities to entity_aliases and touches no projection — so
// the entity leg of retrieval stays on the ledger however the indexes are configured. Three
// of the five retrieval modes are substitutable; this is where the other two live.
type Ledger interface {
	FindEntitiesByName(ctx context.Context, scope domain.Scope, name string) ([]domain.Entity, error)
	// GetEntities names the identities a graph traversal reached. Batched, because the
	// alternative is one lookup per hit.
	GetEntities(ctx context.Context, ws domain.WorkspaceID, ids []domain.EntityID) (map[domain.EntityID]domain.Entity, error)
}

// Retriever runs the planned candidate generators and fuses their results.
type Retriever struct {
	planner  Planner
	ledger   Ledger
	indexes  index.Set
	embedder embedding.Embedder
	weights  Weights
	traces   TraceSink
	redact   bool
	now      func() time.Time
	logger   *slog.Logger
	tracer   trace.Tracer
}

// Options configures retrieval.
type Options struct {
	Weights Weights
	Now     func() time.Time
	// Traces persists what each query considered and returned. Optional: a deployment
	// that does not want query text retained simply does not configure one, and one that
	// does can still redact the text per request.
	Traces TraceSink
	// RedactQueryText stores the hash of a query without its words, for deployments where
	// what people asked is itself sensitive (AGENTS.md section 6.12).
	RedactQueryText bool

	// PlanningModel turns on LLM query planning. Nil keeps the heuristic planner, which is
	// also the fallback whenever a configured model cannot answer: retrieval must work
	// without a model at query time (AGENTS.md section 19.4).
	PlanningModel llm.LLM
	// PlanningTimeout bounds the planning call. Short by default, because a planner that
	// makes a query slower than the retrieval it was meant to improve is not worth having.
	PlanningTimeout time.Duration
}

// New builds a retriever. The embedder may be nil, in which case the vector leg is skipped
// and the plan records why.
func New(ledger Ledger, indexes index.Set, embedder embedding.Embedder, opts Options,
	logger *slog.Logger, tracer trace.Tracer) *Retriever {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("retrieval")
	}
	heuristic := heuristicPlanner{hasEmbedder: embedder != nil}
	var chosen Planner = heuristic
	if opts.PlanningModel != nil {
		timeout := opts.PlanningTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		chosen = llmPlanner{
			model: opts.PlanningModel, fallback: heuristic, timeout: timeout,
			logger: logger, hasVector: embedder != nil,
		}
	}

	return &Retriever{
		planner:  chosen,
		ledger:   ledger,
		indexes:  indexes,
		embedder: embedder,
		weights:  opts.Weights.withDefaults(),
		traces:   opts.Traces,
		redact:   opts.RedactQueryText,
		now:      now,
		logger:   logger,
		tracer:   tracer,
	}
}

// Query runs retrieval end to end.
func (r *Retriever) Query(ctx context.Context, req domain.QueryRequest) (domain.QueryResult, error) {
	ctx, span := r.tracer.Start(ctx, "retrieval.Query", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(req.Scope.WorkspaceID)),
	))
	defer span.End()

	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return domain.QueryResult{}, err
	}

	plan := r.planner.Plan(ctx, req)

	queryStarted := r.now()

	var (
		candidates []candidate
		// Entities found by the precise retrievers seed graph expansion, which is what
		// section 19.5 means by using retrieved entities as entry points.
		seeds []domain.EntityID
	)

	// One retrieval per planned search. A heuristic plan issues exactly one — the question
	// as asked — so this is the same single pass it always was. A planner that reshaped the
	// question issues several, and fusion merges them: a record found by two searches earns
	// two contributions and ranks above one found by either alone, which is the whole point
	// of asking more than once.
	//
	// Graph is held back until every other search has run, because it expands from what the
	// others found and seeds gathered from one sub-query are just as good a starting point
	// for another (AGENTS.md section 19.5).
	for _, sub := range plan.SubQueries {
		searched := req
		searched.Query = sub.Text

		found, err := r.runModes(ctx, searched, &plan, seedingModes(plan.Modes), nil)
		if err != nil {
			return domain.QueryResult{}, err
		}
		for _, c := range found {
			if c.hit.Surface == domain.SurfaceEntity {
				seeds = append(seeds, domain.EntityID(c.hit.RecordID))
			}
		}
		candidates = append(candidates, found...)
	}

	if slices.Contains(plan.Modes, domain.ModeGraph) {
		found, err := r.runModes(ctx, req, &plan,
			[]domain.RetrievalMode{domain.ModeGraph}, seeds)
		if err != nil {
			return domain.QueryResult{}, err
		}
		candidates = append(candidates, found...)
	}

	items := fuse(candidates, r.weights, req.Limit)
	result := domain.QueryResult{Items: items, Total: countDistinct(candidates)}
	if req.Explain {
		result.Plan = &plan
	}
	result.TraceID = r.record(ctx, req, candidates, items, r.now().Sub(queryStarted))

	span.SetAttributes(
		attribute.Int("strata.results", len(items)),
		attribute.Int("strata.candidates", result.Total),
		attribute.Int("strata.sub_queries", len(plan.SubQueries)),
		attribute.String("strata.planner", plan.Planner),
	)
	if r.logger != nil {
		r.logger.DebugContext(ctx, "retrieval complete",
			slog.Int("candidates", result.Total),
			slog.Int("results", len(items)),
			slog.Int("sub_queries", len(plan.SubQueries)),
			slog.String("planner", plan.Planner))
	}
	return result, nil
}

// seedingModes is every mode except graph, in plan order.
func seedingModes(modes []domain.RetrievalMode) []domain.RetrievalMode {
	out := make([]domain.RetrievalMode, 0, len(modes))
	for _, mode := range modes {
		if mode != domain.ModeGraph {
			out = append(out, mode)
		}
	}
	return out
}

// runModes executes one search across the named retrievers.
func (r *Retriever) runModes(ctx context.Context, req domain.QueryRequest,
	plan *domain.RetrievalPlan, modes []domain.RetrievalMode,
	seeds []domain.EntityID) ([]candidate, error) {
	var candidates []candidate

	for _, mode := range modes {
		// A mode whose index is not configured is skipped rather than failed. The planner
		// picks modes from the query's shape, not from the deployment's, so a deployment
		// running without one projection would otherwise have every query fail instead of
		// answering from the legs it does have.
		if !r.available(mode) {
			continue
		}

		started := r.now()

		var (
			found []candidate
			err   error
		)
		switch mode {
		case domain.ModeLexical:
			found, err = r.lexical(ctx, req, false)
		case domain.ModeExact:
			found, err = r.lexical(ctx, req, true)
		case domain.ModeVector:
			found, err = r.vector(ctx, req)
		case domain.ModeEntity:
			found, err = r.entity(ctx, req)
		case domain.ModeGraph:
			found, err = r.graph(ctx, req, seeds)
		}
		if err != nil {
			return nil, err
		}

		candidates = append(candidates, found...)
		// Accumulated across sub-queries rather than replaced, so the count reports how
		// much a retriever contributed to the whole answer.
		plan.Candidates[mode] += len(found)
		if plan.Elapsed == nil {
			plan.Elapsed = map[domain.RetrievalMode]time.Duration{}
		}
		plan.Elapsed[mode] += r.now().Sub(started)
	}

	return candidates, nil
}

// lexical runs full-text or substring search.
func (r *Retriever) lexical(ctx context.Context, req domain.QueryRequest, exact bool) ([]candidate, error) {
	mode := domain.ModeLexical
	if exact {
		mode = domain.ModeExact
	}

	hits, err := r.indexes.Lexical.Search(ctx, domain.LexicalQuery{
		Scope:          req.Scope,
		Text:           req.Query,
		Surfaces:       req.Filters.Surfaces,
		ValidAt:        req.Temporal.ValidAt,
		Statuses:       statusStrings(req.Filters.Statuses),
		Classification: req.Filters.Classifications,
		MemoryKinds:    req.Filters.MemoryKinds,
		EntityTypes:    req.Filters.EntityTypes,
		Policy:         req.Policy,
		ActiveAt:       req.Temporal.ActiveAt,
		Exact:          exact,
		// Each retriever fetches more than the final limit: fusion needs depth to work
		// with, and a record ranked fourth by two retrievers should be able to beat one
		// ranked first by a single retriever.
		Limit: req.Limit * 3,
	})
	if err != nil {
		return nil, err
	}
	return toCandidates(mode, hits), nil
}

// vector runs nearest-neighbour search.
func (r *Retriever) vector(ctx context.Context, req domain.QueryRequest) ([]candidate, error) {
	vectors, err := r.embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		// A retrieval path failing must not fail the query: the other retrievers still
		// have answers, and returning them beats returning an error.
		if r.logger != nil {
			r.logger.WarnContext(ctx, "vector retrieval unavailable for this query",
				slog.String("error", err.Error()))
		}
		return nil, nil
	}
	if len(vectors) != 1 {
		return nil, nil
	}

	hits, err := r.indexes.Vectors.Search(ctx, domain.VectorQuery{
		Scope:          req.Scope,
		Embedding:      vectors[0],
		Surfaces:       req.Filters.Surfaces,
		Model:          r.embedder.Model(),
		Version:        r.embedder.Version(),
		ValidAt:        req.Temporal.ValidAt,
		Statuses:       statusStrings(req.Filters.Statuses),
		Classification: req.Filters.Classifications,
		MemoryKinds:    req.Filters.MemoryKinds,
		EntityTypes:    req.Filters.EntityTypes,
		Policy:         req.Policy,
		ActiveAt:       req.Temporal.ActiveAt,
		Limit:          req.Limit * 3,
	})
	if err != nil {
		return nil, err
	}
	return toCandidates(domain.ModeVector, hits), nil
}

// entity resolves the query as a name.
func (r *Retriever) entity(ctx context.Context, req domain.QueryRequest) ([]candidate, error) {
	entities, err := r.ledger.FindEntitiesByName(ctx, req.Scope, req.Query)
	if err != nil {
		return nil, err
	}

	out := make([]candidate, 0, len(entities))
	rank := 0
	for _, entity := range entities {
		// Entities carry no classification of their own, so the lever policy has here is
		// the entity type. Without this a rule hiding a type would still leak the names.
		if !req.Policy.Allows("", "", "", "", entity.EntityType) {
			continue
		}
		rank++
		out = append(out, candidate{
			mode:  domain.ModeEntity,
			rank:  rank,
			score: 1,
			hit: domain.Hit{
				Surface:  domain.SurfaceEntity,
				RecordID: string(entity.ID),
				Content:  entity.CanonicalName,
				Detail:   map[string]any{"retriever": "entity", "entity_type": entity.EntityType},
			},
		})
	}
	return out, nil
}

// graph expands from entities the other retrievers found.
func (r *Retriever) graph(ctx context.Context, req domain.QueryRequest, seeds []domain.EntityID) ([]candidate, error) {
	if len(seeds) == 0 {
		// Nothing to expand from. This is the common case for a prose query that matched
		// only passages, and it is why graph runs last.
		return nil, nil
	}

	hits, err := r.indexes.Graph.Expand(ctx, domain.GraphExpandQuery{
		Scope:      req.Scope,
		Roots:      seeds,
		Depth:      req.GraphDepth,
		Predicates: req.Filters.Predicates,
		ValidAt:    req.Temporal.ValidAt,
		Policy:     req.Policy,
		ActiveAt:   req.Temporal.ActiveAt,
		Limit:      req.Limit * 2,
	})
	if err != nil {
		return nil, err
	}

	// Name the entities the walk reached. One batched canonical read, because traversal
	// reports identifiers and only the ledger knows what they are called. Doing it here
	// rather than inside the index is what lets a graph backend hold nothing but edges.
	reached := make([]domain.EntityID, 0, len(hits))
	for _, hit := range hits {
		reached = append(reached, hit.EntityID)
	}
	names, err := r.ledger.GetEntities(ctx, req.Scope.WorkspaceID, reached)
	if err != nil {
		return nil, err
	}

	out := make([]candidate, 0, len(hits))
	rank := 0
	for _, hit := range hits {
		// An edge can point at an entity the ledger does not have. Not with PostgreSQL,
		// where graph_edges carries foreign keys to entities with ON DELETE CASCADE — but
		// a substituted backend has no such guarantee, and holding a stale edge for a
		// while is exactly what an eventually-consistent index does. Dropping the hit
		// keeps a nameless result off the page and a citation from pointing at nothing.
		//
		// The scoped lookup means this also drops anything outside the workspace, which
		// is the second line of the tenancy defence: the traversal already refuses a
		// foreign root, and nothing it did return could be named here anyway.
		entity, known := names[hit.EntityID]
		if !known {
			continue
		}
		// The same check the entity leg makes, for the same reason. An entity carries no
		// classification, so its type is the only lever policy has, and graph_edges has no
		// column for it — the type lives on the entity, which is why this cannot be pushed
		// into the traversal.
		//
		// It is applied here rather than later because here is where the disclosure
		// happens: traversal returns opaque identifiers and this is the read that turns
		// one into a name. Filtering at the point a name is produced is not the
		// after-the-fact filtering section 22.4 forbids; it is the narrowing, applied
		// where the data actually appears.
		if !req.Policy.Allows("", "", "", "", entity.EntityType) {
			continue
		}
		rank++
		out = append(out, candidate{
			mode:  domain.ModeGraph,
			rank:  rank,
			score: 1 / float64(1+hit.Depth),
			hit: domain.Hit{
				Surface:  domain.SurfaceEntity,
				RecordID: string(hit.EntityID),
				Content:  entity.CanonicalName,
				Detail:   map[string]any{"retriever": "graph", "depth": hit.Depth},
			},
			path: &domain.GraphPath{
				FromEntityID: hit.FromEntityID,
				ViaPredicate: hit.ViaPredicate,
				ViaAssertion: hit.ViaAssertion,
				Depth:        hit.Depth,
			},
		})
	}
	return out, nil
}

func toCandidates(mode domain.RetrievalMode, hits []domain.Hit) []candidate {
	out := make([]candidate, 0, len(hits))
	for i, hit := range hits {
		out = append(out, candidate{mode: mode, rank: i + 1, score: hit.Score, hit: hit})
	}
	return out
}

func statusStrings(statuses []domain.AssertionStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, string(status))
	}
	return out
}

func countDistinct(candidates []candidate) int {
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[string(c.hit.Surface)+"\x00"+c.hit.RecordID] = true
	}
	return len(seen)
}

// Explain renders a plan as readable lines, for the CLI and for debugging.
func Explain(plan domain.RetrievalPlan) []string {
	var out []string
	for _, mode := range plan.Modes {
		line := string(mode) + ": " + plan.Reasons[mode]
		if count, ok := plan.Candidates[mode]; ok {
			line += " (" + strconv.Itoa(count) + " candidate"
			if count != 1 {
				line += "s"
			}
			if elapsed, ok := plan.Elapsed[mode]; ok {
				line += ", " + elapsed.Round(time.Millisecond).String()
			}
			line += ")"
		}
		out = append(out, line)
	}

	// Sorted: map order would make the explanation differ between identical queries, and an
	// explanation that changes shape run to run is hard to trust and harder to diff.
	skipped := make([]string, 0, len(plan.Skipped))
	for mode := range plan.Skipped {
		skipped = append(skipped, string(mode))
	}
	sort.Strings(skipped)
	for _, mode := range skipped {
		out = append(out, mode+": skipped, "+plan.Skipped[domain.RetrievalMode(mode)])
	}
	return out
}

// record persists a trace of this query, when a sink is configured.
//
// Best effort: a query that answered correctly must not fail because its trace could not be
// written. The failure is logged rather than returned, because losing explainability is a
// problem for later and losing the answer is a problem now.
func (r *Retriever) record(ctx context.Context, req domain.QueryRequest, candidates []candidate, items []domain.RetrievedItem, latency time.Duration) domain.TraceID {
	if r.traces == nil {
		return ""
	}

	trace := domain.RetrievalTrace{
		WorkspaceID:   req.Scope.WorkspaceID,
		GraphSpaceID:  req.Scope.GraphSpaceID,
		QueryText:     req.Query,
		Redacted:      r.redact,
		Principal:     req.Principal,
		Purpose:       req.Purpose,
		Action:        domain.ActionRead,
		PolicyFilters: req.Policy,
		Filters:       req.Filters,
		Latency:       latency,
		QueryTime:     r.now(),
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		key := string(c.hit.Surface) + "/" + c.hit.RecordID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		trace.CandidateRefs = append(trace.CandidateRefs, domain.ScoredRef{
			Surface: c.hit.Surface, RecordID: c.hit.RecordID, Score: c.score,
		})
	}
	for _, item := range items {
		trace.SelectedRefs = append(trace.SelectedRefs, domain.ScoredRef{
			Surface: item.Surface, RecordID: item.RecordID, Score: item.Score,
		})
	}

	recorded, err := r.traces.RecordTrace(ctx, trace)
	if err != nil {
		if r.logger != nil {
			r.logger.WarnContext(ctx, "cannot record retrieval trace",
				slog.String("error", err.Error()))
		}
		return ""
	}
	return recorded.ID
}

// available reports whether the index a mode needs is configured.
//
// ModeEntity is always available: it resolves names against the canonical alias table, so
// it depends on the ledger rather than on any index.
func (r *Retriever) available(mode domain.RetrievalMode) bool {
	switch mode {
	case domain.ModeLexical, domain.ModeExact:
		return r.indexes.Lexical != nil
	case domain.ModeVector:
		return r.indexes.Vectors != nil
	case domain.ModeGraph:
		return r.indexes.Graph != nil
	default:
		return true
	}
}
