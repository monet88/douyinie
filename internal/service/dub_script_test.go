package service_test

import (
	"context"
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

	// 1. Create meaning-first translation variant with a fast-speaking short slot
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
				EndMs:      3200, // Brisk source: 1100ms for a long sentence
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
	if len(dubVariant.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(dubVariant.Segments))
	}

	// Segment 1 had plenty of time (2000ms for short phrase)
	if dubVariant.Segments[0].SpokenText == "" {
		t.Errorf("segment 0 spoken text is empty")
	}

	// Segment 2 had tight slot (1100ms) - should be shorten-first adapted
	seg1 := dubVariant.Segments[1]
	if !seg1.IsShortened {
		t.Errorf("expected segment 1 to be shortened for brisk slot")
	}
	if len(seg1.SpokenText) >= len(seg1.MeaningText) {
		t.Errorf("expected shortened spoken text to be shorter than meaning text: %q vs %q", seg1.SpokenText, seg1.MeaningText)
	}

	// Critical facts, numbers, names, negation must still survive!
	if !seg1.PassedQAGate {
		t.Errorf("expected shortened segment to pass QA gate")
	}
	if !seg1.NegationPolarity {
		t.Errorf("expected negation polarity to be preserved in shortened text")
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
				EndMs:      1200, // Brisk slot
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
	if !seg.PassedQAGate {
		t.Errorf("expected shortened English segment to pass QA gate")
	}
}
