package main

import (
	"reflect"
	"testing"
)

func TestParseFanIndices(t *testing.T) {
	cases := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{"0-7", []int{0, 1, 2, 3, 4, 5, 6, 7}, false},
		{"0-0", []int{0}, false},
		{"0,2,4", []int{0, 2, 4}, false},
		{" 1 , 3 ", []int{1, 3}, false},
		{"", nil, true},
		{"7-0", nil, true},   // inverted range
		{"0-99", nil, true},  // out of bounds
		{"0,abc", nil, true}, // bad token
		{"-1", nil, true},    // parses as bad range (empty lo)
	}
	for _, tc := range cases {
		got, err := parseFanIndices(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFanIndices(%q): expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFanIndices(%q): unexpected error %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseFanIndices(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}
