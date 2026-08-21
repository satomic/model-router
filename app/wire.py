"""Translation between the OpenAI chat-completions wire format and the Anthropic Messages
wire format, in both directions.

Why this exists: a caller may speak either protocol and an upstream connection may speak
either protocol, which is four combinations. Writing four pipelines would mean four places to
fix every bug, so one shape is chosen as canonical and the other is converted at the edges:

    client (either protocol) -> [canonical: OpenAI chat completions] -> upstream (either)

OpenAI is the canonical form because everything between the two edges already reads it: the
router extracts the prompt from `messages`, the rules match on it, the trace records it, and
the model policy filters the catalog it belongs to. Making Anthropic canonical would have
meant rewriting all of that; making it a second first-class internal form would have meant
maintaining both.

Streaming follows the same rule with the same reasoning: an Anthropic upstream's events are
decoded into OpenAI chunks (AnthropicStreamDecoder) and an Anthropic client's events are
emitted from OpenAI chunks (AnthropicEventEncoder). Two parsers and two emitters over one
internal vocabulary, rather than four stream pipelines.

Nothing here performs IO or reads configuration: it is pure dict-to-dict, so it is testable
without a server and without an upstream.
"""
from __future__ import annotations

import json
import secrets
import time

# Anthropic requires max_tokens on every request; OpenAI treats it as optional. A request that
# arrives without one still has to be sent, so it gets this ceiling. Deliberately large: this
# is a cap, not a target, and a small default would truncate long answers for callers who
# never asked for a limit.
DEFAULT_MAX_TOKENS = 8192

# Anthropic stop_reason -> OpenAI finish_reason. "pause_turn" and "refusal" are newer values;
# both end the turn from the caller's point of view, so they map to "stop" rather than to
# nothing, because a null finish_reason on a completed response makes clients hang.
_STOP_REASON_TO_FINISH = {
    "end_turn": "stop",
    "stop_sequence": "stop",
    "max_tokens": "length",
    "tool_use": "tool_calls",
    "pause_turn": "stop",
    "refusal": "stop",
}
_FINISH_TO_STOP_REASON = {
    "stop": "end_turn",
    "length": "max_tokens",
    "tool_calls": "tool_use",
    "content_filter": "refusal",
    "function_call": "tool_use",
}

# Sampling parameters OpenAI has and Anthropic does not. Dropped rather than approximated: a
# frequency penalty silently reinterpreted as something else is worse than one not applied.
_UNSUPPORTED_BY_ANTHROPIC = ("presence_penalty", "frequency_penalty", "seed", "n", "logprobs")


def new_id(prefix: str) -> str:
    return f"{prefix}{secrets.token_hex(12)}"


