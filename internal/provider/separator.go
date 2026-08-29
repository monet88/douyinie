package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

// SeparationRequest encapsulates the input parameters for audio stem separation.
type SeparationRequest struct {
	AssetID       string                `json:"asset_id"`
	SourceAudio   worker.ArtifactRef    `json:"source_audio"`
	AudioRolePlan *domain.AudioRolePlan `json:"audio_role_plan,omitempty"`
}

// SeparationResult encapsulates the output separated stems.
type SeparationResult struct {
	ProviderID    string
	ModelName     string
	ModelVersion  string
	VocalsWAV     []byte
	BackgroundWAV []byte
	DurationMs    int64
	SampleRate    int
	Channels      int
}

// AudioSeparatorProvider defines the capability interface for vocal/background audio stem separation.
type AudioSeparatorProvider interface {
	// SeparateStems separates the source audio into vocals and background (music/SFX/ambience) stems.
	SeparateStems(ctx context.Context, req SeparationRequest) (*SeparationResult, error)
}
