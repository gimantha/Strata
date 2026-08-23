# ADR 0005: Typed object columns for assertions

Status: accepted
Date: 2026-08-23

## Context

An assertion's object can be an entity reference, a string, an integer, an exact decimal,
a boolean, a timestamp, a date, a duration, a geo coordinate, a JSON document, a URI, or a
symbol. AGENTS.md section 6.9 is explicit that these must not all be stringified.

The storage choice matters beyond tidiness. Later phases need to filter numerically, match
values exactly for deduplication and conflict detection, and eventually reason about
quantities and units. Any of those becomes guesswork if a revenue figure is stored as the
text `"12345678.90"`.

## Decision

Assertions store their object in one typed column per kind - `object_entity_id`,
`object_text`, `object_integer`, `object_decimal numeric`, `object_boolean`,
`object_timestamp`, `object_date`, `object_duration_ns`, `object_geo_lat`/`lon`,
`object_json jsonb` - with `object_kind` selecting which one is populated.

Alongside them sits `object_key`, a canonical comparable rendering of the value produced by
`AssertionObject.Key()`. Equality of objects is defined as equality of this key.

Decimals are carried through the Go layer as strings and stored as `numeric`, never as
`float64`.

## Alternatives

- **A single `object_value jsonb` column.** Simpler schema, and rejected. Numeric
  comparison would depend on JSON casting, exact decimals would round-trip through
  IEEE-754 unless hand-encoded, and indexes would be expression-based and easy to get
  subtly wrong.
- **A single `object_text` column with a kind discriminator.** Rejected for the same
  reasons, plus it makes every comparison a parse.
- **A separate table per object kind.** Correct but heavy: every read of an assertion
  would need a union or eleven left joins, for a gain that indexes on nullable columns
  already provide.
- **Storing only `object_key`.** Rejected: the key is lossy by design (it upper-cases
  symbols and truncates coordinates), so it can compare values but not return them.

## Trade-offs

The assertions table is wide, and eleven columns are NULL on any given row. In PostgreSQL
a NULL column costs a bit in the row's null bitmap and nothing in storage, so the price is
schema verbosity rather than space.

The real cost is duplication: adding an object kind means touching the schema, the scan
function, and the key function together. That is accepted because the alternative moves
the cost to every future query instead.

`object_key` must stay consistent with `AssertionObject.Key()`. It is written only by the
insert path, which computes it from the domain object, so the two cannot drift without a
code change that a test would catch.

## Migration impact

Adding a kind is an additive migration plus a new branch in three places. Existing rows are
unaffected, since each kind uses its own column.
