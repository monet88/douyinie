package seam1_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// TestSeam1_OperatorUITempoAuditionBrowserSmoke drives the real Operator UI in a real headless
// browser against a live RuntimeHost for the Issue #156 audition surface: the operator hears
// the retained natural waveform and one eligible tempo alternative through the run-scoped media
// endpoint, and nothing in that surface selects, approves or publishes the alternative.
//
// The seam-1 API test (TestSeam1_Issue156_DeterministicExhaustionRealFFmpegCandidate) proves the
// candidate's bytes and ownership bounds over HTTP; the Node VM harness stubs the DOM. Neither
// covers the client: the two URLs app.js builds, the browser decoding and playing both
// waveforms, and the absence of any selection control. Requires node, ffmpeg (the candidate is a
// real FFmpeg transform) and a Chromium-family browser; skipped when any is unavailable. Pin the
// browser with DOUYINIE_CHROME_BIN.
func TestSeam1_OperatorUITempoAuditionBrowserSmoke(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the Operator UI browser smoke")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the tempo candidate for the browser smoke")
	}

	h := setupHarness(t)
	ctx := context.Background()

	lane := defaultVITTSFake(t, h)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	// The natural recovery sequence is exhausted before tempo is attempted: the single turn has
	// no sibling to regroup with and no rewritable text, and the fixed-rate lane overruns its
	// 1000ms slot by 5% -> the only remaining remedy is one in-window atempo candidate.
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Alo nha bạn"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}, turns)
	lane.DurationMs = 1050

	_, assign := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	synthResp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assign.CASHash,
	})
	if synthResp != nil && synthResp.Body != nil {
		defer synthResp.Body.Close()
	}
	if synthResp == nil || synthResp.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("dub synthesize for the tempo fixture failed: %v", variant)
	}
	if len(variant.Segments) != 0 {
		t.Fatalf("the candidate was auto-selected: %d resolved segments", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected exactly 1 review segment, got %d", len(variant.ReviewSegments))
	}
	tc := variant.ReviewSegments[0].TempoCandidate
	if tc == nil || !tc.Selectable || tc.TransformedAudioSHA256 == "" {
		t.Fatalf("expected an in-window tempo candidate, got %+v", tc)
	}

	// The item the operator auditions is read from the same run-scoped endpoint the UI drives.
	items := fetchRunReviewItems(t, h, runID, false)
	itemID := ""
	for _, item := range items {
		if item.Details["tempo_candidate"] != nil {
			itemID = item.ID
		}
	}
	if itemID == "" {
		t.Fatalf("the run review queue exposes no tempo candidate: %+v", items)
	}

	script, err := filepath.Abs(filepath.Join("..", "..", "internal", "server", "testdata", "ui", "tempo_audition_browser_smoke.mjs"))
	if err != nil {
		t.Fatalf("resolve the browser smoke script: %v", err)
	}

	published := lane.Invocations
	cmd := exec.Command(nodeBin, script, h.server.URL+"/", runID, itemID, tc.NaturalAudioSHA256, tc.TransformedAudioSHA256)
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 3 {
		t.Skipf("no Chromium-family browser available for the browser smoke:\n%s", out)
	}
	if err != nil {
		t.Fatalf("tempo audition browser smoke failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "tempo audition browser smoke checks passed") {
		t.Fatalf("tempo audition browser smoke produced no summary:\n%s", out)
	}
	t.Logf("browser smoke output:\n%s", out)

	// Auditioning the alternative is playback of two stored artifacts, never synthesis.
	if lane.Invocations != published {
		t.Errorf("auditioning the candidate invoked the TTS provider: %d -> %d invocations", published, lane.Invocations)
	}
	// Nothing the browser surfaced may select the alternative or produce unattended output.
	after := fetchRunReviewItems(t, h, runID, false)
	if len(after) != 1 || after[0].Status != domain.ReviewItemStatusPending || after[0].Details["tempo_candidate"] == nil {
		t.Errorf("the audition changed the review item: %+v", after)
	}
	if _, err := h.db.GetLatestRenderArtifactIndex(ctx, assetID, "vi", domain.RenderKindFinal); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the audition produced a final render: err=%v", err)
	}
	if mix, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the audition consumed the candidate into a dub mix: mix=%v err=%v", mix, err)
	}
}
