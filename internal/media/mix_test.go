package media_test

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/monet88/douyinie/internal/media"
)

func TestMedia_WAVHeaderParsingAndExtraction(t *testing.T) {
	durationMs := int64(1000)
	sampleRate := 16000
	channels := 1

	wavBytes := media.GeneratePCM16WAV(sampleRate, channels, durationMs)
	if len(wavBytes) == 0 {
		t.Fatalf("expected non-empty WAV bytes")
	}

	probedDur, err := media.ProbeWAVBytes(wavBytes)
	if err != nil {
		t.Fatalf("probe wav bytes failed: %v", err)
	}
	if probedDur != durationMs {
		t.Errorf("expected %dms duration, got %dms", durationMs, probedDur)
	}

	samples, info, err := media.ExtractPCM16Samples(wavBytes)
	if err != nil {
		t.Fatalf("extract pcm16 samples failed: %v", err)
	}
	if info.SampleRate != uint32(sampleRate) || info.NumChannels != uint16(channels) {
		t.Errorf("unexpected header info: %+v", info)
	}
	expectedNumSamples := (sampleRate * int(durationMs)) / 1000
	if len(samples) != expectedNumSamples {
		t.Errorf("expected %d samples, got %d", expectedNumSamples, len(samples))
	}

	encoded := media.EncodePCM16Samples(samples, sampleRate, channels)
	reprobed, err := media.ProbeWAVBytes(encoded)
	if err != nil {
		t.Fatalf("reprobe encoded wav bytes failed: %v", err)
	}
	if reprobed != durationMs {
		t.Errorf("expected %dms reprobed duration, got %dms", durationMs, reprobed)
	}
}

func TestMedia_MixPCM16_DialogueSuppressionAndCrossfade(t *testing.T) {
	sampleRate := 16000
	channels := 1
	durationMs := int64(2000)
	totalSamples := (sampleRate * int(durationMs)) / 1000

	// 1. Create background samples with constant amplitude (e.g. 10000)
	bgSamples := make([]int16, totalSamples)
	for i := range bgSamples {
		bgSamples[i] = 10000
	}

	// 2. Speech clip covering [500ms, 1000ms] with amplitude 5000
	clipSamples := make([]int16, (sampleRate*500)/1000)
	for i := range clipSamples {
		clipSamples[i] = 5000
	}
	clips := []media.DubSpeechClip{
		{
			StartMs:  500,
			Channels: 1,
			Samples:  clipSamples,
		},
	}

	// 3. Suppression window [500ms, 1000ms] with dialogue suppression and -6dB ducking
	suppressWindows := []media.PreservationWindowInterval{
		{
			StartMs: 500,
			EndMs:   1000,
			Action:  "suppress_dialogue",
		},
	}

	crossfadeMs := int64(25)
	duckingGainDb := -6.0 // factor ~0.501

	mixed := media.MixPCM16(bgSamples, sampleRate, channels, clips, suppressWindows, crossfadeMs, duckingGainDb)
	if len(mixed) != totalSamples {
		t.Fatalf("expected %d mixed samples, got %d", totalSamples, len(mixed))
	}

	// Check sample before window (e.g. at 200ms) -> should be original 10000
	sampleAt200ms := mixed[(sampleRate*200)/1000]
	if sampleAt200ms != 10000 {
		t.Errorf("expected sample at 200ms to be 10000, got %d", sampleAt200ms)
	}

	// Check sample inside window (e.g. at 750ms) -> ducked bg (~5011) + speech clip (5000) = ~10011
	sampleAt750ms := mixed[(sampleRate*750)/1000]
	duckedExpected := int16(10000.0 * math.Pow(10.0, -6.0/20.0))
	expectedInside := duckedExpected + 5000
	if math.Abs(float64(sampleAt750ms-expectedInside)) > 50 {
		t.Errorf("expected sample at 750ms to be ~%d, got %d", expectedInside, sampleAt750ms)
	}

	// Check sample after window (e.g. at 1500ms) -> should be original 10000
	sampleAt1500ms := mixed[(sampleRate*1500)/1000]
	if sampleAt1500ms != 10000 {
		t.Errorf("expected sample at 1500ms to be 10000, got %d", sampleAt1500ms)
	}
}

