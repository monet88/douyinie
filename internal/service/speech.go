package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/speech"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
)

// SpeechASRResult is the provider-agnostic output of one ASR attempt.
// It carries the accepted transcript: raw VAD-turn segments with text and
// timestamps, plus the provider/model identity for provenance.
type SpeechASRResult struct {
	ProviderID   string
	ModelName    string
	ModelVersion string
	LanguageCode string
	Segments     []domain.ASRRawSegment
}

// SpeechAlignmentResult is the provider-agnostic output of the forced aligner.
type SpeechAlignmentResult struct {
	ProviderID   string
	ModelName    string
	ModelVersion string
	WordTimings  []domain.WordTiming
}

// SpeechService orchestrates the canonical speech understanding pipeline:
//
//	accepted transcript (ASR) -> forced alignment -> conditional diarization
//	-> canonical SpeechBlock segmentation -> TranscriptArtifact (with speaker labels)
//
// Diarization/speaker assignment runs BEFORE canonical SpeechBlock segmentation
// (per #18 canonical pipeline order) and only when speaker evidence warrants it.
type SpeechService struct {
	db     *storage.DB
	cas    *cas.Store
	router *provider.Router

	// ASRInvoke executes one ASR provider attempt and returns the accepted
	// transcript (or an error). When nil, the router-backed default
	// (defaultASRInvoke) is used once ConfigureRouter has been called.
	ASRInvoke func(ctx context.Context, req SpeechASRRequest) (*SpeechASRResult, error)

	// AlignerInvoke executes one forced-alignment attempt.
	// When nil, the router-backed default (defaultAlignerInvoke) is used.
	AlignerInvoke func(ctx context.Context, req SpeechAlignmentRequest) (*SpeechAlignmentResult, error)

	// Diarize performs conditional diarization over the accepted word timings.
	// It may return nil when speaker evidence does not warrant diarization.
	// The evidence-gated default (defaultDiarize) is installed at construction.
	// It receives explicit per-request values to guarantee concurrency safety (Finding 2).
	Diarize func(ctx context.Context, words []domain.WordTiming, req SpeechDiarizationRequest) (*domain.DiarizationPlan, error)

	// SegmentConfig supplies the canonical segmentation rules.
	SegmentConfig func() domain.SegmentRuleConfig

	// EvidenceConfig governs the pre-diarization multi-speaker evidence gate;
	// it hashes into transcript provenance (Issue #44 Finding 4).
	EvidenceConfig func() domain.DiarizationEvidenceConfig
}

// DiarizeEvidence evaluates whether pre-diarization word timings and upstream
// speaker evidence justify invoking a real diarization provider.
//
// Semantics:
//  1. Pauses alone (gaps >= MinTurnGapMs) count distinct temporal speech turns as supporting timing evidence.
//  2. If RequireSpeakerCues is true, timing turns alone are INSUFFICIENT: valid upstream speaker evidence
//     (HasMultiSpeakerCues == true && Confidence >= MinEvidenceConfidence) is required.
//     A single narrator pausing between sentences fails closed (returns 1 turn / below threshold),
//     preventing false-positive multi-speaker diarization invocations (Finding 6).
func DiarizeEvidence(words []domain.WordTiming, evidence *domain.SpeakerEvidence, cfg domain.DiarizationEvidenceConfig) int {
	if len(words) == 0 {
		return 0
	}
	pauseTurns := 1
	for i := 1; i < len(words); i++ {
		gap := words[i].StartMs - words[i-1].EndMs
		if gap >= cfg.MinTurnGapMs {
			pauseTurns++
		}
	}
	if cfg.RequireSpeakerCues {
		if evidence == nil || !evidence.HasMultiSpeakerCues || (cfg.MinEvidenceConfidence > 0 && evidence.Confidence < cfg.MinEvidenceConfidence) {
			return 1
		}
		if evidence.SpeakerChangeCount > 0 && evidence.SpeakerChangeCount+1 < pauseTurns {
			return evidence.SpeakerChangeCount + 1
		}
	}
	return pauseTurns
}

// defaultDiarizerID is the provenance identity of the single-speaker fallback
// speaker-assignment default. It is NOT a real diarization provider: it only
// assigns a canonical single speaker so speaker labels exist before canonical
// segmentation. DiarizationRan stays false when only this fallback ran.
const defaultDiarizerID = "default-single-speaker-fallback"

