package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	"github.com/monet88/douyinie/internal/worker"
)

type SeparatorInvokeFunc func(ctx context.Context, p provider.Provider, req provider.SeparationRequest) (*provider.SeparationResult, error)

// AudioMixService orchestrates vocal separation into stems, soundtrack preservation planning,
// and deterministic audio mixing.
//
// Invariants (CapCap-derived, locked by #16 §5-§6, #18, #37):
//  1. Mixer purity: AudioMixService is a deterministic executor of accepted timing/mix plans,
//     not a timing-policy engine. It cannot alter source anchors, shift timings, or rescue overlong clips.
//  2. Mixer refusal: Mixer strictly REFUSES any candidate whose measured duration exceeds its slot
//     (measured end > slot end), never overlays across the next block, truncates words, or hides overlap.
//  3. Soundtrack preservation: Background music (BGM), sound effects (Foley/SFX), ambience, and
//     music-vocals/singing are preserved outside and through dialogue windows as separation/mix permits.
//  4. Dialogue-only suppression: Inside active speech windows, source dialogue is suppressed while
//     background stems remain preserved. Outside speech windows, original mix is preserved.
//  5. Zero-speech / no-dub bypass: Audio with no dub-eligible speech passes through cleanly without TTS injection.
//  6. Testable independently with fake accepted dub audio once SpeechBlocks are known.
type AudioMixService struct {
	db     *storage.DB
	cas    *cas.Store
	router *provider.Router

	// SeparatorInvoke executes one audio separation attempt.
	// When nil, router-backed invocation is used.
	SeparatorInvoke SeparatorInvokeFunc
}

// NewAudioMixService creates a new AudioMixService instance.
func NewAudioMixService(db *storage.DB, casStore *cas.Store) *AudioMixService {
	return &AudioMixService{
		db:  db,
		cas: casStore,
	}
}

// ConfigureRouter injects the provider router.
func (s *AudioMixService) ConfigureRouter(router *provider.Router) {
	s.router = router
}

// AudioSeparationInput defines the input parameters for audio stem separation.
type AudioSeparationInput struct {
	RunID            string                  `json:"run_id"`
	AssetID          string                  `json:"asset_id"`
	JobID            string                  `json:"job_id,omitempty"`
	ExecutionProfile domain.ExecutionProfile `json:"execution_profile,omitempty"`
}

