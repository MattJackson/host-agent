#!/bin/bash
# Deploy host-agent on a Dell PowerEdge host. Idempotent, safe to re-run
# (called every 30 min via reconcile timer on classe).
#
# host-agent runs the dell-fans controller AND the per-host monitoring
# exporters (node, cadvisor, ipmi, smartctl, + nvidia-gpu via GPU overlay)
# bundled together so every Dell box gets the same uniform set.
#
# - Hard-fails if /dev/ipmi0 isn't present (host needs ipmi_devintf module
#   loaded; see infra/host-requirements.sh).
# - Auto-detects GPU on the host and manages a docker-compose.override.yml
#   symlink → docker-compose.gpu.yml so the standard compose override
#   convention applies the GPU layer. Plain `docker compose up` (including
#   the one in reconcile.sh) then picks it up automatically — no special
#   args needed downstream. Same files work on classe (P4+A5500) and
#   docker-2 (no GPU) with no per-host edits.
# - No bridge network needed — controller talks to the BMC, exporters are
#   on host network.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

if [ -f .env ]; then set -a; . ./.env; set +a; fi

if [ ! -c /dev/ipmi0 ]; then
  echo "ERROR: /dev/ipmi0 not present. Load ipmi_devintf (see host-requirements.sh)." >&2
  exit 1
fi

if command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
  ln -sfn docker-compose.gpu.yml docker-compose.override.yml
else
  rm -f docker-compose.override.yml
fi

# Migration helper: if a prior `dell-fans` project is still running on
# this host (pre-rename), tear it down first so its container claim on
# /dev/ipmi0 doesn't clash with the new host-agent project.
if docker ps --format '{{.Names}}' | grep -qx 'dell-fans'; then
  echo "Migrating: tearing down old 'dell-fans' project so host-agent can claim /dev/ipmi0"
  docker compose -p dell-fans down --remove-orphans || true
fi

docker compose -p host-agent up -d --remove-orphans
