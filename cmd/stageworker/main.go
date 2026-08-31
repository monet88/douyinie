package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/worker"
)

// Model identity config keys (Issue #44 Finding 2): the RuntimeHost embeds
// manifest/model-registry-driven checkpoint identity in every stage command so
// provenance and routing distinguish Qwen3-ASR 1.7B from 0.6B, and so diarizer
// model/version changes yield distinct artifacts. Identity is required for
// real stages; a missing identity fails closed instead of being guessed.
const (
	cfgModelName       = "model_name"
	cfgModelVersion    = "model_version"
	cfgVADModelName    = "vad_model_name"
	cfgVADModelVersion = "vad_model_version"

	defaultVADModelName    = "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"
	defaultVADModelVersion = "v2.0.4"
)

// requireConfigString extracts a required string config value.
func requireConfigString(cfg map[string]any, key string) (string, error) {
	v, _ := cfg[key].(string)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("missing required config %q", key)
	}
	return v, nil
}

type envelopeResult struct {
	env worker.Envelope
	err error
}

func main() {
	var (
		family      = flag.String("family", "generic", "Worker family identifier")
		workerID    = flag.String("worker-id", "", "Worker identifier (auto-generated if empty)")
		heartbeatMS = flag.Int64("heartbeat-ms", int64(worker.HeartbeatInterval.Milliseconds()), "Heartbeat interval in ms")
	)
	flag.Parse()

	if *workerID == "" {
		*workerID = uuid.NewString()
	}

	enc := worker.NewEncoder(os.Stdout)
	dec := worker.NewDecoder(os.Stdin)

	workerIDStr := *workerID
	familyStr := *family

	// Send hello with protocol version
	if err := enc.Encode(worker.MessageTypeHello, worker.HelloPayload{
		WorkerID: workerIDStr,
		Family:   familyStr,
		Schema:   worker.ProtocolVersion,
	}); err != nil {
		log.Fatalf("failed to send hello: %v", err)
	}

	log.Printf("[StageWorker] hello from %s (family=%s, schema=%d)", workerIDStr, familyStr, worker.ProtocolVersion)

	// Heartbeat goroutine
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	hbInterval := time.Duration(*heartbeatMS) * time.Millisecond
	if hbInterval < 1*time.Second {
		hbInterval = 1 * time.Second
	}

	go func() {
		ticker := time.NewTicker(hbInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = enc.Encode(worker.MessageTypeHeartbeat, worker.HeartbeatPayload{})
			}
		}
	}()

	// Signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Asynchronous stdin reader loop
	inbox := make(chan envelopeResult, 16)
	go func() {
		for {
			env, err := dec.Decode()
			inbox <- envelopeResult{env: env, err: err}
			if err != nil {
				return
			}
		}
	}()

	var (
		currentCmd    *worker.Command
		currentCancel context.CancelFunc
		stageDone     chan stageResult
	)

	// Main event loop: coordinates stdin messages, execution completion, and OS signals
	for {
		select {
		case <-sigChan:
			if currentCancel != nil {
				currentCancel()
			}
			if currentCmd != nil {
				_ = enc.Encode(worker.MessageTypeInterrupted, worker.InterruptedPayload{
					CommandID: currentCmd.ID,
					Reason:    "signal received",
				})
			}
			os.Exit(0)

		case res := <-stageDone:
			cmd := *currentCmd
			currentCmd = nil
			currentCancel = nil
			stageDone = nil

			if res.err != nil {
				if errors.Is(res.err, context.Canceled) || res.interrupted {
					_ = enc.Encode(worker.MessageTypeCancelComplete, worker.CancelCompletePayload{
						CommandID: cmd.ID,
					})
				} else {
					_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
						Error: worker.NewError("EXECUTION_ERROR", res.err.Error(), nil),
					})
				}
				continue
			}
			if res.interrupted {
				_ = enc.Encode(worker.MessageTypeCancelComplete, worker.CancelCompletePayload{
					CommandID: cmd.ID,
				})
				continue
			}

			// Send complete
			if err := enc.Encode(worker.MessageTypeComplete, worker.CompletePayload{
				CommandID: cmd.ID,
				Artifact:  res.artifact,
			}); err != nil {
				log.Fatalf("complete send failed: %v", err)
			}
			log.Printf("[StageWorker] command %s complete", cmd.ID)

		case in := <-inbox:
			if in.err != nil {
				if errors.Is(in.err, io.EOF) {
					os.Exit(0)
				}
				log.Printf("[StageWorker] decode error: %v", in.err)
				_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
					Error: worker.NewError("PROTOCOL_ERROR", fmt.Sprintf("decode error: %v", in.err), nil),
				})
				os.Exit(1)
			}

			env := in.env
			if err := worker.ValidateEnvelope(env); err != nil {
				log.Printf("[StageWorker] invalid envelope: %v", err)
				_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
					Error: worker.NewError("VALIDATION_ERROR", err.Error(), nil),
				})
				continue
			}

			switch env.Type {
			case worker.MessageTypeHeartbeat:
				_ = enc.Encode(worker.MessageTypeHeartbeat, worker.HeartbeatPayload{})

			case worker.MessageTypeCommand:
				var cmd worker.Command
				if err := json.Unmarshal(env.Payload, &cmd); err != nil {
					_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
						Error: worker.NewError("PARSE_ERROR", fmt.Sprintf("parse command: %v", err), nil),
					})
					continue
				}

				if currentCmd != nil {
					_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
						Error: worker.NewError("BUSY", "worker is already executing a command", nil),
					})
					continue
				}

				log.Printf("[StageWorker] received command id=%s stage=%s attempt=%s", cmd.ID, cmd.Stage, cmd.AttemptID)

				// Send ready
				if err := enc.Encode(worker.MessageTypeReady, worker.ReadyPayload{CommandID: cmd.ID}); err != nil {
					log.Fatalf("ready send failed: %v", err)
				}

				cmdCopy := cmd
				currentCmd = &cmdCopy
				cmdCtx, cmdCancel := context.WithCancel(ctx)
				currentCancel = cmdCancel
				ch := make(chan stageResult, 1)
				stageDone = ch

				go func() {
					ch <- executeStage(cmdCtx, cmdCopy, enc)
				}()

			case worker.MessageTypeCancel:
				var cancelPayload worker.CancelPayload
				if err := json.Unmarshal(env.Payload, &cancelPayload); err != nil {
					_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
						Error: worker.NewError("PARSE_ERROR", "invalid cancel payload", nil),
					})
					continue
				}

				// Acknowledge cancel
				_ = enc.Encode(worker.MessageTypeCancelAck, worker.CancelAckPayload{CommandID: cancelPayload.CommandID})

				// If configured to ignore cancel (test hook for forced termination), do not cancel context
				if currentCmd != nil && currentCmd.ID == cancelPayload.CommandID {
					if ignore, ok := currentCmd.Config["ignore_cancel"].(bool); ok && ignore {
						log.Printf("[StageWorker] ignoring cancel request for test command %s", currentCmd.ID)
						continue
					}
					if currentCancel != nil {
						currentCancel()
					}
				}

			default:
				_ = enc.Encode(worker.MessageTypeError, worker.ErrorPayload{
					Error: worker.NewError("UNKNOWN_TYPE", fmt.Sprintf("unknown message type: %s", env.Type), nil),
				})
			}
		}
	}
}

