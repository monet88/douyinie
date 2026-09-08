package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

// snapshotTestCmd builds a stage command with a verified snapshot envelope on
// demand. It returns the command and the envelope LocalPath.
func snapshotTestCmd(t *testing.T, stage string, withEnvelope, reqSnap bool, topModelPath string) (worker.Command, string) {
	t.Helper()
	dir := t.TempDir()
	audio := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(audio, []byte("fake wav"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"model_name": "qwen3-asr", "model_version": "0.6b"}
	snapDir := ""
	if withEnvelope {
		snapDir = filepath.Join(dir, "snap")
		if err := os.MkdirAll(snapDir, 0755); err != nil {
			t.Fatal(err)
		}
		worker.SetModelSnapshotEnvelope(cfg, worker.ModelSnapshotEnvelope{
			Primary: worker.ModelSnapshotRef{
				Role:                   "primary",
				DependencyName:         "qwen3-asr",
				Version:                "0.6b",
				SnapshotManifestSHA256: "abc",
				LocalPath:              snapDir,
			},
		})
	}
	if reqSnap {
		cfg["require_model_snapshot"] = true
	}
	if topModelPath != "" {
		cfg["model_path"] = topModelPath
	}
	return worker.Command{
		ID:         "cmd-test",
		Family:     stage,
		Stage:      stage,
		Config:     cfg,
		Inputs:     []worker.ArtifactRef{{Path: audio}},
		OutputPath: filepath.Join(dir, "out.json"),
	}, snapDir
}

func TestResolvePrimarySnapshotPathStrictUsesEnvelope(t *testing.T) {
	cmd, snapDir := snapshotTestCmd(t, "asr", true, true, "")
	got, reqSnap, err := resolvePrimarySnapshotPath(cmd, "asr")
	if err != nil {
		t.Fatalf("strict envelope resolution failed: %v", err)
	}
	if !reqSnap {
		t.Fatal("expected require_model_snapshot=true")
	}
	if got != snapDir {
		t.Fatalf("expected LocalPath %q, got %q", snapDir, got)
	}
}

func TestResolvePrimarySnapshotPathStrictMissingFailsClosed(t *testing.T) {
	cmd, _ := snapshotTestCmd(t, "asr", false, true, "")
	if _, _, err := resolvePrimarySnapshotPath(cmd, "asr"); err == nil ||
		!strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED, got %v", err)
	}
}

func TestResolvePrimarySnapshotPathRejectsUnverifiedOverride(t *testing.T) {
	cmd, _ := snapshotTestCmd(t, "aligner", true, true, filepath.Join("evil", "override"))
	if _, _, err := resolvePrimarySnapshotPath(cmd, "aligner"); err == nil ||
		!strings.Contains(err.Error(), "unverified override") {
		t.Fatalf("expected unverified override rejection, got %v", err)
	}
}

func TestResolvePrimarySnapshotPathNonStrictEmpty(t *testing.T) {
	cmd, _ := snapshotTestCmd(t, "asr", false, false, "")
	got, reqSnap, err := resolvePrimarySnapshotPath(cmd, "asr")
	if err != nil {
		t.Fatalf("non-strict empty resolution failed: %v", err)
	}
	if reqSnap || got != "" {
		t.Fatalf("expected empty non-strict resolution, got %q/%v", got, reqSnap)
	}
}

func TestRunASRAdapterStrictMissingEnvelopeFailsClosed(t *testing.T) {
	cmd, _ := snapshotTestCmd(t, "asr", false, true, "")
	if _, err := runASRAdapter(context.Background(), cmd, nil); err == nil ||
		!strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED, got %v", err)
	}
}

func TestRunAlignerAdapterStrictMissingEnvelopeFailsClosed(t *testing.T) {
	cmd, _ := snapshotTestCmd(t, "aligner", false, true, "")
	cmd.Config["text"] = "你好"
	if _, err := runAlignerAdapter(context.Background(), cmd, nil); err == nil ||
		!strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED, got %v", err)
	}
}

// recordingAdapter installs a fake DOUYINIE_*_ADAPTER python script that
// records its stdin request and returns a canned response. It returns the
// record file path.
func recordingAdapter(t *testing.T, envKey, canned string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		if _, err := exec.LookPath("python"); err != nil {
			t.Skip("no python runtime for adapter subprocess test")
		}
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "request.json")
	script := filepath.Join(dir, "fake_adapter.py")
	body := "import json,os,sys\n" +
		"raw=sys.stdin.read()\n" +
		"open(r'" + record + "','w',encoding='utf-8').write(raw)\n" +
		"sys.stdout.write(r'" + canned + "')\n"
	if err := os.WriteFile(script, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKey, script)
	return record
}

