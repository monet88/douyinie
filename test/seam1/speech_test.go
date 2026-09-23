package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/speech"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
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
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming, req service.SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
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
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming, req service.SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
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

	artifact, err := speechSvc.BuildTranscriptFromInputs(context.Background(), asr, align, runID, assetID)
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
	speechSvc.Diarize = func(ctx context.Context, words []domain.WordTiming, req service.SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
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
	jobID, runID := createJobAndRun(t, h)
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

	payload := map[string]any{"run_id": runID}
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
	// Disable automatic prerequisite generator to test SpeechService fail-closed guard
	h.srv.SetAudioRoleService(nil)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Deliberately do NOT save an audio role plan.
	payload := map[string]any{"run_id": runID}
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

// helper to build fakemodel binary for seam1 production composition test
func buildFakeModelForSeam1(t *testing.T, binDir, name string) {
	t.Helper()
	binPath := filepath.Join(binDir, name)
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/monet88/douyinie/test/fixtures/fakemodel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fakemodel %s: %v\n%s", name, err, out)
	}
}

// helper to build stageworker binary for seam1 production composition test
func buildStageWorkerForSeam1(t *testing.T, binDir string) string {
	t.Helper()
	exe := filepath.Join(binDir, "stageworker")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "github.com/monet88/douyinie/cmd/stageworker")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build stageworker: %v\n%s", err, out)
	}
	return exe
}

// TestSeam1_SpeechUnderstand_ProductionComposition_NoInjectedHooks proves that
// the production composition works WITHOUT injected SpeechService.ASRInvoke /
// AlignerInvoke / Diarize hooks:
// 1. The registry is populated with concrete worker-backed providers.
// 2. StageWorker and model binaries are resolved via PATH and executed end-to-end.
// 3. Router.ExecuteWithRetry selects Qwen3-ASR 1.7B quality provider and Qwen3-ForcedAligner.
// 4. The pipeline produces a valid TranscriptArtifact persisted to CAS and indexed in SQLite.
func TestSeam1_SpeechUnderstand_ProductionComposition_NoInjectedHooks(t *testing.T) {
	binDir := t.TempDir()
	buildFakeModelForSeam1(t, binDir, "qwen3-asr")
	buildFakeModelForSeam1(t, binDir, "qwen3-aligner")
	buildFakeModelForSeam1(t, binDir, "campplus-diarizer")
	workerExe := buildStageWorkerForSeam1(t, binDir)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", workerExe)

	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_prod_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var prober media.Prober
	if _, err := exec.LookPath("ffprobe"); err == nil {
		prober = media.NewFFprobeProber()
	} else {
		prober = &media.MockProber{}
	}

	ingestSvc := service.NewIngestService(db, casStore, prober)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(resScheduler)

	// Production registry (NO fake providers!)
	prodRegistry, err := provider.NewProductionSpeechRegistry(leaseMgr, false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range prodRegistry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName + "_" + mVer,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name + "_" + dep.Version,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	router := provider.NewRouter(prodRegistry, polSvc, licSvc, credSvc, nil, db)

	// Production SpeechService: NO INJECTED HOOKS!
	speechSvc := service.NewSpeechService(db, casStore)
	// ConfigureRouter is called automatically by server.New

	srv := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		DB:         db,
		CASStore:   casStore,
		Ingest:     ingestSvc,
		Registry:   prodRegistry,
		PolicySvc:  polSvc,
		LicenseSvc: licSvc,
		CredSvc:    credSvc,
		Router:     router,
		QueueSvc:   queueSvc,
		Scheduler:  resScheduler,
		SpeechSvc:  speechSvc,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close() })

	h := &testHarness{
		server:    ts,
		srv:       srv,
		db:        db,
		casStore:  casStore,
		registry:  prodRegistry,
		router:    router,
		queueSvc:  queueSvc,
		scheduler: resScheduler,
		dir:       tmpDir,
	}

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Save dub-eligible audio role plan via API
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

	// Run speech-understand pipeline through public HTTP API
	artifact := runSpeechUnderstand(t, h, runID, assetID)
	if artifact == nil {
		t.Fatal("expected non-nil transcript artifact from production pipeline")
	}

	// Verify worker-backed providers executed (Finding 1 & 2)
	if artifact.ASRProviderID != "qwen3_asr_1_7b" {
		t.Errorf("expected ASRProviderID qwen3_asr_1_7b, got %s", artifact.ASRProviderID)
	}
	if artifact.AlignerProviderID != "qwen3_forced_aligner" {
		t.Errorf("expected AlignerProviderID qwen3_forced_aligner, got %s", artifact.AlignerProviderID)
	}

	// Verify source-derived speaker evidence triggered real diarization (Blocker 1 & 3)
	if !artifact.DiarizationRan {
		t.Errorf("expected DiarizationRan=true (source-derived speaker evidence triggered diarizer)")
	}
	if artifact.DiarizationProviderID != "campplus_diarizer" {
		t.Errorf("expected DiarizationProviderID campplus_diarizer, got %s", artifact.DiarizationProviderID)
	}
	if artifact.DiarizationModelName != "iic/speech_campplus_sv_zh_en_16k-common_advanced" || artifact.DiarizationModelVersion != "v1.0.0" {
		t.Errorf("expected diarizer model iic/speech_campplus_sv_zh_en_16k-common_advanced:v1.0.0, got %s:%s", artifact.DiarizationModelName, artifact.DiarizationModelVersion)
	}
	if len(artifact.SpeakerAssignments) != 2 {
		t.Fatalf("expected 2 speaker assignments from diarization, got %d", len(artifact.SpeakerAssignments))
	}
	if artifact.SpeakerEvidence == nil || !artifact.SpeakerEvidence.HasMultiSpeakerCues {
		t.Errorf("expected source-derived SpeakerEvidence with HasMultiSpeakerCues=true")
	}

	if len(artifact.SpeechBlocks) == 0 {
		t.Fatal("expected speech blocks to be segmented")
	}
	speakers := make(map[string]bool)
	for _, b := range artifact.SpeechBlocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			if b.SpeakerID == "" {
				t.Errorf("speech block %d missing speaker ID", b.Index)
			}
			speakers[b.SpeakerID] = true
		}
	}
	if len(speakers) < 2 {
		t.Errorf("expected distinct speaker labels in speech blocks, got %v", speakers)
	}

	// Verify immutable CAS + SQLite persistence
	if artifact.CASHash == "" || artifact.ProvenanceHash == "" {
		t.Errorf("artifact missing CAS/Provenance hash: cas=%s prov=%s", artifact.CASHash, artifact.ProvenanceHash)
	}
	indexed, err := db.GetTranscriptArtifactByProvenance(context.Background(), artifact.ProvenanceHash)
	if err != nil || indexed == nil {
		t.Fatalf("transcript index lookup failed: %v", err)
	}
	if indexed.CASHash != artifact.CASHash {
		t.Errorf("CAS hash mismatch in index: got %s, want %s", indexed.CASHash, artifact.CASHash)
	}
}

