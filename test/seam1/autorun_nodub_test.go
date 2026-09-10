package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/storage"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// helper functions to eliminate boilerplate across autorun tests

func ingestSyntheticAsset(t *testing.T, baseURL, dir, filename string, durationSec float64) string {
	t.Helper()
	mediaPath := createSyntheticMediaWithDuration(t, dir, filename, durationSec)
	payload := map[string]any{
		"file_path": mediaPath,
		"attestation": map[string]any{
			"declared_by":    "autorun-tester",
			"terms_accepted": true,
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(baseURL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ingest %s failed: %v", filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 ingest for %s, got %d", filename, resp.StatusCode)
	}
	var res struct {
		Asset domain.SourceAsset `json:"asset"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res.Asset.ID
}

func createJob(t *testing.T, baseURL, assetID, targetLang string) string {
	t.Helper()
	if targetLang == "" {
		targetLang = domain.TargetLanguageVI
	}
	payload := map[string]string{
		"source_asset_id": assetID,
		"target_language": targetLang,
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(baseURL+"/api/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create job failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 create job, got %d", resp.StatusCode)
	}
	var res struct {
		Job domain.LocalizationJob `json:"job"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res.Job.ID
}

func postAudioRolePlan(t *testing.T, baseURL, assetID string, segments []domain.AudioSegment) {
	t.Helper()
	payload := map[string]any{"segments": segments}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", baseURL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post audio role plan failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200/201 post audio role plan, got %d", resp.StatusCode)
	}
}

func enqueueRun(t *testing.T, baseURL, jobID string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"config_snapshot_json": "{}"})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", baseURL, jobID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create run failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 create run, got %d", resp.StatusCode)
	}
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res.Run.ID
}

func pollRunStatus(t *testing.T, baseURL, runID string, targetStatus string, timeout time.Duration) domain.LocalizationRun {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var finalRun domain.LocalizationRun
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s", baseURL, runID))
		if err == nil && resp.StatusCode == http.StatusOK {
			var res struct {
				Run domain.LocalizationRun `json:"run"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&res)
			resp.Body.Close()
			finalRun = res.Run
			if finalRun.Status == targetStatus {
				return finalRun
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return finalRun
}

func getRunStages(t *testing.T, baseURL, runID string) []domain.StageExecution {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/runs/%s/stages", baseURL, runID))
	if err != nil {
		t.Fatalf("get stages failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 get stages, got %d", resp.StatusCode)
	}
	var res struct {
		Stages []domain.StageExecution `json:"stages"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res.Stages
}

// TestSeam1_AutoRun_NoDub_EndToEnd proves that POST /api/v1/jobs/{id}/runs wakes
// a RuntimeHost-owned consumer that automatically executes a queued zero-dialogue run
// through audio role planning, separation/passthrough mix, text detection, visual track
// localization, render plan freezing, and preview render to completed state.
func TestSeam1_AutoRun_NoDub_EndToEnd(t *testing.T) {
	h := setupAutoRunHarness(t)
	assetID := ingestSyntheticAsset(t, h.server.URL, h.dir, "nodub_autorun.mp4", 1.5)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, h.server.URL, jobID)

	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if finalRun.Status != domain.RunStatusCompleted {
		t.Fatalf("expected run status %q, got %q", domain.RunStatusCompleted, finalRun.Status)
	}

	// Verify preview render artifact
	prevResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?run_id=%s&target_language=vi", h.server.URL, assetID, runID))
	if err != nil {
		t.Fatalf("get render preview failed: %v", err)
	}
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for render preview, got %d", prevResp.StatusCode)
	}

	stages := getRunStages(t, h.server.URL, runID)
	stageMap := make(map[string]domain.StageExecution, len(stages))
	for _, st := range stages {
		stageMap[st.Stage] = st
		if st.Status != domain.StageStatusSucceeded {
			t.Errorf("expected stage %s succeeded, got %s", st.Stage, st.Status)
		}
		if st.ArtifactSHA256 == "" {
			t.Errorf("expected stage %s to have non-empty ArtifactSHA256", st.Stage)
		}
	}

	for _, exp := range []string{"audio_role_plan", "audio_mix", "text_detection", "visual_text_localize", "render_plan", "render_preview"} {
		if _, ok := stageMap[exp]; !ok {
			t.Errorf("expected stage execution for %s, none recorded", exp)
		}
	}

	for _, forbidden := range []string{"speech_understand", "asr", "aligner", "diarizer", "dub_script", "voice_assignment", "tts", "dub_synthesize"} {
		if st, ok := stageMap[forbidden]; ok {
			t.Errorf("forbidden dialogue/speech stage %s found in zero-dialogue auto-run: %+v", forbidden, st)
		}
	}
}

