package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/storage"
)

// createJobAndRun ingests synthetic media and creates a job + run, returning IDs.
func createJobAndRun(t *testing.T, h *testHarness) (jobID, runID string) {
	return createJobAndRunWithDuration(t, h, 1.5)
}

// createJobAndRunWithDuration ingests synthetic media of specified duration and creates a job + run, returning IDs.
func createJobAndRunWithDuration(t *testing.T, h *testHarness, durationSec float64) (jobID, runID string) {
	t.Helper()
	mediaPath := createSyntheticMediaWithDuration(t, h.dir, "queue_source.mp4", durationSec)
	ingestPayload := map[string]any{
		"file_path": mediaPath,
		"attestation": map[string]any{
			"declared_by":    "queue-tester",
			"terms_accepted": true,
		},
	}
	ingestBody, _ := json.Marshal(ingestPayload)
	ingestResp, err := http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(ingestBody))
	if err != nil {
		t.Fatalf("ingest failed: %v", err)
	}
	defer ingestResp.Body.Close()
	if ingestResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 ingest, got %d", ingestResp.StatusCode)
	}
	var ingestRes struct {
		Asset domain.SourceAsset `json:"asset"`
	}
	_ = json.NewDecoder(ingestResp.Body).Decode(&ingestRes)

	jobPayload := map[string]string{
		"source_asset_id": ingestRes.Asset.ID,
		"target_language": domain.TargetLanguageVI,
	}
	jobBody, _ := json.Marshal(jobPayload)
	jobResp, err := http.Post(h.server.URL+"/api/v1/jobs", "application/json", bytes.NewReader(jobBody))
	if err != nil {
		t.Fatalf("create job failed: %v", err)
	}
	defer jobResp.Body.Close()
	if jobResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 job, got %d", jobResp.StatusCode)
	}
	var jobResult struct {
		Job domain.LocalizationJob `json:"job"`
	}
	_ = json.NewDecoder(jobResp.Body).Decode(&jobResult)

	runBody, _ := json.Marshal(map[string]string{"config_snapshot_json": "{}"})
	runResp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobResult.Job.ID), "application/json", bytes.NewReader(runBody))
	if err != nil {
		t.Fatalf("create run failed: %v", err)
	}
	defer runResp.Body.Close()
	if runResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 run, got %d", runResp.StatusCode)
	}
	var runResult struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(runResp.Body).Decode(&runResult)

	return jobResult.Job.ID, runResult.Run.ID
}

