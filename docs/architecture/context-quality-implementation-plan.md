# Context quality implementation plan and handoff

This document is the durable handoff for the generic context quality work discovered by
the Strata/Cognee event-history comparison. The changes improve Strata itself. The
comparison remains an external measurement and its answer keys never enter ingestion,
retrieval, or answer generation.

## Rules for continuing

- Preserve immutable assertions, evidence chains, workspace scope, and separate world,
  knowledge, source, and lifecycle clocks from `AGENTS.md`.
- Complete and test one phase before committing it. Do not rewrite the commits below.
- Never commit `comparison/.env`, `.runtime/`, `.venv/`, caches, model outputs, or result
  folders. Generated synthetic fixtures are safe to commit.
- Run `go test ./...` and `python3 -m unittest comparison/test_comparison.py` after the
  final change. Tests that use PostgreSQL may require the repository's existing test setup.

## Completed phases

1. **Auditable retrieval decisions** — `0779a3d`. Persist the actual retrieval plan and
   per-candidate channels, ranks, signals, and graph paths.
2. **Canonical predicate vocabulary** — `9d6363a`. Add predicate aliases and resolve
   spelling variants to a workspace canonical predicate.
3. **Structured relation roles** — `acf65b1`. Encode contextual/n-ary roles as stable
   qualifiers while preserving legacy scope compatibility and searchable display text.
4. **Canonical-slot corrections** — `df6cea8`. Reconcile explicit corrections by
   source, subject, predicate, and scope without depending on functional ontology flags.
5. **Query intent** — `882a2cc`. Parse identifiers, quoted anchors, answer requirements,
   and world/knowledge-time intent deterministically; explicit API filters take priority.
6. **Focused hybrid candidates** — `b50ac62`. Seed graph/entity retrieval from query
   anchors, use strict lexical matching before an explicit fallback, and bound vector tails.
7. **Focused complete context** — `6b6d7bf`. Gate unrelated identifier matches, preserve
   distinct-subject facts, expand required history/premises, and allocate budget by need.
8. **Rendered provenance** — `f53a626`. Put evidence excerpts and upstream source event,
   sequence, version, event time, and knowledge time into context and citations.
9. **Deterministic cited proofs** — `11d2edd`. Compute exact typed deltas, surface decision
   derivations with every premise, and emit bounded absence proofs after canonical checks.

## Phase 10 — regression quality and stable extraction

Status: complete in the Phase 10 commit containing this document.

- Feed bounded canonical predicate names/aliases into open-mode extraction as advisory
  vocabulary while keeping the mode open.
- Prompt for stable state subject orientation and correction slots.
- Forward explicit `valid_at`, `known_at`, and `active_at` from the comparison question to
  Strata's context endpoint; translate recorded checkpoint times to `known_at`.
- Mark a deterministic predeclared holdout cohort and provide a post-run quality gate that
  requires zero query errors, 100% valid citations, and configurable grounded accuracy
  (default 97%).
- The 1,000-event fixtures were regenerated and hashed. The focused tests, full Go suite,
  and 18 Python harness tests passed before committing.

## Final follow-up — old-knowledge purge

Status: pending and intentionally last.

Implement an auditable, workspace-scoped purge that is distinct from supersession and
privacy erasure:

1. Add a migration for legal/operational holds and durable purge runs.
2. Add a ledger plan operation that only selects complete old source-event provenance
   units whose knowledge is inactive, expired, retracted, superseded, or quarantined.
   Protect held records and events that still contain active knowledge.
3. Make dry-run the default and require cutoff, graph space, principal, and reason.
4. Execute canonical deletion transactionally in dependency order, retain the purge audit
   record, and return raw artifact blob keys for deletion.
5. Remove/rebuild derived graph, vector, and lexical projections so purged content cannot
   remain in external indexes. Persist partial failure for retry; hydrated reads must fail
   closed when a stale projection points to deleted canonical data.
6. Add admin HTTP/CLI entry points only if they fit existing wiring cleanly; otherwise land
   the tested service and ledger port first and document invocation.
7. Test dry-run, active-knowledge protection, holds, tenant isolation, dependency cleanup,
   and idempotent repeat execution. Commit this separately after Phase 10.

## Current verification state

Phases 1–10 passed their focused package tests when committed. The purge still needs its
implementation and final full-suite verification. Use `git status --short` before each commit and stage
only intended source, migrations, docs, synthetic fixtures, and tests.
