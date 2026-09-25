package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	reviewSvc, db, casStore, _, assetID := setupFullReviewHarnessWithRender(t)
	return reviewSvc, db, casStore, assetID
}

func seedReviewRun(t *testing.T, db *storage.DB, assetID, runID string) string {
	t.Helper()
	ctx := context.Background()
	jobID := "job-" + runID
	now := time.Now().UTC()
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID: jobID, SourceAssetID: assetID, TargetLanguage: "vi", Status: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create review fixture job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now}); err != nil {
		t.Fatalf("create review fixture run: %v", err)
	}
	return jobID
}

// setupFullReviewHarnessWithRender additionally returns the render service a region
// correction now drives, so a test can pin the composition backend (or fail inside it).
func setupFullReviewHarnessWithRender(t *testing.T) (*service.ReviewService, *storage.DB, *cas.Store, *service.RenderService, string) {
	t.Helper()
	reviewSvc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()

	srcObj, err := casStore.Put(bytes.NewReader([]byte("dummy video media bytes 12345")))
	if err != nil {
		t.Fatalf("put dummy video media: %v", err)
	}

	wavBytes := media.GeneratePCM16WAV(16000, 1, 35000)
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
		ID:                     "pf-" + assetID,
		AssetID:                assetID,
		DurationMs:             35000,
		DurationSec:            35.0,
		Width:                  1080,
		Height:                 1920,
		FrameRate:              30.0,
		NormalizedAudioSHA256:  wavObj.SHA256,
		NormalizedAudioCASPath: wavObj.Path,
		CreatedAt:              time.Now().UTC(),
	})

	rolePlan := domain.AudioRolePlan{
		ID:      "role-plan-" + assetID,
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	}
	roleBytes, _ := json.Marshal(rolePlan)
	roleObj, err := casStore.Put(bytes.NewReader(roleBytes))
	if err != nil {
		t.Fatalf("put audio role plan: %v", err)
	}
	rolePlan.CASHash = roleObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, rolePlan); err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}
	if err := db.SaveAudioRolePlanIndex(ctx, storage.AudioRolePlanIndex{
		ID:             rolePlan.ID,
		AssetID:        rolePlan.AssetID,
		ProviderID:     rolePlan.ProviderID,
		ModelName:      rolePlan.ModelName,
		ModelVersion:   rolePlan.ModelVersion,
		CASHash:        rolePlan.CASHash,
		ProvenanceHash: rolePlan.ProvenanceHash,
		CreatedAt:      rolePlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save audio role plan index: %v", err)
	}

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
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   35000,
			},
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: wavObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   35000,
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
	// Setup DubMixArtifact
	dubMix := domain.DubMixArtifact{
		ID:             "dubmix-" + assetID,
		SchemaVersion:  domain.DubMixSchemaVersion,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioCASHash:   wavObj.SHA256,
		OverallStatus:  "PASS",
		CreatedAt:      time.Now().UTC(),
	}
	dmBytes, _ := json.Marshal(dubMix)
	dmObj, _ := casStore.Put(bytes.NewReader(dmBytes))
	_ = db.SaveDubMixArtifactIndex(ctx, storage.DubMixArtifactIndex{
		ID:             dubMix.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        dmObj.SHA256,
		ProvenanceHash: "prov-dubmix-" + assetID,
		OverallStatus:  "PASS",
		CreatedAt:      dubMix.CreatedAt,
	})

	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		var segs []domain.TranslationSegment
		for _, s := range req.Segments {
			target := "Bản dịch: " + s.SourceText
			if s.SourceText == "关注" {
				target = "Theo dõi"
			}
			segs = append(segs, domain.TranslationSegment{
				Index:        s.Index,
				SourceText:   s.SourceText,
				TargetText:   target,
				StartMs:      s.StartMs,
				EndMs:        s.EndMs,
				QAConfidence: 0.95,
				PassedQAGate: true,
			})
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "qwen_trans",
			ModelVersion: "v1",
			Segments:     segs,
		}, nil
	}
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
	visSvc.SetTranslationService(transSvc)
	renderSvc := service.NewRenderService(db, casStore)
	// A region correction renders the preview the operator will see. Composition stays
	// deterministic here (no ffmpeg, no real media) so these tests observe the
	// correction's own state transitions instead of a renderer.
	renderSvc.SetCustomComposer(mockPreviewComposer)

	reviewSvc.SetTranslationService(transSvc)
	reviewSvc.SetDubbingService(dubSvc)
	reviewSvc.SetAudioMixService(mixSvc)
	reviewSvc.SetVisualTextService(visSvc)
	reviewSvc.SetRenderService(renderSvc)

	return reviewSvc, db, casStore, renderSvc, assetID
}

func pinReviewTranslationTranscript(t *testing.T, casStore *cas.Store, v *domain.TranslationVariant) {
	t.Helper()
	blocks := make([]domain.SpeechBlock, 0, len(v.Segments))
	inputSegments := make([]domain.TranslationInputSegment, 0, len(v.Segments))
	for _, seg := range v.Segments {
		blocks = append(blocks, domain.SpeechBlock{Index: seg.Index, StartMs: seg.StartMs, EndMs: seg.EndMs, SourceText: seg.SourceText, SpeakerID: seg.SpeakerID, SegmentType: domain.SpeechBlockTypeSpeech})
		inputSegments = append(inputSegments, domain.TranslationInputSegment{Index: seg.Index, SourceText: seg.SourceText, SpeakerID: seg.SpeakerID, StartMs: seg.StartMs, EndMs: seg.EndMs})
	}
	artifact := domain.TranscriptArtifact{AssetID: v.AssetID, SpeechBlocks: blocks}
	b, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal review transcript: %v", err)
	}
	obj, err := casStore.Put(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("put review transcript: %v", err)
	}
	v.TranscriptArtifactCAS = obj.SHA256
	if v.EffectiveGlossary.Hash == "" {
		glossaryBytes, err := json.Marshal([]domain.GlossaryEntry{})
		if err != nil {
			t.Fatalf("marshal empty review glossary: %v", err)
		}
		glossaryHash := sha256.Sum256(glossaryBytes)
		v.EffectiveGlossary.Hash = hex.EncodeToString(glossaryHash[:])
	}
	if v.InputHash == "" {
		payload := struct {
			SourceLanguage        string                           `json:"source_language"`
			Segments              []domain.TranslationInputSegment `json:"segments"`
			EffectiveGlossaryHash string                           `json:"effective_glossary_hash,omitempty"`
		}{
			SourceLanguage:        strings.ToLower(strings.TrimSpace(v.SourceLanguage)),
			Segments:              inputSegments,
			EffectiveGlossaryHash: v.EffectiveGlossary.Hash,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal review translation input hash: %v", err)
		}
		h := sha256.Sum256(data)
		v.InputHash = hex.EncodeToString(h[:])
	}
}

// mockPreviewComposer writes a deterministic placeholder instead of invoking a real
// renderer, mirroring the composition seam the seam-1 tests already use.
func mockPreviewComposer(_ context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
	payload := []byte("MOCK_COMPOSED_PREVIEW_BYTES")
	if err := os.WriteFile(req.OutputPath, payload, 0o644); err != nil {
		return nil, err
	}
	return &media.CompositionResult{
		OutputPath: req.OutputPath,
		ByteSize:   int64(len(payload)),
		DurationMs: 1000,
		Renderer:   "mock-preview-composer",
	}, nil
}

func bindBaselineDubMixToRun(t *testing.T, db *storage.DB, assetID, targetLang, runID string) string {
	t.Helper()
	ctx := context.Background()
	latest, err := db.GetDubMixArtifactIndex(ctx, assetID, targetLang)
	if err != nil {
		t.Fatalf("get baseline dub mix index: %v", err)
	}
	createdAt := time.Now().UTC()
	if err := db.SaveDubMixArtifactIndex(ctx, storage.DubMixArtifactIndex{
		ID:             "dubmix-" + runID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: targetLang,
		CASHash:        latest.CASHash,
		ProvenanceHash: "prov-dubmix-" + runID,
		OverallStatus:  latest.OverallStatus,
		CreatedAt:      createdAt,
	}); err != nil {
		t.Fatalf("bind baseline dub mix to run %s: %v", runID, err)
	}
	return latest.CASHash
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

// A replay run consumes the dub stage's artifact without owning a dub_segments_variants row of its own, so
// a run-scoped projection found nothing and the blocker never reached the operator: the run shipped with the
// source dialogue stripped and no voice, and the queue showed only the unrelated OCR items (live evidence,
// run 959e8dab). The projection must read the variant through the run's own stage execution.
func TestReviewService_ReplayRunProjectsDubOverrunFromStageArtifact(t *testing.T) {
	svc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()
	const originRun, replayRun = "run-dub-origin", "run-dub-replay"

	dubSegVar := domain.DubSegmentsVariant{
		ID:             "dub-seg-replay-1",
		AssetID:        assetID,
		RunID:          originRun,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-dub-replay-1",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              12400,
				SlotDurationMs:     12400,
				MeasuredDurationMs: 13520,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubSegBytes, _ := json.Marshal(dubSegVar)
	dubSegObj, _ := casStore.Put(bytes.NewReader(dubSegBytes))
	if err := db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dubSegVar.ID,
		AssetID:        assetID,
		RunID:          originRun,
		TargetLanguage: "vi",
		CASHash:        dubSegObj.SHA256,
		ProvenanceHash: dubSegVar.ProvenanceHash,
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save dub segments index: %v", err)
	}

	// The replay run's own record: it ran dub_synthesize and consumed that artifact.
	now := time.Now().UTC()
	job := domain.LocalizationJob{ID: "job-dub-replay", SourceAssetID: assetID, TargetLanguage: "vi", Status: "running", CreatedAt: now}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: replayRun, JobID: job.ID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: replayRun, Stage: "dub_synthesize", Status: "succeeded",
		ArtifactSHA256: dubSegObj.SHA256, StartedAt: &now, CompletedAt: &now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create stage execution: %v", err)
	}

	items, err := svc.ProjectReviewItemsForRun(ctx, assetID, "vi", replayRun)
	if err != nil {
		t.Fatalf("ProjectReviewItemsForRun failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected the replay run to project its dub overrun, got %d item(s): %+v", len(items), items)
	}
	if items[0].Type != domain.ReviewItemTypeTTSOverrun || items[0].Severity != "blocker" {
		t.Errorf("expected a blocker TTS overrun item, got type=%s severity=%s", items[0].Type, items[0].Severity)
	}
	if items[0].StartMs != 0 || items[0].EndMs != 12400 {
		t.Errorf("item must carry the segment slot it names, got %d-%d", items[0].StartMs, items[0].EndMs)
	}
}

func TestReviewService_ManualOverride_TTSOverrunRemainsPending(t *testing.T) {
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

	// 2. Timing/coverage blockers are not waivable by generic manual override.
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
	if _, err := svc.RecordManualOverride(ctx, overrideInput); err == nil || !strings.Contains(err.Error(), "hard timing blocker") {
		t.Fatalf("expected hard timing blocker rejection, got %v", err)
	}

	// 3. The blocker remains pending until correction/re-synthesis produces selectable audio.
	pendingAfter, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}
	if len(pendingAfter) != 1 || pendingAfter[0].ID != flaggedItem.ID {
		t.Errorf("expected blocker to remain pending, got %+v", pendingAfter)
	}

	// 4. No override audit changes the item's status.
	allItems, err := svc.ProjectAllReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("ProjectAllReviewItems failed: %v", err)
	}
	if len(allItems) != 1 {
		t.Fatalf("expected 1 item in full projection, got %d", len(allItems))
	}
	if allItems[0].Status != domain.ReviewItemStatusPending {
		t.Errorf("expected status pending, got %s", allItems[0].Status)
	}
}

