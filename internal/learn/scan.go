package learn

// scan.go — first-run box scan: turn a handful of (fan%, settled-temp)
// observations into the curve ramp-start that holds TARGET, so a never-seen box
// starts near-optimal instead of trimming ≤1°C/10min up from a blind default.
// The continuous learner (TargetSeek) then maintains it for drift.

// ScanPoint is one settled observation: the steady-state temperature a class
// reached while the chassis fan was held at FanPct.
type ScanPoint struct {
	FanPct int
	TempC  float64
}

// FitComfort fits a line through the scan points (temp = a + b·fan; b<0 since
// more fan ⇒ cooler), finds the fan that would hold TARGET, and returns the
// curve comfort (ramp-start) that places control.Curve so its output AT target
// equals that fan — i.e. the curve will hold the plant at target.
//
// Falls back to `target - fallbackMargin` (clamped) when the scan is
// uninformative: <2 distinct fan levels, or no usable cooling relationship
// (b >= 0, e.g. an idle class whose temp never moved with fan). The fallback is
// safe — the continuous learner trims from there.
func FitComfort(points []ScanPoint, target, emergency, minFan, maxFan, floorComfort, fallbackMargin int) int {
	hiComfort := emergency - 1
	fallback := clampInt(target-fallbackMargin, floorComfort, hiComfort)

	// Need at least two distinct fan levels to fit a slope.
	if !twoDistinctFans(points) {
		return fallback
	}

	a, b, ok := linfit(points)
	if !ok || b >= -1e-6 {
		// No (or wrong-sign) cooling relationship — can't place from this scan.
		return fallback
	}

	// Fan that yields temp == target on the fitted line.
	fStar := (float64(target) - a) / b
	if fStar < float64(minFan) {
		fStar = float64(minFan)
	}
	if fStar > float64(maxFan) {
		fStar = float64(maxFan)
	}

	span := float64(maxFan - minFan)
	if span <= 0 {
		return fallback
	}
	k := (fStar - float64(minFan)) / span // fraction of fan range needed at target
	if k >= 0.999 {
		// Needs ~max fan to hold target → ramp earliest we allow.
		return floorComfort
	}

	// Place the curve so fan(target) == fStar:
	//   fan(t) = minFan + (t-comfort)/(emergency-comfort) * span
	//   at t=target, fan=fStar  ⇒  k = (target-comfort)/(emergency-comfort)
	//   ⇒  comfort = (target - k*emergency) / (1 - k)
	comfort := (float64(target) - k*float64(emergency)) / (1.0 - k)
	ci := int(comfort + 0.5)
	return clampInt(ci, floorComfort, hiComfort)
}

func twoDistinctFans(pts []ScanPoint) bool {
	if len(pts) < 2 {
		return false
	}
	first := pts[0].FanPct
	for _, p := range pts[1:] {
		if p.FanPct != first {
			return true
		}
	}
	return false
}

// linfit returns ordinary-least-squares (a, b) for temp = a + b*fan.
func linfit(pts []ScanPoint) (a, b float64, ok bool) {
	n := float64(len(pts))
	if n < 2 {
		return 0, 0, false
	}
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		x, y := float64(p.FanPct), p.TempC
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0, 0, false
	}
	b = (n*sxy - sx*sy) / denom
	a = (sy - b*sx) / n
	return a, b, true
}
