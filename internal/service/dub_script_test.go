package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
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

	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID,
		dubScriptSpeechBlocks([2]int64{0, 2000}, [2]int64{2100, 5700}, [2]int64{5800, 6900})...)
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
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID,
		dubScriptSpeechBlocks([2]int64{0, 4500})...)

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
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:        "plan-" + assetB,
		AssetID:   assetB,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetA, runID,
		dubScriptSpeechBlocks([2]int64{0, 2000})...)

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
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID,
		dubScriptSpeechBlocks([2]int64{0, 2000})...)

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

func TestTranslationService_AdaptDubScript_MissingAudioRolePlan_FailsClosed(t *testing.T) {
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
		SHA256:              "fake-sha256-no-plan",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-no-plan",
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     "job-no-plan",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID)

	transVariant, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "job-no-plan",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 2000},
		},
	})
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	// Deliberately do NOT save AudioRolePlan. AdaptDubScript must fail closed.
	_, err = svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		JobID:                 "job-no-plan",
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transVariant.CASHash,
	})
	if err == nil {
		t.Fatal("expected error due to missing AudioRolePlan, got nil")
	}
	if !errors.Is(err, domain.ErrAudioRolePlanRequired) {
		t.Errorf("expected ErrAudioRolePlanRequired, got %v", err)
	}
}

func TestTranslationService_AdaptDubScript_NoDubPlan_ReturnsErrNoDubbingRequired(t *testing.T) {
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
		SHA256:              "fake-sha256-no-dub",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-no-dub",
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     "job-no-dub",
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})

	// Save AudioRolePlan with 0 dialogue segments (Instrumental BGM only)
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID)

	transVariant, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "job-no-dub",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 2000},
		},
	})
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}

	_, err = svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		JobID:                 "job-no-dub",
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		TranslationVariantCAS: transVariant.CASHash,
	})
	if err == nil {
		t.Fatal("expected ErrNoDubbingRequired on no-dub AudioRolePlan, got nil")
	}
	if !errors.Is(err, domain.ErrNoDubbingRequired) {
		t.Errorf("expected ErrNoDubbingRequired, got %v", err)
	}
}

// dubScriptSpeechBlocks builds the canonical speech blocks for [start,end] pairs,
// preserving the row order the caller lists them in.
func dubScriptSpeechBlocks(bounds ...[2]int64) []domain.SpeechBlock {
	blocks := make([]domain.SpeechBlock, 0, len(bounds))
	for i, b := range bounds {
		blocks = append(blocks, domain.SpeechBlock{
			Index:       i,
			StartMs:     b[0],
			EndMs:       b[1],
			SpeakerID:   "SPEAKER_00",
			SourceText:  "段",
			SegmentType: domain.SpeechBlockTypeSpeech,
		})
	}
	return blocks
}

// recordingSpokenAdapter captures every adaptation request so a test can assert on the
// immutable boundary evidence AdaptDubScript derived (HasNextTurn, SourceGapAfterMs).
type recordingSpokenAdapter struct {
	requests []provider.SpokenScriptAdaptationRequest
}

func (a *recordingSpokenAdapter) AdaptSpokenScript(_ context.Context, req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
	a.requests = append(a.requests, req)
	return provider.NewDefaultSpokenScriptAdapter().AdaptSpokenScript(context.Background(), req)
}

// setDubScriptSegmentTuning installs a recording adapter and returns it for assertions.
func setDubScriptSegmentTuning(svc *service.TranslationService) *recordingSpokenAdapter {
	adapter := &recordingSpokenAdapter{}
	svc.ConfigureSpokenAdapter(adapter)
	return adapter
}

// sourceGapArgs returns the boundary evidence (hasNextTurn, sourceGapAfterMs) captured for the
// request at position i. Adaptation runs segments in order, so the capture order is segment order.
func (a *recordingSpokenAdapter) sourceGapArgs(i int) (bool, int64, bool) {
	if i < 0 || i >= len(a.requests) {
		return false, 0, false
	}
	r := a.requests[i]
	return r.HasNextTurn, r.SourceGapAfterMs, true
}

