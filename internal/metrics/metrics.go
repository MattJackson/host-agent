// Package metrics emits the Prometheus textfile-collector file that
// the bash original writes to /var/lib/host-agent/state/metrics.prom.
//
// The shape (metric names, label keys, label values, HELP/TYPE lines)
// must match the bash exactly — node-exporter's textfile collector
// picks the file up unchanged and the Grafana dashboard queries
// against these exact series.
package metrics

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot is the per-cycle inputs the metrics file reflects.
type Snapshot struct {
	// Setpoint and EWMA baseline.
	CurrentSpeed int
	BaseSpeed    float64
	Samples      int
	InEmergency  int // 0 or 1

	// Wall-clock seconds the most recent cycle took (work only, NOT including the sleep INTERVAL).
	// Float so sub-second cycles are visible (an integer rounds an 800ms cycle to 0).
	CycleDurationSeconds float64

	// Per-class temps (0 if absent).
	CPUMax        int
	PassiveGPUMax int
	ActiveGPUMax  int
	HDDMax        int
	SSDMax        int

	// Active GPU's max OWN fan speed (%) — what triggers chassis assist.
	ActiveGPUFanMax int

	// Per-class targets/emergencies (from config).
	CPUTarget           int
	PassiveGPUTarget    int
	HDDTarget           int
	SSDTarget           int
	CPUEmergency        int
	PassiveGPUEmergency int
	ActiveGPUEmergency  int
	HDDEmergency        int
	SSDEmergency        int
	// Active GPU has no temperature target — chassis-assist threshold
	// is keyed off the card's OWN fan speed.
	ActiveGPUOwnFanThreshold int

	// Per-class PID candidates (0 in emergency).
	CPUCand int
	PGCand  int
	HDDCand int
	SSDCand int

	// Per-class proximity floors (0 in emergency).
	CPUPF int
	PGPF  int
	AGPF  int
	HDDPF int
	SSDPF int

	// Active-GPU intake-air assist (0 below target).
	AGAssist int

	// Binding source string — "cpu", "pg", "hdd", "ssd",
	// "cpu_pf", "pg_pf", "ag_pf", "hdd_pf", "ssd_pf", "ag_assist",
	// or "emergency".
	Source string
}

