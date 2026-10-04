package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Issue #156 acceptance: the dub-media endpoint never exposes arbitrary filesystem paths. An
// unclassified storage failure wraps an *os.PathError carrying the absolute CAS object path, so
// the classifier must answer a fixed message and keep the detail server-side.
func TestWriteDubMediaError_NeverEchoesFilesystemPaths(t *testing.T) {
	casPath := `C:\douyinie-data\cas\objects\ab\cdef0123456789.wav`
	err := fmt.Errorf("stat audio media %s: %w", strings.Repeat("a", 64), &os.PathError{
		Op: "stat", Path: casPath, Err: fs.ErrPermission,
	})

	rec := httptest.NewRecorder()
	(&Server{}).writeDubMediaError(rec, "run-1", err)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for an unclassified resolution failure, got %d", rec.Code)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if decErr := json.NewDecoder(rec.Body).Decode(&payload); decErr != nil {
		t.Fatalf("decode refusal body: %v", decErr)
	}
	if payload.Error != "dub media resolution failed" {
		t.Fatalf("expected the fixed refusal message, got %q", payload.Error)
	}
	if strings.Contains(rec.Body.String(), casPath) || strings.Contains(rec.Body.String(), "douyinie-data") {
		t.Fatalf("refusal body leaked a filesystem path: %s", rec.Body.String())
	}
}
