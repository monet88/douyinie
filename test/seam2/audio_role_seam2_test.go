package seam2_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/worker"
)

func TestSeam2_AudioRolePlan_EndToEndRealMedia(t *testing.T) {
	sepPy := os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN")
	if sepPy == "" {
		sepPy = `D:\douyinie-ref\phase1.1-runtime\venvs\separator\Scripts\python.exe`
	}
	if _, err := os.Stat(sepPy); err != nil {
		t.Skipf("skipping real separator test: venv python not found: %s", sepPy)
	}

	arPy := os.Getenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN")
	if arPy == "" {
		arPy = `D:\douyinie-ref\phase1.1-runtime\venvs\audio_role\Scripts\python.exe`
	}
	if _, err := os.Stat(arPy); err != nil {
		t.Skipf("skipping real audio_role test: venv python not found: %s", arPy)
	}

	fixturePath := `D:\douyinie-ref\_live-tests\vertical-slice-20260819\source-5s.mp4`
	if _, err := os.Stat(fixturePath); err != nil {
		t.Skipf("skipping real audio_role test: fixture not found: %s", fixturePath)
	}

	t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", sepPy)
	t.Setenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN", arPy)

	yamnetPath := os.Getenv("DOUYINIE_YAMNET_MODEL_PATH")
	if yamnetPath == "" {
		yamnetPath = `D:\douyinie-ref\phase1.1-runtime\staging\yamnet_v1\yamnet.tflite`
	}
	// The audio_role stage fails closed (AUDIO_ROLE_MODEL_ASSET_MISSING) when the model is
	// inaccessible, and the adapter's resolve_model_path accepts either the snapshot root
	// directory (expecting yamnet.tflite inside it) or a direct .tflite file. The external model
	// is not part of the repo, so its absence must skip here rather than fail downstream.
	if fi, err := os.Stat(yamnetPath); err != nil {
		t.Skipf("skipping real audio_role test: yamnet model not found: %s", yamnetPath)
	} else if fi.IsDir() {
		if _, err := os.Stat(filepath.Join(yamnetPath, "yamnet.tflite")); err != nil {
			t.Skipf("skipping real audio_role test: yamnet.tflite not found in snapshot root: %s", yamnetPath)
		}
	}

	// Default models for tests when snapshot path is not available or ignored
	t.Setenv("AUDIO_SEPARATOR_MODEL_DIR", os.Getenv("AUDIO_SEPARATOR_MODEL_DIR"))

	exe := buildStageWorker(t)
	ctx := context.Background()

	// Start separator and audio_role, each in its own family worker.
	// One supervisor per family: a Supervisor tracks a single worker process, so spawning a second
	// family on the same supervisor orphans the first one (it keeps running and keeps its image
	// file locked).
	sepSup := worker.NewSupervisor()
	if err := sepSup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn separator: %v", err)
	}
	arSup := worker.NewSupervisor()
	if err := arSup.Spawn(ctx, "audio_role", exe, "-family", "audio_role", "-heartbeat-ms", "1000"); err != nil {
		_ = sepSup.Terminate()
		t.Fatalf("spawn audio_role: %v", err)
	}
	sepClient := worker.NewClient(sepSup)
	if _, err := sepClient.Handshake(ctx, 15*time.Second); err != nil {
		_ = sepSup.Terminate()
		_ = arSup.Terminate()
		t.Fatalf("separator handshake: %v", err)
	}
	arClient := worker.NewClient(arSup)
	if _, err := arClient.Handshake(ctx, 15*time.Second); err != nil {
		_ = sepSup.Terminate()
		_ = arSup.Terminate()
		t.Fatalf("audio_role handshake: %v", err)
	}
	defer func() {
		_ = sepClient.Shutdown()
		_ = arClient.Shutdown()
		_ = sepSup.Terminate()
		_ = arSup.Terminate()
		// On Windows a running executable may be renamed but not deleted, and t.TempDir's cleanup
		// runs after this defer; deleting the image here is the only reliable probe that every
		// spawned worker has really exited, and its failure must not fail the test itself.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if err := os.Remove(exe); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("stageworker image %s still locked after the shutdown wait", exe)
	}()

	// 1. Run Separator
	sepOutPath := filepath.Join(t.TempDir(), "out-separator.json")
	sepCmd := worker.Command{
		ID:         "cmd-sep",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "attempt-sep",
		RunID:      "run-sep",
		OutputPath: sepOutPath,
		Inputs: []worker.ArtifactRef{
			{Path: fixturePath},
		},
		Config: map[string]any{
			"model_name":             "htdemucs",
			"model_version":          "v4",
			"require_model_snapshot": false,
		},
	}

	_, err := sepClient.Run(ctx, sepCmd, 60*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("separator execution failed: %v", err)
	}

	sepData, err := os.ReadFile(sepOutPath)
	if err != nil {
		t.Fatalf("read separator output JSON: %v", err)
	}
	var sepRes map[string]any
	if err := json.Unmarshal(sepData, &sepRes); err != nil {
		t.Fatalf("unmarshal separator JSON: %v", err)
	}

	declaredRate := int(sepRes["sample_rate"].(float64))
	declaredChannels := int(sepRes["channels"].(float64))

	vocalsB64, ok := sepRes["vocals_data"].(string)
	if !ok || vocalsB64 == "" {
		t.Fatalf("separator output missing vocals_data base64")
	}
	bgB64, ok := sepRes["background_data"].(string)
	if !ok || bgB64 == "" {
		t.Fatalf("separator output missing background_data base64")
	}

	vocalsBytes, err := base64.StdEncoding.DecodeString(vocalsB64)
	if err != nil {
		t.Fatalf("decode vocals_data: %v", err)
	}
	bgBytes, err := base64.StdEncoding.DecodeString(bgB64)
	if err != nil {
		t.Fatalf("decode background_data: %v", err)
	}

	vocalsInfo, err := media.ParseWAVHeader(vocalsBytes)
	if err != nil {
		t.Fatalf("ParseWAVHeader vocals WAV: %v", err)
	}

	if vocalsInfo.SampleRate != 16000 || vocalsInfo.NumChannels != 1 {
		t.Errorf("measured vocals WAV: %d Hz / %d ch, expected 16000 Hz / 1 ch", vocalsInfo.SampleRate, vocalsInfo.NumChannels)
	}

	// The audio-role analyzer reads the background stem under the same contract as the vocals
	// stem, so a stereo/44.1 kHz background must fail the end-to-end test instead of passing it.
	bgInfo, err := media.ParseWAVHeader(bgBytes)
	if err != nil {
		t.Fatalf("ParseWAVHeader background WAV: %v", err)
	}
	if bgInfo.SampleRate != 16000 || bgInfo.NumChannels != 1 || bgInfo.BitsPerSample != 16 {
		t.Errorf("measured background WAV: %d Hz / %d ch / %d-bit, expected 16000 Hz / 1 ch / 16-bit", bgInfo.SampleRate, bgInfo.NumChannels, bgInfo.BitsPerSample)
	}

	if declaredRate != 16000 || declaredChannels != 1 {
		t.Errorf("separator declared %d Hz / %d ch, expected 16000 Hz / 1 ch", declaredRate, declaredChannels)
	}

	vocalsFilePath := filepath.Join(t.TempDir(), "vocals.wav")
	if err := os.WriteFile(vocalsFilePath, vocalsBytes, 0644); err != nil {
		t.Fatalf("write vocals.wav: %v", err)
	}
	bgFilePath := filepath.Join(t.TempDir(), "bg.wav")
	if err := os.WriteFile(bgFilePath, bgBytes, 0644); err != nil {
		t.Fatalf("write bg.wav: %v", err)
	}

	// 2. Run Audio Role
	arOutPath := filepath.Join(t.TempDir(), "out-ar.json")
	arCmd := worker.Command{
		ID:         "cmd-ar",
		Family:     "audio_role",
		Stage:      "audio_role",
		AttemptID:  "attempt-ar",
		RunID:      "run-ar",
		OutputPath: arOutPath,
		Config: map[string]any{
			"vocals_audio":           vocalsFilePath,
			"background_audio":       bgFilePath,
			"require_model_snapshot": false,
			"model_path":             yamnetPath,
		},
	}

	_, err = arClient.Run(ctx, arCmd, 60*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("audio_role execution failed: %v", err)
	}

	arData, err := os.ReadFile(arOutPath)
	if err != nil {
		t.Fatalf("read audio_role output JSON: %v", err)
	}
	var arRes map[string]any
	if err := json.Unmarshal(arData, &arRes); err != nil {
		t.Fatalf("unmarshal audio_role JSON: %v", err)
	}

	plan, ok := arRes["segments"].([]any)
	if !ok {
		t.Fatalf("audio_role output missing 'segments' array. Got: %s", string(arData))
	}
	if len(plan) == 0 {
		t.Errorf("expected >0 audio role segments, got 0")
	}
	t.Logf("Success! Separator -> AudioRole Pipeline OK. Generated %d segments.", len(plan))
}
