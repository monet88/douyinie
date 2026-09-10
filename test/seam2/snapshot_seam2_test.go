package seam2_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
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
func TestSeam2_ModelSnapshot_OCR_MultiDependencyEnvelope(t *testing.T) {
	detDir := t.TempDir()
	recDir := t.TempDir()
	clsDir := t.TempDir()

	_ = os.WriteFile(filepath.Join(detDir, "inference.pdmodel"), []byte("det_model"), 0644)
	_ = os.WriteFile(filepath.Join(recDir, "inference.pdmodel"), []byte("rec_model"), 0644)
	_ = os.WriteFile(filepath.Join(clsDir, "inference.pdmodel"), []byte("cls_model"), 0644)

	ocrEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "det",
			DependencyName:         "PP-OCRv6_medium_det",
			Version:                "v6",
			SnapshotManifestSHA256: "det1122334455667788",
			LocalPath:              detDir,
		},
		Dependencies: []worker.ModelSnapshotRef{
			{
				Role:                   "rec",
				DependencyName:         "PP-OCRv6_medium_rec",
				Version:                "v6",
				SnapshotManifestSHA256: "rec1122334455667788",
				LocalPath:              recDir,
			},
			{
				Role:                   "ori",
				DependencyName:         "PP-LCNet_x1_0_textline_ori",
				Version:                "v1",
				SnapshotManifestSHA256: "ori1122334455667788",
				LocalPath:              clsDir,
			},
		},
	}

	// 1. Envelope validation succeeds
	cmd := worker.Command{
		ID:        "cmd-ocr-snap",
		Family:    "ocr",
		Stage:     "ocr",
		AttemptID: "att-ocr-1",
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: ocrEnv,
		},
	}
	b, _ := json.Marshal(cmd)
	env := worker.Envelope{
		Type:    worker.MessageTypeCommand,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: b,
	}
	if err := worker.ValidateEnvelope(env); err != nil {
		t.Fatalf("expected valid OCR multi-dependency envelope to pass validation, got: %v", err)
	}

	// 2. Validate snapshot paths succeeds
	if err := worker.ValidateSnapshotPaths(&ocrEnv); err != nil {
		t.Fatalf("expected valid paths to pass, got: %v", err)
	}

	// 3. If any dependency path is missing, ValidateSnapshotPaths fails closed
	badEnv := ocrEnv
	badEnv.Dependencies = []worker.ModelSnapshotRef{
		badEnv.Dependencies[0],
		{
			Role:                   "ori",
			DependencyName:         "PP-LCNet_x1_0_textline_ori",
			Version:                "v1",
			SnapshotManifestSHA256: "ori1122334455667788",
			LocalPath:              filepath.Join(t.TempDir(), "nonexistent_cls"),
		},
	}
	if err := worker.ValidateSnapshotPaths(&badEnv); err == nil {
		t.Fatalf("expected missing dependency snapshot path to fail closed")
	}
}

func TestSeam2_ModelSnapshot_TTS_VoiceAssetAndCheckpointValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Create Kokoro snapshot directory with voice assets
	kokoroDir := filepath.Join(tmpDir, "kokoro")
	voicesDir := filepath.Join(kokoroDir, "voices")
	if err := os.MkdirAll(voicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	voicePath := filepath.Join(voicesDir, "af_heart.pt")
	if err := os.WriteFile(voicePath, []byte("kokoro_voice_af_heart"), 0644); err != nil {
		t.Fatal(err)
	}

	kokoroEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "hexgrad/Kokoro-82M",
			Version:                "v1.0",
			SnapshotManifestSHA256: "kokoro_sha256_snapshot_manifest",
			LocalPath:              kokoroDir,
			EntrypointFile:         voicePath,
		},
		EntrypointFile: voicePath,
	}

	cmd := worker.Command{
		ID:        "cmd-tts-snap-1",
		Family:    "tts",
		Stage:     "tts",
		AttemptID: "att-tts-1",
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: kokoroEnv,
			"text":                        "Hello world from Kokoro",
			"language":                    "en",
			"voice_id":                    "af_heart",
			"model_name":                  "hexgrad/Kokoro-82M",
			"model_version":               "v1.0",
		},
	}
	b, _ := json.Marshal(cmd)
	env := worker.Envelope{
		Type:    worker.MessageTypeCommand,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: b,
	}

	// Envelope validation succeeds
	if err := worker.ValidateEnvelope(env); err != nil {
		t.Fatalf("expected valid TTS envelope to pass validation, got: %v", err)
	}

	// Snapshot paths validation succeeds
	if err := worker.ValidateSnapshotPaths(&kokoroEnv); err != nil {
		t.Fatalf("expected valid Kokoro snapshot paths to pass, got: %v", err)
	}

	// 2. Missing entrypoint voice file fails closed
	badKokoroEnv := kokoroEnv
	badKokoroEnv.Primary.EntrypointFile = filepath.Join(kokoroDir, "voices", "missing.pt")
	badKokoroEnv.EntrypointFile = badKokoroEnv.Primary.EntrypointFile
	if err := worker.ValidateSnapshotPaths(&badKokoroEnv); err == nil {
		t.Fatalf("expected missing entrypoint voice file to fail closed")
	}

	// 3. VieNeu snapshot directory validation
	vieneuDir := filepath.Join(tmpDir, "vieneu")
	if err := os.MkdirAll(vieneuDir, 0755); err != nil {
		t.Fatal(err)
	}
	vieneuEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "pnnbao-ump/VieNeu-TTS-v3-Turbo",
			Version:                "v3.2.9",
			SnapshotManifestSHA256: "vieneu_sha256_snapshot_manifest",
			LocalPath:              vieneuDir,
		},
	}
	if err := worker.ValidateSnapshotPaths(&vieneuEnv); err != nil {
		t.Fatalf("expected valid VieNeu snapshot paths to pass, got: %v", err)
	}
}

