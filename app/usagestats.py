"""Precomputed usage statistics.

Answering a Usage page request by reading and parsing every trace file is O(traces): at a few
thousand records that is seconds of blocking work per page view, repeated for every range
button and every drill-down. The records are immutable once written, so the scan is done once
in the background and the page reads the result.

What is stored is a per (date, user, model) bucket rather than a finished report. A finished
report would have to be precomputed for the cross product of every range and every user; the
buckets answer all of those by summing a few thousand small rows in memory.

Latency is the one figure that does not survive plain summing. Each bucket therefore carries a
histogram, and each histogram entry keeps both a count and the sum of the values that landed in
it -- so a percentile is reported as the mean of the bucket it falls in rather than the bucket's
floor. In the sparse tail, where the buckets are widest and a P95 actually lands, that entry
usually holds a single sample and the figure comes back exact.
"""
import asyncio
import logging
import os
import time
from typing import Any

from .authstore import mtime, read_json, write_json
from .config import DATA_DIR

logger = logging.getLogger("mr")

ROLLUP_PATH = DATA_DIR / "usage_rollup.json"
# Separate from the rollup: it has to be writable *while* the rollup is being computed, and it
# is what makes "a build is running" visible to the other workers sharing data/.
BUILD_PATH = DATA_DIR / "usage_build.json"

ROLLUP_VERSION = 1

DEFAULT_INTERVAL = 3600.0
# The longest range the console offers, so every button is answered from one file.
DEFAULT_DAYS = 90

# A build that claimed the lease this long ago is assumed dead -- a worker killed mid-scan
# must not leave the button greyed out forever.
_BUILD_LEASE_TTL = 1800.0


