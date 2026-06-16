package envelope

import (
	"testing"
)

func TestClass_String(t *testing.T) {
	cases := map[Class]string{
		CPU:        "cpu",
		PassiveGPU: "passive_gpu",
		ActiveGPU:  "active_gpu",
		HDD:        "hdd",
		SSD:        "ssd",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("Class(%q).String() = %q, want %q", string(c), got, want)
		}
	}
}

func TestGet(t *testing.T) {
	// Known classes return their envelope and no error.
	for _, c := range []Class{CPU, PassiveGPU, HDD, SSD} {
		e, err := Get(c)
		if err != nil {
			t.Errorf("Get(%s) returned error: %v", c, err)
		}
		if e != DefaultEnvelopes[c] {
			t.Errorf("Get(%s) = %+v, want %+v", c, e, DefaultEnvelopes[c])
		}
	}
	// ActiveGPU is intentionally absent → error, zero envelope.
	if e, err := Get(ActiveGPU); err == nil {
		t.Errorf("Get(ActiveGPU) should error (no chassis envelope), got %+v", e)
	}
	// Unknown class → error.
	if _, err := Get(Class("bogus")); err == nil {
		t.Error("Get(bogus) should return an error")
	}
}

func TestDefaultEnvelopes_AllClassesPresent(t *testing.T) {
	expected := []Class{CPU, PassiveGPU, HDD, SSD}
	excluded := ActiveGPU

	for _, c := range expected {
		if _, ok := DefaultEnvelopes[c]; !ok {
			t.Errorf("expected class %s to be present in DefaultEnvelopes", c)
		}
	}

	if _, ok := DefaultEnvelopes[excluded]; ok {
		t.Errorf("expected class %s to NOT be present in DefaultEnvelopes", excluded)
	}
}

func TestDefaultEnvelopes_ExactValues(t *testing.T) {
	tests := []struct {
		name string
		c    Class
		want Envelope
	}{
		{
			name: "CPU",
			c:    CPU,
			want: Envelope{
				MinSafe:       20,
				PreferredLow:  55,
				PreferredMid:  65,
				PreferredHigh: 75,
				MaxSafe:       85,
				Emergency:     90,
			},
		},
		{
			name: "PassiveGPU",
			c:    PassiveGPU,
			want: Envelope{
				MinSafe:       30,
				PreferredLow:  65,
				PreferredMid:  72,
				PreferredHigh: 80,
				MaxSafe:       85,
				Emergency:     90,
			},
		},
		{
			name: "HDD",
			c:    HDD,
			want: Envelope{
				MinSafe:       10,
				PreferredLow:  32,
				PreferredMid:  38,
				PreferredHigh: 43,
				MaxSafe:       45,
				Emergency:     50,
			},
		},
		{
			name: "SSD",
			c:    SSD,
			want: Envelope{
				MinSafe:       15,
				PreferredLow:  45,
				PreferredMid:  50,
				PreferredHigh: 60,
				MaxSafe:       70,
				Emergency:     80,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DefaultEnvelopes[tt.c]
			if !ok {
				t.Fatalf("class %s not found in DefaultEnvelopes", tt.c)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEnvelope_Valid(t *testing.T) {
	tests := []struct {
		name     string
		envelope Envelope
		want     bool
	}{
		{
			name: "valid envelope",
			envelope: Envelope{
				MinSafe:       20,
				PreferredLow:  55,
				PreferredMid:  65,
				PreferredHigh: 75,
				MaxSafe:       85,
				Emergency:     90,
			},
			want: true,
		},
		{
			name: "invalid PreferredLow > PreferredMid",
			envelope: Envelope{
				MinSafe:       20,
				PreferredLow:  70,
				PreferredMid:  65,
				PreferredHigh: 75,
				MaxSafe:       85,
				Emergency:     90,
			},
			want: false,
		},
		{
			name: "invalid MaxSafe > Emergency",
			envelope: Envelope{
				MinSafe:       20,
				PreferredLow:  55,
				PreferredMid:  65,
				PreferredHigh: 75,
				MaxSafe:       95,
				Emergency:     90,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.envelope.Valid()
			if got != tt.want {
				t.Errorf("Envelope.Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultEnvelopes_AllValid(t *testing.T) {
	for c, e := range DefaultEnvelopes {
		t.Run(string(c), func(t *testing.T) {
			if !e.Valid() {
				t.Errorf("envelope for class %s failed validation: %+v", c, e)
			}
		})
	}
}
