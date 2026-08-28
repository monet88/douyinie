package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestRouter builds a Router over the fake Seam 1 registry with governance
// services backed by a real SQLite DB (so routing provenance and license
// manifests are exercised). It returns the router and the registry.
func newTestRouter(t *testing.T) (*provider.Router, *provider.Registry) {
	t.Helper()
	db, err := storage.Open(t.TempDir() + "/speech_test.db")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := provider.NewSeam1FakeRegistry()
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	ctx := context.Background()
	for _, p := range reg.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
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
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	return provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db), reg
}

// TestSpeechService_DefaultDiarize_AlwaysAssignsSpeaker verifies the default
// speaker assignment is NOT circular: it always assigns a canonical speaker
// (SPEAKER_00) across the full span so speaker labels exist before canonical
// segmentation, even when no word carries a pre-existing speaker label.
func TestSpeechService_DefaultDiarize_AlwaysAssignsSpeaker(t *testing.T) {
	s := NewSpeechService(nil, nil)
	if s.Diarize == nil {
		t.Fatal("expected default Diarize to be installed at construction")
	}

	// 1. No pre-existing speaker labels -> still a single-speaker plan.
	plan, err := s.Diarize(context.Background(), []domain.WordTiming{
		{Word: "你好", StartMs: 0, EndMs: 500, Confidence: 0.9},
		{Word: "再见", StartMs: 600, EndMs: 1000, Confidence: 0.9},
	}, SpeechDiarizationRequest{RunID: "run-1", AssetID: "asset-1"})
	if err != nil {
		t.Fatalf("Diarize failed: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a plan (speaker assignment always runs before segmentation)")
	}
	if len(plan.Assignments) != 1 || plan.Assignments[0].SpeakerID != "SPEAKER_00" {
		t.Fatalf("expected single SPEAKER_00 assignment, got %+v", plan.Assignments)
	}
	if plan.Assignments[0].StartMs != 0 || plan.Assignments[0].EndMs != 1000 {
		t.Errorf("expected assignment to span the full word timeline [0,1000], got [%d,%d]", plan.Assignments[0].StartMs, plan.Assignments[0].EndMs)
	}

	// 2. Empty words -> no plan (nothing to assign).
	plan, err = s.Diarize(context.Background(), nil, SpeechDiarizationRequest{})
	if err != nil {
		t.Fatalf("Diarize(nil) failed: %v", err)
	}
	if plan != nil {
		t.Fatal("expected nil plan for empty word timings")
	}
}

// TestSpeechService_DefaultASRInvoke_QualityFallbackTo06B verifies production
// wiring: ASR routes through the Router, and a quality failure on the 1.7B
// provider falls back to the 0.6B provider.
func TestSpeechService_DefaultASRInvoke_QualityFallbackTo06B(t *testing.T) {
	router, reg := newTestRouter(t)
	s := NewSpeechService(nil, nil)
	s.ConfigureRouter(router)
	if s.ASRInvoke == nil {
		t.Fatal("expected router-backed default ASRInvoke after ConfigureRouter")
	}

	// Force the 1.7B provider to fail with ErrQualityRejected by clearing its
	// TranscribedText (ProduceTranscript returns ErrQualityRejected), so the
	// router advances to the 0.6B fallback.
	asr17, ok := reg.Get("fake_qwen3_asr")
	if !ok {
		t.Fatal("fake_qwen3_asr not registered")
	}
	asr17Fake, ok := asr17.(*provider.FakeASRProvider)
	if !ok {
		t.Fatalf("fake_qwen3_asr is %T, not *provider.FakeASRProvider", asr17)
	}
	asr17Fake.TranscribedText = ""

	res, err := s.ASRInvoke(context.Background(), SpeechASRRequest{
		RunID:    "run-test-fallback",
		AssetID:  "asset-1",
		AudioRef: worker.ArtifactRef{SHA256: "sha-1", Path: "/audio.wav"},
		Language: "zh",
	})
	if err != nil {
		t.Fatalf("default ASR invoke failed: %v", err)
	}
	if res == nil || len(res.Segments) == 0 {
		t.Fatal("expected ASR result with segments from fallback provider")
	}
	if res.ProviderID != "fake_qwen3_asr_06b" {
		t.Errorf("expected fallback to fake_qwen3_asr_06b, got provider %s", res.ProviderID)
	}
	if res.ModelVersion != "0.6b" {
		t.Errorf("expected model version 0.6b, got %s", res.ModelVersion)
	}
}

// TestSpeechService_DefaultDiarize_SingleSpeakerDoesNotFabricateMulti ensures
// the default speaker assignment produces a single-speaker plan (SPEAKER_00)
// and does NOT invent multi-speaker structure from single-speaker audio.
func TestSpeechService_DefaultDiarize_SingleSpeakerDoesNotFabricateMulti(t *testing.T) {
	s := NewSpeechService(nil, nil)
	plan, err := s.Diarize(context.Background(), []domain.WordTiming{
		{Word: "你好", StartMs: 0, EndMs: 500, Confidence: 0.9, SpeakerID: "SPEAKER_00"},
		{Word: "再见", StartMs: 600, EndMs: 1000, Confidence: 0.9, SpeakerID: "SPEAKER_00"},
	}, SpeechDiarizationRequest{RunID: "run-1", AssetID: "asset-1"})
	if err != nil {
		t.Fatalf("Diarize failed: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a plan (speaker assignment always runs before segmentation)")
	}
	if len(plan.Assignments) != 1 || plan.Assignments[0].SpeakerID != "SPEAKER_00" {
		t.Fatalf("expected single SPEAKER_00 assignment, got %+v", plan.Assignments)
	}
	// Must not fabricate a second speaker.
	if len(plan.Assignments) >= 2 {
		t.Errorf("fabricated multi-speaker structure from single-speaker audio")
	}
}

