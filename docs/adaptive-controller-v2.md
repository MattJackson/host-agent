# host-agent v2 — Adaptive Setpoint Controller

| | |
|---|---|
| **Status** | Draft — design only, not implemented |
| **Target version** | host-agent v0.2.0 (or v1.0.0 if API stability bumps semver) |
| **Predecessor** | v1 line (v0.1.x), frozen on branch `host-agent-v1` |
| **Audience** | Future contributors + fleet operators |
| **Last updated** | 2026-05-17 |

---

## 1. Summary

Replace the fan controller's fixed per-class temperature targets (`CPU_TARGET=70`, `GPU_TARGET=83`, `HDD_TARGET=40`, etc.) with an **intent-driven adaptive controller**. Operators express what they want (`max-cool`, `balanced`, `min-noise`, `eco`); the controller derives initial targets from per-class hardware envelopes encoded in the agent, then **continuously refines those targets** based on what the specific host's airflow and ambient conditions can actually sustain.

The control layer (PID, every 15s) is unchanged. A new **intent layer** sits above it, adjusting targets every ~10 min within bounded ranges.

Net effect: operators stop having to know the right temperature for HDDs vs. SSDs vs. passive GPUs on their specific hardware. The controller finds the operating point that satisfies intent given physical reality, and adapts as that reality changes (summer, dust buildup, hardware swap).

---

## 2. Motivation

### Where v1 falls short

v1's per-class targets are fixed numbers chosen at configuration time. Two real problems:

**Problem 1: humans don't know the right number.** Operators have to know that a Tesla P4 wants 75°C, a Seagate Exos wants 38°C, an NVMe SSD wants 50°C, and a Xeon E5 wants 65°C — and that these answers vary by chassis, by location, by season. The numbers live in tribal knowledge or scattered comments, not in the agent.

**Problem 2: fixed targets fight reality.** A target the operator wrote assuming "ideal" conditions may be unachievable in this chassis with this airflow with this ambient temp. The PID oscillates around the unachievable setpoint forever, never settling. Observed on classe 2026-05-16 with passive_GPU target=83°C: temp swung 70-80°C in slow cycles. The card was fine; the controller was fighting itself.

### What v2 fixes

- **Intent over numbers.** `HOST_AGENT_MODE=balanced` replaces `CPU_TARGET=65 GPU_TARGET=75 HDD_TARGET=38 SSD_TARGET=50 …`. One env var, every class gets sensible targets.
- **Self-correcting.** When the controller observes the system can't cleanly hold the initial target, it drifts the target toward what's actually achievable, within mode-defined bounds.
- **Fleet-uniform.** The same `HOST_AGENT_MODE=balanced` setting produces appropriate behavior across heterogeneous hardware (R730xd + Tesla P4, Unraid + spinning rust, Mac Mini + NVMe) because each chassis discovers its own equilibrium.

---

## 3. Goals and non-goals

### Goals

- Operator API consists primarily of one knob: **`HOST_AGENT_MODE`**.
- Hardware temperature envelopes (preferred ranges, safety limits) are **encoded in the agent**, not in per-host config.
- Adaptive targets converge to a stable operating point within ~1-3 hours of cold start.
- All target movement is **bounded** by mode-defined safety limits — never drifts into damaging territory.
- Adaptive behavior is **observable** via metrics and a Grafana dashboard row.
- **v1-compatible**: per-class env overrides (`CPU_TARGET=70`) still work and disable adaptive on that class.
- Fits the existing host-agent architecture: single Go binary, single image, fleet-wide deployment via standard image tag.

### Non-goals

- **Replacing the PID layer.** v2 sits above the existing PID, doesn't replace it. Same math, same gains, same cadence.
- **Machine learning** in any non-trivial sense. The "learning" here is a rolling-window adaptive controller — well-understood control engineering, not statistical inference.
- **Predicting load.** v2 reacts to observed thermal state. It doesn't try to predict that "the model is about to do a long generation, ramp fans now."
- **Active GPU thermal management.** Per v1 design, active GPUs (those with their own fans, e.g. RTX A5500) drive chassis assist via own-fan-saturation, not chassis target. v2 keeps that model — adaptive applies only to classes that draw chassis fans directly (CPU, passive_GPU, HDD, SSD).
- **Per-device targets.** The unit of control is the **class** (all HDDs share one target, all CPUs share one target). Per-device adaptive control is out of scope.
- **Cross-host coordination.** Each host's adaptive controller is independent.

