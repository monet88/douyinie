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
		if strings.TrimSpace(in.DubScriptVariantCAS) == "" {
			if idx, err := s.db.GetDubScriptVariantIndexByRun(ctx, in.RunID); err == nil && idx != nil {
				if idx.AssetID != in.AssetID || !strings.EqualFold(idx.TargetLanguage, targetLang) {
					return nil, fmt.Errorf("run %s dub script lineage mismatch for voice assignment", in.RunID)
				}
				in.DubScriptVariantCAS = idx.CASHash
			} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return nil, fmt.Errorf("resolve run dub script lineage for voice assignment: %w", err)
			}
		}
		if strings.TrimSpace(in.TranscriptArtifactCAS) == "" {
			casHash, err := resolveRunTranscriptCAS(ctx, s.db, in.RunID, in.AssetID, "voice assignment")
			if err != nil {
				return nil, err
			}
			in.TranscriptArtifactCAS = casHash
		}
		if strings.TrimSpace(in.TranscriptArtifactCAS) == "" && strings.TrimSpace(in.DubScriptVariantCAS) != "" {
			dubScript, _, err := s.loadDubScriptVariant(ctx, in.AssetID, targetLang, in.DubScriptVariantCAS)
			if err != nil {
				return nil, fmt.Errorf("resolve dub script lineage for voice assignment: %w", err)
			}
			if strings.TrimSpace(dubScript.TranslationVariantCAS) != "" {
				transcriptCAS, err := s.transcriptCASFromTranslationVariant(in.AssetID, targetLang, dubScript.TranslationVariantCAS)
				if err != nil {
					return nil, err
				}
				in.TranscriptArtifactCAS = transcriptCAS
			}
		}
	}

	// Check if video has audio role plan with no dub-eligible dialogue (no-speech bypass)
	if s.db != nil && in.AssetID != "" {
		rolePlan, err := s.db.GetAudioRolePlan(ctx, in.AssetID)
		if err == nil && rolePlan != nil {
			if !domain.IsDubEligible(rolePlan) {
				return nil, domain.ErrNoDubbingRequired
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("get audio role plan: %w", err)
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
			rp, err := s.db.GetAudioRolePlan(ctx, in.AssetID)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					return nil, fmt.Errorf("%w: audio role plan not found for asset %s", domain.ErrAudioRolePlanRequired, in.AssetID)
				}
				return nil, fmt.Errorf("%w: failed to get audio role plan for asset %s: %v", domain.ErrAudioRolePlanRequired, in.AssetID, err)
			}
			if rp == nil {
				return nil, fmt.Errorf("%w: audio role plan is nil for asset %s", domain.ErrAudioRolePlanRequired, in.AssetID)
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
		segIdx := in.SegmentIndex
		if segIdx < 0 || segIdx >= len(dubScript.Segments) {
			return nil, fmt.Errorf("%w: segment index %d out of bounds (total segments: %d)", domain.ErrDubScriptVariantNotFound, segIdx, len(dubScript.Segments))
		}
		seg := dubScript.Segments[segIdx]
		if seg.SpokenText != "" {
			sampleText = seg.SpokenText
		} else if seg.MeaningText != "" {
			sampleText = seg.MeaningText
		}
		if strings.TrimSpace(sampleText) == "" {
			return nil, fmt.Errorf("%w: segment %d has empty translation text", domain.ErrDubScriptVariantNotFound, segIdx)
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
	rolePlan, err := s.db.GetAudioRolePlan(ctx, in.AssetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, domain.ErrAudioRolePlanRequired
		}
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
	dubScript, dubScriptCAS, err := s.loadDubScriptVariant(ctx, in.AssetID, targetLang, in.DubScriptVariantCAS)
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

	pass, err := s.synthesizeSegmentsPass(ctx, in, dubScript, dubScriptCAS, voiceAssign, voiceAssignCAS, nil, nil)
	if err != nil {
		return nil, err
	}

	// Issue #94: a speaker whose accepted playback window is still overrun by a
	// fixed-rate preset lane (ZeroTTS) after the bounded rewrite/regroup remedies
	// are exhausted escalates once, at whole-speaker scope, to the duration-controlled
	// fallback lane (CosyVoice3). The escalation supersedes that speaker's frozen
	// assignment and regenerates only its descendants; whatever is still unresolved
	// after the escalated regeneration projects REVIEW instead of hopping again.
	if plan := s.planSpeakerEscalation(ctx, in, voiceAssign, pass); plan != nil {
		superseding, err := s.applySpeakerEscalation(ctx, in, voiceAssign, plan)
		if err != nil {
			return nil, err
		}
		if superseding == nil {
			// The run moved on to a newer assignment that is not this escalation, so
			// the unresolved overrun above stays REVIEW instead of reverting it.
			return pass, nil
		}
		return s.synthesizeSegmentsPass(ctx, in, dubScript, dubScriptCAS, superseding, superseding.CASHash, pass, plan.evidence)
	}

	return pass, nil
}

// CanReuseVariant proves that a persisted dub artifact belongs to the exact
// current playback/voice/script lineage and fit-policy identity. It is read-only.
func (s *DubbingService) CanReuseVariant(ctx context.Context, in domain.DubbingJobInput, variant *domain.DubSegmentsVariant) bool {
	if variant == nil || variant.SchemaVersion != domain.DubSegmentsSchemaVersion || s.db == nil || s.cas == nil {
		return false
	}
	in.TargetLanguage = strings.ToLower(strings.TrimSpace(in.TargetLanguage))
	rolePlan, err := s.db.GetAudioRolePlan(ctx, in.AssetID)
	if err != nil || rolePlan == nil || rolePlan.CASHash == "" {
		return false
	}
	in.AudioRolePlanCAS = rolePlan.CASHash
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
	dubScript, dubCAS, err := s.loadDubScriptVariant(ctx, in.AssetID, in.TargetLanguage, in.DubScriptVariantCAS)
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
	rolePlan, err := s.db.GetAudioRolePlan(ctx, in.AssetID)
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
func (s *DubbingService) synthesizeSegmentsPass(ctx context.Context, in domain.DubbingJobInput, dubScript *domain.DubScriptVariant, dubScriptCAS string, voiceAssign *domain.VoiceAssignment, voiceAssignCAS string, priorHint *domain.DubSegmentsVariant, escalations []domain.VoiceProviderEscalation) (*domain.DubSegmentsVariant, error) {
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

	for i := 0; i < len(dubScript.Segments); i++ {
		seg := dubScript.Segments[i]
		spkID := seg.SpeakerID
		if spkID == "" {
			spkID = "SPEAKER_00"
		}
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
		// 2) Corresponding prior DubbingFitPlan exists, has decision ACCEPT, no review, and satisfies zero-overrun fit.
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
				priorFP.Decision == domain.FitActionAccept &&
				priorFP.DurationDeltaMs <= 0 &&
				priorFP.MeasuredDurationMs > 0 &&
				priorFP.MeasuredDurationMs <= priorFP.UsableSlotMs &&
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
							currSpkK := currSegK.SpeakerID
							if currSpkK == "" {
								currSpkK = "SPEAKER_00"
							}
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
		protectedTerms := glossaryForSource(translationContract.EffectiveGlossary, seg.SourceText)

		attempt := 1
		currentSpeed := 1.0
		var finalCandidate *domain.TTSCandidate
		var finalFitPlan domain.DubbingFitPlan
		var finalDecision domain.FitAction
		var finalRequiresReview bool
		var finalReviewReason string

		for attempt <= 3 {
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

			supportsSpeedFit, fixedRateVoice := ttsFitCapabilities(selectedProv)
			if fixedRateVoice {
				fixedRateSpeakers[spkID] = true
			}

			// Evaluate fit
			evalInput := FitEvaluationInput{
				SegmentIndex:       seg.Index,
				SpeakerID:          spkID,
				StartMs:            seg.StartMs,
				EndMs:              seg.EndMs,
				NextTurnStartMs:    nextTurnStartMs,
				NextTurnSpeakerID:  nextTurnSpkID,
				SourceGapAfterMs:   seg.SourceGapAfterMs,
				MeasuredDurationMs: probedMs,
				AttemptNumber:      attempt,
				CurrentSpeed:       currentSpeed,
				CanShortenText:     len(strings.Fields(currentText)) > 3,
				SupportsSpeedFit:   supportsSpeedFit,
				FixedRateVoice:     fixedRateVoice,
				DubPlaybackEndMs:   playbackEndMs,
				EffectiveReserveMs: effectiveReserveMs,
			}

			evalRes := fc.EvaluateCandidate(ctx, evalInput)
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
				// Retry with adjusted speed
				currentSpeed = evalRes.RecommendedSpeed
				attempt++
			} else if evalRes.Decision == domain.FitActionRewrite {
				// Shorten phrasing
				previousText := currentText
				if s.spokenAdapter != nil {
					adaptRes, err := s.spokenAdapter.AdaptSpokenScript(ctx, provider.SpokenScriptAdaptationRequest{
						SourceText:            seg.SourceText,
						SourceLanguage:        dubScript.SourceLanguage,
						MeaningText:           seg.MeaningText,
						TargetLanguage:        targetLang,
						SlotDurationMs:        slotDurationMs,
						SourceSpeakingRateCPS: seg.SourceSpeakingRateCPS,
						SourceGapAfterMs:      seg.SourceGapAfterMs,
						HasNextTurn:           nextTurnStartMs > 0,
						ProtectedTerms:        protectedTerms,
					})
					if err == nil && adaptRes.SpokenText != "" && adaptRes.SpokenText != currentText {
						currentText = adaptRes.SpokenText
					} else {
						// Simple trim fallback
						words := strings.Fields(currentText)
						if len(words) > 2 {
							candidateText := strings.Join(words[:len(words)-1], " ")
							if glossaryTargetsPreserved(currentText, candidateText, protectedTerms) {
								currentText = candidateText
							}
						}
					}
				}
				qa := NewMeaningFirstQAGate().ValidateSegment(seg.SourceText, currentText, dubScript.SourceLanguage, targetLang, protectedTerms)
				if !qa.Passed {
					// The measured rewrite is only a candidate. If shortening damages
					// meaning, keep the last meaning-valid text/evidence and let the
					// bounded fit loop advance to its remaining regroup/review remedy.
					// Never turn a failed rewrite attempt into the canonical spoken text.
					currentText = previousText
					attempt++
					continue
				}
				attempt++
			} else {
				// REVIEW or REGROUP: break loop
				break
			}
		}

		// Handle REGROUP within same speaker turn:
		// When FitController decides REGROUP, iteratively extend adjacent same-speaker blocks (1..N)
		// without crossing speaker boundaries, noneligible gaps, or moving later anchors.
		regrouped := false
		if finalDecision == domain.FitActionRegroup {
			consumedIndices := []int{seg.Index}
			combinedStartMs := seg.StartMs
			combinedEndMs := seg.EndMs
			combinedSourceText := seg.SourceText
			combinedSpokenText := currentText

			var lastRegroupSynthRes *provider.TTSSynthesisResult
			var lastRegroupProbedMs int64
			var lastRegroupEvalRes FitEvaluationResult
			var lastRegroupCombinedSlotMs int64
			var lastRegroupCombinedStartMs int64
			var lastRegroupCombinedEndMs int64
			var lastRegroupCombinedSourceText string
			var lastRegroupCombinedSpokenText string
			var lastRegroupPlaybackEndMs int64
			var lastRegroupReserveMs int64
			var lastRegroupFitPolicyID string
			var lastRegroupIndices []int
			lastRegroupReviewReason := "DURATION_OVERRUN"

			currIdx := i
			for currIdx+1 < len(dubScript.Segments) {
				nextSeg := dubScript.Segments[currIdx+1]
				nextSpk := nextSeg.SpeakerID
				if nextSpk == "" {
					nextSpk = "SPEAKER_00"
				}

				// Verify adjacent same-speaker eligibility and gap constraint
				prevSeg := dubScript.Segments[currIdx]
				if nextSpk != spkID || nextSeg.StartMs < prevSeg.EndMs || prevSeg.SourceGapAfterMs <= 0 || prevSeg.SourceGapAfterMs >= 600 {
					break
				}

				consumedIndices = append(consumedIndices, nextSeg.Index)
				currIdx++

				combinedEndMs = nextSeg.EndMs
				combinedSlotMs := combinedEndMs - combinedStartMs
				nextBoundaryMs, boundaryErr := playbackBoundaryForBlock(nextSeg.Index, nextSeg.StartMs, nextSeg.EndMs, transcript, rolePlan)
				if boundaryErr != nil {
					return nil, boundaryErr
				}
				groupPlaybackEndMs, groupReserveMs, groupFitPolicyID := fc.ResolvePlaybackWindow(combinedEndMs, nextBoundaryMs)
				groupPlaybackDurationMs := groupPlaybackEndMs - combinedStartMs
				if groupPlaybackDurationMs <= 0 {
					return nil, fmt.Errorf("invalid regroup playback window for segment %d", seg.Index)
				}
				combinedSourceText = strings.TrimSpace(combinedSourceText + " " + nextSeg.SourceText)
				nextSpoken := nextSeg.SpokenText
				if nextSpoken == "" {
					nextSpoken = nextSeg.MeaningText
				}
				combinedSpokenText = strings.TrimSpace(combinedSpokenText + " " + nextSpoken)
				groupProtectedTerms := glossaryForSource(translationContract.EffectiveGlossary, combinedSourceText)
				groupQA := NewMeaningFirstQAGate().ValidateSegment(combinedSourceText, combinedSpokenText, dubScript.SourceLanguage, targetLang, groupProtectedTerms)
				if !groupQA.Passed {
					lastRegroupCombinedSlotMs = combinedSlotMs
					lastRegroupCombinedStartMs = combinedStartMs
					lastRegroupCombinedEndMs = combinedEndMs
					lastRegroupCombinedSourceText = combinedSourceText
					lastRegroupCombinedSpokenText = combinedSpokenText
					lastRegroupPlaybackEndMs = groupPlaybackEndMs
					lastRegroupReserveMs = groupReserveMs
					lastRegroupFitPolicyID = groupFitPolicyID
					lastRegroupIndices = append([]int(nil), consumedIndices...)
					lastRegroupReviewReason = qaReviewReason(seg.Index, groupQA)
					lastRegroupEvalRes = FitEvaluationResult{
						Decision: domain.FitActionReview, SlotDurationMs: groupPlaybackDurationMs, UsableSlotMs: groupPlaybackDurationMs,
						RequiresReview: true, ReviewReason: lastRegroupReviewReason, Reason: "regrouped text failed meaning/glossary QA",
						DubPlaybackEndMs: groupPlaybackEndMs, EffectiveReserveMs: groupReserveMs, FitPolicyID: groupFitPolicyID,
					}
					break
				}

				var nextNextTurnStartMs int64
				var nextNextTurnSpkID string
				if currIdx+1 < len(dubScript.Segments) {
					nextNextTurnStartMs = dubScript.Segments[currIdx+1].StartMs
					nextNextTurnSpkID = dubScript.Segments[currIdx+1].SpeakerID
				}

				synthReq := provider.TTSSynthesisRequest{
					RunID:          in.RunID,
					AssetID:        in.AssetID,
					SegmentIndex:   seg.Index,
					SpeakerID:      spkID,
					Text:           combinedSpokenText,
					Language:       targetLang,
					Voice:          voice,
					Speed:          1.0,
					SlotDurationMs: groupPlaybackDurationMs,
					UsableSlotMs:   groupPlaybackDurationMs,
					AttemptNumber:  1,
				}

				synthRes, selectedProv, err := s.invokeTTSWithFallback(ctx, synthReq, in.ExecutionProfile, in.AuthorizedCredentials)
				if err != nil {
					return nil, fmt.Errorf("regroup tts synthesis for segment %d (group %v): %w", seg.Index, consumedIndices, err)
				}

				probedCombinedMs, probeErr := media.ProbeWAVBytes(synthRes.AudioData)
				if probeErr != nil || probedCombinedMs <= 0 {
					return nil, fmt.Errorf("regroup probe media duration for segment %d (group %v): %w", seg.Index, consumedIndices, probeErr)
				}

				supportsSpeedFit, fixedRateVoice := ttsFitCapabilities(selectedProv)
				if fixedRateVoice {
					fixedRateSpeakers[spkID] = true
				}

				regroupEvalInput := FitEvaluationInput{
					SegmentIndex:       seg.Index,
					SpeakerID:          spkID,
					StartMs:            combinedStartMs,
					EndMs:              combinedEndMs,
					NextTurnStartMs:    nextNextTurnStartMs,
					NextTurnSpeakerID:  nextNextTurnSpkID,
					SourceGapAfterMs:   nextSeg.SourceGapAfterMs,
					MeasuredDurationMs: probedCombinedMs,
					AttemptNumber:      1,
					CurrentSpeed:       1.0,
					CanShortenText:     len(strings.Fields(combinedSpokenText)) > 4,
					SupportsSpeedFit:   supportsSpeedFit,
					FixedRateVoice:     fixedRateVoice,
					DubPlaybackEndMs:   groupPlaybackEndMs,
					EffectiveReserveMs: groupReserveMs,
				}
				regroupEvalRes := fc.EvaluateCandidate(ctx, regroupEvalInput)

				lastRegroupSynthRes = synthRes
				lastRegroupProbedMs = probedCombinedMs
				lastRegroupEvalRes = regroupEvalRes
				lastRegroupCombinedSlotMs = combinedSlotMs
				lastRegroupCombinedStartMs = combinedStartMs
				lastRegroupCombinedEndMs = combinedEndMs
				lastRegroupCombinedSourceText = combinedSourceText
				lastRegroupCombinedSpokenText = combinedSpokenText
				lastRegroupPlaybackEndMs = groupPlaybackEndMs
				lastRegroupReserveMs = groupReserveMs
				lastRegroupFitPolicyID = groupFitPolicyID
				lastRegroupIndices = append([]int(nil), consumedIndices...)
				lastRegroupReviewReason = regroupEvalRes.ReviewReason

				if regroupEvalRes.Decision == domain.FitActionAccept && !regroupEvalRes.RequiresReview && probedCombinedMs <= groupPlaybackDurationMs {
					// Successfully fit and accepted!
					var audioPath, audioSHA string
					if s.cas != nil {
						casObj, err := s.cas.Put(bytes.NewReader(synthRes.AudioData))
						if err != nil {
							return nil, fmt.Errorf("regroup commit audio segment %d to CAS: %w", seg.Index, err)
						}
						audioPath = casObj.Path
						audioSHA = casObj.SHA256
					} else {
						audioSHA = synthRes.AudioSHA256
					}

					dubSeg := domain.DubSegment{
						Index:              seg.Index,
						SpeechBlockIndices: consumedIndices,
						SpeakerID:          spkID,
						StartMs:            combinedStartMs,
						EndMs:              combinedEndMs,
						SlotDurationMs:     combinedSlotMs,
						SourceText:         combinedSourceText,
						SpokenText:         combinedSpokenText,
						AudioCASPath:       audioPath,
						AudioSHA256:        audioSHA,
						MeasuredDurationMs: probedCombinedMs,
						Voice:              voice,
						FitDecision:        domain.FitActionAccept,
						ReviewReason:       "",
						RequiresReview:     false,
						NaturalGapAfterMs:  regroupEvalRes.NaturalGapMs,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
					}
					selectedSegments = append(selectedSegments, dubSeg)

					combinedFitPlan := domain.DubbingFitPlan{
						SegmentIndex:       seg.Index,
						SpeakerID:          spkID,
						SlotDurationMs:     combinedSlotMs,
						UsableSlotMs:       regroupEvalRes.UsableSlotMs,
						MeasuredDurationMs: probedCombinedMs,
						DurationDeltaMs:    regroupEvalRes.DurationDeltaMs,
						SpeedFactor:        1.0,
						NaturalGapMs:       regroupEvalRes.NaturalGapMs,
						Decision:           domain.FitActionAccept,
						DecisionReason:     "regrouped same-speaker turn into combined slot",
						AttemptCount:       1,
						DubPlaybackEndMs:   groupPlaybackEndMs,
						EffectiveReserveMs: groupReserveMs,
						FitPolicyID:        groupFitPolicyID,
						SpeechBlockIndices: append([]int(nil), consumedIndices...),
					}
					fitPlans = append(fitPlans, combinedFitPlan)

					regrouped = true
					i = currIdx
					break
				}

				// If FitController returned REGROUP, loop continues to attempt extending next eligible block
				if regroupEvalRes.Decision != domain.FitActionRegroup {
					// Non-regroup decision that is not accept (e.g. REVIEW) -> stop extending
					break
				}
			}

			if !regrouped && len(consumedIndices) > 1 {
				// Multiple blocks were consumed but remedies remained unresolved -> place entire consumed group in ReviewSegments
				var audioPath, audioSHA string
				if lastRegroupSynthRes != nil {
					if s.cas != nil {
						casObj, err := s.cas.Put(bytes.NewReader(lastRegroupSynthRes.AudioData))
						if err == nil {
							audioPath = casObj.Path
							audioSHA = casObj.SHA256
						}
					} else {
						audioSHA = lastRegroupSynthRes.AudioSHA256
					}
				}

				revReason := lastRegroupReviewReason
				if revReason == "" {
					revReason = "DURATION_OVERRUN"
				}

				revSeg := domain.DubSegmentReview{
					Index:              seg.Index,
					SpeechBlockIndices: append([]int(nil), lastRegroupIndices...),
					SpeakerID:          spkID,
					StartMs:            lastRegroupCombinedStartMs,
					EndMs:              lastRegroupCombinedEndMs,
					SlotDurationMs:     lastRegroupCombinedSlotMs,
					SourceText:         lastRegroupCombinedSourceText,
					SpokenText:         lastRegroupCombinedSpokenText,
					AudioCASPath:       audioPath,
					AudioSHA256:        audioSHA,
					MeasuredDurationMs: lastRegroupProbedMs,
					Voice:              voice,
					FitDecision:        domain.FitActionReview,
					ReviewReason:       revReason,
					AttemptCount:       1,
					DubPlaybackEndMs:   lastRegroupPlaybackEndMs,
					EffectiveReserveMs: lastRegroupReserveMs,
				}
				reviewSegments = append(reviewSegments, revSeg)
				overallStatus = "REVIEW_REQUIRED"

				combinedFitPlan := domain.DubbingFitPlan{
					SegmentIndex:       seg.Index,
					SpeakerID:          spkID,
					SlotDurationMs:     lastRegroupCombinedSlotMs,
					UsableSlotMs:       lastRegroupEvalRes.UsableSlotMs,
					MeasuredDurationMs: lastRegroupProbedMs,
					DurationDeltaMs:    lastRegroupEvalRes.DurationDeltaMs,
					SpeedFactor:        1.0,
					NaturalGapMs:       0,
					Decision:           domain.FitActionReview,
					DecisionReason:     "regrouped same-speaker turn still overruns combined slot",
					AttemptCount:       1,
					DubPlaybackEndMs:   lastRegroupPlaybackEndMs,
					EffectiveReserveMs: lastRegroupReserveMs,
					FitPolicyID:        lastRegroupFitPolicyID,
					SpeechBlockIndices: append([]int(nil), lastRegroupIndices...),
				}
				fitPlans = append(fitPlans, combinedFitPlan)

				regrouped = true
				i = currIdx
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
				DubPlaybackEndMs:   playbackEndMs,
				EffectiveReserveMs: effectiveReserveMs,
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
		FitPolicyID:           fc.policyID(),
		FitConfig:             &fc.config,
		Segments:              selectedSegments,
		ReviewSegments:        reviewSegments,
		FitPlans:              fitPlans,
		ProvenanceHash:        provenanceHash,
		OverallStatus:         overallStatus,
		Escalations:           resolveEscalationOutcomes(escalations, reviewSegments),
		FixedRateSpeakers:     slices.Sorted(maps.Keys(fixedRateSpeakers)),
		CreatedAt:             time.Now().UTC(),
	}

	// 7. Commit to CAS and SQLite
	if s.cas != nil && s.db != nil {
		data, err := json.Marshal(variant)
		if err != nil {
			return nil, fmt.Errorf("marshal dub segments variant: %w", err)
		}
		casObj, err := s.cas.Put(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("put dub segments variant in CAS: %w", err)
		}
		variant.CASHash = casObj.SHA256

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
		if err := s.db.SaveDubSegmentsVariantIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("save dub segments variant index: %w", err)
		}
	}

	return variant, nil
}

// resolveEscalationOutcomes marks whether a whole-speaker regeneration cleared every
// unresolved slot overrun for that speaker. A false outcome is what projects REVIEW.
func resolveEscalationOutcomes(escalations []domain.VoiceProviderEscalation, reviewSegments []domain.DubSegmentReview) []domain.VoiceProviderEscalation {
	if len(escalations) == 0 {
		return nil
	}
	unresolved := make(map[string]bool)
	for _, rev := range reviewSegments {
		if rev.FitDecision == domain.FitActionReview && rev.SlotDurationMs > 0 && rev.MeasuredDurationMs > rev.SlotDurationMs {
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
		if rev.FitDecision != domain.FitActionReview || !slices.Contains(pass.FixedRateSpeakers, rev.SpeakerID) {
			continue
		}
		if rev.SlotDurationMs <= 0 || rev.MeasuredDurationMs <= rev.SlotDurationMs {
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

// loadDubScriptVariant loads the DubScriptVariant from CAS or SQLite.
func (s *DubbingService) loadDubScriptVariant(ctx context.Context, assetID, targetLang, explicitCAS string) (*domain.DubScriptVariant, string, error) {
	if s.cas == nil || s.db == nil {
		return nil, "", fmt.Errorf("database and CAS store required to load dub script variant")
	}

	casHash := explicitCAS
	if casHash == "" {
		idx, err := s.db.GetDubScriptVariantIndex(ctx, assetID, targetLang)
		if err != nil {
			return nil, "", fmt.Errorf("dub script variant not found for asset %s in language %s: %w", assetID, targetLang, err)
		}
		casHash = idx.CASHash
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
		return res, p, err
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

		inputHash := fmt.Sprintf("%x", sha256.Sum256([]byte(req.Text+"_"+req.Voice.ID)))
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

	inputHash := fmt.Sprintf("%x", sha256.Sum256([]byte(req.Text+"_"+req.Voice.ID)))
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

// ttsFitCapabilities reports the rate-control contract of the TTS lane that
// actually produced a candidate: whether it accepts a measured speed-fit
// resynthesis, and whether it is a fixed-rate lane that fails closed on any
// non-1.0 speed request (so overrun must use rewrite/regroup/review instead).
func ttsFitCapabilities(p provider.Provider) (supportsSpeedFit, fixedRateVoice bool) {
	if p == nil {
		return false, false
	}
	for _, f := range p.Capability().Features {
		switch f {
		case "measured_duration_speed_fit", "zero_overrun_fit":
			supportsSpeedFit = true
		case provider.FeatureFixedRateVoice:
			fixedRateVoice = true
		}
	}
	return supportsSpeedFit, fixedRateVoice
}

// isProviderEquivalent checks if two provider IDs match (allowing fake_ prefix normalization).
func isProviderEquivalent(id1, id2 string) bool {
	id1 = strings.ToLower(strings.TrimSpace(id1))
	id2 = strings.ToLower(strings.TrimSpace(id2))
	return id1 == id2 || strings.TrimPrefix(id1, "fake_") == strings.TrimPrefix(id2, "fake_")
}
