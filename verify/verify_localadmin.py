"""Verify the local super administrator: sign-in, the forced-password-change gate, the
credential's storage, and the ways a live session can be downgraded.

This account is the way into the console where github.com is not reachable, and it is a
genuine super administrator -- so most of what is checked here is that it is *not* usable
while it still sits on the documented default password.

The script mutates auth.local_admin in config.yaml (that is where the credential lives) and
restores the original block at the end. The server holds its RouterConfig in a module
global, so a direct file write is followed by POST /v1/auth/local/enabled, which re-reads
config.yaml and reassigns it -- that is how the running process is made to see the change.
"""
import _bootstrap  # noqa: F401

import hashlib

import httpx

from app.config import load_raw, save_raw
from app.localadmin import DEFAULT_PASSWORD, DEFAULT_USERNAME, hash_password
from verify_auth_helper import BASE, make_client

NEW_PASSWORD = "verify-local-admin-1"
NEW_PASSWORD_2 = "verify-local-admin-2"
RENAMED = "verify-root-admin"

admin, _api_key, gh_login = make_client()
ORIGINAL = dict((load_raw().get("auth") or {}).get("local_admin") or {})


def section(title):
    print(f"\n=== {title} ===")


def stored() -> dict:
    """The local_admin block as it currently sits on disk."""
    return dict((load_raw().get("auth") or {}).get("local_admin") or {})


def write_local_admin(block: dict) -> None:
    """Write auth.local_admin straight to config.yaml, then make the server re-read it.

    POST /v1/auth/local/enabled is the reload lever: it reads config.yaml, sets `enabled`
    and reassigns the server's RouterConfig. Without it the process would keep serving the
    credential it loaded at startup.
    """
    auth_doc = dict(load_raw().get("auth") or {})
    auth_doc["local_admin"] = dict(block)
    save_raw({"auth": auth_doc})
    r = admin.post("/v1/auth/local/enabled", json={"enabled": bool(block.get("enabled", True))})
    r.raise_for_status()


def sign_in(username: str, password: str) -> tuple[httpx.Client, httpx.Response]:
    """Return (client carrying the resulting session, the login response)."""
    client = httpx.Client(base_url=BASE, timeout=60)
    r = client.post("/v1/auth/local/login", json={"username": username, "password": password})
    return client, r


# The default password has to be in force for the gate checks to mean anything, and a
# previous run of this script may have left one stored. Blanking the hash fields is the
# documented lockout-recovery path, so this is also what an operator would do.
if str(ORIGINAL.get("password_hash") or "").strip():
    print("a password is already stored; blanking it so the default is in force again")
write_local_admin({**ORIGINAL, "enabled": True, "username": DEFAULT_USERNAME,
                   "password_hash": "", "password_salt": "", "updated_at": None})

