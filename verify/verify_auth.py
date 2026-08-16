"""Verify authentication, user_id attribution, multi-provider binding, and admin/normal-user
privilege separation."""
import _bootstrap  # noqa: F401

import httpx

from app.authstore import AuthStore
from app.config import DATA_DIR, load_raw
from verify_auth_helper import BASE, make_client

client, api_key, login = make_client()
store = AuthStore(DATA_DIR)


def section(title):
    print(f"\n=== {title} ===")


# 1. API keys are mandatory
section("API key enforcement")
body = {"messages": [{"role": "user", "content": "hi"}], "max_tokens": 20}
r = httpx.post(f"{BASE}/v1/chat/completions", json=body, timeout=60)
assert r.status_code == 401, r.status_code
print("no key -> 401:", r.json()["detail"][:40], "…")

r = httpx.post(
    f"{BASE}/v1/chat/completions", json=body, timeout=60,
    headers={"Authorization": "Bearer fmr_totally-bogus"},
)
assert r.status_code == 401
print("forged key -> 401:", r.json()["detail"])

r = httpx.get(f"{BASE}/v1/models", timeout=60)
assert r.status_code == 401
print("GET /v1/models without a key -> 401")

# All three ways of passing the key must be accepted
for header in ({"Authorization": f"Bearer {api_key}"}, {"api-key": api_key}, {"x-api-key": api_key}):
    r = httpx.get(f"{BASE}/v1/models", headers=header, timeout=60)
    assert r.status_code == 200, (header, r.status_code)
print("Bearer / api-key / x-api-key all accepted -> 200")

# 2. user_id attribution (Copilot BYOK sends no x-user-id, and forging one has no effect)
section("user_id attribution")
r = client.post(
    "/v1/chat/completions", json=body,
    headers={"x-user-id": "someone-else"},  # a forged value must be ignored
)
r.raise_for_status()
tid = r.headers["x-trace-id"]
t = client.get(f"/v1/traces/{tid}").json()
assert t["user_id"] == login, f"user_id should come from the key owner, got {t['user_id']}"
assert t["user_id"] is not None, "user_id must not be null"
assert t["api_key_id"] and t["api_key_name"]
print(f"x-user-id: someone-else ignored; trace user_id={t['user_id']} (the key owner)")
print("api_key:", t["api_key_name"], t["api_key_id"])

# 3. A disabled key stops working immediately
section("Key lifecycle")
record, tmp_key = store.create_api_key(login, "verify-temp")
r = httpx.get(f"{BASE}/v1/models", headers={"Authorization": f"Bearer {tmp_key}"}, timeout=60)
assert r.status_code == 200, "a newly created key must work at once (cross-process mtime re-read)"
print("key created in another process works immediately -> 200")
client.patch(f"/v1/keys/{record['id']}", json={"disabled": True})
r = httpx.get(f"{BASE}/v1/models", headers={"Authorization": f"Bearer {tmp_key}"}, timeout=60)
assert r.status_code == 401, "disabling must take effect immediately"
print("disabled key -> 401")
assert client.delete(f"/v1/keys/{record['id']}").status_code == 200
r = httpx.get(f"{BASE}/v1/models", headers={"Authorization": f"Bearer {tmp_key}"}, timeout=60)
assert r.status_code == 401
print("deleted key -> 401")

# 3b. A key stays readable to its owner: the listing carries the plaintext, and that plaintext
# still authenticates -- i.e. it is the real credential and not a re-derived look-alike.
section("Owner can read their own key back")
own_rec, own_key = store.create_api_key(login, "verify-readback")
try:
    mine = client.get("/v1/keys").json()
    row = next(k for k in mine if k["id"] == own_rec["id"])
    assert row.get("key") == own_key, "the owner's own listing must carry the plaintext"
    assert "key_hash" not in row, "the digest must never leave the process"
    r = httpx.get(f"{BASE}/v1/models", headers={"Authorization": f"Bearer {row['key']}"}, timeout=60)
    assert r.status_code == 200, "the key read back from the listing must authenticate"
    print("owner's /v1/keys returns the plaintext, and it authenticates -> 200")

    # A record written before plaintext was stored: it must still list (with no "key") and still
    # work, because the digest is what lookup compares.
    legacy_id = own_rec["id"]
    with store._lock:  # noqa: SLF001 deliberately simulating an old on-disk record
        store._keys[legacy_id].pop("key", None)
        store._save_keys()
    mine = client.get("/v1/keys").json()
    row = next(k for k in mine if k["id"] == legacy_id)
    assert "key" not in row, "a pre-existing key has no plaintext to hand out"
    r = httpx.get(f"{BASE}/v1/models", headers={"Authorization": f"Bearer {own_key}"}, timeout=60)
    assert r.status_code == 200, "a key without a stored plaintext must keep working"
    print("legacy key (hash only) lists without a plaintext and still authenticates -> 200")
finally:
    client.delete(f"/v1/keys/{own_rec['id']}")

