# ADR 0013: Context selection is greedy under a hard budget, with structural redundancy

Status: accepted
Date: 2026-08-23

## Context

Retrieval returns a ranked list. A prompt needs something else: a bounded amount of text that
covers as much of what was asked as the budget allows, with citations, and without saying the
same thing four times. AGENTS.md section 20.2 lists ten factors that should influence that
choice — relevance, temporal match, source diversity, coverage, redundancy, confidence,
evidence quality, contradiction awareness, memory-kind priority, and per-section caps — and
does not say how to combine them.

Two constraints shape the answer. The budget is a ceiling, not a target: a prompt that
overflows a model's context window fails outright, so exceeding it is a different class of
error from under-filling it. And the result must be explainable, because the first question
anyone asks about an assembled prompt is why some fact they expected is missing.

## Decision

**Selection is greedy, maximizing marginal value.** Each round picks the candidate with the
highest value given what has already been chosen: merit (relevance, confidence, evidence
quality, temporal fit, memory-kind priority) discounted by similarity to what is in, plus a
bonus for covering a subject or predicate nothing else covers. The exact formulation is a
knapsack with an objective that changes as items are added; greedy lands within a few percent
and can explain itself, and here that trade is worth taking.

**The budget is enforced while rendering, not estimated afterwards.** Every fragment is
billed as it is written — headings, markers, fences, reference lines. Summing the cost of the
content and rendering afterwards would miss the scaffolding, and on small budgets the
scaffolding is most of the block.

**An item is not affordable unless its citation is affordable.** The reference line is
reserved at the moment the item is written. An earlier version trimmed the reference list
when the budget ran out, which produced markers pointing at references that did not exist —
worse than omitting the item, because it looks like a citation.

**Redundancy is lexical with one structural exception.** Overlap is Jaccard similarity over
content words, which works with no embedder configured and catches the case that matters:
the same fact restated. On the evaluation corpus, four differently worded statements of one
fact score 0.58–0.70 against each other while genuinely different facts that share vocabulary
top out at 0.31, so the cutoff sits at 0.5, in the gap.

The exception: two claims about the same subject and predicate that assert *different values*
are never redundant, however similarly they read. "tier LEGACY until March" and "tier PREMIUM
since March" overlap on half their words and are exactly the pair a reader must see together.
Judging them lexically would keep one at random — and a reader cannot tell which they got.

**Sections are budgeted by share, then leftovers are redistributed.** Facts get 45%, excerpts
30%, graph 12%, conflicts 15%, history 8%. A flood of near-duplicate passages cannot push out
every fact, and a query with no contradictions does not waste the conflict share on nothing.

**Contradictions are annotated, never resolved.** A disputed claim is discounted by 15% and
rendered with the competing value beside it. Picking a side here would be the coin flip the
reconciler deliberately refuses to make (ADR 0006).

## Consequences

Measured against a top-k baseline at an equal 800-token budget, on a corpus where half the
documents restate one fact (`evaluation_test.go`):

| metric | assembled | top-k |
|---|---|---|
| distinct facts | 4 | 3 |
| mean pairwise redundancy | 0.181 | 0.438 |
| items | 4 | 6 |
| tokens used | 553 | 800 |

Fewer items, more distinct facts, and 247 tokens left unspent. That is the shape AGENTS.md
section 20.2 asks for — "prefer ten non-redundant useful facts over fifty near-duplicate
chunks" — as a measurement rather than an aspiration.

The token estimator is the weakest link. It is a heuristic with no vocabulary, biased to
over-count and declaring a 10% tolerance, because under-counting overflows a context window
at the worst possible moment while over-counting merely wastes a little room. `Estimator` is
an interface; a caller targeting a specific model should supply that model's tokenizer.

Lexical redundancy does not catch two differently worded statements of the same fact when
they share few words. That is a real limit, not an oversight: catching it needs embeddings,
embeddings are optional, and a redundancy filter that silently stops working when the
embedder is unconfigured would be worse than one with a known blind spot.

## Alternatives considered

**Fill by rank until the budget runs out.** What the baseline above does. Simple, and it
spends most of a small budget restating one fact — the measurement is in the table.

**Embedding-based MMR.** The standard answer, and better at catching paraphrase. Rejected as
the *primary* mechanism because it makes redundancy reduction depend on a configured
embedder; the lexical version runs always, and an embedding-aware variant can be layered on
later without changing the interface.

**Summarizing selected content to fit.** Cheaper on tokens, and it breaks the citation
contract: a summary of three claims cannot be quoted back to any one of them. Consolidation
into derived assertions is phase 12, where the summary becomes a claim with its own
provenance rather than prose with none.
