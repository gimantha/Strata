package http

import (
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

type contextRequest struct {
	Query string `json:"query"`

	ValidAt  *time.Time `json:"valid_at,omitempty"`
	KnownAt  *time.Time `json:"known_at,omitempty"`
	ActiveAt *time.Time `json:"active_at,omitempty"`

	Surfaces        []string `json:"surfaces,omitempty"`
	Classifications []string `json:"classifications,omitempty"`
	MemoryKinds     []string `json:"memory_kinds,omitempty"`
	Predicates      []string `json:"predicates,omitempty"`
	MinConfidence   float64  `json:"min_confidence,omitempty"`

	TokenBudget int      `json:"token_budget,omitempty"`
	MaxItems    int      `json:"max_items,omitempty"`
	Sections    []string `json:"sections,omitempty"`
	Explain     bool     `json:"explain,omitempty"`
}

// handleContext returns a prompt-ready context block (AGENTS.md sections 20 and 25.2).
//
// /query returns what matched; this returns what is worth spending a token budget on, with
// the citations needed to check it. They are separate endpoints because they fail
// differently: a disappointing query is a ranking problem, a disappointing block is usually
// a budget or redundancy problem, and debugging either through the other is miserable.
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}
	if s.assembler == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeProjectionNotReady, "http.handleContext",
			"context assembly is not configured on this server"))
		return
	}

	var req contextRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	assembly := domain.ContextRequest{
		Scope:     scope,
		Query:     req.Query,
		Principal: principalFrom(r.Context()).Ref(),
		Temporal: domain.TemporalQuery{
			ValidAt: req.ValidAt, KnownAt: req.KnownAt, ActiveAt: req.ActiveAt,
		},
		Filters:     domain.QueryFilters{MinConfidence: req.MinConfidence, Predicates: req.Predicates},
		TokenBudget: req.TokenBudget,
		MaxItems:    req.MaxItems,
		Explain:     req.Explain,
	}

	for _, section := range req.Sections {
		parsed, err := domain.ParseContextSection(section)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		assembly.Sections = append(assembly.Sections, parsed)
	}
	for _, surface := range req.Surfaces {
		parsed, err := domain.ParseSurface(surface)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		assembly.Filters.Surfaces = append(assembly.Filters.Surfaces, parsed)
	}
	for _, classification := range req.Classifications {
		parsed, err := domain.ParseClassification(classification)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		assembly.Filters.Classifications = append(assembly.Filters.Classifications, parsed)
	}
	for _, kind := range req.MemoryKinds {
		parsed, err := domain.ParseMemoryKind(kind)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		assembly.Filters.MemoryKinds = append(assembly.Filters.MemoryKinds, parsed)
	}

	block, err := s.assembler.Assemble(r.Context(), assembly)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	body := map[string]any{
		"context":   block.Text,
		"items":     contextItemsJSON(block.Items, req.Explain),
		"citations": citationsJSON(block.Citations),
		"budget": map[string]any{
			"limit":       block.Budget.Limit,
			"used":        block.Budget.Used,
			"scaffolding": block.Budget.Scaffolding,
			"by_section":  sectionTokensJSON(block.Budget.BySection),
			"estimator":   block.Budget.Estimator,
			"tolerance":   block.Budget.Tolerance,
		},
	}
	if req.Explain {
		body["dropped"] = droppedJSON(block.Dropped)
		if block.Plan != nil {
			body["plan"] = map[string]any{
				"modes":      modeStrings(block.Plan.Modes),
				"reasons":    reasonStrings(block.Plan.Reasons),
				"skipped":    reasonStrings(block.Plan.Skipped),
				"candidates": candidateCounts(block.Plan.Candidates),
			}
		}
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func contextItemsJSON(items []domain.ContextItem, explain bool) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry := map[string]any{
			"marker":    item.Marker,
			"section":   string(item.Section),
			"text":      item.Text,
			"surface":   string(item.Surface),
			"record_id": item.RecordID,
			"tokens":    item.Tokens,
			// Trusted says whether this line is a claim the graph holds or bytes a
			// document contained. A caller re-rendering the items itself must keep that
			// boundary, so it travels with the item rather than only with the text.
			"trusted": item.Section.Trusted(),
		}
		if item.Conflict != nil {
			entry["conflict"] = map[string]any{
				"conflict_set_id": string(item.Conflict.ConflictSetID),
				"reason":          item.Conflict.Reason,
				"contradicted_by": item.Conflict.Others,
			}
		}
		if explain {
			entry["relevance"] = item.Relevance
			entry["selection"] = item.Selection
			entry["redundancy"] = item.Redundancy
			entry["signals"] = item.Signals
		}
		out = append(out, entry)
	}
	return out
}

func citationsJSON(citations []domain.Citation) []map[string]any {
	out := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		entry := map[string]any{
			"marker":  citation.Marker,
			"surface": string(citation.Surface),
		}
		if citation.AssertionID != nil {
			entry["assertion_id"] = string(*citation.AssertionID)
		}
		if len(citation.EvidenceIDs) > 0 {
			evidence := make([]string, 0, len(citation.EvidenceIDs))
			for _, id := range citation.EvidenceIDs {
				evidence = append(evidence, string(id))
			}
			entry["evidence_ids"] = evidence
		}
		if citation.ChunkID != nil {
			entry["chunk_id"] = string(*citation.ChunkID)
		}
		if citation.EpisodeID != nil {
			entry["episode_id"] = string(*citation.EpisodeID)
		}
		if citation.SourceName != "" {
			entry["source_name"] = citation.SourceName
		}
		if citation.Locator != "" {
			entry["locator"] = citation.Locator
		}
		if citation.Quote != "" {
			entry["quote"] = citation.Quote
		}
		if citation.ValidFrom != nil {
			entry["valid_from"] = citation.ValidFrom
		}
		if citation.ValidTo != nil {
			entry["valid_to"] = citation.ValidTo
		}
		if citation.Status != "" {
			entry["status"] = string(citation.Status)
		}
		if citation.Confidence > 0 {
			entry["confidence"] = citation.Confidence
		}
		out = append(out, entry)
	}
	return out
}

func droppedJSON(dropped []domain.DroppedItem) []map[string]any {
	out := make([]map[string]any, 0, len(dropped))
	for _, item := range dropped {
		out = append(out, map[string]any{
			"surface":    string(item.Surface),
			"record_id":  item.RecordID,
			"section":    string(item.Section),
			"reason":     string(item.Reason),
			"detail":     item.Detail,
			"relevance":  item.Relevance,
			"redundancy": item.Redundancy,
		})
	}
	return out
}

func sectionTokensJSON(counts map[domain.ContextSection]int) map[string]int {
	out := make(map[string]int, len(counts))
	for section, tokens := range counts {
		out[string(section)] = tokens
	}
	return out
}
