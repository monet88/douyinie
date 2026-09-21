package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

type DiscoveryService struct {
	router *provider.Router
	store  *storage.DB
}

func NewDiscoveryService(router *provider.Router, store *storage.DB) *DiscoveryService {
	return &DiscoveryService{router: router, store: store}
}

const defaultFollowBaselineLimit = 20
const maxFollowBaselineLimit = 50

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

type FollowCreatorRequest struct {
	URL                   string   `json:"url"`
	RecentLimit           int      `json:"recent_limit,omitempty"`
	ConfiguredBy          string   `json:"configured_by,omitempty"`
	RightsAttestationID   string   `json:"rights_attestation_id,omitempty"`
	AuthorizedCredentials []string `json:"authorized_credentials,omitempty"`
}

type LoadOlderRequest struct {
	RecentLimit           int      `json:"recent_limit,omitempty"`
	AuthorizedCredentials []string `json:"authorized_credentials,omitempty"`
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

func (s *DiscoveryService) FollowCreator(ctx context.Context, req FollowCreatorRequest) (*domain.FollowedCreator, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("durable Douyin discovery unavailable")
	}
	limit := boundedRecentLimit(req.RecentLimit)
	lookup, err := s.LookupCreator(ctx, DiscoveryRequest{URL: req.URL, RecentLimit: limit, AuthorizedCredentials: req.AuthorizedCredentials})
	if err != nil {
		return nil, err
	}
	if !lookup.RecentViewAvailable {
		return nil, fmt.Errorf("%w: creator recent baseline is unavailable", domain.ErrNoEligibleProvider)
	}
	if strings.TrimSpace(lookup.Creator.SecUID) == "" {
		return nil, fmt.Errorf("%w: creator sec_uid is required", domain.ErrInvalidDiscoveryRequest)
	}
	now := time.Now().UTC()
	creator := domain.FollowedCreator{
		SecUID: lookup.Creator.SecUID, CanonicalURL: lookup.Creator.CanonicalURL, DisplayName: lookup.Creator.DisplayName, AvatarURL: lookup.Creator.AvatarURL,
		Followed: true, FollowedAt: now, BaselineAt: now, AutomationMode: domain.ChannelAutomationDetect,
		TargetLanguage: "vi", ReviewPosture: "auto", CoverColor: "#000000", ConfiguredBy: strings.TrimSpace(req.ConfiguredBy),
		RightsAttestationID: strings.TrimSpace(req.RightsAttestationID),
		PollState:           "idle", CreatedAt: now, UpdatedAt: now,
	}
	if creator.ConfiguredBy == "" {
		creator.ConfiguredBy = "local-operator"
	}
	if creator.RightsAttestationID != "" {
		attestation, err := s.store.GetRightsAttestation(ctx, creator.RightsAttestationID)
		if err != nil {
			return nil, fmt.Errorf("resolve rights attestation: %w", err)
		}
		if !attestation.TermsAccepted {
			return nil, domain.ErrRightsAttestationRequired
		}
	}
	if previous, getErr := s.store.GetFollowedCreator(ctx, creator.SecUID); getErr == nil {
		creator.AutomationMode = previous.AutomationMode
		creator.TargetLanguage = previous.TargetLanguage
		creator.ReviewPosture = previous.ReviewPosture
		creator.CoverColor = previous.CoverColor
		if creator.RightsAttestationID == "" {
			creator.RightsAttestationID = previous.RightsAttestationID
		}
		creator.CreatedAt = previous.CreatedAt
	} else if !errors.Is(getErr, storage.ErrNotFound) {
		return nil, fmt.Errorf("load existing followed creator: %w", getErr)
	}
	baseline := make([]domain.DouyinVideo, 0, limit)
	for _, observed := range boundedVideos(lookup.RecentVideos, limit) {
		video, err := retainedVideo(creator.SecUID, observed, domain.DiscoveryOriginBaseline, domain.DiscoveryDispositionSeen, now)
		if err != nil {
			return nil, err
		}
		baseline = append(baseline, video)
	}
	if err := s.store.SaveFollowBaseline(ctx, creator, baseline); err != nil {
		return nil, err
	}
	return s.store.GetFollowedCreator(ctx, creator.SecUID)
}

func (s *DiscoveryService) UnfollowCreator(ctx context.Context, secUID string) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("durable Douyin discovery unavailable")
	}
	if strings.TrimSpace(secUID) == "" {
		return fmt.Errorf("%w: sec_uid is required", domain.ErrInvalidDiscoveryRequest)
	}
	return s.store.SetCreatorFollowed(ctx, secUID, false, time.Now().UTC())
}

func (s *DiscoveryService) ListFollowedCreators(ctx context.Context) ([]domain.FollowedCreator, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("durable Douyin discovery unavailable")
	}
	return s.store.ListFollowedCreators(ctx)
}

func (s *DiscoveryService) ListRetainedVideos(ctx context.Context, secUID string) ([]domain.DouyinVideo, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("durable Douyin discovery unavailable")
	}
	return s.store.ListDouyinVideos(ctx, secUID)
}

