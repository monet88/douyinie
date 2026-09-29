package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
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
	router.SetAudioRolePlanResolver(func(ctx context.Context, assetID, runID string) (*domain.AudioRolePlan, error) {
		return service.ResolveRunScopedAudioRolePlan(ctx, db, casStore, assetID, runID)
	})
	dubSvc := service.NewDubbingService(db, casStore)
	dubSvc.ConfigureRouter(router)
	return dubSvc, db, casStore, reg, router
}

func setupAssetJobRunAudioRole(t *testing.T, db *storage.DB, casStore *cas.Store, assetID, runID, lang string) {
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
		ID:             "plan-" + assetID,
		AssetID:        assetID,
		ProviderID:     "test-role-provider",
		ModelName:      "test-role-model",
		ModelVersion:   "1",
		ProvenanceHash: "prov-role-" + assetID,
		CreatedAt:      time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBytes, _ := json.Marshal(plan)
	planObj, err := casStore.Put(bytes.NewReader(planBytes))
	if err != nil {
		t.Fatalf("put audio role plan: %v", err)
	}
	plan.CASHash = planObj.SHA256
	_ = db.SaveAudioRolePlan(context.Background(), plan)
	_ = db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "audio_role_plan",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: planObj.SHA256,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})
}

func pinDubbingScriptLineage(t *testing.T, db *storage.DB, casStore *cas.Store, dubScript *domain.DubScriptVariant, runID string, glossary ...domain.GlossaryEntry) {
	t.Helper()
	if dubScript.RunID == "" {
		dubScript.RunID = runID
	}
	if dubScript.SchemaVersion == 0 {
		dubScript.SchemaVersion = domain.DubScriptSchemaVersion
	}
	if dubScript.SourceLanguage == "" {
		dubScript.SourceLanguage = "zh"
	}
	if dubScript.ProvenanceHash == "" {
		dubScript.ProvenanceHash = "prov-dubscript-" + dubScript.ID
	}
	tv := domain.TranslationVariant{
		ID:             "translation-" + dubScript.ID,
		SchemaVersion:  domain.TranslationSchemaVersion,
		AssetID:        dubScript.AssetID,
		RunID:          runID,
		SourceLanguage: dubScript.SourceLanguage,
		TargetLanguage: dubScript.TargetLanguage,
		ContractID:     service.TranslationContractID,
		ProvenanceHash: "prov-translation-" + dubScript.ID,
		ProviderID:     "test-translation-provider",
		ModelName:      "test-translation-model",
		ModelVersion:   "1",
		CreatedAt:      time.Now().UTC(),
	}
	if len(glossary) > 0 {
		tv.EffectiveGlossary = domain.EffectiveGlossary{Entries: append([]domain.GlossaryEntry(nil), glossary...), Hash: "test-effective-glossary"}
	}
	blocks := make([]domain.SpeechBlock, 0, len(dubScript.Segments))
	for _, seg := range dubScript.Segments {
		tv.Segments = append(tv.Segments, domain.TranslationSegment{
			Index: seg.Index, SourceText: seg.SourceText, TargetText: seg.MeaningText,
			SpeakerID: seg.SpeakerID, StartMs: seg.StartMs, EndMs: seg.EndMs,
		})
		blocks = append(blocks, domain.SpeechBlock{
			Index: seg.Index, StartMs: seg.StartMs, EndMs: seg.EndMs, SpeakerID: seg.SpeakerID,
			SourceText: seg.SourceText, SegmentType: domain.SpeechBlockTypeSpeech,
		})
	}
	tvBytes, _ := json.Marshal(tv)
	tvObj, err := casStore.Put(bytes.NewReader(tvBytes))
	if err != nil {
		t.Fatalf("put translation contract: %v", err)
	}
	dubScript.TranslationVariantCAS = tvObj.SHA256

	transcript := domain.TranscriptArtifact{
		ID: "transcript-" + dubScript.ID, AssetID: dubScript.AssetID, RunID: runID,
		SourceLanguage: dubScript.SourceLanguage, SpeechBlocks: blocks,
		ProvenanceHash: "prov-transcript-" + dubScript.ID, CreatedAt: time.Now().UTC(),
	}
	transcriptBytes, _ := json.Marshal(transcript)
	transcriptObj, err := casStore.Put(bytes.NewReader(transcriptBytes))
	if err != nil {
		t.Fatalf("put transcript artifact: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(context.Background(), storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: transcript.AssetID, RunID: runID, CASHash: transcriptObj.SHA256,
		ProvenanceHash: transcript.ProvenanceHash, CreatedAt: transcript.CreatedAt,
	}); err != nil {
		t.Fatalf("save transcript artifact index: %v", err)
	}
	scriptBytes, err := json.Marshal(dubScript)
	if err != nil {
		t.Fatalf("marshal dub script lineage fixture: %v", err)
	}
	scriptObj, err := casStore.Put(bytes.NewReader(scriptBytes))
	if err != nil {
		t.Fatalf("put dub script lineage fixture: %v", err)
	}
	if err := db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: dubScript.AssetID, RunID: runID, TargetLanguage: dubScript.TargetLanguage,
		CASHash: scriptObj.SHA256, ProvenanceHash: dubScript.ProvenanceHash, OverallQAScore: dubScript.OverallQAScore,
		CreatedAt: dubScript.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script lineage fixture index: %v", err)
	}
}

func TestDubbingService_AssignVoices_FreezesAndPersists(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	if res.ProviderID == "" {
		t.Errorf("expected populated ProviderID, got empty")
	}
	if res.ModelName == "" {
		t.Errorf("expected populated ModelName, got empty")
	}
}

func TestDubbingService_AuditionVoice_Contextual_PreviewLocalAndFailClosed(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// 1. Setup AudioRolePlan with mid-video dialogue (30s-34s) in 60s video
	service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, domain.AudioRolePlan{
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 15000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 15000, EndMs: 30000, Role: domain.AudioRoleSingingMusicVocal},
			{StartMs: 30000, EndMs: 34000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 34000, EndMs: 60000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})

	// 2. Setup DubScriptVariant at segment index 0 (30000ms-34000ms)
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
				StartMs:        30000,
				EndMs:          34000,
				SlotDurationMs: 4000,
				SpokenText:     "Đoạn hội thoại ở giữa video.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_mid",
		CreatedAt:      time.Now().UTC(),
	})

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	// Fail closed when stems are missing
	_, err := dubSvc.AuditionVoice(context.Background(), in)
	if err == nil || !errors.Is(err, domain.ErrAudioStemsNotFound) {
		t.Fatalf("expected ErrAudioStemsNotFound when stems missing, got: %v", err)
	}

	// Setup 60s background and vocal stems in CAS + SQLite
	bgPCM := media.GeneratePCM16WAV(16000, 1, 60000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	vocalsPCM := media.GeneratePCM16WAV(16000, 1, 60000)
	vocalsObj, _ := casStore.Put(bytes.NewReader(vocalsPCM))

	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   60000,
			},
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: vocalsObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   60000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_mid_test",
		CreatedAt:      time.Now().UTC(),
	})

	res, err := dubSvc.AuditionVoice(context.Background(), in)
	if err != nil {
		t.Fatalf("AuditionVoice failed: %v", err)
	}

	if !res.IsContextual {
		t.Errorf("expected IsContextual=true")
	}
	if !res.ContextualMixed {
		t.Errorf("expected ContextualMixed=true")
	}
	if res.MeasuredDurationMs != 10000 {
		t.Errorf("expected preview-local measured duration of 10000ms, got %dms", res.MeasuredDurationMs)
	}
	if res.SampleText != "Đoạn hội thoại ở giữa video." {
		t.Errorf("expected sample text from DubScriptVariant, got %q", res.SampleText)
	}
}

func TestDubbingService_AuditionVoice_Contextual_FailClosedWithoutDubScriptOrAudioRolePlan(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Setup background stem so stem requirement is satisfied
	bgPCM := media.GeneratePCM16WAV(16000, 1, 20000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   20000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_failclosed_test",
		CreatedAt:      time.Now().UTC(),
	})

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	// 1. Missing DubScriptVariant -> MUST fail closed with ErrDubScriptVariantNotFound (no fallback to generic sentence)
	_, err := dubSvc.AuditionVoice(context.Background(), in)
	if err == nil || !errors.Is(err, domain.ErrDubScriptVariantNotFound) {
		t.Fatalf("expected ErrDubScriptVariantNotFound when DubScriptVariant is missing, got: %v", err)
	}

	// Now setup DubScriptVariant for assetID
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
				EndMs:          2000,
				SlotDurationMs: 2000,
				SpokenText:     "Câu thoại thực tế.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_failclosed",
		CreatedAt:      time.Now().UTC(),
	})

	// 2. Missing AudioRolePlan for another asset -> MUST fail closed with ErrAudioRolePlanRequired
	assetIDNoPlan := uuid.NewString()
	runIDNoPlan := uuid.NewString()
	_ = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
		ID:                  assetIDNoPlan,
		SHA256:              "sha256_" + assetIDNoPlan,
		ByteSize:            1024,
		MimeType:            "video/mp4",
		RightsAttestationID: "att-" + assetID,
		CASPath:             "/mock.mp4",
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             uuid.NewString(),
		AssetID:        assetIDNoPlan,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_noplan_test",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             uuid.NewString(),
		AssetID:        assetIDNoPlan,
		RunID:          runIDNoPlan,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_noplan",
		CreatedAt:      time.Now().UTC(),
	})

	inNoPlan := domain.VoiceAuditionInput{
		RunID:          runIDNoPlan,
		AssetID:        assetIDNoPlan,
		TargetLanguage: "vi",
		Voice: domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_f1",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
		IsContextual: true,
		SegmentIndex: 0,
	}
	_, err = dubSvc.AuditionVoice(context.Background(), inNoPlan)
	if err == nil || !errors.Is(err, domain.ErrAudioRolePlanRequired) {
		t.Fatalf("expected ErrAudioRolePlanRequired when AudioRolePlan is missing, got: %v", err)
	}
}

func TestDubbingService_AuditionVoice_Contextual_NearSourceEndClamped(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Total audio is 15s (15000ms). Segment is at 13000ms-15000ms (near source end).
	service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, domain.AudioRolePlan{
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 13000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 13000, EndMs: 15000, Role: domain.AudioRoleNarrationDialogue},
		},
	})

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
				StartMs:        13000,
				EndMs:          15000,
				SlotDurationMs: 2000,
				SpokenText:     "Đoạn kết thúc video.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_end",
		CreatedAt:      time.Now().UTC(),
	})

	bgPCM := media.GeneratePCM16WAV(16000, 1, 15000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   15000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_end_test",
		CreatedAt:      time.Now().UTC(),
	})

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	res, err := dubSvc.AuditionVoice(context.Background(), in)
	if err != nil {
		t.Fatalf("AuditionVoice failed on near-end segment: %v", err)
	}
	if !res.ContextualMixed {
		t.Errorf("expected ContextualMixed=true")
	}
	if res.MeasuredDurationMs != 10000 {
		t.Errorf("expected 10000ms preview duration, got %dms", res.MeasuredDurationMs)
	}
	if res.SampleText != "Đoạn kết thúc video." {
		t.Errorf("expected sample text from segment, got %q", res.SampleText)
	}
}

func TestDubbingService_AuditionVoice_Contextual_SpeechExceedsSourceTailFailsClosed(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Total audio is 15s (15000ms). Segment starts at 13000ms, dialogue is 13000ms-15000ms.
	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 13000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 13000, EndMs: 15000, Role: domain.AudioRoleNarrationDialogue},
		},
	})

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
				StartMs:        13000,
				EndMs:          15000,
				SlotDurationMs: 2000,
				SpokenText:     "Câu thoại quá dài không thể vừa với đoạn kết.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_overflow",
		CreatedAt:      time.Now().UTC(),
	})

	bgPCM := media.GeneratePCM16WAV(16000, 1, 15000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   15000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_overflow_test",
		CreatedAt:      time.Now().UTC(),
	})

	// Override TTSInvoke to synthesize 4000ms speech (13000 + 4000 = 17000ms > 15000ms total background)
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		pcm := media.GeneratePCM16WAV(16000, 1, 4000)
		return &provider.TTSSynthesisResult{
			AudioData:          pcm,
			Format:             "wav",
			SampleRate:         16000,
			Channels:           1,
			MeasuredDurationMs: 4000,
			ProviderID:         p.ID(),
			ModelName:          "test_tts",
			ModelVersion:       "1.0",
		}, nil
	}

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	_, err := dubSvc.AuditionVoice(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error when synthesized speech exceeds source tail bounds, got nil")
	}
	if !errors.Is(err, domain.ErrSoundtrackPreservationFailed) {
		t.Fatalf("expected ErrSoundtrackPreservationFailed, got: %v", err)
	}
}

func TestDubbingService_AuditionVoice_Contextual_SpeechExceedsNominal10sExpandsWindow(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Total audio is 40s (40000ms). Segment starts at 5000ms, dialogue is 5000ms-20000ms (15s dialogue window).
	service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 5000, EndMs: 20000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 20000, EndMs: 40000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})

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
				StartMs:        5000,
				EndMs:          18000,
				SlotDurationMs: 13000,
				SpokenText:     "Đoạn văn dịch dài mười hai giây cần mở rộng cửa sổ preview.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_12s",
		CreatedAt:      time.Now().UTC(),
	})

	bgPCM := media.GeneratePCM16WAV(16000, 1, 40000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   40000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_12s_test",
		CreatedAt:      time.Now().UTC(),
	})

	// Override TTSInvoke to synthesize 12000ms speech
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		pcm := media.GeneratePCM16WAV(16000, 1, 12000)
		return &provider.TTSSynthesisResult{
			AudioData:          pcm,
			Format:             "wav",
			SampleRate:         16000,
			Channels:           1,
			MeasuredDurationMs: 12000,
			ProviderID:         p.ID(),
			ModelName:          "test_tts",
			ModelVersion:       "1.0",
		}, nil
	}

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	res, err := dubSvc.AuditionVoice(context.Background(), in)
	if err != nil {
		t.Fatalf("AuditionVoice failed for 12s speech: %v", err)
	}
	if !res.ContextualMixed {
		t.Errorf("expected ContextualMixed=true")
	}
	if res.MeasuredDurationMs < 12000 {
		t.Errorf("expected preview window >= 12000ms to contain full speech, got %dms", res.MeasuredDurationMs)
	}
}

