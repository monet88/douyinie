package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
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

	audioRoleSvc := service.NewAudioRoleServiceWithAnalyzer(db, casStore, audioMixSvc, service.NewDeterministicTestAudioRoleAnalyzer())
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

	// 8500 - 10000 ms: Uncertain (irregular noisy vocal energy with low autocorrelation)
	var lcg int32 = 12345
	for i := 8500 * sampleRate / 1000; i < 10000*sampleRate/1000; i++ {
		bgSamples[i] = 500
		lcg = (lcg*1103515245 + 12345) & 0x7fffffff
		vocalsSamples[i] = int16((lcg % 900) - 450)
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

	requiredRoles := []domain.AudioRole{
		domain.AudioRoleNarrationDialogue,
		domain.AudioRoleSingingMusicVocal,
		domain.AudioRoleInstrumentalBgm,
		domain.AudioRoleAmbienceSFX,
		domain.AudioRoleUncertain,
	}
	for _, r := range requiredRoles {
		if !foundRoles[r] {
			t.Errorf("missing required canonical role %q in generated plan: %+v", r, plan.Segments)
		}
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

	// 0 - 3000 ms: Dialogue (speech consonants and modulation)
	for i := 0; i < 3000*sampleRate/1000; i++ {
		if (i % 4) == 0 {
			vocalsSamples[i] = -2500
		} else if (i % 4) == 2 {
			vocalsSamples[i] = 2500
		} else {
			vocalsSamples[i] = int16(3000 * math.Sin(2*math.Pi*180*float64(i)/float64(sampleRate)))
		}
		bgSamples[i] = 500
	}
	// 3000 - 6000 ms: Uncertain role (marginal noisy vocal energy with low autocorrelation)
	var lcg int32 = 67890
	for i := 3000 * sampleRate / 1000; i < 6000*sampleRate/1000; i++ {
		bgSamples[i] = 500
		lcg = (lcg*1103515245 + 12345) & 0x7fffffff
		vocalsSamples[i] = int16((lcg % 900) - 450)
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
		t.Fatalf("expected plan to contain uncertain segments, but found none: %+v", plan.Segments)
	}

	// Project review items via ReviewService
	items, err := h.reviewSvc.ProjectReviewItems(ctx, assetID, domain.TargetLanguageVI)
	if err != nil {
		t.Fatalf("ProjectReviewItems failed: %v", err)
	}

	var foundAudioRoleReviewItem bool
	for _, it := range items {
		if it.Type == domain.ReviewItemTypeAudioRole && it.Reason == "uncertain_audio_role" && it.Status == domain.ReviewItemStatusPending {
			foundAudioRoleReviewItem = true
			break
		}
	}
	if !foundAudioRoleReviewItem {
		t.Fatalf("expected pending audio_role_uncertain ReviewItem for uncertain segment, got: %+v", items)
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

type mockCustomAnalyzer struct {
	pID     string
	mName   string
	mVer    string
	cfgHash string
}

func (m *mockCustomAnalyzer) AnalyzerInfo() (string, string, string, string) {
	return m.pID, m.mName, m.mVer, m.cfgHash
}

func (m *mockCustomAnalyzer) AnalyzeAudioRoles(ctx context.Context, req service.AudioRoleAnalysisRequest) (*service.AudioRoleAnalysisResult, error) {
	return &service.AudioRoleAnalysisResult{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
		ProviderID:   m.pID,
		ModelName:    m.mName,
		ModelVersion: m.mVer,
	}, nil
}

type provenanceRoleAnalyzer struct {
	pID     string
	mName   string
	mVer    string
	cfgHash string
	role    domain.AudioRole
}

func (a *provenanceRoleAnalyzer) AnalyzerInfo() (string, string, string, string) {
	return a.pID, a.mName, a.mVer, a.cfgHash
}

func (a *provenanceRoleAnalyzer) AnalyzeAudioRoles(ctx context.Context, req service.AudioRoleAnalysisRequest) (*service.AudioRoleAnalysisResult, error) {
	return &service.AudioRoleAnalysisResult{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: req.DurationMs, Role: a.role},
		},
		ProviderID:   a.pID,
		ModelName:    a.mName,
		ModelVersion: a.mVer,
	}, nil
}

func TestAudioRoleService_InjectedAnalyzerProvenance(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	customAnalyzer := &mockCustomAnalyzer{
		pID:     "vendor_audio_lab",
		mName:   "deep_acoustic_role_classifier",
		mVer:    "v3.2",
		cfgHash: "cfg-deterministic-1234",
	}
	h.audioRole.SetAnalyzer(customAnalyzer)

	durationMs := int64(3000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000
	vocalSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)
	for i := range vocalSamples {
		vocalSamples[i] = 1000
		bgSamples[i] = 500
	}

	assetID, normSHA := createControlledAsset(t, h, durationMs, vocalSamples, bgSamples)

	// Fetch stems CAS hash
	stemsIdx, err := h.db.GetAudioStemsArtifactIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get stems idx: %v", err)
	}

	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("GenerateAudioRolePlan failed: %v", err)
	}

	// 1. Assert plan metadata matches injected analyzer exactly
	if plan.ProviderID != "vendor_audio_lab" {
		t.Errorf("expected ProviderID vendor_audio_lab, got %s", plan.ProviderID)
	}
	if plan.ModelName != "deep_acoustic_role_classifier" {
		t.Errorf("expected ModelName deep_acoustic_role_classifier, got %s", plan.ModelName)
	}
	if plan.ModelVersion != "v3.2" {
		t.Errorf("expected ModelVersion v3.2, got %s", plan.ModelVersion)
	}

	// 2. Assert provenance hash was calculated using the injected analyzer's identity and config hash
	expectedProvHash := domain.ComputeAudioRolePlanProvenanceHash(
		normSHA,
		stemsIdx.CASHash,
		"vendor_audio_lab",
		"deep_acoustic_role_classifier",
		"v3.2",
		"cfg-deterministic-1234",
	)
	if plan.ProvenanceHash != expectedProvHash {
		t.Errorf("provenance hash mismatch:\nexpected: %s\ngot:      %s", expectedProvHash, plan.ProvenanceHash)
	}

	// 3. Assert SQLite index is queryable by this exact provenance hash
	idx, err := h.db.GetAudioRolePlanByProvenance(ctx, expectedProvHash)
	if err != nil {
		t.Fatalf("GetAudioRolePlanByProvenance failed: %v", err)
	}
	if idx.ProviderID != "vendor_audio_lab" || idx.ModelName != "deep_acoustic_role_classifier" {
		t.Errorf("indexed metadata mismatch: %+v", idx)
	}
}