func TestReviewService_ManualOverride_IsolatedByRunForSharedSourceException(t *testing.T) {
	svc, db, _, assetID := setupReviewTestHarness(t)
	ctx := context.Background()
	const (
		runA = "run-review-override-a"
		runB = "run-review-override-b"
	)

	// Source analysis is intentionally asset-scoped, so both runs see the same
	// deterministic ReviewItem ID. Resolution, however, is a run-scoped operator
	// decision and must never bleed from run A into run B.
	if err := db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID:      "role-plan-shared-review",
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 1200, EndMs: 2400, Role: domain.AudioRoleUncertain},
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveAudioRolePlan failed: %v", err)
	}

	itemsA, err := svc.ProjectReviewItemsForRun(ctx, assetID, "vi", runA)
	if err != nil {
		t.Fatalf("ProjectReviewItemsForRun A failed: %v", err)
	}
	itemsB, err := svc.ProjectReviewItemsForRun(ctx, assetID, "vi", runB)
	if err != nil {
		t.Fatalf("ProjectReviewItemsForRun B failed: %v", err)
	}
	if len(itemsA) != 1 || len(itemsB) != 1 || itemsA[0].ID != itemsB[0].ID {
		t.Fatalf("expected one shared source exception in both runs, A=%+v B=%+v", itemsA, itemsB)
	}

	if _, err := svc.RecordManualOverride(ctx, service.ManualOverrideInput{
		RunID:          runA,
		AssetID:        assetID,
		TargetLanguage: "vi",
		ReviewItemID:   itemsA[0].ID,
		Reason:         "Accepted for run A only",
		Operator:       "operator-a",
	}); err != nil {
		t.Fatalf("RecordManualOverride run A failed: %v", err)
	}

	pendingA, err := svc.ProjectReviewItemsForRun(ctx, assetID, "vi", runA)
	if err != nil {
		t.Fatalf("ProjectReviewItemsForRun A after override failed: %v", err)
	}
	if len(pendingA) != 0 {
		t.Fatalf("run A should have no pending shared exception after override: %+v", pendingA)
	}

	pendingB, err := svc.ProjectReviewItemsForRun(ctx, assetID, "vi", runB)
	if err != nil {
		t.Fatalf("ProjectReviewItemsForRun B after run A override failed: %v", err)
	}
	if len(pendingB) != 1 || pendingB[0].ID != itemsB[0].ID || pendingB[0].Status != domain.ReviewItemStatusPending {
		t.Fatalf("run B shared exception must remain pending, got %+v", pendingB)
	}

	allA, err := svc.ProjectAllReviewItemsForRun(ctx, assetID, "vi", runA)
	if err != nil {
		t.Fatalf("ProjectAllReviewItemsForRun A failed: %v", err)
	}
	if len(allA) != 1 || allA[0].Status != domain.ReviewItemStatusManualOverride {
		t.Fatalf("run A full projection should retain manual_override audit status, got %+v", allA)
	}
	allB, err := svc.ProjectAllReviewItemsForRun(ctx, assetID, "vi", runB)
	if err != nil {
		t.Fatalf("ProjectAllReviewItemsForRun B failed: %v", err)
	}
	if len(allB) != 1 || allB[0].Status != domain.ReviewItemStatusPending {
		t.Fatalf("run B full projection should remain pending, got %+v", allB)
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
	runID := "run-correct-target"
	jobID := seedReviewRun(t, db, assetID, runID)

	// 1. Setup initial TranslationVariant
	transVar := domain.TranslationVariant{
		ID:             "trans-init-1",
		SchemaVersion:  domain.TranslationSchemaVersion,
		ContractID:     service.TranslationContractID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
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
	pinReviewTranslationTranscript(t, casStore, &transVar)
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
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
		RunID:                 runID,
		JobID:                 jobID,
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
		RunID:          runID,
		JobID:          jobID,
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
		RunID:          runID,
		JobID:          jobID,
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
		RunID:          runID,
		JobID:          jobID,
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
		RunID:              runID,
		JobID:              jobID,
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

// Segment indices are the source speech-block indices, so they are sparse: a clip whose speech blocks are
// 0 and 6 carries those two indices in a two-element slice. Resolving the correction by slice position
// rejected the operator's edit of the overrunning segment (live evidence, run eec68c8d: "segment index 6
// out of bounds (total 4)") and, worse, would have patched the wrong segment whenever the index happened
// to fit.
func TestReviewService_CorrectTargetText_ResolvesSparseSegmentIndices(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-sparse"
	jobID := seedReviewRun(t, db, assetID, runID)

	transVar := domain.TranslationVariant{
		ID:             "trans-sparse-1",
		SchemaVersion:  domain.TranslationSchemaVersion,
		ContractID:     service.TranslationContractID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-sparse",
		OverallQAScore: 0.5,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "点击右上角", TargetText: "Nhấn vào góc trên bên phải của màn hình", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
			{Index: 6, SourceText: "以及独特的水下景观", TargetText: "cùng cảnh quan dưới nước vô cùng độc đáo", StartMs: 21840, EndMs: 27040, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: transVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", CASHash: tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash, OverallQAScore: transVar.OverallQAScore, CreatedAt: transVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save translation index: %v", err)
	}

	dubScriptVar := domain.DubScriptVariant{
		ID:                    "dubscript-sparse-1",
		AssetID:               assetID,
		RunID:                 runID,
		JobID:                 jobID,
		TargetLanguage:        "vi",
		SourceLanguage:        "zh",
		TranslationVariantCAS: tObj.SHA256,
		ProvenanceHash:        "prov-dubscript-sparse",
		OverallQAScore:        0.5,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SourceText: "点击右上角", MeaningText: transVar.Segments[0].TargetText, SpokenText: transVar.Segments[0].TargetText, SlotDurationMs: 1500, PassedQAGate: true, RequiresReview: true, ReviewReason: "DURATION_OVERRUN"},
			{Index: 6, SourceText: "以及独特的水下景观", MeaningText: transVar.Segments[1].TargetText, SpokenText: transVar.Segments[1].TargetText, SlotDurationMs: 5200, PassedQAGate: true, RequiresReview: true, ReviewReason: "DURATION_OVERRUN"},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScriptVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScriptVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScriptVar.ProvenanceHash, OverallQAScore: dubScriptVar.OverallQAScore, CreatedAt: dubScriptVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID:              runID,
		JobID:              jobID,
		AssetID:            assetID,
		TargetLanguage:     "vi",
		SegmentIndex:       6,
		NewTargetText:      "cùng cảnh quan dưới nước độc đáo",
		SpokenTextOverride: "cùng cảnh quan dưới nước độc đáo",
		Reason:             "Shortened for the 5.2s slot",
		Operator:           "editor_monet",
	})
	if err != nil {
		t.Fatalf("CorrectTargetText must resolve segment index 6 to its position, got: %v", err)
	}

	rc, err := casStore.Get(res.TranslationVariantCAS)
	if err != nil {
		t.Fatalf("load corrected translation variant: %v", err)
	}
	defer rc.Close()
	var corrected domain.TranslationVariant
	if err := json.NewDecoder(rc).Decode(&corrected); err != nil {
		t.Fatalf("decode corrected translation variant: %v", err)
	}
	if len(corrected.Segments) != 2 {
		t.Fatalf("expected both segments preserved, got %d", len(corrected.Segments))
	}
	if corrected.Segments[1].Index != 6 || corrected.Segments[1].TargetText != "cùng cảnh quan dưới nước độc đáo" {
		t.Errorf("segment 6 must carry the corrected text, got index=%d text=%q", corrected.Segments[1].Index, corrected.Segments[1].TargetText)
	}
	if corrected.Segments[0].TargetText != transVar.Segments[0].TargetText {
		t.Errorf("segment 0 must stay untouched, got %q", corrected.Segments[0].TargetText)
	}
}

// A mix refusal parks a run before its visual stage, so a text correction that answers that refusal runs
// while no text region plan exists yet. Live evidence (run 27a758e6): all three tts_overrun corrections
// failed with `text region plan not found: record not found`, leaving the operator holding a refusal they
// could not act on. The correction must rebuild translation, dub script, fit and mix, and leave the visual
// track and render plan to the resumed pipeline, which runs them after the mix.
func TestReviewService_CorrectTargetText_BeforeVisualStage_RebuildsDubOnly(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-premix"
	jobID := seedReviewRun(t, db, assetID, runID)

	transVar := domain.TranslationVariant{
		ID:             "trans-premix-1",
		SchemaVersion:  domain.TranslationSchemaVersion,
		ContractID:     service.TranslationContractID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-premix",
		OverallQAScore: 0.5,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "把手机放在水盆下方", TargetText: "Đặt điện thoại ở phía dưới chậu nước, bạn sẽ có được thước phim đậm chất điện ảnh", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: transVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", CASHash: tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash, OverallQAScore: transVar.OverallQAScore, CreatedAt: transVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save translation index: %v", err)
	}

	dubScriptVar := domain.DubScriptVariant{
		ID:                    "dubscript-premix-1",
		AssetID:               assetID,
		RunID:                 runID,
		JobID:                 jobID,
		TargetLanguage:        "vi",
		SourceLanguage:        "zh",
		TranslationVariantCAS: tObj.SHA256,
		ProvenanceHash:        "prov-dubscript-premix",
		OverallQAScore:        0.5,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SourceText: "把手机放在水盆下方", MeaningText: transVar.Segments[0].TargetText, SpokenText: transVar.Segments[0].TargetText, SlotDurationMs: 1500, PassedQAGate: true, RequiresReview: true, ReviewReason: "DURATION_OVERRUN"},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScriptVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScriptVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScriptVar.ProvenanceHash, OverallQAScore: dubScriptVar.OverallQAScore, CreatedAt: dubScriptVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	// The pipeline stopped at audio_mix, so text_detection never ran and no plan exists to localize against.
	if err := db.DeleteTextRegionPlanIndex(ctx, assetID, "prov-text-"+assetID); err != nil {
		t.Fatalf("delete seeded text region plan: %v", err)
	}

	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID:              runID,
		JobID:              jobID,
		AssetID:            assetID,
		TargetLanguage:     "vi",
		SegmentIndex:       0,
		NewTargetText:      "Đặt điện thoại dưới chậu nước, bạn có thước phim điện ảnh",
		SpokenTextOverride: "Đặt điện thoại dưới chậu nước, bạn có thước phim điện ảnh",
		Reason:             "tts_overrun: shorten to fit the slot",
		Operator:           "editor_monet",
	})
	if err != nil {
		t.Fatalf("a correction answering a mix refusal must not require a text region plan: %v", err)
	}
	if res.DubMixCAS == "" {
		t.Errorf("expected the mix to be rebuilt: %+v", res)
	}
	if res.LocalizedSubtitleCAS != "" || res.RenderPlanCAS != "" {
		t.Errorf("the visual stage never ran, so no visual track or render plan may be claimed: %+v", res)
	}
}

// A resumed run replays whatever its stage rows recorded. The correction rebuilds the dub segments and
// the mix, so it must record them: otherwise the resumed run reuses the pre-correction artifact and the
// mixer refuses the very overrun the operator just fixed (live evidence: run 27a758e6 resumed from
// `segment 0 measured 13280ms exceeds slot 12400ms` after all three segments had been shortened to fit).
func TestReviewService_CorrectTargetText_RecordsRebuiltStagesForResume(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-resume-1"
	now := time.Now().UTC()
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID: "job-resume-1", SourceAssetID: assetID, TargetLanguage: "vi",
		Status: "interrupted", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "job-resume-1", Status: "interrupted", CreatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	staleCAS := "stale-dub-synthesize-cas"
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: "se-stale", RunID: runID, Stage: "dub_synthesize", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: staleCAS, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record stale stage: %v", err)
	}

	transVar := domain.TranslationVariant{
		ID: "trans-resume-1", SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID, AssetID: assetID, TargetLanguage: "vi", SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-resume", OverallQAScore: 0.5,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "点击右上角", TargetText: "Nhấn vào góc trên bên phải của màn hình", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: transVar.ID, RunID: runID, AssetID: assetID, TargetLanguage: "vi", CASHash: tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash, OverallQAScore: transVar.OverallQAScore, CreatedAt: transVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save translation index: %v", err)
	}
	dubScriptVar := domain.DubScriptVariant{
		ID: "dubscript-resume-1", RunID: runID, AssetID: assetID, TargetLanguage: "vi", SourceLanguage: "zh",
		TranslationVariantCAS: tObj.SHA256, ProvenanceHash: "prov-dubscript-resume", OverallQAScore: 0.5,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SourceText: "点击右上角", MeaningText: transVar.Segments[0].TargetText, SpokenText: transVar.Segments[0].TargetText, SlotDurationMs: 1500, PassedQAGate: true, RequiresReview: true, ReviewReason: "DURATION_OVERRUN"},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScriptVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScriptVar.ID, RunID: runID, AssetID: assetID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScriptVar.ProvenanceHash, OverallQAScore: dubScriptVar.OverallQAScore, CreatedAt: dubScriptVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}
	// The dubbing service resolves the voice assignment run-bound, as the live pipeline does.
	va := domain.VoiceAssignment{
		ID: "va-resume-1", RunID: runID, AssetID: assetID, TargetLanguage: "vi",
		Assignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {ID: "vi-preset-1", Name: "Preset Voice 1", Language: "vi", ProviderID: "fake_tts"},
		},
		CreatedAt: time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: va.ID, RunID: runID, AssetID: assetID, TargetLanguage: "vi", CASHash: vaObj.SHA256,
		ProvenanceHash: "prov-va-resume", CreatedAt: va.CreatedAt,
	}); err != nil {
		t.Fatalf("save run-bound voice assignment: %v", err)
	}

	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", SpokenTextOverride: "Nhấn góc trên",
		Reason: "Shortened for the 1.5s slot", Operator: "editor_monet",
	})
	if err != nil {
		t.Fatalf("CorrectTargetText failed: %v", err)
	}

	latest := map[string]string{}
	stages, err := db.ListStageExecutions(ctx, runID)
	if err != nil {
		t.Fatalf("list stage executions: %v", err)
	}
	for _, st := range stages {
		latest[st.Stage] = st.ArtifactSHA256
	}
	if latest["dub_synthesize"] != res.DubSegmentsVariantCAS || latest["dub_synthesize"] == staleCAS {
		t.Errorf("resumed run would replay dub segments %q, want the corrected %q", latest["dub_synthesize"], res.DubSegmentsVariantCAS)
	}
	if latest["audio_mix"] != res.DubMixCAS {
		t.Errorf("resumed run would replay dub mix %q, want the corrected %q", latest["audio_mix"], res.DubMixCAS)
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
	pinReviewTranslationTranscript(t, casStore, &transVar)
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

func TestReviewService_CorrectRegionGeometry_FailClosedOnPersistenceErrors(t *testing.T) {
	svc, _, _, assetID := setupFullReviewHarness(t)
	ctx := context.Background()

	newRole := domain.TextRoleSemanticText
	// 1. Missing TextRegionPlan index in DB
	_, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "region-1",
				NewRole:  &newRole,
			},
		},
		Reason:   "Test missing plan",
		Operator: "tester",
	})
	if err == nil {
		t.Errorf("expected error when TextRegionPlan is missing in DB, got nil")
	}
}

