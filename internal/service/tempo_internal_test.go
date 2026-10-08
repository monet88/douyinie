package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #156 unit coverage for the review-only tempo candidate derivation. These cases drive
// the derivation the synthesis pass makes for each unresolved overrun directly, because the
// full pass cannot state a corrupt parent, a missing tool, a nil CAS store, an exact frame
// boundary or an accepted window narrower than the slot without a whole fixture run.

func newTempoUnitHarness(t *testing.T) (*DubbingService, *storage.DB, *cas.Store) {
	t.Helper()
	dir := t.TempDir()
	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}
	db, err := storage.Open(filepath.Join(dir, "tempo_unit.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewDubbingService(db, casStore), db, casStore
}

// seedTempoUnitLineage persists the asset/job/run rows a committed variant points at, so a
// cancellation-evidence case can publish through the real commit path.
func seedTempoUnitLineage(t *testing.T, db *storage.DB, assetID, jobID, runID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              "att-" + assetID,
		AttestationType: "OWNER_DIRECT",
		DeclaredBy:      "tempo-unit-test",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		SHA256:              strings.Repeat("3", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		RightsAttestationID: "att-" + assetID,
		CASPath:             "/source.mp4",
		CreatedAt:           now,
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateRunEnqueued(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "running",
		ConfigSnapshotJSON: "{}",
		CreatedAt:          now,
	}, jobID); err != nil {
		t.Fatalf("create run: %v", err)
	}
}

func tempoUnitReviewSegment(audioSHA string, naturalMs, startMs, windowEndMs int64, reason string) domain.DubSegmentReview {
	return domain.DubSegmentReview{
		Index:              0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            startMs,
		EndMs:              windowEndMs,
		DubPlaybackEndMs:   windowEndMs,
		AudioSHA256:        audioSHA,
		MeasuredDurationMs: naturalMs,
		FitDecision:        domain.FitActionReview,
		ReviewReason:       reason,
	}
}

// TestTempoCandidateForReview_MissingOrCorruptParentRecordsSourceUnavailable proves a source
// that cannot be read is evidence, never a factor accusation: a missing artifact and an
// artifact that is not a WAV container both record TEMPO_SOURCE_UNAVAILABLE, commit no
// transformed artifact and stay unselectable.
func TestTempoCandidateForReview_MissingOrCorruptParentRecordsSourceUnavailable(t *testing.T) {
	svc, _, casStore := newTempoUnitHarness(t)

	missing := tempoUnitReviewSegment(strings.Repeat("a", 64), 1200, 0, 1000, "DURATION_OVERRUN")
	candMissing := svc.tempoCandidateForReview(context.Background(), "fit-policy-v1", 0, &missing)
	if candMissing == nil {
		t.Fatal("expected a candidate record for a missing parent")
	}
	if candMissing.Reason != domain.TempoReasonSourceInvalid {
		t.Fatalf("expected TEMPO_SOURCE_UNAVAILABLE, got %s", candMissing.Reason)
	}
	if candMissing.TransformedAudioSHA256 != "" || candMissing.Selectable {
		t.Fatalf("missing parent must commit no transformed artifact, got sha=%q selectable=%v", candMissing.TransformedAudioSHA256, candMissing.Selectable)
	}

	corruptObj, err := casStore.Put(bytes.NewReader([]byte("not_a_valid_wav_container_header_content")))
	if err != nil {
		t.Fatalf("put corrupt object: %v", err)
	}
	corrupt := tempoUnitReviewSegment(corruptObj.SHA256, 1200, 0, 1000, "DURATION_OVERRUN")
	candCorrupt := svc.tempoCandidateForReview(context.Background(), "fit-policy-v1", 0, &corrupt)
	if candCorrupt.Reason != domain.TempoReasonSourceInvalid {
		t.Fatalf("expected TEMPO_SOURCE_UNAVAILABLE for a corrupt parent, got %s", candCorrupt.Reason)
	}
	if candCorrupt.TransformedAudioSHA256 != "" {
		t.Fatalf("corrupt parent must commit no transformed artifact, got %s", candCorrupt.TransformedAudioSHA256)
	}
}

// TestTempoCandidateForReview_NilCASRecordsSourceUnavailable proves a service without a CAS
// store records TEMPO_SOURCE_UNAVAILABLE safely without panicking.
func TestTempoCandidateForReview_NilCASRecordsSourceUnavailable(t *testing.T) {
	svc := &DubbingService{}
	rev := tempoUnitReviewSegment(strings.Repeat("a", 64), 1200, 0, 1000, "DURATION_OVERRUN")
	cand := svc.tempoCandidateForReview(context.Background(), "fit-policy-v1", 0, &rev)
	if cand == nil {
		t.Fatal("expected candidate record for nil CAS")
	}
	if cand.Reason != domain.TempoReasonSourceInvalid {
		t.Fatalf("expected TEMPO_SOURCE_UNAVAILABLE for nil CAS, got %s", cand.Reason)
	}
	if cand.TransformedAudioSHA256 != "" || cand.Selectable {
		t.Fatalf("nil CAS must commit no transformed artifact, got sha=%q selectable=%v", cand.TransformedAudioSHA256, cand.Selectable)
	}
}

// TestTempoCandidateForReview_ToolUnavailableRecordsEvidence proves a host without ffmpeg
// records TEMPO_TOOL_UNAVAILABLE without committing anything, so the overrun stays reviewable.
func TestTempoCandidateForReview_ToolUnavailableRecordsEvidence(t *testing.T) {
	svc, _, casStore := newTempoUnitHarness(t)

	naturalWAV := media.GeneratePCM16WAV(16000, 1, 1200)
	natObj, err := casStore.Put(bytes.NewReader(naturalWAV))
	if err != nil {
		t.Fatalf("put natural audio: %v", err)
	}

	// An empty PATH makes exec.LookPath("ffmpeg") fail deterministically.
	t.Setenv("PATH", t.TempDir())

	rev := tempoUnitReviewSegment(natObj.SHA256, 1200, 0, 1000, "DURATION_OVERRUN")
	cand := svc.tempoCandidateForReview(context.Background(), "fit-policy-v1", 0, &rev)
	if cand.Reason != domain.TempoReasonToolUnavailable {
		t.Fatalf("expected TEMPO_TOOL_UNAVAILABLE, got %s", cand.Reason)
	}
	if cand.TransformedAudioSHA256 != "" || cand.Selectable {
		t.Fatalf("a missing tool must commit no transformed artifact, got sha=%q selectable=%v", cand.TransformedAudioSHA256, cand.Selectable)
	}
}

// TestTempoCandidateForReview_OneFrameOverrunIsFrameExact proves frame-exact eligibility: a
// waveform one frame over its accepted window at 16 kHz yields factor = 16001/16000 > 1, so it
// qualifies for a transform instead of being suppressed by floored integer division.
func TestTempoCandidateForReview_OneFrameOverrunIsFrameExact(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	svc, _, casStore := newTempoUnitHarness(t)

	const sampleRate = 16000
	const windowFrames = 16000            // exactly 1000ms at 16 kHz
	const oneFrameOver = windowFrames + 1 // 16001 samples -> 1000.0625ms

	samples := make([]int16, oneFrameOver)
	for i := range samples {
		samples[i] = int16(4000.0 * math.Sin(2.0*math.Pi*440.0*float64(i)/float64(sampleRate)))
	}
	obj, err := casStore.Put(bytes.NewReader(media.EncodePCM16Samples(samples, sampleRate, 1)))
	if err != nil {
		t.Fatalf("put wav: %v", err)
	}

	rev := tempoUnitReviewSegment(obj.SHA256, 1000, 0, 1000, "DURATION_OVERRUN")
	cand := svc.tempoCandidateForReview(context.Background(), "playback-window-v2", 0, &rev)
	if cand == nil {
		t.Fatal("expected a tempo candidate for a one-frame overrun")
	}
	if expected := float64(oneFrameOver) / float64(windowFrames); math.Abs(cand.Factor-expected) > 1e-6 {
		t.Fatalf("expected frame-exact factor %v, got %v", expected, cand.Factor)
	}
	if cand.TransformedAudioSHA256 == "" {
		t.Fatal("expected transformed audio for an in-range one-frame overrun")
	}
	if cand.PlaybackDurationMs != 1000 {
		t.Fatalf("expected PlaybackDurationMs 1000, got %d", cand.PlaybackDurationMs)
	}
	rc, err := casStore.Get(cand.TransformedAudioSHA256)
	if err != nil {
		t.Fatalf("get transformed audio: %v", err)
	}
	defer rc.Close()
	transBytes, _ := io.ReadAll(rc)
	transFrames, transRate, err := media.WAVFrameGeometry(transBytes)
	if err != nil {
		t.Fatalf("WAVFrameGeometry: %v", err)
	}
	if cand.Selectable != (transFrames <= windowFrames && transRate == sampleRate) {
		t.Fatalf("Selectable=%v does not match frame geometry: frames=%d window=%d rate=%d", cand.Selectable, transFrames, windowFrames, transRate)
	}
}

// TestAttachTempoCandidates_OnlyUnresolvedDurationOverrunDerivesCandidate proves only a unit
// whose remedy sequence actually ran derives DSP evidence: any other review verdict derives no
// candidate even with intact audio and a probe past the playback window.
func TestAttachTempoCandidates_OnlyUnresolvedDurationOverrunDerivesCandidate(t *testing.T) {
	svc, _, casStore := newTempoUnitHarness(t)

	obj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1200)))
	if err != nil {
		t.Fatalf("put wav: %v", err)
	}

	for _, reason := range []string{"QA_REJECTED", "INVALID_FIT_POLICY", "INVALID_TIMING", "tts_unresolvable_overrun"} {
		variant := domain.DubSegmentsVariant{
			ID:             uuid.NewString(),
			AssetID:        "asset-qa",
			RunID:          "run-qa",
			TargetLanguage: "vi",
			FitPolicyID:    "playback-window-v2",
			ReviewSegments: []domain.DubSegmentReview{
				tempoUnitReviewSegment(obj.SHA256, 1200, 0, 1000, reason),
			},
		}
		svc.attachTempoCandidates(context.Background(), &variant)
		if tc := variant.ReviewSegments[0].TempoCandidate; tc != nil {
			t.Fatalf("reason %s must not derive a tempo candidate, got %+v", reason, tc)
		}
	}
}

