"""One tokenizer for every system, so a token budget means the same thing for all of them."""

from __future__ import annotations

import os

from datasets import DATA_DIR

ENCODING = "o200k_base"

# Keep the tokenizer file next to the datasets, so a run works offline after one fetch.
os.environ.setdefault("TIKTOKEN_CACHE_DIR", str(DATA_DIR / "tiktoken"))

_encoding = None


def _enc():
    global _encoding
    if _encoding is None:
        import tiktoken

        _encoding = tiktoken.get_encoding(ENCODING)
    return _encoding


def count(text: str) -> int:
    return len(_enc().encode(text, disallowed_special=()))


def cut(text: str, limit: int) -> tuple[str, bool]:
    """Keep the first `limit` tokens. Returns the text and whether anything was cut."""
    tokens = _enc().encode(text, disallowed_special=())
    if len(tokens) <= limit:
        return text, False
    return _enc().decode(tokens[:limit]), True
