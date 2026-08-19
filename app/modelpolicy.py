"""Which models a given caller may use: named model groups bound to user / team / organization
scopes, resolved as a **union**.

The shape in config.yaml:

    model_groups:                 # independent, named, reusable; an empty group is legal
      starter:   [gpt-4o]
      full:      [gpt-4o, gpt-5.4, o3-pro]
      locked:    []

    model_policy:
      enabled: true
      default_group: starter      # every signed-in user, before any binding below
      users:
        alice: full               # a GitHub login
      teams:
        my-enterprise/14501973: full   # "<enterprise slug>/<team id>", as app/ghcache.py keys it
      organizations:
        acme: full                # an organization login

Resolution, and the two questions the shape forces:

1. **Union, not override.** A caller's effective set is the union of the default group and every
   group bound to a scope they belong to. Precedence was specified as a union, so a team binding
   can only ever *add* to what the user already had -- there is no way to configure a scope that
   takes models away, and that is a deliberate property: an operator can hand out a wider group
   without auditing every narrower binding first.

2. **An empty group contributes nothing, so the union is empty only when every contributor is.**
   That is what makes "a newly signed-in user gets an empty group" work: set `default_group` to an
   empty group and a user with no other binding resolves to *nothing*, which is refused outright
   (403 on a call, an empty list on /v1/models). Add any non-empty binding for them and the union
   grows. The empty group is therefore not a "deny" rule that overrides -- it is the absence of a
   grant, which under a union is the same thing right up until something else grants.

3. **No binding at all means unrestricted.** If the policy is enabled, `default_group` is unset,
   and no scope binding matches, the caller gets the whole catalog. Enabling the toggle before
   filling the tables in must not lock the deployment out of itself; an operator who wants a
   closed default says so by pointing `default_group` at an empty group. This is the one place
   where "not configured" and "configured to nothing" deliberately differ, and it is why
   `default_group` exists as a separate field rather than being inferred.

4. **Administrators are exempt.** Same posture as app/keypolicy.py: an admin's authority comes
   from `auth.admin_logins` (or the local admin account), and a model policy is a distribution
   control, not a privilege boundary. Making it apply to admins would let one save lock the
   operator out of the playground with no way back except editing config.yaml by hand.

How the effective set is *enforced* is not in this module. The router applies it by narrowing the
catalog (`RouterConfig.restricted_to`) before any routing decision runs, so a rule whose model is
not permitted is skipped with the same `step["skipped"]` the catalog check already produces, the
decision model is only ever offered permitted candidates, and the default-model substitution
lands inside the permitted set. Nothing needs a new failure mode; see app/main.py.

Membership answers come from app/ghcache.py, cache-first with a live fallback, exactly as
keypolicy does -- including its rule that a truncated or errored member list is never
authoritative. Unlike keypolicy, a membership lookup that cannot be answered here fails *open*
for that one scope: it simply does not contribute. Failing closed would be wrong under a union,
because the union's failure mode is "the user sees fewer models than the operator granted", and
silently narrowing someone's model list on a transient GitHub error is worse than briefly
including a scope they may have left.
"""
import asyncio
import logging
import threading
import time

from . import ghcache

logger = logging.getLogger("mr")

# Resolution is on the hot path -- every /v1/chat/completions and every /v1/models call needs an
# effective set -- and each scope lookup reads a JSON file. So results are memoised per login for
# a short window. 60s bounds how long a membership or policy change takes to show up in routing;
# a configuration save calls invalidate() and does not wait for it.
_TTL = 60.0
_MAX_ENTRIES = 5000  # bound the dict on a deployment with many distinct callers

_lock = threading.Lock()
_cache: dict[str, tuple[float, dict]] = {}


def invalidate() -> None:
    """Drop every memoised verdict. Called when the configuration changes."""
    with _lock:
        _cache.clear()


def _cached(login: str) -> dict | None:
    with _lock:
        entry = _cache.get(login)
        if entry and entry[0] > time.time():
            return entry[1]
        return None


def _store(login: str, verdict: dict) -> None:
    with _lock:
        if len(_cache) >= _MAX_ENTRIES:
            _cache.clear()
        _cache[login] = (time.time() + _TTL, verdict)


def _team_target(key: str) -> tuple[str, str] | None:
    """Split a `teams` key into (enterprise slug, team id).

    ghcache identifies an enterprise team by both parts, so a bare team id cannot be looked up.
    A key without a slash is reported rather than guessed at.
    """
    slug, _, team_id = str(key).partition("/")
    slug, team_id = slug.strip(), team_id.strip()
    if not slug or not team_id:
        return None
    return slug, team_id


def _order(cfg, names: set[str]) -> list[str]:
    """Catalog order, so the list a user sees matches the Models page rather than set order."""
    return [name for name in cfg.models if name in names]


