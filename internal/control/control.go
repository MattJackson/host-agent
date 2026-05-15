// Package control implements the pure control-math functions used by
// the dell-fans controller: per-class PID step, proximity-to-emergency
// floor, EWMA baseline update, and active-GPU intake-air assist.
//
// Everything here is a deterministic function of its inputs — no I/O,
// no time, no globals — so it can be unit tested exhaustively.
package control

import (
	"math"
)

// PIDParams are the per-class inputs to StepPID. Gains (FanGain,
// DerivativeGain) are shared across all classes; the rest vary per class.
type PIDParams struct {
	// Temp is this cycle's max temperature for the class.
	// Temp <= 0 means "no reading" — abstain (return 0).
	Temp int
	// Target is the class's deadband center.
	Target int
	// Deadband is the half-width of the happy zone (±°C).
	Deadband int
	// LastTemp is the previous cycle's reading; -1 = no prior (first
	// cycle, or class just came online), in which case D-term is 0.
	LastTemp int
	// CurrentSpeed is the current chassis fan setpoint (% of max).
	CurrentSpeed int
	// MinFan / MaxFan are the legal output range — candidate is clamped
	// to this range before being returned. Drift toward MinFan happens
	// when the class is in deadband at-or-below-target.
	MinFan int
	MaxFan int
	// FanGain is P: % fan per °C error.
	FanGain float64
	// DerivativeGain is D: % fan per °C/cycle rate-of-change.
	DerivativeGain float64
	// DeadbandDriftRate is the per-cycle drift toward MinFan when the
	// class is in deadband at-or-below-target.
	DeadbandDriftRate int
}

// StepPID runs one class's PID step and returns the candidate fan speed
// for the cycle. The main loop takes max() across all class candidates
// + proximity floors + active-GPU assist; this function only computes
// the candidate for ONE class.
//
// Behavior (matches bash exactly):
//   - Temp <= 0: return 0 (abstain — don't bind max() at any speed).
//   - At-or-below-target and inside deadband: drift toward MinFan by
//     DeadbandDriftRate, clamped to MinFan. NOT toward base — see bash
//     comment for the stuck-at-base trap that caused.
//   - Otherwise: step = error * FanGain + d_temp * DerivativeGain,
//     rounded half-away-from-zero, then candidate = clamp(
//     CurrentSpeed + step, MinFan, MaxFan).
func StepPID(p PIDParams) int {
	if p.Temp <= 0 {
		return 0
	}

	error := p.Temp - p.Target
	dTemp := 0
	if p.LastTemp >= 0 {
		dTemp = p.Temp - p.LastTemp
	}

	// Asymmetric deadband: at-or-below-target AND |error| <= deadband.
	if error <= 0 && abs(error) <= p.Deadband {
		cand := p.CurrentSpeed - p.DeadbandDriftRate
		if cand < p.MinFan {
			cand = p.MinFan
		}
		return cand
	}

	// step = e*P + d*D, rounded half-away-from-zero.
	stepF := float64(error)*p.FanGain + float64(dTemp)*p.DerivativeGain
	step := roundHalfAway(stepF)
	cand := p.CurrentSpeed + step
	return clamp(cand, p.MinFan, p.MaxFan)
}

// ProximityFloor computes the per-class proximity-to-emergency floor.
// Returns minFan when temp is at or below (emergency - window); ramps
// linearly to maxFan as temp climbs to emergency; clamps to maxFan if
// above. Result is rounded half-away-from-zero to an int %.
//
// Bash's awk: round-up-from-0.5 — but since the linear ramp always
// produces positive numbers in [minFan, maxFan], "half-away" and
// "half-up" agree. We use roundHalfAway for consistency.
//
// temp <= 0 is not handled here — the caller gates with `temp > 0`
// per the bash original. The math here assumes temp > 0; with temp == 0
// it would still return minFan because diff = -emergency+window is
// negative.
func ProximityFloor(temp, emergency, window, minFan, maxFan int) int {
	if window <= 0 {
		// Degenerate: no ramp — at-or-above emergency is full, below is min.
		if temp >= emergency {
			return maxFan
		}
		return minFan
	}
	diff := temp - (emergency - window)
	if diff <= 0 {
		return minFan
	}
	span := float64(maxFan - minFan)
	f := float64(minFan) + (float64(diff)/float64(window))*span
	if f > float64(maxFan) {
		f = float64(maxFan)
	}
	if f < float64(minFan) {
		f = float64(minFan)
	}
	return int(math.Floor(f + 0.5))
}

// Ewma returns the new EWMA baseline: (1-alpha)*prev + alpha*sample.
// alpha=0.001 → ~half-life of 693 cycles ≈ 24-48hr at 15-30s INTERVAL.
//
// Bash uses awk's "%.4f" — we return float64 and let the state writer
// format. Equivalent to passing through awk's intermediate precision.
func Ewma(prev, sample, alpha float64) float64 {
	return (1.0-alpha)*prev + alpha*sample
}

// ActiveGPUAssist returns the chassis floor lift caused by an active
// GPU exceeding its target. Returns 0 when temp <= target — no assist
// needed.
//
// When temp > target:
//
//	assist = MinFan + round(overshoot * AssistGain), clamped to MaxFan.
//
// "Assist" semantics: chassis fans can't cool the active GPU's die
// directly, but they can cool the intake air the card's own fan pulls
// in. The lift contributes to the max()-wins binding in the main loop.
func ActiveGPUAssist(temp, target int, assistGain float64, minFan, maxFan int) int {
	if temp <= target {
		return 0
	}
	overshoot := temp - target
	lift := roundHalfAway(float64(overshoot) * assistGain)
	cand := minFan + lift
	return clamp(cand, minFan, maxFan)
}

// MaxWinsResult is the outcome of the max-across-candidates aggregation
// the main loop runs every cycle. NewSpeed is the bound setpoint;
// Source records which class/floor contributed it ("cpu", "pg",
// "cpu_pf", "ag_assist", etc.) for the log line.
type MaxWinsResult struct {
	NewSpeed int
	Source   string
}

// MaxCandidate is one entry into the max() aggregation. Name is the
// binding-source string the bash original logs (e.g. "cpu", "pg_pf",
// "ag_assist"); Value is the candidate fan speed in %.
type MaxCandidate struct {
	Name  string
	Value int
}

// MaxWins runs the bash-equivalent loop: start with the first candidate
// as the initial choice, then for each remaining candidate replace if
// strictly greater. Bash's `for pair in ...; do ... -gt ...` semantics:
// strict greater, so ties don't change the source.
func MaxWins(initial MaxCandidate, candidates []MaxCandidate, minFan, maxFan int) MaxWinsResult {
	newSpeed := initial.Value
	src := initial.Name
	for _, c := range candidates {
		if c.Value > newSpeed {
			newSpeed = c.Value
			src = c.Name
		}
	}
	newSpeed = clamp(newSpeed, minFan, maxFan)
	return MaxWinsResult{NewSpeed: newSpeed, Source: src}
}

// roundHalfAway implements bash's `(s >= 0 ? s + 0.5 : s - 0.5)` truncation
// rule — round half-away-from-zero. Go's math.Round already does this.
func roundHalfAway(f float64) int {
	return int(math.Round(f))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
