package media_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/media"
)

func TestAtempoFilterArgs_OnlyOneFilterWithFiniteNumericArg(t *testing.T) {
	valid := []float64{1.001, 1.05, 1.10, 1.20, 1.25}
	for _, f := range valid {
		args, err := media.AtempoFilterArgs(f)
		if err != nil {
			t.Fatalf("expected factor %v to be valid, got err: %v", f, err)
		}
		// Invariant: exactly -vn -af atempo=<factor> -c:a pcm_s16le
		if len(args) != 5 || args[0] != "-vn" || args[1] != "-af" || args[3] != "-c:a" || args[4] != "pcm_s16le" {
			t.Fatalf("unexpected filter args structure: %v", args)
		}
		if !strings.Contains(args[2], "atempo=") {
			t.Fatalf("expected atempo filter token, got %s", args[2])
		}
		// Never crop, atrim, rubberband or generic chain
		for _, forbidden := range []string{"atrim", "asetpts", "crop", "rubberband", ","} {
			if strings.Contains(args[2], forbidden) {
				t.Fatalf("filter args must not contain %q: %s", forbidden, args[2])
			}
		}
	}

	invalid := []float64{0, -1.0, 0.99, 1.0, 1.250001, 1.26, 1.5, 2.0}
	for _, f := range invalid {
		if _, err := media.AtempoFilterArgs(f); err == nil || !errors.Is(err, media.ErrAtempoFactorOutOfRange) {
			t.Fatalf("expected ErrAtempoFactorOutOfRange for %v, got: %v", f, err)
		}
	}
}

func TestApplyAtempoWAV_RealFFmpegTransform(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed on test host")
	}

	ctx := context.Background()
	// Generate 1000ms 16kHz mono WAV
	sourceWAV := media.GeneratePCM16WAV(16000, 1, 1000)

	// Case 1: factor just above 1 (1.05)
	res1, err := media.ApplyAtempoWAV(ctx, media.AtempoRequest{
		SourceWAV: sourceWAV,
		Factor:    1.05,
	})
	if err != nil {
		t.Fatalf("transform factor 1.05 failed: %v", err)
	}
	dur1, err := media.ProbeWAVBytes(res1)
	if err != nil {
		t.Fatalf("probe result 1.05: %v", err)
	}
	// 1000ms / 1.05 ≈ 952ms (allow +/- 30ms probe leeway, no crop)
	if dur1 < 920 || dur1 > 980 {
		t.Fatalf("expected duration around 952ms, got %dms", dur1)
	}

	// Case 2: factor exactly 1.25
	res2, err := media.ApplyAtempoWAV(ctx, media.AtempoRequest{
		SourceWAV: sourceWAV,
		Factor:    1.25,
	})
	if err != nil {
		t.Fatalf("transform factor 1.25 failed: %v", err)
	}
	dur2, err := media.ProbeWAVBytes(res2)
	if err != nil {
		t.Fatalf("probe result 1.25: %v", err)
	}
	// 1000ms / 1.25 = 800ms
	if dur2 < 770 || dur2 > 830 {
		t.Fatalf("expected duration around 800ms, got %dms", dur2)
	}

	// Case 3: factor above 1.25 (e.g. 1.26) must be rejected before execution
	_, err = media.ApplyAtempoWAV(ctx, media.AtempoRequest{
		SourceWAV: sourceWAV,
		Factor:    1.26,
	})
	if err == nil || !errors.Is(err, media.ErrAtempoFactorOutOfRange) {
		t.Fatalf("expected ErrAtempoFactorOutOfRange for factor 1.26, got: %v", err)
	}

	// Case 4: factor <= 1 (e.g. 1.00) rejected
	_, err = media.ApplyAtempoWAV(ctx, media.AtempoRequest{
		SourceWAV: sourceWAV,
		Factor:    1.00,
	})
	if err == nil || !errors.Is(err, media.ErrAtempoFactorOutOfRange) {
		t.Fatalf("expected ErrAtempoFactorOutOfRange for factor 1.00, got: %v", err)
	}
}

