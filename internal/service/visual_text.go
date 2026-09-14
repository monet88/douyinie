package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
)

// OCRInvokeFunc defines the pluggable provider invocation hook for OCR/visual text detection.
type OCRInvokeFunc func(ctx context.Context, p provider.Provider, req provider.OCRRequest) (*provider.OCRResult, error)

// VisualTextService orchestrates visual text detection, tracking, role classification (TextRegionPlan),
// and source-derived artifact persistence in CAS.
//
// Invariants (locked by #16 §7, #18, #31):
//  1. Multi-role classification: Classifies on-screen text into first-class roles:
//     speech_subtitle | semantic_text | instructional_ui_text | brand_keep | ignore/noise.
//  2. Dynamic geometry tracking & interpolation: Tracks text bounding boxes across sampled frames
//     and linearly interpolates geometry across small gaps (e.g. up to 1000ms).
//  3. Source-derived & language-reusable: The plan is derived purely from source visual media;
//     it is computed once and reused across VI/EN localization jobs when provenance matches.
//  4. Confidence evidence & protected metadata: Persists per-region detection quality metrics
//     and protection flags for tutorial controls, timeline tracks, and tap targets.
type VisualTextService struct {
	db             *storage.DB
	cas            *cas.Store
	router         *provider.Router
	translationSvc *TranslationService

	// OCRInvoke executes one OCR detection attempt.
	// When nil, router-backed invocation is used.
	OCRInvoke OCRInvokeFunc

	// ClassifyConfig optionally supplies custom classification heuristics.
	ClassifyConfig func(w, h int) domain.TextRegionClassifyConfig

	// SubtitlePlacementSelector optionally supplies a pluggable AI/heuristic subtitle placement selector.
	SubtitlePlacementSelector domain.SubtitlePlacementSelector
}

// NewVisualTextService creates a new VisualTextService instance.
func NewVisualTextService(db *storage.DB, casStore *cas.Store) *VisualTextService {
	return &VisualTextService{
		db:  db,
		cas: casStore,
	}
}

func (s *VisualTextService) ConfigureRouter(router *provider.Router) {
	s.router = router
}

// SetTranslationService injects the TranslationService used to translate semantic/UI text.
func (s *VisualTextService) SetTranslationService(svc *TranslationService) {
	s.translationSvc = svc
}

// VisualTextDetectionInput defines input parameters for visual text detection and tracking.
type VisualTextDetectionInput struct {
	RunID                 string                  `json:"run_id"`
	AssetID               string                  `json:"asset_id"`
	JobID                 string                  `json:"job_id,omitempty"`
	FrameSampleStepMs     int64                   `json:"frame_sample_step_ms,omitempty"`
	MaxFrames             int                     `json:"max_frames,omitempty"`
	ExecutionProfile      domain.ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                `json:"authorized_credentials,omitempty"`
}

