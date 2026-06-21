# Changelog

All notable changes to this project are documented here. This project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) and the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.3.17] — 2026-06-20

### Changed — XC730xd-12 (primary NAS): constant fan floor for cool drives over noise

**Context**: v0.3.16's stable point (43°C) is stable but sits at the warm edge of the HDD ideal-longevity band (~30-40°C). For a primary NAS the operator prioritizes drive safety over noise, and the plant analysis showed you cannot get cool *and* quiet *and* stable on this array (dead time ≈ time constant — servoing to a cool target hunts/overshoots).

**Change**: resolve the tradeoff in favor of cool+stable by holding a high **constant chassis floor** instead of servoing — `MIN_FAN=55` in `profiles/dell_xc730xd_12.env`. From this array's measured fan→temp curve (~45%→40°C, ~55%→38°C), a 55% floor parks every drive in the ~38°C ideal band with **zero servoing — no hunt, no overshoot, rock-stable**. The per-class PID and proximity floor still stack on top for transients and the emergency trip is unchanged; this only raises the floor they can't drop below. `HDD_TARGET`/`HDD_DEADBAND` reverted to the shipped defaults (40/3) — with the floor holding ~38°C the servo rarely engages and acts only as a backstop.

Noise is intentionally traded for cooler drives. Profile-only, this chassis only; the shipped default (`MIN_FAN=10`, `HDD_TARGET=40`) is unchanged for other hosts. The 55% value is the data-derived estimate and may be tuned live to land the exact temp.

## [0.3.16] — 2026-06-20

### Changed — per-server HDD tuning for the XC730xd-12 (target the measured stable point)

**Context**: with v0.3.15's pacing, `unraid-1`'s 12-bay array stopped overshooting but converged toward the 40°C shipped default very slowly — sitting at ~45°C for ~25 min after each restart while the paced fan crawled up to the ~33% needed to hold 40. Live characterization explains why: this cage + 60mm fans give the array a **~4.5 min thermal dead time with its time constant about equal to it**, and a natural min-fan rest of ~45-46°C. On a plant where dead time ≈ time constant you can have *stable* or *tight-to-40*, not both — servoing tightly to 40 is inherently twitchy/slow.

**Change**: tune `profiles/dell_xc730xd_12.env` to the measured **stable point — `HDD_TARGET=43`, `HDD_DEADBAND=1`**. 43°C is where this array holds rock-steady at a modest steady fan (~25%) with no hunt and no slow hot transit — cooler than the ~46°C it drifted to before drift was removed, and comfortably under the 50°C emergency. The tight deadband is required for the target to bite: the band's top edge (`target+deadband=44`) must sit below the ~45°C natural rest, or the drive just parks at the band edge and the fan never engages.

This is a profile-only change — the shipped default stays `HDD_TARGET=40` for hosts whose disk plants tolerate tighter servoing. It's the first concrete per-server tune; a future auto-tune mode will measure each box's dead time / natural rest and pick this automatically.

## [0.3.15] — 2026-06-20

### Fixed — slow disk limit cycle (sample-and-hold pacing for the HDD/SSD servo)

**Symptom**: after v0.3.14 the HDD setpoint stopped sawtoothing fast but settled into a *slow* limit cycle instead — on `unraid-1`, drive temp swinging ~33↔45°C and fan ~10↔80% over a ~35-40 min period, never parking flat at the 40°C target.

**Root cause (measured live)**: a spinning disk has **~4.5 minutes of thermal dead time** — when the fan ramped 28→81%, the drive temperature didn't move *at all* for ~4.5 min. The controller steps the fan every main cycle (15-30s), i.e. ~20× faster than the plant responds, so the ramp winds the fan far past the equilibrium speed before the drive reacts (windup). It then overshoots (drive crashes 46→33), slams the fan to MinFan, and the drive slowly rewarms back through the whole deadband at 10% fan — entering the band at a fan speed too low to hold it, so it climbs out the top and the cycle repeats. Neither the old coast-to-MinFan nor v0.3.14's HOLD could fix this, because both can enter the deadband at the *wrong* fan speed; the overshoot feeding them that wrong speed was the real bug.

**Fix**: **sample-and-hold pacing** for the slow disk plants. Outside the deadband (the gentle servo ramp), the HDD/SSD PID now takes at most one step per `HDD_STEP_INTERVAL` / `SSD_STEP_INTERVAL` seconds (default **240 ≈ the measured dead time**) and holds the stepped value in between — so each step's effect registers before the next, and the fan converges to the equilibrium speed (~33% to hold 40°C on `unraid-1`) without ever overshooting. Inside the deadband it holds the current speed exactly as before (instant). **Safety is untouched**: the emergency trip short-circuits before the PID runs, and the per-class proximity floor is computed and `max()`'d in separately and is *never* paced — so approach-to-emergency response stays instant.

New per-class config (both default 240s, `0` → built-in default): `HDD_STEP_INTERVAL`, `SSD_STEP_INTERVAL`. CPU/GPU are unpaced — their plants respond in seconds, not minutes.

### Tests

