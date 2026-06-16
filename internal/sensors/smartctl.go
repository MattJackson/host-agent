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
//
// Not safe for concurrent use: Read/Probe mutate the unguarded cache
// fields and are only ever called from the single main-cycle goroutine.
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

	// Bound the whole discovery pass: on a host with many drives behind a
	// slow RAID controller, the per-drive `smartctl -i` calls (each with
	// the runner's own 30s timeout) could otherwise serialise into many
	// minutes. 120s is generous for any realistic drive count.
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

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

	var classifyErrs int
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

		// Classify by Rotation Rate. If `smartctl -i` fails (permission,
		// RAID timeout, device busy) the drive silently defaults to SSD,
		// which would put it on the wrong PID track for the process
		// lifetime — so count the failures and surface them in the label
		// the caller logs.
		drive := Drive{Dev: dev, Spec: spec, Type: DriveSSD}
		info, infoErr := s.Runner.Run(ctx, "smartctl", "-i", "-d", spec, dev)
		if infoErr != nil {
			classifyErrs++
		}
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
	label = fmt.Sprintf("HDD monitoring: %d drive(s) (%d HDD, %d SSD) — %s",
		len(s.Drives), hddCount, ssdCount, list.String())
	if classifyErrs > 0 {
		label += fmt.Sprintf(" [WARN: %d drive(s) failed `smartctl -i` classification and defaulted to SSD]", classifyErrs)
	}
	return label, false
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
			// Exit code 2 from `-n standby` is ambiguous: it means the
			// drive is in STANDBY/SLEEP (don't wake it — tag zZ) OR
			// smartctl hit an OpenDevice error, e.g. a RAID controller
			// pulled a failed drive (tag x so a vanished drive is
			// distinguishable from a sleeping one on the dashboard).
			if standbyRE.MatchString(out) {
				b.Add(fmt.Sprintf("d%d:zZ", i))
			} else {
				b.Add(fmt.Sprintf("d%d:x", i))
			}
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
	// 2. SATA attribute 194 (Temperature_Celsius) or 190
	//   (Airflow_Temperature_Cel). Some SATA-over-SCSI / SAT-translated
	//   drives report temperature only under 190, so accept either ID
	//   whose attribute name contains "Temperature". Column 10 (1-indexed)
	//   is the RAW_VALUE column.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[0] != "194" && fields[0] != "190" {
			continue
		}
		if !strings.Contains(fields[1], "Temperature") {
			continue
		}
		if t, err := strconv.Atoi(fields[9]); err == nil && t > 0 {
			return t, true
		}
		// This Temperature row didn't parse (e.g. a broken airflow sensor
		// reporting attr 190 RAW=0). Keep scanning — a later valid 194 row
		// may still carry the real temperature. (Was `break`, which silently
		// suppressed a good 194 when a bad 190 preceded it.)
		continue
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

// standbyRE distinguishes a genuine STANDBY/SLEEP exit-2 from an
// OpenDevice-error exit-2 in `smartctl -A -n standby` output. Word
// boundaries avoid matching substrings like "asleep"/"sleeping".
var standbyRE = regexp.MustCompile(`(?i)\bSTANDBY\b|\bSLEEP\b`)