func TestRunASRAdapterSendsSnapshotModelPath(t *testing.T) {
	canned := `{"segments": [{"start_ms": 0, "end_ms": 1200, "text": "你好世界", "confidence": 0.9, "language_code": "zh"}]}`
	record := recordingAdapter(t, "DOUYINIE_ASR_ADAPTER", canned)
	cmd, snapDir := snapshotTestCmd(t, "asr", true, true, "")
	if _, err := runASRAdapter(context.Background(), cmd, nil); err != nil {
		t.Fatalf("runASRAdapter failed: %v", err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req["model_path"] != snapDir {
		t.Fatalf("expected model_path %q, got %v", snapDir, req["model_path"])
	}
	if req["require_model_snapshot"] != true {
		t.Fatalf("expected require_model_snapshot=true, got %v", req["require_model_snapshot"])
	}
}

func TestRunAlignerAdapterSendsSnapshotModelPath(t *testing.T) {
	canned := `{"word_timings": [{"word": "你好", "start_ms": 0, "end_ms": 500, "confidence": 0.9}]}`
	record := recordingAdapter(t, "DOUYINIE_ALIGNER_ADAPTER", canned)
	cmd, snapDir := snapshotTestCmd(t, "aligner", true, true, "")
	cmd.Config["text"] = "你好世界"
	if _, err := runAlignerAdapter(context.Background(), cmd, nil); err != nil {
		t.Fatalf("runAlignerAdapter failed: %v", err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req["model_path"] != snapDir {
		t.Fatalf("expected model_path %q, got %v", snapDir, req["model_path"])
	}
	if req["require_model_snapshot"] != true {
		t.Fatalf("expected require_model_snapshot=true, got %v", req["require_model_snapshot"])
	}
}

func TestDispatchStage_ProbeSnapshotExemption(t *testing.T) {
	// 1. Unrelated stage with mode=probe and require_model_snapshot=true MUST fail closed with WORKER_SNAPSHOT_PATH_REQUIRED
	for _, unrelatedStage := range []string{"asr", "aligner", "tts", "ocr", "diarize"} {
		cmd := worker.Command{
			ID:     "probe-unrelated-" + unrelatedStage,
			Stage:  unrelatedStage,
			Config: map[string]any{"require_model_snapshot": true, "mode": "probe"},
		}
		_, err := dispatchStage(context.Background(), cmd, nil)
		if err == nil {
			t.Fatalf("expected error for stage %s with mode=probe and no snapshot, got nil", unrelatedStage)
		}
		if !strings.Contains(err.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
			t.Fatalf("expected WORKER_SNAPSHOT_PATH_REQUIRED for stage %s with mode=probe, got %v", unrelatedStage, err)
		}
	}

	// 2. separator with mode=probe is exempted from the top-level snapshot check
	cmdSep := worker.Command{
		ID:     "probe-sep",
		Stage:  "separator",
		Config: map[string]any{"require_model_snapshot": true, "mode": "probe"},
	}
	_, errSep := dispatchStage(context.Background(), cmdSep, nil)
	// Even if it fails downstream (e.g. runner not found in test environment), it must NOT fail with WORKER_SNAPSHOT_PATH_REQUIRED
	if errSep != nil && strings.Contains(errSep.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("separator mode=probe should be exempted from snapshot check, got %v", errSep)
	}

	// 3. separator_probe is exempted from the top-level snapshot check
	cmdSepProbe := worker.Command{
		ID:     "probe-sep-stage",
		Stage:  "separator_probe",
		Config: map[string]any{"require_model_snapshot": true},
	}
	_, errSepProbe := dispatchStage(context.Background(), cmdSepProbe, nil)
	if errSepProbe != nil && strings.Contains(errSepProbe.Error(), "WORKER_SNAPSHOT_PATH_REQUIRED") {
		t.Fatalf("separator_probe stage should be exempted from snapshot check, got %v", errSepProbe)
	}
}

func TestRunTTSAdapter_VoicePresetValidation(t *testing.T) {
	// Rejection of unknown Kokoro voice presets mentions all domain.FrozenKokoroVoiceOrder
	cmdKokoro := worker.Command{
		ID: "tts-kokoro-unknown",
		Config: map[string]any{
			"text":       "hello world",
			"model_name": "kokoro-tts",
			"voice_id":   "unknown_voice",
		},
	}
	_, errKokoro := runTTSAdapter(context.Background(), cmdKokoro, nil)
	if errKokoro == nil || !strings.Contains(errKokoro.Error(), "TTS_VOICE_ASSET_MISSING") {
		t.Fatalf("expected TTS_VOICE_ASSET_MISSING for unknown kokoro voice, got %v", errKokoro)
	}
	for _, v := range domain.FrozenKokoroVoiceOrder {
		if !strings.Contains(errKokoro.Error(), v) {
			t.Errorf("expected error message to include frozen voice %q, got: %s", v, errKokoro.Error())
		}
	}

	// Rejection of unknown VieNeu voice presets mentions all domain.FrozenVieNeuVoiceOrder
	cmdVieNeu := worker.Command{
		ID: "tts-vieneu-unknown",
		Config: map[string]any{
			"text":       "xin chào",
			"model_name": "vieneu-tts",
			"voice_id":   "unknown_voice",
		},
	}
	_, errVieNeu := runTTSAdapter(context.Background(), cmdVieNeu, nil)
	if errVieNeu == nil || !strings.Contains(errVieNeu.Error(), "TTS_VOICE_ASSET_MISSING") {
		t.Fatalf("expected TTS_VOICE_ASSET_MISSING for unknown vieneu voice, got %v", errVieNeu)
	}
	for _, v := range domain.FrozenVieNeuVoiceOrder {
		if !strings.Contains(errVieNeu.Error(), v) {
			t.Errorf("expected error message to include frozen voice %q, got: %s", v, errVieNeu.Error())
		}
	}
}
