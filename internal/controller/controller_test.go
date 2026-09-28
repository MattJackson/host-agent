package controller

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/metrics"
	"github.com/pq/docker-server/host-agent/internal/runner"
	"github.com/pq/docker-server/host-agent/internal/sensors"
	"github.com/pq/docker-server/host-agent/internal/state"
)

// stubReader returns a fixed Reading.
type stubReader struct {
	readings []sensors.Reading
	oks      []bool
	idx      int
}

func (s *stubReader) Read(_ context.Context) (sensors.Reading, bool) {
	if s.idx >= len(s.readings) {
		return s.readings[len(s.readings)-1], s.oks[len(s.oks)-1]
	}
	r := s.readings[s.idx]
	ok := s.oks[s.idx]
	s.idx++
	return r, ok
}

type bufLog struct {
	lines []string
}

func (b *bufLog) Printf(f string, v ...any) {
	b.lines = append(b.lines, fmt.Sprintf(f, v...))
}

// findRepoRootDir locates the host-agent/ directory by walking up.
func findRepoRootDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for p := wd; p != "/"; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, "profiles", "default.env")); err == nil {
			return p
		}
	}
	t.Fatal("could not find host-agent/profiles/default.env")
	return ""
}

func defaultCfg(t *testing.T) *config.Config {
	t.Helper()
	repoRoot := findRepoRootDir(t)
	cfg, err := config.Load(filepath.Join(repoRoot, "profiles"), "", func(string) (string, bool) { return "", false }, &bufLog{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func newTestController(t *testing.T, cfg *config.Config, reader *stubReader) *Controller {
	t.Helper()
	dir := t.TempDir()
	r := runner.NewFakeRunner()
	c := New(cfg, ipmi.New(r), reader, &bufLog{},
		filepath.Join(dir, "base"),
		filepath.Join(dir, "metrics.prom"))
	c.PersistInterval = 60 * time.Second
	c.Now = func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) }
	return c
}

func TestLoadState(t *testing.T) {
	cfg := defaultCfg(t)

	t.Run("missing file → MinFan", func(t *testing.T) {
		c := newTestController(t, cfg, &stubReader{readings: []sensors.Reading{{CPUMax: 50}}, oks: []bool{true}})
		c.StatePath = filepath.Join(t.TempDir(), "nope")
		c.LoadState()
		if c.CurrentSpeed != cfg.MinFan {
			t.Errorf("CurrentSpeed=%d, want MinFan=%d", c.CurrentSpeed, cfg.MinFan)
		}
		if c.Samples != 0 {
			t.Errorf("Samples=%d, want 0", c.Samples)
		}
	})

	t.Run("resumes from last_speed (clamped)", func(t *testing.T) {
		c := newTestController(t, cfg, &stubReader{readings: []sensors.Reading{{CPUMax: 50}}, oks: []bool{true}})
		// Persist a last_speed ABOVE MaxFan so clampInt's hi branch fires.
		if err := state.Write(c.StatePath, state.State{BaseSpeed: 40, LastSpeed: cfg.MaxFan + 50, Samples: 7}); err != nil {
			t.Fatal(err)
		}
		c.LoadState()
		if c.CurrentSpeed != cfg.MaxFan {
			t.Errorf("CurrentSpeed=%d, want clamped to MaxFan=%d", c.CurrentSpeed, cfg.MaxFan)
		}
		if c.Samples != 7 {
			t.Errorf("Samples=%d, want 7", c.Samples)
		}
		if c.BaseSpeed != 40 {
			t.Errorf("BaseSpeed=%v, want 40", c.BaseSpeed)
		}
	})

	t.Run("legacy fallback to base when last_speed=0", func(t *testing.T) {
		c := newTestController(t, cfg, &stubReader{readings: []sensors.Reading{{CPUMax: 50}}, oks: []bool{true}})
		if err := state.Write(c.StatePath, state.State{BaseSpeed: 33.6, LastSpeed: 0, Samples: 3}); err != nil {
			t.Fatal(err)
		}
		c.LoadState()
		// int(33.6+0.5)=34, within [MinFan,MaxFan].
		if c.CurrentSpeed != 34 {
			t.Errorf("CurrentSpeed=%d, want 34 (legacy fallback to base)", c.CurrentSpeed)
		}
	})
}

func TestCycle_NormalOperation(t *testing.T) {
	cfg := defaultCfg(t)
	reader := &stubReader{
		readings: []sensors.Reading{{
			CPUMax: 55, PassiveGPUMax: 0, ActiveGPUMax: 0, HDDMax: 35, SSDMax: 0,
			Details: "P0.t1:55 d0h:35 ",
		}},
		oks: []bool{true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)

	snap := c.Cycle(context.Background())
	// CPU 55 vs target 70 — error=-15, abs > deadband=3 → PID step.
	// step = -15*0.5 = -7.5 → -8. cand = 20 + -8 = 12 → clamp 20.
	// HDD 35 vs target 40 — error=-5, abs > deadband=3 → step = -5*0.5 = -2.5 → -3. cand = 20-3=17 → clamp 20.
	if snap.CurrentSpeed != cfg.MinFan {
		t.Errorf("cool inputs should hold at MinFan, got %d", snap.CurrentSpeed)
	}
	if snap.InEmergency != 0 {
		t.Error("not emergency")
	}
}

func TestCycle_EmergencyTriggers100Percent(t *testing.T) {
	cfg := defaultCfg(t)
	reader := &stubReader{
		readings: []sensors.Reading{{
			CPUMax:  85, // >= CPU_EMERGENCY=80
			Details: "P0.t1:85 ",
		}},
		oks: []bool{true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)

	snap := c.Cycle(context.Background())
	if snap.CurrentSpeed != 100 {
		t.Errorf("emergency should set 100%%, got %d", snap.CurrentSpeed)
	}
	if snap.InEmergency != 1 {
		t.Error("InEmergency should be 1")
	}
	if snap.Source != "emergency" {
		t.Errorf("source: got %q want emergency", snap.Source)
	}
	// PIDs and floors should be zeroed in emergency.
	if snap.CPUCand != 0 || snap.CPUPF != 0 {
		t.Errorf("emergency: PID/PF should be 0, got cand=%d pf=%d", snap.CPUCand, snap.CPUPF)
	}
}

func TestCycle_TempReadFailFanFullForSafety(t *testing.T) {
	cfg := defaultCfg(t)
	reader := &stubReader{
		readings: []sensors.Reading{{}},
		oks:      []bool{false},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = 30
	c.BaseSpeed = 30.0

	snap := c.Cycle(context.Background())
	if snap.CurrentSpeed != 100 {
		t.Errorf("temp read fail: got %d want 100", snap.CurrentSpeed)
	}
}

func TestCycle_PassiveGPU_DrivesCurve(t *testing.T) {
	cfg := defaultCfg(t)
	// P4 at 88°C. v3 curve: comfort 75, emergency 90 → window 15.
	// Curve(88) = ProximityFloor(88,90,15): diff = 88-(90-15) = 13,
	// f = 20 + (13/15)*80 = 89.3 → 89. The GPU class binds.
	reader := &stubReader{
		readings: []sensors.Reading{{
			PassiveGPUMax: 88,
			Details:       "Gp0:88 ",
		}},
		oks: []bool{true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)

	snap := c.Cycle(context.Background())
	if snap.PGCand != 89 {
		t.Errorf("pg curve: got %d want 89", snap.PGCand)
	}
	if snap.PGPF != 0 {
		t.Errorf("pg_pf retired in v3, should be 0, got %d", snap.PGPF)
	}
	if snap.CurrentSpeed != 89 {
		t.Errorf("setpoint should be the GPU curve=89, got %d", snap.CurrentSpeed)
	}
	if snap.Source != "pg" {
		t.Errorf("source: got %q want pg", snap.Source)
	}
}

func TestCycle_ActiveGPU_OwnFanBelowThreshold_NoAssist(t *testing.T) {
	cfg := defaultCfg(t)
	// A5500 at 84°C but own fan at 59% — well below the 85% threshold.
	// Chassis should stay quiet; assist=0; source falls to whatever the
	// CPU class candidate dictates.
	reader := &stubReader{
		readings: []sensors.Reading{{
			CPUMax:          54, // moderate, in deadband
			ActiveGPUMax:    84,
			ActiveGPUFanMax: 59,
			Details:         "Ga0:84@59% ",
		}},
		oks: []bool{true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)

	snap := c.Cycle(context.Background())
	if snap.AGAssist != 0 {
		t.Errorf("ag_assist (below threshold): got %d want 0", snap.AGAssist)
	}
	if snap.Source == "ag_assist" {
		t.Errorf("source should NOT be ag_assist when below threshold, got %q", snap.Source)
	}
}

func TestCycle_ActiveGPU_OwnFanAboveThreshold_AssistRamps(t *testing.T) {
	cfg := defaultCfg(t)
	// A5500 own fan at 92% — past 85% threshold. Card is working hard.
	// threshold=85, ownFan=92 → progress (92-85)/(100-85) = 7/15
	// span = MaxFan-MinFan = 100-20 = 80
	// lift = round(7/15 * 80) = round(37.33) = 37
	// assist = 20 + 37 = 57
	reader := &stubReader{
		readings: []sensors.Reading{{
			CPUMax:          54,
			ActiveGPUMax:    87,
			ActiveGPUFanMax: 92,
			Details:         "Ga0:87@92% ",
		}},
		oks: []bool{true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)

	snap := c.Cycle(context.Background())
	if snap.AGAssist != 57 {
		t.Errorf("ag_assist (own fan 92%%): got %d want 57", snap.AGAssist)
	}
	if snap.Source != "ag_assist" {
		t.Errorf("source: got %q want ag_assist", snap.Source)
	}
	if snap.CurrentSpeed != 57 {
		t.Errorf("setpoint should be ag_assist=57, got %d", snap.CurrentSpeed)
	}
}

func TestCycle_ExitEmergencyHoldsAboveBaseline(t *testing.T) {
	cfg := defaultCfg(t)
	// Cycle 1: CPU at emergency → 100%.
	// Cycle 2: CPU at 78 (well below emergency 80, just outside ramp).
	//   PF at 78: diff = 78-(80-10) = 8, f = 20 + (8/10)*80 = 84. Active.
	reader := &stubReader{
		readings: []sensors.Reading{
			{CPUMax: 85},
			{CPUMax: 78},
		},
		oks: []bool{true, true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = 25.0 // some lived-in baseline

	_ = c.Cycle(context.Background()) // emergency hit
	if !c.InEmergency {
		t.Fatal("should be in emergency after cycle 1")
	}
	snap2 := c.Cycle(context.Background())
	if c.InEmergency {
		t.Error("should have cleared emergency in cycle 2")
	}
	// Exit speed must be max(MinFan, base=25, all proximity floors).
	// cpu_pf at 78 = 84 (above base) → exit at 84 minimum.
	if c.CurrentSpeed < 84 {
		t.Errorf("post-emergency speed too low: %d (want >= 84)", c.CurrentSpeed)
	}
	if snap2.InEmergency != 0 {
		t.Error("snapshot should report emergency cleared")
	}
}

// stepCycle advances the controller's clock by dt and runs one Cycle, so the
// cycle sees exactly dt since the previous one (Now may be called more than once
// per cycle, e.g. by PersistState, so the clock must not tick per call).
func stepCycle(c *Controller, clock *time.Time, dt time.Duration) metrics.Snapshot {
	*clock = clock.Add(dt)
	return c.Cycle(context.Background())
}

// manualClock points c.Now at a clock the test advances via stepCycle.
func manualClock(c *Controller) *time.Time {
	clock := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return clock }
	return &clock
}

// TestCycle_PostEmergency_RampsDownUnderSlew: emergency is still an immediate
// 100%, but when it clears the setpoint eases down at the slew limit instead of
// dropping straight to the curve, then settles exactly on the curve.
//
// default profile: CPU comfort 60, emergency 80, fan 20..100 → 4%/°C.
// dt = 15s → EWMA alpha = 1-e^-0.25 = 0.2212; slew = 0.2%/s*15 = 3%/cycle.
func TestCycle_PostEmergency_RampsDownUnderSlew(t *testing.T) {
	cfg := defaultCfg(t)
	readings := []sensors.Reading{
		{CPUMax: 68}, // 1: smooth seeds 68 → curve 20+8*4 = 52; rise from 20 is immediate
		{CPUMax: 85}, // 2: RAW 85 >= 80 → emergency 100 (smooth 68+0.2212*17 = 71.76)
	}
	for i := 0; i < 14; i++ {
		readings = append(readings, sensors.Reading{CPUMax: 72})
	}
	oks := make([]bool, len(readings))
	for i := range oks {
		oks[i] = true
	}
	c := newTestController(t, cfg, &stubReader{readings: readings, oks: oks})
	clk := manualClock(c)
	c.CurrentSpeed = cfg.MinFan

	if s := stepCycle(c, clk, 15*time.Second); s.CurrentSpeed != 52 {
		t.Fatalf("cycle 1: got %d want 52 (rise not slew-limited)", s.CurrentSpeed)
	}
	if s := stepCycle(c, clk, 15*time.Second); s.CurrentSpeed != 100 || s.InEmergency != 1 {
		t.Fatalf("cycle 2: got %d emerg=%d, want 100/1 (emergency bypasses slew)", s.CurrentSpeed, s.InEmergency)
	}
	// Cycles 3..: smooth 71.76→71.81→… (curve ≈ 67→68). Setpoint 97, 94, …, 70
	// (10 steps of 3), then 67 < demand 68 → lands on demand 68 and holds.
	want := []int{97, 94, 91, 88, 85, 82, 79, 76, 73, 70, 68, 68, 68, 68}
	for i, w := range want {
		s := stepCycle(c, clk, 15*time.Second)
		if s.CurrentSpeed != w {
			t.Fatalf("cycle %d: got %d want %d (demand %d)", i+3, s.CurrentSpeed, w, s.FanDemand)
		}
		if i == 0 && s.Source != "slew" {
			t.Errorf("cycle 3: source %q, want slew (setpoint held above demand)", s.Source)
		}
	}
}

// TestCycle_EmergencyUsesRawNotSmoothed: a jump from 70 to 80 trips emergency
// on the raw max even though the smoothed value is only 72.2.
func TestCycle_EmergencyUsesRawNotSmoothed(t *testing.T) {
	cfg := defaultCfg(t)
	c := newTestController(t, cfg, &stubReader{
		readings: []sensors.Reading{{CPUMax: 70}, {CPUMax: 80}},
		oks:      []bool{true, true},
	})
	clk := manualClock(c)
	c.CurrentSpeed = cfg.MinFan
	_ = stepCycle(c, clk, 15*time.Second)
	s := stepCycle(c, clk, 15*time.Second)
	// smooth = 70 + 0.2212*10 = 72.21 — far below 80, yet raw 80 must trip.
	if s.InEmergency != 1 || s.CurrentSpeed != 100 {
		t.Fatalf("raw 80 must trip emergency: emerg=%d speed=%d", s.InEmergency, s.CurrentSpeed)
	}
	if math.Abs(s.CPUSmooth-72.212) > 0.01 {
		t.Errorf("smoothed cpu = %.3f, want 72.212", s.CPUSmooth)
	}
}

// TestCycle_SmoothedCurveInput: a one-cycle spike feeds the curve its smoothed
// value, and the smoothing is dt-aware (a 3s cycle moves it less than a 27s one).
func TestCycle_SmoothedCurveInput(t *testing.T) {
	cases := []struct {
		name       string
		dt         time.Duration
		wantSmooth float64
		wantCurve  int
	}{
		// comfort 60 / emergency 80 / fan 20..100 → 4%/°C. Seed 70, spike 76.
		// 3s:  alpha 1-e^-0.05 = 0.04877 → 70.293 → 20+10.293*4 = 61.17 → 61
		{"3s", 3 * time.Second, 70.293, 61},
		// 15s: alpha 0.22120 → 71.327 → 20+11.327*4 = 65.31 → 65
		{"15s", 15 * time.Second, 71.327, 65},
		// 27s: alpha 0.36237 → 72.174 → 20+12.174*4 = 68.70 → 69
		{"27s", 27 * time.Second, 72.174, 69},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultCfg(t)
			c := newTestController(t, cfg, &stubReader{
				readings: []sensors.Reading{{CPUMax: 70}, {CPUMax: 76}},
				oks:      []bool{true, true},
			})
			clk := manualClock(c)
			c.CurrentSpeed = cfg.MinFan
			_ = stepCycle(c, clk, tc.dt)
			s := stepCycle(c, clk, tc.dt)
			if math.Abs(s.CPUSmooth-tc.wantSmooth) > 0.01 {
				t.Errorf("smoothed = %.3f, want %.3f", s.CPUSmooth, tc.wantSmooth)
			}
			// Raw 76 would have given 20+16*4 = 84.
			if s.CPUCand != tc.wantCurve {
				t.Errorf("curve = %d, want %d (smoothed, not raw 84)", s.CPUCand, tc.wantCurve)
			}
		})
	}
}

// TestCycle_NoisyIdleDoesNotPulse is the regression for the incident: an idle
// CPU whose hottest core flips 75↔77 every cycle. Comfort 70 (= emergency 80
// minus the 10°C CPU span), MIN_FAN 10 → 9%/°C.
//
// Before: raw curve(75) = 55, curve(77) = 73 → 18% swing every cycle.
// Now: the alternating ±1°C input settles to a smoothed oscillation of
// ±a/(2-a) = ±0.124°C around 76 (a = 0.2212): 75.876 → 10+5.876*9 = 62.9 → 63,
// 76.124 → 65.1 → 65. A 2% swing (the fall 65→63 is within the 3%/cycle slew).
func TestCycle_NoisyIdleDoesNotPulse(t *testing.T) {
	cfg := defaultCfg(t)
	cfg.MinFan = 10
	cfg.CPUComfort = 70
	var readings []sensors.Reading
	for i := 0; i < 80; i++ {
		readings = append(readings, sensors.Reading{CPUMax: 75 + 2*(i%2)})
	}
	oks := make([]bool, len(readings))
	for i := range oks {
		oks[i] = true
	}
	c := newTestController(t, cfg, &stubReader{readings: readings, oks: oks})
	clk := manualClock(c)
	c.CurrentSpeed = cfg.MinFan
	lo, hi := 101, -1
	for i := range readings {
		s := stepCycle(c, clk, 15*time.Second)
		if i < 40 { // let the EWMA and slew settle
			continue
		}
		lo, hi = min(lo, s.CurrentSpeed), max(hi, s.CurrentSpeed)
	}
	if lo != 63 || hi != 65 {
		t.Errorf("settled setpoint range [%d,%d], want [63,65] (raw curve would swing 55↔73)", lo, hi)
	}
}

// TestCycle_MonitorOnly_NoBMCWrites asserts the FanControl gate: when
// monitor-only, a full Cycle (including the emergency path) must issue
// ZERO ipmitool calls, so iDRAC's automatic thermal policy is never
// disturbed. This is the pre-12G R410 fix — a box whose BMC rejects
// SetFan must be left on iDRAC auto, not stranded in manual failsafe.
func TestCycle_MonitorOnly_NoBMCWrites(t *testing.T) {
	cfg := defaultCfg(t)
	dir := t.TempDir()
	r := runner.NewFakeRunner()

	// Emergency-hot CPU: in normal mode this fires SetFan(100) + EngageManual.
	reader := &stubReader{
		readings: []sensors.Reading{{CPUMax: 85, Details: "P0.t1:85 "}},
		oks:      []bool{true},
	}
	c := New(cfg, ipmi.New(r), reader, &bufLog{},
		filepath.Join(dir, "base"), filepath.Join(dir, "metrics.prom"))
	c.Now = func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) }
	c.FanControl = false // monitor-only

	snap := c.Cycle(context.Background())

	var ipmiCalls int
	for _, call := range r.Calls {
		if call.Name == "ipmitool" {
			ipmiCalls++
			t.Errorf("monitor-only Cycle issued an ipmitool call: %v", call.Args)
		}
	}
	if ipmiCalls != 0 {
		t.Fatalf("monitor-only must issue 0 ipmitool calls, got %d", ipmiCalls)
	}
	// Metrics/observability still work: the emergency was still detected.
	if snap.InEmergency != 1 {
		t.Errorf("monitor-only should still compute+report emergency, InEmergency=%d", snap.InEmergency)
	}
}

// TestNew_DefaultsFanControlOn guards the invariant that capable boxes
// (R730xd) are unchanged: New() must default FanControl=true.
func TestNew_DefaultsFanControlOn(t *testing.T) {
	cfg := defaultCfg(t)
	r := runner.NewFakeRunner()
	c := New(cfg, ipmi.New(r), &stubReader{}, &bufLog{}, "/x", "/y")
	if !c.FanControl {
		t.Fatal("New() must default FanControl=true so capable boxes keep driving fans")
	}
}
