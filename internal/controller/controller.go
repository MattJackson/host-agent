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
	lastCycleDuration int

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
		c.lastCycleDuration = int(time.Since(cycleStart).Round(time.Second).Seconds())
	}()
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

	// Exiting emergency? Pick max(MinFan, base, every proximity floor).
	if c.InEmergency {
		intBase := clampInt(int(c.BaseSpeed+0.5), cfg.MinFan, cfg.MaxFan)
		exitSpeed := intBase
		floors := c.proximityFloors(reading)
		for _, f := range floors {
			if f.Value > exitSpeed {
				exitSpeed = f.Value
			}
		}
		c.CurrentSpeed = exitSpeed
		_ = c.IPMI.SetFan(ctx, c.CurrentSpeed)
		c.Log.Printf("Emergency cleared — fan=%d%% (base=%d cpu_pf=%d pg_pf=%d ag_pf=%d hdd_pf=%d ssd_pf=%d)",
			c.CurrentSpeed, intBase,
			floors[0].Value, floors[1].Value, floors[2].Value, floors[3].Value, floors[4].Value)
		c.InEmergency = false
	}

	// Compute per-class PID candidates.
	cpuCand := control.StepPID(control.PIDParams{
		Temp: reading.CPUMax, Target: cfg.CPUTarget, Deadband: cfg.CPUDeadband,
		LastTemp: c.LastCPUTemp, CurrentSpeed: c.CurrentSpeed,
		MinFan: cfg.MinFan, MaxFan: cfg.MaxFan,
		FanGain: cfg.FanGain, DerivativeGain: cfg.DerivativeGain,
		DeadbandDriftRate: cfg.DeadbandDriftRate,
	})
	pgCand := control.StepPID(control.PIDParams{
		Temp: reading.PassiveGPUMax, Target: cfg.GPUTarget, Deadband: cfg.GPUDeadband,
		LastTemp: c.LastPGTemp, CurrentSpeed: c.CurrentSpeed,
		MinFan: cfg.MinFan, MaxFan: cfg.MaxFan,
		FanGain: cfg.FanGain, DerivativeGain: cfg.DerivativeGain,
		DeadbandDriftRate: cfg.DeadbandDriftRate,
	})
	hddCand := control.StepPID(control.PIDParams{
		Temp: reading.HDDMax, Target: cfg.HDDTarget, Deadband: cfg.HDDDeadband,
		LastTemp: c.LastHDDTemp, CurrentSpeed: c.CurrentSpeed,
		MinFan: cfg.MinFan, MaxFan: cfg.MaxFan,
		FanGain: cfg.FanGain, DerivativeGain: cfg.DerivativeGain,
		DeadbandDriftRate: cfg.DeadbandDriftRate,
	})
	ssdCand := control.StepPID(control.PIDParams{
		Temp: reading.SSDMax, Target: cfg.SSDTarget, Deadband: cfg.SSDDeadband,
		LastTemp: c.LastSSDTemp, CurrentSpeed: c.CurrentSpeed,
		MinFan: cfg.MinFan, MaxFan: cfg.MaxFan,
		FanGain: cfg.FanGain, DerivativeGain: cfg.DerivativeGain,
		DeadbandDriftRate: cfg.DeadbandDriftRate,
	})

	// Per-class proximity floors. Each gates on temp > 0.
	floors := c.proximityFloors(reading)
	cpuPF, pgPF, agPF, hddPF, ssdPF := floors[0].Value, floors[1].Value, floors[2].Value, floors[3].Value, floors[4].Value

	// Active-GPU assist (chassis-floor lift when active GPU > target).
	agAssist := 0
	if reading.ActiveGPUMax > 0 {
		agAssist = control.ActiveGPUAssist(
			reading.ActiveGPUMax, cfg.ActiveGPUTarget,
			cfg.AssistGain, cfg.MinFan, cfg.MaxFan,
		)
	}

	// max-wins aggregation — order matches bash so tie-breaking does too.
	r := control.MaxWins(
		control.MaxCandidate{Name: "cpu", Value: cpuCand},
		[]control.MaxCandidate{
			{Name: "pg", Value: pgCand},
			{Name: "hdd", Value: hddCand},
			{Name: "ssd", Value: ssdCand},
			{Name: "cpu_pf", Value: cpuPF},
			{Name: "pg_pf", Value: pgPF},
			{Name: "ag_pf", Value: agPF},
			{Name: "hdd_pf", Value: hddPF},
			{Name: "ssd_pf", Value: ssdPF},
			{Name: "ag_assist", Value: agAssist},
		},
		cfg.MinFan, cfg.MaxFan,
	)

	if r.NewSpeed != c.CurrentSpeed {
		c.CurrentSpeed = r.NewSpeed
		_ = c.IPMI.SetFan(ctx, c.CurrentSpeed)
	}

	// Log line — match bash shape.
	c.Log.Printf("%scpu:%d p_gpu:%d a_gpu:%d hdd:%d ssd:%d | pid c%d/p%d/h%d/s%d pf c%d/p%d/a%d/h%d/s%d ag_assist:%d → %d%%(%s) base:%s",
		reading.Details,
		reading.CPUMax, reading.PassiveGPUMax, reading.ActiveGPUMax, reading.HDDMax, reading.SSDMax,
		cpuCand, pgCand, hddCand, ssdCand,
		cpuPF, pgPF, agPF, hddPF, ssdPF,
		agAssist, c.CurrentSpeed, r.Source, formatBase(c.BaseSpeed))

	// EWMA + samples.
	c.BaseSpeed = control.Ewma(c.BaseSpeed, float64(c.CurrentSpeed), cfg.AdaptAlpha)
	c.Samples++

	// D-term state update — only update classes that returned a real reading.
	c.LastCPUTemp = reading.CPUMax
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
		HDDMax:               reading.HDDMax,
		SSDMax:               reading.SSDMax,
		CPUTarget:            cfg.CPUTarget,
		PassiveGPUTarget:     cfg.GPUTarget,
		ActiveGPUTarget:      cfg.ActiveGPUTarget,
		HDDTarget:            cfg.HDDTarget,
		SSDTarget:            cfg.SSDTarget,
		CPUEmergency:         cfg.CPUEmergency,
		PassiveGPUEmergency:  cfg.GPUEmergency,
		ActiveGPUEmergency:   cfg.ActiveGPUEmergency,
		HDDEmergency:         cfg.HDDEmergency,
		SSDEmergency:         cfg.SSDEmergency,
		CPUCand:              cpuCand,
		PGCand:               pgCand,
		HDDCand:              hddCand,
		SSDCand:              ssdCand,
		CPUPF:                cpuPF,
		PGPF:                 pgPF,
		AGPF:                 agPF,
		HDDPF:                hddPF,
		SSDPF:                ssdPF,
		AGAssist:             agAssist,
		Source:               r.Source,
	}
	_ = metrics.WriteAtomic(c.MetricsPath, snap)
	return snap
}

