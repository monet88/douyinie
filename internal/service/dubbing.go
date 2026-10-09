package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

type TTSInvokeFunc func(ctx context.Context, p provider.Provider, req provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error)

// DubbingService orchestrates voice assignment, pre-dub voice audition, TTS synthesis,
// actual synthesized-media duration probing, and the measured-duration fit controller.
type DubbingService struct {
	db            *storage.DB
	cas           *cas.Store
	router        *provider.Router
	fitController *FitController
	spokenAdapter provider.SpokenScriptAdapter

	// TTSInvoke executes one TTS provider synthesis attempt. When nil,
	// router-backed invocation is used.
	TTSInvoke TTSInvokeFunc

	// AtempoTransform overrides media.ApplyAtempoWAV for controllable transform testing.
	AtempoTransform func(ctx context.Context, req media.AtempoRequest) ([]byte, error)

	// beforeRunRowClaim, when set, runs once immediately before a pass attempts to claim the
	// run's dub-variant row. It is a test-only seam (see export_test.go): it lands a concurrent
	// operator reassignment inside the publish/escalation window deterministically, which is the
	// interleaving the claim statement's own ownership guard exists to refuse.
	beforeRunRowClaim func()
}

// lineageRecoveryState tracks per-source-lineage consumption of native-speed, measured-
// rewrite and bounded-regroup remedies within one logical synthesis attempt, carrying state
// across regroup and whole-speaker escalation without resetting budgets or topology.
type lineageRecoveryState struct {
	NativeAttempts   map[int]int
	RewriteAttempts  map[int]int
	AcceptedRewrites map[int]string
	// ChosenGroups records, per triggering canonical block index, the bounded group selected
	// for that trigger within one lineage. Membership is never empty and always begins with
	// the trigger; only an entry of two or more members carries a topology a later pass
	// replays, while a shorter one resolves no group and is re-derived from the same pinned
	// canonical inputs.
	ChosenGroups map[int]chosenRegroup
}

// chosenRegroup is one lineage's chosen bounded group: the exact member block indices in
// source order and the merged text that was selected for them, which stay authoritative
// when a later pass replays the consumed remedy.
type chosenRegroup struct {
	MemberIndices []int
	SourceText    string
	SpokenText    string
}

// regroupPlan is one bounded same-speaker group under consideration: the member script
// segments in source order with the merged source and spoken text chosen for them.
type regroupPlan struct {
	Members       []domain.DubScriptSegment
	MemberIndices []int
	SourceText    string
	SpokenText    string
}

func newLineageRecoveryState() *lineageRecoveryState {
	return &lineageRecoveryState{
		NativeAttempts:   make(map[int]int),
		RewriteAttempts:  make(map[int]int),
		AcceptedRewrites: make(map[int]string),
		ChosenGroups:     make(map[int]chosenRegroup),
	}
}

// NewDubbingService creates a new DubbingService instance.
func NewDubbingService(db *storage.DB, casStore *cas.Store) *DubbingService {
	return &DubbingService{
		db:            db,
		cas:           casStore,
		fitController: NewFitController(),
		spokenAdapter: provider.NewDefaultSpokenScriptAdapter(),
	}
}

// ConfigureRouter injects the provider router.
func (s *DubbingService) ConfigureRouter(router *provider.Router) {
	s.router = router
}

// ConfigureFitController injects a custom fit controller.
func (s *DubbingService) ConfigureFitController(fc *FitController) {
	s.fitController = fc
}

// ConfigureSpokenAdapter injects a custom spoken script adapter for REWRITE fit handling.
func (s *DubbingService) ConfigureSpokenAdapter(adapter provider.SpokenScriptAdapter) {
	s.spokenAdapter = adapter
}

// AssignVoices assigns stable preset voice profiles per speaker and freezes the VoiceAssignment for the run.
// Engine hopping across sentences for a single speaker is strictly prohibited.
func (s *DubbingService) AssignVoices(ctx context.Context, in domain.VoiceAssignmentInput) (*domain.VoiceAssignment, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, fmt.Errorf("asset_id is required")
	}

	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang
	if s.db != nil {
		dubScriptCAS, transcriptCAS, err := s.resolveVoiceAssignmentLineageCAS(ctx, in, targetLang)
		if err != nil {
			return nil, err
		}
		in.DubScriptVariantCAS = dubScriptCAS
		in.TranscriptArtifactCAS = transcriptCAS
	}

	// Check if video has audio role plan with no dub-eligible dialogue (no-speech bypass)
	if s.db != nil && in.AssetID != "" {
		resolveRunID := strings.TrimSpace(in.RunID)
		rolePlan, err := ResolveRunScopedAudioRolePlan(ctx, s.db, s.cas, in.AssetID, resolveRunID)
		if err != nil {
			if resolveRunID != "" {
				return nil, fmt.Errorf("resolve run-scoped audio role plan for voice assignment: %w", err)
			}
		} else if rolePlan != nil {
			if !domain.IsDubEligible(rolePlan) {
				return nil, domain.ErrNoDubbingRequired
			}
		}
	}
	// 1. Resolve distinct speakers from TranscriptArtifact or DubScriptVariant
	speakers, err := s.resolveSpeakers(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("resolve speakers for voice assignment: %w", err)
	}
	if len(speakers) == 0 {
		speakers = []string{"SPEAKER_00"}
	}
	sort.Strings(speakers)

	// 1.5 Check if a VoiceAssignment is already frozen for this exact run.
	if s.db != nil {
		if existingIdx, err := s.db.GetVoiceAssignmentIndexByRun(ctx, in.AssetID, in.RunID, targetLang); err == nil && existingIdx != nil {
			var existing domain.VoiceAssignment
			loaded := false
			if s.cas != nil && existingIdx.CASHash != "" {
				if rc, err := s.cas.Get(existingIdx.CASHash); err == nil {
					defer rc.Close()
					if err := json.NewDecoder(rc).Decode(&existing); err == nil {
						existing.CASHash = existingIdx.CASHash
						loaded = true
					}
				}
			}
			if !loaded && existingIdx.AssignmentsJSON != "" {
				if err := json.Unmarshal([]byte(existingIdx.AssignmentsJSON), &existing); err == nil {
					existing.CASHash = existingIdx.CASHash
					loaded = true
				}
			}

			if loaded {
				if in.UseSameVoiceForAll != existing.UseSameVoiceForAll {
					return nil, domain.ErrVoiceAssignmentFrozen
				}
				for spk, custom := range in.CustomAssignments {
					existingProf, ok := existing.Assignments[spk]
					if !ok || !domain.VoiceProfileEquivalent(existingProf, custom) {
						return nil, domain.ErrVoiceAssignmentFrozen
					}
				}
				if existing.DubScriptVariantCAS != in.DubScriptVariantCAS || existing.TranscriptArtifactCAS != in.TranscriptArtifactCAS {
					return s.supersedeVoiceAssignment(ctx, &existing, in)
				}
				return &existing, nil
			}
		}
	}

	// 2. Build or validate assignments
	assignments := make(map[string]domain.VoiceProfile)
	presetVoices := provider.DefaultPresetVoices(targetLang)
	if len(presetVoices) == 0 {
		return nil, domain.ErrNoEligibleTTSProvider
	}

	// Deduplicate preset voices by identity for distinct assignment
	var distinctPresets []domain.VoiceProfile
	seenPreset := make(map[string]bool)
	for _, v := range presetVoices {
		key := v.ProviderID + "/" + v.VoiceID
		if !seenPreset[key] {
			seenPreset[key] = true
			distinctPresets = append(distinctPresets, v)
		}
	}
	if len(distinctPresets) == 0 {
		distinctPresets = presetVoices
	}

	if in.UseSameVoiceForAll {
		// Single selected voice for all speakers (deterministic: sorted speaker key)
		var chosenVoice domain.VoiceProfile
		if len(in.CustomAssignments) > 0 {
			var customKeys []string
			for k := range in.CustomAssignments {
				customKeys = append(customKeys, k)
			}
			sort.Strings(customKeys)
			for _, k := range customKeys {
				if v := in.CustomAssignments[k]; v.ID != "" {
					if v.ProviderID != "" && v.VoiceID != "" && !provider.IsVerifiedTTSVoice(v.ProviderID, v.VoiceID) {
						return nil, fmt.Errorf("%w: custom voice assignment for speaker %s has unverified voice %q on provider %s", domain.ErrTTSVoiceAssetMissing, k, v.VoiceID, v.ProviderID)
					}
					chosenVoice = v
					break
				}
			}
		}
		if chosenVoice.ID == "" {
			chosenVoice = distinctPresets[0]
		}
		for _, spk := range speakers {
			assignments[spk] = chosenVoice
		}
	} else {
		// Assign distinct preset voices per speaker
		for i, spk := range speakers {
			if custom, ok := in.CustomAssignments[spk]; ok && custom.ID != "" {
				if custom.ProviderID != "" && custom.VoiceID != "" && !provider.IsVerifiedTTSVoice(custom.ProviderID, custom.VoiceID) {
					return nil, fmt.Errorf("%w: custom voice assignment for speaker %s has unverified voice %q on provider %s", domain.ErrTTSVoiceAssetMissing, spk, custom.VoiceID, custom.ProviderID)
				}
				assignments[spk] = custom
			} else {
				presetIdx := i % len(distinctPresets)
				assignments[spk] = distinctPresets[presetIdx]
			}
		}
	}
	distinguishabilityQC := domain.EvaluateVoiceDistinguishability(assignments, in.UseSameVoiceForAll)

	// 3. Compute deterministic provenance hash
	provenanceHash, err := s.computeVoiceAssignmentProvenanceHash(in, assignments)
	if err != nil {
		return nil, fmt.Errorf("compute voice assignment cache identity: %w", err)
	}
	// 4. Check idempotent cache in SQLite / CAS
	if s.db != nil && s.cas != nil {
		if cachedIdx, err := s.db.GetVoiceAssignmentByProvenance(ctx, provenanceHash); err == nil && cachedIdx != nil {
			rc, err := s.cas.Get(cachedIdx.CASHash)
			if err == nil {
				defer rc.Close()
				data, err := io.ReadAll(rc)
				if err == nil {
					var cachedAssignment domain.VoiceAssignment
					if err := json.Unmarshal(data, &cachedAssignment); err == nil {
						cachedAssignment.CASHash = cachedIdx.CASHash
						cachedAssignment.ProvenanceHash = cachedIdx.ProvenanceHash
						cachedAssignment.RunID = in.RunID
						cachedAssignment.JobID = in.JobID
						// Ensure index is also bound for this run
						idx := storage.VoiceAssignmentIndex{
							ID:                 uuid.NewString(),
							AssetID:            in.AssetID,
							RunID:              in.RunID,
							JobID:              in.JobID,
							TargetLanguage:     targetLang,
							CASHash:            cachedIdx.CASHash,
							ProvenanceHash:     provenanceHash,
							AssignmentsJSON:    string(data),
							UseSameVoiceForAll: in.UseSameVoiceForAll,
							CreatedAt:          time.Now().UTC(),
						}
						if err := s.db.SaveVoiceAssignmentIndex(ctx, idx); err != nil {
							return nil, fmt.Errorf("bind cached voice assignment index to run: %w", err)
						}
						return &cachedAssignment, nil
					}
				}
			}
		}
	}
	now := time.Now().UTC()
	assignment := &domain.VoiceAssignment{
		ID:                    uuid.NewString(),
		SchemaVersion:         domain.VoiceAssignmentSchemaVersion,
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		TargetLanguage:        targetLang,
		Assignments:           assignments,
		UseSameVoiceForAll:    in.UseSameVoiceForAll,
		Distinguishability:    distinguishabilityQC,
		DubScriptVariantCAS:   in.DubScriptVariantCAS,
		TranscriptArtifactCAS: in.TranscriptArtifactCAS,
		ProvenanceHash:        provenanceHash,
		FrozenAt:              now,
		CreatedAt:             now,
	}
	// 5. Commit to CAS and SQLite
	if s.cas != nil && s.db != nil {
		data, err := json.Marshal(assignment)
		if err != nil {
			return nil, fmt.Errorf("marshal voice assignment: %w", err)
		}
		casObj, err := s.cas.Put(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("put voice assignment in CAS: %w", err)
		}
		assignment.CASHash = casObj.SHA256

		idx := storage.VoiceAssignmentIndex{
			ID:                 assignment.ID,
			AssetID:            assignment.AssetID,
			RunID:              assignment.RunID,
			JobID:              assignment.JobID,
			TargetLanguage:     assignment.TargetLanguage,
			CASHash:            assignment.CASHash,
			ProvenanceHash:     assignment.ProvenanceHash,
			AssignmentsJSON:    string(data),
			UseSameVoiceForAll: assignment.UseSameVoiceForAll,
			CreatedAt:          assignment.CreatedAt,
		}
		if err := s.db.SaveVoiceAssignmentIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("save voice assignment index: %w", err)
		}
	}

	return assignment, nil
}

// resolveVoiceAssignmentLineageCAS returns the run's authoritative dub-script and transcript CAS
// lineage, proving any caller-supplied CAS against it rather than trusting it. Both values are
// frozen onto the VoiceAssignment and decide whether a later call re-reads the frozen assignment
// or supersedes it, so an unproven client value must never become lineage: run-resolved evidence
// stays the single source of truth and every disagreement fails closed (CODING_STANDARDS §10).
func (s *DubbingService) resolveVoiceAssignmentLineageCAS(ctx context.Context, in domain.VoiceAssignmentInput, targetLang string) (string, string, error) {
	suppliedDubScriptCAS := strings.TrimSpace(in.DubScriptVariantCAS)
	suppliedTranscriptCAS := strings.TrimSpace(in.TranscriptArtifactCAS)

	authoritativeDubScriptCAS := ""
	if idx, err := s.db.GetDubScriptVariantIndexByRun(ctx, in.RunID); err == nil && idx != nil {
		if idx.AssetID != in.AssetID || !strings.EqualFold(idx.TargetLanguage, targetLang) {
			return "", "", fmt.Errorf("%w: run %s dub script lineage mismatch for voice assignment", domain.ErrTranslationOwnershipMismatch, in.RunID)
		}
		authoritativeDubScriptCAS = idx.CASHash
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", "", fmt.Errorf("resolve run dub script lineage for voice assignment: %w", err)
	}
	if suppliedDubScriptCAS != "" {
		if authoritativeDubScriptCAS != "" && suppliedDubScriptCAS != authoritativeDubScriptCAS {
			return "", "", fmt.Errorf("%w: run %s dub script lineage mismatch for voice assignment: input=%s run=%s",
				domain.ErrTranslationOwnershipMismatch, in.RunID, suppliedDubScriptCAS, authoritativeDubScriptCAS)
		}
		if authoritativeDubScriptCAS == "" && s.cas != nil {
			// The run pins no script here, so the supplied artifact's own binding is the only proof left.
			dubScript, _, err := s.loadDubScriptVariant(ctx, in.AssetID, in.RunID, targetLang, suppliedDubScriptCAS)
			if err != nil {
				return "", "", fmt.Errorf("prove supplied dub script lineage for voice assignment: %w", err)
			}
			if dubScript.RunID != "" && dubScript.RunID != in.RunID {
				return "", "", fmt.Errorf("%w: supplied dub script %s belongs to run %s, not %s",
					domain.ErrTranslationOwnershipMismatch, suppliedDubScriptCAS, dubScript.RunID, in.RunID)
			}
		}
	}
	dubScriptCAS := authoritativeDubScriptCAS
	if dubScriptCAS == "" {
		dubScriptCAS = suppliedDubScriptCAS
	}

	// A supplied CAS must first prove it is this asset's transcript: ownership is decided by the
	// artifact itself, before the run's own lineage is consulted, so a foreign artifact is refused
	// as a foreign artifact instead of being reported as a lineage mismatch with the run.
	if suppliedTranscriptCAS != "" {
		if err := s.proveTranscriptArtifactBinding(suppliedTranscriptCAS, in.AssetID); err != nil {
			return "", "", err
		}
	}

	authoritativeTranscriptCAS, err := resolveRunTranscriptCAS(ctx, s.db, in.RunID, in.AssetID, "voice assignment")
	if err != nil {
		return "", "", err
	}
	if authoritativeTranscriptCAS == "" && suppliedTranscriptCAS == "" && dubScriptCAS != "" {
		// Nothing pins a transcript for this run: the dub script's translation contract still
		// names the one it was built from. Only used to fill, never to override a supplied value.
		dubScript, _, err := s.loadDubScriptVariant(ctx, in.AssetID, in.RunID, targetLang, dubScriptCAS)
		if err != nil {
			return "", "", fmt.Errorf("resolve dub script lineage for voice assignment: %w", err)
		}
		if strings.TrimSpace(dubScript.TranslationVariantCAS) != "" {
			transcriptCAS, err := s.transcriptCASFromTranslationVariant(in.AssetID, targetLang, dubScript.TranslationVariantCAS)
			if err != nil {
				return "", "", err
			}
			authoritativeTranscriptCAS = transcriptCAS
		}
	}
	if suppliedTranscriptCAS != "" && authoritativeTranscriptCAS != "" && suppliedTranscriptCAS != authoritativeTranscriptCAS {
		return "", "", fmt.Errorf("%w: run %s transcript lineage mismatch for voice assignment: input=%s run=%s",
			domain.ErrTranscriptLineageMismatch, in.RunID, suppliedTranscriptCAS, authoritativeTranscriptCAS)
	}
	if authoritativeTranscriptCAS != "" {
		return dubScriptCAS, authoritativeTranscriptCAS, nil
	}
	return dubScriptCAS, suppliedTranscriptCAS, nil
}

// proveTranscriptArtifactBinding checks that a supplied transcript CAS really is this asset's
// transcript artifact: it must decode, and it must not carry another asset's id. It is the ownership
// proof a non-empty client CAS has to pass before it can become frozen VoiceAssignment lineage.
//
// The artifact's producer run id is deliberately NOT an ownership claim: transcript identity is
// run-independent (content/provenance addressed), so a later run of the same asset legitimately reads
// the artifact an earlier run persisted - see
// test/seam1/autorun_dialogue_test.go TestSeam1_DubbingService_AssignVoices_ExplicitCAS_Validation/
// CrossRunTranscriptCAS_ReusesAssetScopedArtifact. A run that pins its own transcript is still enforced
// by the equality check in resolveVoiceAssignmentLineageCAS, and synthesis re-verifies the frozen
// transcript against the run's pinned lineage.
func (s *DubbingService) proveTranscriptArtifactBinding(casHash, assetID string) error {
	if s.cas == nil {
		return nil
	}
	rc, err := s.cas.Get(casHash)
	if err != nil {
		return fmt.Errorf("%w: read supplied transcript %s for voice assignment: %v", domain.ErrTranscriptLineageProofFailed, casHash, err)
	}
	defer rc.Close()
	var transcript domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&transcript); err != nil {
		return fmt.Errorf("%w: decode supplied transcript %s for voice assignment: %v", domain.ErrTranscriptLineageProofFailed, casHash, err)
	}
	if transcript.AssetID != "" && transcript.AssetID != assetID {
		return fmt.Errorf("%w: supplied transcript %s belongs to asset %s, not %s",
			domain.ErrTranslationOwnershipMismatch, casHash, transcript.AssetID, assetID)
	}
	return nil
}

// ReassignVoice explicitly changes one or more speakers' frozen voices for a run (Issue #40),
// recording the invalidation scope (only changed speakers' TTS/DubSegment/DubMix/FinalRender
// descendants) and supersession provenance on the new immutable VoiceAssignment.
func (s *DubbingService) ReassignVoice(ctx context.Context, in domain.VoiceAssignmentInput) (*domain.VoiceAssignment, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, fmt.Errorf("asset_id is required")
	}
	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	// 1. Existing frozen assignment must exist
	existingIdx, err := s.db.GetVoiceAssignmentIndexByRun(ctx, in.AssetID, in.RunID, targetLang)
	if err != nil || existingIdx == nil {
		return nil, domain.ErrVoiceAssignmentNotFound
	}
	var existing domain.VoiceAssignment
	loaded := false
	if s.cas != nil && existingIdx.CASHash != "" {
		if rc, err := s.cas.Get(existingIdx.CASHash); err == nil {
			defer rc.Close()
			if err := json.NewDecoder(rc).Decode(&existing); err == nil {
				existing.CASHash = existingIdx.CASHash
				loaded = true
			}
		}
	}
	if !loaded && existingIdx.AssignmentsJSON != "" {
		if err := json.Unmarshal([]byte(existingIdx.AssignmentsJSON), &existing); err == nil {
			existing.CASHash = existingIdx.CASHash
			loaded = true
		}
	}
	if !loaded {
		return nil, fmt.Errorf("load existing frozen voice assignment: %w", domain.ErrVoiceAssignmentNotFound)
	}

	return s.supersedeVoiceAssignment(ctx, &existing, in)
}

