"""The two public datasets, pinned to exact versions and checked by SHA-256.

Files are downloaded on demand into bench/data/ and are never committed. LoCoMo is shared
under a non-commercial license, and LongMemEval is large.

Both datasets are turned into the same simple shape: conversations made of sessions made
of turns, and questions that point at one conversation. Every system gets exactly this.
"""

from __future__ import annotations

import hashlib
import json
import re
import sys
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path

HERE = Path(__file__).resolve().parent
DATA_DIR = HERE / "data"

SOURCES = {
    "locomo": dict(
        file="locomo10.json",
        url="https://raw.githubusercontent.com/snap-research/locomo/"
        "3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376/data/locomo10.json",
        sha256="79fa87e90f04081343b8c8debecb80a9a6842b76a7aa537dc9fdf651ea698ff4",
        about="LoCoMo (Snap Research, ACL 2024), 10 long two-person conversations.",
    ),
    "longmemeval": dict(
        file="longmemeval_s_cleaned.json",
        url="https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/"
        "98d7416c24c778c2fee6e6f3006e7a073259d48f/longmemeval_s_cleaned.json",
        sha256="d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442",
        about="LongMemEval-S, cleaned release (ICLR 2025), 500 questions, about 115k "
        "tokens of chat history each.",
    ),
}

LOCOMO_CATEGORIES = {
    1: "multi-hop",
    2: "temporal",
    3: "open-domain",
    4: "single-hop",
    5: "adversarial",
}

# Questions used for tuning. Look at failures here, never in the test split. The report
# shows the test split as the headline number. Chosen before any run, and never changed.
LOCOMO_DEV_CONVERSATIONS = ("conv-26", "conv-30")
LONGMEMEVAL_DEV_PER_TYPE = 5


@dataclass
class Turn:
    turn_id: str
    speaker: str
    text: str


@dataclass
class Session:
    session_id: str
    time: datetime | None
    turns: list[Turn]


@dataclass
class Conversation:
    conv_id: str
    sessions: list[Session]

    def turn_text(self) -> dict[str, str]:
        return {t.turn_id: t.text for s in self.sessions for t in s.turns}


@dataclass
class Question:
    qid: str
    conv_id: str
    question: str
    answer: str
    category: str
    evidence: list[str]
    question_date: datetime | None
    abstention: bool
    split: str


@dataclass
class Dataset:
    name: str
    sha256: str
    conversations: dict[str, Conversation]
    questions: list[Question] = field(default_factory=list)


def path_for(name: str) -> Path:
    return DATA_DIR / SOURCES[name]["file"]


def fetch(name: str) -> Path:
    """Download one dataset if it is missing, and check its SHA-256 either way."""
    source = SOURCES[name]
    target = path_for(name)
    if target.exists():
        if sha256_file(target) != source["sha256"]:
            raise RuntimeError(f"{target} does not match the pinned SHA-256. Delete it and fetch again.")
        return target
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    partial = target.with_suffix(target.suffix + ".part")
    digest = hashlib.sha256()
    print(f"downloading {name} from {source['url']}", file=sys.stderr)
    with urllib.request.urlopen(source["url"]) as response, partial.open("wb") as out:
        while chunk := response.read(1 << 20):
            digest.update(chunk)
            out.write(chunk)
    if digest.hexdigest() != source["sha256"]:
        partial.unlink()
        raise RuntimeError(f"{name}: downloaded file does not match the pinned SHA-256")
    partial.rename(target)
    return target


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        while chunk := f.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def load(name: str, path: Path | None = None) -> Dataset:
    path = path or fetch(name)
    raw = json.loads(path.read_text())
    digest = sha256_file(path)
    if name == "locomo":
        return load_locomo(raw, digest)
    if name == "longmemeval":
        return load_longmemeval(raw, digest)
    raise ValueError(f"unknown dataset {name!r}")


# --- LoCoMo -------------------------------------------------------------------------------


