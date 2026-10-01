package seam1_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #155 (R4): bounded same-speaker regrouping and preserved recovery state through
// speaker escalation. These cases drive the RuntimeHost API over Seam 1 and assert exact
// canonical member IDs, synthesis call counts, source anchors and review/selection evidence.

// issue155Turn is one canonical speech turn of a bounded-regroup fixture. A turn marked
// scriptAbsent stays pinned in the canonical transcript but is filtered out of the dub
// script, which is how a dub-ineligible source turn appears to script adjacency.
type issue155Turn struct {
	speaker      string
	startMs      int64
	endMs        int64
	source       string
	spoken       string
	scriptAbsent bool
	// declaredGapMs overrides the gap the dub script declares to the next script turn,
	// so a case can prove the pass resolves the boundary from the pinned canonical
	// timeline instead of trusting the script's own adjacency. 0 derives it.
	declaredGapMs int64
}

// issue155PinLineage pins one canonical transcript, audio-role plan, translation contract and
// dub script for a bounded-regroup case, so every boundary the pass resolves comes from the
// pinned canonical timeline rather than from script adjacency alone.
func issue155PinLineage(t *testing.T, h *testHarness, jobID, assetID, runID string, roleSegments []domain.AudioSegment, turns []issue155Turn) (string, *domain.DubScriptVariant) {
	t.Helper()
	saveAndPinAudioRolePlan(t, h, assetID, runID, roleSegments)

	blocks := make([]domain.SpeechBlock, 0, len(turns))
	segments := make([]domain.DubScriptSegment, 0, len(turns))
	translationSegments := make([]domain.TranslationSegment, 0, len(turns))
	// The script's declared gap is measured to the next turn the script actually carries,
	// which is the only adjacency the synthesis pass can see on its own.
	nextScriptStartMs := func(from int) int64 {
		for j := from + 1; j < len(turns); j++ {
			if !turns[j].scriptAbsent {
				return turns[j].startMs
			}
		}
		return 0
	}
	for i, turn := range turns {
		blocks = append(blocks, domain.SpeechBlock{
			Index: i, SegmentType: domain.SpeechBlockTypeSpeech, SourceText: turn.source,
			SpeakerID: turn.speaker, StartMs: turn.startMs, EndMs: turn.endMs,
		})
		if turn.scriptAbsent {
			continue
		}
		gapMs := turn.declaredGapMs
		if gapMs == 0 {
			if next := nextScriptStartMs(i); next > turn.endMs {
				gapMs = next - turn.endMs
			}
		}
		segments = append(segments, domain.DubScriptSegment{
			Index: i, SpeakerID: turn.speaker, StartMs: turn.startMs, EndMs: turn.endMs,
			SlotDurationMs: turn.endMs - turn.startMs, SourceText: turn.source,
			MeaningText: turn.spoken, SpokenText: turn.spoken,
			SourceGapAfterMs: gapMs, PassedQAGate: true,
		})
		translationSegments = append(translationSegments, domain.TranslationSegment{
			Index: i, SourceText: turn.source, TargetText: turn.spoken, SpeakerID: turn.speaker,
			StartMs: turn.startMs, EndMs: turn.endMs, PassedQAGate: true,
		})
	}

	transcript := domain.TranscriptArtifact{
		ID: "transcript-155-" + assetID, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", SpeechBlocks: blocks,
		ProvenanceHash: "prov-transcript-155-" + assetID, CreatedAt: time.Now().UTC(),
	}
	transcriptBytes, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("marshal transcript fixture: %v", err)
	}
	transcriptObj, err := h.casStore.Put(bytes.NewReader(transcriptBytes))
	if err != nil {
		t.Fatalf("put transcript fixture: %v", err)
	}
	if err := h.db.SaveTranscriptArtifactIndex(context.Background(), storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: transcriptObj.SHA256,
		ProvenanceHash: transcript.ProvenanceHash, CreatedAt: transcript.CreatedAt,
	}); err != nil {
		t.Fatalf("save transcript index fixture: %v", err)
	}
	if err := h.db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID: "se-speech-155-" + runID, RunID: runID, Stage: "speech_understand",
		Status: domain.StageStatusSucceeded, ArtifactSHA256: transcriptObj.SHA256,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pin speech_understand stage fixture: %v", err)
	}

	dubScript := &domain.DubScriptVariant{
		ID: "dubscript-155-" + assetID, SchemaVersion: domain.DubScriptSchemaVersion,
		AssetID: assetID, RunID: runID, JobID: jobID, SourceLanguage: "zh",
		TargetLanguage: "vi", Segments: segments, CreatedAt: time.Now().UTC(),
	}
	translation := domain.TranslationVariant{
		ID: "tv-155-" + assetID, SchemaVersion: domain.TranslationSchemaVersion,
		ContractID: service.TranslationContractID, AssetID: assetID, RunID: runID, JobID: jobID,
		SourceLanguage: "zh", TargetLanguage: "vi", TranscriptArtifactCAS: transcriptObj.SHA256,
		InputHash: "h-155-" + assetID, ProvenanceHash: "p-155-" + assetID,
		Segments: translationSegments, CreatedAt: time.Now().UTC(),
	}
	translationBytes, err := json.Marshal(translation)
	if err != nil {
		t.Fatalf("marshal translation contract fixture: %v", err)
	}
	translationObj, err := h.casStore.Put(bytes.NewReader(translationBytes))
	if err != nil {
		t.Fatalf("put translation contract fixture: %v", err)
	}
	dubScript.TranslationVariantCAS = translationObj.SHA256

	scriptBytes, err := json.Marshal(dubScript)
	if err != nil {
		t.Fatalf("marshal dub script fixture: %v", err)
	}
	scriptObj, err := h.casStore.Put(bytes.NewReader(scriptBytes))
	if err != nil {
		t.Fatalf("put dub script fixture: %v", err)
	}
	if err := h.db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		CASHash: scriptObj.SHA256, ProvenanceHash: "prov-155-" + assetID, CreatedAt: dubScript.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index fixture: %v", err)
	}
	return scriptObj.SHA256, dubScript
}

// issue155Membership collects the canonical member IDs every selected or review unit covers.
func issue155Membership(variant *domain.DubSegmentsVariant) map[int]int {
	counts := make(map[int]int)
	for _, seg := range variant.Segments {
		for _, member := range seg.SpeechBlockIndices {
			counts[member]++
		}
	}
	for _, seg := range variant.ReviewSegments {
		for _, member := range seg.SpeechBlockIndices {
			counts[member]++
		}
	}
	return counts
}

// issue155GroupedSegment returns the single selected segment that merges several canonical
// members, or nil when the variant selected none.
func issue155GroupedSegment(variant *domain.DubSegmentsVariant) *domain.DubSegment {
	for i := range variant.Segments {
		if len(variant.Segments[i].SpeechBlockIndices) > 1 {
			return &variant.Segments[i]
		}
	}
	return nil
}

