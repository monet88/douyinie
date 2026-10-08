package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #156: Generate playable review-only FFmpeg tempo candidates once remedies are exhausted.

func TestDubbingService_Issue156_TempoCandidateEligibleRange(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := "asset-tempo-1"
	runID := "run-tempo-1"
	targetLang := "vi"

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)

	// Block: slot is 1000ms [0, 1000].
	// Probed duration is 1200ms -> factor = 1200 / 1000 = 1.20 (within 1 < factor <= 1.25)
	// DubScript with CanShortenText=false (short text <= 3 words: "Alo một hai") and no same-turn regroup
	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo một hai")

	// Fake TTS injects 1200ms audio (16kHz mono)
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		wav := media.GeneratePCM16WAV(16000, 1, 1200)
		return &provider.TTSSynthesisResult{
			AudioData:           wav,
			AudioSHA256:         "sha256-natural-1200",
			PredictedDurationMs: 1200,
		}, nil
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	// Invariant: Unselected and REVIEW_REQUIRED
	if variant.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("expected overall status REVIEW_REQUIRED, got %s", variant.OverallStatus)
	}
	if len(variant.Segments) != 0 {
		t.Fatalf("overrun candidate must not be selected, got %d segments", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment, got %d", len(variant.ReviewSegments))
	}

	rev := variant.ReviewSegments[0]
	if rev.TempoCandidate == nil {
		t.Fatal("expected tempo candidate to be populated on review segment")
	}
	tc := rev.TempoCandidate
	if tc.Factor != 1.20 {
		t.Fatalf("expected factor 1.20, got %v", tc.Factor)
	}
	if tc.NaturalAudioSHA256 != rev.AudioSHA256 {
		t.Fatalf("expected NaturalAudioSHA256 %s == rev.AudioSHA256 %s", tc.NaturalAudioSHA256, rev.AudioSHA256)
	}
	if tc.TransformedAudioSHA256 == "" || tc.TransformedAudioSHA256 == tc.NaturalAudioSHA256 {
		t.Fatalf("expected distinct transformed hash, got %s", tc.TransformedAudioSHA256)
	}
	// Measured natural: 1200ms. Transformed re-probed duration: ~1000ms.
	if tc.NaturalDurationMs != 1200 {
		t.Fatalf("expected NaturalDurationMs 1200, got %d", tc.NaturalDurationMs)
	}
	if tc.TransformedDurationMs <= 0 || tc.TransformedDurationMs > 1050 {
		t.Fatalf("expected transformed duration near 1000ms, got %d", tc.TransformedDurationMs)
	}
	if !tc.Selectable {
		t.Fatalf("expected selectable=true since ~1000ms fits slot 1000ms, got reason: %s", tc.Reason)
	}
	if tc.Reason != domain.TempoReasonFits {
		t.Fatalf("expected reason %s, got %s", domain.TempoReasonFits, tc.Reason)
	}

	// Verify both natural and transformed audio bytes exist in CAS
	if !casStore.Exists(tc.NaturalAudioSHA256) {
		t.Fatalf("natural audio missing from CAS: %s", tc.NaturalAudioSHA256)
	}
	if !casStore.Exists(tc.TransformedAudioSHA256) {
		t.Fatalf("transformed audio missing from CAS: %s", tc.TransformedAudioSHA256)
	}

	// The pass is published exactly once, after the candidate is attached: the run's index row
	// resolves to a payload that already carries the candidate the operator audits, so no
	// intermediate variant without the evidence was ever the published one.
	idx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
	if err != nil || idx == nil {
		t.Fatalf("expected a published dub segments variant, got idx=%v err=%v", idx, err)
	}
	publishedRC, err := casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("load published variant: %v", err)
	}
	defer publishedRC.Close()
	var published domain.DubSegmentsVariant
	if err := json.NewDecoder(publishedRC).Decode(&published); err != nil {
		t.Fatalf("decode published variant: %v", err)
	}
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
		t.Fatal("the published variant must carry the attached tempo candidate")
	}
	if got := published.ReviewSegments[0].TempoCandidate.TransformedAudioSHA256; got != tc.TransformedAudioSHA256 {
		t.Fatalf("published candidate transformed sha %s != returned %s", got, tc.TransformedAudioSHA256)
	}
}

func TestDubbingService_Issue156_Above125GeneratesNoDSPCandidate(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := "asset-tempo-over125"
	runID := "run-tempo-over125"
	targetLang := "vi"

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)

	// Block: slot 1000ms. Measured audio: 1300ms -> factor = 1.30 > 1.25.
	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo")

	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		wav := media.GeneratePCM16WAV(16000, 1, 1300)
		return &provider.TTSSynthesisResult{AudioData: wav, PredictedDurationMs: 1300}, nil
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment, got %d", len(variant.ReviewSegments))
	}
	rev := variant.ReviewSegments[0]
	if rev.TempoCandidate == nil {
		t.Fatal("expected tempo candidate metadata on review segment")
	}
	tc := rev.TempoCandidate
	// Factor 1.30: no DSP generated
	if tc.Factor != 1.30 {
		t.Fatalf("expected factor 1.30, got %v", tc.Factor)
	}
	if tc.TransformedAudioSHA256 != "" {
		t.Fatalf("expected no transformed audio for factor > 1.25, got %s", tc.TransformedAudioSHA256)
	}
	if tc.Reason != domain.TempoReasonFactorOutOfRange {
		t.Fatalf("expected reason %s, got %s", domain.TempoReasonFactorOutOfRange, tc.Reason)
	}
	if tc.Selectable {
		t.Fatal("factor > 1.25 must not be selectable")
	}
}

// TestDubbingService_Issue156_OversizedOutputRejectedAndNeverCommitted proves that if FFmpeg
// output hits or exceeds the safety bound during writing, the output is rejected with
// TEMPO_OUTPUT_TOO_LARGE, no transformed audio is committed to CAS, and the safety bound
// does NOT become a cropped or accepted candidate.
func TestDubbingService_Issue156_OversizedOutputRejectedAndNeverCommitted(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	const (
		assetID    = "asset-tempo-oversized"
		runID      = "run-tempo-oversized"
		targetLang = "vi"
	)

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)
	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo")

	// TTS generates 1200ms audio
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		wav := media.GeneratePCM16WAV(16000, 1, 1200)
		return &provider.TTSSynthesisResult{AudioData: wav, PredictedDurationMs: 1200}, nil
	}

	// Mock atempo transform returning ErrAtempoOutputTooLarge (as triggered when hitting safety bound)
	dubSvc.AtempoTransform = func(ctx context.Context, req media.AtempoRequest) ([]byte, error) {
		return nil, media.ErrAtempoOutputTooLarge
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit must not fail the pass for oversized transform: %v", err)
	}

	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment, got %d", len(variant.ReviewSegments))
	}
	rev := variant.ReviewSegments[0]
	tc := rev.TempoCandidate
	if tc == nil {
		t.Fatal("expected tempo candidate metadata")
	}

	// Safety bound hit: Reason MUST be TEMPO_OUTPUT_TOO_LARGE
	if tc.Reason != domain.TempoReasonOutputTooLarge {
		t.Fatalf("expected reason %s, got %s", domain.TempoReasonOutputTooLarge, tc.Reason)
	}
	// Must NEVER be committed to CAS or marked selectable/cropped
	if tc.TransformedAudioSHA256 != "" {
		t.Fatalf("oversized transform must not have transformed hash, got %s", tc.TransformedAudioSHA256)
	}
	if tc.Selectable {
		t.Fatal("oversized transform must NEVER become an accepted or selectable candidate")
	}
	if len(variant.Segments) != 0 {
		t.Fatalf("oversized transform must not select any segments, got %d", len(variant.Segments))
	}
}

