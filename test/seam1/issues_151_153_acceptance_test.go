package seam1_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	expectSeam1MixRefusal(t, resp, http.StatusUnprocessableEntity, "is unreadable")
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

		// Each fallible sub-case below mutates the fixture it derives from. Segments, FitPlans and
		// their membership slices are slices, so a plain struct copy aliases every other sub-case's
		// edit and an earlier refusal would mask its own guard.
		cloneVariant := func(v domain.DubSegmentsVariant) domain.DubSegmentsVariant {
			out := v
			out.Segments = append([]domain.DubSegment(nil), v.Segments...)
			out.FitPlans = append([]domain.DubbingFitPlan(nil), v.FitPlans...)
			for i := range out.Segments {
				out.Segments[i].SpeechBlockIndices = append([]int(nil), v.Segments[i].SpeechBlockIndices...)
			}
			for i := range out.FitPlans {
				out.FitPlans[i].SpeechBlockIndices = append([]int(nil), v.FitPlans[i].SpeechBlockIndices...)
			}
			return out
		}

		// 2. End + 1ms: 3601ms audio exceeds playback window -> REFUSED
		overWav := media.GeneratePCM16WAV(16000, 1, 3601)
		overObj, _ := h.casStore.Put(bytes.NewReader(overWav))
		overVariant := cloneVariant(exactVariant)
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
		expectSeam1MixRefusal(t, respOver, http.StatusUnprocessableEntity, "segment 0 decoded waveform exceeds playback window")
		if mixOver == nil || mixOver.OverallStatus != "REFUSED" || !strings.Contains(mixOver.RefusalReason, "exceeds playback window") {
			t.Fatalf("end + 1ms (3401ms) must be refused with the playback-window reason, got %+v", mixOver)
		}

		// 3. Metadata shorter than actual audio: claims 3400ms, but actual audio has 3800ms -> REFUSED
		longWav := media.GeneratePCM16WAV(16000, 1, 3800)
		longObj, _ := h.casStore.Put(bytes.NewReader(longWav))
		lyingVariant := cloneVariant(exactVariant)
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
		expectSeam1MixRefusal(t, respLying, http.StatusUnprocessableEntity, "segment 0 decoded waveform exceeds playback window")
		if mixLying == nil || mixLying.OverallStatus != "REFUSED" {
			t.Fatalf("lying metadata (audio exceeds playback window) must be refused, got %+v", mixLying)
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
		roundVariant := cloneVariant(exactVariant)
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
		expectSeam1MixRefusal(t, respRound, http.StatusUnprocessableEntity, "segment 0 decoded waveform exceeds playback window")
		if mixRound == nil || mixRound.OverallStatus != "REFUSED" {
			t.Fatalf("sample rounding overrun must be refused, got %+v", mixRound)
		}

		// 5. Missing middle clip: eligible blocks 0 and 1, but block 1 is omitted -> REFUSED
		twoSegRoleBody, _ := json.Marshal(map[string]any{"segments": []domain.AudioSegment{
			{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 5000, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
		}})
		respTwo, _ := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(twoSegRoleBody))
		respTwo.Body.Close()
		twoRolePlan, _ := h.db.GetAudioRolePlan(context.Background(), assetID)
		missingMiddleVariant := cloneVariant(exactVariant)
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
		expectSeam1MixRefusal(t, respMissing, http.StatusUnprocessableEntity, "dub-eligible source member 1 has no accepted replacement clip")
		if mixMissing == nil || mixMissing.OverallStatus != "REFUSED" {
			t.Fatalf("missing middle replacement clip must be refused, got %+v", mixMissing)
		}

		// 6. Foreign member: referencing non-existent member 99 -> REFUSED. The variant pins the plan
		// the asset currently holds (case 5 replaced it), so the refusal is the membership scan's.
		foreignVariant := cloneVariant(exactVariant)
		foreignVariant.ID = "foreign-member-refuse"
		foreignVariant.AudioRolePlanCAS = twoRolePlan.CASHash
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
		expectSeam1MixRefusal(t, respForeign, http.StatusUnprocessableEntity, "segment 0 references foreign or non-dub-eligible source member 99")
		if mixForeign == nil || mixForeign.OverallStatus != "REFUSED" {
			t.Fatalf("foreign source member must be refused, got %+v", mixForeign)
		}

		// 7. Corrupt pinned CAS: bytes in CAS are corrupted, DB has valid index -> fail closed without fallback
		corruptObj, _ := h.casStore.Put(bytes.NewReader([]byte("{invalid-json-corrupt-payload")))
		respCorrupt, mixCorrupt := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": corruptObj.SHA256, "audio_stems_cas": stems.CASHash,
		})
		expectSeam1MixRefusal(t, respCorrupt, http.StatusUnprocessableEntity, "could not be decoded")
		if mixCorrupt == nil || mixCorrupt.OverallStatus != "REFUSED" {
			t.Fatalf("corrupt pinned CAS must fail closed, got %+v", mixCorrupt)
		}
	})
}

