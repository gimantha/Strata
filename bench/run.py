"""Run the public memory benchmark. See bench/README.md for the rules.

Four steps, each one safe to stop and start again. A step skips the questions it has
already done.

  run.py fetch locomo
  run.py retrieve --run R --dataset locomo --backend strata [selection] [--budget 4000]
  run.py answer   --run R --answerer anthropic:claude-haiku-5-5
  run.py judge    --run R --judge anthropic:claude-sonnet-5-5
  run.py report   --run R

Every backend in one run gets the same questions and the same budget. The answer and
judge steps then treat every backend's context in exactly the same way.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import platform
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import datasets  # noqa: E402
import prompts  # noqa: E402
import tokens  # noqa: E402
from backend import make_backend  # noqa: E402
from models import ChatModel, load_env  # noqa: E402

RUNS = HERE / "runs"
ANSWER_MAX_TOKENS = 2048
JUDGE_MAX_TOKENS = 256


# --- small file helpers -------------------------------------------------------------------


def now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def read_json(path: Path, default=None):
    return json.loads(path.read_text()) if path.exists() else default


def write_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(value, indent=2, sort_keys=True, default=str) + "\n")
    tmp.replace(path)


def read_jsonl(path: Path) -> list[dict]:
    if not path.exists():
        return []
    rows = []
    for line in path.read_text().splitlines():
        if line.strip():
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                # A run stopped in the middle of a write leaves a broken last line. Skip it;
                # that question is simply done again.
                pass
    return rows


def append_jsonl(path: Path, row: dict) -> None:
    with path.open("a") as f:
        f.write(json.dumps(row, default=str) + "\n")


def harness_hash() -> str:
    digest = hashlib.sha256()
    for path in sorted(HERE.glob("*.py")):
        digest.update(path.name.encode() + b"\0" + path.read_bytes())
    return digest.hexdigest()[:16]


async def limited(items, worker, limit: int) -> None:
    gate = asyncio.Semaphore(limit)

    async def one(item):
        async with gate:
            await worker(item)

    await asyncio.gather(*(one(i) for i in items))


# --- run manifest -------------------------------------------------------------------------


def selection_args(args) -> dict:
    return dict(
        split=args.split,
        conversations=args.conversations,
        per_category=args.per_category,
        max_questions=args.max_questions,
    )


def load_selection(manifest: dict):
    ds = datasets.load(manifest["dataset"])
    if ds.sha256 != manifest["dataset_sha256"]:
        raise SystemExit("the dataset file changed since this run started")
    convs, questions = datasets.select(ds, **manifest["selection"])
    return {c.conv_id: c for c in convs}, questions


def open_run(args) -> tuple[Path, dict]:
    run_dir = RUNS / args.run
    manifest = read_json(run_dir / "manifest.json")
    if manifest is None:
        raise SystemExit(f"no run named {args.run!r}; start it with the retrieve step")
    return run_dir, manifest


def backend_dirs(run_dir: Path, only: str | None) -> list[Path]:
    dirs = sorted(p for p in run_dir.iterdir() if (p / "contexts.jsonl").exists())
    if only:
        dirs = [p for p in dirs if p.name == only]
        if not dirs:
            raise SystemExit(f"backend {only!r} has no contexts in this run yet")
    return dirs


def check_same(path: Path, settings: dict, what: str) -> None:
    """Refuse to mix two different models in one result file."""
    old = read_json(path)
    if old is None:
        write_json(path, settings)
    elif old.get("model") != settings["model"] or old.get("prompt_hash") != settings["prompt_hash"]:
        raise SystemExit(
            f"{path.parent.name}: {what} was done with {old.get('model')} (prompts {old.get('prompt_hash')}). "
            f"Use a new run for {settings['model']} (prompts {settings['prompt_hash']})."
        )


# --- steps --------------------------------------------------------------------------------


def cmd_fetch(args) -> None:
    names = list(datasets.SOURCES) if args.dataset == "all" else [args.dataset]
    for name in names:
        print(datasets.fetch(name))
    tokens.count("warm up")  # downloads the tokenizer file once, so later runs work offline
    print("tokenizer ready:", tokens.ENCODING)


async def cmd_retrieve(args) -> None:
    load_env()
    run_dir = RUNS / args.run
    ds = datasets.load(args.dataset)
    convs, questions = datasets.select(ds, **selection_args(args))
    if not questions:
        raise SystemExit("the selection has no questions")
    ids = hashlib.sha256("\n".join(q.qid for q in questions).encode()).hexdigest()[:16]
    wanted = dict(
        dataset=args.dataset,
        dataset_sha256=ds.sha256,
        selection=selection_args(args),
        budget=args.budget,
        question_ids_sha256=ids,
        questions=len(questions),
        conversations=len(convs),
        tokenizer=tokens.ENCODING,
    )
    manifest = read_json(run_dir / "manifest.json")
    if manifest is None:
        manifest = dict(run=args.run, created_at=now(), **wanted)
        write_json(run_dir / "manifest.json", manifest)
    else:
        different = {k: (manifest.get(k), v) for k, v in wanted.items() if manifest.get(k) != v}
        if different:
            raise SystemExit(f"this run was started with other settings: {different}")

    out = run_dir / args.backend
    out.mkdir(parents=True, exist_ok=True)
    options = dict(
        extraction=args.extraction,
        embedding=args.embedding,
        strata_port=args.strata_port,
        strata_planner=args.strata_planner,
        strata_workers=args.strata_workers,
        ingest_timeout=args.ingest_timeout,
    )
    backend = make_backend(args.backend, out, options)
    record = read_json(out / "backend.json", {})
    try:
        await backend.start()
        print(f"{args.backend}: ingesting {len(convs)} conversations", file=sys.stderr)
        ingest = await backend.ingest(convs)
        record.setdefault("ingest_history", []).append(dict(at=now(), **ingest))
        record.update(
            backend=args.backend,
            settings=backend.settings(),
            budget_exempt=backend.budget_exempt,
            ingest=ingest,
            ingested_at=now(),
            harness_sha=harness_hash(),
            host=dict(machine=platform.machine(), system=platform.system(), python=platform.python_version()),
        )
        write_json(out / "backend.json", record)

        path = out / "contexts.jsonl"
        done = {r["qid"] for r in read_jsonl(path) if not r.get("error")}
        todo = [q for q in questions if q.qid not in done]
        by_conv = {c.conv_id: c for c in convs}
        print(f"{args.backend}: retrieving for {len(todo)} questions ({len(done)} already done)", file=sys.stderr)

        async def one(q):
            row = dict(qid=q.qid, category=q.category, split=q.split, abstention=q.abstention, at=now())
            try:
                got = await backend.retrieve(q, args.budget)
                row["returned_tokens"] = tokens.count(got.context)
                context, was_cut = got.context, False
                if not backend.budget_exempt:
                    context, was_cut = tokens.cut(got.context, args.budget)
                row.update(
                    context=context,
                    tokens=tokens.count(context) if was_cut else row["returned_tokens"],
                    cut=was_cut,
                    latency_ms=round(got.latency_ms, 1),
                    info=got.info,
                    evidence=datasets.evidence_recall(q, by_conv[q.conv_id], context),
                )
            except Exception as err:  # recorded and counted as wrong, never hidden
                row["error"] = f"{type(err).__name__}: {err}"[:500]
            append_jsonl(path, row)

        await limited(todo, one, args.concurrency)
    finally:
        await backend.close()
    errors = sum(1 for r in read_jsonl(out / "contexts.jsonl") if r.get("error"))
    print(f"{args.backend}: done, {errors} retrieval errors", file=sys.stderr)


async def cmd_answer(args) -> None:
    load_env()
    run_dir, manifest = open_run(args)
    _, questions = load_selection(manifest)
    by_id = {q.qid: q for q in questions}
    model = ChatModel(args.answerer)
    try:
        for bdir in backend_dirs(run_dir, args.backend):
            check_same(bdir / "answer.json", dict(model=args.answerer, prompt_hash=prompts.prompt_hash(), max_tokens=ANSWER_MAX_TOKENS), "answering")
            contexts = {r["qid"]: r for r in read_jsonl(bdir / "contexts.jsonl")}
            path = bdir / "answers.jsonl"
            done = {r["qid"] for r in read_jsonl(path) if not r.get("error")}
            # A question whose retrieval failed is not answered. The report counts it as wrong.
            todo = [qid for qid, ctx in contexts.items() if qid not in done and qid in by_id and not ctx.get("error")]
            print(f"{bdir.name}: answering {len(todo)} questions", file=sys.stderr)

            async def one(qid):
                q, ctx = by_id[qid], contexts[qid]
                row = dict(qid=qid, at=now())
                prompt = prompts.answer_prompt(q.question, ctx["context"], datasets.format_time(q.question_date))
                start = time.perf_counter()
                try:
                    got = await model.complete(prompt, ANSWER_MAX_TOKENS)
                    row.update(
                        response=got.text,
                        answer=prompts.final_answer(got.text),
                        input_tokens=got.input_tokens,
                        output_tokens=got.output_tokens,
                        model=got.model,
                        latency_ms=round((time.perf_counter() - start) * 1000, 1),
                    )
                except Exception as err:
                    row["error"] = f"{type(err).__name__}: {err}"[:500]
                append_jsonl(path, row)

            await limited(todo, one, args.concurrency)
    finally:
        await model.close()


async def cmd_judge(args) -> None:
    load_env()
    run_dir, manifest = open_run(args)
    _, questions = load_selection(manifest)
    by_id = {q.qid: q for q in questions}
    model = ChatModel(args.judge)
    try:
        for bdir in backend_dirs(run_dir, args.backend):
            check_same(bdir / "judge.json", dict(model=args.judge, prompt_hash=prompts.prompt_hash(), max_tokens=JUDGE_MAX_TOKENS), "judging")
            answers = {r["qid"]: r for r in read_jsonl(bdir / "answers.jsonl") if not r.get("error")}
            path = bdir / "judgments.jsonl"
            done = {(r["qid"], r["repeat"]) for r in read_jsonl(path) if not r.get("error")}
            todo = [(qid, k) for qid in answers for k in range(args.repeats) if (qid, k) not in done and qid in by_id]
            print(f"{bdir.name}: judging {len(todo)} answers", file=sys.stderr)

            async def one(item):
                qid, repeat = item
                q, ans = by_id[qid], answers[qid]
                row = dict(qid=qid, repeat=repeat, at=now())
                prompt = prompts.judge_prompt(manifest["dataset"], q.category, q.abstention, q.question, q.answer, ans["answer"])
                try:
                    got = await model.complete(prompt, JUDGE_MAX_TOKENS)
                    row.update(reply=got.text, label=prompts.judge_label(manifest["dataset"], got.text), model=got.model)
                except Exception as err:
                    row["error"] = f"{type(err).__name__}: {err}"[:500]
                append_jsonl(path, row)

            await limited(todo, one, args.concurrency)
    finally:
        await model.close()


def cmd_report(args) -> None:
    import report

    run_dir, manifest = open_run(args)
    text, scores = report.build(run_dir, manifest)
    write_json(run_dir / "scores.json", scores)
    (run_dir / "REPORT.md").write_text(text)
    print(text)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("fetch", help="download a dataset and check it")
    p.add_argument("dataset", choices=[*datasets.SOURCES, "all"])

    p = sub.add_parser("retrieve", help="ingest, then get context for each question")
    p.add_argument("--run", required=True)
    p.add_argument("--dataset", required=True, choices=list(datasets.SOURCES))
    p.add_argument("--backend", required=True)
    p.add_argument("--split", default="all", choices=["all", "dev", "test"])
    p.add_argument("--conversations", type=int)
    p.add_argument("--per-category", type=int)
    p.add_argument("--max-questions", type=int)
    p.add_argument("--budget", type=int, default=4000, help="context tokens per question (default 4000)")
    p.add_argument("--extraction", default="none", help="model a system uses to read text: none or openai:<model>")
    p.add_argument("--embedding", default="none", help="embedding model: none, hashing or openai:<model>")
    p.add_argument("--concurrency", type=int, default=8)
    p.add_argument("--ingest-timeout", type=float, default=6 * 3600)
    p.add_argument("--strata-port", type=int, default=18089)
    p.add_argument("--strata-planner", default="heuristic", choices=["heuristic", "llm"])
    p.add_argument("--strata-workers", type=int, default=4)

    for name, flag, default in (("answer", "--answerer", None), ("judge", "--judge", None)):
        p = sub.add_parser(name)
        p.add_argument("--run", required=True)
        p.add_argument("--backend")
        p.add_argument(flag, required=True, help="provider:model, for example anthropic:claude-haiku-5-5")
        p.add_argument("--concurrency", type=int, default=8)
        if name == "judge":
            p.add_argument("--repeats", type=int, default=1, help="judge each answer this many times")

    p = sub.add_parser("report")
    p.add_argument("--run", required=True)

    args = parser.parse_args()
    if args.cmd == "fetch":
        cmd_fetch(args)
    elif args.cmd == "retrieve":
        asyncio.run(cmd_retrieve(args))
    elif args.cmd == "answer":
        asyncio.run(cmd_answer(args))
    elif args.cmd == "judge":
        asyncio.run(cmd_judge(args))
    elif args.cmd == "report":
        cmd_report(args)


if __name__ == "__main__":
    main()
