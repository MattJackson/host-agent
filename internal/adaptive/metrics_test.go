package adaptive

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
)

func TestRenderObserverMetrics_EmptyObserver(t *testing.T) {
	o := NewObserver(480, 10)
	output := RenderObserverMetrics(o)
	lines := bytes.Split(output, []byte("\n"))

	// Verify all 8 metric HELP/TYPE pairs are present
	helpCount := 0
	typeCount := 0
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("# HELP ")) {
			helpCount++
		}
		if bytes.HasPrefix(line, []byte("# TYPE ")) {
			typeCount++
		}
	}

	if helpCount != 8 {
		t.Errorf("expected 8 HELP lines, got %d", helpCount)
	}
	if typeCount != 8 {
		t.Errorf("expected 8 TYPE lines, got %d", typeCount)
	}

	// Verify every class label appears for every metric (4 classes × 8 metrics = 32 series lines minimum)
	classMetricCount := 0
	for _, line := range lines {
		if bytes.Contains(line, []byte("adaptive_window_samples_filled{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_mean{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_stddev{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_p10{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_p50{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_p90{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_fan_change_rate{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_inlet_mean{class=")) {
			classMetricCount++
		}
	}

	if classMetricCount != 32 {
		t.Errorf("expected 32 metric series lines (4 classes × 8 metrics), got %d", classMetricCount)
	}

	// Verify all sample counts are 0 and float values render as 0.0000
	for _, line := range lines {
		if bytes.Contains(line, []byte("adaptive_window_samples_filled{class=")) && !bytes.HasSuffix(bytes.TrimRight(line, "\r"), []byte("} 0")) {
			t.Errorf("expected sample count 0 for %s", string(line))
		}
		if bytes.Contains(line, []byte("adaptive_window_temp_mean{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_stddev{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_p10{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_p50{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_temp_p90{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_fan_change_rate{class=")) ||
			bytes.Contains(line, []byte("adaptive_window_inlet_mean{class=")) {
			if !bytes.Contains(line, []byte(" 0.0000")) && !bytes.HasSuffix(bytes.TrimRight(line, "\r"), []byte("0.0000}")) {
				t.Errorf("expected float value 0.0000 for %s", string(line))
			}
		}
	}
}

func TestRenderObserverMetrics_WithData(t *testing.T) {
	o := NewObserver(480, 10)

	// Add 3 known samples to PassiveGPU class with temps 70.0, 72.0, 74.0
	now := time.Now()
	for _, temp := range []float64{70.0, 72.0, 74.0} {
		o.Add(envelope.PassiveGPU, Sample{
			Timestamp:    now,
			TempCelsius:  temp,
			FanDemandPct: 50,
			InletCelsius: 22.0,
		})
		now = now.Add(15 * time.Second)
	}

	output := RenderObserverMetrics(o)
	outputStr := string(output)

	// Verify samples_filled for passive_gpu is 3
	if !bytes.Contains([]byte(outputStr), []byte("adaptive_window_samples_filled{class=\"passive_gpu\"} 3")) {
		t.Errorf("expected adaptive_window_samples_filled{class=\"passive_gpu\"} 3, not found in output")
	}

	// Verify temp_mean for passive_gpu is 72.0
	if !bytes.Contains([]byte(outputStr), []byte("adaptive_window_temp_mean{class=\"passive_gpu\"} 72.0000")) {
		t.Errorf("expected adaptive_window_temp_mean{class=\"passive_gpu\"} 72.0000, not found in output")
	}

	// Verify CPU/HDD/SSD lines still appear with 0 samples
	for _, class := range []envelope.Class{envelope.CPU, envelope.HDD, envelope.SSD} {
		classStr := string(class)
		if !bytes.Contains([]byte(outputStr), []byte("adaptive_window_samples_filled{class=\""+classStr+"\"} 0")) {
			t.Errorf("expected %s to have 0 samples", classStr)
		}
	}
}

func TestWriteObserverMetrics_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "adaptive.prom")

	o := NewObserver(480, 10)

	err := WriteObserverMetrics(testPath, o)
	if err != nil {
		t.Fatalf("WriteObserverMetrics failed: %v", err)
	}

	// Verify the file exists
	if _, err := os.Stat(testPath); os.IsNotExist(err) {
		t.Fatal("output file was not created")
	}

	// Verify content matches RenderObserverMetrics
	rendered := RenderObserverMetrics(o)
	content, err := os.ReadFile(testPath)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}
	if !bytes.Equal(rendered, content) {
		t.Error("file content does not match RenderObserverMetrics output")
	}

	// Verify no .tmp file is left behind
	tmpPath := testPath + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatal(".tmp file was not cleaned up")
	}
}