func TestReviewService_ReassignVoice_VisualTrackFailClosed(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-reassign-fc-01"
	jobID := "job-reassign-" + uuid.NewString()[:8]
	now := time.Now().UTC()
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "running",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	transVar := domain.TranslationVariant{
		ID:             "trans-fc-1",
		SchemaVersion:  domain.TranslationSchemaVersion,
		ContractID:     service.TranslationContractID,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "测试", TargetText: "Thử nghiệm", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1500, PassedQAGate: true, QAConfidence: 0.95},
		},
		CreatedAt: now,
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	transBytes, _ := json.Marshal(transVar)
	transObj, err := casStore.Put(bytes.NewReader(transBytes))
	if err != nil {
		t.Fatalf("put translation contract: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: "transcript-fc-1", AssetID: assetID, RunID: runID, CASHash: transVar.TranscriptArtifactCAS,
		ProvenanceHash: "prov-transcript-fc-1", CreatedAt: now,
	}); err != nil {
		t.Fatalf("save transcript index: %v", err)
	}
	// Setup DubScriptVariant in DB and CAS
	dsVar := domain.DubScriptVariant{
		ID:                    "dubscript-fc-1",
		SchemaVersion:         domain.DubScriptSchemaVersion,
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        "vi",
		SourceLanguage:        "zh",
		TranslationVariantCAS: transObj.SHA256,
		ProvenanceHash:        "prov-ds-fc-1",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SourceText:          "测试",
				SpokenText:          "Thử nghiệm",
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               1500,
				SlotDurationMs:      1500,
				EstimatedDurationMs: 1000,
				PassedQAGate:        true,
				QAConfidence:        0.95,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dsVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             dsVar.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dsObj.SHA256,
		ProvenanceHash: dsVar.ProvenanceHash,
		CreatedAt:      dsVar.CreatedAt,
	})

	// Setup initial VoiceAssignment for runID pinned to the exact script/transcript lineage.
	va := domain.VoiceAssignment{
		ID:                    "va-fc-1",
		SchemaVersion:         domain.VoiceAssignmentSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   dsObj.SHA256,
		TranscriptArtifactCAS: transVar.TranscriptArtifactCAS,
		Assignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vi-preset-1",
				Name:       "Preset Voice 1",
				Language:   "vi",
				Gender:     "female",
				ProviderID: "fake_tts",
			},
		},
		ProvenanceHash: "prov-va-fc-1",
		FrozenAt:       time.Now().UTC(),
		CreatedAt:      time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	assignmentsJSON, _ := json.Marshal(va.Assignments)
	_ = db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID:              va.ID,
		AssetID:         assetID,
		RunID:           runID,
		TargetLanguage:  "vi",
		CASHash:         vaObj.SHA256,
		ProvenanceHash:  va.ProvenanceHash,
		AssignmentsJSON: string(assignmentsJSON),
		CreatedAt:       va.CreatedAt,
	})

	// Case A: Corrupt CAS hash in LocalizedVisualTrackIndex
	_ = db.SaveLocalizedVisualTrackIndex(ctx, storage.LocalizedVisualTrackIndex{
		ID:                "vis-corrupt-1",
		AssetID:           assetID,
		RunID:             runID,
		TargetLanguage:    "vi",
		TextRegionPlanCAS: "some_plan_cas",
		CASHash:           "non_existent_visual_cas_hash_999",
		ProvenanceHash:    "prov-vis-corrupt-1",
		CreatedAt:         time.Now().UTC(),
	})

	_, err = svc.ReassignVoice(ctx, service.VoiceReassignCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vi-preset-2",
				Name:       "Preset Voice 2",
				Language:   "vi",
				Gender:     "male",
				ProviderID: "fake_tts",
			},
		},
		Reason:   "Test corrupt visual track CAS",
		Operator: "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "load localized visual track from CAS") {
		t.Fatalf("expected fail-closed error on corrupt visual track CAS, got: %v", err)
	}

	// Case B: Corrupt JSON payload in CAS
	badJSONObj, _ := casStore.Put(bytes.NewReader([]byte("{invalid-json-bytes-for-visual-track")))
	_ = db.SaveLocalizedVisualTrackIndex(ctx, storage.LocalizedVisualTrackIndex{
		ID:                "vis-corrupt-2",
		AssetID:           assetID,
		RunID:             runID,
		TargetLanguage:    "vi",
		TextRegionPlanCAS: "some_plan_cas",
		CASHash:           badJSONObj.SHA256,
		ProvenanceHash:    "prov-vis-corrupt-2",
		CreatedAt:         time.Now().UTC(),
	})

	_, err = svc.ReassignVoice(ctx, service.VoiceReassignCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vi-preset-3",
				Name:       "Preset Voice 3",
				Language:   "vi",
				Gender:     "female",
				ProviderID: "fake_tts",
			},
		},
		Reason:   "Test corrupt visual track JSON decode",
		Operator: "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "decode localized visual track") {
		t.Fatalf("expected fail-closed error on corrupt visual track JSON, got: %v", err)
	}

	// Case C: Valid LocalizedVisualTrack in CAS
	validVisTrack := domain.LocalizedVisualTrack{
		ID:             "vis-valid-1",
		SchemaVersion:  domain.LocalizedVisualTrackSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-vis-valid-1",
		SubtitleCues: []domain.SubtitleCue{
			{
				StartMs: 0,
				EndMs:   1500,
				Text:    "Thử nghiệm phụ đề",
				X:       100,
				Y:       200,
				Width:   300,
				Height:  50,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	validVisBytes, _ := json.Marshal(validVisTrack)
	validVisObj, _ := casStore.Put(bytes.NewReader(validVisBytes))
	_ = db.SaveLocalizedVisualTrackIndex(ctx, storage.LocalizedVisualTrackIndex{
		ID:                validVisTrack.ID,
		AssetID:           assetID,
		RunID:             runID,
		TargetLanguage:    "vi",
		TextRegionPlanCAS: "some_plan_cas",
		CASHash:           validVisObj.SHA256,
		ProvenanceHash:    "prov-vis-valid-1",
		CreatedAt:         validVisTrack.CreatedAt,
	})

	res, err := svc.ReassignVoice(ctx, service.VoiceReassignCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vi-preset-4",
				Name:       "Preset Voice 4",
				Language:   "vi",
				Gender:     "female",
				ProviderID: "fake_tts",
			},
		},
		Reason:   "Valid voice reassign with visual track",
		Operator: "tester",
	})
	if err != nil {
		t.Fatalf("expected success with valid visual track, got err: %v", err)
	}
	if res.RenderPlanCAS == "" {
		t.Fatalf("expected non-empty RenderPlanCAS")
	}

	stages, err := db.ListStageExecutions(ctx, runID)
	if err != nil {
		t.Fatalf("ListStageExecutions failed: %v", err)
	}
	var previewStage *domain.StageExecution
	for _, st := range stages {
		if st.Stage == "render_preview" {
			previewStage = &st
		}
	}
	if previewStage == nil {
		t.Fatalf("expected render_preview stage execution to be recorded")
	}
	if previewStage.Status != domain.StageStatusQueued || previewStage.ArtifactSHA256 != "" {
		t.Fatalf("expected render_preview stage to be invalidated (status=queued, empty artifact), got status=%s artifact=%s",
			previewStage.Status, previewStage.ArtifactSHA256)
	}
}

func TestReviewService_CorrectRegionGeometry_DubMixStorageErrorAndNoDubPreservation(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-dubmix-test-01"
	runACAS := bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	// Newer run B for the same asset/target must not replace run A's DubMix in
	// a selected-run correction.
	runBArtifact := domain.DubMixArtifact{
		ID:                  "dubmix-run-b-newer",
		SchemaVersion:       domain.DubMixSchemaVersion,
		AssetID:             assetID,
		RunID:               "run-dubmix-test-02",
		TargetLanguage:      "vi",
		AudioCASHash:        "audio-run-b",
		SampleRate:          16000,
		Channels:            1,
		Format:              "wav",
		DurationMs:          1000,
		DialogueSuppressed:  true,
		SoundtrackPreserved: true,
		OverallStatus:       "PASS",
		CreatedAt:           time.Now().UTC().Add(time.Minute),
	}
	runBBytes, _ := json.Marshal(runBArtifact)
	runBObj, err := casStore.Put(bytes.NewReader(runBBytes))
	if err != nil {
		t.Fatalf("put newer run B dub mix metadata: %v", err)
	}
	if err := db.SaveDubMixArtifactIndex(ctx, storage.DubMixArtifactIndex{
		ID:             runBArtifact.ID,
		AssetID:        assetID,
		RunID:          runBArtifact.RunID,
		TargetLanguage: "vi",
		CASHash:        runBObj.SHA256,
		ProvenanceHash: "prov-dubmix-run-b-newer",
		OverallStatus:  "PASS",
		CreatedAt:      runBArtifact.CreatedAt,
	}); err != nil {
		t.Fatalf("save newer run B dub mix index: %v", err)
	}

	// 1. Setup TextRegionPlan with a valid region
	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-dm-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-dm-1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "region-dm-1",
				Text:        "关注",
				Role:        domain.TextRoleSemanticText,
				FirstSeenMs: 0,
				LastSeenMs:  1500,
			},
		},
		CreatedAt: time.Now().UTC(),
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

	// Case A: Valid DubMix index in DB -> passed to RenderPlan
	newRole := domain.TextRoleSemanticText
	res, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "region-dm-1",
				NewRole:  &newRole,
			},
		},
		Reason:   "Test dub mix pass-through",
		Operator: "tester",
	})
	if err != nil {
		t.Fatalf("expected success with existing dub mix index, got err: %v", err)
	}
	if res.RenderPlanCAS == "" {
		t.Fatalf("expected non-empty RenderPlanCAS")
	}

	rcPlan, err := casStore.Get(res.RenderPlanCAS)
	if err != nil {
		t.Fatalf("load render plan from CAS: %v", err)
	}
	var rPlan domain.RenderPlan
	_ = json.NewDecoder(rcPlan).Decode(&rPlan)
	rcPlan.Close()
	if rPlan.DubMixCASHash == "" {
		t.Errorf("expected non-empty DubMixCASHash in RenderPlan")
	}
	if rPlan.DubMixCASHash != runACAS {
		t.Fatalf("selected run correction pinned DubMixCASHash=%s, want run A %s (newer run B=%s)", rPlan.DubMixCASHash, runACAS, runBObj.SHA256)
	}

	// Case B: Storage error on GetDubMixArtifactIndex -> must fail closed and propagate error
	_ = db.Close() // Force unexpected storage failure on closed DB
	_, err = svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "region-dm-1",
				NewRole:  &newRole,
			},
		},
		Reason:   "Test storage error propagation",
		Operator: "tester",
	})
	if err == nil {
		t.Fatalf("expected error on storage failure during CorrectRegionGeometry, got nil")
	}
}

func TestReviewService_CorrectRegionGeometry_MissingDubMixFailsClosed(t *testing.T) {
	svc, db, casStore, _ := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-dubmix-missing-01"
	newRole := domain.TextRoleSemanticText

	// Dedicated asset with NO DubMix index. A valid no-speech/no-dub run still executes
	// AudioMixService and persists a PASS passthrough DubMixArtifact, so its absence here is an
	// incomplete/invalid review state and must fail closed — not be treated as a no-dub success.
	assetIDNoDub := "asset-no-dub-01"
	srcObj, err := casStore.Put(bytes.NewReader([]byte("no-dub source media bytes")))
	if err != nil {
		t.Fatalf("put no-dub media: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetIDNoDub,
		SHA256:              srcObj.SHA256,
		ByteSize:            int64(len("no-dub source media bytes")),
		MimeType:            "video/mp4",
		OriginalFilename:    "video.mp4",
		RightsAttestationID: "att-review-001",
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create no-dub source asset: %v", err)
	}

	noDubPlan := domain.TextRegionPlan{
		ID:             "text-plan-nodub-1",
		AssetID:        assetIDNoDub,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-nodub-1",
		Regions: []domain.TrackedTextRegion{
			{ID: "region-nodub-1", Text: "关注", Role: domain.TextRoleSemanticText, FirstSeenMs: 0, LastSeenMs: 1500},
		},
		CreatedAt: time.Now().UTC(),
	}
	noDubBytes, _ := json.Marshal(noDubPlan)
	noDubObj, _ := casStore.Put(bytes.NewReader(noDubBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             noDubPlan.ID,
		AssetID:        assetIDNoDub,
		CASHash:        noDubObj.SHA256,
		ProvenanceHash: noDubPlan.ProvenanceHash,
		CreatedAt:      noDubPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save no-dub text region plan index: %v", err)
	}

	// No DubMix index exists for assetIDNoDub -> CorrectRegionGeometry must fail closed
	// BEFORE freezing any RenderPlan.
	_, err = svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetIDNoDub,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{RegionID: "region-nodub-1", NewRole: &newRole},
		},
		Reason:   "Missing DubMix index must fail closed",
		Operator: "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "dub mix") {
		t.Fatalf("expected fail-closed error on missing DubMix index, got: %v", err)
	}

	// Prove it failed BEFORE freezing RenderPlan: no RenderPlan row persisted.
	if idx, gerr := db.GetRenderPlanIndex(ctx, assetIDNoDub, "vi"); gerr == nil && idx != nil {
		t.Errorf("expected no RenderPlan frozen when DubMix is missing, got %+v", idx)
	}
}

func TestReviewService_CorrectRegionGeometry_ProtectedOcclusionRestoresCurrentPlan(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-occlusion-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	// A protected brand mark and a semantic region whose overlay can be dragged onto it.
	brandBox := domain.BoundingBox{X: 50, Y: 60, Width: 120, Height: 40}
	semBox := domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}
	srcPlan := domain.TextRegionPlan{
		ID:             "text-plan-occlusion-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-occlusion-1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:                "reg-brand",
				Text:              "SUPOR",
				Role:              domain.TextRoleBrandKeep,
				ProtectedMetadata: domain.ProtectedRegionMetadata{IsProtected: true, Reason: "brand_authenticity"},
				FirstSeenMs:       0,
				LastSeenMs:        1500,
				Keyframes:         []domain.RegionKeyframe{{TimestampMs: 0, Box: brandBox, Observed: true, Confidence: 0.98}},
			},
			{
				ID:          "reg-sem",
				Text:        "关注",
				Role:        domain.TextRoleSemanticText,
				FirstSeenMs: 0,
				LastSeenMs:  1500,
				Keyframes:   []domain.RegionKeyframe{{TimestampMs: 0, Box: semBox, Observed: true, Confidence: 0.9}},
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	srcBytes, _ := json.Marshal(srcPlan)
	srcObj, _ := casStore.Put(bytes.NewReader(srcBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             srcPlan.ID,
		AssetID:        assetID,
		CASHash:        srcObj.SHA256,
		ProvenanceHash: srcPlan.ProvenanceHash,
		CreatedAt:      srcPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}

	// Drag the semantic overlay onto the protected brand mark (-150 canonical px on Y).
	_, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{RegionID: "reg-sem", BoxDeltaY: -150},
		},
		Reason:   "Drag semantic overlay onto the protected brand mark",
		Operator: "tester",
	})
	if err == nil || !errors.Is(err, domain.ErrSubtitleOverlapsProtectedRegion) {
		t.Fatalf("expected fail-closed protected-region rejection, got: %v", err)
	}

	// Fail-closed means the rejected edit must not remain the asset's current plan.
	current, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current text region plan index: %v", err)
	}
	if current.ProvenanceHash != srcPlan.ProvenanceHash || current.CASHash != srcObj.SHA256 {
		t.Errorf("rejected edit stayed current: provenance=%s cas=%s, want %s / %s",
			current.ProvenanceHash, current.CASHash, srcPlan.ProvenanceHash, srcObj.SHA256)
	}

	// The restored plan is the usable base: a non-occluding reclassification still succeeds.
	brandRole := domain.TextRoleBrandKeep
	res, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{RegionID: "reg-sem", NewRole: &brandRole},
		},
		Reason:   "Recover after the rejected drag",
		Operator: "tester",
	})
	if err != nil {
		t.Fatalf("recovery correction on the restored plan failed: %v", err)
	}
	_ = res
}

