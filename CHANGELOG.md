# Changelog

All notable changes to this project are documented here. This project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) and the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.1.0] — TBD

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

[Unreleased]: https://github.com/mattjackson/host-agent/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.0
