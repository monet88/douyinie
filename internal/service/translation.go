package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// TranslationInvokeFunc executes one translation provider attempt.
type TranslationInvokeFunc func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error)

// TranslationService orchestrates the meaning-first translation pipeline:
//  1. Validates input segments or loads SpeechBlocks from TranscriptArtifact.
//  2. Computes deterministic provenance hash and checks CAS/SQLite cache.
//  3. Selects provider via Router with policy-checked fallback.
//  4. Invokes provider to generate target translations (VI/EN).
//  5. Evaluates Translation QA Gate (facts, names, numbers, negation polarity).
//  6. Emits immutable TranslationVariant artifact to CAS and indexes in SQLite.
type TranslationService struct {
	db     *storage.DB
	cas    *cas.Store
	router *provider.Router
	qaGate *MeaningFirstQAGate

	// TranslateInvoke executes one translation provider attempt. When nil,
	// the router-backed default is used.
	TranslateInvoke TranslationInvokeFunc
}

// NewTranslationService creates a new TranslationService instance.
func NewTranslationService(db *storage.DB, casStore *cas.Store) *TranslationService {
	return &TranslationService{
		db:     db,
		cas:    casStore,
		qaGate: NewMeaningFirstQAGate(),
	}
}

// ConfigureRouter injects the provider router.
func (s *TranslationService) ConfigureRouter(router *provider.Router) {
	s.router = router
}

