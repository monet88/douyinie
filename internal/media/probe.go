package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
)

// Prober defines the interface for probing media stream metadata and validating container integrity.
type Prober interface {
	Probe(ctx context.Context, filePath string, expectedHash string) (*domain.PreflightReport, error)
}

// FFprobeProber implements Prober using the external ffprobe utility.
type FFprobeProber struct {
	ffprobePath string
}

// NewFFprobeProber creates a new FFprobe-backed prober.
func NewFFprobeProber(ffprobePath ...string) *FFprobeProber {
	bin := "ffprobe"
	if len(ffprobePath) > 0 && ffprobePath[0] != "" {
		bin = ffprobePath[0]
	}
	return &FFprobeProber{ffprobePath: bin}
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"r_frame_rate"`
	AvgFrameRate string `json:"avg_frame_rate"`
	Channels     int    `json:"channels"`
	SampleRate   string `json:"sample_rate"`
	BitRate      string `json:"bit_rate"`
	Duration     string `json:"duration"`
}

type ffprobeFormat struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
	Size       string `json:"size"`
	BitRate    string `json:"bit_rate"`
}

// Probe executes ffprobe on the target media file and verifies container integrity and content fingerprint.
func (p *FFprobeProber) Probe(ctx context.Context, filePath string, expectedHash string) (*domain.PreflightReport, error) {
	report := &domain.PreflightReport{
		ID:        uuid.NewString(),
		CreatedAt: time.Now().UTC(),
	}
	var probeErrors []string

	// 1. Verify content fingerprint
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file for fingerprint check: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("hash computation: %w", err)
	}
	_ = f.Close()

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if expectedHash != "" {
		if strings.ToLower(actualHash) != strings.ToLower(expectedHash) {
			report.FingerprintMatch = false
			probeErrors = append(probeErrors, fmt.Sprintf("fingerprint mismatch: expected %s, got %s", expectedHash, actualHash))
		} else {
			report.FingerprintMatch = true
		}
	} else {
		report.FingerprintMatch = true
	}

	// 2. Execute ffprobe
	cmd := exec.CommandContext(ctx, p.ffprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		filePath,
	)

	out, err := cmd.Output()
	if err != nil {
		report.ContainerValid = false
		probeErrors = append(probeErrors, fmt.Sprintf("ffprobe execution failed: %v", err))
		report.Errors = probeErrors
		return report, nil
	}

	var ffOut ffprobeOutput
	if err := json.Unmarshal(out, &ffOut); err != nil {
		report.ContainerValid = false
		probeErrors = append(probeErrors, fmt.Sprintf("failed to parse ffprobe json: %v", err))
		report.Errors = probeErrors
		return report, nil
	}

	report.ContainerFormat = ffOut.Format.FormatName
	if ffOut.Format.Duration != "" {
		if dur, err := strconv.ParseFloat(ffOut.Format.Duration, 64); err == nil {
			report.DurationSec = dur
			report.DurationMs = int64(dur * 1000)
		}
	}

	if ffOut.Format.BitRate != "" {
		if br, err := strconv.ParseInt(ffOut.Format.BitRate, 10, 64); err == nil {
			report.VideoBitRate = br
		}
	}

	for _, st := range ffOut.Streams {
		if st.CodecType == "video" && report.VideoCodec == "" {
			report.VideoCodec = st.CodecName
			report.Width = st.Width
			report.Height = st.Height
			report.FrameRate = parseFrameRate(st.AvgFrameRate, st.RFrameRate)
			if st.BitRate != "" {
				if vbr, err := strconv.ParseInt(st.BitRate, 10, 64); err == nil {
					report.VideoBitRate = vbr
				}
			}
			if report.DurationSec == 0 && st.Duration != "" {
				if dur, err := strconv.ParseFloat(st.Duration, 64); err == nil {
					report.DurationSec = dur
					report.DurationMs = int64(dur * 1000)
				}
			}
		} else if st.CodecType == "audio" && report.AudioCodec == "" {
			report.AudioCodec = st.CodecName
			report.AudioChannels = st.Channels
			if st.SampleRate != "" {
				if sr, err := strconv.Atoi(st.SampleRate); err == nil {
					report.AudioSampleRate = sr
				}
			}
			if st.BitRate != "" {
				if abr, err := strconv.ParseInt(st.BitRate, 10, 64); err == nil {
					report.AudioBitRate = abr
				}
			}
			if report.DurationSec == 0 && st.Duration != "" {
				if dur, err := strconv.ParseFloat(st.Duration, 64); err == nil {
					report.DurationSec = dur
					report.DurationMs = int64(dur * 1000)
				}
			}
		}
	}

	// Container validity evaluation
	hasStreams := len(ffOut.Streams) > 0
	hasFormat := report.ContainerFormat != ""
	if hasStreams && hasFormat {
		report.ContainerValid = true
	} else {
		report.ContainerValid = false
		probeErrors = append(probeErrors, "no valid video or audio streams detected")
	}

	report.Errors = probeErrors
	return report, nil
}

func parseFrameRate(avg, r string) float64 {
	fps := parseFrac(avg)
	if fps > 0 {
		return fps
	}
	return parseFrac(r)
}

func parseFrac(frac string) float64 {
	if frac == "" || frac == "0/0" {
		return 0
	}
	parts := strings.Split(frac, "/")
	if len(parts) == 1 {
		v, _ := strconv.ParseFloat(parts[0], 64)
		return v
	}
	if len(parts) == 2 {
		num, err1 := strconv.ParseFloat(parts[0], 64)
		den, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 == nil && err2 == nil && den > 0 {
			return num / den
		}
	}
	return 0
}

// MockProber for synthetic tests or fallback simulation.
type MockProber struct {
	CustomReport *domain.PreflightReport
	CustomErr    error
}

func (m *MockProber) Probe(ctx context.Context, filePath string, expectedHash string) (*domain.PreflightReport, error) {
	if m.CustomErr != nil {
		return nil, m.CustomErr
	}
	if m.CustomReport != nil {
		report := *m.CustomReport
		if report.ID == "" {
			report.ID = uuid.NewString()
		}
		if report.CreatedAt.IsZero() {
			report.CreatedAt = time.Now().UTC()
		}
		return &report, nil
	}

	// Default synthetic report
	return &domain.PreflightReport{
		ID:               uuid.NewString(),
		DurationSec:      10.0,
		DurationMs:       10000,
		VideoCodec:       "h264",
		AudioCodec:       "aac",
		Width:            1080,
		Height:           1920,
		FrameRate:        30.0,
		AudioChannels:    2,
		AudioSampleRate:  44100,
		AudioBitRate:     128000,
		VideoBitRate:     2000000,
		ContainerFormat:  "mp4",
		ContainerValid:   true,
		FingerprintMatch: true,
		CreatedAt:        time.Now().UTC(),
	}, nil
}
