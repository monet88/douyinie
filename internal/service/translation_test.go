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

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

func setupTranslationTestEnv(t *testing.T) (*storage.DB, *cas.Store, *provider.Router, *provider.Registry) {
	t.Helper()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup DB: %v", err)
	}

	reg := provider.NewSeam1FakeRegistry()
	// Translation routing is remote-only in production. These legacy-named fakes
	// exercise translation service behavior, so model them as remote gateway lanes.
	for _, id := range []string{"fake_llm_translator", "fake_local_translator_fallback"} {
		if p, ok := reg.Get(id); ok {
			p.(*provider.FakeTranslationProvider).Cap.ExecutionTier = "cloud"
		}
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

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

	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db, casStore, router, reg
}

func TestTranslationService_Translate_VI_and_EN(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()

	// Seed rights attestation, source asset, job, and run in DB
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})

	err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	jobID := "job-1"
	err = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	err = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	tObj, _ := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `"}`)))
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      time.Now().UTC(),
	})

	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          jobID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{
				Index:      0,
				SourceText: "今天天气很好。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    0,
				EndMs:      1500,
			},
			{
				Index:      1,
				SourceText: "请将温度调至25度，张伟说不要打开窗户。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    1600,
				EndMs:      4000,
			},
		},
	}

	// 1. Translate to Vietnamese
	variantVI, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("Translate to VI failed: %v", err)
	}

	if variantVI.TargetLanguage != "vi" {
		t.Errorf("expected target language vi, got %s", variantVI.TargetLanguage)
	}
	if len(variantVI.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(variantVI.Segments))
	}
	if !variantVI.Segments[0].PassedQAGate || !variantVI.Segments[1].PassedQAGate {
		t.Errorf("expected all segments to pass QA gate")
	}
	if variantVI.OverallQAScore < 0.9 {
		t.Errorf("expected overall QA score >= 0.9, got %f", variantVI.OverallQAScore)
	}
	if variantVI.CASHash == "" {
		t.Errorf("expected non-empty CASHash")
	}

	// 2. Translate to English
	input.TargetLanguage = "en"
	variantEN, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("Translate to EN failed: %v", err)
	}

	if variantEN.TargetLanguage != "en" {
		t.Errorf("expected target language en, got %s", variantEN.TargetLanguage)
	}
	if len(variantEN.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(variantEN.Segments))
	}
	if variantEN.OverallQAScore < 0.9 {
		t.Errorf("expected overall QA score >= 0.9, got %f", variantEN.OverallQAScore)
	}
}

func TestTranslationService_ProviderReportedNegativeCannotOverrideLexicalNegationMismatch(t *testing.T) {
	db, casStore, _, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	jobID := uuid.NewString()
	attID := uuid.NewString()

	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "semantic-negation-source", ByteSize: 100, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: jobID, SourceAssetID: assetID, TargetLanguage: "en", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: jobID, Status: "running", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID)

	svc.TranslateInvoke = func(_ context.Context, _ provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		return &provider.TranslationResult{
			ProviderID:   provider.GatewayGeminiTranslationProviderID,
			ModelVersion: provider.GatewayGeminiModelAlias,
			Segments: []domain.TranslationSegment{{
				Index:            0,
				SourceText:       req.Segments[0].SourceText,
				TargetText:       "Open the window.",
				NegationPolarity: false, // provider self-report contradicts the source
			}},
		}, nil
	}

	variant, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          jobID,
		SourceLanguage: "zh",
		TargetLanguage: "en",
		Segments: []domain.TranslationInputSegment{{
			Index:      0,
			SourceText: "不要打开窗户",
			StartMs:    0,
			EndMs:      1000,
		}},
	})
	if err != nil {
		t.Fatalf("expected the flagged candidate to be persisted for review, got: %v", err)
	}
	if len(variant.Segments) != 1 {
		t.Fatalf("expected 1 persisted segment, got %d", len(variant.Segments))
	}
	seg := variant.Segments[0]
	if seg.PassedQAGate {
		t.Fatalf("expected the gate verdict to flag the inverted negation, got passed_qa_gate=true")
	}
	if !seg.NegationPolarity {
		t.Errorf("expected the gate's own polarity verdict (source is negative) to override the provider self-report, got false")
	}
	if !strings.Contains(seg.ReviewReason, "negation polarity inverted") {
		t.Errorf("expected the review reason to name the inverted negation, got %q", seg.ReviewReason)
	}
}

func TestTranslationService_IdempotentCache(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "fake-sha", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: "j1", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "j1", Status: "running", CreatedAt: time.Now().UTC()})

	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID)
	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "j1",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1500},
		},
	}

	firstVariant, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("first translate failed: %v", err)
	}

	secondVariant, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("second translate failed: %v", err)
	}

	if firstVariant.ID != secondVariant.ID {
		t.Errorf("expected cached variant ID %s, got %s", firstVariant.ID, secondVariant.ID)
	}
	if firstVariant.ProvenanceHash != secondVariant.ProvenanceHash {
		t.Errorf("expected matching provenance hash")
	}
}

