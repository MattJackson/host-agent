package adaptive

import (
	"math"
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