---

## 4. User-facing API

### Primary knob

```
HOST_AGENT_MODE=balanced
```

Values:

| Mode | Intent | Trade-off |
|---|---|---|
| `max-cool` | Coolest hardware operating temp the chassis can deliver | Loudest fans, highest fan power |
| `balanced` | Middle-of-envelope targets, moderate fan use | Default; suits most fleet hosts |
| `min-noise` | Quietest acceptable operation — hardware at top of preferred range, never unsafe | Hardware runs warmer (still within spec); slowest fans |
| `eco` | Minimize **total system power** (component idle + fan power, which itself is 10-20W per fan at high RPM) | Hardware moderately warm, very slow fans, optimized for utility bill |

### Per-class overrides (advanced)

```
CPU_TARGET=70           # numeric override — disables adaptive on CPU
GPU_TARGET=75
HDD_TARGET=38
SSD_TARGET=50
```

If set, the adaptive layer is bypassed for that class. The PID uses the fixed number. Useful for:
- Operators with strong opinions backed by their own data
- Reproducing v1 behavior on a per-class basis during migration
- Forcing aggressive cooling on a specific class temporarily (e.g. during a benchmark)

A per-class override does NOT disable adaptive for other classes — they continue using mode-derived targets.

### Deprecated v1 envs

These continue to work but are deprecated:

- `GPU_DEADBAND`, `CPU_DEADBAND`, `HDD_DEADBAND`, `SSD_DEADBAND` — deadbands are now mode-derived and dynamically widened based on observed variance
- `GPU_APPROACH_WINDOW`, `CPU_APPROACH_WINDOW`, `HDD_APPROACH_WINDOW`, `SSD_APPROACH_WINDOW` — replaced by mode-derived approach behavior

v1 envs are honored but logged at startup as deprecated. Migration is purely additive (set `HOST_AGENT_MODE` to opt into v2 behavior; leave existing envs and they keep working).

### Diagnostic envs

```
HOST_AGENT_ADAPTIVE_DRY_RUN=true   # observer runs, target stays at mode-initial, no drift
HOST_AGENT_ADAPTIVE_DISABLED=true  # observer doesn't run; pure v1 behavior
HOST_AGENT_OBSERVER_WINDOW_MINUTES=120   # default 120; adjust for testing
HOST_AGENT_ADAPTIVE_CYCLE_MINUTES=10     # default 10; how often target reconciles
HOST_AGENT_ADAPTIVE_DRIFT_RATE_PER_CYCLE=1   # default 1°C; max target change per cycle
```

These are intentionally minor knobs. v2's promise is that the defaults work.

---

## 5. Concepts

### Class

A **thermal class** is a group of hardware components that share fan control behavior. v2 inherits v1's classes:

- `cpu` — CPUs, driven by CPU die temperature
- `passive_gpu` — GPUs without own fans, rely on chassis airflow
- `active_gpu` — GPUs with own fans (own-fan-driven assist, not adaptive in v2)
- `hdd` — spinning drives
- `ssd` — solid-state drives

Each class has one target temp and one deadband at any given time, applied to whichever physical sensors belong to that class on this host.

### Envelope

A **class envelope** is the agent's encoded knowledge of what temperatures are safe and preferred for a given class of hardware. Compiled in, not user-config (though overridable).

```go
type Envelope struct {
    MinSafe       int  // below this, suspect sensor failure
    PreferredLow  int  // "max-cool" intent target
    PreferredMid  int  // "balanced" intent target  
    PreferredHigh int  // "min-noise" intent target
    MaxSafe       int  // upper bound for adaptive drift (never targeted above this)
    Emergency     int  // immediate 100% fans (existing v1 behavior)
}
```

The four numbered points and one emergency define **mode → target** mappings.

### Mode

A **mode** is the operator's intent. Modes are an enum: `MaxCool`, `Balanced`, `MinNoise`, `Eco`. Each mode defines:
- How to derive initial target from envelope (mostly: which preferred point)
- How to score observed performance (temp distribution vs. fan-change rate vs. total power)
- Initial deadband

### Adaptive target

The **adaptive target** is the per-class temperature setpoint currently in use by the PID. Initialized from mode + envelope. Refined over time by the adaptive layer based on observation.

Persisted to disk so it survives container restart.

### Observation window

A **rolling time window** (default 2 hours) over which the controller collects samples used to make adaptive decisions. Sampled every PID cycle (15s), so ~480 samples in the window.

