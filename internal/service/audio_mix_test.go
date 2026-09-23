package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func setupAudioMixTestHarness(t *testing.T) (*service.AudioMixService, *storage.DB, *cas.Store, *provider.Registry, *provider.Router) {
	t.Helper()
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "audio_mix_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	mixSvc := service.NewAudioMixService(db, casStore)
	mixSvc.ConfigureRouter(router)
	return mixSvc, db, casStore, reg, router
}

func TestAudioMixService_MissingAudioRolePlanFailsClosed(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()

	assetID := "asset_no_plan_" + uuid.NewString()[:8]
	runID := "run_test_" + uuid.NewString()[:8]

	// Put dummy source media in CAS and DB
	dummyMedia := []byte("DUMMY_AUDIO_DATA_FOR_MIX_TEST")
	mediaObj, err := casStore.Put(bytes.NewReader(dummyMedia))
	if err != nil {
		t.Fatalf("put media in cas: %v", err)
	}
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})

	err = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mediaObj.SHA256,
		CASPath:             mediaObj.Path,
		ByteSize:            int64(len(dummyMedia)),
		CreatedAt:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save source asset: %v", err)
	}

	// Deliberately do NOT save AudioRolePlan in DB.
	// Invariant: AudioMixService must fail closed with ErrAudioRolePlanRequired
	mixArtifact, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})

	if err == nil {
		t.Fatalf("expected error on missing AudioRolePlan, got success: %+v", mixArtifact)
	}
	if !errors.Is(err, domain.ErrAudioRolePlanRequired) {
		t.Fatalf("expected ErrAudioRolePlanRequired, got %v", err)
	}
	if mixArtifact != nil {
		t.Fatalf("expected nil artifact on fail-closed missing plan, got %+v", mixArtifact)
	}
}

func TestAudioMixService_ZeroSpokenSpeechPassthrough(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()

	assetID := "asset_zero_speech_" + uuid.NewString()[:8]
	runID := "run_test_" + uuid.NewString()[:8]

	// Put dummy source media in CAS and DB
	dummyMedia := media.GeneratePCM16WAV(16000, 1, 3000)
	mediaObj, err := casStore.Put(bytes.NewReader(dummyMedia))
	if err != nil {
		t.Fatalf("put media in cas: %v", err)
	}
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})

	err = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mediaObj.SHA256,
		CASPath:             mediaObj.Path,
		ByteSize:            int64(len(dummyMedia)),
		CreatedAt:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save source asset: %v", err)
	}
	// Save PreflightReport in DB
	preflight := domain.PreflightReport{
		ID:                     "preflight_" + assetID,
		AssetID:                assetID,
		DurationSec:            3.0,
		DurationMs:             3000,
		AudioChannels:          1,
		AudioSampleRate:        16000,
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  mediaObj.SHA256,
		NormalizedAudioCASPath: mediaObj.Path,
		CreatedAt:              time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, preflight); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}

	// Save AudioRolePlan with 0 spoken dialogue segments (only BGM/Instrumental)
	rolePlan := domain.AudioRolePlan{
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleInstrumentalBgm},
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := db.SaveAudioRolePlan(ctx, rolePlan); err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}

	mixArtifact, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
	})
	if err != nil {
		t.Fatalf("zero-speech mix failed: %v", err)
	}
	if mixArtifact.OverallStatus != "PASS" {
		t.Errorf("expected PASS status, got %s", mixArtifact.OverallStatus)
	}
	if mixArtifact.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed = false on zero-speech video")
	}
	if !mixArtifact.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved = true")
	}
	if mixArtifact.AudioCASHash != mediaObj.SHA256 {
		t.Errorf("expected bitstream-exact AudioCASHash %s, got %s", mediaObj.SHA256, mixArtifact.AudioCASHash)
	}
	if mixArtifact.AudioCASPath != mediaObj.Path {
		t.Errorf("expected AudioCASPath %s, got %s", mediaObj.Path, mixArtifact.AudioCASPath)
	}
	if mixArtifact.DubSegmentsCAS != "" {
		t.Errorf("expected empty DubSegmentsCAS, got %s", mixArtifact.DubSegmentsCAS)
	}
	if mixArtifact.AudioStemsCAS != "" {
		t.Errorf("expected empty AudioStemsCAS on clean passthrough, got %s", mixArtifact.AudioStemsCAS)
	}
}

func TestAudioMixService_ZeroDurationSlotOverrun_Refused(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()

	assetID := "asset_zero_slot_" + uuid.NewString()[:8]
	runID := "run_test_" + uuid.NewString()[:8]

	dummyMedia := media.GeneratePCM16WAV(16000, 1, 35000)
	mediaObj, err := casStore.Put(bytes.NewReader(dummyMedia))
	if err != nil {
		t.Fatalf("put media in cas: %v", err)
	}
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mediaObj.SHA256,
		CASPath:             mediaObj.Path,
		ByteSize:            int64(len(dummyMedia)),
		CreatedAt:           time.Now().UTC(),
	})
	preflight := domain.PreflightReport{
		ID:                     "preflight_" + assetID,
		AssetID:                assetID,
		DurationSec:            35.0,
		DurationMs:             35000,
		AudioChannels:          1,
		AudioSampleRate:        16000,
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  mediaObj.SHA256,
		NormalizedAudioCASPath: mediaObj.Path,
		CreatedAt:              time.Now().UTC(),
	}
	_ = db.SavePreflightReport(ctx, preflight)

	rolePlan := domain.AudioRolePlan{
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	}
	_ = db.SaveAudioRolePlan(ctx, rolePlan)

	// Stems artifact
	stems := domain.AudioStemArtifacts{
		ID:      uuid.NewString(),
		AssetID: assetID,
		Stems: []domain.AudioStem{
			{Type: domain.StemTypeBackground, AudioCASHash: mediaObj.SHA256},
			{Type: domain.StemTypeVocals, AudioCASHash: mediaObj.SHA256},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemsBytes, _ := json.Marshal(stems)
	stemsObj, _ := casStore.Put(bytes.NewReader(stemsBytes))
	_ = db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:        stems.ID,
		AssetID:   assetID,
		CASHash:   stemsObj.SHA256,
		CreatedAt: stems.CreatedAt,
	})
	// DubSegments containing a segment where start_ms == end_ms (0ms slot) but measured_duration_ms > 0
	dubSegments := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		Segments: []domain.DubSegment{
			{
				Index:              12,
				SpeechBlockIndices: []int{12},
				StartMs:            29680,
				EndMs:              29680,
				SlotDurationMs:     0,
				MeasuredDurationMs: 640,
				FitDecision:        domain.FitActionAccept,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubSegments)
	dubObj, _ := casStore.Put(bytes.NewReader(dubBytes))
	_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dubSegments.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        dubObj.SHA256,
		CreatedAt:      dubSegments.CreatedAt,
	})

	_, err = mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		DubSegmentsCAS: dubObj.SHA256,
		AudioStemsCAS:  stemsObj.SHA256,
	})
	if err == nil {
		t.Fatalf("expected ErrMixerOverrunRefused on 0ms slot with positive measured duration, got nil error")
	}
	if !errors.Is(err, domain.ErrMixerOverrunRefused) {
		t.Fatalf("expected ErrMixerOverrunRefused, got %v", err)
	}
}

