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

// TestSeam1_Issue157_SelectionRefusesNonOwnedDubbingMedia proves a run whose dubbing claim is bound
// to another asset or language is refused as a classified 403, the same class the sibling review
// routes answer, instead of surfacing an internal fault: the candidate the operator is accepting is
// not this run's media, and no fault occurred.
func TestSeam1_Issue157_SelectionRefusesNonOwnedDubbingMedia(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()
	_, _, runID, items := issue157ParkedFixture(t, h, false)

	idx, err := h.db.GetDubSegmentsVariantIndexByRun(ctx, runID)
	if err != nil || idx == nil {
		t.Fatalf("read run dub variant index: %v", err)
	}
	// The run's dubbing claim now names another target language, which is the same foreign binding
	// loadRunDubbingVariant refuses for another asset or run.
	foreign := *idx
	foreign.TargetLanguage = "en"
	if err := h.db.UpsertDubSegmentsVariantIndex(ctx, foreign); err != nil {
		t.Fatalf("rebind the run's dub variant index: %v", err)
	}

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusForbidden || result != nil {
		t.Fatalf("expected the non-owned dubbing artifact to be refused with 403, got status=%d result=%+v refusal=%s",
			status, result, refusal)
	}
	if !strings.Contains(refusal, "not owned by this run") {
		t.Fatalf("expected the refusal to name the ownership fault, got %q", refusal)
	}
	if overrides := issue157OverrideCount(t, h, runID); overrides != 0 {
		t.Fatalf("a refused selection must not write audit rows, got %d", overrides)
	}
	if now := issue157RunVariant(t, h, runID); now == nil || now.CASHash != idx.CASHash {
		t.Fatalf("a refused selection must leave the run's dubbing artifact alone, got %+v", now)
	}
}

