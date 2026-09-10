package provider_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/worker"
)

// Item 13: FakeSeparator must preserve source gaps (background-by-default) rather than zeroing background.
func TestFakeSeparatorProvider_PreservesSourceGaps_BackgroundByDefault(t *testing.T) {
	p := provider.NewFakeSeparatorProvider("fake_uvr_test")
	ctx := context.Background()

	// Create 3 seconds of audio:
	// 0.0 - 1.0s: dialogue
	// 1.0 - 2.0s: gap (uncovered by AudioRolePlan)
	// 2.0 - 3.0s: dialogue
	sampleRate := 16000
	channels := 1
	totalSamples := 3 * sampleRate
	srcSamples := make([]int16, totalSamples)
	for i := range srcSamples {
		srcSamples[i] = int16(1000 + i%500)
	}

	srcWAV := media.EncodePCM16Samples(srcSamples, sampleRate, channels)
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source.wav")
	if err := os.WriteFile(srcPath, srcWAV, 0644); err != nil {
		t.Fatalf("write source wav: %v", err)
	}

	plan := &domain.AudioRolePlan{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2000, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
	}

	res, err := p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID: "test_asset",
		SourceAudio: worker.ArtifactRef{
			Path: srcPath,
		},
		AudioRolePlan: plan,
	})
	if err != nil {
		t.Fatalf("SeparateStems failed: %v", err)
	}

	bgSamples, _, err := media.ExtractPCM16Samples(res.BackgroundWAV)
	if err != nil {
		t.Fatalf("extract bg samples: %v", err)
	}
	vocalSamples, _, err := media.ExtractPCM16Samples(res.VocalsWAV)
	if err != nil {
		t.Fatalf("extract vocal samples: %v", err)
	}

	// 1. Spoken span 1 (0.0-1.0s): vocalSamples must match source; bgSamples must be 0
	for i := 0; i < 16000; i++ {
		if vocalSamples[i] != srcSamples[i] {
			t.Fatalf("sample %d: expected vocal sample %d, got %d", i, srcSamples[i], vocalSamples[i])
		}
		if bgSamples[i] != 0 {
			t.Fatalf("sample %d: expected bg sample 0 on spoken span, got %d", i, bgSamples[i])
		}
	}

	// 2. Uncovered gap span (1.0-2.0s): vocalSamples must be 0; bgSamples MUST PRESERVE source (background-by-default)!
	for i := 16000; i < 32000; i++ {
		if vocalSamples[i] != 0 {
			t.Fatalf("sample %d: expected 0 vocals on gap span, got %d", i, vocalSamples[i])
		}
		if bgSamples[i] != srcSamples[i] {
			t.Fatalf("sample %d: background-by-default violated! expected %d, got %d", i, srcSamples[i], bgSamples[i])
		}
	}

	// 3. Spoken span 2 (2.0-3.0s): vocalSamples must match source; bgSamples must be 0
	for i := 32000; i < 48000; i++ {
		if vocalSamples[i] != srcSamples[i] {
			t.Fatalf("sample %d: expected vocal sample %d, got %d", i, srcSamples[i], vocalSamples[i])
		}
		if bgSamples[i] != 0 {
			t.Fatalf("sample %d: expected bg sample 0 on spoken span, got %d", i, bgSamples[i])
		}
	}
}