// TestSeam1_QueuePersistsAcrossRestart proves Seam 1 acceptance: queue state survives
// restart with correct crash-recovery semantics. A run that was actively running is
// interrupted on restart; a queued never-started run survives restart as queued.
// The surviving state is observed through a reconstructed Seam-1 RuntimeHost and its
// public HTTP API (GET /api/v1/queue, GET /api/v1/runs/{id}), not only direct reads.
func TestSeam1_QueuePersistsAcrossRestart(t *testing.T) {
	h := setupHarness(t)
	dbPath := filepath.Join(h.dir, "douyinie_test.db")

	_, runA := createJobAndRun(t, h)
	_, runB := createJobAndRun(t, h)

	// Run A starts executing; run B stays queued.
	if err := h.queueSvc.MarkRunning(context.Background(), runA); err != nil {
		t.Fatalf("mark run A running: %v", err)
	}

	// Simulate daemon crash: close the DB, reopen the same file.
	if err := h.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	reopened, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer reopened.Close()

	// Recover persisted queue/runtime state, then reconstruct a fresh RuntimeHost over it.
	recovered, err := queue.NewService(reopened).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("expected 1 interrupted run (the running one), got %d: %v", len(recovered), recovered)
	}
	if recovered[0] != runA {
		t.Errorf("expected interrupted run %s, got %v", runA, recovered)
	}

	casStore, err := cas.NewStore(h.dir)
	if err != nil {
		t.Fatalf("reopen cas store: %v", err)
	}
	restartQueue := queue.NewService(reopened)
	restartScheduler := scheduler.New()
	restartSrv, _, _ := newRuntimeHost(t, reopened, casStore, restartQueue, restartScheduler)
	restartTS := httptest.NewServer(restartSrv.Handler())
	defer restartTS.Close()

	// Observe the surviving queue through the public HTTP API of the restarted host.
	queueResp, err := http.Get(restartTS.URL + "/api/v1/queue")
	if err != nil {
		t.Fatalf("GET queue after restart failed: %v", err)
	}
	defer queueResp.Body.Close()
	if queueResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 queue after restart, got %d", queueResp.StatusCode)
	}
	var queueResult struct {
		Queue []domain.QueueEntry `json:"queue"`
	}
	if err := json.NewDecoder(queueResp.Body).Decode(&queueResult); err != nil {
		t.Fatalf("decode queue after restart: %v", err)
	}
	if len(queueResult.Queue) != 2 {
		t.Fatalf("expected 2 queue entries after restart, got %d", len(queueResult.Queue))
	}
	for _, e := range queueResult.Queue {
		if e.RunID == runA && e.Status != domain.RunStatusInterrupted {
			t.Errorf("expected running run A interrupted after crash, got %s", e.Status)
		}
		if e.RunID == runB && e.Status != domain.RunStatusQueued {
			t.Errorf("expected queued run B to survive restart as queued, got %s", e.Status)
		}
	}

	// GET /api/v1/runs/{id} on the restarted host confirms each run's persisted status.
	for _, want := range []struct {
		runID  string
		status string
	}{{runA, domain.RunStatusInterrupted}, {runB, domain.RunStatusQueued}} {
		runResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s", restartTS.URL, want.runID))
		if err != nil {
			t.Fatalf("GET run %s after restart failed: %v", want.runID, err)
		}
		var runResult struct {
			Run domain.LocalizationRun `json:"run"`
		}
		decodeErr := json.NewDecoder(runResp.Body).Decode(&runResult)
		runResp.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode run %s after restart: %v", want.runID, decodeErr)
		}
		if runResult.Run.Status != want.status {
			t.Errorf("run %s via API after restart: expected status %s, got %s", want.runID, want.status, runResult.Run.Status)
		}
	}
}

// TestSeam1_ActiveRunSlots_PersistedEnforcement proves active_run_slots=1 is enforced in
// persisted queue/runtime state, not only by the in-memory scheduler lease: a second run
// cannot become running (ErrSlotBusy), and a non-queued run cannot be marked running.
func TestSeam1_ActiveRunSlots_PersistedEnforcement(t *testing.T) {
	h := setupHarness(t)
	_, runA := createJobAndRun(t, h)
	_, runB := createJobAndRun(t, h)

	ctx := context.Background()
	if err := h.queueSvc.MarkRunning(ctx, runA); err != nil {
		t.Fatalf("mark run A running: %v", err)
	}

	// A second run cannot become running while run A holds the slot.
	if err := h.queueSvc.MarkRunning(ctx, runB); !errors.Is(err, queue.ErrSlotBusy) {
		t.Fatalf("expected ErrSlotBusy for second running run, got %v", err)
	}

	// Marking an already-running run running again is also rejected (not queued).
	if err := h.queueSvc.MarkRunning(ctx, runA); !errors.Is(err, queue.ErrNotQueued) {
		t.Fatalf("expected ErrNotQueued for re-mark of running run, got %v", err)
	}

	// After run A is cancelled (terminal), run B can take the slot.
	if err := h.queueSvc.Cancel(ctx, runA); err != nil {
		t.Fatalf("cancel run A: %v", err)
	}
	if err := h.queueSvc.MarkRunning(ctx, runB); err != nil {
		t.Fatalf("mark run B running after A terminal: %v", err)
	}
}

