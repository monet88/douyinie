package seam1_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #157: the refusal, partial-approval and concurrency gates of exact reviewed-candidate
// selection.
//
// The joined case in issue_157_candidate_selection_test.go proves the whole pipeline reaches an
// accepted selection and replays it. These cases drive the same RuntimeHost API (Seam 1) from a
// directly pinned lineage so each gate is proven on its own: a selection that is not the operator's
// exact pending candidate is refused, an incomplete acceptance never mixes anything, and two
// concurrent selections cannot mint two decisions.

// issue157Run reads the run over the API.
func issue157Run(t *testing.T, h *testHarness, runID string) domain.LocalizationRun {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s", h.server.URL, runID))
	if err != nil {
		t.Fatalf("GET run %s: %v", runID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run %s status=%d", runID, resp.StatusCode)
	}
	var body struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode run %s: %v", runID, err)
	}
	return body.Run
}

// issue157ParkedFixture pins a two-turn lineage, synthesizes it and returns the run's parked
// review items. Each turn keeps a 150ms gap to the next canonical boundary, so the frozen 30%
// natural reserve leaves a window far shorter than the 1200ms of synthesized speech: the overrun
// needs a factor inside the review-only atempo band (#156, 1 < factor <= 1.25) that no attempt may
// auto-approve. Each turn therefore projects as an unresolved review unit with exactly one
// selectable candidate. sameVoice drives the whole run through a single voice (issue #93/#155).
func issue157ParkedFixture(t *testing.T, h *testHarness, sameVoice bool) (jobID, assetID, runID string, items []domain.ReviewItem) {
	t.Helper()
	return issue157ParkedFixtureWithMedia(t, h, 6.0, sameVoice)
}

// issue157ParkedFixtureWithMedia is the same two-turn parked lineage over a caller-chosen source
// media duration. A media shorter than the pinned source timeline is the state in which the
// acceptance's own window gate holds but the mixer's placement gate must refuse, which is the hard
// media refusal the route's status mapping is asserted against.
func issue157ParkedFixtureWithMedia(t *testing.T, h *testHarness, mediaSeconds float64, sameVoice bool) (jobID, assetID, runID string, items []domain.ReviewItem) {
	t.Helper()
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Xin chao ban"},
		{speaker: "SPEAKER_01", startMs: 1150, endMs: 2150, source: "再见", spoken: "Tam biet ban"},
	}
	roles := []domain.AudioSegment{
		{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 1150, EndMs: 2150, Role: domain.AudioRoleNarrationDialogue},
	}
	jobID, runID = createJobAndRunWithDuration(t, h, mediaSeconds)
	assetID = getJobViaAPI(t, h, jobID).SourceAssetID

	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID, roles, turns)
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 1200

	assignPayload := map[string]any{"run_id": runID, "target_language": "vi"}
	if sameVoice {
		assignPayload["use_same_voice_for_all"] = true
	}
	respAssign, assign := runAssignVoices(t, h, assetID, assignPayload)
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assign.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 2 || len(variant.Segments) != 0 {
		selected := make([]string, 0, len(variant.Segments))
		for _, seg := range variant.Segments {
			selected = append(selected, fmt.Sprintf("%s slot=%d-%d", seg.SpeakerID, seg.StartMs, seg.EndMs))
		}
		reviewed := make([]string, 0, len(variant.ReviewSegments))
		for _, rev := range variant.ReviewSegments {
			reviewed = append(reviewed, fmt.Sprintf("%s#%d slot=%d-%d playbackEnd=%d", rev.SpeakerID, rev.Index, rev.StartMs, rev.EndMs, rev.DubPlaybackEndMs))
		}
		t.Fatalf("expected two unresolved review units and no selection, got status=%s selected=%v reviewed=%v",
			variant.OverallStatus, selected, reviewed)
	}
	// The run parks on the unresolved units exactly as the drained pipeline leaves it, so every
	// acceptance below resolves a paused run rather than a never-started one.
	parkRunForReview(t, h, runID)
	items = issue157ReviewItems(t, h, runID)
	if len(items) != 2 {
		t.Fatalf("expected two pending review items, got %+v", items)
	}
	if items[0].ID == items[1].ID {
		t.Fatalf("expected two distinct review units, got %s twice", items[0].ID)
	}
	return jobID, assetID, runID, items
}

// issue157Selection is the payload an operator sends for one exact candidate.
func issue157Selection(itemID string) map[string]any {
	return map[string]any{
		"review_item_id":  itemID,
		"candidate":       "transformed",
		"manual_override": true,
		"reason":          "operator auditioned the tempo alternative and accepted it",
		"operator":        "seam1-reviewer",
	}
}

// TestSeam1_Issue157_SelectionRefusesUngovernedBinding proves an acceptance only ever applies to
// the operator's exact pending candidate of this run: every mismatch is refused, nothing is
// published and the run's artifacts and review queue stay as they were.
func TestSeam1_Issue157_SelectionRefusesUngovernedBinding(t *testing.T) {
	h := setupHarness(t)
	_, assetID, runID, items := issue157ParkedFixture(t, h, false)
	item := items[0]
	base := issue157RunVariant(t, h, runID)

	otherJobID, otherRunID := createJobAndRunWithDuration(t, h, 1.0)

	expectRefusal := func(name string, runID string, payload map[string]any, wantStatus int, wantIn string) {
		t.Helper()
		status, result, refusal, err := issue157PostAccept(h, runID, payload)
		if err != nil {
			t.Fatalf("%s: POST accept-candidate: %v", name, err)
		}
		if status != wantStatus {
			t.Fatalf("%s: expected %d, got %d (%s)", name, wantStatus, status, refusal)
		}
		if result != nil {
			t.Fatalf("%s: a refused selection must not report a result: %+v", name, result)
		}
		if !strings.Contains(refusal, wantIn) {
			t.Fatalf("%s: expected the refusal to name %q, got %q", name, wantIn, refusal)
		}
	}

	expectRefusal("changed target language", runID,
		withSelectionValue(issue157Selection(item.ID), "target_language", "en"),
		http.StatusConflict, "belongs to asset")
	expectRefusal("changed job", runID,
		withSelectionValue(issue157Selection(item.ID), "job_id", "job-somewhere-else"),
		http.StatusConflict, "belongs to job")
	expectRefusal("missing audit reason", runID,
		withSelectionValue(issue157Selection(item.ID), "reason", "   "),
		http.StatusBadRequest, "reason/notes is required")
	expectRefusal("unsupported candidate", runID,
		withSelectionValue(issue157Selection(item.ID), "candidate", "speedup"),
		http.StatusBadRequest, "unsupported candidate")
	expectRefusal("unknown review item", runID,
		issue157Selection("rev-dubseg-rev-not-an-artifact-0"),
		http.StatusConflict, "superseded dubbing artifact")
	expectRefusal("malformed review item", runID,
		issue157Selection("not-a-dubbing-review-item"),
		http.StatusConflict, "is not a dubbing review item")
	expectRefusal("fit unit that is not unresolved", runID,
		issue157Selection(fmt.Sprintf("rev-dubseg-rev-%s-7", base.CASHash)),
		http.StatusConflict, "is not unresolved in run")
	expectRefusal("foreign run's item", otherRunID,
		issue157Selection(item.ID),
		http.StatusConflict, "owns no dubbing artifact")

	// The asset-scoped entry point binds the run it names to the asset in the path, and refuses a
	// selection that does not name a run at all.
	otherAssetID := getJobViaAPI(t, h, otherJobID).SourceAssetID
	foreignBody := mustJSON(t, withSelectionValue(issue157Selection(item.ID), "run_id", runID))
	foreignResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/review/accept-candidate", h.server.URL, otherAssetID),
		"application/json", bytes.NewReader(foreignBody))
	if err != nil {
		t.Fatalf("POST asset-scoped accept-candidate: %v", err)
	}
	foreignRaw, _ := io.ReadAll(foreignResp.Body)
	foreignResp.Body.Close()
	if foreignResp.StatusCode != http.StatusConflict || !strings.Contains(string(foreignRaw), "belongs to asset") {
		t.Fatalf("expected the asset-scoped selection for a foreign asset to be refused with 409, got %d %s",
			foreignResp.StatusCode, string(foreignRaw))
	}
	noRunResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/review/accept-candidate", h.server.URL, assetID),
		"application/json", bytes.NewReader(mustJSON(t, issue157Selection(item.ID))))
	if err != nil {
		t.Fatalf("POST asset-scoped accept-candidate: %v", err)
	}
	noRunRaw, _ := io.ReadAll(noRunResp.Body)
	noRunResp.Body.Close()
	if noRunResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(noRunRaw), "run_id is required") {
		t.Fatalf("expected an asset-scoped selection without a run to be refused with 400, got %d %s",
			noRunResp.StatusCode, string(noRunRaw))
	}

	// Every refusal above was a refusal: nothing was published for the run.
	if now := issue157RunVariant(t, h, runID); now.CASHash != base.CASHash {
		t.Fatalf("a refused selection must not move the run's dubbing artifact: %s -> %s", base.CASHash, now.CASHash)
	}
	if pending := issue157ReviewItems(t, h, runID); len(pending) != 2 {
		t.Fatalf("a refused selection must not resolve review items: %+v", pending)
	}
	if mix := issue157StageArtifact(t, h, runID, "audio_mix"); mix != "" {
		t.Fatalf("a refused selection must not mix: audio_mix artifact %s", mix)
	}
	if _, err := h.db.GetDubMixArtifactIndexByRun(context.Background(), runID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a refused selection must not publish a mix index row: %v", err)
	}
	if overrides := issue157OverrideCount(t, h, runID); overrides != 0 {
		t.Fatalf("a refused selection must not write audit rows, got %d", overrides)
	}
}

// withSelectionValue copies a selection payload with one value replaced.
func withSelectionValue(payload map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	out[key] = value
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return data
}