func TestTranslationService_PolicyFallbackRouting(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "fake-sha", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: "j1", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "j1", Status: "running", CreatedAt: time.Now().UTC()})

	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID)
	// Make primary provider fail with error
	primaryProv, ok := reg.Get("fake_llm_translator")
	if !ok {
		t.Fatalf("fake_llm_translator not found")
	}
	primaryFake := primaryProv.(*provider.FakeTranslationProvider)
	primaryFake.InjectError = errors.New("simulated network timeout")

	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "j1",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1500},
		},
	}

	// Should fallback to fake_local_translator_fallback
	variant, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("Translate with fallback failed: %v", err)
	}

	if variant.ProviderID != "fake_local_translator_fallback" {
		t.Errorf("expected fallback provider fake_local_translator_fallback, got %s", variant.ProviderID)
	}

	// Verify attempt provenance in DB
	attempts, err := db.ListProviderAttempts(ctx, runID, "translation")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	t.Logf("found %d attempts: %+v", len(attempts), attempts)
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 attempts recorded in provenance, got %d", len(attempts))
	}
	if attempts[0].Status != "failed" || attempts[1].Status != "succeeded" {
		t.Errorf("expected attempt 1 failed and attempt 2 succeeded, got %s and %s", attempts[0].Status, attempts[1].Status)
	}
}

func TestTranslationService_QAGate_FlagsInsteadOfRejecting(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "fake-sha", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: "j1", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "j1", Status: "running", CreatedAt: time.Now().UTC()})

	service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID)
	// Make fake provider corrupt numbers
	prov, _ := reg.Get("fake_llm_translator")
	fake := prov.(*provider.FakeTranslationProvider)
	fake.CorruptNumbers = true

	// Also make fallback corrupt numbers: no clean lane exists, so the best flagged
	// candidate must be persisted for operator review instead of failing the stage.
	fbProv, _ := reg.Get("fake_local_translator_fallback")
	fbFake := fbProv.(*provider.FakeTranslationProvider)
	fbFake.CorruptNumbers = true

	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "j1",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "剪刀以45度角剪掉枝条。", StartMs: 0, EndMs: 1500},
		},
	}

	variant, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("expected best-effort translation variant when every lane trips the QA gate, got: %v", err)
	}
	if len(variant.Segments) != 1 {
		t.Fatalf("expected the flagged candidate to be persisted, got %d segments", len(variant.Segments))
	}
	seg := variant.Segments[0]
	if seg.PassedQAGate {
		t.Fatalf("expected the corrupted-number segment to stay flagged, got passed_qa_gate=true")
	}
	if seg.QAConfidence >= 0.6 {
		t.Errorf("expected a low QA confidence for a flagged segment, got %f", seg.QAConfidence)
	}
	if !strings.Contains(seg.ReviewReason, "45") {
		t.Errorf("expected the review reason to name the corrupted number, got %q", seg.ReviewReason)
	}

	// Provenance invariant: every lane is still recorded as quality_failed.
	attempts, err := db.ListProviderAttempts(ctx, runID, "translation")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts recorded, got %d", len(attempts))
	}
	for i, att := range attempts {
		if att.Status != "quality_failed" {
			t.Errorf("attempt %d status must be quality_failed, got %s", i, att.Status)
		}
		if !strings.Contains(att.ErrorMessage, domain.ErrQualityRejected.Error()) || !strings.Contains(att.ErrorMessage, domain.ErrNumberCorrupted.Error()) {
			t.Errorf("attempt %d must keep the specific QA reason, got %s", i, att.ErrorMessage)
		}
	}
}

func TestTranslationService_PathologicalASRRepetition_SkippedFromTranslation(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "fake-sha", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: "j1", SourceAssetID: assetID, TargetLanguage: "en", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "j1", Status: "running", CreatedAt: time.Now().UTC()})

	// Save transcript containing real speech and pathological repetition noise
	transcript := domain.TranscriptArtifact{
		ID:      uuid.NewString(),
		AssetID: assetID,
		RunID:   runID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 2000, SourceText: "这快递居然这么快", SegmentType: domain.SpeechBlockTypeSpeech},
			{Index: 1, StartMs: 2000, EndMs: 5000, SourceText: "ってるチュルチュンチュンチュンチュンチュルチュンチュルチュンチュンチュンチュンチュルチュンチュルチュンチュンチュンチュンチュルチュンマカナペンキの", SegmentType: domain.SpeechBlockTypeSpeech},
			{Index: 2, StartMs: 5000, EndMs: 7000, SourceText: "老天爷我终于瘦了", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID:             transcript.ID,
		AssetID:        assetID,
		RunID:          runID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: "prov-test",
		CreatedAt:      transcript.CreatedAt,
	})

	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "j1",
		SourceLanguage: "zh",
		TargetLanguage: "en",
	}

	variant, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("Translate failed unexpectedly: %v", err)
	}

	if len(variant.Segments) != 2 {
		t.Fatalf("expected exactly 2 real speech segments (pathological noise skipped), got %d", len(variant.Segments))
	}
	if variant.Segments[0].Index != 0 || variant.Segments[1].Index != 2 {
		t.Errorf("expected segments [0, 2], got indices [%d, %d]", variant.Segments[0].Index, variant.Segments[1].Index)
	}
}

