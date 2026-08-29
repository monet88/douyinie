package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
	"io"
	"strings"
	"time"
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
	db            *storage.DB
	cas           *cas.Store
	router        *provider.Router
	qaGate        *MeaningFirstQAGate
	spokenAdapter provider.SpokenScriptAdapter

	// TranslateInvoke executes one translation provider attempt. When nil,
	// the router-backed default is used.
	TranslateInvoke TranslationInvokeFunc
}

// NewTranslationService creates a new TranslationService instance.
func NewTranslationService(db *storage.DB, casStore *cas.Store) *TranslationService {
	return &TranslationService{
		db:            db,
		cas:           casStore,
		qaGate:        NewMeaningFirstQAGate(),
		spokenAdapter: provider.NewDefaultSpokenScriptAdapter(),
	}
}

// ConfigureRouter injects the provider router.
func (s *TranslationService) ConfigureRouter(router *provider.Router) {
	s.router = router
}

// ConfigureSpokenAdapter injects a custom spoken script adapter.
func (s *TranslationService) ConfigureSpokenAdapter(adapter provider.SpokenScriptAdapter) {
	s.spokenAdapter = adapter
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

	// 1. Resolve segments: if none provided, load SpeechBlocks from TranscriptArtifact.
	// Preserve the transcript CAS hash as a content input to deterministic cache identity.
	if len(in.Segments) == 0 {
		segments, transcriptCAS, err := s.loadSegmentsFromTranscript(ctx, in.AssetID)
		if err != nil {
			return nil, fmt.Errorf("failed to load source segments: %w", err)
		}
		in.Segments = segments
		if strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
			in.TranscriptArtifactCAS = transcriptCAS
		}
	}

	if len(in.Segments) == 0 {
		return nil, domain.ErrEmptyTranslationInput
	}

	// 2. Resolve provider policy/capability/health/profile ordering before cache lookup.
	// Provider/model/version are part of the canonical stage cache identity.
	var routeRes *provider.RouteResult
	if s.TranslateInvoke == nil {
		if s.router == nil {
			return nil, fmt.Errorf("provider router is not configured")
		}
		var err error
		routeRes, err = s.router.Route(ctx, translationRouteRequest(in))
		if err != nil {
			return nil, fmt.Errorf("routing translation provider failed: %w", err)
		}
		if routeRes.SelectedProvider == nil {
			return nil, fmt.Errorf("routing translation provider returned no selected provider")
		}

		provenanceHash, err := s.computeProvenanceHash(in, routeRes.SelectedProvider)
		if err != nil {
			return nil, fmt.Errorf("compute translation cache identity: %w", err)
		}

		// 3. Check idempotent cache in SQLite / CAS for the currently selected
		// provider/model/version identity. A changed provider or model version must
		// not reuse an artifact produced by the previous model.
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
	}

	// 4. Provider invocation uses Router-owned retry/fallback semantics so
	// ProviderAttempt and alternate SelectionDecision provenance stay complete.
	result, selectedProv, err := s.invokeTranslationWithFallback(ctx, in, routeRes)
	if err != nil {
		return nil, fmt.Errorf("translation provider execution failed: %w", err)
	}
	provenanceHash, err := s.computeProvenanceHash(in, selectedProv)
	if err != nil {
		return nil, fmt.Errorf("compute selected translation cache identity: %w", err)
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
		SchemaVersion:  domain.TranslationSchemaVersion,
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
func (s *TranslationService) loadSegmentsFromTranscript(ctx context.Context, assetID string) ([]domain.TranslationInputSegment, string, error) {
	if s.db == nil || s.cas == nil {
		return nil, "", fmt.Errorf("database and CAS required to load transcript")
	}

	idx, err := s.db.GetTranscriptArtifactIndex(ctx, assetID)
	if err != nil {
		return nil, "", fmt.Errorf("transcript artifact not found for asset %s: %w", assetID, err)
	}

	rc, err := s.cas.Get(idx.CASHash)
	if err != nil {
		return nil, "", fmt.Errorf("read transcript artifact from CAS: %w", err)
	}
	defer rc.Close()

	var transcript domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&transcript); err != nil {
		return nil, "", fmt.Errorf("decode transcript artifact: %w", err)
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

	return segments, idx.CASHash, nil
}

// computeProvenanceHash computes deterministic cache identity for translation.
func (s *TranslationService) computeProvenanceHash(in domain.TranslationJobInput, p provider.Provider) (string, error) {
	if p == nil {
		return "", fmt.Errorf("translation provider is required for cache identity")
	}
	inputHash, err := s.computeTranslationInputHash(in)
	if err != nil {
		return "", err
	}
	inputHashes := []string{inputHash}
	if transcriptCAS := strings.TrimSpace(in.TranscriptArtifactCAS); transcriptCAS != "" {
		inputHashes = append(inputHashes, transcriptCAS)
	}
	modelName, modelVersion := p.ModelInfo()
	return cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       string(provider.TypeTranslation),
		InputHashes: inputHashes,
		SemanticConfig: map[string]any{
			"source_language":  in.SourceLanguage,
			"meaning_contract": "facts_names_numbers_negation_v1",
		},
		ProviderID:    p.ID(),
		ModelName:     modelName,
		ModelVersion:  modelVersion,
		Language:      in.TargetLanguage,
		SchemaVersion: domain.TranslationSchemaVersion,
	})
}

