package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// WorkstationService owns the durable Douyin download request lifecycle and
// media-library projection. Media authority stays in source_acquisitions and
// immutable SourceAsset records.
type WorkstationService struct {
	db          *storage.DB
	acquisition *AcquisitionService
	credentials *governance.CredentialService
	wake        chan struct{}
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	startOnce   sync.Once
	stopOnce    sync.Once
}

type DownloadRequest struct {
	AwemeIDs              []string                 `json:"aweme_ids,omitempty"`
	Videos                []domain.DiscoveredVideo `json:"videos,omitempty"`
	AttestationID         string                   `json:"attestation_id"`
	AuthorizedCredentials []string                 `json:"authorized_credentials,omitempty"`
	ConsentGranted        bool                     `json:"consent_granted,omitempty"`
}

func NewWorkstationService(db *storage.DB, acquisition *AcquisitionService, credentials *governance.CredentialService) *WorkstationService {
	return &WorkstationService{db: db, acquisition: acquisition, credentials: credentials, wake: make(chan struct{}, 1)}
}

func (s *WorkstationService) Start() error {
	if s == nil || s.db == nil || s.acquisition == nil {
		return errors.New("workstation service is not fully wired")
	}
	var startErr error
	s.startOnce.Do(func() {
		if _, err := s.db.RecoverDouyinDownloads(context.Background(), time.Now().UTC()); err != nil {
			startErr = err
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.wg.Add(1)
		go s.runDownloadWorker(ctx)
		s.signal()
	})
	return startErr
}

func (s *WorkstationService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
			s.wg.Wait()
		}
	})
}

func (s *WorkstationService) QueueDownloads(ctx context.Context, req DownloadRequest) ([]domain.DouyinVideo, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("workstation service is not configured")
	}
	attestationID := strings.TrimSpace(req.AttestationID)
	if attestationID == "" {
		return nil, domain.ErrRightsAttestationRequired
	}
	attestation, err := s.db.GetRightsAttestation(ctx, attestationID)
	if err != nil {
		return nil, fmt.Errorf("resolve rights attestation: %w", err)
	}
	if !attestation.TermsAccepted {
		return nil, domain.ErrRightsAttestationRequired
	}
	credentialRef := ""
	if len(req.AuthorizedCredentials) > 0 {
		candidate := strings.TrimSpace(req.AuthorizedCredentials[0])
		if governance.IsPotentialRawSecret(candidate) {
			return nil, domain.ErrRawSecretForbidden
		}
		if s.credentials == nil {
			return nil, domain.ErrAuthRequired
		}
		ref, err := s.credentials.GetCredentialRef(ctx, candidate)
		if err != nil || ref == nil || strings.TrimSpace(ref.ID) == "" {
			return nil, domain.ErrAuthRequired
		}
		credentialRef = ref.ID
	}

	ids := make([]string, 0, len(req.AwemeIDs)+len(req.Videos))
	observations := make([]domain.DouyinVideo, 0, len(req.Videos))
	seenIDs := make(map[string]struct{})
	now := time.Now().UTC()
	for _, observed := range req.Videos {
		if strings.TrimSpace(observed.AwemeID) == "" || strings.TrimSpace(observed.SourceID) == "" || strings.TrimSpace(observed.CanonicalURL) == "" {
			return nil, fmt.Errorf("%w: download video requires aweme_id, source_id, and canonical_url", domain.ErrInvalidDiscoveryRequest)
		}
		if observed.SourceID != "douyin:aweme:"+observed.AwemeID || provider.DouyinAwemeIDFromURL(observed.CanonicalURL) != observed.AwemeID {
			return nil, fmt.Errorf("%w: download video identity does not match canonical Douyin aweme", domain.ErrInvalidDiscoveryRequest)
		}
		if _, exists := seenIDs[observed.AwemeID]; exists {
			continue
		}
		video := domain.DouyinVideo{
			DiscoveredVideo: observed,
			SecUID: func() string {
				if observed.Creator != nil {
					return observed.Creator.SecUID
				}
				return ""
			}(),
			FirstObservedAt: now, LastObservedAt: now, Origin: domain.DiscoveryOriginHistorical,
			Disposition: domain.DiscoveryDispositionSeen, AcquisitionRequestState: domain.AcquisitionRequestNone,
			LibraryVisible: true, CreatedAt: now, UpdatedAt: now,
		}
		observations = append(observations, video)
		seenIDs[observed.AwemeID] = struct{}{}
		ids = append(ids, observed.AwemeID)
	}
	for _, id := range req.AwemeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("%w: aweme_id is required", domain.ErrInvalidDiscoveryRequest)
		}
		if _, exists := seenIDs[id]; exists {
			continue
		}
		seenIDs[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: at least one video is required", domain.ErrInvalidDiscoveryRequest)
	}
	result, err := s.db.QueueDouyinDownloads(ctx, observations, ids, attestationID, credentialRef, req.ConsentGranted, now)
	if err != nil {
		return nil, err
	}
	s.signal()
	return result, nil
}

