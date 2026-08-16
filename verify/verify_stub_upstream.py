"""A local OpenAI-compatible stub upstream for end-to-end verification (no real Azure/Foundry
credentials needed).

    python verify_stub_upstream.py            # listens on 127.0.0.1:8899

It doubles as the target for verifying "an OpenAI-compatible address that is not Foundry": point a
provider at http://127.0.0.1:8899/v1 with api_type: openai.
"""
import json
import time
import uuid

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import StreamingResponse

app = FastAPI(title="Stub Upstream")


def _reply_for(model: str, messages: list[dict]) -> str:
    """Answer decision requests with JSON; answer ordinary requests with recognisable text."""
    system = next((m for m in messages if m.get("role") == "system"), None)
    if system and "model router" in str(system.get("content", "")).lower():
        catalog = str(system.get("content"))
        names = [
            line.split(":")[0].strip("- ").strip()
            for line in catalog.splitlines()
            if line.startswith("- ")
        ]
        prompt = str(messages[-1].get("content", ""))
        # Route refactor/architecture-style prompts to the last (stronger) model and everything
        # else to the first, so the difference in decisions is easy to observe. The Chinese
        # keywords are kept because the live config.yaml's rules use those exact words.
        pick = names[-1] if any(k in prompt for k in ("重构", "架构", "refactor")) else names[0]
        return json.dumps({"model": pick, "rationale": f"stub picked {pick}"}, ensure_ascii=False)
    return f"[stub:{model}] OK"


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body = await request.json()
    model = body.get("model") or "stub-model"
    text = _reply_for(model, body.get("messages") or [])
    cid = f"chatcmpl-{uuid.uuid4().hex[:12]}"
    created = int(time.time())
    usage = {
        "prompt_tokens": 42,
        "completion_tokens": max(1, len(text) // 4),
        "total_tokens": 42 + max(1, len(text) // 4),
    }

    if body.get("stream"):
        def gen():
            for ch in text:
                chunk = {
                    "id": cid, "object": "chat.completion.chunk", "created": created,
                    "model": model,
                    "choices": [{"index": 0, "delta": {"content": ch}, "finish_reason": None}],
                }
                yield f"data: {json.dumps(chunk, ensure_ascii=False)}\n\n"
            done = {
                "id": cid, "object": "chat.completion.chunk", "created": created,
                "model": model,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": usage,
            }
            yield f"data: {json.dumps(done)}\n\n"
            yield "data: [DONE]\n\n"

        return StreamingResponse(gen(), media_type="text/event-stream")

    return {
        "id": cid, "object": "chat.completion", "created": created, "model": model,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": text},
            "finish_reason": "stop",
        }],
        "usage": usage,
    }


@app.post("/v1/responses")
async def responses(request: Request):
    """The Responses API shape (models declaring api: responses land here)."""
    body = await request.json()
    model = body.get("model") or "stub-model"
    inputs = body.get("input") or []
    messages = [
        {"role": m.get("role"), "content": m.get("content")}
        for m in inputs if isinstance(m, dict)
    ]
    text = _reply_for(model, messages)
    return {
        "id": f"resp-{uuid.uuid4().hex[:12]}",
        "object": "response",
        "created_at": int(time.time()),
        "model": model,
        "status": "completed",
        "output": [{
            "type": "message", "role": "assistant",
            "content": [{"type": "output_text", "text": text}],
        }],
        "usage": {"input_tokens": 42, "output_tokens": 8, "total_tokens": 50},
    }


@app.get("/healthz")
async def healthz():
    return {"status": "ok", "stub": True}


if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=8899, log_level="warning")
