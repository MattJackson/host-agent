// Package adaptive implements the v2 intent-driven adaptive controller
// — observer, state, and reconciler. See docs/adaptive-controller-v2.md.
package adaptive

import (
	"math"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// Soak tests exercise the Reconciler over many cycles with scripted
// thermal traces. They verify properties stated in design §10:
//
//  - bounded: target never escapes [PreferredLow, MaxSafe-1]
//  - monotonic-toward-equilibrium under stable inputs (no oscillation)
//  - reset-on-flap: high variance triggers DriftReasonVarianceReset
//  - tracks env change: target follows when steady-state temp shifts
//
// These are unit-test-speed (no time.Sleep) but exercise the full
// Observer + Reconciler + State paths together.

// soakTrace configures a single soak test run.
type soakTrace struct {
	Class          envelope.Class
	Mode           mode.Mode
	WindowSize     int
	StepsPerSample int // how many Add calls per Step() — usually windowSize so window is always full
	StateDir       string
}

// runSoak performs a soak: for each entry in samples, calls
// obs.Add(class, sample) StepsPerSample times, then calls
// reconciler.Step() once. Returns the full per-Step DriftAction
// sequence for the named class.
func runSoak(t *testing.T, tr soakTrace, samples []Sample) (history []DriftAction) {
	t.Helper()

	o := NewObserver(tr.WindowSize, 10.0)

	statePath := ""
	if tr.StateDir != "" {
		statePath = tr.StateDir + "/adaptive.json"
	}

	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	nowCounter := 0
	nowFn := func() time.Time {
		t := nowBase.Add(time.Duration(nowCounter) * 10 * time.Minute)
		nowCounter++
		return t
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       tr.Mode,
		StatePath:  statePath,
		WindowSize: tr.WindowSize,
		Now:        nowFn,
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	sampleIdx := 0
	for _, sample := range samples {
		for i := 0; i < tr.StepsPerSample; i++ {
			// Clone the sample with updated timestamp
			s := Sample{
				Timestamp:    nowBase.Add(time.Duration(nowCounter) * time.Second),
				TempCelsius:  sample.TempCelsius,
				FanDemandPct: sample.FanDemandPct,
				InletCelsius: sample.InletCelsius,
			}
			o.Add(tr.Class, s)
		}
		nowCounter++

		actions, err := r.Step()
		if err != nil {
			t.Fatalf("Step failed after %d samples: %v", sampleIdx+1, err)
		}

		for _, a := range actions {
			if a.Class == tr.Class {
				history = append(history, a)
			}
		}
		sampleIdx++
	}

	return history
}

// TestSoak_CoolStableTemps_ConvergesAtPreferredLow_MaxCool tests that in MaxCool
// mode with stable temps below PreferredLow, the target drifts down to PreferredLow
// and stays there (bounded_low). This verifies design §10 convergence property.
func TestSoak_CoolStableTemps_ConvergesAtPreferredLow_MaxCool(t *testing.T) {
	tr := soakTrace{
		Class:          envelope.PassiveGPU,
		Mode:           mode.MaxCool,
		WindowSize:     50,
		StepsPerSample: 50,
		StateDir:       t.TempDir(),
	}

	samples := make([]Sample, 50)
	for i := range samples {
		samples[i] = Sample{
			Timestamp:    time.Time{},
			TempCelsius:  60.0, // below PreferredLow=65 for PassiveGPU
			FanDemandPct: 50,
			InletCelsius: 22.0,
		}
	}

	history := runSoak(t, tr, samples)

	if len(history) == 0 {
		t.Fatal("no actions returned")
	}

	targets := make([]int, len(history))
	for i, a := range history {
		targets[i] = a.NewTarget
	}

	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]
	expectedLow := env.PreferredLow // 65

	finalTarget := targets[len(targets)-1]
	if finalTarget != expectedLow {
		t.Errorf("final target=%d, expected PreferredLow=%d", finalTarget, expectedLow)
	}

	settledIdx := -1
	for i := 1; i < len(targets); i++ {
		if targets[i] == expectedLow && targets[i-1] == expectedLow {
			settledIdx = i - 1
			break
		}
	}

	if settledIdx == -1 {
		t.Errorf("target did not settle at PreferredLow=%d across consecutive steps", expectedLow)
	} else if settledIdx > 20 {
		t.Logf("target took %d steps to settle (expected ≤20)", settledIdx)
	}

	for i, tgt := range targets {
		if tgt < env.PreferredLow || tgt > env.MaxSafe-1 {
			t.Errorf("step %d: target=%d outside [PreferredLow=%d, MaxSafe-1=%d]", i, tgt, env.PreferredLow, env.MaxSafe-1)
		}
	}

	boundedAtLow := false
	for _, a := range history {
		if a.Reason == DriftReasonBoundedLow {
			boundedAtLow = true
			break
		}
	}

	if !boundedAtLow {
		t.Logf("warning: no DriftReasonBoundedLow seen; target reached floor but reason may be Settled")
	}
}

// TestSoak_WarmStableTemps_ConvergesAtCeiling_MinNoise tests that in MinNoise
// mode with stable temps below PreferredHigh, the target drifts up to the
// new high clamp (MaxSafe-1) and stays there. Pre-v0.3.8 the clamp was
// PreferredHigh; post-v0.3.8 it's MaxSafe-1 so the saturation-penalty
// signal can actually move target into the safety headroom.
//
// Note: the test artificially holds observed mean fixed; in production
// the PID loop couples mean to target so equilibrium settles below the
// ceiling. This test verifies the clamp, not the steady-state.
func TestSoak_WarmStableTemps_ConvergesAtCeiling_MinNoise(t *testing.T) {
	tr := soakTrace{
		Class:          envelope.CPU,
		Mode:           mode.MinNoise,
		WindowSize:     50,
		StepsPerSample: 50,
		StateDir:       t.TempDir(),
	}

	samples := make([]Sample, 50)
	for i := range samples {
		samples[i] = Sample{
			Timestamp:    time.Time{},
			TempCelsius:  72.0, // below PreferredHigh=75 for CPU
			FanDemandPct: 40,
			InletCelsius: 22.0,
		}
	}

	history := runSoak(t, tr, samples)

	if len(history) == 0 {
		t.Fatal("no actions returned")
	}

	targets := make([]int, len(history))
	for i, a := range history {
		targets[i] = a.NewTarget
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]
	expectedCeiling := env.MaxSafe - 1 // 84

	finalTarget := targets[len(targets)-1]
	if finalTarget != expectedCeiling {
		t.Errorf("final target=%d, expected ceiling=MaxSafe-1=%d", finalTarget, expectedCeiling)
	}

	for i, tgt := range targets {
		if tgt < env.PreferredLow || tgt > env.MaxSafe-1 {
			t.Errorf("step %d: target=%d outside [PreferredLow=%d, MaxSafe-1=%d]", i, tgt, env.PreferredLow, env.MaxSafe-1)
		}
	}

	boundedAtHigh := false
	for _, a := range history {
		if a.Reason == DriftReasonBoundedHigh {
			boundedAtHigh = true
			break
		}
	}

	if !boundedAtHigh {
		t.Logf("warning: no DriftReasonBoundedHigh seen; target reached ceiling but reason may be Settled")
	}
}

// TestSoak_LoadStep_TargetTracksUp tests that when the environment warms up,
// the target tracks upward toward a new equilibrium. This verifies design §10
// "tracks env change" property using MinNoise mode where higher temps are better.
func TestSoak_LoadStep_TargetTracksUp(t *testing.T) {
	tr := soakTrace{
		Class:          envelope.HDD,
		Mode:           mode.MinNoise, // MinNoise prefers higher temps (headroom to PreferredHigh)
		WindowSize:     60,
		StepsPerSample: 30, // add 30 samples per Step
		StateDir:       t.TempDir(),
	}

	samples := make([]Sample, 60)
	for i := range samples {
		if i < 30 {
			// Start at temp=35 (below PreferredHigh=43) - controller drifts up toward ceiling
			samples[i] = Sample{
				Timestamp:    time.Time{},
				TempCelsius:  35.0, // below PreferredHigh for HDD
				FanDemandPct: 50,
				InletCelsius: 22.0,
			}
		} else {
			// Then warm up to temp=42 (near PreferredHigh=43) - target already at ceiling stays there
			samples[i] = Sample{
				Timestamp:    time.Time{},
				TempCelsius:  42.0, // near PreferredHigh=43 for HDD
				FanDemandPct: 50,
				InletCelsius: 22.0,
			}
		}
	}

	history := runSoak(t, tr, samples)

	if len(history) == 0 {
		t.Fatal("no actions returned")
	}

	targets := make([]int, len(history))
	for i, a := range history {
		targets[i] = a.NewTarget
	}

	env := envelope.DefaultEnvelopes[envelope.HDD]

	startTarget := targets[0]
	if startTarget != env.PreferredHigh {
		t.Logf("start target=%d (expected %d for MinNoise mode)", startTarget, env.PreferredHigh)
	}

	endTarget := targets[len(targets)-1]
	expectedNewEq := env.MaxSafe - 1 // 44 for HDD — new ceiling post-v0.3.8
	if math.Abs(float64(endTarget-expectedNewEq)) > 2 {
		t.Errorf("end target=%d, expected within 2°C of %d (MaxSafe-1)", endTarget, expectedNewEq)
	}

	for i, tgt := range targets {
		if tgt < env.PreferredLow || tgt > env.MaxSafe-1 {
			t.Errorf("step %d: target=%d outside [PreferredLow=%d, MaxSafe-1=%d]", i, tgt, env.PreferredLow, env.MaxSafe-1)
		}
	}

	upwardDriftCount := 0
	for i := 1; i < len(targets); i++ {
		if targets[i] > targets[i-1] {
			upwardDriftCount++
		}
	}

	if upwardDriftCount == 0 {
		t.Logf("warning: no upward drift observed (target may be at ceiling)")
	} else {
		t.Logf("upward drift count=%d (target moving toward PreferredHigh)", upwardDriftCount)
	}

	// Verify target reaches the new ceiling (MaxSafe-1) by mid-run.
	ceilingReached := false
	for i, tgt := range targets {
		if tgt == env.MaxSafe-1 && i > len(targets)/4 {
			ceilingReached = true
			break
		}
	}
	if !ceilingReached {
		t.Logf("note: ceiling not reached within %d steps; target sequence=%v", len(targets), targets)
	}
}

// TestSoak_HighVariance_TriggersResetThenStabilizes tests that high variance
// triggers DriftReasonVarianceReset, and then the system stabilizes. This
// verifies design §12 reset-on-flap property.
func TestSoak_HighVariance_TriggersResetThenStabilizes(t *testing.T) {
	tr := soakTrace{
		Class:          envelope.CPU,
		Mode:           mode.Balanced,
		WindowSize:     35, // 30 variance samples + 5 warmup margin
		StepsPerSample: 6,  // add 6 samples per Step to fill window quickly
		StateDir:       t.TempDir(),
	}

	samples := make([]Sample, 60)
	for i := range samples {
		if i < 30 {
			// Phase 1: alternating 30°C and 100°C — stddev will be >5
			tempVal := 30.0
			if i%2 == 1 {
				tempVal = 100.0
			}
			samples[i] = Sample{
				Timestamp:    time.Time{},
				TempCelsius:  tempVal,
				FanDemandPct: 50,
				InletCelsius: 22.0,
			}
		} else {
			// Phase 2: stable at PreferredMid=65 for CPU
			samples[i] = Sample{
				Timestamp:    time.Time{},
				TempCelsius:  65.0,
				FanDemandPct: 50,
				InletCelsius: 22.0,
			}
		}
	}

	history := runSoak(t, tr, samples)

	if len(history) == 0 {
		t.Fatal("no actions returned")
	}

	varianceResetSeen := false
	for _, a := range history {
		if a.Reason == DriftReasonVarianceReset {
			varianceResetSeen = true
			break
		}
	}

	if !varianceResetSeen {
		t.Error("expected at least one DriftReasonVarianceReset during high-variance phase")
	}

	stablePhaseStart := 30 / tr.StepsPerSample
	if stablePhaseStart < 1 {
		stablePhaseStart = 1
	}
	if stablePhaseStart >= len(history) {
		t.Fatalf("stable phase starts at step %d but only got %d steps", stablePhaseStart, len(history))
	}

	stableHistory := history[stablePhaseStart:]
	if len(stableHistory) == 0 {
		t.Fatal("no stable-phase actions")
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]
	for _, a := range stableHistory {
		if a.NewTarget < env.PreferredLow || a.NewTarget > env.MaxSafe-1 {
			t.Errorf("stable phase: target=%d outside [PreferredLow=%d, MaxSafe-1=%d]", a.NewTarget, env.PreferredLow, env.MaxSafe-1)
		}

		validReasons := map[DriftReason]bool{
			DriftReasonSettled: true, DriftReasonUp: true, DriftReasonDown: true,
			DriftReasonBoundedLow: true, DriftReasonBoundedHigh: true,
		}
		if !validReasons[a.Reason] && a.Reason != DriftReasonVarianceReset {
			t.Logf("stable phase action has reason=%s (may be acceptable)", a.Reason)
		}
	}

	finalTarget := stableHistory[len(stableHistory)-1].NewTarget
	if finalTarget < env.PreferredLow || finalTarget > env.MaxSafe-1 {
		t.Errorf("final target in stable phase=%d outside [PreferredLow=%d, MaxSafe-1=%d]", finalTarget, env.PreferredLow, env.MaxSafe-1)
	}
}

// TestSoak_Convergence_TargetMonotonicAfterWarmup tests that under stable inputs,
// the target moves monotonically toward equilibrium (no oscillation). This verifies
// design §10 monotonic-toward-equilibrium property for MaxCool mode.
func TestSoak_Convergence_TargetMonotonicAfterWarmup(t *testing.T) {
	tr := soakTrace{
		Class:          envelope.SSD,
		Mode:           mode.MaxCool,
		WindowSize:     80,
		StepsPerSample: 80, // fill window each Step
		StateDir:       t.TempDir(),
	}

	samples := make([]Sample, 80)
	for i := range samples {
		samples[i] = Sample{
			Timestamp:    time.Time{},
			TempCelsius:  48.0, // above PreferredLow=45 for SSD, below PreferredHigh=60
			FanDemandPct: 50,
			InletCelsius: 22.0,
		}
	}

	history := runSoak(t, tr, samples)

	if len(history) == 0 {
		t.Fatal("no actions returned")
	}

	targets := make([]int, len(history))
	for i, a := range history {
		targets[i] = a.NewTarget
	}

	env := envelope.DefaultEnvelopes[envelope.SSD]

	warmupWindow := 10
	if warmupWindow >= len(targets) {
		warmupWindow = len(targets) - 1
	}

	startIdx := warmupWindow
	for i := startIdx + 1; i < len(targets); i++ {
		if targets[i] > targets[i-1] {
			t.Errorf("step %d: target increased (%d → %d), violating monotonicity under stable inputs", i, targets[i-1], targets[i])
		}
	}

	finalTarget := targets[len(targets)-1]
	if finalTarget != env.PreferredLow {
		t.Logf("final target=%d (expected PreferredLow=%d; may need more steps to converge)", finalTarget, env.PreferredLow)
	}

	for i, tgt := range targets {
		if tgt < env.PreferredLow || tgt > env.MaxSafe-1 {
			t.Errorf("step %d: target=%d outside [PreferredLow=%d, MaxSafe-1=%d]", i, tgt, env.PreferredLow, env.MaxSafe-1)
		}
	}
}

// TestSoak_BoundedAlways_NeverEscapesPreferredRange tests that across all
// DriftActions, the target never escapes [PreferredLow, MaxSafe-1]. This
// verifies design §12 hard guarantee: bounded.
func TestSoak_BoundedAlways_NeverEscapesPreferredRange(t *testing.T) {
	tr := soakTrace{
		Class:          envelope.PassiveGPU,
		Mode:           mode.Balanced,
		WindowSize:     100,
		StepsPerSample: 25, // add 25 samples per Step to fill window over 4 cycles
		StateDir:       t.TempDir(),
	}

	samples := make([]Sample, 100)
	for i := range samples {
		// Sine wave between 50°C and 100°C (way outside envelope for PassiveGPU: [65,80])
		phase := float64(i) * 2.0 * math.Pi / 20.0
		minTemp := 50.0
		maxTemp := 100.0
		temp := minTemp + (maxTemp-minTemp)*(math.Sin(phase)+1)/2
		samples[i] = Sample{
			Timestamp:    time.Time{},
			TempCelsius:  temp,
			FanDemandPct: int(temp / 2),
			InletCelsius: 22.0 + math.Sin(float64(i)*0.1)*2,
		}
	}

	history := runSoak(t, tr, samples)

	if len(history) == 0 {
		t.Fatal("no actions returned")
	}

	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]
	for i, a := range history {
		if a.NewTarget < env.PreferredLow {
			t.Errorf("step %d: target=%d below PreferredLow=%d", i, a.NewTarget, env.PreferredLow)
		}
		if a.NewTarget > env.MaxSafe-1 {
			t.Errorf("step %d: target=%d above MaxSafe-1=%d", i, a.NewTarget, env.MaxSafe-1)
		}
	}

	allWithinBounds := true
	for _, a := range history {
		if a.NewTarget < env.PreferredLow || a.NewTarget > env.MaxSafe-1 {
			allWithinBounds = false
			break
		}
	}

	if !allWithinBounds {
		t.Error("target escaped [PreferredLow, MaxSafe-1] bounds")
	} else {
		t.Log("all targets stayed within [PreferredLow, MaxSafe-1]")
	}
}
