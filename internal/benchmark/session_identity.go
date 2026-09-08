package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CorpusDigest binds one frozen benchmark corpus to its cryptographic digest.
type CorpusDigest struct {
	Name         string `json:"name"`
	Digest       string `json:"digest"`
	EntriesCount int    `json:"entries_count,omitempty"`
}

// ProviderBaseline records the runtime or service baseline identity of a provider.
// For local models, SnapshotManifestDigest is the SHA-256 of the model snapshot manifest.
// For remote models, ServiceBaselineID is the upstream service version or gateway baseline.
type ProviderBaseline struct {
	Role                   string `json:"role"`
	ProviderID             string `json:"provider_id"`
	ModelVersion           string `json:"model_version,omitempty"`
	SnapshotManifestDigest string `json:"snapshot_manifest_digest,omitempty"`
	ServiceBaselineID      string `json:"service_baseline_id,omitempty"`
}

// EnvironmentAttestation records hardware and host environment characteristics.
// Invariant (Issue #59 / #63): on Windows WDDM per-process VRAM is not available;
// telemetry measures device-wide baseline and peak VRAM rather than exact per-process usage.
type EnvironmentAttestation struct {
	OS                   string `json:"os"`
	Arch                 string `json:"arch"`
	GPUModel             string `json:"gpu_model,omitempty"`
	TotalVRAMBytes       uint64 `json:"total_vram_bytes,omitempty"`
	DriverVersion        string `json:"driver_version,omitempty"`
	CUDAOrRuntimeVersion string `json:"cuda_or_runtime_version,omitempty"`
}

