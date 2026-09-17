package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/storage"
)

// RouteRequest defines the input criteria for provider selection.
type RouteRequest struct {
	RunID                 string
	Stage                 ProviderType
	Language              string
	ExecutionProfile      domain.ExecutionProfile
	ConsentGranted        bool
	AuthorizedCredentials []string // Valid credential reference IDs or names
	RequiredFeatures      []string
	ExcludedProviders     []string
	PreferredProviderID   string
	CandidateInputHash    func(p Provider) string // Optional per-candidate input hash generator for fallback provenance
}

// RouteResult returns the selected provider, fallback candidates, and the selection decision.
type RouteResult struct {
	SelectedProvider Provider
	FallbackOrdered  []Provider
	Decision         domain.SelectionDecision
}

// Router orchestrates policy-before-health provider selection, credential-backed authorization, and execution provenance.
type Router struct {
	registry    *Registry
	policySvc   *governance.PolicyService
	licenseSvc  *governance.LicenseService
	credSvc     *governance.CredentialService
	snapshotSvc *governance.SnapshotService
	circuit     *CircuitBreaker
	db          *storage.DB
}

// NewRouter creates a new ProviderRouter.
func NewRouter(
	registry *Registry,
	policySvc *governance.PolicyService,
	licenseSvc *governance.LicenseService,
	credSvc *governance.CredentialService,
	circuit *CircuitBreaker,
	db *storage.DB,
) *Router {
	if circuit == nil {
		circuit = NewCircuitBreaker(CircuitBreakerConfig{})
	}
	return &Router{
		registry:   registry,
		policySvc:  policySvc,
		licenseSvc: licenseSvc,
		credSvc:    credSvc,
		circuit:    circuit,
		db:         db,
	}
}

// Circuit returns the underlying circuit breaker.
func (r *Router) Circuit() *CircuitBreaker {
	return r.circuit
}

// SetSnapshotService sets the model snapshot verification service and propagates
// it to registered providers that accept snapshot injection.
func (r *Router) SetSnapshotService(snapSvc *governance.SnapshotService) {
	r.snapshotSvc = snapSvc
	if r.registry != nil {
		for _, p := range r.registry.ListAll() {
			if sp, ok := p.(interface {
				SetSnapshotService(*governance.SnapshotService)
			}); ok {
				sp.SetSnapshotService(snapSvc)
			}
		}
	}
}

// SnapshotService returns the configured snapshot service.
func (r *Router) SnapshotService() *governance.SnapshotService {
	return r.snapshotSvc
}

// Helper to check if slice contains string (case-insensitive)
func containsFold(slice []string, s string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, s) || item == "*" {
			return true
		}
	}
	return false
}