func TestDubbingService_AuditionVoice_Contextual_UncoveredSuppressionFailsClosed(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	bgPCM := media.GeneratePCM16WAV(16000, 1, 30000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   30000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_uncovered_test",
		CreatedAt:      time.Now().UTC(),
	})

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
				StartMs:        2000,
				EndMs:          6000,
				SlotDurationMs: 4000,
				SpokenText:     "Kiểm tra đoạn không được che phủ.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_uncovered",
		CreatedAt:      time.Now().UTC(),
	})

	// TTS generates 4000ms speech from 2000ms to 6000ms
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		pcm := media.GeneratePCM16WAV(16000, 1, 4000)
		return &provider.TTSSynthesisResult{
			AudioData:          pcm,
			Format:             "wav",
			SampleRate:         16000,
			Channels:           1,
			MeasuredDurationMs: 4000,
			ProviderID:         p.ID(),
			ModelName:          "test_tts",
			ModelVersion:       "1.0",
		}, nil
	}

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	t.Run("SpeechExceedsDialogueEnd", func(t *testing.T) {
		// Dialogue is only 2000ms-4000ms, but speech runs 2000ms-6000ms -> uncovered from 4000ms to 6000ms
		service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, domain.AudioRolePlan{
			ID:        uuid.NewString(),
			AssetID:   assetID,
			CreatedAt: time.Now().UTC(),
			Segments: []domain.AudioSegment{
				{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
				{StartMs: 2000, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
				{StartMs: 4000, EndMs: 30000, Role: domain.AudioRoleInstrumentalBgm},
			},
		})
		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil || !errors.Is(err, domain.ErrSoundtrackPreservationFailed) {
			t.Fatalf("expected ErrSoundtrackPreservationFailed when dialogue suppression window is shorter than speech, got: %v", err)
		}
	})

	t.Run("DialogueGapDuringSpeech", func(t *testing.T) {
		// Dialogue has a gap from 3500ms to 4500ms (e.g. singing) during 2000ms-6000ms speech
		service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, domain.AudioRolePlan{
			ID:        uuid.NewString(),
			AssetID:   assetID,
			CreatedAt: time.Now().UTC(),
			Segments: []domain.AudioSegment{
				{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleInstrumentalBgm},
				{StartMs: 2000, EndMs: 3500, Role: domain.AudioRoleNarrationDialogue},
				{StartMs: 3500, EndMs: 4500, Role: domain.AudioRoleSingingMusicVocal},
				{StartMs: 4500, EndMs: 6000, Role: domain.AudioRoleNarrationDialogue},
				{StartMs: 6000, EndMs: 30000, Role: domain.AudioRoleInstrumentalBgm},
			},
		})
		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil || !errors.Is(err, domain.ErrSoundtrackPreservationFailed) {
			t.Fatalf("expected ErrSoundtrackPreservationFailed when dialogue suppression window has gaps, got: %v", err)
		}
	})

	t.Run("NoDialogueSuppression", func(t *testing.T) {
		// Entire interval is singing/music-vocal
		service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, domain.AudioRolePlan{
			ID:        uuid.NewString(),
			AssetID:   assetID,
			CreatedAt: time.Now().UTC(),
			Segments: []domain.AudioSegment{
				{StartMs: 0, EndMs: 30000, Role: domain.AudioRoleSingingMusicVocal},
			},
		})
		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil {
			t.Fatalf("expected error when no dialogue suppression exists")
		}
	})
}

func TestDubbingService_AuditionVoice_Contextual_BrokenDeclaredVocalsStemFailsClosed(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 5000, EndMs: 20000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})

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
				EndMs:          2000,
				SlotDurationMs: 2000,
				SpokenText:     "Thử nghiệm vocal stem hỏng.",
			},
		},
	}
	dBytes, _ := json.Marshal(dubScript)
	dObj, _ := casStore.Put(bytes.NewReader(dBytes))
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dObj.SHA256,
		ProvenanceHash: "prov_dub_script_vocal_test",
		CreatedAt:      time.Now().UTC(),
	})

	bgPCM := media.GeneratePCM16WAV(16000, 1, 20000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	t.Run("MissingCASObjectForDeclaredVocals", func(t *testing.T) {
		stemArtifact := domain.AudioStemArtifacts{
			ID:            uuid.NewString(),
			SchemaVersion: domain.AudioStemsSchemaVersion,
			AssetID:       assetID,
			ProviderID:    "fake_separator",
			ModelName:     "uvr_mdx",
			ModelVersion:  "1.0",
			Stems: []domain.AudioStem{
				{
					Type:         domain.StemTypeBackground,
					AudioCASHash: bgObj.SHA256,
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
				{
					Type:         domain.StemTypeVocals,
					AudioCASHash: "non_existent_vocals_hash_12345",
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
			},
			CreatedAt: time.Now().UTC(),
		}
		sBytes, _ := json.Marshal(stemArtifact)
		sObj, _ := casStore.Put(bytes.NewReader(sBytes))
		_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
			ID:             stemArtifact.ID,
			AssetID:        assetID,
			ProviderID:     stemArtifact.ProviderID,
			ModelName:      stemArtifact.ModelName,
			ModelVersion:   stemArtifact.ModelVersion,
			CASHash:        sObj.SHA256,
			ProvenanceHash: "prov_stem_missing_vocal",
			CreatedAt:      time.Now().UTC(),
		})

		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil || !errors.Is(err, domain.ErrSoundtrackPreservationFailed) {
			t.Fatalf("expected ErrSoundtrackPreservationFailed on missing declared vocals CAS object, got: %v", err)
		}
	})

	t.Run("CorruptDeclaredVocalsWAV", func(t *testing.T) {
		corruptObj, _ := casStore.Put(bytes.NewReader([]byte("not a valid wav header")))
		stemArtifact := domain.AudioStemArtifacts{
			ID:            uuid.NewString(),
			SchemaVersion: domain.AudioStemsSchemaVersion,
			AssetID:       assetID,
			ProviderID:    "fake_separator",
			ModelName:     "uvr_mdx",
			ModelVersion:  "1.0",
			Stems: []domain.AudioStem{
				{
					Type:         domain.StemTypeBackground,
					AudioCASHash: bgObj.SHA256,
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
				{
					Type:         domain.StemTypeVocals,
					AudioCASHash: corruptObj.SHA256,
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
			},
			CreatedAt: time.Now().UTC(),
		}
		sBytes, _ := json.Marshal(stemArtifact)
		sObj, _ := casStore.Put(bytes.NewReader(sBytes))
		_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
			ID:             stemArtifact.ID,
			AssetID:        assetID,
			ProviderID:     stemArtifact.ProviderID,
			ModelName:      stemArtifact.ModelName,
			ModelVersion:   stemArtifact.ModelVersion,
			CASHash:        sObj.SHA256,
			ProvenanceHash: "prov_stem_corrupt_vocal",
			CreatedAt:      time.Now().UTC(),
		})

		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil || !errors.Is(err, domain.ErrSoundtrackPreservationFailed) {
			t.Fatalf("expected ErrSoundtrackPreservationFailed on corrupt declared vocals stem, got: %v", err)
		}
	})

	t.Run("ExplicitNoVocalsSucceeds", func(t *testing.T) {
		// Contract explicitly declares NO vocals stem (AudioCASHash: "")
		stemArtifact := domain.AudioStemArtifacts{
			ID:            uuid.NewString(),
			SchemaVersion: domain.AudioStemsSchemaVersion,
			AssetID:       assetID,
			ProviderID:    "fake_separator",
			ModelName:     "uvr_mdx",
			ModelVersion:  "1.0",
			Stems: []domain.AudioStem{
				{
					Type:         domain.StemTypeBackground,
					AudioCASHash: bgObj.SHA256,
					SampleRate:   16000,
					Channels:     1,
					Format:       "wav",
					DurationMs:   20000,
				},
			},
			CreatedAt: time.Now().UTC(),
		}
		sBytes, _ := json.Marshal(stemArtifact)
		sObj, _ := casStore.Put(bytes.NewReader(sBytes))
		_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
			ID:             stemArtifact.ID,
			AssetID:        assetID,
			ProviderID:     stemArtifact.ProviderID,
			ModelName:      stemArtifact.ModelName,
			ModelVersion:   stemArtifact.ModelVersion,
			CASHash:        sObj.SHA256,
			ProvenanceHash: "prov_stem_novocal_ok",
			CreatedAt:      time.Now().UTC(),
		})

		res, err := dubSvc.AuditionVoice(context.Background(), in)
		if err != nil {
			t.Fatalf("expected success with explicit no-vocals contract, got: %v", err)
		}
		if !res.ContextualMixed {
			t.Errorf("expected ContextualMixed=true")
		}
	})
}