func TestRenderObserverMetrics_ClassOrder(t *testing.T) {
	o := NewObserver(480, 10)
	output := RenderObserverMetrics(o)
	lines := bytes.Split(output, []byte("\n"))

	// Find the first occurrence of each metric for class order verification
	expectedOrder := []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD}
	metricNames := []string{
		"adaptive_window_samples_filled",
		"adaptive_window_temp_mean",
		"adaptive_window_temp_stddev",
		"adaptive_window_temp_p10",
		"adaptive_window_temp_p50",
		"adaptive_window_temp_p90",
		"adaptive_window_fan_change_rate",
		"adaptive_window_inlet_mean",
	}

	for _, metricName := range metricNames {
		var foundClasses []envelope.Class
		for _, line := range lines {
			if bytes.Contains(line, []byte(metricName+"{class=")) && !bytes.HasPrefix(line, []byte("#")) {
				for _, c := range expectedOrder {
					if bytes.Contains(line, []byte("class=\""+string(c)+"\"")) {
						foundClasses = append(foundClasses, c)
						break
					}
				}
			}
		}

		if len(foundClasses) != 4 {
			t.Errorf("expected 4 class entries for %s, found %d", metricName, len(foundClasses))
			continue
		}

		for i, expected := range expectedOrder {
			if foundClasses[i] != expected {
				t.Errorf("for metric %s: expected order %v, got %v (position %d was %q instead of %q)",
					metricName, expectedOrder, foundClasses, i, foundClasses[i], expected)
			}
		}
	}
}

func TestRenderReconcilerMetrics_StableShape(t *testing.T) {
	o := NewObserver(480, 10)
	r, err := NewReconciler(ReconcilerOptions{
		Observer:  o,
		Mode:      "balanced",
		StatePath: "",
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	output := RenderReconcilerMetrics(r)
	lines := bytes.Split(output, []byte("\n"))

	requiredSeries := map[string]bool{
		"adaptive_mode_info":               false,
		"adaptive_target_celsius":          false,
		"adaptive_deadband_celsius":        false,
		"adaptive_envelope_preferred_low":  false,
		"adaptive_envelope_preferred_high": false,
		"adaptive_envelope_max_safe":       false,
		"adaptive_target_drifts_total":     false,
		"adaptive_target_resets_total":     false,
	}

	requiredClassLabels := []envelope.Class{envelope.CPU, envelope.PassiveGPU, envelope.HDD, envelope.SSD}

	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("# HELP ")) || bytes.HasPrefix(line, []byte("# TYPE ")) {
			continue
		}
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		for metricName := range requiredSeries {
			if bytes.Contains(line, []byte(metricName+"{")) {
				requiredSeries[metricName] = true
				break
			}
		}
	}

	// Note: drift and reset counters only appear when count > 0, so we skip them here
	delete(requiredSeries, "adaptive_target_drifts_total")
	delete(requiredSeries, "adaptive_target_resets_total")

	for name, found := range requiredSeries {
		if !found {
			t.Errorf("required series %s not found in output", name)
		}
	}

	for _, c := range requiredClassLabels {
		classStr := string(c)
		if !bytes.Contains(output, []byte("adaptive_target_celsius{class=\""+classStr+"\"}")) {
			t.Errorf("expected adaptive_target_celsius line for %s", classStr)
		}
		if !bytes.Contains(output, []byte("adaptive_deadband_celsius{class=\""+classStr+"\"}")) {
			t.Errorf("expected adaptive_deadband_celsius line for %s", classStr)
		}
	}

	helpCount := 0
	typeCount := 0
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("# HELP ")) {
			helpCount++
		}
		if bytes.HasPrefix(line, []byte("# TYPE ")) {
			typeCount++
		}
	}

	expectedPairs := 1 + 4*5
	if helpCount < expectedPairs {
		t.Errorf("expected at least %d HELP lines, got %d", expectedPairs, helpCount)
	}
}

func TestRenderReconcilerMetrics_DriftCountersTickOnUp(t *testing.T) {
	o := NewObserver(480, 10)

	now := time.Now()
	for i := 0; i < 50; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    now,
			TempCelsius:  30.0 + float64(i)*0.1,
			FanDemandPct: 20,
			InletCelsius: 20.0,
		})
		now = now.Add(15 * time.Second)
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:            o,
		Mode:                "balanced", // starts at 65°C for CPU, can drift up or down
		StatePath:           "",
		WindowSize:          480,
		VarianceResetStdDev: 5.0,
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	// Fill the rest of the window to pass warmup gate (need 480 samples total)
	// Use very low temps so balanced mode wants to drift up toward PreferredHigh
	for i := 50; i < 480; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    now,
			TempCelsius:  20.0, // very low - will want to drift UP in balanced mode
			FanDemandPct: 10,
			InletCelsius: 20.0,
		})
		now = now.Add(15 * time.Second)
	}

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	foundUpDrift := false
	for _, a := range actions {
		if a.Class == envelope.CPU && a.Reason == DriftReasonUp {
			foundUpDrift = true
			break
		}
	}

	output := RenderReconcilerMetrics(r)

	cpuUpLine := "adaptive_target_drifts_total{class=\"cpu\",direction=\"up\"} 1"
	if !bytes.Contains(output, []byte(cpuUpLine)) {
		t.Errorf("expected drift-up counter line %q not found; output:\n%s", cpuUpLine, string(output))
	}

	if !foundUpDrift {
		t.Logf("No drift-up detected for CPU class; actions: %+v", actions)
	}
}

