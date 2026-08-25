# Distributed mode

Running Strata as a fleet: several worker processes, optional push delivery through NATS
JetStream, ordered processing of upstream records, and backpressure that keeps a scaled-out
deployment inside what its dependencies can serve (AGENTS.md sections 27.5, 28, phase 14).

The short version: **the ledger hands out work; the broker only says work exists.** Turning
NATS off costs latency and nothing else. See
[ADR 0019](../adr/0019-the-broker-carries-notice-the-ledger-carries-work.md) for why.

## Processes

| Process | Role |
|---|---|
| `contextgraphd` | HTTP API. Accepts and archives, does not process unless `CG_EMBEDDED_WORKER=true`. |
| `cgworker` | Consumes the outbox and runs the pipeline. Run as many as the workload needs. |
| `cgmcp` | MCP server over stdio. |
| `cgctl` | Administration and inspection. |

A worker logs its delivery mode at startup — `poll` or `push+poll` — so a latency complaint
can be traced to a missing broker without reading configuration.

```
starting cgworker worker_id=host/4021/x8k2qm delivery=push+poll
```

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CG_WORKER_CONCURRENCY` | `4` | Handlers in flight per worker |
| `CG_WORKER_BATCH_SIZE` | `16` | Maximum items claimed per poll |
| `CG_WORKER_LEASE` | `60s` | How long a claim is held before it can be reclaimed |
| `CG_WORKER_POLL_INTERVAL` | `500ms` | Base interval between claims |
| `CG_WORKER_IDLE_BACKOFF_MAX` | `5s` | How far the interval stretches while the queue is empty |
| `CG_WORKER_MAX_EVENTS_PER_SECOND` | `0` | Per-worker intake cap; `0` is uncapped |
| `CG_NATS_URL` | *(empty)* | Enables push delivery |
| `CG_NATS_STREAM` | `STRATA_WORK` | JetStream stream name |
| `CG_NATS_DURABLE` | `strata-workers` | Consumer group; workers sharing it share the queue |
| `CG_NATS_ACK_WAIT` | `60s` | Redelivery deadline for an unacknowledged notification |
| `CG_NATS_MAX_ACK_PENDING` | `256` | Unacknowledged deliveries allowed per consumer |

`CG_NATS_URL` is validated at startup: a configured broker that cannot be reached fails the
process rather than falling back to polling, because silently degrading would surface as an
unexplained latency regression weeks later.

Run one locally with `scripts/dev-nats.sh start` (port 14222, JetStream enabled).

## Exactly once, without duplicate knowledge

Claims use `FOR UPDATE SKIP LOCKED` against `outbox_events`, so any number of workers share
one queue without handing the same item to two of them. A worker that dies mid-flight
leaves its claim behind; the lease expires and the reaper — which every worker runs, so
recovery does not depend on one designated process being alive — returns the item to the
queue.

Reprocessing is therefore possible and safe. Handlers are idempotent by stage key: an item
delivered twice produces one episode and one set of chunks, which is the property the fleet
tests assert rather than the number of times a handler ran.

## Ordering: partition keys

`outbox_events.partition_key` names what an item may not run concurrently with. The claim
skips any item whose partition already has one in flight, so the guarantee holds across
processes without a caller having to take a lock.

Ingest sets it from the workspace, source, and upstream external id:

```
<workspace>|<source>|<external-id>
```

Two versions of one upstream record therefore process in order, while different records stay
fully parallel. An ingest with no external id gets an empty key and no constraint — a one-off
upload supersedes nothing, and serializing every such upload from one source would turn a
bulk import into a queue of one.

Finishing an item makes the worker poll again immediately, since anything queued behind it
in the same partition was passed over precisely because it was in flight.

## Backpressure

Three limits, each answering a different failure:

- **Concurrency** bounds handlers in flight. A worker whose handlers are all busy claims
  nothing, leaving items visible to the rest of the fleet rather than queueing them in its
  own memory.
- **`CG_WORKER_MAX_EVENTS_PER_SECOND`** bounds intake. Per worker, not fleet-wide: a global
  limiter needs coordination that becomes the bottleneck it was meant to prevent. For a
  fleet-wide target, divide by the worker count.
- **`CG_NATS_MAX_ACK_PENDING`** bounds undelivered notifications per consumer.

## Checkpoints

`projection_checkpoints` is a high-water mark. An older cursor cannot move a projection
backwards, `records_projected` accumulates across the fleet, and `advanced_by` records the
process that last advanced it — so a projection that has stopped progressing is attributable
rather than merely stale.

```
cgctl projections status --graph-space <id>
```

A rebuild resets `records_projected`, because after a rebuild the count describes the
rebuild.

## What a broker outage looks like

Nothing is lost. Publishing is best effort and happens after the outbox row is committed, so
a failed publish costs latency; workers keep claiming from the ledger at their poll interval.
The connection reconnects indefinitely rather than giving up, since a worker that needed a
manual restart after a bus outage would be worse than one that simply got slower.

## Testing against real infrastructure

The distributed tests run against a real JetStream server, never a fake — the behavior under
test is delivery semantics, and a fake would reproduce whatever its author already believed.
`CG_REQUIRE_NATS=1` turns a missing server into a failure rather than a skip, which is how CI
avoids losing the coverage silently.

```
scripts/dev-nats.sh start
CG_REQUIRE_PG=1 CG_REQUIRE_NATS=1 go test ./...
```

Each test allocates its own stream: `go test ./...` runs packages in parallel against one
server, and a work-queue stream is exclusive.
