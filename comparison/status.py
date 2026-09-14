"""Read persisted benchmark progress without touching either engine."""

import argparse
import datetime
import errno
import json
import os
from common import ROOT


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run", nargs="?")
    args = parser.parse_args()
    folders = (
        [ROOT / "results" / args.run]
        if args.run
        else sorted(
            (ROOT / "results").glob("*/manifest.json"), key=lambda p: p.stat().st_mtime
        )[-1:]
    )
    if not folders:
        raise SystemExit("No run found")
    folder = folders[0]
    if folder.is_file():
        folder = folder.parent
    manifest = json.loads((folder / "manifest.json").read_text())
    records = []
    journal = (
        folder / "model-requests.jsonl"
        if (folder / "model-requests.jsonl").exists()
        else folder / "ollama-requests.jsonl"
    )
    final_journal = (
        folder / "model-requests.json"
        if (folder / "model-requests.json").exists()
        else folder / "ollama-requests.json"
    )
    calls_available = journal.exists() or final_journal.exists()
    if journal.exists():
        for line in journal.read_text().splitlines():
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    elif final_journal.exists():
        records = json.loads(final_journal.read_text())
    answers = folder / "answers.jsonl"
    progress_values = [
        item.get("processed", 0) for item in manifest["progress"].values()
    ]
    processed = min(progress_values) if progress_values else 0
    started = datetime.datetime.fromisoformat(manifest["started_at"])
    elapsed = max(
        (datetime.datetime.now(datetime.timezone.utc) - started).total_seconds(), 0
    )
    rate = processed / elapsed * 3600 if processed and elapsed else None
    eta = None
    if rate and manifest["status"] == "running":
        eta = (
            datetime.datetime.now(datetime.timezone.utc)
            + datetime.timedelta(hours=(manifest["events"] - processed) / rate)
        ).isoformat()
    launcher = ROOT / ".runtime" / manifest["run_id"] / "launcher.json"
    process_alive = None
    if launcher.exists() and manifest["status"] == "running":
        pid = json.loads(launcher.read_text())["pid"]
        try:
            os.kill(pid, 0)
            process_alive = True
        except OSError as exc:
            process_alive = exc.errno == errno.EPERM
    print(
        json.dumps(
            dict(
                run_id=manifest["run_id"],
                status=manifest["status"],
                events_planned=manifest["events"],
                progress=manifest["progress"],
                process_alive=process_alive,
                events_per_hour=round(rate, 2) if rate else None,
                estimated_completion_utc=eta,
                completed_answer_records=(
                    len(answers.read_text().splitlines()) if answers.exists() else 0
                ),
                planned_answer_records=manifest["questions"] * len(manifest["systems"]),
                completed_model_calls=len(records) if calls_available else None,
                generated_tokens=(
                    sum(r.get("output_tokens", 0) for r in records)
                    if calls_available
                    else None
                ),
                report=str(folder / "REPORT.md"),
                error=manifest.get("error"),
            ),
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
