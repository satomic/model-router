"""GitHub Copilot AI-credit pool monitoring, and the gate that keeps BYOK traffic off third-party
models while the pool still has credits in it.

Why this exists: every Copilot Business / Enterprise seat comes with a monthly allowance of AI
credits that is **pooled across the enterprise** and **forfeited at month end**. A user who
routes everything through this service on BYOK spends the customer's Azure/OpenAI bill while
the credits they already paid for expire unused. So this module polls GitHub on a cron
schedule, works out whether the pool is exhausted, and -- when the operator turns the gate on
-- answers a chat request with a short note instead of routing it, for as long as the pool has
credits left.

What GitHub actually offers (verified live against three enterprises, September 2026; the API
documents none of the pool arithmetic, so the derivation is written down here):

  GET /enterprises/{slug}/settings/billing/usage/summary?product=copilot
      Per-SKU month-to-date totals. The SKUs with unitType "ai-units" (`copilot_ai_unit`,
      `coding_agent_ai_unit`) are the pool: `discountQuantity` is what the pool covered,
      `netQuantity` is metered overage. discountQuantity is capped at the pool size, so
      **netQuantity > 0 means the pool is exhausted and sum(discountQuantity) is its exact
      size**. While netQuantity == 0 the size has to be estimated from seats.
  GET /enterprises/{slug}/copilot/billing/seats
      One entry per licensed user with `plan_type` (enterprise / business). Sum of the
      per-plan included amounts = pool size. **404s on very large enterprises**, in which case
      the size is unknown until the pool is exhausted (see above) -- but exhaustion itself is
      still detectable.
  GET /enterprises/{slug}/settings/billing/budgets  (+ /{id}/user-states)
      User-level budgets (sku ai_credits / premium_requests) are the only per-user limit
      GitHub enforces: a user over theirs is blocked on Copilot regardless of the pool, so it
      would be wrong to send them back to Copilot. Amounts are USD; 1 credit = $0.01.
  GET /enterprises/{slug}/settings/billing/ai_credit/usage?user=login
      Per-user consumption exists but costs one call per user and says nothing about the
      user's *limit*, so the budget endpoints are used for the per-user view instead.

The snapshot is persisted to data/ai_credits.json so every worker answers the gate from the
same file and a restart does not start blind. The token is never stored -- ghcache.token_fp
notices a token change so a snapshot fetched under another token's visibility is ignored.
"""
import asyncio
import logging
import os
import threading
import time
from datetime import datetime, timezone

import httpx

from . import config, cronexpr, ghadmin, ghcache
from .authstore import mtime, read_json, write_json
from .ghcache import token_fp

logger = logging.getLogger("mr")

SNAPSHOT_PATH = config.DATA_DIR / "ai_credits.json"
LOCK_PATH = config.DATA_DIR / "ai_credits.lock"

# Included AI credits per seat per month, from GitHub's published plan table. If GitHub changes
# these the seat-based estimate drifts, but the exact figure takes over the moment the pool is
# exhausted, so the gate itself stays correct.
INCLUDED_CREDITS = {"enterprise": 3900, "business": 1900}
USD_PER_CREDIT = 0.01

_ACCEPT = "application/vnd.github+json"
_API_VERSION = "2022-11-28"
_TIMEOUT = 30.0
_PAGE = 100
# Ceilings on paginated walks. 50 pages of seats is 5000 users; an enterprise beyond that has
# most likely 404'd the seats endpoint anyway.
_MAX_PAGES = 50
# The lease keeps N workers from polling GitHub at once; long enough for a slow enterprise.
_LEASE_TTL = 600.0
# A snapshot older than this many schedule intervals (and never less than 15 minutes) no longer
# blocks anyone: a gate that fails closed on stale data would lock users out through a GitHub
# outage, which is worse than a few requests slipping through to BYOK.
_STALE_INTERVALS = 2.0
_STALE_FLOOR = 900.0

POOL_AVAILABLE = "available"
POOL_EXHAUSTED = "exhausted"
POOL_UNKNOWN = "unknown"
POOL_NONE = "none"  # no Copilot seats at all

