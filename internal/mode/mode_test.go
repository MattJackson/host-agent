package mode

import (
	"fmt"
	"testing"

	"github.com/pq/docker-server/host-agent/internal/envelope"
)

func TestMode_Valid(t *testing.T) {
	tests := []struct {
		name string
		m    Mode
		want bool
	}{
		{"MaxCool valid", MaxCool, true},
		{"Balanced valid", Balanced, true},
		{"MinNoise valid", MinNoise, true},
		{"Eco valid", Eco, true},
		{"invalid string", Mode("foo"), false},
		{"empty string", Mode(""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.m.Valid()
			if got != tt.want {
				t.Errorf("Mode(%q).Valid() = %v, want %v", tt.m, got, tt.want)
			}
		})
	}
}

func TestAll(t *testing.T) {
	want := []Mode{MaxCool, Balanced, MinNoise, Eco}
	got := All()
	if len(got) != len(want) {
		t.Fatalf("All() returned %d values, want %d", len(got), len(want))
	}
	for i, m := range got {
		if m != want[i] {
			t.Errorf("All()[%d] = %q, want %q", i, m, want[i])
		}
	}
}

func TestInitialTarget(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		expected map[envelope.Class][2]int // class -> [target, deadband]
	}{
		{
			name: "MaxCool",
			mode: MaxCool,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {55, 2},
				envelope.PassiveGPU: {75, 2},
				envelope.HDD:        {32, 2},
				envelope.SSD:        {45, 2},
			},
		},
		{
			name: "Balanced",
			mode: Balanced,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {65, 3},
				envelope.PassiveGPU: {80, 3},
				envelope.HDD:        {38, 3},
				envelope.SSD:        {50, 3},
			},
		},
		{
			name: "MinNoise",
			mode: MinNoise,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {75, 4},
				envelope.PassiveGPU: {83, 4},
				envelope.HDD:        {43, 4},
				envelope.SSD:        {60, 4},
			},
		},
		{
			name: "Eco",
			mode: Eco,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {75, 5},
				envelope.PassiveGPU: {83, 5},
				envelope.HDD:        {43, 5},
				envelope.SSD:        {60, 5},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for c, env := range envelope.DefaultEnvelopes {
				wantTarget, wantDeadband := tt.expected[c][0], tt.expected[c][1]
				gotTarget, gotDeadband := InitialTarget(env, tt.mode)
				if gotTarget != wantTarget {
					t.Errorf("class %s: target = %d, want %d", c, gotTarget, wantTarget)
				}
				if gotDeadband != wantDeadband {
					t.Errorf("class %s: deadband = %d, want %d", c, gotDeadband, wantDeadband)
				}
			}
		})
	}
}

func TestInitialTarget_InvalidMode(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.CPU]
	gotTarget, gotDeadband := InitialTarget(env, Mode("garbage"))
	wantTarget, wantDeadband := env.PreferredMid, 3 // Balanced fallback
	if gotTarget != wantTarget {
		t.Errorf("target = %d, want %d (Balanced fallback)", gotTarget, wantTarget)
	}
	if gotDeadband != wantDeadband {
		t.Errorf("deadband = %d, want %d (Balanced fallback)", gotDeadband, wantDeadband)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantM   Mode
		wantSet bool
		wantErr bool
	}{
		{"unset", "", Default, false, false},
		{"empty", "", Default, false, false},
		{"balanced", "balanced", Balanced, true, false},
		{"MAX-COOL (case)", "MAX-COOL", MaxCool, true, false},
		{"  min_noise  (whitespace + underscore)", "  min_noise  ", MinNoise, true, false},
		{"eco", "eco", Eco, true, false},
		{"garbage", "garbage", Default, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == "" {
				t.Setenv(envVar, "")
			} else {
				t.Setenv(envVar, tt.value)
			}
			gotM, gotSet, err := Parse()
			if gotSet != tt.wantSet {
				t.Errorf("set = %v, want %v", gotSet, tt.wantSet)
			}
			if gotM != tt.wantM {
				t.Errorf("mode = %q, want %q", gotM, tt.wantM)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, want err=%v", err, tt.wantErr)
			}
		})
	}
}

const scoreTestEpsilon = 1e-9

