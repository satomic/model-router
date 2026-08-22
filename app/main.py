"""Model Router: OpenAI-compatible entry point + configuration API + end-to-end
trace recording.

Authentication model:
- /v1/chat/completions, /v1/models  -> API key required (Authorization: Bearer mr_...)
- /v1/config                        -> administrators only (GitHub OAuth session)
- /v1/keys, /v1/usage, /v1/traces   -> signed-in users; non-admins see only their own data
- /v1/auth/*, /healthz, the console -> public (the console itself is served from /, and
                                       every non-API path falls through to it)
- /v1/release                       -> public (the header shows the version to everyone);
                                       forcing a check is administrators only
"""
import asyncio
import contextlib
import copy
import json
import logging
import time
import uuid
from collections import Counter, defaultdict
from contextlib import asynccontextmanager
from datetime import datetime, timezone

from fastapi import Body, FastAPI, Header, HTTPException, Request, Response
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import (
    FileResponse,
    JSONResponse,
    RedirectResponse,
    StreamingResponse,
)

from . import auth as authlib
from . import (
    ghadmin,
    ghcache,
    keypolicy,
    keyscope,
    localadmin,
    modelpolicy,
    release,
    scopepolicy,
    wire,
)
from .version import ISSUES_URL, RELEASES_URL, REPO_URL, VERSION
from .authstore import AuthStore
from .config import (
    CATALOG_PLACEHOLDER,
    CONFIG_PATH,
    DATA_DIR,
    DEFAULT_DECISION_PROMPT,
    LOG_DIR,
    ROOT,
    TEMPLATE_PATH,
    RouterConfig,
    ensure_config_file,
    load_config,
    load_raw,
    migrate_legacy_layout,
    save_raw,
    validate_raw,
)
from .providers import ClientPool
from .routing import (
    extract_user_prompt,
    route_by_ai,
    route_by_rules,
    route_combined,
    truncate_for_decision,
)
from .sessions import SessionStore
from .traces import TraceStore

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s"
)
logger = logging.getLogger("mr")

# Before the seeding check, never after: this is what stops an upgrade from finding the new
# data/config.yaml empty and quietly starting from the template with every credential gone.
for _moved in migrate_legacy_layout():
    logger.info("moved existing state under data/: %s", _moved)

if ensure_config_file():
    # Worth a line at INFO: on a fresh volume this is the difference between "my settings are
    # gone" and "this deployment started from the template", and the operator needs to know
    # the local-admin password change is waiting for them.
    logger.info(
        "created %s from %s -- sign in as the local administrator to configure it",
        CONFIG_PATH, TEMPLATE_PATH,
    )

cfg = load_config()
sessions = SessionStore(cfg.session_ttl, cfg.max_sessions)
traces = TraceStore(LOG_DIR / "traces")
authstore = AuthStore(DATA_DIR)
pool = ClientPool()


# The background refresh waits this long before its first run: startup must never block on
# GitHub, and a service that cannot start because github.com is slow is worse than one whose
# cache is a minute stale.
_CACHE_WARMUP_DELAY = 10.0
# After a failed refresh, wait this long instead of the full interval -- a GitHub outage is
# usually short, and an empty cache means every request pays a live probe meanwhile.
_CACHE_RETRY_DELAY = 300.0


async def _cache_refresh_loop() -> None:
    """Keep data/github/ warm.

    Every iteration is wrapped: a GitHub outage must not kill the loop, because a background
    task that dies silently is worse than no background task at all -- the cache would simply
    stop ageing forward and nobody would be told.
    The lease keeps N workers from all refreshing the shared data/ directory; losing it is
    not an error, it just means another worker is doing the work.
    """
    await asyncio.sleep(_CACHE_WARMUP_DELAY)
    while True:
        delay = ghcache.refresh_seconds(cfg)
        try:
            if not cfg.key_policy.get("enabled") or not cfg.gh_admin_token:
                # Nothing to cache while access control is off: the policy is what decides
                # which member lists are worth having.
                pass
            elif ghcache.acquire_lease():
                try:
                    await ghcache.refresh(cfg)
                finally:
                    ghcache.release_lease()
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001 the loop must outlive any single failure
            logger.warning("GitHub cache refresh failed: %s", e)
            delay = min(delay, _CACHE_RETRY_DELAY)
        await asyncio.sleep(delay)


@asynccontextmanager
async def lifespan(app: FastAPI):
    tasks = [
        asyncio.create_task(_cache_refresh_loop()),
        asyncio.create_task(release.loop()),
    ]
    yield
    for task in tasks:
        task.cancel()
    for task in tasks:
        with contextlib.suppress(asyncio.CancelledError):
            await task
    await pool.aclose()


app = FastAPI(title="Model Router", version=VERSION, lifespan=lifespan)
app.add_middleware(
    CORSMiddleware,
    allow_origins=["http://localhost:5173", "http://127.0.0.1:5173"],  # Vite dev server
    allow_credentials=True,  # the session cookie must travel cross-origin
    allow_methods=["*"],
    allow_headers=["*"],
    expose_headers=["*"],
)

# Request fields passed straight through to the backend model
_PASSTHROUGH_FIELDS = (
    "messages", "temperature", "top_p", "max_tokens", "max_completion_tokens",
    "stop", "n", "presence_penalty", "frequency_penalty", "tools", "tool_choice",
    "response_format", "seed", "user", "stream",
)


# -- Authentication shortcuts -------------------------------------------------
def _user(request: Request) -> dict:
    return authlib.require_user(request, authstore, cfg)


def _admin(request: Request) -> dict:
    return authlib.require_admin(request, authstore, cfg)


def _api_key(request: Request) -> dict:
    return authlib.require_api_key(request, authstore)


def _is_admin_login(login: str) -> bool:
    """Whether this login is an administrator, from either identity source.

    An API key record carries no admin flag -- it names its owner and nothing else -- so a call
    authenticated by a key has to ask the configuration, which is also what makes an admin list
    edit take effect on the very next request rather than when the key is next recreated.
    """
    return cfg.is_admin_login(login) or cfg.is_local_admin_login(login)


async def _decide_model(
    prompt: str, session_id: str | None, interaction_id: str | None = None,
    allowed: list[str] | None = None,
) -> tuple[str, str, float, dict]:
    """Return (model, reason, decision_ms, analysis). Includes session-sticky logic.

    Two binding keys, checked in that order. `interaction_id` is the tighter one: it holds
    the model constant across the tool-call loop of a single user question, which is what
    stops one question being routed N times. `session_id` is the broader, opt-in one: it
    holds a model across a whole conversation.

    `allowed` is the caller's effective model set from the model policy, or None when they are
    unrestricted. It narrows the catalog every strategy sees (see RouterConfig.restricted_to), so
    a model the caller may not use is unreachable through rules, through the decision model, and
    through the default-model substitution alike -- rather than being caught by a check that each
    of those three paths would have to remember to make.
    """
    t0 = time.perf_counter()
    # The narrowed view is what every branch below reads. `cfg` itself is untouched: it is module
    # state shared by every concurrent request, so a per-caller restriction may never be written
    # into it.
    view = cfg if allowed is None else cfg.restricted_to(allowed)

    if cfg.sticky:
        for key, kind in ((interaction_id, "interaction"), (session_id, "session")):
            if not key:
                continue
            cached = sessions.get(_bind_key(kind, key))
            if not cached:
                continue
            if cached not in view.models:
                # The binding predates a policy change, or two callers with different effective
                # sets share a session id. Either way a stale binding must not resurrect a model
                # this caller may no longer use, so the decision is made again.
                logger.info(
                    "sticky %s binding to %s dropped: not in the caller's effective model set",
                    kind, cached,
                )
                continue
            analysis = {
                # The console renders both kinds the same way -- a note saying the decision was
                # skipped -- so they share the analysis type.
                "type": "session",
                "note": f"{kind} {key} is already bound to model {cached}, "
                        "skipping the routing decision",
                "bound_by": kind,
            }
            reason = "interaction-sticky" if kind == "interaction" else "session-sticky"
            return cached, reason, (time.perf_counter() - t0) * 1000, analysis

    if cfg.strategy == "ai":
        model, reason, analysis = await route_by_ai(prompt, view, pool)
    elif cfg.strategy == "rule-then-ai":
        model, reason, analysis = await route_combined(prompt, view, pool)
    else:
        model, reason, analysis = route_by_rules(prompt, view)

    if allowed is not None:
        analysis["policy_models"] = list(view.models)

    if cfg.sticky:
        if interaction_id:
            sessions.set(_bind_key("interaction", interaction_id), model)
            analysis["interaction_bound"] = interaction_id
        if session_id:
            sessions.set(_bind_key("session", session_id), model)
            analysis["session_bound"] = session_id
    return model, reason, (time.perf_counter() - t0) * 1000, analysis


