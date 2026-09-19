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

// Two consecutive captions in the same band must not draw two bars at once, and the handover between them
// must stay covered: their padded windows overlap on the sampling step, and each box alone leaves the other
// caption's line exposed (live evidence, live-20260918: region-002 x=305 [0,2000] over region-004 x=231
// [1500,3500] drew a ragged doubled bar, and covering each box alone exposed 你 / 角 outside the bar at 1.6s).
// The contested span gets one union bar; each caption keeps its own box outside it.
func TestBuildSubtitleCovers_SequentialCaptionsHandOverThroughUnionBar(t *testing.T) {
	first := subtitleRegion("region-a", domain.BoundingBox{X: 305, Y: 946, Width: 453, Height: 76}, 0, 1500)
	second := subtitleRegion("region-b", domain.BoundingBox{X: 231, Y: 948, Width: 603, Height: 72}, 2000, 3000)
	plan := coverTestPlan(first, second)

	covers, _ := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(covers) != 3 {
		t.Fatalf("expected both caption covers plus one handover bar, got %d: %+v", len(covers), covers)
	}
	// Exactly one bar is composited at any instant.
	for i := range covers {
		for j := i + 1; j < len(covers); j++ {
			if covers[i].StartMs < covers[j].EndMs && covers[j].StartMs < covers[i].EndMs {
				t.Fatalf("covers %s [%d,%d] and %s [%d,%d] are composited together",
					covers[i].RegionID, covers[i].StartMs, covers[i].EndMs, covers[j].RegionID, covers[j].StartMs, covers[j].EndMs)
			}
		}
	}
	// Region A carries no pad of its own (its 1500ms grid step exceeds the pad cap), B pads 1000ms, so the
	// contested span is [1000,1500] and both pads survive on the outside of it.
	byID := map[string]domain.CoverBox{}
	for _, c := range covers {
		byID[c.RegionID] = c
	}
	a, b, handover := byID["region-a"], byID["region-b"], byID["handover-region-a-region-b"]
	if a.RegionID == "" || b.RegionID == "" || handover.RegionID == "" {
		t.Fatalf("expected two caption covers and one handover bar, got %+v", covers)
	}
	if a.StartMs != 0 || a.EndMs != 1000 {
		t.Errorf("expected region-a to keep its own span [0,1000], got %+v", a)
	}
	if handover.StartMs != 1000 || handover.EndMs != 1500 {
		t.Errorf("expected the handover bar over the contested [1000,1500], got %+v", handover)
	}
	if b.StartMs != 1500 || b.EndMs != 4000 {
		t.Errorf("expected region-b to keep its own span [1500,4000], got %+v", b)
	}
	// The handover bar is the union of both caption boxes: neither line can peek out of it.
	wantX, wantRight := 231-coverPaddingPx, 231+603+coverPaddingPx
	if handover.X != wantX || handover.X+handover.Width != wantRight {
		t.Errorf("expected the handover to span both boxes [%d,%d], got [%d,%d]", wantX, wantRight, handover.X, handover.X+handover.Width)
	}
	wantTop, wantBottom := 946-coverPaddingPx, 946+76+coverPaddingPx
	if handover.Y != wantTop || handover.Y+handover.Height != wantBottom {
		t.Errorf("expected the handover to span both bands vertically [%d,%d], got [%d,%d]", wantTop, wantBottom, handover.Y, handover.Y+handover.Height)
	}
}