func TestRenderReconcilerMetrics_ResetCountersTick(t *testing.T) {
	o := NewObserver(480, 10)

	now := time.Now()
	for i := 0; i < 480; i++ {
		var temp float64
		if i%2 == 0 {
			temp = 30.0 // low
		} else {
			temp = 50.0 // high - creates stddev > 5°C threshold
		}
		o.Add(envelope.CPU, Sample{
			Timestamp:    now,
			TempCelsius:  temp,
			FanDemandPct: 30,
			InletCelsius: 20.0,
		})
		now = now.Add(15 * time.Second)
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:            o,
		Mode:                "balanced",
		StatePath:           "",
		WindowSize:          480,
		VarianceResetStdDev: 5.0,
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	actions, err := r.Step()
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}

	foundReset := false
	for _, a := range actions {
		if a.Class == envelope.CPU && a.Reason == DriftReasonVarianceReset {
			foundReset = true
			break
		}
	}

	output := RenderReconcilerMetrics(r)

	cpuResetLine := "adaptive_target_resets_total{class=\"cpu\",reason=\"variance_reset\"} 1"
	if !bytes.Contains(output, []byte(cpuResetLine)) {
		t.Errorf("expected variance-reset counter line %q not found; output:\n%s", cpuResetLine, string(output))
	}

	if !foundReset {
		t.Logf("No variance reset detected for CPU class; actions: %+v", actions)
	}
}

func TestWriteAdaptiveMetrics_WithNilReconciler(t *testing.T) {
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "adaptive.prom")

	o := NewObserver(480, 10)

	now := time.Now()
	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    now,
			TempCelsius:  float64(70 + i),
			FanDemandPct: 50,
			InletCelsius: 22.0,
		})
		now = now.Add(15 * time.Second)
	}

	err := WriteAdaptiveMetrics(testPath, o, nil)
	if err != nil {
		t.Fatalf("WriteAdaptiveMetrics failed: %v", err)
	}

	content, err := os.ReadFile(testPath)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}

	if !bytes.Contains(content, []byte("adaptive_window_samples_filled")) {
		t.Error("expected observer series adaptive_window_samples_filled not found")
	}
	if !bytes.Contains(content, []byte("adaptive_window_temp_mean")) {
		t.Error("expected observer series adaptive_window_temp_mean not found")
	}

	if bytes.Contains(content, []byte("adaptive_target_celsius")) {
		t.Error("unexpected reconciler series adaptive_target_celsius found (should be nil)")
	}
	if bytes.Contains(content, []byte("adaptive_mode_info")) {
		t.Error("unexpected reconciler series adaptive_mode_info found (should be nil)")
	}

	tmpPath := testPath + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatal(".tmp file was not cleaned up")
	}
}

func TestWriteAdaptiveMetrics_FileAtomic(t *testing.T) {
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "adaptive.prom")

	o := NewObserver(480, 10)

	now := time.Now()
	for i := 0; i < 5; i++ {
		o.Add(envelope.CPU, Sample{
			Timestamp:    now,
			TempCelsius:  float64(70 + i),
			FanDemandPct: 50,
			InletCelsius: 22.0,
		})
		now = now.Add(15 * time.Second)
	}

	r, err := NewReconciler(ReconcilerOptions{
		Observer:            o,
		Mode:                "balanced",
		StatePath:           "",
		WindowSize:          480,
		VarianceResetStdDev: 5.0,
	})
	if err != nil {
		t.Fatalf("NewReconciler failed: %v", err)
	}

	err = WriteAdaptiveMetrics(testPath, o, r)
	if err != nil {
		t.Fatalf("WriteAdaptiveMetrics failed: %v", err)
	}

	content, err := os.ReadFile(testPath)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}

	if !bytes.Contains(content, []byte("adaptive_window_samples_filled")) {
		t.Error("expected observer metrics not found")
	}

	if !bytes.Contains(content, []byte("adaptive_target_celsius")) {
		t.Error("expected reconciler metrics adaptive_target_celsius not found")
	}
	if !bytes.Contains(content, []byte("adaptive_mode_info")) {
		t.Error("expected reconciler metrics adaptive_mode_info not found")
	}

	tmpPath := testPath + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatal(".tmp file was not cleaned up after successful write")
	}
}
