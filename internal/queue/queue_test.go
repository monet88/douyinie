package queue_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/storage"
)

func TestQueue_UpdateQueueStatus_InterruptedSnapsRunningStages(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Seed rights attestation, source asset, job, run for FK constraints
	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              uuid.NewString(),
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		Notes:           "test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  uuid.NewString(),
		SHA256:              strings.Repeat("1", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "clip.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "clip.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             uuid.NewString(),
		SourceAssetID:  asset.ID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{
		ID:        uuid.NewString(),
		JobID:     job.ID,
		Status:    domain.RunStatusRunning,
		CreatedAt: now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Insert stage execution in running state
	se := domain.StageExecution{
		ID:        uuid.NewString(),
		RunID:     run.ID,
		Stage:     "audio_mix",
		Status:    domain.StageStatusRunning,
		StartedAt: &now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateStageExecution(ctx, se); err != nil {
		t.Fatalf("create stage execution: %v", err)
	}

	// Transition queue status to interrupted
	if err := db.UpdateQueueStatus(ctx, run.ID, domain.RunStatusInterrupted, domain.RunStatusInterrupted); err != nil {
		t.Fatalf("update queue status: %v", err)
	}

	// Verify stage execution was snapped to interrupted
	stages, err := db.ListStageExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0].Status != domain.StageStatusInterrupted {
		t.Fatalf("expected stage status %s, got %s", domain.StageStatusInterrupted, stages[0].Status)
	}
}

func TestQueue_Resume_InterruptedRunGetsPosition(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              uuid.NewString(),
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		Notes:           "test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  uuid.NewString(),
		SHA256:              strings.Repeat("a", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "resume.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "resume.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}
	job := domain.LocalizationJob{
		ID:             "job-resume-test",
		SourceAssetID:  asset.ID,
		TargetLanguage: "vi",
		Status:         "created",
		CreatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{
		ID:        "run-resume-test",
		JobID:     job.ID,
		Status:    "running",
		CreatedAt: now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Interrupt the run (e.g. crash/unrecoverable worker timeout)
	if err := db.UpdateQueueStatus(ctx, run.ID, domain.RunStatusInterrupted, domain.RunStatusInterrupted); err != nil {
		t.Fatalf("update queue status to interrupted: %v", err)
	}

	entry, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status != domain.RunStatusInterrupted {
		t.Fatalf("expected status %s, got %s", domain.RunStatusInterrupted, entry.Status)
	}
	if entry.Position != 0 {
		t.Fatalf("expected null position (0 in struct), got %d", entry.Position)
	}

	// Resume interrupted run
	if err := qSvc.Resume(ctx, run.ID); err != nil {
		t.Fatalf("resume interrupted run: %v", err)
	}

	entryResumed, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get resumed queue entry: %v", err)
	}
	if entryResumed.Status != domain.RunStatusQueued {
		t.Fatalf("expected resumed status %s, got %s", domain.RunStatusQueued, entryResumed.Status)
	}
	if entryResumed.Position != 1 {
		t.Fatalf("expected assigned position 1, got %d", entryResumed.Position)
	}
}

// #108: an interrupted run is recoverable, not terminal - the operator console offers both Resume
// (re-drain the queue) and Cancel (abandon it). Only completed/cancelled are refused.
func TestQueue_Cancel_StatusContract(t *testing.T) {
	ctx := context.Background()

	t.Run("interrupted run is cancellable", func(t *testing.T) {
		db, qSvc, runID := newCancellableRun(t)
		if err := db.UpdateQueueStatus(ctx, runID, domain.RunStatusInterrupted, domain.RunStatusInterrupted); err != nil {
			t.Fatalf("interrupt run: %v", err)
		}

		if err := qSvc.Cancel(ctx, runID); err != nil {
			t.Fatalf("cancel interrupted run: %v", err)
		}
		assertRunLifecycleStatus(t, db, runID, domain.RunStatusCancelled)
	})

	t.Run("a resumed run stays cancellable", func(t *testing.T) {
		db, qSvc, runID := newCancellableRun(t)
		if err := db.UpdateQueueStatus(ctx, runID, domain.RunStatusInterrupted, domain.RunStatusInterrupted); err != nil {
			t.Fatalf("interrupt run: %v", err)
		}
		if err := qSvc.Resume(ctx, runID); err != nil {
			t.Fatalf("resume interrupted run: %v", err)
		}

		if err := qSvc.Cancel(ctx, runID); err != nil {
			t.Fatalf("cancel resumed run: %v", err)
		}
		assertRunLifecycleStatus(t, db, runID, domain.RunStatusCancelled)
	})

	t.Run("terminal runs are refused", func(t *testing.T) {
		for _, status := range []string{domain.RunStatusCompleted, domain.RunStatusCancelled} {
			db, qSvc, runID := newCancellableRun(t)
			if err := db.UpdateQueueStatus(ctx, runID, status, status); err != nil {
				t.Fatalf("move run to %s: %v", status, err)
			}

			if err := qSvc.Cancel(ctx, runID); !errors.Is(err, queue.ErrNotQueued) {
				t.Fatalf("expected ErrNotQueued cancelling a %s run, got %v", status, err)
			}
			assertRunLifecycleStatus(t, db, runID, status)
		}
	})

	t.Run("a completion between the cancellation's read and its guarded write keeps the run completed", func(t *testing.T) {
		db, qSvc, runID := newCancellableRun(t)

		// Step 1: the read a cancellation's precheck performs, seeing a status that still admits it.
		entry, err := db.GetQueueEntryByRunID(ctx, runID)
		if err != nil {
			t.Fatalf("read queue entry: %v", err)
		}
		if entry.Status != domain.RunStatusRunning {
			t.Fatalf("the cancellation must read an active run, got %s", entry.Status)
		}

		// Step 2: the completion commits between that read and the write. This ordering is the race
		// the precheck cannot see: it decided on a status the completion then replaced.
		if applied, err := db.CompleteQueueEntryIfActive(ctx, runID, false); err != nil || !applied {
			t.Fatalf("complete run between the read and the write: applied=%v err=%v", applied, err)
		}

		// Step 3: the write the cancellation issues next is refused by the state itself - the
		// precheck is not what protects the completion, the guarded write is - so the completion's
		// result stands and the loser cannot overwrite the winner.
		if applied, err := db.CancelQueueEntryIfNonTerminal(ctx, runID); err != nil {
			t.Fatalf("cancel after the completion: %v", err)
		} else if applied {
			t.Fatal("a cancellation that reaches its write after the completion must not be applied")
		}
		assertRunLifecycleStatus(t, db, runID, domain.RunStatusCompleted)

		// A later cancellation of the finished run is refused too, so the operator sees one answer
		// whichever half of the sequence refuses them.
		if err := qSvc.Cancel(ctx, runID); !errors.Is(err, queue.ErrNotQueued) {
			t.Fatalf("expected ErrNotQueued cancelling a completed run, got %v", err)
		}
		assertRunLifecycleStatus(t, db, runID, domain.RunStatusCompleted)
	})

	t.Run("a cancellation is refused while the run row already finished", func(t *testing.T) {
		db, qSvc, runID := newCancellableRun(t)
		// The run's two rows disagree - the entry still looks active while the run is finished - which
		// is the state the cancellation's write has to decide on its own: the entry half of the guard
		// admits it, and the run half must refuse it and roll the whole transition back rather than
		// abandon a run that is already completed.
		if err := db.UpdateQueueStatus(ctx, runID, domain.RunStatusPaused, domain.RunStatusCompleted); err != nil {
			t.Fatalf("move run row to completed: %v", err)
		}

		if err := qSvc.Cancel(ctx, runID); !errors.Is(err, queue.ErrNotQueued) {
			t.Fatalf("expected ErrNotQueued when the run row already finished, got %v", err)
		}
		run, err := db.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status != domain.RunStatusCompleted {
			t.Fatalf("the refused cancellation must not overwrite the finished run row, got %s", run.Status)
		}
	})
}

// newCancellableRun seeds an isolated db with a running run and its queue entry.
func newCancellableRun(t *testing.T) (*storage.DB, *queue.Service, string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "queue_cancel.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	runID := "run-" + uuid.NewString()
	ra := domain.RightsAttestation{
		ID:              "att-" + runID,
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		Notes:           "test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	asset := domain.SourceAsset{
		ID:                  "asset-" + runID,
		SHA256:              strings.Repeat("b", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "cancel.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "cancel.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}
	job := domain.LocalizationJob{
		ID:             "job-" + runID,
		SourceAssetID:  asset.ID,
		TargetLanguage: "vi",
		Status:         "created",
		CreatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := domain.LocalizationRun{ID: runID, JobID: job.ID, Status: domain.RunStatusRunning, CreatedAt: now}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, runID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, runID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	return db, qSvc, runID
}

// assertRunLifecycleStatus checks the queue entry and its run agree on the status an operator sees.
func assertRunLifecycleStatus(t *testing.T, db *storage.DB, runID, want string) {
	t.Helper()
	ctx := context.Background()
	entry, err := db.GetQueueEntryByRunID(ctx, runID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status != want {
		t.Fatalf("expected queue entry status %s, got %s", want, entry.Status)
	}
	run, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != want {
		t.Fatalf("expected run status %s, got %s", want, run.Status)
	}
}
