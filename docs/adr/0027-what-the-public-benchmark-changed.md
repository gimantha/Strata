# ADR 0027 — What the public benchmark changed in retrieval and assembly

Status: accepted
Date: 2026-11-06

## Context

`bench/` runs Strata on public question sets (LoCoMo, LongMemEval) next to two simple
references. The cheapest check needs no model at all: does the chat message that holds the
answer reach the context at all? It is called evidence recall. On the 1,540 LoCoMo
questions, with a 4,000-token budget, Strata at `ee46602` found 36% of the evidence. Plain
BM25 keyword search over single messages, which just lists lines until the budget is full,
found 76%.

Five separate faults explained the gap. Each one was measured on the LoCoMo dev questions
(conversations 26 and 30, 233 questions) and fixed in turn. The test questions (1,307) were
not looked at until the end.

1. **Reference lines took most of the block.** Each excerpt's reference line held the chunk
   ID, the idempotency key, the source event, the sequence, the recorded time and more:
   about 120 tokens, against about 20 for a chat message. The reference list was 69% of
   the block. Each excerpt also had its own two fence lines, about 26 tokens.
2. **The budget was left a third empty.** A fixed quarter of the budget was held back for
   headings, which cost about 150 tokens. The token estimator counted 1.4 to 1.7 times what
   real tokenizers count. A fixed limit of 40 items stopped large budgets early.
3. **Every word of a question counted the same.** PostgreSQL's `ts_rank_cd` has no notion
   of how rare a word is. In a chat, the speaker's name is on half the messages and counted
   as much as "adoption".
4. **A name searched on its own flooded the list.** Each proper name in a question became an
   extra search through every retriever. A person's name in their own memory is on hundreds
   of records, so this returned hundreds of records in an order that had nothing to do with
   the question. Exact search counts by rank only and weighs 1.4, so its first hit, merely
   the shortest record with the name, outranked the best real match.
5. **Weak matches were thrown away, and replies were never shown.** Fusion drops each
   retriever's results below 35% of its best (ADR 0012). With BM25 a message that shares
   one of three rare words scores about a third of the best, so it was dropped even when
   budget was left. And in a chat, the words of a question often match the turn that asked
   about something, while the answer is in the reply, which shares no words with it.

## Decision

**Short reference lines, one fence, time next to each excerpt.** A reference line in the
text says where the item came from: source, upstream record, place in it, and for a claim
the time of its source. Every identifier stays in the structured `citations`. A question
about sources or evidence (the existing `provenance` requirement) still gets every
identifier in the text. All excerpts share one fence. Each excerpt starts with its marker
and its source time with the weekday, for example `[3] (2023-07-03 Mon 13:36)`; further
lines of a passage are indented, so a passage cannot pass a line of its own off as another
excerpt. The weekday is there because chat says "last Saturday".

**Use the budget.** Only the real cost of the header and of headings that can appear is
held back; each item pays for its own reference line. The estimator is now
`heuristic-v2`: six letters, three digits or one punctuation mark per token, plus 5%. It
lands 1.2 to 1.3 times the `o200k_base` count on chat, docs, code and rendered blocks, so
it still counts high. The default item limit is one per 40 tokens of budget, between 40
and 200. Selection keeps each candidate's highest overlap up to date after each pick
instead of comparing against everything picked so far, which made it fifteen times faster
at this size.

**BM25 in PostgreSQL.** Which records match is decided by the tsquery as before (strict,
anchored, relaxed). Only the order changed: BM25 with k1 = 1.2 and b = 0.75. How rare a word
is comes from at most 5,000 records read with the search's own filters, so statistics never
come from records the caller may not see, and the cost stays bounded on a large graph
space. For a question with no identifier, relaxed matches now fill the list after the
strict ones; before, they ran only when strict search found nothing. PostgreSQL and
OpenSearch now both rank with BM25 (see ADR 0023).

**A name on its own runs entity lookup only.** Identifier sub-queries (`CASE-42`) still run
every retriever: an identifier is in a few records. Name sub-queries exist to resolve the
name to its entity for graph expansion, and now do only that. The question's own text
search still contains the name, weighted by how rare it is.

**Context assembly keeps weak results; a plain search does not.** `QueryRequest` gains
`KeepWeakTail`. The assembler sets it. Weak results are fused among themselves and placed
after every strong result, with scores below the weakest strong one. A plain search still
drops them, as ADR 0012 decided.

**Neighbouring messages.** When a retrieved excerpt is one message of a conversation, the
message after it is offered at 0.7 of its relevance and the one before at 0.4, and a picked
neighbour is shown next to its message. A neighbour comes from the same source event, so it
shares the source and collection the caller could already see. Its classification is
filtered in the query that reads it and checked again on the record, the same two gates a
retrieved passage passes.