try:
    # 1. What the sign-in page is told, before anyone has signed in
    section("Public status")
    st = httpx.get(f"{BASE}/v1/auth/status", timeout=30).json()
    assert st["local_admin_enabled"] is True, st
    assert st["local_admin_username"] == DEFAULT_USERNAME, st["local_admin_username"]
    print(f"local_admin_enabled=True username={st['local_admin_username']} "
          f"(oauth configured={st['configured']})")

    # 2. Failed sign-ins are indistinguishable: a wrong username must not reveal that the
    #    account has been renamed.
    section("Failed sign-in")
    _c, bad_pw = sign_in(DEFAULT_USERNAME, "not-the-password")
    _c2, bad_user = sign_in("no-such-admin", DEFAULT_PASSWORD)
    assert bad_pw.status_code == 401 and bad_user.status_code == 401, (
        bad_pw.status_code, bad_user.status_code)
    assert bad_pw.json()["detail"] == bad_user.json()["detail"], "the two failures differ"
    assert "fmr_session" not in bad_pw.cookies, "a failed sign-in must not set a session"
    print("wrong password and wrong username -> 401 with one message:",
          bad_pw.json()["detail"])

    # 3. The default credential signs in, and says so
    section("Default sign-in, forced change pending")
    local, r = sign_in(DEFAULT_USERNAME, DEFAULT_PASSWORD)
    assert r.status_code == 200, (r.status_code, r.text)
    assert r.json()["must_change_password"] is True, r.json()
    st = local.get("/v1/auth/status").json()
    user = st["user"]
    assert st["authenticated"] and user["is_admin"] is True, user
    assert user["local_admin"] is True and user["must_change_password"] is True, user
    print(f"signed in as {user['login']}: is_admin={user['is_admin']} "
          f"must_change_password={user['must_change_password']}")

    # A *second* request is the regression test: auth.current_user recomputes is_admin on
    # every request, and resolving a local admin through admin_logins would demote it one
    # request after sign-in.
    again = local.get("/v1/auth/status").json()["user"]
    assert again["is_admin"] is True, "the second request demoted the local administrator"
    print("second request still is_admin (the recompute knows about auth.local_admin)")

    # A GitHub administrator must not be caught by the gate at all
    gh_user = admin.get("/v1/auth/status").json()["user"]
    assert not gh_user.get("must_change_password"), gh_user
    assert not gh_user.get("local_admin"), gh_user
    print(f"the GitHub administrator {gh_login} is unaffected by the gate")

    # 4. The gate: nothing but status / logout / change-password is reachable
    section("The forced-change gate")
    for path in ("/v1/config", "/v1/keys", "/v1/traces", "/v1/usage", "/v1/access/cache"):
        g = local.get(path)
        assert g.status_code == 403, (path, g.status_code)
        assert g.json()["detail"] == "password_change_required", (path, g.json())
    print("GET /v1/config, /v1/keys, /v1/traces, /v1/usage, /v1/access/cache -> 403 "
          "password_change_required")
    assert local.get("/v1/auth/status").status_code == 200, "status must stay reachable"

    # Reaching the change-password endpoint is proved by getting a *different* refusal from
    # it: 422 on the new password means the request got past the gate.
    weak = local.post("/v1/auth/local/password",
                      json={"current_password": DEFAULT_PASSWORD, "new_password": "short"})
    assert weak.status_code == 422, (weak.status_code, weak.text)
    print("POST /v1/auth/local/password is exempt (422 on the password, not 403 on the gate):",
          weak.json()["detail"])

    # Logout is exempt too, on a throwaway session so the main one survives
    spare, r = sign_in(DEFAULT_USERNAME, DEFAULT_PASSWORD)
    assert r.status_code == 200
    assert spare.post("/v1/auth/logout").status_code == 200, "logout must stay reachable"
    assert spare.get("/v1/auth/status").json()["authenticated"] is False
    print("logout is exempt: a gated session can still sign itself out")

    # 5. Which new passwords are refused
    section("New-password rules")
    for label, payload, code in (
        ("too short", {"current_password": DEFAULT_PASSWORD, "new_password": "abc123"}, 422),
        ("the default again",
         {"current_password": DEFAULT_PASSWORD, "new_password": DEFAULT_PASSWORD}, 422),
        ("empty username",
         {"current_password": DEFAULT_PASSWORD, "new_password": NEW_PASSWORD,
          "new_username": "   "}, 422),
        ("wrong current password",
         {"current_password": "wrong", "new_password": NEW_PASSWORD}, 403),
    ):
        rr = local.post("/v1/auth/local/password", json=payload)
        assert rr.status_code == code, (label, rr.status_code, rr.text)
        detail = rr.json()["detail"]
        assert detail != "password_change_required", (label, detail)
        print(f"  {label} -> {code}: {detail}")
    assert not str(stored().get("password_hash") or "").strip(), (
        "a refused change must not have written anything")
    print("nothing was written by any of the refusals")

    # A second session opened on the old password, to prove the change invalidates it
    doomed, r = sign_in(DEFAULT_USERNAME, DEFAULT_PASSWORD)
    assert r.status_code == 200, (r.status_code, r.text, stored())

    # 6. The change itself
    section("Changing the password")
    r = local.post("/v1/auth/local/password",
                   json={"current_password": DEFAULT_PASSWORD, "new_password": NEW_PASSWORD})
    assert r.status_code == 200, (r.status_code, r.text)
    assert r.json()["username"] == DEFAULT_USERNAME, r.json()
    print("POST /v1/auth/local/password -> 200")

    block = stored()
    assert block["password_hash"] and block["password_salt"], block
    assert block["updated_at"], "updated_at should record when the credential changed"
    digest = str(block["password_hash"])
    salt = str(block["password_salt"])
    assert digest != hashlib.sha256(NEW_PASSWORD.encode()).hexdigest(), (
        "the password is stored as a bare sha256 -- it must be salted scrypt")
    assert len(bytes.fromhex(salt)) >= 16, f"salt is only {len(salt) // 2} bytes"
    assert hash_password(NEW_PASSWORD, salt)[1] == digest, (
        "the stored digest is not scrypt(new password, stored salt)")
    # A second hash of the same password must land on a different salt, or the salt is fixed
    assert hash_password(NEW_PASSWORD)[0] != salt, "hash_password reused the salt"
    print(f"stored as salted scrypt: salt={len(bytes.fromhex(salt))} bytes, "
          f"digest={digest[:16]}… (not sha256, and re-salted each time)")

    # 7. The gate opens for the same session, without signing in again
    section("After the change")
    user = local.get("/v1/auth/status").json()["user"]
    assert user["must_change_password"] is False, user
    assert user["is_admin"] is True, user
    for path in ("/v1/config", "/v1/keys", "/v1/traces"):
        assert local.get(path).status_code == 200, (path, local.get(path).status_code)
    print("must_change_password=False; /v1/config, /v1/keys, /v1/traces -> 200")

    # Requirement 3: the local super administrator sees every user's traces
    items = local.get("/v1/traces", params={"limit": 200}).json()["items"]
    # A null user_id is legitimate -- a Copilot BYOK call carries no login -- so sort with a
    # key rather than sorted(), which cannot compare None against a string.
    owners = {s["user_id"] for s in items}
    assert gh_login in owners, f"the local admin should see {gh_login}'s traces, saw {owners}"
    shown = sorted(owners, key=lambda o: (o is None, o or ""))
    print(f"the local administrator sees traces from {len(owners)} user(s): {shown}")

    # Sessions opened with the old password are gone; the one that made the change is not
    assert doomed.get("/v1/auth/status").json()["authenticated"] is False, (
        "a session opened with the old password must not survive the change")
    print("the other session opened on the old password was dropped")

    # 8. The old password is dead, the new one works
    section("Old and new credentials")
    _c, old = sign_in(DEFAULT_USERNAME, DEFAULT_PASSWORD)
    assert old.status_code == 401, old.status_code
    fresh, r = sign_in(DEFAULT_USERNAME, NEW_PASSWORD)
    assert r.status_code == 200 and r.json()["must_change_password"] is False, r.text
    assert fresh.get("/v1/config").status_code == 200
    print("the default password -> 401; the new one signs straight into the console")

    # 9. Renaming the account (the requirement's "configure his own username")
    section("Renaming the account")
    r = local.post("/v1/auth/local/password", json={
        "current_password": NEW_PASSWORD,
        "new_password": NEW_PASSWORD_2,
        "new_username": RENAMED,
    })
    assert r.status_code == 200 and r.json()["username"] == RENAMED, r.text
    user = local.get("/v1/auth/status").json()["user"]
    assert user["login"] == RENAMED and user["is_admin"] is True, user
    print(f"the session that renamed the account follows it: login={user['login']} is_admin=True")
    _c, gone = sign_in(DEFAULT_USERNAME, NEW_PASSWORD_2)
    assert gone.status_code == 401, "the old username must stop working"
    renamed, r = sign_in(RENAMED, NEW_PASSWORD_2)
    assert r.status_code == 200, r.text
    print(f"{DEFAULT_USERNAME!r} -> 401, {RENAMED!r} -> 200")
    assert httpx.get(f"{BASE}/v1/auth/status", timeout=30).json()["local_admin_username"] == RENAMED
    print("the sign-in page advertises the new username")

    # 10. The digest never leaves the process, and no unrelated save can clear it
    section("The digest is not readable, and not clearable by accident")
    cfg_doc = admin.get("/v1/config").json()
    echoed = (cfg_doc.get("auth") or {}).get("local_admin") or {}
    assert echoed.get("password_hash") == "", echoed
    assert echoed.get("password_salt") == "", echoed
    assert stored()["password_hash"], "the file should still hold the digest"
    assert stored()["password_hash"] not in admin.get("/v1/config").text
    assert echoed.get("username") == RENAMED and echoed.get("updated_at"), echoed
    print("GET /v1/config blanks password_hash/salt while keeping username and updated_at")
    st = local.get("/v1/auth/status").text
    assert stored()["password_hash"] not in st and stored()["password_salt"] not in st
    print("GET /v1/auth/status carries neither the digest nor the salt")

    # Submitting only part of the auth section is refused rather than wiping the rest
    r = admin.put("/v1/config", json={"auth": {"key_policy": {"enabled": True}}})
    assert r.status_code == 422, (r.status_code, r.text)
    detail = r.json()["detail"]
    joined = " ".join(detail) if isinstance(detail, list) else str(detail)
    assert "auth.local_admin.password_hash" in joined, joined
    assert stored()["password_hash"], "the refused save must have left the digest alone"
    print("PUT /v1/config with only auth.key_policy -> 422 naming local_admin.password_hash")

    # ...and the console's own round-trip (which submits the blanked hash) preserves it
    before = stored()["password_hash"]
    r = admin.put("/v1/config", json=cfg_doc)
    assert r.status_code == 200, (r.status_code, r.text)
    assert stored()["password_hash"] == before, "a GET -> PUT round-trip cleared the password"
    assert renamed.get("/v1/config").status_code == 200, "the credential stopped working"
    print("GET -> PUT round-trip left the digest intact and the account working")

    # 11. Disabling the account downgrades sessions already issued to it
    section("Disabling the account")
    assert admin.post("/v1/auth/local/enabled", json={"enabled": False}).status_code == 200
    user = renamed.get("/v1/auth/status").json()["user"]
    assert user["is_admin"] is False, "disabling must downgrade a live local-admin session"
    r = renamed.get("/v1/config")
    assert r.status_code == 403 and r.json()["detail"] != "password_change_required", r.text
    print("a live session drops to is_admin=False and /v1/config -> 403")
    _c, denied = sign_in(RENAMED, NEW_PASSWORD_2)
    assert denied.status_code == 503, denied.status_code
    print("signing in while disabled -> 503:", denied.json()["detail"])
    assert httpx.get(f"{BASE}/v1/auth/status", timeout=30).json()["local_admin_enabled"] is False
    assert admin.post("/v1/auth/local/enabled", json={"enabled": True}).status_code == 200
    assert renamed.get("/v1/auth/status").json()["user"]["is_admin"] is True, (
        "re-enabling should restore the live session")
    print("re-enabled: the same session is an administrator again")

finally:
    # 12. Put config.yaml back the way it was found, and prove the restored state works
    section("Restoring config.yaml")
    write_local_admin(ORIGINAL)
    now = stored()
    assert now == ORIGINAL, (now, ORIGINAL)
    print("auth.local_admin restored:", {k: (v if k != "password_hash" else bool(v))
                                         for k, v in now.items()})

if not str(ORIGINAL.get("password_hash") or "").strip() and ORIGINAL.get("enabled", True):
    # The original state was "default password in force", so it must sign in and be gated
    back, r = sign_in(str(ORIGINAL.get("username") or DEFAULT_USERNAME), DEFAULT_PASSWORD)
    assert r.status_code == 200 and r.json()["must_change_password"] is True, r.text
    assert back.get("/v1/config").status_code == 403
    back.post("/v1/auth/logout")
    print("the restored default credential signs in and is gated again")

print("\nALL LOCAL ADMIN CHECKS PASSED")
