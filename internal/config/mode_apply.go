package config

import (
	"os"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// ApplyMode bridges HOST_AGENT_MODE (v2 intent) into the v1-shaped
// Config struct. Behavior depends on whether the operator opted into
// v2 by setting HOST_AGENT_MODE:
//
//   - HOST_AGENT_MODE unset → no-op. cfg keeps whatever Load() resolved
//     (profile defaults + per-class env-var overrides). This is the
//     pure v1 path — every host on the prior release lands here.
//
//   - HOST_AGENT_MODE set → mode-derived per-class target+deadband
//     replace cfg's profile-loaded values, EXCEPT for classes where
//     the operator explicitly set a per-class env var
//     (CPU_TARGET / GPU_TARGET / HDD_TARGET / SSD_TARGET). Profile
//     entries DO NOT count as overrides in v2 — they're treated as v1
//     fallback defaults that yield to mode-derived values. Operators
//     who want a chassis-specific fixed target set it via env var (or
//     compose .env), not in the profile file.
//
// Returns the resolved mode, whether HOST_AGENT_MODE was explicitly
// set, and any parse error from mode.Parse.
//
// See design doc §4 (user-facing API) + §14 (migration matrix).
func ApplyMode(cfg *Config) (resolved mode.Mode, set bool, err error) {
	resolved, set, err = mode.Parse()
	if !set {
		// v1 behavior — profile-loaded values are authoritative.
		return resolved, set, err
	}

	type classBinding struct {
		targetEnvKey   string
		deadbandEnvKey string
		class          envelope.Class
		target         *int
		deadband       *int
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
		// Per-class env-var override wins. Profile-set values are
		// not overrides in v2 — they're replaced by mode-derived
		// values when HOST_AGENT_MODE is set.
		if _, envSet := os.LookupEnv(b.targetEnvKey); !envSet {
			*b.target = t
		}
		if _, envSet := os.LookupEnv(b.deadbandEnvKey); !envSet {
			*b.deadband = d
		}
	}
	return resolved, set, err
}