func TestSeam2_ModelSnapshot_Separator_AssetAndCheckpointValidation(t *testing.T) {
	tmpDir := t.TempDir()
	uvrDir := filepath.Join(tmpDir, "uvr")
	if err := os.MkdirAll(uvrDir, 0755); err != nil {
		t.Fatal(err)
	}
	uvrFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if err := os.WriteFile(uvrFile, []byte("valid_onnx_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	uvrEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "UVR-MDX-NET-Inst_HQ_4.onnx",
			Version:                "v3",
			SnapshotManifestSHA256: "uvr_manifest_sha",
			LocalPath:              uvrDir,
			EntrypointFile:         uvrFile,
		},
		EntrypointFile: uvrFile,
	}

	if err := worker.ValidateSnapshotPaths(&uvrEnv); err != nil {
		t.Fatalf("expected valid UVR snapshot paths to pass, got: %v", err)
	}

	// Missing entrypoint file fails ValidateSnapshotPaths
	badUVREnv := uvrEnv
	badUVREnv.Primary.EntrypointFile = filepath.Join(uvrDir, "missing.onnx")
	badUVREnv.EntrypointFile = badUVREnv.Primary.EntrypointFile
	if err := worker.ValidateSnapshotPaths(&badUVREnv); err == nil {
		t.Fatalf("expected missing UVR entrypoint to fail ValidateSnapshotPaths")
	}

	// Demucs fallback envelope validation
	demucsDir := filepath.Join(tmpDir, "demucs")
	if err := os.MkdirAll(demucsDir, 0755); err != nil {
		t.Fatal(err)
	}
	demucsFile := filepath.Join(demucsDir, "955717e8-8726e21a.th")
	if err := os.WriteFile(demucsFile, []byte("valid_th_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	demucsEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "htdemucs",
			Version:                "v4",
			SnapshotManifestSHA256: "demucs_manifest_sha",
			LocalPath:              demucsDir,
			EntrypointFile:         demucsFile,
		},
		EntrypointFile: demucsFile,
	}
	if err := worker.ValidateSnapshotPaths(&demucsEnv); err != nil {
		t.Fatalf("expected valid Demucs snapshot paths to pass, got: %v", err)
	}

	// Complete Demucs local repo layout with htdemucs.yaml bag definition
	yamlFile := filepath.Join(demucsDir, "htdemucs.yaml")
	if err := os.WriteFile(yamlFile, []byte("models: ['955717e8']\n"), 0644); err != nil {
		t.Fatal(err)
	}
	demucsCompleteEnv := demucsEnv
	demucsCompleteEnv.Dependencies = []worker.ModelSnapshotRef{
		{
			Role:                   "bag_definition",
			DependencyName:         "htdemucs.yaml",
			Version:                "v4",
			SnapshotManifestSHA256: "bag_manifest_sha",
			LocalPath:              demucsDir,
			EntrypointFile:         yamlFile,
		},
	}
	if err := worker.ValidateSnapshotPaths(&demucsCompleteEnv); err != nil {
		t.Fatalf("expected complete Demucs snapshot with htdemucs.yaml to pass ValidateSnapshotPaths, got: %v", err)
	}
}

