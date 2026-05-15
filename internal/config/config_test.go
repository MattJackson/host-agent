package config

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nullLogger absorbs logger output during tests without polluting test
// output. Using stdlib log.Logger here would dump to stderr; we don't
// care about parse warnings in the success path.
type nullLogger struct{}

func (nullLogger) Printf(string, ...any) {}

func TestParseLine_DefaultForm(t *testing.T) {
	cases := []struct {
		in           string
		wantKey, val string
		ok           bool
	}{
		{`: "${INTERVAL:=15}"`, "INTERVAL", "15", true},
		{`: "${CPU_TARGET:=70}"`, "CPU_TARGET", "70", true},
		{`: "${MIN_FAN:=20}"  # 20% = 3600 RPM`, "MIN_FAN", "20", true},
		{`: "${SENSOR_INLET_NAME:=Ambient Temp}"`, "SENSOR_INLET_NAME", "Ambient Temp", true},
		{`: "${SENSOR_CPU1_NAME:=}"`, "SENSOR_CPU1_NAME", "", true},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if k != c.wantKey || v != c.val || ok != c.ok {
			t.Errorf("parseLine(%q) = %q,%q,%v want %q,%q,%v",
				c.in, k, v, ok, c.wantKey, c.val, c.ok)
		}
	}
}

