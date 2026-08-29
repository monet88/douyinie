package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

func setupDubbingTestHarness(t *testing.T) (*service.DubbingService, *storage.DB, *cas.Store, *provider.Registry, *provider.Router) {
	t.Helper()
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "dubbing_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}

	reg := provider.NewSeam1FakeRegistry()
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	initCtx := context.Background()
	for _, p := range reg.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
	}
	credSvc := governance.NewCredentialService(db)
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)
	dubSvc := service.NewDubbingService(db, casStore)
	dubSvc.ConfigureRouter(router)
	return dubSvc, db, casStore, reg, router
}

func setupAssetJobRunAudioRole(t *testing.T, db *storage.DB, assetID, runID, lang string) {
	t.Helper()
	_ = db.CreateRightsAttestation(context.Background(), domain.RightsAttestation{
		ID:              "att-" + assetID,
		AttestationType: "OWNER_DIRECT",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
		ID:                  assetID,
		SHA256:              "sha256_" + assetID,
		ByteSize:            1024,
		MimeType:            "video/mp4",
		RightsAttestationID: "att-" + assetID,
		CASPath:             "/mock.mp4",
		CreatedAt:           time.Now().UTC(),
	})
	job := domain.LocalizationJob{
		ID:             "job-" + assetID,
		SourceAssetID:  assetID,
		TargetLanguage: lang,
		Status:         "running",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	_ = db.CreateJob(context.Background(), job)
	run := domain.LocalizationRun{
		ID:                 runID,
		JobID:              job.ID,
		Status:             "running",
		ConfigSnapshotJSON: "{}",
		CreatedAt:          time.Now().UTC(),
	}
	_, _ = db.CreateRunEnqueued(context.Background(), run, job.ID)
	plan := domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	_ = db.SaveAudioRolePlan(context.Background(), plan)
}

func TestDubbingService_AssignVoices_FreezesAndPersists(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// 2. Assign voices for multi-speaker setup
	in := domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	}

	assignment, err := dubSvc.AssignVoices(context.Background(), in)
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	if assignment.CASHash == "" || assignment.ProvenanceHash == "" {
		t.Fatalf("expected populated CASHash and ProvenanceHash")
	}
	if len(assignment.Assignments) == 0 {
		t.Fatalf("expected assigned voices for speakers")
	}

	// 3. Verify retrieval from SQLite and CAS
	idx, err := db.GetVoiceAssignmentIndex(context.Background(), assetID, "vi")
	if err != nil {
		t.Fatalf("GetVoiceAssignmentIndex failed: %v", err)
	}
	if idx.CASHash != assignment.CASHash {
		t.Errorf("expected CASHash %s, got %s", assignment.CASHash, idx.CASHash)
	}

	rc, err := casStore.Get(assignment.CASHash)
	if err != nil {
		t.Fatalf("cas.Get failed: %v", err)
	}
	defer rc.Close()
}

func TestDubbingService_AuditionVoice(t *testing.T) {
	dubSvc, db, _, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	in := domain.VoiceAuditionInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Voice: domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_f1",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
		SampleText: "Thử giọng mẫu",
	}

	res, err := dubSvc.AuditionVoice(context.Background(), in)
	if err != nil {
		t.Fatalf("AuditionVoice failed: %v", err)
	}

	if res.MeasuredDurationMs <= 0 {
		t.Errorf("expected positive measured duration, got %d", res.MeasuredDurationMs)
	}
	if res.AudioCASHash == "" {
		t.Errorf("expected populated AudioCASHash")
	}
}

