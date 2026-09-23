package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

const (
	// TestAcousticProviderID is the provider identity for the deterministic test/harness analyzer.
	// NOTE: This analyzer is for deterministic Seam 1 and integration test suites (test-only).
	// Issue #80 production audio role classification is the governed YAMNet TFLite provider.
	TestAcousticProviderID   = "test_acoustic_analyzer"
	TestAcousticModelName    = "deterministic_signal_classifier"
	TestAcousticModelVersion = "v0.1-test"
)

// AudioRolePlanInput defines the input parameters for automatic AudioRolePlan generation.
type AudioRolePlanInput struct {
	AssetID          string                  `json:"asset_id"`
	RunID            string                  `json:"run_id,omitempty"`
	JobID            string                  `json:"job_id,omitempty"`
	ExecutionProfile domain.ExecutionProfile `json:"execution_profile,omitempty"`
}

// Type aliases to domain audio role analysis contracts
type AudioRoleAnalysisRequest = domain.AudioRoleAnalysisRequest
type AudioRoleAnalysisResult = domain.AudioRoleAnalysisResult
type AudioRoleAnalyzer = domain.AudioRoleAnalyzer

type keyLock struct {
	mu  sync.Mutex
	ref int
}

type keyLocker struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

func newKeyLocker() *keyLocker {
	return &keyLocker{
		locks: make(map[string]*keyLock),
	}
}

func (kl *keyLocker) Lock(key string) func() {
	kl.mu.Lock()
	l, ok := kl.locks[key]
	if !ok {
		l = &keyLock{}
		kl.locks[key] = l
	}
	l.ref++
	kl.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		kl.mu.Lock()
		l.ref--
		if l.ref == 0 {
			delete(kl.locks, key)
		}
		kl.mu.Unlock()
	}
}

type snapshotRuntimeIdentifier interface {
	SnapshotRuntimeIdentity() (snapshotSHA, runtimeSHA string, err error)
}

func analyzerProvenanceHash(preflightNormSHA, stemsCASHash string, analyzer domain.AudioRoleAnalyzer) (string, error) {
	pID, mName, mVer, cfgHash := analyzer.AnalyzerInfo()
	var snapSHA, rtSHA string
	if sri, ok := analyzer.(snapshotRuntimeIdentifier); ok {
		var err error
		snapSHA, rtSHA, err = sri.SnapshotRuntimeIdentity()
		if err != nil {
			return "", err
		}
	}
	return domain.ComputeAudioRolePlanProvenanceHash(
		preflightNormSHA,
		stemsCASHash,
		pID,
		mName,
		mVer,
		cfgHash,
		snapSHA,
		rtSHA,
	), nil
}

// AudioRoleService coordinates automatic source analysis to generate and persist canonical AudioRolePlans.
type AudioRoleService struct {
	db          *storage.DB
	cas         *cas.Store
	audioMixSvc *AudioMixService
	router      *provider.Router
	analyzer    AudioRoleAnalyzer
	assetLocker *keyLocker
}

// NewAudioRoleService constructs a production AudioRoleService with no direct analyzer.
// Production execution is configured through ConfigureRouter so provider governance remains authoritative.
func NewAudioRoleService(db *storage.DB, casStore *cas.Store, audioMixSvc *AudioMixService) *AudioRoleService {
	return &AudioRoleService{
		db:          db,
		cas:         casStore,
		audioMixSvc: audioMixSvc,
		analyzer:    nil,
		assetLocker: newKeyLocker(),
	}
}

// NewAudioRoleServiceWithAnalyzer constructs an AudioRoleService with an explicitly provided analyzer for deterministic test harnesses.
func NewAudioRoleServiceWithAnalyzer(db *storage.DB, casStore *cas.Store, audioMixSvc *AudioMixService, analyzer AudioRoleAnalyzer) *AudioRoleService {
	return &AudioRoleService{
		db:          db,
		cas:         casStore,
		audioMixSvc: audioMixSvc,
		analyzer:    analyzer,
		assetLocker: newKeyLocker(),
	}
}

// SetAnalyzer replaces the active AudioRoleAnalyzer implementation.
func (s *AudioRoleService) SetAnalyzer(analyzer AudioRoleAnalyzer) {
	s.analyzer = analyzer
}

