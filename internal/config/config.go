// Package config loads fan-controller profile files. The profile system
// mirrors `set -a; . profile.env; set +a` semantics with `${KEY:=value}`
// defaults: env > model-specific profile > default.env. First-set wins.
//
// We deliberately do NOT use a generic shell parser. The profile files
// are bounded to four supported line forms (see parseLine) and refuse
// to expand `$(…)`, arithmetic, or shell-variable references. Profiles
// are config, not code.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Config holds every tunable the controller and reads from profile env
// files. All thresholds are integers (matching bash's integer arithmetic);
// the floating-point gains are float64. Strings have no defaults here —
// the loader fills them in from the env or default.env.
type Config struct {
	// Loop cadence.
	IntervalSec int

	// CPU class — full PID.
	CPUTarget         int
	CPUDeadband       int
	CPUEmergency      int
	CPUApproachWindow int

	// Passive GPU class — full PID.
	GPUTarget         int
	GPUDeadband       int
	GPUEmergency      int
	GPUApproachWindow int

	// Active GPU class — assist only (no PID candidate), plus its own
	// emergency threshold.
	ActiveGPUTarget         int
	ActiveGPUEmergency      int
	ActiveGPUApproachWindow int

	// HDD class — full PID.
	HDDTarget         int
	HDDDeadband       int
	HDDEmergency      int
	HDDApproachWindow int
	HDDReadInterval  int // seconds between smartctl polls

	// SSD class — full PID (split off HDDs because their thermal envelope
	// is 10-15°C wider).
	SSDTarget         int
	SSDDeadband       int
	SSDEmergency      int
	SSDApproachWindow int

	// Fan system.
	MinFan            int
	MaxFan            int
	FanGain           float64
	DerivativeGain    float64
	AssistGain        float64
	DeadbandDriftRate int
	AdaptAlpha        float64

	// Probing.
	GPUAware string
	HDDAware string

	// Per-chassis IPMI sensor mapping — read by vmagent's run script in
	// the bash original. Loaded here for completeness even though the
	// controller itself doesn't use them.
	SensorCPU1Name    string
	SensorCPU1ID      string
	SensorCPU2Name    string
	SensorCPU2ID      string
	SensorInletName   string
	SensorInletID     string
	SensorExhaustName string
	SensorExhaustID   string

	// Raw is the full key/value map after profile resolution. Useful for
	// debugging and for future tunables the typed struct hasn't grown a
	// field for yet.
	Raw map[string]string
}

// Logger is the subset of stdlib *log.Logger that the loader needs.
// Mockable in tests.
type Logger interface {
	Printf(format string, v ...any)
}