func TestDubbingService_Issue156_TransformedStillOverrunMarkedUnselectable(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := "asset-tempo-residual"
	runID := "run-tempo-residual"
	targetLang := "vi"

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)

	// Deterministic residual overrun fixture: a 1000ms synthesized sine wave against a slot the
	// fixture keeps shorter than it, so the factor the fit controller derives stays inside
	// 1 < factor <= 1.25 and the transform still lands over the accepted window. Selectable MUST
	// be false with Reason TempoReasonOverrun, and the output must NOT be cropped or retried to
	// force duration - the assertion below re-measures the produced bytes, not a prediction.
	const (
		slotMs    int64 = 922
		naturalMs int64 = 1000
	)
	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, slotMs, "Alo")

	// Generate 1000ms 16kHz sine wave PCM16
	sampleRate := 16000
	numSamples := (sampleRate * int(naturalMs)) / 1000
	sineSamples := make([]int16, numSamples)
	for i := range sineSamples {
		sineSamples[i] = int16(5000.0 * math.Sin(2.0*math.Pi*440.0*float64(i)/float64(sampleRate)))
	}
	sineWAV := media.EncodePCM16Samples(sineSamples, sampleRate, 1)

	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		return &provider.TTSSynthesisResult{AudioData: sineWAV, PredictedDurationMs: naturalMs}, nil
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	if len(variant.ReviewSegments) == 0 {
		t.Fatal("expected at least one review segment")
	}
	rev := variant.ReviewSegments[0]
	tc := rev.TempoCandidate
	if tc == nil {
		t.Fatal("expected tempo candidate")
	}
	if tc.TransformedAudioSHA256 == "" {
		t.Fatal("expected transformed audio hash to be populated")
	}
	rc, err := casStore.Get(tc.TransformedAudioSHA256)
	if err != nil {
		t.Fatalf("get transformed audio: %v", err)
	}
	defer rc.Close()
	audioBytes, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read transformed audio: %v", err)
	}
	header, err := media.ParseWAVHeader(audioBytes)
	if err != nil {
		t.Fatalf("parse transformed wav header: %v", err)
	}
	// Verify actual bytes produced: 923ms > 922ms (residual overrun)
	if header.DurationMs <= slotMs {
		t.Fatalf("fixture expected residual overrun > %dms, got %dms", slotMs, header.DurationMs)
	}
	if tc.Selectable {
		t.Fatalf("residual overrun must be unselectable (Selectable=false), got true with duration %dms > slot %dms", header.DurationMs, slotMs)
	}
	if tc.Reason != domain.TempoReasonOverrun {
		t.Fatalf("expected TempoReasonOverrun, got %s", tc.Reason)
	}
}

// TestDubbingService_Issue156_CancelledPassPublishesEvidenceNotCanonicalMedia pins the
// cancellation clause of the slice: a transform cancelled after synthesis completed is recorded
// as TEMPO_CANCELLED evidence and published so the operator can read why the alternative is
// missing, while no canonical media is produced - no selected segment and no transformed
// artifact. Run control itself is unaffected: the caller's own cancellation check stops the
// run, so a resumed run re-derives the candidate from a stage that is not succeeded.
func TestDubbingService_Issue156_CancelledPassPublishesEvidenceNotCanonicalMedia(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	const (
		assetID    = "asset-tempo-cancel"
		runID      = "run-tempo-cancel"
		targetLang = "vi"
	)

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)

	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo")

	ctx, cancel := context.WithCancel(context.Background())
	dubSvc.TTSInvoke = func(callCtx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		cancel() // cancel right after TTS finishes but before or during tempo transform
		wav := media.GeneratePCM16WAV(16000, 1, 1200)
		return &provider.TTSSynthesisResult{AudioData: wav, PredictedDurationMs: 1200}, nil
	}

	variant, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled propagated to caller, got %v", err)
	}
	if variant == nil {
		t.Fatal("expected published variant returned with cancellation error for caller inspection")
	}
	// No canonical media: nothing is selected for playback and no transformed artifact exists.
	if len(variant.Segments) != 0 {
		t.Fatalf("a cancelled pass must not select segments, got %d", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 || variant.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("expected one review segment carrying cancellation evidence, got %+v", variant.ReviewSegments)
	}
	tc := variant.ReviewSegments[0].TempoCandidate
	if tc.Reason != domain.TempoReasonCancelled {
		t.Fatalf("expected %s, got %s", domain.TempoReasonCancelled, tc.Reason)
	}
	if tc.TransformedAudioSHA256 != "" || tc.Selectable {
		t.Fatalf("a cancelled transform must commit no artifact and stay unselectable, got sha=%q selectable=%v", tc.TransformedAudioSHA256, tc.Selectable)
	}

	// The evidence is published - the operator reads it - and it is the only thing published.
	idx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
	if err != nil || idx == nil {
		t.Fatalf("expected the evidence-bearing variant to be published, got idx=%v err=%v", idx, err)
	}
	publishedRC, err := casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("load published variant: %v", err)
	}
	defer publishedRC.Close()
	var published domain.DubSegmentsVariant
	if err := json.NewDecoder(publishedRC).Decode(&published); err != nil {
		t.Fatalf("decode published variant: %v", err)
	}
	if len(published.Segments) != 0 {
		t.Fatalf("the published variant must not carry selected segments after cancellation, got %d", len(published.Segments))
	}
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
		t.Fatal("the published variant must carry the cancellation evidence")
	}
	if got := published.ReviewSegments[0].TempoCandidate.Reason; got != domain.TempoReasonCancelled {
		t.Fatalf("persisted reason must be %s, got %s", domain.TempoReasonCancelled, got)
	}
}

