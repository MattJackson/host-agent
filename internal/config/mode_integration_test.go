package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pq/docker-server/host-agent/internal/mode"
)

// writeTempProfile writes a synthetic profile to a temp directory and returns it.
func writeTempProfile(t *testing.T, contents string) (profileDir string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "default.env"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestIntegration_Load_NoEnv_NoMode_v1Behavior tests the §14 row
// "No env vars set → Profile defaults used (v1 behavior, ApplyMode noop)".
// Without HOST_AGENT_MODE set, ApplyMode does NOT inject mode-derived
// values — cfg keeps exactly what Load() resolved from profiles.
func TestIntegration_Load_NoEnv_NoMode_v1Behavior(t *testing.T) {
	profileDir := writeTempProfile(t, `: "${CPU_TARGET:=99}"`)

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, set, _ := ApplyMode(cfg)

	if set {
		t.Error("set = true want false (HOST_AGENT_MODE not set)")
	}

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced (default fallback)", resolved)
	}

	// CPU was set by profile and stays. Other classes were NOT set by
	// profile and stay at zero (Go default int). v1 behavior: profile
	// is authoritative; mode is dormant.
	if cfg.CPUTarget != 99 {
		t.Errorf("CPUTarget = %d want 99 (profile)", cfg.CPUTarget)
	}
	if cfg.GPUTarget != 0 || cfg.HDDTarget != 0 || cfg.SSDTarget != 0 {
		t.Errorf("GPU/HDD/SSD = %d/%d/%d want all 0 (profile didn't set, mode dormant)", cfg.GPUTarget, cfg.HDDTarget, cfg.SSDTarget)
	}
}

// TestIntegration_Load_Mode_Balanced_NoProfileTargets tests the §14 row
// "HOST_AGENT_MODE=balanced → v2 adaptive on for all classes".
func TestIntegration_Load_Mode_Balanced_NoProfileTargets(t *testing.T) {
	profileDir := writeTempProfile(t, "")
	t.Setenv("HOST_AGENT_MODE", "balanced")

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, set, _ := ApplyMode(cfg)

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced", resolved)
	}
	if !set {
		t.Error("set = false want true")
	}

	wantCPU := 65
	wantGPUDeadband := 3
	if cfg.CPUTarget != wantCPU {
		t.Errorf("CPUTarget = %d want %d (Balanced)", cfg.CPUTarget, wantCPU)
	}
	if cfg.GPUDeadband != wantGPUDeadband {
		t.Errorf("GPUDeadband = %d want %d (Balanced)", cfg.GPUDeadband, wantGPUDeadband)
	}

	wantHDDTarget := 38
	wantSSDTarget := 50
	if cfg.HDDTarget != wantHDDTarget || cfg.HDDDeadband != 3 {
		t.Errorf("HDD target/deadband = %d/%d want %d/3 (Balanced)", cfg.HDDTarget, cfg.HDDDeadband, wantHDDTarget)
	}
	if cfg.SSDTarget != wantSSDTarget || cfg.SSDDeadband != 3 {
		t.Errorf("SSD target/deadband = %d/%d want %d/3 (Balanced)", cfg.SSDTarget, cfg.SSDDeadband, wantSSDTarget)
	}
}

// TestIntegration_Load_Mode_Plus_PerClassEnvOverride tests the §14 row
// "HOST_AGENT_MODE=balanced + CPU_TARGET=70 → v2 adaptive on for GPU/HDD/SSD;
// CPU uses fixed 70".
func TestIntegration_Load_Mode_Plus_PerClassEnvOverride(t *testing.T) {
	profileDir := writeTempProfile(t, "")
	t.Setenv("HOST_AGENT_MODE", "balanced")
	t.Setenv("CPU_TARGET", "70")

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, _, _ := ApplyMode(cfg)

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced", resolved)
	}

	if cfg.CPUTarget != 70 {
		t.Errorf("CPUTarget = %d want 70 (env override)", cfg.CPUTarget)
	}

	if cfg.CPUDeadband != 3 {
		t.Errorf("CPUDeadband = %d want 3 (mode-derived since CPU_DEADBAND unset)", cfg.CPUDeadband)
	}

	wantGPU := 80
	wantGPUDeadband := 3
	if cfg.GPUTarget != wantGPU || cfg.GPUDeadband != wantGPUDeadband {
		t.Errorf("GPU target/deadband = %d/%d want %d/3 (Balanced)", cfg.GPUTarget, cfg.GPUDeadband, wantGPU)
	}

	wantHDDTarget := 38
	if cfg.HDDTarget != wantHDDTarget || cfg.HDDDeadband != 3 {
		t.Errorf("HDD target/deadband = %d/%d want %d/3 (Balanced)", cfg.HDDTarget, cfg.HDDDeadband, wantHDDTarget)
	}

	wantSSDTarget := 50
	if cfg.SSDTarget != wantSSDTarget || cfg.SSDDeadband != 3 {
		t.Errorf("SSD target/deadband = %d/%d want %d/3 (Balanced)", cfg.SSDTarget, cfg.SSDDeadband, wantSSDTarget)
	}
}

