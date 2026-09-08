package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

type customTranslationHarness struct {
	ts       *httptest.Server
	db       *storage.DB
	casStore *cas.Store
	reg      *provider.Registry
	router   *provider.Router
	snapSvc  *governance.SnapshotService
	licSvc   *governance.LicenseService
	polSvc   *governance.PolicyService
	credSvc  *governance.CredentialService
}

func setupCustomTranslationHarness(t *testing.T, reg *provider.Registry) *customTranslationHarness {
	t.Helper()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(filepath.Join(tmpDir, "cas"))
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_seam1_trans.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

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
	router.SetSnapshotService(snapSvc)

	ingestSvc := service.NewIngestService(db, casStore, &media.MockProber{})
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	srv := server.New(server.Config{
		Addr:           "127.0.0.1:0",
		DB:             db,
		CASStore:       casStore,
		Ingest:         ingestSvc,
		Registry:       reg,
		PolicySvc:      polSvc,
		LicenseSvc:     licSvc,
		CredSvc:        credSvc,
		SnapshotSvc:    snapSvc,
		Router:         router,
		QueueSvc:       queueSvc,
		Scheduler:      resScheduler,
		TranslationSvc: service.NewTranslationService(db, casStore),
		VisualTextSvc:  service.NewVisualTextService(db, casStore),
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = db.Close()
	})

	return &customTranslationHarness{
		ts:       ts,
		db:       db,
		casStore: casStore,
		reg:      reg,
		router:   router,
		snapSvc:  snapSvc,
		licSvc:   licSvc,
		polSvc:   polSvc,
		credSvc:  credSvc,
	}
}

func seedSeam1AssetAndJob(t *testing.T, db *storage.DB) (string, string) {
	t.Helper()
	ctx := context.Background()
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "seam1-tester",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	assetID := uuid.NewString()
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "sha256-seam1-asset",
		ByteSize:            2048,
		CreatedAt:           time.Now().UTC(),
	})
	jobID := "job-" + uuid.NewString()
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "pending",
		CreatedAt:      time.Now().UTC(),
	})
	return assetID, jobID
}

func TestSeam1_Translation_LocalProfile_HardExcludesRemoteCandidates(t *testing.T) {
	reg := provider.NewRegistry()

	// 1. Register remote candidates with allowed policy
	gemini, _ := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		"gateway_deepseek_v4_flash_vision_exp",
		"deepseek/deepseek-v4-flash-vision-exp",
		"baseline-deepseek-v4-2026-08",
		0.95,
		"http://127.0.0.1:8080",
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	// 2. Register local fake Qwen with snapshot bypassed for routing check
	fakeQwen := provider.NewFakeTranslationProvider("qwen3_4b_translator")
	_ = reg.Register(fakeQwen)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-" + uuid.NewString()

	// 3. POST /api/v1/assets/:id/translation with execution_profile: "local"
	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   "vi",
		"source_language":   "zh",
		"execution_profile": "local",
		"segments": []map[string]any{
			{"index": 0, "source_text": "你好，世界"},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200/201, got status %d", resp.StatusCode)
	}

	var res struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if res.Variant.ProviderID != "qwen3_4b_translator" {
		t.Fatalf("expected local qwen provider, got %q", res.Variant.ProviderID)
	}

	// 4. Verify selection decisions in SQLite: remote candidates must be PROFILE_TIER_EXCLUDED
	decisions, err := h.db.ListSelectionDecisions(context.Background(), runID, string(provider.TypeTranslation))
	if err != nil || len(decisions) == 0 {
		t.Fatalf("expected recorded selection decision: %v", err)
	}
	for _, cand := range decisions[0].CandidatesEvaluated {
		if cand.ProviderID == "gateway_gemini_3_8_flash" || cand.ProviderID == "gateway_deepseek_v4_flash_vision_exp" {
			if cand.Eligible {
				t.Fatalf("remote candidate %s must be marked ineligible under Local profile", cand.ProviderID)
			}
			if cand.RejectionCode != "PROFILE_TIER_EXCLUDED" {
				t.Fatalf("expected PROFILE_TIER_EXCLUDED for %s, got %s", cand.ProviderID, cand.RejectionCode)
			}
		}
	}
}

