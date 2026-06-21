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

	// Diagnostic: wall-clock seconds the last Cycle() call took.
	// Exposed via metrics.Snapshot.CycleDurationSeconds so we can SEE
	// in Grafana how long each cycle takes instead of guessing.
	lastCycleDuration float64

	// D-term per class.
	LastCPUTemp int
	LastPGTemp  int
	LastHDDTemp int
	LastSSDTemp int

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
	}
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
	_ = c.IPMI.EngageManual(ctx)
	reading, ok := c.Reader.Read(ctx)
	if !ok {
		c.Log.Printf("Temp read failed — fans 100%% for safety")
		_ = c.IPMI.SetFan(ctx, 100)
		c.CurrentSpeed = 100
		// Build a degenerate snapshot for metrics — emergency=1 conveys
		// the safety state even though no class fired.
		snap := c.snapshotEmergency(reading, "emergency")
		_ = metrics.WriteAtomic(c.MetricsPath, snap)
		return snap
	}

	cfg := c.Cfg
	// Emergency check: any class >= its emergency threshold.
	if reading.CPUMax >= cfg.CPUEmergency ||
		reading.PassiveGPUMax >= cfg.GPUEmergency ||
		reading.ActiveGPUMax >= cfg.ActiveGPUEmergency ||
		(reading.HDDMax > 0 && reading.HDDMax >= cfg.HDDEmergency) ||
		(reading.SSDMax > 0 && reading.SSDMax >= cfg.SSDEmergency) {
		if c.CurrentSpeed != 100 {
			_ = c.IPMI.SetFan(ctx, 100)
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
	// fan. The v3 curve is memoryless — a pure function of the current
	// temperature — so once temps drop back under emergency it lands on the
	// correct setpoint on this very cycle; no special exit-speed handoff needed.
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
	cpuCurve := control.Curve(reading.CPUMax, cfg.CPUComfort, cfg.CPUEmergency, cfg.MinFan, cfg.MaxFan)
	pgCurve := control.Curve(reading.PassiveGPUMax, cfg.GPUComfort, cfg.GPUEmergency, cfg.MinFan, cfg.MaxFan)
	hddCurve := control.Curve(reading.HDDMax, cfg.HDDComfort, cfg.HDDEmergency, cfg.MinFan, cfg.MaxFan)
	ssdCurve := control.Curve(reading.SSDMax, cfg.SSDComfort, cfg.SSDEmergency, cfg.MinFan, cfg.MaxFan)

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

	c.CurrentSpeed = r.NewSpeed
	// Re-issue SetFan every cycle, not only on change. The BMC's
	// revert-to-auto watchdog tracks the fan-PWM command specifically;
	// a steady-state cycle that skips SetFan lets manual control lapse
	// and fans run away to 100%. Idempotent — same value to same BMC.
	_ = c.IPMI.SetFan(ctx, c.CurrentSpeed)

	// Log line.
	c.Log.Printf("%scpu:%d p_gpu:%d a_gpu:%d hdd:%d ssd:%d | curve c%d/p%d/h%d/s%d ag_assist:%d → %d%%(%s)",
		reading.Details,
		reading.CPUMax, reading.PassiveGPUMax, reading.ActiveGPUMax, reading.HDDMax, reading.SSDMax,
		cpuCurve, pgCurve, hddCurve, ssdCurve,
		agAssist, c.CurrentSpeed, r.Source)

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
	}
	if reading.PassiveGPUMax > 0 {
		c.LastPGTemp = reading.PassiveGPUMax
	}
	if reading.HDDMax > 0 {
		c.LastHDDTemp = reading.HDDMax
	}
	if reading.SSDMax > 0 {
		c.LastSSDTemp = reading.SSDMax
	}

	// Persist + metrics.
	now := c.Now()
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
		CPUCand:  cpuCurve,
		PGCand:   pgCurve,
		HDDCand:  hddCurve,
		SSDCand:  ssdCurve,
		CPUPF:    0,
		PGPF:     0,
		AGPF:     0,
		HDDPF:    0,
		SSDPF:    0,
		AGAssist: agAssist,
		Source:   r.Source,
	}
	_ = metrics.WriteAtomic(c.MetricsPath, snap)
	return snap
}

// snapshotEmergency builds a Snapshot with the PID/floor fields zeroed.
// Bash sets them to 0 explicitly during emergency cycles so the textfile
// reflects the actual short-circuited state.
func (c *Controller) snapshotEmergency(reading sensors.Reading, source string) metrics.Snapshot {
	cfg := c.Cfg
	return metrics.Snapshot{
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
		Source:                   source,
	}
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
