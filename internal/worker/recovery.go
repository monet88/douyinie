package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrNoRecoveryFile is returned when no crash recovery marker exists.
var ErrNoRecoveryFile = errors.New("no recovery marker")

// AttemptRecord is the durable crash-recovery record for one lost worker
// attempt. A record is committed before the worker starts and marked complete
// only after RuntimeHost has verified the output artifact.
type AttemptRecord struct {
	ID             string    `json:"id"`
	RunID          string    `json:"run_id"`
	Stage          string    `json:"stage"`
	Family         string    `json:"family"`
	Status         string    `json:"status"`
	ArtifactSHA256 string    `json:"artifact_sha256,omitempty"`
	ErrorMessage   string    `json:"error_message,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RecoveryStore persists StageWorker attempt state outside SQLite so a lost
// worker process or daemon crash can be reconciled deterministically. It also
// maintains an artifact manifest proving committed artifacts survive even when
// the attempt itself is interrupted.
type RecoveryStore struct {
	dir string
	mu  sync.Mutex
}

// NewRecoveryStore creates a recovery store under the RuntimeHost data dir.
func NewRecoveryStore(dataDir string) (*RecoveryStore, error) {
	dir := filepath.Join(dataDir, "stageworker-recovery")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create recovery dir: %w", err)
	}
	return &RecoveryStore{dir: dir}, nil
}

// BeginAttempt writes a running attempt record before dispatch.
func (s *RecoveryStore) BeginAttempt(runID, stage, family string) (*AttemptRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := AttemptRecord{
		ID:        uuid.NewString(),
		RunID:     runID,
		Stage:     stage,
		Family:    family,
		Status:    LifecycleRunning,
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.writeLocked(rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// CommitAttempt marks an attempt succeeded after the artifact has been
// verified in CAS.
func (s *RecoveryStore) CommitAttempt(rec AttemptRecord, artifactSHA string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec.Status = LifecycleSucceeded
	rec.ArtifactSHA256 = artifactSHA
	rec.UpdatedAt = time.Now().UTC()
	if err := s.writeLocked(rec); err != nil {
		return err
	}
	return s.appendManifestLocked(artifactSHA)
}

// CancelAttempt marks an attempt cancelled (cooperative cancellation) and keeps
// the committed artifact manifest intact.
func (s *RecoveryStore) CancelAttempt(rec AttemptRecord, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec.Status = LifecycleCancelled
	rec.ErrorMessage = reason
	rec.UpdatedAt = time.Now().UTC()
	return s.writeLocked(rec)
}

// InterruptAttempt marks an attempt interrupted and keeps the committed
// artifact manifest intact.
func (s *RecoveryStore) InterruptAttempt(rec AttemptRecord, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec.Status = LifecycleInterrupted
	rec.ErrorMessage = reason
	rec.UpdatedAt = time.Now().UTC()
	return s.writeLocked(rec)
}

// ListRunning returns all running attempt records for startup reconciliation.
func (s *RecoveryStore) ListRunning() ([]AttemptRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list recovery records: %w", err)
	}
	var records []AttemptRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == "manifest.json" {
			continue
		}
		rec, err := s.readLocked(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		if rec.Status == LifecycleRunning || rec.Status == LifecycleCancelling {
			records = append(records, *rec)
		}
	}
	return records, nil
}

// Recover transitions every running attempt to interrupted. Previously
// committed artifacts remain in CAS and are listed in the manifest.
func (s *RecoveryStore) Recover() ([]AttemptRecord, error) {
	records, err := s.ListRunning()
	if err != nil {
		return nil, err
	}
	for i := range records {
		if err := s.InterruptAttempt(records[i], "runtime crashed; lost attempt interrupted"); err != nil {
			return records, err
		}
		records[i].Status = LifecycleInterrupted
	}
	return records, nil
}

// Manifest returns committed artifact hashes that survived recovery.
func (s *RecoveryStore) Manifest() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readManifestLocked()
}

func (s *RecoveryStore) writeAtomic(destPath string, data []byte) error {
	tmpFile, err := os.CreateTemp(s.dir, "recovery-*.tmp")
	if err != nil {
		return fmt.Errorf("create recovery tmp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write recovery tmp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync recovery tmp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close recovery tmp file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("atomic rename recovery file: %w", err)
	}
	tmpPath = ""
	return nil
}

func (s *RecoveryStore) writeLocked(rec AttemptRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal recovery record: %w", err)
	}
	path := filepath.Join(s.dir, rec.ID+".json")
	return s.writeAtomic(path, b)
}

func (s *RecoveryStore) readLocked(path string) (*AttemptRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec AttemptRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *RecoveryStore) appendManifestLocked(sha string) error {
	hashes, err := s.readManifestLocked()
	if err != nil {
		return err
	}
	for _, h := range hashes {
		if h == sha {
			return nil
		}
	}
	hashes = append(hashes, sha)
	return s.writeManifestLocked(hashes)
}

func (s *RecoveryStore) readManifestLocked() ([]string, error) {
	path := filepath.Join(s.dir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var hashes []string
	if err := json.Unmarshal(b, &hashes); err != nil {
		return nil, err
	}
	return hashes, nil
}

func (s *RecoveryStore) writeManifestLocked(hashes []string) error {
	b, err := json.MarshalIndent(hashes, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return s.writeAtomic(filepath.Join(s.dir, "manifest.json"), b)
}