async def evaluate(cfg, login: str, is_admin: bool = False) -> dict:
    """Resolve the effective model set for `login`.

    Returns:
        enabled        whether the policy is switched on at all
        unrestricted   True when the whole catalog applies (policy off, admin, or no binding)
        models         the effective model names, in catalog order
        default_group  the group name every signed-in user starts from ('' when unset)
        contributions  one row per grant that applied: {scope, name, group, models, source}
        reason         a short machine-readable explanation of which of the above decided

    Only contributions that *applied* are returned. A regular user's own view is the main
    consumer, and listing the teams and organizations they are **not** in would publish the
    policy tables to everybody who can sign in.
    """
    login = (login or "").strip().lower()
    if not cfg.model_policy_enabled:
        return {
            "enabled": False,
            "unrestricted": True,
            "models": list(cfg.models),
            "default_group": cfg.default_group,
            "contributions": [],
            "reason": "policy-disabled",
        }
    if is_admin:
        return {
            "enabled": True,
            "unrestricted": True,
            "models": list(cfg.models),
            "default_group": cfg.default_group,
            "contributions": [],
            "reason": "administrator",
        }

    cached = _cached(login)
    if cached is not None:
        return cached

    policy = cfg.model_policy
    contributions: list[dict] = []
    allowed: set[str] = set()

    def contribute(scope: str, name: str, group: str, source: str = "config") -> None:
        members = cfg.group_models(group)
        allowed.update(members)
        contributions.append({
            "scope": scope,
            "name": name,
            "group": group,
            "models": members,
            "source": source,
        })

    # 1. The default every signed-in user starts from.
    if cfg.default_group and cfg.default_group in cfg.model_groups:
        contribute("default", cfg.default_group, cfg.default_group)

    # 2. A binding on this exact login.
    users = policy.get("users") or {}
    user_group = next(
        (g for k, g in users.items() if str(k).strip().lower() == login and g), None
    )
    if user_group and user_group in cfg.model_groups:
        contribute("user", login, str(user_group))

    # 3. Teams and organizations, whose bindings only apply on proven membership. Every lookup
    #    runs concurrently: they are independent, and a serial walk over a dozen scopes would put
    #    a dozen round trips on the first uncached request of each user.
    orgs = [
        (str(name), str(group))
        for name, group in (policy.get("organizations") or {}).items()
        if group and group in cfg.model_groups
    ]
    teams = [
        (str(name), str(group))
        for name, group in (policy.get("teams") or {}).items()
        if group and group in cfg.model_groups
    ]

    async def check_org(org: str):
        try:
            return await ghcache.is_org_member(cfg, org, login)
        except Exception as e:  # noqa: BLE001 see the module docstring: one scope fails open
            logger.warning("model policy: org %s membership check failed: %s", org, e)
            return None, "error"

    async def check_team(key: str):
        target = _team_target(key)
        if target is None:
            logger.warning(
                "model policy: teams key %r is not '<enterprise-slug>/<team-id>', ignoring", key
            )
            return None, "malformed"
        try:
            return await ghcache.is_team_member(cfg, target[0], target[1], login)
        except Exception as e:  # noqa: BLE001
            logger.warning("model policy: team %s membership check failed: %s", key, e)
            return None, "error"

    org_results, team_results = await asyncio.gather(
        asyncio.gather(*[check_org(name) for name, _ in orgs]),
        asyncio.gather(*[check_team(name) for name, _ in teams]),
    )
    for (name, group), (member, source) in zip(orgs, org_results):
        if member:
            contribute("organization", name, group, source)
    for (name, group), (member, source) in zip(teams, team_results):
        if member:
            contribute("team", name, group, source)

    if not contributions:
        # Nothing is bound to this caller at all. See point 3 of the module docstring: this is
        # "unconfigured", not "configured to nothing", and it must not lock anyone out.
        verdict = {
            "enabled": True,
            "unrestricted": True,
            "models": list(cfg.models),
            "default_group": cfg.default_group,
            "contributions": [],
            "reason": "no-binding",
        }
    else:
        verdict = {
            "enabled": True,
            "unrestricted": False,
            "models": _order(cfg, allowed),
            "default_group": cfg.default_group,
            "contributions": contributions,
            "reason": "union" if allowed else "empty-group",
        }
    _store(login, verdict)
    return verdict


async def allowed_models(cfg, login: str, is_admin: bool = False) -> list[str] | None:
    """The effective model names, or None when the caller is unrestricted.

    None rather than the full catalog, so a caller can tell "no policy applies" from "the policy
    happens to allow everything" -- the router only narrows its catalog for the former.
    """
    verdict = await evaluate(cfg, login, is_admin)
    return None if verdict["unrestricted"] else verdict["models"]