// supersedeVoiceAssignment applies custom voice profiles on top of a base frozen
// assignment and persists the result as an immutable superseding assignment that
// records the base CAS, the affected speakers, and the descendant invalidation scope.
// The base assignment object itself is never mutated.
func (s *DubbingService) supersedeVoiceAssignment(ctx context.Context, existing *domain.VoiceAssignment, in domain.VoiceAssignmentInput) (*domain.VoiceAssignment, error) {
	targetLang := in.TargetLanguage
	if in.DubScriptVariantCAS == "" {
		in.DubScriptVariantCAS = existing.DubScriptVariantCAS
	}
	if in.TranscriptArtifactCAS == "" {
		in.TranscriptArtifactCAS = existing.TranscriptArtifactCAS
	}

	// 2. Build new assignments
	newAssignments := make(map[string]domain.VoiceProfile)
	for spk, prof := range existing.Assignments {
		newAssignments[spk] = prof
	}
	if in.UseSameVoiceForAll && len(in.CustomAssignments) > 0 {
		var customKeys []string
		for k := range in.CustomAssignments {
			customKeys = append(customKeys, k)
		}
		sort.Strings(customKeys)
		var singleVoice domain.VoiceProfile
		for _, k := range customKeys {
			if v := in.CustomAssignments[k]; v.ID != "" {
				if v.ProviderID != "" && v.VoiceID != "" && !provider.IsVerifiedTTSVoice(v.ProviderID, v.VoiceID) {
					return nil, fmt.Errorf("%w: custom voice reassignment for speaker %s has unverified voice %q on provider %s", domain.ErrTTSVoiceAssetMissing, k, v.VoiceID, v.ProviderID)
				}
				singleVoice = v
				break
			}
		}
		if singleVoice.ID != "" {
			for spk := range newAssignments {
				newAssignments[spk] = singleVoice
			}
		}
	} else {
		for spk, custom := range in.CustomAssignments {
			if _, ok := newAssignments[spk]; ok && custom.ID != "" {
				if custom.ProviderID != "" && custom.VoiceID != "" && !provider.IsVerifiedTTSVoice(custom.ProviderID, custom.VoiceID) {
					return nil, fmt.Errorf("%w: custom voice reassignment for speaker %s has unverified voice %q on provider %s", domain.ErrTTSVoiceAssetMissing, spk, custom.VoiceID, custom.ProviderID)
				}
				newAssignments[spk] = custom
			}
		}
	}
	// 3. Compute affected speakers
	affected := domain.AffectedSpeakers(existing.Assignments, newAssignments)
	lineageUnchanged := existing.DubScriptVariantCAS == in.DubScriptVariantCAS && existing.TranscriptArtifactCAS == in.TranscriptArtifactCAS
	if len(affected) == 0 && existing.UseSameVoiceForAll == in.UseSameVoiceForAll && lineageUnchanged {
		// Idempotent: nothing changed
		return existing, nil
	}

	provenanceHash, err := s.computeVoiceAssignmentProvenanceHash(in, newAssignments)
	if err != nil {
		return nil, fmt.Errorf("compute reassignment cache identity: %w", err)
	}

	distinguishabilityQC := domain.EvaluateVoiceDistinguishability(newAssignments, in.UseSameVoiceForAll)
	now := time.Now().UTC()
	newAssignment := &domain.VoiceAssignment{
		ID:                    uuid.NewString(),
		SchemaVersion:         domain.VoiceAssignmentSchemaVersion,
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		TargetLanguage:        targetLang,
		Assignments:           newAssignments,
		UseSameVoiceForAll:    in.UseSameVoiceForAll,
		DubScriptVariantCAS:   in.DubScriptVariantCAS,
		TranscriptArtifactCAS: in.TranscriptArtifactCAS,
		SupersedesCAS:         existing.CASHash,
		InvalidatedSpeakers:   affected,
		InvalidationScope:     domain.VoiceChangeInvalidationStages(),
		Distinguishability:    distinguishabilityQC,
		ProvenanceHash:        provenanceHash,
		FrozenAt:              now,
		CreatedAt:             now,
	}

	// 4. Commit new assignment to CAS and SQLite index
	if s.cas != nil && s.db != nil {
		data, err := json.Marshal(newAssignment)
		if err != nil {
			return nil, fmt.Errorf("marshal reassigned voice assignment: %w", err)
		}
		casObj, err := s.cas.Put(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("put reassigned voice assignment in CAS: %w", err)
		}
		newAssignment.CASHash = casObj.SHA256

		idx := storage.VoiceAssignmentIndex{
			ID:                 newAssignment.ID,
			AssetID:            newAssignment.AssetID,
			RunID:              newAssignment.RunID,
			JobID:              newAssignment.JobID,
			TargetLanguage:     newAssignment.TargetLanguage,
			CASHash:            newAssignment.CASHash,
			ProvenanceHash:     newAssignment.ProvenanceHash,
			AssignmentsJSON:    string(data),
			UseSameVoiceForAll: newAssignment.UseSameVoiceForAll,
			CreatedAt:          newAssignment.CreatedAt,
		}
		if err := s.db.SaveVoiceAssignmentIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("save reassigned voice assignment index: %w", err)
		}
	}

	return newAssignment, nil
}

// isSpeechIntervalCoveredBySuppression proves whether the target speech interval [startMs, endMs]
// is strictly and continuously covered by dialogue suppression intervals from AudioRolePlan.
func isSpeechIntervalCoveredBySuppression(speechStartMs, speechEndMs int64, segments []domain.AudioSegment) bool {
	if speechStartMs >= speechEndMs {
		return false
	}
	type interval struct {
		start int64
		end   int64
	}
	var intervals []interval
	for _, seg := range segments {
		if seg.Role == domain.AudioRoleNarrationDialogue {
			s := seg.StartMs
			e := seg.EndMs
			if s < speechStartMs {
				s = speechStartMs
			}
			if e > speechEndMs {
				e = speechEndMs
			}
			if s < e {
				intervals = append(intervals, interval{start: s, end: e})
			}
		}
	}
	if len(intervals) == 0 {
		return false
	}
	// Sort intervals by start time
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start == intervals[j].start {
			return intervals[i].end < intervals[j].end
		}
		return intervals[i].start < intervals[j].start
	})

	// Merge contiguous or overlapping intervals
	currStart := intervals[0].start
	currEnd := intervals[0].end
	if currStart > speechStartMs {
		return false
	}

	for i := 1; i < len(intervals); i++ {
		next := intervals[i]
		if next.start <= currEnd {
			if next.end > currEnd {
				currEnd = next.end
			}
		} else {
			// Gap detected
			return false
		}
	}

	return currStart <= speechStartMs && currEnd >= speechEndMs
}

// MaxAuditionAudioBytes bounds the audition audio artifact handed back to callers.
// Audition clips are seconds long (~10s of 48kHz stereo PCM16 is ~2MB), so a larger
// artifact is corrupt or hostile and must fail closed rather than being buffered and
// base64-encoded into an HTTP response.
const MaxAuditionAudioBytes int64 = 8 << 20

// AuditionVoice generates a short ~5s standalone or ~10s contextual audio clip to audition a voice profile.
// For contextual audition (IsContextual=true):
// - Uses actual translated segment text from DubScriptVariant.
// - Synthesizes candidate voice segment.
// - Fails closed if audio stems or background stem are missing/invalid or audio mixing fails.
// - Extracts a preview-local ~10s window (or expanded if speech exceeds ~10s) containing the entire clip without truncation.
// - Slices background stem and vocal stem to the preview window, offsets speech clips and suppression windows.
// - Preserves available source vocals/music-vocals outside dialogue windows while suppressing only dialogue.
// - Proves speech interval is strictly covered by dialogue suppression windows from AudioRolePlan (preventing collision).
// - Mixes synthesized speech with preserved stems and reports ContextualMixed=true.
func (s *DubbingService) AuditionVoice(ctx context.Context, in domain.VoiceAuditionInput) (*domain.VoiceAuditionResult, error) {
	if in.Voice.ID == "" {
		return nil, fmt.Errorf("voice profile is required")
	}

	targetLang := in.TargetLanguage
	if targetLang == "" {
		targetLang = in.Voice.Language
	}
	if targetLang == "" {
		targetLang = "vi"
	}

	// Check audio role plan and dub-eligibility (fail closed on missing/unreadable plan when asset_id is provided or contextual)
	var rolePlan *domain.AudioRolePlan
	if in.AssetID != "" {
		if s.db == nil {
			if in.IsContextual {
				return nil, fmt.Errorf("%w: missing database for contextual audition", domain.ErrAudioRolePlanRequired)
			}
		} else {
			resolveRunID := strings.TrimSpace(in.RunID)
			rp, err := ResolveRunScopedAudioRolePlan(ctx, s.db, s.cas, in.AssetID, resolveRunID)
			if err != nil {
				return nil, fmt.Errorf("%w: failed to get audio role plan for asset %s: %v", domain.ErrAudioRolePlanRequired, in.AssetID, err)
			}
			if rp == nil {
				return nil, fmt.Errorf("%w: audio role plan not found for asset %s", domain.ErrAudioRolePlanRequired, in.AssetID)
			}
			if !domain.IsDubEligible(rp) {
				return nil, domain.ErrNoDubbingRequired
			}
			rolePlan = rp
		}
	} else if in.IsContextual {
		return nil, fmt.Errorf("%w: asset_id is required for contextual audition", domain.ErrAudioRolePlanRequired)
	}

	var sampleText string
	var startMs int64
	var sourceEndMs int64
	var slotEndMs int64
	var slotDurationMs int64 = 5000
	// For contextual audition (IsContextual=true):
	// 1) Fail closed if storage/DB/asset is missing.
	// 2) Require actual translated segment from DubScriptVariant (or fail closed if missing/unresolvable).
	// 3) Require AudioRolePlan to prove source dialogue suppression.
	if in.IsContextual {
		if s.cas == nil || s.db == nil || strings.TrimSpace(in.AssetID) == "" {
			return nil, fmt.Errorf("%w: missing required storage/database for contextual audition", domain.ErrSoundtrackPreservationFailed)
		}
		if strings.TrimSpace(in.RunID) == "" {
			return nil, errors.New("run_id is required for contextual audition")
		}
		if rolePlan == nil {
			return nil, fmt.Errorf("%w: audio role plan required to prove dialogue suppression", domain.ErrAudioRolePlanRequired)
		}
		// Contextual audition is run-scoped: never substitute an asset-latest script
		// from another run just because the requested run is missing its own binding.
		runID := strings.TrimSpace(in.RunID)
		dIdx, loadErr := s.db.GetDubScriptVariantIndexByRun(ctx, runID)
		if loadErr != nil || dIdx == nil || dIdx.CASHash == "" {
			return nil, fmt.Errorf("%w: dub script variant not found for run %s", domain.ErrDubScriptVariantNotFound, runID)
		}
		if dIdx.AssetID != in.AssetID || !strings.EqualFold(dIdx.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("dub script variant run binding mismatch for run %s", runID)
		}
		rc, err := s.cas.Get(dIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("%w: read dub script from CAS: %v", domain.ErrDubScriptVariantNotFound, err)
		}
		defer rc.Close()
		var dubScript domain.DubScriptVariant
		if err := json.NewDecoder(rc).Decode(&dubScript); err != nil || len(dubScript.Segments) == 0 {
			return nil, fmt.Errorf("%w: dub script has no translated segments", domain.ErrDubScriptVariantNotFound)
		}
		if dubScript.SchemaVersion != domain.DubScriptSchemaVersion || dubScript.AssetID != in.AssetID || !strings.EqualFold(dubScript.TargetLanguage, targetLang) {
			return nil, fmt.Errorf("%w: dub script does not satisfy current audition contract", domain.ErrDubScriptVariantNotFound)
		}
		var matchedSeg *domain.DubScriptSegment
		for i := range dubScript.Segments {
			if dubScript.Segments[i].Index == in.SegmentIndex {
				matchedSeg = &dubScript.Segments[i]
				break
			}
		}
		if matchedSeg == nil {
			return nil, fmt.Errorf("%w: segment index %d not found in dub script (total segments: %d)", domain.ErrDubScriptVariantNotFound, in.SegmentIndex, len(dubScript.Segments))
		}
		seg := *matchedSeg
		if seg.SpokenText != "" {
			sampleText = seg.SpokenText
		} else if seg.MeaningText != "" {
			sampleText = seg.MeaningText
		}
		if strings.TrimSpace(sampleText) == "" {
			return nil, fmt.Errorf("%w: segment %d has empty translation text", domain.ErrDubScriptVariantNotFound, in.SegmentIndex)
		}
		if seg.StartMs < 0 || seg.EndMs <= seg.StartMs || seg.SlotDurationMs < 0 {
			return nil, fmt.Errorf("%w: invalid segment slot bounds (start=%d, end=%d, slot_duration=%d)", domain.ErrSoundtrackPreservationFailed, seg.StartMs, seg.EndMs, seg.SlotDurationMs)
		}
		canonicalSlotDur := seg.EndMs - seg.StartMs
		if seg.SlotDurationMs > 0 && seg.SlotDurationMs != canonicalSlotDur {
			return nil, fmt.Errorf("%w: inconsistent segment slot duration %d != end-start (%d-%d=%d)", domain.ErrSoundtrackPreservationFailed, seg.SlotDurationMs, seg.EndMs, seg.StartMs, canonicalSlotDur)
		}
		startMs = seg.StartMs
		sourceEndMs = seg.EndMs
		slotEndMs = sourceEndMs
		slotDurationMs = canonicalSlotDur

		// Borrowing is optional and evidence-gated. When this run pins a transcript,
		// contextual audition uses the same canonical next-vocal boundary and frozen
		// FitController policy as full synthesis. Without that run proof it simply
		// falls back to the immutable source end; it never consults asset-latest state.
		var transcriptCAS string
		if resolvedCAS, stageErr := resolveRunTranscriptCAS(ctx, s.db, runID, in.AssetID, "contextual audition"); stageErr == nil && resolvedCAS != "" {
			transcriptCAS = resolvedCAS
		} else if stageErr != nil {
			return nil, stageErr
		}
		if transcriptCAS != "" {
			if rolePlan.CASHash == "" {
				return nil, errors.New("contextual audition requires a CAS-pinned audio role plan")
			}
			transcript, pinnedRolePlan, loadErr := s.loadPlaybackTimeline(ctx, domain.DubbingJobInput{
				RunID: runID, AssetID: in.AssetID, TargetLanguage: targetLang,
				TranscriptArtifactCAS: transcriptCAS, AudioRolePlanCAS: rolePlan.CASHash,
			})
			if loadErr != nil {
				return nil, loadErr
			}
			nextBoundary, boundaryErr := playbackBoundaryForBlock(seg.Index, seg.StartMs, seg.EndMs, transcript, pinnedRolePlan)
			if boundaryErr != nil {
				return nil, boundaryErr
			}
			fc := s.fitController
			if fc == nil {
				fc = NewFitController()
			}
			playbackEndMs, _, policyID := fc.ResolvePlaybackWindow(sourceEndMs, nextBoundary)
			if policyID != "" && playbackEndMs >= sourceEndMs {
				slotEndMs = playbackEndMs
				slotDurationMs = slotEndMs - startMs
			}
		}
	} else {
		sampleText = in.SampleText
		if sampleText == "" {
			if targetLang == "vi" {
				sampleText = "Xin chào, đây là bản thử giọng mẫu tự nhiên cho video của bạn."
			} else {
				sampleText = "Hello, this is a sample voice audition for your video."
			}
		}
	}
	req := provider.TTSSynthesisRequest{
		RunID:          in.RunID,
		AssetID:        in.AssetID,
		SegmentIndex:   in.SegmentIndex,
		Text:           sampleText,
		Language:       targetLang,
		Voice:          in.Voice,
		Speed:          1.0,
		SlotDurationMs: slotDurationMs,
		UsableSlotMs:   slotDurationMs,
		AttemptNumber:  1,
	}

	synthRes, _, err := s.invokeTTSWithFallback(ctx, req, domain.ExecutionProfileHybrid, nil)
	if err != nil {
		return nil, fmt.Errorf("synthesize audition voice: %w", err)
	}

	finalAudioData := synthRes.AudioData
	contextualMixed := false

	// If contextual audition, mix with preserved background and vocal stems
	if in.IsContextual {
		if s.cas == nil || s.db == nil || in.AssetID == "" {
			return nil, fmt.Errorf("%w: missing required storage/database for contextual audition", domain.ErrSoundtrackPreservationFailed)
		}

		stemsIdx, err := s.db.GetAudioStemsArtifactIndex(ctx, in.AssetID)
		if err != nil || stemsIdx == nil || stemsIdx.CASHash == "" {
			return nil, fmt.Errorf("%w: stems artifact index not found for asset %s", domain.ErrAudioStemsNotFound, in.AssetID)
		}
		if stemsIdx.AssetID != in.AssetID {
			return nil, fmt.Errorf("%w: stems index belongs to asset %s, expected %s", domain.ErrAudioStemsNotFound, stemsIdx.AssetID, in.AssetID)
		}

		r, err := s.cas.Get(stemsIdx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("%w: read stems artifact from CAS: %v", domain.ErrAudioStemsNotFound, err)
		}
		defer r.Close()

		var stemArtifacts domain.AudioStemArtifacts
		if err := json.NewDecoder(r).Decode(&stemArtifacts); err != nil {
			return nil, fmt.Errorf("%w: decode stems artifact: %v", domain.ErrSoundtrackPreservationFailed, err)
		}
		if stemArtifacts.SchemaVersion != domain.AudioStemsSchemaVersion || stemArtifacts.AssetID != in.AssetID {
			return nil, fmt.Errorf("%w: stems artifact is incompatible with current asset/schema", domain.ErrSoundtrackPreservationFailed)
		}

		var bgStem, vocalsStem domain.AudioStem
		for _, st := range stemArtifacts.Stems {
			if st.Type == domain.StemTypeBackground {
				bgStem = st
			} else if st.Type == domain.StemTypeVocals {
				vocalsStem = st
			}
		}

		if bgStem.AudioCASHash == "" {
			return nil, fmt.Errorf("%w: missing background stem in stems artifact", domain.ErrSoundtrackPreservationFailed)
		}

		bgR, err := s.cas.Get(bgStem.AudioCASHash)
		if err != nil {
			return nil, fmt.Errorf("%w: read background stem from CAS: %v", domain.ErrSoundtrackPreservationFailed, err)
		}
		defer bgR.Close()

		bgBytes, err := io.ReadAll(bgR)
		if err != nil {
			return nil, fmt.Errorf("%w: read background stem bytes: %v", domain.ErrSoundtrackPreservationFailed, err)
		}

		bgSamples, bgHeader, err := media.ExtractPCM16Samples(bgBytes)
		if err != nil || bgHeader == nil {
			return nil, fmt.Errorf("%w: extract background PCM16 samples: %v", domain.ErrSoundtrackPreservationFailed, err)
		}

		sampleRate := int(bgHeader.SampleRate)
		channels := int(bgHeader.NumChannels)
		if sampleRate <= 0 || channels <= 0 {
			return nil, fmt.Errorf("%w: invalid background stem format (rate=%d, ch=%d)", domain.ErrSoundtrackPreservationFailed, sampleRate, channels)
		}

		// Read and resample vocal stem if declared in artifact (fail closed on CAS/load/decode/format error)
		var vocalsSamples []int16
		if vocalsStem.AudioCASHash != "" {
			vR, err := s.cas.Get(vocalsStem.AudioCASHash)
			if err != nil {
				return nil, fmt.Errorf("%w: read vocals stem from CAS: %v", domain.ErrSoundtrackPreservationFailed, err)
			}
			defer vR.Close()

			vBytes, err := io.ReadAll(vR)
			if err != nil {
				return nil, fmt.Errorf("%w: read vocals stem bytes: %v", domain.ErrSoundtrackPreservationFailed, err)
			}

			vSamp, vHeader, err := media.ExtractPCM16Samples(vBytes)
			if err != nil || vHeader == nil {
				return nil, fmt.Errorf("%w: extract vocals PCM16 samples: %v", domain.ErrSoundtrackPreservationFailed, err)
			}

			vRate := int(vHeader.SampleRate)
			vCh := int(vHeader.NumChannels)
			if vRate <= 0 || vCh <= 0 {
				return nil, fmt.Errorf("%w: invalid vocals stem format (rate=%d, ch=%d)", domain.ErrSoundtrackPreservationFailed, vRate, vCh)
			}

			if vRate != sampleRate || vCh != channels {
				vSamp = media.ResamplePCM16(vSamp, vRate, vCh, sampleRate, channels)
			}
			vocalsSamples = vSamp
		}

		// Extract synthesized speech samples
		speechSamples, spkHeader, err := media.ExtractPCM16Samples(synthRes.AudioData)
		if err != nil || spkHeader == nil {
			return nil, fmt.Errorf("%w: extract synthesized speech samples: %v", domain.ErrSoundtrackPreservationFailed, err)
		}

		// Determine speech duration and source bounds
		speechFrames := int64(len(speechSamples) / int(spkHeader.NumChannels))
		speechDurMs := (speechFrames * 1000) / int64(spkHeader.SampleRate)
		if speechDurMs <= 0 {
			speechDurMs = slotDurationMs
		}
		speechEndMs := startMs + speechDurMs

		totalBgFrames := int64(len(bgSamples) / channels)
		totalBgDurMs := (totalBgFrames * 1000) / int64(sampleRate)

		// Fail closed if speech exceeds source bounds
		if startMs < 0 || speechDurMs <= 0 || startMs >= totalBgDurMs || speechEndMs > totalBgDurMs {
			return nil, fmt.Errorf("%w: synthesized speech interval [%d, %d]ms exceeds source audio duration %dms", domain.ErrSoundtrackPreservationFailed, startMs, speechEndMs, totalBgDurMs)
		}

		// Fail closed if speech overruns the accepted playback window. Source anchors
		// remain immutable; only proven post-speech silence may extend this ceiling.
		if speechEndMs > slotEndMs {
			return nil, fmt.Errorf("%w: synthesized speech interval [%d, %d]ms overruns accepted playback window [%d, %d]ms (duration %dms > slot %dms)", domain.ErrTTSDurationOverrun, startMs, speechEndMs, startMs, slotEndMs, speechDurMs, slotDurationMs)
		}
		// Only the immutable source speech window is suppressed. Any accepted tail
		// borrowing remains over preserved background/ambience, matching the final mixer.
		suppressionEndMs := speechEndMs
		if suppressionEndMs > sourceEndMs {
			suppressionEndMs = sourceEndMs
		}
		if !isSpeechIntervalCoveredBySuppression(startMs, suppressionEndMs, rolePlan.Segments) {
			return nil, fmt.Errorf("%w: source speech interval [%d, %d]ms is not fully covered by verified dialogue suppression intervals in audio role plan", domain.ErrSoundtrackPreservationFailed, startMs, suppressionEndMs)
		}

		// Determine preview-local window containing the synthesized speech segment without truncation.
		// Nominal preview window is ~10s; expanded beyond ~10s only when necessary to contain synthesized speech.
		const nominalPreviewDurMs int64 = 10000
		targetWinDurMs := nominalPreviewDurMs
		if speechDurMs > targetWinDurMs {
			targetWinDurMs = speechDurMs
		}
		if targetWinDurMs > totalBgDurMs {
			targetWinDurMs = totalBgDurMs
		}

		// Position window at startMs and clamp near source end
		var windowStartMs int64 = startMs
		var windowEndMs int64 = windowStartMs + targetWinDurMs
		if windowEndMs > totalBgDurMs {
			windowEndMs = totalBgDurMs
			windowStartMs = windowEndMs - targetWinDurMs
			if windowStartMs < 0 {
				windowStartMs = 0
			}
		}

		// Verify window strictly contains [startMs, speechEndMs] within source bounds
		if windowStartMs > startMs || windowEndMs < speechEndMs {
			return nil, fmt.Errorf("%w: preview window [%d, %d]ms cannot fully contain speech [%d, %d]ms", domain.ErrSoundtrackPreservationFailed, windowStartMs, windowEndMs, startMs, speechEndMs)
		}

		startFrame := (windowStartMs * int64(sampleRate)) / 1000
		endFrame := (windowEndMs * int64(sampleRate)) / 1000
		if startFrame < 0 {
			startFrame = 0
		}
		if endFrame > totalBgFrames {
			endFrame = totalBgFrames
		}

		speechOffsetMs := startMs - windowStartMs
		speechStartFrame := (speechOffsetMs * int64(sampleRate)) / 1000

		// Ensure frame count fits resampled speech to guarantee zero sample drop in MixPCM16Stems
		resampledSpeechSamples := speechSamples
		resampledRate := int(spkHeader.SampleRate)
		resampledCh := int(spkHeader.NumChannels)
		if resampledRate != sampleRate || resampledCh != channels {
			resampledSpeechSamples = media.ResamplePCM16(speechSamples, resampledRate, resampledCh, sampleRate, channels)
			resampledRate = sampleRate
			resampledCh = channels
		}
		resampledSpeechFrames := int64(len(resampledSpeechSamples) / channels)

		if speechStartFrame+resampledSpeechFrames > (endFrame - startFrame) {
			neededEndFrame := startFrame + speechStartFrame + resampledSpeechFrames
			if neededEndFrame <= totalBgFrames {
				endFrame = neededEndFrame
				windowEndMs = (endFrame * 1000) / int64(sampleRate)
			} else {
				return nil, fmt.Errorf("%w: speech samples extend beyond total source audio frames", domain.ErrSoundtrackPreservationFailed)
			}
		}

		if startFrame >= endFrame {
			return nil, fmt.Errorf("%w: invalid preview window [%d, %d]ms", domain.ErrSoundtrackPreservationFailed, windowStartMs, windowEndMs)
		}

		slicedBgSamples := bgSamples[startFrame*int64(channels) : endFrame*int64(channels)]

		// Slice vocal samples to preview window if available
		var slicedVocalsSamples []int16
		if len(vocalsSamples) > 0 {
			totalVocalsFrames := int64(len(vocalsSamples) / channels)
			vStartFrame := startFrame
			vEndFrame := endFrame
			if vStartFrame < 0 {
				vStartFrame = 0
			}
			if vEndFrame > totalVocalsFrames {
				vEndFrame = totalVocalsFrames
			}
			if vStartFrame < vEndFrame {
				slicedVocalsSamples = vocalsSamples[vStartFrame*int64(channels) : vEndFrame*int64(channels)]
			}
		}

		// Speech clip with preview-local offset
		speechClips := []media.DubSpeechClip{
			{
				StartMs:    speechOffsetMs,
				SampleRate: int(spkHeader.SampleRate),
				Channels:   int(spkHeader.NumChannels),
				Samples:    speechSamples,
			},
		}

		// Build preview-local suppression windows for narration dialogue from verified AudioRolePlan
		var suppressWindows []media.PreservationWindowInterval
		for _, rSeg := range rolePlan.Segments {
			if rSeg.Role == domain.AudioRoleNarrationDialogue {
				segStart := rSeg.StartMs
				segEnd := rSeg.EndMs
				if segEnd > windowStartMs && segStart < windowEndMs {
					localStart := segStart - windowStartMs
					localEnd := segEnd - windowStartMs
					if localStart < 0 {
						localStart = 0
					}
					previewDur := windowEndMs - windowStartMs
					if localEnd > previewDur {
						localEnd = previewDur
					}
					if localStart < localEnd {
						suppressWindows = append(suppressWindows, media.PreservationWindowInterval{
							StartMs: localStart,
							EndMs:   localEnd,
							Action:  "suppress_dialogue",
						})
					}
				}
			}
		}

		mixedSamples := media.MixPCM16Stems(
			slicedBgSamples,
			slicedVocalsSamples,
			sampleRate,
			channels,
			speechClips,
			suppressWindows,
			25,   // 25ms crossfade
			-2.0, // gentle ducking
		)
		if len(mixedSamples) == 0 {
			return nil, fmt.Errorf("%w: stem mixing produced 0 samples", domain.ErrSoundtrackPreservationFailed)
		}

		finalAudioData = media.EncodePCM16Samples(mixedSamples, sampleRate, channels)
		contextualMixed = true
	}

	// Probe duration from final audio (fail closed)
	probedMs, err := media.ProbeWAVBytes(finalAudioData)
	if err != nil {
		return nil, fmt.Errorf("probe audition audio duration: %w", err)
	}
	if probedMs <= 0 {
		return nil, fmt.Errorf("invalid probed duration %dms for audition audio", probedMs)
	}
	if s.cas == nil {
		return nil, fmt.Errorf("CAS store is required to persist audition audio")
	}
	// Oversize artifacts fail closed before they reach CAS or a transport that
	// would buffer and base64-encode them unbounded.
	if int64(len(finalAudioData)) > MaxAuditionAudioBytes {
		return nil, fmt.Errorf("audition audio artifact is %d bytes and exceeds the %d byte limit", len(finalAudioData), MaxAuditionAudioBytes)
	}
	casObj, err := s.cas.Put(bytes.NewReader(finalAudioData))
	if err != nil {
		return nil, fmt.Errorf("put audition audio in CAS: %w", err)
	}
	casHash, casPath := casObj.SHA256, casObj.Path

	var providerID, modelName, modelVersion string
	if synthRes != nil {
		providerID = synthRes.ProviderID
		modelName = synthRes.ModelName
		modelVersion = synthRes.ModelVersion
	}
	if providerID == "" && in.Voice.ProviderID != "" {
		providerID = in.Voice.ProviderID
	}

	return &domain.VoiceAuditionResult{
		Voice:              in.Voice,
		AudioCASHash:       casHash,
		AudioCASPath:       casPath,
		AudioBytes:         finalAudioData,
		MeasuredDurationMs: probedMs,
		IsContextual:       in.IsContextual,
		ContextualMixed:    contextualMixed,
		SampleText:         sampleText,
		ProviderID:         providerID,
		ModelName:          modelName,
		ModelVersion:       modelVersion,
	}, nil
}

