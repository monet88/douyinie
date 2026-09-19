package service

import (
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

// An unreliable recognizer returns a different garbage token for the same physical caption on every
// sample. Live evidence (run 4f86657f, 1080x1440): the caption band box (228,951,606,65) was read as
// "别品同" -> "别品" -> "济室" -> "点酒房" over 11000-12500 ms at 0.15-0.31 confidence. Keying tracking
// on exact text shattered that caption into four single-sample regions, every one of them discarded
// as noise, and the Chinese caption survived into the render.
func TestBuildAndInterpolateTracks_UnreliableReadingsOfOneBoxStayOneCaption(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 228, Y: 951, Width: 606, Height: 65}
	dets := []provider.RawTextDetection{
		{FrameIndex: 330, TimestampMs: 11000, Text: "别品同", Box: box, Confidence: 0.1511},
		{FrameIndex: 345, TimestampMs: 11500, Text: "别品", Box: box, Confidence: 0.1709},
		{FrameIndex: 360, TimestampMs: 12000, Text: "济室", Box: domain.BoundingBox{X: 227, Y: 950, Width: 607, Height: 66}, Confidence: 0.2937},
		{FrameIndex: 375, TimestampMs: 12500, Text: "点酒房", Box: domain.BoundingBox{X: 229, Y: 950, Width: 605, Height: 69}, Confidence: 0.3084},
	}

	regions := buildAndInterpolateTracks(dets, 500, cfg)
	if len(regions) != 1 {
		t.Fatalf("expected one tracked caption for one physical box misread four times, got %d: %+v", len(regions), regions)
	}
	reg := regions[0]
	if reg.Role != domain.TextRoleSpeechSubtitle {
		t.Errorf("role = %s, want %s (the cover hides the box, not the recognized text)", reg.Role, domain.TextRoleSpeechSubtitle)
	}
	if reg.ConfidenceEvidence.ObservationCount != 4 {
		t.Errorf("observation count = %d, want 4", reg.ConfidenceEvidence.ObservationCount)
	}
	if reg.FirstSeenMs != 11000 || reg.LastSeenMs != 12500 {
		t.Errorf("window = %d-%d ms, want 11000-12500 ms", reg.FirstSeenMs, reg.LastSeenMs)
	}
	if !reg.ReviewRequired {
		t.Error("an unreadable caption must be surfaced for operator review")
	}
	if reg.Text != "点酒房" {
		t.Errorf("cluster text = %q, want the highest-confidence reading %q", reg.Text, "点酒房")
	}
}

// Confident readings are trustworthy, so two different readable captions occupying the same band
// box are two captions, not one region carrying two texts.
func TestBuildAndInterpolateTracks_ConfidentDistinctTextsStaySeparate(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 228, Y: 951, Width: 606, Height: 65}
	dets := []provider.RawTextDetection{
		{FrameIndex: 330, TimestampMs: 11000, Text: "轻松解放双手", Box: box, Confidence: 0.99},
		{FrameIndex: 345, TimestampMs: 11500, Text: "把手机放在水盆下方", Box: box, Confidence: 0.99},
	}

	regions := buildAndInterpolateTracks(dets, 500, cfg)
	if len(regions) != 2 {
		t.Fatalf("expected two captions for two readable texts, got %d: %+v", len(regions), regions)
	}
	for _, reg := range regions {
		if reg.Role != domain.TextRoleSpeechSubtitle {
			t.Errorf("region %s role = %s, want %s", reg.ID, reg.Role, domain.TextRoleSpeechSubtitle)
		}
	}
}

