// Package mode encodes operator intent for the adaptive controller.
// HOST_AGENT_MODE expresses what the operator wants — coolest hardware,
// quietest fans, balanced, or minimum total power — and the controller
// translates that into per-class temperature targets via the envelope
// package. See docs/adaptive-controller-v2.md §4 + §8.
package mode

import (
	"math"

	"github.com/pq/docker-server/host-agent/internal/envelope"
)

// Mode is the operator's intent. One of four enumerated values.
type Mode string

const (
	MaxCool  Mode = "max-cool"
	Balanced Mode = "balanced"
	MinNoise Mode = "min-noise"
	Eco      Mode = "eco"
)

// Default is what callers should use when HOST_AGENT_MODE is unset.
const Default = Balanced

// All returns every valid Mode value, in stable order.
func All() []Mode {
	return []Mode{MaxCool, Balanced, MinNoise, Eco}
}

// Valid reports whether m is one of the four known modes.
func (m Mode) Valid() bool {
	switch m {
	case MaxCool, Balanced, MinNoise, Eco:
		return true
	}
	return false
}

// String returns the canonical lowercase-kebab string for the mode.
func (m Mode) String() string {
	return string(m)
}

// InitialTarget returns the initial PID setpoint and deadband for the
// given envelope under the given mode. Per design §8.
//
//	max-cool:   target = PreferredLow,  deadband = 2
//	balanced:   target = PreferredMid,  deadband = 3
//	min-noise:  target = PreferredHigh, deadband = 4
//	eco:        target = PreferredHigh, deadband = 5
//
// If mode is invalid, falls back to Balanced.
func InitialTarget(env envelope.Envelope, m Mode) (target, deadband int) {
	switch m {
	case MaxCool:
		return env.PreferredLow, 2
	case Balanced:
		return env.PreferredMid, 3
	case MinNoise:
		return env.PreferredHigh, 4
	case Eco:
		return env.PreferredHigh, 5
	}
	// Fallback for invalid mode — caller's parser should have rejected
	// earlier, but defensive default.
	return env.PreferredMid, 3
}

// WindowStats is the input to a score function. It mirrors the fields
// adaptive.Observer will emit. Defined here (not in adaptive package)
// to keep mode unaware of the observer's storage layout.
//
// The adaptive package will pass a value of this shape to Score().
type WindowStats struct {
	TempMean      float64
	TempStdDev    float64
	TempP10       float64
	TempP50       float64
	TempP90       float64
	FanDemandMean float64
	FanChangeRate float64 // changes per minute
	InletMean     float64
	InletStdDev   float64
	Samples       int
}

// ScoreFunc evaluates how well a temperature distribution satisfies the
// mode's intent. Lower is better. Real implementations land in T11.
type ScoreFunc func(env envelope.Envelope, stats WindowStats) float64

// Score returns the mode's score function. T11 will replace stubs with
// real bodies (design §8).
func (m Mode) Score() ScoreFunc {
	switch m {
	case MaxCool:
		return scoreMaxCool
	case MinNoise:
		return scoreMinNoise
	case Eco:
		return scoreEco
	}
	// Balanced is the default.
	return scoreBalanced
}

// The four score functions translate mode intent + envelope into a
// "lower-is-better" objective over a temperature distribution. The
// reconciler evaluates the score at the current observed TempMean and
// at synthesized projections (TempMean ± DriftRatePerCycle), then drifts
// the class target toward the lower score.
//
// All four are satisficing over the envelope's preferred band rather
// than optimizing toward a single point. Anywhere inside
// [PreferredLow, PreferredHigh] scores zero on the band-violation term
// (mode-specific tilt may apply); outside the band, the magnitude
// grows linearly with distance. This is the key fix vs v0.3.1, where
// scoreBalanced was a deviation-from-PreferredMid optimizer that would
// drag target into PID-saturation territory whenever observed mean
// sat anywhere off PreferredMid — even when temp was well within safe
// operating range.

