package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	// ErrTextRegionPlanNotFound is returned when a TextRegionPlan artifact is missing.
	ErrTextRegionPlanNotFound = errors.New("text region plan not found")
	// ErrOCRFailed is returned when the OCR provider fails to detect or track text regions.
	ErrOCRFailed = errors.New("ocr detection or tracking failed")
)

// TextRegionSchemaVersion is the canonical schema version for TextRegionPlan artifacts.
const TextRegionPlanSchemaVersion = 1

// TextRegionRole represents the classified operational role of an on-screen text region.
type TextRegionRole string

const (
	// TextRoleSpeechSubtitle: Dialogue captions (spoken dialogue/narration).
	// Rendered with compact fit-content subtitle box.
	TextRoleSpeechSubtitle TextRegionRole = "speech_subtitle"

	// TextRoleSemanticText: Content-critical visual text (steps, ingredient names, floating badges/titles).
	// Covered and localized in-place.
	TextRoleSemanticText TextRegionRole = "semantic_text"

	// TextRoleInstructionalUIText: Application UI buttons, menus, and controls taught in tutorials (e.g. CapCut/JianYing).
	// Covered and localized in-place using standard target terminology.
	TextRoleInstructionalUIText TextRegionRole = "instructional_ui_text"

	// TextRoleBrandKeep: Manufacturer logos, trademarks, and verified prop brands.
	// Preserved unchanged.
	TextRoleBrandKeep TextRegionRole = "brand_keep"

	// TextRoleIgnoreNoise: Watermarks, compression noise, and decorative non-content text.
	// Ignored without processing.
	TextRoleIgnoreNoise TextRegionRole = "ignore/noise"

	// TextRoleUncertain: Ambiguous role classification flagged for review.
	TextRoleUncertain TextRegionRole = "uncertain"
)

// IsValidTextRegionRole returns true if the role is a recognized Phase 1 text role.
func IsValidTextRegionRole(r TextRegionRole) bool {
	switch r {
	case TextRoleSpeechSubtitle, TextRoleSemanticText, TextRoleInstructionalUIText, TextRoleBrandKeep, TextRoleIgnoreNoise, TextRoleUncertain:
		return true
	default:
		return false
	}
}

// BoundingBox represents rectangular region coordinates in pixel space.
type BoundingBox struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// CenterY returns the vertical center coordinate of the bounding box.
func (b BoundingBox) CenterY() int {
	return b.Y + b.Height/2
}

// CenterX returns the horizontal center coordinate of the bounding box.
func (b BoundingBox) CenterX() int {
	return b.X + b.Width/2
}

// Area returns the area of the box in square pixels.
func (b BoundingBox) Area() int {
	return b.Width * b.Height
}

// RegionKeyframe captures the geometry and detection status of a text region on a specific sampled frame.
type RegionKeyframe struct {
	FrameIndex  int         `json:"frame_index"`
	TimestampMs int64       `json:"timestamp_ms"`
	Box         BoundingBox `json:"box"`
	Confidence  float64     `json:"confidence"`
	Observed    bool        `json:"observed"` // true if detected by OCR; false if linearly interpolated
}

// ProtectedRegionMetadata holds non-occlusion constraints and protection flags.
type ProtectedRegionMetadata struct {
	IsProtected bool   `json:"is_protected"`
	Reason      string `json:"reason,omitempty"` // e.g. "instructional_ui_control", "timeline_track", "tap_target"
}

// ConfidenceEvidence holds detection quality indicators and tracking continuity metrics.
type ConfidenceEvidence struct {
	MeanConfidence     float64 `json:"mean_confidence"`
	MinConfidence      float64 `json:"min_confidence"`
	ObservationCount   int     `json:"observation_count"`
	InterpolatedFrames int     `json:"interpolated_frames"`
	LowConfidence      bool    `json:"low_confidence"`
}

// TrackedTextRegion represents a single discrete on-screen text entity tracked across frames.
type TrackedTextRegion struct {
	ID                 string                  `json:"id"`
	Text               string                  `json:"text"`
	Role               TextRegionRole          `json:"role"`
	FirstFrameIndex    int                     `json:"first_frame_index"`
	LastFrameIndex     int                     `json:"last_frame_index"`
	FirstSeenMs        int64                   `json:"first_seen_ms"`
	LastSeenMs         int64                   `json:"last_seen_ms"`
	Keyframes          []RegionKeyframe        `json:"keyframes"`
	ConfidenceEvidence ConfidenceEvidence      `json:"confidence_evidence"`
	ProtectedMetadata  ProtectedRegionMetadata `json:"protected_metadata"`
	ReviewRequired     bool                    `json:"review_required"`
	ReviewReason       string                  `json:"review_reason,omitempty"`
}

// TextRegionPlan is the immutable source-derived artifact containing tracked and classified text regions.
// It is reusable across target languages (VI/EN) for the same source media asset.
type TextRegionPlan struct {
	ID             string              `json:"id"`
	SchemaVersion  int                 `json:"schema_version"`
	AssetID        string              `json:"asset_id"`
	ProviderID     string              `json:"provider_id"`
	ModelName      string              `json:"model_name"`
	ModelVersion   string              `json:"model_version"`
	FrameWidth     int                 `json:"frame_width"`
	FrameHeight    int                 `json:"frame_height"`
	Regions        []TrackedTextRegion `json:"regions"`
	CASHash        string              `json:"cas_hash,omitempty"`
	ProvenanceHash string              `json:"provenance_hash"`
	CreatedAt      time.Time           `json:"created_at"`
}