// TestSeam1_Issue157_SelectionRefusesCancelledRun proves a run that is no longer in flight keeps
// its history but cannot release media the operator selected after it stopped.
func TestSeam1_Issue157_SelectionRefusesCancelledRun(t *testing.T) {
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixture(t, h, false)
	base := issue157RunVariant(t, h, runID)

	resp, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/cancel", h.server.URL, runID), "application/json", nil)
	if err != nil {
		t.Fatalf("POST cancel run: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel run status=%d", resp.StatusCode)
	}

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusConflict || result != nil {
		t.Fatalf("expected a cancelled run to refuse the selection with 409, got status=%d result=%+v refusal=%s", status, result, refusal)
	}
	if !strings.Contains(refusal, "cancelled") {
		t.Fatalf("expected the refusal to name the run's status, got %q", refusal)
	}
	if now := issue157RunVariant(t, h, runID); now.CASHash != base.CASHash {
		t.Fatalf("a refused selection must not move the cancelled run's artifact: %s -> %s", base.CASHash, now.CASHash)
	}
	if mix := issue157StageArtifact(t, h, runID, "audio_mix"); mix != "" {
		t.Fatalf("a cancelled run must not mix: audio_mix artifact %s", mix)
	}
}

// TestSeam1_Issue157_SelectionRefusesSupersededDubbingArtifact proves the item identity is the
// artifact the operator was looking at: once the run's dubbing artifact is superseded, the old
// item cannot be accepted into the new one.
func TestSeam1_Issue157_SelectionRefusesSupersededDubbingArtifact(t *testing.T) {
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixture(t, h, false)
	ctx := context.Background()

	idx, err := h.db.GetDubSegmentsVariantIndexByRun(ctx, runID)
	if err != nil || idx == nil {
		t.Fatalf("read run dub variant index: %v", err)
	}
	rc, err := h.casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("read run dub variant from CAS: %v", err)
	}
	var superseding domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&superseding); err != nil {
		rc.Close()
		t.Fatalf("decode run dub variant: %v", err)
	}
	rc.Close()

	// A superseding pass: the same units, different bytes (so a different artifact identity).
	superseding.CASHash = ""
	superseding.CreatedAt = superseding.CreatedAt.Add(time.Minute)
	superseding.ProvenanceHash = idx.ProvenanceHash + "-superseding"
	obj, err := h.casStore.Put(bytes.NewReader(mustJSON(t, superseding)))
	if err != nil {
		t.Fatalf("put superseding dub variant: %v", err)
	}
	if err := h.db.UpsertDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             idx.ID,
		AssetID:        idx.AssetID,
		RunID:          idx.RunID,
		JobID:          idx.JobID,
		TargetLanguage: idx.TargetLanguage,
		CASHash:        obj.SHA256,
		ProvenanceHash: superseding.ProvenanceHash,
		OverallStatus:  superseding.OverallStatus,
		CreatedAt:      superseding.CreatedAt,
	}); err != nil {
		t.Fatalf("save superseding dub variant index: %v", err)
	}

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusConflict || result != nil {
		t.Fatalf("expected the superseded item to be refused with 409, got status=%d result=%+v refusal=%s", status, result, refusal)
	}
	if !strings.Contains(refusal, "superseded dubbing artifact") {
		t.Fatalf("expected the refusal to name the superseded artifact, got %q", refusal)
	}
	if now := issue157RunVariant(t, h, runID); now.CASHash != obj.SHA256 {
		t.Fatalf("the superseding artifact must stay the run's artifact: %s != %s", now.CASHash, obj.SHA256)
	}
	if overrides := issue157OverrideCount(t, h, runID); overrides != 0 {
		t.Fatalf("a refused selection must not write audit rows, got %d", overrides)
	}
}

// TestSeam1_Issue157_PartialApprovalStaysPausedAndNeverMixesPartially proves a per-group acceptance
// while other units are unresolved: the run keeps its accepted evidence and stays paused with no
// partial mix, and only the last required group makes the descendants real.
func TestSeam1_Issue157_PartialApprovalStaysPausedAndNeverMixesPartially(t *testing.T) {
	h := setupHarness(t)
	_, assetID, runID, items := issue157ParkedFixture(t, h, true)
	ctx := context.Background()
	base := issue157RunVariant(t, h, runID)

	status, first, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || first == nil {
		t.Fatalf("expected the first group's acceptance to succeed, got %d (%s)", status, refusal)
	}
	if first.CoverageComplete || !first.RunPaused || first.RemainingReviewCount != 1 {
		t.Fatalf("expected an incomplete acceptance to keep the run paused with one unit left, got %+v", first)
	}
	if first.DubMixCAS != "" || first.RenderPlanCAS != "" || first.PreviewRenderCAS != "" {
		t.Fatalf("an incomplete acceptance must not rebuild any descendant, got %+v", first)
	}

	// No partial mix anywhere: not a stage pin, not an index row, not a served render.
	for _, stage := range []string{"audio_mix", "render_plan", "render_preview"} {
		if artifact := issue157StageArtifact(t, h, runID, stage); artifact != "" {
			t.Fatalf("an incomplete acceptance must not pin the %s stage, got %s", stage, artifact)
		}
	}
	if _, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("an incomplete acceptance must not publish a mix index row: %v", err)
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID)); err != nil {
		t.Fatalf("preview request: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("an incomplete acceptance must not serve a preview built from part of the run")
		}
	}

	// The successor carries the accepted group and keeps the other one unresolved.
	partial := issue157RunVariant(t, h, runID)
	if partial.CASHash != first.DubSegmentsVariantCAS || partial.CASHash == base.CASHash {
		t.Fatalf("expected the run to hold the successor %s, got %s", first.DubSegmentsVariantCAS, partial.CASHash)
	}
	if partial.OverallStatus != "REVIEW_REQUIRED" || len(partial.Segments) != 1 || len(partial.ReviewSegments) != 1 {
		t.Fatalf("expected one selected and one unresolved unit, got status=%s selected=%d review=%d",
			partial.OverallStatus, len(partial.Segments), len(partial.ReviewSegments))
	}
	if len(partial.AcceptedCandidates) != 1 || partial.AcceptedCandidates[0].ReviewItemID != items[0].ID {
		t.Fatalf("expected the accepted group's evidence, got %+v", partial.AcceptedCandidates)
	}
	// The unit that was accepted carries the accepted waveform; the other unit's slots are untouched.
	if got := partial.Segments[0].SpeakerID; got != items[0].SpeakerID {
		t.Fatalf("the accepted unit must stay the one the operator selected, got speaker %s (item %s)", got, items[0].SpeakerID)
	}
	if got := partial.Segments[0].AudioSHA256; got != first.SelectedAudioSHA256 {
		t.Fatalf("the selected unit must carry the accepted waveform, got %s", got)
	}
	// The remaining group is re-projected against the successor artifact, so its item id carries
	// that new identity: the unit is identified by its index and speaker, not by the id the
	// operator read before the first acceptance.
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 || pending[0].ItemIndex != items[1].ItemIndex || pending[0].SpeakerID != items[1].SpeakerID {
		t.Fatalf("expected only the second group (unit %d/%s) still pending, got %+v", items[1].ItemIndex, items[1].SpeakerID, pending)
	}
	if run := issue157Run(t, h, runID); run.Status == domain.RunStatusCompleted {
		t.Fatalf("a partially accepted run must not complete, got %s", run.Status)
	}

	// The id the operator read before the first acceptance names the superseded artifact, so an
	// approval carrying it fails closed instead of landing on a state the operator never reviewed.
	staleStatus, _, staleRefusal := issue157Accept(t, h, runID, issue157Selection(items[1].ID))
	if staleStatus != http.StatusConflict || !strings.Contains(staleRefusal, "superseded dubbing artifact") {
		t.Fatalf("expected the pre-acceptance id of the second group to be refused as stale, got %d (%s)", staleStatus, staleRefusal)
	}

	// The last required group, against the artifact the run actually holds: only now are the
	// descendants rebuilt from the accepted waveforms.
	status, second, refusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if status != http.StatusOK || second == nil {
		t.Fatalf("expected the last group's acceptance to succeed, got %d (%s)", status, refusal)
	}
	if !second.CoverageComplete || second.RemainingReviewCount != 0 || second.DubMixCAS == "" || second.RenderPlanCAS == "" || second.PreviewRenderCAS == "" {
		t.Fatalf("expected a complete acceptance to rebuild the delivery, got %+v", second)
	}
	complete := issue157RunVariant(t, h, runID)
	if complete.CASHash != second.DubSegmentsVariantCAS || complete.OverallStatus != "PASS" {
		t.Fatalf("expected a passing successor, got status=%s cas=%s", complete.OverallStatus, complete.CASHash)
	}
	if len(complete.Segments) != 2 || len(complete.ReviewSegments) != 0 || len(complete.AcceptedCandidates) != 2 {
		t.Fatalf("expected both units selected with both acceptance records, got selected=%d review=%d evidence=%d",
			len(complete.Segments), len(complete.ReviewSegments), len(complete.AcceptedCandidates))
	}
	if pending := issue157ReviewItems(t, h, runID); len(pending) != 0 {
		t.Fatalf("expected an empty review queue after complete coverage, got %+v", pending)
	}

	// The rebuilt mix is the one built from the variant that carries both accepted waveforms,
	// and both accepted waveforms are playable from the run.
	mixIdx, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixIdx == nil || mixIdx.CASHash != second.DubMixCAS {
		t.Fatalf("expected the run's mix to be the rebuilt one %s, got %+v (%v)", second.DubMixCAS, mixIdx, err)
	}
	mixRC, err := h.casStore.Get(mixIdx.CASHash)
	if err != nil {
		t.Fatalf("read rebuilt mix from CAS: %v", err)
	}
	var mix domain.DubMixArtifact
	if err := json.NewDecoder(mixRC).Decode(&mix); err != nil {
		mixRC.Close()
		t.Fatalf("decode rebuilt mix: %v", err)
	}
	mixRC.Close()
	if mix.DubSegmentsCAS != complete.CASHash {
		t.Fatalf("the rebuilt mix must pin the fully accepted variant %s, got %s", complete.CASHash, mix.DubSegmentsCAS)
	}
	for _, hash := range []string{first.SelectedAudioSHA256, second.SelectedAudioSHA256} {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/dub-media/%s", h.server.URL, runID, hash))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("accepted waveform %s must be playable: status=%v err=%v", hash, statusOf(resp), err)
		}
		resp.Body.Close()
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected a rebuilt preview after complete coverage: status=%v err=%v", statusOf(resp), err)
	} else {
		resp.Body.Close()
	}
}

