# ADR 0025 — Two model bugs that degraded instead of failing

Status: accepted
Date: 2026-08-28
Phase: 16 (query planning), retrospective on phase 8 (extraction)

## Context

Asked to test the LLM planner against a real model, we had no credentials and no local
server. Testing the client-side contract instead — the schema we send and the request we
build — found two defects, one of them nine phases old. Neither could have been found by
the tests that existed, and neither would have produced a visible failure in production.

**The schema could not be accepted.** `planSchema` used `minItems`, `maxItems` and
`maxLength`, made `mode_reasons` optional, and gave it an open `additionalProperties` map.
Strict structured output rejects all four. `extraction.ResultSchema` had documented the rule
since phase 8 — *"every property is required and additionalProperties is false throughout,
which is what strict structured-output modes demand"* — and the planner simply did not
follow it.

**Temperature 0 was never sent.** `llm.GenerateRequest.Temperature` was a `float64`, so
"unset" and "explicitly zero" shared a representation, and the adapter forwarded it only
when `> 0`. Both callers ask for zero on purpose. Extraction's own comment says *"Extraction
wants reproducibility, not creativity"*, and every extraction run against a real provider
had been at that provider's default instead — in the write path for derived knowledge, where
non-determinism means the same document need not yield the same claims twice.

What makes both worth an ADR is not the mistakes. It is that the system was built to survive
them. A rejected schema is an unavailable model; an unavailable model degrades to the
heuristic planner, which works. The logs carry a warning and everything else looks healthy.
The fallback that makes the feature safe is the same fallback that hides its absence.

## Decision

**`Temperature` is `*float64`.** Nil is omitted from the request, honouring the reason the
old behaviour existed — some models reject any temperature but their default. Zero is sent,
because zero is a request.

**`llm.RunStrictSchemaConformance` checks every schema we send.** It rejects unsupported
keywords, an object without `additionalProperties: false`, and any property absent from
`required`. Both schemas run it, incumbent included, on the principle of ADR 0020.

**The planner is tested through the real adapter over HTTP.** A local server that validates
strict mode the way a provider does, driving `openai.Provider` rather than a scripted
`llm.LLM`: the schema, the encoding, the retries, and every degradation path.

## Addendum: what a real model then showed

The above was written without a model to test against. An open-weights one — qwen2.5:3b
under Ollama, behind the same OpenAI-compatible adapter — was pulled afterwards, and found a
third defect within four questions.

Handed a prompt-injection attempt, it returned four sub-queries and labelled every one
`original`. Since decode replaces the text of anything so labelled with the question as
asked, the plan became the same search four times. Sub-queries are fused rather than merged,
so each duplicate was another RRF contribution: three originals beside one decomposition
would have weighted the original phrasing three times as heavily, on nothing but a model's
formatting habit. Sub-queries are now deduplicated by text, and a rewrite that lands back on
the question as asked is relabelled `original` rather than counted as a second search.

No scripted test had produced that shape, because no author thinks to write it. Two things
the same run confirmed: Ollama honours temperature 0 and a fixed seed, so the same question
plans the same way; and the schema, having been shaped for strict mode, was accepted.

The live tests are opt-in — `CG_REQUIRE_OLLAMA=1` — rather than running whenever a server is
up, which inverts the convention the storage harnesses use. What a model decides is not a
property of this repository, and a gate that quietly came to depend on one would fail for
reasons no change to the code caused. They therefore show as skipped in a bare `go test
./...`, which is the honest report: they did not run.

## Consequences

A schema that no provider would accept now fails a test rather than a request. An explicit
temperature reaches the wire. Extraction is deterministic for the first time.

The limits are worth stating plainly. The conformance suite encodes strict-mode rules as we
understand them; a provider that changes its rules will not tell us, and the suite is only as
current as its list. The wire tests prove the planner can talk to a provider and survive
everything a provider can do to it. The live tests prove a real model's output survives
contact with decode. Neither proves a given model plans *well* on a particular corpus — that
needs judgment and a corpus, and it is measurement rather than construction. What the live
run did show is that qwen2.5:3b reaches for `exact` and `graph` on almost everything and
never chose `entity`, including for *"who confirmed the renewal"*, where entity retrieval is
the obvious first move.

The general lesson is the one that keeps recurring in this project, seen from a new angle.
ADR 0024 recorded that a filter existing in the domain and nowhere in the query path fails
silently toward disclosure. This is the same shape: **a graceful fallback converts a
permanent failure into an invisible one.** Wherever we degrade rather than fail, something
must independently assert that the good path is reachable at all — because the degraded path
will not complain, and it is the only thing anyone will see running.
