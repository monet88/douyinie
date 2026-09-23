package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const (
	translationMeaningContractVersion = "facts_names_numbers_negation_v4"
	TranslationContractID             = translationMeaningContractVersion + "+request_glossary_v1"
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

// resolveRunTranscriptCAS resolves the pinned transcript artifact for a run from its stage execution
// or run index, refusing any index entry that belongs to a different asset.
func resolveRunTranscriptCAS(ctx context.Context, db *storage.DB, runID, assetID, purpose string) (string, error) {
	if db == nil || strings.TrimSpace(runID) == "" {
		return "", nil
	}
	if assetID != "" {
		if run, err := db.GetRun(ctx, runID); err == nil && run != nil {
			if run.JobID != "" {
				if job, err := db.GetJob(ctx, run.JobID); err == nil && job != nil {
					if job.SourceAssetID != "" && job.SourceAssetID != assetID {
						return "", fmt.Errorf("run %s transcript lineage mismatch for %s: %s != %s", runID, purpose, job.SourceAssetID, assetID)
					}
				} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
					return "", fmt.Errorf("resolve run job for %s: %w", purpose, err)
				}
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("resolve run %s for %s: %w", runID, purpose, err)
		}
	}
	if casHash, err := db.GetStageArtifactHash(ctx, runID, "speech_understand"); err == nil && casHash != "" {
		return casHash, nil
	} else if err != nil {
		return "", fmt.Errorf("resolve run speech artifact for %s: %w", purpose, err)
	} else if idx, err := db.GetTranscriptArtifactIndexByRun(ctx, runID); err == nil && idx != nil {
		if assetID != "" && idx.AssetID != assetID {
			return "", fmt.Errorf("run %s transcript lineage mismatch for %s: %s != %s", runID, purpose, idx.AssetID, assetID)
		}
		return idx.CASHash, nil
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", fmt.Errorf("resolve run transcript lineage for %s: %w", purpose, err)
	}
	return "", nil
}

// Translate executes the meaning-first translation pipeline and returns the immutable TranslationVariant.
func (s *TranslationService) Translate(ctx context.Context, in domain.TranslationJobInput) (*domain.TranslationVariant, error) {
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

	if s.db != nil && strings.TrimSpace(in.RunID) != "" {
		if run, err := s.db.GetRun(ctx, in.RunID); err == nil && run != nil {
			if strings.TrimSpace(in.JobID) != "" && run.JobID != "" && in.JobID != run.JobID {
				return nil, fmt.Errorf("%w: job_id mismatch for run %s: %s != %s", domain.ErrTranslationOwnershipMismatch, in.RunID, in.JobID, run.JobID)
			}
			jobID := run.JobID
			if jobID == "" {
				jobID = in.JobID
			}
			if jobID != "" {
				job, err := s.db.GetJob(ctx, jobID)
				if err == nil && job != nil {
					if in.AssetID != "" && job.SourceAssetID != "" && job.SourceAssetID != in.AssetID {
						return nil, fmt.Errorf("%w: run %s belongs to asset %s, not %s", domain.ErrTranslationOwnershipMismatch, in.RunID, job.SourceAssetID, in.AssetID)
					}
				} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
					return nil, fmt.Errorf("lookup job %s for run %s: %w", jobID, in.RunID, err)
				}
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("lookup run %s: %w", in.RunID, err)
		}
	}
	if s.db != nil && strings.TrimSpace(in.JobID) != "" {
		job, err := s.db.GetJob(ctx, in.JobID)
		if err == nil && job != nil {
			if in.AssetID != "" && job.SourceAssetID != "" && job.SourceAssetID != in.AssetID {
				return nil, fmt.Errorf("%w: job %s belongs to asset %s, not %s", domain.ErrTranslationOwnershipMismatch, in.JobID, job.SourceAssetID, in.AssetID)
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("lookup job %s: %w", in.JobID, err)
		}
	}

	// resolveRunTranscriptCAS resolves the pinned transcript artifact for a run from its stage execution
	// or run index, refusing any index entry that belongs to a different asset.
	// A run-scoped direct translation request may already provide canonical segments,
	// but the resulting artifact must still pin the source transcript it came from.
	// Prefer run evidence only; never infer this lineage from an unrelated asset-latest run.
	if !in.Ephemeral && s.db != nil && strings.TrimSpace(in.RunID) != "" {
		transcriptCAS, err := resolveRunTranscriptCAS(ctx, s.db, in.RunID, in.AssetID, "translation")
		if err != nil {
			return nil, err
		}
		if transcriptCAS == "" {
			return nil, fmt.Errorf("missing pinned speech_understand transcript lineage for run %s", in.RunID)
		}
		if strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
			in.TranscriptArtifactCAS = transcriptCAS
		} else if transcriptCAS != in.TranscriptArtifactCAS {
			return nil, fmt.Errorf("run %s transcript lineage mismatch for translation: input=%s run=%s", in.RunID, in.TranscriptArtifactCAS, transcriptCAS)
		}
	}

	// 1. Resolve segments: if none provided, load SpeechBlocks from TranscriptArtifact.
	// Preserve the transcript CAS hash as a content input to deterministic cache identity.
	if len(in.Segments) == 0 {
		if strings.TrimSpace(in.RunID) != "" && strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
			return nil, fmt.Errorf("missing pinned speech_understand transcript lineage for run %s", in.RunID)
		}
		segments, transcriptCAS, err := s.loadSegmentsFromTranscript(ctx, in.AssetID, in.TranscriptArtifactCAS)
		if err != nil {
			return nil, fmt.Errorf("failed to load source segments: %w", err)
		}
		in.Segments = segments
		if strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
			in.TranscriptArtifactCAS = transcriptCAS
		}
	}
	if len(in.Segments) > 0 {
		filtered := make([]domain.TranslationInputSegment, 0, len(in.Segments))
		for _, seg := range in.Segments {
			text := strings.TrimSpace(seg.SourceText)
			if text != "" && !domain.IsPathologicalRepetitionNoise(text) {
				filtered = append(filtered, seg)
			}
		}
		in.Segments = filtered
	}

	if len(in.Segments) == 0 {
		return nil, domain.ErrEmptyTranslationInput
	}
	// Explicit compatibility proof: when direct segments are provided without an explicit transcript CAS,
	// allow fallback to the asset's latest transcript index only when candidate segments exactly match.
	if strings.TrimSpace(in.TranscriptArtifactCAS) == "" && strings.TrimSpace(in.RunID) == "" && s.db != nil && s.cas != nil {
		if idx, err := s.db.GetTranscriptArtifactIndex(ctx, in.AssetID); err == nil && idx != nil && idx.CASHash != "" {
			candidateSegments, _, loadErr := s.loadSegmentsFromTranscript(ctx, in.AssetID, idx.CASHash)
			if loadErr == nil && translationSegmentsExactlyMatch(in.Segments, candidateSegments) {
				in.TranscriptArtifactCAS = idx.CASHash
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("resolve compatible transcript lineage for translation: %w", err)
		}
	}
	if s.db != nil && strings.TrimSpace(in.RunID) != "" {
		if run, err := s.db.GetRun(ctx, in.RunID); err == nil && run != nil {
			var frozenGlossary []domain.GlossaryEntry
			if strings.TrimSpace(run.ConfigSnapshotJSON) != "" {
				var cfg struct {
					Glossary []domain.GlossaryEntry `json:"glossary"`
				}
				if err := json.Unmarshal([]byte(run.ConfigSnapshotJSON), &cfg); err != nil {
					return nil, fmt.Errorf("decode frozen run glossary: %w", err)
				}
				frozenGlossary = cfg.Glossary
			}
			if len(in.Glossary) == 0 {
				in.Glossary = frozenGlossary
			} else if !CanonicalGlossaryEqual(frozenGlossary, in.Glossary) {
				return nil, fmt.Errorf("%w: request glossary conflicts with frozen run snapshot", domain.ErrGlossaryConflict)
			} else {
				in.Glossary = frozenGlossary
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("lookup run %s: %w", in.RunID, err)
		}
	}
	effective, err := effectiveGlossary(in.Glossary, in.Segments)
	if err != nil {
		return nil, fmt.Errorf("invalid glossary: %w", err)
	}
	in.EffectiveGlossary = effective
	inputHash, err := s.computeTranslationInputHash(in)
	if err != nil {
		return nil, fmt.Errorf("compute translation input identity: %w", err)
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
						if err := json.Unmarshal(data, &cachedVariant); err == nil && cachedVariant.SchemaVersion == domain.TranslationSchemaVersion && cachedVariant.ContractID == TranslationContractID && cachedVariant.EffectiveGlossary.Hash == in.EffectiveGlossary.Hash && cachedVariant.InputHash == inputHash && cachedVariant.TranscriptArtifactCAS == in.TranscriptArtifactCAS {
							if cachedVariant.AssetID != in.AssetID {
								cachedVariant.ID = uuid.NewString()
								cachedVariant.AssetID = in.AssetID
								cachedVariant.RunID = in.RunID
								cachedVariant.JobID = in.JobID
								cachedVariant.CreatedAt = time.Now().UTC()
								payload, err := json.MarshalIndent(&cachedVariant, "", "  ")
								if err == nil {
									if obj, err := s.cas.Put(bytes.NewReader(payload)); err == nil {
										cachedVariant.CASHash = obj.SHA256
									}
								}
							} else {
								cachedVariant.CASHash = cachedIdx.CASHash
							}
							cachedVariant.ProvenanceHash = cachedIdx.ProvenanceHash

							if s.db != nil && !in.Ephemeral {
								_ = s.db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
									ID:             cachedVariant.ID,
									AssetID:        in.AssetID,
									RunID:          in.RunID,
									JobID:          in.JobID,
									TargetLanguage: in.TargetLanguage,
									CASHash:        cachedVariant.CASHash,
									ProvenanceHash: cachedVariant.ProvenanceHash,
									ProviderID:     cachedVariant.ProviderID,
									ModelName:      cachedVariant.ModelName,
									ModelVersion:   cachedVariant.ModelVersion,
									OverallQAScore: cachedVariant.OverallQAScore,
									CreatedAt:      time.Now().UTC(),
								})
							}
							return &cachedVariant, nil
						}
					}
				}
			}
		}
	}

	// 4. Provider invocation uses Router-owned retry/fallback semantics so
	// ProviderAttempt and alternate SelectionDecision provenance stay complete.
	result, selectedProv, validatedSegments, overallQAScore, err := s.invokeTranslationWithFallback(ctx, in, routeRes)
	if err != nil {
		return nil, fmt.Errorf("translation provider execution failed: %w", err)
	}
	provenanceHash, err := s.computeProvenanceHash(in, selectedProv)
	if err != nil {
		return nil, fmt.Errorf("compute selected translation cache identity: %w", err)
	}

	// 5. Meaning-First QA Gate: Evaluated per candidate attempt inside invokeTranslationWithFallback.
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

	serviceBaselineID := result.ServiceBaselineID
	observedModel := result.ObservedModel
	systemFingerprint := result.SystemFingerprint
	rp := provider.ExtractRemoteProvenance(selectedProv)
	if serviceBaselineID == "" {
		serviceBaselineID = rp.ServiceBaselineID
	}
	if observedModel == "" {
		observedModel = rp.ObservedModel
	}
	if systemFingerprint == "" {
		systemFingerprint = rp.SystemFingerprint
	}

	// 6. Build immutable TranslationVariant
	variant := &domain.TranslationVariant{
		ID:                    uuid.NewString(),
		SchemaVersion:         domain.TranslationSchemaVersion,
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		SourceLanguage:        in.SourceLanguage,
		TargetLanguage:        in.TargetLanguage,
		ContractID:            TranslationContractID,
		EffectiveGlossary:     in.EffectiveGlossary,
		TranscriptArtifactCAS: in.TranscriptArtifactCAS,
		InputHash:             inputHash,
		Segments:              validatedSegments,
		ProviderID:            provID,
		ModelName:             modelName,
		ModelVersion:          modelVersion,
		ServiceBaselineID:     serviceBaselineID,
		ObservedModel:         observedModel,
		SystemFingerprint:     systemFingerprint,
		ProvenanceHash:        provenanceHash,
		OverallQAScore:        overallQAScore,
		CreatedAt:             time.Now().UTC(),
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

	if s.db != nil && !in.Ephemeral {
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

		// Record the stage only for canonical translations: an ephemeral inline translation did
		// not run the translation stage, so it must not append a success row for it.
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
		if !in.Ephemeral {
			_ = s.db.CreateStageExecution(ctx, stageExec)
		}
	}

	return variant, nil
}