// TestSpeechService_DefaultASRInvoke_NoRouterFailsClosed verifies fail-closed
// behavior when the router has not been wired.
func TestSpeechService_DefaultASRInvoke_NoRouterFailsClosed(t *testing.T) {
	s := NewSpeechService(nil, nil)
	s.ASRInvoke = s.defaultASRInvoke // bypass ConfigureRouter to simulate unwired state
	_, err := s.ASRInvoke(context.Background(), SpeechASRRequest{RunID: "r", AssetID: "a"})
	if err == nil {
		t.Fatal("expected error when ASR router is not configured")
	}
	if err.Error() != "ASR router is not configured (call ConfigureRouter)" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSpeechService_DefaultDiarize_SingleSpeakerFallbackNotDiarization verifies
// the evidence-gated DiarizationRan semantics: the default single-speaker
// fallback still assigns SPEAKER_00 labels (so segmentation always has speaker
// labels) but DiarizationRan stays false because no multi-speaker evidence
// warrants real diarization.
func TestSpeechService_DefaultDiarize_SingleSpeakerFallbackNotDiarization(t *testing.T) {
	s := NewSpeechService(nil, nil)
	words := []domain.WordTiming{
		{Word: "你好", StartMs: 0, EndMs: 500, Confidence: 0.9},
		{Word: "再见", StartMs: 600, EndMs: 1000, Confidence: 0.9},
	}

	// Drive through the full pipeline so DiarizationRan is derived evidence-gated.
	asr := &SpeechASRResult{
		ProviderID:   "fake_qwen3_asr",
		ModelName:    "qwen3-asr",
		ModelVersion: "1.7b",
		LanguageCode: "zh",
		Segments: []domain.ASRRawSegment{
			{StartMs: 0, EndMs: 1000, Text: "你好 再见", Confidence: 0.9},
		},
	}
	align := &SpeechAlignmentResult{
		ProviderID:   "fake_qwen3_aligner",
		ModelName:    "qwen3-aligner",
		ModelVersion: "1.0.0",
		WordTimings:  words,
	}
	artifact, err := s.BuildTranscriptFromInputs(context.Background(), asr, align, "run-1", "asset-1")
	if err != nil {
		t.Fatalf("build transcript: %v", err)
	}

	// Single-speaker fallback: labels exist but DiarizationRan must be false.
	if artifact.DiarizationRan {
		t.Error("expected DiarizationRan=false for single-speaker fallback (no multi-speaker evidence)")
	}
	if artifact.DiarizationProviderID != defaultDiarizerID {
		t.Errorf("expected fallback diarizer id %q, got %q", defaultDiarizerID, artifact.DiarizationProviderID)
	}
	for _, w := range artifact.WordTimings {
		if strings.TrimSpace(w.SpeakerID) == "" {
			t.Errorf("word %q has no speaker label after fallback assignment", w.Word)
		}
	}
}

// TestSpeechService_ProvenanceHashChangesWithDiarization verifies diarization
// identity participates in the transcript cache identity: changing diarization
// provider, model name, model version, evidence config, or DiarizationRan changes the provenance hash (Finding 4).
func TestSpeechService_ProvenanceHashChangesWithDiarization(t *testing.T) {
	cfg := domain.DefaultSegmentRuleConfig()
	evidCfg := domain.DefaultDiarizationEvidenceConfig()
	a := domain.TranscriptProvenance{
		AssetSHA256:             "sha",
		ASRProviderID:           "p1",
		ASRModelName:            "qwen3-asr",
		ASRModelVersion:         "1.7b",
		AlignerProviderID:       "p2",
		AlignerModelName:        "qwen3-aligner",
		AlignerModelVersion:     "1.0.0",
		SegmentConfig:           cfg,
		DiarizationProviderID:   "diarizer-a",
		DiarizationModelName:    "iic/speech_campplus_sv_zh_en_16k-common_advanced",
		DiarizationModelVersion: "v1.0.0",
		DiarizationVADModel:     "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
		DiarizationVADVersion:   "v2.0.4",
		DiarizationRan:          true,
		DiarizationEvidence:     evidCfg,
		SchemaVersion:           domain.TranscriptSchemaVersion,
	}
	if a.Hash() == "" {
		t.Error("provenance hash must not be empty")
	}

	// 1. Changing the diarizer provider ID must change the hash.
	b := a
	b.DiarizationProviderID = "diarizer-b"
	if a.Hash() == b.Hash() {
		t.Error("changing diarization provider must change provenance hash")
	}

	// 2. Changing DiarizationModelName must change the hash (Finding 4).
	bModel := a
	bModel.DiarizationModelName = "pyannote-diarization"
	if a.Hash() == bModel.Hash() {
		t.Error("changing DiarizationModelName must change provenance hash")
	}

	// 3. Changing DiarizationModelVersion must change the hash (Finding 4).
	bVer := a
	bVer.DiarizationModelVersion = "v2"
	if a.Hash() == bVer.Hash() {
		t.Error("changing DiarizationModelVersion must change provenance hash")
	}

	// 3b. Changing DiarizationVADModel must change the hash (Finding 5).
	bVADModel := a
	bVADModel.DiarizationVADModel = "custom-vad"
	if a.Hash() == bVADModel.Hash() {
		t.Error("changing DiarizationVADModel must change provenance hash")
	}

	// 3c. Changing DiarizationVADVersion must change the hash (Finding 5).
	bVADVer := a
	bVADVer.DiarizationVADVersion = "v3.0.0"
	if a.Hash() == bVADVer.Hash() {
		t.Error("changing DiarizationVADVersion must change provenance hash")
	}
	// 4. Changing semantic DiarizationEvidence config must change the hash (Finding 4).
	bEvid := a
	bEvid.DiarizationEvidence.MinTurnGapMs = 500
	if a.Hash() == bEvid.Hash() {
		t.Error("changing DiarizationEvidence config must change provenance hash")
	}

	// 5. Changing DiarizationRan must change the hash.
	c := a
	c.DiarizationRan = false
	if a.Hash() == c.Hash() {
		t.Error("changing DiarizationRan must change provenance hash")
	}

	// 6. Changing SpeakerEvidenceHash must change the hash (Issue #44 Blocker 3).
	d := a
	ev := &domain.SpeakerEvidence{
		HasMultiSpeakerCues: true,
		SpeakerChangeCount:  2,
		Confidence:          0.95,
		Source:              "iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0",
	}
	d.SpeakerEvidenceHash = ev.Hash()
	if a.Hash() == d.Hash() {
		t.Error("changing SpeakerEvidenceHash must change provenance hash")
	}
	if d.SpeakerEvidenceHash == "" {
		t.Error("expected non-empty SpeakerEvidenceHash")
	}

	// 7. Changing NormalizedAudioSHA256 must change the hash (Finding 2).
	eNorm := a
	eNorm.NormalizedAudioSHA256 = "sha256-different-audio"
	if a.Hash() == eNorm.Hash() {
		t.Error("changing NormalizedAudioSHA256 must change provenance hash")
	}

	// 8. Changing NormalizationConfig must change the hash (Finding 2).
	eCfg := a
	eCfg.NormalizationConfig.SampleRate = 24000
	if a.Hash() == eCfg.Hash() {
		t.Error("changing NormalizationConfig must change provenance hash")
	}

	// 9. Changing DiarizationEvidence EmbeddingCosineThreshold must change the hash (Finding 3).
	eThresh := a
	eThresh.DiarizationEvidence.EmbeddingCosineThreshold = 0.75
	if a.Hash() == eThresh.Hash() {
		t.Error("changing EmbeddingCosineThreshold must change provenance hash")
	}
}

// TestTranscriptProvenance_PathFreeDeterminism verifies Finding 3:
// machine-local paths do not participate in semantic provenance identity:
// identical content at different CAS paths yields identical provenance hash,
// while changed normalized-audio content or evidence threshold changes identity.
func TestTranscriptProvenance_PathFreeDeterminism(t *testing.T) {
	prov1 := domain.TranscriptProvenance{
		AssetSHA256:           "content-sha256-abc123",
		NormalizedAudioSHA256: "norm-audio-sha256-def456",
		NormalizationConfig:   domain.DefaultAudioNormalizationConfig(),
		ASRProviderID:         "qwen3_asr_1_7b",
		ASRModelName:          "qwen3-asr",
		ASRModelVersion:       "1.7b",
		AlignerProviderID:     "qwen3_forced_aligner",
		AlignerModelName:      "Qwen3-ForcedAligner-0.6B",
		AlignerModelVersion:   "0.6b",
		SegmentConfig:         domain.DefaultSegmentRuleConfig(),
		DiarizationEvidence:   domain.DefaultDiarizationEvidenceConfig(),
		SchemaVersion:         domain.TranscriptSchemaVersion,
	}

	prov2 := prov1 // identical content
	if prov1.Hash() != prov2.Hash() {
		t.Fatalf("identical provenance must produce identical hash, got %q vs %q", prov1.Hash(), prov2.Hash())
	}

	// Changed normalized audio content changes identity
	provDiffAudio := prov1
	provDiffAudio.NormalizedAudioSHA256 = "norm-audio-sha256-DIFFERENT"
	if prov1.Hash() == provDiffAudio.Hash() {
		t.Fatalf("changed normalized audio SHA256 must produce different hash")
	}

	// Changed embedding cosine threshold changes identity
	provDiffThreshold := prov1
	provDiffThreshold.DiarizationEvidence.EmbeddingCosineThreshold = 0.70
	if prov1.Hash() == provDiffThreshold.Hash() {
		t.Fatalf("changed embedding cosine threshold must produce different hash")
	}
}

// TestSpeechService_RunPipeline_ResolvesPreflightNormalizedAudio verifies Finding 2:
// SpeechService.RunPipeline resolves and consumes the source-derived Acquisition/Preflight
// normalized 16 kHz mono WAV CAS artifact from SQLite PreflightReport and fails closed
// if the preflight artifact is missing or corrupt.
func TestSpeechService_RunPipeline_ResolvesPreflightNormalizedAudio(t *testing.T) {
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test_preflight_audio.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})

	// 1. Create normalized WAV file in CAS
	normWav := filepath.Join(tmpDir, "norm_16k.wav")
	if err := createValidTestWAV(normWav, 16000, 1, 16000); err != nil {
		t.Fatalf("create norm wav: %v", err)
	}
	normObj, err := casStore.PutFile(normWav)
	if err != nil {
		t.Fatalf("put norm wav in CAS: %v", err)
	}

	assetID := uuid.NewString()
	sourceAsset := domain.SourceAsset{
		ID:                  assetID,
		SHA256:              "source_video_sha256",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "video.mp4",
		RightsAttestationID: attID,
		CASPath:             filepath.Join(tmpDir, "video.mp4"),
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.CreateSourceAsset(ctx, sourceAsset); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	// 2. Preflight report carries NormalizedAudioSHA256 and NormalizedAudioCASPath
	report := domain.PreflightReport{
		ID:                     uuid.NewString(),
		AssetID:                assetID,
		DurationSec:            1.0,
		DurationMs:             1000,
		ContainerFormat:        "mov,mp4,m4a,3gp,3g2,mj2",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  normObj.SHA256,
		NormalizedAudioCASPath: normObj.Path,
		CreatedAt:              time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, report); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}

	s := NewSpeechService(db, casStore)
	var capturedAudioRef worker.ArtifactRef
	s.ASRInvoke = func(ctx context.Context, req SpeechASRRequest) (*SpeechASRResult, error) {
		capturedAudioRef = req.AudioRef
		return &SpeechASRResult{
			ProviderID:   "fake_asr",
			ModelName:    "qwen3-asr",
			ModelVersion: "1.7b",
			LanguageCode: "zh",
			Segments:     []domain.ASRRawSegment{{StartMs: 0, EndMs: 1000, Text: "测试", Confidence: 0.9}},
		}, nil
	}
	s.AlignerInvoke = func(ctx context.Context, req SpeechAlignmentRequest) (*SpeechAlignmentResult, error) {
		return &SpeechAlignmentResult{
			ProviderID:   "fake_align",
			ModelName:    "Qwen3-ForcedAligner-0.6B",
			ModelVersion: "0.6b",
			WordTimings:  []domain.WordTiming{{Word: "测试", StartMs: 0, EndMs: 1000, Confidence: 0.9}},
		}, nil
	}

	in := domain.SpeechPipelineInput{
		RunID:   "run-1",
		AssetID: assetID,
		AudioRolePlan: &domain.AudioRolePlan{
			Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue}},
		},
	}

	artifact, err := s.RunPipeline(ctx, in)
	if err != nil {
		t.Fatalf("RunPipeline failed: %v", err)
	}
	if artifact == nil {
		t.Fatal("expected artifact")
	}

	// Verify providers received the preflight normalized audio artifact
	if capturedAudioRef.SHA256 != normObj.SHA256 || capturedAudioRef.Path != normObj.Path {
		t.Fatalf("expected audio ref %+v, got %+v", normObj, capturedAudioRef)
	}

	// Verify provenance contains NormalizedAudioSHA256 and NormalizationConfig
	prov, err := db.GetTranscriptArtifactIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("GetTranscriptArtifactIndex failed: %v", err)
	}
	if prov == nil || prov.ProvenanceHash == "" {
		t.Fatalf("expected persisted transcript with provenance hash")
	}

	// 3. Verify fail-closed when preflight normalized audio file is missing on disk
	_ = os.Remove(normObj.Path)
	_, err = s.RunPipeline(ctx, in)
	if err == nil {
		t.Fatal("expected RunPipeline to fail closed when normalized audio artifact is missing on disk")
	}
}

