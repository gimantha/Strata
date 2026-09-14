"""Local protocol bridge; real Ollama inference, with cosine-preserving zero padding.

Both engines share this bridge. It does not extract knowledge, rank results, change
prompts, or supply answers. Requests never fall back to a hosted model.
"""

import asyncio
import hashlib
import json
import time

import httpx
from aiohttp import web
from common import append, now, redact_error


def pad_vector(vector, width=1536):
    if not vector or len(vector) > width:
        raise ValueError(f"Cannot pad embedding of width {len(vector)} to {width}")
    return list(vector) + [0.0] * (width - len(vector))


class OllamaGateway:
    def __init__(self, upstream="http://127.0.0.1:11434", context=8192, journal=None):
        self.upstream = upstream.rstrip("/")
        self.context = context
        self.base_url = "http://127.0.0.1:18090/v1"
        self.lock = asyncio.Lock()
        self.requests = []
        self.journal = journal

    async def start(self):
        self.client = httpx.AsyncClient(base_url=self.upstream, timeout=3600)
        version = await self.client.get("/api/version")
        version.raise_for_status()
        self.version = version.json()
        tags = await self.client.get("/api/tags")
        tags.raise_for_status()
        self.tags = tags.json()
        app = web.Application(client_max_size=16 * 1024 * 1024)
        app.router.add_post("/v1/chat/completions", self.chat)
        app.router.add_post("/v1/embeddings", self.embed)
        self.runner = web.AppRunner(app, shutdown_timeout=5)
        await self.runner.setup()
        await web.TCPSite(self.runner, "127.0.0.1", 18090).start()

    async def call(self, path, payload):
        async with self.lock:
            started = time.monotonic()
            entry = dict(
                path=path,
                model=payload["model"],
                started_at=now(),
                request_sha256=hashlib.sha256(
                    json.dumps(payload, sort_keys=True).encode()
                ).hexdigest(),
                schema_enforced=isinstance(payload.get("format"), dict),
            )
            try:
                response = await self.client.post(path, json=payload)
                entry["status"] = response.status_code
                result = response.json()
                if path == "/api/chat" and response.is_success:
                    entry.update(
                        completion=result.get("message", {}).get("content", ""),
                        input_tokens=result.get("prompt_eval_count", 0),
                        output_tokens=result.get("eval_count", 0),
                        done_reason=result.get("done_reason"),
                    )
                response.raise_for_status()
                return result
            except Exception as exc:
                entry["error"] = redact_error(exc)
                raise
            finally:
                entry["seconds"] = round(time.monotonic() - started, 3)
                self.requests.append(entry)
                if self.journal:
                    append(self.journal, entry)

    async def chat(self, request):
        try:
            p = await request.json()
            options = dict(
                num_ctx=self.context,
                temperature=p.get("temperature", 0),
                seed=p.get("seed", 42),
            )
            limit = p.get("max_completion_tokens", p.get("max_tokens"))
            if limit:
                options["num_predict"] = limit
            body = dict(
                model=p["model"],
                messages=p["messages"],
                stream=False,
                options=options,
                keep_alive="30m",
            )
            fmt = p.get("response_format", {})
            if fmt.get("type") == "json_schema":
                body["format"] = fmt["json_schema"]["schema"]
            elif fmt.get("type") == "json_object":
                body["format"] = "json"
            result = await self.call("/api/chat", body)
            message = result["message"]
            return web.json_response(
                dict(
                    id="ollama-local",
                    object="chat.completion",
                    created=int(time.time()),
                    model=result["model"],
                    choices=[
                        dict(
                            index=0,
                            message=dict(
                                role="assistant", content=message.get("content", "")
                            ),
                            finish_reason=(
                                "length"
                                if result.get("done_reason") == "length"
                                else "stop"
                            ),
                        )
                    ],
                    usage=dict(
                        prompt_tokens=result.get("prompt_eval_count", 0),
                        completion_tokens=result.get("eval_count", 0),
                        total_tokens=result.get("prompt_eval_count", 0)
                        + result.get("eval_count", 0),
                    ),
                )
            )
        except Exception as exc:
            return web.json_response(
                {"error": {"message": str(exc), "type": "ollama_gateway_error"}},
                status=502,
            )

    async def embed(self, request):
        try:
            p = await request.json()
            # Nominal width is a storage constraint, not a request to invent semantic dimensions.
            width = p.get("dimensions", 1536)
            if width != 1536:
                raise ValueError("This comparison gateway requires width=1536")
            result = await self.call(
                "/api/embed",
                dict(
                    model=p["model"], input=p["input"], truncate=False, keep_alive="30m"
                ),
            )
            vectors = [pad_vector(v, width) for v in result["embeddings"]]
            return web.json_response(
                dict(
                    object="list",
                    model=result["model"],
                    data=[
                        dict(object="embedding", index=i, embedding=v)
                        for i, v in enumerate(vectors)
                    ],
                    usage=dict(
                        prompt_tokens=result.get("prompt_eval_count", 0),
                        total_tokens=result.get("prompt_eval_count", 0),
                    ),
                )
            )
        except Exception as exc:
            return web.json_response(
                {"error": {"message": str(exc), "type": "ollama_gateway_error"}},
                status=502,
            )

    async def close(self):
        if hasattr(self, "runner"):
            await self.runner.cleanup()
        if hasattr(self, "client"):
            await self.client.aclose()
