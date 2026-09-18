package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

var (
	// ErrFFmpegNotFound is returned when ffmpeg executable is not available on PATH.
	ErrFFmpegNotFound = errors.New("ffmpeg executable not found on system PATH")
	// ErrLibassUnavailable is returned when ffmpeg lacks ASS/libass filter support.
	ErrLibassUnavailable = errors.New("ffmpeg libass/ass filter support unavailable")
	// ErrCompositionFailed is returned when ffmpeg composition subprocess fails.
	ErrCompositionFailed = errors.New("video composition failed")
)

// FFmpegAvailable reports whether the ffmpeg binary is discoverable in PATH.
func FFmpegAvailable(customPath ...string) bool {
	p := "ffmpeg"
	if len(customPath) > 0 && customPath[0] != "" {
		p = customPath[0]
	}
	_, err := exec.LookPath(p)
	return err == nil
}

// CheckFFmpegLibassCapability verifies that FFmpeg is discoverable and has ASS/libass filter support.
// Invariant: NativeRenderBackend requires FFmpeg/libass-class deterministic composition.
// Fails closed if libass filter support is absent.
func CheckFFmpegLibassCapability(ffmpegPath string) error {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if !FFmpegAvailable(ffmpegPath) {
		return ErrFFmpegNotFound
	}
	cmd := exec.Command(ffmpegPath, "-filters")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: failed to query ffmpeg filters: %v", domain.ErrRenderBackendUnavailable, err)
	}
	outStr := string(out)
	if !strings.Contains(outStr, " ass ") && !strings.Contains(outStr, " subtitles ") {
		return fmt.Errorf("%w: %v", domain.ErrRenderBackendUnavailable, ErrLibassUnavailable)
	}
	return nil
}

// EscapeFFmpegFilterPath escapes file path for use inside FFmpeg filter parameters (such as ass=filename='...').
// Converts backslashes to forward slashes, escapes colons (for Windows drive letters), and escapes single quotes.
func EscapeFFmpegFilterPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	p = strings.ReplaceAll(p, `'`, `\'`)
	return p
}

// FormatASSTimestamp formats a millisecond integer into ASS timestamp format (H:MM:SS.cc).
func FormatASSTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	totalCentis := ms / 10
	centis := totalCentis % 100
	totalSecs := totalCentis / 100
	secs := totalSecs % 60
	totalMins := totalSecs / 60
	mins := totalMins % 60
	hours := totalMins / 60
	return fmt.Sprintf("%d:%02d:%02d.%02d", hours, mins, secs, centis)
}

// ParseASSColor converts various color formats (named colors, color@opacity, #RRGGBB, #AARRGGBB, &HAABBGGRR&)
// into ASS color representations: BGR hex (6 chars), Alpha hex (2 chars), and full ASS hex (&HAABBGGRR&).
// In ASS, Alpha is 00 for fully opaque and FF for fully transparent.
func ParseASSColor(input string, defaultAlpha uint8) (bgrHex, alphaHex, fullASSHex string) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "FFFFFF", fmt.Sprintf("%02X", defaultAlpha), fmt.Sprintf("&H%02XFFFFFF&", defaultAlpha)
	}

	// Check if input is already ASS format &H[AA]BBGGRR[&]
	if strings.HasPrefix(s, "&H") || strings.HasPrefix(s, "&h") {
		trimmed := strings.Trim(s, "&Hh")
		if len(trimmed) == 8 {
			aa := strings.ToUpper(trimmed[0:2])
			bgr := strings.ToUpper(trimmed[2:8])
			return bgr, aa, fmt.Sprintf("&H%s%s&", aa, bgr)
		} else if len(trimmed) == 6 {
			bgr := strings.ToUpper(trimmed)
			aa := fmt.Sprintf("%02X", defaultAlpha)
			return bgr, aa, fmt.Sprintf("&H%s%s&", aa, bgr)
		}
	}

	var r, g, b uint8 = 255, 255, 255
	alpha := defaultAlpha

	// Check for @opacity suffix, e.g. "black@0.6"
	opacityPart := ""
	colorPart := s
	if atIdx := strings.Index(s, "@"); atIdx >= 0 {
		colorPart = strings.TrimSpace(s[:atIdx])
		opacityPart = strings.TrimSpace(s[atIdx+1:])
	}

	if opacityPart != "" {
		if op, err := strconv.ParseFloat(opacityPart, 64); err == nil {
			if op < 0 {
				op = 0
			}
			if op > 1 {
				op = 1
			}
			// op 1.0 -> alpha 0 (opaque); op 0.0 -> alpha 255 (transparent)
			alpha = uint8((1.0-op)*255.0 + 0.5)
		}
	}

	colorLower := strings.ToLower(colorPart)
	switch colorLower {
	case "white":
		r, g, b = 255, 255, 255
	case "black":
		r, g, b = 0, 0, 0
	case "red":
		r, g, b = 255, 0, 0
	case "green":
		r, g, b = 0, 255, 0
	case "blue":
		r, g, b = 0, 0, 255
	case "yellow":
		r, g, b = 255, 255, 0
	case "cyan":
		r, g, b = 0, 255, 255
	case "magenta":
		r, g, b = 255, 0, 255
	case "gray", "grey":
		r, g, b = 128, 128, 128
	case "transparent":
		r, g, b = 0, 0, 0
		alpha = 255
	default:
		// Try hex format
		hexStr := strings.TrimPrefix(colorPart, "#")
		if len(hexStr) == 3 {
			if val, err := strconv.ParseUint(hexStr, 16, 32); err == nil {
				r = uint8((val>>8)&0xF) * 0x11
				g = uint8((val>>4)&0xF) * 0x11
				b = uint8(val&0xF) * 0x11
			}
		} else if len(hexStr) == 6 {
			if val, err := strconv.ParseUint(hexStr, 16, 32); err == nil {
				r = uint8((val >> 16) & 0xFF)
				g = uint8((val >> 8) & 0xFF)
				b = uint8(val & 0xFF)
			}
		} else if len(hexStr) == 8 {
			if val, err := strconv.ParseUint(hexStr, 16, 64); err == nil {
				webAlpha := uint8((val >> 24) & 0xFF)
				alpha = 255 - webAlpha // invert for ASS alpha
				r = uint8((val >> 16) & 0xFF)
				g = uint8((val >> 8) & 0xFF)
				b = uint8(val & 0xFF)
			}
		}
	}

	bgr := fmt.Sprintf("%02X%02X%02X", b, g, r)
	aa := fmt.Sprintf("%02X", alpha)
	return bgr, aa, fmt.Sprintf("&H%s%s&", aa, bgr)
}