func TestDubbingService_SynthesizeAndFit_ProbesDurationAndEnforcesZeroOverrun(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")
	// 2. Setup DubScriptVariant with 2 segments
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
				SourceText:     "今天天气很好。",
				MeaningText:    "Hôm nay thời tiết rất tốt.",
				SpokenText:     "Hôm nay thời tiết tốt.",
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          2000,
				SlotDurationMs: 2000,
			},
			{
				Index:          1,
				SourceText:     "我们去公园散步吧。",
				MeaningText:    "Chúng ta đi dạo công viên nhé.",
				SpokenText:     "Đi dạo công viên nhé.",
				SpeakerID:      "SPEAKER_00",
				StartMs:        2200,
				EndMs:          4200,
				SlotDurationMs: 2000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptData, _ := json.Marshal(dubScript)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptData))
	dubScript.CASHash = scriptObj.SHA256
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        scriptObj.SHA256,
		ProvenanceHash: "prov_script_1",
		CreatedAt:      dubScript.CreatedAt,
	})

	// 3. Setup VoiceAssignment
	voiceAssignIn := domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	}
	voiceAssign, err := dubSvc.AssignVoices(context.Background(), voiceAssignIn)
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	// 4. Configure fake provider duration to fit (1500ms <= 2000ms slot)
	fakeTTS, ok := reg.Get("fake_vieneu_tts_vi")
	if !ok {
		t.Fatalf("fake_vieneu_tts_vi not in registry")
	}
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 1500

	// 5. Run SynthesizeAndFit
	jobIn := domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: dubScript.CASHash,
		VoiceAssignmentCAS:  voiceAssign.CASHash,
	}

	dubSegments, err := dubSvc.SynthesizeAndFit(context.Background(), jobIn)
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	if len(dubSegments.Segments) != 2 {
		t.Fatalf("expected 2 dub segments, got %d", len(dubSegments.Segments))
	}
	if dubSegments.OverallStatus != "PASS" {
		t.Errorf("expected OverallStatus PASS, got %s", dubSegments.OverallStatus)
	}

	for _, seg := range dubSegments.Segments {
		if seg.MeasuredDurationMs <= 0 {
			t.Errorf("expected probed duration > 0, got %d", seg.MeasuredDurationMs)
		}
		if seg.MeasuredDurationMs > seg.SlotDurationMs {
			t.Errorf("overrun violated: measured %dms > slot %dms", seg.MeasuredDurationMs, seg.SlotDurationMs)
		}
		if seg.RequiresReview {
			t.Errorf("expected no review required for fitting segment")
		}

		// Verify audio in CAS
		rc, err := casStore.Get(seg.AudioSHA256)
		if err != nil {
			t.Errorf("failed to read synthesized audio from CAS: %v", err)
		} else {
			rc.Close()
		}
	}
}

func TestDubbingService_SynthesizeAndFit_OverlongCandidateFlaggedForReview(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")
	// 2. Setup DubScriptVariant with 1 tight segment (1000ms)
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
				SourceText:     "非常长的句子无法压缩。",
				MeaningText:    "Một câu rất dài không thể nén lại.",
				SpokenText:     "Một câu rất dài không thể nén lại được nữa.",
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          1000,
				SlotDurationMs: 1000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptData, _ := json.Marshal(dubScript)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptData))
	dubScript.CASHash = scriptObj.SHA256
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        scriptObj.SHA256,
		ProvenanceHash: "prov_script_overrun",
		CreatedAt:      dubScript.CreatedAt,
	})

	// 3. Setup VoiceAssignment
	voiceAssign, _ := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})

	// 4. Force severe overrun in fake provider (2500ms vs 1000ms slot)
	fakeTTS, _ := reg.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 2500

	// 5. Run SynthesizeAndFit
	jobIn := domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: dubScript.CASHash,
		VoiceAssignmentCAS:  voiceAssign.CASHash,
	}

	dubSegments, err := dubSvc.SynthesizeAndFit(context.Background(), jobIn)
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	if dubSegments.OverallStatus != "REVIEW_REQUIRED" {
		t.Errorf("expected OverallStatus REVIEW_REQUIRED for overrun, got %s", dubSegments.OverallStatus)
	}
	if len(dubSegments.Segments) != 0 {
		t.Errorf("overlong candidate MUST NOT be selected into Segments, got %d segments", len(dubSegments.Segments))
	}
	if len(dubSegments.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment, got %d", len(dubSegments.ReviewSegments))
	}
	if dubSegments.ReviewSegments[0].ReviewReason != "DURATION_OVERRUN" {
		t.Errorf("expected ReviewReason DURATION_OVERRUN, got %s", dubSegments.ReviewSegments[0].ReviewReason)
	}
	if dubSegments.ReviewSegments[0].MeasuredDurationMs <= dubSegments.ReviewSegments[0].SlotDurationMs {
		t.Errorf("expected measured duration %d > slot duration %d", dubSegments.ReviewSegments[0].MeasuredDurationMs, dubSegments.ReviewSegments[0].SlotDurationMs)
	}
}