// saturationPenalty grows quadratically above 90% mean fan demand:
// 0 at ≤90, 25 at 95, 100 at 100. A fan stuck near MaxFan is the
// worst observable state regardless of how "settled" temp looks on
// the variance/fan-change-rate terms — those go LOW under sustained
// saturation because the output is pinned, not because the system is
// happy. Pre-v0.3.7 the score functions were blind to this: a P4
// running 78°C in-band with chassis fans pinned at 100 scored
// identically to one at 78°C with fans cruising at 40, so adaptive
// never drifted target up to a fan-relieving point. Penalty weight
// varies per mode (max-cool tolerates saturation by intent; min-noise
// abhors it).
func saturationPenalty(fanDemandMean float64) float64 {
	excess := math.Max(0, fanDemandMean-90.0) / 10.0
	return excess * excess * 100.0
}

func scoreMaxCool(env envelope.Envelope, s WindowStats) float64 {
	// Lean toward PreferredLow. Score grows linearly above PreferredLow;
	// below it (the box is already as cool as max-cool wants), score
	// reduces to variance only so projections settle.
	//
	// Saturation penalty has weight 1.0 — max-cool tolerates saturation
	// somewhat (the intent IS maximum cooling) but a sustained 100%
	// fan above PreferredLow is the signal that PreferredLow is
	// unachievable for current load. Small nudge upward beats spinning
	// at MaxFan forever.
	aboveLow := math.Max(0, s.TempMean-float64(env.PreferredLow))
	variance := s.TempStdDev * s.TempStdDev
	return aboveLow + 0.5*variance + 1.0*saturationPenalty(s.FanDemandMean)
}

func scoreBalanced(env envelope.Envelope, s WindowStats) float64 {
	// Satisficing: any temp inside [PreferredLow, PreferredHigh] is
	// equally fine. Outside the band, penalize distance. This keeps
	// adaptive from chasing in-band drift that would push the PID into
	// saturation for no thermal benefit.
	//
	// Saturation penalty (weight 5.0): at FanDemandMean=95 the penalty
	// is 125 — dominant over a ±1°C bandViolation — so any sustained
	// fan saturation drives target up regardless of where mean sits in
	// the band. This is the bugfix for the "fan stuck at 100 while
	// temp is comfortably at 78" pattern, where mean-only scoring saw
	// "in-band, low variance, low fan-change" and reported "settled".
	bandViolation := bandDistance(s.TempMean, float64(env.PreferredLow), float64(env.PreferredHigh))
	variance := s.TempStdDev * s.TempStdDev
	return bandViolation + 0.3*variance + 0.3*s.FanChangeRate + 5.0*saturationPenalty(s.FanDemandMean)
}

func scoreMinNoise(env envelope.Envelope, s WindowStats) float64 {
	// Lean toward PreferredHigh (warmer = quieter). Score shrinks as
	// mean approaches PreferredHigh; hard penalty (5x) for crossing
	// above it. Below PreferredLow is unusual in min-noise — the box
	// is so cool there's nothing to do — and contributes only via
	// variance.
	//
	// Saturation penalty (weight 10.0): min-noise is the explicit
	// opposite of saturation — heavy weight forces target as high as
	// envelope allows when fans are pinned. A min-noise host should
	// never sit at 100% fan; if it does, target is wrong.
	belowHigh := math.Max(0, float64(env.PreferredHigh)-s.TempMean)
	aboveHigh := math.Max(0, s.TempMean-float64(env.PreferredHigh))
	variance := s.TempStdDev * s.TempStdDev
	return belowHigh + 5.0*aboveHigh + 2.0*s.FanChangeRate + 0.5*variance + 10.0*saturationPenalty(s.FanDemandMean)
}

func scoreEco(env envelope.Envelope, s WindowStats) float64 {
	return scoreMinNoise(env, s)
}

// bandDistance returns 0 when x is inside [lo, hi]; otherwise the
// linear distance to the nearest band edge.
func bandDistance(x, lo, hi float64) float64 {
	if x < lo {
		return lo - x
	}
	if x > hi {
		return x - hi
	}
	return 0
}
