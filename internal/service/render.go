package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/storage"
)

// RenderService coordinates RenderPlan freezing and NativeRenderBackend composition
// for PreviewRenderArtifact and FinalRenderArtifact.
//
// Invariants (CapCap-derived, locked by #16 §8, #18, #30):
//  1. NativeRenderBackend: FFmpeg/libass-class deterministic composition over frozen exact artifact refs.
//  2. RenderPlan freezes exact artifact IDs/versions (SourceAsset SHA, DubMix CAS, Subtitle plan/cues).
//  3. Preview uses proxies/lower encode quality but consumes the SAME accepted timeline,
//     subtitle layout/style plan, audio selection, and render-plan semantics as final.
//  4. No separate preview-only subtitle/layout engine.
//  5. PreviewRenderArtifact and FinalRenderArtifact are distinct immutables.
type RenderService struct {
	db             *storage.DB
	casStore       *cas.Store
	ffmpegPath     string
	fontFile       string
	customComposer func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error)
}

// NewRenderService creates a new RenderService instance.
func NewRenderService(db *storage.DB, casStore *cas.Store) *RenderService {
	return &RenderService{
		db:       db,
		casStore: casStore,
	}
}

// SetFFmpegPath configures a custom ffmpeg binary path.
func (s *RenderService) SetFFmpegPath(p string) {
	s.ffmpegPath = p
}

// SetFontFile configures the font file used for subtitle text rendering.
func (s *RenderService) SetFontFile(p string) {
	s.fontFile = p
}

// SetCustomComposer injects a custom composition backend seam for testing.
func (s *RenderService) SetCustomComposer(fn func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error)) {
	s.customComposer = fn
}

// RenderPlanInput specifies the parameters required to freeze a RenderPlan.
type RenderPlanInput struct {
	RunID                  string               `json:"run_id"`
	JobID                  string               `json:"job_id,omitempty"`
	AssetID                string               `json:"asset_id"`
	TargetLanguage         string               `json:"target_language"`
	DubMixCAS              string               `json:"dub_mix_cas,omitempty"`               // optional explicit CAS hash
	SubtitlePlanCAS        string               `json:"subtitle_plan_cas,omitempty"`         // optional explicit CAS hash of SubtitlePlanArtifact
	SubtitlePlanArtifactID string               `json:"subtitle_plan_artifact_id,omitempty"` // optional artifact ID
	SubtitleCues           []domain.SubtitleCue `json:"subtitle_cues,omitempty"`
	CoverBoxes             []domain.CoverBox    `json:"cover_boxes,omitempty"`
	OverlayCues            []domain.SubtitleCue `json:"overlay_cues,omitempty"`
}

