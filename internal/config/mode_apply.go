package config

import (
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// ApplyMode reads HOST_AGENT_MODE and fills any per-class target /
// deadband that wasn't explicitly set by an env var or profile file.
//
// Precedence (highest first):
//   1. Per-class env var or profile entry (CPU_TARGET=70, etc.)
//   2. Mode-derived target via envelope.DefaultEnvelopes + mode.InitialTarget
//   3. Existing zero value (unchanged)
//
// Per-class explicitness is determined by presence of the key in cfg.Raw.
// If a key is present (any source) we treat it as an explicit override
// and the mode-derived value is NOT applied for that class.
//
// Returns the resolved mode, whether HOST_AGENT_MODE was explicitly set,
// and any parse error from mode.Parse. On parse error, the function
// still applies the fallback (mode.Default = Balanced) so the controller
// can keep running.
//
// See design doc §4 + §14 for the user-facing API + migration matrix.
func ApplyMode(cfg *Config) (resolved mode.Mode, set bool, err error) {
	resolved, set, err = mode.Parse()

	type classBinding struct {
		targetKey   string
		deadbandKey string
		class       envelope.Class
target      *int
	deadband    *int
	}
	bindings := []classBinding{
		{"CPU_TARGET", "CPU_DEADBAND", envelope.CPU, &cfg.CPUTarget, &cfg.CPUDeadband},
		{"GPU_TARGET", "GPU_DEADBAND", envelope.PassiveGPU, &cfg.GPUTarget, &cfg.GPUDeadband},
		{"HDD_TARGET", "HDD_DEADBAND", envelope.HDD, &cfg.HDDTarget, &cfg.HDDDeadband},
		{"SSD_TARGET", "SSD_DEADBAND", envelope.SSD, &cfg.SSDTarget, &cfg.SSDDeadband},
	}
	for _, b := range bindings {
		env, ok := envelope.DefaultEnvelopes[b.class]
		if !ok {
			continue
		}
		t, d := mode.InitialTarget(env, resolved)
		if _, explicit := cfg.Raw[b.targetKey]; !explicit {
			*b.target = t
		}
		if _, explicit := cfg.Raw[b.deadbandKey]; !explicit {
			*b.deadband = d
		}
	}
	return resolved, set, err
}
