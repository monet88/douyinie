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
	"strconv"
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
	// Validate model_snapshot envelope if present or if model snapshot is required (Issue #64)
	if snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config); parseErr != nil {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr), nil)
	} else if snapEnv != nil {
		if err := worker.ValidateSnapshotPaths(snapEnv); err != nil {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("snapshot path validation failed: %v", err), nil)
		}
	} else {
		isSeparatorProbe := cmd.Stage == "separator_probe" || (cmd.Stage == "separator" && cmd.Config["mode"] == "probe")
		if reqSnap, _ := cmd.Config["require_model_snapshot"].(bool); reqSnap && !isSeparatorProbe {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				"required model snapshot envelope missing from command config", nil)
		}
	}

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
	case "tts_probe":
		return runTTSProbeAdapter(ctx, cmd, enc)
	case "separator":
		if mode, _ := cmd.Config["mode"].(string); mode == "probe" {
			return runSeparatorProbeAdapter(ctx, cmd, enc)
		}
		return runSeparatorAdapter(ctx, cmd, enc)
	case "separator_probe":
		return runSeparatorProbeAdapter(ctx, cmd, enc)
	case "audio_role":
		if mode, _ := cmd.Config["mode"].(string); mode == "probe" {
			return runAudioRoleProbeAdapter(ctx, cmd, enc)
		}
		return runAudioRoleAdapter(ctx, cmd, enc)
	case "audio_role_probe":
		return runAudioRoleProbeAdapter(ctx, cmd, enc)
	case "ocr":
		return runOCRAdapter(ctx, cmd, enc)
	case "translation":
		return runTranslationAdapter(ctx, cmd, enc)
	default:
		// For unrecognized stages, fall back to the marker artifact placeholder.
		return writeMarkerArtifact(cmd)
	}
}