// FreezeRenderPlan freezes exact artifact IDs/versions into an immutable RenderPlan.
// Validates that source asset, dub mix artifact, audio tracks, and subtitle layout/style plan exist and are valid.
func (s *RenderService) FreezeRenderPlan(ctx context.Context, in RenderPlanInput) (*domain.RenderPlan, error) {
	if strings.TrimSpace(in.AssetID) == "" {
		return nil, fmt.Errorf("%w: asset_id is required", domain.ErrRenderPlanInvalid)
	}
	if strings.TrimSpace(in.RunID) == "" {
		return nil, fmt.Errorf("%w: run_id is required", domain.ErrRenderPlanInvalid)
	}
	if !domain.IsValidTargetLanguage(in.TargetLanguage) {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidTargetLanguage, in.TargetLanguage)
	}

	// 1. Resolve SourceAsset
	asset, err := s.db.GetSourceAsset(ctx, in.AssetID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, domain.ErrAssetNotFound) {
			return nil, fmt.Errorf("%w: source asset %s", domain.ErrRenderSourceNotFound, in.AssetID)
		}
		return nil, fmt.Errorf("lookup source asset: %w", err)
	}
	if !s.casStore.Exists(asset.SHA256) {
		return nil, fmt.Errorf("%w: source video file missing in CAS (%s)", domain.ErrRenderSourceNotFound, asset.SHA256)
	}

	// 2. Resolve PreflightReport for timeline
	preflight, err := s.db.GetPreflightReport(ctx, in.AssetID)
	if err != nil {
		return nil, fmt.Errorf("%w: preflight report missing for asset %s", domain.ErrRenderSourceNotFound, in.AssetID)
	}
	timeline := domain.RenderTimeline{
		DurationMs: preflight.DurationMs,
		Width:      preflight.Width,
		Height:     preflight.Height,
		FrameRate:  preflight.FrameRate,
	}
	if timeline.DurationMs <= 0 {
		timeline.DurationMs = int64(preflight.DurationSec * 1000)
	}

	// 3. Resolve DubMixArtifact
	var dubMix domain.DubMixArtifact
	var dubMixCAS string
	if in.DubMixCAS != "" {
		dubMixCAS = in.DubMixCAS
		reader, err := s.casStore.Get(dubMixCAS)
		if err != nil {
			return nil, fmt.Errorf("%w: load dub mix from CAS (%s): %v", domain.ErrRenderSourceNotFound, dubMixCAS, err)
		}
		defer reader.Close()
		if err := json.NewDecoder(reader).Decode(&dubMix); err != nil {
			return nil, fmt.Errorf("decode dub mix from CAS: %w", err)
		}
	} else {
		idx, err := s.db.GetDubMixArtifactIndex(ctx, in.AssetID, in.TargetLanguage)
		if err != nil {
			return nil, fmt.Errorf("%w: dub mix artifact missing for asset %s (%s)", domain.ErrRenderSourceNotFound, in.AssetID, in.TargetLanguage)
		}
		dubMixCAS = idx.CASHash
		reader, err := s.casStore.Get(dubMixCAS)
		if err != nil {
			return nil, fmt.Errorf("%w: load dub mix from CAS (%s): %v", domain.ErrRenderSourceNotFound, dubMixCAS, err)
		}
		defer reader.Close()
		if err := json.NewDecoder(reader).Decode(&dubMix); err != nil {
			return nil, fmt.Errorf("decode dub mix from CAS: %w", err)
		}
	}

	// Invariant: Refused dub mix must not enter rendering
	if dubMix.OverallStatus != "PASS" {
		return nil, fmt.Errorf("%w: status is %s (reason: %s)", domain.ErrDubMixNotRenderable, dubMix.OverallStatus, dubMix.RefusalReason)
	}
	if !s.casStore.Exists(dubMix.AudioCASHash) {
		return nil, fmt.Errorf("%w: dub mix audio file missing in CAS (%s)", domain.ErrRenderSourceNotFound, dubMix.AudioCASHash)
	}

	// 4. Resolve or Build SubtitlePlanArtifact
	var subPlanRef domain.SubtitlePlanRef
	var cues []domain.SubtitleCue

	if in.SubtitlePlanCAS != "" {
		r, err := s.casStore.Get(in.SubtitlePlanCAS)
		if err != nil {
			return nil, fmt.Errorf("%w: load subtitle plan artifact from CAS (%s): %v", domain.ErrSubtitlePlanNotFound, in.SubtitlePlanCAS, err)
		}
		defer r.Close()
		var subArt domain.SubtitlePlanArtifact
		if err := json.NewDecoder(r).Decode(&subArt); err != nil {
			return nil, fmt.Errorf("decode subtitle plan artifact: %w", err)
		}
		cues = subArt.Cues
		subPlanRef = domain.SubtitlePlanRef{
			ArtifactID:     subArt.ID,
			SchemaVersion:  subArt.SchemaVersion,
			CASHash:        in.SubtitlePlanCAS,
			ProvenanceHash: subArt.ProvenanceHash,
			Format:         subArt.Format,
			CueCount:       len(subArt.Cues),
			CueSpecHash:    domain.ComputeCueSpecHash(subArt.Cues),
		}
	} else if len(in.SubtitleCues) > 0 {
		// Validate provided subtitle cues
		for i, cue := range in.SubtitleCues {
			if cue.StartMs < 0 || cue.EndMs <= cue.StartMs {
				return nil, fmt.Errorf("%w: cue %d invalid time bounds [%d, %d]", domain.ErrRenderPlanInvalid, i, cue.StartMs, cue.EndMs)
			}
		}
		cues = in.SubtitleCues
		cueHash := domain.ComputeCueSpecHash(cues)
		subProv, err := domain.ComputeSubtitlePlanProvenanceHash(in.AssetID, in.TargetLanguage, cues, "compact_fit_cues")
		if err != nil {
			return nil, fmt.Errorf("compute subtitle plan provenance: %w", err)
		}
		assContent := media.GenerateASSContent(timeline, cues, s.fontFile)
		subArt := domain.SubtitlePlanArtifact{
			ID:             subProv,
			SchemaVersion:  domain.SubtitlePlanSchemaVersion,
			AssetID:        in.AssetID,
			TargetLanguage: in.TargetLanguage,
			Format:         "compact_fit_cues",
			Cues:           cues,
			ASSContent:     assContent,
			ProvenanceHash: subProv,
		}
		artBytes, err := json.Marshal(subArt)
		if err != nil {
			return nil, fmt.Errorf("marshal subtitle plan artifact: %w", err)
		}
		subObj, err := s.casStore.Put(bytes.NewReader(artBytes))
		if err != nil {
			return nil, fmt.Errorf("store subtitle plan artifact in CAS: %w", err)
		}
		subArt.CASHash = subObj.SHA256

		subPlanRef = domain.SubtitlePlanRef{
			ArtifactID:     subArt.ID,
			SchemaVersion:  subArt.SchemaVersion,
			CASHash:        subArt.CASHash,
			ProvenanceHash: subArt.ProvenanceHash,
			Format:         subArt.Format,
			CueCount:       len(subArt.Cues),
			CueSpecHash:    cueHash,
		}
	} else {
		// Auto-resolve persisted LocalizedSubtitleTrack if available
		subTrackIdx, err := s.db.GetLocalizedSubtitleTrackIndex(ctx, in.AssetID, in.TargetLanguage)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("query localized subtitle track index: %w", err)
		}
		if subTrackIdx != nil {
			if subTrackIdx.CASHash == "" {
				return nil, fmt.Errorf("localized subtitle track index has empty CAS hash")
			}
			r, err := s.casStore.Get(subTrackIdx.CASHash)
			if err != nil {
				return nil, fmt.Errorf("load localized subtitle track from CAS (%s): %w", subTrackIdx.CASHash, err)
			}
			defer r.Close()
			var subTrack domain.LocalizedSubtitleTrack
			if err := json.NewDecoder(r).Decode(&subTrack); err != nil {
				return nil, fmt.Errorf("decode localized subtitle track (%s): %w", subTrackIdx.CASHash, err)
			}
			cues = subTrack.Cues
			cueHash := domain.ComputeCueSpecHash(cues)
			subProv, err := domain.ComputeSubtitlePlanProvenanceHash(in.AssetID, in.TargetLanguage, cues, "compact_fit_cues")
			if err != nil {
				return nil, fmt.Errorf("compute subtitle plan provenance: %w", err)
			}
			assContent := media.GenerateASSContent(timeline, cues, s.fontFile)
			subArt := domain.SubtitlePlanArtifact{
				ID:             subProv,
				SchemaVersion:  domain.SubtitlePlanSchemaVersion,
				AssetID:        in.AssetID,
				TargetLanguage: in.TargetLanguage,
				Format:         "compact_fit_cues",
				Cues:           cues,
				ASSContent:     assContent,
				ProvenanceHash: subProv,
			}
			artBytes, err := json.Marshal(subArt)
			if err != nil {
				return nil, fmt.Errorf("marshal subtitle plan artifact: %w", err)
			}
			subObj, err := s.casStore.Put(bytes.NewReader(artBytes))
			if err != nil {
				return nil, fmt.Errorf("store subtitle plan artifact in CAS: %w", err)
			}
			subArt.CASHash = subObj.SHA256
			subPlanRef = domain.SubtitlePlanRef{
				ArtifactID:     subArt.ID,
				SchemaVersion:  subArt.SchemaVersion,
				CASHash:        subArt.CASHash,
				ProvenanceHash: subArt.ProvenanceHash,
				Format:         subArt.Format,
				CueCount:       len(subArt.Cues),
				CueSpecHash:    cueHash,
			}
		} else {
			// Truly absent optional captions -> clean empty subtitle plan
			cues = []domain.SubtitleCue{}
			cueHash := domain.ComputeCueSpecHash(cues)
			subProv, err := domain.ComputeSubtitlePlanProvenanceHash(in.AssetID, in.TargetLanguage, cues, "compact_fit_cues")
			if err != nil {
				return nil, fmt.Errorf("compute subtitle plan provenance: %w", err)
			}
			assContent := media.GenerateASSContent(timeline, cues, s.fontFile)
			subArt := domain.SubtitlePlanArtifact{
				ID:             subProv,
				SchemaVersion:  domain.SubtitlePlanSchemaVersion,
				AssetID:        in.AssetID,
				TargetLanguage: in.TargetLanguage,
				Format:         "compact_fit_cues",
				Cues:           cues,
				ASSContent:     assContent,
				ProvenanceHash: subProv,
			}
			artBytes, err := json.Marshal(subArt)
			if err != nil {
				return nil, fmt.Errorf("marshal subtitle plan artifact: %w", err)
			}
			subObj, err := s.casStore.Put(bytes.NewReader(artBytes))
			if err != nil {
				return nil, fmt.Errorf("store subtitle plan artifact in CAS: %w", err)
			}
			subArt.CASHash = subObj.SHA256

			subPlanRef = domain.SubtitlePlanRef{
				ArtifactID:     subArt.ID,
				SchemaVersion:  subArt.SchemaVersion,
				CASHash:        subArt.CASHash,
				ProvenanceHash: subArt.ProvenanceHash,
				Format:         subArt.Format,
				CueCount:       0,
				CueSpecHash:    cueHash,
			}
		}
	}

	// 5. Resolve frozen source-text covers from the run's localized visual track.
	// Covers are what hides a burned-in source caption; without them the source text and its
	// replacement are both visible. Resolution is run-scoped: an earlier run's track for the same
	// asset must never leak a cover into this plan (cross-run bleed).
	covers := in.CoverBoxes
	overlayCues := in.OverlayCues
	if covers == nil && overlayCues == nil {
		covers, overlayCues = s.resolveVisualTrackLayers(ctx, in.RunID, in.AssetID, in.TargetLanguage)
	}
	for i, cue := range overlayCues {
		if cue.StartMs < 0 || cue.EndMs <= cue.StartMs {
			return nil, fmt.Errorf("%w: overlay cue %d invalid time bounds [%d, %d]", domain.ErrRenderPlanInvalid, i, cue.StartMs, cue.EndMs)
		}
	}

	fitted := make([]domain.CoverBox, 0, len(covers))
	for _, c := range covers {
		clamped, ok := domain.ClampCoverBoxToFrame(c, timeline.Width, timeline.Height)
		if !ok {
			continue
		}
		if err := domain.ValidateCoverBox(clamped, timeline.Width, timeline.Height); err != nil {
			return nil, err
		}
		fitted = append(fitted, clamped)
	}
	if len(fitted) == 0 {
		fitted = nil
	}
	covers = fitted

	// 6. Compute Provenance & Build RenderPlan
	provHash, err := domain.ComputeRenderPlanProvenanceHash(
		in.AssetID,
		in.TargetLanguage,
		asset.SHA256,
		dubMixCAS,
		dubMix.AudioCASHash,
		timeline,
		subPlanRef,
		covers,
		overlayCues,
	)
	if err != nil {
		return nil, fmt.Errorf("compute render plan provenance: %w", err)
	}

	// Check if already frozen
	if existingIdx, err := s.db.GetRenderPlanByProvenance(ctx, provHash); err == nil && existingIdx != nil {
		r, err := s.casStore.Get(existingIdx.CASHash)
		if err == nil {
			defer r.Close()
			var existingPlan domain.RenderPlan
			if err := json.NewDecoder(r).Decode(&existingPlan); err == nil {
				existingPlan.CASHash = existingIdx.CASHash
				existingPlan.ProvenanceHash = existingIdx.ProvenanceHash
				return &existingPlan, nil
			}
		}
	}

	plan := domain.RenderPlan{
		ID:                uuid.NewString(),
		SchemaVersion:     domain.RenderPlanSchemaVersion,
		AssetID:           in.AssetID,
		RunID:             in.RunID,
		JobID:             in.JobID,
		TargetLanguage:    in.TargetLanguage,
		SourceAssetSHA256: asset.SHA256,
		DubMixCASHash:     dubMixCAS,
		AudioCASHash:      dubMix.AudioCASHash,
		Timeline:          timeline,
		SubtitlePlan:      subPlanRef,
		SubtitleCues:      cues,
		CoverBoxes:        covers,
		OverlayCues:       overlayCues,
		ProvenanceHash:    provHash,
		CreatedAt:         time.Now().UTC(),
	}

	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("marshal render plan: %w", err)
	}
	planObj, err := s.casStore.Put(bytes.NewReader(planBytes))
	if err != nil {
		return nil, fmt.Errorf("store render plan in CAS: %w", err)
	}
	plan.CASHash = planObj.SHA256

	err = s.db.SaveRenderPlanIndex(ctx, storage.RenderPlanIndex{
		ID:             plan.ID,
		AssetID:        plan.AssetID,
		RunID:          plan.RunID,
		JobID:          plan.JobID,
		TargetLanguage: plan.TargetLanguage,
		CASHash:        plan.CASHash,
		ProvenanceHash: plan.ProvenanceHash,
		CreatedAt:      plan.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save render plan index: %w", err)
	}

	return &plan, nil
}

