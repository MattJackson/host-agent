#!/bin/bash
# Thin reconcile wrapper. Safe to run every 30 min (compose-reconcile
# timer on classe calls this). On non-reconcile hosts the file is
# optional — watchtower keeps the container updated; this script just
# exists so the bootstrap path matches every other /srv stack.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

if [ -f .env ]; then set -a; . ./.env; set +a; fi

# Auto-detect nvidia runtime if not explicitly set. Sets the env var
# the compose interpolates into `runtime:`. Default (unset) stays runc.
if [ -z "${HOST_AGENT_RUNTIME:-}" ] \
   && command -v nvidia-smi >/dev/null 2>&1 \
   && nvidia-smi -L >/dev/null 2>&1; then
  export HOST_AGENT_RUNTIME=nvidia
fi

docker compose -p host-agent up -d --remove-orphans