// proximityFloors computes the 5 per-class proximity floors in canonical
// order: cpu, pg, ag, hdd, ssd. Returns a fixed-length slice.
func (c *Controller) proximityFloors(reading sensors.Reading) []control.MaxCandidate {
	cfg := c.Cfg
	floors := []control.MaxCandidate{
		{Name: "cpu_pf", Value: 0},
		{Name: "pg_pf", Value: 0},
		{Name: "ag_pf", Value: 0},
		{Name: "hdd_pf", Value: 0},
		{Name: "ssd_pf", Value: 0},
	}
	if reading.CPUMax > 0 {
		floors[0].Value = control.ProximityFloor(reading.CPUMax, cfg.CPUEmergency, cfg.CPUApproachWindow, cfg.MinFan, cfg.MaxFan)
	}
	if reading.PassiveGPUMax > 0 {
		floors[1].Value = control.ProximityFloor(reading.PassiveGPUMax, cfg.GPUEmergency, cfg.GPUApproachWindow, cfg.MinFan, cfg.MaxFan)
	}
	if reading.ActiveGPUMax > 0 {
		floors[2].Value = control.ProximityFloor(reading.ActiveGPUMax, cfg.ActiveGPUEmergency, cfg.ActiveGPUApproachWindow, cfg.MinFan, cfg.MaxFan)
	}
	if reading.HDDMax > 0 {
		floors[3].Value = control.ProximityFloor(reading.HDDMax, cfg.HDDEmergency, cfg.HDDApproachWindow, cfg.MinFan, cfg.MaxFan)
	}
	if reading.SSDMax > 0 {
		floors[4].Value = control.ProximityFloor(reading.SSDMax, cfg.SSDEmergency, cfg.SSDApproachWindow, cfg.MinFan, cfg.MaxFan)
	}
	return floors
}

// snapshotEmergency builds a Snapshot with the PID/floor fields zeroed.
// Bash sets them to 0 explicitly during emergency cycles so the textfile
// reflects the actual short-circuited state.
func (c *Controller) snapshotEmergency(reading sensors.Reading, source string) metrics.Snapshot {
	cfg := c.Cfg
	return metrics.Snapshot{
		CurrentSpeed:         c.CurrentSpeed,
		BaseSpeed:            c.BaseSpeed,
		Samples:              c.Samples,
		CycleDurationSeconds: c.lastCycleDuration,
		InEmergency:          1,
		CPUMax:               reading.CPUMax,
		PassiveGPUMax:        reading.PassiveGPUMax,
		ActiveGPUMax:         reading.ActiveGPUMax,
		HDDMax:               reading.HDDMax,
		SSDMax:               reading.SSDMax,
		CPUTarget:            cfg.CPUTarget,
		PassiveGPUTarget:     cfg.GPUTarget,
		ActiveGPUTarget:      cfg.ActiveGPUTarget,
		HDDTarget:            cfg.HDDTarget,
		SSDTarget:            cfg.SSDTarget,
		CPUEmergency:         cfg.CPUEmergency,
		PassiveGPUEmergency:  cfg.GPUEmergency,
		ActiveGPUEmergency:   cfg.ActiveGPUEmergency,
		HDDEmergency:         cfg.HDDEmergency,
		SSDEmergency:         cfg.SSDEmergency,
		Source:               source,
	}
}

// formatBase renders the EWMA baseline the way bash logs it. After any
// ewma() call, $base_speed is awk's "%.4f" output (always 4 decimals).
// We replicate that. On the first cycle (before any EWMA update has
// run), the bash original logs the raw integer assigned from $MIN_FAN
// — but the log line we're matching here runs AFTER one full cycle,
// post-EWMA, so %.4f is always correct.
func formatBase(b float64) string {
	return fmt.Sprintf("%.4f", b)
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