// issue155AssertFixedRatePassStaysPublished proves a failed escalated pass published nothing
// new: storage still names the completed fixed-rate pass, no half-built group reached it, and
// the whole-speaker escalation decision is durable. Both the interrupted and the cancelled
// escalated pass must uphold it, so scenario names which one failed.
func issue155AssertFixedRatePassStaysPublished(t *testing.T, h *testHarness, assetID, runID, assignmentCAS, scenario string) *domain.VoiceAssignment {
	t.Helper()
	idx, err := h.db.GetDubSegmentsVariantIndex(context.Background(), assetID, "vi")
	if err != nil || idx == nil {
		t.Fatalf("the completed fixed-rate pass must stay published: %v", err)
	}
	rc, err := h.casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("published variant missing from CAS: %v", err)
	}
	defer rc.Close()
	var published domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&published); err != nil {
		t.Fatalf("decode published variant: %v", err)
	}
	if published.OverallStatus != "REVIEW_REQUIRED" || len(published.Escalations) != 0 ||
		len(published.Segments) != 1 || len(published.ReviewSegments) != 1 {
		t.Fatalf("%s escalated pass left status=%s escalations=%d segments=%d reviews=%d published",
			scenario, published.OverallStatus, len(published.Escalations), len(published.Segments), len(published.ReviewSegments))
	}
	for _, seg := range published.Segments {
		if len(seg.SpeechBlockIndices) != 1 {
			t.Fatalf("published a half-built group: %+v", seg.SpeechBlockIndices)
		}
	}
	// The whole-speaker escalation decision itself is durable, so a retry does not have to
	// re-decide it.
	interrupted := runGetVoiceAssignment(t, h, assetID, runID)
	if interrupted.SupersedesCAS != assignmentCAS {
		t.Fatalf("the escalation decision must survive the %s, got %+v", scenario, interrupted)
	}
	return interrupted
}

// issue155Review returns the variant's single review candidate.
func issue155Review(t *testing.T, variant *domain.DubSegmentsVariant) *domain.DubSegmentReview {
	t.Helper()
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected exactly 1 review candidate, got %d (status=%s selected=%d)", len(variant.ReviewSegments), variant.OverallStatus, len(variant.Segments))
	}
	return &variant.ReviewSegments[0]
}

// issue155Plan returns the fit plan released for the unit with the given membership.
func issue155Plan(t *testing.T, variant *domain.DubSegmentsVariant, members []int) *domain.DubbingFitPlan {
	t.Helper()
	for i := range variant.FitPlans {
		if slices.Equal(variant.FitPlans[i].SpeechBlockIndices, members) {
			return &variant.FitPlans[i]
		}
	}
	t.Fatalf("no fit plan for membership %v in %+v", members, variant.FitPlans)
	return nil
}

// ---------------------------------------------------------------------------
//  1. The group is the maximal contiguous eligible prefix, capped at five canonical
//     members: the sixth adjacent same-speaker turn is a boundary, not a member, and an
//     overrunning five-member group stays whole instead of absorbing it.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_BoundedRegroupSelectsAtMostFiveMembers(t *testing.T) {
	cases := []struct {
		name          string
		durations     map[int]int64
		groupAccepted bool
	}{
		{name: "GroupFits", durations: map[int]int64{0: 1500, 5: 300}, groupAccepted: true},
		{name: "OverrunningGroupStopsAtCap", durations: map[int]int64{0: 3100, 5: 300}, groupAccepted: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHarness(t)
			jobID, runID := createJobAndRun(t, h)
			assetID := getJobViaAPI(t, h, jobID).SourceAssetID

			turns := []issue155Turn{
				{speaker: "SPEAKER_00", startMs: 0, endMs: 500, source: "第一句", spoken: "Vế một"},
				{speaker: "SPEAKER_00", startMs: 600, endMs: 1100, source: "第二句", spoken: "Vế hai"},
				{speaker: "SPEAKER_00", startMs: 1200, endMs: 1700, source: "第三句", spoken: "Vế ba"},
				{speaker: "SPEAKER_00", startMs: 1800, endMs: 2300, source: "第四句", spoken: "Vế bốn"},
				{speaker: "SPEAKER_00", startMs: 2400, endMs: 2900, source: "第五句", spoken: "Vế năm"},
				{speaker: "SPEAKER_00", startMs: 3000, endMs: 3500, source: "第六句", spoken: "Vế sáu"},
			}
			scriptCAS, dubScript := issue155PinLineage(t, h, jobID, assetID, runID,
				[]domain.AudioSegment{{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue}}, turns)

			lane := defaultVITTSFake(t, h)
			lane.CustomDurations = tc.durations

			_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
			payload := map[string]any{
				"run_id":                 runID,
				"target_language":        "vi",
				"dub_script_variant_cas": scriptCAS,
				"voice_assignment_cas":   assignment.CASHash,
			}
			resp, variant := runDubSynthesize(t, h, assetID, payload)
			if resp.StatusCode != http.StatusCreated || variant == nil {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
			}

			// Exactly three invocations: the triggering block, its one bounded group (five
			// members), and the excluded sixth block. No narrower candidate was ever probed.
			if lane.Invocations != 3 {
				t.Fatalf("expected one group synthesis, got %d lane invocations", lane.Invocations)
			}
			groupMembers := []int{0, 1, 2, 3, 4}
			var group *domain.DubSegment
			var groupReview *domain.DubSegmentReview
			if tc.groupAccepted {
				if variant.OverallStatus != "PASS" || len(variant.ReviewSegments) != 0 || len(variant.Segments) != 2 {
					t.Fatalf("expected PASS with a group and the excluded block, got status=%s selected=%d review=%d",
						variant.OverallStatus, len(variant.Segments), len(variant.ReviewSegments))
				}
				group = issue155GroupedSegment(variant)
			} else {
				if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.Segments) != 1 {
					t.Fatalf("an overrunning group must stay in review, got status=%s selected=%d",
						variant.OverallStatus, len(variant.Segments))
				}
				groupReview = issue155Review(t, variant)
			}
			members := groupMembers
			if groupReview != nil {
				members = groupReview.SpeechBlockIndices
			} else if group == nil {
				t.Fatalf("expected one grouped unit, got %+v", variant.Segments)
			} else {
				members = group.SpeechBlockIndices
			}
			if !slices.Equal(members, groupMembers) {
				t.Fatalf("expected members %v, got %v", groupMembers, members)
			}
			// The sixth turn always stays its own unit and keeps its canonical anchor.
			var tail *domain.DubSegment
			for i := range variant.Segments {
				if slices.Equal(variant.Segments[i].SpeechBlockIndices, []int{5}) {
					tail = &variant.Segments[i]
				}
			}
			if tail == nil || tail.StartMs != 3000 || tail.EndMs != 3500 {
				t.Fatalf("the sixth block must remain its own unit with its canonical anchor, got %+v", tail)
			}
			if groupReview == nil {
				if group.StartMs != dubScript.Segments[0].StartMs || group.EndMs != dubScript.Segments[4].EndMs {
					t.Fatalf("group mutated its canonical anchors: %d..%d", group.StartMs, group.EndMs)
				}
				if len(group.SourceText) == 0 || len(group.SpokenText) == 0 {
					t.Fatalf("group dropped member text: source=%q spoken=%q", group.SourceText, group.SpokenText)
				}
				for _, memberText := range []string{"第一句", "第五句"} {
					if !bytes.Contains([]byte(group.SourceText), []byte(memberText)) {
						t.Fatalf("group source text lost member %q: %q", memberText, group.SourceText)
					}
				}
			} else if groupReview.StartMs != 0 || groupReview.EndMs != 2900 {
				t.Fatalf("review group anchors drifted: %d..%d", groupReview.StartMs, groupReview.EndMs)
			}
			if counts := issue155Membership(variant); len(counts) != 6 {
				t.Fatalf("expected unique coverage of all six canonical members, got %v", counts)
			} else {
				for member, count := range counts {
					if count != 1 {
						t.Fatalf("member %d covered %d times", member, count)
					}
				}
			}
			// Append-only fit evidence: one plan per released unit, and the group's plan is
			// its own measurement with its own verdict.
			if len(variant.FitPlans) != 2 {
				t.Fatalf("expected one fit plan per released unit, got %d", len(variant.FitPlans))
			}
			var groupPlan *domain.DubbingFitPlan
			for i := range variant.FitPlans {
				if len(variant.FitPlans[i].SpeechBlockIndices) == 5 {
					groupPlan = &variant.FitPlans[i]
				}
			}
			if groupPlan == nil || groupPlan.MeasuredDurationMs != tc.durations[0] || groupPlan.AttemptCount != 1 {
				t.Fatalf("group fit evidence must record its own single measurement, got %+v", groupPlan)
			}
			wantGroupEnd := int64(0)
			wantDecision := domain.FitActionAccept
			wantReason := "regrouped same-speaker turn into combined slot"
			if groupReview != nil {
				wantGroupEnd = groupReview.DubPlaybackEndMs
				wantDecision = domain.FitActionReview
				wantReason = "unresolvable duration overrun"
			} else {
				wantGroupEnd = group.DubPlaybackEndMs
			}
			if groupPlan.DubPlaybackEndMs != wantGroupEnd || groupPlan.Decision != wantDecision ||
				!strings.Contains(groupPlan.DecisionReason, wantReason) {
				t.Fatalf("unexpected group fit evidence: %+v", groupPlan)
			}

			// A compatible completed replay reuses the published variant without new synthesis.
			respReplay, replay := runDubSynthesize(t, h, assetID, payload)
			if respReplay.StatusCode != http.StatusCreated || replay == nil {
				t.Fatalf("replay failed: %d", respReplay.StatusCode)
			}
			if replay.CASHash != variant.CASHash {
				t.Fatalf("replay must reproduce the same variant, got %s", replay.CASHash)
			}
			if lane.Invocations != 3 {
				t.Fatalf("a completed compatible replay must not synthesize again, got %d invocations", lane.Invocations)
			}
		})
	}
}

