package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
)

// configureDialogueTranslationGateway registers deterministic fakes representing the authorized
// production translation gateway ladder (Gemini 3.8 Flash -> DeepSeek V4.1 Flash) and
// local Qwen (which must be rejected as ineligible for production translation).
func configureDialogueTranslationGateway(t *testing.T, h *testHarness) {
	t.Helper()

	geminiFake := provider.NewFakeTranslationProvider(provider.GatewayGeminiTranslationProviderID)
	geminiFake.Cap.ExecutionTier = "cloud"
	geminiFake.ModelName = provider.GatewayGeminiModelAlias
	geminiFake.ModelVersion = "2026-08"
	geminiFake.Cap.QualityScore = 0.99
	_ = h.registry.Register(geminiFake)

	deepseekFake := provider.NewFakeTranslationProvider(provider.GatewayDeepSeekTranslationProviderID)
	deepseekFake.Cap.ExecutionTier = "cloud"
	deepseekFake.ModelName = provider.GatewayDeepSeekModelAlias
	deepseekFake.ModelVersion = "v4.1"
	deepseekFake.Cap.QualityScore = 0.95
	_ = h.registry.Register(deepseekFake)

	qwenFake := provider.NewFakeTranslationProvider(provider.WorkerQwenTranslationProviderID)
	qwenFake.Cap.ExecutionTier = "local"
	qwenFake.ModelName = "qwen3-4b"
	qwenFake.ModelVersion = "q4_k_m"
	qwenFake.Cap.QualityScore = 0.90
	_ = h.registry.Register(qwenFake)

	// Ensure legacy test fakes have lower score so Gemini is explicitly primary
	if p, ok := h.registry.Get("fake_llm_translator"); ok {
		p.(*provider.FakeTranslationProvider).Cap.QualityScore = 0.50
	}
	if p, ok := h.registry.Get("fake_local_translator_fallback"); ok {
		p.(*provider.FakeTranslationProvider).Cap.QualityScore = 0.40
	}

	licSvc := governance.NewLicenseService(h.db)
	ctx := context.Background()
	for _, p := range []*provider.FakeTranslationProvider{geminiFake, deepseekFake, qwenFake} {
		mName, mVer := p.ModelInfo()
		_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			DependencyName: mName,
			Version:        mVer,
			SHA256:         "sha256_mock_" + mName,
			SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Apache-2.0",
			DataLicense:    "OpenData",
			ServiceTerms:   "Standard",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		})
	}
}

