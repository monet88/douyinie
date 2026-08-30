package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
	"net/http"
	"testing"
	"time"
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
	if res.Audition.ProviderID == "" {
		t.Fatalf("expected populated ProviderID on audition result")
	}
	if res.Audition.ModelName == "" {
		t.Fatalf("expected populated ModelName on audition result")
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

// ---------------------------------------------------------------------------
// Test 10: Contextual ~10s Voice Audition Mixed with Preserved BGM/SFX
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAudition_Contextual_MixedWithBGM(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup audio role plan with dialogue and BGM
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 4000, EndMs: 10000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

	// 2. Setup DubScriptVariant with actual translated segment
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      4000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	if dubVariant == nil {
		t.Fatalf("failed to setup dub script variant")
	}

	// 3. Setup AudioStems in CAS and SQLite with background audio
	bgPCM := media.GeneratePCM16WAV(16000, 1, 10000)
	bgCASObj, err := h.casStore.Put(bytes.NewReader(bgPCM))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}

	stemArtifact := domain.AudioStemArtifacts{
		ID:            "stem_art_audition_test",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mock",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgCASObj.SHA256,
				AudioCASPath: bgCASObj.Path,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   10000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemBytes, _ := json.Marshal(stemArtifact)
	stemObj, err := h.casStore.Put(bytes.NewReader(stemBytes))
	if err != nil {
		t.Fatalf("put stems artifact: %v", err)
	}
	_ = h.db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        stemObj.SHA256,
		ProvenanceHash: "prov_stem_audition_123",
		CreatedAt:      time.Now().UTC(),
	})

	// 4. Request Contextual ~10s voice audition (is_contextual: true) without providing explicit sample_text
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
		"is_contextual": true,
		"segment_index": 0,
	}
	bodyBytes, _ := json.Marshal(auditionPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for contextual voice-audition, got %d", resp.StatusCode)
	}

	var res struct {
		Audition domain.VoiceAuditionResult `json:"audition_result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode audition result: %v", err)
	}

	// Verify contextual audition properties
	if !res.Audition.IsContextual {
		t.Errorf("expected is_contextual=true in audition result")
	}
	if !res.Audition.ContextualMixed {
		t.Errorf("expected contextual_mixed=true in audition result")
	}
	if res.Audition.SampleText == "" {
		t.Errorf("expected non-empty sample text loaded from DubScriptVariant")
	}
	if res.Audition.MeasuredDurationMs <= 0 {
		t.Errorf("expected positive measured duration, got %d", res.Audition.MeasuredDurationMs)
	}
	if res.Audition.AudioCASHash == "" {
		t.Errorf("expected AudioCASHash in audition result")
	}

	// Verify no full dub was generated (DubSegmentsVariantIndex and DubMixArtifactIndex do NOT exist)
	dubIdx, err := h.db.GetDubSegmentsVariantIndex(context.Background(), assetID, "vi")
	if err == nil && dubIdx != nil {
		t.Errorf("expected no DubSegmentsVariant to be generated merely for voice audition")
	}
	mixIdx, err := h.db.GetDubMixArtifactIndex(context.Background(), assetID, "vi")
	if err == nil && mixIdx != nil {
		t.Errorf("expected no DubMixArtifact to be generated merely for voice audition")
	}
}

// ---------------------------------------------------------------------------
// Test 11: No-Speech Videos Display "No dubbing required" and Skip Audition / Assignment
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAudition_NoSpeech_SkipsAuditionAndAssignment(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. AudioRolePlan with ONLY instrumental BGM (zero dub-eligible dialogue)
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 8000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

	// 2. Call Voice Audition -> returns 422 Unprocessable Entity with "No dubbing required"
	auditionPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"voice": domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_f1",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
	}
	bodyBytes, _ := json.Marshal(auditionPayload)
	respAudition, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition failed: %v", err)
	}
	defer respAudition.Body.Close()
	if respAudition.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity for no-speech voice audition, got %d", respAudition.StatusCode)
	}
	var errRes struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(respAudition.Body).Decode(&errRes)
	if errRes.Error != "No dubbing required" {
		t.Errorf("expected error 'No dubbing required', got %q", errRes.Error)
	}

	// 3. Call Voice Assignment -> returns 422 Unprocessable Entity with "No dubbing required"
	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity for no-speech voice assignment, got %d", respAssign.StatusCode)
	}
	if assign != nil {
		t.Errorf("expected nil voice assignment for no-speech video")
	}
}

// ---------------------------------------------------------------------------
// Test 12: Contextual Voice Audition with Mid-Video Segment & Long Stems (Preview-Local Window & Fail Closed)
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAudition_Contextual_MidVideoSegmentAndFailClosed(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup DubScriptVariant with segment at index 0 situated at 25000ms (mid-video)
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    25000,
			EndMs:      29000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	if dubVariant == nil {
		t.Fatalf("failed to setup dub script variant")
	}

	// 2. Setup audio role plan with multiple segments across a 60-second video:
	// 0-10s: Intro BGM, 10-25s: Singing/Music-Vocal, 25-29s: Dialogue (mid-video), 29-60s: Outro BGM
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 10000, EndMs: 25000, Role: domain.AudioRoleSingingMusicVocal},
			{StartMs: 25000, EndMs: 29000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 29000, EndMs: 60000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	// 3. Setup long source stems (60s = 60000ms) for background and vocals
	bgPCM := media.GeneratePCM16WAV(16000, 1, 60000)
	bgCASObj, err := h.casStore.Put(bytes.NewReader(bgPCM))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}

	vocalsPCM := media.GeneratePCM16WAV(16000, 1, 60000)
	vocalsCASObj, err := h.casStore.Put(bytes.NewReader(vocalsPCM))
	if err != nil {
		t.Fatalf("put vocals stem: %v", err)
	}

	stemArtifact := domain.AudioStemArtifacts{
		ID:            "stem_art_midvideo_test",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mock",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgCASObj.SHA256,
				AudioCASPath: bgCASObj.Path,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   60000,
			},
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: vocalsCASObj.SHA256,
				AudioCASPath: vocalsCASObj.Path,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   60000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemBytes, _ := json.Marshal(stemArtifact)
	stemObj, err := h.casStore.Put(bytes.NewReader(stemBytes))
	if err != nil {
		t.Fatalf("put stems artifact: %v", err)
	}
	_ = h.db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        stemObj.SHA256,
		ProvenanceHash: "prov_stem_midvideo_123",
		CreatedAt:      time.Now().UTC(),
	})

	// 4. Request Contextual voice audition for segment 0 (mid-video: 25000-29000ms)
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
		"is_contextual": true,
		"segment_index": 0,
	}
	bodyBytes, _ := json.Marshal(auditionPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for contextual voice-audition on mid-video segment, got %d", resp.StatusCode)
	}

	var res struct {
		Audition domain.VoiceAuditionResult `json:"audition_result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode audition result: %v", err)
	}

	// Verify duration is preview-local ~10s (10000ms), NOT the full 60s (60000ms) timeline
	if res.Audition.MeasuredDurationMs != 10000 {
		t.Errorf("expected preview-local measured duration of 10000ms, got %dms", res.Audition.MeasuredDurationMs)
	}
	if !res.Audition.ContextualMixed {
		t.Errorf("expected contextual_mixed=true in result evidence")
	}
	if !res.Audition.IsContextual {
		t.Errorf("expected is_contextual=true")
	}

	// 5. Fail Closed Verification:
	// Create a new asset with DubScript and AudioRolePlan but NO audio stems artifact.
	// Contextual audition must fail closed (500 Internal Server Error / ErrAudioStemsNotFound) rather than silently returning dry TTS.
	h2 := setupHarness(t)
	jobID2, runID2 := createJobAndRun(t, h2)
	job2 := getJobViaAPI(t, h2, jobID2)
	assetID2 := job2.SourceAssetID

	planBody2, _ := json.Marshal(planPayload)
	_, _ = http.Post(h2.server.URL+"/api/v1/assets/"+assetID2+"/audio-role-plan", "application/json", bytes.NewReader(planBody2))
	_, _ = setupDubScriptForSeam1(t, h2, runID2, assetID2, segments)

	auditionPayload2 := map[string]any{
		"run_id":          runID2,
		"target_language": "vi",
		"voice": domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_female_natural",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
		"is_contextual": true,
		"segment_index": 0,
	}
	bodyBytes2, _ := json.Marshal(auditionPayload2)
	resp2, err := http.Post(h2.server.URL+"/api/v1/assets/"+assetID2+"/voice-audition", "application/json", bytes.NewReader(bodyBytes2))
	if err != nil {
		t.Fatalf("POST voice-audition without stems failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 fail closed when stems missing for contextual audition, got %d", resp2.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 13: Contextual Voice Audition Fail Closed Without DubScript / AudioRolePlan
// and Near-Source-End Clamping via Seam 1 API
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAudition_Contextual_FailClosedAndNearEnd(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Setup 20s background audio stems in CAS and SQLite
	bgPCM := media.GeneratePCM16WAV(16000, 1, 20000)
	bgCASObj, err := h.casStore.Put(bytes.NewReader(bgPCM))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}
	stemArtifact := domain.AudioStemArtifacts{
		ID:            "stem_art_seam1_failclosed_test",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mock",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgCASObj.SHA256,
				AudioCASPath: bgCASObj.Path,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   20000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemBytes, _ := json.Marshal(stemArtifact)
	stemObj, err := h.casStore.Put(bytes.NewReader(stemBytes))
	if err != nil {
		t.Fatalf("put stems artifact: %v", err)
	}
	_ = h.db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        stemObj.SHA256,
		ProvenanceHash: "prov_stem_seam1_failclosed_123",
		CreatedAt:      time.Now().UTC(),
	})

	// Setup audio role plan (13s-15s near source end)
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 13000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 13000, EndMs: 15000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 15000, EndMs: 20000, Role: domain.AudioRoleInstrumentalBgm},
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
		"is_contextual": true,
		"segment_index": 0,
	}
	bodyBytes, _ := json.Marshal(auditionPayload)

	// 1. Without DubScriptVariant, contextual audition MUST fail closed (500 Internal Server Error)
	respNoScript, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition without dub script failed: %v", err)
	}
	defer respNoScript.Body.Close()
	if respNoScript.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when DubScriptVariant missing, got %d", respNoScript.StatusCode)
	}

	// 2. Now setup DubScriptVariant for segment near end (13000ms-15000ms)
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "结尾附近的对话。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    13000,
			EndMs:      15000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	if dubVariant == nil {
		t.Fatalf("setup dub script variant failed")
	}

	// 3. Request contextual audition on near-end segment -> succeeds and returns clamped preview window
	respSuccess, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST voice-audition near end failed: %v", err)
	}
	defer respSuccess.Body.Close()
	if respSuccess.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for contextual voice-audition near end, got %d", respSuccess.StatusCode)
	}
	var res struct {
		Audition domain.VoiceAuditionResult `json:"audition_result"`
	}
	if err := json.NewDecoder(respSuccess.Body).Decode(&res); err != nil {
		t.Fatalf("decode audition result: %v", err)
	}
	if !res.Audition.ContextualMixed {
		t.Errorf("expected ContextualMixed=true")
	}
	if res.Audition.MeasuredDurationMs != 10000 {
		t.Errorf("expected 10000ms preview window duration, got %dms", res.Audition.MeasuredDurationMs)
	}
	if res.Audition.SampleText == "" {
		t.Errorf("expected non-empty sample text loaded from DubScriptVariant")
	}
}

