package benchmark_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
)

func TestSession_LifecycleAndEvidenceRecording(t *testing.T) {
	input := sampleIdentityInput()
	sess, err := benchmark.NewSession(input)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if sess.Status != benchmark.SessionStatusInitialized {
		t.Errorf("expected INITIALIZED status, got %s", sess.Status)
	}

	// 1. Record acquisition evidence
	acqEntry := benchmark.AcquisitionEntryEvidence{
		EntryID:             "aweme_1001",
		CanonicalURL:        "https://www.douyin.com/video/1001",
		ExpectedAwemeID:     "1001",
		ObservedAwemeID:     "1001",
		SourceAssetID:       "asset-uuid-1001",
		SourceAssetCASHash:  "cas-sha256-1001",
		IntegrityPassed:     true,
		ExpectedDurationMs:  15000,
		ObservedDurationMs:  15050,
		DurationToleranceMs: 200,
		HTTPStatusCode:      201,
		Status:              "PASS",
		Timestamp:           time.Now().UTC(),
		Attempts: []benchmark.ProviderAttemptRef{
			{
				AttemptID:     "att-1",
				ProviderID:    "jiji_downloader",
				AttemptNumber: 1,
				Outcome:       "succeeded",
				LatencyMs:     1250,
			},
		},
	}
	sess.RecordAcquisition(acqEntry)

	if !sess.IsAcquisitionComplete("aweme_1001") {
		t.Errorf("expected aweme_1001 to be complete")
	}
	gotAcq, ok := sess.GetAcquisition("aweme_1001")
	if !ok || gotAcq == nil {
		t.Fatalf("expected to get acquisition entry")
	}
	if gotAcq.SourceAssetID != "asset-uuid-1001" {
		t.Errorf("expected asset ID asset-uuid-1001, got %s", gotAcq.SourceAssetID)
	}
	if len(gotAcq.Attempts) != 1 || gotAcq.Attempts[0].LatencyMs != 1250 {
		t.Errorf("expected 1 attempt with 1250ms latency, got %+v", gotAcq.Attempts)
	}

	// 2. Record quality case evidence
	caseID := "video_01_vi"
	qc := benchmark.QualityCaseEvidence{
		CaseID:             caseID,
		SourceVideoID:      "video_01",
		PrimaryCategory:    "lifestyle_narration",
		TargetLanguage:     "vi",
		Profile:            "local",
		JobID:              "native-job-uuid-1234",
		RunID:              "native-run-uuid-5678",
		SourceAssetID:      "asset-uuid-video-01",
		SourceAssetCASHash: "cas-sha256-video-01",
		Stages:             make(map[string]benchmark.StageExecutionEvidence),
		Status:             "IN_PROGRESS",
		CreatedAt:          time.Now().UTC(),
	}
	sess.RecordQualityCase(qc)

	// Update stage evidence
	speechStage := benchmark.StageExecutionEvidence{
		Stage:         "speech_understand",
		Status:        "COMPLETED",
		ProviderID:    "qwen3_asr",
		ModelVersion:  "1.7b",
		OutputCASHash: "cas-transcript-hash-999",
		StartedAt:     time.Now().UTC(),
		CompletedAt:   time.Now().UTC(),
		DurationMs:    2300,
	}
	sess.UpdateQualityStage(caseID, speechStage)

	// Record relational QC evidence
	relationalQC := benchmark.RelationalQCEvidence{
		QualityResultIDs: []string{"qr-uuid-001"},
		QualityResults: []domain.QualityResult{
			{
				ID:            "qr-uuid-001",
				RunID:         "native-run-uuid-5678",
				OverallStatus: domain.QualityStatusPass,
			},
		},
		ReviewItemIDs: []string{"ri-uuid-002"},
		ReviewItems: []domain.ReviewItem{
			{
				ID:     "ri-uuid-002",
				RunID:  "native-run-uuid-5678",
				Status: domain.ReviewItemStatusAutoPass,
			},
		},
	}
	sess.RecordRelationalQC(caseID, relationalQC)

	// Record telemetry
	telemetry := benchmark.ResourceTelemetrySample{
		Stage:                        "speech_understand",
		Timestamp:                    time.Now().UTC(),
		DeviceBaselineVRAMBytes:      2147483648,
		DevicePeakVRAMBytes:          4294967296,
		DevicePeakAboveBaselineBytes: 2147483648,
		ElapsedMs:                    2300,
		RTF:                          0.15,
	}
	sess.RecordTelemetry(caseID, telemetry)

	gotQC, ok := sess.GetQualityCase(caseID)
	if !ok || gotQC == nil {
		t.Fatalf("expected to get quality case")
	}
	if gotQC.JobID != "native-job-uuid-1234" || gotQC.RunID != "native-run-uuid-5678" {
		t.Errorf("native job/run UUIDs not preserved: job=%s, run=%s", gotQC.JobID, gotQC.RunID)
	}
	if stage, ok := gotQC.Stages["speech_understand"]; !ok || stage.OutputCASHash != "cas-transcript-hash-999" {
		t.Errorf("stage evidence mismatch: %+v", stage)
	}
	if len(gotQC.RelationalQC.QualityResults) != 1 || gotQC.RelationalQC.QualityResults[0].ID != "qr-uuid-001" {
		t.Errorf("relational QC mismatch: %+v", gotQC.RelationalQC)
	}
	if len(gotQC.Telemetry) != 1 || gotQC.Telemetry[0].DevicePeakVRAMBytes != 4294967296 {
		t.Errorf("telemetry mismatch: %+v", gotQC.Telemetry)
	}
}

