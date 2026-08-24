package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/policy"
	"github.com/gimantha/strata/internal/retrieval"
)

var errUnknownTool = errors.New("unknown tool")

// Deps are the services the tools call, declared by their consumer.
type Deps struct {
	Gateway   *ingest.Gateway
	Retriever *retrieval.Retriever
	Assembler *contextblock.Assembler
	Policy    *policy.Service
	Ledger    Ledger
}

// Ledger is the canonical read surface the tools need.
type Ledger interface {
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	GetAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error)
	ProvenanceChain(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.ProvenanceChain, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	GetSourceByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.Source, error)
	GetTrace(ctx context.Context, ws domain.WorkspaceID, id domain.TraceID) (domain.RetrievalTrace, error)
}

// Tools is the MCP tool surface (AGENTS.md section 26).
type Tools struct {
	deps      Deps
	principal domain.Principal
	scope     domain.Scope
	// defaultSource attributes knowledge ingested through MCP, so an agent's contributions
	// are distinguishable from a connector's.
	defaultSource string
}

func NewTools(deps Deps, principal domain.Principal, scope domain.Scope, defaultSource string) *Tools {
	if defaultSource == "" {
		defaultSource = "mcp"
	}
	return &Tools{deps: deps, principal: principal, scope: scope, defaultSource: defaultSource}
}

// Definitions lists the tools and their argument schemas.
//
// Small and stable by design. These are the questions an agent asks, not a projection of the
// internal service layer — so the internals can be reorganized without every agent needing
// to relearn the surface.
func (t *Tools) Definitions() []toolDefinition {
	return []toolDefinition{
		{
			Name: "context_graph_search",
			Description: "Search the graph across lexical, exact, vector, entity, and graph " +
				"retrieval, fused into one ranking. Returns canonical ids to follow.",
			InputSchema: object(map[string]any{
				"query":    stringProp("what to search for"),
				"limit":    intProp("maximum results (default 10)"),
				"valid_at": stringProp("only what held at this RFC3339 instant"),
				"surfaces": arrayProp("restrict to chunk, episode, entity, or assertion"),
				"explain":  boolProp("include the retrieval plan and per-retriever ranks"),
			}, "query"),
		},
		{
			Name: "context_graph_get_context",
			Description: "Assemble a prompt-ready context block within a token budget, with " +
				"citations. Use this instead of search when the result goes into a prompt.",
			InputSchema: object(map[string]any{
				"query":        stringProp("the question the context should answer"),
				"token_budget": intProp("hard ceiling on the block (default 2000)"),
				"valid_at":     stringProp("assemble what held at this RFC3339 instant"),
				"sections":     arrayProp("facts, history, graph, excerpts, conflicts"),
			}, "query"),
		},
		{
			Name: "context_graph_ingest",
			Description: "Record source material. Returns immediately with an event id; " +
				"extraction happens asynchronously.",
			InputSchema: object(map[string]any{
				"content":         stringProp("the text to ingest"),
				"source_name":     stringProp("registered source to attribute it to"),
				"media_type":      stringProp("text/plain, text/markdown, or application/json"),
				"idempotency_key": stringProp("resending the same key returns the original event"),
			}, "content"),
		},
		{
			Name:        "context_graph_get_entity",
			Description: "Fetch one identity and the claims about it.",
			InputSchema: object(map[string]any{
				"entity_id": stringProp("canonical entity id"),
				"limit":     intProp("maximum claims to return"),
			}, "entity_id"),
		},
		{
			Name:        "context_graph_get_assertion",
			Description: "Fetch one claim with its temporal coordinates and status.",
			InputSchema: object(map[string]any{
				"assertion_id": stringProp("canonical assertion id"),
			}, "assertion_id"),
		},
		{
			Name: "context_graph_explain",
			Description: "Walk a claim back to its evidence, chunk, episode, and source. Use " +
				"this to check a fact rather than trusting it.",
			InputSchema: object(map[string]any{
				"assertion_id": stringProp("canonical assertion id"),
				"trace_id":     stringProp("explain a past retrieval instead of a claim"),
			}, ""),
		},
		{
			Name: "context_graph_temporal_query",
			Description: "Ask what was true, or what was believed, at a past instant. " +
				"valid_at asks about the world; known_at asks about this system's belief.",
			InputSchema: object(map[string]any{
				"subject":   stringProp("entity id to ask about"),
				"predicate": stringProp("restrict to one predicate"),
				"valid_at":  stringProp("what held in the world at this RFC3339 instant"),
				"known_at":  stringProp("what this system believed at this RFC3339 instant"),
				"limit":     intProp("maximum claims to return"),
			}, ""),
		},
	}
}

