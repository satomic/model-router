"""On-disk cache of the GitHub enterprise / organization / team structure and its member
lists, plus cache-first membership answers.

Why this exists: every access decision used to be a live GitHub round trip
(app/keypolicy.py probes `/orgs/{org}/members/{login}` per user) behind ghadmin's in-process
TTL cache -- which is keyed on time.monotonic() and therefore dies with the worker. Restart
the service and every first request pays GitHub's latency again, and a busy console burns
rate limit on questions whose answer changes maybe once a week.

So the structure and the member lists are persisted under data/github/ and refreshed on a
timer, and membership becomes a set lookup:

  data/github/structure.json  the discover() payload: enterprises, their orgs and teams
  data/github/members.json    one entry per scope: {"org:acme": {logins, truncated, error}}
  data/github/probe.json      individual live-probe results, positive and negative
  data/github/refresh.lock    a best-effort lease, so N workers do not all refresh at once

Two rules govern trust, and both matter more than the speed-up:

  * A member list is authoritative only when it is fresh, complete and error-free. A
    truncated or errored list is never authoritative -- "not in the first 5000 logins I could
    read" is not "not a member", and reading it as one would deny legitimate users. Those
    cases fall through to a live probe, i.e. exactly today's behaviour.
  * Timestamps are wall-clock time.time(), never time.monotonic(). Monotonic values are
    meaningless once written to disk and read back after a restart -- they would make every
    cache entry look impossibly old or impossibly fresh depending on the host's uptime.

The token is never stored. `token_fp` is sha256(token)[:12], which is enough to notice that
the token changed (and therefore that the cached answers may reflect a different visibility)
without keeping the secret in a second place.
"""
import asyncio
import hashlib
import logging
import os
import threading
import time
from pathlib import Path

from . import config, ghadmin
from .authstore import mtime, read_json, write_json

logger = logging.getLogger("mr")

CACHE_DIR = config.DATA_DIR / "github"
STRUCTURE_PATH = CACHE_DIR / "structure.json"
MEMBERS_PATH = CACHE_DIR / "members.json"
PROBE_PATH = CACHE_DIR / "probe.json"
LOCK_PATH = CACHE_DIR / "refresh.lock"

# How long a refreshed member list stays authoritative. Beyond this the entry is still
# shown in the admin UI (with its age) but no longer answers membership on its own.
DEFAULT_REFRESH_SECONDS = 3600
_MEMBERS_TTL_SLACK = 2.0  # A list counts as fresh for slack x the refresh interval, so a
                          # single skipped refresh does not fall back to live probing.

# Live-probe results are cached with asymmetric TTLs. Positives last longer because losing
# access is rarer than gaining it; negatives expire quickly so a user who was just added to
# an org gets in without waiting for the next full refresh.
_PROBE_TTL = 600.0
_NEG_TTL = 120.0

# Upper bound on how many member lists one refresh will fetch. Under allow_all_orgs an
# enterprise can reference thousands of orgs; fetching every member list would be a far
# bigger GitHub bill than the per-user probing this replaces.
_MAX_MEMBER_SCOPES = 60
# Concurrency for those fetches: enough to keep the refresh short, low enough to stay clear
# of GitHub's secondary rate limit.
_FETCH_CONCURRENCY = 4

# Lease lifetime. Long enough for a slow refresh to finish, short enough that a worker
# killed mid-refresh does not block the next one for long.
_LEASE_TTL = 600.0

# Sources reported alongside every answer, so an admin can see which layer decided.
SOURCE_CACHE = "cache"  # answered from a complete member list: zero GitHub calls
SOURCE_PROBE = "probe"  # answered from a cached individual probe: zero GitHub calls
SOURCE_LIVE = "live"    # a real GitHub call was made

# Guards the read-modify-write of probe.json inside one process. Cross-process safety comes
# from the atomic replace in write_json plus the mtime re-read below: a lost probe entry only
# costs one extra GitHub call, so a real lock is not warranted.
_probe_lock = threading.Lock()
_probe_cache: dict | None = None
_probe_mtime = 0.0


