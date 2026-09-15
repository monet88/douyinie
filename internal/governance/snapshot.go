package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// FileFingerprint captures file size and modification time for lightweight stat-based rechecks.
type FileFingerprint struct {
	SizeBytes int64
	ModTime   time.Time
}

// SnapshotVerificationBinding represents an in-process, runtime-only authorization binding
// asserting that snapshot bytes on disk have been verified.
type SnapshotVerificationBinding struct {
	ID                     string                     `json:"id"`
	DependencyName         string                     `json:"dependency_name"`
	Version                string                     `json:"version"`
	SnapshotManifestSHA256 string                     `json:"snapshot_manifest_sha256"`
	LocalPath              string                     `json:"local_path"`
	FileCount              int                        `json:"file_count"`
	TotalBytes             int64                      `json:"total_bytes"`
	VerifiedAt             time.Time                  `json:"verified_at"`
	Active                 bool                       `json:"active"`
	RevocationReason       string                     `json:"revocation_reason,omitempty"`
	Manifest               domain.SnapshotManifest    `json:"manifest"`
	Fingerprints           map[string]FileFingerprint `json:"-"`
	RuntimeIdentity        *domain.RuntimeIdentity    `json:"runtime_identity,omitempty"`
	VerificationEventID    string                     `json:"verification_event_id,omitempty"`
}

// SnapshotService manages in-process model snapshot verification, startup checks,
// file fingerprinting, and audit logging.
type SnapshotService struct {
	mu       sync.RWMutex
	db       *storage.DB
	licSvc   *LicenseService
	bindings map[string]*SnapshotVerificationBinding // key: makeKey(dep, version)
}

// NewSnapshotService creates a new SnapshotService.
func NewSnapshotService(db *storage.DB, licSvc *LicenseService) *SnapshotService {
	return &SnapshotService{
		db:       db,
		licSvc:   licSvc,
		bindings: make(map[string]*SnapshotVerificationBinding),
	}
}

func makeSnapshotKey(dep, ver string) string {
	dep = strings.TrimSpace(dep)
	ver = strings.TrimSpace(ver)
	if ver == "" {
		ver = "v1"
	}
	return dep + "@" + ver
}

