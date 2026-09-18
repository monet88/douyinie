package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/storage"
)

// TestSeam1_FullDub_VerticalSlice_EndToEnd tests the complete Douyinie Phase 1 pipeline (T16)
// through the versioned localhost RuntimeHost API (Seam 1):
//
// Pipeline Flow:
// 1. Rights Attestation + Source Media Ingest + Preflight (via createJobAndRun)
// 2. AudioRolePlan (dialogue vs singing vs BGM/SFX)
// 3. Speech Understanding (ASR + forced aligner -> canonical SpeechBlocks -> TranscriptArtifact)
// 4. Translation (Meaning-First TranslationVariant)
// 5. Dub Script Adaptation (Duration-Adapted DubScriptVariant)
// 6. Voice Assignment (Frozen VoiceAssignment per speaker)
// 7. Dub Synthesis + Duration Probing + Fit Controller -> DubSegmentsVariant (accepted DubSegments)
// 8. Stem Separation + Audio Mixing (DubSegments + AudioStemArtifacts + AudioRolePlan -> DubMixArtifact)
// 9. Visual Text Detection (TextRegionPlan) + Localization (LocalizedVisualTrack + LocalizedSubtitleTrack)
// 10. RenderPlan Freezing (SourceAsset + DubMix + LocalizedSubtitleTrack -> frozen RenderPlan)
// 11. Preview Render (Proxy profile + identical plan semantics -> PreviewRenderArtifact)
// 12. Final Render (High-quality profile + identical plan semantics -> FinalRenderArtifact)
// 13. ReviewItem Projection verification (exception-only review)
func TestSeam1_FullDub_VerticalSlice_EndToEnd(t *testing.T) {
	h := setupHarness(t)

	// 1. Create Job and Run (ingests media and completes preflight)
	jobID, runID := createJobAndRunWithDuration(t, h, 10.0)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 2. Setup AudioRolePlan, TranslationVariant, and DubScriptVariant (2 speech turns with generous 3000ms slots)
	// Gap 1:  [4000ms, 5500ms] (gap = 1500ms)
	// Turn 2: [5500ms, 8500ms] (slot = 3000ms)
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      3000,
		},
		{
			Index:      1,
			SourceText: "明天也会很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    3500,
			EndMs:      6500,
		},
	}
	transVariant, dubScriptVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	if len(transVariant.Segments) != 2 || len(dubScriptVariant.Segments) != 2 {
		t.Fatalf("expected 2 translation & dub script segments, got trans=%d dub=%d", len(transVariant.Segments), len(dubScriptVariant.Segments))
	}
	// Overwrite AudioRolePlan to reflect exact speech turns vs BGM intervals
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 3500, EndMs: 6500, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	roleResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil || (roleResp.StatusCode != http.StatusOK && roleResp.StatusCode != http.StatusCreated) {
		t.Fatalf("setup audio role plan failed: %v", err)
	}
	roleResp.Body.Close()
	// 6. Voice Assignment (Frozen VoiceAssignment per speaker)
	assignBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	}
	resp, voiceAssign := runAssignVoices(t, h, assetID, assignBody)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST voice-assignment failed: status=%d", resp.StatusCode)
	}
	// 7. TTS Synthesis + Measured Duration Probing + Fit Gate (T14)
	synthBody := map[string]any{
		"run_id":               runID,
		"job_id":               jobID,
		"target_language":      "vi",
		"voice_assignment_cas": voiceAssign.CASHash,
		"dub_script_cas":       dubScriptVariant.CASHash,
	}
	resp, dubSegmentsVariant := runDubSynthesize(t, h, assetID, synthBody)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST dub-synthesize failed: status=%d", resp.StatusCode)
	}

	// Invariant: All accepted DubSegments must satisfy zero adjacent-speech overrun
	if len(dubSegmentsVariant.Segments) != 2 {
		t.Fatalf("expected 2 accepted DubSegments, got %d", len(dubSegmentsVariant.Segments))
	}
	for i, seg := range dubSegmentsVariant.Segments {
		slotBudget := seg.EndMs - seg.StartMs
		if seg.MeasuredDurationMs > slotBudget {
			t.Errorf("segment %d overruns slot budget: measured=%dms slot=%dms", i, seg.MeasuredDurationMs, slotBudget)
		}
		if seg.NaturalGapAfterMs < 0 {
			t.Errorf("segment %d has negative inter-turn gap: %dms", i, seg.NaturalGapAfterMs)
		}
	}
	// Explicitly assert perceptible inter-turn gap preservation against source timing/accepted segments
	seg0 := dubSegmentsVariant.Segments[0]
	expectedGap0 := segments[1].StartMs - (segments[0].StartMs + seg0.MeasuredDurationMs)
	if seg0.NaturalGapAfterMs != expectedGap0 {
		t.Errorf("segment 0 natural gap mismatch: got %dms, expected %dms", seg0.NaturalGapAfterMs, expectedGap0)
	}
	if seg0.NaturalGapAfterMs < 500 {
		t.Errorf("expected perceptible inter-turn gap >= 500ms, got %dms", seg0.NaturalGapAfterMs)
	}

	// 8. Stem Separation & Audio Mixing (T15)
	// Separate Stems
	sepBody := map[string]any{
		"run_id": runID,
	}
	resp, stems := runSeparateStems(t, h, assetID, sepBody)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST separate-stems failed: status=%d", resp.StatusCode)
	}
	if stems == nil || len(stems.Stems) == 0 {
		t.Fatalf("expected valid audio stems, got %+v", stems)
	}

	// Mix Audio
	mixBody := map[string]any{
		"run_id":           runID,
		"job_id":           jobID,
		"target_language":  "vi",
		"dub_segments_cas": dubSegmentsVariant.CASHash,
		"audio_stems_cas":  stems.CASHash,
	}
	resp, dubMix := runAudioMix(t, h, assetID, mixBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST audio-mix failed: status=%d", resp.StatusCode)
	}
	if dubMix.OverallStatus != "PASS" {
		t.Fatalf("expected PASS dub mix status, got %s (reason: %s)", dubMix.OverallStatus, dubMix.RefusalReason)
	}
	if dubMix.AudioCASHash == "" {
		t.Fatalf("expected non-empty AudioCASHash on dub mix")
	}
	// Invariant: Soundtrack preservation semantics using DubMix preservation plan/output evidence
	if !dubMix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved=true on dub mix")
	}
	if !dubMix.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed=true on dub mix")
	}
	if !dubMix.PreservationPlan.PreserveSFX {
		t.Errorf("expected PreserveSFX=true in preservation plan")
	}
	if !dubMix.PreservationPlan.PreserveAmbience {
		t.Errorf("expected PreserveAmbience=true in preservation plan")
	}
	if len(dubMix.PreservationPlan.SpeechWindows) != 2 {
		t.Fatalf("expected 2 speech windows in preservation plan, got %d", len(dubMix.PreservationPlan.SpeechWindows))
	}
	for i, win := range dubMix.PreservationPlan.SpeechWindows {
		if win.Action != "suppress_dialogue" {
			t.Errorf("speech window %d expected action suppress_dialogue, got %s", i, win.Action)
		}
		if win.StartMs != segments[i].StartMs || win.EndMs != segments[i].EndMs {
			t.Errorf("speech window %d bounds [%d, %d] != expected [%d, %d]", i, win.StartMs, win.EndMs, segments[i].StartMs, segments[i].EndMs)
		}
	}

	// Invariant: Non-tautological acoustic signal verification from CAS audio samples
	// Proves output audio behavior from actual CAS samples (T15 contract):
	// - Outside speech windows: mixed output preserves original soundtrack background
	// - Inside speech windows: active dub matches background + localized dub (excluding vocals/dialogue);
	//   after dub ends, mixed matches background while vocals remain suppressed.
	var bgStem, vocalsStem domain.AudioStem
	for _, s := range stems.Stems {
		if s.Type == domain.StemTypeBackground {
			bgStem = s
		} else if s.Type == domain.StemTypeVocals {
			vocalsStem = s
		}
	}
	if bgStem.AudioCASHash == "" || vocalsStem.AudioCASHash == "" {
		t.Fatalf("missing bg or vocals stem in stems artifact: %+v", stems)
	}

	rBG, err := h.casStore.Get(bgStem.AudioCASHash)
	if err != nil {
		t.Fatalf("failed to read bg stem from CAS (%s): %v", bgStem.AudioCASHash, err)
	}
	defer rBG.Close()
	bgData, err := io.ReadAll(rBG)
	if err != nil {
		t.Fatalf("read bg stem bytes failed: %v", err)
	}
	bgSamples, _, err := media.ExtractPCM16Samples(bgData)
	if err != nil {
		t.Fatalf("extract bg PCM16 samples failed: %v", err)
	}

	rVocals, err := h.casStore.Get(vocalsStem.AudioCASHash)
	if err != nil {
		t.Fatalf("failed to read vocals stem from CAS (%s): %v", vocalsStem.AudioCASHash, err)
	}
	defer rVocals.Close()
	vocalsData, err := io.ReadAll(rVocals)
	if err != nil {
		t.Fatalf("read vocals stem bytes failed: %v", err)
	}
	vocalsSamples, _, err := media.ExtractPCM16Samples(vocalsData)
	if err != nil {
		t.Fatalf("extract vocals PCM16 samples failed: %v", err)
	}

	rDub0, err := h.casStore.Get(dubSegmentsVariant.Segments[0].AudioSHA256)
	if err != nil {
		t.Fatalf("failed to read dub segment 0 from CAS: %v", err)
	}
	defer rDub0.Close()
	dub0Data, err := io.ReadAll(rDub0)
	if err != nil {
		t.Fatalf("read dub segment 0 bytes failed: %v", err)
	}
	dub0Samples, _, err := media.ExtractPCM16Samples(dub0Data)
	if err != nil {
		t.Fatalf("extract dub segment 0 PCM16 samples failed: %v", err)
	}

	rDub1, err := h.casStore.Get(dubSegmentsVariant.Segments[1].AudioSHA256)
	if err != nil {
		t.Fatalf("failed to read dub segment 1 from CAS: %v", err)
	}
	defer rDub1.Close()
	dub1Data, err := io.ReadAll(rDub1)
	if err != nil {
		t.Fatalf("read dub segment 1 bytes failed: %v", err)
	}
	dub1Samples, _, err := media.ExtractPCM16Samples(dub1Data)
	if err != nil {
		t.Fatalf("extract dub segment 1 PCM16 samples failed: %v", err)
	}

	rAudio, err := h.casStore.Get(dubMix.AudioCASHash)
	if err != nil {
		t.Fatalf("failed to read mixed audio from CAS (%s): %v", dubMix.AudioCASHash, err)
	}
	defer rAudio.Close()
	mixedData, err := io.ReadAll(rAudio)
	if err != nil {
		t.Fatalf("read mixed audio bytes failed: %v", err)
	}
	mixedSamples, mixedHdr, err := media.ExtractPCM16Samples(mixedData)
	if err != nil {
		t.Fatalf("extract mixed PCM16 samples failed: %v", err)
	}
	mixRate := int(mixedHdr.SampleRate)
	if mixRate <= 0 {
		t.Fatalf("invalid mixed audio sample rate: %d", mixRate)
	}

	// 1. Outside speech window (gap [3000ms, 3500ms] at t=3250ms): original background preserved
	sampleAtGap := mixedSamples[(mixRate*3250)/1000]
	expectedAtGap := bgSamples[(mixRate*3250)/1000]
	if sampleAtGap != expectedAtGap {
		t.Errorf("expected sample at 3250ms (outside speech window / gap) to preserve background %d, got %d", expectedAtGap, sampleAtGap)
	}

	// 2. Outside speech window (outro [6500ms, 10000ms] at t=8000ms): original background preserved
	sampleAtOutro := mixedSamples[(mixRate*8000)/1000]
	expectedAtOutro := bgSamples[(mixRate*8000)/1000]
	if sampleAtOutro != expectedAtOutro {
		t.Errorf("expected sample at 8000ms (outside speech window / outro) to preserve background %d, got %d", expectedAtOutro, sampleAtOutro)
	}

	// 3. Inside speech window 0 during active dub ([0ms, 1320ms] at t=500ms): dialogue suppressed + bg + dub
	idx500 := (mixRate * 500) / 1000
	sampleAtTurn0Active := mixedSamples[idx500]
	dub0Idx500 := (mixRate * (500 - int(dubSegmentsVariant.Segments[0].StartMs))) / 1000
	expectedTurn0Active := int16(int32(bgSamples[idx500]) + int32(dub0Samples[dub0Idx500]))
	if math.Abs(float64(sampleAtTurn0Active-expectedTurn0Active)) > 50 {
		t.Errorf("expected sample at 500ms (turn 0 active dub) to be ~%d (bg %d + dub %d), got %d",
			expectedTurn0Active, bgSamples[idx500], dub0Samples[dub0Idx500], sampleAtTurn0Active)
	}
	if vocalsSamples[idx500] != 0 && sampleAtTurn0Active == expectedTurn0Active+vocalsSamples[idx500] {
		t.Errorf("expected source vocals (%d) to be excluded from mixed output at 500ms", vocalsSamples[idx500])
	}

	// 4. Inside speech window 0 during natural gap ([1320ms, 3000ms] at t=2000ms): source dialogue suppressed to 0, leaving bg
	idx2000 := (mixRate * 2000) / 1000
	sampleAtTurn0Suppressed := mixedSamples[idx2000]
	expectedTurn0Suppressed := bgSamples[idx2000]
	if sampleAtTurn0Suppressed != expectedTurn0Suppressed {
		t.Errorf("expected sample at 2000ms (turn 0 dialogue suppressed) to match bg %d, got %d", expectedTurn0Suppressed, sampleAtTurn0Suppressed)
	}
	if vocalsSamples[idx2000] != 0 && sampleAtTurn0Suppressed == expectedTurn0Suppressed+vocalsSamples[idx2000] {
		t.Errorf("expected source vocals (%d) to remain suppressed at 2000ms", vocalsSamples[idx2000])
	}

	// 5. Inside speech window 1 during active dub ([3500ms, 4820ms] at t=4000ms): dialogue suppressed + bg + dub
	idx4000 := (mixRate * 4000) / 1000
	sampleAtTurn1Active := mixedSamples[idx4000]
	dub1Idx4000 := (mixRate * (4000 - int(dubSegmentsVariant.Segments[1].StartMs))) / 1000
	expectedTurn1Active := int16(int32(bgSamples[idx4000]) + int32(dub1Samples[dub1Idx4000]))
	if math.Abs(float64(sampleAtTurn1Active-expectedTurn1Active)) > 50 {
		t.Errorf("expected sample at 4000ms (turn 1 active dub) to be ~%d (bg %d + dub %d), got %d",
			expectedTurn1Active, bgSamples[idx4000], dub1Samples[dub1Idx4000], sampleAtTurn1Active)
	}
	if vocalsSamples[idx4000] != 0 && sampleAtTurn1Active == expectedTurn1Active+vocalsSamples[idx4000] {
		t.Errorf("expected source vocals (%d) to be excluded from mixed output at 4000ms", vocalsSamples[idx4000])
	}

	// 6. Inside speech window 1 during natural gap ([4820ms, 6500ms] at t=5500ms): source dialogue suppressed to 0, leaving bg
	idx5500 := (mixRate * 5500) / 1000
	sampleAtTurn1Suppressed := mixedSamples[idx5500]
	expectedTurn1Suppressed := bgSamples[idx5500]
	if sampleAtTurn1Suppressed != expectedTurn1Suppressed {
		t.Errorf("expected sample at 5500ms (turn 1 dialogue suppressed) to match bg %d, got %d", expectedTurn1Suppressed, sampleAtTurn1Suppressed)
	}
	if vocalsSamples[idx5500] != 0 && sampleAtTurn1Suppressed == expectedTurn1Suppressed+vocalsSamples[idx5500] {
		t.Errorf("expected source vocals (%d) to remain suppressed at 5500ms", vocalsSamples[idx5500])
	}
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	respDetect, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(detectBody),
	)
	if err != nil || respDetect.StatusCode != http.StatusCreated {
		t.Fatalf("POST detect-text failed: resp=%v err=%v", respDetect, err)
	}
	respDetect.Body.Close()

	visTrackBody, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	})
	respVis, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/localized-visual-track", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(visTrackBody),
	)
	if err != nil || respVis.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(respVis.Body)
		t.Fatalf("POST localized-visual-track failed: status=%d body=%s err=%v", respVis.StatusCode, string(b), err)
	}
	var visTrackResp struct {
		Track domain.LocalizedVisualTrack `json:"localized_visual_track"`
	}
	_ = json.NewDecoder(respVis.Body).Decode(&visTrackResp)
	respVis.Body.Close()

	// Invariant: Subtitle track text must be grounded in canonical TranslationVariant
	resp, err = http.Get(fmt.Sprintf("%s/api/v1/assets/%s/localized-subtitle-track?target_language=vi", h.server.URL, assetID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET localized-subtitle-track failed: resp=%v err=%v", resp, err)
	}
	var subTrackResp struct {
		Track domain.LocalizedSubtitleTrack `json:"localized_subtitle_track"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&subTrackResp)
	resp.Body.Close()

	if len(subTrackResp.Track.Cues) != len(transVariant.Segments) {
		t.Fatalf("expected %d subtitle cues, got %d", len(transVariant.Segments), len(subTrackResp.Track.Cues))
	}
	// Verify that caption text reflects the canonical TranslationVariant text
	for i, seg := range transVariant.Segments {
		if subTrackResp.Track.Cues[i].Text != seg.TargetText {
			t.Errorf("subtitle cue %d text %q != canonical TranslationVariant %q", i, subTrackResp.Track.Cues[i].Text, seg.TargetText)
		}
	}

	// 10. Freeze RenderPlan (T11)
	planBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"dub_mix_cas":     dubMix.CASHash,
	}
	resp, renderPlan := runFreezeRenderPlan(t, h, assetID, planBody)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST render-plan failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	if renderPlan.ProvenanceHash == "" || renderPlan.CASHash == "" {
		t.Fatalf("expected valid provenance and CAS hashes on RenderPlan")
	}

	// Invariant: Subtitle plan was auto-resolved from persisted LocalizedSubtitleTrack
	if renderPlan.SubtitlePlan.CueCount != 2 {
		t.Errorf("expected 2 subtitle cues in frozen RenderPlan, got %d", renderPlan.SubtitlePlan.CueCount)
	}

	// 11. Preview Render pass
	prevBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"plan_cas_hash":   renderPlan.CASHash,
	}
	resp, previewArtifact := runRenderPreview(t, h, assetID, prevBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST render/preview failed: status=%d", resp.StatusCode)
	}
	if previewArtifact.OverallStatus != "PASS" {
		t.Errorf("expected PASS status on preview render, got %s", previewArtifact.OverallStatus)
	}

	// 12. Final Render pass
	finalBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"plan_cas_hash":   renderPlan.CASHash,
	}
	resp, finalArtifact := runRenderFinal(t, h, assetID, finalBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST render/final failed: status=%d", resp.StatusCode)
	}
	if finalArtifact.OverallStatus != "PASS" {
		t.Errorf("expected PASS status on final render, got %s", finalArtifact.OverallStatus)
	}

	// Invariant: Preview and Final share exact semantic plan references (Parity)
	if previewArtifact.ConsumedPlan.PlanProvenanceHash != finalArtifact.ConsumedPlan.PlanProvenanceHash {
		t.Errorf("parity breach: preview plan prov %q != final plan prov %q",
			previewArtifact.ConsumedPlan.PlanProvenanceHash, finalArtifact.ConsumedPlan.PlanProvenanceHash)
	}
	if previewArtifact.ConsumedPlan.DubMixCASHash != finalArtifact.ConsumedPlan.DubMixCASHash {
		t.Errorf("parity breach: preview dub mix %q != final dub mix %q",
			previewArtifact.ConsumedPlan.DubMixCASHash, finalArtifact.ConsumedPlan.DubMixCASHash)
	}
	if previewArtifact.ConsumedPlan.SubtitlePlan.CASHash != finalArtifact.ConsumedPlan.SubtitlePlan.CASHash {
		t.Errorf("parity breach: preview sub plan %q != final sub plan %q",
			previewArtifact.ConsumedPlan.SubtitlePlan.CASHash, finalArtifact.ConsumedPlan.SubtitlePlan.CASHash)
	}

	// Distinct Immutables Invariant: Preview and Final have different artifact provenance hashes and schemas
	if previewArtifact.ProvenanceHash == finalArtifact.ProvenanceHash {
		t.Errorf("preview and final must have distinct artifact provenance hashes, got %s", previewArtifact.ProvenanceHash)
	}
	if previewArtifact.SchemaVersion != domain.PreviewRenderSchemaVersion {
		t.Errorf("expected preview schema version %d, got %d", domain.PreviewRenderSchemaVersion, previewArtifact.SchemaVersion)
	}
	if finalArtifact.SchemaVersion != domain.FinalRenderSchemaVersion {
		t.Errorf("expected final schema version %d, got %d", domain.FinalRenderSchemaVersion, finalArtifact.SchemaVersion)
	}

	// 13. ReviewItem Projection check for clean run (0 blockers)
	resp, err = http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items failed: resp=%v err=%v", resp, err)
	}
	var reviewResp struct {
		AssetID     string              `json:"asset_id"`
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reviewResp); err != nil {
		t.Fatalf("decode review-items response: %v", err)
	}
	resp.Body.Close()

	if reviewResp.Count != 0 {
		t.Errorf("expected 0 review items on clean passing run, got %d: %+v", reviewResp.Count, reviewResp.ReviewItems)
	}

	// Run-scoped review endpoint check
	resp, err = http.Get(fmt.Sprintf("%s/api/v1/runs/%s/review-items", h.server.URL, runID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run review-items failed: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
}

// TestSeam1_FullDub_ReviewItemProjection_SurfacesExceptions proves that when upstream quality gates
// encounter unresolvable overruns, low OCR confidence, or QA flags, the ReviewItem projection
// correctly surfaces actionable exception items in the projection API without blocking unaffected artifacts.
func TestSeam1_FullDub_ReviewItemProjection_SurfacesExceptions(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. AudioRolePlan with an uncertain segment
	rolePlan := domain.AudioRolePlan{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2000, EndMs: 4000, Role: domain.AudioRoleUncertain},
		},
	}
	rolePlanBytes, _ := json.Marshal(rolePlan)
	resp, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(rolePlanBytes),
	)
	if err != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
		t.Fatalf("POST audio-role-plan failed: %v", err)
	}
	resp.Body.Close()

	// 2. TextRegionPlan with uncertain role / low confidence region
	textPlan := domain.TextRegionPlan{
		ID:             "text-plan-review-test",
		SchemaVersion:  domain.TextRegionPlanSchemaVersion,
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-review-1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:             "reg_uncertain_1",
				Role:           domain.TextRoleUncertain,
				Text:           "模糊文字",
				FirstSeenMs:    500,
				LastSeenMs:     1500,
				ReviewRequired: true,
				ReviewReason:   "low_ocr_confidence_uncertain",
			},
		},
	}
	textBytes, _ := json.Marshal(textPlan)
	textObj, _ := h.casStore.Put(bytes.NewReader(textBytes))
	textPlan.CASHash = textObj.SHA256
	_ = h.db.SaveTextRegionPlanIndex(context.Background(), storage.TextRegionPlanIndex{
		ID:             textPlan.ID,
		AssetID:        assetID,
		CASHash:        textPlan.CASHash,
		ProvenanceHash: textPlan.ProvenanceHash,
		CreatedAt:      time.Now().UTC(),
	})

	// 3. DubSegmentsVariant with an unresolvable overrun review item
	dubVariant := domain.DubSegmentsVariant{
		ID:             "dub-seg-review-test",
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-dub-review-1",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeakerID:          "spk_1",
				StartMs:            0,
				EndMs:              2000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 1800,
				FitDecision:        domain.FitActionAccept,
			},
		},
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              1,
				SpeakerID:          "spk_1",
				StartMs:            2000,
				EndMs:              4000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 2900,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "unresolvable_tts_overrun_exceeds_max_attempts",
				AttemptCount:       3,
			},
		},
	}
	dubBytes, _ := json.Marshal(dubVariant)
	dubObj, _ := h.casStore.Put(bytes.NewReader(dubBytes))
	dubVariant.CASHash = dubObj.SHA256
	_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             dubVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        dubVariant.CASHash,
		ProvenanceHash: dubVariant.ProvenanceHash,
		OverallStatus:  "PASS",
		CreatedAt:      time.Now().UTC(),
	})

	// Query ReviewItems via Seam 1 API
	resp, err = http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items failed: resp=%v err=%v", resp, err)
	}
	var reviewResp struct {
		AssetID     string              `json:"asset_id"`
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reviewResp)
	resp.Body.Close()

	if reviewResp.Count != 3 {
		t.Fatalf("expected 3 review items (audio role + OCR + TTS overrun), got %d: %+v", reviewResp.Count, reviewResp.ReviewItems)
	}

	foundAudio := false
	foundOCR := false
	foundTTS := false
	for _, item := range reviewResp.ReviewItems {
		switch item.Type {
		case domain.ReviewItemTypeAudioRole:
			foundAudio = true
			if item.Severity != "warning" {
				t.Errorf("expected warning severity for audio role, got %s", item.Severity)
			}
		case domain.ReviewItemTypeUncertainRole, domain.ReviewItemTypeLowConfidenceOCR:
			foundOCR = true
			if item.RegionID != "reg_uncertain_1" {
				t.Errorf("expected region reg_uncertain_1, got %s", item.RegionID)
			}
		case domain.ReviewItemTypeTTSOverrun:
			foundTTS = true
			if item.Severity != "blocker" {
				t.Errorf("expected blocker severity for unresolvable overrun, got %s", item.Severity)
			}
			if item.ItemIndex != 1 {
				t.Errorf("expected item index 1, got %d", item.ItemIndex)
			}
		}
	}

	if !foundAudio || !foundOCR || !foundTTS {
		t.Errorf("missing expected review item types: audio=%v ocr=%v tts=%v", foundAudio, foundOCR, foundTTS)
	}
}

// TestSeam1_FullDub_AudioMix_RefusesOverrunCandidate proves that AudioMix strictly refuses
// overlong dub segments and prevents them from entering the final mix or render.
func TestSeam1_FullDub_AudioMix_RefusesOverrunCandidate(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Create audio stems
	sepBody := map[string]any{"run_id": runID}
	resp, stems := runSeparateStems(t, h, assetID, sepBody)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST separate-stems failed: status=%d", resp.StatusCode)
	}

	// AudioRolePlan
	rolePlan := domain.AudioRolePlan{
		Segments: []domain.AudioSegment{
			{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBytes, _ := json.Marshal(rolePlan)
	resp, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(roleBytes),
	)
	if err != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
		t.Fatalf("POST audio-role-plan failed: %v", err)
	}
	resp.Body.Close()

	// Create synthesized audio with 3500ms duration (exceeds 2000ms slot [1000, 3000])
	overlongWav := media.GeneratePCM16WAV(16000, 1, 3500)
	audioObj, _ := h.casStore.Put(bytes.NewReader(overlongWav))
	overlongVariant := domain.DubSegmentsVariant{
		ID:             "overlong-variant",
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-overlong",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeakerID:          "spk_1",
				StartMs:            1000,
				EndMs:              3000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 3500, // OVERRUN!
				AudioSHA256:        audioObj.SHA256,
				FitDecision:        domain.FitActionAccept,
			},
		},
	}
	varBytes, _ := json.Marshal(overlongVariant)
	varObj, _ := h.casStore.Put(bytes.NewReader(varBytes))
	overlongVariant.CASHash = varObj.SHA256
	_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             overlongVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        overlongVariant.CASHash,
		ProvenanceHash: overlongVariant.ProvenanceHash,
		OverallStatus:  "PASS",
		CreatedAt:      time.Now().UTC(),
	})

	// Attempt AudioMix -> MUST REFUSE (422)
	mixBody := map[string]any{
		"run_id":           runID,
		"job_id":           jobID,
		"target_language":  "vi",
		"dub_segments_cas": overlongVariant.CASHash,
		"audio_stems_cas":  stems.CASHash,
	}
	resp, _ = runAudioMix(t, h, assetID, mixBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity for overrun mixer refusal, got %d", resp.StatusCode)
	}

	// Attempt FreezeRenderPlan -> MUST FAIL CLOSED
	planBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	}
	resp, _ = runFreezeRenderPlan(t, h, assetID, planBody)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("expected FreezeRenderPlan to fail closed when dub mix is refused/missing, got status %d", resp.StatusCode)
	}
}

// TestSeam1_FullDub_SubtitleDubSemanticGrounding_Consistency tests that even when
// DubScriptVariant applies shorten-first adaptation to speech, the LocalizedSubtitleTrack
// remains strictly grounded in canonical TranslationVariant meaning, so subtitles and dub
// never disagree semantically.
func TestSeam1_FullDub_SubtitleDubSemanticGrounding_Consistency(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup translation with distinct meaning vs adapted spoken copy
	transSegments := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "今天天气很好。", StartMs: 1000, EndMs: 4000, SpeakerID: "spk_1"},
	}
	respTrans, transVariant := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"source_language": "zh",
		"target_language": "vi",
		"segments":        transSegments,
	})
	if respTrans.StatusCode != http.StatusCreated && respTrans.StatusCode != http.StatusOK {
		t.Fatalf("POST translate failed: %d", respTrans.StatusCode)
	}

	// 2. Seed the required AudioRolePlan for this speech-bearing fixture.
	roleBody, err := json.Marshal(map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 1000, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	if err != nil {
		t.Fatalf("marshal audio role plan: %v", err)
	}
	roleResp, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(roleBody),
	)
	if err != nil {
		t.Fatalf("POST audio-role-plan failed: %v", err)
	}
	roleResp.Body.Close()
	if roleResp.StatusCode != http.StatusCreated && roleResp.StatusCode != http.StatusOK {
		t.Fatalf("POST audio-role-plan failed: %d", roleResp.StatusCode)
	}

	// 3. Setup DubScriptVariant with shortened spoken copy
	respDub, dubScriptVariant := runDubScript(t, h, assetID, map[string]any{
		"run_id":                  runID,
		"job_id":                  jobID,
		"source_language":         "zh",
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	})
	if respDub.StatusCode != http.StatusCreated && respDub.StatusCode != http.StatusOK {
		t.Fatalf("POST dub-script failed: %d", respDub.StatusCode)
	}

	// 4. Run detect-text & localized-visual-track
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST detect-text failed")
	}
	resp.Body.Close()

	visBody, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	})
	resp, err = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/localized-visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(visBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST localized-visual-track failed")
	}
	resp.Body.Close()

	// 4. Retrieve LocalizedSubtitleTrack
	resp, err = http.Get(fmt.Sprintf("%s/api/v1/assets/%s/localized-subtitle-track?target_language=vi", h.server.URL, assetID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET localized-subtitle-track failed")
	}
	var subTrackResp struct {
		Track domain.LocalizedSubtitleTrack `json:"localized_subtitle_track"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&subTrackResp)
	resp.Body.Close()

	if len(subTrackResp.Track.Cues) != 1 {
		t.Fatalf("expected 1 subtitle cue, got %d", len(subTrackResp.Track.Cues))
	}

	// Subtitle text MUST match canonical TranslationVariant (MeaningText), not hallucinated or diverged text
	if subTrackResp.Track.Cues[0].Text != transVariant.Segments[0].TargetText {
		t.Errorf("subtitle text %q must equal canonical TranslationVariant target text %q",
			subTrackResp.Track.Cues[0].Text, transVariant.Segments[0].TargetText)
	}
	if dubScriptVariant.Segments[0].MeaningText != transVariant.Segments[0].TargetText {
		t.Errorf("dub script meaning text %q must equal canonical TranslationVariant target text %q",
			dubScriptVariant.Segments[0].MeaningText, transVariant.Segments[0].TargetText)
	}
}

