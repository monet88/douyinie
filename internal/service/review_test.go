package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
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

	assetID := "test-asset-review-001"
	err = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
		ID:                  assetID,
		SHA256:              "sha256-mock",
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