// Interpolated keyframes must stay inside the region's own on-screen window and on the sampling
// grid. The OCR adapter reports FrameIndex as an absolute video frame number while TimestampMs is
// the sample time, so a tracker that multiplies FrameIndex by the sample step emits timestamps far
// outside the video (live evidence, run 4f86657f: an 8500-9000 ms caption carrying 128000 ms
// keyframes), which then defeats any window arithmetic built on keyframe timestamps.
func TestBuildAndInterpolateTracks_InterpolatedKeyframesStayOnTheSamplingGrid(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 307, Y: 948, Width: 448, Height: 70}
	dets := []provider.RawTextDetection{
		// 30 fps: 500 ms of video is 15 frames, so the sample grid is 255, 270, 300. The 9500 ms
		// sample is missing, i.e. the keyframe at that time can only be interpolated.
		{FrameIndex: 255, TimestampMs: 8500, Text: "把手机放在胶带里", Box: box, Confidence: 0.99},
		{FrameIndex: 270, TimestampMs: 9000, Text: "把手机放在胶带里", Box: box, Confidence: 0.99},
		{FrameIndex: 300, TimestampMs: 10000, Text: "把手机放在胶带里", Box: box, Confidence: 0.99},
	}

	regions := buildAndInterpolateTracks(dets, 500, cfg)
	if len(regions) != 1 {
		t.Fatalf("expected one tracked region, got %d", len(regions))
	}
	reg := regions[0]
	if reg.FirstSeenMs != 8500 || reg.LastSeenMs != 10000 {
		t.Fatalf("region window = %d-%d ms, want 8500-10000 ms", reg.FirstSeenMs, reg.LastSeenMs)
	}
	interpolated := 0
	for _, kf := range reg.Keyframes {
		if !kf.Observed {
			interpolated++
			if kf.TimestampMs != 9500 {
				t.Errorf("interpolated keyframe timestamp = %d ms, want 9500 ms (the missed sample)", kf.TimestampMs)
			}
		}
	}
	if interpolated != 1 {
		t.Errorf("interpolated keyframe count = %d, want 1", interpolated)
	}
	for _, kf := range reg.Keyframes {
		if kf.TimestampMs < reg.FirstSeenMs || kf.TimestampMs > reg.LastSeenMs {
			t.Errorf("keyframe timestamp %d ms is outside the region window %d-%d ms",
				kf.TimestampMs, reg.FirstSeenMs, reg.LastSeenMs)
		}
		if (kf.TimestampMs-8500)%500 != 0 {
			t.Errorf("keyframe timestamp %d ms is not on the 500 ms sampling grid", kf.TimestampMs)
		}
	}
	if len(reg.Keyframes) != 4 {
		t.Errorf("keyframe count = %d, want 4 (one per 500 ms sample), not one per video frame", len(reg.Keyframes))
	}
}

// Live evidence (run 4f86657f): one 12 s segment carrying four sentences became a single three-line
// block of text covering a third of the frame. Captions must be reading-sized and follow the speech.
func TestSplitCaptionText_BreaksTheLiveSegmentIntoReadingSizedCues(t *testing.T) {
	segment := "Đặt điện thoại lên tủ là bạn có ngay góc nhìn từ trên cao y hệt. " +
		"Đặt điện thoại bên dưới gói mì gà cay đã cắt miệng, bạn sẽ có cảnh quay thò tay lấy đồ cực kỳ độc lạ. " +
		"Đặt điện thoại vào trong cuộn băng dính rồi tùy ý để ở một nơi nào đó, bạn sẽ có được khung hình tương tác như thế này."

	pieces := splitCaptionText(segment, captionCueMaxChars)
	if len(pieces) < 3 {
		t.Fatalf("expected the 22 s block to break into reading-sized cues, got %d pieces: %q", len(pieces), pieces)
	}
	for _, piece := range pieces {
		if got := len([]rune(piece)); got > captionCueMaxChars {
			t.Errorf("piece carries %d chars, more than the %d budget: %q", got, captionCueMaxChars, piece)
		}
	}
	// Every word survives in order: the captions stay grounded in the canonical translation.
	if joined, want := strings.Join(pieces, " "), strings.Join(strings.Fields(segment), " "); joined != want {
		t.Errorf("pieces do not reproduce the segment text:\n got %q\nwant %q", joined, want)
	}

	windows := captionCueWindows(0, 22000, pieces)
	if len(windows) != len(pieces) {
		t.Fatalf("got %d windows for %d pieces", len(windows), len(pieces))
	}
	if windows[0][0] != 0 || windows[len(windows)-1][1] != 22000 {
		t.Errorf("windows must span the segment: got %v", windows)
	}
	for i, w := range windows {
		if w[1] <= w[0] {
			t.Errorf("window %d is empty: %v", i, w)
		}
		if i > 0 && w[0] != windows[i-1][1] {
			t.Errorf("windows %d and %d are not contiguous: %v vs %v", i-1, i, windows[i-1], w)
		}
	}
}

// A caption that already fits stays a single cue: splitting must not churn short segments.
func TestSplitCaptionText_ShortSentenceStaysOneCue(t *testing.T) {
	text := "Bước 1: Chuẩn bị đầy đủ các nguyên liệu tươi ngon."
	pieces := splitCaptionText(text, captionCueMaxChars)
	if len(pieces) != 1 || pieces[0] != text {
		t.Fatalf("expected the text unchanged as one cue, got %q", pieces)
	}
	windows := captionCueWindows(1000, 3000, pieces)
	if len(windows) != 1 || windows[0] != [2]int64{1000, 3000} {
		t.Fatalf("expected the segment window unchanged, got %v", windows)
	}
}