func TestReviewService_CorrectRegionGeometry_OverrideMintsNewImmutablePlanIdentity(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-lineage-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	// Setup a source TextRegionPlan with one region to override.
	origProv := "prov-text-lineage-1"
	srcPlan := domain.TextRegionPlan{
		ID:             "text-plan-lineage-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: origProv,
		Regions: []domain.TrackedTextRegion{
			{
				ID:             "reg-lineage-1",
				Text:           "注意",
				Role:           domain.TextRoleUncertain,
				FirstSeenMs:    0,
				LastSeenMs:     1500,
				ReviewRequired: true,
				ReviewReason:   "uncertain_role",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	srcBytes, _ := json.Marshal(srcPlan)
	srcObj, _ := casStore.Put(bytes.NewReader(srcBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             srcPlan.ID,
		AssetID:        assetID,
		CASHash:        srcObj.SHA256,
		ProvenanceHash: origProv,
		CreatedAt:      srcPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}

	// Reclassify the uncertain region to semantic_text.
	semanticRole := domain.TextRoleSemanticText
	res, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{RegionID: "reg-lineage-1", NewRole: &semanticRole},
		},
		Reason:   "Reclassify uncertain region",
		Operator: "tester",
	})
	if err != nil {
		t.Fatalf("CorrectRegionGeometry failed: %v", err)
	}
	if res.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected auto_resolved, got: %s", res.Status)
	}

	// The overridden plan is latest.
	overIdx, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get latest plan index: %v", err)
	}
	if overIdx == nil {
		t.Fatalf("expected latest plan index")
	}
	if overIdx.ProvenanceHash == origProv {
		t.Errorf("overridden plan must NOT reuse source provenance")
	}
	if overIdx.ID == srcPlan.ID {
		t.Errorf("overridden plan must carry a NEW immutable ID")
	}

	// The source plan is still retrievable by its ORIGINAL provenance (never overwritten).
	srcIdx, err := db.GetTextRegionPlanByProvenance(ctx, origProv)
	if err != nil {
		t.Fatalf("source plan must remain retrievable by original provenance: %v", err)
	}
	if srcIdx == nil || srcIdx.CASHash != srcObj.SHA256 {
		t.Errorf("source plan index was overwritten: got %+v, want CAS %s", srcIdx, srcObj.SHA256)
	}

	// The operator's intent is recorded as an append-only audit row bound to the run.
	audits, err := db.GetReviewOverridesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("get region correction audit: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit row for the correction, got %d", len(audits))
	}
	audit := audits[0]
	if audit.Action != string(domain.ReviewOverrideActionRegionGeometry) {
		t.Errorf("expected region geometry audit action, got %q", audit.Action)
	}
	if audit.ItemType != domain.ReviewItemTypeRegionGeometry {
		t.Errorf("expected region geometry item type, got %q", audit.ItemType)
	}
	if audit.RegionID != "reg-lineage-1" || audit.ItemIndex != 0 {
		t.Errorf("audit must name the corrected region and its index, got region=%q index=%d", audit.RegionID, audit.ItemIndex)
	}
	if audit.Reason != "Reclassify uncertain region" || audit.Operator != "tester" {
		t.Errorf("audit must carry operator intent, got reason=%q operator=%q", audit.Reason, audit.Operator)
	}
	if audit.AssetID != assetID || audit.TargetLanguage != "vi" {
		t.Errorf("audit must be bound to the corrected asset/language, got asset=%q lang=%q", audit.AssetID, audit.TargetLanguage)
	}

	// A region correction is not an acceptance of a projected exception item, so it
	// must never flip a review item to manual_override.
	allItems, err := svc.ProjectAllReviewItemsForRun(ctx, assetID, "vi", runID)
	if err != nil {
		t.Fatalf("project all review items: %v", err)
	}
	for _, it := range allItems {
		if it.Status == domain.ReviewItemStatusManualOverride {
			t.Errorf("region correction must not mark review item %q as manual_override", it.ID)
		}
	}
}

func TestReviewService_RegionOverride_TruthfulnessAndLowOCRFlags(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-truth-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	// Setup TextRegionPlan with:
	// 1. "reg-uncertain": uncertain role exception
	// 2. "reg-low-ocr": low OCR confidence exception on subtitle
	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-truth-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-truth-1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:             "reg-uncertain",
				Text:           "模糊文本",
				Role:           domain.TextRoleUncertain,
				FirstSeenMs:    0,
				LastSeenMs:     1000,
				ReviewRequired: true,
				ReviewReason:   "uncertain_role",
			},
			{
				ID:             "reg-low-ocr",
				Text:           "低置信度文本",
				Role:           domain.TextRoleSpeechSubtitle,
				FirstSeenMs:    1000,
				LastSeenMs:     2000,
				ReviewRequired: true,
				ReviewReason:   "low_ocr_confidence_subtitle",
				ConfidenceEvidence: domain.ConfidenceEvidence{
					LowConfidence:  true,
					MeanConfidence: 0.38,
				},
			},
		},
		CreatedAt: time.Now().UTC(),
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

	// Initial pending queue: 2 exceptions
	itemsInitial, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsInitial) != 2 {
		t.Fatalf("expected 2 pending review exceptions, got %d (err: %v)", len(itemsInitial), err)
	}

	// Step 1: Reclassify "reg-uncertain" to semantic_text (role-only edit, genuine uncertain-role exception cleared)
	semanticRole := domain.TextRoleSemanticText
	res1, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "reg-uncertain",
				NewRole:  &semanticRole,
			},
		},
		Reason:   "Reclassify uncertain role to semantic text",
		Operator: "editor_monet",
	})
	if err != nil {
		t.Fatalf("CorrectRegionGeometry step 1 failed: %v", err)
	}
	if res1.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected auto_resolved for resolved uncertain role exception, got: %s", res1.Status)
	}

	// Check queue: only "reg-low-ocr" remains pending (1 item)
	itemsAfterStep1, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsAfterStep1) != 1 {
		t.Fatalf("expected 1 remaining pending item, got %d (err: %v)", len(itemsAfterStep1), err)
	}
	if itemsAfterStep1[0].RegionID != "reg-low-ocr" {
		t.Errorf("expected 'reg-low-ocr' to remain pending, got: %s", itemsAfterStep1[0].RegionID)
	}

	// Step 2: Role-only edit on "reg-low-ocr" (change role to semantic_text, NewText = nil)
	// Must PRESERVE low-OCR ReviewRequired flag and remain pending!
	res2, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "reg-low-ocr",
				NewRole:  &semanticRole,
			},
		},
		Reason:   "Role-only change on low OCR region",
		Operator: "editor_monet",
	})
	if err != nil {
		t.Fatalf("CorrectRegionGeometry step 2 failed: %v", err)
	}
	if res2.Status != domain.ReviewItemStatusPending {
		t.Errorf("role-only edit on low-OCR region must remain pending, got: %s", res2.Status)
	}

	// Check queue: "reg-low-ocr" is STILL in pending review queue
	itemsAfterStep2, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsAfterStep2) != 1 {
		t.Fatalf("expected 'reg-low-ocr' to remain pending in queue, got %d items", len(itemsAfterStep2))
	}
	if itemsAfterStep2[0].RegionID != "reg-low-ocr" || itemsAfterStep2[0].Type != domain.ReviewItemTypeLowConfidenceOCR {
		t.Errorf("expected low_confidence_ocr exception for 'reg-low-ocr', got: %+v", itemsAfterStep2[0])
	}

	// Step 3: Relabel edit on "reg-low-ocr" providing explicit corrected text (resolves OCR exception)
	correctedText := "Văn bản đã sửa lỗi OCR"
	res3, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{
				RegionID: "reg-low-ocr",
				NewRole:  &semanticRole,
				NewText:  &correctedText,
			},
		},
		Reason:   "Operator corrected OCR text",
		Operator: "editor_monet",
	})
	if err != nil {
		t.Fatalf("CorrectRegionGeometry step 3 failed: %v", err)
	}
	if res3.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected auto_resolved when OCR text is explicitly corrected, got: %s", res3.Status)
	}

	// Check queue: queue is now clean (0 pending items)
	itemsAfterStep3, err := svc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil || len(itemsAfterStep3) != 0 {
		t.Fatalf("expected queue zero after OCR text correction, got %d items: %+v", len(itemsAfterStep3), itemsAfterStep3)
	}
}

// Target-text correction is run-scoped. A missing run ID must fail before it can
// adopt an asset-latest translation/voice lineage or persist a synthetic correction run.
func TestReviewService_CorrectTargetText_RequiresRunID(t *testing.T) {
	svc, db, _, assetID := setupFullReviewHarness(t)
	ctx := context.Background()

	before, err := db.GetTranslationVariantIndex(ctx, assetID, "vi")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("read pre-correction translation index: %v", err)
	}
	_, err = svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", Reason: "must stay run-bound", Operator: "editor",
	})
	if err == nil || !strings.Contains(err.Error(), "run_id is required") {
		t.Fatalf("expected missing run_id to fail closed, got %v", err)
	}
	after, afterErr := db.GetTranslationVariantIndex(ctx, assetID, "vi")
	if errors.Is(afterErr, storage.ErrNotFound) {
		if before != nil {
			t.Fatal("translation index disappeared after rejected correction")
		}
		return
	}
	if afterErr != nil {
		t.Fatalf("read post-correction translation index: %v", afterErr)
	}
	if before == nil || after.CASHash != before.CASHash {
		t.Fatalf("rejected correction mutated translation lineage: before=%v after=%v", before, after)
	}
}

func TestReviewService_CorrectTargetText_RejectsIncompatibleLineage(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-compat"
	jobID := seedReviewRun(t, db, assetID, runID)

	transVar := domain.TranslationVariant{
		ID:             "trans-compat-1",
		SchemaVersion:  domain.TranslationSchemaVersion,
		ContractID:     service.TranslationContractID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-compat",
		OverallQAScore: 0.5,
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "点击右上角",
				TargetText:   "Nhấn góc trên",
				StartMs:      0,
				EndMs:        1500,
				QAConfidence: 0.9,
				PassedQAGate: true,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		OverallQAScore: transVar.OverallQAScore,
		CreatedAt:      transVar.CreatedAt,
	})

	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID: "se-foreign", RunID: runID, Stage: "speech_understand", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: "foreign-transcript-cas", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	_, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên mới", Reason: "lineage check", Operator: "editor",
	})
	if err == nil || !strings.Contains(err.Error(), "transcript lineage mismatch") {
		t.Fatalf("expected transcript lineage mismatch error, got %v", err)
	}

	// 2. Fix transcript execution back to matching
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID: "se-match", RunID: runID, Stage: "speech_understand", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: transVar.TranscriptArtifactCAS, CreatedAt: time.Now().UTC().Add(time.Second), UpdatedAt: time.Now().UTC().Add(time.Second),
	})
	_ = db.UpsertRun(ctx, domain.LocalizationRun{
		ID: runID, JobID: jobID, Status: "running",
		ConfigSnapshotJSON: `{"glossary":[{"source":"点击","target":"CLICK_REQUIRED"}]}`,
		CreatedAt:          time.Now().UTC(),
	})
	_, err = svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên mới", Reason: "glossary check", Operator: "editor",
	})
	if err == nil || !strings.Contains(err.Error(), "effective glossary mismatch") {
		t.Fatalf("expected effective glossary mismatch error, got %v", err)
	}
}
func TestReviewService_CorrectRegionGeometry_RejectsGeometryOutsideCanonicalFrame(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-frame-bounds-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	srcPlan := domain.TextRegionPlan{
		ID:             "text-plan-frame-bounds-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-frame-bounds-1",
		CreatedAt:      time.Now().UTC(),
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "reg-edge",
				Text:        "关注",
				Role:        domain.TextRoleSemanticText,
				FirstSeenMs: 0,
				LastSeenMs:  1500,
				Keyframes: []domain.RegionKeyframe{
					{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true, Confidence: 0.9},
				},
			},
		},
	}
	srcBytes, _ := json.Marshal(srcPlan)
	srcObj, _ := casStore.Put(bytes.NewReader(srcBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             srcPlan.ID,
		AssetID:        assetID,
		CASHash:        srcObj.SHA256,
		ProvenanceHash: srcPlan.ProvenanceHash,
		CreatedAt:      srcPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}

	for name, delta := range map[string]domain.RegionOverride{
		"dragged past the left frame edge":    {RegionID: "reg-edge", BoxDeltaX: -400},
		"resized below the canonical minimum": {RegionID: "reg-edge", BoxDeltaW: -300},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
				RunID:          runID,
				AssetID:        assetID,
				TargetLanguage: "vi",
				Overrides:      []domain.RegionOverride{delta},
				Reason:         "must fail closed, never clamp",
				Operator:       "tester",
			})
			if !errors.Is(err, domain.ErrRegionOverrideInvalid) {
				t.Fatalf("expected ErrRegionOverrideInvalid for %s, got: %v", name, err)
			}
			current, err := db.GetTextRegionPlanIndex(ctx, assetID)
			if err != nil {
				t.Fatalf("get current text region plan index: %v", err)
			}
			if current.ProvenanceHash != srcPlan.ProvenanceHash || current.CASHash != srcObj.SHA256 {
				t.Errorf("clamped geometry became the current plan: provenance=%s cas=%s, want %s / %s",
					current.ProvenanceHash, current.CASHash, srcPlan.ProvenanceHash, srcObj.SHA256)
			}
		})
	}

	// Control: in-frame geometry still applies, so the guard does not over-reject.
	if _, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-edge", BoxDeltaX: 20}},
		Reason:         "in-frame drag",
		Operator:       "tester",
	}); err != nil {
		t.Fatalf("in-frame geometry must still apply, got: %v", err)
	}
	current, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current text region plan index after in-frame edit: %v", err)
	}
	if current.ProvenanceHash == srcPlan.ProvenanceHash {
		t.Errorf("an in-frame edit must become the asset's current plan")
	}
}

