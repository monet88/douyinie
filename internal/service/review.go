package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// ReviewService projects actionable exceptions from stage artifacts into structured ReviewItem projections.
// Invariant: Exception-only review — passing or auto-resolved items do not surface in the review queue.
type ReviewService struct {
	db             *storage.DB
	cas            *cas.Store
	translationSvc *TranslationService
	dubbingSvc     *DubbingService
	audioMixSvc    *AudioMixService
	visualTextSvc  *VisualTextService
	renderSvc      *RenderService
}

// NewReviewService creates a new ReviewService instance.
func NewReviewService(db *storage.DB, casStore *cas.Store) *ReviewService {
	return &ReviewService{
		db:  db,
		cas: casStore,
	}
}

// SetTranslationService injects the TranslationService used for translation rerun.
func (s *ReviewService) SetTranslationService(svc *TranslationService) {
	s.translationSvc = svc
}

// SetDubbingService injects the DubbingService used for TTS synthesis rerun.
func (s *ReviewService) SetDubbingService(svc *DubbingService) {
	s.dubbingSvc = svc
}

// SetAudioMixService injects the AudioMixService used for stem re-mixing.
func (s *ReviewService) SetAudioMixService(svc *AudioMixService) {
	s.audioMixSvc = svc
}

// SetVisualTextService injects the VisualTextService used for subtitle track regeneration.
func (s *ReviewService) SetVisualTextService(svc *VisualTextService) {
	s.visualTextSvc = svc
}

// SetRenderService injects the RenderService used for render plan refreezing.
func (s *ReviewService) SetRenderService(svc *RenderService) {
	s.renderSvc = svc
}

// ManualOverrideInput defines parameters for recording an operator manual override.
type ManualOverrideInput struct {
	RunID          string                `json:"run_id,omitempty"`
	JobID          string                `json:"job_id,omitempty"`
	AssetID        string                `json:"asset_id"`
	TargetLanguage string                `json:"target_language"`
	ReviewItemID   string                `json:"review_item_id"`
	ItemType       domain.ReviewItemType `json:"item_type"`
	Stage          string                `json:"stage"`
	ItemIndex      int                   `json:"item_index,omitempty"`
	SegmentID      string                `json:"segment_id,omitempty"`
	RegionID       string                `json:"region_id,omitempty"`
	Reason         string                `json:"reason"`
	Operator       string                `json:"operator"`
}

// RecordManualOverride records an auditable operator acceptance of a flagged exception.

func (s *ReviewService) validateTranslationCompatibility(ctx context.Context, requestedRunID string, in TargetTextCorrectionInput, tVar *domain.TranslationVariant, transIdx *storage.TranslationVariantIndex) error {
	if tVar.AssetID != in.AssetID || !strings.EqualFold(tVar.TargetLanguage, in.TargetLanguage) {
		return fmt.Errorf("translation variant artifact %s belongs to asset %s (%s), expected asset %s (%s)", transIdx.CASHash, tVar.AssetID, tVar.TargetLanguage, in.AssetID, in.TargetLanguage)
	}
	if tVar.SchemaVersion != domain.TranslationSchemaVersion || tVar.ContractID != TranslationContractID {
		return fmt.Errorf("translation variant %s uses stale translation contract (schema=%d contract=%q)", transIdx.CASHash, tVar.SchemaVersion, tVar.ContractID)
	}

	// 1. Check pinned transcript lineage
	if s.db != nil && requestedRunID != "" {
		if strings.TrimSpace(tVar.TranscriptArtifactCAS) == "" {
			return fmt.Errorf("translation variant %s is missing transcript lineage", transIdx.CASHash)
		}
		runTranscriptCAS, err := resolveRunTranscriptCAS(ctx, s.db, requestedRunID, in.AssetID, "correction")
		if err != nil {
			return fmt.Errorf("resolve run transcript for correction: %w", err)
		}
		if runTranscriptCAS != "" && tVar.TranscriptArtifactCAS != runTranscriptCAS {
			return fmt.Errorf("translation variant transcript lineage mismatch: variant=%s run=%s", tVar.TranscriptArtifactCAS, runTranscriptCAS)
		}
	}
	// 2. Check frozen run glossary
	if s.db != nil && requestedRunID != "" {
		run, err := s.db.GetRun(ctx, requestedRunID)
		if err != nil {
			return fmt.Errorf("load run %s for correction glossary check: %w", requestedRunID, err)
		}
		if run == nil {
			return fmt.Errorf("run %s not found for correction glossary check", requestedRunID)
		}
		if tVar.EffectiveGlossary.Hash == "" {
			return fmt.Errorf("translation variant %s is missing effective glossary hash", transIdx.CASHash)
		}
		frozenGlossary, hasFrozen, err := decodeFrozenRunGlossary(run.ConfigSnapshotJSON)
		if err != nil {
			return fmt.Errorf("malformed run config snapshot JSON: %w", err)
		}
		if hasFrozen {
			var segs []domain.TranslationInputSegment
			for _, seg := range tVar.Segments {
				segs = append(segs, domain.TranslationInputSegment{
					Index:      seg.Index,
					SourceText: seg.SourceText,
					SpeakerID:  seg.SpeakerID,
					StartMs:    seg.StartMs,
					EndMs:      seg.EndMs,
				})
			}
			effective, err := effectiveGlossary(frozenGlossary, segs)
			if err != nil {
				return fmt.Errorf("compute effective glossary for correction: %w", err)
			}
			if tVar.EffectiveGlossary.Hash != effective.Hash {
				return fmt.Errorf("translation variant effective glossary mismatch: variant=%s run=%s", tVar.EffectiveGlossary.Hash, effective.Hash)
			}
		}
	}

	// 3. Check InputHash against canonical segments from transcript
	if s.translationSvc != nil && tVar.TranscriptArtifactCAS != "" && s.cas != nil {
		if strings.TrimSpace(tVar.InputHash) == "" {
			return fmt.Errorf("translation variant %s is missing input hash", transIdx.CASHash)
		}
		// Canonical segmentation is re-derived from the run's own pinned role-plan lineage: the
		// operator's current plan for the asset may have been edited since the run froze this
		// variant, which must not re-segment (and so invalidate) an otherwise valid correction.
		canonicalSegments, _, err := s.translationSvc.loadSegmentsFromTranscript(ctx, in.AssetID, requestedRunID, tVar.TranscriptArtifactCAS)
		if err != nil {
			return fmt.Errorf("load canonical segments for correction: %w", err)
		}
		if len(canonicalSegments) == 0 {
			return fmt.Errorf("transcript %s contains no canonical translation segments", tVar.TranscriptArtifactCAS)
		}
		jobIn := domain.TranslationJobInput{
			AssetID:               in.AssetID,
			RunID:                 requestedRunID,
			SourceLanguage:        tVar.SourceLanguage,
			TargetLanguage:        in.TargetLanguage,
			Segments:              canonicalSegments,
			EffectiveGlossary:     tVar.EffectiveGlossary,
			TranscriptArtifactCAS: tVar.TranscriptArtifactCAS,
		}
		expectedInputHash, err := s.translationSvc.computeTranslationInputHash(jobIn)
		if err != nil {
			return fmt.Errorf("compute expected input hash for correction: %w", err)
		}
		if tVar.InputHash != expectedInputHash {
			return fmt.Errorf("translation variant input hash mismatch: variant=%s expected=%s", tVar.InputHash, expectedInputHash)
		}
	}

	// 4. Check Provider/Model lineage if router is configured
	if s.translationSvc != nil && s.translationSvc.router != nil {
		jobIn := domain.TranslationJobInput{
			AssetID:               in.AssetID,
			RunID:                 requestedRunID,
			SourceLanguage:        tVar.SourceLanguage,
			TargetLanguage:        in.TargetLanguage,
			EffectiveGlossary:     tVar.EffectiveGlossary,
			TranscriptArtifactCAS: tVar.TranscriptArtifactCAS,
		}
		routeRes, err := s.translationSvc.router.Route(ctx, translationRouteRequest(jobIn))
		if err != nil {
			return fmt.Errorf("route provider for correction check: %w", err)
		}
		if routeRes == nil || routeRes.SelectedProvider == nil {
			return errors.New("no eligible translation provider selected for correction check")
		}
		if strings.TrimSpace(tVar.ProviderID) == "" {
			return fmt.Errorf("translation variant %s is missing provider lineage", transIdx.CASHash)
		}
		// The variant froze the provider that produced it. Translation's production ladder scores by exact
		// provider ID and Router deliberately ignores PreferredProviderID for this stage, so the frozen
		// provider is proven still eligible through the route result instead: selected, or standing among
		// the eligible fallback candidates. Health, policy, license or credential loss removes it from both
		// and the correction fails closed.
		if !frozenProviderEligible(routeRes, tVar.ProviderID) {
			return fmt.Errorf("translation variant provider mismatch: variant=%s not eligible in the current provider route (active=%s)", tVar.ProviderID, routeRes.SelectedProvider.ID())
		}
	}
	return nil
}

// frozenProviderEligible reports whether the router still proves a frozen provider eligible for the stage:
// it is the selected candidate or stands among the eligible fallback candidates. A provider the router
// dropped (unhealthy, policy-blocked, unlicensed, unauthorized) is absent from both.
func frozenProviderEligible(res *provider.RouteResult, providerID string) bool {
	if res == nil || res.SelectedProvider == nil {
		return false
	}
	if isProviderEquivalent(res.SelectedProvider.ID(), providerID) {
		return true
	}
	for _, candidate := range res.FallbackOrdered {
		if candidate != nil && isProviderEquivalent(candidate.ID(), providerID) {
			return true
		}
	}
	return false
}

