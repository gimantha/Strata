// Recovery classification for backup and restore (AGENTS.md section 40).
//
// Section 40's guidance rests on one distinction: the canonical ledger is authoritative and
// must survive, while derived indexes may be backed up for recovery speed but must be
// rebuildable. Written down as prose that distinction goes stale the first time somebody
// adds a table, and a backup plan that silently omits a table is discovered during a
// restore, which is the worst possible moment.
//
// So the classification lives here as data, and a test walks the live schema and fails on
// anything unclassified.

package ledger

import (
	"context"
	"slices"
	"strings"

	"github.com/gimantha/strata/internal/domain"
)

// schemaMigrationsTable records which migrations have been applied.
const schemaMigrationsTable = "schema_migrations"

// CanonicalTables must be restored from backup. Nothing regenerates them.
//
// The list is broader than "the assertion ledger", and deliberately so. Audit events are a
// record of what was disclosed, and disclosure cannot be re-derived. Retrieval traces
// explain answers already given. Resolution decisions record judgements, including human
// ones. Pipeline runs are how a replay knows what has already been done. Losing any of them
// loses history that no amount of reprocessing brings back.
func CanonicalTables() []string {
	return []string{
		// Scope, identity, and access.
		"workspaces", "graph_spaces", "collections", "principals", "principal_workspaces",
		"policy_sets", "sources",

		// The ingestion ledger and its archived material.
		"artifacts", "source_events", "episodes", "chunks",
		"pipeline_runs", "pipeline_stage_runs",

		// Knowledge: claims, what supports them, and what they disagree about.
		"entities", "entity_aliases", "entity_identifiers", "predicates", "assertions",
		"evidence", "derivations", "derivation_inputs", "conflict_sets",
		"ontology_versions",

		// Judgements and history that cannot be recomputed.
		"model_runs", "resolution_candidates", "resolution_decisions",
		"audit_events", "retrieval_traces",

		// Connector position. Losing it re-reads a stream from the beginning, which
		// idempotency survives but an operator would rather not pay for.
		"cdc_streams",

		// Work in flight. The outbox holds accepted events not yet processed, so it is
		// canonical in the only sense that matters: dropping it loses work a caller was
		// already told was durable (AGENTS.md sections 28.1, 40.5).
		"outbox_events",
	}
}

// DerivedTables can be dropped and rebuilt from the canonical tables.
//
// Backing them up is a speed decision, not a safety one: a large deployment may prefer to
// restore an index rather than replay a year of events, but it must always be able to
// choose the replay (AGENTS.md sections 2.3, 15.2, 40.4).
func DerivedTables() []string {
	return []string{
		"vector_records", "lexical_records", "graph_edges", "projection_checkpoints",
	}
}

// TableClassification reports how the live schema divides up.
type TableClassification struct {
	Canonical []string
	Derived   []string
	// Unclassified is a table the schema has and this file does not know about. Any entry
	// here is a hole in the backup guidance, not a curiosity.
	Unclassified []string
	// Missing is a table this file claims exists and the schema does not, which means the
	// guidance is describing a system that no longer exists.
	Missing []string
}

// ClassifyTables compares the classification above against the database as it stands.
//
// Reading the live schema rather than the migration files on purpose: a deployment restores
// what its database contains, and a migration that was written but never applied is not part
// of that.
func (s *Store) ClassifyTables(ctx context.Context) (TableClassification, error) {
	const op = "ledger.ClassifyTables"

	rows, err := s.pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return TableClassification{}, mapError(err, op, "cannot list tables")
	}
	defer rows.Close()

	live := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return TableClassification{}, mapError(err, op, "cannot read a table name")
		}
		live[name] = true
	}
	if err := rows.Err(); err != nil {
		return TableClassification{}, mapError(err, op, "cannot list tables")
	}

	canonical, derived := CanonicalTables(), DerivedTables()
	known := map[string]bool{}
	for _, name := range slices.Concat(canonical, derived) {
		known[name] = true
	}
	// The migration bookkeeping table belongs to neither list: it travels with the schema
	// and is restored by restoring the schema.
	known[schemaMigrationsTable] = true

	out := TableClassification{Canonical: canonical, Derived: derived}
	for name := range live {
		if !known[name] {
			out.Unclassified = append(out.Unclassified, name)
		}
	}
	for name := range known {
		if !live[name] && name != schemaMigrationsTable {
			out.Missing = append(out.Missing, name)
		}
	}
	slices.Sort(out.Unclassified)
	slices.Sort(out.Missing)
	return out, nil
}

// Complete reports whether the classification covers the schema exactly.
func (c TableClassification) Complete() bool {
	return len(c.Unclassified) == 0 && len(c.Missing) == 0
}

// Problem describes what is wrong with the classification, for an operator or a test.
func (c TableClassification) Problem() string {
	if c.Complete() {
		return ""
	}
	var parts []string
	if len(c.Unclassified) > 0 {
		parts = append(parts, "not classified as canonical or derived, so the backup "+
			"guidance does not cover them: "+strings.Join(c.Unclassified, ", "))
	}
	if len(c.Missing) > 0 {
		parts = append(parts, "classified but absent from the schema, so the guidance "+
			"describes a system that no longer exists: "+strings.Join(c.Missing, ", "))
	}
	return strings.Join(parts, "; ")
}

// DropDerived removes every derived record in a workspace, for a restore drill.
//
// Separate from DeleteProjections, which exists to support a rebuild in normal operation.
// This one is named for what a drill does: prove the derived half is genuinely disposable
// by disposing of it.
func (s *Store) DropDerived(ctx context.Context, ws domain.WorkspaceID) error {
	return s.DeleteProjections(ctx, ws)
}
