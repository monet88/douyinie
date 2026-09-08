package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

// Frozen RC identities and digests for audio stem separation (Issue #69)
const (
	// UVR Primary frozen RC identities
	UVRModelID          = domain.PinnedUVRModelID
	UVRModelVersion     = domain.PinnedUVRModelVersion
	UVRArtifactSHA      = domain.PinnedUVRArtifactSHA256
	UVRProviderID       = "uvr_separator"

	// Demucs Fallback frozen RC identities
	DemucsModelID       = domain.PinnedDemucsModelID
	DemucsModelVersion  = domain.PinnedDemucsModelVersion
	DemucsCheckpointSHA = domain.PinnedDemucsCheckpointSHA
	DemucsSignature     = domain.PinnedDemucsSignature
	DemucsBagYAML       = domain.PinnedDemucsBagYAML
	DemucsBagYAMLSHA    = domain.PinnedDemucsBagYAMLSHA256
	DemucsProviderID    = "demucs_separator"
)

// SeparationRequest encapsulates the input parameters for audio stem separation.
type SeparationRequest struct {
	AssetID       string                `json:"asset_id"`
	SourceAudio   worker.ArtifactRef    `json:"source_audio"`
	AudioRolePlan *domain.AudioRolePlan `json:"audio_role_plan,omitempty"`
}

// SeparationResult encapsulates the output separated stems.
type SeparationResult struct {
	ProviderID             string
	ModelName              string
	ModelVersion           string
	SnapshotManifestSHA256 string
	RuntimeIdentity        string
	VocalsWAV              []byte
	BackgroundWAV          []byte
	DurationMs             int64
	SampleRate             int
	Channels               int
}

// AudioSeparatorProvider defines the capability interface for vocal/background audio stem separation.
type AudioSeparatorProvider interface {
	// SeparateStems separates the source audio into vocals and background (music/SFX/ambience) stems.
	SeparateStems(ctx context.Context, req SeparationRequest) (*SeparationResult, error)
}
