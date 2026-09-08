package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

// RawTextDetection represents a raw OCR detection on a specific sampled video frame.
type RawTextDetection struct {
	FrameIndex  int                `json:"frame_index"`
	TimestampMs int64              `json:"timestamp_ms"`
	Text        string             `json:"text"`
	Confidence  float64            `json:"confidence"`
	Box         domain.BoundingBox `json:"box"`
}

// OCRRequest encapsulates input parameters for visual text detection/OCR.
type OCRRequest struct {
	AssetID           string             `json:"asset_id"`
	SourceVideo       worker.ArtifactRef `json:"source_video"`
	FrameSampleStepMs int64              `json:"frame_sample_step_ms,omitempty"`
	MaxFrames         int                `json:"max_frames,omitempty"`
}

// OCRResult encapsulates raw detected text regions across sampled frames.
type OCRResult struct {
	ProviderID        string
	ModelName         string
	ModelVersion      string
	FrameWidth        int
	FrameHeight       int
	FrameSampleStepMs int64
	Detections        []RawTextDetection
	DetSnapshotDigest string
	RecSnapshotDigest string
	OriSnapshotDigest string
	RuntimeIdentity   string
}

// OCRSnapshotProvider is optionally implemented by OCR providers that expose their verified snapshot and runtime identities.
type OCRSnapshotProvider interface {
	OCRSnapshotDigests() (detSHA, recSHA, oriSHA, runtimeIdentity string)
}

// OCRRegionProvider defines the capability interface for visual text detection and OCR.
type OCRRegionProvider interface {
	// DetectRegions performs visual OCR text detection across sampled frames of the source media.
	DetectRegions(ctx context.Context, req OCRRequest) (*OCRResult, error)
}