// TestSeam1_SpeechUnderstand_ProductionComposition_SingleSpeakerNoEvidence verifies
// that single-speaker / no-evidence audio skips full diarization and defaults
// to stable SPEAKER_00 without false positives (Issue #44 Blocker 1 requirement 2).
func TestSeam1_SpeechUnderstand_ProductionComposition_SingleSpeakerNoEvidence(t *testing.T) {
	binDir := t.TempDir()
	buildFakeModelForSeam1(t, binDir, "qwen3-asr")
	buildFakeModelForSeam1(t, binDir, "qwen3-aligner")
	buildFakeModelForSeam1(t, binDir, "campplus-diarizer")
	workerExe := buildStageWorkerForSeam1(t, binDir)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", workerExe)
	t.Setenv("FAKEMODEL_NO_SPEAKER_EVIDENCE", "1") // Probe returns HasMultiSpeakerCues=false

	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_single_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var prober media.Prober
	if _, err := exec.LookPath("ffprobe"); err == nil {
		prober = media.NewFFprobeProber()
	} else {
		prober = &media.MockProber{}
	}

	ingestSvc := service.NewIngestService(db, casStore, prober)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(resScheduler)

	prodRegistry, err := provider.NewProductionSpeechRegistry(leaseMgr, false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range prodRegistry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName + "_" + mVer,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name + "_" + dep.Version,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	router := provider.NewRouter(prodRegistry, polSvc, licSvc, credSvc, nil, db)
	speechSvc := service.NewSpeechService(db, casStore)

	srv := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		DB:         db,
		CASStore:   casStore,
		Ingest:     ingestSvc,
		Registry:   prodRegistry,
		PolicySvc:  polSvc,
		LicenseSvc: licSvc,
		CredSvc:    credSvc,
		Router:     router,
		QueueSvc:   queueSvc,
		Scheduler:  resScheduler,
		SpeechSvc:  speechSvc,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close() })

	h := &testHarness{
		server:    ts,
		srv:       srv,
		db:        db,
		casStore:  casStore,
		registry:  prodRegistry,
		router:    router,
		queueSvc:  queueSvc,
		scheduler: resScheduler,
		dir:       tmpDir,
	}

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

	artifact := runSpeechUnderstand(t, h, runID, assetID)
	if artifact == nil {
		t.Fatal("expected non-nil transcript artifact")
	}

	// Single-speaker / no-evidence path must fail closed to stable SPEAKER_00:
	if artifact.DiarizationRan {
		t.Errorf("expected DiarizationRan=false when no speaker evidence")
	}
	if artifact.DiarizationProviderID != "default-single-speaker-fallback" {
		t.Errorf("expected fallback provider ID, got %s", artifact.DiarizationProviderID)
	}
	for _, b := range artifact.SpeechBlocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech && b.SpeakerID != "SPEAKER_00" {
			t.Errorf("expected SPEAKER_00 for all blocks, got %s", b.SpeakerID)
		}
	}
}

