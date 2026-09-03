package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SnapshotFileEntry represents one enumerated file inside a model snapshot.
type SnapshotFileEntry struct {
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
}

// SnapshotManifest defines the canonical manifest contract for an executable model snapshot.
// It enumerates every primary/dependency model, config, tokenizer, voice, or asset file.
type SnapshotManifest struct {
	SchemaVersion          string              `json:"schema_version"`
	ModelID                string              `json:"model_id"`
	ModelVersion           string              `json:"model_version"`
	Files                  []SnapshotFileEntry `json:"files"`
	SnapshotManifestSHA256 string              `json:"snapshot_manifest_sha256,omitempty"`
}

// ValidateRelativePath validates and normalizes a manifest file relative path.
// It enforces that the path remains strictly within its snapshot root:
// - rejects empty strings and dot paths (".", "./", ".\\", "..")
// - rejects absolute paths ("/foo", "\\foo")
// - rejects volume-qualified paths ("C:foo", "C:\\foo", "\\\\server\\share")
// - rejects parent directory traversal ("..", "../foo", "foo/../../bar")
func ValidateRelativePath(p string) (string, error) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", fmt.Errorf("empty relative_path")
	}

	// Reject volume name (e.g. C:, \\server\share)
	if vol := filepath.VolumeName(trimmed); vol != "" {
		return "", fmt.Errorf("volume-qualified path rejected: %s", p)
	}

	// Also check for drive letter colon on any platform (e.g. "c:foo")
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return "", fmt.Errorf("drive letter in path rejected: %s", p)
	}

	// Reject absolute paths
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "\\") {
		return "", fmt.Errorf("absolute path rejected: %s", p)
	}

	// Reject parent traversal (..) and dot components (.)
	parts := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	if len(parts) == 0 {
		return "", fmt.Errorf("empty or dot path rejected: %s", p)
	}
	for _, part := range parts {
		if part == ".." {
			return "", fmt.Errorf("parent directory traversal rejected: %s", p)
		}
		if part == "." {
			return "", fmt.Errorf("dot path segment rejected: %s", p)
		}
	}

	clean := filepath.ToSlash(filepath.Clean(trimmed))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("path escapes snapshot root: %s", p)
	}

	return clean, nil
}

// NormalizeRelativePath returns a normalized, forward-slashed, clean relative path.
func NormalizeRelativePath(p string) string {
	norm, err := ValidateRelativePath(p)
	if err != nil {
		cleaned := filepath.ToSlash(filepath.Clean(p))
		cleaned = strings.TrimPrefix(cleaned, "/")
		cleaned = strings.TrimPrefix(cleaned, "./")
		return cleaned
	}
	return norm
}

// ValidateSnapshotManifest validates the manifest contract:
// - schema_version, model_id, model_version present
// - files non-empty
// - each relative_path is valid and strictly contained in the snapshot root
// - no duplicate normalized paths
// - valid 64-char sha256 and non-negative size_bytes
func ValidateSnapshotManifest(m *SnapshotManifest) error {
	if m == nil {
		return fmt.Errorf("snapshot manifest is nil")
	}
	if strings.TrimSpace(m.SchemaVersion) == "" {
		return fmt.Errorf("manifest schema_version is required")
	}
	if strings.TrimSpace(m.ModelID) == "" {
		return fmt.Errorf("manifest model_id is required")
	}
	if strings.TrimSpace(m.ModelVersion) == "" {
		return fmt.Errorf("manifest model_version is required")
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("snapshot manifest must declare at least one file")
	}

	seenPaths := make(map[string]bool, len(m.Files))
	for i, f := range m.Files {
		normPath, err := ValidateRelativePath(f.RelativePath)
		if err != nil {
			return fmt.Errorf("file[%d] invalid relative_path %q: %w", i, f.RelativePath, err)
		}
		if seenPaths[normPath] {
			return fmt.Errorf("duplicate normalized path in manifest: %s", normPath)
		}
		seenPaths[normPath] = true

		sha := strings.TrimSpace(f.SHA256)
		if len(sha) != 64 {
			return fmt.Errorf("file[%d] %s has invalid sha256 length %d (want 64)", i, normPath, len(sha))
		}
		if f.SizeBytes < 0 {
			return fmt.Errorf("file[%d] %s has negative size: %d", i, normPath, f.SizeBytes)
		}
	}
	return nil
}

