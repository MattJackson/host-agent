// Package controller wires sensors, control math, IPMI, metrics, and
// state persistence into the per-cycle decision loop. Cycles are
// pure-ish: given a Reading and the previous internal state, the
// controller produces a setpoint, a log line, a Snapshot, and a State.
//
// The point of separating Cycle() from Run() is testability: an
// end-to-end test feeds canned readings and asserts on outputs without
// real subprocesses or real sleep.
package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/control"
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/metrics"
	"github.com/pq/docker-server/host-agent/internal/sensors"
	"github.com/pq/docker-server/host-agent/internal/state"
)

// TempReader is the interface the controller needs from temperature
// sources. The composite implementation aggregates CPU + GPU + smartctl;
// tests can inject a stub.
type TempReader interface {
	// Read returns this cycle's temperatures. ok=false → fail-safe
	// (controller commands 100% fans).
	Read(ctx context.Context) (sensors.Reading, bool)
}

// Logger duck-types stdlib log.Logger.
type Logger interface {
	Printf(format string, v ...any)
}

// CurveSmoothTauSec is the time constant of the EWMA applied to each class's
// temperature before it reaches the fan curve. The CPU input is the single
// hottest core, which jitters ±2°C cycle to cycle; fed raw into a proportional
// curve that is pure fan noise. 60s is ~4 cycles at the default 15s interval —
// long enough to average that jitter away, short against the minutes-scale
// thermal response of the heatsinks, and the raw-max emergency check (which is
// NOT smoothed) remains the fast safety path.
const CurveSmoothTauSec = 60.0

// SlewDownPctPerSec limits how fast the chassis setpoint may FALL: 0.2%/s is 3%
// per 15s cycle, ~7.5 min from 100% to a 10% floor. Increases are never
// limited. Together with the smoothing this turns residual noise into a setpoint
// that steps up promptly and eases down, instead of pulsing the fans.
const SlewDownPctPerSec = 0.2

// Controller holds all the persistent state and per-cycle dependencies.
type Controller struct {
	Cfg    *config.Config
	IPMI   *ipmi.Client
	Reader TempReader
	Log    Logger

	// State persistence.
	StatePath   string
	MetricsPath string

	// Internal state.
	CurrentSpeed int
	BaseSpeed    float64
	Samples      int
	InEmergency  bool

	// FanControl gates every write to the BMC (EngageManual + SetFan).
	// When false the controller is MONITOR-ONLY: it still reads sensors,
	// runs the curve math, and emits metrics/log lines, but never touches
	// fan control — iDRAC's own thermal policy owns the fans.
	//
	// This exists because a controller that has taken the BMC into manual
	// mode but cannot command a fan speed is the single worst state: it
	// disables iDRAC's automatic thermal protection AND fails to replace
	// it, so the BMC falls back to its manual-mode failsafe (full-speed on
	// 11G iDRAC6 / R410). main.go sets this false when the startup SetFan
	// capability probe fails (the BMC rejects `0x30 0x30 0x02`, e.g. every
	// pre-12G Dell) or when HOST_AGENT_FAN_CONTROL=off is set. New()
	// defaults it true so capable boxes (R730xd and friends) are unchanged.
	FanControl bool

	// Diagnostic: wall-clock seconds the last Cycle() call took.
	// Exposed via metrics.Snapshot.CycleDurationSeconds so we can SEE
	// in Grafana how long each cycle takes instead of guessing.
	lastCycleDuration float64

	// D-term per class.
	LastCPUTemp int
	LastPGTemp  int
	LastHDDTemp int
	LastSSDTemp int

	// Per-class CURVE demand (% fan THIS class's curve asked for this cycle),
	// distinct from CurrentSpeed (the max-wins chassis fan). The learner's
	// reclaim guard must key off the class's OWN demand, not the chassis: when
	// one class (e.g. a hot GPU) pins the chassis high, the other classes' curves
	// are still at MIN_FAN and have no fan to reclaim — feeding them the chassis
	// value made their comfort ratchet up anyway (the residual docker-1 drift).
	LastCPUDemand int
	LastPGDemand  int
	LastHDDDemand int
	LastSSDDemand int

	// Per-class EWMA-smoothed temperature fed to the curve (0 = unseeded; the
	// first reading seeds it, and a class that drops out is reset). Updated on
	// every successful read, emergency cycles included, so the curve resumes
	// from a current value when an emergency clears. Only the curve reads
	// these — emergency detection and the observer/learner use raw temps.
	SmoothCPUTemp float64
	SmoothPGTemp  float64
	SmoothHDDTemp float64
	SmoothSSDTemp float64

	// lastReadAt is when the previous successful read happened; the gap to the
	// current one is the dt for the smoothing and the down-slew limit (cycles
	// run 3–27s apart, so neither can be per-cycle).
	lastReadAt time.Time

	// Persist cadence.
	PersistInterval time.Duration
	LastPersist     time.Time

	// Now is injected for deterministic tests.
	Now func() time.Time
}

