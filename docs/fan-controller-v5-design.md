# host-agent v5 — Target-Seeking Adaptive Curve

| | |
|---|---|
| **Status** | Draft — design only, not implemented |
| **Target version** | host-agent v0.5.0 |
| **Predecessor** | v0.4.0 (one memoryless proportional curve per class) |
| **Builds on** | the `internal/adaptive` machinery (kept dormant through v0.4.0) |
| **Last updated** | 2026-06-21 |

---

## 1. Summary

Keep the adaptive "learning" layer — it was the right idea — but **change what it
optimizes**. The operator's goal, in their words: *"get me to the temp, stay
there, keep fans optimal (not bouncing), and as quiet as possible to maintain
that temp."*

That is a **single constrained objective, not two competing ones**:

> **Hold each class at its `TARGET`, using the *minimum* fan speed that maintains
> it — stably.**

"As quiet as possible" and "hold the target" are the *same* operating point: the
equilibrium fan that settles the plant exactly at TARGET is, by definition, the
slowest fan that holds it (any more over-cools = wasted noise; any less = misses
target). The v0.3.x reconciler's bug was optimizing *quiet without the target
constraint* — so it let the temperature rise to save noise. v0.5.0 optimizes quiet
**subject to** holding the target, which is what you actually want.

It also formalizes the lifecycle you described — *"I've not seen this box before:
scan, learn its airflow, optimize, save, run"* — as a first-class first-run box
scan (§7).

The mechanism is a **two-timescale controller**:

- **Inner loop (fast, every cycle, memoryless):** the v0.4.0 proportional
  temperature→fan curve. Inherently stable on any plant — cannot wind up or hunt.
- **Outer loop (slow, learning):** observe the *steady-state* temperature each
  class actually settles at, compare it to the operator's `TARGET`, and slowly
  shift the curve so the steady-state error goes to zero. This is integral action
  at the plant's *settling* timescale (minutes-to-hours), not the control cycle —
  so it eliminates the proportional curve's offset **without** reintroducing the
  dead-time limit cycle that killed the old PID.

Net: the controller holds your target (the thing v0.4.0's proportional offset
gives up), stays stable (the thing the old integrating PID gave up), and learns
each host's airflow on its own (the original adaptive vision) — but it learns the
*means* (how much fan), never the *ends* (what temperature), so it can't fight you.

---

## 2. Motivation

### What was right about the old adaptive layer

The vision — "the operator shouldn't have to know the exact right temperature for
a P4 vs. an Exos drive on *their* chassis; the agent should learn it" — is correct
and worth keeping. Per-host airflow, ambient, and dust genuinely differ; a static
number can't be optimal everywhere.

### What was wrong

It optimized **fan demand (noise)**, so with thermal headroom it drifted the
*target itself* upward to quiet the fans — turning "balanced HDD = 38°C" into a
drive sitting at 46°C (the bug that started the 2026-06 saga). Learning that moves
the goal away from what the operator set is worse than no learning.

### What v0.4.0 fixed and what it gave up

v0.4.0 replaced the integrating PID with a memoryless proportional curve: stable
on every plant, no hunt. But a proportional controller has **steady-state offset**
— it settles *near* a comfortable band, not *on* a target. We accepted that as
"leniency." v0.5.0 removes the offset the right way: a slow outer learner, not a
fast integrator.

---

## 3. Architecture — two timescales

```
            operator sets:  TARGET (desired hold temp),  EMERGENCY (hard cap)
                                   │
          ┌────────────────────────┴───────────────────────┐
          │  OUTER LOOP  (slow learner, ~every settle-time) │
          │  observe steady-state temp  → error = Tss-TARGET│
          │  nudge rampStart to drive error → 0  (±≤1°C)    │
          │  gated: only when inner loop has SETTLED         │
          └────────────────────────┬───────────────────────┘
                                   │ learned rampStart (persisted)
          ┌────────────────────────┴───────────────────────┐
          │  INNER LOOP  (every cycle, memoryless)          │
          │  fan = lerp(temp, rampStart→EMERGENCY,           │
          │             MIN_FAN→MAX_FAN)        [v0.4.0 law] │
          └─────────────────────────────────────────────────┘
                                   │
                              max() across classes → chassis fans
                              (+ emergency trip, unchanged)
```

