# Docker deployment

The recommended way to run this. Nothing has to be prepared first — no `config.yaml`, no
`pip install`, no frontend build. The image carries the built console, and the configuration is
**created for you** from the template on first start.

```bash
docker run -d --name model-router \
  -p 8000:8000 \
  -v mr-data:/data \
  --restart unless-stopped \
  ghcr.io/satomic/model-router:latest
```

Then open <http://localhost:8000/> and sign in as the **local super administrator** —
`admin` / `admin1234`, which forces a password change before anything else is reachable.

> Use the local administrator, not the setup wizard. The wizard is restricted to requests from
> `127.0.0.1`, and a browser on your host does not look like loopback to a container — it arrives
> as the Docker bridge address, so `can_setup` is `false` and the wizard stays hidden. This is why
> the local account exists; see [Sign-in and authentication](authentication.md).

From there, the console is the whole setup: fill in a backend connection under **Routing
configuration → Backend connections**, and optionally GitHub OAuth under **Access control**. Every
save is written back to `config.yaml` on the volume.

## The configuration file is created automatically

On startup the router copies [config.example.yaml](../config.example.yaml) to the configured
config path if nothing is there yet, and says so in the log:

```
INFO mr: created /data/config.yaml from /app/config.example.yaml -- sign in as the local administrator to configure it
```

The copy is byte-for-byte, so the seeded file keeps the template's comments documenting every
field. It carries **placeholders only** — no credentials — so a fresh deployment grants no access
to any backend until you configure one. An existing file is never overwritten: the message above
appears on a first start and never again, and seeing it later means the volume was not mounted
where the router expects it.

## Port mapping

`-p <host>:<container>`. **The container port is fixed at 8000** — that is what the process binds
and what the image's health check probes — so change only the host side:

| Flag | Result |
|---|---|
| `-p 8000:8000` | reachable at `http://localhost:8000/`, and from other machines |
| `-p 18000:8000` | same container, reachable at `http://localhost:18000/` |
| `-p 127.0.0.1:8000:8000` | **this machine only** — right when a reverse proxy on the same host is the only intended client |

Whatever host port you publish is also the base URL your OpenAI-compatible clients point at:
`http://<host>:<port>/v1`. Getting the port wrong shows up as a connection refused rather than an
auth error.

## Data volume

Everything that must outlive the container lives under a **single mount point, `/data`** — which
is the same layout a source checkout uses, so a deployment can be moved between the two by
copying one directory:

| Path | Contents | Losing it means |
|---|---|---|
| `/data/config.yaml` | the whole configuration, credentials included | backend connections, OAuth app, admin list and the local admin's password all reset to the template |
| `/data/auth_sessions.json` | sign-in sessions | everyone is signed out |
| `/data/api_keys.json` | issued API keys | every issued API key stops working |
| `/data/github/` | the cached GitHub structure and member lists | the next access decision falls back to a live GitHub probe and the cache refills |
| `/data/logs/traces/` | full-chain trace records | the call history and usage statistics are gone |

`-v mr-data:/data` is all that is required — there is nothing else to mount. **Mounting nothing
still runs** — Docker supplies an anonymous volume — but it is then easy to lose the state
without noticing, so name it.

Two forms, and the trade-off between them:

```bash
# A named volume: nothing to prepare, ownership is handled, but the files sit inside
# Docker's storage area rather than somewhere you can casually open.
-v mr-data:/data

# A bind mount: config.yaml is a normal file you can edit and back up with host tools.
# The container runs as uid 10001, so grant it access first, or the first start cannot
# create the configuration (a Linux host; Docker Desktop on macOS/Windows handles this).
mkdir -p ./mr-data && sudo chown -R 10001:10001 ./mr-data
-v ./mr-data:/data
```

Editing `config.yaml` by hand is a restart, not a reload: the file is re-read at startup and when
the console saves. Changing it from the console applies immediately.

Backing up is copying that one directory:

```bash
docker run --rm -v mr-data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/mr-backup.tar.gz -C /data .
```

The archive contains `config.yaml` with your provider keys, the OAuth client secret and any
Enterprise token — **treat it as a credential**, not as an ordinary backup.

The image sets one variable, `MR_DATA_DIR=/data`, and everything else is positioned relative to
it. The other two are available for splitting the configuration or the traces onto a different
disk, and are otherwise not needed:

| Variable | Default | What it holds |
|---|---|---|
| `MR_DATA_DIR` | `/data` in the image, `<repo>/data` from source | **the root of all persistent state** — the only one a normal deployment sets |
| `MR_CONFIG_FILE` | `<data>/config.yaml` | the configuration file to read and write |
| `MR_LOG_DIR` | `<data>/logs` | trace records, under a `traces/` subdirectory |

## Upgrading