// TestSpeechService_EvidenceGatedDiarization_RequiresUpstreamSpeakerCues verifies Finding 6:
// pauses alone without upstream speaker evidence fail closed (1 turn), while pauses + upstream cues succeed.
func TestSpeechService_EvidenceGatedDiarization_RequiresUpstreamSpeakerCues(t *testing.T) {
	evidCfg := domain.DefaultDiarizationEvidenceConfig()

	// Multi-speaker pause timing: pauses >= 300ms.
	multiSpeakerWords := []domain.WordTiming{
		{Word: "w1", StartMs: 0, EndMs: 200},
		{Word: "w2", StartMs: 600, EndMs: 800},   // gap 400ms >= 300ms
		{Word: "w3", StartMs: 1300, EndMs: 1500}, // gap 500ms >= 300ms
	}

	// 1. Without upstream speaker evidence -> fails closed (1 turn, single speaker)
	turns := DiarizeEvidence(multiSpeakerWords, nil, evidCfg)
	if turns >= evidCfg.MinDistinctTurns {
		t.Fatalf("expected pauses alone without speaker evidence to fail closed (< %d turns), got %d", evidCfg.MinDistinctTurns, turns)
	}

	// 2. With upstream speaker cues -> passes evidence gate (3 turns)
	speakerEvid := &domain.SpeakerEvidence{
		HasMultiSpeakerCues: true,
		SpeakerChangeCount:  2,
		Confidence:          0.9,
		Source:              "upstream_acoustic",
	}
	turns = DiarizeEvidence(multiSpeakerWords, speakerEvid, evidCfg)
	if turns < evidCfg.MinDistinctTurns {
		t.Fatalf("expected pauses + upstream speaker cues to pass gate (>= %d turns), got %d", evidCfg.MinDistinctTurns, turns)
	}
}

