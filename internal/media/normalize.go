package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// NormalizeAudio16kMono extracts and normalizes audio from sourcePath to a 16 kHz mono WAV PCM file at outPath.
// Normalization fails closed if sourcePath is empty, ffmpeg is missing, or extraction fails.
func NormalizeAudio16kMono(ctx context.Context, sourcePath string, outPath string) error {
	if strings.TrimSpace(sourcePath) == "" {
		return fmt.Errorf("source path is required")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-i", sourcePath,
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "16000",
		"-ac", "1",
		outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg audio normalization failed: %w; stderr: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