func TestTranslationService_ResolveRunTranscriptCAS_RefusesMismatchedAsset(t *testing.T) {
	db, casStore, _, _ := setupTranslationTestEnv(t)
	ctx := context.Background()

	assetA := uuid.NewString()
	assetB := uuid.NewString()
	runB := uuid.NewString()
	jobB := "job-b"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetA, RightsAttestationID: attID, SHA256: "sha-a", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetB, RightsAttestationID: attID, SHA256: "sha-b", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: jobB, SourceAssetID: assetB, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runB, JobID: jobB, Status: "running", CreatedAt: time.Now().UTC()})

	// Put speech_understand stage execution for runB
	dummyData := []byte(`{"dummy":"transcript"}`)
	obj, _ := casStore.Put(bytes.NewReader(dummyData))
	now := time.Now().UTC()
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runB,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: obj.SHA256,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	// Resolving for assetB should succeed and return obj.SHA256
	casHash, err := service.ResolveRunTranscriptCASForTest(ctx, db, runB, assetB, "translation")
	if err != nil {
		t.Fatalf("resolve for matching asset failed: %v", err)
	}
	if casHash != obj.SHA256 {
		t.Fatalf("expected hash %s, got %s", obj.SHA256, casHash)
	}

	// Resolving for assetA must fail closed with lineage mismatch, NOT returning casHash
	mismatchHash, err := service.ResolveRunTranscriptCASForTest(ctx, db, runB, assetA, "translation")
	if err == nil {
		t.Fatalf("expected lineage mismatch error when run belongs to assetB, got hash: %s", mismatchHash)
	}
	if !strings.Contains(err.Error(), "transcript lineage mismatch") && !strings.Contains(err.Error(), assetB) {
		t.Fatalf("expected lineage mismatch error naming asset, got: %v", err)
	}
}

func TestTranslationService_Translate_RefusesMismatchedAssetOrJob(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetA := uuid.NewString()
	assetB := uuid.NewString()
	runB := uuid.NewString()
	jobB := "job-b"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetA, RightsAttestationID: attID, SHA256: "sha-a", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetB, RightsAttestationID: attID, SHA256: "sha-b", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: jobB, SourceAssetID: assetB, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runB, JobID: jobB, Status: "running", CreatedAt: time.Now().UTC()})

	// 1. Run belongs to assetB, requested for assetA -> fail closed
	_, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runB,
		AssetID:        assetA,
		JobID:          jobB,
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	})
	if err == nil || !errors.Is(err, domain.ErrTranslationOwnershipMismatch) {
		t.Fatalf("expected ErrTranslationOwnershipMismatch for cross-asset run, got: %v", err)
	}

	// 2. JobID mismatch for run
	_, err = svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runB,
		AssetID:        assetB,
		JobID:          "unrelated-job",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	})
	if err == nil || !errors.Is(err, domain.ErrTranslationOwnershipMismatch) {
		t.Fatalf("expected ErrTranslationOwnershipMismatch for mismatched JobID, got: %v", err)
	}
}

func TestTranslationService_Translate_FrozenGlossaryAuthoritative(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	jobID := "job-glossary"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "sha-g", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: jobID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "running",
		ConfigSnapshotJSON: `{"glossary":[{"source":"SUPOR","target":"Nồi Supor","note":"Brand"}]}`,
		CreatedAt:          time.Now().UTC(),
	})
	tObj, _ := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `"}`)))
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      time.Now().UTC(),
	})

	invocations := 0
	svc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		invocations++
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "fake_model",
			ModelVersion: "1",
			Segments: []domain.TranslationSegment{
				{Index: 0, SourceText: req.Segments[0].SourceText, TargetText: "Chào Nồi Supor"},
			},
		}, nil
	}

	// 1. Conflicting request glossary must fail closed before routing (zero provider calls, zero mutation)
	_, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          jobID,
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "SUPOR 你好"},
		},
		Glossary: []domain.GlossaryEntry{
			{Source: "SUPOR", Target: "Khác", Note: "Different"},
		},
	})
	if err == nil || !errors.Is(err, domain.ErrGlossaryConflict) {
		t.Fatalf("expected ErrGlossaryConflict for conflicting glossary, got: %v", err)
	}
	if invocations != 0 {
		t.Fatalf("expected 0 provider invocations on glossary conflict, got %d", invocations)
	}
	if _, err := db.GetTranslationVariantIndexByRun(ctx, runID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected no variant index stored in DB on failure, got err=%v", err)
	}

	// 2. Empty request glossary inherits frozen snapshot
	v1, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          jobID,
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "SUPOR 你好"},
		},
	})
	if err != nil {
		t.Fatalf("expected success with inherited frozen glossary, got: %v", err)
	}
	if invocations != 1 {
		t.Fatalf("expected 1 provider invocation, got %d", invocations)
	}
	if len(v1.EffectiveGlossary.Entries) != 1 || v1.EffectiveGlossary.Entries[0].Target != "Nồi Supor" {
		t.Fatalf("expected effective glossary target 'Nồi Supor', got %+v", v1.EffectiveGlossary)
	}

	// 3. Canonically equivalent request glossary is accepted and preserves frozen snapshot semantics
	runID2 := uuid.NewString()
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:                 runID2,
		JobID:              jobID,
		Status:             "running",
		ConfigSnapshotJSON: `{"glossary":[{"source":"SUPOR","target":"Nồi Supor","note":"Brand"}]}`,
		CreatedAt:          time.Now().UTC(),
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID2,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      time.Now().UTC(),
	})
	v2, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID2,
		AssetID:        assetID,
		JobID:          jobID,
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "SUPOR 你好"},
		},
		Glossary: []domain.GlossaryEntry{
			{Source: " ｓｕｐｏｒ ", Target: "Nồi Supor", Note: "Brand"},
		},
	})
	if err != nil {
		t.Fatalf("expected success with canonically equivalent glossary, got: %v", err)
	}
	if invocations != 2 {
		t.Fatalf("expected 2 provider invocations, got %d", invocations)
	}
	if v2.EffectiveGlossary.Hash != v1.EffectiveGlossary.Hash {
		t.Fatalf("expected identical effective glossary hash, got %s vs %s", v2.EffectiveGlossary.Hash, v1.EffectiveGlossary.Hash)
	}
}

