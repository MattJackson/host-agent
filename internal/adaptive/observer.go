// Package adaptive implements the v2 intent-driven adaptive controller
// — observer (this file), state, and reconciler. See
// docs/adaptive-controller-v2.md.
package adaptive

import (
	"math"
	"sync"
	"time"

	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/mode"
)

// Sample is one snapshot of a thermal class's state taken once per PID
// cycle (15s by default). The observer maintains a rolling window of
// these per class.
type Sample struct {
	Timestamp    time.Time
	TempCelsius  float64 // class-aggregated (mean of all members of this class on this host)
	FanDemandPct int     // controller's commanded fan %
	InletCelsius float64 // ambient inlet temp (from chassis sensor)
}

// Observer maintains per-class rolling windows of Samples and computes
// statistics for the reconciler. Safe for concurrent use: the PID loop
// calls Add (write) every 15s while the reconciler goroutine calls
// Stats (read) every 10 min.
type Observer struct {
	mu            sync.Mutex
	windowSize    int                        // max samples retained per class
	inletJumpC    float64                    // °C: an inlet jump > this triggers window reset for all classes
	minSafe       map[envelope.Class]float64 // lower-bound sanity (below = discard)
	maxSane       map[envelope.Class]float64 // upper-bound sanity (above = discard)
	buffers       map[envelope.Class][]Sample
	lastInlet     float64
	haveLastInlet bool
}

// NewObserver builds a fresh observer. windowSize is the max number of
// samples held per class (chosen by caller from
// OBSERVER_WINDOW_MINUTES * 60 / PID_INTERVAL_SECONDS, default 480 for
// 120 min @ 15s).
//
// Sample-validity bounds for each class come from envelope.DefaultEnvelopes:
//
//	minSafe = env.MinSafe
//	maxSane = 2 * env.Emergency   (anything above is treated as sensor fault)
//
// inletJumpC is the magnitude of a single-sample inlet temp change that
// is treated as an environmental shock; passing one triggers a window
// reset for every class. Default 10.
func NewObserver(windowSize int, inletJumpC float64) *Observer {
	if windowSize <= 0 {
		windowSize = 480
	}
	if inletJumpC <= 0 {
		inletJumpC = 10
	}
	minSafe := map[envelope.Class]float64{}
	maxSane := map[envelope.Class]float64{}
	for c, env := range envelope.DefaultEnvelopes {
		minSafe[c] = float64(env.MinSafe)
		maxSane[c] = 2 * float64(env.Emergency)
	}
	return &Observer{
		windowSize:    windowSize,
		inletJumpC:    inletJumpC,
		minSafe:       minSafe,
		maxSane:       maxSane,
		buffers:       map[envelope.Class][]Sample{},
		lastInlet:     0,
		haveLastInlet: false,
	}
}

// Add appends a sample to the named class's ring buffer. NaN
// temperatures and values outside [MinSafe, 2*Emergency] are discarded
// (returns false). A sudden inlet-temp jump > inletJumpC resets every
// class's buffer before recording the sample.
//
// Returns (accepted, resetTriggered).
//
// Concurrency: this method takes Observer's mutex.
func (o *Observer) Add(class envelope.Class, s Sample) (accepted, reset bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Sensor sanity check: bail on NaN or out-of-envelope readings.
	if math.IsNaN(s.TempCelsius) {
		return false, false
	}
	if lo, ok := o.minSafe[class]; ok && s.TempCelsius < lo {
		return false, false
	}
	if hi, ok := o.maxSane[class]; ok && s.TempCelsius > hi {
		return false, false
	}

	// Inlet-jump reset (applies across ALL classes including this one).
	if o.haveLastInlet && math.Abs(s.InletCelsius-o.lastInlet) > o.inletJumpC {
		for c := range o.buffers {
			o.buffers[c] = o.buffers[c][:0]
		}
		reset = true
	}
	o.lastInlet = s.InletCelsius
	o.haveLastInlet = true

	// Append to ring buffer, evicting oldest if full.
	buf := o.buffers[class]
	if len(buf) >= o.windowSize {
		buf = buf[1:]
	}
	buf = append(buf, s)
	o.buffers[class] = buf
	return true, reset
}