// TestAttachTempoCandidates_AcceptedWindowNotSlotDecidesFactor proves the factor comes from the
// retained waveform over the accepted playback window, not over the segment slot: a 2000ms slot
// with a 1100ms window and a 1200ms waveform derives 1200/1100, while slot-derived arithmetic
// (0.6) would produce no candidate at all. The recorded PlaybackDurationMs is the window.
func TestAttachTempoCandidates_AcceptedWindowNotSlotDecidesFactor(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	svc, _, casStore := newTempoUnitHarness(t)

	const (
		slotMs    int64 = 2000
		windowMs  int64 = 1100
		naturalMs int64 = 1200
	)

	obj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, naturalMs)))
	if err != nil {
		t.Fatalf("put retained waveform: %v", err)
	}

	variant := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		AssetID:        "asset-window-not-slot",
		RunID:          "run-window-not-slot",
		TargetLanguage: "vi",
		FitPolicyID:    "playback-window-v2",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              slotMs,
				DubPlaybackEndMs:   windowMs,
				AudioSHA256:        obj.SHA256,
				MeasuredDurationMs: naturalMs,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
			},
		},
	}

	svc.attachTempoCandidates(context.Background(), &variant)
	tc := variant.ReviewSegments[0].TempoCandidate
	if tc == nil {
		t.Fatal("expected a tempo candidate for an overrun of the accepted playback window")
	}
	if tc.PlaybackDurationMs != windowMs {
		t.Fatalf("expected the accepted window %dms to be recorded, got %dms (slot is %dms)", windowMs, tc.PlaybackDurationMs, slotMs)
	}
	if expected := float64(naturalMs) / float64(windowMs); math.Abs(tc.Factor-expected) > 0.01 {
		t.Fatalf("expected window-derived factor %v, got %v", expected, tc.Factor)
	}
	if tc.TransformedAudioSHA256 == "" {
		t.Fatalf("expected a produced transform for an in-range window factor, got reason %s", tc.Reason)
	}
}

