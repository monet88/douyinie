package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// Client is RuntimeHost's protocol-side controller for one StageWorker
// subprocess. It owns the NDJSON reader, heartbeat tracking, lifecycle state,
// and cancel escalation. A background read loop prevents stale processes from
// blocking cancellation.
type Client struct {
	supervisor *Supervisor
	encoder    *Encoder

	mu         sync.Mutex
	state      string
	command    string
	lastAck    time.Time
	cancelled  bool
	cancelDone chan struct{}

	decoder     *Decoder
	readLoop    chan envelopeResult
	readStop    chan struct{}
	readStopped chan struct{}
	startOnce   sync.Once
}

type envelopeResult struct {
	env Envelope
	err error
}

// ErrCancelled is returned when Run observes a cooperative cancel request.
var ErrCancelled = errors.New("stageworker command cancelled")

// NewClient creates a protocol client over an already-spawned supervisor.
func NewClient(sup *Supervisor) *Client {
	c := &Client{
		supervisor:  sup,
		encoder:     NewEncoder(sup.StdinWriter()),
		decoder:     NewDecoder(sup.StdoutReader()),
		state:       LifecycleStarting,
		readLoop:    make(chan envelopeResult, 16),
		readStop:    make(chan struct{}),
		readStopped: make(chan struct{}),
	}
	c.startReadLoop()
	return c
}

// State returns the host-side lifecycle state.
func (c *Client) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) setState(state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

// Handshake reads the worker hello and rejects protocol version mismatches.
func (c *Client) Handshake(ctx context.Context, timeout time.Duration) (HelloPayload, error) {
	c.startReadLoop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return HelloPayload{}, ctx.Err()
		case <-timer.C:
			return HelloPayload{}, errors.New("worker handshake timed out")
		case res := <-c.readLoop:
			if res.err != nil {
				return HelloPayload{}, res.err
			}
			if res.env.Type == MessageTypeHeartbeat {
				c.supervisor.RecordHeartbeat()
				continue
			}
			if res.env.Type != MessageTypeHello {
				return HelloPayload{}, fmt.Errorf("expected hello, got %s", res.env.Type)
			}
			if err := ValidateEnvelope(res.env); err != nil {
				return HelloPayload{}, err
			}
			var hello HelloPayload
			if err := decodePayload(res.env.Payload, &hello); err != nil {
				return HelloPayload{}, err
			}
			if hello.Schema != ProtocolVersion {
				return HelloPayload{}, fmt.Errorf("%w: worker schema %d, want %d", VersionError, hello.Schema, ProtocolVersion)
			}
			c.setState(LifecycleIdle)
			return hello, nil
		}
	}
}

// Start sends a command and waits for ready.
func (c *Client) Start(ctx context.Context, cmd Command, timeout time.Duration) error {
	if err := c.encoder.Encode(MessageTypeCommand, cmd); err != nil {
		return err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("worker ready timeout")
		case res := <-c.readLoop:
			if res.err != nil {
				return res.err
			}
			if err := ValidateEnvelope(res.env); err != nil {
				return err
			}
			switch res.env.Type {
			case MessageTypeHeartbeat:
				c.supervisor.RecordHeartbeat()
			case MessageTypeReady:
				var p ReadyPayload
				_ = decodePayload(res.env.Payload, &p)
				if p.CommandID != cmd.ID {
					return fmt.Errorf("ready command mismatch: got %s, want %s", p.CommandID, cmd.ID)
				}
				c.mu.Lock()
				c.command = cmd.ID
				c.state = LifecycleRunning
				c.cancelled = false
				c.cancelDone = make(chan struct{})
				c.mu.Unlock()
				return nil
			case MessageTypeError:
				var p ErrorPayload
				_ = decodePayload(res.env.Payload, &p)
				c.setState(LifecycleFailed)
				return p.Error
			default:
				return fmt.Errorf("unexpected %s while waiting for ready", res.env.Type)
			}
		}
	}
}

