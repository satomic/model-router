# Foundry Model Router

An OpenAI-compatible model router: it accepts `/v1/chat/completions` requests and routes them to a
suitable backend model, either by rules or by an AI decision (gpt-4.1). The backend is not limited to
Azure AI Foundry — any OpenAI-compatible address and key can be configured, and each model can be
bound to a different connection. It ships with an Azure-portal-styled React console: GitHub sign-in,
API key management, usage statistics, full-chain traces, and configuration management.

## Quick start

```powershell
.venv\Scripts\Activate.ps1
pip install -r requirements.txt
copy config.example.yaml config.yaml     # first run: generate the local config from the template
uvicorn app.main:app --host 0.0.0.0 --port 8000
```

Open http://localhost:8000/ . The first visit lands on the setup wizard; enter a GitHub OAuth Client
ID / Secret and the administrator logins, sign in, then create an API key on the "API keys" page and
point your client at it:

| Field | Value |
|---|---|
| Base URL | `http://localhost:8000/v1` |
| API Key | `fmr_...` |
| Model | any model name registered under "Routing configuration" |

Without access to github.com, sign in with the local super administrator instead
(`admin` / `admin1234`, which forces a password change before anything else is reachable).

## What it does

- **Routes by rules or by an AI decision model**, then adapts parameters per model (reasoning models,
  the Responses API) before calling the backend.
- **One user interaction is one routing decision and one trace.** An agentic client such as GitHub
  Copilot answers a single question with a loop of HTTP requests; an `x-interaction-id` holds the
  model constant across that loop and folds every turn into a single trace record, instead of
  re-routing the same prompt N times.
- **Attributes every call to a real user.** Copilot BYOK passes no identity, so `user_id` comes from
  the owner of the API key and cannot be forged by the client.
- **Gates who may create a key** on GitHub Enterprise / organization / Enterprise Team membership,
  answered from a local cache where it can be trusted.
- **Records the full chain** — request, routing decision, backend call, response, and per-turn tool
  calls — readable in the console as a collapsible JSON tree.

## Documentation

| Document | Contents |
|---|---|
| [Getting started](docs/getting-started.md) | installation, running behind a proxy, frontend development, console languages |
| [Sign-in and authentication](docs/authentication.md) | the GitHub OAuth App, API keys, the local super administrator, the permission matrix |
| [Backend connections](docs/providers.md) | providers, per-model bindings, non-Foundry OpenAI-compatible endpoints |
| [Router logic](docs/router-logic.md) | the request flow, interaction stickiness, rule and AI routing, the editable decision prompt |
| [Configuration](docs/configuration.md) | `config.yaml`, the console's configuration pages, hot reload |
| [Access control](docs/access-control.md) | the key-creation policy and the local GitHub structure/member cache |
| [API](docs/api.md) | every endpoint |
| [Full-chain logging](docs/traces.md) | the trace format, turns, and how the listing stays cheap at scale |
| [Verification scripts](docs/verification.md) | the `verify/` suite and the frontend gates |

## Layout

```
app/         FastAPI backend: routing, providers, auth, key policy, traces
frontend/    React + Vite console (built output is served by FastAPI from /)
docs/        the documents listed above
verify/      end-to-end verification scripts
config.yaml  all configuration, including credentials -- gitignored
```

Credentials never enter the repository: `config.yaml`, `.env`, `data/` and `logs/` are gitignored,
and [config.example.yaml](config.example.yaml) is the committed template with placeholders only.
