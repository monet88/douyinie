package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrObjectNotFound    = errors.New("cas object not found")
	ErrHashMismatch      = errors.New("cas object hash mismatch")
	ErrInvalidHash       = errors.New("invalid sha256 hash format")
	ErrInvalidSourcePath = errors.New("invalid source file path")
)

// Object represents a committed item in the Content-Addressed Store.
type Object struct {
	SHA256   string `json:"sha256"`
	ByteSize int64  `json:"byte_size"`
	Path     string `json:"path"`
}

// Store manages content-addressed files with atomic same-volume commits.
type Store struct {
	rootDir string
	casDir  string
	tmpDir  string
	mu      sync.RWMutex
}

// NewStore initializes a CAS filesystem store.
func NewStore(rootDir string) (*Store, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve abs root: %w", err)
	}

	casDir := filepath.Join(absRoot, "cas")
	tmpDir := filepath.Join(casDir, "tmp")

	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return nil, fmt.Errorf("create cas tmp dir: %w", err)
	}

	return &Store{
		rootDir: absRoot,
		casDir:  casDir,
		tmpDir:  tmpDir,
	}, nil
}

// ResolvePath returns the absolute path where an object with the given SHA-256 should reside.
func (s *Store) ResolvePath(hash string) (string, error) {
	cleanHash := strings.ToLower(strings.TrimSpace(hash))
	if len(cleanHash) != 64 {
		return "", ErrInvalidHash
	}
	for _, c := range cleanHash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", ErrInvalidHash
		}
	}
	// Path layout: <rootDir>/cas/<ab>/<cd>/<sha256>
	return filepath.Join(s.casDir, cleanHash[0:2], cleanHash[2:4], cleanHash), nil
}

// Exists checks if an object with the given SHA-256 is present in the CAS.
func (s *Store) Exists(hash string) bool {
	p, err := s.ResolvePath(hash)
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// Put writes stream content into CAS using atomic same-volume staging and rename.
func (s *Store) Put(r io.Reader) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Create temporary file in same-volume tmp directory
	tmpFile, err := os.CreateTemp(s.tmpDir, "cas-stage-*.tmp")
	if err != nil {
		return Object{}, fmt.Errorf("create staging file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		// Clean up staging file if not committed
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	// 2. Stream into temp file while computing SHA-256
	hasher := sha256.New()
	multiWriter := io.MultiWriter(tmpFile, hasher)

	written, err := io.Copy(multiWriter, r)
	if err != nil {
		_ = tmpFile.Close()
		return Object{}, fmt.Errorf("write staging file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return Object{}, fmt.Errorf("sync staging file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return Object{}, fmt.Errorf("close staging file: %w", err)
	}

	// 3. Compute final SHA-256
	hashStr := hex.EncodeToString(hasher.Sum(nil))
	destPath, err := s.ResolvePath(hashStr)
	if err != nil {
		return Object{}, err
	}

	// 4. Ensure destination parent directory exists
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return Object{}, fmt.Errorf("create dest dir: %w", err)
	}

	// 5. If file already exists, verify its content hash before reuse
	if info, err := os.Stat(destPath); err == nil && !info.IsDir() {
		if err := s.verifyPathHash(destPath, hashStr); err == nil {
			_ = os.Remove(tmpPath)
			tmpPath = ""
			return Object{
				SHA256:   hashStr,
				ByteSize: info.Size(),
				Path:     destPath,
			}, nil
		}
		// Existing file on disk is corrupt; fail closed
		return Object{}, fmt.Errorf("%w: existing file at %s is corrupted", ErrHashMismatch, destPath)
	}

	// 6. Atomic same-volume rename
	if err := os.Rename(tmpPath, destPath); err != nil {
		return Object{}, fmt.Errorf("atomic rename into CAS: %w", err)
	}
	tmpPath = "" // Disarm defer remove

	return Object{
		SHA256:   hashStr,
		ByteSize: written,
		Path:     destPath,
	}, nil
}

func (s *Store) verifyPathHash(path string, expectedHash string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("hash existing file: %w", err)
	}

	computed := hex.EncodeToString(hasher.Sum(nil))
	if computed != strings.ToLower(expectedHash) {
		return fmt.Errorf("%w: expected %s, got %s", ErrHashMismatch, expectedHash, computed)
	}
	return nil
}

// PutFile ingests a local file into the CAS.
func (s *Store) PutFile(srcPath string) (Object, error) {
	info, err := os.Stat(srcPath)
	if err != nil {
		return Object{}, fmt.Errorf("%w: %v", ErrInvalidSourcePath, err)
	}
	if info.IsDir() {
		return Object{}, fmt.Errorf("%w: path is a directory", ErrInvalidSourcePath)
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return Object{}, fmt.Errorf("open source file: %w", err)
	}
	defer f.Close()

	return s.Put(f)
}

// Get opens a CAS object for reading.
func (s *Store) Get(hash string) (io.ReadCloser, error) {
	p, err := s.ResolvePath(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, err
	}
	return f, nil
}

// VerifyIntegrity checks if the object exists and its actual content matches the hash.
func (s *Store) VerifyIntegrity(hash string) error {
	p, err := s.ResolvePath(hash)
	if err != nil {
		return err
	}

	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrObjectNotFound
		}
		return err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("read object for verification: %w", err)
	}

	computed := hex.EncodeToString(hasher.Sum(nil))
	if computed != strings.ToLower(hash) {
		return fmt.Errorf("%w: expected %s, got %s", ErrHashMismatch, hash, computed)
	}

	return nil
}
