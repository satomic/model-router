"""Verify Anthropic-compatible support: both client protocols, both upstream protocols, and
per-key model scope.

The point of this script is the *cross* product, not the diagonal. A router that only ever
answered an Anthropic client from an Anthropic upstream would pass a naive test and still be
useless, because the reason to convert at all is that the two choices are independent:

    /v1/chat/completions  ->  OpenAI upstream        (the existing path, kept honest)
    /v1/chat/completions  ->  Anthropic upstream
    /v1/messages          ->  OpenAI upstream
    /v1/messages          ->  Anthropic upstream

each in streaming and non-streaming form, so all eight combinations are exercised.

Structure and prerequisites:

* Part 1 is pure translation, no server and no network: it asserts the properties that are easy
  to get wrong and invisible end-to-end (system messages hoisted, a tool result becoming a user
  turn, consecutive same-role turns merged, a streamed tool call reassembled).
* Part 2 needs the router on 127.0.0.1:8000, a `stub` provider pointing at
  verify_stub_upstream.py for the OpenAI-upstream half, and a connection with
  `api_type: anthropic` for the other half. The Anthropic cases are **skipped with a message**
  rather than failed when no such connection is configured, the same gating verify_access.py
  uses for its enterprise token: a repository without Anthropic credentials must still be able
  to run everything else.
* Part 3 covers the per-key scope, including the two refusals that make it a security control
  rather than a filter: a scope may not name a model its owner cannot use, and a scoped key's
  own /v1/models must agree with what a call through it would actually reach.

Routing is forced with temporary keyword rules (the verify_rules.py pattern) because the caller's
`model` field is deliberately ignored by this router, so it is the only way to aim a request at a
particular connection. `strategy`, `rules` and `models` are restored in a `finally`.
"""
import _bootstrap  # noqa: F401

import json

from app import wire
from verify_auth_helper import make_client

client, admin_key, admin_login = make_client()

ok = fail = skip = 0
failures: list[str] = []


def check(cond, label, extra=""):
    global ok, fail
    if cond:
        ok += 1
        print(f"[OK  ] {label} {extra}")
    else:
        fail += 1
        failures.append(label)
        print(f"[FAIL] {label} {extra}")


def skipped(label, why):
    global skip
    skip += 1
    print(f"[SKIP] {label} ({why})")


# -- Part 1: translation, with no server involved -----------------------------
print("\n== translation ==")

req = {
    "messages": [
        {"role": "system", "content": "be terse"},
        {"role": "developer", "content": "and correct"},
        {"role": "user", "content": "first"},
        {"role": "user", "content": "second"},
        {"role": "assistant", "content": "", "tool_calls": [
            {"id": "call_1", "type": "function",
             "function": {"name": "lookup", "arguments": '{"q": "x"}'}},
        ]},
        {"role": "tool", "tool_call_id": "call_1", "content": "42"},
    ],
    "temperature": 1.6,
    "stop": ["END"],
    "user": "alice",
    "presence_penalty": 0.5,
}
body = wire.openai_request_to_anthropic(req, "up-model")
check(body["system"] == "be terse\n\nand correct", "system and developer messages hoisted")
# Merging keeps one block per original message rather than joining the texts: the
# protocol accepts several text blocks in one turn, and keeping them separate means
# nothing is invented about how the two messages were meant to be delimited.
check(body["messages"][0]["role"] == "user"
      and [b["text"] for b in body["messages"][0]["content"]] == ["first", "second"],
      "consecutive same-role turns merged into one turn of two blocks")
tool_turn = body["messages"][1]
check(tool_turn["role"] == "assistant"
      and tool_turn["content"][0]["type"] == "tool_use"
      and tool_turn["content"][0]["input"] == {"q": "x"},
      "assistant tool_calls became a tool_use block")
result_turn = body["messages"][2]
check(result_turn["role"] == "user"
      and result_turn["content"][0]["type"] == "tool_result"
      and result_turn["content"][0]["tool_use_id"] == "call_1",
      "tool result became a user turn with tool_result")
check(body["temperature"] == 1.0, "temperature clamped into 0..1", f"got {body['temperature']}")
check(body["stop_sequences"] == ["END"], "stop -> stop_sequences")
check(body["metadata"] == {"user_id": "alice"}, "user -> metadata.user_id")
check("presence_penalty" not in body, "unsupported parameter not forwarded")
check(wire.anthropic_unsupported_params(req) == ["presence_penalty"],
      "unsupported parameter reported for the trace")
check(body["max_tokens"] == wire.DEFAULT_MAX_TOKENS, "max_tokens defaulted when absent")

