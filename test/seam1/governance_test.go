package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
)

func TestSeam1_PolicyBeforeHealth(t *testing.T) {
	h := setupHarness(t)
	runID := setupRunAndPlan(t, h)

	// 1. Route TTS for Vietnamese:
	// "fake_indextts2_blocked" has higher quality (0.99) and is Healthy=true, but has PolicyState=BLOCKED.
	// Router must select "fake_vieneu_tts_vi" (0.92, ALLOWED) and NEVER select the blocked provider.
	routePayload := map[string]any{
		"run_id":   runID,
		"stage":    "tts",
		"language": "vi",
	}
	body, _ := json.Marshal(routePayload)

	resp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res struct {
		SelectedProviderID string                   `json:"selected_provider_id"`
		Decision           domain.SelectionDecision `json:"decision"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode decision response: %v", err)
	}

	if res.SelectedProviderID == "fake_indextts2_blocked" {
		t.Fatalf("CRITICAL INVARIANT VIOLATION: healthy but policy-blocked provider was selected!")
	}
	if res.SelectedProviderID != "fake_vieneu_tts_vi" {
		t.Errorf("expected fake_vieneu_tts_vi, got %s", res.SelectedProviderID)
	}

	// 2. Dynamically block fake_vieneu_tts_vi via policy API
	blockBody, _ := json.Marshal(map[string]string{
		"policy_state": "BLOCKED",
		"reason":       "dynamic testing block",
	})
	req, _ := http.NewRequest(http.MethodPut, h.server.URL+"/api/v1/policies/fake_vieneu_tts_vi", bytes.NewReader(blockBody))
	req.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /api/v1/policies failed: %v", err)
	}
	putResp.Body.Close()

	// 3. Routing again should fail-closed (HTTP 422) because no allowed candidate remains
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide second call failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when all candidates are blocked, got %d", resp2.StatusCode)
	}
}

func TestSeam1_RequiredFeaturesEnforcement(t *testing.T) {
	h := setupHarness(t)

	// 1. Request OCR with declared feature "fit_content_geometry" -> succeeds and selects fake_paddle_ocr
	supportedPayload := map[string]any{
		"run_id":            uuid.NewString(),
		"stage":             "ocr",
		"language":          "zh",
		"required_features": []string{"fit_content_geometry"},
	}
	body1, _ := json.Marshal(supportedPayload)
	resp1, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide with feature failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp1.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp1.StatusCode, string(bodyBytes))
	}
	var res1 struct {
		SelectedProviderID string `json:"selected_provider_id"`
	}
	_ = json.NewDecoder(resp1.Body).Decode(&res1)
	if res1.SelectedProviderID != "fake_paddle_ocr" {
		t.Errorf("expected fake_paddle_ocr, got %s", res1.SelectedProviderID)
	}

	// 2. Request OCR with unsupported feature -> returns 422
	unsupportedPayload := map[string]any{
		"run_id":            uuid.NewString(),
		"stage":             "ocr",
		"language":          "zh",
		"required_features": []string{"exotic_unsupported_ocr_feature"},
	}
	body2, _ := json.Marshal(unsupportedPayload)
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide unsupported feature failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity for unsupported required feature, got %d", resp2.StatusCode)
	}
}

func TestSeam1_CredentialBackedAuthorization_NoSelfAuthorization(t *testing.T) {
	h := setupHarness(t)

	// Block standard ASR so only authorization-required provider remains
	blockBody, _ := json.Marshal(map[string]string{
		"policy_state": "BLOCKED",
		"reason":       "isolation test",
	})
	req, _ := http.NewRequest(http.MethodPut, h.server.URL+"/api/v1/policies/fake_qwen3_asr", bytes.NewReader(blockBody))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Also block the 0.6B ASR fallback so only fake_auth_cloud_asr remains eligible.
	blockBody06B, _ := json.Marshal(map[string]string{
		"policy_state": "BLOCKED",
		"reason":       "isolation test",
	})
	req06B, _ := http.NewRequest(http.MethodPut, h.server.URL+"/api/v1/policies/fake_qwen3_asr_06b", bytes.NewReader(blockBody06B))
	req06B.Header.Set("Content-Type", "application/json")
	resp06B, _ := http.DefaultClient.Do(req06B)
	resp06B.Body.Close()
	// 1. Caller attempts to self-authorize merely by passing the provider ID string in authorized_credentials -> Fails 422
	selfAuthPayload := map[string]any{
		"run_id":                 uuid.NewString(),
		"stage":                  "asr",
		"language":               "zh",
		"authorized_credentials": []string{"fake_auth_cloud_asr"},
	}
	selfAuthBody, _ := json.Marshal(selfAuthPayload)
	resp1, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(selfAuthBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide self-auth failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when caller attempts self-authorization without valid CredentialRef, got %d", resp1.StatusCode)
	}

	// 2. Register valid CredentialRef for the provider via /api/v1/credentials
	credRefPayload := map[string]any{
		"name":         "enterprise_asr_key_ref",
		"provider_id":  "fake_auth_cloud_asr",
		"storage_type": "env_ref",
		"key_ref":      "enterprise_vault_token_01",
	}
	credRefBody, _ := json.Marshal(credRefPayload)
	credResp, err := http.Post(h.server.URL+"/api/v1/credentials", "application/json", bytes.NewReader(credRefBody))
	if err != nil || credResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/credentials failed: status=%d, err=%v", credResp.StatusCode, err)
	}
	var credResult struct {
		CredentialRef domain.CredentialRef `json:"credential_ref"`
	}
	_ = json.NewDecoder(credResp.Body).Decode(&credResult)
	credResp.Body.Close()

	// 2a. Routing with registered but unbacked credential reference fails closed (422)
	unbackedPayload := map[string]any{
		"run_id":                 uuid.NewString(),
		"stage":                  "asr",
		"language":               "zh",
		"authorized_credentials": []string{credResult.CredentialRef.ID},
	}
	unbackedBody, _ := json.Marshal(unbackedPayload)
	unbackedResp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(unbackedBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide unbacked failed: %v", err)
	}
	if unbackedResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when backing key is not verifiable, got %d", unbackedResp.StatusCode)
	}
	unbackedResp.Body.Close()

	// 3. Set backing environment variable -> Routing with registered credential reference ID succeeds (200 OK)
	t.Setenv("enterprise_vault_token_01", "mock_enterprise_secret_token_01")
	validAuthPayload := map[string]any{
		"run_id":                 uuid.NewString(),
		"stage":                  "asr",
		"language":               "zh",
		"authorized_credentials": []string{credResult.CredentialRef.ID},
	}
	validAuthBody, _ := json.Marshal(validAuthPayload)
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(validAuthBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide valid cred failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp2.Body)
		t.Fatalf("expected 200 OK with valid credential reference, got %d: %s", resp2.StatusCode, string(bodyBytes))
	}
	var res2 struct {
		SelectedProviderID string `json:"selected_provider_id"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&res2)
	if res2.SelectedProviderID != "fake_auth_cloud_asr" {
		t.Errorf("expected fake_auth_cloud_asr, got %s", res2.SelectedProviderID)
	}
}

