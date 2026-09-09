package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// runtimeHostWriteTimeout bounds the HTTP response-write window. Localhost
// synchronous ML endpoints may run multiple bounded worker invocations
// (each capped by its own per-worker timeout) before the handler first
// writes the response, so this must stay finite yet well above the
// multi-minute worst case. Per-worker timeouts still bound execution.
const runtimeHostWriteTimeout = 30 * time.Minute

type Server struct {
	db             *storage.DB
	casStore       *cas.Store
	ingest         *service.IngestService
	acquisition    *service.AcquisitionService
	registry       *provider.Registry
	policySvc      *governance.PolicyService
	licenseSvc     *governance.LicenseService
	credSvc        *governance.CredentialService
	snapshotSvc    *governance.SnapshotService
	router         *provider.Router
	queueSvc       *queue.Service
	scheduler      *scheduler.Scheduler
	executor       Executor
	speechSvc      *service.SpeechService
	translationSvc *service.TranslationService
	dubbingSvc     *service.DubbingService
	audioMixSvc    *service.AudioMixService
	audioRoleSvc   *service.AudioRoleService
	visualTextSvc  *service.VisualTextService
	renderSvc      *service.RenderService
	reviewSvc      *service.ReviewService
	bundleSvc      *service.BundleService
	mux            *http.ServeMux
	server         *http.Server
}

// Config specifies initialization options for RuntimeHost Server.
type Config struct {
	Addr           string
	DB             *storage.DB
	CASStore       *cas.Store
	Ingest         *service.IngestService
	Acquisition    *service.AcquisitionService // Douyin URL acquisition ladder (T05)
	Registry       *provider.Registry
	PolicySvc      *governance.PolicyService
	LicenseSvc     *governance.LicenseService
	CredSvc        *governance.CredentialService
	Router         *provider.Router
	SnapshotSvc    *governance.SnapshotService // RC executable model snapshot verification service (Issue #64)
	QueueSvc       *queue.Service
	Scheduler      *scheduler.Scheduler
	Executor       Executor                    // Injected execution seam for testing and custom worker dispatch
	SpeechSvc      *service.SpeechService      // Speech understanding pipeline (T08)
	TranslationSvc *service.TranslationService // Translation & Meaning-First Localization pipeline (T06)
	DubbingSvc     *service.DubbingService     // TTS & Measured-Duration Dubbing pipeline (T14)
	AudioMixSvc    *service.AudioMixService    // Audio stems + soundtrack preservation + dialogue-suppression mix (T15)
	AudioRoleSvc   *service.AudioRoleService   // Automatic AudioRolePlan generation (Issue #80)
	VisualTextSvc  *service.VisualTextService  // OCR detection, tracking, and TextRegionPlan (T09)
	RenderSvc      *service.RenderService      // NativeRenderBackend + frozen RenderPlan + preview/final parity (T11)
	ReviewSvc      *service.ReviewService      // Exception-only ReviewItem projection service (T16)
	BundleSvc      *service.BundleService      // Job export/import + bundle integrity (T21)
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
	if cfg.SnapshotSvc == nil && cfg.DB != nil {
		cfg.SnapshotSvc = governance.NewSnapshotService(cfg.DB, cfg.LicenseSvc)
	}
	if cfg.Router != nil && cfg.SnapshotSvc != nil && cfg.Router.SnapshotService() == nil {
		cfg.Router.SetSnapshotService(cfg.SnapshotSvc)
	}
	if cfg.Router == nil && cfg.Registry != nil {
		cfg.Router = provider.NewRouter(cfg.Registry, cfg.PolicySvc, cfg.LicenseSvc, cfg.CredSvc, nil, cfg.DB)
		if cfg.SnapshotSvc != nil {
			cfg.Router.SetSnapshotService(cfg.SnapshotSvc)
		}
	}
	if cfg.QueueSvc == nil && cfg.DB != nil {
		cfg.QueueSvc = queue.NewService(cfg.DB)
	}
	if cfg.Scheduler == nil {
		cfg.Scheduler = scheduler.New()
	}
	if cfg.ReviewSvc == nil && cfg.DB != nil && cfg.CASStore != nil {
		cfg.ReviewSvc = service.NewReviewService(cfg.DB, cfg.CASStore)
	}
	if cfg.BundleSvc == nil && cfg.DB != nil && cfg.CASStore != nil {
		cfg.BundleSvc = service.NewBundleService(cfg.DB, cfg.CASStore, cfg.LicenseSvc)
	}

	// Wire router-backed speech defaults when both are available.
	// Speech service defaults are set here so main.go does not need to
	// manage the SpeechService ↔ Router dependency explicitly.
	if cfg.SpeechSvc != nil && cfg.Router != nil {
		cfg.SpeechSvc.ConfigureRouter(cfg.Router)
	}
	if cfg.TranslationSvc != nil && cfg.Router != nil {
		cfg.TranslationSvc.ConfigureRouter(cfg.Router)
	}
	if cfg.DubbingSvc != nil && cfg.Router != nil {
		cfg.DubbingSvc.ConfigureRouter(cfg.Router)
	}
	if cfg.AudioMixSvc != nil && cfg.Router != nil {
		cfg.AudioMixSvc.ConfigureRouter(cfg.Router)
	}
	if cfg.AudioRoleSvc == nil && cfg.DB != nil && cfg.CASStore != nil {
		cfg.AudioRoleSvc = service.NewAudioRoleService(cfg.DB, cfg.CASStore, cfg.AudioMixSvc)
	}
	if cfg.VisualTextSvc != nil {
		if cfg.Router != nil {
			cfg.VisualTextSvc.ConfigureRouter(cfg.Router)
		}
		if cfg.TranslationSvc != nil {
			cfg.VisualTextSvc.SetTranslationService(cfg.TranslationSvc)
		}
	}
	if cfg.ReviewSvc != nil {
		if cfg.TranslationSvc != nil {
			cfg.ReviewSvc.SetTranslationService(cfg.TranslationSvc)
		}
		if cfg.DubbingSvc != nil {
			cfg.ReviewSvc.SetDubbingService(cfg.DubbingSvc)
		}
		if cfg.AudioMixSvc != nil {
			cfg.ReviewSvc.SetAudioMixService(cfg.AudioMixSvc)
		}
		if cfg.VisualTextSvc != nil {
			cfg.ReviewSvc.SetVisualTextService(cfg.VisualTextSvc)
		}
		if cfg.RenderSvc != nil {
			cfg.ReviewSvc.SetRenderService(cfg.RenderSvc)
		}
	}
	s := &Server{
		db:             cfg.DB,
		casStore:       cfg.CASStore,
		ingest:         cfg.Ingest,
		acquisition:    cfg.Acquisition,
		registry:       cfg.Registry,
		policySvc:      cfg.PolicySvc,
		licenseSvc:     cfg.LicenseSvc,
		credSvc:        cfg.CredSvc,
		router:         cfg.Router,
		snapshotSvc:    cfg.SnapshotSvc,
		queueSvc:       cfg.QueueSvc,
		scheduler:      cfg.Scheduler,
		executor:       cfg.Executor,
		speechSvc:      cfg.SpeechSvc,
		translationSvc: cfg.TranslationSvc,
		dubbingSvc:     cfg.DubbingSvc,
		audioMixSvc:    cfg.AudioMixSvc,
		audioRoleSvc:   cfg.AudioRoleSvc,
		visualTextSvc:  cfg.VisualTextSvc,
		renderSvc:      cfg.RenderSvc,
		reviewSvc:      cfg.ReviewSvc,
		bundleSvc:      cfg.BundleSvc,
		mux:            http.NewServeMux(),
	}
	s.routes()

	s.server = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: runtimeHostWriteTimeout,
	}

	return s
}

