"""Real engine adapters. No synthetic retrieval, graph writes, or answer injection."""

import asyncio
import hashlib
import json
import os
import secrets
import subprocess
import time
from collections import Counter
from pathlib import Path

import httpx

from common import ROOT, now, plain, save


class Strata:
    name = "strata"

    def __init__(
        self, run_id, runtime, key, models, base_url=None, gateway_kind=None
    ):
        self.run_id, self.runtime = run_id, runtime
        self.key, self.models = key, models
        self.base_url = base_url
        self.gateway_kind = gateway_kind
        self.receipts = {}
        self.checkpoints = {}
        self.process = None
        self.database = "sc_" + hashlib.sha256(run_id.encode()).hexdigest()[:20]

    async def start(self):
        self.runtime.mkdir(parents=True, exist_ok=True)
        # Outbox workers claim across workspaces. A separate database prevents an
        # interrupted diagnostic's expiring lease from entering this run's queue.
        createdb = await asyncio.create_subprocess_exec(
            "docker",
            "exec",
            "strata-comparison-postgres",
            "createdb",
            "-U",
            "postgres",
            self.database,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        _, error = await createdb.communicate()
        if createdb.returncode:
            raise RuntimeError(
                f"Cannot create fresh benchmark database: {error.decode()}"
            )
        token = secrets.token_hex(24)
        keyfile = self.runtime / "api-keys.json"
        save(
            keyfile,
            dict(
                version=1,
                keys=[
                    dict(
                        key_id="comparison",
                        secret_sha256=hashlib.sha256(token.encode()).hexdigest(),
                        principal_id="comparison-admin",
                        kind="service",
                        display_name="Comparison",
                        system_role="admin",
                    )
                ],
            ),
        )
        keyfile.chmod(0o600)
        env = os.environ.copy()
        # Do not inherit a user's production backend configuration.
        env = {k: v for k, v in env.items() if not k.startswith("CG_")}
        env.update(
            CG_DATABASE_URL=f"postgres://postgres:comparison-local@127.0.0.1:55439/{self.database}?sslmode=disable",
            CG_HTTP_ADDR="127.0.0.1:18089",
            CG_AUTO_MIGRATE="true",
            CG_API_KEYS_FILE=str(keyfile),
            CG_BLOB_DIR=str(self.runtime / "blobs"),
            CG_EMBEDDED_WORKER="true",
            CG_LLM_PROVIDER="openai",
            CG_LLM_MODEL=self.models["strata_extraction"],
            CG_LLM_API_KEY=self.key,
            CG_EMBEDDING_PROVIDER="openai",
            CG_EMBEDDING_MODEL=self.models["strata_embedding"],
            CG_EMBEDDING_API_KEY=self.key,
            CG_LOG_FORMAT="json",
            CG_LLM_TIMEOUT="120s",
            CG_WORKER_LEASE="10m",
        )
        if self.base_url:
            env.update(
                CG_LLM_BASE_URL=self.base_url,
                CG_EMBEDDING_BASE_URL=self.base_url,
                CG_LLM_TIMEOUT="60m",
                CG_WORKER_LEASE="70m",
                CG_WORKER_CONCURRENCY="4" if self.gateway_kind == "anthropic" else "1",
            )
        self.log = (self.runtime / "server.log").open("w")
        self.process = subprocess.Popen(
            [str(ROOT / ".runtime/contextgraphd")],
            cwd=ROOT.parent,
            env=env,
            stdout=self.log,
            stderr=subprocess.STDOUT,
        )
        self.client = httpx.AsyncClient(
            base_url="http://127.0.0.1:18089",
            headers={"Authorization": f"Bearer comparison.{token}"},
            timeout=120,
        )
        for _ in range(120):
            if self.process.poll() is not None:
                raise RuntimeError(
                    f"Strata exited; inspect {self.runtime/'server.log'}"
                )
            try:
                r = await self.client.get("/readyz")
                if r.status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            await asyncio.sleep(0.5)
        else:
            raise TimeoutError("Strata readiness timed out")
        ws = await self.request(
            "POST", "/v1/workspaces", dict(slug=self.run_id, name=self.run_id)
        )
        gs = await self.request(
            "POST",
            f"/v1/workspaces/{ws['id']}/graph-spaces",
            dict(slug="comparison", name="Comparison"),
        )
        self.ws, self.gs = ws["id"], gs["id"]
        await self.request(
            "POST",
            f"/v1/workspaces/{self.ws}/sources",
            dict(kind="api", name="procurement-register"),
        )
        save(
            self.runtime / "scope.json",
            dict(workspace_id=self.ws, graph_space_id=self.gs, database=self.database),
        )

    async def request(self, method, path, payload=None):
        r = await self.client.request(method, path, json=payload)
        r.raise_for_status()
        return r.json()

    async def ingest(self, event):
        receipt = await self.request(
            "POST", f"/v1/graph-spaces/{self.gs}/events", self._payload(event)
        )
        self.receipts[event["event_id"]] = receipt
        return receipt

    def _payload(self, event):
        return dict(
            source_name="procurement-register",
            external_id=event["project"],
            event_type="procurement_event",
            operation=event["operation"],
            media_type="text/plain",
            idempotency_key=event["event_id"],
            content=event["content"],
            event_time=event["event_time"],
            source_time=event["event_time"],
            source_sequence=event["source_sequence"],
            source_version=event["source_version"],
            metadata=dict(
                comparison_event_id=event["event_id"],
                simulated_received_at=event["received_at"],
            ),
        )

    async def ingest_batch(self, events):
        response = await self.request(
            "POST",
            f"/v1/graph-spaces/{self.gs}/events",
            [self._payload(event) for event in events],
        )
        failures = [item for item in response["results"] if item.get("error")]
        if failures:
            raise RuntimeError(f"Strata rejected {len(failures)} batch items")
        for event, item in zip(events, response["results"]):
            self.receipts[event["event_id"]] = item["receipt"]
        return dict(accepted=response["accepted"], failed=response["failed"])

    async def flush(self):
        pending = {eid: r for eid, r in self.receipts.items() if not r.get("terminal")}
        start = time.monotonic()
        while pending:
            if time.monotonic() - start > 3600:
                raise TimeoutError(
                    f"Strata processing timeout: {len(pending)} events pending"
                )
            batch = list(pending.items())[:40]
            statuses = await asyncio.gather(
                *[
                    self.request("GET", f"/v1/events/{r['source_event_id']}/status")
                    for _, r in batch
                ]
            )
            for (eid, receipt), status in zip(batch, statuses):
                if status["status"] in ("processed", "failed", "quarantined"):
                    receipt["terminal"] = status
                    del pending[eid]
            if pending:
                await asyncio.sleep(1)
        failures = {
            eid: r["terminal"]
            for eid, r in self.receipts.items()
            if r["terminal"]["status"] != "processed"
        }
        return dict(
            events_processed=len(self.receipts) - len(failures),
            events_terminal=len(self.receipts),
            failures=failures,
        )

    async def retrieve(self, question, budget):
        payload = dict(
            query=question["question"],
            token_budget=budget,
            max_items=30,
            explain=True,
        )
        for coordinate in ("valid_at", "known_at", "active_at"):
            if question.get(coordinate):
                payload[coordinate] = question[coordinate]
        if question.get("known_checkpoint") in self.checkpoints:
            payload["known_at"] = self.checkpoints[question["known_checkpoint"]]
        raw = await self.request(
            "POST",
            f"/v1/graph-spaces/{self.gs}/context",
            payload,
        )
        return raw["context"], raw

    async def _episode_id(self, source_event_id):
        proc = await asyncio.create_subprocess_exec(
            "docker",
            "exec",
            "strata-comparison-postgres",
            "psql",
            "-U",
            "postgres",
            "-d",
            self.database,
            "-tA",
            "-c",
            f"SELECT id FROM episodes WHERE source_event_id='{source_event_id}' ORDER BY sequence LIMIT 1",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        out, err = await proc.communicate()
        if proc.returncode or not out.strip():
            raise RuntimeError(
                "Cannot resolve capability-probe episode: " + err.decode().strip()
            )
        return out.decode().strip()

    async def capability_probe(self, folder, events):
        """Exercise native bitemporal, provenance, and decision-derivation APIs.

        The ordinary benchmark deliberately relies on extraction. This bounded probe uses
        the same already-ingested source events but commits explicit claims so the database
        contract, rather than model wording, is what is under test.
        """
        project_events = {
            event["wave"]: event for event in events if event["project"] == "Project-001"
        }
        required = {2, 3, 7}
        if not required <= project_events.keys():
            raise RuntimeError("Capability probe needs Project-001 waves 2, 3, and 7")

        async def origin(wave):
            event = project_events[wave]
            source_event_id = self.receipts[event["event_id"]]["source_event_id"]
            return event, source_event_id, await self._episode_id(source_event_id)

        audit_event, audit_source, audit_episode = await origin(2)
        decision_event, decision_source, decision_episode = await origin(3)
        correction_event, correction_source, correction_episode = await origin(7)

        async def commit(source_event_id, claims, derivation=None):
            payload = dict(source_event_id=source_event_id, claims=claims)
            if derivation:
                payload["derivation"] = derivation
            return await self.request(
                "POST", f"/v1/graph-spaces/{self.gs}/assertions", payload
            )

        def evidence(episode_id, text):
            return [dict(episode_id=episode_id, extracted_text=text, confidence=1)]

        old = await commit(
            audit_source,
            [
                dict(
                    subject=dict(name="Project-001", type="project"),
                    predicate="capability_audit_status",
                    object=dict(kind="symbol", text="FAILED"),
                    event_time="2026-01-03T00:00:00Z",
                    valid_from="2026-01-03T00:00:00Z",
                    valid_to="2026-01-04T00:00:00Z",
                    provenance_mode="extracted",
                    evidence=evidence(audit_episode, audit_event["content"]),
                )
            ],
        )
        old_id = old["assertions"][0]["id"]

        decision = await commit(
            decision_source,
            [
                dict(
                    subject=dict(name="Project-001", type="project"),
                    predicate="capability_supplier_selection",
                    object=dict(kind="symbol", text="ALDER_SELECTED"),
                    event_time="2026-01-05T00:00:00Z",
                    valid_from="2026-01-05T00:00:00Z",
                    valid_to="2026-01-25T00:00:00Z",
                    provenance_mode="extracted",
                    evidence=evidence(decision_episode, decision_event["content"]),
                )
            ],
        )
        decision_id = decision["assertions"][0]["id"]
        trace = await commit(
            decision_source,
            [
                dict(
                    subject=dict(name="Project-001", type="project"),
                    predicate="capability_decision_reason",
                    object=dict(kind="symbol", text="SECURITY_AUDIT_FAILURE"),
                    event_time="2026-01-05T00:00:00Z",
                    provenance_mode="derived",
                )
            ],
            derivation=dict(
                method="decision_trace",
                rule_name="supplier_selection_rationale",
                rule_version="1",
                input_assertion_ids=[old_id, decision_id],
                parameters=dict(decision_event_id=decision_event["event_id"]),
            ),
        )
        trace_id = trace["assertions"][0]["id"]
        known_before_correction = now()

        corrected = await commit(
            correction_source,
            [
                dict(
                    subject=dict(name="Project-001", type="project"),
                    predicate="capability_audit_status",
                    object=dict(kind="symbol", text="PASSED"),
                    event_time="2026-01-03T00:00:00Z",
                    valid_from="2026-01-03T00:00:00Z",
                    valid_to="2026-01-04T00:00:00Z",
                    provenance_mode="extracted",
                    supersedes=[old_id],
                    evidence=evidence(correction_episode, correction_event["content"]),
                )
            ],
        )
        corrected_id = corrected["assertions"][0]["id"]

        query_path = f"/v1/graph-spaces/{self.gs}/assertions/query"
        base_query = dict(
            predicates=["capability_audit_status"],
            valid_at="2026-01-03T12:00:00Z",
            limit=10,
        )
        historical = await self.request(
            "POST", query_path, {**base_query, "known_at": known_before_correction}
        )
        current = await self.request("POST", query_path, base_query)
        outside_validity = await self.request(
            "POST", query_path, {**base_query, "valid_at": "2026-01-05T00:00:00Z"}
        )
        old_provenance = await self.request(
            "GET", f"/v1/assertions/{old_id}/provenance"
        )
        corrected_provenance = await self.request(
            "GET", f"/v1/assertions/{corrected_id}/provenance"
        )
        decision_provenance = await self.request(
            "GET", f"/v1/assertions/{trace_id}/provenance"
        )

        def values(response):
            return [a.get("object", {}).get("value") for a in response["assertions"]]

        artifact = dict(
            known_before_correction=known_before_correction,
            assertion_ids=dict(
                original=old_id, correction=corrected_id, decision=decision_id, trace=trace_id
            ),
            historical_query=historical,
            current_query=current,
            outside_validity_query=outside_validity,
            provenance=dict(
                original=old_provenance,
                correction=corrected_provenance,
                decision_trace=decision_provenance,
            ),
            checks=dict(
                historical_belief_is_failed=values(historical) == ["FAILED"],
                current_belief_is_passed=values(current) == ["PASSED"],
                world_time_filter_excludes_claim=outside_validity.get("count") == 0,
                correction_superseded_original=old_id in corrected.get("superseded", []),
                original_has_source_chain=bool(old_provenance.get("evidence_chain")),
                correction_has_source_chain=bool(corrected_provenance.get("evidence_chain")),
                decision_trace_has_derivation=bool(decision_provenance.get("derivation")),
            ),
        )
        save(folder / "strata-native-capabilities.json", artifact)
        return artifact["checks"]

    async def snapshot(self, folder, checkpoint, questions):
        # Read-only canonical inspection. UUIDs are engine-generated and validated by UUID.
        import uuid

        ws = str(uuid.UUID(self.ws))
        gs = str(uuid.UUID(self.gs))
        sql = f"""SELECT json_build_object(
          'assertions',(SELECT coalesce(json_agg(row_to_json(a)),'[]'::json) FROM assertions a WHERE a.workspace_id='{ws}' AND a.graph_space_id='{gs}'),
          'entities',(SELECT coalesce(json_agg(row_to_json(e)),'[]'::json) FROM entities e WHERE e.workspace_id='{ws}' AND e.graph_space_id='{gs}'),
          'evidence',(SELECT coalesce(json_agg(row_to_json(v)),'[]'::json) FROM evidence v JOIN assertions a ON a.id=v.assertion_id WHERE a.workspace_id='{ws}' AND a.graph_space_id='{gs}'),
          'model_runs',(SELECT coalesce(json_agg(row_to_json(m)),'[]'::json) FROM model_runs m WHERE m.workspace_id='{ws}' AND m.graph_space_id='{gs}'))"""
        proc = await asyncio.create_subprocess_exec(
            "docker",
            "exec",
            "strata-comparison-postgres",
            "psql",
            "-U",
            "postgres",
            "-d",
            self.database,
            "-tA",
            "-c",
            sql,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        out, err = await proc.communicate()
        if proc.returncode:
            raise RuntimeError(err.decode())
        data = json.loads(out)
        save(folder / f"{self.name}-graph-{checkpoint}.json", data)
        self.checkpoints[checkpoint] = now()
        save(folder / f"{self.name}-receipts.json", self.receipts)
        # Save actual native temporal query results separately from common NL question scores.
        probes = []
        for q in questions:
            if not (q.get("valid_at") or q.get("known_checkpoint")):
                continue
            payload = dict(query=q["question"], explain=True, limit=30)
            if q.get("valid_at"):
                payload["valid_at"] = q["valid_at"]
            if q.get("known_checkpoint") in self.checkpoints:
                payload["known_at"] = self.checkpoints[q["known_checkpoint"]]
            probes.append(
                dict(
                    question_id=q["id"],
                    request=payload,
                    response=await self.request(
                        "POST", f"/v1/graph-spaces/{self.gs}/query", payload
                    ),
                )
            )
        save(folder / f"{self.name}-native-probes-{checkpoint}.json", probes)
        traces = await self.request(
            "GET", f"/v1/graph-spaces/{self.gs}/traces?limit=200"
        )
        save(folder / f"{self.name}-retrieval-traces-{checkpoint}.json", traces)
        # Follow real assertion -> evidence -> source endpoints for a bounded sample.
        provenance = []
        for a in data["assertions"][:10]:
            provenance.append(
                await self.request("GET", f"/v1/assertions/{a['id']}/provenance")
            )
        save(folder / f"{self.name}-provenance-{checkpoint}.json", provenance)
        return dict(
            assertions=len(data["assertions"]),
            entities=len(data["entities"]),
            evidence=len(data["evidence"]),
            assertion_statuses=dict(Counter(a["status"] for a in data["assertions"])),
            assertions_with_supersedes=sum(
                bool(a.get("supersedes_id")) for a in data["assertions"]
            ),
            assertions_with_valid_to=sum(
                bool(a.get("valid_to")) for a in data["assertions"]
            ),
            source_events_with_assertions=len(
                {a["source_event_id"] for a in data["assertions"]}
            ),
            distinct_predicate_names=len(
                {a["predicate_name"] for a in data["assertions"]}
            ),
            model_run_statuses=dict(Counter(m["status"] for m in data["model_runs"])),
        )

    async def close(self):
        if hasattr(self, "client"):
            await self.client.aclose()
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                await asyncio.to_thread(self.process.wait, 30)
            except subprocess.TimeoutExpired:
                self.process.kill()
        if hasattr(self, "log"):
            self.log.close()


class Cognee:
    name = "cognee"

    def __init__(
        self,
        run_id,
        runtime,
        key,
        models,
        base_url=None,
        gateway_kind=None,
        capability_mode="default",
    ):
        self.run_id, self.runtime, self.key, self.models = run_id, runtime, key, models
        self.pending = 0
        self.base_url = base_url
        self.gateway_kind = gateway_kind
        self.capability_mode = capability_mode

    async def start(self):
        self.runtime.mkdir(parents=True, exist_ok=True)
        os.environ.update(
            LLM_API_KEY=self.key,
            EMBEDDING_API_KEY=self.key,
            LLM_PROVIDER="openai",
            LLM_MODEL="openai/" + self.models["cognee_extraction"],
            EMBEDDING_PROVIDER="openai",
            EMBEDDING_MODEL="openai/" + self.models["cognee_embedding"],
            SYSTEM_ROOT_DIRECTORY=str(self.runtime / "system"),
            DATA_ROOT_DIRECTORY=str(self.runtime / "data"),
            COGNEE_LOGS_DIR=str(self.runtime / "logs"),
            TELEMETRY_DISABLED="1",
        )
        if self.base_url:
            os.environ.update(
                LLM_ENDPOINT=self.base_url,
                EMBEDDING_ENDPOINT=self.base_url,
                EMBEDDING_PROVIDER="openai_compatible",
                EMBEDDING_MODEL=self.models["cognee_embedding"],
                EMBEDDING_DIMENSIONS="1536",
                EMBEDDING_MAX_COMPLETION_TOKENS="2048",
                HF_HUB_OFFLINE="1",
                LLM_SEED="42",
                LITELLM_LOCAL_MODEL_COST_MAP="True",
            )
            # The local bridge enforces JSON schemas through Ollama's grammar API.
            # Declare that real endpoint capability so LiteLLM does not incorrectly
            # downgrade an unknown local model to prompted-JSON fallback.
            import litellm

            litellm.register_model(
                {
                    "openai/"
                    + self.models["cognee_extraction"]: dict(
                        max_tokens=8192,
                        max_input_tokens=8192,
                        max_output_tokens=4096,
                        input_cost_per_token=0,
                        output_cost_per_token=0,
                        cache_creation_input_token_cost=0,
                        cache_read_input_token_cost=0,
                        litellm_provider="openai",
                        mode="chat",
                        supports_response_schema=True,
                    )
                }
            )
        # Capability trials are explicit. Cognee's temporal pipeline replaces the
        # default task list, while provenance is appended only to the default
        # pipeline, so the two modes must be measured as separate runs.
        os.environ.pop("CONTRADICTION_DETECTION", None)
        os.environ["PROVENANCE_TRACKING"] = (
            "true" if self.capability_mode == "provenance" else "false"
        )
        import cognee

        self.cognee = cognee
        cognee.config.system_root_directory(str(self.runtime / "system"))
        cognee.config.data_root_directory(str(self.runtime / "data"))
        from cognee.infrastructure.llm.config import get_llm_config
        from cognee.infrastructure.databases.vector.embeddings.config import (
            get_embedding_config,
        )
        from cognee.infrastructure.databases.graph.config import get_graph_config
        from cognee.modules.cognify.config import get_cognify_config

        llm, emb, graph, core = (
            get_llm_config(),
            get_embedding_config(),
            get_graph_config(),
            get_cognify_config(),
        )
        save(
            self.runtime / "resolved-config.json",
            dict(
                version=cognee.__version__,
                llm=llm.llm_model,
                embedding=emb.embedding_model,
                dimensions=emb.embedding_dimensions,
                graph=graph.graph_database_provider,
                contradiction_detection=core.contradiction_detection,
                provenance_tracking=core.provenance_tracking,
                temporal_cognify=self.capability_mode == "temporal",
                capability_mode=self.capability_mode,
                capability_limit=(
                    "Cognee 1.5.4 temporal_cognify replaces the default pipeline; "
                    "PROVENANCE_TRACKING applies to the default pipeline only."
                    if self.capability_mode in ("temporal", "provenance")
                    else None
                ),
                search_type="HYBRID_COMPLETION",
                ingestion="add + cognify",
                native_schema_capability_registered=bool(self.base_url),
                local_chunks_per_batch=1 if self.base_url else None,
                local_data_concurrency=(
                    4 if self.gateway_kind == "anthropic" else 1
                ) if self.base_url else None,
            ),
        )

    async def ingest(self, event):
        result = await self.cognee.add(event["content"], dataset_name=self.run_id)
        self.pending += 1
        return plain(result)

    async def ingest_batch(self, events):
        result = await self.cognee.add(
            [event["content"] for event in events], dataset_name=self.run_id
        )
        self.pending += len(events)
        return dict(accepted=len(events), pipeline=plain(result))

    async def flush(self):
        if not self.pending:
            return {}
        concurrency = 4 if self.gateway_kind == "anthropic" else 1
        kwargs = (
            {"chunks_per_batch": 1, "data_per_batch": concurrency}
            if self.base_url
            else {}
        )
        result = await self.cognee.cognify(
            datasets=[self.run_id],
            temporal_cognify=self.capability_mode == "temporal",
            **kwargs,
        )
        self.pending = 0
        return plain(result)

    async def retrieve(self, question, budget):
        query_type = (
            self.cognee.SearchType.TEMPORAL
            if self.capability_mode == "temporal"
            else self.cognee.SearchType.HYBRID_COMPLETION
        )
        result = await self.cognee.search(
            query_text=question["question"],
            query_type=query_type,
            datasets=[self.run_id],
            only_context=True,
            top_k=30,
        )
        raw = plain(result)
        parts = [r.get("search_result", r) if isinstance(r, dict) else r for r in raw]
        text = "\n\n".join(p if isinstance(p, str) else json.dumps(p) for p in parts)
        return text, raw

    async def snapshot(self, folder, checkpoint, questions):
        from cognee.modules.users.methods import get_default_user
        from cognee.modules.data.methods import get_authorized_existing_datasets
        from cognee.context_global_variables import (
            set_database_global_context_variables,
        )
        from cognee.infrastructure.databases.graph import get_graph_engine

        user = await get_default_user()
        datasets = await get_authorized_existing_datasets([self.run_id], "read", user)
        graphs = []
        for ds in datasets:
            async with set_database_global_context_variables(ds.id, user.id):
                graph = await get_graph_engine()
                nodes, edges = await graph.get_graph_data()
                graphs.append(
                    dict(dataset=str(ds.id), nodes=plain(nodes), edges=plain(edges))
                )
        save(folder / f"{self.name}-graph-{checkpoint}.json", graphs)

        provenance = []
        decision_traces = []
        if self.capability_mode == "provenance":
            from cognee.modules.provenance import get_provenance_manager

            manager = get_provenance_manager()
            for graph in graphs:
                dataset_id = graph["dataset"]
                for node in graph["nodes"][:100]:
                    node_id = str(node[0]) if isinstance(node, (list, tuple)) else str(node)
                    entry = await manager.get_provenance(f"{dataset_id}:{node_id}")
                    if entry:
                        provenance.append(entry)
                for edge in graph["edges"]:
                    if len(edge) < 4 or not isinstance(edge[3], dict):
                        continue
                    text = str(edge[3].get("edge_text", ""))
                    if not any(
                        marker in text
                        for marker in ("SECURITY_AUDIT_FAILURE", "SERVICE_OUTAGE")
                    ):
                        continue
                    source_id = f"{dataset_id}:{edge[0]}"
                    target_id = f"{dataset_id}:{edge[1]}"
                    relationship_id = f"rel:{source_id}:{edge[2]}:{target_id}"
                    decision_traces.append(
                        dict(
                            edge=edge,
                            provenance=await manager.get_provenance(relationship_id),
                        )
                    )
            save(folder / f"{self.name}-provenance-{checkpoint}.json", provenance)
            save(
                folder / f"{self.name}-decision-traces-{checkpoint}.json",
                decision_traces,
            )

        if self.capability_mode == "temporal":
            temporal_nodes = []
            temporal_ids = set()
            for graph in graphs:
                for node in graph["nodes"]:
                    if (
                        isinstance(node, (list, tuple))
                        and len(node) > 1
                        and isinstance(node[1], dict)
                        and node[1].get("type") in ("Event", "Timestamp")
                    ):
                        temporal_nodes.append(node)
                        temporal_ids.add(str(node[0]))
            temporal_edges = [
                edge
                for graph in graphs
                for edge in graph["edges"]
                if str(edge[0]) in temporal_ids or str(edge[1]) in temporal_ids
            ]
            save(
                folder / f"{self.name}-temporal-probes-{checkpoint}.json",
                dict(nodes=temporal_nodes, edges=temporal_edges),
            )

        edge_properties = [
            edge[3]
            for graph in graphs
            for edge in graph["edges"]
            if len(edge) > 3 and isinstance(edge[3], dict)
        ]
        return dict(
            nodes=sum(len(g["nodes"]) for g in graphs),
            edges=sum(len(g["edges"]) for g in graphs),
            temporal_mode=(
                "temporal_cognify + TEMPORAL retrieval"
                if self.capability_mode == "temporal"
                else "disabled"
            ),
            provenance_mode=(
                "PROVENANCE_TRACKING" if self.capability_mode == "provenance" else "disabled"
            ),
            provenance_entries_sampled=len(provenance),
            decision_trace_edges=len(decision_traces),
            timestamp_nodes=sum(
                1
                for graph in graphs
                for node in graph["nodes"]
                if isinstance(node, (list, tuple))
                and len(node) > 1
                and isinstance(node[1], dict)
                and node[1].get("type") == "Timestamp"
            ),
            superseded_edges=sum(bool(p.get("superseded")) for p in edge_properties),
        )

    async def close(self):
        pass
