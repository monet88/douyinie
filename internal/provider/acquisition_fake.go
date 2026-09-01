package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

// FakeAcquisitionProvider is a deterministic acquisition adapter for Seam 1
// acceptance tests. It exposes the behavior under test (probe/acquire
// outcomes, structural states, auth gating) without network or subprocess use.
type FakeAcquisitionProvider struct {
	BaseFakeProvider
	// Descriptor returned by Probe when ProbeErr is nil.
	Descriptor *domain.SourceDescriptor
	// ProbeErr, when set, is returned by Probe (use *domain.AcquisitionError
	// for structural states).
	ProbeErr error
	// AcquireErr, when set, is returned by Acquire instead of writing media.
	AcquireErr error
	// SecretErr, when set and an authRef is supplied, is returned by Acquire
	// before any media work — models a permanent credential/config failure
	// (domain.ErrAuthRequired wrapped by CredentialService.MaterializeSecret).
	SecretErr error
	// Method reported on successful acquire ("api" | "browser"); empty -> "api".
	Method string
	// MediaBytes is written to destDir/<source-id><MediaExt> on successful acquire.
	MediaBytes []byte
	// MediaExt is the produced file's extension (default ".mp4"); the reported
	// MimeType always reflects the actual produced file, never a constant.
	MediaExt string
	// Calls records the ordered sequence of "probe"/"acquire" invocations.
	Calls []string
	// SawAuthRef records the auth reference passed to the last acquire.
	SawAuthRef string
}

func NewFakeAcquisitionProvider(id string, policy domain.PolicyState, quality float64) *FakeAcquisitionProvider {
	return &FakeAcquisitionProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeAcquisition,
			Policy:       policy,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeAcquisition),
				ExecutionTier:  "hybrid",
				CostPerUnit:    0.0,
				QualityScore:   quality,
				MaxConcurrency: 1,
				Features:       []string{"douyin"},
			},
			ModelName:    id,
			ModelVersion: "v1",
		},
		MediaBytes: []byte("FAKE_DOUYIN_MEDIA_PAYLOAD_" + id),
	}
}

func (p *FakeAcquisitionProvider) Probe(ctx context.Context, locator domain.SourceLocator, authRef string) (*domain.SourceDescriptor, error) {
	p.Calls = append(p.Calls, "probe")
	if p.ProbeErr != nil {
		return nil, p.ProbeErr
	}
	if p.Descriptor != nil {
		return p.Descriptor, nil
	}
	aweme := extractAwemeID(locator.Location)
	if aweme == "" {
		aweme = "0"
	}
	return &domain.SourceDescriptor{
		SourceID:     "douyin:aweme:" + aweme,
		Platform:     "douyin",
		CanonicalURL: "https://www.douyin.com/video/" + aweme,
		MediaType:    "video",
	}, nil
}

func (p *FakeAcquisitionProvider) Acquire(ctx context.Context, desc domain.SourceDescriptor, destDir string, authRef string) (*AcquiredMedia, error) {
	p.Calls = append(p.Calls, "acquire")
	p.SawAuthRef = authRef
	if authRef != "" && p.SecretErr != nil {
		return nil, p.SecretErr
	}
	if p.AcquireErr != nil {
		return nil, p.AcquireErr
	}
	ext := p.MediaExt
	if ext == "" {
		ext = ".mp4"
	}
	path := filepath.Join(destDir, strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(desc.SourceID)+ext)
	if err := os.WriteFile(path, p.MediaBytes, 0644); err != nil {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: p.ProviderID, Detail: "write failed"}
	}
	method := p.Method
	if method == "" {
		method = "api"
	}
	return &AcquiredMedia{FilePath: path, MimeType: mediaMimeType(path), Method: method, Authenticated: authRef != ""}, nil
}
