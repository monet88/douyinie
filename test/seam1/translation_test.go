package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

// runDubScript executes the dub script adaptation pipeline over the Seam 1 HTTP API.
func runDubScript(t *testing.T, h *testHarness, assetID string, req map[string]any) (*http.Response, *domain.DubScriptVariant) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal dub-script request: %v", err)
	}

	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/dub-script", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("dub-script HTTP request failed: %v", err)
	}

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var result struct {
			Variant domain.DubScriptVariant `json:"dub_script_variant"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode dub-script response: %v", err)
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

// TestSeam1_Translation_QAGate_Flags verifies that fact, name, number, and negation corruptions
// are flagged on the persisted variant and surfaced as pending review exceptions instead of
// failing the stage. The gate reports what is wrong; the operator corrects or accepts it.
func TestSeam1_Translation_QAGate_Flags(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	assertFlagged := func(t *testing.T, req map[string]any, wantReasons ...string) {
		t.Helper()
		resp, variant := runTranslation(t, h, assetID, req)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201 Created with a flagged variant, got %d body=%s", resp.StatusCode, string(raw))
		}
		if variant == nil || len(variant.Segments) != 1 {
			t.Fatalf("expected the flagged candidate to be persisted, got %#v", variant)
		}
		if variant.Segments[0].PassedQAGate {
			t.Fatalf("expected the corrupted segment to stay flagged, got passed_qa_gate=true")
		}
		reason := strings.ToLower(variant.Segments[0].ReviewReason)
		matched := false
		for _, want := range wantReasons {
			if strings.Contains(reason, strings.ToLower(want)) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("expected the review reason to name the violation (%v), got %q", wantReasons, variant.Segments[0].ReviewReason)
		}
		for _, item := range fetchRunReviewItems(t, h, runID, false) {
			if item.Type == domain.ReviewItemTypeTranslationQA {
				return
			}
		}
		t.Fatalf("expected a pending translation_qa exception for the flagged segment")
	}

	t.Run("Number corruption flagged", func(t *testing.T) {
		prov, _ := h.registry.Get("fake_llm_translator")
		fake := prov.(*provider.FakeTranslationProvider)
		fake.CorruptNumbers = true
		defer func() { fake.CorruptNumbers = false }()

		fbProv, _ := h.registry.Get("fake_local_translator_fallback")
		fbFake := fbProv.(*provider.FakeTranslationProvider)
		fbFake.CorruptNumbers = true
		defer func() { fbFake.CorruptNumbers = false }()

		assertFlagged(t, map[string]any{
			"run_id":          runID,
			"target_language": "vi",
			"segments": []domain.TranslationInputSegment{
				{Index: 0, SourceText: "步骤1：准备抹茶粉20克，不要加糖。", StartMs: 0, EndMs: 2000},
			},
		}, "number")
	})

	t.Run("Negation inversion flagged", func(t *testing.T) {
		prov, _ := h.registry.Get("fake_llm_translator")
		fake := prov.(*provider.FakeTranslationProvider)
		fake.CorruptNegation = true
		defer func() { fake.CorruptNegation = false }()

		fbProv, _ := h.registry.Get("fake_local_translator_fallback")
		fbFake := fbProv.(*provider.FakeTranslationProvider)
		fbFake.CorruptNegation = true
		defer func() { fbFake.CorruptNegation = false }()

		assertFlagged(t, map[string]any{
			"run_id":          runID,
			"target_language": "vi",
			"segments": []domain.TranslationInputSegment{
				// Deliberately free of semantic anchors, numbers and names so the
				// negation check is the one that reports.
				{Index: 0, SourceText: "不要忘记带伞。", StartMs: 0, EndMs: 2000},
			},
		}, "negation polarity inverted")
	})

	t.Run("Name corruption flagged", func(t *testing.T) {
		prov, _ := h.registry.Get("fake_llm_translator")
		fake := prov.(*provider.FakeTranslationProvider)
		fake.CorruptNames = true
		defer func() { fake.CorruptNames = false }()

		fbProv, _ := h.registry.Get("fake_local_translator_fallback")
		fbFake := fbProv.(*provider.FakeTranslationProvider)
		fbFake.CorruptNames = true
		defer func() { fbFake.CorruptNames = false }()

		assertFlagged(t, map[string]any{
			"run_id":          runID,
			"target_language": "vi",
			"segments": []domain.TranslationInputSegment{
				{Index: 0, SourceText: "SUPOR电饭煲是张伟推荐的。", StartMs: 0, EndMs: 2000},
			},
		}, "name/brand", "entity")
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

// TestSeam1_DubScript_DurationAdaptation_ShortenFirst verifies that DubScript adaptation
// produces an immutable, CAS-persisted DubScriptVariant where fast-cadence/tight slots
// are shortened first while preserving facts, names, numbers, and negation polarity.
func TestSeam1_DubScript_DurationAdaptation_ShortenFirst(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	// 1. First run meaning-first translation
	transReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	respTrans, transVariant := runTranslation(t, h, assetID, transReq)
	if respTrans.StatusCode != http.StatusCreated || transVariant == nil {
		t.Fatalf("meaning-first translation failed: status %d", respTrans.StatusCode)
	}

	// 2. Run DubScript adaptation over Seam 1 API
	dubReq := map[string]any{
		"run_id":                  runID,
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	}
	respDub, dubVariant := runDubScript(t, h, assetID, dubReq)
	if respDub.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from dub-script API, got %d", respDub.StatusCode)
	}
	if dubVariant == nil {
		t.Fatalf("expected non-nil dub script variant")
	}
	if dubVariant.TargetLanguage != "vi" {
		t.Errorf("expected target language vi, got %s", dubVariant.TargetLanguage)
	}
	if dubVariant.CASHash == "" {
		t.Errorf("expected non-empty CASHash for dub script variant")
	}
	for _, seg := range dubVariant.Segments {
		if !seg.PassedQAGate {
			t.Errorf("segment %d failed QA gate", seg.Index)
		}
		if seg.SpokenText == "" {
			t.Errorf("segment %d has empty spoken text", seg.Index)
		}
		if seg.EstimatedDurationMs <= 0 {
			t.Errorf("segment %d has non-positive estimated duration: %d", seg.Index, seg.EstimatedDurationMs)
		}
		// Source-relative invariants:
		//  - Duration overrun must route to review.
		//  - A fitting segment may still require review for cadence or QA reasons.
		//  - Cadence/target-rate metadata must always be populated and positive.
		if seg.EstimatedDurationMs > seg.SlotDurationMs && !seg.RequiresReview {
			t.Errorf("segment %d overruns slot but was not flagged for review", seg.Index)
		}
		if seg.EstimatedDurationMs <= seg.SlotDurationMs && seg.ReviewReason == "DURATION_OVERRUN" {
			t.Errorf("segment %d fits slot but was incorrectly marked DURATION_OVERRUN", seg.Index)
		}
		if seg.TargetSpeakingRateCPS <= 0 {
			t.Errorf("segment %d expected positive target speaking rate, got %f", seg.Index, seg.TargetSpeakingRateCPS)
		}
		if seg.CadenceRatio <= 0 {
			t.Errorf("segment %d expected positive cadence ratio, got %f", seg.Index, seg.CadenceRatio)
		}
		if seg.TargetWordBudget < 0 {
			t.Errorf("segment %d expected non-negative target word budget, got %d", seg.Index, seg.TargetWordBudget)
		}
	}

	// 3. Verify GET /api/v1/assets/{id}/dub-script-variant?target_language=vi
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/dub-script-variant?target_language=vi")
	if err != nil {
		t.Fatalf("GET dub-script-variant failed: %v", err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET dub-script-variant, got %d", getResp.StatusCode)
	}
	var getResult struct {
		Variant domain.DubScriptVariant `json:"dub_script_variant"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&getResult)
	if getResult.Variant.CASHash != dubVariant.CASHash {
		t.Errorf("expected matching CASHash from GET, got %s vs %s", getResult.Variant.CASHash, dubVariant.CASHash)
	}
}

