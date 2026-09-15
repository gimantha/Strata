"""Resume a benchmark interrupted after a completed checkpoint.

Only durable boundaries are resumed: completed answer rows are retained, accepted Strata
events are re-submitted through their idempotency keys, and Cognee cognify is retried over
its existing dataset (the same operation each normal checkpoint already performs).
"""

import argparse
import asyncio
import datetime
import hashlib
import json
import os
import subprocess
import time

import httpx
import tiktoken

from adapters import Cognee, Strata
from anthropic_gateway import AnthropicGateway
from common import ROOT, append, load_env, now, redact_error, save
from run import answer, report
from scoring import score


def read_jsonl(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line]


async def checkpoint_knowledge_time(strata, checkpoint):
    # Source sequence is wave*10 in this fixed corpus. The max recorded time through the
    # completed wave is after all checkpoint knowledge and before the later batch.
    max_sequence = max(
        int(event["source_sequence"])
        for event in read_jsonl(ROOT / "fixtures/events.jsonl")[:checkpoint]
    )
    sql = (
        "SELECT coalesce(max(recorded_at)::text,'') FROM source_events "
        f"WHERE workspace_id='{strata.ws}' AND graph_space_id='{strata.gs}' "
        f"AND source_sequence::int <= {max_sequence}"
    )
    proc = await asyncio.create_subprocess_exec(
        "docker", "exec", "strata-comparison-postgres", "psql", "-U", "postgres",
        "-d", strata.database, "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql,
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    out, error = await proc.communicate()
    if proc.returncode or not out.strip():
        raise RuntimeError("Cannot reconstruct checkpoint knowledge time: " + error.decode())
    return datetime.datetime.fromisoformat(out.decode().strip()).isoformat()


async def run(args):
    folder = ROOT / "results" / args.run_id
    runtime = ROOT / ".runtime" / args.run_id
    manifest_path = folder / "manifest.json"
    if not manifest_path.exists():
        raise ValueError("run manifest does not exist")
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("status") not in ("running", "failed"):
        raise ValueError(f"run status {manifest.get('status')!r} is not resumable")
    if manifest.get("provider") != "anthropic":
        raise ValueError("this recovery path currently supports the Anthropic profile")
    if manifest.get("progress", {}).get("strata", {}).get("accepted") != manifest["events"]:
        raise ValueError("Strata did not durably accept the complete final batch")
    if manifest.get("progress", {}).get("cognee", {}).get("accepted") != manifest["events"]:
        raise ValueError("Cognee did not durably accept the complete final batch")

    events = read_jsonl(ROOT / "fixtures/events.jsonl")
    questions = read_jsonl(ROOT / "fixtures/questions.jsonl")
    rows = read_jsonl(folder / "answers.jsonl")
    # Processing may have crossed the next barrier before a snapshot/evaluation failed.
    # Resume from the last checkpoint whose full answer matrix is durable, not merely the
    # largest processed counter in the manifest.
    checkpoint = 0
    row_keys = {(row["system"], row["question_id"]) for row in rows}
    for candidate in sorted({q["checkpoint"] for q in questions}):
        required = {
            (system, q["id"])
            for system in manifest["systems"]
            for q in questions
            if q["checkpoint"] <= candidate
        }
        if required <= row_keys:
            checkpoint = candidate
    expected_existing = sum(q["checkpoint"] <= checkpoint for q in questions) * len(
        manifest["systems"]
    )
    if len(rows) != expected_existing:
        raise ValueError(
            f"checkpoint {checkpoint} requires {expected_existing} existing answers, found {len(rows)}"
        )
    pending_questions = [q for q in questions if q["checkpoint"] > checkpoint]
    if {q["checkpoint"] for q in pending_questions} != {len(events)}:
        raise ValueError("resume must start at the last completed checkpoint")

    key = load_env("anthropic")
    if not key:
        raise RuntimeError("ANTHROPIC_API_KEY is not configured")
    models = manifest["models"]
    gateway = AnthropicGateway(
        key,
        embedding_model=models["strata_embedding"],
        embedding_backend=manifest["local_embeddings"]["backend"],
        embedding_upstream=manifest["local_embeddings"].get("upstream")
        or "http://127.0.0.1:11434",
        journal=folder / "model-requests.jsonl",
        concurrency=manifest["anthropic"]["inference_concurrency"],
    )
    engines = []
    prior_error = manifest.pop("error", None)
    manifest["recovery"] = dict(
        resumed_at=now(),
        from_checkpoint=checkpoint,
        reason="runner process terminated externally without a handled benchmark error",
        method=(
            "release abandoned Strata leases; replay accepted events through idempotency; "
            "retry Cognee cognify over its durable dataset"
        ),
        resume_harness_sha256={
            name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
            for name in ("resume.py", "adapters.py")
        },
    )
    if prior_error:
        manifest["recovery"]["prior_error"] = prior_error
    manifest["status"] = "resuming"
    save(manifest_path, manifest)
    report(folder, manifest, rows)

    try:
        await gateway.start()
        base_url = gateway.base_url
        placeholder = "anthropic-local-gateway"

        strata = Strata(
            args.run_id, runtime / "strata", placeholder, models,
            base_url=base_url, gateway_kind="anthropic",
        )
        await strata.resume()
        existing_receipts = folder / "strata-receipts.json"
        if existing_receipts.exists():
            strata.receipts = json.loads(existing_receipts.read_text())
        strata.checkpoints[checkpoint] = await checkpoint_knowledge_time(strata, checkpoint)
        engines.append(strata)

        cognee = Cognee(
            args.run_id, runtime / "cognee", placeholder, models,
            base_url=base_url, gateway_kind="anthropic", capability_mode="default",
        )
        await cognee.start()
        cognee.pending = len(events) - checkpoint
        engines.append(cognee)

        # Replaying only into Strata reconstructs receipts and is idempotent. Cognee's
        # final add already committed before the interruption; cognify below resumes it.
        receipt = await strata.ingest_batch(events[checkpoint:])
        append(folder / "events.jsonl", dict(
            system="strata", mode="resume_idempotent_batch", count=len(events)-checkpoint,
            accepted_at=now(), receipt=receipt,
        ))

        async def flush_one(engine):
            started = time.monotonic()
            result = await engine.flush()
            append(folder / "batches.jsonl", dict(
                system=engine.name, through_event=len(events), recovery=True,
                flush_seconds=round(time.monotonic()-started, 3), result=result,
            ))
            return result

        results = await asyncio.gather(*(flush_one(e) for e in engines), return_exceptions=True)
        for result in results:
            if isinstance(result, BaseException):
                raise result
        for engine, result in zip(engines, results):
            processed = (
                result.get("events_processed", len(events))
                if isinstance(result, dict) else len(events)
            )
            # Strata reports all reconstructed receipts. Cognee reports a task map.
            manifest["progress"][engine.name]["processed"] = (
                processed if engine.name == "strata" else len(events)
            )
        save(manifest_path, manifest)

        encoding = tiktoken.get_encoding("cl100k_base")
        async with httpx.AsyncClient(timeout=3600) as client:
            for engine in engines:
                stats = await engine.snapshot(folder, len(events), pending_questions)
                manifest["progress"][engine.name]["graph"] = stats
                for question in pending_questions:
                    row = dict(
                        system=engine.name, question_id=question["id"],
                        category=question["category"],
                        cohort=question.get("cohort", "development"),
                        checkpoint=len(events), question=question["question"],
                    )
                    started = time.monotonic()
                    try:
                        context, raw = await engine.retrieve(question, args.context_tokens)
                        tokens = encoding.encode(context)
                        context = encoding.decode(tokens[: args.context_tokens])
                        row.update(
                            context=context, retrieval_raw=raw,
                            context_tokens=min(len(tokens), args.context_tokens),
                            context_truncated=len(tokens) > args.context_tokens,
                            retrieval_ms=round((time.monotonic()-started)*1000, 2),
                        )
                        answer_started = time.monotonic()
                        output, meta = await answer(
                            client, placeholder, question, context, models["answer"], base_url,
                        )
                        row.update(
                            answer=output, answer_metadata=meta,
                            answer_ms=round((time.monotonic()-answer_started)*1000, 2),
                            score=score(question, output, context,
                                        {e["event_id"]: e for e in events}),
                        )
                    except Exception as exc:
                        row["error"] = redact_error(exc)
                    row["elapsed_ms"] = round((time.monotonic()-started)*1000, 2)
                    rows.append(row)
                    append(folder / "answers.jsonl", row)
                    print(json.dumps(dict(
                        stage="question", system=engine.name, id=question["id"],
                        score=row.get("score", {}).get("fact_accuracy"),
                        error=row.get("error"),
                    )), flush=True)
                    report(folder, manifest, rows)

        manifest["status"] = (
            "completed" if len(rows) == len(questions)*len(engines) else "incomplete"
        )
        manifest.pop("error", None)
        manifest["recovery"]["completed_at"] = now()
    except BaseException as exc:
        manifest["status"] = "failed"
        manifest["error"] = redact_error(exc)
        print(manifest["error"], flush=True)
    finally:
        for engine in engines:
            try:
                await engine.close()
            except Exception as exc:
                manifest.setdefault("cleanup_errors", []).append(redact_error(exc))
        try:
            all_requests = read_jsonl(folder / "model-requests.jsonl")
            save(folder / "model-requests.json", all_requests)
            await gateway.close()
        except Exception as exc:
            manifest.setdefault("cleanup_errors", []).append(redact_error(exc))
        manifest["ended_at"] = now()
        save(manifest_path, manifest)
        report(folder, manifest, rows)
        print(f"Report: {folder/'REPORT.md'}", flush=True)
    return 0 if manifest["status"] == "completed" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_id")
    parser.add_argument("--context-tokens", type=int, default=6000)
    raise SystemExit(asyncio.run(run(parser.parse_args())))
