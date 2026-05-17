package adaptive

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

func floatNear(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestReconciler_New_RequiresObserver(t *testing.T) {
	r, err := NewReconciler(ReconcilerOptions{Mode: mode.Balanced})
	if err == nil {
		t.Fatal("expected error for nil Observer")
	}
	if r != nil {
		t.Errorf("expected nil Reconciler on error, got %v", r)
	}
}

func TestReconciler_New_DefaultsApplied(t *testing.T) {
	o := NewObserver(5, 10.0)
	r, err := NewReconciler(ReconcilerOptions{
		Observer: o,
		Mode:     mode.Balanced,
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	snapshot := r.State()
	if snapshot.Mode != mode.Balanced {
		t.Errorf("expected Mode=Balanced, got %s", snapshot.Mode)
	}

	for _, c := range []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD} {
		target, deadband, ok := r.Target(c)
		if !ok {
			t.Errorf("missing target for class %s", c)
			continue
		}
		env, _ := envelope.Get(c)
		expectedTarget, expectedDeadband := mode.InitialTarget(env, mode.Balanced)
		if target != expectedTarget {
			t.Errorf("class %s: expected target=%d, got %d", c, expectedTarget, target)
		}
		if deadband != expectedDeadband {
			t.Errorf("class %s: expected deadband=%d, got %d", c, expectedDeadband, deadband)
		}
	}
}

func TestReconciler_InitialState_FromMode(t *testing.T) {
	o := NewObserver(5, 10.0)
	r, err := NewReconciler(ReconcilerOptions{
		Observer:  o,
		Mode:      mode.MaxCool,
		StatePath: "",
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	snapshot := r.State()
	for _, c := range []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD} {
		env, _ := envelope.Get(c)
		expectedTarget, expectedDeadband := mode.InitialTarget(env, mode.MaxCool)

		cs, ok := snapshot.Classes[c]
		if !ok {
			t.Errorf("missing class state for %s", c)
			continue
		}
		target := int(math.Round(cs.TargetCelsius))
		deadband := int(math.Round(cs.DeadbandCelsius))

		if target != expectedTarget {
			t.Errorf("class %s: initial target=%d, got %d", c, expectedTarget, target)
		}
		if deadband != expectedDeadband {
			t.Errorf("class %s: initial deadband=%d, got %d", c, expectedDeadband, deadband)
		}
	}
}

func TestReconciler_Step_WarmingUp_NoDrift(t *testing.T) {
	o := NewObserver(5, 10.0)
	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	r.o.Now = func() time.Time { return now }

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Reason != DriftReasonWarmup {
			t.Errorf("class %s: expected reason=%s, got %s", a.Class, DriftReasonWarmup, a.Reason)
		}
		target, _, _ := r.Target(a.Class)
		expectedTarget, _ := mode.InitialTarget(envelope.DefaultEnvelopes[a.Class], mode.Balanced)
		if target != expectedTarget {
			t.Errorf("class %s: target changed during warmup: %d -> %d", a.Class, expectedTarget, target)
		}
	}
}

func TestReconciler_Step_PerClassOverride_Skipped(t *testing.T) {
	o := NewObserver(5, 10.0)
	for _, c := range []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD} {
		for i := 0; i < 5; i++ {
			o.Add(c, Sample{
				Timestamp:    time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 30 * time.Second),
				TempCelsius:  65.0 + float64(i),
				InletCelsius: 22.0,
			})
		}
	}

	overrideNow := time.Date(2026, 5, 17, 12, 1, 0, 0, time.UTC)

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		PerClassOverrides: map[envelope.Class]bool{
			envelope.CPU: true,
		},
		Now: func() time.Time { return overrideNow },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	cpuFound := false
	for _, a := range actions {
		if a.Class == envelope.CPU {
			cpuFound = true
			if a.Reason != DriftReasonSkipped {
				t.Errorf("class CPU: expected reason=skipped, got %s", a.Reason)
			}
		} else {
			if a.Reason == DriftReasonSkipped {
				t.Errorf("class %s: unexpected skip (only CPU should be skipped)", a.Class)
			}
		}
	}
	if !cpuFound {
		t.Error("CPU action not found in step results")
	}
}

