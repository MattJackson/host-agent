package main

import (
	"context"
	"time"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/controller"
	"github.com/pq/docker-server/host-agent/internal/ipmi"
	"github.com/pq/docker-server/host-agent/internal/learn"
	"github.com/pq/docker-server/host-agent/internal/metrics"
	"github.com/pq/docker-server/host-agent/internal/sensors"
)

// writeScanMetrics keeps metrics.prom live DURING the scan — otherwise the
// setpoint/temp series freeze for the whole ~30-min scan and Grafana goes blind
// exactly when the fans are doing something dramatic. Source="scan" marks it.
func writeScanMetrics(cfg *config.Config, fanLvl int, r sensors.Reading) {
	_ = metrics.WriteAtomic(metricsFile, metrics.Snapshot{
		CurrentSpeed:        fanLvl,
		InEmergency:         0,
		CPUMax:              r.CPUMax,
		PassiveGPUMax:       r.PassiveGPUMax,
		ActiveGPUMax:        r.ActiveGPUMax,
		ActiveGPUFanMax:     r.ActiveGPUFanMax,
		HDDMax:              r.HDDMax,
		SSDMax:              r.SSDMax,
		CPUTarget:           cfg.CPUComfort,
		PassiveGPUTarget:    cfg.GPUComfort,
		HDDTarget:           cfg.HDDComfort,
		SSDTarget:           cfg.SSDComfort,
		CPUEmergency:        cfg.CPUEmergency,
		PassiveGPUEmergency: cfg.GPUEmergency,
		ActiveGPUEmergency:  cfg.ActiveGPUEmergency,
		HDDEmergency:        cfg.HDDEmergency,
		SSDEmergency:        cfg.SSDEmergency,
		Source:              "scan",
	})
}

// scanLevels are the fixed chassis-fan percentages the first-run box scan
// dwells at to learn each plant's fan→temp relationship. Ordered HIGH→LOW so the
// scan starts in the safe (coolest) state and steps the fan down; if a class
// approaches its emergency on the way down we stop descending (keeping the
// hotter, safe data points) rather than push it into a trip.
var scanLevels = []int{80, 55, 30}

const (
	scanFallbackMargin = 5 // FitComfort fallback = target - this
	scanApproachMargin = 3 // stop descending if any class >= emergency - this
)

// scanWorthwhile reports whether a class with a thermally-significant, slow
// plant is present — HDD/SSD/passive-GPU. A CPU-only box (fast plant, idle
// near ambient) gains nothing from a 30-min fan scan, so we skip it there.
func scanWorthwhile(r sensors.Reading) bool {
	return r.HDDMax > 0 || r.SSDMax > 0 || r.PassiveGPUMax > 0
}

