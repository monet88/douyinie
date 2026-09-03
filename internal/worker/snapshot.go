package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ConfigKeyModelSnapshot is the key under Command.Config for the model snapshot envelope.
const ConfigKeyModelSnapshot = "model_snapshot"

// ModelSnapshotRef describes a verified local model snapshot provided by RuntimeHost.
// Machine-local absolute paths are carried here across Seam 2 for local execution only,
// and are never exported to portable evidence bundles.
type ModelSnapshotRef struct {
	Role                   string `json:"role,omitempty"` // e.g. "primary", "vad", "det", "rec", "ori"
	DependencyName         string `json:"dependency_name"`
	Version                string `json:"version"`
	SnapshotManifestSHA256 string `json:"snapshot_manifest_sha256"`
	LocalPath              string `json:"local_path"`
}

// ModelSnapshotEnvelope is the typed envelope placed in Command.Config["model_snapshot"].
// It carries one primary snapshot ref plus zero-or-more dependency snapshot refs.
// Multiple dependencies may share a role.
type ModelSnapshotEnvelope struct {
	Primary      ModelSnapshotRef   `json:"primary"`
	Dependencies []ModelSnapshotRef `json:"dependencies,omitempty"`
}

// GetModelSnapshotEnvelope extracts and decodes the typed ModelSnapshotEnvelope from Command.Config.
// Returns nil, nil if the envelope is not present.
func GetModelSnapshotEnvelope(cfg map[string]any) (*ModelSnapshotEnvelope, error) {
	if cfg == nil {
		return nil, nil
	}
	raw, ok := cfg[ConfigKeyModelSnapshot]
	if !ok || raw == nil {
		return nil, nil
	}

	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal %s config: %w", ConfigKeyModelSnapshot, err)
	}

	var env ModelSnapshotEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("unmarshal %s config: %w", ConfigKeyModelSnapshot, err)
	}
	return &env, nil
}

// SetModelSnapshotEnvelope injects the typed ModelSnapshotEnvelope into Command.Config.
func SetModelSnapshotEnvelope(cfg map[string]any, env ModelSnapshotEnvelope) {
	if cfg != nil {
		cfg[ConfigKeyModelSnapshot] = env
	}
}

// ValidateSnapshotPaths validates that all required snapshot paths exist and are accessible directories/files.
// StageWorker validates required path presence without independently re-resolving model repositories.
func ValidateSnapshotPaths(env *ModelSnapshotEnvelope) error {
	if env == nil {
		return fmt.Errorf("model snapshot envelope is nil")
	}

	if strings.TrimSpace(env.Primary.LocalPath) == "" {
		return fmt.Errorf("primary model snapshot missing local_path")
	}
	if _, err := os.Stat(env.Primary.LocalPath); err != nil {
		return fmt.Errorf("primary model snapshot path inaccessible (%s): %w", env.Primary.LocalPath, err)
	}

	for i, dep := range env.Dependencies {
		if strings.TrimSpace(dep.LocalPath) == "" {
			return fmt.Errorf("dependency model snapshot [%d] (%s) missing local_path", i, dep.DependencyName)
		}
		if _, err := os.Stat(dep.LocalPath); err != nil {
			return fmt.Errorf("dependency model snapshot [%d] (%s) path inaccessible (%s): %w", i, dep.DependencyName, dep.LocalPath, err)
		}
	}
	return nil
}
