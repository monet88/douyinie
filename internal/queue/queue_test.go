package queue_test

import (
	"context"
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