func TestSeam2_TTSStage_EnvelopeEntrypointOverrideProtection(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	tmpDir := t.TempDir()
	vieneuDir := filepath.Join(tmpDir, "vieneu")
	catDir := filepath.Join(vieneuDir, "src", "vieneu", "assets")
	if err := os.MkdirAll(catDir, 0755); err != nil {
		t.Fatal(err)
	}
	catFile := filepath.Join(catDir, "voices_v3_turbo.json")
	if err := os.WriteFile(catFile, []byte(`{"presets": {"Trúc Ly": {"id": "Trúc Ly"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(vieneuDir, "moss_tokenizer"), 0755); err != nil {
		t.Fatal(err)
	}

	validEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "pnnbao-ump/VieNeu-TTS-v3-Turbo",
			Version:                "v3.2.9",
			SnapshotManifestSHA256: "vieneu_sha256_snapshot_manifest",
			LocalPath:              vieneuDir,
			EntrypointFile:         catFile,
		},
		EntrypointFile: catFile,
	}

	// 1. Injected top-level entrypoint_file attempting unverified override fails closed
	outPath1 := filepath.Join(tmpDir, "out-override.json")
	cmdOverride := worker.Command{
		ID:         "cmd-tts-override",
		Family:     "tts",
		Stage:      "tts",
		AttemptID:  "att-tts-override",
		RunID:      "run-tts-override",
		OutputPath: outPath1,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: validEnv,
			"text":                        "Xin chào Việt Nam",
			"model_name":                  "pnnbao-ump/VieNeu-TTS-v3-Turbo",
			"model_version":               "v3.2.9",
			"language":                    "vi",
			"voice_id":                    "Trúc Ly",
			"speed":                       "1.0",
			"entrypoint_file":             filepath.Join(tmpDir, "unverified_override.json"),
		},
	}
	_, err := client.Run(context.Background(), cmdOverride, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected injected top-level entrypoint_file override to fail closed")
	}
	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") && !strings.Contains(err.Error(), "attempts unverified override") {
		t.Fatalf("expected error indicating unverified override, got: %v", err)
	}

	// 2. Missing envelope entrypoint for VieNeu hard route fails closed
	outPath2 := filepath.Join(tmpDir, "out-no-ep.json")
	noEpEnv := validEnv
	noEpEnv.Primary.EntrypointFile = ""
	noEpEnv.EntrypointFile = ""
	cmdNoEp := worker.Command{
		ID:         "cmd-tts-no-ep",
		Family:     "tts",
		Stage:      "tts",
		AttemptID:  "att-tts-no-ep",
		RunID:      "run-tts-no-ep",
		OutputPath: outPath2,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: noEpEnv,
			"text":                        "Xin chào Việt Nam",
			"model_name":                  "pnnbao-ump/VieNeu-TTS-v3-Turbo",
			"model_version":               "v3.2.9",
			"language":                    "vi",
			"voice_id":                    "Trúc Ly",
			"speed":                       "1.0",
		},
	}
	_, err = client.Run(context.Background(), cmdNoEp, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected missing envelope entrypoint to fail closed for VieNeu")
	}
	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED for missing envelope entrypoint, got: %v", err)
	}

	// 3. Alternate catalog path in envelope fails closed
	altCatDir := filepath.Join(vieneuDir, "assets")
	_ = os.MkdirAll(altCatDir, 0755)
	altCatFile := filepath.Join(altCatDir, "voices_v3_turbo.json")
	_ = os.WriteFile(altCatFile, []byte(`{"presets": {"Trúc Ly": {"id": "Trúc Ly"}}}`), 0644)

	outPath3 := filepath.Join(tmpDir, "out-alt-ep.json")
	altEpEnv := validEnv
	altEpEnv.Primary.EntrypointFile = altCatFile
	altEpEnv.EntrypointFile = altCatFile
	cmdAltEp := worker.Command{
		ID:         "cmd-tts-alt-ep",
		Family:     "tts",
		Stage:      "tts",
		AttemptID:  "att-tts-alt-ep",
		RunID:      "run-tts-alt-ep",
		OutputPath: outPath3,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: altEpEnv,
			"text":                        "Xin chào Việt Nam",
			"model_name":                  "pnnbao-ump/VieNeu-TTS-v3-Turbo",
			"model_version":               "v3.2.9",
			"language":                    "vi",
			"voice_id":                    "Trúc Ly",
			"speed":                       "1.0",
		},
	}
	_, err = client.Run(context.Background(), cmdAltEp, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected alternate catalog entrypoint in envelope to fail closed for VieNeu")
	}
	if !strings.Contains(err.Error(), "TTS_VOICE_ASSET_MISSING") {
		t.Fatalf("expected TTS_VOICE_ASSET_MISSING for alternate catalog entrypoint, got: %v", err)
	}
}

func TestSeam2_SeparatorStage_EnvelopeEntrypointOverrideProtection(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	tmpDir := t.TempDir()
	uvrDir := filepath.Join(tmpDir, "uvr")
	if err := os.MkdirAll(uvrDir, 0755); err != nil {
		t.Fatal(err)
	}
	uvrFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if err := os.WriteFile(uvrFile, []byte("onnx_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	validEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "UVR-MDX-NET-Inst_HQ_4.onnx",
			Version:                "v3",
			SnapshotManifestSHA256: "uvr_sha256_snapshot_manifest",
			LocalPath:              uvrDir,
			EntrypointFile:         uvrFile,
		},
		EntrypointFile: uvrFile,
	}

	audioFile := filepath.Join(tmpDir, "input.wav")
	if err := os.WriteFile(audioFile, []byte("RIFF1234WAVEfmt ...."), 0644); err != nil {
		t.Fatal(err)
	}
	inArt := worker.ArtifactRef{
		Path: audioFile,
	}

	// 1. Injected top-level entrypoint_file attempting unverified override fails closed
	outPath1 := filepath.Join(tmpDir, "out-override.json")
	cmdOverride := worker.Command{
		ID:         "cmd-sep-override",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "att-sep-override",
		RunID:      "run-sep-override",
		Inputs:     []worker.ArtifactRef{inArt},
		OutputPath: outPath1,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: validEnv,
			"model_name":                  "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version":               "v3",
			"entrypoint_file":             filepath.Join(tmpDir, "unverified_override.onnx"),
		},
	}
	_, err := client.Run(context.Background(), cmdOverride, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected injected top-level entrypoint_file override to fail closed")
	}
	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") && !strings.Contains(err.Error(), "attempts unverified override") {
		t.Fatalf("expected error indicating unverified override, got: %v", err)
	}

	// 2. Missing envelope when require_model_snapshot is true fails closed
	outPath2 := filepath.Join(tmpDir, "out-no-env.json")
	cmdNoEnv := worker.Command{
		ID:         "cmd-sep-no-env",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "att-sep-no-env",
		RunID:      "run-sep-no-env",
		Inputs:     []worker.ArtifactRef{inArt},
		OutputPath: outPath2,
		Config: map[string]any{
			"model_name":             "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version":          "v3",
			"require_model_snapshot": true,
		},
	}
	_, err = client.Run(context.Background(), cmdNoEnv, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected missing envelope when require_model_snapshot is true to fail closed")
	}
	if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED, got: %v", err)
	}

	// 3. Demucs htdemucs_ft rejection
	outPath3 := filepath.Join(tmpDir, "out-ft.json")
	cmdFT := worker.Command{
		ID:         "cmd-sep-ft",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "att-sep-ft",
		RunID:      "run-sep-ft",
		Inputs:     []worker.ArtifactRef{inArt},
		OutputPath: outPath3,
		Config: map[string]any{
			"model_name":    "htdemucs_ft",
			"model_version": "v4",
		},
	}
	_, err = client.Run(context.Background(), cmdFT, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatalf("expected htdemucs_ft to fail closed")
	}
	if !strings.Contains(err.Error(), "DEMUCS_FT_SUBSTITUTION_REJECTED") {
		t.Fatalf("expected DEMUCS_FT_SUBSTITUTION_REJECTED, got: %v", err)
	}
}

func TestSeam2_SeparatorStage_PreCorpusSmoke_Execution(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup synthetic input WAV
	audioPath := filepath.Join(tmpDir, "pre_corpus_input.wav")
	dummyWAV := []byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	if err := os.WriteFile(audioPath, dummyWAV, 0644); err != nil {
		t.Fatalf("write audio file: %v", err)
	}

	// UVR local snapshot directory with canonical entrypoint
	uvrDir := filepath.Join(tmpDir, "uvr_snapshot")
	if err := os.MkdirAll(uvrDir, 0755); err != nil {
		t.Fatal(err)
	}
	uvrFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if err := os.WriteFile(uvrFile, []byte("onnx_model_payload"), 0644); err != nil {
		t.Fatal(err)
	}
	mdxFile := filepath.Join(uvrDir, "mdx_model_data.json")
	if err := os.WriteFile(mdxFile, []byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals", "compensate": 1.035}}`), 0644); err != nil {
		t.Fatal(err)
	}
	uvrEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "UVR-MDX-NET-Inst_HQ_4.onnx",
			Version:                "v3",
			SnapshotManifestSHA256: "uvr_manifest_sha",
			LocalPath:              uvrDir,
			EntrypointFile:         uvrFile,
		},
		Dependencies: []worker.ModelSnapshotRef{
			{
				Role:                   "model_metadata",
				DependencyName:         "mdx_model_data.json",
				Version:                "v3",
				SnapshotManifestSHA256: "mdx_manifest_sha",
				LocalPath:              uvrDir,
				EntrypointFile:         mdxFile,
			},
		},
		EntrypointFile: uvrFile,
	}
	// Demucs local snapshot directory with complete local repo layout (checkpoint + htdemucs.yaml)
	demucsDir := filepath.Join(tmpDir, "demucs_snapshot")
	if err := os.MkdirAll(demucsDir, 0755); err != nil {
		t.Fatal(err)
	}
	demucsFile := filepath.Join(demucsDir, "955717e8-8726e21a.th")
	if err := os.WriteFile(demucsFile, []byte("demucs_model_payload"), 0644); err != nil {
		t.Fatal(err)
	}
	yamlFile := filepath.Join(demucsDir, "htdemucs.yaml")
	if err := os.WriteFile(yamlFile, []byte("models: ['955717e8']\n"), 0644); err != nil {
		t.Fatal(err)
	}
	demucsEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "htdemucs",
			Version:                "v4",
			SnapshotManifestSHA256: "demucs_manifest_sha",
			LocalPath:              demucsDir,
			EntrypointFile:         demucsFile,
		},
		Dependencies: []worker.ModelSnapshotRef{
			{
				Role:                   "bag_definition",
				DependencyName:         "htdemucs.yaml",
				Version:                "v4",
				SnapshotManifestSHA256: "bag_manifest_sha",
				LocalPath:              demucsDir,
				EntrypointFile:         yamlFile,
			},
		},
		EntrypointFile: demucsFile,
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "separator.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "separator.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("separator.py adapter not found at %s", adapterPath)
	}

	// Wrapper mock script to provide deterministic factory output while exercising the real separator.py main() and envelope validation
	wrapperPath := filepath.Join(tmpDir, "mock_smoke_separator.py")
	wrapperCode := `
import sys
import os
import io
import wave
import struct
import json

adapter_dir = os.path.dirname(r'` + adapterPath + `')
sys.path.insert(0, adapter_dir)
import separator

def smoke_factory(audio_path, model_name, model_version, model_path=None, entrypoint_file=None, require_model_snapshot=False):
    # Verify snapshot parameters are preserved and accessible
    if require_model_snapshot:
        assert model_path and os.path.exists(model_path)
        assert entrypoint_file and os.path.exists(entrypoint_file)
    num_samples = int((16000 * 3000) / 1000)
    buf = io.BytesIO()
    with wave.open(buf, 'wb') as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(16000)
        wf.writeframes(struct.pack(f'<{num_samples}h', *([100] * num_samples)))
    wav_data = buf.getvalue()
    resolved_name = "htdemucs" if "demucs" in model_name.lower() else separator.UVR_CANONICAL_FILENAME
    rt_ident = "demucs 4.1.0a2" if "demucs" in model_name.lower() else "python-audio-separator 0.47.0"
    return {
        'vocals_data': wav_data,
        'background_data': wav_data,
        'duration_ms': 3000,
        'sample_rate': 16000,
        'channels': 1,
        'model_name': resolved_name,
        'model_version': model_version,
        'runtime_identity': rt_ident,
    }

separator._SEPARATOR_MODEL_FACTORY = smoke_factory
def smoke_probe_factory(model_name, model_version):
    if "demucs" in model_name.lower():
        return {
            "status": "ok",
            "package_name": "demucs",
            "package_version": separator.PINNED_DEMUCS_VERSION,
            "source_revision": separator.PINNED_DEMUCS_COMMIT,
            "runtime_versions": {"demucs": separator.PINNED_DEMUCS_VERSION, "torch": "2.2.0"},
            "adapter_revision": f"cmd/stageworker/adapters/separator.py@v{separator.PINNED_DEMUCS_VERSION}"
        }
    return {
        "status": "ok",
        "package_name": "audio-separator",
        "package_version": separator.PINNED_AUDIO_SEPARATOR_VERSION,
        "source_revision": separator.PINNED_AUDIO_SEPARATOR_COMMIT,
        "runtime_versions": {"audio-separator": separator.PINNED_AUDIO_SEPARATOR_VERSION, "onnxruntime": "1.19.0"},
        "adapter_revision": f"cmd/stageworker/adapters/separator.py@v{separator.PINNED_AUDIO_SEPARATOR_VERSION}"
    }
separator._SEPARATOR_PROBE_FACTORY = smoke_probe_factory
if __name__ == '__main__':
    separator.main()
`
	if err := os.WriteFile(wrapperPath, []byte(wrapperCode), 0644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	var pyLauncher string
	if runtime.GOOS == "windows" {
		pyLauncher = filepath.Join(tmpDir, "smoke_python_runner.bat")
		batContent := fmt.Sprintf("@echo off\npython -u %q %%*\n", wrapperPath)
		if err := os.WriteFile(pyLauncher, []byte(batContent), 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		pyLauncher = filepath.Join(tmpDir, "smoke_python_runner.sh")
		shContent := fmt.Sprintf("#!/bin/sh\npython3 -u %q \"$@\"\n", wrapperPath)
		if err := os.WriteFile(pyLauncher, []byte(shContent), 0755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", pyLauncher)
	t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
	t.Setenv("DOUYINIE_SEPARATOR_BIN", "")
	t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "")
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()
	if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	defer sup.Terminate()

	client := worker.NewClient(sup)
	if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// 1. UVR Pre-Corpus Smoke Execution
	outUVR := filepath.Join(tmpDir, "smoke_uvr_out.json")
	cmdUVR := worker.Command{
		ID:         "cmd-smoke-uvr",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "att-smoke-uvr",
		RunID:      "run-smoke-uvr",
		OutputPath: outUVR,
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: uvrEnv,
			"model_name":                  "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version":               "v3",
			"require_model_snapshot":      true,
		},
	}
	artUVR, err := client.Run(ctx, cmdUVR, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("UVR pre-corpus smoke execution failed: %v", err)
	}
	if artUVR.SHA256 == "" {
		t.Fatalf("expected real SHA256 for UVR smoke output")
	}
	dataUVR, err := os.ReadFile(outUVR)
	if err != nil {
		t.Fatal(err)
	}
	var resUVR struct {
		VocalsData      string `json:"vocals_data"`
		BackgroundData  string `json:"background_data"`
		DurationMs      int64  `json:"duration_ms"`
		ModelName       string `json:"model_name"`
		RuntimeIdentity string `json:"runtime_identity"`
	}
	if err := json.Unmarshal(dataUVR, &resUVR); err != nil {
		t.Fatal(err)
	}
	if resUVR.VocalsData == "" || resUVR.BackgroundData == "" || resUVR.DurationMs != 3000 {
		t.Fatalf("unexpected UVR smoke output: %+v", resUVR)
	}
	if resUVR.ModelName != "UVR-MDX-NET-Inst_HQ_4.onnx" {
		t.Errorf("expected model name UVR-MDX-NET-Inst_HQ_4.onnx, got %s", resUVR.ModelName)
	}
	if !strings.Contains(resUVR.RuntimeIdentity, "python-audio-separator") {
		t.Errorf("expected python-audio-separator in runtime identity, got %s", resUVR.RuntimeIdentity)
	}
	if strings.Contains(resUVR.RuntimeIdentity, "@") {
		t.Errorf("StageWorker must not invent source commit provenance, got %s", resUVR.RuntimeIdentity)
	}
	outDemucs := filepath.Join(tmpDir, "smoke_demucs_out.json")
	cmdDemucs := worker.Command{
		ID:         "cmd-smoke-demucs",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "att-smoke-demucs",
		RunID:      "run-smoke-demucs",
		OutputPath: outDemucs,
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: demucsEnv,
			"model_name":                  "htdemucs",
			"model_version":               "v4",
			"require_model_snapshot":      true,
		},
	}
	artDemucs, err := client.Run(ctx, cmdDemucs, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("Demucs pre-corpus smoke execution failed: %v", err)
	}
	if artDemucs.SHA256 == "" {
		t.Fatalf("expected real SHA256 for Demucs smoke output")
	}
	dataDemucs, err := os.ReadFile(outDemucs)
	if err != nil {
		t.Fatal(err)
	}
	var resDemucs struct {
		VocalsData      string `json:"vocals_data"`
		BackgroundData  string `json:"background_data"`
		DurationMs      int64  `json:"duration_ms"`
		ModelName       string `json:"model_name"`
		RuntimeIdentity string `json:"runtime_identity"`
	}
	if err := json.Unmarshal(dataDemucs, &resDemucs); err != nil {
		t.Fatal(err)
	}
	if resDemucs.VocalsData == "" || resDemucs.BackgroundData == "" || resDemucs.DurationMs != 3000 {
		t.Fatalf("unexpected Demucs smoke output: %+v", resDemucs)
	}
	if resDemucs.ModelName != "htdemucs" {
		t.Errorf("expected model name htdemucs, got %s", resDemucs.ModelName)
	}
	if !strings.Contains(resDemucs.RuntimeIdentity, "demucs") {
		t.Errorf("expected demucs in runtime identity, got %s", resDemucs.RuntimeIdentity)
	}
	if strings.Contains(resDemucs.RuntimeIdentity, "@") {
		t.Errorf("StageWorker must not invent source commit provenance, got %s", resDemucs.RuntimeIdentity)
	}
	// 3. StageWorker separator runtime probe command (Seam 2 runtime-evidence bootstrap)
	outProbe := filepath.Join(tmpDir, "smoke_probe_out.json")
	cmdProbe := worker.Command{
		ID:         "cmd-smoke-probe",
		Family:     "separator",
		Stage:      "separator_probe",
		AttemptID:  "att-smoke-probe",
		RunID:      "run-smoke-probe",
		OutputPath: outProbe,
		Config: map[string]any{
			"model_name":    "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version": "v3",
			"mode":          "probe",
		},
	}
	artProbe, err := client.Run(ctx, cmdProbe, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("StageWorker separator_probe run failed: %v", err)
	}
	dataProbe, err := os.ReadFile(artProbe.Path)
	if err != nil {
		t.Fatalf("failed to read probe output: %v", err)
	}
	var resProbe struct {
		Status          string            `json:"status"`
		PackageName     string            `json:"package_name"`
		PackageVersion  string            `json:"package_version"`
		SourceRevision  string            `json:"source_revision"`
		RuntimeVersions map[string]string `json:"runtime_versions"`
	}
	if err := json.Unmarshal(dataProbe, &resProbe); err != nil {
		t.Fatalf("unmarshal probe artifact: %v", err)
	}
	if resProbe.Status != "ok" || resProbe.PackageName != "audio-separator" {
		t.Fatalf("unexpected probe status/pkg: %+v", resProbe)
	}
	if resProbe.SourceRevision != domain.PinnedUVRSourceRevision {
		t.Errorf("expected source revision %s, got %s", domain.PinnedUVRSourceRevision, resProbe.SourceRevision)
	}
	if resProbe.PackageVersion != domain.PinnedUVRPackageVersion {
		t.Errorf("expected package version %s, got %s", domain.PinnedUVRPackageVersion, resProbe.PackageVersion)
	}
}

