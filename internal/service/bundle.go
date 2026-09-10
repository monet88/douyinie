package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/storage"
)

// ImportOptions defines behavior when importing a job bundle.
type ImportOptions struct {
	Overwrite bool `json:"overwrite"`
}

// BundleService handles self-contained job bundle export and import with hash verification,
// license obligation tracking, and secret/path scrubbing.
type BundleService struct {
	db         *storage.DB
	cas        *cas.Store
	licenseSvc *governance.LicenseService
}

// NewBundleService creates a new BundleService.
func NewBundleService(db *storage.DB, casStore *cas.Store, licenseSvc *governance.LicenseService) *BundleService {
	return &BundleService{
		db:         db,
		cas:        casStore,
		licenseSvc: licenseSvc,
	}
}

// ExportBundle creates a self-contained, secret-scrubbed, hash-verified zip bundle of a job.
func (s *BundleService) ExportBundle(ctx context.Context, jobID string, w io.Writer) (*domain.JobBundleManifest, error) {
	if s.db == nil || s.cas == nil {
		return nil, errors.New("database and CAS store are required for export")
	}

	// 1. Retrieve job
	job, err := s.db.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}

	// 2. Retrieve source asset
	asset, err := s.db.GetSourceAsset(ctx, job.SourceAssetID)
	if err != nil {
		return nil, fmt.Errorf("lookup source asset: %w", err)
	}

	// 3. Retrieve rights attestation (if present)
	var attestation *domain.RightsAttestation
	if asset.RightsAttestationID != "" {
		ra, err := s.db.GetRightsAttestation(ctx, asset.RightsAttestationID)
		if err == nil {
			attestation = ra
		} else if !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("lookup rights attestation: %w", err)
		}
	}

	// 4. Retrieve preflight report (if present)
	var preflight *domain.PreflightReport
	pr, err := s.db.GetPreflightReport(ctx, asset.ID)
	if err == nil {
		preflight = pr
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("lookup preflight report: %w", err)
	}

	// 5. Retrieve audio role plan (if present)
	var audioRolePlan *domain.AudioRolePlan
	arp, err := s.db.GetAudioRolePlan(ctx, asset.ID)
	if err == nil {
		audioRolePlan = arp
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("lookup audio role plan: %w", err)
	}

	// 6. Retrieve source-derived artifacts: audio stems and text region plans
	var audioStems *domain.AudioStemArtifacts
	if stemsIdx, err := s.db.GetAudioStemsArtifactIndex(ctx, asset.ID); err == nil && stemsIdx != nil {
		if stemsObj, err := s.loadJSONFromCAS(ctx, stemsIdx.CASHash); err == nil {
			var loaded domain.AudioStemArtifacts
			if json.Unmarshal(stemsObj, &loaded) == nil {
				audioStems = &loaded
			}
		}
	}

	var textRegionPlans []domain.TextRegionPlan
	if trpIdx, err := s.db.GetTextRegionPlanIndex(ctx, asset.ID); err == nil && trpIdx != nil {
		if trpObj, err := s.loadJSONFromCAS(ctx, trpIdx.CASHash); err == nil {
			var loaded domain.TextRegionPlan
			if json.Unmarshal(trpObj, &loaded) == nil {
				textRegionPlans = append(textRegionPlans, loaded)
			}
		}
	}

	// 7. Retrieve runs for this job
	runs, err := s.db.ListRunsByJobID(ctx, job.ID)
	if err != nil {
		return nil, fmt.Errorf("list runs for job: %w", err)
	}

	var bundleRuns []domain.JobBundleRunData
	for _, run := range runs {
		runData := domain.JobBundleRunData{
			Run: run,
		}

		// Stage executions
		if se, err := s.db.ListStageExecutions(ctx, run.ID); err == nil {
			runData.StageExecutions = se
		}

		// Selection decisions
		if dec, err := s.db.ListSelectionDecisions(ctx, run.ID, ""); err == nil {
			runData.SelectionDecisions = dec
		}

		// Provider attempts
		if att, err := s.db.ListProviderAttempts(ctx, run.ID, ""); err == nil {
			runData.ProviderAttempts = att
		}

		// Review overrides
		if ro, err := s.db.GetReviewOverridesByRun(ctx, run.ID); err == nil {
			runData.ReviewOverrides = ro
		}

		// Quality results
		if qr, err := s.db.GetQualityResultsByRun(ctx, run.ID); err == nil {
			runData.QualityResults = qr
		}

		// Transcript artifact
		if idx, err := s.db.GetTranscriptArtifactIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var ta domain.TranscriptArtifact
				if json.Unmarshal(blob, &ta) == nil {
					runData.TranscriptArtifact = &ta
				}
			}
		}

		// Translation variant
		if idx, err := s.db.GetTranslationVariantIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var tv domain.TranslationVariant
				if json.Unmarshal(blob, &tv) == nil {
					runData.TranslationVariant = &tv
				}
			}
		}

		// Dub script variant
		if idx, err := s.db.GetDubScriptVariantIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var dsv domain.DubScriptVariant
				if json.Unmarshal(blob, &dsv) == nil {
					runData.DubScriptVariant = &dsv
				}
			}
		}

		// Voice assignment
		if idx, err := s.db.GetVoiceAssignmentIndexByRunID(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var va domain.VoiceAssignment
				if json.Unmarshal(blob, &va) == nil {
					runData.VoiceAssignment = &va
				}
			}
		}

		// Dub segments variant
		if idx, err := s.db.GetDubSegmentsVariantIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var dsv domain.DubSegmentsVariant
				if json.Unmarshal(blob, &dsv) == nil {
					runData.DubSegmentsVariant = &dsv
				}
			}
		}

		// Dub mix artifact
		if idx, err := s.db.GetDubMixArtifactIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var dma domain.DubMixArtifact
				if json.Unmarshal(blob, &dma) == nil {
					runData.DubMixArtifact = &dma
				}
			}
		}

		// Localized subtitle track
		if idx, err := s.db.GetLocalizedSubtitleTrackIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var lst domain.LocalizedSubtitleTrack
				if json.Unmarshal(blob, &lst) == nil {
					runData.LocalizedSubtitleTrack = &lst
				}
			}
		}

		// Localized visual track
		if idx, err := s.db.GetLocalizedVisualTrackIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var lvt domain.LocalizedVisualTrack
				if json.Unmarshal(blob, &lvt) == nil {
					runData.LocalizedVisualTrack = &lvt
				}
			}
		}

		// Render plan
		if idx, err := s.db.GetRenderPlanIndexByRun(ctx, run.ID); err == nil && idx != nil {
			if blob, err := s.loadJSONFromCAS(ctx, idx.CASHash); err == nil {
				var rp domain.RenderPlan
				if json.Unmarshal(blob, &rp) == nil {
					runData.RenderPlan = &rp
				}
			}
		}

		// Render artifacts (preview and final)
		if indices, err := s.db.GetRenderArtifactIndicesByRun(ctx, run.ID); err == nil && len(indices) > 0 {
			rad := &domain.RenderArtifactIndexData{}
			for _, raIdx := range indices {
				if raIdx.Kind == "preview" {
					if blob, err := s.loadJSONFromCAS(ctx, raIdx.CASHash); err == nil {
						var pra domain.PreviewRenderArtifact
						if json.Unmarshal(blob, &pra) == nil {
							rad.Preview = &pra
						}
					}
				} else if raIdx.Kind == "final" {
					if blob, err := s.loadJSONFromCAS(ctx, raIdx.CASHash); err == nil {
						var fra domain.FinalRenderArtifact
						if json.Unmarshal(blob, &fra) == nil {
							rad.Final = &fra
						}
					}
				}
			}
			if rad.Preview != nil || rad.Final != nil {
				runData.RenderArtifact = rad
			}
		}

		bundleRuns = append(bundleRuns, runData)
	}

	// 8. License Manifests (carry 4 obligation layers: CODE/MODEL/DATA_LICENSE + SERVICE_TERMS)
	var licenseEntries []domain.LicenseManifestEntry
	if lmList, err := s.db.ListLicenseManifests(ctx, ""); err == nil {
		licenseEntries = lmList
	}
	// Validate each license entry has all 4 obligation layers
	for _, entry := range licenseEntries {
		if strings.TrimSpace(entry.CodeLicense) == "" ||
			strings.TrimSpace(entry.ModelLicense) == "" ||
			strings.TrimSpace(entry.DataLicense) == "" ||
			strings.TrimSpace(entry.ServiceTerms) == "" {
			return nil, domain.ErrJobBundleLicenseIncomplete
		}
	}

	// 9. Assemble manifest before scrubbing
	manifest := &domain.JobBundleManifest{
		BundleVersion:     1,
		ExportedAt:        time.Now().UTC(),
		Job:               *job,
		SourceAsset:       *asset,
		RightsAttestation: attestation,
		PreflightReport:   preflight,
		AudioRolePlan:     audioRolePlan,
		AudioStems:        audioStems,
		TextRegionPlans:   textRegionPlans,
		Runs:              bundleRuns,
		LicenseManifests:  licenseEntries,
	}

	// 10. Scrub manifest in-place (removes secrets and machine-local absolute paths)
	ScrubManifest(manifest)

	// Validate scrubbed manifest
	if err := ValidateBundleManifest(manifest); err != nil {
		return nil, fmt.Errorf("scrubbed manifest failed validation: %w", err)
	}

	// 11. Traverse and discover all reachable CAS artifacts (fail-closed if any missing)
	reachableHashes, err := s.collectReachableCASHashes(manifest)
	if err != nil {
		return nil, fmt.Errorf("traverse reachable artifacts: %w", err)
	}

	// 12. Write Zip bundle
	zw := zip.NewWriter(w)

	var bundleArtifacts []domain.JobBundleArtifact
	for hash := range reachableHashes {
		if !s.cas.Exists(hash) {
			return nil, fmt.Errorf("reachable CAS object %s is missing from store", hash)
		}

		rc, err := s.cas.Get(hash)
		if err != nil {
			return nil, fmt.Errorf("read CAS object %s: %w", hash, err)
		}

		bundlePath := "artifacts/" + hash
		entryWriter, err := zw.Create(bundlePath)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("create zip entry %s: %w", bundlePath, err)
		}

		written, err := io.Copy(entryWriter, rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("write zip entry %s: %w", bundlePath, err)
		}

		bundleArtifacts = append(bundleArtifacts, domain.JobBundleArtifact{
			SHA256:     hash,
			ByteSize:   written,
			BundlePath: bundlePath,
		})
	}

	manifest.Artifacts = bundleArtifacts

	// Serialize manifest.json into zip
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest.json: %w", err)
	}

	// Final scan on manifest bytes
	if err := ValidateManifestJSON(manifestBytes); err != nil {
		return nil, fmt.Errorf("manifest validation failed: %w", err)
	}

	manifestWriter, err := zw.Create("manifest.json")
	if err != nil {
		return nil, fmt.Errorf("create manifest.json in zip: %w", err)
	}
	if _, err := manifestWriter.Write(manifestBytes); err != nil {
		return nil, fmt.Errorf("write manifest.json to zip: %w", err)
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip bundle: %w", err)
	}

	return manifest, nil
}

