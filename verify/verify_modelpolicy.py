"""Verify the model policy: model groups, union resolution across scopes, and enforcement.

The interesting assertions are the ones that would still pass if enforcement had been written as
"check the routed model against the allow list at the end", which is the implementation this one
deliberately is not:

1. **A rule naming a model the caller may not use is *skipped*, not obeyed and not rejected.** The
   evaluated step carries the catalog `skipped` marker, and the request still succeeds on another
   model. A late check would have produced a 403 for a prompt the operator's own rule matched.
2. **The decision model is never offered a model the caller may not use.** Asserted on the AI
   sub-analysis's own `candidates` list, not on the model it happened to pick -- a classifier that
   is told about a forbidden model and merely does not choose it this time is a latent bug.
3. **A sticky binding predating a policy change is dropped and re-decided.** The binding is real
   and it is honoured for everyone else; the assertion is that it stops being honoured for a
   caller who lost the model.

The rest covers the surface: `/v1/models` narrowed, the `403` naming the reason when the effective
set is empty, the three deliberately-different states (off / enabled-but-unbound / bound to an
empty group), the union of `default_group` with a user binding, `/v1/models/available` agreeing
with all of it, the administrator exemption, the signed-in-user registry, and the validation
refusals.

Rules and models are read out of the live configuration rather than hardcoded: the assertions are
about which model a *particular configured rule* reaches, so a repository whose config.yaml names
different models must still be able to run this. `model_groups` / `model_policy` (plus `strategy`
and `session`) are rewritten for the run and restored in a `finally`, the pattern verify_rules.py
and verify_combined.py use. Run verify_stub_upstream.py first if no real backend is reachable.
"""
import _bootstrap  # noqa: F401

from uuid import uuid4

import httpx

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


def analysis_of(resp):
    trace = client.get(f"/v1/traces/{resp.headers['x-trace-id']}").json()
    return trace["routing"]["analysis"]


def model_ids(resp) -> list[str]:
    return [m["id"] for m in resp.json()["data"]]


# -- The identity under test ---------------------------------------------------
# A synthetic non-administrator: the policy exempts administrators, so the login make_client()
# returns cannot be the subject of a single restriction assertion. create_session() also records
# the sign-in, which is what the /v1/access/users check later reads back.
USER = "verify-policy-user"
raw = load_raw()
admins = [str(x).strip().lower() for x in ((raw.get("auth") or {}).get("admin_logins") or [])]
assert USER not in admins, "the subject of these checks must not be an administrator"

user_sid = store.create_session(
    {"login": USER, "name": USER, "avatar_url": None, "is_admin": False}, 3600
)
for record in store.list_api_keys(USER):
    store.delete_api_key(record["id"])
_rec, user_key = store.create_api_key(USER, "verify-policy")
user = httpx.Client(
    base_url=BASE, timeout=300, cookies={"mr_session": user_sid},
    headers={"Authorization": f"Bearer {user_key}"},
)

# -- What the live configuration gives us to work with -------------------------
doc = client.get("/v1/config").json()
CATALOG = list(doc.get("models") or {})
assert len(CATALOG) >= 2, f"these checks need at least two models, catalog is {CATALOG}"
DEFAULT_MODEL = next(
    (n for n, m in (doc.get("models") or {}).items() if m.get("default")), CATALOG[0]
)

RULES = [r for r in (doc.get("rules") or []) if r.get("keywords") and r.get("model") in CATALOG]
assert len(RULES) >= 2, "these checks need at least two keyword rules naming catalog models"


def prompt_for(index: int) -> str:
    """A prompt that matches RULES[index] and no rule ahead of it.

    Rules are evaluated in order, so a keyword that also appears in an earlier rule would be
    decided by that earlier rule and the assertion would be about the wrong row. Built here
    rather than written out because the keywords live in config.yaml, not in this file.
    """
    earlier = [
        str(k).lower()
        for r in (doc.get("rules") or [])[: (doc.get("rules") or []).index(RULES[index])]
        for k in (r.get("keywords") or [])
    ]
    for keyword in RULES[index]["keywords"]:
        text = f"{keyword}"
        if not any(e in text.lower() for e in earlier):
            return text
    raise SystemExit(
        f"every keyword of rule {RULES[index].get('name')!r} also appears in an earlier rule, "
        "so no prompt can reach it -- adjust config.yaml's rules to run this script"
    )


