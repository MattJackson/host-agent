package livetargets

import (
	"sync"
	"testing"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/envelope"
)

func TestNew_Empty(t *testing.T) {
	s := New()
	if _, _, ok := s.Get(envelope.CPU); ok {
		t.Errorf("Get on empty store returned ok=true")
	}
}

func TestSetGet_Roundtrip(t *testing.T) {
	s := New()
	s.Set(envelope.PassiveGPU, 73, 4)
	target, deadband, ok := s.Get(envelope.PassiveGPU)
	if !ok {
		t.Fatal("ok=false after Set")
	}
	if target != 73 || deadband != 4 {
		t.Errorf("got (%d, %d), want (73, 4)", target, deadband)
	}
}

func TestSet_Overwrites(t *testing.T) {
	s := New()
	s.Set(envelope.CPU, 65, 3)
	s.Set(envelope.CPU, 68, 5)
	target, deadband, _ := s.Get(envelope.CPU)
	if target != 68 || deadband != 5 {
		t.Errorf("got (%d, %d), want (68, 5)", target, deadband)
	}
}

func TestApplyTo_AllFourClasses(t *testing.T) {
	s := New()
	s.Set(envelope.CPU, 65, 3)
	s.Set(envelope.PassiveGPU, 72, 4)
	s.Set(envelope.HDD, 38, 3)
	s.Set(envelope.SSD, 50, 3)

	cfg := &config.Config{
		// pre-seed with sentinel values to confirm ApplyTo overwrites them
		CPUTarget: 99, CPUDeadband: 99,
		GPUTarget: 99, GPUDeadband: 99,
		HDDTarget: 99, HDDDeadband: 99,
		SSDTarget: 99, SSDDeadband: 99,
	}
	s.ApplyTo(cfg)

	if cfg.CPUTarget != 65 || cfg.CPUDeadband != 3 {
		t.Errorf("CPU: got (%d, %d) want (65, 3)", cfg.CPUTarget, cfg.CPUDeadband)
	}
	if cfg.GPUTarget != 72 || cfg.GPUDeadband != 4 {
		t.Errorf("GPU: got (%d, %d) want (72, 4)", cfg.GPUTarget, cfg.GPUDeadband)
	}
	if cfg.HDDTarget != 38 || cfg.HDDDeadband != 3 {
		t.Errorf("HDD: got (%d, %d) want (38, 3)", cfg.HDDTarget, cfg.HDDDeadband)
	}
	if cfg.SSDTarget != 50 || cfg.SSDDeadband != 3 {
		t.Errorf("SSD: got (%d, %d) want (50, 3)", cfg.SSDTarget, cfg.SSDDeadband)
	}
}

func TestApplyTo_UnsetClass_LeavesCfgUntouched(t *testing.T) {
	s := New()
	s.Set(envelope.CPU, 65, 3) // only CPU set

	cfg := &config.Config{
		CPUTarget: 99, CPUDeadband: 99,
		GPUTarget: 88, GPUDeadband: 88, // sentinel — must survive ApplyTo
		HDDTarget: 77, HDDDeadband: 77,
		SSDTarget: 66, SSDDeadband: 66,
	}
	s.ApplyTo(cfg)

	if cfg.CPUTarget != 65 {
		t.Errorf("CPU not applied: got %d", cfg.CPUTarget)
	}
	if cfg.GPUTarget != 88 || cfg.GPUDeadband != 88 {
		t.Errorf("GPU clobbered by unset class: got (%d, %d) want (88, 88)", cfg.GPUTarget, cfg.GPUDeadband)
	}
	if cfg.HDDTarget != 77 {
		t.Errorf("HDD clobbered: got %d", cfg.HDDTarget)
	}
	if cfg.SSDTarget != 66 {
		t.Errorf("SSD clobbered: got %d", cfg.SSDTarget)
	}
}

func TestApplyTo_ActiveGPU_Ignored(t *testing.T) {
	// ActiveGPU has no chassis target. Set should still work (no
	// crash) but ApplyTo must not break.
	s := New()
	s.Set(envelope.ActiveGPU, 999, 999) // garbage values — must not leak into cfg

	cfg := &config.Config{CPUTarget: 55, GPUTarget: 75, HDDTarget: 38, SSDTarget: 50}
	s.ApplyTo(cfg) // should be no-op for ActiveGPU

	if cfg.CPUTarget != 55 || cfg.GPUTarget != 75 || cfg.HDDTarget != 38 || cfg.SSDTarget != 50 {
		t.Errorf("ActiveGPU Set leaked into cfg: %+v", cfg)
	}
}

func TestConcurrent_SetAndApply(t *testing.T) {
	// Race detector should catch any unprotected access. Spawn N
	// writers hammering Set and N readers calling Get + ApplyTo
	// concurrently.
	s := New()
	cfg := &config.Config{}
	const N = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s.Set(envelope.CPU, 60+(i%10), 3)
			s.Set(envelope.PassiveGPU, 70+(i%10), 4)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s.ApplyTo(cfg)
			_, _, _ = s.Get(envelope.CPU)
		}
	}()
	wg.Wait()
}
