package seam1_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

func testBenchmarkIdentity() benchmark.SessionIdentityInput {
	return benchmark.SessionIdentityInput{
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition_100_urls", Digest: "62ce8541701236f69fb31f39ee724473f09a42b424ea1fef73f323af46d58f2c", EntriesCount: 100},
			{Name: "quality_24_videos", Digest: "6bc654807f8868b2f0a03504975abfa4fe748f30426344eb959c33f6fcd51b54", EntriesCount: 24},
		},
		ExecutionProfile: "hybrid",
		BuildIdentity:    "7bbac44969442444934338867db4abcb29a4d24b",
		ConfigSnapshot: map[string]any{
			"zero_overrun_strict": true,
			"profile":             "hybrid",
			"max_retries":         1,
		},
		ProviderBaselines: []benchmark.ProviderBaseline{
			{
				Role:              "translation_primary",
				ProviderID:        provider.GatewayGeminiTranslationProviderID,
				ModelVersion:      provider.GatewayGeminiModelAlias,
				ServiceBaselineID: "seam1-test-gemini-baseline",
			},
			{
				Role:              "translation_fallback",
				ProviderID:        provider.GatewayDeepSeekTranslationProviderID,
				ModelVersion:      provider.GatewayDeepSeekModelAlias,
				ServiceBaselineID: "seam1-test-deepseek-baseline",
			},
			{
				Role:                   "tts_vi",
				ProviderID:             "vieneu_tts_vi",
				ModelVersion:           "v3.2.9",
				SnapshotManifestDigest: "1278db0090b98ccf23e56f2423857fc9d32a5118",
			},
		},
		Environment: benchmark.EnvironmentAttestation{
			OS:                   "windows",
			Arch:                 "amd64",
			GPUModel:             "NVIDIA GeForce RTX 2060 SUPER",
			TotalVRAMBytes:       8589934592,
			DriverVersion:        "560.94",
			CUDAOrRuntimeVersion: "12.6",
		},
		Operator: benchmark.OperatorMetadata{
			OperatorID:     "test-operator",
			SessionPurpose: "Seam 1 Benchmark Runner Integration Gate",
		},
	}
}

