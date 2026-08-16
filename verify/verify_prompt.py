"""Verify that the AI decision prompt is configurable, and check the preview endpoint.

Three things are covered:
1. The rendering logic itself (offline, pure functions) -- placeholder substitution, appending the
   catalog when the placeholder is missing, and braces not blowing up;
2. The preview endpoint: rendering from draft models / ai_router through the same code path as
   `route_by_ai`;
3. End to end: write a custom prompt into config.yaml, send one real request, and confirm the
   trace's `routing.analysis.decision_system` is exactly that custom version.

The script temporarily rewrites `ai_router` and **restores it verbatim** at the end (including
removing fields that did not exist before).
"""
import _bootstrap  # noqa: F401

import copy

from app.config import (
    CATALOG_PLACEHOLDER,
    DEFAULT_DECISION_PROMPT,
    RouterConfig,
    load_raw,
    validate_raw,
)
from verify_auth_helper import make_client

client, _api_key, _login = make_client()

ok = fail = 0


def check(cond, label, extra=""):
    global ok, fail
    if cond:
        ok += 1
        print(f"[OK  ] {label} {extra}")
    else:
        fail += 1
        print(f"[FAIL] {label} {extra}")


def section(title):
    print(f"\n=== {title} ===")


BASE_RAW = load_raw()
ORIGINAL_AI = copy.deepcopy(dict(BASE_RAW.get("ai_router") or {}))
HAD_PROMPT = "decision_prompt" in ORIGINAL_AI

# ── 1. Rendering logic (offline) ──────────────────────────────────────
section("Rendering logic")

models = {
    "fast-model": {"description": "light and fast", "default": True},
    "deep-model": {"description": "deep reasoning"},
    "no-desc": {},
}


def cfg_with(prompt=None):
    raw = copy.deepcopy(BASE_RAW)
    raw["models"] = copy.deepcopy(models)
    ai = dict(ORIGINAL_AI)
    if prompt is None:
        ai.pop("decision_prompt", None)
    else:
        ai["decision_prompt"] = prompt
    raw["ai_router"] = ai
    return RouterConfig(raw)


default_cfg = cfg_with(None)
check(
    default_cfg.decision_prompt == DEFAULT_DECISION_PROMPT,
    "a missing decision_prompt falls back to the built-in default",
)
check(
    cfg_with("   ").decision_prompt == DEFAULT_DECISION_PROMPT,
    "a blank string falls back too (clearing the field in the UI = back to the default)",
)

catalog = default_cfg.model_catalog_text()
check("- fast-model: light and fast" in catalog, "the catalog carries name and description")
check("- no-desc: " in catalog, "a model without a description is still listed by name")

rendered = default_cfg.render_decision_prompt()
check(CATALOG_PLACEHOLDER not in rendered, "the placeholder is substituted after rendering")
check(catalog in rendered, "the rendered result embeds the full catalog")
check(
    '{"model": "<model-name>"' in rendered,
    "the JSON braces in the default prompt survive verbatim (literal replacement, not str.format)",
)

# The key regression: bare braces in a user-supplied prompt used to raise KeyError via .format
tricky = f'Output {{"model": "x", "rationale": "y"}}. Candidates:\n{CATALOG_PLACEHOLDER}\nEnd {{{{ }}}}'
try:
    out = cfg_with(tricky).render_decision_prompt()
    check('{"model": "x"' in out and "{{" in out, "unescaped braces in a custom prompt still render")
except Exception as e:  # noqa: BLE001
    check(False, "unescaped braces in a custom prompt still render", f"raised {e!r}")

no_ph = cfg_with("You are a model router. Output JSON only, no explanation.")
out = no_ph.render_decision_prompt()
check(
    catalog in out and out.index("You are a model router") < out.index("- fast-model"),
    "without a placeholder the catalog is appended, so the decision model still sees candidates",
)

# ── 2. Validation rules ───────────────────────────────────────────────
section("Config validation")


def errs(ai_patch):
    raw = copy.deepcopy(BASE_RAW)
    raw["ai_router"] = {**ORIGINAL_AI, **ai_patch}
    return validate_raw(raw)


check(validate_raw(BASE_RAW) == [], "the existing config.yaml passes validation", validate_raw(BASE_RAW))
check(errs({"decision_prompt": tricky}) == [], "a valid prompt containing braces is accepted")
check(errs({"decision_prompt": ""}) == [], "an empty prompt is valid (= use the default)")
check(
    any("too short" in e for e in errs({"decision_prompt": "pick one"})),
    "an over-short prompt is rejected",
    errs({"decision_prompt": "pick one"}),
)
check(
    any("must be a string" in e for e in errs({"decision_prompt": 123})),
    "a non-string prompt is rejected",
)
check(
    any("decision_model" in e for e in errs({"decision_model": ""})),
    "an empty decision model is rejected",
)
check(
    any("is not in providers" in e for e in errs({"decision_provider": "no-such"})),
    "a decision provider that does not exist is rejected",
)
check(
    any("timeout_seconds" in e for e in errs({"timeout_seconds": 0})),
    "a non-positive timeout is rejected",
)

# ── 3. The preview endpoint ───────────────────────────────────────────
section("Preview endpoint")

