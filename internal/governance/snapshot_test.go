package governance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/storage"
)

func createTestFile(t *testing.T, dir, relPath, content string) (string, int64) {
	t.Helper()
	fullPath := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("write file %s: %v", relPath, err)
	}
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:]), int64(len(content))
}

func setupTestSnapshotService(t *testing.T) (*storage.DB, *governance.SnapshotService, *governance.LicenseService) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "snap_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	return db, snapSvc, licSvc
}

func TestSnapshot_CanonicalManifestDeterminism(t *testing.T) {
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	shaC := strings.Repeat("c", 64)

	manifest1 := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "b/config.json", SHA256: shaB, SizeBytes: 20},
			{RelativePath: "a/weights.bin", SHA256: shaA, SizeBytes: 100},
			{RelativePath: "vocab.txt", SHA256: shaC, SizeBytes: 50},
		},
	}

	// Manifest 2 has reverse order
	manifest2 := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "vocab.txt", SHA256: shaC, SizeBytes: 50},
			{RelativePath: "a/weights.bin", SHA256: shaA, SizeBytes: 100},
			{RelativePath: "b/config.json", SHA256: shaB, SizeBytes: 20},
		},
	}

	sha1, err := domain.ComputeSnapshotManifestSHA256(&manifest1)
	if err != nil {
		t.Fatalf("compute sha1: %v", err)
	}

	sha2, err := domain.ComputeSnapshotManifestSHA256(&manifest2)
	if err != nil {
		t.Fatalf("compute sha2: %v", err)
	}

	if sha1 != sha2 {
		t.Fatalf("expected deterministic canonical hashes to match: %s != %s", sha1, sha2)
	}
}

