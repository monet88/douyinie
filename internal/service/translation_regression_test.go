package service_test

import (
	"context"
	"errors"
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

func TestMeaningFirstQAGate_AcceptsEquivalentCompactClockRangeFormatting(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"工作日900-17:00",
		"Weekdays 9:00-17:00",
		"zh",
		"en",
	)
	if !res.Passed {
		t.Fatalf("expected compact 900 and 9:00 to be equivalent clock times, got violations=%v err=%v", res.Violations, res.Err)
	}
}

func TestMeaningFirstQAGate_DoesNotEquateArbitraryCompactNumberWithClockHour(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"库存900件",
		"Stock: 9 units",
		"zh",
		"en",
	)
	if res.Passed || !errors.Is(res.Err, domain.ErrNumberCorrupted) {
		t.Fatalf("expected arbitrary 900 -> 9 change to remain rejected, got passed=%v violations=%v err=%v", res.Passed, res.Violations, res.Err)
	}
}

func TestMeaningFirstQAGate_AffirmativeToNegativeStillFailsClosed(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"打开窗户",
		"Do not open the window",
		"zh",
		"en",
	)
	if res.Passed || !errors.Is(res.Err, domain.ErrNegationInverted) {
		t.Fatalf("expected affirmative source -> negative target to remain rejected, got passed=%v violations=%v err=%v", res.Passed, res.Violations, res.Err)
	}
}

func TestMeaningFirstQAGate_DoesNotTreatAttachedCupQuantityAsBrand(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"1CUP",
		"1 cup",
		"zh",
		"en",
	)
	if !res.Passed {
		t.Fatalf("expected 1CUP to be treated as a quantity token rather than a protected brand, got violations=%v err=%v", res.Violations, res.Err)
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

func TestTranslationService_QAInvalidPrimary_ValidFallback_AdvancesAndRecordsProvenance(t *testing.T) {
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
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "qa-fallback-source", ByteSize: 100, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: "qa-fallback-job", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "qa-fallback-job", Status: "running", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Primary provider produces corrupted numbers -> triggers Meaning-First QA rejection
	primaryProv, ok := reg.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primaryProv.(*provider.FakeTranslationProvider).CorruptNumbers = true

	// Fallback provider produces valid numbers
	fbProv, ok := reg.Get("fake_local_translator_fallback")
	if !ok {
		t.Fatal("fake_local_translator_fallback not found")
	}
	fbProv.(*provider.FakeTranslationProvider).CorruptNumbers = false

	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "qa-fallback-job",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "剪刀以45度角剪掉枝条。", StartMs: 0, EndMs: 1500},
		},
	}

	variant, err := svc.Translate(ctx, input)
	if err != nil {
		t.Fatalf("expected successful translation via fallback, got: %v", err)
	}
	if variant.ProviderID != "fake_local_translator_fallback" {
		t.Fatalf("expected fallback provider fake_local_translator_fallback, got %s", variant.ProviderID)
	}
	if !variant.Segments[0].PassedQAGate {
		t.Fatalf("expected segment to pass QA gate on fallback")
	}

	// Invariant: Router recorded quality_failed for primary, succeeded for fallback
	attempts, err := db.ListProviderAttempts(ctx, runID, "translation")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts recorded, got %d", len(attempts))
	}
	if attempts[0].ProviderID != "fake_llm_translator" || attempts[0].Status != "quality_failed" {
		t.Fatalf("expected attempt 0 to be quality_failed for fake_llm_translator, got %+v", attempts[0])
	}
	if !strings.Contains(attempts[0].ErrorMessage, domain.ErrQualityRejected.Error()) || !strings.Contains(attempts[0].ErrorMessage, domain.ErrNumberCorrupted.Error()) {
		t.Fatalf("expected quality_failed error message to contain ErrQualityRejected and ErrNumberCorrupted, got: %s", attempts[0].ErrorMessage)
	}
	if attempts[1].ProviderID != "fake_local_translator_fallback" || attempts[1].Status != "succeeded" {
		t.Fatalf("expected attempt 1 to be succeeded for fake_local_translator_fallback, got %+v", attempts[1])
	}

	// Invariant: Router recorded fallback SelectionDecision for alternate candidate
	decisions, err := db.ListSelectionDecisions(ctx, runID, "translation")
	if err != nil {
		t.Fatalf("list selection decisions: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected at least 2 selection decisions, got %d", len(decisions))
	}
	lastDecision := decisions[len(decisions)-1]
	if lastDecision.SelectedProviderID != "fake_local_translator_fallback" {
		t.Fatalf("expected last selection decision for fake_local_translator_fallback, got %s", lastDecision.SelectedProviderID)
	}
	if !strings.Contains(lastDecision.DecisionReason, "selected alternate candidate fake_local_translator_fallback after prior candidate failure") {
		t.Fatalf("unexpected decision reason: %s", lastDecision.DecisionReason)
	}
}

