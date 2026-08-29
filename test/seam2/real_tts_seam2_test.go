package seam2_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/worker"
)

func resolveTestTTSPython(t *testing.T) string {
	t.Helper()
	if py := os.Getenv("DOUYINIE_TTS_PYTHON_BIN"); py != "" {
		if _, err := os.Stat(py); err == nil {
			return py
		}
	}
	t.Skip("isolated TTS python environment not configured: set DOUYINIE_TTS_PYTHON_BIN to run real TTS model tests")
	return ""
}

func TestSeam2_TTSStage_RealVieNeuSynthesisAndDurationProbe(t *testing.T) {
	ttsPy := resolveTestTTSPython(t)
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", ttsPy)

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 10*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	outPath := filepath.Join(t.TempDir(), "out-vieneu-real.json")
	cmd := worker.Command{
		ID:         "cmd-vieneu-real",
		Family:     "tts",
		Stage:      "tts",
		AttemptID:  "attempt-vieneu-real",
		RunID:      "run-vieneu-real",
		OutputPath: outPath,
		Config: map[string]any{
			"text":             "Xin chào Việt Nam, đây là bài kiểm tra tổng hợp giọng nói thực tế.",
			"model_name":       "vieneu-tts",
			"model_version":    "1.0.0",
			"language":         "vi",
			"voice_id":         "vi_female_natural",
			"speed":            "1.0",
			"slot_duration_ms": "5000",
		},
	}

	artifact, err := client.Run(context.Background(), cmd, 120*time.Second, 120*time.Second)
	if err != nil {
		t.Fatalf("VieNeu real model synthesis failed: %v", err)
	}

	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for VieNeu artifact, got %q", artifact.SHA256)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output JSON: %v", err)
	}

	var res struct {
		AudioData          string `json:"audio_data"`
		AudioSHA256        string `json:"audio_sha256"`
		MeasuredDurationMs int64  `json:"measured_duration_ms"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("unmarshal output JSON: %v", err)
	}

	if res.MeasuredDurationMs <= 0 {
		t.Fatalf("expected positive measured duration from VieNeu, got %d", res.MeasuredDurationMs)
	}

	wavBytes, err := base64.StdEncoding.DecodeString(res.AudioData)
	if err != nil || len(wavBytes) == 0 {
		t.Fatalf("decode base64 audio data: %v", err)
	}

	probedDurMs, err := media.ProbeWAVBytes(wavBytes)
	if err != nil {
		t.Fatalf("ProbeWAVBytes on real VieNeu synthesis failed: %v", err)
	}

	if probedDurMs != res.MeasuredDurationMs {
		t.Errorf("probed duration (%d ms) != measured duration (%d ms)", probedDurMs, res.MeasuredDurationMs)
	}
	t.Logf("VieNeu Real Synthesis SUCCESS: probed_dur=%d ms, sha256=%s", probedDurMs, res.AudioSHA256)
}

func TestSeam2_TTSStage_RealCosyVoice3SynthesisSpeedFitAndDurationProbe(t *testing.T) {
	ttsPy := resolveTestTTSPython(t)
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", ttsPy)

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 10*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	outPath := filepath.Join(t.TempDir(), "out-cosyvoice-real.json")
	// Request with a constrained slot_duration_ms = 1500ms to trigger speed-fit recalibration
	cmd := worker.Command{
		ID:         "cmd-cosyvoice-real",
		Family:     "tts",
		Stage:      "tts",
		AttemptID:  "attempt-cosyvoice-real",
		RunID:      "run-cosyvoice-real",
		OutputPath: outPath,
		Config: map[string]any{
			"text":             "八百标兵奔北坡，北坡炮兵并排跑。",
			"model_name":       "cosyvoice-tts",
			"model_version":    "3.0.0",
			"language":         "zh",
			"voice_id":         "中文女",
			"speed":            "1.0",
			"slot_duration_ms": "1500",
		},
	}

	artifact, err := client.Run(context.Background(), cmd, 120*time.Second, 120*time.Second)
	if err != nil {
		t.Fatalf("CosyVoice3 real model synthesis failed: %v", err)
	}

	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for CosyVoice3 artifact, got %q", artifact.SHA256)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output JSON: %v", err)
	}

	var res struct {
		AudioData           string `json:"audio_data"`
		AudioSHA256         string `json:"audio_sha256"`
		MeasuredDurationMs  int64  `json:"measured_duration_ms"`
		PredictedDurationMs int64  `json:"predicted_duration_ms"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("unmarshal output JSON: %v", err)
	}

	if res.MeasuredDurationMs <= 0 {
		t.Fatalf("expected positive measured duration from CosyVoice3, got %d", res.MeasuredDurationMs)
	}

	// Invariant: speed-fit recalibration compressed duration relative to natural pass
	if res.PredictedDurationMs <= 0 {
		t.Fatalf("expected predicted (natural pass) duration, got %d", res.PredictedDurationMs)
	}

	wavBytes, err := base64.StdEncoding.DecodeString(res.AudioData)
	if err != nil || len(wavBytes) == 0 {
		t.Fatalf("decode base64 audio data: %v", err)
	}

	probedDurMs, err := media.ProbeWAVBytes(wavBytes)
	if err != nil {
		t.Fatalf("ProbeWAVBytes on real CosyVoice3 synthesis failed: %v", err)
	}

	if probedDurMs != res.MeasuredDurationMs {
		t.Errorf("probed duration (%d ms) != measured duration (%d ms)", probedDurMs, res.MeasuredDurationMs)
	}
	t.Logf("CosyVoice3 Real Synthesis SUCCESS: natural_pred=%d ms, speed_fit_measured=%d ms, sha256=%s",
		res.PredictedDurationMs, res.MeasuredDurationMs, res.AudioSHA256)
}