// TestDubbingService_Issue156_MidSubprocessCancellationProvesIntegrity proves that cancelling
// while the FFmpeg subprocess is actively running:
// 1. Really kills the active helper subprocess (proven via ATEMPO_HELPER_MARKER).
// 2. Produces no partial transformed audio file or hash in CAS.
// 3. Attaches TempoCandidate with Reason=TEMPO_CANCELLED and original factor/playback/natural evidence.
// 4. Persists the review evidence safely into CAS/Index.
// 5. Propagates context.Canceled error to the caller so RuntimeHost does not mark dub_synthesize succeeded.
func TestDubbingService_Issue156_MidSubprocessCancellationProvesIntegrity(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	const (
		assetID    = "asset-mid-subproc-cancel"
		runID      = "run-mid-subproc-cancel"
		targetLang = "vi"
	)

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)
	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo")

	// Build the controllable helper stand-in for ffmpeg
	helperBin := filepath.Join(t.TempDir(), "atempohelper.exe")
	src := filepath.Join("..", "media", "testdata", "atempohelper", "main.go")
	cmd := exec.Command("go", "build", "-o", helperBin, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build atempo helper (%s): %v\n%s", src, err, string(out))
	}

	markerFile := filepath.Join(t.TempDir(), "started.marker")
	t.Setenv("ATEMPO_HELPER_MARKER", markerFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// TTS generates 1200ms audio against 1000ms slot (in-range factor 1.20)
	dubSvc.TTSInvoke = func(callCtx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		wav := media.GeneratePCM16WAV(16000, 1, 1200)
		return &provider.TTSSynthesisResult{AudioData: wav, PredictedDurationMs: 1200}, nil
	}

	// Override transform to use the controllable helper and cancel context only after helper started
	dubSvc.AtempoTransform = func(callCtx context.Context, req media.AtempoRequest) ([]byte, error) {
		req.FFmpegPath = helperBin
		go func() {
			// Wait for the helper to write its marker (proving it really started), then cancel
			// its context. The bounded wait must cancel on the timeout path too: a helper that
			// never signalled would otherwise leave the transform blocked and hang the suite.
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(markerFile); err == nil {
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Cancel while the helper subprocess is actively running.
			cancel()
		}()
		return media.ApplyAtempoWAV(callCtx, req)
	}

	variant, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})

	// Assertion 1: SynthesizeAndFit MUST return cancellation error
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled returned to caller, got %v", err)
	}
	if variant == nil {
		t.Fatal("expected published variant returned alongside cancellation")
	}

	// Assertion 2: Helper process really started before cancellation
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatalf("helper process did not start: marker missing: %v", err)
	}

	// Assertion 3: No transformed hash/media
	if len(variant.Segments) != 0 {
		t.Fatalf("cancelled pass must select zero segments, got %d", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 || variant.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("expected 1 review segment with tempo candidate, got %+v", variant.ReviewSegments)
	}
	tc := variant.ReviewSegments[0].TempoCandidate
	if tc.TransformedAudioSHA256 != "" || tc.Selectable {
		t.Fatalf("cancelled transform must have empty transformed sha and unselectable, got sha=%q selectable=%v", tc.TransformedAudioSHA256, tc.Selectable)
	}
	if tc.Factor != 1.20 {
		t.Fatalf("expected factor 1.20 recorded, got %v", tc.Factor)
	}
	if tc.NaturalDurationMs != 1200 || tc.PlaybackDurationMs != 1000 {
		t.Fatalf("expected natural 1200ms and playback 1000ms, got nat=%d play=%d", tc.NaturalDurationMs, tc.PlaybackDurationMs)
	}
	if tc.Reason != domain.TempoReasonCancelled {
		t.Fatalf("expected reason %s, got %s", domain.TempoReasonCancelled, tc.Reason)
	}

	// Assertion 4: Persisted DubSegmentsVariant review evidence contains TEMPO_CANCELLED
	idx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
	if err != nil || idx == nil {
		t.Fatalf("expected variant index persisted, got err=%v idx=%v", err, idx)
	}
	rc, err := casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("load persisted variant from CAS: %v", err)
	}
	defer rc.Close()
	var published domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&published); err != nil {
		t.Fatalf("decode persisted variant: %v", err)
	}
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
		t.Fatal("persisted variant must have review segment with candidate")
	}
	persistedTC := published.ReviewSegments[0].TempoCandidate
	if persistedTC.Reason != domain.TempoReasonCancelled {
		t.Fatalf("persisted reason must be %s, got %s", domain.TempoReasonCancelled, persistedTC.Reason)
	}
	if persistedTC.TransformedAudioSHA256 != "" {
		t.Fatalf("persisted transformed sha must be empty, got %s", persistedTC.TransformedAudioSHA256)
	}
}

// TestDubbingService_Issue156_CancelledDubSynthesizeDoesNotMarkSucceeded proves that when
// cancellation strikes mid-transform, the stage cannot be marked StageStatusSucceeded:
// SynthesizeAndFit propagates the cancellation error, allowing the stage controller to record
// StageStatusInterrupted rather than falsely recording success.
func TestDubbingService_Issue156_CancelledDubSynthesizeDoesNotMarkSucceeded(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	const (
		assetID    = "asset-stage-cancel"
		runID      = "run-stage-cancel"
		jobID      = "job-stage-cancel"
		targetLang = "vi"
	)

	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)
	scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo")

	now := time.Now().UTC()
	se := domain.StageExecution{
		ID:        uuid.NewString(),
		RunID:     runID,
		Stage:     "dub_synthesize",
		Status:    domain.StageStatusRunning,
		StartedAt: &now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateStageExecution(context.Background(), se); err != nil {
		t.Fatalf("create stage execution: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	dubSvc.TTSInvoke = func(callCtx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		wav := media.GeneratePCM16WAV(16000, 1, 1200)
		return &provider.TTSSynthesisResult{AudioData: wav, PredictedDurationMs: 1200}, nil
	}
	dubSvc.AtempoTransform = func(callCtx context.Context, req media.AtempoRequest) ([]byte, error) {
		cancel() // simulate cancellation during atempo execution
		return nil, callCtx.Err()
	}

	variant, ttsErr := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:                 runID,
		AssetID:               assetID,
		JobID:                 jobID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   scriptCAS,
		VoiceAssignmentCAS:    assignCAS,
		TranscriptArtifactCAS: transcriptCAS,
	})

	// Invariant: SynthesizeAndFit MUST return an error when cancelled
	if ttsErr == nil || !errors.Is(ttsErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", ttsErr)
	}

	// Emulate RuntimeHost stage controller logic:
	// When ttsErr != nil, the stage MUST NOT transition to StageStatusSucceeded.
	if ttsErr != nil {
		if errors.Is(ttsErr, context.Canceled) {
			se.Status = domain.StageStatusInterrupted
		} else {
			se.Status = domain.StageStatusFailed
		}
		se.ErrorMessage = ttsErr.Error()
		nowFin := time.Now().UTC()
		se.CompletedAt = &nowFin
		se.UpdatedAt = nowFin
		if err := db.UpdateStageExecution(context.Background(), se); err != nil {
			t.Fatalf("update stage execution: %v", err)
		}
	} else {
		// This branch must NOT be reached on cancellation!
		se.Status = domain.StageStatusSucceeded
		se.ArtifactSHA256 = variant.CASHash
		_ = db.UpdateStageExecution(context.Background(), se)
	}

	stages, err := db.ListStageExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stage executions: %v", err)
	}
	var dubStage *domain.StageExecution
	for i := range stages {
		if stages[i].Stage == "dub_synthesize" {
			dubStage = &stages[i]
			break
		}
	}
	if dubStage == nil {
		t.Fatal("stage dub_synthesize not found")
	}
	if dubStage.Status == domain.StageStatusSucceeded {
		t.Fatalf("stage dub_synthesize MUST NOT be marked succeeded on cancellation: got %s", dubStage.Status)
	}
	if dubStage.Status != domain.StageStatusInterrupted {
		t.Fatalf("expected stage status %s, got %s", domain.StageStatusInterrupted, dubStage.Status)
	}
}

func TestDubbingService_Issue156_TempoDeferredUntilEscalationExhausted(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required on test host")
	}

	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := "asset-escalation-tempo-order"
	runID := "run-escalation-tempo-order"
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	const (
		segmentSlotMs int64 = 1000
		pass1Ms       int64 = 1500 // > 1.25 -> fixed-rate overrun, triggers escalation
		pass2Ms       int64 = 1200 // 1200 / 1000 = 1.20 -> final pass derives exactly one atempo candidate
	)

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          segmentSlotMs,
				SlotDurationMs: segmentSlotMs,
				SourceText:     "你好",
				MeaningText:    "Alo",
				SpokenText:     "Alo",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:             runID,
		AssetID:           assetID,
		TargetLanguage:    "vi",
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": provider.DefaultPresetVoices("vi")[0]},
	})
	if err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}

	fixedProvider, ok := reg.Get("fake_zerotts_tts_vi")
	if !ok {
		t.Fatal("fixed-rate lane must be registered")
	}
	fixedFake := fixedProvider.(*provider.FakeTTSProvider)
	fixedFake.DurationMs = pass1Ms

	// Register fallback lane (CosyVoice)
	fallbackFake := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", pass2Ms)
	if err := reg.Register(fallbackFake); err != nil {
		t.Fatalf("register fallback lane: %v", err)
	}
	if err := governance.NewLicenseService(db).RegisterManifest(context.Background(), domain.LicenseManifestEntry{
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
	}); err != nil {
		t.Fatalf("register fallback lane manifest: %v", err)
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit: %v", err)
	}

	// Prove escalation ran once
	if len(variant.Escalations) != 1 {
		t.Fatalf("expected one escalation, got %+v", variant.Escalations)
	}

	// Prove final pass carries the tempo candidate derived from the final (pass 2) waveform
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment, got %d", len(variant.ReviewSegments))
	}
	rev := variant.ReviewSegments[0]
	tc := rev.TempoCandidate
	if tc == nil {
		t.Fatal("expected tempo candidate on final unresolved escalated segment")
	}
	if tc.NaturalDurationMs != pass2Ms {
		t.Fatalf("tempo candidate must be derived from the final escalated waveform (%dms), got %dms", pass2Ms, tc.NaturalDurationMs)
	}
	if tc.Factor != float64(pass2Ms)/float64(segmentSlotMs) {
		t.Fatalf("expected factor %v, got %v", float64(pass2Ms)/float64(segmentSlotMs), tc.Factor)
	}
	if tc.TransformedAudioSHA256 == "" {
		t.Fatal("expected transformed audio hash to be populated on final pass")
	}
}