// TestSeam1_FullDub_ReviewItemProjection_TranslationQA_Failure tests that translation meaning QA failures
// surface as structured ReviewItem entries in the exception review projection.
func TestSeam1_FullDub_ReviewItemProjection_TranslationQA_Failure(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Create a TranslationVariant with a QA failure (PassedQAGate = false)
	tVariant := domain.TranslationVariant{
		ID:             "trans-qa-fail-1",
		SchemaVersion:  domain.TranslationSchemaVersion,
		RunID:          runID,
		JobID:          jobID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		ProvenanceHash: "prov-trans-qa-fail",
		OverallQAScore: 0.45,
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "今天天气很好。",
				TargetText:   "Dịch sai nghĩa",
				QAConfidence: 0.45,
				PassedQAGate: false,
			},
		},
	}
	tBytes, _ := json.Marshal(tVariant)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	tVariant.CASHash = tObj.SHA256
	_ = h.db.SaveTranslationVariantIndex(context.Background(), storage.TranslationVariantIndex{
		ID:             tVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        tVariant.CASHash,
		ProvenanceHash: tVariant.ProvenanceHash,
		OverallQAScore: tVariant.OverallQAScore,
		CreatedAt:      time.Now().UTC(),
	})

	// Query ReviewItems
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items failed: %v", err)
	}
	var reviewResp struct {
		AssetID     string              `json:"asset_id"`
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reviewResp)
	resp.Body.Close()

	if reviewResp.Count != 1 {
		t.Fatalf("expected 1 translation_qa review item, got %d: %+v", reviewResp.Count, reviewResp.ReviewItems)
	}
	if reviewResp.ReviewItems[0].Type != domain.ReviewItemTypeTranslationQA {
		t.Errorf("expected review item type %s, got %s", domain.ReviewItemTypeTranslationQA, reviewResp.ReviewItems[0].Type)
	}
	if reviewResp.ReviewItems[0].Stage != "translation" {
		t.Errorf("expected stage translation, got %s", reviewResp.ReviewItems[0].Stage)
	}
}

