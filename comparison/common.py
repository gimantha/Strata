import dataclasses
import datetime
import json
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parent


def plain(obj):
    if hasattr(obj, "model_dump"):
        return obj.model_dump(mode="json")
    if dataclasses.is_dataclass(obj):
        return dataclasses.asdict(obj)
    if isinstance(obj, dict):
        return {str(k): plain(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [plain(v) for v in obj]
    if isinstance(obj, (str, int, float, bool)) or obj is None:
        return obj
    return str(obj)


def save(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(plain(data), indent=2) + "\n")
    temporary.replace(path)


def append(path, data):
    with Path(path).open("a") as f:
        f.write(json.dumps(plain(data)) + "\n")


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def load_env(provider="openai"):
    from dotenv import load_dotenv

    load_dotenv(ROOT / ".env", override=False)
    variable = {
        "openai": "OPENAI_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
    }.get(provider)
    if not variable:
        return ""
    return os.environ.get(variable, "")


def redact_error(error):
    message = str(error) or type(error).__name__
    for key in [
        "OPENAI_API_KEY",
        "ANTHROPIC_API_KEY",
        "LLM_API_KEY",
        "EMBEDDING_API_KEY",
        "CG_LLM_API_KEY",
        "CG_EMBEDDING_API_KEY",
    ]:
        value = os.environ.get(key)
        if value:
            message = message.replace(value, "[REDACTED]")
    return message[:2000]
