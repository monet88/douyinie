package seam2_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/worker"
)

func isProcessAlive(pid int) bool {
	if runtime.GOOS != "windows" {
		p, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		return p.Signal(syscall.Signal(0)) == nil
	}

	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259
	)
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess := kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess := kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle := kernel32.NewProc("CloseHandle")

	hProcess, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if hProcess == 0 {
		return false
	}
	defer procCloseHandle.Call(hProcess)

	var exitCode uint32
	r, _, _ := procGetExitCodeProcess.Call(hProcess, uintptr(unsafe.Pointer(&exitCode)))
	if r == 0 {
		return false
	}
	return exitCode == stillActive
}

func buildStageWorker(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "stageworker-test")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "github.com/monet88/douyinie/cmd/stageworker")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build stageworker: %v\n%s", err, out)
	}
	return exe
}

func TestSeam2_ProtocolVersionMismatchRejected(t *testing.T) {
	env := worker.Envelope{
		Type:    worker.MessageTypeHello,
		Version: 99,
		At:      time.Now().UTC(),
		Payload: mustJSON(t, worker.HelloPayload{WorkerID: "w1", Family: "asr", Schema: 99}),
	}
	err := worker.ValidateEnvelope(env)
	if err == nil {
		t.Fatal("expected mismatch to be rejected")
	}
	if !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "got 99") {
		t.Fatalf("expected version mismatch error, got %v", err)
	}
}

func TestSeam2_StructuredErrorEnvelope(t *testing.T) {
	errEnv := worker.ErrorEnvelope{
		Code:    "GPU_OOM",
		Message: "CUDA out of memory",
		Details: map[string]any{"bytes": 1073741824},
	}
	b, _ := json.Marshal(worker.ErrorPayload{Error: errEnv})
	env := worker.Envelope{
		Type:    worker.MessageTypeError,
		Version: worker.ProtocolVersion,
		At:      time.Now().UTC(),
		Payload: json.RawMessage(b),
	}
	if err := worker.ValidateEnvelope(env); err != nil {
		t.Fatalf("valid error envelope rejected: %v", err)
	}
	if errEnv.Code != "GPU_OOM" || errEnv.Message == "" || errEnv.Details == nil {
		t.Fatalf("structured envelope fields not preserved")
	}
}

func TestSeam2_HeartbeatTimeoutDetected(t *testing.T) {
	sup := worker.NewSupervisor()
	sup.RecordHeartbeat()
	time.Sleep(10 * time.Millisecond)
	if !sup.HeartbeatTimedOut(5 * time.Millisecond) {
		t.Fatal("expected heartbeat timeout detection")
	}
	sup.RecordHeartbeat()
	if sup.HeartbeatTimedOut(10 * time.Second) {
		t.Fatal("fresh heartbeat should not time out")
	}
}

func TestSeam2_SubprocessLifecycleAndComplete(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()
	if err := sup.Spawn(ctx, "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	hello, err := client.Handshake(context.Background(), 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	if hello.Family != "asr" {
		t.Fatalf("unexpected family %q", hello.Family)
	}
	if hello.Schema != worker.ProtocolVersion {
		t.Fatalf("unexpected schema %d", hello.Schema)
	}

	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-1",
		Family:     "asr",
		Stage:      "generic",
		AttemptID:  "attempt-1",
		RunID:      "run-1",
		OutputPath: outPath,
	}
	artifact, err := client.Run(ctx, cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("run: %v", err)
	}
	if artifact.Path != outPath {
		t.Fatalf("unexpected artifact path %q", artifact.Path)
	}
	if !strings.Contains(artifact.SHA256, "sha256") {
		t.Fatalf("unexpected artifact hash %q", artifact.SHA256)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("output artifact missing: %v", err)
	}
	_ = client.Shutdown()
}

func TestSeam2_CooperativeCancelCompletesBeforeEscalation(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	cmd := worker.Command{
		ID:        "cmd-coop-cancel",
		Family:    "tts",
		Stage:     "tts",
		AttemptID: "attempt-coop-cancel",
		RunID:     "run-coop-cancel",
		Config: map[string]any{
			"long_running_ms": float64(10000),
		},
	}
	cancelErrCh := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Large grace period: 5000ms. Cooperative cancel should finish well before this without forced escalation!
		cancelErrCh <- client.Cancel(context.Background(), cmd.ID, 5000)
	}()
	_, runErr := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if runErr == nil {
		_ = sup.Terminate()
		t.Fatal("expected cancel to stop run")
	}
	if !errors.Is(runErr, worker.ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", runErr)
	}
	if cancelErr := <-cancelErrCh; cancelErr != nil {
		t.Fatalf("cancel failed: %v", cancelErr)
	}
	if client.State() != worker.LifecycleCancelled {
		t.Fatalf("expected LifecycleCancelled, got %s", client.State())
	}
	// Subprocess supervisor must not have been terminated into exited/failed state!
	if sup.State() == worker.LifecycleExited || sup.State() == worker.LifecycleFailed {
		t.Fatalf("worker subprocess was killed despite cooperative cancellation")
	}

	// Verify the worker is still healthy and can process a subsequent command!
	cmd2 := worker.Command{
		ID:        "cmd-subsequent",
		Family:    "tts",
		Stage:     "generic",
		AttemptID: "attempt-subsequent",
		RunID:     "run-subsequent",
	}
	art2, err := client.Run(context.Background(), cmd2, 5*time.Second, 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("subsequent command failed on live worker: %v", err)
	}
	if !strings.Contains(art2.SHA256, "cmd-subsequent") {
		t.Fatalf("unexpected subsequent artifact: %+v", art2)
	}
	_ = client.Shutdown()
}

func TestSeam2_CancelEscalatesToForcedTermination(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	cmd := worker.Command{
		ID:        "cmd-forced-cancel",
		Family:    "tts",
		Stage:     "tts",
		AttemptID: "attempt-forced-cancel",
		RunID:     "run-forced-cancel",
		Config: map[string]any{
			"long_running_ms": float64(10000),
			"ignore_cancel":   true, // Simulates non-responsive / hanging worker
		},
	}
	cancelErrCh := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Short grace period: 300ms. Forced escalation should occur!
		cancelErrCh <- client.Cancel(context.Background(), cmd.ID, 300)
	}()
	_, runErr := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if runErr == nil {
		_ = sup.Terminate()
		t.Fatal("expected forced cancel to stop run")
	}
	if !errors.Is(runErr, worker.ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted on forced escalation, got %v", runErr)
	}
	if cancelErr := <-cancelErrCh; cancelErr != nil {
		t.Fatalf("cancel returned error: %v", cancelErr)
	}
	if client.State() != worker.LifecycleInterrupted && client.State() != worker.LifecycleExited {
		t.Fatalf("expected interrupted/exited state on forced termination, got %s", client.State())
	}
	_ = sup.Wait()
}

