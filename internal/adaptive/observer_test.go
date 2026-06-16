package adaptive

import (
	"math"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
)

func TestObserver_Add_AcceptsValid(t *testing.T) {
	o := NewObserver(480, 10)
	sample := Sample{
		Timestamp:    time.Now(),
		TempCelsius:  72.5,
		FanDemandPct: 60,
		InletCelsius: 22.0,
	}

	accepted, reset := o.Add(envelope.PassiveGPU, sample)
	if !accepted {
		t.Fatalf("expected valid sample to be accepted")
	}
	if reset {
		t.Errorf("unexpected reset on first sample")
	}

	stats := o.Stats(envelope.PassiveGPU)
	if stats.Samples != 1 {
		t.Errorf("expected Samples=1, got %d", stats.Samples)
	}
	if math.Abs(stats.TempMean-72.5) > 0.0001 {
		t.Errorf("expected TempMean=72.5, got %f", stats.TempMean)
	}
}

func TestObserver_Add_DiscardsNaN(t *testing.T) {
	o := NewObserver(480, 10)
	sample := Sample{
		Timestamp:    time.Now(),
		TempCelsius:  math.NaN(),
		FanDemandPct: 60,
		InletCelsius: 22.0,
	}

	accepted, reset := o.Add(envelope.PassiveGPU, sample)
	if accepted {
		t.Errorf("expected NaN sample to be rejected")
	}
	if reset {
		t.Error("unexpected reset on rejected sample")
	}

	stats := o.Stats(envelope.PassiveGPU)
	if stats.Samples != 0 {
		t.Errorf("expected Samples=0 after rejection, got %d", stats.Samples)
	}
}

func TestObserver_Add_DiscardsBelowMinSafe(t *testing.T) {
	o := NewObserver(480, 10)
	sample := Sample{
		Timestamp:    time.Now(),
		TempCelsius:  5.0, // HDD MinSafe=10
		FanDemandPct: 60,
		InletCelsius: 22.0,
	}

	accepted, reset := o.Add(envelope.HDD, sample)
	if accepted {
		t.Errorf("expected temp below MinSafe to be rejected")
	}
	if reset {
		t.Error("unexpected reset on rejected sample")
	}

	stats := o.Stats(envelope.HDD)
	if stats.Samples != 0 {
		t.Errorf("expected Samples=0 after rejection, got %d", stats.Samples)
	}
}

func TestObserver_Add_DiscardsAboveMaxSane(t *testing.T) {
	o := NewObserver(480, 10)
	sample := Sample{
		Timestamp:    time.Now(),
		TempCelsius:  200.0, // PassiveGPU 2*Emergency=180
		FanDemandPct: 60,
		InletCelsius: 22.0,
	}

	accepted, reset := o.Add(envelope.PassiveGPU, sample)
	if accepted {
		t.Errorf("expected temp above MaxSane to be rejected")
	}
	if reset {
		t.Error("unexpected reset on rejected sample")
	}

	stats := o.Stats(envelope.PassiveGPU)
	if stats.Samples != 0 {
		t.Errorf("expected Samples=0 after rejection, got %d", stats.Samples)
	}
}

func TestObserver_Add_RingBufferEvicts(t *testing.T) {
	o := NewObserver(3, 10)
	now := time.Now()

	for i := 1; i <= 5; i++ {
		sample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  float64(70 + i), // temps: 71, 72, 73, 74, 75
			FanDemandPct: 50,
			InletCelsius: 22.0,
		}
		accepted, _ := o.Add(envelope.PassiveGPU, sample)
		if !accepted {
			t.Fatalf("expected valid sample %d to be accepted", i)
		}
	}

	stats := o.Stats(envelope.PassiveGPU)
	if stats.Samples != 3 {
		t.Errorf("expected Samples=3 after evicting oldest, got %d", stats.Samples)
	}
	// Ring buffer with windowSize=3: after 5 adds, last three remain [73, 74, 75]
	expectedMean := (73.0 + 74.0 + 75.0) / 3.0
	if math.Abs(stats.TempMean-expectedMean) > 0.0001 {
		t.Errorf("expected TempMean=%f (most recent three), got %f", expectedMean, stats.TempMean)
	}
}

