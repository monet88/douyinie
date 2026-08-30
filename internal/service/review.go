package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// ReviewService projects actionable exceptions from stage artifacts into structured ReviewItem projections.
// Invariant: Exception-only review — passing or auto-resolved items do not surface in the review queue.
type ReviewService struct {
	db  *storage.DB
	cas *cas.Store
}

// NewReviewService creates a new ReviewService instance.
func NewReviewService(db *storage.DB, casStore *cas.Store) *ReviewService {
	return &ReviewService{
		db:  db,
		cas: casStore,
	}
}

// ProjectReviewItems collects and projects all actionable review exceptions for a given asset and target language.
func (s *ReviewService) ProjectReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
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

	return items, nil
}