// AudioMixInput defines the input parameters for deterministic audio mixing.
type AudioMixInput struct {
	RunID                 string                  `json:"run_id"`
	AssetID               string                  `json:"asset_id"`
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

// SeparateAudio isolates source vocals and background audio into immutable AudioStemArtifacts.
func (s *AudioMixService) SeparateAudio(ctx context.Context, input AudioSeparationInput) (*domain.AudioStemArtifacts, error) {
	if strings.TrimSpace(input.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if s.db == nil {
		return nil, errors.New("database is not configured")
	}

	if _, err := s.db.GetSourceAsset(ctx, input.AssetID); err != nil {
		return nil, fmt.Errorf("load source asset: %w", err)
	}

	report, err := s.db.GetPreflightReport(ctx, input.AssetID)
	if err != nil {
		return nil, fmt.Errorf("preflight report required for audio separation: %w", err)
	}
	if report.NormalizedAudioSHA256 == "" || report.NormalizedAudioCASPath == "" {
		return nil, fmt.Errorf("normalized audio missing from preflight report for asset %s", input.AssetID)
	}

	normPath := report.NormalizedAudioCASPath
	if _, err := os.Stat(normPath); err != nil && report.NormalizedAudioSHA256 != "" && s.cas != nil {
		if p, err := s.cas.ResolvePath(report.NormalizedAudioSHA256); err == nil {
			if _, err := os.Stat(p); err == nil {
				normPath = p
			}
		}
	}
	f, err := os.Open(normPath)
	if err != nil {
		return nil, fmt.Errorf("normalized audio artifact missing or unreadable at %s: %w", normPath, err)
	}
	_ = f.Close()

	rolePlan, _ := s.db.GetAudioRolePlan(ctx, input.AssetID)

	// If router is available, pre-route to check for cached artifact by provenance across eligible order
	sourceAudioRef := worker.ArtifactRef{
		SHA256: report.NormalizedAudioSHA256,
		Path:   normPath,
	}

	req := provider.SeparationRequest{
		AssetID:       input.AssetID,
		SourceAudio:   sourceAudioRef,
		AudioRolePlan: rolePlan,
	}

	var routeRes *provider.RouteResult
	routeReq := provider.RouteRequest{
		RunID:            input.RunID,
		Stage:            provider.TypeSeparator,
		Language:         "*",
		ExecutionProfile: input.ExecutionProfile,
	}

	if s.router != nil {
		if rRes, err := s.router.Route(ctx, routeReq); err == nil && rRes.SelectedProvider != nil {
			routeRes = rRes
			// Check candidates in policy-eligible order: SelectedProvider first, then FallbackOrdered
			eligibleProvs := append([]provider.Provider{rRes.SelectedProvider}, rRes.FallbackOrdered...)
			for _, p := range eligibleProvs {
				if p == nil {
					continue
				}
				// Verify provider is currently healthy and policy-allowed
				if p.PolicyState() != provider.PolicyAllowed || !p.IsHealthy() {
					continue
				}
				mName, mVer := p.ModelInfo()
				if expectedProvHash, err := domain.ComputeAudioStemsProvenanceHash(input.AssetID, p.ID(), mName, mVer); err == nil {
					if existingIdx, err := s.db.GetAudioStemsArtifactByProvenance(ctx, expectedProvHash); err == nil && existingIdx != nil && existingIdx.CASHash != "" {
						stemsReader, err := s.cas.Get(existingIdx.CASHash)
						if err == nil {
							defer stemsReader.Close()
							var cached domain.AudioStemArtifacts
							if err := json.NewDecoder(stemsReader).Decode(&cached); err == nil {
								cached.CASHash = existingIdx.CASHash
								return &cached, nil
							}
						}
					}
				}
			}
		}
	}
	var res *provider.SeparationResult
	if s.SeparatorInvoke != nil {
		res, err = s.SeparatorInvoke(ctx, nil, req)
		if err != nil {
			return nil, fmt.Errorf("invoke separator: %w", err)
		}
	} else if s.router != nil {
		if routeRes == nil {
			var err error
			routeRes, err = s.router.Route(ctx, routeReq)
			if err != nil {
				return nil, fmt.Errorf("route separator provider: %w", err)
			}
		}

		err = s.router.ExecuteRoutedWithRetry(ctx, routeReq, routeRes, report.NormalizedAudioSHA256, 3, func(p provider.Provider, attemptNum int) error {
			sepProv, ok := p.(provider.AudioSeparatorProvider)
			if !ok {
				return fmt.Errorf("provider %s does not implement AudioSeparatorProvider", p.ID())
			}
			out, err := sepProv.SeparateStems(ctx, req)
			if err != nil {
				return err
			}
			res = out
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("execute separator with retry: %w", err)
		}
	} else {
		// Default fallback to fake provider if router not set
		fakeProv := provider.NewFakeSeparatorProvider("fake_uvr_separator")
		res, err = fakeProv.SeparateStems(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("default separator fallback: %w", err)
		}
	}

	if res == nil || len(res.BackgroundWAV) == 0 {
		return nil, domain.ErrSeparatorFailed
	}

	// Store stems in CAS
	vocalsObj, err := s.cas.Put(bytes.NewReader(res.VocalsWAV))
	if err != nil {
		return nil, fmt.Errorf("store vocals in CAS: %w", err)
	}
	vocalsCASHash := vocalsObj.SHA256
	vocalsPath := vocalsObj.Path

	bgObj, err := s.cas.Put(bytes.NewReader(res.BackgroundWAV))
	if err != nil {
		return nil, fmt.Errorf("store background stem in CAS: %w", err)
	}
	bgCASHash := bgObj.SHA256
	bgPath := bgObj.Path

	durMs := res.DurationMs
	if durMs <= 0 {
		durMs, _ = media.ProbeWAVBytes(res.BackgroundWAV)
	}

	stems := []domain.AudioStem{
		{
			Type:         domain.StemTypeVocals,
			AudioCASHash: vocalsCASHash,
			AudioCASPath: vocalsPath,
			SampleRate:   res.SampleRate,
			Channels:     res.Channels,
			Format:       "wav",
			DurationMs:   durMs,
		},
		{
			Type:         domain.StemTypeBackground,
			AudioCASHash: bgCASHash,
			AudioCASPath: bgPath,
			SampleRate:   res.SampleRate,
			Channels:     res.Channels,
			Format:       "wav",
			DurationMs:   durMs,
		},
	}

	provHash, err := domain.ComputeAudioStemsProvenanceHash(input.AssetID, res.ProviderID, res.ModelName, res.ModelVersion)
	if err != nil {
		return nil, fmt.Errorf("compute audio stems provenance hash: %w", err)
	}

	stemArtifact := domain.AudioStemArtifacts{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.AudioStemsSchemaVersion,
		AssetID:        input.AssetID,
		ProviderID:     res.ProviderID,
		ModelName:      res.ModelName,
		ModelVersion:   res.ModelVersion,
		Stems:          stems,
		ProvenanceHash: provHash,
		CreatedAt:      time.Now().UTC(),
	}
	stemBytes, err := json.Marshal(stemArtifact)
	stemObj, err := s.cas.Put(bytes.NewReader(stemBytes))
	if err != nil {
		return nil, fmt.Errorf("store audio stems artifact in CAS: %w", err)
	}
	stemArtifact.CASHash = stemObj.SHA256

	// Save to SQLite
	err = s.db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:             stemArtifact.ID,
		AssetID:        stemArtifact.AssetID,
		ProviderID:     stemArtifact.ProviderID,
		ModelName:      stemArtifact.ModelName,
		ModelVersion:   stemArtifact.ModelVersion,
		CASHash:        stemArtifact.CASHash,
		ProvenanceHash: stemArtifact.ProvenanceHash,
		CreatedAt:      stemArtifact.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("persist audio stems artifact index: %w", err)
	}
	return &stemArtifact, nil
}

// MixAudio deterministically mixes localized speech and preserved background stems.
//
// Invariants (CapCap-derived, locked by #16 §5-§6, #18, #37):
// - Mixer purity: deterministic executor, no invented rescue policy, no anchor alteration.
// - Mixer refusal: strictly REFUSES candidates that violate accepted playback, lineage, waveform, or coverage evidence.
// - Preserves BGM, SFX, ambience, and music-vocal outside and through dialogue windows.
// - Suppresses source dialogue inside speech windows with smooth crossfades (15-30ms).
// unresolvedDubReason explains a dub-eligible mix that has no clip to place, naming the dub stage's own
// review outcome for the affected slots so the refusal carries the real overrun instead of a generic one.
func unresolvedDubReason(dubSegments *domain.DubSegmentsVariant, dubArtifactErr string) string {
	if dubSegments == nil {
		if dubArtifactErr != "" {
			return "dubbing is required but no dub segment artifact could be loaded: " + dubArtifactErr
		}
		return "dubbing is required but no dub segment artifact carries a candidate clip"
	}
	if len(dubSegments.Segments)+len(dubSegments.ReviewSegments) == 0 {
		return "dubbing is required but no dub segment artifact carries a candidate clip"
	}
	for _, rev := range dubSegments.ReviewSegments {
		if rev.SlotDurationMs > 0 && rev.MeasuredDurationMs > rev.SlotDurationMs {
			return fmt.Sprintf("dubbing is required but every candidate dub segment overran its immutable slot and none was accepted: segment %d measured %dms exceeds slot %dms (start: %dms, end: %dms)",
				rev.Index, rev.MeasuredDurationMs, rev.SlotDurationMs, rev.StartMs, rev.EndMs)
		}
	}
	unacceptedCount := 0
	for _, rev := range dubSegments.ReviewSegments {
		if rev.FitDecision != domain.FitActionAccept {
			unacceptedCount++
		}
	}
	return fmt.Sprintf("dubbing is required but no candidate dub segment could be placed: %d candidate(s) require review and %d lack an accepted fit",
		len(dubSegments.ReviewSegments), unacceptedCount)
}

func (s *AudioMixService) loadMixTranscript(casHash, assetID string) (*domain.TranscriptArtifact, error) {
	if strings.TrimSpace(casHash) == "" {
		return nil, errors.New("dub artifact does not pin transcript_artifact_cas")
	}
	r, err := s.cas.Get(casHash)
	if err != nil {
		return nil, fmt.Errorf("read pinned transcript %s: %w", casHash, err)
	}
	defer r.Close()
	var transcript domain.TranscriptArtifact
	if err := json.NewDecoder(r).Decode(&transcript); err != nil {
		return nil, fmt.Errorf("decode pinned transcript %s: %w", casHash, err)
	}
	if transcript.AssetID != "" && transcript.AssetID != assetID {
		return nil, fmt.Errorf("pinned transcript asset mismatch: %s != %s", transcript.AssetID, assetID)
	}
	return &transcript, nil
}

func eligibleSpeechBlocks(transcript *domain.TranscriptArtifact, rolePlan *domain.AudioRolePlan) map[int]domain.SpeechBlock {
	out := make(map[int]domain.SpeechBlock)
	if transcript == nil || rolePlan == nil {
		return out
	}
	for _, b := range transcript.SpeechBlocks {
		if !domain.IsSpeechBlock(b) {
			continue
		}
		hasDialogue := false
		blockedByProtectedVocal := false
		for _, seg := range rolePlan.Segments {
			if b.StartMs >= seg.EndMs || b.EndMs <= seg.StartMs {
				continue
			}
			switch seg.Role {
			case domain.AudioRoleNarrationDialogue:
				hasDialogue = true
			case domain.AudioRoleSingingMusicVocal, domain.AudioRoleUncertain:
				blockedByProtectedVocal = true
			}
		}
		if hasDialogue && !blockedByProtectedVocal {
			out[b.Index] = b
		}
	}
	return out
}

func (s *AudioMixService) loadDubSpeechClip(seg domain.DubSegment) (media.DubSpeechClip, error) {
	if seg.AudioSHA256 == "" && seg.AudioCASPath == "" {
		return media.DubSpeechClip{}, errors.New("neither audio_sha256 nor audio_cas_path provided")
	}
	var audioBytes []byte
	var readErr error
	if seg.AudioSHA256 != "" {
		if s.cas != nil {
			r, err := s.cas.Get(seg.AudioSHA256)
			if err == nil {
				audioBytes, readErr = io.ReadAll(r)
				_ = r.Close()
				if readErr != nil {
					return media.DubSpeechClip{}, fmt.Errorf("read audio from CAS (%s): %w", seg.AudioSHA256, readErr)
				}
			} else {
				readErr = err
			}
		} else {
			readErr = errors.New("CAS store is not configured")
		}
	}
	if audioBytes == nil && seg.AudioCASPath != "" {
		f, err := os.Open(seg.AudioCASPath)
		if err == nil {
			audioBytes, readErr = io.ReadAll(f)
			_ = f.Close()
			if readErr != nil {
				return media.DubSpeechClip{}, fmt.Errorf("read audio from path (%s): %w", seg.AudioCASPath, readErr)
			}
		} else {
			if readErr != nil {
				return media.DubSpeechClip{}, fmt.Errorf("read audio from CAS (%s: %v) and path (%s: %w)", seg.AudioSHA256, readErr, seg.AudioCASPath, err)
			}
			return media.DubSpeechClip{}, fmt.Errorf("read audio from path (%s): %w", seg.AudioCASPath, err)
		}
	}
	if audioBytes == nil {
		return media.DubSpeechClip{}, fmt.Errorf("read audio from CAS (%s): %w", seg.AudioSHA256, readErr)
	}

	clipSamples, clipHeader, err := media.ExtractPCM16Samples(audioBytes)
	if err != nil {
		return media.DubSpeechClip{}, fmt.Errorf("decode PCM16 samples: %w", err)
	}
	if len(clipSamples) == 0 {
		return media.DubSpeechClip{}, errors.New("decoded zero PCM16 samples")
	}
	return media.DubSpeechClip{
		StartMs:    seg.StartMs,
		SampleRate: int(clipHeader.SampleRate),
		Channels:   int(clipHeader.NumChannels),
		Samples:    clipSamples,
	}, nil
}

func (s *AudioMixService) MixAudio(ctx context.Context, input AudioMixInput) (*domain.DubMixArtifact, error) {
	if strings.TrimSpace(input.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if !domain.IsValidTargetLanguage(input.TargetLanguage) {
		return nil, domain.ErrInvalidTargetLanguage
	}

	asset, err := s.db.GetSourceAsset(ctx, input.AssetID)
	if err != nil {
		return nil, fmt.Errorf("load source asset: %w", err)
	}

	// 1. Load AudioRolePlan (required to determine dialogue/narration vs zero-speech bypass)
	rolePlan, err := s.db.GetAudioRolePlan(ctx, input.AssetID)
	if err != nil || rolePlan == nil {
		return nil, domain.ErrAudioRolePlanRequired
	}

	// Check if there are dub-eligible segments in the role plan
	hasDubEligibleSpeech := domain.IsDubEligible(rolePlan)

	// 2. If valid rolePlan has no dub-eligible speech, perform bitstream-exact passthrough
	if !hasDubEligibleSpeech {
		report, err := s.db.GetPreflightReport(ctx, input.AssetID)
		if err != nil {
			return nil, fmt.Errorf("preflight report required for no-dub audio passthrough: %w", err)
		}
		if report.NormalizedAudioSHA256 == "" || report.NormalizedAudioCASPath == "" {
			return nil, fmt.Errorf("normalized audio missing from preflight report for asset %s", input.AssetID)
		}
		if _, err := os.Stat(report.NormalizedAudioCASPath); err != nil {
			return nil, fmt.Errorf("normalized audio CAS artifact missing at %s: %w", report.NormalizedAudioCASPath, err)
		}

		durMs := report.DurationMs
		if durMs <= 0 {
			durMs = int64(report.DurationSec * 1000)
		}
		sampleRate := report.AudioSampleRate
		if sampleRate <= 0 {
			sampleRate = 16000
		}
		channels := report.AudioChannels
		if channels <= 0 {
			channels = 1
		}

		crossfadeMs := input.CrossfadeDurationMs
		if crossfadeMs <= 0 {
			crossfadeMs = 25
		}
		preservationPlan := domain.SoundtrackPreservationPlan{
			AssetID:             input.AssetID,
			PreserveSinging:     true,
			PreserveSFX:         true,
			PreserveAmbience:    true,
			CrossfadeDurationMs: crossfadeMs,
			DuckingGainDb:       input.DuckingGainDb,
			SpeechWindows:       make([]domain.PreservationWindow, 0),
			SingingWindows:      make([]domain.PreservationWindow, 0),
		}

		provHash, _ := domain.ComputeDubMixProvenanceHash(
			input.AssetID,
			input.TargetLanguage,
			"",
			"",
			preservationPlan,
		)

		mixArtifact := domain.DubMixArtifact{
			ID:                  uuid.NewString(),
			SchemaVersion:       domain.DubMixSchemaVersion,
			AssetID:             input.AssetID,
			RunID:               input.RunID,
			JobID:               input.JobID,
			TargetLanguage:      input.TargetLanguage,
			AudioCASHash:        report.NormalizedAudioSHA256,
			AudioCASPath:        report.NormalizedAudioCASPath,
			SampleRate:          sampleRate,
			Channels:            channels,
			Format:              "wav",
			DurationMs:          durMs,
			DubSegmentsCAS:      "",
			AudioStemsCAS:       "",
			PreservationPlan:    preservationPlan,
			DialogueSuppressed:  false,
			SoundtrackPreserved: true,
			ProvenanceHash:      provHash,
			OverallStatus:       "PASS",
			CreatedAt:           time.Now().UTC(),
		}

		mixBytes, err := json.Marshal(mixArtifact)
		if err != nil {
			return nil, fmt.Errorf("marshal dub mix artifact: %w", err)
		}
		mixObj, err := s.cas.Put(bytes.NewReader(mixBytes))
		if err != nil {
			return nil, fmt.Errorf("store dub mix artifact in CAS: %w", err)
		}
		mixArtifact.CASHash = mixObj.SHA256

		err = s.db.SaveDubMixArtifactIndex(ctx, storage.DubMixArtifactIndex{
			ID:             mixArtifact.ID,
			AssetID:        mixArtifact.AssetID,
			RunID:          mixArtifact.RunID,
			JobID:          mixArtifact.JobID,
			TargetLanguage: mixArtifact.TargetLanguage,
			CASHash:        mixArtifact.CASHash,
			ProvenanceHash: mixArtifact.ProvenanceHash,
			OverallStatus:  mixArtifact.OverallStatus,
			CreatedAt:      mixArtifact.CreatedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("persist dub mix artifact index: %w", err)
		}
		return &mixArtifact, nil
	}

	// 2. Load or compute AudioStems (for dub-eligible audio mixing)
	var stemsArtifact *domain.AudioStemArtifacts
	var stemsCASRef string
	if input.AudioStemsCAS != "" {
		stemsCASRef = input.AudioStemsCAS
		r, err := s.cas.Get(input.AudioStemsCAS)
		if err == nil {
			defer r.Close()
			var a domain.AudioStemArtifacts
			if err := json.NewDecoder(r).Decode(&a); err == nil {
				stemsArtifact = &a
				stemsArtifact.CASHash = stemsCASRef
			}
		}
		if stemsArtifact == nil {
			return nil, fmt.Errorf("explicit audio stems CAS %s is unreadable or invalid", input.AudioStemsCAS)
		}
	}
	if stemsArtifact == nil {
		// Run separation
		stemsArtifact, err = s.SeparateAudio(ctx, AudioSeparationInput{
			RunID:            input.RunID,
			AssetID:          input.AssetID,
			JobID:            input.JobID,
			ExecutionProfile: input.ExecutionProfile,
		})
		if err != nil {
			return nil, fmt.Errorf("separate audio stems: %w", err)
		}
	}
	// 3. Load DubSegments (if speech is present and dubbing required)
	var dubSegments *domain.DubSegmentsVariant
	var dubSegmentsCASRef string
	// dubArtifactErr names why the dub artifact could not be consumed. An artifact that exists but is
	// unreadable must not fall through to a mix that suppresses the source dialogue with nothing to place;
	// the coverage gate below carries this reason into the refusal.
	var dubArtifactErr string
	if hasDubEligibleSpeech {
		if input.DubSegmentsCAS == "" {
			dubArtifactErr = "dub_segments_cas is required for dub-eligible mixing; latest indexed artifacts are not accepted"
		}
		if input.DubSegmentsCAS != "" {
			dubSegmentsCASRef = input.DubSegmentsCAS
			r, err := s.cas.Get(input.DubSegmentsCAS)
			if err == nil {
				defer r.Close()
				var d domain.DubSegmentsVariant
				if err := json.NewDecoder(r).Decode(&d); err == nil {
					dubSegments = &d
					dubSegments.CASHash = dubSegmentsCASRef
				} else {
					dubArtifactErr = fmt.Sprintf("dub segments CAS %s could not be decoded: %v", dubSegmentsCASRef, err)
				}
			} else {
				dubArtifactErr = fmt.Sprintf("dub segments CAS %s is unreadable: %v", dubSegmentsCASRef, err)
			}
		}
	}

	// 4. Build SoundtrackPreservationPlan
	crossfadeMs := input.CrossfadeDurationMs
	if crossfadeMs <= 0 {
		crossfadeMs = 25 // 25ms canonical default (within 15-30ms envelope)
	}

	preservationPlan := domain.SoundtrackPreservationPlan{
		AssetID:             input.AssetID,
		PreserveSinging:     true, // Canonical invariant: singing vocals preserved untouched
		PreserveSFX:         true,
		PreserveAmbience:    true,
		CrossfadeDurationMs: crossfadeMs,
		DuckingGainDb:       input.DuckingGainDb,
		SpeechWindows:       make([]domain.PreservationWindow, 0),
		SingingWindows:      make([]domain.PreservationWindow, 0),
	}

	if rolePlan != nil {
		for _, seg := range rolePlan.Segments {
			if seg.Role == domain.AudioRoleSingingMusicVocal {
				preservationPlan.SingingWindows = append(preservationPlan.SingingWindows, domain.PreservationWindow{
					StartMs: seg.StartMs,
					EndMs:   seg.EndMs,
					Action:  "preserve_music_vocal",
				})
			}
		}
	}

	// 5. Check Mixer Invariants: Overrun Refusal, Dub Coverage & Timing Purity.
	//
	// A mix that suppresses the source dialogue MUST place the replacement speech. A dub stage whose every
	// candidate still overruns the immutable slot leaves Segments empty and parks the candidates in
	// ReviewSegments; mixing that anyway stripped the source dialogue and shipped a video with no voice at all
	// while reporting PASS (live evidence: runs 264aecaf and 4f86657f, whose mixes came out bit-identical to
	// the background stem under a full-length suppress_dialogue window).
	refuse := func(reason string) (*domain.DubMixArtifact, error) {
		refusedArtifact := domain.DubMixArtifact{
			ID:                  uuid.NewString(),
			SchemaVersion:       domain.DubMixSchemaVersion,
			AssetID:             input.AssetID,
			RunID:               input.RunID,
			JobID:               input.JobID,
			TargetLanguage:      input.TargetLanguage,
			DubSegmentsCAS:      dubSegmentsCASRef,
			AudioStemsCAS:       stemsArtifact.CASHash,
			PreservationPlan:    preservationPlan,
			DialogueSuppressed:  false,
			SoundtrackPreserved: true,
			OverallStatus:       "REFUSED",
			RefusalReason:       reason,
			CreatedAt:           time.Now().UTC(),
		}
		provHash, _ := domain.ComputeDubMixProvenanceHash(input.AssetID, input.TargetLanguage, dubSegmentsCASRef, stemsArtifact.CASHash, preservationPlan)
		refusedArtifact.ProvenanceHash = provHash

		refusedBytes, _ := json.Marshal(refusedArtifact)
		refusedObj, _ := s.cas.Put(bytes.NewReader(refusedBytes))
		refusedArtifact.CASHash = refusedObj.SHA256

		err := s.db.SaveDubMixArtifactIndex(ctx, storage.DubMixArtifactIndex{
			ID:             refusedArtifact.ID,
			AssetID:        refusedArtifact.AssetID,
			RunID:          refusedArtifact.RunID,
			JobID:          refusedArtifact.JobID,
			TargetLanguage: refusedArtifact.TargetLanguage,
			CASHash:        refusedArtifact.CASHash,
			ProvenanceHash: refusedArtifact.ProvenanceHash,
			OverallStatus:  refusedArtifact.OverallStatus,
			RefusalReason:  refusedArtifact.RefusalReason,
			CreatedAt:      refusedArtifact.CreatedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("persist refused dub mix artifact index: %w", err)
		}
		return &refusedArtifact, fmt.Errorf("%w: %s", domain.ErrMixerOverrunRefused, reason)
	}
	if hasDubEligibleSpeech && dubSegments == nil {
		return refuse(dubArtifactErr)
	}
	if hasDubEligibleSpeech {
		if rolePlan.CASHash == "" {
			return refuse("audio role plan is not pinned to CAS")
		}
		if err := verifyPinnedAudioRolePlan(s.cas, rolePlan, rolePlan.CASHash); err != nil {
			return refuse(err.Error())
		}
	}

	speechClips := make([]media.DubSpeechClip, 0)
	speechClipSegments := make([]domain.DubSegment, 0)
	if dubSegments != nil {
		if dubSegments.SchemaVersion != domain.DubSegmentsSchemaVersion || dubSegments.AssetID != input.AssetID || !strings.EqualFold(dubSegments.TargetLanguage, input.TargetLanguage) {
			return refuse(fmt.Sprintf("dub artifact lineage mismatch: schema=%d asset=%s language=%s", dubSegments.SchemaVersion, dubSegments.AssetID, dubSegments.TargetLanguage))
		}
		if dubSegments.OverallStatus != "PASS" {
			reason := unresolvedDubReason(dubSegments, "")
			if reason == "" {
				reason = fmt.Sprintf("dub artifact is not mixable: overall_status=%q", dubSegments.OverallStatus)
			}
			return refuse(reason)
		}
		if input.RunID != "" && dubSegments.RunID != input.RunID {
			return refuse(fmt.Sprintf("dub artifact run lineage mismatch: %s != %s", dubSegments.RunID, input.RunID))
		}
		if input.RunID != "" {
			dubScriptIdx, err := s.db.GetDubScriptVariantIndexByRun(ctx, input.RunID)
			if err != nil || dubScriptIdx == nil || dubScriptIdx.CASHash == "" || dubSegments.DubScriptVariantCAS != dubScriptIdx.CASHash {
				return refuse(fmt.Sprintf("dub script lineage mismatch: dub=%q current=%q", dubSegments.DubScriptVariantCAS, func() string {
					if dubScriptIdx == nil {
						return ""
					}
					return dubScriptIdx.CASHash
				}()))
			}
			voiceIdx, err := s.db.GetVoiceAssignmentIndexByRunID(ctx, input.RunID)
			if err != nil || voiceIdx == nil || voiceIdx.CASHash == "" || dubSegments.VoiceAssignmentCAS != voiceIdx.CASHash {
				return refuse(fmt.Sprintf("voice assignment lineage mismatch: dub=%q current=%q", dubSegments.VoiceAssignmentCAS, func() string {
					if voiceIdx == nil {
						return ""
					}
					return voiceIdx.CASHash
				}()))
			}
		}
		if err := s.validatePinnedDubbingLineage(input, dubSegments); err != nil {
			return refuse(err.Error())
		}
		if dubSegments.AudioRolePlanCAS == "" || rolePlan.CASHash == "" || dubSegments.AudioRolePlanCAS != rolePlan.CASHash {
			return refuse(fmt.Sprintf("audio role plan lineage mismatch: dub=%q current=%q", dubSegments.AudioRolePlanCAS, rolePlan.CASHash))
		}
		if strings.TrimSpace(dubSegments.FitPolicyID) == "" {
			return refuse("dub artifact is missing frozen fit policy identity")
		}
		fitByIndex := make(map[int]domain.DubbingFitPlan, len(dubSegments.FitPlans))
		for _, fp := range dubSegments.FitPlans {
			if _, exists := fitByIndex[fp.SegmentIndex]; exists {
				return refuse(fmt.Sprintf("duplicate fit evidence for segment %d", fp.SegmentIndex))
			}
			fitByIndex[fp.SegmentIndex] = fp
		}
		transcript, err := s.loadMixTranscript(dubSegments.TranscriptArtifactCAS, input.AssetID)
		if err != nil {
			return refuse(err.Error())
		}
		eligible := eligibleSpeechBlocks(transcript, rolePlan)
		if len(eligible) == 0 {
			return refuse("pinned transcript contains no dub-eligible source speech members")
		}
		// Suppression follows the exact eligible canonical SpeechBlock union, never
		// the wider AudioRolePlan dialogue ranges and never a borrowed/group envelope.
		preservationPlan.SpeechWindows = preservationPlan.SpeechWindows[:0]
		for _, block := range transcript.SpeechBlocks {
			if !domain.IsSpeechBlock(block) {
				continue
			}
			if eligibleBlock, ok := eligible[block.Index]; ok && eligibleBlock.StartMs == block.StartMs && eligibleBlock.EndMs == block.EndMs {
				preservationPlan.SpeechWindows = append(preservationPlan.SpeechWindows, domain.PreservationWindow{
					StartMs: block.StartMs, EndMs: block.EndMs, Action: "suppress_dialogue",
				})
			}
		}
		currentFit := NewFitController()
		currentPolicyID := currentFit.policyID()
		if currentPolicyID == "" || dubSegments.FitPolicyID != currentPolicyID {
			return refuse(fmt.Sprintf("dub artifact fit policy mismatch: dub=%q current=%q", dubSegments.FitPolicyID, currentPolicyID))
		}
		covered := make(map[int]bool, len(eligible))
		for _, seg := range dubSegments.Segments {
			if seg.RequiresReview || seg.FitDecision != domain.FitActionAccept {
				return refuse(fmt.Sprintf("segment %d is not an accepted mix candidate", seg.Index))
			}
			fp, ok := fitByIndex[seg.Index]
			if !ok || fp.Decision != domain.FitActionAccept || fp.FitPolicyID != dubSegments.FitPolicyID || fp.DubPlaybackEndMs != seg.DubPlaybackEndMs || fp.EffectiveReserveMs != seg.EffectiveReserveMs || !slices.Equal(fp.SpeechBlockIndices, seg.SpeechBlockIndices) {
				return refuse(fmt.Sprintf("segment %d fit evidence does not match selected playback contract", seg.Index))
			}
			if len(seg.SpeechBlockIndices) == 0 {
				return refuse(fmt.Sprintf("segment %d has no canonical source membership", seg.Index))
			}
			minStart, maxEnd := int64(0), int64(0)
			var lastBlock domain.SpeechBlock
			for _, member := range seg.SpeechBlockIndices {
				block, ok := eligible[member]
				if !ok {
					return refuse(fmt.Sprintf("segment %d references foreign or non-dub-eligible source member %d", seg.Index, member))
				}
				if covered[member] {
					return refuse(fmt.Sprintf("source member %d is covered more than once", member))
				}
				covered[member] = true
				if minStart == 0 || block.StartMs < minStart {
					minStart = block.StartMs
				}
				if block.EndMs > maxEnd {
					maxEnd = block.EndMs
					lastBlock = block
				}
			}
			if seg.StartMs != minStart || seg.EndMs != maxEnd {
				return refuse(fmt.Sprintf("segment %d source envelope %d-%d does not match member envelope %d-%d", seg.Index, seg.StartMs, seg.EndMs, minStart, maxEnd))
			}
			if seg.DubPlaybackEndMs < seg.EndMs || seg.DubPlaybackEndMs <= seg.StartMs {
				return refuse(fmt.Sprintf("segment %d has invalid playback end %d", seg.Index, seg.DubPlaybackEndMs))
			}
			nextBoundary, err := playbackBoundaryForBlock(lastBlock.Index, lastBlock.StartMs, lastBlock.EndMs, transcript, rolePlan)
			if err != nil {
				return refuse(err.Error())
			}
			expectedEnd, expectedReserve, expectedPolicyID := currentFit.ResolvePlaybackWindow(seg.EndMs, nextBoundary)
			if expectedPolicyID != dubSegments.FitPolicyID || seg.DubPlaybackEndMs != expectedEnd || seg.EffectiveReserveMs != expectedReserve {
				return refuse(fmt.Sprintf("segment %d playback policy evidence mismatch: end=%d/%d reserve=%d/%d", seg.Index, seg.DubPlaybackEndMs, expectedEnd, seg.EffectiveReserveMs, expectedReserve))
			}
			clip, err := s.loadDubSpeechClip(seg)
			if err != nil {
				return refuse(fmt.Sprintf("segment %d audio load or decode failure: %v", seg.Index, err))
			}
			if clip.SampleRate <= 0 || clip.Channels <= 0 || len(clip.Samples)%clip.Channels != 0 {
				return refuse(fmt.Sprintf("segment %d decoded waveform has invalid format", seg.Index))
			}
			frames := int64(len(clip.Samples) / clip.Channels)
			allowedMs := seg.DubPlaybackEndMs - seg.StartMs
			if frames*1000 > allowedMs*int64(clip.SampleRate) {
				return refuse(fmt.Sprintf("segment %d decoded waveform exceeds playback window", seg.Index))
			}
			speechClips = append(speechClips, clip)
			speechClipSegments = append(speechClipSegments, seg)
		}
		for member := range eligible {
			if !covered[member] {
				return refuse(fmt.Sprintf("dub-eligible source member %d has no accepted replacement clip", member))
			}
		}
	}
	// The dub artifact this mix consumed is what proves the dub lane ran for these slots: a run whose dub
	// stage produced a variant with no placeable clip (every candidate parked for review) must not suppress
	// the source dialogue and ship the silence. A mix with no dub artifact at all is left to its own
	// passthrough contract, unchanged.
	if hasDubEligibleSpeech && len(speechClips) == 0 && (dubSegments != nil || dubSegmentsCASRef != "") {
		return refuse(unresolvedDubReason(dubSegments, dubArtifactErr))
	}

	// 6. Extract background and vocal stems audio
	var bgStem, vocalsStem domain.AudioStem
	for _, stem := range stemsArtifact.Stems {
		if stem.Type == domain.StemTypeBackground {
			bgStem = stem
		} else if stem.Type == domain.StemTypeVocals {
			vocalsStem = stem
		}
	}
	if bgStem.AudioCASHash == "" {
		return nil, fmt.Errorf("%w: missing required background stem in audio stems artifact", domain.ErrSoundtrackPreservationFailed)
	}

	bgReader, err := s.cas.Get(bgStem.AudioCASHash)
	if err != nil {
		return nil, fmt.Errorf("%w: read background stem from CAS (%s): %v", domain.ErrSoundtrackPreservationFailed, bgStem.AudioCASHash, err)
	}
	defer bgReader.Close()
	bgData, err := io.ReadAll(bgReader)
	if err != nil {
		return nil, fmt.Errorf("%w: read background stem bytes: %v", domain.ErrSoundtrackPreservationFailed, err)
	}

	bgSamples, bgHeader, err := media.ExtractPCM16Samples(bgData)
	if err != nil {
		return nil, fmt.Errorf("%w: extract background PCM16 samples: %v", domain.ErrSoundtrackPreservationFailed, err)
	}

	sampleRate := int(bgHeader.SampleRate)
	channels := int(bgHeader.NumChannels)
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("%w: invalid background stem format: sample rate %d, channels %d", domain.ErrSoundtrackPreservationFailed, sampleRate, channels)
	}
	if len(bgSamples)%channels != 0 {
		return nil, fmt.Errorf("%w: background stem sample count is not channel aligned", domain.ErrSoundtrackPreservationFailed)
	}
	bgFrames := int64(len(bgSamples) / channels)
	order := make([]int, len(speechClips))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return speechClipSegments[order[i]].StartMs < speechClipSegments[order[j]].StartMs
	})
	for pos, idx := range order {
		clip := speechClips[idx]
		seg := speechClipSegments[idx]
		frames := int64(len(clip.Samples) / clip.Channels)
		// Compare exact rational sample extents instead of millisecond-rounded metadata.
		clipEndNumerator := (seg.StartMs*int64(clip.SampleRate) + frames*1000) * int64(sampleRate)
		mediaEndNumerator := bgFrames * int64(clip.SampleRate) * 1000
		if clipEndNumerator > mediaEndNumerator {
			return refuse(fmt.Sprintf("segment %d decoded waveform extends past source media end", seg.Index))
		}
		if pos+1 < len(order) {
			next := speechClipSegments[order[pos+1]]
			if seg.StartMs*int64(clip.SampleRate)+frames*1000 > next.StartMs*int64(clip.SampleRate) {
				return refuse(fmt.Sprintf("segment %d decoded waveform collides with localized segment %d", seg.Index, next.Index))
			}
		}
	}

	var vocalsSamples []int16
	if vocalsStem.AudioCASHash != "" {
		vReader, err := s.cas.Get(vocalsStem.AudioCASHash)
		if err != nil {
			return nil, fmt.Errorf("%w: read vocals stem from CAS (%s): %v", domain.ErrSoundtrackPreservationFailed, vocalsStem.AudioCASHash, err)
		}
		defer vReader.Close()
		vData, err := io.ReadAll(vReader)
		if err != nil {
			return nil, fmt.Errorf("%w: read vocals stem bytes: %v", domain.ErrSoundtrackPreservationFailed, err)
		}
		vSamples, vHeader, err := media.ExtractPCM16Samples(vData)
		if err != nil {
			return nil, fmt.Errorf("%w: extract vocals PCM16 samples: %v", domain.ErrSoundtrackPreservationFailed, err)
		}
		// Resample/match vocals to background rate/channels if needed
		if int(vHeader.SampleRate) != sampleRate || int(vHeader.NumChannels) != channels {
			vSamples = media.ResamplePCM16(vSamples, int(vHeader.SampleRate), int(vHeader.NumChannels), sampleRate, channels)
		}
		vocalsSamples = vSamples
	}

	// 7. Speech clips were loaded, validated, and accounted in step 5 (speechClips)

	// Prepare suppression windows
	suppressIntervals := make([]media.PreservationWindowInterval, len(preservationPlan.SpeechWindows))
	for i, w := range preservationPlan.SpeechWindows {
		suppressIntervals[i] = media.PreservationWindowInterval{
			StartMs: w.StartMs,
			EndMs:   w.EndMs,
			Action:  w.Action,
		}
	}

	// 8. Execute deterministic PCM mixing
	mixedSamples := media.MixPCM16Stems(
		bgSamples,
		vocalsSamples,
		sampleRate,
		channels,
		speechClips,
		suppressIntervals,
		crossfadeMs,
		input.DuckingGainDb,
	)
	mixedWAV := media.EncodePCM16Samples(mixedSamples, sampleRate, channels)
	verifiedMixed, mixedHeader, err := media.ExtractPCM16Samples(mixedWAV)
	if err != nil || int(mixedHeader.SampleRate) != sampleRate || int(mixedHeader.NumChannels) != channels || len(verifiedMixed) != len(mixedSamples) {
		return nil, fmt.Errorf("%w: mixed output format/sample extent verification failed", domain.ErrSoundtrackPreservationFailed)
	}
	mixedDurationMs, _ := media.ProbeWAVBytes(mixedWAV)
	if mixedDurationMs <= 0 {
		mixedDurationMs = bgHeader.DurationMs
	}

	// Store mixed audio in CAS
	mixedAudioObj, err := s.cas.Put(bytes.NewReader(mixedWAV))
	if err != nil {
		return nil, fmt.Errorf("store mixed audio in CAS: %w", err)
	}
	mixedAudioCASHash := mixedAudioObj.SHA256
	mixedAudioCASPath := mixedAudioObj.Path

	// 9. Build DubMixArtifact
	dubSegmentsCAS := ""
	if dubSegments != nil {
		dubSegmentsCAS = dubSegments.CASHash
	}
	provHash, _ := domain.ComputeDubMixProvenanceHash(
		input.AssetID,
		input.TargetLanguage,
		dubSegmentsCAS,
		stemsArtifact.CASHash,
		preservationPlan,
	)

	mixArtifact := domain.DubMixArtifact{
		ID:                  uuid.NewString(),
		SchemaVersion:       domain.DubMixSchemaVersion,
		AssetID:             input.AssetID,
		RunID:               input.RunID,
		JobID:               input.JobID,
		TargetLanguage:      input.TargetLanguage,
		AudioCASHash:        mixedAudioCASHash,
		AudioCASPath:        mixedAudioCASPath,
		SampleRate:          sampleRate,
		Channels:            channels,
		Format:              "wav",
		DurationMs:          mixedDurationMs,
		DubSegmentsCAS:      dubSegmentsCAS,
		AudioStemsCAS:       stemsArtifact.CASHash,
		PreservationPlan:    preservationPlan,
		DialogueSuppressed:  len(suppressIntervals) > 0,
		SoundtrackPreserved: true,
		ProvenanceHash:      provHash,
		OverallStatus:       "PASS",
		CreatedAt:           time.Now().UTC(),
	}

	mixBytes, err := json.Marshal(mixArtifact)
	if err != nil {
		return nil, fmt.Errorf("marshal dub mix artifact: %w", err)
	}
	mixObj, err := s.cas.Put(bytes.NewReader(mixBytes))
	if err != nil {
		return nil, fmt.Errorf("store dub mix artifact in CAS: %w", err)
	}
	mixArtifact.CASHash = mixObj.SHA256

	// Save to SQLite
	err = s.db.SaveDubMixArtifactIndex(ctx, storage.DubMixArtifactIndex{
		ID:             mixArtifact.ID,
		AssetID:        mixArtifact.AssetID,
		RunID:          mixArtifact.RunID,
		JobID:          mixArtifact.JobID,
		TargetLanguage: mixArtifact.TargetLanguage,
		CASHash:        mixArtifact.CASHash,
		ProvenanceHash: mixArtifact.ProvenanceHash,
		OverallStatus:  mixArtifact.OverallStatus,
		CreatedAt:      mixArtifact.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("persist dub mix artifact index: %w", err)
	}
	_ = asset
	return &mixArtifact, nil
}

func (s *AudioMixService) validatePinnedDubbingLineage(input AudioMixInput, dubSegments *domain.DubSegmentsVariant) error {
	if s.cas == nil || dubSegments == nil {
		return errors.New("pinned dubbing lineage requires CAS")
	}
	if strings.TrimSpace(dubSegments.DubScriptVariantCAS) == "" || strings.TrimSpace(dubSegments.VoiceAssignmentCAS) == "" {
		return errors.New("dub artifact is missing pinned script or voice assignment lineage")
	}

	dsRC, err := s.cas.Get(dubSegments.DubScriptVariantCAS)
	if err != nil {
		return fmt.Errorf("read pinned dub script %s: %w", dubSegments.DubScriptVariantCAS, err)
	}
	var dubScript domain.DubScriptVariant
	decodeErr := json.NewDecoder(dsRC).Decode(&dubScript)
	dsRC.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode pinned dub script %s: %w", dubSegments.DubScriptVariantCAS, decodeErr)
	}
	if dubScript.SchemaVersion != domain.DubScriptSchemaVersion || dubScript.AssetID != input.AssetID || !strings.EqualFold(dubScript.TargetLanguage, input.TargetLanguage) {
		return fmt.Errorf("pinned dub script lineage mismatch: schema=%d asset=%s language=%s", dubScript.SchemaVersion, dubScript.AssetID, dubScript.TargetLanguage)
	}
	if input.RunID != "" && dubScript.RunID != input.RunID {
		return fmt.Errorf("pinned dub script run lineage mismatch: %s != %s", dubScript.RunID, input.RunID)
	}

	vaRC, err := s.cas.Get(dubSegments.VoiceAssignmentCAS)
	if err != nil {
		return fmt.Errorf("read pinned voice assignment %s: %w", dubSegments.VoiceAssignmentCAS, err)
	}
	var voiceAssignment domain.VoiceAssignment
	decodeErr = json.NewDecoder(vaRC).Decode(&voiceAssignment)
	vaRC.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode pinned voice assignment %s: %w", dubSegments.VoiceAssignmentCAS, decodeErr)
	}
	if voiceAssignment.SchemaVersion != domain.VoiceAssignmentSchemaVersion || voiceAssignment.AssetID != input.AssetID || !strings.EqualFold(voiceAssignment.TargetLanguage, input.TargetLanguage) {
		return fmt.Errorf("pinned voice assignment lineage mismatch: schema=%d asset=%s language=%s", voiceAssignment.SchemaVersion, voiceAssignment.AssetID, voiceAssignment.TargetLanguage)
	}
	if input.RunID != "" && voiceAssignment.RunID != input.RunID {
		return fmt.Errorf("pinned voice assignment run lineage mismatch: %s != %s", voiceAssignment.RunID, input.RunID)
	}
	if voiceAssignment.DubScriptVariantCAS != dubSegments.DubScriptVariantCAS {
		return fmt.Errorf("voice assignment dub script lineage mismatch: voice=%q dub=%q", voiceAssignment.DubScriptVariantCAS, dubSegments.DubScriptVariantCAS)
	}
	if voiceAssignment.TranscriptArtifactCAS != dubSegments.TranscriptArtifactCAS {
		return fmt.Errorf("voice assignment transcript lineage mismatch: voice=%q dub=%q", voiceAssignment.TranscriptArtifactCAS, dubSegments.TranscriptArtifactCAS)
	}
	return nil
}