def parse_locomo_time(text: str | None) -> datetime | None:
    # The dataset writes dates like "1:56 pm on 8 May, 2023".
    if not text:
        return None
    try:
        return datetime.strptime(text.strip(), "%I:%M %p on %d %B, %Y")
    except ValueError:
        return None


def locomo_turn_text(turn: dict) -> str:
    text = (turn.get("text") or "").strip()
    caption = (turn.get("blip_caption") or "").strip()
    # Photos are shared as a caption, the same way other published harnesses do it.
    if caption:
        text = f"{text} [shares a photo: {caption}]".strip()
    return text


def split_evidence(items) -> list[str]:
    # A few entries hold two ids in one string, like "D8:6; D9:17".
    out = []
    for item in items or []:
        for part in re.split(r"[;,]", str(item)):
            part = part.strip()
            if part:
                out.append(part)
    return out


def load_locomo(raw: list, digest: str) -> Dataset:
    ds = Dataset(name="locomo", sha256=digest, conversations={})
    for sample in raw:
        conv_id = sample["sample_id"]
        conv = sample["conversation"]
        numbers = sorted(
            int(m.group(1)) for key in conv if (m := re.fullmatch(r"session_(\d+)", key))
        )
        sessions = []
        for n in numbers:
            turns = [
                Turn(turn_id=t["dia_id"], speaker=t["speaker"], text=locomo_turn_text(t))
                for t in conv[f"session_{n}"]
            ]
            if turns:
                sessions.append(
                    Session(
                        session_id=f"session_{n}",
                        time=parse_locomo_time(conv.get(f"session_{n}_date_time")),
                        turns=turns,
                    )
                )
        ds.conversations[conv_id] = Conversation(conv_id=conv_id, sessions=sessions)
        # LoCoMo questions have no date of their own. They are asked after the last session.
        last = max((s.time for s in sessions if s.time), default=None)
        for index, qa in enumerate(sample["qa"]):
            category = qa.get("category")
            # Category 5 (adversarial) has no gold answer and is left out by the standard
            # method that published LoCoMo scores use.
            if category == 5 or "answer" not in qa:
                continue
            ds.questions.append(
                Question(
                    qid=f"{conv_id}:{index}",
                    conv_id=conv_id,
                    question=qa["question"],
                    answer=str(qa["answer"]),
                    category=LOCOMO_CATEGORIES[category],
                    evidence=split_evidence(qa.get("evidence")),
                    question_date=last,
                    abstention=False,
                    split="dev" if conv_id in LOCOMO_DEV_CONVERSATIONS else "test",
                )
            )
    return ds


# --- LongMemEval --------------------------------------------------------------------------


def parse_longmemeval_time(text: str | None) -> datetime | None:
    # The dataset writes dates like "2023/05/20 (Sat) 02:21".
    if not text:
        return None
    try:
        return datetime.strptime(re.sub(r"\s*\(\w+\)\s*", " ", text.strip()), "%Y/%m/%d %H:%M")
    except ValueError:
        return None


def stable_rank(qid: str) -> str:
    return hashlib.sha256(qid.encode()).hexdigest()


def load_longmemeval(raw: list, digest: str) -> Dataset:
    ds = Dataset(name="longmemeval", sha256=digest, conversations={})
    # Each question has its own chat history. The dev set is the first few questions of
    # each type in a fixed hash order, so it never depends on file order.
    by_type: dict[str, list[str]] = {}
    for item in raw:
        by_type.setdefault(item["question_type"], []).append(item["question_id"])
    dev = {
        qid
        for qids in by_type.values()
        for qid in sorted(qids, key=stable_rank)[:LONGMEMEVAL_DEV_PER_TYPE]
    }
    for item in raw:
        qid = item["question_id"]
        sessions, evidence, seen = [], [], set()
        for sid, date, turns in zip(
            item["haystack_session_ids"], item["haystack_dates"], item["haystack_sessions"]
        ):
            session_id = sid if sid not in seen else f"{sid}#{len(sessions)}"
            seen.add(session_id)
            session_turns = []
            for i, t in enumerate(turns):
                turn_id = f"{session_id}:{i}"
                session_turns.append(Turn(turn_id=turn_id, speaker=t["role"], text=t["content"]))
                if t.get("has_answer"):
                    evidence.append(turn_id)
            if session_turns:
                sessions.append(
                    Session(session_id=session_id, time=parse_longmemeval_time(date), turns=session_turns)
                )
        ds.conversations[qid] = Conversation(conv_id=qid, sessions=sessions)
        ds.questions.append(
            Question(
                qid=qid,
                conv_id=qid,
                question=item["question"],
                answer=str(item["answer"]),
                category=item["question_type"],
                evidence=evidence,
                question_date=parse_longmemeval_time(item.get("question_date")),
                abstention=qid.endswith("_abs"),
                split="dev" if qid in dev else "test",
            )
        )
    return ds


