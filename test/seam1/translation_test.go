package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

// runTranslation executes the translation pipeline over the Seam 1 HTTP API.
func runTranslation(t *testing.T, h *testHarness, assetID string, req map[string]any) (*http.Response, *domain.TranslationVariant) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal translate request: %v", err)
	}

	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("translate HTTP request failed: %v", err)
	}

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var result struct {
			Variant domain.TranslationVariant `json:"translation_variant"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode translation response: %v", err)
		}
		return resp, &result.Variant
	}

	return resp, nil
}

func setupSpeechUnderstoodAsset(t *testing.T, h *testHarness) (string, string) {
	t.Helper()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Save an audio role plan with narration/dialogue so speech understanding runs.
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp.Body.Close()
	if planResp.StatusCode != http.StatusCreated && planResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200/201 for audio role plan, got %d", planResp.StatusCode)
	}

	// Run speech understanding (ASR -> forced alignment -> conditional diarization -> canonical SpeechBlocks)
	artifact := runSpeechUnderstand(t, h, runID, assetID)
	if artifact == nil || len(artifact.SpeechBlocks) == 0 {
		t.Fatalf("setupSpeechUnderstoodAsset: transcript artifact has 0 speech blocks")
	}

	return assetID, runID
}

// TestSeam1_Translation_HappyPath_VI_and_EN validates that translation from source SpeechBlocks
// into both Vietnamese (VI) and English (EN) produces immutable, CAS-persisted TranslationVariants.
func TestSeam1_Translation_HappyPath_VI_and_EN(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	// 1. Translate to Vietnamese (VI)
	viReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	respVI, variantVI := runTranslation(t, h, assetID, viReq)
	if respVI.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for VI translation, got %d", respVI.StatusCode)
	}
	if variantVI == nil {
		t.Fatalf("expected non-nil translation variant for VI")
	}
	if variantVI.TargetLanguage != "vi" {
		t.Errorf("expected target language 'vi', got '%s'", variantVI.TargetLanguage)
	}
	if len(variantVI.Segments) == 0 {
		t.Fatalf("expected translated segments for VI, got 0")
	}
	if variantVI.CASHash == "" {
		t.Errorf("expected non-empty CASHash for VI variant")
	}
	if variantVI.OverallQAScore < 0.9 {
		t.Errorf("expected overall QA score >= 0.9, got %f", variantVI.OverallQAScore)
	}
	for _, seg := range variantVI.Segments {
		if !seg.PassedQAGate {
			t.Errorf("segment %d failed QA gate", seg.Index)
		}
		if seg.TargetText == "" {
			t.Errorf("segment %d has empty target text", seg.Index)
		}
	}

	// Verify GET /api/v1/assets/{id}/translation-variant?target_language=vi
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/translation-variant?target_language=vi")
	if err != nil {
		t.Fatalf("GET translation-variant failed: %v", err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET translation-variant, got %d", getResp.StatusCode)
	}
	var getResult struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&getResult)
	if getResult.Variant.CASHash != variantVI.CASHash {
		t.Errorf("expected matching CASHash from GET, got %s vs %s", getResult.Variant.CASHash, variantVI.CASHash)
	}

	// 2. Translate to English (EN)
	enReq := map[string]any{
		"run_id":          runID,
		"target_language": "en",
	}
	respEN, variantEN := runTranslation(t, h, assetID, enReq)
	if respEN.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for EN translation, got %d", respEN.StatusCode)
	}
	if variantEN == nil {
		t.Fatalf("expected non-nil translation variant for EN")
	}
	if variantEN.TargetLanguage != "en" {
		t.Errorf("expected target language 'en', got '%s'", variantEN.TargetLanguage)
	}
	if variantEN.CASHash == "" {
		t.Errorf("expected non-empty CASHash for EN variant")
	}
}

// TestSeam1_Translation_MeaningFirst_Preservation verifies that facts, proper names,
// numerical quantities, and negation polarity survive translation into target languages.
func TestSeam1_Translation_MeaningFirst_Preservation(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	// Explicit segments testing numbers (25), proper names (张伟), and negation (不要)
	req := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{
				Index:      0,
				SourceText: "请将温度调至25度，张伟说不要打开窗户。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    0,
				EndMs:      3000,
			},
			{
				Index:      1,
				SourceText: "SUPOR电饭煲拥有3升容量，煮饭不粘锅。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    3100,
				EndMs:      6000,
			},
		},
	}

	resp, variant := runTranslation(t, h, assetID, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}

	if len(variant.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(variant.Segments))
	}

	// Segment 0 checks
	seg0 := variant.Segments[0]
	if !seg0.PassedQAGate {
		t.Errorf("segment 0 failed meaning preservation QA gate")
	}
	if !seg0.NegationPolarity {
		t.Errorf("expected negation polarity true for segment 0 ('不要')")
	}
	if !strings.Contains(seg0.TargetText, "25") {
		t.Errorf("expected number 25 preserved in target text, got: %s", seg0.TargetText)
	}
	if !strings.Contains(seg0.TargetText, "Trương Vĩ") {
		t.Errorf("expected name 'Trương Vĩ' preserved in target text, got: %s", seg0.TargetText)
	}

	// Segment 1 checks
	seg1 := variant.Segments[1]
	if !seg1.PassedQAGate {
		t.Errorf("segment 1 failed meaning preservation QA gate")
	}
	if !strings.Contains(seg1.TargetText, "SUPOR") {
		t.Errorf("expected brand SUPOR preserved in target text, got: %s", seg1.TargetText)
	}
	if !strings.Contains(seg1.TargetText, "3") {
		t.Errorf("expected number 3 preserved in target text, got: %s", seg1.TargetText)
	}
}

// TestSeam1_Translation_QAGate_Rejections verifies that the Translation QA Gate
// rejects fact, name, number, and negation corruptions with HTTP 422 Unprocessable Entity.
func TestSeam1_Translation_QAGate_Rejections(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	t.Run("Number corruption rejected", func(t *testing.T) {
		// Set fake providers to corrupt numbers
		prov, _ := h.registry.Get("fake_llm_translator")
		fake := prov.(*provider.FakeTranslationProvider)
		fake.CorruptNumbers = true
		defer func() { fake.CorruptNumbers = false }()

		fbProv, _ := h.registry.Get("fake_local_translator_fallback")
		fbFake := fbProv.(*provider.FakeTranslationProvider)
		fbFake.CorruptNumbers = true
		defer func() { fbFake.CorruptNumbers = false }()

		req := map[string]any{
			"run_id":          runID,
			"target_language": "vi",
			"segments": []domain.TranslationInputSegment{
				{Index: 0, SourceText: "步骤1：准备抹茶粉20克，不要加糖。", StartMs: 0, EndMs: 2000},
			},
		}

		resp, _ := runTranslation(t, h, assetID, req)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 Unprocessable Entity on number corruption, got %d", resp.StatusCode)
		}
	})

	t.Run("Negation inversion rejected", func(t *testing.T) {
		prov, _ := h.registry.Get("fake_llm_translator")
		fake := prov.(*provider.FakeTranslationProvider)
		fake.CorruptNegation = true
		defer func() { fake.CorruptNegation = false }()

		fbProv, _ := h.registry.Get("fake_local_translator_fallback")
		fbFake := fbProv.(*provider.FakeTranslationProvider)
		fbFake.CorruptNegation = true
		defer func() { fbFake.CorruptNegation = false }()

		req := map[string]any{
			"run_id":          runID,
			"target_language": "vi",
			"segments": []domain.TranslationInputSegment{
				{Index: 0, SourceText: "请不要打开窗户。", StartMs: 0, EndMs: 2000},
			},
		}

		resp, _ := runTranslation(t, h, assetID, req)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 Unprocessable Entity on negation inversion, got %d", resp.StatusCode)
		}
	})

	t.Run("Name corruption rejected", func(t *testing.T) {
		prov, _ := h.registry.Get("fake_llm_translator")
		fake := prov.(*provider.FakeTranslationProvider)
		fake.CorruptNames = true
		defer func() { fake.CorruptNames = false }()

		fbProv, _ := h.registry.Get("fake_local_translator_fallback")
		fbFake := fbProv.(*provider.FakeTranslationProvider)
		fbFake.CorruptNames = true
		defer func() { fbFake.CorruptNames = false }()

		req := map[string]any{
			"run_id":          runID,
			"target_language": "vi",
			"segments": []domain.TranslationInputSegment{
				{Index: 0, SourceText: "SUPOR电饭煲是张伟推荐的。", StartMs: 0, EndMs: 2000},
			},
		}

		resp, _ := runTranslation(t, h, assetID, req)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 Unprocessable Entity on name corruption, got %d", resp.StatusCode)
		}
	})
}

// TestSeam1_Translation_PolicyFallbackRouting verifies that when the primary translation
// provider fails, routing automatically falls back to an allowed secondary provider with
// complete attempt and selection provenance recorded in SQLite.
func TestSeam1_Translation_PolicyFallbackRouting(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	// Inject error in primary translation provider
	primaryProv, _ := h.registry.Get("fake_llm_translator")
	primaryFake := primaryProv.(*provider.FakeTranslationProvider)
	primaryFake.InjectError = fmt.Errorf("simulated upstream rate limit")
	defer func() { primaryFake.InjectError = nil }()

	req := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}

	resp, variant := runTranslation(t, h, assetID, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from fallback translation, got %d", resp.StatusCode)
	}

	if variant.ProviderID != "fake_local_translator_fallback" {
		t.Errorf("expected fallback provider fake_local_translator_fallback, got %s", variant.ProviderID)
	}

	// Verify attempt provenance in SQLite
	attempts, err := h.db.ListProviderAttempts(context.Background(), runID, "translation")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 attempts in provenance, got %d", len(attempts))
	}
	if attempts[0].Status != "failed" || attempts[1].Status != "succeeded" {
		t.Errorf("expected attempt 1 'failed' and attempt 2 'succeeded', got %s and %s", attempts[0].Status, attempts[1].Status)
	}
}

// TestSeam1_Translation_DeterministicCAS_Idempotency verifies that re-running translation
// on identical input returns the cached TranslationVariant without duplicate writes.
func TestSeam1_Translation_DeterministicCAS_Idempotency(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	req := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}

	resp1, variant1 := runTranslation(t, h, assetID, req)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first translation failed: %d", resp1.StatusCode)
	}

	resp2, variant2 := runTranslation(t, h, assetID, req)
	if resp2.StatusCode != http.StatusCreated && resp2.StatusCode != http.StatusOK {
		t.Fatalf("second translation failed: %d", resp2.StatusCode)
	}

	if variant1.ID != variant2.ID {
		t.Errorf("expected identical cached variant ID, got %s vs %s", variant1.ID, variant2.ID)
	}
	if variant1.CASHash != variant2.CASHash {
		t.Errorf("expected identical CASHash, got %s vs %s", variant1.CASHash, variant2.CASHash)
	}
}
