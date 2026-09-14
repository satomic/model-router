# Model Router

A model router (AI routing gateway): it accepts requests and routes them to a suitable backend model, either by rules or by an AI decision. It speaks both the OpenAI chat-completions protocol and the Anthropic Messages protocol, and converts between them, so either kind of client can reach either kind of model. The backend is not limited to Azure AI Foundry, and each model can be bound to a different connection. It ships with an Azure-portal-styled React console: GitHub sign-in, API key management, usage statistics, full-chain traces, and configuration management.

## Quick start (Docker)

```bash
docker run -d --name model-router \
  -p 8000:8000 -v mr-data:/data --restart unless-stopped \
  ghcr.io/satomic/model-router:latest
```

`:latest` is the Go backend — a 29 MB Alpine image with a shell in it, so a container can still
be opened with `docker exec -it model-router sh`. The Python backend is published from the
same release under `:latest-py` for anyone who wants the reference implementation; the two share
the `/data` volume, so switching is a change of tag. See [the two backends](docs/backends.md).

Nothing to prepare: the configuration is created from the template on first start, and the single
`/data` volume holds all of it (the configuration, the sign-in state, the keys and the traces), so
an upgrade is just a new image over the same volume.

Open http://localhost:8000/ and sign in as the local super administrator, `admin` / `admin1234`,
which forces a password change first. Configure a backend connection from the console, create an API
key on the "API keys" page, and point your client at it:

| Field | OpenAI-compatible client | Anthropic-compatible client |
|---|---|---|
| Base URL | `http://localhost:8000/v1` | `http://localhost:8000` |
| API Key | `mr_...` as `Authorization: Bearer` | `mr_...` as `x-api-key` |
| Model | any model name registered under "Routing configuration" | the same, or `auto` |

Volumes, port mapping, upgrades and reverse proxies: [Docker deployment](docs/docker.md).

## Two backends, one service

The router ships two interchangeable implementations: the original Python one (FastAPI) and a Go
one built for throughput. They share `data/`, `config.yaml`, the REST API, the trace format and the
console, so switching is stopping one and starting the other -- no migration.

```bash
./run.sh                      # Go (the default)
./run.sh --backend python     # Python
```

Go holds roughly twice the concurrent in-flight requests and adds 1% latency overhead where
Python adds 61%, in a third of the memory. It ships as a single static binary with the console
embedded inside it — a 29 MB image against the Python one's 319 MB. See
[the benchmark](docs/backend-benchmark.md) for the measurements, the method, and when Python is
still the better choice.

## Running from source

```bash
cd frontend && npm ci && npm run build && cd ..   # both backends serve the built console from /

# Go
server-go/scripts/build.sh && ./run.sh

# Python
pip install -r requirements.txt
./run.sh --backend python
```

`data/config.yaml` is created from `config.example.yaml` on first start here too, and `data/` is the
same single directory the container mounts. Running on your own machine, the first visit can also
use the **setup wizard** to enter a GitHub OAuth Client ID / Secret. It is offered only to requests
from `127.0.0.1`, which is why a container uses the local administrator instead.

## What it does

- **Routes by rules or by an AI decision model**, then adapts parameters per model (reasoning models,
  the Responses API) before calling the backend.
- **Speaks both protocols on the way in and on the way out.** `/v1/chat/completions` and
  `/v1/messages` are two doors onto the same router, and a connection can be Azure OpenAI,
  OpenAI-compatible or Anthropic-compatible. The four combinations all work, streaming included, so
  an Anthropic-style client can be answered by an Azure deployment and the reverse.
- **Scopes each API key independently.** One key can be limited to a set of models, or to every model
  of chosen interface types, always as an intersection with what its owner is allowed, and the scope
  can be narrowed later without reissuing the key.
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
- **Records the full chain**: request, routing decision, backend call, response, and per-turn tool
  calls, readable in the console as a collapsible JSON tree.
- **Visualizes policy relationships** through an administrator-only policy overview and focused
  relationship graphs, with semantic colors, progressive exploration and on-demand user access.
  See [Global policy topology](docs/access-control.md#global-policy-topology).
- **Keeps BYOK from wasting Copilot's own AI credits.** The enterprise's shared AI-credit pool is
  polled from GitHub on a cron schedule, and an optional gate answers requests with a note instead of
  routing them while the pool still has credits -- per enterprise, or per user against seats and
  user-level budgets. See [Copilot AI credits](docs/ai-credits.md).

## Documentation

| Document | Contents |
|---|---|
| [Operations guide](docs/user-guide.md) / [操作指南](docs/user-guide-cn.md)| **the console, screen by screen**: what an administrator configures, then what a standard user does |
| [Architecture and data flow](docs/architecture.md) | **start here**: diagrams of the components, the request path, and every routing strategy |
| [Docker deployment](docs/docker.md) | **the recommended path**: the image, port mapping, the data volume, upgrades, reverse proxies |
| [Getting started](docs/getting-started.md) | running from source, frontend development, console languages |
| [Sign-in and authentication](docs/authentication.md) | the GitHub OAuth App, API keys, the local super administrator, the permission matrix |
| [Backend connections](docs/providers.md) | providers, per-model bindings, non-Foundry OpenAI-compatible and Anthropic-compatible endpoints, protocol conversion |
| [Router logic](docs/router-logic.md) | the request flow, interaction stickiness, rule and AI routing, the editable decision prompt |
| [Configuration](docs/configuration.md) | `config.yaml`, the console's configuration pages, hot reload |
| [Access control](docs/access-control.md) | the key-creation policy and the local GitHub structure/member cache |
| [Model policy](docs/model-policy.md) | model groups, and which models each user / team / organization may use |
| [Copilot AI credits](docs/ai-credits.md) | polling the enterprise AI-credit pool, and the gate that holds BYOK back while credits remain |
| [API](docs/api.md) | every endpoint |
| [Full-chain logging](docs/traces.md) | the trace format, turns, and how the listing stays cheap at scale |
| [Verification scripts](docs/verification.md) | the `verify/` suite and the frontend gates |
| [The two backends](docs/backends.md) | Python and Go: what they share, how to choose one, the Go source layout |
| [Backend benchmark](docs/backend-benchmark.md) | measured throughput, latency, in-flight capacity and footprint of the two |

## Layout

```
app/           Python backend (FastAPI): routing, providers, auth, key policy, traces
server-go/     Go backend: the same service, same data/, same API -- see docs/backends.md
frontend/      React + Vite console (the built output is served by either backend from /)
docs/          the documents listed above
verify/        end-to-end verification scripts
run.sh         launcher: --backend go | python
Dockerfile     Python image (python:3.11-slim); Dockerfile.go builds the Go one (alpine)
data/          ALL persistent state -- config.yaml, sessions, keys, traces -- gitignored
```

Credentials never enter the repository: the whole of `data/` (which is where `config.yaml` lives)
and `.env` are gitignored, and [config.example.yaml](config.example.yaml) is the committed template
with placeholders only.