func TestParseLine_PlainAssign(t *testing.T) {
	cases := []struct {
		in           string
		wantKey, val string
		ok           bool
	}{
		{`CPU_TARGET=70`, "CPU_TARGET", "70", true},
		{`FAN_GAIN=0.5`, "FAN_GAIN", "0.5", true},
		{`SENSOR_NAME="Inlet Temp"`, "SENSOR_NAME", "Inlet Temp", true},
		{`SENSOR_NAME='Inlet Temp'`, "SENSOR_NAME", "Inlet Temp", true},
		{`export FOO=bar`, "FOO", "bar", true},
		{`# comment only`, "", "", false},
		{``, "", "", false},
		// Command substitution forbidden.
		{`X=$(date)`, "", "", false},
		{`X=` + "`hostname`", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if k != c.wantKey || v != c.val || ok != c.ok {
			t.Errorf("parseLine(%q) = %q,%q,%v want %q,%q,%v",
				c.in, k, v, ok, c.wantKey, c.val, c.ok)
		}
	}
}

func TestStripInlineComment(t *testing.T) {
	cases := map[string]string{
		`KEY=value # trailing`:     `KEY=value`,
		`KEY="a # not a comment"`:  `KEY="a # not a comment"`,
		`KEY='a # not a comment'`:  `KEY='a # not a comment'`,
		`# whole line comment`:     ``,
		`plain text`:               `plain text`,
		`KEY=value`:                `KEY=value`,
	}
	for in, want := range cases {
		if got := stripInlineComment(in); got != want {
			t.Errorf("stripInlineComment(%q) = %q want %q", in, got, want)
		}
	}
}

func TestLoad_DefaultProfileRoundTrip(t *testing.T) {
	// The default.env file is the canonical input format — every
	// supported line shape appears in it. Loading it should produce
	// every documented key with the expected typed value.
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")

	cfg, err := Load(profileDir, "", func(string) (string, bool) { return "", false }, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Spot-check the documented defaults from default.env.
	want := map[string]any{
		"IntervalSec":             15,
		"CPUTarget":               70,
		"CPUDeadband":             3,
		"CPUEmergency":            80,
		"CPUApproachWindow":       10,
		"GPUTarget":               83,
		"GPUDeadband":             2,
		"GPUEmergency":            90,
		"GPUApproachWindow":       7,
		"ActiveGPUTarget":         78,
		"ActiveGPUEmergency":      88,
		"ActiveGPUApproachWindow": 10,
		"HDDTarget":               40,
		"HDDDeadband":             3,
		"HDDEmergency":            50,
		"HDDApproachWindow":       5,
		"HDDReadInterval":         60,
		"SSDTarget":               50,
		"SSDDeadband":             5,
		"SSDEmergency":            65,
		"SSDApproachWindow":       8,
		"MinFan":                  20,
		"MaxFan":                  100,
		"FanGain":                 0.5,
		"DerivativeGain":          1.0,
		"AssistGain":              3.0,
		"DeadbandDriftRate":       3,
		"AdaptAlpha":              0.001,
	}

	got := map[string]any{
		"IntervalSec":             cfg.IntervalSec,
		"CPUTarget":               cfg.CPUTarget,
		"CPUDeadband":             cfg.CPUDeadband,
		"CPUEmergency":            cfg.CPUEmergency,
		"CPUApproachWindow":       cfg.CPUApproachWindow,
		"GPUTarget":               cfg.GPUTarget,
		"GPUDeadband":             cfg.GPUDeadband,
		"GPUEmergency":            cfg.GPUEmergency,
		"GPUApproachWindow":       cfg.GPUApproachWindow,
		"ActiveGPUTarget":         cfg.ActiveGPUTarget,
		"ActiveGPUEmergency":      cfg.ActiveGPUEmergency,
		"ActiveGPUApproachWindow": cfg.ActiveGPUApproachWindow,
		"HDDTarget":               cfg.HDDTarget,
		"HDDDeadband":             cfg.HDDDeadband,
		"HDDEmergency":            cfg.HDDEmergency,
		"HDDApproachWindow":       cfg.HDDApproachWindow,
		"HDDReadInterval":         cfg.HDDReadInterval,
		"SSDTarget":               cfg.SSDTarget,
		"SSDDeadband":             cfg.SSDDeadband,
		"SSDEmergency":            cfg.SSDEmergency,
		"SSDApproachWindow":       cfg.SSDApproachWindow,
		"MinFan":                  cfg.MinFan,
		"MaxFan":                  cfg.MaxFan,
		"FanGain":                 cfg.FanGain,
		"DerivativeGain":          cfg.DerivativeGain,
		"AssistGain":              cfg.AssistGain,
		"DeadbandDriftRate":       cfg.DeadbandDriftRate,
		"AdaptAlpha":              cfg.AdaptAlpha,
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("Load default: %s = %v want %v", k, got[k], v)
		}
	}
}

func TestLoad_ModelOverridesDefault(t *testing.T) {
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")

	// r410 sets MIN_FAN=20 and SENSOR_INLET_NAME=Ambient Temp. default
	// has MIN_FAN=20 already, so test r730xd which sets MIN_FAN=10.
	cfg, err := Load(profileDir, "r730xd", func(string) (string, bool) { return "", false }, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinFan != 10 {
		t.Errorf("r730xd MinFan = %d want 10", cfg.MinFan)
	}
	// default.env emergency thresholds still come through.
	if cfg.CPUEmergency != 80 {
		t.Errorf("CPUEmergency from default = %d want 80", cfg.CPUEmergency)
	}
	if cfg.SensorCPU1Name != "Temp" || cfg.SensorCPU1ID != "26" {
		t.Errorf("SENSOR_CPU1 = %q/%q want Temp/26", cfg.SensorCPU1Name, cfg.SensorCPU1ID)
	}
}

func TestLoad_EnvOverridesProfiles(t *testing.T) {
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")

	env := map[string]string{
		"MIN_FAN":      "30", // override r730xd's 10
		"CPU_TARGET":   "65", // override default's 70
		"GPU_AWARE":    "false",
		"NONEXISTENT":  "x", // should be ignored — not a known field
	}
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	cfg, err := Load(profileDir, "r730xd", lookup, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinFan != 30 {
		t.Errorf("env override MinFan = %d want 30", cfg.MinFan)
	}
	if cfg.CPUTarget != 65 {
		t.Errorf("env override CPUTarget = %d want 65", cfg.CPUTarget)
	}
	if cfg.GPUAware != "false" {
		t.Errorf("env GPUAware = %q want false", cfg.GPUAware)
	}
}

func TestLoad_UnknownModelFallsBack(t *testing.T) {
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")

	cfg, err := Load(profileDir, "fictional_xyz_99", func(string) (string, bool) { return "", false }, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Should hit only default.env — MinFan=20.
	if cfg.MinFan != 20 {
		t.Errorf("unknown model MinFan = %d want 20", cfg.MinFan)
	}
}

func TestParseProfile_WarnsOnGarbage(t *testing.T) {
	r := strings.NewReader(`
KEY=value
this is garbage
ANOTHER=2
`)
	got := map[string]string{}
	setDefault := func(k, v string) { got[k] = v }
	var logged []string
	logger := captureLogger{out: &logged}

	if err := parseProfile(r, "test", setDefault, logger); err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	if got["KEY"] != "value" || got["ANOTHER"] != "2" {
		t.Errorf("got = %v want KEY=value ANOTHER=2", got)
	}
	if len(logged) != 1 {
		t.Errorf("expected 1 warning, got %d: %v", len(logged), logged)
	}
}

type captureLogger struct {
	out *[]string
}

func (c captureLogger) Printf(format string, v ...any) {
	*c.out = append(*c.out, format)
}

// findRepoRoot locates the host-agent/ directory containing this test
// by walking up from the test's working directory until it finds the
// `profiles/` subdir.
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

// Compile-time check we satisfy stdlib log.Logger duck-shape.
var _ Logger = (*log.Logger)(nil)
