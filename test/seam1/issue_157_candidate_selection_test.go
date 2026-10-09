package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

// Issue #157: exact reviewed-candidate selection through render and portable replay.
//
// These cases drive the RuntimeHost API over Seam 1 with real SQLite/CAS/router, the deterministic
// fake gateway/TTS lanes and real FFmpeg where a transform or a render is asserted.

// issue157ReviewItems reads the run-scoped pending review queue over the API.
func issue157ReviewItems(t *testing.T, h *testHarness, runID string) []domain.ReviewItem {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/review-items", h.server.URL, runID))
	if err != nil {
		t.Fatalf("GET run review-items: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET run review-items status=%d body=%s", resp.StatusCode, string(body))
	}
	var body struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode review items: %v", err)
	}
	return body.ReviewItems
}

// issue157PostAccept posts one exact reviewed-candidate selection for the run and returns the raw
// outcome without failing the test, so a case can drive the endpoint from more than one goroutine
// or from an unexpected host. The refusal is returned as its decoded message.
func issue157PostAccept(h *testHarness, runID string, payload map[string]any) (int, *service.AcceptReviewedCandidateResult, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, "", fmt.Errorf("marshal accept payload: %w", err)
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/review/accept-candidate", h.server.URL, runID), "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", fmt.Errorf("POST accept-candidate: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var res struct {
			Result service.AcceptReviewedCandidateResult `json:"result"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return resp.StatusCode, nil, "", fmt.Errorf("decode accept result: %w (body %s)", err, string(raw))
		}
		return resp.StatusCode, &res.Result, "", nil
	}
	var errRes struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &errRes); err == nil && errRes.Error != "" {
		return resp.StatusCode, nil, errRes.Error, nil
	}
	return resp.StatusCode, nil, string(raw), nil
}

// issue157Accept posts one selection for the run. A refusal is returned as its status plus its
// decoded message, so a case can assert the refusal class it got.
func issue157Accept(t *testing.T, h *testHarness, runID string, payload map[string]any) (int, *service.AcceptReviewedCandidateResult, string) {
	t.Helper()
	status, result, refusal, err := issue157PostAccept(h, runID, payload)
	if err != nil {
		t.Fatalf("POST accept-candidate: %v", err)
	}
	return status, result, refusal
}

// issue157RunVariant reads the dubbing artifact the run currently holds.
func issue157RunVariant(t *testing.T, h *testHarness, runID string) *domain.DubSegmentsVariant {
	t.Helper()
	ctx := context.Background()
	idx, err := h.db.GetDubSegmentsVariantIndexByRun(ctx, runID)
	if err != nil {
		t.Fatalf("read run %s dub variant index: %v", runID, err)
	}
	rc, err := h.casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("read run %s dub variant from CAS (%s): %v", runID, idx.CASHash, err)
	}
	defer rc.Close()
	var variant domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		t.Fatalf("decode run %s dub variant: %v", runID, err)
	}
	variant.CASHash = idx.CASHash
	return &variant
}

// issue157StageArtifact returns the artifact of a run's newest stage execution, or "".
func issue157StageArtifact(t *testing.T, h *testHarness, runID, stage string) string {
	t.Helper()
	casHash, err := h.db.GetStageArtifactHash(context.Background(), runID, stage)
	if err != nil {
		t.Fatalf("read stage %s artifact for run %s: %v", stage, runID, err)
	}
	return casHash
}

// TestSeam1_Issue157_JoinedPipelineSelectionThroughRenderAndPortableReplay is the joined
// acceptance of the final slice: the production run pipeline (glossary/contract-checked
// translation, measured playback windows, bounded recovery) parks on an unselectable review-only
// tempo candidate, the operator accepts that exact candidate, and the accepted selection survives
// a restart and a bundle replay onto a host with a different CAS root.
func TestSeam1_Issue157_JoinedPipelineSelectionThroughRenderAndPortableReplay(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the tempo candidate and render the rebuilt preview")
	}

	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)

	// One 300ms dialogue slot; a 330ms fixed-rate candidate needs a 1.1x transform, which is the
	// review-only atempo band (#156) and therefore parks the run with exactly one candidate.
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 330

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "issue157_joined.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	parked := pollRunStatus(t, h.server.URL, runID, domain.RunStatusPaused, 30*time.Second)
	if parked.Status != domain.RunStatusPaused {
		t.Fatalf("expected the run to park on an unresolved tempo candidate, got %q; stages: %+v; items: %+v",
			parked.Status, getRunStages(t, h.server.URL, runID), issue157ReviewItems(t, h, runID))
	}
	if job := getJobViaAPI(t, h, jobID); job.Status != "review_required" {
		t.Fatalf("expected job review_required alongside the parked run, got %q", job.Status)
	}
	if mix := issue157StageArtifact(t, h, runID, "audio_mix"); mix != "" {
		t.Fatalf("a parked run must not have mixed, got audio_mix artifact %s", mix)
	}

	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 || items[0].Type != domain.ReviewItemTypeTTSOverrun {
		t.Fatalf("expected exactly one pending tts_overrun review item, got %+v", items)
	}
	item := items[0]
	base := issue157RunVariant(t, h, runID)
	if base.OverallStatus != "REVIEW_REQUIRED" || len(base.ReviewSegments) != 1 {
		t.Fatalf("expected one unresolved review unit, got status=%s review=%d", base.OverallStatus, len(base.ReviewSegments))
	}
	tempo, _ := item.Details["tempo_candidate"].(map[string]any)
	if tempo == nil {
		t.Fatalf("expected the parked item to carry a tempo candidate, got %+v", item.Details)
	}
	factor, _ := tempo["factor"].(float64)
	if factor <= 1 || factor > 1.25 {
		t.Fatalf("expected the deterministic fixture to land in the review-only transform band, got factor %v (details %+v)", factor, tempo)
	}

	// A transformed waveform is a quality waiver the operator must make explicitly: the run never
	// waives it, and the selection is refused without the flag.
	status, _, refusal := issue157Accept(t, h, runID, map[string]any{
		"review_item_id": item.ID,
		"candidate":      "transformed",
		"reason":         "operator accepted the tempo alternative",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a transformed selection without manual_override, got %d (%s)", status, refusal)
	}
	if !strings.Contains(refusal, "manual_override") {
		t.Fatalf("expected the refusal to name the required waiver, got %q", refusal)
	}

	// An explicit waiver is still governed: the original waveform's own overrun is a hard gate and
	// cannot be selected as the alternative.
	natStatus, _, natRefusal := issue157Accept(t, h, runID, map[string]any{
		"review_item_id": item.ID,
		"candidate":      "natural",
		"reason":         "operator tried the retained natural waveform",
	})
	if natStatus != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for the overrunning natural waveform, got %d (%s)", natStatus, natRefusal)
	}
	if !strings.Contains(natRefusal, "accepted playback window") {
		t.Fatalf("expected the refusal to name the playback window gate, got %q", natRefusal)
	}

	// The acceptance appends evidence to the existing artifact family: the QA results and provider
	// attempts the reviewed unit was decided from are not rewritten by promoting it.
	qualityBefore := issue157QualityResultCount(t, h, runID)
	attemptsBefore := issue157ProviderAttempts(t, h, runID)

	// The accepted selection.
	accStatus, accepted, acceptanceRefusal := issue157Accept(t, h, runID, map[string]any{
		"review_item_id":  item.ID,
		"candidate":       "transformed",
		"manual_override": true,
		"reason":          "operator auditioned the tempo alternative and accepted it",
		"operator":        "seam1-reviewer",
	})
	if accStatus != http.StatusOK || accepted == nil {
		t.Fatalf("expected 200 for the exact reviewed-candidate selection, got %d (%s)", accStatus, acceptanceRefusal)
	}
	if !accepted.CoverageComplete || !accepted.Transformed || !accepted.QualityWaiver {
		t.Fatalf("expected a complete transformed acceptance with an explicit waiver, got %+v", accepted)
	}
	if accepted.DubMixCAS == "" || accepted.RenderPlanCAS == "" || accepted.PreviewRenderCAS == "" {
		t.Fatalf("expected the acceptance to rebuild mix, render plan and preview, got %+v", accepted)
	}

	// The successor artifact carries the acceptance evidence against the exact candidate bytes,
	// while the original waveform and quality evidence stay untouched.
	successor := issue157RunVariant(t, h, runID)
	if successor.CASHash != accepted.DubSegmentsVariantCAS {
		t.Fatalf("expected the run's dubbing artifact to be the successor %s, got %s", accepted.DubSegmentsVariantCAS, successor.CASHash)
	}
	if successor.OverallStatus != "PASS" || len(successor.ReviewSegments) != 0 || len(successor.Segments) != 1 {
		t.Fatalf("expected a passing successor with one selected segment, got status=%s review=%d selected=%d",
			successor.OverallStatus, len(successor.ReviewSegments), len(successor.Segments))
	}
	if len(successor.AcceptedCandidates) != 1 {
		t.Fatalf("expected one acceptance evidence record, got %+v", successor.AcceptedCandidates)
	}
	evidence := successor.AcceptedCandidates[0]
	if evidence.ReviewItemID != item.ID || evidence.SelectedAudioSHA256 != accepted.SelectedAudioSHA256 ||
		!evidence.Transformed || !evidence.QualityWaiver || evidence.ReviewOverrideID != accepted.ReviewOverrideID {
		t.Fatalf("acceptance evidence does not match the selection: %+v", evidence)
	}
	if evidence.NaturalAudioSHA256 == evidence.SelectedAudioSHA256 {
		t.Fatalf("a transformed acceptance must name a distinct natural parent, got %s", evidence.NaturalAudioSHA256)
	}
	if successor.Segments[0].AudioSHA256 != accepted.SelectedAudioSHA256 {
		t.Fatalf("the selected segment must carry the accepted waveform, got %s", successor.Segments[0].AudioSHA256)
	}
	if after := issue157QualityResultCount(t, h, runID); after != qualityBefore {
		t.Fatalf("the acceptance must not rewrite QA history: %d -> %d", qualityBefore, after)
	}
	if after := issue157ProviderAttempts(t, h, runID); after != attemptsBefore {
		t.Fatalf("the acceptance must not rewrite provider attempt history: %d -> %d", attemptsBefore, after)
	}
	if successor.Segments[0].FitDecision != domain.FitActionAccept || successor.Segments[0].RequiresReview {
		t.Fatalf("the promoted segment must be an accepted non-review segment, got %+v", successor.Segments[0])
	}
	// The original waveform stays reachable and playable: selection never rewrites it.
	natResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", h.server.URL, runID, evidence.NaturalAudioSHA256))
	if err != nil || natResp.StatusCode != http.StatusOK {
		t.Fatalf("the retained natural waveform must stay playable: status=%v err=%v", natResp.StatusCode, err)
	}
	natResp.Body.Close()
	selResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", h.server.URL, runID, evidence.SelectedAudioSHA256))
	if err != nil || selResp.StatusCode != http.StatusOK {
		t.Fatalf("the accepted waveform must be playable: status=%v err=%v", selResp.StatusCode, err)
	}
	selResp.Body.Close()

	// A repeated identical selection is idempotent: same successor, no second audit row.
	overridesBefore := issue157OverrideCount(t, h, runID)
	replayStatus, replayed, replayRefusal := issue157Accept(t, h, runID, map[string]any{
		"review_item_id":  item.ID,
		"candidate":       "transformed",
		"manual_override": true,
		"reason":          "operator auditioned the tempo alternative and accepted it",
		"operator":        "seam1-reviewer",
	})
	if replayStatus != http.StatusOK || replayed == nil || !replayed.Idempotent {
		t.Fatalf("expected an idempotent replay, got status=%d result=%+v refusal=%s", replayStatus, replayed, replayRefusal)
	}
	if replayed.DubSegmentsVariantCAS != successor.CASHash {
		t.Fatalf("an idempotent replay must not mint another artifact: %s != %s", replayed.DubSegmentsVariantCAS, successor.CASHash)
	}
	if after := issue157OverrideCount(t, h, runID); after != overridesBefore {
		t.Fatalf("an idempotent replay must not append an audit row: %d -> %d", overridesBefore, after)
	}

	// The mix the run serves pins the successor, and preview/final are the accepted semantics.
	mixIdx, err := h.db.GetDubMixArtifactIndexByRun(context.Background(), runID)
	if err != nil || mixIdx == nil {
		t.Fatalf("expected a mix for the accepted run: %v", err)
	}
	if mixIdx.CASHash != accepted.DubMixCAS {
		t.Fatalf("expected the run's mix to be the rebuilt one %s, got %s", accepted.DubMixCAS, mixIdx.CASHash)
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected a rebuilt preview render: status=%v err=%v", statusOf(resp), err)
	} else {
		resp.Body.Close()
	}

	// Restart: a fresh RuntimeHost over the same persisted state still serves the accepted media
	// and no longer queues the resolved item.
	h2 := issue157RestartHost(t, h)
	if items := issue157ReviewItems(t, h2, runID); len(items) != 0 {
		t.Fatalf("expected the accepted item to be resolved after restart, got %+v", items)
	}
	playResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", h2.server.URL, runID, evidence.SelectedAudioSHA256))
	if err != nil || playResp.StatusCode != http.StatusOK {
		t.Fatalf("the accepted waveform must stay playable after restart: status=%v err=%v", statusOf(playResp), err)
	}
	playResp.Body.Close()

	// Portable replay: a second host with a different CAS root imports the bundle and serves the
	// accepted candidate, its natural parent and the approval lineage.
	exportResp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export bundle: %v", err)
	}
	if exportResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(exportResp.Body)
		exportResp.Body.Close()
		t.Fatalf("export bundle status=%d body=%s", exportResp.StatusCode, string(body))
	}
	zipBytes, err := io.ReadAll(exportResp.Body)
	exportResp.Body.Close()
	if err != nil || len(zipBytes) == 0 {
		t.Fatalf("read exported bundle: %v (len %d)", err, len(zipBytes))
	}

	importHost := setupHarness(t)
	if importHost.dir == h.dir {
		t.Fatal("expected the importing host to use a different CAS root")
	}
	impResp, err := http.Post(importHost.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(zipBytes))
	if err != nil {
		t.Fatalf("import bundle: %v", err)
	}
	impBody, _ := io.ReadAll(impResp.Body)
	impResp.Body.Close()
	if impResp.StatusCode != http.StatusOK && impResp.StatusCode != http.StatusCreated {
		t.Fatalf("import bundle status=%d body=%s", impResp.StatusCode, string(impBody))
	}

	imported := issue157RunVariant(t, importHost, runID)
	if len(imported.AcceptedCandidates) != 1 || imported.AcceptedCandidates[0].SelectedAudioSHA256 != evidence.SelectedAudioSHA256 {
		t.Fatalf("imported artifact lost the acceptance lineage: %+v", imported.AcceptedCandidates)
	}
	importedOverrides, err := importHost.db.GetReviewOverridesByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read imported review overrides: %v", err)
	}
	foundOverride := false
	for _, ro := range importedOverrides {
		if ro.ID == accepted.ReviewOverrideID && ro.Action == string(domain.ReviewOverrideActionReviewedCandidate) {
			foundOverride = true
		}
	}
	if !foundOverride {
		t.Fatalf("imported host lost the acceptance audit row %s: %+v", accepted.ReviewOverrideID, importedOverrides)
	}
	for _, hash := range []string{evidence.NaturalAudioSHA256, evidence.SelectedAudioSHA256} {
		importPlay, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", importHost.server.URL, runID, hash))
		if err != nil || importPlay.StatusCode != http.StatusOK {
			t.Fatalf("imported host must serve waveform %s: status=%v err=%v", hash, statusOf(importPlay), err)
		}
		importPlay.Body.Close()
	}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// issue157OverrideCount counts the run's recorded review override rows.
func issue157OverrideCount(t *testing.T, h *testHarness, runID string) int {
	t.Helper()
	rows, err := h.db.GetReviewOverridesByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read review overrides for run %s: %v", runID, err)
	}
	return len(rows)
}

// issue157RestartHost rebuilds a RuntimeHost over the same database and CAS root, simulating a
// process restart without losing persisted state.
func issue157RestartHost(t *testing.T, h *testHarness) *testHarness {
	t.Helper()
	srv, _, _ := newRuntimeHostWithOptions(t, h.db, h.casStore, h.queueSvc, h.scheduler, harnessOptions{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})
	return &testHarness{
		server:     ts,
		srv:        srv,
		db:         h.db,
		casStore:   h.casStore,
		registry:   h.registry,
		router:     h.router,
		queueSvc:   h.queueSvc,
		scheduler:  h.scheduler,
		dubbingSvc: h.dubbingSvc,
		dir:        h.dir,
	}
}