func TestMedia_ResamplePCM16_SampleRatesAndChannels(t *testing.T) {
	// Test 1: 24kHz mono -> 16kHz mono (e.g. CosyVoice TTS -> 16kHz mixer)
	inRate := 24000
	inChannels := 1
	durationMs := int64(1000)
	inFrames := (inRate * int(durationMs)) / 1000
	inSamples := make([]int16, inFrames)
	for i := range inSamples {
		inSamples[i] = 12000
	}

	outRate := 16000
	outChannels := 1
	resampled := media.ResamplePCM16(inSamples, inRate, inChannels, outRate, outChannels)
	expectedOutFrames := (outRate * int(durationMs)) / 1000
	if len(resampled) != expectedOutFrames {
		t.Fatalf("expected %d resampled samples, got %d", expectedOutFrames, len(resampled))
	}
	if resampled[len(resampled)/2] != 12000 {
		t.Errorf("expected constant value 12000 preserved across resampling, got %d", resampled[len(resampled)/2])
	}

	// Test 2: Mono to Stereo channel expansion
	monoToStereo := media.ResamplePCM16(inSamples[:100], 16000, 1, 16000, 2)
	if len(monoToStereo) != 200 {
		t.Fatalf("expected 200 samples for stereo expansion, got %d", len(monoToStereo))
	}
	if monoToStereo[0] != 12000 || monoToStereo[1] != 12000 {
		t.Errorf("expected both channels to have 12000, got L:%d R:%d", monoToStereo[0], monoToStereo[1])
	}

	// Test 3: Stereo to Mono channel downmix
	stereoSamples := []int16{1000, 3000, 2000, 4000} // L, R, L, R
	stereoToMono := media.ResamplePCM16(stereoSamples, 16000, 2, 16000, 1)
	if len(stereoToMono) != 2 {
		t.Fatalf("expected 2 mono samples, got %d", len(stereoToMono))
	}
	if stereoToMono[0] != 2000 || stereoToMono[1] != 3000 {
		t.Errorf("expected averaged mono [2000, 3000], got %+v", stereoToMono)
	}
}

func TestMedia_MixPCM16Stems_SingingPreservationAndDialogueSuppression(t *testing.T) {
	sampleRate := 16000
	channels := 1
	durationMs := int64(3000)
	totalSamples := (sampleRate * int(durationMs)) / 1000

	// 1. Background stem: constant BGM amplitude 1000
	bgSamples := make([]int16, totalSamples)
	for i := range bgSamples {
		bgSamples[i] = 1000
	}

	// 2. Vocals stem: contains singing in [0ms, 1000ms] (amplitude 3000), spoken dialogue in [1000ms, 2000ms] (amplitude 4000), and outro in [2000ms, 3000ms] (amplitude 2000)
	vocalsSamples := make([]int16, totalSamples)
	for i := 0; i < (sampleRate*1000)/1000; i++ {
		vocalsSamples[i] = 3000
	}
	for i := (sampleRate * 1000) / 1000; i < (sampleRate*2000)/1000; i++ {
		vocalsSamples[i] = 4000
	}
	for i := (sampleRate * 2000) / 1000; i < totalSamples; i++ {
		vocalsSamples[i] = 2000
	}

	// 3. Speech window [1000ms, 2000ms] (spoken dialogue to suppress)
	suppressWindows := []media.PreservationWindowInterval{
		{
			StartMs: 1000,
			EndMs:   2000,
			Action:  "suppress_dialogue",
		},
	}

	// 4. TTS speech clip at 24kHz sample rate (different from mixer 16kHz) with amplitude 5000 in [1000ms, 2000ms]
	clipRate := 24000
	clipSamples := make([]int16, (clipRate*1000)/1000)
	for i := range clipSamples {
		clipSamples[i] = 5000
	}
	clips := []media.DubSpeechClip{
		{
			StartMs:    1000,
			SampleRate: clipRate,
			Channels:   1,
			Samples:    clipSamples,
		},
	}

	crossfadeMs := int64(25)
	duckingGainDb := 0.0 // no bg ducking for exact math verification

	mixed := media.MixPCM16Stems(bgSamples, vocalsSamples, sampleRate, channels, clips, suppressWindows, crossfadeMs, duckingGainDb)
	if len(mixed) != totalSamples {
		t.Fatalf("expected %d mixed samples, got %d", totalSamples, len(mixed))
	}

	// Window 1: [0ms, 1000ms] Singing vocal window (outside speech window)
	// -> Background (1000) + Singing vocals (3000) = 4000
	sampleAt500ms := mixed[(sampleRate*500)/1000]
	if sampleAt500ms != 4000 {
		t.Errorf("expected singing vocals preserved outside speech window (sample at 500ms = 4000), got %d", sampleAt500ms)
	}

	// Window 2: [1000ms, 2000ms] Speech window (dialogue suppressed, TTS clip overlaid)
	// -> Background (1000) + Suppressed dialogue (0) + Resampled TTS (5000) = 6000
	sampleAt1500ms := mixed[(sampleRate*1500)/1000]
	if math.Abs(float64(sampleAt1500ms-6000)) > 50 {
		t.Errorf("expected source dialogue suppressed and TTS clip overlaid (sample at 1500ms = ~6000), got %d", sampleAt1500ms)
	}

	// Window 3: [2000ms, 3000ms] Outro vocals window (outside speech window)
	// -> Background (1000) + Outro vocals (2000) = 3000
	sampleAt2500ms := mixed[(sampleRate*2500)/1000]
	if sampleAt2500ms != 3000 {
		t.Errorf("expected outro vocals preserved (sample at 2500ms = 3000), got %d", sampleAt2500ms)
	}
}