// ImportBundle imports and verifies a self-contained job bundle from a random-access reader.
func (s *BundleService) ImportBundle(ctx context.Context, r io.ReaderAt, size int64, opts ImportOptions) (*domain.JobBundleManifest, error) {
	if s.db == nil || s.cas == nil {
		return nil, errors.New("database and CAS store are required for import")
	}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: open zip: %v", domain.ErrJobBundleInvalid, err)
	}

	// 1. Find and read manifest.json
	var manifestFile *zip.File
	zipFiles := make(map[string]*zip.File)
	for _, f := range zr.File {
		zipFiles[f.Name] = f
		if f.Name == "manifest.json" {
			manifestFile = f
		}
	}

	if manifestFile == nil {
		return nil, fmt.Errorf("%w: missing manifest.json in archive", domain.ErrJobBundleInvalid)
	}

	mrc, err := manifestFile.Open()
	if err != nil {
		return nil, fmt.Errorf("open manifest.json in archive: %w", err)
	}
	manifestBytes, err := io.ReadAll(mrc)
	mrc.Close()
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}

	// 2. Validate manifest (fail-closed on secrets, machine-local absolute paths, incomplete license layers)
	if err := ValidateManifestJSON(manifestBytes); err != nil {
		return nil, err
	}

	var manifest domain.JobBundleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("%w: unmarshal manifest.json: %v", domain.ErrJobBundleInvalid, err)
	}

	// Validate typed manifest
	if err := ValidateBundleManifest(&manifest); err != nil {
		return nil, err
	}

	// If overwrite is false, fail early if job already exists on target host
	if !opts.Overwrite {
		existingJob, err := s.db.GetJob(ctx, manifest.Job.ID)
		if err == nil && existingJob != nil {
			return nil, fmt.Errorf("%w: job %s", domain.ErrJobAlreadyExists, manifest.Job.ID)
		}
	}
	// 2.5. Verify all referenced CAS hashes in manifest are present in declared artifacts
	declaredHashes := make(map[string]bool)
	for _, art := range manifest.Artifacts {
		declaredHashes[strings.ToLower(art.SHA256)] = true
	}
	referencedHashes := s.collectManifestReferencedHashes(&manifest)
	for refHash := range referencedHashes {
		if !declaredHashes[refHash] {
			return nil, fmt.Errorf("%w: referenced artifact %s not included in bundle artifacts", domain.ErrJobBundleArtifactMissing, refHash)
		}
	}

	// 3. VERIFY ALL ARTIFACT CONTENT HASHES (TAMPER VERIFICATION GATE)
	// If ANY artifact hash does not match, REJECT before committing any artifact to CAS or DB.
	for _, art := range manifest.Artifacts {
		zf, ok := zipFiles[art.BundlePath]
		if !ok {
			return nil, fmt.Errorf("%w: missing artifact %s at %s", domain.ErrJobBundleArtifactMissing, art.SHA256, art.BundlePath)
		}

		arc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("open artifact %s: %w", art.BundlePath, err)
		}

		hasher := sha256.New()
		written, err := io.Copy(hasher, arc)
		arc.Close()
		if err != nil {
			return nil, fmt.Errorf("read artifact %s: %w", art.BundlePath, err)
		}

		computedHash := hex.EncodeToString(hasher.Sum(nil))
		if strings.ToLower(computedHash) != strings.ToLower(art.SHA256) {
			return nil, fmt.Errorf("%w: declared hash %s != computed hash %s for %s",
				domain.ErrJobBundleTampered, art.SHA256, computedHash, art.BundlePath)
		}
		if art.ByteSize > 0 && written != art.ByteSize {
			return nil, fmt.Errorf("%w: declared size %d != actual size %d for %s",
				domain.ErrJobBundleTampered, art.ByteSize, written, art.BundlePath)
		}
	}

	// 4. Commit all verified artifacts into CAS
	for _, art := range manifest.Artifacts {
		zf := zipFiles[art.BundlePath]
		arc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("open artifact for CAS commit: %w", err)
		}
		_, err = s.cas.Put(arc)
		arc.Close()
		if err != nil {
			return nil, fmt.Errorf("commit artifact %s into CAS: %w", art.SHA256, err)
		}
	}

	// 5. Restore SQLite state
	// Calculate machine-local CAS paths for destination host
	resolvedSourcePath, err := s.cas.ResolvePath(manifest.SourceAsset.SHA256)
	if err != nil {
		return nil, fmt.Errorf("resolve local CAS path: %w", err)
	}
	manifest.SourceAsset.CASPath = resolvedSourcePath

	if manifest.PreflightReport != nil && manifest.PreflightReport.NormalizedAudioSHA256 != "" {
		if normPath, err := s.cas.ResolvePath(manifest.PreflightReport.NormalizedAudioSHA256); err == nil {
			manifest.PreflightReport.NormalizedAudioCASPath = normPath
		}
	}

	// Upsert rights attestation
	if manifest.RightsAttestation != nil {
		if err := s.db.UpsertRightsAttestation(ctx, *manifest.RightsAttestation); err != nil {
			return nil, fmt.Errorf("restore rights attestation: %w", err)
		}
	}

	// Upsert source asset
	if err := s.db.UpsertSourceAsset(ctx, manifest.SourceAsset); err != nil {
		return nil, fmt.Errorf("restore source asset: %w", err)
	}

	// Upsert preflight report
	if manifest.PreflightReport != nil {
		if err := s.db.SavePreflightReport(ctx, *manifest.PreflightReport); err != nil {
			return nil, fmt.Errorf("restore preflight report: %w", err)
		}
	}

	// Upsert audio role plan
	if manifest.AudioRolePlan != nil {
		if err := s.db.SaveAudioRolePlan(ctx, *manifest.AudioRolePlan); err != nil {
			return nil, fmt.Errorf("restore audio role plan: %w", err)
		}
	}

	// Upsert audio stems
	if manifest.AudioStems != nil {
		// Update local CAS paths for stems
		for i := range manifest.AudioStems.Stems {
			stem := &manifest.AudioStems.Stems[i]
			if p, err := s.cas.ResolvePath(stem.AudioCASHash); err == nil {
				stem.AudioCASPath = p
			}
		}
		// Commit updated stems json into CAS
		blob, _ := json.Marshal(manifest.AudioStems)
		obj, err := s.cas.Put(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("restore audio stems to CAS: %w", err)
		}
		idx := storage.AudioStemsArtifactIndex{
			ID:             manifest.AudioStems.ID,
			AssetID:        manifest.AudioStems.AssetID,
			ProviderID:     manifest.AudioStems.ProviderID,
			CASHash:        obj.SHA256,
			ProvenanceHash: manifest.AudioStems.ProvenanceHash,
			CreatedAt:      manifest.AudioStems.CreatedAt,
		}
		if err := s.db.UpsertAudioStemsArtifactIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("restore audio stems index: %w", err)
		}
	}

	// Upsert text region plans
	for _, trp := range manifest.TextRegionPlans {
		blob, _ := json.Marshal(trp)
		obj, err := s.cas.Put(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("restore text region plan to CAS: %w", err)
		}
		idx := storage.TextRegionPlanIndex{
			ID:             trp.ID,
			AssetID:        trp.AssetID,
			ProviderID:     trp.ProviderID,
			CASHash:        obj.SHA256,
			ProvenanceHash: trp.ProvenanceHash,
			CreatedAt:      trp.CreatedAt,
		}
		if err := s.db.UpsertTextRegionPlanIndex(ctx, idx); err != nil {
			return nil, fmt.Errorf("restore text region plan index: %w", err)
		}
	}
	// Upsert job
	if err := s.db.UpsertJob(ctx, manifest.Job); err != nil {
		return nil, fmt.Errorf("restore job: %w", err)
	}

	// Upsert runs and run-associated records
	for _, runData := range manifest.Runs {
		if err := s.db.UpsertRun(ctx, runData.Run); err != nil {
			return nil, fmt.Errorf("restore run %s: %w", runData.Run.ID, err)
		}

		for _, se := range runData.StageExecutions {
			if err := s.db.UpsertStageExecution(ctx, se); err != nil {
				return nil, fmt.Errorf("restore stage execution: %w", err)
			}
		}

		for _, dec := range runData.SelectionDecisions {
			if err := s.db.UpsertSelectionDecision(ctx, dec); err != nil {
				return nil, fmt.Errorf("restore selection decision: %w", err)
			}
		}

		for _, att := range runData.ProviderAttempts {
			if err := s.db.UpsertProviderAttempt(ctx, att); err != nil {
				return nil, fmt.Errorf("restore provider attempt: %w", err)
			}
		}

		for _, ro := range runData.ReviewOverrides {
			if err := s.db.SaveReviewOverride(ctx, ro); err != nil {
				return nil, fmt.Errorf("restore review override: %w", err)
			}
		}

		for _, qr := range runData.QualityResults {
			if err := s.db.SaveQualityResult(ctx, qr); err != nil {
				return nil, fmt.Errorf("restore quality result: %w", err)
			}
		}

		// Transcript artifact
		if ta := runData.TranscriptArtifact; ta != nil {
			blob, _ := json.Marshal(ta)
			obj, err := s.cas.Put(bytes.NewReader(blob))
			if err != nil {
				return nil, fmt.Errorf("restore transcript artifact to CAS: %w", err)
			}
			idx := storage.TranscriptArtifactIndex{
				ID:                ta.ID,
				AssetID:           ta.AssetID,
				RunID:             ta.RunID,
				CASHash:           obj.SHA256,
				ProvenanceHash:    ta.ProvenanceHash,
				ASRProviderID:     ta.ASRProviderID,
				AlignerProviderID: ta.AlignerProviderID,
				CreatedAt:         ta.CreatedAt,
			}
			if err := s.db.UpsertTranscriptArtifactIndex(ctx, idx); err != nil {
				return nil, fmt.Errorf("restore transcript index: %w", err)
			}
		}
		// Translation variant
		if tv := runData.TranslationVariant; tv != nil {
			blob, _ := json.Marshal(tv)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.TranslationVariantIndex{
					ID:             tv.ID,
					AssetID:        tv.AssetID,
					RunID:          tv.RunID,
					TargetLanguage: tv.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: tv.ProvenanceHash,
					ProviderID:     tv.ProviderID,
					ModelName:      tv.ModelName,
					ModelVersion:   tv.ModelVersion,
					CreatedAt:      tv.CreatedAt,
				}
				_ = s.db.UpsertTranslationVariantIndex(ctx, idx)
			}
		}

		// Dub script variant
		if dsv := runData.DubScriptVariant; dsv != nil {
			blob, _ := json.Marshal(dsv)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.DubScriptVariantIndex{
					ID:             dsv.ID,
					AssetID:        dsv.AssetID,
					RunID:          dsv.RunID,
					TargetLanguage: dsv.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: dsv.ProvenanceHash,
					ProviderID:     dsv.ProviderID,
					ModelName:      dsv.ModelName,
					ModelVersion:   dsv.ModelVersion,
					CreatedAt:      dsv.CreatedAt,
				}
				_ = s.db.UpsertDubScriptVariantIndex(ctx, idx)
			}
		}

		// Voice assignment
		if va := runData.VoiceAssignment; va != nil {
			blob, _ := json.Marshal(va)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.VoiceAssignmentIndex{
					ID:             va.ID,
					AssetID:        va.AssetID,
					RunID:          va.RunID,
					TargetLanguage: va.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: va.ProvenanceHash,
					CreatedAt:      va.CreatedAt,
				}
				_ = s.db.UpsertVoiceAssignmentIndex(ctx, idx)
			}
		}

		// Dub segments variant
		if dsv := runData.DubSegmentsVariant; dsv != nil {
			for segIdx := range dsv.Segments {
				seg := &dsv.Segments[segIdx]
				if p, err := s.cas.ResolvePath(seg.AudioSHA256); err == nil {
					seg.AudioCASPath = p
				}
			}
			blob, _ := json.Marshal(dsv)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.DubSegmentsVariantIndex{
					ID:             dsv.ID,
					AssetID:        dsv.AssetID,
					RunID:          dsv.RunID,
					TargetLanguage: dsv.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: dsv.ProvenanceHash,
					CreatedAt:      dsv.CreatedAt,
				}
				_ = s.db.UpsertDubSegmentsVariantIndex(ctx, idx)
			}
		}

		// Dub mix artifact
		if dma := runData.DubMixArtifact; dma != nil {
			if p, err := s.cas.ResolvePath(dma.AudioCASHash); err == nil {
				dma.AudioCASPath = p
			}
			blob, _ := json.Marshal(dma)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.DubMixArtifactIndex{
					ID:             dma.ID,
					AssetID:        dma.AssetID,
					RunID:          dma.RunID,
					TargetLanguage: dma.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: dma.ProvenanceHash,
					CreatedAt:      dma.CreatedAt,
				}
				_ = s.db.UpsertDubMixArtifactIndex(ctx, idx)
			}
		}

		// Localized subtitle track
		if lst := runData.LocalizedSubtitleTrack; lst != nil {
			blob, _ := json.Marshal(lst)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.LocalizedSubtitleTrackIndex{
					ID:             lst.ID,
					AssetID:        lst.AssetID,
					RunID:          lst.RunID,
					TargetLanguage: lst.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: lst.ProvenanceHash,
					CreatedAt:      lst.CreatedAt,
				}
				_ = s.db.UpsertLocalizedSubtitleTrackIndex(ctx, idx)
			}
		}

		// Localized visual track
		if lvt := runData.LocalizedVisualTrack; lvt != nil {
			blob, _ := json.Marshal(lvt)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.LocalizedVisualTrackIndex{
					ID:             lvt.ID,
					AssetID:        lvt.AssetID,
					RunID:          lvt.RunID,
					TargetLanguage: lvt.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: lvt.ProvenanceHash,
					CreatedAt:      lvt.CreatedAt,
				}
				_ = s.db.UpsertLocalizedVisualTrackIndex(ctx, idx)
			}
		}

		// Render plan
		if rp := runData.RenderPlan; rp != nil {
			blob, _ := json.Marshal(rp)
			if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
				idx := storage.RenderPlanIndex{
					ID:             rp.ID,
					AssetID:        rp.AssetID,
					RunID:          rp.RunID,
					TargetLanguage: rp.TargetLanguage,
					CASHash:        obj.SHA256,
					ProvenanceHash: rp.ProvenanceHash,
					CreatedAt:      rp.CreatedAt,
				}
				_ = s.db.UpsertRenderPlanIndex(ctx, idx)
			}
		}

		// Render artifact
		if rad := runData.RenderArtifact; rad != nil {
			if rad.Preview != nil {
				if p, err := s.cas.ResolvePath(rad.Preview.OutputCASHash); err == nil {
					rad.Preview.OutputCASPath = p
				}
				blob, _ := json.Marshal(rad.Preview)
				if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
					idx := storage.RenderArtifactIndex{
						ID:             rad.Preview.ID,
						AssetID:        rad.Preview.AssetID,
						RunID:          rad.Preview.RunID,
						JobID:          rad.Preview.JobID,
						TargetLanguage: rad.Preview.TargetLanguage,
						Kind:           "preview",
						PlanProvenance: rad.Preview.ConsumedPlan.PlanProvenanceHash,
						PlanCASHash:    rad.Preview.ConsumedPlan.PlanCASHash,
						OutputCASHash:  rad.Preview.OutputCASHash,
						CASHash:        obj.SHA256,
						ProvenanceHash: rad.Preview.ProvenanceHash,
						OverallStatus:  rad.Preview.OverallStatus,
						CreatedAt:      rad.Preview.CreatedAt,
					}
					if idx.JobID == "" {
						idx.JobID = runData.Run.JobID
					}
					if idx.OverallStatus == "" {
						idx.OverallStatus = "PASS"
					}
					_ = s.db.UpsertRenderArtifactIndex(ctx, idx)
				}
			}
			if rad.Final != nil {
				if p, err := s.cas.ResolvePath(rad.Final.OutputCASHash); err == nil {
					rad.Final.OutputCASPath = p
				}
				blob, _ := json.Marshal(rad.Final)
				if obj, err := s.cas.Put(bytes.NewReader(blob)); err == nil {
					idx := storage.RenderArtifactIndex{
						ID:             rad.Final.ID,
						AssetID:        rad.Final.AssetID,
						RunID:          rad.Final.RunID,
						JobID:          rad.Final.JobID,
						TargetLanguage: rad.Final.TargetLanguage,
						Kind:           "final",
						PlanProvenance: rad.Final.ConsumedPlan.PlanProvenanceHash,
						PlanCASHash:    rad.Final.ConsumedPlan.PlanCASHash,
						OutputCASHash:  rad.Final.OutputCASHash,
						CASHash:        obj.SHA256,
						ProvenanceHash: rad.Final.ProvenanceHash,
						OverallStatus:  rad.Final.OverallStatus,
						CreatedAt:      rad.Final.CreatedAt,
					}
					if idx.JobID == "" {
						idx.JobID = runData.Run.JobID
					}
					if idx.OverallStatus == "" {
						idx.OverallStatus = "PASS"
					}
					_ = s.db.UpsertRenderArtifactIndex(ctx, idx)
				}
			}
		}
	}

	// License manifests
	for _, entry := range manifest.LicenseManifests {
		if s.licenseSvc != nil {
			existing, err := s.licenseSvc.GetManifest(ctx, entry.DependencyName, entry.Version)
			if err == nil && existing != nil {
				continue // Already registered on this host
			}
			if err := s.licenseSvc.RegisterManifest(ctx, entry); err != nil {
				return nil, fmt.Errorf("register license manifest: %w", err)
			}
		} else {
			if err := s.db.SaveLicenseManifest(ctx, entry); err != nil {
				return nil, fmt.Errorf("save license manifest: %w", err)
			}
		}
	}

	return &manifest, nil
}

