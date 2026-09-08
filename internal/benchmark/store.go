package benchmark

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrSessionNotFound  = errors.New("benchmark session not found")
	ErrInvalidSessionID = errors.New("invalid session ID: must match canonical bms_<64-hex>")
)

// ValidateSessionID verifies that sessionID strictly conforms to the canonical "bms_<64-hex>" format.
// This prevents path traversal attacks and ensures only deterministic session identities address storage paths.
func ValidateSessionID(sessionID string) error {
	if !strings.HasPrefix(sessionID, "bms_") {
		return ErrInvalidSessionID
	}
	hexPart := sessionID[4:]
	if len(hexPart) != 64 {
		return ErrInvalidSessionID
	}
	for i := range hexPart {
		c := hexPart[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ErrInvalidSessionID
		}
	}
	return nil
}

// ErrIdentityMismatch is returned when a resume attempt encounters an existing session
// whose bound identity (corpus, profile, build, config, provider baseline, or environment)
// differs from the expected identity, preventing unsafe evidence mixing.
type ErrIdentityMismatch struct {
	SessionID      string   `json:"session_id"`
	ExpectedDigest string   `json:"expected_digest"`
	ActualDigest   string   `json:"actual_digest"`
	Differences    []string `json:"differences"`
}

func (e *ErrIdentityMismatch) Error() string {
	diffMsg := "none"
	if len(e.Differences) > 0 {
		diffMsg = strings.Join(e.Differences, "; ")
	}
	return fmt.Sprintf("benchmark session %s identity mismatch: expected digest %s, got %s; diffs: %s",
		e.SessionID, e.ExpectedDigest, e.ActualDigest, diffMsg)
}

// Store defines persistence and resume operations for benchmark sessions.
type Store interface {
	Save(s *Session) error
	Load(sessionID string) (*Session, error)
	Resume(expected SessionIdentityInput) (*Session, error)
	ResumeByID(sessionID string, expected SessionIdentityInput) (*Session, error)
}

// FileStore implements sidecar JSON file persistence for benchmark sessions.
// Invariant: benchmark tooling lives outside the product architecture; sidecar serialization
// suffices for session state without adding persistent product tables to SQLite.
type FileStore struct {
	rootDir string
	mu      sync.Mutex
}

// NewFileStore creates a new FileStore rooted at rootDir.
func NewFileStore(rootDir string) (*FileStore, error) {
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("create benchmark session store dir: %w", err)
	}
	return &FileStore{rootDir: rootDir}, nil
}

func (fs *FileStore) sessionDir(sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(fs.rootDir, sessionID), nil
}

func (fs *FileStore) sessionFile(sessionID string) (string, error) {
	dir, err := fs.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "benchmark_session.json"), nil
}

func (fs *FileStore) identityFile(sessionID string) (string, error) {
	dir, err := fs.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "identity.json"), nil
}

// Save writes session state and identity sidecars atomically via temp files and atomic rename.
func (fs *FileStore) Save(s *Session) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	snap := s.Snapshot()
	dir, err := fs.sessionDir(snap.ID)
	if err != nil {
		return fmt.Errorf("validate session ID: %w", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	sessionBytes, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	sessionTmp := filepath.Join(dir, "benchmark_session.json.tmp")
	if err := os.WriteFile(sessionTmp, sessionBytes, 0644); err != nil {
		return fmt.Errorf("write session tmp: %w", err)
	}
	sessionTarget, err := fs.sessionFile(snap.ID)
	if err != nil {
		_ = os.Remove(sessionTmp)
		return err
	}
	if err := os.Rename(sessionTmp, sessionTarget); err != nil {
		_ = os.Remove(sessionTmp)
		return fmt.Errorf("commit session file: %w", err)
	}

	// Also write identity.json for standalone inspection
	identityBytes, err := json.MarshalIndent(snap.IdentityInput, "", "  ")
	if err == nil {
		identityTmp := filepath.Join(dir, "identity.json.tmp")
		if err := os.WriteFile(identityTmp, identityBytes, 0644); err == nil {
			if idTarget, err := fs.identityFile(snap.ID); err == nil {
				_ = os.Rename(identityTmp, idTarget)
			} else {
				_ = os.Remove(identityTmp)
			}
		}
	}

	return nil
}

// Load reads and returns an existing session from disk.
func (fs *FileStore) Load(sessionID string) (*Session, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	target, err := fs.sessionFile(sessionID)
	if err != nil {
		return nil, fmt.Errorf("validate session ID: %w", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("read session file: %w", err)
	}

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal session file: %w", err)
	}
	if s.AcquisitionEntries == nil {
		s.AcquisitionEntries = make(map[string]*AcquisitionEntryEvidence)
	}
	if s.QualityCases == nil {
		s.QualityCases = make(map[string]*QualityCaseEvidence)
	}

	return &s, nil
}

// Resume attempts to resume a session matching expectedIdentity.
// If no existing session exists, a new session is initialized and saved.
// If a session exists but identity differs, ErrIdentityMismatch is returned.
// If identity matches, the existing session is resumed without mutating prior evidence.
func (fs *FileStore) Resume(expected SessionIdentityInput) (*Session, error) {
	sessionID, _, err := ComputeSessionID(expected)
	if err != nil {
		return nil, fmt.Errorf("compute expected session ID: %w", err)
	}

	target, err := fs.sessionFile(sessionID)
	if err != nil {
		return nil, fmt.Errorf("validate session ID: %w", err)
	}
	if _, err := os.Stat(target); os.IsNotExist(err) {
		// New session
		sess, err := NewSession(expected)
		if err != nil {
			return nil, err
		}
		sess.Status = SessionStatusRunning
		if err := fs.Save(sess); err != nil {
			return nil, fmt.Errorf("save initial session: %w", err)
		}
		return sess, nil
	}

	return fs.ResumeByID(sessionID, expected)
}

// ResumeByID resumes an existing session by ID, validating that its stored identity
// strictly matches expected.
func (fs *FileStore) ResumeByID(sessionID string, expected SessionIdentityInput) (*Session, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("validate session ID: %w", err)
	}

	expectedID, expectedDigest, err := ComputeSessionID(expected)
	if err != nil {
		return nil, fmt.Errorf("compute expected session ID: %w", err)
	}

	if sessionID != expectedID {
		return nil, &ErrIdentityMismatch{
			SessionID:      sessionID,
			ExpectedDigest: expectedDigest,
			ActualDigest:   sessionID,
			Differences: []string{
				fmt.Sprintf("target session ID %q does not match identity computed ID %q", sessionID, expectedID),
			},
		}
	}

	existing, err := fs.Load(sessionID)
	if err != nil {
		return nil, err
	}

	// Verify identity digest and granular differences
	diffs := CompareSessionIdentities(expected, existing.IdentityInput)
	if existing.IdentityDigest != expectedDigest || len(diffs) > 0 {
		return nil, &ErrIdentityMismatch{
			SessionID:      sessionID,
			ExpectedDigest: expectedDigest,
			ActualDigest:   existing.IdentityDigest,
			Differences:    diffs,
		}
	}

	// Valid resume: transition to RUNNING if not already completed/failed
	if existing.Status == SessionStatusInitialized || existing.Status == SessionStatusPaused {
		existing.SetStatus(SessionStatusRunning)
		_ = fs.Save(existing)
	}

	return existing, nil
}
