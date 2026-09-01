package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
)

// TypeAcquisition is the provider stage for Douyin source acquisition
// (probe + acquire). Acquisition providers are policy-gated like every other
// stage: a healthy but policy-blocked adapter is never routable.
const TypeAcquisition ProviderType = "acquisition"

// AcquiredMedia is the durable local result of one successful acquire call.
// The file at Path is validated (integrity + fingerprint) by the acquisition
// ladder inside the same candidate step before it becomes a canonical
// SourceAsset; bad bytes fail the candidate and advance the ladder.
type AcquiredMedia struct {
	FilePath      string
	MimeType      string
	Method        string // "api" | "browser" | "external_provider"
	Authenticated bool
}

// AcquisitionProvider is implemented by source adapters (Jiji-based, F2/parser
// fallback, browser-assisted auth last) that can probe a locator into a safe
// descriptor and acquire the media into a destination directory.
//
// Probe must not create durable media assets or download bytes. Acquire may
// return *domain.AcquisitionError to surface a structural acquisition state;
// the Router treats CONTENT_UNAVAILABLE as fail-closed and everything else as
// fallback-eligible so the ladder can advance to the next authorized adapter.
// Session material (cookies/tokens) is resolved from a safe CredentialRef by
// the adapter at call time and must never appear in returned values, logs, or
// provenance.
type AcquisitionProvider interface {
	Provider

	// Probe normalizes the locator (e.g. resolves a Douyin short link to its
	// canonical aweme id) and returns safe descriptor metadata only.
	Probe(ctx context.Context, locator domain.SourceLocator, authRef string) (*domain.SourceDescriptor, error)

	// Acquire downloads the descriptor's media into destDir and returns the
	// local durable file reference. authRef is an opaque CredentialRef id.
	Acquire(ctx context.Context, desc domain.SourceDescriptor, destDir string, authRef string) (*AcquiredMedia, error)
}