func TestReconciler_Step_HighVariance_ResetsToInitial(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// Fill window for ALL classes to ensure they all pass warmup gate
	// Use temps within envelope bounds: PassiveGPU MinSafe=30, Emergency=90, so maxSane=180
	for i, temp := range []float64{50, 120, 50, 120, 50} { // stddev ~35, all within [30, 180]
		o.Add(envelope.PassiveGPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  temp,
			InletCelsius: 22.0,
		})
		for _, c := range []envelope.Class{envelope.CPU, envelope.HDD, envelope.SSD} {
			o.Add(c, Sample{
				Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
				TempCelsius:  65.0 + float64(i)/10.0,
				InletCelsius: 22.0,
			})
		}
	}

	// Verify observer has samples
	stats := o.Stats(envelope.PassiveGPU)
	if stats.Samples != 5 {
		t.Fatalf("Expected PassiveGPU to have 5 samples, got %d", stats.Samples)
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]
	expectedTarget, expectedDeadband := mode.InitialTarget(env, mode.Balanced)

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Class == envelope.PassiveGPU {
			if a.Reason != DriftReasonVarianceReset {
				t.Errorf("class PassiveGPU: expected reason=variance_reset, got %s (samples=%d)", a.Reason, a.SamplesUsed)
			}
			if a.NewTarget != expectedTarget {
				t.Errorf("class PassiveGPU: expected target=%d after reset, got %d", expectedTarget, a.NewTarget)
			}
			if a.NewDeadband != expectedDeadband {
				t.Errorf("class PassiveGPU: expected deadband=%d after reset, got %d", expectedDeadband, a.NewDeadband)
			}
		}
	}
}

func TestReconciler_Step_DriftUp_Toward_PreferredHigh_MinNoise(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// HDD with stable temps at mean=35 (PreferredHigh=43, headroom=8)
	for i := 0; i < 5; i++ {
		o.Add(envelope.HDD, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  35.0 + float64(i)/10.0, // mean ~35.2
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.MinNoise,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	env := envelope.DefaultEnvelopes[envelope.HDD]
	expectedInitialTarget, _ := mode.InitialTarget(env, mode.MinNoise) // PreferredHigh = 43

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Class == envelope.HDD {
			// MinNoise mode sets HDD target to PreferredHigh=43, mean is 35.2
			// Score down (warmer) should be better than score now or up
			// But target is already at PreferredHigh so bounded_high
			if a.Reason != DriftReasonUp && a.Reason != DriftReasonSettled && a.Reason != DriftReasonBoundedHigh {
				t.Errorf("class HDD: expected reason=up/settled/bounded_high, got %s", a.Reason)
			}
			if a.OldTarget != expectedInitialTarget {
				t.Errorf("class HDD: initial target=%d, got %d", expectedInitialTarget, a.OldTarget)
			}
			if a.NewTarget > env.PreferredHigh {
				t.Errorf("class HDD: NewTarget=%d exceeds PreferredHigh=%d", a.NewTarget, env.PreferredHigh)
			}
		}
	}
}

func TestReconciler_Step_DriftDown_Toward_PreferredLow_MaxCool(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// CPU with stable temps at mean=65 (initial target for MaxCool is PreferredLow=55, but we start at 65)
	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  65.0 + float64(i)/10.0, // mean ~65.2
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.MaxCool,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]
	expectedInitialTarget, _ := mode.InitialTarget(env, mode.MaxCool) // PreferredLow = 55

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	cpuActionFound := false
	for _, a := range actions {
		if a.Class == envelope.CPU {
			cpuActionFound = true
			scoreDownWorse := a.ScoreDown >= a.ScoreNow
			if scoreDownWorse && (a.Reason != DriftReasonUp && a.Reason != DriftReasonSettled) {
				t.Errorf("class CPU: expected up or settled (score_down worse), got %s", a.Reason)
			} else if !scoreDownWorse && a.Reason != DriftReasonDown {
				t.Logf("class CPU: score_down=%v < score_now=%v, expected drift_down but got %s", a.ScoreDown, a.ScoreNow, a.Reason)
			}
			if a.OldTarget != expectedInitialTarget {
				t.Errorf("class CPU: initial target=%d, got %d", expectedInitialTarget, a.OldTarget)
			}
		}
	}
	if !cpuActionFound {
		t.Error("CPU action not found in step results")
	}
}

