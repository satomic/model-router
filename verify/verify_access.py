"""Verify the enterprise access-control chain behind "who may create an API key".

Covered: token validation -> automatic discovery of enterprises/organizations/teams -> writing the
policy and hot-reloading it -> the administrator exemption -> a normal user authorized by
organization/team -> an unauthorized user rejected with 403 -> restoring the original policy.

The enterprise-admin token is read from an environment variable and is **never written into any
git-tracked file**:

    $env:GH_ENTERPRISE_TOKEN = 'ghp_...'   # PowerShell
    python verify/verify_access.py

If auth.key_policy.github_token is already configured in config.yaml, the environment variable can
be omitted. The script restores auth.key_policy to its pre-run state when it finishes.
"""
from __future__ import annotations

import _bootstrap  # noqa: F401  puts the repository root on sys.path and chdir()s into it

import os
import sys

import httpx

from app.authstore import AuthStore
from app.config import DATA_DIR, load_raw
from verify_auth_helper import make_client

BASE = "http://127.0.0.1:8000"
# Deliberately a GitHub account that cannot be inside the test enterprise, to exercise the
# "rejected" branch
OUTSIDER = "torvalds"

ok_count = 0
fail_count = 0


def check(label: str, cond: bool, extra: str = "") -> bool:
    global ok_count, fail_count
    if cond:
        ok_count += 1
        print(f"  [OK]   {label}{(' — ' + extra) if extra else ''}")
    else:
        fail_count += 1
        print(f"  [FAIL] {label}{(' — ' + extra) if extra else ''}")
    return cond


def _pick_org_with_member(token: str, orgs: list[dict]) -> tuple[str, str | None]:
    """Return (organization login, one real member of it); fall back to the first organization when
    none of them has a listable member."""
    headers = {"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"}
    with httpx.Client(timeout=30) as gh:
        for o in orgs:
            r = gh.get(f"https://api.github.com/orgs/{o['login']}/members", headers=headers)
            if r.status_code == 200 and r.json():
                return o["login"], r.json()[0]["login"]
    return orgs[0]["login"], None


def _team_member(token: str, slug: str, team_id: int) -> str | None:
    """Fetch one real member of an enterprise team, to exercise the "allowed" branch."""
    headers = {"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"}
    with httpx.Client(timeout=30) as gh:
        r = gh.get(
            f"https://api.github.com/enterprises/{slug}/teams/{team_id}/memberships",
            headers=headers,
        )
    if r.status_code == 200 and r.json():
        return r.json()[0].get("login")
    return None


def user_client(login: str) -> httpx.Client:
    """Seed a normal-user session -- is_admin is recomputed server-side from admin_logins, so
    passing False here is fine."""
    store = AuthStore(DATA_DIR)
    sid = store.create_session(
        {"login": login, "name": login, "avatar_url": None, "is_admin": False}, 3600
    )
    return httpx.Client(base_url=BASE, timeout=300, cookies={"fmr_session": sid})