def _text_of(content) -> str:
    """Flatten a content value (string, or a list of blocks) to plain text."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for block in content:
            if isinstance(block, str):
                parts.append(block)
            elif isinstance(block, dict) and block.get("type") in ("text", "input_text"):
                parts.append(str(block.get("text") or ""))
        return "".join(parts)
    return "" if content is None else str(content)


# ---------------------------------------------------------------------------
# OpenAI request -> Anthropic request
# ---------------------------------------------------------------------------
def _image_block_from_openai(part: dict) -> dict | None:
    """Convert an OpenAI image_url part to an Anthropic image block."""
    url = ((part.get("image_url") or {}) if isinstance(part.get("image_url"), dict) else {}).get(
        "url"
    ) or part.get("url")
    if not url:
        return None
    if url.startswith("data:"):
        # data:<media_type>;base64,<data>
        try:
            header, data = url.split(",", 1)
            media_type = header[5:].split(";", 1)[0] or "image/png"
        except ValueError:
            return None
        return {
            "type": "image",
            "source": {"type": "base64", "media_type": media_type, "data": data},
        }
    return {"type": "image", "source": {"type": "url", "url": url}}


def _content_blocks_from_openai(content) -> list[dict]:
    """OpenAI message content (string or multimodal parts) -> Anthropic content blocks."""
    if content is None:
        return []
    if isinstance(content, str):
        return [{"type": "text", "text": content}] if content else []
    blocks: list[dict] = []
    for part in content if isinstance(content, list) else []:
        if isinstance(part, str):
            if part:
                blocks.append({"type": "text", "text": part})
            continue
        if not isinstance(part, dict):
            continue
        kind = part.get("type")
        if kind in ("text", "input_text"):
            text = str(part.get("text") or "")
            if text:
                blocks.append({"type": "text", "text": text})
        elif kind in ("image_url", "input_image", "image"):
            block = _image_block_from_openai(part)
            if block:
                blocks.append(block)
    return blocks


def _tools_to_anthropic(tools) -> list[dict]:
    out: list[dict] = []
    for tool in tools or []:
        if not isinstance(tool, dict):
            continue
        fn = tool.get("function") if isinstance(tool.get("function"), dict) else tool
        name = fn.get("name")
        if not name:
            continue
        entry: dict = {"name": name, "input_schema": fn.get("parameters") or {"type": "object"}}
        if fn.get("description"):
            entry["description"] = fn["description"]
        out.append(entry)
    return out


def _tool_choice_to_anthropic(choice):
    if choice in (None, "auto"):
        return None  # Anthropic's own default
    if choice == "required":
        return {"type": "any"}
    if choice == "none":
        return {"type": "none"}
    if isinstance(choice, dict):
        name = (choice.get("function") or {}).get("name") or choice.get("name")
        if name:
            return {"type": "tool", "name": name}
    return None


def openai_request_to_anthropic(payload: dict, upstream_model: str) -> dict:
    """Canonical OpenAI chat-completions request -> Anthropic Messages request body.

    Consecutive same-role messages are merged, because Anthropic rejects two user turns in a
    row while OpenAI accepts them, and Copilot-style agent loops do produce them (a tool
    result followed by a user note is two user turns).
    """
    system_parts: list[str] = []
    messages: list[dict] = []

    for msg in payload.get("messages") or []:
        if not isinstance(msg, dict):
            continue
        role = msg.get("role")
        if role in ("system", "developer"):
            text = _text_of(msg.get("content"))
            if text:
                system_parts.append(text)
            continue
        if role == "tool":
            # An OpenAI tool result is its own message; in Anthropic it is a user-turn block.
            block = {
                "type": "tool_result",
                "tool_use_id": msg.get("tool_call_id") or "",
                "content": _text_of(msg.get("content")),
            }
            messages.append({"role": "user", "content": [block]})
            continue
        if role == "assistant":
            blocks = _content_blocks_from_openai(msg.get("content"))
            for call in msg.get("tool_calls") or []:
                if not isinstance(call, dict):
                    continue
                fn = call.get("function") or {}
                try:
                    args = json.loads(fn.get("arguments") or "{}")
                except (TypeError, ValueError):
                    # A partially generated argument string is not worth failing the request
                    # over: send it through as a single field the tool can at least see.
                    args = {"_raw": fn.get("arguments")}
                blocks.append(
                    {
                        "type": "tool_use",
                        "id": call.get("id") or new_id("toolu_"),
                        "name": fn.get("name") or "",
                        "input": args if isinstance(args, dict) else {"_raw": args},
                    }
                )
            if blocks:
                messages.append({"role": "assistant", "content": blocks})
            continue
        # Everything else is a user turn
        blocks = _content_blocks_from_openai(msg.get("content"))
        if blocks:
            messages.append({"role": "user", "content": blocks})

    # Merge consecutive same-role turns
    merged: list[dict] = []
    for msg in messages:
        if merged and merged[-1]["role"] == msg["role"]:
            merged[-1]["content"] = merged[-1]["content"] + msg["content"]
        else:
            merged.append({"role": msg["role"], "content": list(msg["content"])})

    # Anthropic requires at least one message. An empty list can only come from a request whose
    # only content was a system prompt; a single empty user turn keeps it valid.
    if not merged:
        merged = [{"role": "user", "content": [{"type": "text", "text": ""}]}]

    body: dict = {
        "model": upstream_model,
        "messages": merged,
        "max_tokens": int(
            payload.get("max_tokens")
            or payload.get("max_completion_tokens")
            or DEFAULT_MAX_TOKENS
        ),
    }
    if system_parts:
        body["system"] = "\n\n".join(system_parts)
    if payload.get("temperature") is not None:
        # Anthropic's range is 0..1; OpenAI allows up to 2, and a value above 1 is rejected
        # rather than clamped upstream, so it is clamped here.
        body["temperature"] = max(0.0, min(1.0, float(payload["temperature"])))
    if payload.get("top_p") is not None:
        body["top_p"] = float(payload["top_p"])
    stop = payload.get("stop")
    if stop:
        body["stop_sequences"] = [stop] if isinstance(stop, str) else list(stop)
    if payload.get("stream"):
        # Anthropic reports token counts on message_start / message_delta unconditionally,
        # so unlike OpenAI there is no stream_options flag to ask for usage.
        body["stream"] = True
    tools = _tools_to_anthropic(payload.get("tools"))
    if tools:
        body["tools"] = tools
        choice = _tool_choice_to_anthropic(payload.get("tool_choice"))
        if choice:
            body["tool_choice"] = choice
    if payload.get("user"):
        body["metadata"] = {"user_id": str(payload["user"])}
    return body


def anthropic_unsupported_params(payload: dict) -> list[str]:
    """Which OpenAI-only parameters were dropped, for the trace record."""
    dropped = [k for k in _UNSUPPORTED_BY_ANTHROPIC if payload.get(k) not in (None, 1)]
    if payload.get("response_format"):
        dropped.append("response_format")
    return dropped


# ---------------------------------------------------------------------------
# Anthropic response -> OpenAI response
# ---------------------------------------------------------------------------
def _usage_to_openai(usage: dict | None) -> dict:
    usage = usage or {}
    prompt = int(usage.get("input_tokens") or 0)
    completion = int(usage.get("output_tokens") or 0)
    out = {
        "prompt_tokens": prompt,
        "completion_tokens": completion,
        "total_tokens": prompt + completion,
    }
    cached = usage.get("cache_read_input_tokens")
    if cached:
        out["prompt_tokens_details"] = {"cached_tokens": int(cached)}
    return out


def anthropic_response_to_openai(data: dict, model: str) -> dict:
    """Anthropic Messages response -> a chat.completion dict.

    `model` is the router's own model name rather than the upstream one, so a caller always
    sees the name it is entitled to see: which deployment answered is a routing detail that
    belongs in the trace and in x-routed-model, not in the response body.
    """
    text_parts: list[str] = []
    tool_calls: list[dict] = []
    for block in data.get("content") or []:
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind == "text":
            text_parts.append(str(block.get("text") or ""))
        elif kind == "tool_use":
            tool_calls.append(
                {
                    "id": block.get("id") or new_id("call_"),
                    "type": "function",
                    "function": {
                        "name": block.get("name") or "",
                        "arguments": json.dumps(block.get("input") or {}, ensure_ascii=False),
                    },
                }
            )
        # thinking / redacted_thinking blocks are deliberately not surfaced: they have no
        # OpenAI equivalent, and inventing one would put reasoning text into the message
        # content that the caller would render as the answer.

    message: dict = {"role": "assistant", "content": "".join(text_parts) or None}
    if tool_calls:
        message["tool_calls"] = tool_calls
    finish = _STOP_REASON_TO_FINISH.get(data.get("stop_reason") or "", "stop")
    return {
        "id": data.get("id") or new_id("chatcmpl-"),
        "object": "chat.completion",
        "created": int(time.time()),
        "model": model,
        "choices": [{"index": 0, "message": message, "finish_reason": finish}],
        "usage": _usage_to_openai(data.get("usage")),
    }


# ---------------------------------------------------------------------------
# Anthropic stream -> OpenAI chunks
# ---------------------------------------------------------------------------
class AnthropicStreamDecoder:
    """Turns an Anthropic SSE event stream into OpenAI chat.completion.chunk dicts.

    Stateful because the two formats disagree about where information lives: Anthropic sends a
    tool call's id and name once in content_block_start and then streams its arguments as
    partial JSON, while OpenAI repeats the tool-call index on every fragment. The decoder holds
    the block-index-to-tool-call-index mapping that bridges the two.
    """

    def __init__(self, model: str, response_id: str | None = None):
        self.model = model
        self.id = response_id or new_id("chatcmpl-")
        self.created = int(time.time())
        self._tool_index: dict[int, int] = {}  # content-block index -> tool_calls index
        self._next_tool_index = 0
        self._role_sent = False
        self.usage: dict = {}
        self.finish_reason: str | None = None

    def _chunk(
        self, delta: dict, finish_reason: str | None = None, usage: dict | None = None
    ) -> dict:
        chunk = {
            "id": self.id,
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}],
        }
        if usage:
            chunk["usage"] = usage
        return chunk

    def feed(self, event_type: str, data: dict) -> list[dict]:
        """One Anthropic event in, zero or more OpenAI chunks out."""
        out: list[dict] = []
        if event_type == "message_start":
            msg = data.get("message") or {}
            if msg.get("id"):
                # Keep the upstream id, so a trace and the client agree on which call this was
                self.id = msg["id"]
            self.usage.update(msg.get("usage") or {})
            if not self._role_sent:
                self._role_sent = True
                out.append(self._chunk({"role": "assistant", "content": ""}))
        elif event_type == "content_block_start":
            block = data.get("content_block") or {}
            index = int(data.get("index") or 0)
            if block.get("type") == "tool_use":
                tool_index = self._next_tool_index
                self._next_tool_index += 1
                self._tool_index[index] = tool_index
                out.append(
                    self._chunk(
                        {
                            "tool_calls": [
                                {
                                    "index": tool_index,
                                    "id": block.get("id") or new_id("call_"),
                                    "type": "function",
                                    "function": {
                                        "name": block.get("name") or "",
                                        "arguments": "",
                                    },
                                }
                            ]
                        }
                    )
                )
            elif block.get("type") == "text" and block.get("text"):
                out.append(self._chunk({"content": block["text"]}))
        elif event_type == "content_block_delta":
            delta = data.get("delta") or {}
            index = int(data.get("index") or 0)
            kind = delta.get("type")
            if kind == "text_delta" and delta.get("text"):
                out.append(self._chunk({"content": delta["text"]}))
            elif kind == "input_json_delta":
                tool_index = self._tool_index.get(index, 0)
                out.append(
                    self._chunk(
                        {
                            "tool_calls": [
                                {
                                    "index": tool_index,
                                    "function": {
                                        "arguments": delta.get("partial_json") or ""
                                    },
                                }
                            ]
                        }
                    )
                )
            # thinking_delta and signature_delta are dropped for the reason given in
            # anthropic_response_to_openai.
        elif event_type == "message_delta":
            delta = data.get("delta") or {}
            if delta.get("stop_reason"):
                self.finish_reason = _STOP_REASON_TO_FINISH.get(delta["stop_reason"], "stop")
            self.usage.update(data.get("usage") or {})
        elif event_type == "message_stop":
            # Usage rides on the final chunk rather than on a chunk of its own. Anthropic
            # reports token counts unconditionally, so they are known by now, and an extra
            # trailing chunk with an empty choices array is exactly what a client that never
            # asked for stream_options would not expect.
            out.append(self._chunk({}, self.finish_reason or "stop", self.openai_usage()))
        elif event_type == "error":
            # Surfaced as an exception rather than a chunk: the caller's stream has to end in a
            # way that says the answer is incomplete, and a chunk cannot say that.
            err = data.get("error") or {}
            raise RuntimeError(err.get("message") or "anthropic stream error")
        return out

    def openai_usage(self) -> dict:
        return _usage_to_openai(self.usage)


def iter_sse(raw_lines) -> list[tuple[str, dict]]:
    """Parse SSE `event:`/`data:` line pairs into (event_type, payload) tuples.

    Takes an iterable of already-decoded lines. Kept separate from the decoder so the decoder
    can be tested with hand-written events and this can be tested with hand-written bytes.
    """
    events: list[tuple[str, dict]] = []
    event_type = ""
    for line in raw_lines:
        line = line.rstrip("\r")
        if not line:
            continue
        if line.startswith("event:"):
            event_type = line[6:].strip()
        elif line.startswith("data:"):
            payload = line[5:].strip()
            if not payload or payload == "[DONE]":
                continue
            try:
                data = json.loads(payload)
            except ValueError:
                continue
            events.append((event_type or str(data.get("type") or ""), data))
    return events


# ---------------------------------------------------------------------------
# Anthropic request -> OpenAI request  (an Anthropic client hitting /v1/messages)
# ---------------------------------------------------------------------------
def _openai_content_from_anthropic(blocks) -> tuple[list | str, list[dict], list[dict]]:
    """Return (content, tool_calls, tool_results) for one Anthropic message's blocks."""
    parts: list[dict] = []
    tool_calls: list[dict] = []
    tool_results: list[dict] = []
    for block in blocks if isinstance(blocks, list) else []:
        if isinstance(block, str):
            parts.append({"type": "text", "text": block})
            continue
        if not isinstance(block, dict):
            continue
        kind = block.get("type")
        if kind == "text":
            parts.append({"type": "text", "text": str(block.get("text") or "")})
        elif kind == "image":
            source = block.get("source") or {}
            if source.get("type") == "base64":
                url = f"data:{source.get('media_type') or 'image/png'};base64,{source.get('data') or ''}"
            else:
                url = source.get("url") or ""
            if url:
                parts.append({"type": "image_url", "image_url": {"url": url}})
        elif kind == "tool_use":
            tool_calls.append(
                {
                    "id": block.get("id") or new_id("call_"),
                    "type": "function",
                    "function": {
                        "name": block.get("name") or "",
                        "arguments": json.dumps(block.get("input") or {}, ensure_ascii=False),
                    },
                }
            )
        elif kind == "tool_result":
            tool_results.append(
                {
                    "role": "tool",
                    "tool_call_id": block.get("tool_use_id") or "",
                    "content": _text_of(block.get("content")),
                }
            )
    if len(parts) == 1 and parts[0]["type"] == "text":
        return parts[0]["text"], tool_calls, tool_results
    return parts, tool_calls, tool_results