## Consequences

Evidence recall on LoCoMo, 4,000 tokens, no model of any kind. Test questions were not
used for any choice above.

| system | test (1,307) | dev (233) | context tokens | median latency |
|---|---|---|---|---|
| Strata `ee46602`, hashing embeddings | 36.4% | 36.3% | 3,572 | 209 ms |
| BM25 over single messages (reference) | 75.7% | 75.9% | 3,879 | 4 ms |
| Strata now, hashing embeddings | 72.8% | 74.7% | 3,296 | 209 ms |
| Strata now, no embeddings | **76.6%** | 79.7% | 3,249 | 171 ms |

On the test questions Strata without embeddings and the BM25 reference are level: the
paired difference is +0.9 points with a 95% range of −1.1 to +2.9. The dev questions,
where the choices were made, sit 3 points higher, which is the expected cost of tuning on
them. By question type on test, Strata finds more for multi-hop questions (58.5% against
48.6%) and a little less for open-domain (41.5% against 46.6%) and temporal ones (78.9%
against 80.8%).

Evidence recall is not answer accuracy. It is the step that has to work before accuracy
can, and it needs no model, so it is cheap to run after every change. Answer accuracy needs
an answer model and a judge, and has not been measured yet.

The `hashing` embedder lowers recall by about 4 points here. It is a test stand-in that
hashes words, with no notion of meaning, and its results crowd out better keyword matches
in fusion. Real embeddings have not been measured yet.

The golden retrieval file changed. A distractor reached only through the name sub-query
("berlin-office" for a question about turbines in Berlin) is gone; a few partial keyword
matches now follow a full match; exact-only search for a question that names a place but
no identifier now returns nothing, which is the honest answer.

### Later the same day: a second dataset

The same no-model check on a fixed LongMemEval sample (20 questions of each of the six
types; 30 dev, 90 test) showed Strata 6 points behind BM25 on its test questions. Its
chats have assistant replies hundreds of tokens long. BM25 measured a record's length as
its number of distinct word stems, which grows much more slowly than the text, so long
replies that repeat the question's words beat the short user messages that hold the
answer. Length is now measured in characters. Results with that change:

| | test | dev |
|---|---|---|
| LongMemEval sample, BM25 reference | 87.8% | 82.7% |
| LongMemEval sample, Strata | 84.2% (−3.6, range −10.0 to +3.0) | 86.3% |
| LoCoMo, BM25 reference | 75.7% | 75.9% |
| LoCoMo, Strata | 76.9% (+1.3, range −0.8 to +3.2) | 81.0% |

Both are ties on the test questions. On LongMemEval, Strata is still behind on questions
whose answer is in a long assistant reply (86.7% against 100%): a long reply is split into
chunks, and Strata often returns only the part that matched. Neighbour expansion is not
the cause; turning it off changed the LongMemEval sample by one point.

## Alternatives considered

**Drop the reference list for excerpts.** Cheapest, and it breaks the promise that every
marker resolves to a reference in the text (ADR 0013). Short lines keep it.

**Remove the weak-result floor everywhere.** It would help the block and hurt a plain
search, where a question with one excellent answer should not get nine poor ones.

**Neighbour expansion for all documents.** Adjacent chunks of a document can help too, but
they are much longer than chat messages and nothing measured them. Messages only, for now.

**Show a whole chat message when one of its chunks matches.** Tried for the long-reply
weak spot on LongMemEval, with a cap of a quarter of the budget per message. Blocks fell
from about 16 items to 12 and recall dropped by 3 points. Two variants did no better:
neighbours counted by message instead of by chunk (−2 points), and the rest of the same
message offered at 0.9 of the match's relevance (−1 point, inside the noise). Of the two
assistant-reply questions Strata missed, one had the answer in its context ("a 1:10
ratio"); it counts as a miss only because evidence recall wants half of a long message.
The other is a real miss: "the 27th parameter" does not match "27." as a keyword. Judging
this needs answer accuracy, not evidence recall. These variants were compared on the
sample's test questions, so none was kept.

**Keep name searches but weight them down.** Any weight leaves hundreds of records with a
voice in fusion and none of them chosen for the question. Entity lookup is what the name
search was for.

## Migration impact

No schema change and no data change. Rendered blocks look different, and anything that
parsed identifiers out of the rendered text should read `citations` instead, which carries
all of them. A caller that relied on 40 items by default can pass `max_items: 40`. Budget
reports name `heuristic-v2`. Search ranking order changes for prose queries in the
PostgreSQL lexical backend.