// GenerateASSContent generates a deterministic ASS v4.00+ subtitle file from RenderTimeline and SubtitleCues.
// Follows V3 Compact Fit-Content presentation standards and provides deterministic styles.
func GenerateASSContent(timeline domain.RenderTimeline, cues []domain.SubtitleCue, defaultFont string) string {
	width := timeline.Width
	if width <= 0 {
		width = 1080
	}
	height := timeline.Height
	if height <= 0 {
		height = 1920
	}
	fontName := "Arial"
	if defaultFont != "" {
		fontName = defaultFont
	}

	var sb strings.Builder

	// 1. Script Info
	sb.WriteString("[Script Info]\n")
	sb.WriteString("Title: Douyinie Phase 1 Deterministic Subtitles\n")
	sb.WriteString("ScriptType: v4.00+\n")
	sb.WriteString("WrapStyle: 0\n")
	sb.WriteString("ScaledBorderAndShadow: yes\n")
	sb.WriteString(fmt.Sprintf("PlayResX: %d\n", width))
	sb.WriteString(fmt.Sprintf("PlayResY: %d\n", height))
	sb.WriteString("\n")

	// 2. V4+ Styles
	// Invariant: CompactFitBox uses BorderStyle=3 (opaque fit-content background box hugging rendered text),
	// with default scale-aware padding Outline=18, Shadow=0, and semi-transparent dark background.
	sb.WriteString("[V4+ Styles]\n")
	sb.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n")
	sb.WriteString(fmt.Sprintf("Style: CompactFitBox,%s,24,&H00FFFFFF,&H000000FF,&H66000000,&H66000000,0,0,0,0,100,100,0,0,3,18,0,7,0,0,0,1\n", fontName))
	sb.WriteString(fmt.Sprintf("Style: Default,%s,24,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,0,0,0,0,100,100,0,0,1,2,0,2,20,20,40,1\n", fontName))
	sb.WriteString("\n")

	// 3. Events
	sb.WriteString("[Events]\n")
	sb.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")

	for _, cue := range cues {
		if strings.TrimSpace(cue.Text) == "" {
			continue
		}
		startStr := FormatASSTimestamp(cue.StartMs)
		endStr := FormatASSTimestamp(cue.EndMs)

		text := cue.Text
		text = strings.ReplaceAll(text, "\r\n", `\N`)
		text = strings.ReplaceAll(text, "\n", `\N`)
		text = strings.ReplaceAll(text, "{", `\{`)
		text = strings.ReplaceAll(text, "}", `\}`)

		var tags []string

		// Positioning: exact (X, Y) uses \an7\pos(X, Y) top-left anchor.
		if cue.X > 0 || cue.Y > 0 {
			tags = append(tags, fmt.Sprintf(`\an7\pos(%d,%d)`, cue.X, cue.Y))
		}

		// Padding: when BorderStyle=3, \bord sets the padding width of the background box hugging the text.
		padding := cue.PaddingX
		if padding <= 0 {
			padding = 18 // Scale-aware reference default
		}
		tags = append(tags, fmt.Sprintf(`\bord%d`, padding))
		tags = append(tags, `\shad0`)

		if cue.FontSizePx > 0 {
			tags = append(tags, fmt.Sprintf(`\fs%d`, cue.FontSizePx))
		}
		if cue.FontName != "" {
			tags = append(tags, fmt.Sprintf(`\fn%s`, cue.FontName))
		}

		// Font color & alpha
		if cue.FontColor != "" {
			bgr, aa, _ := ParseASSColor(cue.FontColor, 0x00)
			tags = append(tags, fmt.Sprintf(`\c&H%s&`, bgr))
			if aa != "00" {
				tags = append(tags, fmt.Sprintf(`\1a&H%s&`, aa))
			}
		}

		// Box color & alpha
		boxColorStr := cue.BoxColor
		if boxColorStr == "" {
			boxColorStr = "black@0.6" // Default compact fit-content background box
		}
		boxBGR, boxAlpha, _ := ParseASSColor(boxColorStr, 0x66)
		tags = append(tags, fmt.Sprintf(`\3c&H%s&\4c&H%s&`, boxBGR, boxBGR))
		tags = append(tags, fmt.Sprintf(`\3a&H%s&\4a&H%s&`, boxAlpha, boxAlpha))

		tagStr := ""
		if len(tags) > 0 {
			tagStr = "{" + strings.Join(tags, "") + "}"
		}

		style := "CompactFitBox"
		sb.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,%s,,0,0,0,,%s%s\n", startStr, endStr, style, tagStr, text))
	}

	return sb.String()
}

