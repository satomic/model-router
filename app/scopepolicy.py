"""Policy evaluation for "who may narrow an API key's scope".

A key scope (app/keyscope.py) can only ever subtract from what its owner is allowed, so it is
not a privilege escalation. It is a **cost** control in reverse: a user who scopes their key to
one expensive model has pinned every request on that key to it, and the router's whole reason
for existing -- sending the cheap requests to a cheap model -- stops applying to that key. So
whether a user may set a scope at all is an administrator's decision, and the default is no.

Policy shape (auth.key_scope_policy in config.yaml):

    auth:
      key_scope_policy:
        enabled: false                   # false (the default) = nobody may narrow a key
        users: [alice]                   # GitHub logins
        teams: ['satomic/14501973']      # '<enterprise slug>/<team id>', as app/ghcache.py keys it
        organizations: [nekoaru]         # organization logins

Decision order:

1. **Administrators pass.** Same posture as app/keypolicy.py and app/modelpolicy.py: an admin's
   authority comes from `auth.admin_logins` (or the local admin account) and they can edit this
   very policy, so blocking them buys nothing and a bad save would lock the operator out of their
   own console.
2. **Disabled -> deny.** This is the default and it is deliberately the *closed* one, which is
   the opposite of key_policy's default. Scoping is an extra capability rather than the
   pre-existing behaviour: a key with no scope reaches everything its owner may reach, which is
   exactly what every key did before scopes existed, so denying by default changes nothing for
   anybody and adds no cost risk.
3. **Enabled, with at least one of the three levels filled in:** every *configured* level must
   match (AND), and within one level any single match is enough (OR). So `organizations: [acme]`
   plus `teams: ['ent/42']` means "a member of acme who is also on team 42"; `users: [alice]`
   alone means "alice, whichever teams and organizations she is in".
4. **Enabled but all three levels empty -> deny**, with an explanation. Enabled-and-nothing-listed
   is the same trap as key_policy's enabled-but-tokenless: reading it as "allow everybody" would
   make switching the control on the least protected state it has. There is deliberately no
   one-click "everybody may scope": that is the state this control exists to prevent, so the grant
   always names a user, a team or an organization.

Why AND across the levels rather than the OR that app/keypolicy.py uses: keypolicy answers "is
this person one of ours", where any single proof of belonging is enough. This answers "may this
person do a thing that costs money", where the levels are independent conditions an organization
wants to be able to stack -- the requirement was stated as an AND, and the two questions have
opposite failure modes, so they get opposite combinators.

An *unconfigured* level abstains rather than denying, which is the one reading that keeps the
feature usable: under a strict "must match all three" an administrator who lists only an
organization would grant nobody anything, because no login is in an empty user list. That
distinction is stated in the console next to the three tables, not left implicit.

Failures are fail-closed: a membership lookup that cannot be answered denies the level it belongs
to. The cost of being wrong in that direction is that a key covers everything its owner may
reach, i.e. the documented default, whereas the other direction hands out the narrow key this
policy exists to withhold.
"""
import asyncio
import logging

from . import ghcache

logger = logging.getLogger("mr")

# The three levels, in the order the console shows them and the order the reason strings read in.
LEVELS = ("user", "team", "organization")


def _entries(policy: dict, field: str) -> list[str]:
    """The configured values of one level, trimmed and de-blanked, order preserved."""
    out: list[str] = []
    for item in policy.get(field) or []:
        text = str(item).strip()
        if text and text not in out:
            out.append(text)
    return out


def _team_target(key: str) -> tuple[str, str] | None:
    """Split a `teams` entry into (enterprise slug, team id).

    Same key format and same "report rather than guess" handling as app/modelpolicy.py: ghcache
    identifies an enterprise team by both parts, so a bare id cannot be looked up.
    """
    slug, _, team_id = str(key).partition("/")
    slug, team_id = slug.strip(), team_id.strip()
    if not slug or not team_id:
        return None
    return slug, team_id


