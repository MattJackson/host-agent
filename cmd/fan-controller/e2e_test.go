package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/controller"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/runner"
	"github.com/pq/docker-server/host-agent/internal/sensors"
)

// cycleDurationRE matches the cycle-duration metric value, which is real
// wall-clock and therefore non-deterministic. Normalized to a fixed token
// before golden comparison so the test asserts everything BUT that value.
var cycleDurationRE = regexp.MustCompile(`(?m)^fan_controller_cycle_duration_seconds .*$`)

func normalizeMetrics(b []byte) []byte {
	return cycleDurationRE.ReplaceAll(b, []byte("fan_controller_cycle_duration_seconds <scrubbed>"))
}

// stubReader is the same shape as the controller package's test stub —
// duplicated here because Go's tests can't share test-only types
// across packages cleanly.
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

type bufLog struct{}

func (bufLog) Printf(string, ...any) {}

// TestEndToEnd_ThreeCyclesMatchesGolden runs the controller end-to-end
// through three cycles with canned sensor readings and asserts the
// final metrics file matches a golden fixture exactly.
//
// The point of this test is to lock down the exact bytes written to
// /var/lib/host-agent/state/metrics.prom — node-exporter's textfile
// collector and the Grafana dashboard both depend on this exact shape.
// Any drift in metric names, labels, or value formatting breaks the
// dashboard silently.
func TestEndToEnd_ThreeCyclesMatchesGolden(t *testing.T) {
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")
	cfg, err := config.Load(profileDir, "", func(string) (string, bool) { return "", false }, bufLog{})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Three cycles of escalating CPU temp — 55°C, 65°C, 72°C.
	// 55: error=-15, |error|>3 (deadband) → step = -15*0.5 = -7.5 → -8.
	//     cand = 20 + -8 = 12 → clamp 20. Floor=0 (CPU at 55, emerg=80,
	//     window=10, outer edge=70, 55<70).
	// 65: error=-5, |error|>3 → step = -5*.5 + (65-55)*1 = -2.5+10 = 7.5 → 8.
	//     cand = 20+8 = 28. Floor still 0.
	// 72: error=+2, inside deadband but error>0 → step = 2*.5 + (72-65)*1 = 8.
	//     cand = 28+8 = 36. CPU_PF at 72: diff=72-70=2, f=20+(2/10)*80=36.
	//     Both at 36 → tie → cpu wins (initial in MaxWins).
	reader := &stubReader{
		readings: []sensors.Reading{
			{CPUMax: 55, Details: "P0.t1:55 "},
			{CPUMax: 65, Details: "P0.t1:65 "},
			{CPUMax: 72, Details: "P0.t1:72 "},
		},
		oks: []bool{true, true, true},
	}

	dir := t.TempDir()
	r := runner.NewFakeRunner()
	// Allow any ipmitool/nvidia-smi call to no-op succeed.
	r.SetPrefix("ipmitool", nil, runner.FakeResponse{Output: ""})

	c := controller.New(cfg, ipmi.New(r), reader, bufLog{},
		filepath.Join(dir, "base"),
		filepath.Join(dir, "metrics.prom"))
	c.CurrentSpeed = cfg.MinFan
	c.BaseSpeed = float64(cfg.MinFan)
	c.Now = func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) }
	c.PersistInterval = 1 * time.Hour // skip persistence during this test

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = c.Cycle(ctx)
	}

	got, err := os.ReadFile(filepath.Join(dir, "metrics.prom"))
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	got = normalizeMetrics(got)

	goldenPath := filepath.Join("testdata", "metrics_3cycles.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden updated")
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (set UPDATE_GOLDEN=1 to create)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("metrics file mismatch.\n--got--\n%s\n--want--\n%s", got, want)
	}
}

func findRepoRoot(t *testing.T) string {
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