// New constructs a Controller with the bash defaults for cadence and
// the -1 sentinel D-term inputs. The caller must populate the state
// fields (CurrentSpeed, BaseSpeed, Samples) before calling Cycle —
// usually by calling LoadState first.
func New(cfg *config.Config, ipmiClient *ipmi.Client, reader TempReader, log Logger, statePath, metricsPath string) *Controller {
	return &Controller{
		Cfg:             cfg,
		IPMI:            ipmiClient,
		Reader:          reader,
		Log:             log,
		StatePath:       statePath,
		MetricsPath:     metricsPath,
		LastCPUTemp:     -1,
		LastPGTemp:      -1,
		LastHDDTemp:     -1,
		LastSSDTemp:     -1,
		PersistInterval: 60 * time.Second,
		Now:             time.Now,
		FanControl:      true,
	}
}

// engageManual and setFan funnel every BMC write through the FanControl
// gate. In monitor-only mode (FanControl=false) they are no-ops, so the
// controller never disturbs iDRAC's automatic thermal policy.
func (c *Controller) engageManual(ctx context.Context) {
	if !c.FanControl {
		return
	}
	_ = c.IPMI.EngageManual(ctx)
}

func (c *Controller) setFan(ctx context.Context, pct int) {
	if !c.FanControl {
		return
	}
	_ = c.IPMI.SetFan(ctx, pct)
}

// LoadState reads StatePath and seeds CurrentSpeed/BaseSpeed/Samples.
// Missing state file → starts at MinFan. Identical preference order
// to bash: last_speed > base_speed > MinFan.
func (c *Controller) LoadState() {
	s, err := state.Read(c.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.Log.Printf("No persisted state — starting at MIN_FAN=%d%%", c.Cfg.MinFan)
		} else {
			c.Log.Printf("Failed to read state (%v) — starting at MIN_FAN=%d%%", err, c.Cfg.MinFan)
		}
		c.CurrentSpeed = c.Cfg.MinFan
		c.BaseSpeed = float64(c.Cfg.MinFan)
		c.Samples = 0
		return
	}
	c.BaseSpeed = s.BaseSpeed
	c.Samples = s.Samples
	lastSpeedStr := "?"
	lastUpdatedStr := "unknown"
	if s.LastSpeed > 0 {
		lastSpeedStr = fmt.Sprintf("%d", s.LastSpeed)
	}
	if !s.LastUpdated.IsZero() {
		lastUpdatedStr = s.LastUpdated.Format("2006-01-02T15:04:05Z")
	}
	c.Log.Printf("Restored state: base=%g%%, last_speed=%s%%, samples=%d, last_updated=%s",
		s.BaseSpeed, lastSpeedStr, s.Samples, lastUpdatedStr)
	if s.LastSpeed > 0 {
		c.CurrentSpeed = clampInt(s.LastSpeed, c.Cfg.MinFan, c.Cfg.MaxFan)
		c.Log.Printf("Starting at %d%% (resumed from last_speed)", c.CurrentSpeed)
	} else {
		// Legacy fallback.
		intBase := int(s.BaseSpeed + 0.5)
		c.CurrentSpeed = clampInt(intBase, c.Cfg.MinFan, c.Cfg.MaxFan)
		c.Log.Printf("Starting at %d%% (legacy fallback to base)", c.CurrentSpeed)
	}
}