// TestSeam1_DubScript_DeterministicCAS_Idempotency verifies that re-running DubScript
// adaptation returns the cached variant without duplicate CAS writes.
func TestSeam1_DubScript_DeterministicCAS_Idempotency(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	transReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	_, transVariant := runTranslation(t, h, assetID, transReq)
	if transVariant == nil {
		t.Fatalf("translation failed")
	}

	dubReq := map[string]any{
		"run_id":                  runID,
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	}

	resp1, dub1 := runDubScript(t, h, assetID, dubReq)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first dub-script failed: %d", resp1.StatusCode)
	}

	resp2, dub2 := runDubScript(t, h, assetID, dubReq)
	if resp2.StatusCode != http.StatusCreated && resp2.StatusCode != http.StatusOK {
		t.Fatalf("second dub-script failed: %d", resp2.StatusCode)
	}

	if dub1.ID != dub2.ID {
		t.Errorf("expected identical cached variant ID, got %s vs %s", dub1.ID, dub2.ID)
	}
	if dub1.CASHash != dub2.CASHash {
		t.Errorf("expected identical CASHash, got %s vs %s", dub1.CASHash, dub2.CASHash)
	}
}

// TestSeam1_DubScript_CASOwnership_MismatchRejection verifies that supplying a TranslationVariant
// from a different asset or mismatched target language is rejected by the API.
func TestSeam1_DubScript_CASOwnership_MismatchRejection(t *testing.T) {
	h := setupHarness(t)
	asset1, run1 := setupSpeechUnderstoodAsset(t, h)

	// The mismatched destination only needs to exist: the public dub-script
	// endpoint validates TranslationVariant ownership before any source-derived
	// work is consumed. Keep this fixture minimal instead of duplicating the
	// full speech-understanding setup.
	const (
		asset2 = "asset-mismatch-second"
		raID   = "asset-mismatch-rights"
	)
	if err := h.db.CreateRightsAttestation(t.Context(), domain.RightsAttestation{
		ID:              raID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create mismatch rights attestation: %v", err)
	}
	if err := h.db.CreateSourceAsset(t.Context(), domain.SourceAsset{
		ID:                  asset2,
		SHA256:              "asset-mismatch-second-sha",
		ByteSize:            1,
		MimeType:            "video/mp4",
		OriginalFilename:    "asset-mismatch-second.mp4",
		RightsAttestationID: raID,
		CASPath:             "unused-for-ownership-check",
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create mismatch source asset: %v", err)
	}

	// Translate for asset 1 in VI
	transReq := map[string]any{
		"run_id":          run1,
		"target_language": "vi",
	}
	_, transVariant1 := runTranslation(t, h, asset1, transReq)
	if transVariant1 == nil {
		t.Fatalf("translation failed")
	}

	// 1. Submit Asset 1's translation CAS to Asset 2's dub-script endpoint -> must reject
	mismatchedAssetReq := map[string]any{
		"run_id":                  run1,
		"target_language":         "vi",
		"translation_variant_cas": transVariant1.CASHash,
	}
	respAssetMismatch, _ := runDubScript(t, h, asset2, mismatchedAssetReq)
	if respAssetMismatch.StatusCode == http.StatusOK || respAssetMismatch.StatusCode == http.StatusCreated {
		t.Errorf("expected rejection for asset_id mismatch, got status %d", respAssetMismatch.StatusCode)
	}

	// 2. Submit VI translation CAS requesting EN dub-script -> must reject
	mismatchedLangReq := map[string]any{
		"run_id":                  run1,
		"target_language":         "en",
		"translation_variant_cas": transVariant1.CASHash,
	}
	respLangMismatch, _ := runDubScript(t, h, asset1, mismatchedLangReq)
	if respLangMismatch.StatusCode == http.StatusOK || respLangMismatch.StatusCode == http.StatusCreated {
		t.Errorf("expected rejection for target_language mismatch, got status %d", respLangMismatch.StatusCode)
	}
}
