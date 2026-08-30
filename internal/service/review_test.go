package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

func setupReviewTestHarness(t *testing.T) (*service.ReviewService, *storage.DB, *cas.Store, string) {
	t.Helper()
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup cas: %v", err)
	}
	dbPath := filepath.Join(tmpDir, "review_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	attID := "att-review-001"
	err = db.CreateRightsAttestation(context.Background(), domain.RightsAttestation{
		ID:              attID,
		AttestationType: "user_owned",
		DeclaredBy:      "test_operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}

	mockMediaObj, err := casStore.Put(bytes.NewReader([]byte("mock_video_bytes_12345")))
	if err != nil {
		t.Fatalf("put mock video media: %v", err)
	}

	assetID := "test-asset-review-001"
	err = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
		ID:                  assetID,
		SHA256:              mockMediaObj.SHA256,
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "video.mp4",
		RightsAttestationID: attID,
		CreatedAt:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	reviewSvc := service.NewReviewService(db, casStore)
	return reviewSvc, db, casStore, assetID
}

func setupFullReviewHarness(t *testing.T) (*service.ReviewService, *storage.DB, *cas.Store, string) {
	t.Helper()
	reviewSvc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	srcObj, err := casStore.Put(bytes.NewReader([]byte("dummy video media bytes 12345")))
	if err != nil {
		t.Fatalf("put dummy video media: %v", err)
	}

	wavBytes := media.GeneratePCM16WAV(48000, 2, 3000)
	wavObj, err := casStore.Put(bytes.NewReader(wavBytes))
	if err != nil {
		t.Fatalf("put wav media: %v", err)
	}

	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		SHA256:              srcObj.SHA256,
		ByteSize:            int64(len("dummy video media bytes 12345")),
		MimeType:            "video/mp4",
		OriginalFilename:    "video.mp4",
		RightsAttestationID: "att-review-001",
		CreatedAt:           time.Now().UTC(),
	})

	_ = db.SavePreflightReport(ctx, domain.PreflightReport{
		ID:          "pf-" + assetID,
		AssetID:     assetID,
		DurationMs:  3000,
		DurationSec: 3.0,
		Width:       1080,
		Height:      1920,
		FrameRate:   30.0,
		CreatedAt:   time.Now().UTC(),
	})

	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:      "role-plan-" + assetID,
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	})

	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-" + assetID,
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-" + assetID,
		CreatedAt:      time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(tPlan)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             tPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: tPlan.ProvenanceHash,
		CreatedAt:      tPlan.CreatedAt,
	})

	stems := domain.AudioStemArtifacts{
		ID:            "stems-" + assetID,
		AssetID:       assetID,
		SchemaVersion: 1,
		ProviderID:    "fake_uvr_separator",
		ModelName:     "uvr-mdx-net",
		ModelVersion:  "v3",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: wavObj.SHA256,
				SampleRate:   48000,
				Channels:     2,
				Format:       "wav",
				DurationMs:   3000,
			},
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: wavObj.SHA256,
				SampleRate:   48000,
				Channels:     2,
				Format:       "wav",
				DurationMs:   3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stems)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:             stems.ID,
		AssetID:        assetID,
		ProviderID:     stems.ProviderID,
		ModelName:      stems.ModelName,
		ModelVersion:   stems.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov-stems-" + assetID,
		CreatedAt:      stems.CreatedAt,
	})

	// Setup VoiceAssignment
	va := domain.VoiceAssignment{
		ID:             "va-" + assetID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Assignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vi-preset-1",
				Name:       "Preset Voice 1",
				Language:   "vi",
				Gender:     "female",
				ProviderID: "fake_tts",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	_ = db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID:             va.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        vaObj.SHA256,
		ProvenanceHash: "prov-va-" + assetID,
		CreatedAt:      va.CreatedAt,
	})

	transSvc := service.NewTranslationService(db, casStore)
	dubSvc := service.NewDubbingService(db, casStore)
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		pcm := media.GeneratePCM16WAV(16000, 1, 1000)
		return &provider.TTSSynthesisResult{
			AudioData:           pcm,
			SampleRate:          16000,
			Channels:            1,
			Format:              "wav",
			PredictedDurationMs: 1000,
			MeasuredDurationMs:  1000,
		}, nil
	}
	mixSvc := service.NewAudioMixService(db, casStore)
	visSvc := service.NewVisualTextService(db, casStore)
	renderSvc := service.NewRenderService(db, casStore)

	reviewSvc.SetTranslationService(transSvc)
	reviewSvc.SetDubbingService(dubSvc)
	reviewSvc.SetAudioMixService(mixSvc)
	reviewSvc.SetVisualTextService(visSvc)
	reviewSvc.SetRenderService(renderSvc)

	return reviewSvc, db, casStore, assetID
}