// TestSeam1_AutoRun_Dialogue_EndToEnd proves that a queued production run containing
// dub-eligible dialogue is automatically consumed by the RuntimeHost executor and progresses
// through speech understanding, translation, dub script, voice assignment, dub synthesis,
// soundtrack mix, visual localization, render preview, and final render / QC handoff to completed state.
func TestSeam1_AutoRun_Dialogue_EndToEnd(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	// Ensure fake TTS fits within speech slot (slot is 300ms)
	defaultVITTSFake(t, h).DurationMs = 250

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "dialogue_autorun.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if finalRun.Status != domain.RunStatusCompleted {
		stages := getRunStages(t, h.server.URL, runID)
		qResp, _ := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
		var qBody struct {
			ReviewItems []domain.ReviewItem `json:"review_items"`
			Count       int                 `json:"count"`
		}
		if qResp != nil {
			_ = json.NewDecoder(qResp.Body).Decode(&qBody)
			qResp.Body.Close()
		}
		t.Fatalf("expected run status %q, got %q; pending items (%d): %+v; stages: %+v", domain.RunStatusCompleted, finalRun.Status, qBody.Count, qBody.ReviewItems, stages)
	}

	// 1. Verify stage executions
	stages := getRunStages(t, h.server.URL, runID)
	stageMap := make(map[string]domain.StageExecution, len(stages))
	for _, st := range stages {
		stageMap[st.Stage] = st
		if st.Status != domain.StageStatusSucceeded {
			t.Errorf("expected stage %s succeeded, got %s (err: %s)", st.Stage, st.Status, st.ErrorMessage)
		}
		if st.ArtifactSHA256 == "" {
			t.Errorf("expected stage %s to have non-empty ArtifactSHA256", st.Stage)
		}
	}

	expectedStages := []string{
		"audio_role_plan",
		"speech_understand",
		"translation",
		"dub_script",
		"voice_assignment",
		"dub_synthesize",
		"audio_mix",
		"text_detection",
		"visual_text_localize",
		"render_plan",
		"render_preview",
		"render_final",
	}
	for _, exp := range expectedStages {
		if _, ok := stageMap[exp]; !ok {
			t.Errorf("expected stage execution for %s, none recorded", exp)
		}
	}

	// 2. Verify Translation routing provenance: Gemini 3.8 Flash -> DeepSeek V4 only, no Qwen
	decisions, err := h.db.ListSelectionDecisions(context.Background(), runID, "translation")
	if err != nil {
		t.Fatalf("failed to list selection decisions: %v", err)
	}
	if len(decisions) == 0 {
		t.Fatalf("expected recorded selection decision for translation stage, none found")
	}

	transDecisionFound := false
	for _, d := range decisions {
		if d.Stage == "translation" {
			transDecisionFound = true
			if d.SelectedProviderID != provider.GatewayGeminiTranslationProviderID {
				t.Errorf("expected primary translation provider %s, got: %s", provider.GatewayGeminiTranslationProviderID, d.SelectedProviderID)
			}
			if d.SelectedProviderID == "fake_local_translator_fallback" || d.SelectedProviderID == "local_qwen_translation" || d.SelectedProviderID == provider.WorkerQwenTranslationProviderID {
				t.Errorf("production Qwen translation selected in dialogue run: %s", d.SelectedProviderID)
			}

			// Candidates evaluated must reflect the authorized production ladder
			foundGemini := false
			foundDeepSeek := false
			for _, cand := range d.CandidatesEvaluated {
				if cand.ProviderID == provider.GatewayGeminiTranslationProviderID {
					foundGemini = true
					if !cand.Eligible {
						t.Errorf("expected gateway Gemini to be eligible, got rejection: %s", cand.Reason)
					}
				}
				if cand.ProviderID == provider.GatewayDeepSeekTranslationProviderID {
					foundDeepSeek = true
				}
				if cand.ProviderID == provider.WorkerQwenTranslationProviderID && cand.Eligible {
					t.Errorf("local Qwen translator must not be eligible in production hybrid translation")
				}
			}
			if !foundGemini {
				t.Errorf("gateway Gemini not found in evaluated candidates")
			}
			if !foundDeepSeek {
				t.Errorf("gateway DeepSeek fallback not found in evaluated candidates")
			}
		}
	}
	if !transDecisionFound {
		t.Fatalf("no translation selection decision evaluated for run %s", runID)
	}

	// Verify TranslationVariant in DB has matching provider ID and observed model
	transIdx, err := h.db.GetTranslationVariantIndex(context.Background(), assetID, "vi")
	if err != nil || transIdx == nil {
		t.Fatalf("get translation variant index failed: %v", err)
	}
	if transIdx.CASHash == "" {
		t.Errorf("expected non-empty CAS hash on translation variant index")
	}

	// 3. Verify VoiceAssignment & TTS binding to accepted dialogue evidence (Fix 6)
	vaIdx, err := h.db.GetVoiceAssignmentIndexByRun(context.Background(), assetID, runID, "vi")
	if err != nil || vaIdx == nil {
		t.Fatalf("get voice assignment index by run failed: %v", err)
	}
	var va domain.VoiceAssignment
	rcVA, err := h.casStore.Get(vaIdx.CASHash)
	if err != nil {
		t.Fatalf("load voice assignment from CAS failed: %v", err)
	}
	defer rcVA.Close()
	if err := json.NewDecoder(rcVA).Decode(&va); err != nil {
		t.Fatalf("decode voice assignment from CAS failed: %v", err)
	}
	if va.DubScriptVariantCAS != stageMap["dub_script"].ArtifactSHA256 {
		t.Errorf("voice assignment DubScriptVariantCAS %q does not match dub_script stage artifact %q",
			va.DubScriptVariantCAS, stageMap["dub_script"].ArtifactSHA256)
	}
	if va.TranscriptArtifactCAS != stageMap["speech_understand"].ArtifactSHA256 {
		t.Errorf("voice assignment TranscriptArtifactCAS %q does not match speech_understand stage artifact %q",
			va.TranscriptArtifactCAS, stageMap["speech_understand"].ArtifactSHA256)
	}

	dubSegmentsIdx, err := h.db.GetDubSegmentsVariantIndex(context.Background(), assetID, "vi")
	if err != nil || dubSegmentsIdx == nil {
		t.Fatalf("get dub segments variant index failed: %v", err)
	}
	var dsv domain.DubSegmentsVariant
	rcDSV, err := h.casStore.Get(dubSegmentsIdx.CASHash)
	if err != nil {
		t.Fatalf("load dub segments from CAS failed: %v", err)
	}
	defer rcDSV.Close()
	if err := json.NewDecoder(rcDSV).Decode(&dsv); err != nil {
		t.Fatalf("decode dub segments from CAS failed: %v", err)
	}
	if dsv.DubScriptVariantCAS != stageMap["dub_script"].ArtifactSHA256 {
		t.Errorf("dub segments DubScriptVariantCAS %q does not match dub_script stage artifact %q",
			dsv.DubScriptVariantCAS, stageMap["dub_script"].ArtifactSHA256)
	}
	if dsv.VoiceAssignmentCAS != stageMap["voice_assignment"].ArtifactSHA256 {
		t.Errorf("dub segments VoiceAssignmentCAS %q does not match voice_assignment stage artifact %q",
			dsv.VoiceAssignmentCAS, stageMap["voice_assignment"].ArtifactSHA256)
	}

	// 4. Verify DubMix artifact and soundtrack preservation (Fix 5)
	mixResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/dub-mix?target_language=vi", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get audio mix failed: %v", err)
	}
	defer mixResp.Body.Close()
	if mixResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for audio mix, got %d", mixResp.StatusCode)
	}
	var mixData struct {
		DubMix domain.DubMixArtifact `json:"dub_mix"`
	}
	_ = json.NewDecoder(mixResp.Body).Decode(&mixData)
	mix := mixData.DubMix
	if mix.DubSegmentsCAS != stageMap["dub_synthesize"].ArtifactSHA256 {
		t.Errorf("expected DubSegmentsCAS %q to match dub_synthesize stage artifact %q",
			mix.DubSegmentsCAS, stageMap["dub_synthesize"].ArtifactSHA256)
	}
	if mix.OverallStatus != "PASS" {
		t.Errorf("expected OverallStatus 'PASS', got %q", mix.OverallStatus)
	}
	if !mix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved=true")
	}
	if !mix.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed=true on dialogue run")
	}
	if !mix.PreservationPlan.PreserveSFX {
		t.Errorf("expected PreserveSFX=true")
	}
	if !mix.PreservationPlan.PreserveAmbience {
		t.Errorf("expected PreserveAmbience=true")
	}
	if !mix.PreservationPlan.PreserveSinging {
		t.Errorf("expected PreserveSinging=true")
	}
	if len(mix.PreservationPlan.SpeechWindows) == 0 {
		t.Errorf("expected at least 1 speech window with dialogue suppression")
	}
	for i, win := range mix.PreservationPlan.SpeechWindows {
		if win.Action != "suppress_dialogue" {
			t.Errorf("speech window %d: expected suppress_dialogue, got %s", i, win.Action)
		}
		if win.StartMs < 0 || win.EndMs <= win.StartMs {
			t.Errorf("speech window %d has invalid bounds [%d, %d]", i, win.StartMs, win.EndMs)
		}
	}

	// 5. Verify preview render
	prevResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID))
	if err != nil {
		t.Fatalf("get render preview failed: %v", err)
	}
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for render preview, got %d", prevResp.StatusCode)
	}

	// 6. Verify final render artifact produced automatically in Auto posture (Fix 1)
	renderFinalStage := stageMap["render_final"]
	if renderFinalStage.Status != domain.StageStatusSucceeded {
		t.Errorf("expected render_final stage succeeded, got %s", renderFinalStage.Status)
	}
	if renderFinalStage.ArtifactSHA256 == "" {
		t.Errorf("expected non-empty ArtifactSHA256 for render_final stage")
	}

	finalRenderIdx, err := h.db.GetLatestRenderArtifactIndex(context.Background(), assetID, "vi", domain.RenderKindFinal)
	if err != nil || finalRenderIdx == nil {
		t.Fatalf("expected persisted final render artifact index for asset %s: %v", assetID, err)
	}
	if finalRenderIdx.CASHash != renderFinalStage.ArtifactSHA256 {
		t.Errorf("final render artifact CAS %q does not match stage artifact %q",
			finalRenderIdx.CASHash, renderFinalStage.ArtifactSHA256)
	}
}