---

## 6. Architecture

### Two-layer control

```
┌──────────────────────────────────────────────────────────────┐
│  Intent layer (slow loop — 10 min cadence)                   │
│                                                              │
│   HOST_AGENT_MODE + envelopes                                │
│           │                                                  │
│           ▼                                                  │
│   Observer (2-hr rolling window per class)                   │
│           │                                                  │
│           ▼                                                  │
│   Reconciler (rate-limited target adjustment)                │
│           │                                                  │
│           ▼                                                  │
│   State (persisted: adaptive target + deadband per class)    │
│           │                                                  │
└───────────┼──────────────────────────────────────────────────┘
            │  new target + deadband (atomic swap)
            ▼
┌──────────────────────────────────────────────────────────────┐
│  Control layer (fast loop — 15s cadence — UNCHANGED from v1) │
│                                                              │
│   Per-class PIDs read current target+deadband from state     │
│   max() across all classes → fan demand                      │
│   Drift rate limiter → BMC                                   │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### Why two loops

The control layer needs to react quickly to load spikes (15s cadence). The intent layer needs to filter noise and adapt slowly (10-min cadence) — making fast decisions about setpoint would create oscillation between the two layers.

The intent layer's only output is the per-class `(target, deadband)` tuple. The control layer treats those as inputs. Clean separation.

### Single goroutine per layer

- Control layer goroutine: existing fan-controller main loop, unchanged
- Intent layer goroutine: new — sleeps `cycleMinutes`, wakes, runs reconcile cycle, writes new targets to shared state, sleeps

Shared state is a struct guarded by a mutex (no channels needed — writes happen at 10-min cadence, reads happen at 15s, contention is nil).

---

## 7. Class envelopes (the data)

Initial values. Sources cited where research backs them; otherwise sensible-default and revisable per data.

```go
var DefaultEnvelopes = map[Class]Envelope{
    CPU: {
        MinSafe:       20,
        PreferredLow:  55,   // close to ambient + reasonable headroom
        PreferredMid:  65,
        PreferredHigh: 75,
        MaxSafe:       85,   // Xeon E5 family TJunction typically 90-95
        Emergency:     90,
    },
    PassiveGPU: {
        MinSafe:       30,
        PreferredLow:  65,
        PreferredMid:  72,
        PreferredHigh: 80,
        MaxSafe:       85,   // Tesla family slowdown begins 87-92
        Emergency:     90,
    },
    HDD: {
        MinSafe:       10,
        PreferredLow:  32,   // Google HDD paper sweet spot
        PreferredMid:  38,
        PreferredHigh: 43,
        MaxSafe:       45,   // UVa "don't exceed 47°C" research
        Emergency:     50,
    },
    SSD: {
        MinSafe:       15,
        PreferredLow:  45,
        PreferredMid:  50,
        PreferredHigh: 60,
        MaxSafe:       70,
        Emergency:     80,
    },
}
```

### Sourcing

- **HDD**: Google's seminal HDD failure paper (2007) shows lowest AFR in the 35-40°C range; Backblaze independent data ("Drive Stats") confirms no correlation between failure rate and temp in the 15-45°C band but elevated failure above 47°C
- **Passive datacenter GPU**: NVIDIA documents sustained operating temps of 75-87°C for Tesla family; throttle/slowdown at 87-92°C; emergency shutdown at 95°C
- **Xeon E5**: TJunction (max die temp) is published per-SKU at 90-95°C; sustained operation 10-20°C below TJunction is conventional
- **NAND SSD**: TLC/QLC NAND endures higher temps than spinning rust; data retention at low temps is the bigger concern (avoid <0°C); 70°C is the typical thermal throttle floor

### Per-chassis overrides

A profile file (`profiles/<chassis>.env`) can override individual envelope points:

```bash
ENVELOPE_HDD_PREFERRED_LOW=30   # this chassis has aging drives, run cooler
ENVELOPE_HDD_MAX_SAFE=43
```

Profile overrides are merged with defaults at startup. Empty values inherit.

### Future: per-device class detection

v2 keeps v1's class assignment logic (smartctl rotation_rate distinguishes HDD/SSD; nvidia-smi distinguishes active/passive GPU via fan.speed). Future versions might support per-device envelope overrides for outliers (e.g. one drive in a chassis runs hot), but that's v2.1 territory.

---

## 8. Mode definitions

### Mode → initial target

```go
func InitialTarget(env Envelope, mode Mode) (target, deadband int) {
    switch mode {
    case MaxCool:
        return env.PreferredLow, 2     // tight band, aim for coolest
    case Balanced:
        return env.PreferredMid, 3     // default
    case MinNoise:
        return env.PreferredHigh, 4    // wider band, quieter
    case Eco:
        return env.PreferredHigh, 5    // even wider, optimize for total power
    }
}
```

### Mode → scoring criterion

When the adaptive layer evaluates "should I drift the target?", it uses a mode-specific scoring function over the observation window. Lower-is-better.

**All four scoring functions are satisficing over the envelope's preferred band**, not optimizing toward a single point. Inside `[PreferredLow, PreferredHigh]` the band-violation term is zero; outside it grows linearly with distance. This is the key v0.3.2 fix — earlier versions used `balanced = |mean − PreferredMid|`, which would drift target away from observed mean even when temps were comfortably in-band, pushing the PID into saturation for no thermal benefit (see CHANGELOG v0.3.2 for the incident).

#### `max-cool`
```
score = max(0, mean − PreferredLow) + 0.5 * variance
```
"Aim toward PreferredLow; tolerate variance. Below PreferredLow the box is already as cool as we want — settle."

#### `balanced`
```
score = bandDistance(mean, PreferredLow, PreferredHigh)
      + 0.3 * variance
      + 0.3 * fan_change_rate