def _bind_key(kind: str, value: str) -> str:
    """Namespace the two kinds of sticky key so they share one store without an id of one
    kind ever being able to answer a lookup of the other."""
    return f"{kind}:{value}"


# The header GitHub Copilot puts on every request belonging to one user interaction: the
# initial question and each follow-up of its tool-call loop all carry the same value, while
# x-request-id differs per HTTP call. Read in order, so a client that sets a plainer name
# still gets grouped.
_INTERACTION_HEADERS = ("x-interaction-id", "x-conversation-id", "x-copilot-interaction-id")


def _interaction_id(request: Request) -> str | None:
    """The id tying this request to the user interaction it is part of, or None."""
    for name in _INTERACTION_HEADERS:
        value = (request.headers.get(name) or "").strip()
        if value:
            return value[:128]
    return None


def _finalize_trace(
    trace: dict, t_start: float, status: str,
    content: str | None = None, usage: dict | None = None,
    finish_reason: str | None = None, error: str | None = None,
    tool_calls: list | None = None,
) -> None:
    """Close this request out as one turn and hand it to the store.

    The store decides whether that turn opens a new record or appends to the interaction
    already underway -- see TraceStore.add. Everything here describes this HTTP request only;
    the interaction-level totals are computed at merge time.
    """
    trace["status"] = status
    trace["total_ms"] = round((time.perf_counter() - t_start) * 1000, 1)
    response = None
    if error:
        trace["error"] = error
    else:
        response = {
            "content": content,
            "finish_reason": finish_reason,
            "usage": usage,
            # The tool calls the model asked for. This is the half of an agentic chain that
            # used to be lost: the assistant message carrying them was only ever visible in
            # the *next* request's replayed messages, so the final turn -- which asks for no
            # tools -- made it look as though none had been requested at all.
            "tool_calls": tool_calls,
        }
        trace["response"] = response
    trace["backend"]["latency_ms"] = round(
        trace["total_ms"] - trace["routing"]["decision_ms"], 1
    )
    request = trace.get("request") or {}
    messages = request.get("messages") or []
    trace["turns"] = [{
        "ts": trace["ts"],
        "request_id": trace.get("request_id"),
        "initiator": trace.get("initiator"),
        "message_count": len(messages),
        # Kept so the merge can tell an appended chain from a rewritten one, then dropped for
        # the appended case -- the full chain is stored once at the top level.
        "messages": messages,
        "params": request.get("params"),
        "stream": request.get("stream"),
        "model": (trace.get("routing") or {}).get("model"),
        "deployment": (trace.get("backend") or {}).get("deployment"),
        # Both protocols are recorded per turn rather than once per interaction: one chain
        # can be answered by an Azure deployment on its first turn and a Claude endpoint on
        # its second, and a turn that does not say which is which cannot explain itself.
        "client_protocol": trace.get("client_protocol"),
        "protocol": (trace.get("backend") or {}).get("protocol"),
        "status": status,
        "total_ms": trace["total_ms"],
        "response": response,
        "error": error,
    }]
    traces.add(trace)


# Sensitive request headers are never written to a trace
_REDACTED_HEADERS = {"authorization", "api-key", "cookie", "proxy-authorization", "x-api-key"}


def _sanitized_headers(request: Request) -> dict:
    return {
        k: ("<redacted>" if k.lower() in _REDACTED_HEADERS else v)
        for k, v in request.headers.items()
    }


# -- The two protocol entry points --------------------------------------------
# A caller may speak either protocol and an upstream connection may speak either protocol, and
# the two choices are independent: an Anthropic client can be answered by an Azure deployment
# and an OpenAI client by a Claude endpoint. Rather than four pipelines, both entry points
# convert to one canonical form -- an OpenAI chat-completions payload -- which is what routing,
# the model policy and the trace record all already read. See app/wire.py.


@app.post("/v1/chat/completions")
async def chat_completions(
    request: Request,
    x_session_id: str | None = Header(default=None),
):
    key = _api_key(request)
    t_start = time.perf_counter()
    body = await request.json()
    if not body.get("messages"):
        raise HTTPException(status_code=400, detail="messages is required")
    ctx = await _prepare_call(request, body, key, x_session_id, t_start, "openai")
    return await _serve(ctx)


@app.post("/v1/messages")
async def anthropic_messages(
    request: Request,
    x_session_id: str | None = Header(default=None),
):
    """Anthropic Messages entry point, so a client configured with ANTHROPIC_BASE_URL pointing
    at this router works unchanged.

    Authenticated by the same mr_ key as /v1/chat/completions, accepted from either
    `x-api-key` or `Authorization: Bearer` because the two ecosystems send different headers.
    """
    key = _api_key(request)
    t_start = time.perf_counter()
    raw = await request.json()
    if not raw.get("messages"):
        raise HTTPException(status_code=400, detail="messages is required")
    body = wire.anthropic_request_to_openai(raw)
    ctx = await _prepare_call(request, body, key, x_session_id, t_start, "anthropic")
    return await _serve(ctx)


async def _prepare_call(
    request: Request, body: dict, key: dict, x_session_id: str | None,
    t_start: float, client_protocol: str,
) -> dict:
    """Everything both entry points do before an upstream is touched: resolve what the caller
    may use, route, build the trace, build the response headers.

    Returns the context `_serve` needs. Split out rather than inlined twice because every line
    of it -- the policy, the key scope, stickiness, the trace shape -- must behave identically
    whichever protocol the caller spoke, and two copies would drift.
    """
    # user_id comes from the API key's owner: Copilot BYOK never sends x-user-id, so it
    # used to be permanently null
    user_id = key["user_login"]
    messages = body.get("messages") or []
    prompt = extract_user_prompt(messages)
    interaction_id = _interaction_id(request)
    # The caller's effective model set, resolved before anything routes. An empty list is a
    # configured outcome rather than an error -- an operator can bind a scope to an empty group,
    # which is how "this user gets nothing yet" is expressed -- so it is refused here with the
    # reason, instead of being handed to a router that has no model to pick.
    allowed = await modelpolicy.allowed_models(cfg, user_id, _is_admin_login(user_id))
    if allowed is not None and not allowed:
        raise HTTPException(
            status_code=403,
            detail="no models are available to you under the current model policy; "
                   "ask an administrator to assign a model group",
        )
    # Then narrowed again by this key's own scope. Reported separately from the policy refusal
    # above, because the two have different owners: the first needs an administrator, the
    # second the user can fix themselves on the API keys page.
    scope = key.get("scope")
    allowed = keyscope.narrow(cfg, allowed, scope)
    if allowed is not None and not allowed:
        raise HTTPException(
            status_code=403,
            detail="this API key is scoped to models you cannot currently use; "
                   f"widen its scope on the API keys page (scope: {keyscope.describe(scope)})",
        )
    model, reason, decision_ms, analysis = await _decide_model(
        prompt, x_session_id, interaction_id, allowed
    )
    resolved = cfg.resolve_model(model)

    payload = {k: v for k, v in body.items() if k in _PASSTHROUGH_FIELDS}
    if resolved.reasoning:
        # Reasoning models: max_tokens -> max_completion_tokens, and no sampling params
        if "max_tokens" in payload:
            payload["max_completion_tokens"] = payload.pop("max_tokens")
        for p in ("temperature", "top_p", "presence_penalty", "frequency_penalty"):
            payload.pop(p, None)

    trace = {
        "id": str(uuid.uuid4())[:8],
        "ts": datetime.now(timezone.utc).isoformat(),
        "user_id": user_id,
        "api_key_id": key["id"],
        "api_key_name": key.get("name"),
        "api_key_scope": keyscope.describe(scope),
        "session_id": x_session_id,
        # What makes this request part of a user interaction rather than an isolated call.
        # None for a client that sends no such header, in which case every request is its own
        # interaction -- the behaviour this app had before.
        "interaction_id": interaction_id,
        "request_id": request.headers.get("x-request-id"),
        # Copilot marks a request the user triggered as "user" and the follow-ups of its tool
        # loop as "agent"; recorded per turn so the chain shows which is which.
        "initiator": request.headers.get("x-initiator"),
        "client_ip": request.client.host if request.client else None,
        # Which protocol the caller spoke. Worth recording even though the stored request is
        # always canonical: "the answer came back in a shape my client could not read" is
        # otherwise impossible to diagnose from a trace.
        "client_protocol": client_protocol,
        "strategy": cfg.strategy,
        "sticky": cfg.sticky,
        "prompt_preview": prompt[:120],
        "request": {
            "headers": _sanitized_headers(request),
            "messages": messages,
            "params": {k: v for k, v in body.items() if k != "messages"},
            "stream": bool(body.get("stream")),
        },
        "routing": {
            "model": model,
            "reason": reason,
            "decision_ms": round(decision_ms, 1),
            "analysis": analysis,
        },
        "backend": {
            "deployment": resolved.upstream_model,
            "api": resolved.api,
            "protocol": resolved.provider.protocol,
            "provider": resolved.provider.name,
            "base_url": resolved.provider.base_url,
            "api_type": resolved.provider.api_type,
            "sent_params": {k: v for k, v in payload.items() if k != "messages"},
        },
        "response": None,
        "status": "pending",
        "total_ms": None,
    }
    # Resolved now rather than at write time, so x-trace-id names the interaction record this
    # turn joins -- a per-request id would 404 on GET /v1/traces/<id>.
    traces.resolve_interaction(trace)
    logger.info(
        "route id=%s user=%s session=%s interaction=%s model=%s provider=%s protocol=%s->%s "
        "reason=%s decision_ms=%.1f",
        trace["id"], user_id, x_session_id, interaction_id, model, resolved.provider.name,
        client_protocol, resolved.provider.protocol, reason, decision_ms,
    )

    headers = {
        "x-trace-id": trace["id"],
        "x-routed-model": model,
        "x-router-reason": reason,
        "x-router-decision-ms": f"{decision_ms:.1f}",
    }
    if interaction_id:
        headers["x-router-interaction-id"] = interaction_id

    return {
        "model": model,
        "resolved": resolved,
        "payload": payload,
        "trace": trace,
        "headers": headers,
        "t_start": t_start,
        "client_protocol": client_protocol,
    }


