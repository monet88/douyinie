package worker

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateEnvelope performs schema-level validation for every message type in
// the Seam 2 NDJSON contract. It decodes the payload with strict JSON field
// semantics and enforces required fields without accepting arbitrary messages.
func ValidateEnvelope(env Envelope) error {
	if env.Type == "" {
		return fmt.Errorf("message type is required")
	}
	if env.Version != 0 && env.Version != ProtocolVersion {
		return fmt.Errorf("%w: got %d, want %d", VersionError, env.Version, ProtocolVersion)
	}
	if env.At.IsZero() {
		return fmt.Errorf("envelope timestamp is required")
	}
	if len(env.Payload) == 0 {
		if env.Type == MessageTypeHello || env.Type == MessageTypeReady || env.Type == MessageTypeHeartbeat ||
			env.Type == MessageTypeProgress || env.Type == MessageTypeComplete || env.Type == MessageTypeCancel ||
			env.Type == MessageTypeCancelAck || env.Type == MessageTypeCancelComplete || env.Type == MessageTypeInterrupted ||
			env.Type == MessageTypeError {
			return fmt.Errorf("%s payload is required", env.Type)
		}
		return nil
	}

	var err error
	switch env.Type {
	case MessageTypeCommand:
		var p Command
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("id", p.ID)
		}
		if err == nil {
			err = requireNonEmpty("family", p.Family)
		}
		if err == nil {
			err = requireNonEmpty("stage", p.Stage)
		}
		if err == nil {
			err = requireNonEmpty("attempt_id", p.AttemptID)
		}
	case MessageTypeHello:
		var p HelloPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("worker_id", p.WorkerID)
		}
		if err == nil {
			err = requireNonEmpty("family", p.Family)
		}
		if err == nil && p.Schema != ProtocolVersion {
			err = fmt.Errorf("%w: hello schema_version %d, want %d", VersionError, p.Schema, ProtocolVersion)
		}
	case MessageTypeReady:
		var p ReadyPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
	case MessageTypeHeartbeat:
		var p HeartbeatPayload
		err = decodePayload(env.Payload, &p)
	case MessageTypeProgress:
		var p ProgressPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
		if err == nil && (p.Percent < 0 || p.Percent > 100) {
			err = fmt.Errorf("percent must be within [0,100], got %v", p.Percent)
		}
	case MessageTypeComplete:
		var p CompletePayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
		if err == nil {
			err = requireNonEmpty("artifact.sha256", p.Artifact.SHA256)
		}
	case MessageTypeError:
		var p ErrorPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("error.code", p.Error.Code)
		}
		if err == nil {
			err = requireNonEmpty("error.message", p.Error.Message)
		}
	case MessageTypeCancel:
		var p CancelPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
		if err == nil && p.GraceMS <= 0 {
			err = fmt.Errorf("grace_ms must be positive")
		}
	case MessageTypeCancelAck:
		var p CancelAckPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
	case MessageTypeCancelComplete:
		var p CancelCompletePayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
	case MessageTypeInterrupted:
		var p InterruptedPayload
		err = decodePayload(env.Payload, &p)
		if err == nil {
			err = requireNonEmpty("command_id", p.CommandID)
		}
	default:
		return fmt.Errorf("unsupported message type %q", env.Type)
	}
	if err != nil {
		return fmt.Errorf("invalid %s envelope: %w", env.Type, err)
	}
	return nil
}

func decodePayload(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing payload data")
	}
	return nil
}

func requireNonEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}
