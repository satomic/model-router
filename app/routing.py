"""Routing strategies: rule (keyword/length rules) and ai (decision model). Both emit
the full decision analysis."""
import json
import logging
import re
import time
from typing import TYPE_CHECKING

from .config import RouterConfig

if TYPE_CHECKING:
    from .providers import ClientPool

logger = logging.getLogger("fmr")

# Clients such as Copilot wrap the real question in a <userRequest> tag, preceded by a
# large block of context
_USER_REQUEST_RE = re.compile(r"<userRequest>\s*(.*?)\s*</userRequest>", re.DOTALL)


def extract_user_prompt(messages: list[dict]) -> str:
    """Take the latest user message as the routing input; when it contains a
    <userRequest> tag, keep only the real question."""
    for msg in reversed(messages):
        if msg.get("role") == "user":
            content = msg.get("content", "")
            if isinstance(content, list):  # multimodal content parts
                content = " ".join(
                    p.get("text", "") for p in content if isinstance(p, dict)
                )
            matches = _USER_REQUEST_RE.findall(content or "")
            if matches:
                return matches[-1]
            return content or ""
    return ""


def truncate_for_decision(prompt: str, max_chars: int) -> str:
    """For an over-long prompt keep half of the head and half of the tail, so the real
    question at the end is not cut off."""
    if len(prompt) <= max_chars:
        return prompt
    half = max_chars // 2
    return f"{prompt[:half]}\n...[...omitted...]...\n{prompt[-half:]}"


def route_by_rules(prompt: str, cfg: RouterConfig) -> tuple[str, str, dict]:
    """Return (model, reason, analysis). analysis records how each rule was evaluated."""
    evaluated = []
    for rule in cfg.rules:
        name = rule.get("name", "unnamed")
        model = rule.get("model")
        step = {"rule": name, "model": model, "matched": False}
        if model not in cfg.models:
            step["skipped"] = f"model {model!r} is not in the models catalog"
            evaluated.append(step)
            continue
        min_chars = rule.get("min_prompt_chars")
        if min_chars:
            step["check"] = f"len(prompt)={len(prompt)} >= {min_chars}"
            if len(prompt) >= min_chars:
                step["matched"] = True
                evaluated.append(step)
                return model, name, {"type": "rule", "evaluated": evaluated}
        keywords = rule.get("keywords") or []
        if keywords:
            m = re.search(
                "|".join(re.escape(k) for k in keywords), prompt, re.IGNORECASE
            )
            step["check"] = f"keywords={list(keywords)}"
            if m:
                step["matched"] = True
                step["matched_keyword"] = m.group(0)
                evaluated.append(step)
                return model, name, {"type": "rule", "evaluated": evaluated}
        evaluated.append(step)
    return cfg.default_model, "default", {
        "type": "rule",
        "evaluated": evaluated,
        "fallback": "no rule matched, using the default model",
    }


async def route_by_ai(
    prompt: str, cfg: RouterConfig, pool: "ClientPool"
) -> tuple[str, str, dict]:
    """Return (model, reason, analysis). analysis records the decision model's input,
    output and rationale.

    The decision model also goes through the provider pool, so it can use a different
    endpoint/key than the serving models.
    """
    system_prompt = cfg.render_decision_prompt()
    truncated = truncate_for_decision(prompt, cfg.max_prompt_chars)
    decision = cfg.resolve_decision_model()
    analysis: dict = {
        "type": "ai",
        "decision_model": cfg.decision_model,
        "decision_provider": decision.provider.name,
        "decision_input": truncated[:500],
        "prompt_truncated": len(prompt) > cfg.max_prompt_chars,
        "candidates": list(cfg.models),
        # The prompt is configurable now, so the trace must keep the system content that
        # was actually sent; otherwise there is no way to tell afterwards which version a
        # historical request used
        "decision_system": system_prompt,
    }
    t0 = time.perf_counter()
    try:
        client = await pool.get(decision.provider, "chat")
        resp = await client.chat.completions.create(
            model=decision.upstream_model,
            messages=[
                {"role": "system", "content": system_prompt},
                {"role": "user", "content": truncated},
            ],
            max_tokens=120,
            temperature=0,
            response_format={"type": "json_object"},
            timeout=cfg.decision_timeout,
        )
        raw = resp.choices[0].message.content
        analysis["raw_response"] = raw
        analysis["decision_latency_ms"] = round((time.perf_counter() - t0) * 1000, 1)
        if resp.usage:
            analysis["decision_usage"] = {
                "prompt_tokens": resp.usage.prompt_tokens,
                "completion_tokens": resp.usage.completion_tokens,
            }
        data = json.loads(raw)
        choice = data.get("model")
        analysis["rationale"] = data.get("rationale")
        if choice in cfg.models:
            return choice, "ai-decision", analysis
        analysis["error"] = f"the decision model returned unknown model {choice!r}"
        logger.warning("AI decision returned unknown model %r, falling back to default", choice)
    except Exception as e:  # noqa: BLE001 a failed decision must not break the request
        analysis["decision_latency_ms"] = round((time.perf_counter() - t0) * 1000, 1)
        analysis["error"] = str(e)
        logger.warning("AI decision failed (%s), falling back to the default model", e)
    analysis["fallback"] = True
    return cfg.default_model, "ai-fallback-default", analysis