func TestSnapshot_VerificationLifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "snap_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// Create test files in a temp directory
	snapshotDir := t.TempDir()
	hash1, size1 := createTestFile(t, snapshotDir, "weights.bin", "sample model weights binary data")
	hash2, size2 := createTestFile(t, snapshotDir, "config.json", `{"model_type": "transformer"}`)
	hash3, size3 := createTestFile(t, snapshotDir, "vocab.txt", "token1\ntoken2\ntoken3")

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "qwen3-asr",
		ModelVersion:  "1.7b",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "weights.bin", SHA256: hash1, SizeBytes: size1},
			{RelativePath: "config.json", SHA256: hash2, SizeBytes: size2},
			{RelativePath: "vocab.txt", SHA256: hash3, SizeBytes: size3},
		},
	}

	// 1. Fail closed when license manifest is missing (policy-before-health)
	_, err = snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapshotDir)
	if !errors.Is(err, domain.ErrLicenseManifestMissing) {
		t.Fatalf("expected ErrLicenseManifestMissing, got %v", err)
	}

	// Compute manifest digest
	manifestSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("compute manifest sha: %v", err)
	}

	// Register 4-layer license manifest
	err = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3-asr",
		Version:        "1.7b",
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Qwen-Research",
		DataLicense:    "Mixed-Public",
		ServiceTerms:   "Self-Hosted",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("register license manifest: %v", err)
	}

	// 2. Digest mismatch fail closed
	badManifest := manifest
	badManifest.SnapshotManifestSHA256 = "00112233445566778899aabbccddeeff"
	_, err = snapSvc.RegisterAndVerifySnapshot(ctx, badManifest, snapshotDir)
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch, got %v", err)
	}

	// 3. Successful verification and in-process binding creation
	manifest.SnapshotManifestSHA256 = manifestSHA
	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapshotDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot failed: %v", err)
	}
	if !binding.Active {
		t.Fatalf("expected binding to be active")
	}
	if binding.FileCount != 3 {
		t.Fatalf("expected 3 files, got %d", binding.FileCount)
	}

	// 4. Check fingerprint validation before invocation
	if err := snapSvc.CheckBindingFingerprints("qwen3-asr", "1.7b"); err != nil {
		t.Fatalf("CheckBindingFingerprints failed: %v", err)
	}

	// 5. Portable evidence does not export local paths or machine secrets
	evidence, err := snapSvc.GetPortableEvidence(ctx, "qwen3-asr", "1.7b")
	if err != nil {
		t.Fatalf("GetPortableEvidence failed: %v", err)
	}
	if evidence.SnapshotManifestSHA256 != manifestSHA {
		t.Fatalf("evidence sha mismatch: got %s, want %s", evidence.SnapshotManifestSHA256, manifestSHA)
	}
	if len(evidence.Files) != 3 {
		t.Fatalf("expected 3 files in evidence, got %d", len(evidence.Files))
	}
	for _, f := range evidence.Files {
		if filepath.IsAbs(f.RelativePath) {
			t.Fatalf("portable evidence must not contain absolute paths: %s", f.RelativePath)
		}
	}

	// 6. Audit records in SQLite exist
	events, err := db.ListSnapshotVerificationEvents(ctx, "qwen3-asr")
	if err != nil {
		t.Fatalf("ListSnapshotVerificationEvents failed: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("expected verification events recorded in SQLite")
	}
	foundVerified := false
	for _, ev := range events {
		if ev.Outcome == "VERIFIED" && ev.SnapshotManifestSHA256 == manifestSHA {
			foundVerified = true
			break
		}
	}
	if !foundVerified {
		t.Fatalf("no VERIFIED event found in audit records: %+v", events)
	}

	// 7. Mutation detection: modify a file on disk
	time.Sleep(10 * time.Millisecond) // Ensure mtime changes
	weightsPath := filepath.Join(snapshotDir, "weights.bin")
	if err := os.WriteFile(weightsPath, []byte("tampered weights data!"), 0644); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}

	// Pre-invocation check must detect mutation, revoke binding, and fail closed
	err = snapSvc.CheckBindingFingerprints("qwen3-asr", "1.7b")
	if !errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
		t.Fatalf("expected ErrSnapshotMutatedRehashRequired, got %v", err)
	}

	// Subsequent GetBinding fails closed
	_, err = snapSvc.GetBinding("qwen3-asr", "1.7b")
	if !errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
		t.Fatalf("expected ErrSnapshotMutatedRehashRequired on GetBinding, got %v", err)
	}

	// 8a. Rehash/reverification of mutated file with different size fails with ErrSnapshotFileCorrupted
	_, err = snapSvc.Reverify(ctx, "qwen3-asr", "1.7b")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected reverify on size-corrupted file to fail with ErrSnapshotFileCorrupted, got %v", err)
	}

	// 8b. Mutated file with same size but different content fails with ErrSnapshotDigestMismatch
	sameSizeTampered := make([]byte, size1)
	copy(sameSizeTampered, []byte("tampered data same size 32 bytes"))
	if err := os.WriteFile(weightsPath, sameSizeTampered, 0644); err != nil {
		t.Fatalf("write same-size tampered file: %v", err)
	}
	_, err = snapSvc.Reverify(ctx, "qwen3-asr", "1.7b")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected reverify on hash-mismatched file to fail with ErrSnapshotDigestMismatch, got %v", err)
	}
	// Restore original file and reverify succeeds
	if err := os.WriteFile(weightsPath, []byte("sample model weights binary data"), 0644); err != nil {
		t.Fatalf("restore file: %v", err)
	}
	restoredBinding, err := snapSvc.Reverify(ctx, "qwen3-asr", "1.7b")
	if err != nil {
		t.Fatalf("reverify on restored file failed: %v", err)
	}
	if !restoredBinding.Active {
		t.Fatalf("expected restored binding to be active")
	}
}

func TestSnapshot_RejectPathTraversalAndEscapingRoot(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	_ = db
	dir := t.TempDir()

	hash1, size1 := createTestFile(t, dir, "valid.bin", "valid content")
	licEntry := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "traversal-model",
		Version:        "v1",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
	}
	_ = licSvc.RegisterManifest(ctx, licEntry)

	// Test traversal relative paths
	badPaths := []string{
		"../secret.txt",
		"../../etc/passwd",
		"foo/../../bar.bin",
		"/etc/passwd",
		"C:\\Windows\\System32",
	}
	for _, bad := range badPaths {
		manifest := domain.SnapshotManifest{
			SchemaVersion: "1.0",
			ModelID:       "traversal-model",
			ModelVersion:  "v1",
			Files: []domain.SnapshotFileEntry{
				{RelativePath: "valid.bin", SHA256: hash1, SizeBytes: size1},
				{RelativePath: bad, SHA256: hash1, SizeBytes: size1},
			},
		}
		_, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
		if err == nil {
			t.Errorf("expected bad path %q to be rejected, but verification succeeded", bad)
		}
		if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
			t.Errorf("expected ErrSnapshotFileCorrupted for bad path %q, got: %v", bad, err)
		}
	}
}

