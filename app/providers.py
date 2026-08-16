"""Pool of OpenAI clients, reused per provider.

Each model can bind its own provider (endpoint + key + protocol type), so the client
can no longer be a singleton. Clients are cached by
(base_url, api_key, api_type, api_version, kind); after a hot configuration reload,
invalidate() closes the old connections.
"""
import asyncio
import logging

from openai import AsyncAzureOpenAI, AsyncOpenAI

from .config import Provider

logger = logging.getLogger("fmr")

_TIMEOUT = 180.0
_MAX_RETRIES = 1


class ClientPool:
    def __init__(self) -> None:
        self._clients: dict[tuple, AsyncOpenAI | AsyncAzureOpenAI] = {}
        self._lock = asyncio.Lock()

    def _build(self, provider: Provider, kind: str) -> AsyncOpenAI | AsyncAzureOpenAI:
        if not provider.base_url:
            raise ValueError(
                f"provider {provider.name!r} has no base_url; set it on the "
                "\"Backend connections\" page"
            )
        base = provider.base_url.rstrip("/")
        if provider.api_type == "azure":
            if kind == "responses":
                # Some models (e.g. o3-pro) only support the Responses API, which lives
                # on the Azure v1 endpoint
                return AsyncOpenAI(
                    base_url=f"{base}/openai/v1/",
                    api_key=provider.api_key,
                    default_headers={"api-key": provider.api_key},
                    timeout=_TIMEOUT,
                    max_retries=_MAX_RETRIES,
                )
            return AsyncAzureOpenAI(
                azure_endpoint=base,
                api_key=provider.api_key,
                api_version=provider.api_version,
                timeout=_TIMEOUT,
                max_retries=_MAX_RETRIES,
            )
        # Any OpenAI-compatible service
        return AsyncOpenAI(
            base_url=base,
            api_key=provider.api_key,
            timeout=_TIMEOUT,
            max_retries=_MAX_RETRIES,
        )

    async def get(self, provider: Provider, kind: str = "chat"):
        """Get (or create) the client for this provider. kind is one of {chat, responses}."""
        key = (*provider.cache_key, kind)
        client = self._clients.get(key)
        if client is not None:
            return client
        async with self._lock:
            client = self._clients.get(key)
            if client is None:
                client = self._build(provider, kind)
                self._clients[key] = client
                logger.info(
                    "client created provider=%s type=%s kind=%s base=%s",
                    provider.name, provider.api_type, kind, provider.base_url,
                )
            return client

    async def invalidate(self) -> None:
        """Close and drop every cached client after a configuration change."""
        async with self._lock:
            clients = list(self._clients.values())
            self._clients.clear()
        for client in clients:
            try:
                await client.close()
            except Exception:  # noqa: BLE001 a failed close must not block the new config
                pass

    async def aclose(self) -> None:
        await self.invalidate()
