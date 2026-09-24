# The two backends

Model Router ships two implementations of the same service:

| | Path | Runtime |
|---|---|---|
| Python | [`app/`](../app/) | FastAPI + uvicorn |
| Go | [`server-go/`](../server-go/) | one static binary, no dependencies |

They are **interchangeable at run time**. Same `data/` directory, same `config.yaml`, same REST
API, same trace format, same console. Switching backends is stopping one process and starting the
other — there is no migration step and no configuration change.

For the measured differences between them, see [backend-benchmark.md](backend-benchmark.md).
The short version: Go holds roughly twice the concurrent in-flight requests and adds 1 % latency
overhead where Python adds 61 %, in a third of the memory and one 9 MB file.

---

## Choosing at start-up

```bash
./run.sh                          # Go (the default)
./run.sh --backend python         # Python
MR_BACKEND=python ./run.sh        # the same, through the environment

./run.sh --port 9000              # either backend, a different port
./run.sh --backend python --reload  # uvicorn's auto-reload, for editing app/
./run.sh --build --frontend       # rebuild the console and the Go binary first
```

Both listen on `0.0.0.0:8000` by default and serve the console from `/`.

Neither backend needs `run.sh`. To start them directly:

```bash
./server-go/bin/model-router --host 0.0.0.0 --port 8000
python -m uvicorn app.main:app --host 0.0.0.0 --port 8000
```

## Containers

Only the Go image is published: `docker pull ghcr.io/<owner>/model-router:latest` is the Go
backend.

| Tag | Backend | Base | Size |
|---|---|---|---|
| `:latest`, `:2.0.0`, `:2.0`, `:2` | Go | `alpine:3.21` | **29 MB** |

```bash
docker run -p 8000:8000 -v mr-data:/data ghcr.io/<owner>/model-router:latest
```

New features land in the Go backend only, so the Python image is no longer built. The `-py` tags
published by earlier releases (`:latest-py`, `:2.0.0-py`, ...) are still in the registry and
still take the same `/data` volume, but they stop at the last release that produced them.
`Dockerfile` still builds the Python image locally for anyone who needs it.

To build locally (BuildKit is required — the Dockerfiles use `$BUILDPLATFORM` to cross-compile
rather than emulate; with the legacy builder the build fails on that variable):

```bash
docker buildx build -f Dockerfile.go -t model-router:go --load .
docker buildx build -f Dockerfile    -t model-router:py --load .
```

### What the Go image is

Alpine with one binary in it — the console embedded inside the binary — plus `ca-certificates`,
`tzdata` and `config.example.yaml`. No Python, no Node, no dependency tree.

**It ships a shell on purpose.** A `FROM scratch` image is about 8 MB smaller and has nothing at
all to patch, and the first version of this was one. But scratch has no `sh`, which means
`docker exec -it <container> sh` fails and there is no way to look inside a container that is
misbehaving — no `ps`, no `netstat`, no reading the file it just wrote. That is the wrong trade
for a service somebody has to debug under pressure, and 29 MB against the Python image's 319 MB
is still the same argument.

```bash
docker exec -it model-router sh                  # a shell, as the service account
docker exec -it -u root model-router \
  sh -c 'apk add --no-cache curl bind-tools'     # pull in tools mid-investigation
docker exec model-router /app/model-router --healthcheck
```

Two details in there are load-bearing:

- **The account is created before the `COPY`s**, and the files land with `COPY --chown`. A later
  `chown -R /app` instead rewrites every file into a fresh layer, and since `/app` holds the
  binary with the whole console inside it, that one command took the image from 22 MB to 42 MB.
- **`/data` is created and chowned in the image.** A named volume inherits the ownership of the
  directory it is mounted over, so without this the server — running as uid 10001 — cannot seed
  `config.yaml` onto a fresh volume. That is a first-start failure for every new deployment, and
  invisible on a bind mount the operator had already chowned by hand, so the CI smoke test
  asserts the resulting ownership.

The health check is the binary probing itself (`model-router --healthcheck`) rather than a
shelled-out `wget`: it reports the loaded provider list, so an unhealthy container is one that
cannot read its configuration rather than merely one whose port is closed — and it keeps working
if the base image ever drops the tool a shell-based check would have relied on.