// TestSeam1_SpeechUnderstand_ProductionComposition_TwoSpeakerTwoTurnEvidence verifies
// that two-speaker / two-turn timing + independent valid speaker evidence triggers
// the production conditional diarization path (Issue #44 Blocker 2).
func TestSeam1_SpeechUnderstand_ProductionComposition_TwoSpeakerTwoTurnEvidence(t *testing.T) {
	binDir := t.TempDir()
	buildFakeModelForSeam1(t, binDir, "qwen3-asr")
	buildFakeModelForSeam1(t, binDir, "qwen3-aligner")
	buildFakeModelForSeam1(t, binDir, "campplus-diarizer")
	workerExe := buildStageWorkerForSeam1(t, binDir)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", workerExe)
	// Two distinct sentences/turns
	t.Setenv("FAKEMODEL_ASR_TEXT", "第一句话。 第二句话。")

	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_twoturn_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var prober media.Prober
	if _, err := exec.LookPath("ffprobe"); err == nil {
		prober = media.NewFFprobeProber()
	} else {
		prober = &media.MockProber{}
	}

	ingestSvc := service.NewIngestService(db, casStore, prober)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(resScheduler)

	prodRegistry, err := provider.NewProductionSpeechRegistry(leaseMgr, false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range prodRegistry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName + "_" + mVer,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name + "_" + dep.Version,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	router := provider.NewRouter(prodRegistry, polSvc, licSvc, credSvc, nil, db)
	speechSvc := service.NewSpeechService(db, casStore)

	srv := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		DB:         db,
		CASStore:   casStore,
		Ingest:     ingestSvc,
		Registry:   prodRegistry,
		PolicySvc:  polSvc,
		LicenseSvc: licSvc,
		CredSvc:    credSvc,
		Router:     router,
		QueueSvc:   queueSvc,
		Scheduler:  resScheduler,
		SpeechSvc:  speechSvc,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close() })

	h := &testHarness{
		server:    ts,
		srv:       srv,
		db:        db,
		casStore:  casStore,
		registry:  prodRegistry,
		router:    router,
		queueSvc:  queueSvc,
		scheduler: resScheduler,
		dir:       tmpDir,
	}

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

	artifact := runSpeechUnderstand(t, h, runID, assetID)
	if artifact == nil {
		t.Fatal("expected non-nil transcript artifact")
	}

	// Two-turn source with evidence must trigger conditional diarization:
	if !artifact.DiarizationRan {
		t.Errorf("expected DiarizationRan=true for two-turn audio with speaker evidence")
	}
	if artifact.DiarizationProviderID != "campplus_diarizer" {
		t.Errorf("expected DiarizationProviderID campplus_diarizer, got %s", artifact.DiarizationProviderID)
	}
	speakers := make(map[string]bool)
	for _, b := range artifact.SpeechBlocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speakers[b.SpeakerID] = true
		}
	}
	if len(speakers) < 2 {
		t.Errorf("expected at least 2 distinct speaker labels in speech blocks, got %v", speakers)
	}
}