type stageResult struct {
	artifact    worker.ArtifactRef
	err         error
	interrupted bool
}

func executeStage(ctx context.Context, cmd worker.Command, enc *worker.Encoder) stageResult {
	// Report progress as a skeleton placeholder.
	_ = enc.Encode(worker.MessageTypeProgress, worker.ProgressPayload{
		CommandID: cmd.ID,
		Percent:   0,
		Phase:     "starting",
	})

	select {
	case <-ctx.Done():
		return stageResult{interrupted: true, err: ctx.Err()}
	default:
	}

	// Test hook: support spawning a descendant process to prove process tree cleanup
	if pidFile, ok := cmd.Config["descendant_pid_file"].(string); ok && pidFile != "" {
		var childCmd *exec.Cmd
		if runtime.GOOS == "windows" {
			childCmd = exec.Command("ping", "127.0.0.1", "-n", "60")
		} else {
			childCmd = exec.Command("sleep", "60")
		}
		if err := childCmd.Start(); err != nil {
			return stageResult{err: fmt.Errorf("start descendant: %w", err)}
		}
		if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", childCmd.Process.Pid)), 0644); err != nil {
			_ = childCmd.Process.Kill()
			return stageResult{err: fmt.Errorf("write descendant pid: %w", err)}
		}
		childDone := make(chan error, 1)
		go func() {
			childDone <- childCmd.Wait()
		}()

		select {
		case <-ctx.Done():
			_ = childCmd.Process.Kill()
			return stageResult{interrupted: true, err: ctx.Err()}
		case err := <-childDone:
			if err != nil && ctx.Err() != nil {
				return stageResult{interrupted: true, err: ctx.Err()}
			}
		}
	}

	// Test hook: support a configurable long-running command for cancel tests.
	if longMS, ok := cmd.Config["long_running_ms"].(float64); ok && longMS > 0 {
		select {
		case <-time.After(time.Duration(longMS) * time.Millisecond):
		case <-ctx.Done():
			return stageResult{interrupted: true, err: ctx.Err()}
		}
	}

	// Dispatch to the appropriate stage adapter.
	result, err := dispatchStage(ctx, cmd, enc)
	if err != nil {
		return stageResult{err: err}
	}

	_ = enc.Encode(worker.MessageTypeProgress, worker.ProgressPayload{
		CommandID: cmd.ID,
		Percent:   100,
		Phase:     "complete",
	})

	return stageResult{
		artifact: result,
	}
}