// SetExecutor sets or replaces the injected execution seam (e.g. for Seam 1 retry/fallback testing).
func (s *Server) SetExecutor(exec Executor) {
	s.executor = exec
}

// SetSpeechService sets or replaces the injected speech understanding pipeline (T08).
// It also wires router-backed defaults for any hooks the service left nil,
// so a replacement service always gets router-backed defaults regardless of
// the order in which SpeechService, Router, and SetSpeechService are called.
func (s *Server) SetSpeechService(svc *service.SpeechService) {
	s.speechSvc = svc
	if svc != nil && s.router != nil {
		svc.ConfigureRouter(s.router)
	}
}

// SetTranslationService sets or replaces the injected translation pipeline (T06).
func (s *Server) SetTranslationService(svc *service.TranslationService) {
	s.translationSvc = svc
	if svc != nil && s.router != nil {
		svc.ConfigureRouter(s.router)
	}
	if s.reviewSvc != nil {
		s.reviewSvc.SetTranslationService(svc)
	}
}

// SetDubbingService sets or replaces the injected dubbing pipeline (T14).
func (s *Server) SetDubbingService(svc *service.DubbingService) {
	s.dubbingSvc = svc
	if svc != nil && s.router != nil {
		svc.ConfigureRouter(s.router)
	}
	if s.reviewSvc != nil {
		s.reviewSvc.SetDubbingService(svc)
	}
}

// SetAudioMixService sets or replaces the injected audio mix pipeline (T15).
func (s *Server) SetAudioMixService(svc *service.AudioMixService) {
	s.audioMixSvc = svc
	if svc != nil && s.router != nil {
		svc.ConfigureRouter(s.router)
	}
	if s.reviewSvc != nil {
		s.reviewSvc.SetAudioMixService(svc)
	}
}

// SetAudioRoleService sets or replaces the injected audio role service (Issue #80).
func (s *Server) SetAudioRoleService(svc *service.AudioRoleService) {
	s.audioRoleSvc = svc
}

// SetVisualTextService sets or replaces the injected visual text pipeline (T09).
func (s *Server) SetVisualTextService(svc *service.VisualTextService) {
	s.visualTextSvc = svc
	if svc != nil && s.router != nil {
		svc.ConfigureRouter(s.router)
	}
	if s.reviewSvc != nil {
		s.reviewSvc.SetVisualTextService(svc)
	}
}

// SetRenderService sets or replaces the injected render pipeline (T11).
func (s *Server) SetRenderService(svc *service.RenderService) {
	s.renderSvc = svc
	if s.reviewSvc != nil {
		s.reviewSvc.SetRenderService(svc)
	}
}

// SetReviewService sets or replaces the injected review projection service (T16, T19).
func (s *Server) SetReviewService(svc *service.ReviewService) {
	s.reviewSvc = svc
	if svc != nil {
		if s.translationSvc != nil {
			svc.SetTranslationService(s.translationSvc)
		}
		if s.dubbingSvc != nil {
			svc.SetDubbingService(s.dubbingSvc)
		}
		if s.audioMixSvc != nil {
			svc.SetAudioMixService(s.audioMixSvc)
		}
		if s.visualTextSvc != nil {
			svc.SetVisualTextService(s.visualTextSvc)
		}
		if s.renderSvc != nil {
			svc.SetRenderService(s.renderSvc)
		}
	}
}

// SetBundleService sets or replaces the injected bundle service (T21).
func (s *Server) SetBundleService(svc *service.BundleService) {
	s.bundleSvc = svc
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
	s.mux.HandleFunc("POST /api/v1/sources/probe", s.handleProbeSource)
	s.mux.HandleFunc("POST /api/v1/sources/acquire", s.handleAcquireSource)
	s.mux.HandleFunc("GET /api/v1/assets/{id}", s.handleGetAsset)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/preflight", s.handleGetAssetPreflight)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/audio-role-plan", s.handleSaveAudioRolePlan)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/audio-role-plan/generate", s.handleGenerateAudioRolePlan)
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

	// Job Bundle Export & Import (T21)
	s.mux.HandleFunc("POST /api/v1/jobs/{id}/export", s.handleExportJob)
	s.mux.HandleFunc("POST /api/v1/jobs/import", s.handleImportJob)

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
	// Model Snapshots (Issue #64)
	s.mux.HandleFunc("GET /api/v1/snapshots", s.handleListSnapshots)
	s.mux.HandleFunc("POST /api/v1/snapshots/verify", s.handleVerifySnapshot)
	s.mux.HandleFunc("GET /api/v1/snapshots/events", s.handleListSnapshotEvents)
	s.mux.HandleFunc("GET /api/v1/snapshots/{dependency}", s.handleGetSnapshot)
	s.mux.HandleFunc("POST /api/v1/snapshots/{dependency}/reverify", s.handleReverifySnapshot)

	// Credential References
	s.mux.HandleFunc("GET /api/v1/credentials", s.handleListCredentials)
	s.mux.HandleFunc("POST /api/v1/credentials", s.handleRegisterCredential)

	// Layered Configuration
	s.mux.HandleFunc("GET /api/v1/config/layered", s.handleGetLayeredConfig)

	// Speech Understanding (T08: ASR + forced alignment + conditional diarization + canonical SpeechBlocks)
	// The pipeline persists its own immutable TranscriptArtifact; only GET is public.
	s.mux.HandleFunc("POST /api/v1/assets/{id}/speech-understand", s.handleRunSpeechUnderstand)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/transcript", s.handleGetTranscript)

	// Translation (T06: Meaning-First TranslationVariant)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/translate", s.handleRunTranslation)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/translation-variant", s.handleGetTranslationVariant)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/translation", s.handleGetTranslationVariant)

	// Dub Script Adaptation (T13: Duration-Adapted DubScriptVariant)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/dub-script", s.handleRunDubScript)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/dub-script-variant", s.handleGetDubScriptVariant)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/dub-script", s.handleGetDubScriptVariant)

	// Voice Assignment (T14: Frozen per-run VoiceAssignment)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/voice-assignment", s.handleAssignVoices)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/voice-assignment/reassign", s.handleReassignVoices)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/voice-assignment", s.handleGetVoiceAssignment)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/voice-audition", s.handleAuditionVoice)

	// TTS Synthesis & Fit Controller (T14: Measured-duration DubSegmentsVariant)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/dub-synthesize", s.handleRunDubSynthesize)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/dub-segments", s.handleGetDubSegments)
	// Audio Stem Separation & Deterministic Mix (T15)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/separate-stems", s.handleRunSeparateStems)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/audio-stems", s.handleGetAudioStems)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/audio-mix", s.handleRunAudioMix)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/dub-mix", s.handleGetDubMix)
	// Visual Text Detection, Classification & Tracking (T09: TextRegionPlan)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/detect-text", s.handleRunDetectText)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/text-region-plan", s.handleRunDetectText)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/text-region-plan", s.handleGetTextRegionPlan)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/text-regions", s.handleGetTextRegionPlan)
	// Visual Text Localization & Subtitle Track (T10: LocalizedVisualTrack + LocalizedSubtitleTrack)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/visual-track", s.handleLocalizeVisualTrack)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/localized-visual-track", s.handleLocalizeVisualTrack)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/visual-track", s.handleGetLocalizedVisualTrack)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/localized-visual-track", s.handleGetLocalizedVisualTrack)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/subtitle-track", s.handleGetLocalizedSubtitleTrack)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/localized-subtitle-track", s.handleGetLocalizedSubtitleTrack)
	// Render & Preview/Final Parity (T11)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/render-plan", s.handleFreezeRenderPlan)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/render-plan", s.handleGetRenderPlan)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/render/preview", s.handleRenderPreview)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/render/preview", s.handleGetRenderPreview)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/render/final", s.handleRenderFinal)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/render/final", s.handleGetRenderFinal)
	// Exception-only Review Items Projection & Approval Overrides (T16, T19)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/review-items", s.handleGetReviewItems)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/review-items", s.handleGetRunReviewItems)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/review/override", s.handleReviewOverride)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/review/override", s.handleRunReviewOverride)
	s.mux.HandleFunc("POST /api/v1/review-items/{id}/override", s.handleReviewItemDirectOverride)

	// Inspector Target-Text Correction & Targeted Rerun (T19)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/inspector/correct-text", s.handleInspectorCorrectText)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/inspector/correct-text", s.handleRunInspectorCorrectText)

	// Inspector Voice Reassignment & Targeted Rerun (T20)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/inspector/reassign-voice", s.handleInspectorReassignVoice)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/inspector/reassign-voice", s.handleRunInspectorReassignVoice)

	// Inspector Region Override & Targeted Rerun (T20)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/inspector/override-region", s.handleInspectorOverrideRegion)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/inspector/override-region", s.handleRunInspectorOverrideRegion)

	// Final Render Handoff (T20: Auto queue-zero vs Review explicit action)
	s.mux.HandleFunc("POST /api/v1/assets/{id}/render/handoff", s.handleFinalRenderHandoff)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/render/handoff", s.handleRunFinalRenderHandoff)

	// Multimodal Quality Results (T19)
	s.mux.HandleFunc("POST /api/v1/quality-results", s.handleCreateQualityResult)
	s.mux.HandleFunc("GET /api/v1/assets/{id}/quality-results", s.handleGetQualityResults)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/quality-results", s.handleGetRunQualityResults)
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