# RULE_OFF's model is excluded from the group, so its rule must be skipped. It is the first
# keyword rule, so nothing ahead of it can decide instead and mask the skip.
RULE_OFF, RULE_ON = RULES[0], next(r for r in RULES[1:] if r["model"] != RULES[0]["model"])
PROMPT_OFF = prompt_for(0)
PROMPT_ON = prompt_for(RULES.index(RULE_ON))
MODEL_OFF, MODEL_ON = RULE_OFF["model"], RULE_ON["model"]
assert MODEL_OFF != DEFAULT_MODEL, (
    f"rule {RULE_OFF.get('name')!r} routes to the default model {DEFAULT_MODEL}, which cannot be "
    "excluded from a group without emptying it -- adjust config.yaml's rules to run this script"
)
print(f"subject={USER}  default={DEFAULT_MODEL}")
print(f"rule to be skipped : {RULE_OFF.get('name')} -> {MODEL_OFF}  (prompt {PROMPT_OFF!r})")
print(f"rule to be obeyed  : {RULE_ON.get('name')} -> {MODEL_ON}  (prompt {PROMPT_ON!r})")

G_DEFAULT, G_ALLOWED, G_EXTRA, G_EMPTY = (
    "verify-default", "verify-allowed", "verify-extra", "verify-empty",
)
GROUPS = {
    **(doc.get("model_groups") or {}),
    G_DEFAULT: [DEFAULT_MODEL],
    G_ALLOWED: [DEFAULT_MODEL, MODEL_ON],
    G_EXTRA: [MODEL_OFF],
    G_EMPTY: [],
}
ALLOWED_ORDER = [m for m in CATALOG if m in (DEFAULT_MODEL, MODEL_ON)]

saved = {
    "model_groups": doc.get("model_groups"),
    "model_policy": doc.get("model_policy"),
    "strategy": doc.get("strategy"),
    "session": doc.get("session"),
}


def apply(**patch) -> httpx.Response:
    """PUT the whole configuration document back with `patch` applied.

    The whole document, because save_raw replaces top-level keys and _dropped_auth_credentials
    refuses a submission that would wipe the auth section -- the same round trip the console does.
    """
    current = client.get("/v1/config").json()
    current.update(patch)
    return client.put("/v1/config", json=current)


def policy(**fields) -> dict:
    return {"enabled": True, "default_group": "", "users": {}, "teams": {},
            "organizations": {}, **fields}