// Call runs one tool.
//
// Authorization happens here, per call, through the same policy service the HTTP API uses.
// MCP is a transport, not a trust boundary of its own (AGENTS.md section 26).
func (t *Tools) Call(ctx context.Context, name string, arguments json.RawMessage) (callResult, error) {
	handler, ok := map[string]func(context.Context, json.RawMessage) (callResult, error){
		"context_graph_search":         t.search,
		"context_graph_get_context":    t.getContext,
		"context_graph_ingest":         t.ingest,
		"context_graph_get_entity":     t.getEntity,
		"context_graph_get_assertion":  t.getAssertion,
		"context_graph_explain":        t.explain,
		"context_graph_temporal_query": t.temporalQuery,
	}[name]
	if !ok {
		return callResult{}, fmt.Errorf("%w: %s", errUnknownTool, name)
	}
	return handler(ctx, arguments)
}

// authorize evaluates policy for one action, returning the filters every query must apply.
func (t *Tools) authorize(ctx context.Context, action domain.PolicyAction) (domain.PolicyFilters, error) {
	if t.deps.Policy == nil {
		return domain.PolicyFilters{}, nil
	}

	decision, err := t.deps.Policy.Authorize(ctx, domain.AccessRequest{
		Principal: t.principal, Action: action, Scope: t.scope, Purpose: "mcp",
	})
	if err != nil {
		return domain.PolicyFilters{}, err
	}
	if !decision.Allowed {
		return domain.PolicyFilters{}, errors.New(decision.Reason)
	}
	return decision.Filters, nil
}

type searchArgs struct {
	Query    string   `json:"query"`
	Limit    int      `json:"limit"`
	ValidAt  string   `json:"valid_at"`
	Surfaces []string `json:"surfaces"`
	Explain  bool     `json:"explain"`
}

func (t *Tools) search(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args searchArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	filters, err := t.authorize(ctx, domain.ActionRead)
	if err != nil {
		return errorResult(err), nil
	}

	req := domain.QueryRequest{
		Scope: t.scope, Query: args.Query, Principal: t.principal.Ref(),
		Policy: filters, Purpose: "mcp", Limit: orDefault(args.Limit, 10),
		Explain: args.Explain,
	}
	if req.Temporal.ValidAt, err = parseTime(args.ValidAt); err != nil {
		return errorResult(err), nil
	}
	for _, surface := range args.Surfaces {
		parsed, err := domain.ParseSurface(surface)
		if err != nil {
			return errorResult(err), nil
		}
		req.Filters.Surfaces = append(req.Filters.Surfaces, parsed)
	}

	result, err := t.deps.Retriever.Query(ctx, req)
	if err != nil {
		return errorResult(err), nil
	}

	items := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		entry := map[string]any{
			// Canonical ids, always: an agent follows a reference rather than asking for
			// a bigger payload (AGENTS.md section 26).
			"surface": string(item.Surface), "record_id": item.RecordID,
			"content": item.Content, "score": item.Score,
		}
		if args.Explain {
			entry["found_by"] = modeStrings(item.FoundBy)
			entry["signals"] = item.Signals
		}
		items = append(items, entry)
	}

	payload := map[string]any{"results": items, "considered": result.Total}
	if result.TraceID != "" {
		// So the agent can ask why it got these results, later, with one id.
		payload["trace_id"] = string(result.TraceID)
	}
	return textResult(payload)
}