// PersistState writes the current state to disk. Tolerated to fail;
// the caller logs and continues.
func (c *Controller) PersistState() error {
	s := state.State{
		BaseSpeed:   c.BaseSpeed,
		LastSpeed:   c.CurrentSpeed,
		Samples:     c.Samples,
		LastUpdated: c.Now().UTC(),
	}
	if err := state.Write(c.StatePath, s); err != nil {
		return err
	}
	c.LastPersist = c.Now()
	return nil
}

// Cycle runs one decision pass. Returns the per-cycle metrics snapshot
// for diagnostics + tests. Side effects:
//   - calls IPMI.SetFan if the setpoint changed
//   - writes metrics file (best-effort)
//   - emits one log line
//   - persists state every PersistInterval seconds
//   - updates D-term state for next cycle
//
// On Reader.Read returning ok=false, fans are commanded to 100% and
// the cycle short-circuits — same fail-safe as the bash original's
// "Temp read failed — fans 100% for safety" branch.
func (c *Controller) Cycle(ctx context.Context) metrics.Snapshot {
	cycleStart := time.Now()
	defer func() {
		c.lastCycleDuration = time.Since(cycleStart).Seconds()
	}()
	// Re-assert manual fan control every cycle. iDRAC's third-party PCIe
	// cooling response will silently flip the BMC back to auto (→ 100%
	// fans) within ~30s when a non-Dell GPU/HBA is present; subsequent
	// SetFan calls then become no-ops we cannot detect. Idempotent and
	// cheap — one IPMI command per cycle keeps manual sticky.
	c.engageManual(ctx)
	reading, ok := c.Reader.Read(ctx)
	if !ok {
		c.Log.Printf("Temp read failed — fans 100%% for safety")
		c.setFan(ctx, 100)
		c.CurrentSpeed = 100
		// Build a degenerate snapshot for metrics — emergency=1 conveys
		// the safety state even though no class fired.
		snap := c.snapshotEmergency(reading, "emergency")
		_ = metrics.WriteAtomic(c.MetricsPath, snap)
		return snap
	}

	cfg := c.Cfg

	// Elapsed time since the previous successful read drives both the EWMA and
	// the down-slew. First cycle (or a clock that didn't advance / went back):
	// assume one nominal interval.
	now := c.Now()
	dt := now.Sub(c.lastReadAt).Seconds()
	if c.lastReadAt.IsZero() || dt <= 0 {
		dt = float64(cfg.IntervalSec)
		if dt <= 0 {
			dt = 15
		}
	}
	c.lastReadAt = now
	smoothTemp(&c.SmoothCPUTemp, reading.CPUMax, dt)
	smoothTemp(&c.SmoothPGTemp, reading.PassiveGPUMax, dt)
	smoothTemp(&c.SmoothHDDTemp, reading.HDDMax, dt)
	smoothTemp(&c.SmoothSSDTemp, reading.SSDMax, dt)

	// Emergency check: any class >= its emergency threshold. Deliberately on
	// the RAW per-class max, never the smoothed value — the smoothing would
	// delay a genuine trip by up to a time constant.
	if reading.CPUMax >= cfg.CPUEmergency ||
		reading.PassiveGPUMax >= cfg.GPUEmergency ||
		reading.ActiveGPUMax >= cfg.ActiveGPUEmergency ||
		(reading.HDDMax > 0 && reading.HDDMax >= cfg.HDDEmergency) ||
		(reading.SSDMax > 0 && reading.SSDMax >= cfg.SSDEmergency) {
		if c.CurrentSpeed != 100 {
			c.setFan(ctx, 100)
			c.CurrentSpeed = 100
			c.Log.Printf("EMERGENCY (cpu:%d/%d p_gpu:%d/%d a_gpu:%d/%d hdd:%d/%d ssd:%d/%d) — fans 100%%",
				reading.CPUMax, cfg.CPUEmergency,
				reading.PassiveGPUMax, cfg.GPUEmergency,
				reading.ActiveGPUMax, cfg.ActiveGPUEmergency,
				reading.HDDMax, cfg.HDDEmergency,
				reading.SSDMax, cfg.SSDEmergency)
		} else {
			c.Log.Printf("EMERGENCY hold 100%% — %scpu:%d p_gpu:%d a_gpu:%d hdd:%d ssd:%d",
				reading.Details, reading.CPUMax, reading.PassiveGPUMax,
				reading.ActiveGPUMax, reading.HDDMax, reading.SSDMax)
		}
		c.InEmergency = true
		snap := c.snapshotEmergency(reading, "emergency")
		_ = metrics.WriteAtomic(c.MetricsPath, snap)
		return snap
	}

	// Exiting emergency: clear the flag and let the curve below recompute the
	// fan demand. The setpoint then eases down from 100% under the down-slew
	// limit rather than dropping to the curve in one cycle.
	if c.InEmergency {
		c.Log.Printf("Emergency cleared — resuming curve control")
		c.InEmergency = false
	}

	// v3 unified control: ONE memoryless proportional temperature→fan curve per
	// class — fan = MinFan at the class's comfort temp, rising linearly to
	// MaxFan at its emergency temp. Identical law for every class; only the
	// per-class comfort/emergency envelope differs. No PID, no setpoint, no
	// integrator, so it cannot wind up or hunt regardless of plant speed.
	// max() across classes drives the chassis, plus the active-GPU own-fan
	// assist. See docs/fan-controller-v3-design.md.
	//
	// Input is the per-class SMOOTHED temperature (see CurveSmoothTauSec), and
	// comfort is capped at emergency - MinCurveSpan so the ramp can never be
	// narrower than the class's minimum span (startup already clamps cfg; this
	// is the point-of-use backstop).
	caps := comfortCaps(cfg)
	cpuCurve := control.CurveF(c.SmoothCPUTemp, min(cfg.CPUComfort, caps[0]), cfg.CPUEmergency, cfg.MinFan, cfg.MaxFan)
	pgCurve := control.CurveF(c.SmoothPGTemp, min(cfg.GPUComfort, caps[1]), cfg.GPUEmergency, cfg.MinFan, cfg.MaxFan)
	hddCurve := control.CurveF(c.SmoothHDDTemp, min(cfg.HDDComfort, caps[2]), cfg.HDDEmergency, cfg.MinFan, cfg.MaxFan)
	ssdCurve := control.CurveF(c.SmoothSSDTemp, min(cfg.SSDComfort, caps[3]), cfg.SSDEmergency, cfg.MinFan, cfg.MaxFan)

	// Active-GPU assist: chassis-floor lift driven by the card's OWN fan speed,
	// not its die temperature — the card's own fan is the authoritative signal
	// of whether it needs outside help. Die-temp safety is the emergency check.
	agAssist := 0
	if reading.ActiveGPUMax > 0 {
		agAssist = control.ActiveGPUAssist(
			reading.ActiveGPUFanMax, cfg.ActiveGPUOwnFanThreshold,
			cfg.MinFan, cfg.MaxFan,
		)
	}

	// max-wins aggregation across the per-class curves + assist.
	r := control.MaxWins(
		control.MaxCandidate{Name: "cpu", Value: cpuCurve},
		[]control.MaxCandidate{
			{Name: "pg", Value: pgCurve},
			{Name: "hdd", Value: hddCurve},
			{Name: "ssd", Value: ssdCurve},
			{Name: "ag_assist", Value: agAssist},
		},
		cfg.MinFan, cfg.MaxFan,
	)

	// Asymmetric slew on the final setpoint: rises apply now, falls are
	// rate-limited. When the limit is what's holding the fan up, report the
	// binding source as "slew" so the log/metric don't blame a class curve.
	demand := r.NewSpeed
	c.CurrentSpeed = clampInt(control.SlewDown(c.CurrentSpeed, demand, dt, SlewDownPctPerSec), cfg.MinFan, cfg.MaxFan)
	if c.CurrentSpeed > demand {
		r.Source = "slew"
	}
	// Re-issue SetFan every cycle, not only on change. The BMC's
	// revert-to-auto watchdog tracks the fan-PWM command specifically;
	// a steady-state cycle that skips SetFan lets manual control lapse
	// and fans run away to 100%. Idempotent — same value to same BMC.
	c.setFan(ctx, c.CurrentSpeed)

	// Log line.
	c.Log.Printf("%scpu:%d p_gpu:%d a_gpu:%d hdd:%d ssd:%d | curve c%d/p%d/h%d/s%d ag_assist:%d → %d%%(%s) | smooth c%.1f/p%.1f/h%.1f/s%.1f demand %d%%",
		reading.Details,
		reading.CPUMax, reading.PassiveGPUMax, reading.ActiveGPUMax, reading.HDDMax, reading.SSDMax,
		cpuCurve, pgCurve, hddCurve, ssdCurve,
		agAssist, c.CurrentSpeed, r.Source,
		c.SmoothCPUTemp, c.SmoothPGTemp, c.SmoothHDDTemp, c.SmoothSSDTemp, demand)

	// EWMA + samples.
	c.BaseSpeed = control.Ewma(c.BaseSpeed, float64(c.CurrentSpeed), cfg.AdaptAlpha)
	c.Samples++

	// D-term state update — only update classes that returned a real reading.
	// CPU is guarded for symmetry with the optional classes: a CPUMax of 0
	// reaching here (a refactor that returns ok=true with a zero max) would
	// otherwise zero the stored temp and spike the D-term next cycle. Today
	// cpu.Read() returns ok=false on a zero max, so this is defensive.
	if reading.CPUMax > 0 {
		c.LastCPUTemp = reading.CPUMax
		c.LastCPUDemand = cpuCurve
	}
	if reading.PassiveGPUMax > 0 {
		c.LastPGTemp = reading.PassiveGPUMax
		c.LastPGDemand = pgCurve
	}
	if reading.HDDMax > 0 {
		c.LastHDDTemp = reading.HDDMax
		c.LastHDDDemand = hddCurve
	}
	if reading.SSDMax > 0 {
		c.LastSSDTemp = reading.SSDMax
		c.LastSSDDemand = ssdCurve
	}

	// Persist + metrics.
	if now.Sub(c.LastPersist) >= c.PersistInterval {
		if err := c.PersistState(); err != nil {
			c.Log.Printf("persist failed: %v", err)
		}
	}

	snap := metrics.Snapshot{
		CurrentSpeed:         c.CurrentSpeed,
		BaseSpeed:            c.BaseSpeed,
		Samples:              c.Samples,
		CycleDurationSeconds: c.lastCycleDuration,
		InEmergency:          0,
		CPUMax:               reading.CPUMax,
		PassiveGPUMax:        reading.PassiveGPUMax,
		ActiveGPUMax:         reading.ActiveGPUMax,
		ActiveGPUFanMax:      reading.ActiveGPUFanMax,
		HDDMax:               reading.HDDMax,
		SSDMax:               reading.SSDMax,
		// v3: the "target" metric now carries the curve's comfort (ramp-start)
		// temperature — the per-class control parameter.
		CPUTarget:                cfg.CPUComfort,
		PassiveGPUTarget:         cfg.GPUComfort,
		HDDTarget:                cfg.HDDComfort,
		SSDTarget:                cfg.SSDComfort,
		CPUEmergency:             cfg.CPUEmergency,
		PassiveGPUEmergency:      cfg.GPUEmergency,
		ActiveGPUEmergency:       cfg.ActiveGPUEmergency,
		ActiveGPUOwnFanThreshold: cfg.ActiveGPUOwnFanThreshold,
		HDDEmergency:             cfg.HDDEmergency,
		SSDEmergency:             cfg.SSDEmergency,
		// The per-class "candidate" metric now carries the curve output. The
		// legacy proximity-floor (PF) fields are retired in v3 (the curve IS the
		// floor); kept at 0 for metric-schema stability.
		CPUCand:   cpuCurve,
		PGCand:    pgCurve,
		HDDCand:   hddCurve,
		SSDCand:   ssdCurve,
		CPUPF:     0,
		PGPF:      0,
		AGPF:      0,
		HDDPF:     0,
		SSDPF:     0,
		AGAssist:  agAssist,
		Source:    r.Source,
		FanDemand: demand,
	}
	c.fillSmoothing(&snap)
	_ = metrics.WriteAtomic(c.MetricsPath, snap)
	return snap
}