// Load resolves profile precedence and returns a fully populated Config.
//
// Precedence (highest first), matching the bash `:=` semantics:
//  1. environment variables (lookupEnv)
//  2. $profileDir/$model.env
//  3. $profileDir/default.env
//
// "First-set wins": once a key has a value from a higher-precedence
// source, lower-precedence sources are ignored for that key.
func Load(profileDir, model string, lookupEnv func(string) (string, bool), logger Logger) (*Config, error) {
	raw := map[string]string{}

	// Helper: only set if not already set (matches `:=`).
	setDefault := func(k, v string) {
		if _, exists := raw[k]; !exists {
			raw[k] = v
		}
	}

	// 1. Env vars seed the map first — they always win against profile defaults.
	// We can't know up front which keys profiles will provide, so we
	// snapshot env when keys appear from profiles and use lookupEnv lazily.
	// Simpler approach: load profiles into a tentative map, then overlay
	// env (overriding) since env is highest precedence.
	loadProfile := func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return parseProfile(f, path, setDefault, logger)
	}

	// 2. Model-specific profile.
	if model != "" {
		modelPath := profileDir + "/" + model + ".env"
		if _, err := os.Stat(modelPath); err == nil {
			if logger != nil {
				logger.Printf("Loading profile: %s", model)
			}
			if err := loadProfile(modelPath); err != nil {
				return nil, fmt.Errorf("loading %s: %w", modelPath, err)
			}
		} else if logger != nil {
			logger.Printf("No profile for '%s' — using default", model)
		}
	}

	// 3. default.env fills the rest.
	defaultPath := profileDir + "/default.env"
	if _, err := os.Stat(defaultPath); err == nil {
		if err := loadProfile(defaultPath); err != nil {
			return nil, fmt.Errorf("loading %s: %w", defaultPath, err)
		}
	}

	// 4. Env overlay — highest precedence. Iterate over all keys in the
	// resolved map plus a few "always-checked" knobs the env can set
	// independently of any profile (GPU_AWARE, HDD_AWARE, PROFILE).
	envOverlay := func(k string) {
		if lookupEnv == nil {
			return
		}
		if v, ok := lookupEnv(k); ok && v != "" {
			raw[k] = v
		}
	}
	for k := range raw {
		envOverlay(k)
	}
	for _, k := range []string{"INTERVAL", "GPU_AWARE", "HDD_AWARE",
		"MIN_FAN", "MAX_FAN", "FAN_GAIN", "DERIVATIVE_GAIN", "ASSIST_GAIN",
		"DEADBAND_DRIFT_RATE", "ADAPT_ALPHA",
		"CPU_TARGET", "CPU_DEADBAND", "CPU_EMERGENCY", "CPU_APPROACH_WINDOW",
		"GPU_TARGET", "GPU_DEADBAND", "GPU_EMERGENCY", "GPU_APPROACH_WINDOW",
		"ACTIVE_GPU_TARGET", "ACTIVE_GPU_EMERGENCY", "ACTIVE_GPU_APPROACH_WINDOW",
		"HDD_TARGET", "HDD_DEADBAND", "HDD_EMERGENCY", "HDD_APPROACH_WINDOW", "HDD_READ_INTERVAL",
		"SSD_TARGET", "SSD_DEADBAND", "SSD_EMERGENCY", "SSD_APPROACH_WINDOW",
	} {
		envOverlay(k)
	}

	// Build typed Config from raw map.
	cfg := &Config{Raw: raw}
	bindInt(raw, "INTERVAL", &cfg.IntervalSec)
	bindString(raw, "GPU_AWARE", &cfg.GPUAware)
	bindString(raw, "HDD_AWARE", &cfg.HDDAware)

	bindInt(raw, "CPU_TARGET", &cfg.CPUTarget)
	bindInt(raw, "CPU_DEADBAND", &cfg.CPUDeadband)
	bindInt(raw, "CPU_EMERGENCY", &cfg.CPUEmergency)
	bindInt(raw, "CPU_APPROACH_WINDOW", &cfg.CPUApproachWindow)

	bindInt(raw, "GPU_TARGET", &cfg.GPUTarget)
	bindInt(raw, "GPU_DEADBAND", &cfg.GPUDeadband)
	bindInt(raw, "GPU_EMERGENCY", &cfg.GPUEmergency)
	bindInt(raw, "GPU_APPROACH_WINDOW", &cfg.GPUApproachWindow)

	bindInt(raw, "ACTIVE_GPU_TARGET", &cfg.ActiveGPUTarget)
	bindInt(raw, "ACTIVE_GPU_EMERGENCY", &cfg.ActiveGPUEmergency)
	bindInt(raw, "ACTIVE_GPU_APPROACH_WINDOW", &cfg.ActiveGPUApproachWindow)

	bindInt(raw, "HDD_TARGET", &cfg.HDDTarget)
	bindInt(raw, "HDD_DEADBAND", &cfg.HDDDeadband)
	bindInt(raw, "HDD_EMERGENCY", &cfg.HDDEmergency)
	bindInt(raw, "HDD_APPROACH_WINDOW", &cfg.HDDApproachWindow)
	bindInt(raw, "HDD_READ_INTERVAL", &cfg.HDDReadInterval)

	bindInt(raw, "SSD_TARGET", &cfg.SSDTarget)
	bindInt(raw, "SSD_DEADBAND", &cfg.SSDDeadband)
	bindInt(raw, "SSD_EMERGENCY", &cfg.SSDEmergency)
	bindInt(raw, "SSD_APPROACH_WINDOW", &cfg.SSDApproachWindow)

	bindInt(raw, "MIN_FAN", &cfg.MinFan)
	bindInt(raw, "MAX_FAN", &cfg.MaxFan)
	bindFloat(raw, "FAN_GAIN", &cfg.FanGain)
	bindFloat(raw, "DERIVATIVE_GAIN", &cfg.DerivativeGain)
	bindFloat(raw, "ASSIST_GAIN", &cfg.AssistGain)
	bindInt(raw, "DEADBAND_DRIFT_RATE", &cfg.DeadbandDriftRate)
	bindFloat(raw, "ADAPT_ALPHA", &cfg.AdaptAlpha)

	bindString(raw, "SENSOR_CPU1_NAME", &cfg.SensorCPU1Name)
	bindString(raw, "SENSOR_CPU1_ID", &cfg.SensorCPU1ID)
	bindString(raw, "SENSOR_CPU2_NAME", &cfg.SensorCPU2Name)
	bindString(raw, "SENSOR_CPU2_ID", &cfg.SensorCPU2ID)
	bindString(raw, "SENSOR_INLET_NAME", &cfg.SensorInletName)
	bindString(raw, "SENSOR_INLET_ID", &cfg.SensorInletID)
	bindString(raw, "SENSOR_EXHAUST_NAME", &cfg.SensorExhaustName)
	bindString(raw, "SENSOR_EXHAUST_ID", &cfg.SensorExhaustID)

	if cfg.GPUAware == "" {
		cfg.GPUAware = "auto"
	}
	if cfg.HDDAware == "" {
		cfg.HDDAware = "auto"
	}

	return cfg, nil
}