// writeAcquisitionError maps structural acquisition states and governance
// rejections to stable HTTP statuses. Invalid client input (INVALID_URL) is
// a 400 — never a 500 and never mislabeled as content being gone. Policy/auth/
// license/content-unavailable/unsupported-media rejections are 422
// (fail-closed, never retried past policy); download failures are 502.
func writeAcquisitionError(w http.ResponseWriter, err error) {
	var acqErr *domain.AcquisitionError
	if errors.As(err, &acqErr) {
		status := http.StatusUnprocessableEntity
		switch acqErr.State {
		case domain.AcquisitionDownloadFailed:
			status = http.StatusBadGateway
		case domain.AcquisitionInvalidURL:
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{
			"error":             acqErr.Error(),
			"acquisition_state": string(acqErr.State),
			"provider_id":       acqErr.ProviderID,
		})
		return
	}
	switch {
	case errors.Is(err, domain.ErrInvalidURL):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrPolicyBlocked),
		errors.Is(err, domain.ErrConsentRequired),
		errors.Is(err, domain.ErrAuthRequired),
		errors.Is(err, domain.ErrLicenseManifestMissing),
		errors.Is(err, domain.ErrNoEligibleProvider),
		errors.Is(err, domain.ErrRightsAttestationRequired),
		errors.Is(err, domain.ErrUnsupportedMediaType),
		errors.Is(err, domain.ErrContentUnavailable),
		errors.Is(err, domain.ErrCorruptMedia),
		errors.Is(err, domain.ErrFingerprintMismatch),
		errors.Is(err, domain.ErrInconsistentProvenance):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) handleProbeSource(w http.ResponseWriter, r *http.Request) {
	if s.acquisition == nil {
		writeError(w, http.StatusInternalServerError, "acquisition service is not configured")
		return
	}
	var req service.AcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	desc, err := s.acquisition.Probe(r.Context(), req)
	if err != nil {
		writeAcquisitionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"descriptor": desc})
}

func (s *Server) handleAcquireSource(w http.ResponseWriter, r *http.Request) {
	if s.acquisition == nil {
		writeError(w, http.StatusInternalServerError, "acquisition service is not configured")
		return
	}
	var req service.AcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	res, err := s.acquisition.Acquire(r.Context(), req)
	if err != nil {
		writeAcquisitionError(w, err)
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

func (s *Server) handleGenerateAudioRolePlan(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if _, err := s.db.GetSourceAsset(r.Context(), assetID); err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.audioRoleSvc == nil {
		writeError(w, http.StatusInternalServerError, "audio role service is not configured")
		return
	}

	var body struct {
		RunID            string                  `json:"run_id,omitempty"`
		JobID            string                  `json:"job_id,omitempty"`
		ExecutionProfile domain.ExecutionProfile `json:"execution_profile,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := service.AudioRolePlanInput{
		AssetID:          assetID,
		RunID:            body.RunID,
		JobID:            body.JobID,
		ExecutionProfile: body.ExecutionProfile,
	}
	plan, err := s.audioRoleSvc.GenerateAudioRolePlan(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "generate audio role plan: "+err.Error())
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

func (s *Server) handleRunSpeechUnderstand(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.speechSvc == nil {
		writeError(w, http.StatusInternalServerError, "speech service is not configured")
		return
	}

	var body struct {
		RunID    string `json:"run_id"`
		Language string `json:"language,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if body.RunID == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}

	// Load the audio role plan from the asset's CAS metadata. The pipeline
	// fails closed when the plan is missing (ErrAudioRolePlanRequired).
	var rolePlan *domain.AudioRolePlan
	plan, err := s.db.GetAudioRolePlan(r.Context(), assetID)
	if err == nil {
		rolePlan = plan
	} else if !errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Automatic prerequisite generation on the normal production speech path:
	// If no AudioRolePlan has been pre-seeded or generated for this asset,
	// automatically generate and persist the canonical plan using production
	// source analysis before invoking SpeechService.
	if rolePlan == nil && s.audioRoleSvc != nil {
		genPlan, err := s.audioRoleSvc.GenerateAudioRolePlan(r.Context(), service.AudioRolePlanInput{
			AssetID: assetID,
			RunID:   body.RunID,
		})
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("automatic audio role plan prerequisite generation failed: %v", err))
			return
		}
		rolePlan = genPlan
	}

	// Resolve the source media from the asset's CAS metadata on the RuntimeHost
	// side. This is the original source-media object (possibly video), not a
	// normalized WAV — audio extraction/normalization is the worker adapter's
	// responsibility. The localhost API never trusts client-supplied paths.
	if strings.TrimSpace(asset.CASPath) == "" {
		writeError(w, http.StatusUnprocessableEntity, "asset has no source media in CAS to process")
		return
	}
	in := domain.SpeechPipelineInput{
		RunID:         body.RunID,
		AssetID:       assetID,
		AudioPath:     asset.CASPath,
		AudioSHA256:   asset.SHA256,
		AudioRolePlan: rolePlan,
	}
	artifact, err := s.speechSvc.RunPipeline(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrNoDubEligibleSpeech) ||
			errors.Is(err, domain.ErrAudioRolePlanRequired) ||
			errors.Is(err, domain.ErrNoEligibleProvider) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"transcript_artifact": artifact})
}

