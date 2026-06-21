// fan-controller — Dell PowerEdge adaptive fan controller.
// Per-class PIDs (CPU, passive_gpu, active_gpu, hdd, ssd) emit
// candidate fan speeds; max() wins, plus per-class proximity floors
// and an active-GPU assist lift. EWMA-tracked equilibrium baseline
// persisted to /var/lib/host-agent/state/base. See host-agent/README.md.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pq/docker-server/host-agent/internal/adaptive"
	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/controller"
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/livetargets"
	"github.com/pq/docker-server/host-agent/internal/runner"
	"github.com/pq/docker-server/host-agent/internal/sensors"
)

// version is stamped at build time via -ldflags.
var version = "dev"

const (
	profileDir          = "/etc/fan-controller/profiles"
	stateDir            = "/var/lib/host-agent/state"
	stateFile           = "/var/lib/host-agent/state/base"
	metricsFile         = "/var/lib/host-agent/state/metrics.prom"
	adaptiveMetricsFile = "/var/lib/host-agent/state/adaptive.prom"
)

// stdLogger emits the bash log line format: "YYYY-MM-DD HH:MM:SS - msg".
// We can't use stdlib log.SetFlags(0) + custom prefix because the bash
// format has a " - " separator that's awkward to express that way.
type stdLogger struct {
	out *log.Logger
}

func (l *stdLogger) Printf(format string, v ...any) {
	now := time.Now().Format("2006-01-02 15:04:05")
	l.out.Printf("%s - %s", now, fmt.Sprintf(format, v...))
}

