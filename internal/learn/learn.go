// Package learn implements the v0.5.0 target-seeking outer loop: the slow,
// stable learner that places a class's fan-curve ramp-start so the plant settles
// at the operator's TARGET using the MINIMUM fan that maintains it.
//
// It sits ABOVE the memoryless proportional curve (internal/control.Curve). The
// curve is the fast, always-stable inner loop; this is the slow outer loop that
// removes the curve's steady-state offset. Because it only acts on a SETTLED
// observation and moves the ramp-start by at most a degree at a time, it is
// integral action at the plant's settling timescale — it cannot reintroduce the
// dead-time windup/limit-cycle that the old per-cycle integrating PID suffered.
//
// Objective (the whole point of v0.5.0): hold steady-state temp == TARGET. That
// single goal yields both halves of "temp and silence":
//   - steady-state too HOT  → need more fan → lower ramp-start (ramp earlier).
//   - steady-state too COOL → using more fan than needed → raise ramp-start
//     (ramp later) so the fan eases — the "as quiet as possible" half.
//
// The fixed point is exactly TARGET at the minimum fan that holds it.
//
// The learner adjusts the MEANS (ramp-start / how much fan), never the ENDS
// (TARGET). That is the structural fix for the v0.3.x bug where the old
// reconciler optimized noise by moving the target itself and ran drives hot.
package learn

// Params tunes the outer loop. Defaults are deliberately conservative — this is
// a slow lifelong trimmer, not a fast servo.
type Params struct {
	// ToleranceHotC / ToleranceCoolC: ASYMMETRIC dead zone around TARGET. The
	// learner acts only when steady-state is more than ToleranceHotC ABOVE target
	// (→ add fan) or at-or-more-than ToleranceCoolC BELOW target (→ reclaim fan).
	//
	// Asymmetry is the point: a hot tolerance > cool tolerance means the learner
	// TOLERATES running a degree or two warm (so a card that naturally sits at
	// target+1 isn't chased — the bug that ran docker-1's GPU fans up), but
	// EAGERLY reclaims fan whenever the plant is at/below target (so a class that
	// cooled after a transient hot spell doesn't stay over-fanned forever — the
	// bug that left docker-2's 39°C drive at 51% fan). Net: it settles at the
	// minimum fan that holds target, tolerating slightly warm over wasting fan.
	ToleranceHotC  float64
	ToleranceCoolC float64
	// MaxStepC: largest ramp-start change per action, in °C. 1 = gentlest.
	MaxStepC int
	// SettleStdDev: only act when the windowed temperature standard deviation is
	// at/below this — i.e. the inner loop has settled. Acting on an unsettled
	// (in-flight) window is what would couple to the plant's dead time and hunt;
	// this gate is the core stability guarantee of the two-timescale design.
	SettleStdDev float64
	// SatFanP90: if windowed p90 fan demand is at/above this, the fan is
	// effectively saturated — more airflow isn't available, so don't keep
	// lowering ramp-start chasing a target the plant can't reach (the emergency
	// trip is the backstop). p90 (not mean) per the v0.3.9 dip-robustness lesson.
	SatFanP90 float64
	// MinRampStart / MaxRampStart: hard clamp on the learned ramp-start. Keeps
	// the curve within the safe envelope (typically MinSafe .. Emergency-1).
	MinRampStart int
	MaxRampStart int
	// MinFanFloor: the curve's MIN_FAN %, i.e. the fan demand at/below comfort.
	// The reclaim ("too cool → raise ramp-start to quiet the fan") branch is a
	// no-op for noise when demand is already at this floor — there is no fan
	// above the floor to give back. Acting anyway only raises the temperature at
	// which cooling starts, with zero noise benefit, so the ramp-start ratchets
	// monotonically toward MaxRampStart every idle tick. THAT is the drift bug
	// that ran docker-1's CPU comfort to 79 (fans pinned at 10% while the CPU
	// climbed to 77). Below this floor the reclaim branch holds instead.
	MinFanFloor float64
}

