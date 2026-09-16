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

## Phase 11 — anchored retrieval, focused assembly, stated replacement

Status: implemented and tested on 2026-09-15; not yet committed. Motivated by the first
full run on the rebuilt 1f422ae binary (`anthropic-fastembed-batch-1000-strata-1f422ae-rebuilt`),
where Strata scored 70.1 grounded against Cognee's 72.7 and every remaining miss traced to
one of the defects below.

Retrieval (`internal/retrieval`, `internal/domain`, `internal/store`):

1. Capitalized function words ("For", "Return") and month or weekday names are no longer
   entity terms, so they no longer become sub-queries. Identifier sub-queries are queued
   before name phrases, so the sub-query cap can no longer drop the one anchor search.
2. `LexicalQuery.Anchors`: required phrases composed as a tsquery conjunction in PostgreSQL
   and as `match_phrase` musts in OpenSearch, with a conformance case. The retriever's prose
   ladder is now strict, then anchored match-any, then match-any.
3. Exact and entity streams are rank-only in fusion: their scores are not magnitudes, and
   the relative floor had been discarding every passage the exact channel found.
4. Anchored questions fuse three times wider, follow up to `MaxReferenceFollows` record
   identifiers cited by anchored passages (resolved through the ledger's idempotency keys,
   read with `ListChunks`, recorded as `SubQueryReference`), and rank anchored results ahead
   of look-alikes before the cut. Vector-only tails are demoted rather than filtered. A
   followed record carries a one-hop `cites` path and takes the traversal discount, so it
   cannot outrank a passage the question matched directly.

Context assembly (`internal/contextblock`):

5. Requirement expansion and derived proofs run only from candidates that name the
   question's identifier, closing the leak that rendered other records' history as fact.
6. Required premises offer the source passage their evidence was read from (bounded by
   `RequiredExcerptLimit`), so the checkable text is present, not only the paraphrase. The
   passage is related, not required: the first diagnostic showed required companions
   displacing the passage that answered the question.
7. Any `valid_at` question expands slot history, and a claim not yet in force at the
   requested instant renders under HISTORICAL with its start date.
8. Excerpts keep their line breaks; only blank-line and space runs collapse. Reference
   lines name the assertion or chunk, not the evidence and episode identifiers behind
   them, and print instants to the second; each line had cost about a hundred tokens and
   twenty of them left room for three passages.
9. Comparison proofs read unit-bearing text amounts ("USD 12,100") and difference them
   when the units agree.

Ledger and extraction (`internal/extraction`, `internal/knowledge`, `internal/pipeline`,
`internal/store/ledger`):

10. `replaces_prior` in the extraction schema (prompt version 3) flows to
    `Claim.ReplacesPrior`; the reconciler treats it like a correction operation and
    supersedes earlier same-slot claims from the same source, preserving them as history.
11. `EnsurePredicate` resolves spellings that differ only by a `HAS_`, `IS_`, or `WAS_`
    prefix or a `_FOR` suffix to the registered predicate and records the alias.

Harness: `run.py --projects N` generates a reduced deterministic cohort for retrieval
diagnostics; the README documents it.

Diagnostics: `strata-diagnostic-10-phase11` (first build) fixed switch-cause, approval-source,
and current-budget but crowded the excerpt budget and regressed current-supplier,
expired-contact, and late-owner; `strata-diagnostic-10-phase11-02` measures the three
follow-up fixes above. Both are 10-project, Strata-only runs and are not comparable to the
1,000-event scores.

Not done: natural-language month-day dates without a year are still not temporal
coordinates; the vector port has no anchor filter, since prioritization made it unnecessary
for the measured failures. Open-mode extraction still mis-attributes some reason codes
(a `BUDGET_AMENDMENT_REASON` carrying a supplier outage), which no retrieval change fixes.
Full comparison on this build: `anthropic-fastembed-batch-1000-strata-phase11`
(2026-09-15, 14:53 to 17:06 IST, zero query errors, all 1,000 model runs succeeded).
Equal-category grounded accuracy: Strata 71.5, Cognee 71.0 in the same run; facts 95.4 vs
87.9; citations 94.8 vs 89.6. Against the 1f422ae run Strata moved 70.1 to 71.5. Gains:
switch-cause 0 to 65, approval-source 70 to 100, current-budget 70 to 100, past-supplier 60
to 95; 229 assertions superseded with 209 supersedes links, against 5 before. Regressions:
late-owner 90 to 50, current-supplier 100 to 30, cause-chain 53 to 33. In every regressed
case the answer is right and the target passage is absent from SOURCE EXCERPTS, which now
holds two to three passages for questions that expand history: the required claims and
their reference lines take the budget. The next lever is the excerpt share and the size of
history expansion, not retrieval. Holdout gate: Strata 81.0%, Cognee 79.8%, both below 97%.

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