func main() {
	logger := &stdLogger{out: log.New(os.Stdout, "", 0)}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := runner.NewExec()
	ipmiClient := ipmi.New(r)

	// 1. Detect chassis model.
	model := detectModel()
	logger.Printf("Detected model: %s", model)

	// 2. Load profile (env > model > default).
	cfg, err := config.Load(profileDir, model, os.LookupEnv, logger)
	if err != nil {
		logger.Printf("FATAL: profile load: %v", err)
		os.Exit(1)
	}

	m, modeSet, modeErr := config.ApplyMode(cfg)
	if modeErr != nil {
		logger.Printf("WARN: %v (falling back to %s)", modeErr, m)
	} else if modeSet {
		logger.Printf("HOST_AGENT_MODE=%s (explicit)", m)
	} else {
		logger.Printf("HOST_AGENT_MODE unset; using v1 profile values + %s defaults for any unset class", m)
	}
	// Log the effective config once, after mode resolution (the pre-mode
	// snapshot is identical on the v1 path and just duplicates the line).
	logActiveProfile(logger, cfg)

	// Fail closed on dangerous/incoherent config (validated post-ApplyMode,
	// since mode fills per-class targets). Refusing to start hands fans to
	// iDRAC automatic — safe, if louder — rather than running unsafe bounds.
	if err := config.Validate(cfg); err != nil {
		logger.Printf("FATAL: %v", err)
		os.Exit(1)
	}

	// 2b: Build adaptive observer + restore prior window from disk so
	// learnings (sample window, inlet baseline) survive container
	// restart and image upgrade. A 2-hour warmup penalty per restart
	// would otherwise gate every drift decision.
	obs := buildObserver(logger, cfg)
	observerPath := adaptive.DefaultObserverPath
	if op := os.Getenv("HOST_AGENT_OBSERVER_PATH"); op != "" {
		observerPath = op
	}
	if loaded, err := obs.LoadFrom(observerPath); err != nil {
		logger.Printf("adaptive observer: starting cold (load %s: %v)", observerPath, err)
	} else if loaded {
		logger.Printf("adaptive observer: restored sample window from %s", observerPath)
	}

	// 2c: Read env vars for reconciler config.
	var adaptiveCycleMin int = 10
	if v := os.Getenv("ADAPTIVE_CYCLE_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			adaptiveCycleMin = n
		}
	}
	// Adaptive target-drift is OFF by default: the controller is a plain
	// thermostat that holds the mode-derived per-class target, and the PID
	// does the work. Drift (moving the target itself over time to trade
	// temperature for fan noise) was a passive-GPU-specific behavior that,
	// applied uniformly, let cool-running classes (HDD/CPU/SSD) ride well
	// above their configured setpoint to save noise — e.g. a "balanced"
	// HDD target of 38 silently drifting to the MaxSafe-1 ceiling of 44.
	// Backburnered (not deleted) pending a proper, class-scoped GPU solution:
	// set HOST_AGENT_ADAPTIVE_DRIFT=on to re-enable the reconciler.
	driftEnabled := strings.EqualFold(os.Getenv("HOST_AGENT_ADAPTIVE_DRIFT"), "on") ||
		strings.EqualFold(os.Getenv("HOST_AGENT_ADAPTIVE_DRIFT"), "true") ||
		os.Getenv("HOST_AGENT_ADAPTIVE_DRIFT") == "1" ||
		strings.EqualFold(os.Getenv("HOST_AGENT_ADAPTIVE_DRIFT"), "yes")
	adaptiveDisabled := !driftEnabled

	statePath := adaptive.DefaultStatePath
	if sp := os.Getenv("HOST_AGENT_ADAPTIVE_STATE_PATH"); sp != "" {
		statePath = sp
	}

	windowMinutes := 20
	if v := os.Getenv("OBSERVER_WINDOW_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowMinutes = n
		}
	}
	intervalSec := cfg.IntervalSec
	if intervalSec <= 0 {
		intervalSec = 15
	}
	windowSize := windowMinutes * 60 / intervalSec

	// v2: per-class env-var overrides (not profile entries) disable
	// adaptive for that class. Matches ApplyMode's env-var-only rule.
	var perClassOverrides map[envelope.Class]bool
	if !adaptiveDisabled {
		perClassOverrides = make(map[envelope.Class]bool)
		if _, ok := os.LookupEnv("CPU_TARGET"); ok {
			perClassOverrides[envelope.CPU] = true
		}
		if _, ok := os.LookupEnv("GPU_TARGET"); ok {
			perClassOverrides[envelope.PassiveGPU] = true
		}
		if _, ok := os.LookupEnv("HDD_TARGET"); ok {
			perClassOverrides[envelope.HDD] = true
		}
		if _, ok := os.LookupEnv("SSD_TARGET"); ok {
			perClassOverrides[envelope.SSD] = true
		}
	}

	overrideClasses := make([]string, 0, len(perClassOverrides))
	for c := range perClassOverrides {
		overrideClasses = append(overrideClasses, string(c))
	}
	slices.Sort(overrideClasses)

	var recon *adaptive.Reconciler
	if !adaptiveDisabled {
		reconErr := error(nil)
		recon, reconErr = adaptive.NewReconciler(adaptive.ReconcilerOptions{
			Observer:          obs,
			Mode:              m,
			StatePath:         statePath,
			WindowSize:        windowSize,
			PerClassOverrides: perClassOverrides,
		})
		if reconErr != nil {
			logger.Printf("WARN: failed to build reconciler: %v (adaptive disabled)", reconErr)
			adaptiveDisabled = true
		} else {
			logger.Printf("adaptive reconciler: active, cycle=%dmin, state=%s, overrides=%v", adaptiveCycleMin, statePath, overrideClasses)
		}
	}

	// liveTargets is the concurrency-safe handoff between the
	// reconciler (writer goroutine) and the PID main goroutine (reader).
	// When adaptive is disabled it stays empty and ApplyTo is a no-op,
	// so cfg keeps its v1-derived values.
	liveTargets := livetargets.New()

	if adaptiveDisabled {
		logger.Printf("adaptive target-drift: OFF (thermostat mode — holding mode targets; set HOST_AGENT_ADAPTIVE_DRIFT=on to re-enable)")
	} else {
		logger.Printf("adaptive target-drift: ON (HOST_AGENT_ADAPTIVE_DRIFT)")
		// Seed liveTargets from the reconciler's PERSISTED state so the PID
		// uses the learned target/deadband from cycle 1. Without this there's
		// an ADAPTIVE_CYCLE_MINUTES (~10 min) window after every restart where
		// ApplyTo is a no-op and the PID runs on mode-initial values — which
		// can re-introduce a fan-hunt at the narrower initial deadband even
		// though the reconciler already learned a wider, settled one.
		seedLiveTargets(recon, liveTargets, perClassOverrides)
		go runAdaptiveLoop(ctx, logger, recon, liveTargets, time.NewTicker(time.Duration(adaptiveCycleMin)*time.Minute))
	}

	// 3. Vendor guard — refuse to start on non-Dell BMCs.
	vendor, err := ipmiClient.Vendor(ctx)
	if err != nil {
		logger.Printf("FATAL: ipmitool mc info returned no Manufacturer Name. Is /dev/ipmi0 mapped in?")
		os.Exit(1)
	}
	if vendor == "" {
		logger.Printf("FATAL: ipmitool mc info returned no Manufacturer Name. Is /dev/ipmi0 mapped in?")
		os.Exit(1)
	}
	if !strings.Contains(vendor, "Dell") {
		logger.Printf("FATAL: not a Dell BMC (%s). Refusing to issue Dell raw fan commands.", vendor)
		os.Exit(1)
	}
	logger.Printf("Vendor: %s", vendor)

	// 4. Probe GPU + HDD.
	gpu := sensors.NewGPU(r)
	if label, fatal := gpu.Probe(ctx, cfg.GPUAware); fatal {
		logger.Printf("%s", label)
		os.Exit(1)
	} else {
		logger.Printf("%s", label)
	}
	smartctl := sensors.NewSmartctl(r)
	if label, fatal := smartctl.Probe(ctx, cfg.HDDAware); fatal {
		logger.Printf("%s", label)
		os.Exit(1)
	} else {
		logger.Printf("%s", label)
	}

	cpu := sensors.NewCPU(r, osFS{})
	reader := &compositeReader{cpu: cpu, gpu: gpu, smartctl: smartctl}

	// v0.5.0: overlay any persisted learned curve comfort onto cfg so the agent
	// resumes its converged operating point instead of relearning from the
	// profile default each boot. Returns whether this box has a baseline (has
	// been scanned) — if not, we run the first-run box scan below.
	scanned := loadBaseline(learnedStatePath, cfg, logger)

	c := controller.New(cfg, ipmiClient, reader, logger, stateFile, metricsFile)
	c.LoadState()

	// 5. Engage manual control + apply initial speed.
	if err := ipmiClient.EngageManual(ctx); err != nil {
		logger.Printf("WARN: EngageManual: %v", err)
	}
	if err := ipmiClient.SetFan(ctx, c.CurrentSpeed); err != nil {
		logger.Printf("WARN: SetFan: %v", err)
	}
	logger.Printf("Manual control engaged at %d%%", c.CurrentSpeed)

	// v0.5.0 first-run box scan: if this box has no baseline, learn its airflow
	// once (drive fans through fixed levels, fit fan→temp, place each curve to
	// hold TARGET) before entering normal control. The continuous learner then
	// maintains it. Skipped on CPU-only boxes (nothing slow to learn) and when
	// the scan can't run; either way we proceed and the learner trims from the
	// profile default.
	if !scanned {
		scanDwell := 10 * time.Minute
		if v := os.Getenv("SCAN_DWELL_MINUTES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				scanDwell = time.Duration(n) * time.Minute
			}
		}
		if reading, ok := reader.Read(ctx); !ok || !scanWorthwhile(reading) {
			logger.Printf("box scan: skipped (no slow plant present) — using profile comfort")
			scanned = true // nothing to scan; don't retry every boot
		} else if runBoxScan(ctx, logger, cfg, reader, ipmiClient, scanDwell, cfg.IntervalSec) {
			scanned = true
		} else {
			logger.Printf("box scan: did not complete — using profile comfort, will retry next boot")
		}
		if err := saveBaseline(learnedStatePath, cfg, scanned); err != nil {
			logger.Printf("WARN: baseline persist: %v", err)
		}
	}

	// 6. Main loop. Cycle every cfg.IntervalSec; persist + return-to-auto
	// on signal.
	interval := time.Duration(cfg.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// v0.5.0 target-seeking learner cadence. Fires every adaptiveCycleMin; the
	// first fire is one full interval in, giving the observer window time to
	// fill with settled data before the learner acts (belt-and-suspenders with
	// the learner's own settle gate).
	learnInterval := time.Duration(adaptiveCycleMin) * time.Minute
	lastLearn := time.Now()

	// First cycle runs immediately (don't wait for the first tick).
	// Pick up any pending live-target updates before the cycle reads cfg.
	liveTargets.ApplyTo(cfg)
	runCycle(ctx, c)
	sampleObserver(obs, c)
	if err := adaptive.WriteAdaptiveMetrics(adaptiveMetricsFile, obs, recon); err != nil {
		logger.Printf("WARN: adaptive metrics write: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Printf("Shutting down — returning fan control to iDRAC automatic")
			if err := c.PersistState(); err != nil {
				logger.Printf("WARN: shutdown persist state: %v", err)
			}
			// Use a fresh context for the handback — the parent context
			// is already cancelled, but ipmitool still needs ~100ms.
			handbackCtx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := ipmiClient.HandbackAuto(handbackCtx); err != nil {
				logger.Printf("WARN: shutdown handback to iDRAC auto: %v", err)
			}
			hcancel()
			return
		case <-ticker.C:
			// Pick up any pending live-target updates from the adaptive
			// goroutine before the cycle reads cfg.
			liveTargets.ApplyTo(cfg)
			runCycle(ctx, c)
			sampleObserver(obs, c)
			if err := adaptive.WriteAdaptiveMetrics(adaptiveMetricsFile, obs, recon); err != nil {
				logger.Printf("WARN: adaptive metrics write: %v", err)
			}
			// v0.5.0: slow target-seeking learner. Same goroutine as the cycle,
			// so updating cfg.*Comfort here is race-free w.r.t. runCycle.
			if time.Since(lastLearn) >= learnInterval {
				lastLearn = time.Now()
				if runLearnTick(cfg, obs, logger) {
					if err := saveBaseline(learnedStatePath, cfg, scanned); err != nil {
						logger.Printf("WARN: learn persist: %v", err)
					}
				}
			}
			// Persist observer window so it survives container restart.
			// Best-effort; persistence errors are non-fatal.
			if err := obs.SaveTo(observerPath); err != nil {
				logger.Printf("WARN: observer persist: %v", err)
			}
		}
	}
}

