"""Verify the AI decider only sees the real question inside <userRequest>, undisturbed by the
surrounding context noise.

The prompt text stays in Chinese: the assertion below looks for the same literal substring, and the
rule keywords in the live config.yaml are Chinese words.
"""
import _bootstrap  # noqa: F401

from verify_auth_helper import make_client

client, _api_key, _login = make_client()

content = (
    "<context>\nThe current date is 2026-08-14.\nTerminals:\n"
    "Terminal: zsh\nLast Command: some very long noisy context here " + "x" * 500 +
    "\n</context>\n<userRequest>\n帮我重构这个模块的架构\n</userRequest>"
)

r = client.post(
    "/v1/chat/completions",
    json={"messages": [{"role": "user", "content": content}], "max_tokens": 20},
)
r.raise_for_status()
tid = r.headers["x-trace-id"]
t = client.get(f"/v1/traces/{tid}").json()
print("decision_input:", repr(t["routing"]["analysis"]["decision_input"]))
assert "重构" in t["routing"]["analysis"]["decision_input"], "userRequest was not extracted"
assert "noisy context" not in t["routing"]["analysis"]["decision_input"], "context noise was not stripped"
print("OK: the decision model only saw the real question inside <userRequest>")
