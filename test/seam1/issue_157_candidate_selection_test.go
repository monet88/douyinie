package seam1_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
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

// issue157CarriesHostPath reports whether raw embeds the exporting host's directory. The check has to
// cover the forms the path takes on disk and inside JSON: a Windows root is written with escaped
// separators there, so searching the literal string alone would pass on a bundle that leaks it.
func issue157CarriesHostPath(raw []byte, dir string) bool {
	for _, form := range []string{dir, filepath.ToSlash(dir), strings.ReplaceAll(dir, `\`, `\\`)} {
		if form != "" && bytes.Contains(raw, []byte(form)) {
			return true
		}
	}
	return false
}

// issue157AnchoredGateway is the HTTP gateway a joined run translates through: a real gateway endpoint
// the production provider parses, anchor-validates and repairs against, not a provider that answers
// without any of those steps. It records whether the run's frozen glossary reached the wire payload and
// answers the first batch with a duplicated index, so the bounded whole-batch repair has to run before a
// translation can be accepted.
type issue157AnchoredGateway struct {
	*httptest.Server
	calls        int32
	glossarySeen int32
}

type issue157GatewayPayload struct {
	Segments []struct {
		Index      int    `json:"index"`
		SourceText string `json:"source_text"`
	} `json:"segments"`
	EffectiveGlossary []domain.GlossaryEntry `json:"effective_glossary"`
}

func newIssue157AnchoredGateway(t *testing.T) *issue157AnchoredGateway {
	t.Helper()
	g := &issue157AnchoredGateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad gateway request", http.StatusBadRequest)
			return
		}
		var payload issue157GatewayPayload
		for _, m := range req.Messages {
			if m.Role == "user" && json.Unmarshal([]byte(m.Content), &payload) == nil {
				break
			}
		}
		if len(payload.EffectiveGlossary) > 0 {
			atomic.StoreInt32(&g.glossarySeen, 1)
		}
		segments := make([]map[string]any, 0, len(payload.Segments))
		for _, seg := range payload.Segments {
			segments = append(segments, map[string]any{
				"index":             seg.Index,
				"source_text":       seg.SourceText,
				"target_text":       fmt.Sprintf("Bản dịch thoại cho đoạn %d", seg.Index+1),
				"key_facts":         []string{},
				"negation_polarity": false,
			})
		}
		if atomic.AddInt32(&g.calls, 1) == 1 && len(segments) > 0 {
			// A duplicated index the production parser has to reject: the whole batch is re-asked and
			// only the complete answer is accepted, which is the repair the joined acceptance asserts.
			segments = append(segments, segments[0])
		}
		body, _ := json.Marshal(map[string]any{"segments": segments})
		writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_joined_157", string(body))
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *issue157AnchoredGateway) callCount() int    { return int(atomic.LoadInt32(&g.calls)) }
func (g *issue157AnchoredGateway) sawGlossary() bool { return atomic.LoadInt32(&g.glossarySeen) == 1 }

// issue157EnqueueRunWithGlossary starts the run with a glossary frozen into its config snapshot, the way
// an operator's request freezes it before any stage reads it.
func issue157EnqueueRunWithGlossary(t *testing.T, h *testHarness, jobID, source, target string) string {
	t.Helper()
	cfg := mustJSON(t, map[string]any{"glossary": []domain.GlossaryEntry{{Source: source, Target: target}}})
	body := mustJSON(t, map[string]string{"config_snapshot_json": string(cfg)})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create run with a frozen glossary failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 create run with a frozen glossary, got %d", resp.StatusCode)
	}
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res.Run.ID
}

// issue157TranslationVariant decodes the run's pinned translation variant from CAS.
func issue157TranslationVariant(t *testing.T, h *testHarness, casHash string) *domain.TranslationVariant {
	t.Helper()
	rc, err := h.casStore.Get(casHash)
	if err != nil {
		t.Fatalf("read translation variant %s from CAS: %v", casHash, err)
	}
	defer rc.Close()
	var variant domain.TranslationVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		t.Fatalf("decode translation variant %s: %v", casHash, err)
	}
	return &variant
}

// TestSeam1_Issue157_JoinedPipelineSelectionThroughRenderAndPortableReplay is the joined acceptance of
// the final slice: the production run pipeline translates through the real gateway parser - the run's
// frozen glossary travels on the wire and the malformed first batch is repaired in bounded fashion -
// then parks on an unselectable review-only tempo candidate, the operator accepts that exact candidate,
// and the accepted selection survives a restart and a bundle replay onto a host with a different CAS root.
func TestSeam1_Issue157_JoinedPipelineSelectionThroughRenderAndPortableReplay(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the tempo candidate and render the rebuilt preview")
	}

	gateway := newIssue157AnchoredGateway(t)
	h := setupHarnessWithOptions(t, harnessOptions{autoRunExecutor: true, translationGatewayURL: gateway.URL})

	// One 300ms dialogue slot; a 330ms fixed-rate candidate needs a 1.1x transform, which is the
	// review-only atempo band (#156) and therefore parks the run with exactly one candidate. The
	// glossary term is one the transcribed turn actually contains, so it is effective, not inert.
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 330

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "issue157_joined.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := issue157EnqueueRunWithGlossary(t, h, jobID, "语音", "thoại")

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

	// AC8: this run translated through the production gateway path, not around it - the parser rejected
	// the malformed first batch, the bounded repair re-asked exactly once, and the run's frozen glossary
	// travelled on the wire and resolved onto the translation the run actually carries.
	if calls := gateway.callCount(); calls != 2 {
		t.Fatalf("expected the malformed first batch to be repaired in one re-ask, got %d gateway calls", calls)
	}
	if !gateway.sawGlossary() {
		t.Fatal("the run's frozen glossary never reached the gateway payload")
	}
	transIdx, err := h.db.GetTranslationVariantIndexByRun(context.Background(), runID)
	if err != nil || transIdx == nil {
		t.Fatalf("read the run's translation index: %v", err)
	}
	translation := issue157TranslationVariant(t, h, transIdx.CASHash)
	if len(translation.EffectiveGlossary.Entries) != 1 || translation.EffectiveGlossary.Hash == "" {
		t.Fatalf("expected the frozen glossary to resolve onto the run's translation, got %+v", translation.EffectiveGlossary)
	}
	if len(translation.Segments) == 0 {
		t.Fatalf("expected the repaired batch to be translated, got %+v", translation)
	}
	if got := strings.TrimSpace(translation.Segments[0].TargetText); got == "" || got == translation.Segments[0].SourceText {
		t.Fatalf("expected an anchored translation of the repaired batch, got %+v", translation.Segments[0])
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

	// The accepted delivery must publish a final render too, and the run's own final endpoint must
	// resolve exactly the artifact the acceptance reported: a regression that stops the final render
	// from being produced or indexed would pass on the preview alone.
	if accepted.FinalRenderCAS == "" {
		t.Fatalf("expected the accepted candidate to publish a final render, got %+v", accepted)
	}
	respFinal, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/final?run_id=%s&target_language=vi", h.server.URL, assetID, runID))
	if err != nil {
		t.Fatalf("final render request: %v", err)
	}
	defer respFinal.Body.Close()
	if respFinal.StatusCode != http.StatusOK {
		t.Fatalf("expected the run's final render to resolve, got %d", respFinal.StatusCode)
	}
	var finalBody struct {
		FinalRender struct {
			CASHash string `json:"cas_hash"`
		} `json:"final_render"`
	}
	if err := json.NewDecoder(respFinal.Body).Decode(&finalBody); err != nil {
		t.Fatalf("decode final render: %v", err)
	}
	if finalBody.FinalRender.CASHash != accepted.FinalRenderCAS {
		t.Fatalf("the final endpoint must resolve the artifact the acceptance reported: %s != %s",
			finalBody.FinalRender.CASHash, accepted.FinalRenderCAS)
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
	// What the second host receives is scrubbed: the exporting host's own directory never travels
	// inside the bundle. The import below is what proves the accepted candidate, its natural parent and
	// the approval lineage survive the replay onto a different CAS root.
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("read the exported bundle as a zip: %v", err)
	}
	checkedManifest := false
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "manifest.json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open the exported manifest %s: %v", f.Name, err)
		}
		raw, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			t.Fatalf("read the exported manifest %s: %v", f.Name, readErr)
		}
		checkedManifest = true
		if issue157CarriesHostPath(raw, h.dir) {
			t.Fatalf("the exported manifest must not carry the exporting host's path, found %s", h.dir)
		}
	}
	if !checkedManifest {
		t.Fatal("the exported bundle carries no manifest to verify")
	}
	// The manifest is the human-readable half of the delivery; the payload half is the committed
	// artifacts themselves, shipped under their own hashes. A machine-local path inside one of them
	// travels with that hash into every bundle - and the dubbing artifact's hash is the acceptance
	// identity - so the payload is scanned too, not only the manifest.
	var leaked []string
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "manifest.json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open the exported payload %s: %v", f.Name, err)
		}
		raw, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			t.Fatalf("read the exported payload %s: %v", f.Name, readErr)
		}
		if issue157CarriesHostPath(raw, h.dir) {
			leaked = append(leaked, fmt.Sprintf("%s (%d bytes)", f.Name, len(raw)))
		}
	}
	if len(leaked) > 0 {
		t.Errorf("the exported payload must not carry the exporting host's path %s, found in: %s",
			h.dir, strings.Join(leaked, ", "))
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

	// The importing host restores the run's dubbing artifact under the hash every other record of that
	// run names: its dub_synthesize pin, its mix's DubSegmentsCAS and the acceptance evidence inside
	// the artifact itself. A locally re-encoded second hash would leave the imported delivery looking
	// stale, so an identical replay would withdraw and rebuild media the target host already serves.
	sourceVariant, err := h.db.GetDubSegmentsVariantIndexByRun(context.Background(), runID)
	if err != nil || sourceVariant == nil || sourceVariant.CASHash == "" {
		t.Fatalf("read the source dubbing artifact row: %v (%+v)", err, sourceVariant)
	}
	importVariant, err := importHost.db.GetDubSegmentsVariantIndexByRun(context.Background(), runID)
	if err != nil || importVariant == nil {
		t.Fatalf("read the imported dubbing artifact row: %v (%+v)", err, importVariant)
	}
	if !strings.EqualFold(importVariant.CASHash, sourceVariant.CASHash) {
		t.Fatalf("the imported run must resolve the dubbing artifact %s it was exported with, got %s",
			sourceVariant.CASHash, importVariant.CASHash)
	}
	importPin, err := importHost.db.GetStageArtifactHash(context.Background(), runID, "dub_synthesize")
	if err != nil || strings.TrimSpace(importPin) == "" {
		t.Fatalf("read the imported run's dubbing stage pin: %v (%q)", err, importPin)
	}
	if !strings.EqualFold(importPin, importVariant.CASHash) {
		t.Fatalf("the imported run's dubbing stage pin %s must be the artifact its index row claims, got %s",
			importPin, importVariant.CASHash)
	}
	// A bundle is the run's portable delivery, so the importing host must resolve the artifacts the
	// run was exported with - its script, voice assignment and mix - not only the dubbing artifact.
	for _, probe := range []struct {
		name string
		read func() error
	}{
		{"dub script", func() error {
			_, err := importHost.db.GetDubScriptVariantIndexByRun(context.Background(), runID)
			return err
		}},
		{"voice assignment", func() error {
			_, err := importHost.db.GetVoiceAssignmentIndexByRunID(context.Background(), runID)
			return err
		}},
		{"dub mix", func() error {
			_, err := importHost.db.GetDubMixArtifactIndexByRun(context.Background(), runID)
			return err
		}},
	} {
		if err := probe.read(); err != nil {
			t.Fatalf("the imported host must resolve the run's exported %s: %v", probe.name, err)
		}
	}

	// Playback on the importing host has to survive a restart of that host, and it has to be the
	// committed waveform: the target host serves it from its own CAS root, so a read that followed a
	// path recorded by the exporting host would either fail or serve different bytes.
	importRestarted := issue157RestartHost(t, importHost)
	servedResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", importRestarted.server.URL, runID, evidence.SelectedAudioSHA256))
	if err != nil || servedResp.StatusCode != http.StatusOK {
		t.Fatalf("the importing host must serve the accepted waveform after restart: status=%v err=%v", statusOf(servedResp), err)
	}
	servedBytes, readErr := io.ReadAll(servedResp.Body)
	servedResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read the served accepted waveform: %v", readErr)
	}
	if sum := sha256.Sum256(servedBytes); !strings.EqualFold(hex.EncodeToString(sum[:]), evidence.SelectedAudioSHA256) {
		t.Fatalf("the importing host served %d bytes that are not the accepted waveform %s",
			len(servedBytes), evidence.SelectedAudioSHA256)
	}

	// The rendered half of the delivery resolves there too, from its own CAS root, and it is the same
	// media: the accepted candidate rebuilt this render, so the output object the importing host
	// serves is the one the exporting host serves, and the row it resolves it through is the artifact
	// the bundle shipped rather than a copy rewritten for this host.
	for _, probe := range []struct {
		name string
		path string
		key  string
	}{
		{"preview", fmt.Sprintf("/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", assetID, runID), "preview_render"},
		{"final", fmt.Sprintf("/api/v1/assets/%s/render/final?run_id=%s&target_language=vi", assetID, runID), "final_render"},
	} {
		sourcePlan, sourceOutput := issue157RenderLineage(t, h, probe.path, probe.key)
		importPlan, importOutput := issue157RenderLineage(t, importRestarted, probe.path, probe.key)
		if importOutput == "" || !strings.EqualFold(importOutput, sourceOutput) {
			t.Fatalf("the importing host must resolve the run's %s media %s, got %s", probe.name, sourceOutput, importOutput)
		}
		if !strings.EqualFold(importPlan, sourcePlan) {
			t.Fatalf("the importing host must resolve the %s plan %s the render was frozen from, got %s", probe.name, sourcePlan, importPlan)
		}
	}

	// A bundle is portable through more than one hop: what the importing host exports must be as free
	// of machine-local paths as the bundle it received, checked against both hosts' directories. A
	// rewrite that resolved an artifact for the importing host would show up here and nowhere else.
	reexport, err := http.Post(importRestarted.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("re-export the imported job: %v", err)
	}
	if reexport.StatusCode != http.StatusOK && reexport.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(reexport.Body)
		reexport.Body.Close()
		t.Fatalf("re-export the imported job: status=%d body=%s", reexport.StatusCode, string(body))
	}
	reexportBytes, err := io.ReadAll(reexport.Body)
	reexport.Body.Close()
	if err != nil || len(reexportBytes) == 0 {
		t.Fatalf("read the re-exported bundle: %v (len %d)", err, len(reexportBytes))
	}
	rezr, err := zip.NewReader(bytes.NewReader(reexportBytes), int64(len(reexportBytes)))
	if err != nil {
		t.Fatalf("read the re-exported bundle as a zip: %v", err)
	}
	var reexportLeaks []string
	for _, f := range rezr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open the re-exported payload %s: %v", f.Name, err)
		}
		raw, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			t.Fatalf("read the re-exported payload %s: %v", f.Name, readErr)
		}
		if issue157CarriesHostPath(raw, h.dir) || issue157CarriesHostPath(raw, importHost.dir) {
			reexportLeaks = append(reexportLeaks, f.Name)
		}
	}
	if len(reexportLeaks) > 0 {
		t.Errorf("a bundle exported by the importing host must not carry either host's paths, found in: %s", strings.Join(reexportLeaks, ", "))
	}

	// The re-export must be the run's delivery again, not merely a well-formed archive: what the
	// export path writes for this run, with the bytes behind the artifacts it serves. An empty or
	// partial archive has no manifest and no payload, so both are required here.
	entries := make(map[string]int, len(rezr.File))
	manifestFound := false
	for _, f := range rezr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open the re-exported entry %s: %v", f.Name, err)
		}
		raw, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			t.Fatalf("read the re-exported entry %s: %v", f.Name, readErr)
		}
		entries[f.Name] = len(raw)
		if strings.HasSuffix(f.Name, "manifest.json") && len(raw) > 0 {
			manifestFound = true
		}
	}
	if !manifestFound {
		t.Fatalf("the re-exported bundle must carry a nonempty manifest, got %d entries: %v", len(entries), entries)
	}
	reexportVariant := issue157RunVariant(t, importRestarted, runID)
	for _, required := range []struct {
		name string
		hash string
	}{
		{"retained natural waveform", evidence.NaturalAudioSHA256},
		{"accepted waveform", evidence.SelectedAudioSHA256},
		{"accepted dubbing artifact", reexportVariant.CASHash},
	} {
		found := false
		for name, size := range entries {
			if strings.Contains(name, required.hash) && size > 0 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the re-exported bundle must ship the %s %s with its bytes, got %v",
				required.name, required.hash, entries)
		}
	}

	// Multi-hop portability is the claim under test, so the re-export is imported a second time: a
	// third host with its own CAS root must resolve the same delivery from what the middle host wrote.
	thirdHost := setupHarness(t)
	if thirdHost.dir == h.dir || thirdHost.dir == importHost.dir {
		t.Fatal("expected the third host to use a different CAS root from both earlier hosts")
	}
	thirdResp, err := http.Post(thirdHost.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(reexportBytes))
	if err != nil {
		t.Fatalf("import the re-exported bundle: %v", err)
	}
	thirdBody, _ := io.ReadAll(thirdResp.Body)
	thirdResp.Body.Close()
	if thirdResp.StatusCode != http.StatusOK && thirdResp.StatusCode != http.StatusCreated {
		t.Fatalf("import the re-exported bundle: status=%d body=%s", thirdResp.StatusCode, string(thirdBody))
	}
	thirdServed, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", thirdHost.server.URL, runID, evidence.SelectedAudioSHA256))
	if err != nil || thirdServed.StatusCode != http.StatusOK {
		t.Fatalf("the third host must serve the accepted waveform from the re-export: status=%v err=%v",
			statusOf(thirdServed), err)
	}
	thirdBytes, err := io.ReadAll(thirdServed.Body)
	thirdServed.Body.Close()
	if err != nil {
		t.Fatalf("read the third host's accepted waveform: %v", err)
	}
	if sum := sha256.Sum256(thirdBytes); !strings.EqualFold(hex.EncodeToString(sum[:]), evidence.SelectedAudioSHA256) {
		t.Fatalf("the third host served %d bytes that are not the accepted waveform %s",
			len(thirdBytes), evidence.SelectedAudioSHA256)
	}
	if thirdVariant := issue157RunVariant(t, thirdHost, runID); len(thirdVariant.AcceptedCandidates) != 1 ||
		thirdVariant.AcceptedCandidates[0].SelectedAudioSHA256 != evidence.SelectedAudioSHA256 {
		t.Fatalf("the third host must resolve the acceptance lineage the re-export carried, got %+v",
			thirdVariant.AcceptedCandidates)
	}

	// An identical replay on the importing host answers from the recorded decision: the run the
	// bundle carried is acceptable there without re-deciding anything.
	replayStatus, replay, replayRefusal := issue157Accept(t, importRestarted, runID, issue157Selection(item.ID))
	if replayStatus != http.StatusOK || replay == nil || !replay.Idempotent {
		t.Fatalf("expected the imported acceptance replay to answer from the recorded decision, got %d (%s)", replayStatus, replayRefusal)
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

// TestSeam1_Issue157_AcceptanceOnAnImportedRunRebuildsWithoutAQueueRow is the regression for a host
// that imported the run: a bundle carries the run, not the exporting host's local pipeline row, so the
// importing host holds no queue entry for it. An acceptance that completes the run's coverage there
// must still rebuild and deliver the accepted waveform instead of faulting on the missing row, and it
// must report the state the run was restored with rather than a completion it never performed - the
// same tolerance the replayed acceptance already applies to that case.
func TestSeam1_Issue157_AcceptanceOnAnImportedRunRebuildsWithoutAQueueRow(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	h := setupHarness(t)
	jobID, _, runID, items := issue157ParkedFixture(t, h, false)
	if len(items) == 0 {
		t.Fatal("expected a parked run with a pending review item")
	}

	// Export while the items are still undecided: the bundle carries the review state the importing
	// host has to finish, which is what makes its acceptance a fresh decision and not a replay.
	exportResp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/export", h.server.URL, jobID), "application/json", nil)
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
	defaultVITTSFake(t, importHost)
	impResp, err := http.Post(importHost.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(zipBytes))
	if err != nil {
		t.Fatalf("import bundle: %v", err)
	}
	impBody, _ := io.ReadAll(impResp.Body)
	impResp.Body.Close()
	if impResp.StatusCode != http.StatusOK && impResp.StatusCode != http.StatusCreated {
		t.Fatalf("import bundle status=%d body=%s", impResp.StatusCode, string(impBody))
	}
	if _, err := importHost.db.GetQueueEntryByRunID(ctx, runID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the importing host must hold no queue row for the imported run, got %v", err)
	}

	// Drive every group the imported run still owes, exactly as an operator would.
	rebuilt := false
	for {
		pending := issue157ReviewItems(t, importHost, runID)
		if len(pending) == 0 {
			break
		}
		status, result, refusal := issue157Accept(t, importHost, runID, issue157Selection(pending[0].ID))
		if status != http.StatusOK || result == nil {
			t.Fatalf("accepting %s on the importing host failed: status=%d refusal=%s", pending[0].ID, status, refusal)
		}
		if result.RunCompleted {
			t.Errorf("an imported run holds no local queue row, so the acceptance must not report a completion it never performed: %+v", result)
		}
		if !result.CoverageComplete {
			continue
		}
		// Closing the coverage is where the rebuild runs, and the delivery it produced is this
		// acceptance's own: a missing local queue row must not turn it into a fault.
		if result.SelectedAudioSHA256 == "" || result.FinalRenderCAS == "" {
			t.Fatalf("the acceptance must rebuild the delivery on the accepted waveform, got %+v", result)
		}
		rebuilt = true
	}
	if !rebuilt {
		t.Fatal("expected an acceptance that closed the imported run's coverage")
	}
	if pending := issue157ReviewItems(t, importHost, runID); len(pending) != 0 {
		t.Fatalf("expected every imported review item to be resolved, got %+v", pending)
	}
	successor := issue157RunVariant(t, importHost, runID)
	if successor.OverallStatus != "PASS" || len(successor.AcceptedCandidates) == 0 {
		t.Fatalf("the imported run must carry the accepted decisions, got status=%s decisions=%+v",
			successor.OverallStatus, successor.AcceptedCandidates)
	}
	mix := issue157ReadMix(t, importHost, runID)
	if mix == nil {
		t.Fatal("the accepted decisions must rebuild the imported run's mix")
	}
	if !strings.EqualFold(mix.DubSegmentsCAS, successor.CASHash) {
		t.Fatalf("the imported run's mix must be built from its accepted successor %s, got %s",
			successor.CASHash, mix.DubSegmentsCAS)
	}
}

// issue157RestartHost rebuilds a RuntimeHost over the same database and CAS root, simulating a
// process restart without losing persisted state.
func issue157RestartHost(t *testing.T, h *testHarness) *testHarness {
	t.Helper()
	return issue157RestartHostWithOptions(t, h, harnessOptions{})
}

// issue157RestartAutoRunHost restarts a RuntimeHost the way a production host comes back: over the
// same persisted state, with the queue executor enabled, so a run the operator resumes is actually
// driven to its next state instead of waiting on an executor that never returns.
func issue157RestartAutoRunHost(t *testing.T, h *testHarness) *testHarness {
	t.Helper()
	return issue157RestartHostWithOptions(t, h, harnessOptions{autoRunExecutor: true})
}

func issue157RestartHostWithOptions(t *testing.T, h *testHarness, opts harnessOptions) *testHarness {
	t.Helper()
	srv, registry, router := newRuntimeHostWithOptions(t, h.db, h.casStore, h.queueSvc, h.scheduler, opts)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})
	return &testHarness{
		server: ts,
		srv:    srv,
		db:     h.db,
		// The restarted host resolves providers through its own registry and router: a test that has
		// to configure provider lanes for the runs this host resumes configures these.
		casStore:   h.casStore,
		registry:   registry,
		router:     router,
		queueSvc:   h.queueSvc,
		scheduler:  h.scheduler,
		dubbingSvc: h.dubbingSvc,
		dir:        h.dir,
	}
}
