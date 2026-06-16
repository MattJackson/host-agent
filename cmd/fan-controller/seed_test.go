package main

import (
	"path/filepath"
	"testing"

	"github.com/pq/docker-server/host-agent/internal/adaptive"
	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/livetargets"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// reconcilerWithState builds a reconciler whose persisted state holds the
// given per-class (target, deadband), so we can exercise the startup seed.
func reconcilerWithState(t *testing.T, m mode.Mode, classes map[envelope.Class][2]int) *adaptive.Reconciler {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "adaptive.json")
	st := adaptive.NewState(m)
	for c, td := range classes {
		st.Classes[c] = adaptive.ClassState{
			TargetCelsius:   float64(td[0]),
			DeadbandCelsius: float64(td[1]),
		}
	}
	if err := adaptive.SaveState(statePath, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	r, err := adaptive.NewReconciler(adaptive.ReconcilerOptions{
		Observer:   adaptive.NewObserver(480, 10),
		Mode:       m,
		StatePath:  statePath,
		WindowSize: 480,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	return r
}

// TestSeedLiveTargets_AppliesLearnedValuesAtStartup is the v0.3.13 regression
// for the post-restart relearn window: the PID must pick up the reconciler's
// PERSISTED target/deadband on cycle 1, not the mode-initial values.
func TestSeedLiveTargets_AppliesLearnedValuesAtStartup(t *testing.T) {
	// Persisted learned state: PassiveGPU settled at 85/7 (wider than the
	// min-noise initial 83/4).
	r := reconcilerWithState(t, mode.MinNoise, map[envelope.Class][2]int{
		envelope.PassiveGPU: {85, 7},
	})
	lt := livetargets.New()
	seedLiveTargets(r, lt, nil)

	// cfg starts at the mode-initial values the PID would otherwise use.
	cfg := &config.Config{GPUTarget: 83, GPUDeadband: 4}
	lt.ApplyTo(cfg)

	if cfg.GPUTarget != 85 || cfg.GPUDeadband != 7 {
		t.Errorf("after seed+ApplyTo: GPUTarget/Deadband = %d/%d, want 85/7 (learned values, not mode-initial)",
			cfg.GPUTarget, cfg.GPUDeadband)
	}
}

// TestSeedLiveTargets_SkipsOverriddenClass verifies an operator-pinned class
// is NOT seeded, so its override stands (mirrors the reconcile loop).
func TestSeedLiveTargets_SkipsOverriddenClass(t *testing.T) {
	r := reconcilerWithState(t, mode.MinNoise, map[envelope.Class][2]int{
		envelope.PassiveGPU: {85, 7},
	})
	lt := livetargets.New()
	seedLiveTargets(r, lt, map[envelope.Class]bool{envelope.PassiveGPU: true})

	cfg := &config.Config{GPUTarget: 80, GPUDeadband: 2} // operator-pinned values
	lt.ApplyTo(cfg)

	if cfg.GPUTarget != 80 || cfg.GPUDeadband != 2 {
		t.Errorf("overridden class was seeded: GPUTarget/Deadband = %d/%d, want 80/2 (override preserved)",
			cfg.GPUTarget, cfg.GPUDeadband)
	}
}

// TestSeedLiveTargets_NilReconciler is a no-op (adaptive disabled path).
func TestSeedLiveTargets_NilReconciler(t *testing.T) {
	lt := livetargets.New()
	seedLiveTargets(nil, lt, nil) // must not panic
	cfg := &config.Config{GPUTarget: 83, GPUDeadband: 4}
	lt.ApplyTo(cfg)
	if cfg.GPUTarget != 83 {
		t.Errorf("nil reconciler should not change cfg, got GPUTarget=%d", cfg.GPUTarget)
	}
}
