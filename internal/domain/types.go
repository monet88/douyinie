package domain

import (
	"errors"
	"time"
)

var (
	ErrRightsAttestationRequired = errors.New("rights attestation is required before SourceAsset creation")
	ErrAssetNotFound             = errors.New("source asset not found")
	ErrJobNotFound               = errors.New("localization job not found")
	ErrInvalidTargetLanguage     = errors.New("target language must be 'vi' or 'en'")
	ErrCorruptMedia              = errors.New("media container failed integrity verification")
	ErrFingerprintMismatch       = errors.New("computed hash does not match expected CAS fingerprint")
	ErrUncertainRole             = errors.New("uncertain audio role: review required")
)

// RightsAttestation records legal and operator authorization before media asset ingestion.
type RightsAttestation struct {
	ID              string    `json:"id"`
	AttestationType string    `json:"attestation_type"` // e.g. "OWNER_DIRECT", "LICENSED_AUTHORIZED", "FAIR_USE_INTERNAL", "OPERATOR_EXPLICIT_CONFIRMATION"
	DeclaredBy      string    `json:"declared_by"`
	TermsAccepted   bool      `json:"terms_accepted"`
	Notes           string    `json:"notes,omitempty"`
	ConfirmedAt     time.Time `json:"confirmed_at"`
}

// SourceLocator represents the input handle for acquisition.
type SourceLocator struct {
	Type     string `json:"type"` // "local_file", "douyin_url", "web_url"
	Location string `json:"location"`
}

// SourceAsset represents the canonical, normalized source media file in CAS.
type SourceAsset struct {
	ID                  string    `json:"id"`
	SHA256              string    `json:"sha256"`
	ByteSize            int64     `json:"byte_size"`
	MimeType            string    `json:"mime_type"`
	OriginalFilename    string    `json:"original_filename"`
	RightsAttestationID string    `json:"rights_attestation_id"`
	CASPath             string    `json:"cas_path"`
	CreatedAt           time.Time `json:"created_at"`
}