// SpeechASRRequest is the input for one ASR provider attempt.
type SpeechASRRequest struct {
	RunID    string
	AssetID  string
	AudioRef worker.ArtifactRef
	Language string
}

// SpeechAlignmentRequest is the input for one forced-alignment attempt.
type SpeechAlignmentRequest struct {
	RunID    string
	AssetID  string
	AudioRef worker.ArtifactRef
	Text     string
}

// SpeechDiarizationRequest is the input for one conditional diarization attempt.
type SpeechDiarizationRequest struct {
	RunID           string
	AssetID         string
	AudioRef        worker.ArtifactRef
	SpeakerEvidence *domain.SpeakerEvidence
}

// NewSpeechService creates a SpeechService with the evidence-gated conditional
// diarization default installed. Callers may override ASRInvoke/AlignerInvoke/
// Diarize; ConfigureRouter installs router-backed ASR/aligner defaults.
// The cas store is used to persist TranscriptArtifacts as content-addressed
// blobs; it may be nil when persistence is not required.
func NewSpeechService(db *storage.DB, casStore *cas.Store) *SpeechService {
	s := &SpeechService{
		db:             db,
		cas:            casStore,
		SegmentConfig:  domain.DefaultSegmentRuleConfig,
		EvidenceConfig: domain.DefaultDiarizationEvidenceConfig,
	}
	s.Diarize = s.defaultDiarize
	return s
}

// ConfigureRouter wires router-backed ASR/aligner invocation defaults. It
// only sets defaults for hooks the caller left nil, so explicit Seam 1 test
// overrides are never clobbered. Call it from the composition root (server.New)
// whenever both a SpeechService and a Router exist.
func (s *SpeechService) ConfigureRouter(router *provider.Router) {
	if router == nil {
		return
	}
	s.router = router
	if s.ASRInvoke == nil {
		s.ASRInvoke = s.defaultASRInvoke
	}
	if s.AlignerInvoke == nil {
		s.AlignerInvoke = s.defaultAlignerInvoke
	}
}

// BuildTranscriptFromInputs runs the canonical pipeline over already-produced
// provider outputs (used by the Seam 1 API path where the fake providers are
// driven directly and deterministically).
func (s *SpeechService) BuildTranscriptFromInputs(
	ctx context.Context,
	asr *SpeechASRResult,
	align *SpeechAlignmentResult,
	runID string,
	assetID string,
	opts ...SpeechDiarizationRequest,
) (*domain.TranscriptArtifact, error) {
	if asr == nil || len(asr.Segments) == 0 {
		return nil, domain.ErrASRNoCandidates
	}
	if align == nil || len(align.WordTimings) == 0 {
		return nil, domain.ErrAlignmentFailed
	}
	if ctx == nil {
		ctx = context.Background()
	}

	diarizeReq := SpeechDiarizationRequest{
		RunID:   runID,
		AssetID: assetID,
	}
	if len(opts) > 0 {
		diarizeReq = opts[0]
		if diarizeReq.RunID == "" {
			diarizeReq.RunID = runID
		}
		if diarizeReq.AssetID == "" {
			diarizeReq.AssetID = assetID
		}
	}

	// 1. Conditional diarization BEFORE canonical segmentation.
	var assignments []domain.SpeakerAssignment
	var diarizationRan bool
	var diarizationProviderID string
	var diarizationModelName string
	var diarizationModelVersion string
	var diarizationVADModel string
	var diarizationVADVersion string
	var speakerEvidence *domain.SpeakerEvidence = diarizeReq.SpeakerEvidence
	if s.Diarize != nil {
		plan, err := s.Diarize(ctx, align.WordTimings, diarizeReq)
		if err != nil {
			return nil, fmt.Errorf("conditional diarization failed: %w", err)
		}
		if plan != nil {
			assignments = plan.Assignments
			diarizationRan = plan.ProviderID != defaultDiarizerID && (plan.ProviderID != "" || hasMultiSpeakerEvidence(assignments))
			diarizationProviderID = plan.ProviderID
			diarizationModelName = plan.ModelName
			diarizationModelVersion = plan.ModelVersion
			diarizationVADModel = plan.VADModelName
			diarizationVADVersion = plan.VADModelVersion
			if plan.SpeakerEvidence != nil {
				speakerEvidence = plan.SpeakerEvidence
			}
		}
	}

	// 2. Apply speaker assignments to word timings (speaker labels flow into
	//    canonical segmentation).
	words := applySpeakerAssignments(align.WordTimings, assignments)

	// 3. Canonical SpeechBlock segmentation via semantic/punctuation/pause rules.
	cfg := s.SegmentConfig()
	blocks := speech.BuildSpeechBlocks(words, cfg)

	return &domain.TranscriptArtifact{
		ID:                      uuid.NewString(),
		AssetID:                 assetID,
		RunID:                   runID,
		RawSegments:             asr.Segments,
		WordTimings:             words,
		SpeakerAssignments:      assignments,
		SpeakerEvidence:         speakerEvidence,
		SpeechBlocks:            blocks,
		ASRProviderID:           asr.ProviderID,
		AlignerProviderID:       align.ProviderID,
		DiarizationRan:          diarizationRan,
		DiarizationProviderID:   diarizationProviderID,
		DiarizationModelName:    diarizationModelName,
		DiarizationModelVersion: diarizationModelVersion,
		DiarizationVADModel:     diarizationVADModel,
		DiarizationVADVersion:   diarizationVADVersion,
		SourceLanguage:          asr.LanguageCode,
		CreatedAt:               time.Now().UTC(),
	}, nil
}

