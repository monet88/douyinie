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

// stageArtifactHash returns the artifact a stage of this run recorded, i.e. what the stage actually
// consumed or produced. A run that only reused cached artifacts has no variant index row of its own, and
// the stage execution is what still binds it to the artifact.
func (s *VisualTextService) stageArtifactHash(ctx context.Context, runID, stage string) string {
	if s.db == nil {
		return ""
	}
	casHash, err := s.db.GetStageArtifactHash(ctx, runID, stage)
	if err != nil {
		return ""
	}
	return casHash
}

// sampleSlot maps a wall-clock detection time onto the sampling grid slot the OCR pass used, so
// keyframe interpolation stays in sampling space even though the provider numbers frames absolutely.
func sampleSlot(timestampMs, stepMs int64) int {
	if stepMs <= 0 {
		return int(timestampMs)
	}
	return int(timestampMs / stepMs)
}

// rawDetectionCluster groups observations across frames that belong to the same on-screen text entity.
type rawDetectionCluster struct {
	text           string
	observations   []provider.RawTextDetection
	avgBox         domain.BoundingBox
	meanConfidence float64
}

// recomputeAggregates refreshes the cluster's representative token (its highest-confidence reading,
// i.e. the best guess at what the box actually says) and its mean box and confidence.
func (c *rawDetectionCluster) recomputeAggregates() {
	if len(c.observations) == 0 {
		return
	}
	var sumX, sumY, sumW, sumH, confSum float64
	best := c.observations[0]
	for _, o := range c.observations {
		sumX += float64(o.Box.X)
		sumY += float64(o.Box.Y)
		sumW += float64(o.Box.Width)
		sumH += float64(o.Box.Height)
		confSum += o.Confidence
		if o.Confidence > best.Confidence {
			best = o
		}
	}
	n := float64(len(c.observations))
	c.avgBox = domain.BoundingBox{
		X: int(sumX / n), Y: int(sumY / n), Width: int(sumW / n), Height: int(sumH / n),
	}
	c.meanConfidence = confSum / n
	c.text = best.Text
}

// isUnreliableReading reports whether a reading is a guess about glyphs rather than a readable
// token. The threshold is the classifier's review threshold, i.e. the confidence above which the
// pipeline treats a recognized token as content: below it the token is not evidence of what the box
// says, only of where the box is.
func isUnreliableReading(text string, confidence float64, cfg domain.TextRegionClassifyConfig) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	trust := cfg.MinConfidence
	if trust <= 0 {
		trust = 0.55
	}
	return confidence < trust
}

// boxOverlapRatio returns the intersection area relative to the smaller of the two boxes: how much
// of the smaller box the other covers. It answers "is this the same on-screen box?" rather than
// "do these two detections describe the same extent?" (IoU), which is what tracking needs.
func boxOverlapRatio(a, b domain.BoundingBox) float64 {
	ix := min(a.X+a.Width, b.X+b.Width) - max(a.X, b.X)
	iy := min(a.Y+a.Height, b.Y+b.Height) - max(a.Y, b.Y)
	if ix <= 0 || iy <= 0 {
		return 0
	}
	smaller := min(a.Width*a.Height, b.Width*b.Height)
	if smaller <= 0 {
		return 0
	}
	return float64(ix*iy) / float64(smaller)
}