// seedDubScriptAdaptationEnv builds the minimal run-scoped environment AdaptDubScript needs:
// a pinned dialogue plan, a pinned transcript carrying the given speech blocks, and a pinned
// translation variant whose segments mirror those blocks.
func seedDubScriptAdaptationEnv(t *testing.T, db *storage.DB, casStore *cas.Store, svc *service.TranslationService, ctx context.Context, assetID, runID string, blocks []domain.SpeechBlock) *domain.TranslationVariant {
	t.Helper()
	now := time.Now().UTC()
	attID := "att-" + assetID
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", DeclaredBy: "test-operator",
		TermsAccepted: true, ConfirmedAt: now,
	}); err != nil {
		t.Fatalf("create attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "sha-" + assetID, ByteSize: 1024, CreatedAt: now,
	}); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: "job-" + runID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: now}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "job-" + runID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	plan := domain.AudioRolePlan{
		ID: "plan-" + assetID, AssetID: assetID, CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 60000, Role: domain.AudioRoleNarrationDialogue}},
	}
	planObj, err := casStore.Put(bytes.NewReader(mustJSON(t, plan)))
	if err != nil {
		t.Fatalf("put plan: %v", err)
	}
	plan.CASHash = planObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, plan); err != nil {
		t.Fatalf("save plan: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "audio_role_plan", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: planObj.SHA256, CreatedAt: now,
	}); err != nil {
		t.Fatalf("pin plan: %v", err)
	}
	transcriptObj, err := casStore.Put(bytes.NewReader(mustJSON(t, domain.TranscriptArtifact{
		ID: "transcript-" + assetID, AssetID: assetID, RunID: runID, SpeechBlocks: blocks, CreatedAt: now,
	})))
	if err != nil {
		t.Fatalf("put transcript: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "speech_understand", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: transcriptObj.SHA256, CreatedAt: now,
	}); err != nil {
		t.Fatalf("pin transcript: %v", err)
	}
	segments := make([]domain.TranslationSegment, 0, len(blocks))
	for _, b := range blocks {
		segments = append(segments, domain.TranslationSegment{
			Index: b.Index, SourceText: b.SourceText, TargetText: "đã dịch", SpeakerID: b.SpeakerID,
			StartMs: b.StartMs, EndMs: b.EndMs, PassedQAGate: true, QAConfidence: 1.0,
		})
	}
	variant := domain.TranslationVariant{
		ID: uuid.NewString(), SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: "job-" + runID, SourceLanguage: "zh", TargetLanguage: "vi",
		TranscriptArtifactCAS: transcriptObj.SHA256, InputHash: "input-" + runID, ProvenanceHash: "prov-trans-" + runID,
		Segments: segments, EffectiveGlossary: domain.EffectiveGlossary{Hash: "glossary-hash"}, CreatedAt: now,
	}
	variantObj, err := casStore.Put(bytes.NewReader(mustJSON(t, variant)))
	if err != nil {
		t.Fatalf("put variant: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: variant.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: variantObj.SHA256,
		ProvenanceHash: variant.ProvenanceHash, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save variant index: %v", err)
	}
	variant.CASHash = variantObj.SHA256
	return &variant
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// seedDubScriptRunScaffold creates the minimal run-scoped environment (asset, job, run, pinned
// dialogue plan, pinned transcript with the given blocks) and returns the pinned transcript CAS.
// Tests seed their own TranslationVariant on top so they control its transcript binding.
func seedDubScriptRunScaffold(t *testing.T, db *storage.DB, casStore *cas.Store, ctx context.Context, assetID, runID string, blocks []domain.SpeechBlock) string {
	t.Helper()
	now := time.Now().UTC()
	attID := "att-" + assetID
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", DeclaredBy: "test-operator",
		TermsAccepted: true, ConfirmedAt: now,
	}); err != nil {
		t.Fatalf("create attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "sha-" + assetID, ByteSize: 1024, CreatedAt: now,
	}); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: "job-" + runID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: now}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "job-" + runID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	plan := domain.AudioRolePlan{
		ID: "plan-" + assetID, AssetID: assetID, CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 60000, Role: domain.AudioRoleNarrationDialogue}},
	}
	planObj, err := casStore.Put(bytes.NewReader(mustJSON(t, plan)))
	if err != nil {
		t.Fatalf("put plan: %v", err)
	}
	plan.CASHash = planObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, plan); err != nil {
		t.Fatalf("save plan: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "audio_role_plan", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: planObj.SHA256, CreatedAt: now,
	}); err != nil {
		t.Fatalf("pin plan: %v", err)
	}
	transcriptObj, err := casStore.Put(bytes.NewReader(mustJSON(t, domain.TranscriptArtifact{
		ID: "transcript-" + assetID, AssetID: assetID, RunID: runID, SpeechBlocks: blocks, CreatedAt: now,
	})))
	if err != nil {
		t.Fatalf("put transcript: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: "transcript-" + assetID, AssetID: assetID, RunID: runID, CASHash: transcriptObj.SHA256,
		ProvenanceHash: "prov-transcript-" + runID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save transcript index: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "speech_understand", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: transcriptObj.SHA256, CreatedAt: now,
	}); err != nil {
		t.Fatalf("pin transcript: %v", err)
	}
	return transcriptObj.SHA256
}