// TestIntegration_Load_Mode_ReplacesProfileTargets tests that v2 mode
// EXPLICITLY overrides profile-set per-class targets. Profile values are
// v1 fallback only — when HOST_AGENT_MODE is set, mode-derived values
// replace them. Operators wanting a fixed target use an env var, not a
// profile entry. (§4 + §14 of the design.)
func TestIntegration_Load_Mode_ReplacesProfileTargets(t *testing.T) {
	profileDir := writeTempProfile(t, `: "${GPU_TARGET:=80}"`)
	t.Setenv("HOST_AGENT_MODE", "max-cool")

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, _, _ := ApplyMode(cfg)

	if resolved != mode.MaxCool {
		t.Errorf("resolved = %v want MaxCool", resolved)
	}

	// Profile GPU_TARGET=80 yields to mode-derived MaxCool PreferredLow=65.
	wantGPU := 75
	if cfg.GPUTarget != wantGPU {
		t.Errorf("GPUTarget = %d want %d (mode-derived, profile yielded)", cfg.GPUTarget, wantGPU)
	}

	wantCPU := 55
	wantGPUDeadband := 2
	if cfg.CPUTarget != wantCPU || cfg.GPUDeadband != wantGPUDeadband {
		t.Errorf("CPU target = %d (MaxCool=55), GPU deadband = %d (MaxCool=2)", cfg.CPUTarget, cfg.GPUDeadband)
	}

	wantHDDTarget := 32
	wantSSDTarget := 45
	if cfg.HDDTarget != wantHDDTarget || cfg.SSDTarget != wantSSDTarget {
		t.Errorf("HDD/SSD targets = %d/%d want %d/%d (MaxCool-derived)", cfg.HDDTarget, cfg.SSDTarget, wantHDDTarget, wantSSDTarget)
	}
}

// TestIntegration_Load_Mode_BadValue_FallsBack tests that on bad mode value,
// ApplyMode returns an error BUT still applies Balanced fallback.
func TestIntegration_Load_Mode_BadValue_FallsBack(t *testing.T) {
	profileDir := writeTempProfile(t, "")
	t.Setenv("HOST_AGENT_MODE", "garbage")

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, set, err2 := ApplyMode(cfg)

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced (fallback)", resolved)
	}
	if !set {
		t.Error("set = false want true")
	}
	if err2 == nil {
		t.Fatal("err = nil, expected error for invalid mode")
	}

	wantCPU := 65
	wantGPUDeadband := 3
	if cfg.CPUTarget != wantCPU {
		t.Errorf("CPUTarget = %d want %d (fallback applied)", cfg.CPUTarget, wantCPU)
	}
	if cfg.GPUDeadband != wantGPUDeadband {
		t.Errorf("GPUDeadband = %d want %d", cfg.GPUDeadband, wantGPUDeadband)
	}

	wantHDDTarget := 38
	wantSSDTarget := 50
	if cfg.HDDTarget != wantHDDTarget || cfg.SSDTarget != wantSSDTarget {
		t.Errorf("HDD/SSD targets = %d/%d want %d/%d (fallback applied)", cfg.HDDTarget, cfg.SSDTarget, wantHDDTarget, wantSSDTarget)
	}
}

// TestIntegration_Load_Mode_EnvVarsWinOverProfile demonstrates the
// canonical v2 mixing rule: when HOST_AGENT_MODE is set, env-var per-
// class overrides win; profile entries yield to mode-derived values.
//
// This replaces the old "v1 defaults preserved" test — that test's
// premise (profile entries count as overrides) was the v1.5 prototype
// behavior, not the shipping v2 design per §4 + §14.
func TestIntegration_Load_Mode_EnvVarsWinOverProfile(t *testing.T) {
	// Profile sets all four classes to sentinels (v1-style).
	profileDir := writeTempProfile(t, `
: "${CPU_TARGET:=91}"
: "${GPU_TARGET:=92}"
: "${HDD_TARGET:=93}"
: "${SSD_TARGET:=94}"
`)
	// Operator opts into v2 + pins CPU and GPU via env var.
	t.Setenv("HOST_AGENT_MODE", "balanced")
	t.Setenv("CPU_TARGET", "60")
	t.Setenv("GPU_TARGET", "75")

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, _, _ := ApplyMode(cfg)

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced", resolved)
	}

	// CPU + GPU env-set → those values win.
	if cfg.CPUTarget != 60 {
		t.Errorf("CPUTarget = %d want 60 (env override)", cfg.CPUTarget)
	}
	if cfg.GPUTarget != 75 {
		t.Errorf("GPUTarget = %d want 75 (env override)", cfg.GPUTarget)
	}

	// HDD + SSD env-unset, profile-set → profile yields, mode-derived wins.
	if cfg.HDDTarget != 38 {
		t.Errorf("HDDTarget = %d want 38 (mode Balanced, profile yielded)", cfg.HDDTarget)
	}
	if cfg.SSDTarget != 50 {
		t.Errorf("SSDTarget = %d want 50 (mode Balanced, profile yielded)", cfg.SSDTarget)
	}

	// All deadbands are mode-derived (no per-class deadband env vars set).
	if cfg.CPUDeadband != 3 || cfg.GPUDeadband != 3 || cfg.HDDDeadband != 3 || cfg.SSDDeadband != 3 {
		t.Errorf("All deadbands = %d/%d/%d/%d want all 3 (Balanced-derived)", cfg.CPUDeadband, cfg.GPUDeadband, cfg.HDDDeadband, cfg.SSDDeadband)
	}
}
