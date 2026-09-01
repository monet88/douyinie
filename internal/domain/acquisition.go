package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrContentUnavailable is a hard acquisition block: the content itself is
// gone or inaccessible for this account. Fail-closed: the acquisition ladder
// must not fall through to another provider or retry past this error.
var ErrContentUnavailable = errors.New("acquisition fail-closed: content unavailable")

// ErrInvalidURL marks unusable client input (missing/malformed locator or
// unsupported locator type). Fail-closed and a 4xx: it is never mislabeled
// as content being gone, and no other provider can fix bad input (Issue #4
// routing rules: INVALID_URL never falls back).
var ErrInvalidURL = errors.New("acquisition invalid url: locator is missing or malformed")

// ErrUnsupportedMediaType marks a source the V1 pipeline refuses before
// expensive work: Douyinie V1 accepts ordinary video posts only, so probe
// classifies gallery/live sources and rejects them distinctly (resolved #4
// media policy). Fail-closed: a different acquisition method cannot turn a
// gallery into an ordinary video.
var ErrUnsupportedMediaType = errors.New("acquisition unsupported media type: V1 accepts ordinary video posts only")

// ErrInconsistentProvenance marks an internal inconsistency during canonical
// reuse resolution (missing required assets, preflight reports, attestation, or
// CAS bytes for a previously recorded acquisition provenance). Fail-closed:
// do not retry or fallback to alternate providers (CODING_STANDARDS §10).
var ErrInconsistentProvenance = errors.New("acquisition fail-closed: inconsistent provenance or missing required asset/artifact state")

// AcquisitionState is the canonical structural acquisition failure class
// surfaced to the operator (Issue #28 / #17 resolution), reconciled with the
// Issue #4 failure model. Each state is distinct so fixable problems are
// separable from hard blocks.
type AcquisitionState string

const (
	AcquisitionAuthRequired         AcquisitionState = "AUTH_REQUIRED"
	AcquisitionSessionExpired       AcquisitionState = "SESSION_EXPIRED"
	AcquisitionCaptchaRequired      AcquisitionState = "CAPTCHA_REQUIRED"
	AcquisitionAntiBotOrEmpty       AcquisitionState = "ANTI_BOT_OR_EMPTY_RESPONSE"
	AcquisitionContentUnavailable   AcquisitionState = "CONTENT_UNAVAILABLE"
	AcquisitionDownloadFailed       AcquisitionState = "DOWNLOAD_FAILED"
	AcquisitionIntegrityFailed      AcquisitionState = "INTEGRITY_FAILED"
	AcquisitionInvalidURL           AcquisitionState = "INVALID_URL"
	AcquisitionUnsupportedMediaType AcquisitionState = "UNSUPPORTED_MEDIA_TYPE"
)

// AcquisitionError carries a structural acquisition state from a provider
// adapter up through the Router to the transport layer. Detail must never
// contain raw cookies, tokens, signed URLs, or machine-local credential
// material — only safe descriptions (Issue #17: session material is local-only).
type AcquisitionError struct {
	State      AcquisitionState
	ProviderID string
	Detail     string
}

func (e *AcquisitionError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: acquisition failed (provider %s)", e.State, e.ProviderID)
	}
	return fmt.Sprintf("%s: %s (provider %s)", e.State, e.Detail, e.ProviderID)
}

// Unwrap exposes the fail-closed sentinel for each hard-block state so the
// Router's classification and the transport mapping can match without string
// parsing: CONTENT_UNAVAILABLE, INVALID_URL, and UNSUPPORTED_MEDIA_TYPE never
// fall through to another provider (Issue #4 routing rules).
func (e *AcquisitionError) Unwrap() error {
	switch e.State {
	case AcquisitionContentUnavailable:
		return ErrContentUnavailable
	case AcquisitionInvalidURL:
		return ErrInvalidURL
	case AcquisitionUnsupportedMediaType:
		return ErrUnsupportedMediaType
	}
	return nil
}

// SourceDescriptor is the safe, normalized result of probing a locator. It
// carries no media bytes and no auth material; probe resolves the canonical
// source identity without creating a durable asset (Issue #17).
type SourceDescriptor struct {
	SourceID     string `json:"source_id"`     // opaque canonical id, e.g. "douyin:aweme:<id>"
	Platform     string `json:"platform"`      // "douyin"
	CanonicalURL string `json:"canonical_url"` // normalized web URL, never an expiring CDN URL
	MediaType    string `json:"media_type"`    // "video" | "gallery" | "live" | "unknown"
	Title        string `json:"title,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
}

// AcquisitionProvenance is the safe, persistable record of how a SourceAsset
// was acquired. Only references and booleans are stored — never cookies,
// tokens, or signed URLs (Issue #17 / CODING_STANDARDS §8).
type AcquisitionProvenance struct {
	ID             string    `json:"id"`
	AssetID        string    `json:"asset_id"`
	SourceID       string    `json:"source_id"`
	Platform       string    `json:"platform"`
	CanonicalURL   string    `json:"canonical_url"`
	Adapter        string    `json:"adapter"`
	AdapterVersion string    `json:"adapter_version"`
	Method         string    `json:"method"` // "api" | "browser" | "external_provider"
	Authenticated  bool      `json:"authenticated"`
	AuthRefID      string    `json:"auth_ref_id,omitempty"` // CredentialRef id, never a secret
	AcquiredAt     time.Time `json:"acquired_at"`
}