type contextArgs struct {
	Query       string   `json:"query"`
	TokenBudget int      `json:"token_budget"`
	ValidAt     string   `json:"valid_at"`
	Sections    []string `json:"sections"`
}

func (t *Tools) getContext(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args contextArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	filters, err := t.authorize(ctx, domain.ActionRead)
	if err != nil {
		return errorResult(err), nil
	}

	req := domain.ContextRequest{
		Scope: t.scope, Query: args.Query, Principal: t.principal.Ref(),
		Policy: filters, Purpose: "mcp", TokenBudget: args.TokenBudget,
	}
	if req.Temporal.ValidAt, err = parseTime(args.ValidAt); err != nil {
		return errorResult(err), nil
	}
	for _, section := range args.Sections {
		parsed, err := domain.ParseContextSection(section)
		if err != nil {
			return errorResult(err), nil
		}
		req.Sections = append(req.Sections, parsed)
	}

	block, err := t.deps.Assembler.Assemble(ctx, req)
	if err != nil {
		return errorResult(err), nil
	}

	citations := make([]map[string]any, 0, len(block.Citations))
	for _, citation := range block.Citations {
		entry := map[string]any{"marker": citation.Marker}
		if citation.AssertionID != nil {
			entry["assertion_id"] = string(*citation.AssertionID)
		}
		if citation.ChunkID != nil {
			entry["chunk_id"] = string(*citation.ChunkID)
		}
		if citation.SourceName != "" {
			entry["source"] = citation.SourceName
		}
		citations = append(citations, entry)
	}

	return textResult(map[string]any{
		"context":   block.Text,
		"citations": citations,
		"budget": map[string]any{
			"limit": block.Budget.Limit, "used": block.Budget.Used,
		},
	})
}

