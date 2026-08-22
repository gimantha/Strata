package http

import (
	"context"
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/store/ledger"
)

// healthResponse is the liveness and readiness shape.
type healthResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks,omitempty"`
	Schema  *schemaStatus     `json:"schema,omitempty"`
}

type schemaStatus struct {
	Applied int `json:"applied_version"`
	Head    int `json:"expected_version"`
}

// handleHealth is liveness: the process is running. It touches no dependency, so a
// database outage does not cause an orchestrator to kill otherwise healthy processes.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, healthResponse{Status: "ok", Service: s.cfg.ServiceName})
}

// handleReady is readiness: this process can serve traffic right now.
//
// It verifies the database is reachable, the schema matches the migrations compiled
// into this binary, and raw payloads can be archived. A half-migrated deployment or an
// unwritable blob store must not receive ingestion it cannot honor.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	checks := map[string]string{}
	ready := true

	if err := s.ledger.Ping(ctx); err != nil {
		checks["database"] = "unreachable"
		ready = false
	} else {
		checks["database"] = "ok"
	}

	var schema *schemaStatus
	if ready {
		head, headErr := ledger.EmbeddedHead()
		applied, appliedErr := s.ledger.SchemaVersion(ctx)
		switch {
		case headErr != nil || appliedErr != nil:
			checks["schema"] = "unknown"
			ready = false
		case applied != head:
			checks["schema"] = "pending migrations"
			ready = false
		default:
			checks["schema"] = "ok"
		}
		schema = &schemaStatus{Applied: applied, Head: head}
	}

	if s.blobs != nil {
		if err := s.blobs.Healthy(ctx); err != nil {
			checks["blob_store"] = "not writable"
			ready = false
		} else {
			checks["blob_store"] = "ok"
		}
	}

	status := http.StatusOK
	body := healthResponse{Status: "ready", Service: s.cfg.ServiceName, Checks: checks, Schema: schema}
	if !ready {
		status = http.StatusServiceUnavailable
		body.Status = "not_ready"
	}
	s.writeJSON(w, r, status, body)
}
