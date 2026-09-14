#!/usr/bin/env bash
# Launch Model Router on either backend.
#
# The two backends are interchangeable: same data/ directory, same config.yaml, same REST API,
# same trace format. Switching between them needs no migration and no configuration change --
# stop one, start the other.
#
#   ./run.sh                  # the default backend (go)
#   ./run.sh --backend python # the FastAPI/uvicorn backend
#   ./run.sh --backend go     # the Go backend
#   MR_BACKEND=python ./run.sh
#
# Extra flags:
#   --port N / --host ADDR    where to listen (default 0.0.0.0:8000)
#   --build                   rebuild the Go binary (and the console, with --frontend)
#   --frontend                build the console first (npm ci && npm run build)
#   --reload                  python only: uvicorn's auto-reload
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

BACKEND="${MR_BACKEND:-go}"
HOST="${MR_HOST:-0.0.0.0}"
PORT="${MR_PORT:-8000}"
BUILD=0
FRONTEND=0
RELOAD=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backend|-b) BACKEND="$2"; shift 2 ;;
    --backend=*)  BACKEND="${1#*=}"; shift ;;
    --port|-p)    PORT="$2"; shift 2 ;;
    --port=*)     PORT="${1#*=}"; shift ;;
    --host)       HOST="$2"; shift 2 ;;
    --host=*)     HOST="${1#*=}"; shift ;;
    --build)      BUILD=1; shift ;;
    --frontend)   FRONTEND=1; shift ;;
    --reload)     RELOAD=1; shift ;;
    # The header comment above is the help text, printed up to the first non-comment line, so
    # the two can never drift apart. awk rather than sed: BSD sed has no \| alternation.
    -h|--help)    awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; exit 0 ;;
    *) echo "unknown option: $1 (try --help)" >&2; exit 2 ;;
  esac
done

if [[ "$BACKEND" != "go" && "$BACKEND" != "python" ]]; then
  echo "unknown backend: $BACKEND (expected 'go' or 'python')" >&2
  exit 2
fi

if [[ $FRONTEND -eq 1 ]]; then
  echo "==> building the console"
  (cd frontend && npm ci && npm run build)
fi

case "$BACKEND" in
  go)
    BIN="server-go/bin/model-router"
    if [[ $BUILD -eq 1 || ! -x "$BIN" ]]; then
      echo "==> building the Go backend"
      server-go/scripts/build.sh
    fi
    echo "==> starting the Go backend on http://$HOST:$PORT"
    exec "$BIN" --host "$HOST" --port "$PORT"
    ;;
  python)
    PY="${PYTHON:-python3}"
    echo "==> starting the Python backend on http://$HOST:$PORT"
    ARGS=(-m uvicorn app.main:app --host "$HOST" --port "$PORT")
    [[ $RELOAD -eq 1 ]] && ARGS+=(--reload)
    exec "$PY" "${ARGS[@]}"
    ;;
esac
