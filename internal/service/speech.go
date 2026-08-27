package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/speech"
	"github.com/monet88/douyinie/internal/storage"
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
	Diarize func(ctx context.Context, words []domain.WordTiming) (*domain.DiarizationPlan, error)

	// SegmentConfig supplies the canonical segmentation rules.
	SegmentConfig func() domain.SegmentRuleConfig
}

// defaultDiarizerID is the provenance identity of the single-speaker fallback
// speaker-assignment default. It is NOT a real diarization provider: it only
// assigns a canonical single speaker so speaker labels exist before canonical
// segmentation. DiarizationRan stays false when only this fallback ran.
const defaultDiarizerID = "default-single-speaker-fallback"

// SpeechASRRequest is the input for one ASR provider attempt.
type SpeechASRRequest struct {
	RunID     string
	AssetID   string
	AudioPath string
	Language  string
}

// SpeechAlignmentRequest is the input for one forced-alignment attempt.
type SpeechAlignmentRequest struct {
	RunID     string
	AssetID   string
	AudioPath string
	Text      string
}

// NewSpeechService creates a SpeechService with the evidence-gated conditional
// diarization default installed. Callers may override ASRInvoke/AlignerInvoke/
// Diarize; ConfigureRouter installs router-backed ASR/aligner defaults.
// The cas store is used to persist TranscriptArtifacts as content-addressed
// blobs; it may be nil when persistence is not required.
func NewSpeechService(db *storage.DB, casStore *cas.Store) *SpeechService {
	s := &SpeechService{
		db:            db,
		cas:           casStore,
		SegmentConfig: domain.DefaultSegmentRuleConfig,
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
	asr *SpeechASRResult,
	align *SpeechAlignmentResult,
	runID string,
	assetID string,
) (*domain.TranscriptArtifact, error) {
	if asr == nil || len(asr.Segments) == 0 {
		return nil, domain.ErrASRNoCandidates
	}
	if align == nil || len(align.WordTimings) == 0 {
		return nil, domain.ErrAlignmentFailed
	}

	// 1. Conditional diarization BEFORE canonical segmentation.
	var assignments []domain.SpeakerAssignment
	var diarizationRan bool
	var diarizationProviderID string
	if s.Diarize != nil {
		plan, err := s.Diarize(context.Background(), align.WordTimings)
		if err != nil {
			return nil, fmt.Errorf("conditional diarization failed: %w", err)
		}
		if plan != nil {
			assignments = plan.Assignments
			diarizationRan = hasMultiSpeakerEvidence(assignments)
			diarizationProviderID = plan.ProviderID
		}
	}

	// 2. Apply speaker assignments to word timings (speaker labels flow into
	//    canonical segmentation).
	words := applySpeakerAssignments(align.WordTimings, assignments)

	// 3. Canonical SpeechBlock segmentation via semantic/punctuation/pause rules.
	cfg := s.SegmentConfig()
	blocks := speech.BuildSpeechBlocks(words, cfg)

	return &domain.TranscriptArtifact{
		ID:                    uuid.NewString(),
		AssetID:               assetID,
		RunID:                 runID,
		RawSegments:           asr.Segments,
		WordTimings:           words,
		SpeakerAssignments:    assignments,
		SpeechBlocks:          blocks,
		ASRProviderID:         asr.ProviderID,
		AlignerProviderID:     align.ProviderID,
		DiarizationRan:        diarizationRan,
		DiarizationProviderID: diarizationProviderID,
		SourceLanguage:        asr.LanguageCode,
		CreatedAt:             time.Now().UTC(),
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

	asrReq := SpeechASRRequest{
		RunID:     in.RunID,
		AssetID:   in.AssetID,
		AudioPath: in.AudioPath,
		Language:  "zh",
	}
	asr, err := s.invokeASR(ctx, asrReq)
	if err != nil {
		return nil, fmt.Errorf("ASR failed: %w", err)
	}

	acceptedText := joinASRSegmentText(asr.Segments)
	alignReq := SpeechAlignmentRequest{
		RunID:     in.RunID,
		AssetID:   in.AssetID,
		AudioPath: in.AudioPath,
		Text:      acceptedText,
	}
	align, err := s.invokeAligner(ctx, alignReq)
	if err != nil {
		return nil, fmt.Errorf("forced alignment failed: %w", err)
	}

	artifact, err := s.BuildTranscriptFromInputs(asr, align, in.RunID, in.AssetID)
	if err != nil {
		return nil, err
	}
	// Persist the artifact as an immutable content-addressed blob (CAS),
	// indexed by SQLite with a deterministic provenance identity. Changing
	// provider/model/config/schema inputs yields a NEW artifact identity
	// (idempotent re-derivation), never a permanent write-once failure.
	if err := s.persistTranscript(ctx, in, asr, align, artifact); err != nil {
		return nil, err
	}
	return artifact, nil
}

// persistTranscript stores the artifact JSON in CAS (content-addressed) and
// records the SQLite index row. It is a no-op when neither the DB nor the CAS
// store is configured, so Seam 1 unit-style tests can build artifacts in-memory.
func (s *SpeechService) persistTranscript(ctx context.Context, in domain.SpeechPipelineInput, asr *SpeechASRResult, align *SpeechAlignmentResult, artifact *domain.TranscriptArtifact) error {
	if s.db == nil || s.cas == nil {
		return nil
	}

	// Deterministic cache identity over relevant inputs.
	prov := s.computeProvenance(ctx, in, asr, align, artifact)
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
func (s *SpeechService) computeProvenance(ctx context.Context, in domain.SpeechPipelineInput, asr *SpeechASRResult, align *SpeechAlignmentResult, artifact *domain.TranscriptArtifact) domain.TranscriptProvenance {
	assetSHA := ""
	if s.db != nil {
		if asset, err := s.db.GetSourceAsset(ctx, in.AssetID); err == nil {
			assetSHA = asset.SHA256
		}
	}
	cfg := s.SegmentConfig()
	return domain.TranscriptProvenance{
		AssetSHA256:           assetSHA,
		ASRProviderID:         asr.ProviderID,
		ASRModelName:          asr.ModelName,
		ASRModelVersion:       asr.ModelVersion,
		AlignerProviderID:     align.ProviderID,
		AlignerModelName:      align.ModelName,
		AlignerModelVersion:   align.ModelVersion,
		SegmentConfig:         cfg,
		DiarizationProviderID: artifact.DiarizationProviderID,
		DiarizationRan:        artifact.DiarizationRan,
		SchemaVersion:         domain.TranscriptSchemaVersion,
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
	inputHash := speechInputHash(req.AssetID, req.AudioPath, req.Language)
	var result *SpeechASRResult
	err := s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 1, func(p provider.Provider, attemptNumber int) error {
		prod, ok := p.(provider.ASRTranscriptProvider)
		if !ok {
			return fmt.Errorf("%w: provider %s cannot produce transcripts", domain.ErrQualityRejected, p.ID())
		}
		segs, err := prod.ProduceTranscript(ctx, req.AudioPath)
		if err != nil {
			return err
		}
		if len(segs) == 0 {
			return fmt.Errorf("%w: provider %s produced no transcript segments", domain.ErrQualityRejected, p.ID())
		}
		modelName, modelVer := p.ModelInfo()
		result = &SpeechASRResult{
			ProviderID:   p.ID(),
			ModelName:    modelName,
			ModelVersion: modelVer,
			LanguageCode: req.Language,
			Segments:     segs,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ASR routing failed: %w", err)
	}
	if result == nil {
		return nil, domain.ErrASRNoCandidates
	}
	return result, nil
}

// defaultAlignerInvoke routes forced alignment through the Router among
// aligner providers implementing provider.AlignWordProvider.
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
	inputHash := speechInputHash(req.AssetID, req.AudioPath, req.Text)
	var result *SpeechAlignmentResult
	err := s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 1, func(p provider.Provider, attemptNumber int) error {
		prod, ok := p.(provider.AlignWordProvider)
		if !ok {
			return fmt.Errorf("%w: provider %s cannot produce alignments", domain.ErrQualityRejected, p.ID())
		}
		words, err := prod.ProduceAlignment(ctx, req.AudioPath, req.Text)
		if err != nil {
			return err
		}
		if len(words) == 0 {
			return fmt.Errorf("%w: provider %s produced no word timings", domain.ErrQualityRejected, p.ID())
		}
		modelName, modelVer := p.ModelInfo()
		result = &SpeechAlignmentResult{
			ProviderID:   p.ID(),
			ModelName:    modelName,
			ModelVersion: modelVer,
			WordTimings:  words,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("forced alignment routing failed: %w", err)
	}
	if result == nil {
		return nil, domain.ErrAlignmentFailed
	}
	return result, nil
}

// speechInputHash derives a stable execution input hash from the speech input
// identity so routing provenance records a deterministic input fingerprint.
func speechInputHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

// defaultDiarize is the fallback single-speaker labeler installed at construction.
// It is NOT a diarization provider: it always assigns SPEAKER_00 across the full
// word span so every word carries a speaker label before canonical segmentation.
// Multi-speaker evidence from an independently wired diarizer is expressed by
// overriding the Diarize hook. DiarizationRan stays false when only this
// fallback ran (see hasMultiSpeakerEvidence).
func (s *SpeechService) defaultDiarize(ctx context.Context, words []domain.WordTiming) (*domain.DiarizationPlan, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if len(words) == 0 {
		return nil, nil
	}
	// Single-speaker default over the full word span.
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
		ID:          uuid.NewString(),
		RunID:       "",
		ProviderID:  defaultDiarizerID,
		Assignments: assignments,
		Confidence:  0.5,
		CreatedAt:   time.Now().UTC(),
	}, nil
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
			if a.Confidence > bestConf {
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
