package worker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/monet88/douyinie/internal/scheduler"
)

// GPULeaseManager adds deterministic VRAM reclamation to the single-GPU
// ResourceScheduler. A worker family may only be granted the lease after the
// previous holder has terminated and been reaped; this is what guarantees
// PyTorch/CUDA runtime caches are released before the next family starts.
type GPULeaseManager struct {
	scheduler *scheduler.Scheduler
	mu        sync.Mutex
	holder    string
	leaseID   string
	released  bool
	reclaimed time.Time
}

// NewGPULeaseManager wraps a scheduler.
func NewGPULeaseManager(s *scheduler.Scheduler) *GPULeaseManager {
	return &GPULeaseManager{scheduler: s}
}

// Acquire grants the single GPU lease for a family after confirming the
// previous family was fully reclaimed. The caller must have terminated and
// reaped the prior worker before calling Acquire; handoff is rejected if a
// worker is still marked active in the Supervisor.
func (m *GPULeaseManager) Acquire(ctx context.Context, family string, previous *Supervisor) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if previous != nil && previous.State() != LifecycleExited {
		return "", fmt.Errorf("cannot grant lease to %s: previous worker is still active (%s)", family, previous.State())
	}

	leaseID, err := m.scheduler.Acquire(ctx, family)
	if err != nil {
		return "", err
	}
	m.holder = family
	m.leaseID = leaseID
	m.released = false
	m.reclaimed = time.Time{}
	return leaseID, nil
}

// Release relinquishes the lease. It reports whether the lease was active and
// updates the deterministic reclamation timestamp.
func (m *GPULeaseManager) Release(ctx context.Context, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.leaseID != leaseID {
		return nil // stale release
	}
	if err := m.scheduler.Release(ctx, leaseID); err != nil {
		return err
	}
	m.holder = ""
	m.leaseID = ""
	m.released = true
	m.reclaimed = time.Now().UTC()
	return nil
}

// Holder returns the current lease holder family.
func (m *GPULeaseManager) Holder() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holder
}

// LastReclaimed returns when the last lease was fully released.
func (m *GPULeaseManager) LastReclaimed() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reclaimed
}

// SwitchFamilies is the high-level handoff operation: it releases the current
// lease, terminates/reaps the previous supervisor, waits until the process has
// exited, then acquires for the next family. A non-nil previous supervisor is
// required when a lease is held; nil is accepted only for the first acquire.
func (m *GPULeaseManager) SwitchFamilies(ctx context.Context, family string, previous *Supervisor, leaseID string) (string, error) {
	if previous != nil {
		if err := previous.Terminate(); err != nil {
			return "", fmt.Errorf("terminate previous worker: %w", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, TerminationTimeout)
		defer cancel()
		if err := waitForExit(waitCtx, previous); err != nil {
			return "", fmt.Errorf("reap previous worker: %w", err)
		}
	}

	if leaseID != "" {
		if err := m.Release(ctx, leaseID); err != nil {
			return "", fmt.Errorf("release previous lease: %w", err)
		}
	}

	return m.Acquire(ctx, family, previous)
}

func waitForExit(ctx context.Context, sup *Supervisor) error {
	done := make(chan error, 1)
	go func() {
		done <- sup.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return err
	}
}