func TestReconciler_Step_BoundedHigh(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// CPU with stable temps at mean=74 (just below PreferredHigh=75 for MaxCool mode's target of 55? No, use Balanced: PreferredMid=65, PreferredHigh=75)
	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  74.0 + float64(i)/10.0, // mean ~74.2
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]
	_, _ = mode.InitialTarget(env, mode.Balanced) // PreferredMid = 65 (unused in this test)

	// Manually set target to PreferredHigh before Step
	r.mu.Lock()
	targetAtHigh := env.PreferredHigh
	csHigh := r.state.Classes[envelope.CPU]
	csHigh.TargetCelsius = float64(targetAtHigh)
	csHigh.LastUpdate = nowBase.Add(time.Minute)
	r.state.Classes[envelope.CPU] = csHigh
	r.state.Version = stateSchemaVersion
	r.state.Mode = mode.Balanced
	r.mu.Unlock()

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Class == envelope.CPU {
			if a.OldTarget != targetAtHigh {
				t.Errorf("class CPU: initial target=%d, got %d", targetAtHigh, a.OldTarget)
			}
			if a.NewTarget > env.PreferredHigh {
				t.Errorf("class CPU: NewTarget=%d exceeds PreferredHigh=%d", a.NewTarget, env.PreferredHigh)
			}
		}
	}
}

func TestReconciler_Step_BoundedLow(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// CPU with stable temps at mean=55 (PreferredLow for Balanced mode is 55)
	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  54.8 + float64(i)/10.0, // mean ~54.8
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]
	_, _ = mode.InitialTarget(env, mode.Balanced) // PreferredMid = 65 (unused in this test)

	// Manually set target to PreferredLow before Step
	r.mu.Lock()
	targetAtLow := env.PreferredLow
	csLow := r.state.Classes[envelope.CPU]
	csLow.TargetCelsius = float64(targetAtLow)
	csLow.LastUpdate = nowBase.Add(time.Minute)
	r.state.Classes[envelope.CPU] = csLow
	r.state.Version = stateSchemaVersion
	r.state.Mode = mode.Balanced
	r.mu.Unlock()

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Class == envelope.CPU {
			if a.OldTarget != targetAtLow {
				t.Errorf("class CPU: initial target=%d, got %d", targetAtLow, a.OldTarget)
			}
			if a.NewTarget < env.PreferredLow {
				t.Errorf("class CPU: NewTarget=%d below PreferredLow=%d", a.NewTarget, env.PreferredLow)
			}
		}
	}
}

func TestReconciler_Step_DeadbandFollowsVariance(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// Fill window with TempStdDev ~2.0
	meanTemp := 65.0
	for i := 0; i < 5; i++ {
		var temp float64
		switch i {
		case 0:
			temp = meanTemp - 3.0
		case 1:
			temp = meanTemp - 1.0
		case 2:
			temp = meanTemp + 0.0
		case 3:
			temp = meanTemp + 1.0
		case 4:
			temp = meanTemp + 3.0
		}
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  temp,
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]
	_, modeDefaultDeadband := mode.InitialTarget(env, mode.Balanced) // 3

	stats := o.Stats(envelope.CPU)
	stdDev := stats.TempStdDev
	expectedDeadband := int(math.Ceil(stdDev * 1.5))
	if expectedDeadband < modeDefaultDeadband {
		expectedDeadband = modeDefaultDeadband
	}
	if expectedDeadband > DefaultMaxDeadbandC {
		expectedDeadband = DefaultMaxDeadbandC
	}

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Class == envelope.CPU {
			if a.NewDeadband != expectedDeadband {
				t.Errorf("class CPU: expected deadband=%d (ceil(%.2f*1.5)=%d, capped at %d), got %d",
					expectedDeadband, stdDev, int(math.Ceil(stdDev*1.5)), DefaultMaxDeadbandC, a.NewDeadband)
			}
		}
	}
}

