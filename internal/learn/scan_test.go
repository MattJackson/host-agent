package learn

import (
	"testing"

	"github.com/pq/docker-server/host-agent/internal/control"
)

// clean linear plant: temp = 55 - 0.3*fan  →  (30,46) (50,40) (70,34).
func TestFitComfort_PlacesCurveToHoldTarget(t *testing.T) {
	pts := []ScanPoint{{30, 46}, {50, 40}, {70, 34}}
	// target 38, emergency 50, fan 10..100, floor 20, fallback margin 5.
	comfort := FitComfort(pts, 38, 50, 10, 100, 20, 5, 6)

	// Verify the resulting curve actually outputs ~the fan that holds 38.
	// Line: fan that yields 38 = (38-55)/-0.3 = 56.7. control.Curve(38, comfort,50,10,100)
	// must be ≈ 57.
	got := control.Curve(38, comfort, 50, 10, 100)
	if got < 54 || got > 60 {
		t.Fatalf("curve at target should hold ~57%% fan, got %d (comfort=%d)", got, comfort)
	}
}

func TestFitComfort_FallsBackOnSingleLevel(t *testing.T) {
	pts := []ScanPoint{{50, 40}, {50, 41}} // only one distinct fan level
	c := FitComfort(pts, 38, 50, 10, 100, 20, 5, 6)
	if c != 33 { // fallback = target - margin = 38 - 5
		t.Fatalf("single-level scan should fall back to target-margin=33, got %d", c)
	}
}

func TestFitComfort_FallsBackOnNoCooling(t *testing.T) {
	// Temp doesn't drop with fan (idle class) → positive/zero slope → fallback.
	pts := []ScanPoint{{30, 40}, {50, 40}, {70, 40}}
	c := FitComfort(pts, 38, 50, 10, 100, 20, 5, 6)
	if c != 33 {
		t.Fatalf("no cooling relationship should fall back to 33, got %d", c)
	}
}

func TestFitComfort_TargetNeedsMaxFan_RampsEarliest(t *testing.T) {
	// Very hot plant: temp = 70 - 0.1*fan → to hold 38 needs fan=320 → clamped
	// to maxFan, k≈1 → comfort floored.
	pts := []ScanPoint{{30, 67}, {60, 64}, {90, 61}}
	c := FitComfort(pts, 38, 50, 10, 100, 20, 5, 6)
	if c != 20 { // floorComfort
		t.Fatalf("unreachable target should ramp earliest (floor=20), got %d", c)
	}
}

func TestFitComfort_ClampsWithinEnvelope(t *testing.T) {
	pts := []ScanPoint{{30, 46}, {50, 40}, {70, 34}}
	c := FitComfort(pts, 38, 50, 10, 100, 20, 5, 6)
	if c < 20 || c > 49 {
		t.Fatalf("comfort must stay within [floor=20, emergency-1=49], got %d", c)
	}
}

// End-to-end: a fitted comfort fed back through the curve+plant model should
// settle at the target (closing the loop the scan is meant to close).
func TestFitComfort_ClosedLoopSettlesAtTarget(t *testing.T) {
	// plant: temp = 55 - 0.3*fan ; curve picks fan from temp ; find fixed point.
	comfort := FitComfort([]ScanPoint{{30, 46}, {50, 40}, {70, 34}}, 38, 50, 10, 100, 20, 5, 6)
	temp := 46.0
	for i := 0; i < 400; i++ {
		fan := control.Curve(int(temp+0.5), comfort, 50, 10, 100)
		// Thermal inertia (first-order lag) — the real plant doesn't jump to the
		// instantaneous equilibrium each step; that lag is what makes the
		// memoryless-curve loop stable. alpha=0.15.
		target := 55 - 0.3*float64(fan)
		temp += 0.15 * (target - temp)
	}
	if temp < 37 || temp > 39 {
		t.Fatalf("closed loop should settle ~38, got %.1f (comfort=%d)", temp, comfort)
	}
}

// TestFitComfort_HonoursMinSpan: the fitted (or fallback) comfort never exceeds
// emergency - minSpan.
//
// Idle-CPU plant: temp = 80 - 0.1*fan → (30,77) (55,74.5) (80,72). Target 76,
// emergency 80, fan 10..100:
//
//	fan holding 76 = (76-80)/-0.1 = 40 → k = (40-10)/90 = 1/3
//	comfort = (76 - 80/3) / (2/3) = 74
//
// So the unconstrained fit wants a 6°C ramp (74→80).
func TestFitComfort_HonoursMinSpan(t *testing.T) {
	idle := []ScanPoint{{30, 77}, {55, 74.5}, {80, 72}}
	single := []ScanPoint{{50, 75}} // uninformative → fallback target-5 = 71
	cases := []struct {
		name    string
		pts     []ScanPoint
		minSpan int
		want    int
	}{
		{"fit, span 0 treated as 1 (cap 79)", idle, 0, 74},
		{"fit, legacy span 1 (cap 79)", idle, 1, 74},
		{"fit, span 6 exactly fits (cap 74)", idle, 6, 74},
		{"fit, CPU span 10 clamps (cap 70)", idle, 10, 70},
		{"fallback 71, span 1 (cap 79)", single, 1, 71},
		{"fallback 71, span 10 clamps (cap 70)", single, 10, 70},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FitComfort(c.pts, 76, 80, 10, 100, 20, 5, c.minSpan); got != c.want {
				t.Errorf("FitComfort(span=%d) = %d, want %d", c.minSpan, got, c.want)
			}
		})
	}
}
