package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
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

// ---------------------------------------------------------------------------
// Issue #32 T12: End-to-end No-dub visual vertical slice (Video-1 style fixture)
// ---------------------------------------------------------------------------

// TestSeam1_NoDub_EndToEnd_Video1StyleFixture verifies:
// 1. No-speech video routes through valid no-dub AudioRolePlan contract.
// 2. Dubbing/voice-selection/TTS branch skipped completely; zero TTS injected; operator-facing "No dubbing required".
// 3. Visual text localization (compact overlay + subtitles) continues.
// 4. Soundtrack preserved (bitstream-exact where the plan allows passthrough).
// 5. Frozen RenderPlan -> preview -> final with exact semantic parity.
func TestSeam1_NoDub_EndToEnd_Video1StyleFixture(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. AudioRolePlan with 0 dub-eligible dialogue segments (Instrumental BGM + Ambience SFX)
	rolePlanPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 4000, EndMs: 8000, Role: domain.AudioRoleAmbienceSFX},
		},
	}
	roleBody, _ := json.Marshal(rolePlanPayload)
	savePlanResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	defer savePlanResp.Body.Close()
	if savePlanResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for audio role plan, got %d", savePlanResp.StatusCode)
	}

	// 2. Track executor calls to prove ZERO TTS / dub executor calls occur
	var mu sync.Mutex
	var executedStages []string
	h.SetExecutor(func(ctx context.Context, p provider.Provider, attemptNumber int) error {
		mu.Lock()
		executedStages = append(executedStages, string(p.Type()))
		mu.Unlock()
		return nil
	})

	// 3. Speech understanding: POST /api/v1/assets/{id}/speech-understand -> 422 with "no dub-eligible speech"
	speechPayload, _ := json.Marshal(map[string]any{"run_id": runID})
	speechResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/speech-understand", h.server.URL, assetID), "application/json", bytes.NewReader(speechPayload))
	if err != nil {
		t.Fatalf("speech understand failed: %v", err)
	}
	defer speechResp.Body.Close()
	if speechResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for speech understand on no-dub video, got %d", speechResp.StatusCode)
	}
	var speechErrRes struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(speechResp.Body).Decode(&speechErrRes)
	if !strings.Contains(speechErrRes.Error, "no dub-eligible speech") {
		t.Errorf("expected error mentioning no dub-eligible speech, got %q", speechErrRes.Error)
	}

	// 4. Voice Audition: POST /api/v1/assets/{id}/voice-audition -> 422 with "No dubbing required"
	auditionPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"voice": domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_f1",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
	}
	audBody, _ := json.Marshal(auditionPayload)
	audResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/voice-audition", h.server.URL, assetID), "application/json", bytes.NewReader(audBody))
	if err != nil {
		t.Fatalf("voice audition failed: %v", err)
	}
	defer audResp.Body.Close()
	if audResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for voice audition on no-dub video, got %d", audResp.StatusCode)
	}
	var audErrRes struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(audResp.Body).Decode(&audErrRes)
	if audErrRes.Error != "No dubbing required" {
		t.Errorf("expected 'No dubbing required', got %q", audErrRes.Error)
	}

	// 5. Voice Assignment: POST /api/v1/assets/{id}/voice-assignment -> 422 with "No dubbing required"
	assignPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	assignBody, _ := json.Marshal(assignPayload)
	assignResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/voice-assignment", h.server.URL, assetID), "application/json", bytes.NewReader(assignBody))
	if err != nil {
		t.Fatalf("voice assignment failed: %v", err)
	}
	defer assignResp.Body.Close()
	if assignResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for voice assignment on no-dub video, got %d", assignResp.StatusCode)
	}
	var assignErrRes struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(assignResp.Body).Decode(&assignErrRes)
	if assignErrRes.Error != "No dubbing required" {
		t.Errorf("expected 'No dubbing required', got %q", assignErrRes.Error)
	}

	// 6. Router-based Dub Execution (TTS & dialogue adaptation) -> BYPASS decision with zero executor calls
	ttsExecPayload := map[string]any{
		"run_id":      runID,
		"stage":       "tts",
		"language":    "vi",
		"input_hash":  "no_dub_tts_input_hash",
		"max_retries": 1,
	}
	ttsExecBody, _ := json.Marshal(ttsExecPayload)
	ttsExecResp, err := http.Post(fmt.Sprintf("%s/api/v1/routing/execute", h.server.URL), "application/json", bytes.NewReader(ttsExecBody))
	if err != nil {
		t.Fatalf("tts execute failed: %v", err)
	}
	defer ttsExecResp.Body.Close()
	if ttsExecResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for bypassed tts execution, got %d", ttsExecResp.StatusCode)
	}

	decisions := getRoutingDecisions(t, h, runID, "tts")
	if len(decisions) == 0 || decisions[0].PolicyCheckResult != "BYPASS" {
		t.Errorf("expected BYPASS routing decision for TTS stage on no-dub video, got %+v", decisions)
	}

	// Verify ZERO TTS executor calls were made
	mu.Lock()
	for _, st := range executedStages {
		if st == "tts" {
			t.Errorf("TTS executor was called unexpectedly during no-dub execution")
		}
	}
	mu.Unlock()

	// 7. Visual Text Localization: Detect text (T09) & Localize visual track (T10)
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID, "frame_sample_step_ms": 500})
	detectResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
	if err != nil {
		t.Fatalf("detect-text failed: %v", err)
	}
	defer detectResp.Body.Close()
	if detectResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from detect-text, got %d", detectResp.StatusCode)
	}

	visPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	visBody, _ := json.Marshal(visPayload)
	visResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(visBody))
	if err != nil {
		t.Fatalf("visual-track localization failed: %v", err)
	}
	defer visResp.Body.Close()
	if visResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from visual-track, got %d", visResp.StatusCode)
	}

	var visRes struct {
		Track domain.LocalizedVisualTrack `json:"localized_visual_track"`
	}
	if err := json.NewDecoder(visResp.Body).Decode(&visRes); err != nil {
		t.Fatalf("decode visual-track response failed: %v", err)
	}
	if visRes.Track.CASHash == "" || visRes.Track.SubtitleTrackCAS == "" {
		t.Errorf("expected valid CAS hashes for localized visual track and subtitle track")
	}
	if len(visRes.Track.Overlays) == 0 {
		t.Errorf("expected localized visual overlays in no-dub visual vertical slice")
	}

	// 8. Soundtrack Preservation / Audio Mix (T15): zero-speech clean passthrough
	mixPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	mixBody, _ := json.Marshal(mixPayload)
	mixResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-mix", h.server.URL, assetID), "application/json", bytes.NewReader(mixBody))
	if err != nil {
		t.Fatalf("audio mix failed: %v", err)
	}
	defer mixResp.Body.Close()
	if mixResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from audio-mix, got %d", mixResp.StatusCode)
	}

	var mixRes struct {
		DubMix domain.DubMixArtifact `json:"dub_mix"`
	}
	if err := json.NewDecoder(mixResp.Body).Decode(&mixRes); err != nil {
		t.Fatalf("decode audio-mix response failed: %v", err)
	}
	dubMix := mixRes.DubMix
	if dubMix.OverallStatus != "PASS" {
		t.Errorf("expected PASS dub mix status, got %s", dubMix.OverallStatus)
	}
	if dubMix.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed=false on no-dub video")
	}
	if !dubMix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved=true on no-dub video")
	}
	if dubMix.DubSegmentsCAS != "" {
		t.Errorf("expected empty DubSegmentsCAS (zero TTS injected), got %s", dubMix.DubSegmentsCAS)
	}
	if dubMix.AudioStemsCAS != "" {
		t.Errorf("expected empty AudioStemsCAS on no-dub passthrough, got %s", dubMix.AudioStemsCAS)
	}

	// Verify byte/hash identity of preserved audio against preflight source audio
	preflightResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/preflight", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get preflight report failed: %v", err)
	}
	defer preflightResp.Body.Close()
	if preflightResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for preflight report, got %d", preflightResp.StatusCode)
	}
	var pfRes struct {
		Preflight domain.PreflightReport `json:"preflight_report"`
	}
	_ = json.NewDecoder(preflightResp.Body).Decode(&pfRes)
	if dubMix.AudioCASHash != pfRes.Preflight.NormalizedAudioSHA256 {
		t.Errorf("expected bitstream-exact AudioCASHash %s, got %s", pfRes.Preflight.NormalizedAudioSHA256, dubMix.AudioCASHash)
	}
	if dubMix.AudioCASPath != pfRes.Preflight.NormalizedAudioCASPath {
		t.Errorf("expected AudioCASPath %s, got %s", pfRes.Preflight.NormalizedAudioCASPath, dubMix.AudioCASPath)
	}

	// 9. Freeze RenderPlan (T11): combines preserved soundtrack + actual LocalizedSubtitleTrack produced by T10
	freezeResp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   domain.TargetLanguageVI,
		"subtitle_plan_cas": visRes.Track.SubtitleTrackCAS,
	})
	defer freezeResp.Body.Close()
	if freezeResp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("freeze render plan failed: status %d", freezeResp.StatusCode)
	}
	if plan.DubMixCASHash != dubMix.CASHash {
		t.Errorf("expected RenderPlan to freeze exact DubMixCASHash: %s vs %s", plan.DubMixCASHash, dubMix.CASHash)
	}
	if plan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("expected RenderPlan to freeze exact AudioCASHash: %s vs %s", plan.AudioCASHash, dubMix.AudioCASHash)
	}
	if plan.SubtitlePlan.CASHash != visRes.Track.SubtitleTrackCAS {
		t.Errorf("expected RenderPlan to freeze exact SubtitleTrackCAS from T10: %s vs %s", plan.SubtitlePlan.CASHash, visRes.Track.SubtitleTrackCAS)
	}

	// 10. Render Preview & Final: verify preview -> final exact semantic parity
	prevResp, preview := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusCreated || preview == nil {
		t.Fatalf("preview render failed: status %d", prevResp.StatusCode)
	}

	finResp, final := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusCreated || final == nil {
		t.Fatalf("final render failed: status %d", finResp.StatusCode)
	}

	// Semantic Parity Assertions
	if preview.ConsumedPlan.PlanProvenanceHash != final.ConsumedPlan.PlanProvenanceHash {
		t.Errorf("parity violation: PlanProvenanceHash mismatch: preview=%s, final=%s",
			preview.ConsumedPlan.PlanProvenanceHash, final.ConsumedPlan.PlanProvenanceHash)
	}
	if preview.ConsumedPlan.PlanCASHash != final.ConsumedPlan.PlanCASHash {
		t.Errorf("parity violation: PlanCASHash mismatch: preview=%s, final=%s",
			preview.ConsumedPlan.PlanCASHash, final.ConsumedPlan.PlanCASHash)
	}
	if preview.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash || final.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("parity violation: AudioCASHash mismatch against preserved soundtrack: %s", dubMix.AudioCASHash)
	}
	if preview.ConsumedPlan.SubtitlePlan.CASHash != final.ConsumedPlan.SubtitlePlan.CASHash {
		t.Errorf("parity violation: SubtitlePlan CAS mismatch: preview=%s, final=%s",
			preview.ConsumedPlan.SubtitlePlan.CASHash, final.ConsumedPlan.SubtitlePlan.CASHash)
	}
}