// ImportBundleFromReader reads all bundle bytes from a sequential reader into memory/temp file and imports it.
func (s *BundleService) ImportBundleFromReader(ctx context.Context, r io.Reader, opts ImportOptions) (*domain.JobBundleManifest, error) {
	tmpFile, err := os.CreateTemp("", "bundle-import-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp bundle file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	written, err := io.Copy(tmpFile, r)
	if err != nil {
		return nil, fmt.Errorf("copy bundle bytes: %w", err)
	}

	return s.ImportBundle(ctx, tmpFile, written, opts)
}

func (s *BundleService) loadJSONFromCAS(ctx context.Context, casHash string) ([]byte, error) {
	if strings.TrimSpace(casHash) == "" {
		return nil, errors.New("empty CAS hash")
	}
	rc, err := s.cas.Get(casHash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (s *BundleService) collectReachableCASHashes(manifest *domain.JobBundleManifest) (map[string]bool, error) {
	hashes := make(map[string]bool)

	// Helper to add if valid 64-hex and verify presence in CAS
	addIfCAS := func(h string) error {
		clean := strings.ToLower(strings.TrimSpace(h))
		if len(clean) == 64 {
			if !s.cas.Exists(clean) {
				return fmt.Errorf("%w: reachable CAS object %s is missing from store", domain.ErrJobBundleArtifactMissing, clean)
			}
			hashes[clean] = true
		}
		return nil
	}

	// 1. Source asset & preflight
	if err := addIfCAS(manifest.SourceAsset.SHA256); err != nil {
		return nil, err
	}
	if manifest.PreflightReport != nil {
		if err := addIfCAS(manifest.PreflightReport.NormalizedAudioSHA256); err != nil {
			return nil, err
		}
	}

	// 2. AudioStems
	if manifest.AudioStems != nil {
		for _, stem := range manifest.AudioStems.Stems {
			if err := addIfCAS(stem.AudioCASHash); err != nil {
				return nil, err
			}
		}
		if err := addIfCAS(manifest.AudioStems.CASHash); err != nil {
			return nil, err
		}
	}

	// 3. TextRegionPlans
	for _, trp := range manifest.TextRegionPlans {
		if err := addIfCAS(trp.CASHash); err != nil {
			return nil, err
		}
	}
	// 4. Runs
	for _, runData := range manifest.Runs {
		for _, se := range runData.StageExecutions {
			if err := addIfCAS(se.ArtifactSHA256); err != nil {
				return nil, err
			}
		}
		for _, att := range runData.ProviderAttempts {
			if err := addIfCAS(att.InputHash); err != nil {
				return nil, err
			}
		}

		if ta := runData.TranscriptArtifact; ta != nil {
			if err := addIfCAS(ta.CASHash); err != nil {
				return nil, err
			}
		}
		if tv := runData.TranslationVariant; tv != nil {
			if err := addIfCAS(tv.CASHash); err != nil {
				return nil, err
			}
		}
		if dsv := runData.DubScriptVariant; dsv != nil {
			if err := addIfCAS(dsv.CASHash); err != nil {
				return nil, err
			}
		}
		if va := runData.VoiceAssignment; va != nil {
			if err := addIfCAS(va.CASHash); err != nil {
				return nil, err
			}
		}
		if dsv := runData.DubSegmentsVariant; dsv != nil {
			if err := addIfCAS(dsv.CASHash); err != nil {
				return nil, err
			}
			for _, seg := range dsv.Segments {
				if err := addIfCAS(seg.AudioSHA256); err != nil {
					return nil, err
				}
			}
			for _, rev := range dsv.ReviewSegments {
				if err := addIfCAS(rev.AudioSHA256); err != nil {
					return nil, err
				}
			}
		}
		if dma := runData.DubMixArtifact; dma != nil {
			if err := addIfCAS(dma.CASHash); err != nil {
				return nil, err
			}
			if err := addIfCAS(dma.AudioCASHash); err != nil {
				return nil, err
			}
		}
		if lst := runData.LocalizedSubtitleTrack; lst != nil {
			if err := addIfCAS(lst.CASHash); err != nil {
				return nil, err
			}
		}
		if lvt := runData.LocalizedVisualTrack; lvt != nil {
			if err := addIfCAS(lvt.CASHash); err != nil {
				return nil, err
			}
		}
		if rp := runData.RenderPlan; rp != nil {
			if err := addIfCAS(rp.CASHash); err != nil {
				return nil, err
			}
		}
		if rad := runData.RenderArtifact; rad != nil {
			if rad.Preview != nil {
				if err := addIfCAS(rad.Preview.CASHash); err != nil {
					return nil, err
				}
				if err := addIfCAS(rad.Preview.OutputCASHash); err != nil {
					return nil, err
				}
			}
			if rad.Final != nil {
				if err := addIfCAS(rad.Final.CASHash); err != nil {
					return nil, err
				}
				if err := addIfCAS(rad.Final.OutputCASHash); err != nil {
					return nil, err
				}
			}
		}
	}

	return hashes, nil
}

func (s *BundleService) collectManifestReferencedHashes(manifest *domain.JobBundleManifest) map[string]bool {
	hashes := make(map[string]bool)
	addHash := func(h string) {
		clean := strings.ToLower(strings.TrimSpace(h))
		if len(clean) == 64 {
			hashes[clean] = true
		}
	}
	addHash(manifest.SourceAsset.SHA256)
	if manifest.PreflightReport != nil {
		addHash(manifest.PreflightReport.NormalizedAudioSHA256)
	}
	if manifest.AudioStems != nil {
		for _, stem := range manifest.AudioStems.Stems {
			addHash(stem.AudioCASHash)
		}
		addHash(manifest.AudioStems.CASHash)
	}
	for _, trp := range manifest.TextRegionPlans {
		addHash(trp.CASHash)
	}
	for _, runData := range manifest.Runs {
		for _, se := range runData.StageExecutions {
			addHash(se.ArtifactSHA256)
		}
		for _, att := range runData.ProviderAttempts {
			addHash(att.InputHash)
		}
		if ta := runData.TranscriptArtifact; ta != nil {
			addHash(ta.CASHash)
		}
		if tv := runData.TranslationVariant; tv != nil {
			addHash(tv.CASHash)
		}
		if dsv := runData.DubScriptVariant; dsv != nil {
			addHash(dsv.CASHash)
		}
		if va := runData.VoiceAssignment; va != nil {
			addHash(va.CASHash)
		}
		if dsv := runData.DubSegmentsVariant; dsv != nil {
			addHash(dsv.CASHash)
			for _, seg := range dsv.Segments {
				addHash(seg.AudioSHA256)
			}
			for _, rev := range dsv.ReviewSegments {
				addHash(rev.AudioSHA256)
			}
		}
		if dma := runData.DubMixArtifact; dma != nil {
			addHash(dma.CASHash)
			addHash(dma.AudioCASHash)
		}
		if lst := runData.LocalizedSubtitleTrack; lst != nil {
			addHash(lst.CASHash)
		}
		if lvt := runData.LocalizedVisualTrack; lvt != nil {
			addHash(lvt.CASHash)
		}
		if rp := runData.RenderPlan; rp != nil {
			addHash(rp.CASHash)
		}
		if rad := runData.RenderArtifact; rad != nil {
			if rad.Preview != nil {
				addHash(rad.Preview.CASHash)
				addHash(rad.Preview.OutputCASHash)
			}
			if rad.Final != nil {
				addHash(rad.Final.CASHash)
				addHash(rad.Final.OutputCASHash)
			}
		}
	}
	return hashes
}