// Run executes a command to completion, routing heartbeats, progress, errors,
// and completion. It returns the committed artifact reference.
func (c *Client) Run(ctx context.Context, cmd Command, readyTimeout, heartbeatTimeout time.Duration) (ArtifactRef, error) {
	if err := c.Start(ctx, cmd, readyTimeout); err != nil {
		return ArtifactRef{}, err
	}
	c.supervisor.RecordHeartbeat()

	for {
		select {
		case <-ctx.Done():
			return ArtifactRef{}, ctx.Err()
		case <-time.After(heartbeatTimeout):
			if time.Since(c.supervisor.LastHeartbeat()) > heartbeatTimeout {
				return ArtifactRef{}, fmt.Errorf("worker heartbeat timeout after %s", heartbeatTimeout)
			}
		case res := <-c.readLoop:
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					c.mu.Lock()
					state := c.state
					c.mu.Unlock()
					if state == LifecycleCancelling || state == LifecycleTerminating || state == LifecycleInterrupted {
						return ArtifactRef{}, ErrInterrupted
					}
					return ArtifactRef{}, fmt.Errorf("worker exited before completion: %w", res.err)
				}
				return ArtifactRef{}, res.err
			}
			if err := ValidateEnvelope(res.env); err != nil {
				return ArtifactRef{}, err
			}
			switch res.env.Type {
			case MessageTypeHeartbeat:
				c.supervisor.RecordHeartbeat()
			case MessageTypeProgress:
				var p ProgressPayload
				_ = decodePayload(res.env.Payload, &p)
				log.Printf("[StageWorker] progress command=%s phase=%s percent=%.1f", p.CommandID, p.Phase, p.Percent)
			case MessageTypeComplete:
				var p CompletePayload
				_ = decodePayload(res.env.Payload, &p)
				if p.CommandID != cmd.ID {
					return ArtifactRef{}, fmt.Errorf("complete command mismatch: got %s, want %s", p.CommandID, cmd.ID)
				}
				c.setState(LifecycleSucceeded)
				return p.Artifact, nil
			case MessageTypeCancelAck:
				var p CancelAckPayload
				_ = decodePayload(res.env.Payload, &p)
				c.supervisor.RecordHeartbeat()
			case MessageTypeCancelComplete:
				var p CancelCompletePayload
				_ = decodePayload(res.env.Payload, &p)
				c.mu.Lock()
				c.state = LifecycleCancelled
				if c.cancelDone != nil {
					select {
					case <-c.cancelDone:
					default:
						close(c.cancelDone)
					}
				}
				c.mu.Unlock()
				return ArtifactRef{}, ErrCancelled
			case MessageTypeError:
				var p ErrorPayload
				_ = decodePayload(res.env.Payload, &p)
				c.setState(LifecycleFailed)
				return ArtifactRef{}, p.Error
			case MessageTypeInterrupted:
				var p InterruptedPayload
				_ = decodePayload(res.env.Payload, &p)
				c.setState(LifecycleInterrupted)
				return ArtifactRef{}, fmt.Errorf("%w: %s", ErrInterrupted, p.Reason)
			default:
				return ArtifactRef{}, fmt.Errorf("unexpected message type %s", res.env.Type)
			}
		}
	}
}

// Cancel sends a cooperative cancel request, waits for cooperative completion
// or ack, then escalates to forced termination after the grace period.
func (c *Client) Cancel(ctx context.Context, commandID string, graceMS int64) error {
	if graceMS <= 0 {
		graceMS = DefaultGracePeriod
	}
	c.mu.Lock()
	c.state = LifecycleCancelling
	c.cancelled = true
	cancelDone := c.cancelDone
	c.mu.Unlock()

	if err := c.encoder.Encode(MessageTypeCancel, CancelPayload{CommandID: commandID, GraceMS: graceMS}); err != nil {
		_ = c.supervisor.Terminate()
		c.setState(LifecycleInterrupted)
		return err
	}

	grace := time.Duration(graceMS) * time.Millisecond
	timer := time.NewTimer(grace)
	defer timer.Stop()

	for {
		if cancelDone != nil {
			select {
			case <-cancelDone:
				// Observed successful cooperative completion before forced escalation deadline
				return nil
			case <-ctx.Done():
				c.setState(LifecycleTerminating)
				_ = c.supervisor.Terminate()
				c.setState(LifecycleInterrupted)
				return ctx.Err()
			case <-timer.C:
				// Grace period expired before cooperative completion -> escalate to forced termination
				c.setState(LifecycleTerminating)
				if err := c.supervisor.Terminate(); err != nil {
					c.setState(LifecycleInterrupted)
					return err
				}
				c.setState(LifecycleInterrupted)
				return nil
			}
		} else {
			select {
			case <-ctx.Done():
				c.setState(LifecycleTerminating)
				_ = c.supervisor.Terminate()
				c.setState(LifecycleInterrupted)
				return ctx.Err()
			case res := <-c.readLoop:
				if res.err != nil {
					c.setState(LifecycleExited)
					return nil
				}
				if res.env.Type == MessageTypeCancelComplete {
					c.setState(LifecycleCancelled)
					return nil
				}
			case <-timer.C:
				c.setState(LifecycleTerminating)
				if err := c.supervisor.Terminate(); err != nil {
					c.setState(LifecycleInterrupted)
					return err
				}
				c.setState(LifecycleInterrupted)
				return nil
			}
		}
	}
}

// Shutdown terminates the worker and reaps it.
func (c *Client) Shutdown() error {
	c.setState(LifecycleTerminating)
	if err := c.supervisor.Terminate(); err != nil {
		return err
	}
	c.stopReadLoop()
	return c.supervisor.Wait()
}

func (c *Client) startReadLoop() {
	c.startOnce.Do(func() {
		go c.readEnvelopes()
	})
}

func (c *Client) readEnvelopes() {
	defer close(c.readStopped)
	for {
		select {
		case <-c.readStop:
			return
		default:
		}
		env, err := c.decoder.Decode()
		if err != nil {
			select {
			case c.readLoop <- envelopeResult{err: err}:
			case <-c.readStop:
			}
			return
		}
		select {
		case c.readLoop <- envelopeResult{env: env}:
		case <-c.readStop:
			return
		}
	}
}

func (c *Client) stopReadLoop() {
	select {
	case <-c.readStop:
	default:
		close(c.readStop)
	}
	<-c.readStopped
}

func (c *Client) isRunCancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled
}
