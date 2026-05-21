# Changelog

All notable changes to this project are documented here. This project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) and the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.3.6] — 2026-05-21

### Added — `apt-status` sub-service: pending-updates + reboot-required metrics

New s6 service `apt-status` that runs hourly and emits three Prometheus gauges via the existing textfile collector:

- `host_apt_updates_pending{type="all"}` — total upgradable packages on the host
- `host_apt_updates_pending{type="security"}` — security-only subset
- `host_reboot_required` — 1 if `/var/run/reboot-required` exists, 0 otherwise

**Why**: when a fleet runs with `unattended-upgrades` disabled (so patching happens on a planned cadence with a reboot, rather than at 06:00 UTC the morning a GPU-driver postinst can't unload kernel modules cleanly), operators need a Prometheus signal to drive the patch cycle. `host_apt_updates_pending{type="security"}` ticking up is the cue to plan a window; `host_reboot_required` flipping to 1 after `apt full-upgrade` is the single best post-upgrade safety check.

**How**: `chroot /host /usr/lib/update-notifier/apt-check` so the script picks up the host's `python3` + `python3-apt` + dpkg state instead of the container's Alpine rootfs. `--privileged` grants the needed `CAP_SYS_CHROOT`; nothing on the host is written, only read. Output goes to `/var/lib/host-agent/state/apt.prom` which node-exporter already serves at `:9100/metrics`.

**Self-disables** on hosts that don't ship `update-notifier` (Unraid, RHEL, Alpine, …) — the rest of the host-agent stack still runs unchanged.

### Migration

Drop-in image upgrade; no config change, no state schema change, no new volume mounts. Watchtower picks it up on the next 5-min poll. On Debian/Ubuntu hosts the new metric will appear ~1 minute after the new image starts (first sleep is `0`, then hourly thereafter). To dashboard it, add stat panels keyed off `host_apt_updates_pending{host=~"$host",type="security"}` and `host_reboot_required{host=~"$host"}` to your Server Overview.

## [0.3.5] — 2026-05-20

### Fixed — manual fan control silently lapsed on hosts with third-party PCIe cards

**Symptom**: on a Dell R730 with a non-Dell GPU installed, chassis fans drifted to 100% (~17500 RPM) at random intervals despite the controller logging modest target setpoints (e.g. `→ 32%(pg)`). Once stuck, fans stayed at full speed indefinitely until something forced manual mode back. The controller's logs gave no indication of failure — every cycle ran cleanly, recorded the computed PWM, and persisted state. The BMC was simply ignoring the SetFan commands.

**Root cause**: iDRAC's "third-party PCIe cooling response" silently re-asserts auto fan mode whenever it sees a non-Dell PCIe card whose thermal table it doesn't recognize (Tesla P4, RTX A5500, third-party HBAs, etc.). The revert happens with no SEL entry and no kernel log. Once auto mode is back, the BMC ramps fans to 100% (its default "unknown thermal load" response) and every subsequent `0x30 0x30 0x02` SetFan command from us becomes a no-op — the BMC accepts the bytes but ignores the value because manual mode is no longer engaged.

Two compounding bugs let this fail silently:

1. **`EngageManual` was called once at startup**, never re-asserted. The function's own doc comment warned "the BMC overrides our SetFan with its own thermal policy within ~30 seconds" without preceding `EngageManual` — but main.go only called it inside the startup branch.
2. **`SetFan` was gated on `r.NewSpeed != c.CurrentSpeed`**, so steady-state cycles (stable temps → unchanged target) issued zero IPMI commands. The BMC's revert-watchdog tracks the fan-PWM command specifically; a quiet hour of stable temps was enough to lapse manual control on every cycle path that didn't transition setpoint.

Net effect: the only thing keeping fans under control was either (a) a recent transient that flipped `r.NewSpeed`, or (b) a manual `ipmitool raw 0x30 0x30 0x01 0x00` someone happened to issue. Eventually the BMC won and stayed won.

**Fix**: re-assert both manual mode AND the current setpoint every cycle, not only on change.

- `internal/controller/controller.go` `Cycle()`: call `IPMI.EngageManual(ctx)` at the top of every cycle (idempotent on Dell BMCs). The existing emergency / temp-read-fail / exit-emergency paths now run after a fresh manual-mode assert, so their `SetFan(100)` calls are no longer no-ops on a reverted BMC.
- Same file: replace the conditional `if r.NewSpeed != c.CurrentSpeed { SetFan(...) }` at the bottom of the happy path with an unconditional `SetFan(c.CurrentSpeed)` every cycle. Sending the same value to the same BMC is ~10ms and keeps the revert-watchdog satisfied.
- `internal/ipmi/ipmi.go`: extend the `EngageManual` doc comment to explain the third-party PCIe cooling response and the per-cycle re-assert contract callers must honor.

The alternative (and complementary) fix is the iDRAC override flag `ipmitool raw 0x30 0xce 0x00 0x16 0x05 0x00 0x00 0x00 0x05 0x00 0x00 0x00 0x00`, which disables third-party PCIe cooling response entirely. That is a per-host one-shot operation outside the controller's scope; the in-controller fix here is universal and works without any per-host configuration.

### Migration

Drop-in image upgrade; no config change, no state schema change. Watchtower picks it up on the next 5-min poll. After the new image is running on an affected host, the controller's setpoint and the actual fan RPM should track each other again — visible in the `Server Overview` dashboard where the fan-RPM panel and the `host_agent_setpoint_percent` panel had previously diverged for hours at a time.

## [0.3.4] — 2026-05-18

### Fixed — adaptive now drifts inside the band (not just away from it)

**Symptom**: on a host running sustained GPU-inference load, the passive-GPU equilibrium temperature parked 4°C above the balanced-mode initial target (76°C vs target 72°C, band [65, 80]). Adaptive never moved the target. The PID was constantly fighting a 4°C gap, so a +11°C transient on top of equilibrium (76→87°C under a workload burst) pushed the chassis fan setpoint from a baseline ~25% to a peak ~94% — loud, abrupt, and avoidable. Dashboard data showed `adaptive_target_celsius{class="passive_gpu"}` flat-line at 72 with `last_change_direction=0` for 30+ hours despite observed `p50=76, p90=77` parked above the upper deadband edge of 75.

**Root cause**: the v0.3.2 satisficing score functions correctly stopped adaptive from drifting *out of* the preferred band (which had caused the v0.3.1 100%-fan incident). But the reconciler's three-projection synth only adjusted `TempMean ± DriftRatePerCycle`, leaving `TempStdDev` and `FanChangeRate` identical across the (now, up, down) projections. Inside the band `bandViolation` is 0 in all three projections, the variance + fan_change_rate terms cancel, all three projections score equally, `bestDelta = 0`, `Reason = settled` — forever. v0.3.2's own design note had described this as "the architectural slot for a future score-synthesizer that models PID response." This is that release.

**Fix**: the projection synth in `internal/adaptive/reconciler.go` now models the second-order effect of a target change. Raising target by Δ°C:
- raises observed mean by Δ°C (unchanged from v0.3.2),
- reduces `TempStdDev` by `0.30 × Δ` (PID engages less → less jitter),
- reduces `FanChangeRate` by `0.50 × Δ` (PID engages less → fewer fan corrections per minute).

Lowering target mirrors. Both are clamped at 0. The reliefs only need to make in-band projections distinguishable on the variance + fan-change-rate terms; they don't need to predict equilibrium precisely. The envelope clamp (`[PreferredLow, PreferredHigh]`) still bounds drift, the high-variance reset still fires above `5°C` stddev, and the strict `<` comparison in `bestScore` selection means adaptive settles cleanly once both relief terms reach 0 (PID quiescent).

This is **not** a regression of v0.3.2's protection. Replaying the v0.3.2 incident scenario (HDD at 37°C, target 38°C, low variance, PID adding fan every cycle): with v0.3.4's synth, adaptive now drifts target *up* toward equilibrium, reducing PID load — the opposite direction from the v0.3.1 broken behavior. Variance-reset and envelope-bound guarantees are unchanged. Soak tests and bounded-always tests pass unmodified.

Regression test in `internal/adaptive/reconciler_test.go` (`TestReconciler_Step_DriftUp_InBand_AbovetTarget_Balanced`) pins the exact in-band-above-target scenario with hand-calculated scores `(0.84, 0.5559, 1.1778)` for `(ScoreNow, ScoreUp, ScoreDown)`.

### Migration

Drop-in image upgrade; no config change, no state schema change. Watchtower picks it up on the next 5-min poll. After the new image is running, classes whose observed mean sits well above the current target should see `adaptive_target_drifts_total{class=X,direction="up"}` start incrementing on the 10-min reconcile cadence, with target drifting up by 1°C per cycle until the PID quiesces (typically 2-6 cycles, or 20-60 min, depending on how far target sits below equilibrium).

## [0.3.3] — 2026-05-17

### Fixed — intermittent "No data" in dashboard drive panels on busy hosts

**Symptom**: on hosts with many megaraid-attached drives, the Drives stat panel in `examples/grafana/server-overview.json` flickered to "No data" roughly half the time the dashboard was refreshed, and the Temperatures timeseries showed matching gaps in per-drive lines. Only smartctl was affected — node / cadvisor / ipmi / nvidia-gpu were rock-solid in the same period.

**Root cause**: smartctl_exporter 0.13.0 is synchronous — every cold scrape (cache expired) invokes smartctl serially against every enumerated device. On a 9-drive megaraid host the full scan measured ~28s (per-drive `smartctl -A` sequential total was 13.6s; the remaining ~15s is the exporter's `-x` extended-info collection + parsing per drive). vmagent's `scrape_timeout: 30s` was sitting right on top of that ceiling — measured `up{job=smartctl}` over 6h was 0.84 with `scrape_duration_seconds` p95 = max = exactly 30.001s. Scrapes that *could* have completed in ~30-40s got killed at 30s; every killed scrape inserts staleness markers in Prometheus for that target's series; instant-query panels reading those series during the staleness window return empty. With `scrape_interval: 60s` and ~16% scrape kill rate, the resulting "No data" window covered ~30-50% of dashboard refreshes — matching the "50/50" feel reported.

**Fix**: raise smartctl `scrape_timeout` from `30s` to `55s` in `s6/vmagent/run`. Stays under the 60s `scrape_interval` so back-to-back scrapes don't overlap, but gives 25s of headroom over the measured cold-scan cost. Cached scrapes (~20ms) are unaffected.

Hosts with few drives (e.g. typical Unraid box, single-NVMe VMs) saw no symptoms because cold scans complete well under 30s for them. The fix is universal but only heavy-drive hosts actually exercise it.

### Migration

Drop-in image upgrade; no config change. Watchtower picks it up on the next 5-min poll. After the new image is running, `up{job="smartctl"}` should track 1.0 ± occasional transient blips, and "No data" on the Drives panel should disappear.

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

[Unreleased]: https://github.com/mattjackson/host-agent/compare/v0.3.3...HEAD
[0.3.3]: https://github.com/mattjackson/host-agent/releases/tag/v0.3.3
[0.1.5]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.5
[0.1.4]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.4
[0.1.3]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.3
[0.1.2]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.2
[0.1.1]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.1
[0.1.0]: https://github.com/mattjackson/host-agent/releases/tag/v0.1.0