// TestSeam1_ActiveRunSlots_SecondJobStaysQueued proves active_run_slots=1:
// after a lease is granted for run A's family, a second acquire is rejected and run B stays queued.
func TestSeam1_ActiveRunSlots_SecondJobStaysQueued(t *testing.T) {
	h := setupHarness(t)
	_, runA := createJobAndRun(t, h)
	_, runB := createJobAndRun(t, h)

	// Grant the single GPU lease to run A's worker family.
	acquirePayload := map[string]string{"family": "family_asr"}
	acquireBody, _ := json.Marshal(acquirePayload)
	acquireResp, err := http.Post(h.server.URL+"/api/v1/scheduler/acquire", "application/json", bytes.NewReader(acquireBody))
	if err != nil {
		t.Fatalf("acquire lease failed: %v", err)
	}
	defer acquireResp.Body.Close()
	if acquireResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 acquire, got %d", acquireResp.StatusCode)
	}
	var acquireRes struct {
		LeaseID string               `json:"lease_id"`
		Profile scheduler.GPUProfile `json:"profile"`
	}
	_ = json.NewDecoder(acquireResp.Body).Decode(&acquireRes)
	if acquireRes.LeaseID == "" {
		t.Fatalf("expected non-empty lease_id")
	}
	if acquireRes.Profile.MemoryGB != 8 || acquireRes.Profile.ActiveSlots != 1 {
		t.Errorf("unexpected GPU profile: %+v", acquireRes.Profile)
	}

	// A second acquire for run B's family must be rejected (409).
	acquirePayload2 := map[string]string{"family": "family_tts"}
	acquireBody2, _ := json.Marshal(acquirePayload2)
	acquireResp2, err := http.Post(h.server.URL+"/api/v1/scheduler/acquire", "application/json", bytes.NewReader(acquireBody2))
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}
	defer acquireResp2.Body.Close()
	if acquireResp2.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict for second lease, got %d", acquireResp2.StatusCode)
	}

	// Run A proceeds (running), run B remains queued — active_run_slots=1 invariant.
	if err := h.queueSvc.MarkRunning(context.Background(), runA); err != nil {
		t.Fatalf("mark run A running: %v", err)
	}
	runBEntry, err := h.queueSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	for _, e := range runBEntry {
		if e.RunID == runA && e.Status != domain.RunStatusRunning {
			t.Errorf("run A should be running, got %s", e.Status)
		}
		if e.RunID == runB && e.Status != domain.RunStatusQueued {
			t.Errorf("run B must stay queued (active_run_slots=1), got %s", e.Status)
		}
	}

	// Release the lease; a new acquire then succeeds.
	releaseBody, _ := json.Marshal(map[string]string{"lease_id": acquireRes.LeaseID})
	releaseResp, err := http.Post(h.server.URL+"/api/v1/scheduler/release", "application/json", bytes.NewReader(releaseBody))
	if err != nil {
		t.Fatalf("release lease failed: %v", err)
	}
	defer releaseResp.Body.Close()
	if releaseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 release, got %d", releaseResp.StatusCode)
	}

	acquireResp3, err := http.Post(h.server.URL+"/api/v1/scheduler/acquire", "application/json", bytes.NewReader(acquireBody2))
	if err != nil {
		t.Fatalf("re-acquire after release failed: %v", err)
	}
	defer acquireResp3.Body.Close()
	if acquireResp3.StatusCode != http.StatusOK {
		t.Errorf("expected 200 re-acquire after release, got %d", acquireResp3.StatusCode)
	}
}

// TestSeam1_ReorderPauseCancelResume proves queue reorder/pause/cancel/resume persist.
func TestSeam1_ReorderPauseCancelResume(t *testing.T) {
	h := setupHarness(t)
	_, runA := createJobAndRun(t, h)
	_, runB := createJobAndRun(t, h)
	_, runC := createJobAndRun(t, h)

	// Reorder: move run C from position 3 to position 1.
	reorderPayload := map[string]any{"run_id": runC, "position": 1}
	reorderBody, _ := json.Marshal(reorderPayload)
	reorderResp, err := http.Post(h.server.URL+"/api/v1/queue/reorder", "application/json", bytes.NewReader(reorderBody))
	if err != nil {
		t.Fatalf("reorder failed: %v", err)
	}
	defer reorderResp.Body.Close()
	if reorderResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reorder, got %d", reorderResp.StatusCode)
	}

	entries, err := h.queueSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	if entries[0].RunID != runC || entries[1].RunID != runA || entries[2].RunID != runB {
		t.Errorf("unexpected order after reorder: %v", entries)
	}
	if entries[0].Position != 1 || entries[1].Position != 2 || entries[2].Position != 3 {
		t.Errorf("positions not resequenced: %v", entries)
	}

	// Pause run B (currently position 3).
	pauseResp, err := http.Post(h.server.URL+"/api/v1/runs/"+runB+"/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("pause failed: %v", err)
	}
	defer pauseResp.Body.Close()
	if pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 pause, got %d", pauseResp.StatusCode)
	}
	bEntry, err := h.queueSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	foundPaused := false
	for _, e := range bEntry {
		if e.RunID == runB {
			foundPaused = e.Status == domain.RunStatusPaused
		}
	}
	if !foundPaused {
		t.Errorf("expected run B paused, got %+v", bEntry)
	}

	// Resume run B back to queued.
	resumeResp, err := http.Post(h.server.URL+"/api/v1/runs/"+runB+"/resume", "application/json", nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	defer resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 resume, got %d", resumeResp.StatusCode)
	}

	// Cancel run A.
	cancelResp, err := http.Post(h.server.URL+"/api/v1/runs/"+runA+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 cancel, got %d", cancelResp.StatusCode)
	}
	aEntry, err := h.queueSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	foundCancelled := false
	for _, e := range aEntry {
		if e.RunID == runA {
			foundCancelled = e.Status == domain.RunStatusCancelled
		}
	}
	if !foundCancelled {
		t.Errorf("expected run A cancelled, got %+v", aEntry)
	}

	// Cancelling an already-cancelled run must fail (409).
	cancelResp2, err := http.Post(h.server.URL+"/api/v1/runs/"+runA+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("second cancel failed: %v", err)
	}
	defer cancelResp2.Body.Close()
	if cancelResp2.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 on re-cancel of terminal run, got %d", cancelResp2.StatusCode)
	}
}

