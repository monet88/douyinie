package server

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// TestProductionWriteTimeoutCoversLongOperations pins the production HTTP
// write timeout: localhost synchronous ML endpoints legitimately run for
// multiple minutes before the first response byte, so the timeout must equal
// the named long-operation budget and stay finite/non-zero. No sleeping.
func TestProductionWriteTimeoutCoversLongOperations(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.server == nil {
		t.Fatal("expected production http.Server to be configured")
	}
	if got := s.server.WriteTimeout; got != runtimeHostWriteTimeout {
		t.Fatalf("WriteTimeout = %v, want named runtimeHostWriteTimeout %v", got, runtimeHostWriteTimeout)
	}
	if s.server.WriteTimeout <= 0 {
		t.Fatal("WriteTimeout must remain finite/non-zero")
	}
	if want := 30 * time.Minute; s.server.WriteTimeout != want {
		t.Fatalf("WriteTimeout = %v, want %v", s.server.WriteTimeout, want)
	}
	if got := s.server.ReadTimeout; got != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s (must stay unchanged)", got)
	}
}

func TestOperatorUIShellRoutes(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})

	tests := []struct {
		path        string
		contentType string
		contains    []string
	}{
		{
			path:        "/",
			contentType: "text/html",
			contains:    []string{"Douyinie", "Operator", "rel=\"icon\"", "/ui/app.css", "/ui/app.js"},
		},
		{
			path:        "/ui/app.css",
			contentType: "text/css",
			contains:    []string{"--surface", ".app-shell"},
		},
		{
			path:        "/ui/app.js",
			contentType: "javascript",
			contains:    []string{"/api/v1/health", "/api/v1/jobs", "/api/v1/assets/upload"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != 200 {
				t.Fatalf("GET %s status = %d, want 200; body=%q", tt.path, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tt.contentType) {
				t.Fatalf("GET %s Content-Type = %q, want to contain %q", tt.path, got, tt.contentType)
			}
			body := rec.Body.String()
			for _, want := range tt.contains {
				if !strings.Contains(body, want) {
					t.Fatalf("GET %s body missing %q", tt.path, want)
				}
			}
		})
	}
}

func TestOperatorUIUploadsLocalMediaWithoutExposingFilesystemPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}
	ingest := service.NewIngestService(db, casStore, &media.MockProber{CustomReport: &domain.PreflightReport{
		ContainerValid:         true,
		FingerprintMatch:       true,
		ContainerFormat:        "mp4",
		NormalizedAudioSHA256:  "normalized-audio-sha",
		NormalizedAudioCASPath: "cas/normalized-audio",
	}})
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, Ingest: ingest})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "sample.mp4")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte("fake mp4 payload for browser upload")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	for key, value := range map[string]string{
		"declared_by":    "ui-test-operator",
		"terms_accepted": "true",
		"notes":          "browser upload acceptance",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"sample.mp4", "ui-test-operator", "browser upload acceptance"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %q: %s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), root) {
		t.Fatalf("response leaked server filesystem path %q: %s", root, rec.Body.String())
	}
}

func TestRenderMediaRoutesStreamOnlyIndexedOutputCAS(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(root)
	if err != nil {
		t.Fatalf("new cas store: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	attestation := domain.RightsAttestation{
		ID:              "att-ui-media",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "server-test",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, attestation); err != nil {
		t.Fatalf("create attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  "asset-ui-media",
		SHA256:              strings.Repeat("a", 64),
		ByteSize:            16,
		MimeType:            "video/mp4",
		OriginalFilename:    "source.mp4",
		RightsAttestationID: attestation.ID,
		CASPath:             filepath.Join(root, "source.mp4"),
		CreatedAt:           now,
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	mediaBytes := []byte("0123456789abcdef")
	mediaObject, err := casStore.Put(bytes.NewReader(mediaBytes))
	if err != nil {
		t.Fatalf("put media object: %v", err)
	}

	for i, kind := range []string{domain.RenderKindPreview, domain.RenderKindFinal} {
		marker := byte('b' + i)
		if err := db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
			ID:             "idx-" + kind,
			AssetID:        "asset-ui-media",
			RunID:          "run-ui-media",
			JobID:          "job-ui-media",
			TargetLanguage: "vi",
			Kind:           kind,
			PlanProvenance: strings.Repeat(string(marker), 64),
			PlanCASHash:    strings.Repeat("d", 64),
			OutputCASHash:  mediaObject.SHA256,
			CASHash:        strings.Repeat("e", 64),
			ProvenanceHash: strings.Repeat(string(marker+2), 64),
			OverallStatus:  "PASS",
			CreatedAt:      now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("save %s render index: %v", kind, err)
		}
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore})

	t.Run("preview supports byte ranges", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-ui-media/render/preview/media?target_language=vi", nil)
		req.Header.Set("Range", "bytes=2-5")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != 206 {
			t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
			t.Fatalf("Content-Type = %q, want video/mp4", got)
		}
		if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/16" {
			t.Fatalf("Content-Range = %q, want bytes 2-5/16", got)
		}
		if got, want := rec.Body.String(), string(mediaBytes[2:6]); got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})

	t.Run("final streams full indexed media", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-ui-media/render/final/media?target_language=vi", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Body.Bytes(); !bytes.Equal(got, mediaBytes) {
			t.Fatalf("body = %q, want %q", got, mediaBytes)
		}
	})

	t.Run("unindexed asset stays inaccessible", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/other/render/preview/media?target_language=vi", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}
