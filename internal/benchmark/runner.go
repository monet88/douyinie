package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

// RunnerConfig configures the benchmark runner.
type RunnerConfig struct {
	Client  *RuntimeHostClient
	Store   Store
	Session *Session
	Sampler ResourceSampler
}

// AcquisitionInput represents one item to be executed in the acquisition benchmark.
type AcquisitionInput struct {
	EntryID               string                  `json:"entry_id"` // aweme_id or canonical key
	CanonicalURL          string                  `json:"canonical_url"`
	ExpectedAwemeID       string                  `json:"expected_aweme_id"`
	ExpectedDurationMs    int64                   `json:"expected_duration_ms"`
	DurationToleranceMs   int64                   `json:"duration_tolerance_ms"`
	MediaFilePath         string                  `json:"media_file_path,omitempty"` // For local synthetic/seam1 testing
	AttestationDeclaredBy string                  `json:"attestation_declared_by,omitempty"`
	AcquireRequest        *service.AcquireRequest `json:"acquire_request,omitempty"` // For live URL acquisition
}

// QualityCaseInput represents one logical case to be executed in the quality benchmark.
type QualityCaseInput struct {
	CaseID                string                           `json:"case_id"` // e.g. "video_01_vi"
	SourceVideoID         string                           `json:"source_video_id"`
	PrimaryCategory       string                           `json:"primary_category"`
	TargetLanguage        string                           `json:"target_language"`
	Profile               string                           `json:"profile"` // "local", "hybrid"
	ConsentGranted        bool                             `json:"consent_granted,omitempty"`
	AuthorizedCredentials []string                         `json:"authorized_credentials,omitempty"`
	SourceAssetID         string                           `json:"source_asset_id,omitempty"`
	MediaFilePath         string                           `json:"media_file_path,omitempty"`
	Segments              []domain.TranslationInputSegment `json:"segments,omitempty"`
	AudioRoleSegments     []domain.AudioSegment            `json:"audio_role_segments,omitempty"`
	ReferencePack         *ReferenceAnnotationPack         `json:"-"`
	IsNoDub               bool                             `json:"is_no_dub,omitempty"`
}

// BenchmarkRunner drives benchmark execution over the public Seam 1 RuntimeHost API.
// Invariant: The runner sequences only existing public/versioned endpoints; it preserves
// native Job and Run identities, and enables safe resumption after interruption.
type BenchmarkRunner struct {
	client  *RuntimeHostClient
	store   Store
	session *Session
	sampler ResourceSampler
}

// NewBenchmarkRunner creates a new BenchmarkRunner instance.
func NewBenchmarkRunner(cfg RunnerConfig) (*BenchmarkRunner, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("runner requires RuntimeHostClient")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("runner requires Store")
	}
	if cfg.Session == nil {
		return nil, fmt.Errorf("runner requires Session")
	}
	// Sampler is optional; do not fabricate fake zero samples when nil.
	return &BenchmarkRunner{
		client:  cfg.Client,
		store:   cfg.Store,
		session: cfg.Session,
		sampler: cfg.Sampler,
	}, nil
}