// resolveVisualTrackLayers loads the run's localized visual track and returns the in-place
// layers it froze: the covers that hide replaced source text, and the overlay cues that draw the
// localized text on top of them.
//
// A missing track is not a render blocker: the plan then renders exactly as it did before these
// layers existed (source text left in place) rather than failing a run over a visual-layer
// absence. A track that exists but cannot be decoded is likewise skipped - these layers are a
// quality pass, not a correctness gate for the render contract.
func (s *RenderService) resolveVisualTrackLayers(ctx context.Context, runID, assetID, targetLang string) ([]domain.CoverBox, []domain.SubtitleCue) {
	idx, err := s.db.GetLocalizedVisualTrackIndexByRun(ctx, runID)
	if err != nil || idx == nil {
		return nil, nil
	}
	if idx.AssetID != assetID || !strings.EqualFold(idx.TargetLanguage, targetLang) {
		// A run's visual track for a different asset/language is not this plan's evidence.
		return nil, nil
	}
	r, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		return nil, nil
	}
	defer r.Close()
	var track domain.LocalizedVisualTrack
	if err := json.NewDecoder(r).Decode(&track); err != nil {
		return nil, nil
	}

	covers := append([]domain.CoverBox(nil), track.Covers...)
	var overlayCues []domain.SubtitleCue
	for i, ov := range track.Overlays {
		text := strings.TrimSpace(ov.LocalizedText)
		if text == "" || ov.EndMs <= ov.StartMs {
			continue
		}
		// An in-place overlay only replaces source text when a cover hides it first; the
		// inpainting fallback is not implemented by the composer, so it is not composited here.
		if !ov.IsCoverDefault || ov.Inpainting {
			continue
		}
		if ov.Box.Width <= 0 || ov.Box.Height <= 0 {
			continue
		}
		// A box anchored at the frame origin cannot be positioned deterministically: the ASS
		// renderer only emits an explicit position for a non-zero anchor.
		if ov.Box.X <= 0 && ov.Box.Y <= 0 {
			continue
		}
		covers = append(covers, domain.CoverBox{
			RegionID: ov.RegionID,
			Role:     string(ov.Role),
			X:        ov.Box.X,
			Y:        ov.Box.Y,
			Width:    ov.Box.Width,
			Height:   ov.Box.Height,
			StartMs:  ov.StartMs,
			EndMs:    ov.EndMs,
			Color:    "#000000",
			Opacity:  1.0,
		})
		overlayCues = append(overlayCues, domain.SubtitleCue{
			ID:         fmt.Sprintf("overlay-%d", i),
			StartMs:    ov.StartMs,
			EndMs:      ov.EndMs,
			Text:       text,
			X:          ov.Box.X,
			Y:          ov.Box.Y,
			Width:      ov.Box.Width,
			Height:     ov.Box.Height,
			FontSizePx: ov.FontSizePx,
			PaddingX:   ov.PaddingX,
			PaddingY:   ov.PaddingY,
			BoxColor:   ov.BoxColor,
			FontColor:  ov.FontColor,
		})
	}
	return covers, overlayCues
}

