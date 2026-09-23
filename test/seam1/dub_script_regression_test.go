package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

func TestSeam1_DubScript_UnseenTextUsesGenericAdaptationAndPreservesSourceGap(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID
	// Save an audio role plan with narration/dialogue so dub script can adapt.
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp.Body.Close()
	if planResp.StatusCode != http.StatusCreated && planResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200/201 for audio role plan, got %d", planResp.StatusCode)
	}

	const (
		source1 = "请打开窗户并检查声音。"
		target1 = "Vui lòng hãy mở cửa sổ và kiểm tra âm thanh."
		source2 = "再见。"
		target2 = "Tạm biệt."
	)

	for _, id := range []string{"fake_llm_translator", "fake_local_translator_fallback"} {
		p, ok := h.registry.Get(id)
		if !ok {
			t.Fatalf("translation provider %s missing", id)
		}
		fake, ok := p.(*provider.FakeTranslationProvider)
		if !ok {
			t.Fatalf("provider %s has unexpected type %T", id, p)
		}
		fake.CustomTranslations[source1] = target1
		fake.CustomTranslations[source2] = target2
	}
	tObj1, _ := h.casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `"}`)))
	_ = h.db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj1.SHA256,
		CreatedAt:      time.Now().UTC(),
	})

	transReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{Index: 0, SourceText: source1, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2300},
			{Index: 1, SourceText: source2, SpeakerID: "SPEAKER_00", StartMs: 2420, EndMs: 3300},
		},
	}
	respTrans, transVariant := runTranslation(t, h, assetID, transReq)
	if respTrans.StatusCode != http.StatusCreated || transVariant == nil {
		t.Fatalf("unseen meaning-first translation failed: status %d", respTrans.StatusCode)
	}

	dubReq := map[string]any{
		"run_id":                  runID,
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	}
	respDub, dubVariant := runDubScript(t, h, assetID, dubReq)
	if respDub.StatusCode != http.StatusCreated || dubVariant == nil {
		t.Fatalf("unseen dub-script adaptation failed: status %d", respDub.StatusCode)
	}
	if len(dubVariant.Segments) != 2 {
		t.Fatalf("expected 2 dub-script segments, got %d", len(dubVariant.Segments))
	}

	seg := dubVariant.Segments[0]
	if !seg.IsShortened {
		t.Fatal("expected unseen target text to be shortened by generic spoken adaptation")
	}
	if len(seg.SpokenText) >= len(seg.MeaningText) {
		t.Fatalf("expected shortened spoken text, got %q from %q", seg.SpokenText, seg.MeaningText)
	}
	if seg.SourceGapAfterMs != 120 {
		t.Fatalf("expected 120ms source gap after first turn, got %dms", seg.SourceGapAfterMs)
	}
	if seg.NaturalGapMs < seg.SourceGapAfterMs {
		t.Fatalf("predicted pause %dms must preserve source gap %dms", seg.NaturalGapMs, seg.SourceGapAfterMs)
	}
	if seg.CadenceRatio <= 0 {
		t.Fatalf("expected positive source-relative cadence ratio, got %.3f", seg.CadenceRatio)
	}
	if seg.RequiresReview {
		t.Fatalf("unseen generic adaptation should fit without review, got %s", seg.ReviewReason)
	}
}

func TestSeam1_DubScript_FittingTextStillRoutesExtremeCadenceDeviationToReview(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID
	// Save an audio role plan with narration/dialogue so dub script can adapt.
	planPayload2 := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody2, _ := json.Marshal(planPayload2)
	planResp2, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(planBody2))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp2.Body.Close()
	if planResp2.StatusCode != http.StatusCreated && planResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200/201 for audio role plan, got %d", planResp2.StatusCode)
	}

	const (
		source = "现在马上快速打开窗户然后立刻继续操作完成所有步骤。"
		target = "Mở cửa sổ ngay."
	)
	for _, id := range []string{"fake_llm_translator", "fake_local_translator_fallback"} {
		p, ok := h.registry.Get(id)
		if !ok {
			t.Fatalf("translation provider %s missing", id)
		}
		fake, ok := p.(*provider.FakeTranslationProvider)
		if !ok {
			t.Fatalf("provider %s has unexpected type %T", id, p)
		}
		fake.CustomTranslations[source] = target
	}
	tObj2, _ := h.casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `"}`)))
	_ = h.db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj2.SHA256,
		CreatedAt:      time.Now().UTC(),
	})

	transReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{
			{Index: 0, SourceText: source, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000},
		},
	}
	respTrans, transVariant := runTranslation(t, h, assetID, transReq)
	if respTrans.StatusCode != http.StatusCreated || transVariant == nil {
		t.Fatalf("cadence translation failed: status %d", respTrans.StatusCode)
	}

	respDub, dubVariant := runDubScript(t, h, assetID, map[string]any{
		"run_id":                  runID,
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	})
	if respDub.StatusCode != http.StatusCreated || dubVariant == nil {
		t.Fatalf("cadence dub-script adaptation failed: status %d", respDub.StatusCode)
	}
	seg := dubVariant.Segments[0]
	if seg.EstimatedDurationMs > seg.SlotDurationMs {
		t.Fatalf("fixture must fit duration so review is cadence-only: %dms > %dms", seg.EstimatedDurationMs, seg.SlotDurationMs)
	}
	if !seg.RequiresReview {
		t.Fatal("extreme source-relative cadence deviation must route to review even when duration fits")
	}
	if seg.ReviewReason != "CADENCE_TOO_SLOW" {
		t.Fatalf("expected CADENCE_TOO_SLOW, got %q", seg.ReviewReason)
	}
	if !dubVariant.RequiresReview {
		t.Fatal("variant must carry review requirement from cadence-deviant segment")
	}
}