// seedTempoReviewFixture persists the run-scoped transcript, dubbing script and voice
// assignment every Issue #156 tempo case needs, so each case states only the slot it must
// overrun and the spoken text under test. It returns the script, assignment and transcript
// CAS hashes the caller passes to SynthesizeAndFit.
func seedTempoReviewFixture(
	t *testing.T, db *storage.DB, casStore *cas.Store, assetID, runID, targetLang string,
	slotMs int64, spokenText string,
) (scriptCAS, assignCAS, transcriptCAS string) {
	t.Helper()
	transcriptCAS = service.SeedRunTranscriptForTest(context.Background(), db, casStore, assetID, runID,
		domain.SpeechBlock{Index: 0, StartMs: 0, EndMs: slotMs, SpeakerID: "SPEAKER_00", SourceText: "测试音频"})

	script := domain.DubScriptVariant{
		ID: uuid.NewString(), AssetID: assetID, RunID: runID, TargetLanguage: targetLang,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: slotMs, SlotDurationMs: slotMs, SourceText: "测试音频", MeaningText: spokenText, SpokenText: spokenText, PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &script, runID)
	scriptBytes, _ := json.Marshal(&script)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assignment := domain.VoiceAssignment{
		ID: uuid.NewString(), AssetID: assetID, RunID: runID, TargetLanguage: targetLang,
		DubScriptVariantCAS:   scriptObj.SHA256,
		TranscriptArtifactCAS: transcriptCAS,
		Assignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {ID: "fake_zerotts_tts_vi_voice_1", ProviderID: "fake_zerotts_tts_vi", VoiceID: "voice_1"},
		},
		CreatedAt: time.Now().UTC(),
	}
	assignBytes, _ := json.Marshal(&assignment)
	assignObj, _ := casStore.Put(bytes.NewReader(assignBytes))
	return scriptObj.SHA256, assignObj.SHA256, transcriptCAS
}

// TestDubbingService_Issue156_PreEscalationPassPublishedBeforeEscalationFailure proves the
// pre-escalation pass is CAS-committed and run-indexed before the escalation that supersedes
// it. The escalation persists a superseding VoiceAssignment, so when that write fails the
// already-completed pass must still be the run's published artifact instead of being lost.
// That artifact predates the escalation and its remedy sequence is not exhausted, so it
// carries no tempo candidate and no escalation evidence.
func TestDubbingService_Issue156_PreEscalationPassPublishedBeforeEscalationFailure(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	ctx := context.Background()
	assetID := "asset-escalation-publish-order"
	runID := "run-escalation-publish-order"
	targetLang := "vi"
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)

	const (
		segmentSlotMs int64 = 1000
		overrunLaneMs int64 = 1500 // > 1.25 of the slot: a fixed-rate overrun that escalates
	)

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: targetLang,
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          segmentSlotMs,
				SlotDurationMs: segmentSlotMs,
				SourceText:     "你好",
				MeaningText:    "Alo",
				SpokenText:     "Alo",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assign, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID:             runID,
		AssetID:           assetID,
		TargetLanguage:    targetLang,
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": provider.DefaultPresetVoices("vi")[0]},
	})
	if err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}

	fixedProvider, ok := reg.Get("fake_zerotts_tts_vi")
	if !ok {
		t.Fatal("fixed-rate lane must be registered")
	}
	fixedProvider.(*provider.FakeTTSProvider).DurationMs = overrunLaneMs

	// The duration-controlled fallback lane the escalation targets, registered before the run
	// so the escalation is policy/license eligible and the plan is non-nil.
	fallbackFake := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", overrunLaneMs)
	if err := reg.Register(fallbackFake); err != nil {
		t.Fatalf("register fallback lane: %v", err)
	}
	if err := governance.NewLicenseService(db).RegisterManifest(ctx, domain.LicenseManifestEntry{
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
	}); err != nil {
		t.Fatalf("register fallback lane manifest: %v", err)
	}

	// Inject a failure into the escalation's own persistence step (the superseding
	// VoiceAssignment index write), leaving the pass commit and every read untouched.
	abort := "injected escalation persistence failure"
	_ = db.QueryRow(ctx, `CREATE TRIGGER issue156_fail_escalation_insert BEFORE INSERT ON voice_assignments
		BEGIN SELECT RAISE(ABORT, '`+abort+`'); END`).Scan(new(any))
	_ = db.QueryRow(ctx, `CREATE TRIGGER issue156_fail_escalation_update BEFORE UPDATE ON voice_assignments
		BEGIN SELECT RAISE(ABORT, '`+abort+`'); END`).Scan(new(any))

	if _, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      targetLang,
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	}); err == nil {
		t.Fatal("expected the injected escalation persistence failure to surface")
	}

	// The completed pre-escalation pass must be the run's published artifact despite the
	// escalation failing afterwards.
	idx, err := db.GetDubSegmentsVariantIndex(ctx, assetID, targetLang)
	if err != nil || idx == nil {
		t.Fatalf("the completed pre-escalation pass must be published before escalation: idx=%v err=%v", idx, err)
	}
	publishedRC, err := casStore.Get(idx.CASHash)
	if err != nil {
		t.Fatalf("load published variant: %v", err)
	}
	defer publishedRC.Close()
	var published domain.DubSegmentsVariant
	if err := json.NewDecoder(publishedRC).Decode(&published); err != nil {
		t.Fatalf("decode published variant: %v", err)
	}
	if published.OverallStatus != "REVIEW_REQUIRED" {
		t.Fatalf("the published pre-escalation pass must stay unresolved, got %s", published.OverallStatus)
	}
	if len(published.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment on the published pass, got %d", len(published.ReviewSegments))
	}
	if published.ReviewSegments[0].TempoCandidate != nil {
		t.Fatal("the pre-escalation pass must carry no tempo candidate: its remedy sequence is not exhausted")
	}
	if len(published.Escalations) != 0 {
		t.Fatalf("the published pass must predate the escalation, got %+v", published.Escalations)
	}
}

const (
	movedOnRaceAssetID = "asset-moved-on-assignment"
	movedOnRaceRunID   = "run-moved-on-assignment"
	movedOnRaceLang    = "vi"
	movedOnRaceSlotMs  = int64(1000)
	movedOnRaceLaneMs  = int64(1200) // 1.20 of the slot: an eligible atempo overrun
)

