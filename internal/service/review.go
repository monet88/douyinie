package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// ReviewService projects actionable exceptions from stage artifacts into structured ReviewItem projections.
// Invariant: Exception-only review — passing or auto-resolved items do not surface in the review queue.
type ReviewService struct {
	db             *storage.DB
	cas            *cas.Store
	translationSvc *TranslationService
	dubbingSvc     *DubbingService
	audioMixSvc    *AudioMixService
	visualTextSvc  *VisualTextService
	renderSvc      *RenderService
}

// NewReviewService creates a new ReviewService instance.
func NewReviewService(db *storage.DB, casStore *cas.Store) *ReviewService {
	return &ReviewService{
		db:  db,
		cas: casStore,
	}
}

// SetTranslationService injects the TranslationService used for translation rerun.
func (s *ReviewService) SetTranslationService(svc *TranslationService) {
	s.translationSvc = svc
}

// SetDubbingService injects the DubbingService used for TTS synthesis rerun.
func (s *ReviewService) SetDubbingService(svc *DubbingService) {
	s.dubbingSvc = svc
}

// SetAudioMixService injects the AudioMixService used for stem re-mixing.
func (s *ReviewService) SetAudioMixService(svc *AudioMixService) {
	s.audioMixSvc = svc
}

// SetVisualTextService injects the VisualTextService used for subtitle track regeneration.
func (s *ReviewService) SetVisualTextService(svc *VisualTextService) {
	s.visualTextSvc = svc
}

// SetRenderService injects the RenderService used for render plan refreezing.
func (s *ReviewService) SetRenderService(svc *RenderService) {
	s.renderSvc = svc
}

// ManualOverrideInput defines parameters for recording an operator manual override.
type ManualOverrideInput struct {
	RunID          string                `json:"run_id,omitempty"`
	JobID          string                `json:"job_id,omitempty"`
	AssetID        string                `json:"asset_id"`
	TargetLanguage string                `json:"target_language"`
	ReviewItemID   string                `json:"review_item_id"`
	ItemType       domain.ReviewItemType `json:"item_type"`
	Stage          string                `json:"stage"`
	ItemIndex      int                   `json:"item_index,omitempty"`
	SegmentID      string                `json:"segment_id,omitempty"`
	RegionID       string                `json:"region_id,omitempty"`
	Reason         string                `json:"reason"`
	Operator       string                `json:"operator"`
}