func TestTranslationService_CanReuseVariant_PreservesCorrectedTargetTextOnResume(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	jobID := "job-resume-test"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "sha-resume", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: jobID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: time.Now().UTC()})

	// Put transcript in CAS
	transcript := domain.TranscriptArtifact{
		ID:           "transcript-" + assetID,
		AssetID:      assetID,
		SpeechBlocks: []domain.SpeechBlock{{Index: 0, StartMs: 0, EndMs: 1500, SourceText: "你好", SpeakerID: "S1", SegmentType: domain.SpeechBlockTypeSpeech}},
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: tObj.SHA256, CreatedAt: time.Now().UTC(),
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "speech_understand", Status: domain.StageStatusSucceeded, ArtifactSHA256: tObj.SHA256, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	canonicalSegs := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "你好", SpeakerID: "S1", StartMs: 0, EndMs: 1500},
	}
	eff, _ := service.EffectiveGlossaryForTest(nil, canonicalSegs)
	jobIn := domain.TranslationJobInput{
		AssetID:               assetID,
		RunID:                 runID,
		JobID:                 jobID,
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		Segments:              canonicalSegs,
		EffectiveGlossary:     eff,
		TranscriptArtifactCAS: tObj.SHA256,
	}
	inputHash, _ := svc.ComputeTranslationInputHashForTest(jobIn)

	// Obtain active provider ID from router
	p, _ := reg.Get("fake_llm_translator")
	mName, mVer := p.ModelInfo()

	// Corrected variant has chained ProvenanceHash and corrected TargetText
	correctedVariant := domain.TranslationVariant{
		ID:                    "trans-corrected-1",
		SchemaVersion:         domain.TranslationSchemaVersion,
		ContractID:            service.TranslationContractID,
		AssetID:               assetID,
		RunID:                 runID,
		JobID:                 jobID,
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		TranscriptArtifactCAS: tObj.SHA256,
		EffectiveGlossary:     eff,
		InputHash:             inputHash,
		ProvenanceHash:        "chained-provenance-hash-from-correction",
		ProviderID:            p.ID(),
		ModelName:             mName,
		ModelVersion:          mVer,
		OverallQAScore:        0.95,
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "你好", TargetText: "Xin chào (đã sửa)", StartMs: 0, EndMs: 1500, QAConfidence: 0.95, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	cvBytes, _ := json.Marshal(correctedVariant)
	cvObj, _ := casStore.Put(bytes.NewReader(cvBytes))
	correctedVariant.CASHash = cvObj.SHA256

	// Record corrected variant in stage_executions and translation_variant_indices as review service does
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             correctedVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        cvObj.SHA256,
		ProvenanceHash: correctedVariant.ProvenanceHash,
		ProviderID:     p.ID(),
		ModelName:      mName,
		ModelVersion:   mVer,
		OverallQAScore: 0.95,
		CreatedAt:      correctedVariant.CreatedAt,
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "translation",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: cvObj.SHA256,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})

	// CanReuseVariant must accept the corrected variant on resume!
	canReuse := svc.CanReuseVariant(ctx, jobIn, &correctedVariant)
	if !canReuse {
		t.Fatalf("expected CanReuseVariant to return true for valid corrected translation on resume, got false")
	}
	if correctedVariant.Segments[0].TargetText != "Xin chào (đã sửa)" {
		t.Fatalf("expected corrected target text to be preserved, got %q", correctedVariant.Segments[0].TargetText)
	}
}

func TestTranslationService_Translate_RunScopedMissingSpeechTranscriptCAS_FailsClosed(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetID := uuid.NewString()
	run1 := uuid.NewString()
	run2 := uuid.NewString()

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "sha-run-fallback", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: "job-1", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(ctx, domain.LocalizationJob{ID: "job-2", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: run1, JobID: "job-1", Status: "completed", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(ctx, domain.LocalizationRun{ID: run2, JobID: "job-2", Status: "running", CreatedAt: time.Now().UTC().Add(time.Minute)})

	// Run 1 had a transcript, which is the asset's latest transcript index
	tObj1, _ := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `","speech_blocks":[{"index":0,"start_ms":0,"end_ms":1000,"source_text":"你好","speaker_id":"S1","segment_type":"speech"}]}`)))
	_ = db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID:             "transcript-run1",
		AssetID:        assetID,
		RunID:          run1,
		CASHash:        tObj1.SHA256,
		ProvenanceHash: "prov-run1",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: run1, Stage: "speech_understand", Status: domain.StageStatusSucceeded, ArtifactSHA256: tObj1.SHA256, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	invocations := 0
	svc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		invocations++
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "fake_model",
			ModelVersion: "1",
			Segments: []domain.TranslationSegment{
				{Index: 0, SourceText: req.Segments[0].SourceText, TargetText: "Xin chào"},
			},
		}, nil
	}

	// 1. Run 2 has NO speech_understand transcript. Direct translation for Run 2 without segments must FAIL CLOSED.
	// It must NOT fall back to Run 1's transcript!
	_, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          run2,
		AssetID:        assetID,
		JobID:          "job-2",
		TargetLanguage: "vi",
		SourceLanguage: "zh",
	})
	if err == nil || !strings.Contains(err.Error(), "missing pinned speech_understand transcript lineage") {
		t.Fatalf("expected missing pinned speech_understand transcript lineage error, got: %v", err)
	}
	if invocations != 0 {
		t.Fatalf("expected 0 provider invocations on fail-closed run, got %d", invocations)
	}

	// 2. Direct translation with RunID + explicit Segments + no speech_understand stage MUST also FAIL CLOSED:
	// 0 provider calls, 0 mutation, even when the same asset has a valid latest transcript from another run (Run 1).
	_, errExplicit := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          run2,
		AssetID:        assetID,
		JobID:          "job-2",
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好", SpeakerID: "S1", StartMs: 0, EndMs: 1000},
		},
	})
	if errExplicit == nil || !strings.Contains(errExplicit.Error(), "missing pinned speech_understand transcript lineage") {
		t.Fatalf("expected error for run with explicit segments but missing run transcript lineage, got: %v", errExplicit)
	}
	if invocations != 0 {
		t.Fatalf("expected 0 provider invocations on fail-closed run with explicit segments, got %d", invocations)
	}
	if _, err := db.GetTranslationVariantIndexByRun(ctx, run2); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected zero mutation in storage for run2, got err=%v", err)
	}

	// 3. Standalone non-run request (RunID == "") with explicit matching segments preserves compatibility fallback.
	standaloneVariant, errStandalone := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          "",
		AssetID:        assetID,
		JobID:          "",
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好", SpeakerID: "S1", StartMs: 0, EndMs: 1000},
		},
	})
	if errStandalone != nil {
		t.Fatalf("expected standalone non-run explicit segment translation to succeed via asset-latest fallback, got: %v", errStandalone)
	}
	if standaloneVariant.TranscriptArtifactCAS != tObj1.SHA256 {
		t.Fatalf("expected standalone request to bind asset-latest transcript CAS %s, got %s", tObj1.SHA256, standaloneVariant.TranscriptArtifactCAS)
	}
	if invocations != 1 {
		t.Fatalf("expected 1 provider invocation for standalone request, got %d", invocations)
	}
}