// seedMovedOnAssignmentRace seeds the concurrent-reassignment race this slice must survive. The
// fixed-rate assignment A is the base an in-flight call synthesizes under, with its escalation
// available so that call reaches the escalation decision; the operator assignment B - a general
// lane that is neither A's lane nor the escalation target, so B's own pass is final - becomes the
// run's current assignment when mintReassign is called. The tempo transform is stubbed and
// counted, so a case can prove exactly how many transforms ran.
func seedMovedOnAssignmentRace(t *testing.T) (
	dubSvc *service.DubbingService,
	db *storage.DB,
	casStore *cas.Store,
	scriptCAS string,
	assignA *domain.VoiceAssignment,
	mintReassign func() *domain.VoiceAssignment,
	transformCalls func() int,
) {
	t.Helper()
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	setupAssetJobRunAudioRole(t, db, casStore, movedOnRaceAssetID, movedOnRaceRunID, movedOnRaceLang)

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        movedOnRaceAssetID,
		RunID:          movedOnRaceRunID,
		SourceLanguage: "zh",
		TargetLanguage: movedOnRaceLang,
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          movedOnRaceSlotMs,
				SlotDurationMs: movedOnRaceSlotMs,
				SourceText:     "你好",
				MeaningText:    "Alo",
				SpokenText:     "Alo",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, movedOnRaceRunID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptBytes))

	// The duration-controlled fallback lane A's escalation targets: registered and licensed before
	// the run, so A's call plans an escalation instead of finalizing its own pass.
	fallbackFake := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", movedOnRaceLaneMs)
	if err := reg.Register(fallbackFake); err != nil {
		t.Fatalf("register fallback lane: %v", err)
	}
	if err := governance.NewLicenseService(db).RegisterManifest(ctx, domain.LicenseManifestEntry{
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
	}); err != nil {
		t.Fatalf("register fallback lane manifest: %v", err)
	}

	for _, laneID := range []string{"fake_zerotts_tts_vi", "fake_vieneu_tts_vi"} {
		lane, ok := reg.Get(laneID)
		if !ok {
			t.Fatalf("lane %s must be registered", laneID)
		}
		lane.(*provider.FakeTTSProvider).DurationMs = movedOnRaceLaneMs
	}

	assignA, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID:             movedOnRaceRunID,
		AssetID:           movedOnRaceAssetID,
		TargetLanguage:    movedOnRaceLang,
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": provider.DefaultPresetVoices("vi")[0]},
	})
	if err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}

	// The operator's reassignment, minted exactly as the inspector's reassignment path mints it.
	// It is callable rather than pre-applied so a case chooses when it lands: before the in-flight
	// call, or inside that call's publish window.
	mintReassign = func() *domain.VoiceAssignment {
		t.Helper()
		assignB, err := dubSvc.ReassignVoice(ctx, domain.VoiceAssignmentInput{
			RunID:          movedOnRaceRunID,
			AssetID:        movedOnRaceAssetID,
			TargetLanguage: movedOnRaceLang,
			CustomAssignments: map[string]domain.VoiceProfile{
				"SPEAKER_00": {ID: "vieneu_vi_alt", ProviderID: "fake_vieneu_tts_vi", VoiceID: "vieneu_vi_alt", Language: movedOnRaceLang},
			},
		})
		if err != nil {
			t.Fatalf("ReassignVoice: %v", err)
		}
		if assignB.CASHash == assignA.CASHash {
			t.Fatal("precondition: the reassignment must mint a distinct current assignment")
		}
		return assignB
	}

	calls := 0
	dubSvc.AtempoTransform = func(callCtx context.Context, req media.AtempoRequest) ([]byte, error) {
		calls++
		return media.GeneratePCM16WAV(16000, 1, movedOnRaceSlotMs), nil
	}
	return dubSvc, db, casStore, scriptObj.SHA256, assignA, mintReassign, func() int { return calls }
}

// TestDubbingService_Issue156_MovedOnRunDerivesEvidenceOnItsOwnAssignmentPass pins the
// concurrent-reassignment contract for this slice. When an operator reassignment lands while a
// request is in flight, the pass that request synthesized belongs to the superseded assignment:
// it stays committed and readable, is returned as REVIEW evidence, gains NO tempo candidate
// (attaching one would republish superseded-assignment audio as the run's canonical media), and
// the operator's newer assignment is never reverted. The audition evidence belongs to the current
// assignment's own pass, synthesized by its own request from its own retained waveform, so a run
// that ends exhausted under the current assignment always has its candidate.
func TestDubbingService_Issue156_MovedOnRunDerivesEvidenceOnItsOwnAssignmentPass(t *testing.T) {
	dubSvc, db, casStore, scriptCAS, assignA, mintReassign, transformCalls := seedMovedOnAssignmentRace(t)
	ctx := context.Background()
	assignB := mintReassign()

	stale, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               movedOnRaceRunID,
		AssetID:             movedOnRaceAssetID,
		TargetLanguage:      movedOnRaceLang,
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignA.CASHash,
	})
	if err != nil {
		t.Fatalf("the superseded-base synthesis must complete: %v", err)
	}
	if stale.OverallStatus != "REVIEW_REQUIRED" || len(stale.Segments) != 0 {
		t.Fatalf("the superseded pass must stay unresolved and unselected, got status=%s segments=%d", stale.OverallStatus, len(stale.Segments))
	}
	if len(stale.ReviewSegments) != 1 || stale.ReviewSegments[0].TempoCandidate != nil {
		t.Fatalf("the superseded pass must carry no tempo candidate, got %+v", stale.ReviewSegments)
	}
	if len(stale.Escalations) != 0 {
		t.Fatalf("the superseded pass must not record an escalation, got %+v", stale.Escalations)
	}
	if stale.CASHash == "" || !casStore.Exists(stale.CASHash) {
		t.Fatalf("the completed pass must stay committed and readable, got %q", stale.CASHash)
	}
	if got := transformCalls(); got != 0 {
		t.Fatalf("no DSP may run for a superseded pass, got %d transform call(s)", got)
	}
	if idx, err := db.GetVoiceAssignmentIndexByRun(ctx, movedOnRaceAssetID, movedOnRaceRunID, movedOnRaceLang); err != nil || idx == nil || idx.CASHash != assignB.CASHash {
		t.Fatalf("the newer assignment must stay in force, got idx=%v err=%v", idx, err)
	}
	// Nothing had published a pass for the run yet. The superseded pass does not claim the run's
	// dub-variant row, so the run resolves to no artifact until the current assignment's own pass
	// publishes - rather than resolving to superseded-assignment audio (#156).
	if idx, err := db.GetDubSegmentsVariantIndexByRun(ctx, movedOnRaceRunID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a superseded pass must not claim the run's row, got idx=%v err=%v", idx, err)
	}

	// The current assignment's own request synthesizes its own pass, which derives its own
	// candidate from its own retained waveform.
	final, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               movedOnRaceRunID,
		AssetID:             movedOnRaceAssetID,
		TargetLanguage:      movedOnRaceLang,
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignB.CASHash,
	})
	if err != nil {
		t.Fatalf("the current assignment's synthesis failed: %v", err)
	}
	if final.VoiceAssignmentCAS != assignB.CASHash {
		t.Fatalf("the final pass must belong to the current assignment, got %s", final.VoiceAssignmentCAS)
	}
	if len(final.ReviewSegments) != 1 || final.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("the current assignment's pass must derive its own candidate, got %+v", final.ReviewSegments)
	}
	tc := final.ReviewSegments[0].TempoCandidate
	if !tc.Selectable || tc.Reason != domain.TempoReasonFits || tc.TransformedAudioSHA256 == "" {
		t.Fatalf("expected a selectable in-window candidate, got %+v", tc)
	}
	if tc.NaturalDurationMs != movedOnRaceLaneMs {
		t.Fatalf("the candidate must come from the current pass's own waveform (%dms), got %dms", movedOnRaceLaneMs, tc.NaturalDurationMs)
	}
	if got := transformCalls(); got != 1 {
		t.Fatalf("exactly one transform belongs to the current pass, got %d", got)
	}
	if final.OverallStatus != "REVIEW_REQUIRED" || len(final.Segments) != 0 {
		t.Fatalf("the candidate must stay unselected and REVIEW_REQUIRED, got status=%s segments=%d", final.OverallStatus, len(final.Segments))
	}

	// The run's published artifact is the pass that carries the evidence; the superseded artifact
	// stays readable history without one.
	idx, err := db.GetDubSegmentsVariantIndexByRun(ctx, movedOnRaceRunID)
	if err != nil || idx == nil || idx.CASHash != final.CASHash {
		t.Fatalf("the run's published artifact must be the current assignment's pass, got idx=%v err=%v", idx, err)
	}
	published := loadCasVariant(t, casStore, idx.CASHash)
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("the published artifact must carry the derived candidate, got %+v", published.ReviewSegments)
	}
	if superseded := loadCasVariant(t, casStore, stale.CASHash); superseded.ReviewSegments[0].TempoCandidate != nil {
		t.Fatal("the superseded artifact must stay without a tempo candidate")
	}
}