func TestReviewService_CleanRun_EmptyReviewItems(t *testing.T) {
	svc, _, _, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	items, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 review items for clean asset, got %d", len(items))
	}
}

func TestReviewService_CollectsAllExceptionTypes(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// 1. AudioRolePlan with uncertain role
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:      "role-plan-1",
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleUncertain},
		},
		CreatedAt: time.Now().UTC(),
	})

	// 2. TextRegionPlan with review required
	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:             "reg-1",
				Role:           domain.TextRoleSpeechSubtitle,
				Text:           "模糊文本",
				FirstSeenMs:    1000,
				LastSeenMs:     2000,
				ReviewRequired: true,
				ReviewReason:   "low_ocr_confidence_subtitle",
			},
		},
	}
	tBytes, _ := json.Marshal(tPlan)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             tPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: tPlan.ProvenanceHash,
		CreatedAt:      time.Now().UTC(),
	})

	// 3. TranslationVariant with low QA confidence
	transVar := domain.TranslationVariant{
		ID:             "trans-var-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-trans-1",
		OverallQAScore: 0.5,
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "原文",
				TargetText:   "Dịch",
				QAConfidence: 0.4,
				PassedQAGate: false,
			},
		},
	}
	transBytes, _ := json.Marshal(transVar)
	transObj, _ := casStore.Put(bytes.NewReader(transBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        transObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		OverallQAScore: transVar.OverallQAScore,
		CreatedAt:      time.Now().UTC(),
	})

	// 4. DubSegmentsVariant with unselected overrun review item
	dubSegVar := domain.DubSegmentsVariant{
		ID:             "dub-seg-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-dub-1",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "spk_1",
				StartMs:            2000,
				EndMs:              4000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 2800,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       3,
			},
		},
	}
	dubSegBytes, _ := json.Marshal(dubSegVar)
	dubSegObj, _ := casStore.Put(bytes.NewReader(dubSegBytes))
	_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dubSegVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        dubSegObj.SHA256,
		ProvenanceHash: dubSegVar.ProvenanceHash,
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      time.Now().UTC(),
	})

	items, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}

	if len(items) != 4 {
		t.Fatalf("expected 4 review items, got %d: %+v", len(items), items)
	}

	typeCounts := make(map[domain.ReviewItemType]int)
	for _, it := range items {
		typeCounts[it.Type]++
	}

	if typeCounts[domain.ReviewItemTypeAudioRole] != 1 {
		t.Errorf("expected 1 audio_role exception, got %d", typeCounts[domain.ReviewItemTypeAudioRole])
	}
	if typeCounts[domain.ReviewItemTypeLowConfidenceOCR] != 1 {
		t.Errorf("expected 1 low_confidence_ocr exception, got %d", typeCounts[domain.ReviewItemTypeLowConfidenceOCR])
	}
	if typeCounts[domain.ReviewItemTypeTranslationQA] != 1 {
		t.Errorf("expected 1 translation_qa exception, got %d", typeCounts[domain.ReviewItemTypeTranslationQA])
	}
	if typeCounts[domain.ReviewItemTypeTTSOverrun] != 1 {
		t.Errorf("expected 1 tts_overrun exception, got %d", typeCounts[domain.ReviewItemTypeTTSOverrun])
	}
}

