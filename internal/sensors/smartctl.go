package sensors

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

// DriveType is hdd or ssd (NVMe folds into ssd). Classification happens
// at probe time via smartctl -i and the "Rotation Rate" field.
type DriveType int

const (
	DriveHDD DriveType = iota
	DriveSSD
)

func (d DriveType) String() string {
	if d == DriveHDD {
		return "hdd"
	}
	return "ssd"
}

// Drive is one discovered drive.
type Drive struct {
	Dev  string // e.g. /dev/sda, /dev/bus/0
	Spec string // e.g. scsi, megaraid,0, nvme
	Type DriveType
}

// Smartctl handles drive discovery, classification, per-drive temp
// reads, and the cache (60s default) that prevents hammering the RAID
// controller every 15-second main-loop cycle.
type Smartctl struct {
	Runner       runner.Runner
	Enabled      bool
	Drives       []Drive
	ReadInterval time.Duration
	// Now is injected for deterministic tests.
	Now func() time.Time

	// Cache.
	lastRead    time.Time
	cachedHDD   int
	cachedSSD   int
	cachedDeets string
}

func NewSmartctl(r runner.Runner) *Smartctl {
	return &Smartctl{
		Runner:       r,
		ReadInterval: 60 * time.Second,
		Now:          time.Now,
	}
}

// Probe runs `smartctl --scan` and classifies each discovered drive
// HDD vs SSD via Rotation Rate. Returns (label, fatal) — fatal means
// HDD_AWARE=true was set but smartctl is unavailable.
func (s *Smartctl) Probe(ctx context.Context, mode string) (label string, fatal bool) {
	switch mode {
	case "false":
		s.Enabled = false
		return "HDD monitoring disabled (HDD_AWARE=false)", false
	case "true":
		// fall through to scan — if smartctl is broken we fail fatal below
	default: // auto
		// fall through — silently disable on error
	}

	scan, err := s.Runner.Run(ctx, "smartctl", "--scan")
	if err != nil || strings.TrimSpace(scan) == "" {
		s.Enabled = false
		if mode == "true" {
			return "FATAL: HDD_AWARE=true but smartctl not available", true
		}
		if err != nil {
			return "No smartctl — HDD monitoring disabled", false
		}
		return "smartctl --scan returned nothing — HDD monitoring disabled", false
	}

	for _, line := range strings.Split(scan, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Strip trailing # comment.
		if hash := strings.Index(line, "#"); hash >= 0 {
			line = strings.TrimSpace(line[:hash])
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		dev := fields[0]
		// Field 1 is `-d`, field 2 is the spec (scsi, megaraid,N, nvme, sat).
		spec := fields[2]
		if dev == "" || spec == "" {
			continue
		}

		// Classify by Rotation Rate.
		drive := Drive{Dev: dev, Spec: spec, Type: DriveSSD}
		info, _ := s.Runner.Run(ctx, "smartctl", "-i", "-d", spec, dev)
		if rotationRateRE.MatchString(info) {
			drive.Type = DriveHDD
		}
		s.Drives = append(s.Drives, drive)
	}

	if len(s.Drives) == 0 {
		s.Enabled = false
		return "No drives parsed from smartctl --scan — HDD monitoring disabled", false
	}

	s.Enabled = true
	hddCount, ssdCount := 0, 0
	var list strings.Builder
	for _, d := range s.Drives {
		fmt.Fprintf(&list, "%s(%s/%s) ", d.Dev, d.Spec, d.Type)
		if d.Type == DriveHDD {
			hddCount++
		} else {
			ssdCount++
		}
	}
	return fmt.Sprintf("HDD monitoring: %d drive(s) (%d HDD, %d SSD) — %s",
		len(s.Drives), hddCount, ssdCount, list.String()), false
}

// rotationRateRE — `Rotation Rate:     7200 rpm`. Bash:
// `Rotation Rate:[[:space:]]+[0-9]+[[:space:]]+rpm`.
var rotationRateRE = regexp.MustCompile(`Rotation Rate:\s+\d+\s+rpm`)

// Read returns (hddMax, ssdMax, details, ok). Cached for ReadInterval.
// ok=false means the source is disabled — the per-drive read itself
// always returns ok=true once enabled (individual drives that fail to
// read contribute "?" or "zZ" tags to details but max remains 0,
// which the PID treats as abstain).
func (s *Smartctl) Read(ctx context.Context) (hddMax, ssdMax int, details string, ok bool) {
	if !s.Enabled {
		return 0, 0, "", false
	}

	now := s.Now()
	if !s.lastRead.IsZero() && now.Sub(s.lastRead) < s.ReadInterval {
		return s.cachedHDD, s.cachedSSD, s.cachedDeets, true
	}

	var b detailsBuilder
	hddMax = 0
	ssdMax = 0
	for i, d := range s.Drives {
		out, err := s.Runner.Run(ctx, "smartctl", "-A", "-n", "standby", "-d", d.Spec, d.Dev)
		if err != nil && runner.ExitCode(err) == 2 {
			// Drive in standby — don't wake it.
			b.Add(fmt.Sprintf("d%d:zZ", i))
			continue
		}

		t, parsed := ParseSmartctlTemp(out)
		if !parsed || t <= 0 {
			b.Add(fmt.Sprintf("d%d:?", i))
			continue
		}
		// Tag = first letter of "hdd" or "ssd" — bash: `${type%?}` strips
		// the trailing character.
		typeTag := "h"
		if d.Type == DriveSSD {
			typeTag = "s"
		}
		b.Add(fmt.Sprintf("d%d%s:%d", i, typeTag, t))
		switch d.Type {
		case DriveHDD:
			if t > hddMax {
				hddMax = t
			}
		case DriveSSD:
			if t > ssdMax {
				ssdMax = t
			}
		}
	}

	s.cachedHDD = hddMax
	s.cachedSSD = ssdMax
	s.cachedDeets = b.String()
	s.lastRead = now
	return hddMax, ssdMax, b.String(), true
}

// ParseSmartctlTemp extracts a drive temperature in °C from smartctl
// -A output. Three formats, tried in priority order:
//
//  1. SCSI:  "Current Drive Temperature:     31 C"
//  2. SATA:  attribute 194 row, column 10 (raw temp).
//  3. NVMe:  "Temperature:                    32 Celsius"
//
// Returns (temp, true) on success; (0, false) if no format matched
// OR if the parsed value is ≤ 0.
func ParseSmartctlTemp(out string) (int, bool) {
	// 1. SCSI.
	if m := scsiTempRE.FindStringSubmatch(out); m != nil {
		if t, err := strconv.Atoi(m[1]); err == nil && t > 0 {
			return t, true
		}
	}
	// 2. SATA attribute 194 (Temperature_Celsius). Bash:
	//   awk '/^[[:space:]]*194[[:space:]]+Temperature/{print $10; exit}'
	// Column 10 (1-indexed) is the RAW_VALUE column.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[0] != "194" {
			continue
		}
		if !strings.HasPrefix(fields[1], "Temperature") {
			continue
		}
		if t, err := strconv.Atoi(fields[9]); err == nil && t > 0 {
			return t, true
		}
		break // matched the row but couldn't parse — fall through
	}
	// 3. NVMe.
	if m := nvmeTempRE.FindStringSubmatch(out); m != nil {
		if t, err := strconv.Atoi(m[1]); err == nil && t > 0 {
			return t, true
		}
	}
	return 0, false
}

var scsiTempRE = regexp.MustCompile(`Current Drive Temperature:\s+(\d+)`)
var nvmeTempRE = regexp.MustCompile(`Temperature:\s+(\d+)\s+Celsius`)