func TestSeam1_RetryProvenanceAndDecisions_RealExecutionPath(t *testing.T) {
	h := setupHarness(t)
	runID := uuid.NewString()

	// 1. Execute with transient retry simulation via injected executor seam (real HTTP path)
	transientAttempts := 0
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		transientAttempts++
		if transientAttempts < 2 {
			return errors.New("simulated transient worker timeout")
		}
		return nil
	})

	execTransientPayload := map[string]any{
		"run_id":      runID,
		"stage":       "asr",
		"language":    "zh",
		"input_hash":  "hash_input_transient_001",
		"max_retries": 2,
	}
	body1, _ := json.Marshal(execTransientPayload)
	resp1, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute transient failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp1.Body)
		t.Fatalf("expected 200 OK for transient execution retry, got %d: %s", resp1.StatusCode, string(bodyBytes))
	}

	// Query attempts list for runID
	listResp1, err := http.Get(fmt.Sprintf("%s/api/v1/routing/attempts?run_id=%s&stage=asr", h.server.URL, runID))
	if err != nil {
		t.Fatalf("GET /api/v1/routing/attempts failed: %v", err)
	}
	defer listResp1.Body.Close()

	var attemptsResult1 struct {
		Attempts []domain.ProviderAttempt `json:"attempts"`
	}
	if err := json.NewDecoder(listResp1.Body).Decode(&attemptsResult1); err != nil {
		t.Fatalf("decode attempts: %v", err)
	}

	if len(attemptsResult1.Attempts) != 2 {
		t.Fatalf("expected 2 immutable attempts in history, got %d", len(attemptsResult1.Attempts))
	}
	if attemptsResult1.Attempts[0].Status != "failed" || attemptsResult1.Attempts[1].Status != "succeeded" {
		t.Errorf("incorrect attempt sequence: %+v", attemptsResult1.Attempts)
	}

	// 2. Execute with Quality Rejection simulation via injected executor seam (real HTTP path)
	runIDQuality := setupRunAndPlan(t, h)
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		if p.ID() == "fake_vieneu_tts_vi" {
			return domain.ErrQualityRejected
		}
		return nil
	})

	execQualityPayload := map[string]any{
		"run_id":          runIDQuality,
		"stage":           "tts",
		"language":        "vi",
		"consent_granted": true, // allows fallback to fake_cloud_tts_consent
		"input_hash":      "hash_input_quality_002",
		"max_retries":     2,
	}
	body2, _ := json.Marshal(execQualityPayload)
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute quality failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp2.Body)
		t.Fatalf("expected 200 OK for quality execution fallback, got %d: %s", resp2.StatusCode, string(bodyBytes))
	}

	// Query attempts for quality run
	listResp2, err := http.Get(fmt.Sprintf("%s/api/v1/routing/attempts?run_id=%s&stage=tts", h.server.URL, runIDQuality))
	if err != nil {
		t.Fatalf("GET /api/v1/routing/attempts quality failed: %v", err)
	}
	defer listResp2.Body.Close()

	var attemptsResult2 struct {
		Attempts []domain.ProviderAttempt `json:"attempts"`
	}
	_ = json.NewDecoder(listResp2.Body).Decode(&attemptsResult2)

	if len(attemptsResult2.Attempts) != 2 {
		t.Fatalf("expected 2 attempts recorded for quality fallback, got %d", len(attemptsResult2.Attempts))
	}
	if attemptsResult2.Attempts[0].Status != "quality_failed" || attemptsResult2.Attempts[0].ProviderID != "fake_vieneu_tts_vi" {
		t.Errorf("expected quality_failed on candidate 1, got %+v", attemptsResult2.Attempts[0])
	}
	if attemptsResult2.Attempts[1].Status != "succeeded" || attemptsResult2.Attempts[1].ProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected succeeded on fallback candidate 2, got %+v", attemptsResult2.Attempts[1])
	}

	// Query selection decisions for quality run: verify provenance includes decision for fallback provider
	decResp, err := http.Get(fmt.Sprintf("%s/api/v1/routing/decisions?run_id=%s&stage=tts", h.server.URL, runIDQuality))
	if err != nil {
		t.Fatalf("GET /api/v1/routing/decisions failed: %v", err)
	}
	defer decResp.Body.Close()

	var decResult struct {
		Decisions []domain.SelectionDecision `json:"decisions"`
	}
	_ = json.NewDecoder(decResp.Body).Decode(&decResult)
	if len(decResult.Decisions) < 2 {
		t.Fatalf("expected at least 2 selection decisions recorded (initial + fallback), got %d", len(decResult.Decisions))
	}
	if decResult.Decisions[1].SelectedProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected fallback selection decision for fake_cloud_tts_consent, got %s", decResult.Decisions[1].SelectedProviderID)
	}

	// 3. Execute with Policy Rejection simulation (Fail-closed immediately, no retry)
	runIDPolicy := uuid.NewString()
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		return domain.ErrPolicyBlocked
	})

	execPolicyPayload := map[string]any{
		"run_id":      runIDPolicy,
		"stage":       "asr",
		"language":    "zh",
		"input_hash":  "hash_input_policy_003",
		"max_retries": 3,
	}
	body3, _ := json.Marshal(execPolicyPayload)
	resp3, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body3))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute policy failed: %v", err)
	}
	defer resp3.Body.Close()

	if resp3.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity for policy rejection, got %d", resp3.StatusCode)
	}

	listResp3, _ := http.Get(fmt.Sprintf("%s/api/v1/routing/attempts?run_id=%s&stage=asr", h.server.URL, runIDPolicy))
	var attemptsResult3 struct {
		Attempts []domain.ProviderAttempt `json:"attempts"`
	}
	_ = json.NewDecoder(listResp3.Body).Decode(&attemptsResult3)
	listResp3.Body.Close()

	if len(attemptsResult3.Attempts) != 1 || attemptsResult3.Attempts[0].Status != "policy_rejected" {
		t.Errorf("expected exactly 1 policy_rejected attempt without retry, got %+v", attemptsResult3.Attempts)
	}

	// 4. Execute with Circuit Breaker skipping a provider during retry/fallback (persists append-only circuit_broken attempt provenance)
	thirdProv := provider.NewFakeTTSProvider("fake_z_seam1_tts_vi", 1600)
	_ = h.registry.Register(thirdProv)
	mName, mVer := thirdProv.ModelInfo()
	manifestPayload := map[string]any{
		"dependency_name": mName,
		"version":         mVer,
		"sha256":          "sha256_mock_" + mName,
		"source_repo":     "test/" + mName,
		"code_license":    "Apache-2.0",
		"model_license":   "Apache-2.0",
		"data_license":    "OpenData",
		"service_terms":   "Standard",
		"verified":        true,
	}
	manBody, _ := json.Marshal(manifestPayload)
	mResp, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(manBody))
	if err != nil || mResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/licenses for third provider failed: %v", err)
	}
	mResp.Body.Close()

	// Execute with fallback in Cloud profile:
	// Route candidates: 1. fake_cloud_tts_consent, 2. fake_vieneu_tts_vi, 3. fake_z_seam1_tts_vi
	// When candidate 1 executes, it fails quality and trips candidate 2's circuit breaker.
	// Router fallback skips candidate 2 (logging circuit_broken) and executes candidate 3 (logging succeeded).
	runIDCircuit := setupRunAndPlan(t, h)
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		if p.ID() == "fake_cloud_tts_consent" {
			for i := 0; i < 3; i++ {
				h.router.Circuit().RecordFailure("fake_vieneu_tts_vi", true)
			}
			return domain.ErrQualityRejected
		}
		return nil
	})

	execCircuitPayload := map[string]any{
		"run_id":            runIDCircuit,
		"stage":             "tts",
		"language":          "vi",
		"execution_profile": "cloud",
		"consent_granted":   true,
		"input_hash":        "hash_circuit_fallback_002",
		"max_retries":       2,
	}
	circuitBody, _ := json.Marshal(execCircuitPayload)
	cResp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(circuitBody))
	if err != nil {
		t.Fatalf("POST routing execute circuit failed: %v", err)
	}
	defer cResp.Body.Close()

	if cResp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(cResp.Body)
		t.Fatalf("expected 200 OK for circuit fallback execution, got %d: %s", cResp.StatusCode, string(bodyBytes))
	}

	// Verify attempt provenance:
	// Attempt 1: fake_cloud_tts_consent (quality_failed)
	// Attempt 2: fake_vieneu_tts_vi (circuit_broken)
	// Attempt 3: fake_z_seam1_tts_vi (succeeded)
	listResp4, err := http.Get(fmt.Sprintf("%s/api/v1/routing/attempts?run_id=%s&stage=tts", h.server.URL, runIDCircuit))
	if err != nil {
		t.Fatalf("GET /api/v1/routing/attempts failed: %v", err)
	}
	defer listResp4.Body.Close()

	var attemptsResult4 struct {
		Attempts []domain.ProviderAttempt `json:"attempts"`
	}
	_ = json.NewDecoder(listResp4.Body).Decode(&attemptsResult4)

	if len(attemptsResult4.Attempts) != 3 {
		t.Fatalf("expected 3 attempts (quality_failed, circuit_broken, succeeded), got %d: %+v", len(attemptsResult4.Attempts), attemptsResult4.Attempts)
	}
	if attemptsResult4.Attempts[0].Status != "quality_failed" || attemptsResult4.Attempts[0].ProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected attempt 0 to be quality_failed on fake_cloud_tts_consent, got %+v", attemptsResult4.Attempts[0])
	}
	if attemptsResult4.Attempts[1].Status != "circuit_broken" || attemptsResult4.Attempts[1].ProviderID != "fake_vieneu_tts_vi" {
		t.Errorf("expected attempt 1 to be circuit_broken on fake_vieneu_tts_vi, got %+v", attemptsResult4.Attempts[1])
	}
	if attemptsResult4.Attempts[2].Status != "succeeded" || attemptsResult4.Attempts[2].ProviderID != "fake_z_seam1_tts_vi" {
		t.Errorf("expected attempt 2 to be succeeded on fake_z_seam1_tts_vi, got %+v", attemptsResult4.Attempts[2])
	}
}