// TestDubbingService_Issue156_SupersededPublishNeverClaimsTheRunVariantRow pins the other ordering
// of the same race: the current assignment's pass is published first, and the in-flight call for the
// superseded assignment completes afterwards. That pass is still committed history, but it must not
// take the run's canonical dub-variant row - the row the inspector, the review projection and
// playback all resolve - because then the run would resolve to superseded-assignment audio and the
// current assignment's tempo evidence would be unreachable.
func TestDubbingService_Issue156_SupersededPublishNeverClaimsTheRunVariantRow(t *testing.T) {
	dubSvc, db, casStore, scriptCAS, assignA, mintReassign, transformCalls := seedMovedOnAssignmentRace(t)
	ctx := context.Background()
	assignB := mintReassign()

	current, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               movedOnRaceRunID,
		AssetID:             movedOnRaceAssetID,
		TargetLanguage:      movedOnRaceLang,
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignB.CASHash,
	})
	if err != nil {
		t.Fatalf("the current assignment's synthesis failed: %v", err)
	}
	if len(current.ReviewSegments) != 1 || current.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("precondition: the current assignment's pass must carry its own candidate, got %+v", current.ReviewSegments)
	}
	if idx, err := db.GetDubSegmentsVariantIndexByRun(ctx, movedOnRaceRunID); err != nil || idx == nil || idx.CASHash != current.CASHash {
		t.Fatalf("precondition: the current pass must own the run's row, got idx=%v err=%v", idx, err)
	}

	// The in-flight call for the superseded assignment now completes: it publishes its own pass and
	// finds that the run has moved on to the assignment the operator chose later.
	stale, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               movedOnRaceRunID,
		AssetID:             movedOnRaceAssetID,
		TargetLanguage:      movedOnRaceLang,
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignA.CASHash,
	})
	if err != nil {
		t.Fatalf("the superseded-base synthesis must complete: %v", err)
	}
	if stale.CASHash == "" || stale.CASHash == current.CASHash || !casStore.Exists(stale.CASHash) {
		t.Fatalf("the superseded pass must stay committed as its own artifact, got %q", stale.CASHash)
	}
	if len(stale.ReviewSegments) != 1 || stale.ReviewSegments[0].TempoCandidate != nil {
		t.Fatalf("the superseded pass must carry no tempo candidate, got %+v", stale.ReviewSegments)
	}
	if got := transformCalls(); got != 1 {
		t.Fatalf("only the current assignment's pass may transform, got %d transform call(s)", got)
	}
	if idx, err := db.GetDubSegmentsVariantIndexByRun(ctx, movedOnRaceRunID); err != nil || idx == nil || idx.CASHash != current.CASHash {
		t.Fatalf("a superseded publish must hand the run's row back, got idx=%v err=%v", idx, err)
	}
	published := loadCasVariant(t, casStore, current.CASHash)
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("the run's row must resolve to the pass that carries the evidence, got %+v", published.ReviewSegments)
	}
	if superseded := loadCasVariant(t, casStore, stale.CASHash); superseded.ReviewSegments[0].TempoCandidate != nil {
		t.Fatal("the superseded artifact must stay without a tempo candidate")
	}
}

// TestDubbingService_Issue156_ReassignmentInsidePublishWindowRefusesStaleRowClaim forces the one
// interleaving a decision taken earlier in the request cannot cover: the reassignment lands after
// the pre-escalation pass has been synthesized and at the moment it tries to claim the run's
// dub-variant row, so the escalation decision the request makes afterwards is the first thing to
// observe it. The stale pass must still be committed and CAS-readable - it is the artifact this
// call returns as REVIEW evidence - but it must never own the run's dub-variant row, and it must
// gain no tempo candidate. The row belongs to the current assignment's own pass, synthesized by
// its own request, which carries its own tempo evidence.
func TestDubbingService_Issue156_ReassignmentInsidePublishWindowRefusesStaleRowClaim(t *testing.T) {
	dubSvc, db, casStore, scriptCAS, assignA, mintReassign, transformCalls := seedMovedOnAssignmentRace(t)
	ctx := context.Background()

	// The reassignment fires exactly once, at the stale pass's row claim: the pass is synthesized
	// and about to be published, and the escalation decision has not run yet.
	var assignB *domain.VoiceAssignment
	dubSvc.SetBeforeRunRowClaimForTest(func() {
		if assignB == nil {
			assignB = mintReassign()
		}
	})

	stale, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               movedOnRaceRunID,
		AssetID:             movedOnRaceAssetID,
		TargetLanguage:      movedOnRaceLang,
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignA.CASHash,
	})
	if err != nil {
		t.Fatalf("the superseded-base synthesis must complete: %v", err)
	}
	if assignB == nil {
		t.Fatal("precondition: the reassignment must have landed inside the publish window")
	}
	dubSvc.SetBeforeRunRowClaimForTest(nil)

	if stale.OverallStatus != "REVIEW_REQUIRED" || len(stale.Segments) != 0 {
		t.Fatalf("the superseded pass must stay unresolved and unselected, got status=%s segments=%d", stale.OverallStatus, len(stale.Segments))
	}
	if len(stale.ReviewSegments) != 1 || stale.ReviewSegments[0].TempoCandidate != nil {
		t.Fatalf("the superseded pass must carry no tempo candidate, got %+v", stale.ReviewSegments)
	}
	if len(stale.Escalations) != 0 {
		t.Fatalf("the superseded pass must not record an escalation, got %+v", stale.Escalations)
	}
	if stale.CASHash == "" || !casStore.Exists(stale.CASHash) {
		t.Fatalf("the superseded pass must stay committed and readable, got %q", stale.CASHash)
	}
	if got := transformCalls(); got != 0 {
		t.Fatalf("no DSP may run for a superseded pass, got %d transform call(s)", got)
	}
	if idx, err := db.GetVoiceAssignmentIndexByRun(ctx, movedOnRaceAssetID, movedOnRaceRunID, movedOnRaceLang); err != nil || idx == nil || idx.CASHash != assignB.CASHash {
		t.Fatalf("the reassignment that landed mid-publish must stay in force, got idx=%v err=%v", idx, err)
	}
	// The claim that raced the reassignment is refused by the claim statement itself.
	if idx, err := db.GetDubSegmentsVariantIndexByRun(ctx, movedOnRaceRunID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a pass superseded mid-publish must not claim the run's row, got idx=%v err=%v", idx, err)
	}

	// The current assignment's own request synthesizes the pass that owns the row, and derives its
	// own candidate from its own retained waveform.
	final, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID:               movedOnRaceRunID,
		AssetID:             movedOnRaceAssetID,
		TargetLanguage:      movedOnRaceLang,
		DubScriptVariantCAS: scriptCAS,
		VoiceAssignmentCAS:  assignB.CASHash,
	})
	if err != nil {
		t.Fatalf("the current assignment's synthesis failed: %v", err)
	}
	if final.VoiceAssignmentCAS != assignB.CASHash {
		t.Fatalf("the final pass must belong to the current assignment, got %s", final.VoiceAssignmentCAS)
	}
	if len(final.ReviewSegments) != 1 || final.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("the current assignment's pass must derive its own candidate, got %+v", final.ReviewSegments)
	}
	tc := final.ReviewSegments[0].TempoCandidate
	if !tc.Selectable || tc.Reason != domain.TempoReasonFits || tc.TransformedAudioSHA256 == "" {
		t.Fatalf("expected a selectable in-window candidate, got %+v", tc)
	}
	if tc.NaturalDurationMs != movedOnRaceLaneMs {
		t.Fatalf("the candidate must come from the current pass's own waveform (%dms), got %dms", movedOnRaceLaneMs, tc.NaturalDurationMs)
	}
	if got := transformCalls(); got != 1 {
		t.Fatalf("exactly one transform belongs to the current pass, got %d", got)
	}
	if idx, err := db.GetDubSegmentsVariantIndexByRun(ctx, movedOnRaceRunID); err != nil || idx == nil || idx.CASHash != final.CASHash {
		t.Fatalf("the current assignment's pass must own the run's row, got idx=%v err=%v", idx, err)
	}
	if superseded := loadCasVariant(t, casStore, stale.CASHash); superseded.ReviewSegments[0].TempoCandidate != nil {
		t.Fatal("the stale artifact must stay readable and without a tempo candidate")
	}
}