func TestSnapshot_RejectSymlinkIndirection(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	_ = db
	dir := t.TempDir()

	// Create real file outside or inside
	targetPath := filepath.Join(dir, "target.bin")
	content := "real file content"
	_ = os.WriteFile(targetPath, []byte(content), 0644)
	h := sha256.Sum256([]byte(content))
	targetHash := hex.EncodeToString(h[:])

	// Create symlink
	symlinkPath := filepath.Join(dir, "link.bin")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "symlink-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "link.bin", SHA256: targetHash, SizeBytes: int64(len(content))},
		},
	}
	manifestSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "symlink-model",
		Version:        "v1",
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
	})

	// Verification must reject symlink indirection fail-closed
	_, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
	if err == nil {
		t.Fatal("expected symlink indirection to be rejected fail-closed, got success")
	}
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted on symlink, got: %v", err)
	}
}

func TestSnapshot_RejectDirectorySymlink(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	_ = db
	dir := t.TempDir()

	// Create target directory and file
	targetDir := filepath.Join(dir, "real_dir")
	_ = os.MkdirAll(targetDir, 0755)
	targetFile := filepath.Join(targetDir, "file.bin")
	content := "deep content"
	_ = os.WriteFile(targetFile, []byte(content), 0644)
	h := sha256.Sum256([]byte(content))
	fileHash := hex.EncodeToString(h[:])

	// Create directory symlink
	symDir := filepath.Join(dir, "sym_dir")
	if err := os.Symlink(targetDir, symDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "symdir-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "sym_dir/file.bin", SHA256: fileHash, SizeBytes: int64(len(content))},
		},
	}
	manifestSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "symdir-model",
		Version:        "v1",
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
	})

	// Verification must reject directory symlink
	_, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
	if err == nil {
		t.Fatal("expected directory symlink to be rejected fail-closed, got success")
	}
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted, got: %v", err)
	}
}

func TestSnapshot_CheckBindingFingerprints_RejectSymlinkReplacement(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	_ = db
	dir := t.TempDir()

	// Create valid regular file
	origPath := filepath.Join(dir, "model.bin")
	content := "regular model content"
	_ = os.WriteFile(origPath, []byte(content), 0644)
	h := sha256.Sum256([]byte(content))
	fileHash := hex.EncodeToString(h[:])

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "tamper-symlink",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: fileHash, SizeBytes: int64(len(content))},
		},
	}
	manifestSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "tamper-symlink",
		Version:        "v1",
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
	})

	// 1. Initial verification passes
	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
	if err != nil {
		t.Fatalf("initial verify failed: %v", err)
	}
	if !binding.Active {
		t.Fatal("expected binding to be active")
	}

	// 2. Replace regular file with symlink
	outsideFile := filepath.Join(t.TempDir(), "outside.bin")
	_ = os.WriteFile(outsideFile, []byte(content), 0644)
	_ = os.Remove(origPath)
	if err := os.Symlink(outsideFile, origPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	// 3. CheckBindingFingerprints must detect symlink and revoke binding
	err = snapSvc.CheckBindingFingerprints("tamper-symlink", "v1")
	if err == nil {
		t.Fatal("expected symlink replacement to fail fingerprint check")
	}
	if !errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
		t.Fatalf("expected ErrSnapshotMutatedRehashRequired, got: %v", err)
	}
}

func TestSnapshot_RegisterAndVerifySnapshot_DoesNotFabricateRuntimeEvidence(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	defer db.Close()

	dir := t.TempDir()
	content := "fake_uvr_weights"
	_ = os.WriteFile(filepath.Join(dir, "UVR-MDX-NET-Inst_HQ_4.onnx"), []byte(content), 0644)
	h := sha256.Sum256([]byte(content))

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "UVR-MDX-NET-Inst_HQ_4.onnx",
		ModelVersion:  "v3",
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx",
				SHA256:       hex.EncodeToString(h[:]),
				SizeBytes:    int64(len(content)),
			},
		},
	}
	cSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = cSHA
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: manifest.ModelID,
		Version:        manifest.ModelVersion,
		SHA256:         cSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	// Requirement 1: Successful snapshot verification alone leaves runtime identity UNSET
	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot failed: %v", err)
	}
	if binding.RuntimeIdentity != nil {
		t.Fatalf("expected RuntimeIdentity to be nil after RegisterAndVerifySnapshot, got: %+v", binding.RuntimeIdentity)
	}
}