// RegisterAndVerifySnapshot performs full digest verification for all manifest-declared files,
// checks license manifest registration, creates fresh in-process verification bindings,
// and records an immutable audit event in SQLite.
func (ss *SnapshotService) RegisterAndVerifySnapshot(ctx context.Context, manifest domain.SnapshotManifest, localPath string) (*SnapshotVerificationBinding, error) {
	manifest.ModelID = strings.TrimSpace(manifest.ModelID)
	if manifest.ModelID == "" {
		return nil, fmt.Errorf("manifest model_id is required")
	}
	if strings.TrimSpace(manifest.ModelVersion) == "" {
		manifest.ModelVersion = "v1"
	}

	// 1. Validate manifest contract (schema, non-empty files, path safety, no duplicates)
	if err := domain.ValidateSnapshotManifest(&manifest); err != nil {
		ss.recordEvent(ctx, manifest, "CORRUPTED", err.Error(), 0, 0)
		return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, err.Error())
	}

	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot local path: %w", err)
	}

	// Verify localPath root directory
	rootInfo, err := os.Stat(absPath)
	if err != nil {
		errMsg := fmt.Sprintf("snapshot root directory %s inaccessible: %v", absPath, err)
		ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
		return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
	}
	if !rootInfo.IsDir() {
		errMsg := fmt.Sprintf("snapshot root %s is not a directory", absPath)
		ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
		return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
	}
	rootLstat, err := os.Lstat(absPath)
	if err == nil && (rootLstat.Mode()&os.ModeSymlink != 0) {
		errMsg := fmt.Sprintf("snapshot root %s cannot be a symlink", absPath)
		ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
		return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
	}

	// 2. License manifest check (policy-before-health / 4 obligation layers must exist)
	var licEntry *domain.LicenseManifestEntry
	if ss.licSvc != nil {
		le, licErr := ss.licSvc.GetManifest(ctx, manifest.ModelID, manifest.ModelVersion)
		if licErr != nil {
			ss.recordEvent(ctx, manifest, "LICENSE_MISSING", licErr.Error(), 0, 0)
			return nil, fmt.Errorf("%w: %v", domain.ErrLicenseManifestMissing, licErr)
		}
		licEntry = le
	}

	// 3. Canonical manifest hashing
	computedManifestSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		ss.recordEvent(ctx, manifest, "MANIFEST_ERROR", err.Error(), 0, 0)
		return nil, fmt.Errorf("compute manifest digest: %w", err)
	}

	if manifest.SnapshotManifestSHA256 != "" {
		if !strings.EqualFold(manifest.SnapshotManifestSHA256, computedManifestSHA) {
			errMsg := fmt.Sprintf("manifest digest mismatch: declared %s != computed %s", manifest.SnapshotManifestSHA256, computedManifestSHA)
			ss.recordEvent(ctx, manifest, "DIGEST_MISMATCH", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotDigestMismatch, errMsg)
		}
	} else {
		manifest.SnapshotManifestSHA256 = computedManifestSHA
	}

	// Also verify against license manifest SHA256 if declared
	if licEntry != nil && licEntry.SHA256 != "" {
		if !strings.EqualFold(licEntry.SHA256, computedManifestSHA) {
			errMsg := fmt.Sprintf("snapshot manifest digest %s does not match license manifest SHA256 %s", computedManifestSHA, licEntry.SHA256)
			ss.recordEvent(ctx, manifest, "DIGEST_MISMATCH", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotDigestMismatch, errMsg)
		}
	}

	// 4. Full file digest verification on disk with strict boundary & anti-symlink enforcement
	fingerprints := make(map[string]FileFingerprint, len(manifest.Files))
	var totalBytes int64

	for _, fileEntry := range manifest.Files {
		normRelPath, err := domain.ValidateRelativePath(fileEntry.RelativePath)
		if err != nil {
			errMsg := fmt.Sprintf("invalid relative_path %s: %v", fileEntry.RelativePath, err)
			ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
		}

		fullFilePath := filepath.Join(absPath, filepath.FromSlash(normRelPath))

		// Root containment boundary check
		relCheck, err := filepath.Rel(absPath, fullFilePath)
		if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) || filepath.IsAbs(relCheck) {
			errMsg := fmt.Sprintf("path %s escapes snapshot root %s", normRelPath, absPath)
			ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
		}

		// Symlink and reparse-point rejection on every path component from absPath down to file
		curr := absPath
		components := strings.Split(filepath.FromSlash(normRelPath), string(filepath.Separator))
		for _, comp := range components {
			curr = filepath.Join(curr, comp)
			fi, lerr := os.Lstat(curr)
			if lerr != nil {
				errMsg := fmt.Sprintf("file %s missing: %v", normRelPath, lerr)
				ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
				return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				errMsg := fmt.Sprintf("symlink/reparse indirection rejected at %s (%s)", curr, normRelPath)
				ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
				return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
			}
		}

		st, statErr := os.Stat(fullFilePath)
		if statErr != nil {
			errMsg := fmt.Sprintf("file %s missing: %v", normRelPath, statErr)
			ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
		}

		if !st.Mode().IsRegular() {
			errMsg := fmt.Sprintf("file %s is not a regular file (mode: %v)", normRelPath, st.Mode())
			ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
		}

		if st.Size() != fileEntry.SizeBytes {
			errMsg := fmt.Sprintf("file %s size mismatch: got %d, want %d", normRelPath, st.Size(), fileEntry.SizeBytes)
			ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
		}

		// Compute file SHA256
		hashHex, hashErr := hashFile(fullFilePath)
		if hashErr != nil {
			errMsg := fmt.Sprintf("read file %s for hashing: %v", normRelPath, hashErr)
			ss.recordEvent(ctx, manifest, "CORRUPTED", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotFileCorrupted, errMsg)
		}

		if !strings.EqualFold(hashHex, fileEntry.SHA256) {
			errMsg := fmt.Sprintf("file %s hash mismatch: got %s, want %s", normRelPath, hashHex, fileEntry.SHA256)
			ss.recordEvent(ctx, manifest, "DIGEST_MISMATCH", errMsg, 0, 0)
			return nil, fmt.Errorf("%w: %s", domain.ErrSnapshotDigestMismatch, errMsg)
		}

		fingerprints[normRelPath] = FileFingerprint{
			SizeBytes: st.Size(),
			ModTime:   st.ModTime(),
		}
		totalBytes += st.Size()
	}

	// 4. Record successful audit event in SQLite
	eventID := uuid.NewString()
	now := time.Now().UTC()
	ss.recordEventWithID(ctx, eventID, manifest, "VERIFIED", "", len(manifest.Files), totalBytes, now)

	// 5. Create fresh in-process verification binding
	binding := &SnapshotVerificationBinding{
		ID:                     uuid.NewString(),
		DependencyName:         manifest.ModelID,
		Version:                manifest.ModelVersion,
		SnapshotManifestSHA256: computedManifestSHA,
		LocalPath:              absPath,
		FileCount:              len(manifest.Files),
		TotalBytes:             totalBytes,
		VerifiedAt:             now,
		Active:                 true,
		Manifest:               manifest,
		Fingerprints:           fingerprints,
		VerificationEventID:    eventID,
	}
	key := makeSnapshotKey(manifest.ModelID, manifest.ModelVersion)
	ss.mu.Lock()
	ss.bindings[key] = binding
	ss.mu.Unlock()

	return binding, nil
}