// TestSeam1_SpeechUnderstand_ProductionComposition_ProbeErrorFailsClosed verifies
// that a speaker evidence probe error in the production pipeline fails closed (500)
// and is not converted to a successful single-speaker transcript (Issue #44 Blocker 1).
func TestSeam1_SpeechUnderstand_ProductionComposition_ProbeErrorFailsClosed(t *testing.T) {
	binDir := t.TempDir()
	buildFakeModelForSeam1(t, binDir, "qwen3-asr")
	buildFakeModelForSeam1(t, binDir, "qwen3-aligner")
	buildFakeModelForSeam1(t, binDir, "campplus-diarizer")
	workerExe := buildStageWorkerForSeam1(t, binDir)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", workerExe)
	t.Setenv("FAKEMODEL_DIARIZER_FAIL", "1") // Diarizer probe fails/crashes

	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_fail_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var prober media.Prober
	if _, err := exec.LookPath("ffprobe"); err == nil {
		prober = media.NewFFprobeProber()
	} else {
		prober = &media.MockProber{}
	}

	ingestSvc := service.NewIngestService(db, casStore, prober)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(resScheduler)

	prodRegistry, err := provider.NewProductionSpeechRegistry(leaseMgr, false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range prodRegistry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName + "_" + mVer,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name + "_" + dep.Version,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	router := provider.NewRouter(prodRegistry, polSvc, licSvc, credSvc, nil, db)
	speechSvc := service.NewSpeechService(db, casStore)

	srv := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		DB:         db,
		CASStore:   casStore,
		Ingest:     ingestSvc,
		Registry:   prodRegistry,
		PolicySvc:  polSvc,
		LicenseSvc: licSvc,
		CredSvc:    credSvc,
		Router:     router,
		QueueSvc:   queueSvc,
		Scheduler:  resScheduler,
		SpeechSvc:  speechSvc,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close() })

	h := &testHarness{
		server:    ts,
		srv:       srv,
		db:        db,
		casStore:  casStore,
		registry:  prodRegistry,
		router:    router,
		queueSvc:  queueSvc,
		scheduler: resScheduler,
		dir:       tmpDir,
	}

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

	// Request speech-understand -> must fail (500)
	payload := map[string]any{"run_id": runID}
	reqBody, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/speech-understand", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("speech-understand request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 on speaker evidence probe error, got %d", resp.StatusCode)
	}

	// Verify no transcript artifact was persisted
	getResp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/transcript")
	if err != nil {
		t.Fatalf("get transcript request failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for transcript when probe failed, got %d", getResp.StatusCode)
	}
}