# The mirror direction: an Anthropic request from a client, converted to canonical form.
back = wire.anthropic_request_to_openai({
    "model": "ignored-by-the-router",
    "system": "be terse",
    "max_tokens": 64,
    "stop_sequences": ["END"],
    "messages": [
        {"role": "user", "content": [{"type": "text", "text": "hello"}]},
        {"role": "assistant", "content": [
            {"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": {"q": "x"}},
        ]},
        {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "toolu_1", "content": "42"},
        ]},
    ],
})
check(back["messages"][0] == {"role": "system", "content": "be terse"},
      "anthropic system -> a system message")
check(back["messages"][2]["tool_calls"][0]["function"]["arguments"] == '{"q": "x"}',
      "tool_use -> tool_calls with JSON arguments")
check(back["messages"][3]["role"] == "tool"
      and back["messages"][3]["tool_call_id"] == "toolu_1",
      "tool_result -> a tool message, emitted before its own turn")
check(back["stop"] == ["END"] and back["max_tokens"] == 64, "stop_sequences and max_tokens back")

# A streamed tool call, decoded and then re-encoded: the two halves of the stream path meet here.
decoder = wire.AnthropicStreamDecoder("routed-name")
events = [
    ("message_start", {"type": "message_start", "message": {
        "id": "msg_1", "usage": {"input_tokens": 11, "output_tokens": 0}}}),
    ("content_block_start", {"type": "content_block_start", "index": 0,
                             "content_block": {"type": "thinking", "thinking": ""}}),
    ("content_block_delta", {"type": "content_block_delta", "index": 0,
                             "delta": {"type": "thinking_delta", "thinking": "hmm"}}),
    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
    ("content_block_start", {"type": "content_block_start", "index": 1,
                             "content_block": {"type": "tool_use", "id": "toolu_9",
                                               "name": "lookup", "input": {}}}),
    ("content_block_delta", {"type": "content_block_delta", "index": 1,
                             "delta": {"type": "input_json_delta", "partial_json": '{"q":'}}),
    ("content_block_delta", {"type": "content_block_delta", "index": 1,
                             "delta": {"type": "input_json_delta", "partial_json": ' "x"}'}}),
    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "tool_use"},
                       "usage": {"output_tokens": 7}}),
    ("message_stop", {"type": "message_stop"}),
]
chunks = []
for name, data in events:
    chunks.extend(decoder.feed(name, data))
check(all("thinking" not in json.dumps(c) for c in chunks), "thinking blocks dropped, not leaked")
args = "".join(
    (tc.get("function") or {}).get("arguments") or ""
    for c in chunks
    for tc in (c["choices"][0]["delta"] or {}).get("tool_calls") or []
)
check(args == '{"q": "x"}', "streamed tool arguments reassembled", args)
check(chunks[-1]["choices"][0]["finish_reason"] == "tool_calls", "tool_use -> tool_calls finish")
check(chunks[-1].get("usage", {}).get("total_tokens") == 18, "usage carried on the final chunk")

encoder = wire.AnthropicEventEncoder("routed-name")
frames = []
for c in chunks:
    frames.extend(encoder.feed(c))
frames.extend(encoder.finish(chunks[-1].get("usage")))
names = [line[7:] for f in frames for line in f.splitlines() if line.startswith("event: ")]
check(names[0] == "message_start" and names[-1] == "message_stop",
      "re-encoded stream opens and closes correctly", " ".join(names))
check(names.count("content_block_start") == names.count("content_block_stop"),
      "every content block is closed")

resp = wire.anthropic_response_to_openai({
    "id": "msg_2", "stop_reason": "max_tokens",
    "content": [{"type": "text", "text": "hi"}],
    "usage": {"input_tokens": 3, "output_tokens": 4, "cache_read_input_tokens": 2},
}, "routed-name")
check(resp["model"] == "routed-name", "response reports the router's model name, not upstream's")
check(resp["choices"][0]["finish_reason"] == "length", "max_tokens -> length")
check(resp["usage"]["total_tokens"] == 7, "usage mapped")
check(resp["usage"]["prompt_tokens_details"]["cached_tokens"] == 2, "cache read tokens mapped")

mirrored = wire.openai_response_to_anthropic(resp, "routed-name")
check(mirrored["type"] == "message" and mirrored["role"] == "assistant"
      and mirrored["content"][0]["text"] == "hi" and mirrored["stop_reason"] == "max_tokens",
      "canonical response rendered back into an Anthropic message")


