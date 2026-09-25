package service

import (
	"bytes"
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
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
func SeedRunTranscriptForTest(ctx context.Context, db *storage.DB, casStore *cas.Store, assetID, runID string) string {
	if casStore == nil || db == nil {
		return ""
	}
	tObj, err := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"` + assetID + `"}`)))
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
		CreatedAt:      time.Now().UTC(),
	})
	return tObj.SHA256
}