async def evaluate(cfg, login: str, is_admin: bool = False) -> dict:
    """Decide whether `login` may set a scope on their API keys.

    Returns:
        allowed          the verdict
        reason           one English sentence, for logs, 403 bodies and API callers
        reason_code      the same verdict, machine-readable, so the console can translate it
        reason_params    the values that sentence interpolates ({levels: [...]} where it has any)
        policy_enabled   whether the control is switched on at all
        levels           one row per level: {level, configured, passed, matched, source}

    Not memoised, unlike app/modelpolicy.py: this runs on key creation and on one console page,
    never on the request path, and the membership lookups underneath are already cache-first.
    """
    login = (login or "").strip().lower()
    policy = cfg.key_scope_policy

    if is_admin:
        return {
            "allowed": True,
            "reason": "Administrators may set a scope on any key.",
            "reason_code": "admin",
            "reason_params": {},
            "policy_enabled": bool(policy.get("enabled", False)),
            "levels": [],
        }

    if not cfg.key_scope_policy_enabled:
        return {
            "allowed": False,
            "reason": "Narrowing an API key is not enabled on this deployment, so every key "
                      "covers all models and all connection types. Please contact your "
                      "administrator.",
            "reason_code": "off",
            "reason_params": {},
            "policy_enabled": False,
            "levels": [],
        }

    users = _entries(policy, "users")
    teams = _entries(policy, "teams")
    orgs = _entries(policy, "organizations")

    if not (users or teams or orgs):
        return {
            "allowed": False,
            "reason": "Narrowing an API key is enabled, but the administrator has not allowed "
                      "any user, team or organization yet, so every key covers all models and "
                      "all connection types. Please contact your administrator.",
            "reason_code": "nobodyAllowed",
            "reason_params": {},
            "policy_enabled": True,
            "levels": [],
        }

    # Team and organization membership are independent lookups, so they run concurrently: a
    # serial walk would put one round trip per scope on the first request after a cache miss.
    async def check_org(org: str) -> tuple[bool, str]:
        try:
            return await ghcache.is_org_member(cfg, org, login)
        except Exception as e:  # noqa: BLE001 fail-closed, see the module docstring
            logger.warning("key scope policy: org %s membership check failed: %s", org, e)
            return False, "error"

    async def check_team(key: str) -> tuple[bool, str]:
        target = _team_target(key)
        if target is None:
            logger.warning(
                "key scope policy: teams entry %r is not '<enterprise-slug>/<team-id>', ignoring",
                key,
            )
            return False, "malformed"
        try:
            return await ghcache.is_team_member(cfg, target[0], target[1], login)
        except Exception as e:  # noqa: BLE001
            logger.warning("key scope policy: team %s membership check failed: %s", key, e)
            return False, "error"

    team_results, org_results = await asyncio.gather(
        asyncio.gather(*[check_team(k) for k in teams]),
        asyncio.gather(*[check_org(o) for o in orgs]),
    )

    def first_match(names: list[str], results) -> tuple[str, str]:
        for name, (member, source) in zip(names, results):
            if member:
                return name, source
        return "", ""

    user_match = next((u for u in users if u.strip().lower() == login), "")
    team_match, team_source = first_match(teams, team_results)
    org_match, org_source = first_match(orgs, org_results)

    levels = [
        {
            "level": "user",
            "configured": bool(users),
            "passed": (not users) or bool(user_match),
            "matched": user_match,
            "source": "config" if user_match else "",
        },
        {
            "level": "team",
            "configured": bool(teams),
            "passed": (not teams) or bool(team_match),
            "matched": team_match,
            "source": team_source,
        },
        {
            "level": "organization",
            "configured": bool(orgs),
            "passed": (not orgs) or bool(org_match),
            "matched": org_match,
            "source": org_source,
        },
    ]

    failed = [row["level"] for row in levels if row["configured"] and not row["passed"]]
    if failed:
        return {
            "allowed": False,
            # Which levels failed, because the user's only route to the capability is asking an
            # administrator for it, and for that they need to know what to ask to be added to.
            "reason": "You are not allowed to narrow an API key: the administrator requires a "
                      "match at " + _join(failed) + " level, and you do not have one. Every key "
                      "you create covers all models and all connection types. Please contact your "
                      "administrator.",
            "reason_code": "levelsFailed",
            # The level names, not the joined English phrase: the console names and joins them
            # in the reader's language, where the list separator is not a comma everywhere.
            "reason_params": {"levels": failed},
            "policy_enabled": True,
            "levels": levels,
        }

    return {
        "allowed": True,
        "reason": "You may restrict an API key to specific models or connection types.",
        "reason_code": "allowed",
        "reason_params": {},
        "policy_enabled": True,
        "levels": levels,
    }


def _join(levels: list[str]) -> str:
    """'the user', 'the user and team', 'the user, team and organization' -- reason strings only."""
    if len(levels) == 1:
        return f"the {levels[0]}"
    return "the " + ", ".join(levels[:-1]) + f" and {levels[-1]}"