// smoothTemp advances one class's curve-input EWMA. raw <= 0 (class absent this
// cycle) resets it, so a class that returns later re-seeds from its first real
// reading instead of blending with a stale value.
func smoothTemp(s *float64, raw int, dtSec float64) {
	switch {
	case raw <= 0:
		*s = 0
	case *s <= 0:
		*s = float64(raw)
	default:
		*s = control.EwmaDt(*s, float64(raw), dtSec, CurveSmoothTauSec)
	}
}

// comfortCaps returns the per-class highest allowed comfort (cpu, passive_gpu,
// hdd, ssd): emergency - envelope.MinCurveSpan.
func comfortCaps(cfg *config.Config) [4]int {
	return [4]int{
		envelope.MaxRampStart(envelope.CPU, cfg.CPUEmergency),
		envelope.MaxRampStart(envelope.PassiveGPU, cfg.GPUEmergency),
		envelope.MaxRampStart(envelope.HDD, cfg.HDDEmergency),
		envelope.MaxRampStart(envelope.SSD, cfg.SSDEmergency),
	}
}

// fillSmoothing copies the smoothed temps + comfort caps into a snapshot.
func (c *Controller) fillSmoothing(snap *metrics.Snapshot) {
	caps := comfortCaps(c.Cfg)
	snap.CPUSmooth, snap.PGSmooth, snap.HDDSmooth, snap.SSDSmooth = c.SmoothCPUTemp, c.SmoothPGTemp, c.SmoothHDDTemp, c.SmoothSSDTemp
	snap.CPUComfortCap, snap.PGComfortCap, snap.HDDComfortCap, snap.SSDComfortCap = caps[0], caps[1], caps[2], caps[3]
}