// TestSeam1_FullDub_FreezeRenderPlan_CorruptSubtitleTrack_FailsClosed proves that FreezeRenderPlan
// fails closed with a 500 error when an indexed LocalizedSubtitleTrack is corrupt or missing from CAS,
// rather than silently falling back to empty captions.
func TestSeam1_FullDub_FreezeRenderPlan_CorruptSubtitleTrack_FailsClosed(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Ingest minimal AudioRolePlan and separate stems
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	roleResp, _ := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	roleResp.Body.Close()

	sepResp, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	sepResp.Body.Close()

	mixResp, dubMix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"audio_stems_cas": stems.CASHash,
	})
	mixResp.Body.Close()

	// Manually insert a corrupt/non-existent CAS entry into localized_subtitle_tracks index
	err := h.db.SaveLocalizedSubtitleTrackIndex(context.Background(), storage.LocalizedSubtitleTrackIndex{
		ID:             "corrupt-sub-track-1",
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        "non_existent_sha256_corrupt_cas_entry",
		ProvenanceHash: "prov-corrupt-1",
		CueCount:       1,
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("failed to insert corrupt index: %v", err)
	}

	// Attempt to freeze RenderPlan without explicit SubtitlePlanCAS or SubtitleCues
	planBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"dub_mix_cas":     dubMix.CASHash,
	}
	resp, _ := runFreezeRenderPlan(t, h, assetID, planBody)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("expected FreezeRenderPlan to fail closed on corrupt LocalizedSubtitleTrack, got %d", resp.StatusCode)
	}
}

