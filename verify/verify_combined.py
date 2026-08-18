"""Verify the rule-then-ai strategy: both policies active, rules taking precedence.

Three things have to hold, and only the third is new behaviour rather than a composition of the
two existing strategies:

1. A prompt a rule matches goes to that rule's model, `decided_by == "rule"`, and the analysis has
   **no `ai` sub-analysis** -- the absence is the assertion, because it is the only observable proof
   that no decision call was paid for. `x-router-decision-ms` corroborates it: a decision call
   cannot come back in single-digit milliseconds.
2. A prompt no rule matches reaches the decision model: `decided_by == "ai"`, a `reason` from the
   AI branch, and both sub-analyses present. It must NOT silently take the default model the way
   `strategy: rule` does -- that substitution is exactly what this strategy replaces.
3. The two single-strategy behaviours are unchanged, which is checked by the fact that each
   sub-analysis keeps the `type` its counterpart emits (`"rule"` / `"ai"`); the console renders
   them with those renderers, so a change of type here breaks the traces page silently.

`strategy` is switched to `rule-then-ai` for the run and restored in a `finally`, the pattern
verify_rules.py uses. Run verify_stub_upstream.py first if no real Foundry backend is reachable.
"""
import _bootstrap  # noqa: F401

from verify_auth_helper import make_client

client, _api_key, _login = make_client()

ok = fail = 0


def check(cond, label, extra=""):
    global ok, fail
    if cond:
        ok += 1
        print(f"[OK  ] {label} {extra}")
    else:
        fail += 1
        print(f"[FAIL] {label} {extra}")


def analysis_of(resp):
    """Fetch the trace of a request and return its routing analysis."""
    trace = client.get(f"/v1/traces/{resp.headers['x-trace-id']}").json()
    return trace["routing"]["analysis"]


doc = client.get("/v1/config").json()
saved_strategy = doc["strategy"]
# Stickiness would answer the second and later requests from the binding store and skip the
# decision entirely -- and the decision is the whole subject of this script. Disabled for the run
# and restored verbatim afterwards, alongside the strategy.
saved_session = doc.get("session") or {}

doc["strategy"] = "rule-then-ai"
doc["session"] = {**saved_session, "sticky": False}
client.put("/v1/config", json=doc).raise_for_status()
print(f"strategy switched from {saved_strategy!r} to 'rule-then-ai', stickiness disabled for the run")

# The prompts stay in Chinese on purpose, same reason as verify_rules.py: they must contain the
# literal keywords configured in the live config.yaml, and matching is a plain substring test.
RULE_CASES = [
    ("请帮我证明这个数学定理", "o3-pro", "deep-reasoning keyword"),
    ("帮我重构这个模块的架构", "gpt-5.6-sol", "complex-engineering keyword"),
    ("这段代码报错了怎么 debug", "gpt-5.4", "coding keyword"),
    # A length rule wins just as a keyword rule does -- documented in route_combined and asserted
    # here, because "only keywords suppress the AI call" is the other plausible reading of the
    # requirement and the two are indistinguishable without a test.
    ("x" * 7000, "gpt-5.4-pro", "long-prompt rule (min_prompt_chars, not a keyword)"),
]

try:
    print("\n=== A matched rule decides, and costs no decision call ===")
    for prompt, expected, why in RULE_CASES:
        r = client.post(
            "/v1/chat/completions",
            json={"messages": [{"role": "user", "content": prompt}], "max_tokens": 600},
        )
        r.raise_for_status()
        got = r.headers["x-routed-model"]
        reason = r.headers["x-router-reason"]
        ms = float(r.headers["x-router-decision-ms"])
        a = analysis_of(r)

        check(got == expected, f"{why}: model", f"expected={expected} got={got}")
        check(a.get("type") == "rule-then-ai", f"{why}: analysis type", a.get("type"))
        check(a.get("decided_by") == "rule", f"{why}: decided_by", a.get("decided_by"))
        check("ai" not in a, f"{why}: no AI sub-analysis, so no decision call was made")
        check(a.get("rule", {}).get("type") == "rule",
              f"{why}: the rule stage keeps the type the console renders")
        check(any(s.get("matched") for s in a.get("rule", {}).get("evaluated", [])),
              f"{why}: a rule is recorded as matched")
        check(reason not in ("ai-decision", "ai-fallback-default", "default"),
              f"{why}: reason names the rule, not the AI branch", f"reason={reason}")
        check(ms < 200, f"{why}: decision was local", f"{ms} ms")

    print("\n=== An unmatched request falls through to the decision model ===")
    r = client.post(
        "/v1/chat/completions",
        json={"messages": [{"role": "user", "content": "今天天气怎么样"}], "max_tokens": 60},
    )
    r.raise_for_status()
    reason = r.headers["x-router-reason"]
    a = analysis_of(r)

    check(a.get("type") == "rule-then-ai", "unmatched: analysis type", a.get("type"))
    check(a.get("decided_by") == "ai", "unmatched: decided_by", a.get("decided_by"))
    check("ai" in a, "unmatched: the AI sub-analysis is present, so the decision model was called")
    check(a.get("ai", {}).get("type") == "ai",
          "unmatched: the AI stage keeps the type the console renders", a.get("ai", {}).get("type"))
    check(reason in ("ai-decision", "ai-fallback-default"),
          "unmatched: reason comes from the AI branch, not from a default substitution",
          f"reason={reason}")
    check(not any(s.get("matched") for s in a.get("rule", {}).get("evaluated", [])),
          "unmatched: the rule stage recorded no match")
    check("fallback" in a.get("rule", {}),
          "unmatched: the rule stage records the handover rather than a silent skip")
    check(a.get("rule", {}).get("evaluated"),
          "unmatched: every rule is still recorded as evaluated, so the trace shows why none fired")
    print(f"       routed={r.headers['x-routed-model']} reason={reason} "
          f"decision_ms={r.headers['x-router-decision-ms']}")

    print("\n=== The strategy round-trips through the config API ===")
    check(client.get("/v1/config").json()["strategy"] == "rule-then-ai",
          "GET /v1/config reports the saved strategy")
    check(client.get("/healthz").json()["strategy"] == "rule-then-ai",
          "/healthz reports it too")
    bad = client.put("/v1/config", json={"strategy": "rule-and-ai"})
    check(bad.status_code == 422, "an unknown strategy value is rejected", bad.status_code)
    check(client.get("/v1/config").json()["strategy"] == "rule-then-ai",
          "the rejected save left the stored strategy untouched")
finally:
    doc = client.get("/v1/config").json()
    doc["strategy"] = saved_strategy
    doc["session"] = saved_session
    resp = client.put("/v1/config", json=doc)
    after = client.get("/v1/config").json()
    print(f"\nstrategy restored to {after['strategy']!r} (HTTP {resp.status_code}), "
          f"sticky={(after.get('session') or {}).get('sticky')}")

print(f"\n{ok} passed, {fail} failed")
raise SystemExit(1 if fail else 0)
