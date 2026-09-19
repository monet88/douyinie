// Package queue provides the persisted job queue service for RuntimeHost.
//
// The queue enforces the Phase 1 invariant of active_run_slots = 1: at most one
// run is running at a time; all others remain queued and survive daemon restart.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// ErrNotQueued indicates the run has no active (queued/running/paused) queue entry.
var ErrNotQueued = errors.New("run is not an active queue entry")

// ErrSlotBusy indicates a second run cannot start because active_run_slots=1.
var ErrSlotBusy = errors.New("another run is already running (active_run_slots=1)")

// Service coordinates the persisted queue backed by SQLite.
type Service struct {
	db *storage.DB
}

// NewService creates a queue service.
func NewService(db *storage.DB) *Service {
	return &Service{db: db}
}

// Enqueue appends a run to the queue in 'queued' status at the next position.
func (s *Service) Enqueue(ctx context.Context, runID, jobID string) (*domain.QueueEntry, error) {
	now := time.Now().UTC()
	entry := domain.QueueEntry{
		ID:         uuid.NewString(),
		RunID:      runID,
		JobID:      jobID,
		Status:     domain.RunStatusQueued,
		InsertedAt: now,
		UpdatedAt:  now,
	}
	pos, err := s.db.CreateQueueEntry(ctx, entry)
	if err != nil {
		return nil, fmt.Errorf("enqueue run: %w", err)
	}
	entry.Position = pos
	return &entry, nil
}

// List returns all queue entries ordered by position.
func (s *Service) List(ctx context.Context) ([]domain.QueueEntry, error) {
	entries, err := s.db.ListQueueEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list queue: %w", err)
	}
	if entries == nil {
		entries = []domain.QueueEntry{}
	}
	return entries, nil
}

// Reorder moves an active run to a new position, reassigning other active entries.
func (s *Service) Reorder(ctx context.Context, runID string, newPosition int) error {
	if newPosition < 1 {
		return fmt.Errorf("position must be >= 1")
	}
	if err := s.db.ReorderQueueEntries(ctx, runID, newPosition); err != nil {
		if errors.Is(err, storage.ErrNotActiveQueueEntry) {
			return fmt.Errorf("%w: %v", ErrNotQueued, err)
		}
		return fmt.Errorf("reorder queue: %w", err)
	}
	return nil
}

// Pause transitions a queued/running run to paused.
func (s *Service) Pause(ctx context.Context, runID string) error {
	e, err := s.db.GetQueueEntryByRunID(ctx, runID)
	if err != nil {
		return fmt.Errorf("lookup queue entry: %w", err)
	}
	if e.Status != domain.RunStatusQueued && e.Status != domain.RunStatusRunning {
		return fmt.Errorf("%w: status is %s", ErrNotQueued, e.Status)
	}
	if err := s.db.UpdateQueueStatus(ctx, runID, domain.RunStatusPaused, domain.RunStatusPaused); err != nil {
		return fmt.Errorf("pause run: %w", err)
	}
	return nil
}

// Cancel transitions any non-terminal run to cancelled. `interrupted` is recoverable (Resume
// re-drains it from the last incomplete stage), so abandoning it must stay possible; only the
// terminal statuses are refused.
func (s *Service) Cancel(ctx context.Context, runID string) error {
	e, err := s.db.GetQueueEntryByRunID(ctx, runID)
	if err != nil {
		return fmt.Errorf("lookup queue entry: %w", err)
	}
	if e.Status == domain.RunStatusCompleted || e.Status == domain.RunStatusCancelled {
		return fmt.Errorf("%w: status is %s", ErrNotQueued, e.Status)
	}
	if err := s.db.UpdateQueueStatus(ctx, runID, domain.RunStatusCancelled, domain.RunStatusCancelled); err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}
	return nil
}

// Resume transitions a paused or interrupted run back to queued.
func (s *Service) Resume(ctx context.Context, runID string) error {
	e, err := s.db.GetQueueEntryByRunID(ctx, runID)
	if err != nil {
		return fmt.Errorf("lookup queue entry: %w", err)
	}
	if e.Status != domain.RunStatusPaused && e.Status != domain.RunStatusInterrupted {
		return fmt.Errorf("%w: status is %s", ErrNotQueued, e.Status)
	}
	if err := s.db.UpdateQueueStatus(ctx, runID, domain.RunStatusQueued, domain.RunStatusQueued); err != nil {
		return fmt.Errorf("resume run: %w", err)
	}
	return nil
}

// Next returns the lowest-position queued entry (the next run eligible to start).
func (s *Service) Next(ctx context.Context) (*domain.QueueEntry, error) {
	e, err := s.db.NextQueuedEntry(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("next queued run: %w", err)
	}
	return e, nil
}

// MarkRunning transitions a queued run to running, enforcing active_run_slots=1:
// it fails if the run is not queued (ErrNotQueued) or another run is already
// running (ErrSlotBusy).
func (s *Service) MarkRunning(ctx context.Context, runID string) error {
	if err := s.db.MarkRunRunning(ctx, runID); err != nil {
		if errors.Is(err, storage.ErrSlotBusy) {
			return ErrSlotBusy
		}
		if errors.Is(err, storage.ErrNotActiveQueueEntry) {
			return fmt.Errorf("%w: %v", ErrNotQueued, err)
		}
		return err
	}
	return nil
}

// Recover transitions only the runs that were actively running to interrupted after
// a daemon crash. Queued never-started runs survive restart as queued. It returns the
// affected run IDs and is intended to run once at startup.
func (s *Service) Recover(ctx context.Context) ([]string, error) {
	runIDs, err := s.db.MarkAllActiveInterrupted(ctx)
	if err != nil {
		return nil, fmt.Errorf("recover queue: %w", err)
	}
	if runIDs == nil {
		runIDs = []string{}
	}
	return runIDs, nil
}
