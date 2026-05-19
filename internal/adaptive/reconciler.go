// Package adaptive implements the v2 intent-driven adaptive controller
// — observer, state, and reconciler. See docs/adaptive-controller-v2.md.
package adaptive

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// DefaultDriftRatePerCycle is the maximum °C change per reconcile
// cycle (design §10).
const DefaultDriftRatePerCycle = 1

// DefaultMaxDeadbandC caps adaptive deadband growth.
// Above this the PID effectively stops engaging.
const DefaultMaxDeadbandC = 7

// DefaultVarianceResetStdDev: when observed TempStdDev exceeds this
// (in °C), reset the class to mode-initial target. Indicates sensor
// flap or hardware change.
const DefaultVarianceResetStdDev = 5.0

// DefaultMinWindowFillForDrift is the minimum fraction (0-1) of the
// observer window that must be filled before drift decisions are made.
// Below this, the reconciler reports "warming up" and changes nothing.
const DefaultMinWindowFillForDrift = 1.0 // require full window per §10

// varianceReliefPerC and fanReliefPerC model how much temp variance
// and fan change rate fall (per °C of target rise) when the PID
// engages less. These are first-order linear approximations used
// only inside the three-projection score comparison — their job is
// to make in-band projections distinguishable so adaptive can find
// equilibrium. Empirically chosen from observed P4 + HDD windows:
// ~0.3 °C of stddev and ~0.5 changes/min get reclaimed per °C of
// target headroom.
const (
	varianceReliefPerC = 0.30
	fanReliefPerC      = 0.50
)

// DriftReason classifies what the reconciler decided this cycle for a
// given class. Used for metrics, logging, and tests.
type DriftReason string

const (
	DriftReasonSkipped       DriftReason = "skipped"        // per-class override active
	DriftReasonWarmup        DriftReason = "warming_up"     // window not yet full
	DriftReasonSettled       DriftReason = "settled"        // current target scores best
	DriftReasonUp            DriftReason = "drift_up"       // target +1
	DriftReasonDown          DriftReason = "drift_down"     // target -1
	DriftReasonBoundedHigh   DriftReason = "bounded_high"   // wanted to go up but at PreferredHigh
	DriftReasonBoundedLow    DriftReason = "bounded_low"    // wanted to go down but at PreferredLow
	DriftReasonVarianceReset DriftReason = "variance_reset" // TempStdDev > threshold; reset
	DriftReasonError         DriftReason = "error"          // envelope missing, mode invalid, etc.
)

// DriftAction is the record of one class's reconcile decision. The
// reconciler returns one per managed class per Step().
type DriftAction struct {
	Class       envelope.Class
	Reason      DriftReason
	OldTarget   int
	NewTarget   int
	OldDeadband int
	NewDeadband int
	ScoreNow    float64
	ScoreUp     float64
	ScoreDown   float64
	TempMean    float64
	TempStdDev  float64
	SamplesUsed int
	Now         time.Time
}

// ReconcilerMetrics is the exported state snapshot for metrics emission.
type ReconcilerMetrics struct {
	Mode    mode.Mode
	Enabled bool                                // always true for now; reserved for HOST_AGENT_ADAPTIVE_DISABLED toggle
	Classes map[envelope.Class]ClassMetrics     // current per-class state + envelope
	Drifts  map[envelope.Class]map[string]int64 // direction → count
	Resets  map[envelope.Class]map[DriftReason]int64
}

// ClassMetrics is the per-class snapshot included in ReconcilerMetrics.
type ClassMetrics struct {
	TargetCelsius   float64
	DeadbandCelsius float64
	Envelope        envelope.Envelope
	LastUpdate      time.Time
}

// ReconcilerOptions configures a Reconciler.
type ReconcilerOptions struct {
	// Required.
	Observer  *Observer
	Mode      mode.Mode
	StatePath string // where to persist State

	// Optional — all default to constants above when zero-valued.
	DriftRatePerCycle     int
	MaxDeadbandC          int
	VarianceResetStdDev   float64
	MinWindowFillForDrift float64

	// PerClassOverrides: classes in this set are skipped by the
	// reconciler (caller set per-class env var like CPU_TARGET=70).
	// Adaptive does not manage these — caller's fixed value wins.
	PerClassOverrides map[envelope.Class]bool

	// WindowSize is the observer's configured window size (samples),
	// used to compute warmup percentage. Must match what Observer
	// was constructed with.
	WindowSize int

	// Now defaults to time.Now. Override for tests.
	Now func() time.Time

	// Envelopes defaults to envelope.DefaultEnvelopes. Override only
	// in tests that need a controlled envelope.
	Envelopes map[envelope.Class]envelope.Envelope
}

