package adaptive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

func TestLoadState_MissingFile(t *testing.T) {
	path := "/nonexistent/path/that/does/not/exist.json"
	s, loaded, err := LoadState(path)
	if s.Version != 0 || s.Mode != "" || len(s.Classes) != 0 {
		t.Errorf("expected zero State on missing file")
	}
	if loaded {
		t.Errorf("expected loaded=false for missing file")
	}
	if err != nil {
		t.Errorf("expected no error for missing file, got %v", err)
	}
}

func TestLoadState_EmptyDir(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "adaptive.json")
	s, loaded, err := LoadState(path)
	if s.Version != 0 || s.Mode != "" || len(s.Classes) != 0 {
		t.Errorf("expected zero State on missing file in temp dir")
	}
	if loaded {
		t.Errorf("expected loaded=false for missing file")
	}
	if err != nil {
		t.Errorf("expected no error for missing file, got %v", err)
	}
}

func TestSaveState_RoundTrip(t *testing.T) {
	fixedTime := time.Date(2026, 5, 17, 5, 30, 0, 0, time.UTC)
	expected := State{
		Version:    stateSchemaVersion,
		Mode:       mode.Balanced,
		LastUpdate: fixedTime,
		Classes: map[envelope.Class]ClassState{
			envelope.PassiveGPU: {
				TargetCelsius:        73.5,
				DeadbandCelsius:      3.2,
				VarianceObservedEWMA: 1.8,
				InletBaselineCelsius: 22.4,
				LastUpdate:           fixedTime,
				LastChangeDirection:  +1,
			},
		},
	}

	path := filepath.Join(t.TempDir(), "adaptive.json")
	if err := SaveState(path, expected); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	actual, loaded, err := LoadState(path)
	if !loaded {
		t.Fatal("expected load to succeed")
	}
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	if actual.Version != expected.Version {
		t.Errorf("Version mismatch: want %d, got %d", expected.Version, actual.Version)
	}
	if actual.Mode != expected.Mode {
		t.Errorf("Mode mismatch: want %s, got %s", expected.Mode, actual.Mode)
	}
	if !actual.LastUpdate.Equal(expected.LastUpdate) {
		t.Errorf("LastUpdate mismatch")
	}

	expectedGPU, ok := expected.Classes[envelope.PassiveGPU]
	if !ok {
		t.Fatal("expected PassiveGPU in Classes map")
	}
	actualGPU, ok := actual.Classes[envelope.PassiveGPU]
	if !ok {
		t.Fatal("PassiveGPU missing from loaded Classes map")
	}

	if actualGPU.TargetCelsius != expectedGPU.TargetCelsius {
		t.Errorf("TargetCelsius mismatch: want %v, got %v", expectedGPU.TargetCelsius, actualGPU.TargetCelsius)
	}
	if actualGPU.DeadbandCelsius != expectedGPU.DeadbandCelsius {
		t.Errorf("DeadbandCelsius mismatch: want %v, got %v", expectedGPU.DeadbandCelsius, actualGPU.DeadbandCelsius)
	}
	if actualGPU.VarianceObservedEWMA != expectedGPU.VarianceObservedEWMA {
		t.Errorf("VarianceObservedEWMA mismatch: want %v, got %v", expectedGPU.VarianceObservedEWMA, actualGPU.VarianceObservedEWMA)
	}
	if actualGPU.InletBaselineCelsius != expectedGPU.InletBaselineCelsius {
		t.Errorf("InletBaselineCelsius mismatch: want %v, got %v", expectedGPU.InletBaselineCelsius, actualGPU.InletBaselineCelsius)
	}
	if !actualGPU.LastUpdate.Equal(expectedGPU.LastUpdate) {
		t.Errorf("Class LastUpdate mismatch")
	}
	if actualGPU.LastChangeDirection != expectedGPU.LastChangeDirection {
		t.Errorf("LastChangeDirection mismatch: want %d, got %d", expectedGPU.LastChangeDirection, actualGPU.LastChangeDirection)
	}
}

