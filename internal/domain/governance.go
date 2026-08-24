package domain

import (
	"errors"
	"time"
)

var (
	ErrPolicyBlocked          = errors.New("fail-closed: provider is blocked by governance policy")
	ErrConsentRequired        = errors.New("provider requires explicit operator consent for execution")
	ErrAuthRequired           = errors.New("fail-closed: provider requires valid credential authorization reference")
	ErrNoEligibleProvider     = errors.New("no eligible provider found satisfying policy, capability, and health")
	ErrLicenseManifestMissing = errors.New("fail-closed: missing or unverified license manifest entry for dependency")
	ErrCircuitOpen            = errors.New("circuit breaker is open for provider due to repeated failures")
	ErrRawSecretForbidden     = errors.New("storing or persisting raw secrets is forbidden; use safe credential reference")
	ErrQualityRejected        = errors.New("candidate output rejected by quality evaluation gate")
	ErrInvalidPolicyState     = errors.New("invalid policy state: must be ALLOWED, REQUIRES_EXPLICIT_CONSENT, REQUIRES_AUTHORIZATION, or BLOCKED")
	ErrUnsupportedStorageType = errors.New("unsupported storage_type: must be 'env_ref' or 'os_credential_store'")
)

// PolicyState represents the four governance states for provider/model routing.
type PolicyState string

const (
	PolicyAllowed                 PolicyState = "ALLOWED"
	PolicyRequiresExplicitConsent PolicyState = "REQUIRES_EXPLICIT_CONSENT"
	PolicyRequiresAuthorization   PolicyState = "REQUIRES_AUTHORIZATION"
	PolicyBlocked                 PolicyState = "BLOCKED"
)

// IsValid checks if the policy state is one of the four canonical governance states.
func (s PolicyState) IsValid() bool {
	switch s {
	case PolicyAllowed, PolicyRequiresExplicitConsent, PolicyRequiresAuthorization, PolicyBlocked:
		return true
	default:
		return false
	}
}

const (
	StorageTypeEnvRef            = "env_ref"
	StorageTypeOSCredentialStore = "os_credential_store"
)

// ExecutionProfile represents the deployment tier preference.
type ExecutionProfile string

const (
	ExecutionProfileLocal  ExecutionProfile = "local"
	ExecutionProfileHybrid ExecutionProfile = "hybrid" // Default in Phase 1
	ExecutionProfileCloud  ExecutionProfile = "cloud"
)

// Four independent license obligation layers.
const (
	LicenseLayerCode         = "CODE_LICENSE"
	LicenseLayerModel        = "MODEL_LICENSE"
	LicenseLayerData         = "DATA_LICENSE"
	LicenseLayerServiceTerms = "SERVICE_TERMS"
)

// LicenseManifestEntry defines immutable, versioned provenance and licensing terms for an auto-downloaded model weight, checkpoint, or external dependency.
type LicenseManifestEntry struct {
	ID             string    `json:"id"`
	DependencyName string    `json:"dependency_name"`
	Version        string    `json:"version"` // e.g. "1.0.0", "1.7b", "v1"
	SHA256         string    `json:"sha256"`
	SourceRepo     string    `json:"source_repo"`
	CodeLicense    string    `json:"code_license"`
	ModelLicense   string    `json:"model_license"`
	DataLicense    string    `json:"data_license"`
	ServiceTerms   string    `json:"service_terms"`
	Verified       bool      `json:"verified"`
	CreatedAt      time.Time `json:"created_at"`
}

// CredentialRef represents a safe reference to OS-level secure credential storage, keeping secrets out of logs, CAS, and SQLite.
type CredentialRef struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ProviderID  string    `json:"provider_id,omitempty"` // provider ID this credential authorizes
	StorageType string    `json:"storage_type"`          // e.g. "os_credential_store", "env_ref"
	KeyRef      string    `json:"key_ref"`               // safe token name, never raw secret value
	CreatedAt   time.Time `json:"created_at"`
}

// ProviderCapability declares the capabilities and operational metrics of a provider.
type ProviderCapability struct {
	Stage          string   `json:"stage"` // "asr", "aligner", "tts", "separator", "ocr", "translation"
	Languages      []string `json:"languages"`
	ExecutionTier  string   `json:"execution_tier"` // "local", "hybrid", "cloud"
	CostPerUnit    float64  `json:"cost_per_unit"`  // Relative cost index (0.0 for free local)
	QualityScore   float64  `json:"quality_score"`  // 0.0 - 1.0 baseline quality rating
	MaxConcurrency int      `json:"max_concurrency"`
	Features       []string `json:"features,omitempty"`
	MaxRetries     *int     `json:"max_retries,omitempty"` // Optional per-provider retry limit override
}

