// Package mode encodes operator intent for the adaptive controller.
// HOST_AGENT_MODE expresses what the operator wants — coolest hardware,
// quietest fans, balanced, or minimum total power — and the controller
// translates that into per-class temperature targets via the envelope
// package. See docs/adaptive-controller-v2.md §4 + §8.
package mode

import "github.com/pq/docker-server/host-agent/internal/envelope"

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
//   max-cool:   target = PreferredLow,  deadband = 2
//   balanced:   target = PreferredMid,  deadband = 3
//   min-noise:  target = PreferredHigh, deadband = 4
//   eco:        target = PreferredHigh, deadband = 5
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

func scoreMaxCool(env envelope.Envelope, s WindowStats) float64 {
	// TODO(T11): mean(temp) + 0.5 * variance(temp)
	return 0.0
}

func scoreBalanced(env envelope.Envelope, s WindowStats) float64 {
	// TODO(T11): |mean(temp) - PreferredMid| + 0.3*variance + 0.3*fan_change_rate
	return 0.0
}

func scoreMinNoise(env envelope.Envelope, s WindowStats) float64 {
	// TODO(T11): max(0, PreferredHigh - mean(temp)) + 2.0*fan_change_rate + 0.5*variance
	return 0.0
}

func scoreEco(env envelope.Envelope, s WindowStats) float64 {
	// TODO(T11): estimated_total_watts(temp_distribution, fan_change_rate) — needs fan-power model
	return 0.0
}