// TestSeam1_FullDub_LocalizeVisualTrack_MissingCanonicalMeaning_FailsClosed proves that LocalizeVisualTrack
// fails closed rather than falling back to SpokenText when MeaningText is empty in DubScriptVariant.
func TestSeam1_FullDub_LocalizeVisualTrack_MissingCanonicalMeaning_FailsClosed(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Create and save DubScriptVariant where MeaningText is empty (only SpokenText exists)
	dVariant := domain.DubScriptVariant{
		ID:             "dub-script-no-meaning",
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		Segments: []domain.DubScriptSegment{
			{
				Index:       0,
				SourceText:  "今天天气很好。",
				MeaningText: "", // missing canonical translation meaning text!
				SpokenText:  "Thời tiết tốt.",
				StartMs:     0,
				EndMs:       2000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dBytes, _ := json.Marshal(dVariant)
	dObj, _ := h.casStore.Put(bytes.NewReader(dBytes))
	_ = h.db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov-dvar-no-meaning",
		OverallQAScore: 0.9,
		CreatedAt:      time.Now().UTC(),
	})

	// Run detect-text first
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	respDetect, _ := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
	respDetect.Body.Close()

	// LocalizeVisualTrack should fail closed with an error because MeaningText is missing
	visBody, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	})
	respVis, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/localized-visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(visBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer respVis.Body.Close()
	if respVis.StatusCode == http.StatusOK || respVis.StatusCode == http.StatusCreated {
		t.Fatalf("expected LocalizeVisualTrack to fail closed when MeaningText is empty, got status %d", respVis.StatusCode)
	}
}

