// Package worker implements the Phase 1 StageWorker runtime contract (Seam 2):
// a versioned NDJSON protocol over stdin/stdout, structured error envelopes,
// heartbeats, cancellation escalation, and worker lifecycle states.
package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// ProtocolVersion is the current NDJSON envelope schema version. Both
	// RuntimeHost and StageWorker reject mismatched versions on handshake.
	ProtocolVersion = 1

	// MessageTypeHello is sent by the worker immediately after startup.
	MessageTypeHello = "hello"
	// MessageTypeReady reports that the worker has accepted a command and is
	// ready to start executing it.
	MessageTypeReady = "ready"
	// MessageTypeCommand carries a stage command from RuntimeHost to the worker.
	MessageTypeCommand = "command"
	// MessageTypeHeartbeat keeps the RuntimeHost informed that the worker is
	// still alive and making progress.
	MessageTypeHeartbeat = "heartbeat"
	// MessageTypeProgress reports coarse execution progress from the worker.
	MessageTypeProgress = "progress"
	// MessageTypeComplete reports a successful stage attempt.
	MessageTypeComplete = "complete"
	// MessageTypeError reports a structured failure envelope.
	MessageTypeError = "error"
	// MessageTypeCancel is the RuntimeHost -> worker cooperative cancel request.
	MessageTypeCancel = "cancel"
	// MessageTypeCancelAck confirms that the worker received the cancel request.
	MessageTypeCancelAck = "cancel_ack"
	// MessageTypeCancelComplete reports that cooperative cancellation finished
	// before forced termination was required.
	MessageTypeCancelComplete = "cancel_complete"
	// MessageTypeInterrupted reports that a command was interrupted, either by
	// cancellation escalation or by RuntimeHost crash recovery.
	MessageTypeInterrupted = "interrupted"

	// LifecycleStarting is the state before the worker sends hello.
	LifecycleStarting = "starting"
	// LifecycleIdle is the state after hello and between commands.
	LifecycleIdle = "idle"
	// LifecycleRunning is the state while a command is executing.
	LifecycleRunning = "running"
	// LifecycleCancelling is the cooperative cancellation grace state.
	LifecycleCancelling = "cancelling"
	// LifecycleTerminating is the state during forced termination cleanup.
	LifecycleTerminating = "terminating"
	// LifecycleSucceeded is the terminal success state.
	LifecycleSucceeded = "succeeded"
	// LifecycleFailed is the terminal failure state.
	LifecycleFailed = "failed"
	// LifecycleCancelled is the terminal cooperative cancellation state.
	LifecycleCancelled = "cancelled"
	// LifecycleInterrupted is the terminal interrupted state.
	LifecycleInterrupted = "interrupted"
	// LifecycleExited is the terminal OS process state observed by RuntimeHost.
	LifecycleExited = "exited"
)

// LifecycleActive returns true for states that are not terminal.
func LifecycleActive(state string) bool {
	switch state {
	case LifecycleStarting, LifecycleIdle, LifecycleRunning, LifecycleCancelling, LifecycleTerminating:
		return true
	default:
		return false
	}
}

// Command describes a stage attempt to execute in a worker family.
type Command struct {
	ID         string         `json:"id"`
	Family     string         `json:"family"`
	Stage      string         `json:"stage"`
	AttemptID  string         `json:"attempt_id"`
	RunID      string         `json:"run_id"`
	Inputs     []ArtifactRef  `json:"inputs"`
	Config     map[string]any `json:"config"`
	OutputPath string         `json:"output_path"`

	// CPUOnly declares that this command's stage runs on CPU only, so the host
	// must not acquire the authoritative single-GPU lease before spawning. The
	// zero value keeps the GPU-leased behavior every accelerator-backed family
	// relies on; only a provider whose resource contract is CPU-only sets it
	// (Issue #91).
	CPUOnly bool `json:"cpu_only,omitempty"`
}