func TestSeam1_Translation_HybridProfile_OrderingAndFallback(t *testing.T) {
	reg := provider.NewRegistry()

	// Mock gateway server that fails on Gemini but succeeds on DeepSeek
	var geminiAttempted, deepseekAttempted bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "gemini-3.8-flash" {
			geminiAttempted = true
			http.Error(w, "upstream rate limit", http.StatusTooManyRequests)
			return
		}
		if req.Model == "deepseek/deepseek-v4-flash-vision-exp" {
			deepseekAttempted = true
			resp := map[string]any{
				"id":                 "chatcmpl-deepseek",
				"model":              "deepseek-v4-flash-001",
				"system_fingerprint": "fp_deepseek_exp",
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role": "assistant",
							"content": `{"segments": [
								{
									"index": 0,
									"source_text": "你好，世界",
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
			return
		}
		http.Error(w, "unexpected model", http.StatusBadRequest)
	}))
	defer ts.Close()

	gemini, _ := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		ts.Client(),
		ts.URL,
	)
	mockSec := func(ctx context.Context, ref, pID string) (string, error) {
		return "mock-secret-for-" + ref, nil
	}
	gemini.SetSecretResolver(mockSec)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		"gateway_deepseek_v4_flash_vision_exp",
		"deepseek/deepseek-v4-flash-vision-exp",
		"baseline-deepseek-v4-2026-08",
		0.95,
		ts.Client(),
		ts.URL,
	)
	deepseek.SetSecretResolver(mockSec)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	fakeQwen := provider.NewFakeTranslationProvider("qwen3_4b_translator")
	_ = reg.Register(fakeQwen)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-" + uuid.NewString()

	payload := map[string]any{
		"run_id":                 runID,
		"job_id":                 jobID,
		"target_language":        "vi",
		"source_language":        "zh",
		"execution_profile":      "hybrid",
		"authorized_credentials": []string{"test_token"},
		"segments": []map[string]any{
			{"index": 0, "source_text": "你好，世界"},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200/201, got status %d", resp.StatusCode)
	}

	var res struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !geminiAttempted {
		t.Fatalf("expected gemini to be attempted first in hybrid order")
	}
	if !deepseekAttempted {
		t.Fatalf("expected deepseek to be attempted as fallback")
	}
	if res.Variant.ProviderID != "gateway_deepseek_v4_flash_vision_exp" {
		t.Fatalf("expected fallback provider gateway_deepseek_v4_flash_vision_exp, got %q", res.Variant.ProviderID)
	}
	if res.Variant.ObservedModel != "deepseek-v4-flash-001" {
		t.Fatalf("expected observed model 'deepseek-v4-flash-001', got %q", res.Variant.ObservedModel)
	}
	if res.Variant.ServiceBaselineID != "baseline-deepseek-v4-2026-08" {
		t.Fatalf("expected service baseline 'baseline-deepseek-v4-2026-08', got %q", res.Variant.ServiceBaselineID)
	}

	// Verify attempt provenance in SQLite: Gemini recorded as failed, DeepSeek recorded as succeeded
	attempts, err := h.db.ListProviderAttempts(context.Background(), runID, string(provider.TypeTranslation))
	if err != nil || len(attempts) != 2 {
		t.Fatalf("expected 2 attempts recorded in SQLite, got: %d (err: %v)", len(attempts), err)
	}
	if attempts[0].ProviderID != "gateway_gemini_3_8_flash" || (attempts[0].Status != "failed" && attempts[0].Status != "quality_failed") {
		t.Fatalf("unexpected first attempt: %+v", attempts[0])
	}
	if attempts[1].ProviderID != "gateway_deepseek_v4_flash_vision_exp" || attempts[1].Status != "succeeded" {
		t.Fatalf("unexpected second attempt: %+v", attempts[1])
	}
}

func TestSeam1_Translation_LocalProfile_FailsClosedIfSnapshotUnverified(t *testing.T) {
	reg := provider.NewRegistry()

	// Register WorkerTranslationProvider requiring verified snapshot
	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.85)
	qwen.SetRequiresSnapshot(true)
	_ = reg.Register(qwen)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-" + uuid.NewString()

	// Execute with local profile - snapshot is unverified, router must fail closed with 503
	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   "vi",
		"source_language":   "zh",
		"execution_profile": "local",
		"segments": []map[string]any{
			{"index": 0, "source_text": "你好，世界"},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when snapshot unverified, got: %d", resp.StatusCode)
	}
}

func TestSeam1_Translation_MeaningFirstValidation_RejectsFactOrNegationCorruption(t *testing.T) {
	reg := provider.NewRegistry()

	// Fake provider that corrupts numbers
	fakeQwen := provider.NewFakeTranslationProvider("qwen3_4b_translator")
	fakeQwen.CorruptNumbers = true
	_ = reg.Register(fakeQwen)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-" + uuid.NewString()

	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   "vi",
		"source_language":   "zh",
		"execution_profile": "local",
		"segments": []map[string]any{
			{"index": 0, "source_text": "500ml 锅 120 元"},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate failed: %v", err)
	}
	defer resp.Body.Close()

	// Must return 422 Unprocessable Entity because QA gate rejects number corruption
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity when numbers corrupted, got: %d", resp.StatusCode)
	}
}

func TestSeam1_Translation_MissingOrCorruptEnvelopePath_FailsClosed(t *testing.T) {
	reg := provider.NewRegistry()

	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.85)
	qwen.SetRequiresSnapshot(true)
	_ = reg.Register(qwen)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-" + uuid.NewString()

	// Even if unrelated model paths are exported in the environment,
	// missing/inaccessible snapshot envelope path must fail closed!
	t.Setenv("QWEN3_TRANSLATION_MODEL_PATH", "/unrelated/model.gguf")
	t.Setenv("DOUYINIE_SNAPSHOT_DIR", "/unrelated/snapshots")

	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   "vi",
		"source_language":   "zh",
		"execution_profile": "local",
		"segments": []map[string]any{
			{"index": 0, "source_text": "你好，世界"},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when snapshot envelope is missing/corrupt, got: %d", resp.StatusCode)
	}
}

func TestSeam1_VisualTrack_LocalProfile_HardExcludesRemoteCandidates(t *testing.T) {
	reg := provider.NewRegistry()

	// 1. Remote translation providers
	gemini, _ := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		"gateway_deepseek_v4_flash_vision_exp",
		"deepseek/deepseek-v4-flash-vision-exp",
		"baseline-deepseek-v4-2026-08",
		0.95,
		"http://127.0.0.1:8080",
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	// 2. Local fake Qwen
	fakeQwen := provider.NewFakeTranslationProvider("qwen3_4b_translator")
	_ = reg.Register(fakeQwen)

	// 3. OCR provider so detect-text produces regions needing translation
	ocr := provider.NewFakeOCRProvider("fake_paddle_ocr")
	_ = reg.Register(ocr)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-vt-local-" + uuid.NewString()

	// 4. Detect text
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	dResp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/detect-text", "application/json", bytes.NewReader(detectBody))
	if err != nil {
		t.Fatalf("POST /detect-text failed: %v", err)
	}
	defer dResp.Body.Close()
	if dResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from detect-text, got %d", dResp.StatusCode)
	}

	// 5. POST /visual-track with execution_profile: "local"
	visPayload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   "vi",
		"execution_profile": "local",
	}
	visBody, _ := json.Marshal(visPayload)
	vResp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/visual-track", "application/json", bytes.NewReader(visBody))
	if err != nil {
		t.Fatalf("POST /visual-track failed: %v", err)
	}
	defer vResp.Body.Close()
	if vResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(vResp.Body)
		t.Fatalf("expected 201 Created from visual-track, got %d body=%s", vResp.StatusCode, string(b))
	}

	// 6. Verify selection decisions in SQLite: remote candidates must be PROFILE_TIER_EXCLUDED
	decisions, err := h.db.ListSelectionDecisions(context.Background(), runID, string(provider.TypeTranslation))
	if err != nil || len(decisions) == 0 {
		t.Fatalf("expected recorded selection decision for visual track translation: %v", err)
	}
	for _, decision := range decisions {
		if decision.SelectedProviderID != "qwen3_4b_translator" {
			t.Errorf("expected local qwen selected, got %s", decision.SelectedProviderID)
		}
		for _, cand := range decision.CandidatesEvaluated {
			if cand.ProviderID == "gateway_gemini_3_8_flash" || cand.ProviderID == "gateway_deepseek_v4_flash_vision_exp" {
				if cand.Eligible {
					t.Fatalf("remote candidate %s must be marked ineligible under Local profile", cand.ProviderID)
				}
				if cand.RejectionCode != "PROFILE_TIER_EXCLUDED" {
					t.Fatalf("expected PROFILE_TIER_EXCLUDED for %s, got %s", cand.ProviderID, cand.RejectionCode)
				}
			}
		}
	}
}

func TestSeam1_VisualTrack_HybridProfile_ConsentAndCredentials(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "Xuất"
		resp := map[string]any{
			"id":    "chatcmpl-test",
			"model": "gemini-3.8-flash",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": fmt.Sprintf(`{"segments": [{"index": 0, "source_text": "test", "target_text": "%s"}]}`, target),
					},
					"finish_reason": "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	reg := provider.NewRegistry()
	mockSec := func(ctx context.Context, ref, pID string) (string, error) {
		return "mock-secret-for-" + ref, nil
	}

	// Remote gateways requiring explicit consent
	gemini, _ := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		mockServer.Client(),
		mockServer.URL,
	)
	gemini.SetSecretResolver(mockSec)
	gemini.SetPolicyState(domain.PolicyRequiresExplicitConsent)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		"gateway_deepseek_v4_flash_vision_exp",
		"deepseek/deepseek-v4-flash-vision-exp",
		"baseline-deepseek-v4-2026-08",
		0.95,
		mockServer.Client(),
		mockServer.URL,
	)
	deepseek.SetSecretResolver(mockSec)
	deepseek.SetPolicyState(domain.PolicyRequiresExplicitConsent)
	_ = reg.Register(deepseek)

	fakeQwen := provider.NewFakeTranslationProvider("qwen3_4b_translator")
	_ = reg.Register(fakeQwen)

	ocr := provider.NewFakeOCRProvider("fake_paddle_ocr")
	_ = reg.Register(ocr)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	ctx := context.Background()

	// Register credential ref
	_ = h.credSvc.RegisterCredentialRef(ctx, domain.CredentialRef{
		ID:          "cred_gw",
		Name:        "cred_gw",
		StorageType: domain.StorageTypeEnvRef,
		KeyRef:      "GATEWAY_TOKEN_ENV",
	})

	// Detect text
	runIDConsentMissing := "run-vt-hybrid-noconsent-" + uuid.NewString()
	detectBody, _ := json.Marshal(map[string]any{"run_id": runIDConsentMissing})
	dResp, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/detect-text", "application/json", bytes.NewReader(detectBody))
	if err != nil {
		t.Fatalf("POST /detect-text failed: %v", err)
	}
	dResp.Body.Close()

	// 1. Call visual-track with Hybrid profile and valid credential, but consent_granted=false
	visPayloadNoConsent := map[string]any{
		"run_id":                 runIDConsentMissing,
		"job_id":                 jobID,
		"target_language":        "vi",
		"execution_profile":      "hybrid",
		"authorized_credentials": []string{"cred_gw"},
		"consent_granted":        false,
	}
	bNoConsent, _ := json.Marshal(visPayloadNoConsent)
	vResp1, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/visual-track", "application/json", bytes.NewReader(bNoConsent))
	if err != nil {
		t.Fatalf("POST /visual-track failed: %v", err)
	}
	defer vResp1.Body.Close()
	if vResp1.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from visual-track fallback, got %d", vResp1.StatusCode)
	}

	// Verify decisions: gateways rejected with REQUIRES_CONSENT
	decisions1, err := h.db.ListSelectionDecisions(ctx, runIDConsentMissing, string(provider.TypeTranslation))
	if err != nil || len(decisions1) == 0 {
		t.Fatalf("expected recorded selection decisions: %v", err)
	}
	for _, decision := range decisions1 {
		for _, cand := range decision.CandidatesEvaluated {
			if cand.ProviderID == "gateway_gemini_3_8_flash" || cand.ProviderID == "gateway_deepseek_v4_flash_vision_exp" {
				if cand.Eligible {
					t.Fatalf("gateway %s must not be eligible when consent_granted=false", cand.ProviderID)
				}
				if cand.RejectionCode != "REQUIRES_CONSENT" {
					t.Fatalf("expected REQUIRES_CONSENT for %s, got %s", cand.ProviderID, cand.RejectionCode)
				}
			}
		}
	}

	// 2. Call visual-track with Hybrid profile, valid credential, and consent_granted=true
	runIDConsentGranted := "run-vt-hybrid-consent-" + uuid.NewString()
	detectBody2, _ := json.Marshal(map[string]any{"run_id": runIDConsentGranted})
	dResp2, _ := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/detect-text", "application/json", bytes.NewReader(detectBody2))
	dResp2.Body.Close()

	visPayloadConsent := map[string]any{
		"run_id":                 runIDConsentGranted,
		"job_id":                 jobID,
		"target_language":        "vi",
		"execution_profile":      "hybrid",
		"authorized_credentials": []string{"cred_gw"},
		"consent_granted":        true,
	}
	bConsent, _ := json.Marshal(visPayloadConsent)
	vResp2, err := http.Post(h.ts.URL+"/api/v1/assets/"+assetID+"/visual-track", "application/json", bytes.NewReader(bConsent))
	if err != nil {
		t.Fatalf("POST /visual-track failed: %v", err)
	}
	defer vResp2.Body.Close()
	if vResp2.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(vResp2.Body)
		t.Fatalf("expected 201 Created, got %d body=%s", vResp2.StatusCode, string(b))
	}

	// Verify decisions: gateways must now be ELIGIBLE
	decisions2, err := h.db.ListSelectionDecisions(ctx, runIDConsentGranted, string(provider.TypeTranslation))
	if err != nil || len(decisions2) == 0 {
		t.Fatalf("expected recorded selection decisions: %v", err)
	}
	for _, decision := range decisions2 {
		for _, cand := range decision.CandidatesEvaluated {
			if cand.ProviderID == "gateway_gemini_3_8_flash" || cand.ProviderID == "gateway_deepseek_v4_flash_vision_exp" {
				if !cand.Eligible {
					t.Fatalf("gateway %s must be eligible when consent_granted=true and authorized, got rejection: %s (%s)",
						cand.ProviderID, cand.RejectionCode, cand.Reason)
				}
			}
		}
	}
}