// A correction persists its overridden plan, then regenerates the visual/subtitle tracks and the
// render plan, then records the operator's audit row. Those steps are not transactional, so a
// failure after the descendants exist must withdraw every row this correction made current —
// leaving the pre-correction artifacts as the current ones — instead of reporting an error over a
// mutated state.
func TestReviewService_CorrectRegionGeometry_FailedCorrectionRestoresPriorArtifacts(t *testing.T) {
	svc, db, casStore, _ := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-rollback-01"
	assetID := "asset-rollback-01"

	srcObj, err := casStore.Put(bytes.NewReader([]byte("rollback source media bytes")))
	if err != nil {
		t.Fatalf("put source media: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		SHA256:              srcObj.SHA256,
		ByteSize:            int64(len("rollback source media bytes")),
		MimeType:            "video/mp4",
		OriginalFilename:    "video.mp4",
		RightsAttestationID: "att-review-001",
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	srcPlan := domain.TextRegionPlan{
		ID:             "text-plan-rollback-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-rollback-1",
		CreatedAt:      time.Now().UTC(),
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "reg-rollback",
				Text:        "关注",
				Role:        domain.TextRoleSemanticText,
				FirstSeenMs: 0,
				LastSeenMs:  1500,
				Keyframes: []domain.RegionKeyframe{
					{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
				},
			},
		},
	}
	srcBytes, _ := json.Marshal(srcPlan)
	srcPlanObj, _ := casStore.Put(bytes.NewReader(srcBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             srcPlan.ID,
		AssetID:        assetID,
		CASHash:        srcPlanObj.SHA256,
		ProvenanceHash: srcPlan.ProvenanceHash,
		CreatedAt:      srcPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}

	// The run's pre-correction visual track, older than the correction.
	priorVisual := storage.LocalizedVisualTrackIndex{
		ID:                "prior-visual-rollback",
		AssetID:           assetID,
		RunID:             runID,
		TargetLanguage:    "vi",
		TextRegionPlanCAS: srcPlanObj.SHA256,
		CASHash:           srcObj.SHA256,
		ProvenanceHash:    "prov-prior-visual-rollback",
		CreatedAt:         time.Now().UTC().Add(-time.Hour),
	}
	if err := db.SaveLocalizedVisualTrackIndex(ctx, priorVisual); err != nil {
		t.Fatalf("save prior visual track index: %v", err)
	}

	// No DubMix index for this asset, so the correction fails after LocalizeVisualTrack has
	// persisted the run's visual and subtitle tracks and before any render plan is frozen.
	_, err = svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-rollback", BoxDeltaX: 10}},
		Reason:         "descendant regeneration fails after the plan is persisted",
		Operator:       "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "dub mix") {
		t.Fatalf("expected the correction to fail on the missing DubMix index, got: %v", err)
	}

	vis, err := db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
	if err != nil || vis == nil {
		t.Fatalf("the prior visual track must still be the run's current one, got idx=%v err=%v", vis, err)
	}
	if vis.ID != priorVisual.ID || vis.ProvenanceHash != priorVisual.ProvenanceHash {
		t.Errorf("failed correction left its own visual track current: id=%s provenance=%s, want %s / %s",
			vis.ID, vis.ProvenanceHash, priorVisual.ID, priorVisual.ProvenanceHash)
	}
	if sub, err := db.GetLocalizedSubtitleTrackIndexByRun(ctx, runID); err == nil && sub != nil {
		t.Errorf("failed correction left a subtitle track current: %+v", sub)
	}
	if rPlan, err := db.GetRenderPlanIndexByRun(ctx, runID); err == nil && rPlan != nil {
		t.Errorf("failed correction left a render plan current: %+v", rPlan)
	}
	if preview, err := db.GetLatestRenderArtifactIndex(ctx, assetID, "vi", domain.RenderKindPreview); err == nil && preview != nil {
		t.Errorf("failed correction left a preview artifact current: %+v", preview)
	}
	current, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current text region plan index: %v", err)
	}
	if current.ProvenanceHash != srcPlan.ProvenanceHash || current.CASHash != srcPlanObj.SHA256 {
		t.Errorf("failed correction left its own plan current: provenance=%s cas=%s, want %s / %s",
			current.ProvenanceHash, current.CASHash, srcPlan.ProvenanceHash, srcPlanObj.SHA256)
	}
}

// The audit row is the correction's commit point: an unrecorded correction must not be reported as
// successful, and a failure while recording it must not leave the regenerated artifacts (or the
// overridden plan) current.
func TestReviewService_CorrectRegionGeometry_AuditFailureRestoresPriorArtifacts(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-audit-failure-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	srcPlan := domain.TextRegionPlan{
		ID:             "text-plan-audit-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-audit-1",
		CreatedAt:      time.Now().UTC(),
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "reg-audit",
				Text:        "关注",
				Role:        domain.TextRoleSemanticText,
				FirstSeenMs: 0,
				LastSeenMs:  1500,
				Keyframes: []domain.RegionKeyframe{
					{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
				},
			},
		},
	}
	srcBytes, _ := json.Marshal(srcPlan)
	srcObj, _ := casStore.Put(bytes.NewReader(srcBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             srcPlan.ID,
		AssetID:        assetID,
		CASHash:        srcObj.SHA256,
		ProvenanceHash: srcPlan.ProvenanceHash,
		CreatedAt:      srcPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}

	// A first correction succeeds and becomes the state the second one must preserve.
	newRole := domain.TextRoleBrandKeep
	if _, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-audit", NewRole: &newRole}},
		Reason:         "first accepted reclassification",
		Operator:       "tester",
	}); err != nil {
		t.Fatalf("first correction must succeed: %v", err)
	}
	priorPlan, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get plan after first correction: %v", err)
	}
	priorVisual, err := db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
	if err != nil || priorVisual == nil {
		t.Fatalf("get visual track after first correction: idx=%v err=%v", priorVisual, err)
	}
	priorSubtitle, err := db.GetLocalizedSubtitleTrackIndexByRun(ctx, runID)
	if err != nil || priorSubtitle == nil {
		t.Fatalf("get subtitle track after first correction: idx=%v err=%v", priorSubtitle, err)
	}
	priorRender, err := db.GetRenderPlanIndexByRun(ctx, runID)
	if err != nil || priorRender == nil {
		t.Fatalf("get render plan after first correction: idx=%v err=%v", priorRender, err)
	}
	priorPreview, err := db.GetLatestRenderArtifactIndex(ctx, assetID, "vi", domain.RenderKindPreview)
	if err != nil || priorPreview == nil {
		t.Fatalf("get preview artifact after first correction: idx=%v err=%v", priorPreview, err)
	}

	// Remove the audit table so recording the operator's intent fails, without touching the
	// artifacts the correction regenerates first.
	_ = db.QueryRow(ctx, "DROP TABLE review_overrides").Scan(new(any))

	semanticRole := domain.TextRoleSemanticText
	_, err = svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-audit", NewRole: &semanticRole, BoxDeltaX: 15}},
		Reason:         "audit recording fails",
		Operator:       "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("expected the audit failure to surface, got: %v", err)
	}

	current, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current plan after the failed correction: %v", err)
	}
	if current.ProvenanceHash != priorPlan.ProvenanceHash || current.CASHash != priorPlan.CASHash {
		t.Errorf("failed correction left its own plan current: provenance=%s cas=%s, want %s / %s",
			current.ProvenanceHash, current.CASHash, priorPlan.ProvenanceHash, priorPlan.CASHash)
	}
	vis, err := db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
	if err != nil || vis == nil {
		t.Fatalf("the prior visual track must still be current, got idx=%v err=%v", vis, err)
	}
	if vis.ID != priorVisual.ID || vis.ProvenanceHash != priorVisual.ProvenanceHash {
		t.Errorf("failed correction left its own visual track current: id=%s provenance=%s, want %s / %s",
			vis.ID, vis.ProvenanceHash, priorVisual.ID, priorVisual.ProvenanceHash)
	}
	sub, err := db.GetLocalizedSubtitleTrackIndexByRun(ctx, runID)
	if err != nil || sub == nil {
		t.Fatalf("the prior subtitle track must still be current, got idx=%v err=%v", sub, err)
	}
	if sub.ID != priorSubtitle.ID || sub.ProvenanceHash != priorSubtitle.ProvenanceHash {
		t.Errorf("failed correction left its own subtitle track current: id=%s provenance=%s, want %s / %s",
			sub.ID, sub.ProvenanceHash, priorSubtitle.ID, priorSubtitle.ProvenanceHash)
	}
	rPlan, err := db.GetRenderPlanIndexByRun(ctx, runID)
	if err != nil || rPlan == nil {
		t.Fatalf("the prior render plan must still be current, got idx=%v err=%v", rPlan, err)
	}
	if rPlan.ID != priorRender.ID || rPlan.ProvenanceHash != priorRender.ProvenanceHash {
		t.Errorf("failed correction left its own render plan current: id=%s provenance=%s, want %s / %s",
			rPlan.ID, rPlan.ProvenanceHash, priorRender.ID, priorRender.ProvenanceHash)
	}
	preview, err := db.GetLatestRenderArtifactIndex(ctx, assetID, "vi", domain.RenderKindPreview)
	if err != nil {
		t.Fatalf("get preview artifact after the failed correction: %v", err)
	}
	if preview.CASHash != priorPreview.CASHash || preview.ProvenanceHash != priorPreview.ProvenanceHash {
		t.Errorf("failed correction left its own preview artifact current: cas=%s provenance=%s, want %s / %s",
			preview.CASHash, preview.ProvenanceHash, priorPreview.CASHash, priorPreview.ProvenanceHash)
	}
}

// A descendant snapshot the service cannot read must abort the correction before any state is
// persisted. Without a trustworthy prior state the rollback cannot tell which rows the correction
// made current, and a read failure mistaken for "nothing is current" would let a later rollback
// skip those rows while still reporting the correction as complete.
func TestReviewService_CorrectRegionGeometry_DescendantSnapshotFailureAbortsBeforePersisting(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-snapshot-failure-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	srcPlan := domain.TextRegionPlan{
		ID:             "text-plan-snapshot-1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-snapshot-1",
		CreatedAt:      time.Now().UTC(),
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "reg-snapshot",
				Text:        "关注",
				Role:        domain.TextRoleSemanticText,
				FirstSeenMs: 0,
				LastSeenMs:  1500,
				Keyframes: []domain.RegionKeyframe{
					{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
				},
			},
		},
	}
	srcBytes, _ := json.Marshal(srcPlan)
	srcObj, _ := casStore.Put(bytes.NewReader(srcBytes))
	if err := db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             srcPlan.ID,
		AssetID:        assetID,
		CASHash:        srcObj.SHA256,
		ProvenanceHash: srcPlan.ProvenanceHash,
		CreatedAt:      srcPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}

	// Make the descendant snapshot read fail with a real storage error. QueryRow is the storage
	// API available here; the statement executes and reports no rows.
	_ = db.QueryRow(ctx, "DROP TABLE localized_visual_tracks").Scan(new(any))

	_, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-snapshot", BoxDeltaX: 10}},
		Reason:         "snapshot failure must abort before persisting",
		Operator:       "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected the descendant snapshot failure to abort the correction, got: %v", err)
	}
	if !strings.Contains(err.Error(), "localized visual track") {
		t.Errorf("the snapshot failure must name the unreadable artifact, got: %v", err)
	}

	current, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current text region plan index: %v", err)
	}
	if current.ProvenanceHash != srcPlan.ProvenanceHash || current.CASHash != srcObj.SHA256 {
		t.Errorf("a failed descendant snapshot mutated the current plan: provenance=%s cas=%s, want %s / %s",
			current.ProvenanceHash, current.CASHash, srcPlan.ProvenanceHash, srcObj.SHA256)
	}
	if sub, err := db.GetLocalizedSubtitleTrackIndexByRun(ctx, runID); err == nil && sub != nil {
		t.Errorf("a failed descendant snapshot still regenerated descendants: %+v", sub)
	}
	if rPlan, err := db.GetRenderPlanIndexByRun(ctx, runID); err == nil && rPlan != nil {
		t.Errorf("a failed descendant snapshot still froze a render plan: %+v", rPlan)
	}
}

// saveRegionCorrectionPlan installs a source TextRegionPlan whose regions carry canonical
// keyframe geometry, and returns the stored artifact's CAS hash.
func saveRegionCorrectionPlan(t *testing.T, db *storage.DB, casStore *cas.Store, assetID, provenance string, regions []domain.TrackedTextRegion) string {
	t.Helper()
	plan := domain.TextRegionPlan{
		ID:             "text-plan-" + provenance,
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: provenance,
		Regions:        regions,
		CreatedAt:      time.Now().UTC(),
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal source plan: %v", err)
	}
	obj, err := casStore.Put(bytes.NewReader(planBytes))
	if err != nil {
		t.Fatalf("put source plan: %v", err)
	}
	if err := db.SaveTextRegionPlanIndex(context.Background(), storage.TextRegionPlanIndex{
		ID:             plan.ID,
		AssetID:        assetID,
		CASHash:        obj.SHA256,
		ProvenanceHash: plan.ProvenanceHash,
		CreatedAt:      plan.CreatedAt,
	}); err != nil {
		t.Fatalf("save source plan index: %v", err)
	}
	return obj.SHA256
}

// A request canceled after the overridden plan is persisted must still be withdrawn. The
// rollback used to run on the request context, so a client that died while the correction
// was regenerating descendants left the rejected plan (and its descendants) current and
// returned an error describing state the operator could no longer see.
func TestReviewService_CorrectRegionGeometry_RollbackSurvivesRequestCancellation(t *testing.T) {
	svc, db, casStore, renderSvc, assetID := setupFullReviewHarnessWithRender(t)
	runID := "run-cancelled-rollback-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	planCAS := saveRegionCorrectionPlan(t, db, casStore, assetID, "prov-text-cancel-1", []domain.TrackedTextRegion{{
		ID:          "reg-cancel",
		Text:        "关注",
		Role:        domain.TextRoleSemanticText,
		FirstSeenMs: 0,
		LastSeenMs:  1500,
		Keyframes: []domain.RegionKeyframe{
			{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
		},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client disconnects while the correction is rendering the preview for the plan it
	// has already persisted: everything up to the audit row is already current state.
	renderSvc.SetCustomComposer(func(_ context.Context, _ media.CompositionRequest) (*media.CompositionResult, error) {
		cancel()
		return nil, errors.New("preview composition interrupted by client disconnect")
	})

	_, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-cancel", BoxDeltaX: 25}},
		Reason:         "request canceled after the overridden plan was persisted",
		Operator:       "tester",
	})
	if err == nil {
		t.Fatalf("expected the canceled correction to fail")
	}
	if strings.Contains(err.Error(), "could not be withdrawn") {
		t.Fatalf("the withdrawal ran on the canceled request context: %v", err)
	}

	// Reads use a live context: the request context this test canceled stays canceled.
	readCtx := context.Background()
	current, err := db.GetTextRegionPlanIndex(readCtx, assetID)
	if err != nil {
		t.Fatalf("get current plan after the canceled correction: %v", err)
	}
	if current.ProvenanceHash != "prov-text-cancel-1" || current.CASHash != planCAS {
		t.Errorf("a canceled correction left its own plan current: provenance=%s cas=%s, want prov-text-cancel-1 / %s",
			current.ProvenanceHash, current.CASHash, planCAS)
	}
	if vis, err := db.GetLocalizedVisualTrackIndexByRun(readCtx, runID); err == nil && vis != nil {
		t.Errorf("a canceled correction left a visual track current: %+v", vis)
	}
	if sub, err := db.GetLocalizedSubtitleTrackIndexByRun(readCtx, runID); err == nil && sub != nil {
		t.Errorf("a canceled correction left a subtitle track current: %+v", sub)
	}
	if rPlan, err := db.GetRenderPlanIndexByRun(readCtx, runID); err == nil && rPlan != nil {
		t.Errorf("a canceled correction left a render plan current: %+v", rPlan)
	}
	if preview, err := db.GetLatestRenderArtifactIndex(readCtx, assetID, "vi", domain.RenderKindPreview); err == nil && preview != nil {
		t.Errorf("a canceled correction left a preview artifact current: %+v", preview)
	}
}

