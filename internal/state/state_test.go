package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWrite_ByteIdenticalToGolden(t *testing.T) {
	// The bash original writes:
	//   base_speed=37.4221
	//   last_speed=42
	//   samples=18421
	//   last_updated=2026-05-15T17:30:00Z
	// Our Write MUST produce the exact same bytes — round-trippable.
	dir := t.TempDir()
	path := filepath.Join(dir, "base")

	st := State{
		BaseSpeed:   37.4221,
		LastSpeed:   42,
		Samples:     18421,
		LastUpdated: time.Date(2026, 5, 15, 17, 30, 0, 0, time.UTC),
	}
	if err := Write(path, st); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/state.golden")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("byte mismatch with golden:\n--got--\n%s--want--\n%s", got, want)
	}
}

func TestRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base")

	in := State{
		BaseSpeed:   55.1234,
		LastSpeed:   62,
		Samples:     999,
		LastUpdated: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	}
	if err := Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out.BaseSpeed != in.BaseSpeed {
		t.Errorf("BaseSpeed: got %g want %g", out.BaseSpeed, in.BaseSpeed)
	}
	if out.LastSpeed != in.LastSpeed {
		t.Errorf("LastSpeed: got %d want %d", out.LastSpeed, in.LastSpeed)
	}
	if out.Samples != in.Samples {
		t.Errorf("Samples: got %d want %d", out.Samples, in.Samples)
	}
	if !out.LastUpdated.Equal(in.LastUpdated) {
		t.Errorf("LastUpdated: got %v want %v", out.LastUpdated, in.LastUpdated)
	}
}

func TestRead_GoldenFile(t *testing.T) {
	// Direct read of the bash-format golden — verifies we parse the
	// exact bytes the bash original would have written.
	st, err := Read("testdata/state.golden")
	if err != nil {
		t.Fatalf("Read golden: %v", err)
	}
	if st.BaseSpeed != 37.4221 {
		t.Errorf("BaseSpeed: got %g want 37.4221", st.BaseSpeed)
	}
	if st.LastSpeed != 42 {
		t.Errorf("LastSpeed: got %d want 42", st.LastSpeed)
	}
	if st.Samples != 18421 {
		t.Errorf("Samples: got %d want 18421", st.Samples)
	}
	want := time.Date(2026, 5, 15, 17, 30, 0, 0, time.UTC)
	if !st.LastUpdated.Equal(want) {
		t.Errorf("LastUpdated: got %v want %v", st.LastUpdated, want)
	}
}

func TestRead_MissingFile(t *testing.T) {
	_, err := Read("/nonexistent/path/state")
	if err == nil {
		t.Fatal("expected error reading missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("want IsNotExist, got %v", err)
	}
}

func TestRead_EmptyFile_ReturnsZeroState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.BaseSpeed != 0 || st.LastSpeed != 0 || st.Samples != 0 || !st.LastUpdated.IsZero() {
		t.Errorf("expected zero state, got %+v", st)
	}
}

func TestWrite_AtomicRename(t *testing.T) {
	// After Write, no .tmp files should be left behind in the dir.
	dir := t.TempDir()
	path := filepath.Join(dir, "base")
	st := State{BaseSpeed: 1.0, LastSpeed: 20, Samples: 1,
		LastUpdated: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := Write(path, st); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "base" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}