// Render returns the bytes of the textfile. The format (and ordering)
// match the bash heredoc in emit_metrics() exactly.
func Render(s Snapshot) []byte {
	var b bytes.Buffer
	src := s.Source
	if src == "" {
		src = "emergency"
	}
	fmt.Fprintf(&b, "# HELP fan_controller_fan_setpoint_percent Current chassis fan setpoint commanded by controller.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_fan_setpoint_percent gauge\n")
	fmt.Fprintf(&b, "fan_controller_fan_setpoint_percent %d\n", s.CurrentSpeed)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_base_speed_percent EWMA-smoothed baseline fan speed (24-48h adaptation).\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_base_speed_percent gauge\n")
	// Match bash %.4f formatting for base_speed in the metric body.
	fmt.Fprintf(&b, "fan_controller_base_speed_percent %.4f\n", s.BaseSpeed)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_samples_total Number of successful (non-emergency, temps-readable) PID cycles since first run (persisted across restarts; resets to 0 only if state is lost). Does not advance during sustained emergencies or sensor-read failures.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_samples_total counter\n")
	fmt.Fprintf(&b, "fan_controller_samples_total %d\n", s.Samples)

	fmt.Fprintf(&b, "\n# HELP fan_controller_cycle_duration_seconds Wall-clock seconds for the most recent main-loop cycle (get_temps + PIDs + proximity_floor + max() + set_fan, NOT including sleep INTERVAL).\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_cycle_duration_seconds gauge\n")
	fmt.Fprintf(&b, "fan_controller_cycle_duration_seconds %.3f\n", s.CycleDurationSeconds)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_emergency_active Whether the controller is in emergency state (1=yes, 0=no).\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_emergency_active gauge\n")
	fmt.Fprintf(&b, "fan_controller_emergency_active %d\n", s.InEmergency)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_class_temp_celsius Max temperature observed for a hardware class this cycle.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_class_temp_celsius gauge\n")
	fmt.Fprintf(&b, "fan_controller_class_temp_celsius{class=\"cpu\"} %d\n", s.CPUMax)
	fmt.Fprintf(&b, "fan_controller_class_temp_celsius{class=\"passive_gpu\"} %d\n", s.PassiveGPUMax)
	fmt.Fprintf(&b, "fan_controller_class_temp_celsius{class=\"active_gpu\"} %d\n", s.ActiveGPUMax)
	fmt.Fprintf(&b, "fan_controller_class_temp_celsius{class=\"hdd\"} %d\n", s.HDDMax)
	fmt.Fprintf(&b, "fan_controller_class_temp_celsius{class=\"ssd\"} %d\n", s.SSDMax)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_class_target_celsius Per-class target temperature (deadband center) or assist threshold.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_class_target_celsius gauge\n")
	fmt.Fprintf(&b, "fan_controller_class_target_celsius{class=\"cpu\"} %d\n", s.CPUTarget)
	fmt.Fprintf(&b, "fan_controller_class_target_celsius{class=\"passive_gpu\"} %d\n", s.PassiveGPUTarget)
	fmt.Fprintf(&b, "fan_controller_class_target_celsius{class=\"hdd\"} %d\n", s.HDDTarget)
	fmt.Fprintf(&b, "fan_controller_class_target_celsius{class=\"ssd\"} %d\n", s.SSDTarget)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_class_emergency_celsius Per-class emergency threshold — instant fans=100%%.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_class_emergency_celsius gauge\n")
	fmt.Fprintf(&b, "fan_controller_class_emergency_celsius{class=\"cpu\"} %d\n", s.CPUEmergency)
	fmt.Fprintf(&b, "fan_controller_class_emergency_celsius{class=\"passive_gpu\"} %d\n", s.PassiveGPUEmergency)
	fmt.Fprintf(&b, "fan_controller_class_emergency_celsius{class=\"active_gpu\"} %d\n", s.ActiveGPUEmergency)
	fmt.Fprintf(&b, "fan_controller_class_emergency_celsius{class=\"hdd\"} %d\n", s.HDDEmergency)
	fmt.Fprintf(&b, "fan_controller_class_emergency_celsius{class=\"ssd\"} %d\n", s.SSDEmergency)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_class_candidate_percent Per-class PID candidate fan speed. max() across all classes drives fans.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_class_candidate_percent gauge\n")
	fmt.Fprintf(&b, "fan_controller_class_candidate_percent{class=\"cpu\"} %d\n", s.CPUCand)
	fmt.Fprintf(&b, "fan_controller_class_candidate_percent{class=\"passive_gpu\"} %d\n", s.PGCand)
	fmt.Fprintf(&b, "fan_controller_class_candidate_percent{class=\"hdd\"} %d\n", s.HDDCand)
	fmt.Fprintf(&b, "fan_controller_class_candidate_percent{class=\"ssd\"} %d\n", s.SSDCand)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_class_proximity_floor_percent Per-class proximity-to-emergency floor (silent until temp enters approach window).\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_class_proximity_floor_percent gauge\n")
	fmt.Fprintf(&b, "fan_controller_class_proximity_floor_percent{class=\"cpu\"} %d\n", s.CPUPF)
	fmt.Fprintf(&b, "fan_controller_class_proximity_floor_percent{class=\"passive_gpu\"} %d\n", s.PGPF)
	fmt.Fprintf(&b, "fan_controller_class_proximity_floor_percent{class=\"active_gpu\"} %d\n", s.AGPF)
	fmt.Fprintf(&b, "fan_controller_class_proximity_floor_percent{class=\"hdd\"} %d\n", s.HDDPF)
	fmt.Fprintf(&b, "fan_controller_class_proximity_floor_percent{class=\"ssd\"} %d\n", s.SSDPF)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_active_gpu_assist_percent Active-GPU intake-air assist contribution to chassis floor (driven by own-fan saturation).\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_active_gpu_assist_percent gauge\n")
	fmt.Fprintf(&b, "fan_controller_active_gpu_assist_percent %d\n", s.AGAssist)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_active_gpu_own_fan_percent Max own-fan speed (%%) across active GPUs this cycle. Compare to fan_controller_active_gpu_own_fan_threshold to see how much of the card's own self-cooling headroom remains.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_active_gpu_own_fan_percent gauge\n")
	fmt.Fprintf(&b, "fan_controller_active_gpu_own_fan_percent %d\n", s.ActiveGPUFanMax)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_active_gpu_own_fan_threshold Own-fan %% at which chassis assist begins. Below this the chassis stays quiet — the card self-cools.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_active_gpu_own_fan_threshold gauge\n")
	fmt.Fprintf(&b, "fan_controller_active_gpu_own_fan_threshold %d\n", s.ActiveGPUOwnFanThreshold)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "# HELP fan_controller_binding_source_info Which source bound the fan decision this cycle (max-wins). 1 for the active source.\n")
	fmt.Fprintf(&b, "# TYPE fan_controller_binding_source_info gauge\n")
	fmt.Fprintf(&b, "fan_controller_binding_source_info{source=\"%s\"} 1\n", escapeLabelValue(src))
	return b.Bytes()
}

// escapeLabelValue escapes a string for use inside a Prometheus
// text-format label value. Per the exposition format, backslash, double
// quote, and newline must be escaped — an unescaped quote or backslash
// makes node-exporter's textfile collector reject the ENTIRE file,
// silently dropping every metric in it. Current Source values are all
// safe literals; this guards against any future source string.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// WriteAtomic writes the rendered snapshot to path. Uses temp-file-
// then-rename so a scraper never sees a torn file.
//
// Errors here are returned but the caller (the main loop) should
// tolerate them — bash uses `|| true` so a metrics write failure
// never stops fan control.
func WriteAtomic(path string, snap Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, Render(snap), 0o644); err != nil {
		return err
	}
	// Clean up the temp file if the rename fails (e.g. cross-device); on
	// success the rename consumes it and this Remove is a no-op.
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}