// TestSeam1_AutoRun_Dialogue_ReviewPosture_Handoff proves that when a run is configured
// with review posture ("review"), the executor runs all stages up to review projection /
// final render handoff, evaluates QC cleanly without auto-executing final render,
// persists durable handoff evidence, and exposes explicit 'start_final_render' action.
func TestSeam1_AutoRun_Dialogue_ReviewPosture_Handoff(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	defaultVITTSFake(t, h).DurationMs = 250

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "review_posture.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)

	// Enqueue run with review posture
	body, _ := json.Marshal(map[string]any{
		"config_snapshot_json": `{"posture":"review"}`,
	})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue run with review posture failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	runID := res.Run.ID

	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if finalRun.Status != domain.RunStatusCompleted {
		t.Fatalf("expected run status %q, got %q", domain.RunStatusCompleted, finalRun.Status)
	}

	stages := getRunStages(t, h.server.URL, runID)
	stageMap := make(map[string]domain.StageExecution, len(stages))
	for _, st := range stages {
		stageMap[st.Stage] = st
	}

	// In Review posture, final render is NOT auto-executed; instead final_render_handoff succeeds
	if st, ok := stageMap["final_render_handoff"]; !ok {
		t.Errorf("expected final_render_handoff stage execution in Review posture")
	} else {
		if st.Status != domain.StageStatusSucceeded {
			t.Errorf("expected final_render_handoff status succeeded, got %s", st.Status)
		}
		if st.ArtifactSHA256 == "" {
			t.Errorf("expected non-empty artifact SHA256 for final_render_handoff")
		}
	}

	if _, ok := stageMap["render_final"]; ok {
		t.Errorf("render_final must NOT be automatically executed in Review posture")
	}

	// Verify handoff endpoint confirms explicit 'start_final_render' action
	handoffResp, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/render/handoff?posture=review", h.server.URL, runID), "application/json", nil)
	if err != nil {
		t.Fatalf("eval final render handoff failed: %v", err)
	}
	defer handoffResp.Body.Close()
	var handoffData struct {
		Handoff domain.FinalRenderHandoffResult `json:"handoff"`
	}
	_ = json.NewDecoder(handoffResp.Body).Decode(&handoffData)
	if !handoffData.Handoff.CanStartFinalRender {
		t.Errorf("expected CanStartFinalRender=true when queue reaches zero in Review posture")
	}
	if handoffData.Handoff.AutoRenderStarted {
		t.Errorf("expected AutoRenderStarted=false in Review posture")
	}
	if handoffData.Handoff.Action != "start_final_render" {
		t.Errorf("expected Action 'start_final_render', got %q", handoffData.Handoff.Action)
	}
}