// TestDubbingService_Issue156_CachedPreEscalationPassStillDerivesTempoEvidence proves the cached
// fast path is taken only when the pass is really final. A run whose escalation cannot be
// repeated on a later call reads the pre-escalation pass from the idempotent cache; that pass was
// published before any tempo derivation, so the eligible unresolved overrun it still carries must
// gain its candidate from the cached retained waveform and the pass must be republished - all
// without a second TTS invocation.
func TestDubbingService_Issue156_CachedPreEscalationPassStillDerivesTempoEvidence(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	ctx := context.Background()
	const (
		assetID    = "asset-escalation-cached-evidence"
		runID      = "run-escalation-cached-evidence"
		targetLang = "vi"
		slotMs     = int64(1000)
		laneMs     = int64(1200) // 1.20 of the slot: an eligible atempo overrun
	)
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: targetLang,
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          slotMs,
				SlotDurationMs: slotMs,
				SourceText:     "你好",
				MeaningText:    "Alo",
				SpokenText:     "Alo",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assign, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID:             runID,
		AssetID:           assetID,
		TargetLanguage:    targetLang,
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": provider.DefaultPresetVoices("vi")[0]},
	})
	if err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}

	fixedProvider, ok := reg.Get("fake_zerotts_tts_vi")
	if !ok {
		t.Fatal("fixed-rate lane must be registered")
	}
	fixedFake := fixedProvider.(*provider.FakeTTSProvider)
	fixedFake.DurationMs = laneMs

	// The duration-controlled fallback lane the escalation targets, registered and licensed
	// before the run so the first call plans the escalation.
	fallbackFake := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", laneMs)
	if err := reg.Register(fallbackFake); err != nil {
		t.Fatalf("register fallback lane: %v", err)
	}
	if err := governance.NewLicenseService(db).RegisterManifest(ctx, domain.LicenseManifestEntry{
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
	}); err != nil {
		t.Fatalf("register fallback lane manifest: %v", err)
	}

	// Inject a failure into the escalation's own persistence step (the superseding
	// VoiceAssignment write) so the completed pre-escalation pass is published and the call
	// surfaces an error, leaving the run on its original assignment.
	abort := "injected escalation persistence failure"
	_ = db.QueryRow(ctx, `CREATE TRIGGER issue156_cached_evidence_fail_insert BEFORE INSERT ON voice_assignments
		BEGIN SELECT RAISE(ABORT, '`+abort+`'); END`).Scan(new(any))
	_ = db.QueryRow(ctx, `CREATE TRIGGER issue156_cached_evidence_fail_update BEFORE UPDATE ON voice_assignments
		BEGIN SELECT RAISE(ABORT, '`+abort+`'); END`).Scan(new(any))

	input := domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      targetLang,
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	}
	if _, err := dubSvc.SynthesizeAndFit(ctx, input); err == nil {
		t.Fatal("expected the injected escalation persistence failure to surface")
	}
	firstIdx, err := db.GetDubSegmentsVariantIndex(ctx, assetID, targetLang)
	if err != nil || firstIdx == nil {
		t.Fatalf("the pre-escalation pass must be published: idx=%v err=%v", firstIdx, err)
	}
	// Precondition: the published pass predates the derivation, so it carries no candidate.
	firstPass := loadCasVariant(t, casStore, firstIdx.CASHash)
	if len(firstPass.ReviewSegments) != 1 || firstPass.ReviewSegments[0].TempoCandidate != nil {
		t.Fatalf("precondition: the pre-escalation pass must carry no tempo candidate, got %+v", firstPass.ReviewSegments)
	}
	invocationsAfterFirst := fixedFake.Invocations

	// The fallback lane is no longer route-eligible, so the retry cannot repeat the escalation
	// and finalizes the cached pre-escalation pass instead.
	reg.SetRequireSnapshots(true)
	dubSvc.AtempoTransform = func(callCtx context.Context, req media.AtempoRequest) ([]byte, error) {
		return media.GeneratePCM16WAV(16000, 1, slotMs), nil
	}

	retried, err := dubSvc.SynthesizeAndFit(ctx, input)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if fixedFake.Invocations != invocationsAfterFirst {
		t.Fatalf("the retry must not re-run TTS: %d -> %d invocations", invocationsAfterFirst, fixedFake.Invocations)
	}
	if retried == nil || len(retried.ReviewSegments) != 1 || retried.ReviewSegments[0].TempoCandidate == nil {
		t.Fatalf("expected the cached pass republished with tempo evidence, got %+v", retried)
	}
	tc := retried.ReviewSegments[0].TempoCandidate
	if tc.TransformedAudioSHA256 == "" || tc.Reason != domain.TempoReasonFits || !tc.Selectable {
		t.Fatalf("expected a selectable in-window candidate derived from the cached waveform, got %+v", tc)
	}
	if tc.NaturalDurationMs != laneMs {
		t.Fatalf("the candidate must be derived from the cached pass waveform (%dms), got %dms", laneMs, tc.NaturalDurationMs)
	}
	secondIdx, err := db.GetDubSegmentsVariantIndex(ctx, assetID, targetLang)
	if err != nil || secondIdx == nil {
		t.Fatalf("expected the republished final artifact: idx=%v err=%v", secondIdx, err)
	}
	if secondIdx.CASHash == firstIdx.CASHash {
		t.Fatal("the retry must republish the pass that gained tempo evidence")
	}
	if !casStore.Exists(firstIdx.CASHash) {
		t.Fatalf("the pre-escalation artifact must stay readable history: %s", firstIdx.CASHash)
	}
	published := loadCasVariant(t, casStore, secondIdx.CASHash)
	if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil ||
		published.ReviewSegments[0].TempoCandidate.TransformedAudioSHA256 != tc.TransformedAudioSHA256 {
		t.Fatalf("the published final artifact must carry the derived candidate, got %+v", published.ReviewSegments)
	}
}

// loadCasVariant reads and decodes one committed DubSegmentsVariant by its CAS hash.
func loadCasVariant(t *testing.T, casStore *cas.Store, casHash string) *domain.DubSegmentsVariant {
	t.Helper()
	rc, err := casStore.Get(casHash)
	if err != nil {
		t.Fatalf("load variant %s: %v", casHash, err)
	}
	defer rc.Close()
	var variant domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		t.Fatalf("decode variant %s: %v", casHash, err)
	}
	return &variant
}