func TestDubbingService_AuditionVoice_AudioRolePlan_LookupAndNoSpeechBehavior(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
		IsContextual: false,
		SampleText:   "Thử nghiệm mẫu giọng.",
	}

	t.Run("AuditionVoice_MissingAudioRolePlan_FailsClosed", func(t *testing.T) {
		assetNoPlan := uuid.NewString()
		_ = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
			ID:                  assetNoPlan,
			SHA256:              "sha_" + assetNoPlan,
			ByteSize:            1024,
			MimeType:            "video/mp4",
			RightsAttestationID: "att-" + assetID,
			CASPath:             "/mock.mp4",
			CreatedAt:           time.Now().UTC(),
		})

		inNoPlan := in
		inNoPlan.AssetID = assetNoPlan
		inNoPlan.RunID = ""
		_, err := dubSvc.AuditionVoice(context.Background(), inNoPlan)
		if err == nil || !errors.Is(err, domain.ErrAudioRolePlanRequired) {
			t.Fatalf("expected ErrAudioRolePlanRequired when plan is missing for asset, got: %v", err)
		}
	})

	t.Run("AuditionVoice_NoDubEligibleSpeech_ReturnsErrNoDubbingRequired", func(t *testing.T) {
		assetNoSpeech := uuid.NewString()
		_ = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
			ID:                  assetNoSpeech,
			SHA256:              "sha_" + assetNoSpeech,
			ByteSize:            1024,
			MimeType:            "video/mp4",
			RightsAttestationID: "att-" + assetID,
			CASPath:             "/mock.mp4",
			CreatedAt:           time.Now().UTC(),
		})
		_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
			ID:        uuid.NewString(),
			AssetID:   assetNoSpeech,
			CreatedAt: time.Now().UTC(),
			Segments: []domain.AudioSegment{
				{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
			},
		})

		inNoSpeech := in
		inNoSpeech.AssetID = assetNoSpeech
		inNoSpeech.RunID = ""
		_, err := dubSvc.AuditionVoice(context.Background(), inNoSpeech)
		if err == nil || !errors.Is(err, domain.ErrNoDubbingRequired) {
			t.Fatalf("expected ErrNoDubbingRequired when AudioRolePlan has no dub-eligible dialogue, got: %v", err)
		}
	})

	t.Run("AssignVoices_NoDubEligibleSpeech_ReturnsErrNoDubbingRequired", func(t *testing.T) {
		assetNoSpeech := uuid.NewString()
		_ = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
			ID:                  assetNoSpeech,
			SHA256:              "sha_" + assetNoSpeech,
			ByteSize:            1024,
			MimeType:            "video/mp4",
			RightsAttestationID: "att-" + assetID,
			CASPath:             "/mock.mp4",
			CreatedAt:           time.Now().UTC(),
		})
		_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
			ID:        uuid.NewString(),
			AssetID:   assetNoSpeech,
			CreatedAt: time.Now().UTC(),
			Segments: []domain.AudioSegment{
				{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
			},
		})

		runIDNoSpeech := uuid.NewString()
		service.PinAudioRolePlanForTest(context.Background(), db, casStore, runIDNoSpeech, domain.AudioRolePlan{
			ID:        uuid.NewString(),
			AssetID:   assetNoSpeech,
			CreatedAt: time.Now().UTC(),
			Segments: []domain.AudioSegment{
				{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
			},
		})

		assignIn := domain.VoiceAssignmentInput{
			RunID:          runIDNoSpeech,
			AssetID:        assetNoSpeech,
			TargetLanguage: "vi",
		}
		_, err := dubSvc.AssignVoices(context.Background(), assignIn)
		if err == nil || !errors.Is(err, domain.ErrNoDubbingRequired) {
			t.Fatalf("expected ErrNoDubbingRequired from AssignVoices on no-speech video, got: %v", err)
		}
	})

	t.Run("AssignVoices_MissingAudioRolePlan_FailsClosed", func(t *testing.T) {
		assetBare := uuid.NewString()
		_ = db.CreateSourceAsset(context.Background(), domain.SourceAsset{
			ID:                  assetBare,
			SHA256:              "sha_" + assetBare,
			ByteSize:            1024,
			MimeType:            "video/mp4",
			RightsAttestationID: "att-" + assetID,
			CASPath:             "/mock.mp4",
			CreatedAt:           time.Now().UTC(),
		})

		assignIn := domain.VoiceAssignmentInput{
			RunID:          uuid.NewString(),
			AssetID:        assetBare,
			TargetLanguage: "vi",
		}
		_, err := dubSvc.AssignVoices(context.Background(), assignIn)
		if err == nil {
			t.Fatalf("expected AssignVoices to fail closed when run-pinned audio role plan is missing, got nil")
		}
	})

	t.Run("AssignVoices_MissingRunID_ReturnsError", func(t *testing.T) {
		assignIn := domain.VoiceAssignmentInput{
			RunID:          "",
			AssetID:        assetID,
			TargetLanguage: "vi",
		}
		res, err := dubSvc.AssignVoices(context.Background(), assignIn)
		if err == nil || !strings.Contains(err.Error(), "run_id is required") {
			t.Fatalf("expected error containing 'run_id is required', got: %v", err)
		}
		if res != nil {
			t.Fatalf("expected nil result on missing run_id, got: %+v", res)
		}
	})
}
func TestDubbingService_AuditionVoice_Contextual_SlotOverrunAndMetadata(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	bgPCM := media.GeneratePCM16WAV(16000, 1, 30000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgPCM))
	stemArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				Format:       "wav",
				DurationMs:   30000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stemArtifact)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))
	_ = db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        assetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        sObj.SHA256,
		ProvenanceHash: "prov_stem_slot_test",
		CreatedAt:      time.Now().UTC(),
	})

	// Total audio is 30s. Dialogue suppression covers 0ms to 10000ms.
	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 10000, EndMs: 30000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	t.Run("SlotOverrun_FailsClosedEvenWhenSourceAndSuppressionPermit", func(t *testing.T) {
		// Segment slot is [2000, 4000]ms (2000ms slot duration)
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
					StartMs:        2000,
					EndMs:          4000,
					SlotDurationMs: 2000,
					SpokenText:     "Đoạn thoại dài hơn slot segment.",
				},
			},
		}
		dBytes, _ := json.Marshal(dubScript)
		dObj, _ := casStore.Put(bytes.NewReader(dBytes))
		_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
			ID:             dubScript.ID,
			AssetID:        assetID,
			RunID:          runID,
			TargetLanguage: "vi",
			CASHash:        dObj.SHA256,
			ProvenanceHash: "prov_dub_script_overrun",
			CreatedAt:      time.Now().UTC(),
		})

		// TTS synthesizes 2500ms speech -> interval [2000, 4500]ms.
		// Fits source (30000ms) and suppression (10000ms), but overruns slot end (4000ms).
		dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
			pcm := media.GeneratePCM16WAV(16000, 1, 2500)
			return &provider.TTSSynthesisResult{
				AudioData:          pcm,
				Format:             "wav",
				SampleRate:         16000,
				Channels:           1,
				MeasuredDurationMs: 2500,
				ProviderID:         p.ID(),
				ModelName:          "vieneu_tts_vi",
				ModelVersion:       "1.0",
			}, nil
		}

		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil || !errors.Is(err, domain.ErrTTSDurationOverrun) {
			t.Fatalf("expected ErrTTSDurationOverrun when speech overruns segment slot, got: %v", err)
		}
	})

	t.Run("InconsistentSlotDurationMs_FailsClosed", func(t *testing.T) {
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
					StartMs:        2000,
					EndMs:          4000,
					SlotDurationMs: 3000, // Inconsistent with 4000-2000=2000
					SpokenText:     "Inconsistent slot duration.",
				},
			},
		}
		dBytes, _ := json.Marshal(dubScript)
		dObj, _ := casStore.Put(bytes.NewReader(dBytes))
		_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
			ID:             dubScript.ID,
			AssetID:        assetID,
			RunID:          runID,
			TargetLanguage: "vi",
			CASHash:        dObj.SHA256,
			ProvenanceHash: "prov_dub_script_inconsistent",
			CreatedAt:      time.Now().UTC(),
		})

		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil {
			t.Fatalf("expected error on inconsistent slot duration, got nil")
		}
	})

	t.Run("InvalidSlotBounds_FailsClosed", func(t *testing.T) {
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
					StartMs:        4000,
					EndMs:          2000, // EndMs <= StartMs
					SlotDurationMs: 2000,
					SpokenText:     "Invalid slot bounds.",
				},
			},
		}
		dBytes, _ := json.Marshal(dubScript)
		dObj, _ := casStore.Put(bytes.NewReader(dBytes))
		_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
			ID:             dubScript.ID,
			AssetID:        assetID,
			RunID:          runID,
			TargetLanguage: "vi",
			CASHash:        dObj.SHA256,
			ProvenanceHash: "prov_dub_script_invalid_bounds",
			CreatedAt:      time.Now().UTC(),
		})

		_, err := dubSvc.AuditionVoice(context.Background(), in)
		if err == nil {
			t.Fatalf("expected error on invalid slot bounds (EndMs <= StartMs), got nil")
		}
	})

	t.Run("ExactSlotBoundaryFit_PassesAndReturnsMetadata", func(t *testing.T) {
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
					StartMs:        2000,
					EndMs:          4000,
					SlotDurationMs: 2000,
					SpokenText:     "Đoạn thoại khớp chính xác slot.",
				},
			},
		}
		dBytes, _ := json.Marshal(dubScript)
		dObj, _ := casStore.Put(bytes.NewReader(dBytes))
		_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
			ID:             dubScript.ID,
			AssetID:        assetID,
			RunID:          runID,
			TargetLanguage: "vi",
			CASHash:        dObj.SHA256,
			ProvenanceHash: "prov_dub_script_exact",
			CreatedAt:      time.Now().UTC(),
		})

		// TTS synthesizes exactly 2000ms speech -> interval [2000, 4000]ms (exact fit)
		dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
			pcm := media.GeneratePCM16WAV(16000, 1, 2000)
			return &provider.TTSSynthesisResult{
				AudioData:          pcm,
				Format:             "wav",
				SampleRate:         16000,
				Channels:           1,
				MeasuredDurationMs: 2000,
				ProviderID:         "fake_vieneu_tts_vi",
				ModelName:          "vieneu_tts_vi_model",
				ModelVersion:       "v2.1.0",
			}, nil
		}

		res, err := dubSvc.AuditionVoice(context.Background(), in)
		if err != nil {
			t.Fatalf("expected success on exact slot boundary fit, got: %v", err)
		}
		if !res.ContextualMixed {
			t.Errorf("expected ContextualMixed=true")
		}
		if res.ProviderID != "fake_vieneu_tts_vi" {
			t.Errorf("expected ProviderID 'fake_vieneu_tts_vi', got %q", res.ProviderID)
		}
		if res.ModelName != "vieneu_tts_vi_model" {
			t.Errorf("expected ModelName 'vieneu_tts_vi_model', got %q", res.ModelName)
		}
		if res.ModelVersion != "v2.1.0" {
			t.Errorf("expected ModelVersion 'v2.1.0', got %q", res.ModelVersion)
		}
	})
}

func TestDubbingService_SynthesizeAndFit_ProbesDurationAndEnforcesZeroOverrun(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")
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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")
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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID1 := uuid.NewString()
	runID2 := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID1, "vi")
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID2, "vi")
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

// TestDubbingService_AssignVoices_RefusesForgedLineageCAS pins the forged-lineage fix: a
// client-supplied dub-script/transcript CAS may never become frozen lineage unless the run
// itself resolves to it, or the artifact proves it belongs to this run/asset.
func TestDubbingService_AssignVoices_RefusesForgedLineageCAS(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()
	ctx := context.Background()

	assetA, runA := uuid.NewString(), uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetA, runA, "vi")
	scriptA := domain.DubScriptVariant{
		ID: uuid.NewString(), AssetID: assetA, TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{{
			Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1000, SlotDurationMs: 1000,
			SourceText: "第一句", MeaningText: "Câu một", SpokenText: "Câu một",
		}},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &scriptA, runA)

	assetB, runB := uuid.NewString(), uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetB, runB, "vi")
	scriptB := domain.DubScriptVariant{
		ID: uuid.NewString(), AssetID: assetB, TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{{
			Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1000, SlotDurationMs: 1000,
			SourceText: "第二句", MeaningText: "Câu hai", SpokenText: "Câu hai",
		}},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &scriptB, runB)

	idxB, err := db.GetDubScriptVariantIndexByRun(ctx, runB)
	if err != nil || idxB == nil || idxB.CASHash == "" {
		t.Fatalf("expected run B dub script index, got idx=%+v err=%v", idxB, err)
	}
	tIdxB, err := db.GetTranscriptArtifactIndexByRun(ctx, runB)
	if err != nil || tIdxB == nil || tIdxB.CASHash == "" {
		t.Fatalf("expected run B transcript index, got idx=%+v err=%v", tIdxB, err)
	}
	assertNoFrozenAssignment := func(stage string) {
		t.Helper()
		if idx, err := db.GetVoiceAssignmentIndexByRun(ctx, assetA, runA, "vi"); err == nil && idx != nil {
			t.Fatalf("%s: forged lineage produced a frozen voice assignment: %+v", stage, idx)
		}
	}

	// 1. Forged dub script CAS: run B's script offered to run A.
	_, err = dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runA, AssetID: assetA, TargetLanguage: "vi",
		DubScriptVariantCAS: idxB.CASHash,
	})
	if !errors.Is(err, domain.ErrTranslationOwnershipMismatch) {
		t.Fatalf("expected ErrTranslationOwnershipMismatch for forged dub script CAS, got: %v", err)
	}
	assertNoFrozenAssignment("forged dub script CAS")

	// 2. Forged transcript CAS: run B's transcript offered to run A. Ownership is decided by the
	// artifact itself before the run's pinned lineage is consulted, so a foreign asset's transcript
	// is refused as a foreign artifact (the same classification case 3 pins).
	_, err = dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runA, AssetID: assetA, TargetLanguage: "vi",
		TranscriptArtifactCAS: tIdxB.CASHash,
	})
	if !errors.Is(err, domain.ErrTranslationOwnershipMismatch) {
		t.Fatalf("expected ErrTranslationOwnershipMismatch for forged transcript CAS, got: %v", err)
	}
	assertNoFrozenAssignment("forged transcript CAS")

	// 2b. Same-asset forgery: an artifact that really is asset A's transcript, but not the one this
	// run pinned. Ownership proof passes, so the pinned-lineage equality is the guard that refuses it.
	forgedSameAsset := domain.TranscriptArtifact{
		ID:           uuid.NewString(),
		AssetID:      assetA,
		RunID:        runA,
		SpeechBlocks: []domain.SpeechBlock{{Index: 0, StartMs: 0, EndMs: 1000, SpeakerID: "SPEAKER_00", SourceText: "khác"}},
		CreatedAt:    time.Now().UTC(),
	}
	forgedBlob, err := json.Marshal(forgedSameAsset)
	if err != nil {
		t.Fatalf("marshal forged same-asset transcript: %v", err)
	}
	forgedObj, err := casStore.Put(bytes.NewReader(forgedBlob))
	if err != nil {
		t.Fatalf("put forged same-asset transcript: %v", err)
	}
	_, err = dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runA, AssetID: assetA, TargetLanguage: "vi",
		TranscriptArtifactCAS: forgedObj.SHA256,
	})
	if !errors.Is(err, domain.ErrTranscriptLineageMismatch) {
		t.Fatalf("expected ErrTranscriptLineageMismatch for a same-asset transcript the run never pinned, got: %v", err)
	}
	assertNoFrozenAssignment("same-asset forged transcript CAS")

	// 3. A run pinning no transcript of its own still refuses a foreign artifact by its own binding.
	runC := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetA, runC, "vi")
	_, err = dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runC, AssetID: assetA, TargetLanguage: "vi",
		TranscriptArtifactCAS: tIdxB.CASHash,
	})
	if !errors.Is(err, domain.ErrTranslationOwnershipMismatch) {
		t.Fatalf("expected ErrTranslationOwnershipMismatch for foreign transcript artifact, got: %v", err)
	}

	// 4. No regression: the run-bound lineage still succeeds and is frozen unchanged.
	idxA, err := db.GetDubScriptVariantIndexByRun(ctx, runA)
	if err != nil || idxA == nil || idxA.CASHash == "" {
		t.Fatalf("expected run A dub script index, got idx=%+v err=%v", idxA, err)
	}
	tIdxA, err := db.GetTranscriptArtifactIndexByRun(ctx, runA)
	if err != nil || tIdxA == nil || tIdxA.CASHash == "" {
		t.Fatalf("expected run A transcript index, got idx=%+v err=%v", tIdxA, err)
	}
	assignment, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runA, AssetID: assetA, TargetLanguage: "vi",
		DubScriptVariantCAS:   idxA.CASHash,
		TranscriptArtifactCAS: tIdxA.CASHash,
	})
	if err != nil {
		t.Fatalf("AssignVoices with the run-bound lineage failed: %v", err)
	}
	if assignment.DubScriptVariantCAS != idxA.CASHash || assignment.TranscriptArtifactCAS != tIdxA.CASHash {
		t.Fatalf("expected frozen lineage dub=%s transcript=%s, got dub=%s transcript=%s",
			idxA.CASHash, tIdxA.CASHash, assignment.DubScriptVariantCAS, assignment.TranscriptArtifactCAS)
	}
	frozen, err := db.GetVoiceAssignmentIndexByRun(ctx, assetA, runA, "vi")
	if err != nil || frozen == nil || frozen.CASHash != assignment.CASHash {
		t.Fatalf("expected persisted assignment index %s, got %+v err=%v", assignment.CASHash, frozen, err)
	}
}

