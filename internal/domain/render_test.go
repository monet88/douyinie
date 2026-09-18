package domain_test

import (
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestComputeRenderPlanProvenanceHash_Determinism(t *testing.T) {
	timeline := domain.RenderTimeline{
		DurationMs: 1500,
		Width:      640,
		Height:     360,
		FrameRate:  30.0,
	}
	cues1 := []domain.SubtitleCue{
		{StartMs: 0, EndMs: 500, Text: "Xin chào", X: 100, Y: 200, FontSizePx: 24, PaddingX: 18, PaddingY: 10},
		{StartMs: 600, EndMs: 1200, Text: "Thế giới", X: 120, Y: 200, FontSizePx: 24, PaddingX: 18, PaddingY: 10},
	}
	cues2 := []domain.SubtitleCue{
		{StartMs: 600, EndMs: 1200, Text: "Thế giới", X: 120, Y: 200, FontSizePx: 24, PaddingX: 18, PaddingY: 10},
		{StartMs: 0, EndMs: 500, Text: "Xin chào", X: 100, Y: 200, FontSizePx: 24, PaddingX: 18, PaddingY: 10},
	}

	cueHash1 := domain.ComputeCueSpecHash(cues1)
	cueHash2 := domain.ComputeCueSpecHash(cues2)
	if cueHash1 != cueHash2 {
		t.Errorf("expected order-independent deterministic cue hash, got %s != %s", cueHash1, cueHash2)
	}

	subPlanRef1 := domain.SubtitlePlanRef{
		ArtifactID:     "sub-art-1",
		SchemaVersion:  domain.SubtitlePlanSchemaVersion,
		CASHash:        "cas-sub-hash-1",
		ProvenanceHash: "sub-prov-1",
		Format:         "compact_fit_cues",
		CueCount:       len(cues1),
		CueSpecHash:    cueHash1,
	}
	subPlanRef2 := domain.SubtitlePlanRef{
		ArtifactID:     "sub-art-1",
		SchemaVersion:  domain.SubtitlePlanSchemaVersion,
		CASHash:        "cas-sub-hash-1",
		ProvenanceHash: "sub-prov-1",
		Format:         "compact_fit_cues",
		CueCount:       len(cues2),
		CueSpecHash:    cueHash2,
	}
	covers := []domain.CoverBox{
		{RegionID: "region-001", Role: "speech_subtitle", X: 100, Y: 200, Width: 300, Height: 60, StartMs: 0, EndMs: 500, Color: "#000000", Opacity: 1.0},
	}
	overlays := []domain.SubtitleCue{
		{ID: "overlay-0", StartMs: 600, EndMs: 1200, Text: "Độ mờ", X: 120, Y: 200, Width: 100, Height: 30, FontSizePx: 18},
	}

	h1, err := domain.ComputeRenderPlanProvenanceHash("asset-1", "vi", "sha-video", "cas-mix", "cas-audio", timeline, subPlanRef1, covers, overlays)
	if err != nil {
		t.Fatalf("compute hash 1: %v", err)
	}
	h2, err := domain.ComputeRenderPlanProvenanceHash("asset-1", "vi", "sha-video", "cas-mix", "cas-audio", timeline, subPlanRef2, covers, overlays)
	if err != nil {
		t.Fatalf("compute hash 2: %v", err)
	}

	if h1 != h2 {
		t.Errorf("expected identical hash for identical plan ref, got %s != %s", h1, h2)
	}

	// Change subtitle CAS hash -> different hash
	subPlanModified := subPlanRef1
	subPlanModified.CASHash = "cas-sub-hash-different"
	h3, _ := domain.ComputeRenderPlanProvenanceHash("asset-1", "vi", "sha-video", "cas-mix", "cas-audio", timeline, subPlanModified, covers, overlays)
	if h1 == h3 {
		t.Errorf("expected different hash for modified subtitle CAS hash, got identical %s", h1)
	}

	// Change audio CAS ref -> different hash
	h4, _ := domain.ComputeRenderPlanProvenanceHash("asset-1", "vi", "sha-video", "cas-mix", "cas-audio-other", timeline, subPlanRef1, covers, overlays)
	if h1 == h4 {
		t.Errorf("expected different hash for modified audio ref, got identical %s", h1)
	}

	// Change covers -> different hash
	coversModified := []domain.CoverBox{
		{RegionID: "region-001", Role: "speech_subtitle", X: 100, Y: 210, Width: 300, Height: 60, StartMs: 0, EndMs: 500, Color: "#000000", Opacity: 1.0},
	}
	h5, _ := domain.ComputeRenderPlanProvenanceHash("asset-1", "vi", "sha-video", "cas-mix", "cas-audio", timeline, subPlanRef1, coversModified, overlays)
	if h1 == h5 {
		t.Errorf("expected different hash for modified cover box, got identical %s", h1)
	}

	// Change overlays -> different hash
	overlaysModified := []domain.SubtitleCue{
		{ID: "overlay-0", StartMs: 600, EndMs: 1200, Text: "Độ mờ khác", X: 120, Y: 200, Width: 100, Height: 30, FontSizePx: 18},
	}
	h6, _ := domain.ComputeRenderPlanProvenanceHash("asset-1", "vi", "sha-video", "cas-mix", "cas-audio", timeline, subPlanRef1, covers, overlaysModified)
	if h1 == h6 {
		t.Errorf("expected different hash for modified overlay cue, got identical %s", h1)
	}
}

func TestComputeSubtitlePlanProvenanceHash_Determinism(t *testing.T) {
	cues1 := []domain.SubtitleCue{
		{StartMs: 0, EndMs: 500, Text: "Xin chào", X: 100, Y: 200, FontSizePx: 24},
		{StartMs: 600, EndMs: 1200, Text: "Thế giới", X: 120, Y: 200, FontSizePx: 24},
	}
	cues2 := []domain.SubtitleCue{
		{StartMs: 600, EndMs: 1200, Text: "Thế giới", X: 120, Y: 200, FontSizePx: 24},
		{StartMs: 0, EndMs: 500, Text: "Xin chào", X: 100, Y: 200, FontSizePx: 24},
	}

	h1, err := domain.ComputeSubtitlePlanProvenanceHash("asset-1", "vi", cues1, "compact_fit_cues")
	if err != nil {
		t.Fatalf("compute sub prov 1: %v", err)
	}
	h2, err := domain.ComputeSubtitlePlanProvenanceHash("asset-1", "vi", cues2, "compact_fit_cues")
	if err != nil {
		t.Fatalf("compute sub prov 2: %v", err)
	}

	if h1 != h2 {
		t.Errorf("expected order-independent deterministic subtitle plan provenance, got %s != %s", h1, h2)
	}
}

func TestComputeRenderArtifactProvenanceHash_DistinctKinds(t *testing.T) {
	planProv := "plan-prov-12345"
	prevProf := domain.DefaultPreviewEncodeProfile()
	finProf := domain.DefaultFinalEncodeProfile()

	hPrev, err := domain.ComputeRenderArtifactProvenanceHash(planProv, domain.RenderKindPreview, prevProf)
	if err != nil {
		t.Fatalf("preview artifact prov: %v", err)
	}
	hFin, err := domain.ComputeRenderArtifactProvenanceHash(planProv, domain.RenderKindFinal, finProf)
	if err != nil {
		t.Fatalf("final artifact prov: %v", err)
	}

	if hPrev == hFin {
		t.Errorf("expected distinct provenance hashes for preview vs final, got identical %s", hPrev)
	}
}
