package retrieval

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding"
)

// Store is the projection surface retrieval reads, declared by its consumer.
type Store interface {
	SearchLexical(ctx context.Context, q domain.LexicalQuery) ([]domain.Hit, error)
	SearchVectors(ctx context.Context, q domain.VectorQuery) ([]domain.Hit, error)
	ExpandGraph(ctx context.Context, q domain.GraphExpandQuery) ([]domain.GraphHit, error)
	FindEntitiesByName(ctx context.Context, scope domain.Scope, name string) ([]domain.Entity, error)
	GetAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error)
}

// Retriever runs the planned candidate generators and fuses their results.
type Retriever struct {
	store    Store
	embedder embedding.Embedder
	weights  Weights
	now      func() time.Time
	logger   *slog.Logger
	tracer   trace.Tracer
}

// Options configures retrieval.
type Options struct {
	Weights Weights
	Now     func() time.Time
}

// New builds a retriever. The embedder may be nil, in which case the vector leg is skipped
// and the plan records why.
func New(store Store, embedder embedding.Embedder, opts Options, logger *slog.Logger, tracer trace.Tracer) *Retriever {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("retrieval")
	}
	return &Retriever{
		store:    store,
		embedder: embedder,
		weights:  opts.Weights.withDefaults(),
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

	plan := planner{hasEmbedder: r.embedder != nil}.plan(req)

	var (
		candidates []candidate
		// Entities found by the precise retrievers seed graph expansion, which is what
		// section 19.5 means by using retrieved entities as entry points.
		seeds []domain.EntityID
	)

	for _, mode := range plan.Modes {
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
			return domain.QueryResult{}, err
		}

		for _, c := range found {
			if c.hit.Surface == domain.SurfaceEntity {
				seeds = append(seeds, domain.EntityID(c.hit.RecordID))
			}
		}

		candidates = append(candidates, found...)
		plan.Candidates[mode] = len(found)
		if plan.Elapsed == nil {
			plan.Elapsed = map[domain.RetrievalMode]time.Duration{}
		}
		plan.Elapsed[mode] = r.now().Sub(started)
	}

	items := fuse(candidates, r.weights, req.Limit)
	result := domain.QueryResult{Items: items, Total: countDistinct(candidates)}
	if req.Explain {
		result.Plan = &plan
	}

	span.SetAttributes(
		attribute.Int("strata.candidates", result.Total),
		attribute.Int("strata.results", len(items)),
	)
	if r.logger != nil {
		r.logger.DebugContext(ctx, "retrieval complete",
			slog.Int("candidates", result.Total),
			slog.Int("results", len(items)))
	}
	return result, nil
}

// lexical runs full-text or substring search.
func (r *Retriever) lexical(ctx context.Context, req domain.QueryRequest, exact bool) ([]candidate, error) {
	mode := domain.ModeLexical
	if exact {
		mode = domain.ModeExact
	}

	hits, err := r.store.SearchLexical(ctx, domain.LexicalQuery{
		Scope:          req.Scope,
		Text:           req.Query,
		Surfaces:       req.Filters.Surfaces,
		ValidAt:        req.Temporal.ValidAt,
		Statuses:       statusStrings(req.Filters.Statuses),
		Classification: req.Filters.Classifications,
		MemoryKinds:    req.Filters.MemoryKinds,
		EntityTypes:    req.Filters.EntityTypes,
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

	hits, err := r.store.SearchVectors(ctx, domain.VectorQuery{
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
		Limit:          req.Limit * 3,
	})
	if err != nil {
		return nil, err
	}
	return toCandidates(domain.ModeVector, hits), nil
}

// entity resolves the query as a name.
func (r *Retriever) entity(ctx context.Context, req domain.QueryRequest) ([]candidate, error) {
	entities, err := r.store.FindEntitiesByName(ctx, req.Scope, req.Query)
	if err != nil {
		return nil, err
	}

	out := make([]candidate, 0, len(entities))
	for i, entity := range entities {
		out = append(out, candidate{
			mode:  domain.ModeEntity,
			rank:  i + 1,
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

	hits, err := r.store.ExpandGraph(ctx, domain.GraphExpandQuery{
		Scope:      req.Scope,
		Roots:      seeds,
		Depth:      req.GraphDepth,
		Predicates: req.Filters.Predicates,
		ValidAt:    req.Temporal.ValidAt,
		Limit:      req.Limit * 2,
	})
	if err != nil {
		return nil, err
	}

	out := make([]candidate, 0, len(hits))
	rank := 0
	for _, hit := range hits {
		// The seeds themselves are returned at depth zero. They are already candidates
		// from whichever retriever found them, so re-adding them would double-count.
		if hit.Depth == 0 {
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
				Content:  hit.Name,
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