// dispatchStage routes the stage command to the appropriate adapter function.
// Each adapter is a real production path that checks for available model binaries;
// if a binary is not found, it fails closed with a structured error rather than
// silently faking model output. This is the smallest architecture-compliant
// production StageWorker/provider-adapter path.
func dispatchStage(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	switch cmd.Stage {
	case "asr":
		return runASRAdapter(ctx, cmd, enc)
	case "aligner":
		return runAlignerAdapter(ctx, cmd, enc)
	case "diarize":
		return runDiarizerAdapter(ctx, cmd, enc)
	case "diarize_evidence":
		return runDiarizerEvidenceAdapter(ctx, cmd, enc)
	case "tts":
		return runTTSAdapter(ctx, cmd, enc)
	case "separator":
		return runSeparatorAdapter(ctx, cmd, enc)
	case "ocr":
		return runOCRAdapter(ctx, cmd, enc)
	default:
		// For unrecognized stages, fall back to the marker artifact placeholder.
		return writeMarkerArtifact(cmd)
	}
}

// runASRAdapter runs the Qwen3-ASR adapter. A real invocation carries an
// input audio artifact; a missing input fails closed (ASR_MISSING_INPUT).
// The model binary is invoked over a stdin JSON request / stdout JSON response
// contract, and its parsed output is written to the output path with a real
// SHA-256. Binary absence, exec failure, invalid output, or empty output each
// fail closed with a structured error — never a fabricated transcript.
func runASRAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	if len(cmd.Inputs) == 0 {
		return worker.ArtifactRef{}, worker.NewError("ASR_MISSING_INPUT",
			"ASR command has no input audio artifact",
			map[string]any{"stage": "asr", "command_id": cmd.ID})
	}
	// Finding 2: model checkpoint identity is config/manifest-driven; the
	// stage command must declare which Qwen3-ASR checkpoint (1.7B vs 0.6B)
	// it wants. Fail closed when missing — never guess an identity.
	modelName, err := requireConfigString(cmd.Config, cfgModelName)
	if err != nil {
		return worker.ArtifactRef{}, worker.NewError("ASR_MISSING_MODEL_IDENTITY",
			"asr command is missing required config 'model_name' (manifest/model registry driven)",
			map[string]any{"stage": "asr", "command_id": cmd.ID})
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)

	runner, err := resolveASRRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	// stdin JSON request: audio_path is the machine-local input artifact path,
	// plus the declared model identity for observability end-to-end.
	req := map[string]any{
		"audio_path":    cmd.Inputs[0].Path,
		"run_id":        cmd.RunID,
		"attempt_id":    cmd.AttemptID,
		cfgModelName:    modelName,
		cfgModelVersion: modelVersion,
	}
	var out struct {
		Segments     []domain.ASRRawSegment `json:"segments"`
		ModelName    string                 `json:"model_name,omitempty"`
		ModelVersion string                 `json:"model_version,omitempty"`
	}
	out.ModelName = modelName
	out.ModelVersion = modelVersion
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}
	if len(out.Segments) == 0 {
		return worker.ArtifactRef{}, worker.NewError("ASR_NO_SEGMENTS",
			"Qwen3-ASR produced no transcript segments",
			map[string]any{"stage": "asr", "command_id": cmd.ID})
	}
	return writeOutputArtifact(cmd, out)
}