// Canonical segmentation is resolved from the role plan the run consumed, so a later edit of the asset's
// plan cannot re-segment an artifact the run already froze. A storage failure while resolving it is
// returned: degrading to a nil plan would silently drop the dialogue filter and change the verdict.
func TestTranslationService_ResolveCanonicalRolePlan_PinsRunLineageAndFailsClosed(t *testing.T) {
	db, casStore, _, _ := setupTranslationTestEnv(t)
	ctx := context.Background()
	svc := service.NewTranslationService(db, casStore)

	assetID := "asset-role-plan-pin"
	runID := "run-role-plan-pin"
	now := time.Now().UTC()
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: "att-" + assetID, AttestationType: "user_owned", DeclaredBy: "tester", TermsAccepted: true, ConfirmedAt: now,
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, SHA256: "sha-" + assetID, ByteSize: 1024, MimeType: "video/mp4",
		OriginalFilename: "video.mp4", RightsAttestationID: "att-" + assetID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	// The asset's newest plan admits the whole clip; the plan the run froze admits one short window.
	saveAssetPlan := func(plan domain.AudioRolePlan) {
		t.Helper()
		b, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("marshal plan %s: %v", plan.ID, err)
		}
		obj, err := casStore.Put(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("put plan %s: %v", plan.ID, err)
		}
		plan.CASHash = obj.SHA256
		if err := db.SaveAudioRolePlan(ctx, plan); err != nil {
			t.Fatalf("save plan %s: %v", plan.ID, err)
		}
	}
	saveAssetPlan(domain.AudioRolePlan{
		ID: "plan-asset", AssetID: assetID, ProvenanceHash: "prov-plan-asset", CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleNarrationDialogue}},
	})
	runPlanJSON, err := json.Marshal(domain.AudioRolePlan{
		ID: "plan-run", AssetID: assetID, ProvenanceHash: "prov-plan-run", CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue}},
	})
	if err != nil {
		t.Fatalf("marshal run plan: %v", err)
	}
	runPlanObj, err := casStore.Put(bytes.NewReader(runPlanJSON))
	if err != nil {
		t.Fatalf("put run plan: %v", err)
	}
	if got := service.SeedRunTranscriptForTest(ctx, db, casStore, assetID, runID); got == "" {
		t.Fatalf("seed run transcript: no artifact produced")
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runID, Stage: "audio_role_plan", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: runPlanObj.SHA256, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("record run role plan stage: %v", err)
	}

	pinned, err := svc.ResolveCanonicalRolePlanForTest(ctx, assetID, runID)
	if err != nil {
		t.Fatalf("resolve pinned role plan: %v", err)
	}
	if pinned == nil || len(pinned.Segments) != 1 || pinned.Segments[0].EndMs != 1500 {
		t.Fatalf("expected the run's frozen plan [0,1500], got %+v", pinned)
	}

	latest, err := svc.ResolveCanonicalRolePlanForTest(ctx, assetID, "")
	if err != nil {
		t.Fatalf("resolve asset-latest role plan: %v", err)
	}
	if latest == nil || latest.Segments[0].EndMs != 35000 {
		t.Fatalf("expected the asset's newest plan [0,35000] without a run pin, got %+v", latest)
	}

	// A read failure is never reported as "no plan".
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if plan, err := svc.ResolveCanonicalRolePlanForTest(ctx, assetID, ""); err == nil {
		t.Fatalf("expected the storage failure to propagate, got plan %+v", plan)
	}
}