// Stats computes the WindowStats for the named class from the current
// buffer contents. Returns the zero WindowStats with Samples=0 if the
// class has no buffer yet. Computes:
//   - TempMean   = arithmetic mean
//   - TempStdDev = POPULATION stddev (divide by N, not N-1)
//   - TempP10/50/90 = nearest-rank percentile (sort, take index round(p/100 * (N-1)))
//   - FanDemandMean = arithmetic mean of fan-demand percents
//   - FanChangeRate = count of adjacent-sample changes >=1 in fan-demand, divided by window duration in minutes
//   - InletMean/StdDev = same conventions as Temp
//   - Samples = current buffer length
//
// Concurrency: this method takes Observer's mutex.
func (o *Observer) Stats(class envelope.Class) mode.WindowStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.computeStats(class)
}

// computeStats is the unexported worker — the caller is responsible for holding
// the mutex (or being in a single-goroutine context). It contains the math.
func (o *Observer) computeStats(class envelope.Class) mode.WindowStats {
	buf := o.buffers[class]
	n := len(buf)
	if n == 0 {
		return mode.WindowStats{}
	}

	// Compute temp mean
	tempSum := 0.0
	for _, s := range buf {
		tempSum += s.TempCelsius
	}
	tempMean := tempSum / float64(n)

	// Compute temp population stddev
	varianceSum := 0.0
	for _, s := range buf {
		diff := s.TempCelsius - tempMean
		varianceSum += diff * diff
	}
	tempStdDev := math.Sqrt(varianceSum / float64(n))

	// Compute percentiles using nearest-rank: idx = round(p/100 * (N-1))
	sortedTemps := make([]float64, n)
	for i, s := range buf {
		sortedTemps[i] = s.TempCelsius
	}
	sortFloats(sortedTemps)

	tempP10 := sortedTemps[percentileIndex(10, n)]
	tempP50 := sortedTemps[percentileIndex(50, n)]
	tempP90 := sortedTemps[percentileIndex(90, n)]

	// Fan demand mean
	fanSum := 0
	for _, s := range buf {
		fanSum += s.FanDemandPct
	}
	fanMean := float64(fanSum) / float64(n)

	// Fan change rate: count adjacent changes >=1, divide by duration in minutes
	var changes int
	for i := 1; i < n; i++ {
		diff := buf[i].FanDemandPct - buf[i-1].FanDemandPct
		if diff < 0 {
			diff = -diff
		}
		if diff >= 1 {
			changes++
		}
	}

	duration := buf[n-1].Timestamp.Sub(buf[0].Timestamp)
	var fanChangeRate float64
	if duration.Seconds() > 0 {
		fanChangeRate = float64(changes) / duration.Minutes()
	} else {
		fanChangeRate = 0
	}

	// Inlet mean/stddev
	inletSum := 0.0
	for _, s := range buf {
		inletSum += s.InletCelsius
	}
	inletMean := inletSum / float64(n)

	inletVarSum := 0.0
	for _, s := range buf {
		diff := s.InletCelsius - inletMean
		inletVarSum += diff * diff
	}
	inletStdDev := math.Sqrt(inletVarSum / float64(n))

	return mode.WindowStats{
		TempMean:      tempMean,
		TempStdDev:    tempStdDev,
		TempP10:       tempP10,
		TempP50:       tempP50,
		TempP90:       tempP90,
		FanDemandMean: fanMean,
		FanChangeRate: fanChangeRate,
		InletMean:     inletMean,
		InletStdDev:   inletStdDev,
		Samples:       n,
	}
}

// percentileIndex returns the nearest-rank index for percentile p given N samples.
// idx = round(p/100 * (N-1)), clamped to [0, N-1].
func percentileIndex(p int, n int) int {
	idx := int(math.Round(float64(p) / 100.0 * float64(n-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

// sortFloats sorts a slice of floats in ascending order (insertion sort).
func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j] > key {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}

// Reset clears the buffer for a single class. Used by the reconciler
// when mode changes or other intent-layer events require throwing away
// observed history.
func (o *Observer) Reset(class envelope.Class) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buffers[class] = nil
}