func TestSaveState_CreatesParentDir(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "a", "b", "c", "adaptive.json")
	s := NewState(mode.Balanced)
	if err := SaveState(path, s); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("expected file to exist after SaveState")
	}
}

func TestSaveState_AtomicRename_NoLeftoverTmp(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "adaptive.json")
	s := NewState(mode.Balanced)
	if err := SaveState(path, s); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("expected .tmp file to be removed after successful Save, found: %v", err)
	}
}

func TestLoadState_CorruptJSON(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "adaptive.json")
	if err := os.WriteFile(path, []byte(`{not json}`), 0o644); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	expectedRecovery := NewState(mode.Default)
	actual, loaded, err := LoadState(path)
	if loaded {
		t.Fatal("expected loaded=false for corrupt JSON")
	}
	if err == nil {
		t.Fatal("expected error for corrupt JSON")
	}

	if actual.Version != expectedRecovery.Version {
		t.Errorf("recovered Version mismatch: want %d, got %d", expectedRecovery.Version, actual.Version)
	}
	if actual.Mode != expectedRecovery.Mode {
		t.Errorf("recovered Mode mismatch: want %s, got %s", expectedRecovery.Mode, actual.Mode)
	}
	if len(actual.Classes) != 0 {
		t.Errorf("expected empty Classes map on recovery, got %d entries", len(actual.Classes))
	}
}

func TestLoadState_VersionMismatch(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "adaptive.json")
	corruptVersion := `{"version": 99, "mode": "balanced", "last_update": "2026-05-17T05:30:00Z", "classes": {}}`
	if err := os.WriteFile(path, []byte(corruptVersion), 0o644); err != nil {
		t.Fatalf("failed to write version-mismatch file: %v", err)
	}

	expectedRecovery := NewState(mode.Default)
	actual, loaded, err := LoadState(path)
	if loaded {
		t.Fatal("expected loaded=false for version mismatch")
	}
	if err == nil {
		t.Fatal("expected error for version mismatch")
	}

	if actual.Version != expectedRecovery.Version {
		t.Errorf("recovered Version mismatch: want %d, got %d", expectedRecovery.Version, actual.Version)
	}
	if actual.Mode != expectedRecovery.Mode {
		t.Errorf("recovered Mode mismatch: want %s, got %s", expectedRecovery.Mode, actual.Mode)
	}
	if len(actual.Classes) != 0 {
		t.Errorf("expected empty Classes map on recovery, got %d entries", len(actual.Classes))
	}
}

func TestLoadState_NilClassesMap_Initialized(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "adaptive.json")
	noClassesJSON := `{"version": 1, "mode": "balanced", "last_update": "2026-05-17T05:30:00Z"}`
	if err := os.WriteFile(path, []byte(noClassesJSON), 0o644); err != nil {
		t.Fatalf("failed to write JSON without classes field: %v", err)
	}

	actual, loaded, err := LoadState(path)
	if !loaded {
		t.Fatal("expected load to succeed")
	}
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	if actual.Classes == nil {
		t.Fatal("expected Classes map to be initialized as empty map, got nil")
	}

	if len(actual.Classes) != 0 {
		t.Errorf("expected empty Classes map, got %d entries", len(actual.Classes))
	}
}

func TestSaveState_OverwriteExisting(t *testing.T) {
	tmpdir := t.TempDir()
	path := filepath.Join(tmpdir, "adaptive.json")

	firstSave := NewState(mode.Balanced)
	firstSave.LastUpdate = time.Date(2026, 5, 17, 5, 30, 0, 0, time.UTC)
	if err := SaveState(path, firstSave); err != nil {
		t.Fatalf("first SaveState failed: %v", err)
	}

	secondSave := NewState(mode.MaxCool)
	secondSave.LastUpdate = time.Date(2026, 5, 17, 6, 0, 0, 0, time.UTC)
	if err := SaveState(path, secondSave); err != nil {
		t.Fatalf("second SaveState failed: %v", err)
	}

	actual, loaded, err := LoadState(path)
	if !loaded {
		t.Fatal("expected load to succeed after overwrite")
	}
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	if actual.Mode != mode.MaxCool {
		t.Errorf("expected Mode=MaxCool after overwrite, got %s", actual.Mode)
	}
	if !actual.LastUpdate.Equal(secondSave.LastUpdate) {
		t.Errorf("LastUpdate did not match second save")
	}
}