// RunPipeline executes the full speech understanding pipeline through injected
// provider invocations and persists the resulting TranscriptArtifact.
func (s *SpeechService) RunPipeline(ctx context.Context, in domain.SpeechPipelineInput) (*domain.TranscriptArtifact, error) {
	if in.RunID == "" || in.AssetID == "" {
		return nil, errors.New("run_id and asset_id are required")
	}
	// Fail closed on a missing audio role plan and on no dub-eligible speech.
	if in.AudioRolePlan == nil {
		return nil, domain.ErrAudioRolePlanRequired
	}
	if !domain.IsDubEligible(in.AudioRolePlan) {
		return nil, domain.ErrNoDubEligibleSpeech
	}

	// Resolve source-derived Acquisition/Preflight normalized 16 kHz mono WAV CAS artifact.
	var normalizedRef worker.ArtifactRef
	if s.db != nil {
		report, err := s.db.GetPreflightReport(ctx, in.AssetID)
		if err != nil {
			return nil, fmt.Errorf("preflight report required for asset %s: %w", in.AssetID, err)
		}
		if report.NormalizedAudioSHA256 == "" || report.NormalizedAudioCASPath == "" {
			return nil, fmt.Errorf("normalized audio artifact missing from preflight report for asset %s", in.AssetID)
		}
		if _, err := os.Stat(report.NormalizedAudioCASPath); err != nil {
			return nil, fmt.Errorf("normalized audio CAS artifact missing at %s: %w", report.NormalizedAudioCASPath, err)
		}
		normalizedRef = worker.ArtifactRef{
			SHA256: report.NormalizedAudioSHA256,
			Path:   report.NormalizedAudioCASPath,
		}
	} else if in.NormalizedAudioSHA256 != "" && in.NormalizedAudioPath != "" {
		if _, err := os.Stat(in.NormalizedAudioPath); err != nil {
			return nil, fmt.Errorf("preflight normalized audio artifact missing at %s: %w", in.NormalizedAudioPath, err)
		}
		normalizedRef = worker.ArtifactRef{
			SHA256: in.NormalizedAudioSHA256,
			Path:   in.NormalizedAudioPath,
		}
	} else {
		return nil, errors.New("normalized audio artifact required from Acquisition/Preflight")
	}

	audioRef := normalizedRef
	asrReq := SpeechASRRequest{
		RunID:    in.RunID,
		AssetID:  in.AssetID,
		AudioRef: audioRef,
		Language: "zh",
	}
	asr, err := s.invokeASR(ctx, asrReq)
	if err != nil {
		return nil, fmt.Errorf("ASR failed: %w", err)
	}

	acceptedText := joinASRSegmentText(asr.Segments)
	alignReq := SpeechAlignmentRequest{
		RunID:    in.RunID,
		AssetID:  in.AssetID,
		AudioRef: audioRef,
		Text:     acceptedText,
	}
	align, err := s.invokeAligner(ctx, alignReq)
	if err != nil {
		return nil, fmt.Errorf("forced alignment failed: %w", err)
	}

	diarizeReq := SpeechDiarizationRequest{
		RunID:           in.RunID,
		AssetID:         in.AssetID,
		AudioRef:        audioRef,
		SpeakerEvidence: in.SpeakerEvidence,
	}
	artifact, err := s.BuildTranscriptFromInputs(ctx, asr, align, in.RunID, in.AssetID, diarizeReq)
	if err != nil {
		return nil, err
	}
	// Persist the artifact as an immutable content-addressed blob (CAS),
	// indexed by SQLite with a deterministic provenance identity. Changing
	// provider/model/config/schema inputs yields a NEW artifact identity
	// (idempotent re-derivation), never a permanent write-once failure.
	if err := s.persistTranscript(ctx, in, asr, align, artifact, normalizedRef); err != nil {
		return nil, err
	}
	return artifact, nil
}

