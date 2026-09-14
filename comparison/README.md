# Strata vs Cognee core: event-history comparison

A real-engine benchmark of 1,000 synthetic procurement events across 100 independent
projects. It measures retrieval-grounded answers about time, decision rationale, updates,
invalidation, and provenance. There are **140 questions per system**: 10 after event 300,
10 after event 600, and 120 after event 1,000. Neither engine receives the answer key.

**Execution status:** inspect `results/*/manifest.json` and its adjacent `REPORT.md`.
A generated fixture, a passing harness test, or a blocked preflight is not a completed
comparison. Unmeasured scores are N/A, never inferred from product descriptions.

Completed runs can be checked against the predeclared holdout cohort without an LLM judge:

```bash
comparison/.venv/bin/python comparison/quality_gate.py \
  comparison/results/<run-id>/answers.jsonl --system strata
```

The default gate requires at least 97% grounded answer accuracy, 100% citation validity,
and zero query errors on holdout questions. Use `--minimum-grounded` to set a different
release threshold. Cohort membership is fixed by `dataset.py` before either engine runs.

## Run

### Local Ollama profile (no OpenAI key)

With Ollama running and the models present:

```bash
comparison/.venv/bin/python comparison/run.py --provider ollama --smoke --batch-size 1
comparison/.venv/bin/python comparison/run.py --provider ollama --batch-size 1
```

The local profile uses **llama3.1:8b** for both extraction and the common answer step,
and **nomic-embed-text:latest** for both embedders. Override with `--ollama-model`,
`--ollama-embedding`, and `--ollama-url`. Models must already exist on that server.
There is no hosted-model fallback and no real API key is needed.

A full local run can take a day on this machine. To run independently of the terminal:

```bash
comparison/.venv/bin/python comparison/launch.py --run-id ollama-1000
comparison/.venv/bin/python comparison/status.py ollama-1000

# Batch-accept each checkpoint cohort (300, 300, then 400 events).
comparison/.venv/bin/python comparison/launch.py --bulk-ingest --run-id ollama-batch-1000
```

The default launcher uses one-event batches to bound the shared inference queue. With
`--bulk-ingest`, it submits each checkpoint cohort in one native batch and still processes
local Cognee data items and chunks one at a time so hundreds of concurrent calls do not
queue behind the single Ollama worker and exceed client timeouts.
Cognee's first-use connection checks complete before the first Strata batch is submitted.
The three barriers are necessary to evaluate what each system knew after events 300 and
600; submitting later events before those questions would invalidate the temporal test.
The launcher saves its PID
and log under `.runtime/<run>/`, and uses macOS `caffeinate -i` for the child process's
lifetime. Reports update after each processed batch and evaluation checkpoint. Keep
Ollama and Docker running. Runs preserve partial evidence but do not resume after a crash.

`ollama_gateway.py` listens only on localhost port 18090 while the run is active. It
bridges the existing OpenAI-compatible clients to Ollama's native chat/embed APIs,
passing prompts and JSON schemas through. It sets an 8,192-token chat context and
serializes inference on this shared 16 GB machine. Nomic's actual 768-dimensional
embeddings are **zero-padded to 1,536 dimensions for both systems**, because Strata's
schema requires that width. Adding zero coordinates preserves dot products, norms,
and cosine similarity; this is not a new embedding model or simulated embedding.
Models wider than 1,536 dimensions are rejected rather than truncated silently.
Inputs exceeding the embedding model's context are rejected rather than truncated.

The manifest records Ollama's version, installed model digests, context window,
embedding transformation, and inference concurrency. `ollama-requests.jsonl` records
model-call durations/statuses and raw generated completions (synthetic benchmark data;
request prompts are omitted) as calls finish; a final JSON copy is also saved. Each run
archives and hashes the harness source and dependency lock. This is an **ollama-matched** profile,
not either product's default hosted-model profile. Small-model extraction failures
and local inference time affect the result; do not compare these scores to hosted
runs as though only the database changed.

For local models, the adapter registers the bridge's real JSON-schema capability with
LiteLLM. Otherwise an unrecognized Ollama model is routed to prompted JSON even though
the endpoint can enforce the schema. Cognee uses its dedicated `openai_compatible`
embedding adapter to avoid LiteLLM's hosted-model dimension restrictions. On the first
event, Cognee completes its short first-use connection checks before Strata occupies
the shared inference queue; subsequent events are accepted by both concurrently.

