package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #94 — an unresolved fixed-rate (ZeroTTS) timing failure escalates to the
// duration-controlled CosyVoice3 lane at whole-speaker scope, through the existing
// #84/#40 contracts: a superseding speaker-scoped VoiceAssignment plus scoped
// regeneration. These tests drive that over Seam 1 and cover whole-speaker fallback,
// scoped invalidation, immutable source anchors, zero-overrun selection, no
// sentence-level engine hopping, REVIEW retention and deterministic retry.

// escalationFixture is the shared three-segment layout: SPEAKER_00 owns a hard slot
// and a fitting slot, SPEAKER_01 owns a fitting slot that must stay untouched.
func escalationFixture() []domain.TranslationInputSegment {
	return []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1500, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_01", StartMs: 1700, EndMs: 2600, SourceText: "我们去公园散步吧。"},
		{Index: 2, SpeakerID: "SPEAKER_00", StartMs: 2800, EndMs: 4300, SourceText: "明天再继续工作。"},
	}
}

// zeroTTSHardSlotFake drives the unattended VI default lane with one slot (segment 0)
// that the fixed-rate lane cannot fit natively and cannot speed-fit.
func zeroTTSHardSlotFake(t *testing.T, h *testHarness) *provider.FakeTTSProvider {
	t.Helper()
	fake := defaultVITTSFake(t, h)
	fake.DurationMs = 800
	fake.CustomDurations = map[int]int64{0: 1650}
	return fake
}