func (s *Server) handleGetTranscript(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	idx, err := s.db.GetTranscriptArtifactIndex(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "transcript not found for asset")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.casStore == nil {
		writeError(w, http.StatusInternalServerError, "CAS store not configured")
		return
	}
	rc, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read transcript artifact from CAS: "+err.Error())
		return
	}
	defer rc.Close()
	var artifact domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&artifact); err != nil {
		writeError(w, http.StatusInternalServerError, "decode transcript artifact: "+err.Error())
		return
	}
	artifact.CASHash = idx.CASHash
	artifact.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"transcript_artifact": artifact})
}
func (s *Server) handleRunTranslation(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.translationSvc == nil {
		writeError(w, http.StatusInternalServerError, "translation service is not configured")
		return
	}

	var body struct {
		RunID                 string                           `json:"run_id"`
		JobID                 string                           `json:"job_id,omitempty"`
		TargetLanguage        string                           `json:"target_language"`
		SourceLanguage        string                           `json:"source_language,omitempty"`
		Segments              []domain.TranslationInputSegment `json:"segments,omitempty"`
		ExecutionProfile      domain.ExecutionProfile          `json:"execution_profile,omitempty"`
		AuthorizedCredentials []string                         `json:"authorized_credentials,omitempty"`
		ConsentGranted        bool                             `json:"consent_granted,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if strings.TrimSpace(body.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(body.TargetLanguage) == "" {
		writeError(w, http.StatusBadRequest, "target_language is required")
		return
	}

	in := domain.TranslationJobInput{
		RunID:                 body.RunID,
		AssetID:               asset.ID,
		JobID:                 body.JobID,
		SourceLanguage:        body.SourceLanguage,
		TargetLanguage:        body.TargetLanguage,
		Segments:              body.Segments,
		ExecutionProfile:      body.ExecutionProfile,
		AuthorizedCredentials: body.AuthorizedCredentials,
		ConsentGranted:        body.ConsentGranted,
	}

	variant, err := s.translationSvc.Translate(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrMeaningPreservationFailed) ||
			errors.Is(err, domain.ErrFactCorrupted) ||
			errors.Is(err, domain.ErrNameCorrupted) ||
			errors.Is(err, domain.ErrNumberCorrupted) ||
			errors.Is(err, domain.ErrNegationInverted) {
			writeError(w, http.StatusUnprocessableEntity, "translation QA gate rejected: "+err.Error())
			return
		}
		if errors.Is(err, domain.ErrEmptyTranslationInput) {
			writeError(w, http.StatusBadRequest, "empty translation input: "+err.Error())
			return
		}
		if errors.Is(err, domain.ErrNoEligibleProvider) || strings.Contains(err.Error(), "no eligible provider") {
			writeError(w, http.StatusServiceUnavailable, "no eligible provider: "+err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"translation_variant": variant})
}

func (s *Server) handleGetTranslationVariant(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi" // default target language
	}

	idx, err := s.db.GetTranslationVariantIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("translation variant not found for asset %s in language %s", assetID, targetLang))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.casStore == nil {
		writeError(w, http.StatusInternalServerError, "CAS store not configured")
		return
	}
	rc, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read translation variant from CAS: "+err.Error())
		return
	}
	defer rc.Close()
	var variant domain.TranslationVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		writeError(w, http.StatusInternalServerError, "decode translation variant: "+err.Error())
		return
	}
	variant.CASHash = idx.CASHash
	variant.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"translation_variant": variant})
}

func (s *Server) handleRunDubScript(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.translationSvc == nil {
		writeError(w, http.StatusInternalServerError, "translation service is not configured")
		return
	}

	var body struct {
		RunID                 string                  `json:"run_id"`
		JobID                 string                  `json:"job_id,omitempty"`
		TargetLanguage        string                  `json:"target_language"`
		SourceLanguage        string                  `json:"source_language,omitempty"`
		TranslationVariantCAS string                  `json:"translation_variant_cas,omitempty"`
		ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
		AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if strings.TrimSpace(body.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(body.TargetLanguage) == "" {
		writeError(w, http.StatusBadRequest, "target_language is required")
		return
	}

	in := domain.DubScriptJobInput{
		RunID:                 body.RunID,
		AssetID:               asset.ID,
		JobID:                 body.JobID,
		SourceLanguage:        body.SourceLanguage,
		TargetLanguage:        body.TargetLanguage,
		TranslationVariantCAS: body.TranslationVariantCAS,
		ExecutionProfile:      body.ExecutionProfile,
		AuthorizedCredentials: body.AuthorizedCredentials,
	}

	variant, err := s.translationSvc.AdaptDubScript(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrNoDubbingRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "No dubbing required"})
			return
		}
		if errors.Is(err, domain.ErrAudioRolePlanRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		if errors.Is(err, domain.ErrMeaningPreservationFailed) ||
			errors.Is(err, domain.ErrFactCorrupted) ||
			errors.Is(err, domain.ErrNameCorrupted) ||
			errors.Is(err, domain.ErrNumberCorrupted) ||
			errors.Is(err, domain.ErrNegationInverted) {
			writeError(w, http.StatusUnprocessableEntity, "dub script QA gate rejected: "+err.Error())
			return
		}
		if errors.Is(err, domain.ErrDubScriptVariantNotFound) || errors.Is(err, domain.ErrTranslationVariantNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"dub_script_variant": variant})
}

func (s *Server) handleGetDubScriptVariant(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi" // default target language
	}

	idx, err := s.db.GetDubScriptVariantIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("dub script variant not found for asset %s in language %s", assetID, targetLang))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.casStore == nil {
		writeError(w, http.StatusInternalServerError, "CAS store not configured")
		return
	}
	rc, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read dub script variant from CAS: "+err.Error())
		return
	}
	defer rc.Close()
	var variant domain.DubScriptVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		writeError(w, http.StatusInternalServerError, "decode dub script variant: "+err.Error())
		return
	}
	variant.CASHash = idx.CASHash
	variant.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"dub_script_variant": variant})
}

func (s *Server) handleAssignVoices(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.dubbingSvc == nil {
		writeError(w, http.StatusInternalServerError, "dubbing service is not configured")
		return
	}

	var body struct {
		RunID              string                         `json:"run_id"`
		JobID              string                         `json:"job_id,omitempty"`
		TargetLanguage     string                         `json:"target_language"`
		CustomAssignments  map[string]domain.VoiceProfile `json:"custom_assignments,omitempty"`
		UseSameVoiceForAll bool                           `json:"use_same_voice_for_all"`
		ExecutionProfile   domain.ExecutionProfile        `json:"execution_profile,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if strings.TrimSpace(body.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(body.TargetLanguage) == "" {
		writeError(w, http.StatusBadRequest, "target_language is required")
		return
	}

	in := domain.VoiceAssignmentInput{
		RunID:              body.RunID,
		AssetID:            asset.ID,
		JobID:              body.JobID,
		TargetLanguage:     body.TargetLanguage,
		CustomAssignments:  body.CustomAssignments,
		UseSameVoiceForAll: body.UseSameVoiceForAll,
		ExecutionProfile:   body.ExecutionProfile,
	}

	assignment, err := s.dubbingSvc.AssignVoices(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrNoDubbingRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "No dubbing required"})
			return
		}
		if errors.Is(err, domain.ErrAudioRolePlanRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		if errors.Is(err, domain.ErrNoEligibleTTSProvider) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if errors.Is(err, domain.ErrVoiceAssignmentFrozen) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"voice_assignment": assignment})
}