func TestSnapshot_SetRuntimeIdentity_HardenValidation(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	defer db.Close()

	dir := t.TempDir()
	content := "fake_uvr_weights"
	_ = os.WriteFile(filepath.Join(dir, "UVR-MDX-NET-Inst_HQ_4.onnx"), []byte(content), 0644)
	h := sha256.Sum256([]byte(content))

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "UVR-MDX-NET-Inst_HQ_4.onnx",
		ModelVersion:  "v3",
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx",
				SHA256:       hex.EncodeToString(h[:]),
				SizeBytes:    int64(len(content)),
			},
		},
	}
	cSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = cSHA
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: manifest.ModelID,
		Version:        manifest.ModelVersion,
		SHA256:         cSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot failed: %v", err)
	}

	// 1. Rejects wrong primary snapshot digest
	badSnapRT := domain.NewUVRRuntimeIdentity("wrong_snap_digest_sha256", nil)
	err = snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, badSnapRT)
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for wrong primary snapshot digest, got: %v", err)
	}

	// 2. Rejects inconsistent RuntimeManifestSHA256
	badManifestRT := domain.NewUVRRuntimeIdentity(binding.SnapshotManifestSHA256, nil)
	badManifestRT.RuntimeManifestSHA256 = strings.Repeat("f", 64)
	err = snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, badManifestRT)
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for wrong runtime manifest SHA, got: %v", err)
	}

	// 3. Rejects wrong source revision for UVR
	badSourceRT := domain.NewUVRRuntimeIdentity(binding.SnapshotManifestSHA256, nil)
	badSourceRT.SourceRevision = "unverified_commit_hash"
	badSourceRT.RuntimeManifestSHA256 = badSourceRT.ComputeRuntimeManifestSHA256()
	err = snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, badSourceRT)
	if !errors.Is(err, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected ErrSnapshotUnverified for wrong UVR source revision, got: %v", err)
	}

	// 4. Rejects wrong package version for UVR
	badPkgRT := domain.NewUVRRuntimeIdentity(binding.SnapshotManifestSHA256, nil)
	badPkgRT.RuntimeVersions["audio-separator"] = "9.9.9"
	badPkgRT.RuntimeManifestSHA256 = badPkgRT.ComputeRuntimeManifestSHA256()
	err = snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, badPkgRT)
	if !errors.Is(err, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected ErrSnapshotUnverified for wrong UVR package version, got: %v", err)
	}

	// 5. Explicitly validated runtime evidence succeeds
	validRT := domain.NewUVRRuntimeIdentity(binding.SnapshotManifestSHA256, nil)
	err = snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, validRT)
	if err != nil {
		t.Fatalf("expected SetRuntimeIdentity to succeed with valid evidence, got: %v", err)
	}
	if binding.RuntimeIdentity == nil {
		t.Fatal("expected binding to have attached RuntimeIdentity")
	}
	if binding.RuntimeIdentity.SourceRevision != domain.PinnedUVRSourceRevision {
		t.Fatalf("expected %s, got %s", domain.PinnedUVRSourceRevision, binding.RuntimeIdentity.SourceRevision)
	}
}