// ---------------------------------------------------------------------------
// Issue #153 helpers: explicit playback-contract variants and exact refusal reads
// ---------------------------------------------------------------------------

// seam1SegmentSpec describes one accepted dub segment as the caller claims it: which canonical
// source member it replaces, the playback window it claims, and the measured length of its
// synthesized waveform. Stating the claim explicitly is what lets a test express a variant that
// borrows past the boundary the mixer can prove.
type seam1SegmentSpec struct {
	Source             domain.TranslationInputSegment
	SpeechBlockIndices []int
	PlaybackEndMs      int64
	ReserveMs          int64
	ClipMs             int
}

// seam1BuildVariant materializes a DubSegmentsVariant for the pinned lineage, one DubSegment and
// one DubbingFitPlan per spec, each carrying its own clip in CAS.
func seam1BuildVariant(t *testing.T, h *testHarness, assetID, runID, targetLang, id string, lineage seam1DubLineage, specs []seam1SegmentSpec) domain.DubSegmentsVariant {
	t.Helper()
	policyID := seam1CurrentFitPolicyID(t)
	segments := make([]domain.DubSegment, 0, len(specs))
	fitPlans := make([]domain.DubbingFitPlan, 0, len(specs))
	for i, spec := range specs {
		members := spec.SpeechBlockIndices
		if len(members) == 0 {
			members = []int{spec.Source.Index}
		}
		clipMs := spec.ClipMs
		if clipMs <= 0 {
			clipMs = int(spec.Source.EndMs - spec.Source.StartMs)
		}
		wav := media.GeneratePCM16WAV(16000, 1, int64(clipMs))
		obj, err := h.casStore.Put(bytes.NewReader(wav))
		if err != nil {
			t.Fatalf("put seam1 dub clip for %s: %v", id, err)
		}
		slotMs := spec.PlaybackEndMs - spec.Source.StartMs
		segments = append(segments, domain.DubSegment{
			Index: i, SpeechBlockIndices: members, SpeakerID: spec.Source.SpeakerID,
			StartMs: spec.Source.StartMs, EndMs: spec.Source.EndMs,
			SlotDurationMs: slotMs, MeasuredDurationMs: int64(clipMs),
			AudioSHA256: obj.SHA256, Voice: domain.VoiceProfile{ID: "seam1_voice", Language: targetLang},
			FitDecision:      domain.FitActionAccept,
			DubPlaybackEndMs: spec.PlaybackEndMs, EffectiveReserveMs: spec.ReserveMs,
		})
		fitPlans = append(fitPlans, domain.DubbingFitPlan{
			SegmentIndex: i, SpeakerID: spec.Source.SpeakerID, SlotDurationMs: slotMs, UsableSlotMs: slotMs,
			MeasuredDurationMs: int64(clipMs), DubPlaybackEndMs: spec.PlaybackEndMs, EffectiveReserveMs: spec.ReserveMs,
			FitPolicyID: policyID, SpeechBlockIndices: members, Decision: domain.FitActionAccept,
		})
	}
	return domain.DubSegmentsVariant{
		ID: id, SchemaVersion: domain.DubSegmentsSchemaVersion,
		AssetID: assetID, RunID: runID, TargetLanguage: targetLang,
		DubScriptVariantCAS: lineage.DubScriptCAS, VoiceAssignmentCAS: lineage.VoiceAssignmentCAS,
		TranscriptArtifactCAS: lineage.TranscriptCAS, AudioRolePlanCAS: lineage.AudioRolePlanCAS,
		FitPolicyID: policyID, Segments: segments, FitPlans: fitPlans,
		OverallStatus: "PASS", ProvenanceHash: id + "-prov", CreatedAt: time.Now().UTC(),
	}
}

// seam1StoreVariant stores a dub variant in CAS and returns its content address, the handle the
// public audio-mix call consumes.
func seam1StoreVariant(t *testing.T, h *testHarness, v domain.DubSegmentsVariant) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal seam1 dub variant %s: %v", v.ID, err)
	}
	obj, err := h.casStore.Put(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("put seam1 dub variant %s: %v", v.ID, err)
	}
	return obj.SHA256
}

