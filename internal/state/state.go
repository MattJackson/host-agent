// Package state persists the controller's EWMA baseline + last fan
// speed across container restarts. The file is the bash-style
// key=value format the original script writes, byte-identical so the
// new Go controller and old bash controller can read each other's
// state during gradual rollout.
package state

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// State is what we persist between cycles.
type State struct {
	// BaseSpeed is the EWMA-smoothed fan speed, %. Stored to 4 decimals
	// (matches bash awk '%.4f').
	BaseSpeed float64
	// LastSpeed is the integer fan setpoint at last persist time. The
	// controller resumes from this on restart — recent operating point
	// beats lagging EWMA base.
	LastSpeed int
	// Samples counts decision cycles since first run.
	Samples int
	// LastUpdated is RFC3339 UTC ("Z" suffix).
	LastUpdated time.Time
}

// Write atomically renders state to path with the exact bash format:
//
//	base_speed=37.4221
//	last_speed=42
//	samples=18421
//	last_updated=2026-05-15T17:30:00Z
//
// %.4f on BaseSpeed; ISO 8601 UTC with trailing Z on LastUpdated.
// Each value on its own line. No trailing newline-only line.
//
// Writes via temp-file-then-rename so a concurrent reader never sees
// a torn file. The temp file is in the same directory so rename(2) is
// guaranteed atomic.
func Write(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// If anything below fails, clean up. Successful rename moves the
		// file off this path; the Remove then no-ops.
		_ = os.Remove(tmpName)
	}()

	when := s.LastUpdated
	if when.IsZero() {
		when = time.Now().UTC()
	} else {
		when = when.UTC()
	}
	// RFC3339 (not a literal-"Z" layout): `when` is forced to UTC above, so
	// Go emits the "Z" zone marker — byte-identical to the old layout — but
	// if the UTC normalization is ever dropped the zone offset stays
	// truthful instead of silently mislabelling a local time as "Z".
	body := fmt.Sprintf("base_speed=%.4f\nlast_speed=%d\nsamples=%d\nlast_updated=%s\n",
		s.BaseSpeed, s.LastSpeed, s.Samples, when.Format(time.RFC3339))

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// Read parses the bash-style state file. Returns os.ErrNotExist
// (wrapped) when the file is absent — callers should treat that as
// "fresh start". Malformed lines are silently skipped (matching bash's
// `. "$STATE_FILE"`, which would barf on truly invalid syntax but in
// practice the file is only ever written by us).
//
// All four fields are optional. Missing fields keep their zero values.
func Read(path string) (State, error) {
	var s State
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := line[:eq]
		val := strings.TrimSpace(line[eq+1:])
		// Strip a single set of matched quotes if present.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		switch key {
		case "base_speed":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				s.BaseSpeed = f
			}
		case "last_speed":
			if i, err := strconv.Atoi(val); err == nil {
				s.LastSpeed = i
			}
		case "samples":
			if i, err := strconv.Atoi(val); err == nil {
				s.Samples = i
			}
		case "last_updated":
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				s.LastUpdated = t
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return s, err
	}
	return s, nil
}
