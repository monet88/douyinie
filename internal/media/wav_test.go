package media_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monet88/douyinie/internal/media"
)

func TestGenerateAndProbeWAV(t *testing.T) {
	testDurations := []int64{100, 500, 1000, 1500, 2345, 5000}
	sampleRate := 16000
	channels := 1

	for _, expectedMs := range testDurations {
		wavData := media.GeneratePCM16WAV(sampleRate, channels, expectedMs)
		if len(wavData) < 44 {
			t.Fatalf("WAV data too short: %d bytes", len(wavData))
		}

		probedMs, err := media.ProbeWAVBytes(wavData)
		if err != nil {
			t.Fatalf("failed to probe WAV bytes for %dms: %v", expectedMs, err)
		}

		if probedMs != expectedMs {
			t.Errorf("expected %dms, got %dms", expectedMs, probedMs)
		}

		// Also test file probing
		tmpDir := t.TempDir()
		tmpPath := filepath.Join(tmpDir, "test.wav")
		if err := os.WriteFile(tmpPath, wavData, 0644); err != nil {
			t.Fatalf("write tmp wav: %v", err)
		}

		fileProbedMs, err := media.ProbeAudioFileDuration(context.Background(), tmpPath)
		if err != nil {
			t.Fatalf("failed to probe audio file duration: %v", err)
		}
		if fileProbedMs != expectedMs {
			t.Errorf("file probed: expected %dms, got %dms", expectedMs, fileProbedMs)
		}
	}
}

func TestProbeWAV_InvalidHeaders(t *testing.T) {
	// Truncated
	_, err := media.ProbeWAVBytes([]byte("RIFF"))
	if err == nil {
		t.Error("expected error on truncated data")
	}

	// Not a RIFF
	_, err = media.ProbeWAVBytes(make([]byte, 100))
	if err == nil {
		t.Error("expected error on zero bytes")
	}
}