func TestSnapshot_SetRuntimeIdentity_ZeroTTSExactPins(t *testing.T) {
	ctx := context.Background()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	defer db.Close()

	dir := t.TempDir()
	content := []byte("zerotts-snapshot")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), content, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       domain.PinnedZeroTTSModelID,
		ModelVersion:  domain.PinnedZeroTTSModelVersion,
		Files:         []domain.SnapshotFileEntry{{RelativePath: "config.json", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content))}},
	}
	manifestSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = manifestSHA
	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID: uuid.NewString(), DependencyName: manifest.ModelID, Version: manifest.ModelVersion,
		SHA256: manifestSHA, CodeLicense: "Apache-2.0", ModelLicense: "Apache-2.0",
		DataLicense: "Unknown", ServiceTerms: "Local-Offline", Verified: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, dir)
	if err != nil {
		t.Fatal(err)
	}
	newRuntimeIdentity := func() domain.RuntimeIdentity {
		rt := domain.RuntimeIdentity{
			AdapterRevision:       domain.PinnedZeroTTSAdapterRevision,
			SourceRevision:        domain.PinnedZeroTTSSourceRevision,
			RuntimeVersions:       map[string]string{"zerotts": domain.PinnedZeroTTSPackageVersion},
			PrimarySnapshotSHA256: binding.SnapshotManifestSHA256,
		}
		rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
		return rt
	}

	badPackage := newRuntimeIdentity()
	badPackage.RuntimeVersions["zerotts"] = "0.1.3"
	badPackage.RuntimeManifestSHA256 = badPackage.ComputeRuntimeManifestSHA256()
	if err := snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, badPackage); !errors.Is(err, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected wrong ZeroTTS package version to fail closed, got %v", err)
	}

	badSource := newRuntimeIdentity()
	badSource.SourceRevision = "wrong"
	badSource.RuntimeManifestSHA256 = badSource.ComputeRuntimeManifestSHA256()
	if err := snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, badSource); !errors.Is(err, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected wrong ZeroTTS source revision to fail closed, got %v", err)
	}

	valid := newRuntimeIdentity()
	if err := snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, valid); err != nil {
		t.Fatalf("expected exact ZeroTTS runtime identity to pass: %v", err)
	}
	if binding.RuntimeIdentity == nil || binding.RuntimeIdentity.RuntimeVersions["zerotts"] != domain.PinnedZeroTTSPackageVersion {
		t.Fatalf("ZeroTTS runtime identity not attached correctly: %+v", binding.RuntimeIdentity)
	}
}

func TestSnapshot_SetRuntimeIdentity_ExactPackageVersion_SuffixRejection(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, snapSvc, licSvc := setupTestSnapshotService(t)
	defer db.Close()

	uvrDir := filepath.Join(tmpDir, "uvr")
	_ = os.MkdirAll(uvrDir, 0755)
	content := "fake_uvr_weights"
	_ = os.WriteFile(filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx"), []byte(content), 0644)
	h := sha256.Sum256([]byte(content))

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "UVR-MDX-NET-Inst_HQ_4.onnx",
		ModelVersion:  "v3",
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx",
				SHA256:       hex.EncodeToString(h[:]),
				SizeBytes:    int64(len(content)),
			},
		},
	}
	cSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = cSHA
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: manifest.ModelID,
		Version:        manifest.ModelVersion,
		SHA256:         cSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, uvrDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot: %v", err)
	}

	// Suffixes such as +modified or .post1 must fail closed
	for _, badVer := range []string{"0.47.0+modified", "0.47.0.post1", "0.47.0-beta"} {
		rt := domain.NewUVRRuntimeIdentity(binding.SnapshotManifestSHA256, nil)
		rt.RuntimeVersions["audio-separator"] = badVer
		rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
		err = snapSvc.SetRuntimeIdentity(manifest.ModelID, manifest.ModelVersion, rt)
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified for UVR suffix version %q, got: %v", badVer, err)
		}
	}

	// Test Demucs suffix rejection
	demucsDir := filepath.Join(tmpDir, "demucs")
	_ = os.MkdirAll(demucsDir, 0755)
	demucsContent := "demucs checkpoint bytes"
	dh := sha256.Sum256([]byte(demucsContent))
	_ = os.WriteFile(filepath.Join(demucsDir, "955717e8-8726e21a.th"), []byte(demucsContent), 0644)
	demucsManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "htdemucs",
		ModelVersion:  "v4",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "955717e8-8726e21a.th", SHA256: hex.EncodeToString(dh[:]), SizeBytes: int64(len(demucsContent))},
		},
	}
	dSHA, _ := domain.ComputeSnapshotManifestSHA256(&demucsManifest)
	demucsManifest.SnapshotManifestSHA256 = dSHA
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "htdemucs",
		Version:        "v4",
		SHA256:         dSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	demucsBinding, err := snapSvc.RegisterAndVerifySnapshot(ctx, demucsManifest, demucsDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot demucs: %v", err)
	}

	for _, badVer := range []string{"4.1.0a2.post1", "4.1.0a2+custom", "4.1.0a2-rc1"} {
		rt := domain.NewDemucsRuntimeIdentity(demucsBinding.SnapshotManifestSHA256, nil)
		rt.RuntimeVersions["demucs"] = badVer
		rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
		err = snapSvc.SetRuntimeIdentity(demucsManifest.ModelID, demucsManifest.ModelVersion, rt)
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified for Demucs suffix version %q, got: %v", badVer, err)
		}
	}
}
