package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

func getRoutingDecisions(t *testing.T, h *testHarness, runID, stage string) []domain.SelectionDecision {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/routing/decisions?run_id=%s&stage=%s", h.server.URL, runID, stage))
	if err != nil {
		t.Fatalf("get routing decisions failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for routing decisions, got %d", resp.StatusCode)
	}
	var result struct {
		Decisions []domain.SelectionDecision `json:"decisions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode routing decisions failed: %v", err)
	}
	return result.Decisions
}

func getJobViaAPI(t *testing.T, h *testHarness, jobID string) domain.LocalizationJob {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/jobs/%s", h.server.URL, jobID))
	if err != nil {
		t.Fatalf("get job failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for job, got %d", resp.StatusCode)
	}
	var result struct {
		Job domain.LocalizationJob `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode job failed: %v", err)
	}
	return result.Job
}

func TestSeam1_AudioRolePlan_DistinguishRoles(t *testing.T) {
	h := setupHarness(t)

	jobID, _ := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Create and save an AudioRolePlan distinguishing all four roles
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 1000, EndMs: 2000, Role: domain.AudioRoleSingingMusicVocal},
			{StartMs: 2000, EndMs: 3000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 3000, EndMs: 4000, Role: domain.AudioRoleAmbienceSFX},
		},
	}
	body, _ := json.Marshal(planPayload)
	saveResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save plan failed: %v", err)
	}
	defer saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 created, got %d", saveResp.StatusCode)
	}

	// Retrieve the plan and verify
	getResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get plan failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", getResp.StatusCode)
	}

	var getResult struct {
		Plan domain.AudioRolePlan `json:"audio_role_plan"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&getResult)

	if len(getResult.Plan.Segments) != 4 {
		t.Fatalf("expected 4 segments, got %d", len(getResult.Plan.Segments))
	}
	if getResult.Plan.Segments[0].Role != domain.AudioRoleNarrationDialogue ||
		getResult.Plan.Segments[1].Role != domain.AudioRoleSingingMusicVocal ||
		getResult.Plan.Segments[2].Role != domain.AudioRoleInstrumentalBgm ||
		getResult.Plan.Segments[3].Role != domain.AudioRoleAmbienceSFX {
		t.Errorf("unexpected segments: %+v", getResult.Plan.Segments)
	}
}

func TestSeam1_AudioRolePlan_SingingTriageOnly(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)

	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Plan contains ASR-positive singing
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleSingingMusicVocal},
		},
	}
	body, _ := json.Marshal(planPayload)
	saveResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save plan failed: %v", err)
	}
	saveResp.Body.Close()

	var mu sync.Mutex
	executorCalled := false
	// Injected executor mock
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		mu.Lock()
		executorCalled = true
		mu.Unlock()
		return nil
	})

	// Attempt executing a dub branch stage: tts
	execPayload := map[string]any{
		"run_id":      runID,
		"stage":       "tts",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	bodyExec, _ := json.Marshal(execPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(bodyExec))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for singing-only bypass, got %d", resp.StatusCode)
	}

	var execRes struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&execRes)
	if execRes.Status != "succeeded" {
		t.Errorf("expected status 'succeeded', got %q", execRes.Status)
	}

	// Verify the dub executor was NOT called
	mu.Lock()
	called := executorCalled
	mu.Unlock()
	if called {
		t.Errorf("expected dub executor NOT to be called for singing-only bypass")
	}

	// Verify the recorded SelectionDecision is BYPASS through the public Seam 1 API.
	decisions := getRoutingDecisions(t, h, runID, "tts")
	if len(decisions) != 1 {
		t.Errorf("expected 1 selection decision for tts, got %d", len(decisions))
	} else {
		if decisions[0].PolicyCheckResult != "BYPASS" {
			t.Errorf("expected PolicyCheckResult to be 'BYPASS', got %s", decisions[0].PolicyCheckResult)
		}
		if !strings.Contains(decisions[0].DecisionReason, "no-dub") {
			t.Errorf("expected decision reason to contain 'no-dub', got %s", decisions[0].DecisionReason)
		}
	}
}

func TestSeam1_AudioRolePlan_UncertainRoleReviewRequired(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)

	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Plan contains uncertain role segment
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleUncertain},
		},
	}
	body, _ := json.Marshal(planPayload)
	saveResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save plan failed: %v", err)
	}
	saveResp.Body.Close()

	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		return nil
	})

	// Execution should be rejected
	execPayload := map[string]any{
		"run_id":      runID,
		"stage":       "tts",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	bodyExec, _ := json.Marshal(execPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(bodyExec))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity, got %d", resp.StatusCode)
	}

	// Verify job status changed to review_required through the public Seam 1 API.
	jobAfter := getJobViaAPI(t, h, jobID)
	if jobAfter.Status != "review_required" {
		t.Errorf("expected job status 'review_required', got %s", jobAfter.Status)
	}
}

func TestSeam1_AudioRolePlan_NoDubRouteAllowedForRenderOnly(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)

	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Plan contains only BGM (no-dub eligible speech)
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	body, _ := json.Marshal(planPayload)
	saveResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save plan failed: %v", err)
	}
	saveResp.Body.Close()

	var mu sync.Mutex
	var executorCalls []string
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		mu.Lock()
		executorCalls = append(executorCalls, string(p.Type()))
		mu.Unlock()
		return nil
	})

	// 1. Dub stage execution (e.g. translation) should be bypassed
	execPayload1 := map[string]any{
		"run_id":            runID,
		"stage":             "translation",
		"language":          "vi",
		"input_hash":        "input_hash_123",
		"max_retries":       1,
		"required_features": []string{"shorten_first_adaptation"},
	}
	bodyExec1, _ := json.Marshal(execPayload1)
	resp1, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(bodyExec1))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for translation stage on no-dub route, got %d", resp1.StatusCode)
	}

	var execRes1 struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp1.Body).Decode(&execRes1)
	if execRes1.Status != "succeeded" {
		t.Errorf("expected status 'succeeded', got %q", execRes1.Status)
	}

	// Verify translation stage executor was NOT called
	mu.Lock()
	for _, call := range executorCalls {
		if call == "translation" {
			t.Errorf("expected translation stage executor NOT to be called, but it was")
		}
	}
	mu.Unlock()

	// Verify the recorded SelectionDecision is BYPASS for translation through the public Seam 1 API.
	decisions1 := getRoutingDecisions(t, h, runID, "translation")
	if len(decisions1) != 1 {
		t.Errorf("expected 1 selection decision for translation, got %d", len(decisions1))
	} else {
		if decisions1[0].PolicyCheckResult != "BYPASS" {
			t.Errorf("expected PolicyCheckResult to be 'BYPASS', got %s", decisions1[0].PolicyCheckResult)
		}
		if !strings.Contains(decisions1[0].DecisionReason, "no-dub") {
			t.Errorf("expected decision reason to contain 'no-dub', got %s", decisions1[0].DecisionReason)
		}
	}

	// 1b. Generic translation execution (visual localization - NO shorten_first_adaptation) should be ALLOWED
	execPayload1b := map[string]any{
		"run_id":      runID,
		"stage":       "translation",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	bodyExec1b, _ := json.Marshal(execPayload1b)
	resp1b, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(bodyExec1b))
	if err != nil {
		t.Fatalf("execute generic translation request failed: %v", err)
	}
	defer resp1b.Body.Close()

	if resp1b.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for generic translation on no-dub route, got %d", resp1b.StatusCode)
	}

	// Verify generic translation stage executor WAS called
	mu.Lock()
	foundGenericTranslation := false
	for _, call := range executorCalls {
		if call == "translation" {
			foundGenericTranslation = true
		}
	}
	mu.Unlock()
	if !foundGenericTranslation {
		t.Errorf("expected executor to be called for generic translation stage, but it wasn't")
	}

	// 2. Non-dub stage execution (e.g. ocr, render) should be ALLOWED
	execPayload2 := map[string]any{
		"run_id":      runID,
		"stage":       "ocr",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	bodyExec2, _ := json.Marshal(execPayload2)
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(bodyExec2))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for ocr stage on no-dub route, got %d", resp2.StatusCode)
	}

	// Verify ocr stage executor WAS called (continuation proof)
	mu.Lock()
	foundOcr := false
	for _, call := range executorCalls {
		if call == "ocr" {
			foundOcr = true
		}
	}
	mu.Unlock()
	if !foundOcr {
		t.Errorf("expected executor to be called for ocr stage, but it wasn't")
	}
}

func TestSeam1_AudioRolePlan_MixedNarrationAndSinging(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)

	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Plan contains BOTH narration and singing
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 1000, EndMs: 2000, Role: domain.AudioRoleSingingMusicVocal},
		},
	}
	body, _ := json.Marshal(planPayload)
	saveResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save plan failed: %v", err)
	}
	saveResp.Body.Close()

	executorCalled := false
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		executorCalled = true
		return nil
	})

	// Attempt executing a dub branch stage: tts. It should succeed (200 OK) and call provider!
	execPayload := map[string]any{
		"run_id":      runID,
		"stage":       "tts",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	bodyExec, _ := json.Marshal(execPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(bodyExec))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for mixed narration+singing, got %d", resp.StatusCode)
	}

	if !executorCalled {
		t.Errorf("expected provider executor to be called for narration dubbing")
	}
}

func TestSeam1_AudioRolePlan_ReplaceAndSaveAndValidation(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := createJobAndRun(t, h)

	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Try to save an invalid role value. It should fail with 400 Bad Request!
	invalidPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRole("invalid_role_value")},
		},
	}
	bodyInvalid, _ := json.Marshal(invalidPayload)
	respInvalid, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(bodyInvalid))
	if err != nil {
		t.Fatalf("save invalid plan failed: %v", err)
	}
	respInvalid.Body.Close()
	if respInvalid.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid role, got %d", respInvalid.StatusCode)
	}

	// 2. Save a valid plan first
	planPayload1 := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	body1, _ := json.Marshal(planPayload1)
	resp1, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("save plan 1 failed: %v", err)
	}
	resp1.Body.Close()

	// 3. Save a second valid plan for the SAME asset (repeated plan write)
	planPayload2 := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleAmbienceSFX},
		},
	}
	body2, _ := json.Marshal(planPayload2)
	resp2, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("save plan 2 failed: %v", err)
	}
	resp2.Body.Close()

	// 4. Retrieve the plan and verify that it has resolved to the second (current) plan
	getResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get plan failed: %v", err)
	}
	defer getResp.Body.Close()

	var getResult struct {
		Plan domain.AudioRolePlan `json:"audio_role_plan"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&getResult)

	if len(getResult.Plan.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(getResult.Plan.Segments))
	}
	if getResult.Plan.Segments[0].Role != domain.AudioRoleAmbienceSFX {
		t.Errorf("expected role to be updated to ambience/SFX, got %s", getResult.Plan.Segments[0].Role)
	}
}

func TestSeam1_AudioRolePlan_TTSFailsOnMissingOrUnknownRunID(t *testing.T) {
	h := setupHarness(t)
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		return nil
	})

	// Case 1: Missing runID
	execPayload1 := map[string]any{
		"run_id":      "",
		"stage":       "tts",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	body1, _ := json.Marshal(execPayload1)
	resp1, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body1))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(resp1.Body)
		t.Errorf("expected 422 Unprocessable Entity for missing run_id on TTS, got %d: %s", resp1.StatusCode, string(bodyBytes))
	}

	// Case 2: Unknown runID
	execPayload2 := map[string]any{
		"run_id":      "unknown_run_id_value",
		"stage":       "tts",
		"language":    "vi",
		"input_hash":  "input_hash_123",
		"max_retries": 1,
	}
	body2, _ := json.Marshal(execPayload2)
	resp2, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("execute request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(resp2.Body)
		t.Errorf("expected 422 Unprocessable Entity for unknown run_id on TTS, got %d: %s", resp2.StatusCode, string(bodyBytes))
	}
}