func (s *AudioRoleService) Analyzer() AudioRoleAnalyzer {
	return s.analyzer
}

// ConfigureRouter injects the provider router for production audio role provider routing.
func (s *AudioRoleService) ConfigureRouter(router *provider.Router) {
	s.router = router
}

// GenerateAudioRolePlan automatically generates a canonical AudioRolePlan using source preflight and stem evidence.
func (s *AudioRoleService) GenerateAudioRolePlan(ctx context.Context, in AudioRolePlanInput) (*domain.AudioRolePlan, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if s.assetLocker != nil {
		unlock := s.assetLocker.Lock(in.AssetID)
		defer unlock()
	}
	startTime := time.Now().UTC()
	// 1. Verify database, source asset, and preflight report
	if s.db == nil {
		return nil, errors.New("database is not configured")
	}
	asset, err := s.db.GetSourceAsset(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("load source asset %s: %w", in.AssetID, err)
	}

	preflight, err := s.db.GetPreflightReport(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("%w for asset %s: %v", domain.ErrAudioRolePreflightRequired, in.AssetID, err)
	}
	if preflight.NormalizedAudioCASPath == "" || preflight.NormalizedAudioSHA256 == "" {
		return nil, fmt.Errorf("%w: normalized audio artifact missing from preflight report for asset %s", domain.ErrAudioRoleEvidenceMissing, in.AssetID)
	}

	// 2. Separate audio stems or reuse existing stem artifacts
	var stems *domain.AudioStemArtifacts
	stemsIdx, err := s.db.GetAudioStemsArtifactIndex(ctx, in.AssetID)
	if err == nil && stemsIdx != nil && stemsIdx.CASHash != "" && s.cas != nil {
		rc, err := s.cas.Get(stemsIdx.CASHash)
		if err == nil {
			var loaded domain.AudioStemArtifacts
			if err := json.NewDecoder(rc).Decode(&loaded); err == nil {
				loaded.CASHash = stemsIdx.CASHash
				loaded.ProvenanceHash = stemsIdx.ProvenanceHash
				stems = &loaded
			}
			_ = rc.Close()
		}
	}

	if stems == nil && s.audioMixSvc != nil {
		sepInput := AudioSeparationInput{
			RunID:            in.RunID,
			AssetID:          in.AssetID,
			JobID:            in.JobID,
			ExecutionProfile: in.ExecutionProfile,
		}
		separated, err := s.audioMixSvc.SeparateAudio(ctx, sepInput)
		if err != nil {
			return nil, fmt.Errorf("%w: audio stem separation failed: %v", domain.ErrAudioRoleEvidenceMissing, err)
		}
		stems = separated
	}

	// 3. Resolve stem paths
	var vocalsPath, bgPath string
	stemsCASHash := ""
	if stems != nil {
		stemsCASHash = stems.CASHash
		for _, stem := range stems.Stems {
			stemPath := stem.AudioCASPath
			if stemPath == "" && stem.AudioCASHash != "" && s.cas != nil {
				if p, err := s.cas.ResolvePath(stem.AudioCASHash); err == nil {
					if _, err := os.Stat(p); err == nil {
						stemPath = p
					}
				}
			}
			if stem.Type == domain.StemTypeVocals {
				vocalsPath = stemPath
			} else if stem.Type == domain.StemTypeBackground {
				bgPath = stemPath
			}
		}
	}

	analysisReq := AudioRoleAnalysisRequest{
		AssetID:          in.AssetID,
		RunID:            in.RunID,
		DurationMs:       preflight.DurationMs,
		SourceAudioPath:  preflight.NormalizedAudioCASPath,
		VocalsPath:       vocalsPath,
		BackgroundPath:   bgPath,
		ExecutionProfile: in.ExecutionProfile,
	}

	// 4. Deterministic Test Analyzer Path (unit and Seam 1 testing harnesses)
	if s.analyzer != nil {
		analyzer := s.analyzer
		providerID, modelName, modelVersion, _ := analyzer.AnalyzerInfo()
		provHash, err := analyzerProvenanceHash(preflight.NormalizedAudioSHA256, stemsCASHash, analyzer)
		if err != nil {
			return nil, fmt.Errorf("%w: audio role runtime verification failed: %w", domain.ErrAudioRoleAnalyzerUnavailable, err)
		}

		if existingIdx, err := s.db.GetAudioRolePlanByProvenance(ctx, provHash); err == nil && existingIdx != nil {
			existingPlan, err := s.loadAudioRolePlanArtifact(existingIdx)
			if err != nil {
				return nil, fmt.Errorf("load cached audio role plan: %w", err)
			}
			if in.RunID != "" {
				if err := s.ensureGovernanceRecords(ctx, in.RunID, providerID, modelName, modelVersion, provHash, 0); err != nil {
					return nil, fmt.Errorf("ensure audio role governance records on cache hit: %w", err)
				}
			}
			return existingPlan, nil
		}

		analysisRes, err := analyzer.AnalyzeAudioRoles(ctx, analysisReq)
		if err != nil {
			return nil, fmt.Errorf("audio role analysis: %w", err)
		}
		if analysisRes == nil || len(analysisRes.Segments) == 0 {
			return nil, errors.New("audio role analysis produced empty segments")
		}

		plan := domain.AudioRolePlan{
			ID:             uuid.NewString(),
			AssetID:        in.AssetID,
			Segments:       analysisRes.Segments,
			ProviderID:     providerID,
			ModelName:      modelName,
			ModelVersion:   modelVersion,
			ProvenanceHash: provHash,
			CreatedAt:      time.Now().UTC(),
		}

		if s.cas != nil {
			blob, err := json.Marshal(plan)
			if err != nil {
				return nil, fmt.Errorf("marshal audio role plan: %w", err)
			}
			obj, err := s.cas.Put(bytes.NewReader(blob))
			if err != nil {
				return nil, fmt.Errorf("commit audio role plan to CAS: %w", err)
			}
			plan.CASHash = obj.SHA256
		}

		if err := s.db.SaveAudioRolePlan(ctx, plan); err != nil {
			return nil, fmt.Errorf("persist audio role plan: %w", err)
		}

		if in.RunID != "" {
			latencyMs := time.Since(startTime).Milliseconds()
			if err := s.ensureGovernanceRecords(ctx, in.RunID, providerID, modelName, modelVersion, provHash, latencyMs); err != nil {
				return nil, err
			}
		}

		return &plan, nil
	}

	// 5. Production Router Path (Fail closed if router is not configured)
	if s.router == nil {
		return nil, domain.ErrAudioRoleAnalyzerUnavailable
	}

	routeReq := provider.RouteRequest{
		RunID:            in.RunID,
		Stage:            provider.TypeAudioRole,
		Language:         "*",
		ExecutionProfile: in.ExecutionProfile,
		CandidateInputHash: func(p provider.Provider) string {
			if analyzer, ok := p.(domain.AudioRoleAnalyzer); ok {
				h, err := analyzerProvenanceHash(preflight.NormalizedAudioSHA256, stemsCASHash, analyzer)
				if err != nil {
					return ""
				}
				return h
			}
			return ""
		},
	}

	routeRes, err := s.router.Route(ctx, routeReq)
	if err != nil {
		return nil, fmt.Errorf("%w: route audio_role provider: %v", domain.ErrAudioRoleAnalyzerUnavailable, err)
	}
	if routeRes == nil || routeRes.SelectedProvider == nil {
		return nil, domain.ErrNoEligibleProvider
	}

	// Check candidates in policy-eligible order for an existing cached plan
	candidates := append([]provider.Provider{routeRes.SelectedProvider}, routeRes.FallbackOrdered...)
	for _, p := range candidates {
		if p == nil || p.PolicyState() != provider.PolicyAllowed || !p.IsHealthy() {
			continue
		}
		analyzer, ok := p.(domain.AudioRoleAnalyzer)
		if !ok {
			continue
		}
		pID, mName, mVer, _ := analyzer.AnalyzerInfo()
		candProvHash, err := analyzerProvenanceHash(preflight.NormalizedAudioSHA256, stemsCASHash, analyzer)
		if err != nil {
			if p.ID() == routeRes.SelectedProvider.ID() {
				return nil, fmt.Errorf("%w: audio role runtime verification failed for provider %s: %w", domain.ErrAudioRoleAnalyzerUnavailable, pID, err)
			}
			continue
		}
		if existingIdx, err := s.db.GetAudioRolePlanByProvenance(ctx, candProvHash); err == nil && existingIdx != nil {
			existingPlan, err := s.loadAudioRolePlanArtifact(existingIdx)
			if err != nil {
				return nil, fmt.Errorf("load cached audio role plan: %w", err)
			}
			// Cache hit for a new run: ensure provider attempt is recorded for in.RunID so governance evidence is complete.
			if in.RunID != "" {
				if err := s.ensureAttemptRecord(ctx, in.RunID, pID, mName, mVer, candProvHash, 0); err != nil {
					return nil, fmt.Errorf("ensure audio role attempt on cache hit: %w", err)
				}
			}
			return existingPlan, nil
		}
	}

	// Fresh execution: Execute via Router.ExecuteRoutedWithRetry (records SelectionDecision & ProviderAttempt canonically)
	var analysisRes *domain.AudioRoleAnalysisResult
	var executedProviderID, executedModelName, executedModelVersion, executedProvHash string
	selectedAnalyzer, ok := routeRes.SelectedProvider.(domain.AudioRoleAnalyzer)
	if !ok {
		return nil, fmt.Errorf("%w: selected provider %s does not implement AudioRoleAnalyzer", domain.ErrAudioRoleAnalyzerUnavailable, routeRes.SelectedProvider.ID())
	}
	inputHash, err := analyzerProvenanceHash(preflight.NormalizedAudioSHA256, stemsCASHash, selectedAnalyzer)
	if err != nil {
		return nil, fmt.Errorf("%w: audio role runtime verification failed: %w", domain.ErrAudioRoleAnalyzerUnavailable, err)
	}

	err = s.router.ExecuteRoutedWithRetry(ctx, routeReq, routeRes, inputHash, 1, func(p provider.Provider, attemptNum int) error {
		analyzer, ok := p.(domain.AudioRoleAnalyzer)
		if !ok {
			return fmt.Errorf("provider %s does not implement AudioRoleAnalyzer", p.ID())
		}
		pID, mName, mVer, _ := analyzer.AnalyzerInfo()
		executedProviderID = pID
		executedModelName = mName
		executedModelVersion = mVer
		executedProvHash, err = analyzerProvenanceHash(preflight.NormalizedAudioSHA256, stemsCASHash, analyzer)
		if err != nil {
			return fmt.Errorf("%w: audio role runtime verification failed: %w", domain.ErrAudioRoleAnalyzerUnavailable, err)
		}

		res, err := analyzer.AnalyzeAudioRoles(ctx, analysisReq)
		if err != nil {
			return err
		}
		if res == nil || len(res.Segments) == 0 {
			return errors.New("audio role analysis produced empty segments")
		}
		analysisRes = res
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: execute audio_role provider: %v", domain.ErrAudioRoleAnalyzerUnavailable, err)
	}

	plan := domain.AudioRolePlan{
		ID:             uuid.NewString(),
		AssetID:        in.AssetID,
		Segments:       analysisRes.Segments,
		ProviderID:     executedProviderID,
		ModelName:      executedModelName,
		ModelVersion:   executedModelVersion,
		ProvenanceHash: executedProvHash,
		CreatedAt:      time.Now().UTC(),
	}

	if s.cas != nil {
		blob, err := json.Marshal(plan)
		if err != nil {
			return nil, fmt.Errorf("marshal audio role plan: %w", err)
		}
		obj, err := s.cas.Put(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("commit audio role plan to CAS: %w", err)
		}
		plan.CASHash = obj.SHA256
	}

	if err := s.db.SaveAudioRolePlan(ctx, plan); err != nil {
		return nil, fmt.Errorf("persist audio role plan: %w", err)
	}
	if plan.CASHash != "" {
		if err := s.db.SaveAudioRolePlanIndex(ctx, storage.AudioRolePlanIndex{
			ID:             plan.ID,
			AssetID:        plan.AssetID,
			ProviderID:     plan.ProviderID,
			ModelName:      plan.ModelName,
			ModelVersion:   plan.ModelVersion,
			CASHash:        plan.CASHash,
			ProvenanceHash: plan.ProvenanceHash,
			CreatedAt:      plan.CreatedAt,
		}); err != nil {
			return nil, fmt.Errorf("persist audio role plan CAS index: %w", err)
		}
	}

	_ = asset
	return &plan, nil
}

// SaveOperatorAudioRolePlan pins an operator-provided manual AudioRolePlan to CAS and persists it with its index.
func (s *AudioRoleService) SaveOperatorAudioRolePlan(ctx context.Context, assetID string, segments []domain.AudioSegment) (*domain.AudioRolePlan, error) {
	if s.db == nil {
		return nil, errors.New("database is not configured")
	}
	if s.cas == nil {
		return nil, errors.New("CAS store is required to pin audio role plan lineage")
	}
	asset, err := s.db.GetSourceAsset(ctx, assetID)
	if err != nil {
		return nil, err
	}

	plan := domain.AudioRolePlan{
		ID:        uuid.NewString(),
		AssetID:   assetID,
		Segments:  segments,
		CreatedAt: time.Now().UTC(),
	}

	segmentsJSON, err := json.Marshal(segments)
	if err != nil {
		return nil, fmt.Errorf("marshal audio role plan: %w", err)
	}
	configDigest := sha256.Sum256(segmentsJSON)
	plan.ProviderID = "operator"
	plan.ModelName = "manual-role-plan"
	plan.ModelVersion = "1"
	plan.ProvenanceHash = domain.ComputeAudioRolePlanProvenanceHash(asset.SHA256, "", plan.ProviderID, plan.ModelName, plan.ModelVersion, hex.EncodeToString(configDigest[:]))
	artifactBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("marshal audio role plan artifact: %w", err)
	}
	obj, err := s.cas.Put(bytes.NewReader(artifactBytes))
	if err != nil {
		return nil, fmt.Errorf("store audio role plan artifact: %w", err)
	}
	plan.CASHash = obj.SHA256

	if err := s.db.SaveAudioRolePlan(ctx, plan); err != nil {
		return nil, fmt.Errorf("save audio role plan: %w", err)
	}
	if err := s.db.SaveAudioRolePlanIndex(ctx, storage.AudioRolePlanIndex{
		ID:             plan.ID,
		AssetID:        plan.AssetID,
		ProviderID:     plan.ProviderID,
		ModelName:      plan.ModelName,
		ModelVersion:   plan.ModelVersion,
		CASHash:        plan.CASHash,
		ProvenanceHash: plan.ProvenanceHash,
		CreatedAt:      plan.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("save audio role plan index: %w", err)
	}

	return &plan, nil
}

func (s *AudioRoleService) loadAudioRolePlanArtifact(idx *storage.AudioRolePlanIndex) (*domain.AudioRolePlan, error) {
	if s.cas == nil || idx == nil || idx.CASHash == "" {
		return nil, errors.New("cached audio role plan artifact is unavailable")
	}

	rc, err := s.cas.Get(idx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("read CAS artifact %s: %w", idx.CASHash, err)
	}
	defer rc.Close()

	var plan domain.AudioRolePlan
	if err := json.NewDecoder(rc).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode CAS artifact %s: %w", idx.CASHash, err)
	}
	if plan.ID != idx.ID || plan.AssetID != idx.AssetID || plan.ProviderID != idx.ProviderID || plan.ModelName != idx.ModelName || plan.ModelVersion != idx.ModelVersion || plan.ProvenanceHash != idx.ProvenanceHash {
		return nil, errors.New("cached audio role plan artifact metadata does not match provenance index")
	}
	plan.CASHash = idx.CASHash
	return &plan, nil
}