// TestSeam1_Issue157_SelectionRefusesChangedContractLineage proves the approval is bound to the
// text/contract lineage the operator reviewed: once the run re-pins a different dub script, the
// parked item cannot be accepted into the new lineage.
func TestSeam1_Issue157_SelectionRefusesChangedContractLineage(t *testing.T) {
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixture(t, h, false)
	ctx := context.Background()

	// The same canonical turns spoken differently by a later pass, re-pinned the way the inspector's
	// correction path re-pins the run's lineage: the text the operator reviewed is no longer the text
	// the run carries.
	scriptIdx, err := h.db.GetDubScriptVariantIndexByRun(ctx, runID)
	if err != nil || scriptIdx == nil {
		t.Fatalf("read run dub script index: %v", err)
	}
	scriptRC, err := h.casStore.Get(scriptIdx.CASHash)
	if err != nil {
		t.Fatalf("read run dub script from CAS: %v", err)
	}
	var script domain.DubScriptVariant
	if err := json.NewDecoder(scriptRC).Decode(&script); err != nil {
		scriptRC.Close()
		t.Fatalf("decode run dub script: %v", err)
	}
	scriptRC.Close()
	for i := range script.Segments {
		script.Segments[i].SpokenText = script.Segments[i].SpokenText + " (rewritten)"
		script.Segments[i].MeaningText = script.Segments[i].SpokenText
	}
	script.CASHash = ""
	rePinned, err := h.casStore.Put(bytes.NewReader(mustJSON(t, script)))
	if err != nil {
		t.Fatalf("put re-pinned dub script: %v", err)
	}
	if err := h.db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID:             scriptIdx.ID,
		AssetID:        scriptIdx.AssetID,
		RunID:          scriptIdx.RunID,
		JobID:          scriptIdx.JobID,
		TargetLanguage: scriptIdx.TargetLanguage,
		CASHash:        rePinned.SHA256,
		ProvenanceHash: scriptIdx.ProvenanceHash,
		ProviderID:     scriptIdx.ProviderID,
		ModelName:      scriptIdx.ModelName,
		ModelVersion:   scriptIdx.ModelVersion,
		OverallQAScore: scriptIdx.OverallQAScore,
		CreatedAt:      scriptIdx.CreatedAt,
	}); err != nil {
		t.Fatalf("save re-pinned dub script index: %v", err)
	}
	if rePinned.SHA256 == scriptIdx.CASHash {
		t.Fatal("fixture must re-pin different dub script bytes")
	}

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusConflict || result != nil {
		t.Fatalf("expected a changed dub-script lineage to refuse the selection with 409, got status=%d result=%+v refusal=%s", status, result, refusal)
	}
	if !strings.Contains(refusal, "lineage mismatch") {
		t.Fatalf("expected the refusal to name the lineage mismatch, got %q", refusal)
	}
	if overrides := issue157OverrideCount(t, h, runID); overrides != 0 {
		t.Fatalf("a refused selection must not write audit rows, got %d", overrides)
	}
	if mix := issue157StageArtifact(t, h, runID, "audio_mix"); mix != "" {
		t.Fatalf("a refused selection must not mix: audio_mix artifact %s", mix)
	}
}

// TestSeam1_Issue157_InterruptedDeliveryRebuildIsResumedByRetry proves a rebuild failure cannot
// leave an approval that only exists on paper: the accepted decision and its audit row are kept,
// the delivery stays visibly unbuilt, and the operator's retry of the same selection rebuilds it.
func TestSeam1_Issue157_InterruptedDeliveryRebuildIsResumedByRetry(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host to fail a preview render on its missing source")
	}
	h := setupHarness(t)
	_, assetID, runID, items := issue157ParkedFixture(t, h, false)
	ctx := context.Background()

	// The first group's acceptance is saved without any delivery (coverage is incomplete).
	firstStatus, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if firstStatus != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first group to be accepted without a rebuild, got %d (%s)", firstStatus, firstRefusal)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 {
		t.Fatalf("expected the second group still pending, got %+v", pending)
	}

	// The source video the preview render needs is unavailable: the mix and the plan for the
	// accepted selection can still be built, and the failure lands mid-delivery.
	asset, err := h.db.GetSourceAsset(ctx, assetID)
	if err != nil || asset == nil {
		t.Fatalf("read ingested asset: %v", err)
	}
	videoPath, err := h.casStore.ResolvePath(asset.SHA256)
	if err != nil {
		t.Fatalf("resolve source media path: %v", err)
	}
	if err := os.Rename(videoPath, videoPath+".away"); err != nil {
		t.Fatalf("withdraw source media: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_ = os.Rename(videoPath+".away", videoPath)
			restored = true
		}
	}
	t.Cleanup(restore)

	status, _, refusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if status == http.StatusOK {
		t.Fatalf("expected the interrupted rebuild to surface as a failure, got %d (%s)", status, refusal)
	}
	restore()

	// The decision itself survived: the run holds the successor with both acceptances, the audit
	// rows are recorded, and the delivery the run may serve was never produced from it.
	interrupted := issue157RunVariant(t, h, runID)
	if interrupted.OverallStatus != "PASS" || len(interrupted.AcceptedCandidates) != 2 {
		t.Fatalf("the accepted decision must survive a failed rebuild, got status=%s evidence=%d",
			interrupted.OverallStatus, len(interrupted.AcceptedCandidates))
	}
	if got := issue157OverrideCount(t, h, runID); got != 2 {
		t.Fatalf("both acceptances must stay audited through a failed rebuild, got %d rows", got)
	}
	if artifact := issue157StageArtifact(t, h, runID, "render_preview"); artifact != "" {
		t.Fatalf("a failed preview must not leave a preview artifact, got %s", artifact)
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID)); err != nil {
		t.Fatalf("preview request: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a run whose rebuild failed must not serve a preview")
		}
	}

	// The operator retries the same exact selection: the replay re-drives the interrupted rebuild
	// instead of reporting media the run does not have.
	retryStatus, retried, retryRefusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if retryStatus != http.StatusOK || retried == nil || !retried.Idempotent {
		t.Fatalf("expected the retry to resume the acceptance, got %d (%s)", retryStatus, retryRefusal)
	}
	if !retried.CoverageComplete || retried.PreviewRenderCAS == "" || retried.DubMixCAS == "" || retried.RenderPlanCAS == "" {
		t.Fatalf("expected the retry to rebuild the delivery from the accepted waveforms, got %+v", retried)
	}
	if got := issue157RunVariant(t, h, runID); got.CASHash != interrupted.CASHash {
		t.Fatalf("the retry must not mint another decision: %s != %s", got.CASHash, interrupted.CASHash)
	}
	if got := issue157OverrideCount(t, h, runID); got != 2 {
		t.Fatalf("the retry must not append another audit row, got %d", got)
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the resumed rebuild to serve a preview: status=%v err=%v", statusOf(resp), err)
	} else {
		resp.Body.Close()
	}
}

// issue157ResumeStages names the stages a resumed dubbing run records attempts under, in pipeline
// order. A resume must not buy again any stage that had already succeeded.
var issue157ResumeStages = []string{
	"audio_role_plan", "speech_understand", "translation", "dub_script", "voice_assignment",
	"dub_synthesize", "audio_mix", "text_detection", "visual_text_localize", "render_plan",
	"render_preview", "render_final",
}

// issue157StageArtifacts records the artifact every pipeline stage of the run currently publishes, so
// a resume can be judged against the state it started from: a stage that already published an artifact
// must still publish the same one afterwards.
func issue157StageArtifacts(t *testing.T, h *testHarness, runID string) map[string]string {
	t.Helper()
	artifacts := make(map[string]string, len(issue157ResumeStages))
	for _, stage := range issue157ResumeStages {
		artifacts[stage] = issue157StageArtifact(t, h, runID, stage)
	}
	return artifacts
}