// ---------------------------------------------------------------------------
//  2. Gap boundaries: a group needs strictly positive gaps below 600ms. Zero, negative,
//     and 600ms gaps are boundaries; 599ms is eligible.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_BoundedRegroupGapBoundaries(t *testing.T) {
	cases := []struct {
		name          string
		gapMs         int64
		declaredGapMs int64
		grouped       bool
	}{
		// The script claims an eligible gap in the zero, negative and 600ms cases while
		// the pinned canonical timeline shows 0, an overlap and exactly 600ms: the group
		// must follow the canonical timing.
		{name: "ZeroGap", gapMs: 0, declaredGapMs: 100, grouped: false},
		{name: "NegativeGap", gapMs: -50, declaredGapMs: 100, grouped: false},
		{name: "599ms", gapMs: 599, grouped: true},
		{name: "600ms", gapMs: 600, declaredGapMs: 599, grouped: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHarness(t)
			jobID, runID := createJobAndRun(t, h)
			assetID := getJobViaAPI(t, h, jobID).SourceAssetID

			secondStart := int64(1000) + tc.gapMs
			turns := []issue155Turn{
				{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một", declaredGapMs: tc.declaredGapMs},
				{speaker: "SPEAKER_00", startMs: secondStart, endMs: secondStart + 1000, source: "第二句", spoken: "Vế hai"},
			}
			scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
				[]domain.AudioSegment{{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue}}, turns)

			lane := defaultVITTSFake(t, h)
			lane.CustomDurations = map[int]int64{0: 1500, 1: 800}

			_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
			resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
				"run_id":                 runID,
				"target_language":        "vi",
				"dub_script_variant_cas": scriptCAS,
				"voice_assignment_cas":   assignment.CASHash,
			})
			if resp.StatusCode != http.StatusCreated || variant == nil {
				t.Fatalf("dub-synthesize failed: %d", resp.StatusCode)
			}

			// Both branches cost exactly two invocations: one measurement for the
			// triggering block and one for whatever unit the pass released.
			if lane.Invocations != 2 {
				t.Fatalf("expected 2 lane invocations, got %d", lane.Invocations)
			}
			if tc.grouped {
				group := issue155GroupedSegment(variant)
				if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1}) {
					t.Fatalf("gap %dms must be groupable, got segments %+v", tc.gapMs, variant.Segments)
				}
				if group.StartMs != 0 || group.EndMs != secondStart+1000 {
					t.Fatalf("group anchors drifted: %d..%d", group.StartMs, group.EndMs)
				}
				if len(variant.ReviewSegments) != 0 {
					t.Fatalf("grouped case must not retain review evidence: %+v", variant.ReviewSegments)
				}
				return
			}
			if issue155GroupedSegment(variant) != nil {
				t.Fatalf("gap %dms must not group, got %+v", tc.gapMs, variant.Segments)
			}
			review := issue155Review(t, variant)
			if !slices.Equal(review.SpeechBlockIndices, []int{0}) || review.StartMs != 0 || review.EndMs != 1000 {
				t.Fatalf("expected the triggering block alone in review, got %+v", review)
			}
			// The script declares a mergeable gap while the canonical timeline refuses it, so
			// the pass requested a regroup it could not act on: the fit plan must record the
			// REVIEW verdict and its own reason, never the unactionable REGROUP request.
			plan := issue155Plan(t, variant, []int{0})
			if plan.Decision != review.FitDecision || plan.Decision != domain.FitActionReview ||
				!strings.Contains(plan.DecisionReason, "no eligible bounded same-speaker group") {
				t.Fatalf("the fit plan must record the ungroupable verdict, got %+v / %+v", plan, review)
			}
			if len(variant.Segments) != 1 || !slices.Equal(variant.Segments[0].SpeechBlockIndices, []int{1}) {
				t.Fatalf("the second turn must stay its own unit, got %+v", variant.Segments)
			}
		})
	}
}