// A frozen variant is reusable only while it still matches the lineage its run pinned. An operator's later
// edit of the asset's AudioRolePlan re-segments the transcript, so the variant the asset's newest plan would
// produce is not this run's translation; and a lineage that cannot be proven fails closed instead of being
// silently replayed.
func TestTranslationService_CanReuseVariant_RefusesChangedRolePlanAndUnprovenLineage(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	jobID := "job-reuse-lineage"
	attID := uuid.NewString()
	now := time.Now().UTC()
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: now,
	}); err != nil {
		t.Fatalf("create attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "sha-reuse-lineage", ByteSize: 100, CreatedAt: now,
	}); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID: jobID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{
		ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	transcript := domain.TranscriptArtifact{
		ID:      "transcript-" + assetID,
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 1500, SourceText: "你好", SpeakerID: "S1", SegmentType: domain.SpeechBlockTypeSpeech},
			{Index: 1, StartMs: 3000, EndMs: 4000, SourceText: "关注", SpeakerID: "S1", SegmentType: domain.SpeechBlockTypeSpeech},
		},
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: tObj.SHA256,
		ProvenanceHash: "prov-transcript-" + assetID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save transcript index: %v", err)
	}
	recordTranslationStage := func(stage, casHash string) {
		t.Helper()
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID: uuid.NewString(), RunID: runID, Stage: stage, Status: domain.StageStatusSucceeded,
			ArtifactSHA256: casHash, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("record %s stage: %v", stage, err)
		}
	}
	recordTranslationStage("speech_understand", tObj.SHA256)

	newPlan := func(id string, segments []domain.AudioSegment) domain.AudioRolePlan {
		t.Helper()
		b, err := json.Marshal(domain.AudioRolePlan{ID: id, AssetID: assetID, ProvenanceHash: "prov-" + id, Segments: segments, CreatedAt: now})
		if err != nil {
			t.Fatalf("marshal plan %s: %v", id, err)
		}
		obj, err := casStore.Put(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("put plan %s: %v", id, err)
		}
		return domain.AudioRolePlan{ID: id, AssetID: assetID, CASHash: obj.SHA256, ProvenanceHash: "prov-" + id, Segments: segments, CreatedAt: now}
	}
	// The plan this run froze covers both blocks; the plan the asset now carries covers only the first, as an
	// operator edit would leave it.
	pinnedPlan := newPlan("plan-pinned", []domain.AudioSegment{
		{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 2500, EndMs: 4500, Role: domain.AudioRoleNarrationDialogue},
	})
	if err := db.SaveAudioRolePlan(ctx, newPlan("plan-asset-latest", []domain.AudioSegment{
		{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
	})); err != nil {
		t.Fatalf("save asset role plan: %v", err)
	}
	recordTranslationStage("audio_role_plan", pinnedPlan.CASHash)

	pinnedSegs := domain.CanonicalTranslationSegments(&transcript, &pinnedPlan)
	if len(pinnedSegs) != 2 {
		t.Fatalf("fixture must segment both blocks under the pinned plan, got %+v", pinnedSegs)
	}

	// The resume derives canonical input from the run's pinned plan, not from the asset's newest one.
	resolved, err := svc.ResolveCanonicalRolePlanForTest(ctx, assetID, runID)
	if err != nil {
		t.Fatalf("resolve pinned role plan: %v", err)
	}
	if len(domain.CanonicalTranslationSegments(&transcript, resolved)) != 2 {
		t.Fatalf("expected the run's pinned segmentation, got %+v", domain.CanonicalTranslationSegments(&transcript, resolved))
	}
	latest, err := svc.ResolveCanonicalRolePlanForTest(ctx, assetID, "")
	if err != nil {
		t.Fatalf("resolve asset role plan: %v", err)
	}
	latestSegs := domain.CanonicalTranslationSegments(&transcript, latest)
	if len(latestSegs) != 1 {
		t.Fatalf("fixture must re-segment under the asset's newest plan, got %+v", latestSegs)
	}

	// A variant frozen from the asset's newest plan does not match the run's pinned canonical input.
	p, _ := reg.Get("fake_llm_translator")
	modelName, modelVersion := p.ModelInfo()
	eff, err := service.EffectiveGlossaryForTest(nil, latestSegs)
	if err != nil {
		t.Fatalf("effective glossary: %v", err)
	}
	reuseIn := domain.TranslationJobInput{
		AssetID: assetID, RunID: runID, JobID: jobID, SourceLanguage: "zh", TargetLanguage: "vi",
		Segments: latestSegs, EffectiveGlossary: eff, TranscriptArtifactCAS: tObj.SHA256,
	}
	staleHash, err := svc.ComputeTranslationInputHashForTest(reuseIn)
	if err != nil {
		t.Fatalf("compute stale input hash: %v", err)
	}
	staleVariant := domain.TranslationVariant{
		ID: "trans-stale-plan", SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: jobID, SourceLanguage: "zh", TargetLanguage: "vi",
		TranscriptArtifactCAS: tObj.SHA256, EffectiveGlossary: eff, InputHash: staleHash,
		ProviderID: p.ID(), ModelName: modelName, ModelVersion: modelVersion,
		ProvenanceHash: "stale-plan-provenance", OverallQAScore: 0.9,
		Segments:  []domain.TranslationSegment{{Index: 0, SourceText: "你好", TargetText: "Xin chào", StartMs: 0, EndMs: 1500, QAConfidence: 0.9, PassedQAGate: true}},
		CreatedAt: now,
	}
	reuseIn.Segments = pinnedSegs
	if svc.CanReuseVariant(ctx, reuseIn, &staleVariant) {
		t.Fatalf("a variant segmented under the asset's newest plan must not be reused as this run's translation")
	}

	// A run whose pinned transcript belongs to another asset is a lineage mismatch, not "no pinned
	// transcript": the sentinel says which, and nothing is reused on an unproven lineage.
	foreignAssetID := uuid.NewString()
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: foreignAssetID, RightsAttestationID: attID, SHA256: "sha-reuse-foreign", ByteSize: 100, CreatedAt: now,
	}); err != nil {
		t.Fatalf("create foreign asset: %v", err)
	}
	foreignRunID := uuid.NewString()
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID: "job-reuse-foreign", SourceAssetID: foreignAssetID, TargetLanguage: "vi", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create foreign job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{
		ID: foreignRunID, JobID: "job-reuse-foreign", Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create foreign run: %v", err)
	}
	if _, err := service.ResolveRunTranscriptCASForTest(ctx, db, foreignRunID, assetID, "reuse"); !errors.Is(err, domain.ErrTranscriptLineageMismatch) {
		t.Fatalf("expected a typed lineage mismatch, got: %v", err)
	}
	foreignIn := reuseIn
	foreignIn.RunID = foreignRunID
	foreignIn.JobID = ""
	if svc.CanReuseVariant(ctx, foreignIn, &staleVariant) {
		t.Fatalf("a variant must not be reused for a run whose transcript lineage belongs to another asset")
	}

	// A verification failure is not "no pinned transcript" either: it fails closed with its own sentinel.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, err := service.ResolveRunTranscriptCASForTest(ctx, db, runID, assetID, "reuse"); !errors.Is(err, domain.ErrTranscriptLineageProofFailed) {
		t.Fatalf("expected a typed proof failure, got: %v", err)
	}
	if svc.CanReuseVariant(ctx, reuseIn, &staleVariant) {
		t.Fatalf("a variant must not be reused when its lineage cannot be verified")
	}
}

