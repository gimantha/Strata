# ADR 0006: Contradictions are recorded, not resolved by guesswork

Status: accepted
Date: 2026-08-23

## Context

When two claims about the same subject and predicate cannot both hold, something has to
happen. The tempting options are to keep the newest, keep the most confident, or keep the
one from the more trusted source - each of which silently destroys information the system
was asked to remember.

AGENTS.md sections 14.1 and 14.2 are specific: contradiction depends on predicate
semantics, overlapping validity, scope, authority, and cardinality, and unresolvable
conflicts must keep both claims under a conflict set.

## Decision

Phase 2 handles only the cases where the answer is unambiguous, and records everything
else:

- Values that may coexist are left alone. Two `LIKES` claims are two facts.
- A predicate whose conflict policy is `latest_wins` supersedes what it replaces, which is
  a knowledge-time change that leaves the earlier claim queryable as of any earlier
  instant.
- Anything else that genuinely overlaps in world time creates a conflict set. Both claims
  are marked `disputed`, stay believable, and remain visible to retrieval.

Overlap is computed on half-open intervals, so a claim ending exactly when the next begins
is a clean handover rather than a contradiction.

A disputed claim still counts as current belief. Hiding it would present contested
knowledge as settled; dropping it would lose the disagreement entirely.

## Alternatives

- **Highest confidence wins.** Rejected. Confidence is an estimate, and using an estimate
  to delete a fact makes the estimate unfalsifiable.
- **Most trusted source wins.** Deferred, not rejected. It needs the authority model and
  out-of-order handling that phase 5 brings; doing it here with only a trust level would
  encode a rule too crude to be right. Until then such predicates take the conflict-set
  path, which is exactly what scenario D requires.
- **Reject the new claim.** Rejected: the newcomer may well be the correct one, and
  refusing ingestion loses it entirely.
- **Assume unknown predicates are functional.** Rejected, and worth stating explicitly:
  extraction invents predicate names, and assuming exclusivity would manufacture
  contradictions that do not exist. Candidates are registered as non-functional and
  coexisting.

## Trade-offs

Conflict sets accumulate and need operator attention; nothing resolves them automatically
in this phase. That is the intended trade: an open conflict is a visible question, while an
auto-resolved one is an invisible wrong answer.

Detection runs after the commit, so a conflicting claim is briefly `active` before it is
marked `disputed`. Both claims are present throughout, so no reader ever sees knowledge
disappear - only its status settle.

## Migration impact

Phase 5 replaces `Service.reconcile` with the full reconciler. The conflict-set schema,
the disputed status, and the half-open overlap rule are expected to survive that change;
the policy table driving them is what grows.