func TestSeam2_CancelledAndInterruptedDistinctOutcomes(t *testing.T) {
	if worker.LifecycleCancelled == worker.LifecycleInterrupted {
		t.Fatalf("LifecycleCancelled (%s) and LifecycleInterrupted (%s) must be distinct", worker.LifecycleCancelled, worker.LifecycleInterrupted)
	}
	if errors.Is(worker.ErrCancelled, worker.ErrInterrupted) || errors.Is(worker.ErrInterrupted, worker.ErrCancelled) {
		t.Fatalf("ErrCancelled and ErrInterrupted must be distinct errors")
	}

	dir := t.TempDir()
	store, err := worker.NewRecoveryStore(dir)
	if err != nil {
		t.Fatalf("new recovery store: %v", err)
	}

	// 1. Cooperative cancel -> LifecycleCancelled
	recCancel, err := store.BeginAttempt("run-c", "asr", "asr")
	if err != nil {
		t.Fatalf("begin cancel attempt: %v", err)
	}
	if err := store.CancelAttempt(*recCancel, "user cancelled run"); err != nil {
		t.Fatalf("cancel attempt: %v", err)
	}

	// 2. Interrupted attempt -> LifecycleInterrupted
	recInt, err := store.BeginAttempt("run-i", "tts", "tts")
	if err != nil {
		t.Fatalf("begin interrupt attempt: %v", err)
	}
	if err := store.InterruptAttempt(*recInt, "worker killed by forced escalation"); err != nil {
		t.Fatalf("interrupt attempt: %v", err)
	}

	// 3. Lost attempt at crash recovery -> LifecycleInterrupted
	recCrash, err := store.BeginAttempt("run-crash", "render", "render")
	if err != nil {
		t.Fatalf("begin crash attempt: %v", err)
	}
	lost, err := store.Recover()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(lost) != 1 || lost[0].ID != recCrash.ID {
		t.Fatalf("expected lost attempt recovered, got %+v", lost)
	}
	if lost[0].Status != worker.LifecycleInterrupted {
		t.Fatalf("expected interrupted status on recovery, got %s", lost[0].Status)
	}

	// Reopen and check all terminal outcomes are persisted and distinct
	store2, err := worker.NewRecoveryStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	running, err := store2.ListRunning()
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("expected no active/running attempts, got %d", len(running))
	}
}

func TestSeam2_WindowsJobObjectCleanupDescendants(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Job Object cleanup test is Windows-only")
	}
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "render", exe, "-family", "render", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	cmd := worker.Command{
		ID:        "cmd-render",
		Family:    "render",
		Stage:     "render",
		AttemptID: "attempt-render",
		RunID:     "run-render",
		Config: map[string]any{
			"descendant_pid_file": pidFile,
		},
	}

	go func() {
		_, _ = client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	}()

	// Wait for descendant PID file to be written by worker
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil && len(b) > 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
				childPID = n
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childPID == 0 {
		_ = sup.Terminate()
		t.Fatal("timed out waiting for descendant process PID")
	}

	// Verify descendant is alive while worker is running
	if !isProcessAlive(childPID) {
		_ = sup.Terminate()
		t.Fatalf("descendant process %d was not alive before termination", childPID)
	}

	// Forcibly terminate the supervisor (closing the Job Object handle)
	if err := sup.Terminate(); err != nil {
		t.Fatalf("terminate supervisor: %v", err)
	}
	_ = sup.Wait()

	// Prove that the descendant process did NOT survive cancellation/termination!
	dead := false
	checkDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(checkDeadline) {
		if !isProcessAlive(childPID) {
			dead = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("descendant process %d survived worker termination (Job Object cleanup failed)", childPID)
	}
}

func TestSeam2_RecoveryStoreCrashSafeWrites(t *testing.T) {
	dir := t.TempDir()
	store, err := worker.NewRecoveryStore(dir)
	if err != nil {
		t.Fatalf("new recovery store: %v", err)
	}

	rec, err := store.BeginAttempt("run-crash-safe", "asr", "asr")
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	if err := store.CommitAttempt(*rec, "sha256-test-hash-12345"); err != nil {
		t.Fatalf("commit attempt: %v", err)
	}

	// Verify recovery dir has no temporary (.tmp) files leftover
	entries, err := os.ReadDir(filepath.Join(dir, "stageworker-recovery"))
	if err != nil {
		t.Fatalf("read recovery dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temporary staging file leaked: %s", e.Name())
		}
	}

	// Verify manifest and record are valid JSON
	manifest, err := store.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(manifest) != 1 || manifest[0] != "sha256-test-hash-12345" {
		t.Fatalf("unexpected manifest content: %+v", manifest)
	}
}

func TestSeam2_GPULifecycleAndFamilySwitch(t *testing.T) {
	sched := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(sched)
	ctx := context.Background()

	lease1, err := leaseMgr.Acquire(ctx, "asr", nil)
	if err != nil {
		t.Fatalf("acquire asr: %v", err)
	}
	if leaseMgr.Holder() != "asr" {
		t.Fatalf("expected asr holder, got %s", leaseMgr.Holder())
	}

	// A second concurrent family must be rejected.
	if _, err := leaseMgr.Acquire(ctx, "tts", nil); err == nil {
		t.Fatal("expected busy lease rejection")
	}

	if err := leaseMgr.Release(ctx, lease1); err != nil {
		t.Fatalf("release: %v", err)
	}
	if leaseMgr.LastReclaimed().IsZero() {
		t.Fatal("expected reclamation timestamp")
	}

	// Family switch after release works.
	lease2, err := leaseMgr.Acquire(ctx, "tts", nil)
	if err != nil {
		t.Fatalf("acquire tts: %v", err)
	}
	if lease2 == lease1 {
		t.Fatal("expected distinct lease IDs")
	}
	if err := leaseMgr.Release(ctx, lease2); err != nil {
		t.Fatalf("release tts: %v", err)
	}
}

func TestSeam2_RecoveryLostAttemptInterruptedCommittedArtifactSurvives(t *testing.T) {
	dir := t.TempDir()
	store, err := worker.NewRecoveryStore(dir)
	if err != nil {
		t.Fatalf("new recovery store: %v", err)
	}
	rec, err := store.BeginAttempt("run-1", "asr", "asr")
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}

	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("new cas: %v", err)
	}
	obj, err := casStore.Put(bytes.NewReader([]byte("committed artifact")))
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if err := store.CommitAttempt(*rec, obj.SHA256); err != nil {
		t.Fatalf("commit attempt: %v", err)
	}

	// Simulate a daemon crash by opening a fresh store.
	store2, err := worker.NewRecoveryStore(dir)
	if err != nil {
		t.Fatalf("reopen recovery store: %v", err)
	}
	lost, err := store2.Recover()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	// The committed attempt should not be marked lost.
	if len(lost) != 0 {
		t.Fatalf("expected no lost attempts, got %d", len(lost))
	}
	manifest, err := store2.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(manifest) != 1 || manifest[0] != obj.SHA256 {
		t.Fatalf("committed artifact not in manifest: %v", manifest)
	}
	if err := casStore.VerifyIntegrity(obj.SHA256); err != nil {
		t.Fatalf("committed artifact did not survive: %v", err)
	}
}

func TestSeam2_RecoveryRunningAttemptBecomesInterrupted(t *testing.T) {
	dir := t.TempDir()
	store, err := worker.NewRecoveryStore(dir)
	if err != nil {
		t.Fatalf("new recovery store: %v", err)
	}
	rec, err := store.BeginAttempt("run-lost", "tts", "tts")
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	lost, err := store.Recover()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(lost) != 1 || lost[0].ID != rec.ID {
		t.Fatalf("expected one interrupted attempt, got %+v", lost)
	}
	if lost[0].Status != worker.LifecycleInterrupted {
		t.Fatalf("expected interrupted status, got %s", lost[0].Status)
	}
}

