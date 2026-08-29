package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
)

// Helper to run translation and dub-script adaptation to produce DubScriptVariant
func setupDubScriptForSeam1(t *testing.T, h *testHarness, runID, assetID string, segments []domain.TranslationInputSegment) (*domain.TranslationVariant, *domain.DubScriptVariant) {
	t.Helper()

	// 1. Audio role plan with narration/dialogue
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 20000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	defer planResp.Body.Close()
	if planResp.StatusCode != http.StatusOK && planResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200 or 201 for audio role plan, got %d", planResp.StatusCode)
	}

	transReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments":        segments,
	}
	respTrans, transVariant := runTranslation(t, h, assetID, transReq)
	if respTrans.StatusCode != http.StatusCreated || transVariant == nil {
		t.Fatalf("translation failed: status %d", respTrans.StatusCode)
	}

	// 3. DubScript
	dubReq := map[string]any{
		"run_id":                  runID,
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	}
	respDub, dubVariant := runDubScript(t, h, assetID, dubReq)
	if respDub.StatusCode != http.StatusCreated || dubVariant == nil {
		t.Fatalf("dub script adaptation failed: status %d", respDub.StatusCode)
	}

	return transVariant, dubVariant
}

func runAssignVoices(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.VoiceAssignment) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-assignment", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("voice-assignment request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var res struct {
			Assignment domain.VoiceAssignment `json:"voice_assignment"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Assignment
	}
	return resp, nil
}

func runDubSynthesize(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.DubSegmentsVariant) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/dub-synthesize", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("dub-synthesize request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var res struct {
			Variant domain.DubSegmentsVariant `json:"dub_segments_variant"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Variant
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Test 1: TTS Overflow Gate — Overlong measured candidate is rejected from selection
// and cannot reach mixer without operator review / override.
// ---------------------------------------------------------------------------
func TestSeam1_TTSOverflowGate_OverlongCandidateFlaggedForReview(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      1500, // 1500ms slot
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// 1. Assign voices
	respAssign, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || voiceAssign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	// 2. Force fake provider to emit 2500ms audio (severe overrun vs 1500ms slot)
	fakeTTS, ok := h.registry.Get("fake_vieneu_tts_vi")
	if !ok {
		t.Fatalf("fake_vieneu_tts_vi not found")
	}
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 2500

	// 3. Synthesize
	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}
	// 4. Invariant: Overlong candidate cannot be marked as PASS and cannot reach mixer (Segments must be empty)
	if dubSegments.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("expected OverallStatus REVIEW_REQUIRED, got %s", dubSegments.OverallStatus)
	}
	if len(dubSegments.Segments) != 0 {
		t.Fatalf("overlong candidate MUST NOT be in selected Segments (mixer inputs), got %d segments", len(dubSegments.Segments))
	}
	if len(dubSegments.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment recorded, got %d", len(dubSegments.ReviewSegments))
	}

	revSeg := dubSegments.ReviewSegments[0]
	if revSeg.ReviewReason != "DURATION_OVERRUN" {
		t.Fatalf("expected ReviewReason DURATION_OVERRUN, got %s", revSeg.ReviewReason)
	}
	if revSeg.MeasuredDurationMs <= revSeg.SlotDurationMs {
		t.Fatalf("expected measured duration %dms > slot %dms", revSeg.MeasuredDurationMs, revSeg.SlotDurationMs)
	}
}