func translationSegmentsExactlyMatch(a, b []domain.TranslationInputSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Index != b[i].Index ||
			strings.TrimSpace(a[i].SourceText) != strings.TrimSpace(b[i].SourceText) ||
			a[i].SpeakerID != b[i].SpeakerID ||
			a[i].StartMs != b[i].StartMs ||
			a[i].EndMs != b[i].EndMs {
			return false
		}
	}
	return true
}

// CanReuseVariant verifies that a persisted translation artifact proves the current
// request-local translation contract and canonical source input. It performs no mutation.
func (s *TranslationService) CanReuseVariant(ctx context.Context, in domain.TranslationJobInput, variant *domain.TranslationVariant) bool {
	if variant == nil || variant.SchemaVersion != domain.TranslationSchemaVersion || variant.ContractID != TranslationContractID {
		return false
	}
	if s.TranslateInvoke != nil || s.router == nil {
		return false
	}
	in.TargetLanguage = strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	in.SourceLanguage = strings.ToLower(strings.TrimSpace(in.SourceLanguage))
	if in.SourceLanguage == "" {
		in.SourceLanguage = "zh"
	}
	if s.db != nil && strings.TrimSpace(in.RunID) != "" {
		run, err := s.db.GetRun(ctx, in.RunID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false
		}
		if run != nil {
			if strings.TrimSpace(in.JobID) != "" && run.JobID != "" && in.JobID != run.JobID {
				return false
			}
			jobID := run.JobID
			if jobID == "" {
				jobID = in.JobID
			}
			if jobID != "" {
				job, err := s.db.GetJob(ctx, jobID)
				if err == nil && job != nil && in.AssetID != "" && job.SourceAssetID != "" && job.SourceAssetID != in.AssetID {
					return false
				}
			}
			var frozenGlossary []domain.GlossaryEntry
			if strings.TrimSpace(run.ConfigSnapshotJSON) != "" {
				var cfg struct {
					Glossary []domain.GlossaryEntry `json:"glossary"`
				}
				if err := json.Unmarshal([]byte(run.ConfigSnapshotJSON), &cfg); err != nil {
					return false
				}
				frozenGlossary = cfg.Glossary
			}
			if len(in.Glossary) == 0 {
				in.Glossary = frozenGlossary
			} else if !CanonicalGlossaryEqual(frozenGlossary, in.Glossary) {
				return false
			} else {
				in.Glossary = frozenGlossary
			}
		}
	}
	if s.db != nil && strings.TrimSpace(in.JobID) != "" {
		job, err := s.db.GetJob(ctx, in.JobID)
		if err == nil && job != nil && in.AssetID != "" && job.SourceAssetID != "" && job.SourceAssetID != in.AssetID {
			return false
		}
	}
	effective, err := effectiveGlossary(in.Glossary, in.Segments)
	if err != nil {
		return false
	}
	in.EffectiveGlossary = effective
	inputHash, err := s.computeTranslationInputHash(in)
	if err != nil {
		return false
	}
	routeRes, err := s.router.Route(ctx, translationRouteRequest(in))
	if err != nil || routeRes == nil || routeRes.SelectedProvider == nil {
		return false
	}
	expectedProvenance, err := s.computeProvenanceHash(in, routeRes.SelectedProvider)
	if err != nil {
		return false
	}
	provenanceMatches := variant.ProvenanceHash == expectedProvenance
	if !provenanceMatches && s.db != nil && strings.TrimSpace(in.RunID) != "" {
		if stageHash, err := s.db.GetStageArtifactHash(ctx, in.RunID, "translation"); err == nil && stageHash != "" && (stageHash == variant.CASHash || variant.CASHash == "") {
			if idx, err := s.db.GetTranslationVariantIndexByRun(ctx, in.RunID); err == nil && idx != nil {
				selModelName, selModelVer := routeRes.SelectedProvider.ModelInfo()
				if idx.CASHash == stageHash && idx.ProvenanceHash == variant.ProvenanceHash &&
					idx.AssetID == in.AssetID && idx.RunID == in.RunID &&
					idx.ProviderID == routeRes.SelectedProvider.ID() &&
					idx.ModelName == selModelName &&
					idx.ModelVersion == selModelVer &&
					variant.ProviderID == idx.ProviderID &&
					variant.ModelName == idx.ModelName &&
					variant.ModelVersion == idx.ModelVersion {
					provenanceMatches = true
				}
			}
		}
	}
	return variant.AssetID == in.AssetID &&
		strings.EqualFold(variant.SourceLanguage, in.SourceLanguage) &&
		strings.EqualFold(variant.TargetLanguage, in.TargetLanguage) &&
		variant.TranscriptArtifactCAS == in.TranscriptArtifactCAS &&
		variant.EffectiveGlossary.Hash == effective.Hash &&
		variant.InputHash == inputHash &&
		provenanceMatches
}