func (s *DiscoveryService) SetVideoDisposition(ctx context.Context, awemeID, action string) (*domain.DouyinVideo, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("durable Douyin discovery unavailable")
	}
	video, err := s.store.GetDouyinVideo(ctx, awemeID)
	if err != nil {
		return nil, err
	}
	var next domain.DiscoveryDisposition
	switch action {
	case "seen":
		if video.Disposition == domain.DiscoveryDispositionIgnored {
			return video, nil
		}
		next = domain.DiscoveryDispositionSeen
	case "ignore":
		next = domain.DiscoveryDispositionIgnored
	case "unignore":
		if video.Disposition != domain.DiscoveryDispositionIgnored {
			return video, nil
		}
		next = domain.DiscoveryDispositionSeen
	default:
		return nil, fmt.Errorf("%w: unsupported disposition action %q", domain.ErrInvalidDiscoveryRequest, action)
	}
	if err := s.store.SetDouyinVideoDisposition(ctx, awemeID, next, time.Now().UTC()); err != nil {
		return nil, err
	}
	return s.store.GetDouyinVideo(ctx, awemeID)
}

func (s *DiscoveryService) LoadOlder(ctx context.Context, secUID string, req LoadOlderRequest) ([]domain.DouyinVideo, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("durable Douyin discovery unavailable")
	}
	creator, err := s.store.GetFollowedCreator(ctx, secUID)
	if err != nil {
		return nil, err
	}
	limit := boundedRecentLimit(req.RecentLimit)
	lookup, err := s.LookupCreator(ctx, DiscoveryRequest{URL: creator.CanonicalURL, RecentLimit: limit, AuthorizedCredentials: req.AuthorizedCredentials})
	if err != nil {
		return nil, err
	}
	if !lookup.RecentViewAvailable {
		return nil, fmt.Errorf("%w: creator historical view is unavailable", domain.ErrNoEligibleProvider)
	}
	now := time.Now().UTC()
	for _, observed := range boundedVideos(lookup.RecentVideos, limit) {
		if err := s.retainVideo(ctx, secUID, observed, domain.DiscoveryOriginHistorical, domain.DiscoveryDispositionSeen, now); err != nil {
			return nil, err
		}
	}
	return s.store.ListDouyinVideos(ctx, secUID)
}

// RetainMonitoringVideos is the persistence contract later polling (#130) calls
// after a successful observation. It does not fetch, schedule, or advance poll
// checkpoints in this ticket. It validates the baselineToken captured at poll
// start so a stale poll cannot reintroduce New videos after an Unfollow -> Re-follow.
func (s *DiscoveryService) RetainMonitoringVideos(ctx context.Context, secUID string, baselineToken time.Time, videos []domain.DiscoveredVideo) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("durable Douyin discovery unavailable")
	}
	now := time.Now().UTC()
	retained := make([]domain.DouyinVideo, 0, len(videos))
	for _, observed := range videos {
		video, err := retainedVideo(secUID, observed, domain.DiscoveryOriginMonitoring, domain.DiscoveryDispositionNew, now)
		if err != nil {
			return err
		}
		retained = append(retained, video)
	}
	_, err := s.store.SaveMonitoringVideosIfFollowed(ctx, secUID, baselineToken, retained)
	return err
}

func (s *DiscoveryService) retainVideo(ctx context.Context, secUID string, observed domain.DiscoveredVideo, origin domain.DiscoveryOrigin, initialDisposition domain.DiscoveryDisposition, now time.Time) error {
	video, err := retainedVideo(secUID, observed, origin, initialDisposition, now)
	if err != nil {
		return err
	}
	return s.store.SaveDouyinVideoObservation(ctx, video)
}

func retainedVideo(secUID string, observed domain.DiscoveredVideo, origin domain.DiscoveryOrigin, initialDisposition domain.DiscoveryDisposition, now time.Time) (domain.DouyinVideo, error) {
	if strings.TrimSpace(observed.AwemeID) == "" || strings.TrimSpace(observed.SourceID) == "" || strings.TrimSpace(observed.CanonicalURL) == "" {
		return domain.DouyinVideo{}, fmt.Errorf("%w: retained video requires aweme_id, source_id, and canonical_url", domain.ErrInvalidDiscoveryRequest)
	}
	video := domain.DouyinVideo{DiscoveredVideo: observed, SecUID: secUID, FirstObservedAt: now, LastObservedAt: now, Origin: origin,
		Disposition: initialDisposition, AcquisitionRequestState: "none", LibraryVisible: true, CreatedAt: now, UpdatedAt: now}
	return video, nil
}

func boundedRecentLimit(limit int) int {
	if limit <= 0 {
		return defaultFollowBaselineLimit
	}
	if limit > maxFollowBaselineLimit {
		return maxFollowBaselineLimit
	}
	return limit
}

func boundedVideos(videos []domain.DiscoveredVideo, limit int) []domain.DiscoveredVideo {
	if len(videos) <= limit {
		return videos
	}
	return videos[:limit]
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