// TestSeam1_CommittedArtifactsSurviveRestart proves committed CAS artifacts and
// stage execution state survive restart (crash recovery preserves committed output).
func TestSeam1_CommittedArtifactsSurviveRestart(t *testing.T) {
	h := setupHarness(t)
	dbPath := filepath.Join(h.dir, "douyinie_test.db")

	_, runID := createJobAndRun(t, h)

	// Commit an artifact into CAS.
	obj, err := h.casStore.Put(bytes.NewReader([]byte("committed-render-artifact-bytes")))
	if err != nil {
		t.Fatalf("CAS put: %v", err)
	}

	// Record a succeeded stage execution referencing the committed artifact.
	now := time.Now().UTC()
	se := domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "render",
		Status:         domain.StageStatusSucceeded,
		StartedAt:      &now,
		CompletedAt:    &now,
		ArtifactSHA256: obj.SHA256,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := h.db.CreateStageExecution(context.Background(), se); err != nil {
		t.Fatalf("create stage execution: %v", err)
	}

	// Simulate crash: close DB, reopen same file.
	if err := h.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	reopened, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer reopened.Close()

	// Committed artifact survives restart.
	if _, err := reopened.GetRun(context.Background(), runID); err != nil {
		t.Fatalf("run row lost after restart: %v", err)
	}
	if !h.casStore.Exists(obj.SHA256) {
		t.Fatalf("committed CAS artifact lost after restart: %s", obj.SHA256)
	}
	if err := h.casStore.VerifyIntegrity(obj.SHA256); err != nil {
		t.Fatalf("committed CAS artifact integrity failed after restart: %v", err)
	}

	// Stage execution state survives restart with committed artifact reference intact.
	stages, err := reopened.ListStageExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stage executions after restart: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage execution after restart, got %d", len(stages))
	}
	if stages[0].Status != domain.StageStatusSucceeded || stages[0].ArtifactSHA256 != obj.SHA256 {
		t.Errorf("stage execution state lost after restart: %+v", stages[0])
	}

	// RunStateSnapshot reconstructs the crash-recovery projection.
	snap, err := reopened.RunStateSnapshot(context.Background(), runID)
	if err != nil {
		t.Fatalf("run state snapshot: %v", err)
	}
	if snap.ID != runID || len(snap.StageExecutions) != 1 {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
}