// Translate executes the meaning-first translation pipeline and returns the immutable TranslationVariant.
func (s *TranslationService) Translate(ctx context.Context, in domain.TranslationJobInput) (*domain.TranslationVariant, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, fmt.Errorf("asset_id is required")
	}

	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	sourceLang := strings.ToLower(strings.TrimSpace(in.SourceLanguage))
	if sourceLang == "" {
		sourceLang = "zh"
	}
	in.SourceLanguage = sourceLang

	// 1. Resolve segments: if none provided, load SpeechBlocks from TranscriptArtifact
	if len(in.Segments) == 0 {
		segments, err := s.loadSegmentsFromTranscript(ctx, in.AssetID)
		if err != nil {
			return nil, fmt.Errorf("failed to load source segments: %w", err)
		}
		in.Segments = segments
	}

	if len(in.Segments) == 0 {
		return nil, domain.ErrEmptyTranslationInput
	}

	// 2. Compute deterministic provenance hash
	provenanceHash := s.computeProvenanceHash(in)

	// 3. Check idempotent cache in SQLite / CAS
	if s.db != nil && s.cas != nil {
		if cachedIdx, err := s.db.GetTranslationVariantByProvenance(ctx, provenanceHash); err == nil && cachedIdx != nil {
			rc, err := s.cas.Get(cachedIdx.CASHash)
			if err == nil {
				defer rc.Close()
				data, err := io.ReadAll(rc)
				if err == nil {
					var cachedVariant domain.TranslationVariant
					if err := json.Unmarshal(data, &cachedVariant); err == nil {
						cachedVariant.CASHash = cachedIdx.CASHash
						cachedVariant.ProvenanceHash = cachedIdx.ProvenanceHash
						return &cachedVariant, nil
					}
				}
			}
		}
	}
	// 4. Provider routing & invocation with fallback
	result, selectedProv, err := s.invokeTranslationWithFallback(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("translation provider execution failed: %w", err)
	}

	// 5. Meaning-First QA Gate: Validate facts, names, numbers, negation
	var validatedSegments []domain.TranslationSegment
	var totalConfidence float64

	for _, seg := range result.Segments {
		qaRes := s.qaGate.ValidateSegment(seg.SourceText, seg.TargetText, in.SourceLanguage, in.TargetLanguage)
		if !qaRes.Passed {
			if qaRes.Err != nil {
				return nil, fmt.Errorf("translation QA gate rejected segment %d: %w", seg.Index, qaRes.Err)
			}
			return nil, fmt.Errorf("translation QA gate rejected segment %d: %w", seg.Index, domain.ErrMeaningPreservationFailed)
		}

		seg.PassedQAGate = true
		seg.QAConfidence = qaRes.Confidence
		seg.KeyFacts = qaRes.ExtractedFacts
		seg.NegationPolarity = qaRes.NegationPolarity
		totalConfidence += qaRes.Confidence

		validatedSegments = append(validatedSegments, seg)
	}

	overallQAScore := 1.0
	if len(validatedSegments) > 0 {
		overallQAScore = totalConfidence / float64(len(validatedSegments))
	}

	provID := selectedProv.ID()
	modelName, modelVersion := selectedProv.ModelInfo()
	if result.ProviderID != "" {
		provID = result.ProviderID
	}
	if result.ModelName != "" {
		modelName = result.ModelName
	}
	if result.ModelVersion != "" {
		modelVersion = result.ModelVersion
	}

	// 6. Build immutable TranslationVariant
	variant := &domain.TranslationVariant{
		ID:             uuid.NewString(),
		AssetID:        in.AssetID,
		RunID:          in.RunID,
		JobID:          in.JobID,
		SourceLanguage: in.SourceLanguage,
		TargetLanguage: in.TargetLanguage,
		Segments:       validatedSegments,
		ProviderID:     provID,
		ModelName:      modelName,
		ModelVersion:   modelVersion,
		ProvenanceHash: provenanceHash,
		OverallQAScore: overallQAScore,
		CreatedAt:      time.Now().UTC(),
	}

	// 7. Persist to CAS & SQLite index
	if s.cas != nil {
		payload, err := json.MarshalIndent(variant, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal translation variant: %w", err)
		}
		obj, err := s.cas.Put(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("store translation variant in CAS: %w", err)
		}
		variant.CASHash = obj.SHA256
	}

	if s.db != nil {
		idx := storage.TranslationVariantIndex{
			ID:             variant.ID,
			AssetID:        variant.AssetID,
			RunID:          variant.RunID,
			JobID:          variant.JobID,
			TargetLanguage: variant.TargetLanguage,
			CASHash:        variant.CASHash,
			ProvenanceHash: variant.ProvenanceHash,
			ProviderID:     variant.ProviderID,
			ModelName:      variant.ModelName,
			ModelVersion:   variant.ModelVersion,
			OverallQAScore: variant.OverallQAScore,
			CreatedAt:      variant.CreatedAt,
		}
		if err := s.db.SaveTranslationVariantIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("save translation variant index in DB: %w", err)
		}

		// Update stage execution if present
		now := time.Now().UTC()
		stageExec := domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          in.RunID,
			Stage:          "translation",
			Status:         "succeeded",
			ArtifactSHA256: variant.CASHash,
			StartedAt:      &variant.CreatedAt,
			CompletedAt:    &now,
			CreatedAt:      variant.CreatedAt,
			UpdatedAt:      now,
		}
		_ = s.db.CreateStageExecution(ctx, stageExec)
	}

	return variant, nil
}

// loadSegmentsFromTranscript loads SpeechBlocks from TranscriptArtifact in CAS/DB.
func (s *TranslationService) loadSegmentsFromTranscript(ctx context.Context, assetID string) ([]domain.TranslationInputSegment, error) {
	if s.db == nil || s.cas == nil {
		return nil, fmt.Errorf("database and CAS required to load transcript")
	}

	idx, err := s.db.GetTranscriptArtifactIndex(ctx, assetID)
	if err != nil {
		return nil, fmt.Errorf("transcript artifact not found for asset %s: %w", assetID, err)
	}

	rc, err := s.cas.Get(idx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("read transcript artifact from CAS: %w", err)
	}
	defer rc.Close()

	var transcript domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&transcript); err != nil {
		return nil, fmt.Errorf("decode transcript artifact: %w", err)
	}

	var segments []domain.TranslationInputSegment
	for _, block := range transcript.SpeechBlocks {
		text := strings.TrimSpace(block.SourceText)
		if text == "" {
			continue
		}
		segments = append(segments, domain.TranslationInputSegment{
			Index:      block.Index,
			SourceText: text,
			SpeakerID:  block.SpeakerID,
			StartMs:    block.StartMs,
			EndMs:      block.EndMs,
		})
	}

	return segments, nil
}