// issue157MixFromSegments asks the runtime host to mix the given dubbing artifact, exactly as the
// run's own mix stage does, and returns the CAS of the artifact the host published.
func issue157MixFromSegments(t *testing.T, h *testHarness, assetID, runID, segmentsCAS string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"run_id":           runID,
		"target_language":  "vi",
		"dub_segments_cas": segmentsCAS,
		"preserve_singing": true,
	})
	if err != nil {
		t.Fatalf("encode audio mix request: %v", err)
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-mix", h.server.URL, assetID), "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request audio mix: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		DubMix *domain.DubMixArtifact `json:"dub_mix"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode audio mix response (status %d): %v", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("audio mix returned %d: %+v", resp.StatusCode, body)
	}
	if body.DubMix == nil || body.DubMix.CASHash == "" {
		t.Fatalf("audio mix published no artifact: %+v", body)
	}
	return body.DubMix.CASHash
}

// issue157PinStalePlan stores the render plan artifact a delivery held before the operator replaced
// the dubbing artifact it was frozen from - a plan pinning the given mix - and returns its CAS.
func issue157PinStalePlan(t *testing.T, h *testHarness, assetID, runID, mixCAS string) string {
	t.Helper()
	blob, err := json.Marshal(domain.RenderPlan{
		ID:             "rp-replaced-" + runID,
		SchemaVersion:  domain.RenderPlanSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: domain.TargetLanguageVI,
		DubMixCASHash:  mixCAS,
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode render plan fixture: %v", err)
	}
	obj, err := h.casStore.Put(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("store render plan fixture: %v", err)
	}
	return obj.SHA256
}

// issue157PinStage records a succeeded stage execution naming an artifact: the row a pipeline pass,
// or a reviewer correction, leaves behind for a delivery it published.
func issue157PinStage(t *testing.T, h *testHarness, runID, stage, casHash string) {
	t.Helper()
	now := time.Now().UTC()
	if err := h.db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             "se-" + stage + "-" + runID,
		RunID:          runID,
		Stage:          stage,
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: casHash,
		StartedAt:      &now,
		CompletedAt:    &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("record the %s delivery pin: %v", stage, err)
	}
}

// issue157ReadPlan decodes the render plan artifact the run currently holds.
func issue157ReadPlan(t *testing.T, h *testHarness, runID string) *domain.RenderPlan {
	t.Helper()
	idx, err := h.db.GetRenderPlanIndexByRun(context.Background(), runID)
	if err != nil || idx == nil {
		t.Fatalf("read run %s render plan index: %v (%+v)", runID, err, idx)
	}
	rc, err := h.casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("read render plan %s: %v", idx.CASHash, err)
	}
	defer rc.Close()
	var plan domain.RenderPlan
	if err := json.NewDecoder(rc).Decode(&plan); err != nil {
		t.Fatalf("decode render plan %s: %v", idx.CASHash, err)
	}
	return &plan
}

// issue157QualityResultCount counts the run's recorded QA results.
func issue157QualityResultCount(t *testing.T, h *testHarness, runID string) int {
	t.Helper()
	rows, err := h.db.GetQualityResultsByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read quality results for run %s: %v", runID, err)
	}
	return len(rows)
}

// issue157ProviderAttempts counts every provider attempt the run recorded, whichever stage it ran
// under: asking for a hand-listed set of stages would let an attempt that an acceptance added under
// another valid stage - audio_role_plan, for one - pass unnoticed.
func issue157ProviderAttempts(t *testing.T, h *testHarness, runID string) int {
	t.Helper()
	rows, err := h.db.ListProviderAttempts(context.Background(), runID, "")
	if err != nil {
		t.Fatalf("read provider attempts for run %s: %v", runID, err)
	}
	return len(rows)
}

// TestSeam1_Issue157_GroupedUnitAcceptanceKeepsCanonicalMembership proves an acceptance of a unit
// that covers several canonical turns - a bounded regroup group - promotes the whole group: every
// canonical member the unit covered stays covered exactly once by the accepted segment, and the
// single accepted waveform replaces the whole group's slot.
func TestSeam1_Issue157_GroupedUnitAcceptanceKeepsCanonicalMembership(t *testing.T) {
	h := setupHarness(t)
	// Two turns of one speaker inside the bounded regroup gap: the pass merges them into one group,
	// so the unit the operator reviews covers both canonical members at once.
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Xin chao ban"},
		{speaker: "SPEAKER_00", startMs: 1150, endMs: 2150, source: "再见", spoken: "Tam biet ban"},
	}
	roles := []domain.AudioSegment{
		{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 1150, EndMs: 2150, Role: domain.AudioRoleNarrationDialogue},
	}
	jobID, runID := createJobAndRunWithDuration(t, h, 6.0)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID, roles, turns)
	lane := defaultVITTSFake(t, h)
	// The group's window is the two slots plus their gap; a 2400ms synthesis overruns it inside the
	// review-only atempo band, so the whole group parks as one unresolved unit.
	lane.DurationMs = 2400

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assign.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 1 || len(variant.Segments) != 0 {
		t.Fatalf("expected one unresolved grouped unit, got status=%s review=%d selected=%d",
			variant.OverallStatus, len(variant.ReviewSegments), len(variant.Segments))
	}
	unit := variant.ReviewSegments[0]
	if len(unit.SpeechBlockIndices) != 2 {
		t.Fatalf("expected the bounded regroup to cover both canonical members, got %v", unit.SpeechBlockIndices)
	}

	parkRunForReview(t, h, runID)
	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected one pending review item for the grouped unit, got %+v", items)
	}
	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || result == nil {
		t.Fatalf("expected the grouped unit to be accepted, got %d (%s)", status, refusal)
	}
	if !result.CoverageComplete {
		t.Fatalf("the only unit of the run was accepted, so coverage must be complete: %+v", result)
	}

	successor := issue157RunVariant(t, h, runID)
	if successor.OverallStatus != "PASS" || len(successor.ReviewSegments) != 0 || len(successor.Segments) != 1 {
		t.Fatalf("expected one accepted segment and no unresolved unit, got status=%s review=%d selected=%d",
			successor.OverallStatus, len(successor.ReviewSegments), len(successor.Segments))
	}
	promoted := successor.Segments[0]
	if !slices.Equal(promoted.SpeechBlockIndices, unit.SpeechBlockIndices) {
		t.Fatalf("the promoted segment must keep the group's canonical membership %v, got %v",
			unit.SpeechBlockIndices, promoted.SpeechBlockIndices)
	}
	if promoted.AudioSHA256 != result.SelectedAudioSHA256 {
		t.Fatalf("the promoted group must carry the accepted waveform %s, got %s", result.SelectedAudioSHA256, promoted.AudioSHA256)
	}
	covered := map[int]int{}
	for _, seg := range successor.Segments {
		for _, member := range seg.SpeechBlockIndices {
			covered[member]++
		}
	}
	for _, rev := range successor.ReviewSegments {
		for _, member := range rev.SpeechBlockIndices {
			covered[member]++
		}
	}
	if len(covered) != 2 || covered[0] != 1 || covered[1] != 1 {
		t.Fatalf("expected both canonical members covered exactly once, got %v", covered)
	}
	if len(successor.AcceptedCandidates) != 1 {
		t.Fatalf("expected one acceptance record for the grouped unit, got %+v", successor.AcceptedCandidates)
	}
}

// issue157CreateRunInPosture creates one more run for a job with an explicit posture frozen in its
// config snapshot, which is how a run is submitted in Review posture.
func issue157CreateRunInPosture(t *testing.T, h *testHarness, jobID string, posture domain.ReviewPosture) string {
	t.Helper()
	body := mustJSON(t, map[string]string{"config_snapshot_json": fmt.Sprintf(`{"posture":%q}`, posture)})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create %s run: %v", posture, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %s run status=%d body=%s", posture, resp.StatusCode, string(raw))
	}
	var result struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode created %s run: %v", posture, err)
	}
	return result.Run.ID
}

// issue157ReviewPostureFixture pins one overrunning dialogue turn in a run submitted in Review
// posture, so an acceptance rebuilds the delivery and stops at the operator's explicit final render.
func issue157ReviewPostureFixture(t *testing.T, h *testHarness) (jobID, assetID, runID string, items []domain.ReviewItem) {
	t.Helper()
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Xin chao ban"},
	}
	roles := []domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}

	jobID, _ = createJobAndRunWithDuration(t, h, 4.0)
	assetID = getJobViaAPI(t, h, jobID).SourceAssetID
	runID = issue157CreateRunInPosture(t, h, jobID, domain.ReviewPostureReview)

	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID, roles, turns)
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 1200

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assign.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil || variant.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("expected the review-posture run to park on one unresolved unit, got %v", variant)
	}
	parkRunForReview(t, h, runID)
	items = issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected one pending review unit, got %+v", items)
	}
	return jobID, assetID, runID, items
}

// TestSeam1_Issue157_ReviewPostureKeepsExplicitFinalRenderStart proves a complete acceptance under
// Review posture rebuilds the delivery and stops at the operator's explicit final render: no final
// render is executed or pinned, and the result reports the run as still paused.
func TestSeam1_Issue157_ReviewPostureKeepsExplicitFinalRenderStart(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host to rebuild a preview render")
	}
	h := setupHarness(t)
	_, _, runID, items := issue157ReviewPostureFixture(t, h)
	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || result == nil {
		t.Fatalf("expected the acceptance to succeed, got %d (%s)", status, refusal)
	}
	if !result.CoverageComplete || result.PreviewRenderCAS == "" || result.DubMixCAS == "" || result.RenderPlanCAS == "" {
		t.Fatalf("expected the accepted delivery to be rebuilt, got %+v", result)
	}
	if result.HandoffAction != "start_final_render" {
		t.Fatalf("review posture must expose the explicit final-render start, got %q (%s)", result.HandoffAction, result.HandoffMessage)
	}
	if !result.RunPaused || result.RunCompleted {
		t.Fatalf("a review-posture acceptance must leave the run paused on the operator's render, got paused=%v completed=%v",
			result.RunPaused, result.RunCompleted)
	}
	if result.FinalRenderCAS != "" {
		t.Fatalf("review posture must render no final, got %s", result.FinalRenderCAS)
	}
	if artifact := issue157StageArtifact(t, h, runID, "render_final"); artifact != "" {
		t.Fatalf("review posture must not pin a final render, got %s", artifact)
	}
	if run := issue157Run(t, h, runID); run.Status == domain.RunStatusCompleted {
		t.Fatalf("review posture must not complete the run before the explicit render, got %s", run.Status)
	}

	// A repeated identical acceptance is the same report: the delivery exists, so the replay states
	// the state the run actually reached and never releases the final render the operator still owes.
	replayStatus, replayed, replayRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if replayStatus != http.StatusOK || replayed == nil || !replayed.Idempotent {
		t.Fatalf("expected an idempotent replay, got %d (%s)", replayStatus, replayRefusal)
	}
	if replayed.HandoffAction != "start_final_render" || !replayed.RunPaused || replayed.RunCompleted {
		t.Fatalf("a review-posture replay must report the paused state it left, got %+v", replayed)
	}
	if replayed.FinalRenderCAS != "" {
		t.Fatalf("a review-posture replay must not report a final render, got %s", replayed.FinalRenderCAS)
	}
	if replayed.PreviewRenderCAS != result.PreviewRenderCAS || replayed.DubMixCAS != result.DubMixCAS {
		t.Fatalf("the replay must read back the rebuilt delivery: %+v vs %+v", replayed, result)
	}
}

// TestSeam1_Issue157_ConcurrentSelectionsCannotMintTwoDecisions proves two selections of the same
// unit race into exactly one decision: the loser fails closed and its retry is the same idempotent
// acceptance, so the run ends with one successor, one audit row and one acceptance record.
func TestSeam1_Issue157_ConcurrentSelectionsCannotMintTwoDecisions(t *testing.T) {
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixture(t, h, true)
	payload := issue157Selection(items[0].ID)

	type outcome struct {
		status  int
		result  *service.AcceptReviewedCandidateResult
		refusal string
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, result, refusal, err := issue157PostAccept(h, runID, payload)
			results <- outcome{status, result, refusal, err}
		}()
	}
	wg.Wait()
	close(results)

	committed := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("POST accept-candidate: %v", got.err)
		}
		switch got.status {
		case http.StatusOK:
			committed++
		case http.StatusConflict:
			// The loser of the artifact claim: the operator's retry of the same exact selection
			// must resolve to the winner's acceptance rather than mint another one.
			status, result, refusal, err := issue157PostAccept(h, runID, payload)
			if err != nil || status != http.StatusOK || result == nil || !result.Idempotent {
				t.Fatalf("a selection refused by a concurrent acceptance must retry into the same acceptance: status=%d err=%v refusal=%s", status, err, refusal)
			}
			committed++
		default:
			t.Fatalf("unexpected status %d (%s)", got.status, got.refusal)
		}
	}
	if committed != 2 {
		t.Fatalf("expected both selections to resolve to the same decision, got %d", committed)
	}

	variant := issue157RunVariant(t, h, runID)
	if len(variant.AcceptedCandidates) != 1 || len(variant.Segments) != 1 {
		t.Fatalf("concurrent selections must leave exactly one decision, got evidence=%+v selected=%d",
			variant.AcceptedCandidates, len(variant.Segments))
	}
	if got := issue157OverrideCount(t, h, runID); got != 1 {
		t.Fatalf("concurrent selections must leave exactly one audit row, got %d", got)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 || pending[0].ItemIndex != items[1].ItemIndex {
		t.Fatalf("only the accepted unit may leave the queue, got %+v", pending)
	}
}

// issue157ReadMix decodes the run's current dub mix artifact.
func issue157ReadMix(t *testing.T, h *testHarness, runID string) *domain.DubMixArtifact {
	t.Helper()
	idx, err := h.db.GetDubMixArtifactIndexByRun(context.Background(), runID)
	if err != nil || idx == nil {
		t.Fatalf("read run %s dub mix index: %v", runID, err)
	}
	rc, err := h.casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("read run %s dub mix from CAS (%s): %v", runID, idx.CASHash, err)
	}
	defer rc.Close()
	var mix domain.DubMixArtifact
	if err := json.NewDecoder(rc).Decode(&mix); err != nil {
		t.Fatalf("decode run %s dub mix: %v", runID, err)
	}
	return &mix
}

// issue157HasWindow reports whether a preservation plan carries the exact interval.
func issue157HasWindow(windows []domain.PreservationWindow, startMs, endMs int64) bool {
	for _, w := range windows {
		if w.StartMs == startMs && w.EndMs == endMs {
			return true
		}
	}
	return false
}

// TestSeam1_Issue157_AutoPostureAcceptanceRendersFinalAndCompletesRun proves the auto-posture end of
// the slice: the last required acceptance rebuilds the delivery, executes and pins the final render,
// and only then completes the run and its job; the repeated identical acceptance reports that state
// as the idempotent replay it is.
func TestSeam1_Issue157_AutoPostureAcceptanceRendersFinalAndCompletesRun(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to render the final delivery")
	}
	h := setupHarness(t)
	ctx := context.Background()
	jobID, assetID, runID, items := issue157ParkedFixture(t, h, false)

	firstStatus, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if firstStatus != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first group to be accepted without a rebuild, got %d (%s)", firstStatus, firstRefusal)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 {
		t.Fatalf("expected one group still pending, got %+v", pending)
	}

	status, last, refusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if status != http.StatusOK || last == nil {
		t.Fatalf("expected the last acceptance to succeed, got %d (%s)", status, refusal)
	}
	if !last.CoverageComplete || !last.RunCompleted || last.RunPaused {
		t.Fatalf("expected a completed auto run, got %+v", last)
	}
	if last.HandoffAction != "auto_render_started" || last.FinalRenderCAS == "" {
		t.Fatalf("expected an executed auto final render, got action=%q cas=%q", last.HandoffAction, last.FinalRenderCAS)
	}
	if stage := issue157StageArtifact(t, h, runID, "render_final"); stage == "" {
		t.Fatal("expected the render_final stage to record the executed handoff")
	}
	finalIdx, err := h.db.GetLatestRenderArtifactIndex(ctx, assetID, "vi", domain.RenderKindFinal)
	if err != nil || finalIdx == nil || finalIdx.CASHash != last.FinalRenderCAS {
		t.Fatalf("expected the executed final render to be the run's current one, got %+v (%v)", finalIdx, err)
	}
	if run := issue157Run(t, h, runID); run.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the accepted run to complete, got %s", run.Status)
	}
	if job := getJobViaAPI(t, h, jobID); job.Status != "completed" {
		t.Fatalf("expected the accepted run's job to complete, got %s", job.Status)
	}

	// The replay reports the delivery it reached and finishes nothing twice.
	replayStatus, replayed, replayRefusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if replayStatus != http.StatusOK || replayed == nil || !replayed.Idempotent {
		t.Fatalf("expected an idempotent replay, got %d (%s)", replayStatus, replayRefusal)
	}
	if !replayed.RunCompleted || replayed.RunPaused {
		t.Fatalf("a completed auto delivery must replay as completed, got paused=%v completed=%v",
			replayed.RunPaused, replayed.RunCompleted)
	}
	if replayed.HandoffAction != "auto_render_started" || replayed.FinalRenderCAS != last.FinalRenderCAS {
		t.Fatalf("the replay must report the executed final render, got action=%q cas=%q", replayed.HandoffAction, replayed.FinalRenderCAS)
	}
	if got := issue157OverrideCount(t, h, runID); got != 2 {
		t.Fatalf("the replay must not append audit rows, got %d", got)
	}
}

// TestSeam1_Issue157_DeliveredRunCompletionRespectsTheQueueGate proves the shared terminal transition
// refuses to finish a run whose queue entry is not the state that decision resolves, so neither the
// pipeline nor an acceptance can promote a cancelled, finished or never-started run to a completed
// one - the gate the acceptance path previously lacked.
func TestSeam1_Issue157_DeliveredRunCompletionRespectsTheQueueGate(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	// A never-started run is not a delivery the acceptance may finish.
	_, queuedRun := createJobAndRun(t, h)
	if err := service.CompleteDeliveredRun(ctx, h.db, queuedRun, true); err == nil {
		t.Fatal("expected a queued run to be refused as a completion target")
	}
	if run := issue157Run(t, h, queuedRun); run.Status == domain.RunStatusCompleted {
		t.Fatalf("a queued run must not be completed, got %s", run.Status)
	}

	// A cancelled run keeps its history and is never promoted.
	_, cancelRun := createJobAndRun(t, h)
	if err := h.queueSvc.Cancel(ctx, cancelRun); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if err := service.CompleteDeliveredRun(ctx, h.db, cancelRun, true); err != nil {
		t.Fatalf("a cancelled run is a no-op for completion, got %v", err)
	}
	if run := issue157Run(t, h, cancelRun); run.Status != domain.RunStatusCancelled {
		t.Fatalf("a cancelled run must stay cancelled, got %s", run.Status)
	}

	// A paused run is completed only by the caller that resolves the park.
	parkedJob, parkedRun := createJobAndRun(t, h)
	parkRunForReview(t, h, parkedRun)
	if err := service.CompleteDeliveredRun(ctx, h.db, parkedRun, false); err != nil {
		t.Fatalf("the running-only gate is a no-op for a paused run, got %v", err)
	}
	if run := issue157Run(t, h, parkedRun); run.Status != domain.RunStatusPaused {
		t.Fatalf("the running-only gate must leave a paused run paused, got %s", run.Status)
	}
	if err := service.CompleteDeliveredRun(ctx, h.db, parkedRun, true); err != nil {
		t.Fatalf("complete the parked run: %v", err)
	}
	if run := issue157Run(t, h, parkedRun); run.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the parked run to complete, got %s", run.Status)
	}
	if job := getJobViaAPI(t, h, parkedJob); job.Status != "completed" {
		t.Fatalf("expected the parked run's job to complete, got %s", job.Status)
	}

	// An already-finished run is refused instead of being re-completed or marked failed.
	if err := service.CompleteDeliveredRun(ctx, h.db, parkedRun, true); err == nil {
		t.Fatal("expected a second completion of a finished run to be refused")
	}
	if run := issue157Run(t, h, parkedRun); run.Status != domain.RunStatusCompleted {
		t.Fatalf("a refused re-completion must leave the run completed, got %s", run.Status)
	}

	// A queue entry that still claims an active run while the run row itself has already stopped is
	// refused by the guarded write, and the job write that preceded it is undone with it: the
	// transition decides one run state, not two.
	divergentJob, divergentRun := createJobAndRun(t, h)
	if err := h.queueSvc.MarkRunning(ctx, divergentRun); err != nil {
		t.Fatalf("mark the divergent run running: %v", err)
	}
	priorJobStatus := getJobViaAPI(t, h, divergentJob).Status
	rawDB, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `UPDATE localization_runs SET status = 'cancelled' WHERE id = ?`, divergentRun); err != nil {
		t.Fatalf("stop the run row only: %v", err)
	}
	if err := service.CompleteDeliveredRun(ctx, h.db, divergentRun, true); err != nil {
		t.Fatalf("a run row that stopped on its own terms is a refusal, not a failure, got %v", err)
	}
	entry, err := h.db.GetQueueEntryByRunID(ctx, divergentRun)
	if err != nil {
		t.Fatalf("read the divergent queue entry: %v", err)
	}
	if entry.Status != domain.RunStatusRunning {
		t.Fatalf("the guarded write must not complete a run whose own row stopped, got %s", entry.Status)
	}
	if job := getJobViaAPI(t, h, divergentJob); job.Status != priorJobStatus {
		t.Fatalf("the refused transition must undo the job write: %s -> %s", priorJobStatus, job.Status)
	}
}

// TestSeam1_Issue157_CancelledRunIsNeverReportedAsCompleted proves the acceptance reports only the
// state the run really reached: a cancellation that lands while the accepted delivery is being
// rebuilt owns the run's terminal status, so the decision is recorded and the rebuilt delivery is
// delivered without the acceptance claiming a completion it did not perform.
func TestSeam1_Issue157_CancelledRunIsNeverReportedAsCompleted(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupHarness(t)
	ctx := context.Background()
	_, _, runID, items := issue157ParkedFixture(t, h, false)

	firstStatus, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if firstStatus != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first group to be accepted without a rebuild, got %d (%s)", firstStatus, firstRefusal)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 {
		t.Fatalf("expected one group still pending, got %+v", pending)
	}

	// The cancellation lands at the stage row the handoff records once the delivery is fully rebuilt
	// and immediately before the terminal transition, so the run's own terminal status is not the
	// acceptance's to overwrite: the decision is committed, the rebuild has finished, and the run
	// stays cancelled instead of being reported as completed.
	rawDB, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER cancel_during_rebuild AFTER INSERT ON stage_executions
		WHEN NEW.stage = 'render_final' AND NEW.status = 'succeeded'
		BEGIN
			UPDATE queue_entries SET status = 'cancelled', position = NULL WHERE run_id = NEW.run_id;
			UPDATE localization_runs SET status = 'cancelled' WHERE id = NEW.run_id;
		END;`); err != nil {
		t.Fatalf("create cancel trigger: %v", err)
	}

	status, last, refusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if status != http.StatusOK || last == nil {
		t.Fatalf("expected the accepted decision to be recorded, got %d (%s)", status, refusal)
	}
	if last.FinalRenderCAS == "" {
		t.Fatalf("expected the accepted delivery to be rebuilt, got %+v", last)
	}
	if last.RunCompleted {
		t.Fatalf("a run cancelled during the rebuild must not be reported as completed: %+v", last)
	}
	if strings.Contains(last.Message, "completed the run") {
		t.Fatalf("the report must not claim a completion the run never reached, got %q", last.Message)
	}
	if !strings.Contains(last.Message, domain.RunStatusCancelled) {
		t.Fatalf("the report must name the state the run really reached, got %q", last.Message)
	}
	if run := issue157Run(t, h, runID); run.Status != domain.RunStatusCancelled {
		t.Fatalf("the run's own terminal status must stand, got %s", run.Status)
	}
	if pending := issue157ReviewItems(t, h, runID); len(pending) != 0 {
		t.Fatalf("the accepted decision must still be recorded, pending=%+v", pending)
	}

	// A repeated selection is refused by the run's own state before any replay can report it, and the
	// refusal names the state the run reached: the operator is never told a cancelled run is paused or
	// resumable, and nothing re-drives the delivery that was abandoned.
	status, replay, replayRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusConflict || replay != nil {
		t.Fatalf("expected the cancelled run to refuse a repeated selection, got %d (%+v)", status, replay)
	}
	if !strings.Contains(replayRefusal, domain.RunStatusCancelled) {
		t.Fatalf("the refusal must name the state the run reached, got %q", replayRefusal)
	}
	if run := issue157Run(t, h, runID); run.Status != domain.RunStatusCancelled {
		t.Fatalf("the refusal must not re-drive a cancelled run, got %s", run.Status)
	}
}