// putDubScriptTranslationVariant stores a TranslationVariant bound to transcriptCAS and indexes it
// for the run (and, so the asset's latest row is unambiguous, for the asset at createdAt).
func putDubScriptTranslationVariant(t *testing.T, db *storage.DB, casStore *cas.Store, ctx context.Context, assetID, runID, transcriptCAS string, blocks []domain.SpeechBlock, createdAt time.Time) *domain.TranslationVariant {
	t.Helper()
	segments := make([]domain.TranslationSegment, 0, len(blocks))
	for _, b := range blocks {
		segments = append(segments, domain.TranslationSegment{
			Index: b.Index, SourceText: b.SourceText, TargetText: "đã dịch", SpeakerID: b.SpeakerID,
			StartMs: b.StartMs, EndMs: b.EndMs, PassedQAGate: true, QAConfidence: 1.0,
		})
	}
	variant := domain.TranslationVariant{
		ID: uuid.NewString(), SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: "job-" + runID, SourceLanguage: "zh", TargetLanguage: "vi",
		TranscriptArtifactCAS: transcriptCAS, InputHash: "input-" + runID, ProvenanceHash: "prov-trans-" + runID,
		Segments: segments, EffectiveGlossary: domain.EffectiveGlossary{Hash: "glossary-hash"}, CreatedAt: createdAt,
	}
	obj, err := casStore.Put(bytes.NewReader(mustJSON(t, variant)))
	if err != nil {
		t.Fatalf("put variant: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: variant.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: obj.SHA256,
		ProvenanceHash: variant.ProvenanceHash, CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("save variant index: %v", err)
	}
	variant.CASHash = obj.SHA256
	return &variant
}

// A translation variant produced from a different transcript than the one the run pinned must never
// be adapted for that run: the run's transcript pin is the authority (#153).
func TestTranslationService_AdaptDubScript_CrossRunTranscriptMismatch_FailsClosed(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()
	assetID := uuid.NewString()
	runID := "run-transcript-mismatch"
	now := time.Now().UTC()

	blocks := dubScriptSpeechBlocks([2]int64{0, 2000})
	seedDubScriptRunScaffold(t, db, casStore, ctx, assetID, runID, blocks)

	// The variant the run is handed names a transcript this run never pinned.
	foreignTranscript, err := casStore.Put(bytes.NewReader(mustJSON(t, domain.TranscriptArtifact{
		ID: "transcript-foreign", AssetID: assetID, RunID: "other-run",
		SpeechBlocks: dubScriptSpeechBlocks([2]int64{0, 3000}), CreatedAt: now,
	})))
	if err != nil {
		t.Fatalf("put foreign transcript: %v", err)
	}
	variant := putDubScriptTranslationVariant(t, db, casStore, ctx, assetID, runID, foreignTranscript.SHA256, blocks, now)

	_, err = svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: "job-" + runID, TargetLanguage: "vi",
		TranslationVariantCAS: variant.CASHash,
	})
	if err == nil || !errors.Is(err, domain.ErrTranscriptLineageMismatch) {
		t.Fatalf("expected ErrTranscriptLineageMismatch for a variant bound to another transcript, got %v", err)
	}
}

