package provider

import (
	"context"
	"fmt"

	"github.com/monet88/douyinie/internal/domain"
)

// FakeDouyinDiscoveryProvider is deterministic Seam 1 coverage for discovery.
type FakeDouyinDiscoveryProvider struct {
	BaseFakeProvider
	SearchPage *domain.DiscoveryPage
	Video      *domain.DiscoveredVideo
	Creator    *domain.CreatorLookup
	Err        error
	Calls      []string
	SawAuthRef string
}

func NewFakeDouyinDiscoveryProvider(id string, policy domain.PolicyState) *FakeDouyinDiscoveryProvider {
	return &FakeDouyinDiscoveryProvider{BaseFakeProvider: BaseFakeProvider{
		ProviderID: id, ProviderType: TypeDouyinDiscovery, Policy: policy, Healthy: true,
		Cap: domain.ProviderCapability{Stage: string(TypeDouyinDiscovery), ExecutionTier: "hybrid", QualityScore: 1, MaxConcurrency: 1,
			Features: []string{"douyin", "keyword_search", "video_lookup", "creator_lookup"}},
		ModelName: id, ModelVersion: "v1",
	}}
}

func (p *FakeDouyinDiscoveryProvider) Search(_ context.Context, _, _ string, _ int, authRef string) (*domain.DiscoveryPage, error) {
	p.Calls = append(p.Calls, "search")
	p.SawAuthRef = authRef
	if p.Err != nil {
		return nil, p.Err
	}
	if p.SearchPage == nil {
		return &domain.DiscoveryPage{Videos: []domain.DiscoveredVideo{}}, nil
	}
	return p.SearchPage, nil
}

func (p *FakeDouyinDiscoveryProvider) LookupVideo(_ context.Context, _ string, authRef string) (*domain.DiscoveredVideo, error) {
	p.Calls = append(p.Calls, "video")
	p.SawAuthRef = authRef
	if p.Err != nil {
		return nil, p.Err
	}
	if p.Video == nil {
		return nil, fmt.Errorf("fake discovery video not configured")
	}
	return p.Video, nil
}

func (p *FakeDouyinDiscoveryProvider) LookupCreator(_ context.Context, _ string, _ int, authRef string) (*domain.CreatorLookup, error) {
	p.Calls = append(p.Calls, "creator")
	p.SawAuthRef = authRef
	if p.Err != nil {
		return nil, p.Err
	}
	if p.Creator == nil {
		return nil, fmt.Errorf("fake discovery creator not configured")
	}
	return p.Creator, nil
}
