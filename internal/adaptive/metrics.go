package adaptive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pq/docker-server/host-agent/internal/envelope"
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
//	adaptive_window_inlet_mean{class}         gauge — °C
//
// Same metric formatting style as internal/metrics — no labels other
// than `class`. Float values use %.4f. Integer values use %d.
func RenderObserverMetrics(o *Observer) []byte {
	var b bytes.Buffer

	// adaptive_window_samples_filled
	fmt.Fprintf(&b, "# HELP adaptive_window_samples_filled Sample count currently in the observer's rolling window for this class.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_samples_filled gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_samples_filled{class=%q} %d\n", string(c), s.Samples)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_mean
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_mean Mean class temperature across samples in the rolling window.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_mean gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_temp_mean{class=%q} %.4f\n", string(c), s.TempMean)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_stddev
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_stddev Population standard deviation of class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_stddev gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_temp_stddev{class=%q} %.4f\n", string(c), s.TempStdDev)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_p10
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_p10 10th-percentile (nearest-rank) class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_p10 gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_temp_p10{class=%q} %.4f\n", string(c), s.TempP10)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_p50
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_p50 50th-percentile (median, nearest-rank) class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_p50 gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_temp_p50{class=%q} %.4f\n", string(c), s.TempP50)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_temp_p90
	fmt.Fprintf(&b, "# HELP adaptive_window_temp_p90 90th-percentile (nearest-rank) class temperature across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_temp_p90 gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_temp_p90{class=%q} %.4f\n", string(c), s.TempP90)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_fan_change_rate
	fmt.Fprintf(&b, "# HELP adaptive_window_fan_change_rate Number of fan-demand changes per minute across samples.\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_fan_change_rate gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_fan_change_rate{class=%q} %.4f\n", string(c), s.FanChangeRate)
	}
	fmt.Fprintf(&b, "\n")

	// adaptive_window_inlet_mean
	fmt.Fprintf(&b, "# HELP adaptive_window_inlet_mean Mean chassis inlet temperature across samples (currently 0 — inlet plumbing deferred).\n")
	fmt.Fprintf(&b, "# TYPE adaptive_window_inlet_mean gauge\n")
	for _, c := range classesInRenderOrder {
		s := o.Stats(c)
		fmt.Fprintf(&b, "adaptive_window_inlet_mean{class=%q} %.4f\n", string(c), s.InletMean)
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