func TestRoundTrip_AllFourClasses(t *testing.T) {
	fixedTime := time.Date(2026, 5, 17, 5, 30, 0, 0, time.UTC)
	saved := State{
		Version:    stateSchemaVersion,
		Mode:       mode.Eco,
		LastUpdate: fixedTime,
		Classes: map[envelope.Class]ClassState{
			envelope.CPU: {
				TargetCelsius:        64.0,
				DeadbandCelsius:      3.5,
				VarianceObservedEWMA: 1.2,
				InletBaselineCelsius: 22.0,
				LastUpdate:           fixedTime,
				LastChangeDirection:  -1,
			},
			envelope.PassiveGPU: {
				TargetCelsius:        73.5,
				DeadbandCelsius:      4.0,
				VarianceObservedEWMA: 1.8,
				InletBaselineCelsius: 22.4,
				LastUpdate:           fixedTime,
				LastChangeDirection:  +1,
			},
			envelope.HDD: {
				TargetCelsius:        38.0,
				DeadbandCelsius:      2.5,
				VarianceObservedEWMA: 0.9,
				InletBaselineCelsius: 21.8,
				LastUpdate:           fixedTime,
				LastChangeDirection:  0,
			},
			envelope.SSD: {
				TargetCelsius:        50.5,
				DeadbandCelsius:      3.0,
				VarianceObservedEWMA: 1.5,
				InletBaselineCelsius: 22.2,
				LastUpdate:           fixedTime,
				LastChangeDirection:  +1,
			},
		},
	}

	path := filepath.Join(t.TempDir(), "adaptive.json")
	if err := SaveState(path, saved); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	actual, loaded, err := LoadState(path)
	if !loaded {
		t.Fatal("expected load to succeed")
	}
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	allClasses := []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD}
	for _, c := range allClasses {
		savedCS, ok := saved.Classes[c]
		if !ok {
			t.Fatalf("expected %s in saved Classes map", c)
		}
		actualCS, ok := actual.Classes[c]
		if !ok {
			t.Fatalf("%s missing from loaded Classes map", c)
		}

		if actualCS.TargetCelsius != savedCS.TargetCelsius {
			t.Errorf("%s TargetCelsius: want %v, got %v", c, savedCS.TargetCelsius, actualCS.TargetCelsius)
		}
		if actualCS.DeadbandCelsius != savedCS.DeadbandCelsius {
			t.Errorf("%s DeadbandCelsius: want %v, got %v", c, savedCS.DeadbandCelsius, actualCS.DeadbandCelsius)
		}
		if actualCS.VarianceObservedEWMA != savedCS.VarianceObservedEWMA {
			t.Errorf("%s VarianceObservedEWMA: want %v, got %v", c, savedCS.VarianceObservedEWMA, actualCS.VarianceObservedEWMA)
		}
		if actualCS.InletBaselineCelsius != savedCS.InletBaselineCelsius {
			t.Errorf("%s InletBaselineCelsius: want %v, got %v", c, savedCS.InletBaselineCelsius, actualCS.InletBaselineCelsius)
		}
		if !actualCS.LastUpdate.Equal(savedCS.LastUpdate) {
			t.Errorf("%s LastUpdate mismatch", c)
		}
		if actualCS.LastChangeDirection != savedCS.LastChangeDirection {
			t.Errorf("%s LastChangeDirection: want %d, got %d", c, savedCS.LastChangeDirection, actualCS.LastChangeDirection)
		}
	}
}
