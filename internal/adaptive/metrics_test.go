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