// ---------------------------------------------------------------------------
//  3. Group boundaries come from the canonical timeline: another speaker, protected
//     singing/uncertain vocals and an intervening canonical block stop a group, while
//     ordinary BGM/SFX/ambience is not a barrier.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_BoundedRegroupBoundaries(t *testing.T) {
	dialogue := func(startMs, endMs int64) domain.AudioSegment {
		return domain.AudioSegment{StartMs: startMs, EndMs: endMs, Role: domain.AudioRoleNarrationDialogue}
	}
	cases := []struct {
		name            string
		secondSpeaker   string
		gapRole         domain.AudioRole
		expectedGroup   []int
		tailIndex       int
		wantInvocations int
		refused         bool
		hiddenMiddle    bool
		interleaved     bool
	}{
		{name: "OtherSpeaker", secondSpeaker: "SPEAKER_01", tailIndex: 1, wantInvocations: 2},
		{name: "SingingVocal", gapRole: domain.AudioRoleSingingMusicVocal, tailIndex: 1, wantInvocations: 2},
		// The router refuses a lane whose plan carries uncertain audio before any
		// synthesis, so an uncertain gap can never be merged across either.
		{name: "UncertainVocal", gapRole: domain.AudioRoleUncertain, refused: true},
		{name: "OrdinaryBgm", gapRole: domain.AudioRoleInstrumentalBgm, expectedGroup: []int{0, 1}, wantInvocations: 2},
		{name: "OrdinarySfx", gapRole: domain.AudioRoleAmbienceSFX, expectedGroup: []int{0, 1}, wantInvocations: 2},
		// A dub-ineligible source turn sits in the gap and is filtered out of the dub
		// script, so only the canonical timeline shows the boundary.
		{name: "InterveningCanonicalTurn", gapRole: domain.AudioRoleInstrumentalBgm, tailIndex: 2, wantInvocations: 2, hiddenMiddle: true},
		// The speaker boundary after one merged member is only visible to the bounded
		// scan: the trigger's own next turn is still the same speaker.
		{name: "SpeakerBoundaryAfterOneMember", expectedGroup: []int{0, 1}, tailIndex: 2, wantInvocations: 3, interleaved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHarness(t)
			jobID, runID := createJobAndRun(t, h)
			assetID := getJobViaAPI(t, h, jobID).SourceAssetID

			secondSpeaker := tc.secondSpeaker
			if secondSpeaker == "" {
				secondSpeaker = "SPEAKER_00"
			}
			plan := []domain.AudioSegment{dialogue(0, 1000)}
			if tc.gapRole != "" {
				plan = append(plan, domain.AudioSegment{StartMs: 1000, EndMs: 1100, Role: tc.gapRole})
			}
			plan = append(plan, dialogue(1100, 4000))

			turns := []issue155Turn{
				{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
			}
			switch {
			case tc.hiddenMiddle:
				turns = append(turns, issue155Turn{
					speaker: "SPEAKER_00", startMs: 1000, endMs: 1100, source: "插入句",
					spoken: "Vế chèn", scriptAbsent: true,
				}, issue155Turn{
					speaker: "SPEAKER_00", startMs: 1100, endMs: 2000, source: "第二句", spoken: "Vế hai",
				})
			case tc.interleaved:
				turns = append(turns, issue155Turn{
					speaker: "SPEAKER_00", startMs: 1100, endMs: 2000, source: "第二句", spoken: "Vế hai",
				}, issue155Turn{
					speaker: "SPEAKER_01", startMs: 2100, endMs: 3000, source: "第三句", spoken: "Vế ba",
				})
			default:
				turns = append(turns, issue155Turn{
					speaker: secondSpeaker, startMs: 1100, endMs: 2000, source: "第二句", spoken: "Vế hai",
				})
			}
			scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID, plan, turns)

			lane := defaultVITTSFake(t, h)
			lane.CustomDurations = map[int]int64{0: 1500, 1: 800, 2: 800}

			_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
			resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
				"run_id":                 runID,
				"target_language":        "vi",
				"dub_script_variant_cas": scriptCAS,
				"voice_assignment_cas":   assignment.CASHash,
			})
			if tc.refused {
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode == http.StatusCreated || !strings.Contains(string(body), "uncertain") {
					t.Fatalf("an uncertain audio role must be refused before synthesis, got %d body=%s", resp.StatusCode, string(body))
				}
				if lane.Invocations != 0 {
					t.Fatalf("a refused uncertain-role run must not synthesize, got %d invocations", lane.Invocations)
				}
				return
			}
			if resp.StatusCode != http.StatusCreated || variant == nil {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
			}
			// One measurement per released unit: the triggering block, its group where one
			// was formed, and any turn the script still carries after it.
			if lane.Invocations != tc.wantInvocations {
				t.Fatalf("expected %d lane invocations, got %d", tc.wantInvocations, lane.Invocations)
			}
			group := issue155GroupedSegment(variant)
			if tc.expectedGroup == nil {
				if group != nil {
					t.Fatalf("%s must stop the group, got %+v", tc.name, group)
				}
				review := issue155Review(t, variant)
				if !slices.Equal(review.SpeechBlockIndices, []int{0}) {
					t.Fatalf("expected the triggering block alone in review, got %v", review.SpeechBlockIndices)
				}
				// The plan and the review candidate agree on the verdict for this unit.
				plan := issue155Plan(t, variant, []int{0})
				if review.FitDecision != domain.FitActionReview || plan.Decision != review.FitDecision {
					t.Fatalf("a refused group must be review evidence in both records, got %+v / %+v", plan, review)
				}
				if tc.hiddenMiddle && !strings.Contains(plan.DecisionReason, "no eligible bounded same-speaker group") {
					t.Fatalf("a regroup the canonical timeline refused must not survive as the fit verdict, got %+v", plan)
				}
			} else if group == nil || !slices.Equal(group.SpeechBlockIndices, tc.expectedGroup) {
				t.Fatalf("%s must group %v, got %+v", tc.name, tc.expectedGroup, variant.Segments)
			}
			if tc.tailIndex > 0 {
				var tail *domain.DubSegment
				for i := range variant.Segments {
					if slices.Equal(variant.Segments[i].SpeechBlockIndices, []int{tc.tailIndex}) {
						tail = &variant.Segments[i]
					}
				}
				if tail == nil {
					t.Fatalf("canonical member %d must stay its own unit, got %+v", tc.tailIndex, variant.Segments)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
//  4. A rejected group stays whole: the review candidate carries the exact membership,
//     the group's own measured audio and the final member's playback boundary.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_RejectedGroupKeepsWholeMembershipEvidence(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 1500, source: "第二句", spoken: "Vế hai"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	lane := defaultVITTSFake(t, h)
	// The merged turn cannot fit its combined window either: 1800ms of speech for 1500ms.
	lane.CustomDurations = map[int]int64{0: 1800}

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}
	if lane.Invocations != 2 {
		t.Fatalf("expected one triggering measurement and one group measurement, got %d", lane.Invocations)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.Segments) != 0 {
		t.Fatalf("an overrunning group must never be selected, got status=%s selected=%d", variant.OverallStatus, len(variant.Segments))
	}
	review := issue155Review(t, variant)
	if !slices.Equal(review.SpeechBlockIndices, []int{0, 1}) {
		t.Fatalf("review candidate must cover the whole group, got %v", review.SpeechBlockIndices)
	}
	if review.StartMs != 0 || review.EndMs != 1500 || review.SlotDurationMs != 1500 {
		t.Fatalf("review candidate anchors drifted: %d..%d slot %d", review.StartMs, review.EndMs, review.SlotDurationMs)
	}
	if review.MeasuredDurationMs != 1800 || review.DubPlaybackEndMs != 1500 {
		t.Fatalf("review candidate must keep its own measurement and the final member's boundary, got measured=%d playback_end=%d",
			review.MeasuredDurationMs, review.DubPlaybackEndMs)
	}
	if review.AudioSHA256 == "" {
		t.Fatalf("the rejected group's own waveform must remain actionable review evidence")
	}
	if !bytes.Contains([]byte(review.SourceText), []byte("第二句")) || !bytes.Contains([]byte(review.SpokenText), []byte("Vế hai")) {
		t.Fatalf("review candidate cropped member text: source=%q spoken=%q", review.SourceText, review.SpokenText)
	}
	counts := issue155Membership(variant)
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("expected unique coverage of the whole group, got %v", counts)
	}
	// The plan carries the group's own single synthesis and the playback window it was
	// measured against, not the triggering block's earlier attempts.
	plan := issue155Plan(t, variant, []int{0, 1})
	if plan.AttemptCount != 1 || plan.SlotDurationMs != plan.DubPlaybackEndMs-review.StartMs {
		t.Fatalf("unexpected group fit evidence: %+v", plan)
	}
}

// ---------------------------------------------------------------------------
//  5. The group is chosen before one combined synthesis: a five-member window is probed
//     exactly once instead of growing through narrower synth-and-grow candidates.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_GroupIsChosenBeforeOneCombinedSynthesis(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 500, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 600, endMs: 1100, source: "第二句", spoken: "Vế hai"},
		{speaker: "SPEAKER_00", startMs: 1200, endMs: 1700, source: "第三句", spoken: "Vế ba"},
		{speaker: "SPEAKER_00", startMs: 1800, endMs: 2300, source: "第四句", spoken: "Vế bốn"},
		{speaker: "SPEAKER_00", startMs: 2400, endMs: 4400, source: "第五句", spoken: "Vế năm"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	lane := defaultVITTSFake(t, h)
	// 1500ms fits the five-member window but not the first, second or third member windows.
	lane.CustomDurations = map[int]int64{0: 1500}

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}
	if lane.Invocations != 2 {
		t.Fatalf("expected the trigger plus exactly one combined group synthesis, got %d", lane.Invocations)
	}
	group := issue155GroupedSegment(variant)
	if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("expected the maximal five-member prefix in one group, got %+v", variant.Segments)
	}
	if len(variant.ReviewSegments) != 0 || variant.OverallStatus != "PASS" {
		t.Fatalf("expected the single combined group to be selected, got status=%s review=%d",
			variant.OverallStatus, len(variant.ReviewSegments))
	}
}