// Invariant: Overrides are append-only audit records; historical QA scores and failures are never mutated or rewritten to PASS.
// Invariant: Manual override requires an exact, currently pending ReviewItemID for the same asset and language.
// Stage-only or implicit index-0 requests, nonexistent IDs, stale IDs, or non-pending IDs must be rejected.
func (s *ReviewService) RecordManualOverride(ctx context.Context, in ManualOverrideInput) (*domain.ReviewOverride, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if strings.TrimSpace(in.ReviewItemID) == "" {
		return nil, errors.New("exact pending review_item_id is required for manual override")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, errors.New("override reason/notes is required for audit trail")
	}
	if in.TargetLanguage == "" {
		in.TargetLanguage = "vi"
	}
	if strings.TrimSpace(in.Operator) == "" {
		in.Operator = "operator"
	}

	// 1. Verify that the requested review item is currently pending for the
	// requested run when available. This prevents a same-asset newer run from
	// satisfying an override submitted from an older run's inspector.
	var pendingItems []domain.ReviewItem
	var err error
	if strings.TrimSpace(in.RunID) != "" {
		pendingItems, err = s.ProjectReviewItemsForRun(ctx, in.AssetID, in.TargetLanguage, in.RunID)
	} else {
		pendingItems, err = s.ProjectReviewItems(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil {
		return nil, fmt.Errorf("project review items: %w", err)
	}

	var targetItem *domain.ReviewItem
	for i := range pendingItems {
		if pendingItems[i].ID == in.ReviewItemID {
			targetItem = &pendingItems[i]
			break
		}
	}
	if targetItem == nil {
		return nil, fmt.Errorf("review item %q not found in pending review queue for asset %s (%s)", in.ReviewItemID, in.AssetID, in.TargetLanguage)
	}
	if targetItem.Type == domain.ReviewItemTypeTTSOverrun && targetItem.Stage == "dub_synthesize" {
		return nil, fmt.Errorf("review item %q is a hard timing blocker and cannot be manually overridden; correct or re-synthesize the segment", targetItem.ID)
	}

	runID := in.RunID
	if runID == "" {
		runID = targetItem.RunID
	}
	jobID := in.JobID
	if jobID == "" {
		jobID = targetItem.JobID
	}

	override := domain.ReviewOverride{
		ID:             uuid.NewString(),
		RunID:          runID,
		JobID:          jobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		ReviewItemID:   targetItem.ID,
		ItemType:       targetItem.Type,
		Stage:          targetItem.Stage,
		ItemIndex:      targetItem.ItemIndex,
		SegmentID:      targetItem.SegmentID,
		RegionID:       targetItem.RegionID,
		Action:         string(domain.ReviewOverrideActionManualOverride),
		Reason:         in.Reason,
		Operator:       in.Operator,
		CreatedAt:      time.Now().UTC(),
	}

	if err := s.db.SaveReviewOverride(ctx, override); err != nil {
		return nil, fmt.Errorf("persist review override: %w", err)
	}

	return &override, nil
}

// ---------------------------------------------------------------------------
// Issue #157 - exact reviewed-candidate selection
// ---------------------------------------------------------------------------

// ReviewedCandidateKind names which of a review unit's own waveforms an acceptance selects.
type ReviewedCandidateKind string

const (
	// ReviewedCandidateNatural selects the unit's retained natural waveform itself. It is legal
	// only when the actual committed bytes fit the accepted playback window frame-exactly - the
	// one case a floored whole-millisecond overrun probe cannot rule out.
	ReviewedCandidateNatural ReviewedCandidateKind = "natural"
	// ReviewedCandidateTransformed selects the review-only atempo alternative of #156. The
	// operator must waive the transformed-audio quality concern explicitly; the run never does,
	// and duration fit alone never becomes a quality PASS.
	ReviewedCandidateTransformed ReviewedCandidateKind = "transformed"
)

// The reviewed-candidate selection refusals. Every one is a refusal, never a silent downgrade:
// timing, absent audio, wrong ownership, invalid lineage and incomplete required coverage are
// hard gates that no override argument waives.
var (
	// ErrReviewedCandidateNotPending reports a review item that is not currently unresolved
	// against the run's own dubbing artifact.
	ErrReviewedCandidateNotPending = errors.New("reviewed candidate selection refused: the review item is not pending for this run")
	// ErrReviewedCandidateStale reports a review item projected from a dubbing artifact the run
	// no longer holds, i.e. a concurrent acceptance, synthesis or reassignment moved on.
	ErrReviewedCandidateStale = errors.New("reviewed candidate selection refused: the review item belongs to a superseded dubbing artifact")
	// ErrReviewedCandidateNotSelectable reports a candidate whose actual bytes do not fit the
	// accepted playback window under the governing policy.
	ErrReviewedCandidateNotSelectable = errors.New("reviewed candidate selection refused: the candidate does not fit the accepted playback window")
	// ErrReviewedCandidateUnavailable reports a named candidate waveform that is missing,
	// unreadable, corrupt or not valid 16-bit PCM audio.
	ErrReviewedCandidateUnavailable = errors.New("reviewed candidate selection refused: the named candidate waveform is unavailable")
	// ErrReviewedCandidateWaiverRequired reports a transformed-audio selection without the
	// explicit operator quality waiver.
	ErrReviewedCandidateWaiverRequired = errors.New("reviewed candidate selection refused: accepting a transformed waveform requires an explicit manual_override quality waiver")
	// ErrReviewedCandidateConflict reports a run whose dubbing artifact changed while this
	// selection was being applied, or a repeated selection with different parameters.
	ErrReviewedCandidateConflict = errors.New("reviewed candidate selection refused: the run's dubbing artifact changed while this selection was being applied")
	// ErrReviewedCandidateInvalid reports a malformed selection request: a missing asset, run,
	// item or audit reason, or an unsupported candidate/language. It is a caller error, not a
	// gate the operator can argue with.
	ErrReviewedCandidateInvalid = errors.New("reviewed candidate selection refused: malformed selection request")
)

// AcceptReviewedCandidateInput names the exact unresolved review unit and the exact waveform an
// operator accepts for it.
type AcceptReviewedCandidateInput struct {
	RunID          string `json:"run_id"`
	JobID          string `json:"job_id,omitempty"`
	AssetID        string `json:"asset_id"`
	TargetLanguage string `json:"target_language"`
	// ReviewItemID is the exact pending review item, which carries the dubbing artifact identity
	// the operator was looking at.
	ReviewItemID string `json:"review_item_id"`
	// Candidate selects the natural parent or the transformed tempo alternative.
	Candidate      ReviewedCandidateKind `json:"candidate"`
	ManualOverride bool                  `json:"manual_override,omitempty"`
	// Reason and Operator are the auditable decision note and actor.
	Reason                string                  `json:"reason"`
	Operator              string                  `json:"operator,omitempty"`
	ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
}

// AcceptReviewedCandidateResult reports the artifacts the acceptance produced and what the run may
// do next. It never reports an acceptance as complete while unresolved coverage remains.
type AcceptReviewedCandidateResult struct {
	AssetID          string `json:"asset_id"`
	RunID            string `json:"run_id"`
	JobID            string `json:"job_id,omitempty"`
	TargetLanguage   string `json:"target_language"`
	ReviewItemID     string `json:"review_item_id"`
	ReviewOverrideID string `json:"review_override_id,omitempty"`
	// DubSegmentsVariantCAS is the successor variant carrying the acceptance evidence.
	DubSegmentsVariantCAS string `json:"dub_segments_variant_cas"`
	SelectedAudioSHA256   string `json:"selected_audio_sha256"`
	NaturalAudioSHA256    string `json:"natural_audio_sha256,omitempty"`
	Transformed           bool   `json:"transformed"`
	QualityWaiver         bool   `json:"quality_waiver"`
	// RemainingReviewCount is how many review units the successor variant still carries.
	RemainingReviewCount int  `json:"remaining_review_count"`
	CoverageComplete     bool `json:"coverage_complete"`
	Idempotent           bool `json:"idempotent"`
	// RunPaused reports that the run is still paused: coverage is incomplete, so no partial mix was
	// ever produced, or the run is in Review posture awaiting the operator's explicit final render.
	// It is false only once an automatic final render released the run.
	RunPaused bool `json:"run_paused"`
	// RunCompleted reports that the run and its job were finished (auto posture, gates passed).
	RunCompleted bool `json:"run_completed"`
	// DubMixCAS, RenderPlanCAS, PreviewRenderCAS and FinalRenderCAS are the rebuilt descendants.
	DubMixCAS        string                  `json:"dub_mix_cas,omitempty"`
	RenderPlanCAS    string                  `json:"render_plan_cas,omitempty"`
	PreviewRenderCAS string                  `json:"preview_render_cas,omitempty"`
	FinalRenderCAS   string                  `json:"final_render_cas,omitempty"`
	HandoffAction    string                  `json:"handoff_action,omitempty"`
	HandoffMessage   string                  `json:"handoff_message,omitempty"`
	Status           domain.ReviewItemStatus `json:"status"`
	Message          string                  `json:"message"`
}

// selectionLineage is the pinned lineage an acceptance is proven against.
type selectionLineage struct {
	transcript    *domain.TranscriptArtifact
	rolePlan      *domain.AudioRolePlan
	eligible      map[int]domain.SpeechBlock
	fitController *FitController
}

// selectedWaveform is one accepted waveform after its actual bytes and frame geometry were proven.
type selectedWaveform struct {
	hash          string
	audioCASPath  string
	naturalHash   string
	transformed   bool
	tempoFactor   float64
	tempoFilter   string
	tempoToolID   string
	qualityWaiver bool
	measuredMs    int64
	frames        int64
	windowFrames  int64
}

// AcceptReviewedCandidate records the operator's acceptance of one exact reviewed candidate and
// drives the resulting rebuild (Issue #157).
//
// Invariants:
//   - The acceptance is proven against the run's CURRENT dubbing artifact, translation/dubbing
//     contract, governing playback policy and the actual committed waveform bytes. Timing,
//     missing media, wrong ownership, invalid lineage and incomplete coverage are refusals.
//   - A transformed waveform is only accepted under an explicit manual_override quality waiver.
//   - The successor variant appends acceptance evidence to the existing artifact family: the
//     original waveform, QA results, provider attempts and source anchors are preserved, and the
//     acceptance is linked to the exact ReviewOverride and immutable candidate hash.
//   - With other units unresolved the run stays paused and no partial mix is produced.
//   - Once coverage is complete the affected descendants are rebuilt from the accepted waveforms,
//     superseded preview/final refs are withdrawn, and the run's own posture decides between an
//     automatic final render and the explicit final-render action.
func (s *ReviewService) AcceptReviewedCandidate(ctx context.Context, in AcceptReviewedCandidateInput) (*AcceptReviewedCandidateResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, fmt.Errorf("%w: asset_id is required", ErrReviewedCandidateInvalid)
	}
	runID := strings.TrimSpace(in.RunID)
	if runID == "" {
		return nil, fmt.Errorf("%w: run_id is required for reviewed-candidate selection", ErrReviewedCandidateInvalid)
	}
	itemID := strings.TrimSpace(in.ReviewItemID)
	if itemID == "" {
		return nil, fmt.Errorf("%w: exact pending review_item_id is required for reviewed-candidate selection", ErrReviewedCandidateInvalid)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: override reason/notes is required for the acceptance audit trail", ErrReviewedCandidateInvalid)
	}
	kind := ReviewedCandidateKind(strings.ToLower(strings.TrimSpace(string(in.Candidate))))
	if kind == "" {
		kind = ReviewedCandidateNatural
	}
	if kind != ReviewedCandidateNatural && kind != ReviewedCandidateTransformed {
		return nil, fmt.Errorf("%w: unsupported candidate %q (must be %q or %q)", ErrReviewedCandidateInvalid, in.Candidate, ReviewedCandidateNatural, ReviewedCandidateTransformed)
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("%w: unsupported target language '%s' (must be 'vi' or 'en')", ErrReviewedCandidateInvalid, in.TargetLanguage)
	}
	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "operator"
	}
	if s.db == nil || s.cas == nil {
		return nil, errors.New("database and CAS store are required for reviewed-candidate selection")
	}
	if s.audioMixSvc == nil || s.renderSvc == nil {
		return nil, errors.New("audio mix and render services are required for reviewed-candidate selection")
	}

	// 1. Current run/job/asset/language binding. A selection is always an exact run-scoped act.
	run, err := s.db.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run %s for reviewed-candidate selection: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("%w: run %s", ErrReviewedCandidateNotPending, runID)
	}
	// A selection releases media into a run that is still in flight. A cancelled or interrupted
	// run keeps its history as evidence, but its stage pins are no longer the run's current
	// delivery: accepting for it would revive superseded output the operator never selected.
	switch run.Status {
	case domain.RunStatusCancelled, domain.RunStatusInterrupted:
		return nil, fmt.Errorf("%w: run %s is %s and cannot accept a reviewed candidate", ErrReviewedCandidateNotPending, runID, run.Status)
	}
	job, err := s.db.GetJob(ctx, run.JobID)
	if err != nil {
		return nil, fmt.Errorf("load job %s for reviewed-candidate selection: %w", run.JobID, err)
	}
	if job == nil {
		return nil, fmt.Errorf("%w: job %s", ErrReviewedCandidateNotPending, run.JobID)
	}
	if job.SourceAssetID != in.AssetID || !strings.EqualFold(job.TargetLanguage, targetLang) {
		return nil, fmt.Errorf("%w: run %s belongs to asset %s (%s), not %s (%s)",
			ErrReviewedCandidateNotPending, runID, job.SourceAssetID, job.TargetLanguage, in.AssetID, targetLang)
	}
	if in.JobID != "" && in.JobID != job.ID {
		return nil, fmt.Errorf("%w: run %s belongs to job %s, not %s", ErrReviewedCandidateNotPending, runID, job.ID, in.JobID)
	}

	// 2. The run's current dubbing artifact, verified to belong to this asset/language pair and
	// to the voice assignment still in force.
	idx, err := s.runScopedDubbingVariantIndex(ctx, runID, in.AssetID, targetLang)
	if err != nil {
		return nil, err
	}
	if idx == nil || idx.CASHash == "" {
		return nil, fmt.Errorf("%w: run %s owns no dubbing artifact", ErrReviewedCandidateNotPending, runID)
	}
	variant, err := s.loadRunDubbingVariant(idx, in.AssetID, targetLang)
	if err != nil {
		return nil, err
	}
	variant.CASHash = idx.CASHash
	if err := s.verifyVariantVoiceLineage(ctx, runID, in.AssetID, targetLang, variant); err != nil {
		return nil, err
	}

	// 3. A completed identical acceptance is idempotent: the successor variant already carries
	// the evidence, so nothing is appended, no duplicate override is written and the same result
	// is reported. A differently parameterized repeat of the same item fails closed.
	if existing := variant.AcceptedCandidateFor(itemID); existing != nil {
		if existing.Transformed != (kind == ReviewedCandidateTransformed) ||
			(!existing.Transformed && !strings.EqualFold(existing.SelectedAudioSHA256, existing.NaturalAudioSHA256)) {
			return nil, fmt.Errorf("%w: review item %s was already accepted with waveform %s",
				ErrReviewedCandidateConflict, itemID, existing.SelectedAudioSHA256)
		}
		return s.reportReplayedAcceptance(ctx, in, runID, job.ID, targetLang, variant, *existing)
	}

	// 4. Exact pending identity. The projected item names the artifact it came from and the fit
	// unit it belongs to; a unit that is not unresolved in THIS artifact is not selectable.
	itemVariantCAS, itemIndex, ok := parseDubReviewItemID(itemID)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a dubbing review item", ErrReviewedCandidateNotPending, itemID)
	}
	if !strings.EqualFold(itemVariantCAS, idx.CASHash) {
		return nil, fmt.Errorf("%w: review item %s was projected from dubbing artifact %s but run %s now holds %s",
			ErrReviewedCandidateStale, itemID, itemVariantCAS, runID, idx.CASHash)
	}
	rev, ok := findReviewUnit(variant, itemIndex)
	if !ok {
		return nil, fmt.Errorf("%w: fit unit %d is not unresolved in run %s's current dubbing artifact",
			ErrReviewedCandidateNotPending, itemIndex, runID)
	}
	if len(rev.SpeechBlockIndices) == 0 {
		return nil, fmt.Errorf("%w: fit unit %d carries no canonical source membership", ErrReviewedCandidateNotSelectable, rev.Index)
	}

	// 5. Translation/dubbing contract, voice lineage and governing playback policy.
	lineage, err := s.selectionLineage(ctx, runID, in.AssetID, targetLang, variant)
	if err != nil {
		return nil, err
	}
	// Group identity: every covered member must still be dub-eligible and covered exactly once
	// across the variant's units. A foreign, duplicate or ineligible member is not selectable.
	for _, member := range rev.SpeechBlockIndices {
		if _, ok := lineage.eligible[member]; !ok {
			return nil, fmt.Errorf("%w: fit unit %d references foreign or non-dub-eligible source member %d",
				ErrReviewedCandidateNotSelectable, rev.Index, member)
		}
	}
	if count := coverageCount(variant, itemIndex); count != 1 {
		return nil, fmt.Errorf("%w: source coverage for fit unit %d is not exactly one (%d)", ErrReviewedCandidateNotSelectable, rev.Index, count)
	}
	playbackEnd, reserve, err := lineage.acceptedPlaybackWindow(rev, variant.FitPolicyID)
	if err != nil {
		return nil, err
	}

	// 6. The named waveform's actual committed bytes, re-probed and proven frame-exactly against
	// the accepted playback window. This is the only evidence that can authorize the selection.
	selected, err := s.resolveSelectionWaveform(ctx, in.AssetID, rev, kind, playbackEnd, in.ManualOverride)
	if err != nil {
		return nil, err
	}

	// 7. Build the successor variant and its acceptance evidence.
	overrideID := uuid.NewString()
	acceptedAt := time.Now().UTC()
	evidence := domain.AcceptedReviewCandidate{
		ReviewItemID:        itemID,
		ReviewOverrideID:    overrideID,
		ReviewSegmentIndex:  rev.Index,
		SpeakerID:           rev.SpeakerID,
		NaturalAudioSHA256:  selected.naturalHash,
		SelectedAudioSHA256: selected.hash,
		Transformed:         selected.transformed,
		TempoFactor:         selected.tempoFactor,
		TempoFilter:         selected.tempoFilter,
		TempoToolID:         selected.tempoToolID,
		QualityWaiver:       selected.qualityWaiver,
		AcceptedAt:          acceptedAt,
	}
	successor, err := promoteReviewUnit(variant, rev, selected, playbackEnd, reserve, evidence)
	if err != nil {
		return nil, err
	}

	// 8. Publish the successor against the exact base the operator saw. The claim is one
	// statement, so a concurrent acceptance or a superseding synthesis moves the row first and
	// this one is refused instead of overwriting another decision's promotion.
	if err := s.commitReviewedCandidateSuccessor(ctx, successor, variant.CASHash); err != nil {
		return nil, err
	}
	// The audit row is written after the successor exists: the successor is what releases audio,
	// so an audit-only row (or none) can never publish media the operator did not select.
	override := domain.ReviewOverride{
		ID:             overrideID,
		RunID:          runID,
		JobID:          job.ID,
		AssetID:        in.AssetID,
		TargetLanguage: targetLang,
		ReviewItemID:   itemID,
		ItemType:       domain.ReviewItemTypeTTSOverrun,
		Stage:          "dub_synthesize",
		ItemIndex:      rev.Index,
		SegmentID:      rev.SpeakerID,
		Action:         string(domain.ReviewOverrideActionReviewedCandidate),
		Reason:         selectionAuditReason(reason, selected),
		Operator:       operator,
		CreatedAt:      acceptedAt,
	}
	if err := s.db.SaveReviewOverride(ctx, override); err != nil {
		return nil, fmt.Errorf("persist reviewed-candidate acceptance audit: %w", err)
	}
	if err := s.recordCorrectionStage(ctx, runID, "dub_synthesize", successor.CASHash); err != nil {
		return nil, err
	}

	result := &AcceptReviewedCandidateResult{
		AssetID:               in.AssetID,
		RunID:                 runID,
		JobID:                 job.ID,
		TargetLanguage:        targetLang,
		ReviewItemID:          itemID,
		ReviewOverrideID:      overrideID,
		DubSegmentsVariantCAS: successor.CASHash,
		SelectedAudioSHA256:   selected.hash,
		NaturalAudioSHA256:    selected.naturalHash,
		Transformed:           selected.transformed,
		QualityWaiver:         selected.qualityWaiver,
		RemainingReviewCount:  len(successor.ReviewSegments),
	}

	// 9. Incomplete coverage: the run stays paused and no partial mix is produced.
	if successor.OverallStatus != "PASS" {
		result.CoverageComplete = false
		result.RunPaused = true
		result.Status = domain.ReviewItemStatusManualOverride
		result.Message = fmt.Sprintf(
			"accepted %s for fit unit %d; %d review unit(s) remain unresolved, so run %s stays paused and no partial mix was produced",
			selected.hash, rev.Index, len(successor.ReviewSegments), runID)
		return result, nil
	}

	// 10. Complete coverage: rebuild the actual affected descendants from the accepted waveform.
	result.CoverageComplete = true
	if err := s.rebuildSelectionDelivery(ctx, in, job, targetLang, successor, result); err != nil {
		return nil, err
	}
	result.Status = domain.ReviewItemStatusAutoResolved
	if result.Message == "" {
		result.Message = fmt.Sprintf("accepted %s for fit unit %d and rebuilt the run's delivery from the accepted waveform", selected.hash, rev.Index)
	}
	return result, nil
}

// selectionLineage resolves and validates the pinned contract, policy and canonical source
// lineage the acceptance must be proven against. Every mismatch is a refusal: an acceptance may
// never stamp a changed text/voice/contract lineage as previously reviewed.
func (s *ReviewService) selectionLineage(ctx context.Context, runID, assetID, targetLang string, variant *domain.DubSegmentsVariant) (*selectionLineage, error) {
	if variant.SchemaVersion != domain.DubSegmentsSchemaVersion {
		return nil, fmt.Errorf("%w: dubbing artifact uses stale schema %d", ErrReviewedCandidateStale, variant.SchemaVersion)
	}
	if variant.RunID != "" && variant.RunID != runID {
		return nil, fmt.Errorf("%w: dubbing artifact belongs to run %s, not %s", ErrReviewedCandidateNotPending, variant.RunID, runID)
	}

	dubScriptIdx, err := s.db.GetDubScriptVariantIndexByRun(ctx, runID)
	if err != nil || dubScriptIdx == nil || dubScriptIdx.CASHash == "" {
		return nil, fmt.Errorf("%w: run %s pins no dub script artifact: %v", ErrReviewedCandidateNotPending, runID, err)
	}
	if variant.DubScriptVariantCAS != dubScriptIdx.CASHash {
		return nil, fmt.Errorf("%w: dubbing artifact dub script lineage mismatch: artifact=%s run=%s",
			ErrReviewedCandidateStale, variant.DubScriptVariantCAS, dubScriptIdx.CASHash)
	}

	transcriptCAS, err := resolveRunTranscriptCAS(ctx, s.db, runID, assetID, "reviewed-candidate selection")
	if err != nil {
		return nil, fmt.Errorf("resolve run transcript for reviewed-candidate selection: %w", err)
	}
	if transcriptCAS == "" || variant.TranscriptArtifactCAS != transcriptCAS {
		return nil, fmt.Errorf("%w: dubbing artifact transcript lineage mismatch: artifact=%s run=%s",
			ErrReviewedCandidateStale, variant.TranscriptArtifactCAS, transcriptCAS)
	}

	voiceIdx, err := s.db.GetVoiceAssignmentIndexByRunID(ctx, runID)
	if err != nil || voiceIdx == nil || voiceIdx.CASHash == "" {
		return nil, fmt.Errorf("%w: run %s pins no voice assignment: %v", ErrReviewedCandidateNotPending, runID, err)
	}
	if variant.VoiceAssignmentCAS != voiceIdx.CASHash {
		return nil, fmt.Errorf("%w: dubbing artifact voice assignment lineage mismatch: artifact=%s run=%s",
			ErrReviewedCandidateStale, variant.VoiceAssignmentCAS, voiceIdx.CASHash)
	}

	rolePlan, err := ResolveRunScopedAudioRolePlan(ctx, s.db, s.cas, assetID, runID)
	if err != nil {
		return nil, fmt.Errorf("resolve run-scoped audio role plan for reviewed-candidate selection: %w", err)
	}
	if rolePlan == nil || rolePlan.CASHash == "" {
		return nil, fmt.Errorf("%w: run %s pins no audio role plan", ErrReviewedCandidateNotPending, runID)
	}
	if variant.AudioRolePlanCAS != rolePlan.CASHash {
		return nil, fmt.Errorf("%w: dubbing artifact audio role plan lineage mismatch: artifact=%s run=%s",
			ErrReviewedCandidateStale, variant.AudioRolePlanCAS, rolePlan.CASHash)
	}

	fit := NewFitController()
	if variant.FitConfig != nil {
		fit = NewFitController(*variant.FitConfig)
	}
	if variant.FitPolicyID == "" || fit.policyID() != variant.FitPolicyID {
		return nil, fmt.Errorf("%w: dubbing artifact fit policy %q is not the governing policy %q",
			ErrReviewedCandidateStale, variant.FitPolicyID, fit.policyID())
	}

	transcript, err := loadPinnedTranscript(s.cas, variant.TranscriptArtifactCAS, assetID)
	if err != nil {
		// A transcript the acceptance cannot read is a lineage/storage failure of the pinned
		// canonical source, not a candidate-waveform refusal: report the underlying error so the
		// caller classifies it as the missing artifact or storage fault it is.
		return nil, fmt.Errorf("read pinned transcript %s for run %s: %w", variant.TranscriptArtifactCAS, runID, err)
	}
	return &selectionLineage{
		transcript:    transcript,
		rolePlan:      rolePlan,
		eligible:      eligibleSpeechBlocks(transcript, rolePlan),
		fitController: fit,
	}, nil
}

// acceptedPlaybackWindow recomputes the accepted playback window for one review unit from the
// pinned canonical timeline and the governing fit policy, and proves the unit's recorded window
// is still that policy's own answer. A window that the current policy would no longer resolve the
// same way is not a window this acceptance may place audio into.
func (l *selectionLineage) acceptedPlaybackWindow(rev domain.DubSegmentReview, fitPolicyID string) (int64, int64, error) {
	var last domain.SpeechBlock
	hasLast := false
	for _, member := range rev.SpeechBlockIndices {
		block, ok := l.eligible[member]
		if !ok {
			return 0, 0, fmt.Errorf("%w: fit unit %d references non-dub-eligible member %d", ErrReviewedCandidateNotSelectable, rev.Index, member)
		}
		if !hasLast || block.EndMs > last.EndMs {
			last = block
			hasLast = true
		}
	}
	if !hasLast {
		return 0, 0, fmt.Errorf("%w: fit unit %d carries no canonical source membership", ErrReviewedCandidateNotSelectable, rev.Index)
	}
	nextBoundary, err := playbackBoundaryForBlock(last.Index, last.StartMs, last.EndMs, l.transcript, l.rolePlan)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrReviewedCandidateNotSelectable, err)
	}
	playbackEnd, reserve, policyID := l.fitController.ResolvePlaybackWindow(rev.EndMs, nextBoundary)
	if policyID != fitPolicyID || playbackEnd != rev.DubPlaybackEndMs || reserve != rev.EffectiveReserveMs {
		return 0, 0, fmt.Errorf("%w: fit unit %d recorded playback window end=%d reserve=%d, governing policy %s resolves end=%d reserve=%d",
			ErrReviewedCandidateNotSelectable, rev.Index, rev.DubPlaybackEndMs, rev.EffectiveReserveMs, policyID, playbackEnd, reserve)
	}
	if playbackEnd <= rev.StartMs {
		return 0, 0, fmt.Errorf("%w: fit unit %d has no positive accepted playback window", ErrReviewedCandidateNotSelectable, rev.Index)
	}
	return playbackEnd, reserve, nil
}