// TestSeam1_Issue157_SelectionRefusesMissingPinnedLineageArtifact proves the acceptance answers the
// documented class when a canonical artifact the run pins is absent from CAS: an owned hash with no
// object is a missing-artifact refusal - 404, the class the sibling dub-media resolver reports -
// rather than an internal fault, and nothing is recorded.
func TestSeam1_Issue157_SelectionRefusesMissingPinnedLineageArtifact(t *testing.T) {
	for _, absent := range []string{"transcript", "dubbing artifact", "audio role plan"} {
		t.Run(absent, func(t *testing.T) {
			h := setupHarness(t)
			ctx := context.Background()
			_, assetID, runID, items := issue157ParkedFixture(t, h, false)

			var casHash string
			switch absent {
			case "transcript":
				variant := issue157RunVariant(t, h, runID)
				if variant == nil || variant.TranscriptArtifactCAS == "" {
					t.Fatalf("expected the run's dubbing artifact to pin a transcript, got %+v", variant)
				}
				casHash = variant.TranscriptArtifactCAS
			case "dubbing artifact":
				variant := issue157RunVariant(t, h, runID)
				if variant == nil || variant.CASHash == "" {
					t.Fatalf("expected the run to hold a dubbing artifact, got %+v", variant)
				}
				casHash = variant.CASHash
			case "audio role plan":
				plan, err := service.ResolveRunScopedAudioRolePlan(ctx, h.db, h.casStore, assetID, runID)
				if err != nil || plan == nil || plan.CASHash == "" {
					t.Fatalf("read the run's audio role plan: %v (%+v)", err, plan)
				}
				casHash = plan.CASHash
			}
			path, err := h.casStore.ResolvePath(casHash)
			if err != nil {
				t.Fatalf("resolve the pinned %s %s: %v", absent, casHash, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove the pinned %s %s: %v", absent, casHash, err)
			}

			status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
			if status != http.StatusNotFound || result != nil {
				t.Fatalf("expected the missing pinned %s to be refused with 404, got status=%d result=%+v refusal=%s",
					absent, status, result, refusal)
			}
			if !strings.Contains(refusal, "not found in store") {
				t.Fatalf("expected the refusal to name the absence, got %q", refusal)
			}
			if overrides := issue157OverrideCount(t, h, runID); overrides != 0 {
				t.Fatalf("a refused selection must not write audit rows, got %d", overrides)
			}
		})
	}
}

// TestSeam1_Issue157_CompoundedRebuildFailureKeepsItsClass proves the refusal class survives a
// compounded failure: when the mixer refuses the accepted waveform and the compensating stage write
// that follows it fails too, the operator still sees the documented 422 instead of an opaque fault.
func TestSeam1_Issue157_CompoundedRebuildFailureKeepsItsClass(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()
	_, _, runID, items := issue157ParkedFixtureWithMedia(t, h, 1.2, false)

	status, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if status != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first group to be accepted without a rebuild, got %d (%s)", status, firstRefusal)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 {
		t.Fatalf("expected one group still pending, got %+v", pending)
	}

	// The rebuild refuses at the mixer - the pinned media is shorter than the accepted window - and
	// the compensating invalidation that follows it cannot be recorded either.
	rawDB := issue157RawDB(t, h)
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_compensating_invalidation BEFORE INSERT ON stage_executions
		WHEN NEW.stage = 'audio_mix'
		BEGIN
			SELECT RAISE(ABORT, 'compensating stage write failed');
		END;`); err != nil {
		t.Fatalf("create failing stage trigger: %v", err)
	}

	status, result, refusal := issue157Accept(t, h, runID, issue157Selection(pending[0].ID))
	if status != http.StatusUnprocessableEntity || result != nil {
		t.Fatalf("expected the mixer's gate to keep its 422 class under a compounded failure, got %d (%s)", status, refusal)
	}
	if !strings.Contains(refusal, "audio mixer refused") {
		t.Fatalf("expected the refusal to name the mixer's gate, got %q", refusal)
	}
}

// TestSeam1_Issue157_AcceptedCandidateKeepsItsExactSelectionAction proves the audit row an
// acceptance writes stays the exact reviewed-candidate selection: a transformed waveform's quality
// waiver is recorded on that row - the explicit manual_override the request carries and the waiver
// line the decision note keeps - but the row itself is never the generic audit-only manual_override
// the contract forbids standing in for an acceptance, and a replay answers from the recorded
// decision without appending or rewriting it.
func TestSeam1_Issue157_AcceptedCandidateKeepsItsExactSelectionAction(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	_, runID, item, _ := issue157PinnedDeliveryFixture(t, h, "issue157_action.mp4")

	status, accepted, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if status != http.StatusOK || accepted == nil {
		t.Fatalf("expected the exact reviewed candidate to be accepted, got %d (%s)", status, refusal)
	}
	if !accepted.Transformed || !accepted.QualityWaiver || !accepted.CoverageComplete {
		t.Fatalf("expected a complete transformed acceptance with an explicit waiver, got %+v", accepted)
	}
	if accepted.Status != domain.ReviewItemStatusManualOverride {
		t.Fatalf("a human acceptance resolves the unit as an operator override, not an automatic pass, got %+v", accepted)
	}
	rows, err := h.db.GetReviewOverridesByRun(ctx, runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read the run's audit rows: %v (%+v)", err, rows)
	}
	if rows[0].ID != accepted.ReviewOverrideID || rows[0].Action != string(domain.ReviewOverrideActionReviewedCandidate) {
		t.Fatalf("the acceptance audits as the exact reviewed-candidate selection, got %+v", rows[0])
	}
	if !strings.Contains(rows[0].Reason, "manual_override") || !strings.Contains(rows[0].Reason, accepted.SelectedAudioSHA256) {
		t.Fatalf("the waiver and the selected candidate belong on the decision note, got %+v", rows[0])
	}

	// A repeated identical selection answers from the recorded decision and rewrites nothing.
	status, replay, replayRefusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if status != http.StatusOK || replay == nil || !replay.Idempotent {
		t.Fatalf("expected the replay to answer from the recorded decision, got %d (%s)", status, replayRefusal)
	}
	if replay.ReviewOverrideID != accepted.ReviewOverrideID || replay.Status != domain.ReviewItemStatusManualOverride {
		t.Fatalf("expected the replay to report the recorded decision as an operator override, got %+v", replay)
	}
	again, err := h.db.GetReviewOverridesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("re-read the run's audit rows: %v", err)
	}
	if len(again) != 1 || again[0].Action != string(domain.ReviewOverrideActionReviewedCandidate) {
		t.Fatalf("the replay must not append or rewrite the decision, got %+v", again)
	}
}

// TestSeam1_Issue157_RepeatedSelectionWithoutItsWaiverIsRefused proves a repeat of an accepted
// transformed decision is that decision only when it carries the same explicit override: the waiver is
// what authorizes releasing a transformed waveform, so a repeat that drops it is a differently
// parameterized request, and it must fail closed instead of replaying the recorded decision.
func TestSeam1_Issue157_RepeatedSelectionWithoutItsWaiverIsRefused(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	_, runID, item, _ := issue157PinnedDeliveryFixture(t, h, "issue157_replay_waiver.mp4")

	status, accepted, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if status != http.StatusOK || accepted == nil || !accepted.Transformed {
		t.Fatalf("expected the transformed candidate to be accepted, got %d (%s)", status, refusal)
	}
	claimed := issue157RunVariant(t, h, runID)

	// The same item asked for again without the explicit override that authorizes a transformed waveform
	// is not the decision on record, so it is refused and nothing moves.
	status, replay, replayRefusal := issue157Accept(t, h, runID,
		withSelectionValue(issue157Selection(item.ID), "manual_override", false))
	if status != http.StatusConflict {
		t.Fatalf("a repeat without its waiver must fail closed, got %d (%s)", status, replayRefusal)
	}
	if replay != nil {
		t.Fatalf("a refused repeat must report no result, got %+v", replay)
	}
	if got := issue157RunVariant(t, h, runID); got.CASHash != claimed.CASHash {
		t.Fatalf("a refused repeat must not claim another decision, got %s want %s", got.CASHash, claimed.CASHash)
	}
	rows, err := h.db.GetReviewOverridesByRun(ctx, runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("a refused repeat must not append an audit row, got %v (%+v)", err, rows)
	}
}

// TestSeam1_Issue157_RepairedAuditRowCreditsTheDecisionNotTheRetry proves a retried acceptance that
// repairs the audit row the first attempt could not write credits the operator and the note the
// decision was recorded with: the claimed successor carries the decider, so a retry by someone else
// must not transfer the approval - or its audit history - to them. An approval taken without a named
// operator is recorded as the default, and a repair by someone else keeps that default.
func TestSeam1_Issue157_RepairedAuditRowCreditsTheDecisionNotTheRetry(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	for _, tc := range []struct {
		name         string
		operator     string
		wantOperator string
	}{
		{name: "a named operator", operator: "qa_lead", wantOperator: "qa_lead"},
		{name: "no named operator", wantOperator: "operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := setupAutoRunHarness(t)
			configureDialogueTranslationGateway(t, h)
			_, runID, item, _ := issue157PinnedDeliveryFixture(t, h, "issue157_audit_credit.mp4")

			rawDB := issue157RawDB(t, h)
			if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_audit_write BEFORE INSERT ON review_overrides
		BEGIN
			SELECT RAISE(ABORT, 'audit write failed');
		END;`); err != nil {
				t.Fatalf("create failing audit trigger: %v", err)
			}
			decider := issue157Selection(item.ID)
			if tc.operator != "" {
				decider["operator"] = tc.operator
			} else {
				delete(decider, "operator")
			}
			decider["reason"] = "the decider's own note"
			if status, _, refusal := issue157Accept(t, h, runID, decider); status == http.StatusOK {
				t.Fatalf("expected the failed audit write to surface, got %d (%s)", status, refusal)
			}
			if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_audit_write`); err != nil {
				t.Fatalf("drop failing audit trigger: %v", err)
			}
			claimed := issue157RunVariant(t, h, runID)
			if len(claimed.AcceptedCandidates) != 1 {
				t.Fatalf("expected the decision to be claimed before its audit row failed, got %+v", claimed.AcceptedCandidates)
			}
			// The claim is the version that travels in the bundle, so it carries the decider itself: a
			// row can be repaired, an exported lineage is immutable.
			if got := claimed.AcceptedCandidates[0].Operator; got != tc.wantOperator {
				t.Fatalf("the recorded decision must carry the decider %q, got %q", tc.wantOperator, got)
			}

			// Someone else retries the same decision. The row the retry repairs is the decision's own.
			retry := issue157Selection(item.ID)
			retry["operator"] = "another_operator"
			retry["reason"] = "someone else's note"
			if status, _, refusal := issue157Accept(t, h, runID, retry); status != http.StatusOK {
				t.Fatalf("expected the retry to repair the audit row, got %d (%s)", status, refusal)
			}
			rows, err := h.db.GetReviewOverridesByRun(ctx, runID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("read the repaired audit row: %v (%+v)", err, rows)
			}
			if rows[0].Operator != tc.wantOperator || !strings.Contains(rows[0].Reason, "the decider's own note") {
				t.Fatalf("the repaired row must credit the decision, got operator=%q reason=%q", rows[0].Operator, rows[0].Reason)
			}
			if strings.Contains(rows[0].Reason, "someone else's note") {
				t.Fatalf("the repaired row must not carry the retry's note, got %q", rows[0].Reason)
			}
		})
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

// issue157PinLegacyTranslation pins the run's translation index to a rewritten copy of the translation the
// run's dub script was minted from - the state a run carries when it was parked across a translation
// contract or schema bump: every lineage hash still agrees, and only the contract the artifacts were
// produced under moved on.
func issue157PinLegacyTranslation(t *testing.T, h *testHarness, runID string, mutate func(*domain.TranslationVariant)) {
	t.Helper()
	ctx := context.Background()

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

	transRC, err := h.casStore.Get(script.TranslationVariantCAS)
	if err != nil {
		t.Fatalf("read run translation variant from CAS: %v", err)
	}
	var translation domain.TranslationVariant
	if err := json.NewDecoder(transRC).Decode(&translation); err != nil {
		transRC.Close()
		t.Fatalf("decode run translation variant: %v", err)
	}
	transRC.Close()

	mutate(&translation)
	translation.CASHash = ""
	translationObj, err := h.casStore.Put(bytes.NewReader(mustJSON(t, translation)))
	if err != nil {
		t.Fatalf("put legacy translation variant: %v", err)
	}
	if err := h.db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             translation.ID,
		AssetID:        translation.AssetID,
		RunID:          runID,
		JobID:          translation.JobID,
		TargetLanguage: translation.TargetLanguage,
		CASHash:        translationObj.SHA256,
		ProvenanceHash: translation.ProvenanceHash,
		ProviderID:     translation.ProviderID,
		ModelVersion:   translation.ModelVersion,
		CreatedAt:      translation.CreatedAt,
	}); err != nil {
		t.Fatalf("pin legacy translation index: %v", err)
	}
}

// TestSeam1_Issue157_SelectionRefusesSupersededTranslationContract pins AC1's translation contract check:
// a run parked across a translation-contract or schema bump is internally consistent, so every lineage
// hash still agrees and no different artifact is being published - only the contract the artifacts were
// produced under moved on. The acceptance must read that contract itself, because releasing such a run
// stamps as reviewed exactly the media the run's own resume path refuses to reuse.
func TestSeam1_Issue157_SelectionRefusesSupersededTranslationContract(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.TranslationVariant)
	}{
		{
			name:   "contract superseded",
			mutate: func(v *domain.TranslationVariant) { v.ContractID = "legacy-translation-contract-v0" },
		},
		{
			name:   "schema superseded",
			mutate: func(v *domain.TranslationVariant) { v.SchemaVersion = domain.TranslationSchemaVersion - 1 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHarness(t)
			_, _, runID, items := issue157ParkedFixture(t, h, false)

			issue157PinLegacyTranslation(t, h, runID, tc.mutate)

			status, result, refusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
			if status != http.StatusConflict || result != nil {
				t.Fatalf("expected the superseded translation contract to refuse the selection with 409, got status=%d result=%+v refusal=%s", status, result, refusal)
			}
			if !strings.Contains(refusal, "superseded translation contract") {
				t.Fatalf("expected the refusal to name the superseded translation contract, got %q", refusal)
			}
			if overrides := issue157OverrideCount(t, h, runID); overrides != 0 {
				t.Fatalf("a refused selection must not write audit rows, got %d", overrides)
			}
			if mix := issue157StageArtifact(t, h, runID, "audio_mix"); mix != "" {
				t.Fatalf("a refused selection must not mix: audio_mix artifact %s", mix)
			}
		})
	}
}

// TestSeam1_Issue157_AcceptanceNoteIsScrubbedBeforeItShips proves the operator's free text enters the
// system scrubbed: the note is persisted in the acceptance evidence, and a bundle ships that artifact
// verbatim, so a note carrying a host path or a credential must not leave the host inside it.
func TestSeam1_Issue157_AcceptanceNoteIsScrubbedBeforeItShips(t *testing.T) {
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixture(t, h, false)
	ctx := context.Background()

	note := `accepted C:\Users\operator\private\session.log with api_key=sk-abcdefghijklmnopqrstuvwx`
	payload := issue157Selection(items[0].ID)
	payload["reason"] = note
	status, result, refusal := issue157Accept(t, h, runID, payload)
	if status != http.StatusOK || result == nil {
		t.Fatalf("expected the acceptance to succeed, got %d (%s)", status, refusal)
	}

	successor := issue157RunVariant(t, h, runID)
	if len(successor.AcceptedCandidates) != 1 {
		t.Fatalf("expected one acceptance evidence record, got %+v", successor.AcceptedCandidates)
	}
	evidence := successor.AcceptedCandidates[0]
	if strings.Contains(evidence.Reason, `C:\Users`) || strings.Contains(evidence.Reason, "sk-abcdefghij") {
		t.Fatalf("the shipped acceptance evidence must not carry the raw note, got %q", evidence.Reason)
	}
	if !strings.Contains(evidence.Reason, "[REDACTED") {
		t.Fatalf("expected the note to be redacted in place, got %q", evidence.Reason)
	}

	// The audit row a replayed bundle restates carries the same scrubbed text, never the raw note.
	rows, err := h.db.GetReviewOverridesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("read review overrides: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one acceptance audit row, got %d", len(rows))
	}
	if strings.Contains(rows[0].Reason, `C:\Users`) || strings.Contains(rows[0].Reason, "sk-abcdefghij") {
		t.Fatalf("the acceptance audit row must not keep the raw note, got %q", rows[0].Reason)
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
	rawDB := issue157RawDB(t, h)
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
	rawDB := issue157RawDB(t, h)
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
	rawDB := issue157RawDB(t, h)
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
	rawDB := issue157RawDB(t, h)
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
// The run's pipeline published its delivery, and then the operator's correction re-synthesized its
// dubbing: the rows still name the mix, plan and preview of the artifact the accepted candidate
// replaced. A resume that trusted them would serve media the operator never selected: it must
// withdraw them and rebuild the delivery from the accepted successor, the artifact the run's own
// index row claims.
func TestSeam1_Issue157_ResumeWithdrawsADeliveryPinnedToTheReplacedCandidate(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	ctx := context.Background()

	// The delivery the resume withdraws is the one the run's own pipeline published - a real mix, the
	// plan that pinned it and the preview it advertised - not a fixture-written stand-in.
	assetID, runID, item, delivery := issue157PinnedDeliveryFixture(t, h, "issue157_supersede.mp4")
	replacedMix := delivery["audio_mix"]
	replacedPlan := delivery["render_plan"]
	replacedPreview := delivery["render_preview"]
	replaced := issue157RunVariant(t, h, runID)

	// The acceptance claims its successor and writes the audit row, then fails recording the run's
	// dub_synthesize stage artifact: the interrupted state a resume has to converge.
	rawDB := issue157RawDB(t, h)
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_dubbing_pin BEFORE INSERT ON stage_executions
		WHEN NEW.stage = 'dub_synthesize' AND NEW.status = 'succeeded'
		BEGIN
			SELECT RAISE(ABORT, 'dubbing stage write failed');
		END;`); err != nil {
		t.Fatalf("create failing stage trigger: %v", err)
	}
	status, _, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
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
	// The decision the acceptance recorded is the one the resume released: repairing it is idempotent.
	if got := issue157OverrideCount(t, h2, runID); got != 1 {
		t.Fatalf("the resume must not append audit rows, got %d", got)
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
	rawDB := issue157RawDB(t, h)
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

// issue157RawDB opens the harness database for the tests that have to inject a fault at the storage
// boundary, so the rollback a stage or audit write performs is the one a real SQLite fault performs.
func issue157RawDB(t *testing.T, h *testHarness) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "douyinie_test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// issue157NewestStageStatus reads the status of the newest stage row the run resolves a stage
// through, breaking ties exactly as the storage layer does (created_at DESC, id DESC).
func issue157NewestStageStatus(t *testing.T, rawDB *sql.DB, runID, stage string) string {
	t.Helper()
	var status string
	if err := rawDB.QueryRowContext(context.Background(),
		`SELECT status FROM stage_executions WHERE run_id = ? AND stage = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		runID, stage).Scan(&status); err != nil {
		t.Fatalf("read newest %s stage status: %v", stage, err)
	}
	return status
}

// issue157PinnedDeliveryFixture parks a run on one unresolved unit while the run still serves the
// delivery the runtime published for the dubbing artifact it held before: a real mix of that artifact,
// the plan frozen from that mix and the preview and final rendered from that plan. Every part of it is
// published through the seams the operator and the pipeline use - the correction, the render plan
// freeze and the render endpoints - and the fixture closes by proving the run actually serves it,
// because withdrawing a delivery that was never deliverable proves nothing about withdrawal.
func issue157PinnedDeliveryFixture(t *testing.T, h *testHarness, mediaName string) (assetID, runID string, item domain.ReviewItem, delivery map[string]string) {
	t.Helper()
	lane := defaultVITTSFake(t, h)
	// The run's own pass overruns its immutable slot by a factor no attempt may auto-approve but the
	// review-only transform band still governs, so it parks on that unit with an alternative the
	// operator can actually select, having pinned every earlier stage itself.
	lane.DurationMs = 330

	assetID = ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, mediaName, 4.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID = enqueueRun(t, h.server.URL, jobID)
	if parked := pollRunStatus(t, h.server.URL, runID, domain.RunStatusPaused, 60*time.Second); parked.Status != domain.RunStatusPaused {
		t.Fatalf("expected the run's own pass to park on its overrun, got %q", parked.Status)
	}

	// A candidate that fits its immutable slot is auto-accepted, so the operator's correction publishes
	// the artifact the run's delivery is built from: the correction mints the translation and the dub
	// script together, so the corrected lineage keeps the meaning-preservation invariant the run
	// re-checks, and it records the mix, plan and preview pins for that artifact itself.
	lane.DurationMs = 150
	deliveredCorrection, err := issue157CorrectText(t, h, assetID, runID, jobID, 0, deliveredTurnText)
	if err != nil {
		t.Fatalf("correct the dialogue turn into a fitting candidate: %v", err)
	}
	if deliveredCorrection.DubSegmentsVariantCAS == "" || deliveredCorrection.DubMixCAS == "" {
		t.Fatalf("expected the correction to publish a dubbing artifact and its mix, got %+v", deliveredCorrection)
	}
	// The delivery is the mix the correction reran, and the plan the runtime freezes from it: a plan
	// written by hand is not a delivery - the renderer refuses a plan that does not resolve the mix's
	// audio - so the plan pin below is the plan the render plan endpoint froze, and the preview and
	// final pins are the artifacts the render endpoints published from it.
	deliveredMix := deliveredCorrection.DubMixCAS
	issue157AssertDeliverableMix(t, h, deliveredMix, deliveredCorrection.DubSegmentsVariantCAS)
	deliveredPlan := issue157FreezeDeliveryPlan(t, h, assetID, runID, jobID, deliveredMix)
	deliveredPreview := issue157PublishDeliveryRender(t, h, assetID, runID, jobID, "preview", deliveredPlan)
	issue157PublishDeliveryRender(t, h, assetID, runID, jobID, "final", deliveredPlan)
	issue157PinStage(t, h, runID, "audio_mix", deliveredMix)
	issue157PinStage(t, h, runID, "render_plan", deliveredPlan)
	issue157PinStage(t, h, runID, "render_preview", deliveredPreview)

	// The operator corrects the turn again into a candidate that overruns its immutable slot by a factor
	// the review-only transform band still governs: the run holds that unresolved unit while the
	// delivery it serves was published for the artifact the correction replaced.
	lane.DurationMs = 330
	if _, err := issue157CorrectText(t, h, assetID, runID, jobID, 0, parkedTurnText); err == nil {
		if got := issue157StageArtifact(t, h, runID, "audio_mix"); strings.EqualFold(got, deliveredMix) {
			t.Fatalf("the overrunning correction must not leave the replaced delivery pinned, got %s", got)
		}
	}

	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected the correction to hold one unresolved unit, got %+v", items)
	}
	if got := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(got, deliveredMix) {
		t.Fatalf("the published delivery must stay pinned, got %s want %s", got, deliveredMix)
	}
	// The delivery is deliverable in the state the tests drive: the run serves both render kinds it
	// published, from the plan the fixture froze, with the media behind them in the store.
	issue157AssertDeliveryServed(t, h, assetID, runID, deliveredPlan)
	return assetID, runID, items[0], map[string]string{
		"audio_mix":      deliveredMix,
		"render_plan":    deliveredPlan,
		"render_preview": deliveredPreview,
	}
}

// issue157AssertDeliverableMix proves a delivery's mix is media a render can be built from: a PASS mix
// of the successor it names, whose audio the store holds.
func issue157AssertDeliverableMix(t *testing.T, h *testHarness, mixCAS, successorCAS string) {
	t.Helper()
	rc, err := h.casStore.Get(mixCAS)
	if err != nil {
		t.Fatalf("read the delivery mix %s from cas: %v", mixCAS, err)
	}
	defer rc.Close()
	var mix domain.DubMixArtifact
	if err := json.NewDecoder(rc).Decode(&mix); err != nil {
		t.Fatalf("decode the delivery mix %s: %v", mixCAS, err)
	}
	if mix.OverallStatus != "PASS" {
		t.Fatalf("the delivery mix must be a PASS mix, got %q (%s)", mix.OverallStatus, mix.RefusalReason)
	}
	if !strings.EqualFold(mix.DubSegmentsCAS, successorCAS) {
		t.Fatalf("the delivery mix must be mixed from %s, got %s", successorCAS, mix.DubSegmentsCAS)
	}
	if mix.AudioCASHash == "" || !h.casStore.Exists(mix.AudioCASHash) {
		t.Fatalf("the delivery mix's audio must be in the store, got %q", mix.AudioCASHash)
	}
}

// issue157FreezeDeliveryPlan freezes a delivery's render plan through the render plan endpoint: the plan
// a delivery is rendered from is the runtime's, not a fixture's stand-in.
func issue157FreezeDeliveryPlan(t *testing.T, h *testHarness, assetID, runID, jobID, mixCAS string) string {
	t.Helper()
	resp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"dub_mix_cas":     mixCAS,
	})
	if resp.StatusCode != http.StatusCreated || plan == nil || plan.CASHash == "" {
		t.Fatalf("freeze the delivery's render plan: status=%d plan=%+v", resp.StatusCode, plan)
	}
	if plan.SourceAssetSHA256 == "" {
		t.Fatalf("the delivery's plan must resolve the source asset, got %+v", plan)
	}
	return plan.CASHash
}

// issue157PublishDeliveryRender renders one kind of a delivery through the render endpoint and returns
// the artifact CAS it published, having checked the artifact is PASS media the store holds.
func issue157PublishDeliveryRender(t *testing.T, h *testHarness, assetID, runID, jobID, kind, planCAS string) string {
	t.Helper()
	payload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"plan_cas":        planCAS,
	}
	status, artifactCAS, outputCAS, overallStatus := 0, "", "", ""
	switch kind {
	case "preview":
		resp, art := runRenderPreview(t, h, assetID, payload)
		status = resp.StatusCode
		if art != nil {
			artifactCAS, outputCAS, overallStatus = art.CASHash, art.OutputCASHash, art.OverallStatus
		}
	case "final":
		resp, art := runRenderFinal(t, h, assetID, payload)
		status = resp.StatusCode
		if art != nil {
			artifactCAS, outputCAS, overallStatus = art.CASHash, art.OutputCASHash, art.OverallStatus
		}
	default:
		t.Fatalf("unknown delivery render kind %q", kind)
	}
	if status != http.StatusCreated || artifactCAS == "" {
		t.Fatalf("render the delivery's %s: status=%d artifact=%q", kind, status, artifactCAS)
	}
	if overallStatus != "PASS" || outputCAS == "" || !h.casStore.Exists(outputCAS) {
		t.Fatalf("the delivery's %s must be PASS media in the store, got status=%q output=%q", kind, overallStatus, outputCAS)
	}
	return artifactCAS
}

// issue157AssertDeliveryServed proves a run serves the delivery it published, from the plan it was
// rendered against: both render kinds resolve over the API with media the store holds.
func issue157AssertDeliveryServed(t *testing.T, h *testHarness, assetID, runID, planCAS string) {
	t.Helper()
	for _, probe := range []struct {
		name string
		path string
		key  string
	}{
		{"preview", fmt.Sprintf("/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", assetID, runID), "preview_render"},
		{"final", fmt.Sprintf("/api/v1/assets/%s/render/final?run_id=%s&target_language=vi", assetID, runID), "final_render"},
	} {
		consumedPlan, output := issue157RenderLineage(t, h, probe.path, probe.key)
		if output == "" || !h.casStore.Exists(output) {
			t.Fatalf("the delivery's %s must have media in the store, got %q", probe.name, output)
		}
		if !strings.EqualFold(consumedPlan, planCAS) {
			t.Fatalf("the delivery's %s must be rendered from its frozen plan %s, got %s", probe.name, planCAS, consumedPlan)
		}
	}
}

// The two turns the fixture's corrections publish. Both keep the source meaning, so the run's meaning
// gate accepts them, and they differ from each other, so each corrected lineage is one the run did not
// already hold.
const (
	deliveredTurnText = "Kiểm tra đầu vào giọng nói của bạn"
	parkedTurnText    = "Kiểm tra đầu vào giọng nói của bạn nhé"
)

// issue157CorrectText rewrites one dialogue turn through the inspector, the way the operator's
// correction does: the translation and the dub script move together, so the corrected lineage keeps the
// meaning-preservation invariant the run re-checks, and the correction records the stage pins for every
// artifact it published. Its rerun refusal is returned to the caller, which is the state a correction
// whose overrunning candidate its own mix rerun refuses leaves behind.
func issue157CorrectText(t *testing.T, h *testHarness, assetID, runID, jobID string, segmentIndex int, newTargetText string) (*service.TargetTextCorrectionResult, error) {
	t.Helper()
	payload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"segment_index":   segmentIndex,
		"new_target_text": newTargetText,
		"reason":          "operator rewrote the dialogue turn",
		"operator":        "qa_lead",
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/inspector/correct-text", h.server.URL, assetID),
		"application/json", bytes.NewReader(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("POST inspector/correct-text: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("correct-text status=%d body=%s", resp.StatusCode, string(body))
	}
	var body struct {
		Result service.TargetTextCorrectionResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the correction result: %v", err)
	}
	return &body.Result, nil
}

// issue157Resume restarts the host the way a production host comes back, resumes the run and returns
// the restarted harness.
func issue157Resume(t *testing.T, h *testHarness, runID string) *testHarness {
	t.Helper()
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
	return h2
}

// issue157WaitSettled waits for a resumed run to stop moving and returns the state it reached.
func issue157WaitSettled(t *testing.T, h *testHarness, runID string) domain.LocalizationRun {
	t.Helper()
	settled := issue157Run(t, h, runID)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		if settled.Status != domain.RunStatusQueued && settled.Status != domain.RunStatusRunning {
			return settled
		}
		time.Sleep(50 * time.Millisecond)
		settled = issue157Run(t, h, runID)
	}
	return settled
}

// TestSeam1_Issue157_ResumeRepairsTheAcceptedAuditRecord proves a claimed acceptance never delivers
// its successor without the operator's decision on record. An acceptance claims the successor before
// it writes the audit row, so a failure in between leaves the run holding media whose evidence links
// to a row that does not exist; the resume repairs the row from that evidence, and a resume whose
// repair cannot be written fails the run instead of releasing undecided media.
func TestSeam1_Issue157_ResumeRepairsTheAcceptedAuditRecord(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	failAuditWrites := `CREATE TRIGGER fail_audit_write BEFORE INSERT ON review_overrides
		BEGIN
			SELECT RAISE(ABORT, 'audit write failed');
		END;`

	t.Run("the decision is recorded before the media is delivered", func(t *testing.T) {
		h := setupAutoRunHarness(t)
		configureDialogueTranslationGateway(t, h)
		_, runID, item, delivery := issue157PinnedDeliveryFixture(t, h, "issue157_audit_repair.mp4")
		rawDB := issue157RawDB(t, h)
		if _, err := rawDB.ExecContext(ctx, failAuditWrites); err != nil {
			t.Fatalf("create failing audit trigger: %v", err)
		}
		status, _, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
		if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_audit_write`); err != nil {
			t.Fatalf("drop failing audit trigger: %v", err)
		}
		if status != http.StatusInternalServerError || !strings.Contains(refusal, "reviewed candidate acceptance failed") {
			t.Fatalf("expected the injected audit failure to surface as the fixed internal fault, got %d: %s", status, refusal)
		}

		// The claim is committed and the decision is not: the interrupted state a resume has to repair.
		claimed := issue157RunVariant(t, h, runID)
		if claimed == nil || claimed.OverallStatus != "PASS" || len(claimed.AcceptedCandidates) != 1 {
			t.Fatalf("expected the acceptance to claim a complete successor, got %+v", claimed)
		}
		if got := issue157OverrideCount(t, h, runID); got != 0 {
			t.Fatalf("the failed audit write must leave no row, got %d", got)
		}
		if got := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(got, delivery["audio_mix"]) {
			t.Fatalf("the interrupted acceptance must not have released media, got mix %s", got)
		}

		// The resume delivers the accepted successor: it records the decision it is about to release.
		h2 := issue157Resume(t, h, runID)
		if settled := issue157WaitSettled(t, h2, runID); settled.Status != domain.RunStatusCompleted {
			t.Fatalf("expected the resumed run to complete, got %q with items %+v",
				settled.Status, issue157ReviewItems(t, h2, runID))
		}
		if got := issue157OverrideCount(t, h2, runID); got != 1 {
			t.Fatalf("the resume must record exactly the acceptance's decision, got %d rows", got)
		}
		rows, err := h2.db.GetReviewOverridesByRun(ctx, runID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("read the run's audit rows: %v (%+v)", err, rows)
		}
		if rows[0].ID != claimed.AcceptedCandidates[0].ReviewOverrideID {
			t.Fatalf("the repaired row must be the decision the successor carries: %s != %s",
				rows[0].ID, claimed.AcceptedCandidates[0].ReviewOverrideID)
		}
		if rows[0].Action != string(domain.ReviewOverrideActionReviewedCandidate) {
			t.Fatalf("the repaired row must record the exact reviewed-candidate selection, got %+v", rows[0])
		}
		if rows[0].ReviewItemID != item.ID {
			t.Fatalf("the repaired row must name the reviewed item %s, got %+v", item.ID, rows[0])
		}
		// The repair restates the operator's own decision, not a default actor with no reason: the
		// evidence the claimed successor carries is where the interrupted acceptance recorded it.
		evidence := claimed.AcceptedCandidates[0]
		if evidence.Operator == "" || evidence.Reason == "" {
			t.Fatalf("the claimed acceptance must carry the operator's decision, got %+v", evidence)
		}
		if rows[0].Operator != evidence.Operator || !strings.Contains(rows[0].Reason, evidence.Reason) {
			t.Fatalf("the repaired row must restate the decision %q/%q, got %+v",
				evidence.Operator, evidence.Reason, rows[0])
		}
		if mix := issue157ReadMix(t, h2, runID); !strings.EqualFold(mix.DubSegmentsCAS, claimed.CASHash) {
			t.Fatalf("the delivered media must be mixed from the accepted successor %s, got %s",
				claimed.CASHash, mix.DubSegmentsCAS)
		}
	})

	t.Run("a decision that cannot be recorded releases no media", func(t *testing.T) {
		h := setupAutoRunHarness(t)
		configureDialogueTranslationGateway(t, h)
		_, runID, item, delivery := issue157PinnedDeliveryFixture(t, h, "issue157_audit_blocked.mp4")
		rawDB := issue157RawDB(t, h)
		if _, err := rawDB.ExecContext(ctx, failAuditWrites); err != nil {
			t.Fatalf("create failing audit trigger: %v", err)
		}
		// The audit write keeps failing, so the resume cannot record the decision it is about to deliver.
		if status, _, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID)); status == http.StatusOK {
			t.Fatalf("expected the audit write to fail, got %d (%s)", status, refusal)
		}
		h2 := issue157Resume(t, h, runID)
		if settled := issue157WaitSettled(t, h2, runID); settled.Status != domain.RunStatusInterrupted {
			t.Fatalf("expected the run to stop instead of delivering undecided media, got %q", settled.Status)
		}
		var stageErr string
		if err := rawDB.QueryRowContext(ctx,
			`SELECT COALESCE(error_message, '') FROM stage_executions WHERE run_id = ? AND status = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
			runID, domain.StageStatusFailed).Scan(&stageErr); err != nil {
			t.Fatalf("read the failed stage of the stopped run: %v", err)
		}
		if !strings.Contains(stageErr, "audit write failed") {
			t.Fatalf("expected the run to stop on the decision it could not record, got %q", stageErr)
		}
		if got := issue157OverrideCount(t, h2, runID); got != 0 {
			t.Fatalf("no decision could be recorded, got %d audit rows", got)
		}
		if got := issue157StageArtifact(t, h2, runID, "audio_mix"); !strings.EqualFold(got, delivery["audio_mix"]) {
			t.Fatalf("media must not be rebuilt from an undecided acceptance, got mix %s", got)
		}
	})
}

// TestSeam1_Issue157_ConcurrentCompleteAcceptancesRebuildOnce proves two identically complete
// acceptances that arrive together resolve to one decision and one delivery. The claim serializes the
// decision itself, but the rebuild that follows it is not idempotent on its own: a replay that found
// the acceptance before the first request had recorded its handoff would withdraw and re-render the
// delivery that request was still building.
func TestSeam1_Issue157_ConcurrentCompleteAcceptancesRebuildOnce(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	_, runID, item, _ := issue157PinnedDeliveryFixture(t, h, "issue157_concurrent_accept.mp4")

	// Every completion a delivery stage records from here on, so a second rebuild pass is observable.
	rawDB := issue157RawDB(t, h)
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE issue157_render_writes (stage TEXT, at TEXT)`); err != nil {
		t.Fatalf("create render-write ledger: %v", err)
	}
	for _, stage := range []string{"render_preview", "render_final", "final_render_handoff"} {
		for _, op := range []string{"INSERT", "UPDATE"} {
			stmt := fmt.Sprintf(`CREATE TRIGGER count_%s_%s AFTER %s ON stage_executions
				WHEN NEW.stage = '%s' AND NEW.status = 'succeeded'
				BEGIN
					INSERT INTO issue157_render_writes (stage, at) VALUES ('%s', datetime('now'));
				END;`, stage, strings.ToLower(op), op, stage, stage)
			if _, err := rawDB.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("create %s %s counter: %v", stage, op, err)
			}
		}
	}

	payload := issue157Selection(item.ID)
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
			results <- outcome{status: status, result: result, refusal: refusal, err: err}
		}()
	}
	wg.Wait()
	close(results)

	idempotent := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("POST accept-candidate: %v", got.err)
		}
		if got.status != http.StatusOK || got.result == nil {
			t.Fatalf("expected both concurrent acceptances to succeed, got %d (%s)", got.status, got.refusal)
		}
		if got.result.Idempotent {
			idempotent++
		}
	}
	if idempotent != 1 {
		t.Fatalf("expected exactly one of the two concurrent acceptances to report the decision the other published, got %d", idempotent)
	}

	// One decision, one audit row, and one rebuild pass: no delivery stage completed twice.
	if got := issue157OverrideCount(t, h, runID); got != 1 {
		t.Fatalf("two acceptances of one unit must record one decision, got %d rows", got)
	}
	variant := issue157RunVariant(t, h, runID)
	if variant == nil || variant.OverallStatus != "PASS" || len(variant.AcceptedCandidates) != 1 {
		t.Fatalf("expected one complete accepted successor, got %+v", variant)
	}
	if settled := issue157WaitSettled(t, h, runID); settled.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the accepted delivery to complete the run, got %q", settled.Status)
	}
	rows, err := rawDB.QueryContext(ctx, `SELECT stage, COUNT(*) FROM issue157_render_writes GROUP BY stage`)
	if err != nil {
		t.Fatalf("read render-write ledger: %v", err)
	}
	defer rows.Close()
	completed := 0
	for rows.Next() {
		var stage string
		var count int
		if err := rows.Scan(&stage, &count); err != nil {
			t.Fatalf("scan render-write ledger: %v", err)
		}
		if count > 1 {
			t.Fatalf("the delivery stage %s was completed %d times by one decision", stage, count)
		}
		completed += count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate render-write ledger: %v", err)
	}
	if completed == 0 {
		t.Fatalf("expected the acceptance to complete the run's delivery stages through this host")
	}
	if mix := issue157ReadMix(t, h, runID); !strings.EqualFold(mix.DubSegmentsCAS, variant.CASHash) {
		t.Fatalf("the delivered media must be mixed from the accepted successor %s, got %s",
			variant.CASHash, mix.DubSegmentsCAS)
	}
	if got := issue157StageArtifact(t, h, runID, "audio_mix"); got == "" {
		t.Fatalf("the run must publish a mix for its accepted delivery")
	}
}