func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func TestScore_MaxCool_Formula(t *testing.T) {
	// PassiveGPU: PreferredLow=75, PreferredHigh=83.
	// score = max(0, mean - PreferredLow) + 0.5*variance.
	tests := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{TempMean: 80, TempStdDev: 0}, 5.0},  // 5 above PreferredLow
		{WindowStats{TempMean: 80, TempStdDev: 2}, 7.0},  // 5 + 0.5*4
		{WindowStats{TempMean: 60, TempStdDev: 3}, 4.5},  // below PreferredLow → 0 + 0.5*9
		{WindowStats{TempMean: 75, TempStdDev: 0}, 0.0},  // at PreferredLow → ideal
		{WindowStats{TempMean: 90, TempStdDev: 0}, 15.0}, // 15 above PreferredLow
	}

	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]

	for _, tt := range tests {
		t.Run(fmt.Sprintf("mean=%v stddev=%v", tt.stats.TempMean, tt.stats.TempStdDev), func(t *testing.T) {
			scorer := MaxCool.Score()
			got := scorer(env, tt.stats)
			if !floatNear(got, tt.want, scoreTestEpsilon) {
				t.Errorf("score = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScore_Balanced_Formula(t *testing.T) {
	// CPU: PreferredLow=55, PreferredHigh=75.
	// score = bandDistance(mean, 55, 75) + 0.3*variance + 0.3*fanRate.
	// In-band cases score zero on the bandViolation term.
	tests := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{TempMean: 65, TempStdDev: 0, FanChangeRate: 0}, 0.0}, // PreferredMid, in band → 0
		{WindowStats{TempMean: 68, TempStdDev: 0, FanChangeRate: 0}, 0.0}, // still in band → 0
		{WindowStats{TempMean: 60, TempStdDev: 0, FanChangeRate: 4}, 1.2}, // in band; only fanRate term
		{WindowStats{TempMean: 65, TempStdDev: 2, FanChangeRate: 0}, 1.2}, // in band; only variance term
		{WindowStats{TempMean: 50, TempStdDev: 0, FanChangeRate: 0}, 5.0}, // 5 below PreferredLow
		{WindowStats{TempMean: 80, TempStdDev: 0, FanChangeRate: 0}, 5.0}, // 5 above PreferredHigh
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]

	for _, tt := range tests {
		t.Run(fmt.Sprintf("mean=%v stddev=%v fanrate=%v", tt.stats.TempMean, tt.stats.TempStdDev, tt.stats.FanChangeRate), func(t *testing.T) {
			scorer := Balanced.Score()
			got := scorer(env, tt.stats)
			if !floatNear(got, tt.want, scoreTestEpsilon) {
				t.Errorf("score = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScore_MinNoise_Formula(t *testing.T) {
	// PassiveGPU: PreferredLow=75, PreferredHigh=83.
	// score = belowHigh + 5*aboveHigh + 2*fanRate + 0.5*variance.
	tests := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{TempMean: 83, TempStdDev: 0, FanChangeRate: 0}, 0.0},  // at PreferredHigh → ideal
		{WindowStats{TempMean: 88, TempStdDev: 0, FanChangeRate: 0}, 25.0}, // 5 above PreferredHigh → 5*5
		{WindowStats{TempMean: 73, TempStdDev: 0, FanChangeRate: 0}, 10.0}, // 10 below PreferredHigh
		{WindowStats{TempMean: 73, TempStdDev: 2, FanChangeRate: 3}, 18.0}, // 10 + 2*3 + 0.5*4
	}

	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]

	for _, tt := range tests {
		t.Run(fmt.Sprintf("mean=%v stddev=%v fanrate=%v", tt.stats.TempMean, tt.stats.TempStdDev, tt.stats.FanChangeRate), func(t *testing.T) {
			scorer := MinNoise.Score()
			got := scorer(env, tt.stats)
			if !floatNear(got, tt.want, scoreTestEpsilon) {
				t.Errorf("score = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScore_Eco_FallsBackToMinNoise(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.CPU]

	statsList := []WindowStats{
		{TempMean: 65, TempStdDev: 2, FanChangeRate: 3},
		{TempMean: 70, TempStdDev: 1.5, FanChangeRate: 5},
		{TempMean: 80, TempStdDev: 4, FanChangeRate: 0},
	}

	for _, stats := range statsList {
		t.Run(fmt.Sprintf("mean=%v stddev=%v fanrate=%v", stats.TempMean, stats.TempStdDev, stats.FanChangeRate), func(t *testing.T) {
			ecoScorer := Eco.Score()
			minNoiseScorer := MinNoise.Score()

			gotEco := ecoScorer(env, stats)
			wantMinNoise := minNoiseScorer(env, stats)

			if !floatNear(gotEco, wantMinNoise, scoreTestEpsilon) {
				t.Errorf("eco score = %v, want min-noise score = %v", gotEco, wantMinNoise)
			}
		})
	}
}

func TestScore_Dispatch(t *testing.T) {
	stats := WindowStats{TempMean: 65, TempStdDev: 2.5, FanChangeRate: 3.0}
	env := envelope.DefaultEnvelopes[envelope.CPU]

	tests := []struct {
		mode Mode
		want func(env envelope.Envelope, s WindowStats) float64
	}{
		{MaxCool, scoreMaxCool},
		{Balanced, scoreBalanced},
		{MinNoise, scoreMinNoise},
		{Eco, scoreEco},
	}

	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			scorer := tt.mode.Score()
			got := scorer(env, stats)
			want := tt.want(env, stats)
			if !floatNear(got, want, scoreTestEpsilon) {
				t.Errorf("dispatched score = %v, want direct call = %v", got, want)
			}
		})
	}
}

func TestScore_Monotonicity_MaxCool(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.CPU]

	stats60 := WindowStats{TempMean: 60, TempStdDev: 0}
	stats65 := WindowStats{TempMean: 65, TempStdDev: 0}
	stats70 := WindowStats{TempMean: 70, TempStdDev: 0}

	scorer := MaxCool.Score()

	score60 := scorer(env, stats60)
	score65 := scorer(env, stats65)
	score70 := scorer(env, stats70)

	if !(score60 < score65 && score65 < score70) {
		t.Errorf("max-cool should increase with temp: got %v < %v < %v", score60, score65, score70)
	}
}

func TestScore_Balanced_FlatInsideBand(t *testing.T) {
	// HDD: PreferredLow=32, PreferredMid=38, PreferredHigh=43.
	// Satisficing balanced: every value inside the band scores equally
	// on the bandViolation term. This is the v0.3.2 fix — previously,
	// balanced was minimized strictly at PreferredMid, so observed mean
	// anywhere in the band would drift target away to chase a single
	// point, pushing the PID into saturation for no thermal benefit.
	env := envelope.DefaultEnvelopes[envelope.HDD]

	zeroRate := WindowStats{TempStdDev: 0, FanChangeRate: 0}
	scorer := Balanced.Score()

	inBand := []float64{32, 35, 38, 41, 43}
	for _, t1 := range inBand {
		s1 := zeroRate
		s1.TempMean = t1
		got := scorer(env, s1)
		if !floatNear(got, 0.0, scoreTestEpsilon) {
			t.Errorf("mean=%v inside band: score=%v, want 0 (satisficing)", t1, got)
		}
	}

	// Outside the band, the score grows linearly with distance to the
	// nearest band edge.
	tests := []struct {
		mean float64
		want float64
	}{
		{30, 2.0}, // 2 below PreferredLow
		{45, 2.0}, // 2 above PreferredHigh
		{25, 7.0}, // 7 below PreferredLow
		{50, 7.0}, // 7 above PreferredHigh
	}
	for _, tt := range tests {
		s := zeroRate
		s.TempMean = tt.mean
		got := scorer(env, s)
		if !floatNear(got, tt.want, scoreTestEpsilon) {
			t.Errorf("mean=%v outside band: score=%v, want %v", tt.mean, got, tt.want)
		}
	}
}

func TestScore_MinNoise_LowerNearPreferredHigh(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.SSD] // PreferredHigh = 60

	stats60 := WindowStats{TempMean: 60, TempStdDev: 0, FanChangeRate: 0}
	stats55 := WindowStats{TempMean: 55, TempStdDev: 0, FanChangeRate: 0}

	scorer := MinNoise.Score()

	score60 := scorer(env, stats60)
	score55 := scorer(env, stats55)

	if !(score60 < score55) {
		t.Errorf("min-noise should be lower near PreferredHigh: got %v < %v", score60, score55)
	}
}

// TestScore_SaturationPenalty_KeysOffP90NotMean verifies the v0.3.9 fix:
// the saturation penalty must depend ONLY on FanDemandP90, so two windows
// with wildly different means but identical p90 score identically. This is
// what makes a transient fan dip unable to unmask the saturation signal.
func TestScore_SaturationPenalty_KeysOffP90NotMean(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]

	// Same temp/variance/p90; means differ by 30 points (dip vs no dip).
	pinnedClean := WindowStats{TempMean: 81, TempStdDev: 1, FanChangeRate: 0.5, FanDemandMean: 99, FanDemandP90: 100}
	pinnedDipped := WindowStats{TempMean: 81, TempStdDev: 1, FanChangeRate: 0.5, FanDemandMean: 69, FanDemandP90: 100}

	for _, m := range []Mode{MaxCool, Balanced, MinNoise, Eco} {
		t.Run(string(m), func(t *testing.T) {
			scorer := m.Score()
			if a, b := scorer(env, pinnedClean), scorer(env, pinnedDipped); a != b {
				t.Errorf("%s: score must depend only on p90, not mean; clean=%v dipped=%v", m, a, b)
			}
		})
	}
}

// TestScore_MinNoise_DownDriftAllowedWithHeadroom is the anti-regression
// guard for the fix: when the fan genuinely has headroom (p90 well below
// the saturation threshold) and temp sits above PreferredHigh, drifting the
// target DOWN must still score better than holding. The p90 fix must not
// over-suppress legitimate downward drift — only the saturated case.
func TestScore_MinNoise_DownDriftAllowedWithHeadroom(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.PassiveGPU] // PreferredHigh=83
	scorer := MinNoise.Score()

	// Temp above PreferredHigh, fan cruising (p90=60 → zero penalty).
	now := WindowStats{TempMean: 85, TempStdDev: 1, FanChangeRate: 0.5, FanDemandP90: 60}
	// Synth "down 1°C": temp falls, fan rises a little — still unsaturated.
	down := WindowStats{TempMean: 84, TempStdDev: 1.3, FanChangeRate: 1.0, FanDemandP90: 65}

	if !(scorer(env, down) < scorer(env, now)) {
		t.Errorf("with fan headroom and temp above PreferredHigh, down-drift should score lower; now=%v down=%v",
			scorer(env, now), scorer(env, down))
	}
}

func TestSaturationPenalty_QuadraticAbove90(t *testing.T) {
	cases := []struct {
		fan, want float64
	}{
		{0, 0},
		{50, 0},
		{90, 0}, // exactly at threshold
		{95, 25},
		{100, 100},
	}
	for _, c := range cases {
		got := saturationPenalty(c.fan)
		if !floatNear(got, c.want, scoreTestEpsilon) {
			t.Errorf("saturationPenalty(%v) = %v, want %v", c.fan, got, c.want)
		}
	}
}

func TestScore_SaturationDrivesTargetUp_AllModes(t *testing.T) {
	// Regression test for the docker-1 "fan stuck at 100 while temp
	// in-band" pattern. Pre-v0.3.7, mean-only scoring saw in-band temp
	// + low variance + low fan-change-rate as "settled" — score stayed
	// flat across up/now/down projections, no drift. With the
	// saturation penalty + FanDemandMean projection in the synth,
	// raising target ALWAYS scores strictly better than holding when
	// fans are saturated. Verified per-mode because each weights the
	// penalty differently.
	env := envelope.DefaultEnvelopes[envelope.PassiveGPU]

	// Saturated state: in-band, low variance, fan pinned near MaxFan.
	// FanDemandP90 is the field the penalty reads (post-v0.3.9).
	statsNow := WindowStats{
		TempMean: 78, TempStdDev: 1.0, FanChangeRate: 0.5, FanDemandMean: 98, FanDemandP90: 100,
	}
	// Synth "up by 1°C": mean shifts up, fan demand drops.
	statsUp := WindowStats{
		TempMean: 79, TempStdDev: 0.7, FanChangeRate: 0, FanDemandMean: 93, FanDemandP90: 95,
	}

	for _, m := range []Mode{MaxCool, Balanced, MinNoise, Eco} {
		t.Run(string(m), func(t *testing.T) {
			scorer := m.Score()
			now := scorer(env, statsNow)
			up := scorer(env, statsUp)
			if !(up < now) {
				t.Errorf("%s: under saturation, raising target should score lower; got up=%v now=%v", m, up, now)
			}
		})
	}
}
