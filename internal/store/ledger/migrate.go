package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/migrations"
)

// migrationLockID guards concurrent migration runs. Several processes may start at
// once (a server, a worker, a CI job); all but one wait here.
const migrationLockID int64 = 8274490127

// Migration is one embedded SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// LoadMigrations reads and orders migrations from a filesystem, rejecting malformed
// names and duplicate versions rather than guessing an order.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	const op = "ledger.LoadMigrations"

	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot list migrations")
	}

	out := make([]Migration, 0, len(entries))
	seen := map[int]string{}
	for _, name := range entries {
		versionPart, rest, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok {
			return nil, domain.Errorf(domain.CodeInternal, op,
				"migration %q must be named NNNN_description.sql", name)
		}
		version, err := strconv.Atoi(versionPart)
		if err != nil || version <= 0 {
			return nil, domain.Errorf(domain.CodeInternal, op,
				"migration %q must start with a positive version number", name)
		}
		if prev, dup := seen[version]; dup {
			return nil, domain.Errorf(domain.CodeInternal, op,
				"duplicate migration version %d (%s and %s)", version, prev, name)
		}
		seen[version] = name

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot read migration "+name)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     rest,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// EmbeddedMigrations returns the migrations compiled into this binary.
func EmbeddedMigrations() ([]Migration, error) { return LoadMigrations(migrations.FS) }

// EmbeddedHead is the highest version this binary knows about. Readiness compares it
// against the database so a half-migrated deployment never serves traffic.
func EmbeddedHead() (int, error) {
	ms, err := EmbeddedMigrations()
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, nil
	}
	return ms[len(ms)-1].Version, nil
}

// Migrate applies pending migrations and returns how many ran.
//
// Each migration runs in its own transaction, so a failure leaves earlier
// migrations applied and the failing one fully rolled back. Checksums of already
// applied migrations are verified: editing a released migration is an error, not a
// silent divergence between environments.
func (s *Store) Migrate(ctx context.Context, logger *slog.Logger) (int, error) {
	const op = "ledger.Migrate"

	all, err := EmbeddedMigrations()
	if err != nil {
		return 0, err
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot acquire connection")
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return 0, mapError(err, op, "cannot acquire migration lock")
	}
	defer func() {
		// Use a detached context: the lock must be released even on cancellation.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID); err != nil && logger != nil {
			logger.WarnContext(ctx, "could not release migration lock", slog.String("error", err.Error()))
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    int PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, mapError(err, op, "cannot create schema_migrations")
	}

	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return 0, mapError(err, op, "cannot read applied migrations")
	}
	type record struct {
		name     string
		checksum string
	}
	appliedRecords := map[int]record{}
	for rows.Next() {
		var v int
		var r record
		if err := rows.Scan(&v, &r.name, &r.checksum); err != nil {
			rows.Close()
			return 0, mapError(err, op, "cannot scan applied migrations")
		}
		appliedRecords[v] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, mapError(err, op, "cannot read applied migrations")
	}

	applied := 0
	for _, m := range all {
		if rec, ok := appliedRecords[m.Version]; ok {
			if rec.checksum != m.Checksum {
				return applied, domain.Errorf(domain.CodeInternal, op,
					"migration %04d_%s was modified after being applied (checksum %s, expected %s); "+
						"add a new migration instead of editing a released one",
					m.Version, m.Name, m.Checksum, rec.checksum)
			}
			continue
		}

		if err := func() error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return mapError(err, op, "cannot begin migration transaction")
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return mapError(err, op, fmt.Sprintf("migration %04d_%s failed", m.Version, m.Name))
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
				m.Version, m.Name, m.Checksum); err != nil {
				return mapError(err, op, "cannot record migration")
			}
			return tx.Commit(ctx)
		}(); err != nil {
			return applied, err
		}

		applied++
		if logger != nil {
			logger.InfoContext(ctx, "migration applied",
				slog.Int("version", m.Version), slog.String("name", m.Name))
		}
	}

	return applied, nil
}

// SchemaVersion returns the highest applied migration version, or 0 when the
// database has never been migrated.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	const op = "ledger.SchemaVersion"

	// Probe for the table first: referencing a missing relation is a query error,
	// while an unmigrated database is a legitimate state that means version 0.
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, mapError(err, op, "cannot probe for schema_migrations")
	}
	if !exists {
		return 0, nil
	}

	var version int
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, mapError(err, op, "cannot read schema version")
	}
	return version, nil
}
