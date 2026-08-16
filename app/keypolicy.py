"""Policy evaluation for "who may create API keys".

Background: once GitHub OAuth is configured, any GitHub account can sign in to this
service, so signing in is not authorization by itself. The real gate sits at
**creating an API key** -- without a key you cannot call /v1/chat/completions, and
therefore cannot use BYOK.

Policy shape (auth.key_policy in config.yaml):

    auth:
      key_policy:
        enabled: true                 # false = any signed-in user may create keys (default)
        github_token: 'ghp_...'       # enterprise admin PAT
        enterprises:
          satomic:
            enabled: true             # enterprise master switch
            allow_all_orgs: false     # true = membership in any org of the enterprise suffices
            organizations: [nekoaru]  # allowed orgs (inert while the master switch is off)
            teams: [14501973]         # allowed enterprise teams (numeric ids)

Decision order, and why it is this order:
1. Admins (auth.admin_logins) pass immediately -- otherwise a misconfigured policy
   locks the admin out too, and since admins can edit the policy anyway, blocking
   them buys no security.
2. Policy disabled -> allow (preserves the previous default so upgrades do not break
   existing deployments).
3. Policy enabled but no token -> **deny**, with an explanation. Enabled but unable to
   query GitHub means there is no evidence at all, and allowing here would make
   "turn on access control" the least protected state.
4. For each enabled enterprise: check the allowed orgs first
   (`/orgs/{org}/members/{user}` 204/404), then the allowed enterprise teams. Any hit
   allows.
5. Enterprise-level membership is only attempted under `allow_all_orgs`, and it is
   tri-state -- "cannot tell" counts as no match, and evaluation falls back to probing
   the enumerated orgs of that enterprise one by one.

Failures are always fail-closed: better to withhold one key than to hand one out wrongly.

Membership questions go through app/ghcache.py rather than app/ghadmin.py directly: a locally
cached, complete member list answers them with zero GitHub calls, and anything less than a
complete list falls through to exactly the live probe this module used to make. Every evidence
row therefore carries `source` ("cache" / "probe" / "live") -- a decision whose provenance is
invisible is a decision nobody can debug.
"""
import asyncio
import logging

from . import ghadmin, ghcache

logger = logging.getLogger("fmr")

# Under allow_all_orgs, when enterprise-level membership is unavailable, how many orgs
# to probe individually at most. Very large enterprises have thousands of orgs, and
# probing all of them is both slow and rate-limited.
_MAX_ORG_PROBE = 30


async def _team_names(token: str, slug: str) -> dict[str, str]:
    """Map enterprise team id -> name.

    The policy stores **numeric ids** (the membership endpoint only accepts ids; a slug
    404s), but an id means nothing to a user -- "14501973" does not say which team it
    is. So the id is swapped for a name right before display.
    The listing call is cached for 300s and is only reached when teams are actually
    configured, so this adds no extra GitHub traffic.
    Returns an empty map when the team listing is unavailable (that endpoint 404s on
    some enterprises); callers then fall back to showing the id.
    """
    try:
        listing = await ghadmin.list_enterprise_teams(token, slug)
    except Exception as e:  # noqa: BLE001 display-only lookup, never fail the decision
        # Names are cosmetic, so no failure here may affect the authorization decision
        logger.warning("failed to resolve enterprise team names ent=%s: %s", slug, e)
        return {}
    return {
        str(t["id"]): (t.get("name") or t.get("slug") or str(t["id"]))
        for t in listing.get("teams") or []
        if t.get("id") is not None
    }