func TestReviewService_DeterministicIDsAndTimestamps(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	fixedTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-det",
		AssetID:        assetID,
		ProvenanceHash: "prov-text-det",
		Regions: []domain.TrackedTextRegion{
			{
				ID:             "reg-det-1",
				Role:           domain.TextRoleUncertain,
				Text:           "Uncertain OCR text",
				FirstSeenMs:    500,
				LastSeenMs:     1500,
				ReviewRequired: true,
				ReviewReason:   "ambiguous_role",
			},
		},
		CreatedAt: fixedTime,
	}
	tBytes, _ := json.Marshal(tPlan)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             tPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: tPlan.ProvenanceHash,
		CreatedAt:      fixedTime,
	})

	items1, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("first project call failed: %v", err)
	}
	items2, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("second project call failed: %v", err)
	}

	if len(items1) != 1 || len(items2) != 1 {
		t.Fatalf("expected 1 item in both projections, got %d and %d", len(items1), len(items2))
	}

	if items1[0].ID != items2[0].ID {
		t.Errorf("expected deterministic IDs: %q != %q", items1[0].ID, items2[0].ID)
	}
	if !items1[0].CreatedAt.Equal(items2[0].CreatedAt) {
		t.Errorf("expected stable timestamps: %v != %v", items1[0].CreatedAt, items2[0].CreatedAt)
	}
	if !items1[0].CreatedAt.Equal(fixedTime) {
		t.Errorf("expected timestamp derived from immutable artifact: %v != %v", items1[0].CreatedAt, fixedTime)
	}
}

func TestReviewService_CorruptCAS_ReturnsError(t *testing.T) {
	svc, db, _, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// Save index with non-existent CAS hash
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             "trans-corrupt",
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        "non_existent_cas_hash_12345",
		ProvenanceHash: "prov-corrupt",
		CreatedAt:      time.Now().UTC(),
	})

	_, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err == nil {
		t.Fatalf("expected error on corrupt CAS hash, got nil")
	}
}

func TestReviewService_ManualOverride_LeavesExceptionQueue(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// 1. Create a DubSegmentsVariant with an overrun exception
	dubSegVar := domain.DubSegmentsVariant{
		ID:             "dub-seg-ovr-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-dub-ovr-1",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "spk_1",
				StartMs:            2000,
				EndMs:              4000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 2800,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       3,
			},
		},
	}
	dubSegBytes, _ := json.Marshal(dubSegVar)
	dubSegObj, _ := casStore.Put(bytes.NewReader(dubSegBytes))
	_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dubSegVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        dubSegObj.SHA256,
		ProvenanceHash: dubSegVar.ProvenanceHash,
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      time.Now().UTC(),
	})

	// Initial pending queue: 1 item
	items, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 pending item, got %d", len(items))
	}
	flaggedItem := items[0]

	// 2. Record manual override
	overrideInput := service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   flaggedItem.ID,
		ItemType:       flaggedItem.Type,
		Stage:          flaggedItem.Stage,
		ItemIndex:      flaggedItem.ItemIndex,
		Reason:         "Approved by director for stylistic cadence",
		Operator:       "lead_qa_reviewer",
	}
	ro, err := svc.RecordManualOverride(ctx, overrideInput)
	if err != nil {
		t.Fatalf("RecordManualOverride failed: %v", err)
	}
	if ro.Action != "manual_override" || ro.Operator != "lead_qa_reviewer" {
		t.Errorf("override fields mismatch: %+v", ro)
	}

	// 3. Exception-only review queue: item leaves automatically -> 0 items
	pendingAfter, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}
	if len(pendingAfter) != 0 {
		t.Errorf("expected 0 pending items after manual override, got %d: %+v", len(pendingAfter), pendingAfter)
	}

	// 4. Full projection includes the item with manual_override status
	allItems, err := svc.ProjectAllReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectAllReviewItems failed: %v", err)
	}
	if len(allItems) != 1 {
		t.Fatalf("expected 1 item in full projection, got %d", len(allItems))
	}
	if allItems[0].Status != domain.ReviewItemStatusManualOverride {
		t.Errorf("expected status manual_override, got %s", allItems[0].Status)
	}
}