## Logs

The Go server writes one line per HTTP request, to stdout:

```
2026/09/13 23:38:27 INFO mr: 172.17.0.1 "GET /v1/usage?days=7 HTTP/1.1" 200 4182 3.1ms
2026/09/13 23:38:27 INFO mr: 172.17.0.1 "GET /v1/access/discover HTTP/1.1" 422 113 0.0ms
2026/09/13 23:38:28 INFO mr: route id=3293fe6a user=alice model=gpt-4o provider=foundry ...
```

This exists because the Python backend gets it for free from uvicorn and this server is its own
HTTP stack. Without it `docker logs` showed only the handful of events that log explicitly, so
browsing the console produced nothing at all — and a request that failed in the UI left no trace
on the server, which is the worst possible time for the log to be silent.

The duration is measured to the last byte written, so for a streamed answer it is the whole
stream: that is how long the caller actually waited. `X-Forwarded-For` is preferred over the
socket address, or behind a proxy every line would name the proxy.

Set `MR_ACCESS_LOG=off` on a deployment that takes its access log from a proxy in front.

### When an upstream call fails

Two flags help where the message alone does not:

```bash
docker exec model-router /app/model-router --resolve api.example.openai.azure.com
docker exec model-router /app/model-router --healthcheck
```

`--resolve` uses **the server's own resolver**, which is Go's rather than the shell's. That
distinction matters: `nslookup` inside the container goes through musl and the two do not always
agree, so a shell that resolves a name is not proof that the router can.

[`server-go/scripts/diagnose-dns.sh`](../server-go/scripts/diagnose-dns.sh) walks the whole chain
— host, container resolver, UDP vs TCP, then Go — and says which layer to fix:

```bash
server-go/scripts/diagnose-dns.sh admin-xxxx-eastus2.openai.azure.com model-router
```

A "no such host" from inside a container is almost never a wrong endpoint. It is the container's
DNS: a resolver inherited from the host that the container's namespace cannot reach, a VPN
resolver, or an embedded DNS that mishandles a long CNAME chain — an Azure OpenAI endpoint has
seven of them. The router now says so in the error rather than leaving the reader to check a URL
that was already correct.

## Continuous integration

| Workflow | Runs on | Does |
|---|---|---|
| [`go.yml`](../.github/workflows/go.yml) | every push and PR touching `server-go/**` | gofmt, cross-compile for both release platforms, `go vet`, `go test -race` |
| [`docker-publish.yml`](../.github/workflows/docker-publish.yml) | version tags, plus PRs touching either backend | builds **both** images for amd64 + arm64, publishes on a tag, and smoke-tests what it published |

The Go checks are a separate workflow because they need no Docker, no registry credentials and
no tag, so they can run on every commit and finish in under a minute — a compile error is then
caught where it was written rather than as a failed image build at release time.

The smoke test starts the published image on an empty named volume and asserts, from a busybox
container mounted over the same volume, that `config.yaml` was seeded and the traces directory
created. Checked from outside rather than with `docker exec` because the Go image has no shell
to exec — and checking the volume is the more honest test anyway, since the volume is what
survives an upgrade.

---

## What the two share

Everything that is *state* or *contract*:

- **`data/`** — `config.yaml`, `logs/traces/`, `auth_sessions.json`, `api_keys.json`,
  `known_users.json`, `github/`, the usage rollup and the AI-credit snapshot. Both read and write
  the same files with the same semantics, including the atomic-replace writes and the
  cross-process leases, so they can even take turns on one directory.
- **The REST API** — every route under `/v1`, with the same status codes and the same
  `{"detail": ...}` error shape.
- **The console** — the same `frontend/dist` build. The Go binary embeds a copy so it runs
  standalone, but prefers `frontend/dist` on disk when there is one, so `npm run build` shows up
  without recompiling Go.
- **Trace records** — structurally identical JSON, including the interaction-folding rules. A
  trace written by one backend opens correctly in the other's console.

