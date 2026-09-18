package service

import (
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func overlayTestItem(id string, box domain.BoundingBox, startMs, endMs int64) domain.LocalizedOverlayItem {
	return domain.LocalizedOverlayItem{
		RegionID: id, Role: domain.TextRoleSemanticText, SourceText: "面饼：107克", LocalizedText: "Vắt mì: 107g",
		StartMs: startMs, EndMs: endMs, Box: box, IsCoverDefault: true,
	}
}

// Two in-place replacements that share the screen must not composite on top of each other: the ramen packs
// four semantic_text regions all live at 4500-5500ms and drew nested black boxes that clipped each others
// translated text (live-20260918). The dominant label wins and the loser becomes an operator exception
// instead of a cut-off translation.
func TestResolveOverlayCollisions_DominantLabelWinsAndLoserIsReported(t *testing.T) {
	dominant := overlayTestItem("region-008", domain.BoundingBox{X: 242, Y: 479, Width: 308, Height: 181}, 4500, 5500)
	nested := overlayTestItem("region-009", domain.BoundingBox{X: 267, Y: 489, Width: 236, Height: 202}, 4500, 5500)
	apart := overlayTestItem("region-011", domain.BoundingBox{X: 306, Y: 623, Width: 92, Height: 85}, 900, 1500)

	kept, collisions := resolveOverlayCollisions([]domain.LocalizedOverlayItem{nested, dominant, apart})
	if len(kept) != 2 {
		t.Fatalf("expected the dominant replacement plus the non-colliding one, got %+v", kept)
	}
	if kept[0].RegionID != "region-008" {
		t.Errorf("expected the larger observed box to win, got %s", kept[0].RegionID)
	}
	if len(collisions) != 1 || collisions[0].RegionID != "region-009" {
		t.Fatalf("expected the nested region reported as an occlusion, got %+v", collisions)
	}
	if collisions[0].ProtectedBox != dominant.Box {
		t.Errorf("the report must name the box that won, got %+v", collisions[0].ProtectedBox)
	}
	if collisions[0].StartMs != 4500 || collisions[0].EndMs != 5500 {
		t.Errorf("the report must carry the dropped region window, got %d-%d", collisions[0].StartMs, collisions[0].EndMs)
	}
}

// Replacements that never share the screen keep both boxes.
func TestResolveOverlayCollisions_SequentialReplacementsBothKeep(t *testing.T) {
	first := overlayTestItem("region-a", domain.BoundingBox{X: 200, Y: 300, Width: 300, Height: 100}, 0, 2000)
	second := overlayTestItem("region-b", domain.BoundingBox{X: 210, Y: 300, Width: 300, Height: 100}, 2000, 4000)

	kept, collisions := resolveOverlayCollisions([]domain.LocalizedOverlayItem{first, second})
	if len(kept) != 2 || len(collisions) != 0 {
		t.Fatalf("expected both replacements kept, got %d kept / %d collisions", len(kept), len(collisions))
	}
}