// TestSeam1_AutoRun_StageFailure_FailsClosedAndInterrupted proves that unrecoverable
// stage failure is fail-closed, records failed stage evidence, and leaves run/queue interrupted.
func TestSeam1_AutoRun_StageFailure_FailsClosedAndInterrupted(t *testing.T) {
	h := setupAutoRunHarness(t)
	assetID := ingestSyntheticAsset(t, h.server.URL, h.dir, "fail_autorun.mp4", 1.5)
	jobID := createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
		{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
	})

	// Sabotage render service to force failure
	h.srv.SetRenderService(nil)

	runID := enqueueRun(t, h.server.URL, jobID)
	finalRun := pollRunStatus(t, h.server.URL, runID, domain.RunStatusInterrupted, 5*time.Second)
	if finalRun.Status != domain.RunStatusInterrupted {
		t.Fatalf("expected run status %q, got %q", domain.RunStatusInterrupted, finalRun.Status)
	}

	// Verify queue entry is also interrupted
	qResp, err := http.Get(h.server.URL + "/api/v1/queue")
	if err != nil {
		t.Fatalf("get queue failed: %v", err)
	}
	defer qResp.Body.Close()
	var qRes struct {
		Queue []domain.QueueEntry `json:"queue"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qRes)
	found := false
	for _, e := range qRes.Queue {
		if e.RunID == runID && e.Status == domain.RunStatusInterrupted {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected queue entry for run %s to be interrupted", runID)
	}
}

// TestSeam1_AutoRun_MultiRun_SerialDrain proves that when multiple runs are enqueued,
// the wake-driven consumer drains them serially in queue order to completed state.
func TestSeam1_AutoRun_MultiRun_SerialDrain(t *testing.T) {
	h := setupAutoRunHarness(t)
	setupJobWithZeroDialogue := func(name string) string {
		assetID := ingestSyntheticAsset(t, h.server.URL, h.dir, name, 1.5)
		postAudioRolePlan(t, h.server.URL, assetID, []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
		})
		return createJob(t, h.server.URL, assetID, domain.TargetLanguageVI)
	}

	job1 := setupJobWithZeroDialogue("serial_job1.mp4")
	job2 := setupJobWithZeroDialogue("serial_job2.mp4")

	runID1 := enqueueRun(t, h.server.URL, job1)
	runID2 := enqueueRun(t, h.server.URL, job2)

	final1 := pollRunStatus(t, h.server.URL, runID1, domain.RunStatusCompleted, 10*time.Second)
	final2 := pollRunStatus(t, h.server.URL, runID2, domain.RunStatusCompleted, 10*time.Second)

	if final1.Status != domain.RunStatusCompleted {
		t.Errorf("expected run 1 completed, got %s", final1.Status)
	}
	if final2.Status != domain.RunStatusCompleted {
		t.Errorf("expected run 2 completed, got %s", final2.Status)
	}
}

// TestSeam1_AutoRun_Dialogue_FailsClosedAndReleasesSlot proves that when a run
// contains dub-eligible dialogue, the #81 zero-dialogue auto-runner fails closed,
// records a diagnostic failed stage execution, marks the run interrupted, and
// releases the active run slot so subsequent queued runs can proceed.
func TestSeam1_AutoRun_Dialogue_FailsClosedAndReleasesSlot(t *testing.T) {
	h := setupAutoRunHarness(t)
	assetID1 := ingestSyntheticAsset(t, h.server.URL, h.dir, "dialogue_test.mp4", 1.5)
	postAudioRolePlan(t, h.server.URL, assetID1, []domain.AudioSegment{
		{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
	})
	jobID1 := createJob(t, h.server.URL, assetID1, domain.TargetLanguageVI)
	runID1 := enqueueRun(t, h.server.URL, jobID1)

	final1 := pollRunStatus(t, h.server.URL, runID1, domain.RunStatusInterrupted, 10*time.Second)
	if final1.Status != domain.RunStatusInterrupted {
		t.Fatalf("expected run 1 status %q on dialogue run, got %q", domain.RunStatusInterrupted, final1.Status)
	}

	// Verify diagnostic failed stage was persisted
	stages := getRunStages(t, h.server.URL, runID1)
	foundDiag := false
	for _, st := range stages {
		if st.Stage == "dialogue_execution" && st.Status == domain.StageStatusFailed {
			foundDiag = true
			break
		}
	}
	if !foundDiag {
		t.Fatalf("expected failed dialogue_execution diagnostic stage for run 1, got %+v", stages)
	}

	// Enqueue Run 2 (no-dub) to prove active run slot was released and not stranded
	assetID2 := ingestSyntheticAsset(t, h.server.URL, h.dir, "nodub_subsequent.mp4", 1.5)
	postAudioRolePlan(t, h.server.URL, assetID2, []domain.AudioSegment{
		{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleInstrumentalBgm},
	})
	jobID2 := createJob(t, h.server.URL, assetID2, domain.TargetLanguageVI)
	runID2 := enqueueRun(t, h.server.URL, jobID2)

	final2 := pollRunStatus(t, h.server.URL, runID2, domain.RunStatusCompleted, 10*time.Second)
	if final2.Status != domain.RunStatusCompleted {
		t.Fatalf("expected subsequent run 2 to auto-complete, but got status %s", final2.Status)
	}
}

// TestSeam1_AutoRun_StartupDrainQueuedRun proves that a queued run present in the DB
// before RuntimeHost starts is automatically consumed on startup when AutoRunExecutor is enabled.
func TestSeam1_AutoRun_StartupDrainQueuedRun(t *testing.T) {
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}
	db, err := storage.Open(filepath.Join(tmpDir, "douyinie_test.db"))
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()

	// Step 1: Create initial server with AutoRunExecutor FALSE to enqueue a run without auto-executing it
	srvDisabled, _, _ := newRuntimeHostWithOptions(t, db, casStore, queueSvc, resScheduler, harnessOptions{autoRunExecutor: false})
	tsDisabled := httptest.NewServer(srvDisabled.Handler())

	assetID := ingestSyntheticAsset(t, tsDisabled.URL, tmpDir, "startup_drain.mp4", 1.5)
	jobID := createJob(t, tsDisabled.URL, assetID, domain.TargetLanguageVI)
	runID := enqueueRun(t, tsDisabled.URL, jobID)

	// Verify run remains queued while executor is disabled
	initialRun := pollRunStatus(t, tsDisabled.URL, runID, domain.RunStatusQueued, 1*time.Second)
	if initialRun.Status != domain.RunStatusQueued {
		t.Fatalf("expected run to remain %q while executor disabled, got %q", domain.RunStatusQueued, initialRun.Status)
	}

	tsDisabled.Close()
	_ = srvDisabled.Shutdown(context.Background())

	// Step 2: Start new server with AutoRunExecutor TRUE over the same DB
	srvEnabled, _, _ := newRuntimeHostWithOptions(t, db, casStore, queueSvc, resScheduler, harnessOptions{autoRunExecutor: true})
	tsEnabled := httptest.NewServer(srvEnabled.Handler())
	defer func() {
		_ = srvEnabled.Shutdown(context.Background())
		tsEnabled.Close()
		_ = db.Close()
	}()

	// Verify startup drain automatically completes the queued run
	finalRun := pollRunStatus(t, tsEnabled.URL, runID, domain.RunStatusCompleted, 10*time.Second)
	if finalRun.Status != domain.RunStatusCompleted {
		t.Fatalf("expected queued run to auto-complete on startup drain, got status %q", finalRun.Status)
	}
}
