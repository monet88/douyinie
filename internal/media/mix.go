package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// WAVHeaderInfo contains metadata parsed from a WAV header.
type WAVHeaderInfo struct {
	AudioFormat   uint16
	NumChannels   uint16
	SampleRate    uint32
	ByteRate      uint32
	BlockAlign    uint16
	BitsPerSample uint16
	DataOffset    int
	DataSize      uint32
	DurationMs    int64
}

// ParseWAVHeader parses WAV header metadata and data chunk offset.
func ParseWAVHeader(data []byte) (*WAVHeaderInfo, error) {
	if len(data) < 44 {
		return nil, ErrInvalidWAVHeader
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, ErrInvalidWAVHeader
	}

	reader := bytes.NewReader(data[12:])
	info := &WAVHeaderInfo{}
	var foundFmt, foundData bool
	offset := 12

	for {
		var chunkID [4]byte
		var chunkSize uint32
		if err := binary.Read(reader, binary.LittleEndian, &chunkID); err != nil {
			break
		}
		if err := binary.Read(reader, binary.LittleEndian, &chunkSize); err != nil {
			break
		}
		offset += 8

		id := string(chunkID[:])
		if id == "fmt " {
			if err := binary.Read(reader, binary.LittleEndian, &info.AudioFormat); err != nil {
				return nil, ErrInvalidWAVHeader
			}
			if err := binary.Read(reader, binary.LittleEndian, &info.NumChannels); err != nil {
				return nil, ErrInvalidWAVHeader
			}
			if err := binary.Read(reader, binary.LittleEndian, &info.SampleRate); err != nil {
				return nil, ErrInvalidWAVHeader
			}
			if err := binary.Read(reader, binary.LittleEndian, &info.ByteRate); err != nil {
				return nil, ErrInvalidWAVHeader
			}
			if err := binary.Read(reader, binary.LittleEndian, &info.BlockAlign); err != nil {
				return nil, ErrInvalidWAVHeader
			}
			if err := binary.Read(reader, binary.LittleEndian, &info.BitsPerSample); err != nil {
				return nil, ErrInvalidWAVHeader
			}
			offset += int(chunkSize)
			if chunkSize > 16 {
				_, _ = reader.Seek(int64(chunkSize-16), io.SeekCurrent)
			}
			foundFmt = true
		} else if id == "data" {
			info.DataSize = chunkSize
			info.DataOffset = offset
			foundData = true
			break
		} else {
			offset += int(chunkSize)
			_, _ = reader.Seek(int64(chunkSize), io.SeekCurrent)
		}
	}

	if !foundFmt || !foundData || info.SampleRate == 0 || info.NumChannels == 0 || info.BitsPerSample == 0 {
		return nil, ErrInvalidWAVHeader
	}

	bytesPerSec := int64(info.SampleRate) * int64(info.NumChannels) * int64(info.BitsPerSample/8)
	if bytesPerSec == 0 {
		return nil, ErrInvalidWAVHeader
	}

	info.DurationMs = (int64(info.DataSize) * 1000) / bytesPerSec
	return info, nil
}

// ExtractPCM16Samples extracts 16-bit signed integer PCM samples from standard WAV bytes.
func ExtractPCM16Samples(data []byte) ([]int16, *WAVHeaderInfo, error) {
	info, err := ParseWAVHeader(data)
	if err != nil {
		return nil, nil, err
	}
	if info.BitsPerSample != 16 {
		return nil, nil, fmt.Errorf("unsupported bits per sample: %d (expected 16-bit PCM)", info.BitsPerSample)
	}

	dataEnd := info.DataOffset + int(info.DataSize)
	if dataEnd > len(data) {
		dataEnd = len(data)
	}
	rawPCM := data[info.DataOffset:dataEnd]
	numSamples := len(rawPCM) / 2
	samples := make([]int16, numSamples)

	for i := 0; i < numSamples; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(rawPCM[i*2 : i*2+2]))
	}

	return samples, info, nil
}

// EncodePCM16Samples encodes int16 samples back into standard WAV container bytes.
func EncodePCM16Samples(samples []int16, sampleRate int, numChannels int) []byte {
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	if numChannels <= 0 {
		numChannels = 1
	}

	dataSize := len(samples) * 2
	byteRate := sampleRate * numChannels * 2
	blockAlign := numChannels * 2
	chunkSize := 36 + dataSize

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(chunkSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(numChannels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))

	for _, s := range samples {
		_ = binary.Write(&buf, binary.LittleEndian, s)
	}

	return buf.Bytes()
}