func TestSeam2_SeparatorStage_ArbitraryRunnerOverride_RejectedOnRCHardRoute(t *testing.T) {
	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "input.wav")
	if err := os.WriteFile(audioPath, []byte("fake_audio_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	// Arbitrary custom script that attempts to self-attest pinned RC provenance
	badScript := filepath.Join(tmpDir, "arbitrary_fake_separator.py")
	badCode := `import sys, json
out = {
    "status": "ok",
    "package_name": "audio-separator",
    "package_version": "0.47.0",
    "source_revision": "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab",
    "runtime_versions": {"audio-separator": "0.47.0"},
    "adapter_revision": "cmd/stageworker/adapters/separator.py@v0.47.0"
}
print(json.dumps(out))
sys.exit(0)
`
	if err := os.WriteFile(badScript, []byte(badCode), 0644); err != nil {
		t.Fatal(err)
	}

	exe := buildStageWorker(t)

	t.Run("ArbitraryAdapter_Rejected_EvenIfTrustedHashEnvSet", func(t *testing.T) {
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", badScript)
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

		sup := worker.NewSupervisor()
		ctx := context.Background()
		if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
			t.Fatalf("spawn worker: %v", err)
		}
		defer sup.Terminate()

		client := worker.NewClient(sup)
		if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
			t.Fatalf("handshake: %v", err)
		}

		cmdProbe := worker.Command{
			ID:         "cmd-probe-arbitrary",
			Family:     "separator",
			Stage:      "separator_probe",
			AttemptID:  "att-probe-arbitrary",
			OutputPath: filepath.Join(tmpDir, "out_probe_bad.json"),
			Config: map[string]any{
				"model_name":             "UVR-MDX-NET-Inst_HQ_4.onnx",
				"model_version":          "v3",
				"mode":                   "probe",
				"require_model_snapshot": true,
			},
		}
		_, err := client.Run(ctx, cmdProbe, 5*time.Second, 5*time.Second)
		if err == nil {
			t.Fatal("expected arbitrary separator adapter to be rejected on RC hard route, got nil error")
		}
		if !strings.Contains(err.Error(), "SEPARATOR_RUNNER_OVERRIDE_REJECTED") {
			t.Fatalf("expected SEPARATOR_RUNNER_OVERRIDE_REJECTED in error, got: %v", err)
		}
	})

	t.Run("ArbitraryBinary_Rejected_EvenIfTrustedHashEnvSet", func(t *testing.T) {
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
		t.Setenv("DOUYINIE_SEPARATOR_BIN", "echo")
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

		sup := worker.NewSupervisor()
		ctx := context.Background()
		if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
			t.Fatalf("spawn worker: %v", err)
		}
		defer sup.Terminate()

		client := worker.NewClient(sup)
		if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
			t.Fatalf("handshake: %v", err)
		}

		cmdProbe := worker.Command{
			ID:         "cmd-probe-arbitrary-bin",
			Family:     "separator",
			Stage:      "separator_probe",
			AttemptID:  "att-probe-arbitrary-bin",
			OutputPath: filepath.Join(tmpDir, "out_probe_bad_bin.json"),
			Config: map[string]any{
				"model_name":             "UVR-MDX-NET-Inst_HQ_4.onnx",
				"model_version":          "v3",
				"mode":                   "probe",
				"require_model_snapshot": true,
			},
		}
		_, err := client.Run(ctx, cmdProbe, 5*time.Second, 5*time.Second)
		if err == nil {
			t.Fatal("expected arbitrary separator binary to be rejected on RC hard route, got nil error")
		}
		if !strings.Contains(err.Error(), "SEPARATOR_RUNNER_OVERRIDE_REJECTED") {
			t.Fatalf("expected SEPARATOR_RUNNER_OVERRIDE_REJECTED in error, got: %v", err)
		}
	})

	t.Run("RepoAdapter_WithConfiguredPythonPath_SucceedsOnRCHardRoute", func(t *testing.T) {
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
		t.Setenv("DOUYINIE_SEPARATOR_BIN", "")
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "")

		pyInterp := filepath.Join(tmpDir, "valid_probe_interp.py")
		script := fmt.Sprintf(`import sys, json
out = {
    "status": "ok",
    "package_name": "audio-separator",
    "package_version": "0.47.0",
    "source_revision": "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab",
    "runtime_versions": {"audio-separator": "0.47.0", "onnxruntime": "1.19.0"},
    "adapter_revision": "cmd/stageworker/adapters/separator.py@v0.47.0"
}
print(json.dumps(out))
sys.exit(0)
`)
		if err := os.WriteFile(pyInterp, []byte(script), 0755); err != nil {
			t.Fatal(err)
		}

		var pyLauncher string
		if runtime.GOOS == "windows" {
			pyLauncher = filepath.Join(tmpDir, "valid_seam2_runner.bat")
			_ = os.WriteFile(pyLauncher, []byte(fmt.Sprintf("@echo off\npython -u %q %%*\n", pyInterp)), 0755)
		} else {
			pyLauncher = filepath.Join(tmpDir, "valid_seam2_runner.sh")
			_ = os.WriteFile(pyLauncher, []byte(fmt.Sprintf("#!/bin/sh\npython3 -u %q \"$@\"\n", pyInterp)), 0755)
		}
		t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", pyLauncher)

		sup := worker.NewSupervisor()
		ctx := context.Background()
		if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
			t.Fatalf("spawn worker: %v", err)
		}
		defer sup.Terminate()

		client := worker.NewClient(sup)
		if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
			t.Fatalf("handshake: %v", err)
		}

		cmdProbe := worker.Command{
			ID:         "cmd-probe-valid-repo",
			Family:     "separator",
			Stage:      "separator_probe",
			AttemptID:  "att-probe-valid-repo",
			OutputPath: filepath.Join(tmpDir, "out_probe_valid.json"),
			Config: map[string]any{
				"model_name":             "UVR-MDX-NET-Inst_HQ_4.onnx",
				"model_version":          "v3",
				"mode":                   "probe",
				"require_model_snapshot": true,
			},
		}
		art, err := client.Run(ctx, cmdProbe, 5*time.Second, 5*time.Second)
		if err != nil {
			t.Fatalf("expected repo adapter with configured python path to succeed, got: %v", err)
		}
		if art.SHA256 == "" {
			t.Fatalf("expected valid artifact ref from probe")
		}
	})
}

