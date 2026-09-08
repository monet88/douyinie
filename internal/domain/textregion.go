package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrTextRegionPlanNotFound is returned when a TextRegionPlan artifact is missing.
	ErrTextRegionPlanNotFound = errors.New("text region plan not found")
	// ErrOCRFailed is returned when the OCR provider fails to detect or track text regions.
	ErrOCRFailed = errors.New("ocr detection or tracking failed")
)

const TextRegionPlanSchemaVersion = 1
const LocalizedVisualTrackSchemaVersion = 1
const LocalizedSubtitleTrackSchemaVersion = 1

var (
	// ErrSubtitleOverlapsProtectedRegion is returned when a subtitle or overlay occludes a protected area.
	ErrSubtitleOverlapsProtectedRegion = errors.New("subtitle or overlay overlaps protected region")
	// ErrRegionOverrideInvalid is returned when a region override specification is malformed.
	ErrRegionOverrideInvalid = errors.New("invalid text region override")
	// ErrTranslationFailed is returned when visual text translation fails or service is unavailable.
	ErrTranslationFailed = errors.New("translation failed or translation service not available")
)

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

// SceneProtectedRegion represents upstream/operator evidence for visual regions that must not be occluded
// (e.g. detected faces, UI tap targets, timeline/slider controls, brand marks).
type SceneProtectedRegion struct {
	ID      string      `json:"id,omitempty"`
	Reason  string      `json:"reason"` // e.g. "face", "tap_target", "timeline_control", "brand_keep"
	StartMs int64       `json:"start_ms,omitempty"`
	EndMs   int64       `json:"end_ms,omitempty"`
	Box     BoundingBox `json:"box"`
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

// RegionOverride defines manual or AI direct-manipulation adjustments for a tracked text region.
// Allows changing role classification, shifting/scaling bounding box, or providing manual translation text.
type RegionOverride struct {
	RegionID    string          `json:"region_id"`
	NewRole     *TextRegionRole `json:"new_role,omitempty"`
	NewText     *string         `json:"new_text,omitempty"`
	BoxDeltaX   int             `json:"box_delta_x,omitempty"`
	BoxDeltaY   int             `json:"box_delta_y,omitempty"`
	BoxDeltaW   int             `json:"box_delta_w,omitempty"`
	BoxDeltaH   int             `json:"box_delta_h,omitempty"`
	IsProtected *bool           `json:"is_protected,omitempty"`
	Notes       string          `json:"notes,omitempty"`
}

// LocalizedOverlayItem represents a single in-place localized cover/overlay unit.
type LocalizedOverlayItem struct {
	RegionID       string         `json:"region_id"`
	Role           TextRegionRole `json:"role"`
	SourceText     string         `json:"source_text"`
	LocalizedText  string         `json:"localized_text"`
	StartMs        int64          `json:"start_ms"`
	EndMs          int64          `json:"end_ms"`
	Box            BoundingBox    `json:"box"`
	IsCoverDefault bool           `json:"is_cover_default"` // true = deterministic in-place cover; false = inpainting fallback
	Inpainting     bool           `json:"inpainting"`       // true if fallback inpainting requested (non-default)
	BoxColor       string         `json:"box_color,omitempty"`
	FontColor      string         `json:"font_color,omitempty"`
	FontSizePx     int            `json:"font_size_px,omitempty"`
	PaddingX       int            `json:"padding_x,omitempty"`
	PaddingY       int            `json:"padding_y,omitempty"`
}

// LocalizedSubtitleTrack represents a persisted localized subtitle track conforming to the V3 compact fit-content standard.
type LocalizedSubtitleTrack struct {
	ID             string        `json:"id"`
	SchemaVersion  int           `json:"schema_version"`
	AssetID        string        `json:"asset_id"`
	RunID          string        `json:"run_id,omitempty"`
	JobID          string        `json:"job_id,omitempty"`
	TargetLanguage string        `json:"target_language"`
	Format         string        `json:"format"` // "compact_fit_cues" or "ass"
	Cues           []SubtitleCue `json:"cues"`
	CASHash        string        `json:"cas_hash,omitempty"`
	ProvenanceHash string        `json:"provenance_hash,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

// LocalizedVisualTrack is the complete localized visual layer artifact (compact subtitles + in-place overlays).
type LocalizedVisualTrack struct {
	ID                 string                 `json:"id"`
	SchemaVersion      int                    `json:"schema_version"`
	AssetID            string                 `json:"asset_id"`
	RunID              string                 `json:"run_id,omitempty"`
	JobID              string                 `json:"job_id,omitempty"`
	TargetLanguage     string                 `json:"target_language"`
	TextRegionPlanCAS  string                 `json:"text_region_plan_cas"`
	TextRegionPlanProv string                 `json:"text_region_plan_provenance"`
	SubtitleTrackCAS   string                 `json:"subtitle_track_cas,omitempty"`
	Overlays           []LocalizedOverlayItem `json:"overlays"`
	SubtitleCues       []SubtitleCue          `json:"subtitle_cues"`
	ProtectedRegions   []BoundingBox          `json:"protected_regions,omitempty"`
	CASHash            string                 `json:"cas_hash,omitempty"`
	ProvenanceHash     string                 `json:"provenance_hash,omitempty"`
	CreatedAt          time.Time              `json:"created_at"`
}

type termEntry struct {
	source string
	target string
}

// Ordered glossary definitions for deterministic matching.
// Ordered from most specific (longer/compound terms) to least specific.
var (
	standardDictVI = []termEntry{
		{source: "画中画", target: "Lớp phủ"},
		{source: "关键帧", target: "Keyframe"},
		{source: "透明度", target: "Độ mờ"},
		{source: "短视频", target: "Video ngắn"},
		{source: "草稿箱", target: "Bản nháp"},
		{source: "导出", target: "Xuất"},
		{source: "剪辑", target: "Chỉnh sửa"},
		{source: "图层", target: "Lớp"},
		{source: "滤镜", target: "Bộ lọc"},
		{source: "特效", target: "Hiệu ứng"},
		{source: "蒙版", target: "Mặt nạ"},
		{source: "音频", target: "Âm thanh"},
		{source: "文本", target: "Văn bản"},
		{source: "比例", target: "Tỷ lệ"},
		{source: "背景", target: "Phông nền"},
		{source: "调节", target: "Tùy chỉnh"},
		{source: "贴纸", target: "Nhãn dán"},
		{source: "变速", target: "Tốc độ"},
		{source: "动画", target: "Hoạt ảnh"},
		{source: "分割", target: "Tách"},
		{source: "删除", target: "Xóa"},
		{source: "复制", target: "Sao chép"},
		{source: "替换", target: "Thay thế"},
	}

	standardDictEN = []termEntry{
		{source: "画中画", target: "Overlay"},
		{source: "关键帧", target: "Keyframe"},
		{source: "透明度", target: "Opacity"},
		{source: "短视频", target: "Short Video"},
		{source: "草稿箱", target: "Drafts"},
		{source: "导出", target: "Export"},
		{source: "剪辑", target: "Edit"},
		{source: "图层", target: "Layer"},
		{source: "滤镜", target: "Filter"},
		{source: "特效", target: "Effects"},
		{source: "蒙版", target: "Mask"},
		{source: "音频", target: "Audio"},
		{source: "文本", target: "Text"},
		{source: "比例", target: "Ratio"},
		{source: "背景", target: "Background"},
		{source: "调节", target: "Adjust"},
		{source: "贴纸", target: "Stickers"},
		{source: "变速", target: "Speed"},
		{source: "动画", target: "Animation"},
		{source: "分割", target: "Split"},
		{source: "删除", target: "Delete"},
		{source: "复制", target: "Copy"},
		{source: "替换", target: "Replace"},
	}
)

// StandardInstructionalUITerm returns the standard target-language software UI term for CapCut/JianYing concepts.
// Matches deterministically using an ordered lookup table: exact match on source or target.
func StandardInstructionalUITerm(sourceText, targetLang string) (string, bool) {
	normalized := strings.TrimSpace(sourceText)
	if normalized == "" {
		return "", false
	}
	targetLang = strings.ToLower(strings.TrimSpace(targetLang))

	var dict []termEntry
	switch targetLang {
	case "vi":
		dict = standardDictVI
	case "en":
		dict = standardDictEN
	default:
		return sourceText, false
	}

	// 1. Exact match on source
	for _, entry := range dict {
		if strings.EqualFold(normalized, entry.source) {
			return entry.target, true
		}
	}

	// 2. Exact match on target
	for _, entry := range dict {
		if strings.EqualFold(normalized, entry.target) {
			return entry.target, true
		}
	}

	return sourceText, false
}

// NormalizeInstructionalUIText normalizes translated instructional UI text using canonical target-language glossary terms.
// Performs exact matching first, and preserves surrounding text remainder for compound phrases without discarding words.
func NormalizeInstructionalUIText(translatedText, sourceText, targetLang string) string {
	trimmedTrans := strings.TrimSpace(translatedText)
	trimmedSrc := strings.TrimSpace(sourceText)

	// 1. Exact match on source or translation
	if stdTerm, found := StandardInstructionalUITerm(trimmedSrc, targetLang); found {
		return stdTerm
	}
	if stdTerm, found := StandardInstructionalUITerm(trimmedTrans, targetLang); found {
		return stdTerm
	}

	// 2. Canonical dictionary term replacement preserving remainder
	var dict []termEntry
	switch strings.ToLower(strings.TrimSpace(targetLang)) {
	case "vi":
		dict = standardDictVI
	case "en":
		dict = standardDictEN
	default:
		return trimmedTrans
	}

	res := trimmedTrans
	for _, entry := range dict {
		if strings.Contains(res, entry.source) {
			res = strings.ReplaceAll(res, entry.source, entry.target)
		}
	}
	return res
}

// TimeWindowsOverlap returns true if intervals [s1, e1] and [s2, e2] overlap in time.
// If either interval has start == 0 && end == 0, it is treated as spanning all time.
func TimeWindowsOverlap(s1, e1, s2, e2 int64) bool {
	if (s1 == 0 && e1 == 0) || (s2 == 0 && e2 == 0) {
		return true
	}
	if e1 <= 0 {
		e1 = math.MaxInt64
	}
	if e2 <= 0 {
		e2 = math.MaxInt64
	}
	return s1 <= e2 && s2 <= e1
}

// GetProtectedBoxesForTimeWindow collects all relevant protected bounding boxes
// (from tracked text regions' keyframes and scene-protected regions) that are active during [startMs, endMs].
// If excludeRegionID is non-empty, keyframes from that specific region are ignored (e.g. self-occlusion for overlays).
func GetProtectedBoxesForTimeWindow(
	regions []TrackedTextRegion,
	sceneProtected []SceneProtectedRegion,
	startMs, endMs int64,
	excludeRegionID string,
) []BoundingBox {
	var result []BoundingBox

	// 1. From TrackedTextRegion keyframes
	for _, reg := range regions {
		if !reg.ProtectedMetadata.IsProtected {
			continue
		}
		if excludeRegionID != "" && reg.ID == excludeRegionID {
			continue
		}
		if !TimeWindowsOverlap(reg.FirstSeenMs, reg.LastSeenMs, startMs, endMs) {
			continue
		}
		for _, kf := range reg.Keyframes {
			if (startMs == 0 && endMs == 0 && reg.FirstSeenMs == 0 && reg.LastSeenMs == 0) || (kf.TimestampMs >= startMs && kf.TimestampMs <= endMs) || (len(reg.Keyframes) == 1 && reg.FirstSeenMs == 0 && reg.LastSeenMs == 0) {
				result = append(result, kf.Box)
			}
		}
	}
	// 2. From SceneProtectedRegions (faces, tap targets, timeline controls, etc.)
	for _, sp := range sceneProtected {
		if TimeWindowsOverlap(sp.StartMs, sp.EndMs, startMs, endMs) {
			result = append(result, sp.Box)
		}
	}

	return result
}

// BoxesOverlap returns true if two bounding boxes overlap in 2D space.
func BoxesOverlap(a, b BoundingBox) bool {
	return a.X < b.X+b.Width &&
		a.X+a.Width > b.X &&
		a.Y < b.Y+b.Height &&
		a.Y+a.Height > b.Y
}

// SubtitlePlacementSelector defines a pluggable selector hook (e.g. AI or heuristic)
// to choose optimal subtitle box placement. Return (chosenBox, ok). If !ok or out-of-bounds/colliding,
// deterministic safe fallback is used.
type SubtitlePlacementSelector func(frameWidth, frameHeight int, candBox BoundingBox, protectedAreas []BoundingBox) (BoundingBox, bool)

// ComputeCompactSubtitleBoundsWithSelector calculates scale-aware fit-content subtitle box coordinates and padding,
// invoking an optional pluggable placement selector with deterministic safe fallback.
// If no safe in-frame placement exists without occluding protected regions, it fails closed and returns ErrSubtitleOverlapsProtectedRegion.
func ComputeCompactSubtitleBoundsWithSelector(
	frameWidth, frameHeight int,
	text string,
	fontSizePx int,
	paddingX, paddingY int,
	protectedAreas []BoundingBox,
	selector SubtitlePlacementSelector,
) (SubtitleCue, error) {
	if frameWidth <= 0 {
		frameWidth = 1080
	}
	if frameHeight <= 0 {
		frameHeight = 1920
	}
	if fontSizePx <= 0 {
		fontSizePx = int(float64(frameHeight) * 0.018) // ~34px for 1920h
		if fontSizePx < 20 {
			fontSizePx = 20
		}
	}
	// Scale-aware reference padding guidance: ~18-28px horizontal, ~10-16px vertical
	if paddingX <= 0 {
		paddingX = int(float64(frameWidth) * 0.02) // ~22px for 1080w
		if paddingX < 18 {
			paddingX = 18
		} else if paddingX > 28 {
			paddingX = 28
		}
	}
	if paddingY <= 0 {
		paddingY = int(float64(frameHeight) * 0.007) // ~14px for 1920h
		if paddingY < 10 {
			paddingY = 10
		} else if paddingY > 16 {
			paddingY = 16
		}
	}

	// Approximate text width: rough proportional font estimation (average char width ~ 0.55 * fontSize)
	charWidth := int(float64(fontSizePx) * 0.55)
	textLen := len([]rune(text))
	rawTextWidth := textLen * charWidth

	// Check if multi-line needed (max width ~ 80% frame width)
	maxAllowedWidth := int(float64(frameWidth) * 0.80)
	boxWidth := rawTextWidth + 2*paddingX
	boxHeight := fontSizePx + 2*paddingY

	if boxWidth > maxAllowedWidth {
		boxWidth = maxAllowedWidth
		boxHeight = fontSizePx*2 + int(float64(fontSizePx)*0.3) + 2*paddingY
	}

	// Center horizontally by default, clamp inside horizontal frame margins
	defaultX := (frameWidth - boxWidth) / 2
	if defaultX < 10 {
		defaultX = 10
	}
	if defaultX+boxWidth > frameWidth-10 {
		boxWidth = frameWidth - 20
		defaultX = 10
	}

	// Default subtitle placement: lower band (~75% from top)
	defaultY := int(float64(frameHeight) * 0.75)
	minY := int(float64(frameHeight) * 0.10)
	maxY := frameHeight - boxHeight - int(float64(frameHeight)*0.03)
	if defaultY > maxY {
		defaultY = maxY
	}
	if defaultY < minY {
		defaultY = minY
	}

	candBox := BoundingBox{X: defaultX, Y: defaultY, Width: boxWidth, Height: boxHeight}

	// Helper to check if a box overlaps any protected region
	overlapsAny := func(b BoundingBox) bool {
		for _, prot := range protectedAreas {
			if BoxesOverlap(b, prot) {
				return true
			}
		}
		return false
	}

	// Helper to check if a box is inside frame bounds
	inFrame := func(b BoundingBox) bool {
		return b.X >= 0 && b.X+b.Width <= frameWidth && b.Y >= minY && b.Y <= maxY
	}

	var chosenBox BoundingBox
	foundSafe := false

	// Try pluggable selector if provided
	if selector != nil {
		if selBox, ok := selector(frameWidth, frameHeight, candBox, protectedAreas); ok {
			if inFrame(selBox) && !overlapsAny(selBox) {
				chosenBox = selBox
				foundSafe = true
			}
		}
	}

	// Candidate position without shift
	if !foundSafe && inFrame(candBox) && !overlapsAny(candBox) {
		chosenBox = candBox
		foundSafe = true
	}

	// Deterministic safe search: try shifting upward, then downward, in 16px increments
	if !foundSafe {
		// 1. Try upward shifts
		for testY := defaultY - 16; testY >= minY; testY -= 16 {
			testBox := BoundingBox{X: defaultX, Y: testY, Width: boxWidth, Height: boxHeight}
			if !overlapsAny(testBox) {
				chosenBox = testBox
				foundSafe = true
				break
			}
		}
		// 2. If upward shift did not find a clear spot, try downward shifts
		if !foundSafe {
			for testY := defaultY + 16; testY <= maxY; testY += 16 {
				testBox := BoundingBox{X: defaultX, Y: testY, Width: boxWidth, Height: boxHeight}
				if !overlapsAny(testBox) {
					chosenBox = testBox
					foundSafe = true
					break
				}
			}
		}
		// 3. Full frame sweep top to bottom
		if !foundSafe {
			for testY := minY; testY <= maxY; testY += 16 {
				testBox := BoundingBox{X: defaultX, Y: testY, Width: boxWidth, Height: boxHeight}
				if !overlapsAny(testBox) {
					chosenBox = testBox
					foundSafe = true
					break
				}
			}
		}
	}

	if !foundSafe {
		return SubtitleCue{}, fmt.Errorf("%w: cannot place subtitle cue %q (dims %dx%d) without occluding protected regions",
			ErrSubtitleOverlapsProtectedRegion, text, boxWidth, boxHeight)
	}

	return SubtitleCue{
		Text:       text,
		X:          chosenBox.X,
		Y:          chosenBox.Y,
		Width:      chosenBox.Width,
		Height:     chosenBox.Height,
		FontSizePx: fontSizePx,
		PaddingX:   paddingX,
		PaddingY:   paddingY,
		BoxColor:   "black@0.6",
		FontColor:  "#FFFFFF",
	}, nil
}

// ComputeCompactSubtitleBounds calculates scale-aware fit-content subtitle box coordinates and padding.
// Follows the V3 compact fit-content standard: hugs rendered text, 1-2 lines, avoids protected UI areas,
// and guarantees candidate cues remain within visible frame bounds.
func ComputeCompactSubtitleBounds(
	frameWidth, frameHeight int,
	text string,
	fontSizePx int,
	paddingX, paddingY int,
	protectedAreas []BoundingBox,
) (SubtitleCue, error) {
	return ComputeCompactSubtitleBoundsWithSelector(frameWidth, frameHeight, text, fontSizePx, paddingX, paddingY, protectedAreas, nil)
}

// ComputeLocalizedVisualTrackProvenanceHash computes deterministic cache identity for LocalizedVisualTrack.
func ComputeLocalizedVisualTrackProvenanceHash(
	assetID, targetLang, textRegionPlanProv, dubScriptProv string,
	overlays []LocalizedOverlayItem,
	cues []SubtitleCue,
	sceneProtected ...[]SceneProtectedRegion,
) (string, error) {
	cueHash := ComputeCueSpecHash(cues)
	var sp []SceneProtectedRegion
	if len(sceneProtected) > 0 {
		sp = sceneProtected[0]
	}
	payload := struct {
		AssetID               string                 `json:"asset_id"`
		TargetLanguage        string                 `json:"target_language"`
		TextRegionPlanProv    string                 `json:"text_region_plan_prov"`
		DubScriptProv         string                 `json:"dub_script_prov"`
		Overlays              []LocalizedOverlayItem `json:"overlays"`
		CueSpecHash           string                 `json:"cue_spec_hash"`
		SceneProtectedRegions []SceneProtectedRegion `json:"scene_protected_regions,omitempty"`
		SchemaVersion         int                    `json:"schema_version"`
	}{
		AssetID:               strings.TrimSpace(assetID),
		TargetLanguage:        strings.ToLower(strings.TrimSpace(targetLang)),
		TextRegionPlanProv:    strings.TrimSpace(textRegionPlanProv),
		DubScriptProv:         strings.TrimSpace(dubScriptProv),
		Overlays:              overlays,
		CueSpecHash:           cueHash,
		SceneProtectedRegions: sp,
		SchemaVersion:         LocalizedVisualTrackSchemaVersion,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal visual track provenance payload: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// TextRegionPlan is the immutable source-derived artifact containing tracked and classified text regions.
// It is reusable across target languages (VI/EN) for the same source media asset.
type TextRegionPlan struct {
	ID                string              `json:"id"`
	SchemaVersion     int                 `json:"schema_version"`
	AssetID           string              `json:"asset_id"`
	ProviderID        string              `json:"provider_id"`
	ModelName         string              `json:"model_name"`
	ModelVersion      string              `json:"model_version"`
	FrameWidth        int                 `json:"frame_width"`
	FrameHeight       int                 `json:"frame_height"`
	Regions           []TrackedTextRegion `json:"regions"`
	DetSnapshotDigest string              `json:"det_snapshot_digest,omitempty"`
	RecSnapshotDigest string              `json:"rec_snapshot_digest,omitempty"`
	OriSnapshotDigest string              `json:"ori_snapshot_digest,omitempty"`
	RuntimeIdentity   string              `json:"runtime_identity,omitempty"`
	CASHash           string              `json:"cas_hash,omitempty"`
	ProvenanceHash    string              `json:"provenance_hash"`
	CreatedAt         time.Time           `json:"created_at"`
}

// ComputeTextRegionPlanProvenanceHash computes a deterministic SHA-256 identity over the source-derived inputs.
// Crucially, target language is NOT included: the plan is source-derived and language-reusable across VI/EN.
func ComputeTextRegionPlanProvenanceHash(assetID, providerID, modelName, modelVersion string, sampleStepMs int64, extraIdentities ...string) (string, error) {
	payload := struct {
		AssetID         string   `json:"asset_id"`
		ProviderID      string   `json:"provider_id"`
		ModelName       string   `json:"model_name"`
		ModelVersion    string   `json:"model_version"`
		SampleStepMs    int64    `json:"sample_step_ms"`
		SchemaVer       int      `json:"schema_version"`
		ExtraIdentities []string `json:"extra_identities,omitempty"`
	}{
		AssetID:         assetID,
		ProviderID:      providerID,
		ModelName:       modelName,
		ModelVersion:    modelVersion,
		SampleStepMs:    sampleStepMs,
		SchemaVer:       TextRegionPlanSchemaVersion,
		ExtraIdentities: extraIdentities,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ComputeTextRegionPlanOverrideProvenanceHash computes a deterministic SHA-256 identity for an
// operator-overridden TextRegionPlan. It is derived from stable semantic inputs ONLY: the parent
// (source-derived) provenance hash plus the canonical overridden region state (sorted by region ID).
// It deliberately excludes timestamps, paths, run IDs, and job IDs so that re-applying the same
// override is idempotent and the source-derived plan row is never overwritten by a conflicting
// provenance in SaveTextRegionPlanIndex (UNIQUE on provenance_hash).
func ComputeTextRegionPlanOverrideProvenanceHash(parentProvenance string, regions []TrackedTextRegion) (string, error) {
	canon := make([]TrackedTextRegion, len(regions))
	copy(canon, regions)
	sort.Slice(canon, func(i, j int) bool { return canon[i].ID < canon[j].ID })
	payload := struct {
		ParentProvenance string              `json:"parent_provenance"`
		Regions          []TrackedTextRegion `json:"regions"`
		Kind             string              `json:"kind"`
		SchemaVer        int                 `json:"schema_version"`
	}{
		ParentProvenance: strings.TrimSpace(parentProvenance),
		Regions:          canon,
		Kind:             "text_region_plan_override",
		SchemaVer:        TextRegionPlanSchemaVersion,
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

// NearbyObservation represents a spatio-temporally adjacent OCR detection for tracking stability evaluation.
type NearbyObservation struct {
	Text        string      `json:"text"`
	TimestampMs int64       `json:"timestamp_ms"`
	Box         BoundingBox `json:"box"`
}

// ClassifyRegion determines the operational role of a tracked text region deterministically.
func ClassifyRegion(text string, avgBox BoundingBox, avgConfidence float64, cfg TextRegionClassifyConfig) (TextRegionRole, ProtectedRegionMetadata, bool, string) {
	return ClassifyRegionWithInstability(text, avgBox, avgConfidence, cfg, nil)
}

// ClassifyRegionWithInstability determines the operational role of a tracked text region deterministically,
// incorporating spatio-temporal OCR stability across nearby observations.
func ClassifyRegionWithInstability(
	text string,
	avgBox BoundingBox,
	avgConfidence float64,
	cfg TextRegionClassifyConfig,
	nearby []NearbyObservation,
) (TextRegionRole, ProtectedRegionMetadata, bool, string) {
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

	// 5. Spatio-temporal OCR instability check:
	// If an unknown text token is observed with low-to-moderate confidence (e.g. < 0.65),
	// and there is a sequence of 3+ total observations across adjacent sampled frames whose
	// bounding boxes are spatially nearby/overlapping, but whose text changes materially
	// (Latin pseudo-text / background scribble hallucination), classify as IgnoreNoise.
	if len(nearby) >= 2 && avgConfidence < 0.65 {
		var texts []string
		texts = append(texts, trimmed)
		allLatin := isAllLatinOrPunct(trimmed)
		for _, nb := range nearby {
			nbTrimmed := strings.TrimSpace(nb.Text)
			if nbTrimmed != "" {
				texts = append(texts, nbTrimmed)
				if !isAllLatinOrPunct(nbTrimmed) {
					allLatin = false
				}
			}
		}
		// Total observations >= 3
		if len(texts) >= 3 && allLatin {
			distinctCount := countDistinctNormalizedTexts(texts)
			if distinctCount >= 3 {
				return TextRoleIgnoreNoise, ProtectedRegionMetadata{IsProtected: false}, false, "spatio_temporal_ocr_instability_noise"
			}
		}
	}

	// 6. Default: semantic text (titles, step badges, ingredients, floating callouts)
	reviewReq := avgConfidence < cfg.MinConfidence
	var reviewReason string
	if reviewReq {
		reviewReason = "low_ocr_confidence_semantic_text"
	}
	return TextRoleSemanticText, ProtectedRegionMetadata{IsProtected: false}, reviewReq, reviewReason
}

func isAllLatinOrPunct(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || unicode.IsPunct(r) || unicode.IsSpace(r) {
			continue
		}
		return false
	}
	return true
}

func countDistinctNormalizedTexts(texts []string) int {
	seen := make(map[string]bool)
	for _, t := range texts {
		norm := strings.ToLower(strings.TrimSpace(t))
		if norm != "" {
			seen[norm] = true
		}
	}
	return len(seen)
}