// ExecuteAcquisition executes one acquisition benchmark item with resumption support.
func (r *BenchmarkRunner) ExecuteAcquisition(ctx context.Context, input AcquisitionInput) (*AcquisitionEntryEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.session.Status == SessionStatusCorpusInvalidated {
		return nil, errors.New("cannot execute acquisition on CORPUS_INVALIDATED session")
	}
	// 1. Check if already completed (resumed)
	if r.session.IsAcquisitionComplete(input.EntryID) {
		if entry, ok := r.session.GetAcquisition(input.EntryID); ok {
			return entry, nil
		}
	}
	evidence := AcquisitionEntryEvidence{
		EntryID:             input.EntryID,
		CanonicalURL:        input.CanonicalURL,
		ExpectedAwemeID:     input.ExpectedAwemeID,
		ExpectedDurationMs:  input.ExpectedDurationMs,
		DurationToleranceMs: input.DurationToleranceMs,
		Timestamp:           time.Now().UTC(),
	}

	// 2. Perform acquisition
	var asset *domain.SourceAsset
	var acqResult *service.AcquireResult
	var httpStatus int
	var acqErr error

	if input.MediaFilePath != "" {
		// Local file ingest (used in Seam 1 synthetic testing; marked as prohibited substitution)
		evidence.IsLocalSubstitution = true
		declaredBy := input.AttestationDeclaredBy
		if declaredBy == "" {
			declaredBy = "benchmark-runner"
		}
		asset, acqErr = r.client.IngestAsset(ctx, input.MediaFilePath, declaredBy, true)
		if acqErr != nil {
			var httpErr *HTTPError
			if errors.As(acqErr, &httpErr) {
				httpStatus = httpErr.StatusCode
			} else {
				httpStatus = 500
			}
		} else {
			httpStatus = 201
		}
	} else if input.AcquireRequest != nil {
		// Live URL acquisition
		res, err := r.client.AcquireSource(ctx, *input.AcquireRequest)
		if err != nil {
			acqErr = err
			var httpErr *HTTPError
			if errors.As(err, &httpErr) {
				httpStatus = httpErr.StatusCode
			} else {
				httpStatus = 0 // Invariant (Issue #70 Finding 3): Do not fabricate 403. Leave 0 if unknown.
			}
		} else {
			acqResult = res
			asset = res.Asset
			httpStatus = 201
		}
	} else {
		acqErr = fmt.Errorf("neither MediaFilePath nor AcquireRequest provided for entry %s", input.EntryID)
		httpStatus = 400
	}

	evidence.HTTPStatusCode = httpStatus

	if acqErr != nil {
		if errors.Is(acqErr, context.Canceled) || errors.Is(acqErr, context.DeadlineExceeded) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, acqErr
		}
		evidence.IntegrityPassed = false
		evidence.Status = "FAIL"
		evidence.ErrorMessage = acqErr.Error()
		var acqErrTyped *domain.AcquisitionError
		if errors.As(acqErr, &acqErrTyped) {
			evidence.ProviderID = acqErrTyped.ProviderID
		}
	} else if asset != nil {
		evidence.SourceAssetID = asset.ID
		evidence.SourceAssetCASHash = asset.SHA256

		// Invariant (Issue #70 Finding 1): Derive observed aweme identity from real provenance.
		// Do NOT set ObservedAwemeID = input.ExpectedAwemeID.
		if acqResult == nil || acqResult.Provenance == nil {
			if !evidence.IsLocalSubstitution {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = "acquisition result is missing provenance"
			}
		} else {
			prov := acqResult.Provenance
			evidence.ProviderID = prov.Adapter
			evidence.Method = prov.Method
			evidence.AcquiredMediaType = "video"

			if prov.AssetID != asset.ID {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("provenance asset_id mismatch: provenance has %s, asset has %s", prov.AssetID, asset.ID)
			} else if prov.Platform != "douyin" {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("provenance platform must be douyin, got %q", prov.Platform)
			} else if prov.CanonicalURL != input.CanonicalURL {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("provenance canonical_url mismatch: expected %s, got %s", input.CanonicalURL, prov.CanonicalURL)
			} else if !strings.HasPrefix(prov.SourceID, "douyin:aweme:") {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("provenance source_id format invalid: expected prefix douyin:aweme:, got %q", prov.SourceID)
			} else {
				observedID := strings.TrimPrefix(prov.SourceID, "douyin:aweme:")
				if observedID == "" {
					evidence.IntegrityPassed = false
					evidence.Status = "FAIL"
					evidence.ErrorMessage = "provenance source_id has empty aweme_id"
				} else {
					evidence.ObservedAwemeID = observedID
				}
			}
		}

		// Invariant (Issue #70 Finding 2): Prefer AcquireResult.PreflightReport from the same seam;
		// require it to exist, match the asset, and satisfy real container/fingerprint/video/audio checks.
		// Never set IntegrityPassed = true when preflight lookup fails.
		var preflight *domain.PreflightReport
		if acqResult != nil && acqResult.PreflightReport != nil {
			preflight = acqResult.PreflightReport
		} else {
			p, err := r.client.GetAssetPreflight(ctx, asset.ID)
			if err == nil {
				preflight = p
			}
		}

		if preflight == nil {
			evidence.IntegrityPassed = false
			evidence.Status = "FAIL"
			evidence.ErrorMessage = "preflight report missing from acquisition result and could not be retrieved"
		} else {
			evidence.ObservedDurationMs = preflight.DurationMs
			evidence.AudioIntegrityPassed = (strings.TrimSpace(preflight.NormalizedAudioSHA256) != "")
			if preflight.AssetID != asset.ID {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("preflight asset_id mismatch: report has %s, asset has %s", preflight.AssetID, asset.ID)
			} else if !preflight.ContainerValid || !preflight.FingerprintMatch {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("preflight container/fingerprint integrity failed: container_valid=%v, fingerprint_match=%v, errors=%v",
					preflight.ContainerValid, preflight.FingerprintMatch, preflight.Errors)
			} else if strings.TrimSpace(preflight.VideoCodec) == "" || preflight.Width <= 0 || preflight.Height <= 0 || preflight.FrameRate <= 0 {
				evidence.IntegrityPassed = false
				evidence.Status = "FAIL"
				evidence.ErrorMessage = fmt.Sprintf("preflight video stream metadata invalid: codec=%q, dimensions=%dx%d, frame_rate=%.2f",
					preflight.VideoCodec, preflight.Width, preflight.Height, preflight.FrameRate)
			} else {
				evidence.IntegrityPassed = true
				// Duration tolerance check
				diff := int64(math.Abs(float64(evidence.ObservedDurationMs - input.ExpectedDurationMs)))
				if input.DurationToleranceMs > 0 && diff > input.DurationToleranceMs {
					evidence.IntegrityPassed = false
					evidence.Status = "FAIL"
					evidence.ErrorMessage = fmt.Sprintf("duration mismatch: observed %dms, expected %dms (tolerance %dms)",
						evidence.ObservedDurationMs, input.ExpectedDurationMs, input.DurationToleranceMs)
				} else if evidence.Status != "FAIL" {
					evidence.Status = "PASS"
				}
			}
		}

		// Invariant (Issue #70 Finding 2): Local-file substitution must be terminal FAIL
		// on the per-entry evidence so no intermediate consumer can interpret it as a scored success.
		// Diagnostic provenance is preserved, but FAIL is never overwritten with PASS.
		if evidence.IsLocalSubstitution {
			evidence.Status = "FAIL"
			if evidence.ErrorMessage == "" {
				evidence.ErrorMessage = "local-file substitution is prohibited for scored acquisition benchmark"
			}
		}
	}

	// 3. Query attempts for provenance
	attempts, err := r.client.ListAttempts(ctx, "", "acquisition")
	if err == nil && len(attempts) > 0 {
		for _, a := range attempts {
			evidence.Attempts = append(evidence.Attempts, ProviderAttemptRef{
				AttemptID:     a.ID,
				ProviderID:    a.ProviderID,
				CandidateName: a.ModelName,
				AttemptNumber: a.AttemptNumber,
				Outcome:       a.Status,
				LatencyMs:     a.LatencyMs,
				ErrorMessage:  a.ErrorMessage,
				ObservedModel: func() string {
					if a.ObservedModel != "" {
						return a.ObservedModel
					}
					return a.ModelVersion
				}(),
				ServiceBaselineID: a.ServiceBaselineID,
			})
		}
	}

	// Query selection decisions for acquisition provenance
	decisions, err := r.client.ListDecisions(ctx, "", "acquisition")
	if err == nil && len(decisions) > 0 {
		for _, d := range decisions {
			evidence.Decisions = append(evidence.Decisions, SelectionDecisionRef{
				DecisionID:         d.ID,
				Stage:              d.Stage,
				SelectedProviderID: d.SelectedProviderID,
				SelectedCandidate:  d.SelectedProviderID,
				PolicyCheckResult:  d.PolicyCheckResult,
				Reason:             d.DecisionReason,
				Timestamp:          d.CreatedAt,
			})
		}
	}
	// 4. Save evidence and commit sidecar
	r.session.RecordAcquisition(evidence)
	if err := r.store.Save(r.session); err != nil {
		return &evidence, fmt.Errorf("save session after acquisition: %w", err)
	}

	return &evidence, nil
}

