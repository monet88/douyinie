package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Helper to post reassign request
func runReassignVoices(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.VoiceAssignment) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-assignment/reassign", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("voice-assignment reassign request failed: %v", err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var res struct {
			Assignment domain.VoiceAssignment `json:"voice_assignment"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Assignment
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Test 1: Multi-Speaker Independent Assignment + Distinguishability QC + Convenience
// ---------------------------------------------------------------------------
func TestSeam1_MultiSpeaker_IndependentAssignment_And_DistinguishabilityQC(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Multi-speaker script with 2 distinct speakers
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
			StartMs:    2200,
			EndMs:      4200,
		},
	}
	_, _ = setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// 1. Default AssignVoices: independent per-speaker assignment with distinguishable voices
	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("assign voices failed: status %d", respAssign.StatusCode)
	}

	if len(assign.Assignments) != 2 {
		t.Fatalf("expected 2 speaker assignments, got %d", len(assign.Assignments))
	}
	voice0, ok0 := assign.Assignments["SPEAKER_00"]
	voice1, ok1 := assign.Assignments["SPEAKER_01"]
	if !ok0 || !ok1 {
		t.Fatalf("missing expected speakers in assignment: %v", assign.Assignments)
	}
	if voice0.ID == voice1.ID {
		t.Fatalf("expected distinct preset voices for multi-speaker, but both got %s", voice0.ID)
	}
	if assign.Distinguishability == nil {
		t.Fatalf("expected Distinguishability QC result attached to assignment")
	}
	if !assign.Distinguishability.MultiSpeaker || assign.Distinguishability.Status != "PASS" {
		t.Fatalf("expected Distinguishability PASS for default multi-speaker assignment, got %+v", assign.Distinguishability)
	}

	// 2. Convenience control: "Use same voice for all" on a new run
	runResp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/runs", "application/json", nil)
	if err != nil || runResp.StatusCode != http.StatusCreated {
		t.Fatalf("create run 2 failed: %v", err)
	}
	var run2 struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(runResp.Body).Decode(&run2)
	runID2 := run2.Run.ID

	respSame, assignSame := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":                 runID2,
		"target_language":        "vi",
		"use_same_voice_for_all": true,
	})
	if respSame.StatusCode != http.StatusCreated || assignSame == nil {
		t.Fatalf("assign same voice failed: status %d", respSame.StatusCode)
	}
	if !assignSame.UseSameVoiceForAll {
		t.Fatalf("expected use_same_voice_for_all=true")
	}
	sameV0 := assignSame.Assignments["SPEAKER_00"]
	sameV1 := assignSame.Assignments["SPEAKER_01"]
	if sameV0.ID != sameV1.ID {
		t.Fatalf("expected identical voice when use_same_voice_for_all=true, got %s vs %s", sameV0.ID, sameV1.ID)
	}
	if assignSame.Distinguishability == nil || assignSame.Distinguishability.Status != "PASS" {
		t.Fatalf("expected Distinguishability PASS for explicit same_voice convenience, got %+v", assignSame.Distinguishability)
	}

	// 3. Convenience control with multiple custom_assignments: deterministic selection by sorted speaker key
	runResp3, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/runs", "application/json", nil)
	if err != nil || runResp3.StatusCode != http.StatusCreated {
		t.Fatalf("create run 3 failed: %v", err)
	}
	var run3 struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(runResp3.Body).Decode(&run3)
	runID3 := run3.Run.ID

	voiceA := provider.DefaultPresetVoices("vi")[0] // ZeroTTS quangminh
	voiceB := provider.DefaultPresetVoices("vi")[1] // ZeroTTS maichi
	respDet, assignDet := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":                 runID3,
		"target_language":        "vi",
		"use_same_voice_for_all": true,
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_01": voiceB,
			"SPEAKER_00": voiceA,
		},
	})
	if respDet.StatusCode != http.StatusCreated || assignDet == nil {
		t.Fatalf("assign same voice with multiple custom failed: status %d", respDet.StatusCode)
	}
	// Invariant: SPEAKER_00 is alphabetically first, so voiceA must be chosen deterministically for both
	if assignDet.Assignments["SPEAKER_00"].ID != voiceA.ID || assignDet.Assignments["SPEAKER_01"].ID != voiceA.ID {
		t.Fatalf("expected deterministic selection of voiceA (SPEAKER_00 < SPEAKER_01), got %v", assignDet.Assignments)
	}

	// 4. Multi-speaker distinguishability QC flagging accidental/custom duplicate voices
	runResp4, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/runs", "application/json", nil)
	if err != nil || runResp4.StatusCode != http.StatusCreated {
		t.Fatalf("create run 4 failed: %v", err)
	}
	var run4 struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(runResp4.Body).Decode(&run4)
	runID4 := run4.Run.ID

	duplicateVoice := provider.DefaultPresetVoices("vi")[0]
	respDup, assignDup := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":                 runID4,
		"target_language":        "vi",
		"use_same_voice_for_all": false,
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": duplicateVoice,
			"SPEAKER_01": duplicateVoice,
		},
	})
	if respDup.StatusCode != http.StatusCreated || assignDup == nil {
		t.Fatalf("assign dup voices failed: status %d", respDup.StatusCode)
	}
	if assignDup.Distinguishability == nil {
		t.Fatalf("expected Distinguishability QC result on duplicate assignment")
	}
	if assignDup.Distinguishability.Status != "REVIEW_REQUIRED" {
		t.Fatalf("expected Distinguishability REVIEW_REQUIRED on colliding voice profiles, got %s", assignDup.Distinguishability.Status)
	}
	if len(assignDup.Distinguishability.Issues) == 0 {
		t.Fatalf("expected QC issues to report collision, got none")
	}
}

// ---------------------------------------------------------------------------
// Test 2: Transcript Primacy — Consumes speaker labels from TranscriptArtifact (T08)
// ---------------------------------------------------------------------------
func TestSeam1_MultiSpeaker_ConsumesT08TranscriptSpeakers(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Save audio role plan
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil || planResp.StatusCode != http.StatusCreated {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp.Body.Close()

	// 2. Run speech understanding with mock diarizer that outputs 2 speakers
	speechSvc := service.NewSpeechService(h.db, h.casStore)
	speechSvc.ASRInvoke = func(ctx context.Context, req service.SpeechASRRequest) (*service.SpeechASRResult, error) {
		return &service.SpeechASRResult{
			ProviderID:   "fake_qwen3_asr",
			ModelName:    "qwen3-asr",
			ModelVersion: "1.7b",
			LanguageCode: "zh",
			Segments: []domain.ASRRawSegment{
				{StartMs: 0, EndMs: 6000, Text: "你好。我很好。再见。", Confidence: 0.95},
			},
		}, nil
	}
	speechSvc.AlignerInvoke = func(ctx context.Context, req service.SpeechAlignmentRequest) (*service.SpeechAlignmentResult, error) {
		return &service.SpeechAlignmentResult{
			ProviderID:   "fake_qwen3_aligner",
			ModelName:    "qwen3-aligner",
			ModelVersion: "1.0.0",
			WordTimings: []domain.WordTiming{
				{Word: "你好。", StartMs: 0, EndMs: 1500, Confidence: 0.95},
				{Word: "我很好。", StartMs: 1800, EndMs: 3500, Confidence: 0.95},
				{Word: "再见。", StartMs: 3800, EndMs: 5500, Confidence: 0.95},
			},
		}, nil
	}
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming, req service.SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
		return &domain.DiarizationPlan{
			ID:    "diarization_t08",
			RunID: runID,
			Assignments: []domain.SpeakerAssignment{
				{SpeakerID: "SPEAKER_A", StartMs: 0, EndMs: 3500, Confidence: 0.95},
				{SpeakerID: "SPEAKER_B", StartMs: 3501, EndMs: 6000, Confidence: 0.95},
			},
			Confidence: 0.95,
		}, nil
	}
	h.srv.SetSpeechService(speechSvc)

	speechReq := map[string]any{
		"run_id": runID,
	}
	speechBody, _ := json.Marshal(speechReq)
	speechResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/speech-understand", "application/json", bytes.NewReader(speechBody))
	if err != nil || speechResp.StatusCode != http.StatusCreated {
		t.Fatalf("speech-understand failed: status %d", speechResp.StatusCode)
	}
	speechResp.Body.Close()

	// 3. AssignVoices should resolve SPEAKER_A and SPEAKER_B directly from TranscriptArtifact
	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("assign voices from transcript failed: status %d", respAssign.StatusCode)
	}

	if _, okA := assign.Assignments["SPEAKER_A"]; !okA {
		t.Fatalf("expected SPEAKER_A from TranscriptArtifact, got assignments: %v", assign.Assignments)
	}
	if _, okB := assign.Assignments["SPEAKER_B"]; !okB {
		t.Fatalf("expected SPEAKER_B from TranscriptArtifact, got assignments: %v", assign.Assignments)
	}
	if assign.Assignments["SPEAKER_A"].ID == assign.Assignments["SPEAKER_B"].ID {
		t.Fatalf("expected distinct voices assigned for SPEAKER_A and SPEAKER_B")
	}
}

// ---------------------------------------------------------------------------
// Test 3: Invalidation Scope — Voice change regenerates only that speaker's descendants
// ---------------------------------------------------------------------------
func TestSeam1_VoiceChange_InvalidationScope_And_SpeakerScopedRegeneration(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 4 segments across 2 speakers (2 segments each)
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
			SourceText: "测试语音输入",
			SpeakerID:  "SPEAKER_00",
			StartMs:    2200,
			EndMs:      4200,
		},
		{
			Index:      2,
			SourceText: "我们去公园散步吧。",
			SpeakerID:  "SPEAKER_01",
			StartMs:    4500,
			EndMs:      6500,
		},
		{
			Index:      3,
			SourceText: "明天再继续工作。",
			SpeakerID:  "SPEAKER_01",
			StartMs:    6700,
			EndMs:      8700,
		},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// 1. Initial assignment for run
	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", respAssign.StatusCode)
	}

	// 2. Initial synthesis -> variant 1
	respSynth1, variant1 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth1.StatusCode != http.StatusCreated || variant1 == nil {
		t.Fatalf("initial synthesis failed: %d", respSynth1.StatusCode)
	}
	if len(variant1.Segments) != 4 {
		t.Fatalf("expected 4 selected segments in variant 1, got %d", len(variant1.Segments))
	}

	spk0Seg0_v1 := variant1.Segments[0]
	spk0Seg1_v1 := variant1.Segments[1]
	spk1Seg2_v1 := variant1.Segments[2]
	spk1Seg3_v1 := variant1.Segments[3]

	// 3. Register CosyVoice3 provider and license manifest for new voice
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", 1350)
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

	// 4. Explicit operator voice change: reassign SPEAKER_00 only to CosyVoice3 preset
	newVoiceForSpk0 := fakeCosy.VoiceCatalog()[0] // CosyVoice3 VI Female
	respReassign, assign2 := runReassignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": newVoiceForSpk0,
		},
	})
	if respReassign.StatusCode != http.StatusOK || assign2 == nil {
		t.Fatalf("reassign voice failed: %d", respReassign.StatusCode)
	}

	// Invariant: SupersedesCAS links back to assign1
	if assign2.SupersedesCAS != assign1.CASHash {
		t.Fatalf("expected SupersedesCAS=%s, got %s", assign1.CASHash, assign2.SupersedesCAS)
	}
	// Invariant: InvalidatedSpeakers contains ONLY SPEAKER_00
	if len(assign2.InvalidatedSpeakers) != 1 || assign2.InvalidatedSpeakers[0] != "SPEAKER_00" {
		t.Fatalf("expected InvalidatedSpeakers=[SPEAKER_00], got %v", assign2.InvalidatedSpeakers)
	}
	// Invariant: InvalidationScope declared for T18/T20 includes render_plan
	expectedStages := domain.VoiceChangeInvalidationStages()
	if len(assign2.InvalidationScope) != len(expectedStages) {
		t.Fatalf("expected InvalidationScope %v, got %v", expectedStages, assign2.InvalidationScope)
	}
	hasRenderPlan := false
	for _, st := range assign2.InvalidationScope {
		if st == "render_plan" {
			hasRenderPlan = true
			break
		}
	}
	if !hasRenderPlan {
		t.Fatalf("expected render_plan in InvalidationScope, got %v", assign2.InvalidationScope)
	}

	// Instrument provider invocations before synthesis 2
	zeroTTSProv := defaultVITTSFake(t, h)
	zeroTTSInvocationsBefore := zeroTTSProv.Invocations
	cosyInvocationsBefore := fakeCosy.Invocations

	// 5. Synthesize with new VoiceAssignment CAS
	respSynth2, variant2 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign2.CASHash,
	})
	if respSynth2.StatusCode != http.StatusCreated || variant2 == nil {
		t.Fatalf("synthesis with reassigned voice failed: %d", respSynth2.StatusCode)
	}
	if variant2.CASHash == variant1.CASHash {
		t.Fatalf("expected new DubSegmentsVariant CASHash on voice change")
	}

	// Hard Red-Capable Invariant: Unchanged speaker SPEAKER_01 must cause EXACTLY ZERO new TTS invocations
	zeroTTSDelta := zeroTTSProv.Invocations - zeroTTSInvocationsBefore
	if zeroTTSDelta != 0 {
		t.Fatalf("expected unchanged speaker (SPEAKER_01) to cause 0 new TTS invocations, but got %d new invocations", zeroTTSDelta)
	}

	// Hard Red-Capable Invariant: Changed speaker SPEAKER_00 must cause EXACTLY 2 new TTS invocations (2 segments)
	cosyDelta := fakeCosy.Invocations - cosyInvocationsBefore
	if cosyDelta != 2 {
		t.Fatalf("expected changed speaker (SPEAKER_00) to cause exactly 2 new TTS invocations, got %d", cosyDelta)
	}

	// Invariant: SPEAKER_01's segments are reused exactly (same AudioSHA256, same AudioCASPath)
	spk1Seg2_v2 := variant2.Segments[2]
	spk1Seg3_v2 := variant2.Segments[3]
	if spk1Seg2_v2.AudioSHA256 != spk1Seg2_v1.AudioSHA256 {
		t.Fatalf("expected SPEAKER_01 segment 2 AudioSHA256 to be reused, got %s vs %s", spk1Seg2_v2.AudioSHA256, spk1Seg2_v1.AudioSHA256)
	}
	if spk1Seg3_v2.AudioSHA256 != spk1Seg3_v1.AudioSHA256 {
		t.Fatalf("expected SPEAKER_01 segment 3 AudioSHA256 to be reused, got %s vs %s", spk1Seg3_v2.AudioSHA256, spk1Seg3_v1.AudioSHA256)
	}

	// Invariant: SPEAKER_00's segments have new VoiceProfile and regenerated audio
	spk0Seg0_v2 := variant2.Segments[0]
	spk0Seg1_v2 := variant2.Segments[1]
	if spk0Seg0_v2.Voice.ID != newVoiceForSpk0.ID {
		t.Fatalf("expected SPEAKER_00 segment 0 voice %s, got %s", newVoiceForSpk0.ID, spk0Seg0_v2.Voice.ID)
	}
	if spk0Seg1_v2.Voice.ID != newVoiceForSpk0.ID {
		t.Fatalf("expected SPEAKER_00 segment 1 voice %s, got %s", newVoiceForSpk0.ID, spk0Seg1_v2.Voice.ID)
	}
	if spk0Seg0_v2.AudioSHA256 == spk0Seg0_v1.AudioSHA256 {
		t.Fatalf("expected regenerated audio SHA for changed speaker 00, but got same SHA")
	}
	if spk0Seg1_v2.AudioSHA256 == spk0Seg1_v1.AudioSHA256 {
		t.Fatalf("expected regenerated audio SHA for changed speaker 00, but got same SHA")
	}
}

// ---------------------------------------------------------------------------
// Test 4: DubScript Identity Guard — Changing DubScriptVariantCAS forces
// synthesis for all current segments even when voice assignment supersession exists
// ---------------------------------------------------------------------------
func TestSeam1_VoiceChange_DubScriptChangeForcesSynthesisEvenWithSupersession(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_00", StartMs: 2200, EndMs: 4200, SourceText: "测试语音输入"},
		{Index: 2, SpeakerID: "SPEAKER_01", StartMs: 4500, EndMs: 6500, SourceText: "我们去公园散步吧。"},
		{Index: 3, SpeakerID: "SPEAKER_01", StartMs: 6700, EndMs: 8700, SourceText: "明天再继续工作。"},
	}

	_, dubVariant1 := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", respAssign.StatusCode)
	}

	respSynth1, variant1 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant1.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth1.StatusCode != http.StatusCreated || variant1 == nil {
		t.Fatalf("initial synthesis failed: %d", respSynth1.StatusCode)
	}

	// Register CosyVoice3 provider for SPEAKER_00 reassignment
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", 1350)
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
		t.Fatalf("register license manifest for new voice failed: %v", err)
	}
	licResp.Body.Close()

	// Reassign SPEAKER_00 voice -> assign2 (supersedes assign1, invalidates SPEAKER_00)
	newVoiceForSpk0 := fakeCosy.VoiceCatalog()[0] // CosyVoice3 VI Female
	respReassign, assign2 := runReassignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": newVoiceForSpk0,
		},
	})
	if respReassign.StatusCode != http.StatusOK || assign2 == nil {
		t.Fatalf("reassign voice failed: %d", respReassign.StatusCode)
	}

	// Create dubVariant2 with modified script for segment 3 ("明天见。")
	segmentsMod := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_00", StartMs: 2200, EndMs: 4200, SourceText: "测试语音输入"},
		{Index: 2, SpeakerID: "SPEAKER_01", StartMs: 4500, EndMs: 6500, SourceText: "我们去公园散步吧。"},
		{Index: 3, SpeakerID: "SPEAKER_01", StartMs: 6700, EndMs: 8700, SourceText: "明天见。"},
	}
	_, dubVariant2 := setupDubScriptForSeam1(t, h, runID, assetID, segmentsMod)
	if dubVariant2.CASHash == dubVariant1.CASHash {
		t.Fatalf("expected different CASHash for modified dub script")
	}

	// Instrument provider invocations
	zeroTTSProv := defaultVITTSFake(t, h)
	zeroTTSInvocationsBefore := zeroTTSProv.Invocations
	cosyInvocationsBefore := fakeCosy.Invocations

	// Synthesize with dubVariant2 and assign2
	respSynth2, variant2 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant2.CASHash,
		"voice_assignment_cas":   assign2.CASHash,
	})
	if respSynth2.StatusCode != http.StatusCreated || variant2 == nil {
		t.Fatalf("synthesis failed: %d", respSynth2.StatusCode)
	}

	// Red-Capable Invariant: When DubScriptVariantCAS differs from prior variant,
	// prior segments CANNOT be reused even for non-invalidated SPEAKER_01!
	// SPEAKER_01 MUST be re-synthesized for the new dub script.
	zeroTTSDelta := zeroTTSProv.Invocations - zeroTTSInvocationsBefore
	if zeroTTSDelta != 2 {
		t.Fatalf("expected unchanged speaker (SPEAKER_01) to be synthesized for new dub script (2 invocations), but got %d", zeroTTSDelta)
	}

	// SPEAKER_00 synthesized for new voice profile (2 invocations)
	cosyDelta := fakeCosy.Invocations - cosyInvocationsBefore
	if cosyDelta != 2 {
		t.Fatalf("expected changed speaker (SPEAKER_00) to have 2 invocations, got %d", cosyDelta)
	}

	if variant2.DubScriptVariantCAS != dubVariant2.CASHash {
		t.Fatalf("expected DubScriptVariantCAS=%s, got %s", dubVariant2.CASHash, variant2.DubScriptVariantCAS)
	}
}

// ---------------------------------------------------------------------------
// Test 5: Missing or Invalid Prior Fit-Plan Evidence Bypasses Reuse
// ---------------------------------------------------------------------------
func TestSeam1_VoiceChange_InvalidPriorFitPlanBypassesReuse(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_00", StartMs: 2200, EndMs: 4200, SourceText: "测试语音输入"},
		{Index: 2, SpeakerID: "SPEAKER_01", StartMs: 4500, EndMs: 6500, SourceText: "我们去公园散步吧。"},
		{Index: 3, SpeakerID: "SPEAKER_01", StartMs: 6700, EndMs: 8700, SourceText: "明天再继续工作。"},
	}

	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", respAssign.StatusCode)
	}

	respSynth1, variant1 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth1.StatusCode != http.StatusCreated || variant1 == nil {
		t.Fatalf("initial synthesis failed: %d", respSynth1.StatusCode)
	}

	// Corrupt prior variant in CAS/DB by removing its fit plans
	corruptedVariant := *variant1
	corruptedVariant.FitPlans = nil // missing fit plans!
	corruptData, _ := json.Marshal(corruptedVariant)
	corruptCAS, err := h.casStore.Put(bytes.NewReader(corruptData))
	if err != nil {
		t.Fatalf("put corrupt variant in CAS: %v", err)
	}
	_ = h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             corruptedVariant.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        corruptCAS.SHA256,
		ProvenanceHash: corruptedVariant.ProvenanceHash,
		OverallStatus:  "PASS",
		CreatedAt:      time.Now().UTC(),
	})

	// Reassign SPEAKER_00 voice -> assign2
	viPresets := provider.DefaultPresetVoices("vi")
	newVoiceForSpk0 := viPresets[1] // vieneu male
	respReassign, assign2 := runReassignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": newVoiceForSpk0,
		},
	})
	if respReassign.StatusCode != http.StatusOK || assign2 == nil {
		t.Fatalf("reassign voice failed: %d", respReassign.StatusCode)
	}

	zeroTTSProv := defaultVITTSFake(t, h)
	zeroTTSInvocationsBefore := zeroTTSProv.Invocations

	// Synthesize with dubVariant and assign2
	respSynth2, variant2 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign2.CASHash,
	})
	if respSynth2.StatusCode != http.StatusCreated || variant2 == nil {
		t.Fatalf("synthesis failed: %d", respSynth2.StatusCode)
	}

	// Red-Capable Invariant: Because prior fit plans were missing/invalid,
	// SPEAKER_01 CANNOT be silently reused and must be synthesized anew!
	zeroTTSDelta := zeroTTSProv.Invocations - zeroTTSInvocationsBefore
	if zeroTTSDelta < 2 {
		t.Fatalf("expected at least 2 new invocations due to invalid prior fit plan, got %d", zeroTTSDelta)
	}

	// Resulting variant must be valid and contain all 4 fit plans
	if len(variant2.FitPlans) != 4 {
		t.Fatalf("expected 4 fit plans in regenerated variant, got %d", len(variant2.FitPlans))
	}
	for _, fp := range variant2.FitPlans {
		if fp.Decision != domain.FitActionAccept {
			t.Fatalf("expected fit plan decision ACCEPT, got %s", fp.Decision)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 7: A prior variant produced under a different TTS stage identity is not spliced
//         into an escalation pass.
//
// An escalation reuses the segments of every speaker it did not invalidate. Reuse is
// legitimate only while the prior variant was produced by the same stage identity: after
// a TTS pin upgrade the prior segments carry audio from the previous runtime, so reusing
// them would emit one variant whose segments came from two different runtimes, with
// nothing in the artifact recording it.
// ---------------------------------------------------------------------------

// rekeyDubSegmentsVariantToPreRuntimeIdentityProvenance republishes an already-synthesized
// variant under the provenance hash a build without the tts_runtime_identity token would
// have recorded for the same (dub script, voice assignment) pair, and rebinds the
// asset-scoped index row to it. The variant body — segments, fit plans, audio — is
// untouched, so only the recorded stage identity differs.
func rekeyDubSegmentsVariantToPreRuntimeIdentityProvenance(
	t *testing.T, h *testHarness, variant *domain.DubSegmentsVariant,
	dubScriptCAS, voiceAssignCAS, assetID, runID string,
) string {
	t.Helper()

	preRuntimeIdentityKey := preRuntimeIdentityStageCacheKey(t, dubScriptCAS, voiceAssignCAS)

	rekeyed := *variant
	rekeyed.ID = variant.ID + "-pre-runtime-identity"
	rekeyed.CASHash = ""
	rekeyed.ProvenanceHash = preRuntimeIdentityKey
	rekeyed.CreatedAt = time.Now().UTC()
	data, err := json.Marshal(&rekeyed)
	if err != nil {
		t.Fatalf("marshal rekeyed variant: %v", err)
	}
	obj, err := h.casStore.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("put rekeyed variant in CAS: %v", err)
	}
	if err := h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             rekeyed.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        obj.SHA256,
		ProvenanceHash: preRuntimeIdentityKey,
		OverallStatus:  rekeyed.OverallStatus,
		CreatedAt:      rekeyed.CreatedAt,
	}); err != nil {
		t.Fatalf("save rekeyed variant index: %v", err)
	}
	return obj.SHA256
}

func TestSeam1_VoiceChange_PreUpgradeVariantProvenanceBypassesReuse(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_00", StartMs: 2200, EndMs: 4200, SourceText: "测试语音输入"},
		{Index: 2, SpeakerID: "SPEAKER_01", StartMs: 4500, EndMs: 6500, SourceText: "我们去公园散步吧。"},
		{Index: 3, SpeakerID: "SPEAKER_01", StartMs: 6700, EndMs: 8700, SourceText: "明天再继续工作。"},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", respAssign.StatusCode)
	}

	// 1. Initial synthesis: a prior variant with fully accepted evidence for both speakers.
	respSynth1, variant1 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth1.StatusCode != http.StatusCreated || variant1 == nil {
		t.Fatalf("initial synthesis failed: %d", respSynth1.StatusCode)
	}
	if len(variant1.Segments) != 4 {
		t.Fatalf("expected 4 selected segments in variant 1, got %d", len(variant1.Segments))
	}

	// 2. That variant was produced by the previous TTS runtime: republish it under the
	//    pre-runtime-identity provenance the older build recorded.
	rekeyedCAS := rekeyDubSegmentsVariantToPreRuntimeIdentityProvenance(t, h, variant1, dubVariant.CASHash, assign1.CASHash, assetID, runID)
	if rekeyedCAS == variant1.CASHash {
		t.Fatalf("expected the rekeyed variant to be a distinct CAS object")
	}

	// 3. Register the CosyVoice3 lane and reassign SPEAKER_00 only (SPEAKER_01 stays).
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", 1350)
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

	respReassign, assign2 := runReassignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": fakeCosy.VoiceCatalog()[0],
		},
	})
	if respReassign.StatusCode != http.StatusOK || assign2 == nil {
		t.Fatalf("reassign voice failed: %d", respReassign.StatusCode)
	}
	if assign2.SupersedesCAS != assign1.CASHash {
		t.Fatalf("expected SupersedesCAS=%s, got %s", assign1.CASHash, assign2.SupersedesCAS)
	}

	// 4. Synthesis with the superseding assignment: SPEAKER_01 was not invalidated, but
	//    the prior variant predates the current TTS stage identity, so it cannot be reused.
	zeroTTSProv := defaultVITTSFake(t, h)
	zeroTTSInvocationsBefore := zeroTTSProv.Invocations
	cosyInvocationsBefore := fakeCosy.Invocations

	respSynth2, variant2 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign2.CASHash,
	})
	if respSynth2.StatusCode != http.StatusCreated || variant2 == nil {
		t.Fatalf("synthesis with reassigned voice failed: %d", respSynth2.StatusCode)
	}

	// Red-Capable Invariant: the unchanged speaker must be re-synthesized (2 segments),
	// not spliced in from a variant produced by the previous TTS runtime.
	zeroTTSDelta := zeroTTSProv.Invocations - zeroTTSInvocationsBefore
	if zeroTTSDelta != 2 {
		t.Fatalf("expected the non-invalidated speaker to be re-synthesized under the current TTS stage identity (2 invocations), got %d", zeroTTSDelta)
	}
	cosyDelta := fakeCosy.Invocations - cosyInvocationsBefore
	if cosyDelta != 2 {
		t.Fatalf("expected the changed speaker to cause exactly 2 new invocations, got %d", cosyDelta)
	}
	if len(variant2.Segments) != 4 {
		t.Fatalf("expected 4 selected segments in variant 2, got %d", len(variant2.Segments))
	}
}