func (s *Server) handleReassignVoices(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.dubbingSvc == nil {
		writeError(w, http.StatusInternalServerError, "dubbing service is not configured")
		return
	}

	var body struct {
		RunID              string                         `json:"run_id"`
		JobID              string                         `json:"job_id,omitempty"`
		TargetLanguage     string                         `json:"target_language"`
		CustomAssignments  map[string]domain.VoiceProfile `json:"custom_assignments,omitempty"`
		UseSameVoiceForAll bool                           `json:"use_same_voice_for_all"`
		ExecutionProfile   domain.ExecutionProfile        `json:"execution_profile,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if strings.TrimSpace(body.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(body.TargetLanguage) == "" {
		writeError(w, http.StatusBadRequest, "target_language is required")
		return
	}

	in := domain.VoiceAssignmentInput{
		RunID:              body.RunID,
		AssetID:            asset.ID,
		JobID:              body.JobID,
		TargetLanguage:     body.TargetLanguage,
		CustomAssignments:  body.CustomAssignments,
		UseSameVoiceForAll: body.UseSameVoiceForAll,
		ExecutionProfile:   body.ExecutionProfile,
	}

	assignment, err := s.dubbingSvc.ReassignVoice(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrNoDubbingRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "No dubbing required"})
			return
		}
		if errors.Is(err, domain.ErrVoiceAssignmentNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no frozen voice assignment found for run")
			return
		}
		if errors.Is(err, domain.ErrNoEligibleTTSProvider) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voice_assignment": assignment})
}

func (s *Server) handleGetVoiceAssignment(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi" // default target language
	}

	idx, err := s.db.GetVoiceAssignmentIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("voice assignment not found for asset %s in language %s", assetID, targetLang))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.casStore == nil {
		writeError(w, http.StatusInternalServerError, "CAS store not configured")
		return
	}
	rc, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read voice assignment from CAS: "+err.Error())
		return
	}
	defer rc.Close()
	var assignment domain.VoiceAssignment
	if err := json.NewDecoder(rc).Decode(&assignment); err != nil {
		writeError(w, http.StatusInternalServerError, "decode voice assignment: "+err.Error())
		return
	}
	assignment.CASHash = idx.CASHash
	assignment.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"voice_assignment": assignment})
}

func (s *Server) handleAuditionVoice(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.db != nil {
		_, err := s.db.GetSourceAsset(r.Context(), assetID)
		if err != nil {
			if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
				writeError(w, http.StatusNotFound, "asset not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if s.dubbingSvc == nil {
		writeError(w, http.StatusInternalServerError, "dubbing service is not configured")
		return
	}
	var body struct {
		RunID          string              `json:"run_id"`
		TargetLanguage string              `json:"target_language"`
		Voice          domain.VoiceProfile `json:"voice"`
		SampleText     string              `json:"sample_text,omitempty"`
		IsContextual   bool                `json:"is_contextual"`
		SegmentIndex   int                 `json:"segment_index,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := domain.VoiceAuditionInput{
		RunID:          body.RunID,
		AssetID:        assetID,
		TargetLanguage: body.TargetLanguage,
		Voice:          body.Voice,
		SampleText:     body.SampleText,
		IsContextual:   body.IsContextual,
		SegmentIndex:   body.SegmentIndex,
	}

	res, err := s.dubbingSvc.AuditionVoice(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrNoDubbingRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "No dubbing required"})
			return
		}
		if errors.Is(err, domain.ErrAudioRolePlanRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		if errors.Is(err, domain.ErrNoEligibleTTSProvider) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"audition_result": res})
}

func (s *Server) handleRunDubSynthesize(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.dubbingSvc == nil {
		writeError(w, http.StatusInternalServerError, "dubbing service is not configured")
		return
	}

	var body struct {
		RunID                 string                  `json:"run_id"`
		JobID                 string                  `json:"job_id,omitempty"`
		TargetLanguage        string                  `json:"target_language"`
		DubScriptVariantCAS   string                  `json:"dub_script_variant_cas,omitempty"`
		VoiceAssignmentCAS    string                  `json:"voice_assignment_cas,omitempty"`
		ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
		AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if strings.TrimSpace(body.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(body.TargetLanguage) == "" {
		writeError(w, http.StatusBadRequest, "target_language is required")
		return
	}

	in := domain.DubbingJobInput{
		RunID:                 body.RunID,
		AssetID:               asset.ID,
		JobID:                 body.JobID,
		TargetLanguage:        body.TargetLanguage,
		DubScriptVariantCAS:   body.DubScriptVariantCAS,
		VoiceAssignmentCAS:    body.VoiceAssignmentCAS,
		ExecutionProfile:      body.ExecutionProfile,
		AuthorizedCredentials: body.AuthorizedCredentials,
	}

	variant, err := s.dubbingSvc.SynthesizeAndFit(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrNoDubbingRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "No dubbing required"})
			return
		}
		if errors.Is(err, domain.ErrAudioRolePlanRequired) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		if errors.Is(err, domain.ErrDubScriptRequiredForDubbing) || errors.Is(err, domain.ErrVoiceAssignmentRequired) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, domain.ErrNoEligibleTTSProvider) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"dub_segments_variant": variant})
}

func (s *Server) handleGetDubSegments(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi" // default target language
	}

	idx, err := s.db.GetDubSegmentsVariantIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("dub segments variant not found for asset %s in language %s", assetID, targetLang))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.casStore == nil {
		writeError(w, http.StatusInternalServerError, "CAS store not configured")
		return
	}
	rc, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read dub segments variant from CAS: "+err.Error())
		return
	}
	defer rc.Close()
	var variant domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		writeError(w, http.StatusInternalServerError, "decode dub segments variant: "+err.Error())
		return
	}
	variant.CASHash = idx.CASHash
	variant.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"dub_segments_variant": variant})
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

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.snapshotSvc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"bindings": []any{}})
		return
	}
	bindings := s.snapshotSvc.ListBindings()
	writeJSON(w, http.StatusOK, map[string]any{
		"bindings": bindings,
	})
}

func (s *Server) handleVerifySnapshot(w http.ResponseWriter, r *http.Request) {
	if s.snapshotSvc == nil {
		writeError(w, http.StatusInternalServerError, "snapshot service not initialized")
		return
	}
	var body struct {
		Manifest  domain.SnapshotManifest `json:"manifest"`
		LocalPath string                  `json:"local_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	binding, err := s.snapshotSvc.RegisterAndVerifySnapshot(r.Context(), body.Manifest, body.LocalPath)
	if err != nil {
		if errors.Is(err, domain.ErrLicenseManifestMissing) ||
			errors.Is(err, domain.ErrSnapshotDigestMismatch) ||
			errors.Is(err, domain.ErrSnapshotFileCorrupted) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":  "verified",
		"binding": binding,
	})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.snapshotSvc == nil {
		writeError(w, http.StatusInternalServerError, "snapshot service not initialized")
		return
	}
	dep := r.PathValue("dependency")
	ver := r.URL.Query().Get("version")
	evidence, err := s.snapshotSvc.GetPortableEvidence(r.Context(), dep, ver)
	if err != nil {
		if errors.Is(err, domain.ErrSnapshotUnverified) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"evidence": evidence,
	})
}

func (s *Server) handleReverifySnapshot(w http.ResponseWriter, r *http.Request) {
	if s.snapshotSvc == nil {
		writeError(w, http.StatusInternalServerError, "snapshot service not initialized")
		return
	}
	dep := r.PathValue("dependency")
	ver := r.URL.Query().Get("version")
	binding, err := s.snapshotSvc.Reverify(r.Context(), dep, ver)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "reverified",
		"binding": binding,
	})
}

func (s *Server) handleListSnapshotEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}})
		return
	}
	depName := r.URL.Query().Get("dependency_name")
	events, err := s.db.ListSnapshotVerificationEvents(r.Context(), depName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []domain.SnapshotVerificationEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
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

func (s *Server) handleRunSeparateStems(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.audioMixSvc == nil {
		writeError(w, http.StatusInternalServerError, "audio mix service is not configured")
		return
	}

	var body struct {
		RunID            string                  `json:"run_id"`
		JobID            string                  `json:"job_id,omitempty"`
		ExecutionProfile domain.ExecutionProfile `json:"execution_profile,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := service.AudioSeparationInput{
		RunID:            body.RunID,
		AssetID:          asset.ID,
		JobID:            body.JobID,
		ExecutionProfile: body.ExecutionProfile,
	}

	stems, err := s.audioMixSvc.SeparateAudio(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"audio_stems": stems})
}

func (s *Server) handleGetAudioStems(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	idx, err := s.db.GetAudioStemsArtifactIndex(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "audio stems not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load audio stems from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var stems domain.AudioStemArtifacts
	if err := json.NewDecoder(reader).Decode(&stems); err != nil {
		writeError(w, http.StatusInternalServerError, "decode audio stems: "+err.Error())
		return
	}
	stems.CASHash = idx.CASHash
	stems.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"audio_stems": stems})
}

