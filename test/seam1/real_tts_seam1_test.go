package seam1_test

import (
	"context"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func resolveTestTTSPythonSeam1(t *testing.T) string {
	t.Helper()
	if py := os.Getenv("DOUYINIE_TTS_PYTHON_BIN"); py != "" {
		if _, err := os.Stat(py); err == nil {
			return py
		}
	}
	t.Skip("isolated TTS python environment not configured: set DOUYINIE_TTS_PYTHON_BIN to run real TTS model tests")
	return ""
}

func setupRealTTSHarness(t *testing.T) *testHarness {
	t.Helper()
	h := setupHarness(t)

	// Build real worker-backed TTS provider pointing to StageWorker
	vieneuWorker, err := provider.NewWorkerTTSProvider("vieneu_tts_vi", "vieneu-tts", "1.0.0", []string{"vi"}, 0.95)
	if err != nil {
		t.Fatalf("create WorkerTTSProvider: %v", err)
	}

	licSvc := governance.NewLicenseService(h.db)
	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		DependencyName: "vieneu-tts",
		Version:        "1.0.0",
		SHA256:         "sha256_mock_vieneu-tts",
		SourceRepo:     "github.com/monet88/douyinie/models/vieneu-tts",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	// Register in the harness registry
	if err := h.registry.Register(vieneuWorker); err != nil {
		t.Fatalf("register WorkerTTSProvider: %v", err)
	}

	return h
}

func TestSeam1_TTS_RealModelSynthesis_FitGateAndZeroOverrun(t *testing.T) {
	ttsPy := resolveTestTTSPythonSeam1(t)
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", ttsPy)

	// Ensure stageworker binary is built and available
	stageWorkerBin := buildStageWorkerForSeam1(t, t.TempDir())
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerBin)

	h := setupRealTTSHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup DubScript with 2 segments via Seam 1 API
	// Segment 0: Generous slot (0 - 6000 ms, slot 6000ms) -> Must ACCEPT
	// Segment 1: Extremely tight slot (7000 - 7500 ms, slot 500ms) -> Must OVERRUN and NOT enter selected Segments
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, []domain.TranslationInputSegment{
		{
			Index:      0,
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      6000,
			SourceText: "今天天气很好。",
		},
		{
			Index:      1,
			SpeakerID:  "SPEAKER_00",
			StartMs:    7000,
			EndMs:      7500, // 500ms slot
			SourceText: "我们去公园散步吧。",
		},
	})

	// 2. Assign voices for run
	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("assign voices failed: status %d", respAssign.StatusCode)
	}

	// 3. Execute real TTS synthesis and fit controller via Seam 1 API endpoint
	synthReq := map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign.CASHash,
		"execution_profile":      domain.ExecutionProfileLocal,
		"authorized_credentials": []string{"local_user"},
	}
	respSynth, dubResult := runDubSynthesize(t, h, assetID, synthReq)
	if respSynth.StatusCode != http.StatusCreated || dubResult == nil {
		t.Fatalf("dub-synthesize failed: status %d", respSynth.StatusCode)
	}

	// 4. Verify Selection Invariants:
	// Turn 0: Fits within 6000ms -> Must be in selected Segments
	// Turn 1: Overruns 500ms -> Must NOT be in selected Segments; must be in ReviewSegments
	if len(dubResult.Segments) != 1 {
		t.Fatalf("expected exactly 1 accepted segment in final mix, got %d", len(dubResult.Segments))
	}
	acceptedSeg := dubResult.Segments[0]
	if acceptedSeg.Index != 0 {
		t.Errorf("expected segment 0 to be accepted, got index %d", acceptedSeg.Index)
	}
	if acceptedSeg.MeasuredDurationMs > 6000 {
		t.Errorf("accepted segment exceeds slot duration: measured=%d ms, slot=6000 ms", acceptedSeg.MeasuredDurationMs)
	}

	// Verify real audio media was probed and stored in CAS
	rc, err := h.casStore.Get(acceptedSeg.AudioSHA256)
	if err != nil {
		t.Fatalf("failed to load accepted audio from CAS (sha=%s): %v", acceptedSeg.AudioSHA256, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("failed to read accepted audio bytes: %v", err)
	}
	probedDur, err := media.ProbeWAVBytes(data)
	if err != nil {
		t.Fatalf("ProbeWAVBytes failed on real CAS artifact: %v", err)
	}
	if probedDur != acceptedSeg.MeasuredDurationMs {
		t.Errorf("probed duration (%d ms) != measured duration (%d ms)", probedDur, acceptedSeg.MeasuredDurationMs)
	}

	// Verify overrun segment 1 was gated and prevented from entering final mix
	if len(dubResult.ReviewSegments) != 1 {
		t.Fatalf("expected exactly 1 review segment flagged for overrun, got %d", len(dubResult.ReviewSegments))
	}
	revSeg := dubResult.ReviewSegments[0]
	if revSeg.Index != 1 {
		t.Errorf("expected segment 1 in review segments, got index %d", revSeg.Index)
	}
	if revSeg.MeasuredDurationMs <= 500 {
		t.Errorf("expected measured duration to overrun 500ms slot, got %d ms", revSeg.MeasuredDurationMs)
	}
	if revSeg.FitDecision == domain.FitActionAccept {
		t.Errorf("overlong segment cannot have FitActionAccept, got %s", revSeg.FitDecision)
	}

	t.Logf("Seam 1 Real TTS Fit Gate SUCCESS: accepted_seg_dur=%d ms (slot 6000ms), overrun_seg_dur=%d ms (slot 500ms, action=%s)",
		acceptedSeg.MeasuredDurationMs, revSeg.MeasuredDurationMs, revSeg.FitDecision)
}
func TestSeam1_WorkerTTSProvider_RealStageWorkerSynthesis(t *testing.T) {
	ttsPy := resolveTestTTSPythonSeam1(t)
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", ttsPy)

	stageWorkerBin := buildStageWorkerForSeam1(t, t.TempDir())
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerBin)

	vieneuWorker, err := provider.NewWorkerTTSProvider("vieneu_tts_vi", "vieneu-tts", "1.0.0", []string{"vi"}, 0.95)
	if err != nil {
		t.Fatalf("create WorkerTTSProvider: %v", err)
	}
	synthReq := provider.TTSSynthesisRequest{
		RunID:        "run-real-tts-seam1",
		AssetID:      "asset-real-tts-seam1",
		SegmentIndex: 0,
		SpeakerID:    "SPEAKER_00",
		Text:         "Xin chào, đây là kiểm tra WorkerTTSProvider thực tế qua StageWorker.",
		Language:     "vi",
		Voice: domain.VoiceProfile{
			ID:         "vieneu_vi_truc_ly",
			ProviderID: "vieneu_tts_vi",
			VoiceID:    "Trúc Ly",
			Language:   "vi",
		},
		Speed:          1.0,
		SlotDurationMs: 5000,
		AttemptNumber:  1,
	}
	res, err := vieneuWorker.SynthesizeSpeech(context.Background(), synthReq)
	if err != nil {
		t.Fatalf("SynthesizeSpeech through real WorkerTTSProvider failed: %v", err)
	}

	if res.AudioSHA256 == "" || res.MeasuredDurationMs <= 0 {
		t.Fatalf("invalid TTSSynthesisResult: sha=%s, dur=%d", res.AudioSHA256, res.MeasuredDurationMs)
	}

	if len(res.AudioData) == 0 {
		t.Fatalf("expected audio data returned in TTSSynthesisResult")
	}

	probedDur, err := media.ProbeWAVBytes(res.AudioData)
	if err != nil {
		t.Fatalf("ProbeWAVBytes failed on returned audio: %v", err)
	}
	if probedDur != res.MeasuredDurationMs {
		t.Errorf("probed duration (%d ms) != measured duration (%d ms)", probedDur, res.MeasuredDurationMs)
	}

	t.Logf("WorkerTTSProvider Real Synthesis SUCCESS: probed_dur=%d ms, sha256=%s", probedDur, res.AudioSHA256)
}
