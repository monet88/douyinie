package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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

	// 1. Resolve distinct speakers from TranscriptArtifact or DubScriptVariant
	speakers, err := s.resolveSpeakers(ctx, in.AssetID, targetLang)
	if err != nil {
		return nil, fmt.Errorf("resolve speakers for voice assignment: %w", err)
	}
	if len(speakers) == 0 {
		speakers = []string{"SPEAKER_00"}
	}

	// 2. Build or validate assignments
	assignments := make(map[string]domain.VoiceProfile)
	presetVoices := provider.DefaultPresetVoices(targetLang)
	if len(presetVoices) == 0 {
		return nil, domain.ErrNoEligibleTTSProvider
	}

	if in.UseSameVoiceForAll {
		// Single selected voice for all speakers
		var chosenVoice domain.VoiceProfile
		if len(in.CustomAssignments) > 0 {
			for _, v := range in.CustomAssignments {
				chosenVoice = v
				break
			}
		}
		if chosenVoice.ID == "" {
			chosenVoice = presetVoices[0]
		}
		for _, spk := range speakers {
			assignments[spk] = chosenVoice
		}
	} else {
		// Assign distinct preset voices per speaker
		for i, spk := range speakers {
			if custom, ok := in.CustomAssignments[spk]; ok && custom.ID != "" {
				assignments[spk] = custom
			} else {
				presetIdx := i % len(presetVoices)
				assignments[spk] = presetVoices[presetIdx]
			}
		}
	}
	// Check if a VoiceAssignment is already frozen for this exact run.
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
				// Equivalence check: any change to speaker->VoiceProfile mapping or UseSameVoiceForAll fails closed
				if !isVoiceAssignmentEquivalent(&existing, assignments, in.UseSameVoiceForAll) {
					return nil, domain.ErrVoiceAssignmentFrozen
				}
				return &existing, nil
			}
		}
	}

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
		ID:                 uuid.NewString(),
		SchemaVersion:      domain.VoiceAssignmentSchemaVersion,
		AssetID:            in.AssetID,
		RunID:              in.RunID,
		JobID:              in.JobID,
		TargetLanguage:     targetLang,
		Assignments:        assignments,
		UseSameVoiceForAll: in.UseSameVoiceForAll,
		ProvenanceHash:     provenanceHash,
		FrozenAt:           now,
		CreatedAt:          now,
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

