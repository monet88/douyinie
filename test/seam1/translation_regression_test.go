package seam1_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

func TestSeam1_Translation_QAGateRejectsSemanticFactCorruption(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	primaryProv, ok := h.registry.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primary := primaryProv.(*provider.FakeTranslationProvider)
	primary.CustomTranslations["今天天气很好。"] = "Hôm nay thời tiết rất tệ."

	fbProv, ok := h.registry.Get("fake_local_translator_fallback")
	if !ok {
		t.Fatal("fake_local_translator_fallback not found")
	}
	fallback := fbProv.(*provider.FakeTranslationProvider)
	fallback.CustomTranslations["今天天气很好。"] = "Hôm nay thời tiết rất tệ."
	body, err := json.Marshal(map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1500},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for semantic fact corruption, got %d", resp.StatusCode)
	}
}

func TestSeam1_Translation_AcceptsCompoundChineseNumberEquivalent(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	primaryProv, ok := h.registry.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primary := primaryProv.(*provider.FakeTranslationProvider)
	primary.CustomTranslations["温度调到二十五度。"] = "Điều chỉnh nhiệt độ đến 25 độ."

	resp, variant := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{Index: 0, SourceText: "温度调到二十五度。", StartMs: 0, EndMs: 1500},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for equivalent 二十五 -> 25 translation, got %d", resp.StatusCode)
	}
	if variant == nil || len(variant.Segments) != 1 || !variant.Segments[0].PassedQAGate {
		t.Fatalf("expected one QA-passing translation segment, got %#v", variant)
	}
}

func TestSeam1_Translation_QAGate_FallbackToAlternateOnPrimaryCorruption(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	// Primary corrupts semantic facts -> rejected by QA gate
	primaryProv, ok := h.registry.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primary := primaryProv.(*provider.FakeTranslationProvider)
	primary.CustomTranslations["今天天气很好。"] = "Hôm nay thời tiết rất tệ."

	// Fallback provider produces valid translation
	fbProv, ok := h.registry.Get("fake_local_translator_fallback")
	if !ok {
		t.Fatal("fake_local_translator_fallback not found")
	}
	fallback := fbProv.(*provider.FakeTranslationProvider)
	delete(fallback.CustomTranslations, "今天天气很好。")
	fallback.CorruptNumbers = false
	fallback.CorruptNegation = false
	fallback.CorruptNames = false
	fallback.CorruptFacts = false

	body, err := json.Marshal(map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1500},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created on successful fallback, got %d", resp.StatusCode)
	}

	var res struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if res.Variant.ProviderID != "fake_local_translator_fallback" {
		t.Fatalf("expected fallback provider fake_local_translator_fallback, got %s", res.Variant.ProviderID)
	}

	// Verify provenance attempts in SQLite
	attempts, err := h.db.ListProviderAttempts(t.Context(), runID, "translation")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", len(attempts))
	}
	if attempts[0].ProviderID != "fake_llm_translator" || attempts[0].Status != "quality_failed" {
		t.Fatalf("expected attempt 0 to be fake_llm_translator quality_failed, got %+v", attempts[0])
	}
	if attempts[1].ProviderID != "fake_local_translator_fallback" || attempts[1].Status != "succeeded" {
		t.Fatalf("expected attempt 1 to be fake_local_translator_fallback succeeded, got %+v", attempts[1])
	}

	// Verify fallback SelectionDecision in SQLite
	decisions, err := h.db.ListSelectionDecisions(t.Context(), runID, "translation")
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected at least 2 selection decisions, got %d", len(decisions))
	}
	lastDecision := decisions[len(decisions)-1]
	if lastDecision.SelectedProviderID != "fake_local_translator_fallback" {
		t.Fatalf("expected last decision for fake_local_translator_fallback, got %s", lastDecision.SelectedProviderID)
	}
}

func TestSeam1_Translation_SmokeFailurePatternsAccepted(t *testing.T) {
	h := setupHarness(t)
	assetID, runID := setupSpeechUnderstoodAsset(t, h)

	primaryProv, ok := h.registry.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	primary := primaryProv.(*provider.FakeTranslationProvider)

	// Pattern 1: 告别 (farewell) -> affirmative Vietnamese
	src1 := "让你的视频告别平淡"
	tgt1 := "giúp video của bạn tạm biệt sự nhạt nhòa"
	primary.CustomTranslations[src1] = tgt1

	// Pattern 2: SOY lexical word -> Đậu nành
	src2 := "SOY"
	tgt2 := "Đậu nành"
	primary.CustomTranslations[src2] = tgt2

	// Pattern 3: 90kg, 100kg + indefinite article một -> natural Vietnamese
	src3 := "不管是九十公斤还是一百公斤根本就没有区别都只是令人讨厌的猪精罢了才瘦了十公斤而已"
	tgt3 := "dù là 90 kg hay 100 kg cũng không có gì khác biệt, đều chỉ là một con lợn tinh đáng ghét mà thôi, mới giảm có 10 kg mà thôi"
	primary.CustomTranslations[src3] = tgt3

	resp, variant := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{Index: 0, SourceText: src1, StartMs: 0, EndMs: 1500},
			{Index: 1, SourceText: src2, StartMs: 1500, EndMs: 2500},
			{Index: 2, SourceText: src3, StartMs: 2500, EndMs: 5000},
		},
	})

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected translation to succeed with HTTP 200/201, got %d", resp.StatusCode)
	}
	if variant == nil {
		t.Fatal("expected non-nil translation variant")
	}
	if len(variant.Segments) != 3 {
		t.Fatalf("expected 3 translated segments, got %d", len(variant.Segments))
	}
	for i, seg := range variant.Segments {
		if !seg.PassedQAGate {
			t.Errorf("segment %d expected PassedQAGate=true, got false", i)
		}
	}
}