func TestSeam1_DeterministicCASStageIdentity(t *testing.T) {
	// Invariant: Path, mtime, JobID, RunID changes CANNOT redefine identity.
	// Only stage + input hashes + semantic config + provider/model/version + language + schema version define identity.
	h := setupHarness(t)

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	file1 := createSyntheticMedia(t, dir1, "source_1.mp4")
	file2 := createSyntheticMedia(t, dir2, "renamed_source_2.mp4")

	// Ensure identical file content
	content, err := os.ReadFile(file1)
	if err != nil {
		t.Fatalf("read file1: %v", err)
	}
	if err := os.WriteFile(file2, content, 0644); err != nil {
		t.Fatalf("write file2: %v", err)
	}

	// Simulate different file modification times
	_ = os.Chtimes(file1, time.Now().Add(-1*time.Hour), time.Now().Add(-1*time.Hour))
	_ = os.Chtimes(file2, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour))

	// 1. Ingest File 1 via RuntimeHost HTTP API
	payload1, _ := json.Marshal(map[string]any{
		"file_path": file1,
		"attestation": map[string]any{
			"declared_by":    "operator_1",
			"terms_accepted": true,
		},
	})
	resp1, err := http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(payload1))
	if err != nil {
		t.Fatalf("POST /api/v1/assets/ingest file1 failed: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for file1 ingest, got %d", resp1.StatusCode)
	}
	var res1 service.IngestResult
	_ = json.NewDecoder(resp1.Body).Decode(&res1)

	// 2. Ingest File 2 (different path, different mtime, identical content) via RuntimeHost HTTP API
	payload2, _ := json.Marshal(map[string]any{
		"file_path": file2,
		"attestation": map[string]any{
			"declared_by":    "operator_2",
			"terms_accepted": true,
		},
	})
	resp2, err := http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(payload2))
	if err != nil {
		t.Fatalf("POST /api/v1/assets/ingest file2 failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for file2 ingest, got %d", resp2.StatusCode)
	}
	var res2 service.IngestResult
	_ = json.NewDecoder(resp2.Body).Decode(&res2)

	// Prove CAS deduplication and identity through RuntimeHost API
	if res1.Asset.SHA256 != res2.Asset.SHA256 {
		t.Fatalf("CAS SHA256 mismatch for identical content: %s != %s", res1.Asset.SHA256, res2.Asset.SHA256)
	}
	if res1.Asset.ID != res2.Asset.ID {
		t.Fatalf("CAS Asset.ID deduplication failed: %s != %s", res1.Asset.ID, res2.Asset.ID)
	}

	rawContentSHA := res1.Asset.SHA256

	// 3. Prove logical stage cache identity is deterministic and separate from CAS content SHA
	// Run 1 (Job 1, Run 1)
	inputRun1 := domain.StageCacheIdentityInput{
		Stage:          "tts",
		InputHashes:    []string{rawContentSHA},
		SemanticConfig: map[string]any{"voice_id": "v1", "speed": 1.0},
		ProviderID:     "fake_vieneu_tts_vi",
		ModelName:      "vieneu-v1",
		ModelVersion:   "1.0.0",
		Language:       "vi",
		SchemaVersion:  1,
	}
	key1, err := cas.ComputeStageCacheKey(inputRun1)
	if err != nil {
		t.Fatalf("ComputeStageCacheKey run 1 failed: %v", err)
	}

	// Run 2 (Different Run ID, different Job ID, permuted map keys)
	inputRun2 := domain.StageCacheIdentityInput{
		Stage:          "TTS", // case-insensitive
		InputHashes:    []string{rawContentSHA},
		SemanticConfig: map[string]any{"speed": 1.0, "voice_id": "v1"}, // same semantic content
		ProviderID:     "fake_vieneu_tts_vi",
		ModelName:      "vieneu-v1",
		ModelVersion:   "1.0.0",
		Language:       "VI",
		SchemaVersion:  1,
	}
	key2, err := cas.ComputeStageCacheKey(inputRun2)
	if err != nil {
		t.Fatalf("ComputeStageCacheKey run 2 failed: %v", err)
	}

	if key1 != key2 {
		t.Fatalf("deterministic CAS identity failed: key1 (%s) != key2 (%s)", key1, key2)
	}

	// Invariant: Logical stage cache identity is separate and distinct from raw CAS content SHA
	if key1 == rawContentSHA {
		t.Fatalf("logical stage cache key must be separate from CAS content SHA: got identical hash %s", key1)
	}

	// Changing config alters key
	inputRun3 := inputRun1
	inputRun3.SemanticConfig = map[string]any{"voice_id": "v2", "speed": 1.0}
	key3, _ := cas.ComputeStageCacheKey(inputRun3)
	if key1 == key3 {
		t.Fatalf("expected different key when config changes")
	}
}