func TestTranslationService_AllCandidatesQAInvalid_FailsClosed(t *testing.T) {
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
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "all-invalid-source", ByteSize: 100, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{ID: "all-invalid-job", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: "all-invalid-job", Status: "running", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Both primary and fallback invert negation
	primaryProv, _ := reg.Get("fake_llm_translator")
	primaryFake := primaryProv.(*provider.FakeTranslationProvider)
	primaryFake.CustomTranslations = map[string]string{"请不要打开窗户。": "Hãy mở cửa sổ ra nhé."}
	fbProv, _ := reg.Get("fake_local_translator_fallback")
	fbFake := fbProv.(*provider.FakeTranslationProvider)
	fbFake.CustomTranslations = map[string]string{"请不要打开窗户。": "Hãy mở cửa sổ ra nhé."}
	input := domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          "all-invalid-job",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "请不要打开窗户。", StartMs: 0, EndMs: 1500},
		},
	}

	_, err := svc.Translate(ctx, input)
	if err == nil {
		t.Fatalf("expected translation to fail closed when all candidates fail QA")
	}
	if !errors.Is(err, domain.ErrQualityRejected) {
		t.Fatalf("expected error to wrap domain.ErrQualityRejected, got: %v", err)
	}
	if !errors.Is(err, domain.ErrNegationInverted) {
		t.Fatalf("expected error to wrap negation polarity error, got: %v", err)
	}

	// Invariant: Both attempts recorded as quality_failed in SQLite provenance
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
		if !strings.Contains(att.ErrorMessage, domain.ErrQualityRejected.Error()) {
			t.Errorf("attempt %d error message must contain ErrQualityRejected, got %s", i, att.ErrorMessage)
		}
	}

	// Invariant: Fallback selection decision was recorded
	decisions, err := db.ListSelectionDecisions(ctx, runID, "translation")
	if err != nil {
		t.Fatalf("list selection decisions: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected at least 2 selection decisions, got %d", len(decisions))
	}
}

