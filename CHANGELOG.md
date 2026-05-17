# Changelog

All notable changes to this project are documented here. This project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) and the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.3.2] — 2026-05-17

### Fixed — adaptive score functions are now satisficing, not optimizing

**Incident**: ~4 hours after the v0.3.1 fleet deploy, a host went from a quiet ~25% chassis-fan setpoint to **100%** with no thermal cause. HDDs were at 37°C — 13°C below MaxSafe — and the box was screaming.

**Root cause**: `scoreBalanced` was a deviation-from-`PreferredMid` optimizer:
```
score = |mean(temp) − PreferredMid| + 0.3*variance + 0.3*fan_change_rate
```
The reconciler scores three projected futures (now / target+1 / target−1) by adjusting *only* `TempMean` in the synthetic stats. `variance` and `fan_change_rate` were copied unchanged into all three → those terms cancel in the comparison. Net effect: `balanced` was a pure deviation minimizer with no signal for the cost of cooling. When observed HDD mean sat at 40°C against `PreferredMid=38`, the reconciler kept picking "drift target down" cycle after cycle (38→37→36). The PID, seeing a widening positive error, did exactly what an incremental PID does — added `error × FanGain` to the setpoint every cycle until fans saturated.

The drives sat at a perfectly fine 37°C while the box wasted electricity and noise satisfying a fictional emergency the adaptive layer had invented.

**Fix**: all four mode score functions are now **satisficing** over the envelope's preferred band rather than **optimizing** toward a single point. Anywhere inside `[PreferredLow, PreferredHigh]` scores zero on the band-violation term; outside it grows linearly with distance. New formulas in `mode/mode.go`:

| mode | new formula |
|---|---|
| `max-cool` | `max(0, mean − PreferredLow) + 0.5*var` |
| `balanced` | `bandDistance(mean, PreferredLow, PreferredHigh) + 0.3*var + 0.3*rate` |
| `min-noise` | `max(0, PreferredHigh − mean) + 5*max(0, mean − PreferredHigh) + 2*rate + 0.5*var` |
| `eco` | alias of `min-noise` (unchanged) |

Under satisficing, that case never happens. HDD at 37-40°C is inside `[32, 43]` → `bandViolation = 0` → all three projections score equally → reconciler picks `bestDelta=0` → target stays put → PID sees no artificial error → fans stay quiet. The PID saturation guardrail proposed during incident triage was not shipped — it's unnecessary once adaptive stops creating saturation conditions in the first place.

The design doc (`adaptive-controller-v2.md` §8) is updated to match.

### Migration

Drop-in image upgrade; no operator-visible config change. However, hosts that drifted target away from `PreferredMid` under v0.3.0/v0.3.1 will keep their drifted targets after upgrade — satisficing leaves in-band targets alone but doesn't actively pull them back. To reset to mode-initial targets:

```
sudo systemctl stop host-agent || sudo docker stop host-agent-host-agent-1
sudo rm /var/lib/host-agent/state/adaptive.json
sudo docker start host-agent-host-agent-1 || sudo systemctl start host-agent
```

The observer window (`observer.json`) does NOT need to be cleared — it's mode-agnostic.

The `adaptive_mode_preview_score` metric will show different numeric shapes (especially `min-noise`, which now penalizes crossing above `PreferredHigh`). The "lower = better fit" semantics are unchanged; only the magnitude of the numbers differs.

## [0.3.1] — 2026-05-17

### Added — mode preview metrics

Operators can now see what every other mode WOULD do without switching. New metrics emitted on every PID cycle alongside the current state:

- `adaptive_mode_preview_target_celsius{class,mode}` — initial target each mode would set, per class
- `adaptive_mode_preview_deadband_celsius{class,mode}` — initial deadband each mode would set
- `adaptive_mode_preview_score{class,mode}` — score of CURRENT observed temp distribution under each mode's intent. Lower = better fit for that mode given current hardware behavior

Two new panels on the Adaptive Controller dashboard:

- **Mode preview** — overlays current drifting target (thick solid) against the 4 modes' initial targets (thin dashed). At-a-glance "if I switched to min-noise right now, the target would jump from X to Y."
- **Mode fit score** — time-series of each mode's score against current observed stats. A mode whose score is consistently low over days means your hardware naturally behaves the way that mode wants — switching would require less drift work to settle. A high score means switching would force the controller to drift far before equilibrium.

