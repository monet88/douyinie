package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrNoDubEligibleSpeech is returned when the audio role plan has no narration/dialogue segments,
	// so speech understanding is skipped.
	ErrNoDubEligibleSpeech = errors.New("no dub-eligible speech in audio role plan")
	// ErrASRNoCandidates is returned when no ASR provider produced output.
	ErrASRNoCandidates = errors.New("no ASR results from any provider candidate")
	// ErrAlignmentFailed is returned when forced alignment produced no word timings.
	ErrAlignmentFailed = errors.New("forced alignment produced no word-level timings")
	// ErrAudioRolePlanRequired is returned when no audio role plan exists for the asset.
	ErrAudioRolePlanRequired = errors.New("audio role plan required before speech understanding")
)

// ASRRawSegment represents a raw ASR recognition result from one VAD turn.
// Recognizer boundaries are never canonical SpeechBlock boundaries on their own
// (AC: recognizer boundaries never canonical alone).
type ASRRawSegment struct {
	StartMs      int64   `json:"start_ms"`
	EndMs        int64   `json:"end_ms"`
	Text         string  `json:"text"`
	Confidence   float64 `json:"confidence"`
	LanguageCode string  `json:"language_code,omitempty"`
}

// WordTiming represents a single word-level alignment result from the forced aligner.
type WordTiming struct {
	Word       string  `json:"word"`
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Confidence float64 `json:"confidence"`
	SpeakerID  string  `json:"speaker_id,omitempty"` // assigned by conditional diarization before segmentation
}

// SpeakerAssignment maps a speaker to a time region.
// Produced by conditional diarization runs only when speaker evidence warrants it.
type SpeakerAssignment struct {
	SpeakerID  string  `json:"speaker_id"`
	Label      string  `json:"label,omitempty"` // e.g. "SPEAKER_00", "SPEAKER_01"
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Confidence float64 `json:"confidence"`
}

// DiarizationPlan is the output of conditional diarization (runs only when speaker evidence warrants).
type DiarizationPlan struct {
	ID          string              `json:"id"`
	RunID       string              `json:"run_id"`
	ProviderID  string              `json:"provider_id,omitempty"` // diarizer/provider identity (empty for injected test plans)
	Assignments []SpeakerAssignment `json:"assignments"`
	Confidence  float64             `json:"confidence"`
	CreatedAt   time.Time           `json:"created_at"`
}

// SpeechBlock is the canonical atomic speech unit derived after forced alignment
// and diarization. It defines an immutable source timing window.
// ASR boundaries alone are never canonical — only segmentation via semantic/punctuation/pause rules
// produces canonical SpeechBlocks.
type SpeechBlock struct {
	Index             int          `json:"index"`
	StartMs           int64        `json:"start_ms"`
	EndMs             int64        `json:"end_ms"`
	SpeakerID         string       `json:"speaker_id"`
	SourceText        string       `json:"source_text"`
	TokenTimings      []WordTiming `json:"token_timings"`
	SpeakerConfidence float64      `json:"speaker_confidence"`
	SegmentType       string       `json:"segment_type"` // "speech" | "silence"
}

// SpeechBlockSegmentType constants.
const (
	SpeechBlockTypeSpeech  = "speech"
	SpeechBlockTypeSilence = "silence"
)