async def _serve(ctx: dict):
    """Call the upstream in its own protocol and answer in the caller's.

    Four combinations reduce to two decisions taken independently: which upstream branch
    produces the canonical result, and which renderer turns it into the caller's shape.
    """
    resolved = ctx["resolved"]
    payload = ctx["payload"]
    trace = ctx["trace"]
    t_start = ctx["t_start"]
    streaming = bool(payload.get("stream"))
    chunks = None
    completion = None
    try:
        if resolved.provider.protocol == "anthropic":
            client = await pool.get(resolved.provider, "chat")
            body = wire.openai_request_to_anthropic(payload, resolved.upstream_model)
            # What actually went on the wire, which for a converted request is not what the
            # caller sent: a trace that showed only the caller's parameters could not explain
            # a max_tokens the caller never set.
            trace["backend"]["sent_params"] = {
                k: v for k, v in body.items() if k not in ("messages", "system")
            }
            dropped = wire.anthropic_unsupported_params(payload)
            if dropped:
                trace["backend"]["dropped_params"] = dropped
            if streaming:
                chunks = await _open_anthropic_stream(client, body, ctx["model"])
            else:
                data = await client.create(body)
                completion = wire.anthropic_response_to_openai(data, ctx["model"])
        elif resolved.api == "responses":
            client = await pool.get(resolved.provider, "responses")
            completion = await _complete_via_responses_api(
                client, resolved.upstream_model, payload
            )
            if streaming:
                chunks = _chunks_from_completion(completion)
        else:
            client = await pool.get(resolved.provider, "chat")
            if streaming:
                chunks = await _open_openai_stream(client, resolved.upstream_model, payload)
            else:
                resp = await client.chat.completions.create(
                    model=resolved.upstream_model, **payload
                )
                completion = resp.model_dump()

        if streaming:
            if ctx["client_protocol"] == "anthropic":
                body_iter = _sse_anthropic(chunks, trace, t_start, ctx["model"])
            else:
                body_iter = _sse_openai(chunks, trace, t_start)
            return StreamingResponse(
                body_iter, media_type="text/event-stream", headers=ctx["headers"]
            )
        return _finish_and_render(completion, ctx)
    except HTTPException:
        raise
    except Exception as e:  # noqa: BLE001
        if trace["status"] == "pending":
            _finalize_trace(trace, t_start, "error", error=str(e))
        logger.error(
            "backend call failed model=%s provider=%s: %s",
            ctx["model"], resolved.provider.name, e,
        )
        raise HTTPException(status_code=502, detail=f"backend model call failed: {e}")


def _finish_and_render(completion: dict, ctx: dict):
    """Record the turn, then answer in the caller's protocol."""
    choice = (completion.get("choices") or [{}])[0]
    message = choice.get("message") or {}
    _finalize_trace(
        ctx["trace"], ctx["t_start"], "ok",
        content=message.get("content"),
        usage=completion.get("usage"),
        finish_reason=choice.get("finish_reason"),
        tool_calls=message.get("tool_calls") or None,
    )
    if ctx["client_protocol"] == "anthropic":
        return JSONResponse(
            wire.openai_response_to_anthropic(completion, ctx["model"]),
            headers=ctx["headers"],
        )
    return JSONResponse(completion, headers=ctx["headers"])


class _StreamAccumulator:
    """Rebuilds the whole answer from canonical chunks, for the trace record.

    Tool calls stream in fragments: the name arrives on the first delta for an index and the
    arguments accumulate over later ones, so they are assembled by index rather than taken
    from any single chunk.
    """

    def __init__(self) -> None:
        self.parts: list[str] = []
        self.finish: str | None = None
        self.usage: dict | None = None
        self.calls: dict[int, dict] = {}

    def feed(self, chunk: dict) -> None:
        choice = (chunk.get("choices") or [None])[0]
        if choice:
            delta = choice.get("delta") or {}
            if delta.get("content"):
                self.parts.append(delta["content"])
            for tc in delta.get("tool_calls") or []:
                slot = self.calls.setdefault(
                    int(tc.get("index") or 0),
                    {"id": None, "type": "function",
                     "function": {"name": None, "arguments": ""}},
                )
                if tc.get("id"):
                    slot["id"] = tc["id"]
                if tc.get("type"):
                    slot["type"] = tc["type"]
                fn = tc.get("function") or {}
                if fn.get("name"):
                    slot["function"]["name"] = fn["name"]
                if fn.get("arguments"):
                    slot["function"]["arguments"] += fn["arguments"]
            if choice.get("finish_reason"):
                self.finish = choice["finish_reason"]
        if chunk.get("usage"):
            self.usage = chunk["usage"]

    def content(self) -> str:
        return "".join(self.parts)

    def tool_calls(self) -> list | None:
        return [self.calls[i] for i in sorted(self.calls)] or None


async def _open_openai_stream(client, model: str, payload: dict):
    """Open an OpenAI-protocol stream and return an async generator of canonical chunks.

    create() is awaited here rather than inside the generator so an upstream rejection (a bad
    key, an unknown deployment) still becomes a 502 with a message, instead of a 200 whose
    body dies on its first chunk.
    """
    stream = await client.chat.completions.create(model=model, **payload)

    async def gen():
        async for chunk in stream:
            yield chunk.model_dump()

    return gen()


async def _open_anthropic_stream(client, body: dict, model: str):
    """The same, for an Anthropic upstream: the first event is pulled here so an upstream
    error is a 502 rather than a truncated stream."""
    decoder = wire.AnthropicStreamDecoder(model)
    events = client.stream(body)
    try:
        first = await events.__anext__()
    except StopAsyncIteration:
        first = None

    async def gen():
        if first is not None:
            for chunk in decoder.feed(*first):
                yield chunk
        async for event, data in events:
            for chunk in decoder.feed(event, data):
                yield chunk

    return gen()