func TestSeam1_ExecutionProfilesAndLayeredConfig(t *testing.T) {
	h := setupHarness(t)

	// 1. Layered config endpoint returns default Hybrid profile with zero telemetry
	cfgResp, err := http.Get(h.server.URL + "/api/v1/config/layered?profile=hybrid")
	if err != nil {
		t.Fatalf("GET /api/v1/config/layered failed: %v", err)
	}
	defer cfgResp.Body.Close()

	var cfgResult struct {
		Config domain.LayeredConfig `json:"config"`
	}
	if err := json.NewDecoder(cfgResp.Body).Decode(&cfgResult); err != nil {
		t.Fatalf("decode config: %v", err)
	}

	if cfgResult.Config.Profile != domain.ExecutionProfileHybrid {
		t.Errorf("expected hybrid profile default, got %s", cfgResult.Config.Profile)
	}
	if cfgResult.Config.Telemetry.IsEnabled() {
		t.Errorf("expected telemetry disabled by default, got true")
	}

	// 2. Routing with Cloud profile preference selects cloud provider
	cloudPayload := map[string]any{
		"run_id":            setupRunAndPlan(t, h),
		"stage":             "tts",
		"language":          "vi",
		"execution_profile": "cloud",
		"consent_granted":   true,
	}
	cloudBody, _ := json.Marshal(cloudPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(cloudBody))
	if err != nil {
		t.Fatalf("POST routing decide cloud failed: %v", err)
	}
	defer resp.Body.Close()

	var cloudRes struct {
		SelectedProviderID string `json:"selected_provider_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cloudRes)
	if cloudRes.SelectedProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected fake_cloud_tts_consent for cloud profile, got %s", cloudRes.SelectedProviderID)
	}

	// 3. Routing with Hybrid profile preference selects local cost-first provider
	hybridPayload := map[string]any{
		"run_id":            setupRunAndPlan(t, h),
		"stage":             "tts",
		"language":          "vi",
		"execution_profile": "hybrid",
		"consent_granted":   true,
	}
	hybridBody, _ := json.Marshal(hybridPayload)
	hResp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(hybridBody))
	if err != nil {
		t.Fatalf("POST routing decide hybrid failed: %v", err)
	}
	defer hResp.Body.Close()

	var hRes struct {
		SelectedProviderID string `json:"selected_provider_id"`
	}
	_ = json.NewDecoder(hResp.Body).Decode(&hRes)
	if hRes.SelectedProviderID != "fake_vieneu_tts_vi" {
		t.Errorf("expected fake_vieneu_tts_vi for hybrid profile, got %s", hRes.SelectedProviderID)
	}

	// 4. Layered config resolution preserves explicit false/zero overrides via query params
	cfgOverrideResp, err := http.Get(h.server.URL + "/api/v1/config/layered?profile=cloud&max_retries=0&telemetry_enabled=false&zero_overrun_strict=false")
	if err != nil {
		t.Fatalf("GET /api/v1/config/layered with overrides failed: %v", err)
	}
	defer cfgOverrideResp.Body.Close()

	var cfgOverrideResult struct {
		Config domain.LayeredConfig `json:"config"`
	}
	if err := json.NewDecoder(cfgOverrideResp.Body).Decode(&cfgOverrideResult); err != nil {
		t.Fatalf("decode config overrides: %v", err)
	}

	if cfgOverrideResult.Config.GetMaxRetries() != 0 {
		t.Errorf("expected max_retries=0 override preserved, got %d", cfgOverrideResult.Config.GetMaxRetries())
	}
	if cfgOverrideResult.Config.Telemetry.IsEnabled() {
		t.Errorf("expected telemetry_enabled=false override preserved, got true")
	}
	if cfgOverrideResult.Config.IsZeroOverrunStrict() {
		t.Errorf("expected zero_overrun_strict=false override preserved, got true")
	}
}

func TestSeam1_LicenseManifests_ImmutableVersionedHistory(t *testing.T) {
	h := setupHarness(t)

	// 1. Incomplete manifest (missing DATA_LICENSE) should return 400 Bad Request
	incompletePayload := map[string]any{
		"dependency_name": "custom_checkpoint_v1",
		"version":         "1.0.0",
		"sha256":          "sha256_mock_custom_123",
		"source_repo":     "github.com/monet88/models",
		"code_license":    "MIT",
		"model_license":   "Apache-2.0",
		"data_license":    "", // MISSING
		"service_terms":   "Standard",
		"verified":        true,
	}
	incompBody, _ := json.Marshal(incompletePayload)
	resp1, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(incompBody))
	if err != nil {
		t.Fatalf("POST /api/v1/licenses failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for incomplete 4-layer license manifest, got %d", resp1.StatusCode)
	}

	// 2. Complete 4-layer manifest registration for Version 1.0.0 (201 Created)
	v1Payload := map[string]any{
		"dependency_name": "custom_checkpoint_v1",
		"version":         "1.0.0",
		"sha256":          "sha256_mock_custom_123",
		"source_repo":     "github.com/monet88/models",
		"code_license":    "MIT",
		"model_license":   "Apache-2.0",
		"data_license":    "OpenData-Commercial",
		"service_terms":   "Standard",
		"verified":        true,
	}
	v1Body, _ := json.Marshal(v1Payload)
	resp2, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(v1Body))
	if err != nil || resp2.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/licenses v1 failed: status=%d, err=%v", resp2.StatusCode, err)
	}
	resp2.Body.Close()

	// 3. Register Version 2.0.0 for the same dependency (immutable versioning, no overwriting)
	v2Payload := map[string]any{
		"dependency_name": "custom_checkpoint_v1",
		"version":         "2.0.0",
		"sha256":          "sha256_mock_custom_456",
		"source_repo":     "github.com/monet88/models",
		"code_license":    "MIT",
		"model_license":   "Apache-2.0",
		"data_license":    "OpenData-Commercial",
		"service_terms":   "Standard",
		"verified":        true,
	}
	v2Body, _ := json.Marshal(v2Payload)
	resp3, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(v2Body))
	if err != nil || resp3.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/licenses v2 failed: status=%d, err=%v", resp3.StatusCode, err)
	}
	resp3.Body.Close()

	// 4. Query manifests list for dependency: both versions must be preserved
	listResp, err := http.Get(h.server.URL + "/api/v1/licenses")
	if err != nil {
		t.Fatalf("GET /api/v1/licenses failed: %v", err)
	}
	defer listResp.Body.Close()

	var listResult struct {
		Licenses  []domain.LicenseManifestEntry `json:"licenses"`
		Manifests []domain.LicenseManifestEntry `json:"license_manifests"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listResult)

	foundV1, foundV2 := false, false
	for _, m := range listResult.Licenses {
		if m.DependencyName == "custom_checkpoint_v1" && m.Version == "1.0.0" {
			foundV1 = true
		}
		if m.DependencyName == "custom_checkpoint_v1" && m.Version == "2.0.0" {
			foundV2 = true
		}
	}
	if !foundV1 || !foundV2 {
		t.Errorf("expected both immutable versions preserved in manifest history: v1=%v, v2=%v", foundV1, foundV2)
	}
}

