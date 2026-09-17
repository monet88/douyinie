package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
	SourceRevision         string            `json:"source_revision,omitempty"`
	BinarySHA256           string            `json:"binary_sha256,omitempty"`
	EnvironmentLockHash    string            `json:"environment_lock_hash,omitempty"`
	RuntimeVersions        map[string]string `json:"runtime_versions,omitempty"`
	PrimarySnapshotSHA256  string            `json:"primary_snapshot_sha256"`
	DependencySnapshotSHAs []string          `json:"dependency_snapshot_sha256s,omitempty"`
	RuntimeManifestSHA256  string            `json:"runtime_manifest_sha256,omitempty"`
}

// Pinned ZeroTTS runtime/model identities (Issue #92, runtime pack upgraded to v0.1.5).
const (
	PinnedZeroTTSModelID         = "zeroweight-ai/ZeroTTS"
	PinnedZeroTTSModelVersion    = "c2bfbd67dc648cac455077333f7cf5c18a2e3bb4"
	PinnedZeroTTSSourceRevision  = "47e466d7a1a36517cfd240de536523d17c00adac"
	PinnedZeroTTSPackageVersion  = "0.1.5"
	PinnedZeroTTSAdapterRevision = "cmd/stageworker/adapters/tts_engine.py@zerotts-0.1.5"
)

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
		SourceRevision         string            `json:"source_revision,omitempty"`
		BinarySHA256           string            `json:"binary_sha256,omitempty"`
		EnvironmentLockHash    string            `json:"environment_lock_hash,omitempty"`
		RuntimeVersions        map[string]string `json:"runtime_versions,omitempty"`
		PrimarySnapshotSHA256  string            `json:"primary_snapshot_sha256"`
		DependencySnapshotSHAs []string          `json:"dependency_snapshot_sha256s,omitempty"`
	}

	b, _ := json.Marshal(canonicalRuntime{
		AdapterRevision:        r.AdapterRevision,
		SourceRevision:         r.SourceRevision,
		BinarySHA256:           r.BinarySHA256,
		EnvironmentLockHash:    r.EnvironmentLockHash,
		RuntimeVersions:        r.RuntimeVersions,
		PrimarySnapshotSHA256:  r.PrimarySnapshotSHA256,
		DependencySnapshotSHAs: deps,
	})

	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

// Frozen preset voice rotations for TTS (Issue #68).
var (
	FrozenKokoroVoiceOrder  = []string{"af_heart", "am_michael", "af_bella", "am_fenrir"}
	FrozenVieNeuVoiceOrder  = []string{"Trúc Ly", "Phạm Tuyên", "Đoan Trang", "Xuân Vĩnh"}
	FrozenZeroTTSVoiceOrder = []string{"quangminh", "maichi", "giahuy", "baotrang", "hamy", "huuduc", "kimoanh", "tiendat"}
)

// findManifestFile finds a file entry in the snapshot manifest whose normalized relative path satisfies match.
func findManifestFile(manifest SnapshotManifest, match func(normRelPath string) bool) *SnapshotFileEntry {
	for i, f := range manifest.Files {
		norm := strings.ToLower(NormalizeRelativePath(f.RelativePath))
		if match(norm) {
			return &manifest.Files[i]
		}
	}
	return nil
}

