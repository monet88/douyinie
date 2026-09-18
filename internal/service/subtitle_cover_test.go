package service

import (
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func coverTestPlan(regions ...domain.TrackedTextRegion) *domain.TextRegionPlan {
	return &domain.TextRegionPlan{
		FrameWidth:  1080,
		FrameHeight: 1440,
		Regions:     regions,
	}
}

func subtitleRegion(id string, box domain.BoundingBox, startMs, endMs int64) domain.TrackedTextRegion {
	return domain.TrackedTextRegion{
		ID:          id,
		Text:        "把手机放在橱柜上",
		Role:        domain.TextRoleSpeechSubtitle,
		FirstSeenMs: startMs,
		LastSeenMs:  endMs,
		Keyframes: []domain.RegionKeyframe{
			{TimestampMs: startMs, Box: box, Observed: true},
			{TimestampMs: endMs, Box: box, Observed: true},
		},
	}
}

// A burned-in caption is hidden exactly where it sits: the cover tracks the region's own box,
// so a replacement cue that fits lower is drawn over video that never carried source text.
func TestBuildSubtitleCovers_TracksSourceRegionBox(t *testing.T) {
	regionBox := domain.BoundingBox{X: 100, Y: 800, Width: 600, Height: 90}
	plan := coverTestPlan(subtitleRegion("region-sub", regionBox, 0, 12000))

	covers, occlusions := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(occlusions) != 0 {
		t.Fatalf("expected no occlusion reports, got %+v", occlusions)
	}
	if len(covers) != 1 {
		t.Fatalf("expected exactly one cover for the speech_subtitle region, got %d", len(covers))
	}
	cover := covers[0]
	if cover.Role != string(domain.TextRoleSpeechSubtitle) {
		t.Errorf("expected cover role %q, got %q", domain.TextRoleSpeechSubtitle, cover.Role)
	}
	if cover.Opacity != 1.0 || cover.Color != "#000000" {
		t.Errorf("expected an opaque black cover, got color=%q opacity=%.2f", cover.Color, cover.Opacity)
	}
	if cover.StartMs != 0 || cover.EndMs != 12000 {
		t.Errorf("expected the source window [0,12000], got [%d,%d]", cover.StartMs, cover.EndMs)
	}
	// The cover hugs the source caption (region box + anti-alias padding) instead of swallowing
	// the replacement cue's larger fit box.
	wantX, wantY := regionBox.X-coverPaddingPx, regionBox.Y-coverPaddingPx
	wantW, wantH := regionBox.Width+2*coverPaddingPx, regionBox.Height+2*coverPaddingPx
	if cover.X != wantX || cover.Y != wantY || cover.Width != wantW || cover.Height != wantH {
		t.Errorf("expected cover %d,%d %dx%d hugging the region box, got %d,%d %dx%d",
			wantX, wantY, wantW, wantH, cover.X, cover.Y, cover.Width, cover.Height)
	}
	if err := domain.ValidateCoverBox(cover, plan.FrameWidth, plan.FrameHeight); err != nil {
		t.Errorf("cover box must be compositable: %v", err)
	}
}

// A caption tracked across drifting keyframes is covered across the union of those observations:
// a cover that follows only one keyframe leaves the caption's own stroke edges showing.
func TestBuildSubtitleCovers_CoversUnionOfObservedKeyframes(t *testing.T) {
	reg := subtitleRegion("region-sub", domain.BoundingBox{X: 100, Y: 800, Width: 600, Height: 90}, 1000, 3000)
	reg.Keyframes = append(reg.Keyframes, domain.RegionKeyframe{
		TimestampMs: 2000,
		Box:         domain.BoundingBox{X: 96, Y: 796, Width: 640, Height: 96},
		Observed:    true,
	})
	plan := coverTestPlan(reg)

	covers, _ := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(covers) != 1 {
		t.Fatalf("expected one cover, got %d", len(covers))
	}
	if covers[0].X != 96-coverPaddingPx || covers[0].Y != 796-coverPaddingPx {
		t.Errorf("expected the cover to span the drifted keyframes, got %+v", covers[0])
	}
	if covers[0].Width != 640+2*coverPaddingPx || covers[0].Height != 96+2*coverPaddingPx {
		t.Errorf("expected the cover to span the full observed extent, got %+v", covers[0])
	}
}

// Only speech_subtitle regions are covered by this pass; semantic/instructional replacements
// belong to the in-place overlay lane.
func TestBuildSubtitleCovers_IgnoresNonSubtitleRoles(t *testing.T) {
	plan := coverTestPlan(
		domain.TrackedTextRegion{
			ID: "region-semantic", Text: "面饼：107克", Role: domain.TextRoleSemanticText,
			FirstSeenMs: 0, LastSeenMs: 5000,
			Keyframes: []domain.RegionKeyframe{{TimestampMs: 0, Box: domain.BoundingBox{X: 200, Y: 300, Width: 300, Height: 100}, Observed: true}},
		},
	)

	covers, _ := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(covers) != 0 {
		t.Fatalf("expected non-subtitle roles to stay uncovered, got %+v", covers)
	}
}

// A cover that would sit on a protected obstacle is surfaced for review instead of painted: the
// operator can move either region, and silently covering a face/tap target is not acceptable.
func TestBuildSubtitleCovers_ProtectedOverlapSurfacesAsException(t *testing.T) {
	plan := coverTestPlan(subtitleRegion("region-sub", domain.BoundingBox{X: 100, Y: 800, Width: 600, Height: 90}, 0, 5000))
	in := LocalizeVisualTrackInput{
		SceneProtectedRegions: []domain.SceneProtectedRegion{
			{Reason: "face", StartMs: 0, EndMs: 5000, Box: domain.BoundingBox{X: 300, Y: 820, Width: 200, Height: 60}},
		},
	}

	covers, occlusions := buildSubtitleCovers(plan, in)
	if len(covers) != 0 {
		t.Fatalf("expected no cover over a scene-protected obstacle, got %+v", covers)
	}
	if len(occlusions) != 1 || occlusions[0].RegionID != "region-sub" {
		t.Fatalf("expected the collision to surface as an occlusion report for region-sub, got %+v", occlusions)
	}
}

// A region with no observed geometry cannot be covered: interpolated keyframes are not evidence.
func TestBuildSubtitleCovers_SkipsRegionsWithoutObservedGeometry(t *testing.T) {
	reg := subtitleRegion("region-sub", domain.BoundingBox{X: 100, Y: 800, Width: 600, Height: 90}, 0, 5000)
	for i := range reg.Keyframes {
		reg.Keyframes[i].Observed = false
	}
	plan := coverTestPlan(reg)

	covers, _ := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(covers) != 0 {
		t.Fatalf("expected no cover without observed geometry, got %+v", covers)
	}
}

// The caption's true on-screen window extends up to one sampling step beyond its first and last
// detection: the OCR grid proves the caption was there at 11000 and 12500 ms, not that it appeared
// at 11000 and vanished at 12500. Live evidence (run 4f86657f): the caption was still on screen at
// 12750 ms and already on screen at 10750 ms, so an unpadded cover leaves source Chinese visible at
// both ends.
func TestBuildSubtitleCovers_PadsTheWindowByOneSamplingStep(t *testing.T) {
	box := domain.BoundingBox{X: 228, Y: 951, Width: 606, Height: 65}
	reg := subtitleRegion("region-sub", box, 11000, 12500)
	reg.Keyframes = []domain.RegionKeyframe{
		{TimestampMs: 11000, Box: box, Observed: true},
		{TimestampMs: 11500, Box: box, Observed: true},
		{TimestampMs: 12000, Box: box, Observed: true},
		{TimestampMs: 12500, Box: box, Observed: true},
	}

	covers, _ := buildSubtitleCovers(coverTestPlan(reg), LocalizeVisualTrackInput{})
	if len(covers) != 1 {
		t.Fatalf("expected one cover, got %d", len(covers))
	}
	if covers[0].StartMs != 10500 || covers[0].EndMs != 13000 {
		t.Errorf("cover window = %d-%d ms, want 10500-13000 ms (one 500 ms step of bracket on each side)",
			covers[0].StartMs, covers[0].EndMs)
	}
}

// A caption seen on exactly one sample still needs a renderable window: a zero-length cover is
// invalid, so a single detection covers the sampling interval it stands for.
func TestBuildSubtitleCovers_SingleSampleStillCoversOneStep(t *testing.T) {
	box := domain.BoundingBox{X: 228, Y: 951, Width: 606, Height: 65}
	single := subtitleRegion("region-single", box, 17000, 17000)
	single.Keyframes = []domain.RegionKeyframe{{TimestampMs: 17000, Box: box, Observed: true}}

	covers, _ := buildSubtitleCovers(coverTestPlan(single), LocalizeVisualTrackInput{})
	if len(covers) != 1 {
		t.Fatalf("expected one cover, got %d", len(covers))
	}
	if covers[0].StartMs != 17000 || covers[0].EndMs != 17500 {
		t.Errorf("cover window = %d-%d ms, want 17000-17500 ms (one default sampling step)",
			covers[0].StartMs, covers[0].EndMs)
	}
}

// A coarse or gappy track whose keys are not on the sampling grid must not claim a window far
// beyond its own evidence.
func TestBuildSubtitleCovers_DoesNotPadWithoutGridEvidence(t *testing.T) {
	box := domain.BoundingBox{X: 228, Y: 951, Width: 606, Height: 65}
	coarse := subtitleRegion("region-coarse", box, 11000, 23000)
	coarse.Keyframes = []domain.RegionKeyframe{
		{TimestampMs: 11000, Box: box, Observed: true},
		{TimestampMs: 23000, Box: box, Observed: true},
	}

	covers, _ := buildSubtitleCovers(coverTestPlan(coarse), LocalizeVisualTrackInput{})
	if len(covers) != 1 {
		t.Fatalf("expected one cover, got %d", len(covers))
	}
	if covers[0].StartMs != 11000 || covers[0].EndMs != 23000 {
		t.Errorf("cover window = %d-%d ms, want the unpadded 11000-23000 ms",
			covers[0].StartMs, covers[0].EndMs)
	}
}