def _latency_bucket(ms: float) -> int:
    """Round a latency into a histogram bucket."""
    if ms < 1000:
        return int(ms // 10) * 10
    if ms < 10_000:
        return int(ms // 100) * 100
    if ms < 100_000:
        return int(ms // 1000) * 1000
    return int(ms // 10_000) * 10_000


def _empty_bucket() -> dict:
    return {
        "requests": 0, "errors": 0,
        "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
        # {bucket floor as a string: [count, sum of the real values]}
        "latency": {},
    }


def build(store, days: int = DEFAULT_DAYS) -> dict:
    """Scan the trace files and fold them into buckets. Blocking; call it in a thread."""
    started = time.time()
    buckets: dict[tuple[str, str, str], dict] = {}
    records = store.scan(days=days)
    for r in records:
        date = (r.get("ts") or "")[:10]
        if not date:
            continue
        user = r.get("user_id") or "anonymous"
        model = r.get("model") or "unknown"
        b = buckets.setdefault((date, user, model), _empty_bucket())
        u = r.get("usage") or {}
        b["requests"] += 1
        b["errors"] += int(r.get("status") == "error")
        for f in ("prompt_tokens", "completion_tokens", "total_tokens"):
            b[f] += u.get(f) or 0
        total_ms = r.get("total_ms")
        if total_ms is not None:
            key = str(_latency_bucket(float(total_ms)))
            entry = b["latency"].setdefault(key, [0, 0.0])
            entry[0] += 1
            entry[1] += float(total_ms)

    return {
        "version": ROLLUP_VERSION,
        "built_at": time.time(),
        "build_ms": round((time.time() - started) * 1000, 1),
        "days_scanned": days,
        "traces": len(records),
        "buckets": [
            {"date": d, "user": us, "model": m, **b}
            for (d, us, m), b in sorted(buckets.items())
        ],
    }


_cache: dict | None = None
_cache_mtime = 0.0


def load() -> dict:
    """The rollup as last written, re-read only when the file has actually changed."""
    global _cache, _cache_mtime
    stamp = mtime(ROLLUP_PATH)
    if _cache is None or stamp != _cache_mtime:
        data = read_json(ROLLUP_PATH, {})
        # A rollup written by an older layout is discarded rather than misread: it would be
        # summed into totals that quietly mean something else.
        _cache = data if data.get("version") == ROLLUP_VERSION else {}
        _cache_mtime = stamp
    return _cache


def built_at() -> float:
    return float(load().get("built_at") or 0)


# -- Build state, shared across workers ---------------------------------------
def status() -> dict:
    """Whether a build is running, and when the last one finished."""
    state = read_json(BUILD_PATH, {})
    building = bool(
        state.get("state") == "building"
        and time.time() - float(state.get("started_at") or 0) < _BUILD_LEASE_TTL
    )
    return {
        "building": building,
        "built_at": built_at() or None,
        "build_ms": load().get("build_ms"),
        "traces": load().get("traces"),
        "error": state.get("error") or None,
    }


def _claim() -> bool:
    """Best-effort single-flight lease, same reasoning as ghcache's: data/ is shared between
    workers, writes are atomic, and a duplicate scan is wasteful rather than corrupting."""
    state = read_json(BUILD_PATH, {})
    if (
        state.get("state") == "building"
        and time.time() - float(state.get("started_at") or 0) < _BUILD_LEASE_TTL
    ):
        return False
    write_json(BUILD_PATH, {
        "state": "building", "started_at": time.time(), "owner_pid": os.getpid(), "error": None,
    })
    return int(read_json(BUILD_PATH, {}).get("owner_pid") or 0) == os.getpid()


def _release(error: str | None = None) -> None:
    write_json(BUILD_PATH, {
        "state": "idle", "started_at": 0, "finished_at": time.time(),
        "owner_pid": os.getpid(), "error": error,
    })


async def refresh(store, days: int = DEFAULT_DAYS) -> bool:
    """Rebuild the rollup unless one is already running. Returns whether this call did it."""
    if not _claim():
        return False
    try:
        data = await asyncio.to_thread(build, store, days)
        write_json(ROLLUP_PATH, data)
        logger.info(
            "usage rollup rebuilt traces=%s buckets=%s in %.0fms",
            data["traces"], len(data["buckets"]), data["build_ms"],
        )
        _release()
        return True
    except Exception as e:  # noqa: BLE001 the caller is a loop or a button, neither should die
        logger.warning("usage rollup failed: %s", e)
        _release(str(e))
        raise


# -- Serving a query ----------------------------------------------------------
def _percentile(hist: dict[str, list], count: int) -> float | None:
    """The value at the rank the old full sort took, read off the merged histogram.

    Reports the mean of the entry the rank falls in, not its floor: the entry is one sample
    wide almost everywhere in the tail, which is where a P95 lands.
    """
    if not count:
        return None
    target = max(1, int(count * 0.95))
    seen = 0
    for value in sorted(hist, key=int):
        n, total = hist[value]
        seen += n
        if seen >= target:
            return round(total / n, 1)
    return None


def report(days: int, scope: str | None, is_admin: bool) -> dict[str, Any]:
    """Aggregate the buckets into the shape the console reads.

    `scope` narrows every rollup except `by_user`, which stays the full roster: it is what the
    console's scope picker is built from, so narrowing it would remove every other name from
    the list as soon as one was picked.
    """
    rollup = load()
    buckets = rollup.get("buckets") or []

    # The most recent `days` dates that actually have records, matching what scanning the N
    # newest date directories used to select -- a gap-free calendar window would silently
    # shorten the range on a service that was idle over a weekend.
    dates = sorted({b["date"] for b in buckets}, reverse=True)[: max(days, 1)]
    in_range = set(dates)

    by_model: dict[str, int] = {}
    by_day: dict[str, dict] = {}
    by_user: dict[str, dict] = {}
    totals = {"requests": 0, "errors": 0, "prompt_tokens": 0,
              "completion_tokens": 0, "total_tokens": 0}
    latency: dict[str, list] = {}
    latency_count = 0
    latency_sum = 0.0

    for b in buckets:
        if b["date"] not in in_range:
            continue
        owner = b["user"]
        u = by_user.setdefault(owner, {"requests": 0, "total_tokens": 0})
        u["requests"] += b["requests"]
        u["total_tokens"] += b["total_tokens"]
        if scope and owner != scope:
            continue
        totals["requests"] += b["requests"]
        totals["errors"] += b["errors"]
        for f in ("prompt_tokens", "completion_tokens", "total_tokens"):
            totals[f] += b[f]
        by_model[b["model"]] = by_model.get(b["model"], 0) + b["requests"]
        day = by_day.setdefault(b["date"], {"requests": 0, "total_tokens": 0, "errors": 0})
        day["requests"] += b["requests"]
        day["total_tokens"] += b["total_tokens"]
        day["errors"] += b["errors"]
        for value, (n, total) in (b.get("latency") or {}).items():
            entry = latency.setdefault(value, [0, 0.0])
            entry[0] += n
            entry[1] += total
            latency_count += n
            latency_sum += total

    return {
        "scope": scope or "all",
        "is_admin": is_admin,
        "days": days,
        "totals": {
            **totals,
            "avg_ms": round(latency_sum / latency_count, 1) if latency_count else None,
            "p95_ms": _percentile(latency, latency_count),
        },
        # Ties broken by name, so the chart's row order does not drift between rebuilds.
        "by_model": [
            {"model": m, "requests": c}
            for m, c in sorted(by_model.items(), key=lambda kv: (-kv[1], kv[0]))
        ],
        "by_day": [{"date": d, **v} for d, v in sorted(by_day.items())],
        "by_user": sorted(
            [{"user_id": k, **v} for k, v in by_user.items()],
            key=lambda x: (-x["requests"], x["user_id"]),
        ),
        # The page states how old the numbers are, because "precomputed" and "live" answer
        # different questions and a stat card cannot be read without knowing which it is.
        **status(),
    }