// RenderExecutionInput defines input parameters for preview or final video render.
type RenderExecutionInput struct {
	RunID          string `json:"run_id"`
	JobID          string `json:"job_id,omitempty"`
	AssetID        string `json:"asset_id"`
	TargetLanguage string `json:"target_language"`
	PlanProvenance string `json:"plan_provenance,omitempty"` // optional explicit plan provenance
	PlanCAS        string `json:"plan_cas,omitempty"`        // optional explicit plan CAS
	FontFile       string `json:"font_file,omitempty"`       // optional font file override
}

// RenderPreview executes a preview render pass consuming the frozen RenderPlan.
// Uses proxy/lower quality encode settings while preserving identical timeline, subtitle layout, and audio semantics.
func (s *RenderService) RenderPreview(ctx context.Context, in RenderExecutionInput) (*domain.PreviewRenderArtifact, error) {
	plan, err := s.resolvePlan(ctx, in)
	if err != nil {
		return nil, err
	}

	profile := domain.DefaultPreviewEncodeProfile()
	artifactProv, err := domain.ComputeRenderArtifactProvenanceHash(plan.ProvenanceHash, domain.RenderKindPreview, profile)
	if err != nil {
		return nil, fmt.Errorf("compute preview artifact provenance: %w", err)
	}

	// Idempotent cache check
	if existingIdx, err := s.db.GetRenderArtifactByProvenance(ctx, artifactProv); err == nil && existingIdx != nil {
		r, err := s.casStore.Get(existingIdx.CASHash)
		if err == nil {
			defer r.Close()
			var existing domain.PreviewRenderArtifact
			if err := json.NewDecoder(r).Decode(&existing); err == nil {
				existing.CASHash = existingIdx.CASHash
				existing.ProvenanceHash = existingIdx.ProvenanceHash
				return &existing, nil
			}
		}
	}

	compRes, err := s.composeVideo(ctx, plan, profile, in.FontFile)
	if err != nil {
		return nil, err
	}

	defer os.Remove(compRes.OutputPath)
	outObj, err := s.casStore.PutFile(compRes.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("store preview output in CAS: %w", err)
	}

	consumed := domain.ConsumedPlanRef{
		PlanProvenanceHash: plan.ProvenanceHash,
		PlanCASHash:        plan.CASHash,
		SourceAssetSHA256:  plan.SourceAssetSHA256,
		DubMixCASHash:      plan.DubMixCASHash,
		AudioCASHash:       plan.AudioCASHash,
		DurationMs:         plan.Timeline.DurationMs,
		Width:              plan.Timeline.Width,
		Height:             plan.Timeline.Height,
		FrameRate:          plan.Timeline.FrameRate,
		SubtitlePlan:       plan.SubtitlePlan,
		CueSpecHash:        plan.SubtitlePlan.CueSpecHash,
		CueCount:           plan.SubtitlePlan.CueCount,
	}

	artifact := domain.PreviewRenderArtifact{
		ID:               uuid.NewString(),
		SchemaVersion:    domain.PreviewRenderSchemaVersion,
		AssetID:          plan.AssetID,
		RunID:            in.RunID,
		JobID:            in.JobID,
		TargetLanguage:   plan.TargetLanguage,
		ConsumedPlan:     consumed,
		EncodeProfile:    profile,
		OutputCASHash:    outObj.SHA256,
		OutputCASPath:    outObj.Path,
		OutputByteSize:   compRes.ByteSize,
		OutputDurationMs: compRes.DurationMs,
		Renderer:         compRes.Renderer,
		ProvenanceHash:   artifactProv,
		OverallStatus:    "PASS",
		CreatedAt:        time.Now().UTC(),
	}

	artBytes, err := json.Marshal(artifact)
	if err != nil {
		return nil, fmt.Errorf("marshal preview artifact: %w", err)
	}
	artObj, err := s.casStore.Put(bytes.NewReader(artBytes))
	if err != nil {
		return nil, fmt.Errorf("store preview artifact metadata in CAS: %w", err)
	}
	artifact.CASHash = artObj.SHA256

	err = s.db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
		ID:             artifact.ID,
		AssetID:        artifact.AssetID,
		RunID:          artifact.RunID,
		JobID:          artifact.JobID,
		TargetLanguage: artifact.TargetLanguage,
		Kind:           domain.RenderKindPreview,
		PlanProvenance: artifact.ConsumedPlan.PlanProvenanceHash,
		PlanCASHash:    artifact.ConsumedPlan.PlanCASHash,
		OutputCASHash:  artifact.OutputCASHash,
		CASHash:        artifact.CASHash,
		ProvenanceHash: artifact.ProvenanceHash,
		OverallStatus:  artifact.OverallStatus,
		CreatedAt:      artifact.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save preview artifact index: %w", err)
	}

	return &artifact, nil
}

