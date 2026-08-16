# Model Router -- multi-stage build.
#
# Stage 1 builds the React console with Node; stage 2 is the runtime and carries only
# Python plus the built bundle, so Node and node_modules never ship. FastAPI serves
# frontend/dist from the site root, which is why the build is part of the image rather
# than something the operator has to remember: an image cannot go out with a stale bundle.

# ---------- stage 1: the console ----------
FROM node:22-alpine AS frontend

# Optional mirrors, for building on a network that cannot reach the public registries.
# Empty by default, so an unset build arg means "use registry.npmjs.org / pypi.org" and CI
# needs no configuration. Pass them only when needed:
#   docker build --build-arg NPM_REGISTRY=https://mirror.example/npm/ \
#                --build-arg PIP_INDEX=https://mirror.example/pypi/simple/ .
ARG NPM_REGISTRY=""

WORKDIR /build

# Manifests first, as their own layer: dependencies are reinstalled only when they actually
# change, not on every source edit. `npm ci` (not `install`) installs the exact
# package-lock.json tree, so an image built today and one built next month are identical.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci ${NPM_REGISTRY:+--registry "$NPM_REGISTRY"}

COPY frontend/ ./
RUN npm run build


# ---------- stage 2: the runtime ----------
FROM python:3.11-slim AS runtime

# See NPM_REGISTRY above. Unset = pypi.org. Deliberately not named PIP_INDEX_URL: that name
# is one pip reads from the environment, and an empty value would override the default with
# nothing rather than leaving it alone.
ARG PIP_INDEX=""

# PYTHONDONTWRITEBYTECODE: the image is read-only in practice, so .pyc files are dead weight.
# PYTHONUNBUFFERED: without it, `docker logs` shows nothing until a buffer fills, which makes
#   a crash at startup look like a hang.
# PYTHONIOENCODING: a model reply can contain emoji, and a non-UTF-8 default encoding turns
#   logging one into a UnicodeEncodeError that fails the request.
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONIOENCODING=utf-8 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# Dependencies before application code, so editing a .py file does not reinstall them.
COPY requirements.txt ./
RUN pip install --no-cache-dir ${PIP_INDEX:+--index-url "$PIP_INDEX"} -r requirements.txt

# The application. config.example.yaml is required at runtime, not just as documentation:
# a missing config.yaml is seeded from it on startup (app/config.py: ensure_config_file).
COPY app/ ./app/
COPY config.example.yaml ./
COPY --from=frontend /build/dist ./frontend/dist

# One variable, because everything mutable lives under one directory: config.yaml, the
# traces, the sessions, the keys and the GitHub cache are all positioned relative to
# MR_DATA_DIR by app/config.py. So the volume story is a single `-v mr-data:/data`.
#   /data/config.yaml         created from the template on first start, rewritten by the console
#   /data/logs/traces/        full-chain trace records
#   /data/auth_sessions.json  sign-in sessions
#   /data/api_keys.json       issued API keys
#   /data/github/             the cached GitHub structure and member lists
# Keeping these OUT of /app matters: /app is replaced wholesale by the next image, so
# anything stored there would be destroyed by an upgrade. MR_CONFIG_FILE and MR_LOG_DIR can
# still be set to split the configuration or the traces onto another disk.
ENV MR_DATA_DIR=/data

# Non-root: nothing here needs privilege. A fixed uid rather than a name so a host bind mount
# can be chowned to a number the operator can predict (`chown -R 10001:10001 ./mr-data`).
# A *named* volume inherits this directory's ownership automatically and needs no such step.
# Not --system: that flag expects a uid under 999, and a high fixed uid is the more useful
# property here (it is what a bind-mount chown has to target, and it cannot collide with a
# distro account added by a future base-image update).
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin mr \
    && mkdir -p /data \
    && chown -R mr:mr /data /app
USER mr

VOLUME ["/data"]
EXPOSE 8000

# Straight to /healthz, which reports the loaded provider list, so an unhealthy container is
# one that cannot load its configuration -- not merely one whose port is open.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=4).status == 200 else 1)"

# 0.0.0.0 is required: binding loopback inside a container makes the published port unreachable.
# Behind a TLS-terminating reverse proxy, append --proxy-headers --forwarded-allow-ips=<proxy ip>
# so the OAuth callback URL and the cookie's Secure flag follow X-Forwarded-Proto / -Host.
# That is deliberately not the default -- trusting those headers from any client would let a
# caller dictate the callback origin.
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000"]