// TestSeam1_StageExecutionStatesPersist proves stage execution CRUD via the HTTP API.
func TestSeam1_StageExecutionStatesPersist(t *testing.T) {
	h := setupHarness(t)
	_, runID := createJobAndRun(t, h)

	// Create stage executions directly through the queue service + storage.
	now := time.Now().UTC()
	seASR := domain.StageExecution{
		ID:        uuid.NewString(),
		RunID:     runID,
		Stage:     "asr",
		Status:    domain.StageStatusRunning,
		StartedAt: &now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.db.CreateStageExecution(context.Background(), seASR); err != nil {
		t.Fatalf("create asr stage: %v", err)
	}

	// Update to succeeded with an artifact reference.
	seASR.Status = domain.StageStatusSucceeded
	seASR.CompletedAt = &now
	seASR.ArtifactSHA256 = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	seASR.UpdatedAt = time.Now().UTC()
	if err := h.db.UpdateStageExecution(context.Background(), seASR); err != nil {
		t.Fatalf("update stage execution: %v", err)
	}

	// GET /api/v1/runs/{id}/stages returns the persisted stage.
	stagesResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/stages", h.server.URL, runID))
	if err != nil {
		t.Fatalf("GET stages failed: %v", err)
	}
	defer stagesResp.Body.Close()
	if stagesResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 stages, got %d", stagesResp.StatusCode)
	}
	var stagesResult struct {
		Stages []domain.StageExecution `json:"stages"`
	}
	_ = json.NewDecoder(stagesResp.Body).Decode(&stagesResult)
	if len(stagesResult.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stagesResult.Stages))
	}
	if stagesResult.Stages[0].Stage != "asr" || stagesResult.Stages[0].Status != domain.StageStatusSucceeded {
		t.Errorf("unexpected stage state: %+v", stagesResult.Stages[0])
	}
	if stagesResult.Stages[0].ArtifactSHA256 == "" {
		t.Errorf("expected committed artifact sha256 persisted, got empty")
	}
}

// TestSeam1_StageExecutionActiveStates_AlignWithQueue proves the StageExecution active
// states QUEUED/RUNNING/CANCELLING persist via storage and are visible through the public
// HTTP API, replacing the former pending-as-queued conflation. Active stages never take a
// terminal (succeeded/failed/interrupted) label until they actually terminate.
func TestSeam1_StageExecutionActiveStates_AlignWithQueue(t *testing.T) {
	h := setupHarness(t)
	_, runID := createJobAndRun(t, h)

	now := time.Now().UTC()
	stages := []domain.StageExecution{
		{ID: uuid.NewString(), RunID: runID, Stage: "asr", Status: domain.StageStatusQueued, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), RunID: runID, Stage: "aligner", Status: domain.StageStatusRunning, StartedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), RunID: runID, Stage: "tts", Status: domain.StageStatusCancelling, StartedAt: &now, CreatedAt: now, UpdatedAt: now},
	}
	for _, se := range stages {
		if err := h.db.CreateStageExecution(context.Background(), se); err != nil {
			t.Fatalf("create stage %s: %v", se.Stage, err)
		}
	}

	// The three active states must round-trip through the public API untouched.
	stagesResp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/stages", h.server.URL, runID))
	if err != nil {
		t.Fatalf("GET stages failed: %v", err)
	}
	defer stagesResp.Body.Close()
	var stagesResult struct {
		Stages []domain.StageExecution `json:"stages"`
	}
	if err := json.NewDecoder(stagesResp.Body).Decode(&stagesResult); err != nil {
		t.Fatalf("decode stages: %v", err)
	}
	if len(stagesResult.Stages) != 3 {
		t.Fatalf("expected 3 stage executions, got %d", len(stagesResult.Stages))
	}
	got := map[string]string{}
	for _, se := range stagesResult.Stages {
		got[se.Stage] = se.Status
	}
	if got["asr"] != domain.StageStatusQueued || got["aligner"] != domain.StageStatusRunning || got["tts"] != domain.StageStatusCancelling {
		t.Errorf("stage active states not preserved: got %v", got)
	}
	if !domain.StageStatusActive(got["asr"]) || !domain.StageStatusActive(got["aligner"]) || !domain.StageStatusActive(got["tts"]) {
		t.Errorf("all three stage states must be active (non-terminal), got %v", got)
	}
}