async def _chunks_from_completion(completion: dict):
    """A one-chunk stream, for an upstream that cannot stream (the Responses API path)."""
    choice = (completion.get("choices") or [{}])[0]
    message = choice.get("message") or {}
    yield {
        "id": completion.get("id"),
        "object": "chat.completion.chunk",
        "created": completion.get("created"),
        "model": completion.get("model"),
        "choices": [{
            "index": 0,
            "delta": {"role": "assistant", "content": message.get("content")},
            "finish_reason": choice.get("finish_reason") or "stop",
        }],
        "usage": completion.get("usage"),
    }


async def _sse_openai(chunks, trace: dict, t_start: float):
    """Canonical chunks -> an OpenAI SSE stream."""
    acc = _StreamAccumulator()
    try:
        async for chunk in chunks:
            acc.feed(chunk)
            yield f"data: {json.dumps(chunk, ensure_ascii=False, default=str)}\n\n"
        yield "data: [DONE]\n\n"
        _finalize_trace(
            trace, t_start, "ok",
            content=acc.content(), usage=acc.usage, finish_reason=acc.finish,
            tool_calls=acc.tool_calls(),
        )
    except Exception as e:  # noqa: BLE001
        _finalize_trace(trace, t_start, "error", error=str(e))
        raise


async def _sse_anthropic(chunks, trace: dict, t_start: float, model: str):
    """Canonical chunks -> an Anthropic SSE stream.

    A mid-stream failure is emitted as the protocol's own `error` event rather than re-raised:
    the response status is already sent, so raising would end the body with no explanation,
    and Anthropic clients do surface this event to the user.
    """
    acc = _StreamAccumulator()
    encoder = wire.AnthropicEventEncoder(model)
    try:
        async for chunk in chunks:
            acc.feed(chunk)
            for frame in encoder.feed(chunk):
                yield frame
        for frame in encoder.finish(acc.usage):
            yield frame
        _finalize_trace(
            trace, t_start, "ok",
            content=acc.content(), usage=acc.usage, finish_reason=acc.finish,
            tool_calls=acc.tool_calls(),
        )
    except Exception as e:  # noqa: BLE001
        _finalize_trace(trace, t_start, "error", error=str(e))
        yield encoder.error(str(e))


async def _complete_via_responses_api(client, model: str, payload: dict) -> dict:
    """Adapt a chat request to the Responses API and convert the result back to the
    chat.completion shape."""
    kwargs = {"model": model, "input": payload["messages"]}
    max_out = payload.get("max_completion_tokens") or payload.get("max_tokens")
    if max_out:
        kwargs["max_output_tokens"] = max_out
    resp = await client.responses.create(**kwargs)
    usage = resp.usage.model_dump() if resp.usage else None
    # Any OpenAI-compatible upstream can be configured, so a missing created_at must not
    # fail the whole request
    created = int(getattr(resp, "created_at", None) or time.time())
    return {
        "id": resp.id,
        "object": "chat.completion",
        "created": created,
        "model": model,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": resp.output_text},
            "finish_reason": "stop",
        }],
        "usage": usage,
    }


@app.get("/v1/models")
async def list_models(request: Request):
    key = _api_key(request)
    # Filtered by the model policy, because this endpoint is what a client's model picker is
    # populated from: listing a model the very next request would refuse is worse than not
    # listing it. An empty list is a legitimate answer -- see the 403 in chat_completions.
    login = key["user_login"]
    allowed = await modelpolicy.allowed_models(cfg, login, _is_admin_login(login))
    # Narrowed again by the key's own scope, for the same reason: a scoped key's picker must
    # show what that key can actually reach, not what its owner could reach with another key.
    allowed = keyscope.narrow(cfg, allowed, key.get("scope"))
    models = cfg.models if allowed is None else cfg.restricted_to(allowed).models
    return {
        "object": "list",
        "data": [
            {"id": name, "object": "model", "description": meta.get("description", "")}
            for name, meta in models.items()
        ],
    }


# -- Authentication -----------------------------------------------------------
@app.get("/v1/auth/status")
async def auth_status(request: Request):
    """Public: the frontend uses this to choose between the setup wizard, the sign-in
    page and the console."""
    user = authlib.current_user(request, authstore, cfg)
    return {
        "configured": cfg.oauth_configured,
        "authenticated": user is not None,
        "user": user,
        "can_setup": not cfg.oauth_configured and authlib.is_loopback(request),
        "callback_url": authlib.callback_url(request, cfg.gh_callback_url),
        # The console offers the local sign-in form on the strength of this flag, which is
        # what keeps it reachable when github.com is not.
        "local_admin_enabled": cfg.local_admin_enabled,
        "local_admin_username": cfg.local_admin_username if cfg.local_admin_enabled else "",
    }


@app.post("/v1/auth/local/login")
async def local_login(request: Request, payload: dict = Body(...)):
    """Sign in as the local super administrator.

    One message for both a wrong username and a wrong password, so this cannot be used to
    discover whether the account has been renamed. Note there is no rate limiting: this is
    the app's only brute-forceable surface, and a deployment exposed beyond a trusted
    network should disable the account or put a proxy in front of it.
    """
    if not cfg.local_admin_enabled:
        raise HTTPException(status_code=503, detail="the local administrator account is disabled")
    username = str(payload.get("username") or "").strip()
    password = str(payload.get("password") or "")
    if not cfg.is_local_admin_login(username) or not localadmin.verify_password(cfg, password):
        logger.warning("local admin sign-in failed username=%r", username[:64])
        raise HTTPException(status_code=401, detail="incorrect username or password")

    login = cfg.local_admin_username
    sid = authstore.create_session(
        {
            "login": login,
            "name": login,
            "avatar_url": None,
            "is_admin": True,
            # Marks the session's authority as coming from auth.local_admin. auth.py keys
            # both the is_admin recompute and the forced-change gate off this.
            "local_admin": True,
            "must_change_password": localadmin.must_change(cfg),
        },
        cfg.auth_session_ttl,
    )
    response = JSONResponse({
        "ok": True,
        "must_change_password": localadmin.must_change(cfg),
    })
    authlib.set_session_cookie(response, request, sid, cfg.auth_session_ttl)
    logger.info("local admin login login=%s must_change=%s", login, localadmin.must_change(cfg))
    return response


@app.post("/v1/auth/local/password")
async def local_admin_password(request: Request, payload: dict = Body(...)):
    """Change the local administrator's username and/or password.

    Exempt from the forced-change gate in auth.require_user -- it is the one thing an
    account still on the default credential is allowed to do.
    """
    global cfg
    user = _user(request)
    if not user.get("local_admin"):
        raise HTTPException(
            status_code=403,
            detail="only the local administrator can change this credential",
        )
    current = str(payload.get("current_password") or "")
    new_password = str(payload.get("new_password") or "")
    new_username = str(payload.get("new_username") or cfg.local_admin_username).strip()

    if not localadmin.verify_password(cfg, current):
        raise HTTPException(status_code=403, detail="the current password is incorrect")
    for problem in (
        localadmin.validate_username(new_username),
        localadmin.validate_new_password(new_password),
    ):
        if problem:
            raise HTTPException(status_code=422, detail=problem)

    salt, digest = localadmin.hash_password(new_password)
    # The whole auth section has to be written back: save_raw replaces top-level keys
    # wholesale, so submitting only local_admin would wipe the OAuth credentials.
    auth_doc = dict(load_raw().get("auth") or {})
    la = dict(auth_doc.get("local_admin") or {})
    la.update({
        "enabled": True,
        "username": new_username,
        "password_hash": digest,
        "password_salt": salt,
        "updated_at": datetime.now(timezone.utc).isoformat(),
    })
    auth_doc["local_admin"] = la
    cfg = save_raw({"auth": auth_doc})
    authstore.rekey_local_admin(request.cookies.get(authlib.SESSION_COOKIE), new_username)
    logger.info("local admin credential changed username=%s", new_username)
    return {"ok": True, "username": new_username}


@app.post("/v1/auth/local/enabled")
async def set_local_admin_enabled(request: Request, payload: dict = Body(...)):
    """Enable or disable the local administrator account (administrators only)."""
    global cfg
    _admin(request)
    enabled = bool(payload.get("enabled"))
    auth_doc = dict(load_raw().get("auth") or {})
    la = dict(auth_doc.get("local_admin") or {})
    la["enabled"] = enabled
    auth_doc["local_admin"] = la
    cfg = save_raw({"auth": auth_doc})
    logger.info("local admin account enabled=%s", enabled)
    return {"ok": True, "enabled": enabled}