def anthropic_request_to_openai(body: dict) -> dict:
    """Anthropic Messages request body -> canonical OpenAI chat-completions payload.

    The `model` field is carried through unchanged: it is the caller's requested model and the
    router decides what to do with it exactly as it does for an OpenAI request.
    """
    messages: list[dict] = []
    system = body.get("system")
    if system:
        messages.append({"role": "system", "content": _text_of(system)})

    for msg in body.get("messages") or []:
        if not isinstance(msg, dict):
            continue
        role = msg.get("role") or "user"
        content = msg.get("content")
        if isinstance(content, str):
            messages.append({"role": role, "content": content})
            continue
        payload_content, tool_calls, tool_results = _openai_content_from_anthropic(content)
        # Tool results have to precede the turn they belong to, because in OpenAI they are
        # their own messages answering the previous assistant turn's tool calls.
        messages.extend(tool_results)
        if tool_calls:
            entry: dict = {"role": "assistant", "content": payload_content or None}
            entry["tool_calls"] = tool_calls
            messages.append(entry)
        elif payload_content or not tool_results:
            messages.append({"role": role, "content": payload_content})

    payload: dict = {"model": body.get("model"), "messages": messages}
    if body.get("max_tokens"):
        payload["max_tokens"] = int(body["max_tokens"])
    for key in ("temperature", "top_p"):
        if body.get(key) is not None:
            payload[key] = body[key]
    if body.get("stop_sequences"):
        payload["stop"] = list(body["stop_sequences"])
    if body.get("stream"):
        payload["stream"] = True
    tools = []
    for tool in body.get("tools") or []:
        if not isinstance(tool, dict) or not tool.get("name"):
            continue
        tools.append(
            {
                "type": "function",
                "function": {
                    "name": tool["name"],
                    "description": tool.get("description") or "",
                    "parameters": tool.get("input_schema") or {"type": "object"},
                },
            }
        )
    if tools:
        payload["tools"] = tools
        choice = body.get("tool_choice") or {}
        kind = choice.get("type") if isinstance(choice, dict) else None
        if kind == "any":
            payload["tool_choice"] = "required"
        elif kind == "none":
            payload["tool_choice"] = "none"
        elif kind == "tool" and choice.get("name"):
            payload["tool_choice"] = {
                "type": "function",
                "function": {"name": choice["name"]},
            }
    user = (body.get("metadata") or {}).get("user_id")
    if user:
        payload["user"] = str(user)
    return payload


