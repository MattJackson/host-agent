package learn

import "testing"

func params() Params { return DefaultParams(20, 49) }

func TestTargetSeek_NotSettled_HoldsOff(t *testing.T) {
	// Window too noisy (stddev above gate) → never act, even if far from target.
	d := TargetSeek(50, 5.0 /*stddev*/, 50, 33, 38, params())
	if d.Acted || d.Reason != ReasonNotSettled {
		t.Fatalf("noisy window must not act: %+v", d)
	}
}

func TestTargetSeek_InTolerance_HoldsOff(t *testing.T) {
	// 38.5 vs target 38, tolerance 1.0 → within band → no action.
	d := TargetSeek(38.5, 0.3, 50, 33, 38, params())
	if d.Acted || d.Reason != ReasonInTolerance {
		t.Fatalf("within tolerance must not act: %+v", d)
	}
}

func TestTargetSeek_TooHot_LowersRampStart(t *testing.T) {
	// Steady 42 vs target 38 → too hot → more fan → ramp-start DOWN.
	d := TargetSeek(42, 0.3, 60, 33, 38, params())
	if !d.Acted || d.Reason != ReasonTooHot || d.NewRampStart != 32 {
		t.Fatalf("too hot should lower ramp-start 33→32: %+v", d)
	}
}

func TestTargetSeek_TooCool_RaisesRampStart(t *testing.T) {
	// Steady 34 vs target 38 → too cool → less fan (quieter) → ramp-start UP.
	d := TargetSeek(34, 0.3, 40, 33, 38, params())
	if !d.Acted || d.Reason != ReasonTooCool || d.NewRampStart != 34 {
		t.Fatalf("too cool should raise ramp-start 33→34: %+v", d)
	}
}

func TestTargetSeek_TooHotButSaturated_HoldsOff(t *testing.T) {
	// Too hot but fan p90 already at/above saturation → can't help; don't chase.
	d := TargetSeek(45, 0.3, 99, 33, 38, params())
	if d.Acted || d.Reason != ReasonSaturated {
		t.Fatalf("saturated fan must not chase: %+v", d)
	}
}

func TestTargetSeek_ClampsAtBounds(t *testing.T) {
	// Too cool but already at the max ramp-start → clamped, no spurious "acted".
	p := params()
	d := TargetSeek(30, 0.3, 30, p.MaxRampStart, 38, p)
	if d.Acted || d.Reason != ReasonClamped {
		t.Fatalf("at max bound must clamp: %+v", d)
	}
	// Too hot but already at the min ramp-start → clamped.
	d = TargetSeek(50, 0.3, 50, p.MinRampStart, 38, p)
	if d.Acted || d.Reason != ReasonClamped {
		t.Fatalf("at min bound must clamp: %+v", d)
	}
}

func TestTargetSeek_StepIsRateLimited(t *testing.T) {
	// Way too hot (steady 60 vs 38) but MaxStepC=1 → move only 1°C per action.
	d := TargetSeek(60, 0.3, 50, 33, 38, params())
	if !d.Acted || d.NewRampStart != 32 {
		t.Fatalf("step must be rate-limited to 1°C: %+v", d)
	}
}

// TestTargetSeek_ConvergesAndHolds is the "does it actually work" proof: a
// closed-loop simulation against a simple linear plant where steady-state temp
// tracks ramp-start with a fixed offset (measured on unraid-1: ramp-start 33 →
// settle ~39, i.e. +6). The learner must drive steady-state to TARGET and then
// HOLD it — converge, no oscillation.
func TestTargetSeek_ConvergesAndHolds(t *testing.T) {
	const offset = 6 // steadyTemp = rampStart + offset
	const target = 38
	p := DefaultParams(20, 49)

	for _, start := range []int{49, 20, 40, 25} { // start hot, cold, high, low
		rampStart := start
		var lastReasons []Reason
		for tick := 0; tick < 60; tick++ {
			steady := float64(rampStart + offset)
			d := TargetSeek(steady, 0.3 /*settled*/, 50 /*not saturated*/, rampStart, target, p)
			rampStart = d.NewRampStart
			lastReasons = append(lastReasons, d.Reason)
			if len(lastReasons) > 4 {
				lastReasons = lastReasons[len(lastReasons)-4:]
			}
		}
		steady := rampStart + offset
		// Converged to within tolerance of target.
		if steady < target-1 || steady > target+1 {
			t.Errorf("start=%d: did not converge — steady=%d target=%d (rampStart=%d)", start, steady, target, rampStart)
		}
		// And HELD: the last several ticks must all be no-op (in_tolerance), not
		// oscillating between too_hot/too_cool.
		for _, r := range lastReasons {
			if r != ReasonInTolerance {
				t.Errorf("start=%d: not stable at equilibrium — recent reasons %v (expected all in_tolerance)", start, lastReasons)
				break
			}
		}
	}
}

// TestTargetSeek_QuietsWhenOverCooled proves the "as quiet as possible" half:
// a drive sitting below target (over-cooled, wasting fan) makes the learner
// RAISE ramp-start step by step until it reaches the target band, then stop —
// i.e. it backs the fan off to the minimum that still holds target.
func TestTargetSeek_QuietsWhenOverCooled(t *testing.T) {
	const offset = 6
	const target = 40
	p := DefaultParams(20, 49)
	rampStart := 25 // steady 31 — way over-cooled (fan too high)
	moves := 0
	for tick := 0; tick < 60; tick++ {
		steady := float64(rampStart + offset)
		d := TargetSeek(steady, 0.3, 60, rampStart, target, p)
		if d.Acted {
			moves++
			if d.Reason != ReasonTooCool {
				t.Fatalf("over-cooled should only ever raise ramp-start (quieter), got %s", d.Reason)
			}
		}
		rampStart = d.NewRampStart
	}
	if moves == 0 {
		t.Fatal("over-cooled drive should have raised ramp-start to quiet the fan")
	}
	if steady := rampStart + offset; steady < target-1 || steady > target+1 {
		t.Fatalf("did not settle at target after quieting: steady=%d target=%d", steady, target)
	}
}