// DetectAndTrackText executes OCR, tracks regions across frames, interpolates geometry,
// classifies operational roles, and stores the immutable TextRegionPlan in CAS and SQLite.
func (s *VisualTextService) DetectAndTrackText(ctx context.Context, input VisualTextDetectionInput) (*domain.TextRegionPlan, error) {
	if strings.TrimSpace(input.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}

	asset, err := s.db.GetSourceAsset(ctx, input.AssetID)
	if err != nil {
		return nil, fmt.Errorf("load source asset: %w", err)
	}

	stepMs := input.FrameSampleStepMs
	if stepMs <= 0 {
		stepMs = 500
	}

	sourceVideoRef := worker.ArtifactRef{
		SHA256: asset.SHA256,
		Path:   asset.CASPath,
	}

	req := provider.OCRRequest{
		AssetID:           input.AssetID,
		SourceVideo:       sourceVideoRef,
		FrameSampleStepMs: stepMs,
		MaxFrames:         input.MaxFrames,
	}

	routeReq := provider.RouteRequest{
		RunID:            input.RunID,
		Stage:            provider.TypeOCR,
		Language:         "*",
		ExecutionProfile: input.ExecutionProfile,
	}

	var routeRes *provider.RouteResult
	if s.router != nil {
		if rRes, err := s.router.Route(ctx, routeReq); err == nil && rRes.SelectedProvider != nil {
			routeRes = rRes
			// Check candidates in policy-eligible order for cached provenance hit
			eligibleProvs := append([]provider.Provider{rRes.SelectedProvider}, rRes.FallbackOrdered...)
			for _, p := range eligibleProvs {
				if p == nil || p.PolicyState() != provider.PolicyAllowed || !p.IsHealthy() {
					continue
				}
				mName, mVer := p.ModelInfo()
				var extraIdentities []string
				if snapProv, ok := p.(provider.OCRSnapshotProvider); ok {
					detSHA, recSHA, oriSHA, runtimeID := snapProv.OCRSnapshotDigests()
					if detSHA != "" || recSHA != "" || oriSHA != "" || runtimeID != "" {
						extraIdentities = []string{detSHA, recSHA, oriSHA, runtimeID}
					}
				}
				if expectedProvHash, err := domain.ComputeTextRegionPlanProvenanceHash(input.AssetID, p.ID(), mName, mVer, stepMs, extraIdentities...); err == nil {
					if existingIdx, err := s.db.GetTextRegionPlanByProvenance(ctx, expectedProvHash); err == nil && existingIdx != nil && existingIdx.CASHash != "" {
						reader, err := s.cas.Get(existingIdx.CASHash)
						if err == nil {
							defer reader.Close()
							var cached domain.TextRegionPlan
							if err := json.NewDecoder(reader).Decode(&cached); err == nil {
								cached.CASHash = existingIdx.CASHash
								if input.RunID != "" && s.db != nil {
									execs, err := s.db.ListStageExecutions(ctx, input.RunID)
									hasStage := false
									if err == nil {
										for _, e := range execs {
											if e.Stage == "visual_text" && e.Status == "succeeded" {
												hasStage = true
												break
											}
										}
									}
									if !hasStage {
										now := time.Now().UTC()
										stageExec := domain.StageExecution{
											ID:             uuid.NewString(),
											RunID:          input.RunID,
											Stage:          "visual_text",
											Status:         "succeeded",
											ArtifactSHA256: cached.CASHash,
											StartedAt:      &cached.CreatedAt,
											CompletedAt:    &now,
											CreatedAt:      cached.CreatedAt,
											UpdatedAt:      now,
										}
										_ = s.db.CreateStageExecution(ctx, stageExec)
									}
								}
								return &cached, nil
							}
						}
					}
				}
			}
		}
	}

	var res *provider.OCRResult
	if s.OCRInvoke != nil {
		res, err = s.OCRInvoke(ctx, nil, req)
		if err != nil {
			return nil, fmt.Errorf("invoke ocr provider: %w", err)
		}
	} else if s.router != nil {
		if routeRes == nil {
			routeRes, err = s.router.Route(ctx, routeReq)
			if err != nil {
				return nil, fmt.Errorf("route ocr provider: %w", err)
			}
		}

		err = s.router.ExecuteRoutedWithRetry(ctx, routeReq, routeRes, asset.SHA256, 3, func(p provider.Provider, attemptNum int) error {
			ocrProv, ok := p.(provider.OCRRegionProvider)
			if !ok {
				return fmt.Errorf("provider %s does not implement OCRRegionProvider", p.ID())
			}
			out, err := ocrProv.DetectRegions(ctx, req)
			if err != nil {
				return err
			}
			res = out
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("execute ocr with retry: %w", err)
		}
	} else {
		// Default fake provider fallback
		fakeProv := provider.NewFakeOCRProvider("fake_paddle_ocr")
		res, err = fakeProv.DetectRegions(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("default ocr fallback: %w", err)
		}
	}

	if res == nil {
		return nil, domain.ErrOCRFailed
	}

	w := res.FrameWidth
	if w <= 0 {
		w = 1080
	}
	h := res.FrameHeight
	if h <= 0 {
		h = 1920
	}

	var cfg domain.TextRegionClassifyConfig
	if s.ClassifyConfig != nil {
		cfg = s.ClassifyConfig(w, h)
	} else {
		cfg = domain.DefaultTextRegionClassifyConfig(w, h)
	}

	// 1. Group raw detections into spatial/temporal tracks with interpolation
	trackedRegions := buildAndInterpolateTracks(res.Detections, stepMs, cfg)

	// 2. Compute provenance hash (source-derived, no target language)
	var extraIdentities []string
	if res.DetSnapshotDigest != "" || res.RecSnapshotDigest != "" || res.OriSnapshotDigest != "" || res.RuntimeIdentity != "" {
		extraIdentities = []string{res.DetSnapshotDigest, res.RecSnapshotDigest, res.OriSnapshotDigest, res.RuntimeIdentity}
	}
	provHash, err := domain.ComputeTextRegionPlanProvenanceHash(input.AssetID, res.ProviderID, res.ModelName, res.ModelVersion, stepMs, extraIdentities...)
	if err != nil {
		return nil, fmt.Errorf("compute provenance hash: %w", err)
	}

	plan := domain.TextRegionPlan{
		ID:                uuid.NewString(),
		SchemaVersion:     domain.TextRegionPlanSchemaVersion,
		AssetID:           input.AssetID,
		ProviderID:        res.ProviderID,
		ModelName:         res.ModelName,
		ModelVersion:      res.ModelVersion,
		FrameWidth:        w,
		FrameHeight:       h,
		Regions:           trackedRegions,
		DetSnapshotDigest: res.DetSnapshotDigest,
		RecSnapshotDigest: res.RecSnapshotDigest,
		OriSnapshotDigest: res.OriSnapshotDigest,
		RuntimeIdentity:   res.RuntimeIdentity,
		ProvenanceHash:    provHash,
		CreatedAt:         time.Now().UTC(),
	}

	// 3. Store in CAS
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("marshal text region plan: %w", err)
	}
	casObj, err := s.cas.Put(bytes.NewReader(planBytes))
	if err != nil {
		return nil, fmt.Errorf("store text region plan in CAS: %w", err)
	}
	plan.CASHash = casObj.SHA256

	// 4. Persist index in SQLite
	err = s.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             plan.ID,
		AssetID:        plan.AssetID,
		ProviderID:     plan.ProviderID,
		ModelName:      plan.ModelName,
		ModelVersion:   plan.ModelVersion,
		CASHash:        plan.CASHash,
		ProvenanceHash: plan.ProvenanceHash,
		CreatedAt:      plan.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save text region plan index: %w", err)
	}

	// 5. Record succeeded stage execution in SQLite if RunID is present
	if input.RunID != "" && s.db != nil {
		now := time.Now().UTC()
		stageExec := domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          input.RunID,
			Stage:          "visual_text",
			Status:         "succeeded",
			ArtifactSHA256: plan.CASHash,
			StartedAt:      &plan.CreatedAt,
			CompletedAt:    &now,
			CreatedAt:      plan.CreatedAt,
			UpdatedAt:      now,
		}
		_ = s.db.CreateStageExecution(ctx, stageExec)
	}

	return &plan, nil
}

// rawDetectionCluster groups observations across frames that belong to the same on-screen text entity.
type rawDetectionCluster struct {
	text         string
	observations []provider.RawTextDetection
}