// verifySnapshotRegularFile verifies that entry exists on disk under cleanRoot and is a regular file.
func verifySnapshotRegularFile(cleanRoot string, entry *SnapshotFileEntry, assetDesc string) (string, error) {
	if entry == nil {
		return "", fmt.Errorf("%w: missing manifest file entry for %s", ErrSnapshotFileCorrupted, assetDesc)
	}
	fullPath := filepath.Join(cleanRoot, filepath.FromSlash(entry.RelativePath))
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("%w: %s file inaccessible (%s): %v", ErrSnapshotFileCorrupted, assetDesc, fullPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s path (%s) is a directory, expected regular file", ErrSnapshotFileCorrupted, assetDesc, fullPath)
	}
	return fullPath, nil
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

// ResolveTranslationGGUFEntrypoint deterministically resolves the Qwen3-4B-GGUF Q4_K_M entrypoint
// file from a verified snapshot manifest's declared files.
// It preserves snapshotRoot as a generic directory root and returns the absolute path to the regular GGUF file.
// It fails closed if the manifest-declared GGUF entrypoint is absent or ambiguous.
func ResolveTranslationGGUFEntrypoint(manifest SnapshotManifest, snapshotRoot, modelVersion string) (string, error) {
	cleanRoot := strings.TrimSpace(snapshotRoot)
	if cleanRoot == "" {
		return "", fmt.Errorf("%w: snapshot root is empty", ErrSnapshotFileCorrupted)
	}

	var ggufEntries []SnapshotFileEntry
	for _, f := range manifest.Files {
		norm := strings.ToLower(NormalizeRelativePath(f.RelativePath))
		if strings.HasSuffix(norm, ".gguf") {
			ggufEntries = append(ggufEntries, f)
		}
	}
	if len(ggufEntries) == 0 {
		return "", fmt.Errorf("%w: no .gguf files declared in snapshot manifest for %s", ErrSnapshotEntrypointAbsent, manifest.ModelID)
	}

	var q4kmEntries []SnapshotFileEntry
	var conflictingEntries []SnapshotFileEntry

	for _, f := range ggufEntries {
		norm := strings.ToLower(NormalizeRelativePath(f.RelativePath))
		if strings.Contains(norm, "q4_k_m") || strings.Contains(norm, "q4-k-m") {
			q4kmEntries = append(q4kmEntries, f)
			continue
		}
		for _, tag := range []string{"q8_0", "q8_k", "q5_k", "q5_0", "q6_k", "q2_k", "q3_k", "fp16", "f16", "f32"} {
			if strings.Contains(norm, tag) {
				conflictingEntries = append(conflictingEntries, f)
				break
			}
		}
	}

	var chosen SnapshotFileEntry
	if len(q4kmEntries) == 1 {
		chosen = q4kmEntries[0]
	} else if len(q4kmEntries) > 1 {
		return "", fmt.Errorf("%w: multiple (%d) Q4_K_M GGUF files declared in manifest for %s",
			ErrSnapshotEntrypointAmbiguous, len(q4kmEntries), manifest.ModelID)
	} else {
		if len(conflictingEntries) == len(ggufEntries) {
			return "", fmt.Errorf("%w: no Q4_K_M GGUF entrypoint found in manifest for %s (%d conflicting quant files)",
				ErrSnapshotEntrypointAbsent, manifest.ModelID, len(conflictingEntries))
		}
		nonConflicting := len(ggufEntries) - len(conflictingEntries)
		if nonConflicting == 1 && len(ggufEntries) == 1 {
			chosen = ggufEntries[0]
		} else if nonConflicting > 1 {
			return "", fmt.Errorf("%w: ambiguous GGUF entrypoints (%d files) in manifest for %s without explicit Q4_K_M tag",
				ErrSnapshotEntrypointAmbiguous, nonConflicting, manifest.ModelID)
		} else {
			return "", fmt.Errorf("%w: no Q4_K_M GGUF entrypoint found in manifest for %s",
				ErrSnapshotEntrypointAbsent, manifest.ModelID)
		}
	}

	fullPath := filepath.Join(cleanRoot, filepath.FromSlash(chosen.RelativePath))
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("%w: resolved entrypoint file inaccessible (%s): %v",
			ErrSnapshotFileCorrupted, fullPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: resolved entrypoint path (%s) is a directory, expected regular file",
			ErrSnapshotFileCorrupted, fullPath)
	}
	return fullPath, nil
}