func TestSeam2_SeparatorStage_RealCheckpoint_PreCorpusSmoke(t *testing.T) {
	// Explicit test covering genuine pre-corpus smoke execution with physical weights when available.
	// Skipped cleanly if model checkpoints are unavailable in the test environment.
	modelDir := os.Getenv("AUDIO_SEPARATOR_MODEL_DIR")
	if modelDir == "" {
		t.Skip("skipping real checkpoint smoke: AUDIO_SEPARATOR_MODEL_DIR not configured")
	}
	if _, err := os.Stat(modelDir); err != nil {
		t.Skipf("skipping real checkpoint smoke: model dir %s does not exist", modelDir)
	}

	uvrFile := filepath.Join(modelDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if _, err := os.Stat(uvrFile); err != nil {
		t.Skipf("skipping real checkpoint smoke: UVR model file %s does not exist", uvrFile)
	}

	pyBin := os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN")
	if pyBin == "" {
		pyBin = os.Getenv("DOUYINIE_PYTHON_BIN")
	}
	if pyBin == "" {
		t.Skip("skipping real checkpoint smoke: no DOUYINIE_SEPARATOR_PYTHON_BIN or DOUYINIE_PYTHON_BIN set")
	}

	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "input.wav")
	dummyWAV := []byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	if err := os.WriteFile(audioPath, dummyWAV, 0644); err != nil {
		t.Fatal(err)
	}

	outUVR := filepath.Join(tmpDir, "real_uvr_out.json")
	uvrEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "UVR-MDX-NET-Inst_HQ_4.onnx",
			Version:                "v3",
			SnapshotManifestSHA256: "real_uvr_manifest_sha",
			LocalPath:              modelDir,
			EntrypointFile:         uvrFile,
		},
		EntrypointFile: uvrFile,
	}

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()
	if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	defer sup.Terminate()

	client := worker.NewClient(sup)
	if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	cmd := worker.Command{
		ID:         "cmd-real-smoke-uvr",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "att-real-smoke-uvr",
		RunID:      "run-real-smoke-uvr",
		OutputPath: outUVR,
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: uvrEnv,
			"model_name":                  "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version":               "v3",
			"require_model_snapshot":      true,
		},
	}
	art, err := client.Run(ctx, cmd, 60*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("real checkpoint UVR smoke failed: %v", err)
	}
	if art.SHA256 == "" {
		t.Fatalf("expected real SHA256 for real UVR smoke output")
	}
}