func TestFileStore_AtomicSaveAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	store, err := benchmark.NewFileStore(tempDir)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}

	input := sampleIdentityInput()
	sess, err := benchmark.NewSession(input)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	sess.RecordAcquisition(benchmark.AcquisitionEntryEvidence{
		EntryID:      "entry_1",
		CanonicalURL: "https://example.com/1",
		Status:       "PASS",
	})

	if err := store.Save(sess); err != nil {
		t.Fatalf("store save: %v", err)
	}

	// Verify files exist
	sessionFile := filepath.Join(tempDir, sess.ID, "benchmark_session.json")
	if _, err := os.Stat(sessionFile); err != nil {
		t.Errorf("expected session file to exist: %v", err)
	}
	identityFile := filepath.Join(tempDir, sess.ID, "identity.json")
	if _, err := os.Stat(identityFile); err != nil {
		t.Errorf("expected identity file to exist: %v", err)
	}

	// Load session back
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("store load: %v", err)
	}
	if loaded.ID != sess.ID {
		t.Errorf("expected session ID %s, got %s", sess.ID, loaded.ID)
	}
	if loaded.IdentityDigest != sess.IdentityDigest {
		t.Errorf("expected digest %s, got %s", sess.IdentityDigest, loaded.IdentityDigest)
	}
	if !loaded.IsAcquisitionComplete("entry_1") {
		t.Errorf("expected entry_1 to be loaded and complete")
	}
}

func TestFileStore_SafeResume_IdenticalIdentity(t *testing.T) {
	tempDir := t.TempDir()
	store, err := benchmark.NewFileStore(tempDir)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}

	input := sampleIdentityInput()

	// Initial resume creates session
	sess1, err := store.Resume(input)
	if err != nil {
		t.Fatalf("initial resume: %v", err)
	}
	if sess1.Status != benchmark.SessionStatusRunning {
		t.Errorf("expected RUNNING status, got %s", sess1.Status)
	}

	// Populate some completed evidence
	sess1.RecordAcquisition(benchmark.AcquisitionEntryEvidence{
		EntryID:      "entry_100",
		CanonicalURL: "https://example.com/100",
		Status:       "PASS",
	})
	sess1.RecordQualityCase(benchmark.QualityCaseEvidence{
		CaseID:        "case_01",
		SourceVideoID: "v1",
		JobID:         "job-uuid-1",
		RunID:         "run-uuid-1",
		Status:        "IN_PROGRESS",
		Stages: map[string]benchmark.StageExecutionEvidence{
			"speech_understand": {
				Stage:         "speech_understand",
				Status:        "COMPLETED",
				OutputCASHash: "hash-stage-1",
			},
		},
	})
	if err := store.Save(sess1); err != nil {
		t.Fatalf("save sess1: %v", err)
	}

	// Interruption / restart: resume with identical identity
	sess2, err := store.Resume(input)
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}

	if sess2.ID != sess1.ID {
		t.Errorf("expected identical session ID %s, got %s", sess1.ID, sess2.ID)
	}

	// Invariant: Completed evidence is safely preserved across resumption
	if !sess2.IsAcquisitionComplete("entry_100") {
		t.Errorf("expected entry_100 evidence preserved across resume")
	}
	qc, ok := sess2.GetQualityCase("case_01")
	if !ok || qc == nil {
		t.Fatalf("expected case_01 preserved across resume")
	}
	if qc.JobID != "job-uuid-1" || qc.RunID != "run-uuid-1" {
		t.Errorf("native job/run IDs not preserved: job=%s run=%s", qc.JobID, qc.RunID)
	}
	if stage, ok := qc.Stages["speech_understand"]; !ok || stage.Status != "COMPLETED" || stage.OutputCASHash != "hash-stage-1" {
		t.Errorf("stage speech_understand not preserved: %+v", stage)
	}
}