// TestAttachTempoCandidates_DerivesOneCandidatePerQualifyingUnit proves sharing one resolved
// output rate across the variant loop does not drop units: every unresolved overrun with an
// intact waveform derives its own candidate, while a unit that already carries evidence and a
// unit whose verdict is not an exhausted duration overrun are both left untouched.
//
// The shared rate itself is deliberately not observable here: the factor is a ratio of two
// quantities measured in the same rate, so it is rate-invariant (asserting a rate would pin
// nothing). What this case does pin is the loop's coverage, which is what the hoist touched.
func TestAttachTempoCandidates_DerivesOneCandidatePerQualifyingUnit(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	svc, _, casStore := newTempoUnitHarness(t)

	first, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1150)))
	if err != nil {
		t.Fatalf("put first waveform: %v", err)
	}
	second, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1200)))
	if err != nil {
		t.Fatalf("put second waveform: %v", err)
	}

	const alreadyReviewedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	segs := []domain.DubSegmentReview{
		tempoUnitReviewSegment(first.SHA256, 1150, 0, 1000, "DURATION_OVERRUN"),
		tempoUnitReviewSegment(second.SHA256, 1200, 0, 1000, "DURATION_OVERRUN"),
		tempoUnitReviewSegment(alreadyReviewedSHA, 1200, 0, 1000, "QA_REJECTED"),
	}
	segs[1].Index = 1
	segs[2].Index = 2
	segs[2].TempoCandidate = &domain.DubTempoCandidate{
		NaturalAudioSHA256: alreadyReviewedSHA,
		Reason:             domain.TempoReasonCancelled,
	}

	variant := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		AssetID:        "asset-multi-unit",
		RunID:          "run-multi-unit",
		TargetLanguage: "vi",
		FitPolicyID:    "playback-window-v2",
		ReviewSegments: segs,
	}

	svc.attachTempoCandidates(context.Background(), &variant)

	for i, wantFactor := range []float64{1.15, 1.2} {
		tc := variant.ReviewSegments[i].TempoCandidate
		if tc == nil {
			t.Fatalf("unit %d must derive its own candidate", i)
		}
		if tc.NaturalAudioSHA256 != segs[i].AudioSHA256 {
			t.Fatalf("unit %d must keep its own natural waveform, got %s", i, tc.NaturalAudioSHA256)
		}
		if math.Abs(tc.Factor-wantFactor) > 0.01 {
			t.Fatalf("unit %d expected factor %v, got %v", i, wantFactor, tc.Factor)
		}
		if tc.TransformedAudioSHA256 == "" {
			t.Fatalf("unit %d expected a produced transform, got reason %s", i, tc.Reason)
		}
	}

	preserved := variant.ReviewSegments[2].TempoCandidate
	if preserved == nil || preserved.Reason != domain.TempoReasonCancelled || preserved.NaturalAudioSHA256 != alreadyReviewedSHA {
		t.Fatalf("a unit that already carries evidence must be left untouched, got %+v", preserved)
	}
}

