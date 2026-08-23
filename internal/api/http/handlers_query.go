package http

import (
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/retrieval"
)

type queryRequest struct {
	Query string   `json:"query"`
	Modes []string `json:"modes,omitempty"`

	ValidAt  *time.Time `json:"valid_at,omitempty"`
	KnownAt  *time.Time `json:"known_at,omitempty"`
	ActiveAt *time.Time `json:"active_at,omitempty"`

	Surfaces        []string `json:"surfaces,omitempty"`
	Classifications []string `json:"classifications,omitempty"`
	MemoryKinds     []string `json:"memory_kinds,omitempty"`
	Predicates      []string `json:"predicates,omitempty"`
	Statuses        []string `json:"statuses,omitempty"`
	MinConfidence   float64  `json:"min_confidence,omitempty"`

	Limit      int  `json:"limit,omitempty"`
	GraphDepth int  `json:"graph_depth,omitempty"`
	Explain    bool `json:"explain,omitempty"`
}

// handleQuery runs hybrid retrieval (AGENTS.md section 25.2).
//
// It returns references and scores rather than assembled prose. Turning results into a
// token-budgeted context block with citations is a separate endpoint in phase 8, because
// retrieval and assembly fail differently and should be debuggable apart.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}
	if s.retriever == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeProjectionNotReady, "http.handleQuery",
			"retrieval is not configured on this server"))
		return
	}

	var req queryRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	query := domain.QueryRequest{
		Scope:     scope,
		Query:     req.Query,
		Principal: principalFrom(r.Context()).Ref(),
		Temporal: domain.TemporalQuery{
			ValidAt: req.ValidAt, KnownAt: req.KnownAt, ActiveAt: req.ActiveAt,
		},
		Filters:    domain.QueryFilters{MinConfidence: req.MinConfidence, Predicates: req.Predicates},
		Limit:      req.Limit,
		GraphDepth: req.GraphDepth,
		Explain:    req.Explain,
	}

	for _, mode := range req.Modes {
		parsed, err := domain.ParseRetrievalMode(mode)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.Modes = append(query.Modes, parsed)
	}
	for _, surface := range req.Surfaces {
		parsed, err := domain.ParseSurface(surface)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.Filters.Surfaces = append(query.Filters.Surfaces, parsed)
	}
	for _, classification := range req.Classifications {
		parsed, err := domain.ParseClassification(classification)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.Filters.Classifications = append(query.Filters.Classifications, parsed)
	}
	for _, kind := range req.MemoryKinds {
		parsed, err := domain.ParseMemoryKind(kind)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.Filters.MemoryKinds = append(query.Filters.MemoryKinds, parsed)
	}
	for _, status := range req.Statuses {
		parsed, err := domain.ParseAssertionStatus(status)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.Filters.Statuses = append(query.Filters.Statuses, parsed)
	}

	result, err := s.retriever.Query(r.Context(), query)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	items := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		entry := map[string]any{
			"surface":   string(item.Surface),
			"record_id": item.RecordID,
			"content":   item.Content,
			"score":     item.Score,
			"found_by":  modeStrings(item.FoundBy),
		}
		if req.Explain {
			// Signals and ranks make a result arguable rather than merely returned.
			entry["signals"] = item.Signals
			entry["ranks"] = rankStrings(item.Ranks)
		}
		if item.Path != nil {
			entry["path"] = map[string]any{
				"from_entity_id": string(item.Path.FromEntityID),
				"via_predicate":  item.Path.ViaPredicate,
				"via_assertion":  string(item.Path.ViaAssertion),
				"depth":          item.Path.Depth,
			}
		}
		items = append(items, entry)
	}

	body := map[string]any{
		"results":    items,
		"count":      len(items),
		"considered": result.Total,
	}
	if result.Plan != nil {
		body["plan"] = map[string]any{
			"modes":       modeStrings(result.Plan.Modes),
			"reasons":     reasonStrings(result.Plan.Reasons),
			"skipped":     reasonStrings(result.Plan.Skipped),
			"candidates":  candidateCounts(result.Plan.Candidates),
			"explanation": retrieval.Explain(*result.Plan),
		}
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func modeStrings(modes []domain.RetrievalMode) []string {
	out := make([]string, 0, len(modes))
	for _, mode := range modes {
		out = append(out, string(mode))
	}
	return out
}

func reasonStrings(reasons map[domain.RetrievalMode]string) map[string]string {
	out := make(map[string]string, len(reasons))
	for mode, reason := range reasons {
		out[string(mode)] = reason
	}
	return out
}

func candidateCounts(counts map[domain.RetrievalMode]int) map[string]int {
	out := make(map[string]int, len(counts))
	for mode, count := range counts {
		out[string(mode)] = count
	}
	return out
}

func rankStrings(ranks map[domain.RetrievalMode]int) map[string]int {
	out := make(map[string]int, len(ranks))
	for mode, rank := range ranks {
		out[string(mode)] = rank
	}
	return out
}
