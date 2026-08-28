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
)

// TranslationSegment represents a single translated unit (typically mapped 1:1 to a SpeechBlock or visual text region).
type TranslationSegment struct {
	Index            int      `json:"index"`
	SourceText       string   `json:"source_text"`
	TargetText       string   `json:"target_text"`
	SpeakerID        string   `json:"speaker_id,omitempty"`
	StartMs          int64    `json:"start_ms,omitempty"`
	EndMs            int64    `json:"end_ms,omitempty"`
	KeyFacts         []string `json:"key_facts,omitempty"`        // Extracted facts, entities, numbers
	NegationPolarity bool     `json:"negation_polarity"`           // true if negative statement
	QAConfidence     float64  `json:"qa_confidence"`              // Meaning preservation QA score (0.0 - 1.0)
	PassedQAGate     bool     `json:"passed_qa_gate"`
}

// TranslationVariant is the immutable target-language meaning-preserving artifact.
// Consumed explicitly by downstream stages: T10 (visual text localization) and T13 (dubbing translation).
type TranslationVariant struct {
	ID             string               `json:"id"`
	AssetID        string               `json:"asset_id"`
	RunID          string               `json:"run_id"`
	JobID          string               `json:"job_id,omitempty"`
	SourceLanguage string               `json:"source_language"` // e.g. "zh"
	TargetLanguage string               `json:"target_language"` // "vi" or "en"
	Segments       []TranslationSegment `json:"segments"`
	ProviderID     string               `json:"provider_id"`
	ModelName      string               `json:"model_name"`
	ModelVersion   string               `json:"model_version"`
	CASHash        string               `json:"cas_hash,omitempty"`
	ProvenanceHash string               `json:"provenance_hash,omitempty"`
	OverallQAScore float64              `json:"overall_qa_score"`
	CreatedAt      time.Time            `json:"created_at"`
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
}