// A dub-eligible mix whose dub stage accepted NOTHING must refuse instead of stripping the source
// dialogue. Live evidence: runs 264aecaf and 4f86657f placed zero clips (every candidate overran its slot
// and stayed in ReviewSegments) yet reported PASS with dialogue_suppressed=true, and the rendered audio came
// out bit-identical to the background stem - a video with no voice at all and no gate in the way.
func TestAudioMixService_DubRequiredButNothingPlaceable_Refused(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID, runID, stemsCAS := mixFixtureWithDubEligibleSpeech(t, db, casStore)

	// The dub stage built candidates for every speech block and could not fit any of them, so Segments is
	// empty and the candidates sit in ReviewSegments.
	dubSegments := domain.DubSegmentsVariant{
		ID:                uuid.NewString(),
		SchemaVersion:     domain.DubSegmentsSchemaVersion,
		AssetID:           assetID,
		RunID:             runID,
		TargetLanguage:    "vi",
		Segments:          nil,
		FixedRateSpeakers: []string{"SPEAKER_00"},
		OverallStatus:     "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              12000,
				SlotDurationMs:     12000,
				MeasuredDurationMs: 13520,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubSegments)
	dubObj, _ := casStore.Put(bytes.NewReader(dubBytes))

	mix, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		DubSegmentsCAS: dubObj.SHA256,
		AudioStemsCAS:  stemsCAS,
	})
	if !errors.Is(err, domain.ErrMixerOverrunRefused) {
		t.Fatalf("expected ErrMixerOverrunRefused when dubbing is required but nothing is placeable, got err=%v mix=%+v", err, mix)
	}
	if mix == nil || mix.OverallStatus != "REFUSED" {
		t.Fatalf("expected a REFUSED dub mix artifact, got %+v", mix)
	}
	if mix.DialogueSuppressed {
		t.Errorf("refused mix must not claim the source dialogue was suppressed: %+v", mix)
	}
	if !strings.Contains(mix.RefusalReason, "13520") || !strings.Contains(mix.RefusalReason, "12000") {
		t.Errorf("refusal reason must name the overrun that left nothing placeable, got %q", mix.RefusalReason)
	}
}

// mixFixtureWithDubEligibleSpeech seeds the minimum a dub-eligible mix needs: a rights-cleared asset, a
// preflight report, one full-length narration window, and background/vocals stems in CAS.
func mixFixtureWithDubEligibleSpeech(t *testing.T, db *storage.DB, casStore *cas.Store) (assetID, runID, stemsCAS string) {
	t.Helper()
	ctx := context.Background()
	assetID = "asset_dub_eligible_" + uuid.NewString()[:8]
	runID = "run_test_" + uuid.NewString()[:8]

	dummyMedia := media.GeneratePCM16WAV(16000, 1, 35000)
	mediaObj, err := casStore.Put(bytes.NewReader(dummyMedia))
	if err != nil {
		t.Fatalf("put media in cas: %v", err)
	}
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mediaObj.SHA256,
		CASPath:             mediaObj.Path,
		ByteSize:            int64(len(dummyMedia)),
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.SavePreflightReport(ctx, domain.PreflightReport{
		ID:                     "preflight_" + assetID,
		AssetID:                assetID,
		DurationSec:            35.0,
		DurationMs:             35000,
		AudioChannels:          1,
		AudioSampleRate:        16000,
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  mediaObj.SHA256,
		NormalizedAudioCASPath: mediaObj.Path,
		CreatedAt:              time.Now().UTC(),
	})
	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + assetID,
		AssetID:        assetID,
		ProviderID:     "test-role-provider",
		ModelName:      "test-role-model",
		ModelVersion:   "1",
		ProvenanceHash: "prov-role-" + assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	}
	roleBytes, _ := json.Marshal(rolePlan)
	roleObj, err := casStore.Put(bytes.NewReader(roleBytes))
	if err != nil {
		t.Fatalf("put role plan: %v", err)
	}
	rolePlan.CASHash = roleObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, rolePlan); err != nil {
		t.Fatalf("save role plan: %v", err)
	}
	if err := db.SaveAudioRolePlanIndex(ctx, storage.AudioRolePlanIndex{
		ID: rolePlan.ID, AssetID: rolePlan.AssetID, ProviderID: rolePlan.ProviderID,
		ModelName: rolePlan.ModelName, ModelVersion: rolePlan.ModelVersion,
		CASHash: rolePlan.CASHash, ProvenanceHash: rolePlan.ProvenanceHash, CreatedAt: rolePlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save role plan index: %v", err)
	}
	transcript := domain.TranscriptArtifact{
		ID:      "transcript-" + assetID,
		AssetID: assetID,
		RunID:   runID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 1000, EndMs: 3000, SpeakerID: "SPEAKER_00", SourceText: "测试", SegmentType: domain.SpeechBlockTypeSpeech},
		},
		SourceLanguage: "zh",
		CreatedAt:      time.Now().UTC(),
	}
	transcriptBytes, _ := json.Marshal(transcript)
	transcriptObj, err := casStore.Put(bytes.NewReader(transcriptBytes))
	if err != nil {
		t.Fatalf("put transcript artifact: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID: transcript.ID, AssetID: assetID, RunID: runID, CASHash: transcriptObj.SHA256,
		ProvenanceHash: "prov-transcript-" + assetID, CreatedAt: transcript.CreatedAt,
	}); err != nil {
		t.Fatalf("save transcript artifact index: %v", err)
	}
	stems := domain.AudioStemArtifacts{
		ID:      uuid.NewString(),
		AssetID: assetID,
		Stems: []domain.AudioStem{
			{Type: domain.StemTypeBackground, AudioCASHash: mediaObj.SHA256},
			{Type: domain.StemTypeVocals, AudioCASHash: mediaObj.SHA256},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemsBytes, _ := json.Marshal(stems)
	stemsObj, _ := casStore.Put(bytes.NewReader(stemsBytes))
	_ = db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:        stems.ID,
		AssetID:   assetID,
		CASHash:   stemsObj.SHA256,
		CreatedAt: stems.CreatedAt,
	})
	return assetID, runID, stemsObj.SHA256
}