// ArtifactRef is a metadata-only reference to a content-addressed artifact.
// Large media payloads never cross the protocol; workers read/write the file
// at the path provided by RuntimeHost.
type ArtifactRef struct {
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

// ErrorEnvelope is the structured error contract shared by RuntimeHost and
// StageWorker. Code is stable machine-readable identifier, message is a
// human-readable summary, and details carries optional structured context.
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// NewError constructs a structured error envelope.
func NewError(code, message string, details any) ErrorEnvelope {
	return ErrorEnvelope{Code: code, Message: message, Details: details}
}

// Error returns the envelope as an error carrying the stable code.
func (e ErrorEnvelope) Error() string {
	return e.Code + ": " + e.Message
}

// Envelope is the transport envelope for every NDJSON line. Type identifies
// the message kind; Version carries the protocol version on handshake and
// cancel messages; Payload is decoded by the caller into the typed struct.
type Envelope struct {
	Type    string          `json:"type"`
	Version int             `json:"version,omitempty"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// HelloPayload is sent by the worker after startup.
type HelloPayload struct {
	WorkerID string `json:"worker_id"`
	Family   string `json:"family"`
	Schema   int    `json:"schema_version"`
}

// ReadyPayload reports command acceptance.
type ReadyPayload struct {
	CommandID string `json:"command_id"`
}

// HeartbeatPayload reports liveness and coarse progress.
type HeartbeatPayload struct {
	CommandID string `json:"command_id,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// ProgressPayload reports coarse execution progress.
type ProgressPayload struct {
	CommandID string  `json:"command_id"`
	Percent   float64 `json:"percent"`
	Phase     string  `json:"phase"`
}

// CompletePayload reports successful command completion and the committed
// artifact reference. Artifact paths must be metadata-only references; RuntimeHost
// verifies the artifact survives worker termination independently.
type CompletePayload struct {
	CommandID string      `json:"command_id"`
	Artifact  ArtifactRef `json:"artifact"`
}

// CancelPayload is the RuntimeHost -> worker cooperative cancellation request.
// It carries the protocol version and grace period so escalation remains
// deterministic even if the worker started under an older schema.
type CancelPayload struct {
	CommandID string `json:"command_id"`
	GraceMS   int64  `json:"grace_ms"`
}

// CancelAckPayload confirms cooperative cancellation receipt.
type CancelAckPayload struct {
	CommandID string `json:"command_id"`
}

// CancelCompletePayload reports cooperative cancellation completion.
type CancelCompletePayload struct {
	CommandID string `json:"command_id"`
}

// InterruptedPayload reports a command that was interrupted before completion.
type InterruptedPayload struct {
	CommandID string `json:"command_id"`
	Reason    string `json:"reason"`
}

// ErrorPayload is the structured error envelope payload.
type ErrorPayload struct {
	Error ErrorEnvelope `json:"error"`
}

// Decoder reads NDJSON envelopes from an io.Reader.
type Decoder struct {
	scanner *bufio.Scanner
}

// NewDecoder creates a Decoder. The scanner buffer is sized for metadata-only
// envelopes; large payloads are never valid on this seam.
func NewDecoder(r io.Reader) *Decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &Decoder{scanner: scanner}
}

// Decode reads and validates the next envelope.
func (d *Decoder) Decode() (Envelope, error) {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			return Envelope{}, fmt.Errorf("read ndjson: %w", err)
		}
		return Envelope{}, io.EOF
	}
	line := strings.TrimSpace(d.scanner.Text())
	if line == "" {
		return Envelope{}, errors.New("empty ndjson line")
	}
	var env Envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return Envelope{}, fmt.Errorf("decode ndjson envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, errors.New("envelope type is required")
	}
	return env, nil
}

// Encoder writes NDJSON envelopes to an io.Writer.
type Encoder struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

// NewEncoder creates an Encoder.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode serializes a typed message into one NDJSON line.
func (e *Encoder) Encode(msgType string, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", msgType, err)
	}
	env := Envelope{
		Type:    msgType,
		Version: ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: raw,
	}
	line, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal %s envelope: %w", msgType, err)
	}
	e.buf = append(e.buf[:0], line...)
	e.buf = append(e.buf, '\n')
	if _, err := e.w.Write(e.buf); err != nil {
		return fmt.Errorf("write %s envelope: %w", msgType, err)
	}
	return nil
}

// VersionError is returned when a handshake version does not match
// ProtocolVersion.
var VersionError = errors.New("stageworker protocol version mismatch")

// ErrInterrupted is returned when a command execution was interrupted by
// forced cancellation, timeout, signal, or crash recovery.
var ErrInterrupted = errors.New("stageworker command interrupted")