// runAlignerAdapter runs the Qwen3-ForcedAligner adapter. A real invocation
// carries an input audio artifact and the accepted text (cmd.Config["text"]).
// Fail-closed behavior mirrors the ASR adapter: missing input, missing binary,
// exec failure, invalid/empty output all produce structured errors.
func runAlignerAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	if len(cmd.Inputs) == 0 {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_MISSING_INPUT",
			"aligner command has no input audio artifact",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}
	// Finding 2: manifest-driven model identity (required, fail-closed).
	modelName, err := requireConfigString(cmd.Config, cfgModelName)
	if err != nil {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_MISSING_MODEL_IDENTITY",
			"aligner command is missing required config 'model_name' (manifest/model registry driven)",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	text, _ := cmd.Config["text"].(string)
	if strings.TrimSpace(text) == "" {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_MISSING_TEXT",
			"aligner command has no accepted text in config",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}

	runner, err := resolveAlignerRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	// stdin JSON request: audio_path + the accepted text to align + identity.
	req := map[string]any{
		"audio_path":    cmd.Inputs[0].Path,
		"text":          text,
		"run_id":        cmd.RunID,
		"attempt_id":    cmd.AttemptID,
		cfgModelName:    modelName,
		cfgModelVersion: modelVersion,
	}
	var out struct {
		WordTimings  []domain.WordTiming `json:"word_timings"`
		ModelName    string              `json:"model_name,omitempty"`
		ModelVersion string              `json:"model_version,omitempty"`
	}
	out.ModelName = modelName
	out.ModelVersion = modelVersion
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}
	if len(out.WordTimings) == 0 {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_NO_TIMINGS",
			"Qwen3-ForcedAligner produced no word timings",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}
	return writeOutputArtifact(cmd, out)
}

// runTTSAdapter runs the worker-backed TTS adapter (VieNeu, CosyVoice3, Kokoro, Chatterbox).
// Missing text, missing model identity, runner absence, or execution failures fail closed.
func runTTSAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	text, _ := cmd.Config["text"].(string)
	if strings.TrimSpace(text) == "" {
		return worker.ArtifactRef{}, worker.NewError("TTS_MISSING_TEXT",
			"tts command has no text in config",
			map[string]any{"stage": "tts", "command_id": cmd.ID})
	}
	modelName, err := requireConfigString(cmd.Config, cfgModelName)
	if err != nil {
		return worker.ArtifactRef{}, worker.NewError("TTS_MISSING_MODEL_IDENTITY",
			"tts command is missing required config 'model_name' (manifest/model registry driven)",
			map[string]any{"stage": "tts", "command_id": cmd.ID})
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	lang, _ := cmd.Config["language"].(string)
	voiceID, _ := cmd.Config["voice_id"].(string)
	speed, _ := cmd.Config["speed"].(string)
	slotDur, _ := cmd.Config["slot_duration_ms"].(string)

	runner, err := resolveTTSRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"text":             text,
		"language":         lang,
		"voice_id":         voiceID,
		"speed":            speed,
		"slot_duration_ms": slotDur,
		"run_id":           cmd.RunID,
		"attempt_id":       cmd.AttemptID,
		cfgModelName:       modelName,
		cfgModelVersion:    modelVersion,
	}

	var out struct {
		AudioData           []byte `json:"audio_data"`
		AudioPath           string `json:"audio_path"`
		AudioSHA256         string `json:"audio_sha256"`
		MeasuredDurationMs  int64  `json:"measured_duration_ms"`
		PredictedDurationMs int64  `json:"predicted_duration_ms"`
		ModelName           string `json:"model_name"`
		ModelVersion        string `json:"model_version"`
	}

	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}

	if out.ModelName == "" {
		out.ModelName = modelName
	}
	if out.ModelVersion == "" {
		out.ModelVersion = modelVersion
	}

	if len(out.AudioData) == 0 && out.AudioPath == "" {
		return worker.ArtifactRef{}, worker.NewError("TTS_NO_AUDIO",
			"TTS synthesis produced no audio data",
			map[string]any{"stage": "tts", "command_id": cmd.ID})
	}

	return writeOutputArtifact(cmd, out)
}
func invokeCommand(ctx context.Context, binary string, args []string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal model request: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	stageTarget := strings.ToLower(filepath.Base(binary))
	if len(args) > 0 {
		stageTarget = strings.ToLower(filepath.Base(args[0]))
	}

	if err := cmd.Run(); err != nil {
		code := "EXEC_FAILED"
		if strings.Contains(stageTarget, "tts") || strings.Contains(stageTarget, "vieneu") || strings.Contains(stageTarget, "cosyvoice") || strings.Contains(stageTarget, "kokoro") || strings.Contains(stageTarget, "chatterbox") {
			code = "TTS_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "align") {
			code = "ALIGNER_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "diariz") || strings.Contains(stageTarget, "3dspeaker") || strings.Contains(stageTarget, "campplus") {
			code = "DIARIZER_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "asr") || strings.Contains(stageTarget, "qwen3") {
			code = "ASR_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "separator") || strings.Contains(stageTarget, "demucs") || strings.Contains(stageTarget, "uvr") {
			code = "SEPARATOR_EXEC_FAILED"
		}
		return worker.NewError(code, fmt.Sprintf("%s exited with error: %v; stderr: %s", binary, err, strings.TrimSpace(stderr.String())), nil)
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		code := "OUTPUT_INVALID"
		if strings.Contains(stageTarget, "tts") || strings.Contains(stageTarget, "vieneu") || strings.Contains(stageTarget, "cosyvoice") || strings.Contains(stageTarget, "kokoro") || strings.Contains(stageTarget, "chatterbox") {
			code = "TTS_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "align") {
			code = "ALIGNER_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "diariz") || strings.Contains(stageTarget, "3dspeaker") || strings.Contains(stageTarget, "campplus") {
			code = "DIARIZER_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "asr") || strings.Contains(stageTarget, "qwen3") {
			code = "ASR_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "separator") || strings.Contains(stageTarget, "demucs") || strings.Contains(stageTarget, "uvr") {
			code = "SEPARATOR_OUTPUT_INVALID"
		}
		return worker.NewError(code, fmt.Sprintf("%s returned invalid JSON: %v", binary, err), nil)
	}
	return nil
}