// ---------------------------------------------------------------------------
// Issue #80: Automatic Production AudioRolePlan Generation
// ---------------------------------------------------------------------------

func TestSeam1_AudioRolePlan_DedicatedGenerationEndpoint(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Call dedicated POST /api/v1/assets/{id}/audio-role-plan/generate
	genResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan/generate", h.server.URL, assetID), "application/json", bytes.NewReader([]byte(`{"run_id":"`+runID+`"}`)))
	if err != nil {
		t.Fatalf("generate audio-role-plan request failed: %v", err)
	}
	defer genResp.Body.Close()
	if genResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from dedicated generation endpoint, got %d", genResp.StatusCode)
	}

	var res struct {
		Plan domain.AudioRolePlan `json:"audio_role_plan"`
	}
	if err := json.NewDecoder(genResp.Body).Decode(&res); err != nil {
		t.Fatalf("decode generated plan: %v", err)
	}
	if res.Plan.ID == "" || res.Plan.AssetID != assetID {
		t.Fatalf("invalid generated plan: %+v", res.Plan)
	}
	if len(res.Plan.Segments) == 0 {
		t.Fatalf("expected non-empty segments in generated plan")
	}

	// 2. Verify plan is persisted and retrievable via GET
	getResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get audio-role-plan failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET audio-role-plan, got %d", getResp.StatusCode)
	}

	// 3. Verify governance records (SelectionDecision and ProviderAttempt)
	decisions := getRoutingDecisions(t, h, runID, "audio_role_plan")
	if len(decisions) == 0 {
		t.Errorf("expected selection decisions for audio_role_plan stage")
	}

	attempts, err := h.db.ListProviderAttempts(context.Background(), runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) == 0 {
		t.Errorf("expected provider attempts for audio_role_plan stage")
	}
}

