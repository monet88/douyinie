package domain

import (
	"errors"
	"time"
)

// ErrInvalidDiscoveryRequest marks malformed or unsupported operator input.
// RuntimeHost maps it to HTTP 400 without string-matching provider messages.
var ErrInvalidDiscoveryRequest = errors.New("invalid Douyin discovery request")

// DouyinCreator is the transient normalized identity used by discovery.
// sec_uid is the durable identity; display fields are observation snapshots.
type DouyinCreator struct {
	SecUID       string `json:"sec_uid"`
	CanonicalURL string `json:"canonical_url"`
	DisplayName  string `json:"display_name,omitempty"`
	AvatarURL    string `json:"avatar_url,omitempty"`
}

// DiscoveredVideo is metadata-only discovery output. Pointer fields preserve
// unknown provider values instead of fabricating zeroes.
type DiscoveredVideo struct {
	AwemeID      string         `json:"aweme_id"`
	SourceID     string         `json:"source_id"`
	CanonicalURL string         `json:"canonical_url"`
	Title        string         `json:"title,omitempty"`
	CoverURL     string         `json:"cover_url,omitempty"`
	PublishedAt  *time.Time     `json:"published_at,omitempty"`
	LikeCount    *int64         `json:"like_count,omitempty"`
	DurationMs   *int64         `json:"duration_ms,omitempty"`
	Creator      *DouyinCreator `json:"creator,omitempty"`
}

// DiscoveryPage is one transient fetched page. Continuation is opaque to the
// RuntimeHost and is never persisted.
type DiscoveryPage struct {
	Videos       []DiscoveredVideo `json:"videos"`
	Continuation string            `json:"continuation,omitempty"`
	HasMore      bool              `json:"has_more"`
}

// CreatorLookup is a transient creator snapshot plus a bounded recent view.
type CreatorLookup struct {
	Creator             DouyinCreator     `json:"creator"`
	RecentVideos        []DiscoveredVideo `json:"recent_videos"`
	RecentLimit         int               `json:"recent_limit"`
	RecentViewAvailable bool              `json:"recent_view_available"`
}

type DiscoveryDisposition string

const (
	DiscoveryDispositionNew     DiscoveryDisposition = "new"
	DiscoveryDispositionSeen    DiscoveryDisposition = "seen"
	DiscoveryDispositionIgnored DiscoveryDisposition = "ignored"
)

type DiscoveryOrigin string

const (
	DiscoveryOriginBaseline   DiscoveryOrigin = "baseline"
	DiscoveryOriginHistorical DiscoveryOrigin = "historical"
	DiscoveryOriginMonitoring DiscoveryOrigin = "monitoring"
)

type ChannelAutomationMode string

const (
	ChannelAutomationDetect   ChannelAutomationMode = "A"
	ChannelAutomationDownload ChannelAutomationMode = "B"
	ChannelAutomationLocalize ChannelAutomationMode = "C"
)

// FollowedCreator is the durable operator-owned channel state. Provider
// continuation/cursors intentionally never appear here.
type FollowedCreator struct {
	SecUID               string                `json:"sec_uid"`
	CanonicalURL         string                `json:"canonical_url"`
	DisplayName          string                `json:"display_name,omitempty"`
	AvatarURL            string                `json:"avatar_url,omitempty"`
	Followed             bool                  `json:"followed"`
	FollowedAt           time.Time             `json:"followed_at"`
	BaselineAt           time.Time             `json:"baseline_at"`
	AutomationMode       ChannelAutomationMode `json:"automation_mode"`
	TargetLanguage       string                `json:"target_language"`
	ReviewPosture        string                `json:"review_posture"`
	CoverColor           string                `json:"cover_color"`
	ConfiguredBy         string                `json:"configured_by"`
	RightsAttestationID  string                `json:"rights_attestation_id,omitempty"`
	LastSuccessfulPollAt *time.Time            `json:"last_successful_poll_at,omitempty"`
	LastAttemptAt        *time.Time            `json:"last_attempt_at,omitempty"`
	NextCheckAt          *time.Time            `json:"next_check_at,omitempty"`
	PollState            string                `json:"poll_state"`
	FailureCode          string                `json:"failure_code,omitempty"`
	FailureMessage       string                `json:"failure_message,omitempty"`
	CreatedAt            time.Time             `json:"created_at"`
	UpdatedAt            time.Time             `json:"updated_at"`
}