// persistTranscript stores the artifact JSON in CAS (content-addressed) and
// records the SQLite index row. It is a no-op when neither the DB nor the CAS
// store is configured, so Seam 1 unit-style tests can build artifacts in-memory.
func (s *SpeechService) persistTranscript(ctx context.Context, in domain.SpeechPipelineInput, asr *SpeechASRResult, align *SpeechAlignmentResult, artifact *domain.TranscriptArtifact, normalizedRef worker.ArtifactRef) error {
	if s.db == nil || s.cas == nil {
		return nil
	}

	// Deterministic cache identity over relevant inputs.
	prov := s.computeProvenance(ctx, in, asr, align, artifact, normalizedRef)
	provHash := prov.Hash()

	// Idempotent re-derivation: identical provenance reuses the prior artifact.
	if existing, err := s.db.GetTranscriptArtifactByProvenance(ctx, provHash); err == nil && existing != nil {
		loaded, err := s.loadArtifactFromCAS(ctx, existing.CASHash)
		if err == nil {
			loaded.ProvenanceHash = existing.ProvenanceHash
			*artifact = *loaded
			return nil
		}
	}

	// Marshal the artifact (excluding its own CAS/provenance hashes) into CAS.
	blob, err := json.Marshal(artifact)
	if err != nil {
		return fmt.Errorf("marshal transcript artifact: %w", err)
	}
	obj, err := s.cas.Put(bytes.NewReader(blob))
	if err != nil {
		return fmt.Errorf("commit transcript artifact to CAS: %w", err)
	}

	artifact.CASHash = obj.SHA256
	artifact.ProvenanceHash = provHash
	idx := storage.TranscriptArtifactIndex{
		ID:                  artifact.ID,
		AssetID:             artifact.AssetID,
		RunID:               artifact.RunID,
		CASHash:             obj.SHA256,
		ProvenanceHash:      provHash,
		ASRProviderID:       asr.ProviderID,
		ASRModelName:        asr.ModelName,
		ASRModelVersion:     asr.ModelVersion,
		AlignerProviderID:   align.ProviderID,
		AlignerModelName:    align.ModelName,
		AlignerModelVersion: align.ModelVersion,
		SegmentCfgJSON:      s.segmentCfgJSON(),
		CreatedAt:           artifact.CreatedAt,
	}
	if err := s.db.SaveTranscriptArtifactIndex(ctx, idx); err != nil {
		return fmt.Errorf("persist transcript artifact index: %w", err)
	}
	return nil
}

// loadArtifactFromCAS reads a TranscriptArtifact blob from CAS by hash.
func (s *SpeechService) loadArtifactFromCAS(ctx context.Context, casHash string) (*domain.TranscriptArtifact, error) {
	rc, err := s.cas.Get(casHash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var artifact domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&artifact); err != nil {
		return nil, err
	}
	artifact.CASHash = casHash
	return &artifact, nil
}

