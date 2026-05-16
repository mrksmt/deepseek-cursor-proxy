#!/usr/bin/env bash
set -euo pipefail

# ── Build deepseek-cursor-proxy Docker image ───────────────────────────────
# Usage:
#   ./docker-build.sh                            # build Go image (Dockerfile)
#   ./docker-build.sh <tag>                      # build with custom tag

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

IMAGE_NAME="deepseek-cursor-proxy"
TAG="${1:-latest}"

IMAGE_TAG="${IMAGE_NAME}:${TAG}"

echo "==> Building ${IMAGE_TAG} using Dockerfile ..."
docker build -f Dockerfile -t "$IMAGE_TAG" .

echo "==> Done: ${IMAGE_TAG}"