// TestSeam1_Issue157_RetryRepairsTheAcceptedDubbingStagePin proves a retry converges the state a
// failed first acceptance leaves behind. The successor is claimed (the run's index row is the
// accepted variant) and the audit row is written before the run's dub_synthesize stage artifact is
// recorded, so a failure in that last write leaves the stage row naming the pre-acceptance variant -
// the artifact a resumed run reuses. The retry re-records the pin, so a resume continues from the
// operator's selection instead of reusing the variant the selection replaced.
func TestSeam1_Issue157_RetryRepairsTheAcceptedDubbingStagePin(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()
	_, _, runID, items := issue157ParkedFixture(t, h, false)

	// The pipeline's own pin is what a resumed run would reuse until the acceptance's retry repairs it.
	preAcceptance := issue157RunVariant(t, h, runID)
	now := time.Now().UTC()
	if err := h.db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             "se-dub-pin-" + runID,
		RunID:          runID,
		Stage:          "dub_synthesize",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: preAcceptance.CASHash,
		StartedAt:      &now,
		CompletedAt:    &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("record the pre-acceptance dubbing pin: %v", err)
	}
	stalePin := issue157StageArtifact(t, h, runID, "dub_synthesize")
	if stalePin != preAcceptance.CASHash {
		t.Fatalf("the run must pin the artifact its synthesis produced, got %s want %s", stalePin, preAcceptance.CASHash)
	}

	// The acceptance's stage write fails after the successor claim and the audit row: exactly the
	// interrupted state the retry has to converge.
	rawDB, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_dubbing_pin BEFORE INSERT ON stage_executions
		WHEN NEW.stage = 'dub_synthesize' AND NEW.status = 'succeeded'
		BEGIN
			SELECT RAISE(ABORT, 'dubbing stage write failed');
		END;`); err != nil {
		t.Fatalf("create failing stage trigger: %v", err)
	}

	status, _, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status == http.StatusOK {
		t.Fatalf("expected the failed stage write to surface, got %d (%s)", status, refusal)
	}
	claimed := issue157RunVariant(t, h, runID)
	if claimed.CASHash == stalePin {
		t.Fatalf("the successor must be claimed before the pin is recorded, still %s", claimed.CASHash)
	}
	if got := issue157StageArtifact(t, h, runID, "dub_synthesize"); got != stalePin {
		t.Fatalf("the failed write must leave the pre-acceptance pin in place, got %s want %s", got, stalePin)
	}

	// The operator retries the same selection once the fault is gone.
	if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_dubbing_pin`); err != nil {
		t.Fatalf("drop failing stage trigger: %v", err)
	}
	retryStatus, retried, retryRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if retryStatus != http.StatusOK || retried == nil || !retried.Idempotent {
		t.Fatalf("expected the retry to replay the recorded decision, got %d (%s)", retryStatus, retryRefusal)
	}
	if got := issue157StageArtifact(t, h, runID, "dub_synthesize"); got != claimed.CASHash {
		t.Fatalf("the retry must record the accepted successor as the run's dubbing pin: %s != %s", got, claimed.CASHash)
	}
	if count := issue157OverrideCount(t, h, runID); count != 1 {
		t.Fatalf("the retry must not append another audit row, got %d", count)
	}
	if variant := issue157RunVariant(t, h, runID); variant.CASHash != claimed.CASHash {
		t.Fatalf("the retry must not mint another decision: %s != %s", variant.CASHash, claimed.CASHash)
	}
	// The repair is a stage record for the accepted artifact: it must not release the run the
	// unresolved unit still holds paused, or report it as anything else.
	if !retried.RunPaused {
		t.Fatalf("the remaining review unit must keep the run paused, got %+v", retried)
	}
	if run := issue157Run(t, h, runID); run.Status != domain.RunStatusPaused {
		t.Fatalf("the retry must leave the run paused, got %s", run.Status)
	}
}

