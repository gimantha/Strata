"""Turn a run's files into REPORT.md and scores.json.

How questions are counted:
  - retrieval failed or the answer model failed: the question counts as wrong (0);
  - not answered or not judged yet: left out, and the report says the run is incomplete;
  - judged several times: the score is the average of the judge's labels.

Error ranges are 95% bootstrap intervals over questions. A difference between two systems
is shown on the questions both have scores for, with its own interval. If that interval
crosses zero, the two systems are not shown to be different.
"""

from __future__ import annotations

import json
import random
from pathlib import Path

SEED = 7
RESAMPLES = 2000


def read_jsonl(path: Path) -> list[dict]:
    if not path.exists():
        return []
    out = []
    for line in path.read_text().splitlines():
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return out


def latest(rows: list[dict]) -> dict[str, dict]:
    """Per question, the last good row, or the last row if none was good."""
    best: dict[str, dict] = {}
    for row in rows:
        old = best.get(row["qid"])
        if old is None or not row.get("error") or old.get("error"):
            best[row["qid"]] = row
    return best


def percentile(values: list[float], p: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, round(p / 100 * len(ordered) + 0.5) - 1))
    return ordered[index]


def mean(values) -> float | None:
    values = list(values)
    return sum(values) / len(values) if values else None


def interval(values: list[float]) -> tuple[float, float] | None:
    if len(values) < 2:
        return None
    rng = random.Random(SEED)
    n = len(values)
    means = sorted(sum(rng.choice(values) for _ in range(n)) / n for _ in range(RESAMPLES))
    return means[int(0.025 * RESAMPLES)], means[int(0.975 * RESAMPLES) - 1]


def scores_for(bdir: Path) -> tuple[dict, dict[str, float], dict]:
    contexts = latest(read_jsonl(bdir / "contexts.jsonl"))
    answers = latest(read_jsonl(bdir / "answers.jsonl"))
    labels: dict[str, dict[int, int]] = {}
    judge_errors = 0
    for row in read_jsonl(bdir / "judgments.jsonl"):
        if row.get("error"):
            judge_errors += 1
            continue
        labels.setdefault(row["qid"], {})[row["repeat"]] = row["label"]
    judged_any = bool(labels)
    score: dict[str, float] = {}
    counts = dict(retrieval_errors=0, answer_errors=0, not_answered=0, not_judged=0, judge_errors=judge_errors)
    for qid, ctx in contexts.items():
        if ctx.get("error"):
            counts["retrieval_errors"] += 1
            score[qid] = 0.0
            continue
        ans = answers.get(qid)
        if ans is None:
            counts["not_answered"] += 1
            continue
        if ans.get("error"):
            counts["answer_errors"] += 1
            score[qid] = 0.0
            continue
        if qid in labels:
            score[qid] = mean(labels[qid].values())
        else:
            counts["not_judged"] += 1
    if not judged_any:
        # Nothing judged yet: retrieval-only numbers are still useful, accuracy is not.
        score = {}
    return contexts, score, counts


def pct(value: float | None) -> str:
    return "n/a" if value is None else f"{100 * value:.1f}"


def pct_range(values: list[float]) -> str:
    ci = interval(values)
    if not values:
        return "n/a"
    if ci is None:
        return pct(mean(values))
    return f"{pct(mean(values))} ({pct(ci[0])} to {pct(ci[1])})"


