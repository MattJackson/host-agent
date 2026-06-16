// Package control implements the pure control-math functions used by
// the fan controller: per-class PID step, proximity-to-emergency
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
//   - Inside the deadband (|temp-target| <= deadband, EITHER side): coast
//     toward MinFan by DeadbandDriftRate, clamped to MinFan. The caller
//     max()'s in the smooth proximity floor, which then governs the band
//     (see the symmetric-deadband note in the body). NOT toward base — see
//     bash comment for the stuck-at-base trap that caused.
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

	// Symmetric deadband: anywhere within ±deadband of target, coast the
	// PID candidate DOWN toward MinFan rather than ramping. (v0.3.12 — was
	// asymmetric: it coasted only at-or-below target and ramped the instant
	// temp was 1°C above, which—because the card's load equilibrium sits
	// right at target—made the fan saw-tooth: ramp up at target+1, drift
	// down at target, repeat. That hunt is what made fans "all over the
	// place" at a steady temperature.)
	//
	// Coasting down inside the whole band lets the candidate fall back to
	// the proximity floor, which the caller max()'s in. The proximity floor
	// is a SMOOTH, temperature-proportional curve (MinFan at emergency-window
	// up to MaxFan at emergency) — so within the band the floor becomes the
	// de-facto governor: a smooth, monotonic fan-vs-temp response with a
	// single stable equilibrium, instead of the PID's bang-bang hunt. The
	// high side stays safe because the floor ramps hard as temp approaches
	// emergency and the emergency trip forces MaxFan above it; the PID only
	// re-engages (below) once temp exceeds target+deadband, which for tight
	// envelopes is at/above the emergency trip anyway.
	//
	// The adaptive reconciler widens this deadband when it observes variance,
	// so the "settle zone" grows to absorb bursty load automatically — the
	// learned-damping behavior, now actually honored on both sides.
	if abs(error) <= p.Deadband {
		cand := p.CurrentSpeed - p.DeadbandDriftRate
		if cand < p.MinFan {
			cand = p.MinFan
		}
		return cand
	}

	// Saturation escape: error > 0 AND already at MaxFan AND temp not
	// rising means the PID has nowhere to step (positive P+D would
	// clamp to MaxFan, which is a no-op). Without this branch the
	// candidate stays pinned at MaxFan indefinitely even after load
	// eases, because there's never a cycle that explores "would less
	// fan still hold this equilibrium?". P4 under sustained 95% util
	// equilibrates at ~78°C with chassis fans at 100; target=72 yields
	// error=+6 forever and the controller has no way to learn the
	// equilibrium is actually achievable at fan=85.
	//
	// Drift down by DeadbandDriftRate to probe. If load is genuinely
	// near max cooling capacity the next cycle's P+D step will push
	// back up (dTemp goes positive as fan eases) — self-correcting.
	// The non-rising gate (dTemp <= 0) keeps a real climbing transient
	// from triggering the escape and undoing legitimate ramping.
	//
	// The escape also unblocks the adaptive layer: observed
	// FanDemandMean drops off 100, which makes the reconciler's
	// saturation penalty visible across the up/down projections so
	// target drifts up toward the achievable equilibrium.
	if error > 0 && p.CurrentSpeed >= p.MaxFan && dTemp <= 0 {
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

// ActiveGPUAssist returns the chassis floor lift for an active GPU
// based on the card's OWN fan speed — not die temperature.
//
// The active GPU's own fan is the authoritative signal of whether the
// card needs help: as long as it can self-cool with headroom, chassis
// fans should stay quiet (workstation card own-fans are designed to be
// quieter than chassis fans). Only when the card's fan is near max
// does it need outside help moving heat out of its inlet air.
//
// Returns 0 while ownFan < threshold ("card is self-managing fine").
// At/above threshold, ramps linearly from minFan to maxFan as ownFan
// climbs from threshold → 100.
//
// die_temp is intentionally absent — ACTIVE_GPU_EMERGENCY is the
// temperature-based safety net handled separately in the main loop
// (catches dead-fan failures where ownFan reads normal but temp climbs).
func ActiveGPUAssist(ownFan, threshold, minFan, maxFan int) int {
	if ownFan < threshold {
		return 0
	}
	if threshold >= 100 {
		// Degenerate: only activate at exactly 100% own-fan.
		if ownFan >= 100 {
			return maxFan
		}
		return 0
	}
	if ownFan > 100 {
		ownFan = 100
	}
	span := float64(maxFan - minFan)
	progress := float64(ownFan-threshold) / float64(100-threshold)
	lift := roundHalfAway(progress * span)
	return clamp(minFan+lift, minFan, maxFan)
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
