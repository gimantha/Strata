import unittest
import math

from dataset import build
from scoring import aggregate, score
from ollama_gateway import pad_vector
from anthropic_gateway import anthropic_schema, messages_payload
from adapters import Cognee, Strata
from quality_gate import evaluate


class IngestionAccountingTests(unittest.IsolatedAsyncioTestCase):
    async def test_strata_batch_uses_public_batch_endpoint_and_maps_receipts(self):
        engine = Strata("test", None, "unused", {})
        engine.gs = "graph-space"
        events, _ = build(1)
        captured = {}

        async def request(method, path, payload):
            captured.update(method=method, path=path, payload=payload)
            return {
                "accepted": len(payload),
                "failed": 0,
                "results": [
                    {"index": index, "receipt": {"source_event_id": f"source-{index}"}}
                    for index in range(len(payload))
                ],
            }

        engine.request = request
        result = await engine.ingest_batch(events)
        self.assertEqual(result, {"accepted": 10, "failed": 0})
        self.assertEqual(captured["path"], "/v1/graph-spaces/graph-space/events")
        self.assertEqual(len(captured["payload"]), 10)
        self.assertEqual(len(engine.receipts), 10)
        self.assertEqual(engine.receipts["EV-0001"]["source_event_id"], "source-0")

    async def test_failed_event_is_retained_without_blocking_later_events(self):
        engine = Strata("test", None, "unused", {})
        engine.receipts = {
            "EV-0001": {"source_event_id": "bad"},
            "EV-0002": {"source_event_id": "good"},
        }
        calls = []

        async def status(method, path):
            calls.append(path)
            return {"status": "failed" if "/bad/" in path else "processed"}

        engine.request = status
        result = await engine.flush()
        self.assertEqual(result["events_processed"], 1)
        self.assertEqual(result["events_terminal"], 2)
        self.assertEqual(set(result["failures"]), {"EV-0001"})
        engine.receipts["EV-0003"] = {"source_event_id": "later"}
        result = await engine.flush()
        self.assertEqual(result["events_processed"], 2)
        self.assertEqual(result["events_terminal"], 3)
        self.assertEqual(set(result["failures"]), {"EV-0001"})
        self.assertEqual(len(calls), 3)

    def test_cognee_capability_modes_are_explicit(self):
        default = Cognee("test", None, "unused", {}, capability_mode="default")
        temporal = Cognee("test", None, "unused", {}, capability_mode="temporal")
        provenance = Cognee("test", None, "unused", {}, capability_mode="provenance")
        self.assertEqual(default.capability_mode, "default")
        self.assertEqual(temporal.capability_mode, "temporal")
        self.assertEqual(provenance.capability_mode, "provenance")

    async def test_strata_retrieval_forwards_explicit_temporal_coordinates(self):
        engine = Strata("test", None, "unused", {})
        engine.gs = "graph-space"
        engine.checkpoints[600] = "2026-01-12T12:34:56Z"
        captured = {}

        async def request(method, path, payload):
            captured.update(method=method, path=path, payload=payload)
            return {"context": "bounded context"}

        engine.request = request
        context, _ = await engine.retrieve(
            {
                "question": "What was known then?",
                "valid_at": "2026-01-09T12:00:00Z",
                "known_checkpoint": 600,
            },
            2048,
        )
        self.assertEqual(context, "bounded context")
        self.assertEqual(captured["payload"]["valid_at"], "2026-01-09T12:00:00Z")
        self.assertEqual(captured["payload"]["known_at"], "2026-01-12T12:34:56Z")


class EmbeddingBridgeTests(unittest.TestCase):
    def test_padding_preserves_cosine_similarity(self):
        left, right = [0.3, -0.8, 0.2], [-0.2, 0.5, 0.9]

        def cosine(a, b):
            return sum(x * y for x, y in zip(a, b)) / (
                math.sqrt(sum(x * x for x in a)) * math.sqrt(sum(y * y for y in b))
            )

        self.assertEqual(len(pad_vector(left)), 1536)
        self.assertEqual(pad_vector(left)[:3], left)
        self.assertAlmostEqual(
            cosine(left, right), cosine(pad_vector(left), pad_vector(right)), places=14
        )

    def test_padding_rejects_empty_or_wider_vectors(self):
        for vector in ([], [1.0] * 1537):
            with self.assertRaises(ValueError):
                pad_vector(vector)