// CheckBindingFingerprints verifies that all manifest-declared files for an active binding
// have not mutated or disappeared since verification. If mutated, eligibility is revoked immediately.
func (ss *SnapshotService) CheckBindingFingerprints(dependencyName, version string) error {
	key := makeSnapshotKey(dependencyName, version)

	ss.mu.RLock()
	binding, ok := ss.bindings[key]
	ss.mu.RUnlock()

	if !ok || binding == nil {
		return fmt.Errorf("%w: no binding for %s (%s)", domain.ErrSnapshotUnverified, dependencyName, version)
	}

	if !binding.Active {
		return fmt.Errorf("%w: binding revoked (%s)", domain.ErrSnapshotMutatedRehashRequired, binding.RevocationReason)
	}

	// Re-check each declared file against fingerprint
	for relPath, fp := range binding.Fingerprints {
		fullPath := filepath.Join(binding.LocalPath, filepath.FromSlash(relPath))
		st, err := os.Lstat(fullPath)
		if err != nil {
			ss.mu.Lock()
			binding.Active = false
			binding.RevocationReason = fmt.Sprintf("file %s disappeared or inaccessible: %v", relPath, err)
			ss.mu.Unlock()
			return fmt.Errorf("%w: file %s missing: %v", domain.ErrSnapshotMutatedRehashRequired, relPath, err)
		}

		if st.Mode()&os.ModeSymlink != 0 {
			ss.mu.Lock()
			binding.Active = false
			binding.RevocationReason = fmt.Sprintf("file %s replaced with symlink", relPath)
			ss.mu.Unlock()
			return fmt.Errorf("%w: symlink indirection detected for %s", domain.ErrSnapshotMutatedRehashRequired, relPath)
		}

		if st.Size() != fp.SizeBytes || !st.ModTime().Equal(fp.ModTime) {
			ss.mu.Lock()
			binding.Active = false
			binding.RevocationReason = fmt.Sprintf("file %s modified (size %d->%d, mod %v->%v)", relPath, fp.SizeBytes, st.Size(), fp.ModTime, st.ModTime())
			ss.mu.Unlock()
			return fmt.Errorf("%w: file %s mutated on disk", domain.ErrSnapshotMutatedRehashRequired, relPath)
		}
	}

	return nil
}

// GetBinding returns the active verification binding for a model dependency and version.
func (ss *SnapshotService) GetBinding(dependencyName, version string) (*SnapshotVerificationBinding, error) {
	key := makeSnapshotKey(dependencyName, version)

	ss.mu.RLock()
	defer ss.mu.RUnlock()

	b, ok := ss.bindings[key]
	if !ok || b == nil {
		return nil, fmt.Errorf("%w: %s (%s)", domain.ErrSnapshotUnverified, dependencyName, version)
	}
	if !b.Active {
		return nil, fmt.Errorf("%w: %s (%s) revoked: %s", domain.ErrSnapshotMutatedRehashRequired, dependencyName, version, b.RevocationReason)
	}
	return b, nil
}

// ListBindings returns a list of all current in-process verification bindings.
func (ss *SnapshotService) ListBindings() []*SnapshotVerificationBinding {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	list := make([]*SnapshotVerificationBinding, 0, len(ss.bindings))
	for _, b := range ss.bindings {
		list = append(list, b)
	}
	return list
}

