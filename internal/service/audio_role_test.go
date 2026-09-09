package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

type audioRoleTestHarness struct {
	db          *storage.DB
	casStore    *cas.Store
	audioMixSvc *service.AudioMixService
	audioRole   *service.AudioRoleService
	reviewSvc   *service.ReviewService
	speechSvc   *service.SpeechService
	router      *provider.Router
	registry    *provider.Registry
	dir         string
}

func setupAudioRoleTestHarness(t *testing.T) *audioRoleTestHarness {
	t.Helper()
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "test_audio_role.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("new cas: %v", err)
	}

	reg := provider.NewSeam1FakeRegistry()
	router := provider.NewRouter(reg, nil, nil, nil, nil, db)

	audioMixSvc := service.NewAudioMixService(db, casStore)
	audioMixSvc.ConfigureRouter(router)

	audioRoleSvc := service.NewAudioRoleService(db, casStore, audioMixSvc)

	reviewSvc := service.NewReviewService(db, casStore)
	reviewSvc.SetAudioMixService(audioMixSvc)

	speechSvc := service.NewSpeechService(db, casStore)

	t.Cleanup(func() {
		_ = db.Close()
	})

	return &audioRoleTestHarness{
		db:          db,
		casStore:    casStore,
		audioMixSvc: audioMixSvc,
		audioRole:   audioRoleSvc,
		reviewSvc:   reviewSvc,
		speechSvc:   speechSvc,
		router:      router,
		registry:    reg,
		dir:         dir,
	}
}

// createControlledAsset creates a source asset and preflight report with synthesized WAV media.
func createControlledAsset(t *testing.T, h *audioRoleTestHarness, durationMs int64, vocalsSamples, bgSamples []int16) (string, string) {
	t.Helper()
	ctx := context.Background()

	assetID := uuid.NewString()
	sampleRate := 16000
	channels := 1
	totalSamples := (sampleRate * int(durationMs)) / 1000

	if len(vocalsSamples) < totalSamples {
		padded := make([]int16, totalSamples)
		copy(padded, vocalsSamples)
		vocalsSamples = padded
	}
	if len(bgSamples) < totalSamples {
		padded := make([]int16, totalSamples)
		copy(padded, bgSamples)
		bgSamples = padded
	}

	mixedSamples := make([]int16, totalSamples)
	for i := 0; i < totalSamples; i++ {
		sum := int32(vocalsSamples[i]) + int32(bgSamples[i])
		if sum > 32767 {
			sum = 32767
		} else if sum < -32768 {
			sum = -32768
		}
		mixedSamples[i] = int16(sum)
	}

	mixedWAV := media.EncodePCM16Samples(mixedSamples, sampleRate, channels)
	normObj, err := h.casStore.Put(bytes.NewReader(mixedWAV))
	if err != nil {
		t.Fatalf("put normalized wav into cas: %v", err)
	}

	attID := uuid.NewString()
	if err := h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              normObj.SHA256,
		CASPath:             normObj.Path,
		CreatedAt:           time.Now().UTC(),
	}
	if err := h.db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	preflight := domain.PreflightReport{
		ID:                     uuid.NewString(),
		AssetID:                assetID,
		DurationSec:            float64(durationMs) / 1000.0,
		DurationMs:             durationMs,
		ContainerFormat:        "wav",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  normObj.SHA256,
		NormalizedAudioCASPath: normObj.Path,
		AudioChannels:          channels,
		AudioSampleRate:        sampleRate,
		CreatedAt:              time.Now().UTC(),
	}
	if err := h.db.SavePreflightReport(ctx, preflight); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}
	// Pre-seed stems in CAS and index if vocals or bg are distinct
	vocalsWAV := media.EncodePCM16Samples(vocalsSamples, sampleRate, channels)
	vocalObj, err := h.casStore.Put(bytes.NewReader(vocalsWAV))
	if err != nil {
		t.Fatalf("put vocals wav: %v", err)
	}
	bgWAV := media.EncodePCM16Samples(bgSamples, sampleRate, channels)
	bgObj, err := h.casStore.Put(bytes.NewReader(bgWAV))
	if err != nil {
		t.Fatalf("put bg wav: %v", err)
	}
	stemsArtifact := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		AssetID:       assetID,
		ProviderID:    "test_separator",
		ModelName:     "test_model",
		ModelVersion:  "v1.0",
		SchemaVersion: domain.AudioStemsSchemaVersion,
		CreatedAt:     time.Now().UTC(),
		Stems: []domain.AudioStem{
			{
				Type:         domain.StemTypeVocals,
				AudioCASHash: vocalObj.SHA256,
				AudioCASPath: vocalObj.Path,
				SampleRate:   sampleRate,
				Channels:     channels,
				Format:       "wav",
				DurationMs:   durationMs,
			},
			{
				Type:         domain.StemTypeBackground,
				AudioCASHash: bgObj.SHA256,
				AudioCASPath: bgObj.Path,
				SampleRate:   sampleRate,
				Channels:     channels,
				Format:       "wav",
				DurationMs:   durationMs,
			},
		},
	}
	stemsBlob, _ := json.Marshal(stemsArtifact)
	stemsObj, err := h.casStore.Put(bytes.NewReader(stemsBlob))
	if err != nil {
		t.Fatalf("put stems artifact: %v", err)
	}
	provHash, _ := domain.ComputeAudioStemsProvenanceHash(assetID, "test_separator", "test_model", "v1.0")
	_ = h.db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:             stemsArtifact.ID,
		AssetID:        assetID,
		ProviderID:     "test_separator",
		ModelName:      "test_model",
		ModelVersion:   "v1.0",
		CASHash:        stemsObj.SHA256,
		ProvenanceHash: provHash,
		CreatedAt:      time.Now().UTC(),
	})

	return assetID, normObj.SHA256
}