// RecordManualOverride records an auditable operator acceptance of a flagged exception.
// Invariant: Overrides are append-only audit records; historical QA scores and failures are never mutated or rewritten to PASS.
// Invariant: Manual override requires an exact, currently pending ReviewItemID for the same asset and language.
// Stage-only or implicit index-0 requests, nonexistent IDs, stale IDs, or non-pending IDs must be rejected.
func (s *ReviewService) RecordManualOverride(ctx context.Context, in ManualOverrideInput) (*domain.ReviewOverride, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if strings.TrimSpace(in.ReviewItemID) == "" {
		return nil, errors.New("exact pending review_item_id is required for manual override")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, errors.New("override reason/notes is required for audit trail")
	}
	if in.TargetLanguage == "" {
		in.TargetLanguage = "vi"
	}
	if strings.TrimSpace(in.Operator) == "" {
		in.Operator = "operator"
	}

	// 1. Verify that the requested review item is currently pending for the
	// requested run when available. This prevents a same-asset newer run from
	// satisfying an override submitted from an older run's inspector.
	var pendingItems []domain.ReviewItem
	var err error
	if strings.TrimSpace(in.RunID) != "" {
		pendingItems, err = s.ProjectReviewItemsForRun(ctx, in.AssetID, in.TargetLanguage, in.RunID)
	} else {
		pendingItems, err = s.ProjectReviewItems(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil {
		return nil, fmt.Errorf("project review items: %w", err)
	}

	var targetItem *domain.ReviewItem
	for i := range pendingItems {
		if pendingItems[i].ID == in.ReviewItemID {
			targetItem = &pendingItems[i]
			break
		}
	}
	if targetItem == nil {
		return nil, fmt.Errorf("review item %q not found in pending review queue for asset %s (%s)", in.ReviewItemID, in.AssetID, in.TargetLanguage)
	}
	if targetItem.Type == domain.ReviewItemTypeTTSOverrun && targetItem.Stage == "dub_synthesize" {
		return nil, fmt.Errorf("review item %q is a hard timing blocker and cannot be manually overridden; correct or re-synthesize the segment", targetItem.ID)
	}

	runID := in.RunID
	if runID == "" {
		runID = targetItem.RunID
	}
	jobID := in.JobID
	if jobID == "" {
		jobID = targetItem.JobID
	}

	override := domain.ReviewOverride{
		ID:             uuid.NewString(),
		RunID:          runID,
		JobID:          jobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		ReviewItemID:   targetItem.ID,
		ItemType:       targetItem.Type,
		Stage:          targetItem.Stage,
		ItemIndex:      targetItem.ItemIndex,
		SegmentID:      targetItem.SegmentID,
		RegionID:       targetItem.RegionID,
		Action:         string(domain.ReviewOverrideActionManualOverride),
		Reason:         in.Reason,
		Operator:       in.Operator,
		CreatedAt:      time.Now().UTC(),
	}

	if err := s.db.SaveReviewOverride(ctx, override); err != nil {
		return nil, fmt.Errorf("persist review override: %w", err)
	}

	return &override, nil
}

// TargetTextCorrectionInput defines parameters for inspector target text editing and targeted rerun.
type TargetTextCorrectionInput struct {
	RunID                 string                  `json:"run_id"`
	JobID                 string                  `json:"job_id,omitempty"`
	AssetID               string                  `json:"asset_id"`
	TargetLanguage        string                  `json:"target_language"`
	SegmentIndex          int                     `json:"segment_index"`
	NewTargetText         string                  `json:"new_target_text"`
	SpokenTextOverride    string                  `json:"spoken_text_override,omitempty"`
	Reason                string                  `json:"reason,omitempty"`
	Operator              string                  `json:"operator,omitempty"`
	ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
}

// TargetTextCorrectionResult returns the artifacts produced by targeted invalidation and rerun.
type TargetTextCorrectionResult struct {
	AssetID               string                  `json:"asset_id"`
	TargetLanguage        string                  `json:"target_language"`
	SegmentIndex          int                     `json:"segment_index"`
	NewTargetText         string                  `json:"new_target_text"`
	TranslationVariantCAS string                  `json:"translation_variant_cas"`
	DubScriptVariantCAS   string                  `json:"dub_script_variant_cas"`
	DubSegmentsVariantCAS string                  `json:"dub_segments_variant_cas,omitempty"`
	DubMixCAS             string                  `json:"dub_mix_cas,omitempty"`
	LocalizedSubtitleCAS  string                  `json:"localized_subtitle_cas,omitempty"`
	RenderPlanCAS         string                  `json:"render_plan_cas,omitempty"`
	Status                domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message               string                  `json:"message"`
}

// VoiceReassignCorrectionInput defines parameters for inspector voice reassignment and targeted rerun.
type VoiceReassignCorrectionInput struct {
	RunID                 string                         `json:"run_id"`
	JobID                 string                         `json:"job_id,omitempty"`
	AssetID               string                         `json:"asset_id"`
	TargetLanguage        string                         `json:"target_language"`
	CustomAssignments     map[string]domain.VoiceProfile `json:"custom_assignments,omitempty"`
	UseSameVoiceForAll    bool                           `json:"use_same_voice_for_all"`
	Reason                string                         `json:"reason,omitempty"`
	Operator              string                         `json:"operator,omitempty"`
	ExecutionProfile      domain.ExecutionProfile        `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                       `json:"authorized_credentials,omitempty"`
}

// VoiceReassignCorrectionResult returns the artifacts produced by targeted voice invalidation and rerun.
type VoiceReassignCorrectionResult struct {
	VoiceAssignmentCAS    string                  `json:"voice_assignment_cas"`
	DubSegmentsVariantCAS string                  `json:"dub_segments_variant_cas,omitempty"`
	DubMixCAS             string                  `json:"dub_mix_cas,omitempty"`
	RenderPlanCAS         string                  `json:"render_plan_cas,omitempty"`
	InvalidatedSpeakers   []string                `json:"invalidated_speakers,omitempty"`
	Status                domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message               string                  `json:"message"`
}

// RegionGeometryCorrectionInput defines parameters for inspector region override and targeted rerun.
type RegionGeometryCorrectionInput struct {
	RunID                 string                        `json:"run_id,omitempty"`
	JobID                 string                        `json:"job_id,omitempty"`
	AssetID               string                        `json:"asset_id"`
	TargetLanguage        string                        `json:"target_language"`
	Overrides             []domain.RegionOverride       `json:"overrides"`
	InpaintingFallbacks   []string                      `json:"inpainting_fallbacks,omitempty"`
	SceneProtectedRegions []domain.SceneProtectedRegion `json:"scene_protected_regions,omitempty"`
	Reason                string                        `json:"reason,omitempty"`
	Operator              string                        `json:"operator,omitempty"`
}

// RegionGeometryCorrectionResult returns the artifacts produced by region override and visual track regeneration.
type RegionGeometryCorrectionResult struct {
	LocalizedVisualTrackCAS string                  `json:"localized_visual_track_cas"`
	LocalizedSubtitleCAS    string                  `json:"localized_subtitle_cas"`
	RenderPlanCAS           string                  `json:"render_plan_cas,omitempty"`
	PreviewRenderCAS        string                  `json:"preview_render_cas,omitempty"`
	Status                  domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message                 string                  `json:"message"`
}

func (s *ReviewService) recordStageExecution(ctx context.Context, runID, stage, status, casHash string) error {
	if runID == "" {
		return nil
	}
	// A legacy asset-scoped correction runs under a synthetic "corr-run-…" id with no run row, and stage
	// rows are run-bound: there is nothing to attach such a correction's artifacts to.
	if _, err := s.db.GetRun(ctx, runID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("look up run %s for %s stage record: %w", runID, stage, err)
	}
	now := time.Now().UTC()
	se := domain.StageExecution{
		ID:        uuid.NewString(),
		RunID:     runID,
		Stage:     stage,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if status == domain.StageStatusSucceeded {
		se.ArtifactSHA256 = casHash
		se.StartedAt = &now
		se.CompletedAt = &now
	}
	if err := s.db.CreateStageExecution(ctx, se); err != nil {
		return fmt.Errorf("record %s stage for run %s: %w", stage, runID, err)
	}
	return nil
}

// recordCorrectionStage appends a succeeded stage execution for an artifact a correction just produced,
// so a resumed run reuses what the correction built instead of the pre-correction artifact recorded when
// the run first passed through that stage. Nothing else moves the run's stage rows.
func (s *ReviewService) recordCorrectionStage(ctx context.Context, runID, stage, casHash string) error {
	if casHash == "" {
		return nil
	}
	return s.recordStageExecution(ctx, runID, stage, domain.StageStatusSucceeded, casHash)
}

// invalidateCorrectionStage records a queued stage execution with no artifact, invalidating
// any previously succeeded execution of this stage so a resumed run will not reuse stale artifacts.
func (s *ReviewService) invalidateCorrectionStage(ctx context.Context, runID, stage string) error {
	return s.recordStageExecution(ctx, runID, stage, domain.StageStatusQueued, "")
}

// CorrectTargetText updates target text for a segment and triggers targeted rerun of only declared downstream descendants:
// TTS -> DubSegment -> DubMix -> LocalizedSubtitleTrack -> Render.
// Invariant: Source-derived artifacts (source media, audio stems, transcript alignment, text regions) are strictly REUSED.
func (s *ReviewService) CorrectTargetText(ctx context.Context, in TargetTextCorrectionInput) (*TargetTextCorrectionResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if strings.TrimSpace(in.NewTargetText) == "" {
		return nil, errors.New("new_target_text is required")
	}
	if in.SegmentIndex < 0 {
		return nil, errors.New("segment_index must be >= 0")
	}
	if in.TargetLanguage == "" {
		in.TargetLanguage = "vi"
	}
	requestedRunID := strings.TrimSpace(in.RunID)
	if requestedRunID == "" {
		return nil, errors.New("run_id is required for target text correction")
	}

	result := &TargetTextCorrectionResult{
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		SegmentIndex:   in.SegmentIndex,
		NewTargetText:  in.NewTargetText,
		Status:         domain.ReviewItemStatusPending,
	}
	// Fail-closed invariant: target text correction requires all declared downstream rerun services.
	if s.translationSvc == nil {
		return nil, errors.New("translation service is required for target text correction")
	}
	if s.dubbingSvc == nil {
		return nil, errors.New("dubbing service is required for target text correction")
	}
	if s.audioMixSvc == nil {
		return nil, errors.New("audio mix service is required for target text correction")
	}
	if s.visualTextSvc == nil {
		return nil, errors.New("visual text service is required for target text correction")
	}
	if s.renderSvc == nil {
		return nil, errors.New("render service is required for target text correction")
	}

	// 1. Update TranslationVariant with honest QA validation (never hardcoding 1.0/PASS)
	var transIdx *storage.TranslationVariantIndex
	var err error
	transIdx, err = s.db.GetTranslationVariantIndexByRun(ctx, requestedRunID)
	if errors.Is(err, storage.ErrNotFound) {
		// A run that replayed cached stages owns no variant row of its own; its stage
		// execution records the artifact it consumed (see runBoundArtifactCAS).
		casHash, boundErr := s.runBoundArtifactCAS(ctx, requestedRunID, "translation")
		if boundErr != nil {
			return nil, boundErr
		}
		if casHash != "" {
			transIdx = &storage.TranslationVariantIndex{
				AssetID: in.AssetID, RunID: requestedRunID, TargetLanguage: in.TargetLanguage, CASHash: casHash,
			}
			err = nil
		}
	}
	if err == nil && transIdx != nil && (transIdx.AssetID != in.AssetID || !strings.EqualFold(transIdx.TargetLanguage, in.TargetLanguage)) {
		return nil, fmt.Errorf("translation variant run binding mismatch for run %s", requestedRunID)
	}
	if err != nil {
		return nil, fmt.Errorf("load translation variant index: %w", err)
	}
	trc, err := s.cas.Get(transIdx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("load translation variant from CAS (%s): %w", transIdx.CASHash, err)
	}
	var tVar domain.TranslationVariant
	if err := json.NewDecoder(trc).Decode(&tVar); err != nil {
		trc.Close()
		return nil, fmt.Errorf("decode translation variant (%s): %w", transIdx.CASHash, err)
	}
	trc.Close()
	if tVar.AssetID != in.AssetID || !strings.EqualFold(tVar.TargetLanguage, in.TargetLanguage) {
		return nil, fmt.Errorf("translation variant artifact %s belongs to asset %s (%s), expected asset %s (%s)", transIdx.CASHash, tVar.AssetID, tVar.TargetLanguage, in.AssetID, in.TargetLanguage)
	}
	if tVar.SchemaVersion != domain.TranslationSchemaVersion || tVar.ContractID != TranslationContractID {
		return nil, fmt.Errorf("translation variant %s uses stale translation contract (schema=%d contract=%q)", transIdx.CASHash, tVar.SchemaVersion, tVar.ContractID)
	}
	// A segment's own index is the source speech-block index, not its position in the slice: a clip
	// with silent stretches indexes 0,2,4,6, so indexing the slice with it rejected every correction
	// past the first (live evidence, run eec68c8d: "segment index 6 out of bounds (total 4)").
	transPos := slices.IndexFunc(tVar.Segments, func(s domain.TranslationSegment) bool { return s.Index == in.SegmentIndex })
	if transPos < 0 {
		return nil, fmt.Errorf("segment index %d not present in the translation variant (%d segments)", in.SegmentIndex, len(tVar.Segments))
	}

	qaGate := s.translationSvc.qaGate
	if qaGate == nil {
		qaGate = NewMeaningFirstQAGate()
	}

	applicableGlossary := glossaryForSource(tVar.EffectiveGlossary, tVar.Segments[transPos].SourceText)
	qaRes := qaGate.ValidateSegment(tVar.Segments[transPos].SourceText, in.NewTargetText, tVar.SourceLanguage, in.TargetLanguage, applicableGlossary)

	tVar.ID = uuid.NewString()
	tVar.RunID = in.RunID
	if in.JobID != "" {
		tVar.JobID = in.JobID
	}
	tVar.Segments[transPos].TargetText = in.NewTargetText
	tVar.Segments[transPos].PassedQAGate = qaRes.Passed
	tVar.Segments[transPos].QAConfidence = qaRes.Confidence
	tVar.Segments[transPos].ReviewReason = qaReviewReason(in.SegmentIndex, qaRes)
	tVar.Segments[transPos].KeyFacts = qaRes.ExtractedFacts
	tVar.Segments[transPos].NegationPolarity = qaRes.NegationPolarity
	tVar.CreatedAt = time.Now().UTC()

	var totalConf float64
	for _, seg := range tVar.Segments {
		totalConf += seg.QAConfidence
	}
	if len(tVar.Segments) > 0 {
		tVar.OverallQAScore = totalConf / float64(len(tVar.Segments))
	}

	// Compute updated provenance hash
	hTrans := sha256.New()
	_, _ = hTrans.Write([]byte(fmt.Sprintf("%s:%s:%d:%s", transIdx.ProvenanceHash, in.TargetLanguage, in.SegmentIndex, in.NewTargetText)))
	tVar.ProvenanceHash = hex.EncodeToString(hTrans.Sum(nil))

	tBytes, err := json.MarshalIndent(tVar, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal updated translation variant: %w", err)
	}
	tObj, err := s.cas.Put(bytes.NewReader(tBytes))
	if err != nil {
		return nil, fmt.Errorf("store updated translation variant in CAS: %w", err)
	}
	tVar.CASHash = tObj.SHA256
	result.TranslationVariantCAS = tObj.SHA256

	if err := s.db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             tVar.ID,
		AssetID:        in.AssetID,
		RunID:          in.RunID,
		JobID:          in.JobID,
		TargetLanguage: in.TargetLanguage,
		CASHash:        tObj.SHA256,
		ProvenanceHash: tVar.ProvenanceHash,
		ProviderID:     tVar.ProviderID,
		ModelName:      tVar.ModelName,
		ModelVersion:   tVar.ModelVersion,
		OverallQAScore: tVar.OverallQAScore,
		CreatedAt:      tVar.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("save translation variant index: %w", err)
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "translation", tObj.SHA256); err != nil {
		return nil, err
	}

	// 2. Update DubScriptVariant (spoken adaptation rerun)
	dubIn := domain.DubScriptJobInput{
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		SourceLanguage:        tVar.SourceLanguage,
		TargetLanguage:        in.TargetLanguage,
		TranslationVariantCAS: tObj.SHA256,
	}
	dsVar, err := s.translationSvc.AdaptDubScript(ctx, dubIn)
	if err != nil {
		return nil, fmt.Errorf("adapt spoken script rerun failed: %w", err)
	}
	if dsVar == nil {
		return nil, errors.New("adapt spoken script produced nil dub script variant")
	}
	if in.SpokenTextOverride != "" {
		if pos := slices.IndexFunc(dsVar.Segments, func(s domain.DubScriptSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
			seg := &dsVar.Segments[pos]
			seg.SpokenText = in.SpokenTextOverride
			spQa := qaGate.ValidateSegment(seg.SourceText, in.SpokenTextOverride, tVar.SourceLanguage, in.TargetLanguage, glossaryForSource(tVar.EffectiveGlossary, seg.SourceText))
			seg.PassedQAGate = spQa.Passed
			seg.QAConfidence = spQa.Confidence
			seg.KeyFacts = spQa.ExtractedFacts
			seg.NegationPolarity = spQa.NegationPolarity
			estMs := provider.EstimateSpokenDurationMs(in.SpokenTextOverride, in.TargetLanguage)
			seg.EstimatedDurationMs = estMs
			seg.RequiresReview = false
			seg.ReviewReason = ""
			if !spQa.Passed {
				seg.RequiresReview = true
				seg.ReviewReason = domain.ReviewReasonMeaningCorrupted
			}
			if seg.SlotDurationMs > 0 && estMs > seg.SlotDurationMs {
				seg.RequiresReview = true
				if seg.ReviewReason == "" {
					seg.ReviewReason = "DURATION_OVERRUN"
				}
			}
			dsBytes, err := json.MarshalIndent(dsVar, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshal overridden dub script: %w", err)
			}
			dsObj, err := s.cas.Put(bytes.NewReader(dsBytes))
			if err != nil {
				return nil, fmt.Errorf("store overridden dub script in CAS: %w", err)
			}
			dsVar.CASHash = dsObj.SHA256
			if err := s.db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
				ID:             dsVar.ID,
				AssetID:        in.AssetID,
				RunID:          in.RunID,
				JobID:          in.JobID,
				TargetLanguage: in.TargetLanguage,
				CASHash:        dsObj.SHA256,
				ProvenanceHash: dsVar.ProvenanceHash,
				ProviderID:     dsVar.ProviderID,
				ModelName:      dsVar.ModelName,
				ModelVersion:   dsVar.ModelVersion,
				OverallQAScore: dsVar.OverallQAScore,
				CreatedAt:      dsVar.CreatedAt,
			}); err != nil {
				return nil, fmt.Errorf("save overridden dub script index: %w", err)
			}
		}
	}
	result.DubScriptVariantCAS = dsVar.CASHash
	if err := s.recordCorrectionStage(ctx, in.RunID, "dub_script", dsVar.CASHash); err != nil {
		return nil, err
	}

	// 3. Re-freeze the chosen voices against the corrected DubScript/Transcript lineage,
	// then rerun TTS & DubSegments synthesis. A corrected script must not keep pointing
	// at a VoiceAssignment frozen for the pre-correction script: the mixer validates the
	// exact pinned lineage and will (correctly) refuse that stale combination.
	voiceAssignCAS := ""
	var vaIdx *storage.VoiceAssignmentIndex
	vaIdx, err = s.db.GetVoiceAssignmentIndexByRun(ctx, in.AssetID, requestedRunID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("load voice assignment index: %w", err)
	}

	voiceInput := domain.VoiceAssignmentInput{
		RunID:                 in.RunID,
		AssetID:               in.AssetID,
		JobID:                 in.JobID,
		TargetLanguage:        in.TargetLanguage,
		DubScriptVariantCAS:   result.DubScriptVariantCAS,
		TranscriptArtifactCAS: tVar.TranscriptArtifactCAS,
		ExecutionProfile:      in.ExecutionProfile,
	}
	var priorAssignment *domain.VoiceAssignment
	if vaIdx != nil && strings.TrimSpace(vaIdx.CASHash) != "" {
		vaRC, loadErr := s.cas.Get(vaIdx.CASHash)
		if loadErr != nil {
			return nil, fmt.Errorf("load frozen voice assignment from CAS (%s): %w", vaIdx.CASHash, loadErr)
		}
		var loaded domain.VoiceAssignment
		decodeErr := json.NewDecoder(vaRC).Decode(&loaded)
		vaRC.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode frozen voice assignment (%s): %w", vaIdx.CASHash, decodeErr)
		}
		if loaded.AssetID != "" && loaded.AssetID != in.AssetID {
			return nil, fmt.Errorf("voice assignment artifact %s belongs to asset %s, expected %s", vaIdx.CASHash, loaded.AssetID, in.AssetID)
		}
		if loaded.TargetLanguage != "" && !strings.EqualFold(loaded.TargetLanguage, in.TargetLanguage) {
			return nil, fmt.Errorf("voice assignment artifact %s uses language %s, expected %s", vaIdx.CASHash, loaded.TargetLanguage, in.TargetLanguage)
		}
		loaded.CASHash = vaIdx.CASHash
		priorAssignment = &loaded
		voiceInput.CustomAssignments = loaded.Assignments
		voiceInput.UseSameVoiceForAll = loaded.UseSameVoiceForAll
	}

	var frozenAssignment *domain.VoiceAssignment
	if priorAssignment != nil && priorAssignment.RunID == in.RunID {
		frozenAssignment, err = s.dubbingSvc.ReassignVoice(ctx, voiceInput)
	} else {
		frozenAssignment, err = s.dubbingSvc.AssignVoices(ctx, voiceInput)
	}
	if err != nil {
		return nil, fmt.Errorf("freeze corrected voice assignment lineage: %w", err)
	}
	if frozenAssignment == nil || strings.TrimSpace(frozenAssignment.CASHash) == "" {
		return nil, errors.New("freeze corrected voice assignment lineage produced no CAS artifact")
	}
	voiceAssignCAS = frozenAssignment.CASHash

	dubbingJobIn := domain.DubbingJobInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubScriptVariantCAS:   result.DubScriptVariantCAS,
		VoiceAssignmentCAS:    voiceAssignCAS,
		TranscriptArtifactCAS: tVar.TranscriptArtifactCAS,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubSegsVar, err := s.dubbingSvc.SynthesizeAndFit(ctx, dubbingJobIn)
	if err != nil {
		return nil, fmt.Errorf("synthesize and fit rerun failed: %w", err)
	}
	if dubSegsVar == nil {
		return nil, errors.New("synthesize and fit produced nil dub segments variant")
	}
	result.DubSegmentsVariantCAS = dubSegsVar.CASHash
	// The corrected variant replaces this run's dub_synthesize output: record it, or a resumed run
	// replays the pre-correction segments it started from and the mixer refuses them again (live
	// evidence: run 27a758e6 resumed from `segment 0 measured 13280ms exceeds slot 12400ms` after all
	// three segments had been shortened to fit).
	if err := s.recordCorrectionStage(ctx, in.RunID, "dub_synthesize", dubSegsVar.CASHash); err != nil {
		return nil, err
	}

	// 4. Rerun Audio Mix (dialogue suppression + soundtrack preservation)
	mixIn := AudioMixInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubSegmentsCAS:        result.DubSegmentsVariantCAS,
		PreserveSinging:       true,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubMix, err := s.audioMixSvc.MixAudio(ctx, mixIn)
	if err != nil {
		return nil, fmt.Errorf("audio mix rerun failed: %w", err)
	}
	if dubMix == nil {
		return nil, errors.New("audio mix produced nil dub mix artifact")
	}
	result.DubMixCAS = dubMix.CASHash
	if err := s.recordCorrectionStage(ctx, in.RunID, "audio_mix", dubMix.CASHash); err != nil {
		return nil, err
	}

	// 5. Rerun Visual Text Localized Subtitle / Visual Track, but only once a text region plan exists.
	// A run reaches review before its visual stage ran whenever the mixer refuses a dub that cannot fit
	// its immutable slots: text_detection and visual_text_localize run AFTER audio_mix, so the plan this
	// rerun resolves does not exist yet (live evidence: run 27a758e6, whose three tts_overrun corrections
	// all failed with `text region plan not found: record not found`, leaving the operator no way to
	// shorten the text that the refusal was asking about). The resumed pipeline localizes and freezes the
	// render plan after the mix passes, exactly as a first pass would.
	if _, planErr := s.db.GetTextRegionPlanIndex(ctx, in.AssetID); planErr == nil {
		visIn := LocalizeVisualTrackInput{
			RunID:                 in.RunID,
			JobID:                 in.JobID,
			AssetID:               in.AssetID,
			TargetLanguage:        in.TargetLanguage,
			TranslationVariantCAS: tObj.SHA256,
		}
		visTrack, err := s.visualTextSvc.LocalizeVisualTrack(ctx, visIn)
		if err != nil {
			return nil, fmt.Errorf("localize visual track rerun failed: %w", err)
		}
		if visTrack == nil {
			return nil, errors.New("localize visual track produced nil visual track")
		}
		// Invariant: result.LocalizedSubtitleCAS is the actual LocalizedSubtitleTrack CAS.
		result.LocalizedSubtitleCAS = visTrack.SubtitleTrackCAS
		if err := s.recordCorrectionStage(ctx, in.RunID, "visual_text_localize", visTrack.CASHash); err != nil {
			return nil, err
		}

		// 6. Refreeze RenderPlan: pins explicit new DubMixCAS and newly produced subtitle cues/render inputs
		planIn := RenderPlanInput{
			RunID:          in.RunID,
			JobID:          in.JobID,
			AssetID:        in.AssetID,
			TargetLanguage: in.TargetLanguage,
			DubMixCAS:      result.DubMixCAS,
			SubtitleCues:   visTrack.SubtitleCues,
		}
		rPlan, err := s.renderSvc.FreezeRenderPlan(ctx, planIn)
		if err != nil {
			return nil, fmt.Errorf("freeze render plan rerun failed: %w", err)
		}
		if rPlan == nil {
			return nil, errors.New("freeze render plan produced nil render plan")
		}
		result.RenderPlanCAS = rPlan.CASHash
		if err := s.recordCorrectionStage(ctx, in.RunID, "render_plan", rPlan.CASHash); err != nil {
			return nil, err
		}
		if err := s.invalidateCorrectionStage(ctx, in.RunID, "render_preview"); err != nil {
			return nil, err
		}
	} else if !errors.Is(planErr, storage.ErrNotFound) {
		return nil, fmt.Errorf("load text region plan index: %w", planErr)
	}

	// 7. Check if candidate auto-resolved based on honest evidence across all evaluated stages
	isResolved := true

	// Meaning QA must pass
	if pos := slices.IndexFunc(tVar.Segments, func(s domain.TranslationSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
		if tSeg := tVar.Segments[pos]; !tSeg.PassedQAGate || tSeg.QAConfidence < 0.6 {
			isResolved = false
		}
	}

	// Spoken adaptation QA and timing must pass
	if pos := slices.IndexFunc(dsVar.Segments, func(s domain.DubScriptSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
		if dsSeg := dsVar.Segments[pos]; !dsSeg.PassedQAGate || dsSeg.RequiresReview {
			isResolved = false
		}
	}

	// DubSegments candidate must satisfy the accepted playback-window contract.
	if pos := slices.IndexFunc(dubSegsVar.Segments, func(s domain.DubSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
		seg := dubSegsVar.Segments[pos]
		playbackDurationMs := seg.DubPlaybackEndMs - seg.StartMs
		if seg.RequiresReview || seg.FitDecision != domain.FitActionAccept || playbackDurationMs <= 0 || seg.MeasuredDurationMs > playbackDurationMs {
			isResolved = false
		}
	}
	for _, rev := range dubSegsVar.ReviewSegments {
		if rev.Index == in.SegmentIndex {
			isResolved = false
			break
		}
	}

	if result.DubMixCAS == "" || result.LocalizedSubtitleCAS == "" || result.RenderPlanCAS == "" {
		isResolved = false
	}

	if isResolved {
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = "Target text corrected and targeted rerun produced passing candidate (auto-resolved)"
	} else {
		result.Status = domain.ReviewItemStatusPending
		result.Message = "Target text corrected but candidate still requires review"
	}

	return result, nil
}

// ReassignVoice explicitly changes one or more speakers' frozen voices for a run,
// regenerating exactly that speaker's affected downstream scope (TTS -> DubSegment -> DubMix -> RenderPlan)
// while strictly preserving source-derived extractions (source media, audio stems, transcript alignment, text regions).
// Voice reassignment is strictly run-bound so a correction can never adopt another
// run's latest script, transcript, or downstream lineage.
func (s *ReviewService) ReassignVoice(ctx context.Context, in VoiceReassignCorrectionInput) (*VoiceReassignCorrectionResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	pinnedRunID := strings.TrimSpace(in.RunID)
	if pinnedRunID == "" {
		return nil, errors.New("run_id is required for voice reassignment")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	if s.dubbingSvc == nil {
		return nil, errors.New("dubbing service is required for voice reassign rerun")
	}
	if s.audioMixSvc == nil {
		return nil, errors.New("audio mix service is required for voice reassign rerun")
	}
	if s.renderSvc == nil {
		return nil, errors.New("render service is required for voice reassign rerun")
	}

	// 1. Resolve the exact run-bound dub script variant the reassignment regenerates
	// against. Asset-latest fallback is deliberately prohibited: a voice correction
	// must never adopt another run's script merely because the caller omitted run_id.
	var dubScriptIdx *storage.DubScriptVariantIndex
	var err error
	dubScriptIdx, err = s.db.GetDubScriptVariantIndexByRun(ctx, pinnedRunID)
	if err != nil {
		return nil, fmt.Errorf("load run dub script variant: %w", err)
	}
	if dubScriptIdx == nil || dubScriptIdx.AssetID != in.AssetID || !strings.EqualFold(dubScriptIdx.TargetLanguage, in.TargetLanguage) {
		return nil, fmt.Errorf("dub script variant run binding mismatch for run %s", pinnedRunID)
	}

	// 2. Reassign voice profile(s) for the run against the exact current script/transcript lineage.
	// Historical assignments may predate these pins; a successor used by the current mixer must not.
	transcriptCAS, _ := resolveRunTranscriptCAS(ctx, s.db, in.RunID, in.AssetID, "voice reassignment")
	if strings.TrimSpace(transcriptCAS) == "" {
		dubScript, _, loadErr := s.dubbingSvc.loadDubScriptVariant(ctx, in.AssetID, targetLang, dubScriptIdx.CASHash)
		if loadErr != nil {
			return nil, fmt.Errorf("load exact dub script lineage for voice reassignment: %w", loadErr)
		}
		if strings.TrimSpace(dubScript.TranslationVariantCAS) != "" {
			transcriptCAS, err = s.dubbingSvc.transcriptCASFromTranslationVariant(in.AssetID, targetLang, dubScript.TranslationVariantCAS)
			if err != nil {
				return nil, err
			}
		}
	}
	if strings.TrimSpace(transcriptCAS) == "" {
		return nil, fmt.Errorf("pinned transcript artifact is required for voice reassignment")
	}
	assignIn := domain.VoiceAssignmentInput{
		RunID:                 in.RunID,
		AssetID:               in.AssetID,
		JobID:                 in.JobID,
		TargetLanguage:        in.TargetLanguage,
		CustomAssignments:     in.CustomAssignments,
		UseSameVoiceForAll:    in.UseSameVoiceForAll,
		ExecutionProfile:      in.ExecutionProfile,
		DubScriptVariantCAS:   dubScriptIdx.CASHash,
		TranscriptArtifactCAS: transcriptCAS,
	}
	newAssign, err := s.dubbingSvc.ReassignVoice(ctx, assignIn)
	if err != nil {
		return nil, fmt.Errorf("voice reassignment failed: %w", err)
	}

	result := &VoiceReassignCorrectionResult{
		VoiceAssignmentCAS:  newAssign.CASHash,
		InvalidatedSpeakers: newAssign.InvalidatedSpeakers,
	}

	// 3. Synthesize dub segments (reuses unchanged speakers, regenerates changed speakers)
	synthIn := domain.DubbingJobInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		VoiceAssignmentCAS:    newAssign.CASHash,
		DubScriptVariantCAS:   dubScriptIdx.CASHash,
		TranscriptArtifactCAS: transcriptCAS,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubSegsVar, err := s.dubbingSvc.SynthesizeAndFit(ctx, synthIn)
	if err != nil {
		return nil, fmt.Errorf("dub synthesize rerun failed: %w", err)
	}
	// SynthesizeAndFit may escalate a whole speaker to the duration-controlled lane
	// (Issue #94), which pins the variant to a superseding VoiceAssignment. The
	// correction must report the assignment actually in force, including which speakers
	// that final assignment invalidated, not the one the correction asked for.
	if finalCAS := dubSegsVar.VoiceAssignmentCAS; finalCAS != "" && finalCAS != newAssign.CASHash {
		rc, err := s.cas.Get(finalCAS)
		if err != nil {
			return nil, fmt.Errorf("load final voice assignment from CAS (%s): %w", finalCAS, err)
		}
		var final domain.VoiceAssignment
		if err := json.NewDecoder(rc).Decode(&final); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode final voice assignment (%s): %w", finalCAS, err)
		}
		rc.Close()
		result.VoiceAssignmentCAS = finalCAS
		result.InvalidatedSpeakers = final.InvalidatedSpeakers
	}
	result.DubSegmentsVariantCAS = dubSegsVar.CASHash

	// 4. Audio stem mixing
	mixIn := AudioMixInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubSegmentsCAS:        dubSegsVar.CASHash,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubMix, err := s.audioMixSvc.MixAudio(ctx, mixIn)
	if err != nil {
		return nil, fmt.Errorf("audio mix rerun failed: %w", err)
	}
	result.DubMixCAS = dubMix.CASHash

	// 5. Refreeze RenderPlan. in.RunID is guaranteed populated by step 1 (the
	// legacy route adopts the resolved variant's run), so the visual track must
	// always resolve run-bound: an asset-scoped lookup here would let a newer
	// track from another run bleed its cues into the regenerated plan.
	var cues []domain.SubtitleCue
	var visIdx *storage.LocalizedVisualTrackIndex
	visIdx, err = s.db.GetLocalizedVisualTrackIndexByRun(ctx, in.RunID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("get run localized visual track index: %w", err)
	}
	if visIdx != nil && (visIdx.AssetID != in.AssetID || !strings.EqualFold(visIdx.TargetLanguage, in.TargetLanguage)) {
		return nil, fmt.Errorf("localized visual track run binding mismatch for run %s", in.RunID)
	}
	if visIdx != nil {
		rc, err := s.cas.Get(visIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load localized visual track from CAS (%s): %w", visIdx.CASHash, err)
		}
		var visTrack domain.LocalizedVisualTrack
		if err := json.NewDecoder(rc).Decode(&visTrack); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode localized visual track (%s): %w", visIdx.CASHash, err)
		}
		rc.Close()
		cues = visTrack.SubtitleCues
	}
	planIn := RenderPlanInput{
		RunID:          in.RunID,
		JobID:          in.JobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		DubMixCAS:      dubMix.CASHash,
		SubtitleCues:   cues,
	}
	rPlan, err := s.renderSvc.FreezeRenderPlan(ctx, planIn)
	if err != nil {
		return nil, fmt.Errorf("freeze render plan rerun failed: %w", err)
	}
	result.RenderPlanCAS = rPlan.CASHash
	if err := s.recordCorrectionStage(ctx, in.RunID, "voice_assignment", result.VoiceAssignmentCAS); err != nil {
		return nil, err
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "dub_synthesize", result.DubSegmentsVariantCAS); err != nil {
		return nil, err
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "audio_mix", result.DubMixCAS); err != nil {
		return nil, err
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "render_plan", result.RenderPlanCAS); err != nil {
		return nil, err
	}
	if err := s.invalidateCorrectionStage(ctx, in.RunID, "render_preview"); err != nil {
		return nil, err
	}

	// 6. Evaluate auto-resolution status
	isResolved := dubSegsVar.OverallStatus == "PASS" && len(dubSegsVar.ReviewSegments) == 0 &&
		result.DubMixCAS != "" && result.RenderPlanCAS != ""
	if isResolved {
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = "Voice reassigned and targeted rerun produced passing candidate (auto-resolved)"
	} else {
		result.Status = domain.ReviewItemStatusPending
		result.Message = "Voice reassigned but candidate still requires review"
	}

	return result, nil
}

// regionCorrectionRollbackTimeout bounds the withdrawal of a rejected correction.
// The rollback runs detached from the request context, so it needs a deadline of
// its own: without one, a healthy database that is slow to answer could hold the
// request goroutine (and its transaction) open indefinitely.
const regionCorrectionRollbackTimeout = 15 * time.Second

// CorrectRegionGeometry applies direct-manipulation overrides (reclassify, drag, resize, relabel)
// to text regions and regenerates the visual track and render plan descendants.
func (s *ReviewService) CorrectRegionGeometry(ctx context.Context, in RegionGeometryCorrectionInput) (result *RegionGeometryCorrectionResult, err error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if len(in.Overrides) == 0 {
		return nil, errors.New("at least one region override is required")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	if s.visualTextSvc == nil {
		return nil, errors.New("visual text service is required for region correction rerun")
	}
	if s.renderSvc == nil {
		return nil, errors.New("render service is required for region correction rerun")
	}

	// Snapshot every descendant artifact this correction is able to regenerate BEFORE any state is
	// persisted: the rollback can only withdraw the rows this correction makes current itself if it
	// knows what was current beforehand, and a snapshot failure must therefore abort while the asset
	// is still untouched.
	priorDescendants, err := s.currentDescendantArtifacts(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("snapshot current region correction descendants: %w", err)
	}

	// 1. Update and persist TextRegionPlan in CAS and SQLite
	planIdx, err := s.db.GetTextRegionPlanIndex(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("get text region plan index: %w", err)
	}
	if planIdx == nil {
		return nil, errors.New("text region plan index is nil")
	}
	rc, err := s.cas.Get(planIdx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("load text region plan from CAS: %w", err)
	}
	var origPlan domain.TextRegionPlan
	err = json.NewDecoder(rc).Decode(&origPlan)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("decode text region plan: %w", err)
	}
	origPlan.CASHash = planIdx.CASHash
	origPlan.ProvenanceHash = planIdx.ProvenanceHash

	// Fail closed on geometry the operator asked for but the frame cannot hold. ApplyRegionOverrides
	// clamps (a clamped box is always in-frame and valid, so a clamp cannot be detected afterwards),
	// which is right for the lenient callers it also serves but wrong here: a correction that
	// silently moved the box somewhere else must be an actionable error, not a persisted surprise.
	if err := domain.ValidateRegionOverrideGeometry(&origPlan, in.Overrides); err != nil {
		return nil, err
	}

	updatedPlan, err := ApplyRegionOverrides(&origPlan, in.Overrides)
	if err != nil {
		return nil, fmt.Errorf("apply region overrides: %w", err)
	}
	if updatedPlan == nil {
		return nil, errors.New("updated text region plan is nil")
	}

	// Mint a NEW immutable overridden-plan identity. The overridden plan must NOT reuse the
	// canonical source-derived provenance: SaveTextRegionPlanIndex is UNIQUE on provenance_hash
	// and ON CONFLICT updates cas_hash, so reusing the source provenance would overwrite the
	// source plan's index row. Derive deterministic provenance from stable semantic inputs only
	// (parent/source provenance plus canonical overridden region state) so identical overrides
	// are idempotent, and persist as a distinct row preserving the source plan under its own
	// provenance. Never use timestamps, paths, or run IDs as semantic identity.
	overrideProv, err := domain.ComputeTextRegionPlanOverrideProvenanceHash(updatedPlan.ProvenanceHash, updatedPlan.Regions)
	if err != nil {
		return nil, fmt.Errorf("compute overridden text region plan provenance: %w", err)
	}
	updatedPlan.ID = uuid.NewString()
	updatedPlan.ProvenanceHash = overrideProv
	updatedPlan.CreatedAt = time.Now().UTC()

	data, err := json.Marshal(updatedPlan)
	if err != nil {
		return nil, fmt.Errorf("marshal updated text region plan: %w", err)
	}
	casObj, err := s.cas.Put(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("put updated text region plan to CAS: %w", err)
	}
	updatedPlan.CASHash = casObj.SHA256
	if err := s.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             updatedPlan.ID,
		AssetID:        updatedPlan.AssetID,
		ProviderID:     updatedPlan.ProviderID,
		ModelName:      updatedPlan.ModelName,
		ModelVersion:   updatedPlan.ModelVersion,
		CASHash:        casObj.SHA256,
		ProvenanceHash: updatedPlan.ProvenanceHash,
		CreatedAt:      updatedPlan.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("save updated text region plan index: %w", err)
	}

	// Fail-closed: the overridden plan becomes the asset's current plan only once its declared
	// descendants regenerate and the operator's audit row is recorded. Persisting it first is
	// required (LocalizeVisualTrack resolves the current plan by index) but a rejected edit must
	// not stay silently accepted as the current plan, so any failure after this point withdraws
	// every row this correction made current — the plan and the visual/subtitle/render artifacts
	// its regeneration persisted — leaving the pre-correction state current again.
	defer func() {
		if err == nil {
			return
		}
		// The withdrawal must not depend on the request that failed: a client that
		// disconnects (or a cancelled job) between persistence and the audit row is
		// exactly when the rejected state is most likely to be left current, so the
		// rollback runs on a context detached from that cancellation and bounded by
		// its own deadline instead of running on a context that is already done.
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), regionCorrectionRollbackTimeout)
		defer cancelRollback()
		if updatedPlan.ProvenanceHash != planIdx.ProvenanceHash {
			if derr := s.db.DeleteTextRegionPlanIndex(rollbackCtx, in.AssetID, updatedPlan.ProvenanceHash); derr != nil {
				err = fmt.Errorf("%w (and the rejected plan could not be withdrawn: %v)", err, derr)
			}
		}
		if derr := s.withdrawCorrectionDescendants(rollbackCtx, in, priorDescendants); derr != nil {
			err = fmt.Errorf("%w (and the rejected descendants could not be withdrawn: %v)", err, derr)
		}
	}()

	// 2. Localize visual track with direct manipulation overrides
	visIn := LocalizeVisualTrackInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		InpaintingFallbacks:   in.InpaintingFallbacks,
		SceneProtectedRegions: in.SceneProtectedRegions,
		// Only the regions the operator just changed are validated strictly: a collision on one of
		// them is a rejected edit (and the plan is withdrawn below), while a collision on an
		// untouched region stays a visual_occlusion exception instead of blocking the correction.
		StrictOverlapRegionIDs: overrideRegionIDs(in.Overrides),
	}
	visTrack, err := s.visualTextSvc.LocalizeVisualTrack(ctx, visIn)
	if err != nil {
		return nil, fmt.Errorf("localize visual track with overrides failed: %w", err)
	}

	result = &RegionGeometryCorrectionResult{
		LocalizedVisualTrackCAS: visTrack.CASHash,
		LocalizedSubtitleCAS:    visTrack.SubtitleTrackCAS,
	}

	// 2. Refreeze RenderPlan
	// A valid no-speech/no-dub run still executes AudioMixService and persists a PASS passthrough
	// DubMixArtifact. Therefore a missing DubMix index during region correction is an
	// incomplete/invalid review state and must fail closed explicitly, not be treated as a
	// legitimate no-dub success.
	var dubMixIdx *storage.DubMixArtifactIndex
	if strings.TrimSpace(in.RunID) != "" {
		dubMixIdx, err = s.db.GetDubMixArtifactIndexByRun(ctx, in.RunID)
		if err == nil && dubMixIdx != nil && (dubMixIdx.AssetID != in.AssetID || !strings.EqualFold(dubMixIdx.TargetLanguage, in.TargetLanguage)) {
			return nil, fmt.Errorf("dub mix artifact run binding mismatch for run %s", in.RunID)
		}
	} else {
		dubMixIdx, err = s.db.GetDubMixArtifactIndex(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil {
		return nil, fmt.Errorf("get dub mix artifact index: %w", err)
	}
	if dubMixIdx == nil || dubMixIdx.CASHash == "" {
		return nil, errors.New("dub mix artifact index is required for region correction")
	}
	dubMixCAS := dubMixIdx.CASHash

	planIn := RenderPlanInput{
		RunID:          in.RunID,
		JobID:          in.JobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		DubMixCAS:      dubMixCAS,
		SubtitleCues:   visTrack.SubtitleCues,
	}
	rPlan, err := s.renderSvc.FreezeRenderPlan(ctx, planIn)
	if err != nil {
		return nil, fmt.Errorf("freeze render plan rerun failed: %w", err)
	}
	result.RenderPlanCAS = rPlan.CASHash

	// Render the preview the operator will actually look at from the plan just frozen.
	// The UI resolves "the run's preview" as the latest preview artifact, so a refrozen
	// plan without a new preview leaves the pre-correction geometry on screen after a
	// reported success. Rendering is pinned to this exact plan, and it reuses the
	// already-frozen DubMix/DubSegment audio: no speech, translation, TTS or audio stage
	// runs here.
	previewArt, err := s.renderSvc.RenderPreview(ctx, RenderExecutionInput{
		RunID:          in.RunID,
		JobID:          in.JobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		PlanProvenance: rPlan.ProvenanceHash,
		PlanCAS:        rPlan.CASHash,
	})
	if err != nil {
		return nil, fmt.Errorf("preview render for the corrected plan failed: %w", err)
	}
	result.PreviewRenderCAS = previewArt.CASHash

	// 3. Record the operator's direct-manipulation intent as an append-only audit
	// row, one per corrected region. The before/after geometry and role are fully
	// reconstructible from the immutable overridden TextRegionPlan CAS artifact
	// this correction minted; this row supplies WHO applied WHICH region edit and
	// WHY. An unrecorded correction must not be reported as successful.
	if err := s.recordRegionCorrectionAudit(ctx, in, updatedPlan.Regions); err != nil {
		return nil, err
	}

	// 4. Evaluate resolution status
	isResolved := result.LocalizedVisualTrackCAS != "" && result.LocalizedSubtitleCAS != "" && result.RenderPlanCAS != "" && result.PreviewRenderCAS != ""
	if isResolved {
		targetedMap := make(map[string]bool, len(in.Overrides))
		for _, ov := range in.Overrides {
			targetedMap[strings.TrimSpace(ov.RegionID)] = true
		}
		for _, reg := range updatedPlan.Regions {
			if targetedMap[reg.ID] {
				if reg.ReviewRequired || reg.Role == domain.TextRoleUncertain {
					isResolved = false
					break
				}
			}
		}
	}
	if isResolved {
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = "Region geometry/classification updated and visual track regenerated (auto-resolved)"
	} else {
		result.Status = domain.ReviewItemStatusPending
		result.Message = "Region updated but candidate still requires review"
	}
	return result, nil
}

// regionCorrectionDescendants names the artifact a region correction is about to regenerate, by the
// provenance hash that is currently latest for it. Empty means nothing is current yet.
type regionCorrectionDescendants struct {
	visual   string
	subtitle string
	render   string
	preview  string
}

// currentDescendantArtifacts resolves the visual/subtitle/render/preview artifacts current for the
// scope a region correction regenerates: run-scoped when the caller supplied a run, asset-latest
// for the legacy asset-scoped route, mirroring how the correction's descendants resolve it
// themselves.
//
// A missing artifact is a valid empty snapshot. Every other storage error is returned: a read
// failure silently treated as "nothing is current" would skip part of a rollback (or let a
// correction proceed on state it could not describe), which is the opposite of failing closed.
func (s *ReviewService) currentDescendantArtifacts(ctx context.Context, in RegionGeometryCorrectionInput) (regionCorrectionDescendants, error) {
	var snap regionCorrectionDescendants
	if runID := strings.TrimSpace(in.RunID); runID != "" {
		visual, err := s.db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return snap, fmt.Errorf("read current localized visual track: %w", err)
		}
		if visual != nil {
			snap.visual = visual.ProvenanceHash
		}
		subtitle, err := s.db.GetLocalizedSubtitleTrackIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return snap, fmt.Errorf("read current localized subtitle track: %w", err)
		}
		if subtitle != nil {
			snap.subtitle = subtitle.ProvenanceHash
		}
		render, err := s.db.GetRenderPlanIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return snap, fmt.Errorf("read current render plan: %w", err)
		}
		if render != nil {
			snap.render = render.ProvenanceHash
		}
		preview, err := s.db.GetRenderArtifactIndicesByRun(ctx, runID)
		if err != nil {
			return snap, fmt.Errorf("read current preview render: %w", err)
		}
		for _, idx := range preview {
			if idx.Kind == domain.RenderKindPreview && strings.EqualFold(idx.TargetLanguage, in.TargetLanguage) {
				snap.preview = idx.ProvenanceHash
			}
		}
		return snap, nil
	}

	visual, err := s.db.GetLocalizedVisualTrackIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current localized visual track: %w", err)
	}
	if visual != nil {
		snap.visual = visual.ProvenanceHash
	}
	subtitle, err := s.db.GetLocalizedSubtitleTrackIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current localized subtitle track: %w", err)
	}
	if subtitle != nil {
		snap.subtitle = subtitle.ProvenanceHash
	}
	render, err := s.db.GetRenderPlanIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current render plan: %w", err)
	}
	if render != nil {
		snap.render = render.ProvenanceHash
	}
	preview, err := s.db.GetLatestRenderArtifactIndex(ctx, in.AssetID, in.TargetLanguage, domain.RenderKindPreview)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current preview render: %w", err)
	}
	if preview != nil {
		snap.preview = preview.ProvenanceHash
	}
	return snap, nil
}

// withdrawCorrectionDescendants takes back the descendant rows a failed region correction made
// current. A changed provenance means this correction inserted that row (regeneration derives
// provenance from the overridden plan, so it cannot collide with the artifact it replaced); an
// unchanged provenance means the correction upserted a row that already existed, whose content and
// position must survive the failure untouched.
func (s *ReviewService) withdrawCorrectionDescendants(ctx context.Context, in RegionGeometryCorrectionInput, prior regionCorrectionDescendants) error {
	now, err := s.currentDescendantArtifacts(ctx, in)
	if err != nil {
		// Without a trustworthy read the rollback cannot tell which rows this correction made
		// current, so it must report that instead of silently leaving them in place.
		return fmt.Errorf("read current descendants for rollback: %w", err)
	}
	var failures []string
	if now.visual != "" && now.visual != prior.visual {
		if err := s.db.DeleteLocalizedVisualTrackIndex(ctx, now.visual); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if now.subtitle != "" && now.subtitle != prior.subtitle {
		if err := s.db.DeleteLocalizedSubtitleTrackIndex(ctx, now.subtitle); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if now.render != "" && now.render != prior.render {
		if err := s.db.DeleteRenderPlanIndex(ctx, now.render); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if now.preview != "" && now.preview != prior.preview {
		if err := s.db.DeleteRenderArtifactIndex(ctx, now.preview); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// recordRegionCorrectionAudit persists one append-only ReviewOverride row per applied
// region override, naming the corrected region and the operator intent that produced it.
// The row is region-scoped (no ReviewItemID): a correction is not an acceptance of a
// projected exception item, so it must never flip a review item to manual_override.
//
// The batch is committed in one transaction: the audit rows are the correction's commit
// point, so a partially recorded batch would leave an audit trail of an edit the caller
// then reports as failed — and the rollback has nothing to withdraw it by.
func (s *ReviewService) recordRegionCorrectionAudit(ctx context.Context, in RegionGeometryCorrectionInput, regions []domain.TrackedTextRegion) error {
	targeted := make(map[string]bool, len(in.Overrides))
	for _, ov := range in.Overrides {
		targeted[strings.TrimSpace(ov.RegionID)] = true
	}

	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "operator"
	}

	rows := make([]domain.ReviewOverride, 0, len(regions))
	for i, reg := range regions {
		if !targeted[reg.ID] {
			continue
		}
		rows = append(rows, domain.ReviewOverride{
			RunID:          in.RunID,
			JobID:          in.JobID,
			AssetID:        in.AssetID,
			TargetLanguage: in.TargetLanguage,
			ItemType:       domain.ReviewItemTypeRegionGeometry,
			Stage:          "detect_text",
			ItemIndex:      i,
			RegionID:       reg.ID,
			Action:         string(domain.ReviewOverrideActionRegionGeometry),
			Reason:         in.Reason,
			Operator:       operator,
			CreatedAt:      time.Now().UTC(),
		})
	}
	if err := s.db.SaveReviewOverrides(ctx, rows); err != nil {
		return fmt.Errorf("persist region correction audit: %w", err)
	}
	return nil
}

// EvaluateFinalRenderHandoff evaluates whether the exception queue is zero, and if so,
// executes or exposes final render handoff based on the review posture (Auto vs Review).
//
// Invariants (locked by Issue #14 DECISION.md, #18, #42):
// - Auto mode: queue reaches zero -> final render proceeds automatically.
// - Review mode: queue zero exposes an explicit 'Start final render' action.
// - If pending exceptions remain (queue > 0), final render is blocked.
func (s *ReviewService) EvaluateFinalRenderHandoff(ctx context.Context, in domain.FinalRenderHandoffInput) (*domain.FinalRenderHandoffResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	posture := in.Posture
	if posture == "" {
		posture = domain.ReviewPostureAuto
	}
	if posture != domain.ReviewPostureAuto && posture != domain.ReviewPostureReview {
		return nil, fmt.Errorf("invalid review posture '%s': must be 'auto' or 'review'", in.Posture)
	}
	in.Posture = posture

	// 1. Check pending review exceptions for the concrete run when RuntimeHost
	// supplied one. The legacy asset-level projection remains for callers that
	// predate run-aware handoff.
	var pendingItems []domain.ReviewItem
	var err error
	if strings.TrimSpace(in.RunID) != "" {
		pendingItems, err = s.ProjectReviewItemsForRun(ctx, in.AssetID, in.TargetLanguage, in.RunID)
	} else {
		pendingItems, err = s.ProjectReviewItems(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil {
		return nil, fmt.Errorf("query review items for final render handoff: %w", err)
	}

	if len(pendingItems) > 0 {
		return &domain.FinalRenderHandoffResult{
			AssetID:             in.AssetID,
			RunID:               in.RunID,
			JobID:               in.JobID,
			TargetLanguage:      in.TargetLanguage,
			Posture:             in.Posture,
			QueueZero:           false,
			PendingReviewCount:  len(pendingItems),
			CanStartFinalRender: false,
			AutoRenderStarted:   false,
			Action:              "review_required",
			Message:             fmt.Sprintf("Final render blocked: %d pending review exceptions require resolution or manual override", len(pendingItems)),
		}, nil
	}

	// 2. Queue is zero
	result := &domain.FinalRenderHandoffResult{
		AssetID:             in.AssetID,
		RunID:               in.RunID,
		JobID:               in.JobID,
		TargetLanguage:      in.TargetLanguage,
		Posture:             in.Posture,
		QueueZero:           true,
		PendingReviewCount:  0,
		CanStartFinalRender: true,
	}

	if in.Posture == domain.ReviewPostureAuto {
		result.AutoRenderStarted = true
		result.Action = "auto_render_started"
		if s.renderSvc != nil {
			execIn := RenderExecutionInput{
				RunID:          in.RunID,
				JobID:          in.JobID,
				AssetID:        in.AssetID,
				TargetLanguage: in.TargetLanguage,
				FontFile:       in.FontFile,
			}
			art, err := s.renderSvc.RenderFinal(ctx, execIn)
			if err != nil {
				return nil, fmt.Errorf("auto final render execution failed: %w", err)
			}
			result.FinalRenderCAS = art.CASHash
			result.Message = "Queue reached zero in Auto mode: final render executed automatically"
		} else {
			result.Message = "Queue reached zero in Auto mode: final render ready"
		}
	} else {
		// Review posture: explicit Start final render action exposed
		result.AutoRenderStarted = false
		result.Action = "start_final_render"
		result.Message = "Queue reached zero in Review mode: explicit 'Start final render' action is available"
	}

	return result, nil
}

// ProjectReviewItems collects and projects all actionable review exceptions for a given asset and target language.
// Invariant: Exception-only review — passing, auto-resolved, or manually-overridden items do not surface in the pending review queue.
func (s *ReviewService) ProjectReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
	return s.projectReviewItems(ctx, assetID, targetLang, "")
}

// ProjectReviewItemsForRun projects actionable exceptions against the artifacts
// frozen for one concrete run. Asset-scoped source analysis (audio roles/text
// regions) remains shared, while run-produced artifacts are resolved by run ID.
func (s *ReviewService) ProjectReviewItemsForRun(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run_id is required")
	}
	return s.projectReviewItems(ctx, assetID, targetLang, runID)
}

func (s *ReviewService) projectReviewItems(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	allItems, err := s.projectAllReviewItems(ctx, assetID, targetLang, runID)
	if err != nil {
		return nil, err
	}

	var pendingItems []domain.ReviewItem
	for _, it := range allItems {
		if it.Status == domain.ReviewItemStatusPending {
			pendingItems = append(pendingItems, it)
		}
	}
	return pendingItems, nil
}

// ProjectAllReviewItems collects all review exceptions along with their resolution status (pending, auto_pass, auto_resolved, manual_override).
// runBoundArtifactCAS resolves the variant artifact a run bound when the run owns no index row of its
// own. A run whose stages were all cache hits consumes artifacts produced by an older run, so the
// run-scoped lookups find nothing and the review queue would silently stay empty for that run - hiding the
// dub overruns that block the mix. The run's own stage execution records the artifact it consumed, so the
// projection reads the variant through it, still pinned to this asset and language.
func (s *ReviewService) runBoundArtifactCAS(ctx context.Context, runID, stage string) (string, error) {
	casHash, err := s.db.GetStageArtifactHash(ctx, runID, stage)
	if err != nil {
		return "", fmt.Errorf("load run-bound %s artifact for run %s: %w", stage, runID, err)
	}
	return casHash, nil
}

func (s *ReviewService) ProjectAllReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
	return s.projectAllReviewItems(ctx, assetID, targetLang, "")
}

// ProjectAllReviewItemsForRun is the audit-inclusive run-scoped projection.
func (s *ReviewService) ProjectAllReviewItemsForRun(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run_id is required")
	}
	return s.projectAllReviewItems(ctx, assetID, targetLang, runID)
}

func (s *ReviewService) projectAllReviewItems(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	if strings.TrimSpace(assetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if targetLang == "" {
		targetLang = "vi"
	}

	var items []domain.ReviewItem

	// 1. Check AudioRolePlan for uncertain audio roles
	plan, err := s.db.GetAudioRolePlan(ctx, assetID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query audio role plan: %w", err)
	}
	if plan != nil {
		for i, seg := range plan.Segments {
			if seg.Role == domain.AudioRoleUncertain {
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-audio-role-%s-%d-%d", assetID, seg.StartMs, seg.EndMs),
					AssetID:        assetID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeAudioRole,
					Stage:          "audio_role_plan",
					ItemIndex:      i,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         "uncertain_audio_role",
					Status:         domain.ReviewItemStatusPending,
					CreatedAt:      plan.CreatedAt,
				})
			}
		}
	}

	// 2. Check TextRegionPlan for low confidence or uncertain text regions
	textIdx, err := s.db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query text region plan index: %w", err)
	}
	if textIdx != nil {
		if textIdx.CASHash == "" {
			return nil, fmt.Errorf("text region plan index has empty CAS hash")
		}
		rc, err := s.cas.Get(textIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load text region plan from CAS (%s): %w", textIdx.CASHash, err)
		}
		var plan domain.TextRegionPlan
		if err := json.NewDecoder(rc).Decode(&plan); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode text region plan (%s): %w", textIdx.CASHash, err)
		}
		rc.Close()
		createdAt := plan.CreatedAt
		if createdAt.IsZero() {
			createdAt = textIdx.CreatedAt
		}
		for i, reg := range plan.Regions {
			if reg.ReviewRequired || reg.Role == domain.TextRoleUncertain {
				itemType := domain.ReviewItemTypeLowConfidenceOCR
				if reg.Role == domain.TextRoleUncertain {
					itemType = domain.ReviewItemTypeUncertainRole
				}
				reason := reg.ReviewReason
				if reason == "" {
					reason = "text_region_requires_review"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-text-%s-%s", textIdx.CASHash, reg.ID),
					AssetID:        assetID,
					TargetLanguage: targetLang,
					Type:           itemType,
					Stage:          "detect_text",
					ItemIndex:      i,
					RegionID:       reg.ID,
					StartMs:        reg.FirstSeenMs,
					EndMs:          reg.LastSeenMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"text": reg.Text,
						"role": string(reg.Role),
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 3. Check the LocalizedVisualTrack for overlays skipped because they could not clear
	// a protected UI box. The overlay is absent from the track, so the region would ship
	// with its source text untouched unless the operator moves, reclassifies, or accepts it.
	var visIdx *storage.LocalizedVisualTrackIndex
	if runID != "" {
		visIdx, err = s.db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "visual_text_localize")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				visIdx = &storage.LocalizedVisualTrackIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
	} else {
		visIdx, err = s.db.GetLocalizedVisualTrackIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query localized visual track index: %w", err)
	}
	if visIdx != nil && visIdx.CASHash != "" {
		rc, err := s.cas.Get(visIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load localized visual track from CAS (%s): %w", visIdx.CASHash, err)
		}
		var vis domain.LocalizedVisualTrack
		decodeErr := json.NewDecoder(rc).Decode(&vis)
		rc.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode localized visual track (%s): %w", visIdx.CASHash, decodeErr)
		}
		if vis.AssetID != assetID || !strings.EqualFold(vis.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("localized visual track ownership mismatch for asset %q target %q", assetID, targetLang)
		}
		createdAt := vis.CreatedAt
		if createdAt.IsZero() {
			createdAt = visIdx.CreatedAt
		}
		for _, occ := range vis.Occlusions {
			items = append(items, domain.ReviewItem{
				ID:             fmt.Sprintf("rev-visual-occlusion-%s-%s", visIdx.CASHash, occ.RegionID),
				AssetID:        assetID,
				TargetLanguage: targetLang,
				Type:           domain.ReviewItemTypeVisualOcclusion,
				Stage:          "visual_text_localize",
				RegionID:       occ.RegionID,
				StartMs:        occ.StartMs,
				EndMs:          occ.EndMs,
				Severity:       "warning",
				Reason:         "overlay_occludes_protected_region",
				Details: map[string]any{
					"role":          string(occ.Role),
					"source_text":   occ.SourceText,
					"overlay_box":   occ.OverlayBox,
					"protected_box": occ.ProtectedBox,
				},
				Status:    domain.ReviewItemStatusPending,
				CreatedAt: createdAt,
			})
		}
	}

	// 4. Check TranslationVariant for meaning QA failures
	var transIdx *storage.TranslationVariantIndex
	if runID != "" {
		transIdx, err = s.db.GetTranslationVariantIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "translation")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				transIdx = &storage.TranslationVariantIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
		if err == nil && (transIdx.AssetID != assetID || !strings.EqualFold(transIdx.TargetLanguage, targetLang)) {
			return nil, fmt.Errorf("translation variant run binding mismatch for run %s", runID)
		}
	} else {
		transIdx, err = s.db.GetTranslationVariantIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query translation variant index: %w", err)
	}
	if transIdx != nil {
		if transIdx.CASHash == "" {
			return nil, fmt.Errorf("translation variant index has empty CAS hash")
		}
		rc, err := s.cas.Get(transIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load translation variant from CAS (%s): %w", transIdx.CASHash, err)
		}
		var tVar domain.TranslationVariant
		if err := json.NewDecoder(rc).Decode(&tVar); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode translation variant (%s): %w", transIdx.CASHash, err)
		}
		rc.Close()
		if tVar.AssetID != assetID || !strings.EqualFold(tVar.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("%w: translation variant ownership mismatch for asset %q target %q", domain.ErrMeaningPreservationFailed, assetID, targetLang)
		}
		createdAt := tVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = transIdx.CreatedAt
		}
		for _, seg := range tVar.Segments {
			if !seg.PassedQAGate || seg.QAConfidence < 0.6 {
				reason := seg.ReviewReason
				if reason == "" {
					reason = "low_meaning_confidence"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-trans-%s-%d", transIdx.CASHash, seg.Index),
					RunID:          tVar.RunID,
					AssetID:        assetID,
					JobID:          tVar.JobID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeTranslationQA,
					Stage:          "translation",
					ItemIndex:      seg.Index,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"source_text":   seg.SourceText,
						"target_text":   seg.TargetText,
						"qa_confidence": seg.QAConfidence,
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 5. Check DubSegmentsVariant for TTS overruns / unselected review items
	var hasDubSegments bool
	var dubSegIdx *storage.DubSegmentsVariantIndex
	if runID != "" {
		dubSegIdx, err = s.db.GetDubSegmentsVariantIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "dub_synthesize")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				dubSegIdx = &storage.DubSegmentsVariantIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
		if err == nil && (dubSegIdx.AssetID != assetID || !strings.EqualFold(dubSegIdx.TargetLanguage, targetLang)) {
			return nil, fmt.Errorf("dub segments variant run binding mismatch for run %s", runID)
		}
	} else {
		dubSegIdx, err = s.db.GetDubSegmentsVariantIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query dub segments variant index: %w", err)
	}
	if dubSegIdx != nil {
		if dubSegIdx.CASHash == "" {
			return nil, fmt.Errorf("dub segments variant index has empty CAS hash")
		}
		rc, err := s.cas.Get(dubSegIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load dub segments variant from CAS (%s): %w", dubSegIdx.CASHash, err)
		}
		var dsVar domain.DubSegmentsVariant
		if err := json.NewDecoder(rc).Decode(&dsVar); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode dub segments variant (%s): %w", dubSegIdx.CASHash, err)
		}
		rc.Close()
		if dsVar.AssetID != assetID || !strings.EqualFold(dsVar.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("dub segments variant ownership mismatch for asset %q target %q", assetID, targetLang)
		}
		hasDubSegments = true
		createdAt := dsVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = dubSegIdx.CreatedAt
		}
		// ReviewSegments (unselected or unresolvable candidates)
		for _, rev := range dsVar.ReviewSegments {
			reason := rev.ReviewReason
			if reason == "" {
				reason = "tts_unresolvable_overrun"
			}
			items = append(items, domain.ReviewItem{
				ID:             fmt.Sprintf("rev-dubseg-rev-%s-%d", dubSegIdx.CASHash, rev.Index),
				RunID:          dsVar.RunID,
				AssetID:        assetID,
				JobID:          dsVar.JobID,
				TargetLanguage: targetLang,
				Type:           domain.ReviewItemTypeTTSOverrun,
				Stage:          "dub_synthesize",
				ItemIndex:      rev.Index,
				SpeakerID:      rev.SpeakerID,
				StartMs:        rev.StartMs,
				EndMs:          rev.EndMs,
				Severity:       "blocker",
				Reason:         reason,
				Details: map[string]any{
					"measured_duration_ms": rev.MeasuredDurationMs,
					"slot_duration_ms":     rev.SlotDurationMs,
					"fit_decision":         string(rev.FitDecision),
					"attempt_count":        rev.AttemptCount,
				},
				Status:    domain.ReviewItemStatusPending,
				CreatedAt: createdAt,
			})
		}
		// Segments that flagged RequiresReview
		for _, seg := range dsVar.Segments {
			if seg.RequiresReview || seg.FitDecision == domain.FitActionReview {
				reason := seg.ReviewReason
				if reason == "" {
					reason = "tts_segment_requires_review"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-dubseg-seg-%s-%d", dubSegIdx.CASHash, seg.Index),
					RunID:          dsVar.RunID,
					AssetID:        assetID,
					JobID:          dsVar.JobID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeTTSOverrun,
					Stage:          "dub_synthesize",
					ItemIndex:      seg.Index,
					SpeakerID:      seg.SpeakerID,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"measured_duration_ms": seg.MeasuredDurationMs,
						"slot_duration_ms":     seg.SlotDurationMs,
						"fit_decision":         string(seg.FitDecision),
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 6. Check DubScriptVariant for spoken adaptation QA failures or unresolved timing flags
	var dubScriptIdx *storage.DubScriptVariantIndex
	if runID != "" {
		dubScriptIdx, err = s.db.GetDubScriptVariantIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "dub_script")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				dubScriptIdx = &storage.DubScriptVariantIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
		if err == nil && (dubScriptIdx.AssetID != assetID || !strings.EqualFold(dubScriptIdx.TargetLanguage, targetLang)) {
			return nil, fmt.Errorf("dub script variant run binding mismatch for run %s", runID)
		}
	} else {
		dubScriptIdx, err = s.db.GetDubScriptVariantIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query dub script variant index: %w", err)
	}
	if dubScriptIdx != nil {
		if dubScriptIdx.CASHash == "" {
			return nil, fmt.Errorf("dub script variant index has empty CAS hash")
		}
		rc, err := s.cas.Get(dubScriptIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load dub script variant from CAS (%s): %w", dubScriptIdx.CASHash, err)
		}
		var dsVar domain.DubScriptVariant
		if err := json.NewDecoder(rc).Decode(&dsVar); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode dub script variant (%s): %w", dubScriptIdx.CASHash, err)
		}
		rc.Close()
		createdAt := dsVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = dubScriptIdx.CreatedAt
		}
		for _, seg := range dsVar.Segments {
			// If !seg.PassedQAGate, always surface as a QA exception.
			// If seg.RequiresReview is set for timing, only surface if DubSegments has not yet resolved it.
			if !seg.PassedQAGate || (!hasDubSegments && seg.RequiresReview) {
				reason := seg.ReviewReason
				if reason == "" {
					reason = "dub_script_qa_flag"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-dubscript-%s-%d", dubScriptIdx.CASHash, seg.Index),
					RunID:          dsVar.RunID,
					AssetID:        assetID,
					JobID:          dsVar.JobID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeDubScriptQA,
					Stage:          "dub_script",
					ItemIndex:      seg.Index,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"source_text": seg.SourceText,
						"spoken_text": seg.SpokenText,
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 7. Check QualityResults for multimodal QC records (both auto_pass records and flagged issues)
	var qualityResults []domain.QualityResult
	if runID != "" {
		qualityResults, err = s.db.GetQualityResultsByRun(ctx, runID)
	} else {
		qualityResults, err = s.db.GetQualityResults(ctx, assetID, targetLang, "")
	}
	if err == nil {
		for _, qr := range qualityResults {
			if runID != "" {
				if qr.RunID != runID || qr.AssetID != assetID || (qr.TargetLanguage != "" && !strings.EqualFold(qr.TargetLanguage, targetLang)) {
					return nil, fmt.Errorf("quality result run binding mismatch for run %s", runID)
				}
			}
			if qr.OverallStatus == domain.QualityStatusPass {
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-quality-pass-%s", qr.ID),
					AssetID:        assetID,
					TargetLanguage: targetLang,
					RunID:          qr.RunID,
					JobID:          qr.JobID,
					Stage:          qr.Stage,
					Type:           domain.ReviewItemType(qr.Stage + "_quality"),
					Severity:       "info",
					Reason:         "automated_quality_gates_passed",
					Status:         domain.ReviewItemStatusAutoPass,
					CreatedAt:      qr.CreatedAt,
				})
			} else if qr.OverallStatus == domain.QualityStatusReviewRequired || qr.OverallStatus == domain.QualityStatusFail {
				for _, iss := range qr.Issues {
					if iss.ID == "" {
						iss.ID = fmt.Sprintf("rev-quality-%s-%s", qr.ID, iss.Stage)
					}
					if iss.AssetID == "" {
						iss.AssetID = assetID
					}
					if iss.TargetLanguage == "" {
						iss.TargetLanguage = targetLang
					}
					if iss.RunID == "" {
						iss.RunID = qr.RunID
					}
					if iss.JobID == "" {
						iss.JobID = qr.JobID
					}
					if iss.Status == "" {
						iss.Status = domain.ReviewItemStatusPending
					}
					if iss.CreatedAt.IsZero() {
						iss.CreatedAt = qr.CreatedAt
					}
					items = append(items, iss)
				}
			}
		}
	}

	// 7. Apply recorded manual overrides
	var overrides []domain.ReviewOverride
	if runID != "" {
		overrides, err = s.db.GetReviewOverridesByRun(ctx, runID)
		if err == nil {
			for _, ro := range overrides {
				if ro.RunID != runID || ro.AssetID != assetID || !strings.EqualFold(ro.TargetLanguage, targetLang) {
					return nil, fmt.Errorf("review override run binding mismatch for run %s", runID)
				}
			}
		}
	} else {
		overrides, err = s.db.GetReviewOverrides(ctx, assetID, targetLang)
	}
	if err == nil && len(overrides) > 0 {
		for i := range items {
			if matchOverride(items[i], overrides) != nil {
				items[i].Status = domain.ReviewItemStatusManualOverride
			}
		}
	}

	return items, nil
}

func matchOverride(it domain.ReviewItem, overrides []domain.ReviewOverride) *domain.ReviewOverride {
	for _, ro := range overrides {
		if ro.ReviewItemID != "" && ro.ReviewItemID == it.ID {
			return &ro
		}
	}
	return nil
}

// overrideRegionIDs returns the region ids a correction actually touched.
func overrideRegionIDs(overrides []domain.RegionOverride) []string {
	ids := make([]string, 0, len(overrides))
	for _, o := range overrides {
		ids = append(ids, o.RegionID)
	}
	return ids
}