func TestSeam1_SafeCredentialReferences_RejectsRawSecrets(t *testing.T) {
	h := setupHarness(t)

	// 1. Storing a safe credential reference succeeds (201 Created)
	safePayload := map[string]any{
		"name":         "douyin_session_auth",
		"storage_type": "os_credential_store",
		"key_ref":      "douyin_operator_cookie_v1",
	}
	safeBody, _ := json.Marshal(safePayload)
	resp1, err := http.Post(h.server.URL+"/api/v1/credentials", "application/json", bytes.NewReader(safeBody))
	if err != nil {
		t.Fatalf("POST /api/v1/credentials failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusCreated {
		t.Errorf("expected 201 Created for safe credential reference, got %d", resp1.StatusCode)
	}

	// 2. Storing a raw API key secret is rejected (400 Bad Request)
	rawSecretPayload := map[string]any{
		"name":         "leaked_secret",
		"storage_type": "env_ref",
		"key_ref":      "sk-live-secret-api-key-1234567890abcdef",
	}
	secretBody, _ := json.Marshal(rawSecretPayload)
	resp2, err := http.Post(h.server.URL+"/api/v1/credentials", "application/json", bytes.NewReader(secretBody))
	if err != nil {
		t.Fatalf("POST /api/v1/credentials secret failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when attempting to store raw secret, got %d", resp2.StatusCode)
	}
}

func TestSeam1_RouteExecute_NoExecutorFailsClosed(t *testing.T) {
	h := setupHarness(t)
	// Do NOT set executor on harness; srv.executor remains nil.

	execPayload := map[string]any{
		"run_id":     uuid.NewString(),
		"stage":      "asr",
		"language":   "zh",
		"input_hash": "hash_no_executor_test",
	}
	body, _ := json.Marshal(execPayload)

	resp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 500 Internal Server Error when no executor is configured, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

func TestSeam1_RouteExecute_InputHashRequired(t *testing.T) {
	h := setupHarness(t)
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		return nil
	})

	// 1. Missing input_hash
	missingPayload := map[string]any{
		"run_id":   uuid.NewString(),
		"stage":    "asr",
		"language": "zh",
	}
	body1, _ := json.Marshal(missingPayload)
	resp1, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute missing hash failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when input_hash is missing, got %d", resp1.StatusCode)
	}

	// 2. Empty/whitespace input_hash
	emptyPayload := map[string]any{
		"run_id":     uuid.NewString(),
		"stage":      "asr",
		"language":   "zh",
		"input_hash": "   ",
	}
	body2, _ := json.Marshal(emptyPayload)
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute empty hash failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when input_hash is empty/whitespace, got %d", resp2.StatusCode)
	}
}

