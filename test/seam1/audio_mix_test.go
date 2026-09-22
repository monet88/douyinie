package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
)

func seam1CurrentFitPolicyID(t *testing.T) string {
	t.Helper()
	_, _, id := service.NewFitController().ResolvePlaybackWindow(1, 0)
	if id == "" {
		t.Fatal("expected current fit policy identity")
	}
	return id
}

// Helper to run audio separation
func runSeparateStems(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.AudioStemArtifacts) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/separate-stems", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("separate-stems request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var res struct {
			Stems domain.AudioStemArtifacts `json:"audio_stems"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Stems
	}
	return resp, nil
}

// Helper to run audio mix
func runAudioMix(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.DubMixArtifact) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-mix", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("audio-mix request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnprocessableEntity {
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBytes))
		var res struct {
			Mix domain.DubMixArtifact `json:"dub_mix"`
		}
		_ = json.Unmarshal(respBytes, &res)
		if res.Mix.ID != "" {
			return resp, &res.Mix
		}
		return resp, nil
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Test 1: AudioStem Separation Produces Stems (UVR baseline / Demucs fallback)
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_SeparationProducesAudioStems(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	resp, stems := runSeparateStems(t, h, assetID, map[string]any{
		"run_id": runID,
	})
	if resp.StatusCode != http.StatusCreated || stems == nil {
		t.Fatalf("expected 201 Created for separate-stems, got %d", resp.StatusCode)
	}

	if len(stems.Stems) < 2 {
		t.Fatalf("expected at least 2 stems (vocals + background), got %d", len(stems.Stems))
	}

	var hasVocals, hasBackground bool
	for _, s := range stems.Stems {
		if s.Type == domain.StemTypeVocals && s.AudioCASHash != "" {
			hasVocals = true
		}
		if s.Type == domain.StemTypeBackground && s.AudioCASHash != "" {
			hasBackground = true
		}
	}

	if !hasVocals || !hasBackground {
		t.Fatalf("expected both vocals and background stems with populated CAS hashes")
	}

	// Invariant: GET /api/v1/assets/{id}/audio-stems retrieves identical artifact
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/audio-stems")
	if err != nil {
		t.Fatalf("get audio stems failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for GET audio-stems, got %d", getResp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 2: SoundtrackPreservationPlan preserves BGM/SFX and suppresses dialogue inside speech windows
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_SoundtrackPreservation_DialogueSuppressedInsideSpeechWindow(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup speech segment within [0, 3000ms]
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

	// 2. Assign voice and synthesize dub segments
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

	// 3. Mix audio
	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":                runID,
		"target_language":       "vi",
		"dub_segments_cas":      dubSegments.CASHash,
		"ducking_gain_db":       -3.0,
		"crossfade_duration_ms": 25,
	})
	if respMix.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("audio-mix failed: %d", respMix.StatusCode)
	}

	if mix.OverallStatus != "PASS" {
		t.Errorf("expected OverallStatus PASS, got %s", mix.OverallStatus)
	}
	if !mix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved = true")
	}
	if !mix.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed = true inside speech windows")
	}
	if mix.AudioCASHash == "" {
		t.Errorf("expected non-empty AudioCASHash for mixed track")
	}
	if mix.DurationMs <= 0 {
		t.Errorf("expected positive mixed duration, got %d", mix.DurationMs)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Mixer Purity & Overrun Refusal (Invariant 11)
// Mixer refuses candidate whose measured duration exceeds immutable slot.
// Never overlays across next block, truncates words, or shifts anchors.
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_MixerRefusesOverlongCandidate(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Create synthetic overlong DubSegment (e.g. measured 3500ms for a 2000ms slot [0, 2000])
	overlongWAV := media.GeneratePCM16WAV(16000, 1, 3500)
	wavHash, err := h.casStore.Put(bytes.NewReader(overlongWAV))
	if err != nil {
		t.Fatalf("put overlong wav in cas: %v", err)
	}

	overlongSegments := domain.DubSegmentsVariant{
		ID:             "overlong_dub_segments_1",
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              2000, // 2000ms slot
				SlotDurationMs:     2000,
				MeasuredDurationMs: 3500, // 3500ms > 2000ms overrun!
				AudioSHA256:        wavHash.SHA256,
				AudioCASPath:       wavHash.Path,
				Voice:              domain.VoiceProfile{ID: "vieneu_voice", Language: "vi"},
				FitDecision:        domain.FitActionReview,
			},
		},
	}

	// Audio role plan with [0, 2000ms]
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	source := []domain.TranslationInputSegment{{
		Index: 0, SourceText: "dialogue", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000,
	}}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)
	overlongSegments.DubScriptVariantCAS = lineage.DubScriptCAS
	overlongSegments.VoiceAssignmentCAS = lineage.VoiceAssignmentCAS
	overlongSegments.TranscriptArtifactCAS = lineage.TranscriptCAS
	overlongSegments.AudioRolePlanCAS = lineage.AudioRolePlanCAS
	overlongSegments.FitPolicyID = "seam1-overlong-fit-v1"
	overlongSegments.OverallStatus = "PASS"
	overlongSegments.Segments[0].FitDecision = domain.FitActionAccept
	overlongSegments.Segments[0].DubPlaybackEndMs = 2000
	overlongSegments.FitPlans = []domain.DubbingFitPlan{{
		SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2000, UsableSlotMs: 2000,
		MeasuredDurationMs: 3500, DubPlaybackEndMs: 2000, FitPolicyID: seam1CurrentFitPolicyID(t),
		SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
	}}
	segBytes, _ := json.Marshal(overlongSegments)
	segObj, err := h.casStore.Put(bytes.NewReader(segBytes))
	if err != nil {
		t.Fatalf("put overlong segments in cas: %v", err)
	}
	segCASHash := segObj.SHA256

	// Run audio mix with overlong dub segments
	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"dub_segments_cas": segCASHash,
	})

	// Invariant: Mixer must explicitly REFUSE the overlong candidate (HTTP 422 Unprocessable Entity)
	if respMix.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected HTTP 422 Unprocessable Entity on mixer refusal, got %d", respMix.StatusCode)
	}

	if mix == nil || mix.OverallStatus != "REFUSED" {
		t.Fatalf("expected DubMixArtifact with OverallStatus 'REFUSED', got %+v", mix)
	}

	if mix.RefusalReason == "" {
		t.Fatalf("expected descriptive RefusalReason explaining overrun")
	}
}

// ---------------------------------------------------------------------------
// Test 4: Independently testable with fake accepted dub audio without TTS dependency
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_IndependentTestingWithFakeDubAudio(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRunWithDuration(t, h, 4.0)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID
	segments := []domain.TranslationInputSegment{{
		Index: 0, SourceText: "fake source", SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000,
	}}

	// Audio role plan with [1000, 3000ms]
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}
	planResp.Body.Close()
	rolePlan, err := h.db.GetAudioRolePlan(context.Background(), assetID)
	if err != nil {
		t.Fatalf("get audio role plan: %v", err)
	}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "en", segments)

	// Directly construct an accepted DubSegmentsVariant with fake 16kHz audio clips
	clipWAV := media.GeneratePCM16WAV(16000, 1, 1500)
	clipHash, err := h.casStore.Put(bytes.NewReader(clipWAV))
	if err != nil {
		t.Fatalf("store fake dub clip: %v", err)
	}

	fakeDubVariant := domain.DubSegmentsVariant{
		ID:                    "fake_accepted_dub_variant",
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "en",
		DubScriptVariantCAS:   lineage.DubScriptCAS,
		VoiceAssignmentCAS:    lineage.VoiceAssignmentCAS,
		TranscriptArtifactCAS: lineage.TranscriptCAS,
		AudioRolePlanCAS:      rolePlan.CASHash,
		FitPolicyID:           seam1CurrentFitPolicyID(t),
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            1000,
				EndMs:              3000, // 2000ms slot
				SlotDurationMs:     2000,
				MeasuredDurationMs: 1500, // 1500ms <= 2000ms fits cleanly!
				AudioSHA256:        clipHash.SHA256,
				AudioCASPath:       clipHash.Path,
				Voice:              domain.VoiceProfile{ID: "kokoro_voice", Language: "en"},
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   3000,
			},
		},
		FitPlans: []domain.DubbingFitPlan{{
			SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2000, UsableSlotMs: 2000,
			MeasuredDurationMs: 1500, DubPlaybackEndMs: 3000, FitPolicyID: seam1CurrentFitPolicyID(t),
			SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
		}},
	}
	fakeDubVariant.CreatedAt = time.Now().UTC()
	b, _ := json.Marshal(fakeDubVariant)
	dubObj, _ := h.casStore.Put(bytes.NewReader(b))
	fakeDubVariant.CASHash = dubObj.SHA256
	b, _ = json.Marshal(fakeDubVariant)
	dubObj, _ = h.casStore.Put(bytes.NewReader(b))
	dubCAS := dubObj.SHA256

	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "en",
		"dub_segments_cas": dubCAS,
	})

	if respMix.StatusCode != http.StatusCreated || mix == nil {
		body, _ := readAll(respMix)
		t.Fatalf("expected 201 Created for audio mix with fake dub audio, got %d: %s", respMix.StatusCode, string(body))
	}
	if mix.OverallStatus != "PASS" {
		t.Errorf("expected PASS, got %s", mix.OverallStatus)
	}
	if mix.AudioCASHash == "" {
		t.Errorf("expected AudioCASHash")
	}
}

// ---------------------------------------------------------------------------
// Test 5: Audio Role Plan Branching: Singing Vocals Preserved in Soundtrack
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_SingingVocalsPreservedInSoundtrack(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRunWithDuration(t, h, 7.0)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Audio role plan with both narration and singing segments
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2001, EndMs: 6000, Role: domain.AudioRoleSingingMusicVocal},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

	// Only narration segment enters dubbing
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "这是旁白对话。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      2000,
		},
	}
	pinSeam1TranscriptForSegments(t, h, runID, assetID, segments)

	// 1. Audio role plan with both narration and singing segments
	singingPlanPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2001, EndMs: 6000, Role: domain.AudioRoleSingingMusicVocal},
		},
	}
	singingPlanBody, _ := json.Marshal(singingPlanPayload)
	singingPlanResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(singingPlanBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	defer singingPlanResp.Body.Close()

	transReq := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"segments":        segments,
	}
	respTrans, transVariant := runTranslation(t, h, assetID, transReq)
	if respTrans.StatusCode != http.StatusCreated || transVariant == nil {
		t.Fatalf("translation failed: status %d", respTrans.StatusCode)
	}

	dubReq := map[string]any{
		"run_id":                  runID,
		"target_language":         "vi",
		"translation_variant_cas": transVariant.CASHash,
	}
	respDub, dubVariant := runDubScript(t, h, assetID, dubReq)
	if respDub.StatusCode != http.StatusCreated || dubVariant == nil {
		t.Fatalf("dub script adaptation failed: status %d", respDub.StatusCode)
	}
	_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	_, dubSegments := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   voiceAssign.CASHash,
	})

	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"dub_segments_cas": dubSegments.CASHash,
	})

	if respMix.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("audio mix failed: %d", respMix.StatusCode)
	}

	if len(mix.PreservationPlan.SingingWindows) != 1 {
		t.Fatalf("expected 1 singing window in PreservationPlan, got %d", len(mix.PreservationPlan.SingingWindows))
	}
	singingWin := mix.PreservationPlan.SingingWindows[0]
	if singingWin.StartMs != 2001 || singingWin.EndMs != 6000 {
		t.Errorf("unexpected singing window bounds: %+v", singingWin)
	}
	if !mix.PreservationPlan.PreserveSinging {
		t.Errorf("expected PreserveSinging = true")
	}
}

// ---------------------------------------------------------------------------
// Test 6: Zero Spoken Speech Video (Pure Music / Visual Only) Passthrough
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_ZeroSpokenSpeech_PassthroughPreservation(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Audio role plan with 0 spoken lines (pure BGM / instrumental)
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))

	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})

	if respMix.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("audio mix failed on zero-speech video: %d", respMix.StatusCode)
	}

	if mix.OverallStatus != "PASS" {
		t.Errorf("expected PASS, got %s", mix.OverallStatus)
	}
	if mix.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed = false on pure BGM video")
	}
	if !mix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved = true")
	}
}

// ---------------------------------------------------------------------------
// Test 7: Separator Fallback (UVR -> Demucs) & Provenance Integrity
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_SeparatorFallback_UVRToDemucs(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Primary separator provider (UVR) fails
	uvrProvRaw, _ := h.registry.Get("fake_uvr_separator")
	uvrProv := uvrProvRaw.(*provider.FakeSeparatorProvider)
	uvrProv.InjectError = errors.New("UVR model failure")

	// Separate stems should fall back to Demucs
	resp, stems := runSeparateStems(t, h, assetID, map[string]any{
		"run_id": runID,
	})
	if resp.StatusCode != http.StatusCreated || stems == nil {
		t.Fatalf("expected fallback to succeed, got %d", resp.StatusCode)
	}

	if stems.ProviderID != "fake_demucs_separator" {
		t.Errorf("expected fallback ProviderID fake_demucs_separator, got %s", stems.ProviderID)
	}
	if stems.ModelName != "htdemucs" {
		t.Errorf("expected fallback ModelName htdemucs, got %s", stems.ModelName)
	}

	// Invariant: Provenance hash must reflect Demucs identity, not hardcoded UVR
	expectedProv, _ := domain.ComputeAudioStemsProvenanceHash(assetID, "fake_demucs_separator", "htdemucs", "v4")
	if stems.ProvenanceHash != expectedProv {
		t.Errorf("expected provenance hash %s, got %s", expectedProv, stems.ProvenanceHash)
	}
}

// ---------------------------------------------------------------------------
// Test 8: Non-Zero Signal Verification for Singing Preservation & Dialogue Suppression
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_SingingPreservation_SignalAcousticVerification(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Role plan: [0ms, 2000ms] is singing/music-vocal, [2000ms, 4000ms] is dialogue
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleSingingMusicVocal},
			{StartMs: 2000, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	source := []domain.TranslationInputSegment{{
		Index: 0, SourceText: "dialogue", SpeakerID: "SPEAKER_00", StartMs: 2000, EndMs: 4000,
	}}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

	// Create custom AudioStemArtifacts in CAS with known non-zero signals
	sampleRate := 16000
	channels := 1
	durMs := int64(4000)
	totalSamples := (sampleRate * int(durMs)) / 1000

	bgSamples := make([]int16, totalSamples)
	for i := range bgSamples {
		bgSamples[i] = 1000 // Constant BGM
	}
	bgWAV := media.EncodePCM16Samples(bgSamples, sampleRate, channels)
	bgObj, _ := h.casStore.Put(bytes.NewReader(bgWAV))

	vocalsSamples := make([]int16, totalSamples)
	for i := 0; i < (sampleRate*2000)/1000; i++ {
		vocalsSamples[i] = 3000 // Singing vocal in [0, 2s]
	}
	for i := (sampleRate * 2000) / 1000; i < totalSamples; i++ {
		vocalsSamples[i] = 4000 // Dialogue vocal in [2s, 4s]
	}
	vocalsWAV := media.EncodePCM16Samples(vocalsSamples, sampleRate, channels)
	vocalsObj, _ := h.casStore.Put(bytes.NewReader(vocalsWAV))

	stemsArtifact := domain.AudioStemArtifacts{
		ID:            "stems_test_signal",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_uvr_separator",
		ModelName:     "UVR-MDX-NET-Inst_HQ_4.onnx",
		ModelVersion:  "v3",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: vocalsObj.SHA256,
				SampleRate:   sampleRate,
				Channels:     channels,
				Format:       "wav",
				DurationMs:   durMs,
			},
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   sampleRate,
				Channels:     channels,
				Format:       "wav",
				DurationMs:   durMs,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemsBytes, _ := json.Marshal(stemsArtifact)
	stemsObj, _ := h.casStore.Put(bytes.NewReader(stemsBytes))

	// Create DubSegments in CAS with speech clip at [2000ms, 4000ms]
	clipSamples := make([]int16, (sampleRate*2000)/1000)
	for i := range clipSamples {
		clipSamples[i] = 5000 // Localized TTS
	}
	clipWAV := media.EncodePCM16Samples(clipSamples, sampleRate, channels)
	clipObj, _ := h.casStore.Put(bytes.NewReader(clipWAV))

	dubArtifact := domain.DubSegmentsVariant{
		ID:                    "dub_test_signal",
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   lineage.DubScriptCAS,
		VoiceAssignmentCAS:    lineage.VoiceAssignmentCAS,
		TranscriptArtifactCAS: lineage.TranscriptCAS,
		AudioRolePlanCAS:      lineage.AudioRolePlanCAS,
		FitPolicyID:           seam1CurrentFitPolicyID(t),
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            2000,
				EndMs:              4000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 2000,
				AudioSHA256:        clipObj.SHA256,
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   4000,
			},
		},
		FitPlans: []domain.DubbingFitPlan{{
			SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2000, UsableSlotMs: 2000,
			MeasuredDurationMs: 2000, DubPlaybackEndMs: 4000, FitPolicyID: seam1CurrentFitPolicyID(t),
			SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
		}},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubArtifact)
	dubObj, _ := h.casStore.Put(bytes.NewReader(dubBytes))

	// Execute MixAudio
	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"audio_stems_cas":  stemsObj.SHA256,
		"dub_segments_cas": dubObj.SHA256,
	})
	if respMix.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("mix audio failed: %d", respMix.StatusCode)
	}

	// Read mixed audio from CAS and verify signal levels
	r, err := h.casStore.Get(mix.AudioCASHash)
	if err != nil {
		t.Fatalf("failed to read mixed audio from CAS: %v", err)
	}
	defer r.Close()
	mixedData, _ := io.ReadAll(r)
	mixedSamples, _, err := media.ExtractPCM16Samples(mixedData)
	if err != nil {
		t.Fatalf("extract mixed samples failed: %v", err)
	}

	// 1. In singing window at 1000ms: BGM (1000) + Singing Vocals (3000) = 4000
	sampleAt1000ms := mixedSamples[(sampleRate*1000)/1000]
	if sampleAt1000ms != 4000 {
		t.Errorf("expected sample at 1000ms (singing preserved) to be 4000, got %d", sampleAt1000ms)
	}

	// 2. In speech window at 3000ms: BGM (1000) + Suppressed dialogue (0) + Localized Dub (5000) = 6000
	sampleAt3000ms := mixedSamples[(sampleRate*3000)/1000]
	if math.Abs(float64(sampleAt3000ms-6000)) > 50 {
		t.Errorf("expected sample at 3000ms (dialogue suppressed, dub added) to be ~6000, got %d", sampleAt3000ms)
	}

	// Invariant: CAS linkage is preserved
	if mix.AudioStemsCAS != stemsObj.SHA256 {
		t.Errorf("expected AudioStemsCAS %s, got %s", stemsObj.SHA256, mix.AudioStemsCAS)
	}
	if mix.DubSegmentsCAS != dubObj.SHA256 {
		t.Errorf("expected DubSegmentsCAS %s, got %s", dubObj.SHA256, mix.DubSegmentsCAS)
	}
}

// ---------------------------------------------------------------------------
// Test 9: Fail-Closed on Missing or Corrupt Background Stem
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_FailClosed_CorruptOrMissingBackgroundStem(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Narration requires an accepted dub artifact before the mixer can inspect soundtrack evidence.
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	source := []domain.TranslationInputSegment{{
		Index: 0, SourceText: "soundtrack", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000,
	}}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)
	dubSegmentsCAS := putAcceptedSeam1DubSegments(t, h, assetID, runID, "vi", lineage, source)

	// Stems artifact with missing background stem (only vocals)
	badStems := domain.AudioStemArtifacts{
		ID:            "bad_stems_no_bg",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: "sha256_mock_vocals",
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   2000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	badBytes, _ := json.Marshal(badStems)
	badObj, _ := h.casStore.Put(bytes.NewReader(badBytes))

	respMix, _ := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"audio_stems_cas":  badObj.SHA256,
		"dub_segments_cas": dubSegmentsCAS,
	})

	// Must fail closed with 422 or 500, never claim success or fabricate silence
	if respMix.StatusCode == http.StatusCreated || respMix.StatusCode == http.StatusOK {
		t.Fatalf("expected mix to fail closed on missing background stem, got status %d", respMix.StatusCode)
	}

	// A truncated background stem is the harder case: its RIFF/WAVE header and the
	// declared data chunk size survive the cut, so only the fail-closed
	// declared-vs-present size check can reject it. Extraction inherits that verdict
	// and the mix must refuse instead of mixing phantom audio.
	fullBG := media.GeneratePCM16WAV(16000, 1, 4000)
	truncatedBG := append([]byte(nil), fullBG[:len(fullBG)-4096]...)
	truncBGObj, err := h.casStore.Put(bytes.NewReader(truncatedBG))
	if err != nil {
		t.Fatalf("store truncated background stem: %v", err)
	}

	truncStems := domain.AudioStemArtifacts{
		ID:            "bad_stems_truncated_bg",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: truncBGObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   4000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	truncStemsBytes, _ := json.Marshal(truncStems)
	truncStemsObj, err := h.casStore.Put(bytes.NewReader(truncStemsBytes))
	if err != nil {
		t.Fatalf("store truncated stems artifact: %v", err)
	}

	respTrunc, _ := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"audio_stems_cas":  truncStemsObj.SHA256,
		"dub_segments_cas": dubSegmentsCAS,
	})
	if respTrunc.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected truncated background stem to fail closed, got status %d", respTrunc.StatusCode)
	}
	truncBody, _ := io.ReadAll(respTrunc.Body)
	if !bytes.Contains(truncBody, []byte(domain.ErrSoundtrackPreservationFailed.Error())) {
		t.Fatalf("expected soundtrack preservation failure on truncated stem, got body %s", truncBody)
	}
}

// ---------------------------------------------------------------------------
// Test 10: Speech Clip Resampling Across Different Sample Rates (24kHz -> 16kHz)
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_SpeechClipSampleRateResampling(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Audio role plan with narration/dialogue
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 500, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	_, _ = http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	source := []domain.TranslationInputSegment{{
		Index: 0, SourceText: "resample", SpeakerID: "SPEAKER_00", StartMs: 500, EndMs: 1500,
	}}
	lineage := pinSeam1DubLineage(t, h, runID, assetID, "vi", source)

	// Background stem at 16kHz
	bgWAV := media.GeneratePCM16WAV(16000, 1, 3000)
	bgObj, _ := h.casStore.Put(bytes.NewReader(bgWAV))

	stemsArtifact := domain.AudioStemArtifacts{
		ID:            "stems_resample_test",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemsBytes, _ := json.Marshal(stemsArtifact)
	stemsObj, _ := h.casStore.Put(bytes.NewReader(stemsBytes))

	// TTS speech clip synthesized at 24kHz (e.g. CosyVoice) for [500ms, 1500ms]
	clipWAV := media.GeneratePCM16WAV(24000, 1, 1000)
	clipObj, _ := h.casStore.Put(bytes.NewReader(clipWAV))

	dubArtifact := domain.DubSegmentsVariant{
		ID:                    "dub_resample_test",
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   lineage.DubScriptCAS,
		VoiceAssignmentCAS:    lineage.VoiceAssignmentCAS,
		TranscriptArtifactCAS: lineage.TranscriptCAS,
		AudioRolePlanCAS:      lineage.AudioRolePlanCAS,
		FitPolicyID:           seam1CurrentFitPolicyID(t),
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            500,
				EndMs:              1500,
				SlotDurationMs:     1000,
				MeasuredDurationMs: 1000,
				AudioSHA256:        clipObj.SHA256,
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   1500,
			},
		},
		FitPlans: []domain.DubbingFitPlan{{
			SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 1000, UsableSlotMs: 1000,
			MeasuredDurationMs: 1000, DubPlaybackEndMs: 1500, FitPolicyID: seam1CurrentFitPolicyID(t),
			SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
		}},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubArtifact)
	dubObj, _ := h.casStore.Put(bytes.NewReader(dubBytes))

	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"audio_stems_cas":  stemsObj.SHA256,
		"dub_segments_cas": dubObj.SHA256,
	})
	if respMix.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("mix audio with 24kHz speech clip failed: %d", respMix.StatusCode)
	}

	// Verify duration and sample rate
	if mix.SampleRate != 16000 {
		t.Errorf("expected mixed sample rate 16000, got %d", mix.SampleRate)
	}
	if mix.DurationMs != 3000 {
		t.Errorf("expected mixed duration 3000ms, got %dms", mix.DurationMs)
	}
}

// ---------------------------------------------------------------------------
// Test 11: Fallback Cache Reuse Across Policy-Eligible Route Order
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_FallbackCacheReuse_Demucs(t *testing.T) {
	h := setupHarness(t)
	jobID, runID1 := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. UVR fails on Run 1 -> Demucs executes and stores cached artifact
	uvrProvRaw, _ := h.registry.Get("fake_uvr_separator")
	uvrProv := uvrProvRaw.(*provider.FakeSeparatorProvider)
	uvrProv.InjectError = errors.New("UVR failure")

	resp1, stems1 := runSeparateStems(t, h, assetID, map[string]any{
		"run_id": runID1,
	})
	if resp1.StatusCode != http.StatusCreated || stems1 == nil {
		t.Fatalf("run 1 separation failed: %d", resp1.StatusCode)
	}
	if stems1.ProviderID != "fake_demucs_separator" {
		t.Fatalf("expected fallback to fake_demucs_separator on run 1, got %s", stems1.ProviderID)
	}

	// 2. On Run 2: UVR is healthy again, BUT Demucs cache already exists for this asset.
	// Invariant: Router policy-eligible order checks Demucs fallback cache, preventing redundant separation.
	uvrProv.InjectError = nil
	_, runID2 := createJobAndRun(t, h)

	resp2, stems2 := runSeparateStems(t, h, assetID, map[string]any{
		"run_id": runID2,
	})
	if resp2.StatusCode != http.StatusCreated || stems2 == nil {
		t.Fatalf("run 2 separation failed: %d", resp2.StatusCode)
	}
	if stems2.CASHash != stems1.CASHash {
		t.Errorf("expected cached CAS artifact reuse across runs (stems1: %s, stems2: %s)", stems1.CASHash, stems2.CASHash)
	}
}

// ---------------------------------------------------------------------------
// Test 12: AudioMix Fails Closed on Missing AudioRolePlan (Cannot enter dub branch)
// ---------------------------------------------------------------------------
func TestSeam1_AudioMix_MissingAudioRolePlanFailsClosed(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Deliberately do NOT save an AudioRolePlan.
	respMix, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})

	if respMix.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected HTTP 422 Unprocessable Entity on missing audio role plan, got %d", respMix.StatusCode)
	}

	if mix != nil {
		t.Fatalf("expected nil DubMixArtifact on missing audio role plan failure, got %+v", mix)
	}

	var errBody struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(respMix.Body).Decode(&errBody)
	if !strings.Contains(errBody.Error, "audio role plan required") {
		t.Errorf("expected error containing 'audio role plan required', got %q", errBody.Error)
	}

	// Verify no dub mix artifact was persisted in DB
	_, err := h.db.GetDubMixArtifactIndex(context.Background(), assetID, "vi")
	if err == nil {
		t.Errorf("expected no dub mix artifact index in db when AudioRolePlan is missing")
	}
}