See [Ollama embeddings](https://docs.ollama.com/capabilities/embeddings) and
[Cognee local setup](https://docs.cognee.ai/guides/local-setup). This harness uses its
own protocol bridge to satisfy Strata's vector width while keeping both embeddings
identical; it does not claim Cognee needs this bridge for ordinary Ollama usage.

### Anthropic generation with local embeddings (no Ollama)

Set `ANTHROPIC_API_KEY` in `comparison/.env` and run a billed smoke test before the
full comparison:

```bash
comparison/.venv/bin/python comparison/run.py --provider anthropic --smoke --bulk-ingest --batch-size 1000
comparison/.venv/bin/python comparison/launch.py --provider anthropic --bulk-ingest --run-id anthropic-batch-1000
comparison/.venv/bin/python comparison/status.py anthropic-batch-1000
```

This profile uses `claude-haiku-4-5-20251001` for Strata extraction, Cognee extraction,
and common answer generation. Both engines use FastEmbed 0.8 with
`BAAI/bge-small-en-v1.5`. Its native 384-dimensional vectors are padded identically to
1,536 dimensions. `anthropic_gateway.py` translates the OpenAI-compatible generation
requests used by the two engines to the native Anthropic Messages API, requests native
JSON-schema structured output, and exposes the shared in-process FastEmbed model through
the embeddings endpoint. Ollama is not contacted by this profile. The real Anthropic key
stays inside the localhost bridge; the child engine clients receive a non-secret
placeholder. The gateway performs a one-token generation preflight because model-list
access does not prove that the account has usable API credit. Pass
`--embedding-backend ollama --embedding-model nomic-embed-text:latest` only to reproduce
the older mixed Anthropic/Ollama profile.
Anthropic rejects numeric `minimum` and `maximum` schema keywords and does not accept an
`enum` paired with a nullable type array. The bridge removes those numeric wire bounds and
expresses nullable types as `anyOf`. Each engine still applies its own local output
validation before committing extracted knowledge, and the transformation is recorded in
the run manifest.
Provider requests have a 120-second read timeout and up to three transport attempts, so a
single stalled HTTPS response cannot hold a checkpoint indefinitely.

Anthropic generation calls and local embedding calls are written to
`model-requests.jsonl`, including latency and token counts. Prompts and API keys are not
logged. The smoke run and the full run use fresh engine state and separate result folders.

### Temporal and provenance capability trials

Run the two Cognee capability branches separately against the same ten-event smoke corpus:

```bash
# Event-time graph construction plus Cognee TEMPORAL retrieval.
comparison/.venv/bin/python comparison/run.py --provider anthropic --smoke \
  --bulk-ingest --batch-size 1000 --cognee-mode temporal

# Default graph construction plus Cognee's audit provenance ledger.
comparison/.venv/bin/python comparison/run.py --provider anthropic --smoke \
  --bulk-ingest --batch-size 1000 --cognee-mode provenance
```

If another benchmark currently owns the local ports, queue both trials behind it:

```bash
comparison/.venv/bin/python comparison/capability_suite.py \
  --suite-id anthropic-capabilities \
  --wait-for-run anthropic-fastembed-batch-1000-20260914-01
```

Cognee 1.5.4 implements these as distinct task lists: `temporal_cognify=True` replaces
the default cognify pipeline, while `PROVENANCE_TRACKING` appends a task to the default
pipeline. The harness therefore does not label a single Cognee graph as having both.
The temporal run uses `SearchType.TEMPORAL`; the provenance run samples the real
`provenance_entries` ledger through Cognee's provenance manager.

Both capability runs also execute a deterministic Strata native probe using source events
already accepted by the engine. It commits an original belief, a decision derived from that
belief, and a later correction that explicitly supersedes the original. The resulting
`strata-native-capabilities.json` proves, with raw API responses, that:

- `valid_at` selects the world-time interval;
- `known_at` reconstructs the belief held before the correction;
- current belief returns the correction without erasing the original;
- both assertions walk back through evidence to source;
- the decision derivation still references the superseded belief that informed it.

`strata-retrieval-traces-*.json` records the retrieval plan and selected canonical record
references produced by native explained queries. These are operational retrieval traces;
the decision derivation in `strata-native-capabilities.json` is the business-decision trace.

### Hosted profile

Requirements: Go 1.26, Python 3.12, uv, Docker, and an OpenAI API key with model access.
All events are synthetic. Running the benchmark makes billable extraction, embedding,
and answer-generation requests. No source repository documents are uploaded.

```bash
bash comparison/setup.sh
cp comparison/.env.example comparison/.env
# Edit comparison/.env and set OPENAI_API_KEY locally.

# Validate one project's complete ten-event lifecycle first.
comparison/.venv/bin/python comparison/run.py --smoke

# Full comparison: fresh workspace/dataset, 1000 events, 280 answer evaluations.
comparison/.venv/bin/python comparison/run.py

# Optional separate control using the same extraction/embedding models in both.
comparison/.venv/bin/python comparison/run.py --matched-models
```

Infrastructure-only checks (no API key, no model calls, no benchmark ingestion):

```bash
comparison/.venv/bin/python comparison/check_infrastructure.py
```

The benchmark starts its own Strata process on `127.0.0.1:18089` and stops that process
on exit. PostgreSQL is isolated in `strata-comparison-postgres` on port 55439, with a
fresh database per run (recorded in the manifest). This also isolates asynchronous
workers from pending jobs left by earlier interrupted trials. Cognee
files, blobs, server logs, and API-key hashes live under gitignored `.runtime/`.
The PostgreSQL container remains available to inspect results. Stop it with
`docker stop strata-comparison-postgres`. No existing Strata workspace is cleared.

Each invocation creates a new run ID and refuses to overwrite an existing result folder.
Interrupted runs retain receipts, partial answers, and errors. Automatic resume is not
implemented: use a new run, rather than mixing pre-update and post-update evidence.
`--systems strata` or `--systems cognee` runs one engine for diagnostics; it is not a
two-system comparison. `--preflight` checks credential presence only, not validity.

## Exact profile and defaults

Pinned release: **Cognee 1.5.4**, inspected from its installed package. Dependency
versions are frozen in `requirements.lock` (macOS ARM64 / Python 3.12 environment).

| Setting | Strata | Cognee core 1.5.4 |
|---|---|---|
| Extraction | gpt-4.1-mini, documented example | gpt-5-mini, release default |
| Embedding | text-embedding-3-small, 1536 dimensions | text-embedding-3-large, 3072 dimensions |
| Canonical storage | PostgreSQL | SQLite metadata + graph/vector stores |
| Graph / vector / lexical | PostgreSQL / pgvector / full-text | Ladybug / LanceDB / release default |
| Ingestion | source events + running outbox worker | add + incremental cognify |
| Retrieval | native context assembly, heuristic hybrid planner | HYBRID_COMPLETION, only_context |
| Final answer model | gpt-4.1-mini | gpt-4.1-mini |
| Native extra modes | No custom ontology or predicate registry | Temporal cognify, contradiction detection, audit provenance remain off |

Strata's literal configuration default disables extraction and selects no hosted model.
The benchmark enables its documented example models, so this is called the
**documented-defaults profile**, not an assertion that Strata enables these by default.
Default models differ, so results cannot isolate architectural effects. The optional
`--matched-models` profile uses gpt-4.1-mini + text-embedding-3-small for both systems and
must be reported as a separate run.

Both repositories support lexical search, but it is exposed differently. Strata has a
first-class lexical projection backed by PostgreSQL `tsvector`/GIN plus trigram matching;
its default planner runs lexical alongside vector and graph retrieval. Cognee 1.5.4 has
an explicit `CHUNKS_LEXICAL` BM25 retriever. Its default `HYBRID_COMPLETION` path used by
this comparison combines vector-ranked chunks, entities, and graph facts; the installed
implementation does not invoke the BM25 retriever in that path. Accordingly, these
scores evaluate each product's default context path, not a lexical-only head-to-head.

Cognee's online installation page still describes gpt-4o-mini / text-embedding-3-small
and Kuzu. Installed release code takes precedence here. The core `search` function's
signature defaults to HYBRID_COMPLETION despite an older docstring saying GRAPH_COMPLETION.
Newer `remember`/`recall` APIs and their self-improvement/session layers are outside this
requested **core** comparison; caching is not disabled. Temporal and audit-provenance
features disabled by default are not described as universally unsupported.

## Event and clock semantics

Every project has the following ten events, interleaved with other projects:

1. Initial owner/budget; a supplier proposal without approval.
2. A supplier fails a security audit.
3. An alternate supplier is approved because of that audit report.
4. A routine delivery that changes none of those facts.
5. A budget increase for scope expansion.
6. Ownership transfers; the previous owner's validity interval is explicit.
7. A late correction establishes the audit was a false positive. The original decision's
   stated rationale remains part of history.
8. A replacement supplier is approved because of an outage.
9. An old ownership record arrives late, carrying an older source version.
10. A temporary incident contact expires; this role differs from accountable owner.

Both systems receive identical source text including event ID, project, source, world
date, and simulated receipt date. Strata also receives those source dates/version fields
through its public ingestion envelope. All events use natural-language extraction; the
harness does not write precomputed assertions, schemas, or answers into either graph.

Dates are simulated January 2026 dates. Wall-clock processing/knowledge timestamps remain
the actual run time. Do not confuse `received_at` in the synthetic text with Strata's
real `recorded_at`. Primary temporal questions express time in natural language for both
systems. Separate Strata native probes map actual checkpoint wall times to `known_at`
and pass world dates to `valid_at`; their raw results are not mixed into primary scoring.

Events are sent one by one to both running systems in the same arrival order, with a
default 10 ms gap **after both accept**. Cognee cognifies incrementally every 25 events;
the durable local launcher uses one-event batches to bound Ollama queue time. Strata's
worker processes events asynchronously. Both drain before the next microbatch,
and question checkpoints are strict barriers: later events do not exist yet when live
questions are asked. This is an accelerated, backpressured stream simulation, not a
fixed-rate throughput benchmark. Batch processing latency includes different orchestration
overheads and is retained as diagnostic data, not used in the knowledge score.

## Scoring and safeguards

The questions and expected answers are fixed before model calls. A shared answer model
receives only the question, output field names, and retrieved context. It receives no
answer choices, expected values, source corpus, or earlier answers. Context is capped at
6,000 `cl100k_base` tokens for each engine; original retrieval output and truncation are saved.
Strata also applies its native context selection and token estimator within that ceiling.

For each question:

- **Fact accuracy:** fraction of requested values matching the independent answer key.
  Conflicting values for one field fail; returning both old and new values cannot pass.
- **Evidence recall:** fraction of predeclared target event IDs with a verified excerpt.
- **Citation validity:** fraction of cited excerpts present in both the cited source event
  body and the actual context supplied to the answer model. Header-only quotes do not count.
- **Grounded accuracy:** fact accuracy multiplied by evidence recall.

Every category has equal weight in macro scores. Failed queries remain in the denominator.
Strata events reaching a terminal failed/quarantined state are recorded and the stream
continues; they are excluded from processed-success counts. The final status explicitly
identifies ingestion failures. Infrastructure failures or a failed Cognee cognify batch
stop the run with partial results, since that batch cannot be treated as fully indexed.
Scores for unexecuted systems are N/A. The harness does not grade narrative eloquence or
claim that exact-code recall proves broad reasoning. Its reasoning test includes budget
arithmetic; decision tests recover explicit causal evidence and distinguish the original
belief from corrected facts. Quote matching proves traceability, not semantic entailment.
Alternative sufficient evidence may receive less recall than the declared target set;
raw outputs make that limitation reviewable. A single run has no uncertainty interval.

## Native state and provenance

The runner saves read-only graph snapshots at checkpoints, complete Strata assertion,
entity, evidence, and model-run records scoped to its own workspace, sampled Strata
provenance endpoint walks, native temporal probe responses, and retrieval traces. Cognee
snapshots contain the real graph nodes/edges from its own dataset; provenance capability
runs additionally save sampled audit-ledger entries. These let a reviewer inspect whether a
former fact is superseded, merely retained as text, or represented as an ordinary edge.
**Do not infer native invalidation from a correct final answer.** These diagnostics are
not assigned a fabricated apples-to-apples score simply because the two schemas differ.

## Files and reproduction

- `dataset.py`, `fixtures/`: seeded events, independent questions, content hashes.
- `adapters.py`: real HTTP/SDK operations and read-only snapshots.
- `run.py`: arrivals, barriers, shared answer generation, durable per-question artifacts.
- `scoring.py`: transparent deterministic rubric; `test_comparison.py`: corpus/scoring guards.
- `results/<run>/REPORT.md`: per-category and macro results with execution status.

Each answer record includes native context-retrieval time, shared answer-model time, and
end-to-end time. `latencies.json` and the report contain per-system averages over successful
completed questions. Keeping the components separate prevents Ollama generation latency
from being attributed to the context graph implementation.

References inspected for setup and defaults (2026-09-11):

- [Cognee installation](https://docs.cognee.ai/getting-started/installation)
- [Cognee 1.5.4 release](https://pypi.org/project/cognee/1.5.4/)
- [Cognee source](https://github.com/topoteretes/cognee)
- Strata: `configs/dev.env`, `internal/config/config.go`, `internal/api/http/`, and
  `internal/store/ledger/` at the commit recorded in each run manifest.
- Installed Cognee: `infrastructure/llm/config.py`,
  `infrastructure/databases/vector/embeddings/config.py`, `modules/cognify/config.py`,
  and `api/v1/search/search.py`.
