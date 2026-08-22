"""Verify who may narrow an API key's scope (app/scopepolicy.py, and the gate in app/main.py).

A key scope can only ever subtract from what its owner may reach, so this is not a privilege
control and none of the assertions here are about escalation. It is a **cost** control: a user who
scopes a key to the single most expensive model has pinned every request on that key to it. So the
properties worth asserting are the ones a reasonable implementation gets wrong:

1. **Deny is the default, and it is three different denials.** Absent, `enabled: false`, and
   enabled-with-three-empty-lists all refuse, each with its own reason, and none of them reads as
   "allow all". Enabled-and-nothing-listed is the same trap as an enabled key policy with no
   token, and it is the state an administrator lands in the moment they flip the switch.
2. **The levels are combined with AND, not the OR that app/keypolicy.py uses**, while a level left
   empty *abstains* rather than denying. Those two rules only make sense together: under a strict
   "match all three" an administrator who lists one organization would grant nobody anything.
3. **`owner` and `actor` are different people.** The scope is checked against the owner's model
   policy and the permission against the caller, so an administrator narrowing somebody else's
   key exercises their own authority. Asserted by having the administrator patch the test user's
   key while the policy denies that user.
4. **Widening is never refused**, even with the permission withdrawn, or a key narrowed while the
   permission existed would be trapped in that shape forever.
5. **A malformed scope is a 400 and a forbidden scope is a 403.** Two different problems for the
   user, and collapsing them into one status is how "select at least one model" ends up being
   reported as "your administrator has not allowed you".

The first half runs app/scopepolicy.py in-process against a stub configuration and a stub
app/ghcache.py, because the team and organization levels are otherwise only reachable through real
GitHub membership: the AND, the abstention, the fail-closed lookup and the malformed team key are
all asserted there, deterministically and with no network. The second half then asserts that the
HTTP endpoints actually consult it, on both `POST /v1/keys` and `PATCH /v1/keys/{id}`.

**This script rewrites `auth` for the duration of the run and restores it in a `finally`**, the
pattern verify_modelpolicy.py uses for `model_policy`. Two notes on that. It switches
`auth.key_policy.enabled` off, because the key-creation gate runs *before* the scope gate and the
synthetic non-administrator below belongs to no GitHub organization, so with it on every POST
would 403 for the wrong reason. And changing `key_policy` deletes the on-disk GitHub cache under
`data/github/` (app/main.py invalidates it on purpose, since a policy or token change must not
leave stale member lists authoritative), so this run costs one cache rebuild by the background
refresh loop. Nothing is lost that was not a cache.

Run verify_stub_upstream.py first if no real backend is reachable.
"""
import _bootstrap  # noqa: F401

import asyncio
import io
import json
import pathlib
import types

import httpx

from app import keypolicy, scopepolicy
from app.authstore import AuthStore
from app.config import DATA_DIR, load_raw
from verify_auth_helper import BASE, make_client

client, _admin_key, admin_login = make_client()
store = AuthStore(DATA_DIR)

ok = fail = 0


def check(cond, label, extra=""):
    global ok, fail
    if cond:
        ok += 1
        print(f"[OK  ] {label} {extra}")
    else:
        fail += 1
        print(f"[FAIL] {label} {extra}")


def section(title):
    print(f"\n=== {title} ===")


# ==============================================================================
# Part 1 -- the policy itself, in-process
# ==============================================================================
# A stub configuration, so the three levels can be set to exactly the combination under test.
class Cfg:
    def __init__(self, **policy):
        self.key_scope_policy = dict(policy)

    @property
    def key_scope_policy_enabled(self) -> bool:
        return bool(self.key_scope_policy.get("enabled", False))


# A stub ghcache. Team and organization membership are otherwise only reachable through real
# GitHub data, which would make the AND assertions depend on the state of somebody's enterprise.
# "boom" raises, which is how the fail-closed path is reached without breaking anything.
ORG_MEMBERS = {"acme": {"alice"}}
TEAM_MEMBERS = {("ent", "42"): {"alice"}}


async def _is_org_member(cfg, org, login):
    if org == "boom":
        raise RuntimeError("GitHub is unreachable")
    return login in ORG_MEMBERS.get(org, set()), "cache"