# ---------------------------------------------------------------------------
# OpenAI response -> Anthropic response  (answering an Anthropic client)
# ---------------------------------------------------------------------------
def openai_response_to_anthropic(data: dict, model: str) -> dict:
    choice = (data.get("choices") or [{}])[0]
    message = choice.get("message") or {}
    blocks: list[dict] = []
    text = message.get("content")
    if isinstance(text, list):
        text = _text_of(text)
    if text:
        blocks.append({"type": "text", "text": text})
    for call in message.get("tool_calls") or []:
        fn = (call or {}).get("function") or {}
        try:
            args = json.loads(fn.get("arguments") or "{}")
        except (TypeError, ValueError):
            args = {"_raw": fn.get("arguments")}
        blocks.append(
            {
                "type": "tool_use",
                "id": call.get("id") or new_id("toolu_"),
                "name": fn.get("name") or "",
                "input": args if isinstance(args, dict) else {"_raw": args},
            }
        )
    usage = data.get("usage") or {}
    return {
        "id": data.get("id") or new_id("msg_"),
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": blocks,
        "stop_reason": _FINISH_TO_STOP_REASON.get(choice.get("finish_reason") or "stop", "end_turn"),
        "stop_sequence": None,
        "usage": {
            "input_tokens": int(usage.get("prompt_tokens") or 0),
            "output_tokens": int(usage.get("completion_tokens") or 0),
        },
    }


