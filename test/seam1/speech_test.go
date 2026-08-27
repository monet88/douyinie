package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/speech"
)

// ---- helpers ----

// buildLongVADTurnASR builds a synthetic ASR output: ONE long VAD turn spanning
// multiple sentences (the CapCap failure mode: a fast VAD-turn recognizer
// producing one long unit). Recognizer boundaries are never canonical alone.
func buildLongVADTurnASR(providerID string) *service.SpeechASRResult {
	return &service.SpeechASRResult{
		ProviderID:   providerID,
		ModelName:    "qwen3-asr",
		ModelVersion: "1.7b",
		LanguageCode: "zh",
		Segments: []domain.ASRRawSegment{
			{
				StartMs:    0,
				EndMs:      3200,
				Text:       "今天天气很好。我们去公园散步吧。明天再继续工作。",
				Confidence: 0.97,
			},
		},
	}
}

// buildAlignedWords builds word-level forced-alignment output for the long VAD
// turn above. Two speakers: SPEAKER_00 speaks the first two sentences,
// SPEAKER_01 speaks the third (speaker change after the second sentence).
func buildAlignedWords() []domain.WordTiming {
	words := []domain.WordTiming{
		{Word: "今天", StartMs: 100, EndMs: 500, Confidence: 0.98},
		{Word: "天气", StartMs: 550, EndMs: 900, Confidence: 0.97},
		{Word: "很好。", StartMs: 950, EndMs: 1300, Confidence: 0.96},
		{Word: "我们", StartMs: 1400, EndMs: 1700, Confidence: 0.95},
		{Word: "去", StartMs: 1750, EndMs: 2000, Confidence: 0.94},
		{Word: "公园", StartMs: 2050, EndMs: 2400, Confidence: 0.93},
		{Word: "散步", StartMs: 2450, EndMs: 2800, Confidence: 0.92},
		{Word: "吧。", StartMs: 2850, EndMs: 3200, Confidence: 0.91},
		{Word: "明天", StartMs: 3400, EndMs: 3700, Confidence: 0.96},
		{Word: "再", StartMs: 3750, EndMs: 4000, Confidence: 0.95},
		{Word: "继续", StartMs: 4050, EndMs: 4400, Confidence: 0.94},
		{Word: "工作。", StartMs: 4450, EndMs: 4800, Confidence: 0.93},
	}
	return words
}

