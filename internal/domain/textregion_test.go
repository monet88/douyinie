package domain_test

import (
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