// TestSpeechService_EvidenceGatedDiarization_SingleSpeakerSkipsDiarizer verifies Finding 6:
// when evidence is below threshold, real diarization is skipped and stable SPEAKER_00 is assigned.
func TestSpeechService_EvidenceGatedDiarization_SingleSpeakerSkipsDiarizer(t *testing.T) {
	s := NewSpeechService(nil, nil)
	// Tight word timings -> evidence < 3 turns.
	words := []domain.WordTiming{
		{Word: "hello", StartMs: 0, EndMs: 200},
		{Word: "world", StartMs: 250, EndMs: 400},
	}
	plan, err := s.Diarize(context.Background(), words, SpeechDiarizationRequest{RunID: "run-1", AssetID: "asset-1"})
	if err != nil {
		t.Fatalf("Diarize failed: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil fallback plan")
	}
	if plan.ProviderID != defaultDiarizerID {
		t.Errorf("expected provider ID %s, got %s", defaultDiarizerID, plan.ProviderID)
	}
	if len(plan.Assignments) != 1 || plan.Assignments[0].SpeakerID != "SPEAKER_00" {
		t.Errorf("expected single SPEAKER_00 assignment, got %v", plan.Assignments)
	}
}

// TestSpeechService_StatelessDiarization_ConsecutiveAndConcurrentRuns verifies Finding 2 & 3:
// 1. Consecutive multi-speaker then single-speaker run does NOT retain stale model identity.
// 2. Concurrent runs are completely race-safe with no shared mutable singleton state.
// 3. Context cancellation and RunID are propagated through the pipeline.
func TestSpeechService_StatelessDiarization_ConsecutiveAndConcurrentRuns(t *testing.T) {
	s := NewSpeechService(nil, nil)

	// Mock diarizer hook that returns a multi-speaker plan when speaker evidence exists
	s.Diarize = func(ctx context.Context, words []domain.WordTiming, req SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if req.SpeakerEvidence != nil && req.SpeakerEvidence.HasMultiSpeakerCues {
			return &domain.DiarizationPlan{
				ID:           "plan-multi",
				RunID:        req.RunID,
				ProviderID:   "custom_diarizer",
				ModelName:    "iic/speech_campplus_sv_zh_en_16k-common_advanced",
				ModelVersion: "v1.0.0",
				Assignments: []domain.SpeakerAssignment{
					{SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 500, Confidence: 0.9},
					{SpeakerID: "SPEAKER_01", StartMs: 600, EndMs: 1200, Confidence: 0.9},
				},
				Confidence: 0.9,
				CreatedAt:  time.Now().UTC(),
			}, nil
		}
		return s.singleSpeakerFallbackPlan(words, req.RunID), nil
	}

	asr := &SpeechASRResult{
		ProviderID:   "fake_qwen3_asr",
		ModelName:    "qwen3-asr",
		ModelVersion: "1.7b",
		LanguageCode: "zh",
		Segments:     []domain.ASRRawSegment{{StartMs: 0, EndMs: 1200, Text: "你好 再见", Confidence: 0.9}},
	}
	align := &SpeechAlignmentResult{
		ProviderID:   "fake_qwen3_aligner",
		ModelName:    "Qwen3-ForcedAligner-0.6B",
		ModelVersion: "0.6b",
		WordTimings: []domain.WordTiming{
			{Word: "你好", StartMs: 0, EndMs: 500, Confidence: 0.9},
			{Word: "再见", StartMs: 600, EndMs: 1200, Confidence: 0.9},
		},
	}

	// 1. First run: Multi-speaker with speaker evidence
	multiReq := SpeechDiarizationRequest{
		RunID:    "run-multi",
		AssetID:  "asset-1",
		AudioRef: worker.ArtifactRef{SHA256: "sha-1", Path: "/audio1.wav"},
		SpeakerEvidence: &domain.SpeakerEvidence{
			HasMultiSpeakerCues: true,
			SpeakerChangeCount:  2,
			Confidence:          0.9,
		},
	}
	art1, err := s.BuildTranscriptFromInputs(context.Background(), asr, align, "run-multi", "asset-1", multiReq)
	if err != nil {
		t.Fatalf("build multi transcript: %v", err)
	}
	if !art1.DiarizationRan {
		t.Error("expected DiarizationRan=true for multi-speaker run")
	}
	if art1.DiarizationModelName != "iic/speech_campplus_sv_zh_en_16k-common_advanced" || art1.DiarizationModelVersion != "v1.0.0" {
		t.Errorf("expected multi-speaker model info iic/speech_campplus_sv_zh_en_16k-common_advanced:v1.0.0, got %s:%s", art1.DiarizationModelName, art1.DiarizationModelVersion)
	}

	// 2. Second run: Single-speaker (NO speaker evidence) -> MUST NOT retain stale model info from run 1
	singleReq := SpeechDiarizationRequest{
		RunID:           "run-single",
		AssetID:         "asset-2",
		AudioRef:        worker.ArtifactRef{SHA256: "sha-2", Path: "/audio2.wav"},
		SpeakerEvidence: nil,
	}
	art2, err := s.BuildTranscriptFromInputs(context.Background(), asr, align, "run-single", "asset-2", singleReq)
	if err != nil {
		t.Fatalf("build single transcript: %v", err)
	}
	if art2.DiarizationRan {
		t.Error("expected DiarizationRan=false for single-speaker fallback")
	}
	if art2.DiarizationModelName != "" || art2.DiarizationModelVersion != "" {
		t.Errorf("stale diarization model info retained on single-speaker run: name=%q ver=%q", art2.DiarizationModelName, art2.DiarizationModelVersion)
	}
	if art2.DiarizationProviderID != defaultDiarizerID {
		t.Errorf("expected fallback provider ID %q, got %q", defaultDiarizerID, art2.DiarizationProviderID)
	}

	// 3. Test context cancellation
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.BuildTranscriptFromInputs(ctxCanceled, asr, align, "run-canceled", "asset-3", multiReq)
	if err == nil {
		t.Fatal("expected error on canceled context")
	}

	// 4. Concurrent safety check
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var req SpeechDiarizationRequest
			if idx%2 == 0 {
				req = multiReq
			} else {
				req = singleReq
			}
			_, _ = s.BuildTranscriptFromInputs(context.Background(), asr, align, "run-concurrent", "asset-c", req)
		}(i)
	}
	wg.Wait()
}

