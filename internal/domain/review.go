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
	ReviewItemStatusAutoPass       ReviewItemStatus = "auto_pass"
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
	Status         ReviewItemStatus `json:"status"` // "pending", "auto_pass", "auto_resolved", "manual_override"
	CreatedAt      time.Time        `json:"created_at"`
}

// ReviewOverride records an auditable operator acceptance/override of a flagged exception item.
// Invariant: Append-only audit record. Overriding an item never rewrites historical QA scores to PASS.
type ReviewOverride struct {
	ID             string         `json:"id"`
	RunID          string         `json:"run_id,omitempty"`
	JobID          string         `json:"job_id,omitempty"`
	AssetID        string         `json:"asset_id"`
	TargetLanguage string         `json:"target_language"`
	ReviewItemID   string         `json:"review_item_id"`
	ItemType       ReviewItemType `json:"item_type"`
	Stage          string         `json:"stage"`
	ItemIndex      int            `json:"item_index,omitempty"`
	SegmentID      string         `json:"segment_id,omitempty"`
	RegionID       string         `json:"region_id,omitempty"`
	Action         string         `json:"action"`   // "manual_override"
	Reason         string         `json:"reason"`   // Auditable decision note
	Operator       string         `json:"operator"` // Operator / user ID who confirmed the override
	CreatedAt      time.Time      `json:"created_at"`
}

// QualityStatus categorizes the outcome of multimodal quality evaluation.
type QualityStatus string

const (
	QualityStatusPass           QualityStatus = "PASS"
	QualityStatusReviewRequired QualityStatus = "REVIEW_REQUIRED"
	QualityStatusFail           QualityStatus = "FAIL"
)

// QualityMetric represents a named quality measurement.
type QualityMetric struct {
	Name        string  `json:"name"`
	Score       float64 `json:"score"`
	Threshold   float64 `json:"threshold,omitempty"`
	Passed      bool    `json:"passed"`
	Description string  `json:"description,omitempty"`
}

// QualityResult represents an append-only multimodal audiovisual QC report
// evaluating naturalness, soundtrack preservation, text elimination, and synchronization.
// Invariant: Distinct and decoupled from StageExecution lifecycle state.
// A stage execution may be SUCCEEDED while its QualityResult is REVIEW_REQUIRED or FAIL.
type QualityResult struct {
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	JobID          string          `json:"job_id,omitempty"`
	AssetID        string          `json:"asset_id"`
	TargetLanguage string          `json:"target_language,omitempty"`
	Stage          string          `json:"stage"`          // e.g. "speech_understand", "translation", "dub_script", "dub_synthesize", "audio_mix", "visual_track", "render"
	OverallStatus  QualityStatus   `json:"overall_status"` // PASS | REVIEW_REQUIRED | FAIL
	Metrics        []QualityMetric `json:"metrics,omitempty"`
	Issues         []ReviewItem    `json:"issues,omitempty"`
	Details        map[string]any  `json:"details,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ReviewPosture defines the operator review posture (Auto vs Review mode).
type ReviewPosture string

const (
	ReviewPostureAuto   ReviewPosture = "auto"   // Default: queue reaching zero starts final render automatically
	ReviewPostureReview ReviewPosture = "review" // Review mode: queue reaching zero exposes explicit 'Start final render' action
)

// FinalRenderHandoffInput defines parameters for evaluating final-render handoff readiness.
type FinalRenderHandoffInput struct {
	AssetID        string        `json:"asset_id"`
	RunID          string        `json:"run_id,omitempty"`
	JobID          string        `json:"job_id,omitempty"`
	TargetLanguage string        `json:"target_language"`
	Posture        ReviewPosture `json:"posture"` // "auto" (default) or "review"
	FontFile       string        `json:"font_file,omitempty"`
}

// FinalRenderHandoffResult returns the evaluation of final-render readiness and auto-render execution.
type FinalRenderHandoffResult struct {
	AssetID             string        `json:"asset_id"`
	RunID               string        `json:"run_id,omitempty"`
	JobID               string        `json:"job_id,omitempty"`
	TargetLanguage      string        `json:"target_language"`
	Posture             ReviewPosture `json:"posture"`
	QueueZero           bool          `json:"queue_zero"`
	PendingReviewCount  int           `json:"pending_review_count"`
	CanStartFinalRender bool          `json:"can_start_final_render"`
	AutoRenderStarted   bool          `json:"auto_render_started"`
	Action              string        `json:"action"` // "auto_render_started" | "start_final_render" | "review_required"
	FinalRenderCAS      string        `json:"final_render_cas,omitempty"`
	Message             string        `json:"message"`
}