func TestDubbingService_SynthesizeAndFit_ProbeFailureFailsClosed(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

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
				SourceText:     "测试音频探测失败。",
				MeaningText:    "Thử nghiệm lỗi probe.",
				SpokenText:     "Thử nghiệm lỗi probe.",
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          2000,
				SlotDurationMs: 2000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptData, _ := json.Marshal(dubScript)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptData))
	dubScript.CASHash = scriptObj.SHA256

	voiceAssign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	// Corrupt the fake provider synthesis output by passing raw invalid audio bytes that fail ProbeWAVBytes
	fakeTTS, _ := reg.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	// Temporarily override invoke to return corrupt non-WAV bytes
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		return &provider.TTSSynthesisResult{
			AudioSHA256:         "sha256_corrupt",
			ProviderID:          fakeProv.ID(),
			PredictedDurationMs: 1500,
			MeasuredDurationMs:  1500, // Provider claims 1500ms, but actual media cannot be probed!
		}, nil
	}

	jobIn := domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: dubScript.CASHash,
		VoiceAssignmentCAS:  voiceAssign.CASHash,
	}

	_, err = dubSvc.SynthesizeAndFit(context.Background(), jobIn)
	if err == nil {
		t.Fatal("expected SynthesizeAndFit to fail closed when actual audio cannot be probed")
	}
}

func TestDubbingService_VoiceAssignment_CrossRunBinding(t *testing.T) {
	dubSvc, db, _, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID1 := uuid.NewString()
	runID2 := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID1, "vi")

	// 1. Assign voices for Run 1
	in1 := domain.VoiceAssignmentInput{
		RunID:          runID1,
		AssetID:        assetID,
		TargetLanguage: "vi",
	}
	assign1, err := dubSvc.AssignVoices(context.Background(), in1)
	if err != nil {
		t.Fatalf("AssignVoices run 1 failed: %v", err)
	}
	if assign1.RunID != runID1 {
		t.Errorf("expected assign1.RunID %s, got %s", runID1, assign1.RunID)
	}

	// 2. Assign voices for Run 2 with identical settings (cache hit on provenance)
	in2 := domain.VoiceAssignmentInput{
		RunID:          runID2,
		AssetID:        assetID,
		TargetLanguage: "vi",
	}
	assign2, err := dubSvc.AssignVoices(context.Background(), in2)
	if err != nil {
		t.Fatalf("AssignVoices run 2 failed: %v", err)
	}

	// Invariant: provenance hash is identical (cache hit without RunID in hash)
	if assign2.ProvenanceHash != assign1.ProvenanceHash {
		t.Errorf("expected same ProvenanceHash, got %s vs %s", assign1.ProvenanceHash, assign2.ProvenanceHash)
	}
	// Invariant: returned object and index are bound to runID2
	if assign2.RunID != runID2 {
		t.Errorf("expected assign2.RunID %s, got %s", runID2, assign2.RunID)
	}

	// Check DB lookup by runID1 and runID2
	idx1, err := db.GetVoiceAssignmentIndexByRun(context.Background(), assetID, runID1, "vi")
	if err != nil || idx1.RunID != runID1 {
		t.Fatalf("expected index for run 1, got err: %v, idx: %+v", err, idx1)
	}
	idx2, err := db.GetVoiceAssignmentIndexByRun(context.Background(), assetID, runID2, "vi")
	if err != nil || idx2.RunID != runID2 {
		t.Fatalf("expected index for run 2, got err: %v, idx: %+v", err, idx2)
	}
}
func TestDubbingService_VoiceAssignment_SameRun_IdempotentEquivalence(t *testing.T) {
	dubSvc, db, _, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// 1. Initial VoiceAssignment for Run
	in := domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	}
	assign1, err := dubSvc.AssignVoices(context.Background(), in)
	if err != nil {
		t.Fatalf("initial AssignVoices failed: %v", err)
	}

	// 2. Re-call AssignVoices for the SAME run with equivalent input
	assign2, err := dubSvc.AssignVoices(context.Background(), in)
	if err != nil {
		t.Fatalf("idempotent AssignVoices failed: %v", err)
	}

	if assign2.CASHash != assign1.CASHash {
		t.Errorf("expected same CASHash on idempotent re-call, got %s vs %s", assign1.CASHash, assign2.CASHash)
	}
	if assign2.ProvenanceHash != assign1.ProvenanceHash {
		t.Errorf("expected same ProvenanceHash, got %s vs %s", assign1.ProvenanceHash, assign2.ProvenanceHash)
	}
}