// resolveSelectionWaveform loads the named candidate's actual committed bytes, re-probes them and
// proves the frame-exact fit the mixer will re-enforce. Advertisement, recorded duration evidence
// and the tempo candidate's own flag are never the proof: the bytes are.
func (s *ReviewService) resolveSelectionWaveform(
	ctx context.Context,
	assetID string,
	rev domain.DubSegmentReview,
	kind ReviewedCandidateKind,
	playbackEndMs int64,
	manualOverride bool,
) (*selectedWaveform, error) {
	if strings.TrimSpace(rev.AudioSHA256) == "" {
		return nil, fmt.Errorf("%w: fit unit %d carries no retained natural waveform", ErrReviewedCandidateUnavailable, rev.Index)
	}
	out := &selectedWaveform{
		hash:         rev.AudioSHA256,
		audioCASPath: rev.AudioCASPath,
		naturalHash:  rev.AudioSHA256,
	}
	if kind == ReviewedCandidateTransformed {
		tc := rev.TempoCandidate
		if tc == nil || strings.TrimSpace(tc.TransformedAudioSHA256) == "" {
			return nil, fmt.Errorf("%w: fit unit %d has no transformed tempo candidate", ErrReviewedCandidateUnavailable, rev.Index)
		}
		if !manualOverride {
			return nil, fmt.Errorf("%w: fit unit %d", ErrReviewedCandidateWaiverRequired, rev.Index)
		}
		if !tc.Selectable {
			return nil, fmt.Errorf("%w: fit unit %d's transformed candidate is recorded as %s",
				ErrReviewedCandidateNotSelectable, rev.Index, tc.Reason)
		}
		out.hash = strings.ToLower(strings.TrimSpace(tc.TransformedAudioSHA256))
		// The transformed artifact is addressed by hash only; no machine-local path is published
		// for it, so playback and this selection both resolve it through CAS.
		out.audioCASPath = ""
		out.transformed = true
		out.qualityWaiver = true
		out.tempoFactor = tc.Factor
		out.tempoFilter = tc.Filter
		out.tempoToolID = tc.ToolID
		if tc.NaturalAudioSHA256 != "" && !strings.EqualFold(tc.NaturalAudioSHA256, rev.AudioSHA256) {
			return nil, fmt.Errorf("%w: tempo candidate for fit unit %d was derived from %s, not the unit's retained waveform",
				ErrReviewedCandidateUnavailable, rev.Index, tc.NaturalAudioSHA256)
		}
	}

	reader, err := s.cas.Get(out.hash)
	if err != nil {
		return nil, fmt.Errorf("%w: read candidate waveform %s: %v", ErrReviewedCandidateUnavailable, out.hash, err)
	}
	audioBytes, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: read candidate waveform %s: %v", ErrReviewedCandidateUnavailable, out.hash, readErr)
	}
	sum := sha256.Sum256(audioBytes)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), out.hash) {
		return nil, fmt.Errorf("%w: candidate waveform %s does not match its committed bytes", ErrReviewedCandidateUnavailable, out.hash)
	}
	samples, header, err := media.ExtractPCM16Samples(audioBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: decode candidate waveform %s: %v", ErrReviewedCandidateUnavailable, out.hash, err)
	}
	if header.NumChannels == 0 || header.SampleRate == 0 || len(samples) == 0 {
		return nil, fmt.Errorf("%w: candidate waveform %s is not valid 16-bit PCM audio", ErrReviewedCandidateUnavailable, out.hash)
	}
	out.measuredMs = int64(header.DurationMs)

	// Prove the fit in the geometry the mixer will place: resample to the output rate first, then
	// compare frame-exactly against the accepted window.
	channels := int(header.NumChannels)
	inRate := int(header.SampleRate)
	outRate := inRate
	if resolved := resolveMixOutputSampleRate(ctx, s.db, s.cas, assetID); resolved > 0 {
		outRate = resolved
	}
	if outRate != inRate {
		samples = media.ResamplePCM16(samples, inRate, channels, outRate, channels)
	}
	out.frames = int64(len(samples) / channels)
	out.windowFrames = media.PlaybackWindowFrames(rev.StartMs, playbackEndMs, outRate)
	if out.frames > out.windowFrames {
		return nil, fmt.Errorf("%w: fit unit %d candidate %s measures %d frames, the accepted playback window holds %d frames (%dms..%dms at %dHz)",
			ErrReviewedCandidateNotSelectable, rev.Index, out.hash, out.frames, out.windowFrames, rev.StartMs, playbackEndMs, outRate)
	}
	return out, nil
}

// promoteReviewUnit builds the successor variant: the accepted unit leaves ReviewSegments, becomes
// a mixable segment backed by the accepted waveform and its matching accepted fit evidence, and
// the acceptance is appended to the variant's own evidence. The original waveform reference, the
// other units, the fit plans, the escalations and every source anchor are preserved untouched.
func promoteReviewUnit(
	variant *domain.DubSegmentsVariant,
	rev domain.DubSegmentReview,
	selected *selectedWaveform,
	playbackEndMs, reserveMs int64,
	evidence domain.AcceptedReviewCandidate,
) (*domain.DubSegmentsVariant, error) {
	if variant == nil {
		return nil, errors.New("dubbing artifact is required to promote a reviewed candidate")
	}
	next := *variant
	next.Segments = slices.Clone(variant.Segments)
	next.ReviewSegments = make([]domain.DubSegmentReview, 0, len(variant.ReviewSegments))
	for _, unit := range variant.ReviewSegments {
		if unit.Index == rev.Index {
			continue
		}
		next.ReviewSegments = append(next.ReviewSegments, unit)
	}
	if len(next.ReviewSegments) == len(variant.ReviewSegments) {
		return nil, fmt.Errorf("%w: fit unit %d is not unresolved in this dubbing artifact", ErrReviewedCandidateNotPending, rev.Index)
	}

	// The promoted unit's own fit evidence becomes an accepted plan, so the mixer's fit/playback
	// contract check resolves the same window and membership the acceptance proved.
	priorPlan := findFitPlan(variant, rev.Index)
	promoted := domain.DubbingFitPlan{
		SegmentIndex:       rev.Index,
		SpeakerID:          rev.SpeakerID,
		SlotDurationMs:     rev.SlotDurationMs,
		MeasuredDurationMs: selected.measuredMs,
		SpeedFactor:        1.0,
		DubPlaybackEndMs:   playbackEndMs,
		EffectiveReserveMs: reserveMs,
		FitPolicyID:        variant.FitPolicyID,
		CalibrationID:      rev.CalibrationID,
		SpeechBlockIndices: slices.Clone(rev.SpeechBlockIndices),
		Decision:           domain.FitActionAccept,
		DecisionReason:     "OPERATOR_ACCEPTED_REVIEWED_CANDIDATE",
	}
	if priorPlan != nil {
		promoted.UsableSlotMs = priorPlan.UsableSlotMs
		promoted.SpeedFactor = priorPlan.SpeedFactor
		promoted.NaturalGapMs = priorPlan.NaturalGapMs
		promoted.AttemptCount = priorPlan.AttemptCount
	}
	if promoted.UsableSlotMs <= 0 {
		promoted.UsableSlotMs = rev.SlotDurationMs
	}
	promoted.DurationDeltaMs = promoted.MeasuredDurationMs - promoted.UsableSlotMs

	next.FitPlans = make([]domain.DubbingFitPlan, 0, len(variant.FitPlans)+1)
	replaced := false
	for _, plan := range variant.FitPlans {
		if plan.SegmentIndex == rev.Index {
			next.FitPlans = append(next.FitPlans, promoted)
			replaced = true
			continue
		}
		next.FitPlans = append(next.FitPlans, plan)
	}
	if !replaced {
		next.FitPlans = append(next.FitPlans, promoted)
	}

	segment := domain.DubSegment{
		Index:              rev.Index,
		SpeechBlockIndices: slices.Clone(rev.SpeechBlockIndices),
		SpeakerID:          rev.SpeakerID,
		StartMs:            rev.StartMs,
		EndMs:              rev.EndMs,
		SlotDurationMs:     rev.SlotDurationMs,
		SourceText:         rev.SourceText,
		SpokenText:         rev.SpokenText,
		AudioCASPath:       selected.audioCASPath,
		AudioSHA256:        selected.hash,
		MeasuredDurationMs: selected.measuredMs,
		Voice:              rev.Voice,
		FitDecision:        domain.FitActionAccept,
		NaturalGapAfterMs:  promoted.NaturalGapMs,
		DubPlaybackEndMs:   playbackEndMs,
		EffectiveReserveMs: reserveMs,
		CalibrationID:      rev.CalibrationID,
	}
	next.Segments = append(next.Segments, segment)
	sort.SliceStable(next.Segments, func(i, j int) bool {
		if next.Segments[i].StartMs == next.Segments[j].StartMs {
			return next.Segments[i].Index < next.Segments[j].Index
		}
		return next.Segments[i].StartMs < next.Segments[j].StartMs
	})

	next.AcceptedCandidates = append(slices.Clone(variant.AcceptedCandidates), evidence)
	next.OverallStatus = dubVariantStatus(&next)
	next.CASHash = ""
	return &next, nil
}

// dubVariantStatus recomputes a variant's overall status from its own units: it is PASS only when
// every unit is a selected, accepted segment and no review unit remains. It never invents PASS.
func dubVariantStatus(variant *domain.DubSegmentsVariant) string {
	if variant == nil || len(variant.Segments) == 0 {
		return "REVIEW_REQUIRED"
	}
	if len(variant.ReviewSegments) > 0 {
		return "REVIEW_REQUIRED"
	}
	for _, seg := range variant.Segments {
		if seg.RequiresReview || seg.FitDecision != domain.FitActionAccept {
			return "REVIEW_REQUIRED"
		}
	}
	return "PASS"
}

// findFitPlan returns the fit evidence recorded for one fit unit index, or nil.
func findFitPlan(variant *domain.DubSegmentsVariant, index int) *domain.DubbingFitPlan {
	for i := range variant.FitPlans {
		if variant.FitPlans[i].SegmentIndex == index {
			return &variant.FitPlans[i]
		}
	}
	return nil
}

// findReviewUnit returns the unresolved review unit with one fit unit index, or false.
func findReviewUnit(variant *domain.DubSegmentsVariant, index int) (domain.DubSegmentReview, bool) {
	for _, unit := range variant.ReviewSegments {
		if unit.Index == index {
			return unit, true
		}
	}
	return domain.DubSegmentReview{}, false
}

// coverageCount counts how many units of a variant cover one fit unit index, so a duplicated or
// missing unit is refused before it can be accepted.
func coverageCount(variant *domain.DubSegmentsVariant, index int) int {
	count := 0
	for _, seg := range variant.Segments {
		if seg.Index == index {
			count++
		}
	}
	for _, unit := range variant.ReviewSegments {
		if unit.Index == index {
			count++
		}
	}
	return count
}

// parseDubReviewItemID parses the identity a dubbing review item carries: the artifact CAS hash it
// was projected from and the fit unit index it names.
func parseDubReviewItemID(itemID string) (variantCAS string, index int, ok bool) {
	const prefix = "rev-dubseg-rev-"
	rest, ok := strings.CutPrefix(itemID, prefix)
	if !ok {
		return "", 0, false
	}
	sep := strings.LastIndex(rest, "-")
	if sep <= 0 {
		return "", 0, false
	}
	parsed, err := strconv.Atoi(rest[sep+1:])
	if err != nil || parsed < 0 {
		return "", 0, false
	}
	return rest[:sep], parsed, true
}

// selectionAuditReason records the operator's note together with the exact waveform contract that
// was selected, so the audit row states what was accepted and not merely that something was.
func selectionAuditReason(reason string, selected *selectedWaveform) string {
	if !selected.transformed {
		return reason + " [candidate=natural sha256=" + selected.hash + "]"
	}
	return fmt.Sprintf("%s [candidate=transformed sha256=%s manual_override quality waiver filter=%s factor=%s tool=%s]",
		reason, selected.hash, selected.tempoFilter, strconv.FormatFloat(selected.tempoFactor, 'f', -1, 64), selected.tempoToolID)
}

// commitReviewedCandidateSuccessor publishes the successor variant and moves the run's dubbing
// artifact row to it, but only while the row still names the exact base variant this acceptance
// was derived from.
func (s *ReviewService) commitReviewedCandidateSuccessor(ctx context.Context, successor *domain.DubSegmentsVariant, baseCAS string) error {
	successor.CASHash = ""
	data, err := json.Marshal(successor)
	if err != nil {
		return fmt.Errorf("marshal successor dub segments variant: %w", err)
	}
	obj, err := s.cas.Put(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("store successor dub segments variant in CAS: %w", err)
	}
	successor.CASHash = obj.SHA256
	claimed, err := s.db.ClaimDubSegmentsVariantIndexFromBase(ctx, storage.DubSegmentsVariantIndex{
		ID:             successor.ID,
		AssetID:        successor.AssetID,
		RunID:          successor.RunID,
		JobID:          successor.JobID,
		TargetLanguage: successor.TargetLanguage,
		CASHash:        successor.CASHash,
		ProvenanceHash: successor.ProvenanceHash,
		OverallStatus:  successor.OverallStatus,
		CreatedAt:      successor.CreatedAt,
	}, successor.VoiceAssignmentCAS, baseCAS)
	if err != nil {
		return fmt.Errorf("claim successor dub segments variant: %w", err)
	}
	if !claimed {
		return fmt.Errorf("%w: run %s's dubbing artifact no longer holds %s", ErrReviewedCandidateConflict, successor.RunID, baseCAS)
	}
	return nil
}

// rebuildSelectionDelivery rebuilds the actual affected target descendants of a completed
// acceptance and hands off under the run's own posture.
//
// Ordering is the safety property: the accepted successor is already the run's current dubbing
// artifact, so a failure here leaves the run visibly mid-delivery (a queued delivery stage the
// handoff refuses) instead of serving the superseded media. Retrying resumes from the stage that
// failed through the existing stage records.
func (s *ReviewService) rebuildSelectionDelivery(
	ctx context.Context,
	in AcceptReviewedCandidateInput,
	job *domain.LocalizationJob,
	targetLang string,
	successor *domain.DubSegmentsVariant,
	result *AcceptReviewedCandidateResult,
) error {
	// The accepted successor is already the run's dubbing artifact, so every preview/final the run
	// currently serves was rendered from the delivery it just replaced. They are withdrawn before any
	// of the new delivery exists: a rebuild can fail between its stages, and an artifact rendered from
	// the superseded media must not stay servable as current while the accepted media is only partly
	// published. The CAS blobs are immutable and stay; the index rows and stage pins are what make an
	// artifact the one a reader resolves.
	if err := s.withdrawRunRenderArtifacts(ctx, successor.RunID, targetLang); err != nil {
		return err
	}
	for _, stage := range []string{"render_preview", "render_final", "final_render_handoff"} {
		if err := s.invalidateCorrectionStage(ctx, successor.RunID, stage); err != nil {
			return err
		}
	}

	dubMix, err := s.audioMixSvc.MixAudio(ctx, AudioMixInput{
		RunID:                 successor.RunID,
		JobID:                 job.ID,
		AssetID:               in.AssetID,
		TargetLanguage:        targetLang,
		DubSegmentsCAS:        successor.CASHash,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	})
	if err != nil {
		// No mix was published from the superseded variant: take the stage record back so the
		// run's delivery lineage stays refused until it is actually rebuilt.
		if invErr := s.invalidateCorrectionStage(ctx, successor.RunID, "audio_mix"); invErr != nil {
			return fmt.Errorf("rebuild dub mix from the accepted candidate: %v; record invalidated audio_mix stage: %w", err, invErr)
		}
		return fmt.Errorf("rebuild dub mix from the accepted candidate: %w", err)
	}
	result.DubMixCAS = dubMix.CASHash
	if err := s.recordCorrectionStage(ctx, successor.RunID, "audio_mix", dubMix.CASHash); err != nil {
		return err
	}

	plan, err := s.freezeRunRenderPlan(ctx, successor.RunID, job.ID, in.AssetID, targetLang, dubMix.CASHash)
	if err != nil {
		if invErr := s.invalidateCorrectionStage(ctx, successor.RunID, "render_plan"); invErr != nil {
			return fmt.Errorf("rebuild render plan from the accepted mix: %v; record invalidated render_plan stage: %w", err, invErr)
		}
		return fmt.Errorf("rebuild render plan from the accepted mix: %w", err)
	}
	result.RenderPlanCAS = plan.CASHash
	if err := s.recordCorrectionStage(ctx, successor.RunID, "render_plan", plan.CASHash); err != nil {
		return err
	}

	// An index row is what makes a preview/final the artifact a reader resolves. The superseded ones
	// were withdrawn before this rebuild started, so the preview rendered below is the only one the
	// run can resolve.
	preview, err := s.renderSvc.RenderPreview(ctx, RenderExecutionInput{
		RunID:          successor.RunID,
		JobID:          job.ID,
		AssetID:        in.AssetID,
		TargetLanguage: targetLang,
		PlanProvenance: plan.ProvenanceHash,
	})
	if err != nil {
		return fmt.Errorf("rebuild preview render from the accepted mix: %w", err)
	}
	result.PreviewRenderCAS = preview.CASHash
	if err := s.recordCorrectionStage(ctx, successor.RunID, "render_preview", preview.CASHash); err != nil {
		return err
	}

	// Posture and readiness are the run's own: auto mode starts the final render once the queue
	// reaches zero, review mode exposes the explicit action and renders nothing.
	posture, err := ResolveRunPosture(ctx, s.db, successor.RunID)
	if err != nil {
		return err
	}
	handoff, err := s.EvaluateFinalRenderHandoff(ctx, domain.FinalRenderHandoffInput{
		AssetID:        in.AssetID,
		RunID:          successor.RunID,
		JobID:          job.ID,
		TargetLanguage: targetLang,
		Posture:        posture,
	})
	if err != nil {
		return fmt.Errorf("final render handoff after accepted selection: %w", err)
	}
	result.HandoffAction = handoff.Action
	result.HandoffMessage = handoff.Message
	result.FinalRenderCAS = handoff.FinalRenderCAS
	if err := s.recordHandoffStage(ctx, successor.RunID, posture, handoff); err != nil {
		return err
	}

	if handoff.Action == "auto_render_started" {
		result.RunCompleted = true
		result.Message = "accepted candidate, rebuilt the run's delivery, and completed the run: " + handoff.Message
		return s.completeRunAfterAutoHandoff(ctx, job.ID, successor.RunID)
	}
	// Review posture renders nothing: the delivery is rebuilt and the run stays paused on the
	// operator's explicit final-render start, so the result must not report it as released.
	result.RunPaused = true
	result.Message = fmt.Sprintf("accepted candidate and rebuilt the run's delivery; %s", handoff.Message)
	return nil
}

// ResolveRunPosture resolves a run's frozen review posture from its config snapshot. It is the one
// resolver of that contract: the host's own resolution and the service-side acceptance both call it,
// so a run cannot be treated as one posture by the pipeline and another by an acceptance.
func ResolveRunPosture(ctx context.Context, db *storage.DB, runID string) (domain.ReviewPosture, error) {
	run, err := db.GetRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("failed to get run %s: %w", runID, err)
	}
	if run == nil || strings.TrimSpace(run.ConfigSnapshotJSON) == "" {
		return domain.ReviewPostureAuto, nil
	}
	var cfg struct {
		Posture       domain.ReviewPosture `json:"posture"`
		ReviewPosture domain.ReviewPosture `json:"review_posture"`
	}
	if err := json.Unmarshal([]byte(run.ConfigSnapshotJSON), &cfg); err != nil {
		return "", fmt.Errorf("malformed run config snapshot JSON: %w", err)
	}
	p := cfg.Posture
	if p == "" {
		p = cfg.ReviewPosture
	}
	if p != "" {
		if p != domain.ReviewPostureAuto && p != domain.ReviewPostureReview {
			return "", fmt.Errorf("invalid run posture: %q", p)
		}
		return p, nil
	}
	return domain.ReviewPostureAuto, nil
}

// recordHandoffStage records the handoff outcome the way the run pipeline does: the result is
// committed to CAS and the stage execution names it, so the run's own lineage describes the
// delivery it reached.
func (s *ReviewService) recordHandoffStage(ctx context.Context, runID string, posture domain.ReviewPosture, handoff *domain.FinalRenderHandoffResult) error {
	if handoff == nil {
		return nil
	}
	stageName := "render_final"
	if posture == domain.ReviewPostureReview {
		stageName = "final_render_handoff"
	}
	data, err := json.Marshal(handoff)
	if err != nil {
		return fmt.Errorf("marshal final render handoff result: %w", err)
	}
	obj, err := s.cas.Put(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("persist final render handoff result to CAS: %w", err)
	}
	if obj.SHA256 == "" {
		return errors.New("persisted final render handoff result produced empty CAS hash")
	}
	return s.recordCorrectionStage(ctx, runID, stageName, obj.SHA256)
}

// completeRunAfterAutoHandoff finishes the run and its job after an automatic final render, the
// same terminal transition the run pipeline performs in Auto posture. The job write comes first so
// a failed queue transition rolls it back instead of leaving a completed job behind a live run.
func (s *ReviewService) completeRunAfterAutoHandoff(ctx context.Context, jobID, runID string) error {
	var priorJobStatus string
	if job, err := s.db.GetJob(ctx, jobID); err == nil && job != nil {
		priorJobStatus = job.Status
	}
	if err := s.db.UpdateJobStatus(ctx, jobID, "completed"); err != nil {
		return fmt.Errorf("complete job %s after accepted selection: %w", jobID, err)
	}
	if err := s.db.UpdateQueueStatus(ctx, runID, domain.RunStatusCompleted, domain.RunStatusCompleted); err != nil {
		if priorJobStatus != "" {
			if rollbackErr := s.db.UpdateJobStatus(ctx, jobID, priorJobStatus); rollbackErr != nil {
				return fmt.Errorf("complete run %s after accepted selection: %w (rolling job %s back to %s also failed: %v)",
					runID, err, jobID, priorJobStatus, rollbackErr)
			}
		}
		return fmt.Errorf("complete run %s after accepted selection: %w", runID, err)
	}
	return nil
}

