package adaptive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// stateSchemaVersion is the JSON schema version. Bumped when the
// on-disk format changes incompatibly. Loaders that see an unrecognized
// version fall back to empty state.
const stateSchemaVersion = 1

// DefaultStatePath is where the adaptive controller persists its
// learned state. Lives alongside fan-controller's base file in
// /var/lib/host-agent/state/, which is bind-mounted from the host
// via the standard host-agent compose template — so state survives
// container recreate / image upgrade. The earlier path
// /var/lib/host-agent/state/adaptive.json was inside the ephemeral
// container layer and got wiped on every restart, defeating
// persistence (fixed v0.2.2).
const DefaultStatePath = "/var/lib/host-agent/state/adaptive.json"

// DefaultObserverPath is where the observer's rolling sample window is
// persisted. Same /var/lib/host-agent/state/ mount as State, so
// observer samples (the actual learnings about the host's thermal
// behavior) carry across container restart, image upgrade, AND mode
// change. Without this, every restart triggers a 2-hour observer
// warmup before the reconciler can make drift decisions.
const DefaultObserverPath = "/var/lib/host-agent/state/observer.json"

// ClassState is the per-class slice of persisted adaptive state.
// Field names mirror the design doc §11. Float values for the running
// EWMAs are stored with full precision; serialized as JSON numbers
// (no quoting).
type ClassState struct {
	TargetCelsius   float64 `json:"target_celsius"`
	DeadbandCelsius float64 `json:"deadband_celsius"`
	// VarianceObservedEWMA and InletBaselineCelsius are reserved by the
	// design doc (§11) but NOT yet written by the reconciler — the live
	// variance/inlet signals come from Observer.Stats() each cycle, not
	// from persisted EWMAs. They round-trip through persistence so the
	// schema is forward-compatible, but currently always hold their zero
	// (or last-loaded) value. Do not assume they carry live data.
	VarianceObservedEWMA float64   `json:"variance_observed_ewma"`
	InletBaselineCelsius float64   `json:"inlet_baseline_celsius"`
	LastUpdate           time.Time `json:"last_update"`
	LastChangeDirection  int       `json:"last_change_direction"`
}

// State is the full on-disk shape of /var/lib/host-agent/state/adaptive.json.
// The Version field is checked at load time; mismatches reset to empty.
type State struct {
	Version    int                           `json:"version"`
	Mode       mode.Mode                     `json:"mode"`
	LastUpdate time.Time                     `json:"last_update"`
	Classes    map[envelope.Class]ClassState `json:"classes"`
}

// NewState returns a fresh State with the current schema version, the
// given mode, no class entries. Use this as the recovery target after
// load failures or schema-version mismatches.
func NewState(m mode.Mode) State {
	return State{
		Version: stateSchemaVersion,
		Mode:    m,
		Classes: map[envelope.Class]ClassState{},
	}
}

// LoadState reads and decodes State from path. Returns (State, true,
// nil) when a valid file was read and parsed; (empty State, false, nil)
// when no file exists; (NewState(mode.Default), false, err) on parse
// error or version mismatch.
//
// The boolean second return is "load succeeded with a valid file" — it
// is FALSE for both "no file" and "file was corrupt" cases. Callers
// that want to distinguish "missing" from "corrupt" should inspect the
// error.
//
// LoadState NEVER returns a partial State on parse error. Either the
// full file deserialized cleanly or you get the recovery default.
func LoadState(path string) (State, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, false, nil // missing is normal first-run
		}
		return NewState(mode.Default), false, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return NewState(mode.Default), false, fmt.Errorf("decode %s: %w", path, err)
	}
	if s.Version != stateSchemaVersion {
		return NewState(mode.Default), false, fmt.Errorf("%s: version %d unsupported (want %d)", path, s.Version, stateSchemaVersion)
	}
	if s.Classes == nil {
		s.Classes = map[envelope.Class]ClassState{}
	}
	return s, true, nil
}

// SaveState atomically writes s to path. Uses temp-file-then-rename so
// a reader (or a concurrent SaveState call from a different process,
// though we don't expect that) never sees a torn file.
//
// Sets State.Version to the current schema version (caller doesn't have
// to remember). Does NOT update LastUpdate — caller is responsible for
// setting that to time.Now() before calling.
//
// Creates parent directory tree if missing. Errors during MkdirAll,
// WriteFile, or Rename are returned without writing partial state.
func SaveState(path string, s State) error {
	s.Version = stateSchemaVersion
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