- `internal/controller/controller_test.go::TestPacedStep_RampPacedAcrossDeadTime` — a held-hot drive takes one step then holds it across the whole dead-time window, then steps again only after the interval (the anti-windup regression).
- `TestPacedStep_InBandHoldsInstantly` — inside the deadband the paced step holds current speed every cycle (no pacing gate).
- `TestPacedStep_AbstainsOnNoReading` — temp≤0 abstains and clears the held candidate.

### Migration

Drop-in upgrade. Disk fans now ease to their holding speed over ~10-20 min on first convergence (one-time), then sit flat at target instead of cycling. Tune per host with `HDD_STEP_INTERVAL` (longer = gentler/slower, shorter = faster but risks overshoot on slow drives). A future one-time per-server auto-tune mode will measure each box's dead time and set this automatically.

## [0.3.14] — 2026-06-20

### Changed — target-drift is now OFF by default (the controller is a plain thermostat)

**Why**: the adaptive reconciler moved the per-class *target temperature itself* over time to trade temperature for fan noise. That behavior was designed for a passive datacenter GPU (a Tesla P4/P40 equilibrates well above any sane fixed target, so holding one pins the fans at 100% forever for no thermal benefit). Applied uniformly to every class, it let cool-running hardware ride far above its configured setpoint just to stay quiet. Observed live on `unraid-1`: in **balanced** mode (HDD anchor 38°C) the reconciler had drifted the HDD target up to the `MaxSafe-1` ceiling of **44°C**, and the hottest drive then sat at **46°C** — 8°C above the setpoint the operator believed they had selected. A setpoint should be a setpoint, not a starting hint the controller is free to abandon.

**Change**: target-drift no longer runs by default. The controller holds the mode-derived per-class target (`balanced` HDD = 38, etc.) and the PID does the work — a plain thermostat. Mode still selects *what* target you hold and *how* tolerantly you chase it (via deadband width), it just can't move the target out from under you. Re-enable the reconciler with `HOST_AGENT_ADAPTIVE_DRIFT=on` (backburnered, not deleted, pending a proper class-scoped GPU solution). The `adaptive_window_*` observation metrics keep flowing; the `adaptive_target_*`/`adaptive_mode_*` metrics go idle while drift is off.

**Trade-off**: hosts with a hot-tolerant passive GPU will run their chassis fans louder than before, because the fans now chase the GPU's configured target instead of drifting up to quiet themselves. This is intentional — "good server with a loud GPU" until the GPU case is solved on its own terms rather than at the expense of every other component.

### Fixed — fan limit cycle on tight envelopes (deadband now HOLDS instead of collapsing to MinFan)

**Symptom**: a drive sitting near its target with the chassis fan "all over the place" — e.g. `unraid-1` HDD steady at 45–46°C with the setpoint sawtoothing 10→28→49→10. Churn, not cooling, at an essentially constant temperature.

**Root cause**: inside the deadband, `control.StepPID` coasted the candidate *down toward MinFan* (`CurrentSpeed - DeadbandDriftRate`), relying on the proximity floor to act as a smooth governor underneath. That only works when the floor's ramp overlaps the operating band. For a tight envelope like HDD (emergency 50, approach window 5 → floor ramp starts at 45) the band around a 44 target (43–45) has **no floor under it**, so the fan collapsed to minimum, the drive heated until it popped out of the band, the PID slammed it back, and it oscillated.

**Fix**: inside `|temp-target| <= deadband` the PID now **HOLDS** the current fan speed instead of coasting. Because this is an incremental controller (`cand = CurrentSpeed + step`), holding parks the fan at the equilibrium-maintaining speed. The result is a clean three-zone thermostat: ramp up above the band, hold within it (tolerate a little over-target without screaming), step down below it (a cool/idle host still eases to MinFan). The proximity floor remains a pure safety backstop near the emergency trip. Deadband *width* stays the per-mode tolerance knob (max-cool 2° … eco 5°).

### Tests

- `internal/control/control_test.go::TestStepPID_DeadbandHolds` — within ±deadband (either side) the candidate holds the current speed; below the band it steps down via P+D.
- `TestStepPID_SymmetricDeadband_HoldsAboveTargetInBand` — +1°C over target inside the band holds; +4°C (outside) re-engages the ramp.
- `TestStepPID_NoHuntAroundTarget` — temp jittering ±1°C in-band holds the candidate dead steady (no ramp, no collapse) — the anti-limit-cycle regression.
- `cmd/fan-controller` e2e golden updated: the only behavioral delta is the in-band CPU candidate (25→28, hold vs coast); the bound setpoint is unchanged.

### Migration

Drop-in upgrade. On first start the controller holds your mode's targets instead of any previously-drifted values; a stale persisted reconciler state file is ignored while drift is off. Expect quieter, steadier fans on disk/CPU hosts and louder fans on passive-GPU hosts. To restore the prior adaptive behavior, set `HOST_AGENT_ADAPTIVE_DRIFT=on`.

## [0.3.13] — 2026-06-16