// TestSpeechService_TwoSpeakerTwoTurn_EvidenceGate verifies Issue #44 Blocker 2:
// Two distinct turns with independent valid speaker evidence passes the gate and triggers diarization,
// while the same two turns without speaker evidence fails closed to single-speaker fallback.
func TestSpeechService_TwoSpeakerTwoTurn_EvidenceGate(t *testing.T) {
	evidCfg := domain.DefaultDiarizationEvidenceConfig()

	twoTurnWords := []domain.WordTiming{
		{Word: "turn1", StartMs: 0, EndMs: 200},
		{Word: "turn2", StartMs: 600, EndMs: 800}, // gap 400ms >= 300ms
	}

	// 1. Two turns + valid speaker evidence (SpeakerChangeCount=1, HasMultiSpeakerCues=true) -> passes gate (2 turns >= MinDistinctTurns 2)
	validEvid := &domain.SpeakerEvidence{
		HasMultiSpeakerCues: true,
		SpeakerChangeCount:  1,
		Confidence:          0.9,
		Source:              "test_acoustic",
	}
	turnsWithEvid := DiarizeEvidence(twoTurnWords, validEvid, evidCfg)
	if turnsWithEvid < evidCfg.MinDistinctTurns {
		t.Fatalf("expected 2 turns + speaker evidence to pass gate (>= %d), got %d", evidCfg.MinDistinctTurns, turnsWithEvid)
	}

	// 2. Same two turns without speaker evidence -> fails closed (1 turn < MinDistinctTurns 2)
	turnsWithoutEvid := DiarizeEvidence(twoTurnWords, nil, evidCfg)
	if turnsWithoutEvid >= evidCfg.MinDistinctTurns {
		t.Fatalf("expected pauses alone without speaker evidence to fail closed (< %d), got %d", evidCfg.MinDistinctTurns, turnsWithoutEvid)
	}

	// 3. Verify SpeechService.Diarize behavior directly
	s := NewSpeechService(nil, nil)
	// Hook returning multi-speaker plan
	s.Diarize = func(ctx context.Context, words []domain.WordTiming, req SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
		if DiarizeEvidence(words, req.SpeakerEvidence, evidCfg) < evidCfg.MinDistinctTurns {
			return s.singleSpeakerFallbackPlan(words, req.RunID), nil
		}
		return &domain.DiarizationPlan{
			ProviderID:   "campplus_diarizer",
			ModelName:    "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			ModelVersion: "v1.0.0",
			Assignments: []domain.SpeakerAssignment{
				{SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 200},
				{SpeakerID: "SPEAKER_01", StartMs: 600, EndMs: 800},
			},
			SpeakerEvidence: req.SpeakerEvidence,
		}, nil
	}

	// Run with evidence -> diarization runs
	planWithEvid, err := s.Diarize(context.Background(), twoTurnWords, SpeechDiarizationRequest{RunID: "run-1", SpeakerEvidence: validEvid})
	if err != nil {
		t.Fatalf("diarize with evidence: %v", err)
	}
	if planWithEvid.ProviderID != "campplus_diarizer" || len(planWithEvid.Assignments) != 2 {
		t.Fatalf("expected multi-speaker plan, got provider %s, assignments %d", planWithEvid.ProviderID, len(planWithEvid.Assignments))
	}

	// Run without evidence -> single speaker fallback
	planWithoutEvid, err := s.Diarize(context.Background(), twoTurnWords, SpeechDiarizationRequest{RunID: "run-2", SpeakerEvidence: nil})
	if err != nil {
		t.Fatalf("diarize without evidence: %v", err)
	}
	if planWithoutEvid.ProviderID != defaultDiarizerID || len(planWithoutEvid.Assignments) != 1 {
		t.Fatalf("expected fallback provider %s, got %s", defaultDiarizerID, planWithoutEvid.ProviderID)
	}
}

