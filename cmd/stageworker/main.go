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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

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
	if _, err := exec.LookPath("qwen3-asr"); err != nil {
		return worker.ArtifactRef{}, worker.NewError("ASR_BINARY_NOT_FOUND",
			"qwen3-asr binary not available on PATH: install Qwen3-ASR 1.7B/0.6B to enable ASR stage",
			map[string]any{"stage": "asr", "command_id": cmd.ID})
	}

	// stdin JSON request: audio_path is the machine-local input artifact path.
	req := map[string]any{
		"audio_path": cmd.Inputs[0].Path,
		"run_id":     cmd.RunID,
		"attempt_id": cmd.AttemptID,
	}
	var out struct {
		Segments []domain.ASRRawSegment `json:"segments"`
	}
	if err := invokeModel(ctx, "qwen3-asr", req, &out); err != nil {
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
	if _, err := exec.LookPath("qwen3-aligner"); err != nil {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_BINARY_NOT_FOUND",
			"qwen3-aligner binary not available on PATH: install Qwen3-ForcedAligner to enable alignment stage",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}
	text, _ := cmd.Config["text"].(string)
	if strings.TrimSpace(text) == "" {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_MISSING_TEXT",
			"aligner command has no accepted text in config",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}

	// stdin JSON request: audio_path + the accepted text to align.
	req := map[string]any{
		"audio_path": cmd.Inputs[0].Path,
		"text":       text,
		"run_id":     cmd.RunID,
		"attempt_id": cmd.AttemptID,
	}
	var out struct {
		WordTimings []domain.WordTiming `json:"word_timings"`
	}
	if err := invokeModel(ctx, "qwen3-aligner", req, &out); err != nil {
		return worker.ArtifactRef{}, err
	}
	if len(out.WordTimings) == 0 {
		return worker.ArtifactRef{}, worker.NewError("ALIGNER_NO_TIMINGS",
			"Qwen3-ForcedAligner produced no word timings",
			map[string]any{"stage": "aligner", "command_id": cmd.ID})
	}
	return writeOutputArtifact(cmd, out)
}

// invokeModel executes the model binary with a stdin JSON request and decodes
// the stdout JSON response into out. Non-zero exit, decode failure, or a
// structured error response all fail closed.
func invokeModel(ctx context.Context, binary string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal model request: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := "EXEC_FAILED"
		if strings.HasPrefix(binary, "qwen3-asr") {
			code = "ASR_EXEC_FAILED"
		} else {
			code = "ALIGNER_EXEC_FAILED"
		}
		return worker.NewError(code, fmt.Sprintf("%s exited with error: %v; stderr: %s", binary, err, strings.TrimSpace(stderr.String())), nil)
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		code := "OUTPUT_INVALID"
		if strings.HasPrefix(binary, "qwen3-asr") {
			code = "ASR_OUTPUT_INVALID"
		} else {
			code = "ALIGNER_OUTPUT_INVALID"
		}
		return worker.NewError(code, fmt.Sprintf("%s returned invalid JSON: %v", binary, err), nil)
	}
	return nil
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
// compatibility; real ASR/aligner stages never produce placeholder artifacts.
func writeMarkerArtifact(cmd worker.Command) (worker.ArtifactRef, error) {
	if cmd.OutputPath != "" {
		marker := map[string]any{
			"command_id": cmd.ID,
			"stage":      cmd.Stage,
			"family":     cmd.Family,
			"status":     "executed",
		}
		b, _ := json.MarshalIndent(marker, "", "  ")
		if err := os.WriteFile(cmd.OutputPath, b, 0644); err != nil {
			return worker.ArtifactRef{}, fmt.Errorf("write output artifact: %w", err)
		}
	}
	return worker.ArtifactRef{
		SHA256: cmd.ID + "-sha256-placeholder",
		Path:   cmd.OutputPath,
	}, nil
}