// ComputeTextRegionPlanProvenanceHash computes a deterministic SHA-256 identity over the source-derived inputs.
// Crucially, target language is NOT included: the plan is source-derived and language-reusable across VI/EN.
func ComputeTextRegionPlanProvenanceHash(assetID, providerID, modelName, modelVersion string, sampleStepMs int64) (string, error) {
	payload := struct {
		AssetID      string `json:"asset_id"`
		ProviderID   string `json:"provider_id"`
		ModelName    string `json:"model_name"`
		ModelVersion string `json:"model_version"`
		SampleStepMs int64  `json:"sample_step_ms"`
		SchemaVer    int    `json:"schema_version"`
	}{
		AssetID:      assetID,
		ProviderID:   providerID,
		ModelName:    modelName,
		ModelVersion: modelVersion,
		SampleStepMs: sampleStepMs,
		SchemaVer:    TextRegionPlanSchemaVersion,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// TextRegionClassifyConfig contains deterministic classification heuristics.
type TextRegionClassifyConfig struct {
	FrameWidth          int
	FrameHeight         int
	SubtitleBandTopFrac float64  // e.g. 0.65 (bottom 35% of frame)
	MinConfidence       float64  // e.g. 0.50 below which flagged low confidence
	BrandKeywords       []string // e.g. ["SUPOR", "Samyang", "NIKE", "ADIDAS"]
	UIKeywords          []string // e.g. ["导出", "剪辑", "图层", "关键帧", "透明度", "滤镜", "特效", "蒙版", "画中画", "音频", "文本", "比例", "背景"]
	NoisePatterns       []string // e.g. [".*@.*", "抖音号.*", ".*douyin.*"]
}

// DefaultTextRegionClassifyConfig returns sensible classification defaults for video frames.
func DefaultTextRegionClassifyConfig(w, h int) TextRegionClassifyConfig {
	if w <= 0 {
		w = 1080
	}
	if h <= 0 {
		h = 1920
	}
	return TextRegionClassifyConfig{
		FrameWidth:          w,
		FrameHeight:         h,
		SubtitleBandTopFrac: 0.65,
		MinConfidence:       0.55,
		BrandKeywords: []string{
			"SUPOR", "Samyang", "Apple", "Nike", "Adidas", "Sony", "Samsung", "Dyson", "Logitech",
		},
		UIKeywords: []string{
			"导出", "剪辑", "图层", "关键帧", "透明度", "滤镜", "特效", "蒙版", "画中画", "音频", "文本", "比例", "背景", "调节", "贴纸",
			"Xuất", "Lớp phủ", "Keyframe", "Độ mờ", "Export", "Overlay", "Opacity",
		},
		NoisePatterns: []string{
			`(?i)抖音号[:：]?\s*\w+`,
			`(?i)@[\w\.-]+`,
			`(?i)douyin\.com`,
			`(?i)watermark`,
			`(?i)快手号[:：]?\s*\w+`,
		},
	}
}

// ClassifyRegion determines the operational role of a tracked text region deterministically.
func ClassifyRegion(text string, avgBox BoundingBox, avgConfidence float64, cfg TextRegionClassifyConfig) (TextRegionRole, ProtectedRegionMetadata, bool, string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return TextRoleIgnoreNoise, ProtectedRegionMetadata{IsProtected: false}, false, ""
	}

	// 1. Noise / Watermark check (regular expression patterns or very low confidence)
	for _, pat := range cfg.NoisePatterns {
		if re, err := regexp.Compile(pat); err == nil && re.MatchString(trimmed) {
			return TextRoleIgnoreNoise, ProtectedRegionMetadata{IsProtected: false}, false, "matched_noise_watermark_pattern"
		}
	}
	if avgConfidence < 0.35 {
		return TextRoleIgnoreNoise, ProtectedRegionMetadata{IsProtected: false}, false, "confidence_below_noise_floor"
	}

	// 2. Brand keep check
	for _, brand := range cfg.BrandKeywords {
		if strings.EqualFold(trimmed, brand) || strings.Contains(strings.ToUpper(trimmed), strings.ToUpper(brand)) {
			return TextRoleBrandKeep, ProtectedRegionMetadata{IsProtected: true, Reason: "brand_authenticity"}, false, ""
		}
	}

	// 3. Instructional UI Text check
	for _, uiTerm := range cfg.UIKeywords {
		if strings.EqualFold(trimmed, uiTerm) || strings.Contains(trimmed, uiTerm) {
			return TextRoleInstructionalUIText, ProtectedRegionMetadata{
				IsProtected: true,
				Reason:      "instructional_ui_control",
			}, false, ""
		}
	}

	// 4. Speech subtitle check: positioned in bottom subtitle band (lower ~35% of frame)
	subtitleYThreshold := int(float64(cfg.FrameHeight) * cfg.SubtitleBandTopFrac)
	if avgBox.CenterY() >= subtitleYThreshold && avgBox.Width < int(float64(cfg.FrameWidth)*0.92) {
		reviewReq := avgConfidence < cfg.MinConfidence
		var reviewReason string
		if reviewReq {
			reviewReason = "low_ocr_confidence_subtitle"
		}
		return TextRoleSpeechSubtitle, ProtectedRegionMetadata{IsProtected: false}, reviewReq, reviewReason
	}

	// 5. Default: semantic text (titles, step badges, ingredients, floating callouts)
	reviewReq := avgConfidence < cfg.MinConfidence
	var reviewReason string
	if reviewReq {
		reviewReason = "low_ocr_confidence_semantic_text"
	}
	return TextRoleSemanticText, ProtectedRegionMetadata{IsProtected: false}, reviewReq, reviewReason
}