func TestTranslationService_RunScopedAudioRolePlan_NeverAdoptsNewerNoDubPlan(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	jobID := "job-trans-rolediv"
	now := time.Now().UTC()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: "att-" + assetID, AttestationType: "user_owned", DeclaredBy: "tester", TermsAccepted: true, ConfirmedAt: now,
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, SHA256: "sha-" + assetID, ByteSize: 1024, MimeType: "video/mp4",
		OriginalFilename: "video.mp4", RightsAttestationID: "att-" + assetID, CreatedAt: now,
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID: jobID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: now,
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now,
	})

	// Pinned AudioRolePlan A with dialogue
	planAJSON, _ := json.Marshal(domain.AudioRolePlan{
		ID: "planA-" + assetID, AssetID: assetID, ProvenanceHash: "prov-planA-" + assetID, CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}},
	})
	planAObj, err := casStore.Put(bytes.NewReader(planAJSON))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID: "planA-" + assetID, AssetID: assetID, CASHash: planAObj.SHA256, ProvenanceHash: "prov-planA-" + assetID, CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}},
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "audio_role_plan",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: planAObj.SHA256,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	transcript := domain.TranscriptArtifact{
		ID: "transcript-" + assetID, AssetID: assetID, RunID: runID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 1000, EndMs: 3000, SourceText: "测试", SpeakerID: "S1", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		CreatedAt: now,
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: tObj.SHA256,
		ProvenanceHash: "prov-transcript-" + assetID, CreatedAt: now,
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	// TranslationVariant for AdaptDubScript
	tv := domain.TranslationVariant{
		ID: uuid.NewString(), SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: jobID, SourceLanguage: "zh", TargetLanguage: "vi",
		TranscriptArtifactCAS: tObj.SHA256, InputHash: "input-hash-test", ProvenanceHash: "prov-hash-test",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "测试", TargetText: "thử nghiệm", SpeakerID: "S1", StartMs: 1000, EndMs: 3000, PassedQAGate: true},
		},
		CreatedAt: now,
	}
	tvBytes, _ := json.Marshal(tv)
	tvObj, _ := casStore.Put(bytes.NewReader(tvBytes))
	_ = db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID: tv.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: tvObj.SHA256,
		CreatedAt: now,
	})

	// Now save newer AudioRolePlan B for asset with NO dialogue
	planBJSON, _ := json.Marshal(domain.AudioRolePlan{
		ID: "planB-" + assetID, AssetID: assetID, ProvenanceHash: "prov-planB-" + assetID, CreatedAt: now.Add(time.Minute),
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleInstrumentalBgm}},
	})
	planBObj, _ := casStore.Put(bytes.NewReader(planBJSON))
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		ID: "planB-" + assetID, AssetID: assetID, CASHash: planBObj.SHA256, ProvenanceHash: "prov-planB-" + assetID, CreatedAt: now.Add(time.Minute),
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleInstrumentalBgm}},
	})

	// 1. ResolveCanonicalRolePlanForTest for Run A must return Plan A
	pinned, err := svc.ResolveCanonicalRolePlanForTest(ctx, assetID, runID)
	if err != nil {
		t.Fatalf("resolve pinned role plan: %v", err)
	}
	if pinned == nil || pinned.CASHash != planAObj.SHA256 {
		t.Fatalf("expected pinned plan A (%s), got %+v", planAObj.SHA256, pinned)
	}

	// 2. LoadSegmentsFromTranscriptForTest for Run A must use Plan A (returns 1 segment, not 0)
	segs, casHash, err := svc.LoadSegmentsFromTranscriptForTest(ctx, assetID, runID, "")
	if err != nil {
		t.Fatalf("load segments from transcript: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment from Plan A dialogue filter, got %d (Plan B would produce 0)", len(segs))
	}
	if casHash != tObj.SHA256 {
		t.Errorf("expected transcript CAS %s, got %s", tObj.SHA256, casHash)
	}

	// 3. AdaptDubScript for Run A must succeed using Plan A, NOT fail with ErrNoDubbingRequired from Plan B
	dubScript, err := svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: jobID, TargetLanguage: "vi",
		TranslationVariantCAS: tvObj.SHA256,
	})
	if err != nil {
		t.Fatalf("expected AdaptDubScript to succeed using Run A's pinned Plan A, got: %v", err)
	}
	if dubScript == nil || len(dubScript.Segments) != 1 {
		t.Fatalf("expected 1 dub script segment, got %+v", dubScript)
	}
}