# 4. Multiple providers: the provider a model is bound to decides the backend address
section("Multi-provider binding")
providers = load_raw().get("providers") or {}
assert len(providers) >= 2, f"this check needs at least two providers, currently {list(providers)}"
t = client.get(f"/v1/traces/{tid}").json()
bound = (load_raw()["models"][t["routing"]["model"]] or {}).get("provider")
assert t["backend"]["provider"] == bound, (t["backend"]["provider"], bound)
assert t["backend"]["base_url"] == providers[bound]["base_url"]
assert t["backend"]["api_type"] == providers[bound].get("api_type")
print(f"model {t['routing']['model']} -> provider={t['backend']['provider']} "
      f"base_url={t['backend']['base_url']} api_type={t['backend']['api_type']}")
assert providers[bound]["api_key"] not in str(t), "a trace must not contain the provider's plaintext key"
print("trace did not leak the provider's plaintext key OK")

# 5. Privilege separation: a normal user
section("Administrator / normal-user separation")
normal_login = "verify-normal-user"
admins = [str(x) for x in ((load_raw().get("auth") or {}).get("admin_logins") or [])]
assert normal_login not in admins
nsid = store.create_session(
    {"login": normal_login, "name": normal_login, "avatar_url": None, "is_admin": False}, 3600
)
_nrec, nkey = store.create_api_key(normal_login, "verify-normal")
normal = httpx.Client(
    base_url=BASE, timeout=300, cookies={"fmr_session": nsid},
    headers={"Authorization": f"Bearer {nkey}"},
)

st = normal.get("/v1/auth/status").json()
assert st["authenticated"] and st["user"]["is_admin"] is False
print("normal-user session has is_admin=False")

assert normal.get("/v1/config").status_code == 403
assert normal.put("/v1/config", json={"strategy": "ai"}).status_code == 403
print("normal user GET/PUT /v1/config -> 403")

r = normal.post("/v1/chat/completions", json=body)
r.raise_for_status()
ntid = r.headers["x-trace-id"]
nt = normal.get(f"/v1/traces/{ntid}").json()
assert nt["user_id"] == normal_login
print(f"normal user's request attributed to user_id={nt['user_id']}")

# /v1/traces returns a page object, not a bare list: {total, items, offset, limit, truncated}
page = normal.get("/v1/traces", params={"limit": 100}).json()
own = page["items"]
assert own and all(s["user_id"] == normal_login for s in own), "a normal user must only see their own traces"
assert page["total"] >= len(own), (page["total"], len(own))
print(f"normal user's /v1/traces returned only their own {len(own)}/{page['total']} entries")
# Explicitly asking for someone else's user_id must be overridden
spoof = normal.get("/v1/traces", params={"user_id": login}).json()["items"]
assert all(s["user_id"] == normal_login for s in spoof), "a normal user must not see others' traces"
print("normal user passing user_id=<admin> is still forced back to their own")
assert normal.get(f"/v1/traces/{tid}").status_code == 404, "another user's trace detail should 404"
print("normal user reading another user's trace detail -> 404")

u = normal.get("/v1/usage", params={"days": 1}).json()
assert u["scope"] == normal_login and u["is_admin"] is False
assert all(x["user_id"] == normal_login for x in u["by_user"])
print(f"normal user's /v1/usage scope={u['scope']} requests={u['totals']['requests']}")

keys = normal.get("/v1/keys", params={"all": True}).json()
assert all(k["user_login"] == normal_login for k in keys), "all=1 must not let a normal user see every key"
print("normal user's /v1/keys?all=1 still returns only their own keys")

# 6. The administrator view
section("Administrator view")
au = client.get("/v1/usage", params={"days": 1}).json()
assert au["is_admin"] and au["scope"] == "all"
users = {x["user_id"] for x in au["by_user"]}
assert {login, normal_login} <= users, users
print("admin /v1/usage scope=all, users covered:", sorted(users))
ascoped = client.get("/v1/usage", params={"days": 1, "user_id": normal_login}).json()
assert ascoped["scope"] == normal_login
print("admin can drill down by user_id:", ascoped["scope"], ascoped["totals"]["requests"], "requests")
akeys = client.get("/v1/keys", params={"all": True}).json()
assert {k["user_login"] for k in akeys} >= {login, normal_login}
assert all("key_hash" not in k and "key" not in k for k in akeys), "the list must not return key plaintext/hash"
print(f"admin /v1/keys?all=1 -> {len(akeys)} entries, with no plaintext/hash")
assert client.get(f"/v1/traces/{ntid}").status_code == 200
print("admin can read any user's trace detail")

# 7. The bootstrap endpoint closes once OAuth is configured
section("Bootstrap endpoint")
r = httpx.post(f"{BASE}/v1/auth/setup", json={"client_id": "x", "client_secret": "y"}, timeout=30)
cfg_auth = (load_raw().get("auth") or {}).get("github") or {}
if cfg_auth.get("client_id"):
    assert r.status_code == 409, r.status_code
    print("OAuth already configured -> POST /v1/auth/setup 409")
else:
    print("OAuth not configured -> setup is available (localhost), returned", r.status_code)

# Clean up the temporary credentials this run created
store.delete_session(nsid)
for k in store.list_api_keys(normal_login):
    store.delete_api_key(k["id"])
print("\nALL AUTH CHECKS PASSED")