// computeProvenance derives the deterministic provenance identity for the
// transcript pipeline from the source asset, providers, models, segment rules,
// diarization identity, and the canonical schema version.
func (s *SpeechService) computeProvenance(ctx context.Context, in domain.SpeechPipelineInput, asr *SpeechASRResult, align *SpeechAlignmentResult, artifact *domain.TranscriptArtifact, normalizedRef worker.ArtifactRef) domain.TranscriptProvenance {
	assetSHA := ""
	if s.db != nil {
		if asset, err := s.db.GetSourceAsset(ctx, in.AssetID); err == nil {
			assetSHA = asset.SHA256
		}
	} else if in.AudioSHA256 != "" {
		assetSHA = in.AudioSHA256
	}
	cfg := s.SegmentConfig()
	evidCfg := domain.DefaultDiarizationEvidenceConfig()
	if s.EvidenceConfig != nil {
		evidCfg = s.EvidenceConfig()
	}
	var evidHash string
	if artifact.SpeakerEvidence != nil {
		evidHash = artifact.SpeakerEvidence.Hash()
	}
	return domain.TranscriptProvenance{
		AssetSHA256:             assetSHA,
		NormalizedAudioSHA256:   normalizedRef.SHA256,
		NormalizationConfig:     domain.DefaultAudioNormalizationConfig(),
		ASRProviderID:           asr.ProviderID,
		ASRModelName:            asr.ModelName,
		ASRModelVersion:         asr.ModelVersion,
		AlignerProviderID:       align.ProviderID,
		AlignerModelName:        align.ModelName,
		AlignerModelVersion:     align.ModelVersion,
		SegmentConfig:           cfg,
		DiarizationProviderID:   artifact.DiarizationProviderID,
		DiarizationModelName:    artifact.DiarizationModelName,
		DiarizationModelVersion: artifact.DiarizationModelVersion,
		DiarizationVADModel:     artifact.DiarizationVADModel,
		DiarizationVADVersion:   artifact.DiarizationVADVersion,
		DiarizationRan:          artifact.DiarizationRan,
		DiarizationEvidence:     evidCfg,
		SpeakerEvidenceHash:     evidHash,
		SchemaVersion:           domain.TranscriptSchemaVersion,
	}
}

