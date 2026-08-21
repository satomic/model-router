"""Configuration loading: config.yaml is the single source of truth (credentials
included); .env only supplies backward-compatible defaults.

Read and written through ruamel.yaml so comments survive: after the frontend edits
the configuration, config.yaml is still pleasant to edit by hand.
config.yaml holds credentials such as api_key, so it is gitignored;
config.example.yaml is the committed template.

Everything mutable lives under ONE directory, data/, so that persistent state is a single
thing to back up, move or mount:

  data/config.yaml           the configuration, written back to by the console
  data/logs/traces/          full-chain trace records
  data/auth_sessions.json    sign-in sessions
  data/api_keys.json         API keys
  data/github/               the GitHub structure / member cache

That one directory is what a container mounts as a volume -- an image layer is discarded on
every upgrade, so nothing writable may live inside the image. Each path is still individually
overridable, for a deployment that wants traces on a bigger disk than the configuration:

  MR_DATA_DIR       the root of all persistent state      (default <root>/data)
  MR_CONFIG_FILE    the config.yaml to read and write     (default <data>/config.yaml)
  MR_LOG_DIR        full-chain trace records              (default <data>/logs)
"""
import os
import shutil
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


def _path_from_env(var: str, default: Path) -> Path:
    """Resolve a path override, falling back to the repository-root default."""
    raw = os.environ.get(var, "").strip()
    return Path(raw).expanduser().resolve() if raw else default


# DATA_DIR first: the other two default to positions *inside* it, so overriding it alone
# relocates all persistent state together -- which is what makes a container need exactly one
# mount point and one variable.
DATA_DIR = _path_from_env("MR_DATA_DIR", ROOT / "data")
LOG_DIR = _path_from_env("MR_LOG_DIR", DATA_DIR / "logs")

# The committed template, which is also what a missing config.yaml is seeded from. It stays at
# the repository root: it ships with the code and is never written to, so it is not state.
TEMPLATE_PATH = ROOT / "config.example.yaml"
CONFIG_PATH = _path_from_env("MR_CONFIG_FILE", DATA_DIR / "config.yaml")
_CONFIG_PATH = CONFIG_PATH  # kept: the private name is used throughout this module
_yaml = YAML()
_yaml.preserve_quotes = True
_yaml.width = 120


# Where these two lived before all state was consolidated under data/. Kept so that an
# existing checkout keeps working across the change instead of looking unconfigured.
_LEGACY_CONFIG_PATH = ROOT / "config.yaml"
_LEGACY_LOG_DIR = ROOT / "logs"


def migrate_legacy_layout() -> list[str]:
    """Move a pre-existing root-level config.yaml / logs/ under data/. Returns what moved.

    This is the one genuinely dangerous part of consolidating the layout: config.yaml holds
    every credential, and without this an upgrade would find the new default path empty, seed
    it from the template, and present a working-looking install whose providers, OAuth app and
    admin list had all silently reverted. Losing the traces would be recoverable; that would
    not be.

    Only ever moves INTO an empty destination, and only when the destination is still at its
    default position under DATA_DIR -- an operator who pointed MR_CONFIG_FILE somewhere
    explicitly is not migrated on top of. A partially migrated tree therefore stays as it is
    and is reported rather than merged, because merging two trace trees or picking between two
    config files is a judgement call this function has no business making.
    """
    moved = []
    if (
        CONFIG_PATH == DATA_DIR / "config.yaml"
        and not CONFIG_PATH.exists()
        and _LEGACY_CONFIG_PATH.is_file()
    ):
        CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(_LEGACY_CONFIG_PATH), str(CONFIG_PATH))
        moved.append(f"{_LEGACY_CONFIG_PATH} -> {CONFIG_PATH}")

    # The traces live one level down, under <log dir>/traces, so that is what moves: it keeps
    # the uvicorn logs an operator may have redirected into logs/ out of the migration.
    legacy_traces = _LEGACY_LOG_DIR / "traces"
    new_traces = LOG_DIR / "traces"
    if (
        LOG_DIR == DATA_DIR / "logs"
        and legacy_traces.is_dir()
        and not new_traces.exists()
    ):
        new_traces.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(legacy_traces), str(new_traces))
        moved.append(f"{legacy_traces} -> {new_traces}")
    return moved