func TestObserver_Add_InletJumpResetsAllClasses(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	// Add several samples at inlet=20 for CPU, GPU, HDD
	for i := 1; i <= 5; i++ {
		cpuSample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  float64(60 + i),
			FanDemandPct: 50,
			InletCelsius: 20.0,
		}
		gpuSample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  float64(70 + i),
			FanDemandPct: 55,
			InletCelsius: 20.0,
		}
		hddSample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  float64(35 + i),
			FanDemandPct: 40,
			InletCelsius: 20.0,
		}

		o.Add(envelope.CPU, cpuSample)
		o.Add(envelope.PassiveGPU, gpuSample)
		o.Add(envelope.HDD, hddSample)
	}

	// Verify all buffers have 5 samples before reset
	cpuStats := o.Stats(envelope.CPU)
	gpuStats := o.Stats(envelope.PassiveGPU)
	hddStats := o.Stats(envelope.HDD)

	if cpuStats.Samples != 5 {
		t.Errorf("expected CPU Samples=5 before reset, got %d", cpuStats.Samples)
	}
	if gpuStats.Samples != 5 {
		t.Errorf("expected GPU Samples=5 before reset, got %d", gpuStats.Samples)
	}
	if hddStats.Samples != 5 {
		t.Errorf("expected HDD Samples=5 before reset, got %d", hddStats.Samples)
	}

	// Add one CPU sample at inlet=35 (jump of 15 > threshold 10)
	cpuSampleAfterJump := Sample{
		Timestamp:    now.Add(6 * time.Second),
		TempCelsius:  70.0,
		FanDemandPct: 52,
		InletCelsius: 35.0,
	}

	accepted, reset := o.Add(envelope.CPU, cpuSampleAfterJump)
	if !accepted {
		t.Errorf("expected valid CPU sample to be accepted after inlet jump")
	}
	if !reset {
		t.Error("expected reset=true on inlet jump > threshold")
	}

	// All buffers should now be empty before the new sample is recorded
	cpuStats = o.Stats(envelope.CPU)
	gpuStats = o.Stats(envelope.PassiveGPU)
	hddStats = o.Stats(envelope.HDD)

	if cpuStats.Samples != 1 {
		t.Errorf("expected CPU Samples=1 (post-reset only), got %d", cpuStats.Samples)
	}
	if gpuStats.Samples != 0 {
		t.Errorf("expected GPU Samples=0 after reset, got %d", gpuStats.Samples)
	}
	if hddStats.Samples != 0 {
		t.Errorf("expected HDD Samples=0 after reset, got %d", hddStats.Samples)
	}
}

func TestObserver_Stats_EmptyClass(t *testing.T) {
	o := NewObserver(480, 10)

	stats := o.Stats(envelope.CPU)

	if stats.Samples != 0 {
		t.Errorf("expected Samples=0 for empty class, got %d", stats.Samples)
	}
	if stats.TempMean != 0 {
		t.Errorf("expected TempMean=0 for empty class, got %f", stats.TempMean)
	}
	if stats.FanChangeRate != 0 {
		t.Errorf("expected FanChangeRate=0 for empty class, got %f", stats.FanChangeRate)
	}
}

func TestObserver_Stats_TempMeanAndStdDev(t *testing.T) {
	tests := []struct {
		name           string
		temps          []float64
		expectedMean   float64
		expectedStdDev float64
	}{
		{
			name:           "three samples",
			temps:          []float64{70.0, 72.0, 74.0},
			expectedMean:   72.0,
			expectedStdDev: math.Sqrt((4 + 0 + 4) / 3.0), // population stddev (N=3)
		},
		{
			name:           "single sample",
			temps:          []float64{75.0},
			expectedMean:   75.0,
			expectedStdDev: 0.0,
		},
		{
			name:           "two identical samples",
			temps:          []float64{80.0, 80.0},
			expectedMean:   80.0,
			expectedStdDev: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewObserver(480, 10)
			now := time.Now()

			for i, temp := range tt.temps {
				sample := Sample{
					Timestamp:    now.Add(time.Duration(i) * time.Second),
					TempCelsius:  temp,
					FanDemandPct: 50,
					InletCelsius: 22.0,
				}
				o.Add(envelope.CPU, sample)
			}

			stats := o.Stats(envelope.CPU)

			if stats.Samples != len(tt.temps) {
				t.Errorf("expected Samples=%d, got %d", len(tt.temps), stats.Samples)
			}

			if math.Abs(stats.TempMean-tt.expectedMean) > 0.0001 {
				t.Errorf("expected TempMean=%f, got %f", tt.expectedMean, stats.TempMean)
			}

			if math.Abs(stats.TempStdDev-tt.expectedStdDev) > 0.0001 {
				t.Errorf("expected TempStdDev=%f (population), got %f", tt.expectedStdDev, stats.TempStdDev)
			}
		})
	}
}

func TestObserver_Stats_Percentiles_NearestRank(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	// Create 10 samples with temps 1..10 (scaled up to realistic values: 70..79)
	for i := 1; i <= 10; i++ {
		sample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  float64(70 + i), // temps: 71, 72, ..., 80
			FanDemandPct: 50,
			InletCelsius: 22.0,
		}
		o.Add(envelope.CPU, sample)
	}

	stats := o.Stats(envelope.CPU)

	// Nearest-rank formula: idx = round(p/100 * (N-1))
	// N=10, so N-1=9
	// P10: idx = round(0.10 * 9) = round(0.9) = 1 -> sorted[1] = 72
	// P50: idx = round(0.50 * 9) = round(4.5) = 5 (round-half-up) -> sorted[5] = 76
	// P90: idx = round(0.90 * 9) = round(8.1) = 8 -> sorted[8] = 79

	if stats.TempP10 != 72 {
		t.Errorf("expected TempP10=72, got %f", stats.TempP10)
	}
	if stats.TempP50 != 76 {
		t.Errorf("expected TempP50=76, got %f", stats.TempP50)
	}
	if stats.TempP90 != 79 {
		t.Errorf("expected TempP90=79, got %f", stats.TempP90)
	}
}