func TestSeam1_SpeechUnderstand_AutomaticPrerequisiteGeneration(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Verify no AudioRolePlan exists initially
	_, err := h.db.GetAudioRolePlan(context.Background(), assetID)
	if err == nil || !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected no AudioRolePlan initially, got err=%v", err)
	}

	// 1. Direct call to speech-understand WITHOUT calling any pre-generation endpoint!
	// Normal production media must automatically generate and persist the canonical prerequisite.
	speechBody, _ := json.Marshal(map[string]any{"run_id": runID})
	speechResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/speech-understand", h.server.URL, assetID), "application/json", bytes.NewReader(speechBody))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer speechResp.Body.Close()

	// Must NOT fail with ErrAudioRolePlanRequired (it generated the plan automatically!)
	if speechResp.StatusCode != http.StatusCreated && speechResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 201 Created or 422 NoDubEligible, got status %d", speechResp.StatusCode)
	}

	// 2. Verify AudioRolePlan was automatically generated and persisted in the database
	plan, err := h.db.GetAudioRolePlan(context.Background(), assetID)
	if err != nil {
		t.Fatalf("expected AudioRolePlan to be persisted after automatic prerequisite generation: %v", err)
	}
	if plan == nil || len(plan.Segments) == 0 {
		t.Fatalf("expected non-empty persisted AudioRolePlan, got %+v", plan)
	}

	// 3. Verify governance records for audio_role_plan were recorded during prerequisite generation
	decisions := getRoutingDecisions(t, h, runID, "audio_role_plan")
	if len(decisions) == 0 {
		t.Errorf("expected selection decision for automatically generated audio_role_plan")
	}
	attempts, err := h.db.ListProviderAttempts(context.Background(), runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) == 0 {
		t.Errorf("expected provider attempts for automatically generated audio_role_plan")
	}

	// 4. Verify SpeechService guard is NOT relaxed:
	// If audio role service is disabled/unconfigured, speech-understand MUST fail closed with 422 ErrAudioRolePlanRequired
	h.srv.SetAudioRoleService(nil)

	jobID2, runID2 := createJobAndRun(t, h)
	job2 := getJobViaAPI(t, h, jobID2)
	assetID2 := job2.SourceAssetID

	speechBody2, _ := json.Marshal(map[string]any{"run_id": runID2})
	speechResp2, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/speech-understand", h.server.URL, assetID2), "application/json", bytes.NewReader(speechBody2))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer speechResp2.Body.Close()
	if speechResp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected fail-closed 422 Unprocessable Entity when audio role service is nil, got %d", speechResp2.StatusCode)
	}
}

