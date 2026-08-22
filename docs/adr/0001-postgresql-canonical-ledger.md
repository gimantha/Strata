# ADR 0001: PostgreSQL as the canonical ledger

Status: accepted
Date: 2026-08-22

## Context

The architecture requires one authoritative store of canonical knowledge, with graph,
vector, and lexical stores as rebuildable projections of it. That store must support
transactional writes across several tables, a transactional outbox, precise temporal
querying, and eventually vector and full-text search, without requiring an operator to
run four databases to try the system.

## Decision

PostgreSQL is the canonical ledger. SQL is written explicitly and reviewed as part of the
design, with no ORM between the schema and the code.

Projections for later phases will also start in PostgreSQL: pgvector for vectors,
built-in full-text search for lexical retrieval, and adjacency tables with recursive CTEs
for graph traversal. Each sits behind its own interface so a dedicated backend can
replace it without touching canonical semantics.

## Alternatives

- **A dedicated graph database as the source of truth.** Rejected. It would make the
  graph authoritative rather than a projection, which inverts the core design: assertions
  with literal objects and rich temporal metadata do not map cleanly onto property-graph
  edges, and rebuilding would no longer be possible.
- **A document store.** Rejected. Multi-table transactional writes with an outbox are the
  central correctness mechanism here, and they are exactly what document stores make
  awkward.
- **Separate stores from the start** (Postgres plus a graph database plus a vector
  database). Rejected for the MVP. The contract requires a developer to run a meaningful
  complete system with PostgreSQL and a model provider alone.

## Trade-offs

PostgreSQL will not match a specialized vector or graph engine at large scale. That is
accepted: the projection interfaces exist precisely so those backends can be added when
measurements justify them, and the canonical ledger is unaffected when they are.

An ORM would have been faster to write. It would also have hidden the index and temporal
behavior that this system's correctness depends on, which is why the schema is hand-written.

## Migration impact

None yet. Replacing the canonical store later would mean re-implementing the ledger
interface and migrating data; the projection stores are rebuildable and would not need
migration.