// invokeModel executes the model binary with a stdin JSON request and decodes
// the stdout JSON response into out.
func invokeModel(ctx context.Context, binary string, req any, out any) error {
	return invokeCommand(ctx, binary, nil, req, out)
}

type commandRunner struct {
	binary string
	args   []string
}

func resolveASRRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_ASR_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("ASR_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_ASR_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_ASR_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolvePythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("ASR_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_ASR_ADAPTER", nil)
		}
	}

	// Repo-owned Python adapter cmd/stageworker/adapters/asr_qwen3.py
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "asr_qwen3.py"),
		filepath.Join("adapters", "asr_qwen3.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "asr_qwen3.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "asr_qwen3.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "asr_qwen3.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolvePythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}

	// Compatibility fallback for explicitly installed wrappers/test fixtures.
	// Production prefers the repo-owned adapter over guessed executable names.
	for _, name := range []string{"qwen3-asr", "qwen-asr"} {
		if path, err := exec.LookPath(name); err == nil {
			return commandRunner{binary: path}, nil
		}
	}

	return commandRunner{}, worker.NewError("ASR_BINARY_NOT_FOUND",
		"Qwen3-ASR adapter or binary not available: install Qwen3-ASR (pip install qwen-asr) or configure DOUYINIE_ASR_ADAPTER/DOUYINIE_ASR_BIN",
		nil)
}

func resolveAlignerRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_ALIGNER_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("ALIGNER_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_ALIGNER_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_ALIGNER_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolvePythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("ALIGNER_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_ALIGNER_ADAPTER", nil)
		}
	}

	// Repo-owned Python adapter cmd/stageworker/adapters/aligner_qwen3.py
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "aligner_qwen3.py"),
		filepath.Join("adapters", "aligner_qwen3.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "aligner_qwen3.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "aligner_qwen3.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "aligner_qwen3.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolvePythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}

	// Compatibility fallback for explicitly installed wrappers/test fixtures.
	// Production prefers the repo-owned adapter over guessed executable names.
	for _, name := range []string{"qwen3-aligner", "qwen-aligner"} {
		if path, err := exec.LookPath(name); err == nil {
			return commandRunner{binary: path}, nil
		}
	}

	return commandRunner{}, worker.NewError("ALIGNER_BINARY_NOT_FOUND",
		"Qwen3-ForcedAligner adapter or binary not available: install Qwen3-ForcedAligner (pip install qwen-asr) or configure DOUYINIE_ALIGNER_ADAPTER/DOUYINIE_ALIGNER_BIN",
		nil)
}

func resolveDiarizerRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_DIARIZER_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("DIARIZER_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_DIARIZER_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_DIARIZER_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolvePythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("DIARIZER_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_DIARIZER_ADAPTER", nil)
		}
	}

	// Direct binaries on PATH (e.g. test fixtures or compiled wrappers)
	for _, name := range []string{"3dspeaker-diarizer", "diarizer-3dspeaker", "campplus-diarizer"} {
		if path, err := exec.LookPath(name); err == nil {
			return commandRunner{binary: path}, nil
		}
	}

	// Repo-owned Python adapter
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"),
		filepath.Join("adapters", "diarizer_3dspeaker.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "diarizer_3dspeaker.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolvePythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}

	return commandRunner{}, worker.NewError("DIARIZER_BINARY_NOT_FOUND",
		"3D-Speaker/CAM++ diarization adapter or binary not available: install 3D-Speaker/modelscope or configure DOUYINIE_DIARIZER_ADAPTER/DOUYINIE_DIARIZER_BIN",
		nil)
}

func resolveTTSRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_TTS_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("TTS_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_TTS_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_TTS_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolveTTSPythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("TTS_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_TTS_ADAPTER", nil)
		}
	}

	// Direct binaries on PATH
	for _, name := range []string{"vieneu-tts", "cosyvoice-tts", "kokoro-tts"} {
		if path, err := exec.LookPath(name); err == nil {
			return commandRunner{binary: path}, nil
		}
	}

	// Repo-owned Python adapter cmd/stageworker/adapters/tts_engine.py
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "tts_engine.py"),
		filepath.Join("adapters", "tts_engine.py"),
		filepath.Join("..", "..", "cmd", "stageworker", "adapters", "tts_engine.py"),
		filepath.Join("..", "cmd", "stageworker", "adapters", "tts_engine.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "tts_engine.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "tts_engine.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "tts_engine.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolveTTSPythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}
	return commandRunner{}, worker.NewError("TTS_BINARY_NOT_FOUND",
		"TTS adapter or binary not available: configure DOUYINIE_TTS_ADAPTER/DOUYINIE_TTS_BIN or ensure tts_engine.py dependencies are installed",
		nil)
}

func resolveSeparatorRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_SEPARATOR_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("SEPARATOR_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_SEPARATOR_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_SEPARATOR_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolveSeparatorPythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("SEPARATOR_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_SEPARATOR_ADAPTER", nil)
		}
	}
	// Repo-owned Python adapter cmd/stageworker/adapters/separator.py
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "separator.py"),
		filepath.Join("adapters", "separator.py"),
		filepath.Join("..", "..", "cmd", "stageworker", "adapters", "separator.py"),
		filepath.Join("..", "cmd", "stageworker", "adapters", "separator.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "separator.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "separator.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "separator.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolveSeparatorPythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}
	return commandRunner{}, worker.NewError("SEPARATOR_BINARY_NOT_FOUND",
		"audio separator adapter or binary not available: configure DOUYINIE_SEPARATOR_ADAPTER/DOUYINIE_SEPARATOR_BIN or install python-audio-separator/demucs",
		nil)
}

func runSeparatorAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	if len(cmd.Inputs) == 0 {
		return worker.ArtifactRef{}, worker.NewError("SEPARATOR_MISSING_INPUT",
			"separator command has no input audio artifact",
			map[string]any{"stage": "separator", "command_id": cmd.ID})
	}

	modelName, _ := cmd.Config[cfgModelName].(string)
	if modelName == "" {
		modelName = "UVR-MDX-NET-Inst_HQ_4.onnx"
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	if modelVersion == "" {
		modelVersion = "v3"
	}

	runner, err := resolveSeparatorRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"audio_path":    cmd.Inputs[0].Path,
		"run_id":        cmd.RunID,
		"attempt_id":    cmd.AttemptID,
		cfgModelName:    modelName,
		cfgModelVersion: modelVersion,
	}

	var out struct {
		VocalsData       string `json:"vocals_data"`
		BackgroundData   string `json:"background_data"`
		VocalsSHA256     string `json:"vocals_sha256"`
		BackgroundSHA256 string `json:"background_sha256"`
		DurationMs       int64  `json:"duration_ms"`
		SampleRate       int    `json:"sample_rate"`
		Channels         int    `json:"channels"`
		ModelName        string `json:"model_name"`
		ModelVersion     string `json:"model_version"`
	}

	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}

	if out.ModelName == "" {
		out.ModelName = modelName
	}
	if out.ModelVersion == "" {
		out.ModelVersion = modelVersion
	}

	return writeOutputArtifact(cmd, out)
}

func resolveOCRRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_OCR_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("OCR_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_OCR_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_OCR_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolveOCRPythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("OCR_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_OCR_ADAPTER", nil)
		}
	}
	// Repo-owned Python adapter cmd/stageworker/adapters/ocr.py
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "ocr.py"),
		filepath.Join("adapters", "ocr.py"),
		filepath.Join("..", "..", "cmd", "stageworker", "adapters", "ocr.py"),
		filepath.Join("..", "cmd", "stageworker", "adapters", "ocr.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "ocr.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "ocr.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "ocr.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolveOCRPythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}
	return commandRunner{}, worker.NewError("OCR_BINARY_NOT_FOUND",
		"visual OCR adapter or binary not available: configure DOUYINIE_OCR_ADAPTER/DOUYINIE_OCR_BIN or install paddleocr/ppocr",
		nil)
}

func runOCRAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	if len(cmd.Inputs) == 0 {
		return worker.ArtifactRef{}, worker.NewError("OCR_MISSING_INPUT",
			"ocr command has no input media artifact",
			map[string]any{"stage": "ocr", "command_id": cmd.ID})
	}

	modelName, _ := cmd.Config[cfgModelName].(string)
	if modelName == "" {
		modelName = "paddleocr-v6"
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	if modelVersion == "" {
		modelVersion = "v6"
	}

	runner, err := resolveOCRRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"media_path":           cmd.Inputs[0].Path,
		"run_id":               cmd.RunID,
		"attempt_id":           cmd.AttemptID,
		"frame_sample_step_ms": cmd.Config["frame_sample_step_ms"],
		"max_frames":           cmd.Config["max_frames"],
		cfgModelName:           modelName,
		cfgModelVersion:        modelVersion,
	}

	var out struct {
		FrameWidth        int                         `json:"frame_width"`
		FrameHeight       int                         `json:"frame_height"`
		FrameSampleStepMs int64                       `json:"frame_sample_step_ms"`
		Detections        []provider.RawTextDetection `json:"detections"`
		ModelName         string                      `json:"model_name"`
		ModelVersion      string                      `json:"model_version"`
	}

	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}

	if out.ModelName == "" {
		out.ModelName = modelName
	}
	if out.ModelVersion == "" {
		out.ModelVersion = modelVersion
	}

	return writeOutputArtifact(cmd, out)
}

func resolveOCRPythonBinary() string {
	if py := os.Getenv("DOUYINIE_OCR_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
	}
	return resolvePythonBinary()
}

func resolvePythonBinary() string {
	if py := os.Getenv("DOUYINIE_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
	}
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}
func resolveTTSPythonBinary() string {
	if py := os.Getenv("DOUYINIE_TTS_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
	}
	return resolvePythonBinary()
}
func resolveSeparatorPythonBinary() string {
	if py := os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
	}
	return resolvePythonBinary()
}

// runDiarizerAdapter runs the speaker-diarization adapter (Issue #44 Finding
// 3). A real invocation carries an input audio artifact and manifest-driven
// model identity; it fails closed on missing input, missing binary/runtime,
// missing identity, exec failure, or invalid/empty output. The output artifact
// carries absolute-millisecond speaker regions (no word labels: words are stamped
// host-side from independently produced alignments — evidence never depends on
// the SpeakerID this stage produces).
func runDiarizerAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	if len(cmd.Inputs) == 0 {
		return worker.ArtifactRef{}, worker.NewError("DIARIZER_MISSING_INPUT",
			"diarizer command has no input audio artifact",
			map[string]any{"stage": "diarize", "command_id": cmd.ID})
	}
	modelName, err := requireConfigString(cmd.Config, cfgModelName)
	if err != nil {
		return worker.ArtifactRef{}, worker.NewError("DIARIZER_MISSING_MODEL_IDENTITY",
			"diarizer command is missing required config 'model_name' (manifest/model registry driven)",
			map[string]any{"stage": "diarize", "command_id": cmd.ID})
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	vadModelName, _ := cmd.Config[cfgVADModelName].(string)
	if vadModelName == "" {
		vadModelName = defaultVADModelName
	}
	vadModelVersion, _ := cmd.Config[cfgVADModelVersion].(string)
	if vadModelVersion == "" {
		vadModelVersion = defaultVADModelVersion
	}

	runner, err := resolveDiarizerRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"mode":             "diarize",
		"audio_path":       cmd.Inputs[0].Path,
		"run_id":           cmd.RunID,
		"attempt_id":       cmd.AttemptID,
		cfgModelName:       modelName,
		cfgModelVersion:    modelVersion,
		cfgVADModelName:    vadModelName,
		cfgVADModelVersion: vadModelVersion,
	}
	var out struct {
		SpeakerAssignments []domain.SpeakerAssignment `json:"speaker_assignments"`
		ModelName          string                     `json:"model_name,omitempty"`
		ModelVersion       string                     `json:"model_version,omitempty"`
		VADModelName       string                     `json:"vad_model_name,omitempty"`
		VADModelVersion    string                     `json:"vad_model_version,omitempty"`
	}
	out.ModelName = modelName
	out.ModelVersion = modelVersion
	out.VADModelName = vadModelName
	out.VADModelVersion = vadModelVersion
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}
	if len(out.SpeakerAssignments) == 0 {
		return worker.ArtifactRef{}, worker.NewError("DIARIZER_NO_ASSIGNMENTS",
			"3D-Speaker diarizer produced no speaker assignments",
			map[string]any{"stage": "diarize", "command_id": cmd.ID})
	}
	return writeOutputArtifact(cmd, out)
}