def token_fp(token: str) -> str:
    """A short fingerprint of the admin token. Never the token itself."""
    if not token:
        return ""
    return hashlib.sha256(token.encode("utf-8")).hexdigest()[:12]


def _now() -> float:
    return time.time()


def _org_key(org: str) -> str:
    return f"org:{(org or '').strip().lower()}"


def _team_key(slug: str, team_id) -> str:
    return f"team:{(slug or '').strip().lower()}/{team_id}"


def refresh_seconds(cfg) -> float:
    """Refresh interval, from auth.key_policy.cache_refresh_seconds.

    Clamped to a floor: a misconfigured 1 would turn the background loop into a GitHub
    hammer, which is the opposite of what this module is for.
    """
    raw = (cfg.key_policy or {}).get("cache_refresh_seconds")
    try:
        value = float(raw)
    except (TypeError, ValueError):
        return float(DEFAULT_REFRESH_SECONDS)
    return max(60.0, value)


# -- Raw file access ----------------------------------------------------------
def _load_members() -> dict:
    return read_json(MEMBERS_PATH, {"fetched_at": 0, "token_fp": "", "entries": {}})


def _load_structure() -> dict:
    return read_json(
        STRUCTURE_PATH, {"fetched_at": 0, "token_fp": "", "enterprises": []}
    )


def _load_probes() -> dict:
    """Read probe.json through a process-local cache, re-reading only when the file
    changed -- the same mtime-guarded pattern AuthStore uses for sessions and keys."""
    global _probe_cache, _probe_mtime
    current = mtime(PROBE_PATH)
    if _probe_cache is None or current != _probe_mtime:
        _probe_cache = read_json(PROBE_PATH, {"entries": {}})
        _probe_mtime = current
    return _probe_cache


def _record_probe(key: str, member: bool) -> None:
    global _probe_cache, _probe_mtime
    with _probe_lock:
        data = read_json(PROBE_PATH, {"entries": {}})
        entries = data.setdefault("entries", {})
        entries[key] = {"member": bool(member), "at": _now()}
        # Bound the file: probe entries are one per (scope, login) pair and would otherwise
        # grow without limit on a deployment with many users.
        if len(entries) > 5000:
            for stale in sorted(entries, key=lambda k: entries[k].get("at") or 0)[:1000]:
                del entries[stale]
        write_json(PROBE_PATH, data)
        _probe_cache = data
        _probe_mtime = mtime(PROBE_PATH)


def invalidate() -> None:
    """Drop every cached answer. Called when the token or the policy changes: the same
    question can have a different answer under a different token, so keeping the old files
    would leave stale member lists authoritative."""
    global _probe_cache, _probe_mtime
    for path in (STRUCTURE_PATH, MEMBERS_PATH, PROBE_PATH):
        try:
            path.unlink()
        except FileNotFoundError:
            pass
        except OSError as e:  # noqa: PERF203 one message per file is what we want here
            logger.warning("could not remove cache file %s: %s", path, e)
    with _probe_lock:
        _probe_cache = None
        _probe_mtime = 0.0


# -- Membership: cache first, live fallback ------------------------------------
def _list_answers(cfg, key: str, login: str) -> bool | None:
    """Answer from a member list, or None when that list may not be trusted.

    Untrusted means: absent, fetched under a different token, stale, truncated, or errored.
    Each of those is a case where "login not in logins" would be a guess, and a wrong guess
    here denies a legitimate user their API key.
    """
    data = _load_members()
    if data.get("token_fp") != token_fp(cfg.gh_admin_token):
        return None
    entry = (data.get("entries") or {}).get(key)
    if not isinstance(entry, dict):
        return None
    if entry.get("truncated") or entry.get("error"):
        return None
    age = _now() - float(entry.get("fetched_at") or 0)
    if age > refresh_seconds(cfg) * _MEMBERS_TTL_SLACK:
        return None
    logins = entry.get("logins")
    if not isinstance(logins, list):
        return None
    return (login or "").strip().lower() in logins