func TestDubbingService_VoiceAssignment_SameRun_ConflictingReassignmentFailsClosedFrozen(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// 1. Initial VoiceAssignment for Run (default preset mapping)
	in1 := domain.VoiceAssignmentInput{
		RunID:              runID,
		AssetID:            assetID,
		TargetLanguage:     "vi",
		UseSameVoiceForAll: false,
	}
	assign1, err := dubSvc.AssignVoices(context.Background(), in1)
	if err != nil {
		t.Fatalf("initial AssignVoices failed: %v", err)
	}
	origCASHash := assign1.CASHash
	origProvenance := assign1.ProvenanceHash

	// 2. Conflicting re-call: attempt to mutate UseSameVoiceForAll to true
	inConflict1 := domain.VoiceAssignmentInput{
		RunID:              runID,
		AssetID:            assetID,
		TargetLanguage:     "vi",
		UseSameVoiceForAll: true,
	}
	_, err = dubSvc.AssignVoices(context.Background(), inConflict1)
	if !errors.Is(err, domain.ErrVoiceAssignmentFrozen) {
		t.Fatalf("expected ErrVoiceAssignmentFrozen on UseSameVoiceForAll conflict, got: %v", err)
	}

	// 3. Conflicting re-call: attempt to mutate CustomAssignments to a different voice
	inConflict2 := domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "custom_mutated_voice",
				ProviderID: "vieneu_tts_vi",
				VoiceID:    "mutated_voice",
				Language:   "vi",
			},
		},
	}
	_, err = dubSvc.AssignVoices(context.Background(), inConflict2)
	if !errors.Is(err, domain.ErrVoiceAssignmentFrozen) {
		t.Fatalf("expected ErrVoiceAssignmentFrozen on CustomAssignments conflict, got: %v", err)
	}

	// 4. Verify the original assignment remains completely unchanged and loadable in DB and CAS
	idx, err := db.GetVoiceAssignmentIndexByRun(context.Background(), assetID, runID, "vi")
	if err != nil {
		t.Fatalf("failed to get voice assignment index: %v", err)
	}
	if idx.CASHash != origCASHash {
		t.Errorf("CASHash was mutated in DB! expected %s, got %s", origCASHash, idx.CASHash)
	}
	if idx.ProvenanceHash != origProvenance {
		t.Errorf("ProvenanceHash was mutated in DB! expected %s, got %s", origProvenance, idx.ProvenanceHash)
	}

	rc, err := casStore.Get(origCASHash)
	if err != nil {
		t.Fatalf("failed to retrieve original CAS object: %v", err)
	}
	defer rc.Close()
	var loaded domain.VoiceAssignment
	if err := json.NewDecoder(rc).Decode(&loaded); err != nil {
		t.Fatalf("failed to decode original CAS object: %v", err)
	}
	if loaded.UseSameVoiceForAll != false {
		t.Errorf("original CAS object was mutated!")
	}
}
func TestDubbingService_DubSegmentsVariant_CreatedAtNonZero(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// Setup DubScriptVariant in CAS
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               3000,
				SlotDurationMs:      3000,
				SpokenText:          "Chào bạn",
				SourceText:          "你好",
				EstimatedDurationMs: 1200,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, err := casStore.Put(bytes.NewReader(scriptBytes))
	if err != nil {
		t.Fatalf("put script CAS: %v", err)
	}
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        scriptCAS.SHA256,
		ProvenanceHash: "prov_script",
		CreatedAt:      time.Now().UTC(),
	})

	// Setup VoiceAssignment in CAS
	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	// Synthesize and fit
	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	if variant.CreatedAt.IsZero() {
		t.Fatalf("expected non-zero CreatedAt on DubSegmentsVariant, got zero time")
	}

	// Check SQLite persisted index
	idx, err := db.GetDubSegmentsVariantIndex(context.Background(), assetID, "vi")
	if err != nil {
		t.Fatalf("GetDubSegmentsVariantIndex failed: %v", err)
	}
	if idx.CreatedAt.IsZero() {
		t.Fatalf("expected non-zero CreatedAt on stored SQLite index")
	}
}