func (s *AudioRoleService) ensureAttemptRecord(ctx context.Context, runID, providerID, modelName, modelVersion, provHash string, latencyMs int64) error {
	attempts, err := s.db.ListProviderAttempts(ctx, runID, "audio_role_plan")
	if err != nil || len(attempts) == 0 {
		attempt := domain.ProviderAttempt{
			ID:            uuid.NewString(),
			RunID:         runID,
			Stage:         "audio_role_plan",
			ProviderID:    providerID,
			ModelName:     modelName,
			ModelVersion:  modelVersion,
			InputHash:     provHash,
			AttemptNumber: 1,
			Status:        "succeeded",
			LatencyMs:     latencyMs,
			CreatedAt:     time.Now().UTC(),
		}
		if err := s.db.RecordProviderAttempt(ctx, attempt); err != nil {
			return fmt.Errorf("record audio role provider attempt: %w", err)
		}
	}
	return nil
}

func (s *AudioRoleService) ensureGovernanceRecords(ctx context.Context, runID, providerID, modelName, modelVersion, provHash string, latencyMs int64) error {
	decisions, err := s.db.ListSelectionDecisions(ctx, runID, "audio_role_plan")
	if err != nil || len(decisions) == 0 {
		decision := domain.SelectionDecision{
			ID:                 uuid.NewString(),
			RunID:              runID,
			Stage:              "audio_role_plan",
			SelectedProviderID: providerID,
			PolicyCheckResult:  "allowed",
			DecisionReason:     "source-analysis audio role classification",
			CreatedAt:          time.Now().UTC(),
		}
		if err := s.db.RecordSelectionDecision(ctx, decision); err != nil {
			return fmt.Errorf("record audio role selection decision: %w", err)
		}
	}

	attempts, err := s.db.ListProviderAttempts(ctx, runID, "audio_role_plan")
	if err != nil || len(attempts) == 0 {
		attempt := domain.ProviderAttempt{
			ID:            uuid.NewString(),
			RunID:         runID,
			Stage:         "audio_role_plan",
			ProviderID:    providerID,
			ModelName:     modelName,
			ModelVersion:  modelVersion,
			InputHash:     provHash,
			AttemptNumber: 1,
			Status:        "succeeded",
			LatencyMs:     latencyMs,
			CreatedAt:     time.Now().UTC(),
		}
		if err := s.db.RecordProviderAttempt(ctx, attempt); err != nil {
			return fmt.Errorf("record audio role provider attempt: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// DeterministicTestAudioRoleAnalyzer Implementation
// NOTE: For deterministic integration and unit test suites only.
// ---------------------------------------------------------------------------

// DeterministicTestAudioRoleAnalyzer classifies audio signals for test suites.
type DeterministicTestAudioRoleAnalyzer struct {
	VocalSilenceRMS     float64
	VocalUncertainFloor float64
	SingingMinAutocorr  float64
	SingingMaxZCR       float64
	SFXMinCrestFactor   float64
	WindowDurationMs    int64
	HopDurationMs       int64
}

// NewDeterministicTestAudioRoleAnalyzer creates an analyzer with standard test parameters.
func NewDeterministicTestAudioRoleAnalyzer() *DeterministicTestAudioRoleAnalyzer {
	return &DeterministicTestAudioRoleAnalyzer{
		VocalSilenceRMS:     200.0,
		VocalUncertainFloor: 350.0,
		SingingMinAutocorr:  0.65,
		SingingMaxZCR:       0.12,
		SFXMinCrestFactor:   4.5,
		WindowDurationMs:    500,
		HopDurationMs:       500,
	}
}

func (a *DeterministicTestAudioRoleAnalyzer) AnalyzerInfo() (string, string, string, string) {
	return TestAcousticProviderID, TestAcousticModelName, TestAcousticModelVersion, a.configHash()
}

func (a *DeterministicTestAudioRoleAnalyzer) configHash() string {
	raw := fmt.Sprintf("%.1f_%.1f_%.2f_%.2f_%.1f_%d_%d",
		a.VocalSilenceRMS, a.VocalUncertainFloor, a.SingingMinAutocorr, a.SingingMaxZCR,
		a.SFXMinCrestFactor, a.WindowDurationMs, a.HopDurationMs)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:8])
}

func (a *DeterministicTestAudioRoleAnalyzer) AnalyzeAudioRoles(ctx context.Context, req AudioRoleAnalysisRequest) (*AudioRoleAnalysisResult, error) {
	sampleRate := 16000

	var vocalSamples []int16
	if req.VocalsPath != "" {
		data, err := os.ReadFile(req.VocalsPath)
		if err != nil {
			return nil, fmt.Errorf("read vocals stem file %s: %w", req.VocalsPath, err)
		}
		s, info, err := media.ExtractPCM16Samples(data)
		if err != nil {
			return nil, fmt.Errorf("decode vocals stem PCM %s: %w", req.VocalsPath, err)
		}
		vocalSamples = s
		if info != nil && info.SampleRate > 0 {
			sampleRate = int(info.SampleRate)
		}
	}

	var bgSamples []int16
	if req.BackgroundPath != "" {
		data, err := os.ReadFile(req.BackgroundPath)
		if err != nil {
			return nil, fmt.Errorf("read background stem file %s: %w", req.BackgroundPath, err)
		}
		s, info, err := media.ExtractPCM16Samples(data)
		if err != nil {
			return nil, fmt.Errorf("decode background stem PCM %s: %w", req.BackgroundPath, err)
		}
		bgSamples = s
		if info != nil && info.SampleRate > 0 {
			sampleRate = int(info.SampleRate)
		}
	}

	var sourceFallback bool
	// Fallback to source audio ONLY if neither stem was supplied
	if len(vocalSamples) == 0 && len(bgSamples) == 0 && req.SourceAudioPath != "" {
		data, err := os.ReadFile(req.SourceAudioPath)
		if err != nil {
			return nil, fmt.Errorf("read source audio file %s: %w", req.SourceAudioPath, err)
		}
		s, info, err := media.ExtractPCM16Samples(data)
		if err != nil {
			return nil, fmt.Errorf("decode source audio PCM %s: %w", req.SourceAudioPath, err)
		}
		bgSamples = s
		vocalSamples = s
		sourceFallback = true
		if info != nil && info.SampleRate > 0 {
			sampleRate = int(info.SampleRate)
		}
	}

	if len(vocalSamples) == 0 && len(bgSamples) == 0 {
		return nil, errors.New("no readable audio samples available for audio role analysis")
	}

	durationMs := req.DurationMs
	if durationMs <= 0 {
		maxLen := len(vocalSamples)
		if len(bgSamples) > maxLen {
			maxLen = len(bgSamples)
		}
		if maxLen > 0 {
			durationMs = int64(maxLen*1000) / int64(sampleRate)
		}
	}
	if durationMs <= 0 {
		return nil, errors.New("invalid zero duration for audio role analysis")
	}

	totalSamples := int((int64(sampleRate) * durationMs) / 1000)
	if totalSamples <= 0 {
		return nil, errors.New("invalid zero sample count for audio role analysis")
	}

	if len(vocalSamples) < totalSamples {
		padded := make([]int16, totalSamples)
		copy(padded, vocalSamples)
		vocalSamples = padded
	}
	if len(bgSamples) < totalSamples {
		padded := make([]int16, totalSamples)
		copy(padded, bgSamples)
		bgSamples = padded
	}

	windowSamples := (sampleRate * int(a.WindowDurationMs)) / 1000
	if windowSamples <= 0 {
		windowSamples = 8000
	}

	numWindows := totalSamples / windowSamples
	if (totalSamples % windowSamples) != 0 {
		numWindows++
	}

	type rawWindow struct {
		startMs int64
		endMs   int64
		role    domain.AudioRole
	}
	windows := make([]rawWindow, 0, numWindows)

	for w := 0; w < numWindows; w++ {
		startSample := w * windowSamples
		endSample := startSample + windowSamples
		if endSample > totalSamples {
			endSample = totalSamples
		}
		if startSample >= endSample {
			break
		}

		startMs := int64(startSample*1000) / int64(sampleRate)
		endMs := int64(endSample*1000) / int64(sampleRate)
		if endMs > durationMs {
			endMs = durationMs
		}

		vocalChunk := vocalSamples[startSample:endSample]
		bgChunk := bgSamples[startSample:endSample]

		vocalRMS := computeRMS(vocalChunk)
		bgRMS := computeRMS(bgChunk)
		bgPeak := computePeak(bgChunk)
		bgCrestFactor := 0.0
		if bgRMS > 0.001 {
			bgCrestFactor = float64(bgPeak) / bgRMS
		}

		var role domain.AudioRole
		if sourceFallback {
			// Source audio fallback without stem separation is mixed.
			// Any vocal energy in mixed source audio cannot be cleanly separated:
			// mark as uncertain rather than falsely declaring background/no-dub.
			if vocalRMS < a.VocalSilenceRMS {
				if bgCrestFactor >= a.SFXMinCrestFactor && bgPeak > 1000 {
					role = domain.AudioRoleAmbienceSFX
				} else {
					role = domain.AudioRoleInstrumentalBgm
				}
			} else {
				role = domain.AudioRoleUncertain
			}
		} else {
			if vocalRMS < a.VocalSilenceRMS {
				if bgCrestFactor >= a.SFXMinCrestFactor && bgPeak > 1000 {
					role = domain.AudioRoleAmbienceSFX
				} else {
					role = domain.AudioRoleInstrumentalBgm
				}
			} else {
				zcr := computeZCR(vocalChunk)
				autocorr := computeMaxAutocorr(vocalChunk, sampleRate)

				if vocalRMS < a.VocalUncertainFloor && autocorr < 0.30 {
					role = domain.AudioRoleUncertain
				} else if autocorr >= a.SingingMinAutocorr && zcr <= a.SingingMaxZCR {
					role = domain.AudioRoleSingingMusicVocal
				} else if autocorr >= 0.20 || zcr >= 0.04 {
					role = domain.AudioRoleNarrationDialogue
				} else {
					role = domain.AudioRoleUncertain
				}
			}
		}

		windows = append(windows, rawWindow{
			startMs: startMs,
			endMs:   endMs,
			role:    role,
		})
	}

	if len(windows) == 0 {
		return nil, errors.New("audio role analysis produced no temporal windows")
	}

	var merged []domain.AudioSegment
	for _, w := range windows {
		if len(merged) == 0 {
			merged = append(merged, domain.AudioSegment{
				StartMs: w.startMs,
				EndMs:   w.endMs,
				Role:    w.role,
			})
			continue
		}

		lastIdx := len(merged) - 1
		if merged[lastIdx].Role == w.role && merged[lastIdx].EndMs >= w.startMs {
			merged[lastIdx].EndMs = w.endMs
		} else {
			merged = append(merged, domain.AudioSegment{
				StartMs: w.startMs,
				EndMs:   w.endMs,
				Role:    w.role,
			})
		}
	}

	merged[0].StartMs = 0
	merged[len(merged)-1].EndMs = durationMs

	return &AudioRoleAnalysisResult{
		Segments:     merged,
		ProviderID:   TestAcousticProviderID,
		ModelName:    TestAcousticModelName,
		ModelVersion: TestAcousticModelVersion,
	}, nil
}

// ---------------------------------------------------------------------------
// Acoustic Signal Processing Helpers
// ---------------------------------------------------------------------------

func computeRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0.0
	}
	var sumSquares float64
	for _, s := range samples {
		v := float64(s)
		sumSquares += v * v
	}
	return math.Sqrt(sumSquares / float64(len(samples)))
}