func TestAudioMixService_SeparateAudio_UsesNormalizedPreflightAudio_NotRawMP4(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()

	assetID := "asset_norm_test_" + uuid.NewString()[:8]
	runID := "run_norm_test_" + uuid.NewString()[:8]

	// 1. Put raw MP4 file in CAS as SourceAsset
	rawMP4Bytes := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00isommp42")
	mp4Obj, err := casStore.Put(bytes.NewReader(rawMP4Bytes))
	if err != nil {
		t.Fatalf("put mp4 in cas: %v", err)
	}
	attID := uuid.NewString()
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	err = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mp4Obj.SHA256,
		CASPath:             mp4Obj.Path,
		MimeType:            "video/mp4",
		ByteSize:            int64(len(rawMP4Bytes)),
		CreatedAt:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save source asset: %v", err)
	}

	// 2. Put valid normalized 16 kHz mono WAV in CAS as PreflightReport
	normWAVBytes := media.GeneratePCM16WAV(16000, 1, 2000)
	wavObj, err := casStore.Put(bytes.NewReader(normWAVBytes))
	if err != nil {
		t.Fatalf("put wav in cas: %v", err)
	}

	preflight := domain.PreflightReport{
		ID:                     "preflight_" + assetID,
		AssetID:                assetID,
		DurationSec:            2.0,
		DurationMs:             2000,
		AudioChannels:          1,
		AudioSampleRate:        16000,
		ContainerFormat:        "mp4",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  wavObj.SHA256,
		NormalizedAudioCASPath: wavObj.Path,
		CreatedAt:              time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, preflight); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}

	// 3. Execute SeparateAudio
	// If raw MP4 is passed to FakeSeparatorProvider, it fails closed because raw MP4 is not a valid 16-bit PCM WAV.
	// When fixed to use normalized preflight audio, it succeeds and uses wavObj.SHA256 as execution input hash.
	stems, err := mixSvc.SeparateAudio(ctx, service.AudioSeparationInput{
		RunID:   runID,
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("SeparateAudio failed: %v", err)
	}
	if stems == nil || len(stems.Stems) < 2 {
		t.Fatalf("expected valid stems, got %+v", stems)
	}

	// 4. Verify ProviderAttempt InputHash recorded by Router matches NormalizedAudioSHA256, not raw MP4 SHA256
	attempts, err := db.ListProviderAttempts(ctx, runID, string(provider.TypeSeparator))
	if err != nil {
		t.Fatalf("ListProviderAttempts failed: %v", err)
	}
	if len(attempts) == 0 {
		t.Fatalf("expected at least 1 provider attempt recorded")
	}
	lastAttempt := attempts[len(attempts)-1]
	if lastAttempt.InputHash != wavObj.SHA256 {
		t.Errorf("expected ProviderAttempt.InputHash to be normalized audio SHA %s, got %s (raw mp4 SHA was %s)",
			wavObj.SHA256, lastAttempt.InputHash, mp4Obj.SHA256)
	}
}

func TestAudioMixService_SeparateAudio_FailClosedWhenNormalizedEvidenceMissingOrUnreadable(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()

	assetID := "asset_failclosed_" + uuid.NewString()[:8]
	runID := "run_failclosed_" + uuid.NewString()[:8]

	rawMP4Bytes := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00isommp42")
	mp4Obj, err := casStore.Put(bytes.NewReader(rawMP4Bytes))
	if err != nil {
		t.Fatalf("put raw mp4 in cas: %v", err)
	}
	attID := uuid.NewString()
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mp4Obj.SHA256,
		CASPath:             mp4Obj.Path,
		MimeType:            "video/mp4",
		ByteSize:            int64(len(rawMP4Bytes)),
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	// Case 1: PreflightReport missing entirely -> fails closed
	_, err = mixSvc.SeparateAudio(ctx, service.AudioSeparationInput{
		RunID:   runID,
		AssetID: assetID,
	})
	if err == nil || !strings.Contains(err.Error(), "preflight report required for audio separation") {
		t.Fatalf("expected preflight report required error, got %v", err)
	}
	// Case 2: PreflightReport present but NormalizedAudioCASPath / SHA256 missing -> fails closed
	preflight := domain.PreflightReport{
		ID:              "preflight_" + assetID,
		AssetID:         assetID,
		DurationSec:     2.0,
		DurationMs:      2000,
		ContainerFormat: "mp4",
		ContainerValid:  true,
		CreatedAt:       time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, preflight); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}
	_, err = mixSvc.SeparateAudio(ctx, service.AudioSeparationInput{
		RunID:   runID,
		AssetID: assetID,
	})
	if err == nil || !strings.Contains(err.Error(), "normalized audio missing from preflight report") {
		t.Fatalf("expected normalized audio missing error, got %v", err)
	}
	// Case 3: NormalizedAudioCASPath points to nonexistent/unreadable file -> fails closed
	preflight.NormalizedAudioSHA256 = "dummy_sha256"
	preflight.NormalizedAudioCASPath = "F:/nonexistent/missing_normalized_audio.wav"
	if err := db.SavePreflightReport(ctx, preflight); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}
	_, err = mixSvc.SeparateAudio(ctx, service.AudioSeparationInput{
		RunID:   runID,
		AssetID: assetID,
	})
	if err == nil || !strings.Contains(err.Error(), "normalized audio artifact missing") {
		t.Fatalf("expected normalized audio artifact missing/unreadable error, got %v", err)
	}
}