func TestFileStore_MismatchRejection_DifferentIdentity(t *testing.T) {
	tempDir := t.TempDir()
	store, err := benchmark.NewFileStore(tempDir)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}

	input := sampleIdentityInput()
	sess, err := store.Resume(input)
	if err != nil {
		t.Fatalf("initial resume: %v", err)
	}

	// Try to resume with modified build identity
	mismatchedInput := sampleIdentityInput()
	mismatchedInput.BuildIdentity = "different_commit_sha_1234567890"

	// 1. ResumeByID should fail with ErrIdentityMismatch
	_, err = store.ResumeByID(sess.ID, mismatchedInput)
	if err == nil {
		t.Fatalf("expected ErrIdentityMismatch on ResumeByID with different identity")
	}
	var mismatchErr *benchmark.ErrIdentityMismatch
	if !errors.As(err, &mismatchErr) {
		t.Fatalf("expected error type *ErrIdentityMismatch, got %T (%v)", err, err)
	}
	if len(mismatchErr.Differences) == 0 {
		t.Errorf("expected mismatch differences to be enumerated")
	}

	// 2. Direct Resume with mismatchedInput initializes a different session rather than merging
	sessDiff, err := store.Resume(mismatchedInput)
	if err != nil {
		t.Fatalf("resume with different input: %v", err)
	}
	if sessDiff.ID == sess.ID {
		t.Errorf("expected different session ID for different identity, got identical %s", sessDiff.ID)
	}
}

func TestFileStore_InvalidSessionID_PathTraversalRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := benchmark.NewFileStore(tempDir)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}

	invalidIDs := []string{
		"../traversal",
		"..\\windows\\system32",
		"../../etc/passwd",
		"bms_short",
		"bms_" + strings.Repeat("z", 64),
		"bms_" + strings.Repeat("A", 64), // uppercase hex rejected
		"session_1234567890abcdef",
		"/absolute/path/attempt",
		"bms_12345!@#$%^&*()_+",
	}

	validInput := sampleIdentityInput()

	for _, invalidID := range invalidIDs {
		t.Run("invalid_"+invalidID, func(t *testing.T) {
			// 1. ValidateSessionID directly fails
			err := benchmark.ValidateSessionID(invalidID)
			if !errors.Is(err, benchmark.ErrInvalidSessionID) {
				t.Errorf("expected ErrInvalidSessionID for %q, got %v", invalidID, err)
			}

			// 2. Load fails
			_, err = store.Load(invalidID)
			if !errors.Is(err, benchmark.ErrInvalidSessionID) {
				t.Errorf("expected Load to reject %q with ErrInvalidSessionID, got %v", invalidID, err)
			}

			// 3. ResumeByID fails
			_, err = store.ResumeByID(invalidID, validInput)
			if !errors.Is(err, benchmark.ErrInvalidSessionID) {
				t.Errorf("expected ResumeByID to reject %q with ErrInvalidSessionID, got %v", invalidID, err)
			}

			// 4. Save with invalid ID fails
			fakeSession := &benchmark.Session{
				ID:     invalidID,
				Status: benchmark.SessionStatusRunning,
			}
			err = store.Save(fakeSession)
			if !errors.Is(err, benchmark.ErrInvalidSessionID) {
				t.Errorf("expected Save to reject %q with ErrInvalidSessionID, got %v", invalidID, err)
			}
		})
	}
}

