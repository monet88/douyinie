package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #156: Bundle traversal and closure verification for review-only tempo candidates.

func TestBundleService_Issue156_TempoCandidateClosureAndTraversal(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, assetID := seedTestJob(t, dbA, casA, licA)

	// Create a run
	runID := uuid.NewString()
	now := time.Now().UTC()
	run := domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{"profile": "hybrid"}`,
		CreatedAt:          now,
	}
	if err := dbA.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Put natural audio and transformed audio in CAS
	natWAV := media.GeneratePCM16WAV(16000, 1, 1200)
	natObj, err := casA.Put(bytes.NewReader(natWAV))
	if err != nil {
		t.Fatalf("put natural audio: %v", err)
	}

	transWAV := media.GeneratePCM16WAV(16000, 1, 1000)
	transObj, err := casA.Put(bytes.NewReader(transWAV))
	if err != nil {
		t.Fatalf("put transformed audio: %v", err)
	}

	tempoCandidate := &domain.DubTempoCandidate{
		NaturalAudioSHA256:     natObj.SHA256,
		TransformedAudioSHA256: transObj.SHA256,
		Factor:                 1.20,
		NaturalDurationMs:      1200,
		TransformedDurationMs:  1000,
		PlaybackDurationMs:     1000,
		Selectable:             true,
		Reason:                 domain.TempoReasonFits,
		ToolID:                 domain.TempoToolID,
		Filter:                 "atempo=1.200000",
		PolicyVersion:          "playback-window-v2",
	}

	dsv := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		FitPolicyID:    "playback-window-v2",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              1000,
				SlotDurationMs:     1000,
				SourceText:         "测试",
				SpokenText:         "Thử nghiệm",
				AudioCASPath:       natObj.Path,
				AudioSHA256:        natObj.SHA256,
				MeasuredDurationMs: 1200,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       1,
				DubPlaybackEndMs:   1000,
				TempoCandidate:     tempoCandidate,
			},
		},
		CreatedAt: now,
	}

	dsvBytes, _ := json.Marshal(dsv)
	dsvObj, err := casA.Put(bytes.NewReader(dsvBytes))
	if err != nil {
		t.Fatalf("put dsv: %v", err)
	}
	dsv.CASHash = dsvObj.SHA256
	if err := dbA.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dsv.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        dsvObj.SHA256,
		ProvenanceHash: "prov-tempo-dsv",
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("save dsv index: %v", err)
	}

	// 1. Export bundle
	var zipBuf bytes.Buffer
	manifest, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("ExportBundle failed: %v", err)
	}

	// Verify that manifest contains both natural audio and transformed audio in artifacts
	hasNat := false
	hasTrans := false
	for _, art := range manifest.Artifacts {
		if strings.EqualFold(art.SHA256, natObj.SHA256) {
			hasNat = true
		}
		if strings.EqualFold(art.SHA256, transObj.SHA256) {
			hasTrans = true
		}
	}
	if !hasNat {
		t.Fatalf("expected natural audio %s in exported artifacts", natObj.SHA256)
	}
	if !hasTrans {
		t.Fatalf("expected transformed audio %s in exported artifacts", transObj.SHA256)
	}

	// Verify manifest json has scrubbed machine-local path for tempo candidate
	manifestJSON, _ := json.Marshal(manifest)
	if err := service.ValidateManifestJSON(manifestJSON); err != nil {
		t.Fatalf("exported manifest contains machine-local path: %v", err)
	}

	// 2. Import into fresh environment B
	dbB, casB, _, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()

	zipBytes := zipBuf.Bytes()
	importedManifest, err := bundleSvcB.ImportBundle(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), service.ImportOptions{})
	if err != nil {
		t.Fatalf("ImportBundle failed: %v", err)
	}
	if importedManifest == nil {
		t.Fatal("expected non-nil imported manifest")
	}

	// Verify both artifacts exist in destination CAS
	if !casB.Exists(natObj.SHA256) {
		t.Fatalf("natural audio missing in dest CAS: %s", natObj.SHA256)
	}
	if !casB.Exists(transObj.SHA256) {
		t.Fatalf("transformed audio missing in dest CAS: %s", transObj.SHA256)
	}
}

func TestBundleService_Issue156_MissingNaturalParentFailsClosure(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, assetID := seedTestJob(t, dbA, casA, licA)

	runID := uuid.NewString()
	now := time.Now().UTC()
	run := domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{"profile": "hybrid"}`,
		CreatedAt:          now,
	}
	_ = dbA.CreateRun(ctx, run)

	// Declare a natural parent hash that does NOT exist in CAS
	missingNaturalSHA := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	transWAV := media.GeneratePCM16WAV(16000, 1, 1000)
	transObj, _ := casA.Put(bytes.NewReader(transWAV))

	dsv := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		FitPolicyID:    "playback-window-v2",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              1000,
				SlotDurationMs:     1000,
				AudioSHA256:        missingNaturalSHA, // missing natural parent!
				MeasuredDurationMs: 1200,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       1,
				DubPlaybackEndMs:   1000,
				TempoCandidate: &domain.DubTempoCandidate{
					NaturalAudioSHA256:     missingNaturalSHA,
					TransformedAudioSHA256: transObj.SHA256,
					Factor:                 1.20,
					Selectable:             true,
					Reason:                 domain.TempoReasonFits,
					ToolID:                 domain.TempoToolID,
				},
			},
		},
		CreatedAt: now,
	}

	dsvBytes, _ := json.Marshal(dsv)
	dsvObj, _ := casA.Put(bytes.NewReader(dsvBytes))
	dsv.CASHash = dsvObj.SHA256
	_ = dbA.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dsv.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        dsvObj.SHA256,
		ProvenanceHash: "prov-missing-parent",
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      now,
	})

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err == nil || !errors.Is(err, domain.ErrJobBundleArtifactMissing) {
		t.Fatalf("expected ErrJobBundleArtifactMissing for missing natural parent, got: %v", err)
	}
}