func TestTranslationService_HybridLadder_Gemini_DeepSeek_FailsClosedWithoutLocalFallback(t *testing.T) {
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS: %v", err)
	}
	db, err := storage.Open(tmpDir + "/ladder.db")
	if err != nil {
		t.Fatalf("setup DB: %v", err)
	}
	defer db.Close()

	reg := provider.NewRegistry()
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	// Production ladder: Gemini -> DeepSeek. Local Qwen may be registered but is not translation-eligible.
	geminiFake := provider.NewFakeTranslationProvider(provider.GatewayGeminiTranslationProviderID)
	geminiFake.Cap.ExecutionTier = "cloud"
	geminiFake.ModelName = "gemini-3.8-flash"
	geminiFake.ModelVersion = "2026-08"
	geminiFake.CorruptNumbers = true // Primary fails QA on numbers
	_ = reg.Register(geminiFake)

	deepseekFake := provider.NewFakeTranslationProvider(provider.GatewayDeepSeekTranslationProviderID)
	deepseekFake.Cap.ExecutionTier = "cloud"
	deepseekFake.ModelName = "deepseek-v4-flash"
	deepseekFake.ModelVersion = "v4"
	// First fallback fails QA on negation inversion
	deepseekFake.CustomTranslations = map[string]string{
		"请将温度调至25度，张伟说不要打开窗户。": "Vui lòng điều chỉnh nhiệt độ đến 25 độ, Trương Vĩ nói hãy mở cửa sổ.",
	}
	_ = reg.Register(deepseekFake)
	qwenFake := provider.NewFakeTranslationProvider(provider.WorkerQwenTranslationProviderID)
	qwenFake.ModelName = "qwen3-4b"
	qwenFake.ModelVersion = "q4_k_m"
	// Local Qwen would succeed if invoked, which makes this a regression guard against accidental local fallback.
	_ = reg.Register(qwenFake)

	initCtx := context.Background()
	for _, p := range reg.ListAll() {
		mName, mVer := p.ModelInfo()
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

	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	runID := uuid.NewString()
	assetID := uuid.NewString()
	attID := uuid.NewString()

	_ = db.CreateRightsAttestation(initCtx, domain.RightsAttestation{ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC()})
	_ = db.CreateSourceAsset(initCtx, domain.SourceAsset{ID: assetID, RightsAttestationID: attID, SHA256: "ladder-source", ByteSize: 100, CreatedAt: time.Now().UTC()})
	_ = db.CreateJob(initCtx, domain.LocalizationJob{ID: "ladder-job", SourceAssetID: assetID, TargetLanguage: "vi", CreatedAt: time.Now().UTC()})
	_ = db.CreateRun(initCtx, domain.LocalizationRun{ID: runID, JobID: "ladder-job", Status: "running", CreatedAt: time.Now().UTC()})

	input := domain.TranslationJobInput{
		RunID:            runID,
		AssetID:          assetID,
		JobID:            "ladder-job",
		SourceLanguage:   "zh",
		TargetLanguage:   "vi",
		ExecutionProfile: domain.ExecutionProfileHybrid,
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "请将温度调至25度，张伟说不要打开窗户。", StartMs: 0, EndMs: 2000},
		},
	}

	_, err = svc.Translate(initCtx, input)
	if err == nil {
		t.Fatal("expected hybrid translation to fail closed after both remote providers fail quality QA")
	}

	// Invariant: Gemini (quality_failed) -> DeepSeek (quality_failed), then stop. Qwen is never attempted.
	attempts, err := db.ListProviderAttempts(initCtx, runID, "translation")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected exactly 2 remote attempts, got %d", len(attempts))
	}
	if attempts[0].ProviderID != provider.GatewayGeminiTranslationProviderID || attempts[0].Status != "quality_failed" {
		t.Errorf("attempt 0 must be Gemini quality_failed, got %+v", attempts[0])
	}
	if attempts[1].ProviderID != provider.GatewayDeepSeekTranslationProviderID || attempts[1].Status != "quality_failed" {
		t.Errorf("attempt 1 must be DeepSeek quality_failed, got %+v", attempts[1])
	}

	// Invariant: fallback selection decision is recorded for DeepSeek only.
	decisions, err := db.ListSelectionDecisions(initCtx, runID, "translation")
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected at least 2 selection decisions across remote ladder, got %d", len(decisions))
	}
	if decisions[1].SelectedProviderID != provider.GatewayDeepSeekTranslationProviderID {
		t.Errorf("expected decision 1 for DeepSeek, got %s", decisions[1].SelectedProviderID)
	}
	for _, decision := range decisions {
		if decision.SelectedProviderID == provider.WorkerQwenTranslationProviderID {
			t.Fatal("local Qwen must never be selected as a production translation fallback")
		}
	}
}