func TestReviewService_QualityResult_ProjectedAndOverridden(t *testing.T) {
	svc, db, _, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// Save QualityResult with REVIEW_REQUIRED
	qr := domain.QualityResult{
		ID:             "qr-audio-sync-01",
		RunID:          "run-001",
		AssetID:        assetID,
		TargetLanguage: "vi",
		Stage:          "render",
		OverallStatus:  domain.QualityStatusReviewRequired,
		Issues: []domain.ReviewItem{
			{
				ID:             "rev-qr-sync-1",
				AssetID:        assetID,
				TargetLanguage: "vi",
				Type:           domain.ReviewItemTypeVisualOcclusion,
				Stage:          "render",
				Severity:       "warning",
				Reason:         "subtitle box intersects instructional UI icon",
				Status:         domain.ReviewItemStatusPending,
				CreatedAt:      time.Now().UTC(),
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := db.SaveQualityResult(ctx, qr); err != nil {
		t.Fatalf("SaveQualityResult failed: %v", err)
	}

	items, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item from QualityResult, got %d", len(items))
	}
	if items[0].Type != domain.ReviewItemTypeVisualOcclusion {
		t.Errorf("expected type visual_occlusion, got %s", items[0].Type)
	}

	// Record manual override
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   items[0].ID,
		Stage:          "render",
		Reason:         "Icon is non-interactive watermark; overlay is acceptable",
		Operator:       "editor_monet",
	})
	if err != nil {
		t.Fatalf("RecordManualOverride failed: %v", err)
	}

	// Queue is now zero
	pendingAfter, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(pendingAfter) != 0 {
		t.Fatalf("expected 0 pending items after override, got %d (err: %v)", len(pendingAfter), err)
	}
}

func TestReviewService_CorrectTargetText_TargetedRerunAndAutoResolution(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()

	// 1. Setup initial TranslationVariant
	transVar := domain.TranslationVariant{
		ID:             "trans-init-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-init",
		OverallQAScore: 0.5,
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "点击右上角",
				TargetText:   "Nhấn vào góc trên bên phải của màn hình", // Overly long text causing overrun
				StartMs:      0,
				EndMs:        1500,
				QAConfidence: 0.9,
				PassedQAGate: true,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		OverallQAScore: transVar.OverallQAScore,
		CreatedAt:      transVar.CreatedAt,
	})

	// 2. Setup initial DubScriptVariant
	dubScriptVar := domain.DubScriptVariant{
		ID:                    "dubscript-init-1",
		AssetID:               assetID,
		TargetLanguage:        "vi",
		SourceLanguage:        "zh",
		TranslationVariantCAS: tObj.SHA256,
		ProvenanceHash:        "prov-dubscript-init",
		OverallQAScore:        0.5,
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SourceText:     "点击右上角",
				MeaningText:    transVar.Segments[0].TargetText,
				SpokenText:     transVar.Segments[0].TargetText,
				SlotDurationMs: 1500,
				PassedQAGate:   true,
				RequiresReview: true,
				ReviewReason:   "duration_overrun_risk",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScriptVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dubScriptVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        dsObj.SHA256,
		ProvenanceHash: dubScriptVar.ProvenanceHash,
		OverallQAScore: dubScriptVar.OverallQAScore,
		CreatedAt:      dubScriptVar.CreatedAt,
	})

	// 3. Setup initial DubSegmentsVariant with overrun
	dubSegVar := domain.DubSegmentsVariant{
		ID:             "dubseg-init-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-dubseg-init",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SlotDurationMs:     1500,
				MeasuredDurationMs: 2200,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubSegBytes, _ := json.Marshal(dubSegVar)
	dubSegObj, _ := casStore.Put(bytes.NewReader(dubSegBytes))
	_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dubSegVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        dubSegObj.SHA256,
		ProvenanceHash: dubSegVar.ProvenanceHash,
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      dubSegVar.CreatedAt,
	})

	// Pending queue initially has 1 item
	itemsBefore, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsBefore) != 1 {
		t.Fatalf("expected 1 pending item before correction, got %d (err: %v)", len(itemsBefore), err)
	}

	// 4. Execute inspector target text correction with shortened text
	corrIn := service.TargetTextCorrectionInput{
		AssetID:            assetID,
		TargetLanguage:     "vi",
		SegmentIndex:       0,
		NewTargetText:      "Nhấn góc trên", // Shorter concise text
		SpokenTextOverride: "Nhấn góc trên",
		Reason:             "Shortened for 1.5s slot fit",
		Operator:           "editor_monet",
	}

	res, err := svc.CorrectTargetText(ctx, corrIn)
	if err != nil {
		t.Fatalf("CorrectTargetText failed: %v", err)
	}

	if res.TranslationVariantCAS == "" || res.DubScriptVariantCAS == "" {
		t.Errorf("expected new CAS hashes for translation and dub script: %+v", res)
	}
	if res.DubMixCAS == "" {
		t.Errorf("expected new DubMixCAS: %+v", res)
	}
	if res.LocalizedSubtitleCAS == "" {
		t.Errorf("expected new LocalizedSubtitleCAS: %+v", res)
	}
	if res.RenderPlanCAS == "" {
		t.Errorf("expected new RenderPlanCAS: %+v", res)
	}
	if res.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected status auto_resolved, got %s", res.Status)
	}

	// Verify TranslationVariant in CAS has new target text
	trc, err := casStore.Get(res.TranslationVariantCAS)
	if err != nil {
		t.Fatalf("load new translation variant failed: %v", err)
	}
	defer trc.Close()
	var newTVar domain.TranslationVariant
	_ = json.NewDecoder(trc).Decode(&newTVar)
	if newTVar.Segments[0].TargetText != "Nhấn góc trên" {
		t.Errorf("expected updated TargetText %q, got %q", "Nhấn góc trên", newTVar.Segments[0].TargetText)
	}

	// Verify RenderPlan lineage and schema purity: decodes as domain.RenderPlan
	rprc, err := casStore.Get(res.RenderPlanCAS)
	if err != nil {
		t.Fatalf("load render plan from CAS failed: %v", err)
	}
	defer rprc.Close()
	var rPlan domain.RenderPlan
	if err := json.NewDecoder(rprc).Decode(&rPlan); err != nil {
		t.Fatalf("decode render plan failed (schema mismatch): %v", err)
	}
	if rPlan.DubMixCASHash != res.DubMixCAS {
		t.Errorf("RenderPlan did not pin new DubMixCAS: %s != %s", rPlan.DubMixCASHash, res.DubMixCAS)
	}
	if rPlan.SubtitlePlan.CASHash == "" || rPlan.SubtitlePlan.CASHash == res.LocalizedSubtitleCAS {
		t.Errorf("RenderPlan SubtitlePlan CAS invalid or mixed with LocalizedSubtitleTrack CAS: %s vs %s", rPlan.SubtitlePlan.CASHash, res.LocalizedSubtitleCAS)
	}
	// Verify SubtitlePlanArtifact in CAS decodes cleanly
	sprc, err := casStore.Get(rPlan.SubtitlePlan.CASHash)
	if err != nil {
		t.Fatalf("load subtitle plan artifact from CAS failed: %v", err)
	}
	defer sprc.Close()
	var subArt domain.SubtitlePlanArtifact
	if err := json.NewDecoder(sprc).Decode(&subArt); err != nil {
		t.Fatalf("decode subtitle plan artifact failed (schema mismatch): %v", err)
	}
	if len(subArt.Cues) == 0 {
		t.Errorf("expected subtitle plan artifact to have cues")
	}
}