// RenderFinal executes a final high-quality delivery render pass consuming the frozen RenderPlan.
func (s *RenderService) RenderFinal(ctx context.Context, in RenderExecutionInput) (*domain.FinalRenderArtifact, error) {
	plan, err := s.resolvePlan(ctx, in)
	if err != nil {
		return nil, err
	}

	profile := domain.DefaultFinalEncodeProfile()
	artifactProv, err := domain.ComputeRenderArtifactProvenanceHash(plan.ProvenanceHash, domain.RenderKindFinal, profile)
	if err != nil {
		return nil, fmt.Errorf("compute final artifact provenance: %w", err)
	}

	// Idempotent cache check
	if existingIdx, err := s.db.GetRenderArtifactByProvenance(ctx, artifactProv); err == nil && existingIdx != nil {
		r, err := s.casStore.Get(existingIdx.CASHash)
		if err == nil {
			defer r.Close()
			var existing domain.FinalRenderArtifact
			if err := json.NewDecoder(r).Decode(&existing); err == nil {
				existing.CASHash = existingIdx.CASHash
				existing.ProvenanceHash = existingIdx.ProvenanceHash
				return &existing, nil
			}
		}
	}

	compRes, err := s.composeVideo(ctx, plan, profile, in.FontFile)
	if err != nil {
		return nil, err
	}

	defer os.Remove(compRes.OutputPath)
	outObj, err := s.casStore.PutFile(compRes.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("store final output in CAS: %w", err)
	}

	consumed := domain.ConsumedPlanRef{
		PlanProvenanceHash: plan.ProvenanceHash,
		PlanCASHash:        plan.CASHash,
		SourceAssetSHA256:  plan.SourceAssetSHA256,
		DubMixCASHash:      plan.DubMixCASHash,
		AudioCASHash:       plan.AudioCASHash,
		DurationMs:         plan.Timeline.DurationMs,
		Width:              plan.Timeline.Width,
		Height:             plan.Timeline.Height,
		FrameRate:          plan.Timeline.FrameRate,
		SubtitlePlan:       plan.SubtitlePlan,
		CueSpecHash:        plan.SubtitlePlan.CueSpecHash,
		CueCount:           plan.SubtitlePlan.CueCount,
	}

	artifact := domain.FinalRenderArtifact{
		ID:               uuid.NewString(),
		SchemaVersion:    domain.FinalRenderSchemaVersion,
		AssetID:          plan.AssetID,
		RunID:            in.RunID,
		JobID:            in.JobID,
		TargetLanguage:   plan.TargetLanguage,
		ConsumedPlan:     consumed,
		EncodeProfile:    profile,
		OutputCASHash:    outObj.SHA256,
		OutputCASPath:    outObj.Path,
		OutputByteSize:   compRes.ByteSize,
		OutputDurationMs: compRes.DurationMs,
		Renderer:         compRes.Renderer,
		ProvenanceHash:   artifactProv,
		OverallStatus:    "PASS",
		CreatedAt:        time.Now().UTC(),
	}

	artBytes, err := json.Marshal(artifact)
	if err != nil {
		return nil, fmt.Errorf("marshal final artifact: %w", err)
	}
	artObj, err := s.casStore.Put(bytes.NewReader(artBytes))
	if err != nil {
		return nil, fmt.Errorf("store final artifact metadata in CAS: %w", err)
	}
	artifact.CASHash = artObj.SHA256

	err = s.db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
		ID:             artifact.ID,
		AssetID:        artifact.AssetID,
		RunID:          artifact.RunID,
		JobID:          artifact.JobID,
		TargetLanguage: artifact.TargetLanguage,
		Kind:           domain.RenderKindFinal,
		PlanProvenance: artifact.ConsumedPlan.PlanProvenanceHash,
		PlanCASHash:    artifact.ConsumedPlan.PlanCASHash,
		OutputCASHash:  artifact.OutputCASHash,
		CASHash:        artifact.CASHash,
		ProvenanceHash: artifact.ProvenanceHash,
		OverallStatus:  artifact.OverallStatus,
		CreatedAt:      artifact.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save final artifact index: %w", err)
	}

	return &artifact, nil
}