### Fixed — ~10-min fan-hunt window after every restart (seed live targets from persisted state)

**Symptom**: immediately after a restart/redeploy, the fan would hunt for ~one adaptive cycle (~10 min) even on a host the reconciler had already tuned, then settle. Observed live after the v0.3.12 deploy: the P4 sat at 88°C with the fan sawtoothing 69↔86 for ~5 min, then snapped flat at 74% the moment the first reconcile ran.

**Root cause**: `liveTargets` (the reconciler→PID hand-off) starts empty and is only populated when the reconciler's `Step()` runs — the first time being ADAPTIVE_CYCLE_MINUTES after startup. Until then `liveTargets.ApplyTo(cfg)` is a no-op, so the PID runs on the **mode-initial** target/deadband from `ApplyMode` (e.g. PassiveGPU 83/4) instead of the **persisted, learned** values the reconciler loaded from disk (e.g. 85/7). The narrower initial deadband puts the card's load equilibrium on the band edge → the symmetric-deadband loop hunts until the learned wider band is applied.

**Fix**: at startup, seed `liveTargets` directly from the reconciler's persisted per-class state (`seedLiveTargets` in `cmd/fan-controller`), so the PID uses the learned target/deadband from its very first cycle. Operator-overridden classes are skipped, mirroring the reconcile loop's `Skipped` path so a pin isn't clobbered. No-op when adaptive is disabled.

This closes the last "needs babysitting" gap: the controller now resumes its learned equilibrium instantly across restarts/redeploys instead of relearning for 10 minutes each time.

### Tests

- `cmd/fan-controller/seed_test.go::TestSeedLiveTargets_AppliesLearnedValuesAtStartup` — persisted 85/7 reaches the PID config via seed+ApplyTo (not the mode-initial 83/4).
- `TestSeedLiveTargets_SkipsOverriddenClass` — an operator-pinned class is not seeded (override preserved).
- `TestSeedLiveTargets_NilReconciler` — adaptive-disabled path is a safe no-op.

### Migration

Drop-in upgrade. After this, a redeploy no longer triggers a ~10-min hunt; the fan picks up the learned, settled curve immediately.

## [0.3.12] — 2026-06-16

### Fixed — fan hunting at steady temperature (symmetric PID deadband)

**Symptom**: under steady GPU load the chassis fan sawtoothed ~85↔95% while the card temperature held essentially flat (e.g. P4 steady at 84–85°C). The fan was visibly "all over the place" at a constant temperature — churn, not cooling.

**Root cause**: the per-class PID deadband in `control.StepPID` was **asymmetric** — it coasted the fan toward MinFan only when temp was *at or below* target, but the instant temp was even 1°C *above* target it jumped to the `error*FanGain + Δtemp*DerivativeGain` ramp branch. Because a loaded card's equilibrium sits right around its target, temp jittered across the target each cycle: +1°C → ramp up, back to target → coast down by `DeadbandDriftRate`, repeat. That's a limit cycle. The adaptive reconciler had already widened the deadband to its max trying to damp the variance, but the asymmetric logic ignored the deadband on the high side, so the learned damping couldn't bite. The derivative term amplified each temp wiggle.

**Fix**: make the deadband **symmetric** — coast within `|temp-target| <= deadband` on *both* sides (one-line condition change in `control.StepPID`). Inside the band the PID candidate now falls back toward MinFan, which lets the caller's **proximity floor** govern: `ProximityFloor` is a smooth linear ramp (MinFan at `emergency-window` → MaxFan at `emergency`) that the controller already `max()`'s into every setpoint. So in the operating band the fan now follows that smooth, monotonic, single-equilibrium curve instead of the PID's bang-bang hunt. The high side stays safe — the proximity floor ramps hard as temp approaches emergency and the emergency trip forces MaxFan above it; the PID's P+D ramp only re-engages once temp exceeds `target+deadband`.

This is a self-tuning improvement, not a hand-tuned knob: the adaptive layer keeps learning `target`/`deadband` (widening the band absorbs bursty load automatically), and the fast loop now honors that band in both directions. No gains were changed; no per-host tuning added.

**Effect**: in the operating band the fan tracks the smooth proximity-floor curve (for the P4: ~MinFan at 83°C rising to ~74% at 88°C, 100% at 90°C) rather than sawtoothing. Idle/emergency behavior unchanged.

### Tests

- `internal/control/control_test.go::TestStepPID_SymmetricDeadband_CoastsAboveTargetInBand` — above-target-but-in-band now coasts down (was: ramped); just outside the band the P+D ramp still re-engages.
- `internal/control/control_test.go::TestStepPID_NoHuntAroundTarget` — feeds the docker-1 P4 jitter pattern (temp oscillating ±1–2°C around target, all in-band) and asserts the candidate monotonically coasts down — never ramps up on an in-band sample. Would fail under the old asymmetric logic.
- Existing saturation-escape / emergency / idle / clamp tests unchanged and passing; e2e golden regenerated.

### Migration

