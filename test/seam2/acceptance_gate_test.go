package seam2_test

// Issue #43 — Phase 1 acceptance/regression gate (Seam 2).
//
// Seam 2 verifies the worker/runtime contract that the Seam 1 acceptance gate depends on:
// media and serialized artifacts travel as filesystem REFS (path + content address), never as
// inline IPC payloads. The RuntimeHost re-probes the artifact at the returned path to establish
// measured-media truth. This test asserts that transport contract end-to-end against a REAL
// worker-produced stage artifact (ASR adapter over the fake Qwen3 model binary): the worker
// writes the artifact to the exact requested OutputPath, returns a metadata-only ArtifactRef
// whose content address is the independently recomputable SHA-256 of the transported bytes —
// never a placeholder.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/worker"
)

// TestSeam2_AcceptanceGate_ArtifactTransportContract verifies the worker contract moves a real
// artifact object to the RuntimeHost-specified path and returns a metadata-only reference whose
// content address matches an INDEPENDENT SHA-256 recomputation over the on-disk bytes, so the
// execution-plane artifact is verifiably content-addressed and probeable for measured duration.
func TestSeam2_AcceptanceGate_ArtifactTransportContract(t *testing.T) {
	// Real worker-produced artifact: run the ASR adapter against the deterministic fake
	// Qwen3 model binary. This exercises the production invocation path (stdin JSON ->
	// model binary -> parsed stdout -> writeOutputArtifact), not a synthetic marker.
	binDir := buildFakeModel(t, "qwen3-asr")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()
	if err := sup.Spawn(ctx, "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() { _ = client.Shutdown() }()

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	if err := os.WriteFile(audioPath, []byte("fake audio data"), 0644); err != nil {
		t.Fatalf("write input audio: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "acceptance-artifact.json")
	cmd := worker.Command{
		ID:         "acceptance-cmd-1",
		Family:     "asr",
		Stage:      "asr",
		AttemptID:  "acceptance-attempt-1",
		RunID:      "acceptance-run-1",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config:     map[string]any{"model_name": "qwen3-asr-1.7b", "model_version": "1.7b"},
	}

	artifact, err := client.Run(ctx, cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Invariant: the artifact is a metadata-only reference carrying a path + content address.
	if artifact.Path != outPath {
		t.Fatalf("artifact path %q != requested OutputPath %q (filesystem-ref transport breach)", artifact.Path, outPath)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("content address must be a real SHA-256, got %q", artifact.SHA256)
	}

	// Invariant: the bytes land on disk at the requested path (RuntimeHost re-probes here).
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output artifact missing at %s: %v", outPath, err)
	}
	if len(data) == 0 {
		t.Fatalf("output artifact is empty — no real artifact object transported")
	}

	// Invariant: content-address integrity. The returned ref's SHA-256 must equal an
	// INDEPENDENT recomputation over the transported bytes. This proves the ref binds
	// to exactly these bytes (no placeholder, no hash/bytes divergence).
	sum := sha256.Sum256(data)
	recomputed := hex.EncodeToString(sum[:])
	if artifact.SHA256 != recomputed {
		t.Fatalf("content-address integrity breach: ref SHA256 %q != recomputed %q of on-disk bytes", artifact.SHA256, recomputed)
	}

	// Invariant: the artifact object is machine-readable (a real serialized stage output),
	// so the consumption side can parse it rather than guess at a placeholder.
	var marker struct {
		Segments []struct {
			StartMs    int64   `json:"start_ms"`
			EndMs      int64   `json:"end_ms"`
			Text       string  `json:"text"`
			Confidence float64 `json:"confidence"`
		} `json:"segments"`
		ModelName    string `json:"model_name"`
		ModelVersion string `json:"model_version"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatalf("output artifact is not parseable JSON: %v", err)
	}
	if len(marker.Segments) == 0 {
		t.Fatalf("ASR artifact carries no transcript segments: %s", string(data))
	}
	for _, seg := range marker.Segments {
		if seg.EndMs <= seg.StartMs {
			t.Fatalf("ASR segment has non-positive time span: %+v", seg)
		}
		if seg.Text == "" {
			t.Fatalf("ASR segment carries no text: %+v", seg)
		}
	}
	if marker.ModelName != "qwen3-asr-1.7b" || marker.ModelVersion != "1.7b" {
		t.Fatalf("artifact model identity mismatch: got %s@%s", marker.ModelName, marker.ModelVersion)
	}
}
