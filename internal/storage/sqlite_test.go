package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
)

func TestStorage_SQLiteFlow(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_douyinie.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 1. Create RightsAttestation
	attestationID := uuid.NewString()
	ra := domain.RightsAttestation{
		ID:              attestationID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "operator-tester",
		TermsAccepted:   true,
		Notes:           "Test rights attestation",
		ConfirmedAt:     time.Now().UTC(),
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("CreateRightsAttestation failed: %v", err)
	}

	fetchedRA, err := db.GetRightsAttestation(ctx, attestationID)
	if err != nil {
		t.Fatalf("GetRightsAttestation failed: %v", err)
	}
	if fetchedRA.ID != ra.ID || fetchedRA.DeclaredBy != ra.DeclaredBy || !fetchedRA.TermsAccepted {
		t.Errorf("attestation mismatch: got %+v", fetchedRA)
	}

	// 2. Create SourceAsset
	assetID := uuid.NewString()
	sa := domain.SourceAsset{
		ID:                  assetID,
		SHA256:              "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "test_video.mp4",
		RightsAttestationID: attestationID,
		CASPath:             "cas/e3/b0/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.CreateSourceAsset(ctx, sa); err != nil {
		t.Fatalf("CreateSourceAsset failed: %v", err)
	}

	fetchedSA, err := db.GetSourceAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("GetSourceAsset failed: %v", err)
	}
	if fetchedSA.SHA256 != sa.SHA256 || fetchedSA.RightsAttestationID != attestationID {
		t.Errorf("source asset mismatch: got %+v", fetchedSA)
	}

	// Test foreign key constraint: attempting to insert an asset without valid attestation should fail
	badAsset := domain.SourceAsset{
		ID:                  uuid.NewString(),
		SHA256:              "1111111111111111111111111111111111111111111111111111111111111111",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "bad.mp4",
		RightsAttestationID: "non-existent-attestation-id",
		CASPath:             "cas/11/11/1111111111111111111111111111111111111111111111111111111111111111",
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.CreateSourceAsset(ctx, badAsset); err == nil {
		t.Errorf("expected error due to foreign key violation on non-existent attestation ID, got nil")
	}

	// 3. Save PreflightReport
	reportID := uuid.NewString()
	pr := domain.PreflightReport{
		ID:               reportID,
		AssetID:          assetID,
		DurationSec:      15.5,
		DurationMs:       15500,
		VideoCodec:       "h264",
		AudioCodec:       "aac",
		Width:            1080,
		Height:           1920,
		FrameRate:        30.0,
		AudioChannels:    2,
		AudioSampleRate:  44100,
		AudioBitRate:     128000,
		VideoBitRate:     2500000,
		ContainerFormat:  "mov,mp4,m4a,3gp,3g2,mj2",
		ContainerValid:   true,
		FingerprintMatch: true,
		Errors:           nil,
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, pr); err != nil {
		t.Fatalf("SavePreflightReport failed: %v", err)
	}

	fetchedPR, err := db.GetPreflightReport(ctx, assetID)
	if err != nil {
		t.Fatalf("GetPreflightReport failed: %v", err)
	}
	if fetchedPR.AssetID != assetID || fetchedPR.DurationMs != 15500 || !fetchedPR.ContainerValid {
		t.Errorf("preflight report mismatch: got %+v", fetchedPR)
	}

	// 4. Create LocalizationJob
	jobID := uuid.NewString()
	job := domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "pending",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	fetchedJob, err := db.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if fetchedJob.TargetLanguage != domain.TargetLanguageVI || fetchedJob.SourceAssetID != assetID {
		t.Errorf("job mismatch: got %+v", fetchedJob)
	}

	// 5. Create LocalizationRun
	runID := uuid.NewString()
	run := domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "queued",
		ConfigSnapshotJSON: `{"preset":"vietnam_standard","model":"vienew"}`,
		CreatedAt:          time.Now().UTC(),
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	fetchedRun, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if fetchedRun.JobID != jobID || fetchedRun.Status != "queued" {
		t.Errorf("run mismatch: got %+v", fetchedRun)
	}
}