func runCycle(ctx context.Context, c *controller.Controller) {
	// Each cycle gets its own short context for subprocess deadlines.
	// 30s default per runner.Exec call; the cycle wrapper here caps
	// the whole cycle at 60s in case multiple subprocess calls stack.
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Cycle never returns an error — it handles its own fail-safe (on a
	// CPU-read failure it commands 100% fans) and returns the metrics
	// Snapshot, which main writes via the controller's own metricsFile.
	_ = c.Cycle(cctx)
}

// detectModel maps /sys/class/dmi/id/product_name to a profile slug.
// Matches s6/vmagent/run#detect_model byte-for-byte so the version
// label and the profile loaded by the controller stay in lockstep:
//
//	raw=$(cat /sys/class/dmi/id/product_name)
//	raw="${raw#PowerEdge }"
//	echo "$raw" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9' '_' | sed 's/_*$//'
func detectModel() string {
	raw, err := os.ReadFile("/sys/class/dmi/id/product_name")
	if err != nil {
		return "unknown"
	}
	s := strings.TrimRight(string(raw), "\n")
	if s == "" {
		return "unknown"
	}
	s = strings.TrimPrefix(s, "PowerEdge ")
	s = strings.ToLower(s)
	// Replace non-[a-z0-9] with _.
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	// Trim trailing _.
	return trailingUnderscoreRE.ReplaceAllString(out, "")
}

var trailingUnderscoreRE = regexp.MustCompile(`_+$`)

// logActiveProfile emits the long line bash's load_profile() does
// summarizing the resolved config. Matches the bash format so logwatch
// patterns keep working.
func logActiveProfile(l controller.Logger, cfg *config.Config) {
	l.Printf("Active: CPU target=%d±%d emerg=%d°C win=%d | GPU(passive) target=%d±%d emerg=%d°C win=%d | GPU(active) own_fan_thresh=%d%% emerg=%d°C | HDD target=%d±%d emerg=%d°C win=%d read=%ds | FAN=%d-%d%% P=%v D=%v DRIFT=%d%%/cyc INTERVAL=%ds ALPHA=%v",
		cfg.CPUTarget, cfg.CPUDeadband, cfg.CPUEmergency, cfg.CPUApproachWindow,
		cfg.GPUTarget, cfg.GPUDeadband, cfg.GPUEmergency, cfg.GPUApproachWindow,
		cfg.ActiveGPUOwnFanThreshold, cfg.ActiveGPUEmergency,
		cfg.HDDTarget, cfg.HDDDeadband, cfg.HDDEmergency, cfg.HDDApproachWindow, cfg.HDDReadInterval,
		cfg.MinFan, cfg.MaxFan,
		cfg.FanGain, cfg.DerivativeGain,
		cfg.DeadbandDriftRate, cfg.IntervalSec, cfg.AdaptAlpha)
}

// v2: build observer for adaptive controller. Phase 2 of the v2
// rollout — this is READ-ONLY in this phase (no decisions are made
// from the observed stats; that's T12+). Window size derives from
// OBSERVER_WINDOW_MINUTES (default 20) × 60 / cfg.IntervalSec. Short by
// design: the v0.5.0 learner reads this window's p50 as its steady-state
// estimate, so a long window would lag the plant and make the learner overshoot
// (see the inline note at the other windowMinutes default).
//
// Inlet temp is not yet plumbed through internal/sensors.Reading;
// we pass 0 for InletCelsius until a later task adds it. The
// inlet-jump reset feature is therefore a no-op in Phase 2.
func buildObserver(logger controller.Logger, cfg *config.Config) *adaptive.Observer {
	windowMinutes := 20
	if v := os.Getenv("OBSERVER_WINDOW_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowMinutes = n
		}
	}
	intervalSec := cfg.IntervalSec
	if intervalSec <= 0 {
		intervalSec = 15
	}
	windowSize := windowMinutes * 60 / intervalSec
	obs := adaptive.NewObserver(windowSize, 10) // inletJumpC=10
	logger.Printf("adaptive observer: window=%dmin (%d samples @ %ds intervals)", windowMinutes, windowSize, intervalSec)
	return obs
}

