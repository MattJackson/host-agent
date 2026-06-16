package adaptive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// classesInRenderOrder is the stable order observer metrics are emitted
// in, so dashboards see a consistent series-label sequence.
var classesInRenderOrder = []envelope.Class{
	envelope.CPU,
	envelope.PassiveGPU,
	envelope.HDD,
	envelope.SSD,
}

// RenderObserverMetrics produces the textfile-collector bytes for the
// observer's current per-class stats. Each metric is emitted with one
// HELP/TYPE pair and one line per class label.
//
// Series emitted (per design §13):
//
//	adaptive_window_samples_filled{class}     gauge — N samples held
//	adaptive_window_temp_mean{class}          gauge — °C
//	adaptive_window_temp_stddev{class}        gauge — °C
//	adaptive_window_temp_p10{class}           gauge — °C
//	adaptive_window_temp_p50{class}           gauge — °C
//	adaptive_window_temp_p90{class}           gauge — °C
//	adaptive_window_fan_change_rate{class}    gauge — changes/min
//	adaptive_window_fan_demand_p90{class}     gauge — percent (saturation signal)
//	adaptive_window_inlet_mean{class}         gauge — °C
//
// Same metric formatting style as internal/metrics — no labels other
// than `class`. Float values use %.4f. Integer values use %d.
func RenderObserverMetrics(o *Observer) []byte {
	var b bytes.Buffer

	// Compute each class's stats once up front. Stats() takes the observer
	// lock and recomputes mean/stddev/percentiles (two sorts) on every call,
	// so calling it once per metric block would lock+recompute 9× per class
	// per render — and could even render inconsistent values for the same
	// class across metric lines if a sample landed mid-render. One snapshot
	// per class avoids both.
	stats := make(map[envelope.Class]mode.WindowStats, len(classesInRenderOrder))
	for _, c := range classesInRenderOrder {
		stats[c] = o.Stats(c)
	}

	// adaptive_window_samples_filled
	fmt.Fprintf(&b, "# HELP adaptive_window_samples_filled Sample count currently in the observer's rolling window for this class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_samples_filled gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_samples_filled{class=%q} %d\n", string(c), s.Samples)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_mean
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_mean Mean class temperature across samples in the rolling window.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_mean gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_temp_mean{class=%q} %.4f\n", string(c), s.TempMean)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_stddev
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_stddev Population standard deviation of class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_stddev gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_temp_stddev{class=%q} %.4f\n", string(c), s.TempStdDev)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_p10
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_p10 10th-percentile (nearest-rank) class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_p10 gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_temp_p10{class=%q} %.4f\n", string(c), s.TempP10)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_p50
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_p50 50th-percentile (median, nearest-rank) class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_p50 gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_temp_p50{class=%q} %.4f\n", string(c), s.TempP50)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_p90
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_p90 90th-percentile (nearest-rank) class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_p90 gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_temp_p90{class=%q} %.4f\n", string(c), s.TempP90)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_fan_change_rate
	fmt.Fprintf(&b, "# HELP adaptive_window_fan_change_rate Number of fan-demand changes per minute across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_fan_change_rate gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_fan_change_rate{class=%q} %.4f\n", string(c), s.FanChangeRate)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_inlet_mean
	fmt.Fprintf(&b, "# HELP adaptive_window_inlet_mean Mean chassis inlet temperature across samples (currently 0 — inlet plumbing deferred).\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_inlet_mean gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_inlet_mean{class=%q} %.4f\n", string(c), s.InletMean)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_fan_demand_p90 — the saturation signal the reconciler
	// scores on. A high p90 with a low fan_change_rate is a pinned fan; this
	// is what the saturation penalty keys off (robust to transient dips that
	// would drag the mean down). Exposed for diagnosing fan-saturation drift.
	fmt.Fprintf(&b, "# HELP adaptive_window_fan_demand_p90 90th-percentile fan demand (percent) across samples in the rolling window.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_fan_demand_p90 gauge\n")
	for _, c := range classesInRenderOrder {
		s := stats[c]
		fmt.Fprintf(&b, "adaptive_window_fan_demand_p90{class=%q} %.4f\n", string(c), s.FanDemandP90)
	}
	return b.Bytes()
}

