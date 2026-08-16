"""Configuration loading: config.yaml is the single source of truth (credentials
included); .env only supplies backward-compatible defaults.

Read and written through ruamel.yaml so comments survive: after the frontend edits
the configuration, config.yaml is still pleasant to edit by hand.
config.yaml holds credentials such as api_key, so it is gitignored;
config.example.yaml is the committed template.
"""
import os
from pathlib import Path

from dotenv import load_dotenv
from ruamel.yaml import YAML

ROOT = Path(__file__).resolve().parent.parent
load_dotenv(ROOT / ".env")

# Backward compatibility: older deployments keep credentials in .env. When
# config.yaml has no providers, these synthesize the default provider.
ENV_ENDPOINT = os.environ.get("AZURE_OPENAI_ENDPOINT", "")
ENV_API_KEY = os.environ.get("AZURE_OPENAI_API_KEY", "")
ENV_API_VERSION = os.environ.get("AZURE_OPENAI_API_VERSION", "2024-12-01-preview")

DATA_DIR = ROOT / "data"

_CONFIG_PATH = ROOT / "config.yaml"
_yaml = YAML()
_yaml.preserve_quotes = True
_yaml.width = 120

DEFAULT_PROVIDER_NAME = "foundry"
_API_TYPES = ("azure", "openai")

# Placeholder standing for the "model catalog" inside the AI decision prompt.
# Rendering does a **literal replacement** rather than str.format: a custom prompt
# almost always contains JSON braces, which format would treat as placeholders and
# then raise on.
CATALOG_PLACEHOLDER = "{catalog}"

DEFAULT_DECISION_PROMPT = """You are a model router. Given a user prompt, pick the single best backend model.

Available models:
{catalog}

Respond with ONLY a JSON object: {"model": "<model-name>", "rationale": "<one short sentence explaining why>"}.
The model name must be exactly one of the listed names."""


class Provider:
    """One OpenAI-compatible backend connection (Foundry / Azure OpenAI / any
    OpenAI-compatible service)."""

    def __init__(self, name: str, raw: dict):
        self.name = name
        self.base_url: str = (raw.get("base_url") or "").strip()
        self.api_key: str = (raw.get("api_key") or "").strip()
        self.api_type: str = raw.get("api_type") or "azure"
        self.api_version: str = raw.get("api_version") or ENV_API_VERSION

    @property
    def cache_key(self) -> tuple:
        return (self.base_url, self.api_key, self.api_type, self.api_version)

    def public_dict(self) -> dict:
        """Representation without the plaintext key, for trace records."""
        return {"name": self.name, "base_url": self.base_url, "api_type": self.api_type}


class ResolvedModel:
    """The routed model plus its provider and its real upstream model name."""

    def __init__(self, name: str, meta: dict, provider: Provider):
        self.name = name
        self.meta = meta
        self.provider = provider
        # The upstream deployment name may differ from the name this router exposes
        # (e.g. openrouter's anthropic/claude-opus-5).
        self.upstream_model: str = meta.get("model_name") or name
        self.reasoning: bool = bool(meta.get("reasoning"))
        self.api: str = "responses" if meta.get("api") == "responses" else "chat"


