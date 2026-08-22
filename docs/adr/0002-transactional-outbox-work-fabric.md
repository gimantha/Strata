# ADR 0002: A transactional outbox as the default work fabric

Status: accepted
Date: 2026-08-22

## Context

Accepting an ingestion means promising that its processing will happen. The usual way to
break that promise is to commit a database change and then publish a message to a broker:
if the publish fails, the work is lost, and if the commit fails after publishing, work is
queued for something that never existed.

The system also has to run meaningfully on a single node, while remaining able to scale
horizontally later.

## Decision

Work is published by inserting a row into `outbox_events` inside the same transaction as
the canonical mutation that requires it. Consumers claim rows with `FOR UPDATE SKIP
LOCKED` under a lease that they heartbeat, and a reaper returns expired claims to the
queue.

`eventbus.Bus` is the interface. The PostgreSQL outbox is its default implementation;
NATS JetStream becomes a second implementation in phase 14. No broker concept reaches any
domain package.

## Alternatives

- **A message broker from the start.** Rejected for the MVP. It adds required
  infrastructure and reintroduces the dual-write problem unless an outbox is used anyway.
- **Change-data-capture on the ledger's own tables.** Rejected for now. It removes the
  explicit work-item vocabulary (attempts, error class, dead-letter state) that operators
  need, and ties the work fabric to one database's replication mechanics.
- **`LISTEN`/`NOTIFY` instead of polling.** Rejected as the primary mechanism, because
  notifications are not durable across a disconnect. It remains a possible latency
  optimization on top of the durable table.

## Trade-offs

Polling costs a small, bounded query per interval per worker, and queue throughput is
bounded by PostgreSQL rather than by a broker. In exchange, an accepted event and its
queued work cannot disagree, and the whole system runs with one dependency.

## Migration impact

A JetStream adapter must preserve the same guarantees: at-least-once delivery, per-item
attempt accounting, and a durable dead-letter state. The outbox table stays as the
transactional handoff even then, with the broker fed from it.