func TestAudioRoleService_ProvenanceCacheReturnsExactArtifactAfterABASwitch(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(3000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000
	vocalSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)
	assetID, _ := createControlledAsset(t, h, durationMs, vocalSamples, bgSamples)

	analyzerA := &provenanceRoleAnalyzer{
		pID:     "test_audio_role_provider",
		mName:   "role_classifier",
		mVer:    "v1",
		cfgHash: "config-a",
		role:    domain.AudioRoleNarrationDialogue,
	}
	analyzerB := &provenanceRoleAnalyzer{
		pID:     "test_audio_role_provider",
		mName:   "role_classifier",
		mVer:    "v1",
		cfgHash: "config-b",
		role:    domain.AudioRoleAmbienceSFX,
	}

	h.audioRole.SetAnalyzer(analyzerA)
	planA, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{AssetID: assetID, RunID: uuid.NewString()})
	if err != nil {
		t.Fatalf("generate plan A: %v", err)
	}

	h.audioRole.SetAnalyzer(analyzerB)
	planB, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{AssetID: assetID, RunID: uuid.NewString()})
	if err != nil {
		t.Fatalf("generate plan B: %v", err)
	}
	if planA.ProvenanceHash == planB.ProvenanceHash {
		t.Fatal("test setup requires distinct provenance hashes for A and B")
	}

	h.audioRole.SetAnalyzer(analyzerA)
	planAAgain, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{AssetID: assetID, RunID: uuid.NewString()})
	if err != nil {
		t.Fatalf("reuse plan A after B: %v", err)
	}

	if planAAgain.ID != planA.ID {
		t.Fatalf("expected exact cached plan A ID %s, got %s", planA.ID, planAAgain.ID)
	}
	if planAAgain.ProvenanceHash != planA.ProvenanceHash {
		t.Fatalf("expected plan A provenance %s, got %s", planA.ProvenanceHash, planAAgain.ProvenanceHash)
	}
	if planAAgain.CASHash != planA.CASHash {
		t.Fatalf("expected plan A CAS hash %s, got %s", planA.CASHash, planAAgain.CASHash)
	}
	if len(planAAgain.Segments) != 1 || planAAgain.Segments[0].Role != domain.AudioRoleNarrationDialogue {
		t.Fatalf("expected cached plan A dialogue segment, got %+v", planAAgain.Segments)
	}
}