// loadSegmentsFromTranscript loads SpeechBlocks from TranscriptArtifact in CAS/DB.
func (s *TranslationService) loadSegmentsFromTranscript(ctx context.Context, assetID, explicitCAS string) ([]domain.TranslationInputSegment, string, error) {
	if s.cas == nil {
		return nil, "", fmt.Errorf("database and CAS required to load transcript")
	}

	casHash := explicitCAS
	if casHash == "" {
		if s.db == nil {
			return nil, "", fmt.Errorf("database and CAS required to load transcript")
		}
		idx, err := s.db.GetTranscriptArtifactIndex(ctx, assetID)
		if err != nil {
			return nil, "", fmt.Errorf("transcript artifact not found for asset %s: %w", assetID, err)
		}
		casHash = idx.CASHash
	}

	rc, err := s.cas.Get(casHash)
	if err != nil {
		return nil, "", fmt.Errorf("read transcript artifact from CAS (%s): %w", casHash, err)
	}
	defer rc.Close()

	var transcript domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&transcript); err != nil {
		return nil, "", fmt.Errorf("decode transcript artifact: %w", err)
	}

	if transcript.AssetID != "" && transcript.AssetID != assetID {
		return nil, "", fmt.Errorf("transcript artifact %s belongs to asset %q, not %q", casHash, transcript.AssetID, assetID)
	}

	var rolePlan *domain.AudioRolePlan
	if s.db != nil && assetID != "" {
		rolePlan, _ = s.db.GetAudioRolePlan(ctx, assetID)
	}
	segments := domain.CanonicalTranslationSegments(&transcript, rolePlan)
	return segments, casHash, nil
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
		SemanticConfig: func() map[string]any {
			cfg := map[string]any{
				"source_language":    in.SourceLanguage,
				"meaning_contract":   translationMeaningContractVersion,
				"contract_id":        TranslationContractID,
				"effective_glossary": in.EffectiveGlossary.Hash,
			}
			if bp, ok := p.(interface{ ServiceBaselineID() string }); ok {
				if baseline := strings.TrimSpace(bp.ServiceBaselineID()); baseline != "" {
					cfg["service_baseline_id"] = baseline
				}
			}
			return cfg
		}(),
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
		SourceLanguage        string                           `json:"source_language"`
		Segments              []domain.TranslationInputSegment `json:"segments"`
		EffectiveGlossaryHash string                           `json:"effective_glossary_hash,omitempty"`
	}{
		SourceLanguage:        strings.ToLower(strings.TrimSpace(in.SourceLanguage)),
		Segments:              in.Segments,
		EffectiveGlossaryHash: in.EffectiveGlossary.Hash,
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
// A meaning-first QA failure inside the Router.ExecuteRoutedWithRetry attempt returns an error wrapping
// domain.ErrQualityRejected plus the specific QA error, so the Router records quality_failed for that
// attempt and advances to the next candidate. The flagged candidate is retained: a clean lane is always
// preferred, but when every lane carries QA flags the best of them is used and the flags travel to the
// review queue instead of killing the stage.
func (s *TranslationService) invokeTranslationWithFallback(ctx context.Context, in domain.TranslationJobInput, routeRes *provider.RouteResult) (*provider.TranslationResult, provider.Provider, []domain.TranslationSegment, float64, error) {
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
		if err != nil {
			return nil, nil, nil, 0, err
		}
		if res == nil {
			return nil, nil, nil, 0, fmt.Errorf("custom translation hook returned nil result")
		}
		validSegs, score, qaErr := s.validateMeaningQA(in, res.Segments)
		if len(validSegs) == 0 {
			if qaErr != nil {
				return nil, nil, nil, 0, qaErr
			}
			return nil, nil, nil, 0, fmt.Errorf("%w: custom translation hook produced no segments", domain.ErrQualityRejected)
		}
		return res, p, validSegs, score, nil
	}

	if s.router == nil {
		return nil, nil, nil, 0, fmt.Errorf("provider router is not configured")
	}

	routeReq := translationRouteRequest(in)
	if routeRes == nil {
		var err error
		routeRes, err = s.router.Route(ctx, routeReq)
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("routing translation provider failed: %w", err)
		}
	}

	inputHash, err := s.computeTranslationInputHash(in)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("compute translation input hash: %w", err)
	}

	var result *provider.TranslationResult
	var selected provider.Provider
	var validatedSegments []domain.TranslationSegment
	var overallQAScore float64

	type flaggedCandidate struct {
		result   *provider.TranslationResult
		provider provider.Provider
		segments []domain.TranslationSegment
		score    float64
	}
	var bestFlagged *flaggedCandidate

	err = s.router.ExecuteRoutedWithRetry(ctx, routeReq, routeRes, inputHash, 1, func(cand provider.Provider, _ int) error {
		res, invokeErr := s.invokeProvider(ctx, cand, in)
		if invokeErr != nil {
			return invokeErr
		}
		if res == nil {
			return fmt.Errorf("%w: translation provider %s returned nil result", domain.ErrQualityRejected, cand.ID())
		}
		validSegs, score, qaErr := s.validateMeaningQA(in, res.Segments)
		if qaErr != nil {
			if !qaFlagged(validSegs) {
				// Unusable candidate (e.g. no segments at all): nothing to review.
				return qaErr
			}
			if bestFlagged == nil || score > bestFlagged.score {
				bestFlagged = &flaggedCandidate{result: res, provider: cand, segments: validSegs, score: score}
			}
			return qaErr
		}
		result = res
		selected = cand
		validatedSegments = validSegs
		overallQAScore = score
		return nil
	})
	if err != nil {
		if bestFlagged == nil || !translationFlaggedFallbackAllowed(err) {
			return nil, nil, nil, 0, err
		}
		// Every lane tripped the meaning gate. Ship the best of them with per-segment
		// flags: the review queue names the violation and the operator decides.
		result = bestFlagged.result
		selected = bestFlagged.provider
		validatedSegments = bestFlagged.segments
		overallQAScore = bestFlagged.score
	}
	if result == nil || selected == nil {
		return nil, nil, nil, 0, fmt.Errorf("translation execution completed without a selected provider result")
	}
	return result, selected, validatedSegments, overallQAScore, nil
}

