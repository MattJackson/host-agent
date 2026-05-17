// fan-controller — Dell PowerEdge adaptive fan controller.
// Per-class PIDs (CPU, passive_gpu, active_gpu, hdd, ssd) emit
// candidate fan speeds; max() wins, plus per-class proximity floors
// and an active-GPU assist lift. EWMA-tracked equilibrium baseline
// persisted to /var/lib/fan-controller/state/base. See host-agent/README.md.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pq/docker-server/host-agent/internal/adaptive"
	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/controller"
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/runner"
	"github.com/pq/docker-server/host-agent/internal/sensors"
)

// version is stamped at build time via -ldflags.
var version = "dev"

const (
	profileDir  = "/etc/fan-controller/profiles"
	stateDir    = "/var/lib/fan-controller/state"
	stateFile   = "/var/lib/fan-controller/state/base"
	metricsFile = "/var/lib/fan-controller/state/metrics.prom"
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
	logActiveProfile(logger, cfg)

	m, modeSet, modeErr := config.ApplyMode(cfg)
	if modeErr != nil {
		logger.Printf("WARN: %v (falling back to %s)", modeErr, m)
	} else if modeSet {
		logger.Printf("HOST_AGENT_MODE=%s (explicit)", m)
	} else {
		logger.Printf("HOST_AGENT_MODE unset; using v1 profile values + %s defaults for any unset class", m)
	}

	// 2b: Build adaptive observer.
	obs := buildObserver(logger, cfg)

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

	// 6. Main loop. Cycle every cfg.IntervalSec; persist + return-to-auto
	// on signal.
	interval := time.Duration(cfg.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// First cycle runs immediately (don't wait for the first tick).
	runCycle(ctx, c)
	sampleObserver(obs, c)

	for {
		select {
		case <-ctx.Done():
			logger.Printf("Shutting down — returning fan control to iDRAC automatic")
			_ = c.PersistState()
			// Use a fresh context for the handback — the parent context
			// is already cancelled, but ipmitool still needs ~100ms.
			handbackCtx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = ipmiClient.HandbackAuto(handbackCtx)
			hcancel()
			return
		case <-ticker.C:
			runCycle(ctx, c)
			sampleObserver(obs, c)
		}
	}
}

func runCycle(ctx context.Context, c *controller.Controller) {
	// Each cycle gets its own short context for subprocess deadlines.
	// 30s default per runner.Exec call; the cycle wrapper here caps
	// the whole cycle at 60s in case multiple subprocess calls stack.
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
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
// OBSERVER_WINDOW_MINUTES (default 120) × 60 / cfg.IntervalSec.
//
// Inlet temp is not yet plumbed through internal/sensors.Reading;
// we pass 0 for InletCelsius until a later task adds it. The
// inlet-jump reset feature is therefore a no-op in Phase 2.
func buildObserver(logger controller.Logger, cfg *config.Config) *adaptive.Observer {
	windowMinutes := 120
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