// TestAttachTempoCandidates_CancelledTransformRecordsEvidence proves a cancelled or timed-out
// transform is published as TEMPO_CANCELLED evidence rather than aborting the pass: the
// candidate carries the factor, the natural waveform and no transformed artifact, the pass
// still publishes through the cancellation-independent commit, and the persisted payload the
// operator reads carries the same reason.
func TestAttachTempoCandidates_CancelledTransformRecordsEvidence(t *testing.T) {
	svc, db, casStore := newTempoUnitHarness(t)

	const (
		assetID    = "asset-tempo-cancel-attach"
		jobID      = "job-tempo-cancel-attach"
		runID      = "run-tempo-cancel-attach"
		targetLang = "vi"
	)
	seedTempoUnitLineage(t, db, assetID, jobID, runID)

	obj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1200)))
	if err != nil {
		t.Fatalf("put retained waveform: %v", err)
	}

	variant := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: targetLang,
		FitPolicyID:    "playback-window-v2",
		ReviewSegments: []domain.DubSegmentReview{
			tempoUnitReviewSegment(obj.SHA256, 1200, 0, 1000, "DURATION_OVERRUN"),
		},
		CreatedAt: time.Now().UTC(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.attachTempoCandidates(ctx, &variant)
	tc := variant.ReviewSegments[0].TempoCandidate
	if tc == nil {
		t.Fatal("a cancelled transform must still record review evidence")
	}
	if tc.Reason != domain.TempoReasonCancelled {
		t.Fatalf("expected %s, got %s", domain.TempoReasonCancelled, tc.Reason)
	}
	if tc.TransformedAudioSHA256 != "" || tc.Selectable {
		t.Fatalf("a cancelled transform must commit no artifact and stay unselectable, got sha=%q selectable=%v", tc.TransformedAudioSHA256, tc.Selectable)
	}
	if tc.Factor <= 1 {
		t.Fatalf("expected the derived factor to be recorded with the evidence, got %v", tc.Factor)
	}

	// The publication step is cancellation-independent by design, so the evidence survives.
	if err := svc.commitDubSegmentsVariant(context.WithoutCancel(ctx), &variant); err != nil {
		t.Fatalf("publish cancelled-pass evidence: %v", err)
	}
	idx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
	if err != nil || idx == nil {
		t.Fatalf("expected the evidence-bearing variant to be published, got idx=%v err=%v", idx, err)
	}
	rc, err := casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("load published variant: %v", err)
	}
	defer rc.Close()
	var published domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&published); err != nil {
		t.Fatalf("decode published variant: %v", err)
	}
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
		t.Fatal("the published variant must carry the cancelled candidate evidence")
	}
	if got := published.ReviewSegments[0].TempoCandidate.Reason; got != domain.TempoReasonCancelled {
		t.Fatalf("persisted reason must be %s, got %s", domain.TempoReasonCancelled, got)
	}
}