func TestDubbingService_VoiceAssignment_SameRun_IdempotentEquivalence(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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

func TestDubbingService_SynthesizeAndFit_RegroupUsesApplicableGlossaryUnion(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")
	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), AssetID: assetID, TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 500, SlotDurationMs: 500, SpokenText: "Vế thứ nhất", MeaningText: "Vế thứ nhất", SourceText: "前半句", SourceGapAfterMs: 100, EstimatedDurationMs: 800},
			// Malformed downstream text deliberately drops the glossary target. The group
			// must use the union of terms from both canonical source members and refuse it.
			{Index: 1, SpeakerID: "SPEAKER_00", StartMs: 600, EndMs: 2500, SlotDurationMs: 1900, SpokenText: "vế thứ hai", MeaningText: "SuporVN vế thứ hai", SourceText: "SUPOR 后半句", SourceGapAfterMs: 100, EstimatedDurationMs: 800},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID, domain.GlossaryEntry{Source: "SUPOR", Target: "SuporVN"})
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))
	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{RunID: runID, AssetID: assetID, TargetLanguage: "vi"})
	if err != nil {
		t.Fatal(err)
	}

	variant, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID: assetID, RunID: runID, TargetLanguage: "vi", DubScriptVariantCAS: scriptCAS.SHA256, VoiceAssignmentCAS: assign.CASHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected glossary-damaging regroup to require review, got status=%s review=%d selected=%d", variant.OverallStatus, len(variant.ReviewSegments), len(variant.Segments))
	}
	if !strings.Contains(strings.ToLower(variant.ReviewSegments[0].ReviewReason), "supor") {
		t.Fatalf("expected review reason to name the protected glossary entity, got %q", variant.ReviewSegments[0].ReviewReason)
	}
}

func TestDubbingService_SynthesizeAndFit_Regroup_DifferentSpeakerOrNonEligibleGapNeverRegroups(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScriptDiffSpk, runID)
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
	pinDubbingScriptLineage(t, db, casStore, &dubScriptNonEligibleGap, runID)
	scriptBytesB, _ := json.Marshal(dubScriptNonEligibleGap)
	scriptCASB, _ := casStore.Put(bytes.NewReader(scriptBytesB))
	assignB, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices Case B failed: %v", err)
	}

	variantB, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCASB.SHA256,
		VoiceAssignmentCAS:  assignB.CASHash,
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

// Issue #153 / F2: A gap between adjacent same-speaker speech blocks that contains preserved
// soundtrack (singing/music-vocal or uncertain) must NEVER be regrouped across, even when the gap
// duration is < 600ms. Regrouping would swallow the soundtrack interval into dialogue and clash
// spoken audio with playing vocals.
func TestDubbingService_SynthesizeAndFit_Regroup_BlocksInternalSingingOrUncertainSoundtrack(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Pinned AudioRolePlan with singing in the gap [2000, 2300]
	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + assetID,
		AssetID:        assetID,
		ProviderID:     "test-role-provider",
		ModelName:      "test-role-model",
		ModelVersion:   "1",
		ProvenanceHash: "prov-role-" + assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 500, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 500, EndMs: 600, Role: domain.AudioRoleSingingMusicVocal},
			{StartMs: 600, EndMs: 2500, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	}
	roleBytes, _ := json.Marshal(rolePlan)
	roleObj, err := casStore.Put(bytes.NewReader(roleBytes))
	if err != nil {
		t.Fatal(err)
	}
	rolePlan.CASHash = roleObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, rolePlan); err != nil {
		t.Fatal(err)
	}
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "audio_role_plan",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: roleObj.SHA256,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})

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
				SourceGapAfterMs:    100, // 100ms gap, normally eligible because < 600ms, but contains singing!
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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		CASHash: scriptCAS.SHA256, ProvenanceHash: "prov-" + dubScript.ID, CreatedAt: dubScript.CreatedAt,
	})

	assign, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}

	variant, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit failed: %v", err)
	}

	// In TestDubbingService_SynthesizeAndFit_Regroup_SameSpeakerSuccess, these exact two segments
	// regrouped into 1 DubSegment covering [0, 1]. Here, because the gap [500, 600] contains singing,
	// regrouping across the gap is BLOCKED. They must remain separate and NOT be merged into a single segment!
	for _, seg := range variant.Segments {
		if len(seg.SpeechBlockIndices) > 1 {
			t.Fatalf("regroup incorrectly swallowed adjacent block across singing gap! SpeechBlockIndices: %v", seg.SpeechBlockIndices)
		}
	}
}
func TestDubbingService_NoSentenceBySentenceEngineHopping(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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

// TestDubbingService_SynthesizeAndFit_ReusesAcceptedCase2PlanOnSupersede pins the reuse gate on a
// superseding assignment: a prior plan the fit itself accepted (Case 2 — inside the accepted
// playback window while consuming part of the reserved natural gap) is evidence enough to reuse
// the segment. Re-deriving the window from UsableSlotMs/DurationDeltaMs would re-synthesize audio
// that is already accepted, so only the superseded speaker may reach TTS.
func TestDubbingService_SynthesizeAndFit_ReusesAcceptedCase2PlanOnSupersede(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()
	ctx := context.Background()

	assetID, runID := uuid.NewString(), uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Segment 0 (SPEAKER_00) ends at 1400ms with the next turn at 1560ms: the frozen 30% reserve
	// (min 50ms) makes the accepted window 1510ms wide while only 1460ms stay usable. Every fake
	// lane measures 1500ms, so segment 0 is an accepted Case-2 plan (DurationDeltaMs = +40).
	// Segment 1 (SPEAKER_01) is the last turn: its 1640ms window fits 1500ms outright (Case 1).
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1400, SlotDurationMs: 1400,
				SpokenText: "Vế một của người nói đầu tiên", SourceText: "第一句",
				SourceGapAfterMs: 160, EstimatedDurationMs: 1500,
			},
			{
				Index: 1, SpeakerID: "SPEAKER_01", StartMs: 1560, EndMs: 3200, SlotDurationMs: 1640,
				SpokenText: "Vế hai của người nói thứ hai", SourceText: "第二句",
				SourceGapAfterMs: 300, EstimatedDurationMs: 1500,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	base, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{RunID: runID, AssetID: assetID, TargetLanguage: "vi"})
	if err != nil {
		t.Fatalf("AssignVoices failed: %v", err)
	}
	for _, spk := range []string{"SPEAKER_00", "SPEAKER_01"} {
		if base.Assignments[spk].ID == "" {
			t.Fatalf("fixture needs both speakers assigned, got %+v", base.Assignments)
		}
	}

	pass1, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		DubScriptVariantCAS: scriptCAS.SHA256, VoiceAssignmentCAS: base.CASHash,
	})
	if err != nil {
		t.Fatalf("first synthesis pass failed: %v", err)
	}
	if len(pass1.Segments) != 2 {
		t.Fatalf("expected both speakers accepted in the first pass, got %d selected and %d for review",
			len(pass1.Segments), len(pass1.ReviewSegments))
	}
	var case2 *domain.DubbingFitPlan
	for i := range pass1.FitPlans {
		if pass1.FitPlans[i].SegmentIndex == 0 {
			case2 = &pass1.FitPlans[i]
		}
	}
	if case2 == nil || case2.Decision != domain.FitActionAccept || case2.DurationDeltaMs <= 0 || case2.MeasuredDurationMs <= case2.UsableSlotMs {
		t.Fatalf("fixture must produce an accepted Case-2 plan for segment 0, got %+v", case2)
	}

	// Supersede only SPEAKER_01, so SPEAKER_00's accepted segment stays reusable.
	superseding, err := dubSvc.ReassignVoice(ctx, domain.VoiceAssignmentInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{
			"SPEAKER_01": {ID: "vi_alt", ProviderID: "fake_zerotts_tts_vi", VoiceID: "vi_alt", Language: "vi"},
		},
	})
	if err != nil {
		t.Fatalf("ReassignVoice failed: %v", err)
	}
	invalidated := strings.Join(superseding.InvalidatedSpeakers, ",")
	if !strings.Contains(invalidated, "SPEAKER_01") || strings.Contains(invalidated, "SPEAKER_00") {
		t.Fatalf("expected only SPEAKER_01 invalidated, got %v", superseding.InvalidatedSpeakers)
	}

	ttsInvocations := func() int {
		total := 0
		for _, id := range []string{"fake_zerotts_tts_vi", "fake_vieneu_tts_vi"} {
			p, ok := reg.Get(id)
			if !ok {
				continue
			}
			if fake, ok := p.(*provider.FakeTTSProvider); ok {
				total += fake.Invocations
			}
		}
		return total
	}
	before := ttsInvocations()

	pass2, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		DubScriptVariantCAS: scriptCAS.SHA256, VoiceAssignmentCAS: superseding.CASHash,
	})
	if err != nil {
		t.Fatalf("second synthesis pass failed: %v", err)
	}

	if calls := ttsInvocations() - before; calls != 1 {
		t.Errorf("expected only the superseded speaker to reach TTS (1 call), got %d: an accepted Case-2 plan must be reused", calls)
	}
	var reused *domain.DubSegment
	for i := range pass2.Segments {
		if pass2.Segments[i].Index == 0 {
			reused = &pass2.Segments[i]
		}
	}
	if reused == nil || reused.AudioSHA256 != pass1.Segments[0].AudioSHA256 {
		t.Errorf("expected segment 0 to be reused unchanged, got %+v against %+v", reused, pass1.Segments[0])
	}
}

func TestDubbingService_SynthesizeAndFit_Regroup_ThreeBlocksSuccess(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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

// TestDubbingService_SynthesizeAndFit_Regroup_QAFailBeforeSynthesisDropsStaleEvidence pins the
// regroup review-candidate leak: blocks {0,1} synthesize and overrun, then block 2 is pulled into
// the group and fails meaning QA before any synthesis of it. The surfaced review candidate covers
// the whole consumed group, so it must carry that group's own (absent) audio and measured duration
// instead of group {0,1}'s synthesized evidence.
func TestDubbingService_SynthesizeAndFit_Regroup_QAFailBeforeSynthesisDropsStaleEvidence(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Same-speaker chain with 100ms gaps: seg 0 (slot 500) overruns, seg 1 extends the group to
	// a 1100ms slot and still overruns, seg 2 extends it again but its source number 2026 has no
	// counterpart in its spoken text, so the combined group fails QA before synthesis.
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 500, SlotDurationMs: 500,
				SpokenText: "Vế một", SourceText: "第一句",
				SourceGapAfterMs: 100, EstimatedDurationMs: 1200,
			},
			{
				Index: 1, SpeakerID: "SPEAKER_00", StartMs: 600, EndMs: 1100, SlotDurationMs: 500,
				SpokenText: "vế hai", SourceText: "第二句",
				SourceGapAfterMs: 100, EstimatedDurationMs: 1200,
			},
			{
				Index: 2, SpeakerID: "SPEAKER_00", StartMs: 1200, EndMs: 2000, SlotDurationMs: 800,
				SpokenText: "và vế ba", SourceText: "第三句 2026。",
				SourceGapAfterMs: 100, EstimatedDurationMs: 800,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
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

	if len(variant.Segments) != 0 {
		t.Fatalf("expected 0 selected Segments for an unresolved regroup, got %d", len(variant.Segments))
	}
	if len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected exactly 1 ReviewSegment for the consumed regroup, got %d", len(variant.ReviewSegments))
	}
	revSeg := variant.ReviewSegments[0]
	if len(revSeg.SpeechBlockIndices) != 3 || revSeg.SpeechBlockIndices[2] != 2 {
		t.Fatalf("expected the review candidate to cover the whole consumed group [0 1 2], got %v", revSeg.SpeechBlockIndices)
	}
	if !strings.Contains(revSeg.ReviewReason, "in source missing from target") {
		t.Fatalf("expected a pre-synthesis QA review reason for the extended group, got %q", revSeg.ReviewReason)
	}
	if revSeg.AudioSHA256 != "" || revSeg.AudioCASPath != "" {
		t.Errorf("review candidate carries the narrower group's stale audio: path=%q sha=%q", revSeg.AudioCASPath, revSeg.AudioSHA256)
	}
	if revSeg.MeasuredDurationMs != 0 {
		t.Errorf("review candidate carries the narrower group's stale measured duration: %dms", revSeg.MeasuredDurationMs)
	}

	var regroupPlan *domain.DubbingFitPlan
	for i := range variant.FitPlans {
		if len(variant.FitPlans[i].SpeechBlockIndices) == 3 {
			regroupPlan = &variant.FitPlans[i]
		}
	}
	if regroupPlan == nil {
		t.Fatal("expected a fit plan for the consumed 3-block regroup group")
	}
	if regroupPlan.MeasuredDurationMs != 0 {
		t.Errorf("regroup fit plan carries the narrower group's stale measured duration: %dms", regroupPlan.MeasuredDurationMs)
	}
}

func TestDubbingService_SynthesizeAndFit_MissingAudioRolePlan_FailsClosed(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()

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
	_ = db.CreateJob(context.Background(), domain.LocalizationJob{
		ID:             "job-" + assetID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})
	_, _ = db.CreateRunEnqueued(context.Background(), domain.LocalizationRun{
		ID:                 runID,
		JobID:              "job-" + assetID,
		Status:             "running",
		ConfigSnapshotJSON: "{}",
		CreatedAt:          time.Now().UTC(),
	}, "job-"+assetID)

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          2000,
				SlotDurationMs: 2000,
				SpokenText:     "Câu thoại kiểm tra.",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	// Deliberately do NOT save AudioRolePlan. SynthesizeAndFit must fail closed.
	_, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
	})
	if err == nil {
		t.Fatal("expected error due to missing AudioRolePlan, got nil")
	}
	if !errors.Is(err, domain.ErrAudioRolePlanRequired) {
		t.Errorf("expected ErrAudioRolePlanRequired, got %v", err)
	}
}