GATE_REASON = "ai-credits-gate"
# Why a request was gated: the enterprise pool still has credits, or (per-user mode) the
# caller's own user-level budget does. Each has its own note.
REASON_POOL = "pool"
REASON_BUDGET = "budget"

DEFAULT_MESSAGE = (
    "Your GitHub Copilot AI-credit pool ({enterprise}) still has credits left"
    "{remaining_note}. Please use Copilot's built-in models first; this BYOK "
    "route reopens automatically once the pool is used up."
)
DEFAULT_MESSAGE_BUDGET = (
    "Your GitHub Copilot user-level budget in {enterprise} still has "
    "${budget_remaining_usd} of ${budget_total_usd} left. Please use Copilot's built-in "
    "models first; this BYOK route reopens automatically once your budget is used up."
)

_snapshot_cache: dict | None = None
_snapshot_mtime = 0.0
_snapshot_lock = threading.Lock()


class AICreditsError(Exception):
    """A GitHub call failed and the administrator needs to see why."""


# -- Settings -----------------------------------------------------------------
def settings(cfg) -> dict:
    """The `ai_credits` section, normalised so every reader sees the same defaults."""
    raw = dict(getattr(cfg, "ai_credits", None) or {})
    gate = dict(raw.get("gate") or {})
    try:
        floor = max(0.0, float(raw.get("min_remaining_credits") or 0))
    except (TypeError, ValueError):
        floor = 0.0
    return {
        "enabled": bool(raw.get("enabled", False)),
        "schedule": str(raw.get("schedule") or cronexpr.DEFAULT_SCHEDULE).strip(),
        "enterprises": [str(s).strip().lower() for s in (raw.get("enterprises") or []) if str(s).strip()],
        "min_remaining_credits": floor,
        "gate_enabled": bool(gate.get("enabled", False)),
        "per_user": bool(gate.get("per_user", False)),
        "message": str(gate.get("message") or "").strip(),
        "message_budget": str(gate.get("message_budget") or "").strip(),
    }


def validate(raw) -> list[str]:
    """Configuration errors for the `ai_credits` section; empty when valid."""
    if raw is None:
        return []
    if not isinstance(raw, dict):
        return ["ai_credits must be an object"]
    errors = []
    if "enabled" in raw and not isinstance(raw["enabled"], bool):
        errors.append("ai_credits.enabled must be a boolean")
    schedule = raw.get("schedule")
    if schedule is not None:
        problem = cronexpr.validate(str(schedule))
        if problem:
            errors.append(f"ai_credits.schedule is not a valid cron expression: {problem}")
    ents = raw.get("enterprises")
    if ents is not None and not isinstance(ents, list):
        errors.append("ai_credits.enterprises must be a list of enterprise slugs")
    floor = raw.get("min_remaining_credits")
    if floor is not None:
        try:
            if float(floor) < 0:
                errors.append("ai_credits.min_remaining_credits must be >= 0")
        except (TypeError, ValueError):
            errors.append("ai_credits.min_remaining_credits must be a number")
    gate = raw.get("gate")
    if gate is not None:
        if not isinstance(gate, dict):
            errors.append("ai_credits.gate must be an object")
        else:
            for field in ("enabled", "per_user"):
                if field in gate and not isinstance(gate[field], bool):
                    errors.append(f"ai_credits.gate.{field} must be a boolean")
            if "message" in gate and gate["message"] is not None and not isinstance(gate["message"], str):
                errors.append("ai_credits.gate.message must be a string")
            if "message_budget" in gate and gate["message_budget"] is not None and not isinstance(gate["message_budget"], str):
                errors.append("ai_credits.gate.message_budget must be a string")
    return errors


# -- GitHub calls -------------------------------------------------------------
def _headers(token: str) -> dict:
    return {
        "Authorization": f"Bearer {token}",
        "Accept": _ACCEPT,
        "X-GitHub-Api-Version": _API_VERSION,
    }


async def _get(client: httpx.AsyncClient, token: str, path: str, params: dict | None = None):
    """Return (status, body). 401/403 raise -- they mean the token, not the enterprise."""
    resp = await client.get(f"{ghadmin.API}{path}", headers=_headers(token), params=params)
    if resp.status_code == 401:
        raise AICreditsError("token is invalid or expired (GitHub 401)")
    if resp.status_code == 403:
        raise AICreditsError(f"token lacks billing permission or hit a rate limit (GitHub 403): {path}")
    if resp.status_code >= 500:
        raise AICreditsError(f"GitHub {resp.status_code}: {path}")
    try:
        return resp.status_code, resp.json()
    except ValueError:
        return resp.status_code, None