// Issue #156: a run-scoped lookup trusts a decoded variant only when the row it came from claims
// that run. A real index row fetched by run_id carries the requested run, so a body that decodes
// to another run's variant - same asset and language - is refused. The dub_synthesize
// stage-artifact fallback a run with no index row of its own uses carries no RunID claim: a
// replay run consumes an artifact whose immutable body an older run produced, and its own stage
// bound the artifact, so the body still resolves. The asset-scoped path (no requested run) keeps
// applying only the asset/language binding.
func TestRunScopedDubbingVariant_EnforcesRequestedRunOwnership(t *testing.T) {
	_, db, casStore := newTempoUnitHarness(t)
	svc := &ReviewService{db: db, cas: casStore}
	ctx := context.Background()

	const (
		assetID      = "asset-ownership-1"
		jobID        = "job-ownership-1"
		requestedRun = "run-ownership-requested"
		foreignRun   = "run-ownership-foreign"
		ownRun       = "run-ownership-own"
	)
	seedTempoUnitLineage(t, db, assetID, jobID, requestedRun)
	for _, runID := range []string{foreignRun, ownRun} {
		if err := db.CreateRun(ctx, domain.LocalizationRun{
			ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create run %s: %v", runID, err)
		}
	}
	putVariant := func(id, runID string) string {
		t.Helper()
		blob, err := json.Marshal(domain.DubSegmentsVariant{ID: id, AssetID: assetID, RunID: runID, TargetLanguage: "vi"})
		if err != nil {
			t.Fatalf("marshal variant %s: %v", id, err)
		}
		obj, err := casStore.Put(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("put variant %s: %v", id, err)
		}
		return obj.SHA256
	}
	bindStage := func(runID, hash string) {
		t.Helper()
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID: uuid.NewString(), RunID: runID, Stage: "dub_synthesize",
			Status: domain.StageStatusSucceeded, ArtifactSHA256: hash, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("bind %s stage artifact: %v", runID, err)
		}
	}

	// A row fetched by run_id that resolves to another run's variant body is refused.
	foreignHash := putVariant("variant-foreign", foreignRun)
	claimed := &storage.DubSegmentsVariantIndex{
		ID: "variant-foreign", AssetID: assetID, RunID: requestedRun, TargetLanguage: "vi", CASHash: foreignHash,
	}
	if _, err := svc.loadRunDubbingVariant(claimed, assetID, "vi"); !errors.Is(err, ErrDubMediaNotOwned) {
		t.Fatalf("expected ErrDubMediaNotOwned for a row claimed by another run, got %v", err)
	}

	// The requested run has no index row of its own, so the fallback resolves the artifact its
	// stage execution recorded. The synthesized index must not fabricate a run claim: the body
	// names another run, and the stage binding is what makes the artifact this run's to project.
	bindStage(requestedRun, foreignHash)
	idx, err := svc.runScopedDubbingVariantIndex(ctx, requestedRun, assetID, "vi")
	if err != nil {
		t.Fatalf("resolve run-scoped variant index: %v", err)
	}
	if idx == nil || idx.RunID != "" {
		t.Fatalf("the stage-artifact fallback must carry no run claim, got %+v", idx)
	}
	if _, err := svc.loadRunDubbingVariant(idx, assetID, "vi"); err != nil {
		t.Fatalf("a run-bound replay artifact must resolve for the replay run, got %v", err)
	}

	// A run whose bound artifact does claim it still resolves through the same fallback.
	bindStage(ownRun, putVariant("variant-own", ownRun))
	ownIdx, err := svc.runScopedDubbingVariantIndex(ctx, ownRun, assetID, "vi")
	if err != nil {
		t.Fatalf("resolve run-owned variant index: %v", err)
	}
	decoded, err := svc.loadRunDubbingVariant(ownIdx, assetID, "vi")
	if err != nil || decoded == nil || decoded.RunID != ownRun {
		t.Fatalf("expected the run-owned artifact to decode, got variant=%+v err=%v", decoded, err)
	}

	// With no requested run the asset-scoped path applies only the asset/language binding.
	bound := &storage.DubSegmentsVariantIndex{AssetID: assetID, TargetLanguage: "vi", CASHash: foreignHash}
	if _, err := svc.loadRunDubbingVariant(bound, assetID, "vi"); err != nil {
		t.Fatalf("asset-scoped lookup must apply only the asset/language binding, got %v", err)
	}
}