func TestDubbingService_SynthesizeAndFit_NoDubPlan_ReturnsErrNoDubbingRequired(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()

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
	_ = db.CreateJob(context.Background(), domain.LocalizationJob{
		ID:             "job-" + assetID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})
	_, _ = db.CreateRunEnqueued(context.Background(), domain.LocalizationRun{
		ID:                 runID,
		JobID:              "job-" + assetID,
		Status:             "running",
		ConfigSnapshotJSON: "{}",
		CreatedAt:          time.Now().UTC(),
	}, "job-"+assetID)

	// Save AudioRolePlan with 0 dialogue segments (Instrumental BGM only)
	noDubPlan := domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	service.PinAudioRolePlanForTest(context.Background(), db, casStore, runID, noDubPlan)

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          2000,
				SlotDurationMs: 2000,
				SpokenText:     "Câu thoại kiểm tra.",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	_, err := dubSvc.SynthesizeAndFit(context.Background(), domain.DubbingJobInput{
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
	})
	if err == nil {
		t.Fatal("expected ErrNoDubbingRequired on no-dub AudioRolePlan, got nil")
	}
	if !errors.Is(err, domain.ErrNoDubbingRequired) {
		t.Errorf("expected ErrNoDubbingRequired, got %v", err)
	}
}

func TestDubbingService_SynthesizeAndFit_ZeroDurationSlot_FlaggedForReview_NeverReachesMixer(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// Setup DubScriptVariant with 1 zero-duration slot (start_ms == end_ms == 29680)
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:          12,
				SourceText:     "啊",
				MeaningText:    "A",
				SpokenText:     "A",
				SpeakerID:      "SPEAKER_00",
				StartMs:        29680,
				EndMs:          29680,
				SlotDurationMs: 0,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptData, _ := json.Marshal(dubScript)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptData))
	dubScript.CASHash = scriptObj.SHA256
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        scriptObj.SHA256,
		ProvenanceHash: "prov_script_zero_slot",
		CreatedAt:      dubScript.CreatedAt,
	})

	voiceAssign, _ := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})

	// Configure fake provider with measured duration 640ms (like Kokoro/VieNeu synthesized audio)
	fakeTTS, _ := reg.Get("fake_vieneu_tts_vi")
	fakeProv := fakeTTS.(*provider.FakeTTSProvider)
	fakeProv.DurationMs = 640

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

	// Invariant: degenerate 0ms slot cannot fit synthesized audio; MUST NOT reach mixer selected segments!
	if len(dubSegments.Segments) != 0 {
		t.Errorf("zero-duration candidate MUST NOT be selected into Segments (mixer input), got %d segments", len(dubSegments.Segments))
	}
	if len(dubSegments.ReviewSegments) != 1 {
		t.Fatalf("expected 1 review segment for zero-duration slot, got %d", len(dubSegments.ReviewSegments))
	}
	if dubSegments.OverallStatus != "REVIEW_REQUIRED" {
		t.Errorf("expected OverallStatus REVIEW_REQUIRED, got %s", dubSegments.OverallStatus)
	}
}

// seedAuditionDubScriptVariant seeds a single-segment [0ms,4000ms] DubScriptVariant for
// (assetID, runID, lang) carrying spokenText, indexed at createdAt under provenance.
func seedAuditionDubScriptVariant(t *testing.T, db *storage.DB, casStore *cas.Store, assetID, runID, lang, spokenText, provenance string, createdAt time.Time) {
	t.Helper()
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: lang,
		Segments: []domain.DubScriptSegment{
			{
				Index:          0,
				SpeakerID:      "SPEAKER_00",
				StartMs:        0,
				EndMs:          4000,
				SlotDurationMs: 4000,
				SpokenText:     spokenText,
			},
		},
		CreatedAt: createdAt,
	}
	payload, err := json.Marshal(dubScript)
	if err != nil {
		t.Fatalf("marshal dub script variant: %v", err)
	}
	obj, err := casStore.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put dub script variant to CAS: %v", err)
	}
	if err := db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: lang,
		CASHash:        obj.SHA256,
		ProvenanceHash: provenance,
		CreatedAt:      createdAt,
	}); err != nil {
		t.Fatalf("save dub script variant index: %v", err)
	}
}

// seedAuditionStems seeds asset-scoped background + vocals stems of durationMs so a
// contextual audition can mix without failing the source-derived stem requirement.
func seedAuditionStems(t *testing.T, db *storage.DB, casStore *cas.Store, assetID string, durationMs int64) {
	t.Helper()
	bgObj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, durationMs)))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}
	vocalsObj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, durationMs)))
	if err != nil {
		t.Fatalf("put vocals stem: %v", err)
	}
	artifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		ProviderID:    "fake_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "1.0",
		Stems: []domain.AudioStem{
			{Type: domain.StemTypeBackground, AudioCASHash: bgObj.SHA256, SampleRate: 16000, Channels: 1, Format: "wav", DurationMs: durationMs},
			{Type: domain.StemTypeVocals, AudioCASHash: vocalsObj.SHA256, SampleRate: 16000, Channels: 1, Format: "wav", DurationMs: durationMs},
		},
		CreatedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal stems artifact: %v", err)
	}
	obj, err := casStore.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put stems artifact to CAS: %v", err)
	}
	if err := db.SaveAudioStemsArtifactIndex(context.Background(), storage.AudioStemsArtifactIndex{
		ID:             artifact.ID,
		AssetID:        assetID,
		ProviderID:     artifact.ProviderID,
		ModelName:      artifact.ModelName,
		ModelVersion:   artifact.ModelVersion,
		CASHash:        obj.SHA256,
		ProvenanceHash: "prov_stems_" + assetID,
		CreatedAt:      artifact.CreatedAt,
	}); err != nil {
		t.Fatalf("save stems artifact index: %v", err)
	}
}

// TestDubbingService_AuditionVoice_ContextualRunScopedDubScriptSelection is the cross-run
// bleed guard: a run-pinned contextual audition must resolve the DubScriptVariant belonging
// to VoiceAuditionInput.RunID, not the asset/language asset-latest variant. Run B is seeded
// strictly newer at the same segment position with distinct spoken text, so an asset-latest
// lookup would synthesize run B's text for a run A request.
func TestDubbingService_AuditionVoice_ContextualRunScopedDubScriptSelection(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runAID := uuid.NewString()
	runBID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runAID, "vi")

	seedAuditionDubScriptVariant(t, db, casStore, assetID, runAID, "vi", "run A gốc.", "prov_run_a", time.Now().UTC().Add(-2*time.Hour))
	seedAuditionDubScriptVariant(t, db, casStore, assetID, runBID, "vi", "run B mới nhất.", "prov_run_b", time.Now().UTC().Add(-1*time.Hour))
	seedAuditionStems(t, db, casStore, assetID, 20000)

	in := domain.VoiceAuditionInput{
		RunID:          runAID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Voice: domain.VoiceProfile{
			ID:         "vieneu_vi_female_1",
			ProviderID: "fake_vieneu_tts_vi",
			VoiceID:    "vi_f1",
			Name:       "VieNeu Nữ",
			Language:   "vi",
		},
		IsContextual: true,
		SegmentIndex: 0,
	}

	res, err := dubSvc.AuditionVoice(context.Background(), in)
	if err != nil {
		t.Fatalf("AuditionVoice failed: %v", err)
	}
	if res.SampleText != "run A gốc." {
		t.Errorf("run-pinned audition resolved the wrong run's dub script: got %q, want %q", res.SampleText, "run A gốc.")
	}
}

// TestDubbingService_AuditionVoice_ContextualRunScopedMismatchFailsClosed proves a run-pinned
// audition rejects a run dub script whose target language does not match the request, rather
// than silently consuming it (or conflating it with a missing variant).
func TestDubbingService_AuditionVoice_ContextualRunScopedMismatchFailsClosed(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	// The run's dub script exists, but for target language "en"; the request asks "vi".
	seedAuditionDubScriptVariant(t, db, casStore, assetID, runID, "en", "run A English.", "prov_run_mismatch", time.Now().UTC())
	seedAuditionStems(t, db, casStore, assetID, 20000)

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
		IsContextual: true,
		SegmentIndex: 0,
	}

	_, err := dubSvc.AuditionVoice(context.Background(), in)
	if err == nil {
		t.Fatalf("expected fail-closed when the run dub script target language mismatches the request")
	}
	if errors.Is(err, domain.ErrDubScriptVariantNotFound) {
		t.Fatalf("expected an explicit run binding mismatch, got dub script not found: %v", err)
	}
}

func TestDubbingService_AuditionVoice_ContextualRequiresRunAndUsesPlaybackAllowance(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")
	seedAuditionStems(t, db, casStore, assetID, 5000)

	// Source speech ends at 2000ms, followed by a genuine 1000ms vocal gap.
	plan := domain.AudioRolePlan{
		ID: "plan-playback-" + assetID, AssetID: assetID, ProviderID: "test-role-provider",
		ModelName: "test-role-model", ModelVersion: "1", ProvenanceHash: "prov-playback-" + assetID,
		CreatedAt: time.Now().UTC(), Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2000, EndMs: 3000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 3000, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planBytes, _ := json.Marshal(plan)
	planObj, err := casStore.Put(bytes.NewReader(planBytes))
	if err != nil {
		t.Fatal(err)
	}
	plan.CASHash = planObj.SHA256
	if err := db.SaveAudioRolePlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	transcript := domain.TranscriptArtifact{
		ID: "transcript-" + runID, AssetID: assetID, RunID: runID, CreatedAt: time.Now().UTC(),
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, SegmentType: domain.SpeechBlockTypeSpeech, SourceText: "一", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000},
			{Index: 1, SegmentType: domain.SpeechBlockTypeSpeech, SourceText: "二", SpeakerID: "SPEAKER_00", StartMs: 3000, EndMs: 4000},
		},
	}
	transcriptBytes, _ := json.Marshal(transcript)
	transcriptObj, err := casStore.Put(bytes.NewReader(transcriptBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTranscriptArtifactIndex(context.Background(), storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: transcriptObj.SHA256,
		ProvenanceHash: "prov-" + transcript.ID, CreatedAt: transcript.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}

	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", CreatedAt: time.Now().UTC(),
		Segments: []domain.DubScriptSegment{{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SlotDurationMs: 2000, SpokenText: "đoạn thoại"}},
	}
	dubBytes, _ := json.Marshal(dubScript)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dubObj.SHA256,
		ProvenanceHash: "prov-" + dubScript.ID, CreatedAt: dubScript.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}

	voice := domain.VoiceProfile{ID: "vieneu_vi_female_1", ProviderID: "fake_vieneu_tts_vi", VoiceID: "vi_f1", Name: "VieNeu Nữ", Language: "vi"}
	if _, err := dubSvc.AuditionVoice(context.Background(), domain.VoiceAuditionInput{AssetID: assetID, TargetLanguage: "vi", Voice: voice, IsContextual: true}); err == nil || !strings.Contains(err.Error(), "run_id is required") {
		t.Fatalf("expected missing run_id to fail closed, got %v", err)
	}

	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		pcm := media.GeneratePCM16WAV(16000, 1, 2500)
		return &provider.TTSSynthesisResult{AudioData: pcm, Format: "wav", SampleRate: 16000, Channels: 1, MeasuredDurationMs: 2500, ProviderID: p.ID(), ModelName: "fake", ModelVersion: "1"}, nil
	}
	res, err := dubSvc.AuditionVoice(context.Background(), domain.VoiceAuditionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", Voice: voice, IsContextual: true, SegmentIndex: 0,
	})
	if err != nil {
		t.Fatalf("expected 2500ms audition to fit the measured borrowed playback window: %v", err)
	}
	if !res.ContextualMixed {
		t.Fatal("expected contextual audition mix")
	}
}

func TestDubbingService_AuditionVoice_ContextualSparseSegmentIndices(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")
	seedAuditionStems(t, db, casStore, assetID, 15000)

	// Sparse canonical indices: 0, 2, 4, 6
	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1500, SlotDurationMs: 1500, SpokenText: "Segment 0 text"},
			{Index: 2, SpeakerID: "SPEAKER_00", StartMs: 2000, EndMs: 3500, SlotDurationMs: 1500, SpokenText: "Segment 2 text"},
			{Index: 4, SpeakerID: "SPEAKER_00", StartMs: 4000, EndMs: 5500, SlotDurationMs: 1500, SpokenText: "Segment 4 text"},
			{Index: 6, SpeakerID: "SPEAKER_00", StartMs: 6000, EndMs: 7500, SlotDurationMs: 1500, SpokenText: "Segment 6 text"},
		},
	}
	dubBytes, _ := json.Marshal(dubScript)
	dubObj, err := casStore.Put(bytes.NewReader(dubBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dubObj.SHA256,
		ProvenanceHash: "prov-" + dubScript.ID, CreatedAt: dubScript.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}

	var synthesizedText string
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		synthesizedText = req.Text
		pcm := media.GeneratePCM16WAV(16000, 1, 1500)
		return &provider.TTSSynthesisResult{AudioData: pcm, Format: "wav", SampleRate: 16000, Channels: 1, MeasuredDurationMs: 1500, ProviderID: p.ID(), ModelName: "fake", ModelVersion: "1"}, nil
	}

	voice := domain.VoiceProfile{ID: "vieneu_vi_female_1", ProviderID: "fake_vieneu_tts_vi", VoiceID: "vi_f1", Name: "VieNeu Nữ", Language: "vi"}

	// 1. Audition with Index 6 (sparse index past length 4)
	res6, err := dubSvc.AuditionVoice(context.Background(), domain.VoiceAuditionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", Voice: voice, IsContextual: true, SegmentIndex: 6,
	})
	if err != nil {
		t.Fatalf("expected successful contextual audition for sparse canonical index 6, got: %v", err)
	}
	if !res6.ContextualMixed {
		t.Errorf("expected ContextualMixed=true for index 6")
	}
	if synthesizedText != "Segment 6 text" {
		t.Errorf("expected synthesized text 'Segment 6 text', got %q", synthesizedText)
	}

	// 2. Audition with non-existent index (e.g. 3) must fail closed with ErrDubScriptVariantNotFound
	_, errMissing := dubSvc.AuditionVoice(context.Background(), domain.VoiceAuditionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", Voice: voice, IsContextual: true, SegmentIndex: 3,
	})
	if errMissing == nil || !errors.Is(errMissing, domain.ErrDubScriptVariantNotFound) {
		t.Fatalf("expected ErrDubScriptVariantNotFound for missing segment index 3, got: %v", errMissing)
	}

	// 3. Audition with Index 2 must select segment with Index == 2, NOT dubScript.Segments[2] (which has Index 4)
	res2, err := dubSvc.AuditionVoice(context.Background(), domain.VoiceAuditionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", Voice: voice, IsContextual: true, SegmentIndex: 2,
	})
	if err != nil {
		t.Fatalf("expected successful contextual audition for sparse canonical index 2, got: %v", err)
	}
	if !res2.ContextualMixed {
		t.Errorf("expected ContextualMixed=true for index 2")
	}
	if synthesizedText != "Segment 2 text" {
		t.Errorf("expected synthesized text 'Segment 2 text' (Index 2), got %q", synthesizedText)
	}
}

