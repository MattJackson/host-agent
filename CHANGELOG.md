# Changelog

All notable changes to this project are documented here. This project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) and the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.1.5] — 2026-05-16

### Changed

- **Unraid template no longer has a `PROMETHEUS_REMOTE_WRITE_URL` field.**
  After v0.1.4 added the persistent file fallback, the template's URL
  env var became redundant — and worse, the second path was the one
  Force Update kept clobbering. v0.1.5 drops the env field from the
  template entirely. Setup is now a single SSH one-liner that creates
  both the appdata URL file and fetches the template; the web-UI step
  has zero required fields to fill in.
- Container code unchanged from v0.1.4 — env var still works for
  Docker/compose deploys where the template-UI dance doesn't apply.

### Migration

Existing Unraid installs that already have a URL file at
`/mnt/user/appdata/host-agent/config/remote_write_url` keep working.
Future Force Updates won't try to wipe the env field because the
field doesn't exist.

## [0.1.4] — 2026-05-16

### Fixed

- **Unraid Force Update no longer breaks metric push permanently.** Some
  Unraid Force Update paths reset env vars to the template's `Default`
  value, which in v0.1.2/v0.1.3 was the example.com placeholder. vmagent
  would then push to a non-existent domain instead of self-disabling.
  v0.1.4 detects the literal placeholder and treats it as unset.

### Added

- **Persistent URL fallback file**: `/var/lib/fan-controller/config/remote_write_url`
  (on Unraid: `/mnt/user/appdata/host-agent/config/remote_write_url`).
  If `PROMETHEUS_REMOTE_WRITE_URL` env is unset or equal to the placeholder,
  vmagent reads the URL from this file instead. The file lives in the
  state mount, so it survives container recreations, image updates, AND
  template re-curls — set it once, never reconfigure across updates.
- vmagent's "disabled" log message now points users at both options
  (env var or fallback file) with the Unraid appdata path called out.

## [0.1.3] — 2026-05-16

### Changed

- **Active-GPU chassis assist is now own-fan-driven, not temperature-driven.**
  Workstation cards (A5500, RTX A6000, RTX 4000 SFF Ada, etc.) have their
  own dual-axial fans designed to be quieter than chassis fans across the
  card's working temp envelope. Previously the controller would lift
  chassis fans when the active GPU temp exceeded `ACTIVE_GPU_TARGET=78`,
  trading a quiet card-fan for louder chassis fans. The new policy: chassis
  stays at zero assist while the card's OWN fan has self-cooling headroom;
  only when own-fan crosses `ACTIVE_GPU_OWN_FAN_THRESHOLD` (default 85%) does
  chassis ramp in, linearly from `MIN_FAN` to `MAX_FAN` as own-fan goes
  85→100. `ACTIVE_GPU_EMERGENCY` (default 88°C) remains as a temperature-
  based safety net for failed card-fan scenarios.
- **Removed** `ACTIVE_GPU_TARGET`, `ACTIVE_GPU_APPROACH_WINDOW`,
  `ASSIST_GAIN` env vars and the temperature-based active-GPU proximity
  floor. The own-fan signal supersedes all of them. Old env entries are
  silently ignored.

### Added

- `fan_controller_active_gpu_own_fan_percent` and
  `fan_controller_active_gpu_own_fan_threshold` metrics exposed via the
  textfile collector so dashboards can plot own-fan saturation vs threshold.

### Migration

No action needed. Existing deployments pick up the change automatically on
container update; cold A5500 with own-fan <85% now produces zero chassis
assist (quieter chassis), while a maxed-out card still gets help.

## [0.1.2] — 2026-05-16

### Added

- **Natural Unraid drive order** in the bundled Grafana dashboard. The
  `unraid-disks` sub-service now emits a `sort_key` label per slot
  (parity → disk → cache → flash, with zero-padded numerics) and the
  Drive Temperatures panel wraps its queries in `sort_by_label()` to
  render drives in the same order Unraid's own UI uses.

### Fixed

- **Unraid Force Update no longer clears `PROMETHEUS_REMOTE_WRITE_URL`.**
  The CA template's `Default=""` combined with `Required="true"` triggered
  Unraid's saved-vs-default merge logic to revert the user-set URL on
  every image redeploy. Replaced with a placeholder default
  (`https://your-prometheus.example.com/api/v1/write`) so user values
  persist across Force Updates.

### Notes for receivers

The bundled dashboard now uses `sort_by_label`, a PromQL function that
Prometheus 3.x gates behind `--enable-feature=promql-experimental-functions`.
The bundled `examples/server-side/` compose already sets the flag; users
running their own Prometheus must add it manually.

## [0.1.1] — 2026-05-16

### Added

- **`unraid-disks` textfile sub-service** emits `unraid_disk_info{device,slot}`
  mapping Unraid's array slot labels (`disk1`, `parity`, `cache`, ...) to
  Linux device names by parsing `/host/var/local/emhttp/disks.ini`. The
  Drive Temperatures panel joins on `(host, device)` so Unraid drives
  show the same bay names the Unraid UI uses (`disk1 HDD`, `cache SSD`)
  instead of bare Linux device letters.
- Self-disables cleanly on non-Unraid hosts (probes `/host/etc/unraid-version`).

## [0.1.0] — 2026-05-16

First public release.

### Added

- **Adaptive Dell PowerEdge fan controller**, written in Go. Per-class PIDs
  (CPU, passive GPU, active GPU, HDD, SSD) emit candidate fan speeds; `max()`
  across all candidates plus per-class proximity-to-emergency floors drives
  the chassis. EWMA-tracked equilibrium baseline persisted across restarts.
- **Bundled Prometheus exporters**: `node_exporter`, `cadvisor`,
  `ipmi_exporter`, `smartctl_exporter`, `nvidia_gpu_exporter`. Each
  self-disables if its hardware is absent — same image runs on any Linux
  Docker host.
- **`vmagent` push** to a Prometheus / VictoriaMetrics / Mimir / Grafana
  Cloud receiver via remote_write. Optional bearer or basic auth.
  `external_labels.host = $(hostname -s)` so new hosts appear in the
  dashboard on first push, no central config needed.
- **Per-chassis profiles** auto-loaded by `dmidecode` product_name. Shipped:
  R730xd, R730, R410, XC730xd-12. Operator overridable via `PROFILE=foo`.
- **s6-overlay supervision** — one container, one PID 1, per-sub-process
  restart isolation.
- **Unraid Community Applications template** (`install/host-agent.xml`) —
  one-click install, no further per-host config.
- **`install.sh` one-shot installer** for any Linux+Docker host.
- **Grafana dashboard** in `examples/grafana/server-overview.json`.
- **Server-side example compose** in `examples/server-side/` — minimal
  Prometheus + Grafana receiver to get a fresh user end-to-end in ~5 min.

[Unreleased]: https://github.com/mattjackson/host-agent/compare/v0.1.5...HEAD
[0.1.5]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.5
[0.1.4]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.4
[0.1.3]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.3
[0.1.2]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.2
[0.1.1]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.1
[0.1.0]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.0