```
"Any temp inside the preferred band is equally good. Penalize anything outside, plus mild penalty for variance and fan twitchiness."

#### `min-noise`
```
score = max(0, PreferredHigh − mean)            (lean up to ceiling)
      + 5.0 * max(0, mean − PreferredHigh)      (hard cap at PreferredHigh)
      + 2.0 * fan_change_rate
      + 0.5 * variance
```
"Push toward PreferredHigh (warmer = quieter); never cross above it; heavily penalize fan changes."

#### `eco`
```
score = scoreMinNoise(env, stats)   // aliased
```
Currently identical to `min-noise`. Eventually: replace with `estimated_total_watts(temp_distribution, fan_change_rate)` once a per-chassis fan-power model exists. Tracked as a v0.4 item.

The `variance` and `fan_change_rate` terms cancel out in the reconciler's projection comparison (the synth only adjusts `TempMean` ±1, leaving variance/rate unchanged across the three scenarios). They serve only to (a) differentiate modes in `adaptive_mode_preview_score`, and (b) reserve the architectural slot for a future score-synthesizer that models PID response.

### Drift direction from observation

The reconciler periodically computes: "if I moved the target by ±1°C, would the mode score improve?" If yes, move. If no, stay.

```
∆ = +1: try increasing target by 1°C (warmer = quieter)
∆ = -1: try decreasing target by 1°C (cooler = louder)

For each ∆, project: what would temp distribution look like?
  Approximation: if current_mean is 73 and target is 72, then 
  setting target to 73 will likely settle at ~74 (proportional).
  Use observed fan→temp gain.

Pick the ∆ that improves score. Apply if improvement is significant.
```

Specifics in §10.

---

## 9. The Observer

### What it collects

Per class, per PID cycle (15s):

```go
type Sample struct {
    Timestamp     time.Time
    TempCelsius   float64   // class-aggregated (mean of all class members)
    FanDemandPct  int       // controller's commanded fan %
    FanRPMSample  int       // actual RPM (best-effort, IPMI)
    InletCelsius  float64   // ambient
}
```

Stored in a per-class ring buffer sized to fit the window:

```
windowSamples = OBSERVER_WINDOW_MINUTES * 60 / PID_INTERVAL_SECONDS
              = 120 * 60 / 15
              = 480 samples
