package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
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
	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
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
	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
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
		_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
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
		_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
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
		_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
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

		assignIn := domain.VoiceAssignmentInput{
			RunID:          uuid.NewString(),
			AssetID:        assetNoSpeech,
			TargetLanguage: "vi",
		}
		_, err := dubSvc.AssignVoices(context.Background(), assignIn)
		if err == nil || !errors.Is(err, domain.ErrNoDubbingRequired) {
			t.Fatalf("expected ErrNoDubbingRequired from AssignVoices on no-speech video, got: %v", err)
		}
	})

	t.Run("AssignVoices_MissingAudioRolePlan_PreservesCompatibility", func(t *testing.T) {
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
		res, err := dubSvc.AssignVoices(context.Background(), assignIn)
		if err != nil {
			t.Fatalf("expected AssignVoices to succeed when plan is missing (compatibility preserved), got: %v", err)
		}
		if res == nil || len(res.Assignments) == 0 {
			t.Fatalf("expected valid assignments returned")
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
	_ = db.SaveAudioRolePlan(context.Background(), domain.AudioRolePlan{
		ID:        "plan-" + assetID,
		AssetID:   assetID,
		CreatedAt: time.Now().UTC(),
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleInstrumentalBgm},
		},
	})

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