def _ai_unit_items(summary: dict) -> list[dict]:
    return [
        item for item in (summary.get("usageItems") or [])
        if str(item.get("unitType") or "").lower() == "ai-units"
    ]


async def _fetch_seats(client, token, slug) -> tuple[dict | None, dict, str | None]:
    """(plan counts, {login: plan}, error). counts is None when the endpoint is unavailable."""
    counts: dict[str, int] = {}
    logins: dict[str, str] = {}
    page = 1
    total = None
    while page <= _MAX_PAGES:
        status, body = await _get(
            client, token, f"/enterprises/{slug}/copilot/billing/seats",
            {"per_page": _PAGE, "page": page},
        )
        if status == 404:
            return None, {}, None
        if status != 200 or not isinstance(body, dict):
            return None, {}, f"seats endpoint returned {status}"
        total = int(body.get("total_seats") or 0)
        seats = body.get("seats") or []
        for seat in seats:
            plan = str(seat.get("plan_type") or "").lower()
            counts[plan] = counts.get(plan, 0) + 1
            login = str(((seat.get("assignee") or {}).get("login")) or "").lower()
            if login:
                logins[login] = plan
        if not seats or len(logins) >= total:
            break
        page += 1
    if total is not None and len(logins) < total:
        return counts, logins, f"seat list truncated at {len(logins)} of {total}"
    return counts, logins, None


async def _fetch_budgets(client, token, slug) -> tuple[dict, float | None, str | None]:
    """Per-user budget state: ({login: {target_usd, consumed_usd}}, universal_usd, error).

    Only AI-credit budgets count; an Actions budget says nothing about Copilot. The user-states
    of a multi-user budget already reflect any individual override (GitHub reports the
    effective target), and an individual budget is then applied on top so a user who has not
    consumed anything this cycle -- and so is absent from user-states -- still gets their own
    limit rather than the universal one.
    """
    users: dict[str, dict] = {}
    universal: float | None = None
    multi: list[str] = []
    page = 1
    budgets: list[dict] = []
    while page <= _MAX_PAGES:
        status, body = await _get(
            client, token, f"/enterprises/{slug}/settings/billing/budgets",
            {"per_page": _PAGE, "page": page},
        )
        if status == 404:
            return {}, None, None
        if status != 200 or not isinstance(body, dict):
            return {}, None, f"budgets endpoint returned {status}"
        budgets.extend(body.get("budgets") or [])
        if not body.get("has_next_page"):
            break
        page += 1
    for b in budgets:
        if str(b.get("budget_product_sku") or "").lower() not in ("ai_credits", "premium_requests"):
            continue
        scope = str(b.get("budget_scope") or "")
        if scope == "multi_user_customer":
            universal = float(b.get("budget_amount") or 0)
            multi.append(str(b.get("id")))
        elif scope == "multi_user_cost_center":
            multi.append(str(b.get("id")))
    for budget_id in multi:
        page = 1
        while page <= _MAX_PAGES:
            status, body = await _get(
                client, token,
                f"/enterprises/{slug}/settings/billing/budgets/{budget_id}/user-states",
                {"per_page": _PAGE, "page": page},
            )
            if status != 200 or not isinstance(body, dict):
                break
            for st in body.get("user_states") or []:
                login = str(st.get("user") or "").lower()
                if not login:
                    continue
                users[login] = {
                    "target_usd": float(st.get("target_amount") or 0),
                    "consumed_usd": float(st.get("consumed_amount") or 0),
                }
            if not body.get("has_next_page"):
                break
            page += 1
    for b in budgets:
        if str(b.get("budget_scope") or "") != "user":
            continue
        if str(b.get("budget_product_sku") or "").lower() not in ("ai_credits", "premium_requests"):
            continue
        login = str(b.get("user") or b.get("budget_entity_name") or "").lower()
        if not login:
            continue
        users[login] = {
            "target_usd": float(b.get("budget_amount") or 0),
            "consumed_usd": float(b.get("consumed_amount") or 0),
        }
    return users, universal, None