// ---------------------------------------------------------------------------
//  6. A member's accepted rewrite is carried into the group it is later absorbed by, and
//     the escalated pass replays the chosen topology without refilling a spent remedy.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_MemberRewriteCarriesIntoAbsorbingGroup(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	const (
		originalText  = "Hôm nay thời tiết thật sự rất đẹp."
		rewrittenText = "Hôm nay trời rất đẹp."
	)
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 2100, source: "今天天气真的非常好。", spoken: originalText},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	// The fixed-rate lane fits the first turn and cannot fit the second one naturally.
	lane := defaultVITTSFake(t, h)
	lane.CustomDurations = map[int]int64{0: 900, 1: 1500}

	adaptCalls := 0
	h.dubbingSvc.ConfigureSpokenAdapter(seam1SpokenAdapterFunc(func(_ context.Context, _ provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		adaptCalls++
		return &provider.SpokenScriptAdaptationResult{SpokenText: rewrittenText}, nil
	}))
	fallback := registerCosyVoiceFallback(t, h, 900, true)
	fallback.CustomDurations = map[int]int64{0: 1800}

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}

	if adaptCalls != 1 {
		t.Fatalf("the lineage rewrite budget must stay spent across escalation, adapter called %d times", adaptCalls)
	}
	if len(variant.Escalations) != 1 || variant.Escalations[0].SpeakerID != "SPEAKER_00" {
		t.Fatalf("expected one whole-speaker escalation, got %+v", variant.Escalations)
	}
	if !slices.Equal(variant.Escalations[0].TriggerSegmentIndices, []int{1}) {
		t.Fatalf("the overrunning rewritten member is the recorded trigger, got %v", variant.Escalations[0].TriggerSegmentIndices)
	}
	if fallback.Invocations != 2 {
		t.Fatalf("the escalated pass measures the trigger and probes the chosen group once each, got %d invocations", fallback.Invocations)
	}
	group := issue155GroupedSegment(variant)
	if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1}) {
		t.Fatalf("the escalated pass must replay the bounded group topology, got %+v", variant.Segments)
	}
	// The regression: merging must not drop the co-member's accepted rewrite.
	if group.SpokenText != "Vế một "+rewrittenText {
		t.Fatalf("the absorbed member's accepted rewrite was dropped: %q", group.SpokenText)
	}
	if variant.Escalations[0].Resolved != true || variant.OverallStatus != "PASS" {
		t.Fatalf("the escalated group must resolve the overrun, got %+v status=%s", variant.Escalations[0], variant.OverallStatus)
	}
	if len(variant.Segments) != 1 {
		t.Fatalf("expected the single escalated group selection, got %d segments", len(variant.Segments))
	}
}

// ---------------------------------------------------------------------------
//  7. On a rate-capable lane, a consumed native attempt and the consumed group remedy are
//     never replenished: the group is measured once and no further remedy follows it.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_ConsumedNativeAndGroupRemediesAreNotReplenished(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 2000, source: "第二句", spoken: "Vế hai"},
		{speaker: "SPEAKER_00", startMs: 2100, endMs: 4200, source: "第三句", spoken: "Vế ba"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	fittedVoice := provider.VieNeuPresetVoices()[0]
	_, assignment := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":             runID,
		"target_language":    "vi",
		"custom_assignments": map[string]domain.VoiceProfile{"SPEAKER_00": fittedVoice},
	})
	if assignment == nil {
		t.Fatal("voice assignment failed")
	}
	cfg := service.DefaultFitControllerConfig()
	cfg.NativeSpeedEnvelopes = []domain.NativeSpeedEnvelope{{
		ProviderID:     fittedVoice.ProviderID,
		ModelID:        fittedVoice.ProviderID,
		ModelVersion:   "1.0",
		VoiceProfileID: fittedVoice.ID,
		MinSpeed:       0.8,
		MaxSpeed:       1.5,
		Verified:       true,
		CalibrationID:  "cal-155-native",
	}}
	h.dubbingSvc.ConfigureFitController(service.NewFitController(cfg))

	var speeds []float64
	h.dubbingSvc.TTSInvoke = func(_ context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		speeds = append(speeds, req.Speed)
		// The natural candidate and the single native retry both overrun the triggering
		// slot; only the bounded group synthesised at natural speed fits its window.
		durMs := int64(1500)
		if len(speeds) == 3 {
			durMs = 1700
		}
		wave := media.GeneratePCM16WAV(16000, 1, durMs)
		sum := sha256.Sum256(wave)
		return &provider.TTSSynthesisResult{
			AudioData:          wave,
			AudioSHA256:        hex.EncodeToString(sum[:]),
			ProviderID:         p.ID(),
			MeasuredDurationMs: durMs,
		}, nil
	}

	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}

	if len(speeds) != 3 {
		t.Fatalf("expected natural, one native retry and one group synthesis, got %v", speeds)
	}
	if speeds[0] != 1.0 || speeds[1] <= 1.0 || speeds[2] != 1.0 {
		t.Fatalf("unexpected synthesis speed sequence: %v", speeds)
	}
	group := issue155GroupedSegment(variant)
	if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1, 2}) {
		t.Fatalf("expected one bounded three-member group, got %+v", variant.Segments)
	}
	if variant.OverallStatus != "PASS" || len(variant.ReviewSegments) != 0 {
		t.Fatalf("expected the group to resolve every member, got status=%s review=%d",
			variant.OverallStatus, len(variant.ReviewSegments))
	}
	// The group is the last remedy: no resize, rewrite or regroup followed it, and a
	// rate-capable lineage never escalates.
	if len(variant.Escalations) != 0 {
		t.Fatalf("a rate-capable lineage must not escalate, got %+v", variant.Escalations)
	}
	if len(variant.FitPlans) != 1 || len(variant.FitPlans[0].SpeechBlockIndices) != 3 {
		t.Fatalf("expected exactly the group's fit evidence, got %+v", variant.FitPlans)
	}
}