// Route enforces the strict routing order:
// 1. Policy eligibility (PolicyService + LicenseService + Credential-backed Authorization)
// 2. Declared capability (Stage, Language, Required Features)
// 3. Runtime health & circuit breaker
// 4. Profile / Quality / Cost ranking
func (r *Router) Route(ctx context.Context, req RouteRequest) (*RouteResult, error) {
	if r.db != nil {
		isTTS := req.Stage == TypeTTS
		isDialogueTranslation := req.Stage == TypeTranslation && containsFold(req.RequiredFeatures, "shorten_first_adaptation")
		isDubStage := isTTS || isDialogueTranslation

		var plan *domain.AudioRolePlan
		var job *domain.LocalizationJob

		if req.RunID != "" {
			run, err := r.db.GetRun(ctx, req.RunID)
			if err != nil {
				if !errors.Is(err, storage.ErrNotFound) {
					return nil, fmt.Errorf("failed to get run due to storage failure: %w", err)
				}
				if isDubStage {
					return nil, fmt.Errorf("unknown run_id: audio role plan required for dub stage: %w", err)
				}
			}
			if run != nil {
				j, err := r.db.GetJob(ctx, run.JobID)
				if err != nil {
					if !errors.Is(err, domain.ErrJobNotFound) {
						return nil, fmt.Errorf("failed to get job due to storage failure: %w", err)
					}
					if isDubStage {
						return nil, fmt.Errorf("job not found: audio role plan required for dub stage: %w", err)
					}
				}
				job = j
				if job != nil {
					p, err := r.db.GetAudioRolePlan(ctx, job.SourceAssetID)
					if err != nil {
						if !errors.Is(err, storage.ErrNotFound) {
							return nil, fmt.Errorf("failed to get audio role plan due to storage failure: %w", err)
						}
						if isDubStage {
							return nil, fmt.Errorf("audio role plan required for dub stage: %w", err)
						}
					}
					plan = p
				} else if isDubStage {
					return nil, fmt.Errorf("job not found: audio role plan required for dub stage: %w", storage.ErrNotFound)
				}
			}
		} else if isDubStage {
			return nil, fmt.Errorf("missing run_id: audio role plan required for dub stage: %w", storage.ErrNotFound)
		}

		if plan != nil {
			hasUncertain := false
			hasSinging := false
			hasNarration := false
			for _, seg := range plan.Segments {
				if seg.Role == domain.AudioRoleUncertain || (seg.Role != domain.AudioRoleNarrationDialogue &&
					seg.Role != domain.AudioRoleSingingMusicVocal &&
					seg.Role != domain.AudioRoleInstrumentalBgm &&
					seg.Role != domain.AudioRoleAmbienceSFX) {
					hasUncertain = true
				}
				if seg.Role == domain.AudioRoleSingingMusicVocal {
					hasSinging = true
				}
				if seg.Role == domain.AudioRoleNarrationDialogue {
					hasNarration = true
				}
			}

			if isDubStage {
				if hasUncertain {
					if err := r.db.UpdateJobStatus(ctx, job.ID, "review_required"); err != nil {
						return nil, fmt.Errorf("failed to update job status: %w", err)
					}
					return nil, domain.ErrUncertainRole
				}
				if !hasNarration {
					// No-dub bypass contract
					reason := "no-dub: no narration dialogue segments in plan, stage bypassed"
					if hasSinging {
						reason = "no-dub: singing-only/no narration speech segments in plan, stage bypassed"
					}
					decision := domain.SelectionDecision{
						ID:                  uuid.NewString(),
						RunID:               req.RunID,
						Stage:               string(req.Stage),
						SelectedProviderID:  "",
						CandidatesEvaluated: nil,
						PolicyCheckResult:   "BYPASS",
						DecisionReason:      reason,
						CreatedAt:           time.Now().UTC(),
					}
					if err := r.db.RecordSelectionDecision(ctx, decision); err != nil {
						return nil, fmt.Errorf("fail-closed: record selection decision provenance: %w", err)
					}
					return &RouteResult{
						SelectedProvider: nil,
						FallbackOrdered:  nil,
						Decision:         decision,
					}, nil
				}
			}
		}
	}

	if req.ExecutionProfile == "" {
		req.ExecutionProfile = domain.ExecutionProfileHybrid // Cost-first hybrid default
	}

	allProviders := r.registry.ListAll()
	var evaluations []domain.CandidateEvaluation
	type candidateScore struct {
		provider Provider
		score    float64
		eval     domain.CandidateEvaluation
	}
	var eligibleCandidates []candidateScore

	excludedMap := make(map[string]bool)
	for _, id := range req.ExcludedProviders {
		excludedMap[id] = true
	}

	for _, p := range allProviders {
		eval := domain.CandidateEvaluation{
			ProviderID:  p.ID(),
			PolicyState: string(p.PolicyState()),
			HealthOK:    p.IsHealthy(),
		}

		if excludedMap[p.ID()] {
			eval.Eligible = false
			eval.RejectionCode = "EXCLUDED_BY_REQUEST"
			eval.Reason = "provider was explicitly excluded"
			evaluations = append(evaluations, eval)
			continue
		}

		// 1. Policy & Authorization Check (backed by CredentialService)
		isAuth := false
		if r.credSvc != nil && len(req.AuthorizedCredentials) > 0 {
			for _, refIDOrName := range req.AuthorizedCredentials {
				if ok, err := r.credSvc.ValidateCredentialRef(ctx, refIDOrName, p.ID()); ok && err == nil {
					isAuth = true
					break
				}
			}
		}

		var policyEligible bool
		var effectivePolicy domain.PolicyState
		var policyErr error
		if r.policySvc != nil {
			policyEligible, effectivePolicy, policyErr = r.policySvc.EvaluateEligibility(ctx, p.ID(), p.PolicyState(), req.ConsentGranted, isAuth)
		} else {
			effectivePolicy = p.PolicyState()
			if effectivePolicy == "" {
				effectivePolicy = domain.PolicyBlocked
			}
			policyEligible = effectivePolicy == domain.PolicyAllowed
			if !policyEligible {
				policyErr = domain.ErrPolicyBlocked
			}
		}
		eval.PolicyState = string(effectivePolicy)

		if !policyEligible {
			eval.Eligible = false
			if errors.Is(policyErr, domain.ErrConsentRequired) {
				eval.RejectionCode = "REQUIRES_CONSENT"
				eval.Reason = "explicit operator consent required"
			} else if errors.Is(policyErr, domain.ErrAuthRequired) {
				eval.RejectionCode = "REQUIRES_AUTHORIZATION"
				eval.Reason = "valid credential reference authorization required"
			} else {
				eval.RejectionCode = "POLICY_BLOCKED"
				eval.Reason = "blocked by governance policy"
			}
			evaluations = append(evaluations, eval)
			continue
		}

		// License checkpoint validation (versioned & fail-closed)
		modelName, modelVer := p.ModelInfo()
		primaryRequired := primaryCheckpointRequired(p)
		if primaryRequired && modelName != "" && r.licenseSvc != nil {
			if err := r.licenseSvc.VerifyCheckpoint(ctx, modelName, modelVer, ""); err != nil {
				// If manifest is missing or unverified, fail closed
				eval.Eligible = false
				eval.RejectionCode = "LICENSE_MANIFEST_MISSING"
				eval.Reason = "unmanifested or unverified checkpoint: " + err.Error()
				evaluations = append(evaluations, eval)
				continue
			}
		}

		// Snapshot verification binding validation (versioned & fail-closed, Issue #64)
		requiresSnapshot := false
		if rsp, ok := p.(interface{ RequiresSnapshot() bool }); ok {
			requiresSnapshot = rsp.RequiresSnapshot()
		}
		if !requiresSnapshot && r.snapshotSvc != nil && r.snapshotSvc.HasBinding(modelName, modelVer) {
			requiresSnapshot = true
		}

		if primaryRequired && requiresSnapshot && modelName != "" {
			if r.snapshotSvc == nil {
				eval.Eligible = false
				eval.RejectionCode = "SNAPSHOT_UNVERIFIED"
				eval.Reason = "snapshot verification service unavailable: unverified snapshot"
				evaluations = append(evaluations, eval)
				continue
			}
			if err := r.snapshotSvc.CheckBindingFingerprints(modelName, modelVer); err != nil {
				eval.Eligible = false
				if errors.Is(err, domain.ErrSnapshotUnverified) {
					eval.RejectionCode = "SNAPSHOT_UNVERIFIED"
				} else if errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
					eval.RejectionCode = "SNAPSHOT_MUTATED_REHASH_REQUIRED"
				} else if errors.Is(err, domain.ErrSnapshotDigestMismatch) {
					eval.RejectionCode = "SNAPSHOT_DIGEST_MISMATCH"
				} else if errors.Is(err, domain.ErrSnapshotFileCorrupted) {
					eval.RejectionCode = "SNAPSHOT_FILE_CORRUPTED"
				} else {
					eval.RejectionCode = "SNAPSHOT_UNVERIFIED"
				}
				eval.Reason = "snapshot verification check failed: " + err.Error()
				evaluations = append(evaluations, eval)
				continue
			}
		}

		// Verify any dependent checkpoints (e.g. FSMN-VAD for diarizer) fail-closed
		if r.licenseSvc != nil || r.snapshotSvc != nil {
			var deps []ModelDependency
			if dmp, ok := p.(DependentModelProvider); ok {
				deps = dmp.ModelDependencies()
			} else if vip, ok := p.(interface{ VADModelInfo() (string, string) }); ok {
				vName, vVer := vip.VADModelInfo()
				if vName != "" {
					deps = append(deps, ModelDependency{Name: vName, Version: vVer, Role: "vad"})
				}
			}
			depMissing := false
			for _, dep := range deps {
				if dep.Name != "" {
					if r.licenseSvc != nil {
						if err := r.licenseSvc.VerifyCheckpoint(ctx, dep.Name, dep.Version, ""); err != nil {
							eval.Eligible = false
							eval.RejectionCode = "LICENSE_MANIFEST_MISSING"
							eval.Reason = fmt.Sprintf("unmanifested or unverified dependency checkpoint %s (%s): %v", dep.Name, dep.Role, err)
							evaluations = append(evaluations, eval)
							depMissing = true
							break
						}
					}
					depRequiresSnapshot := requiresSnapshot
					if !depRequiresSnapshot && r.snapshotSvc != nil && r.snapshotSvc.HasBinding(dep.Name, dep.Version) {
						depRequiresSnapshot = true
					}
					if depRequiresSnapshot {
						if r.snapshotSvc == nil {
							eval.Eligible = false
							eval.RejectionCode = "DEPENDENCY_SNAPSHOT_INVALID"
							eval.Reason = fmt.Sprintf("snapshot service unavailable for dependency %s (%s)", dep.Name, dep.Role)
							evaluations = append(evaluations, eval)
							depMissing = true
							break
						}
						if err := r.snapshotSvc.CheckBindingFingerprints(dep.Name, dep.Version); err != nil {
							eval.Eligible = false
							eval.RejectionCode = "DEPENDENCY_SNAPSHOT_INVALID"
							eval.Reason = fmt.Sprintf("invalid or unverified dependency snapshot %s (%s): %v", dep.Name, dep.Role, err)
							evaluations = append(evaluations, eval)
							depMissing = true
							break
						}
					}
				}
			}
			if depMissing {
				continue
			}
		}

		// 2. Declared Capability Check (Stage, Language, Required Features)
		cap := p.Capability()
		if req.Stage != "" && p.Type() != req.Stage {
			eval.Eligible = false
			eval.RejectionCode = "STAGE_MISMATCH"
			eval.Reason = fmt.Sprintf("provider stage %s does not match requested %s", p.Type(), req.Stage)
			evaluations = append(evaluations, eval)
			continue
		}

		reqLang := strings.TrimSpace(req.Language)
		if reqLang != "" && reqLang != "*" && len(cap.Languages) > 0 && !containsFold(cap.Languages, reqLang) {
			eval.Eligible = false
			eval.RejectionCode = "LANGUAGE_NOT_SUPPORTED"
			eval.Reason = fmt.Sprintf("language %s not supported", req.Language)
			evaluations = append(evaluations, eval)
			continue
		}

		if len(req.RequiredFeatures) > 0 {
			missingFeature := ""
			for _, reqFeat := range req.RequiredFeatures {
				if !containsFold(cap.Features, reqFeat) {
					missingFeature = reqFeat
					break
				}
			}
			if missingFeature != "" {
				eval.Eligible = false
				eval.RejectionCode = "FEATURE_NOT_SUPPORTED"
				eval.Reason = fmt.Sprintf("missing required feature: %s", missingFeature)
				evaluations = append(evaluations, eval)
				continue
			}
		}

		// Production translation is remote-LLM only. Local model-backed translators remain
		// available as non-production adapters/tests, but Router must never select them for
		// VI/EN translation. This keeps constrained local hardware focused on voice/audio/CV.
		if p.Type() == TypeTranslation && strings.EqualFold(cap.ExecutionTier, "local") {
			eval.Eligible = false
			eval.RejectionCode = "TRANSLATION_LOCAL_PROVIDER_EXCLUDED"
			eval.Reason = "production translation requires an authorized remote gateway provider"
			evaluations = append(evaluations, eval)
			continue
		}

		// Profile tier gating: Local profile hard-excludes every remote/cloud provider candidate
		if req.ExecutionProfile == domain.ExecutionProfileLocal && !strings.EqualFold(cap.ExecutionTier, "local") {
			eval.Eligible = false
			eval.RejectionCode = "PROFILE_TIER_EXCLUDED"
			eval.Reason = fmt.Sprintf("execution profile local hard-excludes non-local tier %s", cap.ExecutionTier)
			evaluations = append(evaluations, eval)
			continue
		}

		// 3. Runtime Health & Circuit Breaker Check
		if !p.IsHealthy() {
			eval.Eligible = false
			eval.RejectionCode = "UNHEALTHY"
			eval.Reason = "provider reported unhealthy state"
			evaluations = append(evaluations, eval)
			continue
		}

		canAttempt := true
		if r.circuit != nil {
			canAttempt = r.circuit.CanAttempt(p.ID())
		}
		eval.CircuitClosed = canAttempt
		if !canAttempt {
			eval.Eligible = false
			eval.RejectionCode = "CIRCUIT_OPEN"
			eval.Reason = "circuit breaker open due to consecutive failures"
			evaluations = append(evaluations, eval)
			continue
		}

		// 4. Profile / Quality / Cost Scoring
		score := calculateScore(p.ID(), cap, req.ExecutionProfile)
		// Translation has a frozen production ladder (Gemini -> DeepSeek), so caller
		// preference must not reorder it. PreferredProviderID remains available to
		// stages such as acquisition and TTS where operator/provider preference is valid.
		if p.Type() != TypeTranslation && req.PreferredProviderID != "" {
			pref := strings.ToLower(req.PreferredProviderID)
			pid := strings.ToLower(p.ID())
			if pid == pref || pid == "fake_"+pref || strings.TrimPrefix(pid, "fake_") == strings.TrimPrefix(pref, "fake_") {
				score += 1000.0
			}
		}
		eval.Eligible = true
		eval.Score = score
		eval.Reason = "eligible for execution"
		evaluations = append(evaluations, eval)

		eligibleCandidates = append(eligibleCandidates, candidateScore{
			provider: p,
			score:    score,
			eval:     eval,
		})
	}

	if len(eligibleCandidates) == 0 {
		// Record decision failure
		dec := domain.SelectionDecision{
			ID:                  uuid.NewString(),
			RunID:               req.RunID,
			Stage:               string(req.Stage),
			SelectedProviderID:  "",
			CandidatesEvaluated: evaluations,
			PolicyCheckResult:   "FAILED",
			DecisionReason:      "no eligible providers found",
			CreatedAt:           time.Now().UTC(),
		}
		if r.db != nil {
			if err := r.db.RecordSelectionDecision(ctx, dec); err != nil {
				return nil, fmt.Errorf("fail-closed: record selection decision provenance: %w", err)
			}
		}
		return nil, domain.ErrNoEligibleProvider
	}

	// Sort eligible candidates deterministically: score desc -> quality desc -> cost asc -> provider ID asc
	sort.Slice(eligibleCandidates, func(i, j int) bool {
		if eligibleCandidates[i].score != eligibleCandidates[j].score {
			return eligibleCandidates[i].score > eligibleCandidates[j].score
		}
		if eligibleCandidates[i].provider.Capability().QualityScore != eligibleCandidates[j].provider.Capability().QualityScore {
			return eligibleCandidates[i].provider.Capability().QualityScore > eligibleCandidates[j].provider.Capability().QualityScore
		}
		if eligibleCandidates[i].provider.Capability().CostPerUnit != eligibleCandidates[j].provider.Capability().CostPerUnit {
			return eligibleCandidates[i].provider.Capability().CostPerUnit < eligibleCandidates[j].provider.Capability().CostPerUnit
		}
		return eligibleCandidates[i].provider.ID() < eligibleCandidates[j].provider.ID()
	})

	selected := eligibleCandidates[0].provider
	var fallback []Provider
	for _, c := range eligibleCandidates[1:] {
		fallback = append(fallback, c.provider)
	}

	decision := domain.SelectionDecision{
		ID:                  uuid.NewString(),
		RunID:               req.RunID,
		Stage:               string(req.Stage),
		SelectedProviderID:  selected.ID(),
		CandidatesEvaluated: evaluations,
		PolicyCheckResult:   "ALLOWED",
		DecisionReason:      fmt.Sprintf("selected highest ranking candidate with score %.2f", eligibleCandidates[0].score),
		CreatedAt:           time.Now().UTC(),
	}

	if r.db != nil {
		if err := r.db.RecordSelectionDecision(ctx, decision); err != nil {
			return nil, fmt.Errorf("fail-closed: record selection decision provenance: %w", err)
		}
	}

	return &RouteResult{
		SelectedProvider: selected,
		FallbackOrdered:  fallback,
		Decision:         decision,
	}, nil
}
func calculateScore(providerID string, cap domain.ProviderCapability, profile domain.ExecutionProfile) float64 {
	var baseScore float64

	switch profile {
	case domain.ExecutionProfileLocal:
		if strings.EqualFold(cap.ExecutionTier, "local") {
			baseScore += 10.0
		} else if strings.EqualFold(cap.ExecutionTier, "hybrid") {
			baseScore += 2.0
		} else {
			baseScore -= 5.0
		}
	case domain.ExecutionProfileCloud:
		if strings.EqualFold(cap.ExecutionTier, "cloud") {
			baseScore += 10.0
		} else if strings.EqualFold(cap.ExecutionTier, "hybrid") {
			baseScore += 5.0
		} else {
			baseScore += 2.0
		}
	case domain.ExecutionProfileHybrid:
		fallthrough
	default:
		// Translation stage has approved production ordering:
		// gateway gemini-3.8-flash -> gateway deepseek-v4.1-flash.
		// Local LLM translators are filtered before scoring.
		// Uses exact provider IDs with deterministic score separation so lane ordering
		// is explicitly guaranteed regardless of minor quality score deltas.
		if strings.EqualFold(cap.Stage, string(TypeTranslation)) || strings.EqualFold(cap.Stage, "translation") {
			switch providerID {
			case GatewayGeminiTranslationProviderID:
				baseScore += 1000.0
			case GatewayDeepSeekTranslationProviderID:
				baseScore += 500.0
			default:
				if strings.EqualFold(cap.ExecutionTier, "cloud") || strings.EqualFold(cap.ExecutionTier, "hybrid") {
					baseScore += 50.0
				} else {
					baseScore += 10.0
				}
			}
		} else {
			// Cost-First Hybrid Default:
			if strings.EqualFold(cap.ExecutionTier, "local") {
				baseScore += 6.0
			} else if strings.EqualFold(cap.ExecutionTier, "hybrid") {
				baseScore += 5.0
			} else {
				baseScore += 3.0
			}
			// Cost penalty
			baseScore -= cap.CostPerUnit * 10.0
		}
	}

	// Quality boost
	baseScore += cap.QualityScore * 4.0
	return baseScore
}