// TestSeam1_BenchmarkRunner_ResumableSession verifies that:
// 1. BenchmarkRunner operates as an external client of public Seam 1 endpoints only.
// 2. Native LocalizationJob and LocalizationRun UUIDs are preserved in the session envelope.
// 3. Acquisition and Quality Case executions are recorded with CAS hashes, attempt provenance, and relational QC.
// 4. Interruption and resumption with identical session identity safely reuses completed evidence.
// 5. Resumption with a mismatched identity strictly fails closed with ErrIdentityMismatch.
func TestSeam1_BenchmarkRunner_ResumableSession(t *testing.T) {
	h := setupHarness(t)
	defaultVITTSFake(t, h).DurationMs = 200
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storeDir := filepath.Join(h.dir, "benchmark_sessions")
	store, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("create benchmark store: %v", err)
	}

	identity := testBenchmarkIdentity()
	session, err := store.Resume(identity)
	if err != nil {
		t.Fatalf("initial session resume: %v", err)
	}

	client := benchmark.NewRuntimeHostClient(h.server.URL, nil)
	runner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   store,
		Session: session,
	})
	if err != nil {
		t.Fatalf("create benchmark runner: %v", err)
	}

	// 1. Execute Acquisition Item
	acqMediaPath := createSyntheticMedia(t, h.dir, "acq_test_01.mp4")
	acqInput := benchmark.AcquisitionInput{
		EntryID:               "aweme_7001",
		CanonicalURL:          "https://www.douyin.com/video/7001",
		ExpectedAwemeID:       "7001",
		ExpectedDurationMs:    1000,
		DurationToleranceMs:   10000,
		MediaFilePath:         acqMediaPath,
		AttestationDeclaredBy: "seam1-tester",
	}

	acqEvidence, err := runner.ExecuteAcquisition(ctx, acqInput)
	if err != nil {
		t.Fatalf("execute acquisition failed: %v", err)
	}
	// Invariant (Issue #70 Finding 2): Local substitution finishes with terminal FAIL on evidence
	if acqEvidence.Status != "FAIL" {
		t.Fatalf("expected acquisition local substitution to be terminal FAIL, got %s", acqEvidence.Status)
	}
	if !acqEvidence.IsLocalSubstitution {
		t.Fatalf("expected IsLocalSubstitution to be true")
	}
	if acqEvidence.ErrorMessage != "local-file substitution is prohibited for scored acquisition benchmark" {
		t.Fatalf("unexpected error message: %s", acqEvidence.ErrorMessage)
	}
	if acqEvidence.SourceAssetID == "" || acqEvidence.SourceAssetCASHash == "" {
		t.Errorf("expected non-empty asset ID and CAS hash: ID=%s CAS=%s", acqEvidence.SourceAssetID, acqEvidence.SourceAssetCASHash)
	}
	if !acqEvidence.IntegrityPassed {
		t.Errorf("expected integrity passed on acquisition evidence")
	}
	// Invariant: Re-executing already completed acquisition immediately reuses evidence
	reusedAcq, err := runner.ExecuteAcquisition(ctx, acqInput)
	if err != nil {
		t.Fatalf("re-execute acquisition: %v", err)
	}
	if reusedAcq.SourceAssetID != acqEvidence.SourceAssetID {
		t.Errorf("expected reused acquisition asset ID %s, got %s", acqEvidence.SourceAssetID, reusedAcq.SourceAssetID)
	}
	if reusedAcq.Status != "FAIL" {
		t.Errorf("expected reused acquisition to preserve terminal FAIL, got %s", reusedAcq.Status)
	}
	// 2. Execute Quality Case
	qcMediaPath := createSyntheticMedia(t, h.dir, "qc_test_01.mp4")
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      3000,
		},
		{
			Index:      1,
			SourceText: "明天也会很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    3500,
			EndMs:      6500,
		},
	}
	audioRoleSegments := []domain.AudioSegment{
		{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		{StartMs: 3500, EndMs: 6500, Role: domain.AudioRoleNarrationDialogue},
	}

	qcInput := benchmark.QualityCaseInput{
		CaseID:            "video_01_vi",
		SourceVideoID:     "video_01",
		PrimaryCategory:   "lifestyle_narration",
		TargetLanguage:    "vi",
		Profile:           "hybrid",
		MediaFilePath:     qcMediaPath,
		Segments:          segments,
		AudioRoleSegments: audioRoleSegments,
	}

	qcEvidence, err := runner.ExecuteQualityCase(ctx, qcInput)
	if err != nil {
		t.Fatalf("execute quality case failed: %v", err)
	}

	// Invariant: Native LocalizationJob and LocalizationRun UUIDs are preserved
	if qcEvidence.JobID == "" {
		t.Errorf("expected native JobID on quality case evidence")
	}
	if qcEvidence.RunID == "" {
		t.Errorf("expected native RunID on quality case evidence")
	}
	if qcEvidence.Status != "COMPLETED" {
		t.Errorf("expected quality case status COMPLETED, got %s", qcEvidence.Status)
	}

	// Invariant (#65): the native LocalizationRun must carry the same semantic
	// config snapshot that is frozen into the benchmark-session identity. A
	// benchmark-labeled profile with an empty product RunConfigSnapshot is not
	// acceptable release evidence.
	persistedRun, err := client.GetRun(ctx, qcEvidence.RunID)
	if err != nil {
		t.Fatalf("get persisted localization run: %v", err)
	}
	wantRunConfig, err := benchmark.CanonicalizeConfig(identity.ConfigSnapshot)
	if err != nil {
		t.Fatalf("canonicalize expected run config: %v", err)
	}
	if persistedRun.ConfigSnapshotJSON != wantRunConfig {
		t.Fatalf("run config snapshot = %s, want session-bound %s", persistedRun.ConfigSnapshotJSON, wantRunConfig)
	}

	// Invariant: All required pipeline stages executed and emitted CAS artifact hashes
	expectedStages := []string{
		"speech_understand",
		"translate",
		"dub_script",
		"voice_assignment",
		"dub_synthesize",
		"separate_stems",
		"audio_mix",
		"detect_text",
		"visual_track",
		"render_plan",
		"render_final",
	}
	for _, stage := range expectedStages {
		st, ok := qcEvidence.Stages[stage]
		if !ok {
			t.Errorf("missing stage %s in case evidence", stage)
			continue
		}
		if st.Status != "COMPLETED" {
			t.Errorf("stage %s status %s != COMPLETED", stage, st.Status)
		}
		if st.OutputCASHash == "" {
			t.Errorf("stage %s missing OutputCASHash", stage)
		}
	}

	// Invariant: Telemetry is not fabricated when no collector is configured (#65 / #72 boundary)
	if len(qcEvidence.Telemetry) != 0 {
		t.Errorf("expected 0 fabricated telemetry samples without collector, got %d", len(qcEvidence.Telemetry))
	}

	// Invariant: Hardened sidecar boundary rejects path traversal and invalid IDs
	if _, err := store.Load("../traversal_attempt"); !errors.Is(err, benchmark.ErrInvalidSessionID) {
		t.Errorf("expected ErrInvalidSessionID on path traversal load, got %v", err)
	}

	// 3. Safe Resume Test: Simulate process restart and resume with identical identity
	storeRestart, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}

	resumedSession, err := storeRestart.Resume(identity)
	if err != nil {
		t.Fatalf("resume session: %v", err)
	}
	if resumedSession.ID != session.ID {
		t.Errorf("expected same session ID on resume: %s vs %s", session.ID, resumedSession.ID)
	}

	// Verify all prior evidence is intact
	if !resumedSession.IsAcquisitionComplete("aweme_7001") {
		t.Errorf("expected aweme_7001 completed in resumed session")
	}
	if !resumedSession.IsQualityCaseComplete("video_01_vi") {
		t.Errorf("expected video_01_vi completed in resumed session")
	}

	resumedRunner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   storeRestart,
		Session: resumedSession,
	})
	if err != nil {
		t.Fatalf("create resumed runner: %v", err)
	}

	// Re-run the completed case: must immediately return without duplicate stage execution
	qcReused, err := resumedRunner.ExecuteQualityCase(ctx, qcInput)
	if err != nil {
		t.Fatalf("re-run quality case on resumed session: %v", err)
	}
	if qcReused.JobID != qcEvidence.JobID || qcReused.RunID != qcEvidence.RunID {
		t.Errorf("resumed execution changed native IDs: job %s!=%s, run %s!=%s",
			qcReused.JobID, qcEvidence.JobID, qcReused.RunID, qcEvidence.RunID)
	}

	// 4. Mismatch Rejection Test: Verify that different identity refuses evidence mixing
	mismatchedIdentity := testBenchmarkIdentity()
	mismatchedIdentity.BuildIdentity = "different_commit_deadbeef12345678"

	// Attempting to resume with a mismatched identity against the existing session must fail closed
	_, err = storeRestart.ResumeByID(session.ID, mismatchedIdentity)
	if err == nil {
		t.Fatalf("expected ErrIdentityMismatch on ResumeByID with different build identity")
	}
	var mismatchErr *benchmark.ErrIdentityMismatch
	if !errors.As(err, &mismatchErr) {
		t.Fatalf("expected *benchmark.ErrIdentityMismatch, got %T (%v)", err, err)
	}
	if len(mismatchErr.Differences) == 0 {
		t.Errorf("expected differences enumerated in mismatch error")
	}

	// Calling store.Resume with mismatchedIdentity must create a new distinct session ID
	newSession, err := storeRestart.Resume(mismatchedIdentity)
	if err != nil {
		t.Fatalf("resume with new identity: %v", err)
	}
	if newSession.ID == session.ID {
		t.Errorf("expected distinct session ID for different identity, got %s", newSession.ID)
	}
}

func TestSeam1_BenchmarkRunner_RejectsQualityProfileMismatch(t *testing.T) {
	h := setupHarness(t)
	store, err := benchmark.NewFileStore(filepath.Join(h.dir, "profile_mismatch_sessions"))
	if err != nil {
		t.Fatalf("create benchmark store: %v", err)
	}
	identity := testBenchmarkIdentity()
	identity.ExecutionProfile = "local"
	identity.ConfigSnapshot = map[string]any{"profile": "local", "zero_overrun_strict": true}
	session, err := store.Resume(identity)
	if err != nil {
		t.Fatalf("resume local session: %v", err)
	}
	runner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client()),
		Store:   store,
		Session: session,
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	_, err = runner.ExecuteQualityCase(context.Background(), benchmark.QualityCaseInput{
		CaseID:          "hybrid_case_in_local_session",
		SourceVideoID:   "video_profile_mismatch",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
	})
	if err == nil || !strings.Contains(err.Error(), "profile mismatch") {
		t.Fatalf("expected profile mismatch rejection before product execution, got %v", err)
	}
	if len(session.QualityCases) != 0 {
		t.Fatalf("profile mismatch must not create benchmark evidence, got %d case(s)", len(session.QualityCases))
	}
}
