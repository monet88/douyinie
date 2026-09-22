package seam1_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

// Issue #93 — ZeroTTS is the unattended Vietnamese default through RuntimeHost.
//
// These tests drive the existing #84 contracts (voice catalog, audition,
// assignment, reassignment and measured-duration synthesis) over Seam 1 and
// cover: VI default rotation, EN compatibility, ZeroTTS audition, actual WAV
// duration authority, overrun rejection without a fake speed resynthesis, and
// historical VieNeu assignment replay.

// defaultVITTSFake returns the fake standing in for the unattended Vietnamese
// default lane, so fixtures control the lane the pipeline actually routes to.
func defaultVITTSFake(t *testing.T, h *testHarness) *provider.FakeTTSProvider {
	t.Helper()
	p, ok := h.registry.Get("fake_zerotts_tts_vi")
	if !ok {
		t.Fatalf("fake_zerotts_tts_vi not found in registry")
	}
	return p.(*provider.FakeTTSProvider)
}

// setupDubScriptForSeam1Lang mirrors setupDubScriptForSeam1 for an explicit target language.
func setupDubScriptForSeam1Lang(t *testing.T, h *testHarness, runID, assetID, targetLang string, segments []domain.TranslationInputSegment) *domain.DubScriptVariant {
	t.Helper()
	pinSeam1TranscriptForSegments(t, h, runID, assetID, segments)

	planBody, _ := json.Marshal(map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 20000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	defer planResp.Body.Close()
	if planResp.StatusCode != http.StatusOK && planResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200 or 201 for audio role plan, got %d", planResp.StatusCode)
	}

	respTrans, transVariant := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": targetLang,
		"segments":        segments,
	})
	if respTrans.StatusCode != http.StatusCreated || transVariant == nil {
		t.Fatalf("translation failed: status %d", respTrans.StatusCode)
	}

	respDub, dubVariant := runDubScript(t, h, assetID, map[string]any{
		"run_id":                  runID,
		"target_language":         targetLang,
		"translation_variant_cas": transVariant.CASHash,
	})
	if respDub.StatusCode != http.StatusCreated || dubVariant == nil {
		t.Fatalf("dub script adaptation failed: status %d", respDub.StatusCode)
	}
	return dubVariant
}

// runAudition posts a voice-audition request and returns the playable payload.
func runAudition(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("voice-audition request failed: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	resp.Body.Close()
	return resp, decoded
}

// ---------------------------------------------------------------------------
// 1. VI default assignment uses the approved ZeroTTS unattended rotation.
// ---------------------------------------------------------------------------
func TestSeam1_VI_DefaultVoiceAssignment_ZeroTTSUnattendedRotation(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_01", StartMs: 2200, EndMs: 4200, SourceText: "我们去公园散步吧。"},
	}
	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", segments)

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	spk0, ok0 := assign.Assignments["SPEAKER_00"]
	spk1, ok1 := assign.Assignments["SPEAKER_01"]
	if !ok0 || !ok1 {
		t.Fatalf("missing speaker assignments: %+v", assign.Assignments)
	}
	if spk0.ProviderID != provider.ZeroTTSProviderID || spk0.VoiceID != "quangminh" {
		t.Fatalf("expected SPEAKER_00 -> ZeroTTS quangminh, got %s/%s", spk0.ProviderID, spk0.VoiceID)
	}
	if spk1.ProviderID != provider.ZeroTTSProviderID || spk1.VoiceID != "maichi" {
		t.Fatalf("expected SPEAKER_01 -> ZeroTTS maichi, got %s/%s", spk1.ProviderID, spk1.VoiceID)
	}
	// Verified presets outside the unattended rotation must never be auto-assigned.
	verifiedOutsideRotation := provider.ZeroTTSPresetVoices()[provider.DefaultVIUnattendedVoiceCount:]
	for _, spk := range []domain.VoiceProfile{spk0, spk1} {
		for _, preset := range verifiedOutsideRotation {
			if spk.VoiceID == preset.VoiceID {
				t.Fatalf("unattended default must not select out-of-rotation preset %s", preset.VoiceID)
			}
		}
	}
	if assign.Distinguishability == nil || assign.Distinguishability.Status != "PASS" {
		t.Fatalf("expected distinguishability PASS for the unattended rotation, got %+v", assign.Distinguishability)
	}

	// Dub synthesis routes the unattended assignment through the ZeroTTS lane only.
	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if len(dubSegments.Segments) != 2 {
		t.Fatalf("expected 2 selected segments, got %d (status %s)", len(dubSegments.Segments), dubSegments.OverallStatus)
	}
	for _, seg := range dubSegments.Segments {
		if seg.Voice.ProviderID != provider.ZeroTTSProviderID {
			t.Fatalf("selected segment %d synthesized on %s, expected %s", seg.Index, seg.Voice.ProviderID, provider.ZeroTTSProviderID)
		}
	}
	if defaultVITTSFake(t, h).Invocations != 2 {
		t.Fatalf("expected exactly 2 ZeroTTS invocations, got %d", defaultVITTSFake(t, h).Invocations)
	}
}