// translationFailClosedErrors mirrors the Router's non-transient governance / policy / provenance
// refusals (internal/provider/router.go branch 1) that terminate execution immediately without
// candidate retry or fallback.
var translationFailClosedErrors = []error{
	domain.ErrPolicyBlocked,
	domain.ErrConsentRequired,
	domain.ErrAuthRequired,
	domain.ErrLicenseManifestMissing,
	domain.ErrRawSecretForbidden,
	domain.ErrContentUnavailable,
	domain.ErrInvalidURL,
	domain.ErrUnsupportedMediaType,
	domain.ErrInconsistentProvenance,
}

// translationFlaggedFallbackAllowed confirms the terminal execution error is genuinely the
// meaning-gate rejection (ErrQualityRejected) that the flagged fallback exists for, and not a
// fail-closed router governance, policy, authorization, or provenance refusal.
func translationFlaggedFallbackAllowed(err error) bool {
	if !errors.Is(err, domain.ErrQualityRejected) {
		return false
	}
	for _, failClosed := range translationFailClosedErrors {
		if errors.Is(err, failClosed) {
			return false
		}
	}
	return true
}

// validateMeaningQA evaluates the meaning preservation QA gate for every segment.
//
// A segment that fails the gate is flagged (PassedQAGate=false plus a ReviewReason
// naming the violation) and returned alongside the ones that passed: the artifact stays
// usable and the operator resolves the flag in the review queue. The returned error
// signals "this candidate carries flags" — never "discard it" — and an error with no
// segments means the candidate is unusable (nothing to review).
func (s *TranslationService) validateMeaningQA(in domain.TranslationJobInput, segments []domain.TranslationSegment) ([]domain.TranslationSegment, float64, error) {
	if len(segments) == 0 {
		return nil, 0, fmt.Errorf("%w: translation produced no segments", domain.ErrQualityRejected)
	}

	validatedSegments := make([]domain.TranslationSegment, 0, len(segments))
	var totalConfidence float64
	var firstViolation error

	for _, seg := range segments {
		qaRes := s.qaGate.ValidateSegment(seg.SourceText, seg.TargetText, in.SourceLanguage, in.TargetLanguage, glossaryForSource(in.EffectiveGlossary, seg.SourceText))

		seg.PassedQAGate = qaRes.Passed
		seg.QAConfidence = qaRes.Confidence
		seg.KeyFacts = qaRes.ExtractedFacts
		seg.NegationPolarity = qaRes.NegationPolarity
		if !qaRes.Passed {
			seg.ReviewReason = qaReviewReason(seg.Index, qaRes)
			if firstViolation == nil {
				firstViolation = qaRes.Err
				if firstViolation == nil {
					firstViolation = domain.ErrMeaningPreservationFailed
				}
			}
		}
		totalConfidence += qaRes.Confidence

		validatedSegments = append(validatedSegments, seg)
	}

	overallQAScore := 1.0
	if len(validatedSegments) > 0 {
		overallQAScore = totalConfidence / float64(len(validatedSegments))
	}

	if firstViolation != nil {
		return validatedSegments, overallQAScore, fmt.Errorf("%w: %w", domain.ErrQualityRejected, firstViolation)
	}
	return validatedSegments, overallQAScore, nil
}