// TestSeam1_Issue157_ResumedRunHonorsTheAcceptedDubbingArtifact proves a resume converges the state a
// failed acceptance leaves behind without the operator re-accepting. The production pipeline parks the
// run on one unresolved candidate; the acceptance claims its successor and writes the audit row, then
// fails before recording the run's dub_synthesize stage artifact - so the stage row still names the
// variant the accepted candidate replaced, and a resume whose dubbing stage reads that row would
// re-park the run on it. The run must continue on the accepted successor instead and deliver media
// mixed from it.
func TestSeam1_Issue157_ResumedRunHonorsTheAcceptedDubbingArtifact(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to resume the accepted delivery")
	}
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	ctx := context.Background()

	// One 300ms dialogue slot whose fixed-rate candidate needs a review-only transform: the pipeline
	// parks the run on exactly one unresolved unit, with every earlier stage pinned by itself.
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 330

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "issue157_resume.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	parked := pollRunStatus(t, h.server.URL, runID, domain.RunStatusPaused, 30*time.Second)
	if parked.Status != domain.RunStatusPaused {
		t.Fatalf("expected the run to park on its unresolved candidate, got %q; stages: %+v; items: %+v",
			parked.Status, getRunStages(t, h.server.URL, runID), issue157ReviewItems(t, h, runID))
	}
	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected exactly one pending review item, got %+v", items)
	}
	// The pipeline's own pin is the variant its synthesis produced: the artifact a resumed run reuses
	// while the accepted successor is only claimed in the run's index row.
	preAcceptance := issue157RunVariant(t, h, runID)
	pinned := issue157StageArtifact(t, h, runID, "dub_synthesize")
	if pinned == "" || pinned != preAcceptance.CASHash {
		t.Fatalf("expected the parked run to pin the variant it synthesized: pin=%s variant=%s", pinned, preAcceptance.CASHash)
	}

	// The acceptance claims its successor and writes the audit row, then fails recording the run's
	// dub_synthesize stage artifact: the interrupted state a resume has to converge.
	rawDB, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_dubbing_pin BEFORE INSERT ON stage_executions
		WHEN NEW.stage = 'dub_synthesize' AND NEW.status = 'succeeded'
		BEGIN
			SELECT RAISE(ABORT, 'dubbing stage write failed');
		END;`); err != nil {
		t.Fatalf("create failing stage trigger: %v", err)
	}
	status, _, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status == http.StatusOK {
		t.Fatalf("expected the failed stage write to surface, got %d (%s)", status, refusal)
	}
	if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_dubbing_pin`); err != nil {
		t.Fatalf("drop failing stage trigger: %v", err)
	}
	claimed := issue157RunVariant(t, h, runID)
	if claimed.CASHash == pinned {
		t.Fatalf("the accepted successor must be claimed before the stage write fails, still %s (refusal: %s)", claimed.CASHash, refusal)
	}
	if claimed.OverallStatus != "PASS" || len(claimed.AcceptedCandidates) != 1 {
		t.Fatalf("expected the claimed successor to carry the decision and complete coverage, got status=%s evidence=%d",
			claimed.OverallStatus, len(claimed.AcceptedCandidates))
	}
	if got := issue157StageArtifact(t, h, runID, "dub_synthesize"); got != pinned {
		t.Fatalf("the failed write must leave the pipeline's pin in place, got %s want %s", got, pinned)
	}
	if mixIdx, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID); err == nil && mixIdx != nil {
		t.Fatalf("an interrupted acceptance must not publish a mix, got %s", mixIdx.CASHash)
	}
	stagesBeforeResume := issue157StageArtifacts(t, h, runID)

	// The operator restarts the host and resumes the run: no repeated acceptance, no new decision.
	h2 := issue157RestartAutoRunHost(t, h)
	configureDialogueTranslationGateway(t, h2)
	resumeResp, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/resume", h2.server.URL, runID), "application/json", nil)
	if err != nil {
		t.Fatalf("resume request: %v", err)
	}
	resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for resume, got %d", resumeResp.StatusCode)
	}
	run := pollRunStatus(t, h2.server.URL, runID, domain.RunStatusCompleted, 30*time.Second)
	if run.Status != domain.RunStatusCompleted {
		t.Fatalf("the resumed run must complete on the accepted successor instead of re-parking on the variant it replaced, got %s", run.Status)
	}

	// The delivery the resumed run published was mixed from the accepted successor: media built from
	// the variant the accepted candidate replaced is never served as the run's current delivery.
	mix := issue157ReadMix(t, h2, runID)
	if !strings.EqualFold(mix.DubSegmentsCAS, claimed.CASHash) {
		t.Fatalf("the resumed run's mix must be mixed from the accepted successor %s, got %s", claimed.CASHash, mix.DubSegmentsCAS)
	}
	// The resume converged the run's recorded lineage to the artifact its mix was built from: the
	// stage row now names the accepted successor instead of the variant the accepted candidate
	// replaced, so a replay never reads a superseded variant as the successful stage.
	if got := issue157StageArtifact(t, h2, runID, "dub_synthesize"); got != claimed.CASHash {
		t.Fatalf("the resumed run's dubbing stage artifact must be the accepted successor %s, got %s", claimed.CASHash, got)
	}
	// Reuse, not re-synthesis: every stage that had already published an artifact before the resume
	// still publishes that artifact afterwards. A resumed run reuses the work it already has, and the
	// dubbing stage is the one that converges - to the accepted successor, not to another synthesis.
	// The mix below is the delivery-level proof: it is mixed from the accepted successor, which only a
	// reuse of that artifact can produce.
	for _, stage := range issue157ResumeStages {
		published := stagesBeforeResume[stage]
		if published == "" || stage == "dub_synthesize" {
			continue
		}
		if got := issue157StageArtifact(t, h2, runID, stage); got != published {
			t.Fatalf("the resume must reuse the %s stage it already had: artifact %s -> %s", stage, published, got)
		}
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h2.server.URL, assetID, runID)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the resumed run to serve its rebuilt preview: status=%v err=%v", statusOf(resp), err)
	} else {
		resp.Body.Close()
	}
	if items := issue157ReviewItems(t, h2, runID); len(items) != 0 {
		t.Fatalf("the accepted selection must stay resolved through the resume, got %+v", items)
	}
	if variant := issue157RunVariant(t, h2, runID); variant.CASHash != claimed.CASHash {
		t.Fatalf("the resume must not mint another decision: %s != %s", variant.CASHash, claimed.CASHash)
	}
	if got := issue157OverrideCount(t, h2, runID); got != 1 {
		t.Fatalf("the resume must not append audit rows, got %d", got)
	}
}

