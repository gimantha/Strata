package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/observability"
)

type ctxKeyPrincipal struct{}

// principalFrom returns the authenticated principal for a request.
func principalFrom(ctx context.Context) domain.Principal {
	if p, ok := ctx.Value(ctxKeyPrincipal{}).(domain.Principal); ok {
		return p
	}
	return domain.Principal{}
}

// authenticated wraps a handler so it only runs for an authenticated caller.
//
// Authentication is required before anything else happens, including reading the body:
// an unauthenticated request must not be able to make the server do work
// (AGENTS.md section 10.2).
func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.identity.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="strata"`)
			s.writeError(w, r, err)
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyPrincipal{}, principal)
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(attribute.String("strata.principal_id", string(principal.ID)))
		}
		next(w, r.WithContext(ctx))
	}
}

// withObservability starts a span, assigns a request id, and records access metrics.
func (s *Server) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Continue an inbound trace when the caller supplies one.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagationCarrier{r.Header})

		route := r.Method + " " + routePattern(r)
		ctx, span := s.tracer.Start(ctx, route, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = domain.NewUUIDString()
		}
		ctx = observability.WithRequestID(ctx, requestID)
		w.Header().Set("X-Request-Id", requestID)

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		elapsed := time.Since(start)

		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
			attribute.Int("http.response.status_code", recorder.status),
		)

		if s.metrics != nil {
			attrs := metric.WithAttributes(
				attribute.String("route", route),
				attribute.Int("status", recorder.status),
			)
			s.metrics.HTTPRequests.Add(ctx, 1, attrs)
			s.metrics.HTTPDuration.Record(ctx, elapsed.Seconds(), attrs)
		}

		s.logger.DebugContext(ctx, "request handled",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", elapsed))
	})
}

// withRecovery converts a panic into a 500 rather than dropping the connection, and
// logs it so the fault is visible.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.ErrorContext(r.Context(), "panic while handling request",
					slog.Any("panic", recovered),
					slog.String("path", r.URL.Path))
				s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http", "an internal error occurred"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// routePattern returns the matched route pattern, which keeps identifiers out of
// metric and span names.
func routePattern(r *http.Request) string {
	if pattern := r.Pattern; pattern != "" {
		return pattern
	}
	return r.URL.Path
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.written {
		s.status = status
		s.written = true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// propagationCarrier adapts http.Header to the trace propagation interface.
type propagationCarrier struct{ h http.Header }

func (c propagationCarrier) Get(key string) string { return c.h.Get(key) }
func (c propagationCarrier) Set(key, value string) { c.h.Set(key, value) }
func (c propagationCarrier) Keys() []string {
	out := make([]string, 0, len(c.h))
	for k := range c.h {
		out = append(out, k)
	}
	return out
}