func TestMedia_ParseWAVHeader_RejectsDataChunkBeyondAvailableBytes(t *testing.T) {
	// The truncation is header-valid: the complete RIFF/WAVE/fmt chunk (including the
	// declared data chunk size) survives, only the audio bytes are cut off. Header
	// validation alone cannot catch this, so the declared-vs-present size mismatch
	// must fail closed instead of yielding phantom audio/duration downstream.
	//
	// Fixture validity is proven structurally from the bytes themselves, never by
	// asking a parser to accept corrupt input.
	const dataChunkSizeOffset = 40 // RIFF(12) + fmt chunk(24) + "data"(4)
	full := media.GeneratePCM16WAV(24000, 2, 1000)
	truncated := append([]byte(nil), full[:len(full)-4096]...)

	if string(truncated[0:4]) != "RIFF" || string(truncated[8:12]) != "WAVE" {
		t.Fatalf("fixture must retain the RIFF/WAVE magic, got % x", truncated[0:12])
	}
	declared := int64(binary.LittleEndian.Uint32(truncated[dataChunkSizeOffset : dataChunkSizeOffset+4]))
	available := int64(len(truncated) - (dataChunkSizeOffset + 4))
	if declared != int64(len(full)-(dataChunkSizeOffset+4)) {
		t.Fatalf("fixture must retain the intact container's declared data size %d, got %d", len(full)-(dataChunkSizeOffset+4), declared)
	}
	if declared <= available {
		t.Fatalf("fixture must declare more audio bytes (%d) than it carries (%d)", declared, available)
	}

	if _, err := media.ParseWAVHeader(truncated); !errors.Is(err, media.ErrInvalidWAVHeader) {
		t.Fatalf("expected ErrInvalidWAVHeader for a data chunk larger than the available bytes, got %v", err)
	}
	if _, err := media.ProbeWAVBytes(truncated); !errors.Is(err, media.ErrInvalidWAVHeader) {
		t.Fatalf("expected duration probing to inherit the parser's fail-closed verdict, got %v", err)
	}
	if _, _, err := media.ExtractPCM16Samples(truncated); !errors.Is(err, media.ErrInvalidWAVHeader) {
		t.Fatalf("expected sample extraction to fail closed on truncated media, got %v", err)
	}

	info, err := media.ParseWAVHeader(full)
	if err != nil {
		t.Fatalf("intact container must still parse: %v", err)
	}
	if info.DurationMs != 1000 {
		t.Errorf("expected 1000ms duration on the intact container, got %dms", info.DurationMs)
	}
}