// sampleObserver pushes one Sample per managed class to the observer,
// reading the most-recent per-class temps from the controller's
// Last*Temp fields. Called after each PID cycle.
//
// Classes with no current reading (Last*Temp == -1) are skipped so the
// observer's discard logic doesn't have to handle the sentinel.
func sampleObserver(obs *adaptive.Observer, c *controller.Controller) {
	now := time.Now()
	push := func(class envelope.Class, temp int) {
		if temp < 0 {
			return
		}
		obs.Add(class, adaptive.Sample{
			Timestamp:    now,
			TempCelsius:  float64(temp),
			FanDemandPct: c.CurrentSpeed,
			InletCelsius: 0, // TODO: plumb real inlet from sensors.Reading
		})
	}
	push(envelope.CPU, c.LastCPUTemp)
	push(envelope.PassiveGPU, c.LastPGTemp)
	push(envelope.HDD, c.LastHDDTemp)
	push(envelope.SSD, c.LastSSDTemp)
}

// compositeReader aggregates CPU + GPU + smartctl into a single Reading.
// On CPU read failure (no coretemp + no IPMI), the cycle aborts → 100%
// for safety.
type compositeReader struct {
	cpu      *sensors.CPU
	gpu      *sensors.GPU
	smartctl *sensors.Smartctl
}

