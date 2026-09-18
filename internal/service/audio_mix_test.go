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
	_ = db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{
		AssetID: assetID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	})
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

	// Segment within suppression window [0, 35000ms] whose audio is missing from CAS
	dubSegments := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: "vi",
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