// seam1MixRefusal reads the public refusal contract off an audio-mix response: the HTTP status and
// the server's own error text.
func seam1MixRefusal(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read audio-mix refusal body: %v", err)
	}
	_ = resp.Body.Close()
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out.Error
}

// expectSeam1MixRefusal asserts the exact public refusal: the status the endpoint returned plus the
// reason the mixer published, instead of merely "not created".
func expectSeam1MixRefusal(t *testing.T, resp *http.Response, wantStatus int, wantReason string) {
	t.Helper()
	gotStatus, reason := seam1MixRefusal(t, resp)
	if gotStatus != wantStatus {
		t.Fatalf("audio-mix refusal status = %d, want %d (reason: %q)", gotStatus, wantStatus, reason)
	}
	if !strings.Contains(reason, "audio mixer refused") {
		t.Fatalf("expected the mixer's own refusal contract, got %q", reason)
	}
	if !strings.Contains(reason, wantReason) {
		t.Fatalf("audio-mix refusal reason = %q, want it to contain %q", reason, wantReason)
	}
}

// ---------------------------------------------------------------------------
// Item 1: a candidate that could only fit by borrowing into a protected vocal region
// (singing/music-vocal or uncertain) is refused; the proven window never enters it.
// ---------------------------------------------------------------------------
func TestSeam1_Issue153_ProtectedRegionBlocksBorrowedPlaybackWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		role domain.AudioRole
	}{
		{name: "SingingMusicVocalRegion", role: domain.AudioRoleSingingMusicVocal},
		{name: "UncertainRegion", role: domain.AudioRoleUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHarness(t)
			jobID, runID := createJobAndRunWithDuration(t, h, 7.0)
			job := getJobViaAPI(t, h, jobID)
			assetID := job.SourceAssetID

			_, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
			postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
				{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
				{StartMs: 3200, EndMs: 4800, Role: tc.role},
			})
			source := []domain.TranslationInputSegment{
				{Index: 0, SourceText: "first", SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000},
			}
			lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

			// The protected region is the boundary: borrowing stops at 3140ms — the 200ms gap less
			// the 60ms reserve — and never reaches into [3200ms, 4800ms].
			windowEnd, reserve, _ := service.NewFitController().ResolvePlaybackWindow(3000, 3200)
			if windowEnd != 3140 || reserve != 60 {
				t.Fatalf("a protected %s region must cap the borrowed window before 3200ms: end=%d reserve=%d, want 3140/60", tc.role, windowEnd, reserve)
			}

			mix := func(cas string) (*http.Response, *domain.DubMixArtifact) {
				return runAudioMix(t, h, assetID, map[string]any{
					"run_id": runID, "job_id": jobID, "target_language": "vi",
					"dub_segments_cas": cas, "audio_stems_cas": stems.CASHash,
				})
			}

			// A candidate that ends exactly at the proven boundary fits the 2140ms window and mixes:
			// the refusal below is about the borrow, not about the fixture being unmixable.
			fitting := seam1BuildVariant(t, h, assetID, runID, "vi", "protected-window-exact", lineage, []seam1SegmentSpec{
				{Source: source[0], PlaybackEndMs: 3140, ReserveMs: 60, ClipMs: 2140},
			})
			resp, mixArtifact := mix(seam1StoreVariant(t, h, fitting))
			if resp.StatusCode != http.StatusCreated || mixArtifact == nil || mixArtifact.OverallStatus != "PASS" {
				t.Fatalf("a candidate ending at the protected boundary must mix: status=%d mix=%+v", resp.StatusCode, mixArtifact)
			}

			// A 2200ms candidate does not fit the 2000ms source slot: it would need playback through
			// 3200ms, exactly where the protected region begins. The mixer recomputes the boundary
			// from the pinned transcript and role plan and refuses the claimed window.
			borrowing := seam1BuildVariant(t, h, assetID, runID, "vi", "protected-window-borrow", lineage, []seam1SegmentSpec{
				{Source: source[0], PlaybackEndMs: 3200, ReserveMs: 0, ClipMs: 2200},
			})
			resp, _ = mix(seam1StoreVariant(t, h, borrowing))
			expectSeam1MixRefusal(t, resp, http.StatusUnprocessableEntity,
				"segment 0 playback policy evidence mismatch: end=3200/3140 reserve=0/60")

			// Same claimed window, but the pinned waveform itself reaches into the region: the
			// frame-exact playback-window check refuses it.
			overlong := seam1BuildVariant(t, h, assetID, runID, "vi", "protected-window-overlong", lineage, []seam1SegmentSpec{
				{Source: source[0], PlaybackEndMs: 3140, ReserveMs: 60, ClipMs: 2200},
			})
			resp, _ = mix(seam1StoreVariant(t, h, overlong))
			expectSeam1MixRefusal(t, resp, http.StatusUnprocessableEntity,
				"segment 0 decoded waveform exceeds playback window")
		})
	}
}

