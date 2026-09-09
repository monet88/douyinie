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
	validEntry := domain.LicenseManifestEntry{
		DependencyName: provider.YAMNetModelID,
		Version:        provider.YAMNetModelVersion,
		SHA256:         domain.PinnedYAMNetManifestSHA256,
		SourceRepo:     "https://tfhub.dev/google/lite-model/yamnet/classification/tflite/1?lite-format=tflite",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "AudioSet dataset: CC-BY-4.0; AudioSet ontology/class map: CC-BY-SA-4.0",
		ServiceTerms:   "https://tfhub.dev/terms (local snapshot runtime inference; no external network calls at inference time)",
		Verified:       true,
	}

	cases := []struct {
		name      string
		transform func(e *domain.LicenseManifestEntry)
	}{
		{
			name: "sha256_mismatch",
			transform: func(e *domain.LicenseManifestEntry) {
				e.SHA256 = "deadbeefdeadbeef000000000000000000000000000000000000000000000000"
			},
		},
		{
			name: "source_repo_mismatch",
			transform: func(e *domain.LicenseManifestEntry) {
				e.SourceRepo = "https://unauthorized-mirror.com/yamnet"
			},
		},
		{
			name: "service_terms_mismatch",
			transform: func(e *domain.LicenseManifestEntry) {
				e.ServiceTerms = "https://other-terms.com"
			},
		},
		{
			name: "code_license_mismatch",
			transform: func(e *domain.LicenseManifestEntry) {
				e.CodeLicense = "GPL-3.0"
			},
		},
		{
			name: "data_license_mismatch",
			transform: func(e *domain.LicenseManifestEntry) {
				e.DataLicense = "Proprietary"
			},
		},
		{
			name: "unverified_entry",
			transform: func(e *domain.LicenseManifestEntry) {
				e.Verified = false
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := storage.Open(filepath.Join(t.TempDir(), "test_"+tc.name+".db"))
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			defer db.Close()

			licSvc := governance.NewLicenseService(db)
			ctx := context.Background()

			mutated := validEntry
			mutated.ID = uuid.NewString()
			mutated.CreatedAt = time.Now().UTC()
			tc.transform(&mutated)

			if err := licSvc.RegisterManifest(ctx, mutated); err != nil {
				t.Fatalf("seed manifest: %v", err)
			}

			// Bootstrap must fail closed on any field mismatch!
			err = provider.BootstrapYAMNetLicenseManifest(ctx, licSvc)
			if err == nil {
				t.Fatalf("expected bootstrap to fail closed on %s, got nil error", tc.name)
			}
		})
	}
}