func bindInt(raw map[string]string, key string, dst *int) {
	if v, ok := raw[key]; ok {
		if i, err := strconv.Atoi(v); err == nil {
			*dst = i
		}
	}
}

func bindFloat(raw map[string]string, key string, dst *float64) {
	if v, ok := raw[key]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = f
		}
	}
}

func bindString(raw map[string]string, key string, dst *string) {
	if v, ok := raw[key]; ok {
		*dst = v
	}
}

// parseProfile reads from r and calls setDefault for each KEY=VAL line.
// Unrecognized lines emit a warning via logger but don't abort the load.
//
// Supported forms (one per line):
//
//	KEY=value
//	KEY="value"
//	KEY='value'
//	: "${KEY:=value}"     # the shell default-form
//	# comment              -> ignored
//	(blank)                -> ignored
//
// Anything else (function defs, command substitutions, $(...), arithmetic)
// is dropped with a warning. We DO NOT expand shell vars. Profiles are
// config, not code.
func parseProfile(r io.Reader, source string, setDefault func(string, string), logger Logger) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		key, val, ok := parseLine(trim)
		if !ok {
			if logger != nil {
				logger.Printf("WARN: %s:%d: unrecognized line, skipping: %s", source, lineNo, trim)
			}
			continue
		}
		setDefault(key, val)
	}
	return scanner.Err()
}

// parseLine returns key, value, true if the line is one of the
// supported forms.
func parseLine(line string) (string, string, bool) {
	// Strip inline comments — but only when outside quotes. The profile
	// files in this repo use trailing # comments on KEY=value lines.
	line = stripInlineComment(line)
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}

	// `: "${KEY:=value}"` — shell default-form.
	if strings.HasPrefix(line, ":") {
		// Find ${...} block.
		i := strings.Index(line, "${")
		j := strings.LastIndex(line, "}")
		if i < 0 || j < 0 || j <= i {
			return "", "", false
		}
		inner := line[i+2 : j]
		// inner = KEY:=value or KEY:-value
		sep := strings.Index(inner, ":=")
		if sep < 0 {
			sep = strings.Index(inner, ":-")
		}
		if sep < 0 {
			// Bare ${KEY} — no default. Not useful for a profile file.
			return "", "", false
		}
		key := strings.TrimSpace(inner[:sep])
		val := inner[sep+2:]
		val = unquote(val)
		if !validKey(key) {
			return "", "", false
		}
		return key, val, true
	}

	// `KEY=value` / `KEY="..."` / `KEY='...'`. (May be prefixed by
	// `export ` in some profile dialects — handle it.)
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}
	eq := strings.Index(line, "=")
	if eq <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:eq])
	val := line[eq+1:]
	if !validKey(key) {
		return "", "", false
	}
	// Refuse anything containing command substitution or arithmetic.
	if strings.Contains(val, "$(") || strings.Contains(val, "`") || strings.Contains(val, "$((") {
		return "", "", false
	}
	val = unquote(val)
	return key, val, true
}

// stripInlineComment removes a trailing `# …` comment from line, but
// only if the `#` is OUTSIDE any quoted region. Hash inside quotes is
// part of the value.
func stripInlineComment(line string) string {
	inSingle, inDouble := false, false
	for i, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				// Strip; trim trailing whitespace from what's left.
				return strings.TrimRight(line[:i], " \t")
			}
		}
	}
	return line
}

// unquote removes a single layer of matched single or double quotes.
// We do NOT expand shell variables — strings are taken literally.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// validKey enforces shell-style identifiers: [A-Za-z_][A-Za-z0-9_]*.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