Pure pre-existing math (`envelope.InitialTarget` + `mode.Score`); no new logic, just exposing what the reconciler already computes for other modes.

### Migration

Drop-in. No operator action; just pull `:latest`.

## [0.3.0] — 2026-05-17

### BREAKING — host path namespace

- **State directory: `/var/lib/fan-controller/` → `/var/lib/host-agent/`** everywhere. The new bind mount is `/var/lib/host-agent:/var/lib/host-agent`. The Go binary is still called `fan-controller` (it IS the fan controller; that name is accurate), but the host-side namespace is now consistent with the umbrella container name (`host-agent`).
- All sub-services updated: node-exporter's `--collector.textfile.directory`, vmagent's WAL + URL-fallback paths, unraid-disks textfile output, fan-controller's EWMA `base` file, adaptive state + observer files.
- No automatic migration — this is a clean break. Operators upgrading from v0.2.x should either:
  1. Recreate the container with the new mount and let it start cold (lose 24-48h of EWMA learning), or
  2. `cp -a /var/lib/fan-controller/. /var/lib/host-agent/` before swapping mounts, to preserve baseline.

### Added — observer window persistence

- **Observer's rolling sample window now persists** to `/var/lib/host-agent/state/observer.json` (atomic write each PID cycle). On container restart / image upgrade / mode change, the observer restores its prior window so the reconciler can make drift decisions immediately — no more 2-hour warmup penalty after every restart.
- Loaded snapshot is validated: schema-version mismatch or windowSize-config change discards it and starts cold (logged).
- The HARDWARE/ENVIRONMENT learnings (sample window, inlet baseline, variance EWMA) are independent of mode. Changing `HOST_AGENT_MODE` now keeps the observer's window — only target/deadband reset to the new mode's initial values. Mode change kicks in fast: first drift decision can fire on the very next 10-min reconcile cycle.

### Fixed

