package domain

import "time"

// ReviewItemType categorizes actionable exceptions surfaced to the operator.
type ReviewItemType string

const (
	ReviewItemTypeTTSOverrun       ReviewItemType = "tts_overrun"
	ReviewItemTypeLowConfidenceOCR ReviewItemType = "low_confidence_ocr"
	ReviewItemTypeUncertainRole    ReviewItemType = "uncertain_role"
	ReviewItemTypeTranslationQA    ReviewItemType = "translation_qa"
	ReviewItemTypeDubScriptQA      ReviewItemType = "dub_script_qa"
	ReviewItemTypeAudioRole        ReviewItemType = "audio_role_uncertain"
	ReviewItemTypeVisualOcclusion  ReviewItemType = "visual_occlusion"
)

// ReviewItemStatus tracks the resolution lifecycle of a review exception.
type ReviewItemStatus string

const (
	ReviewItemStatusPending        ReviewItemStatus = "pending"
	ReviewItemStatusAutoResolved   ReviewItemStatus = "auto_resolved"
	ReviewItemStatusManualOverride ReviewItemStatus = "manual_override"
)

// ReviewItem represents an actionable exception surfaced in the operator review projection
// when automated quality gates flag low confidence, tight timing, or occlusion risks.
type ReviewItem struct {
	ID             string           `json:"id"`
	RunID          string           `json:"run_id,omitempty"`
	AssetID        string           `json:"asset_id"`
	JobID          string           `json:"job_id,omitempty"`
	TargetLanguage string           `json:"target_language,omitempty"`
	Type           ReviewItemType   `json:"type"`
	Stage          string           `json:"stage"` // e.g. "speech_understand", "translation", "dub_script", "dub_synthesize", "visual_track", "audio_mix", "render"
	ItemIndex      int              `json:"item_index,omitempty"`
	SegmentID      string           `json:"segment_id,omitempty"`
	RegionID       string           `json:"region_id,omitempty"`
	SpeakerID      string           `json:"speaker_id,omitempty"`
	StartMs        int64            `json:"start_ms,omitempty"`
	EndMs          int64            `json:"end_ms,omitempty"`
	Severity       string           `json:"severity"` // "warning", "error", "blocker"
	Reason         string           `json:"reason"`
	Details        map[string]any   `json:"details,omitempty"`
	Status         ReviewItemStatus `json:"status"` // "pending", "auto_resolved", "manual_override"
	CreatedAt      time.Time        `json:"created_at"`
}
