package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/adaptive"
	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/learn"
)

type capLog struct{ lines []string }

func (l *capLog) Printf(f string, v ...any) { l.lines = append(l.lines, fmt.Sprintf(f, v...)) }

func (l *capLog) count(sub string) int {
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// spanCfg: emergencies CPU 80 / GPU 90 / HDD 50 / SSD 65, so the span caps are
// CPU 80-10=70, GPU 90-8=82, HDD 50-6=44, SSD 65-6=59. Targets are the
// min-noise ones (CPU 76, GPU 83, HDD 43, SSD 60).
func spanCfg() *config.Config {
	return &config.Config{
		CPUTarget: 76, CPUEmergency: 80, CPUComfort: 60,
		GPUTarget: 83, GPUEmergency: 90, GPUComfort: 75,
		HDDTarget: 43, HDDEmergency: 50, HDDComfort: 38,
		SSDTarget: 60, SSDEmergency: 65, SSDComfort: 48,
		MinFan: 10, MaxFan: 100,
	}
}

func TestComfortCap_PerClass(t *testing.T) {
	cfg := spanCfg()
	cases := []struct {
		class envelope.Class
		emerg int
		want  int
	}{
		{envelope.CPU, cfg.CPUEmergency, 70},
		{envelope.PassiveGPU, cfg.GPUEmergency, 82},
		{envelope.HDD, cfg.HDDEmergency, 44},
		{envelope.SSD, cfg.SSDEmergency, 59},
	}
	for _, c := range cases {
		if got := comfortCap(c.class, c.emerg); got != c.want {
			t.Errorf("comfortCap(%s, %d) = %d, want %d", c.class, c.emerg, got, c.want)
		}
	}
}

func TestIsLegacyUnraidDellFanOptOut(t *testing.T) {
	cases := []struct {
		name, hostOS, model, vendor string
		want                        bool
	}{
		{"supported Dell on Unraid", "Unraid", "dell_xc730xd_12", "Dell Inc.", true},
		{"wrong chassis", "Unraid", "dell_r730xd", "Dell Inc.", false},
		{"non-Dell BMC", "Unraid", "dell_xc730xd_12", "Supermicro", false},
		{"non-Unraid host", "Linux", "dell_xc730xd_12", "Dell Inc.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLegacyUnraidDellFanOptOut(tc.hostOS, tc.model, tc.vendor); got != tc.want {
				t.Fatalf("isLegacyUnraidDellFanOptOut(%q, %q, %q) = %t, want %t", tc.hostOS, tc.model, tc.vendor, got, tc.want)
			}
		})
	}
}

// TestLoadBaseline_ClampsToSpanCap: a restored comfort above the cap is clamped
// DOWN to it (previously: ignored if > emergency-1, kept if <= emergency-1).
func TestLoadBaseline_ClampsToSpanCap(t *testing.T) {
	cases := []struct {
		name                   string
		cpu, gpu, hdd, ssd     int
		wCPU, wGPU, wHDD, wSSD int
	}{
		// The incident values: cpu 76 (emerg-4) → 70, gpu 87 (emerg-3) → 82.
		{"incident comforts clamp down", 76, 87, 43, 58, 70, 82, 43, 58},
		{"at cap kept", 70, 82, 44, 59, 70, 82, 44, 59},
		{"under cap kept", 65, 80, 40, 55, 65, 80, 40, 55},
		// hdd 49 (emerg-1, the old ceiling) → 44; ssd 64 → 59.
		{"old emergency-1 ceiling clamps", 79, 89, 49, 64, 70, 82, 44, 59},
		// Below learnComfortFloor (20): ignored → profile comfort kept.
		{"below floor ignored", 15, 0, 43, 58, 60, 75, 43, 58},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "learned.json")
			b, _ := json.Marshal(baseline{Epoch: learnEpoch, Scanned: true, CPU: c.cpu, GPU: c.gpu, HDD: c.hdd, SSD: c.ssd})
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := spanCfg()
			if scanned, _ := loadBaseline(path, cfg, &capLog{}); !scanned {
				t.Fatal("scanned=true baseline should report scanned")
			}
			got := [4]int{cfg.CPUComfort, cfg.GPUComfort, cfg.HDDComfort, cfg.SSDComfort}
			want := [4]int{c.wCPU, c.wGPU, c.wHDD, c.wSSD}
			if got != want {
				t.Errorf("comfort cpu/gpu/hdd/ssd = %v, want %v", got, want)
			}
		})
	}
}

