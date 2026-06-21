# host-agent v3 — One Curve: Unified Proportional Fan Control

| | |
|---|---|
| **Status** | Draft — design only, not implemented |
| **Target version** | host-agent v0.4.0 (control-law change; semver minor) |
| **Predecessor** | v0.3.x line (PID + adaptive setpoint + per-class workarounds) |
| **Audience** | Future contributors + fleet operators |
| **Last updated** | 2026-06-21 |

---

## 1. Summary

Replace the per-class PID + adaptive-setpoint controller with **one control law for every thermal class: a lenient, memoryless, proportional temperature→fan curve.** Each class differs only by a small **envelope** (a table of temperatures), never by control logic. The chassis fan is `max()` of every class's curve output, plus the unconditional emergency trip.

The guiding principle, in the operator's words: **"heat is heat, fans are fans — same logic for all."** The per-class part is *numbers* (what temperature is comfortable for a drive vs. a CPU vs. a hot-tolerant GPU), not *behavior*.

This **deletes** the integral controller and everything bolted on to tame it: the PID gain/derivative/deadband, the adaptive reconciler and target-drift, the sample-and-hold pacing, and the per-server constant-fan-floor hack. What remains is ~one function and a per-class envelope table.

---

## 2. Motivation — how we got here

Over the v0.3.9 → v0.3.17 line we chased a single recurring symptom — **fans hunting / overshooting / running hot** — through a sequence of fixes, each of which worked around the previous one:

| Version | Change | What it revealed |
|---|---|---|
| 0.3.9–0.3.13 | adaptive drift fixes (p90 saturation, seeding) | drift kept fighting the operator's intent |
| 0.3.14 | killed target-drift; deadband HOLDs instead of collapsing to MinFan | stopped the fast sawtooth; exposed a *slow* limit cycle |
| 0.3.15 | sample-and-hold pacing for the slow disk plant | killed the overshoot; revealed a tight target is unreachable cleanly |
| 0.3.16 | per-server stable-point target (43°C) | stable but warm; needed a tight deadband to "bite" |
| 0.3.17 | per-server constant fan floor (MIN_FAN=55) | **wrong** — deaf to temperature, abandons the controller |

**The root cause of the whole chase: the controller integrates.** v0.3.x is an incremental P+D controller (`fan += error·gain + Δtemp·gain`) with an adaptive setpoint on top. Integral-style control accumulates, and:

