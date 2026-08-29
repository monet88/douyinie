package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

func setupVisualTextService(t *testing.T) (*service.VisualTextService, *storage.DB, *cas.Store, string) {
	t.Helper()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}

	// Create test rights attestation + source asset
	ctx := context.Background()
	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              "ra-123",
		AttestationType: "TEST",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  "asset-123",
		SHA256:              "sha256_mock_video_asset",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "test.mp4",
		RightsAttestationID: "ra-123",
		CASPath:             filepath.Join(tmpDir, "test.mp4"),
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	svc := service.NewVisualTextService(db, casStore)
	return svc, db, casStore, asset.ID
}

func TestVisualTextService_DetectAndTrackText(t *testing.T) {
	svc, db, _, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()
	plan, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-1",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect and track text failed: %v", err)
	}

	if plan == nil {
		t.Fatal("expected non-nil plan")
	}

	if plan.AssetID != assetID {
		t.Errorf("got asset_id %q, want %q", plan.AssetID, assetID)
	}

	if len(plan.Regions) == 0 {
		t.Fatal("expected tracked regions in plan")
	}

	// Verify multi-role classification from default fake OCR scenario
	rolesFound := make(map[domain.TextRegionRole]bool)
	for _, reg := range plan.Regions {
		rolesFound[reg.Role] = true
	}

	if !rolesFound[domain.TextRoleBrandKeep] {
		t.Errorf("expected brand_keep role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleInstructionalUIText] {
		t.Errorf("expected instructional_ui_text role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleSpeechSubtitle] {
		t.Errorf("expected speech_subtitle role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleSemanticText] {
		t.Errorf("expected semantic_text role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleIgnoreNoise] {
		t.Errorf("expected ignore/noise role in plan, found: %+v", rolesFound)
	}

	// Verify linear interpolation of missing frame for subtitle
	for _, reg := range plan.Regions {
		if reg.Role == domain.TextRoleSpeechSubtitle {
			if len(reg.Keyframes) < 4 {
				t.Fatalf("expected at least 4 keyframes for subtitle, got %d", len(reg.Keyframes))
			}
			// Frame 2 was missing in raw detections; verify it exists as interpolated
			var frame2 *domain.RegionKeyframe
			for _, kf := range reg.Keyframes {
				if kf.FrameIndex == 2 {
					frame2 = &kf
					break
				}
			}
			if frame2 == nil {
				t.Fatalf("expected keyframe for frame 2")
			}
			if frame2.Observed {
				t.Errorf("expected frame 2 keyframe to be marked Observed=false (interpolated)")
			}
			if reg.ConfidenceEvidence.InterpolatedFrames != 1 {
				t.Errorf("got %d interpolated frames, want 1", reg.ConfidenceEvidence.InterpolatedFrames)
			}
		}
	}
}

func TestVisualTextService_ProvenanceCacheReuse(t *testing.T) {
	svc, db, _, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()
	reg := provider.NewSeam1FakeRegistry()
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	initCtx := context.Background()
	for _, p := range reg.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
	}
	credSvc := governance.NewCredentialService(db)
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)
	svc.ConfigureRouter(router)

	invokeCount := 0
	svc.OCRInvoke = func(ctx context.Context, p provider.Provider, req provider.OCRRequest) (*provider.OCRResult, error) {
		invokeCount++
		fake := provider.NewFakeOCRProvider("fake_paddle_ocr")
		return fake.DetectRegions(ctx, req)
	}

	// First execution (e.g. for VI job)
	plan1, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-vi",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if invokeCount != 1 {
		t.Errorf("expected 1 invocation, got %d", invokeCount)
	}

	// Second execution (e.g. for EN job reusing source-derived artifacts)
	plan2, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-en",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}

	// With router configured or cached provenance, plan should match
	if plan1.ProvenanceHash != plan2.ProvenanceHash {
		t.Errorf("expected identical provenance hash across language runs")
	}
	if plan1.CASHash != plan2.CASHash {
		t.Errorf("expected identical CAS hash: %s vs %s", plan1.CASHash, plan2.CASHash)
	}
}