func TestReviewService_CorrectTargetText_MissingRequiredServices_FailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// TranslationVariant in DB
	transVar := domain.TranslationVariant{
		ID:             "trans-missing-svc-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-missing",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "测试", TargetText: "Thử nghiệm", QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		CreatedAt:      transVar.CreatedAt,
	})

	// svc has NO downstream services set
	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		SegmentIndex:   0,
		NewTargetText:  "Thử nghiệm mới",
	})
	if err == nil {
		t.Fatalf("expected error when required descendant services are missing, got nil (res: %+v)", res)
	}
	if res != nil && res.Status == domain.ReviewItemStatusAutoResolved {
		t.Fatalf("missing required services must NEVER yield auto_resolved")
	}
}

func TestReviewService_MatchOverride_ZeroValueItemIndexAmbiguity(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// Setup TranslationVariant with two QA-failed segments (index 0 and index 1)
	transVar := domain.TranslationVariant{
		ID:             "trans-var-multi",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-trans-multi",
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "第一句",
				TargetText:   "Câu 1",
				QAConfidence: 0.4,
				PassedQAGate: false,
			},
			{
				Index:        1,
				SourceText:   "第二句",
				TargetText:   "Câu 2",
				QAConfidence: 0.4,
				PassedQAGate: false,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		CreatedAt:      transVar.CreatedAt,
	})

	// Initial queue has 2 pending items
	items, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(items) != 2 {
		t.Fatalf("expected 2 pending items, got %d (err: %v)", len(items), err)
	}

	// 1. Attempting an override with missing ReviewItemID (stage-only or region-only) must fail
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		Stage:          "translation",
		RegionID:       "unrelated-region",
		Reason:         "Watermark override",
		Operator:       "reviewer_1",
	})
	if err == nil {
		t.Fatalf("expected error for missing ReviewItemID, got nil")
	}

	// Verify neither translation segment was overridden
	itemsAfterRejectedOvr, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsAfterRejectedOvr) != 2 {
		t.Fatalf("expected 2 items still pending, got %d", len(itemsAfterRejectedOvr))
	}

	// 2. Record an override for exact ReviewItemID of item 1
	item1ID := items[1].ID
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   item1ID,
		Reason:         "Item 1 approved",
		Operator:       "reviewer_1",
	})
	if err != nil {
		t.Fatalf("RecordManualOverride for item 1 failed: %v", err)
	}

	// Verify only item 0 remains pending
	itemsAfterItem1Ovr, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsAfterItem1Ovr) != 1 {
		t.Fatalf("expected 1 remaining pending item, got %d", len(itemsAfterItem1Ovr))
	}
	if itemsAfterItem1Ovr[0].ItemIndex != 0 {
		t.Errorf("expected item index 0 to remain pending, got %d", itemsAfterItem1Ovr[0].ItemIndex)
	}
}