// The correction's audit rows are its commit point: one row per corrected region, written
// as a batch, so a failure in the middle of a multi-region correction can never leave an
// audit trail describing an edit the caller rolled back.
func TestReviewService_CorrectRegionGeometry_AuditBatchIsAtomic(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-audit-batch-01"
	bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	planCAS := saveRegionCorrectionPlan(t, db, casStore, assetID, "prov-text-batch-1", []domain.TrackedTextRegion{
		{
			ID:          "reg-batch-one",
			Text:        "关注",
			Role:        domain.TextRoleSemanticText,
			FirstSeenMs: 0,
			LastSeenMs:  1500,
			Keyframes: []domain.RegionKeyframe{
				{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
			},
		},
		{
			ID:          "reg-batch-two",
			Text:        "下载",
			Role:        domain.TextRoleSemanticText,
			FirstSeenMs: 0,
			LastSeenMs:  1500,
			Keyframes: []domain.RegionKeyframe{
				{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 400, Width: 300, Height: 60}, Observed: true},
			},
		},
	})

	// Fail the second row of the batch. The first row is already written by then, so only a
	// transactional batch keeps it out of the audit trail of a rolled-back correction.
	_ = db.QueryRow(ctx, `CREATE TRIGGER fail_second_region_audit BEFORE INSERT ON review_overrides
		WHEN NEW.region_id = 'reg-batch-two'
		BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`).Scan(new(any))

	newRole := domain.TextRoleBrandKeep
	_, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides: []domain.RegionOverride{
			{RegionID: "reg-batch-one", NewRole: &newRole},
			{RegionID: "reg-batch-two", NewRole: &newRole},
		},
		Reason:   "two-region correction whose audit batch fails on the second row",
		Operator: "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("expected the failed audit batch to surface, got: %v", err)
	}

	overrides, err := db.GetReviewOverrides(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("get review overrides after the failed batch: %v", err)
	}
	if len(overrides) != 0 {
		t.Errorf("a failed audit batch left %d row(s) behind: %+v", len(overrides), overrides)
	}

	current, err := db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current plan after the failed batch: %v", err)
	}
	if current.ProvenanceHash != "prov-text-batch-1" || current.CASHash != planCAS {
		t.Errorf("a failed audit batch left its plan current: provenance=%s cas=%s, want prov-text-batch-1 / %s",
			current.ProvenanceHash, current.CASHash, planCAS)
	}
}

// A region correction is only visible to the operator once the preview the UI reloads has
// been rendered from the plan it just froze. Freezing a plan without a preview leaves the
// pre-correction geometry on screen behind a reported success.
func TestReviewService_CorrectRegionGeometry_RendersPreviewFromCorrectedPlan(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-preview-correction-01"
	dubMixCAS := bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	saveRegionCorrectionPlan(t, db, casStore, assetID, "prov-text-preview-1", []domain.TrackedTextRegion{{
		ID:          "reg-preview",
		Text:        "关注",
		Role:        domain.TextRoleSemanticText,
		FirstSeenMs: 0,
		LastSeenMs:  1500,
		Keyframes: []domain.RegionKeyframe{
			{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
		},
	}})

	first, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-preview", BoxDeltaX: 40, BoxDeltaY: 20}},
		Reason:         "operator dragged the region",
		Operator:       "tester",
	})
	if err != nil {
		t.Fatalf("first region correction must succeed: %v", err)
	}
	if first.PreviewRenderCAS == "" {
		t.Fatalf("the correction reported no preview render artifact: %+v", first)
	}

	firstPlan, err := db.GetRenderPlanIndexByRun(ctx, runID)
	if err != nil || firstPlan == nil {
		t.Fatalf("get render plan after the first correction: idx=%v err=%v", firstPlan, err)
	}
	assertPreviewConsumesPlan(t, db, casStore, assetID, runID, first.PreviewRenderCAS, firstPlan)

	// A second correction must supersede the first one rather than leave the operator looking at
	// stale geometry. What it supersedes is the localized visual track the inspector reads - the
	// render plan itself is built from pinned subtitles and audio, so a second overlay-only edit
	// freezes an identical recipe and legitimately resolves to the same content-addressed preview.
	// (Freezing overlay references into the RenderPlan is tracked by architecture §4/RenderPlan;
	// until then an overlay edit is visible in the track artifact, not in the burned preview.)
	second, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-preview", BoxDeltaX: 10}},
		Reason:         "operator resized the region again",
		Operator:       "tester",
	})
	if err != nil {
		t.Fatalf("second region correction must succeed: %v", err)
	}
	if second.LocalizedVisualTrackCAS == first.LocalizedVisualTrackCAS {
		t.Fatalf("the second correction reused the first localized visual track: %s", second.LocalizedVisualTrackCAS)
	}
	secondPlan, err := db.GetRenderPlanIndexByRun(ctx, runID)
	if err != nil || secondPlan == nil {
		t.Fatalf("get render plan after the second correction: idx=%v err=%v", secondPlan, err)
	}
	assertPreviewConsumesPlan(t, db, casStore, assetID, runID, second.PreviewRenderCAS, secondPlan)

	// The correction is a targeted rerun: it reuses the frozen dub mix instead of
	// re-running speech, translation, TTS or audio stages.
	afterMix, err := db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || afterMix == nil {
		t.Fatalf("get dub mix after the corrections: idx=%v err=%v", afterMix, err)
	}
	if afterMix.CASHash != dubMixCAS {
		t.Errorf("the correction reran the audio mix: dub mix cas=%s, want %s", afterMix.CASHash, dubMixCAS)
	}
}

// assertPreviewConsumesPlan proves a preview artifact is the one the UI's run-scoped render
// read resolves, and that it was composed from exactly the given render plan.
func assertPreviewConsumesPlan(t *testing.T, db *storage.DB, casStore *cas.Store, assetID, runID, previewCAS string, plan *storage.RenderPlanIndex) {
	t.Helper()
	ctx := context.Background()

	indices, err := db.GetRenderArtifactIndicesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("get render artifacts by run: %v", err)
	}
	latest := ""
	for _, idx := range indices {
		if idx.Kind == domain.RenderKindPreview {
			latest = idx.CASHash
		}
	}
	if latest != previewCAS {
		t.Fatalf("the run-scoped preview the UI resolves is %q, want %q", latest, previewCAS)
	}

	rc, err := casStore.Get(previewCAS)
	if err != nil {
		t.Fatalf("load preview artifact from CAS: %v", err)
	}
	defer rc.Close()
	var artifact domain.PreviewRenderArtifact
	if err := json.NewDecoder(rc).Decode(&artifact); err != nil {
		t.Fatalf("decode preview artifact: %v", err)
	}
	if artifact.AssetID != assetID || artifact.RunID != runID {
		t.Errorf("preview artifact is bound to %s/%s, want %s/%s", artifact.AssetID, artifact.RunID, assetID, runID)
	}
	if artifact.ConsumedPlan.PlanProvenanceHash != plan.ProvenanceHash || artifact.ConsumedPlan.PlanCASHash != plan.CASHash {
		t.Errorf("preview consumed plan %s/%s, want %s/%s",
			artifact.ConsumedPlan.PlanProvenanceHash, artifact.ConsumedPlan.PlanCASHash, plan.ProvenanceHash, plan.CASHash)
	}
	if artifact.OutputCASHash == "" {
		t.Fatalf("preview artifact has no rendered media: %+v", artifact)
	}
	if _, err := casStore.ResolvePath(artifact.OutputCASHash); err != nil {
		t.Errorf("the preview media the operator plays is not resolvable: %v", err)
	}
}

// A region correction validates the geometry the operator actually changed. A collision on an
// untouched region is a pre-existing layout fact the operator has not addressed yet: it must stay
// a visual_occlusion exception instead of failing the whole correction, otherwise the queue can
// never be worked one item at a time.
func TestReviewService_CorrectRegionGeometry_UntouchedOcclusionDoesNotBlockTheEdit(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correction-untouched-occlusion"
	_ = bindBaselineDubMixToRun(t, db, assetID, "vi", runID)

	saveRegionCorrectionPlan(t, db, casStore, assetID, "prov-untouched-occlusion", []domain.TrackedTextRegion{
		{
			ID:          "reg-edited",
			Text:        "关注",
			Role:        domain.TextRoleSemanticText,
			FirstSeenMs: 0,
			LastSeenMs:  1500,
			Keyframes: []domain.RegionKeyframe{
				{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 60}, Observed: true},
			},
		},
		{
			// A protected control the operator has not touched yet, and a protected neighbour whose
			// box sits on top of it: its overlay can never clear the neighbour.
			ID:          "reg-colliding",
			Text:        "剪映",
			Role:        domain.TextRoleInstructionalUIText,
			FirstSeenMs: 0,
			LastSeenMs:  1500,
			ProtectedMetadata: domain.ProtectedRegionMetadata{
				IsProtected: true,
				Reason:      "instructional_ui_control",
			},
			Keyframes: []domain.RegionKeyframe{
				{TimestampMs: 0, Box: domain.BoundingBox{X: 100, Y: 1400, Width: 200, Height: 50}, Observed: true},
			},
		},
		{
			ID:          "reg-neighbour",
			Text:        "音频",
			Role:        domain.TextRoleBrandKeep,
			FirstSeenMs: 0,
			LastSeenMs:  1500,
			ProtectedMetadata: domain.ProtectedRegionMetadata{
				IsProtected: true,
				Reason:      "brand_authenticity",
			},
			Keyframes: []domain.RegionKeyframe{
				{TimestampMs: 0, Box: domain.BoundingBox{X: 150, Y: 1400, Width: 120, Height: 50}, Observed: true},
			},
		},
	})

	result, err := svc.CorrectRegionGeometry(ctx, service.RegionGeometryCorrectionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Overrides:      []domain.RegionOverride{{RegionID: "reg-edited", BoxDeltaX: 30}},
		Reason:         "operator moved the source text out of the way",
		Operator:       "tester",
	})
	if err != nil {
		t.Fatalf("a correction must not be blocked by an unrelated occluded region: %v", err)
	}

	rc, err := casStore.Get(result.LocalizedVisualTrackCAS)
	if err != nil {
		t.Fatalf("read localized visual track: %v", err)
	}
	defer rc.Close()
	var track domain.LocalizedVisualTrack
	if err := json.NewDecoder(rc).Decode(&track); err != nil {
		t.Fatalf("decode localized visual track: %v", err)
	}
	found := false
	for _, occ := range track.Occlusions {
		if occ.RegionID == "reg-colliding" {
			found = true
		}
		if occ.RegionID == "reg-edited" {
			t.Errorf("the edited region %s was reported as occluded", occ.RegionID)
		}
	}
	if !found {
		t.Fatalf("the untouched collision must stay surfaced as an exception, got %+v", track.Occlusions)
	}

	// ...and it is projected for the operator rather than dropped.
	reviewSvc := service.NewReviewService(db, casStore)
	items, err := reviewSvc.ProjectReviewItemsForRun(ctx, assetID, "vi", runID)
	if err != nil {
		t.Fatalf("project review items: %v", err)
	}
	projected := false
	for _, item := range items {
		if item.Type == domain.ReviewItemTypeVisualOcclusion && item.RegionID == "reg-colliding" {
			projected = true
		}
	}
	if !projected {
		t.Fatalf("expected a pending visual_occlusion item for reg-colliding, got %+v", items)
	}
}

func TestReviewService_ProjectAllReviewItems_DecodedArtifactOwnershipMismatch(t *testing.T) {
	reviewSvc, db, casStore, assetID := setupReviewTestHarness(t)
	ctx := context.Background()
	runID := "run-mismatch-" + uuid.NewString()[:8]

	otherAssetID := "other-asset-" + uuid.NewString()[:8]
	tVar := domain.TranslationVariant{
		ID:             uuid.NewString(),
		AssetID:        otherAssetID,
		TargetLanguage: "vi",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "测试", TargetText: "Thử nghiệm", PassedQAGate: false, QAConfidence: 0.4},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(tVar)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	jobID := "job-mismatch-" + uuid.NewString()[:8]
	now := time.Now().UTC()
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "running",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "translation",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("create stage execution: %v", err)
	}

	_, err := reviewSvc.ProjectAllReviewItemsForRun(ctx, assetID, "vi", runID)
	if err == nil {
		t.Fatalf("expected error on translation variant asset mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "ownership mismatch") {
		t.Fatalf("expected ownership mismatch error, got %v", err)
	}
}