@app.post("/v1/auth/setup")
async def auth_setup(request: Request, payload: dict = Body(...)):
    """First-run setup: available only while OAuth is unconfigured and the request comes
    from the local machine."""
    global cfg
    if cfg.oauth_configured:
        raise HTTPException(
            status_code=409,
            detail="OAuth is already configured; change it from the console instead",
        )
    if not authlib.is_loopback(request):
        raise HTTPException(
            status_code=403,
            detail="setup is only allowed from the local machine (127.0.0.1); "
                   "for a remote deployment, edit config.yaml directly",
        )
    client_id = (payload.get("client_id") or "").strip()
    client_secret = (payload.get("client_secret") or "").strip()
    admin_logins = [
        str(x).strip() for x in (payload.get("admin_logins") or []) if str(x).strip()
    ]
    if not client_id or not client_secret:
        raise HTTPException(
            status_code=422, detail="client_id and client_secret are required"
        )
    if not admin_logins:
        raise HTTPException(
            status_code=422, detail="at least one administrator GitHub login is required"
        )

    auth_doc = dict(load_raw().get("auth") or {})
    auth_doc["github"] = {
        "client_id": client_id,
        "client_secret": client_secret,
        "callback_url": (payload.get("callback_url") or "").strip(),
    }
    auth_doc["admin_logins"] = admin_logins
    auth_doc.setdefault("allow_any_github_user", True)
    auth_doc.setdefault("session_ttl_seconds", 7 * 24 * 3600)
    cfg = save_raw({"auth": auth_doc})
    logger.info("OAuth setup completed admins=%s", admin_logins)
    return {"ok": True, "callback_url": authlib.callback_url(request, cfg.gh_callback_url)}


@app.get("/v1/auth/github/login")
async def github_login(request: Request):
    if not cfg.oauth_configured:
        raise HTTPException(status_code=503, detail="GitHub OAuth is not configured")
    url, state = authlib.build_authorize_url(request, cfg)
    response = RedirectResponse(url)
    authlib.set_state_cookie(response, request, state)
    return response


@app.get("/v1/auth/github/callback")
async def github_callback(
    request: Request, code: str | None = None, state: str | None = None,
    error: str | None = None,
):
    if error:
        return RedirectResponse(f"/?login_error={error}")
    if not code:
        raise HTTPException(status_code=400, detail="missing code")
    expected = request.cookies.get(authlib.STATE_COOKIE)
    if not expected or state != expected:
        raise HTTPException(
            status_code=400, detail="state verification failed, please sign in again"
        )

    user = await authlib.exchange_code_for_user(request, cfg, code)
    sid = authstore.create_session(user, cfg.auth_session_ttl)
    response = RedirectResponse("/")
    authlib.set_session_cookie(response, request, sid, cfg.auth_session_ttl)
    response.delete_cookie(authlib.STATE_COOKIE, path="/")
    logger.info("login login=%s admin=%s", user["login"], user["is_admin"])
    return response


@app.post("/v1/auth/logout")
async def logout(request: Request, response: Response):
    authstore.delete_session(request.cookies.get(authlib.SESSION_COOKIE))
    authlib.clear_session_cookie(response, request)
    return {"ok": True}


# -- API key management -------------------------------------------------------
@app.get("/v1/keys")
async def list_keys(request: Request, all: bool = False):
    user = _user(request)
    scope = None if (all and user["is_admin"]) else user["login"]
    # scope is None only for the administrator's cross-user view, and that view must never
    # carry plaintext: an administrator may disable or delete anybody's key, not read it.
    # Anywhere else scope is the caller's own login, so they see their own keys in full.
    return authstore.list_api_keys(scope, include_secret=scope is not None)


@app.post("/v1/keys")
async def create_key(request: Request, payload: dict = Body(default={})):
    user = _user(request)
    # This is the enterprise access-control gate: without a key you cannot use BYOK, so
    # the authorization decision lives at "create key" rather than "sign in" -- signing
    # in is not authorization by itself.
    verdict = await keypolicy.evaluate(cfg, user["login"], bool(user["is_admin"]))
    if not verdict["allowed"]:
        logger.info("api key denied user=%s reason=%s", user["login"], verdict["reason"])
        raise HTTPException(status_code=403, detail=verdict["reason"])

    scope = await _validated_scope(payload.get("scope"), user, user)
    record, plaintext = authstore.create_api_key(
        user["login"], (payload.get("name") or "").strip() or "default", scope
    )
    logger.info(
        "api key created user=%s id=%s scope=%s",
        user["login"], record["id"], keyscope.describe(scope),
    )
    # record already carries the plaintext (the caller is by definition its owner); the
    # explicit key here keeps the response shape obvious at the call site.
    return {**record, "key": plaintext}


@app.patch("/v1/keys/{key_id}")
async def update_key(request: Request, key_id: str, payload: dict = Body(...)):
    user = _user(request)
    record = authstore.get_api_key(key_id)
    if record is None or (
        not user["is_admin"] and record["user_login"] != user["login"]
    ):
        raise HTTPException(status_code=404, detail="key not found")
    # Only the fields actually sent are touched, so the console can save a scope without
    # having to resend a disabled flag it is not editing (and vice versa).
    patch: dict = {}
    if "disabled" in payload:
        patch["disabled"] = bool(payload.get("disabled"))
    if "name" in payload:
        patch["name"] = (payload.get("name") or "").strip() or "default"
    if "scope" in payload:
        # Validated against the key's *owner*, not the caller: an administrator editing
        # somebody else's key must not be able to widen it past what that person may use.
        owner = {"login": record["user_login"], "is_admin": _is_admin_login(record["user_login"])}
        patch["scope"] = await _validated_scope(payload.get("scope"), owner, user)
    updated = authstore.set_api_key_fields(key_id, patch)
    if updated is None:
        raise HTTPException(status_code=404, detail="key not found")
    logger.info(
        "api key updated user=%s id=%s fields=%s",
        user["login"], key_id, ",".join(sorted(patch)),
    )
    return updated


async def _validated_scope(raw, owner: dict, actor: dict) -> dict:
    """Normalize an incoming key scope, and refuse one the owner could not use anyway.

    Explicitly named models are checked against the owner's current policy set, because the
    user picked them from a list and a name that is not on it is a mistake worth reporting
    now. Connection types are deliberately not checked: that scope is a rule, so selecting a
    type before an administrator has granted any model of that type is a legitimate thing to
    do, and it starts working on its own once they do.

    Two different people are consulted, on purpose. `owner` answers "which models may this key
    reach", because the key belongs to them. `actor` answers "may a scope be set at all", because
    that is a permission to perform an action and the person performing it is the caller -- an
    administrator narrowing somebody else's key is exercising their own authority, not that
    person's. The permission gate lives here rather than in the two endpoints so that no future
    caller can add a third way to set a scope and forget it.
    """
    try:
        scope = keyscope.normalize(raw, cfg)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e))
    if scope.get("kind") != keyscope.KIND_ALL:
        # Only narrowing is gated. Widening a key back to "everything its owner may reach" is
        # always allowed: it is the documented default, it carries no cost risk, and refusing it
        # would trap an already-narrowed key in place once the permission was withdrawn.
        verdict = await scopepolicy.evaluate(cfg, actor["login"], bool(actor["is_admin"]))
        if not verdict["allowed"]:
            logger.info(
                "key scope denied user=%s scope=%s reason=%s",
                actor["login"], keyscope.describe(scope), verdict["reason"],
            )
            raise HTTPException(status_code=403, detail=verdict["reason"])
    if scope.get("kind") == "models":
        allowed = await modelpolicy.allowed_models(
            cfg, owner["login"], bool(owner["is_admin"])
        )
        if allowed is not None:
            denied = [m for m in scope["models"] if m not in set(allowed)]
            if denied:
                raise HTTPException(
                    status_code=400,
                    detail="not available to you: " + ", ".join(denied),
                )
    return scope


@app.delete("/v1/keys/{key_id}")
async def delete_key(request: Request, key_id: str):
    user = _user(request)
    record = authstore.get_api_key(key_id)
    if record is None or (
        not user["is_admin"] and record["user_login"] != user["login"]
    ):
        raise HTTPException(status_code=404, detail="key not found")
    authstore.delete_api_key(key_id)
    logger.info("api key deleted user=%s id=%s", user["login"], key_id)
    return {"ok": True}