func TestSeam1_SpeechUnderstand_AudioRoleErrorClassification(t *testing.T) {
	h := setupHarness(t)

	// Case 1: AudioRoleService configured with NO analyzer and an empty router (no eligible provider)
	// -> Automatic prerequisite generation fails with 503 Service Unavailable
	audioMixSvc := service.NewAudioMixService(h.db, h.casStore)
	emptyReg := provider.NewRegistry()
	emptyRouter := provider.NewRouter(emptyReg, nil, nil, nil, nil, h.db)
	emptyAudioRole := service.NewAudioRoleService(h.db, h.casStore, audioMixSvc)
	emptyAudioRole.ConfigureRouter(emptyRouter)
	h.srv.SetAudioRoleService(emptyAudioRole)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	speechBody, err := json.Marshal(map[string]any{"run_id": runID})
	if err != nil {
		t.Fatalf("marshal speech body: %v", err)
	}
	speechResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/speech-understand", h.server.URL, assetID), "application/json", bytes.NewReader(speechBody))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer speechResp.Body.Close()
	bodyBytes, err := io.ReadAll(speechResp.Body)
	if err != nil {
		t.Fatalf("read speechResp body: %v", err)
	}
	if speechResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when audio role analyzer/provider is unavailable, got %d (body: %s)", speechResp.StatusCode, string(bodyBytes))
	}

	// Also verify dedicated generation endpoint returns 503
	genResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan/generate", h.server.URL, assetID), "application/json", bytes.NewReader(speechBody))
	if err != nil {
		t.Fatalf("dedicated generate request failed: %v", err)
	}
	defer genResp.Body.Close()
	if genResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable from dedicated generate endpoint, got %d", genResp.StatusCode)
	}

	// Case 2: Source asset has NO preflight report in DB (truthful unseeded asset)
	// -> Generation fails with 422 Unprocessable Entity
	attID := uuid.NewString()
	if err := h.db.CreateRightsAttestation(context.Background(), domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	unseededAsset := domain.SourceAsset{
		ID:                  "asset-unseeded-" + uuid.NewString()[:8],
		RightsAttestationID: attID,
		SHA256:              "sha256-unseeded-asset-evidence-missing",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	}
	if err := h.db.CreateSourceAsset(context.Background(), unseededAsset); err != nil {
		t.Fatalf("create unseeded source asset: %v", err)
	}
	speechBody2, err := json.Marshal(map[string]any{"run_id": uuid.NewString()})
	if err != nil {
		t.Fatalf("marshal speech body 2: %v", err)
	}
	resp2, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/speech-understand", h.server.URL, unseededAsset.ID), "application/json", bytes.NewReader(speechBody2))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity when preflight is missing, got %d", resp2.StatusCode)
	}

	// Also verify dedicated generation endpoint returns 422 for unseeded asset
	genResp2, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan/generate", h.server.URL, unseededAsset.ID), "application/json", bytes.NewReader(speechBody2))
	if err != nil {
		t.Fatalf("dedicated generate request failed: %v", err)
	}
	defer genResp2.Body.Close()
	if genResp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity from dedicated generate endpoint when preflight missing, got %d", genResp2.StatusCode)
	}
}
