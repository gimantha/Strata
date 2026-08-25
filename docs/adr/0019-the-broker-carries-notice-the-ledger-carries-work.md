# ADR 0019 — The broker carries notice; the ledger carries work

Status: accepted
Date: 2026-08-25
Phase: 14 (distributed production mode)

## Context

Phases 1–13 ran on a single durable queue: work is written to `outbox_events` inside the
same transaction as the mutation that requires it, and workers claim it with
`FOR UPDATE SKIP LOCKED`. That is correct and it scales further than most deployments
need. It has two limits.

The first is latency. A worker learns about work by asking, so the floor is the poll
interval. Shortening it to hide that puts load on the database proportional to the number
of workers rather than to the amount of work — fifty idle workers polling twice a second
is a load pattern that punishes scaling out.

The second is ordering. Two versions of one upstream record must not be processed at the
same time; the newer one can lose the race and leave the graph describing a state the
source has already left. The CDC connector solved this in phase 10 with a per-record
advisory lock, which works but is local to that one path.

Phase 14 asks for a NATS JetStream adapter. The obvious reading is to make the broker the
queue: publish work to JetStream, consume it there, let its durable consumers and
acknowledgements do the scheduling. JetStream is good at this, and it is the wrong choice
here.

## Decision

**The broker delivers notice that work exists. The ledger remains the only thing that
hands work out.**

`natsbus` publishes a copy of each committed outbox event, and a worker that receives one
calls `Outbox.Notify()` — which cuts short the current poll. The claim, the lease, the
retry, the dead-letter, and the partition rule all still happen in PostgreSQL, exactly as
they do with no broker configured.

Three mechanisms make the ledger side scale:

**A partition key on the claim.** `outbox_events.partition_key` names what an item may not
run concurrently with. The claim itself refuses to hand out a second item from a partition
that already has one in flight, so the guarantee holds across processes and does not depend
on a lock a caller has to remember to take. Ingest sets it from the workspace, source, and
upstream external id, so successive versions of one record serialize while different
records stay fully parallel. An empty key means no constraint, and unpartitioned work is
untouched.

**A per-worker rate limit and an idle backoff.** `CG_WORKER_MAX_EVENTS_PER_SECOND` caps how
fast one worker takes on new work, so a fleet cannot outrun a shared model provider or
database. The limit is per worker rather than fleet-wide because a global limiter needs
coordination that becomes the bottleneck it was meant to prevent, and multiplying by the
worker count is arithmetic an operator can do. `CG_WORKER_IDLE_BACKOFF_MAX` doubles the
poll interval while the queue is empty, so an idle fleet is quiet.

**Monotonic checkpoints.** `SaveCheckpoint` is now a high-water mark: an older cursor
cannot move a projection backwards, `records_projected` accumulates across the fleet
instead of recording whichever worker wrote last, and `advanced_by` names the process that
last moved it. A rebuild resets the counter, because after a rebuild the count describes
the rebuild.

## Consequences

A broker outage costs latency and nothing else. Workers fall back to polling, which is the
deployment that never had a broker; no work is lost, delayed past the poll interval, or
duplicated. This is the property that would have been given up by making JetStream the
queue: two systems that both believe they schedule work can disagree about whether a
particular item exists, and reconciling them is a problem with no clean answer.

Publishing is best effort. The outbox row is committed before anything is published, so a
failed publish is logged and the work proceeds at poll speed.

Delivery is at-least-once from the broker and the claim collapses it: a duplicate
notification causes an extra poll that finds nothing. This is why the notification carries
no authority — acting on the message body rather than re-reading the ledger would make
duplicate delivery into duplicate work.

Finishing an item triggers an immediate re-poll on that worker. Anything queued behind it
in the same partition was passed over precisely because it was in flight, and without this
a partitioned stream would move at one item per poll interval however idle the fleet was.
Across the fleet, a successor waits at most one poll interval.

Two deployments sharing a NATS cluster need distinct `CG_NATS_STREAM` values. A work-queue
stream may not overlap another stream's subjects, so the subject space is namespaced by the
stream name.

Contention is real and retry is the answer to it. Workers processing documents about the
same entities do conflict on those entities, and the retry path handles it. A fleet is
faster than one worker, not proportionally faster.

## Alternatives considered

**JetStream as the queue.** Rejected: it makes durability a property of two systems, and
the ledger has to stay authoritative because provenance, replay, and temporal queries are
all defined against it.

**Advisory locks everywhere instead of partition keys.** The phase-10 approach generalized.
Rejected because a lock is taken by the handler, after the item has been claimed: the claim
still hands the work out, and the second worker blocks holding a lease rather than leaving
the item for someone with something else to do.

**Fleet-wide rate limiting.** Rejected: coordinating a shared token bucket needs either a
central service or a consensus round per item.

**Keeping `records_projected` as a replacement rather than a sum.** Rejected: with several
workers advancing one projection, a replaced counter reports the last batch rather than the
total, which reads as a stalled projection.