func buildAndInterpolateTracks(dets []provider.RawTextDetection, stepMs int64, cfg domain.TextRegionClassifyConfig) []domain.TrackedTextRegion {
	if len(dets) == 0 {
		return []domain.TrackedTextRegion{}
	}

	// 1. Cluster detections by text similarity + spatial overlap
	var clusters []rawDetectionCluster
	for _, d := range dets {
		matchedIdx := -1
		for idx, cl := range clusters {
			if strings.EqualFold(strings.TrimSpace(cl.text), strings.TrimSpace(d.Text)) {
				// Same text: check if temporally adjacent or spatial overlap
				lastObs := cl.observations[len(cl.observations)-1]
				if math.Abs(float64(d.TimestampMs-lastObs.TimestampMs)) <= float64(stepMs*4) {
					matchedIdx = idx
					break
				}
			}
		}
		if matchedIdx >= 0 {
			clusters[matchedIdx].observations = append(clusters[matchedIdx].observations, d)
		} else {
			clusters = append(clusters, rawDetectionCluster{
				text:         d.Text,
				observations: []provider.RawTextDetection{d},
			})
		}
	}

	var results []domain.TrackedTextRegion
	regionNum := 1

	for _, cl := range clusters {
		if len(cl.observations) == 0 {
			continue
		}

		// Sort observations chronologically
		sort.Slice(cl.observations, func(i, j int) bool {
			return cl.observations[i].FrameIndex < cl.observations[j].FrameIndex
		})

		firstObs := cl.observations[0]
		lastObs := cl.observations[len(cl.observations)-1]

		obsByFrame := make(map[int]provider.RawTextDetection)
		var confSum float64
		minConf := 1.0
		var avgBox domain.BoundingBox

		for _, obs := range cl.observations {
			obsByFrame[obs.FrameIndex] = obs
			confSum += obs.Confidence
			if obs.Confidence < minConf {
				minConf = obs.Confidence
			}
			avgBox.X += obs.Box.X
			avgBox.Y += obs.Box.Y
			avgBox.Width += obs.Box.Width
			avgBox.Height += obs.Box.Height
		}

		obsCount := len(cl.observations)
		avgBox.X /= obsCount
		avgBox.Y /= obsCount
		avgBox.Width /= obsCount
		avgBox.Height /= obsCount
		meanConf := confSum / float64(obsCount)
		// Collect spatio-temporally nearby observations from other detections (adjacent frames within +/- 4 steps, spatial overlap)
		var nearbyObs []domain.NearbyObservation
		for _, d := range dets {
			if d.FrameIndex >= firstObs.FrameIndex-4 && d.FrameIndex <= lastObs.FrameIndex+4 {
				// Check if spatial overlap with cluster avgBox
				expandedBox := domain.BoundingBox{
					X:      avgBox.X - avgBox.Width/2,
					Y:      avgBox.Y - avgBox.Height/2,
					Width:  avgBox.Width * 2,
					Height: avgBox.Height * 2,
				}
				if domain.BoxesOverlap(expandedBox, d.Box) {
					nearbyObs = append(nearbyObs, domain.NearbyObservation{
						Text:        d.Text,
						TimestampMs: d.TimestampMs,
						Box:         d.Box,
					})
				}
			}
		}

		// Classify role deterministically with spatio-temporal instability tracking
		role, protectedMeta, reviewReq, reviewReason := domain.ClassifyRegionWithInstability(cl.text, avgBox, meanConf, cfg, nearbyObs)
		// Build keyframes with linear interpolation for frame gaps <= 2 steps
		var keyframes []domain.RegionKeyframe
		interpolatedCount := 0

		for fIdx := firstObs.FrameIndex; fIdx <= lastObs.FrameIndex; fIdx++ {
			fTimeMs := int64(fIdx) * stepMs
			if obs, exists := obsByFrame[fIdx]; exists {
				keyframes = append(keyframes, domain.RegionKeyframe{
					FrameIndex:  fIdx,
					TimestampMs: obs.TimestampMs,
					Box:         obs.Box,
					Confidence:  obs.Confidence,
					Observed:    true,
				})
			} else {
				// Linear interpolation between closest preceding and following observations
				var prevObs, nextObs *provider.RawTextDetection
				for p := fIdx - 1; p >= firstObs.FrameIndex; p-- {
					if o, ok := obsByFrame[p]; ok {
						prevObs = &o
						break
					}
				}
				for n := fIdx + 1; n <= lastObs.FrameIndex; n++ {
					if o, ok := obsByFrame[n]; ok {
						nextObs = &o
						break
					}
				}

				var interpBox domain.BoundingBox
				var interpConf float64
				if prevObs != nil && nextObs != nil {
					totalFrames := float64(nextObs.FrameIndex - prevObs.FrameIndex)
					alpha := float64(fIdx-prevObs.FrameIndex) / totalFrames
					interpBox = domain.BoundingBox{
						X:      int(float64(prevObs.Box.X) + alpha*float64(nextObs.Box.X-prevObs.Box.X)),
						Y:      int(float64(prevObs.Box.Y) + alpha*float64(nextObs.Box.Y-prevObs.Box.Y)),
						Width:  int(float64(prevObs.Box.Width) + alpha*float64(nextObs.Box.Width-prevObs.Box.Width)),
						Height: int(float64(prevObs.Box.Height) + alpha*float64(nextObs.Box.Height-prevObs.Box.Height)),
					}
					interpConf = prevObs.Confidence + alpha*(nextObs.Confidence-prevObs.Confidence)
				} else if prevObs != nil {
					interpBox = prevObs.Box
					interpConf = prevObs.Confidence
				} else if nextObs != nil {
					interpBox = nextObs.Box
					interpConf = nextObs.Confidence
				} else {
					interpBox = avgBox
					interpConf = meanConf
				}

				interpolatedCount++
				keyframes = append(keyframes, domain.RegionKeyframe{
					FrameIndex:  fIdx,
					TimestampMs: fTimeMs,
					Box:         interpBox,
					Confidence:  interpConf,
					Observed:    false,
				})
			}
		}

		reg := domain.TrackedTextRegion{
			ID:              fmt.Sprintf("region-%03d", regionNum),
			Text:            cl.text,
			Role:            role,
			FirstFrameIndex: firstObs.FrameIndex,
			LastFrameIndex:  lastObs.FrameIndex,
			FirstSeenMs:     firstObs.TimestampMs,
			LastSeenMs:      lastObs.TimestampMs,
			Keyframes:       keyframes,
			ConfidenceEvidence: domain.ConfidenceEvidence{
				MeanConfidence:     meanConf,
				MinConfidence:      minConf,
				ObservationCount:   obsCount,
				InterpolatedFrames: interpolatedCount,
				LowConfidence:      meanConf < cfg.MinConfidence,
			},
			ProtectedMetadata: protectedMeta,
			ReviewRequired:    reviewReq,
			ReviewReason:      reviewReason,
		}

		results = append(results, reg)
		regionNum++
	}

	return results
}

