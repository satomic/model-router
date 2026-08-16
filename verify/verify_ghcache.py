"""Verify the on-disk GitHub cache: what it writes, when it answers locally, and -- more
importantly -- the cases where it must refuse to answer and fall back to a live probe.

The speed-up is the easy part. The part worth testing is trust: a truncated, errored or stale
member list must never be authoritative, because "not in the first 5000 logins I could read"
is not "not a member", and reading it as one denies a legitimate user their API key.

Env-gated exactly like verify_access.py -- it needs a real enterprise-admin token, which is
never written into any git-tracked file:

    $env:GH_ENTERPRISE_TOKEN = 'ghp_...'   # PowerShell
    python verify/verify_ghcache.py

If auth.key_policy.github_token is already in config.yaml the variable may be omitted. The
script restores auth.key_policy and the cache directory it found on the way out.
"""
from __future__ import annotations

import _bootstrap  # noqa: F401  puts the repository root on sys.path and chdir()s into it

import hashlib
import os
import shutil
import sys
import time

import httpx

from app import ghcache
from app.authstore import AuthStore, read_json, write_json
from app.config import DATA_DIR, load_raw
from verify_auth_helper import BASE, make_client

# A GitHub account that cannot be inside the test enterprise, so it exercises the
# not-a-member path (the same choice verify_access.py makes).
OUTSIDER = "torvalds"

ok_count = 0
fail_count = 0


def check(label: str, cond: bool, extra: str = "") -> bool:
    global ok_count, fail_count
    if cond:
        ok_count += 1
        print(f"  [OK]   {label}" + (f" - {extra}" if extra else ""))
    else:
        fail_count += 1
        print(f"  [FAIL] {label}" + (f" - {extra}" if extra else ""))
    return cond


def user_client(login: str) -> httpx.Client:
    """Seed a normal-user session; is_admin is recomputed server-side from admin_logins."""
    sid = AuthStore(DATA_DIR).create_session(
        {"login": login, "name": login, "avatar_url": None, "is_admin": False}, 3600
    )
    return httpx.Client(base_url=BASE, timeout=300, cookies={"fmr_session": sid})


def sources_for(verdict: dict, kind: str | None = None) -> list[str]:
    """The `source` values in an /v1/access/me verdict, optionally for one kind of row."""
    return [
        str(d.get("source"))
        for d in (verdict.get("detail") or [])
        if kind is None or d.get("kind") == kind
    ]


def clear_probes() -> None:
    """Empty probe.json, so a cached individual probe cannot answer in place of the member
    list under test -- that would hide exactly the fall-through being checked."""
    write_json(ghcache.PROBE_PATH, {"entries": {}})