func TestSeam2_DiarizerStage_SnapshotEnvelopeValidation(t *testing.T) {
	cacheRoot := t.TempDir()
	campplusDir := filepath.Join(cacheRoot, "iic", "speech_campplus_sv_zh_en_16k-common_advanced")
	vadDir := filepath.Join(cacheRoot, "iic", "speech_fsmn_vad_zh-cn-16k-common-pytorch")

	if err := os.MkdirAll(campplusDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vadDir, 0755); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(filepath.Join(campplusDir, "configuration.json"), []byte(`{"model_type": "campplus"}`), 0644)
	_ = os.WriteFile(filepath.Join(campplusDir, "campplus_cn_en_common.pt"), []byte("campplus_weights"), 0644)
	_ = os.WriteFile(filepath.Join(vadDir, "configuration.json"), []byte(`{"model_type": "fsmn_vad"}`), 0644)
	_ = os.WriteFile(filepath.Join(vadDir, "model.pt"), []byte("vad_weights"), 0644)

	diarizerEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			Version:                "v1.0.0",
			SnapshotManifestSHA256: "campplus11223344",
			LocalPath:              campplusDir,
		},
		Dependencies: []worker.ModelSnapshotRef{
			{
				Role:                   "vad",
				DependencyName:         "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
				Version:                "v2.0.4",
				SnapshotManifestSHA256: "vad11223344",
				LocalPath:              vadDir,
			},
		},
	}
	// Setup mock python adapter that records arguments
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "speakerlab", "bin")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "speakerlab", "__init__.py"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(""), 0644)
	mockCode := `
class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs
    def __call__(self, audio_path):
        return [[0.0, 1.5, "SPEAKER_00"], [1.5, 3.2, "SPEAKER_01"]]
    def probe_evidence(self, audio_path):
        return {"has_multi_speaker_cues": True, "speaker_change_count": 2, "confidence": 0.0}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "infer_diarization.py"), []byte(mockCode), 0644)

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	}

	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", adapterPath)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio"), 0644)

	// 1. Missing required snapshot fails closed with WORKER_SNAPSHOT_PATH_REQUIRED
	cmdMissing := worker.Command{
		ID:        "cmd-diarize-req-missing",
		Family:    "diarizer",
		Stage:     "diarize",
		AttemptID: "att-1",
		RunID:     "run-1",
		Inputs:    []worker.ArtifactRef{{Path: audioPath}},
		Config: map[string]any{
			"model_name":             "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":          "v1.0.0",
			"require_model_snapshot": true,
		},
	}
	_, err = client.Run(context.Background(), cmdMissing, 5*time.Second, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected missing snapshot envelope to fail closed with WORKER_SNAPSHOT_PATH_REQUIRED, got: %v", err)
	}

	// 2. Unverified top-level override rejected with WORKER_SNAPSHOT_PATH_REQUIRED
	cmdOverride := worker.Command{
		ID:        "cmd-diarize-override",
		Family:    "diarizer",
		Stage:     "diarize",
		AttemptID: "att-2",
		RunID:     "run-2",
		Inputs:    []worker.ArtifactRef{{Path: audioPath}},
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: diarizerEnv,
			"model_name":                  "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":               "v1.0.0",
			"model_path":                  "/unverified/path",
			"require_model_snapshot":      true,
		},
	}
	_, err = client.Run(context.Background(), cmdOverride, 5*time.Second, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected unverified model_path override to fail closed with WORKER_SNAPSHOT_PATH_REQUIRED, got: %v", err)
	}

	// 3. Valid snapshot envelope executes successfully and reaches Python adapter
	outPath := filepath.Join(t.TempDir(), "out-diarize-snap.json")
	cmdValid := worker.Command{
		ID:         "cmd-diarize-snap-valid",
		Family:     "diarizer",
		Stage:      "diarize",
		AttemptID:  "att-3",
		RunID:      "run-3",
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		OutputPath: outPath,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: diarizerEnv,
			"model_name":                  "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":               "v1.0.0",
			"require_model_snapshot":      true,
		},
	}
	art, err := client.Run(context.Background(), cmdValid, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("expected valid snapshot envelope to execute successfully, got: %v", err)
	}
	if art.SHA256 == "" {
		t.Fatalf("expected real SHA-256 in artifact ref")
	}

	// 4. Same contract applies to diarize_evidence
	outEvidPath := filepath.Join(t.TempDir(), "out-evid-snap.json")
	cmdEvid := worker.Command{
		ID:         "cmd-evid-snap-valid",
		Family:     "diarizer",
		Stage:      "diarize_evidence",
		AttemptID:  "att-4",
		RunID:      "run-4",
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		OutputPath: outEvidPath,
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: diarizerEnv,
			"model_name":                  "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":               "v1.0.0",
			"embedding_cosine_threshold":  0.65,
			"require_model_snapshot":      true,
		},
	}
	artEvid, err := client.Run(context.Background(), cmdEvid, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("expected valid snapshot envelope in diarize_evidence to execute successfully, got: %v", err)
	}
	if artEvid.SHA256 == "" {
		t.Fatalf("expected real SHA-256 in evidence artifact ref")
	}
}

func TestSeam2_DiarizerStage_StrictModeRejectsConstructorFallback(t *testing.T) {
	cacheRoot := t.TempDir()
	campplusDir := filepath.Join(cacheRoot, "iic", "speech_campplus_sv_zh_en_16k-common_advanced")
	vadDir := filepath.Join(cacheRoot, "iic", "speech_fsmn_vad_zh-cn-16k-common-pytorch")

	if err := os.MkdirAll(campplusDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vadDir, 0755); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(filepath.Join(campplusDir, "configuration.json"), []byte(`{"model_type": "campplus"}`), 0644)
	_ = os.WriteFile(filepath.Join(campplusDir, "campplus_cn_en_common.pt"), []byte("campplus_weights"), 0644)
	_ = os.WriteFile(filepath.Join(vadDir, "configuration.json"), []byte(`{"model_type": "fsmn_vad"}`), 0644)
	_ = os.WriteFile(filepath.Join(vadDir, "model.pt"), []byte("vad_weights"), 0644)

	diarizerEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			Version:                "v1.0.0",
			SnapshotManifestSHA256: "campplus11223344",
			LocalPath:              campplusDir,
		},
		Dependencies: []worker.ModelSnapshotRef{
			{
				Role:                   "vad",
				DependencyName:         "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
				Version:                "v2.0.4",
				SnapshotManifestSHA256: "vad11223344",
				LocalPath:              vadDir,
			},
		},
	}

	// Mock python adapter where Diarization3Dspeaker accepts NO kwargs (raises TypeError if model_cache_dir is passed)
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "speakerlab", "bin")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "speakerlab", "__init__.py"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(""), 0644)
	mockCode := `