// PreflightReport captures media stream metadata and integrity verification results.
type PreflightReport struct {
	ID                     string    `json:"id"`
	AssetID                string    `json:"asset_id"`
	DurationSec            float64   `json:"duration_sec"`
	DurationMs             int64     `json:"duration_ms"`
	VideoCodec             string    `json:"video_codec,omitempty"`
	AudioCodec             string    `json:"audio_codec,omitempty"`
	Width                  int       `json:"width,omitempty"`
	Height                 int       `json:"height,omitempty"`
	FrameRate              float64   `json:"frame_rate,omitempty"`
	AudioChannels          int       `json:"audio_channels,omitempty"`
	AudioSampleRate        int       `json:"audio_sample_rate,omitempty"`
	AudioBitRate           int64     `json:"audio_bit_rate,omitempty"`
	VideoBitRate           int64     `json:"video_bit_rate,omitempty"`
	ContainerFormat        string    `json:"container_format"`
	ContainerValid         bool      `json:"container_valid"`
	FingerprintMatch       bool      `json:"fingerprint_match"`
	NormalizedAudioSHA256  string    `json:"normalized_audio_sha256,omitempty"`
	NormalizedAudioCASPath string    `json:"normalized_audio_cas_path,omitempty"`
	Errors                 []string  `json:"errors,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
}

// SourceTimeline represents the immutable temporal coordinate system defined by a SourceAsset.
type SourceTimeline struct {
	AssetID         string  `json:"asset_id"`
	DurationMs      int64   `json:"duration_ms"`
	FrameRate       float64 `json:"frame_rate"`
	AudioSampleRate int     `json:"audio_sample_rate"`
	TotalFrames     int64   `json:"total_frames"`
}

// LocalizationJob represents a localization task (SourceAsset × target_language).
type LocalizationJob struct {
	ID             string    `json:"id"`
	SourceAssetID  string    `json:"source_asset_id"`
	TargetLanguage string    `json:"target_language"` // "vi" or "en"
	Status         string    `json:"status"`          // "pending", "running", "completed", "failed", "review_required"
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// LocalizationRun represents an execution attempt of a LocalizationJob.
type LocalizationRun struct {
	ID                 string     `json:"id"`
	JobID              string     `json:"job_id"`
	Status             string     `json:"status"` // "queued", "running", "paused", "cancelled", "completed", "interrupted"
	ConfigSnapshotJSON string     `json:"config_snapshot_json"`
	CreatedAt          time.Time  `json:"created_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

// Run/queue lifecycle statuses (persisted in SQLite queue_entries + localization_runs).
const (
	RunStatusQueued      = "queued"
	RunStatusRunning     = "running"
	RunStatusPaused      = "paused"
	RunStatusCancelled   = "cancelled"
	RunStatusCompleted   = "completed"
	RunStatusInterrupted = "interrupted"
)

// Stage execution statuses (persisted in SQLite stage_executions).
// Active states are QUEUED -> RUNNING -> CANCELLING per locked #13/#16; terminal
// states are SUCCEEDED / FAILED / INTERRUPTED. 'queued' is the not-yet-started
// active state (replacing the former pending-as-queued conflation), and
// 'cancelling' is the mid-cancellation active state before a terminal outcome.
const (
	StageStatusQueued      = "queued"
	StageStatusRunning     = "running"
	StageStatusCancelling  = "cancelling"
	StageStatusSucceeded   = "succeeded"
	StageStatusFailed      = "failed"
	StageStatusInterrupted = "interrupted"
)

// StageStatusActive reports whether a stage status is an active (non-terminal) state.
func StageStatusActive(s string) bool {
	return s == StageStatusQueued || s == StageStatusRunning || s == StageStatusCancelling
}

// QueueEntry is a persisted job ordering record in the runtime queue.
type QueueEntry struct {
	ID         string    `json:"id"`
	RunID      string    `json:"run_id"`
	JobID      string    `json:"job_id"`
	Position   int       `json:"position"`
	Status     string    `json:"status"` // queued | running | paused | cancelled | completed | interrupted
	InsertedAt time.Time `json:"inserted_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// StageExecution records the persisted lifecycle state of a single pipeline stage within a run.
type StageExecution struct {
	ID             string     `json:"id"`
	RunID          string     `json:"run_id"`
	Stage          string     `json:"stage"`  // asr | aligner | tts | separator | ocr | translation | render
	Status         string     `json:"status"` // queued | running | cancelling | succeeded | failed | interrupted
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	ArtifactSHA256 string     `json:"artifact_sha256,omitempty"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// RunStateSnapshot is the crash-recovery payload: a run plus its persisted stage execution state.
type RunStateSnapshot struct {
	ID              string           `json:"id"`
	JobID           string           `json:"job_id"`
	Status          string           `json:"status"`
	StageExecutions []StageExecution `json:"stage_executions"`
	CreatedAt       time.Time        `json:"created_at"`
	CompletedAt     *time.Time       `json:"completed_at,omitempty"`
}

// Valid target languages for Phase 1.
const (
	TargetLanguageVI = "vi"
	TargetLanguageEN = "en"
)

// IsValidTargetLanguage checks if the language is supported in Phase 1.
func IsValidTargetLanguage(lang string) bool {
	return lang == TargetLanguageVI || lang == TargetLanguageEN
}

// AudioRole represents the temporal classification of an audio channel/segment.
type AudioRole string

const (
	AudioRoleNarrationDialogue AudioRole = "narration/dialogue"
	AudioRoleSingingMusicVocal AudioRole = "singing/music-vocal"
	AudioRoleInstrumentalBgm   AudioRole = "instrumental/background"
	AudioRoleAmbienceSFX       AudioRole = "ambience/SFX"
	AudioRoleUncertain         AudioRole = "uncertain"
)

// AudioSegment represents a temporal slice of audio with a classified role.
type AudioSegment struct {
	StartMs int64     `json:"start_ms"`
	EndMs   int64     `json:"end_ms"`
	Role    AudioRole `json:"role"`
}

// AudioRolePlan is the temporal classification plan of source audio.
type AudioRolePlan struct {
	ID             string         `json:"id"`
	AssetID        string         `json:"asset_id"`
	Segments       []AudioSegment `json:"segments"`
	CASHash        string         `json:"cas_hash,omitempty"`
	ProvenanceHash string         `json:"provenance_hash,omitempty"`
	ProviderID     string         `json:"provider_id,omitempty"`
	ModelName      string         `json:"model_name,omitempty"`
	ModelVersion   string         `json:"model_version,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}
