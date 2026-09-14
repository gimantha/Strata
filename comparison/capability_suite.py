"""Queue isolated temporal and provenance smoke runs behind an active comparison."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

from common import ROOT, now, save


ACTIVE = {"preflight", "running"}


def read_json(path: Path):
    try:
        return json.loads(path.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return {}


def alive(pid):
    if not isinstance(pid, int) or pid < 1:
        return False
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def wait_for_run(run_id, update):
    if not run_id:
        return
    manifest_path = ROOT / "results" / run_id / "manifest.json"
    launcher_path = ROOT / ".runtime" / run_id / "launcher.json"
    while True:
        manifest = read_json(manifest_path)
        launcher = read_json(launcher_path)
        status = manifest.get("status", "waiting_for_manifest")
        update(waiting_for=dict(run_id=run_id, status=status, process_alive=alive(launcher.get("pid"))))
        if status not in ACTIVE and status != "waiting_for_manifest":
            return
        if status in ACTIVE and launcher and not alive(launcher.get("pid")):
            return
        time.sleep(15)


def execute(args, runtime):
    state = dict(
        suite_id=args.suite_id,
        status="waiting",
        provider=args.provider,
        started_at=now(),
        runs={},
    )

    def update(**changes):
        state.update(changes)
        save(runtime / "status.json", state)

    wait_for_run(args.wait_for_run, update)
    return_codes = []
    for mode in ("temporal", "provenance"):
        run_id = f"{args.suite_id}-{mode}"
        command = [
            sys.executable,
            "-u",
            str(ROOT / "run.py"),
            "--provider",
            args.provider,
            "--smoke",
            "--bulk-ingest",
            "--batch-size",
            "1000",
            "--cognee-mode",
            mode,
            "--run-id",
            run_id,
        ]
        state["status"] = f"running_{mode}"
        state["runs"][mode] = dict(run_id=run_id, status="running", started_at=now())
        update(waiting_for=None)
        code = subprocess.run(command, cwd=ROOT.parent).returncode
        return_codes.append(code)
        run_manifest = read_json(ROOT / "results" / run_id / "manifest.json")
        state["runs"][mode].update(
            status=run_manifest.get("status", "unknown"),
            return_code=code,
            ended_at=now(),
            report=str(ROOT / "results" / run_id / "REPORT.md"),
        )
        update()

    update(
        status="completed" if all(code == 0 for code in return_codes) else "completed_with_failures",
        ended_at=now(),
    )
    return 0 if all(code == 0 for code in return_codes) else 1


def launch(args):
    runtime = ROOT / ".runtime" / args.suite_id
    if runtime.exists():
        raise ValueError(f"suite runtime already exists: {runtime}")
    for mode in ("temporal", "provenance"):
        if (ROOT / "results" / f"{args.suite_id}-{mode}").exists():
            raise ValueError(f"suite result already exists for {mode}")
    runtime.mkdir(parents=True)
    command = [
        sys.executable,
        "-u",
        str(Path(__file__).resolve()),
        "--execute",
        "--provider",
        args.provider,
        "--suite-id",
        args.suite_id,
    ]
    if args.wait_for_run:
        command += ["--wait-for-run", args.wait_for_run]
    caffeinate = shutil.which("caffeinate")
    if caffeinate:
        command = [caffeinate, "-i", *command]
    log_path = runtime / "runner.log"
    save(
        runtime / "status.json",
        dict(
            suite_id=args.suite_id,
            status="queued",
            provider=args.provider,
            waiting_for=dict(run_id=args.wait_for_run),
            runs={},
        ),
    )
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
        dict(pid=process.pid, command=command, launched_at=now(), log=str(log_path)),
    )
    print(f"Queued capability suite {args.suite_id}: PID {process.pid}")
    print(f"Status: {runtime/'status.json'}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--suite-id", required=True)
    parser.add_argument("--wait-for-run")
    parser.add_argument("--provider", choices=["anthropic"], default="anthropic")
    parser.add_argument("--execute", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" for c in args.suite_id):
        parser.error("suite ID must contain only letters, digits, underscores and hyphens")
    runtime = ROOT / ".runtime" / args.suite_id
    if args.execute:
        return execute(args, runtime)
    launch(args)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
