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
				envelope.PassiveGPU: {65, 2},
				envelope.HDD:        {32, 2},
				envelope.SSD:        {45, 2},
			},
		},
		{
			name: "Balanced",
			mode: Balanced,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {65, 3},
				envelope.PassiveGPU: {72, 3},
				envelope.HDD:        {38, 3},
				envelope.SSD:        {50, 3},
			},
		},
		{
			name: "MinNoise",
			mode: MinNoise,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {75, 4},
				envelope.PassiveGPU: {80, 4},
				envelope.HDD:        {43, 4},
				envelope.SSD:        {60, 4},
			},
		},
		{
			name: "Eco",
			mode: Eco,
			expected: map[envelope.Class][2]int{
				envelope.CPU:        {75, 5},
				envelope.PassiveGPU: {80, 5},
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
	tests := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{TempMean: 70, TempStdDev: 0}, 70.0},
		{WindowStats{TempMean: 70, TempStdDev: 2}, 72.0},
		{WindowStats{TempMean: 60, TempStdDev: 3}, 64.5},
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
	tests := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{TempMean: 65, TempStdDev: 0, FanChangeRate: 0}, 0.0},
		{WindowStats{TempMean: 68, TempStdDev: 0, FanChangeRate: 0}, 3.0},
		{WindowStats{TempMean: 60, TempStdDev: 0, FanChangeRate: 4}, 6.2},
		{WindowStats{TempMean: 65, TempStdDev: 2, FanChangeRate: 0}, 1.2},
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
	tests := []struct {
		stats WindowStats
		want  float64
	}{
		{WindowStats{TempMean: 80, TempStdDev: 0, FanChangeRate: 0}, 0.0},
		{WindowStats{TempMean: 85, TempStdDev: 0, FanChangeRate: 0}, 0.0},
		{WindowStats{TempMean: 70, TempStdDev: 0, FanChangeRate: 0}, 10.0},
		{WindowStats{TempMean: 70, TempStdDev: 2, FanChangeRate: 3}, 18.0},
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

func TestScore_Balanced_MinimizedAtPreferredMid(t *testing.T) {
	env := envelope.DefaultEnvelopes[envelope.HDD] // PreferredMid = 38

	stats35 := WindowStats{TempMean: 35, TempStdDev: 0, FanChangeRate: 0}
	stats38 := WindowStats{TempMean: 38, TempStdDev: 0, FanChangeRate: 0}
	stats42 := WindowStats{TempMean: 42, TempStdDev: 0, FanChangeRate: 0}

	scorer := Balanced.Score()

	score35 := scorer(env, stats35)
	score38 := scorer(env, stats38)
	score42 := scorer(env, stats42)

	if !(score38 < score35 && score38 < score42) {
		t.Errorf("balanced should be minimized at PreferredMid: got %v < %v < %v", score38, score35, score42)
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