// ---------------------------------------------------------------------------
// Test 2: No Adjacent Speech Collision & No Cumulative Anchor Drift
// ---------------------------------------------------------------------------
func TestSeam1_TTS_NoAdjacentCollisionAndNoAnchorDrift(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 3 distinct turns with immutable source timing anchors
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      2000,
		},
		{
			Index:      1,
			SourceText: "我们去公园散步吧。",
			SpeakerID:  "SPEAKER_01",
			StartMs:    2500,
			EndMs:      4500,
		},
		{
			Index:      2,
			SourceText: "明天再继续工作。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    5000,
			EndMs:      7000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// 1. Assign voices
	respAssign, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || voiceAssign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	// 2. Normal fitting durations (1400ms <= 2000ms slot)
	fakeTTS, _ := h.registry.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 1400

	// 3. Synthesize
	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}

	if dubSegments.OverallStatus != "PASS" {
		t.Fatalf("expected OverallStatus PASS, got %s", dubSegments.OverallStatus)
	}
	if len(dubSegments.Segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(dubSegments.Segments))
	}

	// Verify immutable anchors and zero collision
	for i, seg := range dubSegments.Segments {
		expectedStart := segments[i].StartMs
		expectedEnd := segments[i].EndMs
		if seg.StartMs != expectedStart || seg.EndMs != expectedEnd {
			t.Fatalf("anchor drift on segment %d: expected [%d, %d], got [%d, %d]", i, expectedStart, expectedEnd, seg.StartMs, seg.EndMs)
		}
		if seg.MeasuredDurationMs > seg.SlotDurationMs {
			t.Fatalf("overrun on segment %d: measured %dms > slot %dms", i, seg.MeasuredDurationMs, seg.SlotDurationMs)
		}
		if i+1 < len(dubSegments.Segments) {
			next := dubSegments.Segments[i+1]
			finishMs := seg.StartMs + seg.MeasuredDurationMs
			if finishMs > next.StartMs {
				t.Fatalf("collision: segment %d finish %dms > segment %d start %dms", i, finishMs, i+1, next.StartMs)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Test 3: VoiceAssignment Frozen Per Run / Speaker (No Engine Hopping)
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAssignmentFrozenPerRun(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Setup audio role plan
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

	// 1. Assign voices
	resp1, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if resp1.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("first voice assignment failed: %d", resp1.StatusCode)
	}

	// 2. Fetch frozen voice assignment via GET
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/voice-assignment?target_language=vi")
	if err != nil {
		t.Fatalf("GET voice assignment failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for GET voice assignment, got %d", getResp.StatusCode)
	}

	var fetched struct {
		Assignment domain.VoiceAssignment `json:"voice_assignment"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&fetched)
	if fetched.Assignment.CASHash != assign1.CASHash {
		t.Fatalf("expected matching CASHash %s, got %s", assign1.CASHash, fetched.Assignment.CASHash)
	}
	if fetched.Assignment.FrozenAt.IsZero() {
		t.Fatalf("expected non-zero FrozenAt timestamp")
	}
}

// ---------------------------------------------------------------------------
// Test 4: Actual Synthesized-Duration Probe Required (Predicted is Planning Only)
// ---------------------------------------------------------------------------
func TestSeam1_TTS_ActualSynthesizedDurationProbed(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      2000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// Setup fake provider where predicted duration is 1200ms but probed waveform duration is 1800ms
	fakeTTS, _ := h.registry.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 1800
	fakeProv.CustomPredictedMs = 1200 // planning estimate lies / deviates

	_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})

	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}

	seg := dubSegments.Segments[0]
	// Measured duration must be the true probed 1800ms, not the fake predicted 1200ms
	if seg.MeasuredDurationMs != 1800 {
		t.Fatalf("expected measured probed duration 1800ms, got %dms", seg.MeasuredDurationMs)
	}
}

// ---------------------------------------------------------------------------
// Test 5: Multi-pass Speed-Fit Lane (CosyVoice3 measured-duration lane simulation)
// ---------------------------------------------------------------------------
func TestSeam1_TTS_MeasuredDurationSpeedFitLane(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "测试语音合成调速。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      1500, // slot 1500ms
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	// Register CosyVoice3 provider and license manifest
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", 1700)
	fakeCosy.SpeedFitEnabled = true
	_ = h.registry.Register(fakeCosy)

	licBody, _ := json.Marshal(domain.LicenseManifestEntry{
		DependencyName: "fake_cosyvoice3_tts",
		Version:        "1.0.0",
		SHA256:         "sha256_mock_fake_cosyvoice3_tts",
		SourceRepo:     "github.com/monet88/douyinie/models/fake_cosyvoice3_tts",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	licResp, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licBody))
	if err != nil || licResp.StatusCode != http.StatusCreated {
		t.Fatalf("register license manifest for cosyvoice3 failed: %v", err)
	}
	licResp.Body.Close()

	// Assign CosyVoice3 voice profile with speed-fit capability
	cosyVoice := domain.VoiceProfile{
		ID:         "cosyvoice3_vi_female_1",
		ProviderID: "fake_cosyvoice3_tts",
		VoiceID:    "cosy_vi_f1",
		Name:       "CosyVoice3 VI",
		Language:   "vi",
	}

	_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": cosyVoice,
		},
	})
	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}

	if dubSegments.OverallStatus != "PASS" {
		t.Fatalf("expected OverallStatus PASS after speed-fit, got %s", dubSegments.OverallStatus)
	}

	seg := dubSegments.Segments[0]
	if seg.MeasuredDurationMs > seg.SlotDurationMs {
		t.Fatalf("expected fitted duration <= %dms, got %dms", seg.SlotDurationMs, seg.MeasuredDurationMs)
	}
}

// ---------------------------------------------------------------------------
// Test 6: Pre-Dub Voice Audition Endpoint
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAuditionEndpoint(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Setup audio role plan
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

	auditionPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"voice": domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_female_natural",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
		"sample_text": "Thử nghiệm giọng đọc trước khi lồng tiếng toàn bộ.",
	}
	bodyBytes, _ := json.Marshal(auditionPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for voice-audition, got %d", resp.StatusCode)
	}

	var res struct {
		Audition domain.VoiceAuditionResult `json:"audition_result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode audition result: %v", err)
	}

	if res.Audition.MeasuredDurationMs <= 0 {
		t.Fatalf("expected positive measured duration on audition, got %d", res.Audition.MeasuredDurationMs)
	}
	if res.Audition.AudioCASHash == "" {
		t.Fatalf("expected populated AudioCASHash")
	}

	// Non-existent asset ID must fail with 404 Not Found
	nonExistentResp, err := http.Post(h.server.URL+"/api/v1/assets/non_existent_asset_123/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition with non-existent asset failed: %v", err)
	}
	defer nonExistentResp.Body.Close()
	if nonExistentResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for audition with non-existent asset, got %d", nonExistentResp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 7: Policy-Before-Health Invariant — Blocked provider (IndexTTS2) cannot be routed
// ---------------------------------------------------------------------------
func TestSeam1_TTS_PolicyBlockedProviderCannotBeRouted(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      2000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// Register an otherwise healthy/capable blocked TTS provider (IndexTTS2-like)
	blockedProv := provider.NewFakeTTSProvider("fake_indextts2_blocked", 1500)
	blockedProv.Policy = provider.PolicyBlocked
	blockedProv.Healthy = true
	blockedProv.Cap.QualityScore = 0.99
	_ = h.registry.Register(blockedProv)

	// Attempt to assign blocked IndexTTS2 voice
	blockedVoice := domain.VoiceProfile{
		ID:         "indextts2_clone_voice",
		ProviderID: "fake_indextts2_blocked",
		VoiceID:    "unauthorized_clone",
		Name:       "IndexTTS2 Blocked",
		Language:   "vi",
	}

	_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": blockedVoice,
		},
	})

	// When dub-synthesize runs, router must reject the blocked provider (fail-closed policy before health/ranking)
	respSynth, _ := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})

	// Must fail closed (cannot synthesize with blocked provider)
	if respSynth.StatusCode == http.StatusCreated {
		t.Fatalf("CRITICAL POLICY VIOLATION: synthesis succeeded with policy-blocked provider fake_indextts2_blocked!")
	}
}

