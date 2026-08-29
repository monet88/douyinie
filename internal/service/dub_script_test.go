package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

func TestTranslationService_AdaptDubScript_ShortensOverlongSpokenText(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256-dub",
		ByteSize:            2048,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-dub-1",
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     "job-dub-1",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})

	// 1. Create meaning-first translation variant with different slot budgets:
	// Seg 0: Normal slot (2000ms for short greeting)
	// Seg 1: Brisk but fittable after generic shortening.
	// Seg 2: Extreme overrun that remains too long after shortening -> RequiresReview.
	transInput := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "job-dub-1",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{
				Index:      0,
				SourceText: "今天天气很好。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    0,
				EndMs:      2000,
			},
			{
				Index:      1,
				SourceText: "请将温度调至25度，张伟说不要打开窗户。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    2100,
				EndMs:      5700, // 3600ms slot - generic shortening fits while preserving facts/name/number/negation
			},
			{
				Index:      2,
				SourceText: "请将温度调至25度，张伟说不要打开窗户。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    5800,
				EndMs:      6900, // 1100ms slot - extreme tight
			},
		},
	}

	transVariant, err := svc.Translate(ctx, transInput)
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	// 2. Adapt to DubScriptVariant
	dubInput := domain.DubScriptJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		JobID:                 "job-dub-1",
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transVariant.CASHash,
	}

	dubVariant, err := svc.AdaptDubScript(ctx, dubInput)
	if err != nil {
		t.Fatalf("AdaptDubScript failed: %v", err)
	}

	if dubVariant == nil {
		t.Fatal("expected non-nil DubScriptVariant")
	}
	if dubVariant.TargetLanguage != "vi" {
		t.Errorf("expected target language vi, got %s", dubVariant.TargetLanguage)
	}
	if dubVariant.SchemaVersion != domain.DubScriptSchemaVersion || dubVariant.SchemaVersion < 2 {
		t.Errorf("expected bumped DubScript schema version, got %d", dubVariant.SchemaVersion)
	}
	if len(dubVariant.Segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(dubVariant.Segments))
	}

	// Segment 0: Normal slot (2000ms)
	seg0 := dubVariant.Segments[0]
	if seg0.SpokenText == "" {
		t.Errorf("segment 0 spoken text is empty")
	}
	if seg0.SourceGapAfterMs != 100 {
		t.Errorf("expected 100ms source gap after first turn, got %d", seg0.SourceGapAfterMs)
	}
	if seg0.NaturalGapMs < seg0.SourceGapAfterMs {
		t.Errorf("predicted pause %dms must preserve source gap %dms", seg0.NaturalGapMs, seg0.SourceGapAfterMs)
	}
	if seg0.CadenceRatio <= 0 {
		t.Errorf("expected positive cadence ratio, got %f", seg0.CadenceRatio)
	}

	seg1 := dubVariant.Segments[1]
	if !seg1.IsShortened {
		t.Errorf("expected segment 1 to be shortened for brisk slot")
	}
	if len(seg1.SpokenText) >= len(seg1.MeaningText) {
		t.Errorf("expected shortened spoken text to be shorter than meaning text: %q vs %q", seg1.SpokenText, seg1.MeaningText)
	}
	if seg1.EstimatedDurationMs > seg1.SlotDurationMs {
		t.Errorf("expected shortened segment 1 to fit within slot: %d ms vs %d ms", seg1.EstimatedDurationMs, seg1.SlotDurationMs)
	}
	if seg1.RequiresReview {
		t.Errorf("expected segment 1 to not require review since it fits within slot")
	}
	if !seg1.PassedQAGate {
		t.Errorf("expected shortened segment to pass QA gate")
	}
	if !seg1.NegationPolarity {
		t.Errorf("expected negation polarity to be preserved in shortened text")
	}

	// Segment 2: Extreme tight slot (1100ms) - shortened but overruns slot -> requires review
	seg2 := dubVariant.Segments[2]
	if !seg2.IsShortened {
		t.Errorf("expected segment 2 to be shortened")
	}
	if !seg2.RequiresReview {
		t.Errorf("expected segment 2 to require review due to duration overrun")
	}
	if seg2.ReviewReason != "DURATION_OVERRUN" {
		t.Errorf("expected review reason DURATION_OVERRUN, got %s", seg2.ReviewReason)
	}

	// Variant overall review flag must be true because seg2 requires review
	if !dubVariant.RequiresReview {
		t.Errorf("expected variant to require review due to segment 2 overrun")
	}
}