func TestDubbingService_SynthesizeAndFit_Regroup_SameSpeakerSuccess(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// Two adjacent segments for the SAME speaker:
	// Seg 0 has tight slot 500ms (so individual synthesis at ~800ms overruns)
	// Seg 1 has slot 2000ms (500ms to 2500ms).
	// Combined window = 0ms to 2500ms (2500ms slot), easily accommodating the merged ~1500ms synthesis!
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               500,
				SlotDurationMs:      500,
				SpokenText:          "Vế thứ nhất",
				SourceText:          "前半句",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 800,
			},
			{
				Index:               1,
				SpeakerID:           "SPEAKER_00",
				StartMs:             600,
				EndMs:               2500,
				SlotDurationMs:      1900,
				SpokenText:          "vế thứ hai.",
				SourceText:          "后半句。",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 800,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	// Should have successfully regrouped the 2 same-speaker segments into 1 DubSegment covering [0, 1]
	if len(variant.Segments) != 1 {
		t.Fatalf("expected exactly 1 regrouped DubSegment in Segments, got %d (review: %d)", len(variant.Segments), len(variant.ReviewSegments))
	}
	regroupedSeg := variant.Segments[0]
	if len(regroupedSeg.SpeechBlockIndices) != 2 || regroupedSeg.SpeechBlockIndices[0] != 0 || regroupedSeg.SpeechBlockIndices[1] != 1 {
		t.Errorf("expected SpeechBlockIndices [0, 1], got %v", regroupedSeg.SpeechBlockIndices)
	}
	if regroupedSeg.StartMs != 0 || regroupedSeg.EndMs != 2500 {
		t.Errorf("expected combined timing [0, 2500], got [%d, %d]", regroupedSeg.StartMs, regroupedSeg.EndMs)
	}
	if regroupedSeg.SlotDurationMs != 2500 {
		t.Errorf("expected combined slot duration 2500ms, got %dms", regroupedSeg.SlotDurationMs)
	}
	if len(variant.ReviewSegments) != 0 {
		t.Errorf("expected 0 review segments after successful regroup, got %d", len(variant.ReviewSegments))
	}
}