async def fetch_enterprise(token: str, slug: str, name: str, per_user: bool) -> dict:
    """One enterprise's pool state. Never raises for a per-enterprise problem: the error is
    recorded on the entry so the other enterprises still get their data."""
    entry: dict = {
        "slug": slug, "name": name or slug,
        "pool_total": None, "pool_total_source": None,
        "consumed": 0.0, "metered": 0.0, "gross": 0.0,
        "remaining": None, "state": POOL_UNKNOWN,
        "seats": None, "seat_logins": None, "member_logins": {},
        "users": None, "universal_budget_usd": None,
        "skus": [], "warnings": [], "error": None,
    }
    try:
        async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
            status, summary = await _get(
                client, token, f"/enterprises/{slug}/settings/billing/usage/summary",
                {"product": "copilot"},
            )
            if status == 404:
                entry["error"] = "billing usage endpoint not available for this enterprise (GitHub 404)"
                return entry
            if status != 200 or not isinstance(summary, dict):
                entry["error"] = f"billing usage endpoint returned {status}"
                return entry
            items = _ai_unit_items(summary)
            entry["skus"] = [
                {
                    "sku": it.get("sku"),
                    "gross": float(it.get("grossQuantity") or 0),
                    "covered": float(it.get("discountQuantity") or 0),
                    "metered": float(it.get("netQuantity") or 0),
                }
                for it in items
            ]
            entry["consumed"] = sum(s["covered"] for s in entry["skus"])
            entry["metered"] = sum(s["metered"] for s in entry["skus"])
            entry["gross"] = sum(s["gross"] for s in entry["skus"])

            counts, logins, seat_err = await _fetch_seats(client, token, slug)
            if seat_err:
                entry["warnings"].append(seat_err)
            if counts is not None:
                entry["seats"] = counts
                entry["seat_logins"] = logins
            else:
                entry["warnings"].append(
                    "seat list unavailable (GitHub 404 -- typical of very large enterprises); "
                    "pool size is only known once the pool is exhausted"
                )

            if per_user:
                users, universal, budget_err = await _fetch_budgets(client, token, slug)
                if budget_err:
                    entry["warnings"].append(budget_err)
                entry["users"] = users
                entry["universal_budget_usd"] = universal
    except AICreditsError as e:
        entry["error"] = str(e)
        return entry
    except httpx.HTTPError as e:
        entry["error"] = f"GitHub request failed: {e}"
        return entry

    # Who belongs to this enterprise, for the gate's membership test. The seat list is the
    # authoritative answer when GitHub gives one; when it does not (large enterprises) the key
    # policy's cached org / team member lists are the fallback, so a user the access policy
    # already places in this enterprise is still recognised.
    if entry["seat_logins"] is None:
        entry["member_logins"] = ghcache.cached_enterprise_members(slug)

    _derive_pool(entry)
    return entry


def _derive_pool(entry: dict) -> None:
    """Fill pool_total / remaining / state from what was fetched. See the module docstring for
    why exhaustion is read off netQuantity rather than off the seat estimate."""
    if entry["metered"] > 0:
        entry["pool_total"] = entry["consumed"]
        entry["pool_total_source"] = "exact"
        entry["remaining"] = 0.0
        entry["state"] = POOL_EXHAUSTED
        return
    seats = entry.get("seats")
    if seats is not None:
        total = float(sum(INCLUDED_CREDITS.get(plan, 0) * n for plan, n in seats.items()))
        unknown_plans = [p for p in seats if p not in INCLUDED_CREDITS]
        if unknown_plans:
            entry["warnings"].append(
                f"seat plan(s) {', '.join(unknown_plans)} have no known included amount"
            )
        entry["pool_total"] = total
        entry["pool_total_source"] = "seats"
        entry["remaining"] = max(0.0, total - entry["consumed"])
        if total <= 0:
            entry["state"] = POOL_NONE
        else:
            entry["state"] = POOL_AVAILABLE if entry["remaining"] > 0 else POOL_EXHAUSTED
        return
    # No seat list and nothing metered: the pool exists if anything has been drawn from it.
    entry["state"] = POOL_AVAILABLE if entry["consumed"] > 0 else POOL_UNKNOWN