func (s *WorkstationService) Retry(ctx context.Context, awemeID string) (*domain.DouyinVideo, error) {
	video, err := s.db.RetryDouyinDownload(ctx, awemeID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	s.signal()
	return video, nil
}

func (s *WorkstationService) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *WorkstationService) runDownloadWorker(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			for {
				video, err := s.db.ClaimNextDouyinDownload(ctx, time.Now().UTC())
				if errors.Is(err, storage.ErrNotFound) {
					break
				}
				if err != nil {
					break
				}
				if err := s.acquireOne(ctx, video); err != nil {
					log.Printf("workstation download terminal persistence failed for %s: %v", video.AwemeID, err)
					break
				}
			}
		}
	}
}

func (s *WorkstationService) acquireOne(ctx context.Context, video *domain.DouyinVideo) error {
	credentials := []string{}
	if video.CredentialRef != "" {
		credentials = append(credentials, video.CredentialRef)
	}
	_, err := s.acquisition.Acquire(ctx, AcquireRequest{
		Locator:               domain.SourceLocator{Type: "douyin_url", Location: video.CanonicalURL},
		AttestationID:         video.RightsAttestationID,
		AuthorizedCredentials: credentials,
		ConsentGranted:        video.ConsentGranted,
	})
	return s.finishAcquire(ctx, video, err)
}

func (s *WorkstationService) finishAcquire(ctx context.Context, video *domain.DouyinVideo, err error) error {
	if err == nil {
		return s.db.FinishDouyinDownload(context.WithoutCancel(ctx), video.AwemeID, domain.AcquisitionRequestDownloaded, nil, time.Now().UTC())
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		// Host shutdown leaves the claimed request as downloading. The next
		// Start() runs RecoverDouyinDownloads and makes it queued again.
		return nil
	}
	failure := classifyWorkstationFailure(err)
	return s.db.FinishDouyinDownload(context.WithoutCancel(ctx), video.AwemeID, domain.AcquisitionRequestFailed, failure, time.Now().UTC())
}

func classifyWorkstationFailure(err error) *domain.WorkstationFailure {
	failure := &domain.WorkstationFailure{Code: "ACQUISITION_FAILED", Message: "acquisition failed"}
	var acquisitionErr *domain.AcquisitionError
	if errors.As(err, &acquisitionErr) {
		failure.Code = string(acquisitionErr.State)
		failure.ProviderID = acquisitionErr.ProviderID
		failure.Message = safeAcquisitionFailureMessage(acquisitionErr.State)
		return failure
	}
	switch {
	case errors.Is(err, domain.ErrRightsAttestationRequired):
		failure.Code = "RIGHTS_ATTESTATION_REQUIRED"
		failure.Message = "rights attestation is required"
	case errors.Is(err, domain.ErrAuthRequired):
		failure.Code = "AUTH_REQUIRED"
		failure.Message = "authorized credential reference is required"
	case errors.Is(err, domain.ErrConsentRequired):
		failure.Code = "CONSENT_REQUIRED"
		failure.Message = "explicit operator consent is required"
	case errors.Is(err, domain.ErrPolicyBlocked):
		failure.Code = "POLICY_BLOCKED"
		failure.Message = "provider execution is blocked by policy"
	case errors.Is(err, domain.ErrInconsistentProvenance):
		failure.Code = "INCONSISTENT_PROVENANCE"
		failure.Message = "existing acquisition provenance is inconsistent"
	}
	return failure
}

func safeAcquisitionFailureMessage(state domain.AcquisitionState) string {
	switch state {
	case domain.AcquisitionAuthRequired:
		return "authorized credential reference is required"
	case domain.AcquisitionSessionExpired:
		return "authorized session expired"
	case domain.AcquisitionCaptchaRequired:
		return "provider requires operator CAPTCHA resolution"
	case domain.AcquisitionAntiBotOrEmpty:
		return "provider returned an anti-bot or empty response"
	case domain.AcquisitionContentUnavailable:
		return "source content is unavailable"
	case domain.AcquisitionIntegrityFailed:
		return "downloaded media failed integrity validation"
	case domain.AcquisitionInvalidURL:
		return "source URL is invalid"
	case domain.AcquisitionUnsupportedMediaType:
		return "source media type is unsupported"
	default:
		return "media download failed"
	}
}