// Reconciler runs the slow intent-layer loop (design §10). It reads
// observer stats, applies a mode-scored ±1°C drift per class with
// rate limits and envelope bounds, and persists the resulting State.
//
// Reconciler is goroutine-safe: Step() takes its mutex around the
// reconcile pass and around State accessors.
type Reconciler struct {
	mu      sync.Mutex
	o       ReconcilerOptions
	state   State
	classes []envelope.Class // stable order for action emission

	// driftsByClassDirection is the per-Step accumulating counter state for drift direction. Updated inside Step() while r.mu is held. Read via Metrics().
	driftsByClassDirection map[envelope.Class]map[string]int64 // direction: "up", "down"

	// resetsByClassReason is the per-Step accumulating counter state for reset reasons. Updated inside Step() while r.mu is held. Read via Metrics().
	resetsByClassReason map[envelope.Class]map[DriftReason]int64
}

// NewReconciler builds a Reconciler from options. Loads State from
// opts.StatePath if a valid one exists; otherwise initializes State
// from the mode's per-class InitialTarget values.
//
// Returns an error only if opts.Observer or opts.Mode is unset. All
// other zero-valued options pick documented defaults.
func NewReconciler(opts ReconcilerOptions) (*Reconciler, error) {
	if opts.Observer == nil {
		return nil, fmt.Errorf("ReconcilerOptions.Observer is nil")
	}
	if opts.Mode == "" {
		opts.Mode = mode.Default
	}
	if opts.DriftRatePerCycle <= 0 {
		opts.DriftRatePerCycle = DefaultDriftRatePerCycle
	}
	if opts.MaxDeadbandC <= 0 {
		opts.MaxDeadbandC = DefaultMaxDeadbandC
	}
	if opts.VarianceResetStdDev <= 0 {
		opts.VarianceResetStdDev = DefaultVarianceResetStdDev
	}
	if opts.MinWindowFillForDrift <= 0 {
		opts.MinWindowFillForDrift = DefaultMinWindowFillForDrift
	}
	if opts.WindowSize <= 0 {
		opts.WindowSize = 480
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Envelopes == nil {
		opts.Envelopes = envelope.DefaultEnvelopes
	}
	if opts.PerClassOverrides == nil {
		opts.PerClassOverrides = map[envelope.Class]bool{}
	}

	r := &Reconciler{o: opts}
	// Stable class order for emission. Match the order used by
	// the metrics package (CPU, PassiveGPU, HDD, SSD).
	r.classes = []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD}

	// Try to load State from disk; else initialize from mode-initial.
	if opts.StatePath != "" {
		if s, loaded, _ := LoadState(opts.StatePath); loaded && s.Mode == opts.Mode {
			r.state = s
		}
	}
	if r.state.Version == 0 {
		r.state = NewState(opts.Mode)
	}
	// Ensure every managed class has a ClassState (use mode-initial as
	// starting point).
	for _, c := range r.classes {
		if _, ok := r.state.Classes[c]; ok {
			continue
		}
		env, ok := opts.Envelopes[c]
		if !ok {
			continue
		}
		t, d := mode.InitialTarget(env, opts.Mode)
		r.state.Classes[c] = ClassState{
			TargetCelsius:   float64(t),
			DeadbandCelsius: float64(d),
			LastUpdate:      opts.Now(),
		}
	}

	r.driftsByClassDirection = map[envelope.Class]map[string]int64{}
	r.resetsByClassReason = map[envelope.Class]map[DriftReason]int64{}
	for _, c := range r.classes {
		r.driftsByClassDirection[c] = map[string]int64{}
		r.resetsByClassReason[c] = map[DriftReason]int64{}
	}
	return r, nil
}

// State returns a shallow copy of the current persisted state. Safe
// to call from any goroutine.
func (r *Reconciler) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.state
	out.Classes = make(map[envelope.Class]ClassState, len(r.state.Classes))
	for k, v := range r.state.Classes {
		out.Classes[k] = v
	}
	return out
}

