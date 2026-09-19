package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestClassifyRegion(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1920)

	tests := []struct {
		name          string
		text          string
		box           domain.BoundingBox
		confidence    float64
		wantRole      domain.TextRegionRole
		wantProtected bool
	}{
		{
			name:          "watermark pattern should be ignore/noise",
			text:          "抖音号: 12345678",
			box:           domain.BoundingBox{X: 800, Y: 100, Width: 200, Height: 40},
			confidence:    0.95,
			wantRole:      domain.TextRoleIgnoreNoise,
			wantProtected: false,
		},
		{
			name:          "handle pattern should be ignore/noise",
			text:          "@douyin_creator",
			box:           domain.BoundingBox{X: 800, Y: 100, Width: 200, Height: 40},
			confidence:    0.90,
			wantRole:      domain.TextRoleIgnoreNoise,
			wantProtected: false,
		},
		{
			name:          "very low confidence should be ignore/noise",
			text:          "random garbled text",
			box:           domain.BoundingBox{X: 100, Y: 500, Width: 300, Height: 50},
			confidence:    0.20,
			wantRole:      domain.TextRoleIgnoreNoise,
			wantProtected: false,
		},
		{
			name:          "brand name should be brand_keep",
			text:          "SUPOR",
			box:           domain.BoundingBox{X: 50, Y: 50, Width: 120, Height: 40},
			confidence:    0.98,
			wantRole:      domain.TextRoleBrandKeep,
			wantProtected: true,
		},
		{
			name:          "instructional UI button should be instructional_ui_text and protected",
			text:          "导出",
			box:           domain.BoundingBox{X: 950, Y: 80, Width: 80, Height: 40},
			confidence:    0.92,
			wantRole:      domain.TextRoleInstructionalUIText,
			wantProtected: true,
		},
		{
			name:          "instructional UI keyframe should be instructional_ui_text",
			text:          "关键帧",
			box:           domain.BoundingBox{X: 500, Y: 1400, Width: 100, Height: 40},
			confidence:    0.90,
			wantRole:      domain.TextRoleInstructionalUIText,
			wantProtected: true,
		},
		{
			name:          "bottom speech subtitle",
			text:          "Hãy cắt tỉa gốc hoa xéo 45 độ nhé",
			box:           domain.BoundingBox{X: 200, Y: 1600, Width: 680, Height: 60},
			confidence:    0.91,
			wantRole:      domain.TextRoleSpeechSubtitle,
			wantProtected: false,
		},
		{
			name:          "upper semantic recipe step",
			text:          "Bước 1: Chuẩn bị trà Matcha",
			box:           domain.BoundingBox{X: 100, Y: 300, Width: 500, Height: 60},
			confidence:    0.93,
			wantRole:      domain.TextRoleSemanticText,
			wantProtected: false,
		},
		{
			// A multi-word ASCII caption in the band is speech the pipeline replaces, even though
			// its words are shorter than the single-token letter-run floor.
			name:          "multi-word ASCII caption in the bottom band stays a speech subtitle",
			text:          "Go now",
			box:           domain.BoundingBox{X: 300, Y: 1700, Width: 200, Height: 60},
			confidence:    0.90,
			wantRole:      domain.TextRoleSpeechSubtitle,
			wantProtected: false,
		},
		{
			// The single-token debris that motivated the letter-run floor (live run 4f86657f) is
			// still rejected: in-band geometry alone does not promote it.
			name:          "in-band single-token letter debris stays noise",
			text:          "K2",
			box:           domain.BoundingBox{X: 300, Y: 1700, Width: 200, Height: 60},
			confidence:    0.90,
			wantRole:      domain.TextRoleIgnoreNoise,
			wantProtected: false,
		},
		{
			name:          "in-band digit debris stays noise",
			text:          "11-11",
			box:           domain.BoundingBox{X: 300, Y: 1700, Width: 200, Height: 60},
			confidence:    0.90,
			wantRole:      domain.TextRoleIgnoreNoise,
			wantProtected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRole, gotProt, _, _ := domain.ClassifyRegion(tt.text, tt.box, tt.confidence, cfg)
			if gotRole != tt.wantRole {
				t.Errorf("got role %q, want %q", gotRole, tt.wantRole)
			}
			if gotProt.IsProtected != tt.wantProtected {
				t.Errorf("got is_protected %v, want %v", gotProt.IsProtected, tt.wantProtected)
			}
		})
	}
}

