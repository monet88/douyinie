package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/config"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Executor defines an injected execution seam for stage provider execution.
type Executor func(ctx context.Context, p provider.Provider, attemptNumber int) error

// Server encapsulates the RuntimeHost HTTP daemon.
type Server struct {
	db         *storage.DB
	casStore   *cas.Store
	ingest     *service.IngestService
	registry   *provider.Registry
	policySvc  *governance.PolicyService
	licenseSvc *governance.LicenseService
	credSvc    *governance.CredentialService
	router     *provider.Router
	queueSvc   *queue.Service
	scheduler  *scheduler.Scheduler
	executor   Executor
	mux        *http.ServeMux
	server     *http.Server
}

// Config specifies initialization options for RuntimeHost Server.
type Config struct {
	Addr       string
	DB         *storage.DB
	CASStore   *cas.Store
	Ingest     *service.IngestService
	Registry   *provider.Registry
	PolicySvc  *governance.PolicyService
	LicenseSvc *governance.LicenseService
	CredSvc    *governance.CredentialService
	Router     *provider.Router
	QueueSvc   *queue.Service
	Scheduler  *scheduler.Scheduler
	Executor   Executor // Injected execution seam for testing and custom worker dispatch
}

// New creates a new RuntimeHost Server instance.
func New(cfg Config) *Server {
	if cfg.PolicySvc == nil && cfg.DB != nil {
		cfg.PolicySvc = governance.NewPolicyService(cfg.DB)
	}
	if cfg.LicenseSvc == nil && cfg.DB != nil {
		cfg.LicenseSvc = governance.NewLicenseService(cfg.DB)
	}
	if cfg.CredSvc == nil && cfg.DB != nil {
		cfg.CredSvc = governance.NewCredentialService(cfg.DB)
	}
	if cfg.Router == nil && cfg.Registry != nil {
		cfg.Router = provider.NewRouter(cfg.Registry, cfg.PolicySvc, cfg.LicenseSvc, cfg.CredSvc, nil, cfg.DB)
	}
	if cfg.QueueSvc == nil && cfg.DB != nil {
		cfg.QueueSvc = queue.NewService(cfg.DB)
	}
	if cfg.Scheduler == nil {
		cfg.Scheduler = scheduler.New()
	}

	s := &Server{
		db:         cfg.DB,
		casStore:   cfg.CASStore,
		ingest:     cfg.Ingest,
		registry:   cfg.Registry,
		policySvc:  cfg.PolicySvc,
		licenseSvc: cfg.LicenseSvc,
		credSvc:    cfg.CredSvc,
		router:     cfg.Router,
		queueSvc:   cfg.QueueSvc,
		scheduler:  cfg.Scheduler,
		executor:   cfg.Executor,
		mux:        http.NewServeMux(),
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

// SetExecutor sets or replaces the injected execution seam (e.g. for Seam 1 retry/fallback testing).
func (s *Server) SetExecutor(exec Executor) {
	s.executor = exec
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
	s.mux.HandleFunc("POST /api/v1/assets/{id}/audio-role-plan", s.handleSaveAudioRolePlan)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/audio-role-plan", s.handleGetAudioRolePlan)

	// Localization Jobs & Runs
	s.mux.HandleFunc("POST /api/v1/jobs", s.handleCreateJob)
	s.mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/runs", s.handleCreateRun)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/pause", s.handlePauseRun)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.handleCancelRun)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/resume", s.handleResumeRun)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/stages", s.handleListRunStages)

	// Persisted Queue
	s.mux.HandleFunc("GET /api/v1/queue", s.handleListQueue)
	s.mux.HandleFunc("POST /api/v1/queue/reorder", s.handleReorderQueue)

	// ResourceScheduler (single GPU lease)
	s.mux.HandleFunc("POST /api/v1/scheduler/acquire", s.handleSchedulerAcquire)
	s.mux.HandleFunc("POST /api/v1/scheduler/release", s.handleSchedulerRelease)
	s.mux.HandleFunc("GET /api/v1/scheduler/lease", s.handleSchedulerLease)

	// Provider Registry Inspection
	s.mux.HandleFunc("GET /api/v1/providers", s.handleListProviders)

	// Governance & Routing
	s.mux.HandleFunc("POST /api/v1/routing/decide", s.handleRouteDecide)
	s.mux.HandleFunc("POST /api/v1/routing/execute", s.handleRouteExecute)
	s.mux.HandleFunc("GET /api/v1/routing/decisions", s.handleListDecisions)
	s.mux.HandleFunc("GET /api/v1/routing/attempts", s.handleListAttempts)

	// Policies & Licenses
	s.mux.HandleFunc("GET /api/v1/policies", s.handleListPolicies)
	s.mux.HandleFunc("PUT /api/v1/policies/{id}", s.handleSetPolicy)
	s.mux.HandleFunc("GET /api/v1/licenses", s.handleListLicenses)
	s.mux.HandleFunc("POST /api/v1/licenses", s.handleRegisterLicense)

	// Credential References
	s.mux.HandleFunc("GET /api/v1/credentials", s.handleListCredentials)
	s.mux.HandleFunc("POST /api/v1/credentials", s.handleRegisterCredential)

	// Layered Configuration
	s.mux.HandleFunc("GET /api/v1/config/layered", s.handleGetLayeredConfig)
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

