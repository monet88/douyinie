package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrMixerOverrunRefused is returned when AudioMixService encounters a candidate segment whose measured duration exceeds its immutable slot.
	ErrMixerOverrunRefused = errors.New("audio mixer refused: candidate duration overruns immutable source slot")
	// ErrMixerAnchorDrift is returned when AudioMixService detects attempt to shift or alter source timing anchors.
	ErrMixerAnchorDrift = errors.New("audio mixer refused: source timing anchor alteration is prohibited")
	// ErrSoundtrackPreservationFailed is returned when soundtrack preservation cannot be satisfied.
	ErrSoundtrackPreservationFailed = errors.New("soundtrack preservation plan failed: invalid stems or configuration")
	// ErrAudioStemsNotFound is returned when required AudioStemArtifacts are missing.
	ErrAudioStemsNotFound = errors.New("audio stem artifacts not found")
	// ErrDubMixNotFound is returned when DubMixArtifact is not found.
	ErrDubMixNotFound = errors.New("dub mix artifact not found")
	// ErrSeparatorFailed is returned when vocal separator fails to isolate stems.
	ErrSeparatorFailed = errors.New("audio separation failed to produce stems")
	// ErrAudioRolePreflightRequired is returned when preflight metadata is required to generate an AudioRolePlan.
	ErrAudioRolePreflightRequired = errors.New("audio role plan requires source preflight report")
	// ErrAudioRoleEvidenceMissing is returned when normalized audio or stems evidence required for audio role analysis is missing.
	ErrAudioRoleEvidenceMissing = errors.New("audio role plan requires valid normalized audio and stem evidence")
)

const (
	AudioRolePlanSchemaVersion = 1
	AudioStemsSchemaVersion    = 2
	DubMixSchemaVersion        = 1
)

// AudioRolePlanProvenance captures deterministic provenance for AudioRolePlan.
type AudioRolePlanProvenance struct {
	AssetSHA256            string `json:"asset_sha256"`
	StemsCASHash           string `json:"stems_cas_hash,omitempty"`
	ProviderID             string `json:"provider_id"`
	ModelName              string `json:"model_name"`
	ModelVersion           string `json:"model_version"`
	ConfigHash             string `json:"config_hash,omitempty"`
	SnapshotManifestSHA256 string `json:"snapshot_manifest_sha256,omitempty"`
	RuntimeManifestSHA256  string `json:"runtime_manifest_sha256,omitempty"`
	SchemaVersion          int    `json:"schema_version"`
}

func (p AudioRolePlanProvenance) Hash() string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ComputeAudioRolePlanProvenanceHash computes the deterministic hash for AudioRolePlan.
func ComputeAudioRolePlanProvenanceHash(assetSHA256, stemsCASHash, providerID, modelName, modelVersion, configHash string, snapshotAndRuntimeIdentities ...string) string {
	var snapSHA, rtSHA string
	if len(snapshotAndRuntimeIdentities) > 0 {
		snapSHA = snapshotAndRuntimeIdentities[0]
	}
	if len(snapshotAndRuntimeIdentities) > 1 {
		rtSHA = snapshotAndRuntimeIdentities[1]
	}
	return AudioRolePlanProvenance{
		AssetSHA256:            assetSHA256,
		StemsCASHash:           stemsCASHash,
		ProviderID:             providerID,
		ModelName:              modelName,
		ModelVersion:           modelVersion,
		ConfigHash:             configHash,
		SnapshotManifestSHA256: snapSHA,
		RuntimeManifestSHA256:  rtSHA,
		SchemaVersion:          AudioRolePlanSchemaVersion,
	}.Hash()
}

// AudioRoleAnalysisRequest represents the input to an AudioRoleAnalyzer.
type AudioRoleAnalysisRequest struct {
	AssetID          string
	RunID            string
	SourceAudioPath  string
	VocalsPath       string
	BackgroundPath   string
	DurationMs       int64
	SampleRate       int
	Channels         int
	ExecutionProfile ExecutionProfile
}

// AudioRoleAnalysisResult captures the classified segments and provenance metadata.
type AudioRoleAnalysisResult struct {
	Segments        []AudioSegment
	ModelName       string
	ModelVersion    string
	RuntimeIdentity string
	ProviderID      string
}

// AudioRoleAnalyzer defines the pluggable seam for audio role classification.
type AudioRoleAnalyzer interface {
	AnalyzeAudioRoles(ctx context.Context, req AudioRoleAnalysisRequest) (*AudioRoleAnalysisResult, error)
	AnalyzerInfo() (providerID, modelName, modelVersion, configHash string)
}

// StemType identifies the acoustic content of an audio stem.
type StemType string

const (
	StemTypeVocals     StemType = "vocals"
	StemTypeBackground StemType = "background" // BGM, SFX, Ambience, Music-Vocals
	StemTypeMusic      StemType = "music"
	StemTypeSFX        StemType = "sfx"
	StemTypeAmbience   StemType = "ambience"
)

// AudioStem represents a single isolated audio track artifact.
type AudioStem struct {
	Type         StemType `json:"type"`
	AudioCASHash string   `json:"audio_cas_hash"`
	AudioCASPath string   `json:"audio_cas_path,omitempty"`
	SampleRate   int      `json:"sample_rate"`
	Channels     int      `json:"channels"`
	Format       string   `json:"format"` // "wav"
	DurationMs   int64    `json:"duration_ms"`
}

