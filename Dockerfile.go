# Model Router -- the Go backend, as a single static binary.
#
# Kept beside the Python Dockerfile rather than folded into it as a build target: the two
# runtimes share no base image, no dependency step and no entrypoint, and a single file trying
# to be both would be mostly conditionals. The two images are interchangeable at run time --
# same /data layout, same config.yaml, same REST API -- so switching is a matter of changing
# the image name.
#
#   docker build -f Dockerfile.go -t model-router:go .
#   docker run -p 8000:8000 -v mr-data:/data model-router:go
#
# Stage 1 builds the React console with Node; stage 2 compiles the server with the console
# embedded; stage 3 is a small Alpine runtime carrying that one binary. No interpreter and no
# dependency tree ship -- but a shell does, deliberately, so a misbehaving container can be
# opened with `docker exec -it <container> sh`. See stage 3.

# ---------- stage 1: the console ----------
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend

# Optional mirror, for building on a network that cannot reach the public registry. Empty by
# default, so an unset build arg means "use registry.npmjs.org" and CI needs no configuration.
ARG NPM_REGISTRY=""

WORKDIR /build

# Manifests first, as their own layer: dependencies are reinstalled only when they actually
# change, not on every source edit. `npm ci` (not `install`) installs the exact
# package-lock.json tree, so an image built today and one built next month are identical.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --fetch-retries=2 --fetch-timeout=120000 \
      ${NPM_REGISTRY:+--registry "$NPM_REGISTRY"}

COPY frontend/ ./
RUN npm run build


# ---------- stage 2: the server ----------
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

# Optional module proxy, same reasoning as NPM_REGISTRY above.
ARG GOPROXY=""
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}

WORKDIR /src

# The module graph before the sources, so a code edit does not re-download the dependencies.
COPY server-go/go.mod server-go/go.sum ./
RUN go mod download

COPY server-go/ ./
# The console is embedded rather than copied next to the binary: one file to ship, and a binary
# that carries its own UI cannot go out with a stale copy beside it.
COPY --from=frontend /build/dist ./internal/web/dist

# config.example.yaml is required at runtime, not just as documentation: a missing config.yaml
# is seeded from it on startup. It is picked up through MR_ROOT below.
COPY config.example.yaml /out/config.example.yaml

# CGO off and a static link, so the binary does not depend on the runtime image's libc --
# which is musl on Alpine, not the glibc the builder has. -trimpath keeps the build
# reproducible; -s -w drop the symbol table.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/model-router ./cmd/model-router


# ---------- stage 3: the runtime ----------
#
# Alpine rather than scratch. A scratch image is ~8 MB smaller and has nothing to patch, but it
# also has no shell: `docker exec -it <container> sh` fails, and so does every other way of
# looking inside a container that is misbehaving in production. That trade is the wrong way
# round for a service an operator has to debug at three in the morning -- and 21 MB against
# 319 MB for the Python image is still the same argument.
#
# What this buys: sh, ps, netstat, wget, cat, vi and the rest of busybox, plus apk to add
# anything else while a container is being investigated.
FROM alpine:3.21 AS runtime

# ca-certificates: the release check and the GitHub Enterprise calls speak TLS, and Alpine ships
# no trust store of its own. tzdata: so a timestamp formatted in a non-UTC zone is correct rather
# than silently falling back to UTC.
RUN apk add --no-cache ca-certificates tzdata

# The account before the files, so the copies can set their ownership as they land. A later
# `chown -R /app` would instead rewrite every file into a new layer -- and since /app holds the
# binary with the console inside it, that one command doubled the image from 22 MB to 42 MB.
RUN adduser --disabled-password --uid 10001 --home /home/mr --shell /bin/sh mr

COPY --from=build --chown=mr:mr /out/model-router /app/model-router
COPY --from=build --chown=mr:mr /out/config.example.yaml /app/config.example.yaml

# One variable, because everything mutable lives under one directory: config.yaml, the traces,
# the sessions, the keys and the GitHub cache are all positioned relative to MR_DATA_DIR. So
# the volume story is a single `-v mr-data:/data`.
#   /data/config.yaml         created from the template on first start, rewritten by the console
#   /data/logs/traces/        full-chain trace records
#   /data/auth_sessions.json  sign-in sessions
#   /data/api_keys.json       issued API keys
#   /data/github/             the cached GitHub structure and member lists
# Keeping these OUT of /app matters: /app is replaced wholesale by the next image.
# MR_ROOT tells the binary where config.example.yaml lives: there is no repository tree in the
# image for it to discover one from.
ENV MR_DATA_DIR=/data \
    MR_ROOT=/app

# Non-root: nothing here needs privilege. The account is created above, before the COPYs, and
# is a real one rather than a bare numeric uid so a shell opened with `docker exec` has a name,
# a home and a prompt instead of `I have no name!`. The uid is fixed and high for the same
# reasons the Python image fixes one: it is what a host bind-mount chown has to target
# (`chown -R 10001:10001 ./mr-data`), and it cannot collide with an account a future base-image
# update adds.
#
# /data is created here, owned by that account: a *named* volume inherits the ownership of the
# directory it is mounted over, and without it the first start cannot seed config.yaml onto an
# empty volume. Only the empty directory is chowned, so this costs nothing.
RUN mkdir -p /data && chown mr:mr /data
USER mr

VOLUME ["/data"]
EXPOSE 8000

# The binary probes itself rather than shelling out to wget. It hits /healthz, which reports the
# loaded provider list -- an unhealthy container is one that cannot load its configuration, not
# merely one whose port is open -- and it keeps working if the base image ever loses the tool a
# shell-based check would have depended on.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/app/model-router", "--healthcheck"]

# 0.0.0.0 is required: binding loopback inside a container makes the published port
# unreachable. Behind a TLS-terminating reverse proxy the OAuth callback URL and the cookie's
# Secure flag follow X-Forwarded-Proto / X-Forwarded-Host automatically.
#
# To look inside a running container:
#   docker exec -it model-router sh        # a shell, as the unprivileged service account
#   docker exec -it -u root model-router sh -c 'apk add --no-cache curl bind-tools'
#   docker exec model-router /app/model-router --healthcheck
ENTRYPOINT ["/app/model-router"]
CMD ["--host", "0.0.0.0", "--port", "8000"]