# -- Access control (enterprise policy) ---------------------------------------
@app.get("/v1/access/me")
async def access_me(request: Request):
    """A signed-in user asks whether they may create API keys, and on what evidence. The
    UI turns this into an explicit explanation."""
    user = _user(request)
    # Both verdicts in one request: the page asks them together, and the second is nested rather
    # than merged because the two share field names ("allowed", "reason") that must not collide.
    verdict, key_scope = await asyncio.gather(
        keypolicy.evaluate(cfg, user["login"], bool(user["is_admin"])),
        scopepolicy.evaluate(cfg, user["login"], bool(user["is_admin"])),
    )
    return {
        "login": user["login"],
        "is_admin": bool(user["is_admin"]),
        **verdict,
        "key_scope": key_scope,
    }


@app.get("/v1/access/token")
async def access_token_status(request: Request):
    """An administrator inspects the current token's owner and scopes. Returns **only a
    mask**; the plaintext is never echoed back."""
    _admin(request)
    token = cfg.gh_admin_token
    if not token:
        return {"configured": False, "hint": "", "owner": None, "error": None}
    hint = f"{token[:7]}...{token[-4:]}" if len(token) > 12 else "configured"
    try:
        owner = await ghadmin.verify_token(token)
        return {"configured": True, "hint": hint, "owner": owner, "error": None}
    except ghadmin.GitHubAdminError as e:
        return {"configured": True, "hint": hint, "owner": None, "error": str(e)}


@app.post("/v1/access/verify-token")
async def access_verify_token(request: Request, payload: dict = Body(default={})):
    """Validate a token (possibly an unsaved draft) and return its owner and scopes."""
    _admin(request)
    token = (payload.get("token") or "").strip() or cfg.gh_admin_token
    if not token:
        raise HTTPException(status_code=422, detail="enter a token first")
    try:
        return await ghadmin.verify_token(token)
    except ghadmin.GitHubAdminError as e:
        raise HTTPException(status_code=400, detail=str(e))


@app.get("/v1/access/discover")
async def access_discover(request: Request, refresh: bool = False):
    """Discover the enterprises visible to the token, plus each one's orgs and enterprise
    teams, for the policy configuration page to choose from.

    Served from data/github/structure.json unless ?refresh=1. That is what removes the
    latency this page used to have: enumerating the orgs of a large enterprise is several
    paginated GraphQL round trips, and the answer changes far less often than the page is
    opened. The response carries `cached` and `fetched_at` so the UI can say which it is
    rather than presenting stale data as live.
    """
    _admin(request)
    token = cfg.gh_admin_token
    if not token:
        raise HTTPException(
            status_code=422,
            detail="no GitHub Enterprise token configured; enter and save one to fetch "
                   "the enterprise list automatically",
        )
    if not refresh:
        cached = ghcache.cached_structure(cfg)
        if cached is not None:
            return cached
    if refresh:
        await ghadmin.invalidate_cache()
    try:
        discovered = await ghadmin.discover(token)
    except ghadmin.GitHubAdminError as e:
        raise HTTPException(status_code=400, detail=str(e))
    # Write the fresh structure back so the next page load is free, and so the refresh loop
    # has something to work from before its first tick.
    ghcache.store_structure(cfg, discovered.get("enterprises") or [])
    return {**discovered, "cached": False, "fetched_at": time.time()}


@app.get("/v1/access/cache")
async def access_cache_status(request: Request):
    """The state of the on-disk GitHub cache: ages, per-scope counts, truncation, errors."""
    _admin(request)
    return ghcache.status(cfg)


@app.post("/v1/access/cache/refresh")
async def access_cache_refresh(request: Request):
    """Force a refresh now, instead of waiting for the background loop."""
    _admin(request)
    if not cfg.gh_admin_token:
        raise HTTPException(
            status_code=422, detail="no GitHub Enterprise token configured"
        )
    return await ghcache.refresh(cfg)


# -- Model policy -------------------------------------------------------------
def _model_api_type(name: str) -> str:
    """The connection type a catalog model resolves through, or "" when its connection is gone.

    Resolved through the same provider lookup the router uses, so a model bound to a renamed or
    deleted connection reports nothing rather than a stale type.
    """
    provider = cfg.get_provider((cfg.models.get(name) or {}).get("provider"))
    return provider.api_type if provider is not None else ""


@app.get("/v1/models/available")
async def my_available_models(request: Request):
    """A signed-in user asks which models they may use, and why.

    Session-authenticated rather than key-authenticated, because this feeds the console's own
    "Available models" page. It is the same resolution the API path applies -- one call into
    app/modelpolicy -- so the page cannot drift from what a request would actually be allowed.

    `contributions` names only the grants that applied. Listing the teams and organizations the
    caller is *not* in would publish the policy tables to everybody who can sign in.
    """
    user = _user(request)
    verdict = await modelpolicy.evaluate(cfg, user["login"], bool(user["is_admin"]))
    return {
        "login": user["login"],
        "is_admin": bool(user["is_admin"]),
        **verdict,
        # The metadata the page renders next to each name. Taken from the catalog rather than
        # duplicated into the group, so a description edit shows up here without touching groups.
        "catalog": {
            name: {
                "description": (cfg.models.get(name) or {}).get("description", ""),
                "reasoning": bool((cfg.models.get(name) or {}).get("reasoning")),
                "default": bool((cfg.models.get(name) or {}).get("default")),
                # The connection type behind the model. Needed by the key scope editor, which
                # offers "every model of this type" as a scope and cannot describe what that
                # covers without knowing which type each model sits on.
                "api_type": _model_api_type(name),
            }
            for name in verdict["models"]
        },
        "default_model": (
            cfg.default_model if cfg.models and cfg.default_model in verdict["models"] else
            (verdict["models"][0] if verdict["models"] else None)
        ),
    }


# How many known logins one request will evaluate for key-creation permission, and how many of
# those evaluations may be in flight at once. Both are guards on a cost that is not the caller's:
# an uncached verdict is one or more live GitHub calls, so an unbounded loop over a long
# known_users.json would turn one page load into hundreds of API calls. Users past the cap are
# reported as "unknown" rather than quietly dropped -- see below.
_MAX_ELIGIBILITY_USERS = 200
_ELIGIBILITY_CONCURRENCY = 8


async def _key_eligibility(logins: list[str]) -> dict[str, bool]:
    """Map login -> may that login create an API key, under the saved key policy.

    Deliberately the same keypolicy.evaluate() the Keys page shows the user themselves, rather
    than a second reading of the config: a page that filters by its own idea of the rule would
    eventually disagree with the rule that is actually enforced, and an administrator would be
    configuring a list against a permission nobody has. Cache-first through ghcache means a
    deployment with a warm member list answers this with zero GitHub calls.

    A login whose evaluation raises is left out of the map, which the caller reports as unknown.
    """
    sem = asyncio.Semaphore(_ELIGIBILITY_CONCURRENCY)

    async def one(login: str) -> tuple[str, bool | None]:
        async with sem:
            try:
                verdict = await keypolicy.evaluate(cfg, login, cfg.is_admin_login(login))
                return login, bool(verdict.get("allowed"))
            except Exception as e:  # noqa: BLE001 a display filter must not fail the page
                logger.warning("key-creation eligibility failed for %s: %s", login, e)
                return login, None

    results = await asyncio.gather(*(one(login) for login in logins))
    return {login: allowed for login, allowed in results if allowed is not None}


