package seam1_test

import (
	"bytes"
	"context"
	"encoding/binary"
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
		"glossary": []domain.GlossaryEntry{{Source: "OpenAI", Target: ""}},
	})
	for _, fake := range poisoned {
		fake.InjectError = nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || invalidVariant != nil {
		t.Fatalf("direct invalid glossary must fail at ingress before provider routing: status=%d variant=%v", resp.StatusCode, invalidVariant)
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

	invalid := `{"glossary":[{"source":"OpenAI","target":""}]}`
	resp = postRun(invalid)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid glossary should return 400, got %d", resp.StatusCode)
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

	validConfig := `{"glossary":[{"source":"OpenAI","target":"OPENAI_LOCK"},{"source":" openai ","target":"IGNORE_CONFLICT"}]}`
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
	transcriptObj, err := h.casStore.Put(bytes.NewReader([]byte(fmt.Sprintf(`{"asset_id":%q,"speech_blocks":[{"index":0,"start_ms":1000,"end_ms":2000,"source_text":"OpenAI model","speaker_id":"SPEAKER_00","segment_type":"speech"}]}`, assetID))))
	if err != nil {
		t.Fatalf("put transcript CAS: %v", err)
	}
	_ = h.db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             "se-trans-" + runID,
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: transcriptObj.SHA256,
		CreatedAt:      time.Now().UTC(),
	})

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
	resp, err = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
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

	// Visual translation runs with Ephemeral: true and inherits the frozen run glossary without explicit input
	resp, ephemeral := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"source_language": "en",
		"target_language": "vi",
		"ephemeral":       true,
		"segments": []domain.TranslationInputSegment{{
			Index: 0, SourceText: "OpenAI model", StartMs: 1000, EndMs: 2000,
		}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || ephemeral == nil {
		t.Fatalf("ephemeral visual translation status=%d", resp.StatusCode)
	}
	if len(ephemeral.EffectiveGlossary.Entries) != 1 || ephemeral.EffectiveGlossary.Entries[0].Target != "OPENAI_LOCK" {
		t.Fatalf("ephemeral translation did not inherit frozen run glossary: %+v", ephemeral.EffectiveGlossary)
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

func TestSeam1_Issue153_PlaybackWindowBoundaryMatrix(t *testing.T) {
	fc := service.NewFitController()

	t.Run("NoNextTurn_NoBorrowing", func(t *testing.T) {
		end, reserve, _ := fc.ResolvePlaybackWindow(6000, 0)
		if end != 6000 || reserve != 0 {
			t.Fatalf("no next turn must not borrow silence: end=%d reserve=%d, want 6000/0", end, reserve)
		}
	})

	t.Run("GapSmallerThanReserve_Clamped", func(t *testing.T) {
		// Gap is 40ms, which is smaller than min reserve (50ms). Reserve must clamp to gap (40ms), end must not exceed source end.
		end, reserve, _ := fc.ResolvePlaybackWindow(1000, 1040)
		if end != 1000 {
			t.Fatalf("gap < reserve must not allow borrowing: end=%d, want 1000", end)
		}
		if reserve != 40 {
			t.Fatalf("reserve should be clamped to gap: reserve=%d, want 40", reserve)
		}
	})

	t.Run("Overlap_NoBorrowing", func(t *testing.T) {
		// next turn starts at 2800, before sourceEnd 3000
		end, reserve, _ := fc.ResolvePlaybackWindow(3000, 2800)
		if end != 3000 || reserve != 0 {
			t.Fatalf("overlap must disable borrowing: end=%d reserve=%d, want 3000/0", end, reserve)
		}
	})

	t.Run("DifferentSpeaker_BorrowingPermittedAcrossSpeakers", func(t *testing.T) {
		// Pinned canonical speech timeline: gap between 3000ms and 5000ms is 2000ms.
		// Borrowing is based on speech timeline, not speaker identity.
		end, reserve, _ := fc.ResolvePlaybackWindow(3000, 5000)
		if end != 4600 || reserve != 400 {
			t.Fatalf("different speaker gap must permit standard borrowing: end=%d reserve=%d, want 4600/400", end, reserve)
		}
	})

	t.Run("MixerBoundaryCases", func(t *testing.T) {
		h := setupHarness(t)
		jobID, runID := createJobAndRunWithDuration(t, h, 7.0)
		job := getJobViaAPI(t, h, jobID)
		assetID := job.SourceAssetID

		_, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
		roleBody, _ := json.Marshal(map[string]any{"segments": []domain.AudioSegment{
			{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		}})
		resp, _ := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
		resp.Body.Close()
		rolePlan, _ := h.db.GetAudioRolePlan(context.Background(), assetID)

		source := []domain.TranslationInputSegment{
			{Index: 0, SourceText: "first", SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000},
			{Index: 1, SourceText: "next", SpeakerID: "SPEAKER_01", StartMs: 5000, EndMs: 6000},
		}
		lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

		// Borrowing allows up to 4400ms: duration available = 4400 - 1000 = 3400ms
		playbackEndMs, reserveMs, fitPolicyID := fc.ResolvePlaybackWindow(3000, 5000)

		// 1. Exact end: 3600ms audio matches playback window (4600 - 1000 = 3600ms) -> PASS
		exactWav := media.GeneratePCM16WAV(16000, 1, 3600)
		exactObj, _ := h.casStore.Put(bytes.NewReader(exactWav))
		exactVariant := domain.DubSegmentsVariant{
			ID: "exact-end-pass", SchemaVersion: domain.DubSegmentsSchemaVersion,
			AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			DubScriptVariantCAS: lineage.DubScriptCAS, VoiceAssignmentCAS: lineage.VoiceAssignmentCAS,
			TranscriptArtifactCAS: lineage.TranscriptCAS, AudioRolePlanCAS: rolePlan.CASHash,
			FitPolicyID: fitPolicyID, OverallStatus: "PASS",
			Segments: []domain.DubSegment{{
				Index: 0, SpeechBlockIndices: []int{0}, SpeakerID: "SPEAKER_00",
				StartMs: 1000, EndMs: 3000, SlotDurationMs: 2000, MeasuredDurationMs: 3600,
				AudioSHA256: exactObj.SHA256, FitDecision: domain.FitActionAccept,
				DubPlaybackEndMs: playbackEndMs, EffectiveReserveMs: reserveMs,
			}},
			FitPlans: []domain.DubbingFitPlan{{
				SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: playbackEndMs - 1000, UsableSlotMs: playbackEndMs - 1000,
				MeasuredDurationMs: 3600, DubPlaybackEndMs: playbackEndMs, EffectiveReserveMs: reserveMs,
				FitPolicyID: fitPolicyID, SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
			}},
			CreatedAt: time.Now().UTC(), ProvenanceHash: "exact-end-prov",
		}
		b, _ := json.Marshal(exactVariant)
		vObj, _ := h.casStore.Put(bytes.NewReader(b))
		_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
			ID: exactVariant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			CASHash: vObj.SHA256, ProvenanceHash: exactVariant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: exactVariant.CreatedAt,
		})
		respMix, mix := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": vObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respMix.StatusCode != http.StatusCreated || mix == nil || mix.OverallStatus != "PASS" {
			t.Fatalf("exact end (3400ms) should mix successfully: status=%d mix=%+v", respMix.StatusCode, mix)
		}

		// 2. End + 1ms: 3601ms audio exceeds playback window -> REFUSED
		overWav := media.GeneratePCM16WAV(16000, 1, 3601)
		overObj, _ := h.casStore.Put(bytes.NewReader(overWav))
		overVariant := exactVariant
		overVariant.ID = "over-1ms-refuse"
		overVariant.Segments[0].MeasuredDurationMs = 3601
		overVariant.Segments[0].AudioSHA256 = overObj.SHA256
		overVariant.FitPlans[0].MeasuredDurationMs = 3601
		overVariant.ProvenanceHash = "over-1ms-prov"
		bOver, _ := json.Marshal(overVariant)
		vOverObj, _ := h.casStore.Put(bytes.NewReader(bOver))
		_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
			ID: overVariant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			CASHash: vOverObj.SHA256, ProvenanceHash: overVariant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: overVariant.CreatedAt,
		})
		respOver, mixOver := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": vOverObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respOver.StatusCode == http.StatusCreated && (mixOver != nil && mixOver.OverallStatus == "PASS") {
			t.Fatalf("end + 1ms (3401ms) must fail closed or be refused, got PASS")
		}

		// 3. Metadata shorter than actual audio: claims 3400ms, but actual audio has 3800ms -> REFUSED
		longWav := media.GeneratePCM16WAV(16000, 1, 3800)
		longObj, _ := h.casStore.Put(bytes.NewReader(longWav))
		lyingVariant := exactVariant
		lyingVariant.ID = "lying-metadata-refuse"
		lyingVariant.Segments[0].MeasuredDurationMs = 3400
		lyingVariant.Segments[0].AudioSHA256 = longObj.SHA256
		lyingVariant.FitPlans[0].MeasuredDurationMs = 3400
		lyingVariant.ProvenanceHash = "lying-metadata-prov"
		bLying, _ := json.Marshal(lyingVariant)
		vLyingObj, _ := h.casStore.Put(bytes.NewReader(bLying))
		_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
			ID: lyingVariant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			CASHash: vLyingObj.SHA256, ProvenanceHash: lyingVariant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: lyingVariant.CreatedAt,
		})
		respLying, mixLying := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": vLyingObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respLying.StatusCode == http.StatusCreated && (mixLying != nil && mixLying.OverallStatus == "PASS") {
			t.Fatalf("lying metadata (audio exceeds playback window) must be refused, got PASS")
		}

		// 4. Sample rounding: exactly 1 sample too many for 3600ms at 16000Hz (57601 samples) -> REFUSED
		dataSize := 57601 * 2
		var buf bytes.Buffer
		buf.WriteString("RIFF")
		_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize))
		buf.WriteString("WAVEfmt ")
		_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(16000))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(32000))
		_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
		_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
		buf.WriteString("data")
		_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))
		buf.Write(make([]byte, dataSize))
		roundWav := buf.Bytes()
		roundObj, _ := h.casStore.Put(bytes.NewReader(roundWav))
		roundVariant := exactVariant
		roundVariant.ID = "sample-rounding-refuse"
		roundVariant.Segments[0].AudioSHA256 = roundObj.SHA256
		roundVariant.ProvenanceHash = "sample-rounding-prov"
		bRound, _ := json.Marshal(roundVariant)
		vRoundObj, _ := h.casStore.Put(bytes.NewReader(bRound))
		_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
			ID: roundVariant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			CASHash: vRoundObj.SHA256, ProvenanceHash: roundVariant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: roundVariant.CreatedAt,
		})
		respRound, mixRound := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": vRoundObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respRound.StatusCode == http.StatusCreated && (mixRound != nil && mixRound.OverallStatus == "PASS") {
			t.Fatalf("sample rounding overrun must be refused, got PASS")
		}

		// 5. Missing middle clip: eligible blocks 0 and 1, but block 1 is omitted -> REFUSED
		twoSegRoleBody, _ := json.Marshal(map[string]any{"segments": []domain.AudioSegment{
			{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 5000, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
		}})
		respTwo, _ := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(twoSegRoleBody))
		respTwo.Body.Close()
		twoRolePlan, _ := h.db.GetAudioRolePlan(context.Background(), assetID)
		missingMiddleVariant := exactVariant
		missingMiddleVariant.ID = "missing-middle-refuse"
		missingMiddleVariant.AudioRolePlanCAS = twoRolePlan.CASHash
		missingMiddleVariant.ProvenanceHash = "missing-middle-prov"
		bMissing, _ := json.Marshal(missingMiddleVariant)
		vMissingObj, _ := h.casStore.Put(bytes.NewReader(bMissing))
		_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
			ID: missingMiddleVariant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			CASHash: vMissingObj.SHA256, ProvenanceHash: missingMiddleVariant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: missingMiddleVariant.CreatedAt,
		})
		respMissing, mixMissing := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": vMissingObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respMissing.StatusCode == http.StatusCreated && (mixMissing != nil && mixMissing.OverallStatus == "PASS") {
			t.Fatalf("missing middle replacement clip must be refused, got PASS")
		}

		// 6. Foreign / duplicate member: referencing non-existent member 99 -> REFUSED
		foreignVariant := exactVariant
		foreignVariant.ID = "foreign-member-refuse"
		foreignVariant.Segments[0].SpeechBlockIndices = []int{99}
		foreignVariant.FitPlans[0].SpeechBlockIndices = []int{99}
		foreignVariant.ProvenanceHash = "foreign-member-prov"
		bForeign, _ := json.Marshal(foreignVariant)
		vForeignObj, _ := h.casStore.Put(bytes.NewReader(bForeign))
		_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
			ID: foreignVariant.ID, AssetID: assetID, RunID: runID, JobID: jobID, TargetLanguage: "vi",
			CASHash: vForeignObj.SHA256, ProvenanceHash: foreignVariant.ProvenanceHash, OverallStatus: "PASS", CreatedAt: foreignVariant.CreatedAt,
		})
		respForeign, mixForeign := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": vForeignObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respForeign.StatusCode == http.StatusCreated && (mixForeign != nil && mixForeign.OverallStatus == "PASS") {
			t.Fatalf("foreign source member must be refused, got PASS")
		}

		// 7. Corrupt pinned CAS: bytes in CAS are corrupted, DB has valid index -> fail closed without fallback
		corruptObj, _ := h.casStore.Put(bytes.NewReader([]byte("{invalid-json-corrupt-payload")))
		respCorrupt, _ := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": corruptObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		if respCorrupt.StatusCode == http.StatusCreated || respCorrupt.StatusCode == http.StatusOK {
			t.Fatalf("corrupt pinned CAS must fail closed, got status %d", respCorrupt.StatusCode)
		}
	})
}