func TestDubbingService_RunScopedAudioRolePlan_NeverAdoptsNewerNoDubPlan(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()
	ctx := context.Background()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")
	seedAuditionStems(t, db, casStore, assetID, 10000)

	// Pinned AudioRolePlan A with dub-eligible dialogue
	planA := domain.AudioRolePlan{
		ID: "planA-" + assetID, AssetID: assetID, ProviderID: "test-role-provider",
		ModelName: "test-role-model", ModelVersion: "1", ProvenanceHash: "prov-planA-" + assetID,
		CreatedAt: time.Now().UTC(), Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	planABytes, _ := json.Marshal(planA)
	planAObj, err := casStore.Put(bytes.NewReader(planABytes))
	if err != nil {
		t.Fatal(err)
	}
	planA.CASHash = planAObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, planA); err != nil {
		t.Fatal(err)
	}
	// Pin Plan A to Run A via stage_executions
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "audio_role_plan",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: planAObj.SHA256,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", CreatedAt: time.Now().UTC(),
		Segments: []domain.DubScriptSegment{{
			Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 3000, SlotDurationMs: 3000,
			SourceText: "测试", MeaningText: "thử nghiệm", SpokenText: "thử nghiệm", PassedQAGate: true,
		}},
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptData, _ := json.Marshal(dubScript)
	scriptObj, _ := casStore.Put(bytes.NewReader(scriptData))
	dubScript.CASHash = scriptObj.SHA256
	_ = db.SaveDubScriptVariantIndex(context.Background(), storage.DubScriptVariantIndex{
		ID:             dubScript.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        scriptObj.SHA256,
		ProvenanceHash: "prov-" + dubScript.ID,
		CreatedAt:      dubScript.CreatedAt,
	})
	// Now save a NEW AudioRolePlan B for the same asset with NO dub-eligible dialogue!
	planB := domain.AudioRolePlan{
		ID: "planB-" + assetID, AssetID: assetID, ProviderID: "test-role-provider",
		ModelName: "test-role-model", ModelVersion: "1", ProvenanceHash: "prov-planB-" + assetID,
		CreatedAt: time.Now().UTC().Add(time.Minute), Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
		},
	}
	planBBytes, _ := json.Marshal(planB)
	planBObj, _ := casStore.Put(bytes.NewReader(planBBytes))
	planB.CASHash = planBObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, planB); err != nil {
		t.Fatal(err)
	}

	// Verify DB now returns Plan B for the asset
	latestPlan, _ := db.GetAudioRolePlan(ctx, assetID)
	if domain.IsDubEligible(latestPlan) {
		t.Fatal("expected latestPlan to have no dub-eligible dialogue")
	}

	voice := domain.VoiceProfile{ID: "vieneu_vi_female_1", ProviderID: "fake_vieneu_tts_vi", VoiceID: "vi_f1", Name: "VieNeu Nữ", Language: "vi"}

	// 1. AssignVoices for Run A must succeed and NOT adopt Plan B (which would return ErrNoDubbingRequired)
	va, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi",
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": voice},
	})
	if err != nil {
		t.Fatalf("expected AssignVoices to succeed using Run A's pinned Plan A, got: %v", err)
	}
	if va == nil {
		t.Fatal("expected non-nil VoiceAssignment")
	}

	// 2. AuditionVoice for Run A must succeed and NOT return ErrNoDubbingRequired
	dubSvc.TTSInvoke = func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		pcm := media.GeneratePCM16WAV(16000, 1, 3000)
		return &provider.TTSSynthesisResult{AudioData: pcm, Format: "wav", SampleRate: 16000, Channels: 1, MeasuredDurationMs: 3000, ProviderID: p.ID(), ModelName: "fake", ModelVersion: "1"}, nil
	}
	res, err := dubSvc.AuditionVoice(ctx, domain.VoiceAuditionInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi", Voice: voice, IsContextual: true, SegmentIndex: 0,
	})
	if err != nil {
		t.Fatalf("expected AuditionVoice to succeed using Run A's pinned Plan A, got: %v", err)
	}
	if !res.ContextualMixed {
		t.Fatal("expected ContextualMixed=true")
	}

	// 3. SynthesizeAndFit for Run A must succeed and NOT return ErrNoDubbingRequired
	dubSegs, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi",
		DubScriptVariantCAS:   dubScript.CASHash,
		VoiceAssignmentCAS:    va.CASHash,
		TranscriptArtifactCAS: va.TranscriptArtifactCAS,
	})
	if err != nil {
		t.Fatalf("expected SynthesizeAndFit to succeed using Run A's pinned Plan A, got: %v", err)
	}
	if dubSegs.AudioRolePlanCAS != planAObj.SHA256 {
		t.Errorf("expected DubSegmentsVariant.AudioRolePlanCAS to be %s (Plan A), got %s", planAObj.SHA256, dubSegs.AudioRolePlanCAS)
	}

	// 4. CanReuseVariant for Run A must return true despite newer Plan B on the asset
	inputReuse := domain.DubbingJobInput{
		RunID: runID, AssetID: assetID, TargetLanguage: "vi",
		DubScriptVariantCAS:   dubScript.CASHash,
		VoiceAssignmentCAS:    va.CASHash,
		TranscriptArtifactCAS: va.TranscriptArtifactCAS,
		AudioRolePlanCAS:      planAObj.SHA256,
	}
	reusable := dubSvc.CanReuseVariant(ctx, inputReuse, dubSegs)
	if !reusable {
		t.Errorf("expected CanReuseVariant to return true for Run A despite newer Plan B on asset")
	}
}

// An omitted DubScriptVariantCAS resolves from the current run's own evidence only: never from the
// asset's latest dub-script row, which can belong to another run, and a run that pins no script
// fails closed instead of adopting someone else's text (#153).
func TestDubbingService_SynthesizeAndFit_OmittedDubScriptCASStaysRunScoped(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()
	ctx := context.Background()

	const assetID = "asset-omitted-script"
	const jobID = "job-" + assetID
	runA := "run-omitted-a"
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runA, "vi")
	plan, err := db.GetAudioRolePlan(ctx, assetID)
	if err != nil {
		t.Fatalf("get audio role plan: %v", err)
	}

	createRunWithRolePlanPin := func(runID string) {
		t.Helper()
		if _, err := db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID: runID, JobID: jobID, Status: "running", ConfigSnapshotJSON: "{}", CreatedAt: time.Now().UTC(),
		}, jobID); err != nil {
			t.Fatalf("create run %s: %v", runID, err)
		}
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID: uuid.NewString(), RunID: runID, Stage: "audio_role_plan", Status: domain.StageStatusSucceeded,
			ArtifactSHA256: plan.CASHash, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("pin role plan for run %s: %v", runID, err)
		}
	}

	// Run A's transcript: both the playback-window derivation and the frozen voice assignment bind
	// to it.
	transcript := domain.TranscriptArtifact{
		ID: "transcript-" + runA, AssetID: assetID, RunID: runA, SourceLanguage: "zh",
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 2000, SpeakerID: "SPEAKER_00", SourceText: "今天天气很好。", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		ProvenanceHash: "prov-transcript-" + runA, CreatedAt: time.Now().UTC(),
	}
	transcriptBytes, _ := json.Marshal(transcript)
	transcriptObj, err := casStore.Put(bytes.NewReader(transcriptBytes))
	if err != nil {
		t.Fatalf("put transcript: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runA, CASHash: transcriptObj.SHA256,
		ProvenanceHash: transcript.ProvenanceHash, CreatedAt: transcript.CreatedAt,
	}); err != nil {
		t.Fatalf("save run A transcript index: %v", err)
	}

	translationA := domain.TranslationVariant{
		ID: "translation-" + runA, SchemaVersion: domain.TranslationSchemaVersion, ContractID: service.TranslationContractID,
		AssetID: assetID, RunID: runA, SourceLanguage: "zh", TargetLanguage: "vi",
		ProvenanceHash: "prov-translation-" + runA, CreatedAt: time.Now().UTC(),
		Segments: []domain.TranslationSegment{{
			Index: 0, SourceText: "今天天气很好。", TargetText: "Hôm nay thời tiết rất tốt.",
			SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000,
		}},
	}
	translationABytes, _ := json.Marshal(translationA)
	translationAObj, err := casStore.Put(bytes.NewReader(translationABytes))
	if err != nil {
		t.Fatalf("put run A translation variant: %v", err)
	}

	scriptA := domain.DubScriptVariant{
		ID: "dub-script-a", SchemaVersion: domain.DubScriptSchemaVersion,
		AssetID: assetID, RunID: runA, JobID: jobID, TargetLanguage: "vi", SourceLanguage: "zh",
		TranslationVariantCAS: translationAObj.SHA256,
		ProvenanceHash:        "prov-dub-script-a", CreatedAt: time.Now().UTC(),
		Segments: []domain.DubScriptSegment{{
			Index: 0, SpeakerID: "SPEAKER_00", SourceText: "今天天气很好。",
			MeaningText: "Hôm nay thời tiết rất tốt.", SpokenText: "Hôm nay thời tiết tốt.",
			StartMs: 0, EndMs: 2000, SlotDurationMs: 2000,
		}},
	}
	scriptABytes, _ := json.Marshal(scriptA)
	scriptAObj, err := casStore.Put(bytes.NewReader(scriptABytes))
	if err != nil {
		t.Fatalf("put run A dub script: %v", err)
	}
	scriptA.CASHash = scriptAObj.SHA256
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: scriptA.ID, AssetID: assetID, RunID: runA, JobID: jobID, TargetLanguage: "vi",
		CASHash: scriptA.CASHash, ProvenanceHash: scriptA.ProvenanceHash, CreatedAt: scriptA.CreatedAt,
	}); err != nil {
		t.Fatalf("save run A dub script index: %v", err)
	}

	assignmentA, err := dubSvc.AssignVoices(ctx, domain.VoiceAssignmentInput{
		RunID: runA, AssetID: assetID, TargetLanguage: "vi", DubScriptVariantCAS: scriptA.CASHash,
	})
	if err != nil {
		t.Fatalf("AssignVoices for run A: %v", err)
	}

	fakeTTS, ok := reg.Get("fake_vieneu_tts_vi")
	if !ok {
		t.Fatalf("fake_vieneu_tts_vi not in registry")
	}
	fakeTTS.(*provider.FakeTTSProvider).DurationMs = 1500

	// A newer script written for a different run becomes the asset's latest dub-script row: exactly
	// the state an asset-scoped lookup would hand to run A's synthesis.
	runB := "run-omitted-b"
	createRunWithRolePlanPin(runB)
	scriptB := domain.DubScriptVariant{
		ID: "dub-script-b", SchemaVersion: domain.DubScriptSchemaVersion,
		AssetID: assetID, RunID: runB, JobID: jobID, TargetLanguage: "vi", SourceLanguage: "zh",
		ProvenanceHash: "prov-dub-script-b", CreatedAt: scriptA.CreatedAt.Add(time.Minute),
		Segments: []domain.DubScriptSegment{{
			Index: 0, SpeakerID: "SPEAKER_00", SourceText: "今天天气很好。",
			MeaningText: "Trời hôm nay đẹp.", SpokenText: "Hôm nay trời đẹp.",
			StartMs: 0, EndMs: 2000, SlotDurationMs: 2000,
		}},
	}
	scriptBBytes, _ := json.Marshal(scriptB)
	scriptBObj, err := casStore.Put(bytes.NewReader(scriptBBytes))
	if err != nil {
		t.Fatalf("put run B dub script: %v", err)
	}
	scriptB.CASHash = scriptBObj.SHA256
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: scriptB.ID, AssetID: assetID, RunID: runB, JobID: jobID, TargetLanguage: "vi",
		CASHash: scriptB.CASHash, ProvenanceHash: scriptB.ProvenanceHash, CreatedAt: scriptB.CreatedAt,
	}); err != nil {
		t.Fatalf("save run B dub script index: %v", err)
	}

	// 1. Run A owns run-scoped script evidence, so an omitted CAS resolves to it - never to the
	// newer asset-latest row of run B.
	segmentsA, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID: runA, JobID: jobID, AssetID: assetID, TargetLanguage: "vi",
		VoiceAssignmentCAS: assignmentA.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit for run A: %v", err)
	}
	if segmentsA.DubScriptVariantCAS != scriptA.CASHash {
		t.Fatalf("omitted dub script CAS resolved %s, want run A's own %s",
			segmentsA.DubScriptVariantCAS, scriptA.CASHash)
	}

	// 2. A run with no script evidence at all fails closed rather than adopting another run's script.
	runC := "run-omitted-c"
	createRunWithRolePlanPin(runC)
	_, err = dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID: runC, JobID: jobID, AssetID: assetID, TargetLanguage: "vi",
	})
	if !errors.Is(err, domain.ErrDubScriptRequiredForDubbing) {
		t.Fatalf("a run with no dub script evidence must fail closed, got %v", err)
	}

	// 3. Evidence can also be the run's own dub_script stage pin (a cached-reuse run owns no index
	// row): the pinned artifact is then the run's script.
	runD := "run-omitted-d"
	createRunWithRolePlanPin(runD)
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID: uuid.NewString(), RunID: runD, Stage: "dub_script", Status: domain.StageStatusSucceeded,
		ArtifactSHA256: scriptA.CASHash, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pin run D dub script stage: %v", err)
	}
	if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: "va-" + runD, AssetID: assetID, RunID: runD, TargetLanguage: "vi",
		CASHash: assignmentA.CASHash, ProvenanceHash: assignmentA.ProvenanceHash, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save run D voice assignment index: %v", err)
	}
	segmentsD, err := dubSvc.SynthesizeAndFit(ctx, domain.DubbingJobInput{
		RunID: runD, JobID: jobID, AssetID: assetID, TargetLanguage: "vi",
		VoiceAssignmentCAS: assignmentA.CASHash,
	})
	if err != nil {
		t.Fatalf("SynthesizeAndFit for run D: %v", err)
	}
	if segmentsD.DubScriptVariantCAS != scriptA.CASHash {
		t.Fatalf("run D resolved %s, want its pinned %s", segmentsD.DubScriptVariantCAS, scriptA.CASHash)
	}
}