// TestSeam1_Issue157_ReplayKeepsTheDeliveryWhenItsStoreCannotAnswer proves a replay whose delivery
// lookup fails reports the storage fault instead of deciding the run has no delivery. Withdrawing and
// rebuilding over media the run already serves is what that misreading did: the replay deleted the
// current preview and final index rows and re-rendered them for no reason.
func TestSeam1_Issue157_ReplayKeepsTheDeliveryWhenItsStoreCannotAnswer(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	_, runID, item, delivery := issue157PinnedDeliveryFixture(t, h, "issue157_store_fault.mp4")

	// The delivery the replay below must preserve: one accepted decision, rebuilt and completed.
	status, accepted, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if status != http.StatusOK || accepted == nil {
		t.Fatalf("expected the exact reviewed-candidate selection, got %d (%s)", status, refusal)
	}
	if settled := issue157WaitSettled(t, h, runID); settled.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the accepted delivery to complete the run, got %q", settled.Status)
	}
	settledMix := issue157StageArtifact(t, h, runID, "audio_mix")
	settledPlan := issue157StageArtifact(t, h, runID, "render_plan")
	settledPreview := issue157StageArtifact(t, h, runID, "render_preview")
	if settledMix == "" || settledPlan == "" || settledPreview == "" {
		t.Fatalf("expected the delivered run to publish mix, plan and preview, got %s/%s/%s",
			settledMix, settledPlan, settledPreview)
	}
	if strings.EqualFold(settledMix, delivery["audio_mix"]) {
		t.Fatalf("the acceptance must have rebuilt the delivery's mix, still %s", settledMix)
	}

	// A store that cannot answer the delivery lookup is not a run without a delivery.
	rawDB := issue157RawDB(t, h)
	if _, err := rawDB.ExecContext(ctx, `ALTER TABLE dub_mix_artifacts RENAME TO issue157_hidden_mix_artifacts`); err != nil {
		t.Fatalf("hide the mix index: %v", err)
	}
	faulted, _, faultRefusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if _, err := rawDB.ExecContext(ctx, `ALTER TABLE issue157_hidden_mix_artifacts RENAME TO dub_mix_artifacts`); err != nil {
		t.Fatalf("restore the mix index: %v", err)
	}
	if faulted == http.StatusOK {
		t.Fatalf("a store that cannot answer must not be reported as a completed replay: %s", faultRefusal)
	}

	// Nothing was withdrawn: the delivery the run serves is still the one it published.
	if got := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(got, settledMix) {
		t.Fatalf("the failed replay must not withdraw the run's mix, got %s want %s", got, settledMix)
	}
	if got := issue157StageArtifact(t, h, runID, "render_plan"); !strings.EqualFold(got, settledPlan) {
		t.Fatalf("the failed replay must not invalidate the run's plan, got %s want %s", got, settledPlan)
	}
	if got := issue157StageArtifact(t, h, runID, "render_preview"); !strings.EqualFold(got, settledPreview) {
		t.Fatalf("the failed replay must not withdraw the run's preview, got %s want %s", got, settledPreview)
	}
	if got := issue157OverrideCount(t, h, runID); got != 1 {
		t.Fatalf("the failed replay must not append an audit row, got %d", got)
	}

	// With the store answering again, the same replay is the idempotent success it always was.
	again, replayed, againRefusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if again != http.StatusOK || replayed == nil || !replayed.Idempotent {
		t.Fatalf("expected the replay to resolve to the accepted decision, got %d (%s)", again, againRefusal)
	}
	if replayed.DubSegmentsVariantCAS != accepted.DubSegmentsVariantCAS {
		t.Fatalf("the replay must resolve to the recorded successor %s, got %s",
			accepted.DubSegmentsVariantCAS, replayed.DubSegmentsVariantCAS)
	}
}

