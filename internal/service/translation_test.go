package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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

	svc.TranslateInvoke = func(_ context.Context, _ provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		return &provider.TranslationResult{
			ProviderID:   provider.GatewayGeminiTranslationProviderID,
			ModelVersion: provider.GatewayGeminiModelAlias,
			Segments: []domain.TranslationSegment{{
				Index:            0,
				SourceText:       req.Segments[0].SourceText,
				TargetText:       "Open the window.",
				NegationPolarity: true,
			}},
		}, nil
	}

	_, err := svc.Translate(ctx, domain.TranslationJobInput{
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
	if err == nil || !errors.Is(err, domain.ErrNegationInverted) {
		t.Fatalf("expected provider self-report to be unable to override lexical negation mismatch, got: %v", err)
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

func TestTranslationService_QAGate_Rejection(t *testing.T) {
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

	// Make fake provider corrupt numbers
	prov, _ := reg.Get("fake_llm_translator")
	fake := prov.(*provider.FakeTranslationProvider)
	fake.CorruptNumbers = true

	// Also make fallback corrupt numbers to prevent fallback passing
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

	_, err := svc.Translate(ctx, input)
	if err == nil {
		t.Fatalf("expected translation QA gate to fail on corrupted numbers, got nil")
	}
	if !errors.Is(err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNumberCorrupted in error chain, got: %v", err)
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
