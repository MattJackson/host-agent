#!/bin/sh
# host-agent one-shot installer for any Linux + Docker host.
#
# Usage (on the new host, as a user that can run docker):
#     curl -sf https://docker.pq.io/host-agent-install.sh | sh
#
# What it does:
#   - logs in to the private registry (if you've already provided creds
#     via `docker login`, this is a no-op)
#   - pulls registry.docker.pq.io/host-agent:latest
#   - runs the container with:
#         --privileged --network host --restart=unless-stopped
#         -v /:/host:ro,rslave                       (host fs read view)
#         -v /dev:/dev                               (IPMI + smart devices)
#         -v /var/lib/host-agent:/var/lib/dell-fans  (fan baseline + WAL)
#         --label com.centurylinklabs.watchtower.enable=true
#
# Hardcoded flags — every host gets the same setup. To change a
# default, build + release a new image; watchtower bumps it fleet-wide.

set -eu

IMAGE="registry.docker.pq.io/host-agent:latest"
NAME="host-agent"
STATE_DIR="/var/lib/host-agent"

if ! command -v docker >/dev/null 2>&1; then
  echo "host-agent-install: docker not found on PATH" >&2
  exit 1
fi

mkdir -p "$STATE_DIR"

echo "host-agent-install: pulling $IMAGE"
docker pull "$IMAGE"

# Stop any previous container with the same name (idempotent re-runs)
if docker inspect "$NAME" >/dev/null 2>&1; then
  echo "host-agent-install: stopping existing $NAME"
  docker rm -f "$NAME" >/dev/null
fi

echo "host-agent-install: starting $NAME"
docker run -d \
  --name "$NAME" \
  --restart unless-stopped \
  --privileged \
  --network host \
  --label com.centurylinklabs.watchtower.enable=true \
  -v /:/host:ro,rslave \
  -v /sys:/sys:ro \
  -v /run:/run \
  -v /var/lib/docker:/var/lib/docker:ro \
  -v /dev:/dev \
  -v "$STATE_DIR":/var/lib/dell-fans \
  "$IMAGE"

echo
echo "host-agent-install: done. Container hostname (= dashboard label) is:"
hostname -s
echo
echo "Tail logs with:  docker logs -f $NAME"