class RouterConfig:
    def __init__(self, raw: dict):
        self.raw = raw
        self.strategy: str = raw.get("strategy", "rule")
        self.models: dict[str, dict] = raw.get("models", {})
        self.rules: list[dict] = raw.get("rules", [])
        session = raw.get("session", {})
        self.sticky: bool = session.get("sticky", True)
        self.session_ttl: int = session.get("ttl_seconds", 1800)
        self.max_sessions: int = session.get("max_sessions", 10000)
        ai = raw.get("ai_router", {})
        self.decision_model: str = ai.get("decision_model", "gpt-4.1")
        self.decision_provider_name: str | None = ai.get("decision_provider")
        self.decision_timeout: float = ai.get("timeout_seconds", 5)
        self.max_prompt_chars: int = ai.get("max_prompt_chars", 4000)
        # Editable in the UI. Empty or missing falls back to the built-in default,
        # keeping behaviour identical to older versions.
        self.decision_prompt: str = (
            str(ai.get("decision_prompt") or "").strip() or DEFAULT_DECISION_PROMPT
        )

        self.providers: dict[str, Provider] = {
            name: Provider(name, meta or {})
            for name, meta in (raw.get("providers") or {}).items()
        }
        if not self.providers and ENV_ENDPOINT:
            # Compatibility with old deployments that only have .env
            self.providers[DEFAULT_PROVIDER_NAME] = Provider(
                DEFAULT_PROVIDER_NAME,
                {
                    "base_url": ENV_ENDPOINT,
                    "api_key": ENV_API_KEY,
                    "api_type": "azure",
                    "api_version": ENV_API_VERSION,
                },
            )
        self.default_provider_name: str = raw.get("default_provider") or next(
            iter(self.providers), DEFAULT_PROVIDER_NAME
        )

        auth = raw.get("auth") or {}
        gh = auth.get("github") or {}
        self.gh_client_id: str = (gh.get("client_id") or "").strip()
        self.gh_client_secret: str = (gh.get("client_secret") or "").strip()
        self.gh_callback_url: str = (gh.get("callback_url") or "").strip()
        self.admin_logins: list[str] = [
            str(x).strip().lower() for x in (auth.get("admin_logins") or []) if str(x).strip()
        ]
        self.allow_any_github_user: bool = auth.get("allow_any_github_user", True)
        self.auth_session_ttl: int = auth.get("session_ttl_seconds", 7 * 24 * 3600)
        # Enterprise policy for "who may create API keys"; evaluated in app/keypolicy.py.
        # Defaults to an empty dict, i.e. disabled, so upgrading an existing
        # deployment does not change behaviour.
        self.key_policy: dict = dict(auth.get("key_policy") or {})
        # A username/password super administrator, so the console stays reachable where
        # GitHub is not. Enabled by default: without it a deployment that cannot reach
        # github.com has no way in at all. See app/localadmin.py for the credential rules.
        self.local_admin: dict = dict(auth.get("local_admin") or {})

    @property
    def oauth_configured(self) -> bool:
        return bool(self.gh_client_id and self.gh_client_secret)

    @property
    def local_admin_enabled(self) -> bool:
        return bool(self.local_admin.get("enabled", True))

    @property
    def local_admin_username(self) -> str:
        from .localadmin import DEFAULT_USERNAME  # local import: localadmin imports nothing

        return str(self.local_admin.get("username") or DEFAULT_USERNAME).strip()

    def is_local_admin_login(self, login: str) -> bool:
        """Whether `login` is *the* local administrator right now.

        Recomputed per request like is_admin_login, so disabling or renaming the account
        in config.yaml downgrades sessions that were already issued to it.
        """
        if not self.local_admin_enabled:
            return False
        return (login or "").strip().lower() == self.local_admin_username.lower()

    @property
    def gh_admin_token(self) -> str:
        return str(self.key_policy.get("github_token") or "").strip()

    def is_admin_login(self, login: str) -> bool:
        return (login or "").lower() in self.admin_logins

    @property
    def default_model(self) -> str:
        for name, meta in self.models.items():
            if meta.get("default"):
                return name
        return next(iter(self.models))

    def get_provider(self, name: str | None) -> Provider:
        """Look a provider up by name; fall back to the default when missing."""
        if name and name in self.providers:
            return self.providers[name]
        default = self.providers.get(self.default_provider_name)
        if default is not None:
            return default
        if self.providers:
            return next(iter(self.providers.values()))
        # Nothing configured at all: return an empty provider so the eventual call
        # fails with a clearer message than a KeyError.
        return Provider(DEFAULT_PROVIDER_NAME, {})

    def resolve_model(self, name: str) -> ResolvedModel:
        meta = self.models.get(name) or {}
        return ResolvedModel(name, meta, self.get_provider(meta.get("provider")))

    def model_catalog_text(self) -> str:
        """The catalog fed to the decision model: one `- name: description` per line."""
        return "\n".join(
            f"- {name}: {(meta or {}).get('description', '').strip()}"
            for name, meta in self.models.items()
        )

    def render_decision_prompt(self, catalog: str | None = None) -> str:
        """Fill the model catalog into the prompt template, yielding the exact system
        content sent to the decision model.

        Literal replacement rather than `str.format`: user-written prompts usually
        contain JSON braces, and format would treat `{"model": ...}` as a placeholder
        and raise KeyError. When the placeholder is missing the catalog is appended so
        the decision model can at least see the candidates.
        """
        if catalog is None:
            catalog = self.model_catalog_text()
        template = self.decision_prompt
        if CATALOG_PLACEHOLDER in template:
            return template.replace(CATALOG_PLACEHOLDER, catalog)
        return f"{template}\n\nAvailable models:\n{catalog}"

    def resolve_decision_model(self) -> ResolvedModel:
        """The decision model: prefer its metadata from `models`, otherwise treat it as
        a bare deployment name on the provider."""
        meta = dict(self.models.get(self.decision_model) or {})
        provider_name = self.decision_provider_name or meta.get("provider")
        return ResolvedModel(
            self.decision_model, meta, self.get_provider(provider_name)
        )


