package media_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
)

func TestEscapeFFmpegFilterPath(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{`C:\Users\video.ass`, `C\:/Users/video.ass`},
		{`/tmp/video.ass`, `/tmp/video.ass`},
		{`C:\path with 'quote'\file:1.ass`, `C\:/path with \'quote\'/file\:1.ass`},
	}
	for _, c := range cases {
		got := media.EscapeFFmpegFilterPath(c.input)
		if got != c.expected {
			t.Errorf("EscapeFFmpegFilterPath(%q) = %q, want %q", c.input, got, c.expected)
		}
	}
}

func TestFormatASSTimestamp(t *testing.T) {
	cases := []struct {
		ms       int64
		expected string
	}{
		{0, "0:00:00.00"},
		{500, "0:00:00.50"},
		{1230, "0:00:01.23"},
		{65430, "0:01:05.43"},
		{3661000, "1:01:01.00"},
	}
	for _, c := range cases {
		got := media.FormatASSTimestamp(c.ms)
		if got != c.expected {
			t.Errorf("FormatASSTimestamp(%d) = %q, want %q", c.ms, got, c.expected)
		}
	}
}

func TestParseASSColor(t *testing.T) {
	cases := []struct {
		input        string
		defaultAlpha uint8
		expectedBGR  string
		expectedAA   string
	}{
		{"white", 0x00, "FFFFFF", "00"},
		{"black", 0x00, "000000", "00"},
		{"black@0.6", 0x00, "000000", "66"}, // 0.6 opacity -> alpha = (1-0.6)*255 = 102 = 0x66
		{"white@0.8", 0x00, "FFFFFF", "33"}, // 0.8 opacity -> alpha = (1-0.8)*255 = 51 = 0x33
		{"#FF0000", 0x00, "0000FF", "00"},   // Red in RGB -> BGR is 0000FF
		{"#00FF00", 0x00, "00FF00", "00"},   // Green in RGB -> BGR is 00FF00
		{"#0000FF", 0x00, "FF0000", "00"},   // Blue in RGB -> BGR is FF0000
		{"yellow", 0x00, "00FFFF", "00"},
		{"transparent", 0x00, "000000", "FF"},
	}

	for _, c := range cases {
		bgr, aa, full := media.ParseASSColor(c.input, c.defaultAlpha)
		if bgr != c.expectedBGR || aa != c.expectedAA {
			t.Errorf("ParseASSColor(%q) = (%q, %q, %q), want BGR=%s AA=%s", c.input, bgr, aa, full, c.expectedBGR, c.expectedAA)
		}
	}
}

func TestGenerateASSContent_DeterministicStructureAndBoxSemantics(t *testing.T) {
	timeline := domain.RenderTimeline{
		DurationMs: 5000,
		Width:      1080,
		Height:     1920,
		FrameRate:  30.0,
	}
	cues := []domain.SubtitleCue{
		{
			StartMs:    100,
			EndMs:      1500,
			Text:       "Chào mừng bạn đến với Douyinie",
			X:          150,
			Y:          300,
			FontSizePx: 28,
			PaddingX:   20,
			BoxColor:   "black@0.6",
			FontColor:  "white",
		},
		{
			StartMs:    1600,
			EndMs:      3200,
			Text:       "Dòng 1\nDòng 2",
			X:          150,
			Y:          300,
			FontSizePx: 28,
			PaddingX:   18,
			BoxColor:   "#000000",
			FontColor:  "yellow",
		},
	}

	ass := media.GenerateASSContent(timeline, cues, "Arial")

	// Verify required ASS sections
	if !strings.Contains(ass, "[Script Info]") {
		t.Errorf("expected [Script Info] in ASS output")
	}
	if !strings.Contains(ass, "PlayResX: 1080") || !strings.Contains(ass, "PlayResY: 1920") {
		t.Errorf("expected PlayResX and PlayResY matching timeline in ASS output")
	}
	if !strings.Contains(ass, "[V4+ Styles]") {
		t.Errorf("expected [V4+ Styles] in ASS output")
	}
	// Verify CompactFitBox style definition uses BorderStyle=3 (opaque fit-content box)
	if !strings.Contains(ass, "Style: CompactFitBox,Arial,24,&H00FFFFFF,&H000000FF,&H66000000,&H66000000,0,0,0,0,100,100,0,0,3,18,0,7,0,0,0,1") {
		t.Errorf("expected CompactFitBox style definition with BorderStyle=3, got: %s", ass)
	}
	if !strings.Contains(ass, "[Events]") {
		t.Errorf("expected [Events] in ASS output")
	}

	// Verify Dialogue lines use CompactFitBox and contain exact box tags (\bord, \3c, \4c, \3a, \4a)
	if !strings.Contains(ass, "Dialogue: 0,0:00:00.10,0:00:01.50,CompactFitBox,,0,0,0,,{\\an7\\pos(150,300)\\bord20\\shad0\\fs28\\c&HFFFFFF&\\3c&H000000&\\4c&H000000&\\3a&H66&\\4a&H66&}Chào mừng bạn đến với Douyinie") {
		t.Errorf("expected dialogue line with CompactFitBox style, exact coordinates and box formatting in ASS output, got: %s", ass)
	}
	// Verify line break conversion to \N
	if !strings.Contains(ass, `Dòng 1\NDòng 2`) {
		t.Errorf("expected newline converted to \\N in ASS output, got: %s", ass)
	}
	// Verify second cue with custom yellow font color (\c&H00FFFF&) and black box (\3c&H000000&)
	if !strings.Contains(ass, `\c&H00FFFF&`) {
		t.Errorf("expected yellow font color in second cue, got: %s", ass)
	}
}

func TestCheckFFmpegLibassCapability(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available on test host")
	}
	err := media.CheckFFmpegLibassCapability("ffmpeg")
	if err != nil {
		t.Errorf("expected installed ffmpeg to have libass capability: %v", err)
	}
}
