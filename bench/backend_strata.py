"""Strata as a system under test, run the way a real user would run it.

Each run gets:
  - a fresh binary built from the current checkout, with its git commit recorded, so a
    result can never come from an old build by mistake;
  - its own database in a dedicated PostgreSQL container, so runs cannot mix;
  - one graph space per conversation, like one memory per user.

Sessions go in through Strata's own chat input shape ({"messages": [...]}), which keeps
the speaker and the session time on every turn. That is Strata's best input path.

Sending the same session twice has no extra effect: Strata keeps one event per key. So a
stopped run can simply be started again.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import secrets
import subprocess
import sys
import time
from pathlib import Path

import httpx

from backend import Backend, Retrieved
from datasets import Conversation, Question, Session

REPO = Path(__file__).resolve().parent.parent

PG_CONTAINER = "strata-bench-postgres"
PG_IMAGE = "pgvector/pgvector:pg17"
PG_PORT = 55441
PG_PASSWORD = "bench-local"
SOURCE = "benchmark"
DONE = ("processed", "failed", "quarantined")


def run(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=True, capture_output=True, text=True, **kwargs)


def ensure_postgres() -> None:
    state = run(["docker", "ps", "-a", "--filter", f"name=^{PG_CONTAINER}$", "--format", "{{.State}}"]).stdout.strip()
    if not state:
        run([
            "docker", "run", "-d", "--name", PG_CONTAINER,
            "-e", f"POSTGRES_PASSWORD={PG_PASSWORD}",
            "-p", f"127.0.0.1:{PG_PORT}:5432", PG_IMAGE,
        ])
    elif state != "running":
        run(["docker", "start", PG_CONTAINER])
    for _ in range(60):
        ready = subprocess.run(["docker", "exec", PG_CONTAINER, "pg_isready", "-U", "postgres"], capture_output=True)
        if ready.returncode == 0:
            return
        time.sleep(1)
    raise TimeoutError("benchmark PostgreSQL did not become ready")


def psql(database: str, sql: str) -> str:
    return run([
        "docker", "exec", PG_CONTAINER, "psql", "-U", "postgres", "-d", database,
        "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql,
    ]).stdout.strip()


def build_binary(target: Path) -> dict:
    target.parent.mkdir(parents=True, exist_ok=True)
    run(["go", "build", "-o", str(target), "./cmd/contextgraphd"], cwd=REPO)
    commit = run(["git", "rev-parse", "HEAD"], cwd=REPO).stdout.strip()
    # Changes inside bench/ do not change Strata, so they do not make the build "dirty".
    changed = run(["git", "status", "--porcelain", "--", ".", ":(exclude)bench"], cwd=REPO).stdout.strip()
    return dict(commit=commit, uncommitted_changes=bool(changed), go=run(["go", "env", "GOVERSION"]).stdout.strip())


def utc(value) -> str | None:
    # Dataset times have no zone. They are read as UTC, the same for every system.
    return value.strftime("%Y-%m-%dT%H:%M:%SZ") if value else None


def model_env(kind: str, spec: str) -> dict:
    """Turn --extraction / --embedding into Strata settings."""
    prefix = "CG_LLM" if kind == "extraction" else "CG_EMBEDDING"
    if spec in ("none", "hashing"):
        if kind == "extraction" and spec != "none":
            raise ValueError("extraction must be none or openai:<model>")
        if kind == "embedding" and spec not in ("none", "hashing"):
            raise ValueError("embedding must be none, hashing or openai:<model>")
        return {f"{prefix}_PROVIDER": spec}
    provider, _, model = spec.partition(":")
    if provider == "openai" and model:
        env = {f"{prefix}_PROVIDER": "openai", f"{prefix}_MODEL": model}
        key = os.environ.get("OPENAI_API_KEY", "")
        base = os.environ.get("OPENAI_BASE_URL", "")
        if key:
            env[f"{prefix}_API_KEY"] = key
        if base:
            env[f"{prefix}_BASE_URL"] = base
        if not key and not base:
            raise ValueError(f"--{kind} {spec} needs OPENAI_API_KEY (or OPENAI_BASE_URL for a local server)")
        return env
    if provider == "anthropic":
        raise ValueError(
            "Strata talks to models through an OpenAI-style API only. Anthropic models need the "
            "bridge, which is not in this harness yet. Use openai:<model> for now."
        )
    raise ValueError(f"cannot read --{kind} {spec!r}")


class Strata(Backend):
    name = "strata"

    def __init__(self, run_dir: Path, options: dict):
        super().__init__(run_dir, options)
        self.port = int(options.get("strata_port") or 18089)
        self.planner = options.get("strata_planner") or "heuristic"
        self.extraction = options.get("extraction") or "none"
        self.embedding = options.get("embedding") or "none"
        self.workers = int(options.get("strata_workers") or 4)
        self.ingest_timeout = float(options.get("ingest_timeout") or 6 * 3600)
        self.database = "bench_" + hashlib.sha256(str(run_dir.resolve()).encode()).hexdigest()[:20]
        self.scope_file = self.dir / "scope.json"
        self.scope = json.loads(self.scope_file.read_text()) if self.scope_file.exists() else {}
        self.process = None
        self.build = None
        self.env = {**model_env("extraction", self.extraction), **model_env("embedding", self.embedding)}

    def settings(self):
        return dict(
            extraction=self.extraction,
            embedding=self.embedding,
            query_planner=self.planner,
            worker_concurrency=self.workers,
            build=self.build,
            postgres_image=PG_IMAGE,
            database=self.database,
        )

    async def start(self):
        self.dir.mkdir(parents=True, exist_ok=True)
        self.build = build_binary(self.dir / "bin" / "contextgraphd")
        ensure_postgres()
        exists = psql("postgres", f"SELECT 1 FROM pg_database WHERE datname = '{self.database}'")
        if exists and not self.scope:
            # A database with no record of what is in it cannot be resumed. Start it over.
            psql("postgres", f'DROP DATABASE "{self.database}" WITH (FORCE)')
            exists = ""
        if not exists:
            psql("postgres", f'CREATE DATABASE "{self.database}"')
            self.scope = {}

        token = secrets.token_hex(24)
        keyfile = self.dir / "api-keys.json"
        keyfile.write_text(json.dumps(dict(version=1, keys=[dict(
            key_id="bench",
            secret_sha256=hashlib.sha256(token.encode()).hexdigest(),
            principal_id="bench-admin",
            kind="service",
            display_name="Benchmark",
            system_role="admin",
        )])))
        keyfile.chmod(0o600)

        env = {k: v for k, v in os.environ.items() if not k.startswith("CG_")}
        env.update(
            CG_ENV="dev",
            CG_DATABASE_URL=f"postgres://postgres:{PG_PASSWORD}@127.0.0.1:{PG_PORT}/{self.database}?sslmode=disable",
            CG_HTTP_ADDR=f"127.0.0.1:{self.port}",
            CG_AUTO_MIGRATE="true",
            CG_API_KEYS_FILE=str(keyfile),
            CG_BLOB_DIR=str(self.dir / "blobs"),
            CG_EMBEDDED_WORKER="true",
            CG_WORKER_CONCURRENCY=str(self.workers),
            CG_QUERY_PLANNER=self.planner,
            CG_REQUEST_TIMEOUT="120s",
            CG_LLM_TIMEOUT="120s",
            CG_WORKER_LEASE="10m",
            CG_LOG_FORMAT="json",
        )
        env.update(self.env)
        self.log = (self.dir / "server.log").open("a")
        self.process = subprocess.Popen(
            [str(self.dir / "bin" / "contextgraphd")], cwd=REPO, env=env, stdout=self.log, stderr=subprocess.STDOUT
        )
        self.client = httpx.AsyncClient(
            base_url=f"http://127.0.0.1:{self.port}",
            headers={"Authorization": f"Bearer bench.{token}"},
            timeout=httpx.Timeout(180, connect=10),
        )
        for _ in range(240):
            if self.process.poll() is not None:
                raise RuntimeError(f"Strata stopped while starting; see {self.dir / 'server.log'}")
            try:
                if (await self.client.get("/readyz")).status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            await asyncio.sleep(0.5)
        else:
            raise TimeoutError("Strata did not become ready")

        if not self.scope:
            ws = await self.request("POST", "/v1/workspaces", dict(slug="bench", name="Benchmark"))
            await self.request("POST", f"/v1/workspaces/{ws['id']}/sources", dict(kind="api", name=SOURCE))
            self.scope = dict(workspace_id=ws["id"], graph_spaces={})
            self.save_scope()

    def save_scope(self):
        self.scope_file.write_text(json.dumps(self.scope, indent=2))

    async def request(self, method: str, path: str, payload=None):
        response = await self.client.request(method, path, json=payload)
        if response.status_code >= 400:
            raise RuntimeError(f"Strata {method} {path}: HTTP {response.status_code}: {response.text[:300]}")
        return response.json()

    async def graph_space(self, conv_id: str) -> str:
        if conv_id not in self.scope["graph_spaces"]:
            slug = "c-" + "".join(ch if ch.isalnum() else "-" for ch in conv_id.lower())[:100]
            gs = await self.request(
                "POST", f"/v1/workspaces/{self.scope['workspace_id']}/graph-spaces", dict(slug=slug, name=conv_id)
            )
            self.scope["graph_spaces"][conv_id] = gs["id"]
            self.save_scope()
        return self.scope["graph_spaces"][conv_id]

    def event(self, conv: Conversation, index: int, session: Session) -> dict:
        when = utc(session.time)
        messages = []
        for turn in session.turns:
            message = dict(role=turn.speaker, content=turn.text)
            if when:
                message["timestamp"] = when
            messages.append(message)
        external_id = f"{conv.conv_id}/{session.session_id}"
        payload = dict(
            source_name=SOURCE,
            external_id=external_id,
            event_type="conversation_session",
            operation="append",
            media_type="application/json",
            content_json=dict(messages=messages),
            source_sequence=f"{index:06d}",
            idempotency_key=hashlib.sha256(external_id.encode()).hexdigest()[:40],
        )
        if when:
            payload["event_time"] = when
            payload["source_time"] = when
        return payload

    async def ingest(self, conversations):
        started = time.monotonic()
        submitted = rejected = 0
        for n, conv in enumerate(conversations, 1):
            gs = await self.graph_space(conv.conv_id)
            events = [self.event(conv, i, s) for i, s in enumerate(conv.sessions)]
            for start in range(0, len(events), 50):
                batch = events[start : start + 50]
                response = await self.request("POST", f"/v1/graph-spaces/{gs}/events", batch)
                for item in response["results"]:
                    if item.get("error"):
                        rejected += 1
                        print(f"strata rejected an event: {item['error']}", file=sys.stderr)
                    else:
                        submitted += 1
            if n % 25 == 0 or n == len(conversations):
                print(f"strata: submitted {n}/{len(conversations)} conversations", file=sys.stderr)
        submit_seconds = time.monotonic() - started

        statuses = await self.wait_until_done(submitted)
        usage = psql(
            self.database,
            "SELECT count(*) || ',' || coalesce(sum(prompt_tokens),0) || ',' || "
            "coalesce(sum(completion_tokens),0) || ',' || coalesce(sum(cost_micros),0) FROM model_runs",
        ).split(",")
        return dict(
            conversations=len(conversations),
            events=submitted,
            rejected=rejected,
            statuses=statuses,
            submit_seconds=round(submit_seconds, 1),
            total_seconds=round(time.monotonic() - started, 1),
            model_calls=int(usage[0]),
            model_prompt_tokens=int(usage[1]),
            model_completion_tokens=int(usage[2]),
            model_cost_micros=int(usage[3]),
        )

    async def wait_until_done(self, expected: int) -> dict:
        """Wait until every event is finished and the work queue is empty.

        Counting straight from this run's own database is much cheaper than asking about
        each event over HTTP, and the database holds only this run.
        """
        started = time.monotonic()
        last_print = 0.0
        while True:
            rows = psql(self.database, "SELECT status || '=' || count(*) FROM source_events GROUP BY status")
            statuses = {k: int(v) for k, v in (r.split("=") for r in rows.splitlines() if r)}
            busy = int(psql(self.database, "SELECT count(*) FROM outbox_events WHERE status IN ('pending', 'claimed')"))
            finished = sum(statuses.get(s, 0) for s in DONE)
            total = sum(statuses.values())
            if total >= expected and finished == total and busy == 0:
                return statuses
            if self.process.poll() is not None:
                raise RuntimeError(f"Strata stopped during ingest; see {self.dir / 'server.log'}")
            now = time.monotonic()
            if now - started > self.ingest_timeout:
                raise TimeoutError(f"Strata ingest not finished after {self.ingest_timeout:.0f}s: {statuses}")
            if now - last_print > 15:
                print(f"strata: {finished}/{total} events finished, {busy} work items left", file=sys.stderr)
                last_print = now
            await asyncio.sleep(2)

    async def retrieve(self, question: Question, budget: int) -> Retrieved:
        gs = self.scope["graph_spaces"][question.conv_id]
        start = time.perf_counter()
        response = await self.request(
            "POST", f"/v1/graph-spaces/{gs}/context", dict(query=question.question, token_budget=budget)
        )
        elapsed = (time.perf_counter() - start) * 1000
        return Retrieved(
            context=response.get("context", ""),
            latency_ms=elapsed,
            info=dict(own_budget=response.get("budget"), items=len(response.get("items") or [])),
        )

    async def close(self):
        if getattr(self, "client", None):
            await self.client.aclose()
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.process.kill()
        if getattr(self, "log", None):
            self.log.close()