async def _is_team_member(cfg, slug, team_id, login):
    if slug == "boom":
        raise RuntimeError("GitHub is unreachable")
    return login in TEAM_MEMBERS.get((slug, team_id), set()), "cache"


real_ghcache = scopepolicy.ghcache
scopepolicy.ghcache = types.SimpleNamespace(
    is_org_member=_is_org_member, is_team_member=_is_team_member
)


def ev(cfg, login="alice", is_admin=False) -> dict:
    return asyncio.run(scopepolicy.evaluate(cfg, login, is_admin))


def rows(verdict) -> dict:
    return {row["level"]: row for row in verdict["levels"]}


try:
    section("Deny is the default, and the three denials are distinguishable")
    v = ev(Cfg())
    check(v["allowed"] is False and v["policy_enabled"] is False,
          "no key_scope_policy at all: denied", v["reason"][:48])
    v = ev(Cfg(enabled=False, users=["alice"]))
    check(v["allowed"] is False and "not enabled" in v["reason"],
          "enabled: false denies even a listed user -- the switch is the outer gate")
    v = ev(Cfg(enabled=True))
    check(v["allowed"] is False and v["policy_enabled"] is True
          and "has not allowed" in v["reason"] and v["levels"] == [],
          "enabled with three empty lists: denied, and says why rather than reading as allow-all")

    section("Administrators are exempt, whatever the policy says")
    v = ev(Cfg(enabled=True, users=["alice"]), login="bob", is_admin=True)
    check(v["allowed"] is True and v["levels"] == [],
          "an administrator passes without any level being consulted")
    v = ev(Cfg(enabled=False), login="bob", is_admin=True)
    check(v["allowed"] is True and v["policy_enabled"] is False,
          "and passes while the control is switched off, so a bad save cannot lock them out")

    section("One level: OR within it, and the match is case-insensitive")
    v = ev(Cfg(enabled=True, users=["Alice"]))
    check(v["allowed"] is True and rows(v)["user"]["matched"] == "Alice",
          "a login matches its differently-cased entry")
    check(rows(v)["team"]["configured"] is False and rows(v)["team"]["passed"] is True
          and rows(v)["organization"]["passed"] is True,
          "and the two empty levels abstain rather than denying")
    v = ev(Cfg(enabled=True, users=["alice"]), login="bob")
    check(v["allowed"] is False and "the user level" in v["reason"],
          "an unlisted login is denied, and the reason names the level to ask about")
    v = ev(Cfg(enabled=True, organizations=["nope", "acme"]))
    check(v["allowed"] is True and rows(v)["organization"]["matched"] == "acme",
          "any single entry in a level is enough")
    v = ev(Cfg(enabled=True, organizations=["acme"]), login="carol")
    check(v["allowed"] is False and "the organization level" in v["reason"],
          "a non-member of the only listed organization is denied")
    v = ev(Cfg(enabled=True, teams=["ent/42"]))
    check(v["allowed"] is True and rows(v)["team"]["matched"] == "ent/42",
          "the team level resolves an '<enterprise>/<team id>' entry")
    v = ev(Cfg(enabled=True, teams=["ent/99"]))
    check(v["allowed"] is False, "and a team the caller is not on denies")

    section("Across levels it is AND, which is the opposite of the key policy")
    v = ev(Cfg(enabled=True, users=["alice"], organizations=["acme"]))
    check(v["allowed"] is True, "both configured levels match: allowed")
    v = ev(Cfg(enabled=True, users=["alice"], organizations=["other"]))
    check(v["allowed"] is False and "the organization level" in v["reason"],
          "one level matching is NOT enough -- an OR here would have allowed this")
    check(rows(v)["user"]["passed"] is True and rows(v)["organization"]["configured"] is True
          and rows(v)["organization"]["passed"] is False,
          "and the evidence shows which level failed, not just that something did")
    v = ev(Cfg(enabled=True, users=["bob"], teams=["12"]))
    check(v["allowed"] is False and "the user and team level" in v["reason"],
          "two failing levels are both named", v["reason"][-92:])
    v = ev(Cfg(enabled=True, organizations=["acme"]))
    check(v["allowed"] is True and rows(v)["user"]["configured"] is False,
          "listing only organizations allows everybody in them -- the abstention rule is what "
          "keeps that usable")

    section("Failures deny the level they belong to (fail-closed)")
    # A failed level reports `matched: ""` and `source: ""` whatever went wrong, so the verdict
    # does not distinguish "not a member" from "that organization does not exist" from "GitHub
    # would not answer". Deliberate: the row is shown to the person being denied, and the
    # difference is an administrator's problem, which is why it goes to the log instead.
    v = ev(Cfg(enabled=True, teams=["42"]))
    check(v["allowed"] is False and rows(v)["team"]["matched"] == "",
          "a teams entry that is not '<enterprise>/<team id>' denies rather than being skipped")
    v = ev(Cfg(enabled=True, organizations=["boom"]))
    check(v["allowed"] is False and "the organization level" in v["reason"],
          "a membership lookup that raises denies, it does not pass")
    v = ev(Cfg(enabled=True, teams=["boom/1"]))
    check(v["allowed"] is False and "the team level" in v["reason"],
          "same for the team lookup")
    v = ev(Cfg(enabled=True, users=["alice"], teams=["boom/1"]))
    check(v["allowed"] is False,
          "and one unanswerable level is enough to deny somebody the other levels allow")
    v = ev(Cfg(enabled=True, organizations=["boom"]), login="alice", is_admin=True)
    check(v["allowed"] is True, "but an administrator is decided before any lookup runs")

    section("Every verdict names itself with a code, and every code is in all five catalogs")
    # The English sentence is the record: it goes into the log lines and the 403 bodies, where the
    # reader's locale is unknown. The console shows the same verdict from its catalogs, keyed on
    # the code, so what has to be asserted is that no branch is missing one and no code is missing
    # a string. A new branch whose code nobody translated falls back to English *silently*, which
    # is invisible to every assertion above about the English text.
    scope_cases = {
        "admin": ev(Cfg(enabled=True, users=["alice"]), login="bob", is_admin=True),
        "off": ev(Cfg(enabled=False, users=["alice"])),
        "nobodyAllowed": ev(Cfg(enabled=True)),
        "levelsFailed": ev(Cfg(enabled=True, users=["bob"], teams=["12"])),
        "allowed": ev(Cfg(enabled=True, users=["alice"])),
    }
    wrong = {
        expected: v.get("reason_code")
        for expected, v in scope_cases.items()
        if v.get("reason_code") != expected
    }
    check(not wrong, "each key-scope branch returns its own code", wrong)
    check(
        scope_cases["levelsFailed"]["reason_params"]["levels"] == ["user", "team"],
        "and the failing levels travel as names, for the console to name and order itself",
        scope_cases["levelsFailed"]["reason_params"],
    )
    check(
        all(v["reason_params"] == {} for k, v in scope_cases.items() if k != "levelsFailed"),
        "while the branches whose sentence interpolates nothing carry no parameters",
    )

    # The key-creation policy, for the four branches that need no GitHub data. The three
    # membership ones are read off the module's own map rather than provoked, because reaching
    # them means being a member of somebody's real enterprise.
    class KeyCfg:
        def __init__(self, **policy):
            self.key_policy = dict(policy)

    key_cases = {
        "admin": asyncio.run(keypolicy.evaluate(KeyCfg(), "alice", True)),
        "policyOff": asyncio.run(keypolicy.evaluate(KeyCfg(enabled=False), "alice", False)),
        "noToken": asyncio.run(keypolicy.evaluate(KeyCfg(enabled=True), "alice", False)),
        "noEnterprise": asyncio.run(
            keypolicy.evaluate(KeyCfg(enabled=True, github_token="x"), "alice", False)
        ),
    }
    wrong = {
        expected: v.get("reason_code")
        for expected, v in key_cases.items()
        if v.get("reason_code") != expected
    }
    check(not wrong, "each key-creation branch returns its own code too", wrong)

    key_codes = set(key_cases) | set(keypolicy._MEMBER_CODES.values()) | {
        "member", "noMembership",
    }
    locales = pathlib.Path("frontend/src/i18n/locales")
    missing: dict[str, list[str]] = {}
    for path in sorted(locales.glob("*.json")):
        doc = json.load(io.open(path, encoding="utf-8"))
        gaps = [
            "keys.access.reason." + c
            for c in sorted(key_codes)
            if c not in ((doc["keys"]["access"].get("reason")) or {})
        ] + [
            "keys.scope.verdict." + c
            for c in sorted(scope_cases)
            if c not in ((doc["keys"]["scope"].get("verdict")) or {})
        ] + [
            "keys.scope.levelName." + lvl
            for lvl in scopepolicy.LEVELS
            if lvl not in ((doc["keys"]["scope"].get("levelName")) or {})
        ]
        if gaps:
            missing[path.stem] = gaps
    check(not missing, "and every code has a string in every catalog", missing)
