package provider

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
	"github.com/monet88/douyinie/internal/worker"
)

func newSeparatorEnvelopeTestBinding(t *testing.T, modelID, modelVer string, files map[string][]byte) (*governance.SnapshotService, *governance.SnapshotVerificationBinding, string) {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "envelope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	snapDir := filepath.Join(tmpDir, "snap")
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}
	var entries []domain.SnapshotFileEntry
	for rel, content := range files {
		full := filepath.Join(snapDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0644); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(content)
		entries = append(entries, domain.SnapshotFileEntry{
			RelativePath: rel,
			SHA256:       hex.EncodeToString(h[:]),
			SizeBytes:    int64(len(content)),
		})
	}
	manifest := domain.SnapshotManifest{SchemaVersion: "1.0", ModelID: modelID, ModelVersion: modelVer, Files: entries}
	cSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotManifestSHA256 = cSHA
	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: modelID,
		Version:        modelVer,
		SHA256:         cSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot: %v", err)
	}
	return snapSvc, binding, snapDir
}

func runSeparatorEnvelope(t *testing.T, snapSvc *governance.SnapshotService, modelID, modelVer string) (worker.Command, error) {
	t.Helper()
	bridge := &workerBridge{
		binary:           filepath.Join(t.TempDir(), "nonexistent-stageworker"),
		handshakeTimeout: 5 * time.Second,
		runTimeout:       30 * time.Second,
		snapshotSvc:      snapSvc,
	}
	dummyWav := filepath.Join(t.TempDir(), "dummy.wav")
	if err := os.WriteFile(dummyWav, []byte("fake-pcm"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := newCommand("separator", stageWorkerFamilySeparator, worker.ArtifactRef{Path: dummyWav}, modelID, modelVer)
	cmd.Config["require_model_snapshot"] = true
	cmd.OutputPath = filepath.Join(t.TempDir(), "out.json")
	_, err := bridge.run(context.Background(), cmd)
	return cmd, err
}

func TestWorkerBridge_SeparatorDemucsBagYAMLUsesPrimaryIdentity(t *testing.T) {
	snapSvc, binding, _ := newSeparatorEnvelopeTestBinding(t, DemucsModelID, DemucsModelVersion, map[string][]byte{
		"955717e8-8726e21a.th": []byte("demucs_ckpt_bytes_for_envelope"),
		"htdemucs.yaml":        []byte("models: ['955717e8']\n"),
	})
	// Simulate verified real snapshot: manifest declares pinned hashes (file bytes stay dummy;
	// RegisterAndVerifySnapshot already pinned size/mtime fingerprints, Resolve checks manifest SHAs).
	for i, f := range binding.Manifest.Files {
		if strings.HasSuffix(strings.ToLower(f.RelativePath), "955717e8-8726e21a.th") {
			binding.Manifest.Files[i].SHA256 = domain.PinnedDemucsCheckpointSHA
		}
		if strings.HasSuffix(strings.ToLower(f.RelativePath), "htdemucs.yaml") {
			binding.Manifest.Files[i].SHA256 = domain.PinnedDemucsBagYAMLSHA256
		}
	}

	cmd, err := runSeparatorEnvelope(t, snapSvc, DemucsModelID, DemucsModelVersion)
	if err == nil {
		t.Fatal("expected spawn failure with nonexistent binary, got nil")
	}
	if strings.Contains(err.Error(), "no binding for htdemucs.yaml") || strings.Contains(err.Error(), "pre-invocation") {
		t.Fatalf("envelope must not fail pre-invocation with filename binding, got: %v", err)
	}
	env, err := worker.GetModelSnapshotEnvelope(cmd.Config)
	if err != nil || env == nil {
		t.Fatalf("expected envelope in cmd.Config, got env=%v err=%v", env, err)
	}
	if len(env.Dependencies) != 1 {
		t.Fatalf("expected 1 Demucs metadata dependency, got %d", len(env.Dependencies))
	}
	dep := env.Dependencies[0]
	if dep.DependencyName != DemucsModelID || dep.Version != DemucsModelVersion || dep.Role != "bag_yaml" {
		t.Fatalf("wrong Demucs dep identity: %+v", dep)
	}
	if filepath.Base(dep.EntrypointFile) != "htdemucs.yaml" {
		t.Fatalf("EntrypointFile must be the yaml file, got %q", dep.EntrypointFile)
	}
	if err := snapSvc.CheckBindingFingerprints(dep.DependencyName, dep.Version); err != nil {
		t.Fatalf("dep CheckBindingFingerprints must pass via primary identity, got: %v", err)
	}

	// Mutation of the manifest-contained yaml must fail closed via the primary binding.
	yamlPath := dep.EntrypointFile
	b, _ := os.ReadFile(yamlPath)
	if err := os.WriteFile(yamlPath, append(b, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	if err := snapSvc.CheckBindingFingerprints(DemucsModelID, DemucsModelVersion); !errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
		t.Fatalf("expected ErrSnapshotMutatedRehashRequired after yaml mutation, got: %v", err)
	}
}

func TestWorkerBridge_SeparatorUVRMetadataUsesPrimaryIdentity(t *testing.T) {
	metaContent := []byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`)
	snapSvc, binding, _ := newSeparatorEnvelopeTestBinding(t, UVRModelID, UVRModelVersion, map[string][]byte{
		"UVR-MDX-NET-Inst_HQ_4.onnx": []byte("uvr_onnx_bytes_for_envelope"),
		"mdx_model_data.json":        metaContent,
	})
	for i, f := range binding.Manifest.Files {
		if strings.HasSuffix(strings.ToLower(f.RelativePath), "uvr-mdx-net-inst_hq_4.onnx") {
			binding.Manifest.Files[i].SHA256 = domain.PinnedUVRArtifactSHA256
		}
	}

	cmd, err := runSeparatorEnvelope(t, snapSvc, UVRModelID, UVRModelVersion)
	if err == nil {
		t.Fatal("expected spawn failure with nonexistent binary, got nil")
	}
	if strings.Contains(err.Error(), "no binding for mdx_model_data.json") || strings.Contains(err.Error(), "pre-invocation") {
		t.Fatalf("envelope must not fail pre-invocation with filename binding, got: %v", err)
	}
	env, err := worker.GetModelSnapshotEnvelope(cmd.Config)
	if err != nil || env == nil {
		t.Fatalf("expected envelope in cmd.Config, got env=%v err=%v", env, err)
	}
	if len(env.Dependencies) != 1 {
		t.Fatalf("expected 1 UVR metadata dependency, got %d", len(env.Dependencies))
	}
	dep := env.Dependencies[0]
	if dep.DependencyName != UVRModelID || dep.Version != UVRModelVersion || dep.Role != "model_metadata" {
		t.Fatalf("wrong UVR dep identity: %+v", dep)
	}
	if filepath.Base(dep.EntrypointFile) != "mdx_model_data.json" {
		t.Fatalf("EntrypointFile must be mdx_model_data.json, got %q", dep.EntrypointFile)
	}
	if err := snapSvc.CheckBindingFingerprints(dep.DependencyName, dep.Version); err != nil {
		t.Fatalf("dep CheckBindingFingerprints must pass via primary identity, got: %v", err)
	}

	metaPath := dep.EntrypointFile
	b, _ := os.ReadFile(metaPath)
	if err := os.WriteFile(metaPath, append(b, ' '), 0644); err != nil {
		t.Fatal(err)
	}
	if err := snapSvc.CheckBindingFingerprints(UVRModelID, UVRModelVersion); !errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
		t.Fatalf("expected ErrSnapshotMutatedRehashRequired after metadata mutation, got: %v", err)
	}
}