func (c *compositeReader) Read(ctx context.Context) (sensors.Reading, bool) {
	cpuMax, cpuDeets, ok := c.cpu.Read(ctx)
	if !ok {
		return sensors.Reading{}, false
	}
	r := sensors.Reading{CPUMax: cpuMax, Details: cpuDeets}
	if pg, ag, agFan, deets, ok := c.gpu.Read(ctx); ok {
		r.PassiveGPUMax = pg
		r.ActiveGPUMax = ag
		r.ActiveGPUFanMax = agFan
		r.Details += deets
	}
	if hdd, ssd, deets, ok := c.smartctl.Read(ctx); ok {
		r.HDDMax = hdd
		r.SSDMax = ssd
		r.Details += deets
	}
	return r, true
}

// osFS satisfies sensors.FS against the real filesystem. Paths are
// relative to "/", matching os.DirFS("/") semantics — sensors.CPU
// strips the leading "/" before calling.
type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Clean("/" + name))
}

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Clean("/" + name))
}

// seedLiveTargets pushes the reconciler's persisted per-class target +
// deadband into the live store at startup, so the PID picks up the learned
// values on its first cycle instead of running ADAPTIVE_CYCLE_MINUTES on
// mode-initial values after a restart. Operator-overridden classes are
// skipped (mirrors runAdaptiveLoop's Skipped path) so a pin isn't clobbered.
func seedLiveTargets(r *adaptive.Reconciler, lt *livetargets.Store, overrides map[envelope.Class]bool) {
	if r == nil {
		return
	}
	for class := range r.State().Classes {
		if overrides[class] {
			continue
		}
		if t, d, ok := r.Target(class); ok {
			lt.Set(class, t, d)
		}
	}
}