// ---------------------------------------------------------------------------
// Test 14: Contextual Voice Audition Fails Closed On Uncovered Dialogue Suppression
// or Broken Declared Vocals Stem
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAudition_Contextual_UncoveredSuppressionAndBrokenVocals(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup 20s background audio stems in CAS and SQLite
	bgPCM := media.GeneratePCM16WAV(16000, 1, 20000)
	bgCASObj, err := h.casStore.Put(bytes.NewReader(bgPCM))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}
	// 2. Setup DubScriptVariant with segment at 2000ms-6000ms (4s)
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    2000,
			EndMs:      6000,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	if dubVariant == nil {
		t.Fatalf("setup dub script variant failed")
	}

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
		"is_contextual": true,
		"segment_index": 0,
	}
	bodyBytes, _ := json.Marshal(auditionPayload)

	t.Run("UncoveredDialogueSuppression_FailsClosed", func(t *testing.T) {
		// AudioRolePlan dialogue only covers 2000ms-3000ms (speech is 2000ms-3500ms from fake TTS)
		planPayload := map[string]any{
			"segments": []domain.AudioSegment{
				{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
				{StartMs: 2000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
				{StartMs: 3000, EndMs: 20000, Role: domain.AudioRoleInstrumentalBgm},
			},
		}
		planBody, _ := json.Marshal(planPayload)
		_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

		stemArtifact := domain.AudioStemArtifacts{
			ID:            "stem_art_uncovered_test",
			SchemaVersion: domain.AudioStemsSchemaVersion,
			AssetID:       assetID,
			ProviderID:    "fake_separator",
			ModelName:     "uvr_mock",
			ModelVersion:  "1.0",
			Stems: []domain.AudioStem{
				{
					Type:         domain.StemTypeBackground,
					AudioCASHash: bgCASObj.SHA256,
					AudioCASPath: bgCASObj.Path,
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
			},
			CreatedAt: time.Now().UTC(),
		}
		stemBytes, _ := json.Marshal(stemArtifact)
		stemObj, _ := h.casStore.Put(bytes.NewReader(stemBytes))
		_ = h.db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
			ID:             stemArtifact.ID,
			AssetID:        assetID,
			ProviderID:     stemArtifact.ProviderID,
			ModelName:      stemArtifact.ModelName,
			ModelVersion:   stemArtifact.ModelVersion,
			CASHash:        stemObj.SHA256,
			ProvenanceHash: "prov_stem_uncovered_123",
			CreatedAt:      time.Now().UTC(),
		})

		resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST voice-audition failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500 when speech is not fully covered by dialogue suppression, got %d", resp.StatusCode)
		}
	})

	t.Run("BrokenDeclaredVocalsStem_FailsClosed", func(t *testing.T) {
		// Valid full dialogue covering 2000ms-6000ms
		planPayload := map[string]any{
			"segments": []domain.AudioSegment{
				{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
				{StartMs: 2000, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
				{StartMs: 6000, EndMs: 20000, Role: domain.AudioRoleInstrumentalBgm},
			},
		}
		planBody, _ := json.Marshal(planPayload)
		_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

		// Stems artifact declares a vocals stem with missing CAS hash
		stemArtifact := domain.AudioStemArtifacts{
			ID:            "stem_art_broken_vocals_test",
			SchemaVersion: domain.AudioStemsSchemaVersion,
			AssetID:       assetID,
			ProviderID:    "fake_separator",
			ModelName:     "uvr_mock",
			ModelVersion:  "1.0",
			Stems: []domain.AudioStem{
				{
					Type:         domain.StemTypeBackground,
					AudioCASHash: bgCASObj.SHA256,
					AudioCASPath: bgCASObj.Path,
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
				{
					Type:         domain.StemTypeVocals,
					AudioCASHash: "missing_vocals_cas_sha_12345",
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
			},
			CreatedAt: time.Now().UTC(),
		}
		stemBytes, _ := json.Marshal(stemArtifact)
		stemObj, _ := h.casStore.Put(bytes.NewReader(stemBytes))
		_ = h.db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
			ID:             stemArtifact.ID,
			AssetID:        assetID,
			ProviderID:     stemArtifact.ProviderID,
			ModelName:      stemArtifact.ModelName,
			ModelVersion:   stemArtifact.ModelVersion,
			CASHash:        stemObj.SHA256,
			ProvenanceHash: "prov_stem_broken_vocals_123",
			CreatedAt:      time.Now().UTC(),
		})

		resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST voice-audition failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500 when declared vocals stem is missing/broken, got %d", resp.StatusCode)
		}
	})
}

