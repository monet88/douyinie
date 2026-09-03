package seam2_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/worker"
)

func TestSeam2_ModelSnapshot_ValidateEnvelope(t *testing.T) {
	validEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "qwen3-asr",
			Version:                "1.7b",
			SnapshotManifestSHA256: "aabbccdd11223344",
			LocalPath:              "/models/qwen3-asr",
		},
	}

	validCmd := worker.Command{
		ID:        "cmd-snap-1",
		Family:    "asr",
		Stage:     "asr",
		AttemptID: "att-1",
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: validEnv,
		},
	}
	b, _ := json.Marshal(validCmd)
	envelope := worker.Envelope{
		Type:    worker.MessageTypeCommand,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: b,
	}

	if err := worker.ValidateEnvelope(envelope); err != nil {
		t.Fatalf("expected valid envelope to pass, got %v", err)
	}

	// 2. Missing dependency_name fails validation
	badEnv := validEnv
	badEnv.Primary.DependencyName = ""
	badCmd := validCmd
	badCmd.Config = map[string]any{
		worker.ConfigKeyModelSnapshot: badEnv,
	}
	bBad, _ := json.Marshal(badCmd)
	badEnvelope := worker.Envelope{
		Type:    worker.MessageTypeCommand,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: bBad,
	}
	if err := worker.ValidateEnvelope(badEnvelope); err == nil {
		t.Fatalf("expected missing dependency_name to fail envelope validation")
	}

	// 3. Missing snapshot_manifest_sha256 fails validation
	badEnv2 := validEnv
	badEnv2.Primary.SnapshotManifestSHA256 = ""
	badCmd2 := validCmd
	badCmd2.Config = map[string]any{
		worker.ConfigKeyModelSnapshot: badEnv2,
	}
	bBad2, _ := json.Marshal(badCmd2)
	badEnvelope2 := worker.Envelope{
		Type:    worker.MessageTypeCommand,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: bBad2,
	}
	if err := worker.ValidateEnvelope(badEnvelope2); err == nil {
		t.Fatalf("expected missing snapshot_manifest_sha256 to fail envelope validation")
	}
}

func TestSeam2_ModelSnapshot_MissingPathFailsClosed(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()

	if err := sup.Spawn(ctx, "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	_, err := client.Handshake(context.Background(), 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	outPath := filepath.Join(t.TempDir(), "out.json")
	missingPath := filepath.Join(t.TempDir(), "nonexistent_snapshot_dir")

	cmd := worker.Command{
		ID:         "cmd-missing-path",
		Family:     "asr",
		Stage:      "generic",
		AttemptID:  "attempt-missing",
		RunID:      "run-1",
		OutputPath: outPath,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: worker.ModelSnapshotEnvelope{
				Primary: worker.ModelSnapshotRef{
					Role:                   "primary",
					DependencyName:         "qwen3-asr",
					Version:                "1.7b",
					SnapshotManifestSHA256: "aabbccdd11223344",
					LocalPath:              missingPath,
				},
			},
		},
	}

	_, err = client.Run(ctx, cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected missing snapshot path to fail closed, got success")
	}

	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED error code, got %v", err)
	}
}

func TestSeam2_ModelSnapshot_RequiredEnvelopeMissingFailsClosed(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()

	if err := sup.Spawn(ctx, "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	_, err := client.Handshake(context.Background(), 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-no-envelope",
		Family:     "asr",
		Stage:      "generic",
		AttemptID:  "attempt-no-env",
		RunID:      "run-1",
		OutputPath: outPath,
		Config: map[string]any{
			"require_model_snapshot": true,
		},
	}

	_, err = client.Run(ctx, cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected command requiring snapshot without envelope to fail closed")
	}

	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED, got %v", err)
	}
}

func TestSeam2_ModelSnapshot_DependencyPathMissingFailsClosed(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()

	if err := sup.Spawn(ctx, "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	_, err := client.Handshake(context.Background(), 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	// Create valid primary snapshot directory
	primaryDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(primaryDir, "weights.bin"), []byte("data"), 0644)
	nonexistentDepPath := filepath.Join(t.TempDir(), "nonexistent_vad")

	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-dep-missing",
		Family:     "asr",
		Stage:      "generic",
		AttemptID:  "attempt-dep-missing",
		RunID:      "run-1",
		OutputPath: outPath,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: worker.ModelSnapshotEnvelope{
				Primary: worker.ModelSnapshotRef{
					Role:                   "primary",
					DependencyName:         "campplus-diarizer",
					Version:                "v1",
					SnapshotManifestSHA256: "aabbccdd11223344",
					LocalPath:              primaryDir,
				},
				Dependencies: []worker.ModelSnapshotRef{
					{
						Role:                   "vad",
						DependencyName:         "fsmn-vad",
						Version:                "v2",
						SnapshotManifestSHA256: "5566778899001122",
						LocalPath:              nonexistentDepPath,
					},
				},
			},
		},
	}

	_, err = client.Run(ctx, cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected missing dependency snapshot path to fail closed")
	}

	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED, got %v", err)
	}
}

func TestSeam2_ModelSnapshot_ValidPathsExecuteSuccessfully(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()

	if err := sup.Spawn(ctx, "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	_, err := client.Handshake(context.Background(), 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	// Create valid primary snapshot directory
	primaryDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(primaryDir, "weights.bin"), []byte("data"), 0644)

	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-valid-snap",
		Family:     "asr",
		Stage:      "generic",
		AttemptID:  "attempt-valid",
		RunID:      "run-1",
		OutputPath: outPath,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: worker.ModelSnapshotEnvelope{
				Primary: worker.ModelSnapshotRef{
					Role:                   "primary",
					DependencyName:         "qwen3-asr",
					Version:                "1.7b",
					SnapshotManifestSHA256: "aabbccdd11223344",
					LocalPath:              primaryDir,
				},
			},
		},
	}

	artifact, err := client.Run(ctx, cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("expected valid snapshot paths to execute successfully, got error: %v", err)
	}
	if artifact.Path != outPath {
		t.Fatalf("unexpected artifact path: got %s, want %s", artifact.Path, outPath)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("output artifact missing: %v", err)
	}
}