def _probe_answers(key: str, login: str) -> bool | None:
    """Answer from a cached individual probe, honouring the polarity-dependent TTL."""
    entry = (_load_probes().get("entries") or {}).get(f"{key}:{(login or '').lower()}")
    if not isinstance(entry, dict):
        return None
    member = bool(entry.get("member"))
    age = _now() - float(entry.get("at") or 0)
    if age > (_PROBE_TTL if member else _NEG_TTL):
        return None
    return member


async def is_org_member(cfg, org: str, login: str) -> tuple[bool, str]:
    """(is a member, source). Cache first; a miss costs exactly one GitHub call."""
    token = cfg.gh_admin_token
    key = _org_key(org)
    answer = _list_answers(cfg, key, login)
    if answer is not None:
        return answer, SOURCE_CACHE
    answer = _probe_answers(key, login)
    if answer is not None:
        return answer, SOURCE_PROBE
    if not token:
        # keypolicy refuses before reaching here, but a tokenless call must still be
        # fail-closed rather than raising into an authorization decision.
        return False, SOURCE_LIVE
    result = await ghadmin.check_org_member(token, org, login)
    _record_probe(f"{key}:{(login or '').lower()}", result)
    return result, SOURCE_LIVE


async def is_team_member(cfg, slug: str, team_id, login: str) -> tuple[bool, str]:
    """(is a member, source) for an enterprise team. Same rules as is_org_member."""
    token = cfg.gh_admin_token
    key = _team_key(slug, team_id)
    answer = _list_answers(cfg, key, login)
    if answer is not None:
        return answer, SOURCE_CACHE
    answer = _probe_answers(key, login)
    if answer is not None:
        return answer, SOURCE_PROBE
    if not token:
        return False, SOURCE_LIVE
    result = await ghadmin.check_enterprise_team_member(token, slug, team_id, login)
    _record_probe(f"{key}:{(login or '').lower()}", result)
    return result, SOURCE_LIVE


# -- Structure, for the configuration page -------------------------------------
def cached_structure(cfg) -> dict | None:
    """The stored discover() payload, or None when there is nothing usable.

    Age is deliberately *not* a disqualifier here: this feeds a configuration page that
    reports the fetch time next to the data, and showing a day-old org list beats making
    the admin wait on GitHub for every page load. Membership decisions apply the age check
    (see _list_answers); displaying a structure does not need to.
    """
    data = _load_structure()
    if data.get("token_fp") != token_fp(cfg.gh_admin_token):
        return None
    enterprises = data.get("enterprises")
    if not isinstance(enterprises, list) or not enterprises:
        return None
    return {
        "enterprises": enterprises,
        "cached": True,
        "fetched_at": data.get("fetched_at") or 0,
    }


def store_structure(cfg, enterprises: list) -> None:
    """Persist a structure that was just fetched live, so the next read is free.

    Member lists are deliberately left alone: they belong to refresh(), which is the only
    thing that knows whether each one came back complete.
    """
    if not enterprises:
        return
    write_json(STRUCTURE_PATH, {
        "fetched_at": _now(),
        "token_fp": token_fp(cfg.gh_admin_token),
        "enterprises": enterprises,
        "error": "",
    })