// qaReviewReason returns the operator-facing reason for a flagged segment, or "" when the
// segment passed the gate. Callers use it wherever a QA verdict is (re)computed so a
// corrected segment never keeps a stale reason.
//
// The reason is the meaning-gate violations for that segment, shortened to the sentence the
// review queue shows.
func qaReviewReason(index int, res QAResult) string {
	if res.Passed {
		return ""
	}
	const maxLen = 200
	summary := strings.TrimSpace(strings.Join(res.Violations, "; "))
	if summary == "" {
		if res.Err != nil {
			summary = res.Err.Error()
		} else {
			summary = domain.ErrMeaningPreservationFailed.Error()
		}
	}
	if len(summary) > maxLen {
		summary = summary[:maxLen]
	}
	return fmt.Sprintf("segment %d: %s", index, summary)
}

// qaFlagged reports whether a candidate carries segments the meaning gate flagged and
// can therefore still be persisted as best effort.
func qaFlagged(segments []domain.TranslationSegment) bool {
	for _, seg := range segments {
		if !seg.PassedQAGate {
			return true
		}
	}
	return false
}

func translationRouteRequest(in domain.TranslationJobInput) provider.RouteRequest {
	return provider.RouteRequest{
		RunID:                 in.RunID,
		Stage:                 provider.TypeTranslation,
		Language:              in.TargetLanguage,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
		ConsentGranted:        in.ConsentGranted,
	}
}

