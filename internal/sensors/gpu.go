package sensors

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

// GPU sources per-GPU temps via nvidia-smi and classifies each card by
// its fan.speed field:
//
//	passive (Tesla P4, etc — no fan, chassis-cooled) → drives PID
//	active  (RTX A5500 — own fan)                    → drives assist
type GPU struct {
	Runner  runner.Runner
	Enabled bool
}

// NewGPU constructs a default GPU sensor. Call Probe to populate Enabled.
func NewGPU(r runner.Runner) *GPU { return &GPU{Runner: r} }

// Probe determines whether nvidia-smi is usable. Mode is one of
// "auto", "true", "false" (matches GPU_AWARE values from profiles).
//
// Returns (label, fatal). label is a human-readable description for
// the startup log; fatal=true means GPU_AWARE=true was set but
// nvidia-smi failed — caller should exit.
func (g *GPU) Probe(ctx context.Context, mode string) (label string, fatal bool) {
	switch mode {
	case "false":
		g.Enabled = false
		return "GPU monitoring disabled (GPU_AWARE=false)", false
	case "true":
		out, err := g.Runner.Run(ctx, "nvidia-smi", "-L")
		if err != nil || out == "" {
			return "FATAL: GPU_AWARE=true but nvidia-smi not usable in container", true
		}
		g.Enabled = true
		return "GPU monitoring: " + g.summarize(ctx), false
	default: // "auto" and anything else
		out, err := g.Runner.Run(ctx, "nvidia-smi", "-L")
		if err != nil || out == "" {
			g.Enabled = false
			return "No GPU detected (CPU-only mode)", false
		}
		g.Enabled = true
		return "GPU detected: " + g.summarize(ctx), false
	}
}

func (g *GPU) summarize(ctx context.Context) string {
	out, err := g.Runner.Run(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if err != nil {
		return ""
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return strings.Join(names, ", ")
}

// Read returns (passiveMax, activeMax, details, ok). ok=false means
// the GPU subsystem is disabled OR nvidia-smi returned no data — the
// controller treats this identically to "no GPU temperatures this
// cycle" via PID abstain semantics.
//
// nvidia-smi CSV format:
//
//	index, temperature.gpu, fan.speed
//	0, 75, [N/A]               (passive — no fan)
//	1, 60, 80                  (active — own fan @ 80%)
//	2, 55, [NotSupported]      (passive variant)
func (g *GPU) Read(ctx context.Context) (passiveMax, activeMax int, details string, ok bool) {
	if !g.Enabled {
		return 0, 0, "", false
	}
	out, err := g.Runner.Run(ctx, "nvidia-smi",
		"--query-gpu=index,temperature.gpu,fan.speed",
		"--format=csv,noheader,nounits")
	if err != nil {
		return 0, 0, "", false
	}
	var b detailsBuilder
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		idx := strings.TrimSpace(parts[0])
		tempStr := strings.TrimSpace(parts[1])
		fanRaw := strings.TrimSpace(parts[2])
		fan := strings.TrimSuffix(fanRaw, "%") // shouldn't appear with nounits, but match bash's gsub
		temp, err := strconv.Atoi(tempStr)
		if err != nil {
			continue
		}
		switch fan {
		case "", "[N/A]", "[NotSupported]":
			b.Add(fmt.Sprintf("Gp%s:%d", idx, temp))
			if temp > passiveMax {
				passiveMax = temp
			}
		default:
			b.Add(fmt.Sprintf("Ga%s:%d@%s%%", idx, temp, fan))
			if temp > activeMax {
				activeMax = temp
			}
		}
	}
	return passiveMax, activeMax, b.String(), true
}