func TestApplyAtempoWAV_Cancellation(t *testing.T) {
	// Pre-canceled guard: proves the early exit before any filesystem or subprocess work.
	t.Run("pre_canceled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		sourceWAV := media.GeneratePCM16WAV(16000, 1, 1000)
		_, err := media.ApplyAtempoWAV(ctx, media.AtempoRequest{
			SourceWAV: sourceWAV,
			Factor:    1.10,
		})
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled error, got: %v", err)
		}
	})

	// Running subprocess: proves exec.CommandContext kills a running child process, removes the
	// transform's own temporary working directory, and returns context.Canceled. Uses a
	// controllable helper so the test is deterministic, cross-platform and does not depend on
	// host ffmpeg.
	t.Run("running_subprocess_killed_and_cleaned", func(t *testing.T) {
		helperBin := buildAtempoHelper(t)
		markerFile := filepath.Join(t.TempDir(), "started.marker")
		t.Setenv("ATEMPO_HELPER_MARKER", markerFile)
		workingDirsBefore := atempoWorkingDirs(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		sourceWAV := media.GeneratePCM16WAV(16000, 1, 1000)
		go func() {
			_, err := media.ApplyAtempoWAV(ctx, media.AtempoRequest{
				SourceWAV:  sourceWAV,
				Factor:     1.10,
				FFmpegPath: helperBin,
			})
			done <- err
		}()

		// Wait until the helper subprocess has actually started and written its marker.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(markerFile); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("helper subprocess did not start within deadline")
			}
			time.Sleep(10 * time.Millisecond)
		}

		// The subprocess is running right now. Cancel context and prove it is killed.
		cancel()

		select {
		case err := <-done:
			if err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled, got: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ApplyAtempoWAV did not return after cancellation")
		}

		if after := atempoWorkingDirs(t); after != workingDirsBefore {
			t.Fatalf("a cancelled transform left %d temporary working dir(s) behind: before=%d after=%d", after-workingDirsBefore, workingDirsBefore, after)
		}
	})
}

// atempoWorkingDirs counts the temporary working directories of this transform that currently
// exist, so a cancelled or failed transform is proven to remove its own.
func atempoWorkingDirs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read the temporary directory: %v", err)
	}
	dirs := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "douyinie_atempo") {
			dirs++
		}
	}
	return dirs
}

// buildAtempoHelper compiles the controllable ffmpeg stand-in helper once per test run.
func buildAtempoHelper(t *testing.T) string {
	t.Helper()
	outDir := t.TempDir()
	outBin := filepath.Join(outDir, "atempohelper")
	if runtime.GOOS == "windows" {
		outBin += ".exe"
	}
	src := filepath.Join("testdata", "atempohelper", "main.go")
	cmd := exec.Command("go", "build", "-o", outBin, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build atempo helper (%s): %v\n%s", src, err, string(out))
	}
	return outBin
}

func TestApplyAtempoWAV_OversizedOutputBounded(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed on test host")
	}

	sourceWAV := media.GeneratePCM16WAV(16000, 1, 1000)
	// Bounded max bytes = 100 bytes (WAV header alone is 44 bytes, audio is >30000 bytes)
	const maxOut int64 = 100
	_, err := media.ApplyAtempoWAV(context.Background(), media.AtempoRequest{
		SourceWAV:      sourceWAV,
		Factor:         1.10,
		MaxOutputBytes: maxOut,
	})
	if err == nil || !errors.Is(err, media.ErrAtempoOutputTooLarge) {
		t.Fatalf("expected ErrAtempoOutputTooLarge, got: %v", err)
	}
	// The write is bounded inside the subprocess too (-fs), so the refusal can only report a
	// size within one encoder packet of the ceiling. Without that bound the same case reports
	// the full ~30 KB transform, which is what this assertion discriminates.
	size := int64(-1)
	for _, field := range strings.Fields(err.Error()) {
		if n, convErr := strconv.ParseInt(field, 10, 64); convErr == nil {
			size = n
			break
		}
	}
	if size < 0 {
		t.Fatalf("expected the refusal to report the observed output size, got: %v", err)
	}
	if size > maxOut+8192 {
		t.Fatalf("the transform write was not bounded during execution: reported %d bytes against a %d byte ceiling", size, maxOut)
	}
}

func TestApplyAtempoWAV_InvalidSource(t *testing.T) {
	// Empty source
	_, err := media.ApplyAtempoWAV(context.Background(), media.AtempoRequest{
		SourceWAV: nil,
		Factor:    1.10,
	})
	if err == nil || !errors.Is(err, media.ErrAtempoSourceInvalid) {
		t.Fatalf("expected ErrAtempoSourceInvalid on empty source, got: %v", err)
	}

	// Corrupted WAV bytes
	_, err = media.ApplyAtempoWAV(context.Background(), media.AtempoRequest{
		SourceWAV: []byte("RIFFgarbage_not_a_real_wav_file"),
		Factor:    1.10,
	})
	if err == nil || !errors.Is(err, media.ErrAtempoSourceInvalid) {
		t.Fatalf("expected ErrAtempoSourceInvalid on corrupt header, got: %v", err)
	}

	// Test ffmpeg missing failure explicitly without depending on host state
	sourceWAV := media.GeneratePCM16WAV(16000, 1, 500)
	_, err = media.ApplyAtempoWAV(context.Background(), media.AtempoRequest{
		SourceWAV:  sourceWAV,
		Factor:     1.10,
		FFmpegPath: "nonexistent_ffmpeg_binary_404_xyz",
	})
	if err == nil || !errors.Is(err, media.ErrAtempoUnavailable) {
		t.Fatalf("expected ErrAtempoUnavailable when ffmpeg binary is missing, got: %v", err)
	}
}
