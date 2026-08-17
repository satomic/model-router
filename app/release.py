"""Background check for a newer published release.

The result is cached in memory and on disk under data/, so a restart does not re-poll GitHub and
several workers do not each ask. The check is deliberately unauthenticated: the releases endpoint
is public, and requiring a token would make the feature unavailable exactly where it is most
useful, on a fresh deployment nobody has configured yet.

Failure is not an error state worth surfacing loudly. A router that cannot reach github.com still
routes, so a failed check keeps the last known answer and records why, and the console simply
shows no banner.
"""
import asyncio
import logging
import time

import httpx

from .authstore import read_json, write_json
from .config import DATA_DIR
from .version import RELEASES_API, VERSION, is_newer

logger = logging.getLogger("mr.release")

_PATH = DATA_DIR / "release.json"
# One day. A router is not a package manager: checking more often would add nothing an operator
# would notice and would spend somebody's rate limit doing it.
CHECK_SECONDS = 86400.0
# The first check waits this long. Startup must never depend on github.com being reachable.
WARMUP_SECONDS = 30.0
# After a failure, retry sooner than the full interval but not so soon that an outage becomes a
# tight loop.
RETRY_SECONDS = 3600.0

_state: dict = {}


def _load() -> dict:
    global _state
    if not _state:
        _state = read_json(_PATH, {}) or {}
    return _state


def status() -> dict:
    """What the console renders. Always answers, even before the first check has run."""
    st = _load()
    latest = st.get("latest_version") or ""
    return {
        "current_version": VERSION,
        "latest_version": latest or None,
        # Recomputed here rather than read from disk: the stored answer was true for whatever
        # version was running when it was written, and an upgrade must not keep showing the
        # banner for the release it just installed.
        "update_available": bool(latest) and is_newer(latest, VERSION),
        "release_url": st.get("release_url") or None,
        "published_at": st.get("published_at") or None,
        "checked_at": st.get("checked_at") or None,
        "error": st.get("error") or None,
    }


async def check() -> dict:
    """Ask GitHub once for the latest release and store the answer. Never raises."""
    global _state
    st = dict(_load())
    st["checked_at"] = time.time()
    try:
        async with httpx.AsyncClient(timeout=10.0, follow_redirects=True) as client:
            r = await client.get(
                RELEASES_API,
                headers={
                    "Accept": "application/vnd.github+json",
                    "X-GitHub-Api-Version": "2022-11-28",
                },
            )
        if r.status_code == 404:
            # A repository with no published release yet. Not a failure: there is simply no
            # newer version, and reporting an error here would look like a broken check.
            st.update(latest_version="", release_url=None, published_at=None, error=None)
        else:
            r.raise_for_status()
            body = r.json()
            tag = (body.get("tag_name") or body.get("name") or "").strip()
            st.update(
                latest_version=tag,
                release_url=body.get("html_url"),
                published_at=body.get("published_at"),
                error=None,
            )
            if tag and is_newer(tag, VERSION):
                logger.info("a newer release is available: %s (running %s)", tag, VERSION)
    except Exception as e:  # noqa: BLE001 a failed check must never propagate
        st["error"] = f"{type(e).__name__}: {e}"
        logger.info("release check failed: %s", st["error"])

    _state = st
    write_json(_PATH, st)
    return status()


async def loop() -> None:
    """Poll forever. Wrapped per iteration so no single failure ends the task."""
    await asyncio.sleep(WARMUP_SECONDS)
    while True:
        delay = CHECK_SECONDS
        try:
            result = await check()
            if result["error"]:
                delay = RETRY_SECONDS
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001 belt and braces: check() already swallows
            logger.warning("release check loop error: %s", e)
            delay = RETRY_SECONDS
        await asyncio.sleep(delay)
