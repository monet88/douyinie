package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

type DiscoveryService struct {
	router *provider.Router
}

func NewDiscoveryService(router *provider.Router) *DiscoveryService {
	return &DiscoveryService{router: router}
}

type DiscoveryRequest struct {
	Query                 string     `json:"query,omitempty"`
	URL                   string     `json:"url,omitempty"`
	Continuation          string     `json:"continuation,omitempty"`
	Limit                 int        `json:"limit,omitempty"`
	RecentLimit           int        `json:"recent_limit,omitempty"`
	PublishedAfter        *time.Time `json:"published_after,omitempty"`
	MinLikes              *int64     `json:"min_likes,omitempty"`
	AuthorizedCredentials []string   `json:"authorized_credentials,omitempty"`
}

func (s *DiscoveryService) Search(ctx context.Context, req DiscoveryRequest) (*domain.DiscoveryPage, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, fmt.Errorf("%w: query is required", domain.ErrInvalidDiscoveryRequest)
	}
	var page *domain.DiscoveryPage
	err := s.execute(ctx, "keyword_search", req.AuthorizedCredentials, req.Query+req.Continuation, func(p provider.DouyinDiscoveryProvider, authRef string) error {
		var err error
		page, err = p.Search(ctx, req.Query, req.Continuation, req.Limit, authRef)
		return err
	})
	if err != nil {
		return nil, err
	}
	page.Videos = filterDiscoveredVideos(page.Videos, req.PublishedAfter, req.MinLikes)
	return page, nil
}

func (s *DiscoveryService) LookupVideo(ctx context.Context, req DiscoveryRequest) (*domain.DiscoveredVideo, error) {
	if strings.TrimSpace(req.URL) == "" {
		return nil, fmt.Errorf("%w: url is required", domain.ErrInvalidDiscoveryRequest)
	}
	var video *domain.DiscoveredVideo
	err := s.execute(ctx, "video_lookup", req.AuthorizedCredentials, req.URL, func(p provider.DouyinDiscoveryProvider, authRef string) error {
		var err error
		video, err = p.LookupVideo(ctx, req.URL, authRef)
		return err
	})
	return video, err
}

func (s *DiscoveryService) LookupCreator(ctx context.Context, req DiscoveryRequest) (*domain.CreatorLookup, error) {
	if strings.TrimSpace(req.URL) == "" {
		return nil, fmt.Errorf("%w: url is required", domain.ErrInvalidDiscoveryRequest)
	}
	var creator *domain.CreatorLookup
	err := s.execute(ctx, "creator_lookup", req.AuthorizedCredentials, req.URL, func(p provider.DouyinDiscoveryProvider, authRef string) error {
		var err error
		creator, err = p.LookupCreator(ctx, req.URL, req.RecentLimit, authRef)
		return err
	})
	if err != nil {
		return nil, err
	}
	creator.RecentVideos = filterDiscoveredVideos(creator.RecentVideos, req.PublishedAfter, req.MinLikes)
	return creator, nil
}

func (s *DiscoveryService) execute(ctx context.Context, feature string, credentials []string, input string, run func(provider.DouyinDiscoveryProvider, string) error) error {
	if s == nil || s.router == nil {
		return fmt.Errorf("Douyin discovery unavailable")
	}
	routeReq := provider.RouteRequest{
		Stage:                 provider.TypeDouyinDiscovery,
		ExecutionProfile:      domain.ExecutionProfileHybrid,
		RequiredFeatures:      []string{"douyin", feature},
		AuthorizedCredentials: credentials,
	}
	route, err := s.router.Route(ctx, routeReq)
	if err != nil {
		return err
	}
	authRef := ""
	if len(credentials) > 0 {
		authRef = credentials[0]
	}
	return s.router.ExecuteRoutedWithRetry(ctx, routeReq, route, sha256OfString(input), 1, func(p provider.Provider, _ int) error {
		discovery, ok := p.(provider.DouyinDiscoveryProvider)
		if !ok {
			return fmt.Errorf("provider %s does not implement Douyin discovery", p.ID())
		}
		return run(discovery, authRef)
	})
}

func filterDiscoveredVideos(videos []domain.DiscoveredVideo, after *time.Time, minLikes *int64) []domain.DiscoveredVideo {
	if after == nil && minLikes == nil {
		return videos
	}
	out := make([]domain.DiscoveredVideo, 0, len(videos))
	for _, video := range videos {
		if after != nil && video.PublishedAt != nil && video.PublishedAt.Before(*after) {
			continue
		}
		// Unknown likes remain unknown and remain visible; they are not coerced
		// to zero and therefore cannot be falsely rejected by a minimum.
		if minLikes != nil && video.LikeCount != nil && *video.LikeCount < *minLikes {
			continue
		}
		out = append(out, video)
	}
	return out
}
