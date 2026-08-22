"""What one API key may reach, inside what its owner may reach.

The model policy (app/modelpolicy.py) answers "which models may this person use". This module
answers a narrower question: "which of those may this particular key use". The two compose in
one direction only -- a key scope can subtract, never add:

    effective = policy(owner)  intersected with  scope(key)

That direction is the whole point. A user hands a key to a CI job, an IDE, or a colleague's
tool, and wants that key limited to a Claude model or to two cheap deployments without
involving an administrator. Letting a scope *widen* would turn a self-service field into a
privilege escalation, so intersection is enforced here rather than trusted to the caller.

Three kinds, because those are the three ways a user actually thinks about the limit:

    {"kind": "all"}                                  everything the owner may use (default)
    {"kind": "api_types", "api_types": ["anthropic"]}  every model on connections of that type
    {"kind": "models", "models": ["gpt-4o", ...]}      an explicit pick from their own list

"api_types" is stored as the *type*, not as the list of models it currently resolves to, so a
model added to an Anthropic connection next week is covered by an existing key without anyone
editing it. That is the difference between a rule and a snapshot, and a key that silently
stops covering new models would be a support ticket nobody could explain.
"""
from __future__ import annotations

from .config import _API_TYPES

KINDS = ("all", "api_types", "models")
# "Everything the owner may reach", i.e. no narrowing at all. Named because callers outside this
# module compare against it to decide whether a scope needs permission (see app/scopepolicy.py).
KIND_ALL = "all"
DEFAULT_SCOPE = {"kind": KIND_ALL}


def normalize(raw, cfg=None) -> dict:
    """Validate an incoming scope and return it in canonical form.

    Raises ValueError with a message meant for the caller. Unknown model names are rejected
    rather than dropped: a typo that silently narrows a key to nothing would present as "my
    key stopped working" with no visible cause.
    """
    if raw in (None, "", {}):
        return dict(DEFAULT_SCOPE)
    if not isinstance(raw, dict):
        raise ValueError("scope must be an object")
    kind = raw.get("kind") or "all"
    if kind not in KINDS:
        raise ValueError("scope.kind must be one of " + ", ".join(KINDS))
    if kind == "all":
        return {"kind": "all"}
    if kind == "api_types":
        types = [str(t) for t in (raw.get("api_types") or [])]
        unknown = [t for t in types if t not in _API_TYPES]
        if unknown:
            raise ValueError("unknown connection type: " + ", ".join(unknown))
        if not types:
            raise ValueError("select at least one connection type")
        # Deduplicated in the catalog's own order, so two keys built from the same selection
        # compare equal regardless of the order the checkboxes were ticked in.
        return {"kind": "api_types", "api_types": [t for t in _API_TYPES if t in types]}
    models = [str(m) for m in (raw.get("models") or [])]
    if not models:
        raise ValueError("select at least one model")
    if cfg is not None:
        unknown = [m for m in models if m not in cfg.models]
        if unknown:
            raise ValueError("unknown model: " + ", ".join(unknown))
        models = [m for m in cfg.models if m in set(models)]
    return {"kind": "models", "models": models}


def models_for_api_types(cfg, api_types) -> list[str]:
    """Every catalog model whose connection speaks one of `api_types`, in catalog order."""
    wanted = set(api_types or [])
    out = []
    for name, meta in cfg.models.items():
        provider = cfg.get_provider((meta or {}).get("provider"))
        if provider is not None and provider.api_type in wanted:
            out.append(name)
    return out


def narrow(cfg, allowed: list[str] | None, scope: dict | None) -> list[str] | None:
    """Intersect the owner's allowed set with this key's scope.

    `allowed` of None means "unrestricted" (the model policy's own convention) and is returned
    unchanged for an unscoped key, so the common case adds no work and no behaviour change. A
    scoped key always returns a list, and an empty list is a real answer: the scope may name
    models the owner is no longer permitted, and the caller reports that as a 403 rather than
    silently falling back to the whole catalog.
    """
    scope = scope or DEFAULT_SCOPE
    kind = scope.get("kind") or "all"
    if kind == "all":
        return allowed
    if kind == "api_types":
        picked = models_for_api_types(cfg, scope.get("api_types"))
    else:
        picked = [m for m in cfg.models if m in set(scope.get("models") or [])]
    if allowed is None:
        return picked
    permitted = set(allowed)
    return [m for m in picked if m in permitted]


def describe(scope: dict | None) -> str:
    """A one-line form for the router log and the trace record."""
    scope = scope or DEFAULT_SCOPE
    kind = scope.get("kind") or "all"
    if kind == "api_types":
        return "api_types:" + "+".join(scope.get("api_types") or [])
    if kind == "models":
        return "models:" + "+".join(scope.get("models") or [])
    return "all"
