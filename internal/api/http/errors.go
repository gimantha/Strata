// Package http exposes the HTTP/JSON API.
//
// Responses use stable machine-readable error codes, and raw database or provider
// errors never reach a client (AGENTS.md section 35).
package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/observability"
)

// errorEnvelope is the single error shape every endpoint returns.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      domain.Code `json:"code"`
	Message   string      `json:"message"`
	RequestID string      `json:"request_id,omitempty"`
}

// statusForCode maps a domain code to an HTTP status.
func statusForCode(code domain.Code) int {
	switch code {
	case domain.CodeInvalidArgument:
		return http.StatusBadRequest
	case domain.CodeUnauthenticated:
		return http.StatusUnauthorized
	case domain.CodePermissionDenied:
		return http.StatusForbidden
	case domain.CodeNotFound, domain.CodeWorkspaceNotFound, domain.CodeGraphSpaceNotFound:
		return http.StatusNotFound
	case domain.CodeConflict, domain.CodeSourceEventConflict:
		return http.StatusConflict
	case domain.CodeOntologyViolation, domain.CodeTemporalConflict:
		return http.StatusUnprocessableEntity
	case domain.CodeRateLimited:
		return http.StatusTooManyRequests
	case domain.CodeProjectionNotReady, domain.CodeProviderUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// clientMessage returns text safe to send to a caller.
//
// Domain errors carry messages written for clients. Anything else is reported
// generically: an unexpected error's text may contain a query, a DSN, or source
// content, none of which belong in a response.
func clientMessage(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return "an internal error occurred"
}

// writeJSON sends a JSON response.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this can only be logged.
		s.logger.WarnContext(r.Context(), "could not write response body",
			slog.String("error", err.Error()))
	}
}

// writeError maps an error to its response, logging server-side faults with their full
// cause while returning only the client-safe message.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	code := domain.CodeOf(err)
	status := statusForCode(code)
	requestID := observability.RequestIDFromContext(r.Context())

	if status >= http.StatusInternalServerError {
		s.logger.ErrorContext(r.Context(), "request failed",
			slog.String("code", string(code)),
			slog.String("error", err.Error()),
			slog.String("path", r.URL.Path))
	} else {
		s.logger.InfoContext(r.Context(), "request rejected",
			slog.String("code", string(code)),
			slog.String("error", err.Error()),
			slog.String("path", r.URL.Path))
	}

	s.writeJSON(w, r, status, errorEnvelope{Error: errorBody{
		Code:      code,
		Message:   clientMessage(err),
		RequestID: requestID,
	}})
}

// decodeJSON reads a JSON request body strictly.
//
// Unknown fields are rejected rather than ignored: silently dropping a misspelled
// field is how a caller ends up believing it set a temporal bound or a classification
// that never took effect.
func decodeJSON(r *http.Request, limit int64, dst any) error {
	const op = "http.decodeJSON"

	body := http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return domain.Errorf(domain.CodeInvalidArgument, op, "request body is required")
		}
		return domain.Errorf(domain.CodeInvalidArgument, op, "malformed request body: %s", err.Error())
	}
	// Reject trailing content so two concatenated documents cannot be half-applied.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return domain.Errorf(domain.CodeInvalidArgument, op, "request body must contain exactly one JSON document")
	}
	return nil
}
