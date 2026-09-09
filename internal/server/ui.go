package server

import (
	"embed"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

const operatorUploadMaxBytes int64 = 2 << 30

// operatorUIFS packages the browser-on-localhost operator shell into RuntimeHost.
// The UI remains dependency-free and talks only to the versioned RuntimeHost API.
//
//go:embed ui/index.html ui/app.css ui/app.js
var operatorUIFS embed.FS

func (s *Server) handleOperatorUIIndex(w http.ResponseWriter, _ *http.Request) {
	serveOperatorUIFile(w, "ui/index.html", "text/html; charset=utf-8")
}

func (s *Server) handleOperatorUIAsset(w http.ResponseWriter, r *http.Request) {
	asset := r.PathValue("asset")
	switch asset {
	case "app.css":
		serveOperatorUIFile(w, "ui/app.css", "text/css; charset=utf-8")
	case "app.js":
		serveOperatorUIFile(w, "ui/app.js", "text/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}

func serveOperatorUIFile(w http.ResponseWriter, path, contentType string) {
	data, err := operatorUIFS.ReadFile(path)
	if err != nil {
		http.Error(w, "operator UI asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleUploadAsset(w http.ResponseWriter, r *http.Request) {
	if s.ingest == nil {
		writeError(w, http.StatusInternalServerError, "ingest service is not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, operatorUploadMaxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart upload: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	filename := filepath.Base(strings.TrimSpace(header.Filename))
	if filename == "" || filename == "." {
		writeError(w, http.StatusBadRequest, "uploaded filename is required")
		return
	}

	termsAccepted, err := strconv.ParseBool(strings.TrimSpace(r.FormValue("terms_accepted")))
	if err != nil || !termsAccepted {
		writeError(w, http.StatusBadRequest, "terms_accepted must be true")
		return
	}
	declaredBy := strings.TrimSpace(r.FormValue("declared_by"))
	if declaredBy == "" {
		writeError(w, http.StatusBadRequest, "declared_by is required")
		return
	}

	tmpDir, err := os.MkdirTemp("", "douyinie-upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create upload staging directory: "+err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, filename)
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create upload staging file: "+err.Error())
		return
	}
	if _, err := io.Copy(tmpFile, file); err != nil {
		_ = tmpFile.Close()
		writeError(w, http.StatusBadRequest, "read uploaded media: "+err.Error())
		return
	}
	if err := tmpFile.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "close upload staging file: "+err.Error())
		return
	}

	res, err := s.ingest.IngestLocalFile(r.Context(), service.IngestRequest{
		FilePath: tmpPath,
		Attestation: &domain.RightsAttestation{
			AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
			DeclaredBy:      declaredBy,
			TermsAccepted:   true,
			Notes:           strings.TrimSpace(r.FormValue("notes")),
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRightsAttestationRequired):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, domain.ErrCorruptMedia), errors.Is(err, domain.ErrFingerprintMismatch):
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	asset := map[string]any{
		"id":                    res.Asset.ID,
		"sha256":                res.Asset.SHA256,
		"byte_size":             res.Asset.ByteSize,
		"mime_type":             res.Asset.MimeType,
		"original_filename":     res.Asset.OriginalFilename,
		"rights_attestation_id": res.Asset.RightsAttestationID,
		"created_at":            res.Asset.CreatedAt,
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"asset":       asset,
		"attestation": res.Attestation,
	})
}

func (s *Server) handleGetRenderPreviewMedia(w http.ResponseWriter, r *http.Request) {
	s.serveLatestRenderMedia(w, r, domain.RenderKindPreview)
}

func (s *Server) handleGetRenderFinalMedia(w http.ResponseWriter, r *http.Request) {
	s.serveLatestRenderMedia(w, r, domain.RenderKindFinal)
}

func (s *Server) serveLatestRenderMedia(w http.ResponseWriter, r *http.Request, kind string) {
	if s.db == nil || s.casStore == nil {
		writeError(w, http.StatusInternalServerError, "render media storage is not configured")
		return
	}

	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}
	idx, err := s.db.GetLatestRenderArtifactIndex(r.Context(), r.PathValue("id"), targetLang, kind)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, kind+" render media not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if idx.OutputCASHash == "" {
		writeError(w, http.StatusInternalServerError, kind+" render media has no output CAS binding")
		return
	}

	mediaPath, err := s.casStore.ResolvePath(idx.OutputCASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid render media CAS binding")
		return
	}
	mediaFile, err := os.Open(mediaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, kind+" render media object not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "open render media: "+err.Error())
		return
	}
	defer mediaFile.Close()

	info, err := mediaFile.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stat render media: "+err.Error())
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusInternalServerError, "render media CAS binding resolved to a directory")
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", "inline; filename=\"douyinie-"+kind+".mp4\"")
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "douyinie-"+kind+".mp4", info.ModTime(), mediaFile)
}