// ResolveTTSVoiceEntrypoint validates that the requested voice asset is part of the verified
// model snapshot manifest and accessible on disk, failing closed if missing or unverified.
// For Kokoro, it ensures the manifest declares the exact voice file (e.g. voices/<voice_id>.pt),
// validates the file on disk, and verifies Kokoro checkpoint SHA-256 integrity if present.
// For VieNeu, it ensures the manifest declares model/catalog assets, validates the preset voice
// identity is one of the verified VieNeu presets, and ensures the snapshot root is accessible.
func ResolveTTSVoiceEntrypoint(manifest SnapshotManifest, snapshotRoot, modelName, voiceID string) (string, error) {
	cleanRoot := strings.TrimSpace(snapshotRoot)
	if cleanRoot == "" {
		return "", fmt.Errorf("%w: snapshot root is empty", ErrSnapshotFileCorrupted)
	}
	vID := strings.TrimSpace(voiceID)
	if vID == "" {
		return "", fmt.Errorf("%w: voice_id is required for TTS snapshot validation", ErrTTSVoiceAssetMissing)
	}

	lowerModel := strings.ToLower(modelName)
	if strings.Contains(lowerModel, "zerotts") || strings.Contains(lowerModel, "zeroweight") {
		validZeroTTS := false
		for _, v := range FrozenZeroTTSVoiceOrder {
			if v == vID {
				validZeroTTS = true
				break
			}
		}
		if !validZeroTTS {
			return "", fmt.Errorf("%w: unverified or unknown ZeroTTS voice %q (must be one of %s)",
				ErrTTSVoiceAssetMissing, vID, strings.Join(FrozenZeroTTSVoiceOrder, ", "))
		}

		required := []struct {
			rel  string
			desc string
		}{
			{"config.json", "ZeroTTS config"},
			{"tokenizer.json", "ZeroTTS tokenizer"},
			{"null_voice_emb.npy", "ZeroTTS null voice embedding"},
			{"onnx/text_encoder.onnx", "ZeroTTS text encoder"},
			{"onnx/prefix_step.onnx", "ZeroTTS prefix step model"},
			{"onnx/local_frame_decode.onnx", "ZeroTTS local frame decoder"},
			{"voices/index.json", "ZeroTTS voice index"},
		}
		for _, asset := range required {
			entry := findManifestFile(manifest, func(norm string) bool { return norm == asset.rel })
			if entry == nil {
				return "", fmt.Errorf("%w: %s (%s) not declared in snapshot manifest for %s",
					ErrSnapshotFileCorrupted, asset.desc, asset.rel, manifest.ModelID)
			}
			if _, err := verifySnapshotRegularFile(cleanRoot, entry, asset.desc); err != nil {
				return "", err
			}
		}

		codecEntries := 0
		for i := range manifest.Files {
			norm := strings.ToLower(NormalizeRelativePath(manifest.Files[i].RelativePath))
			if strings.HasPrefix(norm, "onnx/codec/") {
				codecEntries++
				if _, err := verifySnapshotRegularFile(cleanRoot, &manifest.Files[i], "ZeroTTS codec model"); err != nil {
					return "", err
				}
			}
		}
		if codecEntries == 0 {
			return "", fmt.Errorf("%w: ZeroTTS codec assets under onnx/codec/ not declared in snapshot manifest for %s",
				ErrSnapshotFileCorrupted, manifest.ModelID)
		}

		voiceRel := strings.ToLower("voices/" + vID + "/voice.npz")
		voiceEntry := findManifestFile(manifest, func(norm string) bool { return norm == voiceRel })
		if voiceEntry == nil {
			return "", fmt.Errorf("%w: canonical ZeroTTS voice asset %s not declared in snapshot manifest for %s",
				ErrTTSVoiceAssetMissing, "voices/"+vID+"/voice.npz", manifest.ModelID)
		}
		return verifySnapshotRegularFile(cleanRoot, voiceEntry, "ZeroTTS voice asset")
	}

	if strings.Contains(lowerModel, "kokoro") {
		// Pinned Kokoro rotation
		validKokoro := false
		for _, v := range FrozenKokoroVoiceOrder {
			if v == vID {
				validKokoro = true
				break
			}
		}
		if !validKokoro {
			return "", fmt.Errorf("%w: unverified or unknown Kokoro voice %q (must be one of %s)",
				ErrTTSVoiceAssetMissing, vID, strings.Join(FrozenKokoroVoiceOrder, ", "))
		}

		// 1. Manifest must declare exact local config.json
		configEntry := findManifestFile(manifest, func(norm string) bool {
			return norm == "config.json"
		})
		if configEntry == nil {
			return "", fmt.Errorf("%w: Kokoro config.json not declared in snapshot manifest for %s",
				ErrSnapshotFileCorrupted, manifest.ModelID)
		}
		if _, err := verifySnapshotRegularFile(cleanRoot, configEntry, "Kokoro config.json"); err != nil {
			return "", err
		}

		// 2. Manifest must declare pinned checkpoint file with exact SHA-256
		const pinnedKokoroCheckpointSHA = "496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4"
		ckptEntry := findManifestFile(manifest, func(norm string) bool {
			return norm == "kokoro-v1_0.pth" || norm == "kokoro-v1.0.pth" || norm == "kokoro.pth"
		})
		if ckptEntry == nil {
			return "", fmt.Errorf("%w: Kokoro checkpoint file (.pth) not declared in snapshot manifest for %s",
				ErrSnapshotFileCorrupted, manifest.ModelID)
		}
		if !strings.EqualFold(ckptEntry.SHA256, pinnedKokoroCheckpointSHA) {
			return "", fmt.Errorf("%w: Kokoro checkpoint %s SHA-256 mismatch: expected %s, got %s",
				ErrSnapshotDigestMismatch, ckptEntry.RelativePath, pinnedKokoroCheckpointSHA, ckptEntry.SHA256)
		}
		if _, err := verifySnapshotRegularFile(cleanRoot, ckptEntry, "Kokoro checkpoint file"); err != nil {
			return "", err
		}

		// 3. Manifest must declare canonical voice file: voices/<voiceID>.pt
		canonicalVoiceRel := strings.ToLower("voices/" + vID + ".pt")
		voiceEntry := findManifestFile(manifest, func(norm string) bool {
			return norm == canonicalVoiceRel
		})
		if voiceEntry == nil {
			return "", fmt.Errorf("%w: canonical voice asset %s not declared in snapshot manifest for %s",
				ErrTTSVoiceAssetMissing, "voices/"+vID+".pt", manifest.ModelID)
		}
		return verifySnapshotRegularFile(cleanRoot, voiceEntry, "Kokoro voice asset")
	}

	if strings.Contains(lowerModel, "vieneu") || strings.Contains(lowerModel, "pnnbao") {
		// Pinned VieNeu rotation
		validVieNeu := false
		for _, v := range FrozenVieNeuVoiceOrder {
			if v == vID {
				validVieNeu = true
				break
			}
		}
		if !validVieNeu {
			return "", fmt.Errorf("%w: unverified or unknown VieNeu voice %q (must be one of %s)",
				ErrTTSVoiceAssetMissing, vID, strings.Join(FrozenVieNeuVoiceOrder, ", "))
		}

		if len(manifest.Files) == 0 {
			return "", fmt.Errorf("%w: snapshot manifest for %s contains no declared files",
				ErrSnapshotFileCorrupted, manifest.ModelID)
		}

		// Ensure snapshot root exists
		info, err := os.Stat(cleanRoot)
		if err != nil {
			return "", fmt.Errorf("%w: VieNeu snapshot root inaccessible (%s): %v",
				ErrSnapshotFileCorrupted, cleanRoot, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%w: VieNeu snapshot root (%s) is not a directory",
				ErrSnapshotFileCorrupted, cleanRoot)
		}

		// VieNeu must bind strictly to the real pinned upstream v3 Turbo voice catalog/layout:
		// Canonical upstream relative path is src/vieneu/assets/voices_v3_turbo.json.
		// Alternate catalog paths are strictly rejected on the RC hard route.
		catalogEntry := findManifestFile(manifest, func(norm string) bool {
			return norm == "src/vieneu/assets/voices_v3_turbo.json"
		})
		if catalogEntry == nil {
			return "", fmt.Errorf("%w: VieNeu v3 Turbo voice catalog (src/vieneu/assets/voices_v3_turbo.json) not declared in snapshot manifest for %s",
				ErrTTSVoiceAssetMissing, manifest.ModelID)
		}
		fullCatalogPath, err := verifySnapshotRegularFile(cleanRoot, catalogEntry, "VieNeu voice catalog")
		if err != nil {
			return "", err
		}

		catalogBytes, err := os.ReadFile(fullCatalogPath)
		if err != nil {
			return "", fmt.Errorf("%w: failed to read VieNeu voice catalog (%s): %v",
				ErrSnapshotFileCorrupted, fullCatalogPath, err)
		}

		var catMap map[string]any
		if err := json.Unmarshal(catalogBytes, &catMap); err != nil {
			return "", fmt.Errorf("%w: failed to parse VieNeu voice catalog JSON (%s): %v",
				ErrSnapshotFileCorrupted, fullCatalogPath, err)
		}

		voiceExists := false
		if presets, ok := catMap["presets"].(map[string]any); ok {
			for pk, pv := range presets {
				if pk == vID {
					voiceExists = true
					break
				}
				if pm, ok := pv.(map[string]any); ok {
					if idStr, ok := pm["id"].(string); ok && idStr == vID {
						voiceExists = true
						break
					}
					if nameStr, ok := pm["name"].(string); ok && nameStr == vID {
						voiceExists = true
						break
					}
				}
			}
		}
		if !voiceExists {
			for k := range catMap {
				if k == vID {
					voiceExists = true
					break
				}
			}
		}
		if !voiceExists {
			if vList, ok := catMap["voices"].([]any); ok {
				for _, item := range vList {
					if vm, ok := item.(map[string]any); ok {
						if idStr, ok := vm["id"].(string); ok && idStr == vID {
							voiceExists = true
							break
						}
						if nameStr, ok := vm["name"].(string); ok && nameStr == vID {
							voiceExists = true
							break
						}
					}
				}
			}
		}

		if !voiceExists {
			return "", fmt.Errorf("%w: requested VieNeu voice %q not defined in catalog %s",
				ErrTTSVoiceAssetMissing, vID, catalogEntry.RelativePath)
		}

		// Require fixed in-root verified MOSS tokenizer layout declared in manifest
		var mossEntry *SnapshotFileEntry
		for i, f := range manifest.Files {
			norm := NormalizeRelativePath(f.RelativePath)
			if strings.HasPrefix(norm, "moss_tokenizer/") || norm == "moss_tokenizer" {
				mossEntry = &manifest.Files[i]
				break
			}
		}
		if mossEntry == nil {
			return "", fmt.Errorf("%w: fixed in-root MOSS tokenizer (moss_tokenizer/) not declared in snapshot manifest for %s",
				ErrSnapshotFileCorrupted, manifest.ModelID)
		}
		fullMossPath := filepath.Join(cleanRoot, "moss_tokenizer")
		if _, err := os.Stat(fullMossPath); err != nil {
			return "", fmt.Errorf("%w: fixed in-root MOSS tokenizer path inaccessible (%s)", ErrSnapshotFileCorrupted, fullMossPath)
		}

		return fullCatalogPath, nil
	}

	return cleanRoot, nil
}