// TestSeam1_StageCrashRecovery_ActiveVsTerminal proves crash recovery aligns with the
// QUEUED/RUNNING/CANCELLING lifecycle: only actively-executing stages (running/cancelling)
// of an interrupted run transition to interrupted; queued not-yet-started stages survive
// as queued; terminal succeeded/failed outcomes are left untouched.
func TestSeam1_StageCrashRecovery_ActiveVsTerminal(t *testing.T) {
	h := setupHarness(t)
	dbPath := filepath.Join(h.dir, "douyinie_test.db")
	_, runID := createJobAndRun(t, h)

	// Put the run in running state so crash recovery processes its stages.
	if err := h.queueSvc.MarkRunning(context.Background(), runID); err != nil {
		t.Fatalf("mark run running: %v", err)
	}

	now := time.Now().UTC()
	stages := []domain.StageExecution{
		{ID: uuid.NewString(), RunID: runID, Stage: "asr", Status: domain.StageStatusQueued, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), RunID: runID, Stage: "aligner", Status: domain.StageStatusRunning, StartedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), RunID: runID, Stage: "tts", Status: domain.StageStatusCancelling, StartedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), RunID: runID, Stage: "render", Status: domain.StageStatusSucceeded, CompletedAt: &now, ArtifactSHA256: "aaa", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.NewString(), RunID: runID, Stage: "ocr", Status: domain.StageStatusFailed, ErrorMessage: "boom", CreatedAt: now, UpdatedAt: now},
	}
	for _, se := range stages {
		if err := h.db.CreateStageExecution(context.Background(), se); err != nil {
			t.Fatalf("create stage %s: %v", se.Stage, err)
		}
	}

	// Simulate crash: close DB, reopen, run recovery.
	if err := h.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	reopened, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer reopened.Close()
	recovered, err := queue.NewService(reopened).Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0] != runID {
		t.Fatalf("expected run %s interrupted, got %v", runID, recovered)
	}

	stagesAfter, err := reopened.ListStageExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stages after recovery: %v", err)
	}
	got := map[string]string{}
	for _, se := range stagesAfter {
		got[se.Stage] = se.Status
	}
	want := map[string]string{
		"asr":     domain.StageStatusQueued,      // never started -> stays queued
		"aligner": domain.StageStatusInterrupted, // actively running -> interrupted
		"tts":     domain.StageStatusInterrupted, // cancelling mid-flight -> interrupted
		"render":  domain.StageStatusSucceeded,   // committed terminal outcome untouched
		"ocr":     domain.StageStatusFailed,      // failed terminal outcome untouched
	}
	for stage, wantStatus := range want {
		if got[stage] != wantStatus {
			t.Errorf("stage %s after crash recovery: expected %s, got %s", stage, wantStatus, got[stage])
		}
	}
}

// TestSeam1_StageAlignment_PauseAndCancel proves a run that stops actively executing never
// leaves its stages in the active set (locked #13/#16). Pausing a running run returns its
// running/cancelling stages to queued; cancelling terminalizes any running/cancelling stages
// to interrupted. Terminal stage outcomes are untouched in both cases.
func TestSeam1_StageAlignment_PauseAndCancel(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	now := time.Now().UTC()
	mkStages := func(runID string) map[string]domain.StageExecution {
		return map[string]domain.StageExecution{
			"asr":     {ID: uuid.NewString(), RunID: runID, Stage: "asr", Status: domain.StageStatusQueued, CreatedAt: now, UpdatedAt: now},
			"aligner": {ID: uuid.NewString(), RunID: runID, Stage: "aligner", Status: domain.StageStatusRunning, StartedAt: &now, CreatedAt: now, UpdatedAt: now},
			"tts":     {ID: uuid.NewString(), RunID: runID, Stage: "tts", Status: domain.StageStatusCancelling, StartedAt: &now, CreatedAt: now, UpdatedAt: now},
			"render":  {ID: uuid.NewString(), RunID: runID, Stage: "render", Status: domain.StageStatusSucceeded, CompletedAt: &now, CreatedAt: now, UpdatedAt: now},
		}
	}
	seed := func(runID string, stages map[string]domain.StageExecution) {
		t.Helper()
		for _, se := range stages {
			if err := h.db.CreateStageExecution(ctx, se); err != nil {
				t.Fatalf("create stage %s: %v", se.Stage, err)
			}
		}
	}
	assertStages := func(runID, label string, want map[string]string) {
		t.Helper()
		stages, err := h.db.ListStageExecutions(ctx, runID)
		if err != nil {
			t.Fatalf("list stages (%s): %v", label, err)
		}
		got := map[string]string{}
		for _, se := range stages {
			got[se.Stage] = se.Status
		}
		for stage, wantStatus := range want {
			if got[stage] != wantStatus {
				t.Errorf("%s: stage %s: expected %s, got %s", label, stage, wantStatus, got[stage])
			}
		}
	}

	// Scenario 1: pause a running run. Active stages return to queued; succeeded stays terminal.
	_, pauseRun := createJobAndRun(t, h)
	if err := h.queueSvc.MarkRunning(ctx, pauseRun); err != nil {
		t.Fatalf("mark pause-run running: %v", err)
	}
	seed(pauseRun, mkStages(pauseRun))
	if err := h.queueSvc.Pause(ctx, pauseRun); err != nil {
		t.Fatalf("pause: %v", err)
	}
	assertStages(pauseRun, "after pause", map[string]string{
		"asr":     domain.StageStatusQueued,    // never started
		"aligner": domain.StageStatusQueued,    // running -> queued on pause
		"tts":     domain.StageStatusQueued,    // cancelling -> queued on pause
		"render":  domain.StageStatusSucceeded, // terminal untouched
	})

	// Scenario 2: cancel a running run. Running/cancelling stages terminalize to interrupted;
	// never-started queued stages stay queued; terminal outcomes stay terminal.
	_, cancelRun := createJobAndRun(t, h)
	if err := h.queueSvc.MarkRunning(ctx, cancelRun); err != nil {
		t.Fatalf("mark cancel-run running: %v", err)
	}
	seed(cancelRun, mkStages(cancelRun))
	if err := h.queueSvc.Cancel(ctx, cancelRun); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	assertStages(cancelRun, "after cancel", map[string]string{
		"asr":     domain.StageStatusQueued,      // never started, still queued
		"aligner": domain.StageStatusInterrupted, // running -> interrupted on cancel
		"tts":     domain.StageStatusInterrupted, // cancelling -> interrupted on cancel
		"render":  domain.StageStatusSucceeded,   // terminal untouched
	})
}