async def evaluate(cfg, login: str, is_admin: bool) -> dict:
    """Return {allowed, reason, detail, matched, policy_enabled, checked}.

    `reason` is a single sentence for the user; `detail` is the item-by-item evidence
    (the UI renders it as "current permissions and limits"). Never put the token or any
    other credential into the return value.
    """
    policy = cfg.key_policy
    if is_admin:
        return {
            "allowed": True,
            "reason": "You are an administrator of this service and can create API keys directly.",
            "policy_enabled": bool(policy.get("enabled")),
            "matched": {"kind": "admin"},
            "detail": [],
        }

    if not policy.get("enabled"):
        return {
            "allowed": True,
            "reason": "Enterprise access control is disabled, so any signed-in account can create API keys.",
            "policy_enabled": False,
            "matched": None,
            "detail": [],
        }

    token = (policy.get("github_token") or "").strip()
    if not token:
        return {
            "allowed": False,
            "reason": "Enterprise access control is enabled, but the administrator has not "
                      "configured a GitHub Enterprise token, so the service cannot verify your "
                      "enterprise membership. Please contact your administrator.",
            "policy_enabled": True,
            "matched": None,
            "detail": [],
        }

    enterprises = policy.get("enterprises") or {}
    active = {
        slug: rule for slug, rule in enterprises.items()
        if (rule or {}).get("enabled")
    }
    if not active:
        return {
            "allowed": False,
            "reason": "Enterprise access control is enabled, but the administrator has not "
                      "allowed any enterprise yet. Please contact your administrator.",
            "policy_enabled": True,
            "matched": None,
            "detail": [],
        }

    detail: list[dict] = []
    matched: dict | None = None

    for slug, rule in active.items():
        rule = rule or {}
        orgs = [str(o).strip() for o in (rule.get("organizations") or []) if str(o).strip()]
        teams = [t for t in (rule.get("teams") or []) if str(t).strip()]

        # Org and team checks can run concurrently: each is at worst one REST call, and a
        # cache hit is a set lookup. Team-name resolution joins the same gather -- it is just
        # a listing query and does not depend on the membership checks.
        # Each result is (is a member, source) -- see ghcache.
        org_results: list[tuple[bool, str]] = []
        team_results: list[tuple[bool, str]] = []
        team_names: dict[str, str] = {}
        if orgs:
            org_results = list(await asyncio.gather(
                *[ghcache.is_org_member(cfg, o, login) for o in orgs]
            ))
        if teams:
            team_results, team_names = await asyncio.gather(
                asyncio.gather(
                    *[ghcache.is_team_member(cfg, slug, t, login) for t in teams]
                ),
                _team_names(token, slug),
            )
            team_results = list(team_results)

        for org, (ok, source) in zip(orgs, org_results):
            detail.append({
                "enterprise": slug, "kind": "organization", "name": org, "member": ok,
                "source": source,
            })
            if ok and matched is None:
                matched = {"kind": "organization", "enterprise": slug, "name": org}

        for team, (ok, source) in zip(teams, team_results):
            # `name` carries the team name (which users understand); the id is kept
            # separately because admins still need it when troubleshooting, and because
            # `name` has to fall back to the id when the team listing is unavailable.
            tid = str(team)
            tname = team_names.get(tid, tid)
            detail.append({
                "enterprise": slug, "kind": "team", "name": tname,
                "id": tid, "member": ok, "source": source,
            })
            if ok and matched is None:
                matched = {
                    "kind": "team", "enterprise": slug, "name": tname, "id": tid,
                }

        if matched is not None:
            break

        if rule.get("allow_all_orgs"):
            # Enterprise-level membership is tri-state: None = GitHub cannot answer
            # (very large enterprises)
            # Not cached: this route is tri-state and unavailable on large enterprises, so
            # there is no member list to cache -- see ghadmin.check_enterprise_member.
            ent_member = await ghadmin.check_enterprise_member(token, slug, login)
            detail.append({
                "enterprise": slug, "kind": "enterprise", "name": slug,
                "member": ent_member, "source": ghcache.SOURCE_LIVE,
            })
            if ent_member:
                matched = {"kind": "enterprise", "enterprise": slug, "name": slug}
                break
            # When that is unanswerable (or the user is not a direct enterprise member),
            # fall back to checking the orgs of that enterprise one by one
            discovered = await ghadmin.list_enterprise_orgs(token, slug)
            candidates = [
                o["login"] for o in discovered["organizations"]
                if o["login"] not in orgs
            ][:_MAX_ORG_PROBE]
            if candidates:
                probes = list(await asyncio.gather(
                    *[ghcache.is_org_member(cfg, o, login) for o in candidates]
                ))
                for org, (ok, source) in zip(candidates, probes):
                    if ok:
                        matched = {
                            "kind": "organization", "enterprise": slug, "name": org,
                        }
                        detail.append({
                            "enterprise": slug, "kind": "organization",
                            "name": org, "member": True, "source": source,
                        })
                        break
                if matched is None:
                    detail.append({
                        "enterprise": slug, "kind": "org-scan", "name": slug,
                        "member": False, "scanned": len(candidates),
                        "truncated": len(discovered["organizations"]) > len(candidates),
                    })
            if matched is not None:
                break

    if matched is not None:
        where = {
            "organization": f"organization {matched['name']}",
            "team": f"enterprise team {matched['name']}",
            "enterprise": f"enterprise {matched['name']}",
        }.get(matched["kind"], matched["name"])
        return {
            "allowed": True,
            "reason": f"You are a member of {where} (enterprise {matched['enterprise']}), "
                      "so you can create API keys.",
            "policy_enabled": True,
            "matched": matched,
            "detail": detail,
        }

    return {
        "allowed": False,
        "reason": "You do not belong to any allowed enterprise, enterprise team or "
                  "organization, so you cannot create an API key and therefore cannot use "
                  "BYOK. To request access, ask your administrator to add your organization "
                  "to the allow list.",
        "policy_enabled": True,
        "matched": None,
        "detail": detail,
    }