func TestTextRegionPlanProvenanceHash(t *testing.T) {
	h1, err := domain.ComputeTextRegionPlanProvenanceHash("asset-1", "fake_paddle_ocr", "paddleocr", "v4", 500)
	if err != nil {
		t.Fatalf("compute hash: %v", err)
	}
	h2, err := domain.ComputeTextRegionPlanProvenanceHash("asset-1", "fake_paddle_ocr", "paddleocr", "v4", 500)
	if err != nil {
		t.Fatalf("compute hash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("expected deterministic hash, got %s != %s", h1, h2)
	}

	// Changing sample step or model changes provenance
	h3, err := domain.ComputeTextRegionPlanProvenanceHash("asset-1", "fake_paddle_ocr", "paddleocr", "v4", 1000)
	if err != nil {
		t.Fatalf("compute hash: %v", err)
	}
	if h1 == h3 {
		t.Errorf("expected different hash for changed sample step")
	}
}

func TestComputeTextRegionPlanOverrideProvenanceHash(t *testing.T) {
	regions := []domain.TrackedTextRegion{
		{ID: "reg-a", Text: "关注", Role: domain.TextRoleSemanticText, FirstSeenMs: 0, LastSeenMs: 1500},
		{ID: "reg-b", Text: "步骤一", Role: domain.TextRoleInstructionalUIText, FirstSeenMs: 1500, LastSeenMs: 3000},
	}
	h1, err := domain.ComputeTextRegionPlanOverrideProvenanceHash("prov-src-1", regions)
	if err != nil {
		t.Fatalf("compute override prov: %v", err)
	}
	h2, err := domain.ComputeTextRegionPlanOverrideProvenanceHash("prov-src-1", regions)
	if err != nil {
		t.Fatalf("compute override prov: %v", err)
	}
	if h1 != h2 {
		t.Errorf("expected deterministic override provenance, got %s != %s", h1, h2)
	}

	// Order-independence: regions sorted by ID before hashing.
	reversed := []domain.TrackedTextRegion{regions[1], regions[0]}
	hRev, err := domain.ComputeTextRegionPlanOverrideProvenanceHash("prov-src-1", reversed)
	if err != nil {
		t.Fatalf("compute reversed override prov: %v", err)
	}
	if hRev != h1 {
		t.Errorf("expected override provenance to be order-independent, got %s != %s", hRev, h1)
	}

	// Different parent provenance -> different hash.
	hParent, err := domain.ComputeTextRegionPlanOverrideProvenanceHash("prov-src-2", regions)
	if err != nil {
		t.Fatalf("compute override prov: %v", err)
	}
	if hParent == h1 {
		t.Errorf("expected different hash for different parent provenance")
	}

	// Different canonical region state -> different hash.
	changed := []domain.TrackedTextRegion{
		{ID: "reg-a", Text: "关注", Role: domain.TextRoleSemanticText, FirstSeenMs: 0, LastSeenMs: 1500},
		{ID: "reg-b", Text: "步骤一", Role: domain.TextRoleBrandKeep, FirstSeenMs: 1500, LastSeenMs: 3000},
	}
	hChanged, err := domain.ComputeTextRegionPlanOverrideProvenanceHash("prov-src-1", changed)
	if err != nil {
		t.Fatalf("compute changed override prov: %v", err)
	}
	if hChanged == h1 {
		t.Errorf("expected different hash for changed region state")
	}

	// Override provenance must NEVER collide with the source-derived plan provenance.
	srcProv, err := domain.ComputeTextRegionPlanProvenanceHash("asset-1", "fake_paddle_ocr", "paddleocr", "v4", 500)
	if err != nil {
		t.Fatalf("compute source prov: %v", err)
	}
	overrideProv, err := domain.ComputeTextRegionPlanOverrideProvenanceHash(srcProv, regions)
	if err != nil {
		t.Fatalf("compute override prov: %v", err)
	}
	if overrideProv == srcProv {
		t.Errorf("override provenance must differ from source-derived provenance")
	}
}

func TestStandardInstructionalUITerm(t *testing.T) {
	tests := []struct {
		zh        string
		target    string
		wantTerm  string
		wantFound bool
	}{
		{"导出", "vi", "Xuất", true},
		{"导出", "en", "Export", true},
		{"关键帧", "vi", "Keyframe", true},
		{"关键帧", "en", "Keyframe", true},
		{"画中画", "vi", "Lớp phủ", true},
		{"画中画", "en", "Overlay", true},
		{"未知按钮", "vi", "未知按钮", false},
	}

	for _, tt := range tests {
		term, found := domain.StandardInstructionalUITerm(tt.zh, tt.target)
		if found != tt.wantFound {
			t.Errorf("StandardInstructionalUITerm(%q, %q) found=%v, want %v", tt.zh, tt.target, found, tt.wantFound)
		}
		if term != tt.wantTerm {
			t.Errorf("StandardInstructionalUITerm(%q, %q) term=%q, want %q", tt.zh, tt.target, term, tt.wantTerm)
		}
	}
}
func TestComputeCompactSubtitleBounds_NonOcclusion(t *testing.T) {
	frameW, frameH := 1080, 1920
	protectedAreas := []domain.BoundingBox{
		{
			X:      100,
			Y:      1400,
			Width:  880,
			Height: 150, // Timeline track at bottom
		},
		{
			X:      100,
			Y:      1300,
			Width:  880,
			Height: 80, // Another control above
		},
	}

	cue, err := domain.ComputeCompactSubtitleBounds(
		frameW, frameH,
		"Hãy cắt tỉa gốc hoa xéo 45 độ nhé",
		24, 20, 12,
		protectedAreas,
	)
	if err != nil {
		t.Fatalf("expected successful placement, got error: %v", err)
	}

	// Ensure bounding box does not overlap any protected area and stays in frame
	cueBox := domain.BoundingBox{
		X:      cue.X,
		Y:      cue.Y,
		Width:  cue.Width,
		Height: cue.Height,
	}
	for i, prot := range protectedAreas {
		if domain.BoxesOverlap(cueBox, prot) {
			t.Errorf("expected subtitle box not to overlap protected area #%d, cue=(%+v), prot=(%+v)", i, cueBox, prot)
		}
	}
	if cue.Y < int(float64(frameH)*0.10) || cue.Y > frameH-50 {
		t.Errorf("subtitle cue out of frame bounds: Y=%d", cue.Y)
	}
	if cue.PaddingX < 18 || cue.PaddingX > 28 {
		t.Errorf("padding_x %d out of scale-aware range [18, 28]", cue.PaddingX)
	}
	if cue.PaddingY < 10 || cue.PaddingY > 16 {
		t.Errorf("padding_y %d out of scale-aware range [10, 16]", cue.PaddingY)
	}
}

func TestComputeCompactSubtitleBounds_NoSafeSlot_FailsClosed(t *testing.T) {
	frameW, frameH := 1080, 1920
	// Protected area covering entire frame height
	fullScreenCover := []domain.BoundingBox{
		{
			X:      0,
			Y:      0,
			Width:  frameW,
			Height: frameH,
		},
	}

	_, err := domain.ComputeCompactSubtitleBounds(
		frameW, frameH,
		"Subtitle cannot fit anywhere",
		0, 0, 0,
		fullScreenCover,
	)
	if err == nil {
		t.Fatalf("expected ErrSubtitleOverlapsProtectedRegion when frame is completely covered, got nil")
	}
	if !errors.Is(err, domain.ErrSubtitleOverlapsProtectedRegion) {
		t.Errorf("expected ErrSubtitleOverlapsProtectedRegion, got %v", err)
	}
}

func TestComputeCompactSubtitleBounds_MovingKeyframes_And_TimeWindows(t *testing.T) {
	movingRegion := domain.TrackedTextRegion{
		ID:          "moving-face-1",
		Role:        domain.TextRoleBrandKeep,
		FirstSeenMs: 1000,
		LastSeenMs:  3000,
		ProtectedMetadata: domain.ProtectedRegionMetadata{
			IsProtected: true,
			Reason:      "detected_face",
		},
		Keyframes: []domain.RegionKeyframe{
			{TimestampMs: 1000, Box: domain.BoundingBox{X: 100, Y: 100, Width: 200, Height: 200}},
			{TimestampMs: 2000, Box: domain.BoundingBox{X: 100, Y: 1400, Width: 880, Height: 200}}, // moved to subtitle area at t=2000
		},
	}

	// At t=500..800 (before moving region is active): no protected boxes
	boxesBefore := domain.GetProtectedBoxesForTimeWindow([]domain.TrackedTextRegion{movingRegion}, nil, 500, 800, "")
	if len(boxesBefore) != 0 {
		t.Errorf("expected 0 protected boxes before region active, got %d", len(boxesBefore))
	}

	// At t=1500..2500 (during movement): both keyframes (or active keyframe) returned
	boxesDuring := domain.GetProtectedBoxesForTimeWindow([]domain.TrackedTextRegion{movingRegion}, nil, 1500, 2500, "")
	if len(boxesDuring) == 0 {
		t.Fatalf("expected protected boxes during active window")
	}
	foundMovingBox := false
	for _, b := range boxesDuring {
		if b.Y == 1400 {
			foundMovingBox = true
			break
		}
	}
	if !foundMovingBox {
		t.Errorf("expected moving keyframe at Y=1400 to be included in time window")
	}
}

func TestNormalizeInstructionalUIText_RemainderPreservation(t *testing.T) {
	// 1. Exact match
	normExact := domain.NormalizeInstructionalUIText("Xuất", "导出", "vi")
	if normExact != "Xuất" {
		t.Errorf("got %q, want 'Xuất'", normExact)
	}

	// 2. Compound phrase with remainder: "导出视频" -> "Xuất video" (not truncating to "Xuất")
	normCompound := domain.NormalizeInstructionalUIText("Xuất video chất lượng cao", "导出视频", "vi")
	if normCompound != "Xuất video chất lượng cao" {
		t.Errorf("got %q, want 'Xuất video chất lượng cao'", normCompound)
	}

	// 3. English exact and compound
	normEN := domain.NormalizeInstructionalUIText("Export", "导出", "en")
	if normEN != "Export" {
		t.Errorf("got %q, want 'Export'", normEN)
	}

	normENCompound := domain.NormalizeInstructionalUIText("Export high quality video", "导出视频", "en")
	if normENCompound != "Export high quality video" {
		t.Errorf("got %q, want 'Export high quality video'", normENCompound)
	}
}

func TestComputeLocalizedVisualTrackProvenanceHash(t *testing.T) {
	overlays := []domain.LocalizedOverlayItem{
		{RegionID: "reg-1", Role: domain.TextRoleSemanticText, SourceText: "步骤一", LocalizedText: "Bước 1", Box: domain.BoundingBox{X: 10, Y: 20, Width: 100, Height: 30}},
	}
	cues := []domain.SubtitleCue{
		{StartMs: 0, EndMs: 1000, Text: "Xin chào", X: 100, Y: 200, FontSizePx: 24},
	}

	h1, err := domain.ComputeLocalizedVisualTrackProvenanceHash("asset-1", "vi", "plan-prov-1", "dub-prov-1", overlays, cues, nil)
	if err != nil {
		t.Fatalf("compute hash 1: %v", err)
	}
	h2, err := domain.ComputeLocalizedVisualTrackProvenanceHash("asset-1", "vi", "plan-prov-1", "dub-prov-1", overlays, cues, nil)
	if err != nil {
		t.Fatalf("compute hash 2: %v", err)
	}
	if h1 != h2 {
		t.Errorf("expected deterministic hash, got %s != %s", h1, h2)
	}
}

func TestBoxesOverlap(t *testing.T) {
	b1 := domain.BoundingBox{X: 10, Y: 10, Width: 50, Height: 50}
	b2 := domain.BoundingBox{X: 30, Y: 30, Width: 50, Height: 50}
	b3 := domain.BoundingBox{X: 100, Y: 100, Width: 50, Height: 50}

	if !domain.BoxesOverlap(b1, b2) {
		t.Errorf("expected b1 and b2 to overlap")
	}
	if domain.BoxesOverlap(b1, b3) {
		t.Errorf("expected b1 and b3 not to overlap")
	}
}

func TestComputeCompactSubtitleBounds_Selector(t *testing.T) {
	frameW, frameH := 1080, 1920
	text := "Xin chào thế giới"

	// 1. Selector returns valid box
	customSelector := func(fw, fh int, cand domain.BoundingBox, protected []domain.BoundingBox) (domain.BoundingBox, bool) {
		return domain.BoundingBox{X: 100, Y: 1200, Width: cand.Width, Height: cand.Height}, true
	}
	cue, err := domain.ComputeCompactSubtitleBoundsWithSelector(frameW, frameH, text, 24, 20, 12, nil, customSelector)
	if err != nil {
		t.Fatalf("unexpected error with selector: %v", err)
	}
	if cue.X != 100 || cue.Y != 1200 {
		t.Errorf("expected selector chosen coords (100, 1200), got (%d, %d)", cue.X, cue.Y)
	}
	if cue.Width <= 0 || cue.Height <= 0 {
		t.Errorf("expected positive width/height, got %dx%d", cue.Width, cue.Height)
	}

	// 2. Selector returns colliding box -> safe fallback triggers
	collidingProtects := []domain.BoundingBox{
		{X: 90, Y: 1190, Width: 300, Height: 100},
	}
	cueFallback, err := domain.ComputeCompactSubtitleBoundsWithSelector(frameW, frameH, text, 24, 20, 12, collidingProtects, customSelector)
	if err != nil {
		t.Fatalf("unexpected error on fallback: %v", err)
	}
	cueBox := domain.BoundingBox{X: cueFallback.X, Y: cueFallback.Y, Width: cueFallback.Width, Height: cueFallback.Height}
	if domain.BoxesOverlap(cueBox, collidingProtects[0]) {
		t.Errorf("expected fallback to avoid protected area, but cue overlaps: cue=(%d,%d,%d,%d), prot=(%d,%d,%d,%d)",
			cueBox.X, cueBox.Y, cueBox.Width, cueBox.Height,
			collidingProtects[0].X, collidingProtects[0].Y, collidingProtects[0].Width, collidingProtects[0].Height)
	}

	// 3. Selector returns out-of-bounds box -> safe fallback triggers
	outOfBoundsSelector := func(fw, fh int, cand domain.BoundingBox, protected []domain.BoundingBox) (domain.BoundingBox, bool) {
		return domain.BoundingBox{X: -500, Y: -100, Width: cand.Width, Height: cand.Height}, true
	}
	cueOOB, err := domain.ComputeCompactSubtitleBoundsWithSelector(frameW, frameH, text, 24, 20, 12, nil, outOfBoundsSelector)
	if err != nil {
		t.Fatalf("unexpected error on OOB fallback: %v", err)
	}
	if cueOOB.X < 0 || cueOOB.Y < 0 {
		t.Errorf("expected safe fallback inside frame, got (%d, %d)", cueOOB.X, cueOOB.Y)
	}
}
func TestGetProtectedBoxesForTimeWindow_TransientZeroTimestamp(t *testing.T) {
	// A transient protected region observed at t=66000ms only (first_seen_ms=66000, last_seen_ms=66000)
	// must NOT protect time window [0, 0] (transient observation at t=0ms).
	regions := []domain.TrackedTextRegion{
		{
			ID:          "region-094",
			FirstSeenMs: 66000,
			LastSeenMs:  66000,
			ProtectedMetadata: domain.ProtectedRegionMetadata{
				IsProtected: true,
			},
			Keyframes: []domain.RegionKeyframe{
				{
					TimestampMs: 66000,
					Box:         domain.BoundingBox{X: 816, Y: 168, Width: 129, Height: 117},
				},
			},
		},
	}

	boxes := domain.GetProtectedBoxesForTimeWindow(regions, nil, 0, 0, "region-002")
	if len(boxes) != 0 {
		t.Fatalf("expected 0 protected boxes for window [0, 0] when protected region is at 66000ms, got %d", len(boxes))
	}
}

func TestClassifyRegion_SpatioTemporalInstability(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1920, 1080)

	// Control 1: Moving pseudo-text with 3+ adjacent observations, overlapping/near boxes, materially changing Latin gibberish => IgnoreNoise
	box1 := domain.BoundingBox{X: 1600, Y: 200, Width: 120, Height: 25}
	nearbyGibberish := []domain.NearbyObservation{
		{Text: "XybVqwer", TimestampMs: 19000, Box: domain.BoundingBox{X: 1650, Y: 185, Width: 125, Height: 25}},
		{Text: "ZopTyuik", TimestampMs: 20000, Box: domain.BoundingBox{X: 1550, Y: 220, Width: 120, Height: 25}},
	}
	role1, prot1, _, reason1 := domain.ClassifyRegionWithInstability("MnoPlkjh", box1, 0.58, cfg, 0, nearbyGibberish, nil)
	if role1 != domain.TextRoleIgnoreNoise {
		t.Errorf("Control 1: expected IgnoreNoise for unstable pseudo-text sequence, got %v (reason: %s)", role1, reason1)
	}
	if prot1.IsProtected {
		t.Errorf("Control 1: expected IsProtected=false for noise")
	}

	// Control 2: Stable unknown brand across frames => preserved semantic/brand path
	box2 := domain.BoundingBox{X: 500, Y: 300, Width: 150, Height: 40}
	nearbyStableBrand := []domain.NearbyObservation{
		{Text: "NovaBrandX", TimestampMs: 1000, Box: domain.BoundingBox{X: 502, Y: 301, Width: 150, Height: 40}},
		{Text: "NovaBrandX", TimestampMs: 1500, Box: domain.BoundingBox{X: 504, Y: 302, Width: 150, Height: 40}},
	}
	role2, _, _, _ := domain.ClassifyRegionWithInstability("NovaBrandX", box2, 0.58, cfg, 0, nearbyStableBrand, nil)
	if role2 == domain.TextRoleIgnoreNoise {
		t.Errorf("Control 2: stable unknown brand must NOT be classified IgnoreNoise, got %v", role2)
	}
	if role2 != domain.TextRoleSemanticText {
		t.Errorf("Control 2: expected SemanticText for stable brand, got %v", role2)
	}

	// Control 3: Isolated single-frame unknown text => not automatically noise
	box3 := domain.BoundingBox{X: 800, Y: 400, Width: 100, Height: 30}
	role3, _, _, _ := domain.ClassifyRegionWithInstability("UniqueSign", box3, 0.55, cfg, 0, nil, nil)
	if role3 == domain.TextRoleIgnoreNoise {
		t.Errorf("Control 3: isolated single-frame unknown text must NOT be automatically noise, got %v", role3)
	}

	// Control 4: Real subtitle in subtitle band => not noise
	box4 := domain.BoundingBox{X: 400, Y: 900, Width: 500, Height: 60}
	role4, _, _, _ := domain.ClassifyRegionWithInstability("这是一个字幕句子", box4, 0.55, cfg, 0, nearbyGibberish, nil)
	if role4 != domain.TextRoleSpeechSubtitle {
		t.Errorf("Control 4: subtitle band text must be classified SpeechSubtitle, got %v", role4)
	}

	// Control 5: Existing low-confidence floor behavior unchanged
	box5 := domain.BoundingBox{X: 500, Y: 300, Width: 150, Height: 40}
	role5, _, _, reason5 := domain.ClassifyRegionWithInstability("SomeText", box5, 0.25, cfg, 0, nil, nil)
	if role5 != domain.TextRoleIgnoreNoise || reason5 != "confidence_below_noise_floor" {
		t.Errorf("Control 5: expected low-confidence floor IgnoreNoise, got %v (reason: %s)", role5, reason5)
	}
}