// TestLoadBaseline_DiscardsEpoch3: the incident's learned.json (epoch 3) must be
// discarded, not clamped, so the box re-scans under the new span rule.
func TestLoadBaseline_DiscardsEpoch3(t *testing.T) {
	if learnEpoch != 4 {
		t.Fatalf("learnEpoch = %d, want 4", learnEpoch)
	}
	path := filepath.Join(t.TempDir(), "learned.json")
	if err := os.WriteFile(path, []byte(`{"epoch":3,"scanned":true,"cpu":76,"gpu":87,"hdd":43,"ssd":58}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := spanCfg()
	if scanned, _ := loadBaseline(path, cfg, &capLog{}); scanned {
		t.Error("epoch-3 state must not report scanned")
	}
	if cfg.CPUComfort != 60 || cfg.GPUComfort != 75 {
		t.Errorf("epoch-3 comforts must not be applied: cpu=%d gpu=%d", cfg.CPUComfort, cfg.GPUComfort)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale learned.json should be removed")
	}
}

// TestEnforceCurveSpan clamps configured comfort and logs target-within-span
// once per affected class. spanCfg targets vs caps: CPU 76>70, GPU 83>82,
// HDD 43<=44, SSD 60>59 → three "within" lines.
func TestEnforceCurveSpan(t *testing.T) {
	cfg := spanCfg()
	cfg.CPUComfort = 78 // configured above cap 70
	cfg.HDDComfort = 44 // at cap: untouched
	log := &capLog{}
	enforceCurveSpan(cfg, log)
	if cfg.CPUComfort != 70 || cfg.GPUComfort != 75 || cfg.HDDComfort != 44 || cfg.SSDComfort != 48 {
		t.Errorf("comfort cpu/gpu/hdd/ssd = %d/%d/%d/%d, want 70/75/44/48",
			cfg.CPUComfort, cfg.GPUComfort, cfg.HDDComfort, cfg.SSDComfort)
	}
	if n := log.count("comfort 78 → 70"); n != 1 {
		t.Errorf("want 1 cpu clamp log, got %d: %v", n, log.lines)
	}
	if n := log.count("within"); n != 3 {
		t.Errorf("want 3 target-within-span logs (cpu, passive_gpu, ssd), got %d: %v", n, log.lines)
	}
}

// TestRunLearnTick_RespectsSpanCap: the learner's MaxRampStart is the span cap,
// so a too-cool class can raise its comfort only up to emergency - span.
func TestRunLearnTick_RespectsSpanCap(t *testing.T) {
	cases := []struct {
		name    string
		comfort int
		steady  float64
		demand  int
		want    int
	}{
		// steady 70 vs target 76 → too cool; demand 40 > MIN_FAN 10 → wants +1.
		{"raise stops at cap", 70, 70, 40, 70},
		{"raise below cap", 69, 70, 40, 70},
		// A comfort above the cap (not yet enforced) is pulled down to it by the
		// clamp even on the raise path: clamp(71+1, 20, 70) = 70.
		{"above cap pulled down", 71, 70, 40, 70},
		// steady 79 vs 76 → +3 > hot tolerance 2 → lower by 1.
		{"too hot still lowers", 70, 79, 40, 69},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := spanCfg()
			cfg.CPUComfort = c.comfort
			obs := adaptive.NewObserver(40, 10)
			now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < 40; i++ {
				obs.Add(envelope.CPU, adaptive.Sample{
					Timestamp:    now.Add(time.Duration(i) * 15 * time.Second),
					TempCelsius:  c.steady,
					FanDemandPct: c.demand,
				})
			}
			runLearnTick(cfg, obs, &capLog{})
			if cfg.CPUComfort != c.want {
				t.Errorf("cpu comfort = %d, want %d", cfg.CPUComfort, c.want)
			}
		})
	}
}

// TestFitScan_HonoursSpanCap: the scan placement for the idle-CPU plant
// temp = 80 - 0.1*fan (fit comfort 74, see learn.TestFitComfort_HonoursMinSpan)
// is capped at 80 - 10 = 70.
func TestFitScan_HonoursSpanCap(t *testing.T) {
	cfg := spanCfg()
	pts := []learn.ScanPoint{{FanPct: 80, TempC: 72}, {FanPct: 55, TempC: 74.5}, {FanPct: 30, TempC: 77}}
	fitScan(&capLog{}, cfg, "cpu", envelope.CPU, pts, cfg.CPUTarget, cfg.CPUEmergency, &cfg.CPUComfort)
	if cfg.CPUComfort != 70 {
		t.Errorf("scan cpu comfort = %d, want 70 (span cap)", cfg.CPUComfort)
	}
}
