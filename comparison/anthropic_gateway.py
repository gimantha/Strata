"""OpenAI-compatible localhost bridge to Anthropic Messages plus local Ollama embeddings."""

import asyncio
import hashlib
import json
import time
import importlib.metadata

import httpx
from aiohttp import web

from common import ROOT, append, now, redact_error
from ollama_gateway import pad_vector


TRANSIENT = {429, 500, 502, 503, 504, 529}
ANTHROPIC_UNSUPPORTED_SCHEMA_KEYS = {"minimum", "maximum"}


def anthropic_schema(node):
    """Copy a schema while removing numeric bounds Anthropic rejects."""
    if isinstance(node, dict):
        converted = {
            key: anthropic_schema(value)
            for key, value in node.items()
            if key not in ANTHROPIC_UNSUPPORTED_SCHEMA_KEYS
        }
        kinds = converted.get("type")
        if isinstance(kinds, list):
            converted.pop("type")
            values = converted.pop("enum", None)
            alternatives = []
            for kind in kinds:
                alternative = {"type": kind}
                if values is not None:
                    matching = [
                        value
                        for value in values
                        if (kind == "null" and value is None)
                        or (kind == "string" and isinstance(value, str))
                        or (
                            kind == "number"
                            and isinstance(value, (int, float))
                            and not isinstance(value, bool)
                        )
                        or (
                            kind == "integer"
                            and isinstance(value, int)
                            and not isinstance(value, bool)
                        )
                        or (kind == "boolean" and isinstance(value, bool))
                    ]
                    if matching:
                        alternative["enum"] = matching
                alternatives.append(alternative)
            converted["anyOf"] = alternatives
        return converted
    if isinstance(node, list):
        return [anthropic_schema(value) for value in node]
    return node


def messages_payload(payload):
    """Translate the small OpenAI chat subset used by Strata and Cognee."""
    systems, messages = [], []
    for message in payload.get("messages", []):
        role = message.get("role", "user")
        content = message.get("content", "")
        if role == "system":
            systems.append(content if isinstance(content, str) else json.dumps(content))
            continue
        if role not in ("user", "assistant"):
            role = "user"
        if messages and messages[-1]["role"] == role:
            previous = messages[-1]["content"]
            messages[-1]["content"] = f"{previous}\n\n{content}"
        else:
            messages.append(dict(role=role, content=content))
    if not messages:
        messages = [dict(role="user", content="")]
    body = dict(
        model=payload["model"],
        messages=messages,
        max_tokens=(
            payload.get("max_completion_tokens")
            or payload.get("max_tokens")
            or 4096
        ),
        temperature=payload.get("temperature", 0),
    )
    if systems:
        body["system"] = "\n\n".join(systems)
    response_format = payload.get("response_format", {})
    if response_format.get("type") == "json_schema":
        body["output_config"] = {
            "format": {
                "type": "json_schema",
                "schema": anthropic_schema(
                    response_format["json_schema"]["schema"]
                ),
            }
        }
    elif response_format.get("type") == "json_object":
        # Used only as a compatibility fallback. Benchmark-owned calls use schemas.
        body["system"] = body.get("system", "") + "\nReturn only a valid JSON object."
    return body


