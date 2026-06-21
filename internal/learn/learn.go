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
	// ToleranceC: dead zone around TARGET. While |steady-state - target| is
	// within this, do nothing (prevents endless ±1°C twitching at the target).
	ToleranceC float64
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
}

// DefaultParams returns conservative defaults for a slow disk/CPU plant.
func DefaultParams(minRampStart, maxRampStart int) Params {
	return Params{
		ToleranceC:   1.0,
		MaxStepC:     1,
		SettleStdDev: 1.5,
		SatFanP90:    99,
		MinRampStart: minRampStart,
		MaxRampStart: maxRampStart,
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
	if errC < 0 {
		errC = -errC
	}
	if errC <= p.ToleranceC {
		return hold(ReasonInTolerance)
	}

	step := int(errC + 0.5) // round
	if step > p.MaxStepC {
		step = p.MaxStepC
	}
	if step < 1 {
		step = 1
	}

	if steadyTempC > float64(targetC) {
		// Too hot → want MORE fan → LOWER ramp-start.
		if fanDemandP90 >= p.SatFanP90 {
			// Fan already saturated — lowering ramp-start can't deliver more air.
			return hold(ReasonSaturated)
		}
		nr := clampInt(currentRampStart-step, p.MinRampStart, p.MaxRampStart)
		if nr == currentRampStart {
			return hold(ReasonClamped)
		}
		return Decision{NewRampStart: nr, Acted: true, Reason: ReasonTooHot}
	}

	// Too cool → using more fan than needed → RAISE ramp-start (quieter).
	nr := clampInt(currentRampStart+step, p.MinRampStart, p.MaxRampStart)
	if nr == currentRampStart {
		return hold(ReasonClamped)
	}
	return Decision{NewRampStart: nr, Acted: true, Reason: ReasonTooCool}
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