// The recognizer failing to read a caption must not delete the caption. Live evidence (run
// 4f86657f, 1080x1440): `你就得到这样的互动画面` was detected at the correct box (228,951,606,65) on
// four consecutive 500 ms samples while the recognizer returned garbage ("别品同", "别品", "济室",
// "点酒房") at 0.15-0.31 confidence. The cover hides the box, not the text, so the region keeps the
// caption role and is surfaced for operator review.
func TestClassifyRegion_StableUnreadableCaptionKeepsCaptionRole(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	captionBox := domain.BoundingBox{X: 228, Y: 951, Width: 606, Height: 65}

	tests := []struct {
		name            string
		text            string
		box             domain.BoundingBox
		confidence      float64
		observedSamples int
		wantRole        domain.TextRegionRole
		wantReview      bool
		wantReason      string
	}{
		{
			name:            "stable unreadable caption stays a caption",
			text:            "别品同",
			box:             captionBox,
			confidence:      0.15,
			observedSamples: 4,
			wantRole:        domain.TextRoleSpeechSubtitle,
			wantReview:      true,
			wantReason:      "unreadable_caption_low_confidence",
		},
		{
			name:            "one unreadable sample is flicker, not a caption",
			text:            "别品同",
			box:             captionBox,
			confidence:      0.15,
			observedSamples: 1,
			wantRole:        domain.TextRoleIgnoreNoise,
			wantReason:      "confidence_below_noise_floor",
		},
		{
			name:            "narrow low-confidence token in the band is not caption-shaped",
			text:            "BLGOK",
			box:             domain.BoundingBox{X: 513, Y: 855, Width: 130, Height: 56},
			confidence:      0.2,
			observedSamples: 4,
			wantRole:        domain.TextRoleIgnoreNoise,
			wantReason:      "confidence_below_noise_floor",
		},
		{
			name:            "low-confidence block out of the band stays noise",
			text:            "Cott6e",
			box:             domain.BoundingBox{X: 103, Y: 84, Width: 623, Height: 313},
			confidence:      0.2,
			observedSamples: 4,
			wantRole:        domain.TextRoleIgnoreNoise,
			wantReason:      "confidence_below_noise_floor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			role, _, review, reason := domain.ClassifyRegionWithInstability(
				tc.text, tc.box, tc.confidence, cfg, tc.observedSamples, nil, nil)
			if role != tc.wantRole {
				t.Errorf("role = %s, want %s", role, tc.wantRole)
			}
			if review != tc.wantReview {
				t.Errorf("reviewRequired = %v, want %v", review, tc.wantReview)
			}
			if reason != tc.wantReason {
				t.Errorf("reviewReason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// High confidence is not evidence of readable content. The same package pixels were read as
// "BLGOK" (0.987), "CottGG" (0.827), "Cottce" (0.890) and "Cott6e" (0.781) in run 4f86657f, so the
// old confidence-gated instability rule let "BLGOK" through as semantic_text and the render printed
// "BLG OK" onto the milk jug.
func TestClassifyRegion_HighConfidenceUnstableLatinIsPseudoText(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 513, Y: 855, Width: 130, Height: 56}
	// The neighbouring readings of the same box, on adjacent samples.
	nearby := []domain.NearbyObservation{
		{Text: "Cott66", TimestampMs: 11000, Box: domain.BoundingBox{X: 512, Y: 813, Width: 142, Height: 61}},
		{Text: "CottGG", TimestampMs: 11500, Box: domain.BoundingBox{X: 520, Y: 825, Width: 142, Height: 59}},
		{Text: "Cottce", TimestampMs: 12000, Box: domain.BoundingBox{X: 411, Y: 573, Width: 215, Height: 95}},
	}

	role, _, _, reason := domain.ClassifyRegionWithInstability("BLGOK", box, 0.987, cfg, 5, nearby, nil)
	if role != domain.TextRoleIgnoreNoise {
		t.Errorf("role = %s, want %s for mutually disagreeing readings of one box", role, domain.TextRoleIgnoreNoise)
	}
	if reason != "spatio_temporal_ocr_instability_noise" {
		t.Errorf("reviewReason = %q, want %q", reason, "spatio_temporal_ocr_instability_noise")
	}
}

// Live evidence (run 4f86657f, 1080x1440): the mug label was read at 0.98-1.00 confidence on five
// consecutive samples at five places hundreds of pixels apart, as the mug moved. No neighbour
// overlapped those boxes, so the adjacency rule stayed quiet and the overlay printed "BLG OK" onto
// the video at a place the label never stayed at. Readings that wander cannot name one label.
func TestClassifyRegion_WanderingLabelIsNotAnOverlayCandidate(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 513, Y: 855, Width: 130, Height: 56}
	ownBoxes := []domain.BoundingBox{
		{X: 513, Y: 855, Width: 130, Height: 56},
		{X: 523, Y: 867, Width: 128, Height: 53},
		{X: 415, Y: 634, Width: 197, Height: 92},
		{X: 120, Y: 259, Width: 554, Height: 282},
		{X: 290, Y: 0, Width: 369, Height: 177},
	}

	role, _, _, reason := domain.ClassifyRegionWithInstability("BLGOK", box, 0.987, cfg, 5, nil, ownBoxes)
	if role != domain.TextRoleIgnoreNoise {
		t.Errorf("role = %s, want %s for a label read in five different places", role, domain.TextRoleIgnoreNoise)
	}
	if reason != "spatially_unstable_label_noise" {
		t.Errorf("reviewReason = %q, want %q", reason, "spatially_unstable_label_noise")
	}
}

// A logo sits in one place: the same reading a few pixels apart on every sample must keep its role.
func TestClassifyRegion_StationaryLabelKeepsRole(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 200, Y: 300, Width: 220, Height: 40}
	ownBoxes := []domain.BoundingBox{
		{X: 200, Y: 300, Width: 220, Height: 40},
		{X: 202, Y: 301, Width: 219, Height: 41},
		{X: 199, Y: 302, Width: 221, Height: 40},
	}

	role, _, _, _ := domain.ClassifyRegionWithInstability("NET WEIGHT 100g", box, 0.95, cfg, 3, nil, ownBoxes)
	if role != domain.TextRoleSemanticText {
		t.Errorf("role = %s, want %s for a stationary readable label", role, domain.TextRoleSemanticText)
	}
}

// A label the viewer can read is stable: OCR keeps spelling it the same way (at most trivial
// jitter), so the instability rule must not delete it.
func TestClassifyRegion_StableLatinLabelSurvivesInstabilityGate(t *testing.T) {
	cfg := domain.DefaultTextRegionClassifyConfig(1080, 1440)
	box := domain.BoundingBox{X: 200, Y: 300, Width: 220, Height: 40}
	nearby := []domain.NearbyObservation{
		{Text: "NET WEIGHT 100g", TimestampMs: 500, Box: box},
		{Text: "NET WEIGHT 100g", TimestampMs: 1000, Box: box},
		{Text: "NET WEIGHT 1OOg", TimestampMs: 1500, Box: box},
	}

	role, _, _, _ := domain.ClassifyRegionWithInstability("NET WEIGHT 100g", box, 0.95, cfg, 4, nearby, nil)
	if role != domain.TextRoleSemanticText {
		t.Errorf("role = %s, want %s for a stable readable label", role, domain.TextRoleSemanticText)
	}
}

// A long replacement stacks at word boundaries and the box hugs its widest line, so the caption
// stays inside the frame instead of running off one over-wide line.
func TestComputeCompactSubtitleBounds_WrapsLongTextIntoStackedLines(t *testing.T) {
	frameW, frameH := 1080, 1440
	long := "Đặt điện thoại lên tủ bếp, bạn có góc nhìn từ trên cao và khung hình đẹp hơn hẳn mỗi ngày"

	cue, err := domain.ComputeCompactSubtitleBounds(frameW, frameH, long, 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("expected successful placement, got error: %v", err)
	}
	lines := strings.Split(cue.Text, "\n")
	if len(lines) < 2 {
		t.Fatalf("long caption stayed on one line: %q", cue.Text)
	}
	if got := strings.Join(strings.Fields(cue.Text), " "); got != long {
		t.Errorf("wrapping lost words: %q != %q", got, long)
	}
	if cue.Width > int(float64(frameW)*0.80)+1 {
		t.Errorf("wrapped box width %d exceeds the 80%% frame budget", cue.Width)
	}
	wantHeight := len(lines)*cue.FontSizePx + (len(lines)-1)*int(float64(cue.FontSizePx)*0.3) + 2*cue.PaddingY
	if cue.Height != wantHeight {
		t.Errorf("box height %d does not cover %d lines (want %d)", cue.Height, len(lines), wantHeight)
	}
	// The replacement is drawn at the visual weight of the burned-in caption it replaces.
	if want := int(float64(frameH) * 0.037); cue.FontSizePx != want {
		t.Errorf("font size %d, want the source-matched %d", cue.FontSizePx, want)
	}

	short, err := domain.ComputeCompactSubtitleBounds(frameW, frameH, "Bước 1: Trộn trà", 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("expected successful placement for short text, got error: %v", err)
	}
	if strings.Contains(short.Text, "\n") {
		t.Errorf("short caption wrapped unnecessarily: %q", short.Text)
	}
}

// A single word longer than the line cap (a run-together URL, a caption with no spaces) is chunked
// rune-wise instead of left whole: an unbroken word makes the declared box hug a line the renderer
// cannot draw inside it, so the replacement overflows the frame.
func TestComputeCompactSubtitleBounds_ChunksAnOverlongSingleWord(t *testing.T) {
	frameW, frameH := 1080, 1440
	word := strings.Repeat("a", 150)

	cue, err := domain.ComputeCompactSubtitleBounds(frameW, frameH, word, 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("expected successful placement, got error: %v", err)
	}
	lines := strings.Split(cue.Text, "\n")
	if len(lines) < 2 {
		t.Fatalf("over-long word was left unbroken: %d line(s), %q", len(lines), cue.Text)
	}
	if got := strings.ReplaceAll(cue.Text, "\n", ""); got != word {
		t.Errorf("chunking lost runes: %q != %q", got, word)
	}
	if cue.Width > int(float64(frameW)*0.80)+1 {
		t.Errorf("box width %d exceeds the 80%% frame budget", cue.Width)
	}
	// The declared box must hold the widest line it will draw: charWidth is the same estimate the
	// box is measured with (45% of the font size), so a box narrower than this is the overflow.
	charWidth := int(float64(cue.FontSizePx) * 0.45)
	widest := 0
	for _, line := range lines {
		widest = max(widest, len([]rune(line)))
	}
	if drawn := widest * charWidth; drawn > cue.Width {
		t.Errorf("declared box %dpx cannot hold its widest %d-rune line (%dpx drawn)", cue.Width, widest, drawn)
	}
}
