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
