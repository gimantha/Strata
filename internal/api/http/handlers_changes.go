package http

import (
	"context"
	"net/http"

	"github.com/gimantha/strata/internal/connector/cdc"
	"github.com/gimantha/strata/internal/domain"
)

// resolveSourceID accepts either form of source reference.
//
// The gateway does this too, but a connector run needs the id before the first event is
// built, and resolving it once per batch beats resolving it per change.
func (s *Server) resolveSourceID(ctx context.Context, ws domain.WorkspaceID, id, name string) (domain.SourceID, error) {
	const op = "http.resolveSourceID"

	switch {
	case id != "":
		source, err := s.ledger.GetSource(ctx, ws, domain.SourceID(id))
		if err != nil {
			return "", err
		}
		return source.ID, nil
	case name != "":
		source, err := s.ledger.GetSourceByName(ctx, ws, name)
		if err != nil {
			return "", err
		}
		return source.ID, nil
	default:
		return "", domain.Errorf(domain.CodeInvalidArgument, op,
			"source_id or source_name is required")
	}
}

type changesRequest struct {
	SourceName string               `json:"source_name,omitempty"`
	SourceID   string               `json:"source_id,omitempty"`
	Stream     string               `json:"stream,omitempty"`
	Changes    []domain.ChangeEvent `json:"changes"`
}

// handleChanges accepts a batch of row changes from a push connector
// (AGENTS.md sections 10.1 and 11).
//
// A push connector is the other half of the pull adapter: some upstreams can be read from,
// and some can only write to you. Both land in the same place through the same gateway, so
// idempotency, archival, and the outbox commit are identical either way.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}
	if s.connector == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleChanges",
			"the change connector is not configured on this server"))
		return
	}

	var req changesRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(req.Changes) == 0 {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleChanges",
			"at least one change is required"))
		return
	}

	source, err := s.resolveSourceID(r.Context(), scope.WorkspaceID, req.SourceID, req.SourceName)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	result, err := s.connector.Run(r.Context(), cdc.Request{
		Scope:     scope,
		Principal: principalFrom(r.Context()).Ref(),
		SourceID:  source,
		Stream:    req.Stream,
	}, cdc.NewReplayEvents("push", req.Changes))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	events := make([]string, 0, len(result.Events))
	for _, id := range result.Events {
		events = append(events, string(id))
	}

	// 202: the changes are durable, and turning them into knowledge happens on the
	// pipeline. Reporting 200 would imply the graph already reflects them.
	s.writeJSON(w, r, http.StatusAccepted, map[string]any{
		"consumed":   result.Consumed,
		"accepted":   result.Accepted,
		"duplicates": result.Duplicates,
		"skipped":    result.Skipped,
		"events":     events,
	})
}