// TestSeam1_Issue157_ResumeFinishesADeliveryInvalidatedWhileQueued proves a run whose delivery was
// withdrawn and invalidated mid-rebuild still finishes on resume.
//
// Ordering is what breaks it: the rebuild invalidates the delivery stages before it republishes them,
// and the pass read the run's stage rows once, when it started. Recording the mix it just mixed is
// what fails, so the run is left mid-delivery - its delivery stages queued while the artifact the run
// publishes a mix for is already the accepted successor's. A pass that published its rebuild on the
// row it cached at the start would leave that artifact shadowed by the queued row every reader
// resolves, and the delivery could never be handed off again.
func TestSeam1_Issue157_ResumeFinishesADeliveryInvalidatedWhileQueued(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	ctx := context.Background()
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	assetID, runID, item, delivery := issue157PinnedDeliveryFixture(t, h, "issue157_queued.mp4")

	rawDB := issue157RawDB(t, h)
	if _, err := rawDB.ExecContext(ctx, `CREATE TRIGGER fail_mix_record BEFORE INSERT ON stage_executions
		WHEN NEW.stage = 'audio_mix' AND NEW.status = 'succeeded'
		BEGIN
			SELECT RAISE(ABORT, 'mix stage write failed');
		END;`); err != nil {
		t.Fatalf("create failing stage trigger: %v", err)
	}
	status, _, refusal := issue157Accept(t, h, runID, issue157Selection(item.ID))
	if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_mix_record`); err != nil {
		t.Fatalf("drop failing stage trigger: %v", err)
	}
	if status != http.StatusInternalServerError || !strings.Contains(refusal, "reviewed candidate acceptance failed") {
		t.Fatalf("expected the injected mix-record failure to surface as the fixed internal fault, got %d: %s", status, refusal)
	}

	// The withdrawal is what stopped the delivery being current: the run no longer serves the preview
	// and final it published for the artifact the accepted candidate replaced.
	for _, kind := range []string{"preview", "final"} {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/%s?run_id=%s&target_language=vi",
			h.server.URL, assetID, kind, runID))
		if err != nil {
			t.Fatalf("request withdrawn %s render: %v", kind, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected the withdrawn %s render to stop resolving, got %d (%s)",
				kind, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if !strings.Contains(string(body), "render artifact not found") {
			t.Fatalf("the withdrawn %s render must stop resolving because it was withdrawn, got %s",
				kind, strings.TrimSpace(string(body)))
		}
	}
	queued := ""
	for _, stage := range []string{"render_preview", "render_final", "final_render_handoff"} {
		if issue157NewestStageStatus(t, rawDB, runID, stage) == string(domain.StageStatusQueued) {
			queued = stage
		}
	}
	if queued == "" {
		t.Fatalf("expected the interrupted rebuild to leave a delivery stage queued")
	}
	if got := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(got, delivery["audio_mix"]) {
		t.Fatalf("the interrupted rebuild must leave the delivery's mix standing, got %s want %s",
			got, delivery["audio_mix"])
	}
	claimed := issue157RunVariant(t, h, runID)
	if claimed == nil || claimed.OverallStatus != "PASS" || len(claimed.AcceptedCandidates) != 1 {
		t.Fatalf("expected the accepted decision to complete the successor's coverage, got %+v", claimed)
	}

	// The operator restarts the host and resumes: reconciliation supersedes the delivery the run still
	// records, and every stage it rebuilds must be the row the run's readers resolve.
	h2 := issue157Resume(t, h, runID)
	if settled := issue157WaitSettled(t, h2, runID); settled.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the resumed run to complete, got %q", settled.Status)
	}
	resumedDB := issue157RawDB(t, h2)
	for _, stage := range []string{"audio_mix", "render_plan", "render_preview", "render_final"} {
		if got := issue157NewestStageStatus(t, resumedDB, runID, stage); got != string(domain.StageStatusSucceeded) {
			t.Fatalf("the resume must publish %s on its newest row, got %q", stage, got)
		}
		if got := issue157StageArtifact(t, h2, runID, stage); got == "" {
			t.Fatalf("the resumed run must resolve a %s artifact for its delivery", stage)
		}
	}
	// The rebuilt delivery is what the run serves again, on both render kinds.
	for _, kind := range []string{"preview", "final"} {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/%s?run_id=%s&target_language=vi",
			h2.server.URL, assetID, kind, runID))
		if err != nil {
			t.Fatalf("request resumed %s render: %v", kind, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected the resumed run to serve its rebuilt %s, got %d (%s); preview=%s plan=%s",
				kind, resp.StatusCode, strings.TrimSpace(string(body)),
				issue157NewestStageStatus(t, resumedDB, runID, "render_preview"),
				issue157NewestStageStatus(t, resumedDB, runID, "render_plan"))
		}
	}
	mixIdx, err := h2.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixIdx == nil {
		t.Fatalf("the resumed run must publish a mix for its accepted successor: %v (%+v)", err, mixIdx)
	}
	if strings.EqualFold(mixIdx.CASHash, delivery["audio_mix"]) {
		t.Fatalf("the resume must rebuild the mix the accepted successor replaced, still %s", delivery["audio_mix"])
	}
	if rebuilt := issue157ReadMix(t, h2, runID); !strings.EqualFold(rebuilt.DubSegmentsCAS, claimed.CASHash) {
		t.Fatalf("the rebuilt mix must be mixed from the accepted successor %s, got %s",
			claimed.CASHash, rebuilt.DubSegmentsCAS)
	}
	if frozen := issue157ReadPlan(t, h2, runID); !strings.EqualFold(frozen.DubMixCASHash, mixIdx.CASHash) {
		t.Fatalf("the re-frozen plan must pin the rebuilt mix %s, got %s", mixIdx.CASHash, frozen.DubMixCASHash)
	}
	if final := issue157RunVariant(t, h2, runID); final.CASHash != claimed.CASHash {
		t.Fatalf("the resume must not mint another decision: %s != %s", final.CASHash, claimed.CASHash)
	}
	if got := issue157OverrideCount(t, h2, runID); got != 1 {
		t.Fatalf("the resume must not append audit rows, got %d", got)
	}
}

// issue157StageCount counts the run's recorded executions of one stage, which is how a case observes
// whether a delivery was built more than once.
func issue157StageCount(t *testing.T, h *testHarness, runID, stage string) int {
	t.Helper()
	rows, err := h.db.ListStageExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("read run %s stage executions: %v", runID, err)
	}
	count := 0
	for _, row := range rows {
		if row.Stage == stage {
			count++
		}
	}
	return count
}

// issue157RenderLineage resolves one rendered artifact over the API and returns the plan it consumed
// and the media it published: the two ends a reader can check a render against.
func issue157RenderLineage(t *testing.T, h *testHarness, path, key string) (string, string) {
	t.Helper()
	resp, err := http.Get(h.server.URL + path)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%v err=%v", path, statusOf(resp), err)
	}
	raw, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read %s: %v", path, readErr)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %s: %v (body %s)", path, err, string(raw))
	}
	var artifact struct {
		ConsumedPlan struct {
			PlanCASHash string `json:"plan_cas_hash"`
		} `json:"consumed_plan"`
		OutputCASHash string `json:"output_cas_hash"`
	}
	if err := json.Unmarshal(envelope[key], &artifact); err != nil {
		t.Fatalf("decode %s artifact: %v (body %s)", path, err, string(raw))
	}
	return artifact.ConsumedPlan.PlanCASHash, artifact.OutputCASHash
}

// TestSeam1_Issue157_ResumeDuringAcceptanceRebuildConvergesToTheAcceptedDelivery drives the
// interleaving the per-run delivery lock exists for: the operator resolves the run's last review
// unit while the run itself is being resumed, so the pipeline and the acceptance can both be
// building that run's delivery. In either order one decision must be recorded per unit, the run must
// end up serving the delivery built from the accepted artifact - never the media the acceptance
// replaced - its stages must not pin a superseded artifact, and the decision must stay idempotent.
func TestSeam1_Issue157_ResumeDuringAcceptanceRebuildConvergesToTheAcceptedDelivery(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	lane := defaultVITTSFake(t, h)
	lane.DurationMs = 330

	assetID := ingestSyntheticAssetWithFrequency(t, h.server.URL, h.dir, "issue157_resume_storm.mp4", 2.0, 2500)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)
	if parked := pollRunStatus(t, h.server.URL, runID, domain.RunStatusPaused, 60*time.Second); parked.Status != domain.RunStatusPaused {
		t.Fatalf("expected the run's own pass to park on its unresolved candidate, got %q", parked.Status)
	}
	items := issue157ReviewItems(t, h, runID)
	if len(items) != 1 {
		t.Fatalf("expected exactly one pending unit to resolve, got %+v", items)
	}
	replaced := issue157RunVariant(t, h, runID)
	mixesBefore := issue157StageCount(t, h, runID, "audio_mix")

	// The storm: the resume and the acceptance of the run's last unresolved unit are issued together.
	// Nothing orders them, so the pipeline and the acceptance path are free to reach the same
	// delivery at the same time.
	var wg sync.WaitGroup
	var resumeStatus int
	var resumeErr error
	var accStatus int
	var accResult *service.AcceptReviewedCandidateResult
	var accRefusal string
	var accErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		resp, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/resume", h.server.URL, runID), "application/json", nil)
		if err != nil {
			resumeErr = err
			return
		}
		resumeStatus = resp.StatusCode
		resp.Body.Close()
	}()
	go func() {
		defer wg.Done()
		accStatus, accResult, accRefusal, accErr = issue157PostAccept(h, runID, issue157Selection(items[0].ID))
	}()
	wg.Wait()
	if resumeErr != nil {
		t.Fatalf("resume the run while the acceptance runs: %v", resumeErr)
	}
	// A run the acceptance already moved on from is not queued any more, which the route reports as a
	// conflict: the interleaving where the acceptance wins the queue the resume asked for.
	if resumeStatus != http.StatusOK && resumeStatus != http.StatusConflict {
		t.Fatalf("expected the resume to be accepted or answered as a conflict, got %d", resumeStatus)
	}
	if accErr != nil {
		t.Fatalf("accept the last unit during the resume: %v", accErr)
	}
	if accStatus != http.StatusOK || accResult == nil || !accResult.CoverageComplete {
		t.Fatalf("expected the last acceptance to complete the coverage, got %d (%s)", accStatus, accRefusal)
	}
	issue157WaitStagesStable(t, h, runID)

	// One decision per unit, and the run's artifact is the successor that last acceptance named.
	successor := issue157RunVariant(t, h, runID)
	if len(successor.AcceptedCandidates) != 1 {
		t.Fatalf("expected the decision recorded on the run's artifact, got %+v", successor.AcceptedCandidates)
	}
	if !strings.EqualFold(successor.CASHash, accResult.DubSegmentsVariantCAS) {
		t.Fatalf("the run's dubbing artifact must be the successor %s, got %s", accResult.DubSegmentsVariantCAS, successor.CASHash)
	}
	if got := issue157OverrideCount(t, h, runID); got != 1 {
		t.Fatalf("expected one audit row for the decision, got %d", got)
	}

	// The delivery the run resolves is built from the accepted artifact - not from the one the
	// acceptance replaced - and no stage pin is left naming a superseded artifact.
	mix := issue157ReadMix(t, h, runID)
	if !strings.EqualFold(mix.DubSegmentsCAS, successor.CASHash) {
		t.Fatalf("the run's mix must be mixed from the accepted artifact %s, got %s", successor.CASHash, mix.DubSegmentsCAS)
	}
	if strings.EqualFold(mix.DubSegmentsCAS, replaced.CASHash) {
		t.Fatalf("the run must not serve media mixed from the replaced artifact %s", replaced.CASHash)
	}
	mixIdx, err := h.db.GetDubMixArtifactIndexByRun(context.Background(), runID)
	if err != nil || mixIdx == nil {
		t.Fatalf("read the run's mix index: %v (%+v)", err, mixIdx)
	}
	if pinned := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(pinned, mixIdx.CASHash) {
		t.Fatalf("the run's audio_mix stage must pin the delivery it resolves (%s), got %s", mixIdx.CASHash, pinned)
	}
	plan := issue157ReadPlan(t, h, runID)
	if !strings.EqualFold(plan.DubMixCASHash, mixIdx.CASHash) {
		t.Fatalf("the run's plan must pin the mix it resolves %s, got %s", mixIdx.CASHash, plan.DubMixCASHash)
	}
	planIdx, err := h.db.GetRenderPlanIndexByRun(context.Background(), runID)
	if err != nil || planIdx == nil {
		t.Fatalf("read the run's plan index: %v (%+v)", err, planIdx)
	}
	if pinned := issue157StageArtifact(t, h, runID, "render_plan"); !strings.EqualFold(pinned, planIdx.CASHash) {
		t.Fatalf("the run's render_plan stage must pin the plan it resolves (%s), got %s", planIdx.CASHash, pinned)
	}

	// Both renders are frozen from that plan, so the media the run serves is the accepted delivery.
	for _, probe := range []struct {
		name string
		path string
		key  string
	}{
		{"preview", fmt.Sprintf("/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", assetID, runID), "preview_render"},
		{"final", fmt.Sprintf("/api/v1/assets/%s/render/final?run_id=%s&target_language=vi", assetID, runID), "final_render"},
	} {
		consumedPlan, output := issue157RenderLineage(t, h, probe.path, probe.key)
		if output == "" {
			t.Fatalf("the run's %s must publish the media it serves", probe.name)
		}
		if !strings.EqualFold(consumedPlan, planIdx.CASHash) {
			t.Fatalf("the run's %s must be frozen from the resolved plan %s, got %s", probe.name, planIdx.CASHash, consumedPlan)
		}
	}

	// The decision the storm resolved stays idempotent: it reports the delivery and records nothing new.
	overridesAfter := issue157OverrideCount(t, h, runID)
	replayStatus, replay, replayRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if replayStatus != http.StatusOK || replay == nil || !replay.Idempotent {
		t.Fatalf("expected an idempotent replay after the storm, got %d (%s)", replayStatus, replayRefusal)
	}
	if replay.DubSegmentsVariantCAS != accResult.DubSegmentsVariantCAS {
		t.Fatalf("the replay must report the recorded successor %s, got %s", accResult.DubSegmentsVariantCAS, replay.DubSegmentsVariantCAS)
	}
	if got := issue157OverrideCount(t, h, runID); got != overridesAfter {
		t.Fatalf("the replay must not append an audit row, got %d -> %d", overridesAfter, got)
	}
	// Neither path may rebuild the mix more than once: the acceptance rebuilds the delivery it was
	// asked for, and the pipeline either reuses it or rebuilt once before it, so a third mix would mean
	// the run's delivery is being reproduced in a loop. Which of the two wins is not ordered by this
	// case - that is the point of the race - so the bound is per path, and what the run serves is
	// checked above regardless of who published it.
	if got := issue157StageCount(t, h, runID, "audio_mix"); got > mixesBefore+2 {
		t.Fatalf("the run rebuilt its mix more than once per path (%d -> %d): a resume and an acceptance both published the same delivery",
			mixesBefore, got)
	}
}

// issue157WaitStagesStable waits until the run's recorded stages stop moving, so a case that races two
// paths on one run observes the state both of them left behind rather than the state in between.
func issue157WaitStagesStable(t *testing.T, h *testHarness, runID string) {
	t.Helper()
	fingerprint := func() string {
		rows, err := h.db.ListStageExecutions(context.Background(), runID)
		if err != nil {
			t.Fatalf("read run %s stage executions: %v", runID, err)
		}
		var b strings.Builder
		for _, row := range rows {
			fmt.Fprintf(&b, "%s/%s/%s/%s;", row.Stage, row.Status, row.ArtifactSHA256, row.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
		return b.String()
	}
	previous := ""
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		current := fingerprint()
		if current != "" && current == previous {
			return
		}
		previous = current
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the run's stages never settled: %s", previous)
}

// TestSeam1_Issue157_ResumeThatMovesTheReviewedLineageFailsTheDecisionSafely is the other half of the
// accept-versus-resume interleaving: the run's own pass can move the text/contract lineage the
// decision was bound to while the operator is taking it. Either the acceptance wins and the run
// serves the accepted delivery, or the decision is refused as a documented gate - never as an
// internal fault and never by silently delivering media built from an artifact the decision replaced,
// and the unit stays the operator's to resolve.
func TestSeam1_Issue157_ResumeThatMovesTheReviewedLineageFailsTheDecisionSafely(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupHarness(t)
	_, _, runID, items := issue157ParkedFixture(t, h, false)

	// One unit is resolved first, so the run holds a decision while coverage is still incomplete.
	firstStatus, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if firstStatus != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first unit to be accepted without a rebuild, got %d (%s)", firstStatus, firstRefusal)
	}
	pending := issue157ReviewItems(t, h, runID)
	if len(pending) != 1 {
		t.Fatalf("expected one unit left to resolve, got %+v", pending)
	}

	// The host comes back with its queue executor enabled, so the resume below really drives the run.
	h2 := issue157RestartAutoRunHost(t, h)

	// The resume is started first and driven to rest before the decision is taken, which is the
	// deterministic half of this interleaving: by the time the acceptance validates, the run's own pass
	// has finished moving whatever lineage it was going to move. No part of the outcome is left to
	// scheduling, so what this case pins is which class of answer a decision gets once the run has moved
	// on - and that a refusal, when that is the answer, is one of the documented gates.
	resumed, err := http.Post(fmt.Sprintf("%s/api/v1/runs/%s/resume", h2.server.URL, runID), "application/json", nil)
	if err != nil {
		t.Fatalf("resume the run: %v", err)
	}
	resumedBody, _ := io.ReadAll(resumed.Body)
	resumed.Body.Close()
	if resumed.StatusCode != http.StatusOK {
		t.Fatalf("expected the resume to be accepted, got %d (%s)", resumed.StatusCode, strings.TrimSpace(string(resumedBody)))
	}
	if reparked := pollRunStatus(t, h2.server.URL, runID, domain.RunStatusPaused, 60*time.Second); reparked.Status != domain.RunStatusPaused {
		t.Fatalf("expected the resumed pass to park again on the unit still open, got %q", reparked.Status)
	}
	issue157WaitStagesStable(t, h2, runID)

	accStatus, accResult, accRefusal, accErr := issue157PostAccept(h2, runID, issue157Selection(pending[0].ID))
	if accErr != nil {
		t.Fatalf("accept the unit after the resume: %v", accErr)
	}

	if accStatus == http.StatusOK {
		// The pass left the lineage the decision was bound to intact: the decision is taken and the
		// run serves exactly what it accepted.
		if accResult == nil || !accResult.CoverageComplete {
			t.Fatalf("expected a complete acceptance, got %d (%+v)", accStatus, accResult)
		}
		t.Logf("the decision was taken after the resume: coverage complete, the run holds %s", accResult.DubSegmentsVariantCAS)
		successor := issue157RunVariant(t, h2, runID)
		if mix := issue157ReadMix(t, h2, runID); !strings.EqualFold(mix.DubSegmentsCAS, successor.CASHash) {
			t.Fatalf("the run's mix must be mixed from the accepted artifact %s, got %s", successor.CASHash, mix.DubSegmentsCAS)
		}
		if got := issue157OverrideCount(t, h2, runID); got != 2 {
			t.Fatalf("expected one audit row per decision, got %d", got)
		}
		return
	}

	// The run's own pass moved the lineage the decision was bound to: the refusal must be one of the
	// classes the sequential selection cases pin, not an internal fault.
	if accStatus == http.StatusInternalServerError || accStatus != http.StatusConflict && accStatus != http.StatusUnprocessableEntity && accStatus != http.StatusBadRequest {
		t.Fatalf("a decision refused because the run moved its lineage must be a documented gate, got %d (%s)", accStatus, accRefusal)
	}
	t.Logf("the decision was refused as %d (%s)", accStatus, accRefusal)
	// Nothing the run resolves may be built from an artifact it no longer holds.
	if mixIdx, err := h2.db.GetDubMixArtifactIndexByRun(context.Background(), runID); err == nil && mixIdx != nil {
		mix := issue157ReadMix(t, h2, runID)
		current := issue157RunVariant(t, h2, runID)
		if !strings.EqualFold(mix.DubSegmentsCAS, current.CASHash) {
			t.Fatalf("the run must not serve media mixed from an artifact it replaced: mix pins %s, run holds %s",
				mix.DubSegmentsCAS, current.CASHash)
		}
	}
	// A refused decision releases nothing and stays the operator's to resolve.
	if remaining := issue157ReviewItems(t, h2, runID); len(remaining) == 0 {
		t.Fatalf("a refused decision must leave its unit resolvable, got %+v", remaining)
	}
}

// issue157SegmentsForSpeaker returns the selected segments one speaker owns in a variant.
func issue157SegmentsForSpeaker(variant *domain.DubSegmentsVariant, speakerID string) []domain.DubSegment {
	var found []domain.DubSegment
	for _, seg := range variant.Segments {
		if seg.SpeakerID == speakerID {
			found = append(found, seg)
		}
	}
	return found
}

// issue157CanonicalSourcePins fingerprints the artifacts the run's canonical source stages own - the
// speech, alignment, diarization and visual-text stages that read the source media. A regeneration of a
// target delivery is free to rewrite the target-derived stages; it is not free to rewrite what the
// canonical index of the source owns.
func issue157CanonicalSourcePins(t *testing.T, h *testHarness, runID string) []string {
	t.Helper()
	// The source-owned stages: speech understanding, the source audio roles and the separated stems,
	// plus whatever visual stage reads the source video. The target-derived stages (translation,
	// dub_script, dub_synthesize, audio_mix, the renders and the handoff) are deliberately absent: a
	// contract regeneration is allowed to rewrite those.
	canonical := func(stage string) bool {
		switch stage {
		case "speech_understand", "audio_role_plan", "audio_stems":
			return true
		}
		return strings.HasPrefix(stage, "visual")
	}
	var pins []string
	for _, row := range getRunStages(t, h.server.URL, runID) {
		if !canonical(row.Stage) || row.ArtifactSHA256 == "" {
			continue
		}
		pins = append(pins, row.Stage+"="+row.ArtifactSHA256)
	}
	slices.Sort(pins)
	return pins
}

// TestSeam1_Issue157_AcceptedSpeakerSurvivesTheNextSelection proves an acceptance applies to the unit
// the operator named and to nothing else. A speaker whose unit was already resolved keeps its exact
// decision - the same immutable candidate hashes, the same audit row, the same selected segment - when
// the run's last remaining unit is accepted and the delivery is rebuilt from the successor carrying
// both. No second decision, no re-probed waveform and no re-attributed audit row may appear for the
// unit the operator was done with.
func TestSeam1_Issue157_AcceptedSpeakerSurvivesTheNextSelection(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupHarness(t)
	_, assetID, runID, items := issue157ParkedFixture(t, h, false)
	if len(items) < 2 || items[0].SpeakerID == items[1].SpeakerID {
		t.Fatalf("expected two distinct speakers to resolve, got %+v", items)
	}

	// The first unit is resolved alone: coverage stays incomplete, so nothing is mixed and the second
	// speaker stays the operator's to decide.
	firstStatus, first, firstRefusal := issue157Accept(t, h, runID, issue157Selection(items[0].ID))
	if firstStatus != http.StatusOK || first == nil || first.CoverageComplete {
		t.Fatalf("expected the first unit to be accepted without a rebuild, got %d (%s)", firstStatus, firstRefusal)
	}
	partial := issue157RunVariant(t, h, runID)
	if len(partial.Segments) != 1 || len(partial.ReviewSegments) != 1 {
		t.Fatalf("expected one accepted and one unresolved unit, got %d/%d", len(partial.Segments), len(partial.ReviewSegments))
	}
	kept := partial.Segments[0]
	if kept.SpeakerID != items[0].SpeakerID {
		t.Fatalf("the accepted unit must stay the speaker the operator resolved, got %s want %s", kept.SpeakerID, items[0].SpeakerID)
	}
	keptEvidence := partial.AcceptedCandidateFor(items[0].ID)
	if keptEvidence == nil {
		t.Fatalf("expected the first decision recorded on the run's artifact, got %+v", partial.AcceptedCandidates)
	}
	if got := issue157OverrideCount(t, h, runID); got != 1 {
		t.Fatalf("expected one audit row for the first decision, got %d", got)
	}

	// The remaining unit is read again: the successor re-projects the run's queue against itself, so the
	// item the operator resolves next is the one the run currently projects, not the copy the first
	// decision replaced.
	remaining := issue157ReviewItems(t, h, runID)
	if len(remaining) != 1 || remaining[0].SpeakerID != items[1].SpeakerID {
		t.Fatalf("expected the second speaker's unit still pending, got %+v", remaining)
	}

	// The second speaker's unit completes the coverage: the delivery is rebuilt from the successor.
	status, second, refusal := issue157Accept(t, h, runID, issue157Selection(remaining[0].ID))
	if status != http.StatusOK || second == nil || !second.CoverageComplete {
		t.Fatalf("expected the last unit to complete the coverage, got %d (%s)", status, refusal)
	}
	if settled := issue157WaitSettled(t, h, runID); settled.Status != domain.RunStatusCompleted {
		t.Fatalf("expected the accepted delivery to complete the run, got %q", settled.Status)
	}
	successor := issue157RunVariant(t, h, runID)
	if !strings.EqualFold(successor.CASHash, second.DubSegmentsVariantCAS) {
		t.Fatalf("the run's artifact must be the successor %s, got %s", second.DubSegmentsVariantCAS, successor.CASHash)
	}

	// The earlier decision is the same evidence, not a new one minted by the later selection.
	preserved := successor.AcceptedCandidateFor(items[0].ID)
	if preserved == nil {
		t.Fatalf("the earlier speaker's decision must survive the next selection, got %+v", successor.AcceptedCandidates)
	}
	if preserved.ReviewOverrideID != keptEvidence.ReviewOverrideID ||
		preserved.SelectedAudioSHA256 != keptEvidence.SelectedAudioSHA256 ||
		preserved.NaturalAudioSHA256 != keptEvidence.NaturalAudioSHA256 ||
		preserved.QualityWaiver != keptEvidence.QualityWaiver ||
		preserved.Operator != keptEvidence.Operator || preserved.Reason != keptEvidence.Reason ||
		!preserved.AcceptedAt.Equal(keptEvidence.AcceptedAt) {
		t.Fatalf("the earlier decision must stay the same evidence: got %+v want %+v", preserved, keptEvidence)
	}
	// The audit row of the earlier decision is the same row: same id, same recorded decision, same
	// moment. A later selection that re-attributed or rewrote it would show up here.
	rowFor := func(t *testing.T, h *testHarness, overrideID string) domain.ReviewOverride {
		t.Helper()
		rows, err := h.db.GetReviewOverridesByRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("read review overrides for run %s: %v", runID, err)
		}
		for _, row := range rows {
			if row.ID == overrideID {
				return row
			}
		}
		t.Fatalf("expected the audit row %s to stay on record, got %+v", overrideID, rows)
		return domain.ReviewOverride{}
	}
	keptRow := rowFor(t, h, keptEvidence.ReviewOverrideID)
	if got := rowFor(t, h, preserved.ReviewOverrideID); got.Action != keptRow.Action || got.Operator != keptRow.Operator ||
		got.Reason != keptRow.Reason || !got.CreatedAt.Equal(keptRow.CreatedAt) || got.ReviewItemID != keptRow.ReviewItemID {
		t.Fatalf("the earlier decision's audit row must be unchanged: got %+v want %+v", got, keptRow)
	}
	if len(successor.AcceptedCandidates) != 2 {
		t.Fatalf("the successor must extend the earlier decision, got %+v", successor.AcceptedCandidates)
	}
	// The unaffected speaker's selected segment is still the one the operator accepted: same slot, same
	// waveform, same canonical membership - and it is carried into the delivery alongside the new one.
	after := issue157SegmentsForSpeaker(successor, items[0].SpeakerID)
	if len(after) != 1 {
		t.Fatalf("expected exactly one selected segment for %s, got %+v", items[0].SpeakerID, after)
	}
	if after[0].AudioSHA256 != kept.AudioSHA256 || after[0].StartMs != kept.StartMs || after[0].EndMs != kept.EndMs ||
		!slices.Equal(after[0].SpeechBlockIndices, kept.SpeechBlockIndices) {
		t.Fatalf("the unaffected speaker's segment must be preserved: got %+v want %+v", after[0], kept)
	}
	if len(issue157SegmentsForSpeaker(successor, items[1].SpeakerID)) != 1 {
		t.Fatalf("the second speaker's accepted segment must be in the successor, got %+v", successor.Segments)
	}
	if got := issue157OverrideCount(t, h, runID); got != 2 {
		t.Fatalf("each decision records exactly one audit row, got %d", got)
	}

	// The delivery the run serves is mixed from that successor, and both speakers are in it.
	mix := issue157ReadMix(t, h, runID)
	if !strings.EqualFold(mix.DubSegmentsCAS, successor.CASHash) {
		t.Fatalf("the run's mix must be mixed from the successor %s, got %s", successor.CASHash, mix.DubSegmentsCAS)
	}
	mixIdx, err := h.db.GetDubMixArtifactIndexByRun(context.Background(), runID)
	if err != nil || mixIdx == nil {
		t.Fatalf("read the run's mix index: %v (%+v)", err, mixIdx)
	}
	if pinned := issue157StageArtifact(t, h, runID, "audio_mix"); !strings.EqualFold(pinned, mixIdx.CASHash) {
		t.Fatalf("the run's audio_mix stage must pin the delivery it resolves (%s), got %s", mixIdx.CASHash, pinned)
	}
	plan := issue157ReadPlan(t, h, runID)
	if !strings.EqualFold(plan.DubMixCASHash, mixIdx.CASHash) {
		t.Fatalf("the plan must be frozen from the rebuilt mix %s, got %s", mixIdx.CASHash, plan.DubMixCASHash)
	}
	// Both render kinds consume that same plan, so the media the run serves is the accepted delivery
	// rather than one frozen before the second speaker was decided.
	planIdx, err := h.db.GetRenderPlanIndexByRun(context.Background(), runID)
	if err != nil || planIdx == nil {
		t.Fatalf("read the run's plan index: %v (%+v)", err, planIdx)
	}
	for _, probe := range []struct {
		name string
		path string
		key  string
	}{
		{"preview", fmt.Sprintf("/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", assetID, runID), "preview_render"},
		{"final", fmt.Sprintf("/api/v1/assets/%s/render/final?run_id=%s&target_language=vi", assetID, runID), "final_render"},
	} {
		consumedPlan, output := issue157RenderLineage(t, h, probe.path, probe.key)
		if output == "" {
			t.Fatalf("the run's %s must publish the media its accepted delivery renders", probe.name)
		}
		if !strings.EqualFold(consumedPlan, planIdx.CASHash) {
			t.Fatalf("the run's %s must be frozen from the accepted plan %s, got %s", probe.name, planIdx.CASHash, consumedPlan)
		}
	}
}

// TestSeam1_Issue157_StaleContractRegeneratesTheAffectedDescendants proves an old/incompatible
// dubbing contract regenerates the affected target descendants instead of leaving a run serving media
// built for the contract it replaced - and that it does so without touching what the canonical source
// stages own and without transferring an approval to the regenerated artifact. The delivery was
// published for one contract; the operator's correction re-pins a newer one (translation and dub script
// together, the way the inspector's own path mints them), so the run's next pass rebuilds the affected
// descendants for the contract it now carries: while the unit it left unresolved still overruns, that
// rebuild is a refusal of the current contract rather than a mix of the replaced one, and the delivery
// becomes a PASS mix of the accepted successor only once the operator resolves that unit.
func TestSeam1_Issue157_StaleContractRegeneratesTheAffectedDescendants(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to rebuild the accepted delivery")
	}
	h := setupAutoRunHarness(t)
	configureDialogueTranslationGateway(t, h)
	ctx := context.Background()

	// The fixture leaves the run holding a superseded delivery: the mix, plan and preview it serves were
	// published for the artifact the operator's latest correction replaced, and one unit is still open.
	assetID, runID, _, delivery := issue157PinnedDeliveryFixture(t, h, "issue157_stale_contract.mp4")
	replacedMix := delivery["audio_mix"]
	replacedPlan := delivery["render_plan"]
	replacedPreview := delivery["render_preview"]
	replacedVariant := issue157RunVariant(t, h, runID)
	canonicalBefore := issue157CanonicalSourcePins(t, h, runID)
	if len(canonicalBefore) == 0 {
		t.Fatalf("expected the fixture to have pinned canonical source stages")
	}
	overridesBefore := issue157OverrideCount(t, h, runID)

	// The operator restarts the host and resumes the run. The resume drives the pipeline
	// asynchronously, so the state the assertions read is the one that pass left behind: the run is out
	// of the queue, its stage rows have stopped moving, and the stage it has to rerun for the current
	// contract is no longer running.
	h2 := issue157Resume(t, h, runID)
	if settled := issue157WaitSettled(t, h2, runID); settled.Status != domain.RunStatusPaused && settled.Status != domain.RunStatusCompleted {
		t.Fatalf("the resumed run must settle on the regenerated contract, got %q (stages %+v)",
			settled.Status, getRunStages(t, h2.server.URL, runID))
	}
	issue157WaitStagesStable(t, h2, runID)
	rawDB := issue157RawDB(t, h2)
	synthesisStopped := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		if issue157NewestStageStatus(t, rawDB, runID, "dub_synthesize") != string(domain.StageStatusRunning) {
			synthesisStopped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !synthesisStopped {
		t.Fatalf("the resumed pass never stopped rerunning the dubbing stage (stages %+v)",
			getRunStages(t, h2.server.URL, runID))
	}

	// The affected target descendants are regenerated for the contract the run now carries. The run's
	// dubbing artifact is the one the operator's latest correction published; what was stale is the
	// delivery published for the artifact that correction replaced, so the rebuilt mix has to be built
	// from the current artifact instead of the one the served media was mixed for.
	current := issue157RunVariant(t, h2, runID)
	if current == nil || !strings.EqualFold(current.CASHash, replacedVariant.CASHash) {
		t.Fatalf("the correction's artifact must stay the run's own, got %+v want %s", current, replacedVariant.CASHash)
	}
	mixIdx, err := h2.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixIdx == nil {
		t.Fatalf("the resumed run must publish a mix for the regenerated artifact: %v (%+v)", err, mixIdx)
	}
	if strings.EqualFold(mixIdx.CASHash, replacedMix) {
		t.Fatalf("the resume must not keep serving the mix published for the superseded contract, still %s", replacedMix)
	}
	// The run's own delivery is regenerated for the contract it carries. The unit the operator has not
	// resolved yet still overruns its immutable slot, so the regenerated mix is a refusal of that
	// contract rather than a mix of the superseded one: the run publishes no partial delivery, and every
	// reader gates on the artifact it resolves, so a refusal cannot be served as current media.
	mix := issue157ReadMix(t, h2, runID)
	if !strings.EqualFold(mix.DubSegmentsCAS, current.CASHash) {
		t.Fatalf("the rebuilt mix must be mixed from the regenerated artifact %s, got %s", current.CASHash, mix.DubSegmentsCAS)
	}
	if !strings.EqualFold(mix.OverallStatus, "REFUSED") {
		t.Fatalf("an unresolved overrun must refuse the regenerated mix instead of serving the superseded one, got %q", mix.OverallStatus)
	}
	if !strings.Contains(mix.RefusalReason, "overran") {
		t.Fatalf("the refusal must name the overrun that has no accepted candidate, got %q", mix.RefusalReason)
	}
	// The run parks on the unit still open, so the stages after the rebuild may not be pinned yet: what
	// must hold is that nothing the run publishes or serves belongs to the delivery the correction
	// replaced.
	if pinned := issue157StageArtifact(t, h2, runID, "audio_mix"); pinned != "" && !strings.EqualFold(pinned, mixIdx.CASHash) {
		t.Fatalf("the run's mix pin must name the rebuilt mix %s, got %s", mixIdx.CASHash, pinned)
	}
	if pinned := issue157StageArtifact(t, h2, runID, "render_plan"); strings.EqualFold(pinned, replacedPlan) {
		t.Fatalf("the superseded plan must not stay pinned, still %s", replacedPlan)
	}
	// A run parked before its plan stage holds no plan row at all; when it holds one, the plan the run
	// resolves has to be frozen from the rebuilt mix.
	if planIdx, err := h2.db.GetRenderPlanIndexByRun(ctx, runID); err == nil && planIdx != nil {
		raw, err := h2.casStore.Get(planIdx.CASHash)
		if err != nil {
			t.Fatalf("read the run's resolved plan: %v", err)
		}
		var plan domain.RenderPlan
		if err := json.NewDecoder(raw).Decode(&plan); err != nil {
			raw.Close()
			t.Fatalf("decode the run's resolved plan: %v", err)
		}
		raw.Close()
		if !strings.EqualFold(plan.DubMixCASHash, mixIdx.CASHash) {
			t.Fatalf("the plan the run resolves must be frozen from the rebuilt mix %s, got %s", mixIdx.CASHash, plan.DubMixCASHash)
		}
	}
	// A render that names no plan resolves this run's plan index, so the plan the withdrawn delivery was
	// rendered from must not stay resolvable either: a run whose delivery was withdrawn refuses the
	// render instead of publishing media from the replaced mix.
	if renderResp, renderErr := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/render/preview", h2.server.URL, assetID),
		"application/json", bytes.NewReader(mustJSON(t, map[string]any{
			"run_id":          runID,
			"job_id":          issue157Run(t, h2, runID).JobID,
			"target_language": domain.TargetLanguageVI,
		}))); renderErr != nil {
		t.Fatalf("request the run's own preview render: %v", renderErr)
	} else {
		body, _ := io.ReadAll(renderResp.Body)
		renderResp.Body.Close()
		if renderResp.StatusCode != http.StatusNotFound {
			t.Fatalf("a withdrawn delivery must not render from its replaced plan, got %d (%s)",
				renderResp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	if pinned := issue157StageArtifact(t, h2, runID, "render_preview"); strings.EqualFold(pinned, replacedPreview) {
		t.Fatalf("the superseded preview must be withdrawn, still pinned as %s", replacedPreview)
	}
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi",
		h2.server.URL, assetID, runID))
	if err != nil {
		t.Fatalf("request the run's preview: %v", err)
	}
	if resp.StatusCode == http.StatusOK {
		plan, output := issue157RenderLineage(t, h2,
			fmt.Sprintf("/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", assetID, runID), "preview_render")
		// A preview the run serves has to be a committed render, not the placeholder the fixture pinned
		// for the superseded delivery.
		if output == "" || !h2.casStore.Exists(output) {
			t.Fatalf("a resolved preview must serve committed media, got %q (plan %s)", output, plan)
		}
	} else if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a withdrawn preview must answer as not found, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Canonical speech/visual ownership is exactly what it was: the source stages the delivery does not
	// own were not rewritten by the regeneration.
	if after := issue157CanonicalSourcePins(t, h2, runID); !slices.Equal(canonicalBefore, after) {
		t.Fatalf("a contract regeneration must not rewrite canonical ownership:\n before %v\n after  %v", canonicalBefore, after)
	}

	// No approval is transferred to the regenerated artifact: the regeneration itself recorded no
	// decision, and the operator's unit is still the run's to resolve against the contract it now carries
	// rather than the one the superseded delivery was built for.
	if got := issue157OverrideCount(t, h2, runID); got != overridesBefore {
		t.Fatalf("a regeneration must not record decisions by itself, got %d rows (was %d)", got, overridesBefore)
	}
	pending := issue157ReviewItems(t, h2, runID)
	if len(pending) != 1 {
		t.Fatalf("the operator's unit must stay resolvable after the regeneration, got %+v", pending)
	}
	status, decision, refusal := issue157Accept(t, h2, runID, issue157Selection(pending[0].ID))
	if status != http.StatusOK || decision == nil || !decision.CoverageComplete {
		t.Fatalf("expected the regenerated run to accept its remaining unit, got %d (%s)", status, refusal)
	}
	successor := issue157RunVariant(t, h2, runID)
	if !strings.EqualFold(successor.CASHash, decision.DubSegmentsVariantCAS) {
		t.Fatalf("the run's artifact must be the decision's successor %s, got %s", decision.DubSegmentsVariantCAS, successor.CASHash)
	}
	if !strings.EqualFold(successor.DubScriptVariantCAS, current.DubScriptVariantCAS) {
		t.Fatalf("the decision must bind the contract the run carries %s, got %s",
			current.DubScriptVariantCAS, successor.DubScriptVariantCAS)
	}
	if got := issue157OverrideCount(t, h2, runID); got != overridesBefore+1 {
		t.Fatalf("the decision must record exactly one audit row, got %d (was %d)", got, overridesBefore)
	}
	if mix := issue157ReadMix(t, h2, runID); !strings.EqualFold(mix.DubSegmentsCAS, successor.CASHash) {
		t.Fatalf("the completed coverage must rebuild the delivery from the accepted successor %s, got %s",
			successor.CASHash, mix.DubSegmentsCAS)
	}
}
