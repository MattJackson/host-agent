#!/bin/bash
# One-shot host setup for dell-fans. Run by dr/restore.sh on first
# install; run by hand when adding the stack to an existing host.
#
# - Loads ipmi_devintf (creates /dev/ipmi0).
# - Persists it via /etc/modules-load.d/ipmi.conf so it survives reboot.
#
# Safe to re-run.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "ERROR: must run as root" >&2
  exit 1
fi

# Most Dell PowerEdge hosts already have these auto-loaded by udev once
# the BMC is detected, but we make it explicit + persistent so a fresh
# kernel upgrade can't quietly drop the device.
modprobe ipmi_devintf
modprobe ipmi_si

cat > /etc/modules-load.d/ipmi.conf <<'EOF'
# Required by dell-fans: ipmitool needs /dev/ipmi0 to drive chassis fans.
ipmi_devintf
ipmi_si
EOF

if [ ! -c /dev/ipmi0 ]; then
  echo "ERROR: /dev/ipmi0 still missing after modprobe. BMC may not be exposed by this host." >&2
  exit 1
fi

# Adaptive baseline state (EWMA of learned equilibrium fan speed).
# Persisted across container restarts and image updates. Loss is OK —
# controller relearns from MIN_FAN over ~24-48 hrs.
mkdir -p /var/lib/dell-fans/state

echo "OK: /dev/ipmi0 present, modules persisted to /etc/modules-load.d/ipmi.conf, state dir at /var/lib/dell-fans/state"