def load_raw() -> dict:
    with open(_CONFIG_PATH, encoding="utf-8") as f:
        return _yaml.load(f)


def load_config() -> RouterConfig:
    return RouterConfig(load_raw())


def save_raw(updates: dict) -> RouterConfig:
    """Merge the submitted configuration back into config.yaml (top-level keys are
    **replaced wholesale**; file comments are preserved).

    Merging happens at the top level only: `{"auth": {...}}` replaces the entire auth
    section instead of merging field by field. That is deliberate -- deleting entries
    from `models` / `providers` / `rules` depends on this semantic. Switch it to a
    recursive merge and models the user deleted would come back on the next save.

    Callers must therefore submit the **complete** top-level section: to change only
    auth.key_policy, read the existing auth first and write the whole section back,
    or the github credentials and admin_logins in that same section get wiped.
    """
    doc = load_raw()
    for key, value in updates.items():
        doc[key] = value
    with open(_CONFIG_PATH, "w", encoding="utf-8") as f:
        _yaml.dump(doc, f)
    return RouterConfig(doc)


def validate_raw(raw: dict) -> list[str]:
    """Return the list of configuration errors; an empty list means valid."""
    errors = []
    if raw.get("strategy") not in ("rule", "ai", None):
        errors.append("strategy must be 'rule' or 'ai'")

    providers = raw.get("providers")
    if providers is not None:
        if not isinstance(providers, dict) or not providers:
            errors.append("providers must not be empty")
        else:
            for name, meta in providers.items():
                meta = meta or {}
                if not (meta.get("base_url") or "").strip():
                    errors.append(f"provider {name!r} is missing base_url")
                api_type = meta.get("api_type") or "azure"
                if api_type not in _API_TYPES:
                    errors.append(
                        f"provider {name!r}: api_type must be 'azure' or 'openai'"
                    )
            default_provider = raw.get("default_provider")
            if default_provider and default_provider not in providers:
                errors.append(f"default_provider {default_provider!r} is not in providers")

    if "models" in raw:
        models = raw["models"]
        if not isinstance(models, dict) or not models:
            errors.append("models must not be empty")
        else:
            known_providers = set(providers or {})
            for name, meta in models.items():
                ref = (meta or {}).get("provider")
                if ref and known_providers and ref not in known_providers:
                    errors.append(f"model {name!r} references unknown provider {ref!r}")
            for rule in raw.get("rules", []) or []:
                if rule.get("model") not in models:
                    errors.append(
                        f"rule {rule.get('name', '?')} references unknown model {rule.get('model')!r}"
                    )

    session = raw.get("session") or {}
    if not isinstance(session.get("sticky", True), bool):
        errors.append("session.sticky must be a boolean")

    errors.extend(_validate_ai_router(raw.get("ai_router"), providers))

    auth = raw.get("auth")
    if auth is not None:
        if not isinstance(auth, dict):
            errors.append("auth must be an object")
        else:
            admins = auth.get("admin_logins")
            if admins is not None and not isinstance(admins, list):
                errors.append("auth.admin_logins must be a list")
            errors.extend(_validate_key_policy(auth.get("key_policy")))
            errors.extend(_validate_local_admin(auth.get("local_admin")))
    return errors


