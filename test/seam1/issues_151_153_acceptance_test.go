package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

func seam1QueueLen(t *testing.T, h *testHarness) int {
	t.Helper()
	resp, err := http.Get(h.server.URL + "/api/v1/queue")
	if err != nil {
		t.Fatalf("GET queue: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET queue status=%d", resp.StatusCode)
	}
	var out struct {
		Queue []domain.QueueEntry `json:"queue"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	return len(out.Queue)
}

func TestSeam1_Issue151_GlossaryIngressAndCanonicalSpeechVariant(t *testing.T) {
	h := setupHarness(t)
	jobID, baselineRunID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID
	baselineQueueLen := seam1QueueLen(t, h)
	if _, err := h.db.GetTranslationVariantIndexByRun(context.Background(), baselineRunID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("baseline run unexpectedly has a translation artifact before direct invalid ingress: %v", err)
	}

	// Direct API invalid ingress must stop before provider routing and before any
	// translation mutation. Poison every fake translation provider: if routing is
	// reached, this request cannot return the expected ingress 400.
	var poisoned []*provider.FakeTranslationProvider
	for _, p := range h.registry.ListByType(provider.TypeTranslation) {
		if fake, ok := p.(*provider.FakeTranslationProvider); ok {
			fake.InjectError = errors.New("invalid glossary reached translation provider")
			poisoned = append(poisoned, fake)
		}
	}
	resp, invalidVariant := runTranslation(t, h, assetID, map[string]any{
		"run_id": baselineRunID, "job_id": jobID, "source_language": "en", "target_language": "vi",
		"segments": []domain.TranslationInputSegment{{Index: 0, SourceText: "OpenAI model", StartMs: 0, EndMs: 1000}},
		"glossary": []domain.GlossaryEntry{{Source: "OpenAI", Target: "A"}, {Source: " openai ", Target: "B"}},
	})
	for _, fake := range poisoned {
		fake.InjectError = nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || invalidVariant != nil {
		t.Fatalf("direct conflicting glossary must fail at ingress before provider routing: status=%d variant=%v", resp.StatusCode, invalidVariant)
	}
	if _, err := h.db.GetTranslationVariantIndexByRun(context.Background(), baselineRunID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("direct invalid glossary mutated translation state: %v", err)
	}

	postRun := func(config string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"config_snapshot_json": config})
		resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID), "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST run: %v", err)
		}
		return resp
	}

	conflicting := `{"glossary":[{"source":"OpenAI","target":"A"},{"source":" openai ","target":"B"}]}`
	resp = postRun(conflicting)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("conflicting glossary should return 400, got %d", resp.StatusCode)
	}
	if got := seam1QueueLen(t, h); got != baselineQueueLen {
		t.Fatalf("rejected glossary mutated run queue: before=%d after=%d", baselineQueueLen, got)
	}

	oversizedConfigBytes, _ := json.Marshal(map[string]any{
		"glossary": []map[string]string{{"source": "OpenAI", "target": strings.Repeat("x", 270000)}},
	})
	resp = postRun(string(oversizedConfigBytes))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized glossary should return 400, got %d", resp.StatusCode)
	}
	if got := seam1QueueLen(t, h); got != baselineQueueLen {
		t.Fatalf("oversized glossary mutated run queue: before=%d after=%d", baselineQueueLen, got)
	}

	validConfig := `{"glossary":[{"source":"OpenAI","target":"OPENAI_LOCK"}]}`
	resp = postRun(validConfig)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("valid glossary run status=%d", resp.StatusCode)
	}
	var runOut struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&runOut); err != nil {
		resp.Body.Close()
		t.Fatalf("decode run: %v", err)
	}
	resp.Body.Close()
	runID := runOut.Run.ID

	p, ok := h.registry.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator missing")
	}
	fake := p.(*provider.FakeTranslationProvider)
	fake.CustomTranslations["vi:OpenAI model"] = "OPENAI_LOCK mô hình"

	resp, canonical := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"source_language": "en",
		"target_language": "vi",
		"segments": []domain.TranslationInputSegment{{
			Index: 0, SourceText: "OpenAI model", StartMs: 1000, EndMs: 2000, SpeakerID: "SPEAKER_00",
		}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || canonical == nil {
		t.Fatalf("run glossary translation status=%d", resp.StatusCode)
	}
	if len(canonical.EffectiveGlossary.Entries) != 1 || canonical.EffectiveGlossary.Entries[0].Target != "OPENAI_LOCK" {
		t.Fatalf("run glossary did not reach translation: %+v", canonical.EffectiveGlossary)
	}
	if len(canonical.Segments) != 1 || !strings.Contains(canonical.Segments[0].TargetText, "OPENAI_LOCK") {
		t.Fatalf("glossary target not preserved by translation: %+v", canonical.Segments)
	}

	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
	if err != nil {
		t.Fatalf("POST detect-text: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST detect-text status=%d", resp.StatusCode)
	}

	visualBody, _ := json.Marshal(map[string]any{"run_id": runID, "job_id": jobID, "target_language": "vi"})
	resp, err = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/localized-visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(visualBody))
	if err != nil {
		t.Fatalf("POST localized-visual-track: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST localized-visual-track status=%d", resp.StatusCode)
	}

	getURL := fmt.Sprintf("%s/api/v1/assets/%s/translation-variant?target_language=vi&run_id=%s", h.server.URL, assetID, url.QueryEscape(runID))
	resp, err = http.Get(getURL)
	if err != nil {
		t.Fatalf("GET canonical translation: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET canonical translation status=%d", resp.StatusCode)
	}
	var got struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode canonical translation: %v", err)
	}
	if got.Variant.CASHash != canonical.CASHash {
		t.Fatalf("visual translation replaced canonical speech variant: before=%s after=%s", canonical.CASHash, got.Variant.CASHash)
	}
}

func TestSeam1_Issue153_AudioMixAcceptsBorrowedPlaybackWindowAndPinsCAS(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRunWithDuration(t, h, 7.0)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	resp, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("separate-stems status=%d", resp.StatusCode)
	}

	roleBody, _ := json.Marshal(map[string]any{"segments": []domain.AudioSegment{
		{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
	}})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil {
		t.Fatalf("POST audio-role-plan: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("audio-role-plan status=%d", resp.StatusCode)
	}
	rolePlan, err := h.db.GetAudioRolePlan(context.Background(), assetID)
	if err != nil {
		t.Fatalf("get audio role plan: %v", err)
	}

	source := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "first", SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000},
		{Index: 1, SourceText: "next", SpeakerID: "SPEAKER_01", StartMs: 5000, EndMs: 6000},
	}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

	wav := media.GeneratePCM16WAV(16000, 1, 3200)
	audioObj, err := h.casStore.Put(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("put dub audio: %v", err)
	}
	playbackEndMs, reserveMs, fitPolicyID := service.NewFitController().ResolvePlaybackWindow(3000, 5000)
	variant := domain.DubSegmentsVariant{
		ID: "issue151-borrowed-window", SchemaVersion: domain.DubSegmentsSchemaVersion,
		AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
		DubScriptVariantCAS: lineage.DubScriptCAS, VoiceAssignmentCAS: lineage.VoiceAssignmentCAS,
		TranscriptArtifactCAS: lineage.TranscriptCAS, AudioRolePlanCAS: rolePlan.CASHash,
		FitPolicyID: fitPolicyID, OverallStatus: "PASS",
		Segments: []domain.DubSegment{{
			Index: 0, SpeechBlockIndices: []int{0}, SpeakerID: "SPEAKER_00",
			StartMs: 1000, EndMs: 3000, SlotDurationMs: 2000, MeasuredDurationMs: 3200,
			AudioSHA256: audioObj.SHA256, FitDecision: domain.FitActionAccept,
			DubPlaybackEndMs: playbackEndMs, EffectiveReserveMs: reserveMs,
		}},
		FitPlans: []domain.DubbingFitPlan{{
			SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: playbackEndMs - 1000, UsableSlotMs: playbackEndMs - 1000,
			MeasuredDurationMs: 3200, DubPlaybackEndMs: playbackEndMs, EffectiveReserveMs: reserveMs,
			FitPolicyID: fitPolicyID, SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
		}},
		CreatedAt: time.Now().UTC(), ProvenanceHash: "issue151-borrowed-window-prov",
	}
	variantBytes, _ := json.Marshal(variant)
	variantObj, err := h.casStore.Put(bytes.NewReader(variantBytes))
	if err != nil {
		t.Fatalf("put dub variant: %v", err)
	}
	if err := h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID: variant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
		CASHash: variantObj.SHA256, ProvenanceHash: variant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: variant.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub variant index: %v", err)
	}

	mixReq := map[string]any{
		"run_id": runID, "job_id": jobID, "target_language": "vi",
		"dub_segments_cas": variantObj.SHA256, "audio_stems_cas": stems.CASHash,
	}
	resp, mix := runAudioMix(t, h, assetID, mixReq)
	if resp.StatusCode != http.StatusCreated || mix == nil || mix.OverallStatus != "PASS" {
		t.Fatalf("borrowed playback window should mix successfully: status=%d mix=%+v", resp.StatusCode, mix)
	}

	mixReq["dub_segments_cas"] = strings.Repeat("f", 64)
	resp, _ = runAudioMix(t, h, assetID, mixReq)
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		t.Fatalf("unknown pinned dub CAS should fail closed, got status=%d", resp.StatusCode)
	}
}