func (s *Server) handleRunAudioMix(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.audioMixSvc == nil {
		writeError(w, http.StatusInternalServerError, "audio mix service is not configured")
		return
	}

	var body struct {
		RunID                 string                  `json:"run_id"`
		JobID                 string                  `json:"job_id,omitempty"`
		TargetLanguage        string                  `json:"target_language"`
		DubSegmentsCAS        string                  `json:"dub_segments_cas,omitempty"`
		AudioStemsCAS         string                  `json:"audio_stems_cas,omitempty"`
		CrossfadeDurationMs   int64                   `json:"crossfade_duration_ms,omitempty"`
		DuckingGainDb         float64                 `json:"ducking_gain_db,omitempty"`
		PreserveSinging       bool                    `json:"preserve_singing"`
		ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
		AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if strings.TrimSpace(body.RunID) == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(body.TargetLanguage) == "" {
		writeError(w, http.StatusBadRequest, "target_language is required")
		return
	}

	in := service.AudioMixInput{
		RunID:                 body.RunID,
		AssetID:               asset.ID,
		JobID:                 body.JobID,
		TargetLanguage:        body.TargetLanguage,
		DubSegmentsCAS:        body.DubSegmentsCAS,
		AudioStemsCAS:         body.AudioStemsCAS,
		CrossfadeDurationMs:   body.CrossfadeDurationMs,
		DuckingGainDb:         body.DuckingGainDb,
		PreserveSinging:       body.PreserveSinging,
		ExecutionProfile:      body.ExecutionProfile,
		AuthorizedCredentials: body.AuthorizedCredentials,
	}

	mixArtifact, err := s.audioMixSvc.MixAudio(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrMixerOverrunRefused) || errors.Is(err, domain.ErrMixerAnchorDrift) {
			// Refused outcome observable with 422 or 400
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":   err.Error(),
				"dub_mix": mixArtifact,
				"status":  "REFUSED",
			})
			return
		}
		if errors.Is(err, domain.ErrAudioRolePlanRequired) || errors.Is(err, domain.ErrInvalidTargetLanguage) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"dub_mix": mixArtifact})
}

func (s *Server) handleGetDubMix(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}

	idx, err := s.db.GetDubMixArtifactIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "dub mix not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load dub mix from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var mix domain.DubMixArtifact
	if err := json.NewDecoder(reader).Decode(&mix); err != nil {
		writeError(w, http.StatusInternalServerError, "decode dub mix: "+err.Error())
		return
	}
	mix.CASHash = idx.CASHash
	mix.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"dub_mix": mix})
}

func (s *Server) handleRunDetectText(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.visualTextSvc == nil {
		writeError(w, http.StatusInternalServerError, "visual text service is not configured")
		return
	}

	var body struct {
		RunID                 string                  `json:"run_id"`
		JobID                 string                  `json:"job_id,omitempty"`
		FrameSampleStepMs     int64                   `json:"frame_sample_step_ms,omitempty"`
		MaxFrames             int                     `json:"max_frames,omitempty"`
		ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
		AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := service.VisualTextDetectionInput{
		RunID:                 body.RunID,
		AssetID:               asset.ID,
		JobID:                 body.JobID,
		FrameSampleStepMs:     body.FrameSampleStepMs,
		MaxFrames:             body.MaxFrames,
		ExecutionProfile:      body.ExecutionProfile,
		AuthorizedCredentials: body.AuthorizedCredentials,
	}

	plan, err := s.visualTextSvc.DetectAndTrackText(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"text_region_plan": plan})
}

func (s *Server) handleGetTextRegionPlan(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	idx, err := s.db.GetTextRegionPlanIndex(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "text region plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load text region plan from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var plan domain.TextRegionPlan
	if err := json.NewDecoder(reader).Decode(&plan); err != nil {
		writeError(w, http.StatusInternalServerError, "decode text region plan: "+err.Error())
		return
	}
	plan.CASHash = idx.CASHash
	plan.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"text_region_plan": plan})
}

func (s *Server) handleLocalizeVisualTrack(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	asset, err := s.db.GetSourceAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, domain.ErrAssetNotFound) || errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "asset not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.visualTextSvc == nil {
		writeError(w, http.StatusInternalServerError, "visual text service is not configured")
		return
	}

	var body struct {
		RunID                 string                        `json:"run_id"`
		JobID                 string                        `json:"job_id,omitempty"`
		TargetLanguage        string                        `json:"target_language"`
		TranslationVariantCAS string                        `json:"translation_variant_cas,omitempty"`
		Overrides             []domain.RegionOverride       `json:"overrides,omitempty"`
		InpaintingFallbacks   []string                      `json:"inpainting_fallbacks,omitempty"`
		SceneProtectedRegions []domain.SceneProtectedRegion `json:"scene_protected_regions,omitempty"`
		ExecutionProfile      domain.ExecutionProfile       `json:"execution_profile,omitempty"`
		AuthorizedCredentials []string                      `json:"authorized_credentials,omitempty"`
		ConsentGranted        bool                          `json:"consent_granted,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	targetLang := strings.ToLower(strings.TrimSpace(body.TargetLanguage))
	if targetLang == "" {
		targetLang = domain.TargetLanguageVI
	}

	in := service.LocalizeVisualTrackInput{
		RunID:                 body.RunID,
		AssetID:               asset.ID,
		JobID:                 body.JobID,
		TargetLanguage:        targetLang,
		TranslationVariantCAS: body.TranslationVariantCAS,
		Overrides:             body.Overrides,
		InpaintingFallbacks:   body.InpaintingFallbacks,
		SceneProtectedRegions: body.SceneProtectedRegions,
		ExecutionProfile:      body.ExecutionProfile,
		AuthorizedCredentials: body.AuthorizedCredentials,
		ConsentGranted:        body.ConsentGranted,
	}

	track, err := s.visualTextSvc.LocalizeVisualTrack(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrTextRegionPlanNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRegionOverrideInvalid) || errors.Is(err, domain.ErrInvalidTargetLanguage) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, domain.ErrSubtitleOverlapsProtectedRegion) || errors.Is(err, domain.ErrTranslationFailed) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"localized_visual_track": track})
}

func (s *Server) handleGetLocalizedVisualTrack(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("target_language")))
	if targetLang == "" {
		targetLang = domain.TargetLanguageVI
	}

	idx, err := s.db.GetLocalizedVisualTrackIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "localized visual track not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load localized visual track from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var track domain.LocalizedVisualTrack
	if err := json.NewDecoder(reader).Decode(&track); err != nil {
		writeError(w, http.StatusInternalServerError, "decode localized visual track: "+err.Error())
		return
	}
	track.CASHash = idx.CASHash
	track.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"localized_visual_track": track})
}

func (s *Server) handleGetLocalizedSubtitleTrack(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("target_language")))
	if targetLang == "" {
		targetLang = domain.TargetLanguageVI
	}

	idx, err := s.db.GetLocalizedSubtitleTrackIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "localized subtitle track not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load localized subtitle track from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var track domain.LocalizedSubtitleTrack
	if err := json.NewDecoder(reader).Decode(&track); err != nil {
		writeError(w, http.StatusInternalServerError, "decode localized subtitle track: "+err.Error())
		return
	}
	track.CASHash = idx.CASHash
	track.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"localized_subtitle_track": track})
}

// Render endpoints (T11)

func (s *Server) handleFreezeRenderPlan(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.renderSvc == nil {
		writeError(w, http.StatusInternalServerError, "render service is not configured")
		return
	}

	var body struct {
		RunID                  string               `json:"run_id"`
		JobID                  string               `json:"job_id,omitempty"`
		TargetLanguage         string               `json:"target_language"`
		DubMixCAS              string               `json:"dub_mix_cas,omitempty"`
		SubtitlePlanCAS        string               `json:"subtitle_plan_cas,omitempty"`
		SubtitlePlanArtifactID string               `json:"subtitle_plan_artifact_id,omitempty"`
		SubtitleCues           []domain.SubtitleCue `json:"subtitle_cues,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := service.RenderPlanInput{
		RunID:                  body.RunID,
		JobID:                  body.JobID,
		AssetID:                assetID,
		TargetLanguage:         body.TargetLanguage,
		DubMixCAS:              body.DubMixCAS,
		SubtitlePlanCAS:        body.SubtitlePlanCAS,
		SubtitlePlanArtifactID: body.SubtitlePlanArtifactID,
		SubtitleCues:           body.SubtitleCues,
	}

	plan, err := s.renderSvc.FreezeRenderPlan(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrRenderSourceNotFound) || errors.Is(err, domain.ErrDubMixNotRenderable) || errors.Is(err, domain.ErrSubtitlePlanNotFound) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRenderSourceNotFound) || errors.Is(err, domain.ErrDubMixNotRenderable) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"render_plan": plan})
}

