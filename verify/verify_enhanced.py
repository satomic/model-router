"""Verify the enhanced features: full-chain trace recording, decision analysis, hot config reload
through the API, and UI hosting."""
import _bootstrap  # noqa: F401

import json
from uuid import uuid4

import httpx

from app.config import CONFIG_PATH, LOG_DIR, STRATEGIES
from app.traces import _safe
from verify_auth_helper import BASE, make_client

client, api_key, login = make_client()

# Fresh per run: section 5 asserts the *first* request on this session routes by AI decision, and
# a fixed id would still be pinned in the router's sticky cache from an earlier run (default TTL
# 1800s) -- which made this script pass only against a just-restarted server.
SESSION_ID = f"trace-test-{uuid4().hex[:8]}"


def section(title):
    print(f"\n=== {title} ===")


print(f"auth ready: login={login} (administrator)")

# 1. UI hosting: the console owns the site root, and every client route survives a direct hit
section("UI hosting")
r = client.get("/")
assert r.status_code == 200 and "Model Router Console" in r.text
print("GET / ->", r.status_code, "(index.html served)")

# A deep client route must answer with the same shell -- this is what makes a pasted or
# bookmarked /config/models work rather than 404.
for path in ("/config/models", "/access/policy", "/traces/does-not-exist"):
    r = client.get(path)
    assert r.status_code == 200 and "Model Router Console" in r.text, (path, r.status_code)
print("deep links /config/models, /access/policy, /traces/<id> -> 200 (same shell)")

# The catch-all must not shadow the API: an unknown /v1/* path has to keep 404ing as JSON, or
# every client typo turns into a page of HTML that a fetch() then fails to parse.
r = client.get("/v1/no-such-endpoint")
assert r.status_code == 404, r.status_code
assert r.headers["content-type"].startswith("application/json"), r.headers["content-type"]
print("GET /v1/no-such-endpoint -> 404 application/json (API not shadowed)")

# A missing asset must fail loudly rather than receive index.html under a JS content type --
# that is how a stale build gets noticed.
r = client.get("/assets/not-a-real-bundle.js")
assert r.status_code == 404, r.status_code
assert "Model Router Console" not in r.text
print("GET /assets/<missing>.js -> 404 (never index.html)")

# The old /ui/ namespace is removed outright, not redirected.
for path in ("/ui", "/ui/", "/ui/keys"):
    assert client.get(path, follow_redirects=True).status_code == 404, path
print("GET /ui, /ui/, /ui/keys -> 404 (the prefix is gone)")

# 2. Reading the config (administrators only)
section("Config API")
cfg = client.get("/v1/config").json()
# Read from app.config rather than repeating the list here: a hardcoded pair would fail against a
# deployment left on a strategy added later, which is a wrong precondition, not a defect.
assert cfg["strategy"] in STRATEGIES and cfg["models"]
print("GET /v1/config -> strategy:", cfg["strategy"], "| models:", list(cfg["models"]))
print("providers:", list(cfg.get("providers") or {}), "| default:", cfg.get("default_provider"))

# 3. Hot reload (flip sticky) and write-back to the file
orig_sticky = cfg["session"]["sticky"]
cfg["session"]["sticky"] = not orig_sticky
r = client.put("/v1/config", json=cfg)
assert r.status_code == 200 and r.json()["sticky"] == (not orig_sticky)
print("PUT /v1/config -> sticky flipped to", not orig_sticky)
health = client.get("/healthz").json()
assert health["sticky"] == (not orig_sticky), "the hot reload did not take effect"
print("healthz confirms the hot reload | providers:", health["providers"])
# Restore
cfg["session"]["sticky"] = orig_sticky
client.put("/v1/config", json=cfg)
with open(CONFIG_PATH, encoding="utf-8") as f:
    text = f.read()
# The live config.yaml is the operator's own file, so its header text is not fixed -- assert only
# that the round-trip preserved a leading comment, in whatever language it is written.
first = next((ln for ln in text.splitlines() if ln.strip()), "")
assert first.lstrip().startswith("#"), f"yaml comments were lost, first line: {first!r}"
print("config.yaml comments preserved OK")

# 4. An invalid config is rejected
bad = dict(cfg)
bad["strategy"] = "bogus"
r = client.put("/v1/config", json=bad)
assert r.status_code == 422
print("invalid strategy -> 422:", r.json()["detail"])

# 4b. Reading the config while signed out -> 401 (bare httpx, no session cookie)
r = httpx.get(f"{BASE}/v1/config", timeout=30)
assert r.status_code == 401, f"expected 401 without a session, got {r.status_code}"
print("GET /v1/config with no session -> 401 OK")