// putTempoUnitVariant commits one dubbing variant that records voiceAssignCAS as the assignment
// it was synthesized under, and returns the variant's CAS hash plus the hash of the retained
// waveform the variant owns, so a caller can either index it or bind it to a stage execution.
func putTempoUnitVariant(t *testing.T, casStore *cas.Store, assetID, jobID, runID, targetLang, voiceAssignCAS string) (variantCAS, audioSHA string) {
	t.Helper()
	audio, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1200)))
	if err != nil {
		t.Fatalf("put retained waveform: %v", err)
	}
	variant := domain.DubSegmentsVariant{
		ID:                 uuid.NewString(),
		SchemaVersion:      domain.DubSegmentsSchemaVersion,
		AssetID:            assetID,
		RunID:              runID,
		JobID:              jobID,
		TargetLanguage:     targetLang,
		FitPolicyID:        "playback-window-v2",
		OverallStatus:      "REVIEW_REQUIRED",
		VoiceAssignmentCAS: voiceAssignCAS,
		ReviewSegments: []domain.DubSegmentReview{{
			Index:              0,
			SpeakerID:          "SPEAKER_00",
			StartMs:            0,
			EndMs:              1000,
			SlotDurationMs:     1000,
			AudioSHA256:        audio.SHA256,
			MeasuredDurationMs: 1200,
			FitDecision:        domain.FitActionReview,
			ReviewReason:       "DURATION_OVERRUN",
			AttemptCount:       1,
			DubPlaybackEndMs:   1000,
		}},
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(variant)
	if err != nil {
		t.Fatalf("marshal variant: %v", err)
	}
	obj, err := casStore.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("put variant: %v", err)
	}
	return obj.SHA256, audio.SHA256
}