// invokeProvider calls the provider's translation method.
func (s *TranslationService) invokeProvider(ctx context.Context, p provider.Provider, in domain.TranslationJobInput) (*provider.TranslationResult, error) {
	textProv, ok := p.(provider.TextTranslationProvider)
	if !ok {
		return nil, fmt.Errorf("provider %s does not implement TextTranslationProvider", p.ID())
	}

	req := provider.TranslationRequest{
		RunID:                 in.RunID,
		SourceLanguage:        in.SourceLanguage,
		TargetLanguage:        in.TargetLanguage,
		Segments:              in.Segments,
		EffectiveGlossary:     in.EffectiveGlossary,
		AuthorizedCredentials: in.AuthorizedCredentials,
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
	// Fail closed if AudioRolePlan is missing, unreadable, or contains no dub-eligible dialogue.
	if s.db == nil || strings.TrimSpace(in.AssetID) == "" {
		return nil, domain.ErrAudioRolePlanRequired
	}
	rolePlan, err := s.db.GetAudioRolePlan(ctx, in.AssetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, domain.ErrAudioRolePlanRequired
		}
		return nil, fmt.Errorf("get audio role plan: %w", err)
	}
	if rolePlan == nil {
		return nil, domain.ErrAudioRolePlanRequired
	}
	if !domain.IsDubEligible(rolePlan) {
		return nil, domain.ErrNoDubbingRequired
	}

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
			dubSegments = append(dubSegments, domain.DubScriptSegment{
				Index: seg.Index, SourceText: seg.SourceText, MeaningText: seg.TargetText, SpokenText: seg.TargetText,
				SpeakerID: seg.SpeakerID, StartMs: seg.StartMs, EndMs: seg.EndMs, SlotDurationMs: slotDurationMs,
				KeyFacts: seg.KeyFacts, PassedQAGate: seg.PassedQAGate, QAConfidence: seg.QAConfidence,
				RequiresReview: true, ReviewReason: "INVALID_SOURCE_TIMING",
			})
			variantRequiresReview = true
			totalConfidence += seg.QAConfidence
			continue
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
		applicableGlossary := glossaryForSource(transVariant.EffectiveGlossary, seg.SourceText)
		adaptRes, err := adapter.AdaptSpokenScript(ctx, provider.SpokenScriptAdaptationRequest{
			SourceText:            seg.SourceText,
			SourceLanguage:        in.SourceLanguage,
			MeaningText:           meaningText,
			TargetLanguage:        in.TargetLanguage,
			SlotDurationMs:        slotDurationMs,
			SourceSpeakingRateCPS: srcCPS,
			SourceGapAfterMs:      sourceGapAfterMs,
			HasNextTurn:           hasNextTurn,
			ProtectedTerms:        applicableGlossary,
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

		// Run Translation QA Gate to guarantee facts/names/numbers/negation survive.
		// A violation flags the segment for operator review; it never aborts the stage.
		qaRes := s.qaGate.ValidateSegment(seg.SourceText, spokenText, in.SourceLanguage, in.TargetLanguage, applicableGlossary)
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

				qaRes = s.qaGate.ValidateSegment(seg.SourceText, spokenText, in.SourceLanguage, in.TargetLanguage, applicableGlossary)
			}
			if !qaRes.Passed {
				requiresReview = true
				reviewReason = domain.ReviewReasonMeaningCorrupted
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
			PassedQAGate:          qaRes.Passed,
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
	if variant.SchemaVersion != domain.TranslationSchemaVersion || variant.ContractID != TranslationContractID || variant.InputHash == "" || variant.ProvenanceHash == "" {
		return nil, "", fmt.Errorf("translation variant does not satisfy current translation contract")
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