class AnthropicGateway:
    def __init__(
        self,
        api_key,
        embedding_model="BAAI/bge-small-en-v1.5",
        embedding_backend="fastembed",
        embedding_upstream="http://127.0.0.1:11434",
        journal=None,
        concurrency=4,
    ):
        self.api_key = api_key
        self.embedding_model = embedding_model
        self.embedding_backend = embedding_backend
        self.embedding_upstream = embedding_upstream.rstrip("/")
        self.base_url = "http://127.0.0.1:18090/v1"
        self.journal = journal
        self.requests = []
        self.semaphore = asyncio.Semaphore(concurrency)
        self.concurrency = concurrency

    async def start(self):
        self.anthropic = httpx.AsyncClient(
            base_url="https://api.anthropic.com",
            headers={
                "x-api-key": self.api_key,
                "anthropic-version": "2023-06-01",
                "content-type": "application/json",
            },
            timeout=httpx.Timeout(120, connect=30),
        )
        if self.embedding_backend == "fastembed":
            from fastembed import TextEmbedding

            cache = ROOT / ".cache" / "fastembed"
            cache.mkdir(parents=True, exist_ok=True)
            self.embedding_engine = await asyncio.to_thread(
                TextEmbedding,
                model_name=self.embedding_model,
                cache_dir=str(cache),
            )
            self.embedding_version = importlib.metadata.version("fastembed")
            self.embedding_lock = asyncio.Lock()
        elif self.embedding_backend == "ollama":
            self.ollama = httpx.AsyncClient(
                base_url=self.embedding_upstream, timeout=300
            )
            version = await self.ollama.get("/api/version")
            version.raise_for_status()
            self.embedding_version = version.json()
        else:
            raise ValueError(f"Unknown embedding backend {self.embedding_backend!r}")
        app = web.Application(client_max_size=16 * 1024 * 1024)
        app.router.add_post("/v1/chat/completions", self.chat)
        app.router.add_post("/v1/embeddings", self.embed)
        self.runner = web.AppRunner(app, shutdown_timeout=5)
        await self.runner.setup()
        await web.TCPSite(self.runner, "127.0.0.1", 18090).start()

    async def _anthropic_call(self, payload):
        started = time.monotonic()
        entry = dict(
            provider="anthropic",
            path="/v1/messages",
            model=payload["model"],
            started_at=now(),
            request_sha256=hashlib.sha256(
                json.dumps(payload, sort_keys=True).encode()
            ).hexdigest(),
            schema_enforced=bool(payload.get("output_config", {}).get("format")),
        )
        try:
            async with self.semaphore:
                for attempt in range(6):
                    try:
                        response = await self.anthropic.post(
                            "/v1/messages", json=payload
                        )
                    except httpx.TransportError:
                        entry["transport_retries"] = entry.get(
                            "transport_retries", 0
                        ) + 1
                        if entry["transport_retries"] >= 3:
                            raise
                        await asyncio.sleep(2**attempt)
                        continue
                    entry["status"] = response.status_code
                    if response.status_code not in TRANSIENT or attempt == 5:
                        break
                    retry_after = response.headers.get("retry-after")
                    await asyncio.sleep(
                        float(retry_after) if retry_after else min(2**attempt, 30)
                    )
            result = response.json()
            if response.is_success:
                text = "".join(
                    block.get("text", "")
                    for block in result.get("content", [])
                    if block.get("type") == "text"
                )
                usage = result.get("usage", {})
                entry.update(
                    completion=text,
                    input_tokens=usage.get("input_tokens", 0),
                    output_tokens=usage.get("output_tokens", 0),
                    cache_creation_input_tokens=usage.get(
                        "cache_creation_input_tokens", 0
                    ),
                    cache_read_input_tokens=usage.get("cache_read_input_tokens", 0),
                    stop_reason=result.get("stop_reason"),
                )
            response.raise_for_status()
            return result
        except httpx.HTTPStatusError as exc:
            try:
                entry["provider_error"] = exc.response.json().get("error", {})
            except Exception:
                pass
            entry["error"] = redact_error(exc)
            raise
        except Exception as exc:
            entry["error"] = redact_error(exc)
            raise
        finally:
            entry["seconds"] = round(time.monotonic() - started, 3)
            self.requests.append(entry)
            if self.journal:
                append(self.journal, entry)

    async def validate_generation(self, model):
        """Make a one-token request so model listing alone cannot pass preflight."""
        try:
            await self._anthropic_call(
                dict(
                    model=model,
                    max_tokens=1,
                    temperature=0,
                    messages=[dict(role="user", content="Reply with OK.")],
                )
            )
        except httpx.HTTPStatusError as exc:
            try:
                message = exc.response.json()["error"]["message"]
            except Exception:
                message = str(exc)
            raise RuntimeError(
                f"Anthropic generation preflight failed ({exc.response.status_code}): {message}"
            ) from None

    async def chat(self, request):
        try:
            payload = messages_payload(await request.json())
            result = await self._anthropic_call(payload)
            content = "".join(
                block.get("text", "")
                for block in result.get("content", [])
                if block.get("type") == "text"
            )
            usage = result.get("usage", {})
            return web.json_response(
                dict(
                    id=result.get("id", "anthropic"),
                    object="chat.completion",
                    created=int(time.time()),
                    model=result.get("model", payload["model"]),
                    choices=[
                        dict(
                            index=0,
                            message=dict(role="assistant", content=content),
                            finish_reason=(
                                "length"
                                if result.get("stop_reason") == "max_tokens"
                                else "stop"
                            ),
                        )
                    ],
                    usage=dict(
                        prompt_tokens=usage.get("input_tokens", 0),
                        completion_tokens=usage.get("output_tokens", 0),
                        total_tokens=usage.get("input_tokens", 0)
                        + usage.get("output_tokens", 0),
                    ),
                )
            )
        except httpx.HTTPStatusError as exc:
            try:
                detail = exc.response.json()
            except Exception:
                detail = {"error": {"message": str(exc)}}
            return web.json_response(detail, status=exc.response.status_code)
        except Exception as exc:
            return web.json_response(
                {"error": {"message": redact_error(exc), "type": "gateway_error"}},
                status=502,
            )

    async def embed(self, request):
        try:
            payload = await request.json()
            width = payload.get("dimensions", 1536)
            if width != 1536:
                raise ValueError("This comparison gateway requires width=1536")
            started = time.monotonic()
            if payload["model"] != self.embedding_model:
                raise ValueError(
                    f"Embedding model {payload['model']!r} does not match configured "
                    f"model {self.embedding_model!r}"
                )
            inputs = payload["input"]
            inputs = inputs if isinstance(inputs, list) else [inputs]
            if self.embedding_backend == "fastembed":
                async with self.embedding_lock:
                    generated = await asyncio.to_thread(
                        lambda: list(self.embedding_engine.embed(inputs))
                    )
                vectors = [pad_vector(vector.tolist(), width) for vector in generated]
                model_name = self.embedding_model
                input_tokens = 0
                status = 200
                path = "fastembed"
            else:
                response = await self.ollama.post(
                    "/api/embed",
                    json=dict(
                        model=payload["model"],
                        input=inputs,
                        truncate=False,
                        keep_alive="30m",
                    ),
                )
                response.raise_for_status()
                result = response.json()
                vectors = [
                    pad_vector(vector, width) for vector in result["embeddings"]
                ]
                model_name = result.get("model", payload["model"])
                input_tokens = result.get("prompt_eval_count", 0)
                status = response.status_code
                path = "/api/embed"
            entry = dict(
                provider=self.embedding_backend,
                path=path,
                model=payload["model"],
                status=status,
                started_at=now(),
                seconds=round(time.monotonic() - started, 3),
                input_tokens=input_tokens,
                vectors=len(vectors),
            )
            self.requests.append(entry)
            if self.journal:
                append(self.journal, entry)
            return web.json_response(
                dict(
                    object="list",
                    model=model_name,
                    data=[
                        dict(object="embedding", index=index, embedding=vector)
                        for index, vector in enumerate(vectors)
                    ],
                    usage=dict(
                        prompt_tokens=input_tokens,
                        total_tokens=input_tokens,
                    ),
                )
            )
        except Exception as exc:
            return web.json_response(
                {"error": {"message": redact_error(exc), "type": "gateway_error"}},
                status=502,
            )

    async def close(self):
        if hasattr(self, "runner"):
            await self.runner.cleanup()
        for name in ("anthropic", "ollama"):
            if hasattr(self, name):
                await getattr(self, name).aclose()
