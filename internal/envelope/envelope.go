// Package envelope encodes the per-class hardware temperature envelopes
// the adaptive controller uses to derive targets from operator intent
// (HOST_AGENT_MODE). Each Envelope describes one thermal class's
// preferred operating range plus safety bounds. Defaults are compiled
// in; per-chassis profiles can override individual fields.
package envelope

import "fmt"

// Class identifies a thermal class. Values match v1 controller class names.
type Class string

const (
	CPU        Class = "cpu"
	PassiveGPU Class = "passive_gpu"
	ActiveGPU  Class = "active_gpu"
	HDD        Class = "hdd"
	SSD        Class = "ssd"
)

// Envelope is the per-class temperature envelope: safety bounds +
// preferred operating range. All values are degrees Celsius.
// Invariant: MinSafe < PreferredLow <= PreferredMid <= PreferredHigh < MaxSafe < Emergency.
type Envelope struct {
	MinSafe       int // below this is suspect (sensor fault)
	PreferredLow  int // "max-cool" target
	PreferredMid  int // "balanced" target
	PreferredHigh int // "min-noise" target
	MaxSafe       int // upper bound for adaptive drift (never targeted above this)
	Emergency     int // immediate full fans
}

// Valid checks the ordering invariant.
func (e Envelope) Valid() bool {
	return e.MinSafe < e.PreferredLow &&
		e.PreferredLow <= e.PreferredMid &&
		e.PreferredMid <= e.PreferredHigh &&
		e.PreferredHigh < e.MaxSafe &&
		e.MaxSafe < e.Emergency
}

func (c Class) String() string {
	return string(c)
}

// DefaultEnvelopes encodes the per-class hardware temperature envelopes
// the adaptive controller uses to derive targets from operator intent
// (HOST_AGENT_MODE). Only four classes are included: CPU, PassiveGPU, HDD, SSD.
// ActiveGPU is excluded because it has its own fans and doesn't use chassis-targeted cooling.
var DefaultEnvelopes = map[Class]Envelope{
	CPU: {
		MinSafe:       20,
		PreferredLow:  55,
		PreferredMid:  65,
		PreferredHigh: 75,
		MaxSafe:       85,
		Emergency:     90,
	},
	PassiveGPU: {
		// Passive datacenter cards (e.g. Tesla P4/P40) are spec'd to run
		// hot — the P4 throttles ~91°C, shuts down ~95°C. Holding one at
		// 80°C costs 87-99% chassis fan for no thermal benefit. Tuned up
		// (v0.3.11) so min-noise lets the card settle ~84-85°C with much
		// quieter fans, keeping ~6°C of throttle margin. Emergency stays
		// at 90 (~1°C below the hardware thermal-slowdown point) as the
		// hard backstop.
		MinSafe:       30,
		PreferredLow:  75,
		PreferredMid:  80,
		PreferredHigh: 83,
		MaxSafe:       86, // adaptive drift ceiling = MaxSafe-1 = 85
		Emergency:     90,
	},
	HDD: {
		MinSafe:       10,
		PreferredLow:  32,
		PreferredMid:  38,
		PreferredHigh: 43,
		MaxSafe:       45,
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

// Get returns the default envelope for a given class or an error if not found.
func Get(c Class) (Envelope, error) {
	e, ok := DefaultEnvelopes[c]
	if !ok {
		return Envelope{}, fmt.Errorf("unknown thermal class: %s", c)
	}
	return e, nil
}

// MinCurveSpanC is the per-class MINIMUM width, in °C, of the fan curve's ramp
// (comfort → emergency). The curve's proportional gain is
// (MaxFan-MinFan)/span %/°C, so the span is what bounds how hard sensor noise
// is amplified into fan movement. Without a floor on it the learner can
// legitimately place comfort one degree under emergency, collapsing the ramp to
// a few °C: on a dual-socket R730xd in min-noise mode (target 76, emergency 80)
// a 4°C ramp gave ~22%/°C, and ±2°C of hottest-core noise swung the fans
// through their whole range every minute at 2% CPU load.
//
// Values are sized to each class's sensor noise and plant speed, so that
// ordinary noise (after the controller's input smoothing) moves the fan by a
// few %, not tens of %:
//   - CPU 10: the input is the single hottest core across every package,
//     1°C-quantized and noisy by ±2°C cycle to cycle, on a fast plant. 10°C
//     → ≤9%/°C at MIN_FAN 10.
//   - PassiveGPU 8: one on-die sensor, typically ±1°C, behind a heavy passive
//     heatsink (slow plant) — less noise to reject than CPU, and the
//     hot-tolerant modes sit only 7°C under a 90°C emergency, so 10 would force
//     comfort well below target. 8°C → ~11%/°C.
//   - HDD 6 / SSD 6: SMART temps are 1°C-quantized and polled slowly; the
//     plants are the slowest in the box, so a narrower ramp stays calm, and the
//     HDD envelope is only ~7°C from min-noise target to emergency.
//
// The effective ceiling on a class's ramp-start is therefore
// emergency - MinCurveSpanC (see MaxRampStart). A mode whose TARGET sits closer
// to emergency than this still honours the span: the curve begins adding fan
// below target, which is the price of not hunting.
var MinCurveSpanC = map[Class]int{
	CPU:        10,
	PassiveGPU: 8,
	HDD:        6,
	SSD:        6,
}

// defaultMinCurveSpanC is used for a class without an explicit entry.
const defaultMinCurveSpanC = 10

// MinCurveSpan returns MinCurveSpanC for c (defaultMinCurveSpanC if absent).
func MinCurveSpan(c Class) int {
	if s, ok := MinCurveSpanC[c]; ok && s > 0 {
		return s
	}
	return defaultMinCurveSpanC
}

// MaxRampStart returns the highest curve comfort (ramp-start) allowed for class
// c given its configured emergency: emergency - MinCurveSpan(c).
func MaxRampStart(c Class, emergency int) int {
	return emergency - MinCurveSpan(c)
}
