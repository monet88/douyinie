package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

// CanonicalizeConfig converts an arbitrary semantic config into a stable, deterministic JSON string.
func CanonicalizeConfig(cfg any) ([]byte, error) {
	if cfg == nil {
		return []byte("{}"), nil
	}
	rawBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	var parsed any
	if err := json.Unmarshal(rawBytes, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal config for canonicalization: %w", err)
	}

	return json.Marshal(parsed)
}

// ComputeStageCacheKey calculates a deterministic SHA-256 hash representing a stage cache identity.
// Invariant: Path, mtime, JobID, RunID are strictly excluded to ensure deterministic caching across executions.
func ComputeStageCacheKey(input domain.StageCacheIdentityInput) (string, error) {
	sortedInputHashes := make([]string, len(input.InputHashes))
	copy(sortedInputHashes, input.InputHashes)
	sort.Strings(sortedInputHashes)

	canonicalConfig, err := CanonicalizeConfig(input.SemanticConfig)
	if err != nil {
		return "", fmt.Errorf("canonicalize semantic config: %w", err)
	}

	payload := struct {
		Stage         string   `json:"stage"`
		InputHashes   []string `json:"input_hashes"`
		Config        string   `json:"config"`
		ProviderID    string   `json:"provider_id"`
		ModelName     string   `json:"model_name"`
		ModelVersion  string   `json:"model_version"`
		Language      string   `json:"language"`
		SchemaVersion int      `json:"schema_version"`
	}{
		Stage:         strings.ToLower(strings.TrimSpace(input.Stage)),
		InputHashes:   sortedInputHashes,
		Config:        string(canonicalConfig),
		ProviderID:    strings.TrimSpace(input.ProviderID),
		ModelName:     strings.TrimSpace(input.ModelName),
		ModelVersion:  strings.TrimSpace(input.ModelVersion),
		Language:      strings.ToLower(strings.TrimSpace(input.Language)),
		SchemaVersion: input.SchemaVersion,
	}

	serialized, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal identity payload: %w", err)
	}

	hash := sha256.Sum256(serialized)
	return hex.EncodeToString(hash[:]), nil
}