// ---------------------------------------------------------------------------
// Test 7b: Seam 1 Regroup Same-Speaker Turns & No Engine Hopping
// ---------------------------------------------------------------------------
func TestSeam1_TTS_Regroup_SameSpeakerPair(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 2 adjacent same-speaker segments:
	// Seg 0 has 500ms slot (overruns on 800ms speech)
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      500,
		},
		{
			Index:      1,
			SourceText: "我们去公园散步吧。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    600,
			EndMs:      2500,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	fakeTTS, _ := h.registry.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 800

	_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})

	respSynth, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || dubSegments == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}

	if len(dubSegments.Segments) != 1 {
		t.Fatalf("expected 1 regrouped segment, got %d", len(dubSegments.Segments))
	}
	reg := dubSegments.Segments[0]
	if len(reg.SpeechBlockIndices) != 2 || reg.StartMs != 0 || reg.EndMs != 2500 {
		t.Errorf("expected regrouped segment spanning [0, 2500] covering 2 blocks, got %+v", reg)
	}
	if dubSegments.CreatedAt.IsZero() {
		t.Errorf("expected non-zero CreatedAt timestamp on DubSegmentsVariant")
	}
}

func TestSeam1_TTS_NoSentenceBySentenceEngineHopping(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      3000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// Register an alternative viable provider with full license manifest and healthy state
	altTTS := provider.NewFakeTTSProvider("fake_alt_tts_vi", 1500)
	licSvc := governance.NewLicenseService(h.db)
	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		DependencyName: altTTS.ModelName,
		Version:        altTTS.ModelVersion,
		SHA256:         "sha256_mock_" + altTTS.ModelName,
		SourceRepo:     "github.com/monet88/douyinie/models/" + altTTS.ModelName,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	_ = h.registry.Register(altTTS)
	_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vieneu_voice",
				ProviderID: "fake_vieneu_tts_vi",
				VoiceID:    "vi_natural",
				Language:   "vi",
			},
		},
	})

	// Make the primary provider fail
	fakeTTS, _ := h.registry.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.InjectError = errors.New("primary provider synthesis failure")

	respSynth, _ := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})

	// Synthesis must fail closed (500 / 503 error), and NOT hop to fake_alt_tts_vi
	if respSynth.StatusCode == http.StatusCreated {
		t.Fatalf("expected dub-synthesize to fail when frozen provider failed, but it succeeded (engine hopped!)")
	}

	// Invariant: Alternative viable provider MUST NOT have been invoked
	if altTTS.Invocations != 0 {
		t.Fatalf("CRITICAL ENGINE-HOPPING VIOLATION: fake_alt_tts_vi was invoked %d times!", altTTS.Invocations)
	}
}