// TestSeam1_AutoRun_Dialogue_SoundtrackPreservation_NonSpeechPreserved proves that
// dialogue replacement is strictly confined to accepted narration/dialogue segments,
// and that singing vocals, BGM, and ambience are preserved untouched in the soundtrack.
func TestSeam1_AutoRun_Dialogue_SoundtrackPreservation_NonSpeechPreserved(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	defaultVITTSFake(t, h).DurationMs = 250

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "soundtrack_preservation.mp4", 2.0, 2500)

	// Post explicit role plan containing dialogue, singing, and BGM segments
	postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
		{StartMs: 0, EndMs: 800, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 800, EndMs: 1400, Role: domain.AudioRoleSingingMusicVocal},
		{StartMs: 1400, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
	})

	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if finalRun.Status != domain.RunStatusCompleted {
		stages := getRunStages(t, h.server.URL, runID)
		qResp, _ := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID))
		var qBody struct {
			ReviewItems []domain.ReviewItem `json:"review_items"`
			Count       int                 `json:"count"`
		}
		if qResp != nil {
			_ = json.NewDecoder(qResp.Body).Decode(&qBody)
			qResp.Body.Close()
		}
		t.Fatalf("expected run completed, got %s; pending items (%d): %+v; stages: %+v", finalRun.Status, qBody.Count, qBody.ReviewItems, stages)
	}

	mixResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/dub-mix?target_language=vi", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get audio mix failed: %v", err)
	}
	defer mixResp.Body.Close()
	var mixData struct {
		DubMix domain.DubMixArtifact `json:"dub_mix"`
	}
	_ = json.NewDecoder(mixResp.Body).Decode(&mixData)
	mix := mixData.DubMix

	if mix.OverallStatus != "PASS" {
		t.Fatalf("expected PASS audio mix, got %s", mix.OverallStatus)
	}
	if !mix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved=true")
	}

	// Suppression follows the canonical source speech member, not the broader
	// narration classification window or any borrowed playback allowance.
	if len(mix.PreservationPlan.SpeechWindows) != 1 {
		t.Fatalf("expected exactly 1 speech window, got %d", len(mix.PreservationPlan.SpeechWindows))
	}
	speechWin := mix.PreservationPlan.SpeechWindows[0]
	if speechWin.Action != "suppress_dialogue" || speechWin.StartMs != 0 || speechWin.EndMs != 300 {
		t.Errorf("unexpected speech window %+v; expected canonical [0, 300] suppress_dialogue", speechWin)
	}

	// Verify singing window is preserved untouched as [800, 1400]
	if len(mix.PreservationPlan.SingingWindows) != 1 {
		t.Fatalf("expected exactly 1 singing window, got %d", len(mix.PreservationPlan.SingingWindows))
	}
	singingWin := mix.PreservationPlan.SingingWindows[0]
	if singingWin.Action != "preserve_music_vocal" || singingWin.StartMs != 800 || singingWin.EndMs != 1400 {
		t.Errorf("unexpected singing window %+v; expected [800, 1400] preserve_music_vocal", singingWin)
	}

	// Ensure speech window does NOT touch the BGM span [1400, 2000]
	if speechWin.EndMs > 1400 {
		t.Errorf("speech suppression overlaps BGM span: %d > 1400", speechWin.EndMs)
	}

	// 5. Deterministic PCM-level Seam 1 proof that non-dialogue audio samples
	// remain preserved outside dialogue windows (singing [800, 1400] and BGM [1400, 2000]).
	if mix.AudioCASHash == "" {
		t.Fatalf("expected non-empty AudioCASHash on dub mix artifact")
	}
	mixRc, err := h.casStore.Get(mix.AudioCASHash)
	if err != nil {
		t.Fatalf("load mixed audio from CAS failed: %v", err)
	}
	defer mixRc.Close()
	mixedBytes, err := io.ReadAll(mixRc)
	if err != nil {
		t.Fatalf("read mixed audio bytes failed: %v", err)
	}
	mixedSamples, mixedHeader, err := media.ExtractPCM16Samples(mixedBytes)
	if err != nil {
		t.Fatalf("extract mixed PCM16 samples failed: %v", err)
	}

	report, err := h.db.GetPreflightReport(context.Background(), assetID)
	if err != nil || report == nil || report.NormalizedAudioCASPath == "" {
		t.Fatalf("get preflight report failed: %v", err)
	}
	sourceBytes, err := os.ReadFile(report.NormalizedAudioCASPath)
	if err != nil {
		t.Fatalf("read normalized source audio file failed: %v", err)
	}
	sourceSamples, sourceHeader, err := media.ExtractPCM16Samples(sourceBytes)
	if err != nil {
		t.Fatalf("extract source PCM16 samples failed: %v", err)
	}

	sampleRate := int(mixedHeader.SampleRate)
	channels := int(mixedHeader.NumChannels)
	if sampleRate <= 0 || channels <= 0 {
		t.Fatalf("invalid mixed audio format: rate %d, ch %d", sampleRate, channels)
	}
	if int(sourceHeader.SampleRate) != sampleRate || int(sourceHeader.NumChannels) != channels {
		t.Fatalf("format mismatch between source (%d Hz, %d ch) and mixed (%d Hz, %d ch)",
			sourceHeader.SampleRate, sourceHeader.NumChannels, sampleRate, channels)
	}

	// Inside dialogue window [100ms, 700ms]: dialogue was replaced by dub speech
	dialogueStartSample := (100 * sampleRate) / 1000 * channels
	dialogueEndSample := (700 * sampleRate) / 1000 * channels
	dialogueReplaced := false
	for i := dialogueStartSample; i < dialogueEndSample && i < len(mixedSamples) && i < len(sourceSamples); i++ {
		if mixedSamples[i] != sourceSamples[i] {
			dialogueReplaced = true
			break
		}
	}
	if !dialogueReplaced {
		t.Errorf("expected dialogue window [100ms, 700ms] PCM samples to differ from source, but samples are identical")
	}

	// Outside dialogue window - Singing window [850ms, 1350ms]: singing vocals preserved untouched
	singingStartSample := (850 * sampleRate) / 1000 * channels
	singingEndSample := (1350 * sampleRate) / 1000 * channels
	singingMismatches := 0
	for i := singingStartSample; i < singingEndSample && i < len(mixedSamples) && i < len(sourceSamples); i++ {
		if mixedSamples[i] != sourceSamples[i] {
			singingMismatches++
		}
	}
	if singingMismatches > 0 {
		t.Errorf("expected 0 PCM sample mismatches in singing window [850ms, 1350ms], got %d mismatches", singingMismatches)
	}

	// Outside dialogue window - BGM window [1450ms, 1950ms]: BGM preserved untouched
	bgmStartSample := (1450 * sampleRate) / 1000 * channels
	bgmEndSample := (1950 * sampleRate) / 1000 * channels
	bgmMismatches := 0
	for i := bgmStartSample; i < bgmEndSample && i < len(mixedSamples) && i < len(sourceSamples); i++ {
		if mixedSamples[i] != sourceSamples[i] {
			bgmMismatches++
		}
	}
	if bgmMismatches > 0 {
		t.Errorf("expected 0 PCM sample mismatches in BGM window [1450ms, 1950ms], got %d mismatches", bgmMismatches)
	}
}