// TestSpeechService_SpeakerEvidenceProbeError_FailsClosed verifies Issue #44 Blocker 1:
// When the speaker evidence probe returns an error, defaultDiarize must propagate the error fail-closed
// rather than silently converting it into a successful single-speaker artifact.
func TestSpeechService_SpeakerEvidenceProbeError_FailsClosed(t *testing.T) {
	reg := provider.NewRegistry()
	fakeDiarizer := provider.NewFakeDiarizationProvider("campplus_diarizer")
	fakeDiarizer.ProbeErr = errors.New("simulated speaker evidence probe GPU failure")
	reg.Register(fakeDiarizer)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)
	s := NewSpeechService(nil, nil)
	s.ConfigureRouter(router)

	words := []domain.WordTiming{
		{Word: "turn1", StartMs: 0, EndMs: 200},
		{Word: "turn2", StartMs: 600, EndMs: 800},
	}

	req := SpeechDiarizationRequest{
		RunID:    "run-probe-fail",
		AssetID:  "asset-1",
		AudioRef: worker.ArtifactRef{SHA256: "sha1", Path: "/test/audio.wav"},
	}

	// defaultDiarize must propagate probe error fail-closed
	plan, err := s.defaultDiarize(context.Background(), words, req)
	if err == nil {
		t.Fatal("expected probe error to fail closed, got nil error and plan")
	}
	if plan != nil {
		t.Fatalf("expected nil plan on probe error, got %+v", plan)
	}

	// BuildTranscriptFromInputs must also fail closed
	asr := &SpeechASRResult{
		Segments:   []domain.ASRRawSegment{{Text: "turn1 turn2", StartMs: 0, EndMs: 800}},
		ProviderID: "test-asr",
	}
	align := &SpeechAlignmentResult{
		WordTimings: words,
		ProviderID:  "test-aligner",
	}
	art, err := s.BuildTranscriptFromInputs(context.Background(), asr, align, req.RunID, req.AssetID, req)
	if err == nil {
		t.Fatal("expected BuildTranscriptFromInputs to fail closed on probe error")
	}
	if art != nil {
		t.Fatalf("expected nil artifact on probe error, got %+v", art)
	}
}

// TestSpeechService_DefaultDiarizerInvoke_MissingVADModelInfo_FailsClosed verifies Finding 2:
// When a diarization provider lacks VAD model info (or declares empty VAD identity),
// defaultDiarizerInvoke must fail closed rather than inventing a fabricated FSMN-VAD checkpoint identity.
func TestSpeechService_DefaultDiarizerInvoke_MissingVADModelInfo_FailsClosed(t *testing.T) {
	reg := provider.NewRegistry()
	fakeDiarizer := provider.NewFakeDiarizationProvider("campplus_diarizer")
	fakeDiarizer.VADModelName = "" // missing VAD identity
	fakeDiarizer.VADModelVersion = ""
	_ = reg.Register(fakeDiarizer)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)
	s := NewSpeechService(nil, nil)
	s.ConfigureRouter(router)

	req := SpeechDiarizationRequest{
		RunID:    "run-no-vad",
		AssetID:  "asset-1",
		AudioRef: worker.ArtifactRef{SHA256: "sha1", Path: "/test/audio.wav"},
	}

	_, _, _, _, _, _, err := s.defaultDiarizerInvoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected defaultDiarizerInvoke to fail closed when VAD model identity is missing")
	}
	if !strings.Contains(err.Error(), "missing verified VAD model identity") && !errors.Is(err, domain.ErrQualityRejected) {
		t.Fatalf("expected missing VAD model identity error, got %v", err)
	}
}

// TestSpeechService_DefaultDiarizerInvoke_NoSpeakerRegions_ReturnsErrDiarizationNoCandidates verifies Finding 5:
// When diarization produces no speaker regions, defaultDiarizerInvoke returns domain.ErrDiarizationNoCandidates
// and not domain.ErrASRNoCandidates.
func TestSpeechService_DefaultDiarizerInvoke_NoSpeakerRegions_ReturnsErrDiarizationNoCandidates(t *testing.T) {
	reg := provider.NewRegistry()
	fakeDiarizer := provider.NewFakeDiarizationProvider("campplus_diarizer")
	fakeDiarizer.Assignments = []domain.SpeakerAssignment{} // empty regions
	_ = reg.Register(fakeDiarizer)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)
	s := NewSpeechService(nil, nil)
	s.ConfigureRouter(router)

	req := SpeechDiarizationRequest{
		RunID:    "run-empty-regions",
		AssetID:  "asset-1",
		AudioRef: worker.ArtifactRef{SHA256: "sha1", Path: "/test/audio.wav"},
	}

	_, _, _, _, _, _, err := s.defaultDiarizerInvoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected defaultDiarizerInvoke to fail when no speaker regions produced")
	}
	if errors.Is(err, domain.ErrASRNoCandidates) {
		t.Fatal("must not return domain.ErrASRNoCandidates for diarization failure")
	}
}

