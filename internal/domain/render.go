package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	// ErrRenderPlanNotFound is returned when a requested RenderPlan does not exist.
	ErrRenderPlanNotFound = errors.New("render plan not found")
	// ErrRenderPlanInvalid is returned when a RenderPlan fails validation.
	ErrRenderPlanInvalid = errors.New("invalid render plan specification")
	// ErrRenderSourceNotFound is returned when required source media or audio artifacts are missing.
	ErrRenderSourceNotFound = errors.New("render source artifact missing in storage")
	// ErrDubMixNotRenderable is returned when the referenced DubMixArtifact is not in PASS state.
	ErrDubMixNotRenderable = errors.New("referenced dub mix is refused or not renderable")
	// ErrRenderBackendUnavailable is returned when the native rendering engine cannot execute.
	ErrRenderBackendUnavailable = errors.New("native render backend unavailable")
	// ErrRenderArtifactNotFound is returned when a requested preview or final render artifact does not exist.
	ErrRenderArtifactNotFound = errors.New("render artifact not found")
	// ErrSubtitlePlanNotFound is returned when a referenced SubtitlePlanArtifact is missing.
	ErrSubtitlePlanNotFound = errors.New("subtitle plan artifact not found")
)

const (
	RenderPlanSchemaVersion    = 1
	SubtitlePlanSchemaVersion  = 1
	PreviewRenderSchemaVersion = 1
	FinalRenderSchemaVersion   = 1

	RenderKindPreview = "preview"
	RenderKindFinal   = "final"

	DefaultRenderVideoCodec = "libx264"
	DefaultRenderAudioCodec = "aac"
)

