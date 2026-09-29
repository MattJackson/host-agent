#!/bin/sh
# Keep high-churn runtime files in memory even when an older container
# template did not configure a tmpfs mount. This mounts only host-agent's
# private subtree so Docker socket mounts under /run remain visible to
# cAdvisor.
set -eu

runtime_dir=/run/host-agent
mkdir -p "$runtime_dir"

if ! awk '$2 == "/run/host-agent" && $3 == "tmpfs" { found = 1 } END { exit !found }' /proc/mounts; then
  if ! mount -t tmpfs -o mode=0755,nosuid,nodev,size=256m tmpfs "$runtime_dir"; then
    echo "host-agent-entrypoint: ERROR: could not mount $runtime_dir as tmpfs; refusing disk-backed runtime writes" >&2
    exit 1
  fi
fi

exec /init "$@"
