package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
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
		if mName == "" {
			continue
		}
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
	})
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
	plan, err = s.Diarize(context.Background(), nil)
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
		RunID:     "run-test-fallback",
		AssetID:   "asset-1",
		AudioPath: "/audio.wav",
		Language:  "zh",
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
	})
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
	artifact, err := s.BuildTranscriptFromInputs(asr, align, "run-1", "asset-1")
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
// provider or DiarizationRan changes the provenance hash.
func TestSpeechService_ProvenanceHashChangesWithDiarization(t *testing.T) {
	cfg := domain.DefaultSegmentRuleConfig()
	a := domain.TranscriptProvenance{
		AssetSHA256:           "sha",
		ASRProviderID:         "p1",
		ASRModelName:          "qwen3-asr",
		ASRModelVersion:       "1.7b",
		AlignerProviderID:     "p2",
		AlignerModelName:      "qwen3-aligner",
		AlignerModelVersion:   "1.0.0",
		SegmentConfig:         cfg,
		DiarizationProviderID: "diarizer-a",
		DiarizationRan:        true,
		SchemaVersion:         domain.TranscriptSchemaVersion,
	}

	if a.Hash() == "" {
		t.Error("provenance hash must not be empty")
	}

	// Changing the diarizer identity must change the hash.
	b := a
	b.DiarizationProviderID = "diarizer-b"
	if a.Hash() == b.Hash() {
		t.Error("changing diarization provider must change provenance hash")
	}

	// Changing DiarizationRan must change the hash.
	c := a
	c.DiarizationRan = false
	if a.Hash() == c.Hash() {
		t.Error("changing DiarizationRan must change provenance hash")
	}
}