// ExecuteAcquisitionCorpus sequentially executes all 100 entries of an AcquisitionCorpusManifest
// without adding benchmark retries, preserving production retry/fallback behavior,
// and computing the aggregate benchmark summary with fixed denominator 100.
func (r *BenchmarkRunner) ExecuteAcquisitionCorpus(ctx context.Context, manifest *AcquisitionCorpusManifest, authRef string) (*AcquisitionBenchmarkSummary, error) {
	if r.session.Status == SessionStatusCorpusInvalidated {
		return nil, errors.New("cannot execute acquisition corpus on CORPUS_INVALIDATED session")
	}
	if manifest == nil {
		return nil, errors.New("acquisition corpus manifest is nil")
	}
	if err := manifest.VerifyManifestDigest(); err != nil {
		return nil, fmt.Errorf("verify manifest digest: %w", err)
	}
	if err := r.session.RecordAcquisitionManifest(manifest); err != nil {
		return nil, fmt.Errorf("record acquisition manifest to session: %w", err)
	}

	// Execute each entry sequentially with production retry/fallback behavior
	// (no hidden benchmark retries added)
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.session.Status == SessionStatusCorpusInvalidated {
			return nil, errors.New("acquisition corpus execution stopped: session CORPUS_INVALIDATED by oracle proof")
		}
		var creds []string
		if authRef != "" {
			creds = []string{authRef}
		}
		input := AcquisitionInput{
			EntryID:             entry.EntryID,
			CanonicalURL:        entry.CanonicalURL,
			ExpectedAwemeID:     entry.AwemeID,
			ExpectedDurationMs:  entry.ExpectedDurationMs,
			DurationToleranceMs: entry.DurationToleranceMs,
			AcquireRequest: &service.AcquireRequest{
				Locator: domain.SourceLocator{
					Type:     "douyin_url",
					Location: entry.CanonicalURL,
				},
				AuthorizedCredentials: creds,
				ConsentGranted:        true,
			},
		}
		// Execute single acquisition. Resumption ensures previously completed entries are not re-executed.
		_, err := r.ExecuteAcquisition(ctx, input)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, err
			}
			return nil, err
		}
	}

	// Compute deterministic summary with fixed denominator = 100
	summary, err := ComputeAcquisitionSummary(r.session.ID, manifest, r.session.AcquisitionEntries, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("compute acquisition summary: %w", err)
	}

	if err := r.session.RecordAcquisitionSummary(summary); err != nil {
		return nil, fmt.Errorf("record acquisition summary: %w", err)
	}

	if err := r.store.Save(r.session); err != nil {
		return nil, fmt.Errorf("save session after acquisition corpus: %w", err)
	}

	return summary, nil
}

// QualityCorpusExecutionInput contains inputs needed to execute the 48 cases for
// the hard profile frozen into the benchmark session identity. The full release
// denominator is one Local session plus one Hybrid session.
type QualityCorpusExecutionInput struct {
	Manifest              *QualityCorpusManifest
	AuditPlan             *StratifiedAuditPlan
	ReferencePacks        map[string]*ReferenceAnnotationPack // asset_id or source_asset_id -> frozen ReferenceAnnotationPack
	AssetMediaPaths       map[string]string                   // asset_id -> local media file path or synthetic fixture path
	AssetSegments         map[string][]domain.TranslationInputSegment
	AssetAudioRoles       map[string][]domain.AudioSegment
	ConsentGranted        bool
	AuthorizedCredentials []string
}

// ExecuteQualityCorpus executes exactly the same 48 logical cases (24 sources × VI/EN)
// under the single Local or Hybrid hard profile bound into this session identity.
// It respects resumption (reusing already completed cases), records stage/attempt/decision/telemetry evidence,
// computes deterministic profile rollups and summary, and stores them in the session.
func (r *BenchmarkRunner) ExecuteQualityCorpus(ctx context.Context, input QualityCorpusExecutionInput) (*QualityBenchmarkSummary, error) {
	if r.session.Status == SessionStatusCorpusInvalidated {
		return nil, errors.New("cannot execute quality corpus on CORPUS_INVALIDATED session")
	}
	if input.Manifest == nil {
		return nil, errors.New("quality corpus manifest is nil")
	}
	if err := input.Manifest.VerifyManifestDigest(); err != nil {
		return nil, fmt.Errorf("verify quality manifest digest: %w", err)
	}
	if err := r.session.RecordQualityCorpusManifest(input.Manifest); err != nil {
		return nil, fmt.Errorf("bind quality corpus manifest to session: %w", err)
	}
	if input.AuditPlan != nil {
		if err := r.session.RecordAuditPlan(input.AuditPlan); err != nil {
			return nil, fmt.Errorf("bind audit plan to session: %w", err)
		}
	}
	profile := strings.ToLower(strings.TrimSpace(r.session.IdentityInput.ExecutionProfile))
	if profile != string(domain.ExecutionProfileLocal) && profile != string(domain.ExecutionProfileHybrid) {
		return nil, fmt.Errorf("quality benchmark session requires execution profile local or hybrid, got %q", r.session.IdentityInput.ExecutionProfile)
	}
	languages := []string{"vi", "en"}

	// Validate reference annotation packs fail-closed before execution
	if len(input.ReferencePacks) == 0 {
		return nil, errors.New("missing reference annotation packs for quality corpus")
	}
	for _, asset := range input.Manifest.PrimaryAssets {
		pack := input.ReferencePacks[asset.AssetID]
		if pack == nil {
			pack = input.ReferencePacks[asset.SourceAssetID]
		}
		if pack == nil {
			return nil, fmt.Errorf("missing reference annotation pack for primary asset %s", asset.AssetID)
		}
		computedDigest, err := pack.ComputePackDigest()
		if err != nil || !strings.EqualFold(computedDigest, asset.ReferencePackDigest) {
			return nil, fmt.Errorf("reference pack digest mismatch for asset %s: manifest=%s computed=%s", asset.AssetID, asset.ReferencePackDigest, computedDigest)
		}
	}

	// Iterate through the bound profile x 24 primary videos x 2 languages = 48 executions.
	for _, asset := range input.Manifest.PrimaryAssets {
		pack := input.ReferencePacks[asset.AssetID]
		if pack == nil {
			pack = input.ReferencePacks[asset.SourceAssetID]
		}
		for _, lang := range languages {
			caseID := fmt.Sprintf("%s_%s_%s", profile, asset.AssetID, lang)

			// Check resumption
			if r.session.IsQualityCaseComplete(caseID) {
				continue
			}

			var audioRoles []domain.AudioSegment
			if input.AssetAudioRoles != nil {
				audioRoles = input.AssetAudioRoles[asset.AssetID]
			}
			mediaPath := ""
			if input.AssetMediaPaths != nil {
				mediaPath = input.AssetMediaPaths[asset.AssetID]
			}

			caseInput := QualityCaseInput{
				CaseID:                caseID,
				SourceVideoID:         asset.SourceVideoID,
				PrimaryCategory:       string(asset.PrimaryCategory),
				TargetLanguage:        lang,
				Profile:               profile,
				ConsentGranted:        input.ConsentGranted,
				AuthorizedCredentials: append([]string(nil), input.AuthorizedCredentials...),
				SourceAssetID:         asset.SourceAssetID,
				MediaFilePath:         mediaPath,
				Segments:              nil, // Scored release path translates production TranscriptArtifact, not injected segments
				AudioRoleSegments:     audioRoles,
				ReferencePack:         pack,
				IsNoDub:               asset.NoDub,
			}

			_, err := r.ExecuteQualityCase(ctx, caseInput)
			if err != nil {
				// Failure on a single case is recorded as terminal case failure,
				// does not halt execution of remaining cases in the benchmark.
				continue
			}
		}
	}

	// Compute deterministic profile-specific 48-execution summary.
	summary, err := ComputeQualitySummary(r.session.ID, input.Manifest, input.AuditPlan, r.session.QualityCases, input.ReferencePacks, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("compute quality summary: %w", err)
	}

	if err := r.session.RecordQualitySummary(summary); err != nil {
		return nil, fmt.Errorf("record quality summary in session: %w", err)
	}

	if err := r.store.Save(r.session); err != nil {
		return nil, fmt.Errorf("save session after quality corpus: %w", err)
	}

	return summary, nil
}

