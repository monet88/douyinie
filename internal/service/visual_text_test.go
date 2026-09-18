package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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

func TestVisualTextService_SpatioTemporalOCRInstability_FilteredAsNoise(t *testing.T) {
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

	// Mock OCR returning:
	// 1. Spatio-temporal unstable pseudo-text in top-right (frames 10, 11, 12, changing gibberish, conf ~0.58)
	// 2. Stable unknown brand in top-left (frames 10, 11, 12, "NovaBrandX", conf ~0.58)
	// 3. Isolated single-frame unknown text in center (frame 10, "UniqueSign", conf ~0.58)
	svc.OCRInvoke = func(ctx context.Context, p provider.Provider, req provider.OCRRequest) (*provider.OCRResult, error) {
		return &provider.OCRResult{
			ProviderID:   "fake_ppocr",
			ModelName:    "paddleocr",
			ModelVersion: "v4",
			FrameWidth:   1920,
			FrameHeight:  1080,
			Detections: []provider.RawTextDetection{
				// Pseudo-text sequence (unstable across frames)
				{FrameIndex: 10, TimestampMs: 5000, Text: "XybVqwer", Box: domain.BoundingBox{X: 1650, Y: 185, Width: 125, Height: 25}, Confidence: 0.55},
				{FrameIndex: 11, TimestampMs: 5500, Text: "MnoPlkjh", Box: domain.BoundingBox{X: 1600, Y: 200, Width: 120, Height: 25}, Confidence: 0.58},
				{FrameIndex: 12, TimestampMs: 6000, Text: "ZopTyuik", Box: domain.BoundingBox{X: 1550, Y: 220, Width: 120, Height: 25}, Confidence: 0.57},
				// Stable unknown brand (same text across frames)
				{FrameIndex: 10, TimestampMs: 5000, Text: "NovaBrandX", Box: domain.BoundingBox{X: 500, Y: 300, Width: 150, Height: 40}, Confidence: 0.58},
				{FrameIndex: 11, TimestampMs: 5500, Text: "NovaBrandX", Box: domain.BoundingBox{X: 502, Y: 301, Width: 150, Height: 40}, Confidence: 0.58},
				{FrameIndex: 12, TimestampMs: 6000, Text: "NovaBrandX", Box: domain.BoundingBox{X: 504, Y: 302, Width: 150, Height: 40}, Confidence: 0.58},
				// Isolated single-frame unknown text
				{FrameIndex: 10, TimestampMs: 5000, Text: "UniqueSign", Box: domain.BoundingBox{X: 800, Y: 400, Width: 100, Height: 30}, Confidence: 0.58},
			},
		}, nil
	}

	plan, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:             "run-instability-test",
		AssetID:           assetID,
		FrameSampleStepMs: 500,
	})
	if err != nil {
		t.Fatalf("DetectAndTrackText failed: %v", err)
	}

	var foundUnstableAsNoise, foundStableBrandSemantic, foundIsolatedSemantic bool
	for _, reg := range plan.Regions {
		if reg.Text == "XybVqwer" || reg.Text == "MnoPlkjh" || reg.Text == "ZopTyuik" {
			if reg.Role != domain.TextRoleIgnoreNoise {
				t.Errorf("expected unstable pseudo-text %q to be IgnoreNoise, got %v", reg.Text, reg.Role)
			} else {
				foundUnstableAsNoise = true
			}
		}
		if reg.Text == "NovaBrandX" {
			if reg.Role != domain.TextRoleSemanticText {
				t.Errorf("expected stable brand %q to be SemanticText, got %v", reg.Text, reg.Role)
			} else {
				foundStableBrandSemantic = true
			}
		}
		if reg.Text == "UniqueSign" {
			if reg.Role != domain.TextRoleSemanticText {
				t.Errorf("expected isolated text %q to be SemanticText, got %v", reg.Text, reg.Role)
			} else {
				foundIsolatedSemantic = true
			}
		}
	}

	if !foundUnstableAsNoise {
		t.Errorf("did not find unstable pseudo-text classified as IgnoreNoise")
	}
	if !foundStableBrandSemantic {
		t.Errorf("did not find stable brand classified as SemanticText")
	}
	if !foundIsolatedSemantic {
		t.Errorf("did not find isolated text classified as SemanticText")
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

func TestVisualTextService_LocalizeVisualTrack_And_Overrides(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()

	// Wire translation service with a mock/fake
	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: "Bước 1: Chuẩn bị nguyên liệu (đã dịch)",
				},
			},
		}, nil
	}
	svc.SetTranslationService(transSvc)

	// 1. Run detection first
	plan, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-track-1",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Generate LocalizedVisualTrack for VI
	visTrack, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("localize visual track failed: %v", err)
	}

	if visTrack.CASHash == "" {
		t.Error("expected non-empty CASHash for LocalizedVisualTrack")
	}
	if visTrack.SubtitleTrackCAS == "" {
		t.Error("expected non-empty SubtitleTrackCAS for LocalizedSubtitleTrack")
	}

	// Verify overlays contain semantic_text and instructional_ui_text
	foundUI := false
	foundSemantic := false
	for _, ov := range visTrack.Overlays {
		if ov.Role == domain.TextRoleInstructionalUIText {
			foundUI = true
			if ov.LocalizedText != "Xuất" {
				t.Errorf("expected standard instructional UI term 'Xuất' for '导出', got %q", ov.LocalizedText)
			}
			if !ov.IsCoverDefault {
				t.Errorf("expected IsCoverDefault=true for standard instructional UI overlay")
			}
		}
		if ov.Role == domain.TextRoleSemanticText {
			foundSemantic = true
			if ov.LocalizedText != "Bước 1: Chuẩn bị nguyên liệu (đã dịch)" {
				t.Errorf("expected translated semantic text, got %q", ov.LocalizedText)
			}
		}
	}
	if !foundUI {
		t.Errorf("expected instructional_ui_text overlay in visual track")
	}
	if !foundSemantic {
		t.Errorf("expected semantic_text overlay in visual track")
	}

	// 3. Test Direct Manipulation Overrides (Drag/Resize/Reclassify)
	newRole := domain.TextRoleSemanticText
	newText := "Nút Tùy Chỉnh"
	targetRegID := plan.Regions[0].ID

	visTrackWithOverride, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-2",
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID:  targetRegID,
				NewRole:   &newRole,
				NewText:   &newText,
				BoxDeltaX: 10,
				BoxDeltaY: 20,
				BoxDeltaW: 30,
				BoxDeltaH: 15,
			},
		},
	})
	if err != nil {
		t.Fatalf("localize visual track with override failed: %v", err)
	}

	if visTrackWithOverride.ProvenanceHash == visTrack.ProvenanceHash {
		t.Errorf("expected override to alter provenance hash")
	}

	// 4. Test Unknown Region Override ID fails closed
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-unk",
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "unknown-region-999",
			},
		},
	})
	if err == nil || !errors.Is(err, domain.ErrRegionOverrideInvalid) {
		t.Fatalf("expected ErrRegionOverrideInvalid for unknown region_id, got %v", err)
	}

	// 5. Test Drag/Resize Clamping to Frame Bounds
	clampedPlan, err := service.ApplyRegionOverrides(plan, []domain.RegionOverride{
		{
			RegionID:  targetRegID,
			BoxDeltaX: 99999, // Way past frame width
			BoxDeltaY: 99999,
			BoxDeltaW: 5000,
			BoxDeltaH: 5000,
		},
	})
	if err != nil {
		t.Fatalf("expected clamping to succeed, got %v", err)
	}
	var clampedReg *domain.TrackedTextRegion
	for _, r := range clampedPlan.Regions {
		if r.ID == targetRegID {
			clampedReg = &r
			break
		}
	}
	if clampedReg == nil || len(clampedReg.Keyframes) == 0 {
		t.Fatalf("expected clamped region keyframes")
	}
	box := clampedReg.Keyframes[0].Box
	if box.X >= plan.FrameWidth || box.Y >= plan.FrameHeight || box.X+box.Width > plan.FrameWidth || box.Y+box.Height > plan.FrameHeight {
		t.Errorf("expected box to clamp strictly within frame (%dx%d), got %+v", plan.FrameWidth, plan.FrameHeight, box)
	}

	// 6. Test Translation Failure Propagation
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		return nil, errors.New("simulated translation backend failure")
	}
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-fail",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected translation failure to propagate, got nil error")
	}

	// 7. Test Corrupt/Missing DubScript Artifact Fails Closed
	// Insert a corrupt dub script index pointing to non-existent CAS
	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             "dub-corrupt-1",
		AssetID:        assetID,
		RunID:          "run-track-1",
		TargetLanguage: "en",
		CASHash:        "non_existent_cas_hash_corrupt_test",
		ProvenanceHash: "prov-corrupt-1",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save corrupt dubscript index: %v", err)
	}

	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-corrupt",
		AssetID:        assetID,
		TargetLanguage: "en",
	})
	if err == nil {
		t.Fatalf("expected corrupt dubscript CAS artifact to fail closed, got nil error")
	}

	// 8. Test Missing TranslationService Fails Closed
	svcNoTrans := service.NewVisualTextService(db, casStore)
	_, err = svcNoTrans.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-notrans",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil || !errors.Is(err, domain.ErrTranslationFailed) {
		t.Fatalf("expected ErrTranslationFailed when TranslationService is missing, got %v", err)
	}
	// 9. Test SceneProtectedRegions (Upstream/Operator evidence like faces / tap targets)
	// Restore working translation invoke
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: "Bước 1: Chuẩn bị nguyên liệu (đã dịch)",
				},
			},
		}, nil
	}

	// Add face obstacle right at standard subtitle location (Y=1440)
	visTrackScene, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-scene",
		AssetID:        assetID,
		TargetLanguage: "vi",
		SceneProtectedRegions: []domain.SceneProtectedRegion{
			{
				Reason:  "face",
				StartMs: 0,
				EndMs:   5000,
				Box:     domain.BoundingBox{X: 100, Y: 1400, Width: 880, Height: 200},
			},
		},
	})
	if err != nil {
		t.Fatalf("expected safe repositioning around scene-protected face, got error: %v", err)
	}
	for _, cue := range visTrackScene.SubtitleCues {
		cueBox := domain.BoundingBox{X: cue.X, Y: cue.Y, Width: cue.Width, Height: cue.Height}
		faceBox := domain.BoundingBox{X: 100, Y: 1400, Width: 880, Height: 200}
		if domain.BoxesOverlap(cueBox, faceBox) {
			t.Errorf("expected subtitle cue to avoid scene-protected face, but overlapped: cue=%+v", cueBox)
		}
	}

	// 10. Test Overlay Collision with Scene-Protected Face Fails Closed
	// Place a face directly covering the semantic text overlay at Y=300 (where 'Bước 1' is)
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-track-overlay-collision",
		AssetID:        assetID,
		TargetLanguage: "vi",
		SceneProtectedRegions: []domain.SceneProtectedRegion{
			{
				Reason:  "face",
				StartMs: 0,
				EndMs:   5000,
				Box:     domain.BoundingBox{X: 50, Y: 250, Width: 800, Height: 200},
			},
		},
	})
	if err == nil || !errors.Is(err, domain.ErrSubtitleOverlapsProtectedRegion) {
		t.Fatalf("expected ErrSubtitleOverlapsProtectedRegion when overlay occludes face, got %v", err)
	}
}