try:
    # Stickiness would answer most of the requests below from the binding store; it is switched
    # back on for the one check that is about stickiness.
    apply(model_groups=GROUPS, strategy="rule-then-ai",
          session={**(saved["session"] or {}), "sticky": False},
          model_policy=policy(enabled=False)).raise_for_status()

    section("The three states are different, and only one of them restricts")
    check(model_ids(user.get("/v1/models")) == CATALOG,
          "policy disabled: the whole catalog", f"{len(CATALOG)} models")
    v = user.get("/v1/models/available").json()
    check(v["reason"] == "policy-disabled" and v["unrestricted"] is True,
          "policy disabled: reason", v["reason"])

    apply(model_policy=policy()).raise_for_status()
    check(model_ids(user.get("/v1/models")) == CATALOG,
          "enabled with nothing bound: still the whole catalog -- the switch cannot lock anybody out")
    v = user.get("/v1/models/available").json()
    check(v["reason"] == "no-binding" and v["unrestricted"] is True,
          "enabled with nothing bound: reason", v["reason"])
    check(v["contributions"] == [], "enabled with nothing bound: no grants are claimed")

    apply(model_policy=policy(users={USER: G_EMPTY})).raise_for_status()
    check(model_ids(user.get("/v1/models")) == [],
          "bound to an empty group: /v1/models is empty, not the full catalog")
    v = user.get("/v1/models/available").json()
    check(v["reason"] == "empty-group" and v["unrestricted"] is False,
          "bound to an empty group: reason", v["reason"])
    r = user.post("/v1/chat/completions",
                  json={"messages": [{"role": "user", "content": "hi"}], "max_tokens": 16})
    check(r.status_code == 403, "bound to an empty group: a call is refused", r.status_code)
    check("model policy" in str(r.json().get("detail", "")).lower(),
          "the refusal names the policy rather than failing opaquely", r.json().get("detail"))

    section("The administrator exemption")
    # Bound to the same empty group as the user above: the exemption has to come from the admin
    # list, not from the absence of a binding.
    apply(model_policy=policy(users={USER: G_EMPTY, admin_login: G_EMPTY})).raise_for_status()
    check(model_ids(client.get("/v1/models")) == CATALOG,
          "an administrator bound to an empty group still sees the whole catalog")
    v = client.get("/v1/models/available").json()
    check(v["reason"] == "administrator" and v["unrestricted"] is True,
          "and is told why", v["reason"])
    check(model_ids(user.get("/v1/models")) == [],
          "the same binding still restricts the non-administrator, so the exemption is not global")

    section("Union across scopes")
    apply(model_policy=policy(default_group=G_DEFAULT,
                             users={USER: G_EXTRA})).raise_for_status()
    expected_union = [m for m in CATALOG if m in (DEFAULT_MODEL, MODEL_OFF)]
    check(model_ids(user.get("/v1/models")) == expected_union,
          "default group + user binding = the union, in catalog order", str(expected_union))
    v = user.get("/v1/models/available").json()
    check(v["models"] == expected_union and v["reason"] == "union", "the page agrees", v["reason"])
    check([c["scope"] for c in v["contributions"]] == ["default", "user"],
          "both grants are reported, and only those two",
          str([c["scope"] for c in v["contributions"]]))
    check(all(c["group"] in GROUPS for c in v["contributions"]),
          "each grant names the group it came from")
    check(v["default_model"] in expected_union,
          "the model an unrouted request would land on is itself permitted", v["default_model"])

    section("A rule naming a model the caller may not use is skipped, not obeyed")
    apply(model_policy=policy(users={USER: G_ALLOWED})).raise_for_status()
    check(model_ids(user.get("/v1/models")) == ALLOWED_ORDER,
          "the effective set for the routing checks", str(ALLOWED_ORDER))

    r = user.post("/v1/chat/completions",
                  json={"messages": [{"role": "user", "content": PROMPT_OFF}], "max_tokens": 32})
    r.raise_for_status()
    a = analysis_of(r)
    routed = r.headers["x-routed-model"]
    check(routed != MODEL_OFF, "the excluded model was not routed to", f"routed={routed}")
    check(routed in ALLOWED_ORDER, "the request still succeeded, on a permitted model", routed)
    check(a.get("policy_models") == ALLOWED_ORDER,
          "the analysis records the effective set the decision was made inside",
          str(a.get("policy_models")))
    steps = (a.get("rule") or a).get("evaluated") or []
    step = next((s for s in steps if s.get("model") == MODEL_OFF), None)
    check(step is not None and "skipped" in step,
          "the matching rule is recorded as skipped, with a reason", str(step))
    check(not any(s.get("matched") for s in steps),
          "and nothing else claimed a match, so the skip is what happened")

    section("The decision model is only ever offered permitted candidates")
    ai = a.get("ai") or (a if a.get("type") == "ai" else {})
    check(ai.get("candidates") == ALLOWED_ORDER,
          "the AI sub-analysis's own candidate list is the effective set",
          str(ai.get("candidates")))
    check(MODEL_OFF not in (ai.get("decision_system") or ""),
          "and the rendered decision prompt does not mention the excluded model")

    section("A rule naming a permitted model is obeyed as usual")
    r = user.post("/v1/chat/completions",
                  json={"messages": [{"role": "user", "content": PROMPT_ON}], "max_tokens": 32})
    r.raise_for_status()
    a = analysis_of(r)
    check(r.headers["x-routed-model"] == MODEL_ON,
          "the permitted rule decides", r.headers["x-routed-model"])
    check(a.get("decided_by") == "rule", "decided_by", a.get("decided_by"))
    check("ai" not in a, "and no decision call was paid for")

    section("A sticky binding that predates a policy change is dropped")
    apply(session={**(saved["session"] or {}), "sticky": True}).raise_for_status()
    # Fresh per run. This section asserts the *first* request on the session is bound by the rule,
    # and a fixed id would still be pinned in the router's sticky cache from an earlier run (the
    # default TTL is 1800s) -- bound, in fact, to the model this section deliberately narrows to
    # at the end, so the two assertions either side of the narrowing both fail. Same hazard, and
    # the same fix, as SESSION_ID in verify_enhanced.py.
    sid = f"verify-policy-{uuid4().hex[:8]}"
    r = user.post("/v1/chat/completions",
                  headers={"x-session-id": sid},
                  json={"messages": [{"role": "user", "content": PROMPT_ON}], "max_tokens": 32})
    r.raise_for_status()
    check(r.headers["x-routed-model"] == MODEL_ON, "the session is bound to the rule's model",
          r.headers["x-routed-model"])
    r = user.post("/v1/chat/completions",
                  headers={"x-session-id": sid},
                  json={"messages": [{"role": "user", "content": PROMPT_ON}], "max_tokens": 32})
    r.raise_for_status()
    check(r.headers["x-router-reason"] == "session-sticky",
          "and the binding is honoured while the model is still permitted",
          r.headers["x-router-reason"])

    # Narrow the group so the bound model is no longer permitted. The PUT also invalidates the
    # memoised verdicts, so the next request sees the new policy rather than the 60s-old one.
    apply(model_policy=policy(users={USER: G_DEFAULT})).raise_for_status()
    r = user.post("/v1/chat/completions",
                  headers={"x-session-id": sid},
                  json={"messages": [{"role": "user", "content": PROMPT_ON}], "max_tokens": 32})
    r.raise_for_status()
    check(r.headers["x-routed-model"] != MODEL_ON,
          "the stale binding is not resurrected", r.headers["x-routed-model"])
    check(r.headers["x-routed-model"] == DEFAULT_MODEL,
          "the request is re-decided inside the narrowed set", r.headers["x-routed-model"])
    check(r.headers["x-router-reason"] != "session-sticky",
          "and the reason says it was decided again", r.headers["x-router-reason"])
    apply(session={**(saved["session"] or {}), "sticky": False}).raise_for_status()

    section("The signed-in-user registry")
    body = client.get("/v1/access/users").json()
    rows = {u["login"].lower(): u for u in body["users"]}
    check(USER in rows, "the login that signed in is listed", str(sorted(rows)[:5]))
    row = rows.get(USER, {})
    check(row.get("model_group") == G_DEFAULT,
          "with the group currently bound to it", row.get("model_group"))
    check(row.get("sign_ins", 0) >= 1 and row.get("first_seen") and row.get("last_seen"),
          "and first / last sign-in plus a count",
          f"sign_ins={row.get('sign_ins')}")
    check(row.get("kind") in ("github", "local"), "and which door they came through",
          row.get("kind"))
    check(user.get("/v1/access/users").status_code == 403,
          "a non-administrator cannot read the list")
    check(user.get("/v1/config").status_code == 403,
          "nor the configuration, so the policy tables stay private")

    section("Validation refuses a policy that only looks like it grants something")
    before = client.get("/v1/config").json()
    r = apply(model_groups={**GROUPS, "verify-bad": ["no-such-model"]})
    check(r.status_code == 422, "a group naming an unknown model is refused", r.status_code)
    r = apply(model_policy=policy(users={USER: "verify-no-such-group"}))
    check(r.status_code == 422, "a binding naming an unknown group is refused", r.status_code)
    r = apply(model_policy=policy(default_group="verify-no-such-group"))
    check(r.status_code == 422, "so is a default_group that does not exist", r.status_code)
    r = apply(model_policy=policy(enabled="yes"))
    check(r.status_code == 422, "so is a non-boolean enabled", r.status_code)
    after = client.get("/v1/config").json()
    check(after.get("model_groups") == before.get("model_groups")
          and after.get("model_policy") == before.get("model_policy"),
          "and none of the four rejected saves changed the stored configuration")
    check(sorted(after.get("model_groups") or {}) == sorted(GROUPS),
          "an empty group survives the round trip through YAML",
          str(sorted(after.get("model_groups") or {})))
    check((after.get("model_groups") or {}).get(G_EMPTY) in ([], None),
          "and is still empty rather than having become null-and-therefore-absent",
          repr((after.get("model_groups") or {}).get(G_EMPTY)))
finally:
    restored = apply(**saved)
    now = client.get("/v1/config").json()
    print(f"\nmodel_groups / model_policy / strategy / session restored "
          f"(HTTP {restored.status_code}): groups={list(now.get('model_groups') or {})} "
          f"policy_enabled={(now.get('model_policy') or {}).get('enabled')} "
          f"strategy={now.get('strategy')} "
          f"sticky={(now.get('session') or {}).get('sticky')}")
    store.delete_session(user_sid)
    for record in store.list_api_keys(USER):
        store.delete_api_key(record["id"])

print(f"\n{ok} passed, {fail} failed")
if not fail:
    print("\nALL MODEL POLICY CHECKS PASSED")
raise SystemExit(1 if fail else 0)