// SynthesizeAndFit synthesizes speech for all DubScript segments, probes actual synthesized duration,
// and runs the measured-duration fit controller (ACCEPT | RESYNTH | REWRITE | REGROUP | REVIEW).
func (s *DubbingService) SynthesizeAndFit(ctx context.Context, in domain.DubbingJobInput) (*domain.DubSegmentsVariant, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, fmt.Errorf("asset_id is required")
	}

	targetLang := strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	if targetLang != "vi" && targetLang != "en" {
		return nil, fmt.Errorf("unsupported target language '%s': must be 'vi' or 'en'", in.TargetLanguage)
	}
	in.TargetLanguage = targetLang

	// Fail closed if AudioRolePlan is missing, unreadable, or contains no dub-eligible dialogue.
	if s.db == nil || strings.TrimSpace(in.AssetID) == "" {
		return nil, domain.ErrAudioRolePlanRequired
	}
	rolePlan, err := ResolveRunScopedAudioRolePlan(ctx, s.db, s.cas, in.AssetID, in.RunID)
	if err != nil {
		return nil, fmt.Errorf("get audio role plan: %w", err)
	}
	if rolePlan == nil {
		return nil, domain.ErrAudioRolePlanRequired
	}
	if !domain.IsDubEligible(rolePlan) {
		return nil, domain.ErrNoDubbingRequired
	}
	if rolePlan.CASHash == "" {
		return nil, fmt.Errorf("audio role plan for asset %s is not pinned to CAS", in.AssetID)
	}
	in.AudioRolePlanCAS = rolePlan.CASHash

	// 1. Load the exact dub script and voice assignment before resolving any omitted
	// transcript reference. Their persisted lineage is an allowed current-run proof;
	// asset-latest state is not.
	dubScript, dubScriptCAS, err := s.loadDubScriptVariant(ctx, in.AssetID, in.RunID, targetLang, in.DubScriptVariantCAS)
	if err != nil {
		return nil, fmt.Errorf("load dub script for synthesis: %w", err)
	}
	in.DubScriptVariantCAS = dubScriptCAS
	voiceAssign, voiceAssignCAS, err := s.loadVoiceAssignment(ctx, in.AssetID, in.RunID, targetLang, in.VoiceAssignmentCAS)
	if err != nil {
		return nil, fmt.Errorf("load voice assignment for synthesis: %w", err)
	}
	in.VoiceAssignmentCAS = voiceAssignCAS
	if strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
		casHash, err := resolveRunTranscriptCAS(ctx, s.db, in.RunID, in.AssetID, "playback-window derivation")
		if err != nil {
			return nil, err
		}
		in.TranscriptArtifactCAS = casHash
	}
	if strings.TrimSpace(in.TranscriptArtifactCAS) == "" && strings.TrimSpace(voiceAssign.TranscriptArtifactCAS) != "" {
		in.TranscriptArtifactCAS = voiceAssign.TranscriptArtifactCAS
	}
	if strings.TrimSpace(in.TranscriptArtifactCAS) == "" && strings.TrimSpace(dubScript.TranslationVariantCAS) != "" {
		transcriptCAS, err := s.transcriptCASFromTranslationVariant(in.AssetID, targetLang, dubScript.TranslationVariantCAS)
		if err != nil {
			return nil, err
		}
		in.TranscriptArtifactCAS = transcriptCAS
	}
	if strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
		return nil, fmt.Errorf("pinned transcript artifact is required for playback-window derivation")
	}
	if voiceAssign.DubScriptVariantCAS == "" || voiceAssign.DubScriptVariantCAS != dubScriptCAS {
		return nil, fmt.Errorf("voice assignment dub script lineage mismatch: voice=%q dub=%q", voiceAssign.DubScriptVariantCAS, dubScriptCAS)
	}
	if voiceAssign.TranscriptArtifactCAS == "" || voiceAssign.TranscriptArtifactCAS != in.TranscriptArtifactCAS {
		return nil, fmt.Errorf("voice assignment transcript lineage mismatch: voice=%q transcript=%q", voiceAssign.TranscriptArtifactCAS, in.TranscriptArtifactCAS)
	}

	recoveryState := newLineageRecoveryState()

	// commit publishes a pass: CAS derives its identity from the variant's own canonical
	// bytes, and the run-scoped index moves to that artifact while the pass still belongs to the
	// run's current voice assignment. A variant that already carries its committed identity came
	// from the idempotent synthesis cache, so it is the artifact a previous call published and is
	// returned untouched.
	publish := func(variant *domain.DubSegmentsVariant) (*domain.DubSegmentsVariant, error) {
		if variant.CASHash != "" {
			return variant, nil
		}
		commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancelCommit()
		if err := s.commitDubSegmentsVariant(commitCtx, variant); err != nil {
			return nil, err
		}
		return variant, nil
	}

	// Issue #156: the pass that ends the configured remedy sequence derives the review-only
	// atempo alternative for each unresolved unit before those bytes become the published
	// artifact. Committing is cancellation-independent because cancellation is recorded as
	// TEMPO_CANCELLED evidence - a bookkeeping write the operator must be able to read - while
	// run control still stops the run through the caller's own cancellation check.
	finalize := func(variant *domain.DubSegmentsVariant) (*domain.DubSegmentsVariant, error) {
		// A committed variant came from the idempotent synthesis cache, so it already carries the
		// work this call would redo - with two exceptions, both re-derived from the cached pass's
		// own retained waveform without re-running TTS. One is evidence a transient transform
		// failure recorded about the run that was interrupted, which is not a verdict on the
		// pass. The other is a pass published before an escalation a later attempt could not
		// repeat (the fallback lane is no longer route-eligible, or the escalation was refused):
		// it is not final, so an eligible unresolved overrun it still carries must gain its
		// tempo evidence here instead of being returned as the run's canonical artifact.
		if variant.CASHash != "" {
			if !resetRetryableTempoEvidence(variant) && tempoEvidenceComplete(variant) {
				return variant, nil
			}
			variant.CASHash = ""
		}
		s.attachTempoCandidates(ctx, variant)
		published, err := publish(variant)
		if err != nil {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return published, ctxErr
		}
		return published, nil
	}

	pass, err := s.synthesizeSegmentsPass(ctx, in, dubScript, dubScriptCAS, voiceAssign, voiceAssignCAS, nil, nil, recoveryState)
	if err != nil {
		return nil, err
	}

	// Issue #94: a speaker whose accepted playback window is still overrun by a
	// fixed-rate preset lane (ZeroTTS) after the bounded rewrite/regroup remedies
	// are exhausted escalates once, at whole-speaker scope, to the duration-controlled
	// fallback lane (CosyVoice3). The escalation supersedes that speaker's frozen
	// assignment and regenerates only its descendants; whatever is still unresolved
	// after the escalated regeneration projects REVIEW instead of hopping again.
	plan := s.planSpeakerEscalation(ctx, in, voiceAssign, pass)
	if plan == nil {
		// Issue #156: no escalation applies, so this pass already exhausted the natural
		// remedies and each unresolved unit may carry exactly one atempo candidate.
		return finalize(pass)
	}
	// The pre-escalation pass is published before the escalation that supersedes it - and
	// before any work that can fail - so a retried synthesis reads it from the idempotent
	// cache instead of re-running TTS for it, and a failed escalation cannot lose a pass that
	// already completed. Its remedy sequence is not exhausted, so it carries no atempo
	// candidate; only the final pass does.
	//
	// A pass that already belongs to a superseded assignment is committed without claiming the
	// run's dub-variant row (#156): the row keeps resolving to the current assignment's pass, the
	// one that carries this issue's tempo evidence, instead of being taken over by superseded
	// audio. The ownership test is the claim statement itself, so a reassignment that lands while
	// this pass is being published - after the request decided to escalate and before the
	// escalation decision below - refuses the claim rather than being missed by a stale pre-check.
	//
	// ponytail: the superseded artifact is in CAS but not in the index, so the row-based idempotent
	// cache no longer short-circuits a repeat of that stale call. Repeating superseded work is rare
	// and cheap next to canonicalizing superseded audio; index it under a non-canonical key if that
	// retry ever shows up in cost data.
	if _, err := publish(pass); err != nil {
		return nil, err
	}
	superseding, err := s.applySpeakerEscalation(ctx, in, voiceAssign, plan)
	if err != nil {
		return nil, err
	}
	if superseding == nil {
		// The run moved on to a newer assignment that is not this escalation, so the completed
		// pass stays committed and readable, is returned as REVIEW evidence, and never claimed the
		// run's dub-variant row - that row keeps resolving to the current assignment's own pass.
		// Nothing here touches the operator's newer assignment (#155: a newer assignment is never
		// reverted, so no escalation is minted from this stale base).
		//
		// Issue #156 derives no atempo candidate on this path on purpose. A candidate can only be
		// attached to this pass, and attaching it would publish superseded-assignment audio as the
		// run's canonical media and graft cross-assignment tempo evidence onto it. The pass whose
		// remedy sequence ends with tempo evidence is the run's current assignment's pass, and that
		// assignment's own request synthesizes it (ReassignVoice and the inspector corrections mint
		// the superseding assignment and synthesize it in the same request; a call whose base IS the
		// current assignment can never reach this branch). The unresolved overrun therefore stays
		// REVIEW here and gains its evidence there, from its own retained waveform.
		return pass, nil
	}
	escalated, err := s.synthesizeSegmentsPass(ctx, in, dubScript, dubScriptCAS, superseding, superseding.CASHash, pass, plan.evidence, recoveryState)
	if err != nil {
		return nil, err
	}
	// Issue #156: the escalated regeneration is the end of the configured remedy
	// sequence, so only this final pass may derive review-only atempo candidates.
	return finalize(escalated)
}

