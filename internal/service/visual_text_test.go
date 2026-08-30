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

	// 4. LocalizeVisualTrack must render canonical TranslationVariant text, NEVER spokenText
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