// runDiarizerEvidenceAdapter runs the pre-diarization speaker-evidence probe
// adapter (Issue #44 Blocker 1 & 3). It invokes the diarization model to probe
// for multi-speaker cues and writes an immutable evidence artifact.
func runDiarizerEvidenceAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	if len(cmd.Inputs) == 0 {
		return worker.ArtifactRef{}, worker.NewError("DIARIZER_MISSING_INPUT",
			"diarize_evidence command has no input audio artifact",
			map[string]any{"stage": "diarize_evidence", "command_id": cmd.ID})
	}
	modelName, err := requireConfigString(cmd.Config, cfgModelName)
	if err != nil {
		return worker.ArtifactRef{}, worker.NewError("DIARIZER_MISSING_MODEL_IDENTITY",
			"diarize_evidence command is missing required config 'model_name' (manifest/model registry driven)",
			map[string]any{"stage": "diarize_evidence", "command_id": cmd.ID})
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	threshold, ok := cmd.Config["embedding_cosine_threshold"].(float64)
	if !ok || threshold <= 0 || threshold > 1 {
		return worker.ArtifactRef{}, worker.NewError("DIARIZER_MISSING_EVIDENCE_CONFIG",
			"diarize_evidence command requires embedding_cosine_threshold in (0,1]",
			map[string]any{"stage": "diarize_evidence", "command_id": cmd.ID})
	}
	vadModelName, _ := cmd.Config[cfgVADModelName].(string)
	if vadModelName == "" {
		vadModelName = defaultVADModelName
	}
	vadModelVersion, _ := cmd.Config[cfgVADModelVersion].(string)
	if vadModelVersion == "" {
		vadModelVersion = defaultVADModelVersion
	}

	runner, err := resolveDiarizerRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"mode":                       "evidence",
		"audio_path":                 cmd.Inputs[0].Path,
		"run_id":                     cmd.RunID,
		"attempt_id":                 cmd.AttemptID,
		cfgModelName:                 modelName,
		cfgModelVersion:              modelVersion,
		cfgVADModelName:              vadModelName,
		cfgVADModelVersion:           vadModelVersion,
		"embedding_cosine_threshold": threshold,
	}
	var out struct {
		SpeakerEvidence domain.SpeakerEvidence `json:"speaker_evidence"`
		ModelName       string                 `json:"model_name,omitempty"`
		ModelVersion    string                 `json:"model_version,omitempty"`
		VADModelName    string                 `json:"vad_model_name,omitempty"`
		VADModelVersion string                 `json:"vad_model_version,omitempty"`
	}
	out.ModelName = modelName
	out.ModelVersion = modelVersion
	out.VADModelName = vadModelName
	out.VADModelVersion = vadModelVersion
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}
	if out.SpeakerEvidence.Source == "" {
		out.SpeakerEvidence.Source = fmt.Sprintf("%s@%s+%s@%s", modelName, modelVersion, vadModelName, vadModelVersion)
	}
	return writeOutputArtifact(cmd, out)
}

// writeOutputArtifact writes the model's parsed response as JSON to the command
// output path and returns an ArtifactRef carrying the real SHA-256 of the bytes.
func writeOutputArtifact(cmd worker.Command, v any) (worker.ArtifactRef, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return worker.ArtifactRef{}, fmt.Errorf("marshal output artifact: %w", err)
	}
	if cmd.OutputPath != "" {
		if err := os.WriteFile(cmd.OutputPath, b, 0644); err != nil {
			return worker.ArtifactRef{}, fmt.Errorf("write output artifact: %w", err)
		}
	}
	sum := sha256.Sum256(b)
	return worker.ArtifactRef{
		SHA256: hex.EncodeToString(sum[:]),
		Path:   cmd.OutputPath,
	}, nil
}

// writeMarkerArtifact writes a stage marker to the output path. It is only
// used for unrecognized/neutral stages to preserve protocol lifecycle
// compatibility. The returned content address is the real SHA-256 of the
// marker bytes — placeholder hashes are never emitted.
func writeMarkerArtifact(cmd worker.Command) (worker.ArtifactRef, error) {
	marker := map[string]any{
		"command_id": cmd.ID,
		"stage":      cmd.Stage,
		"family":     cmd.Family,
		"status":     "executed",
	}
	return writeOutputArtifact(cmd, marker)
}