# -- Part 2: both protocols, both upstreams -----------------------------------
print("\n== end to end ==")

doc = client.get("/v1/config").json()
saved = {k: doc.get(k) for k in ("strategy", "rules", "models")}

providers = doc.get("providers") or {}
stub_name = next((n for n, p in providers.items() if (p or {}).get("api_type") == "openai"), None)
anthropic_name = next(
    (n for n, p in providers.items() if (p or {}).get("api_type") == "anthropic"), None
)
# The model bound to the Anthropic connection has to come from the live catalog: only the
# operator knows which deployment name that endpoint actually serves.
claude_model = next(
    (
        name for name, meta in (doc.get("models") or {}).items()
        if (meta or {}).get("provider") == anthropic_name
    ),
    None,
) if anthropic_name else None

STUB_MODEL = "verify-stub-model"
STUB_WORD = "STUBPROBE"
CLAUDE_WORD = "CLAUDEPROBE"


def sse_openai(response) -> tuple[str, list[dict]]:
    """Collect the text and the raw chunks of an OpenAI SSE body."""
    text, chunks_out, done = "", [], False
    for line in response.text.splitlines():
        if not line.startswith("data: "):
            continue
        payload = line[6:]
        if payload == "[DONE]":
            done = True
            continue
        chunk = json.loads(payload)
        chunks_out.append(chunk)
        text += ((chunk["choices"][0].get("delta") or {}).get("content") or "")
    assert done, "OpenAI SSE body did not end with [DONE]"
    return text, chunks_out


def sse_anthropic(response) -> tuple[str, list[str]]:
    """Collect the text and the event order of an Anthropic SSE body."""
    text, order = "", []
    for block in response.text.split("\n\n"):
        name = payload = None
        for line in block.splitlines():
            if line.startswith("event: "):
                name = line[7:]
            elif line.startswith("data: "):
                payload = line[6:]
        if not name:
            continue
        order.append(name)
        if name == "content_block_delta" and payload:
            delta = (json.loads(payload).get("delta") or {})
            if delta.get("type") == "text_delta":
                text += delta.get("text") or ""
    return text, order