func TestVisualTextService_LocalizeVisualTrack_EmptyDubScript_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()

	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		target := "Xuất"
		if strings.Contains(req.Segments[0].SourceText, "1") || strings.Contains(req.Segments[0].SourceText, "Bước") {
			target = "Bước 1: Chuẩn bị nguyên liệu (đã dịch)"
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: target,
				},
			},
		}, nil
	}
	svc.SetTranslationService(transSvc)

	// 1. Run detection first to have OCR regions in TextRegionPlan
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-empty-dub",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Put empty DubScriptVariant (0 segments) in CAS and register index
	emptyDub := domain.DubScriptVariant{
		ID:             "dub-empty-1",
		AssetID:        assetID,
		RunID:          "run-empty-dub",
		TargetLanguage: "vi",
		Segments:       []domain.DubScriptSegment{}, // 0 segments
		CreatedAt:      time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(emptyDub)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put empty dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             emptyDub.ID,
		AssetID:        assetID,
		RunID:          emptyDub.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-empty-dub",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// 3. LocalizeVisualTrack must fail closed and NOT fall back to OCR source text
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-empty-dub",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected empty dub script variant to fail closed, got nil error")
	}
}

func TestVisualTextService_LocalizeVisualTrack_MismatchOrStaleSemanticEvidence_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()

	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		target := "Xuất"
		if strings.Contains(req.Segments[0].SourceText, "1") || strings.Contains(req.Segments[0].SourceText, "Bước") {
			target = "Bước 1: Chuẩn bị nguyên liệu (đã dịch)"
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: target,
				},
			},
		}, nil
	}
	svc.SetTranslationService(transSvc)

	// 1. Detection
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-mismatch",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Put canonical TranslationVariant in CAS
	transVariant := domain.TranslationVariant{
		ID:             "trans-canon-1",
		AssetID:        assetID,
		RunID:          "run-mismatch",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{
				Index:      0,
				SourceText: "今天天气很好。",
				TargetText: "Hôm nay thời tiết rất đẹp.",
				StartMs:    0,
				EndMs:      3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	transBytes, _ := json.Marshal(transVariant)
	transObj, err := casStore.Put(bytes.NewReader(transBytes))
	if err != nil {
		t.Fatalf("put translation in CAS: %v", err)
	}
	err = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVariant.ID,
		AssetID:        assetID,
		RunID:          transVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        transObj.SHA256,
		ProvenanceHash: "prov-trans-mismatch",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}
	// 3. Put DubScriptVariant with mismatched MeaningText in CAS
	mismatchedDub := domain.DubScriptVariant{
		ID:                    "dub-mismatch-1",
		AssetID:               assetID,
		RunID:                 "run-mismatch",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transObj.SHA256,
		Segments: []domain.DubScriptSegment{
			{
				Index:       0,
				SourceText:  "今天天气很好。",
				MeaningText: "Văn bản ý nghĩa bị sai lệch hoàn toàn.", // Mismatch with canonical TargetText
				SpokenText:  "Thời tiết đẹp.",
				StartMs:     0,
				EndMs:       3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(mismatchedDub)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             mismatchedDub.ID,
		AssetID:        assetID,
		RunID:          mismatchedDub.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-mismatch",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// 4. LocalizeVisualTrack must fail closed on semantic mismatch
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-mismatch",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected semantic mismatch to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrMeaningPreservationFailed) {
		t.Fatalf("expected ErrMeaningPreservationFailed for semantic mismatch, got %v", err)
	}
}

func TestVisualTextService_LocalizeVisualTrack_CanonicalTranslationGrounding_Success(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()

	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		target := "Xuất"
		if strings.Contains(req.Segments[0].SourceText, "1") || strings.Contains(req.Segments[0].SourceText, "Bước") {
			target = "Bước 1: Chuẩn bị nguyên liệu (đã dịch)"
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: target,
				},
			},
		}, nil
	}
	svc.SetTranslationService(transSvc)

	// 1. Detection
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-grounding",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Canonical TranslationVariant with full meaning text
	canonicalMeaningText := "Bước 1: Chuẩn bị đầy đủ các nguyên liệu tươi ngon."
	transVariant := domain.TranslationVariant{
		ID:             "trans-canon-2",
		AssetID:        assetID,
		RunID:          "run-grounding",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{
				Index:      0,
				SourceText: "第一步：准备好所有新鲜食材。",
				TargetText: canonicalMeaningText,
				StartMs:    0,
				EndMs:      3000,
			},
		},
		CreatedAt: time.Now().UTC().Add(time.Second),
	}
	transBytes, _ := json.Marshal(transVariant)
	transObj, err := casStore.Put(bytes.NewReader(transBytes))
	if err != nil {
		t.Fatalf("put translation in CAS: %v", err)
	}

	err = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVariant.ID,
		AssetID:        assetID,
		RunID:          transVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        transObj.SHA256,
		ProvenanceHash: "prov-trans-grounding",
		CreatedAt:      transVariant.CreatedAt,
	})
	if err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}
	// 3. DubScriptVariant with shorten-first spoken adaptation
	shortenedSpokenText := "Bước 1: Chuẩn bị nguyên liệu."
	dubVariant := domain.DubScriptVariant{
		ID:                    "dub-canon-2",
		AssetID:               assetID,
		RunID:                 "run-grounding",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transObj.SHA256,
		Segments: []domain.DubScriptSegment{
			{
				Index:       0,
				SourceText:  "第一步：准备好所有新鲜食材。",
				MeaningText: canonicalMeaningText,
				SpokenText:  shortenedSpokenText, // Spoken adaptation
				StartMs:     0,
				EndMs:       3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          dubVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-grounding",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// Seed a newer run for the same asset/target. Selected-run localization must
	// not drift to these newer artifacts merely because they are latest.
	newerMeaningText := "SAI: nội dung từ run mới hơn"
	newerTranslation := domain.TranslationVariant{
		ID:             "trans-canon-newer-run",
		AssetID:        assetID,
		RunID:          "run-grounding-newer",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{{
			Index:      0,
			SourceText: "第一步：准备好所有新鲜食材。",
			TargetText: newerMeaningText,
			StartMs:    0,
			EndMs:      3000,
		}},
		CreatedAt: transVariant.CreatedAt.Add(time.Minute),
	}
	newerTransBytes, _ := json.Marshal(newerTranslation)
	newerTransObj, err := casStore.Put(bytes.NewReader(newerTransBytes))
	if err != nil {
		t.Fatalf("put newer-run translation in CAS: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             newerTranslation.ID,
		AssetID:        assetID,
		RunID:          newerTranslation.RunID,
		TargetLanguage: "vi",
		CASHash:        newerTransObj.SHA256,
		ProvenanceHash: "prov-trans-grounding-newer",
		CreatedAt:      newerTranslation.CreatedAt,
	}); err != nil {
		t.Fatalf("save newer-run translation index: %v", err)
	}

	newerDub := domain.DubScriptVariant{
		ID:                    "dub-canon-newer-run",
		AssetID:               assetID,
		RunID:                 newerTranslation.RunID,
		TargetLanguage:        "vi",
		TranslationVariantCAS: newerTransObj.SHA256,
		Segments: []domain.DubScriptSegment{{
			Index:       0,
			SourceText:  "第一步：准备好所有新鲜食材。",
			MeaningText: newerMeaningText,
			SpokenText:  newerMeaningText,
			StartMs:     0,
			EndMs:       3000,
		}},
		CreatedAt: newerTranslation.CreatedAt,
	}
	newerDubBytes, _ := json.Marshal(newerDub)
	newerDubObj, err := casStore.Put(bytes.NewReader(newerDubBytes))
	if err != nil {
		t.Fatalf("put newer-run dub script in CAS: %v", err)
	}
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             newerDub.ID,
		AssetID:        assetID,
		RunID:          newerDub.RunID,
		TargetLanguage: "vi",
		CASHash:        newerDubObj.SHA256,
		ProvenanceHash: "prov-dub-grounding-newer",
		CreatedAt:      newerDub.CreatedAt,
	}); err != nil {
		t.Fatalf("save newer-run dub script index: %v", err)
	}

	// 4. LocalizeVisualTrack must render the selected run's canonical
	// TranslationVariant text, never spokenText or the newer run's text.
	visTrack, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-grounding",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("localize visual track failed: %v", err)
	}

	if len(visTrack.SubtitleCues) != 1 {
		t.Fatalf("expected 1 subtitle cue, got %d", len(visTrack.SubtitleCues))
	}
	if visTrack.SubtitleCues[0].Text != canonicalMeaningText {
		t.Errorf("subtitle text %q != canonical TranslationVariant target text %q (must not use spokenText %q)",
			visTrack.SubtitleCues[0].Text, canonicalMeaningText, shortenedSpokenText)
	}
	if visTrack.SubtitleCues[0].Text == newerMeaningText {
		t.Fatalf("selected run localized using newer run artifact: %q", visTrack.SubtitleCues[0].Text)
	}
}