// runBoxScan drives the chassis fan through scanLevels, dwelling at each so the
// slow plants settle, records each class's settled temperature, fits the
// fan→temp line, and places each class's curve comfort to hold its TARGET.
// Writes the learned comfort into cfg; returns true on a successful scan.
//
// Safety: emergency thresholds are checked every cycle — any class at/above its
// emergency aborts the scan immediately (fans 100%) and returns false. ctx
// cancellation aborts cleanly. On any abort the caller keeps profile-default
// comfort and the continuous learner trims from there.
func runBoxScan(ctx context.Context, logger controller.Logger, cfg *config.Config,
	reader controller.TempReader, ipmiClient *ipmi.Client, dwell time.Duration, intervalSec int) bool {

	interval := time.Duration(intervalSec) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}

	logger.Printf("box scan: no baseline — learning airflow at %v%% × %v each (one-time)", scanLevels, dwell)
	_ = ipmiClient.EngageManual(ctx)

	var cpu, gpu, hdd, ssd []learn.ScanPoint

	// hardEmerg / approaching report a class at/above its emergency, or within
	// scanApproachMargin of it. atEmerg = full abort; approaching = record this
	// level then stop descending (don't push a hot class lower into a trip).
	overBy := func(r sensors.Reading, back int) bool {
		return r.CPUMax >= cfg.CPUEmergency-back ||
			r.PassiveGPUMax >= cfg.GPUEmergency-back ||
			r.ActiveGPUMax >= cfg.ActiveGPUEmergency-back ||
			(r.HDDMax > 0 && r.HDDMax >= cfg.HDDEmergency-back) ||
			(r.SSDMax > 0 && r.SSDMax >= cfg.SSDEmergency-back)
	}

	for _, lvl := range scanLevels {
		if err := ipmiClient.SetFan(ctx, lvl); err != nil {
			logger.Printf("box scan: SetFan(%d%%) failed: %v — aborting", lvl, err)
			return false
		}
		logger.Printf("box scan: holding %d%% for %v", lvl, dwell)
		deadline := time.Now().Add(dwell)
		t := time.NewTicker(interval)
		var last sensors.Reading
		var have, approaching bool
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				t.Stop()
				return false
			case <-t.C:
			}
			reading, ok := reader.Read(ctx)
			if !ok {
				continue
			}
			if overBy(reading, 0) {
				logger.Printf("box scan: EMERGENCY during scan — aborting, fans 100%%")
				_ = ipmiClient.SetFan(ctx, 100)
				t.Stop()
				return false
			}
			_ = ipmiClient.SetFan(ctx, lvl) // re-assert vs BMC auto-revert
			writeScanMetrics(cfg, lvl, reading)
			last = reading
			have = true
			if overBy(reading, scanApproachMargin) {
				// A class is nearing emergency — this is as low as we dare go.
				// Record this level and stop descending.
				approaching = true
				break
			}
		}
		t.Stop()
		if !have {
			logger.Printf("box scan: no readings at %d%% — aborting", lvl)
			return false
		}
		if last.CPUMax > 0 {
			cpu = append(cpu, learn.ScanPoint{FanPct: lvl, TempC: float64(last.CPUMax)})
		}
		if last.PassiveGPUMax > 0 {
			gpu = append(gpu, learn.ScanPoint{FanPct: lvl, TempC: float64(last.PassiveGPUMax)})
		}
		if last.HDDMax > 0 {
			hdd = append(hdd, learn.ScanPoint{FanPct: lvl, TempC: float64(last.HDDMax)})
		}
		if last.SSDMax > 0 {
			ssd = append(ssd, learn.ScanPoint{FanPct: lvl, TempC: float64(last.SSDMax)})
		}
		logger.Printf("box scan: @%d%% settled cpu=%d gpu=%d hdd=%d ssd=%d",
			lvl, last.CPUMax, last.PassiveGPUMax, last.HDDMax, last.SSDMax)
		if approaching {
			logger.Printf("box scan: a class is approaching emergency — stopping descent at %d%%", lvl)
			break
		}
	}

	fit := func(name string, pts []learn.ScanPoint, target, emerg int, comfort *int) {
		if len(pts) == 0 || target <= 0 || emerg <= learnComfortFloor+1 {
			return
		}
		c := learn.FitComfort(pts, target, emerg, cfg.MinFan, cfg.MaxFan, learnComfortFloor, scanFallbackMargin)
		logger.Printf("box scan: fit %s → comfort %d°C (target %d, %d points)", name, c, target, len(pts))
		*comfort = c
	}
	fit("cpu", cpu, cfg.CPUTarget, cfg.CPUEmergency, &cfg.CPUComfort)
	fit("passive_gpu", gpu, cfg.GPUTarget, cfg.GPUEmergency, &cfg.GPUComfort)
	fit("hdd", hdd, cfg.HDDTarget, cfg.HDDEmergency, &cfg.HDDComfort)
	fit("ssd", ssd, cfg.SSDTarget, cfg.SSDEmergency, &cfg.SSDComfort)
	logger.Printf("box scan: complete — comfort cpu=%d gpu=%d hdd=%d ssd=%d",
		cfg.CPUComfort, cfg.GPUComfort, cfg.HDDComfort, cfg.SSDComfort)
	return true
}
