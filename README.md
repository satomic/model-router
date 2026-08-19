# Model Router

An OpenAI-compatible model router: it accepts `/v1/chat/completions` requests and routes them to a
suitable backend model, either by rules or by an AI decision (gpt-4.1). The backend is not limited to
Azure AI Foundry — any OpenAI-compatible address and key can be configured, and each model can be
bound to a different connection. It ships with an Azure-portal-styled React console: GitHub sign-in,
API key management, usage statistics, full-chain traces, and configuration management.

## Quick start (Docker)

```bash
docker run -d --name model-router \
  -p 8000:8000 -v mr-data:/data --restart unless-stopped \
  ghcr.io/satomic/model-router:latest
```

Nothing to prepare: the configuration is created from the template on first start, and the single
`/data` volume holds all of it — the configuration, the sign-in state, the keys and the traces — so
an upgrade is just a new image over the same volume.

Open http://localhost:8000/ and sign in as the local super administrator — `admin` / `admin1234`,
which forces a password change first. Configure a backend connection from the console, create an API
key on the "API keys" page, and point your client at it:

| Field | Value |
|---|---|
| Base URL | `http://localhost:8000/v1` |
| API Key | `mr_...` |
| Model | any model name registered under "Routing configuration" |

Volumes, port mapping, upgrades and reverse proxies: [Docker deployment](docs/docker.md).

## Running from source

```powershell
.venv\Scripts\Activate.ps1
pip install -r requirements.txt
cd frontend; npm ci; npm run build; cd ..   # FastAPI serves the built console from /
uvicorn app.main:app --host 0.0.0.0 --port 8000
```

`data/config.yaml` is created from `config.example.yaml` on first start here too, and `data/` is the
same single directory the container mounts. Running on your own machine, the first visit can also
use the **setup wizard** to enter a GitHub OAuth Client ID / Secret — it is offered only to requests
from `127.0.0.1`, which is why a container uses the local administrator instead.

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
- **Curates the model list per user, team and organization.** Named model groups are granted per
  scope and resolve as a union, and every user has a page showing exactly what they may call and
  which grant made it available.
- **Records the full chain** — request, routing decision, backend call, response, and per-turn tool
  calls — readable in the console as a collapsible JSON tree.

## Documentation

| Document | Contents |
|---|---|
| [Docker deployment](docs/docker.md) | **the recommended path** — the image, port mapping, the data volume, upgrades, reverse proxies |
| [Getting started](docs/getting-started.md) | running from source, frontend development, console languages |
| [Sign-in and authentication](docs/authentication.md) | the GitHub OAuth App, API keys, the local super administrator, the permission matrix |
| [Backend connections](docs/providers.md) | providers, per-model bindings, non-Foundry OpenAI-compatible endpoints |
| [Router logic](docs/router-logic.md) | the request flow, interaction stickiness, rule and AI routing, the editable decision prompt |
| [Configuration](docs/configuration.md) | `config.yaml`, the console's configuration pages, hot reload |
| [Access control](docs/access-control.md) | the key-creation policy and the local GitHub structure/member cache |
| [Model policy](docs/model-policy.md) | model groups, and which models each user / team / organization may use |
| [API](docs/api.md) | every endpoint |
| [Full-chain logging](docs/traces.md) | the trace format, turns, and how the listing stays cheap at scale |
| [Verification scripts](docs/verification.md) | the `verify/` suite and the frontend gates |

## Layout

```
app/         FastAPI backend: routing, providers, auth, key policy, traces
frontend/    React + Vite console (built output is served by FastAPI from /)
docs/        the documents listed above
verify/      end-to-end verification scripts
Dockerfile   multi-stage build: the console is built in a discarded Node stage
data/        ALL persistent state -- config.yaml, sessions, keys, traces -- gitignored
```

Credentials never enter the repository: the whole of `data/` (which is where `config.yaml` lives)
and `.env` are gitignored, and [config.example.yaml](config.example.yaml) is the committed template
with placeholders only.