// ---------------------------------------------------------------------------
// Item 1 (adjacent speech): an immediately following turn proves there is no silence to
// borrow, so any claimed borrow is refused.
// ---------------------------------------------------------------------------
func TestSeam1_Issue153_AdjacentSpeechTurnBlocksBorrowing(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRunWithDuration(t, h, 7.0)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	_, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
		{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 3000, EndMs: 4500, Role: domain.AudioRoleNarrationDialogue},
	})
	source := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "first", SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000},
		{Index: 1, SourceText: "second", SpeakerID: "SPEAKER_01", StartMs: 3000, EndMs: 4500},
	}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

	// The next turn starts exactly at the source end: no proven silence exists, so the accepted
	// window is the source window itself.
	windowEnd, reserve, _ := service.NewFitController().ResolvePlaybackWindow(3000, 3000)
	if windowEnd != 3000 || reserve != 0 {
		t.Fatalf("an adjacent turn must leave no borrow allowance: end=%d reserve=%d, want 3000/0", windowEnd, reserve)
	}

	mix := func(cas string) (*http.Response, *domain.DubMixArtifact) {
		return runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": cas, "audio_stems_cas": stems.CASHash,
		})
	}

	// Both adjacent turns replaced inside their own windows: accepted.
	honest := seam1BuildVariant(t, h, assetID, runID, "vi", "adjacent-honest", lineage, []seam1SegmentSpec{
		{Source: source[0], PlaybackEndMs: 3000, ReserveMs: 0},
		{Source: source[1], PlaybackEndMs: 4500, ReserveMs: 0},
	})
	resp, mixArtifact := mix(seam1StoreVariant(t, h, honest))
	if resp.StatusCode != http.StatusCreated || mixArtifact == nil || mixArtifact.OverallStatus != "PASS" {
		t.Fatalf("a variant that honors the adjacent turn must mix: status=%d mix=%+v", resp.StatusCode, mixArtifact)
	}

	// A variant computed against a timeline that does not carry the adjacent turn claims the
	// standard gap borrowing to 4600ms (reserve 400ms) — a window this timeline never granted.
	forgedEnd, forgedReserve, _ := service.NewFitController().ResolvePlaybackWindow(3000, 5000)
	if forgedEnd != 4600 || forgedReserve != 400 {
		t.Fatalf("expected the standard gap claim to be 4600/400, got %d/%d", forgedEnd, forgedReserve)
	}
	forged := seam1BuildVariant(t, h, assetID, runID, "vi", "adjacent-forged", lineage, []seam1SegmentSpec{
		{Source: source[0], PlaybackEndMs: forgedEnd, ReserveMs: forgedReserve},
		{Source: source[1], PlaybackEndMs: 4500, ReserveMs: 0},
	})
	resp, _ = mix(seam1StoreVariant(t, h, forged))
	expectSeam1MixRefusal(t, resp, http.StatusUnprocessableEntity,
		"segment 0 playback policy evidence mismatch: end=4600/3000 reserve=400/0")
}