async def refresh(cfg) -> dict:
    """Poll every selected enterprise and persist the snapshot. Raises AICreditsError only when
    nothing at all could be done (no token, enterprise listing failed)."""
    token = cfg.gh_admin_token
    if not token:
        raise AICreditsError("no GitHub enterprise administrator token is configured")
    conf = settings(cfg)
    try:
        ents = await ghadmin.list_enterprises(token)
    except ghadmin.GitHubAdminError as e:
        raise AICreditsError(str(e)) from e
    wanted = set(conf["enterprises"])
    selected = [e for e in ents if not wanted or (e.get("slug") or "").lower() in wanted]
    missing = sorted(wanted - {(e.get("slug") or "").lower() for e in ents})
    results = await asyncio.gather(*[
        fetch_enterprise(token, e["slug"], e.get("name") or e["slug"], conf["per_user"])
        for e in selected
    ])
    now = time.time()
    snapshot = {
        "fetched_at": now,
        "fetched_at_iso": datetime.fromtimestamp(now, timezone.utc).isoformat(),
        "token_fp": token_fp(token),
        "schedule": conf["schedule"],
        "per_user": conf["per_user"],
        "enterprises": {r["slug"]: r for r in results},
        "missing_enterprises": missing,
    }
    write_json(SNAPSHOT_PATH, snapshot)
    _reload_snapshot()
    logger.info(
        "AI credits refreshed: %s",
        ", ".join(f"{r['slug']}={r['state']}" for r in results) or "no enterprises",
    )
    return snapshot


# -- Snapshot access ------------------------------------------------------------
def _reload_snapshot() -> dict | None:
    global _snapshot_cache, _snapshot_mtime
    with _snapshot_lock:
        current = mtime(SNAPSHOT_PATH)
        if _snapshot_cache is None or current != _snapshot_mtime:
            data = read_json(SNAPSHOT_PATH, {})
            _snapshot_cache = data if data.get("fetched_at") else {}
            _snapshot_mtime = current
        return _snapshot_cache or None


def load_snapshot() -> dict | None:
    return _reload_snapshot()


def invalidate() -> None:
    """Drop the snapshot. Called when the token changes: pool figures fetched under another
    token's visibility must not keep gating requests."""
    global _snapshot_cache, _snapshot_mtime
    try:
        SNAPSHOT_PATH.unlink()
    except FileNotFoundError:
        pass
    except OSError as e:
        logger.warning("could not remove %s: %s", SNAPSHOT_PATH, e)
    with _snapshot_lock:
        _snapshot_cache = None
        _snapshot_mtime = 0.0


def _interval_seconds(schedule: str, fetched_at: float) -> float:
    """The gap the schedule leaves between the last run and the next -- what "stale" is
    measured against."""
    try:
        anchor = datetime.fromtimestamp(fetched_at, timezone.utc)
        nxt = cronexpr.next_after(schedule, anchor)
        after = cronexpr.next_after(schedule, nxt)
        return max((after - nxt).total_seconds(), 60.0)
    except cronexpr.CronError:
        return 3600.0


def next_run_at(cfg, snapshot: dict | None = None) -> float | None:
    """When the next poll is due (epoch seconds), or None while polling is off."""
    conf = settings(cfg)
    if not conf["enabled"]:
        return None
    snap = snapshot if snapshot is not None else load_snapshot()
    anchor = float((snap or {}).get("fetched_at") or 0)
    if not anchor:
        return time.time()
    try:
        return cronexpr.next_after(conf["schedule"], datetime.fromtimestamp(anchor, timezone.utc)).timestamp()
    except cronexpr.CronError:
        return None


def due(cfg, now: float | None = None) -> bool:
    nxt = next_run_at(cfg)
    if nxt is None:
        return False
    return (now if now is not None else time.time()) >= nxt


def is_stale(cfg, snapshot: dict, now: float | None = None) -> bool:
    conf = settings(cfg)
    fetched = float(snapshot.get("fetched_at") or 0)
    if not fetched:
        return True
    limit = max(_STALE_FLOOR, _STALE_INTERVALS * _interval_seconds(conf["schedule"], fetched))
    return ((now if now is not None else time.time()) - fetched) > limit