func TestAudioMixService_PlaceableSegmentMissingAudio_Refused(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID, runID, stemsCAS := mixFixtureWithDubEligibleSpeech(t, db, casStore)
	rolePlan, err := db.GetAudioRolePlan(ctx, assetID)
	if err != nil {
		t.Fatalf("get role plan: %v", err)
	}
	transcriptIdx, err := db.GetTranscriptArtifactIndexByRun(ctx, runID)
	if err != nil {
		t.Fatalf("get transcript index: %v", err)
	}
	_, _, fitPolicyID := service.NewFitController().ResolvePlaybackWindow(3000, 0)
	if fitPolicyID == "" {
		t.Fatal("expected current fit policy identity")
	}
	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", ProvenanceHash: "prov-dub-script-" + runID,
		Segments:  []domain.DubScriptSegment{{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000, SlotDurationMs: 2000, SourceText: "测试", MeaningText: "kiểm tra", SpokenText: "kiểm tra", PassedQAGate: true}},
		CreatedAt: time.Now().UTC(),
	}
	dubScriptBytes, _ := json.Marshal(dubScript)
	dubScriptObj, err := casStore.Put(bytes.NewReader(dubScriptBytes))
	if err != nil {
		t.Fatalf("put dub script: %v", err)
	}
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dubScriptObj.SHA256,
		ProvenanceHash: dubScript.ProvenanceHash, CreatedAt: dubScript.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}
	voiceAssignment := domain.VoiceAssignment{
		ID: uuid.NewString(), SchemaVersion: domain.VoiceAssignmentSchemaVersion, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		Assignments:         map[string]domain.VoiceProfile{"SPEAKER_00": {ID: "voice-test", ProviderID: "fake_vieneu_tts_vi", VoiceID: "vi_f1", Language: "vi"}},
		DubScriptVariantCAS: dubScriptObj.SHA256, TranscriptArtifactCAS: transcriptIdx.CASHash,
		ProvenanceHash: "prov-voice-" + runID, FrozenAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	voiceBytes, _ := json.Marshal(voiceAssignment)
	voiceObj, err := casStore.Put(bytes.NewReader(voiceBytes))
	if err != nil {
		t.Fatalf("put voice assignment: %v", err)
	}
	assignmentsJSON, _ := json.Marshal(voiceAssignment.Assignments)
	if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: voiceAssignment.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: voiceObj.SHA256,
		ProvenanceHash: voiceAssignment.ProvenanceHash, AssignmentsJSON: string(assignmentsJSON), CreatedAt: voiceAssignment.CreatedAt,
	}); err != nil {
		t.Fatalf("save voice assignment index: %v", err)
	}

	// Segment within suppression window [0, 35000ms] whose audio is missing from CAS
	dubSegments := domain.DubSegmentsVariant{
		ID:                    uuid.NewString(),
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   dubScriptObj.SHA256,
		VoiceAssignmentCAS:    voiceObj.SHA256,
		TranscriptArtifactCAS: transcriptIdx.CASHash,
		AudioRolePlanCAS:      rolePlan.CASHash,
		FitPolicyID:           fitPolicyID,
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            1000,
				EndMs:              3000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 1800,
				AudioSHA256:        "sha256_nonexistent_audio_clip_that_cannot_be_read",
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   3000,
			},
		},
		FitPlans: []domain.DubbingFitPlan{
			{
				SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2000, UsableSlotMs: 2000,
				MeasuredDurationMs: 1800, DubPlaybackEndMs: 3000, FitPolicyID: fitPolicyID,
				SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubBytes, _ := json.Marshal(dubSegments)
	dubObj, _ := casStore.Put(bytes.NewReader(dubBytes))

	mix, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		DubSegmentsCAS: dubObj.SHA256,
		AudioStemsCAS:  stemsCAS,
	})
	if err == nil {
		t.Fatalf("expected ErrMixerOverrunRefused when placeable segment audio is missing, got PASS mix: %+v", mix)
	}
	if !errors.Is(err, domain.ErrMixerOverrunRefused) {
		t.Fatalf("expected ErrMixerOverrunRefused, got %v", err)
	}
	if mix == nil || mix.OverallStatus != "REFUSED" {
		t.Fatalf("expected a REFUSED dub mix artifact, got %+v", mix)
	}
	if !strings.Contains(mix.RefusalReason, "segment 0") {
		t.Errorf("refusal reason must name the segment index, got %q", mix.RefusalReason)
	}
}

func TestAudioMixService_DubEligibleWithUnreadableDubSegmentsCAS_Refused(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID, runID, stemsCAS := mixFixtureWithDubEligibleSpeech(t, db, casStore)

	// Role plan is dub-eligible (narration dialogue [0, 35000ms]), and DubSegmentsCAS is named but unreadable in CAS
	mix, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		DubSegmentsCAS: "sha256_unreadable_missing_dub_segments_cas",
		AudioStemsCAS:  stemsCAS,
	})
	if err == nil {
		t.Fatalf("expected ErrMixerOverrunRefused when dub-eligible plan has unreadable dub artifact, got PASS mix: %+v", mix)
	}
	if !errors.Is(err, domain.ErrMixerOverrunRefused) {
		t.Fatalf("expected ErrMixerOverrunRefused, got %v", err)
	}
	if mix == nil || mix.OverallStatus != "REFUSED" {
		t.Fatalf("expected a REFUSED dub mix artifact, got %+v", mix)
	}
	if !strings.Contains(mix.RefusalReason, "unreadable") {
		t.Errorf("expected refusal reason to mention unreadable CAS, got %q", mix.RefusalReason)
	}
}

func TestAudioMixService_DubEligibleRejectsLatestIndexedDubSegmentsFallback(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID, runID, stemsCAS := mixFixtureWithDubEligibleSpeech(t, db, casStore)

	unreadableCAS := "sha256_unreadable_from_db_index"
	err := db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             "dubseg-unreadable-idx",
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
		CASHash:        unreadableCAS,
		ProvenanceHash: "prov-unreadable",
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save dub segments index: %v", err)
	}

	mix, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		DubSegmentsCAS: "",
		AudioStemsCAS:  stemsCAS,
	})
	if err == nil {
		t.Fatalf("expected ErrMixerOverrunRefused when dub-eligible plan has unreadable dub index CAS, got PASS mix: %+v", mix)
	}
	if !errors.Is(err, domain.ErrMixerOverrunRefused) {
		t.Fatalf("expected ErrMixerOverrunRefused, got %v", err)
	}
	if mix == nil || mix.OverallStatus != "REFUSED" {
		t.Fatalf("expected a REFUSED dub mix artifact, got %+v", mix)
	}
	if !strings.Contains(mix.RefusalReason, "dub_segments_cas is required") || !strings.Contains(mix.RefusalReason, "latest indexed artifacts are not accepted") {
		t.Errorf("expected refusal reason to require exact pinned dub CAS, got %q", mix.RefusalReason)
	}
}