// computeTranslationInputHash hashes only content/semantic translation inputs.
// RunID, JobID, paths, mtimes, execution profile and credential references are
// deliberately excluded from the reusable stage identity.
func (s *TranslationService) computeTranslationInputHash(in domain.TranslationJobInput) (string, error) {
	payload := struct {
		SourceLanguage string                           `json:"source_language"`
		Segments       []domain.TranslationInputSegment `json:"segments"`
	}{
		SourceLanguage: strings.ToLower(strings.TrimSpace(in.SourceLanguage)),
		Segments:       in.Segments,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal translation input identity: %w", err)
	}
	h := sha256.New()
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// invokeTranslationWithFallback routes and executes translation attempts with policy-checked fallback.
func (s *TranslationService) invokeTranslationWithFallback(ctx context.Context, in domain.TranslationJobInput, routeRes *provider.RouteResult) (*provider.TranslationResult, provider.Provider, error) {
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

	routeReq := translationRouteRequest(in)
	if routeRes == nil {
		var err error
		routeRes, err = s.router.Route(ctx, routeReq)
		if err != nil {
			return nil, nil, fmt.Errorf("routing translation provider failed: %w", err)
		}
	}

	inputHash, err := s.computeTranslationInputHash(in)
	if err != nil {
		return nil, nil, fmt.Errorf("compute translation input hash: %w", err)
	}

	var result *provider.TranslationResult
	var selected provider.Provider
	err = s.router.ExecuteRoutedWithRetry(ctx, routeReq, routeRes, inputHash, 1, func(cand provider.Provider, _ int) error {
		res, invokeErr := s.invokeProvider(ctx, cand, in)
		if invokeErr != nil {
			return invokeErr
		}
		if res == nil {
			return fmt.Errorf("translation provider %s returned nil result", cand.ID())
		}
		result = res
		selected = cand
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if result == nil || selected == nil {
		return nil, nil, fmt.Errorf("translation execution completed without a selected provider result")
	}
	return result, selected, nil
}

func translationRouteRequest(in domain.TranslationJobInput) provider.RouteRequest {
	return provider.RouteRequest{
		RunID:                 in.RunID,
		Stage:                 provider.TypeTranslation,
		Language:              in.TargetLanguage,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
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

// AdaptDubScript adapts a TranslationVariant into a duration-adapted DubScriptVariant.
// It enforces shorten-first adaptation for brisk source cadence while preserving facts,
// names, numbers, and negation polarity under QA gate validation.
func (s *TranslationService) AdaptDubScript(ctx context.Context, in domain.DubScriptJobInput) (*domain.DubScriptVariant, error) {
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

	// 1. Load meaning-first TranslationVariant
	transVariant, transCAS, err := s.loadTranslationVariant(ctx, in.AssetID, in.TargetLanguage, in.TranslationVariantCAS)
	if err != nil {
		return nil, fmt.Errorf("load translation variant for dub script adaptation: %w", err)
	}
	in.TranslationVariantCAS = transCAS

	// 2. Compute deterministic provenance cache identity
	provenanceHash, err := s.computeDubScriptProvenanceHash(in, transVariant)
	if err != nil {
		return nil, fmt.Errorf("compute dub script cache identity: %w", err)
	}

	// 3. Check idempotent cache in SQLite / CAS
	if s.db != nil && s.cas != nil {
		if cachedIdx, err := s.db.GetDubScriptVariantByProvenance(ctx, provenanceHash); err == nil && cachedIdx != nil {
			rc, err := s.cas.Get(cachedIdx.CASHash)
			if err == nil {
				defer rc.Close()
				data, err := io.ReadAll(rc)
				if err == nil {
					var cachedVariant domain.DubScriptVariant
					if err := json.Unmarshal(data, &cachedVariant); err == nil {
						cachedVariant.CASHash = cachedIdx.CASHash
						cachedVariant.ProvenanceHash = cachedIdx.ProvenanceHash
						return &cachedVariant, nil
					}
				}
			}
		}
	}

	// 4. Adapt segments with shorten-first principle and source-relative cadence
	var dubSegments []domain.DubScriptSegment
	var totalConfidence float64
	variantRequiresReview := false

	adapter := s.spokenAdapter
	if adapter == nil {
		adapter = provider.NewDefaultSpokenScriptAdapter()
	}

	for i, seg := range transVariant.Segments {
		slotDurationMs := seg.EndMs - seg.StartMs
		if slotDurationMs <= 0 {
			slotDurationMs = 1000
		}

		srcCPS := provider.EstimateSourceSpeakingRate(seg.SourceText, in.SourceLanguage, slotDurationMs)
		hasNextTurn := i+1 < len(transVariant.Segments)
		var sourceGapAfterMs int64
		if hasNextTurn {
			sourceGapAfterMs = transVariant.Segments[i+1].StartMs - seg.EndMs
			if sourceGapAfterMs < 0 {
				sourceGapAfterMs = 0
			}
		}

		meaningText := seg.TargetText
		adaptRes, err := adapter.AdaptSpokenScript(ctx, provider.SpokenScriptAdaptationRequest{
			SourceText:            seg.SourceText,
			SourceLanguage:        in.SourceLanguage,
			MeaningText:           meaningText,
			TargetLanguage:        in.TargetLanguage,
			SlotDurationMs:        slotDurationMs,
			SourceSpeakingRateCPS: srcCPS,
			SourceGapAfterMs:      sourceGapAfterMs,
			HasNextTurn:           hasNextTurn,
		})
		if err != nil {
			return nil, fmt.Errorf("adapt spoken script for segment %d: %w", seg.Index, err)
		}

		spokenText := adaptRes.SpokenText
		isShortened := adaptRes.IsShortened
		estDurationMs := adaptRes.EstimatedDurationMs
		targetCPS := adaptRes.TargetSpeakingRateCPS
		cadenceRatio := adaptRes.CadenceRatio
		naturalGapMs := adaptRes.NaturalGapMs
		usableSlotMs := adaptRes.UsableSlotMs
		targetWordBudget := adaptRes.TargetWordBudget
		requiresReview := adaptRes.RequiresReview
		reviewReason := adaptRes.ReviewReason

		// Run Translation QA Gate to guarantee facts/names/numbers/negation survive
		qaRes := s.qaGate.ValidateSegment(seg.SourceText, spokenText, in.SourceLanguage, in.TargetLanguage)
		if !qaRes.Passed {
			if isShortened {
				// Fallback to unshortened meaningText if shortened version corrupted facts
				spokenText = meaningText
				isShortened = false
				estDurationMs = provider.EstimateSpokenDurationMs(spokenText, in.TargetLanguage)
				targetCPS = provider.EstimateSpokenRateCPS(spokenText, in.TargetLanguage)
				if srcCPS > 0 {
					cadenceRatio = targetCPS / srcCPS
				}
				naturalGapMs = provider.EstimatePauseToNextTurnMs(slotDurationMs, estDurationMs, sourceGapAfterMs, hasNextTurn)
				requiresReview = true
				reviewReason = "QA_GATE_FALLBACK_UNSHORTENED"

				qaRes = s.qaGate.ValidateSegment(seg.SourceText, spokenText, in.SourceLanguage, in.TargetLanguage)
			}
			if !qaRes.Passed {
				if qaRes.Err != nil {
					return nil, fmt.Errorf("dub script QA gate rejected segment %d: %w", seg.Index, qaRes.Err)
				}
				return nil, fmt.Errorf("dub script QA gate rejected segment %d: %w", seg.Index, domain.ErrMeaningPreservationFailed)
			}
		}

		// Double-check the immutable source speech window. Inter-turn source
		// silence lives outside this slot and is preserved by the fixed anchors.
		if estDurationMs > usableSlotMs {
			requiresReview = true
			if reviewReason == "" {
				reviewReason = "DURATION_OVERRUN"
			}
		}

		if requiresReview {
			variantRequiresReview = true
		}

		dubSeg := domain.DubScriptSegment{
			Index:                 seg.Index,
			SourceText:            seg.SourceText,
			MeaningText:           meaningText,
			SpokenText:            spokenText,
			SpeakerID:             seg.SpeakerID,
			StartMs:               seg.StartMs,
			EndMs:                 seg.EndMs,
			SlotDurationMs:        slotDurationMs,
			EstimatedDurationMs:   estDurationMs,
			SourceSpeakingRateCPS: srcCPS,
			TargetSpeakingRateCPS: targetCPS,
			CadenceRatio:          cadenceRatio,
			SourceGapAfterMs:      sourceGapAfterMs,
			NaturalGapMs:          naturalGapMs,
			TargetWordBudget:      targetWordBudget,
			IsShortened:           isShortened,
			RequiresReview:        requiresReview,
			ReviewReason:          reviewReason,
			KeyFacts:              qaRes.ExtractedFacts,
			NegationPolarity:      qaRes.NegationPolarity,
			QAConfidence:          qaRes.Confidence,
			PassedQAGate:          true,
		}
		totalConfidence += qaRes.Confidence
		dubSegments = append(dubSegments, dubSeg)
	}

	overallQAScore := 1.0
	if len(dubSegments) > 0 {
		overallQAScore = totalConfidence / float64(len(dubSegments))
	}

	// 5. Build immutable DubScriptVariant
	variant := &domain.DubScriptVariant{
		ID:                    uuid.NewString(),
		SchemaVersion:         domain.DubScriptSchemaVersion,
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		SourceLanguage:        in.SourceLanguage,
		TargetLanguage:        in.TargetLanguage,
		TranslationVariantCAS: in.TranslationVariantCAS,
		Segments:              dubSegments,
		ProviderID:            transVariant.ProviderID,
		ModelName:             transVariant.ModelName,
		ModelVersion:          transVariant.ModelVersion,
		ProvenanceHash:        provenanceHash,
		OverallQAScore:        overallQAScore,
		RequiresReview:        variantRequiresReview,
		CreatedAt:             time.Now().UTC(),
	}

	// 6. Persist to CAS & SQLite index
	if s.cas != nil {
		payload, err := json.MarshalIndent(variant, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal dub script variant: %w", err)
		}
		obj, err := s.cas.Put(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("store dub script variant in CAS: %w", err)
		}
		variant.CASHash = obj.SHA256
	}

	if s.db != nil {
		idx := storage.DubScriptVariantIndex{
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
		if err := s.db.SaveDubScriptVariantIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("save dub script variant index in DB: %w", err)
		}

		now := time.Now().UTC()
		stageExec := domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          in.RunID,
			Stage:          "dub_script",
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

func (s *TranslationService) loadTranslationVariant(ctx context.Context, assetID, targetLang, casHash string) (*domain.TranslationVariant, string, error) {
	if s.cas == nil {
		return nil, "", fmt.Errorf("CAS store required to load translation variant")
	}

	resolvedCAS := strings.TrimSpace(casHash)
	if resolvedCAS == "" {
		if s.db == nil {
			return nil, "", fmt.Errorf("database required to find translation variant index")
		}
		idx, err := s.db.GetTranslationVariantIndex(ctx, assetID, targetLang)
		if err != nil {
			return nil, "", fmt.Errorf("translation variant not found for asset %s: %w", assetID, err)
		}
		resolvedCAS = idx.CASHash
	}

	rc, err := s.cas.Get(resolvedCAS)
	if err != nil {
		return nil, "", fmt.Errorf("read translation variant from CAS (%s): %w", resolvedCAS, err)
	}
	defer rc.Close()

	var variant domain.TranslationVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		return nil, "", fmt.Errorf("decode translation variant: %w", err)
	}

	// Validate ownership against requested AssetID and TargetLanguage
	if variant.AssetID != assetID {
		return nil, "", fmt.Errorf("translation variant asset_id mismatch: expected %s, got %s", assetID, variant.AssetID)
	}
	if !strings.EqualFold(variant.TargetLanguage, targetLang) {
		return nil, "", fmt.Errorf("translation variant target_language mismatch: expected %s, got %s", targetLang, variant.TargetLanguage)
	}

	variant.CASHash = resolvedCAS
	return &variant, resolvedCAS, nil
}

func (s *TranslationService) computeDubScriptProvenanceHash(in domain.DubScriptJobInput, transVariant *domain.TranslationVariant) (string, error) {
	inputHashes := []string{transVariant.ProvenanceHash}
	if in.TranslationVariantCAS != "" {
		inputHashes = append(inputHashes, in.TranslationVariantCAS)
	}
	return cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       "dub_script",
		InputHashes: inputHashes,
		SemanticConfig: map[string]any{
			"source_language": in.SourceLanguage,
			"adaptation_mode": "shorten_first_source_relative_cadence_v2",
		},
		ProviderID:    transVariant.ProviderID,
		ModelName:     transVariant.ModelName,
		ModelVersion:  transVariant.ModelVersion,
		Language:      in.TargetLanguage,
		SchemaVersion: domain.DubScriptSchemaVersion,
	})
}