type funcSpokenAdapter func(ctx context.Context, req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error)

func (f funcSpokenAdapter) AdaptSpokenScript(ctx context.Context, req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
	return f(ctx, req)
}

// Issue #154: Measured rewrite receives actual duration/overrun, playback allowance,
// canonical meaning, and frozen terminology; removes the last-word deletion fallback;
// and preserves honest prior text/evidence without fresh TTS/probe when the adapter
// errors, returns empty/unchanged text, or fails facts/names/numbers/negation/glossary QA.
func TestDubbingService_Issue154_MeasuredRewrite_HonestFailureAndQAGate(t *testing.T) {
	cases := []struct {
		name       string
		sourceText string
		meaning    string
		spoken     string
		glossary   []domain.GlossaryEntry
		adapterFn  func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error)
		wantAccept bool
		wantCalls  int
		wantText   string
	}{
		{
			name:       "adapter_error_no_last_word_deletion_no_resynth",
			sourceText: "今天天气真的非常好。",
			meaning:    "Hôm nay thời tiết thật sự rất đẹp.",
			spoken:     "Hôm nay thời tiết thật sự rất đẹp.",
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return nil, errors.New("adapter unavailable")
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Hôm nay thời tiết thật sự rất đẹp.",
		},
		{
			name:       "empty_rewrite_no_last_word_deletion_no_resynth",
			sourceText: "今天天气真的非常好。",
			meaning:    "Hôm nay thời tiết thật sự rất đẹp.",
			spoken:     "Hôm nay thời tiết thật sự rất đẹp.",
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return &provider.SpokenScriptAdaptationResult{SpokenText: "   "}, nil
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Hôm nay thời tiết thật sự rất đẹp.",
		},
		{
			name:       "unchanged_rewrite_no_last_word_deletion_no_resynth",
			sourceText: "今天天气真的非常好。",
			meaning:    "Hôm nay thời tiết thật sự rất đẹp.",
			spoken:     "Hôm nay thời tiết thật sự rất đẹp.",
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return &provider.SpokenScriptAdaptationResult{SpokenText: req.MeaningText}, nil
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Hôm nay thời tiết thật sự rất đẹp.",
		},
		{
			name:       "number_corruption_rejected_before_tts",
			sourceText: "这台机器有 500 瓦功率。",
			meaning:    "Cỗ máy này có công suất 500 watt.",
			spoken:     "Cỗ máy này có công suất 500 watt.",
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return &provider.SpokenScriptAdaptationResult{SpokenText: "Máy có công suất 600 watt."}, nil
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Cỗ máy này có công suất 500 watt.",
		},
		{
			name:       "negation_corruption_rejected_before_tts",
			sourceText: "我今天不去商店。",
			meaning:    "Hôm nay tôi không đi cửa hàng.",
			spoken:     "Hôm nay tôi không đi cửa hàng.",
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return &provider.SpokenScriptAdaptationResult{SpokenText: "Hôm nay tôi đi cửa hàng."}, nil
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Hôm nay tôi không đi cửa hàng.",
		},
		{
			name:       "name_corruption_rejected_before_tts",
			sourceText: "这款 SUPOR 电饭煲很好用。",
			meaning:    "Chiếc nồi cơm điện SUPOR này rất dễ dùng.",
			spoken:     "Chiếc nồi cơm điện SUPOR này rất dễ dùng.",
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return &provider.SpokenScriptAdaptationResult{SpokenText: "Chiếc nồi cơm điện này dễ dùng."}, nil
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Chiếc nồi cơm điện SUPOR này rất dễ dùng.",
		},
		{
			name:       "glossary_drop_rejected_before_tts",
			sourceText: "这款 SUPOR 电饭煲很好用。",
			meaning:    "Chiếc nồi cơm điện SuporVN này rất dễ dùng.",
			spoken:     "Chiếc nồi cơm điện SuporVN này rất dễ dùng.",
			glossary:   []domain.GlossaryEntry{{Source: "SUPOR", Target: "SuporVN"}},
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return &provider.SpokenScriptAdaptationResult{SpokenText: "Nồi cơm điện SUPOR dễ dùng."}, nil
			},
			wantAccept: false,
			wantCalls:  1,
			wantText:   "Chiếc nồi cơm điện SuporVN này rất dễ dùng.",
		},
		{
			name:       "valid_glossary_localization_passes_qa_and_probes_fresh_tts",
			sourceText: "这款 SUPOR 500 电饭煲很好用。",
			meaning:    "Chiếc nồi cơm điện SuporVN 500 này thật sự rất dễ dùng.",
			spoken:     "Chiếc nồi cơm điện SuporVN 500 này thật sự rất dễ dùng.",
			glossary:   []domain.GlossaryEntry{{Source: "SUPOR", Target: "SuporVN"}},
			adapterFn: func(req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				if req.MeasuredDurationMs != 1400 || req.PlaybackAllowanceMs != 1000 || req.OverrunMs != 400 {
					return nil, fmt.Errorf("unexpected rewrite budget: measured=%d allowance=%d overrun=%d", req.MeasuredDurationMs, req.PlaybackAllowanceMs, req.OverrunMs)
				}
				if req.MeaningText != "Chiếc nồi cơm điện SuporVN 500 này thật sự rất dễ dùng." {
					return nil, fmt.Errorf("expected canonical MeaningText, got %q", req.MeaningText)
				}
				if len(req.ProtectedTerms) != 1 || req.ProtectedTerms[0].Target != "SuporVN" {
					return nil, fmt.Errorf("expected frozen ProtectedTerms [SuporVN], got %+v", req.ProtectedTerms)
				}
				return &provider.SpokenScriptAdaptationResult{SpokenText: "Nồi SuporVN 500 rất dễ dùng."}, nil
			},
			wantAccept: true,
			wantCalls:  2,
			wantText:   "Nồi SuporVN 500 rất dễ dùng.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
			defer db.Close()

			assetID := uuid.NewString()
			runID := uuid.NewString()
			setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

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
						EndMs:          1000,
						SlotDurationMs: 1000,
						SourceText:     tc.sourceText,
						MeaningText:    tc.meaning,
						SpokenText:     tc.spoken,
					},
				},
				CreatedAt: time.Now().UTC(),
			}
			pinDubbingScriptLineage(t, db, casStore, &dubScript, runID, tc.glossary...)
			scriptBytes, _ := json.Marshal(dubScript)
			scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

			assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
				RunID:          runID,
				AssetID:        assetID,
				TargetLanguage: "vi",
			})
			if err != nil {
				t.Fatalf("AssignVoices: %v", err)
			}

			dubSvc.ConfigureSpokenAdapter(funcSpokenAdapter(func(_ context.Context, req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
				return tc.adapterFn(req)
			}))

			synthCalls := 0
			dubSvc.TTSInvoke = func(_ context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
				synthCalls++
				durMs := int64(1400) // Attempt 1 overruns 1000ms slot
				if synthCalls == 2 {
					durMs = 850 // Attempt 2 (accepted rewrite) fits 1000ms slot
				}
				wav := media.GeneratePCM16WAV(16000, 1, durMs)
				sum := sha256.Sum256(wav)
				return &provider.TTSSynthesisResult{
					AudioData:          wav,
					AudioSHA256:        hex.EncodeToString(sum[:]),
					ProviderID:         p.ID(),
					MeasuredDurationMs: durMs,
				}, nil
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

			if synthCalls != tc.wantCalls {
				t.Fatalf("expected %d TTS synthesis calls, got %d", tc.wantCalls, synthCalls)
			}
			if len(variant.FitPlans) != 1 || variant.FitPlans[0].AttemptCount != tc.wantCalls {
				t.Fatalf("FitPlan.AttemptCount must match actual synthesis count %d, got %+v", tc.wantCalls, variant.FitPlans)
			}
			if tc.wantAccept {
				if variant.OverallStatus != "PASS" || len(variant.Segments) != 1 {
					t.Fatalf("expected PASS with 1 selected segment, got status=%s segments=%d", variant.OverallStatus, len(variant.Segments))
				}
				if variant.Segments[0].SpokenText != tc.wantText {
					t.Fatalf("expected selected SpokenText %q, got %q", tc.wantText, variant.Segments[0].SpokenText)
				}
				if variant.Segments[0].MeasuredDurationMs != 850 {
					t.Fatalf("expected fresh probed duration 850ms, got %dms", variant.Segments[0].MeasuredDurationMs)
				}
			} else {
				if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 1 {
					t.Fatalf("expected REVIEW_REQUIRED with 1 review segment, got status=%s review=%d", variant.OverallStatus, len(variant.ReviewSegments))
				}
				rev := variant.ReviewSegments[0]
				if rev.SpokenText != tc.wantText {
					t.Fatalf("expected preserved honest prior SpokenText %q (never last-word deleted), got %q", tc.wantText, rev.SpokenText)
				}
				if rev.MeasuredDurationMs != 1400 || rev.AttemptCount != 1 {
					t.Fatalf("expected honest prior measured duration 1400ms and AttemptCount=1, got dur=%d attempts=%d", rev.MeasuredDurationMs, rev.AttemptCount)
				}
			}
		})
	}
}