func TestAudioRoleService_MissingOrCorruptEvidence_FailsClosed(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	// 1. Corrupt vocals stem: file with invalid non-WAV bytes
	corruptVocalsPath := filepath.Join(h.dir, "corrupt_vocals.wav")
	_ = os.WriteFile(corruptVocalsPath, []byte("INVALID_TRUNCATED_WAV_HEADER"), 0644)

	corruptReq := service.AudioRoleAnalysisRequest{
		AssetID:         uuid.NewString(),
		DurationMs:      5000,
		VocalsPath:      corruptVocalsPath,
		BackgroundPath:  filepath.Join(h.dir, "valid_bg.wav"),
		SourceAudioPath: filepath.Join(h.dir, "source.wav"),
	}
	analyzer := service.NewDeterministicTestAudioRoleAnalyzer()
	_, err := analyzer.AnalyzeAudioRoles(ctx, corruptReq)
	if err == nil {
		t.Fatal("expected decode failure on corrupt vocals stem, got nil")
	}

	// 2. Nonexistent stem file
	missingReq := service.AudioRoleAnalysisRequest{
		AssetID:         uuid.NewString(),
		DurationMs:      5000,
		VocalsPath:      filepath.Join(h.dir, "nonexistent_vocals.wav"),
		BackgroundPath:  filepath.Join(h.dir, "valid_bg.wav"),
		SourceAudioPath: filepath.Join(h.dir, "source.wav"),
	}
	_, err = analyzer.AnalyzeAudioRoles(ctx, missingReq)
	if err == nil {
		t.Fatal("expected failure on nonexistent vocals stem, got nil")
	}

	// 3. Empty request with no readable samples
	emptyReq := service.AudioRoleAnalysisRequest{
		AssetID:    uuid.NewString(),
		DurationMs: 5000,
	}
	_, err = analyzer.AnalyzeAudioRoles(ctx, emptyReq)
	if err == nil {
		t.Fatal("expected failure when no samples provided, got nil")
	}
}

func TestAudioRoleService_GovernancePersistenceError_Propagated(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(3000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000
	vocalSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)

	assetID, _ := createControlledAsset(t, h, durationMs, vocalSamples, bgSamples)

	// Close database to force persistence/governance failure
	_ = h.db.Close()

	_, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err == nil {
		t.Fatal("expected error when DB write fails, got nil")
	}
}

func TestAudioRoleService_DefaultConstructor_FailsClosedWithoutAnalyzer(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	// Default constructor with no analyzer
	svc := service.NewAudioRoleService(h.db, h.casStore, h.audioMixSvc)
	if svc.Analyzer() != nil {
		t.Fatal("expected NewAudioRoleService to have nil analyzer by default in production")
	}

	durationMs := int64(2000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000
	vocalSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)
	assetID, _ := createControlledAsset(t, h, durationMs, vocalSamples, bgSamples)

	_, err := svc.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   uuid.NewString(),
	})
	if err == nil || !errors.Is(err, domain.ErrAudioRoleAnalyzerUnavailable) {
		t.Fatalf("expected ErrAudioRoleAnalyzerUnavailable when analyzer is not configured, got: %v", err)
	}
}

