package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/storage"
)

const (
	DefaultAcousticProviderID   = "builtin_acoustic_analyzer"
	DefaultAcousticModelName    = "acoustic_signal_classifier"
	DefaultAcousticModelVersion = "v1.0"
)

// AudioRolePlanInput defines the input parameters for automatic AudioRolePlan generation.
type AudioRolePlanInput struct {
	AssetID          string                  `json:"asset_id"`
	RunID            string                  `json:"run_id,omitempty"`
	JobID            string                  `json:"job_id,omitempty"`
	ExecutionProfile domain.ExecutionProfile `json:"execution_profile,omitempty"`
}

// AudioRoleAnalysisRequest represents the evidence bundle provided to an AudioRoleAnalyzer.
type AudioRoleAnalysisRequest struct {
	AssetID          string
	RunID            string
	DurationMs       int64
	SourceAudioPath  string
	VocalsPath       string
	BackgroundPath   string
	ExecutionProfile domain.ExecutionProfile
}

// AudioRoleAnalysisResult represents the output of an AudioRoleAnalyzer.
type AudioRoleAnalysisResult struct {
	Segments     []domain.AudioSegment
	ProviderID   string
	ModelName    string
	ModelVersion string
	Metadata     map[string]any
}

// AudioRoleAnalyzer is the provider-neutral interface for classifying timeline audio roles.
type AudioRoleAnalyzer interface {
	AnalyzeAudioRoles(ctx context.Context, req AudioRoleAnalysisRequest) (*AudioRoleAnalysisResult, error)
}

// AudioRoleService coordinates automatic source analysis to generate and persist canonical AudioRolePlans.
type AudioRoleService struct {
	db          *storage.DB
	cas         *cas.Store
	audioMixSvc *AudioMixService
	analyzer    AudioRoleAnalyzer
}

// NewAudioRoleService constructs a new AudioRoleService instance.
func NewAudioRoleService(db *storage.DB, casStore *cas.Store, audioMixSvc *AudioMixService) *AudioRoleService {
	return &AudioRoleService{
		db:          db,
		cas:         casStore,
		audioMixSvc: audioMixSvc,
		analyzer:    NewAcousticAudioRoleAnalyzer(),
	}
}

// SetAnalyzer replaces the active AudioRoleAnalyzer implementation.
func (s *AudioRoleService) SetAnalyzer(analyzer AudioRoleAnalyzer) {
	if analyzer != nil {
		s.analyzer = analyzer
	}
}