// TestDubbingService_Issue156_ResumeRetriesOnlyTransientTempoEvidence pins the resume clause of
// the slice: a cancelled or unavailable transform is evidence about the run that produced it,
// not a permanent verdict on the pass. A resumed call with the same inputs reads the committed
// pass from the idempotent cache — so no TTS, rewrite, regroup or escalation work is re-run —
// but drops exactly the transient tempo evidence and re-derives the candidate, leaving one
// canonical final artifact on the run's index. A third call, whose candidate is the transform's
// own final answer, is returned untouched.
func TestDubbingService_Issue156_ResumeRetriesOnlyTransientTempoEvidence(t *testing.T) {
	transient := []struct {
		name       string
		transform  func(ctx context.Context) ([]byte, error)
		wantReason string
		wantErr    error
	}{
		{
			name: "cancelled",
			transform: func(ctx context.Context) ([]byte, error) {
				return nil, context.Canceled // the transform's own cancellation surfaces as evidence
			},
			wantReason: domain.TempoReasonCancelled,
			wantErr:    context.Canceled,
		},
		{
			name: "tool unavailable",
			transform: func(ctx context.Context) ([]byte, error) {
				return nil, fmt.Errorf("injected: %w", media.ErrAtempoUnavailable)
			},
			wantReason: domain.TempoReasonToolUnavailable,
			wantErr:    nil,
		},
	}

	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
			defer db.Close()

			assetID := "asset-tempo-resume-" + strings.ReplaceAll(tc.name, " ", "-")
			runID := "run-tempo-resume-" + strings.ReplaceAll(tc.name, " ", "-")
			const targetLang = "vi"

			setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, targetLang)
			scriptCAS, assignCAS, transcriptCAS := seedTempoReviewFixture(t, db, casStore, assetID, runID, targetLang, 1000, "Alo")

			input := domain.DubbingJobInput{
				RunID:                 runID,
				AssetID:               assetID,
				TargetLanguage:        targetLang,
				DubScriptVariantCAS:   scriptCAS,
				VoiceAssignmentCAS:    assignCAS,
				TranscriptArtifactCAS: transcriptCAS,
			}

			ttsCalls := 0
			dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
				ttsCalls++
				return &provider.TTSSynthesisResult{
					AudioData:           media.GeneratePCM16WAV(16000, 1, 1200),
					AudioSHA256:         "sha256-natural-1200",
					PredictedDurationMs: 1200,
				}, nil
			}

			// Call 1: the transform fails transiently, so the pass publishes the evidence and
			// (for cancellation) run control stops the run with the published pass in hand.
			firstCtx, cancelFirst := context.WithCancel(context.Background())
			defer cancelFirst()
			dubSvc.AtempoTransform = func(callCtx context.Context, req media.AtempoRequest) ([]byte, error) {
				if tc.wantReason == domain.TempoReasonCancelled {
					cancelFirst()
				}
				return tc.transform(callCtx)
			}
			first, err := dubSvc.SynthesizeAndFit(firstCtx, input)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("first call error = %v, want %v", err, tc.wantErr)
			}
			if first == nil || len(first.ReviewSegments) != 1 || first.ReviewSegments[0].TempoCandidate == nil {
				t.Fatalf("expected the transient evidence published with the pass, got %+v", first)
			}
			firstCandidate := first.ReviewSegments[0].TempoCandidate
			if firstCandidate.Reason != tc.wantReason {
				t.Fatalf("first candidate reason = %s, want %s", firstCandidate.Reason, tc.wantReason)
			}
			if firstCandidate.TransformedAudioSHA256 != "" {
				t.Fatalf("a transient transform failure must commit no artifact, got %s", firstCandidate.TransformedAudioSHA256)
			}
			firstIdx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
			if err != nil || firstIdx == nil {
				t.Fatalf("expected the evidence-bearing pass published, got idx=%v err=%v", firstIdx, err)
			}
			if ttsCalls != 1 {
				t.Fatalf("first call must synthesize once, got %d TTS calls", ttsCalls)
			}

			// Call 2: fresh context, the transform now succeeds. The cached pass is reused -
			// no second TTS - and only the transient candidate is re-derived.
			dubSvc.AtempoTransform = func(callCtx context.Context, req media.AtempoRequest) ([]byte, error) {
				return media.GeneratePCM16WAV(16000, 1, 1000), nil
			}
			second, err := dubSvc.SynthesizeAndFit(context.Background(), input)
			if err != nil {
				t.Fatalf("resumed call failed: %v", err)
			}
			if ttsCalls != 1 {
				t.Fatalf("the resumed call must reuse the committed synthesis work, got %d TTS calls", ttsCalls)
			}
			if len(second.ReviewSegments) != 1 || second.ReviewSegments[0].TempoCandidate == nil {
				t.Fatalf("expected the re-derived candidate, got %+v", second.ReviewSegments)
			}
			retried := second.ReviewSegments[0].TempoCandidate
			if retried.Reason != domain.TempoReasonFits || !retried.Selectable {
				t.Fatalf("expected a selectable transform, got reason=%s selectable=%v", retried.Reason, retried.Selectable)
			}
			if retried.TransformedAudioSHA256 == "" || retried.TransformedAudioSHA256 == retried.NaturalAudioSHA256 {
				t.Fatalf("expected a distinct transformed artifact, got %q", retried.TransformedAudioSHA256)
			}
			if !casStore.Exists(retried.TransformedAudioSHA256) {
				t.Fatalf("transformed artifact missing from CAS: %s", retried.TransformedAudioSHA256)
			}

			// The run index moved to exactly one canonical final artifact carrying the retried
			// candidate; the transient-evidence artifact stays as content-addressed history.
			secondIdx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
			if err != nil || secondIdx == nil {
				t.Fatalf("expected a published variant after the retry, got idx=%v err=%v", secondIdx, err)
			}
			if secondIdx.CASHash == firstIdx.CASHash {
				t.Fatalf("the retry must republish the final artifact, index still at %s", secondIdx.CASHash)
			}
			if !casStore.Exists(firstIdx.CASHash) {
				t.Fatalf("the transient-evidence artifact must remain readable history: %s", firstIdx.CASHash)
			}
			if second.CASHash != secondIdx.CASHash {
				t.Fatalf("returned variant hash %s != published index hash %s", second.CASHash, secondIdx.CASHash)
			}
			publishedRC, err := casStore.Get(secondIdx.CASHash)
			if err != nil {
				t.Fatalf("load published variant: %v", err)
			}
			defer publishedRC.Close()
			var published domain.DubSegmentsVariant
			if err := json.NewDecoder(publishedRC).Decode(&published); err != nil {
				t.Fatalf("decode published variant: %v", err)
			}
			if len(published.ReviewSegments) != 1 || published.ReviewSegments[0].TempoCandidate == nil {
				t.Fatal("the published final artifact must carry the re-derived candidate")
			}
			if got := published.ReviewSegments[0].TempoCandidate.TransformedAudioSHA256; got != retried.TransformedAudioSHA256 {
				t.Fatalf("published transformed sha %s != returned %s", got, retried.TransformedAudioSHA256)
			}

			// Call 3: the candidate is now the transform's own final answer, so the committed
			// pass is returned untouched - no re-derivation, no republish.
			third, err := dubSvc.SynthesizeAndFit(context.Background(), input)
			if err != nil {
				t.Fatalf("idempotent call failed: %v", err)
			}
			if ttsCalls != 1 {
				t.Fatalf("an idempotent call must not re-synthesize, got %d TTS calls", ttsCalls)
			}
			if third.CASHash != secondIdx.CASHash {
				t.Fatalf("a final candidate must not be re-derived: hash %s != %s", third.CASHash, secondIdx.CASHash)
			}
			thirdIdx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, targetLang)
			if err != nil || thirdIdx == nil || thirdIdx.CASHash != secondIdx.CASHash {
				t.Fatalf("the run index must stay on the final artifact, got %+v err=%v", thirdIdx, err)
			}
		})
	}
}
