// Package contextblock assembles retrieval results into a prompt-ready context block
// (AGENTS.md section 20).
//
// The output of retrieval is not automatically the final prompt. Retrieval answers "what
// matches"; assembly answers "what is worth spending a token budget on", which is a
// different question with different failure modes — a block can be perfectly relevant and
// still useless because it says the same thing nine times, or because it states a fact that
// stopped being true last quarter.
//
// The package is named for the thing it produces rather than sitting at internal/context as
// AGENTS.md section 5 sketches. A package named context forces every caller and every file
// in it to alias the standard library's, and the aliasing would outnumber the code.
package contextblock

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
)

// Retriever is the retrieval surface assembly consumes, declared by its consumer.
type Retriever interface {
	Query(ctx context.Context, req domain.QueryRequest) (domain.QueryResult, error)
}

// Options configure an assembler.
type Options struct {
	Weights   Weights
	Estimator Estimator
	// CandidateMultiple decides how many retrieval results to consider per item that can
	// be selected. Selection can only drop, so a redundancy filter with nothing to fall
	// back on returns a short block.
	CandidateMultiple int
	// ScaffoldingReserve is the fraction of the budget held back for the header and
	// section headings before content selection runs.
	ScaffoldingReserve float64
	// Clock is injectable so temporal behavior is testable.
	Clock func() time.Time
}

// Assembler builds context blocks.
type Assembler struct {
	retriever Retriever
	store     Store
	opts      Options
	logger    *slog.Logger
	tracer    trace.Tracer
}

// DefaultCandidateMultiple is how many candidates are fetched per selectable item.
const DefaultCandidateMultiple = 4

// DefaultScaffoldingReserve holds back budget for headings and references.
const DefaultScaffoldingReserve = 0.25

func New(retriever Retriever, store Store, opts Options, logger *slog.Logger, tracer trace.Tracer) *Assembler {
	opts.Weights = opts.Weights.withDefaults()
	if opts.Estimator == nil {
		opts.Estimator = NewHeuristicEstimator()
	}
	if opts.CandidateMultiple <= 0 {
		opts.CandidateMultiple = DefaultCandidateMultiple
	}
	if opts.ScaffoldingReserve <= 0 {
		opts.ScaffoldingReserve = DefaultScaffoldingReserve
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("contextblock")
	}
	return &Assembler{retriever: retriever, store: store, opts: opts, logger: logger, tracer: tracer}
}

// Assemble retrieves, selects, and renders one context block.
func (a *Assembler) Assemble(ctx context.Context, req domain.ContextRequest) (domain.ContextBlock, error) {
	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return domain.ContextBlock{}, err
	}

	ctx, span := a.tracer.Start(ctx, "context.assemble")
	defer span.End()
	span.SetAttributes(
		attribute.Int("context.token_budget", req.TokenBudget),
		attribute.Int("context.max_items", req.MaxItems),
	)

	result, err := a.retriever.Query(ctx, domain.QueryRequest{
		Scope:     req.Scope,
		Query:     req.Query,
		Principal: req.Principal,
		Temporal:  req.Temporal,
		Filters:   req.Filters,
		Policy:    req.Policy,
		Purpose:   req.Purpose,
		Limit:     req.MaxItems * a.opts.CandidateMultiple,
		Explain:   req.Explain,
	})
	if err != nil {
		return domain.ContextBlock{}, err
	}

	hydrator := &hydrator{store: a.store, req: req, now: a.opts.Clock()}
	candidates, err := hydrator.hydrate(ctx, result.Items)
	if err != nil {
		return domain.ContextBlock{}, err
	}

	// Content gets the budget minus what the header and section headings are expected to
	// cost. Reference lines are charged to the items themselves rather than to this
	// reserve, because their cost scales with how many items are selected. The renderer
	// enforces the real ceiling either way.
	contentBudget := int(float64(req.TokenBudget) * (1 - a.opts.ScaffoldingReserve))
	if contentBudget < 1 {
		contentBudget = 1
	}

	selector := &selector{
		weights: a.opts.Weights, estimator: a.opts.Estimator, req: req,
		cost: a.renderedCost,
	}
	chosen := orderForRendering(selector.choose(candidates, contentBudget))

	block, err := a.render(req, chosen)
	if err != nil {
		return domain.ContextBlock{}, err
	}

	block.Dropped = append(hydrator.dropped, selector.dropped...)
	block.Dropped = append(block.Dropped, a.droppedByRenderer(chosen, block.Items)...)
	if req.Explain {
		block.Plan = result.Plan
	}

	span.SetAttributes(
		attribute.Int("context.items", len(block.Items)),
		attribute.Int("context.tokens", block.Budget.Used),
		attribute.Int("context.dropped", len(block.Dropped)),
	)
	a.logger.DebugContext(ctx, "context assembled",
		slog.Int("items", len(block.Items)),
		slog.Int("tokens", block.Budget.Used),
		slog.Int("budget", block.Budget.Limit),
		slog.Int("candidates", len(candidates)))

	return block, nil
}

