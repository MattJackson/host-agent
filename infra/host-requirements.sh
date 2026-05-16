#!/bin/bash
# One-time host setup for host-agent. Run by dr/restore.sh during DR or
# manually when bringing up a new host. Idempotent.
#
# Loads ipmi_devintf so /dev/ipmi0 appears (Dell PowerEdge BMCs); on
# non-IPMI hosts (Unraid on consumer hardware, etc.) the modprobe fails
# silently and the in-container fan-controller / ipmi-exporter sub-services
# self-disable. No-op the rest of the time.
set -euo pipefail

# 1. ipmi_devintf — only meaningful on Dell-class servers. Best-effort.
if ! lsmod | grep -q '^ipmi_devintf '; then
  modprobe ipmi_devintf 2>/dev/null || echo "note: ipmi_devintf not loaded (non-IPMI host?)"
fi

# Persist across reboots if the module is available.
if modinfo ipmi_devintf >/dev/null 2>&1; then
  install -m 644 /dev/stdin /etc/modules-load.d/ipmi.conf <<<'ipmi_devintf'
fi

# 2. State dir for the fan controller (EWMA baseline + textfile metrics).
install -d -m 755 /var/lib/fan-controller/state

# 3. One-shot migration from the old /var/lib/dell-fans path. Safe to
#    leave indefinitely — the guard checks for the EWMA state file at its
#    new path, so the migration runs at most once per host. Docker may
#    have auto-created an empty /var/lib/fan-controller on first launch
#    of the renamed image; cp -a happily merges into it.
if [ -f /var/lib/dell-fans/state/base ] && [ ! -f /var/lib/fan-controller/state/base ]; then
  echo "host-agent host-requirements: migrating /var/lib/dell-fans → /var/lib/fan-controller (preserves EWMA)"
  cp -a /var/lib/dell-fans/. /var/lib/fan-controller/
  rm -rf /var/lib/dell-fans
fi

echo "host-agent host-requirements OK"
