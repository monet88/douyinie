package media_test

import (
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
