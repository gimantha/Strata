# ADR 0023 — A third backend, and the two wildcard alphabets

Status: accepted
Date: 2026-08-28
Phase: 15 (advanced storage adapters)

## Context

[ADR 0022](0022-a-second-vector-backend-and-what-it-proved.md) put Qdrant behind
`index.Vectors` and argued that a port with one implementation is an assertion. The same was
true of `index.Lexical`, and rather more so: its contract is the largest of the three,
because a search index is where two engines diverge most.

The measurements have not changed. [performance.md](../api/performance.md) shows PostgreSQL
meeting every section 39 target, so this is not a fix for a bottleneck.

## Decision

**OpenSearch behind `index.Lexical`, selected by `CG_LEXICAL_BACKEND`.**

The port has two search modes in one method, and that decided the engine. Full text is
stemmed prose search; `Exact` is literal substring matching for identifiers and codes that
stemming destroys. A prefix or typo-tolerant engine — Meilisearch, Typesense — serves the
first well and cannot serve the second, and the only way to use one would be to weaken the
contract. Phase 15 says not to change canonical semantics to suit a backend, so the choice
was between an engine that could do both and no engine at all.

OpenSearch has both as two views of one field: an analyzed `content` and a `content.exact`
wildcard subfield. One document is written and read either way, which is what the port means
by "the two modes are one method because they are one index".

Three smaller decisions follow the Qdrant precedent. Document ids are a UUIDv5 over
(workspace, surface, record), so replay converges and a workspace cannot address another's
document. Writes refresh rather than merely acknowledge, because the port requires a write to
be visible to the next read. And the mapping is `dynamic: strict` — a field OpenSearch
guesses at is a field whose filter behaviour is a guess, and a date inferred as text does not
answer a range query.

Absent timestamps are omitted rather than written as sentinels, which is the opposite of the
Qdrant adapter. OpenSearch has a first-class `exists` query, so "NULL means unbounded" is
expressible directly and a sentinel date would be a value someone could stumble into. The two
adapters differ because the engines do; what has to match is the answer.

## The two wildcard alphabets

The most useful thing this backend found is a weakness in the contract that was holding it.

The reference's exact mode had a defect: it interpolated the caller's text into a `LIKE`
pattern, so `_` matched any character and `%` matched everything — in the mode that exists
*for* identifiers like `ERR_7731X`. That was fixed when the lexical suite was written, and
the suite gained a case for it.

The case tested `%` and `_`. Those are PostgreSQL's wildcards. They are **ordinary characters
to OpenSearch**, whose wildcards are `*` and `?`. So the case would have passed for an
OpenSearch adapter that escaped nothing at all, and an identifier search containing an
asterisk would have quietly matched its neighbours.

The suite now checks all four characters against every backend, each paired with a document
that a pattern interpretation would wrongly match. Both adapters escape their own alphabet.

This generalises past the specific characters: **a conformance case written from one
implementation's failure modes tests that implementation, not the contract.** The suite was
written before this backend existed and still encoded an assumption about which engine it was
describing.

A second gap surfaced the same way. Nothing in the suite distinguished a full-text query that
requires every term from one that accepts any — the fixture's records all contained all the
query's words, so conjunction and disjunction returned the same set. PostgreSQL's
`websearch_to_tsquery` defaults to AND and OpenSearch's `match` defaults to OR, so the two
would have disagreed on almost every multi-word query while the suite stayed green. There is
now a case for it, and flipping the adapter to OR fails it.

## Consequences

`index.Set` now mixes three backends: vectors in Qdrant, text in OpenSearch, graph and ledger
in PostgreSQL. Nothing above the ports knows.

**Scores are not comparable across backends.** PostgreSQL ranks with `ts_rank_cd` and
OpenSearch with BM25, so the same query returns the same records in a possibly different
order with entirely different scores. The conformance suite compares sets rather than
rankings for exactly this reason, and fusion normalises by rank rather than score — but a
deployment that has tuned retrieval weights against one backend should expect to retune.

**Referential integrity is lost on this leg too**, as it was for vectors: PostgreSQL has
foreign keys where OpenSearch has document fields.

**A third datastore is in the restore path.** The projection is derived, so a rebuild
reconstructs it and losing the cluster costs a replay rather than data.

## Alternatives considered

**A lighter engine.** Meilisearch and Typesense are a fraction of the footprint and neither
does literal substring matching. Rejected on the contract, not on preference.

**Elasticsearch.** Equivalent capability; OpenSearch was chosen for its Apache-2.0 licence,
which fits a project that keeps providers behind ports precisely so no one of them can
dictate terms.

**A separate index per workspace.** Rejected for the same reason as Qdrant's separate
collection per workspace: a search against a workspace nothing has been written to yet would
error rather than return nothing.