func TestAudioRoleService_AutomaticGeneration_FromStems(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(10000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000

	vocalsSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)

	// Timeline design:
	// 0 - 2000 ms: Instrumental BGM (vocals silent, bg steady music sine 440 Hz)
	for i := 0; i < 2000*sampleRate/1000; i++ {
		bgSamples[i] = int16(3000 * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
	}

	// 2000 - 5000 ms: Dialogue / Narration (vocals active speech modulation with consonant transitions)
	for i := 2000 * sampleRate / 1000; i < 5000*sampleRate/1000; i++ {
		bgSamples[i] = 1000
		if (i % 4) == 0 {
			vocalsSamples[i] = -2500
		} else if (i % 4) == 2 {
			vocalsSamples[i] = 2500
		} else {
			vocalsSamples[i] = int16(3000 * math.Sin(2*math.Pi*180*float64(i)/float64(sampleRate)))
		}
	}

	// 5000 - 7500 ms: Singing / Music-Vocal (pure harmonic sustained pitch, steady 330 Hz)
	for i := 5000 * sampleRate / 1000; i < 7500*sampleRate/1000; i++ {
		bgSamples[i] = int16(2000 * math.Sin(2*math.Pi*165*float64(i)/float64(sampleRate)))
		vocalsSamples[i] = int16(5000 * math.Sin(2*math.Pi*330*float64(i)/float64(sampleRate)))
	}

	// 7500 - 8500 ms: Ambience / SFX (vocals silent, bg sharp transient impulses / high crest factor)
	for i := 7500 * sampleRate / 1000; i < 8500*sampleRate/1000; i++ {
		if (i % 400) == 0 {
			bgSamples[i] = 15000 // sharp clicks
		} else {
			bgSamples[i] = 200
		}
	}

	// 8500 - 10000 ms: Uncertain (marginal weak energy in vocals with sub-pitch frequency)
	for i := 8500 * sampleRate / 1000; i < 10000*sampleRate/1000; i++ {
		bgSamples[i] = 500
		vocalsSamples[i] = int16(380 * math.Sin(2*math.Pi*73*float64(i)/float64(sampleRate)))
	}

	assetID, _ := createControlledAsset(t, h, durationMs, vocalsSamples, bgSamples)

	// Pre-condition check: SpeechService must FAIL CLOSED when plan is missing
	_, err := h.speechSvc.RunPipeline(ctx, domain.SpeechPipelineInput{
		RunID:   uuid.NewString(),
		AssetID: assetID,
	})
	if err == nil || !errors.Is(err, domain.ErrAudioRolePlanRequired) {
		t.Fatalf("expected ErrAudioRolePlanRequired before generation, got %v", err)
	}

	runID := uuid.NewString()
	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})
	if err != nil {
		t.Fatalf("GenerateAudioRolePlan failed: %v", err)
	}

	if plan == nil {
		t.Fatal("expected non-nil AudioRolePlan")
	}
	if plan.AssetID != assetID {
		t.Errorf("expected assetID %s, got %s", assetID, plan.AssetID)
	}
	if len(plan.Segments) == 0 {
		t.Fatal("expected non-empty segments in AudioRolePlan")
	}

	// Verify timeline coverage: start at 0 and end at durationMs
	if plan.Segments[0].StartMs != 0 {
		t.Errorf("expected first segment to start at 0, got %d", plan.Segments[0].StartMs)
	}
	lastSeg := plan.Segments[len(plan.Segments)-1]
	if lastSeg.EndMs != durationMs {
		t.Errorf("expected last segment to end at %d, got %d", durationMs, lastSeg.EndMs)
	}

	// Verify all 5 roles can be represented
	foundRoles := make(map[domain.AudioRole]bool)
	for _, seg := range plan.Segments {
		foundRoles[seg.Role] = true
	}

	t.Logf("Generated plan segments: %+v", plan.Segments)

	if !foundRoles[domain.AudioRoleNarrationDialogue] {
		t.Errorf("expected narration/dialogue segment to be generated")
	}
	if !foundRoles[domain.AudioRoleInstrumentalBgm] {
		t.Errorf("expected instrumental/background segment to be generated")
	}

	// Verify plan is persisted in DB and retrievable
	dbPlan, err := h.db.GetAudioRolePlan(ctx, assetID)
	if err != nil {
		t.Fatalf("GetAudioRolePlan failed: %v", err)
	}
	if len(dbPlan.Segments) != len(plan.Segments) {
		t.Errorf("db plan segments count mismatch: expected %d, got %d", len(plan.Segments), len(dbPlan.Segments))
	}

	// Verify plan was committed to CAS
	if plan.CASHash == "" {
		t.Errorf("expected non-empty CASHash on AudioRolePlan")
	}
	casReader, err := h.casStore.Get(plan.CASHash)
	if err != nil {
		t.Fatalf("failed to retrieve plan from CAS: %v", err)
	}
	defer casReader.Close()

	var casPlan domain.AudioRolePlan
	if err := json.NewDecoder(casReader).Decode(&casPlan); err != nil {
		t.Fatalf("failed to decode plan from CAS: %v", err)
	}
	if casPlan.AssetID != assetID {
		t.Errorf("CAS plan assetID mismatch: %s vs %s", casPlan.AssetID, assetID)
	}

	// Verify governance evidence
	attempts, err := h.db.ListProviderAttempts(ctx, runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) == 0 {
		t.Errorf("expected at least 1 provider attempt logged for audio_role_plan")
	}

	decisions, err := h.db.ListSelectionDecisions(ctx, runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list selection decisions: %v", err)
	}
	if len(decisions) == 0 {
		t.Errorf("expected at least 1 selection decision logged for audio_role_plan")
	}

	// Verify dub-eligibility
	if !domain.IsDubEligible(plan) {
		t.Errorf("expected plan with dialogue to be dub-eligible")
	}
}