Drop-in image upgrade; Watchtower pulls on the next 5-min poll. Hosts will see the fan settle onto the smooth proximity-floor curve in the operating band instead of hunting; cards may sit a couple °C warmer in exchange (still governed by the same emergency backstop). No config changes.

## [0.3.11] — 2026-06-16

### Changed — raise the `passive_gpu` thermal envelope (quieter chassis fans for hot-tolerant datacenter cards)

**Why**: on docker-1 the binding `passive_gpu` is a Tesla P4 — a 75W passive datacenter card spec'd to run hot (hardware thermal slowdown ~91°C, shutdown ~95°C). The previous envelope (`PreferredHigh=80`, `MaxSafe=85`) made min-noise hold it at ~82–85°C, which *forced* chassis fans to 87–99% under load for no thermal benefit. The card's `hw_thermal_slowdown`/`sw_thermal_slowdown` counters were `0` across all observed load — i.e. 6–10°C of unused headroom.

**Change**: `passive_gpu` envelope raised to let the card settle ~84–85°C with quieter fans, keeping a ~6°C throttle margin:

| field | old | new |
|-------|-----|-----|
| PreferredLow  | 65 | 75 |
| PreferredMid  | 72 | 80 |
| PreferredHigh | 80 | 83 |
| MaxSafe       | 85 | 86 (adaptive drift ceiling = MaxSafe-1 = **85**) |
| Emergency     | 90 | 90 (unchanged — ~1°C below the hardware slowdown point, hard backstop) |

In min-noise the adaptive target now drifts toward 83 and the saturation-relief ceiling is 85, so under sustained load the card holds ~84–85°C with materially lower fan demand instead of being pinned near 80°C. Idle behavior is unchanged (fans drop when the card is cool). No controller-logic change — this is purely the per-class envelope. Emergency-first ordering is unaffected, and `config.Validate` still passes for all modes (`target < emergency`).

This is a conservative first step (the card tolerates ~88°C); the ceiling can be raised further if more fan-noise reduction is wanted.

### Tests

Updated the envelope-dependent fixtures (exact-values, mode `InitialTarget`, the min-noise/max-cool score-formula bands, mode-derived GPU target integration tests, and the in-band drift / fan-headroom reconciler cases) to the new `passive_gpu` band.

### Migration

Drop-in image upgrade; Watchtower picks it up on the next 5-min poll. Hosts with a passive GPU will see its adaptive target drift up toward the new ceiling over the following reconcile cycles, easing chassis fans; the card will stabilize a few °C warmer (still well within spec). Other classes (CPU/HDD/SSD) and active GPUs are unaffected.

## [0.3.10] — 2026-06-15

Hardening release from a full multi-pass code audit (fix → re-audit → repeat until clean) plus a large test-coverage expansion. No change to the v0.3.9 fan-control behavior; one real latent bug fixed (config validation), several observability/correctness gaps closed, and the audit caught three regressions introduced mid-pass (all fixed before release).

### Fixed

- **Config validation (the one genuinely-impactful latent bug).** `bindInt`/`bindFloat` silently swallowed parse errors, so a malformed numeric (`FAN_GAIN=`, `MIN_FAN=abc`) degraded to `0` — disabling proportional gain or allowing 0% fans — with no operator signal. Parse failures now log a WARN, and a new exported `config.Validate(cfg)` asserts safety invariants (`0 < MIN_FAN ≤ MAX_FAN ≤ 100`, `INTERVAL > 0`, `FAN_GAIN > 0`, `0 < ADAPT_ALPHA < 1`, per-class `target < emergency`). `main.go` calls it after `ApplyMode` and exits — failing closed to iDRAC automatic — on violation. `Validate` is intentionally separate from `Load` so partially-resolved/mode-only configs can still be inspected.
- **`adaptive_target_drifts_total` dropped `bounded_high`/`bounded_low`.** The reconciler accumulated these direction counters but `RenderReconcilerMetrics` iterated a hardcoded `["up","down"]` slice, so sustained bound-pressure (a key envelope-misconfiguration signal) never reached Prometheus. The renderer now emits all four directions; a test asserts the rendered output, not just the in-memory map.
- **smartctl SATA temperature: a bad attribute 190 suppressed a valid 194.** After v0.3.9 began accepting attr 190 (`Airflow_Temperature_Cel`) alongside 194, an unparseable/zero 190 row hit a `break` and abandoned the scan before reaching a valid 194 row (drive temp would abstain). Changed to `continue`.
- **smartctl standby detection matched substrings.** `standbyRE` (`STANDBY|SLEEP`) would classify an error message containing "asleep"/"sleeping" as standby. Now uses word boundaries (`\bSTANDBY\b|\bSLEEP\b`) so a missing/failed drive isn't mistagged as merely sleeping.

### Changed

