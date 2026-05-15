#!/bin/bash
# One-time host setup for host-agent. Run by dr/restore.sh during DR or
# manually when bringing up a new host. Idempotent.
#
# Loads ipmi_devintf so /dev/ipmi0 appears (Dell PowerEdge BMCs); on
# non-IPMI hosts (Unraid on consumer hardware, etc.) the modprobe fails
# silently and the in-container dell-fans / ipmi-exporter sub-services
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
install -d -m 755 /var/lib/dell-fans/state

echo "host-agent host-requirements OK"
