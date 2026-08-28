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
