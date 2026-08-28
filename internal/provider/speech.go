package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

// ASRTranscriptProvider is implemented by ASR providers (worker adapters) that
// can produce raw transcript segments. The Router's ExecuteWithRetry drives the
// 1.7B quality attempt -> 0.6B fallback by routing among providers that
// implement this capability and falling back on domain.ErrQualityRejected.
type ASRTranscriptProvider interface {
	// ProduceTranscript returns the accepted raw VAD-turn transcript segments
	// for the source audio artifact. The artifact is a metadata-only reference
	// resolved by the RuntimeHost carrying the real content SHA256 and CAS path.
	// It may return domain.ErrQualityRejected to signal a quality failure so
	// ExecuteWithRetry advances to the next candidate.
	ProduceTranscript(ctx context.Context, audio worker.ArtifactRef) ([]domain.ASRRawSegment, error)
}

// AlignWordProvider is implemented by forced-aligner providers (worker
// adapters) that align accepted text to the source timeline as word timings.
type AlignWordProvider interface {
	// ProduceAlignment aligns the accepted transcript text to the source audio
	// artifact and returns word-level timings. The artifact is a metadata-only
	// reference resolved by the RuntimeHost carrying the real content SHA256
	// and CAS path. It may return domain.ErrQualityRejected to advance to the
	// next candidate.
	ProduceAlignment(ctx context.Context, audio worker.ArtifactRef, text string) ([]domain.WordTiming, error)
}

// DiarizationProvider is implemented by diarization providers (worker
// adapters) that segment source audio into speaker time regions. It runs as a
// separate conditional pipeline step BEFORE canonical segmentation, gated by
// pre-diarization speech evidence — never by the SpeakerID labels it produces.
type DiarizationProvider interface {
	// ProbeSpeakerEvidence probes the source audio artifact for multi-speaker cues
	// (cheap acoustic/embedding/turn probe) before full diarization.
	ProbeSpeakerEvidence(ctx context.Context, audio worker.ArtifactRef) (*domain.SpeakerEvidence, error)

	// ProduceDiarization returns speaker regions (absolute milliseconds) for
	// the source audio artifact. The artifact is a metadata-only reference
	// resolved by the RuntimeHost carrying the real content SHA256 and CAS
	// path. It may return domain.ErrQualityRejected to advance to the next candidate.
	ProduceDiarization(ctx context.Context, audio worker.ArtifactRef) ([]domain.SpeakerAssignment, error)
}