func TestSeam1_RouteExecute_HonorsExcludedProviders(t *testing.T) {
	h := setupHarness(t)

	var executedProvider string
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		executedProvider = p.ID()
		return nil
	})

	execPayload := map[string]any{
		"run_id":             setupRunAndPlan(t, h),
		"stage":              "tts",
		"language":           "vi",
		"consent_granted":    true,
		"input_hash":         "hash_excluded_test_seam1",
		"excluded_providers": []string{"fake_vieneu_tts_vi"},
	}
	body, _ := json.Marshal(execPayload)

	resp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute with excluded_providers failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if executedProvider == "fake_vieneu_tts_vi" {
		t.Fatalf("CRITICAL INVARIANT VIOLATION: excluded provider was executed!")
	}
	if executedProvider != "fake_cloud_tts_consent" {
		t.Errorf("expected fake_cloud_tts_consent, got %s", executedProvider)
	}
}

func TestSeam1_FallbackSelectionDecision_PersistsEffectivePolicyState(t *testing.T) {
	h := setupHarness(t)
	runID := setupRunAndPlan(t, h)

	// Override policy for fake_cloud_tts_consent from REQUIRES_EXPLICIT_CONSENT to ALLOWED via PUT /api/v1/policies
	overrideBody, _ := json.Marshal(map[string]string{
		"policy_state": "ALLOWED",
		"reason":       "operator policy override",
	})
	req, _ := http.NewRequest(http.MethodPut, h.server.URL+"/api/v1/policies/fake_cloud_tts_consent", bytes.NewReader(overrideBody))
	req.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT policy override failed: %v", err)
	}
	putResp.Body.Close()

	// First candidate fake_vieneu_tts_vi fails quality; fallback selects fake_cloud_tts_consent.
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		if p.ID() == "fake_vieneu_tts_vi" {
			return domain.ErrQualityRejected
		}
		return nil
	})

	execPayload := map[string]any{
		"run_id":          runID,
		"stage":           "tts",
		"language":        "vi",
		"consent_granted": false, // Consent is false, but PolicyService has ALLOWED
		"input_hash":      "hash_fallback_effective_policy",
		"max_retries":     1,
	}
	execBody, _ := json.Marshal(execPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(execBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute fallback failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK for fallback execution, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Query selection decisions via GET /api/v1/routing/decisions
	decResp, err := http.Get(fmt.Sprintf("%s/api/v1/routing/decisions?run_id=%s&stage=tts", h.server.URL, runID))
	if err != nil {
		t.Fatalf("GET /api/v1/routing/decisions failed: %v", err)
	}
	defer decResp.Body.Close()

	var decResult struct {
		Decisions []domain.SelectionDecision `json:"decisions"`
	}
	if err := json.NewDecoder(decResp.Body).Decode(&decResult); err != nil {
		t.Fatalf("decode decisions: %v", err)
	}

	if len(decResult.Decisions) < 2 {
		t.Fatalf("expected at least 2 decisions recorded, got %d", len(decResult.Decisions))
	}

	fallbackDec := decResult.Decisions[1]
	if fallbackDec.SelectedProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected fallback provider fake_cloud_tts_consent, got %s", fallbackDec.SelectedProviderID)
	}

	if fallbackDec.PolicyCheckResult != "ALLOWED" {
		t.Errorf("expected fallback PolicyCheckResult to be 'ALLOWED', got %s", fallbackDec.PolicyCheckResult)
	}
	if len(fallbackDec.CandidatesEvaluated) > 0 && fallbackDec.CandidatesEvaluated[0].PolicyState != "ALLOWED" {
		t.Errorf("expected candidate evaluation PolicyState to be 'ALLOWED', got %s", fallbackDec.CandidatesEvaluated[0].PolicyState)
	}
}

