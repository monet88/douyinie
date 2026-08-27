package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
)

// ASRTranscriptProvider is implemented by ASR providers (worker adapters) that
// can produce raw transcript segments. The Router's ExecuteWithRetry drives the
// 1.7B quality attempt -> 0.6B fallback by routing among providers that
// implement this capability and falling back on domain.ErrQualityRejected.
type ASRTranscriptProvider interface {
	// ProduceTranscript returns the accepted raw VAD-turn transcript segments
	// for the source audio at audioPath. The path is a machine-local reference
	// resolved by the RuntimeHost from asset/CAS metadata; it is never
	// client-supplied. It may return domain.ErrQualityRejected to signal
	// a quality failure so ExecuteWithRetry advances to the next candidate.
	ProduceTranscript(ctx context.Context, audioPath string) ([]domain.ASRRawSegment, error)
}

// AlignWordProvider is implemented by forced-aligner providers (worker
// adapters) that align accepted text to the source timeline as word timings.
type AlignWordProvider interface {
	// ProduceAlignment aligns the accepted transcript text to the source audio
	// at audioPath and returns word-level timings. The path is a machine-local
	// reference resolved by the RuntimeHost from asset/CAS metadata. It may
	// return domain.ErrQualityRejected to advance to the next candidate.
	ProduceAlignment(ctx context.Context, audioPath string, text string) ([]domain.WordTiming, error)
}
