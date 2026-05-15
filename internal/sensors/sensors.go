// Package sensors exposes one TemperatureSource per hardware class and
// a Reading type that the controller aggregates each cycle.
//
// Each source is independently testable via a Runner — file reads go
// through a small filesystem abstraction so /sys/class/hwmon can be
// faked too.
package sensors

import "strings"

// Reading is a single cycle's collected temperatures, in °C. Fields
// are 0 when the source is absent or returned no data — the
// controller's PIDs treat 0 as "abstain" (no candidate).
type Reading struct {
	CPUMax        int
	PassiveGPUMax int
	ActiveGPUMax  int
	HDDMax        int
	SSDMax        int
	// Details is the human-readable per-sensor summary appended to the
	// log line each cycle. Examples: "P0.t1:42 P0.t2:43 Gp0:75 d0h:33".
	Details string
}

// detailsBuilder collects per-sensor detail tags, preserving the bash
// "<tag> " with trailing space format.
type detailsBuilder struct {
	sb strings.Builder
}

func (d *detailsBuilder) Add(tag string) {
	d.sb.WriteString(tag)
	d.sb.WriteByte(' ')
}

func (d *detailsBuilder) String() string {
	return d.sb.String()
}