func computeLegacyAudioStemsProvenanceHashV1(assetID, providerID, modelName, modelVersion string) string {
	payload := struct {
		AssetID      string `json:"asset_id"`
		ProviderID   string `json:"provider_id"`
		ModelName    string `json:"model_name"`
		ModelVersion string `json:"model_version"`
		SchemaVer    int    `json:"schema_version"`
	}{
		AssetID:      assetID,
		ProviderID:   providerID,
		ModelName:    modelName,
		ModelVersion: modelVersion,
		SchemaVer:    1,
	}
	b, _ := json.Marshal(payload)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestAudioMixService_SeparateAudio_SchemaVersion2_BypassesLegacyV1Cache(t *testing.T) {
	mixSvc, db, casStore, reg, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()

	assetID := "asset_schema_bump_" + uuid.NewString()[:8]
	runID := "run_schema_bump_" + uuid.NewString()[:8]

	dummyMedia := []byte("DUMMY_AUDIO_DATA_FOR_SCHEMA_TEST")
	mediaObj, _ := casStore.Put(bytes.NewReader(dummyMedia))
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              mediaObj.SHA256,
		CASPath:             mediaObj.Path,
		ByteSize:            int64(len(dummyMedia)),
		CreatedAt:           time.Now().UTC(),
	})

	normWAVBytes := media.GeneratePCM16WAV(16000, 1, 2000)
	wavObj, _ := casStore.Put(bytes.NewReader(normWAVBytes))
	_ = db.SavePreflightReport(ctx, domain.PreflightReport{
		ID:                     "preflight_" + assetID,
		AssetID:                assetID,
		DurationSec:            2.0,
		DurationMs:             2000,
		AudioChannels:          1,
		AudioSampleRate:        16000,
		ContainerFormat:        "mp4",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  wavObj.SHA256,
		NormalizedAudioCASPath: wavObj.Path,
		CreatedAt:              time.Now().UTC(),
	})

	p := reg.ListAll()[0]
	mName, mVer := p.ModelInfo()
	legacyProvHash := computeLegacyAudioStemsProvenanceHashV1(assetID, p.ID(), mName, mVer)

	legacyArtifact := domain.AudioStemArtifacts{
		ID:            "legacy_stems_v1",
		SchemaVersion: 1,
		AssetID:       assetID,
		ProviderID:    p.ID(),
		ModelName:     mName,
		ModelVersion:  mVer,
		Stems: []domain.AudioStem{
			{
				Type:       domain.StemTypeVocals,
				SampleRate: 44100,
				Channels:   2,
				Format:     "wav",
				DurationMs: 2000,
			},
			{
				Type:       domain.StemTypeBackground,
				SampleRate: 44100,
				Channels:   2,
				Format:     "wav",
				DurationMs: 2000,
			},
		},
		ProvenanceHash: legacyProvHash,
		CreatedAt:      time.Now().UTC(),
	}
	legacyBytes, _ := json.Marshal(legacyArtifact)
	legacyObj, _ := casStore.Put(bytes.NewReader(legacyBytes))

	err := db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:             legacyArtifact.ID,
		AssetID:        assetID,
		ProviderID:     p.ID(),
		ModelName:      mName,
		ModelVersion:   mVer,
		CASHash:        legacyObj.SHA256,
		ProvenanceHash: legacyProvHash,
		CreatedAt:      legacyArtifact.CreatedAt,
	})
	if err != nil {
		t.Fatalf("save legacy stems index: %v", err)
	}

	stems, err := mixSvc.SeparateAudio(ctx, service.AudioSeparationInput{
		RunID:   runID,
		AssetID: assetID,
	})
	if err != nil {
		t.Fatalf("SeparateAudio failed: %v", err)
	}
	if stems.SchemaVersion != 2 {
		t.Fatalf("expected stems.SchemaVersion == 2, got %d (stale cache replayed)", stems.SchemaVersion)
	}
	if stems.Stems[0].SampleRate == 44100 {
		t.Fatalf("expected contract 16000 Hz stems, got stale 44100 Hz from cache")
	}
}

func TestAudioMixService_FrozenFitControllerPolicyRespected(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID := "asset_custom_fit_" + uuid.NewString()[:8]
	runID := "run-mix-custom-fit"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "mock-sha", ByteSize: 1024, CreatedAt: time.Now().UTC(),
	})
	// Setup custom FitControllerConfig
	customCfg := service.DefaultFitControllerConfig()
	customCfg.ReserveRatio = 0.20
	customCfg.PolicyVersion = "playback-custom-v1"
	customFit := service.NewFitController(customCfg)
	_, _, customPolicyID := customFit.ResolvePlaybackWindow(1000, 2000)

	// Stems artifact
	bgWAV := media.GeneratePCM16WAV(16000, 1, 5000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgWAV))
	stems := domain.AudioStemArtifacts{
		ID:            "stems-" + runID,
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   5000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stems)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))

	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + runID,
		AssetID:        assetID,
		Segments:       []domain.AudioSegment{{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue}},
		ProvenanceHash: "prov-role-" + runID,
		CreatedAt:      time.Now().UTC(),
	}
	rpBytes, _ := json.Marshal(rolePlan)
	rpObj, _ := casStore.Put(bytes.NewReader(rpBytes))
	rolePlan.CASHash = rpObj.SHA256
	_ = db.SaveAudioRolePlan(ctx, rolePlan)

	// Transcript
	transcript := domain.TranscriptArtifact{
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 1000, EndMs: 3000, SourceText: "hello", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
		},
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))

	// Dub script
	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", ProvenanceHash: "prov-ds-" + runID,
		Segments:  []domain.DubScriptSegment{{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000, SlotDurationMs: 2000, SourceText: "hello", SpokenText: "xin chào"}},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScript)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScript.ProvenanceHash, CreatedAt: dubScript.CreatedAt,
	})

	// Voice assignment
	va := domain.VoiceAssignment{
		ID: uuid.NewString(), SchemaVersion: domain.VoiceAssignmentSchemaVersion, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		Assignments:           map[string]domain.VoiceProfile{"SPEAKER_00": {ID: "v1", Language: "vi"}},
		DubScriptVariantCAS:   dsObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		ProvenanceHash:        "prov-va-" + runID,
		CreatedAt:             time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	_ = db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: va.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: vaObj.SHA256,
		ProvenanceHash: va.ProvenanceHash, CreatedAt: va.CreatedAt,
	})

	clipWAV := media.GeneratePCM16WAV(16000, 1, 2000)
	clipObj, _ := casStore.Put(bytes.NewReader(clipWAV))

	// 1. DubSegmentsVariant carrying custom FitConfig and matching FitPolicyID
	dubVariant := domain.DubSegmentsVariant{
		ID:                    "dub-custom-fit",
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   dsObj.SHA256,
		VoiceAssignmentCAS:    vaObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		AudioRolePlanCAS:      rpObj.SHA256,
		FitPolicyID:           customPolicyID,
		FitConfig:             &customCfg,
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            1000,
				EndMs:              3000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 2000,
				AudioSHA256:        clipObj.SHA256,
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   3000,
			},
		},
		FitPlans: []domain.DubbingFitPlan{
			{
				SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2000, UsableSlotMs: 2000,
				MeasuredDurationMs: 2000, DubPlaybackEndMs: 3000, FitPolicyID: customPolicyID,
				SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dvBytes, _ := json.Marshal(dubVariant)
	dvObj, _ := casStore.Put(bytes.NewReader(dvBytes))

	// Mix with custom fit policy: must PASS
	mixArtifact, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  sObj.SHA256,
		DubSegmentsCAS: dvObj.SHA256,
	})
	if err != nil || mixArtifact == nil || mixArtifact.OverallStatus != "PASS" {
		t.Fatalf("expected custom fit policy to be accepted by mixer, got err=%v mix=%+v", err, mixArtifact)
	}

	// 2. Modified FitPolicyID mismatching the FitConfig: must REFUSE
	dubVariantTampered := dubVariant
	dubVariantTampered.ID = "dub-custom-fit-tampered"
	dubVariantTampered.FitPolicyID = "tampered-policy-id"
	dvtBytes, _ := json.Marshal(dubVariantTampered)
	dvtObj, _ := casStore.Put(bytes.NewReader(dvtBytes))

	mixTampered, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  sObj.SHA256,
		DubSegmentsCAS: dvtObj.SHA256,
	})
	if err == nil || mixTampered != nil && mixTampered.OverallStatus == "PASS" {
		t.Fatal("expected tampered policy ID to be refused by mixer, got PASS")
	}
}

