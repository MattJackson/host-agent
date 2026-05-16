package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/runner"
	"github.com/pq/docker-server/host-agent/internal/sensors"
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
			CPUMax: 85, // >= CPU_EMERGENCY=80
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

func TestCycle_PassiveGPU_DrivesPID(t *testing.T) {
	cfg := defaultCfg(t)
	// P4 at 88°C — well above target 83 (window=7, emergency=90, so
	// proximity floor active). PID +5 above target → step = 5*0.5+0*1=2.5→3.
	// PF at 88: diff = 88 - (90-7) = 5, f = 20 + (5/7)*80 = 77.14 → 77.
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
	if snap.PGCand != 23 { // 20 + 3
		t.Errorf("pg_cand: got %d want 23", snap.PGCand)
	}
	if snap.PGPF != 77 {
		t.Errorf("pg_pf: got %d want 77", snap.PGPF)
	}
	if snap.CurrentSpeed != 77 {
		t.Errorf("setpoint should be pg_pf=77, got %d", snap.CurrentSpeed)
	}
	if snap.Source != "pg_pf" {
		t.Errorf("source: got %q want pg_pf", snap.Source)
	}
}

func TestCycle_ActiveGPU_AssistContributes(t *testing.T) {
	cfg := defaultCfg(t)
	// A5500 at 80°C — target 78, gain 3 → assist = MinFan + 2*3 = 26.
	// Active-GPU proximity floor at 80: emergency=88, window=10 → outer
	// edge at 78. diff = 80-78 = 2, f = 20 + (2/10)*80 = 36. So PF
	// (36) > assist (26) → source=ag_pf. Pick a temp where assist wins.
	//
	// At 79°C: assist = 20 + 1*3 = 23. ag_pf: diff = 79-78 = 1,
	// f = 20 + 0.1*80 = 28 → PF=28. Still PF wins.
	//
	// At 78°C (= target, NOT above): assist=0 (not above target).
	//
	// So below target, ag_pf is silent (diff=0 → MinFan); at target+1,
	// ag_pf already kicks in. The two factors track each other.
	// Pick a temp BELOW the PF outer edge — but the outer edge IS
	// target, so they collide by design. Use a config where they differ:
	cfg.ActiveGPUTarget = 70
	cfg.ActiveGPUEmergency = 95
	cfg.ActiveGPUApproachWindow = 5
	// At 80°C, target=70, gain=3 → assist = 20 + 10*3 = 50.
	// PF: outer edge = 95-5 = 90. 80 < 90 → PF=MinFan=20. Assist wins.
	reader := &stubReader{
		readings: []sensors.Reading{{
			ActiveGPUMax: 80,
			Details:      "Ga0:80@65% ",
		}},
		oks: []bool{true},
	}
	c := newTestController(t, cfg, reader)
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)

	snap := c.Cycle(context.Background())
	if snap.AGAssist != 50 {
		t.Errorf("ag_assist: got %d want 50", snap.AGAssist)
	}
	if snap.Source != "ag_assist" {
		t.Errorf("source: got %q want ag_assist", snap.Source)
	}
	if snap.CurrentSpeed != 50 {
		t.Errorf("setpoint should be ag_assist=50, got %d", snap.CurrentSpeed)
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