def ensure_config_file() -> bool:
    """Create config.yaml from the template when it does not exist yet. Returns True when
    a file was created.

    Called before the first read so that a fresh deployment starts instead of dying on
    FileNotFoundError. It matters most in a container: the config lives on a mounted volume
    that starts out empty, and requiring the operator to place a file there before the very
    first `docker run` would make the image unusable without a checkout of this repository.

    The template carries placeholders only -- no credentials -- so a seeded file grants no
    access by itself. The local administrator account is what makes it reachable: it is
    enabled by default and forces a password change before anything else can be used.

    shutil.copyfile rather than a YAML round-trip: the template's comments explain every
    field, and they are the seeded file's documentation. copyfile keeps them byte for byte.
    """
    if _CONFIG_PATH.exists():
        return False
    if not TEMPLATE_PATH.exists():
        raise FileNotFoundError(
            f"neither {_CONFIG_PATH} nor the template {TEMPLATE_PATH} exists; "
            "cannot start without a configuration"
        )
    _CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(TEMPLATE_PATH, _CONFIG_PATH)
    return True

DEFAULT_PROVIDER_NAME = "foundry"
# The wire protocol a connection speaks. "azure" and "openai" are both the OpenAI chat
# completions protocol and differ only in how the URL and the key are assembled; "anthropic"
# is the Anthropic Messages protocol, which is a different request and response shape
# altogether -- see app/wire.py for the translation.
_API_TYPES = ("azure", "openai", "anthropic")
# Sent as the `anthropic-version` header. A connection may override it through api_version,
# which for an Anthropic connection means that header rather than Azure's ?api-version=.
ANTHROPIC_VERSION = "2023-06-01"

# The scopes a model policy can bind a model group to, in the order the resolver reports them.
# "user" is the most specific and "organization" the least, but the order carries no precedence:
# resolution is a union (see app/modelpolicy.py), so this is a display order only.
POLICY_SCOPES = ("user", "team", "organization")

# The routing strategies. "rule-then-ai" runs both: the rules decide when one of them matches,
# and only an unmatched request costs a decision call. Exported so the validator, the router and
# anything else that has to enumerate them read from one list.
STRATEGIES = ("rule", "ai", "rule-then-ai")

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
        # For an Anthropic connection this is the `anthropic-version` header, so it must not
        # inherit the Azure default: an Azure api-version string in that header is rejected
        # upstream.
        self.api_version: str = raw.get("api_version") or (
            ANTHROPIC_VERSION if self.api_type == "anthropic" else ENV_API_VERSION
        )

    @property
    def cache_key(self) -> tuple:
        return (self.base_url, self.api_key, self.api_type, self.api_version)

    @property
    def protocol(self) -> str:
        """Which wire protocol this connection speaks: "anthropic" or "openai".

        Everything above this line treats azure and openai as one protocol, because they are:
        the difference is confined to how providers.py builds the client. The protocol is what
        decides whether a request needs translating, so it is asked for by name rather than
        re-derived from api_type at each call site.
        """
        return "anthropic" if self.api_type == "anthropic" else "openai"

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
        # One scalar rather than two toggles, so a config can never describe a state the
        # router has no branch for. "rule-then-ai" is the both-at-once value; the two
        # single-strategy values keep their old meaning, so existing files are unaffected.
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

        # Named model groups: {group name: [model name, ...]}. A group may legally be empty --
        # "this scope contributes nothing" is a configuration an operator asks for on purpose
        # (see app/modelpolicy.py for what that means once the scopes are unioned).
        self.model_groups: dict[str, list[str]] = {
            str(name): [str(m) for m in (members or [])]
            for name, members in (raw.get("model_groups") or {}).items()
        }
        # Which group each scope gets. Defaults to an empty dict, i.e. disabled, so upgrading
        # an existing deployment does not suddenly restrict anybody.
        self.model_policy: dict = dict(raw.get("model_policy") or {})

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

    # -- Model policy ---------------------------------------------------------
    @property
    def model_policy_enabled(self) -> bool:
        """Whether the model policy is enforced at all.

        Defaults to False so that adding the section to config.yaml without turning it on
        changes nothing: an operator can build up groups and bindings first, then enable.
        """
        return bool(self.model_policy.get("enabled", False))

    @property
    def default_group(self) -> str:
        """The group every signed-in user starts with, before any scope binding applies.

        Empty string means "no default", which under union semantics leaves an unbound user
        with nothing of their own -- see app/modelpolicy.py for what happens then.
        """
        return str(self.model_policy.get("default_group") or "").strip()

    def restricted_to(self, names) -> "RouterConfig":
        """A view of this configuration whose model catalog is narrowed to `names`.

        This is how the model policy is enforced, and it is deliberately a *narrowing of the
        catalog* rather than a check bolted onto each routing strategy. Everything downstream
        already treats "not in the models catalog" as a first-class case: match_rules skips such
        a rule with a recorded reason, route_by_ai only ever offers `cfg.models` as candidates
        and falls back when the answer is not one of them, and `default_model` reads from the
        same dict. Narrowing therefore makes a disallowed model unreachable through every path at
        once, with no new failure mode to test and no way for a future strategy to miss the check.

        The view shares `raw`, `providers` and everything else by reference: it is read-only and
        per-request, so copying the provider objects would only cost the connection pool its
        cache keys. Order follows the full catalog, so a narrowed decision prompt lists models in
        the same order the Models page does.
        """
        allowed = set(names)
        view = object.__new__(RouterConfig)
        view.__dict__.update(self.__dict__)
        view.models = {
            name: meta for name, meta in self.models.items() if name in allowed
        }
        return view

    def group_models(self, group: str) -> list[str]:
        """The models in `group`, filtered to what the catalog still has.

        Filtering here rather than at save time keeps a group honest after a model is deleted
        straight out of config.yaml by hand: a group naming a model that no longer exists must
        not make that name routable.
        """
        return [m for m in self.model_groups.get(group, []) if m in self.models]

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
    # Seeding here rather than at import time covers every entry point -- the app, the verify
    # scripts, and any future CLI -- instead of only whichever one remembered to call it.
    ensure_config_file()
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
    if raw.get("strategy") not in STRATEGIES + (None,):
        errors.append("strategy must be one of %s" % ", ".join(repr(s) for s in STRATEGIES))

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
                        f"provider {name!r}: api_type must be one of "
                        + ", ".join(repr(t) for t in _API_TYPES)
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
    errors.extend(_validate_model_groups(raw.get("model_groups"), raw.get("models")))
    errors.extend(
        _validate_model_policy(raw.get("model_policy"), raw.get("model_groups"))
    )

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


