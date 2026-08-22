# ADR 0003: An assertion-first domain model

Status: accepted
Date: 2026-08-22

## Context

Most context-graph implementations make the graph edge the unit of knowledge: a triple,
perhaps with a timestamp and a confidence score attached. That model cannot express what
this system is required to express - what was true in the world versus what the system
believed and when, why it believed it, which source said so, whether two sources
disagree, and whether a statement was observed or inferred.

This decision is recorded now, before assertions are implemented, because the ingestion
model built in phase 1 has to lead into it rather than around it.

## Decision

The unit of knowledge is an immutable `Assertion`: subject, predicate, typed object,
memory kind, temporal coordinates across four clock layers, confidence, status,
provenance mode, and links to its evidence and derivation.

Corrections never mutate an assertion. They create a new one and mark the old one
superseded, which changes knowledge time without rewriting world validity. Graph edges,
vectors, and lexical documents become projections of assertions, and triples are one
possible projection rather than the model itself.

Object values keep their types: entity reference, string, integer, decimal, boolean,
timestamp, date, duration, geo coordinate, JSON, URI, or symbol. Values are not
stringified.

## Alternatives

- **Timestamped triples.** Rejected. It cannot answer "what did we believe on April 10
  about March 25", which is a required query.
- **Mutable entity properties.** Rejected. Overwriting a property destroys history, and
  the facts most worth tracking are exactly the ones that change.
- **Reification only inside the graph store.** Rejected. It would make the graph
  authoritative and the ledger derivative, inverting the projection rule in ADR 0001.

## Trade-offs

Assertions are heavier than edges: more rows, more joins, and more work in the reconciler
that decides how new knowledge relates to old. Reads are also more complex, since a
"current" answer is a query over temporal state rather than a lookup.

That cost is the point. Contradiction handling, supersession, and as-of queries are the
product; a model that cannot represent them cheaply cannot represent them at all.

## Migration impact

Phase 1 records episodes and chunks with exact positional provenance, which is what
phase 2's evidence records will point at. Nothing in the ingestion path needs to change
when assertions arrive.
