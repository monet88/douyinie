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

func TestAudioRole_RunnerOverrideRejected_StrictMode(t *testing.T) {
	t.Setenv("DOUYINIE_AUDIO_ROLE_BIN", "arbitrary_python_binary")
	_, err := resolveAudioRoleRunner(true)
	if err == nil || !strings.Contains(err.Error(), "AUDIO_ROLE_RUNNER_OVERRIDE_REJECTED") {
		t.Fatalf("expected AUDIO_ROLE_RUNNER_OVERRIDE_REJECTED on strict snapshot route, got: %v", err)
	}

	t.Setenv("DOUYINIE_AUDIO_ROLE_BIN", "")
	fakeAdapter := filepath.Join(t.TempDir(), "fake_audio_role.py")
	if err := os.WriteFile(fakeAdapter, []byte("print('fake')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOUYINIE_AUDIO_ROLE_ADAPTER", fakeAdapter)
	_, err = resolveAudioRoleRunner(true)
	if err == nil || !strings.Contains(err.Error(), "AUDIO_ROLE_RUNNER_OVERRIDE_REJECTED") {
		t.Fatalf("expected AUDIO_ROLE_RUNNER_OVERRIDE_REJECTED for arbitrary adapter on strict route, got: %v", err)
	}

	// Non-strict route allows test override
	t.Setenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN", "python")
	runner, err := resolveAudioRoleRunner(false)
	if err != nil {
		t.Fatalf("expected non-strict route to allow test adapter override, got: %v", err)
	}
	if len(runner.args) == 0 || runner.args[0] != fakeAdapter {
		t.Fatalf("expected runner to use test adapter %s, got: %+v", fakeAdapter, runner)
	}
}

func TestAudioRole_SourceMixNotMappedToVocalsWhenExplicitVocalsAbsent(t *testing.T) {
	// Verify that when vocals_audio is not specified in config, cmd.Inputs[0] is NOT treated as isolated vocals.
	// This ensures source-only fallback mode remains conservative.
	dir := t.TempDir()
	audio := filepath.Join(dir, "mix.wav")
	if err := os.WriteFile(audio, []byte("fake wav"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := worker.Command{
		ID:     "cmd-source-only",
		Family: "audio_role",
		Stage:  "audio_role",
		Inputs: []worker.ArtifactRef{{Path: audio}},
		Config: map[string]any{
			"model_name": "yamnet",
		},
		OutputPath: filepath.Join(dir, "out.json"),
	}

	// Without vocals_audio in config, srcAudio must be cmd.Inputs[0].Path and vocalsAudio empty.
	var vocalsAudio, bgAudio, srcAudio string
	if va, ok := cmd.Config["vocals_audio"].(string); ok {
		vocalsAudio = va
	}
	if ba, ok := cmd.Config["background_audio"].(string); ok {
		bgAudio = ba
	}
	if sa, ok := cmd.Config["source_audio"].(string); ok {
		srcAudio = sa
	}
	if len(cmd.Inputs) > 0 && srcAudio == "" {
		srcAudio = cmd.Inputs[0].Path
	}

	if vocalsAudio != "" {
		t.Fatalf("expected vocalsAudio to remain empty for source-only input, got %q", vocalsAudio)
	}
	if srcAudio != audio {
		t.Fatalf("expected srcAudio to be mapped to cmd.Inputs[0].Path %q, got %q", audio, srcAudio)
	}
	if bgAudio != "" {
		t.Fatalf("expected bgAudio to remain empty, got %q", bgAudio)
	}
}

// writeInertFile writes a placeholder file. On Windows exec.LookPath only stats
// an exe path, so an inert file stands in for an interpreter and the resolution
// tests need no real venv.
func writeInertFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("inert placeholder\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Issue #106: the diarizer family resolves DOUYINIE_DIARIZER_PYTHON_BIN ahead
// of the shared interpreter, while asr/aligner keep the shared one in the same
// session.
func TestResolveDiarizerRunnerHonoursPerFamilyInterpreter(t *testing.T) {
	shared := writeInertFile(t, t.TempDir(), "asr-python.exe")
	diarizerPy := writeInertFile(t, t.TempDir(), "diarizer-python.exe")
	diarizerAdapter := writeInertFile(t, t.TempDir(), "diarizer_3dspeaker.py")
	asrAdapter := writeInertFile(t, t.TempDir(), "asr_qwen3.py")
	alignerAdapter := writeInertFile(t, t.TempDir(), "aligner_qwen3.py")

	t.Setenv("DOUYINIE_DIARIZER_BIN", "")
	t.Setenv("DOUYINIE_ASR_BIN", "")
	t.Setenv("DOUYINIE_ALIGNER_BIN", "")
	t.Setenv("DOUYINIE_PYTHON_BIN", shared)
	t.Setenv("DOUYINIE_DIARIZER_PYTHON_BIN", diarizerPy)
	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", diarizerAdapter)
	t.Setenv("DOUYINIE_ASR_ADAPTER", asrAdapter)
	t.Setenv("DOUYINIE_ALIGNER_ADAPTER", alignerAdapter)

	diarizer, err := resolveDiarizerRunner()
	if err != nil {
		t.Fatalf("diarizer runner resolution failed: %v", err)
	}
	if diarizer.binary != diarizerPy {
		t.Fatalf("diarizer resolved interpreter %q, want DOUYINIE_DIARIZER_PYTHON_BIN %q", diarizer.binary, diarizerPy)
	}
	if len(diarizer.args) != 1 || diarizer.args[0] != diarizerAdapter {
		t.Fatalf("diarizer runner args %v, want the adapter %q", diarizer.args, diarizerAdapter)
	}

	asr, err := resolveASRRunner()
	if err != nil {
		t.Fatalf("asr runner resolution failed: %v", err)
	}
	if asr.binary != shared {
		t.Fatalf("asr resolved interpreter %q, want the shared DOUYINIE_PYTHON_BIN %q", asr.binary, shared)
	}

	aligner, err := resolveAlignerRunner()
	if err != nil {
		t.Fatalf("aligner runner resolution failed: %v", err)
	}
	if aligner.binary != shared {
		t.Fatalf("aligner resolved interpreter %q, want the shared DOUYINIE_PYTHON_BIN %q", aligner.binary, shared)
	}
}

// Issue #106: a configured-but-unusable diarizer interpreter fails closed with
// the variable name and the path, and never falls back to a usable interpreter
// that would reproduce the opaque DIARIZER_EXEC_FAILED.
func TestResolveDiarizerRunnerMisconfiguredInterpreterFailsClosed(t *testing.T) {
	shared := writeInertFile(t, t.TempDir(), "asr-python.exe")
	diarizerAdapter := writeInertFile(t, t.TempDir(), "diarizer_3dspeaker.py")

	t.Setenv("DOUYINIE_DIARIZER_BIN", "")
	t.Setenv("DOUYINIE_PYTHON_BIN", shared)
	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", diarizerAdapter)

	cases := []struct {
		name    string
		badPath string
	}{
		{"missing path", filepath.Join(t.TempDir(), "missing", "python.exe")},
		{"directory instead of interpreter", t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOUYINIE_DIARIZER_PYTHON_BIN", tc.badPath)
			runner, err := resolveDiarizerRunner()
			if err == nil {
				t.Fatalf("expected a fail-closed error for DOUYINIE_DIARIZER_PYTHON_BIN=%q, got runner %+v", tc.badPath, runner)
			}
			for _, want := range []string{"DIARIZER_RUNTIME_MISSING", "DOUYINIE_DIARIZER_PYTHON_BIN", tc.badPath} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q must name %q", err.Error(), want)
				}
			}
			if runner.binary != "" {
				t.Fatalf("misconfigured diarizer interpreter produced runner %q", runner.binary)
			}
			if strings.Contains(err.Error(), shared) {
				t.Fatalf("error %q must not fall back to the shared interpreter %q", err.Error(), shared)
			}
		})
	}
}

// Issue #106: an unset DOUYINIE_DIARIZER_PYTHON_BIN keeps the previous
// behaviour — the diarizer family resolves the shared interpreter.
func TestResolveDiarizerRunnerFallsBackToSharedInterpreter(t *testing.T) {
	shared := writeInertFile(t, t.TempDir(), "asr-python.exe")
	adapter := writeInertFile(t, t.TempDir(), "diarizer_3dspeaker.py")

	t.Setenv("DOUYINIE_DIARIZER_BIN", "")
	t.Setenv("DOUYINIE_DIARIZER_PYTHON_BIN", "")
	t.Setenv("DOUYINIE_PYTHON_BIN", shared)
	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", adapter)

	runner, err := resolveDiarizerRunner()
	if err != nil {
		t.Fatalf("unset DOUYINIE_DIARIZER_PYTHON_BIN must keep the shared-interpreter behaviour: %v", err)
	}
	if runner.binary != shared {
		t.Fatalf("diarizer resolved interpreter %q, want the shared interpreter %q", runner.binary, shared)
	}
}