func TestVisualTextService_LocalizeVisualTrack_ExplicitTranslationCASSurvivesResumeWithNewerOverlayIndex(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-resume-pin",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	speechMeaning := "Bước 1: Chuẩn bị đầy đủ các nguyên liệu tươi ngon."
	speechVariant := domain.TranslationVariant{
		ID:             "trans-speech-resume-pin",
		AssetID:        assetID,
		RunID:          "run-resume-pin",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{{
			Index:      0,
			SourceText: "第一步：准备好所有新鲜食材。",
			TargetText: speechMeaning,
			StartMs:    0,
			EndMs:      3000,
		}},
		CreatedAt: time.Now().UTC(),
	}
	speechBytes, _ := json.Marshal(speechVariant)
	speechObj, err := casStore.Put(bytes.NewReader(speechBytes))
	if err != nil {
		t.Fatalf("put speech translation in CAS: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             speechVariant.ID,
		AssetID:        assetID,
		RunID:          speechVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        speechObj.SHA256,
		ProvenanceHash: "prov-speech-resume-pin",
		CreatedAt:      speechVariant.CreatedAt,
	}); err != nil {
		t.Fatalf("save speech translation index: %v", err)
	}

	dubVariant := domain.DubScriptVariant{
		ID:                    "dub-resume-pin",
		AssetID:               assetID,
		RunID:                 "run-resume-pin",
		TargetLanguage:        "vi",
		TranslationVariantCAS: speechObj.SHA256,
		Segments: []domain.DubScriptSegment{{
			Index:       0,
			SourceText:  "第一步：准备好所有新鲜食材。",
			MeaningText: speechMeaning,
			SpokenText:  "Bước 1: Chuẩn bị nguyên liệu.",
			StartMs:     0,
			EndMs:       3000,
		}},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          dubVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-dub-resume-pin",
		CreatedAt:      dubVariant.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// Simulate a previous visual-track attempt: an overlay translation becomes the
	// latest asset/language index, while the dub script still correctly references
	// the speech translation artifact that the caller passes explicitly on resume.
	overlayVariant := domain.TranslationVariant{
		ID:             "trans-overlay-newer",
		AssetID:        assetID,
		RunID:          "run-resume-pin",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{{
			Index:      0,
			SourceText: "屏幕文字",
			TargetText: "Văn bản trên màn hình",
		}},
		CreatedAt: speechVariant.CreatedAt.Add(time.Second),
	}
	overlayBytes, _ := json.Marshal(overlayVariant)
	overlayObj, err := casStore.Put(bytes.NewReader(overlayBytes))
	if err != nil {
		t.Fatalf("put overlay translation in CAS: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             overlayVariant.ID,
		AssetID:        assetID,
		RunID:          overlayVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        overlayObj.SHA256,
		ProvenanceHash: "prov-overlay-newer",
		CreatedAt:      overlayVariant.CreatedAt,
	}); err != nil {
		t.Fatalf("save overlay translation index: %v", err)
	}

	visTrack, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:                 "run-resume-pin",
		AssetID:               assetID,
		TargetLanguage:        "vi",
		TranslationVariantCAS: speechObj.SHA256,
	})
	if err != nil {
		t.Fatalf("explicit speech translation CAS must survive newer overlay index on resume: %v", err)
	}
	if len(visTrack.SubtitleCues) != 1 {
		t.Fatalf("expected 1 subtitle cue, got %d", len(visTrack.SubtitleCues))
	}
	if visTrack.SubtitleCues[0].Text != speechMeaning {
		t.Fatalf("subtitle text %q != explicitly pinned speech translation %q", visTrack.SubtitleCues[0].Text, speechMeaning)
	}
}