// ResamplePCM16 resamples int16 PCM audio samples from inRate, inChannels to outRate, outChannels using linear interpolation.
func ResamplePCM16(samples []int16, inRate, inChannels, outRate, outChannels int) []int16 {
	if len(samples) == 0 {
		return nil
	}
	if inRate <= 0 {
		inRate = 16000
	}
	if outRate <= 0 {
		outRate = inRate
	}
	if inChannels <= 0 {
		inChannels = 1
	}
	if outChannels <= 0 {
		outChannels = inChannels
	}

	// 1. Channel conversion first (if needed)
	var chConverted []int16
	if inChannels == outChannels {
		chConverted = samples
	} else {
		inFrames := len(samples) / inChannels
		chConverted = make([]int16, inFrames*outChannels)
		for f := 0; f < inFrames; f++ {
			if inChannels == 1 && outChannels == 2 {
				// Mono to stereo: copy mono to L and R
				s := samples[f]
				chConverted[f*2] = s
				chConverted[f*2+1] = s
			} else if inChannels == 2 && outChannels == 1 {
				// Stereo to mono: average L and R
				l := int32(samples[f*2])
				r := int32(samples[f*2+1])
				chConverted[f] = int16((l + r) / 2)
			} else {
				// General N to M: average all input channels to mono, then copy to M channels
				var sum int32
				for c := 0; c < inChannels; c++ {
					sum += int32(samples[f*inChannels+c])
				}
				mono := int16(sum / int32(inChannels))
				for c := 0; c < outChannels; c++ {
					chConverted[f*outChannels+c] = mono
				}
			}
		}
	}

	// 2. Sample rate conversion (if needed)
	if inRate == outRate {
		return chConverted
	}

	inFrames := len(chConverted) / outChannels
	if inFrames == 0 {
		return nil
	}
	outFrames := int(math.Round(float64(inFrames) * float64(outRate) / float64(inRate)))
	if outFrames == 0 {
		return nil
	}

	outSamples := make([]int16, outFrames*outChannels)
	ratio := float64(inRate) / float64(outRate)

	for outF := 0; outF < outFrames; outF++ {
		srcPos := float64(outF) * ratio
		srcIdx := int(srcPos)
		frac := srcPos - float64(srcIdx)

		if srcIdx >= inFrames-1 {
			for c := 0; c < outChannels; c++ {
				outSamples[outF*outChannels+c] = chConverted[(inFrames-1)*outChannels+c]
			}
		} else {
			for c := 0; c < outChannels; c++ {
				s0 := float64(chConverted[srcIdx*outChannels+c])
				s1 := float64(chConverted[(srcIdx+1)*outChannels+c])
				val := s0 + frac*(s1-s0)
				if val > 32767.0 {
					val = 32767.0
				} else if val < -32768.0 {
					val = -32768.0
				}
				outSamples[outF*outChannels+c] = int16(val)
			}
		}
	}

	return outSamples
}

// MixPCM16 mixes target dub speech into background stems with speech-window dialogue suppression and smooth crossfades.
func MixPCM16(
	bgSamples []int16,
	sampleRate int,
	channels int,
	speechClips []DubSpeechClip,
	suppressWindows []PreservationWindowInterval,
	crossfadeMs int64,
	duckingGainDb float64,
) []int16 {
	return MixPCM16Stems(bgSamples, nil, sampleRate, channels, speechClips, suppressWindows, crossfadeMs, duckingGainDb)
}

