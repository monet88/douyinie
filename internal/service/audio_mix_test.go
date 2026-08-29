package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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

	// Create stems in CAS
	bgObj, _ := casStore.Put(bytes.NewReader(dummyMedia))
	stemsArtifact := domain.AudioStemArtifacts{
		ID:            "stems_" + assetID,
		SchemaVersion: domain.AudioStemsSchemaVersion,
		AssetID:       assetID,
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				SampleRate:   16000,
				Channels:     1,
				DurationMs:   3000,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	stemsBytes, _ := json.Marshal(stemsArtifact)
	stemsObj, _ := casStore.Put(bytes.NewReader(stemsBytes))

	mixArtifact, err := mixSvc.MixAudio(ctx, service.AudioMixInput{
		RunID:          runID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		AudioStemsCAS:  stemsObj.SHA256,
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
}