// ---------------------------------------------------------------------------
//  8. An overrunning bounded group is the last remedy: it is never resized by a reopened
//     speed attempt nor rewritten, and its whole membership stays review evidence.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_OverrunningGroupIsNotResizedOrRewritten(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 2000, source: "第二句", spoken: "Vế hai"},
		{speaker: "SPEAKER_00", startMs: 2100, endMs: 3000, source: "第三句", spoken: "Vế ba"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	fittedVoice := provider.VieNeuPresetVoices()[0]
	_, assignment := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":             runID,
		"target_language":    "vi",
		"custom_assignments": map[string]domain.VoiceProfile{"SPEAKER_00": fittedVoice},
	})
	if assignment == nil {
		t.Fatal("voice assignment failed")
	}
	// The verified envelope stays available to every measurement of this lineage, so the
	// group's own overrun could be answered by a speed resynthesis if that remedy were
	// still open to it.
	cfg := service.DefaultFitControllerConfig()
	cfg.NativeSpeedEnvelopes = []domain.NativeSpeedEnvelope{{
		ProviderID:     fittedVoice.ProviderID,
		ModelID:        fittedVoice.ProviderID,
		ModelVersion:   "1.0",
		VoiceProfileID: fittedVoice.ID,
		MinSpeed:       0.9,
		MaxSpeed:       1.5,
		Verified:       true,
		CalibrationID:  "cal-155-last-remedy",
	}}
	h.dubbingSvc.ConfigureFitController(service.NewFitController(cfg))

	adapterCalls := 0
	h.dubbingSvc.ConfigureSpokenAdapter(seam1SpokenAdapterFunc(func(_ context.Context, req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		adapterCalls++
		return &provider.SpokenScriptAdaptationResult{SpokenText: req.MeaningText}, nil
	}))

	var speeds []float64
	h.dubbingSvc.TTSInvoke = func(_ context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		speeds = append(speeds, req.Speed)
		// The trigger would need 1.52x, outside the verified envelope, so only grouping
		// remains; the group itself then overruns its own combined window.
		durMs := int64(1600)
		if len(speeds) > 1 {
			durMs = 3100
		}
		wave := media.GeneratePCM16WAV(16000, 1, durMs)
		sum := sha256.Sum256(wave)
		return &provider.TTSSynthesisResult{
			AudioData:          wave,
			AudioSHA256:        hex.EncodeToString(sum[:]),
			ProviderID:         p.ID(),
			MeasuredDurationMs: durMs,
		}, nil
	}

	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}
	if len(speeds) != 2 || speeds[0] != 1.0 || speeds[1] != 1.0 {
		t.Fatalf("an overrunning group must not reopen a speed attempt: %v", speeds)
	}
	if adapterCalls != 0 {
		t.Fatalf("an overrunning group must not reopen a rewrite, adapter called %d times", adapterCalls)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.Segments) != 0 {
		t.Fatalf("expected the whole group in review, got status=%s selected=%d", variant.OverallStatus, len(variant.Segments))
	}
	review := issue155Review(t, variant)
	if !slices.Equal(review.SpeechBlockIndices, []int{0, 1, 2}) {
		t.Fatalf("review candidate must cover the whole group, got %v", review.SpeechBlockIndices)
	}
	if review.SlotDurationMs != 3000 || review.MeasuredDurationMs != 3100 || review.FitDecision != domain.FitActionReview {
		t.Fatalf("unexpected group review evidence: %+v", review)
	}
	if len(variant.FitPlans) != 1 || variant.FitPlans[0].Decision != domain.FitActionReview ||
		!slices.Equal(variant.FitPlans[0].SpeechBlockIndices, []int{0, 1, 2}) {
		t.Fatalf("expected one review fit plan for the whole group, got %+v", variant.FitPlans)
	}
	if !strings.Contains(variant.FitPlans[0].DecisionReason, "unresolvable duration overrun") {
		t.Fatalf("the group's fit verdict must be the honest overrun, got %q", variant.FitPlans[0].DecisionReason)
	}
}

// ---------------------------------------------------------------------------
//  9. An interrupted escalated pass publishes nothing: the escalation decision stays
//     durable, and a later retry re-enters the same attempt semantics from scratch.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_InterruptedEscalatedPassPublishesNoPartialOutput(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	const (
		originalText  = "Hôm nay thời tiết thật sự rất đẹp."
		rewrittenText = "Hôm nay trời rất đẹp."
	)
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 2100, source: "今天天气真的非常好。", spoken: originalText},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	lane := defaultVITTSFake(t, h)
	lane.CustomDurations = map[int]int64{0: 900, 1: 1500}
	adaptCalls := 0
	h.dubbingSvc.ConfigureSpokenAdapter(seam1SpokenAdapterFunc(func(_ context.Context, _ provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		adaptCalls++
		return &provider.SpokenScriptAdaptationResult{SpokenText: rewrittenText}, nil
	}))
	fallback := registerCosyVoiceFallback(t, h, 900, true)
	fallback.InjectError = errors.New("fallback lane interrupted")

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	payload := map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	}
	resp, variant := runDubSynthesize(t, h, assetID, payload)
	if resp.StatusCode < 400 || variant != nil {
		t.Fatalf("an interrupted escalated pass must fail the request, got %d", resp.StatusCode)
	}
	// No canonical partial output: the interrupted escalated pass publishes nothing new,
	// so storage still names the completed fixed-rate pass - never a half-built group.
	interrupted := issue155AssertFixedRatePassStaysPublished(t, h, assetID, runID, assignment.CASHash, "interrupted")

	// The retry re-enters under the durable escalation: the interrupted attempt's rewrite is
	// not re-earned, the bounded group completes, and the assignment is not re-minted.
	fallback.InjectError = nil
	fallback.CustomDurations = map[int]int64{0: 1800}
	respRetry, retried := runDubSynthesize(t, h, assetID, payload)
	if respRetry.StatusCode != http.StatusCreated || retried == nil {
		body, _ := io.ReadAll(respRetry.Body)
		t.Fatalf("retry failed: %d body=%s", respRetry.StatusCode, string(body))
	}
	group := issue155GroupedSegment(retried)
	if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1}) {
		t.Fatalf("the retry must complete the bounded group, got %+v", retried.Segments)
	}
	if retried.OverallStatus != "PASS" || len(retried.Escalations) != 1 || !retried.Escalations[0].Resolved ||
		retried.Escalations[0].SupersededAssignmentCAS != assignment.CASHash {
		t.Fatalf("unexpected retried variant: status=%s escalations=%+v", retried.OverallStatus, retried.Escalations)
	}
	if retried.VoiceAssignmentCAS != interrupted.CASHash {
		t.Fatalf("the retry must reuse the durable escalation, got %s", retried.VoiceAssignmentCAS)
	}
	if adaptCalls != 1 || lane.Invocations != 3 {
		t.Fatalf("the retry must not re-enter the fixed-rate lane or re-earn its rewrite: adapter=%d fixedRateInvocations=%d",
			adaptCalls, lane.Invocations)
	}
}

// ---------------------------------------------------------------------------
//  10. A merged group text that fails meaning QA is never synthesized: its review evidence
//     keeps the playback window it was chosen against and marks the measurement absent
//     instead of inventing one.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_QARefusedGroupKeepsKnownWindowEvidence(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	// The merged source carries a number the merged target drops, so the group fails the
	// meaning/glossary gate before any synthesis runs.
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 2100, source: "2024年第二句", spoken: "Vế hai"},
		{speaker: "SPEAKER_01", startMs: 4000, endMs: 5000, source: "第三句", spoken: "Vế ba"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	lane := defaultVITTSFake(t, h)
	lane.CustomDurations = map[int]int64{0: 1500, 1: 800, 2: 500}

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	if resp.StatusCode != http.StatusCreated || variant == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize failed: %d body=%s", resp.StatusCode, string(body))
	}
	// The refused group spends no synthesis: only the triggering block and the trailing
	// unaffected turn were measured.
	if lane.Invocations != 2 {
		t.Fatalf("a QA-refused group must not be synthesized, got %d lane invocations", lane.Invocations)
	}
	review := issue155Review(t, variant)
	if !slices.Equal(review.SpeechBlockIndices, []int{0, 1}) {
		t.Fatalf("the refused group must stay whole in review, got %v", review.SpeechBlockIndices)
	}
	if review.MeasuredDurationMs != 0 || review.AudioSHA256 != "" || review.AudioCASPath != "" {
		t.Fatalf("an unmeasured group must not claim audio or a measurement: %+v", review)
	}
	if !strings.Contains(review.ReviewReason, "2024") {
		t.Fatalf("the review reason must report the QA violation, got %q", review.ReviewReason)
	}
	// The source span is still recorded, and the known playback window survives with it.
	if review.StartMs != 0 || review.EndMs != 2100 || review.SlotDurationMs != 2100 ||
		review.DubPlaybackEndMs <= review.EndMs || review.EffectiveReserveMs <= 0 {
		t.Fatalf("an unmeasured group keeps its known source span and playback window, got %+v", review)
	}
	plan := issue155Plan(t, variant, []int{0, 1})
	if plan.Decision != domain.FitActionReview || plan.MeasuredDurationMs != 0 || plan.DurationDeltaMs != 0 || plan.AttemptCount != 0 {
		t.Fatalf("the plan must mark the measurement absent, got %+v", plan)
	}
	if plan.DubPlaybackEndMs != review.DubPlaybackEndMs || plan.EffectiveReserveMs != review.EffectiveReserveMs ||
		plan.FitPolicyID == "" || plan.SlotDurationMs != plan.DubPlaybackEndMs-review.StartMs {
		t.Fatalf("the plan must keep the known window evidence, got %+v", plan)
	}
	// Known window evidence is distinguishable from a measurement: the plan holds the wider
	// playback window while the review candidate holds the source span it covers.
	if plan.SlotDurationMs == review.SlotDurationMs || plan.UsableSlotMs <= 0 || plan.UsableSlotMs > plan.SlotDurationMs {
		t.Fatalf("expected the plan's window and the review's span to be distinct known evidence, got %+v / %+v", plan, review)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("expected REVIEW_REQUIRED, got %s", variant.OverallStatus)
	}
}