live = load_raw()
draft_ai = {**dict(live.get("ai_router") or {}), "decision_prompt": tricky}
r = client.post(
    "/v1/config/decision-prompt/preview",
    json={"models": models, "ai_router": draft_ai, "sample_prompt": "refactor this architecture"},
)
r.raise_for_status()
p = r.json()
check(p["model_count"] == 3, "the preview uses the submitted draft catalog, not the models on disk", p["model_count"])
check(p["candidates"] == list(models), "the candidate list matches the draft")
check("- fast-model: light and fast" in p["system"], "a draft model's description shows up in the preview")
check(not p["is_default_prompt"], "is_default_prompt=false for a custom prompt")
check(p["has_placeholder"], "placeholder detection is correct")
check(p["models_without_description"] == ["no-desc"], "models missing a description are flagged")
check(p["default_model"] == "fast-model", "the fallback model is identified correctly")
check(p["user"] == "refactor this architecture" and not p["sample_truncated"], "a short sample is not truncated")

# The preview has to match the real rendering character for character, or it is a lie
expected = RouterConfig({**copy.deepcopy(live), "models": models, "ai_router": draft_ai}) \
    .render_decision_prompt()
check(p["system"] == expected, "the preview matches render_decision_prompt character for character")

r = client.post(
    "/v1/config/decision-prompt/preview",
    json={
        "models": models,
        "ai_router": {**draft_ai, "max_prompt_chars": 40},
        "sample_prompt": "long " * 200,
    },
)
r.raise_for_status()
long_p = r.json()
check(long_p["sample_truncated"], "an over-long sample is flagged as truncated")
check("[...omitted...]" in long_p["user"], "truncation keeps both ends and omits the middle")

r = client.post(
    "/v1/config/decision-prompt/preview",
    json={"models": models, "ai_router": {k: v for k, v in draft_ai.items()
                                          if k != "decision_prompt"}},
)
r.raise_for_status()
check(r.json()["is_default_prompt"], "a draft without a prompt previews the default one")

r = client.get("/v1/config/decision-prompt/default")
r.raise_for_status()
check(r.json()["prompt"] == DEFAULT_DECISION_PROMPT, "the default-prompt endpoint returns the built-in text")

# Non-administrators must not preview (the prompt is part of the configuration)
import httpx  # noqa: E402

from verify_auth_helper import BASE  # noqa: E402

r = httpx.post(f"{BASE}/v1/config/decision-prompt/preview", json={}, timeout=30)
check(r.status_code in (401, 403), "the preview endpoint rejects unauthenticated callers", r.status_code)

# ── 4. End to end: the custom prompt really goes out ──────────────────
section("End to end")

CUSTOM = (
    "You are a model router. Pick exactly one model from the list below and output JSON only: "
    "{\"model\": \"<name>\", \"rationale\": \"<one sentence>\"}.\n\n"
    "Available models:\n" + CATALOG_PLACEHOLDER + "\n\n"
    "VERIFY-PROMPT-MARKER: if this marker shows up in the rationale, this prompt took effect."
)

try:
    live = load_raw()
    put_ai = {**dict(live.get("ai_router") or {}), "decision_prompt": CUSTOM}
    r = client.put("/v1/config", json={"ai_router": put_ai})
    check(r.status_code == 200, "saving the custom prompt", r.text[:120] if r.status_code != 200 else "")
    r.raise_for_status()

    saved = dict(load_raw().get("ai_router") or {})
    check(saved.get("decision_prompt") == CUSTOM, "config.yaml now holds the custom prompt")
    check(
        saved.get("decision_model") == ORIGINAL_AI.get("decision_model"),
        "the other fields in the same section survived (the whole section is written back)",
    )

    strategy = load_raw().get("strategy")
    if strategy != "ai":
        print(f"[SKIP] strategy={strategy}, skipping the real request (AI routing is not enabled)")
    else:
        r = client.post(
            "/v1/chat/completions",
            # Chinese on purpose: this text has to hit the rule keywords in the live config.yaml.
            json={"messages": [{"role": "user", "content": "帮我重构这个模块的架构"}],
                  "max_tokens": 40},
        )
        r.raise_for_status()
        trace = client.get(f"/v1/traces/{r.headers['x-trace-id']}").json()
        analysis = trace["routing"]["analysis"]
        check(
            analysis.get("decision_system", "").startswith("You are a model router"),
            "the trace recorded the custom system prompt actually sent",
        )
        check(
            "VERIFY-PROMPT-MARKER" in analysis.get("decision_system", ""),
            "the custom prompt (marker included) really reached the decision model",
        )
        check(
            CATALOG_PLACEHOLDER not in analysis.get("decision_system", ""),
            "the placeholder in the sent content was replaced by the real catalog",
        )
        for name in load_raw().get("models", {}):
            if not analysis.get("decision_system", "").count(f"- {name}:"):
                check(False, f"the catalog is missing model {name}")
                break
        else:
            check(True, "every configured model appears in the catalog")
        print(f"       routed={r.headers['x-routed-model']} "
              f"reason={r.headers['x-router-reason']} "
              f"rationale={analysis.get('rationale')!r}")
finally:
    restore = dict(ORIGINAL_AI)
    if not HAD_PROMPT:
        restore.pop("decision_prompt", None)
    resp = client.put("/v1/config", json={"ai_router": restore})
    after = dict(load_raw().get("ai_router") or {})
    print(
        f"\nai_router restored (HTTP {resp.status_code}): "
        f"decision_prompt {'present' if 'decision_prompt' in after else 'absent'}, "
        f"identical to the pre-run state={after == ORIGINAL_AI}"
    )

print(f"\n{ok} passed, {fail} failed")
raise SystemExit(1 if fail else 0)