// ---------------------------------------------------------------------------
// Test 8: Cross-Run Voice Assignment Binding & Cache Reusability
// ---------------------------------------------------------------------------
func TestSeam1_TTS_CrossRunVoiceAssignmentIsolation(t *testing.T) {
	h := setupHarness(t)
	jobID, runID1 := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Create run 2 for same job
	runResp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/runs", "application/json", nil)
	if err != nil || runResp.StatusCode != http.StatusCreated {
		t.Fatalf("create run 2 failed: %v", err)
	}
	var run2 struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(runResp.Body).Decode(&run2)
	runID2 := run2.Run.ID

	// 1. Assign voices for run 1
	resp1, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID1,
		"target_language": "vi",
	})
	if resp1.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("assign voices run 1 failed: %d", resp1.StatusCode)
	}
	if assign1.RunID != runID1 {
		t.Fatalf("expected assign1.RunID=%s, got %s", runID1, assign1.RunID)
	}

	// 2. Assign voices for run 2 with identical settings (cache hit on provenance)
	resp2, assign2 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID2,
		"target_language": "vi",
	})
	if resp2.StatusCode != http.StatusCreated || assign2 == nil {
		t.Fatalf("assign voices run 2 failed: %d", resp2.StatusCode)
	}

	// Invariant: provenance hash is identical (locked cache rule: no RunID in provenance hash)
	if assign2.ProvenanceHash != assign1.ProvenanceHash {
		t.Fatalf("expected identical ProvenanceHash across runs, got %s vs %s", assign1.ProvenanceHash, assign2.ProvenanceHash)
	}
	// Invariant: returned VoiceAssignment is bound to runID2
	if assign2.RunID != runID2 {
		t.Fatalf("expected assign2.RunID=%s, got %s", runID2, assign2.RunID)
	}
}

// ---------------------------------------------------------------------------
// Test 9: Same-Run Voice Assignment Immutability & Re-assignment Rejection (409)
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAssignment_SameRun_ConflictingReassignmentReturns409(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Initial VoiceAssignment for Run
	resp1, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if resp1.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", resp1.StatusCode)
	}

	// 2. Equivalent re-assignment on the same run succeeds idempotently (200/201)
	respIdempotent, assignIdempotent := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if (respIdempotent.StatusCode != http.StatusCreated && respIdempotent.StatusCode != http.StatusOK) || assignIdempotent == nil {
		t.Fatalf("idempotent assign voices failed: %d", respIdempotent.StatusCode)
	}
	if assignIdempotent.CASHash != assign1.CASHash {
		t.Fatalf("expected same CASHash on idempotent re-call, got %s vs %s", assign1.CASHash, assignIdempotent.CASHash)
	}

	// 3. Conflicting re-assignment on the same run must return 409 Conflict
	respConflict, _ := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"use_same_voice_for_all": true,
	})
	if respConflict.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on conflicting voice reassignment, got %d", respConflict.StatusCode)
	}
}