func (s *Server) handleGetRenderPlan(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}

	idx, err := s.db.GetRenderPlanIndex(r.Context(), assetID, targetLang)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "render plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load render plan from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var plan domain.RenderPlan
	if err := json.NewDecoder(reader).Decode(&plan); err != nil {
		writeError(w, http.StatusInternalServerError, "decode render plan: "+err.Error())
		return
	}
	plan.CASHash = idx.CASHash
	plan.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"render_plan": plan})
}

func (s *Server) handleRenderPreview(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.renderSvc == nil {
		writeError(w, http.StatusInternalServerError, "render service is not configured")
		return
	}

	var body struct {
		RunID          string `json:"run_id"`
		JobID          string `json:"job_id,omitempty"`
		TargetLanguage string `json:"target_language"`
		PlanProvenance string `json:"plan_provenance,omitempty"`
		PlanCAS        string `json:"plan_cas,omitempty"`
		FontFile       string `json:"font_file,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := service.RenderExecutionInput{
		RunID:          body.RunID,
		JobID:          body.JobID,
		AssetID:        assetID,
		TargetLanguage: body.TargetLanguage,
		PlanProvenance: body.PlanProvenance,
		PlanCAS:        body.PlanCAS,
		FontFile:       body.FontFile,
	}

	artifact, err := s.renderSvc.RenderPreview(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrRenderPlanNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRenderPlanInvalid) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRenderSourceNotFound) || errors.Is(err, domain.ErrRenderBackendUnavailable) || errors.Is(err, domain.ErrSubtitlePlanNotFound) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"preview_render": artifact})
}

func (s *Server) handleGetRenderPreview(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}

	idx, err := s.db.GetLatestRenderArtifactIndex(r.Context(), assetID, targetLang, domain.RenderKindPreview)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "preview render artifact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load preview render from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var art domain.PreviewRenderArtifact
	if err := json.NewDecoder(reader).Decode(&art); err != nil {
		writeError(w, http.StatusInternalServerError, "decode preview render: "+err.Error())
		return
	}
	art.CASHash = idx.CASHash
	art.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"preview_render": art})
}

func (s *Server) handleRenderFinal(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.renderSvc == nil {
		writeError(w, http.StatusInternalServerError, "render service is not configured")
		return
	}

	var body struct {
		RunID          string `json:"run_id"`
		JobID          string `json:"job_id,omitempty"`
		TargetLanguage string `json:"target_language"`
		PlanProvenance string `json:"plan_provenance,omitempty"`
		PlanCAS        string `json:"plan_cas,omitempty"`
		FontFile       string `json:"font_file,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in := service.RenderExecutionInput{
		RunID:          body.RunID,
		JobID:          body.JobID,
		AssetID:        assetID,
		TargetLanguage: body.TargetLanguage,
		PlanProvenance: body.PlanProvenance,
		PlanCAS:        body.PlanCAS,
		FontFile:       body.FontFile,
	}

	artifact, err := s.renderSvc.RenderFinal(r.Context(), in)
	if err != nil {
		if errors.Is(err, domain.ErrRenderPlanNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRenderPlanInvalid) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRenderSourceNotFound) || errors.Is(err, domain.ErrRenderBackendUnavailable) || errors.Is(err, domain.ErrSubtitlePlanNotFound) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"final_render": artifact})
}

func (s *Server) handleGetRenderFinal(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}

	idx, err := s.db.GetLatestRenderArtifactIndex(r.Context(), assetID, targetLang, domain.RenderKindFinal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "final render artifact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reader, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load final render from cas: "+err.Error())
		return
	}
	defer reader.Close()

	var art domain.FinalRenderArtifact
	if err := json.NewDecoder(reader).Decode(&art); err != nil {
		writeError(w, http.StatusInternalServerError, "decode final render: "+err.Error())
		return
	}
	art.CASHash = idx.CASHash
	art.ProvenanceHash = idx.ProvenanceHash
	writeJSON(w, http.StatusOK, map[string]any{"final_render": art})
}

// ReviewItem projection handlers (T16)

func (s *Server) handleGetReviewItems(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}
	includeAll := strings.EqualFold(r.URL.Query().Get("include_resolved"), "true") || strings.EqualFold(r.URL.Query().Get("all"), "true")

	var items []domain.ReviewItem
	var err error
	if includeAll {
		items, err = s.reviewSvc.ProjectAllReviewItems(r.Context(), assetID, targetLang)
	} else {
		items, err = s.reviewSvc.ProjectReviewItems(r.Context(), assetID, targetLang)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []domain.ReviewItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asset_id":        assetID,
		"target_language": targetLang,
		"review_items":    items,
		"count":           len(items),
	})
}

func (s *Server) handleGetRunReviewItems(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}
	run, err := s.db.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	job, err := s.db.GetJob(r.Context(), run.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	includeAll := strings.EqualFold(r.URL.Query().Get("include_resolved"), "true") || strings.EqualFold(r.URL.Query().Get("all"), "true")

	var items []domain.ReviewItem
	if includeAll {
		items, err = s.reviewSvc.ProjectAllReviewItems(r.Context(), job.SourceAssetID, job.TargetLanguage)
	} else {
		items, err = s.reviewSvc.ProjectReviewItems(r.Context(), job.SourceAssetID, job.TargetLanguage)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []domain.ReviewItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":          runID,
		"asset_id":        job.SourceAssetID,
		"target_language": job.TargetLanguage,
		"review_items":    items,
		"count":           len(items),
	})
}

func (s *Server) handleReviewOverride(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	var in service.ManualOverrideInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.AssetID = assetID
	override, err := s.reviewSvc.RecordManualOverride(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":   "accepted",
		"override": override,
	})
}

func (s *Server) handleRunReviewOverride(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.reviewSvc == nil || s.db == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	run, err := s.db.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	job, err := s.db.GetJob(r.Context(), run.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var in service.ManualOverrideInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.RunID = runID
	in.JobID = job.ID
	in.AssetID = job.SourceAssetID
	if in.TargetLanguage == "" {
		in.TargetLanguage = job.TargetLanguage
	}

	override, err := s.reviewSvc.RecordManualOverride(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":   "accepted",
		"override": override,
	})
}

func (s *Server) handleReviewItemDirectOverride(w http.ResponseWriter, r *http.Request) {
	itemID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	var in service.ManualOverrideInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.ReviewItemID = itemID
	override, err := s.reviewSvc.RecordManualOverride(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":   "accepted",
		"override": override,
	})
}