// AuditionVoice generates a short 5s standalone or 10s contextual audio clip to audition a voice profile.
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

	sampleText := in.SampleText
	if sampleText == "" {
		if targetLang == "vi" {
			sampleText = "Xin chào, đây là bản thử giọng mẫu tự nhiên cho video của bạn."
		} else {
			sampleText = "Hello, this is a sample voice audition for your video."
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
		SlotDurationMs: 5000,
		UsableSlotMs:   5000,
		AttemptNumber:  1,
	}

	synthRes, _, err := s.invokeTTSWithFallback(ctx, req, domain.ExecutionProfileHybrid, nil)
	if err != nil {
		return nil, fmt.Errorf("synthesize audition voice: %w", err)
	}

	// Probe duration from synthesized audio (fail closed)
	probedMs, err := media.ProbeWAVBytes(synthRes.AudioData)
	if err != nil {
		return nil, fmt.Errorf("probe audition audio duration: %w", err)
	}
	if probedMs <= 0 {
		return nil, fmt.Errorf("invalid probed duration %dms for audition audio", probedMs)
	}
	var casHash, casPath string
	if s.cas != nil {
		casObj, err := s.cas.Put(bytes.NewReader(synthRes.AudioData))
		if err == nil {
			casHash = casObj.SHA256
			casPath = casObj.Path
		}
	}

	return &domain.VoiceAuditionResult{
		Voice:              in.Voice,
		AudioCASHash:       casHash,
		AudioCASPath:       casPath,
		MeasuredDurationMs: probedMs,
		IsContextual:       in.IsContextual,
		SampleText:         sampleText,
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

	// 1. Load DubScriptVariant
	dubScript, dubScriptCAS, err := s.loadDubScriptVariant(ctx, in.AssetID, targetLang, in.DubScriptVariantCAS)
	if err != nil {
		return nil, fmt.Errorf("load dub script for synthesis: %w", err)
	}
	in.DubScriptVariantCAS = dubScriptCAS

	// 2. Load frozen VoiceAssignment
	voiceAssign, voiceAssignCAS, err := s.loadVoiceAssignment(ctx, in.AssetID, in.RunID, targetLang, in.VoiceAssignmentCAS)
	if err != nil {
		return nil, fmt.Errorf("load voice assignment for synthesis: %w", err)
	}
	in.VoiceAssignmentCAS = voiceAssignCAS
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

	// 5. Iterate through segments, synthesize speech, probe actual duration, drive FitController
	var selectedSegments []domain.DubSegment
	var reviewSegments []domain.DubSegmentReview
	var fitPlans []domain.DubbingFitPlan
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

		slotDurationMs := seg.EndMs - seg.StartMs
		if slotDurationMs <= 0 {
			slotDurationMs = 1000
		}

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

			supportsSpeedFit := false
			if selectedProv != nil {
				for _, f := range selectedProv.Capability().Features {
					if f == "measured_duration_speed_fit" || f == "zero_overrun_fit" {
						supportsSpeedFit = true
						break
					}
				}
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
					})
					if err == nil && adaptRes.SpokenText != "" && adaptRes.SpokenText != currentText {
						currentText = adaptRes.SpokenText
					} else {
						// Simple trim fallback
						words := strings.Fields(currentText)
						if len(words) > 2 {
							currentText = strings.Join(words[:len(words)-1], " ")
						}
					}
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
				combinedSourceText = strings.TrimSpace(combinedSourceText + " " + nextSeg.SourceText)
				nextSpoken := nextSeg.SpokenText
				if nextSpoken == "" {
					nextSpoken = nextSeg.MeaningText
				}
				combinedSpokenText = strings.TrimSpace(combinedSpokenText + " " + nextSpoken)

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
					SlotDurationMs: combinedSlotMs,
					UsableSlotMs:   combinedSlotMs,
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

				supportsSpeedFit := false
				if selectedProv != nil {
					for _, f := range selectedProv.Capability().Features {
						if f == "measured_duration_speed_fit" || f == "zero_overrun_fit" {
							supportsSpeedFit = true
							break
						}
					}
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
				lastRegroupReviewReason = regroupEvalRes.ReviewReason

				if regroupEvalRes.Decision == domain.FitActionAccept && !regroupEvalRes.RequiresReview && probedCombinedMs <= combinedSlotMs {
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
				}
				fitPlans = append(fitPlans, combinedFitPlan)

				regrouped = true
				i = currIdx
			}
		}
		if regrouped {
			continue
		}

		// Check zero overrun selection gate:
		// A candidate that strictly overruns the slot cannot be accepted into selected Segments
		if finalCandidate.MeasuredDurationMs > slotDurationMs {
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
				SlotDurationMs:     slotDurationMs,
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
			}
			selectedSegments = append(selectedSegments, dubSeg)
		} else {
			// Record in ReviewSegments — overlong/review audio is NOT selected and cannot reach mixer
			revSeg := domain.DubSegmentReview{
				Index:              seg.Index,
				SpeakerID:          spkID,
				StartMs:            seg.StartMs,
				EndMs:              seg.EndMs,
				SlotDurationMs:     slotDurationMs,
				SourceText:         seg.SourceText,
				SpokenText:         currentText,
				AudioCASPath:       finalCandidate.AudioCASPath,
				AudioSHA256:        finalCandidate.AudioSHA256,
				MeasuredDurationMs: finalCandidate.MeasuredDurationMs,
				Voice:              voice,
				FitDecision:        finalDecision,
				ReviewReason:       finalReviewReason,
				AttemptCount:       attempt,
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

	variant := &domain.DubSegmentsVariant{
		ID:                  uuid.NewString(),
		SchemaVersion:       domain.DubSegmentsSchemaVersion,
		AssetID:             in.AssetID,
		RunID:               in.RunID,
		JobID:               in.JobID,
		TargetLanguage:      targetLang,
		DubScriptVariantCAS: dubScriptCAS,
		VoiceAssignmentCAS:  voiceAssignCAS,
		Segments:            selectedSegments,
		ReviewSegments:      reviewSegments,
		FitPlans:            fitPlans,
		ProvenanceHash:      provenanceHash,
		OverallStatus:       overallStatus,
		CreatedAt:           time.Now().UTC(),
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

// resolveSpeakers resolves all distinct speaker IDs from TranscriptArtifact or DubScriptVariant.
func (s *DubbingService) resolveSpeakers(ctx context.Context, assetID, targetLang string) ([]string, error) {
	if s.db == nil {
		return []string{"SPEAKER_00"}, nil
	}

	speakerMap := make(map[string]bool)

	// Check TranscriptArtifact first
	if tIdx, err := s.db.GetTranscriptArtifactIndex(ctx, assetID); err == nil && tIdx != nil && s.cas != nil {
		if rc, err := s.cas.Get(tIdx.CASHash); err == nil {
			defer rc.Close()
			var transcript domain.TranscriptArtifact
			if err := json.NewDecoder(rc).Decode(&transcript); err == nil {
				for _, b := range transcript.SpeechBlocks {
					if b.SpeakerID != "" {
						speakerMap[b.SpeakerID] = true
					}
				}
			}
		}
	}

	// Check DubScriptVariant if transcript had no speaker IDs
	if len(speakerMap) == 0 {
		if dIdx, err := s.db.GetDubScriptVariantIndex(ctx, assetID, targetLang); err == nil && dIdx != nil && s.cas != nil {
			if rc, err := s.cas.Get(dIdx.CASHash); err == nil {
				defer rc.Close()
				var dubScript domain.DubScriptVariant
				if err := json.NewDecoder(rc).Decode(&dubScript); err == nil {
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

	return cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       "voice_assignment",
		InputHashes: []string{inputHash},
		SemanticConfig: map[string]any{
			"use_same_voice_for_all": in.UseSameVoiceForAll,
		},
		Language:      in.TargetLanguage,
		SchemaVersion: domain.VoiceAssignmentSchemaVersion,
	})
}

// computeDubSegmentsProvenanceHash computes deterministic cache identity for DubSegmentsVariant.
func (s *DubbingService) computeDubSegmentsProvenanceHash(in domain.DubbingJobInput, dubScript *domain.DubScriptVariant, voiceAssign *domain.VoiceAssignment) (string, error) {
	var inputHashes []string
	if dubScript != nil && dubScript.CASHash != "" {
		inputHashes = append(inputHashes, dubScript.CASHash)
	}
	if voiceAssign != nil && voiceAssign.CASHash != "" {
		inputHashes = append(inputHashes, voiceAssign.CASHash)
	}

	return cas.ComputeStageCacheKey(domain.StageCacheIdentityInput{
		Stage:       string(provider.TypeTTS),
		InputHashes: inputHashes,
		SemanticConfig: map[string]any{
			"zero_overrun_fit": "measured_media_truth_v1",
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

// isVoiceAssignmentEquivalent checks if existing VoiceAssignment has identical speaker mappings and same-voice policy.
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
		if existingProfile.ID != newProfile.ID ||
			existingProfile.ProviderID != newProfile.ProviderID ||
			existingProfile.VoiceID != newProfile.VoiceID ||
			existingProfile.Language != newProfile.Language ||
			existingProfile.Gender != newProfile.Gender ||
			existingProfile.Pitch != newProfile.Pitch ||
			existingProfile.Speed != newProfile.Speed ||
			existingProfile.Timbre != newProfile.Timbre ||
			existingProfile.IsClone != newProfile.IsClone ||
			existingProfile.ReferenceAudioCAS != newProfile.ReferenceAudioCAS {
			return false
		}
	}
	return true
}

// isProviderEquivalent checks if two provider IDs match (allowing fake_ prefix normalization).
func isProviderEquivalent(id1, id2 string) bool {
	id1 = strings.ToLower(strings.TrimSpace(id1))
	id2 = strings.ToLower(strings.TrimSpace(id2))
	return id1 == id2 || strings.TrimPrefix(id1, "fake_") == strings.TrimPrefix(id2, "fake_")
}