# -- The gate -------------------------------------------------------------------
def user_headroom_usd(entry: dict, login: str) -> float | None:
    """How much this user may still consume on Copilot under their user-level budget, in USD.
    None = no user-level budget applies, i.e. only the pool limits them."""
    users = entry.get("users")
    if not isinstance(users, dict):
        return None
    rec = users.get(login)
    if rec:
        return float(rec.get("target_usd") or 0) - float(rec.get("consumed_usd") or 0)
    universal = entry.get("universal_budget_usd")
    if universal is not None:
        return float(universal)
    return None


def _render(template: str, entry: dict, headroom: float | None = None, target: float | None = None) -> str:
    remaining = entry.get("remaining")
    total = entry.get("pool_total")
    note = f" (about {remaining:,.0f} of {total:,.0f} credits)" if remaining is not None and total else ""
    text = template
    for key, value in (
        ("{enterprise}", entry.get("name") or entry.get("slug") or ""),
        ("{slug}", entry.get("slug") or ""),
        ("{remaining_note}", note),
        ("{remaining}", f"{remaining:,.0f}" if remaining is not None else "?"),
        ("{total}", f"{total:,.0f}" if total else "?"),
        ("{budget_remaining_usd}", f"{headroom:,.2f}" if headroom is not None else "?"),
        ("{budget_total_usd}", f"{target:,.2f}" if target is not None else "?"),
        ("{budget_remaining_credits}", f"{headroom / USD_PER_CREDIT:,.0f}" if headroom is not None else "?"),
    ):
        text = text.replace(key, str(value))
    return text


def membership(entry: dict, login: str) -> bool | None:
    """Whether this login belongs to the enterprise: True / False / None (cannot tell).

    A budget record is proof (the user consumed credits there). The seat list is authoritative
    when GitHub returned one, so absence from it is a real "no". Without a seat list the key
    policy's cached member lists decide, and a login none of them mention is *unknown* rather
    than absent -- those lists only cover the scopes the policy names.
    """
    users = entry.get("users")
    if isinstance(users, dict) and login in users:
        return True
    seat_logins = entry.get("seat_logins")
    if isinstance(seat_logins, dict):
        return login in seat_logins
    members = entry.get("member_logins") or {}
    if login in members:
        return True
    return None


def evaluate(conf: dict, entry: dict, login: str) -> dict | None:
    """The gate's verdict for one login against one enterprise, or None to let through.

    Two regimes, chosen by `per_user`:
      * enterprise mode: the caller must belong to this enterprise and its pool must have
        credits left;
      * per-user mode: the caller's own user-level budget decides when one applies -- headroom
        left means "use Copilot" even if the pool is exhausted (the budget is pre-approved
        Copilot spend), none left means GitHub blocks them and BYOK stays open. A caller with
        no user-level budget falls back to the pool test.
    Unknown membership lets the request through: gating someone whose enterprise we cannot
    place would send them to a pool that may not be theirs -- exactly the bug this guards.
    """
    if entry.get("error"):
        return None
    login = (login or "").strip().lower()
    if membership(entry, login) is not True:
        return None
    remaining = entry.get("remaining")
    pool_ok = entry.get("state") == POOL_AVAILABLE and (
        remaining is None or remaining > conf["min_remaining_credits"]
    )
    base = {
        "enterprise": entry.get("slug"),
        "enterprise_name": entry.get("name"),
        "remaining": remaining,
        "pool_total": entry.get("pool_total"),
        "pool_ok": pool_ok,
    }
    if conf["per_user"]:
        headroom = user_headroom_usd(entry, login)
        if headroom is not None:
            if headroom <= 0:
                return None
            rec = (entry.get("users") or {}).get(login) or {}
            target = rec.get("target_usd")
            if target is None:
                target = entry.get("universal_budget_usd")
            return base | {
                "reason": REASON_BUDGET,
                "headroom_usd": headroom,
                "budget_usd": target,
                "message": _render(conf["message_budget"] or DEFAULT_MESSAGE_BUDGET, entry, headroom, target),
            }
    if pool_ok:
        return base | {"reason": REASON_POOL, "message": _render(conf["message"] or DEFAULT_MESSAGE, entry)}
    return None


