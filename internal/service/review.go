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
	Status                  domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message                 string                  `json:"message"`
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

// ReassignVoice explicitly changes one or more speakers' frozen voices for a run,
// regenerating exactly that speaker's affected downstream scope (TTS -> DubSegment -> DubMix -> RenderPlan)
// while strictly preserving source-derived extractions (source media, audio stems, transcript alignment, text regions).
func (s *ReviewService) ReassignVoice(ctx context.Context, in VoiceReassignCorrectionInput) (*VoiceReassignCorrectionResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if strings.TrimSpace(in.RunID) == "" {
		return nil, errors.New("run_id is required")
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

	// 1. Reassign voice profile(s) for the run
	assignIn := domain.VoiceAssignmentInput{
		RunID:              in.RunID,
		AssetID:            in.AssetID,
		JobID:              in.JobID,
		TargetLanguage:     in.TargetLanguage,
		CustomAssignments:  in.CustomAssignments,
		UseSameVoiceForAll: in.UseSameVoiceForAll,
		ExecutionProfile:   in.ExecutionProfile,
	}
	newAssign, err := s.dubbingSvc.ReassignVoice(ctx, assignIn)
	if err != nil {
		return nil, fmt.Errorf("voice reassignment failed: %w", err)
	}

	result := &VoiceReassignCorrectionResult{
		VoiceAssignmentCAS:  newAssign.CASHash,
		InvalidatedSpeakers: newAssign.InvalidatedSpeakers,
	}

	// 2. Synthesize dub segments (reuses unchanged speakers, regenerates changed speakers)
	dubScriptIdx, err := s.db.GetDubScriptVariantIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil {
		return nil, fmt.Errorf("load latest dub script variant: %w", err)
	}

	synthIn := domain.DubbingJobInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		VoiceAssignmentCAS:    newAssign.CASHash,
		DubScriptVariantCAS:   dubScriptIdx.CASHash,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubSegsVar, err := s.dubbingSvc.SynthesizeAndFit(ctx, synthIn)
	if err != nil {
		return nil, fmt.Errorf("dub synthesize rerun failed: %w", err)
	}
	result.DubSegmentsVariantCAS = dubSegsVar.CASHash

	// 3. Audio stem mixing
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

	// 4. Refreeze RenderPlan
	var cues []domain.SubtitleCue
	visIdx, err := s.db.GetLocalizedVisualTrackIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("get localized visual track index: %w", err)
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

	// 5. Evaluate auto-resolution status
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

// CorrectRegionGeometry applies direct-manipulation overrides (reclassify, drag, resize, relabel)
// to text regions and regenerates the visual track and render plan descendants.
func (s *ReviewService) CorrectRegionGeometry(ctx context.Context, in RegionGeometryCorrectionInput) (*RegionGeometryCorrectionResult, error) {
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

	// 2. Localize visual track with direct manipulation overrides
	visIn := LocalizeVisualTrackInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		InpaintingFallbacks:   in.InpaintingFallbacks,
		SceneProtectedRegions: in.SceneProtectedRegions,
	}
	visTrack, err := s.visualTextSvc.LocalizeVisualTrack(ctx, visIn)
	if err != nil {
		return nil, fmt.Errorf("localize visual track with overrides failed: %w", err)
	}

	result := &RegionGeometryCorrectionResult{
		LocalizedVisualTrackCAS: visTrack.CASHash,
		LocalizedSubtitleCAS:    visTrack.SubtitleTrackCAS,
	}

	// 2. Refreeze RenderPlan
	// A valid no-speech/no-dub run still executes AudioMixService and persists a PASS passthrough
	// DubMixArtifact. Therefore a missing DubMix index during region correction is an
	// incomplete/invalid review state and must fail closed explicitly, not be treated as a
	// legitimate no-dub success.
	dubMixIdx, err := s.db.GetDubMixArtifactIndex(ctx, in.AssetID, in.TargetLanguage)
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

	// 3. Evaluate resolution status
	isResolved := result.LocalizedVisualTrackCAS != "" && result.LocalizedSubtitleCAS != "" && result.RenderPlanCAS != ""
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

	// 1. Check pending review exceptions
	pendingItems, err := s.ProjectReviewItems(ctx, in.AssetID, in.TargetLanguage)
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