@app.get("/v1/access/users")
async def list_signed_in_users(request: Request, eligibility: bool = False):
    """Administrators only: every login that has ever signed in.

    Read from data/known_users.json rather than from the session table -- sessions are purged
    when they expire, so they can only ever answer "who is signed in right now", which is not
    the question. This is what makes assigning a model group to a user possible without asking
    them to spell their GitHub login.

    `eligibility=1` adds `can_create_key` per user, which the key-scope allow list needs to offer
    only accounts that may create a key at all. It is opt-in because it costs a policy evaluation
    per user, and the model-policy table that shares this endpoint does not need it.
    """
    _admin(request)
    users = authstore.list_known_users()
    # The group each user currently resolves to by binding, so the admin table can show it
    # without a second round trip per row. Bindings are read straight from the policy: doing a
    # full modelpolicy.evaluate() per user would mean a GitHub membership check per row.
    bindings = {
        str(k).strip().lower(): v
        for k, v in ((cfg.model_policy.get("users") or {}).items())
    }

    allowed: dict[str, bool] = {}
    truncated = False
    if eligibility:
        logins = [str(u.get("login") or "") for u in users if u.get("login")]
        truncated = len(logins) > _MAX_ELIGIBILITY_USERS
        allowed = await _key_eligibility(logins[:_MAX_ELIGIBILITY_USERS])

    def decorate(u: dict) -> dict:
        row = {**u, "model_group": bindings.get(str(u.get("login", "")).lower()) or ""}
        if eligibility:
            # None, not False, when the verdict is unknown: "we could not tell" and "not allowed"
            # are different answers, and a page that hides its rows by this field must not hide a
            # row it never managed to evaluate.
            row["can_create_key"] = allowed.get(str(u.get("login") or ""))
        return row

    return {
        "users": [decorate(u) for u in users],
        "default_group": cfg.default_group,
        "policy_enabled": cfg.model_policy_enabled,
        "key_policy_enabled": bool(cfg.key_policy.get("enabled")),
        "eligibility_evaluated": bool(eligibility),
        "eligibility_truncated": truncated,
    }


# -- Usage statistics ---------------------------------------------------------
@app.get("/v1/usage")
async def usage(request: Request, days: int = 7, user_id: str | None = None):
    """Non-admins see only their own data; admins see everything or one named user."""
    user = _user(request)
    scope = user["login"] if not user["is_admin"] else user_id
    records = traces.scan(days=days, user_login=scope)

    by_model: Counter = Counter()
    by_day: defaultdict[str, dict] = defaultdict(
        lambda: {"requests": 0, "total_tokens": 0, "errors": 0}
    )
    by_user: defaultdict[str, dict] = defaultdict(
        lambda: {"requests": 0, "total_tokens": 0}
    )
    totals = {"requests": 0, "errors": 0, "prompt_tokens": 0,
              "completion_tokens": 0, "total_tokens": 0}
    latency_samples: list[float] = []

    for r in records:
        u = r.get("usage") or {}
        day = (r.get("ts") or "")[:10]
        is_error = r.get("status") == "error"
        totals["requests"] += 1
        totals["errors"] += int(is_error)
        for f in ("prompt_tokens", "completion_tokens", "total_tokens"):
            totals[f] += u.get(f) or 0
        by_model[r.get("model") or "unknown"] += 1
        by_day[day]["requests"] += 1
        by_day[day]["total_tokens"] += u.get("total_tokens") or 0
        by_day[day]["errors"] += int(is_error)
        owner = r.get("user_id") or "anonymous"
        by_user[owner]["requests"] += 1
        by_user[owner]["total_tokens"] += u.get("total_tokens") or 0
        if r.get("total_ms") is not None:
            latency_samples.append(r["total_ms"])

    latency_samples.sort()
    return {
        "scope": scope or "all",
        "is_admin": user["is_admin"],
        "days": days,
        "totals": {
            **totals,
            "avg_ms": round(sum(latency_samples) / len(latency_samples), 1)
            if latency_samples else None,
            "p95_ms": latency_samples[int(len(latency_samples) * 0.95) - 1]
            if latency_samples else None,
        },
        "by_model": [{"model": m, "requests": c} for m, c in by_model.most_common()],
        "by_day": [{"date": d, **v} for d, v in sorted(by_day.items())],
        "by_user": sorted(
            [{"user_id": k, **v} for k, v in by_user.items()],
            key=lambda x: x["requests"], reverse=True,
        ),
    }


# -- Configuration (administrators only) --------------------------------------
def _without_password_hash(raw: dict) -> dict:
    """Echo the configuration with the local administrator's password digest blanked.

    Every other secret in config.yaml (the OAuth client_secret, the enterprise token) is a
    credential the administrator reading this page is expected to manage, so it is returned
    for editing. The password digest is not: an ordinary GitHub administrator is a different
    principal from the local super administrator, and handing them a scrypt digest lets them
    attempt offline recovery of a password they were never given.

    The keys are kept and emptied rather than removed, so the console's round-trip
    (GET -> edit -> PUT) keeps its shape; `updated_at` is what the UI reads to tell whether a
    password has ever been set. put_config restores the stored values on the way back in.
    """
    doc = copy.deepcopy(raw)
    la = ((doc.get("auth") or {}).get("local_admin"))
    if isinstance(la, dict):
        for field in ("password_hash", "password_salt"):
            if field in la:
                la[field] = ""
    return doc


@app.get("/v1/config")
async def get_config(request: Request):
    _admin(request)
    return _without_password_hash(load_raw())


@app.post("/v1/config/decision-prompt/preview")
async def preview_decision_prompt(request: Request, payload: dict = Body(default={})):
    """Preview the rendered AI decision prompt (administrators only).

    Rendering goes through `RouterConfig.render_decision_prompt` -- the exact same code
    path as `route_by_ai` -- so the preview *is* the system content that would be sent,
    not an approximation reimplemented in the frontend.

    `models` / `ai_router` may be supplied as **unsaved drafts** in the request, so an
    admin can see the effect of a new catalog or prompt before pressing save.
    """
    _admin(request)
    raw = dict(load_raw() or {})
    if isinstance(payload.get("models"), dict):
        raw["models"] = payload["models"]
    if isinstance(payload.get("ai_router"), dict):
        raw["ai_router"] = payload["ai_router"]
    draft = RouterConfig(raw)

    catalog = draft.model_catalog_text()
    system = draft.render_decision_prompt(catalog)
    sample = str(payload.get("sample_prompt") or "")
    truncated = truncate_for_decision(sample, draft.max_prompt_chars) if sample else ""

    missing_desc = [
        name for name, meta in draft.models.items()
        if not str((meta or {}).get("description") or "").strip()
    ]
    return {
        "system": system,
        "catalog": catalog,
        "user": truncated,
        "sample_truncated": bool(sample) and len(sample) > draft.max_prompt_chars,
        "model_count": len(draft.models),
        "candidates": list(draft.models),
        "decision_model": draft.decision_model,
        "decision_provider": draft.resolve_decision_model().provider.name,
        "is_default_prompt": draft.decision_prompt == DEFAULT_DECISION_PROMPT,
        "has_placeholder": CATALOG_PLACEHOLDER in draft.decision_prompt,
        # A model with no description is just a bare name in the catalog, leaving the
        # decision model almost nothing to judge it by
        "models_without_description": missing_desc,
        "default_model": draft.default_model if draft.models else None,
        "chars": len(system),
    }


@app.get("/v1/config/decision-prompt/default")
async def default_decision_prompt(request: Request):
    """The built-in default prompt, for the UI's "Restore default" button (admins only)."""
    _admin(request)
    return {"prompt": DEFAULT_DECISION_PROMPT, "placeholder": CATALOG_PLACEHOLDER}


def _dropped_auth_credentials(old: dict, new: dict) -> list[str]:
    """Find the auth-section credentials this PUT would silently wipe.

    save_raw replaces whole top-level keys, so submitting only
    `{"auth": {"key_policy": ...}}` also erases the github credentials and admin_logins
    in that same section -- the result being that nobody can sign in any more, and
    client_secret cannot be read back from GitHub a second time. Rejecting such a
    submission explicitly avoids that damage.
    """
    if "auth" not in new:
        return []
    old_auth = dict((old.get("auth") or {}))
    new_auth = dict((new.get("auth") or {}))
    lost = []
    old_gh = dict(old_auth.get("github") or {})
    new_gh = dict(new_auth.get("github") or {})
    for field in ("client_id", "client_secret"):
        if str(old_gh.get(field) or "").strip() and field not in new_gh:
            lost.append(f"auth.github.{field}")
    if (old_auth.get("admin_logins") or []) and "admin_logins" not in new_auth:
        lost.append("auth.admin_logins")
    if str((old_auth.get("key_policy") or {}).get("github_token") or "").strip():
        if "key_policy" not in new_auth:
            lost.append("auth.key_policy.github_token")
    # Losing this one would silently reset the local super administrator to the documented
    # default password -- the most damaging thing a save from an unrelated tab could do.
    # A *blank* submitted hash is not a loss, though: GET /v1/config deliberately blanks it,
    # so every console save carries empty fields and _restored_password_hash puts the stored
    # digest back. Only dropping the key outright is treated as damage.
    if str((old_auth.get("local_admin") or {}).get("password_hash") or "").strip():
        if "local_admin" not in new_auth:
            lost.append("auth.local_admin.password_hash")
    return lost