// runAdaptiveLoop drives the slow intent-layer reconcile pass. Fires
// every ADAPTIVE_CYCLE_MINUTES. Each action with NewTarget != OldTarget
// (or NewDeadband != OldDeadband) is staged into liveTargets, which
// the PID main goroutine picks up immediately before its next cycle.
//
// Settled/warming-up/skipped actions are not logged or applied —
// they're per-cycle no-ops and would just spam the journal.
//
// Exits when ctx is cancelled.
func runAdaptiveLoop(ctx context.Context, logger controller.Logger, r *adaptive.Reconciler, lt *livetargets.Store, t *time.Ticker) {
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			_ = now
			actions, err := r.Step()
			if err != nil {
				logger.Printf("WARN: adaptive: state persist failed: %v", err)
			}
			for _, a := range actions {
				if a.Reason == adaptive.DriftReasonSettled || a.Reason == adaptive.DriftReasonWarmup || a.Reason == adaptive.DriftReasonSkipped {
					continue
				}
				// Stage the new target+deadband for the PID to pick up.
				lt.Set(a.Class, a.NewTarget, a.NewDeadband)
				logger.Printf("adaptive: class=%s reason=%s target %d->%d deadband %d->%d (mean=%.1f stddev=%.2f n=%d)",
					a.Class, a.Reason, a.OldTarget, a.NewTarget, a.OldDeadband, a.NewDeadband,
					a.TempMean, a.TempStdDev, a.SamplesUsed)
			}
		}
	}
}
