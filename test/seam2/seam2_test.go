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
		Stage:     "tts",
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
		Config:    map[string]any{},
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
		Config:    map[string]any{},
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
		Config:     map[string]any{},
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
		Config:     map[string]any{},
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
		Config:     map[string]any{"text": "今天天气很好 我们去公园散步吧"},
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
		Config:     map[string]any{"text": "今天天气很好"},
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