finally:
    scopepolicy.ghcache = real_ghcache

# ==============================================================================
# Part 2 -- the endpoints consult it
# ==============================================================================
# A synthetic non-administrator: the policy exempts administrators, so the login make_client()
# returns cannot be the subject of a single denial assertion.
USER = "verify-scope-user"
raw = load_raw()
admins = [str(x).strip().lower() for x in ((raw.get("auth") or {}).get("admin_logins") or [])]
assert USER not in admins, "the subject of these checks must not be an administrator"

user_sid = store.create_session(
    {"login": USER, "name": USER, "avatar_url": None, "is_admin": False}, 3600
)
for record in store.list_api_keys(USER):
    store.delete_api_key(record["id"])
user = httpx.Client(base_url=BASE, timeout=300, cookies={"mr_session": user_sid})

doc = client.get("/v1/config").json()
MODELS = doc.get("models") or {}
CATALOG = list(MODELS)
assert CATALOG, "these checks need at least one model in the catalog"
MODEL = next((n for n, m in MODELS.items() if (m or {}).get("default")), CATALOG[0])
API_TYPE = str(
    ((doc.get("providers") or {}).get((MODELS[MODEL] or {}).get("provider")) or {}).get("api_type")
    or "openai"
)
NARROW_TYPES = {"kind": "api_types", "api_types": [API_TYPE]}
NARROW_MODELS = {"kind": "models", "models": [MODEL]}
ALL = {"kind": "all"}
print(f"\nsubject={USER}  admin={admin_login}  model={MODEL}  connection type={API_TYPE}")

