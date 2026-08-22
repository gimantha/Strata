package domain

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error code returned to clients
// (AGENTS.md section 35). Raw database or provider errors are never exposed.
type Code string

const (
	CodeInvalidArgument     Code = "invalid_argument"
	CodeUnauthenticated     Code = "unauthenticated"
	CodePermissionDenied    Code = "permission_denied"
	CodeWorkspaceNotFound   Code = "workspace_not_found"
	CodeGraphSpaceNotFound  Code = "graph_space_not_found"
	CodeSourceEventConflict Code = "source_event_conflict"
	CodeOntologyViolation   Code = "ontology_violation"
	CodeTemporalConflict    Code = "temporal_conflict"
	CodeProjectionNotReady  Code = "projection_not_ready"
	CodeRateLimited         Code = "rate_limited"
	CodeProviderUnavailable Code = "provider_unavailable"
	CodeInternal            Code = "internal"

	// Additions to the section 35 list that this phase genuinely needs.
	CodeNotFound Code = "not_found"
	CodeConflict Code = "conflict"
)

// Error is a domain error carrying a client-safe code, the operation that failed,
// and an optional wrapped cause.
type Error struct {
	Code    Code
	Op      string
	Message string
	Err     error
}

func (e *Error) Error() string {
	switch {
	case e.Op != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %s: %v", e.Op, e.Code, e.Message, e.Err)
	case e.Op != "":
		return fmt.Sprintf("%s: %s: %s", e.Op, e.Code, e.Message)
	case e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	default:
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds a domain error with a formatted message.
func Errorf(code Code, op, format string, a ...any) *Error {
	return &Error{Code: code, Op: op, Message: fmt.Sprintf(format, a...)}
}

// Wrap attaches a code and operation to a cause, preserving the chain for %w.
// It returns nil when err is nil so callers can wrap unconditionally.
func Wrap(err error, code Code, op, message string) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Op: op, Message: message, Err: err}
}

// CodeOf returns the outermost domain code in the chain, defaulting to internal
// for errors that never passed through the domain.
func CodeOf(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	if err == nil {
		return ""
	}
	return CodeInternal
}

// IsCode reports whether err carries the given code anywhere in its chain.
func IsCode(err error, code Code) bool { return CodeOf(err) == code }

// ErrorClass drives retry behavior for asynchronous work (AGENTS.md section 28.4).
// It is persisted on outbox and stage rows so operators can triage failures.
type ErrorClass string

const (
	ErrorClassTransient         ErrorClass = "transient"
	ErrorClassRateLimited       ErrorClass = "rate_limited"
	ErrorClassInvalidSourceData ErrorClass = "invalid_source_data"
	ErrorClassSchema            ErrorClass = "schema"
	ErrorClassPolicy            ErrorClass = "policy"
	ErrorClassModelValidation   ErrorClass = "model_validation"
	ErrorClassStorageConflict   ErrorClass = "storage_conflict"
	ErrorClassInternal          ErrorClass = "internal"
)

var errorClasses = []ErrorClass{
	ErrorClassTransient, ErrorClassRateLimited, ErrorClassInvalidSourceData,
	ErrorClassSchema, ErrorClassPolicy, ErrorClassModelValidation,
	ErrorClassStorageConflict, ErrorClassInternal,
}

func ParseErrorClass(s string) (ErrorClass, error) {
	return parseEnum("error class", s, errorClasses)
}

// Retryable reports whether work that failed with this class should be retried.
// Bad input, schema violations, and policy rejections never become valid by
// waiting, so they go straight to the dead-letter state.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ErrorClassTransient, ErrorClassRateLimited, ErrorClassStorageConflict, ErrorClassInternal:
		return true
	default:
		return false
	}
}

// ClassifyError maps an error to its retry class via its domain code.
func ClassifyError(err error) ErrorClass {
	switch CodeOf(err) {
	case CodeInvalidArgument, CodeNotFound, CodeWorkspaceNotFound, CodeGraphSpaceNotFound:
		return ErrorClassInvalidSourceData
	case CodeOntologyViolation:
		return ErrorClassSchema
	case CodePermissionDenied, CodeUnauthenticated:
		return ErrorClassPolicy
	case CodeRateLimited:
		return ErrorClassRateLimited
	case CodeProviderUnavailable:
		return ErrorClassTransient
	case CodeConflict, CodeSourceEventConflict, CodeTemporalConflict:
		return ErrorClassStorageConflict
	default:
		return ErrorClassInternal
	}
}
