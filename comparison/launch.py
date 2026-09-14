"""Launch a durable local run; progress and results remain under comparison/."""

import argparse
import os
import shutil
import subprocess
import sys
import time

from common import ROOT, now, save


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-id")
    parser.add_argument("--bulk-ingest", action="store_true")
    parser.add_argument("--smoke", action="store_true")
    parser.add_argument(
        "--cognee-mode", choices=["default", "temporal", "provenance"], default="default"
    )
    parser.add_argument(
        "--provider", choices=["ollama", "anthropic"], default="ollama"
    )
    args = parser.parse_args()
    args.run_id = args.run_id or (
        f"{args.provider}-1000-" + time.strftime("%Y%m%d-%H%M%S")
    )
    if not args.run_id or any(
        c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
        for c in args.run_id
    ):
        parser.error(
            "run ID must contain only letters, digits, underscores and hyphens"
        )
    runtime = ROOT / ".runtime" / args.run_id
    if runtime.exists() or (ROOT / "results" / args.run_id).exists():
        parser.error("run already exists; inspect it using status.py")
    runtime.mkdir(parents=True)
    command = [
        sys.executable,
        "-u",
        str(ROOT / "run.py"),
        "--provider",
        args.provider,
        "--batch-size",
        "1000" if args.bulk_ingest else "1",
        "--run-id",
        args.run_id,
        "--cognee-mode",
        args.cognee_mode,
    ]
    if args.bulk_ingest:
        command.append("--bulk-ingest")
    if args.smoke:
        command.append("--smoke")
    # Keep an otherwise idle Mac awake only for this child process's lifetime.
    caffeinate = shutil.which("caffeinate")
    if caffeinate:
        command = [caffeinate, "-i", *command]
    log_path = runtime / "runner.log"
    with log_path.open("x") as log:
        process = subprocess.Popen(
            command,
            cwd=ROOT.parent,
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
            env=os.environ.copy(),
        )
    save(
        runtime / "launcher.json",
        dict(
            pid=process.pid,
            command=command,
            launched_at=now(),
            log=str(log_path),
            prevents_idle_sleep=bool(caffeinate),
        ),
    )
    print(f"Started {args.run_id}: PID {process.pid}. Log: {log_path}")
    print(f"Progress: {sys.executable} comparison/status.py {args.run_id}")


if __name__ == "__main__":
    main()