class Diarization3Dspeaker:
    def __init__(self):
        pass
    def __call__(self, audio_path):
        return [[0.0, 1.5, "SPEAKER_00"], [1.5, 3.2, "SPEAKER_01"]]
    def probe_evidence(self, audio_path):
        return {"has_multi_speaker_cues": True, "speaker_change_count": 2, "confidence": 0.0}
`
	_ = os.WriteFile(filepath.Join(pkgDir, "infer_diarization.py"), []byte(mockCode), 0644)

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	}

	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", adapterPath)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	defer sup.Terminate()

	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio"), 0644)

	// 1. Strict mode: require_model_snapshot=true MUST fail closed when Diarization3Dspeaker does not accept model_cache_dir
	cmdStrict := worker.Command{
		ID:         "cmd-diarize-strict-fail",
		Family:     "diarizer",
		Stage:      "diarize",
		AttemptID:  "att-strict-1",
		RunID:      "run-strict-1",
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		OutputPath: filepath.Join(t.TempDir(), "out-strict.json"),
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: diarizerEnv,
			"model_name":                  "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":               "v1.0.0",
			"require_model_snapshot":      true,
		},
	}
	_, err = client.Run(context.Background(), cmdStrict, 10*time.Second, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected strict mode to fail closed with WORKER_SNAPSHOT_PATH_REQUIRED when model_cache_dir rejected, got: %v", err)
	}

	// 2. Non-strict mode: require_model_snapshot=false allows fallback to DiarizationClass() for mock/non-strict testing
	cmdNonStrict := worker.Command{
		ID:         "cmd-diarize-nonstrict-ok",
		Family:     "diarizer",
		Stage:      "diarize",
		AttemptID:  "att-nonstrict-1",
		RunID:      "run-nonstrict-1",
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		OutputPath: filepath.Join(t.TempDir(), "out-nonstrict.json"),
		Config: map[string]any{
			"model_name":             "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":          "v1.0.0",
			"require_model_snapshot": false,
		},
	}
	art, err := client.Run(context.Background(), cmdNonStrict, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("expected non-strict mode to fall back to DiarizationClass() and succeed, got: %v", err)
	}
	if art.SHA256 == "" {
		t.Fatalf("expected valid artifact in non-strict mode")
	}

	// 3. Strict mode (evidence): require_model_snapshot=true MUST fail closed
	cmdStrictEvid := worker.Command{
		ID:         "cmd-evid-strict-fail",
		Family:     "diarizer",
		Stage:      "diarize_evidence",
		AttemptID:  "att-strict-2",
		RunID:      "run-strict-2",
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		OutputPath: filepath.Join(t.TempDir(), "out-strict-evid.json"),
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: diarizerEnv,
			"model_name":                  "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":               "v1.0.0",
			"embedding_cosine_threshold":  0.65,
			"require_model_snapshot":      true,
		},
	}
	_, err = client.Run(context.Background(), cmdStrictEvid, 10*time.Second, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected strict diarize_evidence to fail closed with WORKER_SNAPSHOT_PATH_REQUIRED when model_cache_dir rejected, got: %v", err)
	}
}

func TestSeam2_AudioRoleStage_YAMNet_StrictModeAndExecution(t *testing.T) {
	cacheRoot := t.TempDir()
	yamnetDir := filepath.Join(cacheRoot, "yamnet")
	if err := os.MkdirAll(yamnetDir, 0755); err != nil {
		t.Fatal(err)
	}

	yamnetEnv := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         "yamnet",
			Version:                "v1",
			SnapshotManifestSHA256: domain.PinnedYAMNetManifestSHA256,
			LocalPath:              yamnetDir,
		},
	}

	// 1. Envelope validation
	cmdSnap := worker.Command{
		ID:        "cmd-audiorole-snap-1",
		Family:    "audio_role",
		Stage:     "audio_role",
		AttemptID: "att-audiorole-1",
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: yamnetEnv,
		},
	}
	bSnap, _ := json.Marshal(cmdSnap)
	envSnap := worker.Envelope{
		Type:    worker.MessageTypeCommand,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: bSnap,
	}
	if err := worker.ValidateEnvelope(envSnap); err != nil {
		t.Fatalf("expected valid audio_role snapshot envelope to pass, got %v", err)
	}

	// 2. Build stageworker if needed
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "audio_role", exe, "-family", "audio_role", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	defer sup.Terminate()

	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer client.Shutdown()

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio"), 0644)

	// Strict mode: missing model asset in snapshot dir fails closed
	cmdStrict := worker.Command{
		ID:         "cmd-audiorole-strict-fail",
		Family:     "audio_role",
		Stage:      "audio_role",
		AttemptID:  "att-strict-1",
		RunID:      "run-strict-1",
		Inputs:     []worker.ArtifactRef{{Path: audioPath}},
		OutputPath: filepath.Join(t.TempDir(), "out-strict.json"),
		Config: map[string]any{
			worker.ConfigKeyModelSnapshot: yamnetEnv,
			"model_name":                  "yamnet",
			"model_version":               "v1",
			"require_model_snapshot":      true,
		},
	}
	_, err := client.Run(context.Background(), cmdStrict, 10*time.Second, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "AUDIO_ROLE_MODEL_ASSET_MISSING") {
		t.Fatalf("expected strict mode to fail closed with AUDIO_ROLE_MODEL_ASSET_MISSING when model asset missing, got: %v", err)
	}
}
