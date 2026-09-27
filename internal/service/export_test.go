package service

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
	"strings"
	"time"
)

// ResolveRunTranscriptCASForTest exposes resolveRunTranscriptCAS for whitebox testing in service_test.
func ResolveRunTranscriptCASForTest(ctx context.Context, db *storage.DB, runID, assetID, purpose string) (string, error) {
	return resolveRunTranscriptCAS(ctx, db, runID, assetID, purpose)
}

// ResolveCanonicalRolePlanForTest exposes resolveCanonicalRolePlan for whitebox testing in service_test.
func (s *TranslationService) ResolveCanonicalRolePlanForTest(ctx context.Context, assetID, runID string) (*domain.AudioRolePlan, error) {
	return s.resolveCanonicalRolePlan(ctx, assetID, runID)
}
func (s *TranslationService) LoadSegmentsFromTranscriptForTest(ctx context.Context, assetID, runID, explicitCAS string) ([]domain.TranslationInputSegment, string, error) {
	return s.loadSegmentsFromTranscript(ctx, assetID, runID, explicitCAS)
}

// ComputeTranslationInputHashForTest exposes computeTranslationInputHash for whitebox testing in service_test.

// EffectiveGlossaryForTest exposes effectiveGlossary for testing in service_test.
func EffectiveGlossaryForTest(entries []domain.GlossaryEntry, segments []domain.TranslationInputSegment) (domain.EffectiveGlossary, error) {
	return effectiveGlossary(entries, segments)
}
func (s *TranslationService) ComputeTranslationInputHashForTest(in domain.TranslationJobInput) (string, error) {
	return s.computeTranslationInputHash(in)
}

// SeedRunTranscriptForTest creates a transcript artifact in CAS and associates it with runID in stage_executions.
// Optional speech blocks make the artifact carry real block timing, which run-scoped playback-boundary
// derivation reads; without them the artifact is an owner-only stub (sufficient for Translate, whose
// callers pass explicit segments).
func SeedRunTranscriptForTest(ctx context.Context, db *storage.DB, casStore *cas.Store, assetID, runID string, blocks ...domain.SpeechBlock) string {
	if casStore == nil || db == nil {
		return ""
	}
	var payload []byte
	if len(blocks) > 0 {
		b, err := json.Marshal(domain.TranscriptArtifact{
			ID:           "transcript-" + assetID,
			AssetID:      assetID,
			RunID:        runID,
			SpeechBlocks: blocks,
			CreatedAt:    time.Now().UTC(),
		})
		if err != nil {
			panic(err)
		}
		payload = b
	} else {
		payload = []byte(`{"asset_id":"` + assetID + `"}`)
	}
	tObj, err := casStore.Put(bytes.NewReader(payload))
	if err != nil {
		panic(err)
	}
	if _, err := db.GetRun(ctx, runID); err != nil {
		_ = db.CreateJob(ctx, domain.LocalizationJob{
			ID:             "job-" + runID,
			SourceAssetID:  assetID,
			TargetLanguage: "vi",
			CreatedAt:      time.Now().UTC(),
		})
		_ = db.CreateRun(ctx, domain.LocalizationRun{
			ID:        runID,
			JobID:     "job-" + runID,
			Status:    "running",
			CreatedAt: time.Now().UTC(),
		})
	}
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      time.Now().UTC().Add(-time.Hour),
	})
	if existingHash, _ := db.GetStageArtifactHash(ctx, runID, "audio_role_plan"); existingHash == "" {
		if plan, err := db.GetAudioRolePlan(ctx, assetID); err == nil && plan != nil {
			casHash := plan.CASHash
			if casHash == "" && casStore != nil {
				if b, err := json.Marshal(plan); err == nil {
					if obj, err := casStore.Put(bytes.NewReader(b)); err == nil {
						casHash = obj.SHA256
						plan.CASHash = casHash
						_ = db.SaveAudioRolePlan(ctx, *plan)
					}
				}
			}
			if casHash != "" {
				_ = db.CreateStageExecution(ctx, domain.StageExecution{
					ID:             uuid.NewString(),
					RunID:          runID,
					Stage:          "audio_role_plan",
					Status:         domain.StageStatusSucceeded,
					ArtifactSHA256: casHash,
					CreatedAt:      time.Now().UTC().Add(-time.Hour),
				})
			}
		}
	}
	return tObj.SHA256
}

// PinAudioRolePlanForTest persists an AudioRolePlan to CAS and DB, and binds it to runID
// via stage_executions with status=succeeded so run-scoped lineage resolution succeeds.
func PinAudioRolePlanForTest(ctx context.Context, db *storage.DB, casStore *cas.Store, runID string, plan domain.AudioRolePlan) string {
	if casStore == nil || db == nil {
		return ""
	}
	if plan.ID == "" {
		plan.ID = uuid.NewString()
	}
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = time.Now().UTC()
	}
	if plan.CASHash == "" || !casStore.Exists(plan.CASHash) {
		b, err := json.Marshal(plan)
		if err != nil {
			panic(err)
		}
		obj, err := casStore.Put(bytes.NewReader(b))
		if err != nil {
			panic(err)
		}
		plan.CASHash = obj.SHA256
	}
	if err := db.SaveAudioRolePlan(ctx, plan); err != nil {
		panic(err)
	}
	if strings.TrimSpace(runID) != "" {
		if _, err := db.GetRun(ctx, runID); err != nil {
			_ = db.CreateJob(ctx, domain.LocalizationJob{
				ID:             "job-" + runID,
				SourceAssetID:  plan.AssetID,
				TargetLanguage: "vi",
				CreatedAt:      time.Now().UTC(),
			})
			_ = db.CreateRun(ctx, domain.LocalizationRun{
				ID:        runID,
				JobID:     "job-" + runID,
				Status:    "running",
				CreatedAt: time.Now().UTC(),
			})
		}
		now := time.Now().UTC()
		se := domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          runID,
			Stage:          "audio_role_plan",
			Status:         domain.StageStatusSucceeded,
			ArtifactSHA256: plan.CASHash,
			StartedAt:      &now,
			CompletedAt:    &now,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := db.CreateStageExecution(ctx, se); err != nil {
			panic(err)
		}
	}
	return plan.CASHash
}