func TestSeam1_RoutingAttempts_POSTForbidden_GETReadOnly(t *testing.T) {
	h := setupHarness(t)

	// 1. POST /api/v1/routing/attempts (forgery vector) must NOT exist and must fail (404/405)
	forgeryPayload := map[string]any{
		"run_id":         uuid.NewString(),
		"stage":          "asr",
		"provider_id":    "forged_provider",
		"model_name":     "forged_model",
		"model_version":  "1.0.0",
		"input_hash":     "forged_hash",
		"attempt_number": 1,
		"status":         "succeeded",
	}
	body, _ := json.Marshal(forgeryPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/attempts", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/attempts request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("CRITICAL SECURITY VIOLATION: public POST /api/v1/routing/attempts forgery path is still accessible! got %d", resp.StatusCode)
	}

	// 2. GET /api/v1/routing/attempts (read-only audit history) must succeed (200 OK)
	getResp, err := http.Get(h.server.URL + "/api/v1/routing/attempts")
	if err != nil {
		t.Fatalf("GET /api/v1/routing/attempts failed: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for GET /api/v1/routing/attempts, got %d", getResp.StatusCode)
	}
}

func TestSeam1_LicenseManifests_SameVersionShadowingRejected(t *testing.T) {
	h := setupHarness(t)

	// 1. Initial registration for version 1.0.0 (201 Created)
	v1Payload := map[string]any{
		"dependency_name": "seam1_checkpoint_immutable",
		"version":         "1.0.0",
		"sha256":          "sha256_orig_seam1_123",
		"source_repo":     "github.com/monet88/models",
		"code_license":    "MIT",
		"model_license":   "Apache-2.0",
		"data_license":    "OpenData",
		"service_terms":   "Standard",
		"verified":        true,
	}
	v1Body, _ := json.Marshal(v1Payload)
	resp1, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(v1Body))
	if err != nil || resp1.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/licenses v1 failed: status=%d, err=%v", resp1.StatusCode, err)
	}
	resp1.Body.Close()

	// 2. Attempt same (dependency_name, version) with modified metadata (400 Bad Request, rejected)
	v1ShadowPayload := map[string]any{
		"dependency_name": "seam1_checkpoint_immutable",
		"version":         "1.0.0",
		"sha256":          "sha256_corrupted_shadow_456",
		"source_repo":     "github.com/monet88/models",
		"code_license":    "GPL-3.0",
		"model_license":   "Proprietary",
		"data_license":    "Restricted",
		"service_terms":   "NonCommercial",
		"verified":        false,
	}
	shadowBody, _ := json.Marshal(v1ShadowPayload)
	resp2, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(shadowBody))
	if err != nil {
		t.Fatalf("POST /api/v1/licenses shadow request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when attempting to shadow/overwrite existing manifest version, got %d", resp2.StatusCode)
	}
}