The image holds no state, so an upgrade is a replacement — the volume carries everything across:

```bash
docker pull ghcr.io/satomic/model-router:latest
docker rm -f model-router
docker run -d --name model-router -p 8000:8000 -v mr-data:/data \
  --restart unless-stopped ghcr.io/satomic/model-router:latest
```

Pin a released tag for anything real. Every version tag publishes `1.2.3`, `1.2`, `1` and
`latest`, so `:1.2` tracks patches while `:latest` can move a major version under you on a
restart.

> **Upgrading from a version that kept state in `/data/state`.** Earlier images split the volume
> into `/data/config.yaml`, `/data/state` and `/data/logs` via three environment variables; now
> everything is positioned under one, `MR_DATA_DIR=/data`. Sessions and keys therefore move up
> one level. If your volume has a `state/` directory, move its contents into `/data` once:
>
> ```bash
> docker run --rm -v mr-data:/data alpine \
>   sh -c 'mv -n /data/state/* /data/state/.[!.]* /data/ 2>/dev/null; rmdir /data/state'
> ```
>
> Skipping it costs the sessions, the issued API keys and the GitHub cache — not the
> configuration, which was already at `/data/config.yaml`. The startup log tells you which paths
> are in use.

## Health

The image ships a `HEALTHCHECK` that polls `/healthz`, which reports the loaded provider list — so
an unhealthy container is one that cannot read its configuration, not merely one whose port is
closed.

```bash
docker ps                       # STATUS shows (healthy) / (unhealthy)
curl http://localhost:8000/healthz
docker logs model-router
```

## Behind a reverse proxy

Terminating TLS elsewhere means the router has to be told, or the OAuth callback URL is built with
the wrong scheme and the session cookie loses its `Secure` flag:

Append the uvicorn flags to the `docker run` command — anything after the image name replaces
the image's default command:

```bash
docker run -d --name model-router -p 8000:8000 -v mr-data:/data \
  --restart unless-stopped ghcr.io/satomic/model-router:latest \
  uvicorn app.main:app --host 0.0.0.0 --port 8000 \
  --proxy-headers --forwarded-allow-ips 10.0.0.2
```

Set `--forwarded-allow-ips` to the proxy's address rather than `*`: those headers are
client-supplied, and trusting them from anyone lets a caller dictate the callback origin. The
GitHub OAuth App's callback URL must match the public address — see
[Sign-in and authentication](authentication.md).

## The image

| Property | Value |
|---|---|
| Registry | `ghcr.io/satomic/model-router` |
| Platforms | `linux/amd64`, `linux/arm64` |
| Base | `python:3.11-slim` (the console is built in a discarded `node:22-alpine` stage) |
| Size | ~275 MB |
| Runs as | uid `10001`, non-root |
| Published by | [.github/workflows/docker-publish.yml](../.github/workflows/docker-publish.yml) on a `v*` tag |

Building it yourself:

```bash
docker build -t model-router .

# On a network that cannot reach pypi.org / registry.npmjs.org:
docker build -t model-router \
  --build-arg PIP_INDEX=https://mirror.example/pypi/simple/ \
  --build-arg NPM_REGISTRY=https://mirror.example/npm/ .
```

The frontend is built inside the image, so a published image can never carry a stale bundle. No
credential is ever baked in: [.dockerignore](../.dockerignore) excludes `data/` (which is where
the configuration, keys, sessions and traces all live) and `.env` from the build context, so they
cannot be copied in even by accident.

## Publishing a release

```bash
git tag v1.0.0
git push origin v1.0.0
```

A tag `v1.0.0` publishes five tags — `1.0.0`, `1.0`, `1`, `v1.0.0` and `latest` — so a deployment
can pin as tightly or as loosely as it likes. A manual `workflow_dispatch` run off a branch gets a
throwaway `branch-<sha>` tag instead and does **not** move `latest`, so `latest` only ever points
at a real release.

The workflow builds both architectures, pushes to GHCR, and then **smoke-tests the image it just
published** — starting it on an empty volume and requiring `/healthz` to answer and
`config.yaml` to have been auto-created. A broken image fails the run instead of sitting under
`latest` with a green check. Pull requests that touch the Dockerfile or the application build the
image but never push it.

The build cache goes to the GitHub Actions cache, never to a tag on this package. A registry cache
exported to `<image>:buildcache` would appear in the package's own tag list, and since the cache is
written *after* the image push it would be the newest manifest — which is what GHCR shows as a
package's headline, so the package page would advertise `docker pull ...:buildcache`, a cache
manifest that will not run. Should the emulated arm64 leg ever need a warm cache badly enough, the
answer is a separate `<image>-cache` package, not a tag on the one people pull from.

The first published package is private; make it public from the repository's **Packages** page if
you want `docker pull` to work without a login.