// TestSeam1_Reorder_WithTerminalEntryInQueue reproduces the reorder collision bug:
// a cancelled/completed entry must release its position so active entries keep an
// unbroken 1..N position space and reorder never hits a UNIQUE(position) conflict.
func TestSeam1_Reorder_WithTerminalEntryInQueue(t *testing.T) {
	h := setupHarness(t)
	_, runA := createJobAndRun(t, h)
	_, runB := createJobAndRun(t, h)
	_, runC := createJobAndRun(t, h)

	// Cancel run B (middle entry, position 2). Its position must be released (NULL).
	cancelResp, err := http.Post(h.server.URL+"/api/v1/runs/"+runB+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("cancel run B failed: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 cancel, got %d", cancelResp.StatusCode)
	}

	entries, err := h.queueSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 queue entries, got %d", len(entries))
	}
	for _, e := range entries {
		if e.RunID == runB {
			if e.Status != domain.RunStatusCancelled || e.Position != 0 {
				t.Errorf("cancelled entry should have status=cancelled and released position (0), got %+v", e)
			}
		} else if e.Position == 0 {
			t.Errorf("active entry %s lost its position: %+v", e.RunID, e)
		}
	}

	// Move run C (position 3) to position 1. With B's position released, no UNIQUE collision.
	reorderBody, _ := json.Marshal(map[string]any{"run_id": runC, "position": 1})
	reorderResp, err := http.Post(h.server.URL+"/api/v1/queue/reorder", "application/json", bytes.NewReader(reorderBody))
	if err != nil {
		t.Fatalf("reorder failed: %v", err)
	}
	defer reorderResp.Body.Close()
	if reorderResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reorder with terminal entry in queue, got %d", reorderResp.StatusCode)
	}

	after, err := h.queueSvc.List(context.Background())
	if err != nil {
		t.Fatalf("list after reorder: %v", err)
	}
	// Active entries must be compacted to positions 1..2 in order C, A.
	var active []domain.QueueEntry
	for _, e := range after {
		if e.Status != domain.RunStatusCancelled {
			active = append(active, e)
		}
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active entries, got %d", len(active))
	}
	if active[0].RunID != runC || active[1].RunID != runA {
		t.Errorf("unexpected active order after reorder: %+v", active)
	}
	if active[0].Position != 1 || active[1].Position != 2 {
		t.Errorf("active positions not compacted to 1..2: %+v", active)
	}
}