// Issue #154: At most one native retry per source lineage on an explicitly rate-capable and
// calibrated lane; a second native retry is refused after the retry still overruns; recovery
// state carries into regroup without budget reset; policy/calibration change invalidates cache
// identity. The lane is deliberately NOT the production VieNeu lane: that adapter has no engine
// rate control and its provider is fixed-rate, so it must never reach this path at all
// (TestProductionSpeechRegistry_VieNeuLaneIsFixedRateNaturalOnly pins that).
func TestDubbingService_Issue154_LineageBudgetAndCalibrationCacheInvalidation(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	dubScript := domain.DubScriptVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubScriptSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.DubScriptSegment{
			{
				Index:            0,
				SpeakerID:        "SPEAKER_00",
				StartMs:          0,
				EndMs:            1000,
				SlotDurationMs:   1000,
				SourceText:       "第一句需要测试速度与重组的恢复预算。",
				MeaningText:      "Câu đầu tiên cần kiểm tra ngân sách khôi phục tốc độ.",
				SpokenText:       "Câu đầu tiên cần kiểm tra ngân sách khôi phục tốc độ.",
				SourceGapAfterMs: 100,
			},
			{
				Index:            1,
				SpeakerID:        "SPEAKER_00",
				StartMs:          1100,
				EndMs:            3500,
				SlotDurationMs:   2400,
				SourceText:       "第二句合并槽位。",
				MeaningText:      "Câu hai hợp nhất.",
				SpokenText:       "Câu hai hợp nhất.",
				SourceGapAfterMs: 100,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	// Assign an explicitly rate-capable lane: CosyVoice3's adapter really passes the requested
	// speed into the engine, so a native speed attempt is legitimate here (the production VieNeu
	// lane is fixed-rate and never reaches this path).
	fitVoice := provider.CosyVoicePresetVoices("vi")[0]
	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:             runID,
		AssetID:           assetID,
		TargetLanguage:    "vi",
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": fitVoice},
	})
	if err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}

	// Fixture constants, shared by the envelope, the fake engine and the assertions so the
	// refusal attribution below stays checkable.
	const (
		segSlotMs         = 1000 // seg 0 slot: 0-1000ms
		naturalSegMs      = 1200 // seg 0 at 1.0x: overruns the slot, so 1.20x is requested
		regroupedMs       = 1800 // regrouped [0,1] at 1.0x: fits its combined window
		minSpeed          = 0.8
		maxSpeed          = 1.5
		realizedSpeedGain = 0.25
	)

	// Configure a verified envelope for that lane. The envelope must name the exact lane the
	// TTSInvoke hook reports: the voice's provider id as model name, at version 1.0.
	envelope := func(calibrationID string) domain.NativeSpeedEnvelope {
		return domain.NativeSpeedEnvelope{
			ProviderID:     fitVoice.ProviderID,
			ModelID:        fitVoice.ProviderID,
			ModelVersion:   "1.0",
			VoiceProfileID: fitVoice.ID,
			MinSpeed:       minSpeed,
			MaxSpeed:       maxSpeed,
			Verified:       true,
			CalibrationID:  calibrationID,
		}
	}
	cfg := service.DefaultFitControllerConfig()
	cfg.NativeSpeedEnvelopes = []domain.NativeSpeedEnvelope{envelope("cal-v1")}
	dubSvc.ConfigureFitController(service.NewFitController(cfg))
	// Adapter returns unchanged text so rewrite is consumed without a synthesis pass,
	// advancing directly to regroup after the single native retry overruns.
	dubSvc.ConfigureSpokenAdapter(funcSpokenAdapter(func(_ context.Context, req provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		return &provider.SpokenScriptAdaptationResult{SpokenText: req.MeaningText}, nil
	}))

	// The fake honors req.Speed - the waveform shortens with the request instead of ignoring it -
	// but realizes only part of the requested gain. That measured shortfall is what the single
	// retry cannot close, so the retry overruns again and only the consumed lineage budget can be
	// refusing the second native attempt. Both the requested and the realized factor stay inside
	// [minSpeed, maxSpeed] by construction: the retry duration never exceeds the natural one.
	var recordedSpeeds []float64
	var recordedDurations []int64
	dubSvc.TTSInvoke = func(_ context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		recordedSpeeds = append(recordedSpeeds, req.Speed)
		naturalMs := int64(regroupedMs)
		if len(recordedSpeeds) <= 2 {
			naturalMs = naturalSegMs
		}
		durMs := int64(float64(naturalMs) / (1 + (req.Speed-1)*realizedSpeedGain))
		recordedDurations = append(recordedDurations, durMs)
		wav := media.GeneratePCM16WAV(16000, 1, durMs)
		sum := sha256.Sum256(wav)
		return &provider.TTSSynthesisResult{
			AudioData:          wav,
			AudioSHA256:        hex.EncodeToString(sum[:]),
			ProviderID:         p.ID(),
			MeasuredDurationMs: durMs,
		}, nil
	}

	jobIn := domain.DubbingJobInput{
		RunID:               runID,
		AssetID:             assetID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: scriptCAS.SHA256,
		VoiceAssignmentCAS:  assign.CASHash,
	}
	variant1, err := dubSvc.SynthesizeAndFit(context.Background(), jobIn)
	if err != nil {
		t.Fatalf("SynthesizeAndFit: %v", err)
	}

	// Exactly 3 calls: (1) seg 0 natural 1.0x, (2) seg 0 single native retry >1.0x, (3) regroup [0,1] at 1.0x.
	// No second native retry and no wasted synthesis on unchanged rewrite!
	if len(recordedSpeeds) != 3 {
		t.Fatalf("expected exactly 3 synthesis calls (natural, 1 native retry, 1 regroup), got %d: %v", len(recordedSpeeds), recordedSpeeds)
	}
	if recordedSpeeds[0] != 1.0 || recordedSpeeds[1] <= 1.0 || recordedSpeeds[2] != 1.0 {
		t.Fatalf("unexpected speed sequence: %v", recordedSpeeds)
	}
	// The retry really applied the requested speed (its waveform got shorter) and still overran
	// seg 0's slot, so refusing the second attempt is a lineage-budget decision rather than the
	// fixture quietly ignoring req.Speed.
	if recordedDurations[1] >= recordedDurations[0] {
		t.Fatalf("native retry must expose req.Speed instead of ignoring it: natural=%dms retry=%dms",
			recordedDurations[0], recordedDurations[1])
	}
	if recordedDurations[1] <= segSlotMs {
		t.Fatalf("fixture must keep the native retry overrunning seg 0's %dms slot, got %dms", segSlotMs, recordedDurations[1])
	}
	if variant1.OverallStatus != "PASS" || len(variant1.Segments) != 1 || len(variant1.Segments[0].SpeechBlockIndices) != 2 {
		t.Fatalf("expected regrouped PASS covering [0,1], got status=%s segments=%+v", variant1.OverallStatus, variant1.Segments)
	}

	// Changing the calibration ID in FitControllerConfig must invalidate the stage cache key
	cfg2 := cfg
	cfg2.NativeSpeedEnvelopes = []domain.NativeSpeedEnvelope{envelope("cal-v2-updated")}
	dubSvc.ConfigureFitController(service.NewFitController(cfg2))
	if dubSvc.CanReuseVariant(context.Background(), jobIn, variant1) {
		t.Fatal("CanReuseVariant must return false when profile calibration identity changes")
	}
}

// Issue #154 R4: an accepted rewrite on a fixed-rate lane still cannot fit its slot (that lane
// has no engine rate control, so a rewrite is its only in-lane remedy and the lane is fixed at
// 1.0), so the run escalates the whole speaker to the duration-controlled fallback lane. The
// escalated pass re-enters with the SAME lineage recovery state: the accepted rewritten text is
// what the fallback lane synthesizes, and the lineage's spent rewrite budget is not refilled, so
// the lineage can never be granted a second rewrite.
func TestDubbingService_Issue154_AcceptedRewriteCarriesIntoSpeakerEscalation(t *testing.T) {
	dubSvc, db, casStore, reg, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	const (
		originalText  = "Hôm nay thời tiết thật sự rất đẹp."
		rewrittenText = "Hôm nay trời rất đẹp."
		segmentSlotMs = 1000
		fixedLaneMs   = 1500 // overruns the slot for the natural and the rewritten candidate alike
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
				SourceText:     "今天天气真的非常好。",
				MeaningText:    originalText,
				SpokenText:     originalText,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	// The unattended VI default lane is a verified fixed-rate preset voice.
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
		t.Fatal("the unattended VI fixed-rate lane must be registered")
	}
	fixedFake, ok := fixedProvider.(*provider.FakeTTSProvider)
	if !ok {
		t.Fatalf("unexpected provider type %T for the fixed-rate lane", fixedProvider)
	}
	fixedFake.DurationMs = fixedLaneMs

	// The duration-controlled fallback lane the escalation targets, registered before the run
	// so the escalation is policy/license eligible.
	fallbackFake := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", fixedLaneMs)
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

	adapterCalls := 0
	dubSvc.ConfigureSpokenAdapter(funcSpokenAdapter(func(_ context.Context, _ provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		adapterCalls++
		return &provider.SpokenScriptAdaptationResult{SpokenText: rewrittenText}, nil
	}))

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

	// Exactly one rewrite for the lineage: the escalated pass must not refill the spent budget.
	if adapterCalls != 1 {
		t.Fatalf("the lineage rewrite budget must stay spent across escalation, adapter called %d times", adapterCalls)
	}
	if len(variant.Escalations) != 1 {
		t.Fatalf("the unresolved fixed-rate overrun must escalate the whole speaker once, got %+v", variant.Escalations)
	}
	esc := variant.Escalations[0]
	if esc.FromProviderID != provider.ZeroTTSProviderID || esc.ToProviderID != provider.CosyVoiceProviderID ||
		len(esc.TriggerSegmentIndices) != 1 || esc.TriggerSegmentIndices[0] != 0 {
		t.Fatalf("unexpected escalation evidence: %+v", esc)
	}
	if esc.Resolved {
		t.Fatalf("the escalated lane still overruns the slot, so the escalation must stay unresolved: %+v", esc)
	}
	if variant.OverallStatus != "REVIEW_REQUIRED" || len(variant.ReviewSegments) != 1 {
		t.Fatalf("expected an unresolved REVIEW after escalation, got status=%s review=%+v", variant.OverallStatus, variant.ReviewSegments)
	}
	if variant.ReviewSegments[0].SpokenText != rewrittenText {
		t.Fatalf("the accepted rewrite must carry into the escalated pass, got %q", variant.ReviewSegments[0].SpokenText)
	}
	if variant.ReviewSegments[0].CalibrationID != "" {
		t.Fatalf("a fixed-rate lineage must never carry native-speed evidence, got %q", variant.ReviewSegments[0].CalibrationID)
	}
}

// Issue #154 AC7: accepting a rewrite returns the candidate to natural playback, so the next
// synthesis of the rewritten text runs at speed 1.0 and neither the selected segment nor the
// fit evidence carries the CalibrationID of the native-speed retry spent before the rewrite.
func TestDubbingService_Issue154_AcceptedRewriteResetsNativeSpeedEvidence(t *testing.T) {
	dubSvc, db, casStore, _, _ := setupDubbingTestHarness(t)
	defer db.Close()

	assetID := uuid.NewString()
	runID := uuid.NewString()
	setupAssetJobRunAudioRole(t, db, casStore, assetID, runID, "vi")

	const (
		segmentSlotMs = 1000
		originalText  = "Hôm nay thời tiết thật sự rất đẹp."
		rewrittenText = "Hôm nay trời rất đẹp."
		// 1200ms natural against a 1000ms slot: the fit must request exactly 1.20x.
		naturalMs    = 1200
		retrySpeed   = 1.2
		realizedGain = 0.25 // the fake lane shortens, but only partly: the retry still overruns
		rewrittenMs  = 900  // rewritten at natural speed finally fits the slot
		calibration  = "cal-v1"
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
				SourceText:     "今天天气真的非常好。",
				MeaningText:    originalText,
				SpokenText:     originalText,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	pinDubbingScriptLineage(t, db, casStore, &dubScript, runID)
	scriptBytes, _ := json.Marshal(dubScript)
	scriptCAS, _ := casStore.Put(bytes.NewReader(scriptBytes))

	// A rate-capable, calibrated lane: CosyVoice3's adapter honors a requested speed, so the
	// single native retry is legitimate here.
	fitVoice := provider.CosyVoicePresetVoices("vi")[0]
	assign, err := dubSvc.AssignVoices(context.Background(), domain.VoiceAssignmentInput{
		RunID:             runID,
		AssetID:           assetID,
		TargetLanguage:    "vi",
		CustomAssignments: map[string]domain.VoiceProfile{"SPEAKER_00": fitVoice},
	})
	if err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}

	cfg := service.DefaultFitControllerConfig()
	cfg.NativeSpeedEnvelopes = []domain.NativeSpeedEnvelope{{
		ProviderID:     fitVoice.ProviderID,
		ModelID:        fitVoice.ProviderID,
		ModelVersion:   "1.0",
		VoiceProfileID: fitVoice.ID,
		MinSpeed:       0.8,
		MaxSpeed:       1.5,
		Verified:       true,
		CalibrationID:  calibration,
	}}
	dubSvc.ConfigureFitController(service.NewFitController(cfg))

	adapterCalls := 0
	dubSvc.ConfigureSpokenAdapter(funcSpokenAdapter(func(_ context.Context, _ provider.SpokenScriptAdaptationRequest) (*provider.SpokenScriptAdaptationResult, error) {
		adapterCalls++
		return &provider.SpokenScriptAdaptationResult{SpokenText: rewrittenText}, nil
	}))

	var speeds []float64
	var texts []string
	dubSvc.TTSInvoke = func(_ context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		speeds = append(speeds, req.Speed)
		texts = append(texts, req.Text)
		durMs := int64(rewrittenMs)
		if req.Text != rewrittenText {
			durMs = int64(float64(naturalMs) / (1 + (req.Speed-1)*realizedGain))
		}
		wav := media.GeneratePCM16WAV(16000, 1, durMs)
		sum := sha256.Sum256(wav)
		return &provider.TTSSynthesisResult{
			AudioData:          wav,
			AudioSHA256:        hex.EncodeToString(sum[:]),
			ProviderID:         p.ID(),
			MeasuredDurationMs: durMs,
		}, nil
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

	// natural 1.0x -> one calibrated retry -> the accepted rewrite re-synthesized at 1.0x.
	if len(speeds) != 3 {
		t.Fatalf("expected exactly 3 synthesis calls (natural, calibrated retry, rewritten), got %d: %v", len(speeds), speeds)
	}
	if speeds[0] != 1.0 || speeds[1] != retrySpeed || speeds[2] != 1.0 {
		t.Fatalf("unexpected speed sequence: %v", speeds)
	}
	if texts[0] != originalText || texts[1] != originalText || texts[2] != rewrittenText {
		t.Fatalf("unexpected synthesized text sequence: %q", texts)
	}
	if adapterCalls != 1 {
		t.Fatalf("exactly one rewrite must be requested, adapter called %d times", adapterCalls)
	}

	if variant.OverallStatus != "PASS" || len(variant.Segments) != 1 || len(variant.FitPlans) != 1 {
		t.Fatalf("expected PASS with one selected segment and its fit plan, got status=%s segments=%d plans=%d",
			variant.OverallStatus, len(variant.Segments), len(variant.FitPlans))
	}
	if got := variant.Segments[0].SpokenText; got != rewrittenText {
		t.Fatalf("expected the accepted rewrite to be selected, got %q", got)
	}
	if got := variant.Segments[0].CalibrationID; got != "" {
		t.Fatalf("natural-speed rewritten selection must not carry a CalibrationID, got %q", got)
	}
	plan := variant.FitPlans[0]
	if plan.AttemptCount != 3 || plan.SpeedFactor != 1.0 || plan.CalibrationID != "" {
		t.Fatalf("fit evidence must record the natural-speed rewritten attempt without calibration: %+v", plan)
	}
}
