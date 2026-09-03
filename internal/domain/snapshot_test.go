package domain_test

import (
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestSnapshot_ValidateRelativePath(t *testing.T) {
	// 1. Valid relative paths
	validPaths := []string{
		"model.bin",
		"weights/model.safetensors",
		"config.json",
		"tokenizer/vocab.json",
		"deeply/nested/dir/asset.bin",
	}
	for _, p := range validPaths {
		norm, err := domain.ValidateRelativePath(p)
		if err != nil {
			t.Errorf("expected valid path %q to pass, got: %v", p, err)
		}
		if norm == "" || strings.Contains(norm, "\\") {
			t.Errorf("expected normalized forward slashes for %q, got %q", p, norm)
		}
	}

	// 2. Reject empty or whitespace
	for _, p := range []string{"", "   ", "\t\n"} {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected empty/whitespace %q to be rejected", p)
		}
	}

	// 3. Reject dot paths
	dotPaths := []string{".", "./", ".\\", "foo/.", "foo/./bar"}
	for _, p := range dotPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected dot path %q to be rejected", p)
		}
	}

	// 4. Reject parent traversal (..)
	traversalPaths := []string{
		"..",
		"../foo",
		"../../foo/bar",
		"foo/..",
		"foo/../bar",
		"foo/bar/../../baz",
		"a/b/../../../c",
	}
	for _, p := range traversalPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected parent traversal %q to be rejected", p)
		}
	}

	// 5. Reject absolute paths
	absPaths := []string{
		"/etc/passwd",
		"/model.bin",
		"\\Windows\\System32",
		"\\foo\\bar",
	}
	for _, p := range absPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected absolute path %q to be rejected", p)
		}
	}

	// 6. Reject volume-qualified paths
	volPaths := []string{
		"C:foo",
		"C:\\foo",
		"C:/foo",
		"D:weights.bin",
		"\\\\server\\share\\model.bin",
		"z:/model.bin",
	}
	for _, p := range volPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected volume-qualified path %q to be rejected", p)
		}
	}
}

func TestSnapshot_ValidateSnapshotManifest_RejectDuplicatesAndInvalid(t *testing.T) {
	dummySHA := strings.Repeat("a", 64)

	// 1. Valid manifest passes
	validManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: 100},
			{RelativePath: "config.json", SHA256: dummySHA, SizeBytes: 200},
		},
	}
	if err := domain.ValidateSnapshotManifest(&validManifest); err != nil {
		t.Fatalf("expected valid manifest to pass, got: %v", err)
	}

	// 2. Duplicate normalized paths rejected
	dupManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: 100},
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&dupManifest); err == nil {
		t.Fatal("expected duplicate paths in manifest to be rejected")
	}

	// Duplicate with alternative slash/prefix
	dupManifest2 := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "subdir/weights.bin", SHA256: dummySHA, SizeBytes: 100},
			{RelativePath: "subdir\\weights.bin", SHA256: dummySHA, SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&dupManifest2); err == nil {
		t.Fatal("expected normalized duplicate paths to be rejected")
	}

	// 3. Traversal path in manifest rejected
	travManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "../secret.txt", SHA256: dummySHA, SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&travManifest); err == nil {
		t.Fatal("expected traversal in manifest to be rejected")
	}

	// 4. Invalid sha length rejected
	badSHAManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: "short_hash", SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&badSHAManifest); err == nil {
		t.Fatal("expected short sha256 to be rejected")
	}

	// 5. Negative size rejected
	negSizeManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: -5},
		},
	}
	if err := domain.ValidateSnapshotManifest(&negSizeManifest); err == nil {
		t.Fatal("expected negative size to be rejected")
	}
}