// ExecuteWithRetry executes a function with transient retry, quality failure fallback, and immutable attempt logging.
// When execution moves to an alternate candidate after failure, it preserves policy re-evaluation and appends a selection decision.
func (r *Router) ExecuteWithRetry(
	ctx context.Context,
	req RouteRequest,
	inputHash string,
	maxRetries int,
	executeFn func(p Provider, attemptNumber int) error,
) error {
	if strings.TrimSpace(inputHash) == "" {
		return errors.New("input_hash is required")
	}
	if executeFn == nil {
		return errors.New("execute function is required")
	}

	routeRes, err := r.Route(ctx, req)
	if err != nil {
		return err
	}
	return r.ExecuteRoutedWithRetry(ctx, req, routeRes, inputHash, maxRetries, executeFn)
}

// ExecuteRoutedWithRetry executes a previously resolved RouteResult without
// routing a second time. This lets callers inspect the selected provider for a
// provider-specific cache lookup while preserving the Router's retry,
// circuit-breaker, policy re-evaluation, ProviderAttempt, and fallback
// SelectionDecision semantics for actual execution.
func (r *Router) ExecuteRoutedWithRetry(
	ctx context.Context,
	req RouteRequest,
	routeRes *RouteResult,
	inputHash string,
	maxRetries int,
	executeFn func(p Provider, attemptNumber int) error,
) error {
	if strings.TrimSpace(inputHash) == "" {
		return errors.New("input_hash is required")
	}
	if executeFn == nil {
		return errors.New("execute function is required")
	}
	if routeRes == nil {
		return errors.New("route result is required")
	}
	if maxRetries <= 0 {
		maxRetries = 1
	}

	if routeRes.SelectedProvider == nil {
		return nil
	}

	candidates := append([]Provider{routeRes.SelectedProvider}, routeRes.FallbackOrdered...)
	var lastErr error
	attemptGlobal := 0

	for idx, p := range candidates {
		candInputHash := inputHash
		if req.CandidateInputHash != nil {
			if h := req.CandidateInputHash(p); h != "" {
				candInputHash = h
			}
		}
		modelName, modelVer := p.ModelInfo()
		primaryRequired := primaryCheckpointRequired(p)
		var effectivePolicy domain.PolicyState

		// When moving to an alternate candidate (idx > 0), re-evaluate policy eligibility and append selection decision provenance
		if idx > 0 {
			isAuth := false
			if r.credSvc != nil && len(req.AuthorizedCredentials) > 0 {
				for _, refIDOrName := range req.AuthorizedCredentials {
					if ok, err := r.credSvc.ValidateCredentialRef(ctx, refIDOrName, p.ID()); ok && err == nil {
						isAuth = true
						break
					}
				}
			}

			var policyEligible bool
			var policyErr error
			if r.policySvc != nil {
				policyEligible, effectivePolicy, policyErr = r.policySvc.EvaluateEligibility(ctx, p.ID(), p.PolicyState(), req.ConsentGranted, isAuth)
			} else {
				effectivePolicy = p.PolicyState()
				policyEligible = effectivePolicy == domain.PolicyAllowed
				if !policyEligible {
					policyErr = domain.ErrPolicyBlocked
				}
			}

			if !policyEligible {
				lastErr = policyErr
				continue
			}

			if primaryRequired && modelName != "" && r.licenseSvc != nil {
				if err := r.licenseSvc.VerifyCheckpoint(ctx, modelName, modelVer, ""); err != nil {
					lastErr = err
					continue
				}
			}
			requiresSnapshot := false
			if rsp, ok := p.(interface{ RequiresSnapshot() bool }); ok {
				requiresSnapshot = rsp.RequiresSnapshot()
			}
			if !requiresSnapshot && r.snapshotSvc != nil && r.snapshotSvc.HasBinding(modelName, modelVer) {
				requiresSnapshot = true
			}
			if primaryRequired && requiresSnapshot && modelName != "" {
				if r.snapshotSvc == nil {
					lastErr = domain.ErrSnapshotUnverified
					continue
				}
				if err := r.snapshotSvc.CheckBindingFingerprints(modelName, modelVer); err != nil {
					lastErr = err
					continue
				}
			}
		}

		// Verify snapshot integrity right before execution attempt (Issue #64)
		requiresSnapshot := false
		if rsp, ok := p.(interface{ RequiresSnapshot() bool }); ok {
			requiresSnapshot = rsp.RequiresSnapshot()
		}
		if !requiresSnapshot && r.snapshotSvc != nil && r.snapshotSvc.HasBinding(modelName, modelVer) {
			requiresSnapshot = true
		}
		if primaryRequired && requiresSnapshot && modelName != "" {
			if r.snapshotSvc == nil {
				lastErr = domain.ErrSnapshotUnverified
				continue
			}
			if err := r.snapshotSvc.CheckBindingFingerprints(modelName, modelVer); err != nil {
				lastErr = err
				continue
			}
		}
		if r.circuit != nil && !r.circuit.CanAttempt(p.ID()) {
			attemptGlobal++
			pa := domain.ProviderAttempt{
				ID:            uuid.NewString(),
				RunID:         req.RunID,
				Stage:         string(req.Stage),
				ProviderID:    p.ID(),
				ModelName:     modelName,
				ModelVersion:  modelVer,
				InputHash:     candInputHash,
				AttemptNumber: attemptGlobal,
				Status:        "circuit_broken",
				ErrorMessage:  domain.ErrCircuitOpen.Error(),
				LatencyMs:     0,
				CostUnits:     0.0,
				CreatedAt:     time.Now().UTC(),
			}
			enrichAttemptMetadata(&pa, p)
			if r.db != nil {
				if err := r.db.RecordProviderAttempt(ctx, pa); err != nil {
					return fmt.Errorf("fail-closed: record circuit_broken attempt provenance: %w", err)
				}
			}
			lastErr = domain.ErrCircuitOpen
			continue
		}

		if idx > 0 {
			// Append SelectionDecision provenance for the alternate candidate actually selected
			altDecision := domain.SelectionDecision{
				ID:                 uuid.NewString(),
				RunID:              req.RunID,
				Stage:              string(req.Stage),
				SelectedProviderID: p.ID(),
				CandidatesEvaluated: []domain.CandidateEvaluation{
					{
						ProviderID:    p.ID(),
						Eligible:      true,
						PolicyState:   string(effectivePolicy),
						HealthOK:      p.IsHealthy(),
						CircuitClosed: true,
						Score:         calculateScore(p.ID(), p.Capability(), req.ExecutionProfile),
						Reason:        "selected alternate fallback candidate after prior failure",
					},
				},
				PolicyCheckResult: string(effectivePolicy),
				DecisionReason:    fmt.Sprintf("selected alternate candidate %s after prior candidate failure", p.ID()),
				CreatedAt:         time.Now().UTC(),
			}
			if r.db != nil {
				if err := r.db.RecordSelectionDecision(ctx, altDecision); err != nil {
					return fmt.Errorf("fail-closed: record alternate selection decision provenance: %w", err)
				}
			}
		}

		candidateRetries := maxRetries
		if p.Capability().MaxRetries != nil {
			candidateRetries = *p.Capability().MaxRetries
			if candidateRetries <= 0 {
				candidateRetries = 1
			}
		}

		for attempt := 1; attempt <= candidateRetries; attempt++ {
			attemptGlobal++
			start := time.Now()
			err := executeFn(p, attempt)
			latency := time.Since(start).Milliseconds()

			if err == nil {
				// Record successful attempt
				pa := domain.ProviderAttempt{
					ID:            uuid.NewString(),
					RunID:         req.RunID,
					Stage:         string(req.Stage),
					ProviderID:    p.ID(),
					ModelName:     modelName,
					ModelVersion:  modelVer,
					InputHash:     candInputHash,
					AttemptNumber: attemptGlobal,
					Status:        "succeeded",
					LatencyMs:     latency,
					CostUnits:     p.Capability().CostPerUnit,
					CreatedAt:     time.Now().UTC(),
				}
				enrichAttemptMetadata(&pa, p)
				if r.db != nil {
					if errDB := r.db.RecordProviderAttempt(ctx, pa); errDB != nil {
						return fmt.Errorf("fail-closed: record succeeded attempt provenance: %w", errDB)
					}
				}
				if r.circuit != nil {
					r.circuit.RecordSuccess(p.ID())
				}
				return nil
			}

			lastErr = err

			// 1. Check for non-transient governance/policy/inconsistency errors (FAIL-CLOSED, no retry, no fallback)
			if errors.Is(err, domain.ErrPolicyBlocked) ||
				errors.Is(err, domain.ErrConsentRequired) ||
				errors.Is(err, domain.ErrAuthRequired) ||
				errors.Is(err, domain.ErrLicenseManifestMissing) ||
				errors.Is(err, domain.ErrRawSecretForbidden) ||
				errors.Is(err, domain.ErrContentUnavailable) ||
				errors.Is(err, domain.ErrInvalidURL) ||
				errors.Is(err, domain.ErrUnsupportedMediaType) ||
				errors.Is(err, domain.ErrInconsistentProvenance) {
				pa := domain.ProviderAttempt{
					ID:            uuid.NewString(),
					RunID:         req.RunID,
					Stage:         string(req.Stage),
					ProviderID:    p.ID(),
					ModelName:     modelName,
					ModelVersion:  modelVer,
					InputHash:     candInputHash,
					AttemptNumber: attemptGlobal,
					Status:        "policy_rejected",
					ErrorMessage:  err.Error(),
					LatencyMs:     latency,
					CostUnits:     0.0,
					CreatedAt:     time.Now().UTC(),
				}
				enrichAttemptMetadata(&pa, p)
				if r.db != nil {
					if errDB := r.db.RecordProviderAttempt(ctx, pa); errDB != nil {
						return fmt.Errorf("fail-closed: record policy_rejected attempt provenance: %w (original: %v)", errDB, err)
					}
				}
				return err
			}

			// 2. Check for Quality failure (Record quality-failed attempt, break attempt loop to advance to alternate candidate)
			if errors.Is(err, domain.ErrQualityRejected) {
				pa := domain.ProviderAttempt{
					ID:            uuid.NewString(),
					RunID:         req.RunID,
					Stage:         string(req.Stage),
					ProviderID:    p.ID(),
					ModelName:     modelName,
					ModelVersion:  modelVer,
					InputHash:     candInputHash,
					AttemptNumber: attemptGlobal,
					Status:        "quality_failed",
					ErrorMessage:  err.Error(),
					LatencyMs:     latency,
					CostUnits:     p.Capability().CostPerUnit,
					CreatedAt:     time.Now().UTC(),
				}
				enrichAttemptMetadata(&pa, p)
				if r.db != nil {
					if errDB := r.db.RecordProviderAttempt(ctx, pa); errDB != nil {
						return fmt.Errorf("fail-closed: record quality_failed attempt provenance: %w", errDB)
					}
				}
				break
			}

			// 3. Transient failure (retry candidate up to candidateRetries, trip circuit breaker if repeated)
			pa := domain.ProviderAttempt{
				ID:            uuid.NewString(),
				RunID:         req.RunID,
				Stage:         string(req.Stage),
				ProviderID:    p.ID(),
				ModelName:     modelName,
				ModelVersion:  modelVer,
				InputHash:     candInputHash,
				AttemptNumber: attemptGlobal,
				Status:        "failed",
				ErrorMessage:  err.Error(),
				LatencyMs:     latency,
				CostUnits:     0.0,
				CreatedAt:     time.Now().UTC(),
			}
			enrichAttemptMetadata(&pa, p)
			if r.db != nil {
				if errDB := r.db.RecordProviderAttempt(ctx, pa); errDB != nil {
					return fmt.Errorf("fail-closed: record failed attempt provenance: %w", errDB)
				}
			}
			if r.circuit != nil {
				r.circuit.RecordFailure(p.ID(), true)
			}
		}
	}

	return fmt.Errorf("all provider candidates failed for stage %s: %w", req.Stage, lastErr)
}

func enrichAttemptMetadata(pa *domain.ProviderAttempt, p Provider) {
	prov := ExtractRemoteProvenance(p)
	pa.ObservedModel = prov.ObservedModel
	pa.ServiceBaselineID = prov.ServiceBaselineID
}