- On a **slow / high-dead-time plant** (a spinning disk: measured ~4.5 min dead time, time-constant about equal) it **winds up** — it ramps the fan far past the holding speed before the temperature responds, overshoots, overcools, and limit-cycles. Pacing was the band-aid.
- On a **hot-tolerant plant** (a passive Tesla that's fine at 85°C) it **pins at 100%** chasing a target the card never reaches. Adaptive drift was the band-aid.

Every per-class workaround we added existed to make the integrator behave on a plant it was mismatched to. **The integrator is the source of the per-class complexity** — remove it and the per-class pressure disappears.

A memoryless proportional law has neither failure mode: it cannot wind up (no accumulator), so plant speed and dead time stop mattering for *stability* — a fast plant settles fast, a slow plant settles slowly, both stable, with identical code.

---

## 3. The one law

For each thermal class with a current temperature `t`:

```
fan(t) = clamp(
  MIN_FAN + (t - comfort) / (emergency - comfort) * (MAX_FAN - MIN_FAN),
  MIN_FAN, MAX_FAN
)
```

- At `t <= comfort`  → `MIN_FAN` (class is happy; contribute the floor only).
- At `t >= emergency` → `MAX_FAN` (and the hard emergency trip fires; see §6).
- In between → a straight, gentle, **memoryless** ramp. The fan is a pure function of *current* temperature — no history, no accumulator, no setpoint to drift.

The chassis command each cycle is:

```
setpoint = max over all present classes of fan_class(t_class)      // plus active-GPU assist (§6)
if any class t >= its emergency:  setpoint = MAX_FAN               // unconditional safety trip
```

This is exactly the existing **proximity floor** mechanism — promoted from "near-emergency backstop" to "the entire controller," with the ramp widened to span the whole operating range instead of just the last few degrees.

### Why proportional is the right call (and its honest cost)

- **Stable on every plant, same code.** Memoryless ⇒ no windup ⇒ no dead-time limit cycle. The single biggest lesson of the v0.3.x line.
- **Genuinely temperature-driven.** Cold class → quiet; hot class → loud. This is what the constant-fan-floor (v0.3.17) threw away and why it was wrong.
- **Lenient by construction.** A proportional controller settles with a **steady-state offset** — it holds *near* a comfortable band, not pinned to an exact target. That offset *is* the leniency: it never fights to the last degree, so it never hunts. You can have *simple + universal + stable* or *tight-to-the-degree*, not both; for fans the former is obviously correct.
- **One slope to reason about.** Stability of a proportional controller on a first-order-plus-dead-time plant depends on loop gain (`curve slope × plant gain`). Keep the slope gentle (wide `comfort→emergency` span) and every plant is comfortably stable. This is a single, analyzable knob, not an emergent interaction of five.

---

## 4. The envelope — the only per-class state

The *only* thing that differs per class is a two-number envelope:

| Class | `comfort` (ramp start, fan = MIN_FAN) | `emergency` (fan = MAX_FAN + hard trip) | Notes |
|---|---|---|---|
| cpu | ~50°C | 90°C | fast plant; settles quickly |
| passive_gpu | ~70°C | 90°C | hot-tolerant; high comfort ⇒ stays quiet until genuinely warm |
| active_gpu | (own-fan assist, see §6) | 88°C | chassis assists only when the card's own fan saturates |
| hdd | ~33°C | 50°C | slow plant; gentle slope ⇒ stable; settles ~38-40°C |
| ssd | ~45°C | 65°C | tolerates higher temps than spinning disks |

Numbers above are illustrative starting points to be confirmed against measured fleet data (see §7). The point is: **this table is the entire per-class surface area.** No per-class code, no per-class control parameters — just where each class's curve starts and where it trips.

`comfort` replaces the old `TARGET`/`DEADBAND`/`APPROACH_WINDOW` triple with a single, more honest number: "below this we don't care; above it we ramp proportionally toward emergency."

---

## 5. What gets deleted

The win is subtraction. Removed entirely:

- **Adaptive reconciler / target-drift** (`internal/adaptive/*`) — the setpoint no longer moves; there's no setpoint, just a curve. (Already disabled by default in v0.3.14.)
- **Incremental PID** — `FAN_GAIN`, `DERIVATIVE_GAIN`, `DEADBAND_DRIFT_RATE`, per-class `DEADBAND`.
- **Sample-and-hold pacing** — `HDD_STEP_INTERVAL`, `SSD_STEP_INTERVAL` (v0.3.15) — unneeded; a memoryless curve has nothing to pace.
- **Per-server constant fan floor** — the `dell_xc730xd_12.env` `MIN_FAN=55` hack (v0.3.17) and the per-server target/deadband overrides (v0.3.16). Reverted.
- **EWMA base-speed tracking** — no longer meaningful without an incremental controller.

Knob count for the HDD class drops from ~10 (target, deadband, emergency, approach window, read interval, step interval, + 4 global PID/EWMA params) to **3** (comfort, emergency, read interval).

Kept:

- **Per-class `max()` aggregation** — hottest-demanding class drives the chassis. Unchanged.
- **Unconditional emergency trip** — temp ≥ emergency ⇒ 100%, short-circuit. The one piece of "logic" that must stay, and it's universal. Unchanged.
- **Active-GPU own-fan assist** — a workstation card with its own fan is a genuinely different actuator (we're reacting to the *card's fan*, not chassis temperature), so it keeps its own small rule. This is the one defensible exception and it's about a different *sensor*, not different *control*.
- **Per-class read cadence** — `HDD_READ_INTERVAL` (smartctl is expensive / can disturb drives). A sampling concern, not a control concern.
- **Modes** (`max-cool` / `balanced` / `min-noise`) — re-expressed as a shift of the `comfort` points (cooler intent ⇒ lower comfort ⇒ ramp starts sooner). Same law, shifted table. *(Open question §8.)*

---

## 6. Safety

Unchanged and universal — none of it is per-class behavior:

1. **Emergency trip**: any class at/above its `emergency` ⇒ fans 100%, computed before/around the curve, short-circuits. This is the hard floor on correctness.
2. **Fail-safe on sensor read failure**: temp read fails ⇒ 100% (existing behavior, kept).
3. **Memoryless ⇒ no stuck states**: there is no accumulator or persisted setpoint that can latch the fan at a wrong value across a transient or a restart. A restart resumes correct behavior on the first cycle (the curve is a pure function of the current reading) — this also eliminates the post-restart "cold-start transit" that plagued v0.3.15-17.
4. **Active-GPU assist** retains its die-temperature emergency backstop (`ACTIVE_GPU_EMERGENCY`) independent of the own-fan signal.

---

## 7. Plant data we already have (unraid-1, Dell XC730xd-12)

Measured live 2026-06-21, useful for setting the HDD curve and validating stability:

- **Dead time** ≈ 4.5 min (fan 28→81% produced no drive-temp movement for ~4.5 min).
- **Steady-state gain** ≈ 0.2 °C per fan-%.
- **Natural min-fan (10%) rest** ≈ 45-46°C.
- **Fan → settled temp** (approx): 29% → ~43°C, 45% → ~40°C, 55% → ~38°C.
- A single outlier drive (**sdf**) runs ~5°C hotter than its bay-mates at the same airflow → a physical bay/airflow issue, not a control issue. The unified curve will not (and should not) chase it; flagged for a `smartctl -a` health check / bay swap.

For this array, an HDD curve of `comfort≈33°C → emergency 50°C` (slope 90%/17°C ≈ 5.3%/°C; loop gain ≈ 1.0) settles the spinning drives around **38-40°C, stable** — the ideal-longevity band — with the fan a continuous function of temperature. No floor, no pacing.

---

## 8. Open questions

1. **Do we keep the PID for fast plants (CPU/GPU), or truly unify?** Recommendation: **truly unify.** The proportional curve is stable on fast plants too; keeping two control laws reintroduces exactly the per-class split we're trying to delete. The only cost is a small offset on the CPU/GPU, which is harmless.
2. **Modes as comfort-shifts** — confirm `max-cool`/`balanced`/`min-noise` map cleanly to lowering/raising each class's `comfort` by a fixed delta. Likely yes; needs a table.
3. **Exact curve endpoints per class** — set from fleet data (we have HDD; need CPU/GPU/SSD measured rests). Until measured, use conservative (cooler) comfort points.
4. **The hot-tolerant GPU** — a high `comfort` (~70-75°C) keeps it quiet until genuinely warm, then ramps. Validate it doesn't pin at 100% under sustained load the way the PID did, and decide the acceptable noise/temp point now that drift is gone (we already accepted "loud but safe GPU" in v0.3.14).
5. **Per-server auto-tune** (operator's idea) — a one-time startup routine that measures a host's dead time + natural rest and sets the `comfort` point automatically. The memoryless law makes this *safe to run* (no instability to trip during measurement). Strong future addition; not required for v3.
6. **Offset acceptance** — confirm with the operator that "settles ~39°C, stable" beats "pinned to 40°C but twitchy." (Established in the 2026-06-21 session: yes.)

---

## 9. Migration

- Drop-in: same image, same `max()`/emergency contract, same metrics names where possible (`fan_controller_class_temp_celsius`, `..._proximity_floor_percent` becomes the primary `..._curve_percent`).
- Existing profiles: `*_TARGET`/`*_DEADBAND`/`*_APPROACH_WINDOW` are reinterpreted/retired in favor of `*_COMFORT` + `*_EMERGENCY`; provide a compatibility shim or a one-time profile migration. Per-server hacks (XC730xd `MIN_FAN=55`, target/deadband overrides) are removed.
- Behavior change operators will see: fans become a smooth continuous function of temperature; disks settle a couple degrees above their `comfort` point and hold there flat; no more hunting, no post-restart transit, no silent drift.

---

## 10. One-paragraph rationale

We spent a release line making an integrating controller behave on plants it was mismatched to — windup on slow disks, pinning on hot-tolerant GPUs — and each fix added a knob. The operator's instinct ("heat is heat, same controller for all") is correct, and the way to honor it is to drop the integrator: a single memoryless proportional temperature→fan curve is stable on every plant with *identical code*, needs only a two-number envelope per class, and is lenient by construction. It deletes far more than it adds. Same law everywhere; the only thing that's per-class is which temperatures matter.