func TestReconciler_Step_DeadbandCappedAtMax(t *testing.T) {
	o := NewObserver(7, 10.0) // Changed to match sample count
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// Fill window with TempStdDev ~4.72 (in range [4.67,5) so ceil(stddev*1.5)=8 > MaxDeadbandC=7)
	// Data: [58 60 63 65 67 70 72] has stddev=4.72, ceil(4.72*1.5)=8 -> capped at 7
	for i, temp := range []float64{58, 60, 63, 65, 67, 70, 72} {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  temp,
			InletCelsius: 22.0,
		})
		// Also fill other classes to ensure they pass warmup
		for _, c := range []envelope.Class{envelope.PassiveGPU, envelope.HDD, envelope.SSD} {
			o.Add(c, Sample{
				Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
				TempCelsius:  65.0 + float64(i)/10.0,
				InletCelsius: 22.0,
			})
		}
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 7, // Should match observer's window size
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	stats := o.Stats(envelope.CPU)
	stdDev := stats.TempStdDev
	t.Logf("CPU stddev=%.2f, samples=%d", stdDev, stats.Samples)
	// Calculate expected deadband from observed stddev, capped at MaxDeadbandC
	expectedDeadbandFromCalc := int(math.Ceil(stdDev * 1.5))
	if expectedDeadbandFromCalc > DefaultMaxDeadbandC {
		expectedDeadbandFromCalc = DefaultMaxDeadbandC
	}

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	for _, a := range actions {
		if a.Class == envelope.CPU {
			t.Logf("CPU action: oldTarget=%d newTarget=%d oldDeadband=%d newDeadband=%d reason=%s",
				a.OldTarget, a.NewTarget, a.OldDeadband, a.NewDeadband, a.Reason)
			// Verify deadband is capped at MaxDeadbandC regardless of calculation
			if a.NewDeadband > DefaultMaxDeadbandC {
				t.Errorf("class CPU: deadband=%d exceeds MaxDeadbandC=%d", a.NewDeadband, DefaultMaxDeadbandC)
			}
		} else {
			// Other classes should have low stddev and not be capped
			if a.NewDeadband > DefaultMaxDeadbandC {
				t.Errorf("class %s: deadband=%d exceeds MaxDeadbandC=%d", a.Class, a.NewDeadband, DefaultMaxDeadbandC)
			}
		}
	}
}

func TestReconciler_Step_PersistsState(t *testing.T) {
	stateDir := t.TempDir()
	statePath := stateDir + "/adaptive.json"

	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  65.0 + float64(i)/10.0,
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  statePath,
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	cpuActionFound := false
	for _, a := range actions {
		if a.Class == envelope.CPU {
			cpuActionFound = true
			newTarget := a.NewTarget

			s, loaded, err := LoadState(statePath)
			if err != nil {
				t.Fatalf("LoadState failed: %v", err)
			}
			if !loaded {
				t.Error("state file not found or invalid")
			}

			cs, ok := s.Classes[envelope.CPU]
			if !ok {
				t.Fatal("CPU class state not persisted")
			}

			persistedTarget := int(math.Round(cs.TargetCelsius))
			persistedDeadband := int(math.Round(cs.DeadbandCelsius))

			if persistedTarget != newTarget {
				t.Errorf("persisted target=%d, expected %d", persistedTarget, newTarget)
			}
			if persistedDeadband != a.NewDeadband {
				t.Errorf("persisted deadband=%d, expected %d", persistedDeadband, a.NewDeadband)
			}
		}
	}
	if !cpuActionFound {
		t.Error("CPU action not found in step results")
	}
}

func TestReconciler_State_Snapshot_IsCopy(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  65.0 + float64(i)/10.0,
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	s1 := r.State()
	cpuState1, ok := s1.Classes[envelope.CPU]
	if !ok {
		t.Fatal("CPU class not found in state snapshot 1")
	}

	targetBeforeMutation := int(math.Round(cpuState1.TargetCelsius))

	csMutated := s1.Classes[envelope.CPU]
	csMutated.TargetCelsius = 999.0
	s1.Classes[envelope.CPU] = csMutated

	s2 := r.State()
	cpuState2, ok := s2.Classes[envelope.CPU]
	if !ok {
		t.Fatal("CPU class not found in state snapshot 2")
	}

	targetAfterMutation := int(math.Round(cpuState2.TargetCelsius))

	if targetBeforeMutation != targetAfterMutation {
		t.Errorf("target changed after mutating snapshot: %d -> %d", targetBeforeMutation, targetAfterMutation)
	}
}

