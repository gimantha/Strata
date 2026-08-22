// Package ledger is the canonical persistence layer.
//
// PostgreSQL is the authoritative store: graph, vector, lexical, summary, and cache
// state are rebuildable projections of what lives here (AGENTS.md sections 2.3, 15).
// SQL is written explicitly rather than through an ORM so temporal and index behavior
// stays visible in review (section 34).
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
)

// Store is the canonical ledger handle.
type Store struct {
	pool *pgxpool.Pool
}

// Open creates a connection pool. The caller owns Close.
func Open(ctx context.Context, cfg config.Config) (*Store, error) {
	const op = "ledger.Open"

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		// The DSN may contain a password; never echo it back in an error.
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "invalid database URL")
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot create connection pool")
	}
	return &Store{pool: pool}, nil
}

// NewStore wraps an existing pool, which integration tests use to share a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying pool for the migration runner and tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all connections.
func (s *Store) Close() { s.pool.Close() }

// Ping verifies the database is reachable, backing the readiness endpoint.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, "ledger.Ping", "database unreachable")
	}
	return nil
}

// InTx runs fn inside a transaction, committing on success and rolling back on any
// error or panic. Every multi-table canonical mutation goes through here so its
// outbox row cannot be committed separately from the mutation itself.
func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	const op = "ledger.InTx"

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot begin transaction")
	}

	committed := false
	defer func() {
		if !committed {
			// Best effort: the context may already be cancelled.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot commit transaction")
	}
	committed = true
	return nil
}

// PostgreSQL error codes this package reacts to.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgSerializationFail   = "40001"
	pgDeadlockDetected    = "40P01"
)

// isNoRows reports whether a query returned nothing.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// isUniqueViolation reports whether err is a unique-index conflict, optionally on a
// specific constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// mapError converts a driver error into a domain error with a client-safe code.
// Raw database errors never reach a client (AGENTS.md section 35).
func mapError(err error, op, message string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wrap(err, domain.CodeNotFound, op, message)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return domain.Wrap(err, domain.CodeConflict, op, message)
		case pgForeignKeyViolation:
			return domain.Wrap(err, domain.CodeInvalidArgument, op, message)
		case pgSerializationFail, pgDeadlockDetected:
			// Retryable: the caller's work was correct, the timing was not.
			return domain.Wrap(err, domain.CodeConflict, op, message)
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, message)
	}
	return domain.Wrap(err, domain.CodeInternal, op, message)
}

// jsonMap normalizes an optional metadata map for a NOT NULL jsonb column.
// map[string]any is used only for genuinely extensible metadata (section 34).
func jsonMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// jsonValue marshals a value for a jsonb column, returning an empty object rather
// than failing a whole transaction over unserializable metadata.
func jsonValue(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal jsonb value: %w", err)
	}
	return b, nil
}

// nullableString maps "" to NULL for optional foreign keys held as typed strings.
func nullableString[T ~string](v T) *string {
	if string(v) == "" {
		return nil
	}
	s := string(v)
	return &s
}