// saveTempoUnitVoiceAssignment records the voice assignment in force for a run.
func saveTempoUnitVoiceAssignment(t *testing.T, db *storage.DB, assetID, jobID, runID, targetLang, casHash string) {
	t.Helper()
	if err := db.SaveVoiceAssignmentIndex(context.Background(), storage.VoiceAssignmentIndex{
		ID:              "va-" + runID,
		AssetID:         assetID,
		RunID:           runID,
		JobID:           jobID,
		TargetLanguage:  targetLang,
		CASHash:         casHash,
		ProvenanceHash:  "prov-va-" + runID,
		AssignmentsJSON: "{}",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save voice assignment %s: %v", casHash, err)
	}
}

// indexTempoUnitVariant binds a committed variant as the run's dub-variant row.
func indexTempoUnitVariant(t *testing.T, db *storage.DB, assetID, jobID, runID, targetLang, variantCAS string) {
	t.Helper()
	if err := db.SaveDubSegmentsVariantIndex(context.Background(), storage.DubSegmentsVariantIndex{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: targetLang,
		CASHash:        variantCAS,
		ProvenanceHash: "prov-indexed-" + variantCAS[:12],
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save variant index: %v", err)
	}
}

// TestRunScopedDubbingVariant_RefusesSupersededAssignmentEvidence covers the two ways a pass
// synthesized under a superseded voice assignment can still resolve for the run (Issue #156):
// the run's own dub-variant row can hold that pass when a reassignment lands after it was
// claimed, and - when the pass never claimed the row - the run's dub_synthesize stage execution
// binds it instead, which is exactly what executeRun records after a refused claim. Both
// surfaces must fail closed: playback refuses the audio and the review projection does not
// surface the superseded assignment's evidence. A pass that belongs to the assignment in force,
// a cross-run replay binding, and a stage-only lineage with no assignment row stay permitted.
func TestRunScopedDubbingVariant_RefusesSupersededAssignmentEvidence(t *testing.T) {
	const (
		assetID = "asset-stale-assign"
		jobID   = "job-stale-assign"
		runID   = "run-stale-assign"
		lang    = "vi"
	)
	ctx := context.Background()

	t.Run("indexed row holding a superseded assignment is refused", func(t *testing.T) {
		_, db, casStore := newTempoUnitHarness(t)
		seedTempoUnitLineage(t, db, assetID, jobID, runID)
		saveTempoUnitVoiceAssignment(t, db, assetID, jobID, runID, lang, "assign-current")
		variantCAS, audioSHA := putTempoUnitVariant(t, casStore, assetID, jobID, runID, lang, "assign-superseded")
		indexTempoUnitVariant(t, db, assetID, jobID, runID, lang, variantCAS)
		svc := &ReviewService{db: db, cas: casStore}

		if _, err := svc.OpenRunDubMedia(ctx, runID, audioSHA); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("playback must refuse a superseded assignment's audio, got err=%v", err)
		}
		if _, err := svc.ProjectReviewItemsForRun(ctx, assetID, lang, runID); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("the pending projection must not surface a superseded assignment's evidence, got err=%v", err)
		}
		if _, err := svc.ProjectAllReviewItemsForRun(ctx, assetID, lang, runID); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("the audit projection must not surface a superseded assignment's evidence, got err=%v", err)
		}
	})

	t.Run("stage-bound superseded assignment without a row is refused", func(t *testing.T) {
		_, db, casStore := newTempoUnitHarness(t)
		seedTempoUnitLineage(t, db, assetID, jobID, runID)
		saveTempoUnitVoiceAssignment(t, db, assetID, jobID, runID, lang, "assign-current")
		variantCAS, audioSHA := putTempoUnitVariant(t, casStore, assetID, jobID, runID, lang, "assign-superseded")
		// The superseded pass never claimed the run's row: the run's own stage execution is the
		// only binding left.
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          runID,
			Stage:          "dub_synthesize",
			Status:         domain.StageStatusSucceeded,
			ArtifactSHA256: variantCAS,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("bind stale stage artifact: %v", err)
		}
		svc := &ReviewService{db: db, cas: casStore}

		if _, err := svc.OpenRunDubMedia(ctx, runID, audioSHA); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("playback must refuse a superseded stage-bound assignment's audio, got err=%v", err)
		}
		if _, err := svc.ProjectAllReviewItemsForRun(ctx, assetID, lang, runID); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("the projection must not surface the stage-bound superseded evidence, got err=%v", err)
		}
	})

	t.Run("a differently cased target language does not exempt a stale variant", func(t *testing.T) {
		_, db, casStore := newTempoUnitHarness(t)
		seedTempoUnitLineage(t, db, assetID, jobID, runID)
		// The stored assignment row and the lookup key can carry different casing: a job row is
		// written from client input, and the guard must still recognize the assignment in force
		// instead of reading the case-sensitive miss as "no assignment to contradict".
		saveTempoUnitVoiceAssignment(t, db, assetID, jobID, runID, "vi", "assign-current")
		_ = casStore
		svc := &ReviewService{db: db, cas: casStore}
		stale := &domain.DubSegmentsVariant{
			ID:                 uuid.NewString(),
			AssetID:            assetID,
			RunID:              runID,
			TargetLanguage:     "VI",
			VoiceAssignmentCAS: "assign-superseded",
		}
		if err := svc.verifyVariantVoiceLineage(ctx, runID, assetID, "VI", stale); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("an upper-cased lookup must still refuse a superseded assignment, got err=%v", err)
		}
		if err := svc.verifyVariantVoiceLineage(ctx, runID, assetID, "vi", stale); !errors.Is(err, ErrDubMediaNotOwned) {
			t.Fatalf("the lower-cased lookup must refuse too, got err=%v", err)
		}
	})

	t.Run("current assignment, cross-run replay and stage-only lineage stay permitted", func(t *testing.T) {
		_, db, casStore := newTempoUnitHarness(t)
		seedTempoUnitLineage(t, db, assetID, jobID, runID)
		saveTempoUnitVoiceAssignment(t, db, assetID, jobID, runID, lang, "assign-current")
		svc := &ReviewService{db: db, cas: casStore}
		// Playback hands back an open CAS reader; close it so the case leaves no file handle
		// behind (a leaked handle makes the Windows temp-dir cleanup of this test fail).
		openAudio := func(run, hash string) error {
			source, err := svc.OpenRunDubMedia(ctx, run, hash)
			if err != nil {
				return err
			}
			return source.Reader.Close()
		}

		// The pass belongs to the assignment in force: nothing to refuse.
		currentCAS, currentAudio := putTempoUnitVariant(t, casStore, assetID, jobID, runID, lang, "assign-current")
		indexTempoUnitVariant(t, db, assetID, jobID, runID, lang, currentCAS)
		if err := openAudio(runID, currentAudio); err != nil {
			t.Fatalf("the current assignment's audio must stay playable, got %v", err)
		}
		if _, err := svc.ProjectAllReviewItemsForRun(ctx, assetID, lang, runID); err != nil {
			t.Fatalf("the current assignment's projection must resolve, got %v", err)
		}

		// A replay run consumes an artifact another run produced under its own assignment; the
		// stage binding is the run's authority, so the assignment check must not apply.
		const replayRunID = "run-replay-consumer"
		if _, err := db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID: replayRunID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: time.Now().UTC(),
		}, jobID); err != nil {
			t.Fatalf("create replay run: %v", err)
		}
		replayCAS, replayAudio := putTempoUnitVariant(t, casStore, assetID, jobID, "run-replay-producer", lang, "assign-producer")
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          replayRunID,
			Stage:          "dub_synthesize",
			Status:         domain.StageStatusSucceeded,
			ArtifactSHA256: replayCAS,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("bind replay stage artifact: %v", err)
		}
		if err := openAudio(replayRunID, replayAudio); err != nil {
			t.Fatalf("a run-bound replay of another run's artifact must stay playable, got %v", err)
		}

		// A stage-only lineage with no assignment row at all has no assignment to contradict.
		const bareRunID = "run-bare-stage-only"
		if _, err := db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID: bareRunID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: time.Now().UTC(),
		}, jobID); err != nil {
			t.Fatalf("create bare run: %v", err)
		}
		bareCAS, bareAudio := putTempoUnitVariant(t, casStore, assetID, jobID, bareRunID, lang, "assign-never-recorded")
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          bareRunID,
			Stage:          "dub_synthesize",
			Status:         domain.StageStatusSucceeded,
			ArtifactSHA256: bareCAS,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("bind bare stage artifact: %v", err)
		}
		if err := openAudio(bareRunID, bareAudio); err != nil {
			t.Fatalf("a stage-only lineage with no assignment row must stay playable, got %v", err)
		}
	})
}