func buildAndInterpolateTracks(dets []provider.RawTextDetection, stepMs int64, cfg domain.TextRegionClassifyConfig) []domain.TrackedTextRegion {
	if len(dets) == 0 {
		return []domain.TrackedTextRegion{}
	}

	// 1. Cluster detections belonging to the same on-screen text entity.
	//
	// Text equality is the primary key, but it cannot be the only one: when the recognizer is
	// unreliable it returns a DIFFERENT garbage token for the same physical caption on every sample
	// (live evidence, run 4f86657f: `别品同` -> `别品` -> `济室` -> `点酒房` over 11000-12500 ms), which
	// shatters one caption into four single-sample regions and drops it from the plan entirely.
	// Two temporally adjacent (<= stepMs*4), spatially overlapping readings that are BOTH below the
	// trust threshold are therefore the same box with two bad reads, not two entities.
	var clusters []rawDetectionCluster
	for _, d := range dets {
		matchedIdx := -1
		for idx, cl := range clusters {
			lastObs := cl.observations[len(cl.observations)-1]
			adjacent := math.Abs(float64(d.TimestampMs-lastObs.TimestampMs)) <= float64(stepMs*4)
			sameText := strings.EqualFold(strings.TrimSpace(cl.text), strings.TrimSpace(d.Text))
			switch {
			case sameText && adjacent:
				matchedIdx = idx
			case adjacent && isUnreliableReading(cl.text, cl.meanConfidence, cfg) &&
				isUnreliableReading(d.Text, d.Confidence, cfg) && boxOverlapRatio(cl.avgBox, d.Box) >= 0.5:
				matchedIdx = idx
			}
			if matchedIdx >= 0 {
				break
			}
		}
		if matchedIdx >= 0 {
			clusters[matchedIdx].observations = append(clusters[matchedIdx].observations, d)
			clusters[matchedIdx].recomputeAggregates()
		} else {
			cl := rawDetectionCluster{text: d.Text, observations: []provider.RawTextDetection{d}}
			cl.recomputeAggregates()
			clusters = append(clusters, cl)
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
			return cl.observations[i].TimestampMs < cl.observations[j].TimestampMs
		})

		firstObs := cl.observations[0]
		lastObs := cl.observations[len(cl.observations)-1]

		// Keyframes live on the SAMPLING grid, not on the video frame grid. The OCR adapter reports
		// FrameIndex as the absolute video frame number (round(timestamp_ms/1000*fps)) while
		// timestamp_ms is the requested sample time, so keyframe slots are derived from timestamps:
		// treating FrameIndex as a sample ordinal multiplied interpolated timestamps by fps*step/1000
		// (live evidence, run 4f86657f: a 8500-9000 ms caption carried keyframes claiming 128000 ms).
		obsBySlot := make(map[int]provider.RawTextDetection)
		minConf := 1.0
		for _, obs := range cl.observations {
			obsBySlot[sampleSlot(obs.TimestampMs, stepMs)] = obs
			if obs.Confidence < minConf {
				minConf = obs.Confidence
			}
		}

		obsCount := len(cl.observations)
		avgBox := cl.avgBox
		meanConf := cl.meanConfidence
		// Collect spatio-temporally nearby observations from OTHER detections: readings of the same
		// place within +/- 4 sampling steps. The window is measured in time, not in frame numbers -
		// the OCR adapter numbers frames absolutely (30 fps), so a +/- 4 frame window is +/- 133 ms
		// and would never see the neighbouring samples it exists to compare against (live evidence,
		// run 4f86657f: "BLGOK" sat next to "CottGG"/"Cottce"/"Cott6e" 500 ms apart and stayed a
		// semantic_text label because the window never reached them).
		//
		// The cluster's own observations are excluded: a region that was read differently on every
		// sample of its own is exactly the evidence this gate needs, but a merged low-confidence
		// caption is judged by the classifier's caption rules, not by its own misreadings.
		ownSlots := make(map[int]bool, len(cl.observations))
		for _, obs := range cl.observations {
			ownSlots[sampleSlot(obs.TimestampMs, stepMs)] = true
		}
		var nearbyObs []domain.NearbyObservation
		for _, d := range dets {
			if d.TimestampMs < firstObs.TimestampMs-stepMs*4 || d.TimestampMs > lastObs.TimestampMs+stepMs*4 {
				continue
			}
			if ownSlots[sampleSlot(d.TimestampMs, stepMs)] {
				continue
			}
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

		// Classify role deterministically with spatio-temporal instability tracking
		ownBoxes := make([]domain.BoundingBox, 0, len(cl.observations))
		for _, obs := range cl.observations {
			ownBoxes = append(ownBoxes, obs.Box)
		}
		role, protectedMeta, reviewReq, reviewReason := domain.ClassifyRegionWithInstability(cl.text, avgBox, meanConf, cfg, len(obsBySlot), nearbyObs, ownBoxes)
		// Build keyframes with linear interpolation for frame gaps <= 2 steps
		var keyframes []domain.RegionKeyframe
		interpolatedCount := 0

		firstSlot, lastSlot := sampleSlot(firstObs.TimestampMs, stepMs), sampleSlot(lastObs.TimestampMs, stepMs)
		for slot := firstSlot; slot <= lastSlot; slot++ {
			fTimeMs := int64(slot) * stepMs
			if obs, exists := obsBySlot[slot]; exists {
				keyframes = append(keyframes, domain.RegionKeyframe{
					FrameIndex:  obs.FrameIndex,
					TimestampMs: obs.TimestampMs,
					Box:         obs.Box,
					Confidence:  obs.Confidence,
					Observed:    true,
				})
			} else {
				// Linear interpolation between closest preceding and following observations
				var prevObs, nextObs *provider.RawTextDetection
				for p := slot - 1; p >= firstSlot; p-- {
					if o, ok := obsBySlot[p]; ok {
						prevObs = &o
						break
					}
				}
				for n := slot + 1; n <= lastSlot; n++ {
					if o, ok := obsBySlot[n]; ok {
						nextObs = &o
						break
					}
				}

				var interpBox domain.BoundingBox
				var interpConf float64
				if prevObs != nil && nextObs != nil {
					prevSlot, nextSlot := sampleSlot(prevObs.TimestampMs, stepMs), sampleSlot(nextObs.TimestampMs, stepMs)
					alpha := float64(slot-prevSlot) / float64(nextSlot-prevSlot)
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
					FrameIndex:  prevObs.FrameIndex,
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
	RunID                 string                        `json:"run_id"`
	AssetID               string                        `json:"asset_id"`
	JobID                 string                        `json:"job_id,omitempty"`
	TargetLanguage        string                        `json:"target_language"`
	TranslationVariantCAS string                        `json:"translation_variant_cas,omitempty"`
	Overrides             []domain.RegionOverride       `json:"overrides,omitempty"`
	InpaintingFallbacks   []string                      `json:"inpainting_fallbacks,omitempty"` // Region IDs where inpainting fallback is explicitly requested
	SceneProtectedRegions []domain.SceneProtectedRegion `json:"scene_protected_regions,omitempty"`
	// StrictOverlapRegionIDs names the regions whose geometry the operator just changed. A
	// collision on one of THESE is a rejected edit (the operator gets actionable feedback and
	// can drag elsewhere); collisions on any other region are pre-existing layout facts and keep
	// following the automatic path - overlay skipped, visual_occlusion exception surfaced - so a
	// correction is not blocked by a region the operator has not touched yet.
	StrictOverlapRegionIDs []string                         `json:"strict_overlap_region_ids,omitempty"`
	PlacementSelector      domain.SubtitlePlacementSelector `json:"-"`
	ExecutionProfile       domain.ExecutionProfile          `json:"execution_profile,omitempty"`
	AuthorizedCredentials  []string                         `json:"authorized_credentials,omitempty"`
	ConsentGranted         bool                             `json:"consent_granted,omitempty"`
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

	frameW, frameH := plan.FrameBounds()

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

				// Enforce the domain's minimum dimension
				if b.Width < domain.MinTextRegionBoxPx {
					b.Width = domain.MinTextRegionBoxPx
				}
				if b.Height < domain.MinTextRegionBoxPx {
					b.Height = domain.MinTextRegionBoxPx
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

// sceneProtectedBoxes returns the boxes the pipeline itself declared protected (faces, tap
// targets, timeline controls) that are active in the region's window. A collision with one of
// these is not a layout the operator can accept, so it stays fail-closed.
func sceneProtectedBoxes(in LocalizeVisualTrackInput, reg domain.TrackedTextRegion) []domain.BoundingBox {
	return domain.GetProtectedBoxesForTimeWindow(nil, in.SceneProtectedRegions, reg.FirstSeenMs, reg.LastSeenMs, reg.ID)
}

// protectedRegionBoxes returns the boxes of other tracked regions the classifier marked
// protected (UI controls, brand marks) that are active in the region's window. A collision
// with one of these skips the overlay and surfaces a visual_occlusion exception instead of
// dead-ending the run (architecture §9.1), because the operator can move or reclassify either
// region.
func protectedRegionBoxes(plan *domain.TextRegionPlan, reg domain.TrackedTextRegion) []domain.BoundingBox {
	if plan == nil {
		return nil
	}
	return domain.GetProtectedBoxesForTimeWindow(plan.Regions, nil, reg.FirstSeenMs, reg.LastSeenMs, reg.ID)
}

// firstProtectedOverlap returns the first protected box the overlay box overlaps, in the
// order the caller supplied.
func firstProtectedOverlap(box domain.BoundingBox, protected []domain.BoundingBox) (domain.BoundingBox, bool) {
	for _, prot := range protected {
		if domain.BoxesOverlap(box, prot) {
			return prot, true
		}
	}
	return domain.BoundingBox{}, false
}

// coverPaddingPx absorbs the anti-aliased edge of a burned-in caption so the cover box does
// not leave a one-pixel halo of the original text behind.
const coverPaddingPx = 6

// resolveOverlayCollisions drops in-place replacements that would land on another replacement sharing the
// screen, and reports each dropped region as an occlusion exception.
//
// Compositing two replacements over one another paints one black box across the other's translated text
// (live evidence, live-20260918: the four semantic_text regions of the ramen pack - 净含量：面饼, 辣鸡肉味拌面,
// HOCHI, 面饼：107克 - all live at 4500-5500ms and drew nested boxes that clipped each other). The dominant
// label of a colliding group wins; the loser keeps its source text untouched and goes to the operator as a
// layout decision instead of shipping a cut-off translation.
func resolveOverlayCollisions(overlays []domain.LocalizedOverlayItem) ([]domain.LocalizedOverlayItem, []domain.OcclusionReport) {
	kept := make([]domain.LocalizedOverlayItem, 0, len(overlays))
	var occlusions []domain.OcclusionReport
	for _, ov := range overlays {
		displaced := -1
		yields := false
		for i, other := range kept {
			if ov.StartMs >= other.EndMs || other.StartMs >= ov.EndMs || !overlayBoxesIntersect(ov.Box, other.Box) {
				continue
			}
			if overlayDominates(ov, other) {
				displaced = i
			} else {
				yields = true
				occlusions = append(occlusions, overlayCollisionReport(ov, other))
			}
			break
		}
		switch {
		case displaced >= 0:
			occlusions = append(occlusions, overlayCollisionReport(kept[displaced], ov))
			kept[displaced] = ov
		case !yields:
			kept = append(kept, ov)
		}
	}
	if len(occlusions) == 0 {
		return overlays, nil
	}
	return kept, occlusions
}

// overlayDominates reports whether a displaces b: the larger observed box is the dominant label of a
// colliding group, and the region ID breaks ties so the outcome never depends on plan order.
func overlayDominates(a, b domain.LocalizedOverlayItem) bool {
	areaA, areaB := a.Box.Width*a.Box.Height, b.Box.Width*b.Box.Height
	if areaA != areaB {
		return areaA > areaB
	}
	return a.RegionID < b.RegionID
}

// overlayCollisionReport names the replacement that won the box, so the operator sees what their dropped
// region collided with instead of an unexplained missing translation.
func overlayCollisionReport(dropped, winner domain.LocalizedOverlayItem) domain.OcclusionReport {
	return domain.OcclusionReport{
		RegionID:     dropped.RegionID,
		Role:         dropped.Role,
		SourceText:   dropped.SourceText,
		OverlayBox:   dropped.Box,
		ProtectedBox: winner.Box,
		StartMs:      dropped.StartMs,
		EndMs:        dropped.EndMs,
	}
}

func overlayBoxesIntersect(a, b domain.BoundingBox) bool {
	return a.X < b.X+b.Width && b.X < a.X+a.Width && a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

// seatReplacementCuesOnCovers moves every replacement cue onto the cover of the source caption it
// replaces and grows that cover around it, so one opaque block carries the localized text.
//
// Left in the default 75% lane, the replacement showed as a second dark box under the cover: same
// screen, different size and offset (live evidence, run 27a758e6 at 1.6s - source cover
// [225,936,639,96] over the burned-in caption, cue [161,1080,757,45] beneath it). A burned-in
// caption is replaced in place, so the cue keeps its fit-content size, centers on the cover's band,
// and the cover grows to frame it. The cover only ever grows sideways: the seated cue sits inside
// the cover's rows, so the block stays inside the caption band it replaces.
//
// A seat that would occlude a protected obstacle (scene-declared face/tap target or a protected
// tracked region) is refused and the cue keeps its lane placement, which is where the lane search
// already put it.
func seatReplacementCuesOnCovers(
	cues []domain.SubtitleCue,
	covers []domain.CoverBox,
	plan *domain.TextRegionPlan,
	sceneProtected []domain.SceneProtectedRegion,
) ([]domain.SubtitleCue, []domain.CoverBox) {
	if plan == nil || plan.FrameWidth <= 0 || plan.FrameHeight <= 0 || len(cues) == 0 || len(covers) == 0 {
		return cues, covers
	}
	clamp := func(value, span, limit int) int {
		if value < 0 {
			return 0
		}
		if value+span > limit {
			return max(0, limit-span)
		}
		return value
	}

	seated := append([]domain.SubtitleCue(nil), cues...)
	boxes := append([]domain.CoverBox(nil), covers...)
	for i := range seated {
		cue := seated[i]
		target := -1
		var bestOverlap int64
		for j := range boxes {
			overlap := min(cue.EndMs, boxes[j].EndMs) - max(cue.StartMs, boxes[j].StartMs)
			if overlap > bestOverlap { // strict: the earliest cover wins a tie
				target, bestOverlap = j, overlap
			}
		}
		if target < 0 {
			continue
		}
		cover := boxes[target]
		moved := cue
		moved.X = clamp(cover.X+(cover.Width-cue.Width)/2, cue.Width, plan.FrameWidth)
		moved.Y = clamp(cover.Y+(cover.Height-cue.Height)/2, cue.Height, plan.FrameHeight)

		left := min(cover.X, moved.X) - coverPaddingPx
		top := min(cover.Y, moved.Y) - coverPaddingPx
		grown := domain.BoundingBox{
			X:      max(0, left),
			Y:      max(0, top),
			Width:  min(plan.FrameWidth, max(cover.X+cover.Width, moved.X+moved.Width)+coverPaddingPx) - max(0, left),
			Height: min(plan.FrameHeight, max(cover.Y+cover.Height, moved.Y+moved.Height)+coverPaddingPx) - max(0, top),
		}
		if _, occluded := firstProtectedOverlap(grown, domain.GetProtectedBoxesForTimeWindow(plan.Regions, sceneProtected, cue.StartMs, cue.EndMs, "")); occluded {
			continue
		}
		seated[i] = moved
		boxes[target].X, boxes[target].Y, boxes[target].Width, boxes[target].Height = grown.X, grown.Y, grown.Width, grown.Height
	}
	return seated, boxes
}

// buildSubtitleCovers derives the opaque cover set that hides source burned-in captions.
//
// The cover of a speech_subtitle region is the region's own tracked box (grown by a small padding
// that absorbs the anti-aliased stroke edges) and is active for exactly the window the source
// caption was on screen. seatReplacementCuesOnCovers widens each cover afterwards to frame the
// replacement text seated on it, because a dub sentence runs longer and wider than the source
// caption it replaces.
//
// A cover that would occlude a protected obstacle (scene-declared face/tap target or a tracked
// protected region) is not emitted: the region is surfaced as a visual_occlusion exception, the
// same rule the in-place overlays follow (architecture §9.1), because the operator can move
// either region.
func buildSubtitleCovers(
	plan *domain.TextRegionPlan,
	in LocalizeVisualTrackInput,
) ([]domain.CoverBox, []domain.OcclusionReport) {
	if plan == nil || plan.FrameWidth <= 0 || plan.FrameHeight <= 0 {
		return nil, nil
	}
	var covers []domain.CoverBox
	var occlusions []domain.OcclusionReport
	// The observed window of each emitted cover, kept beside it so the padded windows of captions that
	// never shared the screen can be split apart again (see splitSequentialCoverWindows).
	var observed [][2]int64

	for _, reg := range plan.Regions {
		if reg.Role != domain.TextRoleSpeechSubtitle {
			continue
		}
		box, ok := reg.Bounds()
		if !ok {
			continue
		}
		startMs, endMs := reg.FirstSeenMs-captionWindowPadMs(reg), reg.LastSeenMs+captionWindowPadMs(reg)
		if startMs < 0 {
			startMs = 0
		}
		// A caption seen on exactly one sample has no grid step to bracket, and a zero-length window
		// is not a renderable cover. Cover at least the sampling interval the detection stands for.
		if endMs-startMs < defaultCaptionCoverMs {
			endMs = startMs + defaultCaptionCoverMs
		}

		cover := clampCoverBox(box, plan.FrameWidth, plan.FrameHeight, coverPaddingPx)
		cover.RegionID = reg.ID
		cover.Role = string(reg.Role)
		cover.StartMs = startMs
		cover.EndMs = endMs
		cover.Color = "#000000"
		cover.Opacity = 1.0
		if err := domain.ValidateCoverBox(cover, plan.FrameWidth, plan.FrameHeight); err != nil {
			continue
		}

		coverGeom := domain.BoundingBox{X: cover.X, Y: cover.Y, Width: cover.Width, Height: cover.Height}
		if prot, occluded := firstProtectedOverlap(coverGeom, sceneProtectedBoxes(in, reg)); occluded {
			occlusions = append(occlusions, domain.OcclusionReport{
				RegionID:     reg.ID,
				Role:         reg.Role,
				SourceText:   reg.Text,
				OverlayBox:   coverGeom,
				ProtectedBox: prot,
				StartMs:      startMs,
				EndMs:        endMs,
			})
			continue
		}
		if prot, occluded := firstProtectedOverlap(coverGeom, protectedRegionBoxes(plan, reg)); occluded {
			occlusions = append(occlusions, domain.OcclusionReport{
				RegionID:     reg.ID,
				Role:         reg.Role,
				SourceText:   reg.Text,
				OverlayBox:   coverGeom,
				ProtectedBox: prot,
				StartMs:      startMs,
				EndMs:        endMs,
			})
			continue
		}
		covers = append(covers, cover)
		observed = append(observed, [2]int64{reg.FirstSeenMs, reg.LastSeenMs})
	}
	return resolveSequentialCoverOverlaps(covers, observed), occlusions
}

// resolveSequentialCoverOverlaps makes the padded cover windows of *different* captions disjoint while
// leaving every one of them fully covered.
//
// A cover is padded by the sampling step on each side so the caption is hidden across the whole interval it
// could have been on screen, and two consecutive captions' padded windows then overlap on that step. Drawing
// both bars there leaves a ragged doubled bar with a strip sticking out (live evidence, live-20260918:
// region-002 [0,2000] x=305 over region-004 [1500,3500] x=231); splitting the overlap and drawing each box
// only over its own span instead exposed the incoming caption, whose line is wider than the outgoing one
// (the same live clip at 1.6s showed 你 / 角 outside the bar). The handover is genuinely ambiguous, so the
// contested span gets ONE bar over the union of both boxes: exactly one bar is composited at any instant and
// no source text is left visible. Captions whose observed windows actually met keep their own overlapping
// covers - they were on screen together.
func resolveSequentialCoverOverlaps(covers []domain.CoverBox, observed [][2]int64) []domain.CoverBox {
	if len(covers) != len(observed) || len(covers) < 2 {
		return covers
	}
	resolved := append([]domain.CoverBox(nil), covers...)
	var handovers []domain.CoverBox
	for i := range covers {
		for j := i + 1; j < len(covers); j++ {
			// Observed together: both captions were on screen, both keep their covers.
			if observed[i][1] > observed[j][0] && observed[j][1] > observed[i][0] {
				continue
			}
			if !coverBoxesIntersect(covers[i], covers[j]) {
				continue
			}
			earlier, later := i, j
			if observed[i][0] > observed[j][0] {
				earlier, later = j, i
			}
			handoverStart := max(resolved[earlier].StartMs, resolved[later].StartMs)
			handoverEnd := min(resolved[earlier].EndMs, resolved[later].EndMs)
			if handoverStart >= handoverEnd {
				continue
			}
			handover := domain.CoverBox{
				RegionID: fmt.Sprintf("handover-%s-%s", covers[earlier].RegionID, covers[later].RegionID),
				Role:     covers[earlier].Role,
				X:        min(covers[earlier].X, covers[later].X),
				Y:        min(covers[earlier].Y, covers[later].Y),
				Width:    max(covers[earlier].X+covers[earlier].Width, covers[later].X+covers[later].Width) - min(covers[earlier].X, covers[later].X),
				Height:   max(covers[earlier].Y+covers[earlier].Height, covers[later].Y+covers[later].Height) - min(covers[earlier].Y, covers[later].Y),
				StartMs:  handoverStart,
				EndMs:    handoverEnd,
				Color:    covers[earlier].Color,
				Opacity:  covers[earlier].Opacity,
			}
			resolved[earlier].EndMs = handoverStart
			resolved[later].StartMs = handoverEnd
			handovers = append(handovers, handover)
		}
	}
	if len(handovers) == 0 {
		return covers
	}
	out := append(resolved, handovers...)
	sort.Slice(out, func(a, b int) bool {
		if out[a].StartMs != out[b].StartMs {
			return out[a].StartMs < out[b].StartMs
		}
		return out[a].RegionID < out[b].RegionID
	})
	return out
}

// coverBoxesIntersect reports whether two composited covers share any pixel.
func coverBoxesIntersect(a, b domain.CoverBox) bool {
	return a.X < b.X+b.Width && b.X < a.X+a.Width && a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

// captionWindowPadMs is how far a caption's cover window extends beyond its first and last
// detection. The OCR samples the video on a fixed grid, so a detection at t proves the caption was
// on screen at t but not whether it appeared or vanished a moment later: live evidence (run
// 4f86657f) has the caption detected at 11000-12500 ms while still visible at 12750 ms and already
// visible at 10750 ms, i.e. ~0.5 s of uncovered source text at each end without the pad. The pad is
// the detection grid step read off the region's own keyframes, so it follows the sampling cadence
// instead of a hard-coded constant.
func captionWindowPadMs(reg domain.TrackedTextRegion) int64 {
	step := int64(0)
	for i := 1; i < len(reg.Keyframes); i++ {
		delta := reg.Keyframes[i].TimestampMs - reg.Keyframes[i-1].TimestampMs
		if delta > 0 {
			step = delta
			break
		}
	}
	if step <= 0 || step > maxCaptionWindowPadMs {
		return 0
	}
	return step
}

// maxCaptionWindowPadMs bounds the uncertainty pad so a sparse region cannot claim a window far
// beyond its own evidence.
const maxCaptionWindowPadMs = 1000

// captionCueMaxChars is how much caption text a single cue may carry, i.e. about two lines of the
// compact-fit style (font 24, border 18) over a 1080 px frame. Live evidence (run 4f86657f): one
// 12 s segment carrying four sentences rendered as a single three-line block that covered a third of
// the frame and stayed up for the whole segment.
const captionCueMaxChars = 84

// degenerateCaptionWindowMs is the window a segment with no usable duration is given. Cues share
// their segment's window in proportion to their text, so a segment of unknown length still yields
// one readable cue.
const degenerateCaptionWindowMs = 700

// splitCaptionText breaks caption text into reading-sized pieces. It packs word by word up to
// maxChars and prefers to break after sentence punctuation once a piece is at least half full, so
// cues follow the shape of speech instead of cutting mid-thought. Every word survives, so the
// captions stay grounded in the canonical translation text.
func splitCaptionText(text string, maxChars int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if maxChars <= 0 {
		maxChars = captionCueMaxChars
	}
	var pieces []string
	var current []string
	currentLen := 0
	flush := func() {
		if len(current) > 0 {
			pieces = append(pieces, strings.Join(current, " "))
			current = nil
			currentLen = 0
		}
	}
	for _, w := range words {
		wLen := len([]rune(w))
		if currentLen > 0 && currentLen+1+wLen > maxChars {
			flush()
		}
		current = append(current, w)
		currentLen += wLen + 1
		if endsSentence(w) && currentLen >= maxChars/2 {
			flush()
		}
	}
	flush()
	return pieces
}

// endsSentence reports whether a word closes a sentence or clause in the target languages.
func endsSentence(word string) bool {
	trimmed := strings.TrimRight(word, `"'”’)`)
	if trimmed == "" {
		return false
	}
	last := []rune(trimmed)[len([]rune(trimmed))-1]
	return last == '.' || last == '!' || last == '?' || last == '。' || last == '！' || last == '？'
}

// captionCueWindows spreads a segment's window across its pieces in proportion to the text each piece
// carries, so a caption is on screen for roughly as long as it is spoken. A segment too short for its
// pieces yields fewer, longer cues rather than a flicker.
func captionCueWindows(startMs, endMs int64, pieces []string) [][2]int64 {
	windows := make([][2]int64, 0, len(pieces))
	if len(pieces) == 0 {
		return windows
	}
	if endMs <= startMs {
		endMs = startMs + degenerateCaptionWindowMs
	}
	totalLen := 0
	lengths := make([]int, len(pieces))
	for i, piece := range pieces {
		lengths[i] = len([]rune(piece))
		totalLen += lengths[i]
	}
	if totalLen == 0 {
		totalLen = 1
	}
	duration := endMs - startMs
	cursor := startMs
	for i := range pieces {
		if i == len(pieces)-1 {
			windows = append(windows, [2]int64{cursor, endMs})
			break
		}
		share := int64(lengths[i]) * duration / int64(totalLen)
		if share < 1 {
			share = 1
		}
		next := cursor + share
		if remaining := len(pieces) - i - 1; next > endMs-int64(remaining) {
			next = endMs - int64(remaining)
		}
		if next <= cursor {
			next = cursor + 1
		}
		windows = append(windows, [2]int64{cursor, next})
		cursor = next
	}
	return windows
}

// placeSegmentCues splits one segment's caption text into reading-sized cues and places each of them
// in the frame with the compact-fit box semantics.
func (s *VisualTextService) placeSegmentCues(
	plan *domain.TextRegionPlan,
	regions []domain.TrackedTextRegion,
	sceneProtected []domain.SceneProtectedRegion,
	selector domain.SubtitlePlacementSelector,
	segmentIndex int,
	text string,
	startMs, endMs int64,
) ([]domain.SubtitleCue, error) {
	pieces := splitCaptionText(text, captionCueMaxChars)
	if len(pieces) == 0 {
		return nil, nil
	}
	windows := captionCueWindows(startMs, endMs, pieces)
	cues := make([]domain.SubtitleCue, 0, len(pieces))
	for i, piece := range pieces {
		protects := domain.GetProtectedBoxesForTimeWindow(regions, sceneProtected, windows[i][0], windows[i][1], "")
		cue, err := domain.ComputeCompactSubtitleBoundsWithSelector(
			plan.FrameWidth, plan.FrameHeight, piece, 0, 0, 0, protects, selector,
		)
		if err != nil {
			return nil, fmt.Errorf("place subtitle cue for segment %d part %d: %w", segmentIndex, i, err)
		}
		cue.ID = fmt.Sprintf("cue-%d", segmentIndex)
		if i > 0 {
			cue.ID = fmt.Sprintf("cue-%d-%d", segmentIndex, i)
		}
		cue.StartMs, cue.EndMs = windows[i][0], windows[i][1]
		cues = append(cues, cue)
	}
	return cues, nil
}

// defaultCaptionCoverMs is the shortest cover window: the default OCR sampling step, i.e. the
// slice of video a single detection stands for.
const defaultCaptionCoverMs = 500

// clampCoverBox grows a box by padding and clamps it into the frame.
func clampCoverBox(box domain.BoundingBox, frameWidth, frameHeight, padding int) domain.CoverBox {
	x := max(0, box.X-padding)
	y := max(0, box.Y-padding)
	right := min(frameWidth, box.X+box.Width+padding)
	bottom := min(frameHeight, box.Y+box.Height+padding)
	return domain.CoverBox{X: x, Y: y, Width: right - x, Height: bottom - y}
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
			if errors.Is(err, storage.ErrNotFound) {
				// A run that reused a cached translation owns no index row of its own; the artifact
				// is still bound to this run by the stage execution that consumed it. Without this
				// the visual lane finds no variant and silently burns the SOURCE caption text as the
				// localized subtitle (live evidence, run 3adede59: a fresh run on a fully cached
				// pipeline rendered the Chinese captions over their own covers).
				if casHash := s.stageArtifactHash(ctx, in.RunID, "translation"); casHash != "" {
					transIdx = &storage.TranslationVariantIndex{
						AssetID:        in.AssetID,
						RunID:          in.RunID,
						TargetLanguage: in.TargetLanguage,
						CASHash:        casHash,
					}
					err = nil
				}
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
	strictOverlapSet := make(map[string]bool, len(in.StrictOverlapRegionIDs))
	for _, id := range in.StrictOverlapRegionIDs {
		strictOverlapSet[id] = true
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
	var occlusions []domain.OcclusionReport
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
				// Inline overlay text: resolved but never published as the run's canonical
				// translation variant (the speech stages own that index).
				Ephemeral: true,
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
			if prot, occluded := firstProtectedOverlap(repBox, sceneProtectedBoxes(in, reg)); occluded {
				// A face/tap target the pipeline itself declared protected is not a layout
				// the operator can accept: fail closed, as before.
				return nil, fmt.Errorf("%w: overlay for semantic text %q (box %+v) occludes protected region (box %+v)",
					domain.ErrSubtitleOverlapsProtectedRegion, reg.ID, repBox, prot)
			}
			if prot, occluded := firstProtectedOverlap(repBox, protectedRegionBoxes(activePlan, reg)); occluded {
				if strictOverlapSet[reg.ID] {
					return nil, fmt.Errorf("%w: overlay for semantic text %q (box %+v) occludes protected region (box %+v)",
						domain.ErrSubtitleOverlapsProtectedRegion, reg.ID, repBox, prot)
				}
				// Another protected tracked region (UI control, brand mark): the overlay is
				// skipped - the source text stays on screen untouched - and the region is
				// surfaced as a pending visual_occlusion exception (architecture 9.1).
				occlusions = append(occlusions, domain.OcclusionReport{
					RegionID:     reg.ID,
					Role:         reg.Role,
					SourceText:   reg.Text,
					OverlayBox:   repBox,
					ProtectedBox: prot,
					StartMs:      reg.FirstSeenMs,
					EndMs:        reg.LastSeenMs,
				})
				continue
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
				// Inline overlay text: resolved but never published as the run's canonical
				// translation variant (the speech stages own that index).
				Ephemeral: true,
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

			// Non-occlusion check: same rule as the semantic-text branch - scene-protected
			// obstacles still fail closed, protected tracked regions are surfaced.
			if prot, occluded := firstProtectedOverlap(repBox, sceneProtectedBoxes(in, reg)); occluded {
				return nil, fmt.Errorf("%w: overlay for instructional UI %q (box %+v) occludes protected region (box %+v)",
					domain.ErrSubtitleOverlapsProtectedRegion, reg.ID, repBox, prot)
			}
			if prot, occluded := firstProtectedOverlap(repBox, protectedRegionBoxes(activePlan, reg)); occluded {
				if strictOverlapSet[reg.ID] {
					return nil, fmt.Errorf("%w: overlay for instructional UI %q (box %+v) occludes protected region (box %+v)",
						domain.ErrSubtitleOverlapsProtectedRegion, reg.ID, repBox, prot)
				}
				occlusions = append(occlusions, domain.OcclusionReport{
					RegionID:     reg.ID,
					Role:         reg.Role,
					SourceText:   reg.Text,
					OverlayBox:   repBox,
					ProtectedBox: prot,
					StartMs:      reg.FirstSeenMs,
					EndMs:        reg.LastSeenMs,
				})
				continue
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

	// 4b. Resolve in-place replacement collisions before anything is composited.
	overlays, overlayCollisions := resolveOverlayCollisions(overlays)
	occlusions = append(occlusions, overlayCollisions...)

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
		if errors.Is(err, storage.ErrNotFound) {
			// Same cache-hit binding as the translation index above: the dub script stage of this run
			// recorded the artifact it consumed, even though the variant row belongs to the run that
			// produced it.
			if casHash := s.stageArtifactHash(ctx, in.RunID, "dub_script"); casHash != "" {
				dubScriptIdx = &storage.DubScriptVariantIndex{
					AssetID:        in.AssetID,
					RunID:          in.RunID,
					TargetLanguage: in.TargetLanguage,
					CASHash:        casHash,
				}
				err = nil
			}
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

				segCues, err := s.placeSegmentCues(&plan, activePlan.Regions, in.SceneProtectedRegions, selector, dSeg.Index, textToRender, startMs, endMs)
				if err != nil {
					return nil, err
				}
				subtitleCues = append(subtitleCues, segCues...)
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
				segCues, err := s.placeSegmentCues(&plan, activePlan.Regions, in.SceneProtectedRegions, selector, seg.Index, textToRender, seg.StartMs, seg.EndMs)
				if err != nil {
					return nil, err
				}
				subtitleCues = append(subtitleCues, segCues...)
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
	// 5b. Freeze the source-text covers: a burned-in source caption is only replaced visually
	// when an opaque box hides it. Regions that collide with a protected obstacle are surfaced
	// as occlusion exceptions instead of being covered.
	covers, coverOcclusions := buildSubtitleCovers(activePlan, in)
	occlusions = append(occlusions, coverOcclusions...)
	// 5c. Seat every replacement on the cover it replaces: one opaque block carries the text.
	subtitleCues, covers = seatReplacementCuesOnCovers(subtitleCues, covers, activePlan, in.SceneProtectedRegions)

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
		covers,
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
		Covers:             covers,
		Occlusions:         occlusions,
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