// An omitted translation_variant_cas must resolve from run-scoped evidence, never from the asset's
// latest translation: here the asset's latest variant was produced by a different run from a
// different transcript (#153).
func TestTranslationService_AdaptDubScript_OmittedCASNeverUsesAssetLatest(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()
	assetID := uuid.NewString()
	runID := "run-omitted-cas"
	now := time.Now().UTC()

	blocks := dubScriptSpeechBlocks([2]int64{0, 2000})
	runTranscriptCAS := seedDubScriptRunScaffold(t, db, casStore, ctx, assetID, runID, blocks)
	runVariant := putDubScriptTranslationVariant(t, db, casStore, ctx, assetID, runID, runTranscriptCAS, blocks, now)

	// A newer variant exists for the same asset from another run with a different transcript. It has
	// no run-scoped binding of its own, so it is reachable only through the asset-latest index.
	foreignTranscript, err := casStore.Put(bytes.NewReader(mustJSON(t, domain.TranscriptArtifact{
		ID: "transcript-latest", AssetID: assetID, RunID: "other-run",
		SpeechBlocks: dubScriptSpeechBlocks([2]int64{500, 2500}), CreatedAt: now.Add(time.Minute),
	})))
	if err != nil {
		t.Fatalf("put latest transcript: %v", err)
	}
	latest := putDubScriptTranslationVariant(t, db, casStore, ctx, assetID, "other-run", foreignTranscript.SHA256, blocks, now.Add(time.Minute))

	// Sanity: the asset-latest index really does point at the other run's variant.
	latestIdx, err := db.GetTranslationVariantIndex(ctx, assetID, "vi")
	if err != nil || latestIdx == nil {
		t.Fatalf("read asset-latest translation index: %v", err)
	}
	if latestIdx.CASHash != latest.CASHash {
		t.Fatalf("fixture precondition failed: asset-latest CAS %s is not the other run's variant %s", latestIdx.CASHash, latest.CASHash)
	}

	dubVariant, err := svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: "job-" + runID, TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AdaptDubScript with omitted translation_variant_cas failed: %v", err)
	}
	if dubVariant.TranslationVariantCAS != runVariant.CASHash {
		t.Fatalf("omitted CAS resolved to %s; expected this run's variant %s (asset-latest is %s)",
			dubVariant.TranslationVariantCAS, runVariant.CASHash, latest.CASHash)
	}
}