// segmentCfgJSON serializes the current segment rule config for the index row.
func (s *SpeechService) segmentCfgJSON() string {
	b, err := json.Marshal(s.SegmentConfig())
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (s *SpeechService) invokeASR(ctx context.Context, req SpeechASRRequest) (*SpeechASRResult, error) {
	if s.ASRInvoke != nil {
		res, err := s.ASRInvoke(ctx, req)
		if err != nil {
			return nil, err
		}
		if res == nil || len(res.Segments) == 0 {
			return nil, domain.ErrASRNoCandidates
		}
		return res, nil
	}
	return nil, errors.New("no ASR provider invocation configured")
}

func (s *SpeechService) invokeAligner(ctx context.Context, req SpeechAlignmentRequest) (*SpeechAlignmentResult, error) {
	if s.AlignerInvoke != nil {
		res, err := s.AlignerInvoke(ctx, req)
		if err != nil {
			return nil, err
		}
		if res == nil || len(res.WordTimings) == 0 {
			return nil, domain.ErrAlignmentFailed
		}
		return res, nil
	}
	return nil, errors.New("no aligner provider invocation configured")
}

// defaultASRInvoke routes ASR through the Router with 1.7B quality attempt ->
// 0.6B fallback. The selected provider must implement provider.ASRTranscriptProvider.
// A provider that cannot produce transcripts, or that returns no segments, is a
// quality failure (ErrQualityRejected) so ExecuteWithRetry advances to the next
// candidate (the 0.6B fallback).
func (s *SpeechService) defaultASRInvoke(ctx context.Context, req SpeechASRRequest) (*SpeechASRResult, error) {
	if s.router == nil {
		return nil, errors.New("ASR router is not configured (call ConfigureRouter)")
	}
	routeReq := provider.RouteRequest{
		RunID:            req.RunID,
		Stage:            provider.TypeASR,
		Language:         req.Language,
		ExecutionProfile: domain.ExecutionProfileHybrid,
	}
	inputHash := speechInputHash("asr", req.AudioRef.SHA256, req.Language)
	var (
		result       []domain.ASRRawSegment
		selectedID   string
		selectedName string
		selectedVer  string
	)
	err := s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 1, func(p provider.Provider, attemptNumber int) error {
		prod, ok := p.(provider.ASRTranscriptProvider)
		if !ok {
			return fmt.Errorf("%w: provider %s cannot produce transcripts", domain.ErrQualityRejected, p.ID())
		}
		segs, err := prod.ProduceTranscript(ctx, req.AudioRef)
		if err != nil {
			return err
		}
		if len(segs) == 0 {
			return fmt.Errorf("%w: provider %s produced no segments", domain.ErrQualityRejected, p.ID())
		}
		result = segs
		selectedID = p.ID()
		selectedName, selectedVer = p.ModelInfo()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ASR routing failed: %w", err)
	}
	if len(result) == 0 {
		return nil, domain.ErrASRNoCandidates
	}
	return &SpeechASRResult{
		Segments:     result,
		ProviderID:   selectedID,
		ModelName:    selectedName,
		ModelVersion: selectedVer,
		LanguageCode: req.Language,
	}, nil
}

func (s *SpeechService) defaultAlignerInvoke(ctx context.Context, req SpeechAlignmentRequest) (*SpeechAlignmentResult, error) {
	if s.router == nil {
		return nil, errors.New("aligner router is not configured (call ConfigureRouter)")
	}
	routeReq := provider.RouteRequest{
		RunID:            req.RunID,
		Stage:            provider.TypeAligner,
		Language:         "zh",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	}
	inputHash := speechInputHash("aligner", req.AudioRef.SHA256, req.Text)
	var (
		result       []domain.WordTiming
		selectedID   string
		selectedName string
		selectedVer  string
	)
	err := s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 1, func(p provider.Provider, attemptNumber int) error {
		prod, ok := p.(provider.AlignWordProvider)
		if !ok {
			return fmt.Errorf("%w: provider %s cannot produce word alignments", domain.ErrQualityRejected, p.ID())
		}
		words, err := prod.ProduceAlignment(ctx, req.AudioRef, req.Text)
		if err != nil {
			return err
		}
		if len(words) == 0 {
			return fmt.Errorf("%w: provider %s produced no word timings", domain.ErrQualityRejected, p.ID())
		}
		result = words
		selectedID = p.ID()
		selectedName, selectedVer = p.ModelInfo()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("forced alignment routing failed: %w", err)
	}
	if len(result) == 0 {
		return nil, domain.ErrAlignmentFailed
	}
	return &SpeechAlignmentResult{
		WordTimings:  result,
		ProviderID:   selectedID,
		ModelName:    selectedName,
		ModelVersion: selectedVer,
	}, nil
}

func (s *SpeechService) defaultDiarize(ctx context.Context, words []domain.WordTiming, req SpeechDiarizationRequest) (*domain.DiarizationPlan, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if len(words) == 0 {
		return nil, nil
	}

	cfg := domain.DefaultDiarizationEvidenceConfig()
	if s.EvidenceConfig != nil {
		cfg = s.EvidenceConfig()
	}

	evidence := req.SpeakerEvidence
	// If no upstream speaker evidence was provided and we have router + audio path,
	// probe the source audio via the diarization provider (Issue #44 Blocker 1).
	if evidence == nil && s.router != nil && req.AudioRef.Path != "" {
		probed, err := s.probeSpeakerEvidence(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("speaker evidence probe failed: %w", err)
		}
		evidence = probed
	}

	if DiarizeEvidence(words, evidence, cfg) < cfg.MinDistinctTurns {
		plan := s.singleSpeakerFallbackPlan(words, req.RunID)
		plan.SpeakerEvidence = evidence
		return plan, nil
	}
	if s.router == nil || req.AudioRef.Path == "" {
		// Evidence exists but no production diarizer path is wired: keep the
		// stable single-speaker fallback rather than fabricating multi-speaker structure.
		plan := s.singleSpeakerFallbackPlan(words, req.RunID)
		plan.SpeakerEvidence = evidence
		return plan, nil
	}

	assignments, selectedID, selectedName, selectedVer, selectedVADName, selectedVADVer, err := s.defaultDiarizerInvoke(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("diarization routing failed: %w", err)
	}
	if len(assignments) < 2 || !hasMultiSpeakerEvidence(assignments) {
		// Provider returned single-speaker regions: collapse assignments to single-speaker
		// spanning the full word timeline, but PRESERVE the selected diarizer provider/model/VAD identity.
		plan := s.singleSpeakerFallbackPlan(words, req.RunID)
		plan.ProviderID = planProviderID(selectedID)
		plan.ModelName = selectedName
		plan.ModelVersion = selectedVer
		plan.VADModelName = selectedVADName
		plan.VADModelVersion = selectedVADVer
		plan.SpeakerEvidence = evidence
		if len(assignments) == 1 && assignments[0].Confidence > 0 {
			plan.Confidence = assignments[0].Confidence
		}
		return plan, nil
	}
	return &domain.DiarizationPlan{
		ID:              uuid.NewString(),
		RunID:           req.RunID,
		ProviderID:      planProviderID(selectedID),
		ModelName:       selectedName,
		ModelVersion:    selectedVer,
		VADModelName:    selectedVADName,
		VADModelVersion: selectedVADVer,
		Assignments:     assignments,
		SpeakerEvidence: evidence,
		Confidence:      assignmentsConfidence(assignments),
		CreatedAt:       time.Now().UTC(),
	}, nil
}

func (s *SpeechService) probeSpeakerEvidence(ctx context.Context, req SpeechDiarizationRequest) (*domain.SpeakerEvidence, error) {
	if s.router == nil {
		return nil, errors.New("diarization router is not configured")
	}
	routeReq := provider.RouteRequest{
		RunID:            req.RunID,
		Stage:            provider.TypeDiarizer,
		Language:         "zh",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	}
	evidCfg := domain.DefaultDiarizationEvidenceConfig()
	if s.EvidenceConfig != nil {
		evidCfg = s.EvidenceConfig()
	}
	inputHash := speechInputHash("diarize_evidence", req.AudioRef.SHA256, fmt.Sprintf("%.4f", evidCfg.EmbeddingCosineThreshold))
	ctx = provider.WithEmbeddingCosineThreshold(ctx, evidCfg.EmbeddingCosineThreshold)
	var result *domain.SpeakerEvidence
	err := s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 1, func(p provider.Provider, attemptNumber int) error {
		prod, ok := p.(provider.DiarizationProvider)
		if !ok {
			return fmt.Errorf("%w: provider %s cannot produce diarization evidence", domain.ErrQualityRejected, p.ID())
		}
		ev, err := prod.ProbeSpeakerEvidence(ctx, req.AudioRef)
		if err != nil {
			return err
		}
		if ev == nil {
			return fmt.Errorf("%w: provider %s produced nil speaker evidence", domain.ErrQualityRejected, p.ID())
		}
		result = ev
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *SpeechService) singleSpeakerFallbackPlan(words []domain.WordTiming, runID string) *domain.DiarizationPlan {
	start, end := words[0].StartMs, words[0].EndMs
	for _, w := range words[1:] {
		if w.StartMs < start {
			start = w.StartMs
		}
		if w.EndMs > end {
			end = w.EndMs
		}
	}
	assignments := []domain.SpeakerAssignment{{
		SpeakerID:  "SPEAKER_00",
		Label:      "SPEAKER_00",
		StartMs:    start,
		EndMs:      end,
		Confidence: 0.5,
	}}
	return &domain.DiarizationPlan{
		ID:           uuid.NewString(),
		RunID:        runID,
		ProviderID:   defaultDiarizerID,
		ModelName:    "",
		ModelVersion: "",
		Assignments:  assignments,
		Confidence:   0.5,
		CreatedAt:    time.Now().UTC(),
	}
}

func (s *SpeechService) defaultDiarizerInvoke(ctx context.Context, req SpeechDiarizationRequest) ([]domain.SpeakerAssignment, string, string, string, string, string, error) {
	routeReq := provider.RouteRequest{
		RunID:            req.RunID,
		Stage:            provider.TypeDiarizer,
		Language:         "zh",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	}
	inputHash := speechInputHash("diarize", req.AudioRef.SHA256)
	var (
		result          []domain.SpeakerAssignment
		selectedID      string
		selectedName    string
		selectedVer     string
		selectedVADName string
		selectedVADVer  string
	)
	err := s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 1, func(p provider.Provider, attemptNumber int) error {
		prod, ok := p.(provider.DiarizationProvider)
		if !ok {
			return fmt.Errorf("%w: provider %s cannot produce diarization", domain.ErrQualityRejected, p.ID())
		}
		regions, err := prod.ProduceDiarization(ctx, req.AudioRef)
		if err != nil {
			return err
		}
		if len(regions) == 0 {
			return fmt.Errorf("%w: provider %s produced no speaker regions", domain.ErrQualityRejected, p.ID())
		}
		result = regions
		selectedID = p.ID()
		selectedName, selectedVer = p.ModelInfo()
		if vip, ok := p.(interface{ VADModelInfo() (string, string) }); ok {
			selectedVADName, selectedVADVer = vip.VADModelInfo()
		}
		if selectedVADName == "" || selectedVADVer == "" {
			return fmt.Errorf("%w: provider %s missing verified VAD model identity", domain.ErrQualityRejected, p.ID())
		}
		return nil
	})
	if err != nil {
		return nil, "", "", "", "", "", fmt.Errorf("diarization routing failed: %w", err)
	}
	if len(result) == 0 {
		return nil, "", "", "", "", "", domain.ErrDiarizationNoCandidates
	}
	return result, selectedID, selectedName, selectedVer, selectedVADName, selectedVADVer, nil
}

func speechInputHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

func planProviderID(id string) string {
	if id == "" {
		return defaultDiarizerID
	}
	return id
}

func assignmentsConfidence(assignments []domain.SpeakerAssignment) float64 {
	var sum float64
	for _, a := range assignments {
		sum += a.Confidence
	}
	if len(assignments) == 0 {
		return 0.5
	}
	return sum / float64(len(assignments))
}

// applySpeakerAssignments stamps each word with its speaker from the diarization
// plan, validating temporal consistency (a word must fall within some assignment;
// overlapping assignments are resolved by best confidence, then nearest start).
func applySpeakerAssignments(words []domain.WordTiming, assignments []domain.SpeakerAssignment) []domain.WordTiming {
	if len(assignments) == 0 {
		return words
	}
	out := make([]domain.WordTiming, len(words))
	copy(out, words)
	for i, w := range out {
		speaker, ok := speakerForWord(w, assignments)
		if ok {
			out[i].SpeakerID = speaker
		}
	}
	return out
}

// speakerForWord resolves the best speaker assignment for a word timing.
func speakerForWord(w domain.WordTiming, assignments []domain.SpeakerAssignment) (string, bool) {
	best := ""
	bestConf := -1.0
	for _, a := range assignments {
		if w.StartMs >= a.StartMs && w.EndMs <= a.EndMs {
			if a.Confidence > bestConf || best == "" {
				best = a.SpeakerID
				bestConf = a.Confidence
			}
		}
	}
	if best != "" {
		return best, true
	}
	// Fall back to nearest assignment by midpoint.
	mid := (w.StartMs + w.EndMs) / 2
	best = ""
	bestDist := int64(1<<62 - 1)
	for _, a := range assignments {
		var dist int64
		switch {
		case mid < a.StartMs:
			dist = a.StartMs - mid
		case mid > a.EndMs:
			dist = mid - a.EndMs
		default:
			dist = 0
		}
		if dist < bestDist {
			bestDist = dist
			best = a.SpeakerID
		}
	}
	return best, best != ""
}

// hasMultiSpeakerEvidence returns true when the speaker assignments contain
// two or more distinct non-empty speaker IDs. The default single-speaker
// fallback (SPEAKER_00) yields false, while a real evidence-gated diarizer
// that detected multiple speakers yields true.
func hasMultiSpeakerEvidence(assignments []domain.SpeakerAssignment) bool {
	seen := make(map[string]bool)
	for _, a := range assignments {
		s := strings.TrimSpace(a.SpeakerID)
		if s != "" {
			seen[s] = true
		}
	}
	return len(seen) >= 2
}

// joinASRSegmentText concatenates the raw ASR segment texts into the accepted
// transcript text used as the aligner input.
func joinASRSegmentText(segs []domain.ASRRawSegment) string {
	var sb strings.Builder
	for i, seg := range segs {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strings.TrimSpace(seg.Text))
	}
	return strings.TrimSpace(sb.String())
}
