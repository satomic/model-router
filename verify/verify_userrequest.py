"""Verify the AI decider only sees the real question inside <userRequest>, undisturbed by the
surrounding context noise.

The prompt text stays in Chinese: the assertion below looks for the same literal substring, and the
rule keywords in the live config.yaml are Chinese words.

`decision_input` only exists when the decision model actually ran, so the strategy is switched to
`ai` for the run and restored in a `finally` -- under `rule` there is no decision at all, and under
`rule-then-ai` this very prompt matches a keyword rule and is decided without one. Stickiness is
disabled for the same reason: a binding left over from an earlier run would answer the request from
the store and skip the decision.
"""
import _bootstrap  # noqa: F401

from verify_auth_helper import make_client

client, _api_key, _login = make_client()

content = (
    "<context>\nThe current date is 2026-08-14.\nTerminals:\n"
    "Terminal: zsh\nLast Command: some very long noisy context here " + "x" * 500 +
    "\n</context>\n<userRequest>\n帮我重构这个模块的架构\n</userRequest>"
)

doc = client.get("/v1/config").json()
saved_strategy, saved_session = doc["strategy"], doc.get("session") or {}
doc["strategy"] = "ai"
doc["session"] = {**saved_session, "sticky": False}
client.put("/v1/config", json=doc).raise_for_status()
print(f"strategy switched from {saved_strategy!r} to 'ai', stickiness disabled for the run")

try:
    r = client.post(
        "/v1/chat/completions",
        json={"messages": [{"role": "user", "content": content}], "max_tokens": 20},
    )
    r.raise_for_status()
    tid = r.headers["x-trace-id"]
    t = client.get(f"/v1/traces/{tid}").json()
    analysis = t["routing"]["analysis"]
    assert analysis["type"] == "ai", f"the decision did not run, analysis type={analysis['type']!r}"
    print("decision_input:", repr(analysis["decision_input"]))
    assert "重构" in analysis["decision_input"], "userRequest was not extracted"
    assert "noisy context" not in analysis["decision_input"], "context noise was not stripped"
    print("OK: the decision model only saw the real question inside <userRequest>")
finally:
    doc = client.get("/v1/config").json()
    doc["strategy"] = saved_strategy
    doc["session"] = saved_session
    resp = client.put("/v1/config", json=doc)
    after = client.get("/v1/config").json()
    print(f"strategy restored to {after['strategy']!r} (HTTP {resp.status_code}), "
          f"sticky={(after.get('session') or {}).get('sticky')}")