// ExecuteQualityCase executes one quality case through all pipeline stages with resumption support.
func (r *BenchmarkRunner) ExecuteQualityCase(ctx context.Context, input QualityCaseInput) (*QualityCaseEvidence, error) {
	sessionProfile := strings.ToLower(strings.TrimSpace(r.session.IdentityInput.ExecutionProfile))
	if sessionProfile != string(domain.ExecutionProfileLocal) && sessionProfile != string(domain.ExecutionProfileHybrid) {
		return nil, fmt.Errorf("quality benchmark session requires execution profile local or hybrid, got %q", r.session.IdentityInput.ExecutionProfile)
	}
	caseProfile := strings.ToLower(strings.TrimSpace(input.Profile))
	if caseProfile == "" {
		caseProfile = sessionProfile
		input.Profile = sessionProfile
	}
	if caseProfile != sessionProfile {
		return nil, fmt.Errorf("quality case profile mismatch: case %q profile %q != session profile %q", input.CaseID, input.Profile, sessionProfile)
	}

	routeOpts := StageRouteOptions{
		ExecutionProfile:      domain.ExecutionProfile(sessionProfile),
		AuthorizedCredentials: input.AuthorizedCredentials,
		ConsentGranted:        input.ConsentGranted,
	}
	// 1. Check if whole case is already complete
	if r.session.IsQualityCaseComplete(input.CaseID) {
		if qc, ok := r.session.GetQualityCase(input.CaseID); ok {
			return qc, nil
		}
	}

	// 2. Retrieve or initialize QualityCaseEvidence
	qc, ok := r.session.GetQualityCase(input.CaseID)
	if !ok || qc == nil {
		qc = &QualityCaseEvidence{
			CaseID:          input.CaseID,
			SourceVideoID:   input.SourceVideoID,
			PrimaryCategory: input.PrimaryCategory,
			TargetLanguage:  input.TargetLanguage,
			Profile:         input.Profile,
			Stages:          make(map[string]StageExecutionEvidence),
			Status:          "IN_PROGRESS",
			CreatedAt:       time.Now().UTC(),
		}
	}
	if qc.StageArtifacts == nil {
		qc.StageArtifacts = &CaseMeasuredStageArtifacts{}
	}
	if input.ReferencePack != nil {
		qc.ReferencePackID = input.ReferencePack.PackID
		qc.ReferencePackDigest = input.ReferencePack.PackDigest
	}
	qc.IsNoDub = input.IsNoDub
	// 3. Ingest asset if needed and preserve native SourceAssetID
	if qc.SourceAssetID == "" {
		if input.SourceAssetID != "" {
			qc.SourceAssetID = input.SourceAssetID
			asset, err := r.client.GetAsset(ctx, input.SourceAssetID)
			if err == nil && asset != nil {
				qc.SourceAssetCASHash = asset.SHA256
			}
		} else if input.MediaFilePath != "" {
			asset, err := r.client.IngestAsset(ctx, input.MediaFilePath, "benchmark-runner", true)
			if err != nil {
				qc.Status = "FAILED"
				qc.ErrorMessage = fmt.Sprintf("ingest asset: %v", err)
				r.session.RecordQualityCase(*qc)
				_ = r.store.Save(r.session)
				return qc, err
			}
			qc.SourceAssetID = asset.ID
			qc.SourceAssetCASHash = asset.SHA256
		} else {
			return nil, fmt.Errorf("quality case %s requires SourceAssetID or MediaFilePath", input.CaseID)
		}
	}

	// 4. Create or reuse native LocalizationJob
	if qc.JobID == "" {
		job, err := r.client.CreateJob(ctx, qc.SourceAssetID, input.TargetLanguage)
		if err != nil {
			qc.Status = "FAILED"
			qc.ErrorMessage = fmt.Sprintf("create job: %v", err)
			r.session.RecordQualityCase(*qc)
			_ = r.store.Save(r.session)
			return qc, err
		}
		qc.JobID = job.ID
	}

	// 5. Create or reuse native LocalizationRun
	if qc.RunID == "" {
		configSnapshotJSON, err := CanonicalizeConfig(r.session.IdentityInput.ConfigSnapshot)
		if err != nil {
			qc.Status = "FAILED"
			qc.ErrorMessage = fmt.Sprintf("canonicalize session config snapshot: %v", err)
			r.session.RecordQualityCase(*qc)
			_ = r.store.Save(r.session)
			return qc, err
		}
		run, err := r.client.CreateRun(ctx, qc.JobID, configSnapshotJSON)
		if err != nil {
			qc.Status = "FAILED"
			qc.ErrorMessage = fmt.Sprintf("create run: %v", err)
			r.session.RecordQualityCase(*qc)
			_ = r.store.Save(r.session)
			return qc, err
		}
		qc.RunID = run.ID
	}

	r.session.RecordQualityCase(*qc)
	_ = r.store.Save(r.session)

	// Helper to execute a stage if not already completed
	runStage := func(stageName string, executeFn func() (casHash string, err error)) error {
		if existing, ok := qc.Stages[stageName]; ok && existing.Status == "COMPLETED" {
			return nil // Skip already-completed stage (resumed)
		}

		start := time.Now().UTC()
		casHash, err := executeFn()
		elapsed := time.Since(start).Milliseconds()

		stageEvidence := StageExecutionEvidence{
			Stage:         stageName,
			OutputCASHash: casHash,
			StartedAt:     start,
			CompletedAt:   time.Now().UTC(),
			DurationMs:    elapsed,
		}

		if err != nil {
			stageEvidence.Status = "FAILED"
			stageEvidence.ErrorMessage = err.Error()
			r.session.UpdateQualityStage(input.CaseID, stageEvidence)
			_ = r.store.Save(r.session)
			return fmt.Errorf("stage %s failed: %w", stageName, err)
		}

		stageEvidence.Status = "COMPLETED"

		// Query attempts for this stage
		attempts, err := r.client.ListAttempts(ctx, qc.RunID, stageName)
		if err == nil {
			for _, a := range attempts {
				stageEvidence.Attempts = append(stageEvidence.Attempts, ProviderAttemptRef{
					AttemptID:     a.ID,
					ProviderID:    a.ProviderID,
					CandidateName: a.ModelName,
					AttemptNumber: a.AttemptNumber,
					Outcome:       a.Status,
					LatencyMs:     a.LatencyMs,
					ErrorMessage:  a.ErrorMessage,
					ObservedModel: func() string {
						if a.ObservedModel != "" {
							return a.ObservedModel
						}
						return a.ModelVersion
					}(),
					ServiceBaselineID: a.ServiceBaselineID,
				})
			}
		}
		// Query selection decisions for this stage
		decisions, err := r.client.ListDecisions(ctx, qc.RunID, stageName)
		if err == nil && len(decisions) > 0 {
			for _, d := range decisions {
				stageEvidence.Decisions = append(stageEvidence.Decisions, SelectionDecisionRef{
					DecisionID:         d.ID,
					Stage:              d.Stage,
					SelectedProviderID: d.SelectedProviderID,
					SelectedCandidate:  d.SelectedProviderID,
					PolicyCheckResult:  d.PolicyCheckResult,
					Reason:             d.DecisionReason,
					Timestamp:          d.CreatedAt,
				})
			}
		}

		// Optional authentic telemetry sampling (owned by #72; absent unless explicit collector injected)
		if r.sampler != nil {
			if sample, ok := r.sampler.SampleStage(stageName, start, elapsed); ok {
				r.session.RecordTelemetry(input.CaseID, sample)
			}
		}
		r.session.UpdateQualityStage(input.CaseID, stageEvidence)
		return r.store.Save(r.session)
	}

	// Stage 1: AudioRolePlan
	var audioRoleCAS string
	if len(input.AudioRoleSegments) > 0 {
		err := runStage("audio_role_plan", func() (string, error) {
			plan, err := r.client.SaveAudioRolePlan(ctx, qc.SourceAssetID, input.AudioRoleSegments)
			if err != nil {
				return "", err
			}
			return plan.ID, nil
		})
		if err != nil {
			return qc, err
		}
		audioRoleCAS = r.session.QualityCases[input.CaseID].Stages["audio_role_plan"].OutputCASHash
	}

	// Stage 2: Speech Understanding
	var transcriptCAS string
	if !input.IsNoDub {
		err := runStage("speech_understand", func() (string, error) {
			artifact, err := r.client.RunSpeechUnderstand(ctx, qc.SourceAssetID, qc.RunID)
			if err != nil {
				return "", err
			}
			qc.StageArtifacts.Transcript = artifact
			return artifact.CASHash, nil
		})
		if err != nil {
			return qc, err
		}
		transcriptCAS = r.session.QualityCases[input.CaseID].Stages["speech_understand"].OutputCASHash
	}
	// Stage 3: Translation
	var translationCAS string
	if !input.IsNoDub {
		err := runStage("translate", func() (string, error) {
			variant, err := r.client.RunTranslation(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, input.Segments, routeOpts)
			if err != nil {
				return "", err
			}
			qc.StageArtifacts.Translation = variant
			return variant.CASHash, nil
		})
		if err != nil {
			return qc, err
		}
		translationCAS = r.session.QualityCases[input.CaseID].Stages["translate"].OutputCASHash
	}

	// Stage 4: Dub Script
	var dubScriptCAS string
	if !input.IsNoDub {
		err := runStage("dub_script", func() (string, error) {
			variant, err := r.client.RunDubScript(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, translationCAS, input.Segments, routeOpts)
			if err != nil {
				return "", err
			}
			qc.StageArtifacts.DubScript = variant
			return variant.CASHash, nil
		})
		if err != nil {
			return qc, err
		}
		dubScriptCAS = r.session.QualityCases[input.CaseID].Stages["dub_script"].OutputCASHash
	}

	// Stage 5: Voice Assignment
	var voiceAssignCAS string
	if !input.IsNoDub {
		err := runStage("voice_assignment", func() (string, error) {
			assign, err := r.client.AssignVoices(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, routeOpts)
			if err != nil {
				return "", err
			}
			qc.StageArtifacts.VoiceAssignment = assign
			return assign.CASHash, nil
		})
		if err != nil {
			return qc, err
		}
		voiceAssignCAS = r.session.QualityCases[input.CaseID].Stages["voice_assignment"].OutputCASHash
	}

	// Stage 6: Dub Synthesize
	var dubSegmentsCAS string
	if !input.IsNoDub {
		err := runStage("dub_synthesize", func() (string, error) {
			segments, err := r.client.RunDubSynthesize(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, voiceAssignCAS, dubScriptCAS, routeOpts)
			if err != nil {
				return "", err
			}
			qc.StageArtifacts.DubSegments = segments
			return segments.CASHash, nil
		})
		if err != nil {
			return qc, err
		}
		dubSegmentsCAS = r.session.QualityCases[input.CaseID].Stages["dub_synthesize"].OutputCASHash
	}
	var stemsCAS string
	err := runStage("separate_stems", func() (string, error) {
		stems, err := r.client.SeparateStems(ctx, qc.SourceAssetID, qc.RunID, routeOpts)
		if err != nil {
			return "", err
		}
		return stems.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	stemsCAS = r.session.QualityCases[input.CaseID].Stages["separate_stems"].OutputCASHash

	// Stage 8: Audio Mix
	var dubMixCAS string
	err = runStage("audio_mix", func() (string, error) {
		mix, err := r.client.RunAudioMix(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, dubSegmentsCAS, stemsCAS, routeOpts)
		if err != nil {
			return "", err
		}
		qc.StageArtifacts.DubMix = mix
		return mix.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	dubMixCAS = r.session.QualityCases[input.CaseID].Stages["audio_mix"].OutputCASHash

	// Stage 9: Text Detection
	var textRegionCAS string
	err = runStage("detect_text", func() (string, error) {
		plan, err := r.client.DetectText(ctx, qc.SourceAssetID, qc.RunID, routeOpts)
		if err != nil {
			return "", err
		}
		return plan.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	textRegionCAS = r.session.QualityCases[input.CaseID].Stages["detect_text"].OutputCASHash

	// Stage 10: Visual Track
	var visTrackCAS, subTrackCAS string
	err = runStage("visual_track", func() (string, error) {
		visTrack, subTrack, err := r.client.LocalizeVisualTrack(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, textRegionCAS, translationCAS, routeOpts)
		if err != nil {
			return "", err
		}
		qc.StageArtifacts.LocalizedVisualTrack = visTrack
		qc.StageArtifacts.LocalizedSubtitleTrack = subTrack
		subTrackCAS = subTrack.CASHash
		return visTrack.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	visTrackCAS = r.session.QualityCases[input.CaseID].Stages["visual_track"].OutputCASHash

	// Stage 11: Render Plan
	var renderPlanCAS string
	err = runStage("render_plan", func() (string, error) {
		plan, err := r.client.FreezeRenderPlan(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, dubMixCAS, subTrackCAS)
		if err != nil {
			return "", err
		}
		return plan.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	renderPlanCAS = r.session.QualityCases[input.CaseID].Stages["render_plan"].OutputCASHash

	// Stage 12: Render Final
	err = runStage("render_final", func() (string, error) {
		artifact, err := r.client.RenderFinal(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, renderPlanCAS)
		if err != nil {
			return "", err
		}
		qc.StageArtifacts.FinalRender = artifact
		return artifact.CASHash, nil
	})
	if err != nil {
		return qc, err
	}

	// 5.5 Deterministically evaluate automated multimodal AV/QC and persist through public API.
	// Make this step resume-safe: inspect existing run quality results first to avoid duplicate appends on retry.
	existingQRs, err := r.client.GetRunQualityResults(ctx, qc.RunID)
	if err != nil {
		return qc, fmt.Errorf("get run quality results for resume check: %w", err)
	}

	var existingBenchmarkQRs []domain.QualityResult
	for _, qr := range existingQRs {
		if IsBenchmarkOwnedQualityResult(qr, qc.RunID, qc.JobID, qc.SourceAssetID, qc.TargetLanguage) {
			existingBenchmarkQRs = append(existingBenchmarkQRs, qr)
		}
	}

	var autoQRID string
	if len(existingBenchmarkQRs) > 1 {
		return qc, fmt.Errorf("fail-closed: multiple (%d) benchmark-owned multimodal_qc results already exist for run %s", len(existingBenchmarkQRs), qc.RunID)
	} else if len(existingBenchmarkQRs) == 1 {
		autoQRID = existingBenchmarkQRs[0].ID
	} else {
		autoQR := deriveAutomatedQualityResult(qc, input.ReferencePack, input.IsNoDub)
		posted, err := r.client.PostQualityResult(ctx, autoQR)
		if err != nil {
			return qc, fmt.Errorf("post automated quality result: %w", err)
		}
		autoQRID = posted.ID
	}

	// 6. Capture relational QC evidence from SQLite (fail-closed)
	qcResults, err := r.client.GetRunQualityResults(ctx, qc.RunID)
	if err != nil {
		return qc, fmt.Errorf("get run quality results for qc capture: %w", err)
	}
	reviewItems, err := r.client.GetRunReviewItems(ctx, qc.RunID, true)
	if err != nil {
		return qc, fmt.Errorf("get run review items for qc capture: %w", err)
	}

	foundAutoQR := false
	var qcIDs []string
	for _, qr := range qcResults {
		qcIDs = append(qcIDs, qr.ID)
		if qr.ID == autoQRID {
			foundAutoQR = true
		}
	}
	if !foundAutoQR {
		return qc, fmt.Errorf("fail-closed: automated quality result %s not captured in run %s quality results", autoQRID, qc.RunID)
	}
	var revIDs []string
	for _, ri := range reviewItems {
		revIDs = append(revIDs, ri.ID)
	}

	r.session.RecordRelationalQC(input.CaseID, RelationalQCEvidence{
		QualityResultIDs: qcIDs,
		QualityResults:   qcResults,
		ReviewItemIDs:    revIDs,
		ReviewItems:      reviewItems,
	})

	// Query all run decisions for complete case provenance
	runDecisions, err := r.client.ListDecisions(ctx, qc.RunID, "")
	if err == nil && len(runDecisions) > 0 {
		var caseDecs []SelectionDecisionRef
		for _, d := range runDecisions {
			caseDecs = append(caseDecs, SelectionDecisionRef{
				DecisionID:         d.ID,
				Stage:              d.Stage,
				SelectedProviderID: d.SelectedProviderID,
				SelectedCandidate:  d.SelectedProviderID,
				PolicyCheckResult:  d.PolicyCheckResult,
				Reason:             d.DecisionReason,
				Timestamp:          d.CreatedAt,
			})
		}
		qc.Decisions = caseDecs
	}
	now := time.Now().UTC()
	qc = r.session.QualityCases[input.CaseID]
	qc.Status = "COMPLETED"
	qc.CompletedAt = &now
	if input.ReferencePack != nil {
		m := EvaluateCaseQuality(qc, input.ReferencePack)
		qc.Metrics = &m
	}
	r.session.RecordQualityCase(*qc)

	// Keep unused CAS reference variables suppression-free
	_ = audioRoleCAS
	_ = transcriptCAS
	_ = visTrackCAS

	if err := r.store.Save(r.session); err != nil {
		return qc, fmt.Errorf("save session at case completion: %w", err)
	}

	return qc, nil
}

const (
	BenchmarkQCProducerKey   = "producer"
	BenchmarkQCProducerValue = "douyinie_benchmark_runner"
	BenchmarkQCSchemaKey     = "benchmark_qc_schema"
	BenchmarkQCSchemaVersion = 1
)

// IsBenchmarkOwnedQualityResult checks if a QualityResult is owned by the benchmark runner
// and matches the current execution scope (runID, jobID, assetID, targetLanguage).
func IsBenchmarkOwnedQualityResult(qr domain.QualityResult, runID, jobID, assetID, targetLanguage string) bool {
	if qr.Stage != "multimodal_qc" {
		return false
	}
	if qr.RunID != runID || qr.JobID != jobID || qr.AssetID != assetID || qr.TargetLanguage != targetLanguage {
		return false
	}
	if qr.Details == nil {
		return false
	}
	producer, ok := qr.Details[BenchmarkQCProducerKey]
	if !ok || producer != BenchmarkQCProducerValue {
		return false
	}
	schemaVal, ok := qr.Details[BenchmarkQCSchemaKey]
	if !ok {
		return false
	}
	switch v := schemaVal.(type) {
	case int:
		return v == BenchmarkQCSchemaVersion
	case float64:
		return int(v) == BenchmarkQCSchemaVersion
	case json.Number:
		n, err := v.Int64()
		return err == nil && int(n) == BenchmarkQCSchemaVersion
	case string:
		return v == "1"
	default:
		return false
	}
}

// deriveAutomatedQualityResult deterministically evaluates multimodal AV/QC signals
// directly from observed typed stage artifacts and frozen reference annotations.
// It never fabricates acoustic scores or constants and fails closed to REVIEW_REQUIRED or FAIL
// when required signals or artifacts are missing or ambiguous.
func deriveAutomatedQualityResult(qc *QualityCaseEvidence, pack *ReferenceAnnotationPack, isNoDub bool) domain.QualityResult {
	qr := domain.QualityResult{
		ID:             uuid.NewString(),
		RunID:          qc.RunID,
		JobID:          qc.JobID,
		AssetID:        qc.SourceAssetID,
		TargetLanguage: qc.TargetLanguage,
		Stage:          "multimodal_qc",
		OverallStatus:  domain.QualityStatusPass,
		CreatedAt:      time.Now().UTC(),
		Details: map[string]any{
			BenchmarkQCProducerKey: BenchmarkQCProducerValue,
			BenchmarkQCSchemaKey:   BenchmarkQCSchemaVersion,
		},
	}

	var metrics []domain.QualityMetric
	var issues []domain.ReviewItem

	failReason := ""
	reviewReason := ""

	// 1. Voice stability and distinguishability
	if isNoDub {
		metrics = append(metrics, domain.QualityMetric{
			Name:        "voice_distinguishability",
			Score:       1.0,
			Threshold:   1.0,
			Passed:      true,
			Description: "no-dub asset; voice distinguishability not applicable",
		})
	} else {
		if qc.StageArtifacts == nil || qc.StageArtifacts.VoiceAssignment == nil {
			failReason = "missing voice assignment artifact"
			metrics = append(metrics, domain.QualityMetric{
				Name:        "voice_distinguishability",
				Score:       0.0,
				Threshold:   1.0,
				Passed:      false,
				Description: "missing voice assignment artifact",
			})
		} else {
			va := qc.StageArtifacts.VoiceAssignment
			if va.Distinguishability == nil {
				reviewReason = "voice assignment missing distinguishability QC"
				metrics = append(metrics, domain.QualityMetric{
					Name:        "voice_distinguishability",
					Score:       0.5,
					Threshold:   1.0,
					Passed:      false,
					Description: "voice assignment missing distinguishability QC report",
				})
			} else {
				distQC := va.Distinguishability
				score := 1.0
				passed := true
				if distQC.Status != "PASS" {
					score = 0.5
					passed = false
					reviewReason = fmt.Sprintf("voice distinguishability %s: %s", distQC.Status, strings.Join(distQC.Issues, "; "))
				}
				metrics = append(metrics, domain.QualityMetric{
					Name:        "voice_distinguishability",
					Score:       score,
					Threshold:   1.0,
					Passed:      passed,
					Description: fmt.Sprintf("multi_speaker=%t distinct_voices=%d/%d status=%s", distQC.MultiSpeaker, distQC.DistinctVoiceIDs, distQC.SpeakerCount, distQC.Status),
				})
			}
		}
	}

	// 2. Soundtrack preservation and dialogue suppression
	if isNoDub {
		metrics = append(metrics, domain.QualityMetric{
			Name:        "soundtrack_preservation",
			Score:       1.0,
			Threshold:   1.0,
			Passed:      true,
			Description: "no-dub asset; soundtrack preservation not applicable",
		})
		metrics = append(metrics, domain.QualityMetric{
			Name:        "dialogue_suppression",
			Score:       1.0,
			Threshold:   1.0,
			Passed:      true,
			Description: "no-dub asset; dialogue suppression not applicable",
		})
	} else if qc.StageArtifacts == nil || qc.StageArtifacts.DubMix == nil {
		failReason = "missing dub mix artifact"
		metrics = append(metrics, domain.QualityMetric{
			Name:        "soundtrack_preservation",
			Score:       0.0,
			Threshold:   1.0,
			Passed:      false,
			Description: "missing dub mix artifact",
		})
	} else {
		dm := qc.StageArtifacts.DubMix
		if dm.OverallStatus == "REFUSED" {
			failReason = fmt.Sprintf("dub mix refused: %s", dm.RefusalReason)
			metrics = append(metrics, domain.QualityMetric{
				Name:        "soundtrack_preservation",
				Score:       0.0,
				Threshold:   1.0,
				Passed:      false,
				Description: fmt.Sprintf("dub mix refused: %s", dm.RefusalReason),
			})
		} else if dm.OverallStatus == "REVIEW_REQUIRED" {
			if reviewReason == "" {
				reviewReason = fmt.Sprintf("dub mix flagged review required: %s", dm.RefusalReason)
			}
			metrics = append(metrics, domain.QualityMetric{
				Name:        "soundtrack_preservation",
				Score:       0.5,
				Threshold:   1.0,
				Passed:      false,
				Description: fmt.Sprintf("dub mix review required: %s", dm.RefusalReason),
			})
		} else if !dm.SoundtrackPreserved {
			failReason = "soundtrack not preserved in dub mix"
			metrics = append(metrics, domain.QualityMetric{
				Name:        "soundtrack_preservation",
				Score:       0.0,
				Threshold:   1.0,
				Passed:      false,
				Description: "soundtrack not preserved in audio mix",
			})
		} else {
			metrics = append(metrics, domain.QualityMetric{
				Name:        "soundtrack_preservation",
				Score:       1.0,
				Threshold:   1.0,
				Passed:      true,
				Description: "soundtrack preserved with valid mix artifact",
			})
		}

		// Dialogue suppression
		if dm.DialogueSuppressed {
			metrics = append(metrics, domain.QualityMetric{
				Name:        "dialogue_suppression",
				Score:       1.0,
				Threshold:   1.0,
				Passed:      true,
				Description: "source dialogue suppressed inside speech windows",
			})
		} else {
			if reviewReason == "" {
				reviewReason = "source dialogue suppression not verified in dub mix"
			}
			metrics = append(metrics, domain.QualityMetric{
				Name:        "dialogue_suppression",
				Score:       0.5,
				Threshold:   1.0,
				Passed:      false,
				Description: "source dialogue suppression not verified",
			})
		}
	}

	// 3. Render integrity
	if qc.StageArtifacts == nil || qc.StageArtifacts.FinalRender == nil {
		failReason = "missing final render artifact"
		metrics = append(metrics, domain.QualityMetric{
			Name:        "render_integrity",
			Score:       0.0,
			Threshold:   1.0,
			Passed:      false,
			Description: "missing final render artifact",
		})
	} else {
		fr := qc.StageArtifacts.FinalRender
		if fr.OverallStatus != "PASS" || fr.OutputCASHash == "" || fr.OutputByteSize <= 0 || fr.OutputDurationMs <= 0 {
			failReason = fmt.Sprintf("final render failed integrity check (status=%s, cas=%s, size=%d, dur=%d)", fr.OverallStatus, fr.OutputCASHash, fr.OutputByteSize, fr.OutputDurationMs)
			metrics = append(metrics, domain.QualityMetric{
				Name:        "render_integrity",
				Score:       0.0,
				Threshold:   1.0,
				Passed:      false,
				Description: "final render failed status or output dimension checks",
			})
		} else {
			metrics = append(metrics, domain.QualityMetric{
				Name:        "render_integrity",
				Score:       1.0,
				Threshold:   1.0,
				Passed:      true,
				Description: fmt.Sprintf("final render PASS (bytes=%d, dur_ms=%d)", fr.OutputByteSize, fr.OutputDurationMs),
			})
		}
	}

	// 4. Burned-in subtitle replacement
	hasBurnedInRef := pack != nil && pack.HasBurnedInSubtitles && len(pack.SubtitleRegions) > 0
	if !hasBurnedInRef {
		metrics = append(metrics, domain.QualityMetric{
			Name:        "burned_in_subtitle_replacement",
			Score:       1.0,
			Threshold:   1.0,
			Passed:      true,
			Description: "no burned-in subtitle regions in reference annotation",
		})
	} else {
		// ReferenceSubtitleRegion specifically describes burned-in dialogue subtitle regions.
		// Existing product code generates LocalizedOverlayItem only for semantic_text/instructional_ui_text;
		// speech_subtitle is rendered as compact SubtitleCue, so retained visual/subtitle artifacts cannot prove
		// that the source burned-in dialogue subtitle was covered or eliminated.
		// Therefore, automated evaluation must remain REVIEW_REQUIRED without fabricating a PASS.
		var hasAnyVisualSubtitleArtifacts bool
		if qc.StageArtifacts != nil {
			if qc.StageArtifacts.LocalizedVisualTrack != nil && (len(qc.StageArtifacts.LocalizedVisualTrack.SubtitleCues) > 0 || len(qc.StageArtifacts.LocalizedVisualTrack.Overlays) > 0) {
				hasAnyVisualSubtitleArtifacts = true
			}
			if qc.StageArtifacts.LocalizedSubtitleTrack != nil && len(qc.StageArtifacts.LocalizedSubtitleTrack.Cues) > 0 {
				hasAnyVisualSubtitleArtifacts = true
			}
		}

		if !hasAnyVisualSubtitleArtifacts {
			if reviewReason == "" {
				reviewReason = "burned-in subtitle regions present in reference but no visual or subtitle artifacts observed"
			}
			metrics = append(metrics, domain.QualityMetric{
				Name:        "burned_in_subtitle_replacement",
				Score:       0.0,
				Threshold:   1.0,
				Passed:      false,
				Description: "no localized subtitle cues or overlays to address reference subtitle regions",
			})
		} else {
			if reviewReason == "" {
				reviewReason = fmt.Sprintf("burned-in dialogue subtitle replacement requires visual review: retained visual/subtitle artifacts cannot prove elimination of %d source dialogue subtitle regions", len(pack.SubtitleRegions))
			}
			metrics = append(metrics, domain.QualityMetric{
				Name:        "burned_in_subtitle_replacement",
				Score:       0.5,
				Threshold:   1.0,
				Passed:      false,
				Description: fmt.Sprintf("burned-in dialogue subtitle replacement unverified: %d reference regions require visual inspection", len(pack.SubtitleRegions)),
			})
		}
	}

	qr.Metrics = metrics
	if failReason != "" {
		qr.OverallStatus = domain.QualityStatusFail
		issues = append(issues, domain.ReviewItem{
			ID:             uuid.NewString(),
			RunID:          qc.RunID,
			AssetID:        qc.SourceAssetID,
			JobID:          qc.JobID,
			TargetLanguage: qc.TargetLanguage,
			Type:           domain.ReviewItemTypeVisualOcclusion,
			Stage:          "multimodal_qc",
			Severity:       "blocker",
			Reason:         failReason,
			Status:         domain.ReviewItemStatusPending,
			CreatedAt:      time.Now().UTC(),
		})
	} else if reviewReason != "" {
		qr.OverallStatus = domain.QualityStatusReviewRequired
		issues = append(issues, domain.ReviewItem{
			ID:             uuid.NewString(),
			RunID:          qc.RunID,
			AssetID:        qc.SourceAssetID,
			JobID:          qc.JobID,
			TargetLanguage: qc.TargetLanguage,
			Type:           domain.ReviewItemTypeVisualOcclusion,
			Stage:          "multimodal_qc",
			Severity:       "warning",
			Reason:         reviewReason,
			Status:         domain.ReviewItemStatusPending,
			CreatedAt:      time.Now().UTC(),
		})
	} else {
		qr.OverallStatus = domain.QualityStatusPass
	}
	qr.Issues = issues
	return qr
}

// DeriveAutomatedQualityResultForTest exposes deriveAutomatedQualityResult for unit tests.
func DeriveAutomatedQualityResultForTest(qc *QualityCaseEvidence, pack *ReferenceAnnotationPack, isNoDub bool) domain.QualityResult {
	return deriveAutomatedQualityResult(qc, pack, isNoDub)
}
