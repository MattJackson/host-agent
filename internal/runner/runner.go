// Package runner abstracts subprocess execution so tests can swap in
// canned outputs without spawning real processes. The fan controller
// drives ipmitool, nvidia-smi, smartctl, and reads /sys files; tests
// need to fake every external command's stdout deterministically.
package runner

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Runner runs a named program with arguments and returns combined stdout.
// Implementations decide on timeouts, environment, and PATH lookup.
//
// Error semantics: a non-nil error means "the program failed to produce
// usable output" — either the binary was missing, exited non-zero, was
// killed by signal, or timed out. Callers that need to distinguish
// "exit 2 = drive in standby" from other failures should use ExitError
// (see ExitCode below).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExitCoder is satisfied by errors that carry a process exit code.
// *exec.ExitError already does; FakeRunner errors do too when seeded.
type ExitCoder interface {
	ExitCode() int
}

// ExitCode pulls the exit code out of any error returned by Runner.Run.
// Returns -1 if the error doesn't carry one (e.g. context timeout,
// binary-not-found, signal kill).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if ec, ok := err.(ExitCoder); ok {
		return ec.ExitCode()
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// Exec is the production Runner. Every Run wraps the caller's context
// in a 30-second timeout — smartctl against a hung RAID controller can
// hang indefinitely, and the controller loop must not deadlock on it.
type Exec struct {
	// DefaultTimeout caps how long any single command can run. 30s is
	// plenty for ipmitool, nvidia-smi, smartctl-i, and smartctl-A even
	// on slow PERC controllers.
	DefaultTimeout time.Duration
}

// NewExec returns a Runner with the 30s default timeout pre-set.
func NewExec() *Exec {
	return &Exec{DefaultTimeout: 30 * time.Second}
}

// Run executes name with args, captures combined stdout+stderr, and
// returns it as a string. Combined output mirrors what bash's
// `cmd 2>/dev/null` lines compute against — the parsers in this
// package look at stdout but smartctl prints status info to stderr
// that some parse paths want to ignore. We keep them combined for
// parity with the bash original; parsers strip lines they don't care
// about.
func (e *Exec) Run(ctx context.Context, name string, args ...string) (string, error) {
	timeout := e.DefaultTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(cctx, name, args...).Output()
	if err != nil {
		// Surface the exit code if any; otherwise leave the error as-is.
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Preserve stderr inside the error message; some parsers
			// glance at it (e.g. smartctl prints "Device is in standby
			// mode" to stderr alongside exit 2).
			return string(out) + string(exitErr.Stderr), err
		}
		return string(out), fmt.Errorf("exec %s: %w", name, err)
	}
	return string(out), nil
}