// snapshotEmergency builds a Snapshot with the PID/floor fields zeroed.
// Bash sets them to 0 explicitly during emergency cycles so the textfile
// reflects the actual short-circuited state.
func (c *Controller) snapshotEmergency(reading sensors.Reading, source string) metrics.Snapshot {
	cfg := c.Cfg
	snap := metrics.Snapshot{
		CurrentSpeed:             c.CurrentSpeed,
		BaseSpeed:                c.BaseSpeed,
		Samples:                  c.Samples,
		CycleDurationSeconds:     c.lastCycleDuration,
		InEmergency:              1,
		CPUMax:                   reading.CPUMax,
		PassiveGPUMax:            reading.PassiveGPUMax,
		ActiveGPUMax:             reading.ActiveGPUMax,
		ActiveGPUFanMax:          reading.ActiveGPUFanMax,
		HDDMax:                   reading.HDDMax,
		SSDMax:                   reading.SSDMax,
		CPUTarget:                cfg.CPUComfort,
		PassiveGPUTarget:         cfg.GPUComfort,
		HDDTarget:                cfg.HDDComfort,
		SSDTarget:                cfg.SSDComfort,
		CPUEmergency:             cfg.CPUEmergency,
		PassiveGPUEmergency:      cfg.GPUEmergency,
		ActiveGPUEmergency:       cfg.ActiveGPUEmergency,
		ActiveGPUOwnFanThreshold: cfg.ActiveGPUOwnFanThreshold,
		HDDEmergency:             cfg.HDDEmergency,
		SSDEmergency:             cfg.SSDEmergency,
		FanDemand:                c.CurrentSpeed,
		Source:                   source,
	}
	c.fillSmoothing(&snap)
	return snap
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