# 5. AI routing + trace completeness
section("AI routing trace")
r = client.post(
    "/v1/chat/completions",
    json={"messages": [{"role": "user", "content": "hello there"}], "max_tokens": 30},
    headers={"x-session-id": SESSION_ID},
)
r.raise_for_status()
trace_id = r.headers["x-trace-id"]
t = client.get(f"/v1/traces/{trace_id}").json()
assert t["request"]["params"]["max_tokens"] == 30, "request parameters were not recorded"
assert t["routing"]["analysis"]["type"] == "ai", "the decision analysis is missing"
assert t["routing"]["analysis"].get("rationale"), "the decision rationale is missing"
assert t["response"]["content"], "the response content was not recorded"
assert t["response"]["usage"], "usage was not recorded"
assert t["status"] == "ok" and t["total_ms"] > 0
# An ordinary client call is one turn, and the aggregate copy at the top level agrees with the
# response's -- the interaction machinery must not change what a single request looks like.
assert t["turn_count"] == 1 and len(t["turns"]) == 1, (t["turn_count"], len(t["turns"]))
assert t["interaction_id"] is None, t["interaction_id"]
assert t["usage"] == t["response"]["usage"], (t["usage"], t["response"]["usage"])
assert t["turns"][0]["message_count"] == 1, t["turns"][0]["message_count"]
assert "messages" not in t["turns"][0], "the only turn duplicated the chain stored at the top level"
assert t["user_id"] == login, "user_id was not recorded as the API key's owner"
assert t["backend"]["provider"] and t["backend"]["base_url"], "backend did not record the provider"
assert api_key not in json.dumps(t, ensure_ascii=False), "the trace leaked the caller's plaintext API key"
print("trace", trace_id, "| model:", t["routing"]["model"], "| provider:", t["backend"]["provider"])
print("  rationale:", t["routing"]["analysis"]["rationale"])
print("  decision took:", t["routing"]["analysis"]["decision_latency_ms"], "ms")
print("  response excerpt:", repr(t["response"]["content"][:50]))
assert t["request"]["headers"].get("authorization") == "<redacted>", "the Authorization header was not redacted"
print("  Authorization header redacted OK")

# 6. Sticky-session trace (the second request should record a session analysis)
r2 = client.post(
    "/v1/chat/completions",
    json={"messages": [{"role": "user", "content": "and again"}], "max_tokens": 30},
    headers={"x-session-id": SESSION_ID},
)
assert r2.headers["x-trace-id"] != trace_id, (
    "a shared x-session-id must not fold two user requests into one record -- a session is a "
    "conversation, and each question in it is its own interaction"
)
t2 = client.get(f"/v1/traces/{r2.headers['x-trace-id']}").json()
assert t2["routing"]["reason"] == "session-sticky"
assert t2["routing"]["analysis"]["type"] == "session"
# Both sticky kinds render as a skipped decision, so the analysis says which key bound it.
assert t2["routing"]["analysis"]["bound_by"] == "session", t2["routing"]["analysis"]
assert t2["turn_count"] == 1, t2["turn_count"]
print("sticky trace analysis:", t2["routing"]["analysis"]["note"])

# 7. Streaming trace: the content is recorded in full once the stream ends
section("Streaming trace")
content = ""
with client.stream(
    "POST", "/v1/chat/completions",
    json={"messages": [{"role": "user", "content": "say OK"}], "max_tokens": 20, "stream": True},
) as r:
    stream_trace_id = r.headers["x-trace-id"]
    for line in r.iter_lines():
        if line.startswith("data: ") and line != "data: [DONE]":
            c = json.loads(line[6:])["choices"]
            if c and c[0].get("delta", {}).get("content"):
                content += c[0]["delta"]["content"]
t3 = client.get(f"/v1/traces/{stream_trace_id}").json()
assert t3["request"]["stream"] is True
assert t3["response"]["content"] == content, "the recorded streaming content does not match"
print("stream trace", stream_trace_id, "| recorded content matches the live stream:", repr(content[:40]))

# 8. The decision process under the rule strategy
section("Rule routing decision analysis")
saved = client.get("/v1/config").json()
saved["strategy"] = "rule"
client.put("/v1/config", json=saved)
r = client.post(
    "/v1/chat/completions",
    # Chinese on purpose: the live config.yaml's rule keywords are Chinese words, and keyword
    # matching is a plain substring test.
    json={"messages": [{"role": "user", "content": "这段代码报错了帮我 debug"}], "max_tokens": 400},
)
t4 = client.get(f"/v1/traces/{r.headers['x-trace-id']}").json()
assert t4["routing"]["analysis"]["type"] == "rule"
steps = t4["routing"]["analysis"]["evaluated"]
assert any(s["matched"] for s in steps), "no rule was recorded as matching"
for s in steps:
    print(" ", "✓" if s["matched"] else "·", s["rule"], "->", s.get("matched_keyword", s.get("check", "")))
saved["strategy"] = "ai"
client.put("/v1/config", json=saved)

# 9. Trace listing + on-disk persistence
section("Trace listing and persistence")
page = client.get("/v1/traces", params={"limit": 10}).json()
lst = page["items"]
assert len(lst) >= 4 and lst[0]["id"]
assert page["total"] >= len(lst), (page["total"], len(lst))
print(f"GET /v1/traces -> {len(lst)}/{page['total']} entries, newest:",
      lst[0]["id"], lst[0]["model"], lst[0]["reason"])
mine = list((LOG_DIR / "traces").glob(f"*/{_safe(login)}/*.json"))
assert len(mine) >= 4, f"there should be at least 4 trace files under {login}, found {len(mine)}"
print(f"{LOG_DIR.name}/traces/*/{_safe(login)}/ -> {len(mine)} persisted trace files")

# 10. Usage aggregation
section("Usage statistics")
u = client.get("/v1/usage", params={"days": 1}).json()
assert u["totals"]["requests"] >= 4, u["totals"]
assert any(m["model"] for m in u["by_model"]), "the per-model aggregation is empty"
print("GET /v1/usage?days=1 -> requests:", u["totals"]["requests"],
      "| tokens:", u["totals"].get("total_tokens"),
      "| models:", [m["model"] for m in u["by_model"]])

print("\nALL ENHANCED CHECKS PASSED")
