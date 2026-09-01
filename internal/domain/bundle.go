package domain

import (
	"errors"
	"time"
)

var (
	ErrJobBundleTampered          = errors.New("bundle artifact hash mismatch: tampered or corrupted artifact detected")
	ErrJobBundleSecretDetected    = errors.New("fail-closed: bundle contains prohibited secrets or credentials")
	ErrJobBundleMachineLocalPath  = errors.New("fail-closed: bundle manifest contains prohibited machine-local absolute paths")
	ErrJobBundleInvalid           = errors.New("invalid job bundle format or manifest")
	ErrJobBundleArtifactMissing   = errors.New("bundle artifact missing from archive")
	ErrJobBundleLicenseIncomplete = errors.New("bundle manifest missing required license obligation layers (CODE/MODEL/DATA_LICENSE + SERVICE_TERMS)")
	ErrJobAlreadyExists           = errors.New("job already exists on target host (use overwrite: true to replace)")
)

// JobBundleArtifact represents a content-addressed media or data artifact inside the bundle archive.
type JobBundleArtifact struct {
	SHA256      string `json:"sha256"`
	ByteSize    int64  `json:"byte_size"`
	BundlePath  string `json:"bundle_path"` // e.g. "artifacts/<sha256>"
	MimeType    string `json:"mime_type,omitempty"`
	Description string `json:"description,omitempty"`
}

// RenderArtifactIndexData carries render artifact records for export.
type RenderArtifactIndexData struct {
	Preview *PreviewRenderArtifact `json:"preview,omitempty"`
	Final   *FinalRenderArtifact   `json:"final,omitempty"`
}

// JobBundleRunData bundles all execution and artifact state for a single localization run.
type JobBundleRunData struct {
	Run                    LocalizationRun          `json:"run"`
	StageExecutions        []StageExecution         `json:"stage_executions,omitempty"`
	SelectionDecisions     []SelectionDecision      `json:"selection_decisions,omitempty"`
	ProviderAttempts       []ProviderAttempt        `json:"provider_attempts,omitempty"`
	TranscriptArtifact     *TranscriptArtifact      `json:"transcript_artifact,omitempty"`
	TranslationVariant     *TranslationVariant      `json:"translation_variant,omitempty"`
	DubScriptVariant       *DubScriptVariant        `json:"dub_script_variant,omitempty"`
	VoiceAssignment        *VoiceAssignment         `json:"voice_assignment,omitempty"`
	DubSegmentsVariant     *DubSegmentsVariant      `json:"dub_segments_variant,omitempty"`
	DubMixArtifact         *DubMixArtifact          `json:"dub_mix_artifact,omitempty"`
	LocalizedSubtitleTrack *LocalizedSubtitleTrack  `json:"localized_subtitle_track,omitempty"`
	LocalizedVisualTrack   *LocalizedVisualTrack    `json:"localized_visual_track,omitempty"`
	RenderPlan             *RenderPlan              `json:"render_plan,omitempty"`
	RenderArtifact         *RenderArtifactIndexData `json:"render_artifact,omitempty"`
	ReviewOverrides        []ReviewOverride         `json:"review_overrides,omitempty"`
	QualityResults         []QualityResult          `json:"quality_results,omitempty"`
}

// JobBundleManifest is the self-contained metadata descriptor for an exported job.
// It contains NO raw secrets and NO machine-local absolute paths.
type JobBundleManifest struct {
	BundleVersion     int                    `json:"bundle_version"` // e.g. 1
	ExportedAt        time.Time              `json:"exported_at"`
	Job               LocalizationJob        `json:"job"`
	SourceAsset       SourceAsset            `json:"source_asset"`
	RightsAttestation *RightsAttestation     `json:"rights_attestation,omitempty"`
	PreflightReport   *PreflightReport       `json:"preflight_report,omitempty"`
	AudioRolePlan     *AudioRolePlan         `json:"audio_role_plan,omitempty"`
	AudioStems        *AudioStemArtifacts    `json:"audio_stems,omitempty"`
	TextRegionPlans   []TextRegionPlan       `json:"text_region_plans,omitempty"`
	Runs              []JobBundleRunData     `json:"runs,omitempty"`
	LicenseManifests  []LicenseManifestEntry `json:"license_manifests,omitempty"`
	Artifacts         []JobBundleArtifact    `json:"artifacts"`
}