// CompositionRequest holds parameters for native deterministic composition.
type CompositionRequest struct {
	FFmpegPath  string
	SourceVideo string // path to source video file
	AudioTrack  string // path to mixed WAV audio file
	Timeline    domain.RenderTimeline
	Cues        []domain.SubtitleCue
	Covers      []domain.CoverBox // opaque boxes hiding replaced source text, composited before the subtitles
	ASSContent  string            // optional pre-generated or custom ASS script content
	FontFile    string            // optional font path
	Profile     domain.EncodeProfile
	OutputPath  string // target output MP4 file path
}

// CompositionResult captures the outcome of native video composition.
type CompositionResult struct {
	OutputPath string
	ByteSize   int64
	DurationMs int64
	Renderer   string // "native-ffmpeg-libass"
}

// ffmpegCoverColor converts a "#RRGGBB" + opacity pair into an ffmpeg color filter spec.
// Opacity 1.0 produces an opaque RGB source; a lower opacity appends the alpha component
// ("0xRRGGBB@a"), which is what the color filter expects.
func ffmpegCoverColor(color string, opacity float64) (string, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(color), "#")
	if clean == "" {
		clean = "000000"
	}
	if len(clean) != 6 {
		return "", fmt.Errorf("cover color %q is not #RRGGBB", color)
	}
	for _, r := range clean {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", fmt.Errorf("cover color %q is not hexadecimal", color)
		}
	}
	spec := "0x" + strings.ToLower(clean)
	if opacity > 0 && opacity < 1 {
		spec = fmt.Sprintf("%s@%.3f", spec, opacity)
	}
	return spec, nil
}

