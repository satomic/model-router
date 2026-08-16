"""End-to-end verification against a locally running router."""
import json

import httpx

BASE = "http://127.0.0.1:8000"


def chat(content: str, session: str | None = None, stream: bool = False):
    headers = {"x-session-id": session} if session else {}
    body = {"messages": [{"role": "user", "content": content}], "max_tokens": 600}
    if stream:
        body["stream"] = True
        with httpx.stream(
            "POST", f"{BASE}/v1/chat/completions", json=body, headers=headers, timeout=120
        ) as r:
            chunks = []
            for line in r.iter_lines():
                if line.startswith("data: ") and line != "data: [DONE]":
                    delta = json.loads(line[6:])["choices"]
                    if delta and delta[0].get("delta", {}).get("content"):
                        chunks.append(delta[0]["delta"]["content"])
            return r.headers, "".join(chunks)
    r = httpx.post(
        f"{BASE}/v1/chat/completions", json=body, headers=headers, timeout=120
    )
    r.raise_for_status()
    return r.headers, r.json()["choices"][0]["message"]["content"]


def show(label, headers, text):
    print(
        f"[{label}] model={headers['x-routed-model']} reason={headers['x-router-reason']} "
        f"decision_ms={headers['x-router-decision-ms']}\n  reply: {text[:60]!r}\n"
    )


# 1. AI routing: simple vs complex
h, t = chat("hello, how are you?")
show("simple", h, t)
assert h["x-routed-model"] == "gpt-4o", "a simple question should route to the default light model"

h, t = chat(
    "Prove that there are infinitely many primes p such that p+2 is also prime is "
    "unsolved; then design a distributed system architecture for real-time fraud "
    "detection with exactly-once semantics and explain the deep reasoning tradeoffs."
)
show("complex", h, t)
assert h["x-routed-model"] != "gpt-4o", "deep reasoning should route to a higher-tier model"

# 2. Session stickiness: a second request on the same session should stay bound
h1, _ = chat("hello", session="sess-abc")
h2, t2 = chat(
    "now prove the Riemann hypothesis with rigorous mathematical reasoning",
    session="sess-abc",
)
show("sticky-1st", h1, "")
show("sticky-2nd", h2, t2)
assert h2["x-router-reason"] == "session-sticky"
assert h2["x-routed-model"] == h1["x-routed-model"], "one session should keep one model"

# 3. Streaming responses
h, t = chat("count from 1 to 5", stream=True)
show("stream", h, t)
assert t, "a stream should carry content"

# 4. Decision log
r = httpx.get(f"{BASE}/v1/router/decisions?limit=10")
print(f"[decisions] {len(r.json())} recent entries, last:")
print(json.dumps(r.json()[-1], ensure_ascii=False, indent=2))

print("\nALL CHECKS PASSED")