saved = {"auth": doc.get("auth")}
created: list[str] = []


def apply_auth(**fields) -> httpx.Response:
    """PUT the whole configuration back with `auth` merged, the way the console saves it.

    The whole document, because save_raw replaces top-level keys; and `auth` merged rather than
    replaced, because _dropped_auth_credentials refuses a submission that would wipe the OAuth
    secret or the enterprise token out of it.
    """
    current = client.get("/v1/config").json()
    current["auth"] = {**(current.get("auth") or {}), **fields}
    return client.put("/v1/config", json=current)


def restore_auth() -> httpx.Response:
    """Put the saved `auth` section back verbatim, replacing rather than merging.

    apply_auth merges, so it can set a key but never remove one: a deployment whose config.yaml
    carries no key_scope_policy at all would be left with whatever the last check set. Replacing
    is safe here because `saved` came from GET /v1/config, which serves an administrator the
    credentials in cleartext, so nothing _dropped_auth_credentials protects goes missing.
    """
    current = client.get("/v1/config").json()
    current["auth"] = saved["auth"]
    return client.put("/v1/config", json=current)


def create(cli, name, scope=None) -> httpx.Response:
    body = {"name": name}
    if scope is not None:
        body["scope"] = scope
    r = cli.post("/v1/keys", json=body)
    if r.status_code == 200:
        created.append(r.json()["id"])
    return r


def key_scope_of(cli) -> dict:
    return cli.get("/v1/access/me").json()["key_scope"]