// ---------------------------------------------------------------------------
//  11. The DubSegments cache identity moves with the bounded-regroup semantics: a variant a
//     pre-#155 build persisted for this exact lineage cannot satisfy a #155 request.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_Pre155CachedVariantCannotSatisfyTheBoundedRegroupContract(t *testing.T) {
	const preIssue155DubSegmentsSchemaVersion = 4

	h := setupHarness(t)
	ctx := context.Background()

	// One synthesis in a throwaway harness publishes the fit policy identity this build
	// stamps, so the legacy stage identity below is rebuilt exactly as a pre-#155 build
	// recorded it.
	probeHarness := setupHarness(t)
	probeJobID, probeRunID := createJobAndRun(t, probeHarness)
	probeAssetID := getJobViaAPI(t, probeHarness, probeJobID).SourceAssetID
	probeScriptCAS, _ := issue155PinLineage(t, probeHarness, probeJobID, probeAssetID, probeRunID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue}},
		[]issue155Turn{{speaker: "SPEAKER_00", startMs: 0, endMs: 800, source: "第一句", spoken: "Vế một"}})
	probeLane := defaultVITTSFake(t, probeHarness)
	probeLane.CustomDurations = map[int]int64{0: 500}
	_, probeAssignment := runAssignVoices(t, probeHarness, probeAssetID, map[string]any{"run_id": probeRunID, "target_language": "vi"})
	respProbe, probe := runDubSynthesize(t, probeHarness, probeAssetID, map[string]any{
		"run_id":                 probeRunID,
		"target_language":        "vi",
		"dub_script_variant_cas": probeScriptCAS,
		"voice_assignment_cas":   probeAssignment.CASHash,
	})
	if respProbe.StatusCode != http.StatusCreated || probe == nil || probe.FitPolicyID == "" {
		body, _ := io.ReadAll(respProbe.Body)
		t.Fatalf("probe dub-synthesize failed: %d body=%s", respProbe.StatusCode, string(body))
	}

	jobID, runID := createJobAndRun(t, h)
	lane := defaultVITTSFake(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID
	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 500, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 600, endMs: 1100, source: "第二句", spoken: "Vế hai"},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue}}, turns)
	// The triggering block overruns its own slot, while the merged group fits its window.
	lane.CustomDurations = map[int]int64{0: 900, 1: 300}

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	payload := map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	}

	// Rebuild the pre-#155 stage identity for this exact lineage: same inputs and fit
	// semantics, one schema version earlier.
	transcriptIdx, err := h.db.GetTranscriptArtifactIndexByRun(ctx, runID)
	if err != nil || transcriptIdx == nil {
		t.Fatalf("expected a pinned transcript: %v", err)
	}
	rolePlanCAS, err := h.db.GetStageArtifactHash(ctx, runID, "audio_role_plan")
	if err != nil || rolePlanCAS == "" {
		t.Fatalf("expected a pinned audio role plan: %v", err)
	}
	legacyProvenance, err := cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage: string(provider.TypeTTS),
		InputHashes: []string{
			scriptCAS, assignment.CASHash, transcriptIdx.CASHash, rolePlanCAS,
		},
		SemanticConfig: map[string]any{
			"zero_overrun_fit":     "measured_playback_window_v2",
			"fit_policy_id":        probe.FitPolicyID,
			"tts_runtime_identity": provider.TTSRuntimeIdentities(),
		},
		Language:      "vi",
		SchemaVersion: preIssue155DubSegmentsSchemaVersion,
	})
	if err != nil {
		t.Fatalf("compute pre-#155 stage identity: %v", err)
	}
	legacy := domain.DubSegmentsVariant{
		ID:                  "legacy-pre-155-dub-segments",
		SchemaVersion:       preIssue155DubSegmentsSchemaVersion,
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignment.CASHash,
		OverallStatus:       "REVIEW_REQUIRED",
		ProvenanceHash:      legacyProvenance,
		CreatedAt:           time.Now().UTC(),
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal pre-#155 variant: %v", err)
	}
	legacyObj, err := h.casStore.Put(bytes.NewReader(legacyData))
	if err != nil {
		t.Fatalf("put pre-#155 variant: %v", err)
	}
	if err := h.db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             legacy.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        legacyObj.SHA256,
		ProvenanceHash: legacy.ProvenanceHash,
		OverallStatus:  legacy.OverallStatus,
		CreatedAt:      legacy.CreatedAt,
	}); err != nil {
		t.Fatalf("index pre-#155 variant: %v", err)
	}
	published := lane.Invocations

	resp, replay := runDubSynthesize(t, h, assetID, payload)
	if resp.StatusCode != http.StatusCreated || replay == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dub-synthesize after the legacy row failed: %d body=%s", resp.StatusCode, string(body))
	}
	if replay.CASHash == legacyObj.SHA256 {
		t.Fatalf("a pre-#155 cached variant satisfied a #155 request: the DubSegments cache identity was not versioned")
	}
	if replay.SchemaVersion != domain.DubSegmentsSchemaVersion {
		t.Fatalf("expected the current schema version, got %d", replay.SchemaVersion)
	}
	if lane.Invocations <= published {
		t.Fatalf("the #155 contract must be honoured from scratch, lane invocations stayed at %d", lane.Invocations)
	}
	group := issue155GroupedSegment(replay)
	if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1}) {
		t.Fatalf("expected a freshly synthesized bounded group, got %+v", replay.Segments)
	}
	if replay.ProvenanceHash == legacy.ProvenanceHash {
		t.Fatalf("the persisted identity must move with the schema, got %s", replay.ProvenanceHash)
	}
}

// ---------------------------------------------------------------------------
//  12. A cancelled escalated pass publishes nothing: the completed fixed-rate pass stays the
//     run's artifact, the escalation decision stays durable, and no group is half-built.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_CancelledEscalatedPassPublishesNoPartialOutput(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	assetID := getJobViaAPI(t, h, jobID).SourceAssetID

	turns := []issue155Turn{
		{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
		{speaker: "SPEAKER_00", startMs: 1100, endMs: 2100, source: "今天天气真的非常好。", spoken: "Hôm nay thời tiết thật sự rất đẹp."},
	}
	scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
		[]domain.AudioSegment{{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue}}, turns)

	lane := defaultVITTSFake(t, h)
	lane.CustomDurations = map[int]int64{0: 900, 1: 1500}
	h.dubbingSvc.ConfigureSpokenAdapter(seam1SpokenAdapterFunc(func(_ context.Context, _ provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		return &provider.SpokenScriptAdaptationResult{SpokenText: "Hôm nay trời rất đẹp."}, nil
	}))

	// The escalated lane cancels the request context on its first synthesis, so the second
	// pass is interrupted deterministically rather than by timing.
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := &issue155CancellingProvider{FakeTTSProvider: registerCosyVoiceFallback(t, h, 900, true), cancel: cancel}
	if err := h.registry.Register(cancelling); err != nil {
		t.Fatalf("register cancelling fallback provider: %v", err)
	}

	_, assignment := runAssignVoices(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	body, _ := json.Marshal(map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": scriptCAS,
		"voice_assignment_cas":   assignment.CASHash,
	})
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, h.server.URL+"/api/v1/assets/"+assetID+"/dub-synthesize", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build dub-synthesize request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("a cancelled escalated pass must fail the request, got %d", resp.StatusCode)
		}
	}
	if cancelling.calls == 0 {
		t.Fatal("the escalated pass was never interrupted on its own lane")
	}
	// No canonical partial output: the completed fixed-rate pass stays published, with no
	// half-built group and no escalation evidence an escalated regeneration never produced.
	issue155AssertFixedRatePassStaysPublished(t, h, assetID, runID, assignment.CASHash, "cancelled")
}