def main() -> int:  # noqa: PLR0915 one linear scenario reads better than split helpers
    client, _api_key, _admin_login = make_client()

    saved_auth = dict(load_raw().get("auth") or {})
    saved_policy = dict(saved_auth.get("key_policy") or {})
    token = (os.environ.get("GH_ENTERPRISE_TOKEN") or "").strip() or str(
        saved_policy.get("github_token") or ""
    ).strip()
    if not token:
        raise SystemExit(
            "No enterprise token. Set the GH_ENTERPRISE_TOKEN environment variable, or "
            "configure auth.key_policy.github_token in config.yaml first."
        )

    # Preserve whatever the deployment had cached: this script rewrites and deletes those
    # files, and an operator's warm cache should not be a casualty of running it.
    backup = DATA_DIR / "github.verify-backup"
    if backup.exists():
        shutil.rmtree(backup)
    if ghcache.CACHE_DIR.exists():
        shutil.copytree(ghcache.CACHE_DIR, backup)

    def put_policy(policy: dict) -> httpx.Response:
        # The whole auth section goes back, or _dropped_auth_credentials refuses the save.
        return client.put("/v1/config", json={"auth": {**saved_auth, "key_policy": policy}})

    outsider: httpx.Client | None = None
    member_client: httpx.Client | None = None
    try:
        print("\n1. The fingerprint identifies the token without storing it")
        fp = ghcache.token_fp(token)
        check("token_fp is 12 hex characters",
              len(fp) == 12 and all(c in "0123456789abcdef" for c in fp), fp)
        check("token_fp is a prefix of sha256(token)",
              hashlib.sha256(token.encode("utf-8")).hexdigest().startswith(fp))
        check("token_fp('') is empty (an absent token has no fingerprint)",
              ghcache.token_fp("") == "")
        check("a different token yields a different fingerprint",
              ghcache.token_fp(token + "x") != fp)

        print("\n2. A forced refresh writes structure.json and members.json")
        # Discover first, so the policy can name a real org: _scopes_to_fetch only fetches
        # the member lists the policy actually references.
        check("wrote the token",
              put_policy({"enabled": True, "github_token": token}).status_code == 200)
        ents = (client.get("/v1/access/discover", params={"refresh": 1}).json() or {}
                ).get("enterprises") or []
        target = next((e for e in ents if e.get("organizations")), None)
        if target is None:
            print("  !      this token sees no enterprise with organizations; "
                  "there is nothing to cache")
            return 1 if fail_count else 0
        slug = str(target["slug"])
        org = str(target["organizations"][0]["login"])
        teams = [str(t["id"]) for t in (target.get("teams") or [])][:1]
        rule = {"enabled": True, "allow_all_orgs": False,
                "organizations": [org], "teams": teams}
        check("wrote a policy naming one organization",
              put_policy({"enabled": True, "github_token": token,
                          "enterprises": {slug: rule}}).status_code == 200,
              f"{slug}/{org} teams={teams}")

        r = client.post("/v1/access/cache/refresh")
        check("POST /v1/access/cache/refresh -> 200", r.status_code == 200, str(r.status_code))
        check("data/github/structure.json was written", ghcache.STRUCTURE_PATH.exists())
        check("data/github/members.json was written", ghcache.MEMBERS_PATH.exists())
        st = r.json() if r.status_code == 200 else {}
        check("the status reports the enterprises it stored",
              bool((st.get("structure") or {}).get("enterprises")),
              f"{len((st.get('structure') or {}).get('enterprises') or [])} enterprises")
        scopes = (st.get("members") or {}).get("scopes") or []
        org_key = f"org:{org.lower()}"
        check("a member list was fetched for the org the policy names",
              any(s["key"] == org_key for s in scopes), str([s["key"] for s in scopes]))
        check("token_matches is true right after a refresh", st.get("token_matches") is True)
        check("the cache is not stale right after a refresh", st.get("stale") is False)
        for s in scopes:
            print(f"         {s['key']}: {s['count']} logins truncated={s['truncated']}"
                  + (f" error={s['error']}" if s["error"] else ""))

        print("\n3. Neither the token nor any member login leaves the process")
        structure_text = ghcache.STRUCTURE_PATH.read_text(encoding="utf-8")
        members_text = ghcache.MEMBERS_PATH.read_text(encoding="utf-8")
        check("structure.json does not contain the token", token not in structure_text)
        check("members.json does not contain the token", token not in members_text)
        check("both files carry the fingerprint instead",
              f'"{fp}"' in structure_text and f'"{fp}"' in members_text)
        status_text = client.get("/v1/access/cache").text
        check("GET /v1/access/cache does not carry the token", token not in status_text)
        # The logins live in members.json by design -- that file *is* the cache. The status
        # endpoint reports counts only: a cache panel is the wrong place to publish an
        # organization's roster to every administrator who opens it.
        entry = (read_json(ghcache.MEMBERS_PATH, {}).get("entries") or {}).get(org_key) or {}
        sample = next(iter(entry.get("logins") or []), None)
        if sample:
            check("GET /v1/access/cache does not publish member logins",
                  sample not in status_text, f"checked {sample!r}")
        else:
            print(f"  !      {org} has no listable members; skipping the login-leak check")

        print("\n4. A cached member is answered locally, with zero GitHub calls")
        complete = bool(sample) and not entry.get("truncated") and not entry.get("error")
        if complete:
            member_client = user_client(str(sample))
            v = member_client.get("/v1/access/me").json()
            check(f"{sample} is allowed", v.get("allowed") is True, str(v.get("reason", "")))
            check("the organization row was answered from the cache",
                  ghcache.SOURCE_CACHE in sources_for(v, "organization"),
                  str(sources_for(v)))
            check("and it matched on kind=organization",
                  (v.get("matched") or {}).get("kind") == "organization",
                  str(v.get("matched")))
            # A second call must stay on the cache. If it fell through to "live" the list is
            # not being trusted and the module is buying nothing.
            again = member_client.get("/v1/access/me").json()
            check("a second call is still answered from the cache",
                  ghcache.SOURCE_CACHE in sources_for(again, "organization"),
                  str(sources_for(again)))
        else:
            print(f"  !      no complete member list for {org}; "
                  "skipping the cache-hit branch")

        print("\n5. An unknown login: answered live, then remembered as a negative")
        clear_probes()
        outsider = user_client(OUTSIDER)
        v = outsider.get("/v1/access/me").json()
        check(f"{OUTSIDER} is refused", v.get("allowed") is False, str(v.get("reason", "")))
        org_sources = sources_for(v, "organization")
        check("the verdict carries an organization row to read a source off",
              bool(org_sources), str(sources_for(v)))
        if ghcache.SOURCE_LIVE in org_sources:
            probes = read_json(ghcache.PROBE_PATH, {}).get("entries") or {}
            probe_key = f"{org_key}:{OUTSIDER.lower()}"
            check("the live probe was written to probe.json", probe_key in probes,
                  str(sorted(probes)[:4]))
            check("and it was stored as a negative",
                  (probes.get(probe_key) or {}).get("member") is False,
                  str(probes.get(probe_key)))
            # Wall-clock, not monotonic: a monotonic value is meaningless once written to
            # disk and read back after a restart, which is the whole point of this file.
            at = float((probes.get(probe_key) or {}).get("at") or 0)
            check("the probe timestamp is wall-clock time (within a minute of now)",
                  abs(time.time() - at) < 60, f"at={at:.0f} now={time.time():.0f}")
            v2 = outsider.get("/v1/access/me").json()
            check("a repeat inside the negative TTL is answered from probe.json, "
                  "with no GitHub call",
                  ghcache.SOURCE_PROBE in sources_for(v2, "organization"),
                  str(sources_for(v2)))
        else:
            check("a complete member list answered the non-member locally",
                  ghcache.SOURCE_CACHE in org_sources, str(org_sources))
            print("  !      the list is complete, so the outsider never reached a live "
                  "probe; the negative-probe branch is not exercised")

        print("\n6. An untrustworthy member list is not authoritative")
        # Each case mutates the cached entry in place, leaving the logins alone. If the flag
        # were ignored the answer would still come back "cache".
        data = read_json(ghcache.MEMBERS_PATH, {})
        original_entry = dict((data.get("entries") or {}).get(org_key) or {})
        if original_entry:
            cases = (
                ("truncated", {"truncated": True}),
                ("errored", {"error": "verify: simulated failure"}),
                ("a month stale", {"fetched_at": time.time() - 30 * 24 * 3600}),
            )
            for label, patch in cases:
                data["entries"][org_key] = {**original_entry, **patch}
                write_json(ghcache.MEMBERS_PATH, data)
                clear_probes()
                v = outsider.get("/v1/access/me").json()
                check(f"a {label} list falls through to a live probe",
                      ghcache.SOURCE_CACHE not in sources_for(v, "organization"),
                      str(sources_for(v)))
            st = client.get("/v1/access/cache").json()
            check("the status reports the cache as stale", st.get("stale") is True)
            # Put the flags back, then confirm the status counts them when they are set.
            for field, counter in (("truncated", "truncated_scopes"),
                                   ("error", "errored_scopes")):
                data["entries"][org_key] = {
                    **original_entry,
                    field: True if field == "truncated" else "verify: simulated failure",
                }
                write_json(ghcache.MEMBERS_PATH, data)
                st = client.get("/v1/access/cache").json()
                check(f"the status counts the {field} scope",
                      st["members"][counter] >= 1, str(st["members"][counter]))
        else:
            print(f"  !      no cached entry for {org_key}; skipping the trust checks")

        print("\n7. A cache fetched under another token is not trusted")
        # Written directly rather than through the API: PUT /v1/config deletes these files
        # outright (ghcache.invalidate), and the guarantee under test here is the weaker one
        # -- that a *surviving* file whose fingerprint does not match is ignored anyway.
        other_fp = ghcache.token_fp("some-other-token")
        data = read_json(ghcache.MEMBERS_PATH, {})
        data["token_fp"] = other_fp
        if original_entry:
            data["entries"][org_key] = original_entry
        write_json(ghcache.MEMBERS_PATH, data)
        structure = read_json(ghcache.STRUCTURE_PATH, {})
        structure["token_fp"] = other_fp
        write_json(ghcache.STRUCTURE_PATH, structure)
        clear_probes()

        st = client.get("/v1/access/cache").json()
        check("the status reports token_matches=false", st.get("token_matches") is False,
              str(st.get("token_matches")))
        v = outsider.get("/v1/access/me").json()
        check("a member list under another fingerprint does not answer",
              ghcache.SOURCE_CACHE not in sources_for(v, "organization"),
              str(sources_for(v)))
        d = client.get("/v1/access/discover").json() or {}
        check("GET /v1/access/discover refetches rather than showing another token's view",
              d.get("cached") is not True, str(d.get("cached")))

        print("\n8. Changing the policy wipes the cache files outright")
        check("wrote a changed policy",
              put_policy({"enabled": True, "github_token": token,
                          "enterprises": {slug: {**rule, "allow_all_orgs": True}}}
                         ).status_code == 200)
        check("members.json is gone", not ghcache.MEMBERS_PATH.exists())
        check("structure.json is gone", not ghcache.STRUCTURE_PATH.exists())
        check("probe.json is gone", not ghcache.PROBE_PATH.exists())
        st = client.get("/v1/access/cache").json()
        check("the status survives an empty cache and calls it stale",
              st.get("stale") is True and not st["members"]["scopes"], str(st.get("stale")))

        print("\n9. Serving the structure from disk, which is the latency the cache removes")
        check("restored the single-org policy",
              put_policy({"enabled": True, "github_token": token,
                          "enterprises": {slug: rule}}).status_code == 200)
        check("refreshed the cache again",
              client.post("/v1/access/cache/refresh").status_code == 200)
        first = client.get("/v1/access/discover").json() or {}
        check("GET /v1/access/discover is served from the cache",
              first.get("cached") is True, str(first.get("cached")))
        check("and it says when it was fetched", bool(first.get("fetched_at")),
              str(first.get("fetched_at")))
        live = client.get("/v1/access/discover", params={"refresh": 1}).json() or {}
        check("?refresh=1 goes to GitHub instead", live.get("cached") is not True,
              str(live.get("cached")))
        check("both routes report the same enterprises",
              {e["slug"] for e in first.get("enterprises") or []}
              == {e["slug"] for e in live.get("enterprises") or []})

        print("\n10. The refresh lease")
        check("the lease can be taken", ghcache.acquire_lease() is True)
        check("the holder may retake its own lease", ghcache.acquire_lease() is True)
        lease = read_json(ghcache.LOCK_PATH, {})
        check("the lease records this pid and an expiry",
              lease.get("owner_pid") == os.getpid()
              and float(lease.get("expires_at") or 0) > time.time(), str(lease))
        write_json(ghcache.LOCK_PATH,
                   {"owner_pid": os.getpid() + 99999, "expires_at": time.time() + 300})
        check("another process's live lease blocks a refresh",
              ghcache.acquire_lease() is False)
        # ...but an expired one must not block forever, or a worker killed mid-refresh would
        # wedge the cache until someone deleted the file by hand.
        write_json(ghcache.LOCK_PATH,
                   {"owner_pid": os.getpid() + 99999, "expires_at": time.time() - 1})
        check("an expired lease does not block", ghcache.acquire_lease() is True)
        ghcache.release_lease()
        check("releasing clears the owner",
              read_json(ghcache.LOCK_PATH, {}).get("owner_pid") is None)

        return 1 if fail_count else 0
    finally:
        for c in (outsider, member_client):
            if isinstance(c, httpx.Client):
                c.close()
        r = put_policy(saved_policy)
        print(f"\nauth.key_policy restored (HTTP {r.status_code})")
        # Restore the operator's cache. The policy write above already invalidated whatever
        # this run left behind, so this puts back only what was found at startup.
        if backup.exists():
            if ghcache.CACHE_DIR.exists():
                shutil.rmtree(ghcache.CACHE_DIR)
            shutil.move(str(backup), str(ghcache.CACHE_DIR))
            print("data/github restored from the pre-run backup")
        client.close()
        print(f"\nResult: {ok_count} passed, {fail_count} failed")


if __name__ == "__main__":
    sys.exit(main())
