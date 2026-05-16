#!/bin/sh
# host-agent one-shot installer for any Linux + Docker host.
#
# Required env (set before invoking):
#     HOST_AGENT_IMAGE                 e.g. ghcr.io/<user>/host-agent:latest
#     PROMETHEUS_REMOTE_WRITE_URL      your receiver's /api/v1/write endpoint
#
# Optional env (auth, mutually exclusive bearer XOR basic):
#     PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN
#     PROMETHEUS_REMOTE_WRITE_USERNAME / _PASSWORD
#     PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY=true
#
# Example:
#     export HOST_AGENT_IMAGE=ghcr.io/example/host-agent:latest
#     export PROMETHEUS_REMOTE_WRITE_URL=https://prom.example/api/v1/write
#     curl -sf https://example.com/host-agent-install.sh | sh
#
# What it does:
#   - pulls $HOST_AGENT_IMAGE
#   - stops any existing host-agent container
#   - starts the container with the standard mount/flag set and
#     forwards any PROMETHEUS_REMOTE_WRITE_* env vars set above so
#     vmagent can target your receiver.

set -eu

: "${HOST_AGENT_IMAGE:?must be set, e.g. ghcr.io/<user>/host-agent:latest}"
: "${PROMETHEUS_REMOTE_WRITE_URL:?must be set to the receiver /api/v1/write endpoint}"

IMAGE="$HOST_AGENT_IMAGE"
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

# Forward PROMETHEUS_REMOTE_WRITE_* env vars set in the caller's shell
# (if any) into the container. Empty / unset → omitted, so the image
# defaults take over.
ENV_ARGS=""
for v in PROMETHEUS_REMOTE_WRITE_URL \
         PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN \
         PROMETHEUS_REMOTE_WRITE_USERNAME \
         PROMETHEUS_REMOTE_WRITE_PASSWORD \
         PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY; do
  eval val=\${$v:-}
  if [ -n "$val" ]; then
    ENV_ARGS="$ENV_ARGS -e $v=$val"
  fi
done

docker run -d \
  --name "$NAME" \
  --restart unless-stopped \
  --privileged \
  --network host \
  --cgroupns host \
  --label com.centurylinklabs.watchtower.enable=true \
  $ENV_ARGS \
  -v /:/host:ro,rslave \
  -v /sys:/sys:ro \
  -v /run/docker.sock:/run/docker.sock \
  -v /run/containerd:/run/containerd:ro \
  -v /var/lib/docker:/var/lib/docker:ro \
  -v /dev:/dev \
  -v "$STATE_DIR":/var/lib/fan-controller \
  "$IMAGE"

echo
echo "host-agent-install: done. Container hostname (= dashboard label) is:"
hostname -s
echo
echo "Tail logs with:  docker logs -f $NAME"