**Inner loop** is exactly v0.4.0's `control.Curve` — unchanged, always stable.

**Outer loop** is the rehabilitated `internal/adaptive`:
- Reuses the existing **observer** (sliding window, percentile stats, dip-robust
  p50/p90 — the v0.3.9 lessons) and **persistence**.
- Replaces the objective: instead of `score = f(fan_demand)`, the action is a
  pure proportional-to-steady-state-error nudge of `rampStart`.
- Replaces the actuator: it adjusts the **curve's rampStart** (how much fan at a
  given temp), not a free-floating "target" decoupled from intent.

### Why two timescales is stable where one PID wasn't

The integrating PID failed because it integrated error **every 15-30 s** on a
plant with ~4.5 min of dead time — it kept ramping before it could see its own
effect (windup → overshoot → limit cycle). The outer learner only acts **after
the inner loop has demonstrably settled** (low windowed variance) and only **once
per many dead-times**, by **≤1°C of rampStart at a time**. By the time it adjusts,
the plant has fully responded to the last adjustment — there is no un-observed
in-flight action to wind up on. This is textbook singular-perturbation / gain-
scheduling separation: a fast inner loop the slow outer loop treats as
instantaneous.

---

## 4. The objective change (the whole point)

| | v0.3.x reconciler | v0.5.0 learner |
|---|---|---|
| Optimizes | minimize fan demand (noise) | minimize \|steady-state temp − TARGET\| |
| Moves | the target (the *ends*) | rampStart / curve placement (the *means*) |
| Can override operator intent? | **yes** (drifted target up) | **no** (TARGET is fixed by the operator) |
| Result | drives ran hot to stay quiet | drives held at the requested temp |

The operator's `TARGET` is now honored as a real setpoint that the system *learns
to hit*, instead of a starting hint the system felt free to abandon.

---

## 5. How it avoids every v0.3.x failure mode

| Old failure | Cause | v0.5.0 guard |
|---|---|---|
| Drives drifted hot (38→44) | objective = noise; moved target up | objective = target; learner moves the curve, never the target |
| Dead-time limit cycle / overshoot | integrator ran at cycle rate | outer loop acts only when settled, ≤1°C per settle-time |
| 23:45 dip cratered target | reacted to a transient | robust windowed steady-state (p50, settle-gated), not instantaneous |
| Pinned at 100% chasing ghost | unreachable target + integral | inner loop is bounded/stable; learner can't demand the impossible, only re-place a reachable curve |
| Fought operator intent | learned the *ends* | learns the *means*; TARGET & EMERGENCY are operator-owned |
| Post-restart transit | state-dependent warmup | inner curve valid on cycle 1; learned rampStart persisted |

---

## 6. Knobs

Back to **two operator numbers per class**, but now both mean exactly what they
say because the learner makes TARGET real:

| Knob | Meaning |
|---|---|
| `*_TARGET` | the temperature to hold this class at (learner seeks it) |
| `*_EMERGENCY` | hard cap → 100% + trip (unchanged, safety) |

`rampStart` is **learned and persisted**, initialized at `TARGET − margin` (or via
the optional one-shot step test, §7). Operators never set it. Internal tunables
(learn rate, settle-variance threshold, learn cadence, max ±°C/step) ship with
safe defaults and rarely need touching.

---

## 7. First-run box scan — the lifecycle

The behavior you described —

> *"oh, I've not seen this box before: scan, learn its airflow, optimize, save
> settings, run."*

— is a first-class lifecycle, not just an optional initializer. It directly serves
"get me to the temp with the minimum fan" because it *measures* each box's
fan→temp relationship instead of guessing it.

### Lifecycle