// DefaultParams returns conservative defaults for a slow disk/CPU plant.
// minFanFloor is the curve's MIN_FAN % (see Params.MinFanFloor).
func DefaultParams(minRampStart, maxRampStart int, minFanFloor float64) Params {
	return Params{
		ToleranceHotC:  2.0, // tolerate up to +2°C over target before adding fan (no chase)
		ToleranceCoolC: 1.0, // reclaim fan once ≥1°C below target (don't stay over-fanned)
		MaxStepC:       1,
		SettleStdDev:   1.5,
		SatFanP90:      99,
		MinRampStart:   minRampStart,
		MaxRampStart:   maxRampStart,
		MinFanFloor:    minFanFloor,
	}
}

// Reason explains a decision (for metrics/observability — the old learner was
// opaque, which is how its drift-up bug hid for weeks).
type Reason string

const (
	ReasonNotSettled  Reason = "not_settled"  // window too noisy to act on
	ReasonInTolerance Reason = "in_tolerance" // already at target (±tolerance)
	ReasonTooHot      Reason = "too_hot"      // lowered ramp-start (more fan)
	ReasonTooCool     Reason = "too_cool"     // raised ramp-start (less fan, quieter)
	ReasonSaturated   Reason = "saturated"    // too hot but fan maxed — can't help
	ReasonClamped     Reason = "clamped"      // wanted to move but hit a bound
	ReasonAtFloor     Reason = "at_floor"     // too cool but fan already at MIN_FAN — nothing to reclaim
)

// Decision is the learner's output for one class for one slow tick.
type Decision struct {
	NewRampStart int
	Acted        bool
	Reason       Reason
}

// TargetSeek decides how to move a class's curve ramp-start toward holding
// steady-state == target at minimum fan. Pure: the caller supplies the windowed
// observation (robust steady-state temp p50, its stddev, p90 fan demand), the
// current ramp-start, the target, and the tuning Params.
//
// steadyTempC: robust steady-state temperature estimate (use window p50).
// tempStdDev:  windowed temperature standard deviation (settle gate).
// fanDemandP90: windowed p90 fan demand % (saturation gate).
func TargetSeek(steadyTempC, tempStdDev, fanDemandP90 float64, currentRampStart, targetC int, p Params) Decision {
	hold := func(r Reason) Decision { return Decision{NewRampStart: currentRampStart, Acted: false, Reason: r} }

	// Gate 1 — settle. Never act on an in-flight transient; this is what keeps
	// the outer loop decoupled from the plant's dead time.
	if tempStdDev > p.SettleStdDev {
		return hold(ReasonNotSettled)
	}

	errC := steadyTempC - float64(targetC) // >0 too hot, <0 too cool

	stepFor := func(mag float64) int {
		s := int(mag + 0.5) // round
		if s > p.MaxStepC {
			s = p.MaxStepC
		}
		if s < 1 {
			s = 1
		}
		return s
	}

	switch {
	case errC > p.ToleranceHotC:
		// More than the hot tolerance above target → want MORE fan → LOWER
		// ramp-start. Skip if the fan's already saturated (can't deliver more).
		if fanDemandP90 >= p.SatFanP90 {
			return hold(ReasonSaturated)
		}
		nr := clampInt(currentRampStart-stepFor(errC), p.MinRampStart, p.MaxRampStart)
		if nr == currentRampStart {
			return hold(ReasonClamped)
		}
		return Decision{NewRampStart: nr, Acted: true, Reason: ReasonTooHot}

	case errC <= -p.ToleranceCoolC:
		// At/below the cool tolerance under target → using more fan than needed
		// → RAISE ramp-start (reclaim fan, quieter). BUT only if there's fan
		// above the floor to reclaim: if demand is already at MIN_FAN, raising
		// ramp-start buys zero quiet and just lifts the cooling onset — the
		// ratchet that drifted comfort to the ceiling. Hold instead.
		if fanDemandP90 <= p.MinFanFloor {
			return hold(ReasonAtFloor)
		}
		nr := clampInt(currentRampStart+stepFor(-errC), p.MinRampStart, p.MaxRampStart)
		if nr == currentRampStart {
			return hold(ReasonClamped)
		}
		return Decision{NewRampStart: nr, Acted: true, Reason: ReasonTooCool}

	default:
		// Within the asymmetric band [target-cool, target+hot] → leave it.
		return hold(ReasonInTolerance)
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
