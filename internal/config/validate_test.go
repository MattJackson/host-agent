package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capturingLogger records every Printf line so tests can assert on
// emitted warnings.
type capturingLogger struct{ lines []string }

func (c *capturingLogger) Printf(format string, v ...any) {
	c.lines = append(c.lines, strings.TrimSpace(fmt.Sprintf(format, v...)))
}

// validBaseConfig returns a Config that passes Validate; tests mutate one
// field to assert that the corresponding invariant is enforced.
func validBaseConfig() *Config {
	return &Config{
		IntervalSec: 15,
		MinFan:      20,
		MaxFan:      100,
		FanGain:     0.5,
		AdaptAlpha:  0.001,
		CPUTarget:   70, CPUDeadband: 3, CPUEmergency: 80,
		GPUTarget: 83, GPUDeadband: 2, GPUEmergency: 90,
		HDDTarget: 40, HDDDeadband: 3, HDDEmergency: 50,
		SSDTarget: 50, SSDDeadband: 5, SSDEmergency: 65,
	}
}

func TestValidate_AcceptsValidConfig(t *testing.T) {
	if err := Validate(validBaseConfig()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidate_AcceptsDefaultProfile(t *testing.T) {
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")
	cfg, err := Load(profileDir, "", func(string) (string, bool) { return "", false }, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("default profile must pass Validate: %v", err)
	}
}

// TestValidate_AcceptsAllModesOnDefaultProfile is the regression guard for
// the v0.3.10 critical: a too-strict `target+deadband < emergency` invariant
// rejected eco (CPU 75+5 vs 80, SSD 60+5 vs 65) and left min-noise on a 1°C
// margin. Every advertised mode must boot cleanly on the default profile.
func TestValidate_AcceptsAllModesOnDefaultProfile(t *testing.T) {
	repoRoot := findRepoRoot(t)
	profileDir := filepath.Join(repoRoot, "profiles")
	for _, m := range []string{"max-cool", "balanced", "min-noise", "eco"} {
		t.Run(m, func(t *testing.T) {
			t.Setenv("HOST_AGENT_MODE", m)
			cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, _, err := ApplyMode(cfg); err != nil {
				t.Fatalf("ApplyMode(%s): %v", m, err)
			}
			if err := Validate(cfg); err != nil {
				t.Fatalf("mode %s must pass Validate on the default profile, got: %v", m, err)
			}
		})
	}
}

func TestValidate_RejectsUnsafe(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // substring expected in the error
	}{
		{"MinFan>MaxFan", func(c *Config) { c.MinFan = 80; c.MaxFan = 20 }, "fan range"},
		{"MinFan zero", func(c *Config) { c.MinFan = 0 }, "fan range"},
		{"MaxFan over 100", func(c *Config) { c.MaxFan = 120 }, "fan range"},
		{"interval zero", func(c *Config) { c.IntervalSec = 0 }, "INTERVAL"},
		{"fan gain zero", func(c *Config) { c.FanGain = 0 }, "FAN_GAIN"},
		{"alpha out of range", func(c *Config) { c.AdaptAlpha = 1.5 }, "ADAPT_ALPHA"},
		{"cpu target==emergency", func(c *Config) { c.CPUTarget = 80; c.CPUEmergency = 80 }, "CPU"},
		{"cpu target>emergency", func(c *Config) { c.CPUTarget = 85; c.CPUEmergency = 80 }, "CPU"},
		{"gpu emergency zero", func(c *Config) { c.GPUEmergency = 0 }, "GPU"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBaseConfig()
			tt.mutate(c)
			err := Validate(c)
			if err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should mention %q", err.Error(), tt.want)
			}
		})
	}
}

// TestLoad_WarnsOnMalformedNumeric verifies a present-but-unparseable
// numeric value is surfaced as a WARN (pre-v0.3.9 it was swallowed) and
// the field keeps its default.
func TestLoad_WarnsOnMalformedNumeric(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "default.env"),
		[]byte("FAN_GAIN=notanumber\nMIN_FAN=oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := &capturingLogger{}
	cfg, err := Load(dir, "", func(string) (string, bool) { return "", false }, log)
	if err != nil {
		t.Fatalf("Load should resolve (validation is separate): %v", err)
	}
	if cfg.FanGain != 0 {
		t.Errorf("malformed FAN_GAIN should leave default 0, got %g", cfg.FanGain)
	}
	joined := strings.Join(log.lines, "\n")
	if !strings.Contains(joined, "FAN_GAIN") || !strings.Contains(joined, "MIN_FAN") {
		t.Errorf("expected WARN lines for FAN_GAIN and MIN_FAN, got:\n%s", joined)
	}
}

// TestLoad_PureEnvOnly verifies a deployment with no profile files at all
// (everything supplied via environment) resolves and validates.
func TestLoad_PureEnvOnly(t *testing.T) {
	dir := t.TempDir() // empty: no default.env, no model profile
	env := map[string]string{
		"INTERVAL": "15", "MIN_FAN": "20", "MAX_FAN": "100",
		"FAN_GAIN": "0.5", "ADAPT_ALPHA": "0.001",
		"CPU_TARGET": "70", "CPU_DEADBAND": "3", "CPU_EMERGENCY": "80",
		"GPU_TARGET": "83", "GPU_DEADBAND": "2", "GPU_EMERGENCY": "90",
		"HDD_TARGET": "40", "HDD_DEADBAND": "3", "HDD_EMERGENCY": "50",
		"SSD_TARGET": "50", "SSD_DEADBAND": "5", "SSD_EMERGENCY": "65",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err := Load(dir, "", lookup, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinFan != 20 || cfg.CPUEmergency != 80 {
		t.Errorf("pure-env resolution wrong: MinFan=%d CPUEmergency=%d", cfg.MinFan, cfg.CPUEmergency)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("pure-env config must validate: %v", err)
	}
}