// HasBinding reports whether a snapshot binding (active or revoked) exists for a dependency and version.
func (ss *SnapshotService) HasBinding(dependencyName, version string) bool {
	key := makeSnapshotKey(dependencyName, version)
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	_, ok := ss.bindings[key]
	return ok
}

// RevokeBinding revokes an in-process verification binding.
func (ss *SnapshotService) RevokeBinding(dependencyName, version, reason string) {
	key := makeSnapshotKey(dependencyName, version)

	ss.mu.Lock()
	defer ss.mu.Unlock()

	if b, ok := ss.bindings[key]; ok && b != nil {
		b.Active = false
		b.RevocationReason = reason
	}
}

// Reverify performs full re-hashing and re-verification of a revoked or mutated snapshot binding.
func (ss *SnapshotService) Reverify(ctx context.Context, dependencyName, version string) (*SnapshotVerificationBinding, error) {
	key := makeSnapshotKey(dependencyName, version)

	ss.mu.RLock()
	b, ok := ss.bindings[key]
	ss.mu.RUnlock()

	if !ok || b == nil {
		return nil, fmt.Errorf("%w: no existing binding for %s (%s) to reverify", domain.ErrSnapshotUnverified, dependencyName, version)
	}

	return ss.RegisterAndVerifySnapshot(ctx, b.Manifest, b.LocalPath)
}

