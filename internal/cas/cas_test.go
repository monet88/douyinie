package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCAS_PutAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatalf("failed to create CAS store: %v", err)
	}

	content := []byte("hello douyinie content addressed store")
	hasher := sha256.New()
	hasher.Write(content)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	// Put via reader
	obj, err := store.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("store.Put failed: %v", err)
	}

	if obj.SHA256 != expectedHash {
		t.Errorf("expected hash %s, got %s", expectedHash, obj.SHA256)
	}
	if obj.ByteSize != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), obj.ByteSize)
	}

	// Verify Exists
	if !store.Exists(expectedHash) {
		t.Errorf("expected object %s to exist", expectedHash)
	}

	// Verify ResolvePath layout
	resolved, err := store.ResolvePath(expectedHash)
	if err != nil {
		t.Fatalf("ResolvePath failed: %v", err)
	}
	expectedSuffix := filepath.Join("cas", expectedHash[0:2], expectedHash[2:4], expectedHash)
	if !bytes.HasSuffix([]byte(resolved), []byte(expectedSuffix)) {
		t.Errorf("expected resolved path to end with %s, got %s", expectedSuffix, resolved)
	}

	// Read back via Get
	reader, err := store.Get(expectedHash)
	if err != nil {
		t.Fatalf("store.Get failed: %v", err)
	}
	defer reader.Close()

	readContent, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("io.ReadAll failed: %v", err)
	}
	if string(readContent) != string(content) {
		t.Errorf("expected content %q, got %q", string(content), string(readContent))
	}

	// Verify Integrity
	if err := store.VerifyIntegrity(expectedHash); err != nil {
		t.Errorf("VerifyIntegrity failed: %v", err)
	}
}

func TestCAS_PutFile(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatalf("failed to create CAS store: %v", err)
	}

	// Create a test file
	srcFile := filepath.Join(tmpDir, "sample.txt")
	sampleData := []byte("sample source file data for ingestion")
	if err := os.WriteFile(srcFile, sampleData, 0644); err != nil {
		t.Fatalf("failed to write sample file: %v", err)
	}

	obj, err := store.PutFile(srcFile)
	if err != nil {
		t.Fatalf("PutFile failed: %v", err)
	}

	if err := store.VerifyIntegrity(obj.SHA256); err != nil {
		t.Errorf("VerifyIntegrity after PutFile failed: %v", err)
	}
}

func TestCAS_ExistingCorruptFile_FailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatalf("failed to create CAS store: %v", err)
	}

	content := []byte("valid content to be stored in CAS")
	obj, err := store.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("initial Put failed: %v", err)
	}

	// Corrupt the file on disk directly at the destination path
	destPath, err := store.ResolvePath(obj.SHA256)
	if err != nil {
		t.Fatalf("ResolvePath failed: %v", err)
	}
	if err := os.WriteFile(destPath, []byte("corrupted file bytes on disk!!"), 0644); err != nil {
		t.Fatalf("failed to tamper with file on disk: %v", err)
	}

	// Subsequent Put with same original content MUST fail closed upon detecting corrupted existing file
	_, err = store.Put(bytes.NewReader(content))
	if err == nil {
		t.Fatalf("expected Put to fail closed when existing destination file is corrupt, got nil")
	}
	if !errors.Is(err, ErrHashMismatch) {
		t.Errorf("expected ErrHashMismatch, got %v", err)
	}
}
