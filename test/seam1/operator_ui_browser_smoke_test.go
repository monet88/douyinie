package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// TestSeam1_OperatorUIRegionCorrectionBrowserSmoke drives the real Operator UI in a real
// headless browser against a live RuntimeHost, and walks the acceptance flow an operator
// takes for a visual exception:
//
//	select run -> select region (seek + overlay) -> drag -> resize -> reclassify
//	-> apply -> targeted rerun -> the player refetches the preview of the corrected plan
//
// The Node VM harness (internal/server/testdata/ui/app_behavior.mjs) cannot cover this: it
// stubs the DOM, so layout metrics, trusted pointer gestures, media loading, the run-scoped
// artifact reloads after an apply, and the browser console are never exercised.
//
// Requires node, ffmpeg (playable preview media) and a Chromium-family browser; skipped when
// any of them is unavailable. Pin the browser with DOUYINIE_CHROME_BIN.
func TestSeam1_OperatorUIRegionCorrectionBrowserSmoke(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the Operator UI browser smoke")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build playable preview media for the browser smoke")
	}

	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// The correction renders the preview the browser plays, so composition is pinned to the
	// run's own media: the decoded player below is a real MP4, not a placeholder.
	installPreviewComposer(t, h, filepath.Join(h.dir, "queue_source.mp4"))

	// The region the smoke manipulates is a speech_subtitle region: its geometry drives a real
	// subtitle cue, so a correction necessarily changes the frozen render plan (and therefore
	// the preview) rather than only the operator's local form state.
	textPlan := domain.TextRegionPlan{
		ID:             "text-plan-browser-smoke",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-browser-smoke",
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "region-smoke-drag",
				Role:        domain.TextRoleSpeechSubtitle,
				Text:        "点击下载应用",
				FirstSeenMs: 700,
				LastSeenMs:  1400,
				Keyframes: []domain.RegionKeyframe{
					{
						FrameIndex:  21,
						TimestampMs: 700,
						Box:         domain.BoundingBox{X: 200, Y: 300, Width: 300, Height: 80},
						Observed:    true,
					},
				},
			},
			{
				ID:                 "region-smoke-role",
				Role:               domain.TextRoleUncertain,
				Text:               "限时优惠",
				FirstSeenMs:        0,
				LastSeenMs:         600,
				ConfidenceEvidence: domain.ConfidenceEvidence{MeanConfidence: 0.55},
				ReviewReason:       "uncertain_visual_role",
				Keyframes: []domain.RegionKeyframe{
					{
						FrameIndex:  0,
						TimestampMs: 0,
						Box:         domain.BoundingBox{X: 200, Y: 900, Width: 300, Height: 80},
						Observed:    true,
					},
				},
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(textPlan)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	if err := h.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             textPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: textPlan.ProvenanceHash,
		CreatedAt:      textPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save text region plan index: %v", err)
	}

	roleBody, _ := json.Marshal(map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	})
	roleResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil || roleResp.StatusCode != http.StatusCreated {
		t.Fatalf("setup audio role plan failed: status=%v err=%v", roleResp.StatusCode, err)
	}
	_ = roleResp.Body.Close()

	sepResp, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	if sepResp.StatusCode != http.StatusCreated || stems == nil {
		t.Fatalf("setup separate stems failed: status=%d", sepResp.StatusCode)
	}
	_ = sepResp.Body.Close()

	mixResp, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
	})
	if mixResp.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("setup audio mix failed: status=%d", mixResp.StatusCode)
	}
	_ = mixResp.Body.Close()

	// The preview the operator starts from: a plan frozen before the correction, so the smoke
	// proves the apply lands a *different* preview rather than re-reading the same artifact.
	freezeResp, initialPlan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"dub_mix_cas":     mix.CASHash,
		"subtitle_cues": []domain.SubtitleCue{
			{StartMs: 0, EndMs: 500, Text: "pre-correction preview cue", X: 100, Y: 1600, FontSizePx: 40},
		},
	})
	if freezeResp.StatusCode != http.StatusCreated || initialPlan == nil {
		t.Fatalf("freeze initial render plan failed: status=%d", freezeResp.StatusCode)
	}
	_ = freezeResp.Body.Close()

	previewResp, initialPreview := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": initialPlan.ProvenanceHash,
	})
	if previewResp.StatusCode != http.StatusCreated || initialPreview == nil {
		t.Fatalf("render initial preview failed: status=%d", previewResp.StatusCode)
	}
	_ = previewResp.Body.Close()

	script, err := filepath.Abs(filepath.Join("..", "..", "internal", "server", "testdata", "ui", "region_correction_browser_smoke.mjs"))
	if err != nil {
		t.Fatalf("resolve browser smoke script: %v", err)
	}
	// The Operator UI is the single-page shell at the root; the asset comes from the run the
	// operator selects, never from the URL.
	uiURL := h.server.URL + "/"

	cmd := exec.Command(nodeBin, script, uiURL)
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 3 {
		t.Skipf("no Chromium-family browser available for the browser smoke:\n%s", out)
	}
	if err != nil {
		t.Fatalf("Operator UI browser smoke failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "region correction browser smoke checks passed") {
		t.Fatalf("Operator UI browser smoke produced no summary:\n%s", out)
	}
	t.Logf("browser smoke output:\n%s", out)

	// The correction the browser performed must have left the run's preview consuming a plan
	// that differs from the pre-correction one, with the upstream audio lineage untouched.
	finalPreview, err := h.db.GetLatestRenderArtifactIndex(ctx, assetID, "vi", domain.RenderKindPreview)
	if err != nil || finalPreview == nil {
		t.Fatalf("get preview artifact after the browser apply: idx=%v err=%v", finalPreview, err)
	}
	if finalPreview.CASHash == initialPreview.CASHash {
		t.Errorf("the browser apply did not produce a new preview artifact (still %s)", finalPreview.CASHash)
	}
	if finalPreview.ProvenanceHash == initialPreview.ProvenanceHash {
		t.Errorf("the browser apply left the preview bound to the pre-correction plan")
	}
	mixAfter, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixAfter == nil {
		t.Fatalf("get dub mix after the browser apply: idx=%v err=%v", mixAfter, err)
	}
	if mixAfter.CASHash != mix.CASHash {
		t.Errorf("the browser apply reran the audio mix: cas=%s, want %s", mixAfter.CASHash, mix.CASHash)
	}
}
