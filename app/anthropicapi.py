"""A minimal async client for the Anthropic Messages API.

Hand-rolled on httpx, which is already a dependency (the OpenAI SDK is built on it), rather
than adding the `anthropic` package: the two calls this router needs are one POST and one
streamed POST, so a dependency would buy retries and typed models the pool and app/wire.py
already provide in the shape the rest of the code expects.

The client is protocol-only. It does not know about routing, policy or tracing: it takes an
Anthropic request body and returns an Anthropic response, and app/wire.py converts at both
ends.
"""
from __future__ import annotations

import json
import logging
from typing import AsyncIterator

import httpx

logger = logging.getLogger("mr")

_TIMEOUT = 180.0
# The one host that rejects an Authorization header. Everywhere else -- Databricks, Bedrock
# gateways, LiteLLM, self-hosted proxies -- authenticates with a bearer token, while the
# official API authenticates with x-api-key. Both headers are sent unless the host is the
# official one, because guessing wrong means a 401 the operator cannot diagnose from the UI.
_OFFICIAL_HOST = "api.anthropic.com"


class AnthropicError(Exception):
    """An upstream error, carrying the status code and the upstream's own message."""

    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


class AnthropicClient:
    def __init__(self, base_url: str, api_key: str, version: str):
        self._url = self._messages_url(base_url)
        headers = {
            "content-type": "application/json",
            "accept": "application/json",
            "anthropic-version": version,
            "x-api-key": api_key,
        }
        if _OFFICIAL_HOST not in self._url:
            headers["authorization"] = f"Bearer {api_key}"
        self._client = httpx.AsyncClient(headers=headers, timeout=_TIMEOUT)

    @staticmethod
    def _messages_url(base_url: str) -> str:
        """Resolve the caller's base URL to the messages endpoint.

        Operators paste whatever their provider's documentation shows them, which is
        sometimes the bare host, sometimes a path ending in /v1, and sometimes the full
        endpoint. All three have to work, because a 404 from a mis-joined URL looks exactly
        like a wrong key from the console.
        """
        base = (base_url or "").rstrip("/")
        if base.endswith("/messages"):
            return base
        if base.endswith("/v1"):
            return f"{base}/messages"
        return f"{base}/v1/messages"

    async def create(self, body: dict) -> dict:
        resp = await self._client.post(self._url, json=body)
        if resp.status_code >= 400:
            raise AnthropicError(resp.status_code, _error_text(resp.status_code, resp.text))
        return resp.json()

    async def stream(self, body: dict) -> AsyncIterator[tuple[str, dict]]:
        """Yield (event_type, payload) for each SSE event.

        The event name is taken from the payload's own `type` field rather than the SSE
        `event:` line: both are always present in this API and the payload is the one that
        survives a proxy that rewrites event names.
        """
        async with self._client.stream(
            "POST", self._url, json={**body, "stream": True}
        ) as resp:
            if resp.status_code >= 400:
                raw = await resp.aread()
                raise AnthropicError(
                    resp.status_code,
                    _error_text(resp.status_code, raw.decode("utf-8", "replace")),
                )
            async for line in resp.aiter_lines():
                line = line.rstrip("\r")
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if not payload or payload == "[DONE]":
                    continue
                try:
                    data = json.loads(payload)
                except ValueError:
                    continue
                event = str(data.get("type") or "")
                if event:
                    yield event, data

    async def close(self) -> None:
        """Named close() rather than aclose(), so ClientPool can close every client it holds
        through one call regardless of which kind it is."""
        await self._client.aclose()


def _error_text(status: int, body: str) -> str:
    """Pull the upstream's message out of its error envelope, falling back to the raw body."""
    try:
        data = json.loads(body)
    except ValueError:
        return f"{status}: {body[:400]}"
    err = data.get("error") if isinstance(data, dict) else None
    if isinstance(err, dict) and err.get("message"):
        return f"{status}: {err['message']}"
    if isinstance(data, dict) and data.get("message"):
        return f"{status}: {data['message']}"
    return f"{status}: {body[:400]}"
