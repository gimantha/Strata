"""Offline tests for the harness. No network, no models, no database.

  bench/.venv/bin/python -m unittest discover -s bench -v
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
import tempfile
import unittest
from datetime import datetime
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import httpx  # noqa: E402

import datasets  # noqa: E402
import prompts  # noqa: E402
import report  # noqa: E402
import tokens  # noqa: E402
from backend import BM25Turns  # noqa: E402
from models import ChatModel, ModelError, parse_spec  # noqa: E402

LOCOMO_SAMPLE = [
    {
        "sample_id": "conv-26",
        "conversation": {
            "speaker_a": "Ann",
            "speaker_b": "Bo",
            "session_1_date_time": "1:56 pm on 8 May, 2023",
            "session_1": [
                {"speaker": "Ann", "dia_id": "D1:1", "text": "I adopted a dog named Rex yesterday."},
                {"speaker": "Bo", "dia_id": "D1:2", "text": "Lovely!", "blip_caption": "a brown dog"},
            ],
            "session_2_date_time": "10:00 am on 9 June, 2023",
            "session_2": [{"speaker": "Bo", "dia_id": "D2:1", "text": "I moved to Lisbon for a new job."}],
        },
        "qa": [
            {"question": "What is the dog's name?", "answer": "Rex", "evidence": ["D1:1"], "category": 4},
            {"question": "When did Bo move?", "answer": 2023, "evidence": ["D2:1; D1:2"], "category": 2},
            {"question": "Trick?", "adversarial_answer": "x", "evidence": [], "category": 5},
        ],
    },
    {
        "sample_id": "conv-99",
        "conversation": {
            "speaker_a": "Cy",
            "speaker_b": "Di",
            "session_1_date_time": "bad date",
            "session_1": [{"speaker": "Cy", "dia_id": "D1:1", "text": "Hello there."}],
        },
        "qa": [{"question": "Who said hello?", "answer": "Cy", "evidence": ["D9:9"], "category": 1}],
    },
]


def lme_item(qid, qtype, answer_turn=True):
    return {
        "question_id": qid,
        "question_type": qtype,
        "question": "Where do I work?",
        "answer": "Acme",
        "question_date": "2023/05/30 (Tue) 10:00",
        "haystack_session_ids": ["s1", "s2"],
        "haystack_dates": ["2023/05/20 (Sat) 02:21", "2023/05/21 (Sun) 09:00"],
        "haystack_sessions": [
            [{"role": "user", "content": "I just started at Acme.", "has_answer": answer_turn},
             {"role": "assistant", "content": "Congratulations!"}],
            [{"role": "user", "content": "Any tips for hiking?"}],
        ],
        "answer_session_ids": ["s1"],
    }


class DatasetTests(unittest.TestCase):
    def test_locomo_shape(self):
        ds = datasets.load_locomo(LOCOMO_SAMPLE, "x")
        self.assertEqual([q.qid for q in ds.questions], ["conv-26:0", "conv-26:1", "conv-99:0"])
        q = ds.questions[1]
        self.assertEqual(q.answer, "2023")
        self.assertEqual(q.evidence, ["D2:1", "D1:2"])
        self.assertEqual(q.category, "temporal")
        self.assertEqual(q.split, "dev")
        self.assertEqual(q.question_date, datetime(2023, 6, 9, 10, 0))
        self.assertEqual(ds.questions[2].split, "test")
        conv = ds.conversations["conv-26"]
        self.assertEqual(conv.sessions[0].time, datetime(2023, 5, 8, 13, 56))
        self.assertIn("[shares a photo: a brown dog]", conv.sessions[0].turns[1].text)
        self.assertIsNone(ds.conversations["conv-99"].sessions[0].time)

    def test_longmemeval_shape_and_dev_split(self):
        raw = [lme_item(f"q{i}", "multi-session") for i in range(8)] + [lme_item("q_abs", "knowledge-update", False)]
        raw[-1]["question_id"] = "z_abs"
        ds = datasets.load_longmemeval(raw, "x")
        q = next(q for q in ds.questions if q.qid == "q0")
        self.assertEqual(q.evidence, ["s1:0"])
        self.assertEqual(q.question_date, datetime(2023, 5, 30, 10, 0))
        self.assertEqual(ds.conversations["q0"].sessions[1].time, datetime(2023, 5, 21, 9, 0))
        dev = [q for q in ds.questions if q.split == "dev" and q.category == "multi-session"]
        self.assertEqual(len(dev), datasets.LONGMEMEVAL_DEV_PER_TYPE)
        abs_q = next(q for q in ds.questions if q.qid == "z_abs")
        self.assertTrue(abs_q.abstention)
        self.assertEqual(abs_q.evidence, [])

    def test_select_is_stable(self):
        raw = [lme_item(f"q{i}", t) for i, t in enumerate(["a", "a", "a", "b", "b"])]
        ds = datasets.load_longmemeval(raw, "x")
        _, first = datasets.select(ds, per_category=1)
        _, again = datasets.select(ds, per_category=1)
        self.assertEqual([q.qid for q in first], [q.qid for q in again])
        self.assertEqual(sorted(q.category for q in first), ["a", "b"])
        convs, qs = datasets.select(ds, max_questions=2)
        self.assertEqual(len(qs), 2)
        self.assertEqual([c.conv_id for c in convs], [q.conv_id for q in qs])

    def test_select_conversations(self):
        ds = datasets.load_locomo(LOCOMO_SAMPLE, "x")
        convs, qs = datasets.select(ds, conversations=1)
        self.assertEqual([c.conv_id for c in convs], ["conv-26"])
        self.assertEqual(len(qs), 2)
        _, test_only = datasets.select(ds, split="test")
        self.assertEqual([q.conv_id for q in test_only], ["conv-99"])

    def test_turn_found(self):
        ctx = datasets.normalize("Caroline: I adopted a dog named Rex yesterday! And more text.")
        self.assertTrue(datasets.turn_found("I adopted a dog named Rex yesterday.", ctx))
        self.assertFalse(datasets.turn_found("I adopted a cat named Rex yesterday.", ctx))
        long_turn = " ".join(f"word{i}" for i in range(40))
        half = datasets.normalize(" ".join(f"word{i}" for i in range(20)))
        self.assertTrue(datasets.turn_found(long_turn, half))
        quarter = datasets.normalize(" ".join(f"word{i}" for i in range(10)))
        self.assertFalse(datasets.turn_found(long_turn, quarter))

    def test_evidence_recall(self):
        ds = datasets.load_locomo(LOCOMO_SAMPLE, "x")
        conv = ds.conversations["conv-26"]
        got = datasets.evidence_recall(ds.questions[1], conv, "Bo: I moved to Lisbon for a new job.")
        self.assertEqual(got, dict(found=1, total=2, missing_ids=0))
        self.assertIsNone(datasets.evidence_recall(ds.questions[2], ds.conversations["conv-99"], "x"))


class PromptTests(unittest.TestCase):
    def test_final_answer(self):
        self.assertEqual(prompts.final_answer("thinking...\nANSWER: Rex"), "Rex")
        self.assertEqual(prompts.final_answer("a\n**Answer:** 7 May 2023"), "7 May 2023")
        self.assertEqual(prompts.final_answer("Answer: first\nmore\nANSWER: second\nline two"), "second\nline two")
        self.assertEqual(prompts.final_answer("  no marker  "), "no marker")

    def test_locomo_judge_label(self):
        self.assertEqual(prompts.judge_label("locomo", 'Close enough. {"label": "CORRECT"}'), 1)
        self.assertEqual(prompts.judge_label("locomo", '{"label": "WRONG"}'), 0)
        self.assertEqual(prompts.judge_label("locomo", "It is WRONG."), 0)
        with self.assertRaises(ValueError):
            prompts.judge_label("locomo", "no idea")

    def test_longmemeval_judge(self):
        self.assertEqual(prompts.judge_label("longmemeval", "Yes."), 1)
        self.assertEqual(prompts.judge_label("longmemeval", "no"), 0)
        p = prompts.judge_prompt("longmemeval", "knowledge-update", False, "Q", "A", "R")
        self.assertIn("updated answer", p)
        p = prompts.judge_prompt("longmemeval", "multi-session", True, "Q", "A", "R")
        self.assertIn("unanswerable", p)
        p = prompts.judge_prompt("locomo", "temporal", False, "Q?", "Gold", "Gen")
        self.assertIn("Gold answer: Gold", p)

    def test_prompt_hash_is_stable(self):
        self.assertEqual(prompts.prompt_hash(), prompts.prompt_hash())


class TokenAndBaselineTests(unittest.TestCase):
    def test_cut(self):
        text = "one two three four five six seven"
        cut, was_cut = tokens.cut(text, 3)
        self.assertTrue(was_cut)
        self.assertEqual(tokens.count(cut), 3)
        same, was_cut = tokens.cut(text, 1000)
        self.assertEqual((same, was_cut), (text, False))

    def test_bm25_ranks_and_respects_budget(self):
        ds = datasets.load_locomo(LOCOMO_SAMPLE, "x")
        b = BM25Turns(Path("."), {})
        asyncio.run(b.ingest(list(ds.conversations.values())))
        got = asyncio.run(b.retrieve(ds.questions[0], 1000))
        self.assertTrue(got.context.splitlines()[0].endswith("I adopted a dog named Rex yesterday."))
        small = asyncio.run(b.retrieve(ds.questions[0], 5))
        self.assertEqual(small.context, "")


class ModelTests(unittest.TestCase):
    def setUp(self):
        os.environ["ANTHROPIC_API_KEY"] = "test-key"
        os.environ["OPENAI_API_KEY"] = "test-key"

    def test_parse_spec(self):
        self.assertEqual(parse_spec("anthropic:claude-x"), ("anthropic", "claude-x"))
        with self.assertRaises(ValueError):
            parse_spec("claude-x")

    def test_anthropic_drops_temperature_after_refusal(self):
        seen = []

        def handler(request):
            body = json.loads(request.content)
            seen.append(body)
            if "temperature" in body:
                return httpx.Response(400, json={"error": {"message": "temperature is not supported"}})
            return httpx.Response(200, json={"content": [{"type": "text", "text": "ANSWER: Rex"}],
                                             "usage": {"input_tokens": 5, "output_tokens": 2}, "model": "m"})

        model = ChatModel("anthropic:m", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
        got = asyncio.run(model.complete("hi", 10))
        self.assertEqual((got.text, got.input_tokens), ("ANSWER: Rex", 5))
        self.assertEqual(len(seen), 2)
        self.assertNotIn("temperature", seen[1])

    def test_retries_overload(self):
        calls = []

        def handler(request):
            calls.append(1)
            if len(calls) == 1:
                return httpx.Response(529, headers={"retry-after": "0"})
            return httpx.Response(200, json={"choices": [{"message": {"content": "ok"}}],
                                             "usage": {"prompt_tokens": 1, "completion_tokens": 1}})

        model = ChatModel("openai:m", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
        self.assertEqual(asyncio.run(model.complete("hi", 10)).text, "ok")
        self.assertEqual(len(calls), 2)

    def test_hard_error_is_raised(self):
        model = ChatModel("openai:m", client=httpx.AsyncClient(transport=httpx.MockTransport(
            lambda r: httpx.Response(401, json={"error": "bad key"}))))
        with self.assertRaises(ModelError):
            asyncio.run(model.complete("hi", 10))


class ReportTests(unittest.TestCase):
    def test_interval_is_repeatable(self):
        values = [1.0, 0.0, 1.0, 1.0, 0.0, 1.0]
        self.assertEqual(report.interval(values), report.interval(values))
        low, high = report.interval(values)
        self.assertLessEqual(low, report.mean(values))
        self.assertGreaterEqual(high, report.mean(values))

    def test_latest_prefers_good_rows(self):
        rows = [dict(qid="a", error="x"), dict(qid="a", v=1), dict(qid="a", error="y")]
        self.assertEqual(report.latest(rows)["a"], dict(qid="a", v=1))

    def test_scores_count_failures_as_wrong(self):
        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)

            def write(name, rows):
                (d / name).write_text("".join(json.dumps(r) + "\n" for r in rows))

            write("contexts.jsonl", [dict(qid="a", split="test"), dict(qid="b", error="boom"),
                                     dict(qid="c", split="test"), dict(qid="d", split="test")])
            write("answers.jsonl", [dict(qid="a", answer="x"), dict(qid="c", error="model down")])
            write("judgments.jsonl", [dict(qid="a", repeat=0, label=1), dict(qid="a", repeat=1, label=0)])
            _, score, counts = report.scores_for(d)
            self.assertEqual(score, dict(a=0.5, b=0.0, c=0.0))
            self.assertEqual(counts["not_answered"], 1)
            self.assertEqual(counts["retrieval_errors"], 1)
            self.assertEqual(counts["answer_errors"], 1)


class QuestionReportTests(unittest.TestCase):
    def test_shows_each_question_with_its_source_and_answers(self):
        ds = datasets.load_locomo(LOCOMO_SAMPLE, "sha")
        convs, qs = datasets.select(ds, conversations=1)
        dog = [q for q in qs if q.question == "What is the dog's name?"][0]
        move = [q for q in qs if q.question == "When did Bo move?"][0]
        with tempfile.TemporaryDirectory() as tmp:
            run = Path(tmp)
            d = run / "strata"
            d.mkdir()

            def write(name, rows):
                (d / name).write_text("".join(json.dumps(r) + "\n" for r in rows))

            write("contexts.jsonl", [
                dict(qid=dog.qid, context="[1] Ann: I adopted a dog named Rex yesterday."),
                dict(qid=move.qid, context="nothing useful"),
            ])
            write("answers.jsonl", [dict(qid=dog.qid, answer="Rex | the dog")])
            write("judgments.jsonl", [dict(qid=dog.qid, repeat=0, label=1)])
            manifest = dict(run="t", dataset="locomo", budget=4000)
            text = report.questions(run, manifest, {c.conv_id: c for c in convs}, qs)

        self.assertIn("What is the dog's name?", text)
        self.assertIn("**Correct answer:** Rex", text)
        self.assertIn("found 1 of 1; correct", text)
        self.assertIn("Rex \\| the dog", text)  # a pipe in an answer cannot break the table
        self.assertIn("I adopted a dog named Rex", text)
        self.assertIn("0 of 2 (missing `D2:1`, `D1:2`)", text)
        self.assertIn("not answered yet", text)


class StrataPayloadTests(unittest.TestCase):
    def test_event_uses_chat_shape(self):
        from backend_strata import Strata, model_env

        ds = datasets.load_locomo(LOCOMO_SAMPLE, "x")
        conv = ds.conversations["conv-26"]
        with tempfile.TemporaryDirectory() as tmp:
            s = Strata(Path(tmp), {})
            ev = s.event(conv, 0, conv.sessions[0])
        self.assertEqual(ev["media_type"], "application/json")
        self.assertEqual(ev["event_time"], "2023-05-08T13:56:00Z")
        self.assertEqual(ev["content_json"]["messages"][0],
                         dict(role="Ann", content="I adopted a dog named Rex yesterday.", timestamp="2023-05-08T13:56:00Z"))
        self.assertEqual(ev["source_sequence"], "000000")
        with self.assertRaises(ValueError):
            model_env("extraction", "anthropic:claude-x")


if __name__ == "__main__":
    unittest.main()
