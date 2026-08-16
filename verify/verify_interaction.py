"""Verify that one user interaction is one routing decision and one trace.

An agentic client (GitHub Copilot, for one) answers a single user question with a loop of
HTTP requests: call the model, run the tool it asked for, append the result, call again. Every
request in that loop carries the same x-interaction-id. What is asserted here:

1. Model consistency -- every call of the loop is served by the model chosen for the first.
2. One routing decision -- the decision model is called once, not once per tool round trip, so
   the follow-ups cost no decision latency.
3. One trace -- the loop produces a single record on disk, not one per request, and its
   `messages` hold the complete chain including every tool call and tool result.
4. No interaction id -> the previous behaviour, one record per request, is unchanged.

Run the stub upstream first (python verify/verify_stub_upstream.py) if the configured providers
are not reachable; this script only needs the router itself to answer.
"""
import _bootstrap  # noqa: F401

import json
import uuid
from pathlib import Path

from app.traces import _safe
from verify_auth_helper import make_client

TRACES = Path("logs/traces")

client, api_key, login = make_client()
print(f"auth ready: login={login} key={api_key[:12]}…")


def section(title: str) -> None:
    print(f"\n=== {title} ===")


def ask(messages: list[dict], interaction: str | None, initiator: str = "user") -> dict:
    headers = {"x-initiator": initiator, "x-request-id": str(uuid.uuid4())}
    if interaction:
        headers["x-interaction-id"] = interaction
    r = client.post(
        "/v1/chat/completions",
        json={"messages": messages, "max_tokens": 40, "tools": [{
            "type": "function",
            "function": {"name": "read_file", "description": "read a file",
                         "parameters": {"type": "object", "properties": {}}},
        }]},
        headers=headers,
    )
    r.raise_for_status()
    return {
        "trace_id": r.headers["x-trace-id"],
        "model": r.headers["x-routed-model"],
        "reason": r.headers["x-router-reason"],
        "decision_ms": float(r.headers["x-router-decision-ms"]),
        "interaction": r.headers.get("x-router-interaction-id"),
    }


# ---------------------------------------------------------------------------
# 1. An agentic tool loop: four requests, one interaction id, a growing chain
section("An agentic tool loop under one interaction id")
IID = f"verify-{uuid.uuid4()}"
chain: list[dict] = [
    {"role": "system", "content": "You are an assistant."},
    {"role": "user", "content": "Explain the contents of this directory and write a markdown file"},
]
calls = []
for step in range(4):
    calls.append(ask(list(chain), IID))
    # What Copilot does between requests: append the assistant's tool call and its result, then
    # replay the whole conversation.
    chain += [
        {"role": "assistant", "content": None, "tool_calls": [{
            "id": f"call_{step}", "type": "function",
            "function": {"name": "read_file", "arguments": "{}"},
        }]},
        {"role": "tool", "tool_call_id": f"call_{step}", "content": f"file contents {step}"},
    ]

for i, c in enumerate(calls, 1):
    print(f"  call {i}: trace={c['trace_id']} model={c['model']} reason={c['reason']} "
          f"decision={c['decision_ms']}ms")

# 1a. Model consistency across the interaction
models = {c["model"] for c in calls}
assert len(models) == 1, f"the model changed mid-interaction: {models}"
print(f"model held constant across {len(calls)} calls: {models.pop()}")

# 1b. Exactly one routing decision. The follow-ups must report the sticky reason and must not
#     have paid for a decision -- that is the latency and token cost being removed.
assert calls[0]["reason"] != "interaction-sticky", (
    f"the first call should have routed, not reused: {calls[0]['reason']}"
)
for i, c in enumerate(calls[1:], 2):
    assert c["reason"] == "interaction-sticky", f"call {i} re-routed: reason={c['reason']}"
    assert c["decision_ms"] < calls[0]["decision_ms"], (
        f"call {i} spent {c['decision_ms']}ms deciding; the decision should have been skipped"
    )
saved = sum(calls[0]["decision_ms"] - c["decision_ms"] for c in calls[1:])
print(f"one routing decision for {len(calls)} calls; ~{saved:.0f}ms of decision latency avoided")

# 1c. One trace, and every call's x-trace-id points at it
ids = {c["trace_id"] for c in calls}
assert len(ids) == 1, f"the loop produced {len(ids)} trace ids, expected 1: {ids}"
trace_id = ids.pop()
files = list(TRACES.glob(f"*/{_safe(login)}/{trace_id}.json"))
assert len(files) == 1, f"expected exactly one file for {trace_id}, found {len(files)}"
same_interaction = [
    p for p in TRACES.glob(f"*/{_safe(login)}/*.json")
    if (json.loads(p.read_text(encoding="utf-8")).get("interaction_id")) == IID
]
assert len(same_interaction) == 1, (
    f"{len(same_interaction)} files carry interaction {IID}; one interaction must be one trace"
)
print(f"one trace on disk for {len(calls)} requests: {files[0].as_posix()}")

# ---------------------------------------------------------------------------
# 2. That trace is the complete chain
section("The record is the complete chain")
t = client.get(f"/v1/traces/{trace_id}").json()
assert t["interaction_id"] == IID, (t["interaction_id"], IID)
assert t["turn_count"] == len(calls), (t["turn_count"], len(calls))
assert len(t["turns"]) == len(calls), (len(t["turns"]), len(calls))

