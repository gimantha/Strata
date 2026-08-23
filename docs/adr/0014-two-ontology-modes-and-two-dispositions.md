# ADR 0014: Two ontology modes, and two dispositions for what they refuse

Status: accepted
Date: 2026-08-23

## Context

AGENTS.md section 8 asks for both an open mode, where extraction invents predicate names
that a registry normalizes, and a guided mode, where extraction is constrained by known
types and validated against them. Phase 9's acceptance criteria are that the same source can
be processed either way, and that an invalid candidate is "rejected or quarantined, not
silently committed".

The "or" in that sentence is the interesting part. Rejection and quarantine are different
outcomes with different costs, and picking one for all cases gets one of them wrong.

## Decision

**Both modes exist because they answer different questions.** A new corpus needs discovery:
nobody knows what predicates it contains, and a schema written before reading it would be
wrong in ways nobody could predict. A production pipeline needs a contract: a model that
starts emitting `RUMORED_TO_SUPPLY` next Tuesday should not quietly widen the graph.
Forcing one mode on both makes exploration rigid or production sloppy.

**The mode is a property of the graph space, not the workspace.** That is what makes the
acceptance criterion testable rather than hypothetical: ingest the same document into two
spaces, one open and one guided, and compare. A workspace-level setting would make that
comparison a migration.

**Versions are immutable and sequenced.** An assertion records the version it was validated
under, so editing a version in place would silently change what committed claims were checked
against — the same class of mistake as editing a committed claim. Superseded versions stay
readable, because a claim whose schema cannot be looked up is a claim nobody can re-check.

**Rejection for callers, quarantine for models.** A caller stating a claim through the API
gets `ontology_violation` with the specific problem, because a caller can fix it and resend.
A model proposing a claim has no such loop, and discarding its output would lose evidence of
what the source actually said — so the claim is committed with status `quarantined`, carrying
the reasons, and held for review. Both satisfy "not silently committed"; they differ in who
can act next.

**Quarantined is not believed.** A held claim is not projected, so it cannot be retrieved,
so it cannot reach a context block. It is also not reconciled: a claim the schema refused must
not supersede one the schema accepted, and it must not create a conflict set against one.

**Prompting narrows; validation decides.** In guided mode the allowed vocabulary goes into
the extraction prompt, which makes a model mostly comply. Mostly is not a guarantee, so the
same schema is enforced again at commit. Neither half is sufficient: prompting alone trusts
the model, and validating alone throws away work the prompt could have prevented.

**Binding is not retroactive.** Switching a space to guided mode leaves existing claims
exactly as they are. Re-validating history against a new schema would rewrite what the system
believed at the time, which is the mistake the whole ledger exists to avoid. `Validate`
reports what a version *would* refuse, changes nothing, and says so in its output.

## Consequences

Aliases are accepted on entity types: sources disagree about whether a company is an
`organization`, an `org`, or a `company`, and rejecting a claim over that is pedantry rather
than validation. Types and predicates each have exactly one canonical form —
`lower_snake_case` and `UPPER_SNAKE_CASE` — so a schema cannot define the same thing twice
under two spellings.

Unconstrained fields stay open. A predicate that names no subject types accepts any defined
type, because an ontology that had to enumerate everything would be unusable for the loose
predicates every real corpus has.

Schema validation runs at define time and refuses the mistakes that would be invisible later:
a duplicate type, an alias that collides with another type, and — the most likely real
error — a predicate that references an undefined type. That last one is a typo that would
otherwise reject every claim using the predicate while looking like a broken extractor.

`Check` returns every violation rather than the first. A candidate with a misspelled type and
an out-of-vocabulary value has two problems, and reporting them one round trip at a time
turns one fix into several.

## Alternatives considered

**One mode, always guided.** Simpler, and it makes the first day with a new corpus miserable:
you cannot write the schema until you have read the data, and you cannot read the data
through the system until you have written the schema.

**Reject everything invalid, no quarantine.** Symmetrical and lossy. The claims a schema
refuses are exactly the ones worth looking at — either the schema is incomplete or the
extractor is drifting — and throwing them away discards the evidence for both.

**Quarantine everything invalid, no rejection.** Hides caller mistakes in a review queue
nobody reads, and returns 201 for a request that did not do what the caller asked.

**Retroactive re-validation on bind.** Tempting, and it would rewrite history: claims that
were valid when committed would silently acquire a status they never had. The report is the
useful half of that idea, and it is the half that does not lie.