// CanonicalManifestBytes computes the deterministic JSON representation of the manifest files
// sorted lexicographically by normalized relative path.
func CanonicalManifestBytes(m *SnapshotManifest) ([]byte, error) {
	if err := ValidateSnapshotManifest(m); err != nil {
		return nil, err
	}

	// Copy and sort entries deterministically by normalized relative path
	entries := make([]SnapshotFileEntry, len(m.Files))
	for i, f := range m.Files {
		normPath, _ := ValidateRelativePath(f.RelativePath)
		entries[i] = SnapshotFileEntry{
			RelativePath: normPath,
			SHA256:       strings.ToLower(strings.TrimSpace(f.SHA256)),
			SizeBytes:    f.SizeBytes,
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelativePath < entries[j].RelativePath
	})

	// Canonical payload representation
	canonicalPayload := struct {
		SchemaVersion string              `json:"schema_version"`
		ModelID       string              `json:"model_id"`
		ModelVersion  string              `json:"model_version"`
		Files         []SnapshotFileEntry `json:"files"`
	}{
		SchemaVersion: m.SchemaVersion,
		ModelID:       m.ModelID,
		ModelVersion:  m.ModelVersion,
		Files:         entries,
	}

	return json.Marshal(canonicalPayload)
}

// ComputeSnapshotManifestSHA256 computes the deterministic SHA-256 digest of the canonical manifest bytes.
func ComputeSnapshotManifestSHA256(m *SnapshotManifest) (string, error) {
	b, err := CanonicalManifestBytes(m)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

// RuntimeIdentity captures execution environment identity, separate from model snapshot identity.
type RuntimeIdentity struct {
	AdapterRevision        string            `json:"adapter_revision"`
	BinarySHA256           string            `json:"binary_sha256,omitempty"`
	EnvironmentLockHash    string            `json:"environment_lock_hash,omitempty"`
	RuntimeVersions        map[string]string `json:"runtime_versions,omitempty"`
	PrimarySnapshotSHA256  string            `json:"primary_snapshot_sha256"`
	DependencySnapshotSHAs []string          `json:"dependency_snapshot_sha256s,omitempty"`
	RuntimeManifestSHA256  string            `json:"runtime_manifest_sha256,omitempty"`
}

// ComputeRuntimeManifestSHA256 computes a deterministic digest for the runtime identity binding.
func (r *RuntimeIdentity) ComputeRuntimeManifestSHA256() string {
	var deps []string
	if len(r.DependencySnapshotSHAs) > 0 {
		deps = make([]string, len(r.DependencySnapshotSHAs))
		copy(deps, r.DependencySnapshotSHAs)
		sort.Strings(deps)
	}

	type canonicalRuntime struct {
		AdapterRevision        string            `json:"adapter_revision"`
		BinarySHA256           string            `json:"binary_sha256,omitempty"`
		EnvironmentLockHash    string            `json:"environment_lock_hash,omitempty"`
		RuntimeVersions        map[string]string `json:"runtime_versions,omitempty"`
		PrimarySnapshotSHA256  string            `json:"primary_snapshot_sha256"`
		DependencySnapshotSHAs []string          `json:"dependency_snapshot_sha256s,omitempty"`
	}

	b, _ := json.Marshal(canonicalRuntime{
		AdapterRevision:        r.AdapterRevision,
		BinarySHA256:           r.BinarySHA256,
		EnvironmentLockHash:    r.EnvironmentLockHash,
		RuntimeVersions:        r.RuntimeVersions,
		PrimarySnapshotSHA256:  r.PrimarySnapshotSHA256,
		DependencySnapshotSHAs: deps,
	})

	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

// SnapshotEvidenceFile represents one file in portable release evidence (no local absolute paths).
type SnapshotEvidenceFile struct {
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
}

// SnapshotPortableEvidence represents portable metadata without weights or machine-local paths.
type SnapshotPortableEvidence struct {
	DependencyName         string                 `json:"dependency_name"`
	Version                string                 `json:"version"`
	SnapshotManifestSHA256 string                 `json:"snapshot_manifest_sha256"`
	Files                  []SnapshotEvidenceFile `json:"files,omitempty"`
	RuntimeIdentity        *RuntimeIdentity       `json:"runtime_identity,omitempty"`
	LicenseManifestID      string                 `json:"license_manifest_id,omitempty"`
	VerificationEventID    string                 `json:"verification_event_id,omitempty"`
	VerifiedAt             time.Time              `json:"verified_at"`
}

// SnapshotVerificationEvent records an immutable audit record of a snapshot verification event in SQLite.
type SnapshotVerificationEvent struct {
	ID                     string    `json:"id"`
	DependencyName         string    `json:"dependency_name"`
	Version                string    `json:"version"`
	SnapshotManifestSHA256 string    `json:"snapshot_manifest_sha256"`
	Outcome                string    `json:"outcome"`
	ErrorMessage           string    `json:"error_message,omitempty"`
	FileCount              int       `json:"file_count"`
	TotalBytes             int64     `json:"total_bytes"`
	VerifierVersion        string    `json:"verifier_version"`
	VerifiedAt             time.Time `json:"verified_at"`
}