// TestBundleService_Issue156_ProviderAttemptDigestIsNotClosureRequired proves that excluding
// ProviderAttempt.InputHash from reachable CAS closure is necessary for #156 bundle-closure acceptance,
// not unrelated cleanup: ProviderAttempt.InputHash is a request/provenance digest (TTS text+voice+speed,
// translation input, discovery query), not a CAS artifact. When treated as a required CAS object,
// ExportBundle fails closed on real runs whose attempts carry valid digests that never existed as CAS files.
// This test pins that real runs export, import, and retain the digest in manifest provenance.
func TestBundleService_Issue156_ProviderAttemptDigestIsNotClosureRequired(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	runID := uuid.NewString()
	now := time.Now().UTC()
	if err := dbA.CreateRun(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{}`,
		CreatedAt:          now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("tts|SPEAKER_00|voice_1|1.0|Thử nghiệm")))
	if err := dbA.RecordProviderAttempt(ctx, domain.ProviderAttempt{
		ID:            uuid.NewString(),
		RunID:         runID,
		Stage:         "dub_synthesize",
		ProviderID:    "fake_zerotts_tts_vi",
		ModelName:     "zerotts",
		ModelVersion:  "1.0.0",
		InputHash:     digest,
		AttemptNumber: 1,
		Status:        "succeeded",
		CreatedAt:     now,
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if casA.Exists(digest) {
		t.Fatalf("precondition failed: digest %s must NOT exist in CAS", digest)
	}

	var zipBuf bytes.Buffer
	manifest, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("ExportBundle must not require an attempt input digest to exist in CAS: %v", err)
	}

	for _, art := range manifest.Artifacts {
		if art.SHA256 == digest {
			t.Fatalf("attempt input digest %s must not be shipped as a bundle artifact", digest)
		}
	}

	foundAttempt := false
	for _, run := range manifest.Runs {
		for _, att := range run.ProviderAttempts {
			if att.InputHash == digest {
				foundAttempt = true
			}
		}
	}
	if !foundAttempt {
		t.Fatalf("attempt digest %s missing from exported manifest provenance", digest)
	}

	dbB, _, _, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()
	zipBytes := zipBuf.Bytes()
	res, err := bundleSvcB.ImportBundle(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), service.ImportOptions{})
	if err != nil {
		t.Fatalf("ImportBundle of a digest-carrying run failed: %v", err)
	}
	if res == nil || res.Job.ID != jobID {
		t.Fatalf("expected imported job ID %s, got %+v", jobID, res)
	}
	importedAttempts, err := dbB.ListProviderAttempts(ctx, runID, "dub_synthesize")
	if err != nil {
		t.Fatalf("list imported provider attempts: %v", err)
	}
	if len(importedAttempts) != 1 || importedAttempts[0].InputHash != digest {
		t.Fatalf("expected 1 imported attempt with digest %s, got %+v", digest, importedAttempts)
	}
}

// TestBundleService_Issue156_DistinctNaturalParentIsShipped proves the closure ships every
// waveform the review candidate references: the retained natural waveform, the candidate's own
// natural parent (distinct when the retained waveform came from a rewrite or regroup), and the
// transformed alternative.
func TestBundleService_Issue156_DistinctNaturalParentIsShipped(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, assetID := seedTestJob(t, dbA, casA, licA)

	runID := uuid.NewString()
	now := time.Now().UTC()
	if err := dbA.CreateRun(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{}`,
		CreatedAt:          now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	retainedObj, _ := casA.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1200)))
	parentObj, _ := casA.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1150)))
	transObj, _ := casA.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1000)))

	dsv := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		FitPolicyID:    "playback-window-v2",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              1000,
				SlotDurationMs:     1000,
				AudioSHA256:        retainedObj.SHA256,
				MeasuredDurationMs: 1200,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       1,
				DubPlaybackEndMs:   1000,
				TempoCandidate: &domain.DubTempoCandidate{
					NaturalAudioSHA256:     parentObj.SHA256,
					TransformedAudioSHA256: transObj.SHA256,
					Factor:                 1.15,
					NaturalDurationMs:      1150,
					TransformedDurationMs:  1000,
					PlaybackDurationMs:     1000,
					Selectable:             true,
					Reason:                 domain.TempoReasonFits,
					ToolID:                 domain.TempoToolID,
					Filter:                 "atempo=1.150000",
					PolicyVersion:          "playback-window-v2",
				},
			},
		},
		CreatedAt: now,
	}

	dsvBytes, _ := json.Marshal(dsv)
	dsvObj, err := casA.Put(bytes.NewReader(dsvBytes))
	if err != nil {
		t.Fatalf("put dsv: %v", err)
	}
	if err := dbA.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dsv.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        dsvObj.SHA256,
		ProvenanceHash: "prov-distinct-parent",
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("save dsv index: %v", err)
	}

	var zipBuf bytes.Buffer
	manifest, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("ExportBundle failed: %v", err)
	}

	required := map[string]string{
		retainedObj.SHA256: "retained natural waveform",
		parentObj.SHA256:   "tempo candidate natural parent",
		transObj.SHA256:    "transformed alternative",
	}
	shipped := map[string]bool{}
	for _, art := range manifest.Artifacts {
		shipped[art.SHA256] = true
	}
	for hash, label := range required {
		if !shipped[hash] {
			t.Fatalf("expected %s %s in exported artifacts", label, hash)
		}
	}
}

