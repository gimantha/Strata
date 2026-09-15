"""Retry transient answer-model failures using each row's already frozen context."""

import argparse
import asyncio
import json
import time

import httpx

from anthropic_gateway import AnthropicGateway
from common import ROOT, append, load_env, now, redact_error, save
from run import answer, report
from scoring import score


def read_jsonl(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line]


def write_jsonl(path, rows):
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text("".join(json.dumps(row) + "\n" for row in rows))
    temporary.replace(path)


async def run(args):
    folder = ROOT / "results" / args.run_id
    manifest_path = folder / "manifest.json"
    answers_path = folder / "answers.jsonl"
    if not manifest_path.exists() or not answers_path.exists():
        raise ValueError("run manifest or answers are missing")
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("status") in ("running", "resuming", "retrying_answers"):
        raise ValueError("wait for the active benchmark process before retrying answers")
    if manifest.get("provider") != "anthropic":
        raise ValueError("answer retry currently supports the Anthropic profile")

    rows = read_jsonl(answers_path)
    retry_indexes = [
        index for index, row in enumerate(rows)
        if row.get("error") and isinstance(row.get("context"), str)
    ]
    if not retry_indexes:
        print("No answer-model failures with frozen context to retry.")
        return 0

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
    events = {row["event_id"]: row for row in read_jsonl(ROOT / "fixtures/events.jsonl")}
    questions = {row["id"]: row for row in read_jsonl(ROOT / "fixtures/questions.jsonl")}
    prior_status = manifest.get("status")
    manifest["status"] = "retrying_answers"
    manifest.setdefault("answer_recovery", []).append(dict(
        started_at=now(), prior_status=prior_status, attempted=len(retry_indexes),
        method="reuse frozen retrieved context and repeat only the shared answer-model call",
    ))
    recovery = manifest["answer_recovery"][-1]
    save(manifest_path, manifest)
    report(folder, manifest, rows)

    await gateway.start()
    placeholder = "anthropic-local-gateway"
    async with httpx.AsyncClient(timeout=3600) as client:
        for index in retry_indexes:
            row = rows[index]
            question = questions[row["question_id"]]
            prior_error = row["error"]
            started = time.monotonic()
            audit = dict(
                attempted_at=now(), system=row["system"],
                question_id=row["question_id"], prior_error=prior_error,
            )
            try:
                output, meta = await answer(
                    client, placeholder, question, row["context"], models["answer"],
                    gateway.base_url,
                )
                answer_ms = round((time.monotonic() - started) * 1000, 2)
                row.pop("error", None)
                row.update(
                    answer=output,
                    answer_metadata=meta,
                    answer_ms=answer_ms,
                    elapsed_ms=round(row.get("retrieval_ms", 0) + answer_ms, 2),
                    score=score(question, output, row["context"], events),
                )
                audit.update(status="succeeded", answer_ms=answer_ms)
            except Exception as exc:
                row["error"] = redact_error(exc)
                audit.update(status="failed", error=row["error"])
            append(folder / "answer-retries.jsonl", audit)
            write_jsonl(answers_path, rows)
            report(folder, manifest, rows)
            print(json.dumps(audit), flush=True)

    await gateway.close()
    all_requests = read_jsonl(folder / "model-requests.jsonl")
    save(folder / "model-requests.json", all_requests)
    recovery["completed_at"] = now()
    recovery["succeeded"] = sum(not rows[index].get("error") for index in retry_indexes)
    recovery["failed"] = sum(bool(rows[index].get("error")) for index in retry_indexes)
    manifest["status"] = (
        "completed"
        if len(rows) == manifest["questions"] * len(manifest["systems"])
        and not any(row.get("error") for row in rows)
        else "incomplete"
    )
    if manifest["status"] == "completed":
        manifest.pop("error", None)
    manifest["ended_at"] = now()
    save(manifest_path, manifest)
    report(folder, manifest, rows)
    return 0 if manifest["status"] == "completed" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_id")
    raise SystemExit(asyncio.run(run(parser.parse_args())))