// resolvePrimarySnapshotPath parses the typed model_snapshot envelope for
// single-model stages (asr, aligner) and returns the verified primary
// LocalPath. A top-level unverified model_path can never override the typed
// envelope. When require_model_snapshot=true, a missing envelope or path
// fails closed with WORKER_SNAPSHOT_PATH_REQUIRED.
func resolvePrimarySnapshotPath(cmd worker.Command, stage string) (string, bool, error) {
	reqSnap, _ := cmd.Config["require_model_snapshot"].(bool)
	snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config)
	if parseErr != nil {
		return "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr),
			map[string]any{"stage": stage, "command_id": cmd.ID})
	}
	var modelPath string
	if snapEnv != nil {
		modelPath = snapEnv.Primary.LocalPath
		if topMP, ok := cmd.Config["model_path"].(string); ok && topMP != "" {
			if modelPath == "" || filepath.Clean(topMP) != filepath.Clean(modelPath) {
				return "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					fmt.Sprintf("top-level model_path (%s) attempts unverified override of model_snapshot envelope (%s)", topMP, modelPath),
					map[string]any{"stage": stage, "command_id": cmd.ID})
			}
		}
	} else if reqSnap {
		return "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("required model snapshot envelope missing from %s command config", stage),
			map[string]any{"stage": stage, "command_id": cmd.ID})
	}
	if reqSnap && modelPath == "" {
		return "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("%s requires verified primary snapshot local_path (got model_path=%q)", stage, modelPath),
			map[string]any{"stage": stage, "command_id": cmd.ID})
	}
	return modelPath, reqSnap, nil
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

	// Verified local snapshot is the source of truth for the model weights;
	// the Python adapter must prefer model_path over any Hub/model ID.
	modelPath, reqSnap, err := resolvePrimarySnapshotPath(cmd, "asr")
	if err != nil {
		return worker.ArtifactRef{}, err
	}
	runner, err := resolveASRRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	// stdin JSON request: audio_path is the machine-local input artifact path,
	// plus the declared model identity for observability end-to-end.
	req := map[string]any{
		"audio_path":             cmd.Inputs[0].Path,
		"run_id":                 cmd.RunID,
		"attempt_id":             cmd.AttemptID,
		cfgModelName:             modelName,
		cfgModelVersion:          modelVersion,
		"model_path":             modelPath,
		"require_model_snapshot": reqSnap,
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
	// Verified local snapshot is the source of truth for the model weights;
	// the Python adapter must prefer model_path over any Hub/model ID.
	modelPath, reqSnap, err := resolvePrimarySnapshotPath(cmd, "aligner")
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	runner, err := resolveAlignerRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	// stdin JSON request: audio_path + the accepted text to align + identity.
	req := map[string]any{
		"audio_path":             cmd.Inputs[0].Path,
		"text":                   text,
		"run_id":                 cmd.RunID,
		"attempt_id":             cmd.AttemptID,
		cfgModelName:             modelName,
		cfgModelVersion:          modelVersion,
		"model_path":             modelPath,
		"require_model_snapshot": reqSnap,
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
	snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config)
	if parseErr != nil {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr), nil)
	}
	var modelPath string
	var entrypointFile string
	if snapEnv != nil {
		modelPath = snapEnv.Primary.LocalPath
		entrypointFile = snapEnv.Primary.EntrypointFile
	} else if reqSnap, _ := cmd.Config["require_model_snapshot"].(bool); reqSnap {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			"required model snapshot envelope missing from tts command config", nil)
	}
	if modelPath == "" {
		if mp, ok := cmd.Config["model_path"].(string); ok && mp != "" {
			modelPath = mp
		}
	}
	// Top-level entrypoint_file cannot override the typed verified model_snapshot envelope.
	// If a top-level override is supplied that differs from the envelope, fail closed.
	if topEP, ok := cmd.Config["entrypoint_file"].(string); ok && topEP != "" {
		if entrypointFile == "" || filepath.Clean(topEP) != filepath.Clean(entrypointFile) {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("top-level entrypoint_file (%s) attempts unverified override of model_snapshot envelope entrypoint (%s)", topEP, entrypointFile),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
	}
	if modelPath != "" && entrypointFile != "" {
		rel, err := filepath.Rel(modelPath, entrypointFile)
		if err != nil || strings.HasPrefix(rel, "..") {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("TTS entrypoint file (%s) escapes snapshot root (%s)", entrypointFile, modelPath), nil)
		}
	}
	lowerModel := strings.ToLower(modelName)
	if lowerModel == strings.ToLower(provider.ZeroTTSModelID) {
		if modelVersion != provider.ZeroTTSModelVersion {
			return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_UNSUPPORTED",
				fmt.Sprintf("ZeroTTS requires exact model revision %s, got %s", provider.ZeroTTSModelVersion, modelVersion),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		speedValue := 1.0
		if strings.TrimSpace(speed) == "" {
			speed = "1.0"
		}
		if rawSpeed, ok := cmd.Config["speed"]; ok {
			switch v := rawSpeed.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					parsed, parseSpeedErr := strconv.ParseFloat(v, 64)
					if parseSpeedErr != nil {
						return worker.ArtifactRef{}, worker.NewError("TTS_SPEED_UNSUPPORTED",
							fmt.Sprintf("invalid ZeroTTS speed %q", v), map[string]any{"stage": "tts", "command_id": cmd.ID})
					}
					speed = v
					speedValue = parsed
				}
			case float64:
				speedValue = v
				speed = strconv.FormatFloat(v, 'f', -1, 64)
			case float32:
				speedValue = float64(v)
				speed = strconv.FormatFloat(float64(v), 'f', -1, 64)
			default:
				return worker.ArtifactRef{}, worker.NewError("TTS_SPEED_UNSUPPORTED",
					fmt.Sprintf("invalid ZeroTTS speed type %T", rawSpeed), map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
		}
		if speedValue != 1.0 {
			return worker.ArtifactRef{}, worker.NewError("TTS_SPEED_UNSUPPORTED",
				fmt.Sprintf("ZeroTTS requires speed=1.0, got %s", speed),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		validZeroTTS := false
		for _, v := range domain.FrozenZeroTTSVoiceOrder {
			if v == voiceID {
				validZeroTTS = true
				break
			}
		}
		if !validZeroTTS {
			return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
				fmt.Sprintf("unverified or unknown ZeroTTS voice preset %q (must be one of %s)", voiceID, strings.Join(domain.FrozenZeroTTSVoiceOrder, ", ")),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		if modelPath == "" || snapEnv == nil {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				"ZeroTTS requires a verified local model_snapshot envelope", map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		if snapEnv.Primary.DependencyName != provider.ZeroTTSModelID || snapEnv.Primary.Version != provider.ZeroTTSModelVersion {
			return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_UNSUPPORTED",
				fmt.Sprintf("ZeroTTS snapshot envelope identity must be %s:%s, got %s:%s",
					provider.ZeroTTSModelID, provider.ZeroTTSModelVersion,
					snapEnv.Primary.DependencyName, snapEnv.Primary.Version),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		if entrypointFile == "" {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				"verified model_snapshot envelope missing required entrypoint_file for ZeroTTS",
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		for _, rel := range []string{
			"config.json",
			"tokenizer.json",
			"null_voice_emb.npy",
			filepath.Join("onnx", "text_encoder.onnx"),
			filepath.Join("onnx", "prefix_step.onnx"),
			filepath.Join("onnx", "local_frame_decode.onnx"),
			filepath.Join("voices", "index.json"),
		} {
			assetPath := filepath.Join(modelPath, rel)
			if fi, err := os.Stat(assetPath); err != nil || fi.IsDir() {
				return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_ASSET_MISSING",
					fmt.Sprintf("ZeroTTS required model asset missing: %s", assetPath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
		}
		codecDir := filepath.Join(modelPath, "onnx", "codec")
		codecEntries, codecErr := os.ReadDir(codecDir)
		if codecErr != nil || len(codecEntries) == 0 {
			return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_ASSET_MISSING",
				fmt.Sprintf("ZeroTTS codec assets missing: %s", codecDir),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		voicePath := filepath.Join(modelPath, "voices", voiceID, "voice.npz")
		if fi, err := os.Stat(voicePath); err != nil || fi.IsDir() {
			return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
				fmt.Sprintf("ZeroTTS voice asset missing from canonical snapshot path: %s", voicePath),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		if filepath.Clean(entrypointFile) != filepath.Clean(voicePath) {
			return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
				fmt.Sprintf("ZeroTTS envelope entrypoint (%s) does not match canonical voice path (%s)", entrypointFile, voicePath),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
	} else if strings.Contains(lowerModel, "zerotts") || strings.Contains(lowerModel, "zeroweight") {
		return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_UNSUPPORTED",
			fmt.Sprintf("unsupported ZeroTTS model identity %q; expected %s", modelName, provider.ZeroTTSModelID),
			map[string]any{"stage": "tts", "command_id": cmd.ID})
	} else if strings.Contains(lowerModel, "kokoro") {
		validKokoro := false
		for _, v := range domain.FrozenKokoroVoiceOrder {
			if v == voiceID {
				validKokoro = true
				break
			}
		}
		if !validKokoro {
			return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
				fmt.Sprintf("unverified or unknown Kokoro voice preset %q (must be one of %s)", voiceID, strings.Join(domain.FrozenKokoroVoiceOrder, ", ")),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		if modelPath != "" || snapEnv != nil {
			// Exact RC Kokoro hard route requires envelope-derived entrypoint
			if entrypointFile == "" {
				return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					"verified model_snapshot envelope missing required entrypoint_file for Kokoro TTS",
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
			// Require manifest-declared, host-verified canonical in-root paths:
			// 1. config.json in root
			configPath := filepath.Join(modelPath, "config.json")
			if fi, err := os.Stat(configPath); err != nil || fi.IsDir() {
				return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_ASSET_MISSING",
					fmt.Sprintf("Kokoro config.json missing in snapshot root: %s", configPath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
			// 2. Canonical voice asset: voices/<voiceID>.pt
			voicePath := filepath.Join(modelPath, "voices", voiceID+".pt")
			if fi, err := os.Stat(voicePath); err != nil || fi.IsDir() {
				return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
					fmt.Sprintf("Kokoro voice asset missing from canonical snapshot path: %s", voicePath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
			if filepath.Clean(entrypointFile) != filepath.Clean(voicePath) {
				return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
					fmt.Sprintf("Kokoro envelope entrypoint file (%s) does not match canonical voice path (%s)", entrypointFile, voicePath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
		}
	} else if strings.Contains(lowerModel, "vieneu") || strings.Contains(lowerModel, "pnnbao") {
		validVieNeu := false
		for _, v := range domain.FrozenVieNeuVoiceOrder {
			if v == voiceID {
				validVieNeu = true
				break
			}
		}
		if !validVieNeu {
			return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
				fmt.Sprintf("unverified or unknown VieNeu voice preset %q (must be one of %s)", voiceID, strings.Join(domain.FrozenVieNeuVoiceOrder, ", ")),
				map[string]any{"stage": "tts", "command_id": cmd.ID})
		}
		if modelPath != "" || snapEnv != nil {
			// Exact RC VieNeu hard route requires envelope-derived entrypoint
			if entrypointFile == "" {
				return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					"verified model_snapshot envelope missing required entrypoint_file for VieNeu TTS",
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
			// Require manifest-declared, host-verified canonical in-root paths:
			// 1. Canonical voice catalog: src/vieneu/assets/voices_v3_turbo.json
			catalogPath := filepath.Join(modelPath, "src", "vieneu", "assets", "voices_v3_turbo.json")
			if fi, err := os.Stat(catalogPath); err != nil || fi.IsDir() {
				return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
					fmt.Sprintf("VieNeu v3 Turbo voice catalog (src/vieneu/assets/voices_v3_turbo.json) missing from snapshot: %s", modelPath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
			if filepath.Clean(entrypointFile) != filepath.Clean(catalogPath) {
				return worker.ArtifactRef{}, worker.NewError("TTS_VOICE_ASSET_MISSING",
					fmt.Sprintf("VieNeu envelope entrypoint file (%s) does not match canonical catalog path (%s)", entrypointFile, catalogPath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
			// 2. Fixed in-root verified MOSS tokenizer layout: moss_tokenizer/
			mossPath := filepath.Join(modelPath, "moss_tokenizer")
			if _, err := os.Stat(mossPath); err != nil {
				return worker.ArtifactRef{}, worker.NewError("TTS_MODEL_ASSET_MISSING",
					fmt.Sprintf("VieNeu fixed in-root verified MOSS tokenizer layout missing from snapshot: %s", mossPath),
					map[string]any{"stage": "tts", "command_id": cmd.ID})
			}
		}
	}

	var runner commandRunner
	if lowerModel == strings.ToLower(provider.ZeroTTSModelID) {
		runner, err = resolvePinnedZeroTTSRunner()
	} else {
		runner, err = resolveTTSRunner()
	}
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
		"model_path":       modelPath,
		"entrypoint_file":  entrypointFile,
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

// runTTSProbeAdapter observes the configured ZeroTTS runtime through the
// repo-owned TTS adapter and returns verifiable package/source identity.
func runTTSProbeAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	modelName, err := requireConfigString(cmd.Config, cfgModelName)
	if err != nil || !strings.EqualFold(modelName, provider.ZeroTTSModelID) {
		return worker.ArtifactRef{}, worker.NewError("TTS_RUNTIME_PROBE_FAILED",
			fmt.Sprintf("ZeroTTS runtime probe requires exact model identity %s", provider.ZeroTTSModelID),
			map[string]any{"stage": "tts_probe", "command_id": cmd.ID})
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	if modelVersion != provider.ZeroTTSModelVersion {
		return worker.ArtifactRef{}, worker.NewError("TTS_RUNTIME_PROBE_FAILED",
			fmt.Sprintf("ZeroTTS runtime probe requires exact model revision %s, got %s", provider.ZeroTTSModelVersion, modelVersion),
			map[string]any{"stage": "tts_probe", "command_id": cmd.ID})
	}

	runner, err := resolvePinnedZeroTTSRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}
	req := map[string]any{
		"mode":          "probe",
		"run_id":        cmd.RunID,
		"attempt_id":    cmd.AttemptID,
		cfgModelName:    provider.ZeroTTSModelID,
		cfgModelVersion: provider.ZeroTTSModelVersion,
	}
	var out struct {
		Status          string            `json:"status"`
		PackageName     string            `json:"package_name"`
		PackageVersion  string            `json:"package_version"`
		SourceRevision  string            `json:"source_revision"`
		RuntimeVersions map[string]string `json:"runtime_versions"`
		AdapterRevision string            `json:"adapter_revision"`
		Error           string            `json:"error,omitempty"`
	}
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, worker.NewError("TTS_RUNTIME_PROBE_FAILED",
			fmt.Sprintf("ZeroTTS runtime probe failed: %v", err),
			map[string]any{"stage": "tts_probe", "command_id": cmd.ID})
	}
	if out.Error != "" || out.Status != "ok" || out.SourceRevision == "" {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("invalid ZeroTTS runtime probe response status=%q source_revision=%q", out.Status, out.SourceRevision)
		}
		return worker.ArtifactRef{}, worker.NewError("TTS_RUNTIME_PROBE_FAILED", msg,
			map[string]any{"stage": "tts_probe", "command_id": cmd.ID})
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
	// Strict offline execution for model-backed requests (Issue #64)
	cmd.Env = append(os.Environ(), "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "MODELSCOPE_OFFLINE=1")
	stageTarget := strings.ToLower(filepath.Base(binary))
	if len(args) > 0 {
		stageTarget = strings.ToLower(filepath.Base(args[0]))
	}

	if err := cmd.Run(); err != nil {
		code := "EXEC_FAILED"
		stderrStr := strings.TrimSpace(stderr.String())
		if strings.Contains(stderrStr, "TTS_VOICE_ASSET_MISSING") {
			code = "TTS_VOICE_ASSET_MISSING"
		} else if strings.Contains(stderrStr, "TTS_MODEL_ASSET_MISSING") {
			code = "TTS_MODEL_ASSET_MISSING"
		} else if strings.Contains(stderrStr, "TTS_RUNTIME_MISSING") {
			code = "TTS_RUNTIME_MISSING"
		} else if strings.Contains(stderrStr, "TTS_RUNTIME_VERSION_MISMATCH") {
			code = "TTS_RUNTIME_VERSION_MISMATCH"
		} else if strings.Contains(stderrStr, "TTS_SPEED_UNSUPPORTED") {
			code = "TTS_SPEED_UNSUPPORTED"
		} else if strings.Contains(stderrStr, "TTS_NO_AUDIO") {
			code = "TTS_NO_AUDIO"
		} else if strings.Contains(stderrStr, "TTS_MODEL_UNSUPPORTED") {
			code = "TTS_MODEL_UNSUPPORTED"
		} else if strings.Contains(stderrStr, "WORKER_SNAPSHOT_PATH_REQUIRED") {
			code = "WORKER_SNAPSHOT_PATH_REQUIRED"
		} else if strings.Contains(stderrStr, "SEPARATOR_METADATA_ASSET_MISSING") {
			code = "SEPARATOR_METADATA_ASSET_MISSING"
		} else if strings.Contains(stderrStr, "SEPARATOR_METADATA_INVALID") {
			code = "SEPARATOR_METADATA_INVALID"
		} else if strings.Contains(stderrStr, "SEPARATOR_MODEL_ASSET_MISSING") {
			code = "SEPARATOR_MODEL_ASSET_MISSING"
		} else if strings.Contains(stderrStr, "SEPARATOR_PACKAGE_VERSION_MISMATCH") {
			code = "SEPARATOR_PACKAGE_VERSION_MISMATCH"
		} else if strings.Contains(stderrStr, "DEMUCS_PACKAGE_VERSION_MISMATCH") {
			code = "DEMUCS_PACKAGE_VERSION_MISMATCH"
		} else if strings.Contains(stderrStr, "DEMUCS_FT_SUBSTITUTION_REJECTED") {
			code = "DEMUCS_FT_SUBSTITUTION_REJECTED"
		} else if strings.Contains(stageTarget, "trans") {
			code = "TRANSLATION_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "tts") || strings.Contains(stageTarget, "zerotts") || strings.Contains(stageTarget, "vieneu") || strings.Contains(stageTarget, "cosyvoice") || strings.Contains(stageTarget, "kokoro") || strings.Contains(stageTarget, "chatterbox") {
			code = "TTS_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "align") {
			code = "ALIGNER_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "diariz") || strings.Contains(stageTarget, "3dspeaker") || strings.Contains(stageTarget, "campplus") {
			code = "DIARIZER_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "asr") || strings.Contains(stageTarget, "qwen3") {
			code = "ASR_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "separator") || strings.Contains(stageTarget, "demucs") || strings.Contains(stageTarget, "uvr") {
			code = "SEPARATOR_EXEC_FAILED"
		} else if strings.Contains(stageTarget, "audio_role") || strings.Contains(stageTarget, "yamnet") {
			code = "AUDIO_ROLE_EXEC_FAILED"
		}
		return worker.NewError(code, fmt.Sprintf("%s exited with error: %v; stderr: %s", binary, err, stderrStr), nil)
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		code := "OUTPUT_INVALID"
		if strings.Contains(stageTarget, "trans") {
			code = "TRANSLATION_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "tts") || strings.Contains(stageTarget, "zerotts") || strings.Contains(stageTarget, "vieneu") || strings.Contains(stageTarget, "cosyvoice") || strings.Contains(stageTarget, "kokoro") || strings.Contains(stageTarget, "chatterbox") {
			code = "TTS_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "align") {
			code = "ALIGNER_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "diariz") || strings.Contains(stageTarget, "3dspeaker") || strings.Contains(stageTarget, "campplus") {
			code = "DIARIZER_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "asr") || strings.Contains(stageTarget, "qwen3") {
			code = "ASR_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "separator") || strings.Contains(stageTarget, "demucs") || strings.Contains(stageTarget, "uvr") {
			code = "SEPARATOR_OUTPUT_INVALID"
		} else if strings.Contains(stageTarget, "audio_role") || strings.Contains(stageTarget, "yamnet") {
			code = "AUDIO_ROLE_OUTPUT_INVALID"
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

	// Repo-owned Python adapters: check SenseVoice-Small ONNX first (lightweight), fallback to Qwen3-ASR
	adapterCandidates := []string{"asr_sensevoice.py", "asr_qwen3.py"}
	var adapterPaths []string
	for _, name := range adapterCandidates {
		adapterPaths = append(adapterPaths,
			filepath.Join("cmd", "stageworker", "adapters", name),
			filepath.Join("adapters", name),
		)
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			adapterPaths = append(adapterPaths,
				filepath.Join(exeDir, "adapters", name),
				filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", name),
				filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", name),
			)
		}
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

	// Issue #106: the diarizer family resolves its own interpreter ahead of the
	// shared one, because it needs the venv carrying modelscope/torchaudio while
	// asr/aligner use the shared venv. Resolved up front so a misconfigured
	// value fails closed with the variable name and path instead of any later
	// lookup silently substituting another interpreter.
	diarizerPy, err := resolveDiarizerPythonBinary()
	if err != nil {
		return commandRunner{}, err
	}

	if script := os.Getenv("DOUYINIE_DIARIZER_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			if diarizerPy != "" {
				return commandRunner{binary: diarizerPy, args: []string{script}}, nil
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

	// Repo-owned Python adapters: check Sherpa-ONNX first (lightweight), fallback to 3D-Speaker
	adapterCandidates := []string{"diarizer_sherpa_onnx.py", "diarizer_3dspeaker.py"}
	var adapterPaths []string
	for _, name := range adapterCandidates {
		adapterPaths = append(adapterPaths,
			filepath.Join("cmd", "stageworker", "adapters", name),
			filepath.Join("adapters", name),
		)
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			adapterPaths = append(adapterPaths,
				filepath.Join(exeDir, "adapters", name),
				filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", name),
				filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", name),
			)
		}
	}
	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				if diarizerPy != "" {
					return commandRunner{binary: diarizerPy, args: []string{absP}}, nil
				}
			}
		}
	}

	return commandRunner{}, worker.NewError("DIARIZER_BINARY_NOT_FOUND",
		"3D-Speaker/CAM++ diarization adapter or binary not available: install 3D-Speaker/modelscope or configure DOUYINIE_DIARIZER_PYTHON_BIN/DOUYINIE_DIARIZER_ADAPTER/DOUYINIE_DIARIZER_BIN",
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

// resolvePinnedZeroTTSRunner deliberately ignores DOUYINIE_TTS_BIN,
// DOUYINIE_TTS_ADAPTER, and PATH TTS wrappers. ZeroTTS always runs through the
// repo-owned adapter; only the Python interpreter is configurable.
func resolvePinnedZeroTTSRunner() (commandRunner, error) {
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

	pyBin := ""
	if configured := strings.TrimSpace(os.Getenv("DOUYINIE_TTS_PYTHON_BIN")); configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			pyBin = path
		} else if fi, statErr := os.Stat(configured); statErr == nil && !fi.IsDir() {
			pyBin = configured
		} else {
			return commandRunner{}, worker.NewError("TTS_RUNTIME_MISSING",
				fmt.Sprintf("configured DOUYINIE_TTS_PYTHON_BIN %q is unavailable", configured), nil)
		}
	} else {
		pyBin = resolvePythonBinary()
	}
	if pyBin == "" {
		return commandRunner{}, worker.NewError("TTS_RUNTIME_MISSING",
			"Python runtime not found for pinned ZeroTTS adapter", nil)
	}
	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if fi, statErr := os.Stat(absP); statErr == nil && !fi.IsDir() {
				return commandRunner{binary: pyBin, args: []string{absP}}, nil
			}
		}
	}
	return commandRunner{}, worker.NewError("TTS_RUNTIME_MISSING",
		"repo-owned ZeroTTS adapter cmd/stageworker/adapters/tts_engine.py not found", nil)
}

func resolveSeparatorRunner(requireSnap bool) (commandRunner, error) {
	// Find repo-owned Python adapter cmd/stageworker/adapters/separator.py
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

	var repoAdapterPath string
	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if fi, err := os.Stat(absP); err == nil && !fi.IsDir() {
				repoAdapterPath = absP
				break
			}
		}
	}

	if bin := os.Getenv("DOUYINIE_SEPARATOR_BIN"); bin != "" {
		if requireSnap {
			return commandRunner{}, worker.NewError("SEPARATOR_RUNNER_OVERRIDE_REJECTED",
				fmt.Sprintf("arbitrary runner binary DOUYINIE_SEPARATOR_BIN=%q cannot self-attest RC provenance on snapshot-required route", bin),
				nil)
		}
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("SEPARATOR_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_SEPARATOR_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_SEPARATOR_ADAPTER"); script != "" {
		if fi, err := os.Stat(script); err == nil && !fi.IsDir() {
			absScript, _ := filepath.Abs(script)
			isRepo := false
			if repoAdapterPath != "" {
				if strings.EqualFold(filepath.Clean(absScript), filepath.Clean(repoAdapterPath)) {
					isRepo = true
				} else if rfi, rerr := os.Stat(repoAdapterPath); rerr == nil && os.SameFile(fi, rfi) {
					isRepo = true
				}
			}

			if requireSnap && !isRepo {
				return commandRunner{}, worker.NewError("SEPARATOR_RUNNER_OVERRIDE_REJECTED",
					fmt.Sprintf("arbitrary runner adapter DOUYINIE_SEPARATOR_ADAPTER=%q cannot self-attest RC provenance on snapshot-required route (repo-owned adapter required)", script),
					nil)
			}

			pyBin := resolveSeparatorPythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{absScript}}, nil
			}
			return commandRunner{}, worker.NewError("SEPARATOR_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_SEPARATOR_ADAPTER", nil)
		}
		return commandRunner{}, worker.NewError("SEPARATOR_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_SEPARATOR_ADAPTER script %q not found", script), nil)
	}

	if repoAdapterPath != "" {
		pyBin := resolveSeparatorPythonBinary()
		if pyBin != "" {
			return commandRunner{binary: pyBin, args: []string{repoAdapterPath}}, nil
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

	snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config)
	if parseErr != nil {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr), nil)
	}
	var modelPath string
	var entrypointFile string
	if snapEnv != nil {
		modelPath = snapEnv.Primary.LocalPath
		entrypointFile = snapEnv.Primary.EntrypointFile
	} else if reqSnap, _ := cmd.Config["require_model_snapshot"].(bool); reqSnap {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			"required model snapshot envelope missing from separator command config", nil)
	}
	if modelPath == "" {
		if mp, ok := cmd.Config["model_path"].(string); ok && mp != "" {
			modelPath = mp
		}
	}
	// Top-level entrypoint_file cannot override the typed verified model_snapshot envelope.
	// If a top-level override is supplied that differs from the envelope, fail closed.
	if topEP, ok := cmd.Config["entrypoint_file"].(string); ok && topEP != "" {
		if entrypointFile == "" || filepath.Clean(topEP) != filepath.Clean(entrypointFile) {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("top-level entrypoint_file (%s) attempts unverified override of model_snapshot envelope entrypoint (%s)", topEP, entrypointFile),
				map[string]any{"stage": "separator", "command_id": cmd.ID})
		}
	}
	if modelPath != "" && entrypointFile != "" {
		rel, err := filepath.Rel(modelPath, entrypointFile)
		if err != nil || strings.HasPrefix(rel, "..") {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("separator entrypoint file (%s) escapes snapshot root (%s)", entrypointFile, modelPath), nil)
		}
	}

	lowerModel := strings.ToLower(modelName)
	var metadataFile string
	if snapEnv != nil {
		for _, dep := range snapEnv.Dependencies {
			if strings.EqualFold(dep.Role, "model_metadata") || strings.EqualFold(dep.DependencyName, "mdx_model_data.json") {
				metadataFile = dep.EntrypointFile
				break
			}
		}
	}
	if metadataFile == "" {
		if mFile, ok := cmd.Config["metadata_file"].(string); ok && mFile != "" {
			metadataFile = mFile
		}
	}

	if strings.Contains(lowerModel, "uvr") || strings.Contains(lowerModel, "mdx") {
		if snapEnv != nil || cmd.Config["require_model_snapshot"] == true {
			if entrypointFile == "" {
				return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					"UVR separator requires verified UVR-MDX-NET-Inst_HQ_4.onnx entrypoint in model snapshot",
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
			if !strings.EqualFold(filepath.Base(entrypointFile), "UVR-MDX-NET-Inst_HQ_4.onnx") {
				return worker.ArtifactRef{}, worker.NewError("SEPARATOR_MODEL_ASSET_MISSING",
					fmt.Sprintf("unverified UVR entrypoint %s (expected UVR-MDX-NET-Inst_HQ_4.onnx)", filepath.Base(entrypointFile)),
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
			if _, err := os.Stat(entrypointFile); err != nil {
				return worker.ArtifactRef{}, worker.NewError("SEPARATOR_MODEL_ASSET_MISSING",
					fmt.Sprintf("UVR artifact file missing from snapshot: %s", entrypointFile),
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
			if metadataFile == "" {
				return worker.ArtifactRef{}, worker.NewError("SEPARATOR_METADATA_ASSET_MISSING",
					"UVR separator requires verified model metadata asset (mdx_model_data.json) in model snapshot",
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
			if _, err := os.Stat(metadataFile); err != nil {
				return worker.ArtifactRef{}, worker.NewError("SEPARATOR_METADATA_ASSET_MISSING",
					fmt.Sprintf("UVR model metadata file missing from snapshot: %s", metadataFile),
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
		}
	} else if strings.Contains(lowerModel, "demucs") {
		if strings.Contains(lowerModel, "htdemucs_ft") {
			return worker.ArtifactRef{}, worker.NewError("DEMUCS_FT_SUBSTITUTION_REJECTED",
				"htdemucs_ft bag is rejected as htdemucs fallback; exact htdemucs required",
				map[string]any{"stage": "separator", "command_id": cmd.ID})
		}
		if snapEnv != nil || cmd.Config["require_model_snapshot"] == true {
			if entrypointFile == "" {
				return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					"Demucs separator requires verified 955717e8-8726e21a.th entrypoint in model snapshot",
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
			if !strings.Contains(strings.ToLower(filepath.Base(entrypointFile)), "955717e8") {
				return worker.ArtifactRef{}, worker.NewError("SEPARATOR_MODEL_ASSET_MISSING",
					fmt.Sprintf("unverified Demucs entrypoint %s (expected 955717e8-8726e21a.th)", filepath.Base(entrypointFile)),
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
			if _, err := os.Stat(entrypointFile); err != nil {
				return worker.ArtifactRef{}, worker.NewError("SEPARATOR_MODEL_ASSET_MISSING",
					fmt.Sprintf("Demucs checkpoint file missing from snapshot: %s", entrypointFile),
					map[string]any{"stage": "separator", "command_id": cmd.ID})
			}
		}
	}

	requireSnap, _ := cmd.Config[worker.ConfigKeyRequireModelSnapshot].(bool)
	if !requireSnap {
		requireSnap, _ = cmd.Config["require_model_snapshot"].(bool)
	}
	runner, err := resolveSeparatorRunner(requireSnap)
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	hasVerifiedBagYAML := false
	if snapEnv != nil {
		for _, dep := range snapEnv.Dependencies {
			if strings.EqualFold(dep.DependencyName, "htdemucs.yaml") || strings.EqualFold(dep.Role, "bag_yaml") {
				hasVerifiedBagYAML = true
				break
			}
		}
	}
	if b, ok := cmd.Config["has_verified_bag_yaml"].(bool); ok && b {
		hasVerifiedBagYAML = true
	}

	req := map[string]any{
		"audio_path":             cmd.Inputs[0].Path,
		"run_id":                 cmd.RunID,
		"attempt_id":             cmd.AttemptID,
		"model_path":             modelPath,
		"entrypoint_file":        entrypointFile,
		"metadata_file":          metadataFile,
		"require_model_snapshot": cmd.Config["require_model_snapshot"],
		"has_verified_bag_yaml":  hasVerifiedBagYAML,
		"model_snapshot":         snapEnv,
		cfgModelName:             modelName,
		cfgModelVersion:          modelVersion,
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
		RuntimeIdentity  string `json:"runtime_identity"`
	}
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}

	if out.ModelName == "" {
		if snapEnv != nil && snapEnv.Primary.DependencyName != "" {
			out.ModelName = snapEnv.Primary.DependencyName
		} else {
			out.ModelName = modelName
		}
	}
	if out.ModelVersion == "" {
		if snapEnv != nil && snapEnv.Primary.Version != "" {
			out.ModelVersion = snapEnv.Primary.Version
		} else {
			out.ModelVersion = modelVersion
		}
	}
	return writeOutputArtifact(cmd, out)
}

// runSeparatorProbeAdapter probes the configured separator Python runtime environment
// for installed distribution versions, exact PEP 610 VCS source revision, and backend versions.
// It fails closed if the runner is missing, execution fails, or the probe cannot prove
// the pinned source revision from trustworthy metadata.
func runSeparatorProbeAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	modelName, _ := cmd.Config[cfgModelName].(string)
	if modelName == "" {
		modelName = "UVR-MDX-NET-Inst_HQ_4.onnx"
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	if modelVersion == "" {
		modelVersion = "v3"
	}

	requireSnap, _ := cmd.Config[worker.ConfigKeyRequireModelSnapshot].(bool)
	if !requireSnap {
		requireSnap, _ = cmd.Config["require_model_snapshot"].(bool)
	}
	runner, err := resolveSeparatorRunner(requireSnap)
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"mode":          "probe",
		"run_id":        cmd.RunID,
		"attempt_id":    cmd.AttemptID,
		cfgModelName:    modelName,
		cfgModelVersion: modelVersion,
	}

	var out struct {
		Status          string            `json:"status"`
		PackageName     string            `json:"package_name"`
		PackageVersion  string            `json:"package_version"`
		SourceRevision  string            `json:"source_revision"`
		RuntimeVersions map[string]string `json:"runtime_versions"`
		AdapterRevision string            `json:"adapter_revision"`
		Error           string            `json:"error,omitempty"`
	}
	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, worker.NewError("SEPARATOR_RUNTIME_PROBE_FAILED",
			fmt.Sprintf("separator runtime probe failed: %v", err),
			map[string]any{"stage": "separator_probe", "command_id": cmd.ID})
	}
	if out.Error != "" {
		return worker.ArtifactRef{}, worker.NewError("SEPARATOR_RUNTIME_PROBE_FAILED",
			out.Error,
			map[string]any{"stage": "separator_probe", "command_id": cmd.ID})
	}
	if out.SourceRevision == "" {
		return worker.ArtifactRef{}, worker.NewError("SEPARATOR_RUNTIME_PROBE_FAILED",
			"separator runtime probe returned empty source revision",
			map[string]any{"stage": "separator_probe", "command_id": cmd.ID})
	}
	return writeOutputArtifact(cmd, out)
}

func runAudioRoleAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	modelName, _ := cmd.Config[cfgModelName].(string)
	if modelName == "" {
		modelName = "yamnet"
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	if modelVersion == "" {
		modelVersion = "v1"
	}

	snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config)
	if parseErr != nil {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr), nil)
	}
	var modelPath, entrypointFile string
	if snapEnv != nil {
		modelPath = snapEnv.Primary.LocalPath
		entrypointFile = snapEnv.Primary.EntrypointFile
		if entrypointFile == "" {
			entrypointFile = snapEnv.EntrypointFile
		}
	} else if reqSnap, _ := cmd.Config["require_model_snapshot"].(bool); reqSnap {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			"required model snapshot envelope missing from audio_role command config", nil)
	}
	if modelPath == "" {
		if mp, ok := cmd.Config["model_path"].(string); ok && mp != "" {
			modelPath = mp
		}
	}

	reqSnap, _ := cmd.Config["require_model_snapshot"].(bool)
	if reqSnap {
		if modelPath == "" {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				"audio_role requires verified primary yamnet snapshot local_path",
				map[string]any{"stage": "audio_role", "command_id": cmd.ID})
		}
		if entrypointFile == "" {
			if fi, err := os.Stat(modelPath); err == nil && !fi.IsDir() {
				entrypointFile = modelPath
			} else {
				entrypointFile = filepath.Join(modelPath, "yamnet.tflite")
			}
		}
		if rel, err := filepath.Rel(modelPath, entrypointFile); err != nil || strings.HasPrefix(rel, "..") {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("YAMNet entrypoint file (%s) escapes snapshot root (%s)", entrypointFile, modelPath), nil)
		}
		if _, err := os.Stat(entrypointFile); err != nil {
			return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_MODEL_ASSET_MISSING",
				fmt.Sprintf("yamnet.tflite entrypoint missing from snapshot path: %s", entrypointFile),
				map[string]any{"stage": "audio_role", "command_id": cmd.ID})
		}
	} else if modelPath != "" {
		if _, err := os.Stat(modelPath); err != nil {
			return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_MODEL_ASSET_MISSING",
				fmt.Sprintf("model path inaccessible: %s", modelPath), nil)
		}
	}
	runner, err := resolveAudioRoleRunner(reqSnap)
	if err != nil {
		return worker.ArtifactRef{}, err
	}

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

	if vocalsAudio == "" && srcAudio == "" {
		return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_MISSING_INPUT",
			"audio_role command missing audio input (vocals_audio or source_audio required)",
			map[string]any{"stage": "audio_role", "command_id": cmd.ID})
	}

	req := map[string]any{
		"mode":             "analyze",
		"run_id":           cmd.RunID,
		"attempt_id":       cmd.AttemptID,
		"vocals_audio":     vocalsAudio,
		"background_audio": bgAudio,
		"source_audio":     srcAudio,
		"model_path": func() string {
			if entrypointFile != "" {
				return entrypointFile
			}
			return modelPath
		}(),
		"config": cmd.Config["config"],
	}

	var out struct {
		Segments        []domain.AudioSegment `json:"segments"`
		ModelName       string                `json:"model_name"`
		ModelVersion    string                `json:"model_version"`
		RuntimeIdentity string                `json:"runtime_identity"`
		ProviderID      string                `json:"provider_id"`
	}

	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}

	return writeOutputArtifact(cmd, out)
}

func runAudioRoleProbeAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	reqSnap, _ := cmd.Config["require_model_snapshot"].(bool)
	runner, err := resolveAudioRoleRunner(reqSnap)
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"mode":       "probe",
		"run_id":     cmd.RunID,
		"attempt_id": cmd.AttemptID,
	}

	var out struct {
		Status          string            `json:"status"`
		PackageName     string            `json:"package_name"`
		PackageVersion  string            `json:"package_version"`
		SourceRevision  string            `json:"source_revision"`
		RuntimeVersions map[string]string `json:"runtime_versions"`
		AdapterRevision string            `json:"adapter_revision"`
		Error           string            `json:"error,omitempty"`
	}

	if err := invokeCommand(ctx, runner.binary, runner.args, req, &out); err != nil {
		return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_RUNTIME_PROBE_FAILED",
			fmt.Sprintf("audio role runtime probe failed: %v", err),
			map[string]any{"stage": "audio_role_probe", "command_id": cmd.ID})
	}
	if out.Error != "" {
		return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_RUNTIME_PROBE_FAILED",
			out.Error,
			map[string]any{"stage": "audio_role_probe", "command_id": cmd.ID})
	}
	if out.Status != "" && out.Status != "ok" {
		return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_RUNTIME_PROBE_FAILED",
			fmt.Sprintf("audio role runtime probe status is not ok (%q)", out.Status),
			map[string]any{"stage": "audio_role_probe", "command_id": cmd.ID})
	}
	if out.SourceRevision == "" {
		return worker.ArtifactRef{}, worker.NewError("AUDIO_ROLE_RUNTIME_PROBE_FAILED",
			"audio role runtime probe returned empty source revision",
			map[string]any{"stage": "audio_role_probe", "command_id": cmd.ID})
	}
	return writeOutputArtifact(cmd, out)
}

func resolveAudioRoleRunner(requireSnap bool) (commandRunner, error) {
	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "audio_role_yamnet.py"),
		filepath.Join("adapters", "audio_role_yamnet.py"),
		filepath.Join("..", "..", "cmd", "stageworker", "adapters", "audio_role_yamnet.py"),
		filepath.Join("..", "cmd", "stageworker", "adapters", "audio_role_yamnet.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "audio_role_yamnet.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "audio_role_yamnet.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "audio_role_yamnet.py"),
		)
	}

	var repoAdapterPath string
	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if fi, err := os.Stat(absP); err == nil && !fi.IsDir() {
				repoAdapterPath = absP
				break
			}
		}
	}

	if bin := os.Getenv("DOUYINIE_AUDIO_ROLE_BIN"); bin != "" {
		if requireSnap {
			return commandRunner{}, worker.NewError("AUDIO_ROLE_RUNNER_OVERRIDE_REJECTED",
				fmt.Sprintf("arbitrary runner binary DOUYINIE_AUDIO_ROLE_BIN=%q cannot self-attest RC provenance on snapshot-required route", bin),
				nil)
		}
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("AUDIO_ROLE_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_AUDIO_ROLE_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_AUDIO_ROLE_ADAPTER"); script != "" {
		if fi, err := os.Stat(script); err == nil && !fi.IsDir() {
			absScript, _ := filepath.Abs(script)
			isRepo := false
			if repoAdapterPath != "" {
				if strings.EqualFold(filepath.Clean(absScript), filepath.Clean(repoAdapterPath)) {
					isRepo = true
				} else if rfi, rerr := os.Stat(repoAdapterPath); rerr == nil && os.SameFile(fi, rfi) {
					isRepo = true
				}
			}

			if requireSnap && !isRepo {
				return commandRunner{}, worker.NewError("AUDIO_ROLE_RUNNER_OVERRIDE_REJECTED",
					fmt.Sprintf("arbitrary runner adapter DOUYINIE_AUDIO_ROLE_ADAPTER=%q cannot self-attest RC provenance on snapshot-required route (repo-owned adapter required)", script),
					nil)
			}

			pyBin := resolveAudioRolePythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{absScript}}, nil
			}
			return commandRunner{}, worker.NewError("AUDIO_ROLE_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_AUDIO_ROLE_ADAPTER", nil)
		}
		return commandRunner{}, worker.NewError("AUDIO_ROLE_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_AUDIO_ROLE_ADAPTER script %q not found", script), nil)
	}

	if repoAdapterPath != "" {
		pyBin := resolveAudioRolePythonBinary()
		if pyBin != "" {
			return commandRunner{binary: pyBin, args: []string{repoAdapterPath}}, nil
		}
	}

	return commandRunner{}, worker.NewError("AUDIO_ROLE_BINARY_NOT_FOUND",
		"YAMNet audio role adapter not available: configure DOUYINIE_AUDIO_ROLE_PYTHON_BIN / DOUYINIE_AUDIO_ROLE_ADAPTER",
		nil)
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
	// Repo-owned Python adapters: check RapidOCR ONNX first (lightweight), fallback to PaddleOCR
	adapterCandidates := []string{"ocr_rapidocr.py", "ocr.py"}
	var adapterPaths []string
	for _, name := range adapterCandidates {
		adapterPaths = append(adapterPaths,
			filepath.Join("cmd", "stageworker", "adapters", name),
			filepath.Join("adapters", name),
			filepath.Join("..", "..", "cmd", "stageworker", "adapters", name),
			filepath.Join("..", "cmd", "stageworker", "adapters", name),
		)
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			adapterPaths = append(adapterPaths,
				filepath.Join(exeDir, "adapters", name),
				filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", name),
				filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", name),
			)
		}
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
	snapEnv, _ := worker.GetModelSnapshotEnvelope(cmd.Config)
	var detDir, recDir, clsDir string
	if snapEnv != nil {
		if snapEnv.Primary.Role == "det" || strings.Contains(strings.ToLower(snapEnv.Primary.DependencyName), "det") {
			detDir = snapEnv.Primary.LocalPath
		}
		for _, dep := range snapEnv.Dependencies {
			role := strings.ToLower(dep.Role)
			name := strings.ToLower(dep.DependencyName)
			if role == "det" || strings.Contains(name, "det") {
				detDir = dep.LocalPath
			} else if role == "rec" || strings.Contains(name, "rec") {
				recDir = dep.LocalPath
			} else if role == "ori" || role == "cls" || strings.Contains(name, "ori") || strings.Contains(name, "cls") {
				clsDir = dep.LocalPath
			}
		}
	}

	reqSnap, _ := cmd.Config["require_model_snapshot"].(bool)
	if reqSnap {
		if detDir == "" || recDir == "" || clsDir == "" {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("ocr requires verified det, rec, and ori snapshots (got det=%q, rec=%q, cls=%q)", detDir, recDir, clsDir),
				map[string]any{"stage": "ocr", "command_id": cmd.ID})
		}
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
		"det_model_dir":        detDir,
		"rec_model_dir":        recDir,
		"cls_model_dir":        clsDir,
		"ori_model_dir":        clsDir,
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

func resolveTranslationRunner() (commandRunner, error) {
	if bin := os.Getenv("DOUYINIE_TRANSLATION_BIN"); bin != "" {
		if path, err := exec.LookPath(bin); err == nil {
			return commandRunner{binary: path}, nil
		}
		return commandRunner{}, worker.NewError("TRANSLATION_BINARY_NOT_FOUND",
			fmt.Sprintf("DOUYINIE_TRANSLATION_BIN %q not found", bin), nil)
	}

	if script := os.Getenv("DOUYINIE_TRANSLATION_ADAPTER"); script != "" {
		if _, err := os.Stat(script); err == nil {
			pyBin := resolveTranslationPythonBinary()
			if pyBin != "" {
				return commandRunner{binary: pyBin, args: []string{script}}, nil
			}
			return commandRunner{}, worker.NewError("TRANSLATION_BINARY_NOT_FOUND",
				"python runtime not found to execute DOUYINIE_TRANSLATION_ADAPTER", nil)
		}
	}

	adapterPaths := []string{
		filepath.Join("cmd", "stageworker", "adapters", "translation_qwen3.py"),
		filepath.Join("adapters", "translation_qwen3.py"),
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		adapterPaths = append(adapterPaths,
			filepath.Join(exeDir, "adapters", "translation_qwen3.py"),
			filepath.Join(exeDir, "..", "cmd", "stageworker", "adapters", "translation_qwen3.py"),
			filepath.Join(exeDir, "..", "..", "cmd", "stageworker", "adapters", "translation_qwen3.py"),
		)
	}

	for _, p := range adapterPaths {
		if absP, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(absP); err == nil {
				pyBin := resolveTranslationPythonBinary()
				if pyBin != "" {
					return commandRunner{binary: pyBin, args: []string{absP}}, nil
				}
			}
		}
	}

	for _, name := range []string{"qwen3-translator", "qwen-translator"} {
		if path, err := exec.LookPath(name); err == nil {
			return commandRunner{binary: path}, nil
		}
	}

	return commandRunner{}, worker.NewError("TRANSLATION_BINARY_NOT_FOUND",
		"Qwen3 translation adapter or binary not available: configure DOUYINIE_TRANSLATION_ADAPTER/DOUYINIE_TRANSLATION_BIN",
		nil)
}

