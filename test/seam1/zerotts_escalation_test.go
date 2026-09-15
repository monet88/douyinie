package seam1_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
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