func TestAudioMixService_CASAuthorityAndPathBypassForbidden(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID := "asset_cas_auth_" + uuid.NewString()[:8]
	runID := "run-cas-auth"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "mock-sha", ByteSize: 1024, CreatedAt: time.Now().UTC(),
	})

	bgWAV := media.GeneratePCM16WAV(16000, 1, 5000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgWAV))
	stems := domain.AudioStemArtifacts{
		ID:            "stems-" + runID,
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   5000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stems)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))

	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + runID,
		AssetID:        assetID,
		Segments:       []domain.AudioSegment{{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue}},
		ProvenanceHash: "prov-role-" + runID,
		CreatedAt:      time.Now().UTC(),
	}
	rpBytes, _ := json.Marshal(rolePlan)
	rpObj, _ := casStore.Put(bytes.NewReader(rpBytes))
	rolePlan.CASHash = rpObj.SHA256
	_ = db.SaveAudioRolePlan(ctx, rolePlan)

	transcript := domain.TranscriptArtifact{
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 1000, EndMs: 3000, SourceText: "hello", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
		},
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))

	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", ProvenanceHash: "prov-ds-" + runID,
		Segments:  []domain.DubScriptSegment{{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 1000, EndMs: 3000, SlotDurationMs: 2000, SourceText: "hello", SpokenText: "xin chào"}},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScript)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScript.ProvenanceHash, CreatedAt: dubScript.CreatedAt,
	})

	va := domain.VoiceAssignment{
		ID: uuid.NewString(), SchemaVersion: domain.VoiceAssignmentSchemaVersion, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		Assignments:           map[string]domain.VoiceProfile{"SPEAKER_00": {ID: "v1", Language: "vi"}},
		DubScriptVariantCAS:   dsObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		ProvenanceHash:        "prov-va-" + runID,
		CreatedAt:             time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	_ = db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: va.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: vaObj.SHA256,
		ProvenanceHash: va.ProvenanceHash, CreatedAt: va.CreatedAt,
	})

	_, _, fitPolicyID := service.NewFitController().ResolvePlaybackWindow(1000, 2000)
	clipWAV := media.GeneratePCM16WAV(16000, 1, 2000)
	clipObj, _ := casStore.Put(bytes.NewReader(clipWAV))

	// 1. Missing AudioSHA256 must be refused (cannot rely on AudioCASPath alone)
	dubVariantNoSHA := domain.DubSegmentsVariant{
		ID:                    "dub-no-sha",
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   dsObj.SHA256,
		VoiceAssignmentCAS:    vaObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		AudioRolePlanCAS:      rpObj.SHA256,
		FitPolicyID:           fitPolicyID,
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            1000,
				EndMs:              3000,
				SlotDurationMs:     2000,
				MeasuredDurationMs: 2000,
				AudioSHA256:        "", // empty hash
				AudioCASPath:       clipObj.Path,
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   3000,
			},
		},
		FitPlans: []domain.DubbingFitPlan{
			{
				SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2000, UsableSlotMs: 2000,
				MeasuredDurationMs: 2000, DubPlaybackEndMs: 3000, FitPolicyID: fitPolicyID,
				SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dnsBytes, _ := json.Marshal(dubVariantNoSHA)
	dnsObj, _ := casStore.Put(bytes.NewReader(dnsBytes))

	_, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  sObj.SHA256,
		DubSegmentsCAS: dnsObj.SHA256,
	})
	if err == nil {
		t.Fatal("expected missing AudioSHA256 to be refused by mixer, got PASS")
	}

	// 2. Mismatched AudioSHA256 with readable path must fail closed on hash mismatch (cannot bypass pinned CAS)
	dubVariantBypass := dubVariantNoSHA
	dubVariantBypass.ID = "dub-bypass-attempt"
	dubVariantBypass.Segments[0].AudioSHA256 = "corrupt_sha_256_attempting_to_bypass_with_valid_path"
	dubVariantBypass.Segments[0].AudioCASPath = clipObj.Path
	dbpBytes, _ := json.Marshal(dubVariantBypass)
	dbpObj, _ := casStore.Put(bytes.NewReader(dbpBytes))

	_, err = mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  sObj.SHA256,
		DubSegmentsCAS: dbpObj.SHA256,
	})
	if err == nil || !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "audio load or decode failure") {
		t.Fatalf("expected hash mismatch failure when trying to bypass CAS with path, got %v", err)
	}
}

