// Package livetargets is the thin concurrency-safe handoff between the
// adaptive reconciler (writer, every 10 min on its own goroutine) and
// the fan-controller's per-cycle config read (reader, every 15s on
// main goroutine).
//
// Without this layer the reconciler couldn't update cfg.{CPU,GPU,HDD,
// SSD}{Target,Deadband} safely — those fields are plain ints, and Go
// guarantees nothing about concurrent int read/write. livetargets.Set
// stages a new value under an RWMutex; the main goroutine calls
// ApplyTo(cfg) immediately before each PID cycle to copy the latest
// targets into cfg. Only one goroutine ever writes cfg directly.
package livetargets

import (
	"sync"

	"github.com/pq/docker-server/host-agent/internal/config"
	"github.com/pq/docker-server/host-agent/internal/envelope"
)

// classTarget is the (target, deadband) pair for one thermal class.
type classTarget struct {
	Target   int
	Deadband int
	// Set is true once Set() has been called for this class. Until
	// then, ApplyTo skips the class so unset classes don't clobber
	// existing cfg defaults with zero values.
	set bool
}

// Store holds the current target+deadband per thermal class. Safe for
// concurrent use: writers (reconciler) take the write lock via Set;
// readers (main goroutine via ApplyTo) take the read lock.
type Store struct {
	mu      sync.RWMutex
	targets map[envelope.Class]classTarget
}

// New returns an empty Store. Until Set is called for a class,
// ApplyTo is a no-op for that class (cfg keeps its existing value).
func New() *Store {
	return &Store{targets: map[envelope.Class]classTarget{}}
}

// Set stages a new (target, deadband) for class. Concurrent-safe.
func (s *Store) Set(class envelope.Class, target, deadband int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets[class] = classTarget{Target: target, Deadband: deadband, set: true}
}

// Get returns the current staged values for class. ok=false if Set
// was never called for that class.
func (s *Store) Get(class envelope.Class) (target, deadband int, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ct, present := s.targets[class]
	if !present || !ct.set {
		return 0, 0, false
	}
	return ct.Target, ct.Deadband, true
}

// ApplyTo writes the staged per-class targets into cfg. Called by the
// main goroutine immediately before each PID cycle. Classes that have
// never been Set leave cfg unchanged.
//
// Mapping:
//
//	envelope.CPU        → cfg.CPUTarget,        cfg.CPUDeadband
//	envelope.PassiveGPU → cfg.GPUTarget,        cfg.GPUDeadband
//	envelope.HDD        → cfg.HDDTarget,        cfg.HDDDeadband
//	envelope.SSD        → cfg.SSDTarget,        cfg.SSDDeadband
//
// ActiveGPU has no chassis target — skipped.
func (s *Store) ApplyTo(cfg *config.Config) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ct, ok := s.targets[envelope.CPU]; ok && ct.set {
		cfg.CPUTarget = ct.Target
		cfg.CPUDeadband = ct.Deadband
	}
	if ct, ok := s.targets[envelope.PassiveGPU]; ok && ct.set {
		cfg.GPUTarget = ct.Target
		cfg.GPUDeadband = ct.Deadband
	}
	if ct, ok := s.targets[envelope.HDD]; ok && ct.set {
		cfg.HDDTarget = ct.Target
		cfg.HDDDeadband = ct.Deadband
	}
	if ct, ok := s.targets[envelope.SSD]; ok && ct.set {
		cfg.SSDTarget = ct.Target
		cfg.SSDDeadband = ct.Deadband
	}
}
