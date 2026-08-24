package governance

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// LicenseService manages the 4 license obligation layers and enforces immutable versioned fail-closed checkpoint validation.
type LicenseService struct {
	mu    sync.RWMutex
	db    *storage.DB
	inMem map[string][]domain.LicenseManifestEntry
}

// NewLicenseService creates a new LicenseService.
func NewLicenseService(db *storage.DB) *LicenseService {
	return &LicenseService{
		db:    db,
		inMem: make(map[string][]domain.LicenseManifestEntry),
	}
}

// RegisterManifest registers an immutable, versioned license manifest entry.
func (ls *LicenseService) RegisterManifest(ctx context.Context, entry domain.LicenseManifestEntry) error {
	if entry.ID == "" {
		entry.ID = uuid.NewString()
	}
	if entry.Version == "" {
		entry.Version = "v1"
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	// Validate 4 obligation layers
	if strings.TrimSpace(entry.CodeLicense) == "" ||
		strings.TrimSpace(entry.ModelLicense) == "" ||
		strings.TrimSpace(entry.DataLicense) == "" ||
		strings.TrimSpace(entry.ServiceTerms) == "" {
		return fmt.Errorf("all 4 license obligation layers (CODE_LICENSE, MODEL_LICENSE, DATA_LICENSE, SERVICE_TERMS) must be declared")
	}

	if strings.TrimSpace(entry.DependencyName) == "" || strings.TrimSpace(entry.SHA256) == "" {
		return fmt.Errorf("dependency_name and sha256 are required")
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	if entries, ok := ls.inMem[entry.DependencyName]; ok {
		for _, existing := range entries {
			if existing.Version == entry.Version {
				return fmt.Errorf("license manifest for dependency %q version %q already exists (immutable)", entry.DependencyName, entry.Version)
			}
		}
	}

	if ls.db != nil {
		if err := ls.db.SaveLicenseManifest(ctx, entry); err != nil {
			return fmt.Errorf("save license manifest: %w", err)
		}
	}

	ls.inMem[entry.DependencyName] = append(ls.inMem[entry.DependencyName], entry)
	return nil
}

// GetManifest retrieves a manifest by dependency name and optional version.
// When an exact version is specified, it fails closed if that version is missing (never falling back to latest).
func (ls *LicenseService) GetManifest(ctx context.Context, dependencyName string, version string) (*domain.LicenseManifestEntry, error) {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	if version != "" {
		if ls.db != nil {
			entry, err := ls.db.GetLicenseManifest(ctx, dependencyName, version)
			if err == nil && entry != nil {
				return entry, nil
			}
			return nil, domain.ErrLicenseManifestMissing
		}

		if entries, ok := ls.inMem[dependencyName]; ok && len(entries) > 0 {
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Version == version {
					return &entries[i], nil
				}
			}
			return nil, domain.ErrLicenseManifestMissing
		}
		return nil, domain.ErrLicenseManifestMissing
	}

	// No version specified: return latest registered
	if ls.db != nil {
		entry, err := ls.db.GetLicenseManifest(ctx, dependencyName, "")
		if err == nil && entry != nil {
			return entry, nil
		}
		return nil, domain.ErrLicenseManifestMissing
	}

	if entries, ok := ls.inMem[dependencyName]; ok && len(entries) > 0 {
		return &entries[len(entries)-1], nil
	}

	return nil, domain.ErrLicenseManifestMissing
}

// ListManifests lists registered license manifests, optionally filtered by dependency name.
func (ls *LicenseService) ListManifests(ctx context.Context, dependencyName string) ([]domain.LicenseManifestEntry, error) {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	if ls.db != nil {
		return ls.db.ListLicenseManifests(ctx, dependencyName)
	}

	var list []domain.LicenseManifestEntry
	if dependencyName != "" {
		list = append(list, ls.inMem[dependencyName]...)
	} else {
		for _, entries := range ls.inMem {
			list = append(list, entries...)
		}
	}
	return list, nil
}

// VerifyCheckpoint performs fail-closed verification of a checkpoint or model dependency.
func (ls *LicenseService) VerifyCheckpoint(ctx context.Context, dependencyName string, version string, expectedSHA256 string) error {
	entry, err := ls.GetManifest(ctx, dependencyName, version)
	if err != nil {
		return fmt.Errorf("%w: dependency %q (ver: %q) not found in manifest", domain.ErrLicenseManifestMissing, dependencyName, version)
	}

	if !entry.Verified {
		return fmt.Errorf("%w: dependency %q is unverified", domain.ErrLicenseManifestMissing, dependencyName)
	}

	if expectedSHA256 != "" && !strings.EqualFold(entry.SHA256, expectedSHA256) {
		return fmt.Errorf("%w: hash mismatch for %q (manifest: %s, expected: %s)", domain.ErrLicenseManifestMissing, dependencyName, entry.SHA256, expectedSHA256)
	}

	return nil
}
