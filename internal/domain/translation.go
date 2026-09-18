package domain

import (
	"errors"
	"time"
)

// Translation domain errors.
var (
	ErrMeaningPreservationFailed  = errors.New("meaning preservation validation failed")
	ErrFactCorrupted              = errors.New("fact or entity corrupted in translation")
	ErrNameCorrupted              = errors.New("named entity corrupted or dropped in translation")
	ErrNumberCorrupted            = errors.New("numerical value or quantity corrupted in translation")
	ErrNegationInverted           = errors.New("negation polarity inverted in translation")
	ErrTranslationVariantNotFound = errors.New("translation variant not found")
	ErrEmptyTranslationInput      = errors.New("empty translation input")
	ErrDubScriptVariantNotFound   = errors.New("dub script variant not found")
)

const (
	// TranslationSchemaVersion is part of the translation stage cache identity, so it
	// moves whenever a TranslationVariant's persisted contract changes. Version 2 adds
	// ReviewReason and the rule that a segment failing the meaning gate is persisted
	// with passed_qa_gate=false for operator review instead of rejecting the candidate:
	// a variant cached under version 1 predates both.
	TranslationSchemaVersion = 2
	DubScriptSchemaVersion   = 2

	// ReviewReasonMeaningCorrupted marks a dub script segment whose spoken text failed
	// the meaning-first gate. The operator corrects it or records a manual override.
	ReviewReasonMeaningCorrupted = "meaning_corrupted"
)

// TranslationSegment represents a single translated unit (typically mapped 1:1 to a SpeechBlock or visual text region).
type TranslationSegment struct {
	Index            int      `json:"index"`
	SourceText       string   `json:"source_text"`
	TargetText       string   `json:"target_text"`
	SpeakerID        string   `json:"speaker_id,omitempty"`
	StartMs          int64    `json:"start_ms,omitempty"`
	EndMs            int64    `json:"end_ms,omitempty"`
	KeyFacts         []string `json:"key_facts,omitempty"` // Extracted facts, entities, numbers
	NegationPolarity bool     `json:"negation_polarity"`   // true if negative statement
	QAConfidence     float64  `json:"qa_confidence"`       // Meaning preservation QA score (0.0 - 1.0)
	PassedQAGate     bool     `json:"passed_qa_gate"`
	// ReviewReason explains the meaning-gate violation that flagged this segment for
	// operator review. Set only when PassedQAGate is false; the operator either corrects
	// the target text or records a manual override.
	ReviewReason string `json:"review_reason,omitempty"`
}

// TranslationVariant is the immutable target-language meaning-preserving artifact.
// Consumed explicitly by downstream stages: T10 (visual text localization) and T13 (dubbing translation).
type TranslationVariant struct {
	ID                string               `json:"id"`
	SchemaVersion     int                  `json:"schema_version"`
	AssetID           string               `json:"asset_id"`
	RunID             string               `json:"run_id"`
	JobID             string               `json:"job_id,omitempty"`
	SourceLanguage    string               `json:"source_language"` // e.g. "zh"
	TargetLanguage    string               `json:"target_language"` // "vi" or "en"
	Segments          []TranslationSegment `json:"segments"`
	ProviderID        string               `json:"provider_id"`
	ModelName         string               `json:"model_name"`
	ModelVersion      string               `json:"model_version"`
	ServiceBaselineID string               `json:"service_baseline_id,omitempty"`
	ObservedModel     string               `json:"observed_model,omitempty"`
	SystemFingerprint string               `json:"system_fingerprint,omitempty"`
	CASHash           string               `json:"cas_hash,omitempty"`
	ProvenanceHash    string               `json:"provenance_hash,omitempty"`
	OverallQAScore    float64              `json:"overall_qa_score"`
	CreatedAt         time.Time            `json:"created_at"`
}

// TranslationInputSegment is a text input segment for translation.
type TranslationInputSegment struct {
	Index      int    `json:"index"`
	SourceText string `json:"source_text"`
	SpeakerID  string `json:"speaker_id,omitempty"`
	StartMs    int64  `json:"start_ms,omitempty"`
	EndMs      int64  `json:"end_ms,omitempty"`
}