func TestObserver_Stats_FanDemandP90_NearestRank(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	// Fan demands 41..50 (added out of order to prove sorting). N=10, N-1=9.
	// P90: idx = round(0.90 * 9) = round(8.1) = 8 -> sorted[8] = 49
	fans := []int{50, 41, 49, 42, 48, 43, 47, 44, 46, 45}
	for i, f := range fans {
		o.Add(envelope.CPU, Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  72.0,
			FanDemandPct: f,
			InletCelsius: 22.0,
		})
	}

	stats := o.Stats(envelope.CPU)
	if stats.FanDemandP90 != 49 {
		t.Errorf("expected FanDemandP90=49, got %f", stats.FanDemandP90)
	}
}

// TestObserver_Stats_FanDemandP90_IgnoresTransientDip is the observer-level
// half of the v0.3.9 limit-cycle fix: a window pinned at MaxFan with a
// minority of dipped samples reports a depressed mean but a p90 still at
// MaxFan — the saturation signal the reconciler must trust.
func TestObserver_Stats_FanDemandP90_IgnoresTransientDip(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	// 4 dipped (24%) + 6 pinned (100%): mean = 69.6, p90 = 100.
	fans := []int{24, 100, 24, 100, 24, 100, 24, 100, 100, 100}
	for i, f := range fans {
		o.Add(envelope.PassiveGPU, Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  81.0,
			FanDemandPct: f,
			InletCelsius: 22.0,
		})
	}

	stats := o.Stats(envelope.PassiveGPU)
	if stats.FanDemandMean >= 90 {
		t.Errorf("expected FanDemandMean<90 (dip-depressed), got %f", stats.FanDemandMean)
	}
	if stats.FanDemandP90 != 100 {
		t.Errorf("expected FanDemandP90=100 (fan genuinely pinned), got %f", stats.FanDemandP90)
	}
}

func TestObserver_Stats_FanChangeRate(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	// 5 samples 60s apart with fan demands [50, 50, 51, 51, 53]
	fanDemands := []int{50, 50, 51, 51, 53}
	for i, fd := range fanDemands {
		sample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Minute), // 60s apart
			TempCelsius:  72.0,
			FanDemandPct: fd,
			InletCelsius: 22.0,
		}
		o.Add(envelope.CPU, sample)
	}

	stats := o.Stats(envelope.CPU)

	// Duration = 4 minutes (from t=0 to t=4)
	// Changes: pairs (50→50)=0, (50→51)=1, (51→51)=0, (51→53)=1 → 2 changes
	// FanChangeRate = 2 / 4 = 0.5
	expectedFanChangeRate := 0.5

	if math.Abs(stats.FanChangeRate-expectedFanChangeRate) > 0.0001 {
		t.Errorf("expected FanChangeRate=%f, got %f", expectedFanChangeRate, stats.FanChangeRate)
	}
}

func TestObserver_Reset(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	for i := 1; i <= 5; i++ {
		sample := Sample{
			Timestamp:    now.Add(time.Duration(i) * time.Second),
			TempCelsius:  float64(70 + i),
			FanDemandPct: 50,
			InletCelsius: 22.0,
		}
		o.Add(envelope.CPU, sample)
	}

	cpuStats := o.Stats(envelope.CPU)
	if cpuStats.Samples != 5 {
		t.Errorf("expected Samples=5 before reset, got %d", cpuStats.Samples)
	}

	o.Reset(envelope.CPU)

	cpuStats = o.Stats(envelope.CPU)
	if cpuStats.Samples != 0 {
		t.Errorf("expected Samples=0 after Reset, got %d", cpuStats.Samples)
	}
}

func TestObserver_Concurrent_AddAndStats(t *testing.T) {
	o := NewObserver(480, 10)
	now := time.Now()

	done := make(chan bool)

	// Writer goroutine: Add 100 iterations
	go func() {
		for i := 0; i < 100; i++ {
			sample := Sample{
				Timestamp:    now.Add(time.Duration(i) * time.Second),
				TempCelsius:  float64(70 + (i % 20)),
				FanDemandPct: 50 + (i % 10),
				InletCelsius: 22.0,
			}
			o.Add(envelope.CPU, sample)
		}
		done <- true
	}()

	// Reader goroutine: Stats 100 iterations
	go func() {
		for i := 0; i < 100; i++ {
			stats := o.Stats(envelope.CPU)
			if stats.Samples < 0 {
				t.Errorf("unexpected negative Samples value")
			}
		}
		done <- true
	}()

	// Wait for both to complete
	<-done
	<-done

	// If we get here without panics, test passes
}
