# Memory lifecycle

Knowledge stops being *useful* long before it stops being *true*. A note that someone is
staying at a hotel tonight is true forever and worth surfacing for a day. The context clock
says the second thing; world validity says the first (AGENTS.md section 21).

## The four clocks, and which one this is

| Clock | Question |
|---|---|
| World | When was this true? |
| Knowledge | When did we believe it? |
| Source | When did the upstream say it? |
| **Context** | **When is it worth surfacing?** |

Only the fourth is editable after the fact, and only through the operations below.

## Expiry and the active window

```json
{
  "predicate": "STAYING_AT",
  "object": {"kind": "string", "text": "Kelvinbridge Hotel"},
  "memory_kind": "working",
  "active_until": "2026-06-02T00:00:00Z",
  "expires_at": "2026-06-02T00:00:00Z"
}
```

After that instant the claim is no longer returned as current context. It is **not** deleted,
retracted, or superseded: it is still asserted, its evidence survives, its episode is intact,
and `active_at` set to an earlier instant still finds it.

Windows are half-open, like every interval here — active until noon is not active at noon.

**Lifecycle belongs to a claim, not to its source.** The passage the claim came from still
says what it says, and will forever. Expiring a claim must not rewrite the record of what was
written.

## Decay

Claims with `decay_starts_at` lose ranking weight over time: halving every 30 days, floored at
0.2. The floor is the point — decay reorders results and never removes them. A weight of zero
would be unfindability, which is deletion under another name (section 21.2).

With `explain`, the multiplier appears in each item's signals:

```json
"signals": {"lexical_rrf": 0.031, "decay": 0.35}
```

## Forgetting is four operations

They are named separately because they differ in what survives, and a single `delete` flag
would make "we stopped using this" indistinguishable from "we were required to erase this" at
exactly the moment that difference matters.

| Kind | What survives | Reversible | Where |
|---|---|---|---|
| `deactivate` | Everything; only the context clock moves | **Yes** | `POST /v1/assertions/{id}/forget` |
| `retract` | Everything; a knowledge-time correction | As-of queries still see it | `POST /v1/assertions/{id}/retract` |
| `retention` | The record that something was removed | No | Not implemented |
| `erasure` | An audit proof and nothing else | No | Not implemented |

```
POST /v1/assertions/{assertion_id}/forget
{"kind": "deactivate", "reason": "the customer asked us to stop using this"}
```

A reason is required. A reversible operation with no recorded motive is indistinguishable
from an accident the first time somebody asks why something is missing.

Passing `retention` or `erasure` returns an error rather than doing something weaker. Those
are deletion workflows needing erasure jobs, projection sweeps, and their own authorization
(section 23); a caller who asks for erasure and gets deactivation would believe data was
destroyed when it was not.

```
POST /v1/assertions/{assertion_id}/reactivate
```

Reversibility is what makes deactivation safe to reach for. An operation people fear is
permanent is one they avoid in favour of something worse.

## Consolidation

Repeated observation becomes a stable fact (section 21.1):

```
POST /v1/graph-spaces/{graph_space_id}/consolidate
{"min_observations": 3, "min_distinct_sources": 1, "dry_run": false}
```

Episodic claims saying the same thing about the same subject are grouped, and a group observed
often enough produces **one derived semantic assertion** with a derivation naming every
observation by id.

- **The observations are untouched.** Consolidation adds a conclusion; it does not consume the
  evidence for it.
- **Confidence is capped at 0.95.** A conclusion drawn from observations is never more certain
  than an observation, however many times it was seen — otherwise one unreliable source
  repeating itself manufactures near-certainty.
- **It is idempotent.** A second pass produces the same claim, which collides on its
  fingerprint. A job that accumulates a duplicate conclusion on every run is worse than one
  that never runs.
- **Three is the default threshold.** Two is a coincidence. Use `dry_run` to evaluate a
  different threshold against real data before adopting it.

```bash
cgctl consolidate --graph-space $GS --min-observations 2 --dry-run
```

```
14 observation(s) examined in 5 group(s)

PREDICATE  SEEN  SOURCES  CONFIDENCE  SUMMARY
LEADS      3     1        0.71        observed 3 times
SHIPS_LATE 4     2        0.78        observed 4 times across 2 sources

2 group(s) would be consolidated. Nothing was written.
```

## Memory kinds

`episodic`, `semantic`, `procedural`, `preference`, `working`, `derived` — one assertion model
with explicit classification rather than separate incompatible stores (section 9).

Kind affects ranking priority: semantic knowledge is the baseline, working memory ranks at
half, on the grounds that scaffolding for a task in progress should not compete with
established facts.

## From the CLI

```bash
cgctl consolidate --graph-space $GS
cgctl forget --assertion $ID --reason "superseded by the new contract"
cgctl reactivate --assertion $ID
```

The reasoning behind decay, expiry, and the four kinds of forgetting is in
[ADR 0017](../adr/0017-decay-ranks-forgetting-is-four-operations.md).