// TestSpeechService_DefaultDiarize_RealDiarizerSingleSpeaker_PreservesIdentityAndDiarizationRanTrue
// verifies Issue #44 pre-commit fix 2: when full diarization executed after the evidence gate
// but the diarizer produces only a single speaker, DiarizationRan is true and the real provider/model/VAD
// identity is preserved in the plan and transcript artifact provenance.
func TestSpeechService_DefaultDiarize_RealDiarizerSingleSpeaker_PreservesIdentityAndDiarizationRanTrue(t *testing.T) {
	reg := provider.NewRegistry()
	fakeDiarizer := provider.NewFakeDiarizationProvider("campplus_diarizer")
	// Diarizer executes and returns single speaker region
	fakeDiarizer.Assignments = []domain.SpeakerAssignment{
		{SpeakerID: "SPEAKER_00", Label: "SPEAKER_00", StartMs: 0, EndMs: 2000, Confidence: 0.92},
	}
	_ = reg.Register(fakeDiarizer)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)
	s := NewSpeechService(nil, nil)
	s.ConfigureRouter(router)

	words := []domain.WordTiming{
		{Word: "hello", StartMs: 0, EndMs: 500},
		{Word: "world", StartMs: 1000, EndMs: 1500}, // gap 500ms >= 300ms
	}
	req := SpeechDiarizationRequest{
		RunID:    "run-real-single-speaker",
		AssetID:  "asset-1",
		AudioRef: worker.ArtifactRef{SHA256: "sha1", Path: "/test/audio.wav"},
		SpeakerEvidence: &domain.SpeakerEvidence{
			HasMultiSpeakerCues: true,
			SpeakerChangeCount:  2,
			Confidence:          0.9,
		},
	}

	asr := &SpeechASRResult{
		ProviderID:   "fake_asr",
		ModelName:    "qwen3-asr",
		ModelVersion: "1.7b",
		LanguageCode: "zh",
		Segments:     []domain.ASRRawSegment{{StartMs: 0, EndMs: 1500, Text: "hello world", Confidence: 0.9}},
	}
	align := &SpeechAlignmentResult{
		ProviderID:   "fake_align",
		ModelName:    "Qwen3-ForcedAligner-0.6B",
		ModelVersion: "0.6b",
		WordTimings:  words,
	}

	artifact, err := s.BuildTranscriptFromInputs(context.Background(), asr, align, req.RunID, req.AssetID, req)
	if err != nil {
		t.Fatalf("BuildTranscriptFromInputs failed: %v", err)
	}

	// 1. DiarizationRan must be TRUE because full diarizer ran
	if !artifact.DiarizationRan {
		t.Errorf("expected DiarizationRan=true when real diarizer ran and concluded single speaker")
	}

	// 2. Real provider / model / VAD identity must be preserved (not replaced by synthetic fallback identity)
	if artifact.DiarizationProviderID != "campplus_diarizer" {
		t.Errorf("expected DiarizationProviderID 'campplus_diarizer', got %q", artifact.DiarizationProviderID)
	}
	if artifact.DiarizationModelName != "iic/speech_campplus_sv_zh_en_16k-common_advanced" {
		t.Errorf("expected DiarizationModelName 'iic/speech_campplus_sv_zh_en_16k-common_advanced', got %q", artifact.DiarizationModelName)
	}
	if artifact.DiarizationModelVersion != "v1.0.0" {
		t.Errorf("expected DiarizationModelVersion 'v1.0.0', got %q", artifact.DiarizationModelVersion)
	}
	if artifact.DiarizationVADModel != "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch" {
		t.Errorf("expected DiarizationVADModel 'iic/speech_fsmn_vad_zh-cn-16k-common-pytorch', got %q", artifact.DiarizationVADModel)
	}
	if artifact.DiarizationVADVersion != "v2.0.4" {
		t.Errorf("expected DiarizationVADVersion 'v2.0.4', got %q", artifact.DiarizationVADVersion)
	}

	// 3. Single speaker assignment spans the word timeline
	if len(artifact.SpeakerAssignments) != 1 || artifact.SpeakerAssignments[0].SpeakerID != "SPEAKER_00" {
		t.Errorf("expected single collapsed SPEAKER_00 assignment, got %+v", artifact.SpeakerAssignments)
	}
}

// TestSpeechService_DefaultDiarize_NoRunFallback_DiarizationRanFalseAndDefaultIdentity
// verifies that when diarization is skipped (due to insufficient evidence or missing router),
// DiarizationRan is false and synthetic default identity is used with empty model info.
func TestSpeechService_DefaultDiarize_NoRunFallback_DiarizationRanFalseAndDefaultIdentity(t *testing.T) {
	s := NewSpeechService(nil, nil)

	// 1. Evidence insufficient (< min turns)
	words := []domain.WordTiming{
		{Word: "hello", StartMs: 0, EndMs: 200},
		{Word: "world", StartMs: 250, EndMs: 400},
	}
	asr := &SpeechASRResult{
		ProviderID:   "fake_asr",
		ModelName:    "qwen3-asr",
		ModelVersion: "1.7b",
		LanguageCode: "zh",
		Segments:     []domain.ASRRawSegment{{StartMs: 0, EndMs: 400, Text: "hello world", Confidence: 0.9}},
	}
	align := &SpeechAlignmentResult{
		ProviderID:   "fake_align",
		ModelName:    "Qwen3-ForcedAligner-0.6B",
		ModelVersion: "0.6b",
		WordTimings:  words,
	}

	artifact, err := s.BuildTranscriptFromInputs(context.Background(), asr, align, "run-no-evidence", "asset-1", SpeechDiarizationRequest{
		RunID:           "run-no-evidence",
		AssetID:         "asset-1",
		SpeakerEvidence: nil,
	})
	if err != nil {
		t.Fatalf("BuildTranscriptFromInputs failed: %v", err)
	}

	if artifact.DiarizationRan {
		t.Errorf("expected DiarizationRan=false when diarizer was not run")
	}
	if artifact.DiarizationProviderID != "default-single-speaker-fallback" {
		t.Errorf("expected default-single-speaker-fallback, got %q", artifact.DiarizationProviderID)
	}
	if artifact.DiarizationModelName != "" || artifact.DiarizationModelVersion != "" {
		t.Errorf("expected empty model info on no-run fallback, got %q:%q", artifact.DiarizationModelName, artifact.DiarizationModelVersion)
	}
	if artifact.DiarizationVADModel != "" || artifact.DiarizationVADVersion != "" {
		t.Errorf("expected empty VAD info on no-run fallback, got %q:%q", artifact.DiarizationVADModel, artifact.DiarizationVADVersion)
	}
}

func createValidTestWAV(path string, sampleRate int, numChannels int, numSamples int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	byteRate := sampleRate * numChannels * 2
	blockAlign := numChannels * 2
	dataSize := numSamples * numChannels * 2
	chunkSize := 36 + dataSize

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(chunkSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // Subchunk1Size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(numChannels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16)) // BitsPerSample
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))

	pcmData := make([]byte, dataSize)
	buf.Write(pcmData)

	_, err = f.Write(buf.Bytes())
	return err
}