func TestTranslationVariant_SchemaGate_RefusesStaleSchemaOrWrongContract(t *testing.T) {
	db, casStore, router, _ := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	jobID := "job-schema-gate"
	now := time.Now().UTC()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: "att-" + assetID, AttestationType: "user_owned", DeclaredBy: "tester", TermsAccepted: true, ConfirmedAt: now,
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, SHA256: "sha-" + assetID, ByteSize: 1024, MimeType: "video/mp4",
		OriginalFilename: "video.mp4", RightsAttestationID: "att-" + assetID, CreatedAt: now,
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID: jobID, SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: now,
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: now,
	})

	// Setup role plan and transcript
	rolePlan := domain.AudioRolePlan{
		ID: "role-" + assetID, AssetID: assetID, ProvenanceHash: "prov-role-" + assetID, CreatedAt: now,
		Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}},
	}
	rpBytes, _ := json.Marshal(rolePlan)
	rpObj, _ := casStore.Put(bytes.NewReader(rpBytes))
	rolePlan.CASHash = rpObj.SHA256
	_ = db.SaveAudioRolePlan(ctx, rolePlan)

	transcript := domain.TranscriptArtifact{
		ID: "transcript-" + assetID, AssetID: assetID, RunID: runID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 1000, EndMs: 3000, SourceText: "测试", SpeakerID: "S1", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		CreatedAt: now,
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))
	_ = db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: tObj.SHA256,
		ProvenanceHash: "prov-transcript-" + assetID, CreatedAt: now,
	})

	jobIn := domain.TranslationJobInput{
		RunID: runID, AssetID: assetID, JobID: jobID, TargetLanguage: "vi",
		TranscriptArtifactCAS: tObj.SHA256,
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "测试", SpeakerID: "S1", StartMs: 1000, EndMs: 3000},
		},
	}

	// 1. CanReuseVariant with stale SchemaVersion (e.g. version 2 when current is 3) must be refused
	staleSchemaVariant := domain.TranslationVariant{
		ID: uuid.NewString(), SchemaVersion: domain.TranslationSchemaVersion - 1, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runID, JobID: jobID, SourceLanguage: "zh", TargetLanguage: "vi",
		TranscriptArtifactCAS: tObj.SHA256, InputHash: "h1", ProvenanceHash: "p1",
		Segments:  []domain.TranslationSegment{{Index: 0, SourceText: "测试", TargetText: "thử", SpeakerID: "S1", StartMs: 1000, EndMs: 3000}},
		CreatedAt: now,
	}
	if svc.CanReuseVariant(ctx, jobIn, &staleSchemaVariant) {
		t.Fatal("CanReuseVariant must refuse variant with stale SchemaVersion")
	}

	// 2. CanReuseVariant with mismatched ContractID must be refused
	wrongContractVariant := domain.TranslationVariant{
		ID: uuid.NewString(), SchemaVersion: domain.TranslationSchemaVersion, ContractID: "legacy-unverified-contract-v0",
		AssetID: assetID, RunID: runID, JobID: jobID, SourceLanguage: "zh", TargetLanguage: "vi",
		TranscriptArtifactCAS: tObj.SHA256, InputHash: "h1", ProvenanceHash: "p1",
		Segments:  []domain.TranslationSegment{{Index: 0, SourceText: "测试", TargetText: "thử", SpeakerID: "S1", StartMs: 1000, EndMs: 3000}},
		CreatedAt: now,
	}
	if svc.CanReuseVariant(ctx, jobIn, &wrongContractVariant) {
		t.Fatal("CanReuseVariant must refuse variant with mismatched ContractID")
	}

	// 3. AdaptDubScript with stale SchemaVersion artifact must fail closed
	staleSchemaBytes, _ := json.Marshal(staleSchemaVariant)
	staleSchemaObj, _ := casStore.Put(bytes.NewReader(staleSchemaBytes))
	_, err := svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: jobID, TargetLanguage: "vi",
		TranslationVariantCAS: staleSchemaObj.SHA256,
	})
	if err == nil || !strings.Contains(err.Error(), "translation variant does not satisfy current translation contract") {
		t.Fatalf("expected AdaptDubScript to fail closed on stale SchemaVersion, got: %v", err)
	}

	// 4. AdaptDubScript with mismatched ContractID must fail closed
	wrongContractBytes, _ := json.Marshal(wrongContractVariant)
	wrongContractObj, _ := casStore.Put(bytes.NewReader(wrongContractBytes))
	_, err = svc.AdaptDubScript(ctx, domain.DubScriptJobInput{
		RunID: runID, AssetID: assetID, JobID: jobID, TargetLanguage: "vi",
		TranslationVariantCAS: wrongContractObj.SHA256,
	})
	if err == nil || !strings.Contains(err.Error(), "translation variant does not satisfy current translation contract") {
		t.Fatalf("expected AdaptDubScript to fail closed on mismatched ContractID, got: %v", err)
	}
}