func TestDubbingService_SynthesizeAndFit_Regroup_DifferentSpeakerOrNonEligibleGapNeverRegroups(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// Case A: Different speakers (SPEAKER_00 and SPEAKER_01)
	// Must NEVER regroup across different speakers!
	dubScriptDiffSpk := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               500,
				SlotDurationMs:      500,
				SpokenText:          "Người thứ nhất nói",
				SourceText:          "第一个人说",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1500,
			},
			{
				Index:               1,
				SpeakerID:           "SPEAKER_01",
				StartMs:             600,
				EndMs:               2500,
				SlotDurationMs:      1900,
				SpokenText:          "Người thứ hai trả lời.",
				SourceText:          "第二个人回答。",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1200,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytesA, _ := json.Marshal(dubScriptDiffSpk)
	scriptCASA, _ := casStore.Put(bytes.NewReader(scriptBytesA))

	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	variantA, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCASA.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit Case A failed: %v", err)
	}

	// Seg 0 must be in ReviewSegments (NOT regrouped with Seg 1 because different speakers)
	if len(variantA.ReviewSegments) == 0 {
		t.Fatalf("Case A: expected segment 0 in ReviewSegments, got 0 review segments")
	}
	if variantA.ReviewSegments[0].Index != 0 || variantA.ReviewSegments[0].SpeakerID != "SPEAKER_00" {
		t.Errorf("Case A: expected review segment for SPEAKER_00 at index 0, got %+v", variantA.ReviewSegments[0])
	}

	// Case B: Same speaker (SPEAKER_00) but non-eligible distant gap (SourceGapAfterMs >= 600ms)
	// Must NEVER regroup across non-eligible gaps!
	dubScriptNonEligibleGap := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               500,
				SlotDurationMs:      500,
				SpokenText:          "Vế thứ nhất rất dài",
				SourceText:          "前半句很长",
				SourceGapAfterMs:    800, // gap >= 600ms is non-eligible
				EstimatedDurationMs: 1500,
			},
			{
				Index:               1,
				SpeakerID:           "SPEAKER_00",
				StartMs:             1300,
				EndMs:               3000,
				SlotDurationMs:      1700,
				SpokenText:          "vế thứ hai sau khoảng lặng.",
				SourceText:          "后半句在长时间停顿后。",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1200,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytesB, _ := json.Marshal(dubScriptNonEligibleGap)
	scriptCASB, _ := casStore.Put(bytes.NewReader(scriptBytesB))

	variantB, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCASB.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit Case B failed: %v", err)
	}

	// Seg 0 must be in ReviewSegments (NOT regrouped with Seg 1 because gap >= 600ms)
	if len(variantB.ReviewSegments) == 0 {
		t.Fatalf("Case B: expected segment 0 in ReviewSegments for non-eligible gap, got 0 review segments")
	}
	if variantB.ReviewSegments[0].Index != 0 {
		t.Errorf("Case B: expected review segment at index 0, got %+v", variantB.ReviewSegments[0])
	}
}
func TestDubbingService_NoSentenceBySentenceEngineHopping(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// Register two viable TTS providers for 'vi'
	// 1. Primary/frozen provider: fake_vieneu_tts_vi
	// 2. Alternative provider: fake_alt_tts_vi (healthy, allowed)
	altTTS := provider.NewFakeTTSProvider("fake_alt_tts_vi", 1500)
	licSvc := governance.NewLicenseService(db)
	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		DependencyName: altTTS.ModelName,
		Version:        altTTS.ModelVersion,
		SHA256:         "sha256_mock_" + altTTS.ModelName,
		SourceRepo:     "github.com/monet88/douyinie/models/" + altTTS.ModelName,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	_ = reg.Register(altTTS)

	// Make the primary frozen provider fail
	fakeTTS, _ := reg.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.InjectError = errors.New("primary tts synthesis failure")
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               3000,
				SlotDurationMs:      3000,
				SpokenText:          "Kiểm tra không nhảy engine",
				SourceText:          "测试不跳引擎",
				EstimatedDurationMs: 1200,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	// VoiceAssignment freezes fake_vieneu_tts_vi for SPEAKER_00
	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {
				ID:         "vieneu_voice",
				ProviderID: "fake_vieneu_tts_vi",
				VoiceID:    "vi_natural",
				Language:   "vi",
			},
		},
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	// Synthesize must FAIL closed because frozen provider failed, and must NOT silently hop to fake_alt_tts_vi!
	_, err = dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err == nil {
		t.Fatalf("expected SynthesizeAndFit to fail when frozen provider failed, but it succeeded (engine hopped!)")
	}
	// Invariant 1: Alternative viable provider MUST NOT have been invoked
	if altTTS.Invocations != 0 {
		t.Fatalf("CRITICAL ENGINE-HOPPING VIOLATION: fake_alt_tts_vi was invoked %d times!", altTTS.Invocations)
	}

	// Invariant 2: Router ExecuteRoutedWithRetry recorded ProviderAttempt rows for the failed frozen provider
	attempts, err := db.ListProviderAttempts(context.Background(), runID, "tts")
	if err != nil {
		t.Fatalf("ListProviderAttempts failed: %v", err)
	}
	if len(attempts) == 0 {
		t.Fatalf("expected ProviderAttempt rows to be recorded by Router for frozen provider, got 0")
	}
	for _, att := range attempts {
		if att.ProviderID != "fake_vieneu_tts_vi" {
			t.Errorf("expected ProviderAttempt only for frozen fake_vieneu_tts_vi, got provider %s", att.ProviderID)
		}
		if att.Status != "failed" {
			t.Errorf("expected failed status, got %s", att.Status)
		}
	}
}

