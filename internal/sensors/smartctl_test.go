package sensors

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func TestParseSmartctlTemp_SCSI(t *testing.T) {
	out := readFixture(t, "smartctl_scsi_hdd.txt")
	temp, ok := ParseSmartctlTemp(out)
	if !ok || temp != 31 {
		t.Errorf("SCSI: got %d/%v want 31/true", temp, ok)
	}
}

func TestParseSmartctlTemp_SATA(t *testing.T) {
	out := readFixture(t, "smartctl_sata_hdd.txt")
	temp, ok := ParseSmartctlTemp(out)
	if !ok || temp != 42 {
		t.Errorf("SATA: got %d/%v want 42/true", temp, ok)
	}
}

func TestParseSmartctlTemp_NVMe(t *testing.T) {
	out := readFixture(t, "smartctl_nvme.txt")
	temp, ok := ParseSmartctlTemp(out)
	if !ok || temp != 37 {
		t.Errorf("NVMe: got %d/%v want 37/true", temp, ok)
	}
}

func TestParseSmartctlTemp_None(t *testing.T) {
	_, ok := ParseSmartctlTemp("some random text\nwith no temp\n")
	if ok {
		t.Error("expected ok=false for unparseable output")
	}
}

func TestParseSmartctlTemp_ZeroIsRejected(t *testing.T) {
	// Genuine 0°C readings (or buggy "0" from a freshly-spun-up drive)
	// must NOT be returned as valid — bash's `[ "$t" -gt 0 ]` guard.
	_, ok := ParseSmartctlTemp("Current Drive Temperature:     0 C\n")
	if ok {
		t.Error("temp=0 should be rejected as not parseable")
	}
}

func TestSmartctl_Probe_ScanAndClassify(t *testing.T) {
	r := runner.NewFakeRunner()
	r.Set("smartctl", []string{"--scan"}, runner.FakeResponse{
		Output: readFixture(t, "smartctl_scan.txt"),
	})
	// All HDDs return rotation-rate-rpm; nvme has no rotation rate.
	hddInfo := readFixture(t, "smartctl_info_hdd.txt")
	ssdInfo := readFixture(t, "smartctl_info_ssd.txt")
	r.Set("smartctl", []string{"-i", "-d", "scsi", "/dev/sda"},
		runner.FakeResponse{Output: hddInfo})
	r.Set("smartctl", []string{"-i", "-d", "megaraid,0", "/dev/bus/0"},
		runner.FakeResponse{Output: hddInfo})
	r.Set("smartctl", []string{"-i", "-d", "megaraid,1", "/dev/bus/0"},
		runner.FakeResponse{Output: ssdInfo})
	r.Set("smartctl", []string{"-i", "-d", "nvme", "/dev/nvme0"},
		runner.FakeResponse{Output: "..."}) // no rotation rate

	s := NewSmartctl(r)
	label, fatal := s.Probe(context.Background(), "auto")
	if fatal {
		t.Fatalf("unexpected fatal: %s", label)
	}
	if !s.Enabled {
		t.Fatal("smartctl should be enabled")
	}
	if len(s.Drives) != 4 {
		t.Fatalf("got %d drives, want 4", len(s.Drives))
	}
	wantTypes := []DriveType{DriveHDD, DriveHDD, DriveSSD, DriveSSD}
	for i, d := range s.Drives {
		if d.Type != wantTypes[i] {
			t.Errorf("drive %d (%s): got type %s, want %s", i, d.Dev, d.Type, wantTypes[i])
		}
	}
}

func TestSmartctl_Read_CacheAndTemp(t *testing.T) {
	r := runner.NewFakeRunner()
	// Two drives: one HDD (SCSI) returning 31°C, one SSD returning NVMe 37°C.
	s := &Smartctl{
		Runner: r,
		Drives: []Drive{
			{Dev: "/dev/sda", Spec: "scsi", Type: DriveHDD},
			{Dev: "/dev/nvme0", Spec: "nvme", Type: DriveSSD},
		},
		Enabled:      true,
		ReadInterval: 60 * time.Second,
		Now:          func() time.Time { return time.Unix(1000, 0) },
	}
	r.Set("smartctl", []string{"-A", "-n", "standby", "-d", "scsi", "/dev/sda"},
		runner.FakeResponse{Output: readFixture(t, "smartctl_scsi_hdd.txt")})
	r.Set("smartctl", []string{"-A", "-n", "standby", "-d", "nvme", "/dev/nvme0"},
		runner.FakeResponse{Output: readFixture(t, "smartctl_nvme.txt")})

	hdd, ssd, deets, ok := s.Read(context.Background())
	if !ok {
		t.Fatal("Read returned ok=false")
	}
	if hdd != 31 {
		t.Errorf("HDD max: got %d want 31", hdd)
	}
	if ssd != 37 {
		t.Errorf("SSD max: got %d want 37", ssd)
	}
	// Details should include both tags.
	if !contains(deets, "d0h:31") {
		t.Errorf("details missing d0h:31: %q", deets)
	}
	if !contains(deets, "d1s:37") {
		t.Errorf("details missing d1s:37: %q", deets)
	}

	// Second call within cache window must NOT spawn smartctl again.
	callsBefore := len(r.Calls)
	s.Now = func() time.Time { return time.Unix(1030, 0) } // +30s, within 60s
	hdd2, ssd2, _, _ := s.Read(context.Background())
	if hdd2 != 31 || ssd2 != 37 {
		t.Errorf("cached read: got %d/%d want 31/37", hdd2, ssd2)
	}
	if len(r.Calls) != callsBefore {
		t.Errorf("cache miss: expected %d calls, got %d", callsBefore, len(r.Calls))
	}

	// After cache expires, must re-poll.
	s.Now = func() time.Time { return time.Unix(1100, 0) } // +100s
	_, _, _, _ = s.Read(context.Background())
	if len(r.Calls) <= callsBefore {
		t.Error("expected re-poll after cache expiry")
	}
}

func TestSmartctl_Read_StandbyDrive(t *testing.T) {
	r := runner.NewFakeRunner()
	r.Set("smartctl", []string{"-A", "-n", "standby", "-d", "scsi", "/dev/sda"},
		runner.FakeResponse{Output: "Device is in STANDBY mode", ExitCode: 2})
	s := &Smartctl{
		Runner:       r,
		Drives:       []Drive{{Dev: "/dev/sda", Spec: "scsi", Type: DriveHDD}},
		Enabled:      true,
		ReadInterval: 60 * time.Second,
		Now:          func() time.Time { return time.Unix(2000, 0) },
	}
	hdd, _, deets, ok := s.Read(context.Background())
	if !ok {
		t.Fatal("ok=false")
	}
	if hdd != 0 {
		t.Errorf("standby drive should yield max=0, got %d", hdd)
	}
	if !contains(deets, "d0:zZ") {
		t.Errorf("standby drive should emit zZ tag, got %q", deets)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