// CanReuseVariant proves that a persisted dub artifact belongs to the exact
// current playback/voice/script lineage and fit-policy identity. It is read-only.
func (s *DubbingService) CanReuseVariant(ctx context.Context, in domain.DubbingJobInput, variant *domain.DubSegmentsVariant) bool {
	if variant == nil || variant.SchemaVersion != domain.DubSegmentsSchemaVersion || s.db == nil || s.cas == nil {
		return false
	}
	in.TargetLanguage = strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	rolePlan, err := ResolveRunScopedAudioRolePlan(ctx, s.db, s.cas, in.AssetID, in.RunID)
	if err != nil || rolePlan == nil || rolePlan.CASHash == "" {
		return false
	}
	if in.AudioRolePlanCAS == "" {
		in.AudioRolePlanCAS = rolePlan.CASHash
	}
	if in.TranscriptArtifactCAS == "" {
		if idx, err := s.db.GetTranscriptArtifactIndexByRun(ctx, in.RunID); err == nil && idx != nil {
			in.TranscriptArtifactCAS = idx.CASHash
		}
	}
	if in.TranscriptArtifactCAS == "" || variant.AssetID != in.AssetID || variant.RunID != in.RunID ||
		!strings.EqualFold(variant.TargetLanguage, in.TargetLanguage) || variant.DubScriptVariantCAS != in.DubScriptVariantCAS ||
		variant.VoiceAssignmentCAS != in.VoiceAssignmentCAS || variant.TranscriptArtifactCAS != in.TranscriptArtifactCAS ||
		variant.AudioRolePlanCAS != in.AudioRolePlanCAS {
		return false
	}
	fc := s.fitController
	if fc == nil {
		fc = NewFitController()
	}
	if variant.FitPolicyID == "" || variant.FitPolicyID != fc.policyID() {
		return false
	}
	dubScript, dubCAS, err := s.loadDubScriptVariant(ctx, in.AssetID, in.RunID, in.TargetLanguage, in.DubScriptVariantCAS)
	if err != nil || dubCAS != in.DubScriptVariantCAS {
		return false
	}
	voiceAssign, voiceCAS, err := s.loadVoiceAssignment(ctx, in.AssetID, in.RunID, in.TargetLanguage, in.VoiceAssignmentCAS)
	if err != nil || voiceCAS != in.VoiceAssignmentCAS {
		return false
	}
	expected, err := s.computeDubSegmentsProvenanceHash(in, dubScript, voiceAssign)
	return err == nil && variant.ProvenanceHash != "" && variant.ProvenanceHash == expected
}

func (s *DubbingService) loadPlaybackTimeline(ctx context.Context, in domain.DubbingJobInput) (*domain.TranscriptArtifact, *domain.AudioRolePlan, error) {
	if s.cas == nil || s.db == nil {
		return nil, nil, errors.New("database and CAS are required for playback-window derivation")
	}
	rc, err := s.cas.Get(in.TranscriptArtifactCAS)
	if err != nil {
		return nil, nil, fmt.Errorf("read pinned transcript artifact %s: %w", in.TranscriptArtifactCAS, err)
	}
	defer rc.Close()
	var transcript domain.TranscriptArtifact
	if err := json.NewDecoder(rc).Decode(&transcript); err != nil {
		return nil, nil, fmt.Errorf("decode pinned transcript artifact %s: %w", in.TranscriptArtifactCAS, err)
	}
	if transcript.AssetID != "" && transcript.AssetID != in.AssetID {
		return nil, nil, fmt.Errorf("pinned transcript asset mismatch: %s != %s", transcript.AssetID, in.AssetID)
	}
	rolePlan, err := ResolveRunScopedAudioRolePlan(ctx, s.db, s.cas, in.AssetID, in.RunID)
	if err != nil || rolePlan == nil {
		return nil, nil, domain.ErrAudioRolePlanRequired
	}
	if in.AudioRolePlanCAS == "" || rolePlan.CASHash == "" || rolePlan.CASHash != in.AudioRolePlanCAS {
		return nil, nil, fmt.Errorf("audio role plan lineage mismatch: pinned=%q current=%q", in.AudioRolePlanCAS, rolePlan.CASHash)
	}
	if err := verifyPinnedAudioRolePlan(s.cas, rolePlan, in.AudioRolePlanCAS); err != nil {
		return nil, nil, err
	}
	return &transcript, rolePlan, nil
}

func verifyPinnedAudioRolePlan(store *cas.Store, current *domain.AudioRolePlan, casHash string) error {
	if store == nil || current == nil || strings.TrimSpace(casHash) == "" {
		return errors.New("pinned audio role plan artifact is required")
	}
	r, err := store.Get(casHash)
	if err != nil {
		return fmt.Errorf("read pinned audio role plan %s: %w", casHash, err)
	}
	defer r.Close()
	var pinned domain.AudioRolePlan
	if err := json.NewDecoder(r).Decode(&pinned); err != nil {
		return fmt.Errorf("decode pinned audio role plan %s: %w", casHash, err)
	}
	if pinned.AssetID != current.AssetID {
		return fmt.Errorf("pinned audio role plan asset mismatch: %s != %s", pinned.AssetID, current.AssetID)
	}
	pinnedSegments, _ := json.Marshal(pinned.Segments)
	currentSegments, _ := json.Marshal(current.Segments)
	if !bytes.Equal(pinnedSegments, currentSegments) {
		return errors.New("pinned audio role plan content does not match current canonical plan")
	}
	if current.ProvenanceHash != "" && pinned.ProvenanceHash != current.ProvenanceHash {
		return errors.New("pinned audio role plan provenance mismatch")
	}
	return nil
}

func (s *DubbingService) loadDubTranslationContract(dubScript *domain.DubScriptVariant) (*domain.TranslationVariant, error) {
	if s.cas == nil || dubScript == nil || strings.TrimSpace(dubScript.TranslationVariantCAS) == "" {
		return nil, errors.New("dub script must pin a translation variant for glossary/QA contract")
	}
	r, err := s.cas.Get(dubScript.TranslationVariantCAS)
	if err != nil {
		return nil, fmt.Errorf("read pinned translation variant %s: %w", dubScript.TranslationVariantCAS, err)
	}
	defer r.Close()
	var v domain.TranslationVariant
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode pinned translation variant %s: %w", dubScript.TranslationVariantCAS, err)
	}
	if v.SchemaVersion != domain.TranslationSchemaVersion || v.ContractID != TranslationContractID || v.AssetID != dubScript.AssetID || !strings.EqualFold(v.TargetLanguage, dubScript.TargetLanguage) {
		return nil, fmt.Errorf("pinned translation variant does not satisfy current translation contract")
	}
	return &v, nil
}

func glossaryTargetsPreserved(original, candidate string, terms []domain.GlossaryEntry) bool {
	lowerOriginal := strings.ToLower(original)
	lowerCandidate := strings.ToLower(candidate)
	for _, term := range terms {
		target := strings.ToLower(strings.TrimSpace(term.Target))
		if target != "" && strings.Contains(lowerOriginal, target) && !strings.Contains(lowerCandidate, target) {
			return false
		}
	}
	return true
}

func playbackBoundaryForBlock(blockIndex int, sourceStartMs, sourceEndMs int64, transcript *domain.TranscriptArtifact, rolePlan *domain.AudioRolePlan) (int64, error) {
	if sourceStartMs < 0 || sourceEndMs <= sourceStartMs || transcript == nil || rolePlan == nil {
		return 0, errors.New("invalid playback boundary inputs")
	}
	found := false
	next := int64(0)
	for _, b := range transcript.SpeechBlocks {
		// Silence/noise blocks are timeline evidence, not canonical speech members.
		// Silence blocks historically carry the zero-value Index, so checking Index
		// before SegmentType can alias them to speech block 0 and invent a timing mismatch.
		if !domain.IsSpeechBlock(b) {
			continue
		}
		if b.Index == blockIndex {
			if b.StartMs != sourceStartMs || b.EndMs != sourceEndMs {
				return 0, fmt.Errorf("speech block %d source timing mismatch: transcript=%d-%d script=%d-%d", blockIndex, b.StartMs, b.EndMs, sourceStartMs, sourceEndMs)
			}
			found = true
			continue
		}
		if b.StartMs < sourceEndMs && b.EndMs > sourceEndMs {
			return sourceEndMs, nil
		}
		if b.StartMs >= sourceEndMs && (next == 0 || b.StartMs < next) {
			next = b.StartMs
		}
	}
	if !found {
		return 0, fmt.Errorf("speech block %d is not present in pinned transcript", blockIndex)
	}
	for _, a := range rolePlan.Segments {
		if a.Role != domain.AudioRoleSingingMusicVocal && a.Role != domain.AudioRoleUncertain {
			continue
		}
		if a.StartMs < sourceEndMs && a.EndMs > sourceEndMs {
			return sourceEndMs, nil
		}
		if a.StartMs >= sourceEndMs && (next == 0 || a.StartMs < next) {
			next = a.StartMs
		}
	}
	return next, nil
}

// hasProtectedSoundtrackInGap reports whether any singing/music-vocal or uncertain audio segment
// exists within the gap interval (startMs, endMs). Regrouping across such a gap would merge dialogue
// across preserved soundtrack and overlay spoken audio on singing or unclassified audio (Issue #153).
func hasProtectedSoundtrackInGap(startMs, endMs int64, rolePlan *domain.AudioRolePlan) bool {
	if rolePlan == nil || endMs <= startMs {
		return false
	}
	for _, a := range rolePlan.Segments {
		if a.Role != domain.AudioRoleSingingMusicVocal && a.Role != domain.AudioRoleUncertain {
			continue
		}
		if a.StartMs < endMs && a.EndMs > startMs {
			return true
		}
	}
	return false
}

// Bounded same-speaker regrouping (Issue #155/R4). A group is the maximal contiguous
// eligible prefix that starts at the triggering canonical source block: same speaker,
// strictly positive source gaps below regroupMaxGapMs, no protected singing/uncertain
// vocal interval and no other canonical speech block in between, capped at
// regroupMaxMembers members. Membership and boundaries come from the pinned canonical
// SpeechBlocks/AudioRolePlan, not from filtered DubScript adjacency; ordinary
// BGM/SFX/ambience is not a grouping barrier.
const (
	regroupMaxMembers = 5
	regroupMaxGapMs   = 600
)

// boundedRegroupMembers returns the canonical members of that group in script order,
// beginning with the triggering segment at triggerPos; a single member means no group is
// available. The trigger must be the pinned canonical block itself - index, anchors and
// speaker identity included - because a group seeded from a drifted script anchor would
// merge turns the canonical timeline does not attribute to that trigger. The scan then
// stops at the first canonical speaker, protected-vocal, overlap, gap or
// non-adjacent-canonical-block boundary, so a group can never skip a source turn.
func boundedRegroupMembers(dubScript *domain.DubScriptVariant, transcript *domain.TranscriptArtifact, rolePlan *domain.AudioRolePlan, triggerPos int, triggerSpeaker string) []domain.DubScriptSegment {
	members := []domain.DubScriptSegment{dubScript.Segments[triggerPos]}
	if transcript == nil {
		return members
	}
	prev, ok := canonicalSpeechBlock(transcript, members[0].Index)
	if !ok || prev.StartMs != members[0].StartMs || prev.EndMs != members[0].EndMs ||
		canonicalSpeaker(prev) != triggerSpeaker {
		return members
	}
	// Same-speaker membership is grounded in the canonical speaker identity: the script's
	// own labelling may drift and never decides whether a turn belongs to the group.
	speakerID := canonicalSpeaker(prev)
	for pos := triggerPos + 1; pos < len(dubScript.Segments) && len(members) < regroupMaxMembers; pos++ {
		next := dubScript.Segments[pos]
		// The script turn must be the same canonical block: a divergent or dropped block
		// is a boundary, not something to merge across.
		nextBlock, found := canonicalSpeechBlock(transcript, next.Index)
		if !found || nextBlock.StartMs != next.StartMs || nextBlock.EndMs != next.EndMs ||
			canonicalSpeaker(nextBlock) != speakerID {
			break
		}
		gapMs := nextBlock.StartMs - prev.EndMs
		if gapMs <= 0 || gapMs >= regroupMaxGapMs {
			break
		}
		if hasProtectedSoundtrackInGap(prev.EndMs, nextBlock.StartMs, rolePlan) ||
			!canonicalGapIsEmpty(transcript, prev, nextBlock) {
			break
		}
		members = append(members, next)
		prev = nextBlock
	}
	return members
}

// boundedRegroupPlan selects that group for a triggering segment and merges its text:
// member anchors and source text stay canonical, an accepted member rewrite travels with
// its member, and the text the group was chosen with is what gets synthesized.
func boundedRegroupPlan(dubScript *domain.DubScriptVariant, transcript *domain.TranscriptArtifact, rolePlan *domain.AudioRolePlan, triggerPos int, triggerSpeaker string, recoveryState *lineageRecoveryState) regroupPlan {
	members := boundedRegroupMembers(dubScript, transcript, rolePlan, triggerPos, triggerSpeaker)
	plan := regroupPlan{Members: members, MemberIndices: make([]int, 0, len(members))}
	sourceParts := make([]string, 0, len(members))
	spokenParts := make([]string, 0, len(members))
	for _, member := range members {
		plan.MemberIndices = append(plan.MemberIndices, member.Index)
		sourceParts = append(sourceParts, member.SourceText)
		memberText := member.SpokenText
		if memberText == "" {
			memberText = member.MeaningText
		}
		if rewritten, ok := recoveryState.AcceptedRewrites[member.Index]; ok && rewritten != "" {
			memberText = rewritten
		}
		spokenParts = append(spokenParts, memberText)
	}
	plan.SourceText = strings.Join(sourceParts, " ")
	plan.SpokenText = strings.Join(spokenParts, " ")
	return plan
}

// carriedRegroupPlan resolves the group a lineage already chose against the current script.
// The recorded members must still sit at the same contiguous script positions, because the
// recorded topology is never re-selected: anything else leaves fewer than two members, so
// the spent remedy stays spent without a group.
func carriedRegroupPlan(dubScript *domain.DubScriptVariant, triggerPos int, chosen chosenRegroup) regroupPlan {
	plan := regroupPlan{MemberIndices: chosen.MemberIndices, SourceText: chosen.SourceText, SpokenText: chosen.SpokenText}
	for offset, index := range chosen.MemberIndices {
		pos := triggerPos + offset
		if pos >= len(dubScript.Segments) || dubScript.Segments[pos].Index != index {
			plan.Members = nil
			break
		}
		plan.Members = append(plan.Members, dubScript.Segments[pos])
	}
	return plan
}

// canonicalSpeechBlock resolves one canonical speech block by index.
func canonicalSpeechBlock(transcript *domain.TranscriptArtifact, index int) (domain.SpeechBlock, bool) {
	for _, block := range transcript.SpeechBlocks {
		if block.Index == index && domain.IsSpeechBlock(block) {
			return block, true
		}
	}
	return domain.SpeechBlock{}, false
}

// canonicalGapIsEmpty reports whether no other canonical speech block occupies the open
// interval between two members, so merging them cannot skip a source turn.
func canonicalGapIsEmpty(transcript *domain.TranscriptArtifact, prev, next domain.SpeechBlock) bool {
	for _, block := range transcript.SpeechBlocks {
		if !domain.IsSpeechBlock(block) || block.Index == prev.Index || block.Index == next.Index {
			continue
		}
		if block.StartMs < next.StartMs && block.EndMs > prev.EndMs {
			return false
		}
	}
	return true
}

// speakerIdentity applies the fallback the synthesis pass attributes to an unlabelled turn, on
// both the script side and the canonical side, so a membership decision compares one identity.
func speakerIdentity(speakerID string) string {
	if speakerID == "" {
		return "SPEAKER_00"
	}
	return speakerID
}

// dubSegmentSpeaker resolves the script speaker of one segment with the fallback the
// synthesis pass applies to an unlabelled turn.
func dubSegmentSpeaker(seg domain.DubScriptSegment) string {
	return speakerIdentity(seg.SpeakerID)
}

// canonicalSpeaker resolves the speaker identity the synthesis pass attributes to a canonical
// SpeechBlock, so a block's identity is comparable with the script speaker of the turn it anchors.
func canonicalSpeaker(block domain.SpeechBlock) string {
	return speakerIdentity(block.SpeakerID)
}

// speakerEscalationPlan carries a whole-speaker escalation to the duration-controlled
// fallback lane plus the audit evidence to record on the regenerated variant.
type speakerEscalationPlan struct {
	target      domain.VoiceProfile // the fallback lane voice every escalated speaker uses
	assignments map[string]domain.VoiceProfile
	evidence    []domain.VoiceProviderEscalation
}