# --- Selection ----------------------------------------------------------------------------


def select(
    ds: Dataset,
    split: str = "all",
    conversations: int | None = None,
    per_category: int | None = None,
    max_questions: int | None = None,
) -> tuple[list[Conversation], list[Question]]:
    """Pick the questions for a run, the same way every time for the same arguments."""
    if split not in ("all", "dev", "test"):
        raise ValueError("split must be all, dev, or test")
    questions = [q for q in ds.questions if split == "all" or q.split == split]
    if per_category:
        keep = set()
        groups: dict[str, list[Question]] = {}
        for q in questions:
            groups.setdefault(q.category, []).append(q)
        for group in groups.values():
            keep.update(q.qid for q in sorted(group, key=lambda q: stable_rank(q.qid))[:per_category])
        questions = [q for q in questions if q.qid in keep]
    if conversations:
        order = list(dict.fromkeys(q.conv_id for q in questions))[:conversations]
        allowed = set(order)
        questions = [q for q in questions if q.conv_id in allowed]
    if max_questions:
        questions = questions[:max_questions]
    conv_ids = list(dict.fromkeys(q.conv_id for q in questions))
    return [ds.conversations[c] for c in conv_ids], questions


# --- Shared rendering ---------------------------------------------------------------------


def format_time(value: datetime | None) -> str:
    if value is None:
        return "unknown date"
    return value.strftime("%A, %d %B %Y, %H:%M")


def render_session(session: Session) -> str:
    lines = [f"Session date: {format_time(session.time)}"]
    lines += [f"{t.speaker}: {t.text}" for t in session.turns]
    return "\n".join(lines)


def render_conversation(conv: Conversation) -> str:
    return "\n\n".join(render_session(s) for s in conv.sessions)


# --- Evidence matching --------------------------------------------------------------------

_WORDS = re.compile(r"[a-z0-9]+")


def normalize(text: str) -> str:
    return " ".join(_WORDS.findall(text.lower()))


def turn_found(turn_text: str, normalized_context: str, window: int = 10) -> bool:
    """Is most of this turn present, word for word, in the retrieved context?

    The turn is cut into pieces of `window` words. It counts as found when at least half
    of the pieces appear in the context. A short turn has one piece, so it must appear in
    full. Systems that rewrite turns into facts will score low here by design; this is a
    check on source passages, not on answers.
    """
    words = normalize(turn_text).split()
    if not words:
        return False
    pieces = [" ".join(words[i : i + window]) for i in range(0, len(words), window)]
    # A tiny last piece matches almost anywhere, so fold it into the one before.
    if len(pieces) > 1 and len(pieces[-1].split()) < window // 2:
        tail = pieces.pop()
        pieces[-1] = pieces[-1] + " " + tail
    hits = sum(1 for p in pieces if p in normalized_context)
    return hits * 2 >= len(pieces)


def evidence_recall(question: Question, conv: Conversation, context: str) -> dict | None:
    texts = conv.turn_text()
    known = [e for e in question.evidence if e in texts]
    if not known:
        return None
    normalized = normalize(context)
    found = [e for e in known if turn_found(texts[e], normalized)]
    return dict(found=len(found), total=len(known), missing_ids=len(question.evidence) - len(known))
