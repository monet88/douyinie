package seam1_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
)

// Issue #156: Generate playable review-only FFmpeg tempo candidates once natural remedies are exhausted.

func TestSeam1_Issue156_DeterministicExhaustionRealFFmpegCandidate(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host for Issue #156 real DSP transform verification")
	}

	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	// Canonical timeline: single turn [0, 1000]ms. Cannot regroup (no other turn).
	// Text <= 3 words ("Alo nha bạn"), so CanShortenText=false (cannot rewrite).
	// ZeroTTS preset lane: fixed-rate voice, reject speed resynthesis.
	// Escalation: CosyVoice fallback lane is either unavailable or fixed-rate remains unresolved.
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Alo nha bạn"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	// Fixed-rate ZeroTTS fake: synthesizes 1200ms audio -> factor = 1200 / 1000 = 1.20 (within 1 < factor <= 1.25)
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 1200

	_, assign := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})

	// Dub synthesize
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assign.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: status %d body=%s", resp.StatusCode, string(body))
	}

	// Acceptance criteria:
	// 1. Candidate remains UNSELECTED and REVIEW_REQUIRED
	if variant.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("expected overall status REVIEW_REQUIRED, got %s", variant.OverallStatus)
	}
	if len(variant.Segments) != 0 {
		t.Fatalf("no candidate may be auto-selected, got %d selected segments", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected exactly 1 review segment, got %d", len(variant.ReviewSegments))
	}

	rev := variant.ReviewSegments[0]
	tc := rev.TempoCandidate
	if tc == nil {
		t.Fatal("expected tempo candidate to be attached to review segment")
	}

	// 2. Factor and measured durations match actual audio
	if tc.Factor != 1.20 {
		t.Fatalf("expected factor 1.20, got %v", tc.Factor)
	}
	if tc.NaturalDurationMs != 1200 {
		t.Fatalf("expected NaturalDurationMs 1200, got %d", tc.NaturalDurationMs)
	}
	if tc.PlaybackDurationMs != 1000 {
		t.Fatalf("expected PlaybackDurationMs 1000, got %d", tc.PlaybackDurationMs)
	}
	if tc.NaturalAudioSHA256 != rev.AudioSHA256 {
		t.Fatalf("expected natural sha %s == rev audio sha %s", tc.NaturalAudioSHA256, rev.AudioSHA256)
	}
	if tc.TransformedAudioSHA256 == "" || tc.TransformedAudioSHA256 == tc.NaturalAudioSHA256 {
		t.Fatalf("expected distinct transformed sha, got %s", tc.TransformedAudioSHA256)
	}
	if tc.ToolID != domain.TempoToolID {
		t.Fatalf("expected tool %s, got %s", domain.TempoToolID, tc.ToolID)
	}
	// The recorded lineage must be exactly the filter the transform ran: AtempoFilterString's own
	// serialization of the recorded factor, not a hard-coded spelling that can drift from it.
	if want := media.AtempoFilterString(tc.Factor); tc.Filter != want {
		t.Fatalf("expected filter %q, got %q", want, tc.Filter)
	}
	// Transformed duration ~1000ms <= 1000ms -> Selectable = true, Reason = TEMPO_CANDIDATE_WITHIN_WINDOW
	if !tc.Selectable || tc.Reason != domain.TempoReasonFits {
		t.Fatalf("expected selectable=true and reason %s, got selectable=%v reason=%s", domain.TempoReasonFits, tc.Selectable, tc.Reason)
	}

	// 3. Prove NO output crop / atrim by inspecting transformed WAV header and sample extent
	rcTrans, err := h.casStore.Get(tc.TransformedAudioSHA256)
	if err != nil {
		t.Fatalf("get transformed audio from CAS: %v", err)
	}
	transBytes, _ := io.ReadAll(rcTrans)
	_ = rcTrans.Close()
	header, err := media.ParseWAVHeader(transBytes)
	if err != nil {
		t.Fatalf("parse transformed wav header: %v", err)
	}
	// A complete transform of the 1200ms retained waveform at factor 1.20 is ~1000ms. The bound
	// is two-sided on purpose: a cropped (atrim) or truncated output lands far below this window
	// and fails here. The filter-token proof that no atrim/crop chain is ever passed lives in
	// media.TestAtempoFilterArgs_OnlyOneFilterWithFiniteNumericArg.
	if header.DurationMs < 975 || header.DurationMs > 1005 {
		t.Fatalf("expected a complete ~1000ms transform, got %dms", header.DurationMs)
	}
	// Verify sample count matches data chunk length (no trailing truncated garbage)
	expectedDataBytes := uint32(int64(header.NumChannels) * int64(header.BitsPerSample/8) * int64(header.SampleRate) * header.DurationMs / 1000)
	if header.DataSize < expectedDataBytes-100 || header.DataSize > expectedDataBytes+100 {
		t.Fatalf("wav data size mismatch with duration: size %d, expected near %d", header.DataSize, expectedDataBytes)
	}

	// 4. Play natural and transformed audio via run-scoped inspector API with zero new provider calls
	publishedInvocations := lane.Invocations

	// Natural audio playback
	reqNat, _ := http.NewRequest("GET", h.server.URL+"/api/v1/runs/"+runID+"/dub-media/"+tc.NaturalAudioSHA256, nil)
	respNat, err := http.DefaultClient.Do(reqNat)
	if err != nil || respNat.StatusCode != http.StatusOK {
		t.Fatalf("play natural audio failed: status %v err %v", respNat.StatusCode, err)
	}
	natBytesStreamed, _ := io.ReadAll(respNat.Body)
	_ = respNat.Body.Close()
	if len(natBytesStreamed) == 0 {
		t.Fatal("streamed natural audio is empty")
	}

	// Transformed audio playback
	reqTrans, _ := http.NewRequest("GET", h.server.URL+"/api/v1/runs/"+runID+"/dub-media/"+tc.TransformedAudioSHA256, nil)
	respTrans, err := http.DefaultClient.Do(reqTrans)
	if err != nil || respTrans.StatusCode != http.StatusOK {
		t.Fatalf("play transformed audio failed: status %v err %v", respTrans.StatusCode, err)
	}
	transBytesStreamed, _ := io.ReadAll(respTrans.Body)
	_ = respTrans.Body.Close()
	if !bytes.Equal(transBytesStreamed, transBytes) {
		t.Fatal("streamed transformed audio does not match CAS bytes")
	}

	// ZERO new provider/TTS calls occurred during playback!
	if lane.Invocations != publishedInvocations {
		t.Fatalf("playback must not invoke TTS provider: invocations grew from %d to %d", publishedInvocations, lane.Invocations)
	}

	// 5. Path / hash ownership bounds: foreign and stale hash rejection
	foreignHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	reqForeign, _ := http.NewRequest("GET", h.server.URL+"/api/v1/runs/"+runID+"/dub-media/"+foreignHash, nil)
	respForeign, err := http.DefaultClient.Do(reqForeign)
	if err != nil || respForeign.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for foreign hash, got %d", respForeign.StatusCode)
	}

	// Cross-run hash check: create a second job/run B and verify run A's hashes are rejected on run B
	jobB, runB := createJobAndRun(t, h)
	_ = jobB
	reqCross, _ := http.NewRequest("GET", h.server.URL+"/api/v1/runs/"+runB+"/dub-media/"+tc.TransformedAudioSHA256, nil)
	respCross, err := http.DefaultClient.Do(reqCross)
	if err != nil || (respCross.StatusCode != http.StatusForbidden && respCross.StatusCode != http.StatusNotFound) {
		t.Fatalf("expected 403 or 404 for cross-run hash, got %d", respCross.StatusCode)
	}

	// 6. Bundle export and closure verification
	respExport, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export bundle request failed: %v", err)
	}
	if respExport.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(respExport.Body)
		_ = respExport.Body.Close()
		// Debug: check which table has this hash
		t.Logf("tc.NaturalAudioSHA256: %s", tc.NaturalAudioSHA256)
		t.Logf("tc.TransformedAudioSHA256: %s", tc.TransformedAudioSHA256)
		t.Fatalf("export bundle via API failed: status %d body=%s", respExport.StatusCode, string(b))
	}
	zipBytes, err := io.ReadAll(respExport.Body)
	_ = respExport.Body.Close()
	if err != nil || len(zipBytes) == 0 {
		t.Fatalf("read exported zip: %v (len %d)", err, len(zipBytes))
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open exported zip: %v", err)
	}
	var manifest domain.JobBundleManifest
	foundManifest := false
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			rc, _ := f.Open()
			_ = json.NewDecoder(rc).Decode(&manifest)
			_ = rc.Close()
			foundManifest = true
			break
		}
	}
	if !foundManifest {
		t.Fatal("manifest.json not found in exported bundle")
	}
	hasNat := false
	hasTrans := false
	for _, art := range manifest.Artifacts {
		if strings.EqualFold(art.SHA256, tc.NaturalAudioSHA256) {
			hasNat = true
		}
		if strings.EqualFold(art.SHA256, tc.TransformedAudioSHA256) {
			hasTrans = true
		}
	}
	if !hasNat || !hasTrans {
		t.Fatalf("bundle closure missing hashes: hasNat=%v hasTrans=%v", hasNat, hasTrans)
	}
}