// resolvePlan loads the RenderPlan either from explicit CAS hash, plan provenance, or latest index.
func (s *RenderService) resolvePlan(ctx context.Context, in RenderExecutionInput) (*domain.RenderPlan, error) {
	if in.PlanCAS != "" {
		r, err := s.casStore.Get(in.PlanCAS)
		if err != nil {
			return nil, fmt.Errorf("%w: load plan from CAS (%s): %v", domain.ErrRenderPlanNotFound, in.PlanCAS, err)
		}
		defer r.Close()
		var plan domain.RenderPlan
		if err := json.NewDecoder(r).Decode(&plan); err != nil {
			return nil, fmt.Errorf("decode plan from CAS: %w", err)
		}
		plan.CASHash = in.PlanCAS
		return &plan, nil
	}

	if in.PlanProvenance != "" {
		idx, err := s.db.GetRenderPlanByProvenance(ctx, in.PlanProvenance)
		if err != nil {
			return nil, fmt.Errorf("%w: lookup plan by provenance: %v", domain.ErrRenderPlanNotFound, err)
		}
		r, err := s.casStore.Get(idx.CASHash)
		if err != nil {
			return nil, fmt.Errorf("%w: load plan by provenance (%s): %v", domain.ErrRenderPlanNotFound, idx.CASHash, err)
		}
		defer r.Close()
		var plan domain.RenderPlan
		if err := json.NewDecoder(r).Decode(&plan); err != nil {
			return nil, fmt.Errorf("decode plan from CAS: %w", err)
		}
		plan.CASHash = idx.CASHash
		plan.ProvenanceHash = idx.ProvenanceHash
		return &plan, nil
	}

	// Default: lookup latest render plan for asset + language
	idx, err := s.db.GetRenderPlanIndex(ctx, in.AssetID, in.TargetLanguage)
	if err != nil {
		return nil, fmt.Errorf("%w: latest render plan index missing for asset %s: %v", domain.ErrRenderPlanNotFound, in.AssetID, err)
	}
	r, err := s.casStore.Get(idx.CASHash)
	if err != nil {
		return nil, fmt.Errorf("%w: load latest plan from CAS (%s): %v", domain.ErrRenderPlanNotFound, idx.CASHash, err)
	}
	defer r.Close()
	var plan domain.RenderPlan
	if err := json.NewDecoder(r).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode plan from CAS: %w", err)
	}
	plan.CASHash = idx.CASHash
	plan.ProvenanceHash = idx.ProvenanceHash
	return &plan, nil
}