// LocalizeVisualTrackInput defines input parameters to generate LocalizedVisualTrack and LocalizedSubtitleTrack.
type LocalizeVisualTrackInput struct {
	RunID                 string                           `json:"run_id"`
	AssetID               string                           `json:"asset_id"`
	JobID                 string                           `json:"job_id,omitempty"`
	TargetLanguage        string                           `json:"target_language"`
	TranslationVariantCAS string                           `json:"translation_variant_cas,omitempty"`
	Overrides             []domain.RegionOverride          `json:"overrides,omitempty"`
	InpaintingFallbacks   []string                         `json:"inpainting_fallbacks,omitempty"` // Region IDs where inpainting fallback is explicitly requested
	SceneProtectedRegions []domain.SceneProtectedRegion    `json:"scene_protected_regions,omitempty"`
	PlacementSelector     domain.SubtitlePlacementSelector `json:"-"`
	ExecutionProfile      domain.ExecutionProfile          `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                         `json:"authorized_credentials,omitempty"`
	ConsentGranted        bool                             `json:"consent_granted,omitempty"`
}

// ApplyRegionOverrides applies direct-manipulation overrides (drag/resize/reclassify/text) to a TextRegionPlan.
// Unknown RegionOverride IDs return an error. Bounding box coordinates and sizes clamp to frame bounds.
func ApplyRegionOverrides(plan *domain.TextRegionPlan, overrides []domain.RegionOverride) (*domain.TextRegionPlan, error) {
	if plan == nil {
		return nil, errors.New("plan is nil")
	}
	if len(overrides) == 0 {
		return plan, nil
	}

	frameW := plan.FrameWidth
	if frameW <= 0 {
		frameW = 1080
	}
	frameH := plan.FrameHeight
	if frameH <= 0 {
		frameH = 1920
	}

	// Build map of existing region IDs to validate overrides
	existingRegionIDs := make(map[string]bool, len(plan.Regions))
	for _, reg := range plan.Regions {
		existingRegionIDs[reg.ID] = true
	}

	overrideMap := make(map[string]domain.RegionOverride, len(overrides))
	for _, ov := range overrides {
		regID := strings.TrimSpace(ov.RegionID)
		if regID == "" {
			return nil, fmt.Errorf("%w: region_id is required", domain.ErrRegionOverrideInvalid)
		}
		if !existingRegionIDs[regID] {
			return nil, fmt.Errorf("%w: unknown region_id %q", domain.ErrRegionOverrideInvalid, regID)
		}
		overrideMap[regID] = ov
	}

	// Clone plan
	updatedRegions := make([]domain.TrackedTextRegion, len(plan.Regions))
	copy(updatedRegions, plan.Regions)

	for i, reg := range updatedRegions {
		ov, found := overrideMap[reg.ID]
		if !found {
			continue
		}

		if ov.NewRole != nil {
			if !domain.IsValidTextRegionRole(*ov.NewRole) {
				return nil, fmt.Errorf("%w: invalid role '%s'", domain.ErrRegionOverrideInvalid, *ov.NewRole)
			}
			reg.Role = *ov.NewRole
			if *ov.NewRole == domain.TextRoleUncertain {
				reg.ReviewRequired = true
				if reg.ReviewReason == "" {
					reg.ReviewReason = "uncertain_role"
				}
			} else {
				// Reclassification of a genuinely uncertain-role exception clears that role exception.
				// Preserve unrelated low-OCR ReviewRequired flags when a role-only edit does not resolve them.
				if reg.ConfidenceEvidence.LowConfidence || strings.Contains(reg.ReviewReason, "low_ocr") || strings.Contains(reg.ReviewReason, "low_confidence") {
					if (*ov.NewRole == domain.TextRoleSpeechSubtitle || *ov.NewRole == domain.TextRoleSemanticText) && (ov.NewText == nil || strings.TrimSpace(*ov.NewText) == "") {
						reg.ReviewRequired = true
						if *ov.NewRole == domain.TextRoleSpeechSubtitle {
							reg.ReviewReason = "low_ocr_confidence_subtitle"
						} else {
							reg.ReviewReason = "low_ocr_confidence_semantic_text"
						}
					} else {
						reg.ReviewRequired = false
						reg.ReviewReason = ""
					}
				} else {
					reg.ReviewRequired = false
					reg.ReviewReason = ""
				}
			}
		}
		if ov.NewText != nil {
			reg.Text = *ov.NewText
			if strings.TrimSpace(*ov.NewText) != "" && reg.Role != domain.TextRoleUncertain {
				reg.ReviewRequired = false
				reg.ReviewReason = ""
			}
		}
		if ov.IsProtected != nil {
			reg.ProtectedMetadata.IsProtected = *ov.IsProtected
			if *ov.IsProtected && reg.ProtectedMetadata.Reason == "" {
				reg.ProtectedMetadata.Reason = "manual_override_protection"
			}
		}

		// Apply bounding box deltas (drag / resize) to all keyframes and clamp to frame bounds
		if ov.BoxDeltaX != 0 || ov.BoxDeltaY != 0 || ov.BoxDeltaW != 0 || ov.BoxDeltaH != 0 {
			for k := range reg.Keyframes {
				b := reg.Keyframes[k].Box
				b.X += ov.BoxDeltaX
				b.Y += ov.BoxDeltaY
				b.Width += ov.BoxDeltaW
				b.Height += ov.BoxDeltaH

				// Enforce minimum dimension
				if b.Width < 1 {
					b.Width = 1
				}
				if b.Height < 1 {
					b.Height = 1
				}

				// Clamp position and size within [0, frameW] and [0, frameH]
				if b.X < 0 {
					b.X = 0
				}
				if b.Y < 0 {
					b.Y = 0
				}
				if b.X >= frameW {
					b.X = frameW - 1
				}
				if b.Y >= frameH {
					b.Y = frameH - 1
				}
				if b.X+b.Width > frameW {
					b.Width = frameW - b.X
				}
				if b.Y+b.Height > frameH {
					b.Height = frameH - b.Y
				}

				reg.Keyframes[k].Box = b
			}
		}

		updatedRegions[i] = reg
	}

	clone := *plan
	clone.Regions = updatedRegions
	return &clone, nil
}

// LocalizeVisualTrack builds the LocalizedVisualTrack (and underlying LocalizedSubtitleTrack)
// with deterministic in-place cover/overlay, standard instructional UI terminology,
// scale-aware compact fit-content subtitle box, and scene-aware non-occlusion.
func (s *VisualTextService) LocalizeVisualTrack(ctx context.Context, in LocalizeVisualTrackInput) (*domain.LocalizedVisualTrack, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, errors.New("asset_id is required")
	}
	if !domain.IsValidTargetLanguage(in.TargetLanguage) {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidTargetLanguage, in.TargetLanguage)
	}

	// Prefer the caller-pinned speech TranslationVariant artifact when provided.
	// This keeps retries/resumes stable even when a previous visual-track attempt
	// wrote newer overlay translations into the asset/language index. Legacy
	// callers without an explicit CAS continue to resolve the latest index row.
	explicitTransCAS := strings.TrimSpace(in.TranslationVariantCAS)
	canonicalTransCAS := explicitTransCAS
	var transIdx *storage.TranslationVariantIndex
	if canonicalTransCAS == "" {
		var err error
		if strings.TrimSpace(in.RunID) != "" {
			transIdx, err = s.db.GetTranslationVariantIndexByRun(ctx, in.RunID)
			if err == nil && transIdx != nil && (transIdx.AssetID != in.AssetID || !strings.EqualFold(transIdx.TargetLanguage, in.TargetLanguage)) {
				return nil, fmt.Errorf("translation variant run binding mismatch for run %s", in.RunID)
			}
		} else {
			transIdx, err = s.db.GetTranslationVariantIndex(ctx, in.AssetID, in.TargetLanguage)
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("query translation variant index: %w", err)
		}
		if transIdx != nil {
			canonicalTransCAS = transIdx.CASHash
		}
	}

	// 1. Load latest TextRegionPlan
	planIdx, err := s.db.GetTextRegionPlanIndex(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrTextRegionPlanNotFound, err)
	}
	rc, err := s.cas.Get(planIdx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("read text region plan from CAS: %w", err)
	}
	defer rc.Close()
	var plan domain.TextRegionPlan
	if err := json.NewDecoder(rc).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode text region plan: %w", err)
	}
	plan.CASHash = planIdx.CASHash
	plan.ProvenanceHash = planIdx.ProvenanceHash

	// 2. Apply any operator overrides (direct manipulation / reclassification)
	activePlan, err := ApplyRegionOverrides(&plan, in.Overrides)
	if err != nil {
		return nil, err
	}

	// 3. Extract protected regions for non-occlusion collision checks
	// Set of region IDs marked for inpainting fallback
	inpaintSet := make(map[string]bool)
	for _, id := range in.InpaintingFallbacks {
		inpaintSet[id] = true
	}

	// Scale-aware overlay parameters
	overlayPaddingX := int(float64(plan.FrameWidth) * 0.015) // ~16px for 1080w
	if overlayPaddingX < 12 {
		overlayPaddingX = 12
	} else if overlayPaddingX > 24 {
		overlayPaddingX = 24
	}
	overlayPaddingY := int(float64(plan.FrameHeight) * 0.005) // ~10px for 1920h
	if overlayPaddingY < 6 {
		overlayPaddingY = 6
	} else if overlayPaddingY > 16 {
		overlayPaddingY = 16
	}
	overlayFontSize := int(float64(plan.FrameHeight) * 0.016) // ~30px for 1920h
	if overlayFontSize < 18 {
		overlayFontSize = 18
	}

	// 4. Generate Overlays for semantic_text & instructional_ui_text
	var overlays []domain.LocalizedOverlayItem
	for _, reg := range activePlan.Regions {
		switch reg.Role {
		case domain.TextRoleSemanticText:
			trimmedText := strings.TrimSpace(reg.Text)
			if trimmedText == "" || domain.IsPathologicalRepetitionNoise(trimmedText) {
				continue
			}
			if s.translationSvc == nil {
				return nil, fmt.Errorf("translation service is required to localize semantic text %q: %w", reg.Text, domain.ErrTranslationFailed)
			}
			tRes, err := s.translationSvc.Translate(ctx, domain.TranslationJobInput{
				RunID:                 in.RunID,
				AssetID:               in.AssetID,
				JobID:                 in.JobID,
				SourceLanguage:        "zh",
				TargetLanguage:        in.TargetLanguage,
				ExecutionProfile:      in.ExecutionProfile,
				AuthorizedCredentials: in.AuthorizedCredentials,
				ConsentGranted:        in.ConsentGranted,
				Segments: []domain.TranslationInputSegment{
					{Index: 0, SourceText: reg.Text, StartMs: reg.FirstSeenMs, EndMs: reg.LastSeenMs},
				},
			})
			if err != nil {
				return nil, fmt.Errorf("translate semantic text %q: %w", reg.Text, err)
			}
			if tRes == nil || len(tRes.Segments) == 0 || tRes.Segments[0].TargetText == "" {
				return nil, fmt.Errorf("translate semantic text %q: empty translation result", reg.Text)
			}
			locText := tRes.Segments[0].TargetText

			baseBox := domain.BoundingBox{Width: int(float64(plan.FrameWidth) * 0.25), Height: overlayFontSize + 2*overlayPaddingY}
			if len(reg.Keyframes) > 0 {
				baseBox = reg.Keyframes[0].Box
			}
			charWidth := int(float64(overlayFontSize) * 0.55)
			fitW := len([]rune(locText))*charWidth + 2*overlayPaddingX
			fitH := overlayFontSize + 2*overlayPaddingY
			boxW := baseBox.Width
			if fitW > boxW {
				boxW = fitW
			}
			boxH := baseBox.Height
			if fitH > boxH {
				boxH = fitH
			}
			if baseBox.X+boxW > plan.FrameWidth {
				boxW = plan.FrameWidth - baseBox.X
			}
			if baseBox.Y+boxH > plan.FrameHeight {
				boxH = plan.FrameHeight - baseBox.Y
			}
			repBox := domain.BoundingBox{X: baseBox.X, Y: baseBox.Y, Width: boxW, Height: boxH}

			// Non-occlusion check: verify overlay does not overlap other protected obstacles during its time window
			overlayProtects := domain.GetProtectedBoxesForTimeWindow(activePlan.Regions, in.SceneProtectedRegions, reg.FirstSeenMs, reg.LastSeenMs, reg.ID)
			for _, prot := range overlayProtects {
				if domain.BoxesOverlap(repBox, prot) {
					return nil, fmt.Errorf("%w: overlay for semantic text %q (box %+v) occludes protected region (box %+v)",
						domain.ErrSubtitleOverlapsProtectedRegion, reg.ID, repBox, prot)
				}
			}

			isInpainting := inpaintSet[reg.ID] // Non-default fallback
			overlays = append(overlays, domain.LocalizedOverlayItem{
				RegionID:       reg.ID,
				Role:           reg.Role,
				SourceText:     reg.Text,
				LocalizedText:  locText,
				StartMs:        reg.FirstSeenMs,
				EndMs:          reg.LastSeenMs,
				Box:            repBox,
				IsCoverDefault: !isInpainting,
				Inpainting:     isInpainting,
				BoxColor:       "black@0.8",
				FontColor:      "#FFFFFF",
				FontSizePx:     overlayFontSize,
				PaddingX:       overlayPaddingX,
				PaddingY:       overlayPaddingY,
			})

		case domain.TextRoleInstructionalUIText:
			trimmedText := strings.TrimSpace(reg.Text)
			if trimmedText == "" || domain.IsPathologicalRepetitionNoise(trimmedText) {
				continue
			}
			if s.translationSvc == nil {
				return nil, fmt.Errorf("translation service is required to localize instructional UI text %q: %w", reg.Text, domain.ErrTranslationFailed)
			}
			tRes, err := s.translationSvc.Translate(ctx, domain.TranslationJobInput{
				RunID:                 in.RunID,
				AssetID:               in.AssetID,
				JobID:                 in.JobID,
				SourceLanguage:        "zh",
				TargetLanguage:        in.TargetLanguage,
				ExecutionProfile:      in.ExecutionProfile,
				AuthorizedCredentials: in.AuthorizedCredentials,
				ConsentGranted:        in.ConsentGranted,
				Segments: []domain.TranslationInputSegment{
					{Index: 0, SourceText: reg.Text, StartMs: reg.FirstSeenMs, EndMs: reg.LastSeenMs},
				},
			})
			if err != nil {
				return nil, fmt.Errorf("translate instructional UI text %q: %w", reg.Text, err)
			}
			if tRes == nil || len(tRes.Segments) == 0 || tRes.Segments[0].TargetText == "" {
				return nil, fmt.Errorf("translate instructional UI text %q: empty translation result", reg.Text)
			}
			locText := domain.NormalizeInstructionalUIText(tRes.Segments[0].TargetText, reg.Text, in.TargetLanguage)

			baseBox := domain.BoundingBox{Width: int(float64(plan.FrameWidth) * 0.15), Height: overlayFontSize + 2*overlayPaddingY}
			if len(reg.Keyframes) > 0 {
				baseBox = reg.Keyframes[0].Box
			}
			charWidth := int(float64(overlayFontSize) * 0.55)
			fitW := len([]rune(locText))*charWidth + 2*overlayPaddingX
			fitH := overlayFontSize + 2*overlayPaddingY
			boxW := baseBox.Width
			if fitW > boxW {
				boxW = fitW
			}
			boxH := baseBox.Height
			if fitH > boxH {
				boxH = fitH
			}
			if baseBox.X+boxW > plan.FrameWidth {
				boxW = plan.FrameWidth - baseBox.X
			}
			if baseBox.Y+boxH > plan.FrameHeight {
				boxH = plan.FrameHeight - baseBox.Y
			}
			repBox := domain.BoundingBox{X: baseBox.X, Y: baseBox.Y, Width: boxW, Height: boxH}

			// Non-occlusion check: verify overlay does not overlap other protected obstacles during its time window
			overlayProtects := domain.GetProtectedBoxesForTimeWindow(activePlan.Regions, in.SceneProtectedRegions, reg.FirstSeenMs, reg.LastSeenMs, reg.ID)
			for _, prot := range overlayProtects {
				if domain.BoxesOverlap(repBox, prot) {
					return nil, fmt.Errorf("%w: overlay for instructional UI %q (box %+v) occludes protected region (box %+v)",
						domain.ErrSubtitleOverlapsProtectedRegion, reg.ID, repBox, prot)
				}
			}

			isInpainting := inpaintSet[reg.ID]
			overlays = append(overlays, domain.LocalizedOverlayItem{
				RegionID:       reg.ID,
				Role:           reg.Role,
				SourceText:     reg.Text,
				LocalizedText:  locText,
				StartMs:        reg.FirstSeenMs,
				EndMs:          reg.LastSeenMs,
				Box:            repBox,
				IsCoverDefault: !isInpainting,
				Inpainting:     isInpainting,
				BoxColor:       "#1E1E1E@0.9",
				FontColor:      "#00E5FF",
				FontSizePx:     overlayFontSize,
				PaddingX:       overlayPaddingX,
				PaddingY:       overlayPaddingY,
			})
		}
	}

	// 5. Generate Subtitle Cues grounded in canonical TranslationVariant
	var subtitleCues []domain.SubtitleCue
	selector := in.PlacementSelector
	if selector == nil {
		selector = s.SubtitlePlacementSelector
	}

	var dubScriptIdx *storage.DubScriptVariantIndex
	if strings.TrimSpace(in.RunID) != "" {
		dubScriptIdx, err = s.db.GetDubScriptVariantIndexByRun(ctx, in.RunID)
		if err == nil && dubScriptIdx != nil && (dubScriptIdx.AssetID != in.AssetID || !strings.EqualFold(dubScriptIdx.TargetLanguage, in.TargetLanguage)) {
			return nil, fmt.Errorf("dub script variant run binding mismatch for run %s", in.RunID)
		}
	} else {
		dubScriptIdx, err = s.db.GetDubScriptVariantIndex(ctx, in.AssetID, in.TargetLanguage)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("query dub script variant index: %w", err)
	}

	if dubScriptIdx != nil || transIdx != nil || canonicalTransCAS != "" {
		// A localized track is requested/present. Must fail closed on any missing/mismatch/empty artifact.
		if dubScriptIdx != nil {
			if dubScriptIdx.CASHash == "" {
				return nil, fmt.Errorf("dub script variant index has empty CAS hash")
			}
			drc, err := s.cas.Get(dubScriptIdx.CASHash)
			if err != nil {
				return nil, fmt.Errorf("load dub script from CAS (%s): %w", dubScriptIdx.CASHash, err)
			}
			defer drc.Close()
			var dVariant domain.DubScriptVariant
			if err := json.NewDecoder(drc).Decode(&dVariant); err != nil {
				return nil, fmt.Errorf("decode dub script artifact (%s): %w", dubScriptIdx.CASHash, err)
			}
			if len(dVariant.Segments) == 0 {
				return nil, fmt.Errorf("dub script variant has zero segments")
			}

			// Load canonical TranslationVariant to cross-check and ground captions
			var tVariant domain.TranslationVariant
			if canonicalTransCAS == "" {
				return nil, fmt.Errorf("%w: missing canonical translation index for dub script", domain.ErrTranslationVariantNotFound)
			}
			if dVariant.TranslationVariantCAS == "" || dVariant.TranslationVariantCAS != canonicalTransCAS {
				return nil, fmt.Errorf("%w: dub script translation CAS %q does not match canonical translation CAS %q",
					domain.ErrMeaningPreservationFailed, dVariant.TranslationVariantCAS, canonicalTransCAS)
			}
			transCAS := canonicalTransCAS
			trc, err := s.cas.Get(transCAS)
			if err != nil {
				return nil, fmt.Errorf("load translation variant from CAS (%s): %w", transCAS, err)
			}
			defer trc.Close()
			if err := json.NewDecoder(trc).Decode(&tVariant); err != nil {
				return nil, fmt.Errorf("decode translation variant artifact (%s): %w", transCAS, err)
			}
			if explicitTransCAS != "" {
				if tVariant.AssetID != in.AssetID || !strings.EqualFold(tVariant.TargetLanguage, in.TargetLanguage) {
					return nil, fmt.Errorf("%w: explicit translation variant ownership mismatch for asset %q target %q",
						domain.ErrMeaningPreservationFailed, in.AssetID, in.TargetLanguage)
				}
			}
			if len(tVariant.Segments) == 0 {
				return nil, fmt.Errorf("canonical translation variant has zero segments")
			}
			if len(tVariant.Segments) != len(dVariant.Segments) {
				return nil, fmt.Errorf("%w: translation segment count %d != dub script segment count %d",
					domain.ErrMeaningPreservationFailed, len(tVariant.Segments), len(dVariant.Segments))
			}

			for i := range dVariant.Segments {
				dSeg := dVariant.Segments[i]
				tSeg := tVariant.Segments[i]

				if dSeg.Index != tSeg.Index {
					return nil, fmt.Errorf("%w: segment index mismatch at %d: translation=%d, dub=%d",
						domain.ErrMeaningPreservationFailed, i, tSeg.Index, dSeg.Index)
				}
				if dSeg.MeaningText == "" {
					return nil, fmt.Errorf("canonical translation variant meaning text missing for dub segment %d", dSeg.Index)
				}
				if tSeg.TargetText == "" {
					return nil, fmt.Errorf("canonical translation target text missing for segment %d", tSeg.Index)
				}
				if dSeg.MeaningText != tSeg.TargetText {
					return nil, fmt.Errorf("%w: dub script meaning text %q does not match canonical translation target text %q for segment %d",
						domain.ErrMeaningPreservationFailed, dSeg.MeaningText, tSeg.TargetText, tSeg.Index)
				}

				// Caption text strictly grounded in canonical TranslationVariant TargetText as source of truth
				textToRender := tSeg.TargetText
				startMs := dSeg.StartMs
				endMs := dSeg.EndMs
				if startMs == 0 && endMs == 0 {
					startMs = tSeg.StartMs
					endMs = tSeg.EndMs
				}

				cueProtects := domain.GetProtectedBoxesForTimeWindow(activePlan.Regions, in.SceneProtectedRegions, startMs, endMs, "")
				cue, err := domain.ComputeCompactSubtitleBoundsWithSelector(
					plan.FrameWidth,
					plan.FrameHeight,
					textToRender,
					0,
					0,
					0,
					cueProtects,
					selector,
				)
				if err != nil {
					return nil, fmt.Errorf("place subtitle cue for segment %d: %w", dSeg.Index, err)
				}
				cue.ID = fmt.Sprintf("cue-%d", dSeg.Index)
				cue.StartMs = startMs
				cue.EndMs = endMs
				subtitleCues = append(subtitleCues, cue)
			}
		} else {
			// A translation exists without a dub script. Use the caller-pinned CAS
			// when present; otherwise this is the legacy latest-index path.
			if canonicalTransCAS == "" {
				return nil, fmt.Errorf("translation variant index has empty CAS hash")
			}
			trc, err := s.cas.Get(canonicalTransCAS)
			if err != nil {
				return nil, fmt.Errorf("load translation variant from CAS (%s): %w", canonicalTransCAS, err)
			}
			defer trc.Close()
			var tVariant domain.TranslationVariant
			if err := json.NewDecoder(trc).Decode(&tVariant); err != nil {
				return nil, fmt.Errorf("decode translation variant artifact (%s): %w", canonicalTransCAS, err)
			}
			if explicitTransCAS != "" {
				if tVariant.AssetID != in.AssetID || !strings.EqualFold(tVariant.TargetLanguage, in.TargetLanguage) {
					return nil, fmt.Errorf("%w: explicit translation variant ownership mismatch for asset %q target %q",
						domain.ErrMeaningPreservationFailed, in.AssetID, in.TargetLanguage)
				}
			}
			if len(tVariant.Segments) == 0 {
				return nil, fmt.Errorf("canonical translation variant has zero segments")
			}
			for _, seg := range tVariant.Segments {
				textToRender := seg.TargetText
				if textToRender == "" {
					return nil, fmt.Errorf("canonical translation target text missing for segment %d", seg.Index)
				}
				cueProtects := domain.GetProtectedBoxesForTimeWindow(activePlan.Regions, in.SceneProtectedRegions, seg.StartMs, seg.EndMs, "")
				cue, err := domain.ComputeCompactSubtitleBoundsWithSelector(
					plan.FrameWidth,
					plan.FrameHeight,
					textToRender,
					0,
					0,
					0,
					cueProtects,
					selector,
				)
				if err != nil {
					return nil, fmt.Errorf("place subtitle cue for translation segment %d: %w", seg.Index, err)
				}
				cue.ID = fmt.Sprintf("cue-%d", seg.Index)
				cue.StartMs = seg.StartMs
				cue.EndMs = seg.EndMs
				subtitleCues = append(subtitleCues, cue)
			}
		}
	} else {
		// Fallback to speech_subtitle regions in TextRegionPlan ONLY when there is genuinely no localization artifact at all
		for i, reg := range activePlan.Regions {
			if reg.Role == domain.TextRoleSpeechSubtitle {
				cueProtects := domain.GetProtectedBoxesForTimeWindow(activePlan.Regions, in.SceneProtectedRegions, reg.FirstSeenMs, reg.LastSeenMs, "")
				cue, err := domain.ComputeCompactSubtitleBoundsWithSelector(
					plan.FrameWidth,
					plan.FrameHeight,
					reg.Text,
					0,
					0,
					0,
					cueProtects,
					selector,
				)
				if err != nil {
					return nil, fmt.Errorf("place subtitle cue for region %s: %w", reg.ID, err)
				}
				cue.ID = fmt.Sprintf("sub-%d", i)
				cue.StartMs = reg.FirstSeenMs
				cue.EndMs = reg.LastSeenMs
				subtitleCues = append(subtitleCues, cue)
			}
		}
	}
	// 6. Persist LocalizedSubtitleTrack
	subTrackProv, err := domain.ComputeSubtitlePlanProvenanceHash(in.AssetID, in.TargetLanguage, subtitleCues, "compact_fit_cues")
	if err != nil {
		return nil, fmt.Errorf("compute subtitle track provenance: %w", err)
	}
	if subTrackProv == "" {
		return nil, fmt.Errorf("compute subtitle track provenance returned empty hash")
	}
	subTrack := domain.LocalizedSubtitleTrack{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.LocalizedSubtitleTrackSchemaVersion,
		AssetID:        in.AssetID,
		RunID:          in.RunID,
		JobID:          in.JobID,
		TargetLanguage: in.TargetLanguage,
		Format:         "compact_fit_cues",
		Cues:           subtitleCues,
		ProvenanceHash: subTrackProv,
		CreatedAt:      time.Now().UTC(),
	}
	subTrackBytes, err := json.Marshal(subTrack)
	if err != nil {
		return nil, fmt.Errorf("marshal localized subtitle track: %w", err)
	}
	subTrackCASObj, err := s.cas.Put(bytes.NewReader(subTrackBytes))
	if err != nil {
		return nil, fmt.Errorf("put subtitle track in CAS: %w", err)
	}
	subTrack.CASHash = subTrackCASObj.SHA256

	err = s.db.SaveLocalizedSubtitleTrackIndex(ctx, storage.LocalizedSubtitleTrackIndex{
		ID:             subTrack.ID,
		AssetID:        subTrack.AssetID,
		RunID:          subTrack.RunID,
		JobID:          subTrack.JobID,
		TargetLanguage: subTrack.TargetLanguage,
		CASHash:        subTrack.CASHash,
		ProvenanceHash: subTrack.ProvenanceHash,
		CueCount:       len(subTrack.Cues),
		CreatedAt:      subTrack.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save localized subtitle track index: %w", err)
	}

	// 7. Persist LocalizedVisualTrack
	allProtectedBoxes := domain.GetProtectedBoxesForTimeWindow(activePlan.Regions, in.SceneProtectedRegions, 0, 0, "")

	dubScriptProvStr := ""
	if dubScriptIdx != nil {
		dubScriptProvStr = dubScriptIdx.ProvenanceHash
	}
	visProv, err := domain.ComputeLocalizedVisualTrackProvenanceHash(
		in.AssetID,
		in.TargetLanguage,
		plan.ProvenanceHash,
		dubScriptProvStr,
		overlays,
		subtitleCues,
		in.SceneProtectedRegions,
	)
	if err != nil {
		return nil, fmt.Errorf("compute visual track provenance: %w", err)
	}

	visTrack := domain.LocalizedVisualTrack{
		ID:                 uuid.NewString(),
		SchemaVersion:      domain.LocalizedVisualTrackSchemaVersion,
		AssetID:            in.AssetID,
		RunID:              in.RunID,
		JobID:              in.JobID,
		TargetLanguage:     in.TargetLanguage,
		TextRegionPlanCAS:  plan.CASHash,
		TextRegionPlanProv: plan.ProvenanceHash,
		SubtitleTrackCAS:   subTrack.CASHash,
		Overlays:           overlays,
		SubtitleCues:       subtitleCues,
		ProtectedRegions:   allProtectedBoxes,
		ProvenanceHash:     visProv,
		CreatedAt:          time.Now().UTC(),
	}
	visTrackBytes, err := json.Marshal(visTrack)
	if err != nil {
		return nil, fmt.Errorf("marshal visual track: %w", err)
	}
	visCASObj, err := s.cas.Put(bytes.NewReader(visTrackBytes))
	if err != nil {
		return nil, fmt.Errorf("put visual track in CAS: %w", err)
	}
	visTrack.CASHash = visCASObj.SHA256

	err = s.db.SaveLocalizedVisualTrackIndex(ctx, storage.LocalizedVisualTrackIndex{
		ID:                visTrack.ID,
		AssetID:           visTrack.AssetID,
		RunID:             visTrack.RunID,
		JobID:             visTrack.JobID,
		TargetLanguage:    visTrack.TargetLanguage,
		TextRegionPlanCAS: visTrack.TextRegionPlanCAS,
		CASHash:           visTrack.CASHash,
		ProvenanceHash:    visTrack.ProvenanceHash,
		CreatedAt:         visTrack.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save localized visual track index: %w", err)
	}

	return &visTrack, nil
}
