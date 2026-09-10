package provider_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestWorkerAudioRoleProvider_EnsureRuntimeIdentity_ExactRevisionAndStatusOK(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	db, err := storage.Open(filepath.Join(tmpDir, "test_ensure_rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// 1. Build stageworker binary
	stageWorkerExe := filepath.Join(tmpDir, "stageworker-ar")
	if runtime.GOOS == "windows" {
		stageWorkerExe += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", stageWorkerExe, "github.com/monet88/douyinie/cmd/stageworker")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stageworker: %v: %s", err, string(out))
	}
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerExe)

	// 2. Setup mock python adapter returning controlled probe JSON
	configFile := filepath.Join(tmpDir, "mock_probe_config.json")
	pyInterpScript := filepath.Join(tmpDir, "mock_probe_interp.py")
	scriptContent := fmt.Sprintf(`import sys, json, os
input_data = sys.stdin.read()
req = json.loads(input_data) if input_data.strip() else {}
config_file = %q
cfg = {}
if os.path.exists(config_file):
    with open(config_file, "r") as f:
        cfg = json.load(f)
print(json.dumps(cfg))
sys.exit(0)
`, configFile)
	if err := os.WriteFile(pyInterpScript, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	var pyLauncher string
	if runtime.GOOS == "windows" {
		pyLauncher = filepath.Join(tmpDir, "mock_py.bat")
		batContent := fmt.Sprintf("@echo off\npython -u %q %%*\n", pyInterpScript)
		if err := os.WriteFile(pyLauncher, []byte(batContent), 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		pyLauncher = filepath.Join(tmpDir, "mock_py.sh")
		shContent := fmt.Sprintf("#!/bin/sh\npython3 -u %q \"$@\"\n", pyInterpScript)
		if err := os.WriteFile(pyLauncher, []byte(shContent), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN", pyLauncher)
	t.Setenv("DOUYINIE_AUDIO_ROLE_ADAPTER", "")
	t.Setenv("DOUYINIE_AUDIO_ROLE_BIN", "")

	// 3. Setup YAMNet license and snapshot
	yamnetDir := filepath.Join(tmpDir, "yamnet_snap")
	if err := os.MkdirAll(yamnetDir, 0755); err != nil {
		t.Fatal(err)
	}
	tfliteFile := filepath.Join(yamnetDir, "yamnet.tflite")
	if err := os.WriteFile(tfliteFile, []byte("fake_tflite_bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	classMapFile := filepath.Join(yamnetDir, "yamnet_class_map.csv")
	if err := os.WriteFile(classMapFile, []byte("fake_csv_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	hTflite := sha256.Sum256([]byte("fake_tflite_bytes"))
	hClassMap := sha256.Sum256([]byte("fake_csv_bytes"))
	snapManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       provider.YAMNetModelID,
		ModelVersion:  provider.YAMNetModelVersion,
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "yamnet.tflite",
				SHA256:       hex.EncodeToString(hTflite[:]),
				SizeBytes:    int64(len("fake_tflite_bytes")),
			},
			{
				RelativePath: "yamnet_class_map.csv",
				SHA256:       hex.EncodeToString(hClassMap[:]),
				SizeBytes:    int64(len("fake_csv_bytes")),
			},
		},
	}
	cSHA, err := domain.ComputeSnapshotManifestSHA256(&snapManifest)
	if err != nil {
		t.Fatal(err)
	}
	snapManifest.SnapshotManifestSHA256 = cSHA

	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.YAMNetModelID,
		Version:        provider.YAMNetModelVersion,
		SHA256:         cSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "AudioSet",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register manifest: %v", err)
	}

	_, err = snapSvc.RegisterAndVerifySnapshot(ctx, snapManifest, yamnetDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot failed: %v", err)
	}

	newTestProvider := func() *provider.WorkerAudioRoleProvider {
		p, pErr := provider.NewWorkerAudioRoleProvider(provider.YAMNetProviderID, provider.YAMNetModelID, provider.YAMNetModelVersion, 0.95)
		if pErr != nil {
			t.Fatal(pErr)
		}
		p.SetSnapshotService(snapSvc)
		p.SetRequiresSnapshot(true)
		return p
	}

	// Subtest 1: Probe status is not "ok" -> fails closed with ErrSnapshotUnverified
	t.Run("ProbeStatusNotOK_FailsClosed", func(t *testing.T) {
		probeData := map[string]any{
			"status":           "error",
			"package_name":     "ai-edge-litert",
			"package_version":  domain.PinnedYAMNetPackageVersion,
			"source_revision":  "google/yamnet@v1",
			"adapter_revision": domain.PinnedYAMNetAdapterRevision,
			"runtime_versions": map[string]string{
				"ai-edge-litert": domain.PinnedYAMNetPackageVersion,
			},
		}
		b, err := json.Marshal(probeData)
		if err != nil {
			t.Fatalf("marshal probeData: %v", err)
		}
		if err := os.WriteFile(configFile, b, 0644); err != nil {
			t.Fatalf("write probe config file: %v", err)
		}

		p := newTestProvider()
		err = p.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail when probe status is error")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})

	// Subtest 2: Adapter revision is a prefix match but not exact -> fails closed with ErrSnapshotUnverified
	t.Run("PrefixRevision_FailsClosed", func(t *testing.T) {
		probeData := map[string]any{
			"status":           "ok",
			"package_name":     "ai-edge-litert",
			"package_version":  domain.PinnedYAMNetPackageVersion,
			"source_revision":  "google/yamnet@v1",
			"adapter_revision": domain.PinnedYAMNetAdapterRevision + "-custom",
			"runtime_versions": map[string]string{
				"ai-edge-litert": domain.PinnedYAMNetPackageVersion,
			},
		}
		b, err := json.Marshal(probeData)
		if err != nil {
			t.Fatalf("marshal probeData: %v", err)
		}
		if err := os.WriteFile(configFile, b, 0644); err != nil {
			t.Fatalf("write probe config file: %v", err)
		}

		p := newTestProvider()
		err = p.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail on non-exact adapter revision prefix")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})

	// Subtest 3: Exact revision and status ok -> succeeds
	t.Run("ExactRevisionAndStatusOK_Succeeds", func(t *testing.T) {
		probeData := map[string]any{
			"status":           "ok",
			"package_name":     "ai-edge-litert",
			"package_version":  domain.PinnedYAMNetPackageVersion,
			"source_revision":  "google/yamnet@v1",
			"adapter_revision": domain.PinnedYAMNetAdapterRevision,
			"runtime_versions": map[string]string{
				"ai-edge-litert": domain.PinnedYAMNetPackageVersion,
			},
		}
		b, err := json.Marshal(probeData)
		if err != nil {
			t.Fatalf("marshal probeData: %v", err)
		}
		if err := os.WriteFile(configFile, b, 0644); err != nil {
			t.Fatalf("write probe config file: %v", err)
		}

		p := newTestProvider()
		err = p.EnsureRuntimeIdentity(ctx)
		if err != nil {
			t.Fatalf("expected EnsureRuntimeIdentity to succeed with exact revision and status ok, got: %v", err)
		}
	})
}
