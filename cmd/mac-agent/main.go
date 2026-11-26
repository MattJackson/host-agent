// mac-agent: single-binary host metrics pusher for macOS.
//
// Collects a node_exporter-compatible subset of host metrics (CPU, memory,
// root filesystem, load, boot time) using only the stock macOS tools
// (top, vm_stat, sysctl) and pushes them to Prometheus via remote_write with
// host=<short hostname>, the same label the Linux host-agent uses, so the
// existing server-overview dashboard picks the Mac up without changes.
//
// Stdlib only. Usage:
//
//	mac-agent run [--url URL]     run in the foreground
//	mac-agent install [--url URL] install a per-user LaunchAgent and start it
//
// The remote_write URL comes from --url or $PROMETHEUS_REMOTE_WRITE_URL and is
// required, matching the Linux host-agent (no built-in endpoint).
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "mac-0.1.0"

const (
	label           = "com.pq.host-agent"
	pushInterval    = 15 * time.Second
	commandTimeout  = 10 * time.Second
	defaultLogsPath = "Library/Logs/host-agent.log"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mac-agent run|install [--url URL]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	url := fs.String("url", os.Getenv("PROMETHEUS_REMOTE_WRITE_URL"), "Prometheus remote_write URL (required)")
	_ = fs.Parse(os.Args[2:])
	if *url == "" {
		fmt.Fprintln(os.Stderr, "mac-agent: set --url or PROMETHEUS_REMOTE_WRITE_URL")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		if err := run(*url); err != nil {
			log.Fatal(err)
		}
	case "install":
		if err := install(*url); err != nil {
			log.Fatal(err)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: mac-agent run|install [--url URL]")
		os.Exit(2)
	}
}

// run collects and pushes every pushInterval until the process is killed.
func run(url string) error {
	host, err := shortHostname()
	if err != nil {
		return err
	}
	log.Printf("mac-agent %s: host=%s url=%s", version, host, url)

	c := newCollector(host)
	client := &http.Client{Timeout: 15 * time.Second}
	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()

	for {
		series := c.collect(time.Now())
		if err := pushRemoteWrite(client, url, series, time.Now()); err != nil {
			log.Printf("push failed: %v", err)
		}
		<-ticker.C
	}
}

// shortHostname returns the first DNS label of the kernel hostname, matching
// `hostname -s` on the Linux hosts.
func shortHostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	if h == "" {
		return "", fmt.Errorf("empty hostname")
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// Collection
// ---------------------------------------------------------------------------

// series is one labelled sample. Labels includes __name__.
type series struct {
	labels map[string]string
	value  float64
}

type collector struct {
	host   string
	ncpu   int
	last   time.Time
	cpuSec map[string]float64 // key "cpu|mode" -> cumulative seconds
}

var (
	cpuRe      = regexp.MustCompile(`CPU usage: ([\d.]+)% user, ([\d.]+)% sys, ([\d.]+)% idle`)
	pageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)
	pagesRe    = regexp.MustCompile(`^(Pages [a-z -]+|File-backed pages):\s+(\d+)\.`)
	bootRe     = regexp.MustCompile(`sec = (\d+)`)
	loadRe     = regexp.MustCompile(`\{\s*([\d.]+)\s+([\d.]+)\s+([\d.]+)`)
)

func newCollector(host string) *collector {
	n := runtime.NumCPU()
	if out, err := run1("sysctl", "-n", "hw.logicalcpu"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && v > 0 {
			n = v
		}
	}
	return &collector{host: host, ncpu: n, cpuSec: map[string]float64{}}
}

// collect gathers one scrape worth of series. CPU is reported as cumulative
// seconds per core and mode (node_cpu_seconds_total) by integrating the
// percentages top reports over the elapsed time, so the dashboard's irate()
// queries work unchanged.
func (c *collector) collect(now time.Time) []series {
	var out []series
	base := func(name string, extra map[string]string) map[string]string {
		m := map[string]string{"__name__": name, "host": c.host}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	out = append(out, series{base("up", map[string]string{"job": "mac-agent"}), 1})
	out = append(out, series{base("host_agent_info", map[string]string{
		"version": version, "chassis": "mac", "profile": "macos",
	}), 1})

	// CPU. `top -l 2 -s 1` takes a 1s sample; the second sample is the
	// instantaneous reading.
	dt := 0.0
	if !c.last.IsZero() {
		dt = now.Sub(c.last).Seconds()
	}
	if user, sys, idle, err := cpuPercent(); err == nil && dt > 0 {
		for i := 0; i < c.ncpu; i++ {
			cpu := strconv.Itoa(i)
			c.cpuSec[cpu+"|user"] += user / 100 * dt
			c.cpuSec[cpu+"|system"] += sys / 100 * dt
			c.cpuSec[cpu+"|idle"] += idle / 100 * dt
		}
	}
	c.last = now
	for i := 0; i < c.ncpu; i++ {
		cpu := strconv.Itoa(i)
		for _, mode := range []string{"user", "system", "idle"} {
			out = append(out, series{base("node_cpu_seconds_total", map[string]string{
				"cpu": cpu, "mode": mode,
			}), c.cpuSec[cpu+"|"+mode]})
		}
	}

	// Memory.
	if total, avail, cached, err := memory(); err == nil {
		out = append(out,
			series{base("node_memory_MemTotal_bytes", nil), total},
			series{base("node_memory_MemAvailable_bytes", nil), avail},
			series{base("node_memory_Cached_bytes", nil), cached},
		)
	}

	// Root filesystem.
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		bs := float64(st.Bsize)
		fsLabels := map[string]string{"device": "rootfs", "mountpoint": "/", "fstype": "apfs"}
		out = append(out,
			series{base("node_filesystem_size_bytes", fsLabels), float64(st.Blocks) * bs},
			series{base("node_filesystem_avail_bytes", fsLabels), float64(st.Bavail) * bs},
		)
	}

	// Load average and boot time.
	if s, err := run1("sysctl", "-n", "vm.loadavg"); err == nil {
		if m := loadRe.FindStringSubmatch(s); m != nil {
			for i, name := range []string{"node_load1", "node_load5", "node_load15"} {
				v, _ := strconv.ParseFloat(m[i+1], 64)
				out = append(out, series{base(name, nil), v})
			}
		}
	}
	if s, err := run1("sysctl", "-n", "kern.boottime"); err == nil {
		if m := bootRe.FindStringSubmatch(s); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			out = append(out, series{base("node_boot_time_seconds", nil), v})
		}
	}

	// Temperatures (HID sensors, see sensors_darwin.go).
	for name, celsius := range readTemperatures() {
		out = append(out, series{base("node_hwmon_temp_celsius", map[string]string{
			"chip": "macos_hid", "sensor": sanitizeSensor(name),
		}), celsius})
	}

	// Network interface byte counters.
	if rx, tx, err := netCounters(); err == nil {
		for dev := range rx {
			out = append(out,
				series{base("node_network_receive_bytes_total", map[string]string{"device": dev}), rx[dev]},
				series{base("node_network_transmit_bytes_total", map[string]string{"device": dev}), tx[dev]},
			)
		}
	}

	return out
}