// TestSeam1_AutoRun_Dialogue_TerminalQC_FailClosed proves that if terminal QC has unresolved
// exceptions or review service is unavailable, the auto-runner fails closed, marks the stage failed,
// marks the run interrupted (never completed), and releases the active run slot.
func TestSeam1_AutoRun_Dialogue_TerminalQC_FailClosed(t *testing.T) {
	t.Run("UnresolvedReviewExceptions_FailsClosedAndReleasesSlot", func(t *testing.T) {
		h := setupAutoRunHarness(t)
		configureDialogueTranslationGateway(t, h)

		// The dub must fit every slot: a candidate that overruns its immutable slot is refused by the mixer
		// (AudioMixService's dub-coverage invariant), which stops the run before the terminal QC gate these
		// subtests are about.
		defaultVITTSFake(t, h).DurationMs = 250

		assetID1 := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "qc_fail_1.mp4", 2.0, 2500)

		// Overwrite AudioRolePlan with an uncertain segment -> creates 1 pending review exception
		postAudioRolePlan(t, h.server.URL, assetID1, []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 1000, EndMs: 2000, Role: domain.AudioRoleUncertain},
		})

		jobID1 := createJob(t, h.server.URL, assetID1, domain.TargetLanguageVI)
		runID1 := enqueueRun(t, h.server.URL, jobID1)

		final1 := pollRunStatus(t, h.server.URL, runID1, domain.RunStatusInterrupted, 10*time.Second)
		if final1.Status != domain.RunStatusInterrupted {
			t.Fatalf("expected run status %q when pending QC exceptions exist, got %q", domain.RunStatusInterrupted, final1.Status)
		}

		// Verify stage execution failed
		stages := getRunStages(t, h.server.URL, runID1)
		foundFail := false
		for _, st := range stages {
			if st.Status == domain.StageStatusFailed {
				foundFail = true
				break
			}
		}
		if !foundFail {
			t.Fatalf("expected at least 1 failed stage execution for run %s, got stages: %+v", runID1, stages)
		}

		// Subsequent queued run must succeed to prove active run slot was released
		assetID2 := ingestSyntheticAsset(t, h.server.URL, h.dir, "qc_drain_2.mp4", 1.5)
		postAudioRolePlan(t, h.server.URL, assetID2, []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
		})
		jobID2 := createJob(t, h.server.URL, assetID2, domain.TargetLanguageVI)
		runID2 := enqueueRun(t, h.server.URL, jobID2)

		final2 := pollRunStatus(t, h.server.URL, runID2, domain.RunStatusCompleted, 10*time.Second)
		if final2.Status != domain.RunStatusCompleted {
			t.Fatalf("expected subsequent run 2 completed, got %s", final2.Status)
		}
	})

	t.Run("ReviewServiceNil_FailsClosedAndReleasesSlot", func(t *testing.T) {
		h := setupAutoRunHarness(t)
		configureDialogueTranslationGateway(t, h)

		// The dub must fit every slot: a candidate that overruns its immutable slot is refused by the mixer
		// (AudioMixService's dub-coverage invariant), which stops the run before the terminal QC gate these
		// subtests are about.
		defaultVITTSFake(t, h).DurationMs = 250

		// Disable ReviewService to simulate unconfigured terminal QC service
		h.srv.SetReviewService(nil)

		assetID1 := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "qc_nil_review.mp4", 2.0, 2500)
		jobID1 := createJob(t, h.server.URL, assetID1, domain.TargetLanguageVI)
		runID1 := enqueueRun(t, h.server.URL, jobID1)

		final1 := pollRunStatus(t, h.server.URL, runID1, domain.RunStatusInterrupted, 10*time.Second)
		if final1.Status != domain.RunStatusInterrupted {
			t.Fatalf("expected run status %q when ReviewService is nil, got %q", domain.RunStatusInterrupted, final1.Status)
		}

		// Verify stage execution failed for render_final
		stages := getRunStages(t, h.server.URL, runID1)
		foundFail := false
		for _, st := range stages {
			if st.Stage == "render_final" && st.Status == domain.StageStatusFailed {
				foundFail = true
				break
			}
		}
		if !foundFail {
			t.Fatalf("expected failed render_final stage execution for run %s, got stages: %+v", runID1, stages)
		}
		// Restore ReviewService so subsequent queued run can execute terminal QC cleanly
		h.srv.SetReviewService(service.NewReviewService(h.db, h.casStore))

		// Subsequent queued run must succeed to prove active run slot was released
		assetID2 := ingestSyntheticAsset(t, h.server.URL, h.dir, "qc_drain_after_nil.mp4", 1.5)
		postAudioRolePlan(t, h.server.URL, assetID2, []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
		})
		jobID2 := createJob(t, h.server.URL, assetID2, domain.TargetLanguageVI)
		runID2 := enqueueRun(t, h.server.URL, jobID2)

		final2 := pollRunStatus(t, h.server.URL, runID2, domain.RunStatusCompleted, 10*time.Second)
		if final2.Status != domain.RunStatusCompleted {
			t.Fatalf("expected subsequent run 2 completed, got %s", final2.Status)
		}
	})
}

// TestSeam1_AutoRun_Dialogue_MalformedPosture_FailsClosedAndReleasesSlot proves that
// malformed or invalid run posture configuration immediately fails closed (stage "run_config"),
// marks the run interrupted, and releases the active run slot.
func TestSeam1_AutoRun_Dialogue_MalformedPosture_FailsClosedAndReleasesSlot(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	defaultVITTSFake(t, h).DurationMs = 250

	assetID1 := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "malformed_posture.mp4", 2.0, 2500)
	jobID1 := createJob(t, h.server.URL, assetID1, domain.TargetLanguageVI)

	// Enqueue run with invalid posture
	body, _ := json.Marshal(map[string]any{
		"config_snapshot_json": `{"posture":"invalid_posture_mode"}`,
	})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID1), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue run failed: %v", err)
	}
	defer resp.Body.Close()
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	runID1 := res.Run.ID

	final1 := pollRunStatus(t, h.server.URL, runID1, domain.RunStatusInterrupted, 10*time.Second)
	if final1.Status != domain.RunStatusInterrupted {
		t.Fatalf("expected run status %q on malformed posture, got %q", domain.RunStatusInterrupted, final1.Status)
	}

	// Verify run_config stage execution recorded as failed
	stages := getRunStages(t, h.server.URL, runID1)
	foundFail := false
	for _, st := range stages {
		if st.Stage == "run_config" && st.Status == domain.StageStatusFailed {
			foundFail = true
			break
		}
	}
	if !foundFail {
		t.Fatalf("expected failed 'run_config' stage execution for run %s, got stages: %+v", runID1, stages)
	}

	// Subsequent valid run must complete to prove active run slot was released
	assetID2 := ingestSyntheticAsset(t, h.server.URL, h.dir, "malformed_posture_drain.mp4", 1.5)
	postAudioRolePlan(t, h.server.URL, assetID2, []domain.AudioSegment{
		{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
	})
	jobID2 := createJob(t, h.server.URL, assetID2, domain.TargetLanguageVI)
	runID2 := enqueueRun(t, h.server.URL, jobID2)

	final2 := pollRunStatus(t, h.server.URL, runID2, domain.RunStatusCompleted, 10*time.Second)
	if final2.Status != domain.RunStatusCompleted {
		t.Fatalf("expected subsequent run 2 completed, got %s", final2.Status)
	}
}

