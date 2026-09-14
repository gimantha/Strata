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
10. **Regression quality and stable extraction** — `5c683cb`. Reuse canonical predicates
    during open extraction, forward temporal filters through the adapter, and add a fixed
    holdout cohort with a deterministic quality gate.

## Phase 10 — regression quality and stable extraction

Status: complete in `5c683cb`.

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

Status: complete in the final implementation commit containing this document.

Implement an auditable, workspace-scoped purge that is distinct from supersession and
privacy erasure:

1. Added migration `0019` for legal/operational holds and durable purge runs.
2. Added a ledger plan operation that only selects complete old source-event provenance
   units whose knowledge is inactive, expired, retracted, superseded, or quarantined.
   Protect held records and events that still contain active knowledge.
3. Dry-run is the HTTP default; cutoff, graph space, principal, reason, and a 30-day minimum
   age are required.
4. Canonical deletion executes transactionally in dependency order, retains the purge audit
   record, and returns raw artifact blob keys for deletion.
5. Derived graph, vector, and lexical projections are rebuilt workspace-wide so purged
   content cannot remain in external indexes. Partial failure is durable and retryable;
   hydrated reads fail closed when a stale projection points to deleted canonical data.
6. Added admin HTTP and CLI plan/run/hold/release/retry entry points.
7. Unit tests cover dry-run, safety age, external cleanup, durable failure, and retry. A real
   PostgreSQL integration test covers active-knowledge protection, holds, tenant isolation,
   retained supersession dependencies, deletion, blobs, and audit.

## Current verification state

Phases 1–10 passed their focused package tests when committed. Integration follow-up
`650b8e2` enforces explicit retrieval-mode boundaries and compares fusion scores within each
retriever/query stream; it closes the policy leak and restores decomposed-query retrieval.

The final PostgreSQL-backed `go test ./...` suite passed with migration 0019 applied,
including purge, recovery classification, graph-policy, and retrieval integration tests.
The comparison harness passed all 18 unit tests with
`(cd comparison && .venv/bin/python -m unittest test_comparison.py)`, and
`scripts/check-domain-deps.sh` passed. The working tree should be clean after the final purge
commit. No paid-provider benchmark rerun is required to verify these implementation changes.