def build(run_dir: Path, manifest: dict) -> tuple[str, dict]:
    bdirs = sorted(p for p in run_dir.iterdir() if (p / "contexts.jsonl").exists())
    data = {}
    for bdir in bdirs:
        contexts, score, counts = scores_for(bdir)
        data[bdir.name] = dict(
            contexts=contexts,
            score=score,
            counts=counts,
            backend=json.loads((bdir / "backend.json").read_text()) if (bdir / "backend.json").exists() else {},
            answer=json.loads((bdir / "answer.json").read_text()) if (bdir / "answer.json").exists() else {},
            judge=json.loads((bdir / "judge.json").read_text()) if (bdir / "judge.json").exists() else {},
            answers=latest(read_jsonl(bdir / "answers.jsonl")),
        )

    total = manifest["questions"]
    lines = [
        f"# Benchmark run `{manifest['run']}`",
        "",
        f"- Dataset: **{manifest['dataset']}** (file SHA-256 `{manifest['dataset_sha256'][:16]}…`)",
        f"- Questions: {total} from {manifest['conversations']} conversations; selection {json.dumps(manifest['selection'])}",
        f"- Context budget: {manifest['budget']} tokens ({manifest['tokenizer']}), the same for every system",
        f"- Started: {manifest['created_at']}",
        "",
        "Scores are the share of questions the judge marked correct, in percent, with a 95% range.",
        "**Test** is the headline: no tuning may look at those questions. **Dev** is where tuning happens.",
        "",
        "## Answer accuracy",
        "",
        "| System | Test | Dev | All | Scored / total | Answer model | Judge |",
        "|---|---|---|---|---|---|---|",
    ]
    scores_out = dict(run=manifest["run"], dataset=manifest["dataset"], budget=manifest["budget"], systems={})
    for name, d in data.items():
        split_of = {qid: c.get("split") for qid, c in d["contexts"].items()}
        values = {
            s: [v for qid, v in d["score"].items() if s == "all" or split_of.get(qid) == s]
            for s in ("test", "dev", "all")
        }
        lines.append(
            f"| {name} | {pct_range(values['test'])} | {pct_range(values['dev'])} | {pct_range(values['all'])} | "
            f"{len(d['score'])} / {total} | {d['answer'].get('model', '-')} | {d['judge'].get('model', '-')} |"
        )
        scores_out["systems"][name] = dict(
            accuracy={s: mean(v) for s, v in values.items()},
            interval={s: interval(v) for s, v in values.items()},
            scored=len(d["score"]),
            counts=d["counts"],
        )

    incomplete = [n for n, d in data.items() if len(d["score"]) < total]
    if incomplete:
        lines += ["", f"**Incomplete:** {', '.join(incomplete)} do not have a score for every question yet. "
                  "Compare systems only when every row shows the full count."]
    problems = [(n, d["counts"]) for n, d in data.items() if any(d["counts"].values())]
    if problems:
        lines += ["", "Problems counted:", ""]
        lines += [f"- {n}: " + ", ".join(f"{k.replace('_', ' ')} {v}" for k, v in c.items() if v) for n, c in problems]

    # Paired differences against Strata, on the questions both have scores for.
    if "strata" in data and data["strata"]["score"] and len(data) > 1:
        lines += ["", "## Strata compared with each other system", "",
                  "Positive means Strata is ahead. Same questions only.", "",
                  "| Other system | Questions | Difference (points) | 95% range |", "|---|---|---|---|"]
        mine = data["strata"]["score"]
        for name, d in data.items():
            if name == "strata" or not d["score"]:
                continue
            common = sorted(set(mine) & set(d["score"]))
            diffs = [mine[q] - d["score"][q] for q in common]
            ci = interval(diffs)
            lines.append(
                f"| {name} | {len(common)} | {pct(mean(diffs))} | "
                f"{'n/a' if ci is None else pct(ci[0]) + ' to ' + pct(ci[1])} |"
            )
            scores_out["systems"][name]["vs_strata"] = dict(questions=len(common), difference=mean(diffs), interval=ci)

    # Accuracy by question type, all questions.
    categories = sorted({c.get("category") for d in data.values() for c in d["contexts"].values()})
    if any(d["score"] for d in data.values()):
        lines += ["", "## Accuracy by question type (all questions)", "",
                  "| Type | " + " | ".join(data) + " |", "|---|" + "---|" * len(data)]
        for cat in categories:
            cells = []
            for name, d in data.items():
                vals = [v for qid, v in d["score"].items() if d["contexts"][qid].get("category") == cat]
                cells.append(f"{pct(mean(vals))} (n={len(vals)})" if vals else "n/a")
            lines.append(f"| {cat} | " + " | ".join(cells) + " |")

    # Retrieval: what reached the answer model, and how fast.
    lines += ["", "## Retrieval", "",
              "Evidence recall: share of the dataset's evidence turns found word for word in the context. "
              "It only makes sense for systems that return source text; systems that rewrite turns into "
              "facts score low here by design.", "",
              "| System | Evidence recall | All evidence found | Context tokens mean / p95 | Cut to budget | Latency p50 / p95 ms |",
              "|---|---|---|---|---|---|"]
    for name, d in data.items():
        rows = [c for c in d["contexts"].values() if not c.get("error")]
        ev = [c["evidence"] for c in rows if c.get("evidence")]
        frac = [e["found"] / e["total"] for e in ev]
        full = [1.0 if e["found"] == e["total"] else 0.0 for e in ev]
        toks = [c["tokens"] for c in rows]
        lat = [c["latency_ms"] for c in rows]
        cut = sum(1 for c in rows if c.get("cut"))
        lines.append(
            f"| {name} | {pct(mean(frac))} | {pct(mean(full))} | "
            f"{mean(toks) or 0:.0f} / {percentile(toks, 95) or 0:.0f} | {cut} | "
            f"{percentile(lat, 50) or 0:.0f} / {percentile(lat, 95) or 0:.0f} |"
        )
        scores_out["systems"].setdefault(name, {})["retrieval"] = dict(
            evidence_recall=mean(frac), all_evidence_found=mean(full), tokens_mean=mean(toks),
            tokens_p95=percentile(toks, 95), cut=cut, latency_p50_ms=percentile(lat, 50), latency_p95_ms=percentile(lat, 95),
        )

    # Cost: what each system spent to take in the data, and to answer.
    lines += ["", "## Cost", "",
              "| System | Events | Ingest time (s) | Model calls in ingest | Ingest tokens in / out | Answer tokens in / out (mean) |",
              "|---|---|---|---|---|---|"]
    for name, d in data.items():
        ing = d["backend"].get("ingest", {})
        ans = [a for a in d["answers"].values() if not a.get("error")]
        lines.append(
            f"| {name} | {ing.get('events', '-')} | {ing.get('total_seconds', '-')} | {ing.get('model_calls', '-')} | "
            f"{ing.get('model_prompt_tokens', '-')} / {ing.get('model_completion_tokens', '-')} | "
            f"{mean(a.get('input_tokens', 0) for a in ans) or 0:.0f} / {mean(a.get('output_tokens', 0) for a in ans) or 0:.0f} |"
        )
        scores_out["systems"].setdefault(name, {})["ingest"] = ing

    lines += ["", "## System settings", ""]
    for name, d in data.items():
        lines.append(f"- **{name}**: `{json.dumps(d['backend'].get('settings', {}), sort_keys=True)}`")
    lines.append("")
    return "\n".join(lines), scores_out