// TranscriptArtifact is the persisted output of the speech understanding pipeline:
// ASR → accepted transcript → forced alignment → conditional diarization → canonical SpeechBlock segmentation.
type TranscriptArtifact struct {
	ID                    string              `json:"id"`
	AssetID               string              `json:"asset_id"`
	RunID                 string              `json:"run_id"`
	RawSegments           []ASRRawSegment     `json:"raw_segments"`
	WordTimings           []WordTiming        `json:"word_timings"`
	SpeakerAssignments    []SpeakerAssignment `json:"speaker_assignments,omitempty"`
	SpeechBlocks          []SpeechBlock       `json:"speech_blocks"`
	ASRProviderID         string              `json:"asr_provider_id"`
	AlignerProviderID     string              `json:"aligner_provider_id"`
	DiarizationRan        bool                `json:"diarization_ran"`
	DiarizationProviderID string              `json:"diarization_provider_id,omitempty"` // diarizer identity when evidence-gated diarization ran
	SourceLanguage        string              `json:"source_language"`
	CASHash               string              `json:"cas_hash,omitempty"`        // content-addressed store object hash
	ProvenanceHash        string              `json:"provenance_hash,omitempty"` // deterministic identity over dependency/config/provider/model/schema inputs
	CreatedAt             time.Time           `json:"created_at"`
}

// TranscriptProvenance captures the deterministic inputs that determine a
// transcript artifact's cache identity: source asset, providers, models,
// segment rules, and the pipeline schema version. Changing any of these
// yields a NEW artifact identity instead of a permanent write-once failure.
type TranscriptProvenance struct {
	AssetSHA256           string            `json:"asset_sha256"`
	ASRProviderID         string            `json:"asr_provider_id"`
	ASRModelName          string            `json:"asr_model_name"`
	ASRModelVersion       string            `json:"asr_model_version"`
	AlignerProviderID     string            `json:"aligner_provider_id"`
	AlignerModelName      string            `json:"aligner_model_name"`
	AlignerModelVersion   string            `json:"aligner_model_version"`
	SegmentConfig         SegmentRuleConfig `json:"segment_config"`
	DiarizationProviderID string            `json:"diarization_provider_id,omitempty"`
	DiarizationRan        bool              `json:"diarization_ran"`
	SchemaVersion         int               `json:"schema_version"`
}

// Hash returns a deterministic SHA-256 identity over the canonical JSON of
// the provenance inputs. Identical pipeline inputs always produce the same
// hash; changing any input produces a different hash.
func (p TranscriptProvenance) Hash() string {
	b, err := json.Marshal(p)
	if err != nil {
		// Canonical struct with only JSON-marshalable fields cannot fail.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TranscriptSchemaVersion is the canonical transcript pipeline schema version.
// It participates in the provenance hash so schema changes re-derive artifacts.
const TranscriptSchemaVersion = 1

// SpeechPipelineInput captures the full input for the speech understanding pipeline run.
// It bundles all needed references so the pipeline can be invoked atomically.
type SpeechPipelineInput struct {
	RunID         string
	AssetID       string
	JobID         string
	AudioPath     string         // source-media CAS path, not a normalized audio artifact; resolved on RuntimeHost; never client-supplied
	AudioRolePlan *AudioRolePlan // for determining if dub-eligible speech exists
}

// SegmentRuleConfig defines the tunable parameters for canonical SpeechBlock segmentation.
type SegmentRuleConfig struct {
	MinSpeechBlockMs   int64 `json:"min_speech_block_ms"`
	MaxSpeechBlockMs   int64 `json:"max_speech_block_ms"`
	PauseSplitMs       int64 `json:"pause_split_ms"`        // gap >= this triggers a block split
	MinSilenceMs       int64 `json:"min_silence_ms"`        // gap >= this is a silence segment
	MinSpeakerChangeMs int64 `json:"min_speaker_change_ms"` // speaker-change gap threshold
	PunctuationSplit   bool  `json:"punctuation_split"`     // split on sentence-final punctuation
}

// DefaultSegmentRuleConfig returns the default segmentation parameters.
// These are empirically tunable (per fixture evidence, not universal hard-coded thresholds).
func DefaultSegmentRuleConfig() SegmentRuleConfig {
	return SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
}

// IsDubEligible returns true if the audio role plan contains at least one
// narration/dialogue segment.
func IsDubEligible(plan *AudioRolePlan) bool {
	if plan == nil {
		return false
	}
	for _, seg := range plan.Segments {
		if seg.Role == AudioRoleNarrationDialogue {
			return true
		}
	}
	return false
}
