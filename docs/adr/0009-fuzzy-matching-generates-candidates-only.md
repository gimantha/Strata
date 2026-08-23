# ADR 0009: Fuzzy name matching generates candidates and never resolves

Status: accepted
Date: 2026-08-23

## Context

Entity resolution decides which identity a mention refers to. AGENTS.md section 12 opens by
calling it the highest-risk component in the system and instructing that it be implemented
conservatively, and section 12.1 lays out a ladder of evidence to prefer, with fuzzy
matching at rung 5.

The risk is asymmetric in a way that is easy to underweight. Creating a duplicate identity
is visible and mergeable. Merging two identities that are not the same thing corrupts every
fact about both, produces no error, and is close to undetectable afterwards - the graph just
quietly says wrong things.

The question this settles is whether a sufficiently close name match may resolve on its own.

## Decision

It may not. Rung 5 generates candidates and records them; it never decides.

Automatic resolution comes only from evidence that actually identifies a record:

1. an upstream system's own primary key;
2. a configured business key such as an email address;
3. an exact match on a known name, after case and whitespace normalization.

A mention that reaches rung 5 gets a new identity, with every near miss recorded in the
decision ledger for a human to merge later.

## The measurement that decided it

Trigram similarity, measured with `pg_trgm` on PostgreSQL 16:

| Comparison | Similarity |
|---|---|
| `acme corporation` vs `acme corporatoin` (transposition typo) | **0.619** |
| `alice chen` vs `alice chan` (two different people) | **0.571** |
| `globex industries` vs `globex industies` (dropped letter) | 0.750 |
| `wolfeschlegelsteinhausenberger` + one character | 0.909 |

The typo and the two different people are 0.05 apart. Any threshold low enough to catch
`corporatoin` also merges Alice Chen with Alice Chan, and the merge is the outcome that
cannot be undone by noticing later - by then facts about both have been read, cited, and
acted on as if they described one person.

The original implementation used a 0.92 threshold with a margin requirement over the
runner-up. The numbers show that threshold catches almost no real typos while still being a
threshold, which is the worst of both: complexity that buys nothing.

## Alternatives

- **A high threshold with a margin requirement.** Implemented first, then removed. It
  resolves so few real cases that it is not worth the risk surface it carries.
- **Blending all evidence into one score.** Rejected. It lets a very high name similarity
  outvote the absence of a key match, which inverts the ladder: weak evidence should never
  overrule strong evidence's silence.
- **Better string metrics** (Jaro-Winkler, Levenshtein, phonetic keys). These would shift
  the numbers without changing the conclusion, because the overlap is not an artifact of
  trigrams: two different people really do have more similar names than one person and
  their typo, often enough that no lexical metric separates them.
- **LLM adjudication of close candidates.** This is rung 8 and is genuinely the right answer
  for the ambiguous middle. It needs the candidate generation this phase provides, plus
  care that the adjudicator sees provenance rather than just two strings. Deferred, not
  rejected.

## Consequences

Duplicate identities will accumulate where sources give no stable key. That is the intended
trade: a duplicate is a visible, reversible imperfection, and a wrong merge is neither.

The decision ledger becomes the review queue. Every ambiguous outcome records the identities
it might have been, so merging later is a lookup rather than an investigation.

Sources that supply stable keys resolve perfectly and never reach rung 5. The practical
advice this implies - configure domain keys, pass through upstream identifiers - is worth
more than any similarity threshold.

A related rule falls out of the same reasoning: when a mention carries a key that matched
nothing, name matching may not bind it to an identity that already holds a *different* key
in that namespace. The source has already said those are two records, and a name should not
be allowed to contradict that.

## Migration impact

None. `MethodFuzzyAlias` was removed from the resolution vocabulary rather than left
unreachable; when adjudication arrives it will introduce its own method name recording that
a candidate was confirmed rather than merely generated.