// TestResetRetryableTempoEvidence_TransientOnly pins which tempo outcomes a resumed pass may
// re-derive. A cancelled or unavailable transform is evidence about the run, not an answer about
// the waveform, so only those two are dropped; every deterministic verdict the transform itself
// produced stays committed, and a pass whose only candidates are final keeps its committed
// identity so an idempotent call republishes nothing.
func TestResetRetryableTempoEvidence_TransientOnly(t *testing.T) {
	cases := []struct {
		reason    string
		retryable bool
	}{
		{domain.TempoReasonCancelled, true},
		{domain.TempoReasonToolUnavailable, true},
		{domain.TempoReasonFits, false},
		{domain.TempoReasonOverrun, false},
		{domain.TempoReasonFactorOutOfRange, false},
		{domain.TempoReasonSourceInvalid, false},
		{domain.TempoReasonTransformFailed, false},
		{domain.TempoReasonOutputTooLarge, false},
		{domain.TempoReasonOutputInvalid, false},
	}

	const committedHash = "committed-sha"
	for _, tc := range cases {
		variant := domain.DubSegmentsVariant{
			ID:             uuid.NewString(),
			AssetID:        "asset-tempo-retry",
			RunID:          "run-tempo-retry",
			TargetLanguage: "vi",
			CASHash:        committedHash,
			ReviewSegments: []domain.DubSegmentReview{
				{Index: 0, TempoCandidate: &domain.DubTempoCandidate{Reason: tc.reason, Factor: 1.2}},
			},
		}
		if got := tempoEvidenceRetryable(tc.reason); got != tc.retryable {
			t.Fatalf("tempoEvidenceRetryable(%s) = %v, want %v", tc.reason, got, tc.retryable)
		}
		if got := resetRetryableTempoEvidence(&variant); got != tc.retryable {
			t.Fatalf("resetRetryableTempoEvidence(%s) = %v, want %v", tc.reason, got, tc.retryable)
		}
		if got := variant.ReviewSegments[0].TempoCandidate == nil; got != tc.retryable {
			t.Fatalf("candidate cleared for %s = %v, want %v", tc.reason, got, tc.retryable)
		}
		if got := variant.CASHash == ""; got != tc.retryable {
			t.Fatalf("committed identity dropped for %s = %v, want %v", tc.reason, got, tc.retryable)
		}
	}

	// A cached pass with no tempo evidence at all is not a retry candidate: the fast path must
	// leave it, and its identity, untouched.
	none := domain.DubSegmentsVariant{ID: uuid.NewString(), CASHash: committedHash, ReviewSegments: []domain.DubSegmentReview{{Index: 0}}}
	if resetRetryableTempoEvidence(&none) || none.CASHash != committedHash {
		t.Fatalf("a pass with no tempo evidence must stay committed, got reset with hash %q", none.CASHash)
	}

	// Granularity is per unit: one transient unit is re-derived while a unit whose transform
	// already answered keeps its artifact and verdict.
	mixed := domain.DubSegmentsVariant{
		ID:      uuid.NewString(),
		CASHash: committedHash,
		ReviewSegments: []domain.DubSegmentReview{
			{Index: 0, TempoCandidate: &domain.DubTempoCandidate{Reason: domain.TempoReasonCancelled}},
			{Index: 1, TempoCandidate: &domain.DubTempoCandidate{Reason: domain.TempoReasonFits, TransformedAudioSHA256: strings.Repeat("f", 64), Selectable: true}},
		},
	}
	if !resetRetryableTempoEvidence(&mixed) {
		t.Fatal("expected the transient unit to be reset")
	}
	if mixed.ReviewSegments[0].TempoCandidate != nil {
		t.Fatal("the transient unit's evidence must be cleared for re-derivation")
	}
	kept := mixed.ReviewSegments[1].TempoCandidate
	if kept == nil || kept.Reason != domain.TempoReasonFits || !kept.Selectable || kept.TransformedAudioSHA256 != strings.Repeat("f", 64) {
		t.Fatalf("a deterministic unit verdict must stay committed, got %+v", kept)
	}
}
