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
//
// It delegates to ParseWAVHeader so a duration probe can never be more lenient
// than the structural parser: corrupted or truncated containers that the parser
// rejects must not yield a duration here either.
func ProbeWAVBytes(data []byte) (int64, error) {
	info, err := ParseWAVHeader(data)
	if err != nil {
		return 0, err
	}
	return info.DurationMs, nil
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