// Cross-speaker canonical next speech: the canonical timeline has another speech block before the
// other speaker's turn, but that block was not carried into the translation variant. Borrowing must
// stop at the canonical next speech, not jump to the next (different-speaker) translation row (#153).
func TestTranslationService_AdaptDubScript_CrossSpeakerCanonicalNextSpeech(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	adapter := setDubScriptSegmentTuning(svc)
	ctx := context.Background()
	assetID := uuid.NewString()
	runID := "run-cross-speaker"

	// Canonical speech: SPEAKER_00 [0,2000], SPEAKER_00 [3000,4000], SPEAKER_01 [5000,7000].
	blocks := dubScriptSpeechBlocks([2]int64{0, 2000}, [2]int64{3000, 4000}, [2]int64{5000, 7000})
	blocks[0].SpeakerID = "SPEAKER_00"
	blocks[1].SpeakerID = "SPEAKER_00"
	blocks[2].SpeakerID = "SPEAKER_01"
	variant := seedDubScriptAdaptationEnv(t, db, casStore, svc, ctx, assetID, runID, blocks)

	// The translation variant carries canonical block 0 and the other speaker's block 2; the
	// intervening canonical block 1 (3000-4000) produced no row.
	variant.Segments = []domain.TranslationSegment{
		variant.Segments[0],
		variant.Segments[2],
	}
	variantObj, err := casStore.Put(bytes.NewReader(mustJSON(t, *variant)))
	if err != nil {
		t.Fatalf("put variant: %v", err)
	}

	dubVariant, err := svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: "job-" + runID, TargetLanguage: "vi",
		TranslationVariantCAS: variantObj.SHA256,
	})
	if err != nil {
		t.Fatalf("AdaptDubScript failed: %v", err)
	}
	if len(dubVariant.Segments) != 2 {
		t.Fatalf("expected 2 dub segments, got %d", len(dubVariant.Segments))
	}
	// The adaptation derived 3000 - 2000 = 1000 from the canonical next speech block (the intervening
	// block 1), not 5000 - 2000 = 3000 from the next TranslationVariant row (the other speaker's
	// turn). Plus the last carried block has no later boundary, so it must not report a next turn.
	hasNext, gap, ok := adapter.sourceGapArgs(0)
	if !ok {
		t.Fatal("no adaptation request captured for segment 0")
	}
	if !hasNext || gap != 1000 {
		t.Errorf("expected canonical next-speech boundary (hasNext=true gap=1000), got hasNext=%v gap=%d", hasNext, gap)
	}
	hasNext, gap, ok = adapter.sourceGapArgs(1)
	if !ok {
		t.Fatal("no adaptation request captured for segment 1")
	}
	if hasNext || gap != 0 {
		t.Errorf("expected no borrowing past the final canonical boundary (hasNext=false gap=0), got hasNext=%v gap=%d", hasNext, gap)
	}
	if got := dubVariant.Segments[0].SourceGapAfterMs; got != 1000 {
		t.Errorf("expected recorded source gap 1000ms, got %d", got)
	}
	if got := dubVariant.Segments[1].SourceGapAfterMs; got != 0 {
		t.Errorf("expected no recorded source gap at the final boundary, got %d", got)
	}
}

// Singing/uncertain source audio constrains the boundary even when a later canonical speech block
// exists: the singing/music-vocal region is itself the next boundary, so the adaptation must not
// extend past it onto the preserved vocal (#153).
func TestTranslationService_AdaptDubScript_SingingBoundaryConstrainsBorrowing(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	adapter := setDubScriptSegmentTuning(svc)
	ctx := context.Background()
	assetID := uuid.NewString()
	runID := "run-singing-boundary"

	// Canonical speech: [0,2000] and [9000,10000]. A singing/music-vocal region sits at [3000,5000],
	// well before the next canonical speech block, so it is the nearest boundary for segment 0.
	blocks := dubScriptSpeechBlocks([2]int64{0, 2000}, [2]int64{9000, 10000})
	seedDubScriptAdaptationEnv(t, db, casStore, svc, ctx, assetID, runID, blocks)
	planWithSinging := domain.AudioRolePlan{
		ID: "plan-" + assetID, AssetID: assetID, CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 3000, EndMs: 5000, Role: domain.AudioRoleSingingMusicVocal},
			{StartMs: 9000, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planObj, err := casStore.Put(bytes.NewReader(mustJSON(t, planWithSinging)))
	if err != nil {
		t.Fatalf("put plan: %v", err)
	}
	if err := db.SaveAudioRolePlan(ctx, planWithSinging); err != nil {
		t.Fatalf("save plan: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "audio_role_plan", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: planObj.SHA256, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pin plan: %v", err)
	}

	dubVariant, err := svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: "job-" + runID, TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AdaptDubScript failed: %v", err)
	}
	// The singing region begins at 3000, so the boundary is 3000 (not the 9000ms speech block):
	// gap = 3000 - 2000 = 1000ms.
	hasNext, gap, ok := adapter.sourceGapArgs(0)
	if !ok {
		t.Fatal("no adaptation request captured for segment 0")
	}
	if !hasNext || gap != 1000 {
		t.Errorf("expected singing-constrained boundary (hasNext=true gap=1000), got hasNext=%v gap=%d", hasNext, gap)
	}
	if got := dubVariant.Segments[0].SourceGapAfterMs; got != 1000 {
		t.Errorf("expected singing-constrained source gap 1000ms, got %d", got)
	}
}