// CandidateEvaluation records individual evaluation metrics during router decision making.
type CandidateEvaluation struct {
	ProviderID    string  `json:"provider_id"`
	Eligible      bool    `json:"eligible"`
	PolicyState   string  `json:"policy_state"`
	HealthOK      bool    `json:"health_ok"`
	CircuitClosed bool    `json:"circuit_closed"`
	Score         float64 `json:"score"`
	RejectionCode string  `json:"rejection_code,omitempty"`
	Reason        string  `json:"reason,omitempty"`
}

// SelectionDecision is an immutable, append-only record of a provider routing decision.
type SelectionDecision struct {
	ID                  string                `json:"id"`
	RunID               string                `json:"run_id"`
	Stage               string                `json:"stage"`
	SelectedProviderID  string                `json:"selected_provider_id"`
	CandidatesEvaluated []CandidateEvaluation `json:"candidates_evaluated"`
	PolicyCheckResult   string                `json:"policy_check_result"`
	DecisionReason      string                `json:"decision_reason"`
	CreatedAt           time.Time             `json:"created_at"`
}

// ProviderAttempt records an immutable invocation attempt for auditing and retry provenance.
type ProviderAttempt struct {
	ID            string    `json:"id"`
	RunID         string    `json:"run_id"`
	Stage         string    `json:"stage"`
	ProviderID    string    `json:"provider_id"`
	ModelName     string    `json:"model_name"`
	ModelVersion  string    `json:"model_version"`
	InputHash     string    `json:"input_hash"`
	AttemptNumber int       `json:"attempt_number"`
	Status        string    `json:"status"` // "succeeded", "failed", "quality_failed", "policy_rejected", "circuit_broken"
	ErrorMessage  string    `json:"error_message,omitempty"`
	LatencyMs     int64     `json:"latency_ms"`
	CostUnits     float64   `json:"cost_units"`
	CreatedAt     time.Time `json:"created_at"`
}

// StageCacheIdentityInput encapsulates all components required to compute a deterministic stage CAS cache key.
type StageCacheIdentityInput struct {
	Stage          string   `json:"stage"`
	InputHashes    []string `json:"input_hashes"`
	SemanticConfig any      `json:"semantic_config"`
	ProviderID     string   `json:"provider_id"`
	ModelName      string   `json:"model_name"`
	ModelVersion   string   `json:"model_version"`
	Language       string   `json:"language"`
	SchemaVersion  int      `json:"schema_version"`
}

// TelemetryConfig configures telemetry reporting (disabled by default in Phase 1).
type TelemetryConfig struct {
	Enabled    *bool   `json:"enabled,omitempty"`
	Endpoint   string  `json:"endpoint,omitempty"`
	SampleRate float64 `json:"sample_rate,omitempty"`
}

// IsEnabled returns true if telemetry is explicitly enabled (defaults to false).
func (t TelemetryConfig) IsEnabled() bool {
	if t.Enabled == nil {
		return false
	}
	return *t.Enabled
}

// LayeredConfig aggregates the layered configuration resolution hierarchy.
type LayeredConfig struct {
	Profile           ExecutionProfile `json:"profile"`
	Telemetry         TelemetryConfig  `json:"telemetry"`
	ZeroOverrunStrict *bool            `json:"zero_overrun_strict,omitempty"`
	MaxRetries        *int             `json:"max_retries,omitempty"`
	MaxAudioOverrunMs *int64           `json:"max_audio_overrun_ms,omitempty"`
	CredentialRefs    []string         `json:"credential_refs,omitempty"`
	CustomSettings    map[string]any   `json:"custom_settings,omitempty"`
}

// IsZeroOverrunStrict returns true if zero overrun enforcement is active (defaults to true).
func (c LayeredConfig) IsZeroOverrunStrict() bool {
	if c.ZeroOverrunStrict == nil {
		return true
	}
	return *c.ZeroOverrunStrict
}

// GetMaxRetries returns the configured max retries (defaults to 2).
func (c LayeredConfig) GetMaxRetries() int {
	if c.MaxRetries == nil {
		return 2
	}
	return *c.MaxRetries
}

// GetMaxAudioOverrunMs returns the configured max audio overrun in ms (defaults to 0).
func (c LayeredConfig) GetMaxAudioOverrunMs() int64 {
	if c.MaxAudioOverrunMs == nil {
		return 0
	}
	return *c.MaxAudioOverrunMs
}
