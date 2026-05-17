package mode

import (
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
		name         string
		mode         Mode
		expected     map[envelope.Class][2]int // class -> [target, deadband]
	}{
		{
			name: "MaxCool",
			mode: MaxCool,
			expected: map[envelope.Class][2]int{
				envelope.CPU:       {55, 2},
				envelope.PassiveGPU: {65, 2},
				envelope.HDD:       {32, 2},
				envelope.SSD:       {45, 2},
			},
		},
		{
			name: "Balanced",
			mode: Balanced,
			expected: map[envelope.Class][2]int{
				envelope.CPU:       {65, 3},
				envelope.PassiveGPU: {72, 3},
				envelope.HDD:       {38, 3},
				envelope.SSD:       {50, 3},
			},
		},
		{
			name: "MinNoise",
			mode: MinNoise,
			expected: map[envelope.Class][2]int{
				envelope.CPU:       {75, 4},
				envelope.PassiveGPU: {80, 4},
				envelope.HDD:       {43, 4},
				envelope.SSD:       {60, 4},
			},
		},
		{
			name: "Eco",
			mode: Eco,
			expected: map[envelope.Class][2]int{
				envelope.CPU:       {75, 5},
				envelope.PassiveGPU: {80, 5},
				envelope.HDD:       {43, 5},
				envelope.SSD:       {60, 5},
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
		name     string
		value    string
		wantM    Mode
		wantSet  bool
		wantErr  bool
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

func TestScore_AllStubs_ReturnZero(t *testing.T) {
	stats := WindowStats{
		TempMean:      65.0,
		TempStdDev:    2.0,
		TempP10:       62.0,
		TempP50:       65.0,
		TempP90:       68.0,
		FanDemandMean: 45.0,
		FanChangeRate: 3.0,
		InletMean:     22.0,
		InletStdDev:   1.0,
		Samples:       480,
	}

	env := envelope.DefaultEnvelopes[envelope.CPU]

	tests := []Mode{MaxCool, Balanced, MinNoise, Eco}
	for _, m := range tests {
		t.Run(m.String(), func(t *testing.T) {
			scorer := m.Score()
			got := scorer(env, stats)
			if got != 0.0 {
				t.Errorf("score = %v, want 0.0 (stub)", got)
			}
		})
	}
}