// GenerateAudioRolePlan automatically generates a canonical AudioRolePlan using source preflight and stem evidence.
func (s *AudioRoleService) GenerateAudioRolePlan(ctx context.Context, in AudioRolePlanInput) (*domain.AudioRolePlan, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	startTime := time.Now().UTC()

	// 1. Verify source asset and preflight report
	if s.db == nil {
		return nil, errors.New("database is not configured")
	}
	asset, err := s.db.GetSourceAsset(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("load source asset %s: %w", in.AssetID, err)
	}

	preflight, err := s.db.GetPreflightReport(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("preflight report required for asset %s: %w", in.AssetID, err)
	}
	if preflight.NormalizedAudioCASPath == "" || preflight.NormalizedAudioSHA256 == "" {
		return nil, fmt.Errorf("normalized audio artifact missing from preflight report for asset %s", in.AssetID)
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
			return nil, fmt.Errorf("audio stem separation failed: %w", err)
		}
		stems = separated
	}

	// 3. Resolve stems paths and metadata
	var vocalsPath, bgPath string
	stemsCASHash := ""
	if stems != nil {
		stemsCASHash = stems.CASHash
		for _, stem := range stems.Stems {
			if stem.Type == domain.StemTypeVocals {
				vocalsPath = stem.AudioCASPath
			} else if stem.Type == domain.StemTypeBackground {
				bgPath = stem.AudioCASPath
			}
		}
	}

	providerID := DefaultAcousticProviderID
	modelName := DefaultAcousticModelName
	modelVersion := DefaultAcousticModelVersion

	// 4. Provenance calculation and idempotent reuse check
	provHash := domain.ComputeAudioRolePlanProvenanceHash(
		preflight.NormalizedAudioSHA256,
		stemsCASHash,
		providerID,
		modelName,
		modelVersion,
	)

	if existingIdx, err := s.db.GetAudioRolePlanByProvenance(ctx, provHash); err == nil && existingIdx != nil {
		// Idempotent hit: return existing plan
		if existingPlan, err := s.db.GetAudioRolePlan(ctx, in.AssetID); err == nil && existingPlan != nil {
			return existingPlan, nil
		}
	}

	// 5. Invoke source analysis
	req := AudioRoleAnalysisRequest{
		AssetID:          in.AssetID,
		RunID:            in.RunID,
		DurationMs:       preflight.DurationMs,
		SourceAudioPath:  preflight.NormalizedAudioCASPath,
		VocalsPath:       vocalsPath,
		BackgroundPath:   bgPath,
		ExecutionProfile: in.ExecutionProfile,
	}

	analyzer := s.analyzer
	if analyzer == nil {
		analyzer = NewAcousticAudioRoleAnalyzer()
	}

	analysisRes, err := analyzer.AnalyzeAudioRoles(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("audio role analysis: %w", err)
	}
	if analysisRes == nil || len(analysisRes.Segments) == 0 {
		return nil, errors.New("audio role analysis produced empty segments")
	}

	if analysisRes.ProviderID != "" {
		providerID = analysisRes.ProviderID
	}
	if analysisRes.ModelName != "" {
		modelName = analysisRes.ModelName
	}
	if analysisRes.ModelVersion != "" {
		modelVersion = analysisRes.ModelVersion
	}

	// 6. Build and commit AudioRolePlan
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

	// 7. Save to SQLite
	if err := s.db.SaveAudioRolePlan(ctx, plan); err != nil {
		return nil, fmt.Errorf("persist audio role plan: %w", err)
	}

	// 8. Record Governance Records (SelectionDecision & ProviderAttempt)
	if in.RunID != "" {
		latencyMs := time.Since(startTime).Milliseconds()
		decision := domain.SelectionDecision{
			ID:                 uuid.NewString(),
			RunID:              in.RunID,
			Stage:              "audio_role_plan",
			SelectedProviderID: providerID,
			PolicyCheckResult:  "allowed",
			DecisionReason:     "source-analysis audio role classification",
			CreatedAt:          time.Now().UTC(),
		}
		_ = s.db.RecordSelectionDecision(ctx, decision)

		attempt := domain.ProviderAttempt{
			ID:            uuid.NewString(),
			RunID:         in.RunID,
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
		_ = s.db.RecordProviderAttempt(ctx, attempt)
	}

	_ = asset
	return &plan, nil
}

// ---------------------------------------------------------------------------
// Production AcousticAudioRoleAnalyzer Implementation
// ---------------------------------------------------------------------------

// AcousticAudioRoleAnalyzer analyzes physical acoustic signals from source audio and separated stems.
type AcousticAudioRoleAnalyzer struct {
	VocalSilenceRMS     float64
	VocalUncertainFloor float64
	SingingMinAutocorr  float64
	SingingMaxZCR       float64
	SFXMinCrestFactor   float64
	WindowDurationMs    int64
	HopDurationMs       int64
}

// NewAcousticAudioRoleAnalyzer creates a new AcousticAudioRoleAnalyzer with calibrated production defaults.
func NewAcousticAudioRoleAnalyzer() *AcousticAudioRoleAnalyzer {
	return &AcousticAudioRoleAnalyzer{
		VocalSilenceRMS:     200.0, // samples below 200 (~ -44 dBFS) are silent
		VocalUncertainFloor: 350.0, // weak/marginal vocal amplitude band
		SingingMinAutocorr:  0.65,  // high pitch stability and sustained harmonicity
		SingingMaxZCR:       0.12,  // singing vowels have lower zero-crossing rate than speech consonants
		SFXMinCrestFactor:   4.5,   // impulsive transients (Foley, taps, clinks) have high peak-to-RMS ratio
		WindowDurationMs:    500,   // 500 ms acoustic analysis window
		HopDurationMs:       500,   // 500 ms hop (contiguous non-overlapping windows)
	}
}

