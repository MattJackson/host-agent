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
// Behavior (three-zone thermostat):
//   - Temp <= 0: return 0 (abstain — don't bind max() at any speed).
//   - Inside the deadband (|temp-target| <= deadband, EITHER side): HOLD the
//     current fan speed (clamped to [MinFan, MaxFan]). Incremental control
//     means holding parks the fan at the equilibrium-maintaining speed; see
//     the symmetric-deadband note in the body for why this replaced the old
//     coast-to-MinFan (which caused a limit cycle on tight envelopes).
//   - Otherwise: step = error * FanGain + d_temp * DerivativeGain,
//     rounded half-away-from-zero, then candidate = clamp(
//     CurrentSpeed + step, MinFan, MaxFan). Above the band this ramps up;
//     below the band (cool/idle host) it steps down toward MinFan.
func StepPID(p PIDParams) int {
	if p.Temp <= 0 {
		return 0
	}

	error := p.Temp - p.Target
	dTemp := 0
	if p.LastTemp >= 0 {
		dTemp = p.Temp - p.LastTemp
	}

	// Symmetric deadband: anywhere within ±deadband of target, HOLD the
	// current fan speed — don't ramp, don't coast.
	//
	// This is the three-zone thermostat the controller is supposed to be:
	//   - temp > target+deadband  → P+D ramps fans UP (below)
	//   - temp within ±deadband   → hold (we're within tolerance; leave it)
	//   - temp < target-deadband  → P+D steps fans DOWN (below), so a
	//     genuinely cool/idle host still eases to MinFan.
	// Because this is an incremental controller (cand = CurrentSpeed + step),
	// holding inside the band naturally parks the fan at the speed that
	// maintains equilibrium, instead of fighting it.
	//
	// History: earlier versions coasted the candidate DOWN toward MinFan
	// inside the band, relying on the proximity floor to act as a smooth
	// governor. That only works when the floor's ramp overlaps the band —
	// which it does NOT for tight envelopes like HDD (emergency 50, window 5
	// → ramp starts at 45, but the band around a 44 target is 43–45). There
	// the collapse-to-MinFan had no governor under it, so the fan dropped to
	// minimum, the drive heated until it popped out of the band, the PID
	// slammed it back, and it oscillated — the "fans all over the place at a
	// steady temperature" limit cycle. Holding kills that wave at the source:
	// a little over target = ramp gently and tolerate; within tolerance =
	// stay put. The proximity floor remains as a pure safety backstop, max()'d
	// in by the caller near the emergency trip.
	//
	// Deadband WIDTH is the per-mode tolerance knob (max-cool 2° … eco 5°):
	// quieter modes tolerate more over-target before reacting, cooler modes
	// react sooner. That — not moving the target — is the intended
	// mode-dependent "how hot before we react" behavior.
	if abs(error) <= p.Deadband {
		return clamp(p.CurrentSpeed, p.MinFan, p.MaxFan)
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