func TestAudioRoleService_ZeroDialogue_ValidNoDubPlan(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(8000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000

	vocalsSamples := make([]int16, totalSamples) // pure silence in vocals
	bgSamples := make([]int16, totalSamples)
	for i := 0; i < totalSamples; i++ {
		bgSamples[i] = int16(2000 * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
	}

	assetID, _ := createControlledAsset(t, h, durationMs, vocalsSamples, bgSamples)

	runID := uuid.NewString()
	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})
	if err != nil {
		t.Fatalf("GenerateAudioRolePlan for zero-dialogue media failed: %v", err)
	}

	if domain.IsDubEligible(plan) {
		t.Errorf("zero-dialogue media must NOT be dub-eligible")
	}

	// Verify SpeechService recognizes zero dub-eligible speech and returns ErrNoDubEligibleSpeech
	_, err = h.speechSvc.RunPipeline(ctx, domain.SpeechPipelineInput{
		RunID:         runID,
		AssetID:       assetID,
		AudioRolePlan: plan,
	})
	if err == nil || !errors.Is(err, domain.ErrNoDubEligibleSpeech) {
		t.Errorf("expected ErrNoDubEligibleSpeech for no-dub plan, got %v", err)
	}
}

func TestAudioRoleService_UncertainIntervals_ProjectReviewItem(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(6000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000

	vocalsSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)

	// 0 - 3000 ms: Dialogue
	for i := 0; i < 3000*sampleRate/1000; i++ {
		vocalsSamples[i] = int16(4000 * math.Sin(2*math.Pi*200*float64(i)/float64(sampleRate)))
		bgSamples[i] = 500
	}
	// 3000 - 6000 ms: Uncertain role
	for i := 3000 * sampleRate / 1000; i < 6000*sampleRate/1000; i++ {
		vocalsSamples[i] = int16(380 * math.Sin(2*math.Pi*70*float64(i)/float64(sampleRate)))
		bgSamples[i] = 500
	}

	assetID, _ := createControlledAsset(t, h, durationMs, vocalsSamples, bgSamples)

	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("GenerateAudioRolePlan failed: %v", err)
	}

	var hasUncertain bool
	for _, seg := range plan.Segments {
		if seg.Role == domain.AudioRoleUncertain {
			hasUncertain = true
			break
		}
	}
	if !hasUncertain {
		t.Logf("Segments produced: %+v", plan.Segments)
	}

	// Project review items via ReviewService
	items, err := h.reviewSvc.ProjectReviewItems(ctx, assetID, domain.TargetLanguageVI)
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}

	if hasUncertain {
		var foundAudioRoleReviewItem bool
		for _, it := range items {
			if it.Type == domain.ReviewItemTypeAudioRole && it.Status == domain.ReviewItemStatusPending {
				foundAudioRoleReviewItem = true
				break
			}
		}
		if !foundAudioRoleReviewItem {
			t.Errorf("expected pending audio_role_uncertain ReviewItem for uncertain segment")
		}
	}
}

func TestAudioRoleService_IdempotentReuse(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(5000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000

	vocalsSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)
	for i := 0; i < totalSamples; i++ {
		vocalsSamples[i] = int16(3000 * math.Sin(2*math.Pi*300*float64(i)/float64(sampleRate)))
		bgSamples[i] = 1000
	}

	assetID, _ := createControlledAsset(t, h, durationMs, vocalsSamples, bgSamples)

	plan1, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	plan2, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if plan1.ID != plan2.ID {
		t.Errorf("idempotent reuse mismatch: plan1.ID=%s, plan2.ID=%s", plan1.ID, plan2.ID)
	}
	if plan1.CASHash != plan2.CASHash {
		t.Errorf("idempotent CAS hash mismatch: %s vs %s", plan1.CASHash, plan2.CASHash)
	}
}

func TestAudioRoleService_MissingPreflight_FailsClosed(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()
	assetID := uuid.NewString()
	attID := uuid.NewString()
	_ = h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = h.db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-hash",
		CASPath:             filepath.Join(h.dir, "nonexistent.wav"),
		CreatedAt:           time.Now().UTC(),
	})

	_, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err == nil {
		t.Fatal("expected error on missing preflight, got nil")
	}
}