// TestSeam1_Issue157_ResumeWithdrawsADeliveryPinnedToTheReplacedCandidate proves a resume withdraws
// and rebuilds a delivery the run published for a dubbing artifact the operator replaced.
//
// The pipeline parks the run on one unresolved unit, and a delivery is published for the variant its
// own synthesis produced: a real mix of that variant, the plan that pinned that mix, and the preview
// the delivery advertised. The acceptance then dies recording the run's dub_synthesize stage artifact -
// so those rows still name the mix, plan and preview of the artifact the accepted candidate replaced.
// A resume that trusted them would serve media the operator never selected: it must withdraw them and
// rebuild the delivery from the accepted successor, the artifact the run's own index row claims.
func TestSeam1_Issue157_ResumeWithdrawsADeliveryPinnedToTheReplacedCandidate(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	ctx := context.Background()

	// One 300ms dialogue slot whose fixed-rate candidate needs a review-only transform: the pipeline
	// parks the run on exactly one unresolved unit, with every earlier stage pinned by itself.
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 330

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "issue157_supersede.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	parked := pollRunStatus(t, h.server.URL, runID, domain.RunStatusPaused, 30*time.Second)
	items := issue157ReviewItems(t, h, runID)
	if parked.Status != domain.RunStatusPaused || len(items) != 1 {
		t.Fatalf("expected the pipeline to park on one unresolved unit, got %q with items %+v", parked.Status, items)
	}

	// The delivery the run published for the variant its own synthesis produced. The preview pin is
	// never decoded, so an opaque artifact id stands for the preview that delivery advertised.
	replaced := issue157RunVariant(t, h, runID)
	replacedMix := issue157MixFromSegments(t, h, assetID, runID, replaced.CASHash)
	replacedPlan := issue157PinStalePlan(t, h, assetID, runID, replacedMix)
	replacedPreview := "preview-of-the-replaced-delivery"
	issue157PinStage(t, h, runID, "audio_mix", replacedMix)
	issue157PinStage(t, h, runID, "render_plan", replacedPlan)
	issue157PinStage(t, h, runID, "render_preview", replacedPreview)

	// The acceptance claims its successor and writes the audit row, then fails recording the run's
	// dub_synthesize stage artifact: the interrupted state a resume has to converge.
	rawDB, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_dubbing_pin BEFORE INSERT ON stage_executions
		WHEN NEW.stage = 'dub_synthesize' AND NEW.status = 'succeeded'
		BEGIN
			SELECT RAISE(ABORT, 'dubbing stage write failed');
		END;`); err != nil {
		t.Fatalf("create failing stage trigger: %v", err)
	}
	status, _, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status == http.StatusOK {
		t.Fatalf("expected the failed stage write to surface, got %d (%s)", status, refusal)
	}
	if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_dubbing_pin`); err != nil {
		t.Fatalf("drop failing stage trigger: %v", err)
	}
	claimed := issue157RunVariant(t, h, runID)
	if claimed.CASHash == replaced.CASHash {
		t.Fatalf("the accepted successor must be claimed, still %s (refusal: %s)", claimed.CASHash, refusal)
	}
	if claimed.OverallStatus != "PASS" || len(claimed.AcceptedCandidates) != 1 {
		t.Fatalf("expected the decision to complete the successor's coverage, got status=%s evidence=%d",
			claimed.OverallStatus, len(claimed.AcceptedCandidates))
	}
	if got := issue157StageArtifact(t, h, runID, "dub_synthesize"); strings.EqualFold(got, claimed.CASHash) {
		t.Fatalf("the failed write must leave the dubbing stage behind its accepted artifact, got %s", got)
	}
	if got := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(got, replacedMix) {
		t.Fatalf("the replaced delivery's mix must still be the run's pin before the resume, got %s", got)
	}

	// The operator restarts the host and resumes the run, accepting nothing further.
	h2 := issue157RestartAutoRunHost(t, h)
	configureDialogueTranslationGateway(t, h2)
	if resp, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/resume", h2.server.URL, runID), "application/json", nil); err != nil {
		t.Fatalf("resume run: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected the paused run to resume, got %d", resp.StatusCode)
		}
	}
	settled := issue157Run(t, h2, runID)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if settled.Status != domain.RunStatusQueued && settled.Status != domain.RunStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
		settled = issue157Run(t, h2, runID)
	}
	if settled.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the resumed run to complete, got %q; pending: %+v; stages: %+v",
			settled.Status, issue157ReviewItems(t, h2, runID), getRunStages(t, h2.server.URL, runID))
	}

	// The delivery is the accepted successor's, and no pin of the replaced delivery survives: the mix is
	// mixed from the accepted successor and the plan is re-frozen from that mix.
	rebuild := issue157ReadMix(t, h2, runID)
	if !strings.EqualFold(rebuild.DubSegmentsCAS, claimed.CASHash) {
		t.Fatalf("the rebuilt mix must be mixed from the accepted successor %s, got %s", claimed.CASHash, rebuild.DubSegmentsCAS)
	}
	mixIdx, err := h2.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixIdx == nil {
		t.Fatalf("the resumed run must publish a mix for its accepted successor: %v (%+v)", err, mixIdx)
	}
	if strings.EqualFold(mixIdx.CASHash, replacedMix) {
		t.Fatalf("the resume must not reuse the mix published for the replaced candidate, still %s", replacedMix)
	}
	if got := issue157StageArtifact(t, h2, runID, "audio_mix"); !strings.EqualFold(got, mixIdx.CASHash) {
		t.Fatalf("the run's mix pin must name the rebuilt mix %s, got %s", mixIdx.CASHash, got)
	}
	rebuiltMix := mixIdx.CASHash
	if got := issue157StageArtifact(t, h2, runID, "render_plan"); got == "" || strings.EqualFold(got, replacedPlan) {
		t.Fatalf("the run's plan pin must be re-frozen, got %s (replaced plan was %s)", got, replacedPlan)
	}
	if frozen := issue157ReadPlan(t, h2, runID); !strings.EqualFold(frozen.DubMixCASHash, rebuiltMix) {
		t.Fatalf("the re-frozen plan must pin the rebuilt mix %s, got %s", rebuiltMix, frozen.DubMixCASHash)
	}
	if got := issue157StageArtifact(t, h2, runID, "render_preview"); got == "" || got == replacedPreview {
		t.Fatalf("the replaced preview must be withdrawn and replaced, got %q", got)
	}
	if resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h2.server.URL, assetID, runID)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the resumed run to serve its rebuilt preview: status=%v err=%v", statusOf(resp), err)
	} else {
		resp.Body.Close()
	}
	if pending := issue157ReviewItems(t, h2, runID); len(pending) != 0 {
		t.Fatalf("the accepted decision must leave no pending unit, got %+v", pending)
	}
	if variant := issue157RunVariant(t, h2, runID); variant.CASHash != claimed.CASHash {
		t.Fatalf("the resume must not mint another decision: %s != %s", variant.CASHash, claimed.CASHash)
	}
	if got := issue157OverrideCount(t, h2, runID); got != 1 {
		t.Fatalf("the resume must not append audit rows, got %d", got)
	}
}