// TranslationJobInput encapsulates inputs required to generate a TranslationVariant.
type TranslationJobInput struct {
	RunID                 string                    `json:"run_id"`
	AssetID               string                    `json:"asset_id"`
	JobID                 string                    `json:"job_id,omitempty"`
	SourceLanguage        string                    `json:"source_language"`
	TargetLanguage        string                    `json:"target_language"`
	Segments              []TranslationInputSegment `json:"segments"`
	TranscriptArtifactCAS string                    `json:"transcript_artifact_cas,omitempty"`
	ExecutionProfile      ExecutionProfile          `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                  `json:"authorized_credentials,omitempty"`
	ConsentGranted        bool                      `json:"consent_granted,omitempty"`
}

type DubScriptSegment struct {
	Index                 int      `json:"index"`
	SourceText            string   `json:"source_text"`
	MeaningText           string   `json:"meaning_text"` // Original translation from TranslationVariant
	SpokenText            string   `json:"spoken_text"`  // Duration-adapted, shorten-first phrasing
	SpeakerID             string   `json:"speaker_id,omitempty"`
	StartMs               int64    `json:"start_ms"`                  // Immutable source window start
	EndMs                 int64    `json:"end_ms"`                    // Immutable source window end
	SlotDurationMs        int64    `json:"slot_duration_ms"`          // EndMs - StartMs
	EstimatedDurationMs   int64    `json:"estimated_duration_ms"`     // Estimated spoken duration
	SourceSpeakingRateCPS float64  `json:"source_speaking_rate_cps"`  // Source syllable-like speech units per second (Han character ~= syllable)
	TargetSpeakingRateCPS float64  `json:"target_speaking_rate_cps"`  // Target syllables per second
	CadenceRatio          float64  `json:"cadence_ratio"`             // Target syllables/sec vs source syllables/sec (source-relative)
	SourceGapAfterMs      int64    `json:"source_gap_after_ms"`       // Immutable source silence until the next speech turn
	NaturalGapMs          int64    `json:"natural_gap_ms"`            // Predicted pause from estimated dub finish to the next source turn
	TargetWordBudget      int      `json:"target_word_budget"`        // Source-relative max target words for the immutable speech slot
	IsShortened           bool     `json:"is_shortened"`              // Whether shorten-first adaptation was applied
	RequiresReview        bool     `json:"requires_review,omitempty"` // Flagged if unresolvable duration overrun or QA fallback
	ReviewReason          string   `json:"review_reason,omitempty"`   // Reason code for operator review
	KeyFacts              []string `json:"key_facts,omitempty"`
	NegationPolarity      bool     `json:"negation_polarity"`
	QAConfidence          float64  `json:"qa_confidence"`
	PassedQAGate          bool     `json:"passed_qa_gate"`
}

// DubScriptVariant is the immutable target-language duration-adapted spoken script artifact.
// Produced by T13 and consumed explicitly by T14 (TTS & fit controller).
type DubScriptVariant struct {
	ID                    string             `json:"id"`
	SchemaVersion         int                `json:"schema_version"`
	AssetID               string             `json:"asset_id"`
	RunID                 string             `json:"run_id"`
	JobID                 string             `json:"job_id,omitempty"`
	SourceLanguage        string             `json:"source_language"` // e.g. "zh"
	TargetLanguage        string             `json:"target_language"` // "vi" or "en"
	TranslationVariantCAS string             `json:"translation_variant_cas,omitempty"`
	Segments              []DubScriptSegment `json:"segments"`
	ProviderID            string             `json:"provider_id"`
	ModelName             string             `json:"model_name"`
	ModelVersion          string             `json:"model_version"`
	CASHash               string             `json:"cas_hash,omitempty"`
	ProvenanceHash        string             `json:"provenance_hash,omitempty"`
	OverallQAScore        float64            `json:"overall_qa_score"`
	RequiresReview        bool               `json:"requires_review,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
}

// DubScriptJobInput encapsulates inputs required to generate a DubScriptVariant.
type DubScriptJobInput struct {
	RunID                 string           `json:"run_id"`
	AssetID               string           `json:"asset_id"`
	JobID                 string           `json:"job_id,omitempty"`
	SourceLanguage        string           `json:"source_language"`
	TargetLanguage        string           `json:"target_language"`
	TranslationVariantCAS string           `json:"translation_variant_cas,omitempty"`
	ExecutionProfile      ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string         `json:"authorized_credentials,omitempty"`
}