- **`fan_controller_cycle_duration_seconds` is now a float (`%.3f`).** Previously an integer that rounded any sub-second cycle to `0`. The e2e golden test now scrubs this inherently-non-deterministic (real wall-clock) line, removing latent flakiness the integer form was masking.
- **`fan_controller_samples_total` HELP text** clarified: it counts successful non-emergency PID cycles (does not advance during sustained emergencies or sensor-read failures). Type remains `counter` (verified persisted + monotonic).
- Prometheus label values are now escaped (`escapeLabelValue`: `\`, `"`, newline) on the dynamic `source` label.
- `RenderObserverMetrics` computes each class's window stats once per render (was 9× per class — 36 lock+recompute cycles → 4); insertion sort replaced with `sort.Float64s`.
- Shutdown path now logs previously-discarded `PersistState`/`HandbackAuto` errors; adaptive-loop ticker is stopped on exit; duplicate active-profile log line removed.

### Internal / safety

- Removed an unreachable `DriftReasonVarianceReset` case that was a future double-count hazard; guarded the `LastCPUTemp` D-term update for symmetry with the optional classes; documented the `r.mu → o.mu` lock order and the emergency-first ordering that makes a deadband top crossing `Emergency` inert.
- **Corrected a too-strict validation invariant before it shipped.** An interim `target+deadband ≥ emergency` check would have rejected `HOST_AGENT_MODE=eco` at startup (CPU 75+5 vs `CPU_EMERGENCY=80`; SSD 60+5 vs 65). The safe invariant is `target < emergency` — the deadband top crossing emergency is inert because the controller evaluates the emergency threshold before the PID/deadband every cycle. A regression test now asserts all four modes boot on the default profile.

### Tests

Large coverage expansion: envelope 17%→100%, controller 73%→94%, adaptive→95%, plus config validation, metrics label-escaping, and sensors parse-path tests. New regression tests pin each fix above (notably: transient-dip non-drift-down already in v0.3.9, bounded-counter render, attr-190→194 fallthrough, `standbyRE` word boundaries, eco-mode validation, post-emergency D-term settling, observer persistence error paths, `percentileIndex` clamps).

### Migration

Drop-in image upgrade; Watchtower picks it up on the next 5-min poll. One behavior change to be aware of: a host with a genuinely-unsafe config (e.g. `MIN_FAN=0`, `target ≥ emergency`, malformed numerics) will now refuse to start and fall back to iDRAC automatic fan control rather than running with bad settings. All shipped profiles and all four modes pass validation on the default profile.

## [0.3.9] — 2026-06-15

### Fixed — transient fan dip drove a downward-drift limit cycle (fans pinned ~97–100% for no thermal reason)

**Symptom**: docker-1's PassiveGPU (P4) sat at 80–81°C — comfortably in-band, 10°C below the 90°C emergency — yet `fan_setpoint_percent` was pinned at 97–100% and `adaptive_target_celsius{class="passive_gpu"}` was decaying monotonically downward (84 → 73 over ~1h, **11 down-drifts and 0 up-drifts in 2h**), with no envelope or operator change. Earlier in the day the same class held a healthy equilibrium at target 84. The decay reliably started right after a brief fan dip and would reverse (target rockets back up) once the dip aged out of the observation window — a limit cycle whose period tracks the window length (~104 min).

**Root cause**: the `saturationPenalty` term — the signal that pushes target *up* to relieve a pinned fan — keyed off `WindowStats.FanDemandMean`, the arithmetic mean of fan demand over the rolling window (`internal/adaptive/observer.go`). A single transient dip (a brief load/temp drop sends the fan low for part of the window) drags that mean below the 90% saturation knee even while the fan is pinned at MaxFan for the *majority* of the window. With the mean below 90, `saturationPenalty` evaluates to 0, so `scoreMinNoise` collapses to its `5·aboveHigh` term. Because the card's true equilibrium under load (`adaptive_window_temp_p50`=81) sits ~1°C above `PreferredHigh` (80), the down-projection (`mean→80`) always scores best, and the reconciler drifts the target down 1°C/cycle — chasing a target the saturated PID can never reach, which pins the fan. The decay continues toward `PreferredLow` until the dip ages out of the window, `FanDemandMean` climbs back above 90 (the window p90 was 100 the entire time), the penalty snaps back, and target shoots up again. v0.3.7 introduced the penalty but used a contamination-prone statistic; this is a regression of the same saturation-blindness class.

**Fix**: the saturation penalty now keys off `WindowStats.FanDemandP90` — the 90th-percentile fan demand over the window (nearest-rank, same convention as the temp percentiles) — instead of the mean. A fan pinned at MaxFan for ≥10% of the window reports p90=100 regardless of any minority dip, so the penalty reflects whether the fan is *actually* pinned. The reconciler's up/down synth projections relieve `FanDemandP90` on the same first-order model they used for `FanDemandMean`. `FanDemandMean` is still computed (and is the only score input that changed); the high/low clamps from v0.3.8 are unchanged. Verified against the captured docker-1 window (`p50=81, p90=100, mean=74.5`): pre-fix the down-projection wins (`ScoreDown=4.16 < ScoreNow=8.11`, drift_down 80→79); post-fix the up-projection wins and the target drifts up to relieve the fan.

**Why p90, not a down-drift guard**: blocking down-drift whenever the fan is currently saturated would also block *legitimate* down-drift in the case where the fan has genuine cooling headroom (p90 < 90) and temp sits above `PreferredHigh` — there, demanding a colder target is achievable and correct. Keying the penalty off p90 distinguishes the two cases with the existing scoring machinery and needs no special-case branch.

### Added

- `adaptive_window_fan_demand_p90{class}` gauge (node-exporter textfile / `:9100`) — the saturation signal the reconciler scores on. Fan demand previously had **zero** dashboard visibility, which is part of why this limit cycle was hard to spot. A high p90 with a low `fan_change_rate` is a pinned fan.

### Tests

- `internal/adaptive/reconciler_test.go::TestReconciler_Step_TransientFanDip_DoesNotDriftDown` — reproduces the docker-1 window (temp 81°C, 6/10 samples pinned at 100, 4/10 dipped to 24 → mean 70, p90 100). Asserts the target does **not** drift down. Confirmed to fail under the old mean-based penalty (`drift_down 80→79`) and pass under the fix.
- `internal/adaptive/reconciler_test.go::TestReconciler_Step_DownDriftStillWorks_WithFanHeadroom` — anti-regression: with the fan cruising at 55% and temp above `PreferredHigh`, the target still drifts down. Guards against over-suppression.
- `internal/mode/mode_test.go::TestScore_SaturationPenalty_KeysOffP90NotMean` — two windows with identical p90 but means 30 points apart score identically, across all four modes.
- `internal/mode/mode_test.go::TestScore_MinNoise_DownDriftAllowedWithHeadroom` — mode-level companion to the headroom test.
- `internal/adaptive/observer_test.go::TestObserver_Stats_FanDemandP90_NearestRank` / `TestObserver_Stats_FanDemandP90_IgnoresTransientDip` — p90 computation and dip-robustness.
- `TestScore_SaturationDrivesTargetUp_AllModes` and the metrics-render count tests updated for the new field/series.

### Migration

Drop-in image upgrade; Watchtower picks it up on the next 5-min poll. Hosts currently caught in the down-drift cycle will see `adaptive_target_celsius` stop decaying and resume drifting up at 1°C per 10-min cycle until fan demand falls below the saturation knee. Hosts not saturating see no change.

## [0.3.8] — 2026-05-28

### Fixed — v0.3.7's saturation penalty couldn't actually move target (still ~90–100% fan after deploy)

**Symptom**: after v0.3.7 shipped, docker-1 P4 dropped off the hard 100% pin but settled at ~90–99% fan (audible saturation oscillation) instead of relieving — same chassis, same load profile, same `PassiveGPU` class. `adaptive_target_celsius{class="passive_gpu"}` reached 80 (PreferredHigh) and stayed there; `fan_setpoint_percent` oscillated 88–99 with binding source pinned at `pg`. Reconciler logged `bounded_high` every 10 minutes — not `settled`, not `drift_up`. The saturation-relief mechanism v0.3.7 added was firing every cycle, scoring up-projection strictly better than now-projection, and being silently clipped to a no-op.

**Root cause**: the high clamp in `internal/adaptive/reconciler.go:reconcileClass` was `env.PreferredHigh`, not `env.MaxSafe`. v0.3.7 added the saturation penalty + matching `FanDemandMean` projection so the score *gradient* pulls target up under saturation — but the clamp at `PreferredHigh` meant adaptive could never use that gradient past the operator-preferred ceiling. PassiveGPU's envelope has PreferredHigh=80 and MaxSafe=85 — 5°C of intentional safety headroom that adaptive was locked out of, even when fans were pinned at MaxFan defending PreferredHigh. The clamp predates v0.3.7 and was originally added (v0.3.2) to prevent a separate runaway-drift incident; before v0.3.7 the score had no "anti-saturation" term to keep target in-band when fans weren't pinned, so a hard `PreferredHigh` cap was the only thing stopping unbounded drift. Post-v0.3.7 the score is self-regulating: `bandViolation` pulls target down toward `PreferredHigh` when fans aren't saturating, `saturationPenalty` pushes up when they are. The clamp was load-bearing in v0.3.6 and obsolete in v0.3.7 — and removing it is what v0.3.7 was supposed to do.

**Fix**: change the high clamp from `env.PreferredHigh` to `env.MaxSafe - 1` (`reconciler.go:449`). The -1 keeps a 1°C buffer below `MaxSafe` so PID deadband can't push *observed* temp past MaxSafe even when target is at the ceiling. Low clamp at `PreferredLow` is unchanged — there's no symmetric "anti-cold" signal pulling target into the cold zone, and a too-low target just makes the PID fight harder without a feedback loop to escape via.

Replaying the docker-1 P4 saturation case against the new clamp: at `target=80, mean=80.8, fan=99`, `scoreBalanced` is `0.8 + 5·81 = 405.8`; the up-projection (`mean=81.8, fan=94`) scores `1.8 + 5·16 = 81.8` — strict-better by 324, drift_up wins, target → 81 on the next cycle. From `target=81` the chain continues until `FanDemandMean` falls below the 90 knee and the saturation term collapses to 0, at which point `bandViolation` (which uses `env.PreferredHigh`, not target) takes over and pulls target back down. Equilibrium for the docker-1 load profile lands at ~82°C with fan ~80–85% — well inside `[PreferredHigh, MaxSafe-1]`, never reaching the ceiling.

**Bounded contract change**: pre-v0.3.8 adaptive target was bounded by `[PreferredLow, PreferredHigh]`. Post-v0.3.8 it's bounded by `[PreferredLow, MaxSafe-1]`. Per-class:

| Class       | PreferredLow | PreferredHigh | MaxSafe-1 (new ceiling) |
|-------------|--------------|---------------|-------------------------|
| CPU         | 55           | 75            | 84                      |
| PassiveGPU  | 65           | 80            | 84                      |
| HDD         | 32           | 43            | 44                      |
| SSD         | 45           | 60            | 69                      |

In MinNoise mode the saturation-penalty weight is 10× vs Balanced's 5×, so MinNoise drifts toward the ceiling much more aggressively under any sustained fan saturation — operators who want fans down even at the cost of running each class in its upper safety headroom should pair this release with `HOST_AGENT_MODE=min-noise`.

Regression tests:
- `internal/adaptive/reconciler_test.go::TestReconciler_Step_DriftsAbovePreferredHigh_UnderSaturation` — pins the v0.3.7 deploy pathology directly (PassiveGPU, target=80, mean=80.8, fan=99 → drift_up to 81). Pre-v0.3.8 this test fails with `bounded_high` and `NewTarget=80`.
- `internal/adaptive/reconciler_test.go::TestReconciler_Step_BoundedAtMaxSafe` — verifies the new ceiling: pre-set target at MaxSafe-1, push it with saturation, target stays at MaxSafe-1 with reason `bounded_high` or `settled` (never escapes).
- Existing soak/bounded tests (`TestSoak_BoundedAlways_NeverEscapesPreferredRange`, etc.) updated to assert the new `[PreferredLow, MaxSafe-1]` contract.

### Migration

Drop-in image upgrade. Watchtower picks it up on the next 5-min poll. Hosts currently saturating after v0.3.7 will see `adaptive_target_celsius` resume drifting up at 1°C per 10-min cycle until fan demand falls below the saturation knee — typically 2–3 cycles past `PreferredHigh` for a sustained-load host. Hosts not saturating see no change (the new clamp only matters when the score's saturation term is non-zero, and `bandViolation` keeps target in-band the rest of the time).

## [0.3.7] — 2026-05-27

### Fixed — fans stuck at 100% under sustained GPU load even after temp settled in-band

**Symptom**: on a Dell R730xd with a Tesla P4, any GPU utilization spike to ≥95% drove chassis fans to 100% within ~30s — expected — but fans then *stayed* pinned at 100% for the entire load duration even after die temp stabilized at 76–79°C, well inside the envelope's preferred band (passive_GPU PreferredHigh=80) and 6–9°C below MaxSafe. Captured via 1-min trace on docker-1 (2026-05-27 13:30–15:07): fan setpoint hit 100% at 13:32 with temp=84°C, dropped to and held 76–79°C from 13:36 onward, and fan stayed at 100% for the next 90+ minutes. `adaptive_target_celsius{class="passive_gpu"}` crept from 72→74 over the same window — the reconciler was nominally trying to do the right thing but at less than 1°C per 10-min cycle.

**Root cause**: three independent blind spots compounded into a stuck-at-MaxFan failure mode.

1. **`internal/control/control.go:StepPID` had no downward path when `error > 0`.** The asymmetric deadband branch only fires for `error <= 0`. Above target, the function always computes `cand = current_speed + positive_step` and returns `clamp(cand, MinFan, MaxFan)`. When `current_speed` already equals `MaxFan`, every cycle's positive step is a no-op clamp; the controller has no mechanism to probe "would less fan also hold this equilibrium?" — even when the real answer is yes.

2. **All four mode score functions in `internal/mode/mode.go` ignored `WindowStats.FanDemandMean`.** A fan saturated at 100% for the whole observation window scores near-zero on `TempStdDev` (output pinned → no jitter) and `FanChangeRate` (no changes to count). With `TempMean` inside `[PreferredLow, PreferredHigh]`, `scoreBalanced` returned `0 + 0.3·variance + 0.3·fanChange` — effectively zero — and the reconciler logged "settled." Saturation, the single most important failure mode the adaptive layer should self-correct, was structurally invisible.

3. **The reconciler's three-projection synth didn't project `FanDemandMean`.** `reconcileClass` synthesizes `statsUp`/`statsDown` by shifting `TempMean ± drift`, with `TempStdDev` and `FanChangeRate` relieved by per-degree empirical constants (v0.3.4). Adding a saturation term to the score would have been a no-op without a matching projection — `FanDemandMean` would be identical across now/up/down, the saturation penalty would cancel, drift gradient would still be zero.

**Fix**: three targeted, mutually-reinforcing changes.

- `internal/control/control.go` `StepPID`: new saturation-escape branch — when `error > 0 && CurrentSpeed >= MaxFan && dTemp <= 0`, drift the candidate down by `DeadbandDriftRate` instead of computing a clamped positive step. Self-correcting: if load is genuinely near max cooling capacity, the next cycle's P+D step pushes back up (`dTemp` goes positive as fan eases). The non-rising gate (`dTemp <= 0`) keeps real climbing transients from triggering the escape during legitimate ramping. Worst-case behavior is small oscillation near MaxFan; best case is convergence to the minimum fan that holds the load's equilibrium, with adaptive then drifting the target up to that equilibrium so error closes and normal deadband logic takes over.
- `internal/mode/mode.go`: new `saturationPenalty(fanDemandMean)` — quadratic above 90% (0 at ≤90, 25 at 95, 100 at 100). Added to all four score functions with mode-appropriate weights: `max-cool=1.0` (tolerates saturation by intent), `balanced=5.0` (saturation is anti-balanced), `min-noise=10.0` and `eco=10.0` (saturation is the literal opposite of intent).
- `internal/adaptive/reconciler.go`: new `fanDemandReliefPerC = 5.0` constant, with `statsUp.FanDemandMean = max(0, mean − 5·drift)` and `statsDown.FanDemandMean = min(100, mean + 5·drift)` in the synth. This makes the new saturation term distinguishable across projections; under saturation, the up-projection scores strictly lower and the reconciler drifts target toward `PreferredHigh` (or wherever fan demand falls below the saturation knee), rather than logging "settled" forever.

Replaying the docker-1 trace against the new score functions: at the saturated state (`mean=78, stddev=1.0, fanChange=0.5, fanDemand=98`), `scoreBalanced` is `5·64 = 320` instead of `~0.5`; the up-projection (`mean=79, stddev=0.7, fanChange=0, fanDemand=93`) scores `5·9 = 45`. Strict-better, so the reconciler drifts target up. After one drift to 75, observed fan demand falls toward the 90 knee on the next window; after two more drifts (target=77) the saturation term collapses to 0 and normal in-band scoring resumes — typically 20–30 min to escape saturation vs. the prior 60+ min plus permanent oscillation.

**Envelope intentionally unchanged**: `PassiveGPU.PreferredMid=72` is correct for cards at light or burst load (most fleet time); the bug only surfaces under sustained heavy load where the v2 adaptive layer is supposed to drift up on its own. The right answer is to fix the saturation blindness in the adaptive layer (this release) rather than redefine "balanced" for every host based on a worst-case load assumption.

Regression tests:
- `internal/control/control_test.go::TestStepPID_SaturationEscape` — pins the docker-1 pathology directly (target=72, temp=78, fan=100, dTemp=0 → escape to 97) plus the three non-firing branches (rising temp, below-MaxFan, etc.) so the fix can't silently regress to monotonic-up-or-clamp.
- `internal/mode/mode_test.go::TestSaturationPenalty_QuadraticAbove90` — pins the penalty curve at five points.
- `internal/mode/mode_test.go::TestScore_SaturationDrivesTargetUp_AllModes` — for `balanced`/`min-noise`/`eco`, under saturated-in-band stats the up-projection scores strictly lower than holding. `max-cool` is intentionally excluded — its weight is small enough that variance can dominate in pathological inputs, which matches the intent.

### Migration

Drop-in image upgrade; no config change, no state schema change, no new env vars. Watchtower picks it up on the next 5-min poll. After the new image is running on a host that's currently saturating, expect `adaptive_target_celsius` to drift up by 1°C per 10-min reconcile cycle until either `FanDemandMean` falls below ~93 (penalty knee) or target reaches `PreferredHigh`. Concurrent with that, the control layer will start oscillating fan by ~3% near `MaxFan` (visible in the `host_agent_setpoint_percent` panel) until adaptive catches up and error closes — that oscillation is the saturation escape working as intended and disappears once normal deadband logic re-engages.

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

[Unreleased]: https://github.com/MattJackson/host-agent/compare/v0.3.7...HEAD
[0.3.7]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.7
[0.3.6]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.6
[0.3.5]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.5
[0.3.4]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.4
[0.3.3]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.3
[0.3.2]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.2
[0.3.1]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.1
[0.3.0]: https://github.com/MattJackson/host-agent/releases/tag/v0.3.0
[0.2.1]: https://github.com/MattJackson/host-agent/releases/tag/v0.2.1
[0.2.0]: https://github.com/MattJackson/host-agent/releases/tag/v0.2.0
[0.1.5]: https://github.com/MattJackson/host-agent/releases/tag/v0.1.5
[0.1.4]: https://github.com/MattJackson/host-agent/releases/tag/v0.1.4
[0.1.3]: https://github.com/MattJackson/host-agent/releases/tag/v0.1.3
[0.1.2]: https://github.com/MattJackson/host-agent/releases/tag/v0.1.2
[0.1.1]: https://github.com/MattJackson/host-agent/releases/tag/v0.1.1
[0.1.0]: https://github.com/MattJackson/host-agent/releases/tag/v0.1.0