func computePeak(samples []int16) int16 {
	var maxVal int16
	for _, s := range samples {
		abs := s
		if abs < 0 {
			if abs == math.MinInt16 {
				abs = math.MaxInt16
			} else {
				abs = -abs
			}
		}
		if abs > maxVal {
			maxVal = abs
		}
	}
	return maxVal
}

func computeZCR(samples []int16) float64 {
	if len(samples) < 2 {
		return 0.0
	}
	crossings := 0
	for i := 0; i < len(samples)-1; i++ {
		s1 := samples[i]
		s2 := samples[i+1]
		if (s1 >= 0 && s2 < 0) || (s1 < 0 && s2 >= 0) {
			crossings++
		}
	}
	return float64(crossings) / float64(len(samples)-1)
}

func computeMaxAutocorr(samples []int16, sampleRate int) float64 {
	n := len(samples)
	if n < 400 {
		return 0.0
	}

	minLag := sampleRate / 500
	maxLag := sampleRate / 80
	if maxLag >= n {
		maxLag = n - 1
	}

	var energy0 float64
	for _, s := range samples {
		v := float64(s)
		energy0 += v * v
	}
	if energy0 < 1e-4 {
		return 0.0
	}

	var maxR float64
	for lag := minLag; lag <= maxLag; lag++ {
		var sumProd float64
		var sumLag float64
		limit := n - lag
		for i := 0; i < limit; i++ {
			v1 := float64(samples[i])
			v2 := float64(samples[i+lag])
			sumProd += v1 * v2
			sumLag += v2 * v2
		}
		denom := math.Sqrt(energy0 * sumLag)
		if denom > 1e-4 {
			r := sumProd / denom
			if r > maxR {
				maxR = r
			}
		}
	}
	return maxR
}