def _validate_local_admin(la) -> list[str]:
    """Validate auth.local_admin.

    The password hash fields are deliberately *not* validated as a required pair: an
    operator who blanks them is asking for the default password back, which is the
    documented way to recover from a lost local-admin password.
    """
    if la is None:
        return []
    if not isinstance(la, dict):
        return ["auth.local_admin must be an object"]

    errors: list[str] = []
    if not isinstance(la.get("enabled", True), bool):
        errors.append("auth.local_admin.enabled must be a boolean")
    username = la.get("username")
    if username is not None and not str(username).strip():
        errors.append("auth.local_admin.username must not be empty")
    return errors


def _validate_ai_router(ai, providers) -> list[str]:
    """Validate ai_router. The prompt only gets soft checks such as "not empty" --
    prompt quality cannot be judged mechanically, and a missing {catalog} placeholder
    is not an error (rendering appends the catalog). Still worth warning the admin
    about, otherwise "I changed the model catalog and the prompt did not budge" is
    painful to diagnose."""
    if ai is None:
        return []
    if not isinstance(ai, dict):
        return ["ai_router must be an object"]

    errors: list[str] = []
    if not str(ai.get("decision_model", "gpt-4.1") or "").strip():
        errors.append("ai_router.decision_model must not be empty")
    ref = ai.get("decision_provider")
    if ref and isinstance(providers, dict) and providers and ref not in providers:
        errors.append(f"ai_router.decision_provider {ref!r} is not in providers")
    for field, low in (("timeout_seconds", 0), ("max_prompt_chars", 1)):
        value = ai.get(field)
        if value is None:
            continue
        if not isinstance(value, (int, float)) or isinstance(value, bool) or value <= low:
            errors.append(f"ai_router.{field} must be a number greater than {low}")

    prompt = ai.get("decision_prompt")
    if prompt is not None:
        if not isinstance(prompt, str):
            errors.append("ai_router.decision_prompt must be a string")
        elif prompt.strip() and len(prompt.strip()) < 20:
            errors.append(
                "ai_router.decision_prompt is too short for the decision model to "
                "reliably emit JSON (leave it empty to use the built-in default)"
            )
    return errors


def _validate_key_policy(policy) -> list[str]:
    """Validate auth.key_policy. Enabled-but-tokenless is a legal yet dangerous
    configuration -- keypolicy.evaluate then denies every non-admin -- so this only
    warns instead of erroring, to avoid trapping the admin in a "cannot save the
    toggle before filling in the token" deadlock."""
    if policy is None:
        return []
    if not isinstance(policy, dict):
        return ["auth.key_policy must be an object"]

    errors: list[str] = []
    if not isinstance(policy.get("enabled", False), bool):
        errors.append("auth.key_policy.enabled must be a boolean")

    enterprises = policy.get("enterprises")
    if enterprises is None:
        return errors
    if not isinstance(enterprises, dict):
        return errors + [
            "auth.key_policy.enterprises must be an object keyed by enterprise slug"
        ]

    for slug, rule in enterprises.items():
        if rule is None:
            continue
        if not isinstance(rule, dict):
            errors.append(f"auth.key_policy.enterprises[{slug!r}] must be an object")
            continue
        for flag in ("enabled", "allow_all_orgs"):
            if not isinstance(rule.get(flag, False), bool):
                errors.append(
                    f"auth.key_policy.enterprises[{slug!r}].{flag} must be a boolean"
                )
        for field in ("organizations", "teams"):
            value = rule.get(field)
            if value is not None and not isinstance(value, list):
                errors.append(
                    f"auth.key_policy.enterprises[{slug!r}].{field} must be a list"
                )
    return errors