class AnthropicEventEncoder:
    """Emits Anthropic SSE events from OpenAI chat.completion.chunk dicts.

    Stateful for the mirror of the decoder's reason: Anthropic requires every content block to
    be opened and closed, and requires a tool call's id and name up front, so the encoder has
    to track which blocks it has opened and close them in order at the end.
    """

    def __init__(self, model: str, message_id: str | None = None):
        self.model = model
        self.id = message_id or new_id("msg_")
        self._started = False
        self._text_open = False
        self._next_index = 0
        self._text_index = 0
        self._tool_blocks: dict[int, int] = {}  # OpenAI tool_calls index -> block index
        self._stop_reason = "end_turn"
        self._output_tokens = 0
        self._input_tokens = 0

    @staticmethod
    def _event(name: str, data: dict) -> str:
        return f"event: {name}\ndata: {json.dumps(data, ensure_ascii=False)}\n\n"

    def start(self, usage: dict | None = None) -> str:
        self._started = True
        self._input_tokens = int((usage or {}).get("prompt_tokens") or 0)
        return self._event(
            "message_start",
            {
                "type": "message_start",
                "message": {
                    "id": self.id,
                    "type": "message",
                    "role": "assistant",
                    "model": self.model,
                    "content": [],
                    "stop_reason": None,
                    "stop_sequence": None,
                    "usage": {"input_tokens": self._input_tokens, "output_tokens": 0},
                },
            },
        )

    def feed(self, chunk: dict) -> list[str]:
        """One OpenAI chunk in, zero or more Anthropic SSE frames out."""
        frames: list[str] = []
        if not self._started:
            frames.append(self.start(chunk.get("usage")))
        choice = (chunk.get("choices") or [{}])[0]
        delta = choice.get("delta") or {}
        content = delta.get("content")
        if content:
            if not self._text_open:
                self._text_open = True
                self._text_index = self._next_index
                self._next_index += 1
                frames.append(
                    self._event(
                        "content_block_start",
                        {
                            "type": "content_block_start",
                            "index": self._text_index,
                            "content_block": {"type": "text", "text": ""},
                        },
                    )
                )
            frames.append(
                self._event(
                    "content_block_delta",
                    {
                        "type": "content_block_delta",
                        "index": self._text_index,
                        "delta": {"type": "text_delta", "text": content},
                    },
                )
            )
        for call in delta.get("tool_calls") or []:
            if not isinstance(call, dict):
                continue
            tool_index = int(call.get("index") or 0)
            fn = call.get("function") or {}
            if tool_index not in self._tool_blocks:
                # A tool call means no more text can be appended to the open text block, so it
                # is closed first: Anthropic blocks do not interleave.
                if self._text_open:
                    self._text_open = False
                    frames.append(
                        self._event(
                            "content_block_stop",
                            {"type": "content_block_stop", "index": self._text_index},
                        )
                    )
                block_index = self._next_index
                self._next_index += 1
                self._tool_blocks[tool_index] = block_index
                frames.append(
                    self._event(
                        "content_block_start",
                        {
                            "type": "content_block_start",
                            "index": block_index,
                            "content_block": {
                                "type": "tool_use",
                                "id": call.get("id") or new_id("toolu_"),
                                "name": fn.get("name") or "",
                                "input": {},
                            },
                        },
                    )
                )
            args = fn.get("arguments")
            if args:
                frames.append(
                    self._event(
                        "content_block_delta",
                        {
                            "type": "content_block_delta",
                            "index": self._tool_blocks[tool_index],
                            "delta": {"type": "input_json_delta", "partial_json": args},
                        },
                    )
                )
        if choice.get("finish_reason"):
            self._stop_reason = _FINISH_TO_STOP_REASON.get(choice["finish_reason"], "end_turn")
        usage = chunk.get("usage") or {}
        if usage.get("completion_tokens"):
            self._output_tokens = int(usage["completion_tokens"])
        if usage.get("prompt_tokens"):
            self._input_tokens = int(usage["prompt_tokens"])
        return frames

    def finish(self, usage: dict | None = None) -> list[str]:
        """Close every open block and end the message. Safe to call after zero chunks."""
        frames: list[str] = []
        if not self._started:
            frames.append(self.start(usage))
        if usage:
            self._output_tokens = int(usage.get("completion_tokens") or self._output_tokens)
            self._input_tokens = int(usage.get("prompt_tokens") or self._input_tokens)
        if self._text_open:
            self._text_open = False
            frames.append(
                self._event(
                    "content_block_stop",
                    {"type": "content_block_stop", "index": self._text_index},
                )
            )
        for block_index in sorted(self._tool_blocks.values()):
            frames.append(
                self._event(
                    "content_block_stop",
                    {"type": "content_block_stop", "index": block_index},
                )
            )
        self._tool_blocks.clear()
        frames.append(
            self._event(
                "message_delta",
                {
                    "type": "message_delta",
                    "delta": {"stop_reason": self._stop_reason, "stop_sequence": None},
                    "usage": {"output_tokens": self._output_tokens},
                },
            )
        )
        frames.append(self._event("message_stop", {"type": "message_stop"}))
        return frames

    def error(self, message: str) -> str:
        """An error frame, for a failure that happens after the stream has already opened."""
        return self._event(
            "error",
            {"type": "error", "error": {"type": "api_error", "message": message}},
        )