// DouyinVideo is retained browsing/lifecycle state only. SourceAsset, Job/Run,
// queue and render truth remain in their existing authoritative tables.
type DouyinVideo struct {
	DiscoveredVideo
	SecUID                  string               `json:"sec_uid,omitempty"`
	FirstObservedAt         time.Time            `json:"first_observed_at"`
	LastObservedAt          time.Time            `json:"last_observed_at"`
	Origin                  DiscoveryOrigin      `json:"origin"`
	Disposition             DiscoveryDisposition `json:"disposition"`
	AcquisitionRequestState string               `json:"acquisition_request_state"`
	AcquisitionFailure      *WorkstationFailure  `json:"acquisition_failure,omitempty"`
	AutomationFailureCode   string               `json:"automation_failure_code,omitempty"`
	RightsAttestationID     string               `json:"rights_attestation_id,omitempty"`
	CredentialRef           string               `json:"credential_ref,omitempty"`
	ConsentGranted          bool                 `json:"consent_granted,omitempty"`
	LibraryVisible          bool                 `json:"library_visible"`
	RemovedAt               *time.Time           `json:"removed_at,omitempty"`
	CreatedAt               time.Time            `json:"created_at"`
	UpdatedAt               time.Time            `json:"updated_at"`
}

const (
	AcquisitionRequestNone        = "none"
	AcquisitionRequestQueued      = "queued"
	AcquisitionRequestDownloading = "downloading"
	AcquisitionRequestDownloaded  = "downloaded"
	AcquisitionRequestFailed      = "failed"
)

// WorkstationFailure is safe durable operator-facing failure state. Detail must
// never contain credential/session material.
type WorkstationFailure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	ProviderID string `json:"provider_id,omitempty"`
}

// MediaLibraryEntry projects workstation browsing state onto canonical
// AcquisitionProvenance/SourceAsset truth. It is not a second media aggregate.
type MediaLibraryEntry struct {
	AwemeID          string              `json:"aweme_id"`
	SourceID         string              `json:"source_id"`
	CanonicalURL     string              `json:"canonical_url"`
	Title            string              `json:"title,omitempty"`
	CoverURL         string              `json:"cover_url,omitempty"`
	PublishedAt      *time.Time          `json:"published_at,omitempty"`
	LikeCount        *int64              `json:"like_count,omitempty"`
	DurationMs       *int64              `json:"duration_ms,omitempty"`
	AcquisitionState string              `json:"acquisition_state"`
	Failure          *WorkstationFailure `json:"failure,omitempty"`
	LegacyFallback   bool                `json:"legacy_fallback,omitempty"`
	Asset            *MediaLibraryAsset  `json:"asset,omitempty"`
	Thumbnail        MediaThumbnail      `json:"thumbnail"`
	Production       []ProductionHandoff `json:"production,omitempty"`
}

type MediaLibraryAsset struct {
	ID             string `json:"id"`
	SHA256         string `json:"sha256"`
	ByteSize       int64  `json:"byte_size"`
	MimeType       string `json:"mime_type"`
	LocalAvailable bool   `json:"local_available"`
}

// MediaThumbnail describes a durable local thumbnail recipe. No expiring CDN
// URL is required after a SourceAsset exists; callers can regenerate frame 0
// from the immutable asset.
type MediaThumbnail struct {
	Kind        string `json:"kind"`
	AssetID     string `json:"asset_id,omitempty"`
	URL         string `json:"url,omitempty"`
	FallbackURL string `json:"fallback_url,omitempty"`
}

type ProductionHandoff struct {
	TargetLanguage string `json:"target_language"`
	JobID          string `json:"job_id"`
	JobStatus      string `json:"job_status"`
	RunID          string `json:"run_id,omitempty"`
	RunStatus      string `json:"run_status,omitempty"`
	View           string `json:"view"`
}