func (s *Server) handleSaveAudioRolePlan(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if _, err := s.db.GetSourceAsset(r.Context(), assetID); err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var body struct {
		Segments []domain.AudioSegment `json:"segments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	for _, seg := range body.Segments {
		switch seg.Role {
		case domain.AudioRoleNarrationDialogue,
			domain.AudioRoleSingingMusicVocal,
			domain.AudioRoleInstrumentalBgm,
			domain.AudioRoleAmbienceSFX,
			domain.AudioRoleUncertain:
			// valid
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid audio role value: %q", seg.Role))
			return
		}
	}

	plan := domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		Segments:  body.Segments,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.db.SaveAudioRolePlan(r.Context(), plan); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"audio_role_plan": plan})
}

func (s *Server) handleGetAudioRolePlan(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	plan, err := s.db.GetAudioRolePlan(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "audio role plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"audio_role_plan": plan})
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

	// Create the run and enqueue it atomically so a partial failure never orphans a run.
	if _, err := s.db.CreateRunEnqueued(r.Context(), run, jobID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create and enqueue run: "+err.Error())
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

func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.queueSvc == nil {
		writeError(w, http.StatusInternalServerError, "queue service is not configured")
		return
	}
	if err := s.queueSvc.Pause(r.Context(), id); err != nil {
		if errors.Is(err, queue.ErrNotQueued) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.RunStatusPaused})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.queueSvc == nil {
		writeError(w, http.StatusInternalServerError, "queue service is not configured")
		return
	}
	if err := s.queueSvc.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, queue.ErrNotQueued) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.RunStatusCancelled})
}

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.queueSvc == nil {
		writeError(w, http.StatusInternalServerError, "queue service is not configured")
		return
	}
	if err := s.queueSvc.Resume(r.Context(), id); err != nil {
		if errors.Is(err, queue.ErrNotQueued) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.RunStatusQueued})
}

func (s *Server) handleListRunStages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "database is not configured")
		return
	}
	stages, err := s.db.ListStageExecutions(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if stages == nil {
		stages = []domain.StageExecution{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stages": stages})
}

func (s *Server) handleListQueue(w http.ResponseWriter, r *http.Request) {
	if s.queueSvc == nil {
		writeError(w, http.StatusInternalServerError, "queue service is not configured")
		return
	}
	entries, err := s.queueSvc.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": entries})
}

func (s *Server) handleReorderQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID    string `json:"run_id"`
		Position int    `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if s.queueSvc == nil {
		writeError(w, http.StatusInternalServerError, "queue service is not configured")
		return
	}
	if err := s.queueSvc.Reorder(r.Context(), body.RunID, body.Position); err != nil {
		if errors.Is(err, queue.ErrNotQueued) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": body.RunID, "position": body.Position})
}

func (s *Server) handleSchedulerAcquire(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Family string `json:"family"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "resource scheduler is not configured")
		return
	}
	leaseID, err := s.scheduler.Acquire(r.Context(), body.Family)
	if err != nil {
		if errors.Is(err, scheduler.ErrLeaseBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  err.Error(),
				"holder": s.scheduler.Holder(),
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lease_id": leaseID,
		"family":   body.Family,
		"profile":  scheduler.Profile(),
	})
}

func (s *Server) handleSchedulerRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "resource scheduler is not configured")
		return
	}
	if err := s.scheduler.Release(r.Context(), body.LeaseID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

func (s *Server) handleSchedulerLease(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusInternalServerError, "resource scheduler is not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"holder":  s.scheduler.Holder(),
		"profile": scheduler.Profile(),
	})
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	type providerSummary struct {
		ID         string                    `json:"id"`
		Type       string                    `json:"type"`
		Policy     string                    `json:"policy"`
		Healthy    bool                      `json:"healthy"`
		Capability domain.ProviderCapability `json:"capability"`
		ModelName  string                    `json:"model_name"`
		ModelVer   string                    `json:"model_version"`
	}

	var list []providerSummary
	if s.registry != nil {
		for _, p := range s.registry.ListAll() {
			effectivePolicy := p.PolicyState()
			if s.policySvc != nil {
				effectivePolicy = s.policySvc.GetPolicy(r.Context(), p.ID(), p.PolicyState())
			}
			mName, mVer := p.ModelInfo()
			list = append(list, providerSummary{
				ID:         p.ID(),
				Type:       string(p.Type()),
				Policy:     string(effectivePolicy),
				Healthy:    p.IsHealthy(),
				Capability: p.Capability(),
				ModelName:  mName,
				ModelVer:   mVer,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

func (s *Server) handleRouteDecide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID                 string                  `json:"run_id"`
		Stage                 string                  `json:"stage"`
		Language              string                  `json:"language"`
		ExecutionProfile      domain.ExecutionProfile `json:"execution_profile"`
		ConsentGranted        bool                    `json:"consent_granted"`
		AuthorizedCredentials []string                `json:"authorized_credentials"`
		RequiredFeatures      []string                `json:"required_features"`
		ExcludedProviders     []string                `json:"excluded_providers"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if body.Stage == "" {
		writeError(w, http.StatusBadRequest, "stage is required")
		return
	}

	req := provider.RouteRequest{
		RunID:                 body.RunID,
		Stage:                 provider.ProviderType(body.Stage),
		Language:              body.Language,
		ExecutionProfile:      body.ExecutionProfile,
		ConsentGranted:        body.ConsentGranted,
		AuthorizedCredentials: body.AuthorizedCredentials,
		RequiredFeatures:      body.RequiredFeatures,
		ExcludedProviders:     body.ExcludedProviders,
	}

	res, err := s.router.Route(r.Context(), req)
	if err != nil {
		if errors.Is(err, domain.ErrNoEligibleProvider) ||
			errors.Is(err, domain.ErrPolicyBlocked) ||
			errors.Is(err, domain.ErrConsentRequired) ||
			errors.Is(err, domain.ErrAuthRequired) ||
			errors.Is(err, domain.ErrLicenseManifestMissing) ||
			errors.Is(err, domain.ErrUncertainRole) ||
			strings.Contains(err.Error(), "audio role plan required") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": err.Error(),
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var selectedProviderID string
	if res.SelectedProvider != nil {
		selectedProviderID = res.SelectedProvider.ID()
	}

	var fallbackIDs []string
	for _, f := range res.FallbackOrdered {
		if f != nil {
			fallbackIDs = append(fallbackIDs, f.ID())
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"selected_provider_id": selectedProviderID,
		"fallback_candidates":  fallbackIDs,
		"decision":             res.Decision,
	})
}

func (s *Server) handleRouteExecute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID                 string                  `json:"run_id"`
		Stage                 string                  `json:"stage"`
		Language              string                  `json:"language"`
		InputHash             string                  `json:"input_hash"`
		ExecutionProfile      domain.ExecutionProfile `json:"execution_profile"`
		ConsentGranted        bool                    `json:"consent_granted"`
		AuthorizedCredentials []string                `json:"authorized_credentials"`
		RequiredFeatures      []string                `json:"required_features"`
		ExcludedProviders     []string                `json:"excluded_providers"`
		MaxRetries            *int                    `json:"max_retries,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if body.Stage == "" {
		writeError(w, http.StatusBadRequest, "stage is required")
		return
	}
	if strings.TrimSpace(body.InputHash) == "" {
		writeError(w, http.StatusBadRequest, "input_hash is required")
		return
	}
	if body.RunID == "" {
		body.RunID = uuid.NewString()
	}
	maxRetries := 2
	if body.MaxRetries != nil {
		maxRetries = *body.MaxRetries
	}

	req := provider.RouteRequest{
		RunID:                 body.RunID,
		Stage:                 provider.ProviderType(body.Stage),
		Language:              body.Language,
		ExecutionProfile:      body.ExecutionProfile,
		ConsentGranted:        body.ConsentGranted,
		AuthorizedCredentials: body.AuthorizedCredentials,
		RequiredFeatures:      body.RequiredFeatures,
		ExcludedProviders:     body.ExcludedProviders,
	}

	if s.router == nil {
		writeError(w, http.StatusInternalServerError, "router is not configured")
		return
	}

	if s.executor == nil {
		writeError(w, http.StatusInternalServerError, "runtime executor is not configured")
		return
	}

	execFn := func(p provider.Provider, attemptNum int) error {
		return s.executor(r.Context(), p, attemptNum)
	}

	err := s.router.ExecuteWithRetry(r.Context(), req, body.InputHash, maxRetries, execFn)

	if err != nil {
		if errors.Is(err, domain.ErrPolicyBlocked) ||
			errors.Is(err, domain.ErrNoEligibleProvider) ||
			errors.Is(err, domain.ErrConsentRequired) ||
			errors.Is(err, domain.ErrAuthRequired) ||
			errors.Is(err, domain.ErrLicenseManifestMissing) ||
			errors.Is(err, domain.ErrUncertainRole) ||
			strings.Contains(err.Error(), "audio role plan required") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "succeeded",
		"run_id": body.RunID,
		"stage":  body.Stage,
	})
}

func (s *Server) handleListDecisions(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	stage := r.URL.Query().Get("stage")

	if s.db == nil {
		writeJSON(w, http.StatusOK, map[string]any{"decisions": []domain.SelectionDecision{}})
		return
	}

	decisions, err := s.db.ListSelectionDecisions(r.Context(), runID, stage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if decisions == nil {
		decisions = []domain.SelectionDecision{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"decisions": decisions})
}

func (s *Server) handleListAttempts(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	stage := r.URL.Query().Get("stage")

	if s.db == nil {
		writeJSON(w, http.StatusOK, map[string]any{"attempts": []domain.ProviderAttempt{}})
		return
	}

	attempts, err := s.db.ListProviderAttempts(r.Context(), runID, stage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if attempts == nil {
		attempts = []domain.ProviderAttempt{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"attempts": attempts})
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	type policyEntry struct {
		ProviderID  string             `json:"provider_id"`
		PolicyState domain.PolicyState `json:"policy_state"`
	}

	var list []policyEntry
	if s.registry != nil {
		for _, p := range s.registry.ListAll() {
			effectivePolicy := p.PolicyState()
			if s.policySvc != nil {
				effectivePolicy = s.policySvc.GetPolicy(r.Context(), p.ID(), p.PolicyState())
			}
			list = append(list, policyEntry{
				ProviderID:  p.ID(),
				PolicyState: effectivePolicy,
			})
		}
	}

	if list == nil {
		list = []policyEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"policies": list})
}

func (s *Server) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	var body struct {
		PolicyState domain.PolicyState `json:"policy_state"`
		Reason      string             `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if body.PolicyState == "" {
		writeError(w, http.StatusBadRequest, "policy_state is required")
		return
	}

	if !body.PolicyState.IsValid() {
		writeError(w, http.StatusBadRequest, "invalid policy_state: must be ALLOWED, REQUIRES_EXPLICIT_CONSENT, REQUIRES_AUTHORIZATION, or BLOCKED")
		return
	}

	if err := s.policySvc.SetPolicy(r.Context(), id, body.PolicyState, body.Reason); err != nil {
		if errors.Is(err, domain.ErrInvalidPolicyState) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id":  id,
		"policy_state": body.PolicyState,
		"reason":       body.Reason,
	})
}

func (s *Server) handleListLicenses(w http.ResponseWriter, r *http.Request) {
	depName := r.URL.Query().Get("dependency_name")
	list, err := s.licenseSvc.ListManifests(r.Context(), depName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []domain.LicenseManifestEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"licenses":          list,
		"license_manifests": list,
	})
}

