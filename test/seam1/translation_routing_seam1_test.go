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
	"strings"
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

func TestSeam1_Translation_LocalProfile_DisablesTranslation(t *testing.T) {
	reg := provider.NewRegistry()

	// 1. Register remote candidates with allowed policy
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
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

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 because Local profile does not provide an E2E translation lane, got status %d", resp.StatusCode)
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
		if req.Model == "deepseek-v4.1-flash" {
			deepseekAttempted = true
			resp := map[string]any{
				"id":                 "chatcmpl-deepseek",
				"model":              "deepseek/deepseek-v4.1-flash",
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
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
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
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
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
	if res.Variant.ProviderID != provider.GatewayDeepSeekTranslationProviderID {
		t.Fatalf("expected fallback provider gateway_deepseek_v4_1_flash, got %q", res.Variant.ProviderID)
	}
	if res.Variant.ObservedModel != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("expected observed model 'deepseek/deepseek-v4.1-flash', got %q", res.Variant.ObservedModel)
	}
	if res.Variant.ServiceBaselineID != "baseline-deepseek-v4-2026-08" {
		t.Fatalf("expected service baseline 'baseline-deepseek-v4-2026-08', got %q", res.Variant.ServiceBaselineID)
	}

	// Verify attempt provenance in SQLite: Gemini recorded as failed, DeepSeek recorded as succeeded
	attempts, err := h.db.ListProviderAttempts(context.Background(), runID, string(provider.TypeTranslation))
	if err != nil || len(attempts) != 2 {
		t.Fatalf("expected 2 attempts recorded in SQLite, got: %d (err: %v)", len(attempts), err)
	}
	if attempts[0].ProviderID != provider.GatewayGeminiTranslationProviderID || (attempts[0].Status != "failed" && attempts[0].Status != "quality_failed") {
		t.Fatalf("unexpected first attempt: %+v", attempts[0])
	}
	if attempts[1].ProviderID != provider.GatewayDeepSeekTranslationProviderID || attempts[1].Status != "succeeded" {
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

func TestSeam1_Translation_MeaningFirstValidation_FlagsFactOrNegationCorruption(t *testing.T) {
	reg := provider.NewRegistry()

	// Remote translation fake corrupts numbers: the gate must flag it, not discard it,
	// and the flag must reach the review queue.
	fakeGateway := provider.NewFakeTranslationProvider(provider.GatewayGeminiTranslationProviderID)
	fakeGateway.Cap.ExecutionTier = "cloud"
	fakeGateway.CorruptNumbers = true
	_ = reg.Register(fakeGateway)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-" + uuid.NewString()

	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   "vi",
		"source_language":   "zh",
		"execution_profile": "hybrid",
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

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 Created with a flagged variant when numbers corrupted, got: %d body=%s", resp.StatusCode, string(raw))
	}
	var translateRes struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&translateRes); err != nil {
		t.Fatalf("decode translate response: %v", err)
	}
	if len(translateRes.Variant.Segments) != 1 {
		t.Fatalf("expected the flagged candidate to be persisted, got %d segments", len(translateRes.Variant.Segments))
	}
	if translateRes.Variant.Segments[0].PassedQAGate {
		t.Fatalf("expected the corrupted-number segment to stay flagged, got passed_qa_gate=true")
	}
	if !strings.Contains(strings.ToLower(translateRes.Variant.Segments[0].ReviewReason), "number") {
		t.Errorf("expected the review reason to name the number corruption, got %q", translateRes.Variant.Segments[0].ReviewReason)
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

func TestSeam1_VisualTrack_LocalProfile_DisablesTranslation(t *testing.T) {
	reg := provider.NewRegistry()

	// 1. Remote translation providers
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
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
	if vResp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(vResp.Body)
		t.Fatalf("expected visual-track to fail closed because Local profile cannot translate visual text, got %d body=%s", vResp.StatusCode, string(b))
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
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		mockServer.Client(),
		mockServer.URL,
	)
	gemini.SetSecretResolver(mockSec)
	gemini.SetPolicyState(domain.PolicyRequiresExplicitConsent)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
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

	// 1. Call visual-track with Hybrid profile and valid credential, but consent_granted=false.
	// With no local translation fallback this must fail closed.
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
	if vResp1.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected visual-track to fail closed when remote translation lacks consent and no local fallback exists, got %d", vResp1.StatusCode)
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
			if cand.ProviderID == provider.GatewayGeminiTranslationProviderID || cand.ProviderID == provider.GatewayDeepSeekTranslationProviderID {
				if !cand.Eligible {
					t.Fatalf("gateway %s must be eligible when consent_granted=true and authorized, got rejection: %s (%s)",
						cand.ProviderID, cand.RejectionCode, cand.Reason)
				}
			}
		}
	}
}

