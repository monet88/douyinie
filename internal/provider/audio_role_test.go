package provider_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

func TestBootstrapYAMNetLicenseManifest_FreshRegistration(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test_lic_fresh.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	ctx := context.Background()

	if err := provider.BootstrapYAMNetLicenseManifest(ctx, licSvc); err != nil {
		t.Fatalf("expected fresh bootstrap to succeed, got: %v", err)
	}

	entry, err := licSvc.GetManifest(ctx, provider.YAMNetModelID, provider.YAMNetModelVersion)
	if err != nil {
		t.Fatalf("get manifest failed: %v", err)
	}
	if entry.SHA256 != domain.PinnedYAMNetManifestSHA256 {
		t.Errorf("manifest sha mismatch: got %s, want %s", entry.SHA256, domain.PinnedYAMNetManifestSHA256)
	}
	if !entry.Verified {
		t.Errorf("expected verified manifest")
	}
	if entry.CodeLicense != "Apache-2.0" || entry.ModelLicense != "Apache-2.0" {
		t.Errorf("unexpected code/model license: %s / %s", entry.CodeLicense, entry.ModelLicense)
	}
}

func TestBootstrapYAMNetLicenseManifest_Idempotent(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test_lic_idempotent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	ctx := context.Background()

	// First registration
	if err := provider.BootstrapYAMNetLicenseManifest(ctx, licSvc); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}

	// Second registration: idempotent hit
	if err := provider.BootstrapYAMNetLicenseManifest(ctx, licSvc); err != nil {
		t.Fatalf("second bootstrap (idempotent) failed: %v", err)
	}
}

func TestBootstrapYAMNetLicenseManifest_MismatchedExisting_FailsClosed(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test_lic_mismatch.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	ctx := context.Background()

	// Seed corrupted / mismatched manifest entry
	badEntry := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.YAMNetModelID,
		Version:        provider.YAMNetModelVersion,
		SHA256:         "deadbeefdeadbeef000000000000000000000000000000000000000000000000",
		SourceRepo:     "https://example.com/bad",
		CodeLicense:    "GPL-3.0",
		ModelLicense:   "Proprietary",
		DataLicense:    "Unknown",
		ServiceTerms:   "None",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := licSvc.RegisterManifest(ctx, badEntry); err != nil {
		t.Fatalf("seed bad manifest: %v", err)
	}

	// Bootstrap should detect mismatch and fail closed!
	err = provider.BootstrapYAMNetLicenseManifest(ctx, licSvc)
	if err == nil {
		t.Fatal("expected bootstrap to fail closed on mismatched manifest, got nil")
	}
}