func (s *Server) handleRegisterLicense(w http.ResponseWriter, r *http.Request) {
	var body domain.LicenseManifestEntry
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if body.ID == "" {
		body.ID = uuid.NewString()
	}
	if body.CreatedAt.IsZero() {
		body.CreatedAt = time.Now().UTC()
	}

	if err := s.licenseSvc.RegisterManifest(r.Context(), body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"license_manifest": body})
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	list, err := s.credSvc.ListCredentialRefs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []domain.CredentialRef{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": list})
}

func (s *Server) handleRegisterCredential(w http.ResponseWriter, r *http.Request) {
	var body domain.CredentialRef
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if body.ID == "" {
		body.ID = uuid.NewString()
	}
	if body.CreatedAt.IsZero() {
		body.CreatedAt = time.Now().UTC()
	}

	if err := s.credSvc.RegisterCredentialRef(r.Context(), body); err != nil {
		if errors.Is(err, domain.ErrRawSecretForbidden) ||
			errors.Is(err, domain.ErrUnsupportedStorageType) ||
			strings.Contains(err.Error(), "required") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"credential": body, "credential_ref": body})
}

func (s *Server) handleGetLayeredConfig(w http.ResponseWriter, r *http.Request) {
	profStr := r.URL.Query().Get("profile")
	profile := domain.ExecutionProfile(profStr)
	if profile == "" {
		profile = domain.ExecutionProfileHybrid
	}

	var userSettings *domain.LayeredConfig
	retriesStr := r.URL.Query().Get("max_retries")
	if retriesStr != "" {
		if n, err := strconv.Atoi(retriesStr); err == nil {
			if userSettings == nil {
				userSettings = &domain.LayeredConfig{CustomSettings: make(map[string]any)}
			}
			userSettings.MaxRetries = &n
		}
	}

	telemetryStr := r.URL.Query().Get("telemetry_enabled")
	if telemetryStr != "" {
		b := strings.EqualFold(telemetryStr, "true") || telemetryStr == "1"
		if userSettings == nil {
			userSettings = &domain.LayeredConfig{CustomSettings: make(map[string]any)}
		}
		userSettings.Telemetry.Enabled = &b
	}

	zeroOverrunStr := r.URL.Query().Get("zero_overrun_strict")
	if zeroOverrunStr != "" {
		b := strings.EqualFold(zeroOverrunStr, "true") || zeroOverrunStr == "1"
		if userSettings == nil {
			userSettings = &domain.LayeredConfig{CustomSettings: make(map[string]any)}
		}
		userSettings.ZeroOverrunStrict = &b
	}

	resolved := config.ResolveLayeredConfig(profile, userSettings, nil)
	writeJSON(w, http.StatusOK, map[string]any{"config": resolved})
}