// Item 14: FakeSeparator must handle stereo/multichannel by frames, not byte arithmetic.
func TestFakeSeparatorProvider_MultichannelStereoByFrames(t *testing.T) {
	p := provider.NewFakeSeparatorProvider("fake_uvr_stereo")
	ctx := context.Background()

	sampleRate := 16000
	channels := 2
	totalFrames := 2 * sampleRate
	totalSamples := totalFrames * channels
	srcSamples := make([]int16, totalSamples)
	for frame := range totalFrames {
		srcSamples[frame*2] = int16(1000 + frame%100)   // Left channel
		srcSamples[frame*2+1] = int16(2000 + frame%100) // Right channel
	}

	srcWAV := media.EncodePCM16Samples(srcSamples, sampleRate, channels)
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "stereo_source.wav")
	if err := os.WriteFile(srcPath, srcWAV, 0644); err != nil {
		t.Fatalf("write stereo wav: %v", err)
	}

	plan := &domain.AudioRolePlan{
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		},
	}

	res, err := p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID: "test_stereo_asset",
		SourceAudio: worker.ArtifactRef{
			Path: srcPath,
		},
		AudioRolePlan: plan,
	})
	if err != nil {
		t.Fatalf("SeparateStems failed: %v", err)
	}

	if res.Channels != 2 {
		t.Fatalf("expected 2 channels, got %d", res.Channels)
	}

	vocalSamples, info, err := media.ExtractPCM16Samples(res.VocalsWAV)
	if err != nil {
		t.Fatalf("extract vocals: %v", err)
	}
	if int(info.NumChannels) != 2 {
		t.Fatalf("expected 2 channels in wav header, got %d", info.NumChannels)
	}
	if len(vocalSamples) != totalSamples {
		t.Fatalf("expected %d total interleaved samples, got %d", totalSamples, len(vocalSamples))
	}

	// Verify both channels are preserved on spoken frame
	for frame := range 16000 {
		if vocalSamples[frame*2] != srcSamples[frame*2] {
			t.Fatalf("frame %d left channel mismatch: expected %d, got %d", frame, srcSamples[frame*2], vocalSamples[frame*2])
		}
		if vocalSamples[frame*2+1] != srcSamples[frame*2+1] {
			t.Fatalf("frame %d right channel mismatch: expected %d, got %d", frame, srcSamples[frame*2+1], vocalSamples[frame*2+1])
		}
	}
}

// Item 15: FakeSeparator must fail closed when explicit SourceAudio.Path cannot be read or is corrupt.
func TestFakeSeparatorProvider_UnreadableOrCorruptSourceFailsClosed(t *testing.T) {
	p := provider.NewFakeSeparatorProvider("fake_uvr_error_test")
	ctx := context.Background()

	// 1. Explicit path points to nonexistent file -> fails closed
	_, err := p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID: "asset_nonexistent",
		SourceAudio: worker.ArtifactRef{
			Path: "F:/nonexistent/path/to/missing_source.wav",
		},
	})
	if err == nil {
		t.Fatal("expected error on nonexistent source audio path, got nil")
	}

	// 2. Explicit path points to corrupt non-WAV bytes -> fails closed
	tmpDir := t.TempDir()
	corruptPath := filepath.Join(tmpDir, "corrupt.wav")
	if err := os.WriteFile(corruptPath, []byte("NOT_A_VALID_WAV_HEADER_DATA_GARBAGE"), 0644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	_, err = p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID: "asset_corrupt",
		SourceAudio: worker.ArtifactRef{
			Path: corruptPath,
		},
	})
	if err == nil {
		t.Fatal("expected error on corrupt PCM/WAV source audio, got nil")
	}

	// 3. Non-.wav missing path -> fails closed
	_, err = p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID: "asset_nonexistent_mp3",
		SourceAudio: worker.ArtifactRef{
			Path: filepath.Join(tmpDir, "missing_source.mp3"),
		},
	})
	if err == nil {
		t.Fatal("expected error on nonexistent non-.wav source audio path, got nil")
	}

	// 4. Existing unsupported format file (e.g. MP3 / non-RIFF) -> fails closed
	unsupportedPath := filepath.Join(tmpDir, "source.mp3")
	if err := os.WriteFile(unsupportedPath, []byte("ID3\x03\x00\x00\x00\x00\x00#TSSE\x00\x00\x00\x0f\x00\x00\x03Lavf58.29.100\xff\xfb\x90d"), 0644); err != nil {
		t.Fatalf("write unsupported format file: %v", err)
	}
	_, err = p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID: "asset_unsupported_mp3",
		SourceAudio: worker.ArtifactRef{
			Path: unsupportedPath,
		},
	})
	if err == nil {
		t.Fatal("expected error on existing unsupported format source audio path, got nil")
	}

	// 5. Truly absent source audio (Path == "") -> synthesizes without failing
	res, err := p.SeparateStems(ctx, provider.SeparationRequest{
		AssetID:     "asset_absent",
		SourceAudio: worker.ArtifactRef{},
	})
	if err != nil {
		t.Fatalf("expected successful synthesis when SourceAudio is truly absent, got %v", err)
	}
	if len(res.VocalsWAV) == 0 || len(res.BackgroundWAV) == 0 {
		t.Fatal("expected non-empty synthesized stems")
	}
}