type ingestArgs struct {
	Content        string `json:"content"`
	SourceName     string `json:"source_name"`
	MediaType      string `json:"media_type"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (t *Tools) ingest(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args ingestArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	if _, err := t.authorize(ctx, domain.ActionWrite); err != nil {
		return errorResult(err), nil
	}

	name := args.SourceName
	if name == "" {
		name = t.defaultSource
	}
	source, err := t.deps.Ledger.GetSourceByName(ctx, t.scope.WorkspaceID, name)
	if err != nil {
		return errorResult(err), nil
	}

	mediaType := args.MediaType
	if mediaType == "" {
		mediaType = normalize.MediaTypePlain
	}

	receipt, err := t.deps.Gateway.Accept(ctx, ingest.Request{
		Scope: t.scope, Principal: t.principal.Ref(), SourceID: source.ID,
		MediaType: mediaType, Payload: []byte(args.Content),
		IdempotencyKey: args.IdempotencyKey,
	})
	if err != nil {
		return errorResult(err), nil
	}

	return textResult(map[string]any{
		"source_event_id": string(receipt.SourceEventID),
		"duplicate":       receipt.Duplicate,
		// Said plainly, because an agent that assumes its facts are queryable on the next
		// call will draw the wrong conclusion from an empty search.
		"note": "accepted and durable; extraction runs asynchronously, so this content " +
			"may not be searchable yet",
	})
}

type entityArgs struct {
	EntityID string `json:"entity_id"`
	Limit    int    `json:"limit"`
}

func (t *Tools) getEntity(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args entityArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	filters, err := t.authorize(ctx, domain.ActionRead)
	if err != nil {
		return errorResult(err), nil
	}

	entity, err := t.deps.Ledger.GetEntity(ctx, t.scope.WorkspaceID, domain.EntityID(args.EntityID))
	if err != nil {
		return errorResult(err), nil
	}

	claims, err := t.deps.Ledger.QueryAssertions(ctx, domain.AssertionQuery{
		Scope:           t.scope,
		SubjectIDs:      []domain.EntityID{entity.ID},
		Statuses:        []domain.AssertionStatus{domain.AssertionActive},
		Classifications: filters.PermittedClassifications(),
		Limit:           orDefault(args.Limit, 25),
	})
	if err != nil {
		return errorResult(err), nil
	}

	out := make([]map[string]any, 0, len(claims))
	for _, claim := range claims {
		if !filters.Allows(claim.Classification, "", claim.Predicate.Name, claim.MemoryKind, "") {
			continue
		}
		out = append(out, assertionJSON(claim))
	}

	return textResult(map[string]any{
		"entity_id": string(entity.ID), "name": entity.CanonicalName,
		"entity_type": entity.EntityType, "assertions": out,
	})
}

type assertionArgs struct {
	AssertionID string `json:"assertion_id"`
}

func (t *Tools) getAssertion(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args assertionArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	filters, err := t.authorize(ctx, domain.ActionRead)
	if err != nil {
		return errorResult(err), nil
	}

	assertion, err := t.deps.Ledger.GetAssertion(ctx, t.scope.WorkspaceID,
		domain.AssertionID(args.AssertionID))
	if err != nil {
		return errorResult(err), nil
	}
	if !filters.Allows(assertion.Classification, "", assertion.Predicate.Name,
		assertion.MemoryKind, "") {
		// Absence rather than denial: confirming a claim exists at a classification the
		// caller cannot read is itself a disclosure.
		return errorResult(errors.New("assertion not found")), nil
	}
	return textResult(assertionJSON(assertion))
}

type explainArgs struct {
	AssertionID string `json:"assertion_id"`
	TraceID     string `json:"trace_id"`
}

func (t *Tools) explain(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args explainArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	filters, err := t.authorize(ctx, domain.ActionRead)
	if err != nil {
		return errorResult(err), nil
	}

	if args.TraceID != "" {
		trace, err := t.deps.Ledger.GetTrace(ctx, t.scope.WorkspaceID, domain.TraceID(args.TraceID))
		if err != nil {
			return errorResult(err), nil
		}
		selected := make([]map[string]any, 0, len(trace.SelectedRefs))
		for _, ref := range trace.SelectedRefs {
			selected = append(selected, map[string]any{
				"surface": string(ref.Surface), "record_id": ref.RecordID, "score": ref.Score,
			})
		}
		return textResult(map[string]any{
			"trace_id": string(trace.ID), "query": trace.QueryText,
			"redacted": trace.Redacted, "considered": len(trace.CandidateRefs),
			"selected": selected, "latency_ms": trace.Latency.Milliseconds(),
		})
	}

	if args.AssertionID == "" {
		return errorResult(errors.New("assertion_id or trace_id is required")), nil
	}

	chain, err := t.deps.Ledger.ProvenanceChain(ctx, t.scope.WorkspaceID,
		domain.AssertionID(args.AssertionID))
	if err != nil {
		return errorResult(err), nil
	}
	if !filters.Allows(chain.Assertion.Classification, "", chain.Assertion.Predicate.Name,
		chain.Assertion.MemoryKind, "") {
		return errorResult(errors.New("assertion not found")), nil
	}

	links := make([]map[string]any, 0, len(chain.Links))
	for _, link := range chain.Links {
		links = append(links, map[string]any{
			"evidence_id": string(link.Evidence.ID),
			"quote":       link.Evidence.ExtractedText,
			"episode_id":  string(link.Episode.ID),
			"source":      link.Source.Name,
			"source_kind": string(link.Source.Kind),
			"trust":       string(link.Source.TrustLevel),
		})
	}

	return textResult(map[string]any{
		"assertion":  assertionJSON(chain.Assertion),
		"subject":    chain.Subject.CanonicalName,
		"provenance": links,
	})
}

type temporalArgs struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	ValidAt   string `json:"valid_at"`
	KnownAt   string `json:"known_at"`
	Limit     int    `json:"limit"`
}

func (t *Tools) temporalQuery(ctx context.Context, raw json.RawMessage) (callResult, error) {
	var args temporalArgs
	if err := decode(raw, &args); err != nil {
		return errorResult(err), nil
	}

	filters, err := t.authorize(ctx, domain.ActionRead)
	if err != nil {
		return errorResult(err), nil
	}

	query := domain.AssertionQuery{
		Scope:           t.scope,
		Classifications: filters.PermittedClassifications(),
		Limit:           orDefault(args.Limit, 25),
		// History is the point of this tool, so superseded claims are included: "what did
		// we believe in March" cannot be answered from current belief alone.
		IncludeSuperseded: true,
	}
	if args.Subject != "" {
		query.SubjectIDs = []domain.EntityID{domain.EntityID(args.Subject)}
	}
	if args.Predicate != "" {
		query.Predicates = []string{domain.NormalizePredicateName(args.Predicate)}
	}
	if query.ValidAt, err = parseTime(args.ValidAt); err != nil {
		return errorResult(err), nil
	}
	if query.KnownAt, err = parseTime(args.KnownAt); err != nil {
		return errorResult(err), nil
	}

	claims, err := t.deps.Ledger.QueryAssertions(ctx, query)
	if err != nil {
		return errorResult(err), nil
	}

	out := make([]map[string]any, 0, len(claims))
	for _, claim := range claims {
		if !filters.Allows(claim.Classification, "", claim.Predicate.Name, claim.MemoryKind, "") {
			continue
		}
		out = append(out, assertionJSON(claim))
	}
	return textResult(map[string]any{"assertions": out, "count": len(out)})
}

func assertionJSON(a domain.Assertion) map[string]any {
	out := map[string]any{
		"assertion_id": string(a.ID),
		"subject_id":   string(a.SubjectID),
		"predicate":    a.Predicate.Name,
		"object":       a.Object.Display(),
		"object_kind":  string(a.Object.Kind),
		"status":       string(a.Status),
		"confidence":   a.Confidence,
		"memory_kind":  string(a.MemoryKind),
		"recorded_at":  a.Temporal.RecordedAt,
	}
	if a.Object.Kind == domain.ObjectEntity {
		out["object_entity_id"] = string(a.Object.EntityID)
	}
	if a.ScopeKey != "" {
		out["scope_key"] = a.ScopeKey
	}
	if a.Temporal.ValidFrom != nil {
		out["valid_from"] = a.Temporal.ValidFrom
	}
	if a.Temporal.ValidTo != nil {
		out["valid_to"] = a.Temporal.ValidTo
	}
	if a.Temporal.SupersededAt != nil {
		out["superseded_at"] = a.Temporal.SupersededAt
	}
	return out
}

func decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("cannot parse arguments: %w", err)
	}
	return nil
}

func parseTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("%q is not an RFC3339 instant", value)
	}
	utc := parsed.UTC()
	return &utc, nil
}

func orDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func modeStrings(modes []domain.RetrievalMode) []string {
	out := make([]string, 0, len(modes))
	for _, mode := range modes {
		out = append(out, string(mode))
	}
	return out
}

// Schema helpers. MCP tools advertise JSON Schema, and hand-writing it per tool is where
// argument names and documentation drift apart.
func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	trimmed := make([]string, 0, len(required))
	for _, name := range required {
		if name != "" {
			trimmed = append(trimmed, name)
		}
	}
	if len(trimmed) > 0 {
		schema["required"] = trimmed
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func arrayProp(description string) map[string]any {
	return map[string]any{
		"type": "array", "items": map[string]any{"type": "string"},
		"description": description,
	}
}