def status(cfg=None) -> dict:
    """Everything the admin UI needs to judge the cache: ages, counts, truncation, errors.

    Never returns logins -- an org's member list is not something the console needs to
    render, and a cache status panel is the wrong place to publish one.
    """
    structure = _load_structure()
    members = _load_members()
    probes = _load_probes()
    now = _now()
    fp = token_fp(cfg.gh_admin_token) if cfg is not None else None

    scopes = []
    for key, entry in sorted((members.get("entries") or {}).items()):
        entry = entry if isinstance(entry, dict) else {}
        kind, _, name = key.partition(":")
        scopes.append({
            "key": key,
            "kind": kind,
            "name": name,
            "count": len(entry.get("logins") or []),
            "truncated": bool(entry.get("truncated")),
            "error": entry.get("error") or "",
            "fetched_at": entry.get("fetched_at") or 0,
        })

    ents = [
        {
            "slug": e.get("slug"),
            "name": e.get("name") or e.get("slug"),
            "organizations": len(e.get("organizations") or []),
            "organizations_truncated": bool(e.get("organizations_truncated")),
            "teams": len(e.get("teams") or []),
            "error": e.get("organizations_error") or e.get("teams_error") or "",
        }
        for e in (structure.get("enterprises") or [])
        if isinstance(e, dict)
    ]

    structure_at = float(structure.get("fetched_at") or 0)
    members_at = float(members.get("fetched_at") or 0)
    interval = refresh_seconds(cfg) if cfg is not None else float(DEFAULT_REFRESH_SECONDS)
    # Staleness is judged per scope as well as on the document, because _list_answers ages
    # each entry against its own fetched_at: an entry that has stopped answering membership
    # must not sit behind a panel that calls the cache fresh because *some* refresh ran
    # recently. The oldest scope decides.
    oldest_scope = min((s["fetched_at"] for s in scopes), default=members_at)
    stale = (
        (not members_at)
        or (now - members_at) > interval * _MEMBERS_TTL_SLACK
        or (bool(scopes) and (now - float(oldest_scope or 0)) > interval * _MEMBERS_TTL_SLACK)
    )
    return {
        "token_configured": bool(cfg.gh_admin_token) if cfg is not None else None,
        # A mismatch means the token was replaced since the last refresh, so nothing cached
        # is being trusted -- worth saying out loud rather than showing a healthy-looking age.
        "token_matches": None if fp is None else (
            bool(structure_at or members_at) and fp == (
                structure.get("token_fp") or members.get("token_fp")
            )
        ),
        "refresh_seconds": interval,
        "structure": {
            "fetched_at": structure_at,
            "age_seconds": (now - structure_at) if structure_at else None,
            "enterprises": ents,
            "error": structure.get("error") or "",
        },
        "members": {
            "fetched_at": members_at,
            "age_seconds": (now - members_at) if members_at else None,
            "scopes": scopes,
            "truncated_scopes": sum(1 for s in scopes if s["truncated"]),
            "errored_scopes": sum(1 for s in scopes if s["error"]),
        },
        "probes": {"count": len(probes.get("entries") or {})},
        "stale": stale,
    }


# -- Refresh ------------------------------------------------------------------
def _scopes_to_fetch(cfg, structure: dict) -> list[tuple[str, str, object]]:
    """Which member lists this policy actually needs: (cache key, kind, target).

    Only scopes the policy references are fetched. Caching member lists the policy never
    consults would be pure cost -- and under allow_all_orgs the enumerated orgs are what
    keypolicy probes as a fallback, so those are worth having locally too.
    """
    by_slug = {
        str(e.get("slug")): e
        for e in (structure.get("enterprises") or [])
        if isinstance(e, dict) and e.get("slug")
    }
    out: list[tuple[str, str, object]] = []
    seen: set[str] = set()

    def add(key: str, kind: str, target) -> None:
        if key not in seen:
            seen.add(key)
            out.append((key, kind, target))

    for slug, rule in ((cfg.key_policy or {}).get("enterprises") or {}).items():
        rule = rule or {}
        if not rule.get("enabled"):
            continue
        for org in rule.get("organizations") or []:
            org = str(org).strip()
            if org:
                add(_org_key(org), "org", org)
        for team in rule.get("teams") or []:
            team = str(team).strip()
            if team:
                add(_team_key(slug, team), "team", (slug, team))
        if rule.get("allow_all_orgs"):
            for org in (by_slug.get(str(slug)) or {}).get("organizations") or []:
                login = str((org or {}).get("login") or "").strip()
                if login:
                    add(_org_key(login), "org", login)
    return out[:_MAX_MEMBER_SCOPES]