```

Per-class state holds the ring buffer + derived statistics, refreshed each cycle.

### Statistics computed

Each adaptive cycle (10 min):

```go
type WindowStats struct {
    TempMean       float64
    TempStdDev     float64
    TempP10        float64
    TempP50        float64
    TempP90        float64
    FanDemandMean  float64
    FanChangeRate  float64   // fan-demand changes per minute
    InletMean      float64
    InletStdDev    float64
    Samples        int       // current window fill
}
```

`FanChangeRate` is computed as: number of times the fan demand changed by ≥1% between adjacent samples, divided by window duration in minutes.

### Window warmup

The observer needs `WINDOW_MINUTES` of data before it can confidently propose adjustments. During warmup:
- Observer collects samples
- Reconciler does NOT adjust target (logs "warming up: X% of window filled")
- Mode-initial target stays in effect

After warmup, reconciler runs every cycle.

### Discarding samples

Samples are discarded from the window if:
- Sensor returns an obviously invalid value (NaN, below MinSafe, above 2×Emergency)
- Inlet temp jumps suddenly (>10°C in one sample): something has changed (door opened, HVAC event, sensor swap) → reset window
- Mode is changed: reset window

---

## 10. The Reconciler

Runs every `ADAPTIVE_CYCLE_MINUTES` (default 10 min). For each class:

```
1. If per-class override is set (e.g. CPU_TARGET=70):
     skip — adaptive disabled for this class

2. If window is not yet filled (warming up):
     skip — log progress

3. Compute current score:
     score_now = mode.score(window_stats)

4. Project score at target±1°C:
     // Approximation: if target moves +1°C, mean settles +1°C (with lag).
     // Variance and fan_change_rate are projected via small empirical
     // factor (initially: variance stays same, fan_change_rate drops 5%
     // per +1°C drift toward PreferredHigh).
     score_up   = mode.score(projected_stats(window_stats, +1))
     score_down = mode.score(projected_stats(window_stats, -1))

5. Pick the best direction:
     ∆ = argmin(score_now, score_up, score_down)

6. If ∆ = 0:
     no change — log "settled"
     return

7. If ∆ = +1 or -1:
     new_target = current_target + ∆
     // Hard bounds — never drift outside envelope
     new_target = clamp(new_target, env.PreferredLow, env.PreferredHigh)

     // Rate limit (default: max 1°C per cycle = 6°C/hr)
     if |new_target - current_target| > DRIFT_RATE_PER_CYCLE:
         clip to ±DRIFT_RATE_PER_CYCLE

8. Update deadband based on observed variance:
     new_deadband = max(mode_default_deadband, ceil(TempStdDev * 1.5))
     // Capped at 7°C to keep PID engaged

9. Atomic swap of (target, deadband) in shared state

10. Persist to state file