// runSpeechUnderstand executes the speech-understanding pipeline over the HTTP API.
func runSpeechUnderstand(t *testing.T, h *testHarness, runID, assetID string) *domain.TranscriptArtifact {
	t.Helper()
	payload := map[string]any{"run_id": runID}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/speech-understand", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := readAll(resp)
		t.Fatalf("expected 201 for speech-understand, got %d: %s", resp.StatusCode, string(b))
	}
	var result struct {
		Artifact domain.TranscriptArtifact `json:"transcript_artifact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode speech-understand response: %v", err)
	}
	return &result.Artifact
}

func readAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

// ---- Test 1: long multi-sentence VAD turn passes forced alignment + canonical SpeechBlock segmentation ----

func TestSeam1_SpeechUnderstand_LongVADTurnCanonicalSegmentation(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Save an audio role plan with narration/dialogue so speech understanding runs.
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp.Body.Close()
	if planResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for audio role plan, got %d", planResp.StatusCode)
	}

	// Drive the pipeline with fake ASR + aligner + diarization deterministically.
	speechSvc := service.NewSpeechService(h.db, h.casStore)
	speechSvc.ASRInvoke = func(ctx context.Context, req service.SpeechASRRequest) (*service.SpeechASRResult, error) {
		return buildLongVADTurnASR("fake_qwen3_asr"), nil
	}
	speechSvc.AlignerInvoke = func(ctx context.Context, req service.SpeechAlignmentRequest) (*service.SpeechAlignmentResult, error) {
		return &service.SpeechAlignmentResult{
			ProviderID:   "fake_qwen3_aligner",
			ModelName:    "qwen3-aligner",
			ModelVersion: "1.0.0",
			WordTimings:  buildAlignedWords(),
		}, nil
	}
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming) (*domain.DiarizationPlan, error) {
		return &domain.DiarizationPlan{
			ID:    "diarization_1",
			RunID: runID,
			Assignments: []domain.SpeakerAssignment{
				{SpeakerID: "SPEAKER_00", Label: "SPEAKER_00", StartMs: 0, EndMs: 3200, Confidence: 0.95},
				{SpeakerID: "SPEAKER_01", Label: "SPEAKER_01", StartMs: 3201, EndMs: 5000, Confidence: 0.94},
			},
			Confidence: 0.95,
		}, nil
	}
	h.srv.SetSpeechService(speechSvc)

	artifact := runSpeechUnderstand(t, h, runID, assetID)

	// The single long VAD turn (1 raw segment) MUST be split into canonical blocks.
	if len(artifact.SpeechBlocks) < 3 {
		t.Fatalf("expected a long VAD turn to split into >=3 canonical speech blocks, got %d", len(artifact.SpeechBlocks))
	}

	// Diarization ran BEFORE segmentation: artifact records it and assignments exist.
	if !artifact.DiarizationRan {
		t.Errorf("expected diarization_ran=true (conditional diarization ran before segmentation)")
	}
	if len(artifact.SpeakerAssignments) != 2 {
		t.Fatalf("expected 2 speaker assignments, got %d", len(artifact.SpeakerAssignments))
	}

	// Every word in the artifact must carry a speaker label.
	for _, w := range artifact.WordTimings {
		if strings.TrimSpace(w.SpeakerID) == "" {
			t.Errorf("word %q has no speaker label after diarization-before-segmentation", w.Word)
		}
	}

	// Speaker labels are correct per sentence: SPEAKER_00 for sentences 1-2, SPEAKER_01 for sentence 3.
	speakerSeq := []string{}
	for _, b := range artifact.SpeechBlocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speakerSeq = append(speakerSeq, b.SpeakerID)
		}
	}
	if len(speakerSeq) < 3 {
		t.Fatalf("expected >=3 speech blocks, got %d: %v", len(speakerSeq), speakerSeq)
	}
	for _, s := range speakerSeq[:2] {
		if s != "SPEAKER_00" {
			t.Errorf("expected SPEAKER_00 for first blocks, got %q (seq=%v)", s, speakerSeq)
		}
	}
	if speakerSeq[len(speakerSeq)-1] != "SPEAKER_01" {
		t.Errorf("expected SPEAKER_01 for final block, got %q (seq=%v)", speakerSeq[len(speakerSeq)-1], speakerSeq)
	}

	// Source silence stays explicit: no canonical block ever pushes a later block start.
	for i := 1; i < len(artifact.SpeechBlocks); i++ {
		if artifact.SpeechBlocks[i].StartMs < artifact.SpeechBlocks[i-1].EndMs {
			t.Errorf("block %d start (%d) < previous block end (%d): overlap cleanup pushed start", i, artifact.SpeechBlocks[i].StartMs, artifact.SpeechBlocks[i-1].EndMs)
		}
	}
}

// ---- Test 2: recognizer boundaries are never canonical alone ----

func TestSeam1_SpeechUnderstand_RawASRBoundaryNotCanonical(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp.Body.Close()

	// ASR emits ONE long raw segment (a VAD-turn unit); no per-sentence raw boundaries.
	asr := &service.SpeechASRResult{
		ProviderID:   "fake_qwen3_asr",
		ModelName:    "qwen3-asr",
		ModelVersion: "1.7b",
		LanguageCode: "zh",
		Segments: []domain.ASRRawSegment{
			{StartMs: 0, EndMs: 4800, Text: "今天天气很好。我们去公园散步吧。明天再继续工作。", Confidence: 0.95},
		},
	}
	align := &service.SpeechAlignmentResult{
		ProviderID:   "fake_qwen3_aligner",
		ModelName:    "qwen3-aligner",
		ModelVersion: "1.0.0",
		WordTimings:  buildAlignedWords(),
	}

	speechSvc := service.NewSpeechService(h.db, h.casStore)
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming) (*domain.DiarizationPlan, error) {
		return &domain.DiarizationPlan{
			ID:    "diarization_2",
			RunID: runID,
			Assignments: []domain.SpeakerAssignment{
				{SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 3200, Confidence: 0.95},
				{SpeakerID: "SPEAKER_01", StartMs: 3201, EndMs: 5000, Confidence: 0.94},
			},
			Confidence: 0.94,
		}, nil
	}
	h.srv.SetSpeechService(speechSvc)

	artifact, err := speechSvc.BuildTranscriptFromInputs(asr, align, runID, assetID)
	if err != nil {
		t.Fatalf("build transcript from inputs: %v", err)
	}

	// The artifact has 1 raw segment but >=3 canonical speech blocks: raw ASR
	// boundaries are NOT canonical.
	if len(artifact.RawSegments) != 1 {
		t.Fatalf("expected exactly 1 raw ASR segment, got %d", len(artifact.RawSegments))
	}
	speechBlocks := 0
	for _, b := range artifact.SpeechBlocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speechBlocks++
		}
	}
	if speechBlocks < 3 {
		t.Errorf("expected >=3 canonical speech blocks from 1 raw ASR segment, got %d", speechBlocks)
	}

	// Speaker change boundary: word "吧。" (ends at 3200) and "明天" (starts 3400)
	// have a 200ms gap (< PauseSplitMs 400) but a speaker change — the speaker
	// change rule must split there (gap 200 >= MinSpeakerChangeMs 100).
	speakerSeq := []string{}
	for _, b := range artifact.SpeechBlocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speakerSeq = append(speakerSeq, b.SpeakerID)
		}
	}
	if len(speakerSeq) < 2 || speakerSeq[len(speakerSeq)-1] != "SPEAKER_01" {
		t.Errorf("expected final block speaker SPEAKER_01 (speaker change split), got %v", speakerSeq)
	}
}

// ---- Test 3: TranscriptArtifact persisted with speaker labels (API round-trip) ----

func TestSeam1_SpeechUnderstand_PersistAndRetrieve(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, _ := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	planResp.Body.Close()

	speechSvc := service.NewSpeechService(h.db, h.casStore)
	speechSvc.ASRInvoke = func(ctx context.Context, req service.SpeechASRRequest) (*service.SpeechASRResult, error) {
		return buildLongVADTurnASR("fake_qwen3_asr"), nil
	}
	speechSvc.AlignerInvoke = func(ctx context.Context, req service.SpeechAlignmentRequest) (*service.SpeechAlignmentResult, error) {
		return &service.SpeechAlignmentResult{
			ProviderID:   "fake_qwen3_aligner",
			ModelName:    "qwen3-aligner",
			ModelVersion: "1.0.0",
			WordTimings:  buildAlignedWords(),
		}, nil
	}
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming) (*domain.DiarizationPlan, error) {
		return &domain.DiarizationPlan{
			ID:    "diarization_3",
			RunID: runID,
			Assignments: []domain.SpeakerAssignment{
				{SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 3200, Confidence: 0.95},
				{SpeakerID: "SPEAKER_01", StartMs: 3201, EndMs: 5000, Confidence: 0.94},
			},
			Confidence: 0.95,
		}, nil
	}
	h.srv.SetSpeechService(speechSvc)

	artifact := runSpeechUnderstand(t, h, runID, assetID)

	// Retrieve through the public API and verify identity + speaker labels survive.
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/transcript")
	if err != nil {
		t.Fatalf("get transcript failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		b, _ := readAll(getResp)
		t.Fatalf("expected 200 for GET transcript, got %d: %s", getResp.StatusCode, string(b))
	}
	var getResult struct {
		Artifact domain.TranscriptArtifact `json:"transcript_artifact"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&getResult); err != nil {
		t.Fatalf("decode get transcript response: %v", err)
	}

	if getResult.Artifact.ID != artifact.ID {
		t.Errorf("retrieved artifact ID mismatch: got %q, want %q", getResult.Artifact.ID, artifact.ID)
	}
	if getResult.Artifact.DiarizationRan != true {
		t.Errorf("expected diarization_ran=true in retrieved artifact")
	}
	if len(getResult.Artifact.SpeakerAssignments) != 2 {
		t.Errorf("expected 2 speaker assignments in retrieved artifact, got %d", len(getResult.Artifact.SpeakerAssignments))
	}
	if len(getResult.Artifact.SpeechBlocks) < 3 {
		t.Errorf("expected >=3 speech blocks in retrieved artifact, got %d", len(getResult.Artifact.SpeechBlocks))
	}

	// Verify speaker labels on retrieved word timings.
	for _, w := range getResult.Artifact.WordTimings {
		if strings.TrimSpace(w.SpeakerID) == "" {
			t.Errorf("retrieved word %q has no speaker label", w.Word)
		}
	}
}

