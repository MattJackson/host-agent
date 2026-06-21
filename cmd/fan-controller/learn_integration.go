package main

import (
	"encoding/json"
	"os"

	"github.com/pq/docker-server/host-agent/internal/adaptive"
	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/controller"
	"github.com/pq/docker-server/host-agent/internal/envelope"
	"github.com/pq/docker-server/host-agent/internal/learn"
)

// learnedStatePath persists the learned per-class curve comfort (ramp-start) so
// the agent resumes its converged operating point across restarts instead of
// re-learning from the profile default each boot.
const learnedStatePath = stateDir + "/learned.json"

// learnComfortFloor is the lowest the learner may push a comfort temperature.
// Below this the curve would ramp fans for a room-temperature class — pointless.
// The upper bound is per-class emergency-1 so the curve always reaches MaxFan by
// emergency.
const learnComfortFloor = 20

type learnedComfort struct {
	CPU int `json:"cpu"`
	GPU int `json:"gpu"`
	HDD int `json:"hdd"`
	SSD int `json:"ssd"`
}

// loadLearnedComfort overlays any persisted learned comfort onto cfg, clamped to
// the safe envelope. No-op on first run (file absent) — cfg keeps its profile
// comfort. Called once at startup, before the control loop.
func loadLearnedComfort(path string, cfg *config.Config, logger controller.Logger) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var lc learnedComfort
	if err := json.Unmarshal(b, &lc); err != nil {
		logger.Printf("learn: ignoring unreadable %s: %v", path, err)
		return
	}
	apply := func(v int, dst *int, hi int) {
		if v >= learnComfortFloor && v <= hi {
			*dst = v
		}
	}
	apply(lc.CPU, &cfg.CPUComfort, cfg.CPUEmergency-1)
	apply(lc.GPU, &cfg.GPUComfort, cfg.GPUEmergency-1)
	apply(lc.HDD, &cfg.HDDComfort, cfg.HDDEmergency-1)
	apply(lc.SSD, &cfg.SSDComfort, cfg.SSDEmergency-1)
	logger.Printf("learn: restored comfort cpu=%d gpu=%d hdd=%d ssd=%d (from %s)",
		cfg.CPUComfort, cfg.GPUComfort, cfg.HDDComfort, cfg.SSDComfort, path)
}

// saveLearnedComfort atomically persists the current learned comfort.
func saveLearnedComfort(path string, cfg *config.Config) error {
	b, err := json.Marshal(learnedComfort{
		CPU: cfg.CPUComfort, GPU: cfg.GPUComfort, HDD: cfg.HDDComfort, SSD: cfg.SSDComfort,
	})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runLearnTick runs the target-seeking learner once over every class, nudging
// each class's curve comfort toward holding steady-state == its TARGET at the
// minimum fan that maintains it. Returns true if any comfort changed.
//
// Runs in the SAME goroutine as the control cycle (called from the main loop on
// a slow cadence), so writing cfg.*Comfort here races nothing — the controller
// reads cfg in the same goroutine's next runCycle.
func runLearnTick(cfg *config.Config, obs *adaptive.Observer, logger controller.Logger) bool {
	type cls struct {
		name    string
		class   envelope.Class
		comfort *int
		target  int
		emerg   int
	}
	classes := []cls{
		{"cpu", envelope.CPU, &cfg.CPUComfort, cfg.CPUTarget, cfg.CPUEmergency},
		{"passive_gpu", envelope.PassiveGPU, &cfg.GPUComfort, cfg.GPUTarget, cfg.GPUEmergency},
		{"hdd", envelope.HDD, &cfg.HDDComfort, cfg.HDDTarget, cfg.HDDEmergency},
		{"ssd", envelope.SSD, &cfg.SSDComfort, cfg.SSDTarget, cfg.SSDEmergency},
	}
	changed := false
	for _, cl := range classes {
		if cl.target <= 0 || cl.emerg <= learnComfortFloor+1 {
			continue
		}
		st := obs.Stats(cl.class)
		if st.TempP50 <= 0 {
			continue // class absent or no data yet
		}
		p := learn.DefaultParams(learnComfortFloor, cl.emerg-1)
		d := learn.TargetSeek(st.TempP50, st.TempStdDev, st.FanDemandP90, *cl.comfort, cl.target, p)
		if d.Acted {
			logger.Printf("learn[%s]: steady=%.1f target=%d stddev=%.2f fanP90=%.0f → %s, comfort %d→%d",
				cl.name, st.TempP50, cl.target, st.TempStdDev, st.FanDemandP90, d.Reason, *cl.comfort, d.NewRampStart)
			*cl.comfort = d.NewRampStart
			changed = true
		}
	}
	return changed
}