```
first boot (no saved scan)
   │
   ├─ SCAN     hold fans at a few fixed speeds (e.g. 20/35/50/70%), let each
   │           settle, record the steady-state temp per class. ~30-45 min, once.
   │           (Safe to run: emergency trip is live throughout; abort to a safe
   │            curve instantly if any class nears EMERGENCY.)
   │
   ├─ LEARN    fit the per-class plant: fan→temp slope + natural rest + the
   │           dead-time/settle-time. This is the box's airflow signature.
   │
   ├─ OPTIMIZE place each class's rampStart so the curve's equilibrium == TARGET
   │           — i.e. the minimum fan that holds the requested temp.
   │
   ├─ SAVE     persist the learned plant + rampStart (and a scan-version stamp).
   │
   └─ RUN      inner curve runs from the saved rampStart; the slow outer learner
               (§3) trims it forever after for drift (dust, ambient, new drives).

subsequent boots → load saved scan, skip straight to RUN. Re-scan on demand
(operator command) or auto when the hardware fingerprint changes (drives added,
chassis swap).
```

### Why scan *and* keep the slow learner

The scan gets you **optimal on day one** (no slow convergence from a guess); the
outer learner handles **slow drift afterward** (summer ambient, dust buildup, a
new drive in a hot bay). Scan = fast, accurate, one-time placement; learner =
gentle lifelong maintenance. Together they're "optimal immediately and stays
optimal."

### Build: save what we can, rebuild what we must

This is a **revival of `internal/adaptive`, not a from-scratch rewrite** — but
rebuild the parts that were aimed at the wrong objective:
- **Keep**: the `Observer` (sliding window, dip-robust p50/p90 — the v0.3.9
  lessons), the persistence layer, the metrics plumbing.
- **Rebuild**: `Reconciler.Step` — drop the noise-minimizing score entirely;
  replace with (a) the scan/learn plant fit and (b) the steady-state-error nudge
  of rampStart, settle-gated and clamped to `[safe-low, EMERGENCY)`.
- **Add**: the scan state machine (SCAN→LEARN→OPTIMIZE→SAVE) and a persisted
  per-box plant model + hardware fingerprint.
- **Metrics**: `adaptive_*` return, now reporting steady-state error, learned
  rampStart, plant slope, and scan/settle state — so the learner is *observable*
  (the old one was opaque, which is how the drift-up bug hid for weeks).

---

## 8. Safety

- **Emergency trip and read-fail fail-safe**: unchanged, instant, independent of
  both loops.
- **Inner loop always valid**: if the learner is disabled, errors, or has no
  history, the memoryless curve still runs correctly from the persisted/initial
  rampStart. Learning is an *optimization on top of* a safe controller, never a
  prerequisite for one.
- **Bounded actuation**: rampStart is clamped so the curve can never ramp later
  than is safe near EMERGENCY, nor demand fan below MIN_FAN.
- **Settle-gated, rate-limited**: the learner cannot make fast or large moves;
  worst case of a bad estimate is a ≤1°C curve nudge that the next observation
  corrects.

---

## 9. Open questions

1. **Learn rate & settle gate** — pick the variance threshold and cadence from
   the measured plant settling times (we have unraid-1's ~4.5 min dead time;
   need CPU/GPU). Start conservative.
2. **Symmetric seeking vs. cap** — should the learner pull a *too-cool* class
   warmer (toward TARGET, saving fan) as readily as it cools a too-hot one? For
   disks/CPU: yes (TARGET is the goal both ways). For a hot-tolerant GPU run in a
   "min-noise" intent: maybe TARGET is a *ceiling* and we only seek downward.
   Resolve per-class or via a mode flag.
3. **Modes** re-expressed as TARGET shifts (max-cool = lower TARGET, etc.).
4. **Convergence reporting** — surface "learned rampStart, steady-state error,
   cycles-to-converge" so an operator can see it working (the old layer was
   opaque, which hid the drift-up bug for weeks).
5. **Interaction across classes** — chassis fan is `max()` of curves; the learner
   is per-class. Confirm a class that never binds `max()` doesn't chase a target
   it can't influence (gate learning on "this class is actually driving fans").

---

## 10. One-paragraph rationale