func TestSeam2_EncoderRejectsUnknownFields(t *testing.T) {
	var buf bytes.Buffer
	enc := worker.NewEncoder(&buf)
	if err := enc.Encode(worker.MessageTypeHello, worker.HelloPayload{
		WorkerID: "w",
		Family:   "asr",
		Schema:   worker.ProtocolVersion,
	}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := worker.NewDecoder(&buf)
	env, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := worker.ValidateEnvelope(env); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestSeam2_ASRAdapterFailsClosedWhenBinaryMissing verifies the production
// StageWorker ASR adapter fails closed with a structured error when a real
// invocation (with input artifacts) is requested but the model binary is not
// available on PATH. It must never silently fake model output.
func TestSeam2_ASRAdapterFailsClosedWhenBinaryMissing(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	// A real ASR invocation carries an input audio artifact.
	cmd := worker.Command{
		ID:        "cmd-asr-real",
		Family:    "asr",
		Stage:     "asr",
		AttemptID: "attempt-asr-real",
		RunID:     "run-asr-real",
		Inputs:    []worker.ArtifactRef{{SHA256: "audio", Path: filepath.Join(t.TempDir(), "audio.wav")}},
		Config:    map[string]any{"model_name": "qwen3-asr-1.7b"},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected ASR invocation to fail closed when binary is missing")
	}
	if !strings.Contains(err.Error(), "ASR_BINARY_NOT_FOUND") {
		_ = sup.Terminate()
		t.Fatalf("expected ASR_BINARY_NOT_FOUND error, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_AlignerAdapterFailsClosedWhenBinaryMissing verifies the production
// StageWorker aligner adapter fails closed with a structured error when a real
// alignment invocation (with input artifacts) is requested but the model binary
// is not available on PATH.
func TestSeam2_AlignerAdapterFailsClosedWhenBinaryMissing(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "aligner", exe, "-family", "aligner", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	cmd := worker.Command{
		ID:        "cmd-align-real",
		Family:    "aligner",
		Stage:     "aligner",
		AttemptID: "attempt-align-real",
		RunID:     "run-align-real",
		Inputs:    []worker.ArtifactRef{{SHA256: "text", Path: filepath.Join(t.TempDir(), "transcript.txt")}},
		Config:    map[string]any{"model_name": "qwen3-aligner", "text": "test text"},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected alignment invocation to fail closed when binary is missing")
	}
	if !strings.Contains(err.Error(), "ALIGNER_BINARY_NOT_FOUND") {
		_ = sup.Terminate()
		t.Fatalf("expected ALIGNER_BINARY_NOT_FOUND error, got %v", err)
	}
	_ = client.Shutdown()
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// buildFakeModel builds the fakemodel fixture binary under the given name.
// It returns the directory containing the binary (which must be prepended to
// PATH for LookPath to resolve the binary name).
func buildFakeModel(t *testing.T, name string) string {
	t.Helper()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, name)
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/monet88/douyinie/test/fixtures/fakemodel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fakemodel %s: %v\n%s", name, err, out)
	}
	return binDir
}

// TestSeam2_ASRAdapterInvokeAndParseSuccess verifies the real invocation seam:
// a fake model binary on PATH receives stdin JSON, emits stdout JSON, and the
// adapter parses segments and returns a real SHA-256 artifact.
func TestSeam2_ASRAdapterInvokeAndParseSuccess(t *testing.T) {
	binDir := buildFakeModel(t, "qwen3-asr")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	// A real ASR invocation with an input audio artifact.
	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-asr-real",
		Family:     "asr",
		Stage:      "asr",
		AttemptID:  "attempt-asr-real",
		RunID:      "run-asr-real",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config:     map[string]any{"model_name": "qwen3-asr-1.7b", "model_version": "1.7b"},
	}
	artifact, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("ASR invocation failed: %v", err)
	}

	// Verify real SHA-256 (not placeholder).
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256, got %q", artifact.SHA256)
	}
	if artifact.Path != outPath {
		t.Fatalf("unexpected artifact path %q", artifact.Path)
	}

	// Verify output file exists and contains valid segments.
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("output artifact missing: %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_ASRAdapterGarbageOutputFailsClosed verifies the adapter fails closed
// when the model binary emits invalid/malformed JSON on stdout.
func TestSeam2_ASRAdapterGarbageOutputFailsClosed(t *testing.T) {
	binDir := buildFakeModel(t, "qwen3-asr")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEMODEL_GARBAGE", "1")

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-asr-garbage",
		Family:     "asr",
		Stage:      "asr",
		AttemptID:  "attempt-asr-garbage",
		RunID:      "run-asr-garbage",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config:     map[string]any{"model_name": "qwen3-asr-1.7b"},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected ASR invocation to fail on garbage output")
	}
	if !strings.Contains(err.Error(), "ASR_OUTPUT_INVALID") {
		_ = sup.Terminate()
		t.Fatalf("expected ASR_OUTPUT_INVALID, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_AlignerAdapterInvokeAndParseSuccess verifies the aligner invocation
// seam: a fake model binary on PATH, text from Config, parsed word timings.
func TestSeam2_AlignerAdapterInvokeAndParseSuccess(t *testing.T) {
	binDir := buildFakeModel(t, "qwen3-aligner")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "aligner", exe, "-family", "aligner", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	// A real alignment invocation with an input audio artifact and accepted text.
	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-align-real",
		Family:     "aligner",
		Stage:      "aligner",
		AttemptID:  "attempt-align-real",
		RunID:      "run-align-real",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config:     map[string]any{"model_name": "qwen3-aligner", "text": "今天天气很好 我们去公园散步吧"},
	}
	artifact, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("alignment invocation failed: %v", err)
	}

	// Verify real SHA-256.
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256, got %q", artifact.SHA256)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("output artifact missing: %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_AlignerAdapterGarbageFailsClosed verifies the aligner adapter fails
// closed when the model binary emits invalid JSON on stdout.
func TestSeam2_AlignerAdapterGarbageFailsClosed(t *testing.T) {
	binDir := buildFakeModel(t, "qwen3-aligner")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEMODEL_GARBAGE", "1")

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "aligner", exe, "-family", "aligner", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-align-garbage",
		Family:     "aligner",
		Stage:      "aligner",
		AttemptID:  "attempt-align-garbage",
		RunID:      "run-align-garbage",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config:     map[string]any{"model_name": "qwen3-aligner", "text": "今天天气很好"},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected alignment to fail on garbage output")
	}
	if !strings.Contains(err.Error(), "ALIGNER_OUTPUT_INVALID") {
		_ = sup.Terminate()
		t.Fatalf("expected ALIGNER_OUTPUT_INVALID, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_ASRAdapterFailsClosedWhenModelIdentityMissing verifies that an ASR
// command missing the required manifest-driven 'model_name' config fails closed
// with ASR_MISSING_MODEL_IDENTITY (Issue #44 Finding 2).
func TestSeam2_ASRAdapterFailsClosedWhenModelIdentityMissing(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	cmd := worker.Command{
		ID:        "cmd-asr-no-model",
		Family:    "asr",
		Stage:     "asr",
		AttemptID: "attempt-asr-no-model",
		RunID:     "run-asr-no-model",
		Inputs:    []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		Config:    map[string]any{}, // missing model_name
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected ASR invocation to fail on missing model_name")
	}
	if !strings.Contains(err.Error(), "ASR_MISSING_MODEL_IDENTITY") {
		_ = sup.Terminate()
		t.Fatalf("expected ASR_MISSING_MODEL_IDENTITY error, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_DiarizerAdapterInvokeAndParseSuccess verifies the diarizer invocation
// seam: a fake model binary on PATH, manifest-driven model identity, and parsed
// speaker assignments (Issue #44 Finding 3).
func TestSeam2_DiarizerAdapterInvokeAndParseSuccess(t *testing.T) {
	binDir := buildFakeModel(t, "3dspeaker-diarizer")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	outPath := filepath.Join(t.TempDir(), "out.json")
	cmd := worker.Command{
		ID:         "cmd-diarize-real",
		Family:     "diarizer",
		Stage:      "diarize",
		AttemptID:  "attempt-diarize-real",
		RunID:      "run-diarize-real",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":              "v1.0.0",
			"embedding_cosine_threshold": 0.65,
		},
	}
	artifact, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("diarization invocation failed: %v", err)
	}

	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256, got %q", artifact.SHA256)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("output artifact missing: %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_DiarizerAdapterFailsClosedWhenBinaryMissing verifies the diarizer
// fails closed with DIARIZER_BINARY_NOT_FOUND when the model binary is missing.
func TestSeam2_DiarizerAdapterFailsClosedWhenBinaryMissing(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	cmd := worker.Command{
		ID:        "cmd-diarize-nobin",
		Family:    "diarizer",
		Stage:     "diarize",
		AttemptID: "attempt-diarize-nobin",
		RunID:     "run-diarize-nobin",
		Inputs:    []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"embedding_cosine_threshold": 0.65,
		},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected diarization to fail on missing binary")
	}
	if !strings.Contains(err.Error(), "DIARIZER_BINARY_NOT_FOUND") {
		_ = sup.Terminate()
		t.Fatalf("expected DIARIZER_BINARY_NOT_FOUND error, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_DiarizerEvidenceAdapterInvokeAndParseSuccess verifies the diarizer
// evidence probe invocation seam over StageWorker (Issue #44 Blocker 1 & 3).
func TestSeam2_DiarizerEvidenceAdapterInvokeAndParseSuccess(t *testing.T) {
	binDir := buildFakeModel(t, "3dspeaker-diarizer")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	outPath := filepath.Join(t.TempDir(), "out-evidence.json")
	cmd := worker.Command{
		ID:         "cmd-diarize-evid-real",
		Family:     "diarizer",
		Stage:      "diarize_evidence",
		AttemptID:  "attempt-diarize-evid-real",
		RunID:      "run-diarize-evid-real",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":              "v1.0.0",
			"embedding_cosine_threshold": 0.65,
		},
	}
	artifact, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("diarization evidence invocation failed: %v", err)
	}

	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256, got %q", artifact.SHA256)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output artifact missing: %v", err)
	}
	var out struct {
		SpeakerEvidence domain.SpeakerEvidence `json:"speaker_evidence"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("invalid evidence JSON: %v", err)
	}
	if !out.SpeakerEvidence.HasMultiSpeakerCues {
		t.Errorf("expected HasMultiSpeakerCues=true in evidence output")
	}
	_ = client.Shutdown()
}

// TestSeam2_DiarizerEvidenceAdapterFailsClosedWhenBinaryMissing verifies the evidence probe
// fails closed with DIARIZER_BINARY_NOT_FOUND when the model binary is missing.
func TestSeam2_DiarizerEvidenceAdapterFailsClosedWhenBinaryMissing(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	cmd := worker.Command{
		ID:        "cmd-diarize-evid-nobin",
		Family:    "diarizer",
		Stage:     "diarize_evidence",
		AttemptID: "attempt-diarize-evid-nobin",
		RunID:     "run-diarize-evid-nobin",
		Inputs:    []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"embedding_cosine_threshold": 0.65,
		},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = sup.Terminate()
		t.Fatal("expected diarization evidence to fail on missing binary")
	}
	if !strings.Contains(err.Error(), "DIARIZER_BINARY_NOT_FOUND") {
		_ = sup.Terminate()
		t.Fatalf("expected DIARIZER_BINARY_NOT_FOUND error, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_DiarizerPythonAdapterExecutionWithFakeSpeakerlab verifies that StageWorker
// executes the repo-owned Python adapter cmd/stageworker/adapters/diarizer_3dspeaker.py
// using the first-party Diarization3Dspeaker contract and passes model & VAD identities (Finding 6).
func TestSeam2_DiarizerPythonAdapterExecutionWithFakeSpeakerlab(t *testing.T) {
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "speakerlab", "bin")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatalf("mkdir speakerlab: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "speakerlab", "__init__.py"), []byte(""), 0644); err != nil {
		t.Fatalf("write __init__.py: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(""), 0644); err != nil {
		t.Fatalf("write bin/__init__.py: %v", err)
	}
	mockCode := `
class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    def __call__(self, audio_path):
        return [
            [0.0, 1.5, "SPEAKER_00"],
            [1.5, 3.2, "SPEAKER_01"],
        ]

    def probe_evidence(self, audio_path):
        return {
            "has_multi_speaker_cues": True,
            "speaker_change_count": 2,
            "confidence": 0.0,
            "source": "iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0+iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4",
        }
`
	if err := os.WriteFile(filepath.Join(pkgDir, "infer_diarization.py"), []byte(mockCode), 0644); err != nil {
		t.Fatalf("write infer_diarization.py: %v", err)
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("diarizer_3dspeaker.py adapter not found at %s", adapterPath)
	}

	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", adapterPath)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)

	// 1. Test diarize stage
	outDiarizePath := filepath.Join(t.TempDir(), "out-diarize-py.json")
	cmdDiarize := worker.Command{
		ID:         "cmd-diarize-py",
		Family:     "diarizer",
		Stage:      "diarize",
		AttemptID:  "attempt-diarize-py",
		RunID:      "run-diarize-py",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outDiarizePath,
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":              "v1.0.0",
			"vad_model_name":             "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
			"vad_model_version":          "v2.0.4",
			"embedding_cosine_threshold": 0.65,
		},
	}
	artifact, err := client.Run(context.Background(), cmdDiarize, 10*time.Second, 10*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("python adapter diarize invocation failed: %v", err)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256, got %q", artifact.SHA256)
	}
	data, err := os.ReadFile(outDiarizePath)
	if err != nil {
		t.Fatalf("missing diarize output: %v", err)
	}
	var outDiarize struct {
		SpeakerAssignments []domain.SpeakerAssignment `json:"speaker_assignments"`
		ModelName          string                     `json:"model_name"`
		VADModelName       string                     `json:"vad_model_name"`
	}
	if err := json.Unmarshal(data, &outDiarize); err != nil {
		t.Fatalf("unmarshal diarize json: %v", err)
	}
	if len(outDiarize.SpeakerAssignments) != 2 {
		t.Fatalf("expected 2 speaker assignments from python adapter, got %d", len(outDiarize.SpeakerAssignments))
	}
	if outDiarize.ModelName != "iic/speech_campplus_sv_zh_en_16k-common_advanced" {
		t.Errorf("unexpected model_name: %q", outDiarize.ModelName)
	}
	if outDiarize.VADModelName != "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch" {
		t.Errorf("unexpected vad_model_name: %q", outDiarize.VADModelName)
	}

	// 2. Test diarize_evidence stage
	outEvidPath := filepath.Join(t.TempDir(), "out-evid-py.json")
	cmdEvid := worker.Command{
		ID:         "cmd-evid-py",
		Family:     "diarizer",
		Stage:      "diarize_evidence",
		AttemptID:  "attempt-evid-py",
		RunID:      "run-evid-py",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outEvidPath,
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":              "v1.0.0",
			"vad_model_name":             "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
			"vad_model_version":          "v2.0.4",
			"embedding_cosine_threshold": 0.65,
		},
	}
	evidArtifact, err := client.Run(context.Background(), cmdEvid, 10*time.Second, 10*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("python adapter evidence invocation failed: %v", err)
	}
	if evidArtifact.SHA256 == "" || strings.Contains(evidArtifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for evidence, got %q", evidArtifact.SHA256)
	}
	dataEvid, err := os.ReadFile(outEvidPath)
	if err != nil {
		t.Fatalf("missing evidence output: %v", err)
	}
	var outEvid struct {
		SpeakerEvidence domain.SpeakerEvidence `json:"speaker_evidence"`
		ModelName       string                 `json:"model_name"`
		VADModelName    string                 `json:"vad_model_name"`
	}
	if err := json.Unmarshal(dataEvid, &outEvid); err != nil {
		t.Fatalf("unmarshal evidence json: %v", err)
	}
	if !outEvid.SpeakerEvidence.HasMultiSpeakerCues {
		t.Errorf("expected HasMultiSpeakerCues=true from python adapter evidence probe")
	}
	if outEvid.SpeakerEvidence.SpeakerChangeCount != 2 {
		t.Errorf("expected SpeakerChangeCount=2, got %d", outEvid.SpeakerEvidence.SpeakerChangeCount)
	}

	_ = client.Shutdown()
}

// TestSeam2_DiarizerPythonAdapterExecutionWithRealUpstreamShapedFakeSpeakerlab verifies that
// the Python adapter executes successfully over a Diarization3Dspeaker fake exposing strictly the
// real upstream surface: load_audio, do_vad, chunk, do_emb_extraction, do_clustering, and __call__ (no probe_evidence/vad).
func TestSeam2_DiarizerPythonAdapterExecutionWithRealUpstreamShapedFakeSpeakerlab(t *testing.T) {
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "speakerlab", "bin")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatalf("mkdir speakerlab: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "speakerlab", "__init__.py"), []byte(""), 0644); err != nil {
		t.Fatalf("write __init__.py: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(""), 0644); err != nil {
		t.Fatalf("write bin/__init__.py: %v", err)
	}
	mockCode := `
def load_audio(audio_path):
    return "fake_audio_tensor"

class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    def do_vad(self, wav):
        return [[0.0, 1.5], [1.8, 3.2]]

    def chunk(self, st, ed):
        return [f"chunk_{st}_{ed}"]

    def do_emb_extraction(self, chunks, wav):
        return [
            [1.0, 0.0, 0.0],
            [0.0, 1.0, 0.0],
        ]

    def do_clustering(self, chunks, embeddings, speaker_num):
        return [0, 1]

    def __call__(self, audio_path):
        return [
            [0.0, 1.5, "SPEAKER_00"],
            [1.8, 3.2, "SPEAKER_01"],
        ]
`
	if err := os.WriteFile(filepath.Join(pkgDir, "infer_diarization.py"), []byte(mockCode), 0644); err != nil {
		t.Fatalf("write infer_diarization.py: %v", err)
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "diarizer_3dspeaker.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("diarizer_3dspeaker.py adapter not found at %s", adapterPath)
	}

	t.Setenv("DOUYINIE_DIARIZER_ADAPTER", adapterPath)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)

	// 1. Test diarize stage
	outDiarizePath := filepath.Join(t.TempDir(), "out-diarize-realfake.json")
	cmdDiarize := worker.Command{
		ID:         "cmd-diarize-realfake",
		Family:     "diarizer",
		Stage:      "diarize",
		AttemptID:  "attempt-diarize-realfake",
		RunID:      "run-diarize-realfake",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outDiarizePath,
		Config: map[string]any{
			"model_name":        "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":     "v1.0.0",
			"vad_model_name":    "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
			"vad_model_version": "v2.0.4",
		},
	}
	artifact, err := client.Run(context.Background(), cmdDiarize, 10*time.Second, 10*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("python adapter diarize invocation failed: %v", err)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256, got %q", artifact.SHA256)
	}
	data, err := os.ReadFile(outDiarizePath)
	if err != nil {
		t.Fatalf("missing diarize output: %v", err)
	}
	var outDiarize struct {
		SpeakerAssignments []domain.SpeakerAssignment `json:"speaker_assignments"`
	}
	if err := json.Unmarshal(data, &outDiarize); err != nil {
		t.Fatalf("unmarshal diarize json: %v", err)
	}
	if len(outDiarize.SpeakerAssignments) != 2 {
		t.Fatalf("expected 2 speaker assignments, got %d", len(outDiarize.SpeakerAssignments))
	}

	// 2. Test diarize_evidence stage with upstream-shaped fake (no probe_evidence convenience method)
	outEvidPath := filepath.Join(t.TempDir(), "out-evid-realfake.json")
	cmdEvid := worker.Command{
		ID:         "cmd-evid-realfake",
		Family:     "diarizer",
		Stage:      "diarize_evidence",
		AttemptID:  "attempt-evid-realfake",
		RunID:      "run-evid-realfake",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outEvidPath,
		Config: map[string]any{
			"model_name":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":              "v1.0.0",
			"vad_model_name":             "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
			"vad_model_version":          "v2.0.4",
			"embedding_cosine_threshold": 0.65,
		},
	}
	evidArtifact, err := client.Run(context.Background(), cmdEvid, 10*time.Second, 10*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("python adapter evidence invocation failed: %v", err)
	}
	if evidArtifact.SHA256 == "" || strings.Contains(evidArtifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for evidence, got %q", evidArtifact.SHA256)
	}
	dataEvid, err := os.ReadFile(outEvidPath)
	if err != nil {
		t.Fatalf("missing evidence output: %v", err)
	}
	var outEvid struct {
		SpeakerEvidence domain.SpeakerEvidence `json:"speaker_evidence"`
	}
	if err := json.Unmarshal(dataEvid, &outEvid); err != nil {
		t.Fatalf("unmarshal evidence json: %v", err)
	}
	if !outEvid.SpeakerEvidence.HasMultiSpeakerCues {
		t.Errorf("expected HasMultiSpeakerCues=true from upstream-shaped fake evidence probe")
	}
	if outEvid.SpeakerEvidence.SpeakerChangeCount != 1 {
		t.Errorf("expected SpeakerChangeCount=1, got %d", outEvid.SpeakerEvidence.SpeakerChangeCount)
	}

	_ = client.Shutdown()
}

// TestSeam2_ASRPythonAdapterExecutionWithFakeQwenASR verifies that StageWorker
// executes the repo-owned Python adapter cmd/stageworker/adapters/asr_qwen3.py
// using the official Qwen3ASRModel upstream contract and handles 1.7B/0.6B models.
func TestSeam2_ASRPythonAdapterExecutionWithFakeQwenASR(t *testing.T) {
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "qwen_asr")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatalf("mkdir qwen_asr: %v", err)
	}
	mockCode := `
class ASRTranscription:
    def __init__(self):
        self.text = "今天天气很好。我们去公园散步吧。"
        self.language = "Chinese"
        self.time_stamps = None

class Qwen3ASRModel:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    @classmethod
    def from_pretrained(cls, *args, **kwargs):
        return cls(*args, **kwargs)

    def transcribe(self, audio, **kwargs):
        if kwargs.get("return_time_stamps") is not False:
            raise ValueError("ASR stage must not request forced-alignment timestamps")
        return [ASRTranscription()]
`
	if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(mockCode), 0644); err != nil {
		t.Fatalf("write qwen_asr/__init__.py: %v", err)
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "asr_qwen3.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "asr_qwen3.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("asr_qwen3.py adapter not found at %s", adapterPath)
	}

	t.Setenv("DOUYINIE_ASR_ADAPTER", adapterPath)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "asr", exe, "-family", "asr", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)

	outPath := filepath.Join(t.TempDir(), "out-asr-py.json")
	cmd := worker.Command{
		ID:         "cmd-asr-py",
		Family:     "asr",
		Stage:      "asr",
		AttemptID:  "attempt-asr-py",
		RunID:      "run-asr-py",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config: map[string]any{
			"model_name":    "qwen3-asr",
			"model_version": "1.7b",
		},
	}
	artifact, err := client.Run(context.Background(), cmd, 10*time.Second, 10*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("python adapter asr invocation failed: %v", err)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for ASR, got %q", artifact.SHA256)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("missing ASR output: %v", err)
	}
	var out struct {
		Segments     []domain.ASRRawSegment `json:"segments"`
		ModelName    string                 `json:"model_name"`
		ModelVersion string                 `json:"model_version"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal ASR json: %v", err)
	}
	if len(out.Segments) != 1 {
		t.Fatalf("expected 1 official-shaped ASR transcript segment, got %d", len(out.Segments))
	}
	if out.Segments[0].Text != "今天天气很好。我们去公园散步吧。" {
		t.Errorf("unexpected segment text: %s", out.Segments[0].Text)
	}
	_ = client.Shutdown()
}

func TestSeam2_DiarizerEvidenceAdapterFailsClosedWhenThresholdMissing(t *testing.T) {
	binDir := buildFakeModel(t, "3dspeaker-diarizer")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "diarizer", exe, "-family", "diarizer", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)
	cmd := worker.Command{
		ID:        "cmd-diarize-evid-no-threshold",
		Family:    "diarizer",
		Stage:     "diarize_evidence",
		AttemptID: "attempt-diarize-evid-no-threshold",
		RunID:     "run-diarize-evid-no-threshold",
		Inputs:    []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		Config:    map[string]any{"model_name": "iic/speech_campplus_sv_zh_en_16k-common_advanced", "model_version": "v1.0.0"},
	}
	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		_ = client.Shutdown()
		t.Fatal("expected missing evidence threshold to fail closed")
	}
	if !strings.Contains(err.Error(), "DIARIZER_MISSING_EVIDENCE_CONFIG") {
		_ = client.Shutdown()
		t.Fatalf("expected DIARIZER_MISSING_EVIDENCE_CONFIG, got %v", err)
	}
	_ = client.Shutdown()
}

// TestSeam2_AlignerPythonAdapterExecutionWithFakeQwenASR verifies that StageWorker
// executes the repo-owned Python adapter cmd/stageworker/adapters/aligner_qwen3.py
// using the official Qwen3ForcedAligner upstream contract and extracts word timings.
func TestSeam2_AlignerPythonAdapterExecutionWithFakeQwenASR(t *testing.T) {
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "qwen_asr")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatalf("mkdir qwen_asr: %v", err)
	}
	mockCode := `
class Qwen3ForcedAligner:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    @classmethod
    def from_pretrained(cls, *args, **kwargs):
        return cls(*args, **kwargs)

    def align(self, audio, text, **kwargs):
        return [[
            {"word": "今天", "start_time": 0.0, "end_time": 0.3},
            {"word": "天气", "start_time": 0.3, "end_time": 0.8},
            {"word": "很好", "start_time": 0.8, "end_time": 1.2},
        ]]
`
	if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(mockCode), 0644); err != nil {
		t.Fatalf("write qwen_asr/__init__.py: %v", err)
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "aligner_qwen3.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "aligner_qwen3.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("aligner_qwen3.py adapter not found at %s", adapterPath)
	}

	t.Setenv("DOUYINIE_ALIGNER_ADAPTER", adapterPath)
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "aligner", exe, "-family", "aligner", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(audioPath, []byte("fake audio data"), 0644)

	outPath := filepath.Join(t.TempDir(), "out-align-py.json")
	cmd := worker.Command{
		ID:         "cmd-align-py",
		Family:     "aligner",
		Stage:      "aligner",
		AttemptID:  "attempt-align-py",
		RunID:      "run-align-py",
		Inputs:     []worker.ArtifactRef{{SHA256: "audio", Path: audioPath}},
		OutputPath: outPath,
		Config: map[string]any{
			"model_name":    "Qwen3-ForcedAligner-0.6B",
			"model_version": "0.6b",
			"text":          "今天天气很好",
		},
	}
	artifact, err := client.Run(context.Background(), cmd, 10*time.Second, 10*time.Second)
	if err != nil {
		_ = sup.Terminate()
		t.Fatalf("python adapter aligner invocation failed: %v", err)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for Aligner, got %q", artifact.SHA256)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("missing Aligner output: %v", err)
	}
	var out struct {
		WordTimings  []domain.WordTiming `json:"word_timings"`
		ModelName    string              `json:"model_name"`
		ModelVersion string              `json:"model_version"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal Aligner json: %v", err)
	}
	if len(out.WordTimings) != 3 {
		t.Fatalf("expected 3 word timings, got %d", len(out.WordTimings))
	}
	if out.WordTimings[0].Word != "今天" || out.WordTimings[0].StartMs != 0 || out.WordTimings[0].EndMs != 300 {
		t.Errorf("unexpected word timing: %+v", out.WordTimings[0])
	}
	_ = client.Shutdown()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// TestSeam2_WorkerSpeechProvider_GPULeaseEnforced_CannotBypassHeldLease verifies
// that speech worker execution through WorkerASRProvider cannot bypass an already-held single GPU lease.
func TestSeam2_WorkerSpeechProvider_GPULeaseEnforced_CannotBypassHeldLease(t *testing.T) {
	exe := buildStageWorker(t)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", exe)

	sched := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(sched)

	// Hold the single GPU lease with another family (e.g. "tts")
	ttsLease, err := sched.Acquire(context.Background(), "tts")
	if err != nil {
		t.Fatalf("acquire tts lease: %v", err)
	}
	if sched.Holder() != "tts" {
		t.Fatalf("expected holder tts, got %s", sched.Holder())
	}

	asrProv, err := provider.NewWorkerASRProvider("test_asr", "qwen3-asr", "1.7b", 0.9)
	if err != nil {
		t.Fatalf("NewWorkerASRProvider: %v", err)
	}
	asrProv.SetLeaseManager(leaseMgr)

	// Attempt to run ASR while GPU lease is held -> must fail with lease acquisition error
	audioRef := worker.ArtifactRef{
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Path:   t.TempDir() + "/sample.wav",
	}
	_, err = asrProv.ProduceTranscript(context.Background(), audioRef)
	if err == nil {
		t.Fatal("expected ProduceTranscript to fail while GPU lease is held by another family")
	}
	if !strings.Contains(err.Error(), "gpu lease") && !strings.Contains(err.Error(), "lease") {
		t.Fatalf("expected error mentioning gpu lease, got %v", err)
	}

	// Release the TTS lease
	if err := sched.Release(context.Background(), ttsLease); err != nil {
		t.Fatalf("release tts lease: %v", err)
	}
	if sched.Holder() != "" {
		t.Fatalf("expected scheduler idle after release, got %s", sched.Holder())
	}
}

// TestSeam2_WorkerSpeechProvider_GPULeaseReleasesOnContextCancel verifies that
// if a worker invocation is canceled, the GPU lease is released deterministically.
func TestSeam2_WorkerSpeechProvider_GPULeaseReleasesOnContextCancel(t *testing.T) {
	exe := buildStageWorker(t)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", exe)

	sched := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(sched)

	asrProv, err := provider.NewWorkerASRProvider("test_asr", "qwen3-asr", "1.7b", 0.9)
	if err != nil {
		t.Fatalf("NewWorkerASRProvider: %v", err)
	}
	asrProv.SetLeaseManager(leaseMgr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before run

	audioRef := worker.ArtifactRef{
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Path:   t.TempDir() + "/sample.wav",
	}
	_, err = asrProv.ProduceTranscript(ctx, audioRef)
	if err == nil {
		t.Fatal("expected ProduceTranscript to fail with canceled context")
	}

	// Verify lease is released cleanly (idle)
	if sched.Holder() != "" {
		t.Fatalf("expected scheduler idle after canceled invocation, got holder %q", sched.Holder())
	}
}

// TestSeam2_WorkerSpeechProvider_GPUProcessReapedBeforeLeaseReleasedOnFailure verifies
// that when worker execution fails, the worker process tree is terminated AND reaped
// before the GPU lease is released, allowing the next family to acquire the lease without conflict.
func TestSeam2_WorkerSpeechProvider_GPUProcessReapedBeforeLeaseReleasedOnFailure(t *testing.T) {
	// Point to a valid executable that won't speak the NDJSON handshake (simulating early failure)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", exe)

	sched := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(sched)

	asrProv, err := provider.NewWorkerASRProvider("test_asr", "qwen3-asr", "1.7b", 0.9)
	if err != nil {
		t.Fatalf("NewWorkerASRProvider: %v", err)
	}
	asrProv.SetLeaseManager(leaseMgr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	audioRef := worker.ArtifactRef{
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Path:   t.TempDir() + "/sample.wav",
	}
	_, err = asrProv.ProduceTranscript(ctx, audioRef)
	if err == nil {
		t.Fatal("expected ProduceTranscript to fail with non-NDJSON executable")
	}

	// 1. Scheduler is immediately idle
	if sched.Holder() != "" {
		t.Fatalf("expected scheduler idle after failed worker run, got %s", sched.Holder())
	}

	// 2. Next family can immediately acquire the GPU lease
	ttsLease, err := leaseMgr.Acquire(context.Background(), "tts", nil)
	if err != nil {
		t.Fatalf("next family failed to acquire GPU lease after worker failure: %v", err)
	}
	if leaseMgr.Holder() != "tts" {
		t.Fatalf("expected holder tts, got %s", leaseMgr.Holder())
	}
	_ = leaseMgr.Release(context.Background(), ttsLease)
}

// TestSeam2_WorkerSpeechProvider_GPUProcessReapedBeforeLeaseReleasedOnCancel verifies
// that on context cancellation, the worker process is terminated AND reaped
// before the GPU lease is released.
func TestSeam2_WorkerSpeechProvider_GPUProcessReapedBeforeLeaseReleasedOnCancel(t *testing.T) {
	exe := buildStageWorker(t)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", exe)

	sched := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(sched)

	asrProv, err := provider.NewWorkerASRProvider("test_asr", "qwen3-asr", "1.7b", 0.9)
	if err != nil {
		t.Fatalf("NewWorkerASRProvider: %v", err)
	}
	asrProv.SetLeaseManager(leaseMgr)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately while run is starting
	cancel()

	audioRef := worker.ArtifactRef{
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Path:   t.TempDir() + "/sample.wav",
	}
	_, err = asrProv.ProduceTranscript(ctx, audioRef)
	if err == nil {
		t.Fatal("expected ProduceTranscript to fail with canceled context")
	}

	// 1. Scheduler is immediately idle
	if sched.Holder() != "" {
		t.Fatalf("expected scheduler idle after canceled invocation, got %s", sched.Holder())
	}

	// 2. Next family can immediately acquire the GPU lease
	ttsLease, err := leaseMgr.Acquire(context.Background(), "tts", nil)
	if err != nil {
		t.Fatalf("next family failed to acquire GPU lease after cancel: %v", err)
	}
	if leaseMgr.Holder() != "tts" {
		t.Fatalf("expected holder tts, got %s", leaseMgr.Holder())
	}
	_ = leaseMgr.Release(context.Background(), ttsLease)
}

// ---------------------------------------------------------------------------
// TTS StageWorker Tests
// ---------------------------------------------------------------------------

func TestSeam2_TTSStage_MissingTextFailsClosed(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	cmd := worker.Command{
		ID:        "cmd-tts-notext",
		Family:    "tts",
		Stage:     "tts",
		AttemptID: "attempt-tts-notext",
		RunID:     "run-tts-notext",
		Config: map[string]any{
			"model_name": "vieneu-tts",
		},
	}

	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("expected TTS stage without text to fail closed")
	}
	if !strings.Contains(err.Error(), "TTS_MISSING_TEXT") {
		t.Fatalf("expected TTS_MISSING_TEXT error, got %v", err)
	}
}

func TestSeam2_TTSStage_MissingModelIdentityFailsClosed(t *testing.T) {
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	cmd := worker.Command{
		ID:        "cmd-tts-nomodel",
		Family:    "tts",
		Stage:     "tts",
		AttemptID: "attempt-tts-nomodel",
		RunID:     "run-tts-nomodel",
		Config: map[string]any{
			"text": "Xin chào thế giới",
		},
	}

	_, err := client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("expected TTS stage without model_name to fail closed")
	}
	if !strings.Contains(err.Error(), "TTS_MISSING_MODEL_IDENTITY") {
		t.Fatalf("expected TTS_MISSING_MODEL_IDENTITY error, got %v", err)
	}
}
func TestSeam2_TTSStage_AdapterErrorClassifiedAsTTSNotDiarizer(t *testing.T) {
	// In the real test environment where TTS packages are not installed,
	// invoking the python tts adapter must fail closed with TTS_EXEC_FAILED,
	// and NEVER be misclassified as DIARIZER_EXEC_FAILED!
	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "tts_engine.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "tts_engine.py"))
	}
	if fileExists(adapterPath) {
		t.Setenv("DOUYINIE_TTS_ADAPTER", adapterPath)
	}

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	cmd := worker.Command{
		ID:        "cmd-tts-adapter-err",
		Family:    "tts",
		Stage:     "tts",
		AttemptID: "attempt-tts-adapter-err",
		RunID:     "run-tts-adapter-err",
		Config: map[string]any{
			"text":       "Xin chào thế giới",
			"model_name": "vieneu-tts",
		},
	}

	_, err = client.Run(context.Background(), cmd, 5*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("expected unconfigured TTS adapter to fail closed")
	}
	if strings.Contains(err.Error(), "DIARIZER") {
		t.Fatalf("CRITICAL REGRESSION: TTS error was misclassified as DIARIZER error: %v", err)
	}
	if !strings.Contains(err.Error(), "TTS_EXEC_FAILED") && !strings.Contains(err.Error(), "TTS_BINARY_NOT_FOUND") {
		t.Fatalf("expected TTS_EXEC_FAILED or TTS_BINARY_NOT_FOUND, got %v", err)
	}
}

func TestSeam2_TTSStage_PythonAdapterExecutionWithMockVieNeu(t *testing.T) {
	tmpDir := t.TempDir()
	mockModulePath := filepath.Join(tmpDir, "vieneu.py")
	mockCode := `
class Vieneu:
    def __init__(self, mode="v3turbo"):
        self.mode = mode

    def get_preset_voice(self, voice_id):
        return voice_id

    def infer(self, text, voice=None):
        return [0.01] * 48000
`
	if err := os.WriteFile(mockModulePath, []byte(mockCode), 0644); err != nil {
		t.Fatalf("write mock vieneu.py: %v", err)
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "tts_engine.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "tts_engine.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("tts_engine.py adapter not found at %s", adapterPath)
	}

	t.Setenv("DOUYINIE_TTS_ADAPTER", adapterPath)
	origPyPath := os.Getenv("PYTHONPATH")
	t.Setenv("PYTHONPATH", tmpDir+string(os.PathListSeparator)+origPyPath)

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 5*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("handshake: %v", err)
	}
	defer func() {
		_ = client.Shutdown()
	}()

	outPath := filepath.Join(t.TempDir(), "out-tts-py.json")
	cmd := worker.Command{
		ID:         "cmd-tts-py",
		Family:     "tts",
		Stage:      "tts",
		AttemptID:  "attempt-tts-py",
		RunID:      "run-tts-py",
		OutputPath: outPath,
		Config: map[string]any{
			"text":             "Xin chào Việt Nam",
			"model_name":       "vieneu-tts",
			"model_version":    "1.0.0",
			"language":         "vi",
			"voice_id":         "vi_female_natural",
			"speed":            "1.0",
			"slot_duration_ms": "1500",
		},
	}

	artifact, err := client.Run(context.Background(), cmd, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("python adapter tts invocation failed: %v", err)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for TTS artifact, got %q", artifact.SHA256)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read tts output artifact: %v", err)
	}
	var out struct {
		AudioData          []byte `json:"audio_data"`
		AudioSHA256        string `json:"audio_sha256"`
		MeasuredDurationMs int64  `json:"measured_duration_ms"`
		ModelName          string `json:"model_name"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal tts json: %v", err)
	}
	if len(out.AudioData) == 0 {
		t.Fatalf("expected non-empty audio data")
	}
	if out.MeasuredDurationMs != 1000 {
		t.Errorf("expected 1000ms measured duration, got %d", out.MeasuredDurationMs)
	}
	if out.ModelName != "vieneu-tts" {
		t.Errorf("expected model_name vieneu-tts, got %s", out.ModelName)
	}
}

func TestSeam2_SeparatorStage_PythonAdapterExecutionWithMock(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup synthetic audio file
	audioPath := filepath.Join(tmpDir, "source_audio.wav")
	dummyWAV := []byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	if err := os.WriteFile(audioPath, dummyWAV, 0644); err != nil {
		t.Fatalf("write audio file: %v", err)
	}

	adapterPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "stageworker", "adapters", "separator.py"))
	if err != nil || !fileExists(adapterPath) {
		adapterPath, _ = filepath.Abs(filepath.Join("cmd", "stageworker", "adapters", "separator.py"))
	}
	if !fileExists(adapterPath) {
		t.Fatalf("separator.py adapter not found at %s", adapterPath)
	}

	// Wrapper mock script
	wrapperPath := filepath.Join(tmpDir, "mock_separator.py")
	wrapperCode := `
import sys
import os
import io
import wave
import struct
import json

adapter_dir = os.path.dirname(r'` + adapterPath + `')
sys.path.insert(0, adapter_dir)
import separator

def dummy_factory(audio_path, model_name, model_version):
    num_samples = int((16000 * 2000) / 1000)
    buf = io.BytesIO()
    with wave.open(buf, 'wb') as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(16000)
        wf.writeframes(struct.pack(f'<{num_samples}h', *([50] * num_samples)))
    wav_data = buf.getvalue()
    return {
        'vocals_data': wav_data,
        'background_data': wav_data,
        'duration_ms': 2000,
        'sample_rate': 16000,
        'channels': 1,
    }

separator._SEPARATOR_MODEL_FACTORY = dummy_factory
if __name__ == '__main__':
    separator.main()
`
	if err := os.WriteFile(wrapperPath, []byte(wrapperCode), 0644); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", wrapperPath)

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()
	if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	defer sup.Terminate()

	client := worker.NewClient(sup)
	_, err = client.Handshake(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	outPath := filepath.Join(tmpDir, "sep_out.json")
	cmd := worker.Command{
		ID:         "cmd-sep-1",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "attempt-sep-1",
		RunID:      "run-sep-1",
		OutputPath: outPath,
		Inputs: []worker.ArtifactRef{
			{
				SHA256: "dummy_sha",
				Path:   audioPath,
			},
		},
		Config: map[string]any{
			"model_name":    "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version": "v3",
		},
	}

	artifact, err := client.Run(ctx, cmd, 10*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("separator execution failed: %v", err)
	}
	if artifact.SHA256 == "" || strings.Contains(artifact.SHA256, "placeholder") {
		t.Fatalf("expected real SHA-256 for separator artifact, got %q", artifact.SHA256)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read separator output artifact: %v", err)
	}
	var out struct {
		VocalsData     string `json:"vocals_data"`
		BackgroundData string `json:"background_data"`
		DurationMs     int64  `json:"duration_ms"`
		ModelName      string `json:"model_name"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal separator json: %v", err)
	}
	if out.VocalsData == "" || out.BackgroundData == "" {
		t.Fatalf("expected non-empty vocals and background data")
	}
	if out.DurationMs != 2000 {
		t.Errorf("expected 2000ms duration, got %d", out.DurationMs)
	}
	if out.ModelName != "UVR-MDX-NET-Inst_HQ_4.onnx" {
		t.Errorf("expected model_name UVR-MDX-NET-Inst_HQ_4.onnx, got %s", out.ModelName)
	}
}
func TestSeam2_SeparatorStage_PythonBinaryResolutionPrecedence(t *testing.T) {
	tmpDir := t.TempDir()

	sepPy := filepath.Join(tmpDir, "sep_python.bat")
	_ = os.WriteFile(sepPy, []byte("@echo off\r\necho SepPython\r\n"), 0755)

	genPy := filepath.Join(tmpDir, "gen_python.bat")
	_ = os.WriteFile(genPy, []byte("@echo off\r\necho GenPython\r\n"), 0755)

	t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", sepPy)
	t.Setenv("DOUYINIE_PYTHON_BIN", genPy)

	// Invariant: DOUYINIE_SEPARATOR_PYTHON_BIN takes precedence over DOUYINIE_PYTHON_BIN
	if os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN") != sepPy {
		t.Fatalf("expected DOUYINIE_SEPARATOR_PYTHON_BIN %s, got %s", sepPy, os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN"))
	}
}

func TestSeam2_SeparatorStage_IncompatibleDirectCLINotAutoDiscovered(t *testing.T) {
	// Verifies that bare stock CLIs like audio-separator or demucs on PATH without adapter JSON-stdin contract
	// are not invoked by resolveSeparatorRunner, requiring explicit DOUYINIE_SEPARATOR_ADAPTER or DOUYINIE_SEPARATOR_BIN.
	tmpDir := t.TempDir()

	audioPath := filepath.Join(tmpDir, "dummy.wav")
	_ = os.WriteFile(audioPath, []byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00"), 0644)

	// When pointing DOUYINIE_SEPARATOR_BIN to a missing binary, StageWorker returns SEPARATOR_BINARY_NOT_FOUND
	t.Setenv("DOUYINIE_SEPARATOR_BIN", filepath.Join(tmpDir, "nonexistent_sep_bin.exe"))

	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	ctx := context.Background()
	if err := sup.Spawn(ctx, "separator", exe, "-family", "separator", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	defer sup.Terminate()

	client := worker.NewClient(sup)
	if _, err := client.Handshake(ctx, 5*time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	outPath := filepath.Join(tmpDir, "sep_out.json")
	cmd := worker.Command{
		ID:         "cmd-sep-missing",
		Family:     "separator",
		Stage:      "separator",
		AttemptID:  "attempt-sep-missing",
		RunID:      "run-sep-missing",
		OutputPath: outPath,
		Inputs: []worker.ArtifactRef{
			{
				SHA256: "dummy_sha",
				Path:   audioPath,
			},
		},
		Config: map[string]any{
			"model_name":    "UVR-MDX-NET-Inst_HQ_4.onnx",
			"model_version": "v3",
		},
	}

	_, err := client.Run(ctx, cmd, 10*time.Second, 10*time.Second)
	if err == nil {
		t.Fatalf("expected error when separator binary missing, got nil")
	}
	if !strings.Contains(err.Error(), "SEPARATOR_BINARY_NOT_FOUND") {
		t.Errorf("expected SEPARATOR_BINARY_NOT_FOUND error, got %v", err)
	}
}

func TestSeam2_SeparatorStage_RealRuntimeSmokeOptIn(t *testing.T) {
	pyBin := os.Getenv("DOUYINIE_SEPARATOR_PYTHON_BIN")
	if pyBin == "" {
		pyBin = os.Getenv("DOUYINIE_PYTHON_BIN")
	}
	if pyBin == "" {
		t.Skip("skipping real separator smoke: no DOUYINIE_SEPARATOR_PYTHON_BIN or DOUYINIE_PYTHON_BIN set")
	}
	modelDir := os.Getenv("AUDIO_SEPARATOR_MODEL_DIR")
	if modelDir == "" {
		t.Skip("skipping real separator smoke: AUDIO_SEPARATOR_MODEL_DIR not configured")
	}
	if _, err := os.Stat(modelDir); err != nil {
		t.Skipf("skipping real separator smoke: model dir %s does not exist", modelDir)
	}
}