// SubtitleCue defines a single deterministic subtitle/overlay layout unit.
// Follows the V3 compact fit-content standard: exact coordinates and padding guidance.
type SubtitleCue struct {
	ID         string `json:"id,omitempty"`
	StartMs    int64  `json:"start_ms"`
	EndMs      int64  `json:"end_ms"`
	Text       string `json:"text"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	FontSizePx int    `json:"font_size_px"`
	PaddingX   int    `json:"padding_x,omitempty"`
	PaddingY   int    `json:"padding_y,omitempty"`
	BoxColor   string `json:"box_color,omitempty"`
	FontColor  string `json:"font_color,omitempty"`
	FontName   string `json:"font_name,omitempty"`
}

// SubtitlePlanRef captures the immutable CAS artifact reference to a frozen subtitle layout/style plan.
// It freezes exact artifact ID, schema version, and CAS hash.
type SubtitlePlanRef struct {
	ArtifactID     string `json:"artifact_id"`
	SchemaVersion  int    `json:"schema_version"`
	CASHash        string `json:"cas_hash"`
	ProvenanceHash string `json:"provenance_hash,omitempty"`
	Format         string `json:"format,omitempty"` // "ass" or "compact_fit_cues"
	CueCount       int    `json:"cue_count"`
	CueSpecHash    string `json:"cue_spec_hash,omitempty"`
}

// SubtitlePlanArtifact is the immutable CAS-backed artifact containing the accepted subtitle layout/style plan.
// Formatted as ASS-compliant structure or compact fit-content cue definitions.
type SubtitlePlanArtifact struct {
	ID             string        `json:"id"`
	SchemaVersion  int           `json:"schema_version"`
	AssetID        string        `json:"asset_id"`
	RunID          string        `json:"run_id,omitempty"`
	JobID          string        `json:"job_id,omitempty"`
	TargetLanguage string        `json:"target_language"`
	Format         string        `json:"format"` // "ass" or "compact_fit_cues"
	Cues           []SubtitleCue `json:"cues"`
	ASSContent     string        `json:"ass_content,omitempty"`
	CASHash        string        `json:"cas_hash,omitempty"`
	ProvenanceHash string        `json:"provenance_hash,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

// RenderTimeline encapsulates the immutable temporal and spatial coordinate system.
type RenderTimeline struct {
	DurationMs int64   `json:"duration_ms"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	FrameRate  float64 `json:"frame_rate"`
}

// RenderPlan is the complete, deterministic composition recipe freezing exact CAS artifact references.
// It freezes exact artifact IDs/versions for video, audio, and subtitle layout/style plans.
type RenderPlan struct {
	ID                string          `json:"id"`
	SchemaVersion     int             `json:"schema_version"`
	AssetID           string          `json:"asset_id"`
	RunID             string          `json:"run_id"`
	JobID             string          `json:"job_id,omitempty"`
	TargetLanguage    string          `json:"target_language"`
	SourceAssetSHA256 string          `json:"source_asset_sha256"`
	DubMixCASHash     string          `json:"dub_mix_cas_hash"`
	AudioCASHash      string          `json:"audio_cas_hash"`
	Timeline          RenderTimeline  `json:"timeline"`
	SubtitlePlan      SubtitlePlanRef `json:"subtitle_plan"`
	SubtitleCues      []SubtitleCue   `json:"subtitle_cues,omitempty"`
	CASHash           string          `json:"cas_hash,omitempty"`
	ProvenanceHash    string          `json:"provenance_hash,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

// EncodeProfile defines the encoding knobs for a render pass.
// Preview uses proxies/lower encode quality while consuming identical plan semantics.
type EncodeProfile struct {
	Kind              string `json:"kind"` // "preview" | "final"
	VideoCodec        string `json:"video_codec"`
	CRF               int    `json:"crf"`
	Preset            string `json:"preset"`
	AudioCodec        string `json:"audio_codec"`
	AudioBitRateK     int    `json:"audio_bit_rate_k"`
	ProxyScaleDivisor int    `json:"proxy_scale_divisor"` // 1 = full resolution, 2 = half resolution (proxy)
}

// DefaultFinalEncodeProfile returns standard high-quality delivery encoding profile.
func DefaultFinalEncodeProfile() EncodeProfile {
	return EncodeProfile{
		Kind:              RenderKindFinal,
		VideoCodec:        DefaultRenderVideoCodec,
		CRF:               20,
		Preset:            "medium",
		AudioCodec:        DefaultRenderAudioCodec,
		AudioBitRateK:     192,
		ProxyScaleDivisor: 1,
	}
}

// DefaultPreviewEncodeProfile returns fast lightweight proxy encoding profile.
func DefaultPreviewEncodeProfile() EncodeProfile {
	return EncodeProfile{
		Kind:              RenderKindPreview,
		VideoCodec:        DefaultRenderVideoCodec,
		CRF:               30,
		Preset:            "veryfast",
		AudioCodec:        DefaultRenderAudioCodec,
		AudioBitRateK:     96,
		ProxyScaleDivisor: 2,
	}
}

// ConsumedPlanRef captures the exact semantic inputs frozen in the RenderPlan
// so preview/final parity can be asserted directly on the emitted artifacts.
type ConsumedPlanRef struct {
	PlanProvenanceHash string          `json:"plan_provenance_hash"`
	PlanCASHash        string          `json:"plan_cas_hash"`
	SourceAssetSHA256  string          `json:"source_asset_sha256"`
	DubMixCASHash      string          `json:"dub_mix_cas_hash"`
	AudioCASHash       string          `json:"audio_cas_hash"`
	DurationMs         int64           `json:"duration_ms"`
	Width              int             `json:"width"`
	Height             int             `json:"height"`
	FrameRate          float64         `json:"frame_rate"`
	SubtitlePlan       SubtitlePlanRef `json:"subtitle_plan"`
	CueSpecHash        string          `json:"cue_spec_hash,omitempty"`
	CueCount           int             `json:"cue_count"`
}

// PreviewRenderArtifact is the distinct immutable artifact produced by a preview render.
type PreviewRenderArtifact struct {
	ID               string          `json:"id"`
	SchemaVersion    int             `json:"schema_version"`
	AssetID          string          `json:"asset_id"`
	RunID            string          `json:"run_id"`
	JobID            string          `json:"job_id,omitempty"`
	TargetLanguage   string          `json:"target_language"`
	ConsumedPlan     ConsumedPlanRef `json:"consumed_plan"`
	EncodeProfile    EncodeProfile   `json:"encode_profile"`
	OutputCASHash    string          `json:"output_cas_hash"`
	OutputCASPath    string          `json:"output_cas_path,omitempty"`
	OutputByteSize   int64           `json:"output_byte_size"`
	OutputDurationMs int64           `json:"output_duration_ms"`
	Renderer         string          `json:"renderer"` // e.g. "native-ffmpeg-libass"
	ProvenanceHash   string          `json:"provenance_hash,omitempty"`
	CASHash          string          `json:"cas_hash,omitempty"`
	OverallStatus    string          `json:"overall_status"` // "PASS"
	CreatedAt        time.Time       `json:"created_at"`
}

// FinalRenderArtifact is the distinct immutable artifact produced by a final render.
type FinalRenderArtifact struct {
	ID               string          `json:"id"`
	SchemaVersion    int             `json:"schema_version"`
	AssetID          string          `json:"asset_id"`
	RunID            string          `json:"run_id"`
	JobID            string          `json:"job_id,omitempty"`
	TargetLanguage   string          `json:"target_language"`
	ConsumedPlan     ConsumedPlanRef `json:"consumed_plan"`
	EncodeProfile    EncodeProfile   `json:"encode_profile"`
	OutputCASHash    string          `json:"output_cas_hash"`
	OutputCASPath    string          `json:"output_cas_path,omitempty"`
	OutputByteSize   int64           `json:"output_byte_size"`
	OutputDurationMs int64           `json:"output_duration_ms"`
	Renderer         string          `json:"renderer"` // e.g. "native-ffmpeg-libass"
	ProvenanceHash   string          `json:"provenance_hash,omitempty"`
	CASHash          string          `json:"cas_hash,omitempty"`
	OverallStatus    string          `json:"overall_status"` // "PASS"
	CreatedAt        time.Time       `json:"created_at"`
}

// ComputeCueSpecHash produces a deterministic hash over the ordered subtitle cues.
func ComputeCueSpecHash(cues []SubtitleCue) string {
	if len(cues) == 0 {
		return "empty"
	}
	sortedCues := make([]SubtitleCue, len(cues))
	copy(sortedCues, cues)
	sort.Slice(sortedCues, func(i, j int) bool {
		if sortedCues[i].StartMs != sortedCues[j].StartMs {
			return sortedCues[i].StartMs < sortedCues[j].StartMs
		}
		if sortedCues[i].EndMs != sortedCues[j].EndMs {
			return sortedCues[i].EndMs < sortedCues[j].EndMs
		}
		return sortedCues[i].Text < sortedCues[j].Text
	})

	b, _ := json.Marshal(sortedCues)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ComputeSubtitlePlanProvenanceHash computes deterministic cache identity for SubtitlePlanArtifact.
func ComputeSubtitlePlanProvenanceHash(assetID, targetLang string, cues []SubtitleCue, format string) (string, error) {
	cueHash := ComputeCueSpecHash(cues)
	payload := struct {
		AssetID        string `json:"asset_id"`
		TargetLanguage string `json:"target_language"`
		Format         string `json:"format"`
		CueSpecHash    string `json:"cue_spec_hash"`
		SchemaVersion  int    `json:"schema_version"`
	}{
		AssetID:        strings.TrimSpace(assetID),
		TargetLanguage: strings.ToLower(strings.TrimSpace(targetLang)),
		Format:         strings.TrimSpace(format),
		CueSpecHash:    cueHash,
		SchemaVersion:  SubtitlePlanSchemaVersion,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal subtitle plan provenance payload: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ComputeRenderPlanProvenanceHash calculates deterministic cache identity for RenderPlan.
// Excludes RunID and JobID per CAS identity invariant.
func ComputeRenderPlanProvenanceHash(
	assetID, targetLang, sourceAssetSHA, dubMixCAS, audioCAS string,
	timeline RenderTimeline,
	subPlan SubtitlePlanRef,
) (string, error) {
	payload := struct {
		AssetID        string          `json:"asset_id"`
		TargetLanguage string          `json:"target_language"`
		SourceSHA      string          `json:"source_asset_sha"`
		DubMixCAS      string          `json:"dub_mix_cas"`
		AudioCAS       string          `json:"audio_cas"`
		Timeline       RenderTimeline  `json:"timeline"`
		SubtitlePlan   SubtitlePlanRef `json:"subtitle_plan"`
		SchemaVersion  int             `json:"schema_version"`
	}{
		AssetID:        strings.TrimSpace(assetID),
		TargetLanguage: strings.ToLower(strings.TrimSpace(targetLang)),
		SourceSHA:      strings.TrimSpace(sourceAssetSHA),
		DubMixCAS:      strings.TrimSpace(dubMixCAS),
		AudioCAS:       strings.TrimSpace(audioCAS),
		Timeline:       timeline,
		SubtitlePlan:   subPlan,
		SchemaVersion:  RenderPlanSchemaVersion,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal render plan provenance payload: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ComputeRenderArtifactProvenanceHash calculates deterministic cache identity for a render artifact.
func ComputeRenderArtifactProvenanceHash(
	planProvenanceHash, kind string,
	profile EncodeProfile,
) (string, error) {
	payload := struct {
		PlanProvenance string        `json:"plan_provenance"`
		Kind           string        `json:"kind"`
		Profile        EncodeProfile `json:"profile"`
		SchemaVersion  int           `json:"schema_version"`
	}{
		PlanProvenance: strings.TrimSpace(planProvenanceHash),
		Kind:           strings.ToLower(strings.TrimSpace(kind)),
		Profile:        profile,
		SchemaVersion: func() int {
			if kind == RenderKindPreview {
				return PreviewRenderSchemaVersion
			}
			return FinalRenderSchemaVersion
		}(),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal render artifact provenance payload: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
