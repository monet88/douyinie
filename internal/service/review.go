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

	// 1. Verify that the requested review item is currently pending for the specified asset and target language.
	pendingItems, err := s.ProjectReviewItems(ctx, in.AssetID, in.TargetLanguage)
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
		Action:         "manual_override",
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
	if in.RunID == "" {
		in.RunID = fmt.Sprintf("corr-run-%d", time.Now().UnixNano())
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
	transIdx, err := s.db.GetTranslationVariantIndex(ctx, in.AssetID, in.TargetLanguage)
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

	if in.SegmentIndex >= len(tVar.Segments) {
		return nil, fmt.Errorf("segment index %d out of bounds (total %d)", in.SegmentIndex, len(tVar.Segments))
	}

	qaGate := s.translationSvc.qaGate
	if qaGate == nil {
		qaGate = NewMeaningFirstQAGate()
	}

	qaRes := qaGate.ValidateSegment(tVar.Segments[in.SegmentIndex].SourceText, in.NewTargetText, tVar.SourceLanguage, in.TargetLanguage)

	tVar.ID = uuid.NewString()
	tVar.RunID = in.RunID
	if in.JobID != "" {
		tVar.JobID = in.JobID
	}
	tVar.Segments[in.SegmentIndex].TargetText = in.NewTargetText
	tVar.Segments[in.SegmentIndex].PassedQAGate = qaRes.Passed
	tVar.Segments[in.SegmentIndex].QAConfidence = qaRes.Confidence
	tVar.Segments[in.SegmentIndex].KeyFacts = qaRes.ExtractedFacts
	tVar.Segments[in.SegmentIndex].NegationPolarity = qaRes.NegationPolarity
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
	if in.SpokenTextOverride != "" && in.SegmentIndex < len(dsVar.Segments) {
		seg := &dsVar.Segments[in.SegmentIndex]
		seg.SpokenText = in.SpokenTextOverride
		spQa := qaGate.ValidateSegment(seg.SourceText, in.SpokenTextOverride, tVar.SourceLanguage, in.TargetLanguage)
		seg.PassedQAGate = spQa.Passed
		seg.QAConfidence = spQa.Confidence
		seg.KeyFacts = spQa.ExtractedFacts
		seg.NegationPolarity = spQa.NegationPolarity
		estMs := provider.EstimateSpokenDurationMs(in.SpokenTextOverride, in.TargetLanguage)
		seg.EstimatedDurationMs = estMs
		if seg.SlotDurationMs > 0 && estMs > seg.SlotDurationMs {
			seg.RequiresReview = true
			seg.ReviewReason = "DURATION_OVERRUN"
		} else {
			seg.RequiresReview = false
			seg.ReviewReason = ""
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
	result.DubScriptVariantCAS = dsVar.CASHash

	// 3. Rerun TTS & DubSegments synthesis
	voiceAssignCAS := ""
	vaIdx, err := s.db.GetVoiceAssignmentIndex(ctx, in.AssetID, in.TargetLanguage)
	if err == nil && vaIdx != nil {
		voiceAssignCAS = vaIdx.CASHash
	}

	dubbingJobIn := domain.DubbingJobInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubScriptVariantCAS:   result.DubScriptVariantCAS,
		VoiceAssignmentCAS:    voiceAssignCAS,
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

	// 5. Rerun Visual Text Localized Subtitle / Visual Track
	visIn := LocalizeVisualTrackInput{
		RunID:          in.RunID,
		JobID:          in.JobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
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

	// 7. Check if candidate auto-resolved based on honest evidence across all evaluated stages
	isResolved := true

	// Meaning QA must pass
	if in.SegmentIndex < len(tVar.Segments) {
		tSeg := tVar.Segments[in.SegmentIndex]
		if !tSeg.PassedQAGate || tSeg.QAConfidence < 0.6 {
			isResolved = false
		}
	}

	// Spoken adaptation QA and timing must pass
	if in.SegmentIndex < len(dsVar.Segments) {
		dsSeg := dsVar.Segments[in.SegmentIndex]
		if !dsSeg.PassedQAGate || dsSeg.RequiresReview {
			isResolved = false
		}
	}

	// DubSegments candidate must fit without overrun
	if in.SegmentIndex < len(dubSegsVar.Segments) {
		seg := dubSegsVar.Segments[in.SegmentIndex]
		if seg.RequiresReview || seg.FitDecision == domain.FitActionReview || (seg.SlotDurationMs > 0 && seg.MeasuredDurationMs > seg.SlotDurationMs) {
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

// ProjectReviewItems collects and projects all actionable review exceptions for a given asset and target language.
// Invariant: Exception-only review — passing, auto-resolved, or manually-overridden items do not surface in the pending review queue.
func (s *ReviewService) ProjectReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
	allItems, err := s.ProjectAllReviewItems(ctx, assetID, targetLang)
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
func (s *ReviewService) ProjectAllReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
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

	// 3. Check TranslationVariant for meaning QA failures
	transIdx, err := s.db.GetTranslationVariantIndex(ctx, assetID, targetLang)
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
		createdAt := tVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = transIdx.CreatedAt
		}
		for _, seg := range tVar.Segments {
			if !seg.PassedQAGate || seg.QAConfidence < 0.6 {
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
					Reason:         "low_meaning_confidence",
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

	// 4. Check DubSegmentsVariant for TTS overruns / unselected review items
	var hasDubSegments bool
	dubSegIdx, err := s.db.GetDubSegmentsVariantIndex(ctx, assetID, targetLang)
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

	// 5. Check DubScriptVariant for spoken adaptation QA failures or unresolved timing flags
	dubScriptIdx, err := s.db.GetDubScriptVariantIndex(ctx, assetID, targetLang)
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

	// 6. Check QualityResults for multimodal QC records (both auto_pass records and flagged issues)
	qualityResults, err := s.db.GetQualityResults(ctx, assetID, targetLang, "")
	if err == nil {
		for _, qr := range qualityResults {
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
	overrides, err := s.db.GetReviewOverrides(ctx, assetID, targetLang)
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