// TestSeam1_RunCompletedAt_PreservedOnNonTerminalTransition verifies that a
// non-terminal status change never clobbers a previously set completed_at.
func TestSeam1_RunCompletedAt_PreservedOnNonTerminalTransition(t *testing.T) {
	h := setupHarness(t)
	_, runID := createJobAndRun(t, h)
	ctx := context.Background()

	// Complete the run (sets completed_at).
	if err := h.queueSvc.Pause(ctx, runID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Complete via storage directly (simulate a completed terminal transition).
	if err := h.db.UpdateQueueStatus(ctx, runID, domain.RunStatusCompleted, domain.RunStatusCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	run, err := h.db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.CompletedAt == nil {
		t.Fatalf("expected completed_at set after completion")
	}

	// Resume attempt on a completed run is rejected by the queue service, but directly
	// exercise the storage path: a paused transition must not wipe completed_at.
	_ = h.db.UpdateQueueStatus(ctx, runID, domain.RunStatusPaused, domain.RunStatusPaused)

	run2, err := h.db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run 2: %v", err)
	}
	if run2.CompletedAt == nil {
		t.Fatalf("completed_at was clobbered by non-terminal transition")
	}
}

// TestSeam1_QueueEndpoints_HTTPSurface proves the public HTTP queue/run surface.
func TestSeam1_QueueEndpoints_HTTPSurface(t *testing.T) {
	h := setupHarness(t)
	_, runA := createJobAndRun(t, h)
	_, runB := createJobAndRun(t, h)

	// GET /api/v1/queue lists both entries ordered.
	queueResp, err := http.Get(h.server.URL + "/api/v1/queue")
	if err != nil {
		t.Fatalf("GET queue failed: %v", err)
	}
	defer queueResp.Body.Close()
	if queueResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 queue, got %d", queueResp.StatusCode)
	}
	var queueResult struct {
		Queue []domain.QueueEntry `json:"queue"`
	}
	_ = json.NewDecoder(queueResp.Body).Decode(&queueResult)
	if len(queueResult.Queue) != 2 {
		t.Fatalf("expected 2 queue entries, got %d", len(queueResult.Queue))
	}
	if queueResult.Queue[0].RunID != runA || queueResult.Queue[1].RunID != runB {
		t.Errorf("unexpected queue order: %+v", queueResult.Queue)
	}

	// GET /api/v1/scheduler/lease reports idle initially.
	leaseResp, err := http.Get(h.server.URL + "/api/v1/scheduler/lease")
	if err != nil {
		t.Fatalf("GET lease failed: %v", err)
	}
	defer leaseResp.Body.Close()
	if leaseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 lease, got %d", leaseResp.StatusCode)
	}
	var leaseResult struct {
		Holder  string               `json:"holder"`
		Profile scheduler.GPUProfile `json:"profile"`
	}
	_ = json.NewDecoder(leaseResp.Body).Decode(&leaseResult)
	if leaseResult.Holder != "" {
		t.Errorf("expected idle holder, got %q", leaseResult.Holder)
	}
	if leaseResult.Profile.ActiveSlots != 1 || leaseResult.Profile.MemoryGB != 8 {
		t.Errorf("unexpected profile: %+v", leaseResult.Profile)
	}
}

// TestSeam1_SchedulerRejectsBusyLease_Unit proves ErrLeaseBusy at the service level.
func TestSeam1_SchedulerRejectsBusyLease_Unit(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	lease1, err := h.scheduler.Acquire(ctx, "family_asr")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := h.scheduler.Acquire(ctx, "family_tts"); err != scheduler.ErrLeaseBusy {
		t.Fatalf("expected ErrLeaseBusy for second acquire, got %v", err)
	}
	if h.scheduler.Holder() != "family_asr" {
		t.Errorf("expected holder family_asr, got %s", h.scheduler.Holder())
	}

	// Stale/duplicate release is a no-op.
	if err := h.scheduler.Release(ctx, "nonexistent-lease"); err != nil {
		t.Fatalf("stale release should be no-op, got %v", err)
	}
	if h.scheduler.Holder() != "family_asr" {
		t.Errorf("holder should be unchanged after stale release, got %s", h.scheduler.Holder())
	}

	if err := h.scheduler.Release(ctx, lease1); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h.scheduler.Holder() != "" {
		t.Errorf("expected idle after release, got %s", h.scheduler.Holder())
	}

	// Re-acquire succeeds after release.
	lease2, err := h.scheduler.Acquire(ctx, "family_tts")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	_ = lease2
}