// Frozen RC identities and digests for audio stem separation (Issue #69)
const (
	PinnedUVRModelID           = "UVR-MDX-NET-Inst_HQ_4.onnx"
	PinnedUVRModelVersion      = "v3"
	PinnedUVRArtifactSHA256    = "3c4b5b9b05090fdf238f38ba5046813982d50e2a652e9cb3324ea79720c3c9c8"
	PinnedUVRSourceRevision    = "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab"
	PinnedUVRPackageVersion    = "0.47.0"
	PinnedUVRMetadataFilename  = "mdx_model_data.json"
	PinnedDemucsModelID        = "htdemucs"
	PinnedDemucsModelVersion   = "v4"
	PinnedDemucsCheckpointSHA  = "8726e21a993978c7ba086d3872e7608d7d5bfca646ca4aca459ffda844faa8b4"
	PinnedDemucsSignature      = "955717e8"
	PinnedDemucsBagYAML        = "htdemucs.yaml"
	PinnedDemucsBagYAMLSHA256  = "239c445d0b14454d541ad8bd9bb271c9e536d267e8a4625208744cbb2e7bb66c"
	PinnedDemucsSourceRevision = "e976d93ecc3865e5757426930257e200846a520a"
	PinnedDemucsPackageVersion = "4.1.0a2"
)