func TestDubbingService_SynthesizeAndFit_Regroup_ThreeBlocksSuccess(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// 3 adjacent segments for the SAME speaker:
	// Seg 0: slot 500ms (synthesis ~1200ms -> overruns 500ms -> REGROUP)
	// Seg 1: slot 500ms (combined 0..1100ms = 1100ms slot; 2-block synthesis ~1500ms -> overruns 1100ms -> REGROUP again!)
	// Seg 2: slot 2000ms (combined 0..3200ms = 3200ms slot; 3-block synthesis ~2000ms -> fits 3200ms slot -> ACCEPT!)
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               500,
				SlotDurationMs:      500,
				SpokenText:          "Vế một",
				SourceText:          "第一句",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1200,
			},
			{
				Index:               1,
				SpeakerID:           "SPEAKER_00",
				StartMs:             600,
				EndMs:               1100,
				SlotDurationMs:      500,
				SpokenText:          "vế hai",
				SourceText:          "第二句",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1200,
			},
			{
				Index:               2,
				SpeakerID:           "SPEAKER_00",
				StartMs:             1200,
				EndMs:               3200,
				SlotDurationMs:      2000,
				SpokenText:          "và vế ba hoàn chỉnh.",
				SourceText:          "和第三句。",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 800,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	// Should have successfully regrouped all 3 same-speaker segments into 1 DubSegment covering [0, 1, 2]
	if len(variant.Segments) != 1 {
		t.Fatalf("expected exactly 1 regrouped DubSegment covering 3 blocks, got %d (review: %d)", len(variant.Segments), len(variant.ReviewSegments))
	}
	regroupedSeg := variant.Segments[0]
	if len(regroupedSeg.SpeechBlockIndices) != 3 || regroupedSeg.SpeechBlockIndices[0] != 0 || regroupedSeg.SpeechBlockIndices[1] != 1 || regroupedSeg.SpeechBlockIndices[2] != 2 {
		t.Errorf("expected SpeechBlockIndices [0, 1, 2], got %v", regroupedSeg.SpeechBlockIndices)
	}
	if regroupedSeg.StartMs != 0 || regroupedSeg.EndMs != 3200 {
		t.Errorf("expected combined timing [0, 3200], got [%d, %d]", regroupedSeg.StartMs, regroupedSeg.EndMs)
	}
	if regroupedSeg.SlotDurationMs != 3200 {
		t.Errorf("expected combined slot duration 3200ms, got %dms", regroupedSeg.SlotDurationMs)
	}
	if len(variant.ReviewSegments) != 0 {
		t.Errorf("expected 0 review segments after successful 3-block regroup, got %d", len(variant.ReviewSegments))
	}
}

func TestDubbingService_SynthesizeAndFit_Regroup_UnresolvedWithoutFurtherBlocks_GoesToReview(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, assetID, runID, "vi")

	// 2 adjacent segments for the SAME speaker, both very short slots:
	// Seg 0: slot 300ms (synthesis ~1500ms -> overruns -> REGROUP)
	// Seg 1: slot 300ms (combined slot = 700ms; combined synthesis ~1500ms -> still overruns 700ms!)
	// No further block exists for this speaker -> must place entire consumed group in ReviewSegments!
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:               0,
				SpeakerID:           "SPEAKER_00",
				StartMs:             0,
				EndMs:               300,
				SlotDurationMs:      300,
				SpokenText:          "Vế ngắn thứ nhất rất dài",
				SourceText:          "第一句很长",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1500,
			},
			{
				Index:               1,
				SpeakerID:           "SPEAKER_00",
				StartMs:             400,
				EndMs:               700,
				SlotDurationMs:      300,
				SpokenText:          "vế ngắn thứ hai cũng rất dài.",
				SourceText:          "第二句也很长。",
				SourceGapAfterMs:    100,
				EstimatedDurationMs: 1500,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	// Unresolved REGROUP must NOT be selected into Segments
	if len(variant.Segments) != 0 {
		t.Fatalf("expected 0 selected Segments for unresolved regroup, got %d", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected exactly 1 ReviewSegment for the consumed regroup, got %d", len(variant.ReviewSegments))
	}
	revSeg := variant.ReviewSegments[0]
	if revSeg.Index != 0 {
		t.Errorf("expected review segment index 0, got %d", revSeg.Index)
	}
	if revSeg.StartMs != 0 || revSeg.EndMs != 700 {
		t.Errorf("expected combined timing [0, 700], got [%d, %d]", revSeg.StartMs, revSeg.EndMs)
	}
	if revSeg.FitDecision != domain.FitActionReview {
		t.Errorf("expected FitDecision REVIEW, got %s", revSeg.FitDecision)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" {
		t.Errorf("expected overall status REVIEW_REQUIRED, got %s", variant.OverallStatus)
	}
}