func TestSeam1_UnknownPolicy_NoDeclaredDefault_FailsClosed(t *testing.T) {
	h := setupHarness(t)

	// Register a new provider with undeclared policy
	pUnknown := provider.NewFakeTTSProvider("fake_seam1_undeclared_tts", 1600)
	pUnknown.Policy = "" // Undeclared policy
	_ = h.registry.Register(pUnknown)

	mName, mVer := pUnknown.ModelInfo()
	_ = h.srv.Handler() // Server handler initialized
	manifestPayload := map[string]any{
		"dependency_name": mName,
		"version":         mVer,
		"sha256":          "sha256_mock_" + mName,
		"source_repo":     "test/" + mName,
		"code_license":    "Apache-2.0",
		"model_license":   "Apache-2.0",
		"data_license":    "OpenData",
		"service_terms":   "Standard",
		"verified":        true,
	}
	manBody, _ := json.Marshal(manifestPayload)
	mResp, _ := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(manBody))
	mResp.Body.Close()

	// Exclude all known allowed providers so only the undeclared provider is considered
	decidePayload := map[string]any{
		"run_id":             setupRunAndPlan(t, h),
		"stage":              "tts",
		"language":           "vi",
		"excluded_providers": []string{"fake_vieneu_tts_vi", "fake_cloud_tts_consent", "fake_indextts2_blocked"},
	}
	decBody, _ := json.Marshal(decidePayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when provider has undeclared policy with no default, got %d", resp.StatusCode)
	}
}

func TestSeam1_SetPolicy_InvalidState_ClientError(t *testing.T) {
	h := setupHarness(t)

	// 1. Sending an invalid/non-canonical policy_state string must return 400 Bad Request
	invalidPayload := map[string]any{
		"policy_state": "INVALID_STATE",
		"reason":       "bad client input",
	}
	body1, _ := json.Marshal(invalidPayload)
	req1, _ := http.NewRequest(http.MethodPut, h.server.URL+"/api/v1/policies/fake_vieneu_tts_vi", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("PUT /api/v1/policies request failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid policy state, got %d", resp1.StatusCode)
	}

	// 2. Sending empty policy_state must return 400 Bad Request
	emptyPayload := map[string]any{
		"policy_state": "",
		"reason":       "empty policy",
	}
	body2, _ := json.Marshal(emptyPayload)
	req2, _ := http.NewRequest(http.MethodPut, h.server.URL+"/api/v1/policies/fake_vieneu_tts_vi", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("PUT /api/v1/policies empty request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty policy state, got %d", resp2.StatusCode)
	}
}

func TestSeam1_RegisterCredential_UnsupportedStorageType_ClientError(t *testing.T) {
	h := setupHarness(t)

	// 1. Attempting to register unsupported storage type (e.g. "aws_secrets_manager", "vault") must return 400 Bad Request
	unsupportedPayload := map[string]any{
		"name":         "unsupported_key_ref",
		"provider_id":  "fake_auth_cloud_asr",
		"storage_type": "aws_secrets_manager",
		"key_ref":      "arn:aws:secretsmanager:us-east-1:12345:secret:token",
	}
	body, _ := json.Marshal(unsupportedPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/credentials", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/credentials unsupported request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for unsupported storage_type, got %d", resp.StatusCode)
	}
}

func setupRunAndPlan(t *testing.T, h *testHarness) string {
	t.Helper()
	_, runID := createJobAndRun(t, h)
	ctx := context.Background()
	run, err := h.db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("failed to get run %q: %v", runID, err)
	}
	job, err := h.db.GetJob(ctx, run.JobID)
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}
	plan := domain.AudioRolePlan{
		ID:      uuid.NewString(),
		AssetID: job.SourceAssetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := h.db.SaveAudioRolePlan(ctx, plan); err != nil {
		t.Fatalf("failed to save audio role plan: %v", err)
	}
	return runID
}