// ---------------------------------------------------------------------------
// Item 2: a variant that maps the same source speech member twice — or re-covers a
// member another segment already covered — is refused.
// ---------------------------------------------------------------------------
func TestSeam1_Issue153_DuplicateSourceMemberCoverageRefused(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRunWithDuration(t, h, 7.0)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	_, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
		{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
	})
	source := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "first", SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000},
	}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

	mix := func(cas string) *http.Response {
		resp, _ := runAudioMix(t, h, assetID, map[string]any{
			"run_id": runID, "job_id": jobID, "target_language": "vi",
			"dub_segments_cas": cas, "audio_stems_cas": stems.CASHash,
		})
		return resp
	}

	// The single accepted segment is itself the honest baseline: it proves the fixture mixes.
	honest := seam1BuildVariant(t, h, assetID, runID, "vi", "single-member-honest", lineage, []seam1SegmentSpec{
		{Source: source[0], PlaybackEndMs: 3000, ReserveMs: 0},
	})
	resp, mixArtifact := runAudioMix(t, h, assetID, map[string]any{
		"run_id": runID, "job_id": jobID, "target_language": "vi",
		"dub_segments_cas": seam1StoreVariant(t, h, honest), "audio_stems_cas": stems.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || mixArtifact == nil || mixArtifact.OverallStatus != "PASS" {
		t.Fatalf("an honest single-member variant must mix: status=%d mix=%+v", resp.StatusCode, mixArtifact)
	}

	// One segment claiming member 0 twice: the member is covered more than once.
	doubled := seam1BuildVariant(t, h, assetID, runID, "vi", "member-twice-in-one-segment", lineage, []seam1SegmentSpec{
		{Source: source[0], SpeechBlockIndices: []int{0, 0}, PlaybackEndMs: 3000, ReserveMs: 0},
	})
	expectSeam1MixRefusal(t, mix(seam1StoreVariant(t, h, doubled)), http.StatusUnprocessableEntity,
		"source member 0 is covered more than once")

	// Two segments both claiming member 0: the second segment re-covers an accepted member.
	recovers := seam1BuildVariant(t, h, assetID, runID, "vi", "member-recovered-by-second-segment", lineage, []seam1SegmentSpec{
		{Source: source[0], PlaybackEndMs: 3000, ReserveMs: 0},
		{Source: source[0], PlaybackEndMs: 3000, ReserveMs: 0},
	})
	expectSeam1MixRefusal(t, mix(seam1StoreVariant(t, h, recovers)), http.StatusUnprocessableEntity,
		"source member 0 is covered more than once")
}

// ---------------------------------------------------------------------------
// Item 4: executeRun parks the run and marks the job review_required when the dub stage
// cannot place any candidate (every one overran its immutable slot).
// ---------------------------------------------------------------------------
func TestSeam1_Issue153_DubReviewRequiredParksRunAndMarksJobReviewRequired(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	// Every synthesized Vietnamese candidate is far longer than its slot, so none can be accepted.
	defaultVITTSFake(t, h).DurationMs = 30000

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "dub_review_park.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusPaused, 10*time.Second)
	if finalRun.Status != domain.RunStatusPaused {
		t.Fatalf("an unplaceable dub candidate must park the run, got %q; stages: %+v", finalRun.Status, getRunStages(t, h.server.URL, runID))
	}
	if finalRun.CompletedAt != nil {
		t.Errorf("a parked run must not carry completed_at, got %v", finalRun.CompletedAt)
	}

	// The parked run is the review lifecycle, not a failure: the job follows the run.
	if job := getJobViaAPI(t, h, jobID); job.Status != "review_required" {
		t.Fatalf("expected job status review_required alongside the parked run, got %q", job.Status)
	}

	// The queue keeps the parked run out of the drain until an operator resumes it.
	qResp, err := http.Get(h.server.URL + "/api/v1/queue")
	if err != nil {
		t.Fatalf("GET queue: %v", err)
	}
	var qBody struct {
		Queue []domain.QueueEntry `json:"queue"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qBody)
	qResp.Body.Close()
	foundParked := false
	for _, entry := range qBody.Queue {
		if entry.RunID == runID {
			foundParked = entry.Status == domain.RunStatusPaused
		}
	}
	if !foundParked {
		t.Fatalf("expected the parked run to sit paused in the queue, got %+v", qBody.Queue)
	}

	// The dub stage succeeded and published the review artifact; the pipeline stopped there
	// instead of mixing a soundtrack with no replacement voice.
	var dubStage *domain.StageExecution
	stages := getRunStages(t, h.server.URL, runID)
	for i := range stages {
		switch stages[i].Stage {
		case "dub_synthesize":
			dubStage = &stages[i]
		case "audio_mix":
			t.Errorf("no mix may follow a REVIEW_REQUIRED dub stage, found %s (%s)", stages[i].Stage, stages[i].Status)
		}
	}
	if dubStage == nil || dubStage.Status != domain.StageStatusSucceeded || dubStage.ArtifactSHA256 == "" {
		t.Fatalf("expected a succeeded dub_synthesize stage carrying the review artifact, got %+v", dubStage)
	}
	rc, err := h.casStore.Get(dubStage.ArtifactSHA256)
	if err != nil {
		t.Fatalf("read dub review artifact: %v", err)
	}
	var variant domain.DubSegmentsVariant
	err = json.NewDecoder(rc).Decode(&variant)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("decode dub review artifact: %v", err)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.Segments) != 0 || len(variant.ReviewSegments) == 0 {
		t.Fatalf("expected an unplaceable REVIEW_REQUIRED variant, got status=%s accepted=%d review=%d",
			variant.OverallStatus, len(variant.Segments), len(variant.ReviewSegments))
	}
}