try:
    # key_policy off for the run: it gates key *creation*, runs before the scope gate, and would
    # deny this synthetic login for an unrelated reason. See the module docstring on the cache.
    apply_auth(
        key_policy={**((saved["auth"] or {}).get("key_policy") or {}), "enabled": False},
        key_scope_policy=None,
    ).raise_for_status()

    section("With no policy configured, a key covers everything and cannot be narrowed")
    me = user.get("/v1/access/me").json()
    check("allowed" in me and me["key_scope"]["allowed"] is False
          and me["key_scope"]["policy_enabled"] is False,
          "/v1/access/me carries the scope verdict separately from the key-creation one",
          "create=%s scope=%s" % (me.get("allowed"), me["key_scope"]["allowed"]))
    r = create(user, "verify-scope-default")
    check(r.status_code == 200 and r.json()["scope"] == ALL,
          "creating a key still works, and it covers all models and all connection types",
          str(r.status_code))
    key_id = r.json()["id"]
    r = create(user, "verify-scope-denied", NARROW_TYPES)
    check(r.status_code == 403 and "not enabled" in r.json()["detail"],
          "POST /v1/keys with a connection-type scope: 403", str(r.status_code))
    r = create(user, "verify-scope-denied", NARROW_MODELS)
    check(r.status_code == 403,
          "POST /v1/keys with a model scope: 403, so the gate is ahead of the model check",
          str(r.status_code))
    r = user.patch(f"/v1/keys/{key_id}", json={"scope": NARROW_TYPES})
    check(r.status_code == 403, "PATCH is gated too, not just create", str(r.status_code))
    r = user.patch(f"/v1/keys/{key_id}", json={"scope": ALL})
    check(r.status_code == 200 and r.json()["scope"] == ALL,
          "but setting the scope to 'all' is accepted while narrowing is refused",
          str(r.status_code))
    r = user.patch(f"/v1/keys/{key_id}", json={"name": "verify-scope-renamed"})
    check(r.status_code == 200 and r.json()["name"] == "verify-scope-renamed",
          "and a PATCH that does not touch the scope is not gated at all")

    section("Switched on with nothing listed is a different denial, not a grant")
    apply_auth(key_scope_policy={"enabled": True}).raise_for_status()
    v = key_scope_of(user)
    check(v["allowed"] is False and v["policy_enabled"] is True
          and "has not allowed" in v["reason"],
          "the verdict distinguishes off from on-but-nobody-listed", v["reason"][:56])
    check(create(user, "verify-scope-denied", NARROW_TYPES).status_code == 403,
          "and the endpoint still refuses")

    section("Listed at the user level: the scope goes through, and it bites")
    apply_auth(key_scope_policy={"enabled": True, "users": [USER]}).raise_for_status()
    v = key_scope_of(user)
    levels = {row["level"]: row for row in v["levels"]}
    check(v["allowed"] is True, "the verdict flips to allowed", v["reason"][:52])
    check(levels["user"]["passed"] is True and levels["team"]["configured"] is False
          and levels["organization"]["configured"] is False,
          "and the two empty levels are reported as not consulted")
    r = create(user, "verify-scope-types", NARROW_TYPES)
    check(r.status_code == 200 and r.json()["scope"] == NARROW_TYPES,
          "POST /v1/keys with a connection-type scope: accepted", str(r.status_code))
    narrowed_id = r.json()["id"] if r.status_code == 200 else key_id
    narrowed_key = r.json().get("key", "")
    r = user.patch(f"/v1/keys/{narrowed_id}", json={"scope": NARROW_MODELS})
    check(r.status_code == 200 and r.json()["scope"] == NARROW_MODELS,
          "PATCH to an explicit model list: accepted", str(r.status_code))
    scoped = httpx.Client(base_url=BASE, timeout=300,
                          headers={"Authorization": "Bearer " + narrowed_key})
    ids = [m["id"] for m in scoped.get("/v1/models").json()["data"]]
    check(ids == [MODEL],
          "and the key it produced really is narrowed: /v1/models returns one model", str(ids))
    scoped.close()

    section("A malformed scope is a 400, not a 403 -- two different problems")
    r = create(user, "verify-scope-bad", {"kind": "models", "models": ["no-such-model-xyz"]})
    check(r.status_code == 400 and "unknown model" in r.json()["detail"],
          "an unknown model name", str(r.status_code))
    r = create(user, "verify-scope-bad", {"kind": "api_types", "api_types": ["nope"]})
    check(r.status_code == 400, "an unknown connection type", str(r.status_code))
    r = create(user, "verify-scope-bad", {"kind": "models", "models": []})
    check(r.status_code == 400, "an empty selection", str(r.status_code))

    section("A second configured level must also match (AND)")
    # The organization entry is one this login cannot match. Whether GitHub answers "not a
    # member" or cannot answer at all does not matter here: both deny, and that the level is
    # consulted at all is the property being asserted.
    apply_auth(key_scope_policy={
        "enabled": True, "users": [USER], "organizations": ["verify-no-such-org"],
    }).raise_for_status()
    v = key_scope_of(user)
    levels = {row["level"]: row for row in v["levels"]}
    check(v["allowed"] is False and "the organization level" in v["reason"],
          "adding an organization the user is not in takes the permission away again",
          v["reason"][:64])
    check(levels["user"]["passed"] is True and levels["organization"]["configured"] is True
          and levels["organization"]["passed"] is False,
          "the user level still passes, so this is the AND and not a lost user entry")
    check(create(user, "verify-scope-denied", NARROW_TYPES).status_code == 403,
          "and the endpoint refuses again")

    section("Administrators are exempt, and it is the caller who is checked, not the owner")
    r = create(client, "verify-scope-admin", NARROW_MODELS)
    check(r.status_code == 200 and r.json()["scope"] == NARROW_MODELS,
          "the administrator narrows their own key while the policy denies everybody",
          str(r.status_code))
    r = client.patch(f"/v1/keys/{narrowed_id}", json={"scope": NARROW_TYPES})
    check(r.status_code == 200 and r.json()["scope"] == NARROW_TYPES,
          "and narrows the user's key, exercising their own permission rather than the owner's",
          str(r.status_code))

    section("Withdrawing the permission does not trap the keys already narrowed")
    apply_auth(key_scope_policy=None).raise_for_status()
    mine = {k["id"]: k for k in user.get("/v1/keys").json()}
    check(mine[narrowed_id]["scope"] == NARROW_TYPES,
          "the restriction is still on the key: taking the permission away rewrites nothing")
    r = user.patch(f"/v1/keys/{narrowed_id}", json={"scope": ALL})
    check(r.status_code == 200 and r.json()["scope"] == ALL,
          "the owner can always clear it back to everything", str(r.status_code))
    r = user.patch(f"/v1/keys/{narrowed_id}", json={"scope": NARROW_MODELS})
    check(r.status_code == 403,
          "but not swap it for a different restriction, the direction that costs money",
          str(r.status_code))

    section("The stored configuration survives the round trip")
    apply_auth(key_scope_policy={
        "enabled": True, "users": [USER], "teams": ["ent/42"], "organizations": ["acme"],
    }).raise_for_status()
    stored = (client.get("/v1/config").json().get("auth") or {}).get("key_scope_policy") or {}
    check(stored.get("enabled") is True and stored.get("users") == [USER]
          and stored.get("teams") == ["ent/42"] and stored.get("organizations") == ["acme"],
          "all three lists come back through YAML unchanged", str(stored))
    r = apply_auth(key_scope_policy={"enabled": "yes"})
    check(r.status_code == 422, "a non-boolean enabled is refused", str(r.status_code))
    r = apply_auth(key_scope_policy={"enabled": True, "users": "alice"})
    check(r.status_code == 422, "so is a level that is not a list", str(r.status_code))
    r = apply_auth(key_scope_policy=[USER])
    check(r.status_code == 422, "so is a policy that is not an object", str(r.status_code))
    after = (client.get("/v1/config").json().get("auth") or {}).get("key_scope_policy") or {}
    check(after == stored, "and none of the three rejected saves changed what is stored")

    section("The endpoint the page reads carries the codes, not only the sentences")
    # Both verdicts come from GET /v1/access/me, which is what the Keys page renders. The English
    # sentence stays in the payload for API callers and is what the 403 bodies above assert on;
    # the code is what the console translates.
    apply_auth(key_scope_policy=None).raise_for_status()
    me = user.get("/v1/access/me").json()
    check(me.get("reason_code") == "policyOff" and me["key_scope"]["reason_code"] == "off",
          "with both controls off, both verdicts name their branch",
          "%s / %s" % (me.get("reason_code"), me["key_scope"]["reason_code"]))
    check(bool(me.get("reason")) and bool(me["key_scope"]["reason"]),
          "and the English sentence is still there, because the log and the 403 bodies use it")

    apply_auth(key_scope_policy={"enabled": True, "users": ["somebody-else"]}).raise_for_status()
    me = user.get("/v1/access/me").json()
    check(me["key_scope"]["reason_code"] == "levelsFailed"
          and me["key_scope"]["reason_params"]["levels"] == ["user"],
          "a denial names the level that failed as data, not only inside the sentence",
          me["key_scope"].get("reason_params"))
    apply_auth(key_scope_policy={"enabled": True, "users": [USER]}).raise_for_status()
    me = user.get("/v1/access/me").json()
    check(me["key_scope"]["reason_code"] == "allowed",
          "and the allow verdict has a code as well, so the page never renders raw English",
          me["key_scope"].get("reason_code"))

    section("The allow lists can only offer accounts that may create a key")
    # The console filters the three tables by key-creation eligibility. Teams and organizations it
    # can decide from the saved key_policy on its own; a user it cannot, because key_policy has no
    # user list and decides by GitHub membership, so the answer has to come from this endpoint.
    apply_auth(
        key_policy={**((saved["auth"] or {}).get("key_policy") or {}), "enabled": False},
    ).raise_for_status()
    plain = client.get("/v1/access/users").json()
    check(plain.get("eligibility_evaluated") is False
          and all("can_create_key" not in u for u in plain["users"]),
          "without the flag the endpoint answers exactly as before, at no extra cost",
          str(plain.get("eligibility_evaluated")))
    check(plain.get("key_policy_enabled") is False,
          "and it reports whether key creation is gated at all, which decides the note on the page")

    graded = client.get("/v1/access/users?eligibility=1").json()
    rows_by_login = {str(u.get("login")): u for u in graded["users"]}
    check(graded.get("eligibility_evaluated") is True
          and all("can_create_key" in u for u in graded["users"]),
          "with it, every row carries a verdict", str(len(graded["users"])))
    check(all(u["can_create_key"] is True for u in graded["users"]),
          "with the gate open every account may create a key, so nothing is filtered out",
          str({k: v["can_create_key"] for k, v in list(rows_by_login.items())[:5]}))
    check(USER in rows_by_login,
          "and the subject of this run is among them, so the next check has something to deny")

    # An enabled policy with no enterprise allowed at all: the fail-closed branch, which is the
    # one that actually hides rows on the page.
    apply_auth(key_policy={"enabled": True, "enterprises": {}}).raise_for_status()
    gated = client.get("/v1/access/users?eligibility=1").json()
    gated_rows = {str(u.get("login")): u for u in gated["users"]}
    check(gated.get("key_policy_enabled") is True,
          "the endpoint now reports the gate as on")
    check(gated_rows[USER]["can_create_key"] is False,
          "a login no enterprise rule admits cannot create a key, so the page will not offer it",
          str(gated_rows[USER]["can_create_key"]))
    check(gated_rows.get(admin_login, {}).get("can_create_key") is True,
          "an administrator still may, because keypolicy decides them before any membership check",
          str(gated_rows.get(admin_login, {}).get("can_create_key")))
    check(all(u["can_create_key"] in (True, False, None) for u in gated["users"]),
          "every verdict is a boolean or an explicit null, never a string the page would misread")
    check(gated.get("eligibility_truncated") is False,
          "and this deployment is inside the per-request evaluation cap")

    r = user.get("/v1/access/users?eligibility=1")
    check(r.status_code == 403,
          "the endpoint stays administrators-only with the flag on", str(r.status_code))
finally:
    restored = restore_auth()
    now = client.get("/v1/config").json().get("auth") or {}
    print(f"\nauth restored (HTTP {restored.status_code}): "
          f"key_policy.enabled={(now.get('key_policy') or {}).get('enabled')} "
          f"key_scope_policy={now.get('key_scope_policy')}")
    if now.get("key_scope_policy") != (saved["auth"] or {}).get("key_scope_policy"):
        fail += 1
        print("[FAIL] the auth section was not restored to what it was before the run")
    if now.get("key_policy") != (saved["auth"] or {}).get("key_policy"):
        # Part 3 rewrites key_policy, and a run that left it rewritten would change who may create
        # a key on this deployment, which is a great deal more than a verification should do.
        fail += 1
        print("[FAIL] key_policy was not restored to what it was before the run")
    for created_id in created:
        client.delete(f"/v1/keys/{created_id}")
    for record in store.list_api_keys(USER):
        store.delete_api_key(record["id"])
    store.delete_session(user_sid)
    user.close()

print(f"\n{ok} passed, {fail} failed")
if not fail:
    print("\nALL KEY SCOPE POLICY CHECKS PASSED")
raise SystemExit(1 if fail else 0)