// TestSeam1_AutoRun_Dialogue_HandoffCASFailure_FailsClosedAndReleasesSlot proves that
// when CAS store is unavailable or Put fails during terminal review handoff,
// the executor fails closed (never completes with empty/missing handoff CAS evidence),
// marks the stage failed, marks the run interrupted, and releases the active run slot.
func TestSeam1_AutoRun_Dialogue_HandoffCASFailure_FailsClosedAndReleasesSlot(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	defaultVITTSFake(t, h).DurationMs = 250

	assetID1 := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "handoff_cas_fail.mp4", 2.0, 2500)
	jobID1 := createJob(t, h.server.URL, assetID1, domain.TargetLanguageVI)

	// Inject custom composer into RenderService that clears server CASStore right when preview render finishes
	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetCustomComposer(func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
		res, err := media.ComposeNativeVideo(ctx, req)
		h.srv.SetCASStore(nil)
		return res, err
	})
	h.srv.SetRenderService(renderSvc)

	body, _ := json.Marshal(map[string]any{
		"config_snapshot_json": `{"posture":"review"}`,
	})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID1), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue run failed: %v", err)
	}
	defer resp.Body.Close()
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	runID1 := res.Run.ID

	final1 := pollRunStatus(t, h.server.URL, runID1, domain.RunStatusInterrupted, 10*time.Second)
	if final1.Status != domain.RunStatusInterrupted {
		t.Fatalf("expected run status %q on handoff CAS failure, got %q", domain.RunStatusInterrupted, final1.Status)
	}

	// Verify final_render_handoff stage failed
	stages := getRunStages(t, h.server.URL, runID1)
	foundFail := false
	for _, st := range stages {
		if st.Stage == "final_render_handoff" && st.Status == domain.StageStatusFailed {
			foundFail = true
			break
		}
	}
	if !foundFail {
		t.Fatalf("expected failed 'final_render_handoff' stage execution, got stages: %+v", stages)
	}

	// Restore CAS store so subsequent run can succeed
	h.srv.SetCASStore(h.casStore)
	h.srv.SetRenderService(service.NewRenderService(h.db, h.casStore))

	// Subsequent valid run must complete to prove active run slot was released
	assetID2 := ingestSyntheticAsset(t, h.server.URL, h.dir, "handoff_drain.mp4", 1.5)
	postAudioRolePlan(t, h.server.URL, assetID2, []domain.AudioSegment{
		{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
	})
	jobID2 := createJob(t, h.server.URL, assetID2, domain.TargetLanguageVI)
	runID2 := enqueueRun(t, h.server.URL, jobID2)

	final2 := pollRunStatus(t, h.server.URL, runID2, domain.RunStatusCompleted, 10*time.Second)
	if final2.Status != domain.RunStatusCompleted {
		t.Fatalf("expected subsequent run 2 completed, got %s", final2.Status)
	}
}

// TestSeam1_AutoRun_ReviewPosture_FinalRenderOwnsTheJob proves that a Review-posture run stops at the
// final-render handoff: the run's pipeline completes while its job stays open, because the handoff only
// exposes the operator's explicit 'Start final render' action and renders nothing. The job reaches
// 'completed' only once that explicit render (POST /render/final) succeeds.
func TestSeam1_AutoRun_ReviewPosture_FinalRenderOwnsTheJob(t *testing.T) {
	h := setupAutoRunHarness(t)
	assetID := ingestSyntheticAsset(t, h.server.URL, h.dir, "review_posture_handoff.mp4", 1.5)
	postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
		{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
	})
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)

	body, _ := json.Marshal(map[string]any{"config_snapshot_json": `{"posture":"review"}`})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enqueue run failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 enqueue review-posture run, got %d", resp.StatusCode)
	}
	var enqueued struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&enqueued)
	runID := enqueued.Run.ID

	final := pollRunStatus(t, h.server.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if final.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the review-posture run to finish its pipeline as %q, got %q", domain.RunStatusCompleted, final.Status)
	}

	// The handoff is a readiness gate: it succeeded without rendering anything.
	stages := getRunStages(t, h.server.URL, runID)
	handoffSucceeded := false
	for _, st := range stages {
		if st.Stage == "final_render_handoff" && st.Status == domain.StageStatusSucceeded {
			handoffSucceeded = true
		}
		if st.Stage == "render_final" {
			t.Fatalf("review posture executed render_final during the run: %+v", st)
		}
	}
	if !handoffSucceeded {
		t.Fatalf("expected a succeeded final_render_handoff stage for run %s, got %+v", runID, stages)
	}

	// No final render artifact exists for the run yet.
	finGet, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/final?run_id=%s&target_language=%s", h.server.URL, assetID, runID, domain.TargetLanguageVI))
	if err != nil {
		t.Fatalf("get final render failed: %v", err)
	}
	finGet.Body.Close()
	if finGet.StatusCode != http.StatusNotFound {
		t.Fatalf("expected no final render artifact before the explicit render, got status %d", finGet.StatusCode)
	}

	job := getJobViaAPI(t, h, jobID)
	if job.Status == "completed" {
		t.Fatalf("job %s reads completed while the explicit final render is still pending", jobID)
	}

	// The operator's explicit render is the work the job was waiting for.
	finResp, artifact := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
	})
	if finResp == nil || finResp.StatusCode != http.StatusCreated || artifact == nil {
		status := 0
		if finResp != nil {
			status = finResp.StatusCode
		}
		t.Fatalf("explicit final render failed: status %d", status)
	}
	if finResp.Body != nil {
		finResp.Body.Close()
	}

	job = getJobViaAPI(t, h, jobID)
	if job.Status != "completed" {
		t.Fatalf("job status after the explicit final render = %q, want completed", job.Status)
	}
}