func TestReviewService_CorrectTargetText_InputHashMatchesWhenTranscriptHasExcludedMembers(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-excluded"
	jobID := seedReviewRun(t, db, assetID, runID)

	// Role plan with dialogue [0-2000] and singing [3000-5000]
	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + runID,
		AssetID:        assetID,
		ProvenanceHash: "prov-role-" + runID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 3000, EndMs: 5000, Role: domain.AudioRoleSingingMusicVocal},
		},
		CreatedAt: time.Now().UTC(),
	}
	rpBytes, _ := json.Marshal(rolePlan)
	rpObj, _ := casStore.Put(bytes.NewReader(rpBytes))
	rolePlan.CASHash = rpObj.SHA256
	_ = db.SaveAudioRolePlan(ctx, rolePlan)

	// Pinned transcript with dialogue block (0) and singing block (1)
	transcript := domain.TranscriptArtifact{
		ID:      "transcript-" + runID,
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 1500, SourceText: "点击右上角", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
			{Index: 1, StartMs: 3200, EndMs: 4800, SourceText: "这是歌声", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))

	// Canonical translation segments from unified predicate contain ONLY segment 0
	canonicalSegs := domain.CanonicalTranslationSegments(&transcript, &rolePlan)
	if len(canonicalSegs) != 1 || canonicalSegs[0].Index != 0 {
		t.Fatalf("expected only dialogue block 0 in canonical segments, got: %+v", canonicalSegs)
	}

	// Translation variant was built from canonical segment 0 only (as run translation does)
	eff, _ := service.EffectiveGlossaryForTest(nil, canonicalSegs)
	jobIn := domain.TranslationJobInput{
		AssetID:               assetID,
		RunID:                 runID,
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		Segments:              canonicalSegs,
		EffectiveGlossary:     eff,
		TranscriptArtifactCAS: tObj.SHA256,
	}
	transSvc := service.NewTranslationService(db, casStore)
	expectedHash, err := transSvc.ComputeTranslationInputHashForTest(jobIn)
	if err != nil {
		t.Fatalf("compute input hash: %v", err)
	}

	transVar := domain.TranslationVariant{
		ID:                    "trans-excl-1",
		SchemaVersion:         domain.TranslationSchemaVersion,
		ContractID:            service.TranslationContractID,
		AssetID:               assetID,
		RunID:                 runID,
		JobID:                 jobID,
		TargetLanguage:        "vi",
		SourceLanguage:        "zh",
		TranscriptArtifactCAS: tObj.SHA256,
		EffectiveGlossary:     eff,
		InputHash:             expectedHash,
		ProvenanceHash:        "prov-trans-excl",
		OverallQAScore:        0.5,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "点击右上角", TargetText: "Nhấn vào góc trên bên phải của màn hình", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	tvBytes, _ := json.Marshal(transVar)
	tvObj, _ := casStore.Put(bytes.NewReader(tvBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        tvObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		OverallQAScore: transVar.OverallQAScore,
		CreatedAt:      transVar.CreatedAt,
	})

	// DubScriptVariant for segment 0
	dubScriptVar := domain.DubScriptVariant{
		ID:                    "dubscript-excl-1",
		AssetID:               assetID,
		RunID:                 runID,
		JobID:                 jobID,
		TargetLanguage:        "vi",
		SourceLanguage:        "zh",
		TranslationVariantCAS: tvObj.SHA256,
		ProvenanceHash:        "prov-ds-excl",
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1500, SlotDurationMs: 1500, SourceText: "点击右上角", SpokenText: "Nhấn vào góc trên bên phải của màn hình", MeaningText: "Nhấn vào góc trên bên phải của màn hình"},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScriptVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScriptVar.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScriptVar.ProvenanceHash, CreatedAt: dubScriptVar.CreatedAt,
	})

	// CorrectTargetText must compute canonical segments using the unified predicate and succeed
	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", SpokenTextOverride: "Nhấn góc trên", Reason: "Shortened", Operator: "tester",
	})
	if err != nil {
		t.Fatalf("expected CorrectTargetText to succeed when transcript contains excluded singing members, got: %v", err)
	}
	if res.TranslationVariantCAS == "" {
		t.Fatalf("expected updated translation variant CAS, got empty")
	}
}

func TestReviewService_CorrectTargetText_FailsClosedOnMissingOrMalformedEvidence(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()

	// 1. Missing transcript lineage on translation variant
	runID1 := "run-failclosed-1"
	jobID1 := seedReviewRun(t, db, assetID, runID1)
	vMissingTranscript := domain.TranslationVariant{
		ID: "v1", SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID1, JobID: jobID1, TargetLanguage: "vi", SourceLanguage: "zh",
		TranscriptArtifactCAS: "", // MISSING
		EffectiveGlossary:     domain.EffectiveGlossary{Hash: "eff-hash"},
		InputHash:             "inp-hash",
		ProvenanceHash:        "prov-failclosed-1",
		Segments:              []domain.TranslationSegment{{Index: 0, SourceText: "测试", TargetText: "Thử"}},
	}
	b1, _ := json.Marshal(vMissingTranscript)
	o1, _ := casStore.Put(bytes.NewReader(b1))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: vMissingTranscript.ID, AssetID: assetID, RunID: runID1, TargetLanguage: "vi", CASHash: o1.SHA256, ProvenanceHash: "prov-failclosed-1",
	})
	_, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID1, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0, NewTargetText: "Sửa", Reason: "test", Operator: "op",
	})
	if err == nil || !strings.Contains(err.Error(), "missing transcript lineage") {
		t.Fatalf("expected missing transcript lineage failure, got %v", err)
	}

	// 2. Missing effective glossary evidence
	runID2 := "run-failclosed-2"
	jobID2 := seedReviewRun(t, db, assetID, runID2)
	vMissingGlossary := domain.TranslationVariant{
		ID: "v2", SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID2, JobID: jobID2, TargetLanguage: "vi", SourceLanguage: "zh",
		TranscriptArtifactCAS: "cas-trans-2",
		EffectiveGlossary:     domain.EffectiveGlossary{Hash: ""}, // MISSING
		InputHash:             "inp-hash",
		ProvenanceHash:        "prov-failclosed-2",
		Segments:              []domain.TranslationSegment{{Index: 0, SourceText: "测试", TargetText: "Thử"}},
	}
	b2, _ := json.Marshal(vMissingGlossary)
	o2, _ := casStore.Put(bytes.NewReader(b2))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: vMissingGlossary.ID, AssetID: assetID, RunID: runID2, TargetLanguage: "vi", CASHash: o2.SHA256, ProvenanceHash: "prov-failclosed-2",
	})
	_, err = svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID2, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0, NewTargetText: "Sửa", Reason: "test", Operator: "op",
	})
	if err == nil || !strings.Contains(err.Error(), "missing effective glossary hash") {
		t.Fatalf("expected missing effective glossary failure, got %v", err)
	}

	// 3. Missing input hash evidence
	runID3 := "run-failclosed-3"
	jobID3 := seedReviewRun(t, db, assetID, runID3)
	tObj3, _ := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `","speech_blocks":[{"index":0,"start_ms":0,"end_ms":1000,"source_text":"测试","speaker_id":"S1","segment_type":"speech"}]}`)))
	eff3, _ := service.EffectiveGlossaryForTest(nil, []domain.TranslationInputSegment{{Index: 0, SourceText: "测试"}})
	vMissingInputHash := domain.TranslationVariant{
		ID: "v3", SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID3, JobID: jobID3, TargetLanguage: "vi", SourceLanguage: "zh",
		TranscriptArtifactCAS: tObj3.SHA256,
		EffectiveGlossary:     eff3,
		InputHash:             "", // MISSING
		ProvenanceHash:        "prov-failclosed-3",
		Segments:              []domain.TranslationSegment{{Index: 0, SourceText: "测试", TargetText: "Thử"}},
	}
	b3, _ := json.Marshal(vMissingInputHash)
	o3, _ := casStore.Put(bytes.NewReader(b3))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: vMissingInputHash.ID, AssetID: assetID, RunID: runID3, TargetLanguage: "vi", CASHash: o3.SHA256, ProvenanceHash: "prov-failclosed-3",
	})
	_, err = svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID3, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0, NewTargetText: "Sửa", Reason: "test", Operator: "op",
	})
	if err == nil || !strings.Contains(err.Error(), "missing input hash") {
		t.Fatalf("expected missing input hash failure, got %v", err)
	}

	// 4. Malformed run config snapshot JSON
	runID4 := "run-failclosed-4"
	jobID4 := "job-failclosed-4"
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID: jobID4, SourceAssetID: assetID, TargetLanguage: "vi", Status: "running", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID: runID4, JobID: jobID4, Status: "running", ConfigSnapshotJSON: "{invalid-json", CreatedAt: time.Now().UTC(),
	})
	vMalformedRun := domain.TranslationVariant{
		ID: "v4", SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID4, JobID: jobID4, TargetLanguage: "vi", SourceLanguage: "zh",
		TranscriptArtifactCAS: "cas-trans-4",
		EffectiveGlossary:     domain.EffectiveGlossary{Hash: "eff-hash"},
		InputHash:             "inp-hash",
		ProvenanceHash:        "prov-failclosed-4",
		Segments:              []domain.TranslationSegment{{Index: 0, SourceText: "测试", TargetText: "Thử"}},
	}
	b4, _ := json.Marshal(vMalformedRun)
	o4, _ := casStore.Put(bytes.NewReader(b4))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: vMalformedRun.ID, AssetID: assetID, RunID: runID4, TargetLanguage: "vi", CASHash: o4.SHA256, ProvenanceHash: "prov-failclosed-4",
	})
	_, err = svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID4, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0, NewTargetText: "Sửa", Reason: "test", Operator: "op",
	})
	if err == nil || !strings.Contains(err.Error(), "malformed run config snapshot JSON") {
		t.Fatalf("expected malformed config failure, got %v", err)
	}
}

// seedRolePlanForTest stores a role plan in CAS, makes it the asset's newest plan, and returns it with its
// CAS hash filled in (the hash a run records when it froze this plan).
func seedRolePlanForTest(t *testing.T, db *storage.DB, casStore *cas.Store, assetID, planID string, segments []domain.AudioSegment) *domain.AudioRolePlan {
	t.Helper()
	ctx := context.Background()
	plan := domain.AudioRolePlan{
		ID:             planID,
		AssetID:        assetID,
		Segments:       segments,
		ProvenanceHash: "prov-" + planID,
		CreatedAt:      time.Now().UTC(),
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal role plan %s: %v", planID, err)
	}
	obj, err := casStore.Put(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("put role plan %s: %v", planID, err)
	}
	plan.CASHash = obj.SHA256
	if err := db.SaveAudioRolePlan(ctx, plan); err != nil {
		t.Fatalf("save role plan %s: %v", planID, err)
	}
	if err := db.SaveAudioRolePlanIndex(ctx, storage.AudioRolePlanIndex{
		ID:             plan.ID,
		AssetID:        assetID,
		CASHash:        plan.CASHash,
		ProvenanceHash: plan.ProvenanceHash,
		CreatedAt:      plan.CreatedAt,
	}); err != nil {
		t.Fatalf("save role plan index %s: %v", planID, err)
	}
	return &plan
}

// recordRunDeliveryStage records a succeeded stage execution for a run, exactly as the pipeline does when
// the stage consumes or produces that artifact.
func recordRunDeliveryStage(t *testing.T, db *storage.DB, runID, stage, casHash string) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          stage,
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: casHash,
		StartedAt:      &now,
		CompletedAt:    &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("record %s stage for run %s: %v", stage, runID, err)
	}
}

// A correction re-derives canonical segmentation from the role-plan lineage the run froze. An operator who
// re-generates or edits the asset's AudioRolePlan afterwards must not invalidate a still-valid pinned
// correction: the run's artifacts were never segmented with that edit.
func TestReviewService_CorrectTargetText_OperatorRolePlanEditKeepsPinnedCorrectionValid(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-roleplan-edit"
	jobID := seedReviewRun(t, db, assetID, runID)

	// The plan the run segmented with: both speech blocks sit inside a dialogue window.
	runPlan := seedRolePlanForTest(t, db, casStore, assetID, "role-"+runID, []domain.AudioSegment{
		{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 2500, EndMs: 4500, Role: domain.AudioRoleNarrationDialogue},
	})
	transcript := domain.TranscriptArtifact{
		ID:      "transcript-" + runID,
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 1500, SourceText: "点击右上角", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
			{Index: 1, StartMs: 3000, EndMs: 4000, SourceText: "关注", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))

	canonical := domain.CanonicalTranslationSegments(&transcript, runPlan)
	if len(canonical) != 2 {
		t.Fatalf("fixture must segment to both dialogue blocks, got %+v", canonical)
	}
	eff, err := service.EffectiveGlossaryForTest(nil, canonical)
	if err != nil {
		t.Fatalf("effective glossary: %v", err)
	}
	transSvc := service.NewTranslationService(db, casStore)
	inputHash, err := transSvc.ComputeTranslationInputHashForTest(domain.TranslationJobInput{
		AssetID: assetID, RunID: runID, SourceLanguage: "zh", TargetLanguage: "vi",
		Segments: canonical, EffectiveGlossary: eff, TranscriptArtifactCAS: tObj.SHA256,
	})
	if err != nil {
		t.Fatalf("compute input hash: %v", err)
	}

	transVar := domain.TranslationVariant{
		ID: "trans-" + runID, SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", SourceLanguage: "zh",
		TranscriptArtifactCAS: tObj.SHA256,
		EffectiveGlossary:     eff,
		InputHash:             inputHash,
		ProvenanceHash:        "prov-" + runID,
		OverallQAScore:        0.7,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "点击右上角", TargetText: "Nhấn vào góc trên bên phải của màn hình", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
			{Index: 1, SourceText: "关注", TargetText: "Theo dõi", StartMs: 3000, EndMs: 4000, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	tvBytes, _ := json.Marshal(transVar)
	tvObj, _ := casStore.Put(bytes.NewReader(tvBytes))
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: transVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
		CASHash: tvObj.SHA256, ProvenanceHash: transVar.ProvenanceHash, OverallQAScore: transVar.OverallQAScore, CreatedAt: transVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}
	recordRunDeliveryStage(t, db, runID, "speech_understand", tObj.SHA256)
	recordRunDeliveryStage(t, db, runID, "audio_role_plan", runPlan.CASHash)

	// The operator then edits the asset's plan so no dialogue window covers the first block any more.
	editedPlan := seedRolePlanForTest(t, db, casStore, assetID, "role-edited-"+runID, []domain.AudioSegment{
		{StartMs: 5000, EndMs: 7000, Role: domain.AudioRoleNarrationDialogue},
	})
	// Premise of the regression: the edited plan re-segments the transcript, so a correction that read the
	// asset's newest plan (the previous behavior) recomputes a different canonical input and rejects the
	// variant instead of correcting it.
	editedSegs := domain.CanonicalTranslationSegments(&transcript, editedPlan)
	if len(editedSegs) != 0 {
		t.Fatalf("fixture must re-segment under the edited plan, got %+v", editedSegs)
	}
	editedHash, err := transSvc.ComputeTranslationInputHashForTest(domain.TranslationJobInput{
		AssetID: assetID, RunID: runID, SourceLanguage: "zh", TargetLanguage: "vi",
		Segments: editedSegs, EffectiveGlossary: eff, TranscriptArtifactCAS: tObj.SHA256,
	})
	if err != nil {
		t.Fatalf("compute edited input hash: %v", err)
	}
	if editedHash == transVar.InputHash {
		t.Fatalf("fixture must re-segment under the edited plan, otherwise this test cannot detect the fix")
	}

	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", SpokenTextOverride: "Nhấn góc trên", Reason: "shortened", Operator: "tester",
	})
	// Input-lineage compatibility is what this contract covers: the correction must be accepted and republish
	// the corrected variant. The mixer's coverage policy under the operator's new plan is a separate gate, so
	// the correction may legitimately stop there — but never before it.
	if err != nil && !strings.Contains(err.Error(), "audio mix rerun failed:") {
		t.Fatalf("a later role plan edit must not invalidate the pinned correction's input lineage, got: %v", err)
	}
	t.Logf("correction after the operator's plan edit: err=%v result=%+v", err, res)
	if err == nil && res.TranslationVariantCAS == "" {
		t.Fatalf("expected a corrected translation variant, got %+v", res)
	}
	// The corrected variant must be the run's current one: reading the asset's newest plan rejected the
	// correction outright before it could republish anything.
	idx, err := db.GetTranslationVariantIndexByRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run translation variant index: %v", err)
	}
	if idx.CASHash == tvObj.SHA256 {
		t.Fatalf("expected the correction to republish the run's variant, index still points at %s", tvObj.SHA256)
	}
	rc, err := casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("load corrected variant: %v", err)
	}
	defer rc.Close()
	var corrected domain.TranslationVariant
	if err := json.NewDecoder(rc).Decode(&corrected); err != nil {
		t.Fatalf("decode corrected variant: %v", err)
	}
	if corrected.Segments[0].TargetText != "Nhấn góc trên" {
		t.Fatalf("expected the corrected target text on the run's variant, got %q", corrected.Segments[0].TargetText)
	}
}