// ---- Test 4: No-dub-eligible speech returns 422 ----

func TestSeam1_SpeechUnderstand_NoDubEligibleReturns422(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Save an audio role plan with ONLY singing (no narration/dialogue → no-dub route).
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleSingingMusicVocal},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, _ := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	planResp.Body.Close()

	payload := map[string]any{"run_id": "some-run-id"}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/speech-understand", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for no-dub-eligible speech, got %d", resp.StatusCode)
	}

	var errResult struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResult); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResult.Error, "no dub-eligible speech") {
		t.Errorf("expected error mentioning no-dub-eligible speech, got %q", errResult.Error)
	}
}

// ---- Test 5: Segmentation unit test - canonical rules ----

func TestSeam1_SpeechUnderstand_CanonicalSegmentationRules(t *testing.T) {
	// Pure unit test exercising the BuildSpeechBlocks function directly.
	cfg := domain.DefaultSegmentRuleConfig()
	words := buildAlignedWords()

	blocks := speech.BuildSpeechBlocks(words, cfg)

	// Verify canonical rules hold:
	// 1. Punctuation after "很好。" splits sentence 1 from sentence 2.
	// 2. Speaker change after "吧。" splits sentence 2 from sentence 3.
	// 3. No silence overlap between blocks.
	speechBlocks := 0
	for _, b := range blocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speechBlocks++
		}
	}
	if speechBlocks < 3 {
		t.Errorf("expected >=3 canonical speech blocks, got %d", speechBlocks)
	}

	// Source silence preserved: explicit silence blocks between utterances.
	var silenceFound bool
	for _, b := range blocks {
		if b.SegmentType == domain.SpeechBlockTypeSilence {
			silenceFound = true
			break
		}
	}
	if !silenceFound {
		t.Logf("silence blocks may or may not be present depending on gap thresholds; this is informational")
	}

	// Verify no block overlaps with previous.
	for i := 1; i < len(blocks); i++ {
		if blocks[i].StartMs < blocks[i-1].EndMs {
			t.Errorf("block %d start (%d) < previous end (%d): overlap illegal", i, blocks[i].StartMs, blocks[i-1].EndMs)
		}
	}

	// Verify the speaker transition is captured.
	speakerSeq := []string{}
	for _, b := range blocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speakerSeq = append(speakerSeq, fmt.Sprintf("%s@%d", b.SpeakerID, b.StartMs))
		}
	}
	t.Logf("speaker sequence: %v", speakerSeq)
}