The adaptive idea was right and we're keeping it; it just optimized the wrong
thing. The goal is *temp **and** silence* — but those aren't a tradeoff: the
quietest a box can run while holding its target IS the equilibrium fan that
settles it at the target. So the objective is one constrained thing — *hold
TARGET with the minimum fan that maintains it, stably* — not the old *minimize
noise* (which sacrificed temp). Implement it as v0.4.0's memoryless proportional
curve (fast, always-stable inner loop) plus a slow outer learner that places the
curve so steady-state == TARGET, seeded by a one-time **box scan** that measures
each host's airflow on first sight (scan → learn → optimize → save → run) and
maintained by gentle lifelong trimming. The learner adjusts the *means* (how much
fan) and never the *ends* (the target), so it learns each box without ever
fighting what the operator asked for: optimal on day one, stays optimal, never
hot to "save noise."

---

## 11. As-built (v0.6.x) + the invariants that make it a system

This is the system actually shipped, and the **invariants** it rests on — the
things that must stay true for it to be a coherent design rather than a pile of
fixes. Every v0.6.x change was converging a *parameter* of this design to what
real hardware required, not bolting on new mechanism.

**The architecture (cascade / two-timescale control + feedforward + safety):**

```
            ┌── SAFETY (independent, instant, never paced) ──────────────┐
            │  temp ≥ emergency → 100% ;  sensor read fail → 100%        │
            └────────────────────────────────────────────────────────────┘
  FEEDFORWARD                INNER LOOP                 OUTER LOOP
  (first-run scan)      (every cycle, memoryless)   (slow, settle-gated)
  measure fan→temp  →   fan = Curve(temp, comfort,  →  TargetSeek nudges
  place comfort to       emergency)  — can't wind      comfort ≤1°C so
  hold target            up or hunt, any plant         steady-state → target
```

**Invariants (must hold; each has a test or a structural guarantee):**

1. **Inner loop is memoryless.** `Curve` is a pure function of current temp →
   cannot wind up or hunt regardless of plant dead-time. *This is the
   foundation; everything else sits on top of a loop that is stable by
   construction.*
2. **Inner loop always provides temp-proportional cooling**, independent of the
   learner. ⇒ the learner's tolerance band can never make the plant unsafe (a
   hot plant gets fan from the curve no matter what the learner believes).
3. **Outer-loop horizon < action cadence.** The learner's observation window
   (20 min) must be short relative to its cadence (10 min) so it sees its own
   effect before acting again. Violating this *is* outer-loop windup — the
   docker-1 GPU chase (v0.6.3 fixed the 120-min window). **Design rule:
   window ≈ 1–2× cadence, never ≫.**
4. **Learner moves the means, never the ends.** It adjusts `comfort` (how much
   fan), never `TARGET`. ⇒ it can learn each box's airflow without ever
   overriding operator intent (the v0.3.x drift bug, structurally impossible
   now).
5. **Objective is asymmetric** = "minimum fan that holds target": tolerate
   `+ToleranceHotC` warm (don't chase), reclaim fan at `-ToleranceCoolC` cool
   (don't over-fan). Symmetric tolerance violates the objective (over-fans after
   any hot blip — v0.6.4 fixed it).
6. **Learner is bounded + settle-gated + saturation-aware.** ≤1°C/tick, clamped
   to `[floor, emergency-1]`, acts only on a low-variance window, won't chase a
   saturated fan. ⇒ worst case of a bad estimate is one small reversible nudge.
7. **Safety is independent of both loops.** Emergency trip and read-fail
   failsafe short-circuit before any curve/learner math and are never paced. ⇒
   no learner/scan state can defeat the thermal backstop.

**Per-class envelope is the only operator surface:** `TARGET` (hold here) +
`EMERGENCY` (hard cap). Everything else (comfort, the learned curve placement)
is derived/learned, not hand-tuned. `GPU_TARGET=87` (v0.6.2) is an *envelope*
value — the card's achievable equilibrium — not a control-logic change.

**Why this is a design, not patches:** the mechanism set has been fixed since
v0.6.0 (scan + curve + learner + safety). v0.6.1–v0.6.4 changed only
*parameters/objective-shape* of that fixed mechanism (metric emission, GPU
envelope, window horizon, tolerance asymmetry) — each one forced by an invariant
above that real hardware exposed. No new control paths were added reactively.
The remaining known refinement is documented (per-class tolerance if one
envelope proves too tight), not discovered ad hoc.