func runTranslationAdapter(ctx context.Context, cmd worker.Command, enc *worker.Encoder) (worker.ArtifactRef, error) {
	rawSegments, ok := cmd.Config["segments"]
	if !ok || rawSegments == nil {
		return worker.ArtifactRef{}, worker.NewError("TRANSLATION_MISSING_INPUT",
			"translation command has no input segments in config",
			map[string]any{"stage": "translation", "command_id": cmd.ID})
	}

	targetLang, _ := cmd.Config["target_language"].(string)
	if strings.TrimSpace(targetLang) == "" {
		return worker.ArtifactRef{}, worker.NewError("TRANSLATION_MISSING_TARGET_LANGUAGE",
			"translation command has no target_language in config",
			map[string]any{"stage": "translation", "command_id": cmd.ID})
	}

	sourceLang, _ := cmd.Config["source_language"].(string)
	if strings.TrimSpace(sourceLang) == "" {
		sourceLang = "zh"
	}

	modelName, _ := cmd.Config[cfgModelName].(string)
	if modelName == "" {
		modelName = "qwen3_4b_translator"
	}
	modelVersion, _ := cmd.Config[cfgModelVersion].(string)
	if modelVersion == "" {
		modelVersion = "Qwen3-4B-Q4_K_M"
	}

	snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config)
	if parseErr != nil {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr), nil)
	}

	var modelPath string
	if snapEnv != nil {
		modelPath = strings.TrimSpace(snapEnv.Primary.EntrypointFile)
		if modelPath == "" {
			modelPath = strings.TrimSpace(snapEnv.EntrypointFile)
		}
		if modelPath == "" {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				"verified model_snapshot envelope missing entrypoint_file for translation", nil)
		}
		info, err := os.Stat(modelPath)
		if err != nil {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("verified model_snapshot entrypoint file inaccessible (%s): %v", modelPath, err), nil)
		}
		if info.IsDir() {
			return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("verified model_snapshot entrypoint path (%s) is a directory; exact GGUF file required", modelPath), nil)
		}
	} else if reqSnap, _ := cmd.Config["require_model_snapshot"].(bool); reqSnap {
		return worker.ArtifactRef{}, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			"required model snapshot envelope missing from translation command config", nil)
	}

	runner, err := resolveTranslationRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"segments":        rawSegments,
		"source_language": sourceLang,
		"target_language": targetLang,
		"run_id":          cmd.RunID,
		"attempt_id":      cmd.AttemptID,
		cfgModelName:      modelName,
		cfgModelVersion:   modelVersion,
		"model_path":      modelPath,
	}
	var out struct {
		Segments     []domain.TranslationSegment `json:"segments"`
		ModelName    string                      `json:"model_name"`
		ModelVersion string                      `json:"model_version"`
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

	if len(out.Segments) == 0 {
		return worker.ArtifactRef{}, worker.NewError("TRANSLATION_NO_SEGMENTS",
			"translation produced no segments",
			map[string]any{"stage": "translation", "command_id": cmd.ID})
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
		if _, err := os.Stat(py); err == nil {
			return py
		}
		// If configured as python.cmd or directory, try sibling python.exe
		if strings.HasSuffix(strings.ToLower(py), ".cmd") || strings.HasSuffix(strings.ToLower(py), ".bat") {
			exe := strings.TrimSuffix(py, filepath.Ext(py)) + ".exe"
			if path, err := exec.LookPath(exe); err == nil {
				return path
			}
			if _, err := os.Stat(exe); err == nil {
				return exe
			}
		}
	}
	return resolvePythonBinary()
}
func resolveSeparatorPythonBinary() string {
	if py := os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
		if _, err := os.Stat(py); err == nil {
			return py
		}
		if strings.HasSuffix(strings.ToLower(py), ".cmd") || strings.HasSuffix(strings.ToLower(py), ".bat") {
			exe := strings.TrimSuffix(py, filepath.Ext(py)) + ".exe"
			if path, err := exec.LookPath(exe); err == nil {
				return path
			}
			if _, err := os.Stat(exe); err == nil {
				return exe
			}
		}
	}
	return resolvePythonBinary()
}

