package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	// ErrAtempoUnavailable reports that the ffmpeg binary a tempo transform needs is missing.
	ErrAtempoUnavailable = errors.New("ffmpeg atempo transform unavailable")
	// ErrAtempoFactorOutOfRange reports a factor outside the single-filter range (1, 1.25].
	ErrAtempoFactorOutOfRange = errors.New("atempo factor must be finite and within (1, 1.25]")
	// ErrAtempoOutputTooLarge reports a transform output past the bounded ceiling.
	ErrAtempoOutputTooLarge = errors.New("ffmpeg atempo output exceeds bound")
	// ErrAtempoOutputInvalid reports output that is not a readable WAV container.
	ErrAtempoOutputInvalid = errors.New("ffmpeg atempo output is not valid audio")
	// ErrAtempoSourceInvalid reports a retained waveform that is absent, empty or not a
	// readable WAV container. It is distinct from ErrAtempoFailed because the source — not the
	// transform invocation — is the failing side, and callers must report it as unavailable
	// source evidence rather than as a transform failure.
	ErrAtempoSourceInvalid = errors.New("ffmpeg atempo source is not usable audio")
	// ErrAtempoFailed reports a failed ffmpeg invocation.
	ErrAtempoFailed = errors.New("ffmpeg atempo transform failed")
)

const (
	// AtempoMaxFactor is the largest single-filter factor a review candidate may request.
	AtempoMaxFactor = 1.25
	// DefaultAtempoMaxOutputBytes bounds the transformed artifact. A 25% tempo change can
	// only shrink audio, so this ceiling sits far above any candidate the dubbing pipeline
	// can legitimately retain while still refusing an unbounded subprocess write.
	DefaultAtempoMaxOutputBytes int64 = 64 << 20
)

// AtempoRequest is one bounded, review-only FFmpeg atempo transform.
type AtempoRequest struct {
	// SourceWAV is the intact retained waveform to speed up.
	SourceWAV []byte
	// Factor is the tempo factor; it must be inside (1, AtempoMaxFactor].
	Factor float64
	// FFmpegPath overrides the ffmpeg binary; empty means "ffmpeg" on PATH.
	FFmpegPath string
	// MaxOutputBytes bounds the produced artifact; 0 means DefaultAtempoMaxOutputBytes.
	MaxOutputBytes int64
}

// AtempoFilterString formats the single atempo filter token for a factor. It is the one place
// the token is spelled, so the lineage a caller records is the exact filter that ran.
func AtempoFilterString(factor float64) string {
	return fmt.Sprintf("atempo=%.6f", factor)
}

// AtempoFilterArgs returns the exact ffmpeg argument tail of the single atempo transform.
// It is pure so the "one filter, finite numeric argument, no crop/atrim" contract is
// directly testable: the only filter token is a finite atempo factor and nothing else.
func AtempoFilterArgs(factor float64) ([]string, error) {
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor <= 1 || factor > AtempoMaxFactor {
		return nil, fmt.Errorf("%w: got %v", ErrAtempoFactorOutOfRange, factor)
	}
	return []string{"-vn", "-af", AtempoFilterString(factor), "-c:a", "pcm_s16le"}, nil
}

// ApplyAtempoWAV applies one FFmpeg atempo filter to source audio and returns the produced
// WAV bytes. The source is validated as a WAV container first, the subprocess inherits ctx
// cancellation, and the write is bounded in the subprocess itself (-fs) and re-checked against
// the same ceiling before the output is re-validated as WAV.
// Work happens in a throwaway temp directory that is always removed, so a cancelled or
// failed transform leaves no partial artifact behind.
func ApplyAtempoWAV(ctx context.Context, req AtempoRequest) ([]byte, error) {
	if len(req.SourceWAV) == 0 {
		return nil, fmt.Errorf("%w: empty source", ErrAtempoSourceInvalid)
	}
	if _, err := ParseWAVHeader(req.SourceWAV); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAtempoSourceInvalid, err)
	}
	args, err := AtempoFilterArgs(req.Factor)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	maxOut := req.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultAtempoMaxOutputBytes
	}
	bin := req.FFmpegPath
	if bin == "" {
		bin = "ffmpeg"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAtempoUnavailable, err)
	}

	tmpDir, err := os.MkdirTemp("", "douyinie_atempo_*")
	if err != nil {
		return nil, fmt.Errorf("%w: temp dir: %v", ErrAtempoFailed, err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	inPath := filepath.Join(tmpDir, "source.wav")
	outPath := filepath.Join(tmpDir, "transformed.wav")
	if err := os.WriteFile(inPath, req.SourceWAV, 0o600); err != nil {
		return nil, fmt.Errorf("%w: stage source: %v", ErrAtempoFailed, err)
	}

	// The subprocess itself is bounded: -fs stops ffmpeg from writing more than one byte past
	// the ceiling, so an unbounded or hostile encoder can never fill the disk. The single byte
	// of slack keeps the "refuse anything over maxOut" semantics exact - a legitimate output of
	// exactly maxOut bytes is never truncated by the limit.
	writeCeiling := maxOut + 1
	cmdArgs := append([]string{"-y", "-v", "error", "-i", inPath}, args...)
	cmdArgs = append(cmdArgs, "-fs", strconv.FormatInt(writeCeiling, 10), outPath)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %v", ctxErr, err)
		}
		return nil, fmt.Errorf("%w: %v; stderr: %s", ErrAtempoFailed, err, strings.TrimSpace(stderr.String()))
	}

	f, err := os.Open(outPath)
	if err != nil {
		return nil, fmt.Errorf("%w: open output: %v", ErrAtempoFailed, err)
	}
	defer func() { _ = f.Close() }()
	out, err := io.ReadAll(io.LimitReader(f, maxOut+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read output: %v", ErrAtempoFailed, err)
	}
	if int64(len(out)) > maxOut {
		return nil, fmt.Errorf("%w: %d bytes > bound %d", ErrAtempoOutputTooLarge, len(out), maxOut)
	}
	if _, err := ParseWAVHeader(out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAtempoOutputInvalid, err)
	}
	return out, nil
}