// TestSeam1_Issue157_CancelledReplayNeverReportsAPause proves a replay reports the run state the
// queue holds at the moment it answers, not the state the acceptance left behind. The operator's
// cancellation lands after this request was admitted - so the request runs to its report - and the
// report must name neither a completion nor a pause: reporting a pause would offer the operator a
// resume of the run they just abandoned.
func TestSeam1_Issue157_CancelledReplayNeverReportsAPause(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()
	_, _, runID, items := issue157ParkedFixture(t, h, false)

	// The first group's acceptance is recorded without a delivery, so no partial mix exists and the
	// run stays parked on the remaining review unit.
	status, first, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first group to be accepted without a rebuild, got %d (%s)", status, refusal)
	}

	// The retry the replay exists for: the successor carries the decision while its audit row was
	// never written. Writing that row is the replay's only write, and the cancellation arrives with
	// it - after the request read the run, before the report reads it again.
	rawDB, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `DELETE FROM review_overrides WHERE run_id = ?`, runID); err != nil {
		t.Fatalf("drop the recorded audit row: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER cancel_during_replay AFTER INSERT ON review_overrides
		WHEN NEW.run_id = '%s'
		BEGIN
			UPDATE queue_entries SET status = 'cancelled', position = NULL WHERE run_id = NEW.run_id;
			UPDATE localization_runs SET status = 'cancelled' WHERE id = NEW.run_id;
		END;`, runID)); err != nil {
		t.Fatalf("create cancel trigger: %v", err)
	}

	status, replay, replayRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || replay == nil || !replay.Idempotent {
		t.Fatalf("expected the replay to answer from the recorded decision, got %d (%s)", status, replayRefusal)
	}
	if replay.RunCompleted {
		t.Fatalf("a cancelled run is not completed: %+v", replay)
	}
	if replay.RunPaused {
		t.Fatalf("a cancelled run must never be reported as paused: %+v", replay)
	}
	if run := issue157Run(t, h, runID); run.Status != domain.RunStatusCancelled {
		t.Fatalf("the replay must leave the state the cancellation set, got %s", run.Status)
	}
	if pending := issue157ReviewItems(t, h, runID); len(pending) != 1 {
		t.Fatalf("the replay must not resolve the remaining review unit, pending=%+v", pending)
	}
}

// TestSeam1_Issue157_RebuildMediaGateIsUnprocessableEntity proves a hard media gate raised by the
// rebuild itself keeps the refusal class the contract documents: the accepted decision stays as
// evidence, and the operator sees 422 - the same class as the selection-time gates - rather than an
// opaque server fault. The fixture's source media is shorter than the pinned source timeline, so the
// accepted window fits its own arithmetic while the mixer refuses to place the waveform past the
// end of the media it must mix into.
func TestSeam1_Issue157_RebuildMediaGateIsUnprocessableEntity(t *testing.T) {
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixtureWithMedia(t, h, 1.2, false)

	status, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first group to be accepted without a rebuild, got %d (%s)", status, firstRefusal)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 {
		t.Fatalf("expected one group still pending, got %+v", pending)
	}

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if status != http.StatusUnprocessableEntity || result != nil {
		t.Fatalf("expected the rebuild's media gate to be 422, got %d (%s)", status, refusal)
	}
	if !strings.Contains(refusal, "audio mixer refused") {
		t.Fatalf("expected the refusal to be the mixer's own gate, got %q", refusal)
	}

	// The decision is evidence: the successor with both acceptances stays the run's dubbing artifact,
	// no mix is reported as accepted, and the run is not released.
	successor := issue157RunVariant(t, h, runID)
	if successor.OverallStatus != "PASS" || len(successor.AcceptedCandidates) != 2 {
		t.Fatalf("expected the accepted decision to survive the refused rebuild, got status=%s evidence=%d",
			successor.OverallStatus, len(successor.AcceptedCandidates))
	}
	if got := issue157OverrideCount(t, h, runID); got != 2 {
		t.Fatalf("expected both acceptances to stay audited, got %d rows", got)
	}
	mixIdx, err := h.db.GetDubMixArtifactIndexByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read the refused mix index: %v", err)
	}
	if mixIdx.OverallStatus == "PASS" {
		t.Fatalf("a refused rebuild must not publish an accepted mix, got %+v", mixIdx)
	}
	if run := issue157Run(t, h, runID); run.Status == domain.RunStatusCompleted {
		t.Fatalf("a refused rebuild must not complete the run, got %s", run.Status)
	}
}

// TestSeam1_Issue157_NoDubSingingPreservationSurvivesAcceptedRebuild proves the accepted rebuild keeps
// the canonical ownership of a source that also carries singing: the singing block is never a dub
// member nor a required replacement, and the rebuilt mix preserves the singing window while it
// suppresses only the dialogue the accepted waveform replaces.
func TestSeam1_Issue157_NoDubSingingPreservationSurvivesAcceptedRebuild(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the mix")
	}
	h := setupHarness(t)

	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Xin chao ban"},
		// A singing stretch the ASR transcribed as speech: never dub-eligible, so it must stay a
		// preserved source window instead of becoming a required replacement.
		{speaker: "SPEAKER_00", startMs: 1150, endMs: 2150, source: "啦", scriptAbsent: true},
	}
	roles := []domain.AudioSegment{
		{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 1150, EndMs: 2150, Role: domain.AudioRoleSingingMusicVocal},
		{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleInstrumentalBgm},
	}
	jobID, runID := createJobAndRunWithDuration(t, h, 6.0)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID, roles, turns)
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 1200

	respAssign, assign := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	if respAssign.StatusCode != http.StatusCreated || assign == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assign.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("dub-synthesize failed: %d", resp.StatusCode)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 1 || len(variant.Segments) != 0 {
		t.Fatalf("expected one unresolved dialogue unit and no selection, got status=%s review=%d selected=%d",
			variant.OverallStatus, len(variant.ReviewSegments), len(variant.Segments))
	}
	if got := variant.ReviewSegments[0].SpeechBlockIndices; !slices.Equal(got, []int{0}) {
		t.Fatalf("the singing block must not be a required replacement, got members %v", got)
	}
	parkRunForReview(t, h, runID)

	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected one pending dialogue unit, got %+v", items)
	}
	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || result == nil || !result.CoverageComplete {
		t.Fatalf("expected the dialogue unit to be accepted with complete coverage, got %d (%s)", status, refusal)
	}

	successor := issue157RunVariant(t, h, runID)
	if successor.OverallStatus != "PASS" || len(successor.Segments) != 1 || len(successor.ReviewSegments) != 0 {
		t.Fatalf("expected one accepted segment and no unresolved unit, got status=%s selected=%d review=%d",
			successor.OverallStatus, len(successor.Segments), len(successor.ReviewSegments))
	}
	for _, seg := range successor.Segments {
		if !slices.Equal(seg.SpeechBlockIndices, []int{0}) {
			t.Fatalf("the accepted segment must cover only the dialogue block, got %v", seg.SpeechBlockIndices)
		}
	}

	mix := issue157ReadMix(t, h, runID)
	if !mix.PreservationPlan.PreserveSinging {
		t.Fatal("the rebuilt mix must preserve singing")
	}
	if !issue157HasWindow(mix.PreservationPlan.SingingWindows, 1150, 2150) {
		t.Fatalf("the singing window must stay preserved untouched, got %+v", mix.PreservationPlan.SingingWindows)
	}
	if !issue157HasWindow(mix.PreservationPlan.SpeechWindows, 0, 1000) {
		t.Fatalf("suppression must follow the accepted dialogue block, got %+v", mix.PreservationPlan.SpeechWindows)
	}
	for _, w := range mix.PreservationPlan.SpeechWindows {
		if w.StartMs < 2150 && w.EndMs > 1150 {
			t.Fatalf("the singing window must never be suppressed as dialogue, got %+v", w)
		}
	}
}

// TestSeam1_Issue157_InjectedCASFailureLeavesNoApprovalAndRetryCompletes proves an injected CAS
// failure at the acceptance's own commit point cannot leave an approval that releases media: no
// successor, no audit row and no mix exist, and the operator's retry of the same exact selection
// completes the acceptance once the store works again.
func TestSeam1_Issue157_InjectedCASFailureLeavesNoApprovalAndRetryCompletes(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()
	_, _, runID, items := issue157ParkedFixture(t, h, true)
	base := issue157RunVariant(t, h, runID)

	// The CAS staging directory is what every write needs: withdrawing it fails the acceptance's own
	// commit without touching a single object a reader resolves.
	staging := filepath.Join(h.dir, "cas", "tmp")
	if err := os.Rename(staging, staging+".away"); err != nil {
		t.Fatalf("withdraw the CAS staging directory: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_ = os.Rename(staging+".away", staging)
			restored = true
		}
	}
	t.Cleanup(restore)

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status == http.StatusOK || result != nil {
		t.Fatalf("expected the injected CAS failure to surface, got %d (%s)", status, refusal)
	}
	restore()

	if now := issue157RunVariant(t, h, runID); now.CASHash != base.CASHash {
		t.Fatalf("a failed acceptance must not move the run's dubbing artifact: %s -> %s", base.CASHash, now.CASHash)
	}
	if got := issue157OverrideCount(t, h, runID); got != 0 {
		t.Fatalf("a failed acceptance must not write audit rows, got %d", got)
	}
	if _, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a failed acceptance must not publish a mix, got %v", err)
	}
	if pending := issue157ReviewItems(t, h, runID); len(pending) != 2 {
		t.Fatalf("a failed acceptance must leave the review queue untouched, got %+v", pending)
	}

	// The retry of the same exact selection completes it against the artifact it was derived from.
	retryStatus, retried, retryRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if retryStatus != http.StatusOK || retried == nil {
		t.Fatalf("expected the retry to complete the acceptance, got %d (%s)", retryStatus, retryRefusal)
	}
	if retried.CoverageComplete || !retried.RunPaused || retried.RemainingReviewCount != 1 {
		t.Fatalf("expected the retry to accept one of two units and stay paused, got %+v", retried)
	}
	after := issue157RunVariant(t, h, runID)
	if after.CASHash != retried.DubSegmentsVariantCAS || len(after.AcceptedCandidates) != 1 {
		t.Fatalf("expected the retry's successor to carry one acceptance, got cas=%s evidence=%d",
			after.CASHash, len(after.AcceptedCandidates))
	}
	if got := issue157OverrideCount(t, h, runID); got != 1 {
		t.Fatalf("the retry must record exactly one audit row, got %d", got)
	}
}