// issue155CancellingProvider cancels the request context when the escalated lane is asked
// to synthesize, so an interrupted second pass is deterministic.
type issue155CancellingProvider struct {
	*provider.FakeTTSProvider
	cancel func()
	calls  int
}

func (p *issue155CancellingProvider) SynthesizeSpeech(_ context.Context, _ provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
	p.calls++
	p.cancel()
	// The cancelled request context above is what the service observes; the provider ctx may
	// not have propagated cancellation yet, so the interpreter reports it deterministically
	// instead of relying on a race that can yield (nil, nil).
	return nil, context.Canceled
}

// ---------------------------------------------------------------------------
//  13. Unaffected-speaker reuse and the same-voice-for-all rule survive a bounded-regroup
//     escalation.
//
// ---------------------------------------------------------------------------
func TestSeam1_Issue155_EscalationPreservesUnaffectedSpeakerAndSharedVoice(t *testing.T) {
	for _, tc := range []struct {
		name              string
		shared            bool
		fallbackInvocated int
		escalations       int
	}{
		// The escalated pass replays the bounded group the first pass already chose, so the
		// escalated speaker costs exactly one call instead of re-deriving the regroup after
		// probing the lone trigger, and the unaffected speaker's already-produced waveform is
		// reused instead of resynthesized.
		{name: "ScopedReuse", shared: false, fallbackInvocated: 1, escalations: 1},
		// Same-voice-for-all pins every speaker to the fallback lane, so the carried
		// speaker's own turn is regenerated on that lane exactly once: group plus turn.
		{name: "SameVoiceForAll", shared: true, fallbackInvocated: 2, escalations: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHarness(t)
			jobID, runID := createJobAndRun(t, h)
			assetID := getJobViaAPI(t, h, jobID).SourceAssetID

			turns := []issue155Turn{
				{speaker: "SPEAKER_00", startMs: 0, endMs: 1000, source: "第一句", spoken: "Vế một"},
				{speaker: "SPEAKER_00", startMs: 1100, endMs: 2100, source: "第二句", spoken: "Vế hai"},
				{speaker: "SPEAKER_01", startMs: 2200, endMs: 3200, source: "第三句", spoken: "Vế ba"},
			}
			scriptCAS, _ := issue155PinLineage(t, h, jobID, assetID, runID,
				[]domain.AudioSegment{{StartMs: 0, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue}}, turns)

			lane := defaultVITTSFake(t, h)
			// The group of the first two turns overruns its combined window, so the
			// fixed-rate speaker escalates; the third turn fits its own slot.
			lane.CustomDurations = map[int]int64{0: 2500, 2: 800}
			fallback := registerCosyVoiceFallback(t, h, 2000, true)
			fallback.CustomDurations = map[int]int64{0: 2000, 2: 900}

			_, assignment := runAssignVoices(t, h, assetID, map[string]any{
				"run_id":                 runID,
				"target_language":        "vi",
				"use_same_voice_for_all": tc.shared,
			})
			if assignment == nil {
				t.Fatal("voice assignment failed")
			}
			resp, variant := runDubSynthesize(t, h, assetID, map[string]any{
				"run_id":                 runID,
				"target_language":        "vi",
				"dub_script_variant_cas": scriptCAS,
				"voice_assignment_cas":   assignment.CASHash,
			})
			if resp.StatusCode != http.StatusCreated || variant == nil {
				t.Fatalf("dub-synthesize failed: %d", resp.StatusCode)
			}
			// Three invocations on the original lane: the triggering block, its bounded
			// group and the unaffected speaker's own turn, produced once each.
			if lane.Invocations != 3 {
				t.Fatalf("the original lane must be invoked once per released unit, got %d", lane.Invocations)
			}
			if fallback.Invocations != tc.fallbackInvocated {
				t.Fatalf("expected %d fallback invocations, got %d", tc.fallbackInvocated, fallback.Invocations)
			}
			if len(variant.Escalations) != tc.escalations || variant.Escalations[0].SpeakerID != "SPEAKER_00" {
				t.Fatalf("expected the overrunning speaker to be escalated, got %+v", variant.Escalations)
			}
			if tc.shared {
				// The carried speaker follows the shared voice onto the fallback lane and
				// is audited without a trigger of its own.
				carried := variant.Escalations[1]
				if carried.SpeakerID != "SPEAKER_01" ||
					carried.Reason != domain.VoiceEscalationReasonSharedVoiceScope ||
					len(carried.TriggerSegmentIndices) != 0 {
					t.Fatalf("unexpected shared-voice escalation evidence: %+v", carried)
				}
			}
			group := issue155GroupedSegment(variant)
			if group == nil || !slices.Equal(group.SpeechBlockIndices, []int{0, 1}) {
				t.Fatalf("expected the escalated speaker's bounded group, got %+v", variant.Segments)
			}
			// The escalated pass replays the chosen group verbatim, so the carried remedy is
			// the same topology and text the first pass selected.
			if group.SpokenText != "Vế một Vế hai" {
				t.Fatalf("the escalated pass must carry the chosen group text, got %q", group.SpokenText)
			}
			if group.Voice.ProviderID != provider.CosyVoiceProviderID {
				t.Fatalf("the escalated speaker must hold one lane, got %s", group.Voice.ProviderID)
			}
			var third *domain.DubSegment
			for i := range variant.Segments {
				if variant.Segments[i].SpeakerID == "SPEAKER_01" {
					third = &variant.Segments[i]
				}
			}
			if third == nil {
				t.Fatalf("the unaffected speaker's turn must stay selected, got %+v", variant.Segments)
			}
			if tc.shared {
				if third.Voice.ProviderID != provider.CosyVoiceProviderID {
					t.Fatalf("same-voice-for-all must move the carried speaker onto the fallback lane, got %s", third.Voice.ProviderID)
				}
			} else if third.Voice.ProviderID != provider.ZeroTTSProviderID {
				t.Fatalf("the unaffected speaker must keep its lane, got %s", third.Voice.ProviderID)
			}
			// No sentence-level hopping: a speaker's selected units never mix lanes.
			for _, seg := range variant.Segments {
				want := provider.CosyVoiceProviderID
				if !tc.shared && seg.SpeakerID == "SPEAKER_01" {
					want = provider.ZeroTTSProviderID
				}
				if seg.Voice.ProviderID != want {
					t.Fatalf("segment %d of %s is on %s, want %s", seg.Index, seg.SpeakerID, seg.Voice.ProviderID, want)
				}
			}
			// One hop only: the run's assignment chain has exactly one supersession.
			current := runGetVoiceAssignment(t, h, assetID, runID)
			if current.SupersedesCAS != assignment.CASHash || current.CASHash != variant.VoiceAssignmentCAS {
				t.Fatalf("expected a single supersession, got %+v", current)
			}
			if tc.shared {
				if current.Assignments["SPEAKER_01"] != current.Assignments["SPEAKER_00"] ||
					current.Assignments["SPEAKER_01"].ProviderID != provider.CosyVoiceProviderID {
					t.Fatalf("same-voice-for-all must keep exactly one voice for every speaker, got %+v", current.Assignments)
				}
			} else if current.Assignments["SPEAKER_01"] != assignment.Assignments["SPEAKER_01"] {
				t.Fatalf("the unaffected speaker must keep its frozen assignment, got %+v", current.Assignments["SPEAKER_01"])
			}
		})
	}
}