// ComposeNativeVideo executes deterministic composition via FFmpeg and libass.
// Invariant: NativeRenderBackend deterministic composition over frozen exact artifact refs.
// Preview and Final consume identical ASS subtitle layout/style, timeline, and audio selection.
func ComposeNativeVideo(ctx context.Context, req CompositionRequest) (*CompositionResult, error) {
	ffmpegExe := req.FFmpegPath
	if ffmpegExe == "" {
		ffmpegExe = "ffmpeg"
	}
	if err := CheckFFmpegLibassCapability(ffmpegExe); err != nil {
		return nil, err
	}

	if _, err := os.Stat(req.SourceVideo); err != nil {
		return nil, fmt.Errorf("source video file not found: %w", err)
	}
	if _, err := os.Stat(req.AudioTrack); err != nil {
		return nil, fmt.Errorf("audio track file not found: %w", err)
	}

	scaleDiv := req.Profile.ProxyScaleDivisor
	if scaleDiv <= 0 {
		scaleDiv = 1
	}

	// Prepare ASS content if cues or ASS content provided
	assContent := req.ASSContent
	if assContent == "" && len(req.Cues) > 0 {
		assContent = GenerateASSContent(req.Timeline, req.Cues, req.FontFile)
	}

	var tmpDir string
	var filterGraph string

	if strings.TrimSpace(assContent) != "" {
		var err error
		tmpDir, err = os.MkdirTemp("", "douyinie_ass_render_*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir for ass subtitle: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		assPath := filepath.Join(tmpDir, "subtitles.ass")
		if err := os.WriteFile(assPath, []byte(assContent), 0644); err != nil {
			return nil, fmt.Errorf("write ass file: %w", err)
		}

		escapedASS := EscapeFFmpegFilterPath(assPath)
		assFilter := fmt.Sprintf("ass=filename='%s'", escapedASS)

		if scaleDiv > 1 {
			filterGraph = fmt.Sprintf("%s,scale=trunc(iw/%d/2)*2:-2", assFilter, scaleDiv)
		} else {
			filterGraph = assFilter
		}
	} else {
		if scaleDiv > 1 {
			filterGraph = fmt.Sprintf("scale=trunc(iw/%d/2)*2:-2", scaleDiv)
		} else {
			filterGraph = "null"
		}
	}

	// Source-text covers are composited BEFORE the subtitle burn and before any proxy scale: covers
	// and ASS events are both authored in timeline coordinates, so the scale has to stay last. A
	// drawbox per cover spends one filter each - the alternative (a color source overlaid per cover)
	// needs a multi-source -filter_complex graph for the same pixels.
	if len(req.Covers) > 0 {
		boxes := make([]string, 0, len(req.Covers))
		for _, cov := range req.Covers {
			if err := domain.ValidateCoverBox(cov, req.Timeline.Width, req.Timeline.Height); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrCompositionFailed, err)
			}
			colorSpec, err := ffmpegCoverColor(cov.Color, cov.Opacity)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrCompositionFailed, err)
			}
			boxes = append(boxes, fmt.Sprintf(
				"drawbox=x=%d:y=%d:w=%d:h=%d:color=%s:t=fill:enable='between(t,%.3f,%.3f)'",
				cov.X, cov.Y, cov.Width, cov.Height, colorSpec,
				float64(cov.StartMs)/1000.0, float64(cov.EndMs)/1000.0,
			))
		}
		filterGraph = strings.Join(boxes, ",") + "," + filterGraph
	}

	crf := req.Profile.CRF
	if crf <= 0 {
		if req.Profile.Kind == domain.RenderKindPreview {
			crf = 30
		} else {
			crf = 20
		}
	}
	preset := req.Profile.Preset
	if preset == "" {
		if req.Profile.Kind == domain.RenderKindPreview {
			preset = "veryfast"
		} else {
			preset = "medium"
		}
	}
	audioBitRate := req.Profile.AudioBitRateK
	if audioBitRate <= 0 {
		if req.Profile.Kind == domain.RenderKindPreview {
			audioBitRate = 96
		} else {
			audioBitRate = 192
		}
	}

	outDir := filepath.Dir(req.OutputPath)
	if outDir != "" {
		_ = os.MkdirAll(outDir, 0755)
	}

	args := []string{
		"-y",
		"-i", req.SourceVideo,
		"-i", req.AudioTrack,
		"-vf", filterGraph,
		"-map", "0:v:0",
		"-map", "1:a:0",
		"-c:v", "libx264",
		"-crf", fmt.Sprintf("%d", crf),
		"-preset", preset,
		"-c:a", "aac",
		"-b:a", fmt.Sprintf("%dk", audioBitRate),
		"-shortest",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		req.OutputPath,
	}

	cmd := exec.CommandContext(ctx, ffmpegExe, args...)
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: ffmpeg command failed: %v (stderr: %s)", ErrCompositionFailed, err, stderrBuf.String())
	}

	stat, err := os.Stat(req.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("stat output video %s: %w", req.OutputPath, err)
	}
	if stat.Size() == 0 {
		return nil, fmt.Errorf("%w: output video file is zero bytes", ErrCompositionFailed)
	}

	durationMs := int64(0)
	prober := NewFFprobeProber()
	if report, err := prober.Probe(ctx, req.OutputPath, ""); err == nil && report != nil {
		durationMs = report.DurationMs
	}

	return &CompositionResult{
		OutputPath: req.OutputPath,
		ByteSize:   stat.Size(),
		DurationMs: durationMs,
		Renderer:   "native-ffmpeg-libass",
	}, nil
}