// composeVideo prepares inputs and invokes the NativeRenderBackend (FFmpeg/libass-class composition).
func (s *RenderService) composeVideo(
	ctx context.Context,
	plan *domain.RenderPlan,
	profile domain.EncodeProfile,
	fontOverride string,
) (*media.CompositionResult, error) {
	// 1. Resolve source video file path from CAS
	videoPath, err := s.casStore.ResolvePath(plan.SourceAssetSHA256)
	if err != nil {
		return nil, fmt.Errorf("%w: source video path: %v", domain.ErrRenderSourceNotFound, err)
	}

	// 2. Resolve audio file path from CAS
	audioPath, err := s.casStore.ResolvePath(plan.AudioCASHash)
	if err != nil {
		return nil, fmt.Errorf("%w: audio track path: %v", domain.ErrRenderSourceNotFound, err)
	}

	// 3. Resolve ASS content / cues from SubtitlePlanArtifact
	assContent := ""
	cues := plan.SubtitleCues
	if plan.SubtitlePlan.CASHash != "" {
		r, err := s.casStore.Get(plan.SubtitlePlan.CASHash)
		if err != nil {
			return nil, fmt.Errorf("%w: load subtitle plan artifact from CAS (%s): %v", domain.ErrSubtitlePlanNotFound, plan.SubtitlePlan.CASHash, err)
		}
		defer r.Close()

		var subArt domain.SubtitlePlanArtifact
		if err := json.NewDecoder(r).Decode(&subArt); err != nil {
			return nil, fmt.Errorf("%w: decode subtitle plan artifact (%s): %v", domain.ErrRenderPlanInvalid, plan.SubtitlePlan.CASHash, err)
		}

		// Strict validation of SubtitlePlanArtifact identity & schema
		if subArt.SchemaVersion != domain.SubtitlePlanSchemaVersion {
			return nil, fmt.Errorf("%w: subtitle plan schema version mismatch (got %d, expected %d)", domain.ErrRenderPlanInvalid, subArt.SchemaVersion, domain.SubtitlePlanSchemaVersion)
		}
		if plan.SubtitlePlan.ArtifactID != "" && subArt.ID != "" && subArt.ID != plan.SubtitlePlan.ArtifactID {
			return nil, fmt.Errorf("%w: subtitle plan artifact ID mismatch (got %s, expected %s)", domain.ErrRenderPlanInvalid, subArt.ID, plan.SubtitlePlan.ArtifactID)
		}
		if len(subArt.Cues) != plan.SubtitlePlan.CueCount {
			return nil, fmt.Errorf("%w: subtitle plan cue count mismatch (got %d in artifact, expected %d in ref)", domain.ErrRenderPlanInvalid, len(subArt.Cues), plan.SubtitlePlan.CueCount)
		}
		if plan.SubtitlePlan.CueSpecHash != "" {
			computedCueHash := domain.ComputeCueSpecHash(subArt.Cues)
			if computedCueHash != plan.SubtitlePlan.CueSpecHash {
				return nil, fmt.Errorf("%w: subtitle plan cue spec hash mismatch (got %s, expected %s)", domain.ErrRenderPlanInvalid, computedCueHash, plan.SubtitlePlan.CueSpecHash)
			}
		}

		cues = subArt.Cues
		assContent = subArt.ASSContent
		if assContent == "" && len(cues) > 0 {
			assContent = media.GenerateASSContent(plan.Timeline, cues, fontOverride)
		}
	} else if len(cues) > 0 {
		assContent = media.GenerateASSContent(plan.Timeline, cues, fontOverride)
	}

	// In-place overlay cues (localized semantic/UI text) are burned with the speech subtitles:
	// both are ASS events over the same video, and the covers freeze composited below already
	// hide the source text each overlay replaces.
	if len(plan.OverlayCues) > 0 {
		cues = append(cues, plan.OverlayCues...)
		assContent = media.GenerateASSContent(plan.Timeline, cues, fontOverride)
	}

	// 4. Create temporary output MP4 file path
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("douyinie_render_%s_*.mp4", profile.Kind))
	if err != nil {
		return nil, fmt.Errorf("create render temp file: %w", err)
	}
	_ = tmpFile.Close()
	_ = os.Remove(tmpFile.Name()) // Let ffmpeg create it

	fontFile := s.fontFile
	if fontOverride != "" {
		fontFile = fontOverride
	}

	req := media.CompositionRequest{
		FFmpegPath:  s.ffmpegPath,
		SourceVideo: videoPath,
		AudioTrack:  audioPath,
		Timeline:    plan.Timeline,
		Cues:        cues,
		Covers:      plan.CoverBoxes,
		ASSContent:  assContent,
		FontFile:    fontFile,
		Profile:     profile,
		OutputPath:  tmpFile.Name(),
	}

	if s.customComposer != nil {
		return s.customComposer(ctx, req)
	}

	res, err := media.ComposeNativeVideo(ctx, req)
	if err != nil {
		_ = os.Remove(tmpFile.Name())
		if errors.Is(err, media.ErrFFmpegNotFound) || errors.Is(err, media.ErrLibassUnavailable) {
			return nil, fmt.Errorf("%w: %v", domain.ErrRenderBackendUnavailable, err)
		}
		return nil, fmt.Errorf("native composition failed: %w", err)
	}

	return res, nil
}
