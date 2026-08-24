package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/storage"
)

func TestIngestService_Flow(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Initialize CAS
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	// Initialize DB
	db, err := storage.Open(filepath.Join(tmpDir, "douyinie.db"))
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	defer db.Close()

	// Initialize Mock Prober
	prober := &media.MockProber{
		CustomReport: &domain.PreflightReport{
			DurationSec:      12.4,
			DurationMs:       12400,
			VideoCodec:       "h264",
			AudioCodec:       "aac",
			Width:            1080,
			Height:           1920,
			FrameRate:        30.0,
			AudioChannels:    2,
			AudioSampleRate:  44100,
			ContainerFormat:  "mp4",
			ContainerValid:   true,
			FingerprintMatch: true,
		},
	}

	service := NewIngestService(db, casStore, prober)

	// Create a dummy source media file
	mediaPath := filepath.Join(tmpDir, "sample_douyin.mp4")
	if err := os.WriteFile(mediaPath, []byte("fake mp4 media stream bytes 1234567890"), 0644); err != nil {
		t.Fatalf("failed to create sample media: %v", err)
	}

	// 1. Ingest without rights attestation should fail
	_, err = service.IngestLocalFile(ctx, IngestRequest{FilePath: mediaPath})
	if !errors.Is(err, domain.ErrRightsAttestationRequired) {
		t.Fatalf("expected ErrRightsAttestationRequired, got %v", err)
	}

	// 2. Ingest with embedded rights attestation
	req := IngestRequest{
		FilePath: mediaPath,
		Attestation: &domain.RightsAttestation{
			AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
			DeclaredBy:      "tester-agent",
			TermsAccepted:   true,
			Notes:           "Acceptance test file",
		},
	}

	result, err := service.IngestLocalFile(ctx, req)
	if err != nil {
		t.Fatalf("IngestLocalFile failed: %v", err)
	}

	if result.Asset == nil || result.PreflightReport == nil || result.Attestation == nil {
		t.Fatalf("incomplete ingest result: %+v", result)
	}

	if result.Asset.SHA256 == "" {
		t.Errorf("asset has empty SHA256")
	}
	if !casStore.Exists(result.Asset.SHA256) {
		t.Errorf("expected asset to exist in CAS: %s", result.Asset.SHA256)
	}

	// Verify persistence in SQLite
	persistedAsset, err := db.GetSourceAsset(ctx, result.Asset.ID)
	if err != nil {
		t.Fatalf("GetSourceAsset failed: %v", err)
	}
	if persistedAsset.RightsAttestationID != result.Attestation.ID {
		t.Errorf("attestation ID mismatch in asset: got %s, expected %s", persistedAsset.RightsAttestationID, result.Attestation.ID)
	}

	persistedReport, err := db.GetPreflightReport(ctx, result.Asset.ID)
	if err != nil {
		t.Fatalf("GetPreflightReport failed: %v", err)
	}
	if persistedReport.DurationMs != 12400 || persistedReport.VideoCodec != "h264" {
		t.Errorf("persisted preflight report mismatch: %+v", persistedReport)
	}

	// 3. Ingest with pre-created AttestationID for a different file
	mediaPath2 := filepath.Join(tmpDir, "sample_douyin_2.mp4")
	if err := os.WriteFile(mediaPath2, []byte("fake mp4 media stream bytes 2222222222"), 0644); err != nil {
		t.Fatalf("failed to create sample media 2: %v", err)
	}

	attestationID := uuid.NewString()
	err = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attestationID,
		AttestationType: "LICENSED_AUTHORIZED",
		DeclaredBy:      "licensee-user",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateRightsAttestation failed: %v", err)
	}

	result2, err := service.IngestLocalFile(ctx, IngestRequest{
		FilePath:      mediaPath2,
		AttestationID: attestationID,
	})
	if err != nil {
		t.Fatalf("Ingest with AttestationID failed: %v", err)
	}
	if result2.Attestation.ID != attestationID {
		t.Errorf("expected attestation ID %s, got %s", attestationID, result2.Attestation.ID)
	}
}