11. Emit metrics (see §13)
```

### Convergence properties

- **Monotonic toward equilibrium**: each cycle moves at most 1°C, in the direction the score function prefers. As long as `mode.score` is convex around the optimum, convergence is monotonic.
- **Bounded**: target is clamped to `[PreferredLow, PreferredHigh]`. Cannot drift to dangerous regions.
- **Hysteresis-free**: if at cycle N the score is best at target T, and at cycle N+1 conditions haven't changed, target stays at T. No oscillation between adjacent targets.

### Edge cases

- **System under sustained load** (target tracking a load-driven equilibrium that's higher than envelope): controller pushes toward PreferredHigh and stays there. Operator sees `adaptive_target` pegged at `PreferredHigh` and can act (upgrade cooling, reduce load, accept).
- **System idle for hours** (target tracking very low equilibrium): controller pushes toward PreferredLow and stays. If load returns, controller drifts back up over the next 30-60 min. The PID handles the load step in the meantime.

---

## 11. State persistence

### File location

```
/var/lib/host-agent/state/adaptive.json
```

Bind-mounted from the host (same dir as v1's `base` EWMA file). Survives container restart.

### Format

```json
{
  "version": 1,
  "mode": "balanced",
  "last_update": "2026-05-17T05:30:00Z",
  "classes": {
    "cpu": {
      "target_celsius": 64,
      "deadband_celsius": 3,
      "variance_observed_ewma": 1.2,
      "inlet_baseline_celsius": 22.5,
      "window_samples_filled": 480,
      "last_change_direction": -1
    },
    "passive_gpu": {
      "target_celsius": 73,
      "deadband_celsius": 4,
      "variance_observed_ewma": 1.8,
      "inlet_baseline_celsius": 22.4,
      "window_samples_filled": 480,
      "last_change_direction": +1
    },
    "hdd": { ... },
    "ssd": { ... }
  }
}
```

### Lifecycle

- **Startup**: read state file. If mode in file matches current `HOST_AGENT_MODE`, resume from saved targets. If mode changed, reset to new mode's initial targets.
- **Each adaptive cycle**: write updated state atomically (write to `adaptive.json.tmp`, fsync, rename).
- **Mode change at runtime**: detected by env reload (not yet supported — `HOST_AGENT_MODE` is read only at startup). When supported, mode change triggers reset.
- **Corruption / parse error**: log warning, ignore, reset to mode-initial targets.

### Migration from v1

On first run after upgrading to v2:
- If `adaptive.json` doesn't exist: normal cold-start, warm window over 2 hrs
- If v1's `base` EWMA file exists: ignored (v2 uses its own state)

No data migration needed. v1's EWMA was for chassis equilibrium, not class targets.

---

## 12. Safety

### Hard guarantees

These are non-negotiable invariants enforced by code:

1. **Target ∈ [PreferredLow, PreferredHigh]** — never drifts outside envelope's preferred range.
2. **Emergency threshold is honored** — at temp ≥ `Emergency`, fans go to 100% immediately, bypassing all adaptive math. Same as v1.
3. **Sensor fault detection** — values outside [MinSafe, 2×Emergency] are discarded. If >10% of samples in a window are discarded, log error and reset window.
4. **Variance ceiling** — if observed `TempStdDev` exceeds 5°C in a 30-min window, log error, reset to mode-initial target. Something's wrong (sensor flapping, hardware change).
5. **Mode override** — if `HOST_AGENT_MODE` env is unset, controller behaves identically to v1 (uses per-class envs or profile defaults). v2 is opt-in.
6. **Disabled override** — `HOST_AGENT_ADAPTIVE_DISABLED=true` skips the intent layer entirely; PID uses whatever per-class envs/profile says.

### Recovery patterns

| Failure | Detection | Recovery |
|---|---|---|
| State file corrupted | JSON parse fails | Log warning, ignore file, reset to mode-initial |
| State file missing | First-run | Cold-start, warm window |
| Sensor returns garbage | Value outside [MinSafe, 2×Emergency] | Discard sample |
| Sensor flapping | >10% samples discarded in window | Reset window, log error |
| Inlet temp spike | >10°C jump | Reset window (environmental change) |
| Mode env changed | Detected at startup | Reset to new mode's initial |
| Hardware change | Class device count changes | Reset window for that class |

### Audit trail

Every target change is logged with the reason:

```
2026-05-17 05:40:00 - adaptive: class=passive_gpu target 73 → 74 (mode=balanced, score 0.42 → 0.39, projected: temp_mean 73.1 → 74.0, fan_change_rate 8.2 → 7.8)
```

Operators can `journalctl -u host-agent | grep adaptive:` to see the decision history.

---

## 13. Observability

### Metrics (textfile collector → node-exporter → vmagent → Prometheus)

```
adaptive_enabled{host="…"} 1
adaptive_mode{host="…",mode="balanced"} 1
adaptive_target_celsius{host="…",class="passive_gpu"} 73
adaptive_deadband_celsius{host="…",class="passive_gpu"} 4
adaptive_envelope_preferred_low{class="passive_gpu"} 65
adaptive_envelope_preferred_high{class="passive_gpu"} 80
adaptive_envelope_max_safe{class="passive_gpu"} 85
adaptive_window_samples_filled{class="passive_gpu"} 480
adaptive_window_temp_mean{class="passive_gpu"} 73.2
adaptive_window_temp_stddev{class="passive_gpu"} 1.8
adaptive_window_fan_change_rate{class="passive_gpu"} 2.4
adaptive_score{class="passive_gpu",mode="balanced"} 0.42
adaptive_target_drifts_total{class="passive_gpu",direction="up"} 12
adaptive_target_drifts_total{class="passive_gpu",direction="down"} 8
adaptive_target_resets_total{class="passive_gpu",reason="sensor_flap"} 1
```

### Grafana dashboard row: "Adaptive Controller"

Panels:
1. **Targets vs. envelope** — per class, current target as a horizontal line, envelope bands (preferred-low to preferred-high) shaded. Operators see at a glance whether classes are converged or pegged.
2. **Temp distribution per class** — p10/p50/p90 of the observation window, overlaid with current target. Shows how well the controller is tracking.
3. **Fan change rate** — per class, changes per minute. High = chasing; low = settled.
4. **Mode score trend** — per class, score over time. Should descend then plateau as controller converges.
5. **Drift events** — annotation track showing target changes with reason.

This is the dashboard for the operator who wants to *understand* what the controller is doing, not just trust it.

---

## 14. Migration from v1

### Compatibility matrix

| v1 setting | v2 behavior |
|---|---|
| No env vars set | Profile defaults used (v1 behavior — adaptive off because `HOST_AGENT_MODE` is unset) |
| `HOST_AGENT_MODE=balanced` | v2 adaptive on for all classes |
| `HOST_AGENT_MODE=balanced` + `CPU_TARGET=70` | v2 adaptive on for GPU/HDD/SSD; CPU uses fixed 70 |
| `CPU_TARGET=70` (no mode) | v1 behavior — fixed targets, no adaptive |
| `HOST_AGENT_ADAPTIVE_DISABLED=true` | Forces v1 behavior even if mode is set |

### Rollout phases

#### Phase 1: ship envelopes + modes (static only)
- Implement envelopes in code
- Implement mode → initial target mapping
- `HOST_AGENT_MODE` env var read at startup
- PID uses mode-derived target (or per-class override)
- **No adaptive layer yet** — targets are fixed after initial derivation
- This phase alone provides: "I don't need to know the right number; I just say balanced"
- **Safe to deploy fleet-wide as a v0.2.0 release**

#### Phase 2: observer (read-only)
- Implement sliding window per class
- Compute statistics every adaptive cycle
- Emit observer metrics
- **No target changes** — just observation
- Let operators watch for 1-2 weeks; build confidence the observer is correct
- Ship as v0.2.1

#### Phase 3: reconciler (active drift)
- Wire observer output to target drift
- Enforce all safety bounds
- Ship as v0.2.2 with `HOST_AGENT_ADAPTIVE_DRY_RUN=true` recommended for first deployment per host
- After 1-2 weeks of dry-run validation per fleet operator, flip to active
- Ship v0.3.0 with adaptive on-by-default for `HOST_AGENT_MODE` users

#### Phase 4: Grafana dashboard
- Add the "Adaptive Controller" row to the bundled dashboard
- Ship with v0.3.0

#### Phase 5: testing scaffolding for future contributors
- Soak tests, scripted thermal traces
- Documented in CONTRIBUTING.md

### Deprecation timeline

- v0.2.0: `CPU_TARGET`/etc. continue to work, no warning
- v0.3.0: per-class targets log a one-time deprecation note at startup if both target and mode are set
- v1.0.0: per-class targets still work but are documented as "advanced override only"
- Never removed entirely — fixed numbers remain a legitimate operator choice

---

## 15. Testing strategy

### Unit tests

- `envelope.InitialTarget(env, mode)` — every mode × every class, expected outputs
- `mode.Score(stats)` — synthetic stats, verify scoring functions
- `reconciler.Step(state, stats, env, mode)` — table-driven, dozens of cases:
  - Warming up → no change
  - Settled at optimum → no change
  - Too hot → drift down
  - Too cool → drift up
  - At envelope boundary → don't cross
  - Sensor flap → reset
  - Mode mismatch → reset
- State load/save roundtrip

### Integration tests

- Mock NVML / IPMI / smartctl with scripted thermal traces
- Run controller through simulated 24-hour day with load steps + ambient drift
- Assert: target converges, doesn't violate bounds, fan changes ≤ expected rate

### Soak tests

- Real host (classe or test VM) running controller for 7 days
- Capture metrics, plot target vs. envelope, fan_change_rate trend
- Manual review of drift decisions via journal log

### Regression tests

- v1 behavior preserved when `HOST_AGENT_MODE` is unset
- v1 behavior preserved when `HOST_AGENT_ADAPTIVE_DISABLED=true`
- v1 per-class targets continue to work when adaptive is enabled (just disable adaptive for that class)

### Property-based tests

Convergence properties stated formally:

- For any starting target T ∈ [PreferredLow, PreferredHigh], and constant load, controller converges in ≤ (PreferredHigh - PreferredLow) cycles
- For any starting target, controller never drifts outside [PreferredLow, PreferredHigh]
- For any sensor flap pattern, `adaptive_target_resets_total` is bounded (no infinite reset loop)

---

## 16. Open questions

1. **Should `eco` mode require a fan power model?** The score function needs `watts = f(fan_rpm)`. Without a per-chassis model, eco devolves to min-noise. First implementation: ship rough lookup tables for known chassis (R730, R730xd, R610, etc.), warn on unknown chassis with "eco mode falls back to min-noise on this chassis."

2. **How aggressive should drift be?** Default `DRIFT_RATE_PER_CYCLE=1°C` × 10-min cycles = 6°C/hr max. Probably too slow to react to seasonal changes within a day. But faster drift risks chasing transients. Tunable, default conservative.

3. **Should the window be class-specific?** HDDs change temp very slowly (minutes); SSDs and GPUs change in seconds. A 2-hr window is over-sampled for HDDs and possibly under-sampled for very dynamic workloads. Initial implementation: one window size for all classes. Future: per-class.

4. **Multi-CPU / multi-GPU envelopes**: when there are 2 CPUs or 2 GPUs of different generations in one class, the envelope is one-size-fits-all. Hot CPU dominates the class temp. Probably fine — if you have a Xeon E5 + Xeon E5-v4 in one chassis, the envelope conservatively fits both. Edge case.

5. **Mode reload at runtime**: currently `HOST_AGENT_MODE` is read at startup. Live reload via SIGHUP would be operator-friendly but adds complexity. Defer to v2.1.

6. **Hardware envelope corrections via fleet telemetry**: long-term, if many operators report `adaptive_target` pegged at `PreferredHigh` for a given chassis + class combo, that's signal the envelope's PreferredHigh is too low for that hardware in real deployments. Could feed back into shipped envelope defaults. Not v2.

---

## 17. Future work (v2.1+)

- **Per-device envelopes**: tag individual drives/GPUs with envelope overrides ("disk7 is the old one, target lower").
- **Time-of-day modes**: `night` mode that auto-switches to `min-noise` between configured hours.
- **Load-aware adaptation**: feed-forward from compute utilization to fan demand (anticipatory).
- **Cross-class scoring**: instead of per-class optimization, optimize the whole-host fan profile (one PID per fan group, not per class).
- **Public API for mode definition**: let advanced operators define custom modes via config (e.g. `night-coding`).
- **Adaptive PID gains**: tune P/D per-class based on observed response, not just target.
- **Telemetry export for fleet envelope tuning**: opt-in anonymous reporting of `(chassis, class, settled_target, settled_variance)` to inform future default envelopes.

---

## 18. Implementation file layout

Concrete files this design implies:

```
host-agent/
├── cmd/fan-controller/
│   └── main.go                    # extended to start the intent layer goroutine
├── internal/
│   ├── envelope/                  # NEW
│   │   ├── envelope.go            # Envelope struct + DefaultEnvelopes map
│   │   ├── envelope_test.go
│   │   └── load.go                # profile-file overrides
│   ├── mode/                      # NEW
│   │   ├── mode.go                # Mode enum + score functions
│   │   ├── mode_test.go
│   │   └── parse.go               # env-var parsing
│   ├── adaptive/                  # NEW
│   │   ├── observer.go            # ring buffer + window stats
│   │   ├── observer_test.go
│   │   ├── reconciler.go          # drift decision
│   │   ├── reconciler_test.go
│   │   ├── state.go               # persistence
│   │   └── state_test.go
│   ├── config/
│   │   └── config.go              # extended: read HOST_AGENT_MODE, mode merge
│   └── controller/
│       └── controller.go          # extended: read adaptive state per cycle
├── docs/
│   ├── adaptive-controller-v2.md  # this document
│   └── envelope-sources.md        # citation list for envelope defaults
├── profiles/
│   └── default.env                # envelope overrides syntax documented
└── CHANGELOG.md                   # v0.2.0 entry
```

---

## 19. Out of scope (explicitly NOT this design)

- Replacing nvidia-smi shell-out with go-nvml (still a `runner.Exec` call — keep the project's no-deps philosophy)
- Changing fan-controller's vendor guard (Dell-only remains)
- Active-GPU adaptive control (own-fan logic stays unchanged)
- Anything outside fan/temp management (GPU clocks, persistence mode, power limits — those are install-time configuration, not continuous control; see 2026-05-16 design decision documented in ai/infra/a5500-perf.sh header)

---

## 20. Decision history

| Date | Decision | Rationale |
|---|---|---|
| 2026-05-17 | Two-layer architecture (intent + control) | Time-scale separation avoids cross-layer oscillation |
| 2026-05-17 | Envelopes encoded in code, not user config | Hardware knowledge belongs in the agent; fleet uniformity |
| 2026-05-17 | Modes as primary user API | Operators express intent, not numbers |
| 2026-05-17 | Per-class overrides preserved | Migration safety; advanced use case |
| 2026-05-17 | Skip ML, use bounded adaptive controller | Predictable, explainable, debuggable |
| 2026-05-17 | Phased rollout (static → observe → adapt) | Each phase delivers value; cumulative confidence |

---

*This document is a design draft. Implementation begins after review and approval.*
