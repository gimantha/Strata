"""What every system under test must do, plus two simple reference systems.

A backend takes in conversations, then returns context text for each question. It never
sees the gold answer. The harness, not the backend, cuts the context to the shared token
budget, so a backend cannot win by returning more text than the others.

The two reference systems show where the floor and the ceiling are:

  bm25          plain keyword search over single turns, filled up to the budget. If a
                memory system cannot beat this, its extra work is not paying off.
  full-context  the whole conversation with no budget. Shows how well the answer model
                does when nothing is left out. Expensive on LongMemEval.
"""

from __future__ import annotations

import math
import re
import time
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path

import tokens
from datasets import Conversation, Question, format_time, render_conversation


@dataclass
class Retrieved:
    context: str
    latency_ms: float
    info: dict = field(default_factory=dict)


class Backend:
    name = "base"
    # True only for reference systems that are allowed to go over the budget.
    budget_exempt = False

    def __init__(self, run_dir: Path, options: dict):
        self.dir = run_dir
        self.options = options

    def settings(self) -> dict:
        """What to record about this backend in the run manifest."""
        return {}

    async def start(self) -> None:
        pass

    async def ingest(self, conversations: list[Conversation]) -> dict:
        raise NotImplementedError

    async def retrieve(self, question: Question, budget: int) -> Retrieved:
        raise NotImplementedError

    async def close(self) -> None:
        pass


# --- full-context -------------------------------------------------------------------------


class FullContext(Backend):
    name = "full-context"
    budget_exempt = True

    async def ingest(self, conversations):
        self.text = {c.conv_id: render_conversation(c) for c in conversations}
        return dict(conversations=len(conversations), events=sum(len(c.sessions) for c in conversations))

    async def retrieve(self, question, budget):
        return Retrieved(context=self.text[question.conv_id], latency_ms=0.0)


# --- bm25 ---------------------------------------------------------------------------------

_WORD = re.compile(r"[a-z0-9]+")
# A short list on purpose. Question words like "when" and "what" carry no topic.
_STOP = set(
    "a an and are as at be but by did do does for from had has have he her his how i if in "
    "is it its me my of on or our she so that the their them they this to was we were what "
    "when where which who whom why will with would you your".split()
)


def words(text: str) -> list[str]:
    return [w for w in _WORD.findall(text.lower()) if w not in _STOP]


class BM25:
    def __init__(self, docs: list[str], k1: float = 1.2, b: float = 0.75):
        self.k1, self.b = k1, b
        self.docs = [Counter(words(d)) for d in docs]
        self.lengths = [sum(d.values()) for d in self.docs]
        self.avg = (sum(self.lengths) / len(self.lengths)) if self.docs else 0.0
        df = Counter(w for d in self.docs for w in d)
        n = len(self.docs)
        self.idf = {w: math.log(1 + (n - f + 0.5) / (f + 0.5)) for w, f in df.items()}

    def search(self, query: str) -> list[tuple[int, float]]:
        terms = set(words(query))
        scored = []
        for i, doc in enumerate(self.docs):
            score = 0.0
            for w in terms:
                tf = doc.get(w)
                if not tf:
                    continue
                norm = tf + self.k1 * (1 - self.b + self.b * self.lengths[i] / (self.avg or 1))
                score += self.idf[w] * tf * (self.k1 + 1) / norm
            if score > 0:
                scored.append((i, score))
        scored.sort(key=lambda x: (-x[1], x[0]))
        return scored


class BM25Turns(Backend):
    name = "bm25"

    async def ingest(self, conversations):
        self.lines: dict[str, list[str]] = {}
        self.index: dict[str, BM25] = {}
        for conv in conversations:
            lines = [
                f"[{format_time(s.time)}] {t.speaker}: {t.text}" for s in conv.sessions for t in s.turns
            ]
            self.lines[conv.conv_id] = lines
            self.index[conv.conv_id] = BM25(lines)
        return dict(conversations=len(conversations), events=sum(len(c.sessions) for c in conversations))

    async def retrieve(self, question, budget):
        start = time.perf_counter()
        lines = self.lines[question.conv_id]
        picked, used = [], 0
        for i, _ in self.index[question.conv_id].search(question.question):
            cost = tokens.count(lines[i]) + 1
            if used + cost > budget:
                continue
            picked.append(lines[i])
            used += cost
        return Retrieved(context="\n".join(picked), latency_ms=(time.perf_counter() - start) * 1000)


def make_backend(name: str, run_dir: Path, options: dict) -> Backend:
    if name == "bm25":
        return BM25Turns(run_dir, options)
    if name == "full-context":
        return FullContext(run_dir, options)
    if name == "strata":
        from backend_strata import Strata

        return Strata(run_dir, options)
    raise ValueError(f"unknown backend {name!r} (known: strata, bm25, full-context)")