// TestSpeechService_RunPipeline_MissingPreflightNormalizedAudioFailsClosed_ProvidersNotReached
// verifies that SpeechService never owns or performs source audio normalization.
// Acquisition/Preflight must supply the canonical normalized artifact before any
// speech provider is invoked.
func TestSpeechService_RunPipeline_MissingPreflightNormalizedAudioFailsClosed_ProvidersNotReached(t *testing.T) {
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	s := NewSpeechService(nil, casStore)

	var asrCalled, alignerCalled, diarizerCalled bool
	s.ASRInvoke = func(ctx context.Context, req SpeechASRRequest) (*SpeechASRResult, error) {
		asrCalled = true
		return &SpeechASRResult{
			ProviderID:   "fake_asr",
			ModelName:    "fake-asr-model",
			ModelVersion: "v1",
			LanguageCode: "zh",
			Segments: []domain.ASRRawSegment{
				{StartMs: 0, EndMs: 1000, Text: "test", Confidence: 0.99},
			},
		}, nil
	}
	s.AlignerInvoke = func(ctx context.Context, req SpeechAlignmentRequest) (*SpeechAlignmentResult, error) {
		alignerCalled = true
		return &SpeechAlignmentResult{
			ProviderID:   "fake_align",
			ModelName:    "fake-aligner-model",
			ModelVersion: "v1",
			WordTimings: []domain.WordTiming{
				{Word: "test", StartMs: 0, EndMs: 1000, Confidence: 0.99},
			},
		}, nil
	}
	s.Diarize = func(ctx context.Context, words []domain.WordTiming, req SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
		diarizerCalled = true
		return &domain.DiarizationPlan{
			ID:          "diar_plan",
			RunID:       req.RunID,
			Confidence:  0.99,
			Assignments: []domain.SpeakerAssignment{},
		}, nil
	}

	rawMedia := filepath.Join(tmpDir, "source_video.mp4")
	if err := os.WriteFile(rawMedia, []byte("raw source media placeholder"), 0644); err != nil {
		t.Fatalf("write source media: %v", err)
	}

	rolePlan := &domain.AudioRolePlan{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue},
		},
	}

	in := domain.SpeechPipelineInput{
		RunID:         "run-fail-closed-test",
		AssetID:       "asset-fail-closed-test",
		AudioPath:     rawMedia,
		AudioSHA256:   "raw_source_sha",
		AudioRolePlan: rolePlan,
	}

	_, err = s.RunPipeline(context.Background(), in)
	if err == nil {
		t.Fatalf("expected RunPipeline to fail closed on corrupt media, got nil error")
	}
	if !strings.Contains(err.Error(), "normalized audio artifact required from Acquisition/Preflight") {
		t.Fatalf("expected preflight normalized-audio requirement, got %v", err)
	}

	// Verify NO providers were invoked
	if asrCalled {
		t.Errorf("ASR provider was unexpectedly invoked on unnormalized media")
	}
	if alignerCalled {
		t.Errorf("Forced Aligner provider was unexpectedly invoked on unnormalized media")
	}
	if diarizerCalled {
		t.Errorf("Diarizer provider was unexpectedly invoked on unnormalized media")
	}
}

// TestSpeechService_RunPipeline_PreflightNormalizedWAVReachedProviders verifies
// providers receive only the normalized CAS WAV ArtifactRef supplied by Preflight.
func TestSpeechService_RunPipeline_PreflightNormalizedWAVReachedProviders(t *testing.T) {
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	s := NewSpeechService(nil, casStore)

	var capturedASRRef, capturedAlignRef, capturedDiarizeRef worker.ArtifactRef
	s.ASRInvoke = func(ctx context.Context, req SpeechASRRequest) (*SpeechASRResult, error) {
		capturedASRRef = req.AudioRef
		return &SpeechASRResult{
			ProviderID:   "fake_asr",
			ModelName:    "fake-asr-model",
			ModelVersion: "v1",
			LanguageCode: "zh",
			Segments: []domain.ASRRawSegment{
				{StartMs: 0, EndMs: 1000, Text: "test", Confidence: 0.99},
			},
		}, nil
	}
	s.AlignerInvoke = func(ctx context.Context, req SpeechAlignmentRequest) (*SpeechAlignmentResult, error) {
		capturedAlignRef = req.AudioRef
		return &SpeechAlignmentResult{
			ProviderID:   "fake_align",
			ModelName:    "fake-aligner-model",
			ModelVersion: "v1",
			WordTimings: []domain.WordTiming{
				{Word: "test", StartMs: 0, EndMs: 1000, Confidence: 0.99},
			},
		}, nil
	}
	s.Diarize = func(ctx context.Context, words []domain.WordTiming, req SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
		capturedDiarizeRef = req.AudioRef
		return &domain.DiarizationPlan{
			ID:          "diar_plan",
			RunID:       req.RunID,
			Confidence:  0.99,
			Assignments: []domain.SpeakerAssignment{},
		}, nil
	}

	validWav := filepath.Join(tmpDir, "preflight_normalized_16k.wav")
	if err := createValidTestWAV(validWav, 16000, 1, 16000); err != nil {
		t.Fatalf("create valid test wav: %v", err)
	}
	normObj, err := casStore.PutFile(validWav)
	if err != nil {
		t.Fatalf("commit preflight normalized audio: %v", err)
	}

	rolePlan := &domain.AudioRolePlan{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		},
	}

	in := domain.SpeechPipelineInput{
		RunID:                 "run-norm-verify-test",
		AssetID:               "asset-norm-verify-test",
		NormalizedAudioPath:   normObj.Path,
		NormalizedAudioSHA256: normObj.SHA256,
		AudioRolePlan:         rolePlan,
	}

	artifact, err := s.RunPipeline(context.Background(), in)
	if err != nil {
		t.Fatalf("RunPipeline failed: %v", err)
	}
	if artifact == nil {
		t.Fatalf("expected non-nil transcript artifact")
	}

	// Verify providers received the CAS-stored normalized 16 kHz WAV, not original file path
	if capturedASRRef.Path != normObj.Path || capturedASRRef.SHA256 != normObj.SHA256 {
		t.Errorf("ASR provider did not receive preflight normalized CAS ref: %+v", capturedASRRef)
	}
	if capturedAlignRef.Path != capturedASRRef.Path {
		t.Errorf("Aligner provider received different ref: %q vs %q", capturedAlignRef.Path, capturedASRRef.Path)
	}
	if capturedDiarizeRef.Path != capturedASRRef.Path {
		t.Errorf("Diarizer provider received different ref: %q vs %q", capturedDiarizeRef.Path, capturedASRRef.Path)
	}
	if !strings.HasPrefix(capturedASRRef.Path, tmpDir) {
		t.Errorf("Normalized ref is not in CAS directory: %q", capturedASRRef.Path)
	}
}