// NewUVRRuntimeIdentity constructs the authoritative RuntimeIdentity evidence for UVR.
func NewUVRRuntimeIdentity(primarySnapSHA string, depSHAs []string) RuntimeIdentity {
	rt := RuntimeIdentity{
		AdapterRevision: "cmd/stageworker/adapters/separator.py@v0.47.0",
		SourceRevision:  PinnedUVRSourceRevision,
		RuntimeVersions: map[string]string{
			"audio-separator": PinnedUVRPackageVersion,
		},
		PrimarySnapshotSHA256:  primarySnapSHA,
		DependencySnapshotSHAs: depSHAs,
	}
	rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
	return rt
}

// NewDemucsRuntimeIdentity constructs the authoritative RuntimeIdentity evidence for Demucs.
func NewDemucsRuntimeIdentity(primarySnapSHA string, depSHAs []string) RuntimeIdentity {
	rt := RuntimeIdentity{
		AdapterRevision: "cmd/stageworker/adapters/separator.py@v4.1.0a2",
		SourceRevision:  PinnedDemucsSourceRevision,
		RuntimeVersions: map[string]string{
			"demucs": PinnedDemucsPackageVersion,
		},
		PrimarySnapshotSHA256:  primarySnapSHA,
		DependencySnapshotSHAs: depSHAs,
	}
	rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
	return rt
}

