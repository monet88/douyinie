// Package scheduler implements the ResourceScheduler: a single active GPU lease
// across the application (Phase 1 architecture §7.1). Worker families are granted
// one lease at a time; a second acquire is rejected until the first is released.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// ErrLeaseBusy indicates the GPU lease is already granted to another worker family.
var ErrLeaseBusy = errors.New("gpu lease is already granted to another worker family")

// GPUProfile describes the Phase 1 target resource profile (8 GB VRAM, single slot).
type GPUProfile struct {
	MemoryGB    int `json:"memory_gb"`
	ActiveSlots int `json:"active_slots"`
}

// Profile returns the fixed Phase 1 GPU profile.
func Profile() GPUProfile {
	return GPUProfile{MemoryGB: 8, ActiveSlots: 1}
}

// Scheduler grants and releases a single GPU lease for worker families.
type Scheduler struct {
	mu     sync.Mutex
	holder string // current lease holder family
	lease  string // current lease ID
}

// New creates a ResourceScheduler.
func New() *Scheduler {
	return &Scheduler{}
}

// Acquire grants the single GPU lease to the requested worker family.
// It returns an error (ErrLeaseBusy) if a lease is already held.
func (s *Scheduler) Acquire(ctx context.Context, family string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.holder != "" {
		return "", ErrLeaseBusy
	}
	if family == "" {
		return "", fmt.Errorf("worker family is required")
	}
	s.holder = family
	s.lease = uuid.NewString()
	return s.lease, nil
}

// Release relinquishes the lease. It is a no-op if the lease does not match the current holder.
func (s *Scheduler) Release(ctx context.Context, leaseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if leaseID == "" {
		return fmt.Errorf("lease_id is required")
	}
	if s.lease != leaseID {
		return nil // stale/duplicate release; lease already released or never matched
	}
	s.holder = ""
	s.lease = ""
	return nil
}

// Holder returns the current lease holder family ("" when idle).
func (s *Scheduler) Holder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holder
}