// MixPCM16Stems mixes target dub speech into background and vocal stems with dialogue suppression and smooth crossfades.
//
// Invariants (CapCap-derived, locked by #16 §5-§6, #18, #37):
// - Outside speech windows: preserves background stem AND source vocals (including singing/music-vocal).
// - Inside speech windows: suppresses source dialogue (gain = 0.0) and ducks background stem by duckingGainDb.
// - Crossfade smoothing: applies linear/cosine crossfade across crossfadeMs at boundaries (15-30ms) to prevent clicks/pumping.
// - Resampling: normalizes clips of any sample rate or channel layout before overlaying.
// - Hard clipping prevention: clamps mixed samples to [-32768, 32767].
func MixPCM16Stems(
	bgSamples []int16,
	vocalsSamples []int16,
	sampleRate int,
	channels int,
	speechClips []DubSpeechClip,
	suppressWindows []PreservationWindowInterval,
	crossfadeMs int64,
	duckingGainDb float64,
) []int16 {
	if len(bgSamples) == 0 {
		return nil
	}
	if channels <= 0 {
		channels = 1
	}
	if sampleRate <= 0 {
		sampleRate = 16000
	}

	totalSamples := len(bgSamples)
	numFrames := totalSamples / channels

	// 1. Background gain mask (defaults to 1.0, ducked during speech windows)
	bgGainMask := make([]float64, numFrames)
	for i := range bgGainMask {
		bgGainMask[i] = 1.0
	}

	// 2. Vocal gain mask (defaults to 1.0 = preserved, suppressed to 0.0 during speech windows)
	vocalGainMask := make([]float64, numFrames)
	for i := range vocalGainMask {
		vocalGainMask[i] = 1.0
	}

	crossfadeSamples := int((int64(sampleRate) * crossfadeMs) / 1000)
	if crossfadeSamples < 1 {
		crossfadeSamples = 1
	}

	duckingFactor := math.Pow(10.0, duckingGainDb/20.0)
	if duckingGainDb == 0 {
		duckingFactor = 1.0
	}

	for _, win := range suppressWindows {
		if win.Action == "suppress_dialogue" {
			startSample := int((win.StartMs * int64(sampleRate)) / 1000)
			endSample := int((win.EndMs * int64(sampleRate)) / 1000)

			if startSample < 0 {
				startSample = 0
			}
			if endSample > numFrames {
				endSample = numFrames
			}
			if startSample >= endSample {
				continue
			}

			// Inside window: duck bg, fully suppress vocals (gain = 0)
			for s := startSample; s < endSample; s++ {
				bgGainMask[s] = duckingFactor
				vocalGainMask[s] = 0.0
			}

			// Fade-in to suppression (start boundary)
			fadeStart := startSample - crossfadeSamples
			if fadeStart < 0 {
				fadeStart = 0
			}
			for s := fadeStart; s < startSample; s++ {
				t := float64(s-fadeStart) / float64(startSample-fadeStart)

				// BG ducks from 1.0 to duckingFactor
				bgGain := 1.0 - t*(1.0-duckingFactor)
				if bgGain < bgGainMask[s] {
					bgGainMask[s] = bgGain
				}

				// Vocals fade from 1.0 to 0.0
				vocalGain := 1.0 - t
				if vocalGain < vocalGainMask[s] {
					vocalGainMask[s] = vocalGain
				}
			}

			// Fade-out from suppression (end boundary)
			fadeEnd := endSample + crossfadeSamples
			if fadeEnd > numFrames {
				fadeEnd = numFrames
			}
			for s := endSample; s < fadeEnd; s++ {
				t := float64(s-endSample) / float64(fadeEnd-endSample)

				// BG returns from duckingFactor to 1.0
				bgGain := duckingFactor + t*(1.0-duckingFactor)
				if bgGain < bgGainMask[s] {
					bgGainMask[s] = bgGain
				}

				// Vocals fade back from 0.0 to 1.0
				vocalGain := t
				if vocalGain < vocalGainMask[s] {
					vocalGainMask[s] = vocalGain
				}
			}
		}
	}

	outSamples := make([]int16, totalSamples)
	// Apply BG and Vocals to output
	for frame := 0; frame < numFrames; frame++ {
		bgGain := bgGainMask[frame]
		vocalGain := vocalGainMask[frame]

		for ch := 0; ch < channels; ch++ {
			idx := frame*channels + ch
			bgVal := float64(bgSamples[idx]) * bgGain
			var vocalVal float64
			if idx < len(vocalsSamples) {
				vocalVal = float64(vocalsSamples[idx]) * vocalGain
			}
			mixed := bgVal + vocalVal
			if mixed > 32767.0 {
				mixed = 32767.0
			} else if mixed < -32768.0 {
				mixed = -32768.0
			}
			outSamples[idx] = int16(mixed)
		}
	}

	// 3. Overlay localized speech clips at exact startMs
	for _, clip := range speechClips {
		clipSamples := clip.Samples
		clipRate := clip.SampleRate
		clipCh := clip.Channels
		if clipRate <= 0 {
			clipRate = sampleRate
		}
		if clipCh <= 0 {
			clipCh = channels
		}
		// Resample/normalize clip if needed
		if clipRate != sampleRate || clipCh != channels {
			clipSamples = ResamplePCM16(clipSamples, clipRate, clipCh, sampleRate, channels)
		}

		startFrame := int((clip.StartMs * int64(sampleRate)) / 1000)
		clipFrames := len(clipSamples) / channels

		for f := 0; f < clipFrames; f++ {
			targetFrame := startFrame + f
			if targetFrame < 0 || targetFrame >= numFrames {
				continue
			}
			for ch := 0; ch < channels; ch++ {
				targetIdx := targetFrame*channels + ch
				clipSample := clipSamples[f*channels+ch]
				sum := float64(outSamples[targetIdx]) + float64(clipSample)
				if sum > 32767.0 {
					sum = 32767.0
				} else if sum < -32768.0 {
					sum = -32768.0
				}
				outSamples[targetIdx] = int16(sum)
			}
		}
	}

	return outSamples
}

// DubSpeechClip represents an accepted speech clip ready for mixing.
type DubSpeechClip struct {
	StartMs    int64
	SampleRate int
	Channels   int
	Samples    []int16
}

// PreservationWindowInterval represents an interval on the timeline for mixing.
type PreservationWindowInterval struct {
	StartMs int64
	EndMs   int64
	Action  string
}