// WriteObserverMetrics writes RenderObserverMetrics output atomically
// to path. Uses temp-file-then-rename so node-exporter never reads a
// torn file. Errors are returned; caller (main loop) should tolerate
// them — a metrics-write failure must NOT stop the controller.
func WriteObserverMetrics(path string, o *Observer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, RenderObserverMetrics(o), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RenderReconcilerMetrics produces the textfile-collector bytes for the
// Reconciler's current state + drift counters. Each metric has its own
// HELP/TYPE pair and one line per class label in classesInRenderOrder order.
//
// Series emitted:
// adaptive_mode_info{mode="balanced"} 1
// adaptive_target_celsius{class} 65
// adaptive_deadband_celsius{class} 3
// adaptive_envelope_preferred_low{class} 55
// adaptive_envelope_preferred_high{class} 75
// adaptive_envelope_max_safe{class} 85
// adaptive_target_drifts_total{class,direction} 12
// adaptive_target_resets_total{class,reason} 1
func RenderReconcilerMetrics(r *Reconciler, obs *Observer) []byte {
	var b bytes.Buffer

	metrics := r.Metrics()

	// adaptive_mode_info — single series, no class label.
	fmt.Fprintf(&b, "# HELP adaptive_mode_info Current active mode of the adaptive controller.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_mode_info gauge\n")
	fmt.Fprintf(&b, "adaptive_mode_info{mode=%q} 1\n", string(metrics.Mode))
	fmt.Fprintf(&b, "\n")

	// adaptive_target_celsius — one HELP/TYPE, one line per class.
	fmt.Fprintf(&b, "# HELP adaptive_target_celsius Current adaptive target temperature for the class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_target_celsius gauge\n")
	for _, c := range classesInRenderOrder {
		if cm, ok := metrics.Classes[c]; ok {
			fmt.Fprintf(&b, "adaptive_target_celsius{class=%q} %.4f\n", string(c), cm.TargetCelsius)
		}
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_deadband_celsius
	fmt.Fprintf(&b, "# HELP adaptive_deadband_celsius Current adaptive deadband for the class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_deadband_celsius gauge\n")
	for _, c := range classesInRenderOrder {
		if cm, ok := metrics.Classes[c]; ok {
			fmt.Fprintf(&b, "adaptive_deadband_celsius{class=%q} %.4f\n", string(c), cm.DeadbandCelsius)
		}
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_envelope_preferred_low
	fmt.Fprintf(&b, "# HELP adaptive_envelope_preferred_low Envelope preferred-low temperature for the class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_envelope_preferred_low gauge\n")
	for _, c := range classesInRenderOrder {
		if cm, ok := metrics.Classes[c]; ok {
			fmt.Fprintf(&b, "adaptive_envelope_preferred_low{class=%q} %d\n", string(c), cm.Envelope.PreferredLow)
		}
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_envelope_preferred_high
	fmt.Fprintf(&b, "# HELP adaptive_envelope_preferred_high Envelope preferred-high temperature for the class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_envelope_preferred_high gauge\n")
	for _, c := range classesInRenderOrder {
		if cm, ok := metrics.Classes[c]; ok {
			fmt.Fprintf(&b, "adaptive_envelope_preferred_high{class=%q} %d\n", string(c), cm.Envelope.PreferredHigh)
		}
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_envelope_max_safe
	fmt.Fprintf(&b, "# HELP adaptive_envelope_max_safe Envelope max-safe temperature for the class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_envelope_max_safe gauge\n")
	for _, c := range classesInRenderOrder {
		if cm, ok := metrics.Classes[c]; ok {
			fmt.Fprintf(&b, "adaptive_envelope_max_safe{class=%q} %d\n", string(c), cm.Envelope.MaxSafe)
		}
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_target_drifts_total — one HELP/TYPE for the whole metric,
	// then one line per (class, direction) tuple that has a non-zero count.
	// Always emit the HELP/TYPE block so the metric exists in Prometheus
	// even before any drift event happens.
	fmt.Fprintf(&b, "# HELP adaptive_target_drifts_total Total number of target drift events per class+direction.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_target_drifts_total counter\n")
	for _, c := range classesInRenderOrder {
		if dirs, ok := metrics.Drifts[c]; ok {
			// Stable, complete order. "bounded_high"/"bounded_low" count
			// cycles where the score wanted to drift but the target was
			// pinned at a clamp — a key envelope-misconfiguration signal,
			// so they must render alongside actual "up"/"down" drifts.
			for _, dir := range []string{"up", "down", "bounded_high", "bounded_low"} {
				if count, ok := dirs[dir]; ok && count > 0 {
					fmt.Fprintf(&b, "adaptive_target_drifts_total{class=%q,direction=%q} %d\n", string(c), dir, count)
				}
			}
		}
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_target_resets_total — one HELP/TYPE, one line per
	// (class, reason) tuple with a non-zero count.
	fmt.Fprintf(&b, "# HELP adaptive_target_resets_total Total number of target reset events per class+reason.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_target_resets_total counter\n")
	for _, c := range classesInRenderOrder {
		if reasons, ok := metrics.Resets[c]; ok {
			for reason, count := range reasons {
				if count > 0 {
					fmt.Fprintf(&b, "adaptive_target_resets_total{class=%q,reason=%q} %d\n", string(c), string(reason), count)
				}
			}
		}
	}
	fmt.Fprintf(&b, "\n")

	// ── Mode preview metrics ────────────────────────────────────────
	// For each mode (not just the active one), emit what its initial
	// target would be per class, and what the CURRENT observed stats
	// would score under that mode's intent. Lets operators see at a
	// glance: "what if I switched to min-noise — what would change?"
	// and "how well does my current hardware behavior fit each mode?"
	// (Lower score = better fit for that mode's intent.)
	allModes := []mode.Mode{mode.MaxCool, mode.Balanced, mode.MinNoise, mode.Eco}

	fmt.Fprintf(&b, "# HELP adaptive_mode_preview_target_celsius Initial target temperature this mode would set for this class (independent of current state).\n")
	fmt.Fprintf(&b, "# TYPE adaptive_mode_preview_target_celsius gauge\n")
	for _, c := range classesInRenderOrder {
		cm, ok := metrics.Classes[c]
		if !ok {
			continue
		}
		for _, m := range allModes {
			t, _ := mode.InitialTarget(cm.Envelope, m)
			fmt.Fprintf(&b, "adaptive_mode_preview_target_celsius{class=%q,mode=%q} %d\n", string(c), string(m), t)
		}
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "# HELP adaptive_mode_preview_deadband_celsius Initial deadband this mode would set for this class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_mode_preview_deadband_celsius gauge\n")
	for _, c := range classesInRenderOrder {
		cm, ok := metrics.Classes[c]
		if !ok {
			continue
		}
		for _, m := range allModes {
			_, d := mode.InitialTarget(cm.Envelope, m)
			fmt.Fprintf(&b, "adaptive_mode_preview_deadband_celsius{class=%q,mode=%q} %d\n", string(c), string(m), d)
		}
	}
	fmt.Fprintf(&b, "\n")

	// Score under each mode given CURRENT observed stats. Only emitted
	// when the observer reference is present (skipped at startup before
	// observer wiring is complete).
	if obs != nil {
		fmt.Fprintf(&b, "# HELP adaptive_mode_preview_score Score of current observed stats under this mode's intent (lower = better fit for that mode given current hardware behavior).\n")
		fmt.Fprintf(&b, "# TYPE adaptive_mode_preview_score gauge\n")
		for _, c := range classesInRenderOrder {
			cm, ok := metrics.Classes[c]
			if !ok {
				continue
			}
			stats := obs.Stats(c)
			for _, m := range allModes {
				score := m.Score()(cm.Envelope, stats)
				fmt.Fprintf(&b, "adaptive_mode_preview_score{class=%q,mode=%q} %.4f\n", string(c), string(m), score)
			}
		}
	}

	return b.Bytes()
}

// WriteAdaptiveMetrics atomically writes the combined observer+reconciler
// textfile-collector output. If r is nil, only observer metrics are emitted
// (e.g. when HOST_AGENT_ADAPTIVE_DISABLED). Uses temp-file-then-rename for
// atomicity so node-exporter never reads a torn file.
func WriteAdaptiveMetrics(path string, o *Observer, r *Reconciler) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	var buf bytes.Buffer
	buf.Write(RenderObserverMetrics(o))
	if r != nil {
		buf.WriteByte('\n')
		buf.Write(RenderReconcilerMetrics(r, o))
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