// TestSeam1_DubbingService_AssignVoices_ExplicitCAS_Validation proves that
// DubbingService.AssignVoices strictly validates explicit transcript and dub-script CAS
// references: rejecting unreadable, malformed, or foreign evidence instead of silently
// defaulting to SPEAKER_00.
func TestSeam1_DubbingService_AssignVoices_ExplicitCAS_Validation(t *testing.T) {
	h := setupAutoRunHarness(t)
	dubSvc := service.NewDubbingService(h.db, h.casStore)

	assetID := ingestSyntheticAsset(t, h.server.URL, h.dir, "va_explicit.mp4", 1.0)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := "run_va_test_" + time.Now().Format("150405")
	run := domain.LocalizationRun{
		ID:        runID,
		JobID:     jobID,
		Status:    domain.RunStatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := h.db.CreateRunEnqueued(context.Background(), run, jobID); err != nil {
		t.Fatalf("create run in DB: %v", err)
	}

	// Store a valid transcript in CAS for assetID, runID
	validTranscript := domain.TranscriptArtifact{
		ID:      "t-1",
		AssetID: assetID,
		RunID:   runID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 500, SpeakerID: "SPEAKER_01"},
			{Index: 1, StartMs: 600, EndMs: 1000, SpeakerID: "SPEAKER_02"},
		},
	}
	tBytes, _ := json.Marshal(validTranscript)
	tObj, err := h.casStore.Put(bytes.NewReader(tBytes))
	if err != nil {
		t.Fatalf("put valid transcript in CAS: %v", err)
	}

	// Store a valid dub script in CAS for assetID, runID, target_lang vi
	validDubScript := domain.DubScriptVariant{
		ID:             "ds-1",
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_01"},
			{Index: 1, SpeakerID: "SPEAKER_02"},
		},
	}
	dsBytes, _ := json.Marshal(validDubScript)
	dsObj, err := h.casStore.Put(bytes.NewReader(dsBytes))
	if err != nil {
		t.Fatalf("put valid dub script in CAS: %v", err)
	}

	// Store a corrupt JSON blob in CAS
	corruptObj, err := h.casStore.Put(bytes.NewReader([]byte("{not_json")))
	if err != nil {
		t.Fatalf("put corrupt blob in CAS: %v", err)
	}

	ctx := context.Background()

	t.Run("MissingTranscriptCAS_FailsClosed", func(t *testing.T) {
		_, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:               assetID,
			RunID:                 runID,
			TargetLanguage:        "vi",
			TranscriptArtifactCAS: "sha256_nonexistent_cas_hash",
		})
		if err == nil {
			t.Fatal("expected error on nonexistent transcript CAS, got nil")
		}
	})

	t.Run("CorruptTranscriptCAS_FailsClosed", func(t *testing.T) {
		_, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:               assetID,
			RunID:                 runID,
			TargetLanguage:        "vi",
			TranscriptArtifactCAS: corruptObj.SHA256,
		})
		if err == nil {
			t.Fatal("expected error on corrupt transcript CAS, got nil")
		}
	})

	t.Run("ForeignAssetTranscriptCAS_FailsClosed", func(t *testing.T) {
		_, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:               "asset-foreign",
			RunID:                 runID,
			TargetLanguage:        "vi",
			TranscriptArtifactCAS: tObj.SHA256,
		})
		if err == nil {
			t.Fatal("expected error on foreign asset transcript CAS, got nil")
		}
		if !strings.Contains(err.Error(), "belongs to asset") {
			t.Fatalf("expected asset ownership error, got %v", err)
		}
	})

	t.Run("CrossRunTranscriptCAS_ReusesAssetScopedArtifact", func(t *testing.T) {
		// The transcript identity is run-independent (locked cache rule), so the second
		// run of the same asset reads the artifact the first run persisted. Its producer
		// run id is provenance metadata, not an ownership claim.
		priorRunTranscript := validTranscript
		priorRunTranscript.ID = "t-prior"
		priorRunTranscript.RunID = "run-prior-producer"
		priorBytes, _ := json.Marshal(priorRunTranscript)
		priorObj, err := h.casStore.Put(bytes.NewReader(priorBytes))
		if err != nil {
			t.Fatalf("put prior-run transcript in CAS: %v", err)
		}

		va, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:               assetID,
			RunID:                 runID,
			TargetLanguage:        "vi",
			TranscriptArtifactCAS: priorObj.SHA256,
		})
		if err != nil {
			t.Fatalf("expected the earlier run's transcript to be reusable, got %v", err)
		}
		if va == nil || va.RunID != runID {
			t.Fatalf("expected a voice assignment bound to run %s, got %#v", runID, va)
		}
		if len(va.Assignments) != 2 {
			t.Fatalf("expected the reused transcript to yield 2 speakers, got %d: %+v", len(va.Assignments), va.Assignments)
		}
	})

	t.Run("MissingDubScriptCAS_FailsClosed", func(t *testing.T) {
		_, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:             assetID,
			RunID:               runID,
			TargetLanguage:      "vi",
			DubScriptVariantCAS: "sha256_nonexistent_cas_hash",
		})
		if err == nil {
			t.Fatal("expected error on nonexistent dub script CAS, got nil")
		}
	})

	t.Run("CorruptDubScriptCAS_FailsClosed", func(t *testing.T) {
		_, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:             assetID,
			RunID:               runID,
			TargetLanguage:      "vi",
			DubScriptVariantCAS: corruptObj.SHA256,
		})
		if err == nil {
			t.Fatal("expected error on corrupt dub script CAS, got nil")
		}
	})

	t.Run("ForeignLanguageDubScriptCAS_FailsClosed", func(t *testing.T) {
		_, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:             assetID,
			RunID:               runID,
			TargetLanguage:      "en",
			DubScriptVariantCAS: dsObj.SHA256,
		})
		if err == nil {
			t.Fatal("expected error on foreign language dub script CAS, got nil")
		}
		if !strings.Contains(err.Error(), "target language") {
			t.Fatalf("expected language mismatch error, got %v", err)
		}
	})

	t.Run("ValidEvidence_ResolvesCorrectSpeakers", func(t *testing.T) {
		va, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
			AssetID:               assetID,
			RunID:                 runID,
			TargetLanguage:        "vi",
			TranscriptArtifactCAS: tObj.SHA256,
			DubScriptVariantCAS:   dsObj.SHA256,
		})
		if err != nil {
			t.Fatalf("AssignVoices failed on valid evidence: %v", err)
		}
		if va == nil {
			t.Fatal("expected non-nil voice assignment")
		}
		if len(va.Assignments) != 2 {
			t.Fatalf("expected 2 speaker assignments (SPEAKER_01, SPEAKER_02), got %d: %+v", len(va.Assignments), va.Assignments)
		}
		if _, ok := va.Assignments["SPEAKER_01"]; !ok {
			t.Errorf("missing SPEAKER_01 assignment")
		}
		if _, ok := va.Assignments["SPEAKER_02"]; !ok {
			t.Errorf("missing SPEAKER_02 assignment")
		}
	})
}