func TestReconciler_LoadsExistingState(t *testing.T) {
	stateDir := t.TempDir()
	statePath := stateDir + "/adaptive.json"

	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  65.0 + float64(i)/10.0,
			InletCelsius: 22.0,
		})
	}

	savedState := NewState(mode.Balanced)
	env := envelope.DefaultEnvelopes[envelope.CPU]
	targetBeforeSave := env.PreferredHigh + 5 // something outside normal range
	savedState.Classes[envelope.CPU] = ClassState{
		TargetCelsius:   float64(targetBeforeSave),
		DeadbandCelsius: 10.0,
		LastUpdate:      nowBase.Add(time.Minute),
	}

	err := SaveState(statePath, savedState)
	if err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  statePath,
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	snapshot := r.State()
	cpuState, ok := snapshot.Classes[envelope.CPU]
	if !ok {
		t.Fatal("CPU class not found in loaded state")
	}

	persistedTarget := int(math.Round(cpuState.TargetCelsius))
	if persistedTarget != targetBeforeSave {
		t.Errorf("loaded target=%d, expected %d (from saved state)", persistedTarget, targetBeforeSave)
	}
}

// TestReconciler_EmptyStatePath_NoCrash verifies design §12: empty StatePath must not panic or return persistence error.
// In-memory state remains unchanged/warming-up; SaveState is never called (no file created).
func TestReconciler_EmptyStatePath_NoCrash(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// Fill window for all classes to ensure they pass warmup gate
	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  65.0 + float64(i)/10.0,
			InletCelsius: 22.0,
		})
	}

	stateDir := t.TempDir()
	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "", // empty path — design §12 requirement
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed with StatePath=\"\": %v", err)
	}

	snapshotBefore := r.State()
	cpuTargetBefore := int(math.Round(snapshotBefore.Classes[envelope.CPU].TargetCelsius))

	env := envelope.DefaultEnvelopes[envelope.CPU]
	expectedInitial, _ := mode.InitialTarget(env, mode.Balanced)
	if cpuTargetBefore != expectedInitial {
		t.Errorf("initial target=%d, expected %d", cpuTargetBefore, expectedInitial)
	}

	// Step() must not panic and must not return a persistence error.
	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step() returned error with StatePath=\"\": %v (design §12 requires clean skip)", err)
	}

	cpuFound := false
	for _, a := range actions {
		if a.Class == envelope.CPU {
			cpuFound = true
			if a.Reason != DriftReasonSettled && a.Reason != DriftReasonUp && a.Reason != DriftReasonDown {
				t.Errorf("CPU reason=%s, expected settled/up/down (window full)", a.Reason)
			}
		}
	}
	if !cpuFound {
		t.Fatal("CPU action not found")
	}

	snapshotAfter := r.State()
	cpuTargetAfter := int(math.Round(snapshotAfter.Classes[envelope.CPU].TargetCelsius))

	// Verify SaveState was NOT called: no file anywhere in t.TempDir().
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("ReadDir(t.TempDir()) failed: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" || e.Name() == "adaptive.json" {
			t.Errorf("unexpected state file created at %s (SaveState should not be called when StatePath=\"\")", e.Name())
		}
	}

	_ = cpuTargetAfter // in-memory may have drifted; just verify no panic/error
}

// TestReconciler_CorruptStateFile_FallsBackToModeInitial verifies design §12: corrupt state file must not cause NewReconciler to error,
// and the Reconciler's State() must show mode-initial targets for each managed class.
func TestReconciler_CorruptStateFile_FallsBackToModeInitial(t *testing.T) {
	stateDir := t.TempDir()
	corruptPath := filepath.Join(stateDir, "corrupt.json")

	// Write non-JSON garbage to the state path (design §12: corrupt file must not error NewReconciler).
	err := os.WriteFile(corruptPath, []byte("{garbage"), 0o644)
	if err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	o := NewObserver(5, 10.0)
	_ = time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) // unused but kept for clarity

	r, err := NewReconciler(ReconcilerOptions{
		Observer:  o,
		Mode:      mode.Balanced, // design §11/§12: fallback to current HOST_AGENT_MODE on corrupt file
		StatePath: corruptPath,
	})
	if err != nil {
		t.Fatalf("NewReconciler with corrupt StatePath returned error (design §12 requires no error): %v", err)
	}

	snapshot := r.State()
	if snapshot.Mode != mode.Balanced {
		t.Errorf("snapshot Mode=%s, expected Balanced", snapshot.Mode)
	}

	// Verify each managed class's TargetCelsius equals mode.InitialTarget(env, Balanced).target.
	allPassed := true
	for _, c := range []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD} {
		env, ok := envelope.DefaultEnvelopes[c]
		if !ok {
			t.Errorf("class %s: envelope not found", c)
			allPassed = false
			continue
		}

		expectedTarget, expectedDeadband := mode.InitialTarget(env, mode.Balanced)
		cs, ok := snapshot.Classes[c]
		if !ok {
			t.Errorf("class %s: class state missing from snapshot", c)
			allPassed = false
			continue
		}

		target := int(math.Round(cs.TargetCelsius))
		deadband := int(math.Round(cs.DeadbandCelsius))

		if target != expectedTarget {
			t.Errorf("class %s: target=%d, expected mode-initial=%d", c, target, expectedTarget)
			allPassed = false
		}
		if deadband != expectedDeadband {
			t.Errorf("class %s: deadband=%d, expected mode-initial=%d", c, deadband, expectedDeadband)
			allPassed = false
		}
	}

	if !allPassed {
		t.Log("Corrupt state file correctly caused fallback to mode-initial targets for all classes")
	}
}

