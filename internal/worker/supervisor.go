package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

const (
	// DefaultGracePeriod is the MS window for cooperative cancellation before
	// escalation to forced termination.
	DefaultGracePeriod int64 = 5000

	// HeartbeatTimeout is the default RuntimeHost liveness deadline.
	HeartbeatTimeout = 30 * time.Second

	// HeartbeatInterval is the default StageWorker heartbeat interval.
	HeartbeatInterval = 5 * time.Second

	// TerminationTimeout bounds forced termination cleanup.
	TerminationTimeout = 5 * time.Second
)

// ProcessState is the observed state of a managed worker process.
type ProcessState struct {
	PID      int
	Family   string
	State    string
	Started  time.Time
	ExitedAt *time.Time
	ExitCode int
}

// Supervisor manages a worker subprocess lifecycle. On Windows the process is
// attached to a Job Object configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// immediately after start, so forced termination kills every descendant.
type Supervisor struct {
	mu sync.Mutex

	cmd    *exec.Cmd
	pid    int
	family string

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	job    *jobObjectHandle

	state    string
	exitedAt *time.Time
	exitCode int

	lastHeartbeat time.Time
	hbMutex       sync.Mutex
}

// NewSupervisor creates an idle supervisor.
func NewSupervisor() *Supervisor {
	return &Supervisor{state: LifecycleIdle}
}

// State returns the current lifecycle state.
func (s *Supervisor) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// ProcessInfo returns observed process information.
func (s *Supervisor) ProcessInfo() ProcessState {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := ProcessState{
		PID:    s.pid,
		Family: s.family,
		State:  s.state,
	}
	if s.exitedAt != nil {
		ps.ExitedAt = s.exitedAt
		ps.ExitCode = s.exitCode
	}
	return ps
}

// StdinWriter returns the worker stdin pipe.
func (s *Supervisor) StdinWriter() io.WriteCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin
}

// StdoutReader returns the worker stdout pipe.
func (s *Supervisor) StdoutReader() io.ReadCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdout
}

// StderrReader returns the worker stderr pipe.
func (s *Supervisor) StderrReader() io.ReadCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr
}

// LastHeartbeat returns the time of the last received heartbeat.
func (s *Supervisor) LastHeartbeat() time.Time {
	s.hbMutex.Lock()
	defer s.hbMutex.Unlock()
	return s.lastHeartbeat
}

// RecordHeartbeat records a heartbeat timestamp.
func (s *Supervisor) RecordHeartbeat() {
	s.hbMutex.Lock()
	defer s.hbMutex.Unlock()
	s.lastHeartbeat = time.Now()
}

// HeartbeatTimedOut reports whether the configured heartbeat deadline expired.
func (s *Supervisor) HeartbeatTimedOut(timeout time.Duration) bool {
	return time.Since(s.LastHeartbeat()) > timeout
}

// Spawn starts a worker subprocess with NDJSON stdin/stdout/stderr pipes.
func (s *Supervisor) Spawn(ctx context.Context, family, binary string, args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = jobObjectSysProcAttr()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create worker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create worker stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("create worker stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start worker %s: %w", family, err)
	}

	s.cmd = cmd
	s.pid = cmd.Process.Pid
	s.family = family
	s.stdin = stdin
	s.stdout = stdout
	s.stderr = stderr
	s.state = LifecycleStarting
	s.exitedAt = nil
	s.exitCode = -1

	job, err := attachJobObject(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		s.state = LifecycleFailed
		return fmt.Errorf("attach worker %s to job object: %w", family, err)
	}
	s.job = job

	s.RecordHeartbeat()
	log.Printf("[Supervisor] spawned worker family=%s pid=%d", family, s.pid)
	return nil
}

// Wait blocks until the worker exits, reaps it, and records exit state. Call
// is idempotent and returns nil if the process has already exited.
func (s *Supervisor) Wait() error {
	s.mu.Lock()
	if s.state == LifecycleExited {
		s.mu.Unlock()
		return nil
	}
	cmd := s.cmd
	s.mu.Unlock()

	if cmd == nil {
		return errors.New("no worker process to wait for")
	}

	err := cmd.Wait()
	now := time.Now()
	exCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exCode = exitErr.ExitCode()
		}
	}

	s.mu.Lock()
	s.exitedAt = &now
	s.exitCode = exCode
	s.state = LifecycleExited
	job := s.job
	s.job = nil
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.stdout != nil {
		_ = s.stdout.Close()
		s.stdout = nil
	}
	if s.stderr != nil {
		_ = s.stderr.Close()
		s.stderr = nil
	}
	s.mu.Unlock()

	if job != nil {
		job.close()
	}
	return err
}

// Terminate forcibly kills the worker and its process tree.
func (s *Supervisor) Terminate() error {
	s.mu.Lock()
	cmd := s.cmd
	pid := s.pid
	state := s.state
	job := s.job
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil || state == LifecycleExited {
		return nil
	}
	if job != nil {
		job.terminate()
		return nil
	}
	return terminateProcessTree(cmd, pid)
}

// Cancel marks the lifecycle state as cancelling.
func (s *Supervisor) Cancel() {
	s.setState(LifecycleCancelling)
}

func (s *Supervisor) setState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}