// Target returns the current target+deadband for a class. Reflects
// the most recent Step() outcome. (target, deadband, ok).
func (r *Reconciler) Target(c envelope.Class) (target int, deadband int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ok := r.state.Classes[c]
	if !ok {
		return 0, 0, false
	}
	return int(math.Round(cs.TargetCelsius)), int(math.Round(cs.DeadbandCelsius)), true
}

// Metrics returns a snapshot of the Reconciler's exported state for
// the metrics writer. Safe to call from any goroutine. Deep copies all maps.
func (r *Reconciler) Metrics() ReconcilerMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := ReconcilerMetrics{
		Mode:    r.o.Mode,
		Enabled: true,
		Classes: make(map[envelope.Class]ClassMetrics, len(r.state.Classes)),
		Drifts:  make(map[envelope.Class]map[string]int64, len(r.driftsByClassDirection)),
		Resets:  make(map[envelope.Class]map[DriftReason]int64, len(r.resetsByClassReason)),
	}
	for c, cs := range r.state.Classes {
		env, ok := r.o.Envelopes[c]
		if !ok {
			continue
		}
		out.Classes[c] = ClassMetrics{
			TargetCelsius:   cs.TargetCelsius,
			DeadbandCelsius: cs.DeadbandCelsius,
			Envelope:        env,
			LastUpdate:      cs.LastUpdate,
		}
	}
	for c, dirs := range r.driftsByClassDirection {
		copyMap := make(map[string]int64, len(dirs))
		for k, v := range dirs {
			copyMap[k] = v
		}
		out.Drifts[c] = copyMap
	}
	for c, reasons := range r.resetsByClassReason {
		copyMap := make(map[DriftReason]int64, len(reasons))
		for k, v := range reasons {
			copyMap[k] = v
		}
		out.Resets[c] = copyMap
	}
	return out
}

// Step runs ONE reconcile pass — once per ADAPTIVE_CYCLE_MINUTES from
// the caller's perspective. For each managed class, evaluates the
// mode score at current target and at ±DriftRatePerCycle, picks the
// best direction subject to bounds and rate limit, updates State, and
// returns one DriftAction per class.
//
// State is persisted to disk after the pass (best-effort; persistence
// errors are returned but the in-memory state is still updated).
//
// Concurrency: Step takes the reconciler mutex for the entire pass.
func (r *Reconciler) Step() ([]DriftAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	actions := make([]DriftAction, 0, len(r.classes))
	now := r.o.Now()

	for _, class := range r.classes {
		action := r.reconcileClass(class, now)
		actions = append(actions, action)
	}

	r.state.LastUpdate = now
	r.state.Mode = r.o.Mode
	r.state.Version = stateSchemaVersion

	var saveErr error
	if r.o.StatePath != "" {
		saveErr = SaveState(r.o.StatePath, r.state)
	}
	return actions, saveErr
}