// computeProvenanceHash computes deterministic cache identity for translation.
func (s *TranslationService) computeProvenanceHash(in domain.TranslationJobInput) string {
	h := sha256.New()
	h.Write([]byte("stage:translation\n"))
	h.Write([]byte("asset_id:" + in.AssetID + "\n"))
	h.Write([]byte("src_lang:" + in.SourceLanguage + "\n"))
	h.Write([]byte("tgt_lang:" + in.TargetLanguage + "\n"))
	for _, seg := range in.Segments {
		h.Write([]byte(fmt.Sprintf("seg:%d:%s:%s:%d:%d\n", seg.Index, seg.SpeakerID, seg.SourceText, seg.StartMs, seg.EndMs)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// invokeTranslationWithFallback routes and executes translation attempts with policy-checked fallback.
func (s *TranslationService) invokeTranslationWithFallback(ctx context.Context, in domain.TranslationJobInput) (*provider.TranslationResult, provider.Provider, error) {
	if s.TranslateInvoke != nil {
		// Custom hook installed (e.g. for unit tests)
		var p provider.Provider = &provider.BaseFakeProvider{
			ProviderID:   "custom_invoke_provider",
			ProviderType: provider.TypeTranslation,
			Policy:       provider.PolicyAllowed,
			Healthy:      true,
			ModelName:    "custom-translator",
			ModelVersion: "1.0",
		}
		res, err := s.TranslateInvoke(ctx, p, in)
		return res, p, err
	}

	if s.router == nil {
		return nil, nil, fmt.Errorf("provider router is not configured")
	}

	routeReq := provider.RouteRequest{
		RunID:                 in.RunID,
		Stage:                 provider.TypeTranslation,
		Language:              in.TargetLanguage,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}

	routeRes, err := s.router.Route(ctx, routeReq)
	if err != nil {
		return nil, nil, fmt.Errorf("routing translation provider failed: %w", err)
	}

	candidates := append([]provider.Provider{routeRes.SelectedProvider}, routeRes.FallbackOrdered...)
	var lastErr error

	for attemptNum, cand := range candidates {
		attemptID := uuid.NewString()
		startTime := time.Now().UTC()
		res, err := s.invokeProvider(ctx, cand, in)
		endTime := time.Now().UTC()
		durationMs := endTime.Sub(startTime).Milliseconds()

		mName, mVer := cand.ModelInfo()
		attempt := domain.ProviderAttempt{
			ID:            attemptID,
			RunID:         in.RunID,
			Stage:         string(provider.TypeTranslation),
			ProviderID:    cand.ID(),
			ModelName:     mName,
			ModelVersion:  mVer,
			InputHash:     s.computeProvenanceHash(in),
			AttemptNumber: attemptNum + 1,
			LatencyMs:     durationMs,
			CreatedAt:     startTime,
		}

		if err == nil {
			attempt.Status = "succeeded"
			if s.db != nil {
				if errDB := s.db.RecordProviderAttempt(ctx, attempt); errDB != nil {
					return nil, nil, fmt.Errorf("record provider attempt: %w", errDB)
				}
			}
			return res, cand, nil
		}

		lastErr = err
		attempt.Status = "failed"
		attempt.ErrorMessage = err.Error()
		if s.db != nil {
			if errDB := s.db.RecordProviderAttempt(ctx, attempt); errDB != nil {
				return nil, nil, fmt.Errorf("record provider attempt: %w", errDB)
			}
		}
	}
	return nil, nil, fmt.Errorf("all candidate translation providers exhausted, last error: %w", lastErr)
}

// invokeProvider calls the provider's translation method.
func (s *TranslationService) invokeProvider(ctx context.Context, p provider.Provider, in domain.TranslationJobInput) (*provider.TranslationResult, error) {
	textProv, ok := p.(provider.TextTranslationProvider)
	if !ok {
		return nil, fmt.Errorf("provider %s does not implement TextTranslationProvider", p.ID())
	}

	req := provider.TranslationRequest{
		RunID:          in.RunID,
		SourceLanguage: in.SourceLanguage,
		TargetLanguage: in.TargetLanguage,
		Segments:       in.Segments,
	}

	return textProv.TranslateText(ctx, req)
}
