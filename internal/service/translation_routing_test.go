package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func seedTestAsset(t *testing.T, ctx context.Context, db *storage.DB) string {
	t.Helper()
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	assetID := uuid.NewString()
	err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256-routing",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	return assetID
}

func TestTranslationService_CacheIsolationAcrossServiceBaselines(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()
	casStore, err := cas.NewStore(filepath.Join(tmpDir, "cas"))
	if err != nil {
		t.Fatalf("NewCAS: %v", err)
	}

	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := map[string]any{
			"id":                 "chatcmpl-test",
			"model":              "gemini-3.8-flash-001",
			"system_fingerprint": "fp_gemini_test",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role": "assistant",
						"content": `{"segments": [
							{
								"index": 0,
								"source_text": "你好世界",
								"target_text": "Xin chào thế giới",
								"key_facts": [],
								"negation_polarity": false
							}
						]}`,
					},
					"finish_reason": "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	// 1. First translation run with Baseline A
	regA := provider.NewRegistry()
	geminiA, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-2026-08-A",
		0.99,
		ts.Client(),
		ts.URL,
	)
	geminiA.SetPolicyState(domain.PolicyAllowed)
	mockResolver := func(ctx context.Context, ref, providerID string) (string, error) {
		return "mock-token-for-" + ref, nil
	}
	geminiA.SetSecretResolver(mockResolver)
	_ = regA.Register(geminiA)
	routerA := provider.NewRouter(regA, nil, nil, nil, nil, db)

	svcA := service.NewTranslationService(db, casStore)
	svcA.ConfigureRouter(routerA)

	assetID := seedTestAsset(t, ctx, db)
	in := domain.TranslationJobInput{
		RunID:                 uuid.NewString(),
		AssetID:               assetID,
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		AuthorizedCredentials: []string{"test_cred"},
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	}

	service.SeedRunTranscriptForTest(ctx, db, casStore, in.AssetID, in.RunID)
	v1, err := svcA.Translate(ctx, in)
	if err != nil {
		t.Fatalf("Translate run 1 failed: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 gateway invocation, got %d", callCount)
	}
	if v1.ServiceBaselineID != "baseline-2026-08-A" {
		t.Fatalf("expected baseline-2026-08-A, got: %q", v1.ServiceBaselineID)
	}

	// 2. Second translation run with IDENTICAL input and Baseline A -> MUST HIT CACHE (callCount stays 1)
	in2 := in
	in2.RunID = uuid.NewString()
	service.SeedRunTranscriptForTest(ctx, db, casStore, in2.AssetID, in2.RunID)
	v2, err := svcA.Translate(ctx, in2)
	if err != nil {
		t.Fatalf("Translate run 2 failed: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected cache hit for same baseline (callCount=1), got: %d", callCount)
	}
	if v2.ProvenanceHash != v1.ProvenanceHash {
		t.Fatalf("expected identical provenance hash for same baseline, got %q vs %q", v2.ProvenanceHash, v1.ProvenanceHash)
	}

	// 3. Third translation run with Baseline B -> CANNOT REUSE CACHE across service baselines!
	regB := provider.NewRegistry()
	geminiB, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-2026-09-B", // Changed service baseline!
		0.99,
		ts.Client(),
		ts.URL,
	)
	geminiB.SetSecretResolver(mockResolver)
	geminiB.SetPolicyState(domain.PolicyAllowed)
	_ = regB.Register(geminiB)
	routerB := provider.NewRouter(regB, nil, nil, nil, nil, db)

	svcB := service.NewTranslationService(db, casStore)
	svcB.ConfigureRouter(routerB)

	in3 := in
	in3.RunID = uuid.NewString()
	service.SeedRunTranscriptForTest(ctx, db, casStore, in3.AssetID, in3.RunID)
	v3, err := svcB.Translate(ctx, in3)
	if err != nil {
		t.Fatalf("Translate run 3 failed: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("changing service baseline must prevent cache reuse; expected callCount=2, got: %d", callCount)
	}
	if v3.ProvenanceHash == v1.ProvenanceHash {
		t.Fatalf("provenance hash must differ across service baselines; got identical: %q", v3.ProvenanceHash)
	}
	if v3.ServiceBaselineID != "baseline-2026-09-B" {
		t.Fatalf("expected baseline-2026-09-B, got: %q", v3.ServiceBaselineID)
	}
}

func TestTranslationService_ConsentGatedRemoteTranslation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()
	casStore, err := cas.NewStore(filepath.Join(tmpDir, "cas"))
	if err != nil {
		t.Fatalf("NewCAS: %v", err)
	}

	reg := provider.NewRegistry()
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyRequiresExplicitConsent)
	_ = reg.Register(gemini)

	polSvc := governance.NewPolicyService(db)
	router := provider.NewRouter(reg, polSvc, nil, nil, nil, db)

	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	assetID := seedTestAsset(t, ctx, db)
	in := domain.TranslationJobInput{
		RunID:                 uuid.NewString(),
		AssetID:               assetID,
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		AuthorizedCredentials: []string{"test_cred"},
		ConsentGranted:        false, // Explicit consent NOT granted
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	}

	service.SeedRunTranscriptForTest(ctx, db, casStore, in.AssetID, in.RunID)
	// Must fail closed with ErrConsentRequired or ErrNoEligibleProvider
	_, err = svc.Translate(ctx, in)
	if err == nil {
		t.Fatal("expected error when consent is not granted, got nil")
	}
	if !errors.Is(err, domain.ErrNoEligibleProvider) && !errors.Is(err, domain.ErrConsentRequired) {
		t.Fatalf("expected ErrNoEligibleProvider / ErrConsentRequired, got: %v", err)
	}
}

func TestTranslationService_LocalExecution_FailsClosedIfSnapshotMissingOrInaccessible(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()
	casStore, err := cas.NewStore(filepath.Join(tmpDir, "cas"))
	if err != nil {
		t.Fatalf("NewCAS: %v", err)
	}

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	reg := provider.NewRegistry()
	qwen, err := provider.NewWorkerTranslationProvider(
		"qwen3_4b_translator",
		"qwen3_4b_translator",
		"Qwen3-4B-Q4_K_M",
		0.85,
	)
	if err != nil {
		t.Fatalf("NewWorkerTranslationProvider: %v", err)
	}
	qwen.SetRequiresSnapshot(true)
	qwen.SetSnapshotService(snapSvc)
	_ = reg.Register(qwen)

	router := provider.NewRouter(reg, nil, licSvc, nil, nil, db)
	router.SetSnapshotService(snapSvc)

	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	assetID := seedTestAsset(t, ctx, db)

	// Set unrelated env model paths - must be ignored!
	t.Setenv("QWEN3_TRANSLATION_MODEL_PATH", "/unrelated/env/qwen3.gguf")
	t.Setenv("DOUYINIE_SNAPSHOT_DIR", "/unrelated/snapshots")

	in := domain.TranslationJobInput{
		RunID:            uuid.NewString(),
		AssetID:          assetID,
		SourceLanguage:   "zh",
		TargetLanguage:   "vi",
		ExecutionProfile: domain.ExecutionProfileLocal,
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	}

	// Since snapshot envelope is unverified in runtimehost, execution must fail closed!
	service.SeedRunTranscriptForTest(ctx, db, casStore, in.AssetID, in.RunID)
	_, err = svc.Translate(ctx, in)
	if err == nil {
		t.Fatal("expected fail-closed error when snapshot envelope is unverified, got nil")
	}
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider, got: %v", err)
	}
}
