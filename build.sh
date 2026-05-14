#!/bin/bash
# Build dell-fans image and push to the private registry. Run on classe
# (where the registry lives). Other hosts just `docker compose pull`.
#
# CI'd be nicer eventually — for now this is the manual-but-explicit path.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

IMAGE="${IMAGE:-registry.docker.pq.io/dell-fans:latest}"

docker build -t "$IMAGE" .
docker push "$IMAGE"

echo
echo "Pushed $IMAGE — Watchtower on each host will pick it up within its poll interval."