async def refresh(cfg) -> dict:
    """Refresh structure.json and every member list the policy references.

    Returns status(). A per-scope failure is recorded in that scope's entry rather than
    aborting the refresh: one unreadable org must not cost the cache every other one.
    """
    token = cfg.gh_admin_token
    if not token:
        return {**status(cfg), "error": "no GitHub token is configured"}

    fp = token_fp(token)
    now = _now()
    structure_error = ""
    try:
        discovered = await ghadmin.discover(token)
        enterprises = discovered.get("enterprises") or []
    except ghadmin.GitHubAdminError as e:
        # Keep whatever structure we already have: a transient GitHub failure should not
        # empty the configuration page.
        structure_error = str(e)
        enterprises = _load_structure().get("enterprises") or []

    structure_doc = {
        "fetched_at": now if not structure_error else (
            _load_structure().get("fetched_at") or 0
        ),
        "token_fp": fp,
        "enterprises": enterprises,
        "error": structure_error,
    }
    write_json(STRUCTURE_PATH, structure_doc)

    scopes = _scopes_to_fetch(cfg, structure_doc)
    semaphore = asyncio.Semaphore(_FETCH_CONCURRENCY)

    async def fetch(kind: str, target) -> dict:
        async with semaphore:
            try:
                if kind == "org":
                    return await ghadmin.list_org_members(token, str(target))
                slug, team_id = target  # type: ignore[misc]
                return await ghadmin.list_enterprise_team_members(token, slug, team_id)
            except Exception as e:  # noqa: BLE001 a scope failure is data, not a crash
                logger.warning("member list fetch failed kind=%s target=%s: %s",
                               kind, target, e)
                return {"logins": [], "truncated": True, "error": str(e)}

    results = await asyncio.gather(*[fetch(kind, target) for _, kind, target in scopes])

    entries: dict[str, dict] = {}
    for (key, kind, _target), result in zip(scopes, results):
        entries[key] = {
            "kind": kind,
            "logins": result.get("logins") or [],
            "truncated": bool(result.get("truncated")),
            "error": result.get("error") or "",
            "fetched_at": _now(),
        }
    write_json(MEMBERS_PATH, {"fetched_at": _now(), "token_fp": fp, "entries": entries})

    # Individual probes are now redundant for every scope that got a complete list, and
    # keeping them would let a stale negative outlive the list that contradicts it.
    _drop_probes_for({
        key for key, entry in entries.items()
        if not entry["truncated"] and not entry["error"]
    })

    logger.info(
        "GitHub cache refreshed enterprises=%d scopes=%d truncated=%d errored=%d",
        len(enterprises), len(entries),
        sum(1 for e in entries.values() if e["truncated"]),
        sum(1 for e in entries.values() if e["error"]),
    )
    return status(cfg)


def _drop_probes_for(keys: set[str]) -> None:
    global _probe_cache, _probe_mtime
    if not keys:
        return
    with _probe_lock:
        data = read_json(PROBE_PATH, {"entries": {}})
        entries = data.get("entries") or {}
        remaining = {
            k: v for k, v in entries.items()
            if k.rsplit(":", 1)[0] not in keys
        }
        if len(remaining) != len(entries):
            data["entries"] = remaining
            write_json(PROBE_PATH, data)
            _probe_cache = data
            _probe_mtime = mtime(PROBE_PATH)


# -- The refresh lease --------------------------------------------------------
def acquire_lease() -> bool:
    """Best-effort "I am the worker that refreshes" lease.

    data/ is explicitly shared between workers, so without this every worker would refresh
    on its own timer. It is deliberately not a real lock: writes are atomic, so a duplicate
    refresh is merely wasteful rather than corrupting, and the cost of getting distributed
    locking wrong is far higher than the cost of an occasional extra refresh.
    """
    lease = read_json(LOCK_PATH, {})
    expires = float(lease.get("expires_at") or 0)
    if expires > _now() and lease.get("owner_pid") != os.getpid():
        return False
    write_json(LOCK_PATH, {"owner_pid": os.getpid(), "expires_at": _now() + _LEASE_TTL})
    # Re-read: if two workers wrote at once, only the one whose value survived the last
    # atomic replace continues.
    return int((read_json(LOCK_PATH, {}).get("owner_pid") or 0)) == os.getpid()


def release_lease() -> None:
    lease = read_json(LOCK_PATH, {})
    if lease.get("owner_pid") == os.getpid():
        write_json(LOCK_PATH, {"owner_pid": None, "expires_at": 0})
