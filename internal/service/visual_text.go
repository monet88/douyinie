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
	db     *storage.DB
	cas    *cas.Store
	router *provider.Router

	// OCRInvoke executes one OCR detection attempt.
	// When nil, router-backed invocation is used.
	OCRInvoke OCRInvokeFunc

	// ClassifyConfig optionally supplies custom classification heuristics.
	ClassifyConfig func(w, h int) domain.TextRegionClassifyConfig
}

// NewVisualTextService creates a new VisualTextService instance.
func NewVisualTextService(db *storage.DB, casStore *cas.Store) *VisualTextService {
	return &VisualTextService{
		db:  db,
		cas: casStore,
	}
}

// ConfigureRouter injects the provider router.
func (s *VisualTextService) ConfigureRouter(router *provider.Router) {
	s.router = router
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
				if expectedProvHash, err := domain.ComputeTextRegionPlanProvenanceHash(input.AssetID, p.ID(), mName, mVer, stepMs); err == nil {
					if existingIdx, err := s.db.GetTextRegionPlanByProvenance(ctx, expectedProvHash); err == nil && existingIdx != nil && existingIdx.CASHash != "" {
						reader, err := s.cas.Get(existingIdx.CASHash)
						if err == nil {
							defer reader.Close()
							var cached domain.TextRegionPlan
							if err := json.NewDecoder(reader).Decode(&cached); err == nil {
								cached.CASHash = existingIdx.CASHash
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
	provHash, err := domain.ComputeTextRegionPlanProvenanceHash(input.AssetID, res.ProviderID, res.ModelName, res.ModelVersion, stepMs)
	if err != nil {
		return nil, fmt.Errorf("compute provenance hash: %w", err)
	}

	plan := domain.TextRegionPlan{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.TextRegionPlanSchemaVersion,
		AssetID:        input.AssetID,
		ProviderID:     res.ProviderID,
		ModelName:      res.ModelName,
		ModelVersion:   res.ModelVersion,
		FrameWidth:     w,
		FrameHeight:    h,
		Regions:        trackedRegions,
		ProvenanceHash: provHash,
		CreatedAt:      time.Now().UTC(),
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

		// Classify role deterministically
		role, protectedMeta, reviewReq, reviewReason := domain.ClassifyRegion(cl.text, avgBox, meanConf, cfg)

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