func resolveAudioRolePythonBinary() string {
	if py := os.Getenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
		if _, err := os.Stat(py); err == nil {
			return py
		}
		if strings.HasSuffix(strings.ToLower(py), ".cmd") || strings.HasSuffix(strings.ToLower(py), ".bat") {
			exe := strings.TrimSuffix(py, filepath.Ext(py)) + ".exe"
			if path, err := exec.LookPath(exe); err == nil {
				return path
			}
			if _, err := os.Stat(exe); err == nil {
				return exe
			}
		}
	}
	return resolvePythonBinary()
}
func resolveTranslationPythonBinary() string {
	if py := os.Getenv("DOUYINIE_TRANSLATION_PYTHON_BIN"); py != "" {
		if path, err := exec.LookPath(py); err == nil {
			return path
		}
	}
	return resolvePythonBinary()
}

// resolveDiarizerPythonBinary resolves the interpreter for the diarizer family
// (Issue #106). DOUYINIE_DIARIZER_PYTHON_BIN — named after the per-family
// interpreter variables of the other stage families — is honoured ahead of the
// shared DOUYINIE_PYTHON_BIN, so the diarizer can run its own venv (the one
// carrying modelscope/torchaudio) while asr/aligner keep the shared one.
//
// Unlike the sibling resolvers this one fails closed: a configured value that
// is not a usable interpreter returns an error naming the variable and the
// path. Falling back to another interpreter would only reproduce the opaque
// DIARIZER_EXEC_FAILED this variable exists to prevent.
func resolveDiarizerPythonBinary() (string, error) {
	configured := strings.TrimSpace(os.Getenv("DOUYINIE_DIARIZER_PYTHON_BIN"))
	if configured == "" {
		return resolvePythonBinary(), nil
	}
	if path, err := exec.LookPath(configured); err == nil {
		return path, nil
	}
	return "", worker.NewError("DIARIZER_RUNTIME_MISSING",
		fmt.Sprintf("configured DOUYINIE_DIARIZER_PYTHON_BIN \"%s\" is not a usable interpreter (no fallback to another interpreter)", configured),
		map[string]any{"stage": "diarizer", "env": "DOUYINIE_DIARIZER_PYTHON_BIN", "path": configured})
}