func TestIngestService_PreflightFailure_LeavesNoSourceAsset(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	db, err := storage.Open(filepath.Join(tmpDir, "douyinie.db"))
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	defer db.Close()

	// Prober reports corrupt container
	prober := &media.MockProber{
		CustomReport: &domain.PreflightReport{
			ContainerValid:   false,
			FingerprintMatch: true,
			Errors:           []string{"corrupt moov atom"},
		},
	}

	service := NewIngestService(db, casStore, prober)

	corruptPath := filepath.Join(tmpDir, "corrupt.mp4")
	if err := os.WriteFile(corruptPath, []byte("corrupt container bytes"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	_, err = service.IngestLocalFile(ctx, IngestRequest{
		FilePath: corruptPath,
		Attestation: &domain.RightsAttestation{
			DeclaredBy:    "operator",
			TermsAccepted: true,
		},
	})
	if err == nil {
		t.Fatalf("expected IngestLocalFile to fail on corrupt container, got nil")
	}
	if !errors.Is(err, domain.ErrCorruptMedia) {
		t.Errorf("expected ErrCorruptMedia, got %v", err)
	}

	// Verify that NO SourceAsset and NO RightsAttestation rows were inserted
	var assetCount, attestationCount int
	_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM source_assets").Scan(&assetCount)
	_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM rights_attestations").Scan(&attestationCount)

	if assetCount != 0 {
		t.Errorf("expected 0 source_assets in DB after preflight failure, found %d", assetCount)
	}
	if attestationCount != 0 {
		t.Errorf("expected 0 rights_attestations in DB after preflight failure, found %d", attestationCount)
	}
}

func TestIngestService_SHADedup_NoOrphanAttestation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	db, err := storage.Open(filepath.Join(tmpDir, "douyinie.db"))
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	defer db.Close()

	prober := &media.MockProber{
		CustomReport: &domain.PreflightReport{
			DurationSec:      10.0,
			DurationMs:       10000,
			VideoCodec:       "h264",
			AudioCodec:       "aac",
			ContainerValid:   true,
			FingerprintMatch: true,
		},
	}

	service := NewIngestService(db, casStore, prober)

	samplePath := filepath.Join(tmpDir, "identical_source.mp4")
	if err := os.WriteFile(samplePath, []byte("identical media payload 12345"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// First ingestion
	res1, err := service.IngestLocalFile(ctx, IngestRequest{
		FilePath: samplePath,
		Attestation: &domain.RightsAttestation{
			DeclaredBy:    "operator_1",
			TermsAccepted: true,
			Notes:         "Initial upload",
		},
	})
	if err != nil {
		t.Fatalf("first ingest failed: %v", err)
	}

	// Second ingestion of identical content with different proposed attestation
	res2, err := service.IngestLocalFile(ctx, IngestRequest{
		FilePath: samplePath,
		Attestation: &domain.RightsAttestation{
			DeclaredBy:    "operator_2_attempting_duplicate",
			TermsAccepted: true,
			Notes:         "Duplicate upload attempt",
		},
	})
	if err != nil {
		t.Fatalf("second ingest failed: %v", err)
	}

	// Verify SHA dedup returned existing SourceAsset and existing Attestation
	if res2.Asset.ID != res1.Asset.ID {
		t.Errorf("expected same Asset ID %s, got %s", res1.Asset.ID, res2.Asset.ID)
	}
	if res2.Attestation.ID != res1.Attestation.ID {
		t.Errorf("expected same Attestation ID %s, got %s", res1.Attestation.ID, res2.Attestation.ID)
	}
	if res2.Attestation.DeclaredBy != "operator_1" {
		t.Errorf("expected original attestation owner 'operator_1', got %s", res2.Attestation.DeclaredBy)
	}

	// Verify NO orphan attestation was created
	var attestationCount, assetCount int
	_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM rights_attestations").Scan(&attestationCount)
	_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM source_assets").Scan(&assetCount)

	if attestationCount != 1 {
		t.Errorf("expected exactly 1 rights_attestation row in DB (no orphan), found %d", attestationCount)
	}
	if assetCount != 1 {
		t.Errorf("expected exactly 1 source_asset row in DB, found %d", assetCount)
	}
}

func TestIngestService_UppercaseExtension_MimeFallback(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	casStore, _ := cas.NewStore(tmpDir)
	db, _ := storage.Open(filepath.Join(tmpDir, "douyinie.db"))
	defer db.Close()

	service := NewIngestService(db, casStore, &media.MockProber{})

	upperFile := filepath.Join(tmpDir, "VIDEO_UPPERCASE.MP4")
	_ = os.WriteFile(upperFile, []byte("fake mp4 content uppercase"), 0644)

	res, err := service.IngestLocalFile(ctx, IngestRequest{
		FilePath: upperFile,
		Attestation: &domain.RightsAttestation{
			DeclaredBy:    "operator",
			TermsAccepted: true,
		},
	})
	if err != nil {
		t.Fatalf("IngestLocalFile failed: %v", err)
	}

	if res.Asset.MimeType != "video/mp4" {
		t.Errorf("expected mime video/mp4, got %s", res.Asset.MimeType)
	}
}