// withdrawRunRenderArtifacts withdraws the run's preview/final index rows while its delivery is
// rebuilt from a newly accepted candidate. Every one of them was rendered from the delivery the
// acceptance replaced, and the rebuild may fail between its stages, so none of them may stay
// servable as current. The CAS blobs are immutable and stay; the index row is what makes an artifact
// the one a reader resolves.
func (s *ReviewService) withdrawRunRenderArtifacts(ctx context.Context, runID, targetLang string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	indices, err := s.db.GetRenderArtifactIndicesByRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("read run %s render artifacts: %w", runID, err)
	}
	for _, idx := range indices {
		if !strings.EqualFold(idx.TargetLanguage, targetLang) {
			continue
		}
		if err := s.db.DeleteRenderArtifactIndex(ctx, idx.ProvenanceHash); err != nil {
			return fmt.Errorf("withdraw superseded render artifact %s: %w", idx.ProvenanceHash, err)
		}
	}
	return nil
}

// freezeRunRenderPlan re-freezes the run's render plan from the localized visual track the run
// itself pinned, pinning the given dub mix. Shared by every correction that rebuilds the run's
// delivery so they resolve the same cues the same way.
func (s *ReviewService) freezeRunRenderPlan(ctx context.Context, runID, jobID, assetID, targetLang, dubMixCAS string) (*domain.RenderPlan, error) {
	var cues []domain.SubtitleCue
	visIdx, err := s.db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("get run localized visual track index: %w", err)
	}
	if visIdx != nil && (visIdx.AssetID != assetID || !strings.EqualFold(visIdx.TargetLanguage, targetLang)) {
		return nil, fmt.Errorf("localized visual track run binding mismatch for run %s", runID)
	}
	if visIdx != nil {
		rc, err := s.cas.Get(visIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load localized visual track from CAS (%s): %w", visIdx.CASHash, err)
		}
		var visTrack domain.LocalizedVisualTrack
		if err := json.NewDecoder(rc).Decode(&visTrack); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode localized visual track (%s): %w", visIdx.CASHash, err)
		}
		rc.Close()
		cues = visTrack.SubtitleCues
	}
	plan, err := s.renderSvc.FreezeRenderPlan(ctx, RenderPlanInput{
		RunID:          runID,
		JobID:          jobID,
		AssetID:        assetID,
		TargetLanguage: targetLang,
		DubMixCAS:      dubMixCAS,
		SubtitleCues:   cues,
	})
	if err != nil {
		return nil, fmt.Errorf("freeze render plan: %w", err)
	}
	return plan, nil
}

// reportReplayedAcceptance answers a repeated identical selection from the evidence the successor
// already carries: no new artifact, no duplicate override, and the run's current delivery state
// as it stands.
func (s *ReviewService) reportReplayedAcceptance(
	ctx context.Context,
	in AcceptReviewedCandidateInput,
	runID, jobID, targetLang string,
	variant *domain.DubSegmentsVariant,
	existing domain.AcceptedReviewCandidate,
) (*AcceptReviewedCandidateResult, error) {
	// A successor can be complete in CAS while a retried request failed before its audit row was
	// written. The audit row is part of the acceptance, so the replay completes it rather than
	// reporting a decision history never recorded.
	if err := s.ensureAcceptanceAudit(ctx, runID, in, targetLang, jobID, existing); err != nil {
		return nil, err
	}
	result := &AcceptReviewedCandidateResult{
		AssetID:               in.AssetID,
		RunID:                 runID,
		JobID:                 jobID,
		TargetLanguage:        targetLang,
		ReviewItemID:          existing.ReviewItemID,
		ReviewOverrideID:      existing.ReviewOverrideID,
		DubSegmentsVariantCAS: variant.CASHash,
		SelectedAudioSHA256:   existing.SelectedAudioSHA256,
		NaturalAudioSHA256:    existing.NaturalAudioSHA256,
		Transformed:           existing.Transformed,
		QualityWaiver:         existing.QualityWaiver,
		RemainingReviewCount:  len(variant.ReviewSegments),
		CoverageComplete:      variant.OverallStatus == "PASS",
		Idempotent:            true,
		Status:                domain.ReviewItemStatusAutoResolved,
	}
	if !result.CoverageComplete {
		result.RunPaused = true
		result.Status = domain.ReviewItemStatusManualOverride
		result.Message = fmt.Sprintf("review item %s was already accepted with waveform %s; %d review unit(s) remain unresolved and no partial mix was produced",
			existing.ReviewItemID, existing.SelectedAudioSHA256, len(variant.ReviewSegments))
		return result, nil
	}

	// Complete coverage means the run's delivery must be the one built from this successor. A
	// rebuild that failed after the successor was published leaves an approved decision with no
	// media, so the retry re-drives the rebuild through the same stages instead of reporting a
	// delivery the run does not actually serve.
	if !s.deliveryBuiltFrom(ctx, runID, variant) {
		job, err := s.db.GetJob(ctx, jobID)
		if err != nil {
			return nil, fmt.Errorf("load job %s to re-drive the replayed acceptance's rebuild: %w", jobID, err)
		}
		if err := s.rebuildSelectionDelivery(ctx, in, job, targetLang, variant, result); err != nil {
			return nil, err
		}
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = fmt.Sprintf("review item %s was already accepted with waveform %s; the interrupted delivery rebuild was re-driven: %s",
			existing.ReviewItemID, existing.SelectedAudioSHA256, result.Message)
		return result, nil
	}

	// The descendants and the handoff are read back from what the run recorded. A stage lookup
	// failure is a storage fault, not "no delivery": reporting an empty artifact would tell the
	// operator media exists when the store could not be read.
	for _, stage := range []struct {
		name string
		into *string
	}{
		{"audio_mix", &result.DubMixCAS},
		{"render_plan", &result.RenderPlanCAS},
		{"render_preview", &result.PreviewRenderCAS},
	} {
		casHash, err := s.db.GetStageArtifactHash(ctx, runID, stage.name)
		if err != nil {
			return nil, fmt.Errorf("read run %s %s stage artifact: %w", runID, stage.name, err)
		}
		*stage.into = casHash
	}
	handoff, err := s.recordedFinalRenderHandoff(ctx, runID)
	if err != nil {
		return nil, err
	}
	if handoff != nil {
		result.FinalRenderCAS = handoff.FinalRenderCAS
		result.HandoffAction = handoff.Action
		result.HandoffMessage = handoff.Message
	}
	result.Message = fmt.Sprintf("review item %s was already accepted with waveform %s; nothing was re-selected or rebuilt",
		existing.ReviewItemID, existing.SelectedAudioSHA256)
	return result, nil
}

// recordedFinalRenderHandoff reads the final-render handoff a run reached from its recorded result.
// The stage names whichever posture froze the decision and the artifact carries the action it
// decided, so a replay reports the decision that was actually made instead of re-deriving one from
// which stages happen to hold artifacts.
func (s *ReviewService) recordedFinalRenderHandoff(ctx context.Context, runID string) (*domain.FinalRenderHandoffResult, error) {
	for _, stage := range []string{"render_final", "final_render_handoff"} {
		casHash, err := s.db.GetStageArtifactHash(ctx, runID, stage)
		if err != nil {
			return nil, fmt.Errorf("read run %s %s stage artifact: %w", runID, stage, err)
		}
		if casHash == "" {
			continue
		}
		rc, err := s.cas.Get(casHash)
		if err != nil {
			return nil, fmt.Errorf("read recorded %s handoff for run %s: %w", stage, runID, err)
		}
		var handoff domain.FinalRenderHandoffResult
		decodeErr := json.NewDecoder(rc).Decode(&handoff)
		rc.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode recorded %s handoff for run %s: %w", stage, runID, decodeErr)
		}
		return &handoff, nil
	}
	return nil, nil
}

// deliveryBuiltFrom reports whether the delivery the run serves was actually produced from this
// variant: the published mix must name it as its source, and the plan and preview the rebuild writes
// must still be present. It is what tells a finished replay apart from one whose rebuild failed, so
// a retry rebuilds instead of reporting media the run never produced.
func (s *ReviewService) deliveryBuiltFrom(ctx context.Context, runID string, variant *domain.DubSegmentsVariant) bool {
	mixIdx, err := s.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixIdx == nil || mixIdx.CASHash == "" {
		return false
	}
	rc, err := s.cas.Get(mixIdx.CASHash)
	if err != nil {
		return false
	}
	defer rc.Close()
	var mix domain.DubMixArtifact
	if err := json.NewDecoder(rc).Decode(&mix); err != nil {
		return false
	}
	if mix.DubSegmentsCAS != variant.CASHash {
		return false
	}
	for _, stage := range []string{"render_plan", "render_preview"} {
		casHash, err := s.db.GetStageArtifactHash(ctx, runID, stage)
		if err != nil || casHash == "" {
			return false
		}
	}
	return true
}

// ensureAcceptanceAudit writes the acceptance audit row when a retried request finds the successor
// already carries the evidence but the row is missing. It is keyed by the recorded override id, so
// a replay never appends a duplicate decision.
func (s *ReviewService) ensureAcceptanceAudit(
	ctx context.Context,
	runID string,
	in AcceptReviewedCandidateInput,
	targetLang, jobID string,
	existing domain.AcceptedReviewCandidate,
) error {
	rows, err := s.db.GetReviewOverridesByRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("read run %s review overrides: %w", runID, err)
	}
	for _, row := range rows {
		if row.ID == existing.ReviewOverrideID {
			return nil
		}
	}
	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "operator"
	}
	row := domain.ReviewOverride{
		ID:             existing.ReviewOverrideID,
		RunID:          runID,
		JobID:          jobID,
		AssetID:        in.AssetID,
		TargetLanguage: targetLang,
		ReviewItemID:   existing.ReviewItemID,
		ItemType:       domain.ReviewItemTypeTTSOverrun,
		Stage:          "dub_synthesize",
		ItemIndex:      existing.ReviewSegmentIndex,
		SegmentID:      existing.SpeakerID,
		Action:         string(domain.ReviewOverrideActionReviewedCandidate),
		Reason:         selectionAuditReason(strings.TrimSpace(in.Reason), selectedWaveformFromEvidence(existing)),
		Operator:       operator,
		CreatedAt:      existing.AcceptedAt,
	}
	if err := s.db.SaveReviewOverride(ctx, row); err != nil {
		return fmt.Errorf("persist reviewed-candidate acceptance audit: %w", err)
	}
	return nil
}

// selectedWaveformFromEvidence reconstructs the audit sentence of a recorded acceptance.
func selectedWaveformFromEvidence(evidence domain.AcceptedReviewCandidate) *selectedWaveform {
	return &selectedWaveform{
		hash:        evidence.SelectedAudioSHA256,
		naturalHash: evidence.NaturalAudioSHA256,
		transformed: evidence.Transformed,
		tempoFactor: evidence.TempoFactor,
		tempoFilter: evidence.TempoFilter,
		tempoToolID: evidence.TempoToolID,
	}
}

// TargetTextCorrectionInput defines parameters for inspector target text editing and targeted rerun.
type TargetTextCorrectionInput struct {
	RunID                 string                  `json:"run_id"`
	JobID                 string                  `json:"job_id,omitempty"`
	AssetID               string                  `json:"asset_id"`
	TargetLanguage        string                  `json:"target_language"`
	SegmentIndex          int                     `json:"segment_index"`
	NewTargetText         string                  `json:"new_target_text"`
	SpokenTextOverride    string                  `json:"spoken_text_override,omitempty"`
	Reason                string                  `json:"reason,omitempty"`
	Operator              string                  `json:"operator,omitempty"`
	ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
}

// TargetTextCorrectionResult returns the artifacts produced by targeted invalidation and rerun.
type TargetTextCorrectionResult struct {
	AssetID               string                  `json:"asset_id"`
	TargetLanguage        string                  `json:"target_language"`
	SegmentIndex          int                     `json:"segment_index"`
	NewTargetText         string                  `json:"new_target_text"`
	TranslationVariantCAS string                  `json:"translation_variant_cas"`
	DubScriptVariantCAS   string                  `json:"dub_script_variant_cas"`
	DubSegmentsVariantCAS string                  `json:"dub_segments_variant_cas,omitempty"`
	DubMixCAS             string                  `json:"dub_mix_cas,omitempty"`
	LocalizedSubtitleCAS  string                  `json:"localized_subtitle_cas,omitempty"`
	RenderPlanCAS         string                  `json:"render_plan_cas,omitempty"`
	Status                domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message               string                  `json:"message"`
}