func TestReviewService_RecordManualOverride_StrictPendingValidation(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// 1. Setup TranslationVariant with a QA failure
	transVar1 := domain.TranslationVariant{
		ID:             "trans-var-strict-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-trans-strict-1",
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "源文本1",
				TargetText:   "Dịch 1",
				QAConfidence: 0.3,
				PassedQAGate: false,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes1, _ := json.Marshal(transVar1)
	tObj1, _ := casStore.Put(bytes.NewReader(tBytes1))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar1.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj1.SHA256,
		ProvenanceHash: transVar1.ProvenanceHash,
		CreatedAt:      transVar1.CreatedAt,
	})

	// 2. Setup QualityResult with PASS (auto_pass item in full projection)
	qrPass := domain.QualityResult{
		ID:             "qr-pass-001",
		RunID:          "run-001",
		AssetID:        assetID,
		TargetLanguage: "vi",
		Stage:          "render",
		OverallStatus:  domain.QualityStatusPass,
		CreatedAt:      time.Now().UTC(),
	}
	_ = db.SaveQualityResult(ctx, qrPass)

	items, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(items) != 1 {
		t.Fatalf("expected 1 pending item, got %d (err: %v)", len(items), err)
	}
	validPendingID := items[0].ID

	// Check Rejection 1: Empty / Missing ReviewItemID
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   "",
		Reason:         "No item id",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "review_item_id is required") {
		t.Errorf("expected error for empty ReviewItemID, got: %v", err)
	}

	// Check Rejection 2: Nonexistent ReviewItemID
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   "rev-trans-nonexistent-hash-0",
		Reason:         "Nonexistent item",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "not found in pending review queue") {
		t.Errorf("expected error for nonexistent ReviewItemID, got: %v", err)
	}

	// Check Rejection 3: Non-pending ReviewItemID (auto_pass item from clean QualityResult)
	autoPassID := fmt.Sprintf("rev-quality-pass-%s", qrPass.ID)
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   autoPassID,
		Reason:         "Trying to override auto_pass item",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "not found in pending review queue") {
		t.Errorf("expected error for non-pending auto_pass item, got: %v", err)
	}

	// Check Rejection 4: Cross-Asset / Cross-Language Mismatch
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        "other-asset-xyz",
		TargetLanguage: "vi",
		ReviewItemID:   validPendingID,
		Reason:         "Wrong asset",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "not found in pending review queue") {
		t.Errorf("expected error for cross-asset mismatch, got: %v", err)
	}
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "en",
		ReviewItemID:   validPendingID,
		Reason:         "Wrong language",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "not found in pending review queue") {
		t.Errorf("expected error for cross-language mismatch, got: %v", err)
	}

	// Check Rejection 5: Stale ReviewItemID after underlying index updates
	staleItemID := validPendingID
	// Update TranslationVariantIndex to a new artifact
	transVar2 := domain.TranslationVariant{
		ID:             "trans-var-strict-2",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-trans-strict-2",
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "源文本2",
				TargetText:   "Dịch 2",
				QAConfidence: 0.2,
				PassedQAGate: false,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes2, _ := json.Marshal(transVar2)
	tObj2, _ := casStore.Put(bytes.NewReader(tBytes2))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar2.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj2.SHA256,
		ProvenanceHash: transVar2.ProvenanceHash,
		CreatedAt:      transVar2.CreatedAt,
	})

	// The old staleItemID should now be rejected as it is no longer the current pending item
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   staleItemID,
		Reason:         "Stale item override",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "not found in pending review queue") {
		t.Errorf("expected error for stale ReviewItemID, got: %v", err)
	}

	// Fetch fresh pending items -> 1 item with fresh ID
	freshItems, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(freshItems) != 1 {
		t.Fatalf("expected 1 fresh pending item, got %d (err: %v)", len(freshItems), err)
	}
	currentPendingID := freshItems[0].ID

	// Success Case: Exact current pending ReviewItemID
	ro, err := svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   currentPendingID,
		Reason:         "Approved by editorial board",
		Operator:       "senior_editor",
	})
	if err != nil {
		t.Fatalf("expected success for exact current pending ReviewItemID, got err: %v", err)
	}
	if ro.ReviewItemID != currentPendingID || ro.Operator != "senior_editor" || ro.Reason != "Approved by editorial board" {
		t.Errorf("unexpected override payload: %+v", ro)
	}

	// Verify queue is now empty
	pendingAfter, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(pendingAfter) != 0 {
		t.Fatalf("expected 0 pending items, got %d", len(pendingAfter))
	}

	// Check Rejection 6: Re-submitting override on already-overridden item must now fail (no longer pending)
	_, err = svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   currentPendingID,
		Reason:         "Duplicate override attempt",
		Operator:       "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "not found in pending review queue") {
		t.Errorf("expected error when overriding already resolved item, got: %v", err)
	}
}
func TestReviewService_AutoPass_ObservableAndOutsideExceptionQueue(t *testing.T) {
	svc, db, _, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	// Save clean QualityResult with OverallStatus == PASS
	qr := domain.QualityResult{
		ID:             "qr-clean-01",
		RunID:          "run-clean-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		Stage:          "render",
		OverallStatus:  domain.QualityStatusPass,
		Metrics: []domain.QualityMetric{
			{Name: "naturalness", Score: 0.98, Passed: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := db.SaveQualityResult(ctx, qr); err != nil {
		t.Fatalf("SaveQualityResult failed: %v", err)
	}

	// Exception-only queue must be empty (0 pending items)
	pending, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending items in exception-only queue, got %d", len(pending))
	}

	// Full projection must include the auto_pass item
	allItems, err := svc.ProjectAllReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectAllReviewItems failed: %v", err)
	}
	if len(allItems) != 1 {
		t.Fatalf("expected 1 item in full projection, got %d", len(allItems))
	}
	if allItems[0].Status != domain.ReviewItemStatusAutoPass {
		t.Errorf("expected status auto_pass, got %s", allItems[0].Status)
	}
}

func TestReviewService_CorrectTargetText_HonestQAEvaluation_And_FailClosed(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()

	// Setup initial TranslationVariant
	transVar := domain.TranslationVariant{
		ID:             "trans-honest-1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-honest-1",
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "请不要关闭窗口",         // "Please don't close the window" (negative)
				TargetText:   "Hãy đóng cửa sổ", // "Please close the window" (negation flipped!)
				StartMs:      0,
				EndMs:        1500,
				QAConfidence: 0.3,
				PassedQAGate: false,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		CreatedAt:      transVar.CreatedAt,
	})

	// 1. CorrectTargetText with still-failing text (negation preserved missing)
	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		SegmentIndex:   0,
		NewTargetText:  "Đóng cửa sổ lại", // Still missing negative "không"
	})
	// It should either return an error on AdaptDubScript / QA rejection or return non-resolved status
	if err == nil && res != nil && res.Status == domain.ReviewItemStatusAutoResolved {
		t.Errorf("failing QA candidate was marked auto_resolved!")
	}

	// If an artifact was saved, verify TranslationVariant in CAS was NOT given fake PASS/1.0
	if res != nil && res.TranslationVariantCAS != "" {
		trc, err := casStore.Get(res.TranslationVariantCAS)
		if err == nil {
			defer trc.Close()
			var savedTVar domain.TranslationVariant
			_ = json.NewDecoder(trc).Decode(&savedTVar)
			if savedTVar.Segments[0].PassedQAGate || savedTVar.Segments[0].QAConfidence >= 0.9 {
				t.Errorf("invented fake QA pass/score for failing correction! %+v", savedTVar.Segments[0])
			}
		}
	}
}