// Captions that were genuinely on screen together keep their overlapping covers: splitting them would
// leave one caption's source text exposed while its replacement is drawn next to it.
func TestBuildSubtitleCovers_SimultaneousCaptionsKeepOverlappingCovers(t *testing.T) {
	left := subtitleRegion("region-l", domain.BoundingBox{X: 100, Y: 900, Width: 400, Height: 80}, 1000, 4000)
	right := subtitleRegion("region-r", domain.BoundingBox{X: 460, Y: 900, Width: 300, Height: 80}, 1000, 4000)
	plan := coverTestPlan(left, right)

	covers, _ := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(covers) != 2 {
		t.Fatalf("expected two covers, got %d", len(covers))
	}
	for _, c := range covers {
		if c.StartMs != 1000 || c.EndMs != 4000 {
			t.Errorf("simultaneous caption cover %s must keep its observed window, got [%d,%d]", c.RegionID, c.StartMs, c.EndMs)
		}
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

// The replacement text belongs on the block that replaced the source caption, not in a lane under
// it: one dark block per replaced caption (live evidence, run 27a758e6 at 1.6s - cover
// [225,936,639,96] over the burned-in caption, cue [161,1080,757,45] beneath it).
func TestSeatReplacementCuesOnCovers_DrawsTextOnTheCoverItReplaces(t *testing.T) {
	plan := coverTestPlan(subtitleRegion("region-004", domain.BoundingBox{X: 231, Y: 942, Width: 627, Height: 84}, 1500, 3500))
	covers, occlusions := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	if len(covers) != 1 || len(occlusions) != 0 {
		t.Fatalf("expected one cover and no occlusions, got %d covers / %d occlusions", len(covers), len(occlusions))
	}
	cover := covers[0]
	centerX := cover.X + cover.Width/2
	cues := []domain.SubtitleCue{{
		ID: "cue-0", StartMs: 0, EndMs: 2902,
		Text: "Đặt điện thoại lên tủ bếp, bạn có góc nhìn từ trên cao.",
		X:    161, Y: 1080, Width: 757, Height: 45, FontSizePx: 25, PaddingX: 18,
	}}

	seated, boxes := seatReplacementCuesOnCovers(cues, covers, plan, nil)
	if len(seated) != 1 || len(boxes) != 1 {
		t.Fatalf("expected one seated cue and one cover, got %d / %d", len(seated), len(boxes))
	}
	got := seated[0]
	if got.X == cues[0].X && got.Y == cues[0].Y {
		t.Fatalf("cue stayed in its lane at %d,%d: the replacement must sit on its cover", got.X, got.Y)
	}
	// Integer geometry: the seat centers the cue within a pixel of the cover's center.
	if gotCenterX := got.X + got.Width/2; abs(gotCenterX-centerX) > 1 {
		t.Errorf("seated cue center x = %d, want the cover center %d", gotCenterX, centerX)
	}
	if wantCenterY := cover.Y + cover.Height/2; abs(got.Y+got.Height/2-wantCenterY) > 1 {
		t.Errorf("seated cue center y = %d, want the cover center %d", got.Y+got.Height/2, wantCenterY)
	}
	grown := domain.BoundingBox{X: boxes[0].X, Y: boxes[0].Y, Width: boxes[0].Width, Height: boxes[0].Height}
	if !boxContains(grown, domain.BoundingBox{X: got.X, Y: got.Y, Width: got.Width, Height: got.Height}) {
		t.Errorf("cover %v does not frame the seated cue %d,%d %dx%d", grown, got.X, got.Y, got.Width, got.Height)
	}
	if !boxContains(grown, domain.BoundingBox{X: cover.X, Y: cover.Y, Width: cover.Width, Height: cover.Height}) {
		t.Errorf("grown cover %v no longer contains the source caption box", grown)
	}
	if got.StartMs != cues[0].StartMs || got.EndMs != cues[0].EndMs {
		t.Errorf("seating changed the cue window to [%d,%d]", got.StartMs, got.EndMs)
	}
}

// A seat that would occlude a protected obstacle keeps the lane placement the lane search chose.
func TestSeatReplacementCuesOnCovers_RefusesASeatOverAProtectedObstacle(t *testing.T) {
	plan := coverTestPlan(subtitleRegion("region-004", domain.BoundingBox{X: 231, Y: 942, Width: 627, Height: 84}, 1500, 3500))
	covers, _ := buildSubtitleCovers(plan, LocalizeVisualTrackInput{})
	cues := []domain.SubtitleCue{{
		ID: "cue-0", StartMs: 0, EndMs: 2902, Text: "Đặt điện thoại lên tủ bếp",
		X: 161, Y: 1080, Width: 757, Height: 45, FontSizePx: 25, PaddingX: 18,
	}}
	sceneProtected := []domain.SceneProtectedRegion{
		{ID: "face-1", Reason: "face", StartMs: 0, EndMs: 5000, Box: domain.BoundingBox{X: 200, Y: 950, Width: 400, Height: 100}},
	}

	seated, boxes := seatReplacementCuesOnCovers(cues, covers, plan, sceneProtected)
	if seated[0].X != cues[0].X || seated[0].Y != cues[0].Y {
		t.Errorf("cue moved onto a seat that occludes a protected obstacle: %d,%d", seated[0].X, seated[0].Y)
	}
	if boxes[0] != covers[0] {
		t.Errorf("cover changed although the seat was refused: %+v", boxes[0])
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func boxContains(outer domain.BoundingBox, inner domain.BoundingBox) bool {
	return inner.X >= outer.X && inner.Y >= outer.Y &&
		inner.X+inner.Width <= outer.X+outer.Width && inner.Y+inner.Height <= outer.Y+outer.Height
}

func TestResolveSequentialCoverOverlaps_CollapsedCoverFiltered(t *testing.T) {
	covers := []domain.CoverBox{
		{
			RegionID: "reg-1",
			Role:     string(domain.TextRoleSpeechSubtitle),
			X:        100,
			Y:        100,
			Width:    200,
			Height:   50,
			StartMs:  500,
			EndMs:    1000,
			Color:    "#000000",
			Opacity:  1.0,
		},
		{
			RegionID: "reg-2",
			Role:     string(domain.TextRoleSpeechSubtitle),
			X:        120,
			Y:        100,
			Width:    200,
			Height:   50,
			StartMs:  0,
			EndMs:    3000,
			Color:    "#000000",
			Opacity:  1.0,
		},
	}
	observed := [][2]int64{
		{500, 1000},
		{1100, 2000},
	}

	resolved := resolveSequentialCoverOverlaps(covers, observed)

	for _, c := range resolved {
		if c.EndMs <= c.StartMs {
			t.Fatalf("cover %s has collapsed time window [%d, %d]", c.RegionID, c.StartMs, c.EndMs)
		}
		if err := domain.ValidateCoverBox(c, 1920, 1080); err != nil {
			t.Fatalf("ValidateCoverBox failed on cover %s [%d, %d]: %v", c.RegionID, c.StartMs, c.EndMs, err)
		}
	}
}