func setupTestTranslationService(db *storage.DB, casStore *cas.Store, svc *service.VisualTextService) {
	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		target := "Xuất"
		if strings.Contains(req.Segments[0].SourceText, "1") || strings.Contains(req.Segments[0].SourceText, "Bước") {
			target = "Bước 1: Chuẩn bị nguyên liệu (đã dịch)"
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: target,
				},
			},
		}, nil
	}
	svc.SetTranslationService(transSvc)
}

func TestVisualTextService_LocalizeVisualTrack_StaleEmbeddedTranslationCAS_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()

	// 1. Detection
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-stale",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Old / Stale TranslationVariant in CAS (matching dub script segment)
	staleMeaningText := "Bản dịch cũ đã lỗi thời."
	staleTrans := domain.TranslationVariant{
		ID:             "trans-stale-1",
		AssetID:        assetID,
		RunID:          "run-stale",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{
				Index:      0,
				SourceText: "第一步：准备好所有新鲜食材。",
				TargetText: staleMeaningText,
				StartMs:    0,
				EndMs:      3000,
			},
		},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	staleBytes, _ := json.Marshal(staleTrans)
	staleObj, err := casStore.Put(bytes.NewReader(staleBytes))
	if err != nil {
		t.Fatalf("put stale translation in CAS: %v", err)
	}

	// 3. New Canonical TranslationVariant in CAS and registered in Index
	currentMeaningText := "Bước 1: Chuẩn bị đầy đủ các nguyên liệu tươi ngon nhất."
	currentTrans := domain.TranslationVariant{
		ID:             "trans-current-1",
		AssetID:        assetID,
		RunID:          "run-stale",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{
				Index:      0,
				SourceText: "第一步：准备好所有新鲜食材。",
				TargetText: currentMeaningText,
				StartMs:    0,
				EndMs:      3000,
			},
		},
		CreatedAt: time.Now().UTC().Add(time.Second),
	}
	currentBytes, _ := json.Marshal(currentTrans)
	currentObj, err := casStore.Put(bytes.NewReader(currentBytes))
	if err != nil {
		t.Fatalf("put current translation in CAS: %v", err)
	}

	err = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             currentTrans.ID,
		AssetID:        assetID,
		RunID:          currentTrans.RunID,
		TargetLanguage: "vi",
		CASHash:        currentObj.SHA256,
		ProvenanceHash: "prov-trans-current",
		CreatedAt:      currentTrans.CreatedAt,
	})
	if err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}

	// 4. DubScriptVariant with stale TranslationVariantCAS embedded (matches staleObj, differs from current indexed TranslationVariant)
	dubVariant := domain.DubScriptVariant{
		ID:                    "dub-stale-1",
		AssetID:               assetID,
		RunID:                 "run-stale",
		TargetLanguage:        "vi",
		TranslationVariantCAS: staleObj.SHA256, // Stale embedded CAS != current indexed CAS
		Segments: []domain.DubScriptSegment{
			{
				Index:       0,
				SourceText:  "第一步：准备好所有新鲜食材。",
				MeaningText: staleMeaningText,
				SpokenText:  "Bản dịch cũ.",
				StartMs:     0,
				EndMs:       3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          dubVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-dub-stale",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// 5. LocalizeVisualTrack must fail closed because embedded CAS != current index CAS
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-stale",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected stale embedded TranslationVariantCAS to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrMeaningPreservationFailed) {
		t.Fatalf("expected ErrMeaningPreservationFailed for stale embedded CAS mismatch, got %v", err)
	}
}

func TestVisualTextService_LocalizeVisualTrack_SegmentCountMismatch_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()

	// 1. Detection
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-seg-count-mismatch",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Canonical TranslationVariant with 2 segments
	transVariant := domain.TranslationVariant{
		ID:             "trans-count-2",
		AssetID:        assetID,
		RunID:          "run-seg-count-mismatch",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{
				Index:      0,
				SourceText: "第一步：准备好所有新鲜食材。",
				TargetText: "Bước 1: Chuẩn bị nguyên liệu.",
				StartMs:    0,
				EndMs:      3000,
			},
			{
				Index:      1,
				SourceText: "第二步：开始烹饪。",
				TargetText: "Bước 2: Bắt đầu nấu ăn.",
				StartMs:    3000,
				EndMs:      6000,
			},
		},
		CreatedAt: time.Now().UTC().Add(time.Second),
	}
	transBytes, _ := json.Marshal(transVariant)
	transObj, err := casStore.Put(bytes.NewReader(transBytes))
	if err != nil {
		t.Fatalf("put translation in CAS: %v", err)
	}

	err = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVariant.ID,
		AssetID:        assetID,
		RunID:          transVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        transObj.SHA256,
		ProvenanceHash: "prov-trans-count-2",
		CreatedAt:      transVariant.CreatedAt,
	})
	if err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}

	// 3. DubScriptVariant with only 1 segment (count mismatch: 1 != 2)
	dubVariant := domain.DubScriptVariant{
		ID:                    "dub-count-1",
		AssetID:               assetID,
		RunID:                 "run-seg-count-mismatch",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transObj.SHA256,
		Segments: []domain.DubScriptSegment{
			{
				Index:       0,
				SourceText:  "第一步：准备好所有新鲜食材。",
				MeaningText: "Bước 1: Chuẩn bị nguyên liệu.",
				SpokenText:  "Bước 1 chuẩn bị.",
				StartMs:     0,
				EndMs:       3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          dubVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-dub-count-1",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// 4. LocalizeVisualTrack must fail closed on segment count mismatch
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-seg-count-mismatch",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected segment count mismatch to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrMeaningPreservationFailed) {
		t.Fatalf("expected ErrMeaningPreservationFailed for segment count mismatch, got %v", err)
	}
}

func TestVisualTextService_LocalizeVisualTrack_SegmentIndexMismatch_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()

	// 1. Detection
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-seg-idx-mismatch",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Canonical TranslationVariant with segment index 0
	transVariant := domain.TranslationVariant{
		ID:             "trans-idx-0",
		AssetID:        assetID,
		RunID:          "run-seg-idx-mismatch",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{
				Index:      0,
				SourceText: "第一步：准备好所有新鲜食材。",
				TargetText: "Bước 1: Chuẩn bị nguyên liệu.",
				StartMs:    0,
				EndMs:      3000,
			},
		},
		CreatedAt: time.Now().UTC().Add(time.Second),
	}
	transBytes, _ := json.Marshal(transVariant)
	transObj, err := casStore.Put(bytes.NewReader(transBytes))
	if err != nil {
		t.Fatalf("put translation in CAS: %v", err)
	}

	err = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVariant.ID,
		AssetID:        assetID,
		RunID:          transVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        transObj.SHA256,
		ProvenanceHash: "prov-trans-idx-0",
		CreatedAt:      transVariant.CreatedAt,
	})
	if err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}

	// 3. DubScriptVariant with segment index 1 (index mismatch: 1 != 0)
	dubVariant := domain.DubScriptVariant{
		ID:                    "dub-idx-1",
		AssetID:               assetID,
		RunID:                 "run-seg-idx-mismatch",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transObj.SHA256,
		Segments: []domain.DubScriptSegment{
			{
				Index:       1, // Mismatched Index (1 != 0)
				SourceText:  "第一步：准备好所有新鲜食材。",
				MeaningText: "Bước 1: Chuẩn bị nguyên liệu.",
				SpokenText:  "Bước 1 chuẩn bị.",
				StartMs:     0,
				EndMs:       3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          dubVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-dub-idx-1",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// 4. LocalizeVisualTrack must fail closed on segment index mismatch
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-seg-idx-mismatch",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected segment index mismatch to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrMeaningPreservationFailed) {
		t.Fatalf("expected ErrMeaningPreservationFailed for segment index mismatch, got %v", err)
	}
}

func TestVisualTextService_LocalizeVisualTrack_EmptyTranslationIndexCASHash_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()

	// 1. Detection
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-empty-trans-hash",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. TranslationVariantIndex with empty CASHash
	err = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             "trans-empty-hash",
		AssetID:        assetID,
		RunID:          "run-empty-trans-hash",
		TargetLanguage: "vi",
		CASHash:        "", // Empty CAS hash
		ProvenanceHash: "prov-trans-empty-hash",
		CreatedAt:      time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}

	// 3. DubScriptVariant in CAS and Index
	dubVariant := domain.DubScriptVariant{
		ID:                    "dub-empty-trans-hash",
		AssetID:               assetID,
		RunID:                 "run-empty-trans-hash",
		TargetLanguage:        "vi",
		TranslationVariantCAS: "some_dummy_cas",
		Segments: []domain.DubScriptSegment{
			{
				Index:       0,
				SourceText:  "第一步：准备好所有新鲜食材。",
				MeaningText: "Bước 1: Chuẩn bị nguyên liệu.",
				SpokenText:  "Bước 1 chuẩn bị.",
				StartMs:     0,
				EndMs:       3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatalf("put dub script in CAS: %v", err)
	}

	err = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          dubVariant.RunID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		ProvenanceHash: "prov-dub-empty-trans-hash",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// 4. LocalizeVisualTrack must fail closed when TranslationVariantIndex has empty CASHash
	_, err = svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-empty-trans-hash",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err == nil {
		t.Fatalf("expected empty TranslationVariantIndex CASHash to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrTranslationVariantNotFound) {
		t.Fatalf("expected ErrTranslationVariantNotFound for empty translation CASHash, got %v", err)
	}
}

func TestVisualTextService_LocalizeVisualTrack_ForwardsRoutingContextToTranslationJobInput(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()

	ctx := context.Background()

	var capturedInputs []domain.TranslationJobInput
	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		capturedInputs = append(capturedInputs, req)
		target := "Xuất"
		if strings.Contains(req.Segments[0].SourceText, "1") || strings.Contains(req.Segments[0].SourceText, "Bước") {
			target = "Bước 1: Chuẩn bị nguyên liệu (đã dịch)"
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments: []domain.TranslationSegment{
				{
					Index:      0,
					SourceText: req.Segments[0].SourceText,
					TargetText: target,
				},
			},
		}, nil
	}
	svc.SetTranslationService(transSvc)

	// 1. Text detection creates regions including instructional UI and semantic text
	_, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-forward-ctx",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}

	// 2. Localize visual track with Hybrid profile, consent, and credential refs
	capturedInputs = nil
	visTrack, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:                 "run-forward-ctx",
		AssetID:               assetID,
		TargetLanguage:        "vi",
		ExecutionProfile:      domain.ExecutionProfileHybrid,
		AuthorizedCredentials: []string{"cred_gateway_1", "cred_gateway_2"},
		ConsentGranted:        true,
	})
	if err != nil {
		t.Fatalf("LocalizeVisualTrack failed: %v", err)
	}
	if visTrack == nil {
		t.Fatal("expected non-nil visual track")
	}

	// Verify that translation was called and all calls received the routing context
	if len(capturedInputs) == 0 {
		t.Fatal("expected at least one nested translation call for visual text")
	}
	for i, captured := range capturedInputs {
		if captured.ExecutionProfile != domain.ExecutionProfileHybrid {
			t.Errorf("call %d: expected execution profile %s, got %s", i, domain.ExecutionProfileHybrid, captured.ExecutionProfile)
		}
		if !captured.ConsentGranted {
			t.Errorf("call %d: expected consent_granted=true, got false", i)
		}
		if len(captured.AuthorizedCredentials) != 2 || captured.AuthorizedCredentials[0] != "cred_gateway_1" || captured.AuthorizedCredentials[1] != "cred_gateway_2" {
			t.Errorf("call %d: expected credentials [cred_gateway_1, cred_gateway_2], got %v", i, captured.AuthorizedCredentials)
		}
	}

	// 3. Verify existing callers without those fields still work (backward compatibility)
	capturedInputs = nil
	visTrackLegacy, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-forward-legacy",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("LocalizeVisualTrack without routing fields failed: %v", err)
	}
	if visTrackLegacy == nil {
		t.Fatal("expected non-nil visual track for legacy caller")
	}
	if len(capturedInputs) == 0 {
		t.Fatal("expected nested translation call for legacy caller")
	}
	for i, captured := range capturedInputs {
		if captured.ExecutionProfile != "" {
			t.Errorf("call %d: expected empty execution profile, got %s", i, captured.ExecutionProfile)
		}
		if captured.ConsentGranted {
			t.Errorf("call %d: expected consent_granted=false, got true", i)
		}
		if len(captured.AuthorizedCredentials) != 0 {
			t.Errorf("call %d: expected empty credentials, got %v", i, captured.AuthorizedCredentials)
		}
	}
}

