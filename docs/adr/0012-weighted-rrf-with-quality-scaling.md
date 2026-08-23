# ADR 0012: Weighted RRF with score-quality scaling, not raw score fusion

Status: accepted
Date: 2026-08-23

## Context

Hybrid retrieval runs five retrievers over the same corpus: trigram lexical, exact phrase,
vector cosine, entity name match, and graph expansion from entity seeds. Each produces a
ranked list with an incomparable score. Trigram similarity is a fraction of shared trigrams,
cosine distance is a geometric quantity, exact match is a boolean, and graph depth is an
integer. Nothing in these numbers lives on a shared scale.

Fusing raw scores requires normalizing them, and every normalization scheme we considered was
a lie of some kind. Min-max normalization within a result set means the worst result of a
strong retriever and the worst result of a useless one both score zero. Z-scores assume a
distribution the scores do not have. Fixed rescaling constants have to be retuned whenever
the corpus changes shape.

## Decision

**Fuse by rank, using Reciprocal Rank Fusion.** Each retriever contributes `1/(k + rank)`
with `k = 60`. Rank is the one thing every retriever agrees on: it means "this retriever
considers this the nth best answer it has", and that statement is comparable across
retrievers in a way the scores are not.

**Weight retrievers by precision, not by expected usefulness.** Entity match carries the
highest weight (2.5) because an exact canonical name match is nearly always the intended
subject; exact phrase follows (1.4); lexical and vector sit at parity (1.0); graph expansion
is the lowest (0.7) because a neighbour reached by traversal is evidence about the subject,
not the subject itself. A graph result is additionally divided by depth so a two-hop
neighbour cannot outrank a directly matching record.

**Scale by score quality on top of rank.** Pure RRF discards how good a match was — the top
result of a retriever that found nothing convincing scores the same as the top result of one
that found an exact hit. Each contribution is therefore multiplied by
`(1 - q) + q * normalizedScore` with `q = 0.6`, where the normalized score is that
retriever's own score mapped to its own range. Rank still dominates; quality breaks the ties
rank cannot see.

**Reward agreement implicitly.** A record found by three retrievers accumulates three
contributions. This is the property that makes hybrid beat its parts, and it falls out of
summation rather than being engineered.

**Drop weak tails relative to the winner.** Results scoring below 35% of the top score are
discarded, because absolute score floors do not survive a change of corpus and a query with
one excellent answer should not return nine bad ones to fill the limit.

**Keep at least one result per surface where possible.** Chunks dominate on volume, so a
purely score-ordered list can bury the single entity or assertion that actually answers the
question. One slot per surface is reserved before ranking fills the rest.

## Consequences

Measured on the evaluation corpus (30 documents, 8 queries, `evaluation_test.go`):

| mode | recall@5 | MRR |
|------|----------|-----|
| hybrid | 0.875 | 0.781 |
| lexical | 0.750 | 0.656 |
| vector | 0.625 | 0.625 |
| exact | 0.375 | 0.375 |
| entity+graph | 0.250 | 0.188 |
| entity | 0.125 | 0.125 |

Hybrid strictly beats every individual mode on recall and is no worse on MRR, which the test
asserts rather than merely reports.

The weights are constants chosen by measurement on this corpus, and they are the part of this
design most likely to be wrong elsewhere. They are exposed on `retrieval.Options` for that
reason. A zero weight means "unset, use the default" rather than "disable" — disabling a
retriever is done by omitting it from the request's mode list. This was not a free choice: an
earlier version treated an unset weight as zero, every contribution multiplied out to zero,
and ranking silently degraded to tie-breaking order while still returning plausible-looking
results. Fused scores of exactly `0.00000` across every result is the signature of that
failure, and it is why `Explain` reports per-retriever ranks and signals.

## Alternatives considered

**Learned fusion weights.** Correct in principle and unjustifiable here — it needs labelled
relevance judgements the deployment does not have, and it would make ranking unexplainable
at the exact moment someone asks why a document surfaced.

**Cascade instead of fusion**: run exact, fall back to lexical, fall back to vector. Cheaper,
and it forfeits agreement entirely. A record that all three retrievers rank second would
never be seen, which is precisely the record most likely to be right.

**Normalizing scores onto a common scale and summing them.** Rejected above; the normalization
is where the untruth enters, and it enters invisibly.
