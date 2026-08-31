package domain_test

import (
	"errors"
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

	h1, err := domain.ComputeLocalizedVisualTrackProvenanceHash("asset-1", "vi", "plan-prov-1", "dub-prov-1", overlays, cues)
	if err != nil {
		t.Fatalf("compute hash 1: %v", err)
	}
	h2, err := domain.ComputeLocalizedVisualTrackProvenanceHash("asset-1", "vi", "plan-prov-1", "dub-prov-1", overlays, cues)
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
