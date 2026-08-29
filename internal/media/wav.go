package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	// ErrInvalidWAVHeader is returned when WAV data has an invalid or unsupported header.
	ErrInvalidWAVHeader = errors.New("invalid or unsupported WAV header")
)

// GeneratePCM16WAV generates a standard 16-bit PCM WAV byte slice for a given sample rate, channels, and duration in milliseconds.
func GeneratePCM16WAV(sampleRate int, channels int, durationMs int64) []byte {
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	if channels <= 0 {
		channels = 1
	}
	if durationMs < 0 {
		durationMs = 0
	}

	numSamples := int((int64(sampleRate) * durationMs) / 1000)
	bytesPerSample := 2 // 16-bit PCM
	dataSize := numSamples * channels * bytesPerSample
	byteRate := sampleRate * channels * bytesPerSample
	blockAlign := channels * bytesPerSample
	chunkSize := 36 + dataSize

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(chunkSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // PCM subchunk size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // AudioFormat = 1 (PCM)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16)) // BitsPerSample
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))

	pcmData := make([]byte, dataSize)
	buf.Write(pcmData)

	return buf.Bytes()
}

// ProbeWAVBytes probes the exact duration in milliseconds from standard WAV bytes.
func ProbeWAVBytes(data []byte) (int64, error) {
	if len(data) < 44 {
		return 0, ErrInvalidWAVHeader
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, ErrInvalidWAVHeader
	}

	reader := bytes.NewReader(data[12:])
	var sampleRate uint32
	var numChannels uint16
	var bitsPerSample uint16
	var dataSize uint32
	var foundFmt, foundData bool

	for {
		var chunkID [4]byte
		var chunkSize uint32
		if err := binary.Read(reader, binary.LittleEndian, &chunkID); err != nil {
			break
		}
		if err := binary.Read(reader, binary.LittleEndian, &chunkSize); err != nil {
			break
		}

		id := string(chunkID[:])
		if id == "fmt " {
			var audioFormat uint16
			_ = binary.Read(reader, binary.LittleEndian, &audioFormat)
			_ = binary.Read(reader, binary.LittleEndian, &numChannels)
			_ = binary.Read(reader, binary.LittleEndian, &sampleRate)
			var byteRate uint32
			_ = binary.Read(reader, binary.LittleEndian, &byteRate)
			var blockAlign uint16
			_ = binary.Read(reader, binary.LittleEndian, &blockAlign)
			_ = binary.Read(reader, binary.LittleEndian, &bitsPerSample)

			if chunkSize > 16 {
				_, _ = reader.Seek(int64(chunkSize-16), io.SeekCurrent)
			}
			foundFmt = true
		} else if id == "data" {
			dataSize = chunkSize
			foundData = true
			break
		} else {
			_, _ = reader.Seek(int64(chunkSize), io.SeekCurrent)
		}
	}

	if !foundFmt || !foundData || sampleRate == 0 || numChannels == 0 || bitsPerSample == 0 {
		return 0, ErrInvalidWAVHeader
	}

	bytesPerSec := int64(sampleRate) * int64(numChannels) * int64(bitsPerSample/8)
	if bytesPerSec == 0 {
		return 0, ErrInvalidWAVHeader
	}

	durationMs := (int64(dataSize) * 1000) / bytesPerSec
	return durationMs, nil
}

// ProbeAudioFileDuration probes the true audio duration in milliseconds from a file path.
// It parses WAV directly, or falls back to ffprobe if needed.
func ProbeAudioFileDuration(ctx context.Context, filePath string) (int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	header := make([]byte, 4096)
	n, err := f.Read(header)
	if err != nil && err != io.EOF {
		return 0, fmt.Errorf("read audio header: %w", err)
	}

	if n >= 44 && string(header[0:4]) == "RIFF" && string(header[8:12]) == "WAVE" {
		data, err := os.ReadFile(filePath)
		if err == nil {
			durMs, err := ProbeWAVBytes(data)
			if err == nil {
				return durMs, nil
			}
		}
	}

	// Fallback to Prober
	prober := NewFFprobeProber()
	report, err := prober.Probe(ctx, filePath, "")
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration probe: %w", err)
	}
	return report.DurationMs, nil
}
