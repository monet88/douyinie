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
	AcquisitionFailureCode  string               `json:"acquisition_failure_code,omitempty"`
	AutomationFailureCode   string               `json:"automation_failure_code,omitempty"`
	LibraryVisible          bool                 `json:"library_visible"`
	RemovedAt               *time.Time           `json:"removed_at,omitempty"`
	CreatedAt               time.Time            `json:"created_at"`
	UpdatedAt               time.Time            `json:"updated_at"`
}
