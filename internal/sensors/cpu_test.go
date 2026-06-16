package sensors

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

// fstestAdapter wraps fstest.MapFS to satisfy the sensors.FS interface
// (matching the minimal ReadFile + ReadDir surface).
type fstestAdapter struct{ fstest.MapFS }

func (a fstestAdapter) ReadFile(name string) ([]byte, error) {
	return a.MapFS.ReadFile(name)
}
func (a fstestAdapter) ReadDir(name string) ([]fs.DirEntry, error) {
	return a.MapFS.ReadDir(name)
}

func TestCPU_Read_Coretemp(t *testing.T) {
	// Two packages, 4 cores each, max @ P0.t3 = 72°C.
	mfs := fstest.MapFS{
		"sys/class/hwmon/hwmon0/name":        &fstest.MapFile{Data: []byte("coretemp\n")},
		"sys/class/hwmon/hwmon0/temp1_input": &fstest.MapFile{Data: []byte("50000\n")},
		"sys/class/hwmon/hwmon0/temp2_input": &fstest.MapFile{Data: []byte("55000\n")},
		"sys/class/hwmon/hwmon0/temp3_input": &fstest.MapFile{Data: []byte("72000\n")},
		"sys/class/hwmon/hwmon1/name":        &fstest.MapFile{Data: []byte("coretemp\n")},
		"sys/class/hwmon/hwmon1/temp1_input": &fstest.MapFile{Data: []byte("48000\n")},
		"sys/class/hwmon/hwmon1/temp2_input": &fstest.MapFile{Data: []byte("51000\n")},
		// Distractor hwmon entry (e.g. ACPI thermal) — should be skipped.
		"sys/class/hwmon/hwmon2/name":        &fstest.MapFile{Data: []byte("acpitz\n")},
		"sys/class/hwmon/hwmon2/temp1_input": &fstest.MapFile{Data: []byte("40000\n")},
	}
	c := &CPU{Filesystem: fstestAdapter{mfs}, HwmonRoot: "/sys/class/hwmon"}
	max, details, ok := c.readCoretemp()
	if !ok {
		t.Fatal("readCoretemp ok=false")
	}
	if max != 72 {
		t.Errorf("max: got %d want 72", max)
	}
	for _, want := range []string{"P0.t1:50", "P0.t2:55", "P0.t3:72", "P1.t1:48", "P1.t2:51"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q: %s", want, details)
		}
	}
	if strings.Contains(details, "acpi") {
		t.Errorf("details should not include acpi: %s", details)
	}
}

func TestCPU_Read_IPMIFallback(t *testing.T) {
	r := runner.NewFakeRunner()
	r.Set("ipmitool", []string{"sdr", "type", "temperature"}, runner.FakeResponse{
		Output: `Inlet Temp       | 04h | ok  |  7.1 | 22 degrees C
CPU1 Temp        | 0Eh | ok  |  3.1 | 51 degrees C
CPU2 Temp        | 0Fh | ok  |  3.2 | 65 degrees C
CPU1 Disabled    | 11h | ns  |  3.1 | Disabled
Exhaust Temp     | 05h | ok  |  7.2 | 31 degrees C
`,
	})
	// No coretemp filesystem → only IPMI source contributes.
	c := &CPU{Filesystem: fstestAdapter{fstest.MapFS{}}, Runner: r, HwmonRoot: "/sys/class/hwmon"}
	max, details, ok := c.Read(context.Background())
	if !ok {
		t.Fatal("Read ok=false")
	}
	if max != 65 {
		t.Errorf("max: got %d want 65", max)
	}
	if !strings.Contains(details, "IPMI3.1:51") || !strings.Contains(details, "IPMI3.2:65") {
		t.Errorf("missing per-entity details: %s", details)
	}
}

func TestCPU_Read_NoSourcesReturnsNotOk(t *testing.T) {
	r := runner.NewFakeRunner()
	c := &CPU{Filesystem: fstestAdapter{fstest.MapFS{}}, Runner: r, HwmonRoot: "/sys/class/hwmon"}
	_, _, ok := c.Read(context.Background())
	if ok {
		t.Error("no coretemp + no IPMI should yield ok=false")
	}
}

func TestNewCPU_Defaults(t *testing.T) {
	c := NewCPU(runner.NewFakeRunner(), fstestAdapter{fstest.MapFS{}})
	if c.HwmonRoot != "/sys/class/hwmon" {
		t.Errorf("HwmonRoot=%q, want /sys/class/hwmon", c.HwmonRoot)
	}
	if c.Runner == nil || c.Filesystem == nil {
		t.Error("NewCPU should populate Runner and Filesystem")
	}
}
