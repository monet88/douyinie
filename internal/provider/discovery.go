package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
)

const TypeDouyinDiscovery ProviderType = "douyin_discovery"

// DouyinDiscoveryProvider exposes metadata-only Douyin discovery operations.
// Provider cursors and raw payloads stay inside the adapter boundary.
type DouyinDiscoveryProvider interface {
	Provider
	Search(ctx context.Context, query, continuation string, limit int, authRef string) (*domain.DiscoveryPage, error)
	LookupVideo(ctx context.Context, rawURL, authRef string) (*domain.DiscoveredVideo, error)
	LookupCreator(ctx context.Context, rawURL string, recentLimit int, authRef string) (*domain.CreatorLookup, error)
}
