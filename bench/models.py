"""Small clients for the answer and judge models.

A model is named as "provider:model", for example "anthropic:claude-haiku-5-5" or
"openai:gpt-4.1-mini". "openai" also covers any server with the same API: set
OPENAI_BASE_URL to point at it.

Keys come from the environment or from bench/.env. They are never printed or saved.
"""

from __future__ import annotations

import asyncio
import os
import random
from dataclasses import dataclass
from pathlib import Path

import httpx

HERE = Path(__file__).resolve().parent

RETRY_STATUS = {408, 409, 429, 500, 502, 503, 504, 529}


def load_env(path: Path = HERE / ".env") -> None:
    """Read KEY=value lines into the environment. Values already set win."""
    if not path.exists():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        value = value.strip().strip('"').strip("'")
        if value:
            os.environ.setdefault(key.strip(), value)


def parse_spec(spec: str) -> tuple[str, str]:
    provider, sep, model = spec.partition(":")
    if not sep or not model or provider not in ("anthropic", "openai"):
        raise ValueError(f"model must look like anthropic:<model> or openai:<model>, got {spec!r}")
    return provider, model


@dataclass
class Completion:
    text: str
    input_tokens: int
    output_tokens: int
    model: str


class ModelError(RuntimeError):
    pass


class ChatModel:
    def __init__(self, spec: str, client: httpx.AsyncClient | None = None, max_attempts: int = 6):
        self.spec = spec
        self.provider, self.model = parse_spec(spec)
        self.client = client or httpx.AsyncClient(timeout=httpx.Timeout(300, connect=30))
        self.max_attempts = max_attempts
        # Some newer models refuse a temperature setting. After one refusal we stop sending it.
        self.send_temperature = True
        if self.provider == "anthropic":
            self.key = os.environ.get("ANTHROPIC_API_KEY", "")
            self.base_url = os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com").rstrip("/")
            if not self.key:
                raise ModelError("ANTHROPIC_API_KEY is not set (put it in bench/.env)")
        else:
            self.key = os.environ.get("OPENAI_API_KEY", "")
            self.base_url = os.environ.get("OPENAI_BASE_URL", "https://api.openai.com/v1").rstrip("/")
            if not self.key and "api.openai.com" in self.base_url:
                raise ModelError("OPENAI_API_KEY is not set (put it in bench/.env)")

    async def complete(self, prompt: str, max_tokens: int, system: str | None = None) -> Completion:
        last_error = "no attempt made"
        for attempt in range(self.max_attempts):
            request = self._request(prompt, max_tokens, system)
            try:
                response = await self.client.post(request["url"], json=request["body"], headers=request["headers"])
            except httpx.TransportError as err:
                last_error = f"network error: {type(err).__name__}"
                await self._wait(attempt)
                continue
            if response.status_code == 400 and self.send_temperature and "temperature" in response.text.lower():
                self.send_temperature = False
                continue
            if response.status_code in RETRY_STATUS:
                last_error = f"HTTP {response.status_code}"
                await self._wait(attempt, response.headers.get("retry-after"))
                continue
            if response.status_code >= 400:
                # The body can hold the model's error message. It never holds the key.
                raise ModelError(f"{self.spec}: HTTP {response.status_code}: {response.text[:300]}")
            return self._parse(response.json())
        raise ModelError(f"{self.spec}: gave up after {self.max_attempts} attempts ({last_error})")

    def _request(self, prompt: str, max_tokens: int, system: str | None) -> dict:
        if self.provider == "anthropic":
            body = dict(model=self.model, max_tokens=max_tokens, messages=[dict(role="user", content=prompt)])
            if system:
                body["system"] = system
            if self.send_temperature:
                body["temperature"] = 0
            headers = {"x-api-key": self.key, "anthropic-version": "2023-06-01"}
            return dict(url=f"{self.base_url}/v1/messages", body=body, headers=headers)
        messages = ([dict(role="system", content=system)] if system else []) + [dict(role="user", content=prompt)]
        body = dict(model=self.model, messages=messages, seed=42)
        # OpenAI's own API wants max_completion_tokens; most other servers still want max_tokens.
        body["max_completion_tokens" if "api.openai.com" in self.base_url else "max_tokens"] = max_tokens
        if self.send_temperature:
            body["temperature"] = 0
        headers = {"Authorization": f"Bearer {self.key}"} if self.key else {}
        return dict(url=f"{self.base_url}/chat/completions", body=body, headers=headers)

    def _parse(self, raw: dict) -> Completion:
        if self.provider == "anthropic":
            text = "".join(block.get("text", "") for block in raw.get("content", []) if block.get("type") == "text")
            usage = raw.get("usage") or {}
            return Completion(text, usage.get("input_tokens", 0), usage.get("output_tokens", 0), raw.get("model", self.model))
        choice = (raw.get("choices") or [{}])[0]
        usage = raw.get("usage") or {}
        return Completion(
            (choice.get("message") or {}).get("content") or "",
            usage.get("prompt_tokens", 0),
            usage.get("completion_tokens", 0),
            raw.get("model", self.model),
        )

    async def _wait(self, attempt: int, retry_after: str | None = None) -> None:
        if retry_after:
            try:
                await asyncio.sleep(min(float(retry_after), 60))
                return
            except ValueError:
                pass
        await asyncio.sleep(min(60, 2**attempt) * (0.5 + random.random() / 2))

    async def close(self) -> None:
        await self.client.aclose()
