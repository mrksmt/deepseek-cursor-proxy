#!/usr/bin/env bash
set -euo pipefail

# ── Run deepseek-cursor-proxy Docker container ─────────────────────────────
# Usage:
#   ./docker-run.sh                              # default port
#   ./docker-run.sh --ngrok --verbose             # pass extra CLI flags
#
# Environment variables (optional):
#   DEEPSEEK_PORT      host port (default: 9000)
#   DEEPSEEK_NGROK     "true" to enable ngrok inside container
#   NGROK_AUTHTOKEN    ngrok authtoken

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

IMAGE="deepseek-cursor-proxy:latest"
CONTAINER_NAME="deepseek-cursor-proxy"

# ── Check image exists ─────────────────────────────────────────────────────
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "==> Image '${IMAGE}' not found. Run ./docker-build.sh first."
    exit 1
fi

# ── Configuration ──────────────────────────────────────────────────────────
HOST_PORT="${DEEPSEEK_PORT:-9000}"
NGROK_FLAG=""
NGROK_AUTH=""

# Map DEEPSEEK_NGROK env var to --ngrok flag for the entrypoint
if [ "${DEEPSEEK_NGROK:-}" = "true" ]; then
    NGROK_FLAG="-e DEEPSEEK_NGROK=true"
fi

# Pass NGROK_AUTHTOKEN if set
if [ -n "${NGROK_AUTHTOKEN:-}" ]; then
    NGROK_AUTH="-e NGROK_AUTHTOKEN=${NGROK_AUTHTOKEN}"
fi

# ── Stop & remove existing container ───────────────────────────────────────
docker stop "$CONTAINER_NAME" 2>/dev/null || true
docker rm "$CONTAINER_NAME" 2>/dev/null || true

# ── Run ────────────────────────────────────────────────────────────────────
echo "==> Starting ${CONTAINER_NAME} on port ${HOST_PORT} ..."
# shellcheck disable=SC2086
docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -p "${HOST_PORT}:9000" \
    -p 4040:4040 \
    -e DEEPSEEK_HOST=0.0.0.0 \
    -e DEEPSEEK_PORT=9000 \
    ${NGROK_FLAG:-} \
    ${NGROK_AUTH:-} \
    -v "${CONTAINER_NAME}-data:/data" \
    "$IMAGE" \
    "$@"

echo "==> Container started. Follow logs with: docker logs -f ${CONTAINER_NAME}"