func (s *Server) handleInspectorCorrectText(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	var in service.TargetTextCorrectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.AssetID = assetID
	result, err := s.reviewSvc.CorrectTargetText(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrTranslationVariantNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

func (s *Server) handleRunInspectorCorrectText(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.reviewSvc == nil || s.db == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	run, err := s.db.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	job, err := s.db.GetJob(r.Context(), run.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var in service.TargetTextCorrectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.RunID = runID
	in.JobID = job.ID
	in.AssetID = job.SourceAssetID
	if in.TargetLanguage == "" {
		in.TargetLanguage = job.TargetLanguage
	}

	result, err := s.reviewSvc.CorrectTargetText(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrTranslationVariantNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

func (s *Server) handleInspectorReassignVoice(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	var in service.VoiceReassignCorrectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.AssetID = assetID
	result, err := s.reviewSvc.ReassignVoice(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrVoiceAssignmentNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

func (s *Server) handleRunInspectorReassignVoice(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.reviewSvc == nil || s.db == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	run, err := s.db.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	job, err := s.db.GetJob(r.Context(), run.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var in service.VoiceReassignCorrectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.RunID = runID
	in.JobID = job.ID
	in.AssetID = job.SourceAssetID
	if in.TargetLanguage == "" {
		in.TargetLanguage = job.TargetLanguage
	}

	result, err := s.reviewSvc.ReassignVoice(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrVoiceAssignmentNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

func (s *Server) handleInspectorOverrideRegion(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	var in service.RegionGeometryCorrectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.AssetID = assetID
	result, err := s.reviewSvc.CorrectRegionGeometry(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrTextRegionPlanNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRegionOverrideInvalid) || errors.Is(err, domain.ErrSubtitleOverlapsProtectedRegion) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

func (s *Server) handleRunInspectorOverrideRegion(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.reviewSvc == nil || s.db == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	run, err := s.db.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	job, err := s.db.GetJob(r.Context(), run.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var in service.RegionGeometryCorrectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	in.RunID = runID
	in.JobID = job.ID
	in.AssetID = job.SourceAssetID
	if in.TargetLanguage == "" {
		in.TargetLanguage = job.TargetLanguage
	}

	result, err := s.reviewSvc.CorrectRegionGeometry(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrTextRegionPlanNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, domain.ErrRegionOverrideInvalid) || errors.Is(err, domain.ErrSubtitleOverlapsProtectedRegion) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}

func (s *Server) handleFinalRenderHandoff(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.reviewSvc == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	var in domain.FinalRenderHandoffInput
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
			return
		}
	}
	in.AssetID = assetID
	if in.TargetLanguage == "" {
		in.TargetLanguage = r.URL.Query().Get("target_language")
	}
	if in.Posture == "" {
		in.Posture = domain.ReviewPosture(r.URL.Query().Get("posture"))
	}
	if in.RunID == "" {
		in.RunID = r.URL.Query().Get("run_id")
	}
	if in.JobID == "" {
		in.JobID = r.URL.Query().Get("job_id")
	}

	result, err := s.reviewSvc.EvaluateFinalRenderHandoff(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"handoff": result,
	})
}

func (s *Server) handleRunFinalRenderHandoff(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.reviewSvc == nil || s.db == nil {
		writeError(w, http.StatusInternalServerError, "review service is not configured")
		return
	}

	run, err := s.db.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	job, err := s.db.GetJob(r.Context(), run.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var in domain.FinalRenderHandoffInput
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
			return
		}
	}
	in.RunID = runID
	in.JobID = job.ID
	in.AssetID = job.SourceAssetID
	if in.TargetLanguage == "" {
		in.TargetLanguage = job.TargetLanguage
	}
	if in.Posture == "" {
		in.Posture = domain.ReviewPosture(r.URL.Query().Get("posture"))
	}

	result, err := s.reviewSvc.EvaluateFinalRenderHandoff(r.Context(), in)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrAssetNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"handoff": result,
	})
}

func (s *Server) handleCreateQualityResult(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "database is not configured")
		return
	}

	var qr domain.QualityResult
	if err := json.NewDecoder(r.Body).Decode(&qr); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	if qr.AssetID == "" {
		writeError(w, http.StatusBadRequest, "asset_id is required")
		return
	}
	if qr.TargetLanguage == "" {
		qr.TargetLanguage = "vi"
	}
	if qr.Stage == "" {
		qr.Stage = "multimodal_qc"
	}

	if err := s.db.SaveQualityResult(r.Context(), qr); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"quality_result": qr,
	})
}

func (s *Server) handleGetQualityResults(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "database is not configured")
		return
	}
	targetLang := r.URL.Query().Get("target_language")
	if targetLang == "" {
		targetLang = "vi"
	}
	stage := r.URL.Query().Get("stage")

	results, err := s.db.GetQualityResults(r.Context(), assetID, targetLang, stage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []domain.QualityResult{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"asset_id":        assetID,
		"target_language": targetLang,
		"quality_results": results,
		"count":           len(results),
	})
}

func (s *Server) handleGetRunQualityResults(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "database is not configured")
		return
	}

	results, err := s.db.GetQualityResultsByRun(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []domain.QualityResult{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":          runID,
		"quality_results": results,
		"count":           len(results),
	})
}

// ---- Job Bundle Handlers (T21) ----

func (s *Server) handleExportJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	s.exportJob(w, r, jobID)
}

func (s *Server) handleImportJob(w http.ResponseWriter, r *http.Request) {
	s.importJob(w, r)
}

func (s *Server) exportJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if s.bundleSvc == nil {
		writeError(w, http.StatusInternalServerError, "bundle service is not configured")
		return
	}

	if jobID == "" {
		writeError(w, http.StatusBadRequest, "job_id is required")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"job_bundle_%s.zip\"", jobID))

	if _, err := s.bundleSvc.ExportBundle(r.Context(), jobID, w); err != nil {
		s.writeBundleError(w, err)
		return
	}
}

func (s *Server) importJob(w http.ResponseWriter, r *http.Request) {
	if s.bundleSvc == nil {
		writeError(w, http.StatusInternalServerError, "bundle service is not configured")
		return
	}

	contentType := r.Header.Get("Content-Type")
	overwrite := r.URL.Query().Get("overwrite") == "true"

	// 1. Multipart form file
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(100 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "parse multipart form: "+err.Error())
			return
		}
		file, _, err := r.FormFile("bundle")
		if err != nil {
			file, _, err = r.FormFile("file")
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "missing bundle file in multipart form")
			return
		}
		defer file.Close()

		if r.FormValue("overwrite") == "true" {
			overwrite = true
		}

		manifest, err := s.bundleSvc.ImportBundleFromReader(r.Context(), file, service.ImportOptions{Overwrite: overwrite})
		if err != nil {
			s.writeBundleError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"status":   "imported",
			"job_id":   manifest.Job.ID,
			"manifest": manifest,
		})
		return
	}

	// 2. Direct zip binary payload
	if strings.HasPrefix(contentType, "application/zip") || strings.HasPrefix(contentType, "application/octet-stream") {
		manifest, err := s.bundleSvc.ImportBundleFromReader(r.Context(), r.Body, service.ImportOptions{Overwrite: overwrite})
		if err != nil {
			s.writeBundleError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"status":   "imported",
			"job_id":   manifest.Job.ID,
			"manifest": manifest,
		})
		return
	}

	writeError(w, http.StatusBadRequest, "unsupported content-type: multipart/form-data or application/zip required")
}

func (s *Server) writeBundleError(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrJobNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if errors.Is(err, domain.ErrJobAlreadyExists) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, domain.ErrJobBundleTampered) ||
		errors.Is(err, domain.ErrJobBundleSecretDetected) ||
		errors.Is(err, domain.ErrJobBundleMachineLocalPath) ||
		errors.Is(err, domain.ErrJobBundleLicenseIncomplete) {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if errors.Is(err, domain.ErrJobBundleInvalid) ||
		errors.Is(err, domain.ErrJobBundleArtifactMissing) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