- **Bug**: adaptive state path `/var/lib/host-agent/state/adaptive.json` was previously inside the ephemeral container layer (the `/var/lib/host-agent` directory wasn't bind-mounted in v0.2.x). State writes succeeded but data was lost on every restart. v0.3.0's new bind mount + path correction makes state actually persistent.

### Migration

```
# On each host running v0.2.x:
sudo systemctl stop host-agent || true
sudo cp -a /var/lib/fan-controller/. /var/lib/host-agent/   # optional, preserves EWMA + adaptive state
# update compose / Unraid template mount: /var/lib/host-agent:/var/lib/host-agent
sudo docker compose -p host-agent up -d  # or Force Update on Unraid
```

The fleet can also just start cold — 24-48h to re-learn EWMA, 2 hours for adaptive observer warmup. Operator's choice.

## [0.2.1] — 2026-05-17

### Changed

- **Image base: `debian:stable-slim` → `alpine:3`** with `gcompat` as a musl→glibc shim for the glibc-linked `nvidia-smi` that NVIDIA Container Runtime injects on GPU hosts. fan-controller and all upstream exporter binaries are Go-static and need neither libc.
- **All Go binaries UPX-compressed** (`upx --best --lzma`) — fan-controller, node_exporter, cadvisor, ipmi_exporter, smartctl_exporter, nvidia_gpu_exporter, vmagent. Each compression wrapped in `|| true` so a binary that refuses to pack falls back to the uncompressed version.
- **Image size: ~370 MB → ~170 MB** (~55% reduction). Runtime memory unchanged; ~50ms one-time decompression cost on container start.

### Verified

- nvidia-smi via gcompat returns expected output on both Tesla P4 (passive) and RTX A5500 (active) on the development host.
- All seven sub-services start cleanly under s6 supervision on Alpine.
- node-exporter `node_textfile_scrape_error=0` — adaptive metrics emit at the expected format.

### Migration

No operator action required. Drop-in replacement of the v0.2.0 container.

## [0.2.0] — 2026-05-17

Adaptive setpoint controller. Operators pick **intent** (`HOST_AGENT_MODE`), the agent picks numbers. Per-class temperature targets are no longer fixed — they're derived from hardware envelopes encoded in the agent + the operator's intent, and continuously refined based on observed equilibrium.

See [`docs/adaptive-controller-v2.md`](docs/adaptive-controller-v2.md) for the full design.

### Added

- **`HOST_AGENT_MODE` env var** — one of `max-cool`, `balanced`, `min-noise`, `eco`. When set, replaces fixed per-class temperature targets with mode-derived values from the encoded hardware envelopes. Default behavior (env unset): pure v1, no change for existing deployments.
- **Class envelopes** (`internal/envelope/`) — per-class temp ranges sourced from real research (Google HDD paper, NVIDIA passive datacenter envelope, Xeon TJunction, NAND specs). One envelope per class: CPU, PassiveGPU, HDD, SSD.
- **Adaptive observer** (`internal/adaptive/observer.go`) — rolling 2-hour window per class of (temp, fan-demand, inlet) samples with population stddev, nearest-rank percentiles (p10/p50/p90), fan-change-rate, sample discard on sensor faults, all-class reset on inlet-temp shock.
- **Adaptive reconciler** (`internal/adaptive/reconciler.go`) — every 10 min, scores mode-intent against observed distribution, drifts per-class target ±1°C per cycle toward equilibrium. Bounded by envelope (target never escapes `[PreferredLow, PreferredHigh]`). Variance-reset safety: TempStdDev > 5°C resets to mode-initial.
- **State persistence** (`internal/adaptive/state.go`) — `/var/lib/host-agent/state/adaptive.json` atomic load/save survives restarts. Mode-change resets, version-mismatch recovers cleanly.
- **Active drift wiring** (`internal/livetargets/`) — concurrency-safe handoff so reconciler decisions update the live PID setpoint without racing.
- **Mode-derived score functions** (`internal/mode/`) — formula bodies for max-cool/balanced/min-noise/eco; eco falls back to min-noise behavior until per-chassis fan-power model is added (v2.1).
- **Soak tests** — 6 scenarios proving §10 convergence + §12 safety properties with scripted thermal traces.
- **Grafana dashboard "Adaptive Controller"** — 6 panels: per-class targets table, target-vs-envelope timeseries, observed temp distribution, fan change rate, drift+reset event counters, window-fill warmup progress.
- **New textfile metrics** (in `adaptive.prom`):
  - `adaptive_mode_info{mode}`
  - `adaptive_target_celsius{class}` + `adaptive_deadband_celsius{class}`
  - `adaptive_envelope_preferred_low/preferred_high/max_safe{class}`
  - `adaptive_window_samples_filled/temp_mean/temp_stddev/temp_p10/p50/p90/fan_change_rate/inlet_mean{class}`
  - `adaptive_target_drifts_total{class,direction}` + `adaptive_target_resets_total{class,reason}`

### Changed

- Per-class env-var overrides (`CPU_TARGET=70`, `GPU_TARGET=75`, etc.) **still win** as operator intent. Profile-set values (e.g. `default.env`) are treated as v1 fallback and yield to mode-derived values when `HOST_AGENT_MODE` is set.

### Migration

**v1 deployments continue unchanged.** `HOST_AGENT_MODE` is opt-in — without it, ApplyMode is a no-op and v1 behavior is preserved.

To opt into v2 adaptive on an existing host:

```
HOST_AGENT_MODE=balanced
```

That's the entire migration. The reconciler immediately starts collecting observer data; first drift decisions land after the 2-hour window fills.

To pin a class at a fixed temp under v2 (e.g. keep an A5500-cooled chassis at 75°C on the passive GPU class):

```
HOST_AGENT_MODE=balanced
GPU_TARGET=75
```

The env var wins; adaptive applies to the other three classes.

### Notes

- Default behavior is intentionally conservative: dry-run-like (no fan changes until the controller has 2 hours of data); rate-limited (max 1°C target change per 10 min); bounded (envelope `[PreferredLow, PreferredHigh]` is a hard ceiling/floor).
- `HOST_AGENT_ADAPTIVE_DISABLED=true` exists as a kill-switch — keeps observer off entirely if you want to revert during testing.
- Active GPU classes (own-fan-driven, like RTX A5500) are intentionally **not** managed by the reconciler — they self-cool via their own blowers and use chassis-assist via the existing fan-controller logic.

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

- **Persistent URL fallback file**: `/var/lib/host-agent/config/remote_write_url`
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