func TestSeam1_Issue156_TransformFactorBoundaries(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	// Subtest A: factor just above 1 (e.g. 1.05 -> slot 1000, measured 1050)
	t.Run("factor just above 1 generates atempo candidate", func(t *testing.T) {
		hA := setupHarness(t)
		laneA := defaultVITTSFake(t, hA)
		jobID, runID := createJobAndRun(t, hA)
		assetID := getJobViaAPI(t, hA, jobID).SourceAssetID
		turns := []issue155Turn{
			{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "一", spoken: "Một"},
		}
		scriptCAS, _ := issue155PinLineage(t, hA, jobID, assetID, runID,
			[]domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}, turns)
		laneA.DurationMs = 1050
		_, assign := runAssignVoices(t, hA, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
		resp, variant := runDubSynthesize(t, hA, assetID, map[string]any{
			"run_id":                 runID,
			"target_language":        "vi",
			"dub_script_variant_cas": scriptCAS,
			"voice_assignment_cas":   assign.CASHash,
		})
		if resp.StatusCode != http.StatusCreated || variant == nil || len(variant.ReviewSegments) != 1 {
			t.Fatalf("dub-synthesize failed: %v", variant)
		}
		tc := variant.ReviewSegments[0].TempoCandidate
		if tc == nil || tc.TransformedAudioSHA256 == "" || tc.Factor != 1.05 {
			t.Fatalf("expected valid tempo candidate at factor 1.05, got %+v", tc)
		}
	})

	// Subtest B: factor exactly 1.25 (slot 1000, measured 1250)
	t.Run("factor exactly 1.25 generates atempo candidate", func(t *testing.T) {
		hB := setupHarness(t)
		laneB := defaultVITTSFake(t, hB)
		jobID, runID := createJobAndRun(t, hB)
		assetID := getJobViaAPI(t, hB, jobID).SourceAssetID
		turns := []issue155Turn{
			{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "一", spoken: "Một"},
		}
		scriptCAS, _ := issue155PinLineage(t, hB, jobID, assetID, runID,
			[]domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}, turns)
		laneB.DurationMs = 1250
		_, assign := runAssignVoices(t, hB, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
		resp, variant := runDubSynthesize(t, hB, assetID, map[string]any{
			"run_id":                 runID,
			"target_language":        "vi",
			"dub_script_variant_cas": scriptCAS,
			"voice_assignment_cas":   assign.CASHash,
		})
		if resp.StatusCode != http.StatusCreated || variant == nil || len(variant.ReviewSegments) != 1 {
			t.Fatalf("dub-synthesize failed: %v", variant)
		}
		tc := variant.ReviewSegments[0].TempoCandidate
		if tc == nil || tc.TransformedAudioSHA256 == "" || tc.Factor != 1.25 {
			t.Fatalf("expected valid tempo candidate at factor 1.25, got %+v", tc)
		}
	})

	// Subtest C: factor above 1.25 (e.g. 1.26 -> slot 1000, measured 1260)
	t.Run("factor above 1.25 generates no DSP candidate", func(t *testing.T) {
		hC := setupHarness(t)
		laneC := defaultVITTSFake(t, hC)
		jobID, runID := createJobAndRun(t, hC)
		assetID := getJobViaAPI(t, hC, jobID).SourceAssetID
		turns := []issue155Turn{
			{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "一", spoken: "Một"},
		}
		scriptCAS, _ := issue155PinLineage(t, hC, jobID, assetID, runID,
			[]domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}, turns)
		laneC.DurationMs = 1260
		_, assign := runAssignVoices(t, hC, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
		resp, variant := runDubSynthesize(t, hC, assetID, map[string]any{
			"run_id":                 runID,
			"target_language":        "vi",
			"dub_script_variant_cas": scriptCAS,
			"voice_assignment_cas":   assign.CASHash,
		})
		if resp.StatusCode != http.StatusCreated || variant == nil || len(variant.ReviewSegments) != 1 {
			t.Fatalf("dub-synthesize failed: %v", variant)
		}
		tc := variant.ReviewSegments[0].TempoCandidate
		if tc == nil || tc.TransformedAudioSHA256 != "" || tc.Reason != domain.TempoReasonFactorOutOfRange {
			t.Fatalf("expected no DSP candidate for factor 1.26, got %+v", tc)
		}
	})
}