// The window arithmetic is the single rule the fit controller and the mixer share, so its
// frame boundaries are pinned here by value rather than through either caller.
func TestMedia_PlaybackWindowFrames_FloorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		startMs    int64
		endMs      int64
		sampleRate int
		wantFrames int64
	}{
		{name: "unknown rate yields no window", startMs: 0, endMs: 1000, sampleRate: 0, wantFrames: 0},
		{name: "empty window", startMs: 1000, endMs: 1000, sampleRate: 44100, wantFrames: 0},
		{name: "inverted window", startMs: 2000, endMs: 1000, sampleRate: 44100, wantFrames: 0},
		{name: "one second at 44.1kHz", startMs: 0, endMs: 1000, sampleRate: 44100, wantFrames: 44100},
		{name: "shifted one second at 44.1kHz", startMs: 1000, endMs: 2000, sampleRate: 44100, wantFrames: 44100},
		{name: "one second at 16kHz", startMs: 1000, endMs: 2000, sampleRate: 16000, wantFrames: 16000},
		{name: "each edge floors a partial frame", startMs: 1, endMs: 2, sampleRate: 44100, wantFrames: 44},
		{name: "borrowed window", startMs: 1000, endMs: 2400, sampleRate: 44100, wantFrames: 61740},
		{name: "window shorter than one frame", startMs: 0, endMs: 1, sampleRate: 10, wantFrames: 0},
	} {
		if got := media.PlaybackWindowFrames(tc.startMs, tc.endMs, tc.sampleRate); got != tc.wantFrames {
			t.Errorf("%s: PlaybackWindowFrames(%d, %d, %d) = %d, want %d", tc.name, tc.startMs, tc.endMs, tc.sampleRate, got, tc.wantFrames)
		}
	}
}

// The fit controller gates a resampled candidate on ResampledPCM16Frames while the mixer places
// what ResamplePCM16 actually allocated, so the two must never drift - including on ratios whose
// product is not a whole number of frames.
func TestMedia_ResampledPCM16Frames_MatchesResampleAllocation(t *testing.T) {
	for _, tc := range []struct {
		inFrames   int64
		inRate     int
		outRate    int
		wantFrames int64
	}{
		{inFrames: 16000, inRate: 16000, outRate: 44100, wantFrames: 44100},
		{inFrames: 16001, inRate: 16000, outRate: 44100, wantFrames: 44103},
		{inFrames: 44100, inRate: 44100, outRate: 16000, wantFrames: 16000},
		{inFrames: 44101, inRate: 44100, outRate: 16000, wantFrames: 16000},
		{inFrames: 1000, inRate: 44100, outRate: 44100, wantFrames: 1000},
		{inFrames: 19744, inRate: 16000, outRate: 48000, wantFrames: 59232},
		{inFrames: 0, inRate: 44100, outRate: 16000, wantFrames: 0},
		{inFrames: 500, inRate: 44100, outRate: 1, wantFrames: 0},
	} {
		if got := media.ResampledPCM16Frames(tc.inFrames, tc.inRate, tc.outRate); got != tc.wantFrames {
			t.Errorf("ResampledPCM16Frames(%d, %d, %d) = %d, want %d", tc.inFrames, tc.inRate, tc.outRate, got, tc.wantFrames)
		}

		samples := make([]int16, tc.inFrames)
		allocated := int64(len(media.ResamplePCM16(samples, tc.inRate, 1, tc.outRate, 1)))
		if allocated != tc.wantFrames {
			t.Errorf("ResamplePCM16(%d frames, %d->%d) allocated %d frames, want %d", tc.inFrames, tc.inRate, tc.outRate, allocated, tc.wantFrames)
		}
	}
}

