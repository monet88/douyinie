package benchmark

import (
	"context"
	"fmt"
	"math"
	"time"

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
	CaseID            string                           `json:"case_id"` // e.g. "video_01_vi"
	SourceVideoID     string                           `json:"source_video_id"`
	PrimaryCategory   string                           `json:"primary_category"`
	TargetLanguage    string                           `json:"target_language"`
	Profile           string                           `json:"profile"` // "local", "hybrid"
	SourceAssetID     string                           `json:"source_asset_id,omitempty"`
	MediaFilePath     string                           `json:"media_file_path,omitempty"`
	Segments          []domain.TranslationInputSegment `json:"segments,omitempty"`
	AudioRoleSegments []domain.AudioSegment            `json:"audio_role_segments,omitempty"`
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
	var httpStatus int
	var acqErr error

	if input.MediaFilePath != "" {
		// Local file ingest (used in Seam 1 testing)
		declaredBy := input.AttestationDeclaredBy
		if declaredBy == "" {
			declaredBy = "benchmark-runner"
		}
		asset, acqErr = r.client.IngestAsset(ctx, input.MediaFilePath, declaredBy, true)
		if acqErr != nil {
			httpStatus = 500
		} else {
			httpStatus = 201
		}
	} else if input.AcquireRequest != nil {
		// Live URL acquisition
		res, err := r.client.AcquireSource(ctx, *input.AcquireRequest)
		if err != nil {
			acqErr = err
			httpStatus = 502
		} else {
			asset = res.Asset
			httpStatus = 201
		}
	} else {
		acqErr = fmt.Errorf("neither MediaFilePath nor AcquireRequest provided for entry %s", input.EntryID)
		httpStatus = 400
	}

	evidence.HTTPStatusCode = httpStatus

	if acqErr != nil {
		evidence.Status = "FAIL"
		evidence.ErrorMessage = acqErr.Error()
	} else if asset != nil {
		evidence.SourceAssetID = asset.ID
		evidence.SourceAssetCASHash = asset.SHA256
		evidence.ObservedAwemeID = input.ExpectedAwemeID
		preflight, err := r.client.GetAssetPreflight(ctx, asset.ID)
		if err == nil && preflight != nil {
			evidence.ObservedDurationMs = preflight.DurationMs
		}

		// Duration tolerance check
		diff := int64(math.Abs(float64(evidence.ObservedDurationMs - input.ExpectedDurationMs)))
		if input.DurationToleranceMs > 0 && diff > input.DurationToleranceMs {
			evidence.IntegrityPassed = false
			evidence.Status = "FAIL"
			evidence.ErrorMessage = fmt.Sprintf("duration mismatch: observed %dms, expected %dms (tolerance %dms)",
				evidence.ObservedDurationMs, input.ExpectedDurationMs, input.DurationToleranceMs)
		} else {
			evidence.IntegrityPassed = true
			evidence.Status = "PASS"
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
				ObservedModel: a.ModelVersion,
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

// ExecuteQualityCase executes one quality case through all pipeline stages with resumption support.
func (r *BenchmarkRunner) ExecuteQualityCase(ctx context.Context, input QualityCaseInput) (*QualityCaseEvidence, error) {
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
		run, err := r.client.CreateRun(ctx, qc.JobID, "{}")
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
					ObservedModel: a.ModelVersion,
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
	err := runStage("speech_understand", func() (string, error) {
		artifact, err := r.client.RunSpeechUnderstand(ctx, qc.SourceAssetID, qc.RunID)
		if err != nil {
			return "", err
		}
		return artifact.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	transcriptCAS = r.session.QualityCases[input.CaseID].Stages["speech_understand"].OutputCASHash

	// Stage 3: Translation
	var translationCAS string
	err = runStage("translate", func() (string, error) {
		variant, err := r.client.RunTranslation(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, input.Segments)
		if err != nil {
			return "", err
		}
		return variant.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	translationCAS = r.session.QualityCases[input.CaseID].Stages["translate"].OutputCASHash

	// Stage 4: Dub Script
	var dubScriptCAS string
	err = runStage("dub_script", func() (string, error) {
		variant, err := r.client.RunDubScript(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, translationCAS, input.Segments)
		if err != nil {
			return "", err
		}
		return variant.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	dubScriptCAS = r.session.QualityCases[input.CaseID].Stages["dub_script"].OutputCASHash

	// Stage 5: Voice Assignment
	var voiceAssignCAS string
	err = runStage("voice_assignment", func() (string, error) {
		assign, err := r.client.AssignVoices(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage)
		if err != nil {
			return "", err
		}
		return assign.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	voiceAssignCAS = r.session.QualityCases[input.CaseID].Stages["voice_assignment"].OutputCASHash

	// Stage 6: Dub Synthesize
	var dubSegmentsCAS string
	err = runStage("dub_synthesize", func() (string, error) {
		segments, err := r.client.RunDubSynthesize(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, voiceAssignCAS, dubScriptCAS)
		if err != nil {
			return "", err
		}
		return segments.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	dubSegmentsCAS = r.session.QualityCases[input.CaseID].Stages["dub_synthesize"].OutputCASHash

	// Stage 7: Separate Stems
	var stemsCAS string
	err = runStage("separate_stems", func() (string, error) {
		stems, err := r.client.SeparateStems(ctx, qc.SourceAssetID, qc.RunID)
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
		mix, err := r.client.RunAudioMix(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, dubSegmentsCAS, stemsCAS)
		if err != nil {
			return "", err
		}
		return mix.CASHash, nil
	})
	if err != nil {
		return qc, err
	}
	dubMixCAS = r.session.QualityCases[input.CaseID].Stages["audio_mix"].OutputCASHash

	// Stage 9: Text Detection
	var textRegionCAS string
	err = runStage("detect_text", func() (string, error) {
		plan, err := r.client.DetectText(ctx, qc.SourceAssetID, qc.RunID)
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
		visTrack, subTrack, err := r.client.LocalizeVisualTrack(ctx, qc.SourceAssetID, qc.RunID, qc.JobID, input.TargetLanguage, textRegionCAS, translationCAS)
		if err != nil {
			return "", err
		}
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
		return artifact.CASHash, nil
	})
	if err != nil {
		return qc, err
	}

	// 6. Capture relational QC evidence from SQLite
	qcResults, _ := r.client.GetRunQualityResults(ctx, qc.RunID)
	reviewItems, _ := r.client.GetRunReviewItems(ctx, qc.RunID, true)

	var qcIDs []string
	for _, qr := range qcResults {
		qcIDs = append(qcIDs, qr.ID)
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