// registerCosyVoiceFallback registers the duration-controlled fallback lane and, unless
// skipped, its license manifest so router policy/license eligibility is satisfied.
func registerCosyVoiceFallback(t *testing.T, h *testHarness, durationMs int64, withManifest bool) *provider.FakeTTSProvider {
	t.Helper()
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", durationMs)
	if err := h.registry.Register(fakeCosy); err != nil {
		t.Fatalf("register cosyvoice3 provider: %v", err)
	}
	if !withManifest {
		return fakeCosy
	}
	licBody, _ := json.Marshal(domain.LicenseManifestEntry{
		DependencyName: "fake_cosyvoice3_tts",
		Version:        "1.0.0",
		SHA256:         "sha256_mock_fake_cosyvoice3_tts",
		SourceRepo:     "github.com/monet88/douyinie/models/fake_cosyvoice3_tts",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	resp, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register license manifest for cosyvoice3 failed: %v (status %d)", err, resp.StatusCode)
	}
	resp.Body.Close()
	return fakeCosy
}

// runGetVoiceAssignment reads the run's current frozen assignment through the API.
func runGetVoiceAssignment(t *testing.T, h *testHarness, assetID, runID string) *domain.VoiceAssignment {
	t.Helper()
	resp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/voice-assignment?target_language=vi&run_id=" + runID)
	if err != nil {
		t.Fatalf("GET voice-assignment failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for frozen assignment read-back, got %d", resp.StatusCode)
	}
	var stored struct {
		Assignment domain.VoiceAssignment `json:"voice_assignment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil {
		t.Fatalf("decode stored assignment: %v", err)
	}
	return &stored.Assignment
}

// ---------------------------------------------------------------------------
//  1. An unresolved ZeroTTS overrun escalates the whole speaker to CosyVoice3, once,
//     after the bounded rewrite/regroup remedies are exhausted.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_UnresolvedOverrunEscalatesWholeSpeakerToCosyVoice3(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	zeroTTS := zeroTTSHardSlotFake(t, h)

	synthPayload := map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	}

	// Pass 1 with no eligible fallback lane: the ZeroTTS overrun stays an unresolved
	// timing failure and the lane evidence is recorded on the variant.
	respFirst, first := runDubSynthesize(t, h, assetID, synthPayload)
	if respFirst.StatusCode != http.StatusCreated || first == nil {
		t.Fatalf("first dub-synthesize failed: %d", respFirst.StatusCode)
	}
	if first.VoiceAssignmentCAS != assign1.CASHash {
		t.Fatalf("expected the pre-escalation assignment CAS on the first variant, got %s", first.VoiceAssignmentCAS)
	}
	if len(first.Escalations) != 0 {
		t.Fatalf("no fallback lane is eligible yet, so no escalation may be recorded: %+v", first.Escalations)
	}
	if first.OverallStatus != "REVIEW_REQUIRED" || len(first.ReviewSegments) != 1 || first.ReviewSegments[0].Index != 0 {
		t.Fatalf("expected the hard slot 0 to be an unresolved REVIEW, got status %s (review=%d)", first.OverallStatus, len(first.ReviewSegments))
	}
	if len(first.FixedRateSpeakers) != 2 || first.FixedRateSpeakers[0] != "SPEAKER_00" || first.FixedRateSpeakers[1] != "SPEAKER_01" {
		t.Fatalf("expected fixed-rate lane evidence for every speaker on the ZeroTTS lane, got %v", first.FixedRateSpeakers)
	}
	preservedSeg, preservedSHA, preservedSlot := segmentForSpeaker(t, first, "SPEAKER_01")
	zeroInvocationsBefore := zeroTTS.Invocations

	// The duration-controlled fallback lane becomes policy/license eligible.
	fakeCosy := registerCosyVoiceFallback(t, h, 700, true)

	// Retrying the same run now escalates rather than silently reproducing the overrun.
	respSecond, second := runDubSynthesize(t, h, assetID, synthPayload)
	if respSecond.StatusCode != http.StatusCreated || second == nil {
		t.Fatalf("escalated dub-synthesize failed: %d", respSecond.StatusCode)
	}

	// Whole-speaker scope: every SPEAKER_00 segment was regenerated on the fallback
	// lane, while the unaffected speaker caused zero new invocations on its own lane.
	if fakeCosy.Invocations != 2 {
		t.Fatalf("expected both SPEAKER_00 segments to be regenerated on the fallback lane, got %d invocations", fakeCosy.Invocations)
	}
	if zeroTTS.Invocations != zeroInvocationsBefore {
		t.Fatalf("unaffected speaker SPEAKER_01 must not be resynthesized on the original lane: %d new invocations", zeroTTS.Invocations-zeroInvocationsBefore)
	}
	if second.VoiceAssignmentCAS == assign1.CASHash {
		t.Fatalf("escalation must supersede the frozen assignment, still on %s", assign1.CASHash)
	}

	// Escalation evidence: one record naming both lanes, the trigger slot, both CAS pins.
	if len(second.Escalations) != 1 {
		t.Fatalf("expected exactly 1 escalation record, got %d", len(second.Escalations))
	}
	// The regenerated variant stays self-describing: the unaffected speaker is still
	// recorded as produced by a fixed-rate lane, the escalated one is not.
	if len(second.FixedRateSpeakers) != 1 || second.FixedRateSpeakers[0] != "SPEAKER_01" {
		t.Fatalf("expected the reused speaker's fixed-rate lane evidence to survive, got %v", second.FixedRateSpeakers)
	}
	esc := second.Escalations[0]
	if esc.SpeakerID != "SPEAKER_00" || esc.FromProviderID != provider.ZeroTTSProviderID || esc.ToProviderID != provider.CosyVoiceProviderID {
		t.Fatalf("unexpected escalation scope: %+v", esc)
	}
	if esc.Reason != domain.VoiceEscalationReasonFixedRateOverrun || len(esc.TriggerSegmentIndices) != 1 || esc.TriggerSegmentIndices[0] != 0 {
		t.Fatalf("unexpected escalation reason/trigger: %+v", esc)
	}
	if esc.SupersededAssignmentCAS != assign1.CASHash || esc.SupersedingAssignmentCAS != second.VoiceAssignmentCAS {
		t.Fatalf("escalation CAS pins must link superseded -> superseding, got %+v", esc)
	}
	if !esc.Resolved {
		t.Fatalf("expected the escalated regeneration to clear the unresolved overrun")
	}

	// Selection: every slot now fits, and no segment exceeds its immutable source slot.
	if second.OverallStatus != "PASS" || len(second.ReviewSegments) != 0 || len(second.Segments) != 3 {
		t.Fatalf("expected 3 selected segments and PASS, got status %s (selected=%d review=%d)", second.OverallStatus, len(second.Segments), len(second.ReviewSegments))
	}
	slots := make(map[int]domain.DubScriptSegment, len(dubVariant.Segments))
	for _, seg := range dubVariant.Segments {
		slots[seg.Index] = seg
	}
	bySpeaker := make(map[string][]domain.DubSegment)
	for _, seg := range second.Segments {
		bySpeaker[seg.SpeakerID] = append(bySpeaker[seg.SpeakerID], seg)
		source, ok := slots[seg.Index]
		if !ok || seg.StartMs != source.StartMs || seg.EndMs != source.EndMs || seg.SlotDurationMs != source.EndMs-source.StartMs {
			t.Fatalf("segment %d mutated its immutable source anchor: %d..%d slot %d", seg.Index, seg.StartMs, seg.EndMs, seg.SlotDurationMs)
		}
		if seg.MeasuredDurationMs > seg.SlotDurationMs {
			t.Fatalf("segment %d overruns its immutable slot: measured %dms > slot %dms", seg.Index, seg.MeasuredDurationMs, seg.SlotDurationMs)
		}
	}
	if len(bySpeaker["SPEAKER_00"]) != 2 || len(bySpeaker["SPEAKER_01"]) != 1 {
		t.Fatalf("unexpected per-speaker selection: %v", bySpeaker)
	}
	// No sentence-level engine hopping: one speaker, one lane, no exceptions.
	for _, seg := range bySpeaker["SPEAKER_00"] {
		if seg.Voice.ProviderID != provider.CosyVoiceProviderID {
			t.Fatalf("escalated speaker segment %d is on %s: engines must not be mixed within one speaker", seg.Index, seg.Voice.ProviderID)
		}
	}
	// The unaffected speaker keeps its lane, its voice and its already-produced artifact.
	kept := bySpeaker["SPEAKER_01"][0]
	if kept.Voice.ProviderID != provider.ZeroTTSProviderID || kept.AudioSHA256 != preservedSHA {
		t.Fatalf("unaffected speaker artifact was not preserved: %s/%s vs %s", kept.Voice.ProviderID, kept.AudioSHA256, preservedSHA)
	}
	if preservedSeg.Index != kept.Index || preservedSlot != kept.SlotDurationMs {
		t.Fatalf("unaffected speaker slot changed: %d vs %d", preservedSlot, kept.SlotDurationMs)
	}

	// The superseding assignment is speaker-scoped, immutable and linked to its base.
	superseding := runGetVoiceAssignment(t, h, assetID, runID)
	if superseding.CASHash != second.VoiceAssignmentCAS {
		t.Fatalf("variant must pin the superseding assignment CAS %s, got %s", second.VoiceAssignmentCAS, superseding.CASHash)
	}
	if superseding.SupersedesCAS != assign1.CASHash {
		t.Fatalf("expected SupersedesCAS=%s, got %s", assign1.CASHash, superseding.SupersedesCAS)
	}
	if len(superseding.InvalidatedSpeakers) != 1 || superseding.InvalidatedSpeakers[0] != "SPEAKER_00" {
		t.Fatalf("expected scoped invalidation of SPEAKER_00 only, got %v", superseding.InvalidatedSpeakers)
	}
	expectedScope := domain.VoiceChangeInvalidationStages()
	if len(superseding.InvalidationScope) != len(expectedScope) {
		t.Fatalf("expected invalidation scope %v, got %v", expectedScope, superseding.InvalidationScope)
	}
	if got := superseding.Assignments["SPEAKER_00"].ProviderID; got != provider.CosyVoiceProviderID {
		t.Fatalf("expected the escalated speaker on %s, got %s", provider.CosyVoiceProviderID, got)
	}
	if keptVoice := superseding.Assignments["SPEAKER_01"]; keptVoice.ProviderID != provider.ZeroTTSProviderID || keptVoice != assign1.Assignments["SPEAKER_01"] {
		t.Fatalf("unaudited speaker reassignment: %+v", keptVoice)
	}

	// Historical assignment evidence stays byte-identical in CAS: never mutated in place.
	rc, err := h.casStore.Get(assign1.CASHash)
	if err != nil {
		t.Fatalf("historical VoiceAssignment must remain readable: %v", err)
	}
	var historical domain.VoiceAssignment
	if err := json.NewDecoder(rc).Decode(&historical); err != nil {
		rc.Close()
		t.Fatalf("decode historical assignment: %v", err)
	}
	rc.Close()
	if got := historical.Assignments["SPEAKER_00"]; got.ProviderID != provider.ZeroTTSProviderID || got.VoiceID != "quangminh" {
		t.Fatalf("historical ZeroTTS assignment was rewritten: %s/%s", got.ProviderID, got.VoiceID)
	}
	if historical.SupersedesCAS != "" {
		t.Fatalf("historical assignment must not gain supersession provenance: %s", historical.SupersedesCAS)
	}
}

// ---------------------------------------------------------------------------
//  2. The escalated lane runs its own measured-duration multi-pass contract:
//     natural pass -> probe -> speed-fit resynthesis -> re-probe -> fit gate.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_EscalationUsesFallbackMeasuredSpeedFit(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	zeroTTSHardSlotFake(t, h)
	// The fallback lane's natural pass overruns segment 0 slightly; its measured
	// speed-fit resynthesis must then bring it inside the immutable slot.
	fakeCosy := registerCosyVoiceFallback(t, h, 700, true)
	fakeCosy.CustomDurations = map[int]int64{0: 1650}

	respSynth, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("escalated dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if variant.OverallStatus != "PASS" || len(variant.ReviewSegments) != 0 || len(variant.Segments) != 3 {
		t.Fatalf("expected the fallback speed-fit lane to resolve every slot, got %s (selected=%d review=%d)", variant.OverallStatus, len(variant.Segments), len(variant.ReviewSegments))
	}
	if len(variant.Escalations) != 1 || !variant.Escalations[0].Resolved {
		t.Fatalf("expected a resolved escalation, got %+v", variant.Escalations)
	}
	// 2 invocations for the hard slot (natural pass + speed-fit resynthesis) and 1 for
	// the fitting slot: the escalated speaker is regenerated on the fallback lane's own
	// multi-pass contract, never through a synthetic speed request against ZeroTTS.
	if fakeCosy.Invocations != 3 {
		t.Fatalf("expected the fallback lane's natural + speed-fit passes, got %d invocations", fakeCosy.Invocations)
	}
	var hardPlan *domain.DubbingFitPlan
	for i := range variant.FitPlans {
		if variant.FitPlans[i].SegmentIndex == 0 {
			hardPlan = &variant.FitPlans[i]
		}
	}
	if hardPlan == nil || hardPlan.Decision != domain.FitActionAccept || hardPlan.AttemptCount != 2 {
		t.Fatalf("expected the hard slot to be accepted on the speed-fit pass, got %+v", hardPlan)
	}
	if hardPlan.SpeedFactor <= 1.0 {
		t.Fatalf("expected a measured speed-fit factor above 1.0, got %.3f", hardPlan.SpeedFactor)
	}
	if hardPlan.MeasuredDurationMs > hardPlan.SlotDurationMs {
		t.Fatalf("speed-fit pass still overruns: measured %dms > slot %dms", hardPlan.MeasuredDurationMs, hardPlan.SlotDurationMs)
	}
}

// ---------------------------------------------------------------------------
//  3. A retried synthesis reuses the same escalation decision, invocation-free and
//     without minting a second superseding assignment.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_EscalationRetryIsDeterministic(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	zeroTTS := zeroTTSHardSlotFake(t, h)
	fakeCosy := registerCosyVoiceFallback(t, h, 700, true)

	synthPayload := map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	}

	respFirst, first := runDubSynthesize(t, h, assetID, synthPayload)
	if respFirst.StatusCode != http.StatusCreated || first == nil || len(first.Escalations) != 1 {
		t.Fatalf("expected the first synthesis to escalate: status %d", respFirst.StatusCode)
	}
	zeroInvocations, cosyInvocations := zeroTTS.Invocations, fakeCosy.Invocations
	supersedingCAS := first.VoiceAssignmentCAS

	respRetry, retry := runDubSynthesize(t, h, assetID, synthPayload)
	if respRetry.StatusCode != http.StatusCreated || retry == nil {
		t.Fatalf("retried dub-synthesize failed: %d", respRetry.StatusCode)
	}
	if retry.CASHash != first.CASHash {
		t.Fatalf("retry must reproduce the same escalation variant: %s vs %s", retry.CASHash, first.CASHash)
	}
	if zeroTTS.Invocations != zeroInvocations || fakeCosy.Invocations != cosyInvocations {
		t.Fatalf("deterministic retry must not resynthesize: zerotts +%d, cosyvoice +%d", zeroTTS.Invocations-zeroInvocations, fakeCosy.Invocations-cosyInvocations)
	}
	if len(retry.Escalations) != 1 || retry.Escalations[0].SupersedingAssignmentCAS != supersedingCAS {
		t.Fatalf("retry must reuse the persisted superseding assignment: %+v", retry.Escalations)
	}
	if current := runGetVoiceAssignment(t, h, assetID, runID); current.CASHash != supersedingCAS {
		t.Fatalf("retry minted another superseding assignment: %s vs %s", current.CASHash, supersedingCAS)
	}
}

// ---------------------------------------------------------------------------
//  3. A fallback lane that still cannot fit projects REVIEW: no further provider hop,
//     no silent re-entry of the original lane.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_EscalationStillOverrunningProjectsReviewWithoutSecondHop(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	zeroTTSHardSlotFake(t, h)
	// The fallback lane is eligible but cannot fit the escalated speaker either.
	registerCosyVoiceFallback(t, h, 4000, true)

	respSynth, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("escalated dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("an unmeetable escalated speaker must project REVIEW, got %s", variant.OverallStatus)
	}
	if len(variant.Escalations) != 1 || variant.Escalations[0].Resolved {
		t.Fatalf("expected an unresolved escalation record, got %+v", variant.Escalations)
	}
	if len(variant.Segments) != 1 || variant.Segments[0].SpeakerID != "SPEAKER_01" {
		t.Fatalf("only the fitting speaker may be selected, got %d segments", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 2 {
		t.Fatalf("expected both escalated-speaker slots in review, got %d", len(variant.ReviewSegments))
	}
	for _, rev := range variant.ReviewSegments {
		if rev.SpeakerID != "SPEAKER_00" {
			t.Fatalf("unexpected review speaker %s", rev.SpeakerID)
		}
		if rev.Voice.ProviderID != provider.CosyVoiceProviderID {
			t.Fatalf("review segment %d hopped to %s instead of holding the fallback lane", rev.Index, rev.Voice.ProviderID)
		}
	}
	// Exactly one hop: the assignment chain has a single supersession, and the run stays
	// on the fallback lane for the escalated speaker.
	current := runGetVoiceAssignment(t, h, assetID, runID)
	if current.SupersedesCAS != assign1.CASHash {
		t.Fatalf("expected a single supersession from %s, got %s", assign1.CASHash, current.SupersedesCAS)
	}
	if current.CASHash != variant.VoiceAssignmentCAS {
		t.Fatalf("variant must pin the single superseding assignment %s, got %s", variant.VoiceAssignmentCAS, current.CASHash)
	}
	if got := current.Assignments["SPEAKER_00"].ProviderID; got != provider.CosyVoiceProviderID {
		t.Fatalf("escalated speaker must hold %s after the failed regeneration, got %s", provider.CosyVoiceProviderID, got)
	}
}

// ---------------------------------------------------------------------------
//  4. Escalation is gated on fallback-lane eligibility: without a verified license the
//     lane is not eligible, so no assignment is superseded and the overrun stays REVIEW.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_EscalationRequiresEligibleFallbackLane(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	zeroTTSHardSlotFake(t, h)
	// Registered but unmanifested: policy/license eligibility must reject the lane.
	fakeCosy := registerCosyVoiceFallback(t, h, 700, false)

	respSynth, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if fakeCosy.Invocations != 0 {
		t.Fatalf("an ineligible fallback lane must not synthesize, got %d invocations", fakeCosy.Invocations)
	}
	if len(variant.Escalations) != 0 || variant.VoiceAssignmentCAS != assign1.CASHash {
		t.Fatalf("ineligible lane must not supersede the assignment: %+v", variant.Escalations)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 1 || variant.ReviewSegments[0].Voice.ProviderID != provider.ZeroTTSProviderID {
		t.Fatalf("expected the ZeroTTS overrun to stay REVIEW, got %s (%d review)", variant.OverallStatus, len(variant.ReviewSegments))
	}
	if current := runGetVoiceAssignment(t, h, assetID, runID); current.CASHash != assign1.CASHash {
		t.Fatalf("frozen assignment must be unchanged, got %s", current.CASHash)
	}
}

// segmentForSpeaker returns the single selected segment of a speaker plus its artifact
// identity, failing when the fixture no longer produces exactly one.
func segmentForSpeaker(t *testing.T, variant *domain.DubSegmentsVariant, speakerID string) (domain.DubSegment, string, int64) {
	t.Helper()
	var matches []domain.DubSegment
	for _, seg := range variant.Segments {
		if seg.SpeakerID == speakerID {
			matches = append(matches, seg)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 selected segment for %s, got %d", speakerID, len(matches))
	}
	return matches[0], matches[0].AudioSHA256, matches[0].SlotDurationMs
}

// ---------------------------------------------------------------------------
//  5. A variant cached under the pre-#94 stage identity cannot satisfy a #94 request.
//     Issue #94 changed both what the TTS stage persists (Escalations,
//     FixedRateSpeakers) and how it behaves (whole-speaker escalation), so the
//     DubSegments cache identity had to move with it.
// ---------------------------------------------------------------------------

// legacyDubSegmentsSchemaVersion is the DubSegments schema identity a pre-#94 build
// hashed into its stage cache key. It is deliberately pinned instead of derived from
// domain.DubSegmentsSchemaVersion, so the test still describes the pre-#94 entry after
// the current identity moves on.
const legacyDubSegmentsSchemaVersion = 1

// seedLegacyCachedDubSegmentsVariant writes the index row and CAS payload a pre-#94
// build would have produced for this exact (dub script, voice assignment, language)
// request: the pre-#94 stage cache key, and a variant body that predates both
// Escalations and FixedRateSpeakers.
func seedLegacyCachedDubSegmentsVariant(t *testing.T, h *testHarness, dubScriptCAS, voiceAssignCAS, assetID, runID string) string {
	t.Helper()

	legacyProvenance, err := cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       string(provider.TypeTTS),
		InputHashes: []string{dubScriptCAS, voiceAssignCAS},
		SemanticConfig: map[string]any{
			"zero_overrun_fit": "measured_media_truth_v1",
		},
		Language:      "vi",
		SchemaVersion: legacyDubSegmentsSchemaVersion,
	})
	if err != nil {
		t.Fatalf("compute pre-#94 stage cache key: %v", err)
	}

	legacy := &domain.DubSegmentsVariant{
		ID:                  "legacy-pre-94-dub-segments",
		SchemaVersion:       legacyDubSegmentsSchemaVersion,
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: dubScriptCAS,
		VoiceAssignmentCAS:  voiceAssignCAS,
		OverallStatus:       "REVIEW_REQUIRED",
		ProvenanceHash:      legacyProvenance,
		CreatedAt:           time.Now().UTC(),
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal pre-#94 variant: %v", err)
	}
	obj, err := h.casStore.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("put pre-#94 variant in CAS: %v", err)
	}
	if err := h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             legacy.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        obj.SHA256,
		ProvenanceHash: legacyProvenance,
		OverallStatus:  legacy.OverallStatus,
		CreatedAt:      legacy.CreatedAt,
	}); err != nil {
		t.Fatalf("save pre-#94 variant index: %v", err)
	}
	return obj.SHA256
}

func TestSeam1_ZeroTTS_PreIssue94CachedVariantCannotSatisfyTheEscalationContract(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	zeroTTSHardSlotFake(t, h)
	registerCosyVoiceFallback(t, h, 700, true)

	// A pre-#94 build already cached a variant for exactly this request identity.
	legacyCAS := seedLegacyCachedDubSegmentsVariant(t, h, dubVariant.CASHash, assign1.CASHash, assetID, runID)

	respSynth, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if variant.CASHash == legacyCAS {
		t.Fatalf("the pre-#94 cached variant satisfied a #94 synthesis request: the DubSegments cache identity was not versioned")
	}
	// The #94 contract must be honoured from scratch: lane evidence, escalation, fit.
	if len(variant.FixedRateSpeakers) == 0 {
		t.Fatalf("expected fixed-rate lane evidence on the regenerated variant, got none")
	}
	if len(variant.Escalations) != 1 || !variant.Escalations[0].Resolved {
		t.Fatalf("expected one resolved whole-speaker escalation, got %+v", variant.Escalations)
	}
	if variant.OverallStatus != "PASS" || len(variant.Segments) != 3 {
		t.Fatalf("expected a fully fitted variant, got status %s (selected=%d)", variant.OverallStatus, len(variant.Segments))
	}
}

// ---------------------------------------------------------------------------
//  6. A review correction reports the assignment actually in force, even when the
//     synthesis it drives escalates a speaker to the fallback lane.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_ReviewCorrectionReportsTheFinalAssignmentAfterEscalation(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}

	zeroTTSHardSlotFake(t, h)
	registerCosyVoiceFallback(t, h, 700, true)

	// The operator reassigns the unaffected speaker to another verified preset, so the
	// correction regenerates that speaker while SPEAKER_00 keeps its unresolved overrun
	// and takes the whole-speaker escalation.
	presets := provider.ZeroTTSPresetVoices()
	if len(presets) <= provider.DefaultVIUnattendedVoiceCount {
		t.Fatalf("fixture needs a verified preset outside the unattended rotation")
	}
	reassignedVoice := presets[provider.DefaultVIUnattendedVoiceCount]

	reassignBody, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_01": reassignedVoice,
		},
		"reason":   "operator selects another verified preset for the unaffected speaker",
		"operator": "seam1",
	})
	reassignURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/reassign-voice", h.server.URL, assetID)
	respReassign, err := http.Post(reassignURL, "application/json", bytes.NewReader(reassignBody))
	if err != nil {
		t.Fatalf("POST inspector/reassign-voice failed: %v", err)
	}
	if respReassign.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respReassign.Body)
		respReassign.Body.Close()
		t.Fatalf("POST inspector/reassign-voice status %d: %s", respReassign.StatusCode, body)
	}
	var decoded struct {
		Result service.VoiceReassignCorrectionResult `json:"result"`
	}
	if err := json.NewDecoder(respReassign.Body).Decode(&decoded); err != nil {
		respReassign.Body.Close()
		t.Fatalf("decode reassign correction result: %v", err)
	}
	respReassign.Body.Close()
	res := decoded.Result

	if res.DubSegmentsVariantCAS == "" {
		t.Fatalf("expected a regenerated dub segments variant")
	}
	rc, err := h.casStore.Get(res.DubSegmentsVariantCAS)
	if err != nil {
		t.Fatalf("load regenerated variant from CAS: %v", err)
	}
	var variant domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		rc.Close()
		t.Fatalf("decode regenerated variant: %v", err)
	}
	rc.Close()
	if variant.DubScriptVariantCAS != dubVariant.CASHash {
		t.Fatalf("correction regenerated against dub script %s, expected %s", variant.DubScriptVariantCAS, dubVariant.CASHash)
	}

	// The correction must genuinely have escalated, otherwise the CAS assertion below
	// would hold for the wrong reason.
	if len(variant.Escalations) != 1 || !variant.Escalations[0].Resolved {
		t.Fatalf("expected one resolved whole-speaker escalation, got %+v", variant.Escalations)
	}
	if variant.Escalations[0].SpeakerID != "SPEAKER_00" {
		t.Fatalf("expected SPEAKER_00 to escalate, got %s", variant.Escalations[0].SpeakerID)
	}

	// The reported CAS names the assignment the variant was pinned to, not the one the
	// correction asked for before the escalation superseded it.
	if res.VoiceAssignmentCAS != variant.VoiceAssignmentCAS {
		t.Fatalf("correction reported assignment %s while the variant is pinned to %s", res.VoiceAssignmentCAS, variant.VoiceAssignmentCAS)
	}
	current := runGetVoiceAssignment(t, h, assetID, runID)
	if current.CASHash != res.VoiceAssignmentCAS {
		t.Fatalf("correction reported %s but the run assignment in force is %s", res.VoiceAssignmentCAS, current.CASHash)
	}
	if current.SupersedesCAS == "" || current.Assignments["SPEAKER_00"].ProviderID != provider.CosyVoiceProviderID {
		t.Fatalf("expected the superseding escalation assignment in force, got %+v", current)
	}
	// The reported invalidation scope is the one the final assignment actually applied:
	// the escalated speaker, not the speaker the correction reassigned before escalating.
	if len(res.InvalidatedSpeakers) != 1 || res.InvalidatedSpeakers[0] != "SPEAKER_00" {
		t.Fatalf("correction reported invalidation scope %v while the final assignment invalidated %v", res.InvalidatedSpeakers, current.InvalidatedSpeakers)
	}
}

// ---------------------------------------------------------------------------
//  7. A synthesis carrying a stale base assignment must not mint its escalation:
//     doing so would make the older assignment current again and revert the
//     operator's newer choice.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_StaleBaseEscalationCannotClobberNewerOperatorAssignment(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	zeroTTSHardSlotFake(t, h)
	fakeCosy := registerCosyVoiceFallback(t, h, 700, true)

	stalePayload := map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	}
	respFirst, first := runDubSynthesize(t, h, assetID, stalePayload)
	if respFirst.StatusCode != http.StatusCreated || first == nil || len(first.Escalations) != 1 {
		t.Fatalf("expected the first synthesis to escalate: status %d", respFirst.StatusCode)
	}
	if first.VoiceAssignmentCAS == assign1.CASHash {
		t.Fatalf("escalation must supersede the base assignment, still on %s", assign1.CASHash)
	}

	// The operator then reassigns the speaker that never overran, on the escalated run.
	presets := provider.ZeroTTSPresetVoices()
	if len(presets) <= provider.DefaultVIUnattendedVoiceCount {
		t.Fatalf("fixture needs a verified preset outside the unattended rotation")
	}
	reassignedVoice := presets[provider.DefaultVIUnattendedVoiceCount]
	reassignBody, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_01": reassignedVoice,
		},
	})
	respReassign, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-assignment/reassign", "application/json", bytes.NewReader(reassignBody))
	if err != nil {
		t.Fatalf("POST voice-assignment/reassign failed: %v", err)
	}
	if respReassign.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respReassign.Body)
		respReassign.Body.Close()
		t.Fatalf("POST voice-assignment/reassign status %d: %s", respReassign.StatusCode, body)
	}
	var reassigned struct {
		Assignment domain.VoiceAssignment `json:"voice_assignment"`
	}
	if err := json.NewDecoder(respReassign.Body).Decode(&reassigned); err != nil {
		respReassign.Body.Close()
		t.Fatalf("decode operator reassignment: %v", err)
	}
	respReassign.Body.Close()
	operatorCAS := reassigned.Assignment.CASHash
	if operatorCAS == "" || operatorCAS == first.VoiceAssignmentCAS {
		t.Fatalf("expected a superseding operator reassignment, got %q", operatorCAS)
	}

	// The stale caller retries its original request identity (base = assign1) after the
	// run has moved on. It must leave the operator's assignment in force.
	cosyInvocations := fakeCosy.Invocations
	respStale, stale := runDubSynthesize(t, h, assetID, stalePayload)
	if respStale.StatusCode != http.StatusCreated || stale == nil {
		t.Fatalf("stale-base dub-synthesize failed: %d", respStale.StatusCode)
	}
	current := runGetVoiceAssignment(t, h, assetID, runID)
	if current.CASHash != operatorCAS {
		t.Fatalf("a stale-base synthesis replaced the operator assignment %s with %s", operatorCAS, current.CASHash)
	}
	if got := current.Assignments["SPEAKER_01"]; !domain.VoiceProfileEquivalent(got, reassignedVoice) {
		t.Fatalf("the operator's voice choice was reverted to %+v", got)
	}
	if len(stale.Escalations) != 0 || fakeCosy.Invocations != cosyInvocations {
		t.Fatalf("a stale-base synthesis applied an escalation it must not: %+v (+%d fallback invocations)", stale.Escalations, fakeCosy.Invocations-cosyInvocations)
	}
	if stale.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("a refused escalation must leave the unresolved overrun on REVIEW, got %s", stale.OverallStatus)
	}
}

// ---------------------------------------------------------------------------
//  8. Under "one voice for all" the escalation pins every speaker to the fallback
//     voice, so every speaker whose profile actually changes is audited: the shared
//     voice follows the escalated speaker, and the trigger evidence stays honest.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_SharedVoiceEscalationAuditsEveryChangedSpeaker(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"use_same_voice_for_all": true,
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	if !assign1.UseSameVoiceForAll || !domain.VoiceProfileEquivalent(assign1.Assignments["SPEAKER_00"], assign1.Assignments["SPEAKER_01"]) {
		t.Fatalf("fixture requires one shared frozen profile, got %+v", assign1.Assignments)
	}

	zeroTTSHardSlotFake(t, h)
	fakeCosy := registerCosyVoiceFallback(t, h, 700, true)

	respSynth, variant := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || variant == nil {
		t.Fatalf("shared-voice dub-synthesize failed: %d", respSynth.StatusCode)
	}
	if variant.OverallStatus != "PASS" || len(variant.Segments) != 3 {
		t.Fatalf("expected the fallback lane to fit every shared-voice slot, got %s (selected=%d)", variant.OverallStatus, len(variant.Segments))
	}
	// The flag makes the escalated speaker's fallback voice the shared one, so both
	// speakers really changed lane and both must be audited.
	if len(variant.Escalations) != 2 {
		t.Fatalf("expected every actually changed speaker to be audited, got %+v", variant.Escalations)
	}
	escalated := make(map[string]domain.VoiceProviderEscalation, len(variant.Escalations))
	for _, esc := range variant.Escalations {
		escalated[esc.SpeakerID] = esc
	}
	triggered, carried := escalated["SPEAKER_00"], escalated["SPEAKER_01"]
	if triggered.Reason != domain.VoiceEscalationReasonFixedRateOverrun || len(triggered.TriggerSegmentIndices) != 1 || triggered.TriggerSegmentIndices[0] != 0 {
		t.Fatalf("expected the overrunning speaker to keep its trigger evidence, got %+v", triggered)
	}
	if carried.Reason != domain.VoiceEscalationReasonSharedVoiceScope || len(carried.TriggerSegmentIndices) != 0 {
		t.Fatalf("expected the carried speaker to be audited without a trigger of its own, got %+v", carried)
	}
	for _, esc := range []domain.VoiceProviderEscalation{triggered, carried} {
		if esc.FromProviderID != provider.ZeroTTSProviderID || esc.ToProviderID != provider.CosyVoiceProviderID {
			t.Fatalf("unexpected lane change: %+v", esc)
		}
		if esc.SupersededAssignmentCAS != assign1.CASHash || esc.SupersedingAssignmentCAS != variant.VoiceAssignmentCAS {
			t.Fatalf("escalation CAS pins must link superseded -> superseding, got %+v", esc)
		}
		if !esc.Resolved {
			t.Fatalf("expected both shared-voice speakers to be fitted on the fallback lane, got %+v", esc)
		}
	}
	// One lane per speaker: no sentence-level hopping inside the shared voice.
	for _, seg := range variant.Segments {
		if seg.Voice.ProviderID != provider.CosyVoiceProviderID {
			t.Fatalf("segment %d stayed on %s after the shared-voice escalation", seg.Index, seg.Voice.ProviderID)
		}
	}
	if len(variant.FixedRateSpeakers) != 0 {
		t.Fatalf("no speaker may still claim a fixed-rate lane, got %v", variant.FixedRateSpeakers)
	}
	// The carried speaker was regenerated for real (its single slot) alongside the two
	// overrunning-speaker slots, so the audit covers a change that actually happened.
	if fakeCosy.Invocations != 3 {
		t.Fatalf("expected both speakers regenerated on the fallback lane, got %d invocations", fakeCosy.Invocations)
	}
	current := runGetVoiceAssignment(t, h, assetID, runID)
	if current.CASHash != variant.VoiceAssignmentCAS || !current.UseSameVoiceForAll {
		t.Fatalf("variant must pin the superseding shared-voice assignment %s, got %s", variant.VoiceAssignmentCAS, current.CASHash)
	}
	if len(current.InvalidatedSpeakers) != 2 || current.InvalidatedSpeakers[0] != "SPEAKER_00" || current.InvalidatedSpeakers[1] != "SPEAKER_01" {
		t.Fatalf("expected the shared-voice escalation to invalidate every speaker, got %v", current.InvalidatedSpeakers)
	}
}

// seedLegacySchemaPriorDubSegmentsVariant re-files an already produced variant as the
// asset-scoped prior variant for its own request identity (dub script + voice assignment
// CAS), rewritten to the pre-#94 schema identity. The segment bodies are left untouched:
// only the persisted contract they were written under differs.
func seedLegacySchemaPriorDubSegmentsVariant(t *testing.T, h *testHarness, produced *domain.DubSegmentsVariant, assetID, runID string) {
	t.Helper()
	rc, err := h.casStore.Get(produced.CASHash)
	if err != nil {
		t.Fatalf("load produced variant from CAS: %v", err)
	}
	var legacy domain.DubSegmentsVariant
	decodeErr := json.NewDecoder(rc).Decode(&legacy)
	rc.Close()
	if decodeErr != nil {
		t.Fatalf("decode produced variant: %v", decodeErr)
	}
	legacy.SchemaVersion = legacyDubSegmentsSchemaVersion

	data, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("marshal pre-#94 schema variant: %v", err)
	}
	obj, err := h.casStore.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("put pre-#94 schema variant in CAS: %v", err)
	}
	if err := h.db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             legacy.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        obj.SHA256,
		ProvenanceHash: legacy.ProvenanceHash,
		OverallStatus:  legacy.OverallStatus,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save pre-#94 schema variant index: %v", err)
	}
}

// ---------------------------------------------------------------------------
//  9. Prior segment reuse is only valid inside the same persisted contract: a variant
//     written under the pre-#94 schema cannot donate its segments to a superseding
//     synthesis that now requires the #94 lane evidence.
//
// ---------------------------------------------------------------------------
func TestSeam1_ZeroTTS_LegacySchemaPriorVariantIsNotReusedBySupersedingSynthesis(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	dubVariant := setupDubScriptForSeam1Lang(t, h, runID, assetID, "vi", escalationFixture())

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("voice assignment failed: %d", respAssign.StatusCode)
	}
	fake := defaultVITTSFake(t, h)
	// Every fixture slot fits at the lane's natural speed, so the test stays about reuse
	// rather than the fit controller.
	fake.DurationMs = 800

	// The run's first synthesis fits every slot on the ZeroTTS lane.
	respFirst, first := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respFirst.StatusCode != http.StatusCreated || first == nil {
		t.Fatalf("first dub-synthesize failed: %d", respFirst.StatusCode)
	}
	if first.OverallStatus != "PASS" || len(first.Segments) != 3 {
		t.Fatalf("expected a fully fitted first synthesis, got %s (selected=%d)", first.OverallStatus, len(first.Segments))
	}
	seedLegacySchemaPriorDubSegmentsVariant(t, h, first, assetID, runID)

	// The operator reassigns the unaffected speaker, producing a real superseding
	// assignment, so the next synthesis has to decide what it may reuse from the prior
	// variant filed above.
	presets := provider.ZeroTTSPresetVoices()
	if len(presets) <= provider.DefaultVIUnattendedVoiceCount {
		t.Fatalf("fixture needs a verified preset outside the unattended rotation")
	}
	reassignBody, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_01": presets[provider.DefaultVIUnattendedVoiceCount],
		},
	})
	respReassign, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/voice-assignment/reassign", "application/json", bytes.NewReader(reassignBody))
	if err != nil {
		t.Fatalf("POST voice-assignment/reassign failed: %v", err)
	}
	var reassigned struct {
		Assignment domain.VoiceAssignment `json:"voice_assignment"`
	}
	decodeErr := json.NewDecoder(respReassign.Body).Decode(&reassigned)
	respReassign.Body.Close()
	if decodeErr != nil {
		t.Fatalf("decode operator reassignment: %v", decodeErr)
	}
	if respReassign.StatusCode != http.StatusOK || reassigned.Assignment.SupersedesCAS != assign1.CASHash {
		t.Fatalf("expected a superseding assignment based on %s, got status %d with base %s", assign1.CASHash, respReassign.StatusCode, reassigned.Assignment.SupersedesCAS)
	}

	invocationsBefore := fake.Invocations
	respSecond, second := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   reassigned.Assignment.CASHash,
	})
	if respSecond.StatusCode != http.StatusCreated || second == nil {
		t.Fatalf("superseding dub-synthesize failed: %d", respSecond.StatusCode)
	}
	if second.OverallStatus != "PASS" || len(second.Segments) != 3 {
		t.Fatalf("expected the superseding synthesis to fit every slot, got %s (selected=%d)", second.OverallStatus, len(second.Segments))
	}
	// Reuse requires the prior variant to be written under the current contract, so the
	// speaker the operator left alone is synthesized again rather than donated a
	// pre-#94 segment.
	if got := fake.Invocations - invocationsBefore; got != len(second.Segments) {
		t.Fatalf("expected every segment synthesized under the current contract, got %d invocations for %d segments: pre-#94 prior segments were reused", got, len(second.Segments))
	}
}