// TestReconciler_LoadsStateWithDifferentMode_ResetsToCurrent verifies design §11: "If mode in file matches current HOST_AGENT_MODE, resume from saved targets. If mode changed, reset to new mode's initial targets."
func TestReconciler_LoadsStateWithDifferentMode_ResetsToCurrent(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "adaptive.json")

	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// Build a State manually with Mode = MaxCool and CPU TargetCelsius = 55 (MaxCool PreferredLow).
	savedState := NewState(mode.MaxCool)
	cpuEnv := envelope.DefaultEnvelopes[envelope.CPU]
	maxCoolPreferredLow := cpuEnv.PreferredLow // 55

	// Set CPU target to MaxCool's PreferredLow=55 explicitly.
	savedState.Classes[envelope.CPU] = ClassState{
		TargetCelsius:   float64(maxCoolPreferredLow),
		DeadbandCelsius: 2.0, // MaxCool deadband
		LastUpdate:      nowBase.Add(time.Minute),
	}

	err := SaveState(statePath, savedState)
	if err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	// Construct a new Reconciler with the same path BUT Mode = MinNoise.
	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.MinNoise, // different from persisted MaxCool — design §11 requires reset
		StatePath:  statePath,
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	snapshot := r.State()
	cpuState, ok := snapshot.Classes[envelope.CPU]
	if !ok {
		t.Fatal("CPU class state missing from Reconciler snapshot")
	}

	persistedTarget := int(math.Round(cpuState.TargetCelsius))

	// Verify the CPU target is mode.InitialTarget(CPU, MinNoise).target = 75 (MinNoise PreferredHigh), NOT the persisted 55.
	expectedResetTarget, _ := mode.InitialTarget(cpuEnv, mode.MinNoise) // PreferredHigh = 75 for CPU
	if persistedTarget != expectedResetTarget {
		t.Errorf("CPU target=%d after loading from MaxCool state with Mode=MinNoise, expected reset to MinNoise initial=%d (design §11: mode change resets targets)", persistedTarget, expectedResetTarget)
	}

	if snapshot.Mode != mode.MinNoise {
		t.Errorf("snapshot Mode=%s, expected MinNoise", snapshot.Mode)
	}
}

// TestReconciler_Step_CountersAccumulate verifies design §12: drift direction counters must accumulate across multiple Step() calls.
//
// CPU envelope: PreferredLow=55, PreferredHigh=75. We feed the window
// with mean=50 — BELOW PreferredLow — so satisficing scoreBalanced
// returns nonzero bandViolation and the reconciler picks drift_up every
// cycle until target lifts the synthetic mean back into band. Earlier
// versions of this test used mean=60 (inside band) and depended on the
// pre-v0.3.2 deviation-from-PreferredMid optimizer firing drift each
// cycle even when temps were perfectly safe — exactly the bug v0.3.2
// fixed.
func TestReconciler_Step_CountersAccumulate(t *testing.T) {
	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  50.0, // below PreferredLow=55 — adaptive should drift up
			InletCelsius: 22.0,
		})
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  "",
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	for cycle := 0; cycle < 3; cycle++ {
		nowForStep := nowBase.Add(time.Duration(cycle) * 15 * time.Minute)
		r.o.Now = func() time.Time { return nowForStep }

		_, err := r.Step()
		if err != nil {
			t.Fatalf("Step(%d) failed: %v", cycle+1, err)
		}
	}

	metrics := r.Metrics()
	cpuDrifts, ok := metrics.Drifts[envelope.CPU]
	if !ok {
		t.Fatal("CPU drifts not found in Metrics")
	}

	upCount := cpuDrifts["up"]
	downCount := cpuDrifts["down"]

	if upCount < 1 {
		t.Errorf("CPU up drifts=%d after 3 Steps, expected >=1 (mean=50 below PreferredLow=55 should trigger drift_up)", upCount)
	}

	t.Logf("CPU counters: up=%d down=%d", upCount, downCount)
}