// ResolveSeparatorEntrypoint validates that the requested separator model asset is part
// of the verified model snapshot manifest and accessible on disk, failing closed if missing,
// mismatched, or unverified.
// For UVR primary, it verifies the exact UVR-MDX-NET-Inst_HQ_4.onnx artifact bytes against
// the frozen RC SHA-256 digest.
// For Demucs fallback, it verifies the exact htdemucs single checkpoint 955717e8-8726e21a.th
// against the frozen RC SHA-256 digest, and explicitly rejects htdemucs_ft or its 4-checkpoint bag.
func ResolveSeparatorEntrypoint(manifest SnapshotManifest, snapshotRoot, modelName string) (string, error) {
	cleanRoot := strings.TrimSpace(snapshotRoot)
	if cleanRoot == "" {
		return "", fmt.Errorf("%w: snapshot root is empty", ErrSnapshotFileCorrupted)
	}

	lowerModel := strings.ToLower(strings.TrimSpace(modelName))
	lowerManifestID := strings.ToLower(strings.TrimSpace(manifest.ModelID))

	if strings.Contains(lowerModel, "uvr") || strings.Contains(lowerModel, "mdx") {
		uvrEntry := findManifestFile(manifest, func(norm string) bool {
			return norm == strings.ToLower(PinnedUVRModelID) || strings.HasSuffix(norm, "/"+strings.ToLower(PinnedUVRModelID))
		})
		if uvrEntry == nil {
			return "", fmt.Errorf("%w: UVR-MDX-NET-Inst_HQ_4.onnx not declared in snapshot manifest for %s",
				ErrSeparatorModelAssetMissing, manifest.ModelID)
		}
		if !strings.EqualFold(uvrEntry.SHA256, PinnedUVRArtifactSHA256) {
			return "", fmt.Errorf("%w: UVR artifact %s SHA-256 mismatch: expected %s, got %s",
				ErrSnapshotDigestMismatch, uvrEntry.RelativePath, PinnedUVRArtifactSHA256, uvrEntry.SHA256)
		}
		return verifySnapshotRegularFile(cleanRoot, uvrEntry, "UVR artifact")
	}

	if strings.Contains(lowerModel, "demucs") {
		// Explicitly reject htdemucs_ft bag
		if strings.Contains(lowerModel, "htdemucs_ft") || strings.Contains(lowerManifestID, "htdemucs_ft") {
			return "", fmt.Errorf("%w: htdemucs_ft bag is rejected as htdemucs fallback; exact htdemucs required",
				ErrSeparatorModelAssetMissing)
		}

		// Reject if manifest declares any of the htdemucs_ft 4-checkpoint models
		ftSignatures := []string{"f7e0c4bc", "d12395a8", "92cfc3b6", "04573f0d", "htdemucs_ft.yaml"}
		for _, f := range manifest.Files {
			norm := strings.ToLower(NormalizeRelativePath(f.RelativePath))
			for _, sig := range ftSignatures {
				if strings.Contains(norm, sig) {
					return "", fmt.Errorf("%w: four-checkpoint htdemucs_ft bag is rejected as htdemucs fallback (found %s)",
						ErrSeparatorModelAssetMissing, f.RelativePath)
				}
			}
		}

		demucsEntry := findManifestFile(manifest, func(norm string) bool {
			return strings.Contains(norm, "955717e8-8726e21a.th") || strings.Contains(norm, PinnedDemucsSignature)
		})
		if demucsEntry == nil {
			return "", fmt.Errorf("%w: canonical htdemucs checkpoint (955717e8-8726e21a.th) not declared in snapshot manifest for %s",
				ErrSeparatorModelAssetMissing, manifest.ModelID)
		}
		if !strings.EqualFold(demucsEntry.SHA256, PinnedDemucsCheckpointSHA) {
			return "", fmt.Errorf("%w: Demucs checkpoint %s SHA-256 mismatch: expected %s, got %s",
				ErrSnapshotDigestMismatch, demucsEntry.RelativePath, PinnedDemucsCheckpointSHA, demucsEntry.SHA256)
		}
		fullPath, err := verifySnapshotRegularFile(cleanRoot, demucsEntry, "Demucs checkpoint file")
		if err != nil {
			return "", err
		}
		// Validate canonical htdemucs.yaml bag definition if declared in manifest
		yamlEntry := findManifestFile(manifest, func(norm string) bool {
			return norm == strings.ToLower(PinnedDemucsBagYAML) || strings.HasSuffix(norm, "/"+strings.ToLower(PinnedDemucsBagYAML))
		})
		if yamlEntry != nil {
			if !strings.EqualFold(yamlEntry.SHA256, PinnedDemucsBagYAMLSHA256) {
				return "", fmt.Errorf("%w: Demucs bag YAML %s SHA-256 mismatch: expected %s, got %s",
					ErrSnapshotDigestMismatch, yamlEntry.RelativePath, PinnedDemucsBagYAMLSHA256, yamlEntry.SHA256)
			}
			if _, err := verifySnapshotRegularFile(cleanRoot, yamlEntry, "Demucs bag YAML"); err != nil {
				return "", err
			}
		}

		return fullPath, nil
	}

	return "", fmt.Errorf("%w: unknown or unverified separator model %q (must be UVR-MDX-NET-Inst_HQ_4.onnx or htdemucs)",
		ErrSeparatorModelAssetMissing, modelName)
}