func TestTranslationService_AdaptDubScript_English_ShortenFirst(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256-dub-en",
		ByteSize:            2048,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-dub-en",
		SourceAssetID:  assetID,
		TargetLanguage: "en",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     "job-dub-en",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})

	transInput := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "job-dub-en",
		SourceLanguage: "zh",
		TargetLanguage: "en",
		Segments: []domain.TranslationInputSegment{
			{
				Index:      0,
				SourceText: "请将温度调至25度，张伟说不要打开窗户。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    0,
				EndMs:      4500, // Fittable brisk slot
			},
		},
	}

	transVariant, err := svc.Translate(ctx, transInput)
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	dubInput := domain.DubScriptJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		JobID:                 "job-dub-en",
		SourceLanguage:        "zh",
		TargetLanguage:        "en",
		TranslationVariantCAS: transVariant.CASHash,
	}

	dubVariant, err := svc.AdaptDubScript(ctx, dubInput)
	if err != nil {
		t.Fatalf("AdaptDubScript failed: %v", err)
	}

	if len(dubVariant.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(dubVariant.Segments))
	}
	seg := dubVariant.Segments[0]
	if !seg.IsShortened {
		t.Errorf("expected English brisk segment to be shortened")
	}
	if len(seg.SpokenText) >= len(seg.MeaningText) {
		t.Errorf("expected English spoken text to be shortened: %q vs %q", seg.SpokenText, seg.MeaningText)
	}
	if seg.EstimatedDurationMs > seg.SlotDurationMs {
		t.Errorf("expected English segment to fit within slot: %d ms vs %d ms", seg.EstimatedDurationMs, seg.SlotDurationMs)
	}
	if seg.RequiresReview {
		t.Errorf("expected English segment to not require review")
	}
	if !seg.PassedQAGate {
		t.Errorf("expected shortened English segment to pass QA gate")
	}
}

func TestTranslationService_AdaptDubScript_CASAssetMismatch_FailsClosed(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	assetA := uuid.NewString()
	assetB := uuid.NewString()
	runID := uuid.NewString()

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetA,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256-a",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetB,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256-b",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-a",
		SourceAssetID:  assetA,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     "job-a",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})

	// Translate for Asset A
	transVariantA, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetA,
		JobID:          "job-a",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 2000},
		},
	})
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	// Try to adapt DubScript for Asset B using Asset A's CAS
	_, err = svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID:                 runID,
		AssetID:               assetB,
		JobID:                 "job-a",
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transVariantA.CASHash,
	})
	if err == nil {
		t.Fatal("expected error due to asset_id mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "asset_id mismatch") {
		t.Errorf("expected 'asset_id mismatch' error, got: %v", err)
	}
}

func TestTranslationService_AdaptDubScript_CASTargetLanguageMismatch_FailsClosed(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	assetID := uuid.NewString()
	runID := uuid.NewString()

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256-lang-mismatch",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-lang",
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     "job-lang",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})

	// Translate for VI
	transVariantVI, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "job-lang",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 2000},
		},
	})
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	// Try to adapt DubScript for EN using VI's TranslationVariant CAS
	_, err = svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		JobID:                 "job-lang",
		SourceLanguage:        "zh",
		TargetLanguage:        "en",
		TranslationVariantCAS: transVariantVI.CASHash,
	})
	if err == nil {
		t.Fatal("expected error due to target_language mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "target_language mismatch") {
		t.Errorf("expected 'target_language mismatch' error, got: %v", err)
	}
}