func TestAudioRoleService_GovernanceCacheCompleteness(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	durationMs := int64(2000)
	sampleRate := 16000
	totalSamples := (sampleRate * int(durationMs)) / 1000
	vocalSamples := make([]int16, totalSamples)
	bgSamples := make([]int16, totalSamples)
	assetID, _ := createControlledAsset(t, h, durationMs, vocalSamples, bgSamples)

	// Run 1: Generates plan and records governance records for run1
	runID1 := uuid.NewString()
	plan1, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID1,
	})
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}

	decisions1, err := h.db.ListSelectionDecisions(ctx, runID1, "audio_role_plan")
	if err != nil || len(decisions1) == 0 {
		t.Fatalf("expected selection decision for run1, got %v (err=%v)", decisions1, err)
	}
	attempts1, err := h.db.ListProviderAttempts(ctx, runID1, "audio_role_plan")
	if err != nil || len(attempts1) == 0 {
		t.Fatalf("expected provider attempt for run1, got %v (err=%v)", attempts1, err)
	}

	// Run 2: Cache hit from same asset and provenance, but new runID2!
	// Governance records MUST be created for runID2 on cache hit!
	runID2 := uuid.NewString()
	plan2, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID2,
	})
	if err != nil {
		t.Fatalf("second run (cache hit) failed: %v", err)
	}
	if plan2.ID != plan1.ID {
		t.Fatalf("expected cached plan ID %s, got %s", plan1.ID, plan2.ID)
	}

	decisions2, err := h.db.ListSelectionDecisions(ctx, runID2, "audio_role_plan")
	if err != nil || len(decisions2) == 0 {
		t.Fatalf("governance bypassed on cache hit: expected selection decision for run2, got %v (err=%v)", decisions2, err)
	}
	attempts2, err := h.db.ListProviderAttempts(ctx, runID2, "audio_role_plan")
	if err != nil || len(attempts2) == 0 {
		t.Fatalf("governance bypassed on cache hit: expected provider attempt for run2, got %v (err=%v)", attempts2, err)
	}
}

func TestAudioRoleService_ConcurrentGeneration_SerializedPerAsset(t *testing.T) {
	h := setupAudioRoleTestHarness(t)
	ctx := context.Background()

	// Seed Asset A
	samplesA := make([]int16, 32000)
	for i := range samplesA {
		samplesA[i] = int16(1000 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	assetIDA, _ := createControlledAsset(t, h, 2000, samplesA, nil)

	// Seed Asset B
	samplesB := make([]int16, 32000)
	for i := range samplesB {
		samplesB[i] = int16(1000 * math.Sin(2*math.Pi*880*float64(i)/16000))
	}
	assetIDB, _ := createControlledAsset(t, h, 2000, samplesB, nil)

	const concurrency = 8
	plansA := make([]*domain.AudioRolePlan, concurrency)
	errsA := make([]error, concurrency)
	plansB := make([]*domain.AudioRolePlan, concurrency)
	errsB := make([]error, concurrency)

	var wg sync.WaitGroup
	wg.Add(concurrency * 2)

	for idx := range concurrency {
		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("run_a_%d_%s", idx, uuid.NewString()[:6])
			plansA[idx], errsA[idx] = h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
				AssetID: assetIDA,
				RunID:   runID,
			})
		}()

		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("run_b_%d_%s", idx, uuid.NewString()[:6])
			plansB[idx], errsB[idx] = h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
				AssetID: assetIDB,
				RunID:   runID,
			})
		}()
	}

	wg.Wait()

	// Verify Asset A results
	for i := range concurrency {
		if errsA[i] != nil {
			t.Fatalf("goroutine %d for asset A failed: %v", i, errsA[i])
		}
		if plansA[i] == nil || plansA[i].ID == "" {
			t.Fatalf("goroutine %d for asset A returned nil or empty plan", i)
		}
		if plansA[i].ID != plansA[0].ID {
			t.Fatalf("goroutine %d for asset A got plan ID %s, expected identical ID %s", i, plansA[i].ID, plansA[0].ID)
		}
	}

	// Verify Asset B results
	for i := range concurrency {
		if errsB[i] != nil {
			t.Fatalf("goroutine %d for asset B failed: %v", i, errsB[i])
		}
		if plansB[i] == nil || plansB[i].ID == "" {
			t.Fatalf("goroutine %d for asset B returned nil or empty plan", i)
		}
		if plansB[i].ID != plansB[0].ID {
			t.Fatalf("goroutine %d for asset B got plan ID %s, expected identical ID %s", i, plansB[i].ID, plansB[0].ID)
		}
	}
	// Ensure Asset A and Asset B are distinct
	if plansA[0].ID == plansB[0].ID {
		t.Fatalf("asset A and asset B produced identical plan ID %s", plansA[0].ID)
	}
}
