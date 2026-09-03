package benchmark

import (
	"time"

	"github.com/monet88/douyinie/internal/domain"
)

// ProviderAttemptRef holds immutable execution reference data for a single provider attempt.
type ProviderAttemptRef struct {
	AttemptID         string `json:"attempt_id"`
	ProviderID        string `json:"provider_id"`
	CandidateName     string `json:"candidate_name,omitempty"`
	AttemptNumber     int    `json:"attempt_number"`
	Outcome           string `json:"outcome"` // "SUCCEEDED", "FAILED", "CIRCUIT_OPEN", etc.
	LatencyMs         int64  `json:"latency_ms"`
	ErrorMessage      string `json:"error_message,omitempty"`
	ObservedModel     string `json:"observed_model,omitempty"`
	ServiceBaselineID string `json:"service_baseline_id,omitempty"`
}

// SelectionDecisionRef records provider selection rationale.
type SelectionDecisionRef struct {
	DecisionID         string    `json:"decision_id"`
	Stage              string    `json:"stage"`
	SelectedProviderID string    `json:"selected_provider_id"`
	SelectedCandidate  string    `json:"selected_candidate,omitempty"`
	PolicyCheckResult  string    `json:"policy_check_result,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	Timestamp          time.Time `json:"timestamp"`
}

// ResourceTelemetrySample records device-wide resource measurements.
// Invariant: on Windows WDDM, per-process VRAM is not available; telemetry records
// device-wide baseline, peak, and peak-above-baseline bound to stage timestamps.
type ResourceTelemetrySample struct {
	Stage                        string    `json:"stage"`
	Timestamp                    time.Time `json:"timestamp"`
	DeviceBaselineVRAMBytes      uint64    `json:"device_baseline_vram_bytes"`
	DevicePeakVRAMBytes          uint64    `json:"device_peak_vram_bytes"`
	DevicePeakAboveBaselineBytes uint64    `json:"device_peak_above_baseline_bytes"`
	ElapsedMs                    int64     `json:"elapsed_ms"`
	RTF                          float64   `json:"rtf"` // Real-time factor (elapsed / media_duration)
}

// StageExecutionEvidence records the verifiable execution outcome of a single pipeline stage.
type StageExecutionEvidence struct {
	Stage         string                 `json:"stage"`
	Status        string                 `json:"status"` // "COMPLETED", "FAILED", "SKIPPED"
	ProviderID    string                 `json:"provider_id,omitempty"`
	ModelVersion  string                 `json:"model_version,omitempty"`
	OutputCASHash string                 `json:"output_cas_hash,omitempty"` // Immutable CAS content hash
	Attempts      []ProviderAttemptRef   `json:"attempts,omitempty"`
	Decisions     []SelectionDecisionRef `json:"decisions,omitempty"`
	StartedAt     time.Time              `json:"started_at"`
	CompletedAt   time.Time              `json:"completed_at"`
	DurationMs    int64                  `json:"duration_ms"`
	ErrorMessage  string                 `json:"error_message,omitempty"`
}

// ReviewOverrideRef records human review actions that produce effective release status
// without overwriting automated QualityResult evidence.
type ReviewOverrideRef struct {
	ReviewItemID string    `json:"review_item_id"`
	Action       string    `json:"action"` // "APPROVE", "REJECT", "OVERRIDE"
	OverriddenBy string    `json:"overridden_by"`
	Timestamp    time.Time `json:"timestamp"`
}

// RelationalQCEvidence records references to SQLite relational quality results and review projections.
type RelationalQCEvidence struct {
	QualityResultIDs []string               `json:"quality_result_ids,omitempty"`
	QualityResults   []domain.QualityResult `json:"quality_results,omitempty"`
	ReviewItemIDs    []string               `json:"review_item_ids,omitempty"`
	ReviewItems      []domain.ReviewItem    `json:"review_items,omitempty"`
	ReviewOverrides  []ReviewOverrideRef    `json:"review_overrides,omitempty"`
}

// AcquisitionEntryEvidence captures execution evidence for one URL in the acquisition benchmark.
type AcquisitionEntryEvidence struct {
	EntryID             string                 `json:"entry_id"` // stable aweme_id or canonical URL key
	CanonicalURL        string                 `json:"canonical_url"`
	ExpectedAwemeID     string                 `json:"expected_aweme_id"`
	ObservedAwemeID     string                 `json:"observed_aweme_id,omitempty"`
	SourceAssetID       string                 `json:"source_asset_id,omitempty"`       // Native SourceAsset UUID
	SourceAssetCASHash  string                 `json:"source_asset_cas_hash,omitempty"` // Immutable CAS hash
	IntegrityPassed     bool                   `json:"integrity_passed"`
	ExpectedDurationMs  int64                  `json:"expected_duration_ms"`
	ObservedDurationMs  int64                  `json:"observed_duration_ms"`
	DurationToleranceMs int64                  `json:"duration_tolerance_ms"`
	HTTPStatusCode      int                    `json:"http_status_code"`
	Status              string                 `json:"status"` // "PASS", "FAIL", "CONTENT_UNAVAILABLE", "DISAPPEARED"
	ErrorMessage        string                 `json:"error_message,omitempty"`
	Attempts            []ProviderAttemptRef   `json:"attempts,omitempty"`
	Decisions           []SelectionDecisionRef `json:"decisions,omitempty"`
	Timestamp           time.Time              `json:"timestamp"`
}

// QualityCaseEvidence captures full execution evidence for one quality corpus case
// (Source video x target language x profile).
type QualityCaseEvidence struct {
	CaseID             string                            `json:"case_id"` // e.g. "video_01_vi"
	SourceVideoID      string                            `json:"source_video_id"`
	PrimaryCategory    string                            `json:"primary_category"`
	TargetLanguage     string                            `json:"target_language"`
	Profile            string                            `json:"profile"` // "local", "hybrid"
	JobID              string                            `json:"job_id"`  // Native LocalizationJob UUID
	RunID              string                            `json:"run_id"`  // Native LocalizationRun UUID
	SourceAssetID      string                            `json:"source_asset_id"`
	SourceAssetCASHash string                            `json:"source_asset_cas_hash"`
	Stages             map[string]StageExecutionEvidence `json:"stages"`
	RelationalQC       RelationalQCEvidence              `json:"relational_qc"`
	Decisions          []SelectionDecisionRef            `json:"decisions,omitempty"`
	Telemetry          []ResourceTelemetrySample         `json:"telemetry,omitempty"`
	Status             string                            `json:"status"` // "PENDING", "IN_PROGRESS", "COMPLETED", "FAILED"
	ErrorMessage       string                            `json:"error_message,omitempty"`
	CreatedAt          time.Time                         `json:"created_at"`
	CompletedAt        *time.Time                        `json:"completed_at,omitempty"`
}