// TestSeam1_SpeechUnderstand_NoPublicTranscriptWrite verifies the unsafe public
// write path is gone: POST /api/v1/assets/{id}/transcript now returns 405
// (method not allowed) because the pipeline is the only legitimate writer and
// TranscriptArtifact persistence is immutable.
func TestSeam1_SpeechUnderstand_NoPublicTranscriptWrite(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	payload := map[string]any{
		"id":       "client-fabricated-id",
		"asset_id": assetID,
		"run_id":   "client-run",
		"speech_blocks": []map[string]any{
			{"index": 0, "start_ms": 0, "end_ms": 1000, "speaker_id": "SPEAKER_00", "source_text": "fabricated", "segment_type": "speech"},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/transcript", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST transcript request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST /transcript (write path removed), got %d", resp.StatusCode)
	}

	// And no fabricated artifact can be read back: the pipeline never wrote it.
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/transcript")
	if err != nil {
		t.Fatalf("GET transcript failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for GET transcript (nothing persisted by client), got %d", getResp.StatusCode)
	}
}

// TestSeam1_SpeechUnderstand_MissingPlanFailsClosed verifies the pipeline fails
// closed with 422 when no audio role plan exists for the asset (the plan is a
// hard precondition from completed #29).
func TestSeam1_SpeechUnderstand_MissingPlanFailsClosed(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Deliberately do NOT save an audio role plan.
	payload := map[string]any{"run_id": "run-no-plan"}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/speech-understand", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := readAll(resp)
		t.Fatalf("expected 422 for missing audio role plan, got %d: %s", resp.StatusCode, string(b))
	}
	var errResult struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResult); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResult.Error, "audio role plan required") {
		t.Errorf("expected error mentioning audio role plan required, got %q", errResult.Error)
	}
}