// reconcileClass evaluates one class. Caller holds r.mu.
func (r *Reconciler) reconcileClass(class envelope.Class, now time.Time) DriftAction {
	cs := r.state.Classes[class]
	action := DriftAction{
		Class:       class,
		OldTarget:   int(math.Round(cs.TargetCelsius)),
		OldDeadband: int(math.Round(cs.DeadbandCelsius)),
		Now:         now,
	}

	env, envOK := r.o.Envelopes[class]
	if !envOK {
		action.Reason = DriftReasonError
		action.NewTarget = action.OldTarget
		action.NewDeadband = action.OldDeadband
		return action
	}

	// Skip if operator pinned this class.
	if r.o.PerClassOverrides[class] {
		action.Reason = DriftReasonSkipped
		action.NewTarget = action.OldTarget
		action.NewDeadband = action.OldDeadband
		return action
	}

	stats := r.o.Observer.Stats(class)
	action.TempMean = stats.TempMean
	action.TempStdDev = stats.TempStdDev
	action.SamplesUsed = stats.Samples

	// Warmup gate.
	fill := float64(stats.Samples) / float64(r.o.WindowSize)
	if fill < r.o.MinWindowFillForDrift {
		action.Reason = DriftReasonWarmup
		action.NewTarget = action.OldTarget
		action.NewDeadband = action.OldDeadband
		return action
	}

	// Variance reset (design §12 — high variance == something's broken).
	if stats.TempStdDev > r.o.VarianceResetStdDev {
		t, d := mode.InitialTarget(env, r.o.Mode)
		cs.TargetCelsius = float64(t)
		cs.DeadbandCelsius = float64(d)
		cs.LastUpdate = now
		cs.LastChangeDirection = 0
		r.state.Classes[class] = cs
		action.Reason = DriftReasonVarianceReset
		action.NewTarget = t
		action.NewDeadband = d

		// Increment counter before returning.
		r.resetsByClassReason[class][DriftReasonVarianceReset]++

		return action
	}

	// Score current + projected ±drift.
	//
	// Synth model: raising the target by Δ°C
	//   - raises observed mean by Δ°C (PID lets equilibrium rise), AND
	//   - reduces PID engagement → lower temp variance + lower fan
	//     change rate.
	//
	// Modeling only the mean (as v0.3.0–v0.3.3 did) cancels the
	// variance and fan-change-rate terms across the three projections
	// — so once observed mean is inside the satisficing band, all
	// three projections tie on bandViolation alone and the reconciler
	// stays "settled" forever, even when the PID is constantly
	// fighting the gap between target and equilibrium. Modeling the
	// PID-engagement relief is what lets adaptive find equilibrium
	// inside the band.
	//
	// Reliefs are first-order linear approximations — the synth only
	// needs to make in-band projections distinguishable, not predict
	// thermal equilibrium exactly. Clamped to 0 (can't reduce below
	// already-quiescent).
	scoreFn := r.o.Mode.Score()
	drift := float64(r.o.DriftRatePerCycle)
	statsUp := stats
	statsUp.TempMean = stats.TempMean + drift
	statsUp.TempStdDev = math.Max(0, stats.TempStdDev-varianceReliefPerC*drift)
	statsUp.FanChangeRate = math.Max(0, stats.FanChangeRate-fanReliefPerC*drift)
	statsDown := stats
	statsDown.TempMean = stats.TempMean - drift
	statsDown.TempStdDev = stats.TempStdDev + varianceReliefPerC*drift
	statsDown.FanChangeRate = stats.FanChangeRate + fanReliefPerC*drift

	action.ScoreNow = scoreFn(env, stats)
	action.ScoreUp = scoreFn(env, statsUp)
	action.ScoreDown = scoreFn(env, statsDown)

	bestDelta := 0
	bestScore := action.ScoreNow
	if action.ScoreUp < bestScore {
		bestDelta = +r.o.DriftRatePerCycle
		bestScore = action.ScoreUp
	}
	if action.ScoreDown < bestScore {
		bestDelta = -r.o.DriftRatePerCycle
	}

	// Bounded clamping per envelope.
	newTarget := action.OldTarget + bestDelta
	if newTarget > env.PreferredHigh {
		newTarget = env.PreferredHigh
		if bestDelta > 0 {
			action.Reason = DriftReasonBoundedHigh
		}
	}
	if newTarget < env.PreferredLow {
		newTarget = env.PreferredLow
		if bestDelta < 0 {
			action.Reason = DriftReasonBoundedLow
		}
	}
	if action.Reason == "" {
		switch {
		case bestDelta > 0:
			action.Reason = DriftReasonUp
		case bestDelta < 0:
			action.Reason = DriftReasonDown
		default:
			action.Reason = DriftReasonSettled
		}
	}
	action.NewTarget = newTarget

	// Deadband: max(mode-default, ceil(stddev*1.5)), capped at MaxDeadbandC.
	_, modeDefaultDeadband := mode.InitialTarget(env, r.o.Mode)
	desiredDeadband := int(math.Ceil(stats.TempStdDev * 1.5))
	if desiredDeadband < modeDefaultDeadband {
		desiredDeadband = modeDefaultDeadband
	}
	if desiredDeadband > r.o.MaxDeadbandC {
		desiredDeadband = r.o.MaxDeadbandC
	}
	action.NewDeadband = desiredDeadband

	// Persist back to state.
	cs.TargetCelsius = float64(newTarget)
	cs.DeadbandCelsius = float64(desiredDeadband)
	cs.LastUpdate = now
	if bestDelta != 0 {
		cs.LastChangeDirection = bestDelta
	}
	r.state.Classes[class] = cs

	switch action.Reason {
	case DriftReasonUp:
		r.driftsByClassDirection[class]["up"]++
	case DriftReasonDown:
		r.driftsByClassDirection[class]["down"]++
	case DriftReasonVarianceReset:
		r.resetsByClassReason[class][DriftReasonVarianceReset]++
	}
	return action
}