// A header-only geometry probe must report exactly the frame count and rate the decoding reader
// yields, otherwise the fit and the mixer would disagree on the same candidate waveform.
func TestMedia_WAVFrameGeometry_MatchesDecodedSamples(t *testing.T) {
	for _, tc := range []struct {
		sampleRate int
		channels   int
		durationMs int64
		wantFrames int64
	}{
		{sampleRate: 44100, channels: 1, durationMs: 1000, wantFrames: 44100},
		{sampleRate: 16000, channels: 2, durationMs: 1234, wantFrames: 19744},
		{sampleRate: 8000, channels: 1, durationMs: 1, wantFrames: 8},
	} {
		wav := media.GeneratePCM16WAV(tc.sampleRate, tc.channels, tc.durationMs)
		frames, rate, err := media.WAVFrameGeometry(wav)
		if err != nil {
			t.Fatalf("WAVFrameGeometry(%dHz/%dch/%dms) failed: %v", tc.sampleRate, tc.channels, tc.durationMs, err)
		}
		if frames != tc.wantFrames || rate != tc.sampleRate {
			t.Errorf("WAVFrameGeometry = %d frames @%dHz, want %d @%dHz", frames, rate, tc.wantFrames, tc.sampleRate)
		}

		samples, _, err := media.ExtractPCM16Samples(wav)
		if err != nil {
			t.Fatalf("extract decoded samples failed: %v", err)
		}
		if decodedFrames := int64(len(samples) / tc.channels); decodedFrames != frames {
			t.Errorf("header geometry %d frames disagrees with decoded %d frames", frames, decodedFrames)
		}
	}

	if _, _, err := media.WAVFrameGeometry([]byte("not a wav")); !errors.Is(err, media.ErrInvalidWAVHeader) {
		t.Errorf("expected ErrInvalidWAVHeader for non-WAV bytes, got %v", err)
	}

	// Bits-per-sample is not frame geometry of a 16-bit reader: both must refuse the same bytes.
	nonPCM := media.GeneratePCM16WAV(16000, 1, 100)
	nonPCM[34] = 8
	if _, _, err := media.WAVFrameGeometry(nonPCM); err == nil {
		t.Error("expected non-16-bit WAV to be refused by the geometry probe")
	}
	if _, _, err := media.ExtractPCM16Samples(nonPCM); err == nil {
		t.Error("expected non-16-bit WAV to be refused by the decoder")
	}
}

// The mixer refuses a clip once its frames overflow the window; expressing that through
// PlaybackWindowFrames must be exactly the placement arithmetic the mixer enforced before, on
// every boundary combination, or the fit and the mixer would gate on different windows.
func TestMedia_PlaybackWindowFrames_MatchesMixerPlacementArithmetic(t *testing.T) {
	for _, rate := range []int{8000, 16000, 22050, 44100, 48000} {
		for _, startMs := range []int64{0, 1, 7, 1000, 1234} {
			for _, slotMs := range []int64{1, 500, 1000, 2400} {
				endMs := startMs + slotMs
				windowFrames := media.PlaybackWindowFrames(startMs, endMs, rate)
				startFrame := (startMs * int64(rate)) / 1000
				for _, frames := range []int64{windowFrames - 1, windowFrames, windowFrames + 1} {
					if frames < 0 {
						continue
					}
					legacyRefuses := (startFrame+frames)*1000 > endMs*int64(rate)
					if legacyRefuses != (frames > windowFrames) {
						t.Fatalf("window %d-%dms at %dHz: %d frames refuses=%v, window frames=%d",
							startMs, endMs, rate, frames, legacyRefuses, windowFrames)
					}
				}
			}
		}
	}
}

// A floored millisecond probe is the only evidence the fit has without waveform geometry, so its
// conservative frame bound must never be smaller than the frames the exact geometry would place.
func TestMedia_ProbeBoundDominatesExactFrameCount(t *testing.T) {
	for _, clipRate := range []int{8000, 16000, 44100, 48000} {
		for _, outRate := range []int{8000, 16000, 44100, 48000} {
			for _, frames := range []int64{16000, 44100, 44101, 48001} {
				probeMs := (frames * 1000) / int64(clipRate)
				if probeMs <= 0 {
					continue // the fit refuses a zero probe as invalid timing
				}
				bound := ((probeMs + 1) * int64(outRate)) / 1000
				exact := media.ResampledPCM16Frames(frames, clipRate, outRate)
				if exact > bound {
					t.Fatalf("%d frames at %dHz -> %dHz: probe bound %d is below the exact %d frames",
						frames, clipRate, outRate, bound, exact)
				}
			}
		}
	}
}