// synthesizeSegmentsPass synthesizes every dub segment, probes the actual synthesized
// duration, and runs the measured-duration fit controller (ACCEPT | RESYNTH | REWRITE |
// REGROUP | REVIEW). A superseding assignment reuses prior selected segments for every
// speaker it did not invalidate; priorHint hands the variant an escalation regenerates
// over in memory instead of relying on the asset-scoped index lookup.
func (s *DubbingService) synthesizeSegmentsPass(ctx context.Context, in domain.DubbingJobInput, dubScript *domain.DubScriptVariant, dubScriptCAS string, voiceAssign *domain.VoiceAssignment, voiceAssignCAS string, priorHint *domain.DubSegmentsVariant, escalations []domain.VoiceProviderEscalation, recoveryState *lineageRecoveryState) (*domain.DubSegmentsVariant, error) {
	targetLang := in.TargetLanguage
	transcript, rolePlan, err := s.loadPlaybackTimeline(ctx, in)
	if err != nil {
		return nil, err
	}
	translationContract, err := s.loadDubTranslationContract(dubScript)
	if err != nil {
		return nil, err
	}
	// 3. Compute deterministic provenance hash
	provenanceHash, err := s.computeDubSegmentsProvenanceHash(in, dubScript, voiceAssign)
	if err != nil {
		return nil, fmt.Errorf("compute dub segments cache identity: %w", err)
	}

	// 4. Check idempotent cache in SQLite / CAS
	if s.db != nil && s.cas != nil {
		if cachedIdx, err := s.db.GetDubSegmentsVariantByProvenance(ctx, provenanceHash); err == nil && cachedIdx != nil {
			rc, err := s.cas.Get(cachedIdx.CASHash)
			if err == nil {
				defer rc.Close()
				data, err := io.ReadAll(rc)
				if err == nil {
					var cachedVariant domain.DubSegmentsVariant
					if err := json.Unmarshal(data, &cachedVariant); err == nil {
						cachedVariant.CASHash = cachedIdx.CASHash
						cachedVariant.ProvenanceHash = cachedIdx.ProvenanceHash
						return &cachedVariant, nil
					}
				}
			}
		}
	}

	// 5. Invalidation scope optimization: load superseded DubSegmentsVariant if current VoiceAssignment
	// supersedes another assignment (Issue #40). Reuses prior selected DubSegments ONLY if the prior
	// variant was written under the current schema and matches BOTH the superseded VoiceAssignmentCAS
	// and the exact same DubScriptVariantCAS.
	var priorVariant *domain.DubSegmentsVariant
	invalidatedSpeakersSet := make(map[string]bool)
	if voiceAssign.SupersedesCAS != "" {
		for _, spk := range voiceAssign.InvalidatedSpeakers {
			invalidatedSpeakersSet[spk] = true
		}
		// An escalation regenerates against the exact variant it supersedes: an
		// in-memory prior variant wins over the asset-scoped index lookup below.
		priorFromHint := priorHint != nil && priorHint.SchemaVersion == domain.DubSegmentsSchemaVersion && priorHint.VoiceAssignmentCAS == voiceAssign.SupersedesCAS && priorHint.DubScriptVariantCAS != "" && priorHint.DubScriptVariantCAS == dubScriptCAS
		if priorFromHint {
			priorVariant = priorHint
		}
		if !priorFromHint && s.db != nil && s.cas != nil {
			if prevIdx, err := s.db.GetDubSegmentsVariantIndex(ctx, in.AssetID, targetLang); err == nil && prevIdx != nil {
				if rc, err := s.cas.Get(prevIdx.CASHash); err == nil {
					defer rc.Close()
					var prevVar domain.DubSegmentsVariant
					if err := json.NewDecoder(rc).Decode(&prevVar); err == nil &&
						prevVar.SchemaVersion == domain.DubSegmentsSchemaVersion &&
						prevVar.VoiceAssignmentCAS == voiceAssign.SupersedesCAS &&
						prevVar.DubScriptVariantCAS != "" &&
						prevVar.DubScriptVariantCAS == dubScriptCAS {
						priorVariant = &prevVar
					}
				}
			}
		}
	}

	// A prior variant may only be spliced into this pass when it was produced under the
	// same stage cache identity (same TTS runtime/model pins, same fit semantics, same
	// schema). A pin upgrade changes that identity without changing the assignment, so
	// reusing the prior segments would emit a variant whose audio came from two runtimes.
	priorReusable := false
	if priorVariant != nil && voiceAssign.SupersedesCAS != "" {
		if priorKey, err := s.computeDubSegmentsProvenanceHash(in, dubScript, &domain.VoiceAssignment{CASHash: voiceAssign.SupersedesCAS}); err == nil {
			priorReusable = priorVariant.ProvenanceHash == priorKey
		}
	}

	priorSegmentByIndex := make(map[int]domain.DubSegment)
	priorFitPlanByIndex := make(map[int]domain.DubbingFitPlan)
	if priorVariant != nil {
		for _, seg := range priorVariant.Segments {
			priorSegmentByIndex[seg.Index] = seg
		}
		for _, fp := range priorVariant.FitPlans {
			priorFitPlanByIndex[fp.SegmentIndex] = fp
		}
	}

	// 6. Iterate through segments, synthesize speech, probe actual duration, drive FitController
	var selectedSegments []domain.DubSegment
	var reviewSegments []domain.DubSegmentReview
	var fitPlans []domain.DubbingFitPlan
	fixedRateSpeakers := make(map[string]bool)
	overallStatus := "PASS"
	fc := s.fitController
	if fc == nil {
		fc = NewFitController()
	}
	// The mixer resamples every candidate to the background stem rate, so that rate is the
	// geometry the fit must prove its window against. It is resolved once per pass: a missing
	// stems artifact, unreadable header, or report is not a synthesis failure, it only drops
	// the fit back to the millisecond window (rate 0) while the mix stage keeps its own gate.
	outputSampleRate := resolveMixOutputSampleRate(ctx, s.db, s.cas, in.AssetID)

	for i := 0; i < len(dubScript.Segments); i++ {
		seg := dubScript.Segments[i]
		spkID := dubSegmentSpeaker(seg)
		voice, ok := voiceAssign.Assignments[spkID]
		if !ok || voice.ID == "" {
			// fallback to default preset voice for target language
			presets := provider.DefaultPresetVoices(targetLang)
			if len(presets) > 0 {
				voice = presets[0]
			} else {
				return nil, fmt.Errorf("%w: speaker %s", domain.ErrVoiceProfileNotFound, spkID)
			}
		}

		// Fail-safe speaker-scoped invalidation check:
		// If this VoiceAssignment supersedes a prior assignment and this speaker was NOT invalidated,
		// reuse the prior validated DubSegment and FitPlan directly without re-synthesizing ONLY IF:
		// 0) The prior variant was produced under this exact stage cache identity (priorReusable).
		// 1) Matching prior segment is present and valid with non-empty audio and accepted status.
		// 2) Corresponding prior DubbingFitPlan exists and was accepted with a measured
		//    candidate (its own decision is the evidence, not a re-derived window).
		// 3) Grouped SpeechBlockIndices / timing match the current dub script and speaker sequence.
		if priorReusable && !invalidatedSpeakersSet[spkID] {
			priorSeg, segExists := priorSegmentByIndex[seg.Index]
			priorFP, fpExists := priorFitPlanByIndex[seg.Index]
			if segExists && fpExists &&
				priorSeg.SpeakerID == spkID &&
				domain.VoiceProfileEquivalent(priorSeg.Voice, voice) &&
				priorSeg.AudioSHA256 != "" && priorSeg.AudioCASPath != "" &&
				priorSeg.FitDecision == domain.FitActionAccept &&
				!priorSeg.RequiresReview &&
				// The plan's own accepted decision is the reuse evidence. Re-deriving the window
				// from UsableSlotMs/DurationDeltaMs would reject Case-2 accepts, which fit the
				// accepted playback window while consuming part of the reserved natural gap.
				priorFP.Decision == domain.FitActionAccept &&
				priorFP.MeasuredDurationMs > 0 &&
				priorSeg.StartMs == seg.StartMs {
				validGrouping := true
				groupCount := len(priorSeg.SpeechBlockIndices)
				if groupCount <= 1 {
					if groupCount == 1 && priorSeg.SpeechBlockIndices[0] != seg.Index {
						validGrouping = false
					}
					if priorSeg.EndMs != seg.EndMs {
						validGrouping = false
					}
				} else {
					if i+groupCount > len(dubScript.Segments) {
						validGrouping = false
					} else {
						for k := 0; k < groupCount; k++ {
							currSegK := dubScript.Segments[i+k]
							currSpkK := dubSegmentSpeaker(currSegK)
							if currSegK.Index != priorSeg.SpeechBlockIndices[k] || currSpkK != spkID {
								validGrouping = false
								break
							}
						}
						if validGrouping {
							lastSeg := dubScript.Segments[i+groupCount-1]
							if priorSeg.EndMs != lastSeg.EndMs {
								validGrouping = false
							}
						}
					}
				}

				if validGrouping {
					// A reused segment keeps the lane evidence of the pass that
					// actually produced it, so the variant stays self-describing.
					if slices.Contains(priorVariant.FixedRateSpeakers, spkID) {
						fixedRateSpeakers[spkID] = true
					}
					selectedSegments = append(selectedSegments, priorSeg)
					fitPlans = append(fitPlans, priorFP)
					if groupCount > 1 {
						i += groupCount - 1
					}
					continue
				}
			}
		}

		actualSlotDur := seg.EndMs - seg.StartMs
		if actualSlotDur <= 0 {
			fitPolicyID := fc.policyID()
			fitPlans = append(fitPlans, domain.DubbingFitPlan{
				SegmentIndex: seg.Index, SpeakerID: spkID, SlotDurationMs: actualSlotDur,
				UsableSlotMs: actualSlotDur, DubPlaybackEndMs: seg.EndMs, FitPolicyID: fitPolicyID,
				SpeechBlockIndices: []int{seg.Index}, Decision: domain.FitActionReview,
				DecisionReason: "INVALID_TIMING",
			})
			reviewSegments = append(reviewSegments, domain.DubSegmentReview{
				Index: seg.Index, SpeechBlockIndices: []int{seg.Index}, SpeakerID: spkID,
				StartMs: seg.StartMs, EndMs: seg.EndMs, SlotDurationMs: actualSlotDur,
				SourceText: seg.SourceText, SpokenText: seg.SpokenText, FitDecision: domain.FitActionReview,
				ReviewReason: "INVALID_TIMING", DubPlaybackEndMs: seg.EndMs,
			})
			overallStatus = "REVIEW_REQUIRED"
			continue
		}
		nextVocalStartMs, err := playbackBoundaryForBlock(seg.Index, seg.StartMs, seg.EndMs, transcript, rolePlan)
		if err != nil {
			return nil, err
		}
		playbackEndMs, effectiveReserveMs, fitPolicyID := fc.ResolvePlaybackWindow(seg.EndMs, nextVocalStartMs)
		if playbackEndMs < seg.EndMs || playbackEndMs <= seg.StartMs {
			return nil, fmt.Errorf("invalid playback window for segment %d: %d-%d", seg.Index, seg.StartMs, playbackEndMs)
		}
		slotDurationMs := playbackEndMs - seg.StartMs

		var nextTurnStartMs int64
		var nextTurnSpkID string
		if i+1 < len(dubScript.Segments) {
			nextTurnStartMs = dubScript.Segments[i+1].StartMs
			nextTurnSpkID = dubScript.Segments[i+1].SpeakerID
		}

		currentText := seg.SpokenText
		if currentText == "" {
			currentText = seg.MeaningText
		}
		if rw, ok := recoveryState.AcceptedRewrites[seg.Index]; ok && rw != "" {
			currentText = rw
		}
		protectedTerms := glossaryForSource(translationContract.EffectiveGlossary, seg.SourceText)

		attempt := 1
		currentSpeed := 1.0
		currentCalibrationID := ""
		var finalCandidate *domain.TTSCandidate
		var finalFitPlan domain.DubbingFitPlan
		var finalDecision domain.FitAction
		var finalRequiresReview bool
		var finalReviewReason string
		// Issue #155/R4: a lineage that already chose its one bounded group replays that
		// topology and text instead of probing the lone trigger as a fresh regroup attempt.
		chosenGroup := recoveryState.ChosenGroups[seg.Index]
		carriedTopology := len(chosenGroup.MemberIndices) >= 2
		for attempt <= 3 {
			if carriedTopology {
				finalDecision = domain.FitActionRegroup
				break
			}
			synthReq := provider.TTSSynthesisRequest{
				RunID:          in.RunID,
				AssetID:        in.AssetID,
				SegmentIndex:   seg.Index,
				SpeakerID:      spkID,
				Text:           currentText,
				Language:       targetLang,
				Voice:          voice,
				Speed:          currentSpeed,
				SlotDurationMs: slotDurationMs,
				UsableSlotMs:   seg.SlotDurationMs,
				AttemptNumber:  attempt,
			}

			synthRes, selectedProv, err := s.invokeTTSWithFallback(ctx, synthReq, in.ExecutionProfile, in.AuthorizedCredentials)
			if err != nil {
				return nil, fmt.Errorf("tts synthesis for segment %d attempt %d: %w", seg.Index, attempt, err)
			}

			// PROBE TRUE SYNTHESIZED-MEDIA DURATION (fail closed on probe failure)
			probedMs, err := media.ProbeWAVBytes(synthRes.AudioData)
			if err != nil {
				return nil, fmt.Errorf("probe synthesized media duration for segment %d attempt %d: %w", seg.Index, attempt, err)
			}
			if probedMs <= 0 {
				return nil, fmt.Errorf("invalid probed duration %dms for segment %d attempt %d", probedMs, seg.Index, attempt)
			}
			var audioPath, audioSHA string
			if s.cas != nil {
				casObj, err := s.cas.Put(bytes.NewReader(synthRes.AudioData))
				if err != nil {
					return nil, fmt.Errorf("commit synthesized audio segment %d to CAS: %w", seg.Index, err)
				}
				audioPath = casObj.Path
				audioSHA = casObj.SHA256
			} else {
				audioSHA = synthRes.AudioSHA256
			}

			fixedRateVoice := ttsFitCapabilities(selectedProv)
			if fixedRateVoice {
				fixedRateSpeakers[spkID] = true
			}
			var provID, modelID, modelVer string
			if selectedProv != nil {
				provID = selectedProv.ID()
				modelID, modelVer = selectedProv.ModelInfo()
			}

			// Evaluate fit
			measuredFrames, measuredSampleRate := candidateFrameGeometry(synthRes.AudioData)
			evalInput := FitEvaluationInput{
				SegmentIndex:              seg.Index,
				SpeakerID:                 spkID,
				StartMs:                   seg.StartMs,
				EndMs:                     seg.EndMs,
				NextTurnStartMs:           nextTurnStartMs,
				NextTurnSpeakerID:         nextTurnSpkID,
				SourceGapAfterMs:          seg.SourceGapAfterMs,
				MeasuredDurationMs:        probedMs,
				AttemptNumber:             attempt,
				CurrentSpeed:              currentSpeed,
				CanShortenText:            len(strings.Fields(currentText)) > 3,
				FixedRateVoice:            fixedRateVoice,
				DubPlaybackEndMs:          playbackEndMs,
				EffectiveReserveMs:        effectiveReserveMs,
				ProviderID:                provID,
				ModelID:                   modelID,
				ModelVersion:              modelVer,
				VoiceProfileID:            voice.ID,
				NativeAttemptsForLineage:  recoveryState.NativeAttempts[seg.Index],
				RewriteAttemptsForLineage: recoveryState.RewriteAttempts[seg.Index],
				OutputSampleRate:          outputSampleRate,
				MeasuredFrames:            measuredFrames,
				MeasuredSampleRate:        measuredSampleRate,
			}

			evalRes := fc.EvaluateCandidate(ctx, evalInput)
			activeCalID := currentCalibrationID
			if activeCalID == "" {
				activeCalID = evalRes.CalibrationID
			}
			finalFitPlan = domain.DubbingFitPlan{
				SegmentIndex:       seg.Index,
				SpeakerID:          spkID,
				SlotDurationMs:     slotDurationMs,
				UsableSlotMs:       evalRes.UsableSlotMs,
				MeasuredDurationMs: probedMs,
				DurationDeltaMs:    evalRes.DurationDeltaMs,
				SpeedFactor:        currentSpeed,
				NaturalGapMs:       evalRes.NaturalGapMs,
				Decision:           evalRes.Decision,
				DecisionReason:     evalRes.Reason,
				AttemptCount:       attempt,
				DubPlaybackEndMs:   evalRes.DubPlaybackEndMs,
				EffectiveReserveMs: evalRes.EffectiveReserveMs,
				FitPolicyID:        fitPolicyID,
				CalibrationID:      activeCalID,
				SpeechBlockIndices: []int{seg.Index},
			}

			candidate := &domain.TTSCandidate{
				CandidateID:         uuid.NewString(),
				SegmentIndex:        seg.Index,
				SpeakerID:           spkID,
				Text:                currentText,
				Voice:               voice,
				AudioCASPath:        audioPath,
				AudioSHA256:         audioSHA,
				PredictedDurationMs: synthRes.PredictedDurationMs,
				MeasuredDurationMs:  probedMs,
				SpeedFactor:         currentSpeed,
				CalibrationID:       currentCalibrationID,
				AttemptNumber:       attempt,
				ProbedAt:            time.Now().UTC(),
			}

			finalCandidate = candidate
			finalDecision = evalRes.Decision
			finalRequiresReview = evalRes.RequiresReview
			finalReviewReason = evalRes.ReviewReason

			if evalRes.Decision == domain.FitActionAccept {
				break
			} else if evalRes.Decision == domain.FitActionResynth {
				recoveryState.NativeAttempts[seg.Index]++
				currentSpeed = evalRes.RecommendedSpeed
				currentCalibrationID = evalRes.CalibrationID
				attempt++
			} else if evalRes.Decision == domain.FitActionRewrite {
				recoveryState.RewriteAttempts[seg.Index]++
				rewriteAccepted := false
				if s.spokenAdapter != nil {
					// OverrunMs is documented as the measured overrun above PlaybackAllowanceMs,
					// so it must be measured against that window. The frame-exact REWRITE path can
					// reach here with a floored millisecond probe that still fits the window while
					// the resampled waveform does not: substituting DurationDeltaMs there would
					// report an overrun above a budget the candidate never exceeded, since that
					// delta is measured against the reserve-reduced usable slot.
					overrunMs := measuredOverrunAboveAllowanceMs(probedMs, seg.StartMs, playbackEndMs,
						measuredFrames, measuredSampleRate, outputSampleRate)
					adaptRes, err := s.spokenAdapter.AdaptSpokenScript(ctx, provider.SpokenScriptAdaptationRequest{
						SourceText:            seg.SourceText,
						SourceLanguage:        dubScript.SourceLanguage,
						MeaningText:           seg.MeaningText,
						TargetLanguage:        targetLang,
						SlotDurationMs:        slotDurationMs,
						PlaybackAllowanceMs:   slotDurationMs,
						MeasuredDurationMs:    probedMs,
						OverrunMs:             overrunMs,
						SourceSpeakingRateCPS: seg.SourceSpeakingRateCPS,
						SourceGapAfterMs:      seg.SourceGapAfterMs,
						HasNextTurn:           nextTurnStartMs > 0,
						ProtectedTerms:        protectedTerms,
					})
					if err == nil && adaptRes != nil {
						candidateText := strings.TrimSpace(adaptRes.SpokenText)
						if candidateText != "" && candidateText != strings.TrimSpace(currentText) &&
							glossaryTargetsPreserved(seg.MeaningText, candidateText, protectedTerms) &&
							glossaryTargetsPreserved(currentText, candidateText, protectedTerms) {
							qa := NewMeaningFirstQAGate().ValidateSegment(seg.SourceText, candidateText, dubScript.SourceLanguage, targetLang, protectedTerms)
							if qa.Passed {
								currentText = candidateText
								recoveryState.AcceptedRewrites[seg.Index] = currentText
								currentSpeed = 1.0
								currentCalibrationID = ""
								rewriteAccepted = true
							}
						}
					}
				}
				if !rewriteAccepted {
					// Adapter error, empty/unchanged text, or QA failure preserves honest
					// prior text and measured evidence without a wasted synthesis/probe,
					// and advances to the next allowed remedy (regroup or review).
					evalInput.RewriteAttemptsForLineage = recoveryState.RewriteAttempts[seg.Index]
					nextRes := fc.EvaluateCandidate(ctx, evalInput)
					finalDecision = nextRes.Decision
					finalRequiresReview = nextRes.RequiresReview
					finalReviewReason = nextRes.ReviewReason
					finalFitPlan.Decision = nextRes.Decision
					finalFitPlan.DecisionReason = nextRes.Reason
					break
				}
				attempt++
			} else {
				// REVIEW or REGROUP: break loop
				break
			}
		}

		// Bounded same-speaker regrouping (#155/R4): the FitController asks for REGROUP,
		// the group is chosen before any synthesis, and only that group's own measured
		// result may be selected. Everything unresolved stays review evidence.
		regrouped := false
		if finalDecision == domain.FitActionRegroup {
			// The bounded group is chosen before any synthesis: the maximal contiguous
			// eligible prefix of canonical same-speaker members starting at this block,
			// capped at regroupMaxMembers. A group this lineage already chose is replayed
			// verbatim instead; nothing here grows or re-enters.
			plan := regroupPlan{}
			if carriedTopology {
				plan = carriedRegroupPlan(dubScript, i, chosenGroup)
			} else {
				plan = boundedRegroupPlan(dubScript, transcript, rolePlan, i, spkID, recoveryState)
				// The selection runs against the pinned canonical timeline, so a later pass of
				// this attempt finds the same group (or none) and replays that choice instead
				// of spending a second remedy.
				recoveryState.ChosenGroups[seg.Index] = chosenRegroup{
					MemberIndices: plan.MemberIndices, SourceText: plan.SourceText, SpokenText: plan.SpokenText,
				}
			}
			members := plan.Members
			if len(members) < 2 {
				if carriedTopology {
					// A lineage that chose a group must still resolve it: this path synthesizes
					// nothing, so a replayed remedy without its group would leave the review
					// path with no measured candidate at all.
					return nil, fmt.Errorf("carried regroup topology for segment %d no longer resolves a group (%d members)", seg.Index, len(members))
				}
				// Nothing eligible to merge, so grouping has no group: the block keeps its
				// own measured overrun as honest review evidence. The fit plan records that
				// verdict, not the REGROUP request it could not act on.
				finalDecision = domain.FitActionReview
				finalRequiresReview = true
				if finalReviewReason == "" {
					finalReviewReason = "DURATION_OVERRUN"
				}
				finalFitPlan.Decision = domain.FitActionReview
				finalFitPlan.DecisionReason = "no eligible bounded same-speaker group: " + finalReviewReason
			} else {
				last := members[len(members)-1]
				memberIndices := plan.MemberIndices
				groupSourceText := plan.SourceText
				groupSpokenText := plan.SpokenText
				groupStartMs := seg.StartMs
				groupEndMs := last.EndMs

				// The group's playback window is the one its final member is bounded by, and
				// that boundary is what the selected or review evidence records.
				nextBoundaryMs, boundaryErr := playbackBoundaryForBlock(last.Index, last.StartMs, last.EndMs, transcript, rolePlan)
				if boundaryErr != nil {
					return nil, boundaryErr
				}
				groupPlaybackEndMs, groupReserveMs, groupFitPolicyID := fc.ResolvePlaybackWindow(groupEndMs, nextBoundaryMs)
				groupSlotMs := groupPlaybackEndMs - groupStartMs

				groupProtectedTerms := glossaryForSource(translationContract.EffectiveGlossary, groupSourceText)
				groupQA := NewMeaningFirstQAGate().ValidateSegment(groupSourceText, groupSpokenText, dubScript.SourceLanguage, targetLang, groupProtectedTerms)

				// Exactly one combined synthesis and probe for the chosen group, and none at
				// all when the merged text already fails meaning/glossary QA.
				var groupAudioPath, groupAudioSHA string
				var groupProbedMs int64
				var groupEvalRes FitEvaluationResult
				accepted := false
				if groupQA.Passed {
					synthRes, selectedProv, synthErr := s.invokeTTSWithFallback(ctx, provider.TTSSynthesisRequest{
						RunID:          in.RunID,
						AssetID:        in.AssetID,
						SegmentIndex:   seg.Index,
						SpeakerID:      spkID,
						Text:           groupSpokenText,
						Language:       targetLang,
						Voice:          voice,
						Speed:          1.0,
						SlotDurationMs: groupSlotMs,
						UsableSlotMs:   groupSlotMs,
						AttemptNumber:  1,
					}, in.ExecutionProfile, in.AuthorizedCredentials)
					if synthErr != nil {
						return nil, fmt.Errorf("regroup tts synthesis for segment %d (group %v): %w", seg.Index, memberIndices, synthErr)
					}
					groupProbedMs, synthErr = media.ProbeWAVBytes(synthRes.AudioData)
					if synthErr != nil || groupProbedMs <= 0 {
						return nil, fmt.Errorf("regroup probe media duration for segment %d (group %v): %w", seg.Index, memberIndices, synthErr)
					}
					if s.cas != nil {
						casObj, casErr := s.cas.Put(bytes.NewReader(synthRes.AudioData))
						if casErr != nil {
							return nil, fmt.Errorf("regroup commit audio segment %d to CAS: %w", seg.Index, casErr)
						}
						groupAudioPath, groupAudioSHA = casObj.Path, casObj.SHA256
					} else {
						groupAudioSHA = synthRes.AudioSHA256
					}
					fixedRateVoice := ttsFitCapabilities(selectedProv)
					if fixedRateVoice {
						fixedRateSpeakers[spkID] = true
					}
					var groupProvID, groupModelID, groupModelVer string
					if selectedProv != nil {
						groupProvID = selectedProv.ID()
						groupModelID, groupModelVer = selectedProv.ModelInfo()
					}
					groupFrames, groupSampleRate := candidateFrameGeometry(synthRes.AudioData)
					// GroupedCandidate keeps the group the last remedy of this attempt: no
					// further regroup, speed or rewrite may be granted from its measurement.
					groupEvalRes = fc.EvaluateCandidate(ctx, FitEvaluationInput{
						SegmentIndex:       seg.Index,
						SpeakerID:          spkID,
						StartMs:            groupStartMs,
						EndMs:              groupEndMs,
						MeasuredDurationMs: groupProbedMs,
						AttemptNumber:      1,
						CurrentSpeed:       1.0,
						GroupedCandidate:   true,
						FixedRateVoice:     fixedRateVoice,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
						ProviderID:         groupProvID,
						ModelID:            groupModelID,
						ModelVersion:       groupModelVer,
						VoiceProfileID:     voice.ID,
						OutputSampleRate:   outputSampleRate,
						MeasuredFrames:     groupFrames,
						MeasuredSampleRate: groupSampleRate,
					})
					accepted = groupEvalRes.Decision == domain.FitActionAccept && !groupEvalRes.RequiresReview &&
						groupProbedMs <= groupSlotMs
				}
				// One combined synthesis is what the group spends, so the evidence records
				// that call of its own - not the triggering block's earlier attempts - and a
				// QA-refused group spends none. The playback window is known before any
				// synthesis, so an unmeasured group still records the slot, usable slot and
				// reserve it never got to fill while MeasuredDurationMs stays 0.
				groupAttemptCount := 0
				groupUsableSlotMs := usableWindowMs(groupSlotMs, groupReserveMs)
				if groupQA.Passed {
					groupAttemptCount = 1
					groupUsableSlotMs = groupEvalRes.UsableSlotMs
				}

				if accepted {
					selectedSegments = append(selectedSegments, domain.DubSegment{
						Index:              seg.Index,
						SpeechBlockIndices: memberIndices,
						SpeakerID:          spkID,
						StartMs:            groupStartMs,
						EndMs:              groupEndMs,
						SlotDurationMs:     groupEndMs - groupStartMs,
						SourceText:         groupSourceText,
						SpokenText:         groupSpokenText,
						AudioCASPath:       groupAudioPath,
						AudioSHA256:        groupAudioSHA,
						MeasuredDurationMs: groupProbedMs,
						Voice:              voice,
						FitDecision:        domain.FitActionAccept,
						NaturalGapAfterMs:  groupEvalRes.NaturalGapMs,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
					})
					fitPlans = append(fitPlans, domain.DubbingFitPlan{
						SegmentIndex:       seg.Index,
						SpeakerID:          spkID,
						SlotDurationMs:     groupSlotMs,
						UsableSlotMs:       groupUsableSlotMs,
						MeasuredDurationMs: groupProbedMs,
						DurationDeltaMs:    groupEvalRes.DurationDeltaMs,
						SpeedFactor:        1.0,
						NaturalGapMs:       groupEvalRes.NaturalGapMs,
						Decision:           domain.FitActionAccept,
						DecisionReason:     "regrouped same-speaker turn into combined slot",
						AttemptCount:       groupAttemptCount,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
						FitPolicyID:        groupFitPolicyID,
						SpeechBlockIndices: memberIndices,
					})
				} else {
					// The review unit's operator-facing reason and the fit's own verdict for
					// that unit are separate evidence: the fit plan keeps what the measured
					// candidate was actually refused or granted by.
					reviewReason := groupEvalRes.ReviewReason
					fitReason := groupEvalRes.Reason
					if !groupQA.Passed {
						reviewReason = qaReviewReason(seg.Index, groupQA)
						fitReason = reviewReason
					} else if reviewReason == "" {
						reviewReason = "DURATION_OVERRUN"
					}
					reviewSegments = append(reviewSegments, domain.DubSegmentReview{
						Index:              seg.Index,
						SpeechBlockIndices: memberIndices,
						SpeakerID:          spkID,
						StartMs:            groupStartMs,
						EndMs:              groupEndMs,
						SlotDurationMs:     groupEndMs - groupStartMs,
						SourceText:         groupSourceText,
						SpokenText:         groupSpokenText,
						AudioCASPath:       groupAudioPath,
						AudioSHA256:        groupAudioSHA,
						MeasuredDurationMs: groupProbedMs,
						Voice:              voice,
						FitDecision:        domain.FitActionReview,
						ReviewReason:       reviewReason,
						AttemptCount:       groupAttemptCount,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
					})
					fitPlans = append(fitPlans, domain.DubbingFitPlan{
						SegmentIndex:       seg.Index,
						SpeakerID:          spkID,
						SlotDurationMs:     groupSlotMs,
						UsableSlotMs:       groupUsableSlotMs,
						MeasuredDurationMs: groupProbedMs,
						DurationDeltaMs:    groupEvalRes.DurationDeltaMs,
						SpeedFactor:        1.0,
						Decision:           domain.FitActionReview,
						DecisionReason:     fitReason,
						AttemptCount:       groupAttemptCount,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
						FitPolicyID:        groupFitPolicyID,
						SpeechBlockIndices: memberIndices,
					})
					overallStatus = "REVIEW_REQUIRED"
				}
				regrouped = true
				i += len(members) - 1
			}
		}

		if regrouped {
			continue
		}

		// Check playback-window selection gate: a candidate that exceeds the accepted
		// window or has non-positive duration cannot be selected for mixing.
		if finalCandidate == nil || finalCandidate.MeasuredDurationMs <= 0 || finalCandidate.MeasuredDurationMs > slotDurationMs {
			finalRequiresReview = true
			if finalReviewReason == "" {
				finalReviewReason = "DURATION_OVERRUN"
			}
			finalDecision = domain.FitActionReview
			overallStatus = "REVIEW_REQUIRED"
		}

		if finalRequiresReview {
			overallStatus = "REVIEW_REQUIRED"
		}

		// Calculate natural gap after
		var naturalGapAfter int64
		if nextTurnStartMs > seg.StartMs+finalCandidate.MeasuredDurationMs {
			naturalGapAfter = nextTurnStartMs - (seg.StartMs + finalCandidate.MeasuredDurationMs)
		}

		if finalDecision == domain.FitActionAccept && !finalRequiresReview && finalCandidate.MeasuredDurationMs <= slotDurationMs {
			dubSeg := domain.DubSegment{
				Index:              seg.Index,
				SpeechBlockIndices: []int{seg.Index},
				SpeakerID:          spkID,
				StartMs:            seg.StartMs,
				EndMs:              seg.EndMs,
				SlotDurationMs:     actualSlotDur,
				SourceText:         seg.SourceText,
				SpokenText:         currentText,
				AudioCASPath:       finalCandidate.AudioCASPath,
				AudioSHA256:        finalCandidate.AudioSHA256,
				MeasuredDurationMs: finalCandidate.MeasuredDurationMs,
				Voice:              voice,
				FitDecision:        finalDecision,
				ReviewReason:       "",
				RequiresReview:     false,
				NaturalGapAfterMs:  naturalGapAfter,
				DubPlaybackEndMs:   playbackEndMs,
				EffectiveReserveMs: effectiveReserveMs,
				CalibrationID:      finalCandidate.CalibrationID,
			}
			selectedSegments = append(selectedSegments, dubSeg)
		} else {
			// Record in ReviewSegments — overlong/review audio is NOT selected and cannot reach mixer
			revSeg := domain.DubSegmentReview{
				Index:              seg.Index,
				SpeechBlockIndices: []int{seg.Index},
				SpeakerID:          spkID,
				StartMs:            seg.StartMs,
				EndMs:              seg.EndMs,
				SlotDurationMs:     actualSlotDur,
				SourceText:         seg.SourceText,
				SpokenText:         currentText,
				AudioCASPath:       finalCandidate.AudioCASPath,
				AudioSHA256:        finalCandidate.AudioSHA256,
				MeasuredDurationMs: finalCandidate.MeasuredDurationMs,
				Voice:              voice,
				FitDecision:        finalDecision,
				ReviewReason:       finalReviewReason,
				AttemptCount:       finalFitPlan.AttemptCount,
				DubPlaybackEndMs:   playbackEndMs,
				EffectiveReserveMs: effectiveReserveMs,
				CalibrationID:      finalCandidate.CalibrationID,
			}
			reviewSegments = append(reviewSegments, revSeg)
			overallStatus = "REVIEW_REQUIRED"
		}
		fitPlans = append(fitPlans, finalFitPlan)
	}

	// 6. Verify zero collision and no anchor drift across the selected segments
	for i := 0; i < len(selectedSegments)-1; i++ {
		cur := selectedSegments[i]
		next := selectedSegments[i+1]
		curFinishMs := cur.StartMs + cur.MeasuredDurationMs
		if curFinishMs > next.StartMs {
			selectedSegments[i].RequiresReview = true
			selectedSegments[i].ReviewReason = "ADJACENT_COLLISION"
			overallStatus = "REVIEW_REQUIRED"
		}
	}

	// Prove complete canonical replacement coverage before committing PASS. Every
	// dub-eligible source SpeechBlock must appear exactly once across selected or
	// review evidence; otherwise RuntimeHost must pause for review before mixing.
	eligibleMembers := eligibleSpeechBlocks(transcript, rolePlan)
	coveredMembers := make(map[int]int, len(eligibleMembers))
	for _, seg := range selectedSegments {
		for _, member := range seg.SpeechBlockIndices {
			coveredMembers[member]++
		}
	}
	for _, seg := range reviewSegments {
		for _, member := range seg.SpeechBlockIndices {
			coveredMembers[member]++
		}
	}
	for member, count := range coveredMembers {
		if _, ok := eligibleMembers[member]; !ok || count != 1 {
			overallStatus = "REVIEW_REQUIRED"
		}
	}
	for member, block := range eligibleMembers {
		if coveredMembers[member] != 0 {
			continue
		}
		reviewSegments = append(reviewSegments, domain.DubSegmentReview{
			Index: member, SpeechBlockIndices: []int{member}, SpeakerID: block.SpeakerID,
			StartMs: block.StartMs, EndMs: block.EndMs, SlotDurationMs: block.EndMs - block.StartMs,
			SourceText: block.SourceText, FitDecision: domain.FitActionReview,
			ReviewReason: "MISSING_REPLACEMENT", AttemptCount: 0,
			DubPlaybackEndMs: block.EndMs,
		})
		overallStatus = "REVIEW_REQUIRED"
	}

	fitPolicyID := fc.policyID()
	var fitConfig *domain.FitControllerConfig
	if fitPolicyID != "" {
		cfgCopy := fc.effectiveConfig()
		fitConfig = &cfgCopy
	}
	variant := &domain.DubSegmentsVariant{
		ID:                    uuid.NewString(),
		SchemaVersion:         domain.DubSegmentsSchemaVersion,
		AssetID:               in.AssetID,
		RunID:                 in.RunID,
		JobID:                 in.JobID,
		TargetLanguage:        targetLang,
		DubScriptVariantCAS:   dubScriptCAS,
		VoiceAssignmentCAS:    voiceAssignCAS,
		TranscriptArtifactCAS: in.TranscriptArtifactCAS,
		AudioRolePlanCAS:      in.AudioRolePlanCAS,
		FitPolicyID:           fitPolicyID,
		FitConfig:             fitConfig,
		Segments:              selectedSegments,
		ReviewSegments:        reviewSegments,
		FitPlans:              fitPlans,
		ProvenanceHash:        provenanceHash,
		OverallStatus:         overallStatus,
		Escalations:           resolveEscalationOutcomes(escalations, reviewSegments),
		FixedRateSpeakers:     slices.Sorted(maps.Keys(fixedRateSpeakers)),
		CreatedAt:             time.Now().UTC(),
	}

	// Invariant: Do not publish the pass inside synthesizeSegmentsPass before the final recovery/escalation
	// decision is made. Build the pass first; the caller publishes or attaches review-only tempo candidates
	// exactly once when the configured recovery sequence has concluded.
	return variant, nil
}

