package runner

import (
	"context"
	"fmt"
	"strings"
)

// FakeRunner is a test double that returns canned outputs for specific
// (name, args) tuples. Keys are matched in two passes: first an exact
// match against "name\x00arg1\x00arg2...", then a prefix match against
// "name\x00arg1...". This lets tests register a single broad response
// for all smartctl invocations or pin-point a single ipmitool call.
type FakeRunner struct {
	Responses map[string]FakeResponse
	// Calls records every call seen, in order, for test assertions.
	Calls []FakeCall
}

// FakeResponse is the canned output for one call. Output is the
// combined stdout/stderr returned to the caller; Err (if non-nil) is
// returned as-is; ExitCode is wrapped into the error so ExitCode(err)
// returns it.
type FakeResponse struct {
	Output   string
	Err      error
	ExitCode int
}

// FakeCall captures one invocation for inspection.
type FakeCall struct {
	Name string
	Args []string
}

// NewFakeRunner returns an empty fake. Use Set to register canned
// responses.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Responses: make(map[string]FakeResponse)}
}

// Set registers output and (optionally) a non-zero exit code for a
// specific command. Pass the empty-args slice to match the no-arg form.
func (f *FakeRunner) Set(name string, args []string, resp FakeResponse) {
	f.Responses[key(name, args)] = resp
}

// SetPrefix registers a response matching any call whose name+args
// start with the given name+prefixArgs. Exact matches still win.
func (f *FakeRunner) SetPrefix(name string, prefixArgs []string, resp FakeResponse) {
	f.Responses["prefix:"+key(name, prefixArgs)] = resp
}

func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.Calls = append(f.Calls, FakeCall{Name: name, Args: append([]string(nil), args...)})

	// Exact match first.
	if r, ok := f.Responses[key(name, args)]; ok {
		return f.toReturn(r)
	}
	// Prefix match — try progressively shorter argument lists.
	for n := len(args); n >= 0; n-- {
		if r, ok := f.Responses["prefix:"+key(name, args[:n])]; ok {
			return f.toReturn(r)
		}
	}
	// Unknown call — return empty output without error, mirroring the
	// bash original which usually swallows stderr via 2>/dev/null and
	// reacts to empty stdout. Tests that want to assert "I covered all
	// my mocks" can scan f.Calls.
	return "", nil
}

func (f *FakeRunner) toReturn(r FakeResponse) (string, error) {
	if r.Err != nil {
		return r.Output, r.Err
	}
	if r.ExitCode != 0 {
		return r.Output, fakeExitError{code: r.ExitCode}
	}
	return r.Output, nil
}

func key(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + "\x00" + strings.Join(args, "\x00")
}

type fakeExitError struct {
	code int
}

func (e fakeExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

func (e fakeExitError) ExitCode() int {
	return e.code
}