// renderedCost is what one candidate will spend: its own text and its reference line.
func (a *Assembler) renderedCost(c candidate) int {
	citation := c.citation
	citation.Marker = 1 // one digit; the difference against a three-digit marker is noise
	return a.opts.Estimator.Estimate(c.text) + a.opts.Estimator.Estimate(referenceLine(citation))
}

// render writes the block, dropping anything that no longer fits.
func (a *Assembler) render(req domain.ContextRequest, chosen []selection) (domain.ContextBlock, error) {
	r, err := newRenderer(a.opts.Estimator, req.TokenBudget)
	if err != nil {
		return domain.ContextBlock{}, err
	}

	asOf := ""
	if req.Temporal.ValidAt != nil {
		asOf = req.Temporal.ValidAt.UTC().Format(time.RFC3339)
	}
	r.header(req.Query, asOf)

	block := domain.ContextBlock{}
	marker := 0
	current := domain.ContextSection("")

	for _, sel := range chosen {
		if sel.candidate.section != current {
			r.startSection(sel.candidate.section)
			current = sel.candidate.section
		}

		text, ok := r.writeItem(marker+1, sel, sel.candidate.citation)
		if !ok {
			// Out of room. Keep going rather than stopping: a later item may be short
			// enough to fit, and the reference list still has to be written.
			continue
		}
		marker++

		citation := sel.candidate.citation
		citation.Marker = marker

		block.Items = append(block.Items, domain.ContextItem{
			Section:    sel.candidate.section,
			Marker:     marker,
			Text:       text,
			Surface:    sel.candidate.surface,
			RecordID:   sel.candidate.recordID,
			Relevance:  sel.candidate.relevance,
			Selection:  sel.score,
			Redundancy: sel.redundancy,
			Tokens:     a.opts.Estimator.Estimate(text),
			Signals:    sel.signals,
			Conflict:   sel.candidate.conflict,
		})
		block.Citations = append(block.Citations, citation)
	}

	block.Citations = r.writeReferences(block.Citations)
	block.Text = r.block.String()
	block.Budget = domain.BudgetReport{
		Limit:       req.TokenBudget,
		Used:        r.used,
		Scaffolding: r.scaffold,
		BySection:   r.section,
		Tolerance:   a.opts.Estimator.Tolerance(),
		Estimator:   a.opts.Estimator.Name(),
	}
	return block, nil
}

// droppedByRenderer records selections that did not survive rendering.
func (a *Assembler) droppedByRenderer(chosen []selection, rendered []domain.ContextItem) []domain.DroppedItem {
	present := make(map[string]struct{}, len(rendered))
	for _, item := range rendered {
		present[string(item.Surface)+"/"+item.RecordID] = struct{}{}
	}

	var out []domain.DroppedItem
	for _, sel := range chosen {
		key := string(sel.candidate.surface) + "/" + sel.candidate.recordID
		if _, ok := present[key]; ok {
			continue
		}
		out = append(out, domain.DroppedItem{
			Surface: sel.candidate.surface, RecordID: sel.candidate.recordID,
			Section: sel.candidate.section, Reason: domain.DropBudget,
			Detail:    "selected but did not fit once scaffolding was rendered",
			Relevance: sel.candidate.relevance, Redundancy: sel.redundancy,
		})
	}
	return out
}