# --- Per-question report ------------------------------------------------------------------


def _cell(text, limit: int | None = None) -> str:
    """Make text safe for one Markdown table cell."""
    text = " ".join(str(text).split())
    if limit and len(text) > limit:
        text = text[: limit - 1].rstrip() + "…"
    return text.replace("|", "\\|")


def _judged(labels: dict[int, int] | None) -> str:
    if not labels:
        return ""
    value = mean(labels.values())
    if value == 1:
        return "correct"
    if value == 0:
        return "wrong"
    return f"correct in {sum(labels.values())} of {len(labels)} judgings"


def questions(run_dir: Path, manifest: dict, conversations: dict, selected: list) -> str:
    """QUESTIONS.md: every question, its correct answer, the messages that hold the answer,
    and for each system whether those messages reached the context, its answer, and the
    judge's label. Steps not run yet are shown as such, so a run with no model still gets a
    useful report."""
    import datasets

    bdirs = sorted(p for p in run_dir.iterdir() if (p / "contexts.jsonl").exists())
    systems = {}
    for bdir in bdirs:
        labels: dict[str, dict[int, int]] = {}
        for row in read_jsonl(bdir / "judgments.jsonl"):
            if not row.get("error"):
                labels.setdefault(row["qid"], {})[row["repeat"]] = row["label"]
        systems[bdir.name] = dict(
            contexts=latest(read_jsonl(bdir / "contexts.jsonl")),
            answers=latest(read_jsonl(bdir / "answers.jsonl")),
            labels=labels,
        )
    names = list(systems)

    def status(name: str, q, known: list[str], turns: dict) -> dict:
        s = systems[name]
        ctx = s["contexts"].get(q.qid)
        out = dict(found=None, answer="", judge="", note="")
        if ctx is None:
            out["note"] = "not run"
            return out
        if ctx.get("error"):
            out["note"] = "retrieval failed"
            return out
        normalized = datasets.normalize(ctx.get("context", ""))
        out["found"] = [e for e in known if datasets.turn_found(turns[e].text, normalized)]
        ans = s["answers"].get(q.qid)
        if ans is None:
            out["answer"] = "not answered yet"
        elif ans.get("error"):
            out["answer"] = "answer failed"
        else:
            out["answer"] = ans.get("answer") or ""
            out["judge"] = _judged(s["labels"].get(q.qid)) or "not judged yet"
        return out

    lines = [
        f"# Questions in run `{manifest['run']}`",
        "",
        f"Dataset **{manifest['dataset']}**, {len(selected)} questions, a context budget of "
        f"{manifest['budget']} tokens. Systems: {', '.join(names) or 'none yet'}.",
        "",
        "For each question: the correct answer from the dataset, the messages the dataset marks "
        "as holding it, and for each system whether those messages reached its context "
        "(\"found 1 of 2\"), its answer and the judge's label. A message counts as found when "
        "at least half of it appears word for word. Answers and labels appear once the answer "
        "and judge steps have run.",
        "",
        "Dev questions were used to tune Strata; test questions were not.",
        "",
        "## Summary",
        "",
        "| # | Type | Split | Question | Correct answer | " + " | ".join(names) + " |",
        "|---|---|---|---|---|" + "---|" * len(names),
    ]
    details = []
    for n, q in enumerate(selected, 1):
        conv = conversations[q.conv_id]
        turns = {t.turn_id: t for s in conv.sessions for t in s.turns}
        when = {t.turn_id: s.time for s in conv.sessions for t in s.turns}
        known = [e for e in q.evidence if e in turns]
        per = {name: status(name, q, known, turns) for name in names}

        cells = []
        for name in names:
            st = per[name]
            if st["found"] is None:
                cells.append(st["note"])
                continue
            parts = [f"found {len(st['found'])} of {len(known)}" if known else "no source marked"]
            if st["judge"] and st["judge"] != "not judged yet":
                parts.append(st["judge"])
            cells.append("; ".join(parts))
        lines.append(
            f"| [{n}](#q{n}) | {_cell(q.category)} | {q.split} | {_cell(q.question, 90)} | "
            f"{_cell(q.answer, 60)} | " + " | ".join(cells) + " |"
        )

        details += ["", f'<a id="q{n}"></a>', f"### {n}. {_cell(q.question)}", ""]
        details.append(f"- ID `{q.qid}`, type {q.category}, {q.split} split")
        if q.question_date:
            details.append(f"- Asked on {datasets.format_time(q.question_date)}")
        details.append(f"- **Correct answer:** {_cell(q.answer)}")
        if q.abstention:
            details.append("- This question has no answer in the chats: the right reply says so.")
        if known:
            details.append("- **Messages that hold the answer:**")
            for e in known:
                t = turns[e]
                details.append(f"  - `{e}` ({datasets.format_time(when[e])}) {t.speaker}: {_cell(t.text, 400)}")
        elif q.evidence:
            details.append("- The dataset names source messages that are not in the chats.")
        details += ["", "| System | Source messages found | Answer | Judge |", "|---|---|---|---|"]
        for name in names:
            st = per[name]
            if st["found"] is None:
                details.append(f"| {name} | {st['note']} | | |")
                continue
            found = f"{len(st['found'])} of {len(known)}" if known else "-"
            if known and len(st["found"]) < len(known):
                found += " (missing " + ", ".join(f"`{e}`" for e in known if e not in st["found"]) + ")"
            details.append(f"| {name} | {found} | {_cell(st['answer'], 300)} | {st['judge']} |")

    lines += ["", "## Each question"] + details + [""]
    return "\n".join(lines)