Two small response differences are deliberate and documented in
[backend-benchmark.md](backend-benchmark.md#two-deliberate-output-differences).

### config.yaml writes

Saving from the console is the one operation that rewrites a file the operator maintains by
hand, so the Go writer is held to a strict property: **GET /v1/config, submit it back unedited,
and the file does not move by a single byte** — comments, key order, quoting, sequence
indentation, multi-line prompts and line endings all survive. `internal/config/save_test.go`
enforces it, including for CRLF files, and for repeated saves.

Reaching that took three fixes worth knowing about:

- **CRLF files.** Given Windows line endings the YAML parser reads each `\r` as a blank line of
  its own and hands back a header comment that is already doubled. Left alone, every save added
  one blank line per comment line — the file grew each time. The bytes are now normalised to LF
  before parsing and the original convention is restored on write.
- **Multi-line strings.** The AI decision prompt came back as one folded, single-quoted blob with
  every newline doubled: valid YAML, unusable by the person who has to edit it. Multi-line values
  are now written as `|` literal blocks.
- **Sequence indentation.** go-yaml indents a sequence's dashes under their key; ruamel writes
  them flush with it. Both parse identically, but alternating backends would have churned 80
  lines of an operator's file. The rendered text is re-indented to the flush form and then
  **verified by re-parsing** — on any mismatch the emitter's own output is kept, so the worst
  case is the indented style rather than a damaged credentials file.

One difference remains, in Go's favour: the Python backend normalises a CRLF file to LF on save,
and the Go backend preserves it. The content the two write is identical.

---

## The Go source layout

Each package mirrors the Python module of the same name, so the two are readable side by side:

| Go | Python | |
|---|---|---|
| `internal/config` | `app/config.py` | `config.yaml` loading, validation, comment-preserving writes |
| `internal/routing` | `app/routing.py` | the three routing strategies |
| `internal/wire` | `app/wire.py` | OpenAI ↔ Anthropic conversion, both directions, streaming included |
| `internal/upstream` | `app/providers.py` + `app/anthropicapi.py` | the upstream clients and their pool |
| `internal/traces` | `app/traces.py` | trace storage and interaction folding |
| `internal/authstore` | `app/authstore.py` | sessions, API keys, the known-user registry |
| `internal/auth` | `app/auth.py` | OAuth, cookies, API-key authentication |
| `internal/localadmin` | `app/localadmin.py` | the local super administrator's credential |
| `internal/keyscope` | `app/keyscope.py` | what one key may reach |
| `internal/keypolicy` | `app/keypolicy.py` | who may create keys |
| `internal/scopepolicy` | `app/scopepolicy.py` | who may narrow a key |
| `internal/modelpolicy` | `app/modelpolicy.py` | which models a caller may use |
| `internal/ghadmin` | `app/ghadmin.py` | the GitHub Enterprise API client |
| `internal/ghcache` | `app/ghcache.py` | the on-disk membership cache |
| `internal/aicredits` | `app/aicredits.py` | Copilot credit polling and the BYOK gate |
| `internal/usagestats` | `app/usagestats.py` | the background usage rollup |
| `internal/cronexpr` | `app/cronexpr.py` | the five-field cron parser |
| `internal/release` | `app/release.py` | the release check |
| `internal/server` | `app/main.py` | the HTTP surface |
| `internal/omap` | — | an ordered map; see below |

### Why `internal/omap` exists

Python dictionaries preserve insertion order, and this project depends on that. The order of
`models` in `config.yaml` decides the catalog order, which decides the default model, the
candidate list handed to the decision model, and the order every narrowed model list comes back
in. A Go `map` would randomise all of it on every request.

`omap.Map` is an insertion-ordered map that round-trips through both JSON and YAML without losing
key order, and keeps numbers as `json.Number` so an integer written by one backend does not read
back as a float in the other. It is what makes the console's GET → edit → PUT round-trip of
`config.yaml` come back looking like the file the operator wrote.

---

## Building and testing the Go backend

```bash
server-go/scripts/build.sh        # embeds frontend/dist if present, writes server-go/bin/model-router
cd server-go && go test ./...     # unit tests for the pure logic
cd server-go && go vet ./...
```

The unit tests cover the parts where a subtle mistake would be invisible in production: the
protocol conversions in both directions, the streaming decoder and encoder, the cron parser, the
version comparison, key-scope intersection, and the ordered map's YAML/JSON round trip.
