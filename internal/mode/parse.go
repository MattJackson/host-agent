package mode

import (
	"fmt"
	"os"
	"strings"
)

// envVar is the operator-facing environment variable name.
const envVar = "HOST_AGENT_MODE"

// Parse reads HOST_AGENT_MODE from the environment and returns the
// resolved mode plus whether it was explicitly set. Empty/unset returns
// (Default, false, nil). Invalid value returns (Default, true, error).
//
// Accepts lowercase, uppercase, and mixed-case forms; trims whitespace.
// "max-cool" and "max_cool" and "maxcool" all resolve to MaxCool.
func Parse() (m Mode, set bool, err error) {
	raw, ok := os.LookupEnv(envVar)
	if !ok || strings.TrimSpace(raw) == "" {
		return Default, false, nil
	}
	set = true
	norm := normalize(raw)
	switch norm {
	case "maxcool", "max-cool", "max_cool":
		return MaxCool, set, nil
	case "balanced":
		return Balanced, set, nil
	case "minnoise", "min-noise", "min_noise":
		return MinNoise, set, nil
	case "eco":
		return Eco, set, nil
	}
	return Default, set, fmt.Errorf("%s=%q: not one of max-cool, balanced, min-noise, eco", envVar, raw)
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