// SetRuntimeIdentity associates validated runtime identity with an existing snapshot binding.
func (ss *SnapshotService) SetRuntimeIdentity(dependencyName, version string, rt domain.RuntimeIdentity) error {
	key := makeSnapshotKey(dependencyName, version)

	ss.mu.Lock()
	defer ss.mu.Unlock()

	b, ok := ss.bindings[key]
	if !ok || b == nil {
		return fmt.Errorf("%w: %s (%s)", domain.ErrSnapshotUnverified, dependencyName, version)
	}

	// 1. Primary snapshot digest must match the binding
	if rt.PrimarySnapshotSHA256 == "" || !strings.EqualFold(rt.PrimarySnapshotSHA256, b.SnapshotManifestSHA256) {
		return fmt.Errorf("%w: runtime primary snapshot digest mismatch: expected %s, got %s",
			domain.ErrSnapshotDigestMismatch, b.SnapshotManifestSHA256, rt.PrimarySnapshotSHA256)
	}

	// 2. RuntimeManifestSHA256 must recompute correctly and match if provided
	expectedManifestSHA := rt.ComputeRuntimeManifestSHA256()
	if rt.RuntimeManifestSHA256 != "" && !strings.EqualFold(rt.RuntimeManifestSHA256, expectedManifestSHA) {
		return fmt.Errorf("%w: runtime manifest SHA mismatch: expected %s, got %s",
			domain.ErrSnapshotDigestMismatch, expectedManifestSHA, rt.RuntimeManifestSHA256)
	}
	rt.RuntimeManifestSHA256 = expectedManifestSHA

	// 3. For pinned runtime packs: validate frozen source revision and package version.
	lowerDep := strings.ToLower(dependencyName)
	if strings.Contains(lowerDep, "zerotts") || strings.Contains(lowerDep, "zeroweight") {
		if dependencyName != domain.PinnedZeroTTSModelID || version != domain.PinnedZeroTTSModelVersion {
			return fmt.Errorf("%w: ZeroTTS model identity mismatch: expected %s:%s, got %s:%s",
				domain.ErrSnapshotUnverified, domain.PinnedZeroTTSModelID, domain.PinnedZeroTTSModelVersion, dependencyName, version)
		}
		if rt.SourceRevision != domain.PinnedZeroTTSSourceRevision {
			return fmt.Errorf("%w: ZeroTTS runtime source revision mismatch: expected %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedZeroTTSSourceRevision, rt.SourceRevision)
		}
		ver, ok := rt.RuntimeVersions["zerotts"]
		if !ok || strings.TrimSpace(ver) != domain.PinnedZeroTTSPackageVersion {
			return fmt.Errorf("%w: ZeroTTS runtime package version mismatch: expected exact %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedZeroTTSPackageVersion, ver)
		}
		if rt.AdapterRevision != domain.PinnedZeroTTSAdapterRevision {
			return fmt.Errorf("%w: ZeroTTS adapter revision mismatch: expected %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedZeroTTSAdapterRevision, rt.AdapterRevision)
		}
	} else if strings.Contains(lowerDep, "demucs") {
		if rt.SourceRevision != domain.PinnedDemucsSourceRevision {
			return fmt.Errorf("%w: Demucs runtime source revision mismatch: expected %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedDemucsSourceRevision, rt.SourceRevision)
		}
		ver, ok := rt.RuntimeVersions["demucs"]
		if !ok || strings.TrimSpace(ver) != domain.PinnedDemucsPackageVersion {
			return fmt.Errorf("%w: Demucs runtime package version mismatch: expected exact %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedDemucsPackageVersion, ver)
		}
	} else if strings.Contains(lowerDep, "uvr") || strings.Contains(lowerDep, "mdx") {
		if rt.SourceRevision != domain.PinnedUVRSourceRevision {
			return fmt.Errorf("%w: UVR runtime source revision mismatch: expected %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedUVRSourceRevision, rt.SourceRevision)
		}
		ver, ok := rt.RuntimeVersions["audio-separator"]
		if !ok || strings.TrimSpace(ver) != domain.PinnedUVRPackageVersion {
			return fmt.Errorf("%w: UVR runtime package version mismatch: expected exact %s, got %s",
				domain.ErrSnapshotUnverified, domain.PinnedUVRPackageVersion, ver)
		}
	}

	// 4. Validate dependency snapshot SHAs consistency if declared
	for _, depSHA := range rt.DependencySnapshotSHAs {
		if strings.TrimSpace(depSHA) == "" || len(depSHA) != 64 {
			return fmt.Errorf("%w: invalid dependency snapshot digest %q in runtime identity",
				domain.ErrSnapshotDigestMismatch, depSHA)
		}
	}

	b.RuntimeIdentity = &rt
	return nil
}

// GetPortableEvidence constructs portable evidence without weights or machine-local absolute paths.
func (ss *SnapshotService) GetPortableEvidence(ctx context.Context, dependencyName, version string) (*domain.SnapshotPortableEvidence, error) {
	b, err := ss.GetBinding(dependencyName, version)
	if err != nil {
		return nil, err
	}

	files := make([]domain.SnapshotEvidenceFile, len(b.Manifest.Files))
	for i, f := range b.Manifest.Files {
		files[i] = domain.SnapshotEvidenceFile{
			RelativePath: domain.NormalizeRelativePath(f.RelativePath),
			SHA256:       f.SHA256,
			SizeBytes:    f.SizeBytes,
		}
	}

	var licID string
	if ss.licSvc != nil {
		if entry, err := ss.licSvc.GetManifest(ctx, dependencyName, version); err == nil && entry != nil {
			licID = entry.ID
		}
	}

	return &domain.SnapshotPortableEvidence{
		DependencyName:         b.DependencyName,
		Version:                b.Version,
		SnapshotManifestSHA256: b.SnapshotManifestSHA256,
		Files:                  files,
		RuntimeIdentity:        b.RuntimeIdentity,
		LicenseManifestID:      licID,
		VerificationEventID:    b.VerificationEventID,
		VerifiedAt:             b.VerifiedAt,
	}, nil
}

func (ss *SnapshotService) recordEvent(ctx context.Context, manifest domain.SnapshotManifest, outcome, errMsg string, fileCount int, totalBytes int64) {
	ss.recordEventWithID(ctx, uuid.NewString(), manifest, outcome, errMsg, fileCount, totalBytes, time.Now().UTC())
}

func (ss *SnapshotService) recordEventWithID(ctx context.Context, id string, manifest domain.SnapshotManifest, outcome, errMsg string, fileCount int, totalBytes int64, at time.Time) {
	if ss.db == nil {
		return
	}
	ev := domain.SnapshotVerificationEvent{
		ID:                     id,
		DependencyName:         manifest.ModelID,
		Version:                manifest.ModelVersion,
		SnapshotManifestSHA256: manifest.SnapshotManifestSHA256,
		Outcome:                outcome,
		ErrorMessage:           errMsg,
		FileCount:              fileCount,
		TotalBytes:             totalBytes,
		VerifierVersion:        "1.0",
		VerifiedAt:             at,
	}
	_ = ss.db.RecordSnapshotVerificationEvent(ctx, ev)
}

func hashFile(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
