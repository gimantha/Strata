# ADR 0018: A package is a verified stream, and an importer trusts nothing in it

Status: accepted
Date: 2026-08-24

## Context

Phase 13 asks for portable context packages with integrity manifests, and AGENTS.md section 29
states the constraint that shapes the whole design: "Import must not blindly trust IDs or
permissions. Validate schema, signatures/hashes if supported, scope mapping, and policy before
commit."

The tempting implementation is a JSON document with arrays of records, imported by inserting
them with the ids they carry. It is easy, it round-trips perfectly between two instances of the
same code, and every one of its convenient properties is a defect: it does not stream, ids
from one deployment collide in another, and a truncated download imports most of a workspace
without saying so.

## Decision

**A package is newline-delimited JSON: a header, records in dependency order, a manifest.**
One file, greppable and diffable, written without buffering a workspace in memory. Records go
out ontology → predicates → entities → assertions → evidence, so an importer reading forwards
never holds a reference it cannot resolve.

**The manifest is a trailer, not a header.** That is what makes streaming possible: digests
accumulate while records are written and the totals exist only at the end. A reader gets them
at the end too, which costs nothing, because nothing may be committed before the whole package
has been verified anyway.

**Digests are chained, so order is part of the integrity check.** Each step hashes the previous
digest with the next line. A package whose records were shuffled contains the same set of lines
and produces a different digest — which matters because records reference each other. Per
section digests exist alongside the overall one so a mismatch says *which* part is wrong: "the
assertions section is corrupt" is a far better message than "the package is corrupt".

**Nothing is committed until the digest verifies.** A truncated transfer is refused with a
message that names the cause, rather than importing most of a workspace and leaving the rest
silently absent. Absence is the failure mode nobody notices.

**Identifiers are provenance, never identity.** Entities and claims are re-resolved by name
through the target's own resolver, exactly as if they had arrived from any other source. Two
deployments cannot collide, an import into a populated workspace merges rather than duplicates,
and a re-import is recognized by fingerprint rather than by bookkeeping.

**Knowledge time is the importer's own.** The package carries the exporter's clocks as source
time. Copying them into knowledge time would claim this system believed something before the
package existed, which is the specific lie the ledger is built to prevent.

**Provenance points at the package.** An import archives the package as a source event, and
every imported claim cites the episode that produced — carrying the original quote and source
name inside it. The exporter's chunk ids mean nothing here; what this deployment actually saw
is the package, and saying so is more honest than a dangling reference.

**Predicates arrive as candidates, and only when asked for.** A predicate marked functional
elsewhere would, adopted unreviewed, start retiring claims here. `accept_predicates` is opt-in
and the definitions land as candidates rather than approved semantics.

**Completeness is two facts, not one.** `filtered` says a classification ceiling was in force,
so material above it is absent whether or not any existed; `excluded` counts what the export
pass actually dropped. They answer different questions — "may this be treated as a backup" and
"why is the claim I expected missing" — and collapsing them produces either a warning that
fires on every export and is ignored within a week, or a silence that hides a partial package.

**MCP is a transport, not a trust boundary.** The server authenticates once as a specific
principal, and every tool call goes through the same policy evaluation as an HTTP request from
that principal. An MCP client cannot choose who it is.

**The MCP protocol is implemented directly.** It is JSON-RPC 2.0 over newline-delimited stdio —
about a hundred lines. Taking a dependency for it would put a third party between this system
and its own contract, which section 3 rules out for exactly the same reason it rules out
provider SDKs in the domain.

## Consequences

`AssertionObject` gained an `UnmarshalJSON`. It had a custom marshaller and no counterpart, so
a symbol arrived from a package with its kind intact and its text gone. A type in that state is
silently lossy everywhere a value is written and read back — a package, a queued job, a cached
response — and the round-trip test now covers every object kind.

Tool results are JSON text blocks carrying canonical ids. An agent follows a reference rather
than asking for a larger payload, which is what section 26 means by keeping payloads small.
Search additionally returns a trace id, so "why did I get these results" is a later question
with a cheap answer.

A failed tool returns a successful JSON-RPC response with `isError`, not a transport error. The
agent asked a reasonable question badly and can retry; a transport error tends to make clients
drop the conversation instead.

Notifications get no reply. MCP sends `notifications/initialized` on every connection, so
answering it breaks the handshake with every strict client rather than none — which is why
there is a test for silence.

## Alternatives considered

**A tar archive with per-file sections.** Natural per-section hashing and a familiar shape.
Rejected because `tar` needs each entry's size before writing it, which means buffering every
section — surrendering the streaming property that motivated the format.

**A single JSON document.** Simplest to write and read, and it requires the whole package in
memory at both ends. Fine for a thousand claims and useless for a million.

**Signatures rather than hashes.** Stronger, and it needs key distribution, rotation, and trust
policy that this phase has no answer for. The digest detects corruption and truncation, which
are the failures that actually happen in transit; signing is a later decision that this format
leaves room for.

**Preserving ids on import.** Makes a round trip byte-identical and breaks the moment two
deployments exchange packages in both directions. The identifiers are ours to mint.
