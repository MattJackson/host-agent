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
// "No env vars set → Profile defaults used (v1 behavior)".
func TestIntegration_Load_NoEnv_NoMode_v1Behavior(t *testing.T) {
	profileDir := writeTempProfile(t, `: "${CPU_TARGET:=99}"`)

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, _, _ := ApplyMode(cfg)

	if cfg.CPUTarget != 99 {
		t.Errorf("CPUTarget = %d want 99 (profile sentinel)", cfg.CPUTarget)
	}

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced", resolved)
	}

	if cfg.CPUDeadband != 3 {
		t.Errorf("CPUDeadband = %d want 3 (Balanced default)", cfg.CPUDeadband)
	}

	if cfg.GPUTarget != 72 || cfg.GPUDeadband != 3 {
		t.Errorf("GPU target/deadband = %d/%d want 72/3 (Balanced fallback)", cfg.GPUTarget, cfg.GPUDeadband)
	}

	if cfg.HDDTarget != 38 || cfg.HDDDeadband != 3 {
		t.Errorf("HDD target/deadband = %d/%d want 38/3 (Balanced fallback)", cfg.HDDTarget, cfg.HDDDeadband)
	}

	if cfg.SSDTarget != 50 || cfg.SSDDeadband != 3 {
		t.Errorf("SSD target/deadband = %d/%d want 50/3 (Balanced fallback)", cfg.SSDTarget, cfg.SSDDeadband)
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

	wantGPU := 72
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

// TestIntegration_Load_Mode_Plus_ProfileOverride tests that profile-set per-class
// entries count as "explicit override" same as env vars (§14 migration matrix).
func TestIntegration_Load_Mode_Plus_ProfileOverride(t *testing.T) {
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

	wantGPU := 80
	if cfg.GPUTarget != wantGPU {
		t.Errorf("GPUTarget = %d want %d (profile override wins over mode-derived)", cfg.GPUTarget, wantGPU)
	}

	wantCPU := 55
	wantGPUDeadband := 2
	if cfg.CPUTarget != wantCPU || cfg.GPUDeadband != wantGPUDeadband {
		t.Errorf("CPU target = %d (MaxCool), GPU deadband = %d (MaxCool)", cfg.CPUTarget, cfg.GPUDeadband)
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

// TestIntegration_Load_Mode_NoChange_v1Defaults_Preserved tests the §14 row
// confirming full v1-compatibility: a host that has all four classes explicitly
// configured in its profile is unaffected by switching to v2.
func TestIntegration_Load_Mode_NoChange_v1Defaults_Preserved(t *testing.T) {
	profileDir := writeTempProfile(t, `
: "${CPU_TARGET:=91}"
: "${GPU_TARGET:=92}"
: "${HDD_TARGET:=93}"
: "${SSD_TARGET:=94}"
`)
	t.Setenv("HOST_AGENT_MODE", "balanced")

	cfg, err := Load(profileDir, "", os.LookupEnv, nullLogger{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, _, _ := ApplyMode(cfg)

	if resolved != mode.Balanced {
		t.Errorf("resolved = %v want Balanced", resolved)
	}

	wantCPU := 91
	wantGPU := 92
	wantHDD := 93
	wantSSD := 94
	if cfg.CPUTarget != wantCPU {
		t.Errorf("CPUTarget = %d want %d (profile sentinel preserved)", cfg.CPUTarget, wantCPU)
	}
	if cfg.GPUTarget != wantGPU {
		t.Errorf("GPUTarget = %d want %d (profile sentinel preserved)", cfg.GPUTarget, wantGPU)
	}
	if cfg.HDDTarget != wantHDD {
		t.Errorf("HDDTarget = %d want %d (profile sentinel preserved)", cfg.HDDTarget, wantHDD)
	}
	if cfg.SSDTarget != wantSSD {
		t.Errorf("SSDTarget = %d want %d (profile sentinel preserved)", cfg.SSDTarget, wantSSD)
	}

	if cfg.CPUDeadband != 3 || cfg.GPUDeadband != 3 || cfg.HDDDeadband != 3 || cfg.SSDDeadband != 3 {
		t.Errorf("All deadbands = %d/%d/%d/%d want all 3 (Balanced-derived)", cfg.CPUDeadband, cfg.GPUDeadband, cfg.HDDDeadband, cfg.SSDDeadband)
	}
}