// ---------------------------------------------------------------------------
// 2. English keeps the existing Kokoro rotation and lane.
// ---------------------------------------------------------------------------
func TestSeam1_EN_DefaultVoiceAssignment_KeepsKokoroLane(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
	}
	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "en", segments)

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "en",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("EN voice assignment failed: %d", respAssign.StatusCode)
	}
	spk0 := assign.Assignments["SPEAKER_00"]
	if spk0.ProviderID != provider.KokoroProviderID || spk0.VoiceID != "af_heart" {
		t.Fatalf("expected EN default SPEAKER_00 -> Kokoro af_heart, got %s/%s", spk0.ProviderID, spk0.VoiceID)
	}
	// The ZeroTTS provider is Vietnamese-only and must never leak into EN routing.
	if spk0.ProviderID == provider.ZeroTTSProviderID {
		t.Fatalf("EN assignment must not route to the ZeroTTS VI lane")
	}

	kokoro, ok := h.registry.Get("fake_kokoro_tts_en")
	if !ok {
		t.Fatalf("fake_kokoro_tts_en not found")
	}
	kokoroProv := kokoro.(*provider.FakeTTSProvider)
	kokoroBefore := kokoroProv.Invocations

	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "en",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("EN dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if kokoroProv.Invocations-kokoroBefore != 1 {
		t.Fatalf("expected 1 Kokoro invocation for the EN default lane, got %d", kokoroProv.Invocations-kokoroBefore)
	}
	if defaultVITTSFake(t, h).Invocations != 0 {
		t.Fatalf("EN synthesis must not invoke the ZeroTTS VI lane, got %d invocations", defaultVITTSFake(t, h).Invocations)
	}
}

// ---------------------------------------------------------------------------
//  3. Audition: verified ZeroTTS presets outside the unattended rotation stay
//     selectable and return playable audio without leaking filesystem paths.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_AuditionOutsideUnattendedRotation(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	presets := provider.ZeroTTSPresetVoices()
	if len(presets) <= provider.DefaultVIUnattendedVoiceCount {
		t.Fatalf("expected verified presets beyond the unattended rotation")
	}
	manual := presets[provider.DefaultVIUnattendedVoiceCount]

	// Audio role plan with dub-eligible dialogue is required for audition.
	planBody, _ := json.Marshal(map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	planResp, _ := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	planResp.Body.Close()

	resp, payload := runAudition(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"voice":           manual,
		"sample_text":     "Thử nghiệm giọng đọc trước khi lồng tiếng toàn bộ.",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for ZeroTTS preset audition, got %d (%v)", resp.StatusCode, payload)
	}
	audioURL, _ := payload["audio_data_url"].(string)
	if len(audioURL) < len("data:audio/wav;base64,") || audioURL[:len("data:audio/wav;base64,")] != "data:audio/wav;base64," {
		t.Fatalf("audition must return browser-playable audio, got %q", audioURL)
	}
	audition, _ := payload["audition_result"].(map[string]any)
	if audition == nil {
		t.Fatalf("audition_result missing from response: %v", payload)
	}
	// The harness substitutes a fake for the production lane, so accept the
	// registered fixture identity while pinning it to the ZeroTTS provider.
	if got, _ := audition["provider_id"].(string); strings.TrimPrefix(got, "fake_") != provider.ZeroTTSProviderID {
		t.Fatalf("expected audition provider %s, got %q", provider.ZeroTTSProviderID, got)
	}
	if casPath, _ := audition["audio_cas_path"].(string); casPath != "" {
		t.Fatalf("audition must not expose a local filesystem path, got %q", casPath)
	}
	if ms, _ := audition["measured_duration_ms"].(float64); ms <= 0 {
		t.Fatalf("expected positive measured duration from the probed artifact, got %v", audition["measured_duration_ms"])
	}
}

// ---------------------------------------------------------------------------
// 4. Actual synthesized WAV duration is authoritative over predicted duration.
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_ActualWAVDurationIsAuthoritative(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 3000, SourceText: "今天天气很好。"},
	}
	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", segments)

	fakeTTS := defaultVITTSFake(t, h)
	fakeTTS.DurationMs = 1200
	fakeTTS.CustomPredictedMs = 300 // planning estimate lies about the true media

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if len(dubSegments.Segments) != 1 {
		t.Fatalf("expected 1 selected segment, got %d (status %s)", len(dubSegments.Segments), dubSegments.OverallStatus)
	}
	if got := dubSegments.Segments[0].MeasuredDurationMs; got != 1200 {
		t.Fatalf("expected probed duration 1200ms to be authoritative, got %dms", got)
	}
	if len(dubSegments.FitPlans) != 1 || dubSegments.FitPlans[0].MeasuredDurationMs != 1200 {
		t.Fatalf("fit plan must record the probed 1200ms duration, got %+v", dubSegments.FitPlans)
	}
}