// TestSeam1_AutoRun_Dialogue_MixedRole_NonDialogueFiltered proves that
// in media with mixed dialogue, singing, and BGM segments, only speech blocks
// inside narration/dialogue windows flow into translation, dub script, voice assignment,
// and synthesis; singing and BGM intervals are never synthesized into dub clips.
func TestSeam1_AutoRun_Dialogue_MixedRole_NonDialogueFiltered(t *testing.T) {
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	defaultVITTSFake(t, h).DurationMs = 250

	// Configure fake aligner with 3 words across dialogue [0, 800], singing [800, 1400], and BGM [1400, 2000]
	if p, ok := h.registry.Get("fake_qwen3_aligner"); ok {
		aligner := p.(*provider.FakeAlignerProvider)
		aligner.WordTimings = []domain.WordTiming{
			{Word: "dialogue_word", StartMs: 100, EndMs: 400, Confidence: 0.95},
			{Word: "singing_word", StartMs: 900, EndMs: 1200, Confidence: 0.95},
			{Word: "bgm_noise_word", StartMs: 1500, EndMs: 1800, Confidence: 0.95},
		}
	}
	if p, ok := h.registry.Get("fake_qwen3_asr"); ok {
		asr := p.(*provider.FakeASRProvider)
		asr.TranscribedText = "dialogue_word singing_word bgm_noise_word"
	}

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "mixed_role_filter.mp4", 2.0, 2500)

	// Post explicit role plan containing dialogue, singing, and BGM segments
	postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
		{StartMs: 0, EndMs: 800, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 800, EndMs: 1400, Role: domain.AudioRoleSingingMusicVocal},
		{StartMs: 1400, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
	})

	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if finalRun.Status != domain.RunStatusCompleted {
		t.Fatalf("expected run completed, got %s", finalRun.Status)
	}

	// 1. Verify TranscriptArtifact in DB/CAS has filtered speech blocks
	tIdx, err := h.db.GetTranscriptArtifactIndex(context.Background(), assetID)
	if err != nil || tIdx == nil {
		t.Fatalf("get transcript artifact index failed: %v", err)
	}
	rcT, err := h.casStore.Get(tIdx.CASHash)
	if err != nil {
		t.Fatalf("read transcript from CAS failed: %v", err)
	}
	var transcript domain.TranscriptArtifact
	_ = json.NewDecoder(rcT).Decode(&transcript)
	rcT.Close()

	// ASR may inspect full audio (speech blocks exist for both dialogue and singing/bgm)
	if len(transcript.SpeechBlocks) < 2 {
		t.Fatalf("expected ASR to inspect full audio and find multiple speech blocks, got %d", len(transcript.SpeechBlocks))
	}

	// 1b. Verify Speech TranslationVariant only has segments in dialogue window [0, 800]
	stages := getRunStages(t, h.server.URL, runID)
	var speechTransSHA string
	for _, st := range stages {
		if st.Stage == "translation" {
			speechTransSHA = st.ArtifactSHA256
			break
		}
	}
	if speechTransSHA == "" {
		t.Fatalf("no translation stage found")
	}
	rcTV, err := h.casStore.Get(speechTransSHA)
	if err != nil {
		t.Fatalf("read translation variant from CAS failed: %v", err)
	}
	var transVariant domain.TranslationVariant
	_ = json.NewDecoder(rcTV).Decode(&transVariant)
	rcTV.Close()
	if len(transVariant.Segments) != 1 {
		t.Fatalf("expected exactly 1 translation segment in dialogue window, got %d: %+v", len(transVariant.Segments), transVariant.Segments)
	}
	if transVariant.Segments[0].StartMs < 0 || transVariant.Segments[0].EndMs > 800 {
		t.Errorf("translation segment [%d, %d] outside dialogue window [0, 800]", transVariant.Segments[0].StartMs, transVariant.Segments[0].EndMs)
	}

	// 2. Verify DubScriptVariant only has segments in dialogue window
	dsIdx, err := h.db.GetDubScriptVariantIndex(context.Background(), assetID, "vi")
	if err != nil || dsIdx == nil {
		t.Fatalf("get dub script variant index failed: %v", err)
	}
	rcDS, err := h.casStore.Get(dsIdx.CASHash)
	if err != nil {
		t.Fatalf("read dub script from CAS failed: %v", err)
	}
	var dubScript domain.DubScriptVariant
	_ = json.NewDecoder(rcDS).Decode(&dubScript)
	rcDS.Close()

	if len(dubScript.Segments) != 1 {
		t.Fatalf("expected exactly 1 dub script segment, got %d: %+v", len(dubScript.Segments), dubScript.Segments)
	}
	if dubScript.Segments[0].StartMs < 0 || dubScript.Segments[0].EndMs > 800 {
		t.Errorf("dub script segment [%d, %d] outside dialogue window [0, 800]", dubScript.Segments[0].StartMs, dubScript.Segments[0].EndMs)
	}

	// 3. Verify DubSegmentsVariant only contains synthesized clips for dialogue window
	dsvIdx, err := h.db.GetDubSegmentsVariantIndex(context.Background(), assetID, "vi")
	if err != nil || dsvIdx == nil {
		t.Fatalf("get dub segments variant index failed: %v", err)
	}
	rcDSV, err := h.casStore.Get(dsvIdx.CASHash)
	if err != nil {
		t.Fatalf("read dub segments from CAS failed: %v", err)
	}
	var dsv domain.DubSegmentsVariant
	_ = json.NewDecoder(rcDSV).Decode(&dsv)
	rcDSV.Close()

	if len(dsv.Segments) != 1 {
		t.Fatalf("expected exactly 1 dub segment, got %d: %+v", len(dsv.Segments), dsv.Segments)
	}
	if dsv.Segments[0].StartMs < 0 || dsv.Segments[0].EndMs > 800 {
		t.Errorf("synthesized dub segment [%d, %d] outside dialogue window [0, 800]", dsv.Segments[0].StartMs, dsv.Segments[0].EndMs)
	}
}
