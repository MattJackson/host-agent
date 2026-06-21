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

// learnEpoch is the learning-state schema/semantics version. BUMP IT whenever the
// learner's objective or curve placement changes in a way that makes previously
// persisted comfort values unsafe or meaningless to resume — on a mismatch
// loadBaseline DISCARDS the stale state and the box relearns from a clean scan.
// This is what lets a plain "push the new image" clean up the whole fleet: every
// box reads its old learned.json, sees the wrong epoch, drops it, and re-learns
// from the safe side. Routine releases that DON'T touch learning semantics keep
// the same epoch so hard-won learnings survive the upgrade.
//
// epoch 2 (v0.6.6): floor-guarded reclaim. Pre-epoch (epoch 0/absent) comforts
// were ratcheted up by the unguarded reclaim branch (docker-1 CPU→79) and MUST
// be discarded.
const learnEpoch = 2

type baseline struct {
	Epoch   int  `json:"epoch"`   // learnEpoch at save time; mismatch ⇒ discard + relearn
	Scanned bool `json:"scanned"` // true once the first-run box scan has placed comfort
	CPU     int  `json:"cpu"`
	GPU     int  `json:"gpu"`
	HDD     int  `json:"hdd"`
	SSD     int  `json:"ssd"`
}

// loadBaseline overlays any persisted learned comfort onto cfg (clamped to the
// safe envelope) and reports whether this box has already been scanned. No-op /
// returns false on first run (file absent) — cfg keeps its profile comfort and
// the caller runs the box scan.
func loadBaseline(path string, cfg *config.Config, logger controller.Logger) (scanned bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var bl baseline
	if err := json.Unmarshal(b, &bl); err != nil {
		logger.Printf("learn: ignoring unreadable %s: %v", path, err)
		return false
	}
	// Schema-epoch gate: stale-semantics state is discarded, not resumed. This is
	// the fleet-wide auto-cleanup — an old (pre-floor-guard) learned.json would
	// otherwise reload the drifted comforts that pinned fans low on a hot plant.
	if bl.Epoch != learnEpoch {
		logger.Printf("learn: discarding stale learned state (epoch %d != %d) — relearning from scan; %s", bl.Epoch, learnEpoch, path)
		_ = os.Remove(path)
		return false
	}
	apply := func(v int, dst *int, hi int) {
		if v >= learnComfortFloor && v <= hi {
			*dst = v
		}
	}
	apply(bl.CPU, &cfg.CPUComfort, cfg.CPUEmergency-1)
	apply(bl.GPU, &cfg.GPUComfort, cfg.GPUEmergency-1)
	apply(bl.HDD, &cfg.HDDComfort, cfg.HDDEmergency-1)
	apply(bl.SSD, &cfg.SSDComfort, cfg.SSDEmergency-1)
	logger.Printf("learn: restored baseline scanned=%v comfort cpu=%d gpu=%d hdd=%d ssd=%d (from %s)",
		bl.Scanned, cfg.CPUComfort, cfg.GPUComfort, cfg.HDDComfort, cfg.SSDComfort, path)
	return bl.Scanned
}

// saveBaseline atomically persists the current comfort + scanned flag.
func saveBaseline(path string, cfg *config.Config, scanned bool) error {
	b, err := json.Marshal(baseline{
		Epoch:   learnEpoch,
		Scanned: scanned,
		CPU:     cfg.CPUComfort, GPU: cfg.GPUComfort, HDD: cfg.HDDComfort, SSD: cfg.SSDComfort,
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
		p := learn.DefaultParams(learnComfortFloor, cl.emerg-1, float64(cfg.MinFan))
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