// TestSeam1_FullDub_ReviewItemProjection_DeterministicAndFailsOnCorruptCAS proves that
// ReviewItem projections produce stable deterministic IDs and timestamps, and fail closed on corrupt CAS references.
func TestSeam1_FullDub_ReviewItemProjection_DeterministicAndFailsOnCorruptCAS(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Insert a TranslationVariant with low QA score
	tVariant := domain.TranslationVariant{
		ID:             "trans-var-det",
		SchemaVersion:  domain.TranslationSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		ProvenanceHash: "prov-trans-det",
		OverallQAScore: 0.4,
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "原文",
				TargetText:   "Dịch",
				QAConfidence: 0.3,
				PassedQAGate: false,
			},
		},
		CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	tBytes, _ := json.Marshal(tVariant)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	_ = h.db.SaveTranslationVariantIndex(context.Background(), storage.TranslationVariantIndex{
		ID:             tVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: tVariant.ProvenanceHash,
		OverallQAScore: tVariant.OverallQAScore,
		CreatedAt:      tVariant.CreatedAt,
	})

	// Call review-items endpoint multiple times to verify determinism
	resp1, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("first review-items call failed: %v, status: %d", err, resp1.StatusCode)
	}
	var res1 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
	}
	_ = json.NewDecoder(resp1.Body).Decode(&res1)
	resp1.Body.Close()

	resp2, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("second review-items call failed: %v, status: %d", err, resp2.StatusCode)
	}
	var res2 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&res2)
	resp2.Body.Close()

	if len(res1.ReviewItems) != 1 || len(res2.ReviewItems) != 1 {
		t.Fatalf("expected exactly 1 review item, got %d and %d", len(res1.ReviewItems), len(res2.ReviewItems))
	}

	item1 := res1.ReviewItems[0]
	item2 := res2.ReviewItems[0]

	// ID and CreatedAt must be strictly identical across calls
	if item1.ID != item2.ID {
		t.Errorf("expected deterministic ReviewItem ID: %q != %q", item1.ID, item2.ID)
	}
	if !item1.CreatedAt.Equal(item2.CreatedAt) {
		t.Errorf("expected stable timestamp: %v != %v", item1.CreatedAt, item2.CreatedAt)
	}
	if !item1.CreatedAt.Equal(tVariant.CreatedAt) {
		t.Errorf("expected timestamp derived from immutable artifact: %v != %v", item1.CreatedAt, tVariant.CreatedAt)
	}

	// 2. Corrupt CAS reference -> must return 500 error, not silently omit
	_ = h.db.SaveTextRegionPlanIndex(context.Background(), storage.TextRegionPlanIndex{
		ID:             "corrupt-text-plan",
		AssetID:        assetID,
		CASHash:        "non_existent_text_plan_cas_hash",
		ProvenanceHash: "prov-corrupt-text",
		CreatedAt:      time.Now().UTC(),
	})

	respCorrupt, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer respCorrupt.Body.Close()
	if respCorrupt.StatusCode == http.StatusOK {
		t.Fatalf("expected review-items to return error on corrupt CAS reference, got 200 OK")
	}
}