// resolveDiarizerSnapshotPaths parses and validates the typed model_snapshot envelope
// for the diarizer stages. When require_model_snapshot=true, both primary CAMPPlus
// and dependency VAD local paths must exist and be valid directories.
func resolveDiarizerSnapshotPaths(cmd worker.Command, stage string) (modelPath string, vadModelPath string, reqSnap bool, err error) {
	reqSnap, _ = cmd.Config["require_model_snapshot"].(bool)
	snapEnv, parseErr := worker.GetModelSnapshotEnvelope(cmd.Config)
	if parseErr != nil {
		return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("invalid model_snapshot envelope: %v", parseErr),
			map[string]any{"stage": stage, "command_id": cmd.ID})
	}

	if snapEnv != nil {
		modelPath = snapEnv.Primary.LocalPath
		for _, dep := range snapEnv.Dependencies {
			role := strings.ToLower(dep.Role)
			name := strings.ToLower(dep.DependencyName)
			if role == "vad" || strings.Contains(name, "vad") {
				vadModelPath = dep.LocalPath
				break
			}
		}

		// Top-level unverified overrides cannot supersede envelope paths.
		if topMP, ok := cmd.Config["model_path"].(string); ok && topMP != "" {
			if modelPath == "" || filepath.Clean(topMP) != filepath.Clean(modelPath) {
				return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					fmt.Sprintf("top-level model_path (%s) attempts unverified override of model_snapshot envelope (%s)", topMP, modelPath),
					map[string]any{"stage": stage, "command_id": cmd.ID})
			}
		}
		if topVAD, ok := cmd.Config["vad_model_path"].(string); ok && topVAD != "" {
			if vadModelPath == "" || filepath.Clean(topVAD) != filepath.Clean(vadModelPath) {
				return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
					fmt.Sprintf("top-level vad_model_path (%s) attempts unverified override of model_snapshot envelope (%s)", topVAD, vadModelPath),
					map[string]any{"stage": stage, "command_id": cmd.ID})
			}
		}
	} else if reqSnap {
		return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
			fmt.Sprintf("required model snapshot envelope missing from %s command config", stage),
			map[string]any{"stage": stage, "command_id": cmd.ID})
	} else {
		if mp, ok := cmd.Config["model_path"].(string); ok && mp != "" {
			modelPath = mp
		}
		if vp, ok := cmd.Config["vad_model_path"].(string); ok && vp != "" {
			vadModelPath = vp
		}
	}

	if reqSnap {
		if modelPath == "" || vadModelPath == "" {
			return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("%s requires verified primary campplus and vad snapshots (got model_path=%q, vad_model_path=%q)", stage, modelPath, vadModelPath),
				map[string]any{"stage": stage, "command_id": cmd.ID})
		}
		if info, statErr := os.Stat(modelPath); statErr != nil || !info.IsDir() {
			return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("primary model snapshot path inaccessible or not a directory (%s)", modelPath),
				map[string]any{"stage": stage, "command_id": cmd.ID})
		}
		if info, statErr := os.Stat(vadModelPath); statErr != nil || !info.IsDir() {
			return "", "", reqSnap, worker.NewError("WORKER_SNAPSHOT_PATH_REQUIRED",
				fmt.Sprintf("vad model snapshot path inaccessible or not a directory (%s)", vadModelPath),
				map[string]any{"stage": stage, "command_id": cmd.ID})
		}
	}

	return modelPath, vadModelPath, reqSnap, nil
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

	modelPath, vadModelPath, reqSnap, err := resolveDiarizerSnapshotPaths(cmd, "diarize")
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	runner, err := resolveDiarizerRunner()
	if err != nil {
		return worker.ArtifactRef{}, err
	}

	req := map[string]any{
		"mode":                   "diarize",
		"audio_path":             cmd.Inputs[0].Path,
		"run_id":                 cmd.RunID,
		"attempt_id":             cmd.AttemptID,
		cfgModelName:             modelName,
		cfgModelVersion:          modelVersion,
		cfgVADModelName:          vadModelName,
		cfgVADModelVersion:       vadModelVersion,
		"model_path":             modelPath,
		"vad_model_path":         vadModelPath,
		"require_model_snapshot": reqSnap,
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
	modelPath, vadModelPath, reqSnap, err := resolveDiarizerSnapshotPaths(cmd, "diarize_evidence")
	if err != nil {
		return worker.ArtifactRef{}, err
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
		"model_path":                 modelPath,
		"vad_model_path":             vadModelPath,
		"require_model_snapshot":     reqSnap,
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