// commitDubSegmentsVariant marshals a dub segments variant into CAS and points the run-scoped
// index at it while the variant's own voice assignment is still the run's current one. It is
// idempotent for the same bytes because CAS is content-addressed, so a variant re-committed after
// later evidence is attached supersedes the index entry in place. A pass that belongs to a
// superseded assignment is committed without the index row (#156): it stays readable by hash and
// is returned as evidence, but the row - what the inspector, the review projection and playback
// resolve - keeps pointing at the current assignment's own pass.
//
// The ownership test is the row write itself, not a decision taken earlier in the request: the
// claim statement refuses to claim while an assignment other than the variant's own is in force,
// so a reassignment landing between this request's earlier decisions and this write cannot slip
// a superseded pass into the row the way a pre-check-then-write pair lets it.
func (s *DubbingService) commitDubSegmentsVariant(ctx context.Context, variant *domain.DubSegmentsVariant) error {
	if variant == nil || s.cas == nil || s.db == nil {
		return nil
	}
	// The published bytes are canonical: an artifact never embeds an identity - its own or a
	// predecessor's - that CAS is about to derive from those very bytes, so re-publishing the
	// same content always yields the same hash.
	variant.CASHash = ""
	data, err := json.Marshal(variant)
	if err != nil {
		return fmt.Errorf("marshal dub segments variant: %w", err)
	}
	casObj, err := s.cas.Put(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("put dub segments variant in CAS: %w", err)
	}
	variant.CASHash = casObj.SHA256
	if s.beforeRunRowClaim != nil {
		s.beforeRunRowClaim()
	}

	idx := storage.DubSegmentsVariantIndex{
		ID:             variant.ID,
		AssetID:        variant.AssetID,
		RunID:          variant.RunID,
		JobID:          variant.JobID,
		TargetLanguage: variant.TargetLanguage,
		CASHash:        variant.CASHash,
		ProvenanceHash: variant.ProvenanceHash,
		OverallStatus:  variant.OverallStatus,
		CreatedAt:      variant.CreatedAt,
	}
	if _, err := s.db.ClaimDubSegmentsVariantIndexForAssignment(ctx, idx, variant.VoiceAssignmentCAS); err != nil {
		return fmt.Errorf("save dub segments variant index: %w", err)
	}
	return nil
}

// tempoEvidenceRetryable reports whether a recorded tempo outcome is evidence about the run
// that produced it rather than the transform's own answer to the waveform. A cancelled or
// unavailable transform says nothing about the candidate, so a resumed call re-derives it; every
// other outcome - fits, residual overrun, factor out of range, missing source, failed, oversized
// or invalid output - is the deterministic result of the transform and stays committed.
func tempoEvidenceRetryable(reason string) bool {
	return reason == domain.TempoReasonCancelled || reason == domain.TempoReasonToolUnavailable
}

// resetRetryableTempoEvidence clears the transient tempo evidence of a variant read back from the
// idempotent synthesis cache so the candidate can be derived again, and drops the committed
// identity so the republished pass becomes the run's one canonical final artifact. Non-transient
// candidates, and every other field of the cached pass, are left exactly as committed. It reports
// whether anything was reset, so an unchanged pass keeps its committed identity untouched.
func resetRetryableTempoEvidence(variant *domain.DubSegmentsVariant) bool {
	reset := false
	for i := range variant.ReviewSegments {
		candidate := variant.ReviewSegments[i].TempoCandidate
		if candidate == nil || !tempoEvidenceRetryable(candidate.Reason) {
			continue
		}
		variant.ReviewSegments[i].TempoCandidate = nil
		reset = true
	}
	if reset {
		variant.CASHash = ""
	}
	return reset
}

// tempoEvidenceComplete reports whether every review unit attachTempoCandidates would derive a
// candidate for already carries tempo evidence. A committed pass that still has such a unit was
// published before the derivation ran - the pre-escalation pass an escalation later superseded
// but a retried call could not repeat - so the cached fast path must not treat it as final.
func tempoEvidenceComplete(variant *domain.DubSegmentsVariant) bool {
	for i := range variant.ReviewSegments {
		rev := &variant.ReviewSegments[i]
		if rev.TempoCandidate != nil || rev.AudioSHA256 == "" || rev.MeasuredDurationMs <= 0 {
			continue
		}
		if !unresolvedTimingFailure(*rev) || rev.DubPlaybackEndMs <= rev.StartMs {
			continue
		}
		return false
	}
	return true
}

// attachTempoCandidates derives the review-only FFmpeg atempo alternative for every unresolved
// duration overrun of a FINAL synthesis pass and attaches it to the variant in place; the
// caller publishes the variant exactly once afterwards (Issue #156). It must never run before
// the natural/native/rewrite/regroup remedies and the whole-speaker escalation have been spent:
// a pass that is about to escalate is superseded, so deriving a candidate from its waveform
// would commit DSP the escalated pass immediately discards. The factor is the unit's own
// retained frame geometry over its accepted playback window, and the candidate stays unselected
// and REVIEW_REQUIRED at every factor.
//
// Only a unit whose review verdict is the fit controller's unresolved DURATION_OVERRUN derives
// a candidate: a unit flagged for invalid timing, a QA refusal or a missing replacement never
// had its duration remedy sequence run, so handing it DSP evidence would misreport an unrelated
// refusal as an exhausted tempo remedy.
func (s *DubbingService) attachTempoCandidates(ctx context.Context, variant *domain.DubSegmentsVariant) {
	if variant == nil {
		return
	}
	// The mixer's output rate is one property of the asset, not of the unit, so it is resolved
	// once for the whole variant — resolving it reads the background stem out of CAS, and doing
	// that per overrun would read full-length audio again for every unresolved unit. It is
	// resolved on the first unit that actually derives a candidate, since a variant with none
	// needs no rate at all.
	outRateResolved := false
	outRate := 0
	for i := range variant.ReviewSegments {
		rev := &variant.ReviewSegments[i]
		if rev.TempoCandidate != nil || rev.AudioSHA256 == "" || rev.MeasuredDurationMs <= 0 {
			continue
		}
		if !unresolvedTimingFailure(*rev) {
			continue
		}
		// The accepted playback window is what the unit must fit, so it is the divisor; a
		// non-positive window has no duration to fit and derives no candidate.
		if rev.DubPlaybackEndMs <= rev.StartMs {
			continue
		}
		if !outRateResolved {
			outRate = resolveMixOutputSampleRate(ctx, s.db, s.cas, variant.AssetID)
			outRateResolved = true
		}
		rev.TempoCandidate = s.tempoCandidateForReview(ctx, variant.FitPolicyID, outRate, rev)
	}
}