def _validate_model_groups(groups, models) -> list[str]:
    """Validate model_groups: {name: [model, ...]}.

    An **empty list is legal and meaningful** -- it is how an operator says "this group grants
    nothing", which the requirement asks for explicitly (a freshly signed-in user can be given
    an empty group). So emptiness is never an error here.

    A member that is not in the models catalog *is* an error, because it can only be a typo or a
    stale reference: the group would silently grant less than it appears to. Deleting a model in
    the console prunes it from every group (see the frontend's removeModel), so a save arriving
    from the UI cannot produce this.
    """
    if groups is None:
        return []
    if not isinstance(groups, dict):
        return ["model_groups must be an object keyed by group name"]

    errors: list[str] = []
    known = set(models or {})
    for name, members in groups.items():
        if not str(name).strip():
            errors.append("model_groups has an entry with an empty name")
        if members is None:
            continue  # an omitted list reads the same as [], i.e. an empty group
        if not isinstance(members, list):
            errors.append(f"model_groups[{name!r}] must be a list of model names")
            continue
        for member in members:
            if known and member not in known:
                errors.append(
                    f"model_groups[{name!r}] references unknown model {member!r}"
                )
    return errors


def _validate_model_policy(policy, groups) -> list[str]:
    """Validate model_policy: which model group each scope gets.

      model_policy:
        enabled: false
        default_group: starter        # every signed-in user, before any binding
        users:         {login: group}
        teams:         {team slug or id: group}
        organizations: {org login: group}

    Like _validate_key_policy this leans towards warning-free tolerance: an enabled policy with
    no bindings at all is legal (it just means everyone gets the default group), so that an
    admin can turn the toggle on before filling the tables in rather than being deadlocked by
    the validator. A binding naming a group that does not exist is an error, though -- it grants
    nothing while looking like it grants something, which is the one failure mode an operator
    cannot see from the page.
    """
    if policy is None:
        return []
    if not isinstance(policy, dict):
        return ["model_policy must be an object"]

    errors: list[str] = []
    if not isinstance(policy.get("enabled", False), bool):
        errors.append("model_policy.enabled must be a boolean")

    known_groups = set(groups or {})

    default_group = policy.get("default_group")
    if default_group not in (None, "") and known_groups and default_group not in known_groups:
        errors.append(f"model_policy.default_group {default_group!r} is not a known model group")

    for field in ("users", "teams", "organizations"):
        table = policy.get(field)
        if table is None:
            continue
        if not isinstance(table, dict):
            errors.append(f"model_policy.{field} must be an object keyed by name")
            continue
        for key, group in table.items():
            if group in (None, ""):
                continue  # an explicit blank is "no binding", not a broken one
            if known_groups and group not in known_groups:
                errors.append(
                    f"model_policy.{field}[{key!r}] references unknown model group {group!r}"
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
