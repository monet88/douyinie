package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Server encapsulates the RuntimeHost HTTP daemon.
type Server struct {
	db       *storage.DB
	casStore *cas.Store
	ingest   *service.IngestService
	registry *provider.Registry
	mux      *http.ServeMux
	server   *http.Server
}

// Config specifies initialization options for RuntimeHost Server.
type Config struct {
	Addr     string
	DB       *storage.DB
	CASStore *cas.Store
	Ingest   *service.IngestService
	Registry *provider.Registry
}

// New creates a new RuntimeHost Server instance.
func New(cfg Config) *Server {
	s := &Server{
		db:       cfg.DB,
		casStore: cfg.CASStore,
		ingest:   cfg.Ingest,
		registry: cfg.Registry,
		mux:      http.NewServeMux(),
	}

	s.routes()

	s.server = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	return s
}

// Handler returns the underlying http.Handler for in-memory / testing purposes.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Start begins listening on the configured address.
func (s *Server) Start() error {
	return s.server.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) routes() {
	// Health checks
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	// Rights Attestations
	s.mux.HandleFunc("POST /api/v1/attestations", s.handleCreateAttestation)
	s.mux.HandleFunc("GET /api/v1/attestations/{id}", s.handleGetAttestation)

	// Source Asset Ingest & Retrieval
	s.mux.HandleFunc("POST /api/v1/assets/ingest", s.handleIngestAsset)
	s.mux.HandleFunc("GET /api/v1/assets/{id}", s.handleGetAsset)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/preflight", s.handleGetAssetPreflight)

	// Localization Jobs & Runs
	s.mux.HandleFunc("POST /api/v1/jobs", s.handleCreateJob)
	s.mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/runs", s.handleCreateRun)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.handleGetRun)

	// Provider Registry Inspection
	s.mux.HandleFunc("GET /api/v1/providers", s.handleListProviders)
}

// JSON helpers
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
	})
}

// Handlers

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   "v1.0.0",
		"component": "runtimehost",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleCreateAttestation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AttestationType string `json:"attestation_type"`
		DeclaredBy      string `json:"declared_by"`
		TermsAccepted   bool   `json:"terms_accepted"`
		Notes           string `json:"notes,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if !body.TermsAccepted || strings.TrimSpace(body.DeclaredBy) == "" {
		writeError(w, http.StatusBadRequest, "terms_accepted must be true and declared_by must be specified")
		return
	}

	attestationType := body.AttestationType
	if attestationType == "" {
		attestationType = "OPERATOR_EXPLICIT_CONFIRMATION"
	}

	ra := domain.RightsAttestation{
		ID:              uuid.NewString(),
		AttestationType: attestationType,
		DeclaredBy:      body.DeclaredBy,
		TermsAccepted:   body.TermsAccepted,
		Notes:           body.Notes,
		ConfirmedAt:     time.Now().UTC(),
	}

	if err := s.db.CreateRightsAttestation(r.Context(), ra); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist attestation: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"attestation": ra})
}

func (s *Server) handleGetAttestation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ra, err := s.db.GetRightsAttestation(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "attestation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"attestation": ra})
}

func (s *Server) handleIngestAsset(w http.ResponseWriter, r *http.Request) {
	var req service.IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	res, err := s.ingest.IngestLocalFile(r.Context(), req)
	if err != nil {
		if errors.Is(err, domain.ErrRightsAttestationRequired) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, domain.ErrCorruptMedia) || errors.Is(err, domain.ErrFingerprintMismatch) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleGetAsset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"asset": asset})
}

func (s *Server) handleGetAssetPreflight(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	report, err := s.db.GetPreflightReport(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "preflight report not found for asset")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"preflight_report": report})
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceAssetID  string `json:"source_asset_id"`
		TargetLanguage string `json:"target_language"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if !domain.IsValidTargetLanguage(body.TargetLanguage) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidTargetLanguage.Error())
		return
	}

	// Verify source asset exists
	if _, err := s.db.GetSourceAsset(r.Context(), body.SourceAssetID); err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusBadRequest, "source_asset_id not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	job := domain.LocalizationJob{
		ID:             uuid.NewString(),
		SourceAssetID:  body.SourceAssetID,
		TargetLanguage: body.TargetLanguage,
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := s.db.CreateJob(r.Context(), job); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"job": job})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.db.ListJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list jobs: "+err.Error())
		return
	}
	if jobs == nil {
		jobs = []domain.LocalizationJob{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.db.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrJobNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if _, err := s.db.GetJob(r.Context(), jobID); err != nil {
		if errors.Is(err, domain.ErrJobNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var body struct {
		ConfigSnapshotJSON string `json:"config_snapshot_json"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.ConfigSnapshotJSON == "" {
		body.ConfigSnapshotJSON = "{}"
	}

	run := domain.LocalizationRun{
		ID:                 uuid.NewString(),
		JobID:              jobID,
		Status:             "queued",
		ConfigSnapshotJSON: body.ConfigSnapshotJSON,
		CreatedAt:          time.Now().UTC(),
	}

	if err := s.db.CreateRun(r.Context(), run); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.db.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	type providerSummary struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Policy  string `json:"policy"`
		Healthy bool   `json:"healthy"`
	}

	types := []provider.ProviderType{
		provider.TypeASR,
		provider.TypeAligner,
		provider.TypeTTS,
		provider.TypeSeparator,
		provider.TypeOCR,
		provider.TypeTranslation,
	}

	var list []providerSummary
	if s.registry != nil {
		for _, t := range types {
			for _, p := range s.registry.ListByType(t) {
				list = append(list, providerSummary{
					ID:      p.ID(),
					Type:    string(p.Type()),
					Policy:  string(p.PolicyState()),
					Healthy: p.IsHealthy(),
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}
