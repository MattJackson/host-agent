package sensors

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

// FS is a minimal filesystem read interface so tests can supply a
// fake /sys/class/hwmon tree. Production code passes os.DirFS("/").
type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

// CPU sources the hottest CPU die temp this cycle. Tries coretemp via
// /sys/class/hwmon first (universal on Intel hosts); falls back to
// IPMI entity 3.x ("Processor") if coretemp produced no data.
//
// Returns the (maxTemp, details, ok) triple. ok=false means BOTH
// sources failed — caller treats as full-failure ("temps not readable,
// fans 100% for safety" in the bash original).
type CPU struct {
	Filesystem FS
	Runner     runner.Runner
	// HwmonRoot defaults to "/sys/class/hwmon" but is overridable for
	// tests against a fake FS.
	HwmonRoot string
}

// NewCPU constructs a default CPU sensor against the real filesystem
// and a real subprocess runner.
func NewCPU(r runner.Runner, fsys FS) *CPU {
	return &CPU{
		Filesystem: fsys,
		Runner:     r,
		HwmonRoot:  "/sys/class/hwmon",
	}
}

// Read returns the max CPU die temp and a details string (e.g.
// "P0.t1:42 P0.t2:43 "). On total failure (no coretemp, no IPMI),
// returns max=0 and ok=false.
func (c *CPU) Read(ctx context.Context) (max int, details string, ok bool) {
	if m, d, ok := c.readCoretemp(); ok {
		return m, d, true
	}
	if m, d, ok := c.readIPMI(ctx); ok {
		return m, d, true
	}
	return 0, "", false
}

// readCoretemp walks /sys/class/hwmon* looking for entries with
// name="coretemp" and reads all temp*_input millidegree files.
//
// Bash:
//
//	for dir in /sys/class/hwmon/hwmon*; do
//	  name=$(cat $dir/name); [ "$name" = "coretemp" ] || continue
//	  pkg=${dir##*hwmon}
//	  for f in $dir/temp*_input; do
//	    mc=$(cat $f); t=$((mc / 1000))
//	    idx=${f##*temp}; idx=${idx%_input}
//	    details+="P${pkg}.t${idx}:${t} "
//	    [ $t -gt $max ] && max=$t
//	  done
//	done
func (c *CPU) readCoretemp() (int, string, bool) {
	entries, err := c.Filesystem.ReadDir(strings.TrimPrefix(c.HwmonRoot, "/"))
	if err != nil {
		return 0, "", false
	}
	var b detailsBuilder
	max := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "hwmon") {
			continue
		}
		dir := filepath.Join(c.HwmonRoot, e.Name())
		nameBytes, err := c.Filesystem.ReadFile(strings.TrimPrefix(filepath.Join(dir, "name"), "/"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(nameBytes)) != "coretemp" {
			continue
		}
		pkg := strings.TrimPrefix(e.Name(), "hwmon")
		// Find temp*_input files under this dir.
		subEntries, err := c.Filesystem.ReadDir(strings.TrimPrefix(dir, "/"))
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			n := sub.Name()
			if !strings.HasPrefix(n, "temp") || !strings.HasSuffix(n, "_input") {
				continue
			}
			mcBytes, err := c.Filesystem.ReadFile(strings.TrimPrefix(filepath.Join(dir, n), "/"))
			if err != nil {
				continue
			}
			mc, err := strconv.Atoi(strings.TrimSpace(string(mcBytes)))
			if err != nil {
				continue
			}
			t := mc / 1000
			idx := strings.TrimSuffix(strings.TrimPrefix(n, "temp"), "_input")
			b.Add(fmt.Sprintf("P%s.t%s:%d", pkg, idx, t))
			if t > max {
				max = t
			}
		}
	}
	if max == 0 {
		return 0, "", false
	}
	return max, b.String(), true
}

// IPMI SDR "type temperature" lookup. Entity ID 3.x = Processor.
// We use ipmitool, NOT freeipmi, to match the bash exactly.
//
//	$ ipmitool sdr type temperature
//	Inlet Temp       | 04h | ok  |  7.1 | 22 degrees C
//	CPU1 Temp        | 0Eh | ok  |  3.1 | 51 degrees C
//	CPU2 Temp        | 0Fh | ok  |  3.2 | 50 degrees C
//	CPU1 Temp        | 11h | ns  |  3.1 | Disabled
//
// Bash:
//
//	entity=$(echo $line | awk -F'|' '{gsub(/[[:space:]]/,"",$4); print $4}')
//	case $entity in 3.*) ;; *) continue ;; esac
//	case $line in *Disabled*) continue ;; esac
//	t=$(echo $line | grep -oP '\d+(?= degrees)')
var ipmiDegreesRE = regexp.MustCompile(`(\d+)\s+degrees`)

func (c *CPU) readIPMI(ctx context.Context) (int, string, bool) {
	out, err := c.Runner.Run(ctx, "ipmitool", "sdr", "type", "temperature")
	if err != nil || out == "" {
		return 0, "", false
	}
	var b detailsBuilder
	max := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			continue
		}
		entity := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, fields[3])
		if !strings.HasPrefix(entity, "3.") {
			continue
		}
		if strings.Contains(line, "Disabled") {
			continue
		}
		m := ipmiDegreesRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		b.Add(fmt.Sprintf("IPMI%s:%d", entity, t))
		if t > max {
			max = t
		}
	}
	if max == 0 {
		return 0, "", false
	}
	return max, b.String(), true
}