func TestAudioMixService_GroupedSegmentStartingAt0ms_Accepted(t *testing.T) {
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	ctx := context.Background()
	assetID := "asset_grouped_0ms_" + uuid.NewString()[:8]
	runID := "run-mix-grouped-0ms"

	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "mock-sha", ByteSize: 1024, CreatedAt: time.Now().UTC(),
	})

	fitCtrl := service.NewFitController(service.DefaultFitControllerConfig())
	expectedEnd, expectedReserve, fitPolicyID := fitCtrl.ResolvePlaybackWindow(2500, 0)

	bgWAV := media.GeneratePCM16WAV(16000, 1, 5000)
	bgObj, _ := casStore.Put(bytes.NewReader(bgWAV))
	stems := domain.AudioStemArtifacts{
		ID:            "stems-" + runID,
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   5000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	sBytes, _ := json.Marshal(stems)
	sObj, _ := casStore.Put(bytes.NewReader(sBytes))

	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + runID,
		AssetID:        assetID,
		Segments:       []domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}},
		ProvenanceHash: "prov-role-" + runID,
		CreatedAt:      time.Now().UTC(),
	}
	rpBytes, _ := json.Marshal(rolePlan)
	rpObj, _ := casStore.Put(bytes.NewReader(rpBytes))
	rolePlan.CASHash = rpObj.SHA256
	_ = db.SaveAudioRolePlan(ctx, rolePlan)

	// Transcript with 2 contiguous blocks, first starting at 0ms
	transcript := domain.TranscriptArtifact{
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: 0, EndMs: 1000, SourceText: "first", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
			{Index: 1, StartMs: 1200, EndMs: 2500, SourceText: "second", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
		},
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))

	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", ProvenanceHash: "prov-ds-" + runID,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2500, SlotDurationMs: 2500, SourceText: "first second", SpokenText: "thứ nhất thứ hai"},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScript)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	_ = db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScript.ProvenanceHash, CreatedAt: dubScript.CreatedAt,
	})

	va := domain.VoiceAssignment{
		ID: uuid.NewString(), SchemaVersion: domain.VoiceAssignmentSchemaVersion, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		Assignments:           map[string]domain.VoiceProfile{"SPEAKER_00": {ID: "v1", Language: "vi"}},
		DubScriptVariantCAS:   dsObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		ProvenanceHash:        "prov-va-" + runID,
		CreatedAt:             time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	_ = db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: va.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: vaObj.SHA256,
		ProvenanceHash: va.ProvenanceHash, CreatedAt: va.CreatedAt,
	})

	clipWAV := media.GeneratePCM16WAV(16000, 1, 2200)
	clipObj, _ := casStore.Put(bytes.NewReader(clipWAV))

	dubVariant := domain.DubSegmentsVariant{
		ID:                    "dub-grouped-0ms",
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   dsObj.SHA256,
		VoiceAssignmentCAS:    vaObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		AudioRolePlanCAS:      rpObj.SHA256,
		FitPolicyID:           fitPolicyID,
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0, 1},
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              2500,
				SlotDurationMs:     2500,
				MeasuredDurationMs: 2200,
				AudioSHA256:        clipObj.SHA256,
				AudioCASPath:       clipObj.Path,
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   expectedEnd,
				EffectiveReserveMs: expectedReserve,
			},
		},
		FitPlans: []domain.DubbingFitPlan{
			{
				SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: 2500, UsableSlotMs: 2500,
				MeasuredDurationMs: 2200, DubPlaybackEndMs: expectedEnd, EffectiveReserveMs: expectedReserve,
				FitPolicyID: fitPolicyID, SpeechBlockIndices: []int{0, 1}, Decision: domain.FitActionAccept,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dvBytes, _ := json.Marshal(dubVariant)
	dvObj, _ := casStore.Put(bytes.NewReader(dvBytes))

	mix, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  sObj.SHA256,
		DubSegmentsCAS: dvObj.SHA256,
	})
	if err != nil {
		t.Fatalf("expected grouped dub starting at 0ms to be accepted, got error: %v", err)
	}
	if mix == nil || mix.OverallStatus != "PASS" {
		t.Fatalf("expected mix OverallStatus PASS, got %+v", mix)
	}
}

// frameExactFixture pairs the mixer's frame-exact window with the fit evidence for the same
// clip, so one fixture can drive both sides of the acceptance rule.
type frameExactFixture struct {
	assetID        string
	runID          string
	stemsCAS       string
	dubSegmentsCAS string
	clipProbeMs    int64
	sampleRate     int
	speechStartMs  int64
	playbackEndMs  int64
	reserveMs      int64
	policyID       string
}