// TestSeam1_Translation_MeaningGateFlag_SurfacesForReviewAndOverride proves the operator loop for a
// meaning-gate violation at Seam 1: the translation stage persists the best-effort variant with the
// offending segment flagged, the violation surfaces as a pending review exception that names what is
// wrong, and the operator clears it by recording a manual override. A flagged gate verdict must inform
// the operator, never dead-end the run.
func TestSeam1_Translation_MeaningGateFlag_SurfacesForReviewAndOverride(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	const sourceText = "请不要打开窗户。"
	const inverted = "Hãy mở cửa sổ ra nhé." // prohibition rendered as an affirmative
	for _, id := range []string{"fake_llm_translator", "fake_local_translator_fallback"} {
		p, ok := h.registry.Get(id)
		if !ok {
			t.Fatalf("provider %s not registered in the seam 1 harness", id)
		}
		p.(*provider.FakeTranslationProvider).CustomTranslations = map[string]string{sourceText: inverted}
	}

	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"source_language":   "zh",
		"target_language":   "vi",
		"execution_profile": "hybrid",
		"segments": []map[string]any{
			{"index": 0, "source_text": sourceText, "start_ms": 0, "end_ms": 1500},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 Created with a flagged variant, got %d body=%s", resp.StatusCode, string(raw))
	}
	var translateRes struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&translateRes); err != nil {
		t.Fatalf("decode translate response: %v", err)
	}
	if len(translateRes.Variant.Segments) != 1 {
		t.Fatalf("expected the flagged candidate to be persisted, got %d segments", len(translateRes.Variant.Segments))
	}
	flagged := translateRes.Variant.Segments[0]
	if flagged.PassedQAGate {
		t.Fatalf("expected the inverted negation to stay flagged, got passed_qa_gate=true")
	}
	if !strings.Contains(flagged.ReviewReason, "negation polarity inverted") {
		t.Errorf("expected the persisted review reason to name the inverted negation, got %q", flagged.ReviewReason)
	}

	// The operator can see exactly what is wrong.
	pending := fetchRunReviewItems(t, h, runID, false)
	var item *domain.ReviewItem
	for i := range pending {
		if pending[i].Type == domain.ReviewItemTypeTranslationQA && pending[i].ItemIndex == 0 {
			item = &pending[i]
			break
		}
	}
	if item == nil {
		t.Fatalf("expected a pending translation_qa review item, got %+v", pending)
	}
	if !strings.Contains(item.Reason, "negation polarity inverted") {
		t.Errorf("expected the review item reason to name the inverted negation, got %q", item.Reason)
	}
	if item.Status != domain.ReviewItemStatusPending {
		t.Errorf("expected a pending review item, got %s", item.Status)
	}

	// ...and can accept it as-is: the manual override clears the queue.
	overrideBody, _ := json.Marshal(map[string]any{
		"review_item_id": item.ID,
		"item_type":      string(item.Type),
		"stage":          item.Stage,
		"item_index":     item.ItemIndex,
		"reason":         "operator accepted the translation as-is",
		"operator":       "seam1-operator",
	})
	ovResp, err := http.Post(h.server.URL+"/api/v1/runs/"+runID+"/review/override", "application/json", bytes.NewReader(overrideBody))
	if err != nil {
		t.Fatalf("POST review override failed: %v", err)
	}
	defer ovResp.Body.Close()
	if ovResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(ovResp.Body)
		t.Fatalf("expected 201 Created for the manual override, got %d body=%s", ovResp.StatusCode, string(raw))
	}

	for _, remaining := range fetchRunReviewItems(t, h, runID, false) {
		if remaining.ID == item.ID {
			t.Fatalf("overridden item %s must leave the pending queue", item.ID)
		}
	}
	resolved := fetchRunReviewItems(t, h, runID, true)
	var seen bool
	for _, it := range resolved {
		if it.ID == item.ID {
			seen = true
			if it.Status != domain.ReviewItemStatusManualOverride {
				t.Errorf("expected the override to be recorded on the item, got status %s", it.Status)
			}
		}
	}
	if !seen {
		t.Fatalf("expected the audited item to remain visible with include_resolved=true")
	}
}

func fetchRunReviewItems(t *testing.T, h *testHarness, runID string, includeResolved bool) []domain.ReviewItem {
	t.Helper()
	url := h.server.URL + "/api/v1/runs/" + runID + "/review-items"
	if includeResolved {
		url += "?include_resolved=true"
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET review-items failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items returned %d", resp.StatusCode)
	}
	var out struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode review-items: %v", err)
	}
	return out.ReviewItems
}