// sanitizeSensor turns a HID product name such as "PMU tdie1" into a label
// value with no spaces, matching the Linux hwmon sensor naming style.
func sanitizeSensor(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), "_"))
}

// netCounters parses `netstat -ibn` link rows (one per interface) for
// cumulative received and transmitted bytes. Loopback is skipped.
func netCounters() (rx, tx map[string]float64, err error) {
	out, err := run1("netstat", "-ibn")
	if err != nil {
		return nil, nil, err
	}
	rx, tx = map[string]float64{}, map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !strings.HasPrefix(f[2], "<Link#") || f[0] == "lo0" {
			continue
		}
		in, errIn := strconv.ParseFloat(f[6], 64)
		outB, errOut := strconv.ParseFloat(f[9], 64)
		if errIn != nil || errOut != nil {
			continue
		}
		rx[f[0]] = in
		tx[f[0]] = outB
	}
	return rx, tx, nil
}

func cpuPercent() (user, sys, idle float64, err error) {
	out, err := run1("top", "-l", "2", "-s", "1", "-n", "0")
	if err != nil {
		return 0, 0, 0, err
	}
	m := cpuRe.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return 0, 0, 0, fmt.Errorf("no CPU usage line in top output")
	}
	last := m[len(m)-1]
	user, _ = strconv.ParseFloat(last[1], 64)
	sys, _ = strconv.ParseFloat(last[2], 64)
	idle, _ = strconv.ParseFloat(last[3], 64)
	return user, sys, idle, nil
}