def gate(cfg, login: str) -> dict | None:
    """Decide whether this caller's request is answered with a note instead of being routed.

    Returns None to let the request through, otherwise the verdict from `evaluate` (with
    `message`, `reason`, `enterprise`). Every uncertainty resolves to "let it through": a
    snapshot from another token, a stale snapshot, an enterprise the caller cannot be placed
    in, a user GitHub itself would block. The gate protects a budget; it must never be the
    reason a developer cannot work.
    """
    conf = settings(cfg)
    if not conf["gate_enabled"]:
        return None
    snap = load_snapshot()
    if not snap or snap.get("token_fp") != token_fp(cfg.gh_admin_token):
        return None
    if is_stale(cfg, snap):
        return None
    for entry in (snap.get("enterprises") or {}).values():
        verdict = evaluate(conf, entry, login)
        if verdict:
            return verdict
    return None


# -- Status for the console -------------------------------------------------------
def status(cfg) -> dict:
    conf = settings(cfg)
    snap = load_snapshot()
    now = time.time()
    token_ok = bool(snap) and snap.get("token_fp") == token_fp(cfg.gh_admin_token)
    nxt = next_run_at(cfg, snap)
    out = {
        "settings": conf,
        "token_configured": bool(cfg.gh_admin_token),
        "default_message": DEFAULT_MESSAGE,
        "default_message_budget": DEFAULT_MESSAGE_BUDGET,
        "included_credits": INCLUDED_CREDITS,
        "fetched_at": (snap or {}).get("fetched_at") if token_ok else None,
        "stale": bool(snap) and token_ok and is_stale(cfg, snap, now),
        "token_changed": bool(snap) and not token_ok,
        "next_run_at": nxt,
        "due": bool(nxt) and now >= nxt,
        "snapshot_per_user": bool((snap or {}).get("per_user")),
        "missing_enterprises": (snap or {}).get("missing_enterprises") or [],
        "enterprises": [],
    }
    if snap and token_ok:
        for entry in (snap.get("enterprises") or {}).values():
            users = entry.get("users")
            seat_logins = entry.get("seat_logins") or {}
            members = entry.get("member_logins") or {}
            rows = []
            if isinstance(users, dict) or seat_logins:
                for login in sorted(set(seat_logins) | set(users or {})):
                    rec = (users or {}).get(login) or {}
                    headroom = user_headroom_usd(entry, login) if isinstance(users, dict) else None
                    verdict = evaluate(conf, entry, login)
                    rows.append({
                        "login": login,
                        "plan": seat_logins.get(login),
                        "target_usd": rec.get("target_usd"),
                        "consumed_usd": rec.get("consumed_usd"),
                        "headroom_usd": headroom,
                        "blocked_on_copilot": headroom is not None and headroom <= 0,
                        # What the gate would do for this login right now, under the saved settings
                        "gate": verdict["reason"] if verdict else None,
                    })
                rows.sort(key=lambda r: -(r["consumed_usd"] or 0))
            out["enterprises"].append({
                k: v for k, v in entry.items() if k not in ("users", "seat_logins", "member_logins")
            } | {
                "users": rows,
                "seat_count": len(seat_logins) if seat_logins else None,
                "member_source": "seats" if isinstance(entry.get("seat_logins"), dict)
                                 else ("cache" if members else None),
                "member_count": len(seat_logins) if isinstance(entry.get("seat_logins"), dict) else len(members),
            })
    return out


# -- Lease ------------------------------------------------------------------------
def acquire_lease() -> bool:
    lease = read_json(LOCK_PATH, {})
    if float(lease.get("expires_at") or 0) > time.time() and lease.get("owner_pid") != os.getpid():
        return False
    write_json(LOCK_PATH, {"owner_pid": os.getpid(), "expires_at": time.time() + _LEASE_TTL})
    return int(read_json(LOCK_PATH, {}).get("owner_pid") or 0) == os.getpid()


def release_lease() -> None:
    if read_json(LOCK_PATH, {}).get("owner_pid") == os.getpid():
        write_json(LOCK_PATH, {"owner_pid": None, "expires_at": 0})
