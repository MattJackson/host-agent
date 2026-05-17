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
		MinSafe:       30,
		PreferredLow:  65,
		PreferredMid:  72,
		PreferredHigh: 80,
		MaxSafe:       85,
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