// TestVisualTextService_LocalizeVisualTrack_ProtectedOverlapSurfacesAsException pins the
// operator-review contract for a collision with another protected tracked region
// (architecture §9.1): the overlay is skipped (the source text stays on screen untouched),
// the collision is reported in the artifact, and the region is projected as a pending
// visual_occlusion exception instead of aborting the stage and dead-ending the run.
// A collision with a scene-protected region (face / tap target) still fails closed, which
// TestVisualTextService_LocalizeVisualTrack_And_Overrides covers.
func TestVisualTextService_LocalizeVisualTrack_ProtectedOverlapSurfacesAsException(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()

	detected, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-occlusion",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}
	if len(detected.Regions) < 2 {
		t.Fatalf("expected at least 2 detected regions, got %d", len(detected.Regions))
	}

	// Publish a plan where a protected control (the brand mark region) sits exactly on the
	// box of the largest overlay-producing region: that neighbour's overlay can never clear
	// it, which is the live failure this test pins (two tracked UI controls sharing screen
	// space).
	plan := *detected
	regions := make([]domain.TrackedTextRegion, len(detected.Regions))
	copy(regions, detected.Regions)
	guardIdx, victim, victimArea := -1, -1, 0
	for i, reg := range regions {
		if len(reg.Keyframes) == 0 {
			continue
		}
		if reg.ProtectedMetadata.IsProtected {
			if guardIdx < 0 {
				guardIdx = i
			}
			continue
		}
		if reg.Role != domain.TextRoleSemanticText && reg.Role != domain.TextRoleInstructionalUIText {
			continue
		}
		if area := reg.Keyframes[0].Box.Width * reg.Keyframes[0].Box.Height; area > victimArea {
			victim, victimArea = i, area
		}
	}
	if guardIdx < 0 || victim < 0 {
		t.Fatalf("fixture needs one protected region and one overlay-producing region: guard=%d victim=%d", guardIdx, victim)
	}
	victimReg := regions[victim]
	guard := regions[guardIdx]
	guard.FirstSeenMs = victimReg.FirstSeenMs
	guard.LastSeenMs = victimReg.LastSeenMs
	guard.Keyframes = []domain.RegionKeyframe{{TimestampMs: victimReg.FirstSeenMs, Box: victimReg.Keyframes[0].Box}}
	regions[guardIdx] = guard
	plan.Regions = regions
	plan.ProvenanceHash = "prov-occlusion-guard"
	plan.ID = "plan-occlusion-guard"
	plan.CreatedAt = time.Now().UTC()

	planBytes, _ := json.Marshal(plan)
	planObj, err := casStore.Put(bytes.NewReader(planBytes))
	if err != nil {
		t.Fatalf("put plan in CAS: %v", err)
	}
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             plan.ID,
		AssetID:        assetID,
		CASHash:        planObj.SHA256,
		ProvenanceHash: plan.ProvenanceHash,
		CreatedAt:      plan.CreatedAt,
	}); err != nil {
		t.Fatalf("save plan index: %v", err)
	}

	vis, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-occlusion",
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("protected-region collision must surface for review, not fail the stage: %v", err)
	}
	if len(vis.Occlusions) == 0 {
		t.Fatal("expected occlusion reports for regions colliding with the protected control, got none")
	}
	if vis.Occlusions[0].RegionID != victimReg.ID {
		t.Fatalf("expected %s (the region whose overlay cannot clear the protected control) to be reported, got %+v", victimReg.ID, vis.Occlusions)
	}
	occluded := make(map[string]domain.OcclusionReport, len(vis.Occlusions))
	for _, occ := range vis.Occlusions {
		if occ.RegionID == "" {
			t.Error("occlusion report without a region id")
		}
		if occ.RegionID == guard.ID {
			t.Errorf("the protected control itself must not be reported as occluded")
		}
		occluded[occ.RegionID] = occ
	}
	for _, ov := range vis.Overlays {
		if _, skip := occluded[ov.RegionID]; skip {
			t.Errorf("occluded region %s must not carry an overlay", ov.RegionID)
		}
	}

	// The persisted artifact carries the report (the overlay is absent from it).
	rc, err := casStore.Get(vis.CASHash)
	if err != nil {
		t.Fatalf("read persisted visual track: %v", err)
	}
	var persisted domain.LocalizedVisualTrack
	decodeErr := json.NewDecoder(rc).Decode(&persisted)
	rc.Close()
	if decodeErr != nil {
		t.Fatalf("decode persisted visual track: %v", decodeErr)
	}
	if len(persisted.Occlusions) != len(vis.Occlusions) {
		t.Fatalf("persisted track lost its occlusion reports: %d != %d", len(persisted.Occlusions), len(vis.Occlusions))
	}

	// ...and the operator sees one pending exception per skipped overlay.
	reviewSvc := service.NewReviewService(db, casStore)
	items, err := reviewSvc.ProjectReviewItemsForRun(ctx, assetID, "vi", "run-occlusion")
	if err != nil {
		t.Fatalf("project review items: %v", err)
	}
	projected := make(map[string]bool)
	for _, item := range items {
		if item.Type != domain.ReviewItemTypeVisualOcclusion {
			continue
		}
		projected[item.RegionID] = true
		if item.Status != domain.ReviewItemStatusPending {
			t.Errorf("expected a pending occlusion item, got %s", item.Status)
		}
		if item.Stage != "visual_text_localize" {
			t.Errorf("expected stage visual_text_localize, got %s", item.Stage)
		}
	}
	for regionID := range occluded {
		if !projected[regionID] {
			t.Fatalf("expected a visual_occlusion exception for %s, got %+v", regionID, items)
		}
	}
}