// ResolveSeparatorMetadataPath resolves and validates the model metadata asset (e.g. mdx_model_data.json)
// required by UVR separation, failing closed if missing or not accessible on disk.
func ResolveSeparatorMetadataPath(manifest SnapshotManifest, snapshotRoot string) (string, error) {
	cleanRoot := strings.TrimSpace(snapshotRoot)
	if cleanRoot == "" {
		return "", fmt.Errorf("%w: snapshot root is empty", ErrSnapshotFileCorrupted)
	}
	metaEntry := findManifestFile(manifest, func(norm string) bool {
		return norm == strings.ToLower(PinnedUVRMetadataFilename) || strings.HasSuffix(norm, "/"+strings.ToLower(PinnedUVRMetadataFilename)) ||
			norm == "vr_model_data.json" || strings.HasSuffix(norm, "/vr_model_data.json")
	})
	if metaEntry == nil {
		return "", fmt.Errorf("%w: UVR model metadata (%s) not declared in snapshot manifest for %s",
			ErrSeparatorModelAssetMissing, PinnedUVRMetadataFilename, manifest.ModelID)
	}
	return verifySnapshotRegularFile(cleanRoot, metaEntry, "UVR model metadata")
}

// Frozen RC identities and digests for audio role analysis (Issue #80)
const (
	PinnedYAMNetModelID         = "yamnet"
	PinnedYAMNetModelVersion    = "v1"
	PinnedYAMNetArtifactSHA256  = "10c95ea3eb9a7bb4cb8bddf6feb023250381008177ac162ce169694d05c317de"
	PinnedYAMNetClassMapSHA256  = "cdf24d193e196d9e95912a2667051ae203e92a2ba09449218ccb40ef787c6df2"
	PinnedYAMNetManifestSHA256  = "305743f2153ec1250ed1149fcb5acbfe1944d548630f9b23c6c53655dea79943"
	PinnedYAMNetPackageVersion  = "2.2.0"
	PinnedYAMNetAdapterRevision = "cmd/stageworker/adapters/audio_role_yamnet.py@v2.2.0"
)