// ---------------------------------------------------------------------------
// Test 15: Contextual Voice Audition Slot Overrun Fails Closed & Secondary Metadata
// ---------------------------------------------------------------------------
func TestSeam1_TTS_VoiceAudition_Contextual_SlotOverrunFailsClosed_AndSecondaryMetadata(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup 30s background audio stem in CAS and SQLite
	bgPCM := media.GeneratePCM16WAV(16000, 1, 30000)
	bgCASObj, err := h.casStore.Put(bytes.NewReader(bgPCM))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}

	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgCASObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   30000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemBytes, _ := json.Marshal(stemArtifact)
	stemObj, _ := h.casStore.Put(bytes.NewReader(stemBytes))
	_ = h.db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        stemObj.SHA256,
		ProvenanceHash: "prov_stem_slot_seam1",
		CreatedAt:      time.Now().UTC(),
	})

	// 2. AudioRolePlan with dialogue from 0ms to 10000ms
	_ = h.db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 10000, EndMs: 30000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})

	auditionPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"voice": domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_f1",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
		"is_contextual": true,
		"segment_index": 0,
	}

	t.Run("SlotOverrun_FailsClosed", func(t *testing.T) {
		// DubScript segment is 2000ms-3000ms (1000ms slot).
		// fake_vieneu_tts_vi generates 1500ms audio -> ends at 3500ms.
		// 3500ms fits total audio (30s) and suppression (10s), but overruns slot end (3000ms).
		segmentsOverrun := []domain.TranslationInputSegment{
			{
				Index:      0,
				SourceText: "短段落。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    2000,
				EndMs:      3000, // 1000ms slot
			},
		}
		_, _ = setupDubScriptForSeam1(t, h, runID, assetID, segmentsOverrun)

		bodyBytes, _ := json.Marshal(auditionPayload)
		resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST voice-audition failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500 when speech overruns segment slot, got %d", resp.StatusCode)
		}
	})

	t.Run("ExactSlotFit_PassesAndReturnsSecondaryMetadata", func(t *testing.T) {
		// DubScript segment is 2000ms-3500ms (1500ms slot).
		// fake_vieneu_tts_vi generates 1500ms audio -> ends at 3500ms (exact fit).
		segmentsExact := []domain.TranslationInputSegment{
			{
				Index:      0,
				SourceText: "精确匹配的段落。",
				SpeakerID:  "SPEAKER_00",
				StartMs:    2000,
				EndMs:      3500, // 1500ms slot
			},
		}
		_, _ = setupDubScriptForSeam1(t, h, runID, assetID, segmentsExact)

		bodyBytes, _ := json.Marshal(auditionPayload)
		resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-audition", "application/json", bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("POST voice-audition failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on exact slot boundary fit, got %d", resp.StatusCode)
		}

		var res struct {
			Audition domain.VoiceAuditionResult `json:"audition_result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("decode audition result: %v", err)
		}

		if !res.Audition.ContextualMixed {
			t.Errorf("expected ContextualMixed=true")
		}
		if res.Audition.ProviderID != "fake_vieneu_tts_vi" {
			t.Errorf("expected ProviderID 'fake_vieneu_tts_vi', got %q", res.Audition.ProviderID)
		}
		if res.Audition.ModelName != "fake_vieneu_tts_vi" {
			t.Errorf("expected ModelName 'fake_vieneu_tts_vi', got %q", res.Audition.ModelName)
		}
		if res.Audition.ModelVersion != "1.0.0" {
			t.Errorf("expected ModelVersion '1.0.0', got %q", res.Audition.ModelVersion)
		}
	})
}
