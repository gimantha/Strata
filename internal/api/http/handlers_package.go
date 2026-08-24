package http

import (
	"net/http"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/portable"
)

// handleExportPackage streams a portable context package (AGENTS.md section 29).
//
// Distinct from /export, which dumps assertions as rows for an operator to look at. A package
// is a self-contained artifact meant to rebuild knowledge somewhere else: it carries entities,
// predicates, evidence, temporal coordinates, and an integrity manifest, and it is versioned
// so a future reader can refuse one it would misread.
func (s *Server) handleExportPackage(w http.ResponseWriter, r *http.Request) {
	scope, filters, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionExport)
	if !ok {
		return
	}
	if s.exporter == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleExportPackage",
			"package export is not configured on this server"))
		return
	}

	req := portable.ExportRequest{
		Scope:             scope,
		Principal:         principalFrom(r.Context()).Ref(),
		Policy:            filters,
		IncludeSuperseded: r.URL.Query().Get("include_superseded") == "true",
		IncludeChunks:     r.URL.Query().Get("include_chunks") == "true",
		Limit:             intParam(r, "limit", 0),
		Notes:             r.URL.Query().Get("notes"),
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="context-package.jsonl"`)
	w.WriteHeader(http.StatusOK)

	result, err := s.exporter.Export(r.Context(), req, w)
	if err != nil {
		// The status line is already sent, so this cannot become an error response. The
		// package will lack its manifest, which is exactly how a reader detects it.
		s.logger.ErrorContext(r.Context(), "package export failed mid-stream",
			"error", err.Error())
		return
	}

	s.logger.InfoContext(r.Context(), "context package exported",
		"graph_space_id", string(scope.GraphSpaceID),
		"digest", result.Manifest.Digest,
		"bytes", result.Bytes)
}

// handleImportPackage reads a portable package into this graph space.
//
// Nothing is committed until the whole package has been read and its digest verified, so a
// truncated upload leaves the target unchanged rather than half-populated.
func (s *Server) handleImportPackage(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionAdmin)
	if !ok {
		return
	}
	if s.importer == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleImportPackage",
			"package import is not configured on this server"))
		return
	}

	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer body.Close()

	summary, err := s.importer.Import(r.Context(), portable.ImportRequest{
		Scope:            scope,
		Principal:        principalFrom(r.Context()).Ref(),
		SourceName:       r.URL.Query().Get("source_name"),
		DryRun:           r.URL.Query().Get("dry_run") == "true",
		AcceptPredicates: r.URL.Query().Get("accept_predicates") == "true",
	}, body)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	rejected := make([]map[string]any, 0, len(summary.Rejected))
	for _, rejection := range summary.Rejected {
		rejected = append(rejected, map[string]any{
			"kind": string(rejection.Kind), "source_id": rejection.SourceID,
			"reason": rejection.Reason,
		})
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"from": map[string]any{
			"workspace":  summary.Header.Source.WorkspaceSlug,
			"graphspace": summary.Header.Source.GraphSpaceSlug,
			"instance":   summary.Header.Source.Instance,
			"filtered":   summary.Header.Policy.Filtered,
			"excluded":   summary.Header.Policy.Excluded,
		},
		"entities":   summary.Entities,
		"assertions": summary.Assertions,
		"evidence":   summary.Evidence,
		"duplicates": summary.Duplicates,
		"rejected":   rejected,
		"summary":    summary.Describe(),
	})
}