func (a *AcousticAudioRoleAnalyzer) AnalyzeAudioRoles(ctx context.Context, req AudioRoleAnalysisRequest) (*AudioRoleAnalysisResult, error) {
	sampleRate := 16000

	var vocalSamples []int16
	if req.VocalsPath != "" {
		if data, err := os.ReadFile(req.VocalsPath); err == nil {
			if s, info, err := media.ExtractPCM16Samples(data); err == nil {
				vocalSamples = s
				if info != nil && info.SampleRate > 0 {
					sampleRate = int(info.SampleRate)
				}
			}
		}
	}

	var bgSamples []int16
	if req.BackgroundPath != "" {
		if data, err := os.ReadFile(req.BackgroundPath); err == nil {
			if s, _, err := media.ExtractPCM16Samples(data); err == nil {
				bgSamples = s
			}
		}
	}

	// Fallback to source audio if neither stem was read
	if len(vocalSamples) == 0 && len(bgSamples) == 0 && req.SourceAudioPath != "" {
		if data, err := os.ReadFile(req.SourceAudioPath); err == nil {
			if s, info, err := media.ExtractPCM16Samples(data); err == nil {
				// With only source audio, treat as background/mixed
				bgSamples = s
				if info != nil && info.SampleRate > 0 {
					sampleRate = int(info.SampleRate)
				}
			}
		}
	}

	durationMs := req.DurationMs
	if durationMs <= 0 {
		maxLen := len(vocalSamples)
		if len(bgSamples) > maxLen {
			maxLen = len(bgSamples)
		}
		if maxLen > 0 {
			durationMs = int64(maxLen*1000) / int64(sampleRate)
		} else {
			durationMs = 1000 // minimum 1s placeholder
		}
	}

	totalSamples := int((int64(sampleRate) * durationMs) / 1000)
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
		windowSamples = 8000 // 500 ms at 16k
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
		if vocalRMS < a.VocalSilenceRMS {
			// Vocal stem silent: pure background/soundtrack
			if bgCrestFactor >= a.SFXMinCrestFactor && bgPeak > 1000 {
				role = domain.AudioRoleAmbienceSFX
			} else {
				role = domain.AudioRoleInstrumentalBgm
			}
		} else {
			// Vocal stem active: evaluate vocal acoustics
			zcr := computeZCR(vocalChunk)
			autocorr := computeMaxAutocorr(vocalChunk, sampleRate)

			if vocalRMS < a.VocalUncertainFloor && autocorr < 0.30 {
				// Weak, ambiguous, or corrupted vocal signal
				role = domain.AudioRoleUncertain
			} else if autocorr >= a.SingingMinAutocorr && zcr <= a.SingingMaxZCR {
				// Steady harmonicity + sustained musical pitch
				role = domain.AudioRoleSingingMusicVocal
			} else if autocorr >= 0.20 || zcr >= 0.04 {
				// Natural speech modulation (formants + consonants)
				role = domain.AudioRoleNarrationDialogue
			} else {
				// Ambiguous / unclassified
				role = domain.AudioRoleUncertain
			}
		}

		windows = append(windows, rawWindow{
			startMs: startMs,
			endMs:   endMs,
			role:    role,
		})
	}

	// Merge contiguous windows of the same role into clean segments
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

	// Ensure boundary invariants: begins at 0 and ends at durationMs
	if len(merged) == 0 {
		merged = []domain.AudioSegment{
			{StartMs: 0, EndMs: durationMs, Role: domain.AudioRoleInstrumentalBgm},
		}
	} else {
		merged[0].StartMs = 0
		merged[len(merged)-1].EndMs = durationMs
	}

	return &AudioRoleAnalysisResult{
		Segments:     merged,
		ProviderID:   DefaultAcousticProviderID,
		ModelName:    DefaultAcousticModelName,
		ModelVersion: DefaultAcousticModelVersion,
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

	// Human vocal pitch range: 80 Hz to 500 Hz
	minLag := sampleRate / 500 // ~32 samples at 16k
	maxLag := sampleRate / 80  // ~200 samples at 16k
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