// OperatorMetadata captures execution context and attestation without modifying ground truth.
type OperatorMetadata struct {
	OperatorID     string    `json:"operator_id"`
	SessionPurpose string    `json:"session_purpose,omitempty"`
	Notes          string    `json:"notes,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
}

// SessionIdentityInput represents the complete set of parameters that deterministically
// identify a benchmark session envelope.
type SessionIdentityInput struct {
	CorpusDigests     []CorpusDigest         `json:"corpus_digests"`
	ExecutionProfile  string                 `json:"execution_profile"` // "local", "hybrid", "cloud"
	BuildIdentity     string                 `json:"build_identity"`    // commit SHA or build version
	ConfigSnapshot    any                    `json:"config_snapshot"`   // semantic configuration or digest
	ProviderBaselines []ProviderBaseline     `json:"provider_baselines"`
	Environment       EnvironmentAttestation `json:"environment"`
	Operator          OperatorMetadata       `json:"operator"`
}

type canonicalIdentityPayload struct {
	CorpusDigests     []CorpusDigest         `json:"corpus_digests"`
	ExecutionProfile  string                 `json:"execution_profile"`
	BuildIdentity     string                 `json:"build_identity"`
	ConfigJSON        string                 `json:"config_json"`
	ProviderBaselines []ProviderBaseline     `json:"provider_baselines"`
	Environment       EnvironmentAttestation `json:"environment"`
	OperatorID        string                 `json:"operator_id"`
	SessionPurpose    string                 `json:"session_purpose"`
}

// CanonicalizeConfig converts an arbitrary semantic config into a stable, deterministic JSON string.
func CanonicalizeConfig(cfg any) (string, error) {
	if cfg == nil {
		return "{}", nil
	}
	if s, ok := cfg.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return "{}", nil
		}
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			// If not JSON, treat as raw literal string
			return s, nil
		}
		canonicalBytes, err := json.Marshal(parsed)
		if err != nil {
			return "", fmt.Errorf("canonicalize config string: %w", err)
		}
		return string(canonicalBytes), nil
	}
	rawBytes, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	var parsed any
	if err := json.Unmarshal(rawBytes, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal config for canonicalization: %w", err)
	}
	canonicalBytes, err := json.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("canonicalize config object: %w", err)
	}
	return string(canonicalBytes), nil
}

// CanonicalizeSessionIdentity produces deterministic canonical JSON bytes representing
// the benchmark session identity.
func CanonicalizeSessionIdentity(input SessionIdentityInput) ([]byte, error) {
	// Normalize corpus digests first, then sort deterministically
	normalizedCorpus := make([]CorpusDigest, len(input.CorpusDigests))
	for i, c := range input.CorpusDigests {
		normalizedCorpus[i] = CorpusDigest{
			Name:         strings.TrimSpace(c.Name),
			Digest:       strings.ToLower(strings.TrimSpace(c.Digest)),
			EntriesCount: c.EntriesCount,
		}
	}
	sort.Slice(normalizedCorpus, func(i, j int) bool {
		if normalizedCorpus[i].Name != normalizedCorpus[j].Name {
			return normalizedCorpus[i].Name < normalizedCorpus[j].Name
		}
		if normalizedCorpus[i].Digest != normalizedCorpus[j].Digest {
			return normalizedCorpus[i].Digest < normalizedCorpus[j].Digest
		}
		return normalizedCorpus[i].EntriesCount < normalizedCorpus[j].EntriesCount
	})

	// Normalize provider baselines first, then sort deterministically
	normalizedBaselines := make([]ProviderBaseline, len(input.ProviderBaselines))
	for i, b := range input.ProviderBaselines {
		normalizedBaselines[i] = ProviderBaseline{
			Role:                   strings.ToLower(strings.TrimSpace(b.Role)),
			ProviderID:             strings.TrimSpace(b.ProviderID),
			ModelVersion:           strings.TrimSpace(b.ModelVersion),
			SnapshotManifestDigest: strings.ToLower(strings.TrimSpace(b.SnapshotManifestDigest)),
			ServiceBaselineID:      strings.TrimSpace(b.ServiceBaselineID),
		}
	}
	sort.Slice(normalizedBaselines, func(i, j int) bool {
		if normalizedBaselines[i].Role != normalizedBaselines[j].Role {
			return normalizedBaselines[i].Role < normalizedBaselines[j].Role
		}
		if normalizedBaselines[i].ProviderID != normalizedBaselines[j].ProviderID {
			return normalizedBaselines[i].ProviderID < normalizedBaselines[j].ProviderID
		}
		if normalizedBaselines[i].ModelVersion != normalizedBaselines[j].ModelVersion {
			return normalizedBaselines[i].ModelVersion < normalizedBaselines[j].ModelVersion
		}
		if normalizedBaselines[i].SnapshotManifestDigest != normalizedBaselines[j].SnapshotManifestDigest {
			return normalizedBaselines[i].SnapshotManifestDigest < normalizedBaselines[j].SnapshotManifestDigest
		}
		return normalizedBaselines[i].ServiceBaselineID < normalizedBaselines[j].ServiceBaselineID
	})

	configJSON, err := CanonicalizeConfig(input.ConfigSnapshot)
	if err != nil {
		return nil, fmt.Errorf("canonicalize config: %w", err)
	}

	payload := canonicalIdentityPayload{
		CorpusDigests:     normalizedCorpus,
		ExecutionProfile:  strings.ToLower(strings.TrimSpace(input.ExecutionProfile)),
		BuildIdentity:     strings.ToLower(strings.TrimSpace(input.BuildIdentity)),
		ConfigJSON:        configJSON,
		ProviderBaselines: normalizedBaselines,
		Environment: EnvironmentAttestation{
			OS:                   strings.ToLower(strings.TrimSpace(input.Environment.OS)),
			Arch:                 strings.ToLower(strings.TrimSpace(input.Environment.Arch)),
			GPUModel:             strings.TrimSpace(input.Environment.GPUModel),
			TotalVRAMBytes:       input.Environment.TotalVRAMBytes,
			DriverVersion:        strings.TrimSpace(input.Environment.DriverVersion),
			CUDAOrRuntimeVersion: strings.TrimSpace(input.Environment.CUDAOrRuntimeVersion),
		},
		OperatorID:     strings.TrimSpace(input.Operator.OperatorID),
		SessionPurpose: strings.TrimSpace(input.Operator.SessionPurpose),
	}

	return json.Marshal(payload)
}

// ComputeSessionID calculates a deterministic session identity from the given input.
// Returns:
//   - sessionID: prefixed identifier "bms_<64-char-hex>"
//   - identityDigest: 64-char lowercase hex SHA-256
//   - err: serialization error if any
func ComputeSessionID(input SessionIdentityInput) (sessionID string, identityDigest string, err error) {
	canonicalBytes, err := CanonicalizeSessionIdentity(input)
	if err != nil {
		return "", "", fmt.Errorf("compute session identity: %w", err)
	}

	hash := sha256.Sum256(canonicalBytes)
	identityDigest = hex.EncodeToString(hash[:])
	sessionID = "bms_" + identityDigest
	return sessionID, identityDigest, nil
}

// CompareSessionIdentities compares two session identities and returns a list of human-readable
// differences. An empty slice indicates identical identities.
func CompareSessionIdentities(expected, actual SessionIdentityInput) []string {
	var diffs []string

	if strings.ToLower(strings.TrimSpace(expected.ExecutionProfile)) != strings.ToLower(strings.TrimSpace(actual.ExecutionProfile)) {
		diffs = append(diffs, fmt.Sprintf("execution profile mismatch: expected %q, got %q", expected.ExecutionProfile, actual.ExecutionProfile))
	}
	if strings.ToLower(strings.TrimSpace(expected.BuildIdentity)) != strings.ToLower(strings.TrimSpace(actual.BuildIdentity)) {
		diffs = append(diffs, fmt.Sprintf("build identity mismatch: expected %q, got %q", expected.BuildIdentity, actual.BuildIdentity))
	}

	expCfg, _ := CanonicalizeConfig(expected.ConfigSnapshot)
	actCfg, _ := CanonicalizeConfig(actual.ConfigSnapshot)
	if expCfg != actCfg {
		diffs = append(diffs, fmt.Sprintf("config snapshot mismatch: expected %s, got %s", expCfg, actCfg))
	}

	// Compare corpus digests
	expCorpusMap := make(map[string]string)
	for _, c := range expected.CorpusDigests {
		expCorpusMap[strings.TrimSpace(c.Name)] = strings.ToLower(strings.TrimSpace(c.Digest))
	}
	actCorpusMap := make(map[string]string)
	for _, c := range actual.CorpusDigests {
		actCorpusMap[strings.TrimSpace(c.Name)] = strings.ToLower(strings.TrimSpace(c.Digest))
	}
	if len(expCorpusMap) != len(actCorpusMap) {
		diffs = append(diffs, fmt.Sprintf("corpus count mismatch: expected %d, got %d", len(expCorpusMap), len(actCorpusMap)))
	}
	for name, expDigest := range expCorpusMap {
		actDigest, ok := actCorpusMap[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("missing corpus %q in actual session", name))
		} else if expDigest != actDigest {
			diffs = append(diffs, fmt.Sprintf("corpus %q digest mismatch: expected %q, got %q", name, expDigest, actDigest))
		}
	}
	for name := range actCorpusMap {
		if _, ok := expCorpusMap[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("unexpected corpus %q in actual session", name))
		}
	}

	// Compare provider baselines
	expBaselineMap := make(map[string]string)
	for _, p := range expected.ProviderBaselines {
		key := fmt.Sprintf("%s:%s", strings.ToLower(strings.TrimSpace(p.Role)), strings.TrimSpace(p.ProviderID))
		val := fmt.Sprintf("model=%s,snapshot=%s,service=%s", strings.TrimSpace(p.ModelVersion), strings.ToLower(strings.TrimSpace(p.SnapshotManifestDigest)), strings.TrimSpace(p.ServiceBaselineID))
		expBaselineMap[key] = val
	}
	actBaselineMap := make(map[string]string)
	for _, p := range actual.ProviderBaselines {
		key := fmt.Sprintf("%s:%s", strings.ToLower(strings.TrimSpace(p.Role)), strings.TrimSpace(p.ProviderID))
		val := fmt.Sprintf("model=%s,snapshot=%s,service=%s", strings.TrimSpace(p.ModelVersion), strings.ToLower(strings.TrimSpace(p.SnapshotManifestDigest)), strings.TrimSpace(p.ServiceBaselineID))
		actBaselineMap[key] = val
	}
	if len(expBaselineMap) != len(actBaselineMap) {
		diffs = append(diffs, fmt.Sprintf("provider baseline count mismatch: expected %d, got %d", len(expBaselineMap), len(actBaselineMap)))
	}
	for key, expVal := range expBaselineMap {
		actVal, ok := actBaselineMap[key]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("missing provider baseline %q in actual session", key))
		} else if expVal != actVal {
			diffs = append(diffs, fmt.Sprintf("provider baseline %q mismatch: expected (%s), got (%s)", key, expVal, actVal))
		}
	}
	for key := range actBaselineMap {
		if _, ok := expBaselineMap[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("unexpected provider baseline %q in actual session", key))
		}
	}

	// Compare environment
	if strings.ToLower(strings.TrimSpace(expected.Environment.OS)) != strings.ToLower(strings.TrimSpace(actual.Environment.OS)) {
		diffs = append(diffs, fmt.Sprintf("environment OS mismatch: expected %q, got %q", expected.Environment.OS, actual.Environment.OS))
	}
	if strings.ToLower(strings.TrimSpace(expected.Environment.Arch)) != strings.ToLower(strings.TrimSpace(actual.Environment.Arch)) {
		diffs = append(diffs, fmt.Sprintf("environment Arch mismatch: expected %q, got %q", expected.Environment.Arch, actual.Environment.Arch))
	}
	if strings.TrimSpace(expected.Environment.GPUModel) != strings.TrimSpace(actual.Environment.GPUModel) {
		diffs = append(diffs, fmt.Sprintf("environment GPUModel mismatch: expected %q, got %q", expected.Environment.GPUModel, actual.Environment.GPUModel))
	}

	return diffs
}