// TestVisualTextService_LocalizeVisualTrack_OverlayTranslationsStayEphemeral pins the ownership
// boundary between the visual lane and the run's canonical translation: translating overlay text
// is an inline lookup, so it must not republish the run-scoped TranslationVariant index nor
// append a `translation` stage row. Publishing it makes every artifact that pins the canonical
// CAS (DubScriptVariant.TranslationVariantCAS) fail closed on the next visual_text_localize.
func TestVisualTextService_LocalizeVisualTrack_OverlayTranslationsStayEphemeral(t *testing.T) {
	svc, db, casStore, assetID := setupVisualTextService(t)
	defer db.Close()
	setupTestTranslationService(db, casStore, svc)

	ctx := context.Background()

	detected, err := svc.DetectAndTrackText(ctx, service.VisualTextDetectionInput{
		RunID:   "run-ephemeral",
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("detect text failed: %v", err)
	}
	if len(detected.Regions) == 0 {
		t.Fatal("expected detected regions")
	}

	// The speech stages already published the canonical translation for this run.
	canonical := domain.TranslationVariant{
		ID:             "trans-canonical",
		AssetID:        assetID,
		RunID:          "run-ephemeral",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "第一步:准备好所有新鲜食材。", TargetText: "Buoc 1: Chuan bi nguyen lieu.", StartMs: 0, EndMs: 3000},
		},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	canonicalBytes, _ := json.Marshal(canonical)
	canonicalObj, err := casStore.Put(bytes.NewReader(canonicalBytes))
	if err != nil {
		t.Fatalf("put canonical translation in CAS: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             canonical.ID,
		AssetID:        assetID,
		RunID:          canonical.RunID,
		TargetLanguage: "vi",
		CASHash:        canonicalObj.SHA256,
		ProvenanceHash: "prov-canonical",
		CreatedAt:      canonical.CreatedAt,
	}); err != nil {
		t.Fatalf("save canonical translation index: %v", err)
	}

	before, err := db.GetTranslationVariantIndexByRun(ctx, "run-ephemeral")
	if err != nil || before == nil {
		t.Fatalf("read canonical translation index before localize: %v", err)
	}
	beforeStages, err := db.ListStageExecutions(ctx, "run-ephemeral")
	if err != nil {
		t.Fatalf("list stage executions before localize: %v", err)
	}
	translationStagesBefore := 0
	for _, se := range beforeStages {
		if se.Stage == "translation" {
			translationStagesBefore++
		}
	}

	// Localize runs the overlay translation path (the fixture's semantic-text region).
	if _, err := svc.LocalizeVisualTrack(ctx, service.LocalizeVisualTrackInput{
		RunID:          "run-ephemeral",
		AssetID:        assetID,
		TargetLanguage: "vi",
	}); err != nil {
		t.Fatalf("localize visual track failed: %v", err)
	}

	after, err := db.GetTranslationVariantIndexByRun(ctx, "run-ephemeral")
	if err != nil || after == nil {
		t.Fatalf("read canonical translation index after localize: %v", err)
	}
	if after.CASHash != before.CASHash {
		t.Fatalf("overlay translation republished the run's canonical translation:\n before=%s\n after=%s", before.CASHash, after.CASHash)
	}
	afterStages, err := db.ListStageExecutions(ctx, "run-ephemeral")
	if err != nil {
		t.Fatalf("list stage executions after localize: %v", err)
	}
	translationStagesAfter := 0
	for _, se := range afterStages {
		if se.Stage == "translation" {
			translationStagesAfter++
		}
	}
	if translationStagesAfter != translationStagesBefore {
		t.Fatalf("overlay translation appended %d translation stage row(s); the visual lane did not run the translation stage",
			translationStagesAfter-translationStagesBefore)
	}
}