// ---------------------------------------------------------------------------
//  5. A ZeroTTS overrun cannot enter the selected set and never takes a
//     speed-resynthesis path (the lane fails closed on any non-1.0 speed).
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_OverrunRejectedWithoutSpeedResynthesis(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1500, SourceText: "今天天气很好。"},
	}
	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", segments)

	// 1650ms into a 1500ms slot: the ~10% band that a speed nudge would target.
	fakeTTS := defaultVITTSFake(t, h)
	fakeTTS.DurationMs = 1650

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign.CASHash,
	})
	// A speed-resynthesis attempt would have failed closed with
	// ErrTTSSpeedUnsupported; the fixed-rate lane must remediate by rewrite/review.
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("ZeroTTS overrun must resolve through rewrite/review, got status %d", respSynth.StatusCode)
	}
	if dubSegments.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("expected REVIEW_REQUIRED for the overrun, got %s", dubSegments.OverallStatus)
	}
	if len(dubSegments.Segments) != 0 {
		t.Fatalf("an overrunning ZeroTTS candidate must not enter the selected set, got %d segments", len(dubSegments.Segments))
	}
	if len(dubSegments.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment, got %d", len(dubSegments.ReviewSegments))
	}
	rev := dubSegments.ReviewSegments[0]
	if rev.ReviewReason != "DURATION_OVERRUN" {
		t.Fatalf("expected DURATION_OVERRUN, got %s", rev.ReviewReason)
	}
	if rev.MeasuredDurationMs <= rev.SlotDurationMs {
		t.Fatalf("expected measured %dms > slot %dms", rev.MeasuredDurationMs, rev.SlotDurationMs)
	}
	if rev.Voice.ProviderID != provider.ZeroTTSProviderID {
		t.Fatalf("expected the review candidate on the ZeroTTS lane, got %s", rev.Voice.ProviderID)
	}
}

// ---------------------------------------------------------------------------
//  6. Historical VieNeu assignments keep their provider/voice identity, remain
//     routable, and are not rewritten by the new default rotation.
//
// ---------------------------------------------------------------------------
func TestSeam1_HistoricalVieNeuAssignment_RemainsRoutableAndUnrewritten(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
	}
	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", segments)

	historicalVoice := provider.VieNeuPresetVoices()[0]
	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": historicalVoice,
		},
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("historical VieNeu assignment failed: %d", respAssign.StatusCode)
	}
	frozen := assign.Assignments["SPEAKER_00"]
	if frozen.ProviderID != provider.VieNeuProviderID || frozen.VoiceID != historicalVoice.VoiceID {
		t.Fatalf("historical assignment identity was rewritten: %s/%s", frozen.ProviderID, frozen.VoiceID)
	}

	vieneu, ok := h.registry.Get("fake_vieneu_tts_vi")
	if !ok {
		t.Fatalf("fake_vieneu_tts_vi not found")
	}
	vieneuProv := vieneu.(*provider.FakeTTSProvider)
	vieneuBefore := vieneuProv.Invocations

	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("historical VieNeu replay failed: %d", respSynth.StatusCode)
	}
	if len(dubSegments.Segments) != 1 {
		t.Fatalf("expected 1 selected segment on replay, got %d", len(dubSegments.Segments))
	}
	if got := dubSegments.Segments[0].Voice; got.ProviderID != provider.VieNeuProviderID || got.VoiceID != historicalVoice.VoiceID {
		t.Fatalf("replayed segment lost its frozen voice identity: %s/%s", got.ProviderID, got.VoiceID)
	}
	if vieneuProv.Invocations-vieneuBefore != 1 {
		t.Fatalf("expected the frozen VieNeu provider to be invoked once, got %d", vieneuProv.Invocations-vieneuBefore)
	}
	if defaultVITTSFake(t, h).Invocations != 0 {
		t.Fatalf("historical VieNeu replay must not be rerouted to the ZeroTTS default lane")
	}

	// The frozen assignment is immutable: reading it back yields the same identity.
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/voice-assignment?target_language=vi&run_id=" + runID)
	if err != nil {
		t.Fatalf("GET voice-assignment failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for frozen assignment read-back, got %d", getResp.StatusCode)
	}
	var stored struct {
		Assignment domain.VoiceAssignment `json:"voice_assignment"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&stored); err != nil {
		t.Fatalf("decode stored assignment: %v", err)
	}
	if stored.Assignment.CASHash != assign.CASHash {
		t.Fatalf("frozen assignment CAS changed: %s vs %s", stored.Assignment.CASHash, assign.CASHash)
	}
	storedVoice := stored.Assignment.Assignments["SPEAKER_00"]
	if storedVoice.ProviderID != provider.VieNeuProviderID || storedVoice.VoiceID != historicalVoice.VoiceID {
		t.Fatalf("stored assignment was rewritten in place: %s/%s", storedVoice.ProviderID, storedVoice.VoiceID)
	}
}
