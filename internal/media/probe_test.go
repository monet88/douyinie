package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMockProber(t *testing.T) {
	ctx := context.Background()
	prober := &MockProber{}
	report, err := prober.Probe(ctx, "fake_path.mp4", "")
	if err != nil {
		t.Fatalf("MockProber failed: %v", err)
	}

	if !report.ContainerValid || !report.FingerprintMatch {
		t.Errorf("expected container valid and fingerprint match true, got %+v", report)
	}
	if report.DurationMs != 10000 {
		t.Errorf("expected 10000ms duration, got %d", report.DurationMs)
	}
}

func TestFFprobeProber_RealMedia(t *testing.T) {
	// Skip if ffprobe or ffmpeg is not in PATH
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available in PATH, skipping real media probe test")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available in PATH, skipping real media probe test")
	}

	tmpDir := t.TempDir()
	sampleVideo := filepath.Join(tmpDir, "test_synth.mp4")

	// Generate 1-second synthetic MP4 using ffmpeg testsrc and sine wave audio
	cmd := exec.Command("ffmpeg",
		"-y",
		"-f", "lavfi", "-i", "testsrc=duration=1.0:size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1.0",
		"-c:v", "libx264",
		"-c:a", "aac",
		sampleVideo,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate synthetic video: %v, output: %s", err, string(out))
	}

	prober := NewFFprobeProber()
	report, err := prober.Probe(context.Background(), sampleVideo, "")
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	if !report.ContainerValid {
		t.Errorf("expected container valid, errors: %v", report.Errors)
	}
	if !report.FingerprintMatch {
		t.Errorf("expected fingerprint match")
	}
	if report.Width != 320 || report.Height != 240 {
		t.Errorf("expected 320x240 resolution, got %dx%d", report.Width, report.Height)
	}
	if report.VideoCodec != "h264" {
		t.Errorf("expected h264 video codec, got %s", report.VideoCodec)
	}
	if report.AudioCodec != "aac" {
		t.Errorf("expected aac audio codec, got %s", report.AudioCodec)
	}
	if report.DurationMs < 900 || report.DurationMs > 1100 {
		t.Errorf("expected approx 1000ms duration, got %d", report.DurationMs)
	}
}

func TestFFprobeProber_CorruptFile(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available in PATH")
	}

	tmpDir := t.TempDir()
	corruptFile := filepath.Join(tmpDir, "corrupt.mp4")
	if err := os.WriteFile(corruptFile, []byte("NOT_A_VALID_MEDIA_FILE_HEADER"), 0644); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	prober := NewFFprobeProber()
	report, err := prober.Probe(context.Background(), corruptFile, "")
	if err != nil {
		t.Fatalf("Probe unexpectedly returned hard error: %v", err)
	}

	if report.ContainerValid {
		t.Errorf("expected corrupt file to fail container validity, got true")
	}
	if len(report.Errors) == 0 {
		t.Errorf("expected errors in report for corrupt file")
	}
}
