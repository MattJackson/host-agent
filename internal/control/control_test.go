package control

import (
	"math"
	"testing"
)

func TestStepPID_AbstainOnZeroTemp(t *testing.T) {
	// Temp <= 0 must return 0 (abstain), regardless of every other
	// parameter. This is what lets a temporarily missing sensor (GPU
	// runtime regress, smartctl error) NOT lock fans high.
	p := PIDParams{
		Temp:              0,
		Target:            70,
		Deadband:          3,
		LastTemp:          50,
		CurrentSpeed:      80,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	if got := StepPID(p); got != 0 {
		t.Errorf("temp=0 → got %d want 0", got)
	}
	p.Temp = -5
	if got := StepPID(p); got != 0 {
		t.Errorf("temp=-5 → got %d want 0", got)
	}
}

func TestStepPID_DeadbandDriftsTowardMinFan(t *testing.T) {
	// At target, in deadband, current > minFan → drift down by rate,
	// clamped to minFan.
	cases := []struct {
		name                                       string
		temp, target, deadband, current, min, rate int
		want                                       int
	}{
		{"at target, current well above min", 70, 70, 3, 50, 20, 3, 47},
		{"at target, current near min", 70, 70, 3, 21, 20, 3, 20},
		{"at target, current already at min", 70, 70, 3, 20, 20, 3, 20},
		{"1°C below target (inside deadband)", 69, 70, 3, 50, 20, 3, 47},
		{"3°C below target (deadband edge)", 67, 70, 3, 50, 20, 3, 47},
		// 4°C below target = outside deadband → PID step, not drift.
		// error=-4, step = -4*0.5 = -2, cand = 50 + -2 = 48.
		{"4°C below target (PID, not drift)", 66, 70, 3, 50, 20, 3, 48},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := PIDParams{
				Temp: c.temp, Target: c.target, Deadband: c.deadband,
				LastTemp: c.temp, CurrentSpeed: c.current,
				MinFan: c.min, MaxFan: 100,
				FanGain: 0.5, DerivativeGain: 1.0,
				DeadbandDriftRate: c.rate,
			}
			if got := StepPID(p); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestStepPID_AsymmetricDeadband_AboveTargetAlwaysSteps(t *testing.T) {
	// Critical: above target, inside |error| <= deadband, PID still
	// steps (NOT drifts). Bash semantics: `if error <= 0 && abs <= db`
	// — positive errors always do P+D.
	p := PIDParams{
		Temp:              71, // +1 above target, inside deadband
		Target:            70,
		Deadband:          3,
		LastTemp:          71, // d_temp = 0
		CurrentSpeed:      50,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	// step = 1*0.5 + 0*1.0 = 0.5 → round half-away = 1. cand = 51.
	if got := StepPID(p); got != 51 {
		t.Errorf("+1 above target, inside deadband: got %d want 51", got)
	}
}

func TestStepPID_HighError_FullPIDStep(t *testing.T) {
	// 10°C over target, rising 5°C/cycle.
	// step = 10*0.5 + 5*1.0 = 10 → cand = 60+10 = 70.
	p := PIDParams{
		Temp:              80,
		Target:            70,
		Deadband:          3,
		LastTemp:          75,
		CurrentSpeed:      60,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	if got := StepPID(p); got != 70 {
		t.Errorf("+10 error +5/cyc: got %d want 70", got)
	}
}

func TestStepPID_NegativeStep_RoundedAwayFromZero(t *testing.T) {
	// 5°C below target, falling — error=-5, d_temp=-2.
	// step = -5*0.5 + -2*1.0 = -4.5 → round half-away = -5. cand = 50-5 = 45.
	p := PIDParams{
		Temp:              65,
		Target:            70,
		Deadband:          3,
		LastTemp:          67,
		CurrentSpeed:      50,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	if got := StepPID(p); got != 45 {
		t.Errorf("-5 error -2/cyc: got %d want 45", got)
	}
}

func TestStepPID_ClampedToMaxFan(t *testing.T) {
	// Big positive error: cand would overshoot MaxFan, must clamp.
	p := PIDParams{
		Temp:              90,
		Target:            70,
		Deadband:          3,
		LastTemp:          85,
		CurrentSpeed:      95,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	// step = 20*0.5 + 5*1.0 = 15 → cand = 95+15 = 110 → clamp 100.
	if got := StepPID(p); got != 100 {
		t.Errorf("clamp to MaxFan: got %d want 100", got)
	}
}

func TestStepPID_ClampedToMinFan(t *testing.T) {
	// Big negative step, current near min.
	p := PIDParams{
		Temp:              50,
		Target:            70,
		Deadband:          3,
		LastTemp:          70,
		CurrentSpeed:      25,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	// error=-20, d=-20. Outside deadband (|-20|>3) → step = -20*.5 + -20*1 = -30.
	// cand = 25-30 = -5 → clamp 20.
	if got := StepPID(p); got != 20 {
		t.Errorf("clamp to MinFan: got %d want 20", got)
	}
}

func TestStepPID_FirstCycleNoLastTemp(t *testing.T) {
	// LastTemp = -1 → d_temp forced to 0. Error+D would otherwise be
	// nonsense on the first cycle after restart.
	p := PIDParams{
		Temp:              80,
		Target:            70,
		Deadband:          3,
		LastTemp:          -1,
		CurrentSpeed:      50,
		MinFan:            20,
		MaxFan:            100,
		FanGain:           0.5,
		DerivativeGain:    1.0,
		DeadbandDriftRate: 3,
	}
	// step = 10*0.5 + 0 = 5 → cand = 55.
	if got := StepPID(p); got != 55 {
		t.Errorf("first cycle: got %d want 55", got)
	}
}

func TestProximityFloor_BelowWindow_ReturnsMinFan(t *testing.T) {
	// emergency=80, window=10, so silent zone is < 70.
	got := ProximityFloor(60, 80, 10, 20, 100)
	if got != 20 {
		t.Errorf("below window: got %d want 20", got)
	}
	// Exactly at outer edge (emergency - window): floor = MinFan.
	got = ProximityFloor(70, 80, 10, 20, 100)
	if got != 20 {
		t.Errorf("at outer edge: got %d want 20", got)
	}
}

func TestProximityFloor_LinearRamp(t *testing.T) {
	// emergency=80, window=10. At temp=75 (halfway through window),
	// floor = 20 + 0.5*80 = 60.
	got := ProximityFloor(75, 80, 10, 20, 100)
	if got != 60 {
		t.Errorf("halfway: got %d want 60", got)
	}
	// At emergency: floor = MaxFan.
	got = ProximityFloor(80, 80, 10, 20, 100)
	if got != 100 {
		t.Errorf("at emergency: got %d want 100", got)
	}
	// Above emergency: still clamped to MaxFan.
	got = ProximityFloor(90, 80, 10, 20, 100)
	if got != 100 {
		t.Errorf("above emergency: got %d want 100", got)
	}
}

func TestProximityFloor_NarrowWindow_HDDExample(t *testing.T) {
	// HDD defaults: emergency=50, window=5. Ramp 45→50.
	cases := map[int]int{
		40: 20, // below window
		45: 20, // at outer edge
		46: 36, // 20 + (1/5)*80 = 36
		47: 52,
		48: 68,
		49: 84,
		50: 100,
	}
	for temp, want := range cases {
		if got := ProximityFloor(temp, 50, 5, 20, 100); got != want {
			t.Errorf("HDD ramp temp=%d: got %d want %d", temp, got, want)
		}
	}
}

func TestProximityFloor_ZeroWindowDegenerate(t *testing.T) {
	// Window=0 isn't a sensible config but mustn't divide-by-zero.
	got := ProximityFloor(80, 80, 0, 20, 100)
	if got != 100 {
		t.Errorf("zero window at emergency: got %d want 100", got)
	}
	got = ProximityFloor(70, 80, 0, 20, 100)
	if got != 20 {
		t.Errorf("zero window below emergency: got %d want 20", got)
	}
}

func TestEwma_HalfLifeApprox(t *testing.T) {
	// alpha=0.001 → half-life = ln(2)/alpha ≈ 693 cycles. We verify the
	// math directly: after one step, base = (1-α)*old + α*new.
	got := Ewma(50.0, 100.0, 0.001)
	want := 50.05
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ewma one step: got %g want %g", got, want)
	}
	// Steady-state: prev == sample → Ewma returns sample.
	got = Ewma(50.0, 50.0, 0.001)
	if math.Abs(got-50.0) > 1e-9 {
		t.Errorf("ewma steady: got %g want 50", got)
	}
}

func TestActiveGPUAssist_BelowTargetReturnsZero(t *testing.T) {
	// temp <= target → no assist, no chassis floor lift.
	got := ActiveGPUAssist(75, 78, 3.0, 20, 100)
	if got != 0 {
		t.Errorf("below target: got %d want 0", got)
	}
	got = ActiveGPUAssist(78, 78, 3.0, 20, 100)
	if got != 0 {
		t.Errorf("at target: got %d want 0", got)
	}
}

func TestActiveGPUAssist_AboveTarget_LiftsChassisFloor(t *testing.T) {
	// target=78, gain=3 → A5500 at 82 lifts by 4*3=12, on top of MinFan=20.
	got := ActiveGPUAssist(82, 78, 3.0, 20, 100)
	if got != 32 {
		t.Errorf("4 above target: got %d want 32", got)
	}
	// At 88 (= emergency, but assist still computed pre-emergency check):
	// 10°C overshoot * 3 = 30 lift → 50.
	got = ActiveGPUAssist(88, 78, 3.0, 20, 100)
	if got != 50 {
		t.Errorf("10 above target: got %d want 50", got)
	}
	// Very hot card: assist clamps to MaxFan.
	got = ActiveGPUAssist(120, 78, 3.0, 20, 100)
	if got != 100 {
		t.Errorf("clamp to MaxFan: got %d want 100", got)
	}
}

func TestMaxWins_PicksHighestWithCorrectSource(t *testing.T) {
	r := MaxWins(
		MaxCandidate{Name: "cpu", Value: 40},
		[]MaxCandidate{
			{Name: "pg", Value: 35},
			{Name: "hdd", Value: 55},
			{Name: "ssd", Value: 45},
			{Name: "cpu_pf", Value: 20},
			{Name: "pg_pf", Value: 50},
		},
		20, 100,
	)
	if r.NewSpeed != 55 || r.Source != "hdd" {
		t.Errorf("max-wins: got %d/%s want 55/hdd", r.NewSpeed, r.Source)
	}
}

func TestMaxWins_TieKeepsEarlierSource(t *testing.T) {
	// Bash uses strict `-gt`, so an exact tie does NOT replace.
	r := MaxWins(
		MaxCandidate{Name: "cpu", Value: 50},
		[]MaxCandidate{
			{Name: "pg", Value: 50},
			{Name: "hdd", Value: 50},
		},
		20, 100,
	)
	if r.Source != "cpu" {
		t.Errorf("tie should keep cpu: got %s", r.Source)
	}
}

func TestMaxWins_ClampsToFanRange(t *testing.T) {
	r := MaxWins(
		MaxCandidate{Name: "cpu", Value: 5},
		nil,
		20, 100,
	)
	if r.NewSpeed != 20 {
		t.Errorf("below MinFan: got %d want 20", r.NewSpeed)
	}
	r = MaxWins(
		MaxCandidate{Name: "cpu", Value: 150},
		nil,
		20, 100,
	)
	if r.NewSpeed != 100 {
		t.Errorf("above MaxFan: got %d want 100", r.NewSpeed)
	}
}