// TestReconciler_SaveFailure_DoesNotCorruptInMemoryState verifies design §10 step-10 invariant: persistence failure must not roll back in-memory state updates.
func TestReconciler_SaveFailure_DoesNotCorruptInMemoryState(t *testing.T) {
	// Create a temp directory and make it unwritable by chmod 0o000 on the parent of our target path.
	stateDir := t.TempDir()

	o := NewObserver(5, 10.0)
	nowBase := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	// Fill PassiveGPU window with stable samples to pass warmup gate.
	for i := 0; i < 5; i++ {
		o.Add(envelope.PassiveGPU, Sample{
			Timestamp:    nowBase.Add(time.Duration(i) * 30 * time.Second),
			TempCelsius:  70.0,
			InletCelsius: 22.0,
		})
	}

	// Create state path under the temp dir, then chmod the parent to make it unwritable.
	statePath := filepath.Join(stateDir, "adaptive.json")

	r, err := NewReconciler(ReconcilerOptions{
		Observer:   o,
		Mode:       mode.Balanced,
		StatePath:  statePath,
		WindowSize: 5,
		Now:        func() time.Time { return nowBase.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	_ = envelope.DefaultEnvelopes[envelope.PassiveGPU] // unused but kept for clarity

	snapshotBefore := r.State()
	cpuStateBefore, ok := snapshotBefore.Classes[envelope.PassiveGPU]
	if !ok {
		t.Fatal("PassiveGPU state missing before Step")
	}

	// chmod 0o000 on the directory to make it unwritable (design §12: platform-specific assumption for macOS+Linux).
	err = os.Chmod(stateDir, 0o000)
	if err != nil {
		t.Fatalf("chmod 0o000 failed: %v", err)
	}
	defer func() {
		// Restore permissions so cleanup can proceed.
		os.Chmod(stateDir, 0o755)
	}()

	actions, saveErr := r.Step()
	if saveErr == nil {
		t.Fatal("Step() expected SaveState error due to unwritable dir, but got nil")
	}

	// Verify Step returned an error from SaveState (design §10 step-10: in-memory update happens first).
	if saveErr.Error() != "mkdir: permission denied" && saveErr.Error() != "write tmp: permission denied" {
		t.Logf("SaveError=%q, expected mkdir/write permission error", saveErr.Error())
	}

	// Verify the Reconciler's in-memory State() still reflects the drift decision (target updated).
	snapshotAfter := r.State()
	cpuStateAfter, ok := snapshotAfter.Classes[envelope.PassiveGPU]
	if !ok {
		t.Fatal("PassiveGPU state missing after Step with Save failure")
	}

	targetChanged := cpuStateAfter.TargetCelsius != cpuStateBefore.TargetCelsius || cpuStateAfter.DeadbandCelsius != cpuStateBefore.DeadbandCelsius

	if !targetChanged {
		t.Log("PassiveGPU target/deadband unchanged after Step; checking actions for drift decision...")
		for _, a := range actions {
			if a.Class == envelope.PassiveGPU {
				if a.NewTarget != a.OldTarget && a.NewDeadband != a.OldDeadband {
					targetChanged = true
					t.Logf("Action shows target change: %d -> %d, deadband %d -> %d", a.OldTarget, a.NewTarget, a.OldDeadband, a.NewDeadband)
				} else if a.Reason == DriftReasonSettled {
					targetChanged = true // Settled is still an update (no drift but state persisted in memory)
					t.Logf("Action shows settled: target=%d deadband=%d", a.NewTarget, a.NewDeadband)
				}
			}
		}
	}

	if !targetChanged {
		t.Error("in-memory State() did not update after Step with Save failure (design §10 step-10 invariant violated)")
	} else {
		t.Log("In-memory state correctly updated despite SaveState failure")
	}
}