// AudioStemArtifacts is the immutable source-derived artifact containing separated audio stems (e.g. UVR baseline / Demucs fallback).
type AudioStemArtifacts struct {
	ID             string      `json:"id"`
	SchemaVersion  int         `json:"schema_version"`
	AssetID        string      `json:"asset_id"`
	ProviderID     string      `json:"provider_id"`
	ModelName      string      `json:"model_name"`
	ModelVersion   string      `json:"model_version"`
	Stems          []AudioStem `json:"stems"`
	CASHash        string      `json:"cas_hash,omitempty"`
	ProvenanceHash string      `json:"provenance_hash,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
}

// SoundtrackPreservationPlan defines how background stems are preserved and how dialogue is suppressed.
type SoundtrackPreservationPlan struct {
	AssetID             string               `json:"asset_id"`
	PreserveSinging     bool                 `json:"preserve_singing"`      // True: singing vocals kept in soundtrack
	PreserveSFX         bool                 `json:"preserve_sfx"`          // True: foley/sfx preserved throughout
	PreserveAmbience    bool                 `json:"preserve_ambience"`     // True: ambience preserved
	CrossfadeDurationMs int64                `json:"crossfade_duration_ms"` // 15-30ms smooth crossfade at speech boundaries
	DuckingGainDb       float64              `json:"ducking_gain_db"`       // Optional gentle ducking inside speech windows (e.g. -2dB)
	SpeechWindows       []PreservationWindow `json:"speech_windows"`        // Exact source speech intervals where dialogue is suppressed
	SingingWindows      []PreservationWindow `json:"singing_windows"`       // Singing intervals preserved untouched
}

// PreservationWindow represents a timeline slice with a specific mixing behavior.
type PreservationWindow struct {
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Action  string `json:"action"` // "suppress_dialogue", "preserve_music_vocal", "passthrough"
}

// DubMixArtifact is the final mixed audio track combining localized speech with preserved soundtrack.
type DubMixArtifact struct {
	ID                  string                     `json:"id"`
	SchemaVersion       int                        `json:"schema_version"`
	AssetID             string                     `json:"asset_id"`
	RunID               string                     `json:"run_id"`
	JobID               string                     `json:"job_id,omitempty"`
	TargetLanguage      string                     `json:"target_language"`
	AudioCASHash        string                     `json:"audio_cas_hash"`
	AudioCASPath        string                     `json:"audio_cas_path,omitempty"`
	SampleRate          int                        `json:"sample_rate"`
	Channels            int                        `json:"channels"`
	Format              string                     `json:"format"` // "wav"
	DurationMs          int64                      `json:"duration_ms"`
	DubSegmentsCAS      string                     `json:"dub_segments_cas,omitempty"`
	AudioStemsCAS       string                     `json:"audio_stems_cas,omitempty"`
	PreservationPlan    SoundtrackPreservationPlan `json:"preservation_plan"`
	DialogueSuppressed  bool                       `json:"dialogue_suppressed"`
	SoundtrackPreserved bool                       `json:"soundtrack_preserved"`
	CASHash             string                     `json:"cas_hash,omitempty"`
	ProvenanceHash      string                     `json:"provenance_hash,omitempty"`
	OverallStatus       string                     `json:"overall_status"` // "PASS", "REVIEW_REQUIRED", "REFUSED"
	RefusalReason       string                     `json:"refusal_reason,omitempty"`
	CreatedAt           time.Time                  `json:"created_at"`
}

// ComputeAudioStemsProvenanceHash computes deterministic hash for AudioStemArtifacts.
func ComputeAudioStemsProvenanceHash(assetID, providerID, modelName, modelVersion string) (string, error) {
	payload := struct {
		AssetID      string `json:"asset_id"`
		ProviderID   string `json:"provider_id"`
		ModelName    string `json:"model_name"`
		ModelVersion string `json:"model_version"`
		SchemaVer    int    `json:"schema_version"`
	}{
		AssetID:      assetID,
		ProviderID:   providerID,
		ModelName:    modelName,
		ModelVersion: modelVersion,
		SchemaVer:    AudioStemsSchemaVersion,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ComputeDubMixProvenanceHash computes deterministic hash for DubMixArtifact.
func ComputeDubMixProvenanceHash(assetID, targetLang, dubSegmentsCAS, stemsCAS string, plan SoundtrackPreservationPlan) (string, error) {
	payload := struct {
		AssetID        string                     `json:"asset_id"`
		TargetLanguage string                     `json:"target_language"`
		DubSegmentsCAS string                     `json:"dub_segments_cas"`
		StemsCAS       string                     `json:"stems_cas"`
		Plan           SoundtrackPreservationPlan `json:"plan"`
		SchemaVer      int                        `json:"schema_version"`
	}{
		AssetID:        assetID,
		TargetLanguage: targetLang,
		DubSegmentsCAS: dubSegmentsCAS,
		StemsCAS:       stemsCAS,
		Plan:           plan,
		SchemaVer:      DubMixSchemaVersion,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