// buildFrameExactFixture builds an asset whose single dub-eligible speech window is
// [speechStartMs, speechEndMs] mixed at 44.1kHz, with one accepted dub clip of exactly
// clipFrames output frames pinned in CAS.
func buildFrameExactFixture(t *testing.T, db *storage.DB, casStore *cas.Store, clipFrames, speechStartMs, speechEndMs int64) frameExactFixture {
	t.Helper()
	ctx := context.Background()
	const sampleRate = 44100
	assetID := "asset_frames_" + uuid.NewString()[:8]
	runID := "run-frames-" + uuid.NewString()[:8]

	attID := uuid.NewString()
	if err := db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", TermsAccepted: true, ConfirmedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID: assetID, RightsAttestationID: attID, SHA256: "mock-sha-" + assetID, ByteSize: 1024, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	fitCtrl := service.NewFitController()
	playbackEndMs, reserveMs, policyID := fitCtrl.ResolvePlaybackWindow(speechEndMs, 0)
	if playbackEndMs != speechEndMs || reserveMs != 0 {
		t.Fatalf("fixture expects an unborrowed window ending at %d, got end %d reserve %d", speechEndMs, playbackEndMs, reserveMs)
	}

	bgObj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(sampleRate, 1, 5000)))
	if err != nil {
		t.Fatalf("put background stem: %v", err)
	}
	stems := domain.AudioStemArtifacts{
		ID:            "stems-" + runID,
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{Type: domain.StemTypeBackground, AudioCASHash: bgObj.SHA256, SampleRate: sampleRate, Channels: 1, Format: "wav", DurationMs: 5000},
			{Type: domain.StemTypeVocals, AudioCASHash: bgObj.SHA256, SampleRate: sampleRate, Channels: 1, Format: "wav", DurationMs: 5000},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemsBytes, _ := json.Marshal(stems)
	stemsObj, _ := casStore.Put(bytes.NewReader(stemsBytes))

	rolePlan := domain.AudioRolePlan{
		ID:             "role-" + runID,
		AssetID:        assetID,
		Segments:       []domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}},
		ProvenanceHash: "prov-role-" + runID,
		CreatedAt:      time.Now().UTC(),
	}
	roleBytes, _ := json.Marshal(rolePlan)
	roleObj, _ := casStore.Put(bytes.NewReader(roleBytes))
	rolePlan.CASHash = roleObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, rolePlan); err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}

	transcript := domain.TranscriptArtifact{
		AssetID: assetID,
		SpeechBlocks: []domain.SpeechBlock{
			{Index: 0, StartMs: speechStartMs, EndMs: speechEndMs, SourceText: "测试", SpeakerID: "SPEAKER_00", SegmentType: domain.SpeechBlockTypeSpeech},
		},
	}
	tBytes, _ := json.Marshal(transcript)
	tObj, _ := casStore.Put(bytes.NewReader(tBytes))

	dubScript := domain.DubScriptVariant{
		ID: uuid.NewString(), SchemaVersion: domain.DubScriptSchemaVersion, AssetID: assetID, RunID: runID,
		SourceLanguage: "zh", TargetLanguage: "vi", ProvenanceHash: "prov-ds-" + runID,
		Segments: []domain.DubScriptSegment{
			{Index: 0, SpeakerID: "SPEAKER_00", StartMs: speechStartMs, EndMs: speechEndMs, SlotDurationMs: speechEndMs - speechStartMs, SourceText: "测试", SpokenText: "thử"},
		},
		CreatedAt: time.Now().UTC(),
	}
	dsBytes, _ := json.Marshal(dubScript)
	dsObj, _ := casStore.Put(bytes.NewReader(dsBytes))
	if err := db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
		ID: dubScript.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: dsObj.SHA256,
		ProvenanceHash: dubScript.ProvenanceHash, CreatedAt: dubScript.CreatedAt,
	}); err != nil {
		t.Fatalf("save dub script index: %v", err)
	}

	va := domain.VoiceAssignment{
		ID: uuid.NewString(), SchemaVersion: domain.VoiceAssignmentSchemaVersion, AssetID: assetID, RunID: runID, TargetLanguage: "vi",
		Assignments:           map[string]domain.VoiceProfile{"SPEAKER_00": {ID: "v1", Language: "vi"}},
		DubScriptVariantCAS:   dsObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		ProvenanceHash:        "prov-va-" + runID,
		CreatedAt:             time.Now().UTC(),
	}
	vaBytes, _ := json.Marshal(va)
	vaObj, _ := casStore.Put(bytes.NewReader(vaBytes))
	if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID: va.ID, AssetID: assetID, RunID: runID, TargetLanguage: "vi", CASHash: vaObj.SHA256,
		ProvenanceHash: va.ProvenanceHash, CreatedAt: va.CreatedAt,
	}); err != nil {
		t.Fatalf("save voice assignment index: %v", err)
	}

	clipWAV := media.EncodePCM16Samples(make([]int16, clipFrames), sampleRate, 1)
	clipProbeMs, err := media.ProbeWAVBytes(clipWAV)
	if err != nil {
		t.Fatalf("probe clip fixture: %v", err)
	}
	clipObj, err := casStore.Put(bytes.NewReader(clipWAV))
	if err != nil {
		t.Fatalf("put clip: %v", err)
	}

	dubVariant := domain.DubSegmentsVariant{
		ID:                    "dub-frames-" + runID,
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               assetID,
		RunID:                 runID,
		TargetLanguage:        "vi",
		DubScriptVariantCAS:   dsObj.SHA256,
		VoiceAssignmentCAS:    vaObj.SHA256,
		TranscriptArtifactCAS: tObj.SHA256,
		AudioRolePlanCAS:      rolePlan.CASHash,
		FitPolicyID:           policyID,
		OverallStatus:         "PASS",
		Segments: []domain.DubSegment{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            speechStartMs,
				EndMs:              speechEndMs,
				SlotDurationMs:     speechEndMs - speechStartMs,
				MeasuredDurationMs: clipProbeMs,
				AudioSHA256:        clipObj.SHA256,
				AudioCASPath:       clipObj.Path,
				FitDecision:        domain.FitActionAccept,
				DubPlaybackEndMs:   playbackEndMs,
				EffectiveReserveMs: reserveMs,
			},
		},
		FitPlans: []domain.DubbingFitPlan{
			{
				SegmentIndex: 0, SpeakerID: "SPEAKER_00", SlotDurationMs: speechEndMs - speechStartMs,
				UsableSlotMs: speechEndMs - speechStartMs, MeasuredDurationMs: clipProbeMs,
				DubPlaybackEndMs: playbackEndMs, EffectiveReserveMs: reserveMs,
				FitPolicyID: policyID, SpeechBlockIndices: []int{0}, Decision: domain.FitActionAccept,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dvBytes, _ := json.Marshal(dubVariant)
	dvObj, _ := casStore.Put(bytes.NewReader(dvBytes))

	return frameExactFixture{
		assetID: assetID, runID: runID, stemsCAS: stemsObj.SHA256, dubSegmentsCAS: dvObj.SHA256,
		clipProbeMs: clipProbeMs, sampleRate: sampleRate, speechStartMs: speechStartMs,
		playbackEndMs: playbackEndMs, reserveMs: reserveMs, policyID: policyID,
	}
}

func (f frameExactFixture) mix(t *testing.T, mixSvc *service.AudioMixService) (*domain.DubMixArtifact, error) {
	t.Helper()
	return mixSvc.MixAudio(context.Background(), service.AudioMixInput{
		RunID:          f.runID,
		AssetID:        f.assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  f.stemsCAS,
		DubSegmentsCAS: f.dubSegmentsCAS,
	})
}

func (f frameExactFixture) fitInput(measuredFrames int64) service.FitEvaluationInput {
	return service.FitEvaluationInput{
		SegmentIndex:       0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            f.speechStartMs,
		EndMs:              f.playbackEndMs,
		DubPlaybackEndMs:   f.playbackEndMs,
		EffectiveReserveMs: f.reserveMs,
		MeasuredDurationMs: f.clipProbeMs,
		MeasuredFrames:     measuredFrames,
		MeasuredSampleRate: f.sampleRate,
		OutputSampleRate:   f.sampleRate,
		AttemptNumber:      3,
		FixedRateVoice:     true,
	}
}

// The fit and the mixer must agree at the frame-exact window boundary: the fit may only ACCEPT
// what the frame-exact mixer will place.
func TestAudioMixAndFit_AgreeOnFrameExactPlaybackWindow(t *testing.T) {
	ctx := context.Background()
	const windowFrames = 44100 // 1000ms at 44.1kHz

	// 1. A clip that fills the window exactly is mixed, and the fit accepts it from exact geometry.
	mixSvc, db, casStore, _, _ := setupAudioMixTestHarness(t)
	fitted := buildFrameExactFixture(t, db, casStore, windowFrames, 1000, 2000)
	mix, err := fitted.mix(t, mixSvc)
	if err != nil {
		t.Fatalf("a clip exactly filling the window must mix, got error: %v", err)
	}
	if mix == nil || mix.OverallStatus != "PASS" {
		t.Fatalf("expected mixed PASS artifact, got %+v", mix)
	}
	fitRes := service.NewFitController().EvaluateCandidate(ctx, fitted.fitInput(windowFrames))
	if fitRes.Decision != domain.FitActionAccept {
		t.Fatalf("fit must ACCEPT the exactly-placeable clip: %+v", fitRes)
	}

	// 2. One frame past the window: the mixer refuses it, and the fit must not hand it over. Its
	// floored probe still reads 1000ms, which is the whole slot - the pre-fix fit accepted on that
	// probe alone.
	overflowing := buildFrameExactFixture(t, db, casStore, windowFrames+1, 1000, 2000)
	if overflowing.clipProbeMs != 1000 {
		t.Fatalf("one frame more must still floor to a 1000ms probe, got %dms", overflowing.clipProbeMs)
	}
	_, err = overflowing.mix(t, mixSvc)
	if !errors.Is(err, domain.ErrMixerOverrunRefused) {
		t.Fatalf("expected the frame-exact mixer to refuse one frame past the window, got %v", err)
	}
	fitRes = service.NewFitController().EvaluateCandidate(ctx, overflowing.fitInput(windowFrames+1))
	if fitRes.Decision == domain.FitActionAccept {
		t.Fatalf("fit must not ACCEPT a clip the mixer refuses: %+v", fitRes)
	}

	// 3. The same fixture without exact geometry keeps the conservative probe bound, which cannot
	// prove the 1ms-boundary candidate either.
	probeOnly := overflowing.fitInput(windowFrames + 1)
	probeOnly.MeasuredFrames = 0
	probeOnly.MeasuredSampleRate = 0
	if res := service.NewFitController().EvaluateCandidate(ctx, probeOnly); res.Decision == domain.FitActionAccept {
		t.Fatalf("a floored probe equal to the window must not ACCEPT: %+v", res)
	}
}
