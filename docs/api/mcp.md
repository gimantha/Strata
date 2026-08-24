# MCP and portable packages

Two ways knowledge leaves and enters the system: an agent calling tools, and a file moving
between deployments.

## MCP

`cgmcp` speaks the Model Context Protocol over stdio, so a client launches it as a subprocess:

```json
{
  "mcpServers": {
    "strata": {
      "command": "cgmcp",
      "args": ["--graph-space", "01a0...", "--key-id", "agent-key"],
      "env": {"CG_DATABASE_URL": "postgres://..."}
    }
  }
}
```

**It is not a privileged bypass.** The server authenticates once as the principal behind
`--key-id`, and every call goes through the same workspace resolution and policy evaluation as
an HTTP request from that principal. A client cannot choose who it is.

Logs go to stderr, always: stdout belongs to the protocol, and one stray line desynchronizes
the session.

### The tools

| Tool | For |
|---|---|
| `context_graph_search` | Find things. Returns canonical ids and a trace id |
| `context_graph_get_context` | A prompt-ready block within a token budget, with citations |
| `context_graph_ingest` | Record source material |
| `context_graph_get_entity` | One identity and the claims about it |
| `context_graph_get_assertion` | One claim with its temporal coordinates |
| `context_graph_explain` | Walk a claim to its evidence, or a past retrieval to its results |
| `context_graph_temporal_query` | What was true, or what was believed, at a past instant |

Every result carries canonical ids so an agent follows a reference instead of asking for a
bigger payload. `search` also returns a `trace_id`, which `explain` accepts — "why did I get
those results" becomes a cheap later question.

`ingest` says plainly that extraction is asynchronous. An agent that assumes its content is
searchable on the next call will otherwise draw the wrong conclusion from an empty result.

A tool that fails returns a normal response with `isError` rather than a JSON-RPC error: the
agent asked a reasonable question badly and can retry, whereas a transport error tends to make
clients drop the conversation.

## Portable packages

A package moves knowledge between deployments (AGENTS.md section 29). It is newline-delimited
JSON — a header, records in dependency order, a manifest — so it streams, greps, and diffs.

```bash
cgctl package export --graph-space $GS --out acme.jsonl
cgctl package verify --file acme.jsonl          # no database needed
cgctl package import --graph-space $OTHER --file acme.jsonl --accept-predicates
```

```
GET  /v1/graph-spaces/{graph_space_id}/package
POST /v1/graph-spaces/{graph_space_id}/package?dry_run=true
```

### What travels

Entities with their aliases, claims with all four clocks, evidence quotes, predicate
definitions, the ontology, and — with `--include-chunks` — the source passages. Chunks are off
by default: a package is knowledge, and copying source material has different policy
consequences from copying conclusions.

Precomputed embeddings are tagged with the model and version that produced them. An untagged
vector is worse than none: it silently pollutes a projection with numbers from another
geometry.

### Integrity

The manifest is the last line, carrying per-section counts and chained digests:

```json
{"format":"strata.context-package.manifest",
 "counts":{"entity":2,"assertion":1},
 "digests":{"entity":"sha256:...","assertion":"sha256:..."},
 "digest":"sha256:a156892a..."}
```

Digests are chained, so **order is part of the check** — a shuffled package has the same lines
and a different digest. A mismatch names the section:

```
package integrity check failed in the entity section:
  expected sha256:f08f5685..., computed sha256:16c74dad...
```

**Nothing is committed until the digest verifies.** A truncated transfer is refused with a
message saying so, rather than importing most of a workspace and leaving the rest silently
absent.

### What an importer does not trust

- **Identifiers.** Entities and claims are re-resolved by name through the target's own
  resolver. Two deployments cannot collide, and importing into a populated workspace merges
  rather than duplicates.
- **Knowledge time.** The exporter's clocks become source time. Copying them would claim this
  system believed something before the package existed.
- **Predicate semantics.** Definitions arrive as candidates and only with
  `--accept-predicates`. A predicate marked functional elsewhere would otherwise start
  retiring claims here.
- **Provenance.** Imported claims cite the archived package — the source material this
  deployment actually holds — carrying the original quote and source name inside the evidence.

Re-importing the same package is safe: claims collide on their fingerprints and the summary
reports them as already present.

### Completeness

Export is policy-aware: what a principal may not read, they may not package. The header records
two distinct facts:

| Field | Means |
|---|---|
| `max_classification` / `filtered` | A ceiling was in force, so material above it is absent whether or not any existed |
| `excluded` | How many claims the export pass actually dropped |

They answer different questions — "may this be treated as a backup" and "why is the claim I
expected missing" — and a package that looks complete when it is not is worse than no package.

The reasoning, and why tar and signatures were not used, is in
[ADR 0018](../adr/0018-packages-are-verified-streams-not-trusted-dumps.md).