func (s *WorkstationService) ListMedia(ctx context.Context) ([]domain.MediaLibraryEntry, error) {
	videos, err := s.db.ListDouyinVideos(ctx, "")
	if err != nil {
		return nil, err
	}
	entries := make([]domain.MediaLibraryEntry, 0, len(videos))
	seenSource := make(map[string]struct{})
	for _, video := range videos {
		if !video.LibraryVisible {
			continue
		}
		if video.AcquisitionRequestState == domain.AcquisitionRequestNone {
			if _, err := s.db.GetSourceAcquisitionBySourceID(ctx, video.SourceID); errors.Is(err, storage.ErrNotFound) {
				continue
			} else if err != nil {
				return nil, err
			}
		}
		entry := domain.MediaLibraryEntry{
			AwemeID: video.AwemeID, SourceID: video.SourceID, CanonicalURL: video.CanonicalURL,
			Title: video.Title, CoverURL: video.CoverURL, PublishedAt: video.PublishedAt, LikeCount: video.LikeCount, DurationMs: video.DurationMs,
			AcquisitionState: video.AcquisitionRequestState, Failure: video.AcquisitionFailure,
			Thumbnail: domain.MediaThumbnail{Kind: "provider_snapshot", FallbackURL: video.CoverURL},
		}
		if err := s.attachMediaTruth(ctx, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		seenSource[video.SourceID] = struct{}{}
	}
	provenance, err := s.db.ListSourceAcquisitions(ctx, "douyin")
	if err != nil {
		return nil, err
	}
	for _, prov := range provenance {
		if _, exists := seenSource[prov.SourceID]; exists {
			continue
		}
		entry := domain.MediaLibraryEntry{
			AwemeID: awemeIDFromSourceID(prov.SourceID), SourceID: prov.SourceID, CanonicalURL: prov.CanonicalURL,
			AcquisitionState: domain.AcquisitionRequestDownloaded, LegacyFallback: true,
			Thumbnail: domain.MediaThumbnail{Kind: "local_source_frame", AssetID: prov.AssetID},
		}
		if err := s.attachMediaTruth(ctx, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (s *WorkstationService) attachMediaTruth(ctx context.Context, entry *domain.MediaLibraryEntry) error {
	prov, err := s.db.GetSourceAcquisitionBySourceID(ctx, entry.SourceID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	asset, err := s.db.GetSourceAsset(ctx, prov.AssetID)
	if err != nil {
		return err
	}
	localAvailable := false
	if info, statErr := os.Stat(asset.CASPath); statErr == nil && !info.IsDir() {
		localAvailable = true
	}
	entry.AcquisitionState = domain.AcquisitionRequestDownloaded
	entry.Failure = nil
	entry.Asset = &domain.MediaLibraryAsset{ID: asset.ID, SHA256: asset.SHA256, ByteSize: asset.ByteSize, MimeType: asset.MimeType, LocalAvailable: localAvailable}
	entry.Thumbnail = domain.MediaThumbnail{Kind: "local_source_frame", AssetID: asset.ID, URL: "/api/v1/assets/" + url.PathEscape(asset.ID) + "/thumbnail", FallbackURL: entry.CoverURL}
	jobs, err := s.db.ListJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.SourceAssetID != asset.ID {
			continue
		}
		handoff := domain.ProductionHandoff{TargetLanguage: job.TargetLanguage, JobID: job.ID, JobStatus: job.Status, View: "jobs"}
		runs, err := s.db.ListRunsByJobID(ctx, job.ID)
		if err != nil {
			return err
		}
		if len(runs) > 0 {
			latest := runs[len(runs)-1]
			handoff.RunID, handoff.RunStatus = latest.ID, latest.Status
		}
		switch job.Status {
		case "review_required":
			handoff.View = "inspector"
		case "completed":
			handoff.View = "result"
		}
		entry.Production = append(entry.Production, handoff)
	}
	return nil
}

func awemeIDFromSourceID(sourceID string) string {
	const prefix = "douyin:aweme:"
	if strings.HasPrefix(sourceID, prefix) {
		return strings.TrimPrefix(sourceID, prefix)
	}
	return ""
}
