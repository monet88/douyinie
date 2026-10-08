package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Xin chao ban"},
		{speaker: "SPEAKER_01", startMs: 1150, endMs: 2150, source: "再见", spoken: "Tam biet ban"},
	}
	roles := []domain.AudioSegment{
		{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 1150, EndMs: 2150, Role: domain.AudioRoleNarrationDialogue},
	}
	jobID, runID = createJobAndRunWithDuration(t, h, 6.0)
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

// issue157QualityResultCount counts the run's recorded QA results.
func issue157QualityResultCount(t *testing.T, h *testHarness, runID string) int {
	t.Helper()
	rows, err := h.db.GetQualityResultsByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read quality results for run %s: %v", runID, err)
	}
	return len(rows)
}

// issue157ProviderAttempts counts the run's recorded provider attempts across every stage.
func issue157ProviderAttempts(t *testing.T, h *testHarness, runID string) int {
	t.Helper()
	total := 0
	for _, stage := range []string{"translation", "dub_synthesize", "text_region_detect", "render"} {
		rows, err := h.db.ListProviderAttempts(context.Background(), runID, stage)
		if err != nil {
			t.Fatalf("read provider attempts for run %s stage %s: %v", runID, stage, err)
		}
		total += len(rows)
	}
	return total
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

// TestSeam1_Issue157_ReviewPostureKeepsExplicitFinalRenderStart proves a complete acceptance under
// Review posture rebuilds the delivery and stops at the operator's explicit final render: no final
// render is executed or pinned, and the result reports the run as still paused.
func TestSeam1_Issue157_ReviewPostureKeepsExplicitFinalRenderStart(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host to rebuild a preview render")
	}
	h := setupHarness(t)
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "你好", spoken: "Xin chao ban"},
	}
	roles := []domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}}

	jobID, _ := createJobAndRunWithDuration(t, h, 4.0)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID
	runID := issue157CreateRunInPosture(t, h, jobID, domain.ReviewPostureReview)

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

	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected one pending review unit, got %+v", items)
	}
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