// resolveMixOutputSampleRate resolves the rate the mixer resamples every dub candidate to: the
// background stem's header rate (header-only, no decode), falling back to the preflight report's
// probed rate. 0 means "unknown" and keeps a fit on its millisecond window. It is the one
// resolution of that rate, shared by synthesis and by the reviewed-candidate acceptance that
// proves a selected waveform against the window the mixer will place it in.
func resolveMixOutputSampleRate(ctx context.Context, db *storage.DB, store *cas.Store, assetID string) int {
	if db == nil || store == nil || assetID == "" {
		return 0
	}
	if idx, err := db.GetAudioStemsArtifactIndex(ctx, assetID); err == nil && idx != nil && idx.AssetID == assetID && idx.CASHash != "" {
		if rc, err := store.Get(idx.CASHash); err == nil {
			var stems domain.AudioStemArtifacts
			decodeErr := json.NewDecoder(rc).Decode(&stems)
			rc.Close()
			if decodeErr == nil && stems.SchemaVersion == domain.AudioStemsSchemaVersion && stems.AssetID == assetID {
				for _, stem := range stems.Stems {
					if stem.Type != domain.StemTypeBackground || stem.AudioCASHash == "" {
						continue
					}
					bgRC, err := store.Get(stem.AudioCASHash)
					if err != nil {
						break
					}
					bgBytes, readErr := io.ReadAll(bgRC)
					bgRC.Close()
					if readErr != nil {
						break
					}
					if _, rate, err := media.WAVFrameGeometry(bgBytes); err == nil && rate > 0 {
						return rate
					}
					break
				}
			}
		}
	}
	report, err := db.GetPreflightReport(ctx, assetID)
	if err != nil || report == nil || report.AudioSampleRate <= 0 {
		return 0
	}
	return report.AudioSampleRate
}

// candidateFrameGeometry reports the header-only frame geometry of a synthesized waveform —
// the very frames the mixer will place after resampling. A non-standard header degrades to
// zero, which leaves the fit on its millisecond window instead of failing the synthesis pass.
func candidateFrameGeometry(audio []byte) (int64, int) {
	frames, rate, err := media.WAVFrameGeometry(audio)
	if err != nil || frames <= 0 || rate <= 0 {
		return 0, 0
	}
	return frames, rate
}

// measuredOverrunAboveAllowanceMs returns the measured overrun of a synthesized candidate above
// the accepted playback allowance (playbackEndMs - startMs). The mixer enforces that window
// frame-exactly, so a candidate whose floored millisecond probe fits the window can still
// overrun it in frames. That excess is converted back into the window's time base and rounded
// up, since an excess frame occupies part of the next millisecond; without positive frame
// evidence only the probe overrun is reported, and a candidate that proves no overrun reports 0.
func measuredOverrunAboveAllowanceMs(probedMs, startMs, playbackEndMs, measuredFrames int64, measuredSampleRate, outputSampleRate int) int64 {
	if overrunMs := probedMs - (playbackEndMs - startMs); overrunMs > 0 {
		return overrunMs
	}
	if outputSampleRate <= 0 || measuredFrames <= 0 || measuredSampleRate <= 0 {
		return 0
	}
	excessFrames := media.ResampledPCM16Frames(measuredFrames, measuredSampleRate, outputSampleRate) -
		media.PlaybackWindowFrames(startMs, playbackEndMs, outputSampleRate)
	if excessFrames <= 0 {
		return 0
	}
	return (excessFrames*1000 + int64(outputSampleRate) - 1) / int64(outputSampleRate)
}

// tempoCandidateForReview derives the review-only FFmpeg atempo alternative for one unresolved
// duration overrun (Issue #156). It runs only for an overrun whose own retained waveform is
// still intact: the factor is the waveform's exact frame extent resampled to the mixer's output
// rate over the accepted playback window — the same arithmetic the fit controller and the mixer
// gate with — and a transform is generated only when 1 < factor <= 1.25.
//
// outRate is the caller-resolved mixer output rate for the asset; 0 means "unknown" and keeps
// the comparison in the source's own time base.
//
// Floored whole-millisecond probes are recorded as measured evidence but are never the
// eligibility or fit gate: a probe that fits a window while the resampled waveform still
// overruns it by a frame is a real overrun, and treating it as a fit would both suppress a
// genuine candidate and misreport a transformed candidate as selectable.
//
// Every other outcome is recorded as evidence with no DSP artifact. The candidate is never
// selected and never turns REVIEW_REQUIRED into a PASS; a cancelled or timed-out transform is
// recorded as TEMPO_CANCELLED evidence with nothing committed for the transform, so the
// operator can read why the alternative is missing instead of losing the pass, and a failed
// transform still commits nothing.
func (s *DubbingService) tempoCandidateForReview(ctx context.Context, fitPolicyID string, outRate int, rev *domain.DubSegmentReview) *domain.DubTempoCandidate {
	base := domain.DubTempoCandidate{
		NaturalAudioSHA256: rev.AudioSHA256,
		NaturalDurationMs:  rev.MeasuredDurationMs,
		PlaybackDurationMs: rev.DubPlaybackEndMs - rev.StartMs,
		Reason:             domain.TempoReasonSourceInvalid,
		ToolID:             domain.TempoToolID,
		PolicyVersion:      fitPolicyID,
	}
	if rev.AudioSHA256 == "" || rev.DubPlaybackEndMs <= rev.StartMs || s.cas == nil {
		return &base
	}
	// The retained waveform must still be intact: a missing or corrupt natural parent is
	// evidence, not a transform input. No factor-range verdict is recorded before the factor
	// exists — the source itself is what is unavailable, and the factor is only derivable from
	// that source.
	if err := s.cas.VerifyIntegrity(rev.AudioSHA256); err != nil {
		return &base
	}
	rc, err := s.cas.Get(rev.AudioSHA256)
	if err != nil {
		return &base
	}
	source, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return &base
	}
	frames, rate, err := media.WAVFrameGeometry(source)
	if err != nil || frames <= 0 || rate <= 0 {
		return &base
	}
	// The mixer resamples every candidate to the asset's output rate, so both the retained
	// waveform and the accepted window are measured in that rate. Without one, the comparison
	// stays frame-exact in the source's own time base — either way it never falls back to a
	// floored millisecond probe.
	if outRate <= 0 {
		outRate = rate
	}
	sourceFrames := media.ResampledPCM16Frames(frames, rate, outRate)
	windowFrames := media.PlaybackWindowFrames(rev.StartMs, rev.DubPlaybackEndMs, outRate)
	if sourceFrames <= 0 || windowFrames <= 0 {
		return &base
	}
	factor := float64(sourceFrames) / float64(windowFrames)
	base.Factor = factor
	// Above 1.25 (or at/below 1, which no unresolved overrun produces) no DSP candidate exists.
	if !(factor > 1) || factor > media.AtempoMaxFactor {
		base.Reason = domain.TempoReasonFactorOutOfRange
		return &base
	}

	transformFn := media.ApplyAtempoWAV
	if s.AtempoTransform != nil {
		transformFn = s.AtempoTransform
	}
	transformed, err := transformFn(ctx, media.AtempoRequest{SourceWAV: source, Factor: factor})
	if err != nil {
		// A cancelled or timed-out transform is evidence, not a pass failure: run control still
		// stops the run through its own cancellation check, and the operator reads why no
		// alternative exists instead of losing the pass.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			base.Reason = domain.TempoReasonCancelled
			return &base
		}
		base.Reason = tempoReasonForError(err)
		return &base
	}
	transformedMs, probeErr := media.ProbeWAVBytes(transformed)
	transformedFrames, transformedRate, geomErr := media.WAVFrameGeometry(transformed)
	if probeErr != nil || transformedMs <= 0 || geomErr != nil || transformedFrames <= 0 || transformedRate <= 0 {
		base.Reason = domain.TempoReasonOutputInvalid
		return &base
	}
	obj, err := s.cas.Put(bytes.NewReader(transformed))
	if err != nil {
		base.Reason = domain.TempoReasonTransformFailed
		return &base
	}
	base.TransformedAudioSHA256 = obj.SHA256
	base.TransformedDurationMs = transformedMs
	base.Filter = media.AtempoFilterString(factor)
	// Residual overrun is honest evidence: the transform is never cropped or retried to force
	// the duration, and an overlong output is marked unselectable rather than discarded. The
	// verdict is the frame-exact comparison the mixer enforces, not the floored probe: the
	// transformed waveform is re-measured by frame geometry in the same output rate.
	transformedFramesOut := media.ResampledPCM16Frames(transformedFrames, transformedRate, outRate)
	base.Selectable = transformedFramesOut > 0 && transformedFramesOut <= windowFrames
	if base.Selectable {
		base.Reason = domain.TempoReasonFits
	} else {
		base.Reason = domain.TempoReasonOverrun
	}
	return &base
}

// tempoReasonForError maps a bounded-transform failure onto its deterministic review reason. A
// cancelled or timed-out transform never reaches here: the caller classifies it as
// TEMPO_CANCELLED before calling, so this mapping starts at the failures below that.
func tempoReasonForError(err error) string {
	switch {
	case errors.Is(err, media.ErrAtempoSourceInvalid):
		return domain.TempoReasonSourceInvalid
	case errors.Is(err, media.ErrAtempoUnavailable):
		return domain.TempoReasonToolUnavailable
	case errors.Is(err, media.ErrAtempoOutputTooLarge):
		return domain.TempoReasonOutputTooLarge
	case errors.Is(err, media.ErrAtempoOutputInvalid):
		return domain.TempoReasonOutputInvalid
	default:
		return domain.TempoReasonTransformFailed
	}
}

// unresolvedTimingFailure reports whether a review unit carries the FitController's own
// unresolved DURATION_OVERRUN verdict. That verdict is the source of truth for a timing
// failure: a candidate can overrun the accepted playback window frame-exactly while its
// floored millisecond probe still fits the slot, so a measured-vs-slot comparison alone
// misses a real failure. Unrelated review reasons (invalid timing, QA refusals, missing
// replacements) are not timing failures and never qualify.
func unresolvedTimingFailure(rev domain.DubSegmentReview) bool {
	return rev.FitDecision == domain.FitActionReview && rev.ReviewReason == "DURATION_OVERRUN"
}

// resolveEscalationOutcomes marks whether a whole-speaker regeneration cleared every
// unresolved slot overrun for that speaker. A false outcome is what projects REVIEW.
func resolveEscalationOutcomes(escalations []domain.VoiceProviderEscalation, reviewSegments []domain.DubSegmentReview) []domain.VoiceProviderEscalation {
	if len(escalations) == 0 {
		return nil
	}
	unresolved := make(map[string]bool)
	for _, rev := range reviewSegments {
		if unresolvedTimingFailure(rev) {
			unresolved[rev.SpeakerID] = true
		}
	}
	resolved := append([]domain.VoiceProviderEscalation(nil), escalations...)
	for i := range resolved {
		resolved[i].Resolved = !unresolved[resolved[i].SpeakerID]
	}
	return resolved
}

// planSpeakerEscalation decides which speakers may escalate to the duration-controlled
// fallback lane. A speaker qualifies only when a fixed-rate lane produced its candidate
// and the accepted playback window is still overrun after rewrite/regroup exhaustion, the
// affected speaker is not already on the fallback lane, and that lane is policy/license/
// runtime eligible for this run. Everything else keeps the existing review outcome.
func (s *DubbingService) planSpeakerEscalation(ctx context.Context, in domain.DubbingJobInput, voiceAssign *domain.VoiceAssignment, pass *domain.DubSegmentsVariant) *speakerEscalationPlan {
	if s.router == nil || s.db == nil || pass == nil || len(pass.FixedRateSpeakers) == 0 {
		return nil
	}
	targets := provider.CosyVoicePresetVoices(in.TargetLanguage)
	if len(targets) == 0 || !provider.IsVerifiedTTSVoice(targets[0].ProviderID, targets[0].VoiceID) {
		return nil
	}
	target := targets[0]

	triggers := make(map[string][]int)
	var speakers []string
	for _, rev := range pass.ReviewSegments {
		if !unresolvedTimingFailure(rev) || !slices.Contains(pass.FixedRateSpeakers, rev.SpeakerID) {
			continue
		}
		from, ok := voiceAssign.Assignments[rev.SpeakerID]
		if !ok || isProviderEquivalent(from.ProviderID, target.ProviderID) {
			continue
		}
		if _, seen := triggers[rev.SpeakerID]; !seen {
			speakers = append(speakers, rev.SpeakerID)
		}
		triggers[rev.SpeakerID] = append(triggers[rev.SpeakerID], rev.Index)
	}
	if len(speakers) == 0 {
		return nil
	}
	sort.Strings(speakers)
	if !s.escalationLaneEligible(ctx, in, target.ProviderID) {
		return nil
	}

	plan := &speakerEscalationPlan{target: target, assignments: make(map[string]domain.VoiceProfile, len(speakers))}
	for _, spk := range speakers {
		plan.assignments[spk] = target
	}

	// "One voice for all" keeps its meaning under escalation: the superseding assignment
	// pins every speaker to the fallback lane voice, so every speaker whose profile
	// actually changes is audited even when its own slots fit. A carried speaker has no
	// overrun trigger of its own, which is what keeps the trigger evidence honest.
	audited := speakers
	if voiceAssign.UseSameVoiceForAll {
		for spk, from := range voiceAssign.Assignments {
			if slices.Contains(speakers, spk) || domain.VoiceProfileEquivalent(from, target) {
				continue
			}
			audited = append(audited, spk)
		}
		sort.Strings(audited)
	}
	for _, spk := range audited {
		from := voiceAssign.Assignments[spk]
		reason := domain.VoiceEscalationReasonFixedRateOverrun
		if len(triggers[spk]) == 0 {
			reason = domain.VoiceEscalationReasonSharedVoiceScope
		}
		plan.evidence = append(plan.evidence, domain.VoiceProviderEscalation{
			SpeakerID:               spk,
			FromProviderID:          from.ProviderID,
			FromVoiceID:             from.VoiceID,
			ToProviderID:            target.ProviderID,
			ToVoiceID:               target.VoiceID,
			Reason:                  reason,
			TriggerSegmentIndices:   triggers[spk],
			SupersededAssignmentCAS: voiceAssign.CASHash,
		})
	}
	return plan
}

// escalationLaneEligible proves through the router (policy -> license -> snapshot ->
// capability -> health) that the duration-controlled fallback lane may run for this run
// and language before any speaker assignment is superseded.
func (s *DubbingService) escalationLaneEligible(ctx context.Context, in domain.DubbingJobInput, providerID string) bool {
	res, err := s.router.Route(ctx, provider.RouteRequest{
		RunID:                 in.RunID,
		Stage:                 provider.TypeTTS,
		Language:              in.TargetLanguage,
		ExecutionProfile:      in.ExecutionProfile,
		AuthorizedCredentials: in.AuthorizedCredentials,
		PreferredProviderID:   providerID,
	})
	if err != nil || res == nil || res.SelectedProvider == nil {
		return false
	}
	return isProviderEquivalent(res.SelectedProvider.ID(), providerID)
}

// applySpeakerEscalation persists the whole-speaker fallback provider change as a
// superseding VoiceAssignment and records the superseding CAS on the plan evidence.
// A run whose current assignment already is exactly this escalation reuses it, so a
// retried synthesis never mints a second superseding assignment for the same decision.
// A run that has moved on to any other assignment refuses the escalation (nil, nil).
func (s *DubbingService) applySpeakerEscalation(ctx context.Context, in domain.DubbingJobInput, base *domain.VoiceAssignment, plan *speakerEscalationPlan) (*domain.VoiceAssignment, error) {
	superseding, movedOn := s.currentRunAssignment(ctx, in, base)
	if movedOn && (superseding == nil || !isEscalationOfBase(superseding, base, plan)) {
		// The run already points at a newer assignment than the base this request
		// synthesized under (an operator reassignment made while the request was in
		// flight, for example). Minting from the stale base would make that older
		// assignment current again and silently revert the operator's decision, so no
		// escalation is applied and the unresolved pass stays REVIEW. An unreadable
		// current assignment counts as moved on: never supersede what cannot be read.
		return nil, nil
	}
	if superseding == nil {
		var err error
		superseding, err = s.supersedeVoiceAssignment(ctx, base, domain.VoiceAssignmentInput{
			RunID:                 in.RunID,
			AssetID:               in.AssetID,
			JobID:                 in.JobID,
			TargetLanguage:        in.TargetLanguage,
			UseSameVoiceForAll:    base.UseSameVoiceForAll,
			CustomAssignments:     plan.assignments,
			DubScriptVariantCAS:   in.DubScriptVariantCAS,
			TranscriptArtifactCAS: base.TranscriptArtifactCAS,
		})
		if err != nil {
			return nil, fmt.Errorf("escalate unresolved timing failure to the duration-controlled lane: %w", err)
		}
	}
	for i := range plan.evidence {
		plan.evidence[i].SupersedingAssignmentCAS = superseding.CASHash
	}
	return superseding, nil
}

// currentRunAssignment returns the assignment the run currently points at, plus whether
// the run has moved past base at all. A run with no stored assignment (or one already
// equal to base) has not moved; a run whose current assignment exists but cannot be read
// still reports the move, so callers fail safe instead of minting from a stale base.
func (s *DubbingService) currentRunAssignment(ctx context.Context, in domain.DubbingJobInput, base *domain.VoiceAssignment) (*domain.VoiceAssignment, bool) {
	if s.db == nil || s.cas == nil {
		return nil, false
	}
	idx, err := s.db.GetVoiceAssignmentIndexByRun(ctx, in.AssetID, in.RunID, in.TargetLanguage)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false
	}
	if err != nil {
		// Unknown read failure: assume the run moved on so the caller never supersedes
		// an assignment it could not read.
		return nil, true
	}
	if idx == nil || idx.CASHash == "" || idx.CASHash == base.CASHash {
		return nil, false
	}
	rc, err := s.cas.Get(idx.CASHash)
	if err != nil {
		return nil, true
	}
	defer rc.Close()
	var current domain.VoiceAssignment
	if err := json.NewDecoder(rc).Decode(&current); err != nil {
		return nil, true
	}
	current.CASHash = idx.CASHash
	return &current, true
}

// isEscalationOfBase reports whether current already is exactly the escalation the plan
// would mint from base, so a retried synthesis reuses it instead of minting a second one.
func isEscalationOfBase(current, base *domain.VoiceAssignment, plan *speakerEscalationPlan) bool {
	if current.SupersedesCAS != base.CASHash ||
		current.UseSameVoiceForAll != base.UseSameVoiceForAll ||
		len(current.Assignments) != len(base.Assignments) {
		return false
	}
	for spk, baseProfile := range base.Assignments {
		// "One voice for all" keeps its meaning: escalation replaces every profile
		// under the shared-voice flag, leaving no mixed lanes inside a speaker.
		want := baseProfile
		if _, escalated := plan.assignments[spk]; escalated || base.UseSameVoiceForAll {
			want = plan.target
		}
		if !domain.VoiceProfileEquivalent(current.Assignments[spk], want) {
			return false
		}
	}
	return true
}