// VoiceReassignCorrectionInput defines parameters for inspector voice reassignment and targeted rerun.
type VoiceReassignCorrectionInput struct {
	RunID                 string                         `json:"run_id"`
	JobID                 string                         `json:"job_id,omitempty"`
	AssetID               string                         `json:"asset_id"`
	TargetLanguage        string                         `json:"target_language"`
	CustomAssignments     map[string]domain.VoiceProfile `json:"custom_assignments,omitempty"`
	UseSameVoiceForAll    bool                           `json:"use_same_voice_for_all"`
	Reason                string                         `json:"reason,omitempty"`
	Operator              string                         `json:"operator,omitempty"`
	ExecutionProfile      domain.ExecutionProfile        `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                       `json:"authorized_credentials,omitempty"`
}

// VoiceReassignCorrectionResult returns the artifacts produced by targeted voice invalidation and rerun.
type VoiceReassignCorrectionResult struct {
	VoiceAssignmentCAS    string                  `json:"voice_assignment_cas"`
	DubSegmentsVariantCAS string                  `json:"dub_segments_variant_cas,omitempty"`
	DubMixCAS             string                  `json:"dub_mix_cas,omitempty"`
	RenderPlanCAS         string                  `json:"render_plan_cas,omitempty"`
	InvalidatedSpeakers   []string                `json:"invalidated_speakers,omitempty"`
	Status                domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message               string                  `json:"message"`
}

// RegionGeometryCorrectionInput defines parameters for inspector region override and targeted rerun.
type RegionGeometryCorrectionInput struct {
	RunID                 string                        `json:"run_id,omitempty"`
	JobID                 string                        `json:"job_id,omitempty"`
	AssetID               string                        `json:"asset_id"`
	TargetLanguage        string                        `json:"target_language"`
	Overrides             []domain.RegionOverride       `json:"overrides"`
	InpaintingFallbacks   []string                      `json:"inpainting_fallbacks,omitempty"`
	SceneProtectedRegions []domain.SceneProtectedRegion `json:"scene_protected_regions,omitempty"`
	Reason                string                        `json:"reason,omitempty"`
	Operator              string                        `json:"operator,omitempty"`
}

// RegionGeometryCorrectionResult returns the artifacts produced by region override and visual track regeneration.
type RegionGeometryCorrectionResult struct {
	LocalizedVisualTrackCAS string                  `json:"localized_visual_track_cas"`
	LocalizedSubtitleCAS    string                  `json:"localized_subtitle_cas"`
	RenderPlanCAS           string                  `json:"render_plan_cas,omitempty"`
	PreviewRenderCAS        string                  `json:"preview_render_cas,omitempty"`
	Status                  domain.ReviewItemStatus `json:"status"` // "auto_resolved" or "pending"
	Message                 string                  `json:"message"`
}

func (s *ReviewService) recordStageExecution(ctx context.Context, runID, stage, status, casHash string) error {
	if runID == "" {
		return nil
	}
	// A legacy asset-scoped correction runs under a synthetic "corr-run-…" id with no run row, and stage
	// rows are run-bound: there is nothing to attach such a correction's artifacts to.
	if _, err := s.db.GetRun(ctx, runID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("look up run %s for %s stage record: %w", runID, stage, err)
	}
	now := time.Now().UTC()
	se := domain.StageExecution{
		ID:        uuid.NewString(),
		RunID:     runID,
		Stage:     stage,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if status == domain.StageStatusSucceeded {
		se.ArtifactSHA256 = casHash
		se.StartedAt = &now
		se.CompletedAt = &now
	}
	if err := s.db.CreateStageExecution(ctx, se); err != nil {
		return fmt.Errorf("record %s stage for run %s: %w", stage, runID, err)
	}
	return nil
}

// recordCorrectionStage appends a succeeded stage execution for an artifact a correction just produced,
// so a resumed run reuses what the correction built instead of the pre-correction artifact recorded when
// the run first passed through that stage. Nothing else moves the run's stage rows.
func (s *ReviewService) recordCorrectionStage(ctx context.Context, runID, stage, casHash string) error {
	if casHash == "" {
		return nil
	}
	return s.recordStageExecution(ctx, runID, stage, domain.StageStatusSucceeded, casHash)
}

// invalidateCorrectionStage records a queued stage execution with no artifact, invalidating
// any previously succeeded execution of this stage so a resumed run will not reuse stale artifacts.
func (s *ReviewService) invalidateCorrectionStage(ctx context.Context, runID, stage string) error {
	return s.recordStageExecution(ctx, runID, stage, domain.StageStatusQueued, "")
}

// CorrectTargetText updates target text for a segment and triggers targeted rerun of only declared downstream descendants:
// TTS -> DubSegment -> DubMix -> LocalizedSubtitleTrack -> Render.
// Invariant: Source-derived artifacts (source media, audio stems, transcript alignment, text regions) are strictly REUSED.
func (s *ReviewService) CorrectTargetText(ctx context.Context, in TargetTextCorrectionInput) (*TargetTextCorrectionResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if strings.TrimSpace(in.NewTargetText) == "" {
		return nil, errors.New("new_target_text is required")
	}
	if in.SegmentIndex < 0 {
		return nil, errors.New("segment_index must be >= 0")
	}
	if in.TargetLanguage == "" {
		in.TargetLanguage = "vi"
	}
	requestedRunID := strings.TrimSpace(in.RunID)
	if requestedRunID == "" {
		return nil, errors.New("run_id is required for target text correction")
	}

	result := &TargetTextCorrectionResult{
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		SegmentIndex:   in.SegmentIndex,
		NewTargetText:  in.NewTargetText,
		Status:         domain.ReviewItemStatusPending,
	}
	// Fail-closed invariant: target text correction requires all declared downstream rerun services.
	if s.translationSvc == nil {
		return nil, errors.New("translation service is required for target text correction")
	}
	if s.dubbingSvc == nil {
		return nil, errors.New("dubbing service is required for target text correction")
	}
	if s.audioMixSvc == nil {
		return nil, errors.New("audio mix service is required for target text correction")
	}
	if s.visualTextSvc == nil {
		return nil, errors.New("visual text service is required for target text correction")
	}
	if s.renderSvc == nil {
		return nil, errors.New("render service is required for target text correction")
	}

	// 1. Update TranslationVariant with honest QA validation (never hardcoding 1.0/PASS)
	var transIdx *storage.TranslationVariantIndex
	var err error
	transIdx, err = s.db.GetTranslationVariantIndexByRun(ctx, requestedRunID)
	if errors.Is(err, storage.ErrNotFound) {
		// A run that replayed cached stages owns no variant row of its own; its stage
		// execution records the artifact it consumed (see runBoundArtifactCAS).
		casHash, boundErr := s.runBoundArtifactCAS(ctx, requestedRunID, "translation")
		if boundErr != nil {
			return nil, boundErr
		}
		if casHash != "" {
			transIdx = &storage.TranslationVariantIndex{
				AssetID: in.AssetID, RunID: requestedRunID, TargetLanguage: in.TargetLanguage, CASHash: casHash,
			}
			err = nil
		}
	}
	if err == nil && transIdx != nil && (transIdx.AssetID != in.AssetID || !strings.EqualFold(transIdx.TargetLanguage, in.TargetLanguage)) {
		return nil, fmt.Errorf("translation variant run binding mismatch for run %s", requestedRunID)
	}
	if err != nil {
		return nil, fmt.Errorf("load translation variant index: %w", err)
	}
	trc, err := s.cas.Get(transIdx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("load translation variant from CAS (%s): %w", transIdx.CASHash, err)
	}
	var tVar domain.TranslationVariant
	if err := json.NewDecoder(trc).Decode(&tVar); err != nil {
		trc.Close()
		return nil, fmt.Errorf("decode translation variant (%s): %w", transIdx.CASHash, err)
	}
	trc.Close()
	if err := s.validateTranslationCompatibility(ctx, requestedRunID, in, &tVar, transIdx); err != nil {
		return nil, err
	}
	// A segment's own index is the source speech-block index, not its position in the slice: a clip
	// with silent stretches indexes 0,2,4,6, so indexing the slice with it rejected every correction
	// past the first (live evidence, run eec68c8d: "segment index 6 out of bounds (total 4)").
	transPos := slices.IndexFunc(tVar.Segments, func(s domain.TranslationSegment) bool { return s.Index == in.SegmentIndex })
	if transPos < 0 {
		return nil, fmt.Errorf("segment index %d not present in the translation variant (%d segments)", in.SegmentIndex, len(tVar.Segments))
	}

	qaGate := s.translationSvc.qaGate
	if qaGate == nil {
		qaGate = NewMeaningFirstQAGate()
	}

	applicableGlossary := glossaryForSource(tVar.EffectiveGlossary, tVar.Segments[transPos].SourceText)
	qaRes := qaGate.ValidateSegment(tVar.Segments[transPos].SourceText, in.NewTargetText, tVar.SourceLanguage, in.TargetLanguage, applicableGlossary)

	tVar.ID = uuid.NewString()
	tVar.RunID = in.RunID
	if in.JobID != "" {
		tVar.JobID = in.JobID
	}
	tVar.Segments[transPos].TargetText = in.NewTargetText
	tVar.Segments[transPos].PassedQAGate = qaRes.Passed
	tVar.Segments[transPos].QAConfidence = qaRes.Confidence
	tVar.Segments[transPos].ReviewReason = qaReviewReason(in.SegmentIndex, qaRes)
	tVar.Segments[transPos].KeyFacts = qaRes.ExtractedFacts
	tVar.Segments[transPos].NegationPolarity = qaRes.NegationPolarity
	tVar.CreatedAt = time.Now().UTC()

	var totalConf float64
	for _, seg := range tVar.Segments {
		totalConf += seg.QAConfidence
	}
	if len(tVar.Segments) > 0 {
		tVar.OverallQAScore = totalConf / float64(len(tVar.Segments))
	}

	// Compute updated provenance hash chaining from original provenance
	baseProvenance := tVar.ProvenanceHash
	if baseProvenance == "" && transIdx != nil {
		baseProvenance = transIdx.ProvenanceHash
	}
	if baseProvenance == "" {
		return nil, fmt.Errorf("translation variant %s is missing provenance hash", transIdx.CASHash)
	}
	hTrans := sha256.New()
	_, _ = hTrans.Write([]byte(fmt.Sprintf("%s:%s:%d:%s", baseProvenance, in.TargetLanguage, in.SegmentIndex, in.NewTargetText)))
	tVar.ProvenanceHash = hex.EncodeToString(hTrans.Sum(nil))

	tBytes, err := json.MarshalIndent(tVar, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal updated translation variant: %w", err)
	}
	tObj, err := s.cas.Put(bytes.NewReader(tBytes))
	if err != nil {
		return nil, fmt.Errorf("store updated translation variant in CAS: %w", err)
	}
	tVar.CASHash = tObj.SHA256
	result.TranslationVariantCAS = tObj.SHA256

	if err := s.db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             tVar.ID,
		AssetID:        in.AssetID,
		RunID:          in.RunID,
		JobID:          in.JobID,
		TargetLanguage: in.TargetLanguage,
		CASHash:        tObj.SHA256,
		ProvenanceHash: tVar.ProvenanceHash,
		ProviderID:     tVar.ProviderID,
		ModelName:      tVar.ModelName,
		ModelVersion:   tVar.ModelVersion,
		OverallQAScore: tVar.OverallQAScore,
		CreatedAt:      tVar.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("save translation variant index: %w", err)
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "translation", tObj.SHA256); err != nil {
		return nil, err
	}

	// 2. Update DubScriptVariant (spoken adaptation rerun)
	dubIn := domain.DubScriptJobInput{
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		SourceLanguage:        tVar.SourceLanguage,
		TargetLanguage:        in.TargetLanguage,
		TranslationVariantCAS: tObj.SHA256,
	}
	dsVar, err := s.translationSvc.AdaptDubScript(ctx, dubIn)
	if err != nil {
		return nil, fmt.Errorf("adapt spoken script rerun failed: %w", err)
	}
	if dsVar == nil {
		return nil, errors.New("adapt spoken script produced nil dub script variant")
	}
	if in.SpokenTextOverride != "" {
		if pos := slices.IndexFunc(dsVar.Segments, func(s domain.DubScriptSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
			seg := &dsVar.Segments[pos]
			seg.SpokenText = in.SpokenTextOverride
			spQa := qaGate.ValidateSegment(seg.SourceText, in.SpokenTextOverride, tVar.SourceLanguage, in.TargetLanguage, glossaryForSource(tVar.EffectiveGlossary, seg.SourceText))
			seg.PassedQAGate = spQa.Passed
			seg.QAConfidence = spQa.Confidence
			seg.KeyFacts = spQa.ExtractedFacts
			seg.NegationPolarity = spQa.NegationPolarity
			estMs := provider.EstimateSpokenDurationMs(in.SpokenTextOverride, in.TargetLanguage)
			seg.EstimatedDurationMs = estMs
			seg.RequiresReview = false
			seg.ReviewReason = ""
			if !spQa.Passed {
				seg.RequiresReview = true
				seg.ReviewReason = domain.ReviewReasonMeaningCorrupted
			}
			if seg.SlotDurationMs > 0 && estMs > seg.SlotDurationMs {
				seg.RequiresReview = true
				if seg.ReviewReason == "" {
					seg.ReviewReason = "DURATION_OVERRUN"
				}
			}
			dsBytes, err := json.MarshalIndent(dsVar, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshal overridden dub script: %w", err)
			}
			dsObj, err := s.cas.Put(bytes.NewReader(dsBytes))
			if err != nil {
				return nil, fmt.Errorf("store overridden dub script in CAS: %w", err)
			}
			dsVar.CASHash = dsObj.SHA256
			if err := s.db.SaveDubScriptVariantIndex(ctx, storage.DubScriptVariantIndex{
				ID:             dsVar.ID,
				AssetID:        in.AssetID,
				RunID:          in.RunID,
				JobID:          in.JobID,
				TargetLanguage: in.TargetLanguage,
				CASHash:        dsObj.SHA256,
				ProvenanceHash: dsVar.ProvenanceHash,
				ProviderID:     dsVar.ProviderID,
				ModelName:      dsVar.ModelName,
				ModelVersion:   dsVar.ModelVersion,
				OverallQAScore: dsVar.OverallQAScore,
				CreatedAt:      dsVar.CreatedAt,
			}); err != nil {
				return nil, fmt.Errorf("save overridden dub script index: %w", err)
			}
		}
	}
	result.DubScriptVariantCAS = dsVar.CASHash
	if err := s.recordCorrectionStage(ctx, in.RunID, "dub_script", dsVar.CASHash); err != nil {
		return nil, err
	}

	// 3. Re-freeze the chosen voices against the corrected DubScript/Transcript lineage,
	// then rerun TTS & DubSegments synthesis. A corrected script must not keep pointing
	// at a VoiceAssignment frozen for the pre-correction script: the mixer validates the
	// exact pinned lineage and will (correctly) refuse that stale combination.
	voiceAssignCAS := ""
	var vaIdx *storage.VoiceAssignmentIndex
	vaIdx, err = s.db.GetVoiceAssignmentIndexByRun(ctx, in.AssetID, requestedRunID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("load voice assignment index: %w", err)
	}

	voiceInput := domain.VoiceAssignmentInput{
		RunID:                 in.RunID,
		AssetID:               in.AssetID,
		JobID:                 in.JobID,
		TargetLanguage:        in.TargetLanguage,
		DubScriptVariantCAS:   result.DubScriptVariantCAS,
		TranscriptArtifactCAS: tVar.TranscriptArtifactCAS,
		ExecutionProfile:      in.ExecutionProfile,
	}
	var priorAssignment *domain.VoiceAssignment
	if vaIdx != nil && strings.TrimSpace(vaIdx.CASHash) != "" {
		vaRC, loadErr := s.cas.Get(vaIdx.CASHash)
		if loadErr != nil {
			return nil, fmt.Errorf("load frozen voice assignment from CAS (%s): %w", vaIdx.CASHash, loadErr)
		}
		var loaded domain.VoiceAssignment
		decodeErr := json.NewDecoder(vaRC).Decode(&loaded)
		vaRC.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode frozen voice assignment (%s): %w", vaIdx.CASHash, decodeErr)
		}
		if loaded.AssetID != "" && loaded.AssetID != in.AssetID {
			return nil, fmt.Errorf("voice assignment artifact %s belongs to asset %s, expected %s", vaIdx.CASHash, loaded.AssetID, in.AssetID)
		}
		if loaded.TargetLanguage != "" && !strings.EqualFold(loaded.TargetLanguage, in.TargetLanguage) {
			return nil, fmt.Errorf("voice assignment artifact %s uses language %s, expected %s", vaIdx.CASHash, loaded.TargetLanguage, in.TargetLanguage)
		}
		loaded.CASHash = vaIdx.CASHash
		priorAssignment = &loaded
		voiceInput.CustomAssignments = loaded.Assignments
		voiceInput.UseSameVoiceForAll = loaded.UseSameVoiceForAll
	}

	var frozenAssignment *domain.VoiceAssignment
	if priorAssignment != nil && priorAssignment.RunID == in.RunID {
		frozenAssignment, err = s.dubbingSvc.ReassignVoice(ctx, voiceInput)
	} else {
		frozenAssignment, err = s.dubbingSvc.AssignVoices(ctx, voiceInput)
	}
	if err != nil {
		return nil, fmt.Errorf("freeze corrected voice assignment lineage: %w", err)
	}
	if frozenAssignment == nil || strings.TrimSpace(frozenAssignment.CASHash) == "" {
		return nil, errors.New("freeze corrected voice assignment lineage produced no CAS artifact")
	}
	voiceAssignCAS = frozenAssignment.CASHash

	dubbingJobIn := domain.DubbingJobInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubScriptVariantCAS:   result.DubScriptVariantCAS,
		VoiceAssignmentCAS:    voiceAssignCAS,
		TranscriptArtifactCAS: tVar.TranscriptArtifactCAS,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubSegsVar, err := s.dubbingSvc.SynthesizeAndFit(ctx, dubbingJobIn)
	if err != nil {
		return nil, fmt.Errorf("synthesize and fit rerun failed: %w", err)
	}
	if dubSegsVar == nil {
		return nil, errors.New("synthesize and fit produced nil dub segments variant")
	}
	result.DubSegmentsVariantCAS = dubSegsVar.CASHash
	// The corrected variant replaces this run's dub_synthesize output: record it, or a resumed run
	// replays the pre-correction segments it started from and the mixer refuses them again (live
	// evidence: run 27a758e6 resumed from `segment 0 measured 13280ms exceeds slot 12400ms` after all
	// three segments had been shortened to fit).
	if err := s.recordCorrectionStage(ctx, in.RunID, "dub_synthesize", dubSegsVar.CASHash); err != nil {
		return nil, err
	}

	// 4. Rerun Audio Mix (dialogue suppression + soundtrack preservation)
	mixIn := AudioMixInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubSegmentsCAS:        result.DubSegmentsVariantCAS,
		PreserveSinging:       true,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubMix, err := s.audioMixSvc.MixAudio(ctx, mixIn)
	if err != nil {
		return nil, fmt.Errorf("audio mix rerun failed: %w", err)
	}
	if dubMix == nil {
		return nil, errors.New("audio mix produced nil dub mix artifact")
	}
	result.DubMixCAS = dubMix.CASHash
	if err := s.recordCorrectionStage(ctx, in.RunID, "audio_mix", dubMix.CASHash); err != nil {
		return nil, err
	}

	// 5. Rerun Visual Text Localized Subtitle / Visual Track, but only once a text region plan exists.
	// A run reaches review before its visual stage ran whenever the mixer refuses a dub that cannot fit
	// its immutable slots: text_detection and visual_text_localize run AFTER audio_mix, so the plan this
	// rerun resolves does not exist yet (live evidence: run 27a758e6, whose three tts_overrun corrections
	// all failed with `text region plan not found: record not found`, leaving the operator no way to
	// shorten the text that the refusal was asking about). The resumed pipeline localizes and freezes the
	// render plan after the mix passes, exactly as a first pass would.
	if _, planErr := s.db.GetTextRegionPlanIndex(ctx, in.AssetID); planErr == nil {
		visIn := LocalizeVisualTrackInput{
			RunID:                 in.RunID,
			JobID:                 in.JobID,
			AssetID:               in.AssetID,
			TargetLanguage:        in.TargetLanguage,
			TranslationVariantCAS: tObj.SHA256,
		}
		visTrack, err := s.visualTextSvc.LocalizeVisualTrack(ctx, visIn)
		if err != nil {
			return nil, fmt.Errorf("localize visual track rerun failed: %w", err)
		}
		if visTrack == nil {
			return nil, errors.New("localize visual track produced nil visual track")
		}
		// Invariant: result.LocalizedSubtitleCAS is the actual LocalizedSubtitleTrack CAS.
		result.LocalizedSubtitleCAS = visTrack.SubtitleTrackCAS
		if err := s.recordCorrectionStage(ctx, in.RunID, "visual_text_localize", visTrack.CASHash); err != nil {
			return nil, err
		}

		// 6. Refreeze RenderPlan: pins explicit new DubMixCAS and newly produced subtitle cues/render inputs
		planIn := RenderPlanInput{
			RunID:          in.RunID,
			JobID:          in.JobID,
			AssetID:        in.AssetID,
			TargetLanguage: in.TargetLanguage,
			DubMixCAS:      result.DubMixCAS,
			SubtitleCues:   visTrack.SubtitleCues,
		}
		rPlan, err := s.renderSvc.FreezeRenderPlan(ctx, planIn)
		if err != nil {
			return nil, fmt.Errorf("freeze render plan rerun failed: %w", err)
		}
		if rPlan == nil {
			return nil, errors.New("freeze render plan produced nil render plan")
		}
		result.RenderPlanCAS = rPlan.CASHash
		if err := s.recordCorrectionStage(ctx, in.RunID, "render_plan", rPlan.CASHash); err != nil {
			return nil, err
		}
		if err := s.invalidateCorrectionStage(ctx, in.RunID, "render_preview"); err != nil {
			return nil, err
		}
	} else if !errors.Is(planErr, storage.ErrNotFound) {
		return nil, fmt.Errorf("load text region plan index: %w", planErr)
	}

	// 7. Check if candidate auto-resolved based on honest evidence across all evaluated stages
	isResolved := true

	// Meaning QA must pass
	if pos := slices.IndexFunc(tVar.Segments, func(s domain.TranslationSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
		if tSeg := tVar.Segments[pos]; !tSeg.PassedQAGate || tSeg.QAConfidence < 0.6 {
			isResolved = false
		}
	}

	// Spoken adaptation QA and timing must pass
	if pos := slices.IndexFunc(dsVar.Segments, func(s domain.DubScriptSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
		if dsSeg := dsVar.Segments[pos]; !dsSeg.PassedQAGate || dsSeg.RequiresReview {
			isResolved = false
		}
	}

	// DubSegments candidate must satisfy the accepted playback-window contract.
	if pos := slices.IndexFunc(dubSegsVar.Segments, func(s domain.DubSegment) bool { return s.Index == in.SegmentIndex }); pos >= 0 {
		seg := dubSegsVar.Segments[pos]
		playbackDurationMs := seg.DubPlaybackEndMs - seg.StartMs
		if seg.RequiresReview || seg.FitDecision != domain.FitActionAccept || playbackDurationMs <= 0 || seg.MeasuredDurationMs > playbackDurationMs {
			isResolved = false
		}
	}
	for _, rev := range dubSegsVar.ReviewSegments {
		if rev.Index == in.SegmentIndex {
			isResolved = false
			break
		}
	}

	if result.DubMixCAS == "" || result.LocalizedSubtitleCAS == "" || result.RenderPlanCAS == "" {
		isResolved = false
	}

	if isResolved {
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = "Target text corrected and targeted rerun produced passing candidate (auto-resolved)"
	} else {
		result.Status = domain.ReviewItemStatusPending
		result.Message = "Target text corrected but candidate still requires review"
	}

	return result, nil
}

// ReassignVoice explicitly changes one or more speakers' frozen voices for a run,
// regenerating exactly that speaker's affected downstream scope (TTS -> DubSegment -> DubMix -> RenderPlan)
// while strictly preserving source-derived extractions (source media, audio stems, transcript alignment, text regions).
// Voice reassignment is strictly run-bound so a correction can never adopt another
// run's latest script, transcript, or downstream lineage.
func (s *ReviewService) ReassignVoice(ctx context.Context, in VoiceReassignCorrectionInput) (*VoiceReassignCorrectionResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	pinnedRunID := strings.TrimSpace(in.RunID)
	if pinnedRunID == "" {
		return nil, errors.New("run_id is required for voice reassignment")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	if s.dubbingSvc == nil {
		return nil, errors.New("dubbing service is required for voice reassign rerun")
	}
	if s.audioMixSvc == nil {
		return nil, errors.New("audio mix service is required for voice reassign rerun")
	}
	if s.renderSvc == nil {
		return nil, errors.New("render service is required for voice reassign rerun")
	}

	// 1. Resolve the exact run-bound dub script variant the reassignment regenerates
	// against. Asset-latest fallback is deliberately prohibited: a voice correction
	// must never adopt another run's script merely because the caller omitted run_id.
	var dubScriptIdx *storage.DubScriptVariantIndex
	var err error
	dubScriptIdx, err = s.db.GetDubScriptVariantIndexByRun(ctx, pinnedRunID)
	if err != nil {
		return nil, fmt.Errorf("load run dub script variant: %w", err)
	}
	if dubScriptIdx == nil || dubScriptIdx.AssetID != in.AssetID || !strings.EqualFold(dubScriptIdx.TargetLanguage, in.TargetLanguage) {
		return nil, fmt.Errorf("dub script variant run binding mismatch for run %s", pinnedRunID)
	}

	// 2. Reassign voice profile(s) for the run against the exact current script/transcript lineage.
	// Historical assignments may predate these pins; a successor used by the current mixer must not.
	// The asset-latest fallback below applies only when the run genuinely pins no transcript: a run whose
	// pinned-transcript proof failed must not silently adopt another run's transcript.
	transcriptCAS, err := resolveRunTranscriptCAS(ctx, s.db, in.RunID, in.AssetID, "voice reassignment")
	if err != nil {
		return nil, fmt.Errorf("resolve run transcript lineage for voice reassignment: %w", err)
	}
	if strings.TrimSpace(transcriptCAS) == "" {
		dubScript, _, loadErr := s.dubbingSvc.loadDubScriptVariant(ctx, in.AssetID, pinnedRunID, targetLang, dubScriptIdx.CASHash)
		if loadErr != nil {
			return nil, fmt.Errorf("load exact dub script lineage for voice reassignment: %w", loadErr)
		}
		if strings.TrimSpace(dubScript.TranslationVariantCAS) != "" {
			transcriptCAS, err = s.dubbingSvc.transcriptCASFromTranslationVariant(in.AssetID, targetLang, dubScript.TranslationVariantCAS)
			if err != nil {
				return nil, err
			}
		}
	}
	if strings.TrimSpace(transcriptCAS) == "" {
		return nil, fmt.Errorf("pinned transcript artifact is required for voice reassignment")
	}
	assignIn := domain.VoiceAssignmentInput{
		RunID:                 in.RunID,
		AssetID:               in.AssetID,
		JobID:                 in.JobID,
		TargetLanguage:        in.TargetLanguage,
		CustomAssignments:     in.CustomAssignments,
		UseSameVoiceForAll:    in.UseSameVoiceForAll,
		ExecutionProfile:      in.ExecutionProfile,
		DubScriptVariantCAS:   dubScriptIdx.CASHash,
		TranscriptArtifactCAS: transcriptCAS,
	}
	newAssign, err := s.dubbingSvc.ReassignVoice(ctx, assignIn)
	if err != nil {
		return nil, fmt.Errorf("voice reassignment failed: %w", err)
	}

	result := &VoiceReassignCorrectionResult{
		VoiceAssignmentCAS:  newAssign.CASHash,
		InvalidatedSpeakers: newAssign.InvalidatedSpeakers,
	}

	// 3. Synthesize dub segments (reuses unchanged speakers, regenerates changed speakers)
	synthIn := domain.DubbingJobInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		VoiceAssignmentCAS:    newAssign.CASHash,
		DubScriptVariantCAS:   dubScriptIdx.CASHash,
		TranscriptArtifactCAS: transcriptCAS,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubSegsVar, err := s.dubbingSvc.SynthesizeAndFit(ctx, synthIn)
	if err != nil {
		return nil, fmt.Errorf("dub synthesize rerun failed: %w", err)
	}
	// SynthesizeAndFit may escalate a whole speaker to the duration-controlled lane
	// (Issue #94), which pins the variant to a superseding VoiceAssignment. The
	// correction must report the assignment actually in force, including which speakers
	// that final assignment invalidated, not the one the correction asked for.
	if finalCAS := dubSegsVar.VoiceAssignmentCAS; finalCAS != "" && finalCAS != newAssign.CASHash {
		rc, err := s.cas.Get(finalCAS)
		if err != nil {
			return nil, fmt.Errorf("load final voice assignment from CAS (%s): %w", finalCAS, err)
		}
		var final domain.VoiceAssignment
		if err := json.NewDecoder(rc).Decode(&final); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode final voice assignment (%s): %w", finalCAS, err)
		}
		rc.Close()
		result.VoiceAssignmentCAS = finalCAS
		result.InvalidatedSpeakers = final.InvalidatedSpeakers
	}
	result.DubSegmentsVariantCAS = dubSegsVar.CASHash

	// 4. Audio stem mixing
	mixIn := AudioMixInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		DubSegmentsCAS:        dubSegsVar.CASHash,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
	}
	dubMix, err := s.audioMixSvc.MixAudio(ctx, mixIn)
	if err != nil {
		return nil, fmt.Errorf("audio mix rerun failed: %w", err)
	}
	result.DubMixCAS = dubMix.CASHash

	// 5. Refreeze RenderPlan from the run's own pinned visual track. in.RunID is guaranteed
	// populated by step 1 (the legacy route adopts the resolved variant's run), so the visual
	// track must always resolve run-bound: an asset-scoped lookup here would let a newer
	// track from another run bleed its cues into the regenerated plan.
	rPlan, err := s.freezeRunRenderPlan(ctx, in.RunID, in.JobID, in.AssetID, in.TargetLanguage, dubMix.CASHash)
	if err != nil {
		return nil, fmt.Errorf("freeze render plan rerun failed: %w", err)
	}
	result.RenderPlanCAS = rPlan.CASHash
	if err := s.recordCorrectionStage(ctx, in.RunID, "voice_assignment", result.VoiceAssignmentCAS); err != nil {
		return nil, err
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "dub_synthesize", result.DubSegmentsVariantCAS); err != nil {
		return nil, err
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "audio_mix", result.DubMixCAS); err != nil {
		return nil, err
	}
	if err := s.recordCorrectionStage(ctx, in.RunID, "render_plan", result.RenderPlanCAS); err != nil {
		return nil, err
	}
	if err := s.invalidateCorrectionStage(ctx, in.RunID, "render_preview"); err != nil {
		return nil, err
	}

	// 6. Evaluate auto-resolution status
	isResolved := dubSegsVar.OverallStatus == "PASS" && len(dubSegsVar.ReviewSegments) == 0 &&
		result.DubMixCAS != "" && result.RenderPlanCAS != ""
	if isResolved {
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = "Voice reassigned and targeted rerun produced passing candidate (auto-resolved)"
	} else {
		result.Status = domain.ReviewItemStatusPending
		result.Message = "Voice reassigned but candidate still requires review"
	}

	return result, nil
}

// regionCorrectionRollbackTimeout bounds the withdrawal of a rejected correction.
// The rollback runs detached from the request context, so it needs a deadline of
// its own: without one, a healthy database that is slow to answer could hold the
// request goroutine (and its transaction) open indefinitely.
const regionCorrectionRollbackTimeout = 15 * time.Second

// CorrectRegionGeometry applies direct-manipulation overrides (reclassify, drag, resize, relabel)
// to text regions and regenerates the visual track and render plan descendants.
func (s *ReviewService) CorrectRegionGeometry(ctx context.Context, in RegionGeometryCorrectionInput) (result *RegionGeometryCorrectionResult, err error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if len(in.Overrides) == 0 {
		return nil, errors.New("at least one region override is required")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	if s.visualTextSvc == nil {
		return nil, errors.New("visual text service is required for region correction rerun")
	}
	if s.renderSvc == nil {
		return nil, errors.New("render service is required for region correction rerun")
	}

	// Snapshot every descendant artifact this correction is able to regenerate BEFORE any state is
	// persisted: the rollback can only withdraw the rows this correction makes current itself if it
	// knows what was current beforehand, and a snapshot failure must therefore abort while the asset
	// is still untouched.
	priorDescendants, err := s.currentDescendantArtifacts(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("snapshot current region correction descendants: %w", err)
	}

	// 1. Update and persist TextRegionPlan in CAS and SQLite
	planIdx, err := s.db.GetTextRegionPlanIndex(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("get text region plan index: %w", err)
	}
	if planIdx == nil {
		return nil, errors.New("text region plan index is nil")
	}
	rc, err := s.cas.Get(planIdx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("load text region plan from CAS: %w", err)
	}
	var origPlan domain.TextRegionPlan
	err = json.NewDecoder(rc).Decode(&origPlan)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("decode text region plan: %w", err)
	}
	origPlan.CASHash = planIdx.CASHash
	origPlan.ProvenanceHash = planIdx.ProvenanceHash

	// Fail closed on geometry the operator asked for but the frame cannot hold. ApplyRegionOverrides
	// clamps (a clamped box is always in-frame and valid, so a clamp cannot be detected afterwards),
	// which is right for the lenient callers it also serves but wrong here: a correction that
	// silently moved the box somewhere else must be an actionable error, not a persisted surprise.
	if err := domain.ValidateRegionOverrideGeometry(&origPlan, in.Overrides); err != nil {
		return nil, err
	}

	updatedPlan, err := ApplyRegionOverrides(&origPlan, in.Overrides)
	if err != nil {
		return nil, fmt.Errorf("apply region overrides: %w", err)
	}
	if updatedPlan == nil {
		return nil, errors.New("updated text region plan is nil")
	}

	// Mint a NEW immutable overridden-plan identity. The overridden plan must NOT reuse the
	// canonical source-derived provenance: SaveTextRegionPlanIndex is UNIQUE on provenance_hash
	// and ON CONFLICT updates cas_hash, so reusing the source provenance would overwrite the
	// source plan's index row. Derive deterministic provenance from stable semantic inputs only
	// (parent/source provenance plus canonical overridden region state) so identical overrides
	// are idempotent, and persist as a distinct row preserving the source plan under its own
	// provenance. Never use timestamps, paths, or run IDs as semantic identity.
	overrideProv, err := domain.ComputeTextRegionPlanOverrideProvenanceHash(updatedPlan.ProvenanceHash, updatedPlan.Regions)
	if err != nil {
		return nil, fmt.Errorf("compute overridden text region plan provenance: %w", err)
	}
	updatedPlan.ID = uuid.NewString()
	updatedPlan.ProvenanceHash = overrideProv
	updatedPlan.CreatedAt = time.Now().UTC()

	data, err := json.Marshal(updatedPlan)
	if err != nil {
		return nil, fmt.Errorf("marshal updated text region plan: %w", err)
	}
	casObj, err := s.cas.Put(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("put updated text region plan to CAS: %w", err)
	}
	updatedPlan.CASHash = casObj.SHA256
	if err := s.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             updatedPlan.ID,
		AssetID:        updatedPlan.AssetID,
		ProviderID:     updatedPlan.ProviderID,
		ModelName:      updatedPlan.ModelName,
		ModelVersion:   updatedPlan.ModelVersion,
		CASHash:        casObj.SHA256,
		ProvenanceHash: updatedPlan.ProvenanceHash,
		CreatedAt:      updatedPlan.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("save updated text region plan index: %w", err)
	}

	// Fail-closed: the overridden plan becomes the asset's current plan only once its declared
	// descendants regenerate and the operator's audit row is recorded. Persisting it first is
	// required (LocalizeVisualTrack resolves the current plan by index) but a rejected edit must
	// not stay silently accepted as the current plan, so any failure after this point withdraws
	// every row this correction made current — the plan and the visual/subtitle/render artifacts
	// its regeneration persisted — leaving the pre-correction state current again.
	defer func() {
		if err == nil {
			return
		}
		// The withdrawal must not depend on the request that failed: a client that
		// disconnects (or a cancelled job) between persistence and the audit row is
		// exactly when the rejected state is most likely to be left current, so the
		// rollback runs on a context detached from that cancellation and bounded by
		// its own deadline instead of running on a context that is already done.
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), regionCorrectionRollbackTimeout)
		defer cancelRollback()
		if updatedPlan.ProvenanceHash != planIdx.ProvenanceHash {
			if derr := s.db.DeleteTextRegionPlanIndex(rollbackCtx, in.AssetID, updatedPlan.ProvenanceHash); derr != nil {
				err = fmt.Errorf("%w (and the rejected plan could not be withdrawn: %v)", err, derr)
			}
		}
		if derr := s.withdrawCorrectionDescendants(rollbackCtx, in, priorDescendants); derr != nil {
			err = fmt.Errorf("%w (and the rejected descendants could not be withdrawn: %v)", err, derr)
		}
	}()

	// 2. Localize visual track with direct manipulation overrides
	visIn := LocalizeVisualTrackInput{
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		AssetID:               in.AssetID,
		TargetLanguage:        in.TargetLanguage,
		InpaintingFallbacks:   in.InpaintingFallbacks,
		SceneProtectedRegions: in.SceneProtectedRegions,
		// Only the regions the operator just changed are validated strictly: a collision on one of
		// them is a rejected edit (and the plan is withdrawn below), while a collision on an
		// untouched region stays a visual_occlusion exception instead of blocking the correction.
		StrictOverlapRegionIDs: overrideRegionIDs(in.Overrides),
	}
	visTrack, err := s.visualTextSvc.LocalizeVisualTrack(ctx, visIn)
	if err != nil {
		return nil, fmt.Errorf("localize visual track with overrides failed: %w", err)
	}

	result = &RegionGeometryCorrectionResult{
		LocalizedVisualTrackCAS: visTrack.CASHash,
		LocalizedSubtitleCAS:    visTrack.SubtitleTrackCAS,
	}

	// 2. Refreeze RenderPlan
	// A valid no-speech/no-dub run still executes AudioMixService and persists a PASS passthrough
	// DubMixArtifact. Therefore a missing DubMix index during region correction is an
	// incomplete/invalid review state and must fail closed explicitly, not be treated as a
	// legitimate no-dub success.
	var dubMixIdx *storage.DubMixArtifactIndex
	if strings.TrimSpace(in.RunID) != "" {
		dubMixIdx, err = s.db.GetDubMixArtifactIndexByRun(ctx, in.RunID)
		if err == nil && dubMixIdx != nil && (dubMixIdx.AssetID != in.AssetID || !strings.EqualFold(dubMixIdx.TargetLanguage, in.TargetLanguage)) {
			return nil, fmt.Errorf("dub mix artifact run binding mismatch for run %s", in.RunID)
		}
	} else {
		dubMixIdx, err = s.db.GetDubMixArtifactIndex(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil {
		return nil, fmt.Errorf("get dub mix artifact index: %w", err)
	}
	if dubMixIdx == nil || dubMixIdx.CASHash == "" {
		return nil, errors.New("dub mix artifact index is required for region correction")
	}
	dubMixCAS := dubMixIdx.CASHash

	planIn := RenderPlanInput{
		RunID:          in.RunID,
		JobID:          in.JobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		DubMixCAS:      dubMixCAS,
		SubtitleCues:   visTrack.SubtitleCues,
	}
	rPlan, err := s.renderSvc.FreezeRenderPlan(ctx, planIn)
	if err != nil {
		return nil, fmt.Errorf("freeze render plan rerun failed: %w", err)
	}
	result.RenderPlanCAS = rPlan.CASHash

	// Render the preview the operator will actually look at from the plan just frozen.
	// The UI resolves "the run's preview" as the latest preview artifact, so a refrozen
	// plan without a new preview leaves the pre-correction geometry on screen after a
	// reported success. Rendering is pinned to this exact plan, and it reuses the
	// already-frozen DubMix/DubSegment audio: no speech, translation, TTS or audio stage
	// runs here.
	previewArt, err := s.renderSvc.RenderPreview(ctx, RenderExecutionInput{
		RunID:          in.RunID,
		JobID:          in.JobID,
		AssetID:        in.AssetID,
		TargetLanguage: in.TargetLanguage,
		PlanProvenance: rPlan.ProvenanceHash,
		PlanCAS:        rPlan.CASHash,
	})
	if err != nil {
		return nil, fmt.Errorf("preview render for the corrected plan failed: %w", err)
	}
	result.PreviewRenderCAS = previewArt.CASHash

	// 3. Record the operator's direct-manipulation intent as an append-only audit
	// row, one per corrected region. The before/after geometry and role are fully
	// reconstructible from the immutable overridden TextRegionPlan CAS artifact
	// this correction minted; this row supplies WHO applied WHICH region edit and
	// WHY. An unrecorded correction must not be reported as successful.
	if err := s.recordRegionCorrectionAudit(ctx, in, updatedPlan.Regions); err != nil {
		return nil, err
	}

	// 4. Evaluate resolution status
	isResolved := result.LocalizedVisualTrackCAS != "" && result.LocalizedSubtitleCAS != "" && result.RenderPlanCAS != "" && result.PreviewRenderCAS != ""
	if isResolved {
		targetedMap := make(map[string]bool, len(in.Overrides))
		for _, ov := range in.Overrides {
			targetedMap[strings.TrimSpace(ov.RegionID)] = true
		}
		for _, reg := range updatedPlan.Regions {
			if targetedMap[reg.ID] {
				if reg.ReviewRequired || reg.Role == domain.TextRoleUncertain {
					isResolved = false
					break
				}
			}
		}
	}
	if isResolved {
		result.Status = domain.ReviewItemStatusAutoResolved
		result.Message = "Region geometry/classification updated and visual track regenerated (auto-resolved)"
	} else {
		result.Status = domain.ReviewItemStatusPending
		result.Message = "Region updated but candidate still requires review"
	}
	return result, nil
}

// regionCorrectionDescendants names the artifact a region correction is about to regenerate, by the
// provenance hash that is currently latest for it. Empty means nothing is current yet.
type regionCorrectionDescendants struct {
	visual   string
	subtitle string
	render   string
	preview  string
}

// currentDescendantArtifacts resolves the visual/subtitle/render/preview artifacts current for the
// scope a region correction regenerates: run-scoped when the caller supplied a run, asset-latest
// for the legacy asset-scoped route, mirroring how the correction's descendants resolve it
// themselves.
//
// A missing artifact is a valid empty snapshot. Every other storage error is returned: a read
// failure silently treated as "nothing is current" would skip part of a rollback (or let a
// correction proceed on state it could not describe), which is the opposite of failing closed.
func (s *ReviewService) currentDescendantArtifacts(ctx context.Context, in RegionGeometryCorrectionInput) (regionCorrectionDescendants, error) {
	var snap regionCorrectionDescendants
	if runID := strings.TrimSpace(in.RunID); runID != "" {
		visual, err := s.db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return snap, fmt.Errorf("read current localized visual track: %w", err)
		}
		if visual != nil {
			snap.visual = visual.ProvenanceHash
		}
		subtitle, err := s.db.GetLocalizedSubtitleTrackIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return snap, fmt.Errorf("read current localized subtitle track: %w", err)
		}
		if subtitle != nil {
			snap.subtitle = subtitle.ProvenanceHash
		}
		render, err := s.db.GetRenderPlanIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return snap, fmt.Errorf("read current render plan: %w", err)
		}
		if render != nil {
			snap.render = render.ProvenanceHash
		}
		preview, err := s.db.GetRenderArtifactIndicesByRun(ctx, runID)
		if err != nil {
			return snap, fmt.Errorf("read current preview render: %w", err)
		}
		for _, idx := range preview {
			if idx.Kind == domain.RenderKindPreview && strings.EqualFold(idx.TargetLanguage, in.TargetLanguage) {
				snap.preview = idx.ProvenanceHash
			}
		}
		return snap, nil
	}

	visual, err := s.db.GetLocalizedVisualTrackIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current localized visual track: %w", err)
	}
	if visual != nil {
		snap.visual = visual.ProvenanceHash
	}
	subtitle, err := s.db.GetLocalizedSubtitleTrackIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current localized subtitle track: %w", err)
	}
	if subtitle != nil {
		snap.subtitle = subtitle.ProvenanceHash
	}
	render, err := s.db.GetRenderPlanIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current render plan: %w", err)
	}
	if render != nil {
		snap.render = render.ProvenanceHash
	}
	preview, err := s.db.GetLatestRenderArtifactIndex(ctx, in.AssetID, in.TargetLanguage, domain.RenderKindPreview)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return snap, fmt.Errorf("read current preview render: %w", err)
	}
	if preview != nil {
		snap.preview = preview.ProvenanceHash
	}
	return snap, nil
}

// withdrawCorrectionDescendants takes back the descendant rows a failed region correction made
// current. A changed provenance means this correction inserted that row (regeneration derives
// provenance from the overridden plan, so it cannot collide with the artifact it replaced); an
// unchanged provenance means the correction upserted a row that already existed, whose content and
// position must survive the failure untouched.
func (s *ReviewService) withdrawCorrectionDescendants(ctx context.Context, in RegionGeometryCorrectionInput, prior regionCorrectionDescendants) error {
	now, err := s.currentDescendantArtifacts(ctx, in)
	if err != nil {
		// Without a trustworthy read the rollback cannot tell which rows this correction made
		// current, so it must report that instead of silently leaving them in place.
		return fmt.Errorf("read current descendants for rollback: %w", err)
	}
	var failures []string
	if now.visual != "" && now.visual != prior.visual {
		if err := s.db.DeleteLocalizedVisualTrackIndex(ctx, now.visual); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if now.subtitle != "" && now.subtitle != prior.subtitle {
		if err := s.db.DeleteLocalizedSubtitleTrackIndex(ctx, now.subtitle); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if now.render != "" && now.render != prior.render {
		if err := s.db.DeleteRenderPlanIndex(ctx, now.render); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if now.preview != "" && now.preview != prior.preview {
		if err := s.db.DeleteRenderArtifactIndex(ctx, now.preview); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// recordRegionCorrectionAudit persists one append-only ReviewOverride row per applied
// region override, naming the corrected region and the operator intent that produced it.
// The row is region-scoped (no ReviewItemID): a correction is not an acceptance of a
// projected exception item, so it must never flip a review item to manual_override.
//
// The batch is committed in one transaction: the audit rows are the correction's commit
// point, so a partially recorded batch would leave an audit trail of an edit the caller
// then reports as failed — and the rollback has nothing to withdraw it by.
func (s *ReviewService) recordRegionCorrectionAudit(ctx context.Context, in RegionGeometryCorrectionInput, regions []domain.TrackedTextRegion) error {
	targeted := make(map[string]bool, len(in.Overrides))
	for _, ov := range in.Overrides {
		targeted[strings.TrimSpace(ov.RegionID)] = true
	}

	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "operator"
	}

	rows := make([]domain.ReviewOverride, 0, len(regions))
	for i, reg := range regions {
		if !targeted[reg.ID] {
			continue
		}
		rows = append(rows, domain.ReviewOverride{
			RunID:          in.RunID,
			JobID:          in.JobID,
			AssetID:        in.AssetID,
			TargetLanguage: in.TargetLanguage,
			ItemType:       domain.ReviewItemTypeRegionGeometry,
			Stage:          "detect_text",
			ItemIndex:      i,
			RegionID:       reg.ID,
			Action:         string(domain.ReviewOverrideActionRegionGeometry),
			Reason:         in.Reason,
			Operator:       operator,
			CreatedAt:      time.Now().UTC(),
		})
	}
	if err := s.db.SaveReviewOverrides(ctx, rows); err != nil {
		return fmt.Errorf("persist region correction audit: %w", err)
	}
	return nil
}

// EvaluateFinalRenderHandoff evaluates whether the exception queue is zero, and if so,
// executes or exposes final render handoff based on the review posture (Auto vs Review).
//
// Invariants (locked by Issue #14 DECISION.md, #18, #42):
// - Auto mode: queue reaches zero -> final render proceeds automatically.
// - Review mode: queue zero exposes an explicit 'Start final render' action.
// - If pending exceptions remain (queue > 0), final render is blocked.
func (s *ReviewService) EvaluateFinalRenderHandoff(ctx context.Context, in domain.FinalRenderHandoffInput) (*domain.FinalRenderHandoffResult, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	posture := in.Posture
	if posture == "" {
		posture = domain.ReviewPostureAuto
	}
	if posture != domain.ReviewPostureAuto && posture != domain.ReviewPostureReview {
		return nil, fmt.Errorf("invalid review posture '%s': must be 'auto' or 'review'", in.Posture)
	}
	in.Posture = posture

	// 1. Check pending review exceptions for the concrete run when RuntimeHost
	// supplied one. The legacy asset-level projection remains for callers that
	// predate run-aware handoff.
	var pendingItems []domain.ReviewItem
	var err error
	if strings.TrimSpace(in.RunID) != "" {
		pendingItems, err = s.ProjectReviewItemsForRun(ctx, in.AssetID, in.TargetLanguage, in.RunID)
	} else {
		pendingItems, err = s.ProjectReviewItems(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil {
		return nil, fmt.Errorf("query review items for final render handoff: %w", err)
	}

	if len(pendingItems) > 0 {
		return &domain.FinalRenderHandoffResult{
			AssetID:             in.AssetID,
			RunID:               in.RunID,
			JobID:               in.JobID,
			TargetLanguage:      in.TargetLanguage,
			Posture:             in.Posture,
			QueueZero:           false,
			PendingReviewCount:  len(pendingItems),
			CanStartFinalRender: false,
			AutoRenderStarted:   false,
			Action:              "review_required",
			Message:             fmt.Sprintf("Final render blocked: %d pending review exceptions require resolution or manual override", len(pendingItems)),
		}, nil
	}

	// 2. The run's own delivery lineage must still be current. A correction supersedes the dub mix or
	// render plan for one run; until that run re-freezes them, a handoff would publish the artifacts the
	// correction replaced, so the refusal here names them and keeps the run resumable.
	if reason, err := s.staleDeliveryLineage(ctx, in.AssetID, in.TargetLanguage, in.RunID); err != nil {
		return nil, err
	} else if reason != "" {
		return nil, errors.New(reason)
	}

	// 3. Queue is zero and the lineage is current.
	result := &domain.FinalRenderHandoffResult{
		AssetID:             in.AssetID,
		RunID:               in.RunID,
		JobID:               in.JobID,
		TargetLanguage:      in.TargetLanguage,
		Posture:             in.Posture,
		QueueZero:           true,
		PendingReviewCount:  0,
		CanStartFinalRender: true,
	}

	if in.Posture == domain.ReviewPostureAuto {
		result.AutoRenderStarted = true
		result.Action = "auto_render_started"
		if s.renderSvc != nil {
			execIn := RenderExecutionInput{
				RunID:          in.RunID,
				JobID:          in.JobID,
				AssetID:        in.AssetID,
				TargetLanguage: in.TargetLanguage,
				FontFile:       in.FontFile,
			}
			art, err := s.renderSvc.RenderFinal(ctx, execIn)
			if err != nil {
				return nil, fmt.Errorf("auto final render execution failed: %w", err)
			}
			result.FinalRenderCAS = art.CASHash
			result.Message = "Queue reached zero in Auto mode: final render executed automatically"
		} else {
			result.Message = "Queue reached zero in Auto mode: final render ready"
		}
	} else {
		// Review posture: explicit Start final render action exposed
		result.AutoRenderStarted = false
		result.Action = "start_final_render"
		result.Message = "Queue reached zero in Review mode: explicit 'Start final render' action is available"
	}

	return result, nil
}

// finalRenderDeliveryStages name the run stages whose artifacts a final render consumes, in pipeline order.
// A correction that invalidates one of them leaves the run mid-delivery: the resumed pipeline rebuilds the
// stage before the handoff stage runs again.
var finalRenderDeliveryStages = []string{"audio_mix", "render_plan", "render_preview"}

// staleDeliveryLineage reports why a run may not hand off for final render, or "" when its delivery lineage
// is current. The explicit final render resolves the run's own render plan and executes the dub mix that
// plan pins, so a correction that superseded this run's mix or render plan leaves the run holding artifacts
// the correction replaced; handing off would publish them. Only this run's evidence is read: an asset-latest
// plan belongs to another run. Read failures are returned: a lineage this cannot describe must never be
// reported as fresh.
func (s *ReviewService) staleDeliveryLineage(ctx context.Context, assetID, targetLang, runID string) (string, error) {
	if s.db == nil || strings.TrimSpace(runID) == "" {
		return "", nil
	}
	stages, err := s.db.ListStageExecutions(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("read run %s delivery lineage: %w", runID, err)
	}
	// ListStageExecutions is ordered by creation, so the last row per stage is its newest attempt.
	newest := make(map[string]domain.StageExecution, len(finalRenderDeliveryStages))
	for _, se := range stages {
		for _, stage := range finalRenderDeliveryStages {
			if se.Stage == stage {
				newest[stage] = se
			}
		}
	}
	for _, stage := range finalRenderDeliveryStages {
		if se, ok := newest[stage]; ok && se.Status == domain.StageStatusQueued {
			return fmt.Sprintf("final render handoff refused: a correction invalidated the run's %s artifact and the run has not rebuilt it yet; resume the run to re-freeze the corrected lineage before final render", stage), nil
		}
	}

	mixCAS := ""
	if se, ok := newest["audio_mix"]; ok && se.Status == domain.StageStatusSucceeded {
		mixCAS = strings.TrimSpace(se.ArtifactSHA256)
	}
	if mixCAS == "" {
		idx, err := s.db.GetDubMixArtifactIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("read run %s dub mix lineage: %w", runID, err)
		}
		if idx != nil {
			mixCAS = strings.TrimSpace(idx.CASHash)
		}
	}

	planCAS := ""
	if se, ok := newest["render_plan"]; ok && se.Status == domain.StageStatusSucceeded {
		planCAS = strings.TrimSpace(se.ArtifactSHA256)
	}
	if planCAS == "" {
		// The run's own render-plan index row is the only other lineage evidence: an asset-latest plan
		// belongs to whichever run wrote it last and cannot stand in for this run's lineage (#153).
		idx, err := s.db.GetRenderPlanIndexByRun(ctx, runID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("read run %s render plan lineage: %w", runID, err)
		}
		if idx != nil {
			if idx.AssetID != assetID || !strings.EqualFold(idx.TargetLanguage, targetLang) {
				return "", fmt.Errorf("run %s render plan lineage belongs to asset %s language %s, not %s/%s",
					runID, idx.AssetID, idx.TargetLanguage, assetID, targetLang)
			}
			planCAS = strings.TrimSpace(idx.CASHash)
		}
	}
	if mixCAS == "" {
		return "", nil
	}
	if planCAS == "" {
		// A run holding a mix but no plan of its own has no describable delivery lineage: the handoff
		// would resolve the plan from evidence this run never pinned, so refuse instead of reporting
		// the run fresh (#153).
		return fmt.Sprintf("final render handoff refused: run %s has no render plan in its own lineage; re-freeze the render plan from its current dub mix before final render", runID), nil
	}

	rc, err := s.cas.Get(planCAS)
	if err != nil {
		return "", fmt.Errorf("read render plan %s for handoff lineage: %w", planCAS, err)
	}
	defer rc.Close()
	var plan domain.RenderPlan
	if err := json.NewDecoder(rc).Decode(&plan); err != nil {
		return "", fmt.Errorf("decode render plan %s for handoff lineage: %w", planCAS, err)
	}
	if plan.DubMixCASHash != "" && plan.DubMixCASHash != mixCAS {
		return fmt.Sprintf("final render handoff refused: render plan %s still pins dub mix %s but the run's current dub mix is %s; re-freeze the render plan from the corrected mix before final render", planCAS, plan.DubMixCASHash, mixCAS), nil
	}
	return "", nil
}

// ProjectReviewItems collects and projects all actionable review exceptions for a given asset and target language.
// Invariant: Exception-only review — passing, auto-resolved, or manually-overridden items do not surface in the pending review queue.
func (s *ReviewService) ProjectReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
	return s.projectReviewItems(ctx, assetID, targetLang, "")
}

// ProjectReviewItemsForRun projects actionable exceptions against the artifacts
// frozen for one concrete run. Asset-scoped source analysis (audio roles/text
// regions) remains shared, while run-produced artifacts are resolved by run ID.
func (s *ReviewService) ProjectReviewItemsForRun(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run_id is required")
	}
	return s.projectReviewItems(ctx, assetID, targetLang, runID)
}

func (s *ReviewService) projectReviewItems(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	allItems, err := s.projectAllReviewItems(ctx, assetID, targetLang, runID)
	if err != nil {
		return nil, err
	}

	var pendingItems []domain.ReviewItem
	for _, it := range allItems {
		if it.Status == domain.ReviewItemStatusPending {
			pendingItems = append(pendingItems, it)
		}
	}
	return pendingItems, nil
}

// ProjectAllReviewItems collects all review exceptions along with their resolution status (pending, auto_pass, auto_resolved, manual_override).
// runBoundArtifactCAS resolves the variant artifact a run bound when the run owns no index row of its
// own. A run whose stages were all cache hits consumes artifacts produced by an older run, so the
// run-scoped lookups find nothing and the review queue would silently stay empty for that run - hiding the
// dub overruns that block the mix. The run's own stage execution records the artifact it consumed, so the
// projection reads the variant through it, still pinned to this asset and language.
func (s *ReviewService) runBoundArtifactCAS(ctx context.Context, runID, stage string) (string, error) {
	casHash, err := s.db.GetStageArtifactHash(ctx, runID, stage)
	if err != nil {
		return "", fmt.Errorf("load run-bound %s artifact for run %s: %w", stage, runID, err)
	}
	return casHash, nil
}

// MaxDubMediaPlaybackBytes bounds one artifact served by the run-scoped dubbing media endpoint.
// It is review/transport policy for this endpoint, so it owns its own literal rather than
// aliasing the transform's output bound: the two currently agree (both 64 MiB) because the
// inspector must be able to audition every audio artifact a run can commit for a review-only
// candidate, but either ceiling may move independently - a tighter playback bound, a larger
// transform cap - and neither move should silently redefine the other.
const MaxDubMediaPlaybackBytes int64 = 64 << 20

// Run-scoped dubbing media playback (Issue #156). The transport resolves nothing itself: it
// parses the request, calls OpenRunDubMedia and maps these sentinel classes to status codes.
var (
	// ErrDubMediaInvalidHash reports a requested media hash that is not a sha256 digest.
	ErrDubMediaInvalidHash = errors.New("invalid audio hash: must be a 64-character hex sha256")
	// ErrDubMediaRunNotFound reports an unknown run, or a run whose job no longer exists.
	ErrDubMediaRunNotFound = errors.New("run not found")
	// ErrDubMediaVariantAbsent reports a run that owns no dubbing variant at all.
	ErrDubMediaVariantAbsent = errors.New("dub segments variant not found for run")
	// ErrDubMediaNotOwned reports a variant or hash bound to another asset, language or run.
	ErrDubMediaNotOwned = errors.New("dubbing media not owned by this run")
	// ErrDubMediaTooLarge reports an owned artifact past the playback size bound.
	ErrDubMediaTooLarge = errors.New("audio artifact exceeds playback size limit")
	// ErrDubMediaObjectMissing reports an owned hash whose CAS object is absent.
	ErrDubMediaObjectMissing = errors.New("audio media artifact not found in store")
)

// DubMediaSource is one resolved, run-owned audio artifact ready to stream.
type DubMediaSource struct {
	Reader io.ReadCloser
	Size   int64
}

// OpenRunDubMedia resolves one bounded audio artifact (natural or transformed) for inspector
// audition. It is strictly run-scoped and hash-owned: the hash must appear in this run's
// current DubSegmentsVariant - as a natural waveform on Segments/ReviewSegments or as an
// atempo alternative on ReviewSegments - so arbitrary paths, cross-run hashes and foreign
// hashes are refused. Playback reads CAS only and performs no provider, TTS or transform call.
func (s *ReviewService) OpenRunDubMedia(ctx context.Context, runID, rawHash string) (*DubMediaSource, error) {
	if s == nil || s.db == nil || s.cas == nil {
		return nil, errors.New("database and CAS store required for dubbing playback")
	}
	hash := strings.ToLower(strings.TrimSpace(rawHash))
	if _, hexErr := hex.DecodeString(hash); hexErr != nil || len(hash) != 64 {
		return nil, fmt.Errorf("%w: %q", ErrDubMediaInvalidHash, rawHash)
	}
	run, err := s.db.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrDubMediaRunNotFound, runID)
		}
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	job, err := s.db.GetJob(ctx, run.JobID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("%w: job %s", ErrDubMediaRunNotFound, run.JobID)
		}
		return nil, fmt.Errorf("load job %s: %w", run.JobID, err)
	}
	idx, err := s.runScopedDubbingVariantIndex(ctx, runID, job.SourceAssetID, job.TargetLanguage)
	if err != nil {
		return nil, err
	}
	if idx == nil {
		return nil, fmt.Errorf("%w: run %s", ErrDubMediaVariantAbsent, runID)
	}
	variant, err := s.loadRunDubbingVariant(idx, job.SourceAssetID, job.TargetLanguage)
	if err != nil {
		return nil, err
	}
	if err := s.verifyVariantVoiceLineage(ctx, runID, job.SourceAssetID, job.TargetLanguage, variant); err != nil {
		return nil, err
	}
	if !dubVariantOwnsAudioHash(variant, hash) {
		return nil, fmt.Errorf("%w: audio hash is not owned by this run's dubbing variant", ErrDubMediaNotOwned)
	}
	// Playback enforces its own named ceiling (MaxDubMediaPlaybackBytes) against the committed
	// object size, so an oversized or hostile artifact is refused before any of its bytes are read.
	size, err := s.cas.Size(hash)
	if err != nil {
		if errors.Is(err, cas.ErrObjectNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrDubMediaObjectMissing, hash)
		}
		return nil, fmt.Errorf("stat audio media %s: %w", hash, err)
	}
	if size > MaxDubMediaPlaybackBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrDubMediaTooLarge, size)
	}
	reader, err := s.cas.Get(hash)
	if err != nil {
		if errors.Is(err, cas.ErrObjectNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrDubMediaObjectMissing, hash)
		}
		return nil, fmt.Errorf("open audio media %s: %w", hash, err)
	}
	return &DubMediaSource{Reader: reader, Size: size}, nil
}

// dubVariantOwnsAudioHash reports whether hash is one of the variant's committed audio
// references: a selected segment waveform, a review segment's retained natural waveform, either
// side of its tempo candidate, or either side of an accepted reviewed candidate (#157). The last
// one is what keeps a promoted unit's own retained natural parent playable and auditable after the
// acceptance: the unit leaves ReviewSegments, so the acceptance evidence is the only remaining
// reference to the bytes the operator compared against.
func dubVariantOwnsAudioHash(variant *domain.DubSegmentsVariant, hash string) bool {
	if variant == nil || hash == "" {
		return false
	}
	if slices.ContainsFunc(variant.Segments, func(seg domain.DubSegment) bool {
		return strings.EqualFold(seg.AudioSHA256, hash)
	}) {
		return true
	}
	if slices.ContainsFunc(variant.AcceptedCandidates, func(accepted domain.AcceptedReviewCandidate) bool {
		return strings.EqualFold(accepted.SelectedAudioSHA256, hash) ||
			strings.EqualFold(accepted.NaturalAudioSHA256, hash)
	}) {
		return true
	}
	return slices.ContainsFunc(variant.ReviewSegments, func(rev domain.DubSegmentReview) bool {
		if strings.EqualFold(rev.AudioSHA256, hash) {
			return true
		}
		tc := rev.TempoCandidate
		return tc != nil && (strings.EqualFold(tc.NaturalAudioSHA256, hash) ||
			strings.EqualFold(tc.TransformedAudioSHA256, hash))
	})
}

// runScopedDubbingVariantIndex resolves the DubSegmentsVariant index bound to runID, preferring
// the run's own index row and falling back to the variant its dub_synthesize stage execution
// recorded, still pinned to this asset and language. (nil, nil) means the run owns no dubbing
// variant at all; a row bound to another asset or language is ErrDubMediaNotOwned.
//
// The index row is fetched by run_id, so its RunID is the requested run and is the claim the
// decoded variant must satisfy. The fallback instead synthesizes an index from the requested
// run's own dub_synthesize stage artifact, with no RunID claim: a replay run legitimately
// consumes an artifact whose immutable body an older run produced (see runBoundArtifactCAS), so
// the stage execution - not a re-derived run claim - is the authoritative run binding. The
// asset/language binding still applies to both paths.
func (s *ReviewService) runScopedDubbingVariantIndex(ctx context.Context, runID, assetID, targetLang string) (*storage.DubSegmentsVariantIndex, error) {
	idx, err := s.db.GetDubSegmentsVariantIndexByRun(ctx, runID)
	if errors.Is(err, storage.ErrNotFound) {
		casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "dub_synthesize")
		if boundErr != nil {
			return nil, boundErr
		}
		if casHash == "" {
			return nil, nil
		}
		idx, err = &storage.DubSegmentsVariantIndex{AssetID: assetID, TargetLanguage: targetLang, CASHash: casHash}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query dub segments variant index: %w", err)
	}
	if idx.AssetID != assetID || !strings.EqualFold(idx.TargetLanguage, targetLang) {
		return nil, fmt.Errorf("%w: dub segments variant run binding mismatch for run %s", ErrDubMediaNotOwned, runID)
	}
	return idx, nil
}

// loadRunDubbingVariant reads and decodes one indexed variant, verifying it still belongs to
// this asset and language - and to idx's run when the index row claims one - before any of its
// audio hashes are trusted.
func (s *ReviewService) loadRunDubbingVariant(idx *storage.DubSegmentsVariantIndex, assetID, targetLang string) (*domain.DubSegmentsVariant, error) {
	if idx.CASHash == "" {
		return nil, errors.New("dub segments variant index has empty CAS hash")
	}
	rc, err := s.cas.Get(idx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("load dub segments variant from CAS (%s): %w", idx.CASHash, err)
	}
	defer rc.Close()
	var variant domain.DubSegmentsVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		return nil, fmt.Errorf("decode dub segments variant (%s): %w", idx.CASHash, err)
	}
	if variant.AssetID != assetID || !strings.EqualFold(variant.TargetLanguage, targetLang) ||
		(idx.RunID != "" && variant.RunID != idx.RunID) {
		return nil, fmt.Errorf("%w: dub segments variant ownership mismatch for asset %q target %q", ErrDubMediaNotOwned, assetID, targetLang)
	}
	return &variant, nil
}

// verifyVariantVoiceLineage refuses a variant the run itself produced under a voice assignment
// that is no longer the one in force for the run (#156). The dub-variant row and the run's
// dub_synthesize stage artifact both survive a reassignment: the row keeps the last pass it
// claimed, and executeRun records the superseded pass a refused claim returned, so without this
// check the inspector would play and project a superseded assignment's audio and tempo evidence
// after ReassignVoice made another assignment current.
//
// Only a variant that names the requested run is judged, because its voice assignment is the
// run's own lineage. A cross-run replay binds another run's artifact through the stage execution
// and carries that run's assignment, which is the replay run's authority, not a contradiction;
// a run with no assignment row at all has no lineage to contradict. Everything else fails
// closed, including a variant of this run that records no assignment while the run does have one:
// missing lineage evidence is refused rather than read as an all-clear.
func (s *ReviewService) verifyVariantVoiceLineage(ctx context.Context, runID, assetID, targetLang string, variant *domain.DubSegmentsVariant) error {
	if variant == nil || runID == "" || variant.RunID != runID {
		return nil
	}
	current, err := s.db.GetVoiceAssignmentIndexByRun(ctx, assetID, runID, strings.ToLower(strings.TrimSpace(targetLang)))
	if errors.Is(err, storage.ErrNotFound) && targetLang != strings.ToLower(strings.TrimSpace(targetLang)) {
		// The voice-assignment row can be written from differently cased client input than the job
		// row this lookup is keyed on, so a case-sensitive miss would read as "no assignment in
		// force" and silently exempt the very variant this guard exists to refuse.
		current, err = s.db.GetVoiceAssignmentIndexByRun(ctx, assetID, runID, targetLang)
	}
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("load current voice assignment for run %s: %w", runID, err)
	}
	if current == nil || current.CASHash == "" {
		return nil
	}
	if current.CASHash != variant.VoiceAssignmentCAS {
		return fmt.Errorf("%w: dub segments variant was not produced under the voice assignment in force for run %s", ErrDubMediaNotOwned, runID)
	}
	return nil
}

func (s *ReviewService) ProjectAllReviewItems(ctx context.Context, assetID, targetLang string) ([]domain.ReviewItem, error) {
	return s.projectAllReviewItems(ctx, assetID, targetLang, "")
}

// ProjectAllReviewItemsForRun is the audit-inclusive run-scoped projection.
func (s *ReviewService) ProjectAllReviewItemsForRun(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run_id is required")
	}
	return s.projectAllReviewItems(ctx, assetID, targetLang, runID)
}

func (s *ReviewService) projectAllReviewItems(ctx context.Context, assetID, targetLang, runID string) ([]domain.ReviewItem, error) {
	if strings.TrimSpace(assetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if targetLang == "" {
		targetLang = "vi"
	}

	var items []domain.ReviewItem

	// 1. Check AudioRolePlan for uncertain audio roles
	plan, err := s.db.GetAudioRolePlan(ctx, assetID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query audio role plan: %w", err)
	}
	if plan != nil {
		for i, seg := range plan.Segments {
			if seg.Role == domain.AudioRoleUncertain {
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-audio-role-%s-%d-%d", assetID, seg.StartMs, seg.EndMs),
					AssetID:        assetID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeAudioRole,
					Stage:          "audio_role_plan",
					ItemIndex:      i,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         "uncertain_audio_role",
					Status:         domain.ReviewItemStatusPending,
					CreatedAt:      plan.CreatedAt,
				})
			}
		}
	}

	// 2. Check TextRegionPlan for low confidence or uncertain text regions
	textIdx, err := s.db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query text region plan index: %w", err)
	}
	if textIdx != nil {
		if textIdx.CASHash == "" {
			return nil, fmt.Errorf("text region plan index has empty CAS hash")
		}
		rc, err := s.cas.Get(textIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load text region plan from CAS (%s): %w", textIdx.CASHash, err)
		}
		var plan domain.TextRegionPlan
		if err := json.NewDecoder(rc).Decode(&plan); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode text region plan (%s): %w", textIdx.CASHash, err)
		}
		rc.Close()
		createdAt := plan.CreatedAt
		if createdAt.IsZero() {
			createdAt = textIdx.CreatedAt
		}
		for i, reg := range plan.Regions {
			if reg.ReviewRequired || reg.Role == domain.TextRoleUncertain {
				itemType := domain.ReviewItemTypeLowConfidenceOCR
				if reg.Role == domain.TextRoleUncertain {
					itemType = domain.ReviewItemTypeUncertainRole
				}
				reason := reg.ReviewReason
				if reason == "" {
					reason = "text_region_requires_review"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-text-%s-%s", textIdx.CASHash, reg.ID),
					AssetID:        assetID,
					TargetLanguage: targetLang,
					Type:           itemType,
					Stage:          "detect_text",
					ItemIndex:      i,
					RegionID:       reg.ID,
					StartMs:        reg.FirstSeenMs,
					EndMs:          reg.LastSeenMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"text": reg.Text,
						"role": string(reg.Role),
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 3. Check the LocalizedVisualTrack for overlays skipped because they could not clear
	// a protected UI box. The overlay is absent from the track, so the region would ship
	// with its source text untouched unless the operator moves, reclassifies, or accepts it.
	var visIdx *storage.LocalizedVisualTrackIndex
	if runID != "" {
		visIdx, err = s.db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "visual_text_localize")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				visIdx = &storage.LocalizedVisualTrackIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
	} else {
		visIdx, err = s.db.GetLocalizedVisualTrackIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query localized visual track index: %w", err)
	}
	if visIdx != nil && visIdx.CASHash != "" {
		rc, err := s.cas.Get(visIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load localized visual track from CAS (%s): %w", visIdx.CASHash, err)
		}
		var vis domain.LocalizedVisualTrack
		decodeErr := json.NewDecoder(rc).Decode(&vis)
		rc.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode localized visual track (%s): %w", visIdx.CASHash, decodeErr)
		}
		if vis.AssetID != assetID || !strings.EqualFold(vis.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("localized visual track ownership mismatch for asset %q target %q", assetID, targetLang)
		}
		createdAt := vis.CreatedAt
		if createdAt.IsZero() {
			createdAt = visIdx.CreatedAt
		}
		for _, occ := range vis.Occlusions {
			items = append(items, domain.ReviewItem{
				ID:             fmt.Sprintf("rev-visual-occlusion-%s-%s", visIdx.CASHash, occ.RegionID),
				AssetID:        assetID,
				TargetLanguage: targetLang,
				Type:           domain.ReviewItemTypeVisualOcclusion,
				Stage:          "visual_text_localize",
				RegionID:       occ.RegionID,
				StartMs:        occ.StartMs,
				EndMs:          occ.EndMs,
				Severity:       "warning",
				Reason:         "overlay_occludes_protected_region",
				Details: map[string]any{
					"role":          string(occ.Role),
					"source_text":   occ.SourceText,
					"overlay_box":   occ.OverlayBox,
					"protected_box": occ.ProtectedBox,
				},
				Status:    domain.ReviewItemStatusPending,
				CreatedAt: createdAt,
			})
		}
	}

	// 4. Check TranslationVariant for meaning QA failures
	var transIdx *storage.TranslationVariantIndex
	if runID != "" {
		transIdx, err = s.db.GetTranslationVariantIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "translation")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				transIdx = &storage.TranslationVariantIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
		if err == nil && (transIdx.AssetID != assetID || !strings.EqualFold(transIdx.TargetLanguage, targetLang)) {
			return nil, fmt.Errorf("translation variant run binding mismatch for run %s", runID)
		}
	} else {
		transIdx, err = s.db.GetTranslationVariantIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query translation variant index: %w", err)
	}
	if transIdx != nil {
		if transIdx.CASHash == "" {
			return nil, fmt.Errorf("translation variant index has empty CAS hash")
		}
		rc, err := s.cas.Get(transIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load translation variant from CAS (%s): %w", transIdx.CASHash, err)
		}
		var tVar domain.TranslationVariant
		if err := json.NewDecoder(rc).Decode(&tVar); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode translation variant (%s): %w", transIdx.CASHash, err)
		}
		rc.Close()
		if tVar.AssetID != assetID || !strings.EqualFold(tVar.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("%w: translation variant ownership mismatch for asset %q target %q", domain.ErrMeaningPreservationFailed, assetID, targetLang)
		}
		createdAt := tVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = transIdx.CreatedAt
		}
		for _, seg := range tVar.Segments {
			if !seg.PassedQAGate || seg.QAConfidence < 0.6 {
				reason := seg.ReviewReason
				if reason == "" {
					reason = "low_meaning_confidence"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-trans-%s-%d", transIdx.CASHash, seg.Index),
					RunID:          tVar.RunID,
					AssetID:        assetID,
					JobID:          tVar.JobID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeTranslationQA,
					Stage:          "translation",
					ItemIndex:      seg.Index,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"source_text":   seg.SourceText,
						"target_text":   seg.TargetText,
						"qa_confidence": seg.QAConfidence,
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 5. Check DubSegmentsVariant for TTS overruns / unselected review items
	var hasDubSegments bool
	var dubSegIdx *storage.DubSegmentsVariantIndex
	if runID != "" {
		dubSegIdx, err = s.runScopedDubbingVariantIndex(ctx, runID, assetID, targetLang)
		if err != nil {
			return nil, err
		}
	} else {
		dubSegIdx, err = s.db.GetDubSegmentsVariantIndex(ctx, assetID, targetLang)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("query dub segments variant index: %w", err)
		}
	}
	if dubSegIdx != nil {
		dsVar, err := s.loadRunDubbingVariant(dubSegIdx, assetID, targetLang)
		if err != nil {
			return nil, err
		}
		if err := s.verifyVariantVoiceLineage(ctx, runID, assetID, targetLang, dsVar); err != nil {
			return nil, err
		}
		hasDubSegments = true
		createdAt := dsVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = dubSegIdx.CreatedAt
		}
		// ReviewSegments (unselected or unresolvable candidates)
		for _, rev := range dsVar.ReviewSegments {
			reason := rev.ReviewReason
			if reason == "" {
				reason = "tts_unresolvable_overrun"
			}
			details := map[string]any{
				"measured_duration_ms": rev.MeasuredDurationMs,
				"slot_duration_ms":     rev.SlotDurationMs,
				"fit_decision":         string(rev.FitDecision),
				"attempt_count":        rev.AttemptCount,
				"natural_audio_sha256": rev.AudioSHA256,
			}
			if tc := rev.TempoCandidate; tc != nil {
				details["tempo_candidate"] = map[string]any{
					"factor":                   tc.Factor,
					"natural_audio_sha256":     tc.NaturalAudioSHA256,
					"transformed_audio_sha256": tc.TransformedAudioSHA256,
					"natural_duration_ms":      tc.NaturalDurationMs,
					"transformed_duration_ms":  tc.TransformedDurationMs,
					"playback_duration_ms":     tc.PlaybackDurationMs,
					"selectable":               tc.Selectable,
					"reason":                   tc.Reason,
					"tool_id":                  tc.ToolID,
					"filter":                   tc.Filter,
					"policy_version":           tc.PolicyVersion,
				}
			}
			items = append(items, domain.ReviewItem{
				ID:             fmt.Sprintf("rev-dubseg-rev-%s-%d", dubSegIdx.CASHash, rev.Index),
				RunID:          dsVar.RunID,
				AssetID:        assetID,
				JobID:          dsVar.JobID,
				TargetLanguage: targetLang,
				Type:           domain.ReviewItemTypeTTSOverrun,
				Stage:          "dub_synthesize",
				ItemIndex:      rev.Index,
				SpeakerID:      rev.SpeakerID,
				StartMs:        rev.StartMs,
				EndMs:          rev.EndMs,
				Severity:       "blocker",
				Reason:         reason,
				Details:        details,
				Status:         domain.ReviewItemStatusPending,
				CreatedAt:      createdAt,
			})
		}
		// Segments that flagged RequiresReview
		for _, seg := range dsVar.Segments {
			if seg.RequiresReview || seg.FitDecision == domain.FitActionReview {
				reason := seg.ReviewReason
				if reason == "" {
					reason = "tts_segment_requires_review"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-dubseg-seg-%s-%d", dubSegIdx.CASHash, seg.Index),
					RunID:          dsVar.RunID,
					AssetID:        assetID,
					JobID:          dsVar.JobID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeTTSOverrun,
					Stage:          "dub_synthesize",
					ItemIndex:      seg.Index,
					SpeakerID:      seg.SpeakerID,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"measured_duration_ms": seg.MeasuredDurationMs,
						"slot_duration_ms":     seg.SlotDurationMs,
						"fit_decision":         string(seg.FitDecision),
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 6. Check DubScriptVariant for spoken adaptation QA failures or unresolved timing flags
	var dubScriptIdx *storage.DubScriptVariantIndex
	if runID != "" {
		dubScriptIdx, err = s.db.GetDubScriptVariantIndexByRun(ctx, runID)
		if errors.Is(err, storage.ErrNotFound) {
			casHash, boundErr := s.runBoundArtifactCAS(ctx, runID, "dub_script")
			if boundErr != nil {
				return nil, boundErr
			}
			if casHash != "" {
				dubScriptIdx = &storage.DubScriptVariantIndex{AssetID: assetID, RunID: runID, TargetLanguage: targetLang, CASHash: casHash}
				err = nil
			}
		}
		if err == nil && (dubScriptIdx.AssetID != assetID || !strings.EqualFold(dubScriptIdx.TargetLanguage, targetLang)) {
			return nil, fmt.Errorf("dub script variant run binding mismatch for run %s", runID)
		}
	} else {
		dubScriptIdx, err = s.db.GetDubScriptVariantIndex(ctx, assetID, targetLang)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query dub script variant index: %w", err)
	}
	if dubScriptIdx != nil {
		if dubScriptIdx.CASHash == "" {
			return nil, fmt.Errorf("dub script variant index has empty CAS hash")
		}
		rc, err := s.cas.Get(dubScriptIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("load dub script variant from CAS (%s): %w", dubScriptIdx.CASHash, err)
		}
		var dsVar domain.DubScriptVariant
		if err := json.NewDecoder(rc).Decode(&dsVar); err != nil {
			rc.Close()
			return nil, fmt.Errorf("decode dub script variant (%s): %w", dubScriptIdx.CASHash, err)
		}
		rc.Close()
		createdAt := dsVar.CreatedAt
		if createdAt.IsZero() {
			createdAt = dubScriptIdx.CreatedAt
		}
		for _, seg := range dsVar.Segments {
			// If !seg.PassedQAGate, always surface as a QA exception.
			// If seg.RequiresReview is set for timing, only surface if DubSegments has not yet resolved it.
			if !seg.PassedQAGate || (!hasDubSegments && seg.RequiresReview) {
				reason := seg.ReviewReason
				if reason == "" {
					reason = "dub_script_qa_flag"
				}
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-dubscript-%s-%d", dubScriptIdx.CASHash, seg.Index),
					RunID:          dsVar.RunID,
					AssetID:        assetID,
					JobID:          dsVar.JobID,
					TargetLanguage: targetLang,
					Type:           domain.ReviewItemTypeDubScriptQA,
					Stage:          "dub_script",
					ItemIndex:      seg.Index,
					StartMs:        seg.StartMs,
					EndMs:          seg.EndMs,
					Severity:       "warning",
					Reason:         reason,
					Details: map[string]any{
						"source_text": seg.SourceText,
						"spoken_text": seg.SpokenText,
					},
					Status:    domain.ReviewItemStatusPending,
					CreatedAt: createdAt,
				})
			}
		}
	}

	// 7. Check QualityResults for multimodal QC records (both auto_pass records and flagged issues)
	var qualityResults []domain.QualityResult
	if runID != "" {
		qualityResults, err = s.db.GetQualityResultsByRun(ctx, runID)
	} else {
		qualityResults, err = s.db.GetQualityResults(ctx, assetID, targetLang, "")
	}
	if err == nil {
		for _, qr := range qualityResults {
			if runID != "" {
				if qr.RunID != runID || qr.AssetID != assetID || (qr.TargetLanguage != "" && !strings.EqualFold(qr.TargetLanguage, targetLang)) {
					return nil, fmt.Errorf("quality result run binding mismatch for run %s", runID)
				}
			}
			if qr.OverallStatus == domain.QualityStatusPass {
				items = append(items, domain.ReviewItem{
					ID:             fmt.Sprintf("rev-quality-pass-%s", qr.ID),
					AssetID:        assetID,
					TargetLanguage: targetLang,
					RunID:          qr.RunID,
					JobID:          qr.JobID,
					Stage:          qr.Stage,
					Type:           domain.ReviewItemType(qr.Stage + "_quality"),
					Severity:       "info",
					Reason:         "automated_quality_gates_passed",
					Status:         domain.ReviewItemStatusAutoPass,
					CreatedAt:      qr.CreatedAt,
				})
			} else if qr.OverallStatus == domain.QualityStatusReviewRequired || qr.OverallStatus == domain.QualityStatusFail {
				for _, iss := range qr.Issues {
					if iss.ID == "" {
						iss.ID = fmt.Sprintf("rev-quality-%s-%s", qr.ID, iss.Stage)
					}
					if iss.AssetID == "" {
						iss.AssetID = assetID
					}
					if iss.TargetLanguage == "" {
						iss.TargetLanguage = targetLang
					}
					if iss.RunID == "" {
						iss.RunID = qr.RunID
					}
					if iss.JobID == "" {
						iss.JobID = qr.JobID
					}
					if iss.Status == "" {
						iss.Status = domain.ReviewItemStatusPending
					}
					if iss.CreatedAt.IsZero() {
						iss.CreatedAt = qr.CreatedAt
					}
					items = append(items, iss)
				}
			}
		}
	}

	// 7. Apply recorded manual overrides
	var overrides []domain.ReviewOverride
	if runID != "" {
		overrides, err = s.db.GetReviewOverridesByRun(ctx, runID)
		if err == nil {
			for _, ro := range overrides {
				if ro.RunID != runID || ro.AssetID != assetID || !strings.EqualFold(ro.TargetLanguage, targetLang) {
					return nil, fmt.Errorf("review override run binding mismatch for run %s", runID)
				}
			}
		}
	} else {
		overrides, err = s.db.GetReviewOverrides(ctx, assetID, targetLang)
	}
	if err == nil && len(overrides) > 0 {
		for i := range items {
			if matchOverride(items[i], overrides) != nil {
				items[i].Status = domain.ReviewItemStatusManualOverride
			}
		}
	}

	return items, nil
}

func matchOverride(it domain.ReviewItem, overrides []domain.ReviewOverride) *domain.ReviewOverride {
	for _, ro := range overrides {
		if ro.ReviewItemID != "" && ro.ReviewItemID == it.ID {
			return &ro
		}
	}
	return nil
}

// overrideRegionIDs returns the region ids a correction actually touched.
func overrideRegionIDs(overrides []domain.RegionOverride) []string {
	ids := make([]string, 0, len(overrides))
	for _, o := range overrides {
		ids = append(ids, o.RegionID)
	}
	return ids
}
