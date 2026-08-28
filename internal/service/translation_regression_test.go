package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
)

func TestMeaningFirstQAGate_RejectsSemanticFactCorruption(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"今天天气很好。",
		"Hôm nay thời tiết rất tệ.",
		"zh",
		"vi",
	)
	if res.Passed {
		t.Fatal("expected semantic fact corruption to be rejected")
	}
	if !errors.Is(res.Err, domain.ErrFactCorrupted) {
		t.Fatalf("expected ErrFactCorrupted, got %v", res.Err)
	}
}

func TestMeaningFirstQAGate_AcceptsEquivalentCompoundChineseNumber(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"温度调到二十五度。",
		"Điều chỉnh nhiệt độ đến 25 độ.",
		"zh",
		"vi",
	)
	if !res.Passed {
		t.Fatalf("expected 二十五 and 25 to be numerically equivalent, got violations=%v err=%v", res.Violations, res.Err)
	}
}

func TestMeaningFirstQAGate_AcceptsEquivalentEnglishNumberWords(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"温度调到二十五度。",
		"Set the temperature to twenty five degrees.",
		"zh",
		"en",
	)
	if !res.Passed {
		t.Fatalf("expected 二十五 and twenty five to be numerically equivalent, got violations=%v err=%v", res.Violations, res.Err)
	}
}

func TestMeaningFirstQAGate_AcceptsEquivalentVietnameseNumberWords(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"温度调到二十五度。",
		"Điều chỉnh nhiệt độ đến hai mươi lăm độ.",
		"zh",
		"vi",
	)
	if !res.Passed {
		t.Fatalf("expected 二十五 and hai mươi lăm to be numerically equivalent, got violations=%v err=%v", res.Violations, res.Err)
	}
}

func TestTranslationService_FallbackAppendsSelectionDecision(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "translation-fallback-source", ByteSize: 100, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: "translation-fallback-job", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "translation-fallback-job", Status: "running", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	primaryProv, ok := reg.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primaryProv.(*provider.FakeTranslationProvider).InjectError = errors.New("simulated transient translation failure")

	_, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "translation-fallback-job",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1500},
		},
	})
	if err != nil {
		t.Fatalf("translate with fallback: %v", err)
	}

	decisions, err := db.ListSelectionDecisions(ctx, runID, "translation")
	if err != nil {
		t.Fatalf("list selection decisions: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected initial and fallback SelectionDecision records, got %d", len(decisions))
	}
	if decisions[len(decisions)-1].SelectedProviderID != "fake_local_translator_fallback" {
		t.Fatalf("expected fallback selection decision for fake_local_translator_fallback, got %s", decisions[len(decisions)-1].SelectedProviderID)
	}
}

func TestTranslationService_CacheIdentityChangesWithProviderModelVersion(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "translation-cache-source", ByteSize: 100, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: "translation-cache-job", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "translation-cache-job", Status: "running", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "translation-cache-job",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1500},
		},
	}

	first, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("first translation: %v", err)
	}
	if first.SchemaVersion != domain.TranslationSchemaVersion {
		t.Fatalf("expected TranslationVariant schema version %d, got %d", domain.TranslationSchemaVersion, first.SchemaVersion)
	}

	primaryProv, ok := reg.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primary := primaryProv.(*provider.FakeTranslationProvider)
	primary.ModelVersion = "2.0"

	licenseSvc := governance.NewLicenseService(db)
	if err := licenseSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		DependencyName: "llm-translator",
		Version:        "2.0",
		SHA256:         "sha256_mock_llm-translator_2.0",
		SourceRepo:     "github.com/monet88/douyinie/models/llm-translator",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register v2 translation manifest: %v", err)
	}

	second, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("second translation after model version change: %v", err)
	}
	if first.ProvenanceHash == second.ProvenanceHash {
		t.Fatalf("expected provider model-version change to alter translation cache identity; both=%s", first.ProvenanceHash)
	}
	if first.ID == second.ID {
		t.Fatalf("expected a new immutable TranslationVariant after provider model-version change; both=%s", first.ID)
	}
}