def _restore_password_hash(old: dict, updates: dict) -> None:
    """Put the stored password digest back into an incoming auth section, in place.

    GET /v1/config blanks the digest (see _without_password_hash), so the console's own
    round-trip submits empty strings for it. Taking those at face value would reset the
    local super administrator to the default password on every unrelated save -- so an
    empty submitted field means "unchanged", and the only way to set the password remains
    POST /v1/auth/local/password, which requires the current one.
    """
    new_la = (updates.get("auth") or {}).get("local_admin")
    if not isinstance(new_la, dict):
        return
    old_la = (old.get("auth") or {}).get("local_admin") or {}
    for field in ("password_hash", "password_salt"):
        if not str(new_la.get(field) or "").strip() and str(old_la.get(field) or "").strip():
            new_la[field] = old_la[field]


@app.put("/v1/config")
async def put_config(request: Request):
    """Write the frontend configuration back to config.yaml (comments preserved) and hot-reload the runtime configuration."""
    global cfg, sessions
    _admin(request)
    updates = await request.json()
    existing = dict(load_raw() or {})
    _restore_password_hash(existing, updates)
    dropped = _dropped_auth_credentials(existing, updates)
    if dropped:
        raise HTTPException(
            status_code=422,
            detail=[
                "this submission would clear the following configured items: "
                + ", ".join(dropped)
                + ". The auth section is replaced as a whole, so submit these fields "
                "along with your changes (pass an empty string or empty list to clear "
                "one on purpose)."
            ],
        )
    merged = existing
    merged.update(updates)
    errors = validate_raw(merged)
    if errors:
        raise HTTPException(status_code=422, detail=errors)
    old_ttl, old_max = cfg.session_ttl, cfg.max_sessions
    old_policy = cfg.key_policy
    old_model_policy = (cfg.model_policy, cfg.model_groups)
    cfg = save_raw(updates)
    if (cfg.session_ttl, cfg.max_sessions) != (old_ttl, old_max):
        sessions = SessionStore(cfg.session_ttl, cfg.max_sessions)
    # A provider's endpoint/key may have changed, so drop the old clients
    await pool.invalidate()
    if (cfg.model_policy, cfg.model_groups) != old_model_policy:
        # Effective model sets are memoised for a minute; an admin who just edited a group
        # expects the change to be live when they check, not on the next tick.
        modelpolicy.invalidate()
    if cfg.key_policy != old_policy:
        # The token or the allow list changed, so cached decisions are no longer trustworthy.
        # The on-disk cache has to go too: a new token can see a different set of members, so
        # keeping the old member lists would leave them authoritative under a token that never
        # produced them.
        await ghadmin.invalidate_cache()
        ghcache.invalidate()
    authstore.refresh_admin_flags(cfg.is_admin_login)
    logger.info(
        "config updated: strategy=%s sticky=%s providers=%s",
        cfg.strategy, cfg.sticky, list(cfg.providers),
    )
    return {"ok": True, "strategy": cfg.strategy, "sticky": cfg.sticky}


# -- Trace queries ------------------------------------------------------------
@app.get("/v1/traces")
async def list_traces(
    request: Request,
    limit: int = 50,
    offset: int = 0,
    date: str | None = None,
    user_id: str | None = None,
    trace_id: str | None = None,
    session_id: str | None = None,
):
    """Monitoring: a page of call-trace summaries, newest first, read off disk.

    Returns {total, items, offset, limit, truncated} rather than a bare list, because a
    console that pages needs to know how much there is beyond the page it holds.
    Non-admin users are forcibly filtered down to their own records -- an overwrite rather
    than a rejection, so a client passing someone else's user_id simply sees its own.
    """
    user = _user(request)
    if not user["is_admin"]:
        user_id = user["login"]
    return traces.query(
        date=date,
        user_id=user_id,
        trace_id=trace_id,
        session_id=session_id,
        offset=offset,
        limit=min(max(limit, 1), 500),
    )


@app.delete("/v1/traces")
async def delete_traces(
    request: Request, date: str | None = None, user_id: str | None = None
):
    """Administrators only: delete every trace matching date and/or user.

    A criterion is required. An unfiltered DELETE would be a wipe-all -- not what this is
    for, and not undoable.
    """
    _admin(request)
    if not date and not user_id:
        raise HTTPException(status_code=422, detail="pass date and/or user_id")
    return {"deleted": traces.delete_many(date=date, user_id=user_id)}


@app.delete("/v1/traces/{trace_id}")
async def delete_trace(request: Request, trace_id: str):
    """Administrators only. A regular user cannot erase the record of their own calls --
    that is the point of keeping one."""
    _admin(request)
    if not traces.delete(trace_id):
        raise HTTPException(status_code=404, detail="trace not found")
    return {"ok": True, "deleted": 1}


@app.get("/v1/traces/{trace_id}")
async def get_trace(request: Request, trace_id: str):
    user = _user(request)
    trace = traces.get(trace_id)
    if trace is None:
        raise HTTPException(status_code=404, detail="trace not found")
    if not user["is_admin"] and trace.get("user_id") != user["login"]:
        raise HTTPException(status_code=404, detail="trace not found")
    return trace


@app.get("/v1/router/decisions")
async def recent_decisions(request: Request, limit: int = 50):
    """Legacy endpoint: routing-decision summaries derived from traces."""
    user = _user(request)
    scope = None if user["is_admin"] else user["login"]
    return list(reversed(traces.list(limit, user_id=scope)))


@app.get("/healthz")
async def healthz():
    return {
        "status": "ok",
        "strategy": cfg.strategy,
        "sticky": cfg.sticky,
        "providers": list(cfg.providers),
        # The console's header reads the version and the project links from here rather than
        # from a second request: it already polls this endpoint, and a build's identity belongs
        # with the rest of what it reports about itself.
        "version": VERSION,
        "repo_url": REPO_URL,
        "issues_url": ISSUES_URL,
        "releases_url": RELEASES_URL,
    }


@app.get("/v1/release")
async def release_status():
    """The last answer from the release check. Public, like /healthz.

    Never triggers a check of its own: the console asks on every load, and a page open in ten
    tabs must not become ten calls to github.com.
    """
    return release.status()


@app.post("/v1/release/check")
async def release_check(request: Request):
    """Force a check now. Administrators only: it makes an outbound request."""
    _admin(request)
    return await release.check()


_dist = ROOT / "frontend" / "dist"
_index = _dist / "index.html"

# Prefixes the API and the docs own. An *unknown* path underneath one of these has to keep
# 404ing as JSON -- answering it with the console shell would turn every client typo into a
# 200 page of HTML that a fetch() then fails to parse. "ui/" is listed because the old
# /ui/* namespace is deliberately gone, not silently redirected.
_RESERVED = ("v1/", "healthz", "docs", "redoc", "openapi.json", "ui/")

if _dist.exists():

    @app.get("/")
    async def index():
        return FileResponse(_index)

    # Registered last, so every concrete route above still wins -- and anything added below
    # this point would be unreachable. This is what lets a real URL such as /config/models
    # survive a reload or a paste into somebody else's browser.
    @app.get("/{full_path:path}", include_in_schema=False)
    async def spa(full_path: str):
        if full_path == "ui" or full_path.startswith(_RESERVED):
            raise HTTPException(status_code=404, detail="not found")
        candidate = (_dist / full_path).resolve()
        if candidate.is_relative_to(_dist.resolve()):
            if candidate.is_file():
                return FileResponse(candidate)  # assets/, favicon, ...
            # An extension in the last segment means a missing *file*, not a client route:
            # 404 it, so a stale build asking for a bundle that no longer exists fails
            # loudly instead of receiving index.html under a JavaScript content type.
            if "." in candidate.name:
                raise HTTPException(status_code=404, detail="not found")
        # An unknown application path: hand over the shell and let the router render its own
        # not-found view. The server has no list of client routes to check against.
        return FileResponse(_index)

else:

    @app.get("/")
    async def index():
        return RedirectResponse("/docs")