// A role plan that cannot be resolved for the variant must fail closed: silently falling back to the
// asset's newest plan (or to no plan at all) would decide the correction on state the run never used.
func TestReviewService_CorrectTargetText_UnresolvablePinnedRolePlanFailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-roleplan-missing"
	jobID := seedReviewRun(t, db, assetID, runID)

	transVar := domain.TranslationVariant{
		ID: "trans-" + runID, SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", SourceLanguage: "zh",
		ProvenanceHash: "prov-" + runID, OverallQAScore: 0.7,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "点击右上角", TargetText: "Nhấn vào góc trên bên phải của màn hình", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	tvBytes, _ := json.Marshal(transVar)
	tvObj, _ := casStore.Put(bytes.NewReader(tvBytes))
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: transVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
		CASHash: tvObj.SHA256, ProvenanceHash: transVar.ProvenanceHash, OverallQAScore: transVar.OverallQAScore, CreatedAt: transVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}
	recordRunDeliveryStage(t, db, runID, "speech_understand", transVar.TranscriptArtifactCAS)
	// The run froze a role plan the CAS no longer holds.
	recordRunDeliveryStage(t, db, runID, "audio_role_plan", "missing-role-plan-cas")

	_, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", Reason: "shortened", Operator: "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "read pinned audio role plan from CAS") {
		t.Fatalf("expected the unresolvable pinned role plan to fail the correction closed, got: %v", err)
	}
}

// seedTranslationLadderRouter builds the production translation ladder (gemini then deepseek) with no
// governance service, so eligibility is decided purely by each provider's policy state.
func seedTranslationLadderRouter(t *testing.T) (*provider.Router, *provider.GatewayTranslationProvider, *provider.GatewayTranslationProvider) {
	t.Helper()
	newLane := func(id, alias string, quality float64) *provider.GatewayTranslationProvider {
		p, err := provider.NewGatewayTranslationProvider(id, alias, "baseline-"+id, quality, "http://127.0.0.1:8080")
		if err != nil {
			t.Fatalf("create gateway provider %s: %v", id, err)
		}
		p.SetPolicyState(domain.PolicyAllowed)
		return p
	}
	gemini := newLane(provider.GatewayGeminiTranslationProviderID, provider.GatewayGeminiModelAlias, 0.90)
	deepseek := newLane(provider.GatewayDeepSeekTranslationProviderID, provider.GatewayDeepSeekModelAlias, 0.99)
	reg := provider.NewRegistry()
	_ = reg.Register(gemini)
	_ = reg.Register(deepseek)
	return provider.NewRouter(reg, nil, nil, nil, nil, nil), gemini, deepseek
}

// seedCorrectionVariantForTest pins a translation variant for a run under the given provider, matching the
// canonical segmentation the harness's role plan produces (all blocks are dialogue).
func seedCorrectionVariantForTest(t *testing.T, db *storage.DB, casStore *cas.Store, assetID, runID, jobID, providerID string) {
	t.Helper()
	ctx := context.Background()
	transVar := domain.TranslationVariant{
		ID: "trans-" + runID, SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", SourceLanguage: "zh",
		ProviderID: providerID, ProvenanceHash: "prov-" + runID, OverallQAScore: 0.7,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "点击右上角", TargetText: "Nhấn vào góc trên bên phải của màn hình", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinReviewTranslationTranscript(t, casStore, &transVar)
	b, _ := json.Marshal(transVar)
	obj, _ := casStore.Put(bytes.NewReader(b))
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: transVar.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
		CASHash: obj.SHA256, ProvenanceHash: transVar.ProvenanceHash, ProviderID: providerID, OverallQAScore: transVar.OverallQAScore, CreatedAt: transVar.CreatedAt,
	}); err != nil {
		t.Fatalf("save translation variant index: %v", err)
	}
	recordRunDeliveryStage(t, db, runID, "speech_understand", transVar.TranscriptArtifactCAS)
}

// A variant frozen under a provider that does not win the current ladder is still correctable: the router
// proves the frozen provider eligible as a fallback candidate, so a health or ranking change elsewhere must
// not strand an operator's correction.
func TestReviewService_CorrectTargetText_FrozenTranslationProviderStillEligible(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-provider-fallback"
	jobID := seedReviewRun(t, db, assetID, runID)
	seedCorrectionVariantForTest(t, db, casStore, assetID, runID, jobID, provider.GatewayDeepSeekTranslationProviderID)

	router, gemini, deepseek := seedTranslationLadderRouter(t)
	// Premise: another lane wins the default route, which is what stranded the frozen variant before.
	routeRes, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeTranslation, Language: "vi"})
	if err != nil {
		t.Fatalf("route translation ladder: %v", err)
	}
	if routeRes.SelectedProvider.ID() != gemini.ID() {
		t.Fatalf("fixture must select the ladder's primary lane, got %s", routeRes.SelectedProvider.ID())
	}
	if len(routeRes.FallbackOrdered) != 1 || routeRes.FallbackOrdered[0].ID() != deepseek.ID() {
		t.Fatalf("fixture must carry the frozen lane as the eligible fallback, got %+v", routeRes.FallbackOrdered)
	}
	transSvc := service.NewTranslationService(db, casStore)
	transSvc.ConfigureRouter(router)
	svc.SetTranslationService(transSvc)

	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", SpokenTextOverride: "Nhấn góc trên", Reason: "shortened", Operator: "tester",
	})
	if err != nil {
		t.Fatalf("frozen provider eligibility must survive ladder reordering, got: %v", err)
	}
	if res.TranslationVariantCAS == "" {
		t.Fatalf("expected a corrected translation variant, got %+v", res)
	}
}

// A frozen provider the router no longer admits fails the correction closed instead of silently rewriting
// the variant's provider lineage.
func TestReviewService_CorrectTargetText_FrozenTranslationProviderNotEligibleFailsClosed(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()
	runID := "run-correct-provider-blocked"
	jobID := seedReviewRun(t, db, assetID, runID)
	seedCorrectionVariantForTest(t, db, casStore, assetID, runID, jobID, provider.GatewayDeepSeekTranslationProviderID)

	router, _, deepseek := seedTranslationLadderRouter(t)
	deepseek.SetPolicyState(domain.PolicyBlocked)
	transSvc := service.NewTranslationService(db, casStore)
	transSvc.ConfigureRouter(router)
	svc.SetTranslationService(transSvc)

	_, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", Reason: "shortened", Operator: "tester",
	})
	if err == nil || !strings.Contains(err.Error(), "not eligible in the current provider route") {
		t.Fatalf("expected an ineligible frozen provider to fail closed, got: %v", err)
	}
}

// Voice reassignment must refuse to guess a transcript when the run's pinned-transcript proof cannot be
// completed: falling back to the asset's newest transcript would regenerate a correction from another run's
// lineage.
func TestReviewService_ReassignVoice_PropagatesTranscriptLineageFailure(t *testing.T) {
	svc, db, casStore, assetID := setupFullReviewHarness(t)
	ctx := context.Background()

	// The run's owning job belongs to a different asset, so the pinned-transcript proof cannot be completed
	// for this asset and the correction must report that instead of falling back.
	runID := "run-voice-lineage-proof"
	foreignJobID := "job-voice-lineage-proof"
	foreignAssetID := "asset-voice-lineage-proof"
	now := time.Now().UTC()
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: foreignAssetID, SHA256: "sha-" + foreignAssetID, ByteSize: 2048, MimeType: "video/mp4",
		OriginalFilename: "other.mp4", RightsAttestationID: "att-review-001", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create foreign asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID: foreignJobID, SourceAssetID: foreignAssetID, TargetLanguage: "vi", Status: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create foreign job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: foreignJobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	dubScriptVar := domain.DubScriptVariant{
		ID: "dubscript-" + runID, RunID: runID, AssetID: assetID, TargetLanguage: "vi", SourceLanguage: "zh",
		ProvenanceHash: "prov-" + runID, OverallQAScore: 0.5,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SourceText: "点击右上角", MeaningText: "Nhấn góc trên", SpokenText: "Nhấn góc trên", SlotDurationMs: 1500, PassedQAGate: true},
		},
		CreatedAt: now,
	}
	dsBytes, _ := json.Marshal(dubScriptVar)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScriptVar.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		CASHash: dsObj.SHA256, ProvenanceHash: dubScriptVar.ProvenanceHash, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	_, err := svc.ReassignVoice(ctx, service.VoiceReassignCorrectionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", Reason: "voice swap", Operator: "tester",
	})
	if err == nil {
		t.Fatalf("expected the transcript lineage failure to propagate, got nil")
	}
	if !errors.Is(err, domain.ErrTranscriptLineageMismatch) {
		t.Fatalf("expected a typed transcript lineage mismatch, got: %v", err)
	}
}

// Final-render handoff must refuse to publish a render built from artifacts a correction superseded: until
// the run re-freezes the corrected mix into its render plan, the explicit render would consume the
// pre-correction plan.
func TestReviewService_EvaluateFinalRenderHandoff_RefusesStaleDeliveryLineageUntilRefrozen(t *testing.T) {
	svc, db, casStore, renderSvc, assetID := setupFullReviewHarnessWithRender(t)
	ctx := context.Background()
	runID := "run-handoff-stale-lineage"
	jobID := seedReviewRun(t, db, assetID, runID)

	// An operator-visible, delivery-complete run: its mix is bound to it and its plan was frozen from it.
	baselineMixCAS := bindBaselineDubMixToRun(t, db, assetID, "vi", runID)
	recordRunDeliveryStage(t, db, runID, "audio_mix", baselineMixCAS)
	baselinePlan, err := renderSvc.FreezeRenderPlan(ctx, service.RenderPlanInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", DubMixCAS: baselineMixCAS,
	})
	if err != nil {
		t.Fatalf("freeze baseline render plan: %v", err)
	}
	recordRunDeliveryStage(t, db, runID, "render_plan", baselinePlan.CASHash)

	handoffIn := domain.FinalRenderHandoffInput{
		AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi", Posture: domain.ReviewPostureReview,
	}
	handoff, err := svc.EvaluateFinalRenderHandoff(ctx, handoffIn)
	if err != nil {
		t.Fatalf("baseline handoff: %v", err)
	}
	if !handoff.CanStartFinalRender {
		t.Fatalf("fixture must start handoff-eligible, got %+v", handoff)
	}

	// The run reaches review before text_detection/visual_text_localize ran, so a text correction rebuilds
	// the mix but cannot re-freeze the render plan (CorrectTargetText skips those stages).
	if err := db.DeleteTextRegionPlanIndex(ctx, assetID, "prov-text-"+assetID); err != nil {
		t.Fatalf("drop text region plan for a pre-visual review state: %v", err)
	}
	seedCorrectionVariantForTest(t, db, casStore, assetID, runID, jobID, "fake_trans")
	res, err := svc.CorrectTargetText(ctx, service.TargetTextCorrectionInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", SegmentIndex: 0,
		NewTargetText: "Nhấn góc trên", SpokenTextOverride: "Nhấn góc trên", Reason: "shortened", Operator: "tester",
	})
	if err != nil {
		t.Fatalf("correction: %v", err)
	}
	if res.DubMixCAS == "" || res.RenderPlanCAS != "" {
		t.Fatalf("fixture must supersede the mix without re-freezing the plan, got %+v", res)
	}
	pending, err := svc.ProjectReviewItemsForRun(ctx, assetID, "vi", runID)
	if err != nil {
		t.Fatalf("project review items: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("fixture must leave an empty exception queue so only the lineage gate can refuse, got %+v", pending)
	}

	_, err = svc.EvaluateFinalRenderHandoff(ctx, handoffIn)
	if err == nil || !strings.Contains(err.Error(), "final render handoff refused") {
		t.Fatalf("expected the handoff to be refused for the superseded mix, got: %v", err)
	}
	if !strings.Contains(err.Error(), res.DubMixCAS) {
		t.Fatalf("refusal must name the run's current mix %s, got: %v", res.DubMixCAS, err)
	}

	// Re-freezing the plan from the corrected mix (what the resumed pipeline's render_plan stage does)
	// makes the run handoff-eligible again.
	refrozen, err := renderSvc.FreezeRenderPlan(ctx, service.RenderPlanInput{
		RunID: runID, JobID: jobID, AssetID: assetID, TargetLanguage: "vi", DubMixCAS: res.DubMixCAS,
	})
	if err != nil {
		t.Fatalf("re-freeze render plan: %v", err)
	}
	recordRunDeliveryStage(t, db, runID, "render_plan", refrozen.CASHash)

	handoff, err = svc.EvaluateFinalRenderHandoff(ctx, handoffIn)
	if err != nil {
		t.Fatalf("handoff after re-freezing the corrected lineage: %v", err)
	}
	if !handoff.CanStartFinalRender {
		t.Fatalf("expected an eligible handoff once the lineage is re-frozen, got %+v", handoff)
	}
}
