# ADR 0017: Decay ranks, expiry scopes, and forgetting is four operations

Status: accepted
Date: 2026-08-24

## Context

AGENTS.md section 21 asks for consolidation, decay, expiration, and forgetting, with two
constraints that shape everything else: decay must affect retrieval relevance and not
historical truth (21.2), and the four ways of forgetting "must not share one ambiguous
`delete` flag" (21.4).

Both constraints exist because the obvious implementations quietly destroy the property this
system is built on. A decay function that drops old memories from results is deletion with a
ranking function's vocabulary. A single `delete` flag makes "we stopped using this", "this
turned out to be wrong", and "we were legally required to erase this" indistinguishable a
year later, when the difference matters most.

## Decision

**Decay is a multiplier with a floor, never a filter.** A decayed memory's contribution to a
fused score is scaled by `0.5^(elapsed / half_life)`, floored at 0.2. It loses to a fresh
memory and still beats nothing. Zero would be unfindability, which is what section 21.2
forbids under a different name.

**Expiry filters; decay does not.** They look similar and mean opposite things. Decay is the
system's guess that older material is probably less relevant; expiry is the deployment saying
"stop using this after Thursday". A guess should reorder results and a statement should
exclude them, so expiry is a `WHERE` clause and decay is a multiplier.

**Lifecycle belongs to a claim, not to the document it came from.** When a working memory
expires, the source passage still says what it said — and it will say that forever, because
that is what the document contains. Expiring a claim must not rewrite the record of what was
written. This surfaced as a test failure that looked like a leak and was actually the design
working: the assertion was gone from active context, the chunk was not.

**Forgetting is four named operations, and this phase implements two.**

| Kind | What survives | Reversible |
|---|---|---|
| `deactivate` | Everything. Only the context clock moves | Yes |
| `retract` | Everything. Knowledge-time correction: it was wrong | No, but as-of queries still see it |
| `retention` | The record that something was removed | No |
| `erasure` | An audit proof, and nothing else | No |

`deactivate` is implemented here; `retract` already existed on the knowledge service and is
refused by this path rather than duplicated. The two destructive kinds are **refused with an
explanatory error**, because they need erasure jobs, projection sweeps, and their own
authorization (section 23). Shipping them as a flag on the reversible operation is how an
undoable action and an irreversible one end up one typo apart.

**Deactivation requires a reason.** A reversible operation with no recorded motive is
indistinguishable from an accident the moment anybody asks why something is missing.

**Deactivation reaches the projections.** It re-derives them through the normal path rather
than patching a projected row. A claim that is "deactivated" in the ledger and still surfacing
as current context is deactivated in name only — which is exactly what the first
implementation did, and what the acceptance test caught.

**Consolidation adds a conclusion; it never consumes the evidence.** Three episodic
observations of the same claim produce one derived semantic assertion carrying a derivation
that names all three by id. The observations stay active, unchanged, and independently
queryable. A derived fact that cannot be walked back to what produced it is an assertion
nobody can check, which is the failure mode consolidation is most prone to.

**A conclusion is never more certain than an observation.** Confidence rises with repetition
and with independent sources and is capped at 0.95. Without the cap, one unreliable source
repeating itself all day would manufacture near-certainty out of nothing.

**Consolidation is idempotent through the ledger, not through bookkeeping.** A second pass
over unchanged observations produces the same claim, which collides on its fingerprint and is
recognized as a duplicate. A background job that accumulates a copy of every conclusion each
time it runs is worse than one that never runs.

## Consequences

Three observations is the default threshold. Two is a coincidence; three is the smallest
number that distinguishes repetition from chance, and it is a constant on
`ConsolidationRule` precisely because it is a judgement rather than a discovery. `--dry-run`
exists so a threshold change can be evaluated against real data before it is adopted.

The projections gained four lifecycle columns. Without them retrieval could not tell an
expired working note from current knowledge — the context clock existed on assertions since
phase 2 and had never been enforced anywhere a user would notice.

The half-life is thirty days, deployment-wide. Per-predicate half-lives are the obvious next
refinement and are deliberately not here: one tunable that everyone understands beats twelve
that nobody audits, and there is no evidence yet about which predicates need different
curves.

## Alternatives considered

**Delete expired memories.** Simple, cheap, and it destroys the ledger's core property.
"What did we believe last March" stops being answerable the first time a cleanup job runs.

**Decay by recomputing scores at write time.** Faster to query and wrong within a day: the
weight depends on when the question is asked, not when the record was written.

**A single `delete` endpoint with a `mode` parameter.** Fewer endpoints, and it puts a
reversible operation and an irreversible one behind the same call with a string deciding
which. The enumeration exists so that choosing is deliberate.

**Consolidating with a model.** Tempting for summarization and genuinely useful later. It is
the wrong tool for "has this been said three times", which is a grouping problem with an
exact answer, and a model would make the answer nondeterministic and unreplayable.