class AnthropicBridgeTests(unittest.TestCase):
    def test_translates_system_messages_and_structured_output(self):
        schema = {
            "type": "object",
            "properties": {"answer": {"type": "string"}},
            "required": ["answer"],
            "additionalProperties": False,
        }
        result = messages_payload(
            {
                "model": "claude-haiku-4-5-20251001",
                "messages": [
                    {"role": "system", "content": "follow the schema"},
                    {"role": "user", "content": "question"},
                ],
                "response_format": {
                    "type": "json_schema",
                    "json_schema": {"name": "answer", "schema": schema},
                },
            }
        )
        self.assertEqual(result["system"], "follow the schema")
        self.assertEqual(
            result["messages"], [{"role": "user", "content": "question"}]
        )
        self.assertEqual(result["output_config"]["format"]["schema"], schema)
        self.assertEqual(result["temperature"], 0)

    def test_default_anthropic_path_does_not_require_ollama(self):
        from anthropic_gateway import AnthropicGateway

        gateway = AnthropicGateway("unused")
        self.assertEqual(gateway.embedding_backend, "fastembed")
        self.assertEqual(gateway.embedding_model, "BAAI/bge-small-en-v1.5")

    def test_anthropic_schema_removes_unsupported_numeric_bounds(self):
        schema = {
            "type": "object",
            "properties": {
                "confidence": {"type": "number", "minimum": 0, "maximum": 1}
            },
        }
        converted = anthropic_schema(schema)
        self.assertEqual(
            converted["properties"]["confidence"], {"type": "number"}
        )
        self.assertEqual(schema["properties"]["confidence"]["minimum"], 0)

    def test_anthropic_schema_converts_nullable_enum_to_any_of(self):
        original = {
            "type": ["string", "null"],
            "enum": ["string", "integer", None],
        }
        self.assertEqual(
            anthropic_schema(original),
            {
                "anyOf": [
                    {"type": "string", "enum": ["string", "integer"]},
                    {"type": "null", "enum": [None]},
                ]
            },
        )


class CorpusTests(unittest.TestCase):
    def test_deterministic_complete_stream(self):
        events, questions = build()
        self.assertEqual((events, questions), build())
        self.assertEqual(len(events), 1000)
        self.assertEqual(len({e["event_id"] for e in events}), 1000)
        self.assertEqual(len(questions), 140)
        self.assertEqual({q["checkpoint"] for q in questions}, {300, 600, 1000})
        self.assertEqual({q["cohort"] for q in questions}, {"development", "holdout"})
        by_id = {e["event_id"]: e for e in events}
        for q in questions:
            self.assertTrue(q["expected"])
            for eid in q["evidence_ids"]:
                self.assertLessEqual(by_id[eid]["arrival_index"], q["checkpoint"])
                self.assertEqual(by_id[eid]["project"], q["project"])
        # The two adapters only need events: no expected answers in source documents.
        self.assertTrue(all("expected" not in e for e in events))

    def test_late_event_and_correction_differ_from_arrival_time(self):
        events, _ = build()
        for p in {e["project"] for e in events}:
            wave = {e["wave"]: e for e in events if e["project"] == p}
            self.assertLess(wave[9]["event_time"], wave[6]["event_time"])
            self.assertGreater(wave[9]["arrival_index"], wave[6]["arrival_index"])
            self.assertLess(
                int(wave[9]["source_sequence"]), int(wave[6]["source_sequence"])
            )
            self.assertEqual(wave[7]["event_time"], wave[2]["event_time"])
            self.assertGreater(wave[7]["received_at"], wave[3]["received_at"])


class ScoringTests(unittest.TestCase):
    def setUp(self):
        self.events, self.questions = build(1)
        self.by_id = {e["event_id"]: e for e in self.events}
        self.q = next(q for q in self.questions if "-current-budget-" in q["id"])
        self.e = self.by_id[self.q["evidence_ids"][0]]
        self.quote = self.e["content"].split("\n\n")[1]
        self.answer = dict(
            facts=[dict(key="budget_usd", value=self.q["expected"]["budget_usd"][0])],
            citations=[dict(event_id=self.e["event_id"], quote=self.quote)],
        )

    def test_correct_grounded_answer(self):
        s = score(self.q, self.answer, self.e["content"], self.by_id)
        self.assertEqual(s["grounded_accuracy"], 1)

    def test_lucky_answer_without_retrieved_evidence_gets_no_grounded_credit(self):
        s = score(self.q, self.answer, "unrelated retrieval", self.by_id)
        self.assertEqual(s["fact_accuracy"], 1)
        self.assertEqual(s["grounded_accuracy"], 0)

    def test_fabricated_or_wrong_source_citation_fails(self):
        self.answer["citations"][0]["event_id"] = "EV-9999"
        s = score(self.q, self.answer, self.e["content"], self.by_id)
        self.assertEqual(s["citation_validity"], 0)

    def test_listing_both_old_and_new_values_does_not_game_accuracy(self):
        self.answer["facts"].append(dict(key="budget_usd", value="wrong"))
        s = score(self.q, self.answer, self.e["content"], self.by_id)
        self.assertEqual(s["fact_accuracy"], 0)

    def test_failed_queries_remain_in_denominator(self):
        rows = [
            dict(system="engine", category="updates", score=dict(fact_accuracy=1)),
            dict(system="engine", category="updates", error="timeout"),
        ]
        self.assertEqual(aggregate(rows)["engine"]["updates"]["fact_accuracy"], 50)

    def test_quality_gate_uses_only_predeclared_holdout_rows(self):
        rows = [
            dict(system="strata", cohort="development", error="ignored"),
            dict(
                system="strata",
                cohort="holdout",
                score=dict(grounded_accuracy=1, citation_validity=1),
            ),
        ]
        result = evaluate(rows, "strata", minimum_grounded=0.97)
        self.assertTrue(result["passed"])
        self.assertEqual(result["questions"], 1)


if __name__ == "__main__":
    unittest.main()