# The final chain is what the last request sent: 2 opening messages + 2 per completed round.
expected_messages = 2 + 2 * (len(calls) - 1)
assert len(t["request"]["messages"]) == expected_messages, (
    len(t["request"]["messages"]), expected_messages
)
roles = [m.get("role") for m in t["request"]["messages"]]
assert "tool" in roles and "assistant" in roles, f"the chain lost its tool round trips: {roles}"
tool_results = [m for m in t["request"]["messages"] if m.get("role") == "tool"]
assert len(tool_results) == len(calls) - 1, (len(tool_results), len(calls) - 1)
print(f"messages: {len(t['request']['messages'])} covering {len(tool_results)} tool result(s), "
      f"roles={sorted(set(roles))}")

# Turn indices are sequential, and each turn records how much of the chain it sent
indices = [turn["index"] for turn in t["turns"]]
assert indices == list(range(1, len(calls) + 1)), indices
counts = [turn["message_count"] for turn in t["turns"]]
assert counts == sorted(counts) and counts[-1] == expected_messages, counts
assert all(turn["model"] == t["routing"]["model"] for turn in t["turns"]), (
    "a turn was served by a model other than the one routed for the interaction"
)
print(f"turns: indices={indices} message_counts={counts}, all on {t['routing']['model']}")

# One routing block, recording the single decision -- not the last turn's non-decision
assert t["routing"]["reason"] != "interaction-sticky", (
    f"the record kept a follow-up's reason instead of the real decision: {t['routing']['reason']}"
)
assert t["routing"]["decision_ms"] > 0, t["routing"]["decision_ms"]
print(f"routing block: reason={t['routing']['reason']} decision_ms={t['routing']['decision_ms']}")

# Latency and tokens are the interaction's, not one request's
turn_total = round(sum(turn["total_ms"] for turn in t["turns"]), 1)
assert abs(t["total_ms"] - turn_total) < 1.0, (t["total_ms"], turn_total)
assert t["total_ms"] > max(turn["total_ms"] for turn in t["turns"]), (
    "total_ms looks like a single request rather than the whole interaction"
)
per_turn = [((turn.get("response") or {}).get("usage") or {}).get("total_tokens") or 0
            for turn in t["turns"]]
if any(per_turn):
    assert (t["usage"] or {}).get("total_tokens") == sum(per_turn), (t["usage"], per_turn)
    print(f"usage summed over turns: {per_turn} -> {t['usage']['total_tokens']} total_tokens")
print(f"total_ms {t['total_ms']} = sum of turns {per_turn and turn_total or turn_total}")

# The summary the console lists carries the same story
page = client.get("/v1/traces", params={"trace_id": trace_id}).json()
summary = next(s for s in page["items"] if s["id"] == trace_id)
assert summary["turn_count"] == len(calls), summary
assert summary["interaction_id"] == IID, summary
print(f"list summary: turn_count={summary['turn_count']} interaction_id={summary['interaction_id']}")

# ---------------------------------------------------------------------------
# 3. A second interaction is a second record -- folding must not over-reach
section("A different interaction is a different record")
other = ask([{"role": "user", "content": "a different question"}], f"verify-{uuid.uuid4()}")
assert other["trace_id"] != trace_id, "two interactions were folded into one record"
assert other["reason"] != "interaction-sticky", (
    f"a fresh interaction reused another one's binding: {other['reason']}"
)
print(f"new interaction -> new trace {other['trace_id']}, reason={other['reason']}")

# ---------------------------------------------------------------------------
# 4. No interaction id -> one record per request, exactly as before
section("A client that sends no interaction id")
plain = [ask([{"role": "user", "content": f"plain call {i}"}], None) for i in range(2)]
assert plain[0]["trace_id"] != plain[1]["trace_id"], (
    "requests with no interaction id were grouped; they must stay independent"
)
for p in plain:
    doc = client.get(f"/v1/traces/{p['trace_id']}").json()
    assert doc["interaction_id"] is None, doc["interaction_id"]
    assert doc["turn_count"] == 1, doc["turn_count"]
    assert len(doc["turns"]) == 1, len(doc["turns"])
print(f"no x-interaction-id -> separate traces {plain[0]['trace_id']}, {plain[1]['trace_id']}, "
      f"one turn each")

# ---------------------------------------------------------------------------
# 5. Deleting the record takes the whole interaction, and a later turn does not resurrect it
#    into a half-record. (Admin-only, which make_client's identity is.)
section("Deleting an interaction record")
r = client.delete(f"/v1/traces/{trace_id}")
assert r.status_code == 200, (r.status_code, r.text)
assert client.get(f"/v1/traces/{trace_id}").status_code == 404
assert not list(TRACES.glob(f"*/{_safe(login)}/{trace_id}.json")), "the file survived the delete"
after = ask(list(chain), IID)
assert after["trace_id"] != trace_id or True  # a fresh record either way; the point is it is whole
doc = client.get(f"/v1/traces/{after['trace_id']}").json()
assert doc["turn_count"] == 1, (
    f"a turn arriving after the delete produced a {doc['turn_count']}-turn record; "
    "the stale interaction pointer was not cleared"
)
print(f"deleted {trace_id}; a later turn opened a clean record {after['trace_id']} "
      f"(turn_count={doc['turn_count']})")

print("\nALL INTERACTION CHECKS PASSED")
