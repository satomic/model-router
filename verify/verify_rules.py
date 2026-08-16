"""Verify the rule-based routing strategy.

The script switches `strategy` to `rule` itself and restores the original value on the way out
(the same approach verify_prompt.py takes with ai_router.decision_prompt). It used to require the
operator to have flipped it beforehand, which meant that against a deployment left on `ai` the
assertions silently graded the *AI* router's choices -- and one of them happens to agree, so the
run looked like a genuine partial failure rather than a wrong precondition.
"""
import _bootstrap  # noqa: F401

from verify_auth_helper import make_client

client, _api_key, _login = make_client()

saved_strategy = client.get("/v1/config").json()["strategy"]
if saved_strategy != "rule":
    doc = client.get("/v1/config").json()
    doc["strategy"] = "rule"
    client.put("/v1/config", json=doc).raise_for_status()
    print(f"strategy switched from {saved_strategy!r} to 'rule' for this run")

# The prompts stay in Chinese on purpose: they have to contain the literal keywords configured in
# the live config.yaml's rules, and keyword matching is a plain substring test. Translating them
# would silently stop the rules from matching and the assertions would test nothing.
CASES = [
    ("请帮我证明这个数学定理", "o3-pro", "deep-reasoning keyword"),
    ("帮我重构这个模块的架构", "gpt-5.6-sol", "complex-engineering keyword"),
    ("这段代码报错了怎么 debug", "gpt-5.4", "coding keyword"),
    ("x" * 7000, "gpt-5.4-pro", "long-prompt rule"),
    ("今天天气怎么样", "gpt-4o", "no rule matches, falls back to the default"),
]

failures = []
try:
    for prompt, expected, why in CASES:
        r = client.post(
            "/v1/chat/completions",
            json={"messages": [{"role": "user", "content": prompt}], "max_tokens": 600},
        )
        r.raise_for_status()
        got = r.headers["x-routed-model"]
        reason = r.headers["x-router-reason"]
        ok = got == expected
        print(
            f"[{'OK ' if ok else 'FAIL'}] {why}: expected={expected} got={got} "
            f"reason={reason} decision_ms={r.headers['x-router-decision-ms']}"
        )
        # A wrong reason is its own failure: the right model chosen by the AI router would
        # otherwise pass a test of the rule engine.
        if not ok or reason == "ai-decision":
            failures.append(f"{why} (got {got} via {reason})")
finally:
    if saved_strategy != "rule":
        doc = client.get("/v1/config").json()
        doc["strategy"] = saved_strategy
        client.put("/v1/config", json=doc)
        print(f"strategy restored to {saved_strategy!r}")

assert not failures, "; ".join(failures)
print("\nRULE ROUTING PASSED")