// memory returns total, available and file-backed (cached) bytes. Available
// is free + inactive + speculative pages, the same notion Activity Monitor
// and node_exporter's MemAvailable approximate.
func memory() (total, avail, cached float64, err error) {
	ms, err := run1("sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, 0, 0, err
	}
	total, _ = strconv.ParseFloat(strings.TrimSpace(ms), 64)

	vm, err := run1("vm_stat")
	if err != nil {
		return 0, 0, 0, err
	}
	pageSize := 16384.0
	if m := pageSizeRe.FindStringSubmatch(vm); m != nil {
		pageSize, _ = strconv.ParseFloat(m[1], 64)
	}
	pages := map[string]float64{}
	for _, line := range strings.Split(vm, "\n") {
		if m := pagesRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			v, _ := strconv.ParseFloat(m[2], 64)
			pages[m[1]] = v
		}
	}
	avail = (pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"]) * pageSize
	cached = pages["File-backed pages"] * pageSize
	if math.IsNaN(avail) {
		return 0, 0, 0, fmt.Errorf("bad vm_stat")
	}
	return total, avail, cached, nil
}

func run1(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// ---------------------------------------------------------------------------
// Prometheus remote_write (protobuf + snappy, hand-encoded)
// ---------------------------------------------------------------------------

func pushRemoteWrite(client *http.Client, url string, sers []series, ts time.Time) error {
	ms := ts.UnixMilli()
	var req []byte
	for _, s := range sers {
		req = appendBytesField(req, 1, encodeTimeSeries(s, ms))
	}
	body := snappyEncode(req)

	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote_write %s: HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// encodeTimeSeries: message TimeSeries { repeated Label labels = 1; repeated Sample samples = 2; }
func encodeTimeSeries(s series, ms int64) []byte {
	var b []byte
	names := make([]string, 0, len(s.labels))
	for k := range s.labels {
		names = append(names, k)
	}
	sort.Strings(names) // Prometheus requires sorted labels; "__name__" sorts first
	for _, k := range names {
		var l []byte
		l = appendBytesField(l, 1, []byte(k))
		l = appendBytesField(l, 2, []byte(s.labels[k]))
		b = appendBytesField(b, 1, l)
	}
	var smp []byte
	smp = appendTag(smp, 1, 1) // Sample.value: double, fixed64
	smp = appendFixed64(smp, math.Float64bits(s.value))
	smp = appendTag(smp, 2, 0) // Sample.timestamp: int64, varint
	smp = appendVarint(smp, uint64(ms))
	b = appendBytesField(b, 2, smp)
	return b
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field, wireType int) []byte {
	return appendVarint(b, uint64(field<<3|wireType))
}

func appendBytesField(b []byte, field int, data []byte) []byte {
	b = appendTag(b, field, 2)
	b = appendVarint(b, uint64(len(data)))
	return append(b, data...)
}

func appendFixed64(b []byte, v uint64) []byte {
	for i := 0; i < 8; i++ {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// snappyEncode produces a valid snappy block using only literal elements
// (no back-references). Remote-write receivers accept it; it is slightly
// larger than compressed output but needs no dependency.
func snappyEncode(src []byte) []byte {
	dst := appendVarint(nil, uint64(len(src)))
	for i := 0; i < len(src); {
		n := len(src) - i
		if n > 65536 {
			n = 65536
		}
		lit := src[i : i+n]
		l := len(lit) - 1
		switch {
		case l < 60:
			dst = append(dst, byte(l<<2))
		case l < 256:
			dst = append(dst, 60<<2, byte(l))
		default:
			dst = append(dst, 61<<2, byte(l), byte(l>>8))
		}
		dst = append(dst, lit...)
		i += n
	}
	return dst
}

// ---------------------------------------------------------------------------
// install: per-user LaunchAgent (runs at login, restarts on failure)
// ---------------------------------------------------------------------------

func install(url string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	logPath := filepath.Join(home, defaultLogsPath)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>--url</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, label, exe, url, logPath, logPath)

	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return err
	}

	uid := strconv.Itoa(os.Getuid())
	// Reload if already present; ignore the error when it is not loaded.
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+label).Run()
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("installed %s\nplist: %s\nlog:   %s\npushing to %s\n", label, plistPath, logPath, url)
	return nil
}