// resolveSpeakers resolves all distinct speaker IDs from TranscriptArtifact or DubScriptVariant.
func (s *DubbingService) resolveSpeakers(ctx context.Context, in domain.VoiceAssignmentInput) ([]string, error) {
	speakerMap := make(map[string]bool)

	// 1. Explicit or indexed TranscriptArtifact
	transCAS := in.TranscriptArtifactCAS
	if transCAS != "" {
		if s.cas == nil {
			return nil, fmt.Errorf("cas store required to read transcript artifact %s", transCAS)
		}
		rc, err := s.cas.Get(transCAS)
		if err != nil {
			return nil, fmt.Errorf("read transcript artifact %s from CAS: %w", transCAS, err)
		}
		var transcript domain.TranscriptArtifact
		decodeErr := json.NewDecoder(rc).Decode(&transcript)
		_ = rc.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode transcript artifact %s: %w", transCAS, decodeErr)
		}
		if in.AssetID != "" && transcript.AssetID != "" && transcript.AssetID != in.AssetID {
			return nil, fmt.Errorf("transcript artifact %s belongs to asset %q, expected %q", transCAS, transcript.AssetID, in.AssetID)
		}
		// The producer's run id is provenance metadata, not an ownership claim: the
		// transcript is content-addressed with a run-independent provenance identity
		// (identical inputs reuse the artifact the first run persisted), so a re-run
		// of the same asset legitimately reads an artifact minted by an earlier run.
		// The media itself is pinned by the asset check above.
		for _, b := range transcript.SpeechBlocks {
			if b.SpeakerID != "" {
				speakerMap[b.SpeakerID] = true
			}
		}
	} else if s.db != nil && in.AssetID != "" {
		if tIdx, err := s.db.GetTranscriptArtifactIndex(ctx, in.AssetID); err == nil && tIdx != nil && tIdx.CASHash != "" {
			if s.cas != nil {
				rc, err := s.cas.Get(tIdx.CASHash)
				if err != nil {
					return nil, fmt.Errorf("read indexed transcript artifact %s from CAS: %w", tIdx.CASHash, err)
				}
				var transcript domain.TranscriptArtifact
				decodeErr := json.NewDecoder(rc).Decode(&transcript)
				_ = rc.Close()
				if decodeErr != nil {
					return nil, fmt.Errorf("decode indexed transcript artifact %s: %w", tIdx.CASHash, decodeErr)
				}
				if in.AssetID != "" && transcript.AssetID != "" && transcript.AssetID != in.AssetID {
					return nil, fmt.Errorf("indexed transcript artifact %s belongs to asset %q, expected %q", tIdx.CASHash, transcript.AssetID, in.AssetID)
				}
				for _, b := range transcript.SpeechBlocks {
					if b.SpeakerID != "" {
						speakerMap[b.SpeakerID] = true
					}
				}
			}
		}
	}

	// 2. Explicit or indexed DubScriptVariant if transcript had no speaker IDs
	if len(speakerMap) == 0 {
		dubScriptCAS := in.DubScriptVariantCAS
		if dubScriptCAS != "" {
			if s.cas == nil {
				return nil, fmt.Errorf("cas store required to read dub script variant %s", dubScriptCAS)
			}
			rc, err := s.cas.Get(dubScriptCAS)
			if err != nil {
				return nil, fmt.Errorf("read dub script variant %s from CAS: %w", dubScriptCAS, err)
			}
			var dubScript domain.DubScriptVariant
			decodeErr := json.NewDecoder(rc).Decode(&dubScript)
			_ = rc.Close()
			if decodeErr != nil {
				return nil, fmt.Errorf("decode dub script variant %s: %w", dubScriptCAS, decodeErr)
			}
			if in.AssetID != "" && dubScript.AssetID != "" && dubScript.AssetID != in.AssetID {
				return nil, fmt.Errorf("dub script variant %s belongs to asset %q, expected %q", dubScriptCAS, dubScript.AssetID, in.AssetID)
			}
			// Same rule as the transcript: the variant is an asset-scoped artifact whose
			// identity excludes the run, so a re-run reuses the earlier run's variant.
			// Asset and target language are the checks that pin what is being dubbed.
			if in.TargetLanguage != "" && dubScript.TargetLanguage != "" && !strings.EqualFold(dubScript.TargetLanguage, in.TargetLanguage) {
				return nil, fmt.Errorf("dub script variant %s target language %q does not match requested %q", dubScriptCAS, dubScript.TargetLanguage, in.TargetLanguage)
			}
			for _, seg := range dubScript.Segments {
				if seg.SpeakerID != "" {
					speakerMap[seg.SpeakerID] = true
				}
			}
		} else if s.db != nil && in.AssetID != "" {
			if dIdx, err := s.db.GetDubScriptVariantIndex(ctx, in.AssetID, in.TargetLanguage); err == nil && dIdx != nil && dIdx.CASHash != "" {
				if s.cas != nil {
					rc, err := s.cas.Get(dIdx.CASHash)
					if err != nil {
						return nil, fmt.Errorf("read indexed dub script variant %s from CAS: %w", dIdx.CASHash, err)
					}
					var dubScript domain.DubScriptVariant
					decodeErr := json.NewDecoder(rc).Decode(&dubScript)
					_ = rc.Close()
					if decodeErr != nil {
						return nil, fmt.Errorf("decode indexed dub script variant %s: %w", dIdx.CASHash, decodeErr)
					}
					if in.AssetID != "" && dubScript.AssetID != "" && dubScript.AssetID != in.AssetID {
						return nil, fmt.Errorf("indexed dub script variant %s belongs to asset %q, expected %q", dIdx.CASHash, dubScript.AssetID, in.AssetID)
					}
					for _, seg := range dubScript.Segments {
						if seg.SpeakerID != "" {
							speakerMap[seg.SpeakerID] = true
						}
					}
				}
			}
		}
	}

	if len(speakerMap) == 0 {
		return []string{"SPEAKER_00"}, nil
	}

	var speakers []string
	for spk := range speakerMap {
		speakers = append(speakers, spk)
	}
	return speakers, nil
}

func (s *DubbingService) transcriptCASFromTranslationVariant(assetID, targetLang, translationCAS string) (string, error) {
	if s.cas == nil || strings.TrimSpace(translationCAS) == "" {
		return "", errors.New("translation artifact is required to resolve transcript lineage")
	}
	rc, err := s.cas.Get(translationCAS)
	if err != nil {
		return "", fmt.Errorf("read translation artifact %s for transcript lineage: %w", translationCAS, err)
	}
	defer rc.Close()
	var variant domain.TranslationVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		return "", fmt.Errorf("decode translation artifact %s for transcript lineage: %w", translationCAS, err)
	}
	if variant.AssetID != assetID || !strings.EqualFold(variant.TargetLanguage, targetLang) {
		return "", fmt.Errorf("translation artifact lineage mismatch while resolving transcript")
	}
	if variant.SchemaVersion != domain.TranslationSchemaVersion || variant.ContractID != TranslationContractID || variant.InputHash == "" || variant.ProvenanceHash == "" {
		return "", fmt.Errorf("translation artifact does not satisfy current translation contract")
	}
	if strings.TrimSpace(variant.TranscriptArtifactCAS) == "" {
		return "", fmt.Errorf("translation artifact is missing transcript lineage")
	}
	return variant.TranscriptArtifactCAS, nil
}

// loadDubScriptVariant loads the DubScriptVariant from CAS or from the run's own evidence.
//
// An omitted explicitCAS resolves from run-scoped evidence only: the run's dub-script index row, or
// the artifact the run's dub_script stage execution pinned. The asset/language index is never
// consulted here — every caller of this loader is run-scoped, and adopting the asset's latest script
// would silently synthesize another run's text while claiming this run's lineage (#153). A caller
// that supplies no run_id is refused instead of being served asset-latest state.
func (s *DubbingService) loadDubScriptVariant(ctx context.Context, assetID, runID, targetLang, explicitCAS string) (*domain.DubScriptVariant, string, error) {
	if s.cas == nil || s.db == nil {
		return nil, "", fmt.Errorf("database and CAS store required to load dub script variant")
	}

	casHash := explicitCAS
	if casHash == "" {
		if strings.TrimSpace(runID) == "" {
			return nil, "", fmt.Errorf("%w: run_id is required to resolve a dub script variant", domain.ErrDubScriptRequiredForDubbing)
		}
		idx, err := s.db.GetDubScriptVariantIndexByRun(ctx, runID)
		switch {
		case err == nil && idx != nil:
			if idx.AssetID != assetID || !strings.EqualFold(idx.TargetLanguage, targetLang) {
				return nil, "", fmt.Errorf("%w: run %s dub script variant belongs to asset %s language %s, not %s/%s",
					domain.ErrTranslationOwnershipMismatch, runID, idx.AssetID, idx.TargetLanguage, assetID, targetLang)
			}
			casHash = idx.CASHash
		case err != nil && !errors.Is(err, storage.ErrNotFound):
			return nil, "", fmt.Errorf("resolve run %s dub script lineage: %w", runID, err)
		default:
			// The run reused a cached dub script and so owns no index row of its own; its dub_script
			// stage execution still pinned the exact artifact the run consumed.
			pinnedCAS, hashErr := s.db.GetStageArtifactHash(ctx, runID, "dub_script")
			if hashErr != nil {
				return nil, "", fmt.Errorf("resolve run %s dub script stage artifact: %w", runID, hashErr)
			}
			if pinnedCAS == "" {
				return nil, "", fmt.Errorf("%w: run %s pins no dub script variant", domain.ErrDubScriptRequiredForDubbing, runID)
			}
			casHash = pinnedCAS
		}
	}

	rc, err := s.cas.Get(casHash)
	if err != nil {
		return nil, "", fmt.Errorf("read dub script variant from CAS: %w", err)
	}
	defer rc.Close()

	var variant domain.DubScriptVariant
	if err := json.NewDecoder(rc).Decode(&variant); err != nil {
		return nil, "", fmt.Errorf("decode dub script variant: %w", err)
	}

	// Validate CAS ownership
	if variant.AssetID != "" && variant.AssetID != assetID {
		return nil, "", fmt.Errorf("dub script CAS object belongs to asset %q, not %q", variant.AssetID, assetID)
	}
	if variant.TargetLanguage != "" && !strings.EqualFold(variant.TargetLanguage, targetLang) {
		return nil, "", fmt.Errorf("dub script CAS object target language %q does not match requested %q", variant.TargetLanguage, targetLang)
	}

	variant.CASHash = casHash
	return &variant, casHash, nil
}

// loadVoiceAssignment loads the VoiceAssignment from CAS or SQLite.
func (s *DubbingService) loadVoiceAssignment(ctx context.Context, assetID, runID, targetLang, explicitCAS string) (*domain.VoiceAssignment, string, error) {
	if s.cas == nil || s.db == nil {
		return nil, "", fmt.Errorf("database and CAS store required to load voice assignment")
	}

	casHash := explicitCAS
	if casHash == "" {
		var idx *storage.VoiceAssignmentIndex
		var err error
		if runID != "" {
			idx, err = s.db.GetVoiceAssignmentIndexByRun(ctx, assetID, runID, targetLang)
		}
		if idx == nil || err != nil {
			if runID == "" {
				idx, err = s.db.GetVoiceAssignmentIndex(ctx, assetID, targetLang)
			}
			if idx == nil || err != nil {
				return nil, "", fmt.Errorf("voice assignment not found for asset %s (run %s) in language %s: %w", assetID, runID, targetLang, domain.ErrVoiceAssignmentNotFound)
			}
		}
		casHash = idx.CASHash
	}

	rc, err := s.cas.Get(casHash)
	if err != nil {
		return nil, "", fmt.Errorf("read voice assignment from CAS: %w", err)
	}
	defer rc.Close()

	var assignment domain.VoiceAssignment
	if err := json.NewDecoder(rc).Decode(&assignment); err != nil {
		return nil, "", fmt.Errorf("decode voice assignment: %w", err)
	}

	// Validate CAS ownership
	if assignment.AssetID != "" && assignment.AssetID != assetID {
		return nil, "", fmt.Errorf("voice assignment CAS object belongs to asset %q, not %q", assignment.AssetID, assetID)
	}
	if assignment.TargetLanguage != "" && !strings.EqualFold(assignment.TargetLanguage, targetLang) {
		return nil, "", fmt.Errorf("voice assignment CAS object target language %q does not match requested %q", assignment.TargetLanguage, targetLang)
	}

	assignment.CASHash = casHash
	return &assignment, casHash, nil
}

// computeVoiceAssignmentProvenanceHash computes deterministic cache identity for VoiceAssignment.
func (s *DubbingService) computeVoiceAssignmentProvenanceHash(in domain.VoiceAssignmentInput, assignments map[string]domain.VoiceProfile) (string, error) {
	inputHash, err := domain.ComputeVoiceAssignmentInputHash(in.AssetID, in.TargetLanguage, assignments, in.UseSameVoiceForAll)
	if err != nil {
		return "", err
	}

	inputHashes := []string{inputHash}
	if in.DubScriptVariantCAS != "" {
		inputHashes = append(inputHashes, in.DubScriptVariantCAS)
	}
	if in.TranscriptArtifactCAS != "" {
		inputHashes = append(inputHashes, in.TranscriptArtifactCAS)
	}
	return cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       "voice_assignment",
		InputHashes: inputHashes,
		SemanticConfig: map[string]any{
			"use_same_voice_for_all": in.UseSameVoiceForAll,
		},
		Language:      in.TargetLanguage,
		SchemaVersion: domain.VoiceAssignmentSchemaVersion,
	})
}

// computeDubSegmentsProvenanceHash computes deterministic cache identity for DubSegmentsVariant.
//
// The variant's audio is produced by a pinned TTS runtime, so that identity is a semantic
// input: `tts_runtime_identity` carries every lane's model revision and runtime pack, without
// which a pin upgrade leaves the key unchanged and cached rows replay audio from the previous
// runtime (CODING_STANDARDS §4). No single ProviderID/ModelName/ModelVersion triple is set —
// one pass can mix lanes (per-speaker assignment plus fallback-lane escalation) — and the
// schema version stays put because a pin upgrade does not change the artifact's shape.
func (s *DubbingService) computeDubSegmentsProvenanceHash(in domain.DubbingJobInput, dubScript *domain.DubScriptVariant, voiceAssign *domain.VoiceAssignment) (string, error) {
	var inputHashes []string
	if dubScript != nil && dubScript.CASHash != "" {
		inputHashes = append(inputHashes, dubScript.CASHash)
	}
	if voiceAssign != nil && voiceAssign.CASHash != "" {
		inputHashes = append(inputHashes, voiceAssign.CASHash)
	}
	if in.TranscriptArtifactCAS != "" {
		inputHashes = append(inputHashes, in.TranscriptArtifactCAS)
	}
	if in.AudioRolePlanCAS != "" {
		inputHashes = append(inputHashes, in.AudioRolePlanCAS)
	}
	fc := s.fitController
	if fc == nil {
		fc = NewFitController()
	}

	return cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       string(provider.TypeTTS),
		InputHashes: inputHashes,
		SemanticConfig: map[string]any{
			"zero_overrun_fit":     "measured_playback_window_v2",
			"fit_policy_id":        fc.policyID(),
			"tts_runtime_identity": provider.TTSRuntimeIdentities(),
		},
		Language:      in.TargetLanguage,
		SchemaVersion: domain.DubSegmentsSchemaVersion,
	})
}

// errEmptyTTSResult fails the pass closed when a provider reports success without a result.
// Every caller probes result.AudioData, so (nil result, nil error) must never leave this boundary.
var errEmptyTTSResult = errors.New("tts provider returned no synthesis result")

// invokeTTSWithFallback routes and executes TTS attempts with policy-checked fallback.
func (s *DubbingService) invokeTTSWithFallback(ctx context.Context, req provider.TTSSynthesisRequest, profile domain.ExecutionProfile, creds []string) (*provider.TTSSynthesisResult, provider.Provider, error) {
	if s.TTSInvoke != nil {
		// Custom hook installed (e.g. for unit tests)
		var p provider.Provider = &provider.BaseFakeProvider{
			ProviderID:   req.Voice.ProviderID,
			ProviderType: provider.TypeTTS,
			Policy:       provider.PolicyAllowed,
			Healthy:      true,
			ModelName:    req.Voice.ProviderID,
			ModelVersion: "1.0",
		}
		res, err := s.TTSInvoke(ctx, p, req)
		if err != nil {
			return res, p, err
		}
		if res == nil {
			return nil, nil, errEmptyTTSResult
		}
		return res, p, nil
	}

	if s.router == nil {
		return nil, nil, fmt.Errorf("provider router is not configured")
	}

	routeReq := provider.RouteRequest{
		RunID:                 req.RunID,
		Stage:                 provider.TypeTTS,
		Language:              req.Language,
		ExecutionProfile:      profile,
		AuthorizedCredentials: creds,
		PreferredProviderID:   req.Voice.ProviderID,
	}

	inputHash := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s_%s_%.4f", req.Text, req.Voice.ID, req.Speed))))
	if req.Voice.ProviderID != "" {
		// Frozen VoiceAssignment per speaker/run: must strictly invoke req.Voice.ProviderID
		// without sentence-by-sentence engine hopping, while preserving Router's ExecuteRoutedWithRetry
		// provenance, ProviderAttempt records, and circuit breaker accounting.
		routeRes, err := s.router.Route(ctx, routeReq)
		if err != nil {
			return nil, nil, fmt.Errorf("routing tts provider %s: %w", req.Voice.ProviderID, err)
		}
		if routeRes.SelectedProvider == nil || !isProviderEquivalent(routeRes.SelectedProvider.ID(), req.Voice.ProviderID) {
			return nil, nil, fmt.Errorf("%w: frozen provider %s is not eligible", domain.ErrNoEligibleTTSProvider, req.Voice.ProviderID)
		}
		// Constrain route result to the frozen provider only (no fallback candidates)
		routeRes.FallbackOrdered = nil

		var result *provider.TTSSynthesisResult
		var selected provider.Provider

		err = s.router.ExecuteRoutedWithRetry(ctx, routeReq, routeRes, inputHash, 2, func(p provider.Provider, attemptNum int) error {
			ttsProv, ok := p.(provider.TTSProvider)
			if !ok {
				return fmt.Errorf("provider %s does not implement TTSProvider", p.ID())
			}
			res, synthErr := ttsProv.SynthesizeSpeech(ctx, req)
			if synthErr != nil {
				return synthErr
			}
			result = res
			selected = p
			return nil
		})

		if err != nil {
			return nil, nil, fmt.Errorf("frozen tts provider %s failed after retries: %w", req.Voice.ProviderID, err)
		}
		if result == nil {
			return nil, nil, fmt.Errorf("frozen tts provider %s: %w", req.Voice.ProviderID, errEmptyTTSResult)
		}
		return result, selected, nil
	}
	// Generic routing fallback when no voice provider is frozen
	routeRes, err := s.router.Route(ctx, routeReq)
	if err != nil {
		return nil, nil, fmt.Errorf("routing tts provider: %w", err)
	}
	if routeRes.SelectedProvider == nil {
		return nil, nil, domain.ErrNoEligibleTTSProvider
	}

	var result *provider.TTSSynthesisResult
	var selected provider.Provider

	err = s.router.ExecuteWithRetry(ctx, routeReq, inputHash, 2, func(p provider.Provider, attemptNum int) error {
		ttsProv, ok := p.(provider.TTSProvider)
		if !ok {
			return fmt.Errorf("provider %s does not implement TTSProvider", p.ID())
		}
		res, synthErr := ttsProv.SynthesizeSpeech(ctx, req)
		if synthErr != nil {
			return synthErr
		}
		result = res
		selected = p
		return nil
	})

	if err != nil {
		return nil, nil, err
	}
	if result == nil {
		return nil, nil, errEmptyTTSResult
	}
	return result, selected, nil
}

func isVoiceAssignmentEquivalent(existing *domain.VoiceAssignment, newAssignments map[string]domain.VoiceProfile, useSameVoice bool) bool {
	if existing == nil {
		return false
	}
	if existing.UseSameVoiceForAll != useSameVoice {
		return false
	}
	if len(existing.Assignments) != len(newAssignments) {
		return false
	}
	for spk, newProfile := range newAssignments {
		existingProfile, ok := existing.Assignments[spk]
		if !ok {
			return false
		}
		if !domain.VoiceProfileEquivalent(existingProfile, newProfile) {
			return false
		}
	}
	return true
}

// ttsFitCapabilities reports whether the TTS lane that actually produced a
// candidate is a fixed-rate lane that fails closed on any non-1.0 speed request,
// so overrun must use rewrite/regroup/review instead of a speed resynthesis.
func ttsFitCapabilities(p provider.Provider) bool {
	if p == nil {
		return false
	}
	for _, f := range p.Capability().Features {
		if f == provider.FeatureFixedRateVoice {
			return true
		}
	}
	return false
}

// isProviderEquivalent checks if two provider IDs match (allowing fake_ prefix normalization).
func isProviderEquivalent(id1, id2 string) bool {
	id1 = strings.ToLower(strings.TrimSpace(id1))
	id2 = strings.ToLower(strings.TrimSpace(id2))
	return id1 == id2 || strings.TrimPrefix(id1, "fake_") == strings.TrimPrefix(id2, "fake_")
}