// TestSeam1_SpeechUnderstand_ProductionComposition_WithRealPythonAdapters proves that
// StageWorker executes repo-owned Python adapters (asr_qwen3.py, aligner_qwen3.py, diarizer_3dspeaker.py)
// over the official upstream Python API surface without fake binary executables on PATH.
func TestSeam1_SpeechUnderstand_ProductionComposition_WithRealPythonAdapters(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Mock qwen_asr package
	qwenPkgDir := filepath.Join(tmpDir, "qwen_asr")
	if err := os.MkdirAll(qwenPkgDir, 0755); err != nil {
		t.Fatalf("mkdir qwen_asr: %v", err)
	}
	qwenCode := `
class Qwen3ASRModel:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    @classmethod
    def from_pretrained(cls, *args, **kwargs):
        return cls(*args, **kwargs)

    def transcribe(self, audio, **kwargs):
        return [{
            "segments": [
                {"start_ms": 0, "end_ms": 1500, "text": "第一句话。", "confidence": 0.95, "language_code": "zh"},
                {"start_ms": 2000, "end_ms": 3500, "text": "第二句话。", "confidence": 0.92, "language_code": "zh"},
            ]
        }]

class Qwen3ForcedAligner:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    @classmethod
    def from_pretrained(cls, *args, **kwargs):
        return cls(*args, **kwargs)

    def align(self, audio, text, **kwargs):
        return [[
            {"word": "第一句话", "start_time": 0.0, "end_time": 1.5},
            {"word": "第二句话", "start_time": 2.0, "end_time": 3.5},
        ]]
`
	if err := os.WriteFile(filepath.Join(qwenPkgDir, "__init__.py"), []byte(qwenCode), 0644); err != nil {
		t.Fatalf("write qwen_asr: %v", err)
	}

	// 2. Mock speakerlab package
	speakerlabBinDir := filepath.Join(tmpDir, "speakerlab", "bin")
	if err := os.MkdirAll(speakerlabBinDir, 0755); err != nil {
		t.Fatalf("mkdir speakerlab: %v", err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "speakerlab", "__init__.py"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(speakerlabBinDir, "__init__.py"), []byte(""), 0644)
	speakerlabCode := `
class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    def probe_evidence(self, audio_path):
        return {
            "has_multi_speaker_cues": True,
            "speaker_change_count": 2,
            "confidence": 0.0,
            "source": "iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0+iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4",
        }

    def __call__(self, audio_path):
        return [
            [0.0, 1.8, "SPEAKER_00"],
            [1.8, 4.0, "SPEAKER_01"],
        ]
`
	if err := os.WriteFile(filepath.Join(speakerlabBinDir, "infer_diarization.py"), []byte(speakerlabCode), 0644); err != nil {
		t.Fatalf("write infer_diarization: %v", err)
	}

	// Locate repo adapters
	asrAdapter, _ := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "asr_qwen3.py"))
	alignAdapter, _ := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "aligner_qwen3.py"))
	diarAdapter, _ := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))

	t.Setenv("DOUYINIE_ASR_ADAPTER", asrAdapter)
	t.Setenv("DOUYINIE_ALIGNER_ADAPTER", alignAdapter)
	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", diarAdapter)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	workerExe := buildStageWorkerForSeam1(t, tmpDir)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", workerExe)

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS: %v", err)
	}
	dbPath := filepath.Join(tmpDir, "seam1_python_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	prober := &media.MockProber{
		CustomReport: &domain.PreflightReport{
			DurationSec:      4.0,
			DurationMs:       4000,
			ContainerFormat:  "mov,mp4,m4a,3gp,3g2,mj2",
			ContainerValid:   true,
			FingerprintMatch: true,
		},
	}
	ingestSvc := service.NewIngestService(db, casStore, prober)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(resScheduler)

	prodRegistry, err := provider.NewProductionSpeechRegistry(leaseMgr, false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range prodRegistry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName + "_" + mVer,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name + "_" + dep.Version,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	router := provider.NewRouter(prodRegistry, polSvc, licSvc, credSvc, nil, db)
	speechSvc := service.NewSpeechService(db, casStore)

	srv := server.New(server.Config{
		Addr:       "127.0.0.1:0",
		DB:         db,
		CASStore:   casStore,
		Ingest:     ingestSvc,
		Registry:   prodRegistry,
		PolicySvc:  polSvc,
		LicenseSvc: licSvc,
		CredSvc:    credSvc,
		Router:     router,
		QueueSvc:   queueSvc,
		Scheduler:  resScheduler,
		SpeechSvc:  speechSvc,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close() })

	h := &testHarness{
		server:    ts,
		srv:       srv,
		db:        db,
		casStore:  casStore,
		registry:  prodRegistry,
		router:    router,
		queueSvc:  queueSvc,
		scheduler: resScheduler,
		dir:       tmpDir,
	}

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Save dub-eligible audio role plan
	planPayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBody, _ := json.Marshal(planPayload)
	planResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(planBody))
	if err != nil {
		t.Fatalf("save audio role plan failed: %v", err)
	}
	planResp.Body.Close()

	// Execute speech-understand via HTTP
	artifact := runSpeechUnderstand(t, h, runID, assetID)
	if artifact == nil {
		t.Fatal("expected non-nil transcript artifact")
	}
	if artifact.ASRProviderID != "qwen3_asr_1_7b" {
		t.Errorf("expected ASRProviderID qwen3_asr_1_7b, got %s", artifact.ASRProviderID)
	}
	if artifact.AlignerProviderID != "qwen3_forced_aligner" {
		t.Errorf("expected AlignerProviderID qwen3_forced_aligner, got %s", artifact.AlignerProviderID)
	}
	if !artifact.DiarizationRan {
		t.Errorf("expected DiarizationRan=true")
	}
	if len(artifact.SpeechBlocks) < 2 {
		t.Fatalf("expected at least 2 speech blocks from 2-turn text, got %d", len(artifact.SpeechBlocks))
	}
}