// NewYAMNetRuntimeIdentity constructs the authoritative RuntimeIdentity evidence for YAMNet.
func NewYAMNetRuntimeIdentity(primarySnapSHA string, depSHAs []string) RuntimeIdentity {
	rt := RuntimeIdentity{
		AdapterRevision: PinnedYAMNetAdapterRevision,
		SourceRevision:  "google/yamnet@v1",
		RuntimeVersions: map[string]string{
			"ai-edge-litert": PinnedYAMNetPackageVersion,
		},
		PrimarySnapshotSHA256:  primarySnapSHA,
		DependencySnapshotSHAs: depSHAs,
	}
	rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
	return rt
}

// ResolveAudioRoleEntrypoint validates that the requested audio role model asset (yamnet.tflite)
// is part of the verified model snapshot manifest and accessible on disk, failing closed if missing or unverified.
func ResolveAudioRoleEntrypoint(manifest SnapshotManifest, snapshotRoot, modelName string) (string, error) {
	cleanRoot := strings.TrimSpace(snapshotRoot)
	if cleanRoot == "" {
		return "", fmt.Errorf("%w: empty snapshotRoot", ErrSnapshotFileCorrupted)
	}

	lowerModel := strings.ToLower(strings.TrimSpace(modelName))
	if !strings.Contains(lowerModel, "yamnet") && !strings.Contains(lowerModel, "audio_role") {
		return "", fmt.Errorf("%w: unrecognized audio role model %s", ErrAudioRoleModelAssetMissing, modelName)
	}

	var tfliteEntry *SnapshotFileEntry
	for i, f := range manifest.Files {
		norm := strings.ToLower(NormalizeRelativePath(f.RelativePath))
		if strings.HasSuffix(norm, "yamnet.tflite") || norm == "yamnet.tflite" {
			tfliteEntry = &manifest.Files[i]
			break
		}
	}
	if tfliteEntry == nil {
		return "", fmt.Errorf("%w: yamnet.tflite not declared in snapshot manifest for %s",
			ErrAudioRoleModelAssetMissing, modelName)
	}
	if !strings.EqualFold(tfliteEntry.SHA256, PinnedYAMNetArtifactSHA256) {
		return "", fmt.Errorf("%w: YAMNet artifact %s SHA-256 mismatch: expected %s, got %s",
			ErrSnapshotDigestMismatch, tfliteEntry.RelativePath, PinnedYAMNetArtifactSHA256, tfliteEntry.SHA256)
	}
	return verifySnapshotRegularFile(cleanRoot, tfliteEntry, "yamnet.tflite")
}

// ResolveAudioRoleClassMapPath validates and returns the class map CSV file declared in the snapshot manifest.
func ResolveAudioRoleClassMapPath(manifest SnapshotManifest, snapshotRoot string) (string, error) {
	cleanRoot := strings.TrimSpace(snapshotRoot)
	if cleanRoot == "" {
		return "", fmt.Errorf("%w: empty snapshotRoot", ErrSnapshotFileCorrupted)
	}
	var classMapEntry *SnapshotFileEntry
	for i, f := range manifest.Files {
		norm := strings.ToLower(NormalizeRelativePath(f.RelativePath))
		if strings.HasSuffix(norm, "yamnet_class_map.csv") || norm == "yamnet_class_map.csv" {
			classMapEntry = &manifest.Files[i]
			break
		}
	}
	if classMapEntry == nil {
		return "", fmt.Errorf("%w: yamnet_class_map.csv not declared in snapshot manifest", ErrAudioRoleModelAssetMissing)
	}
	if !strings.EqualFold(classMapEntry.SHA256, PinnedYAMNetClassMapSHA256) {
		return "", fmt.Errorf("%w: YAMNet class map %s SHA-256 mismatch: expected %s, got %s",
			ErrSnapshotDigestMismatch, classMapEntry.RelativePath, PinnedYAMNetClassMapSHA256, classMapEntry.SHA256)
	}
	return verifySnapshotRegularFile(cleanRoot, classMapEntry, "yamnet_class_map.csv")
}