// TestBundleService_Issue156_MissingTransformedAudioFailsClosure proves that a tempo candidate
// referencing an absent TransformedAudioSHA256 object fails closed with ErrJobBundleArtifactMissing.
func TestBundleService_Issue156_MissingTransformedAudioFailsClosure(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, assetID := seedTestJob(t, dbA, casA, licA)

	runID := uuid.NewString()
	now := time.Now().UTC()
	_ = dbA.CreateRun(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{}`,
		CreatedAt:          now,
	})

	natWAV := media.GeneratePCM16WAV(16000, 1, 1200)
	natObj, _ := casA.Put(bytes.NewReader(natWAV))

	missingTransSHA := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	dsv := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		FitPolicyID:    "playback-window-v2",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              1000,
				SlotDurationMs:     1000,
				AudioSHA256:        natObj.SHA256,
				MeasuredDurationMs: 1200,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				DubPlaybackEndMs:   1000,
				TempoCandidate: &domain.DubTempoCandidate{
					NaturalAudioSHA256:     natObj.SHA256,
					TransformedAudioSHA256: missingTransSHA, // missing transformed audio!
					Factor:                 1.20,
					Selectable:             true,
					Reason:                 domain.TempoReasonFits,
					ToolID:                 domain.TempoToolID,
				},
			},
		},
		CreatedAt: now,
	}

	dsvBytes, _ := json.Marshal(dsv)
	dsvObj, _ := casA.Put(bytes.NewReader(dsvBytes))
	_ = dbA.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dsv.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        dsvObj.SHA256,
		ProvenanceHash: "prov-missing-transformed",
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      now,
	})

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err == nil || !errors.Is(err, domain.ErrJobBundleArtifactMissing) {
		t.Fatalf("expected ErrJobBundleArtifactMissing for missing transformed audio, got: %v", err)
	}
}

// TestBundleService_Issue156_MissingStageExecutionArtifactFailsClosure proves that an actual
// stage artifact (e.g. dub_synthesize) absent from CAS still fails closed with ErrJobBundleArtifactMissing.
func TestBundleService_Issue156_MissingStageExecutionArtifactFailsClosure(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	runID := uuid.NewString()
	now := time.Now().UTC()
	_ = dbA.CreateRun(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{}`,
		CreatedAt:          now,
	})

	missingStageArtifactSHA := "9999999999999999999999999999999999999999999999999999999999999999"
	_ = dbA.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "dub_synthesize",
		Status:         "succeeded",
		ArtifactSHA256: missingStageArtifactSHA, // missing stage execution artifact!
		CreatedAt:      now,
	})

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err == nil || !errors.Is(err, domain.ErrJobBundleArtifactMissing) {
		t.Fatalf("expected ErrJobBundleArtifactMissing for missing stage execution artifact, got: %v", err)
	}
}