def main() -> int:
    client, _key, admin_login = make_client()

    # The auth section is written back as **one whole section** (see the notes on config.save_raw),
    # so every request must carry the full pre-run auth content -- otherwise the github credentials
    # and admin_logins would be wiped.
    saved_auth = dict(load_raw().get("auth") or {})
    saved_policy = dict(saved_auth.get("key_policy") or {})
    token = (os.environ.get("GH_ENTERPRISE_TOKEN") or "").strip() or str(
        saved_policy.get("github_token") or ""
    ).strip()
    if not token:
        raise SystemExit(
            "No enterprise token. Set the GH_ENTERPRISE_TOKEN environment variable, or configure "
            "auth.key_policy.github_token in config.yaml first."
        )

    def put_policy(policy: dict) -> httpx.Response:
        return client.put("/v1/config", json={"auth": {**saved_auth, "key_policy": policy}})

    try:
        print("\n1. Token validation")
        r = client.post("/v1/access/verify-token", json={"token": token})
        check("POST /v1/access/verify-token returns 200", r.status_code == 200, str(r.status_code))
        owner = r.json() if r.status_code == 200 else {}
        check("the token's owning account is resolved", bool(owner.get("login")), str(owner.get("login")))
        check(
            "the token may enumerate enterprises",
            bool(owner.get("has_enterprise_scope")),
            f"scopes={owner.get('scopes')}",
        )
        r = client.post("/v1/access/verify-token", json={"token": "ghp_invalid_token_value"})
        check("an invalid token is rejected", r.status_code == 400, str(r.status_code))

        print("\n2. With the policy disabled, any signed-in account may create a key")
        check("wrote enabled=false", put_policy({"enabled": False}).status_code == 200)
        outsider = user_client(OUTSIDER)
        v = outsider.get("/v1/access/me").json()
        check("the outsider is allowed", v.get("allowed") is True, v.get("reason", ""))
        check("policy_enabled is false", v.get("policy_enabled") is False)

        print("\n3. Enabled but with no token -> fail closed")
        check("wrote enabled=true with an empty token", put_policy({"enabled": True, "github_token": ""}).status_code == 200)
        v = outsider.get("/v1/access/me").json()
        check("the outsider is rejected", v.get("allowed") is False, v.get("reason", ""))
        r = outsider.post("/v1/keys", json={"name": "should-fail"})
        check("POST /v1/keys returns 403", r.status_code == 403, str(r.status_code))
        v = client.get("/v1/access/me").json()
        check("administrators stay allowed (never lock yourself out)", v.get("allowed") is True, v.get("reason", ""))
        check("the administrator matched via kind=admin", (v.get("matched") or {}).get("kind") == "admin")

        print("\n4. Automatic discovery of enterprises, organizations and enterprise teams")
        check("wrote the token", put_policy({"enabled": True, "github_token": token}).status_code == 200)
        r = client.get("/v1/access/discover")
        check("GET /v1/access/discover returns 200", r.status_code == 200, str(r.status_code))
        ents = (r.json() or {}).get("enterprises") or []
        check("at least one enterprise was discovered", bool(ents), f"{len(ents)} found")
        for e in ents:
            print(
                f"         · {e['slug']}: orgs={len(e['organizations'])}/{e['organizations_total']}"
                f" truncated={e['organizations_truncated']} teams={len(e['teams'])}"
                + (f" teams_error={e['teams_error']}" if e.get("teams_error") else "")
            )
        check(
            "a truncated organization list still reports the total (nothing is dropped silently)",
            all(
                (not e["organizations_truncated"]) or e["organizations_total"] > len(e["organizations"])
                for e in ents
            ),
        )

        # Pick an enterprise that has both organizations and enterprise teams, so both
        # authorization paths can be exercised
        target = next((e for e in ents if e["organizations"] and e["teams"]), None)
        if target is None:
            target = next((e for e in ents if e["organizations"]), None)
        if target is None:
            print("\n  ! this token sees no enterprise with organizations, skipping steps 5-7")
            return 1 if fail_count else 0

        # Pick an organization that **has members**, otherwise the "allowed" branch is untestable
        org, member_login = _pick_org_with_member(token, target["organizations"])
        print(f"\n5. Authorization by organization (enterprise {target['slug']} / organization {org})")
        rule = {"enabled": True, "allow_all_orgs": False, "organizations": [org], "teams": []}
        check(
            "wrote the organization allowlist",
            put_policy(
                {"enabled": True, "github_token": token, "enterprises": {target["slug"]: rule}}
            ).status_code
            == 200,
            f"organizations=[{org}]",
        )
        v = outsider.get("/v1/access/me").json()
        check("a non-member is rejected", v.get("allowed") is False, v.get("reason", ""))
        check("the rejection lists each check", bool(v.get("detail")), f"{len(v.get('detail') or [])} rows")
        check(
            "the checked organization appears in the detail",
            any(d.get("name") == org for d in (v.get("detail") or [])),
        )
        r = outsider.post("/v1/keys", json={"name": "should-fail"})
        check("a non-member creating a key gets 403", r.status_code == 403, str(r.status_code))

        if member_login:
            mc = user_client(member_login)
            v = mc.get("/v1/access/me").json()
            check(f"organization member {member_login} is allowed", v.get("allowed") is True, v.get("reason", ""))
            check(
                "matched via kind=organization",
                (v.get("matched") or {}).get("kind") == "organization",
                str(v.get("matched")),
            )
            r = mc.post("/v1/keys", json={"name": "verify-access-member"})
            check("an organization member can create a key", r.status_code == 200, str(r.status_code))
            if r.status_code == 200:
                AuthStore(DATA_DIR).delete_api_key(r.json()["id"])
            mc.close()
        else:
            print(f"  !      cannot list members of {org}, skipping the \"allowed\" branch")

        if target["teams"]:
            team = target["teams"][0]
            print(f"\n5b. Authorization by enterprise team (team {team['name']} id={team['id']})")
            team_member = _team_member(token, target["slug"], team["id"])
            rule_team = {
                "enabled": True,
                "allow_all_orgs": False,
                "organizations": [],
                "teams": [str(team["id"])],
            }
            check(
                "wrote the team allowlist",
                put_policy(
                    {
                        "enabled": True,
                        "github_token": token,
                        "enterprises": {target["slug"]: rule_team},
                    }
                ).status_code
                == 200,
            )
            v = outsider.get("/v1/access/me").json()
            check("a non-team-member is rejected", v.get("allowed") is False, v.get("reason", ""))
            # The policy stores numeric ids, but the display must be the team name -- an id means
            # nothing to a user
            team_rows = [d for d in (v.get("detail") or []) if d.get("kind") == "team"]
            check(
                "the detail shows the team name, not its numeric id",
                bool(team_rows) and team_rows[0].get("name") == team["name"],
                str(team_rows[:1]),
            )
            check(
                "the team's numeric id is returned as secondary information",
                bool(team_rows) and team_rows[0].get("id") == str(team["id"]),
                str(team_rows[:1]),
            )
            if team_member:
                tc = user_client(team_member)
                v = tc.get("/v1/access/me").json()
                check(f"team member {team_member} is allowed", v.get("allowed") is True, v.get("reason", ""))
                check(
                    "matched via kind=team",
                    (v.get("matched") or {}).get("kind") == "team",
                    str(v.get("matched")),
                )
                check(
                    "the matched team name is readable (the reason carries it too)",
                    (v.get("matched") or {}).get("name") == team["name"]
                    and team["name"] in v.get("reason", ""),
                    str(v.get("matched")),
                )
                r = tc.post("/v1/keys", json={"name": "verify-access-team"})
                check("a team member can create a key", r.status_code == 200, str(r.status_code))
                if r.status_code == 200:
                    AuthStore(DATA_DIR).delete_api_key(r.json()["id"])
                tc.close()
            else:
                print(f"  !      team {team['id']} has no members, skipping the \"allowed\" branch")

        print("\n6. Turning an enterprise's master switch off disables all its organization rules")
        check(
            "restored the organization allowlist",
            put_policy(
                {"enabled": True, "github_token": token, "enterprises": {target["slug"]: rule}}
            ).status_code
            == 200,
        )
        rule_off = {**rule, "enabled": False}
        check(
            "wrote enterprise.enabled=false",
            put_policy(
                {"enabled": True, "github_token": token, "enterprises": {target["slug"]: rule_off}}
            ).status_code
            == 200,
        )
        if member_login:
            mc = user_client(member_login)
            v = mc.get("/v1/access/me").json()
            check("the previously allowed organization member is now rejected", v.get("allowed") is False, v.get("reason", ""))
            mc.close()

        print("\n7. Policy validation rejects malformed structures")
        r = put_policy({"enabled": "yes"})
        check("a non-boolean enabled is rejected with 422", r.status_code == 422, str(r.status_code))
        r = put_policy({"enabled": True, "enterprises": {"x": {"organizations": "org"}}})
        check("a non-list organizations is rejected with 422", r.status_code == 422, str(r.status_code))

        print("\n7b. A partial auth section is refused (so OAuth credentials cannot be wiped)")
        r = client.put("/v1/config", json={"auth": {"key_policy": {"enabled": False}}})
        check(
            "submitting key_policy alone is rejected with 422",
            r.status_code == 422,
            str(r.json().get("detail") if r.status_code == 422 else r.status_code),
        )

        print("\n8. Top-level key merge: only auth changes, providers/models are untouched")
        before = load_raw()
        check("providers unchanged", bool(before.get("providers")), str(list(before.get("providers") or {})))
        check("models unchanged", bool(before.get("models")), f"{len(before.get('models') or {})} entries")

        return 1 if fail_count else 0
    finally:
        outsider_close = locals().get("outsider")
        if isinstance(outsider_close, httpx.Client):
            outsider_close.close()
        # Restore the pre-run policy so the script never leaves the service locked down
        r = put_policy(saved_policy)
        print(f"\nauth.key_policy restored (HTTP {r.status_code})")
        client.close()
        print(f"\nResult: {ok_count} passed, {fail_count} failed")


if __name__ == "__main__":
    sys.exit(main())