func TestSession_SelectionDecisionCapture(t *testing.T) {
	sess, err := benchmark.NewSession(sampleIdentityInput())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// 1. Record acquisition with decision
	now := time.Now().UTC()
	acqEvidence := benchmark.AcquisitionEntryEvidence{
		EntryID:      "entry_01",
		CanonicalURL: "https://example.com/video1",
		Status:       "PASS",
		Decisions: []benchmark.SelectionDecisionRef{
			{
				DecisionID:         "dec-acq-01",
				Stage:              "acquisition",
				SelectedProviderID: "douyin_direct_fetcher",
				PolicyCheckResult:  "PASS",
				Reason:             "healthy and under rate limit",
				Timestamp:          now,
			},
		},
	}
	sess.RecordAcquisition(acqEvidence)

	gotAcq, ok := sess.GetAcquisition("entry_01")
	if !ok || len(gotAcq.Decisions) != 1 {
		t.Fatalf("expected 1 acquisition decision, got %+v", gotAcq)
	}
	if gotAcq.Decisions[0].SelectedProviderID != "douyin_direct_fetcher" {
		t.Errorf("expected provider douyin_direct_fetcher, got %s", gotAcq.Decisions[0].SelectedProviderID)
	}

	// 2. Record stage execution with decision
	stageEvidence := benchmark.StageExecutionEvidence{
		Stage:  "translation",
		Status: "COMPLETED",
		Decisions: []benchmark.SelectionDecisionRef{
			{
				DecisionID:         "dec-trans-01",
				Stage:              "translation",
				SelectedProviderID: "qwen3_4b_translator",
				PolicyCheckResult:  "PASS",
				Reason:             "lowest latency local candidate",
				Timestamp:          now,
			},
		},
	}
	sess.UpdateQualityStage("case_01", stageEvidence)

	snap := sess.Snapshot()
	qc, ok := snap.QualityCases["case_01"]
	if !ok || len(qc.Stages["translation"].Decisions) != 1 {
		t.Fatalf("expected 1 stage decision, got %+v", qc)
	}
	if qc.Stages["translation"].Decisions[0].DecisionID != "dec-trans-01" {
		t.Errorf("expected decision ID dec-trans-01, got %s", qc.Stages["translation"].Decisions[0].DecisionID)
	}
}

type mockAuthenticCollector struct {
	samples map[string]benchmark.ResourceTelemetrySample
}

func (m *mockAuthenticCollector) SampleStage(stage string, startedAt time.Time, elapsedMs int64) (benchmark.ResourceTelemetrySample, bool) {
	if s, ok := m.samples[stage]; ok {
		return s, true
	}
	return benchmark.ResourceTelemetrySample{}, false
}

func TestBenchmarkRunner_OptionalTelemetry_NoFabricatedZeros(t *testing.T) {
	sess, err := benchmark.NewSession(sampleIdentityInput())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// By default, no telemetry samples exist
	qc := benchmark.QualityCaseEvidence{
		CaseID: "case_01",
		Status: "IN_PROGRESS",
	}
	sess.RecordQualityCase(qc)

	// With no collector injected, Telemetry must remain nil/empty
	snap := sess.Snapshot()
	if len(snap.QualityCases["case_01"].Telemetry) != 0 {
		t.Errorf("expected 0 telemetry samples by default, got %d", len(snap.QualityCases["case_01"].Telemetry))
	}

	// When authentic collector returns a sample, it is recorded
	collector := &mockAuthenticCollector{
		samples: map[string]benchmark.ResourceTelemetrySample{
			"translation": benchmark.ComputeResourceSample(
				"translation",
				time.Now().UTC(),
				4294967296, // 4 GB baseline
				6442450944, // 6 GB peak
				500,
				1000,
			),
		},
	}

	sample, ok := collector.SampleStage("translation", time.Now().UTC(), 500)
	if !ok {
		t.Fatalf("expected authentic collector to supply sample")
	}
	sess.RecordTelemetry("case_01", sample)

	snap = sess.Snapshot()
	if len(snap.QualityCases["case_01"].Telemetry) != 1 {
		t.Fatalf("expected 1 authentic telemetry sample, got %d", len(snap.QualityCases["case_01"].Telemetry))
	}
	recorded := snap.QualityCases["case_01"].Telemetry[0]
	if recorded.DevicePeakAboveBaselineBytes != 2147483648 { // 2 GB difference
		t.Errorf("expected 2 GB peak above baseline, got %d", recorded.DevicePeakAboveBaselineBytes)
	}
	if recorded.RTF != 0.5 {
		t.Errorf("expected RTF 0.5, got %f", recorded.RTF)
	}
}