try:
    if not stub_name:
        raise SystemExit(
            "no provider with api_type: openai is configured, so the OpenAI-upstream half "
            "cannot run. Add one pointing at verify_stub_upstream.py (base_url "
            "http://127.0.0.1:8899/v1) and run it, then try again."
        )
    doc["strategy"] = "rule"
    doc["models"] = dict(doc.get("models") or {})
    doc["models"][STUB_MODEL] = {
        "provider": stub_name,
        "model_name": "stub-model",
        "description": "temporary, created by verify_anthropic.py",
    }
    rules = [{"name": "verify-stub", "keywords": [STUB_WORD], "model": STUB_MODEL}]
    if claude_model:
        rules.append(
            {"name": "verify-claude", "keywords": [CLAUDE_WORD], "model": claude_model}
        )
    doc["rules"] = rules
    r = client.put("/v1/config", json=doc)
    check(r.status_code == 200, "temporary routing configuration accepted", r.text[:160])

    # 1 + 2. OpenAI client, OpenAI upstream. The existing path, asserted so a regression in it
    # shows up here rather than only in the older scripts.
    r = client.post("/v1/chat/completions", json={
        "messages": [{"role": "user", "content": f"{STUB_WORD} say hello"}],
        "max_tokens": 64,
    })
    check(r.status_code == 200 and r.headers.get("x-routed-model") == STUB_MODEL,
          "openai client -> openai upstream", f"{r.status_code} {r.headers.get('x-routed-model')}")
    check("[stub:" in json.dumps(r.json()), "openai upstream answer reached the caller")

    r = client.post("/v1/chat/completions", json={
        "messages": [{"role": "user", "content": f"{STUB_WORD} say hello"}],
        "max_tokens": 64, "stream": True,
    })
    text, chunks = sse_openai(r)
    check("[stub:" in text, "openai client -> openai upstream, streaming", repr(text[:40]))

    # 3 + 4. Anthropic client, OpenAI upstream: the conversion nobody thinks to test.
    r = client.post("/v1/messages", json={
        "model": "ignored",
        "messages": [{"role": "user", "content": f"{STUB_WORD} say hello"}],
        "max_tokens": 64,
    })
    data = r.json() if r.status_code == 200 else {}
    check(r.status_code == 200 and data.get("type") == "message"
          and data.get("role") == "assistant"
          and (data.get("content") or [{}])[0].get("type") == "text",
          "anthropic client -> openai upstream", f"{r.status_code} {r.text[:120]}")
    check(bool(data.get("stop_reason")) and (data.get("usage") or {}).get("input_tokens") is not None,
          "converted message carries stop_reason and usage")
    check(r.headers.get("x-routed-model") == STUB_MODEL,
          "router headers present on the anthropic path")

    r = client.post("/v1/messages", json={
        "model": "ignored",
        "messages": [{"role": "user", "content": f"{STUB_WORD} say hello"}],
        "max_tokens": 64, "stream": True,
    })
    text, order = sse_anthropic(r)
    check("[stub:" in text, "anthropic client -> openai upstream, streaming", repr(text[:40]))
    check(order[:2] == ["message_start", "content_block_start"]
          and order[-2:] == ["message_delta", "message_stop"],
          "emitted event order is valid", " ".join(order[:3] + ["..."] + order[-2:]))

    # 5-8. The Anthropic upstream half.
    if not claude_model:
        skipped("anthropic upstream cases",
                "no connection with api_type: anthropic (plus a model bound to it) is configured")
    else:
        r = client.post("/v1/chat/completions", json={
            "messages": [{"role": "user", "content": f"{CLAUDE_WORD} reply with the word ok"}],
            "max_tokens": 300,
        })
        data = r.json() if r.status_code == 200 else {}
        content = ((data.get("choices") or [{}])[0].get("message") or {}).get("content")
        check(r.status_code == 200 and r.headers.get("x-routed-model") == claude_model
              and bool(content),
              "openai client -> anthropic upstream", f"{r.status_code} {r.text[:160]}")
        check((data.get("usage") or {}).get("completion_tokens", 0) > 0,
              "converted completion carries usage")
        trace_id = r.headers.get("x-trace-id")
        trace = client.get(f"/v1/traces/{trace_id}").json() if trace_id else {}
        turn = (trace.get("turns") or [{}])[-1] if isinstance(trace, dict) else {}
        check(turn.get("protocol") == "anthropic",
              "trace records the upstream protocol", json.dumps(turn)[:160])
        check(turn.get("client_protocol") == "openai", "trace records the caller's protocol")

        r = client.post("/v1/chat/completions", json={
            "messages": [{"role": "user", "content": f"{CLAUDE_WORD} count 1 to 5, digits only"}],
            "max_tokens": 300, "stream": True,
        })
        text, chunks = sse_openai(r)
        check(bool(text.strip()), "openai client -> anthropic upstream, streaming", repr(text[:60]))
        check(chunks[-1]["choices"][0].get("finish_reason") is not None,
              "converted stream ends with a finish_reason")

        r = client.post("/v1/messages", json={
            "model": "ignored",
            "messages": [{"role": "user", "content": f"{CLAUDE_WORD} reply with the word ok"}],
            "max_tokens": 300,
        })
        data = r.json() if r.status_code == 200 else {}
        check(r.status_code == 200 and data.get("type") == "message"
              and (data.get("content") or [{}])[0].get("text"),
              "anthropic client -> anthropic upstream", f"{r.status_code} {r.text[:160]}")

        r = client.post("/v1/messages", json={
            "model": "ignored",
            "messages": [{"role": "user", "content": f"{CLAUDE_WORD} count 1 to 5, digits only"}],
            "max_tokens": 300, "stream": True,
        })
        text, order = sse_anthropic(r)
        check(bool(text.strip()), "anthropic client -> anthropic upstream, streaming",
              repr(text[:60]))
        check(order.count("content_block_start") == order.count("content_block_stop"),
              "every emitted content block is closed")

    # -- Part 3: per-key model scope ------------------------------------------
    print("\n== key scope ==")

    created = []

    def new_key(name, scope=None):
        payload = {"name": name}
        if scope is not None:
            payload["scope"] = scope
        r = client.post("/v1/keys", json=payload)
        if r.status_code != 200:
            return None, r
        created.append(r.json()["id"])
        return r.json(), r

    def models_for(key_plaintext):
        r = client.get("/v1/models", headers={"Authorization": f"Bearer {key_plaintext}"})
        return sorted(m["id"] for m in (r.json().get("data") or []))

    def route_with(key_plaintext, word):
        return client.post(
            "/v1/chat/completions",
            headers={"Authorization": f"Bearer {key_plaintext}"},
            json={"messages": [{"role": "user", "content": f"{word} hello"}], "max_tokens": 64},
        )

    plain, r = new_key("verify-scope-all")
    check(plain is not None and plain.get("scope") == {"kind": "all"},
          "a key created without a scope reads back as unrestricted",
          json.dumps((plain or {}).get("scope")))

    picked, r = new_key("verify-scope-models", {"kind": "models", "models": [STUB_MODEL]})
    check(picked is not None, "a models scope is accepted", r.text[:160])
    if picked:
        check(models_for(picked["key"]) == [STUB_MODEL],
              "/v1/models shows only the scoped model", str(models_for(picked["key"])))
        # The rule naming a different model cannot pull the request outside the scope: the
        # narrowed catalog is what the router sees, so the rule is skipped rather than obeyed.
        rr = route_with(picked["key"], CLAUDE_WORD if claude_model else STUB_WORD)
        check(rr.status_code == 200 and rr.headers.get("x-routed-model") == STUB_MODEL,
              "a scoped key cannot be routed outside its scope",
              f"{rr.status_code} {rr.headers.get('x-routed-model')}")

    if anthropic_name:
        typed, r = new_key("verify-scope-types", {"kind": "api_types", "api_types": ["anthropic"]})
        check(typed is not None, "an api_types scope is accepted", r.text[:160])
        if typed:
            listed = models_for(typed["key"])
            check(STUB_MODEL not in listed and (not claude_model or claude_model in listed),
                  "/v1/models shows only models of the scoped connection type", str(listed))
            if claude_model:
                rr = route_with(typed["key"], STUB_WORD)
                check(rr.status_code == 200
                      and rr.headers.get("x-routed-model") == claude_model,
                      "a type-scoped key stays on its own connection type",
                      f"{rr.status_code} {rr.headers.get('x-routed-model')}")
            # And the scope is editable afterwards, which was the explicit requirement.
            rr = client.patch(f"/v1/keys/{typed['id']}",
                              json={"scope": {"kind": "models", "models": [STUB_MODEL]}})
            check(rr.status_code == 200
                  and rr.json().get("scope") == {"kind": "models", "models": [STUB_MODEL]},
                  "a key's scope can be changed later", rr.text[:160])
            check(models_for(typed["key"]) == [STUB_MODEL],
                  "the edited scope takes effect immediately")
            rr = client.patch(f"/v1/keys/{typed['id']}", json={"scope": {"kind": "all"}})
            check(rr.status_code == 200 and rr.json().get("scope") == {"kind": "all"},
                  "a scope can be widened back to the owner's full set")
    else:
        skipped("api_types scope cases", "no connection with api_type: anthropic is configured")

    _bad, r = new_key("verify-scope-bad-kind", {"kind": "everything"})
    check(r.status_code == 400, "an unknown scope kind is refused", f"{r.status_code} {r.text[:100]}")
    _bad, r = new_key("verify-scope-bad-model", {"kind": "models", "models": ["no-such-model"]})
    check(r.status_code == 400, "an unknown model name is refused", f"{r.status_code} {r.text[:100]}")
    _bad, r = new_key("verify-scope-empty", {"kind": "models", "models": []})
    check(r.status_code == 400, "an empty selection is refused", f"{r.status_code} {r.text[:100]}")
    _bad, r = new_key("verify-scope-bad-type", {"kind": "api_types", "api_types": ["gemini"]})
    check(r.status_code == 400, "an unknown connection type is refused",
          f"{r.status_code} {r.text[:100]}")

    # A disabled flag and a scope must be independently patchable: the console saves one
    # without resending the other.
    if plain:
        rr = client.patch(f"/v1/keys/{plain['id']}", json={"disabled": True})
        check(rr.status_code == 200 and rr.json().get("disabled") is True
              and rr.json().get("scope") == {"kind": "all"},
              "patching disabled leaves the scope alone")
        rr = client.patch(f"/v1/keys/{plain['id']}",
                          json={"scope": {"kind": "models", "models": [STUB_MODEL]}})
        check(rr.status_code == 200 and rr.json().get("disabled") is True,
              "patching the scope leaves the disabled flag alone")
        rr = client.post(
            "/v1/chat/completions",
            headers={"Authorization": f"Bearer {plain['key']}"},
            json={"messages": [{"role": "user", "content": "hello"}]},
        )
        check(rr.status_code == 401, "a disabled key is still refused", str(rr.status_code))

    for key_id in created:
        client.delete(f"/v1/keys/{key_id}")
finally:
    doc = client.get("/v1/config").json()
    doc.update(saved)
    r = client.put("/v1/config", json=doc)
    print(f"\nconfiguration restored ({r.status_code})")

print(f"\n{ok} passed, {fail} failed, {skip} skipped")
assert not failures, "; ".join(failures)
print("ANTHROPIC COMPATIBILITY PASSED")
