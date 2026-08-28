package service

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/storage"
)

// IngestService coordinates rights attestation, CAS commit, media preflight, and persistence.
type IngestService struct {
	db             *storage.DB
	cas            *cas.Store
	prober         media.Prober
	normalizeAudio func(context.Context, string, string) error
}

// NewIngestService creates a new IngestService.
func NewIngestService(db *storage.DB, casStore *cas.Store, prober media.Prober) *IngestService {
	return &IngestService{
		db:             db,
		cas:            casStore,
		prober:         prober,
		normalizeAudio: media.NormalizeAudio16kMono,
	}
}

// normalizeAudioArtifact creates the canonical 16 kHz mono PCM WAV source-derived
// artifact owned by Acquisition/Preflight and commits it into CAS. Any failure is
// fatal: a successful ingest must never publish a PreflightReport without the
// normalized audio identity required by downstream speech understanding.
func (s *IngestService) normalizeAudioArtifact(ctx context.Context, sourcePath string) (cas.Object, error) {
	if s.cas == nil {
		return cas.Object{}, errors.New("CAS store is required for preflight audio normalization")
	}
	normalizer := s.normalizeAudio
	if normalizer == nil {
		normalizer = media.NormalizeAudio16kMono
	}

	tmpDir, err := os.MkdirTemp("", "douyinie-ingest-audio-*")
	if err != nil {
		return cas.Object{}, fmt.Errorf("create normalization temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpWav := filepath.Join(tmpDir, "normalized_16k.wav")
	if err := normalizer(ctx, sourcePath, tmpWav); err != nil {
		return cas.Object{}, fmt.Errorf("normalize source audio to 16 kHz mono WAV: %w", err)
	}
	normObj, err := s.cas.PutFile(tmpWav)
	if err != nil {
		return cas.Object{}, fmt.Errorf("commit normalized audio into CAS: %w", err)
	}
	return normObj, nil
}

// IngestRequest defines the payload for local file ingestion.
type IngestRequest struct {
	FilePath      string                    `json:"file_path"`
	AttestationID string                    `json:"attestation_id,omitempty"`
	Attestation   *domain.RightsAttestation `json:"attestation,omitempty"`
}

// IngestResult contains the persisted asset and its preflight report.
type IngestResult struct {
	Asset           *domain.SourceAsset       `json:"asset"`
	PreflightReport *domain.PreflightReport   `json:"preflight_report"`
	Attestation     *domain.RightsAttestation `json:"attestation"`
}

// IngestLocalFile processes a local media file into CAS, runs preflight analysis, and stores records in SQLite.
func (s *IngestService) IngestLocalFile(ctx context.Context, req IngestRequest) (*IngestResult, error) {
	if strings.TrimSpace(req.FilePath) == "" {
		return nil, fmt.Errorf("file_path is required")
	}

	// 1. Validate Rights Attestation parameters BEFORE processing
	var existingAttestation *domain.RightsAttestation
	if req.AttestationID != "" {
		ra, err := s.db.GetRightsAttestation(ctx, req.AttestationID)
		if err != nil {
			return nil, fmt.Errorf("lookup rights attestation: %w", err)
		}
		existingAttestation = ra
	} else if req.Attestation != nil {
		if !req.Attestation.TermsAccepted || strings.TrimSpace(req.Attestation.DeclaredBy) == "" {
			return nil, fmt.Errorf("%w: terms must be accepted and declared_by must be specified", domain.ErrRightsAttestationRequired)
		}
	} else {
		return nil, domain.ErrRightsAttestationRequired
	}

	// 2. Commit file into CAS (atomic same-volume rename + hash fingerprint calculation)
	casObj, err := s.cas.PutFile(req.FilePath)
	if err != nil {
		return nil, fmt.Errorf("commit into CAS: %w", err)
	}

	// 3. SHA Deduplication: if an asset with this SHA256 already exists, reuse it without creating orphan attestation
	existingAsset, err := s.db.GetSourceAssetBySHA256(ctx, casObj.SHA256)
	if err == nil && existingAsset != nil {
		ra, err := s.db.GetRightsAttestation(ctx, existingAsset.RightsAttestationID)
		if err != nil {
			return nil, fmt.Errorf("lookup existing asset rights attestation: %w", err)
		}
		report, err := s.db.GetPreflightReport(ctx, existingAsset.ID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("lookup existing asset preflight report: %w", err)
		}
		if report != nil && (report.NormalizedAudioSHA256 == "" || report.NormalizedAudioCASPath == "") {
			normObj, err := s.normalizeAudioArtifact(ctx, existingAsset.CASPath)
			if err != nil {
				return nil, fmt.Errorf("repair existing asset preflight audio: %w", err)
			}
			report.NormalizedAudioSHA256 = normObj.SHA256
			report.NormalizedAudioCASPath = normObj.Path
			if err := s.db.SavePreflightReport(ctx, *report); err != nil {
				return nil, fmt.Errorf("persist repaired preflight audio identity: %w", err)
			}
		}
		return &IngestResult{
			Asset:           existingAsset,
			PreflightReport: report,
			Attestation:     ra,
		}, nil
	}

	// 4. Preflight & Media Integrity Check: MUST pass before persisting NEW canonical SourceAsset
	report, err := s.prober.Probe(ctx, casObj.Path, casObj.SHA256)
	if err != nil {
		return nil, fmt.Errorf("media preflight probe: %w", err)
	}
	if !report.ContainerValid {
		return nil, fmt.Errorf("%w: %s", domain.ErrCorruptMedia, strings.Join(report.Errors, "; "))
	}
	if !report.FingerprintMatch {
		return nil, fmt.Errorf("%w: hash mismatch in preflight report", domain.ErrFingerprintMismatch)
	}
	// 5. Extract & normalize 16kHz mono WAV as source-derived Acquisition/Preflight CAS artifact
	if report.NormalizedAudioSHA256 == "" || report.NormalizedAudioCASPath == "" {
		normObj, err := s.normalizeAudioArtifact(ctx, casObj.Path)
		if err != nil {
			return nil, fmt.Errorf("preflight audio normalization: %w", err)
		}
		report.NormalizedAudioSHA256 = normObj.SHA256
		report.NormalizedAudioCASPath = normObj.Path
	}

	// 5. Preflight passed: Record Rights Attestation before creating new SourceAsset
	var attestation *domain.RightsAttestation
	if req.AttestationID != "" {
		attestation = existingAttestation
	} else {
		ra := *req.Attestation
		if ra.ID == "" {
			ra.ID = uuid.NewString()
		}
		if ra.ConfirmedAt.IsZero() {
			ra.ConfirmedAt = time.Now().UTC()
		}
		if ra.AttestationType == "" {
			ra.AttestationType = "OPERATOR_EXPLICIT_CONFIRMATION"
		}
		if err := s.db.CreateRightsAttestation(ctx, ra); err != nil {
			return nil, fmt.Errorf("persist rights attestation: %w", err)
		}
		attestation = &ra
	}

	// 6. Determine MIME type with case-normalized extension
	ext := strings.ToLower(filepath.Ext(req.FilePath))
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		if ext == ".mp4" || ext == ".mov" || ext == ".mkv" || ext == ".avi" {
			mimeType = "video/" + strings.TrimPrefix(ext, ".")
		} else if ext == ".mp3" || ext == ".wav" || ext == ".m4a" || ext == ".aac" {
			mimeType = "audio/" + strings.TrimPrefix(ext, ".")
		} else {
			mimeType = "application/octet-stream"
		}
	}

	// 7. Persist NEW SourceAsset
	sa := domain.SourceAsset{
		ID:                  uuid.NewString(),
		SHA256:              casObj.SHA256,
		ByteSize:            casObj.ByteSize,
		MimeType:            mimeType,
		OriginalFilename:    filepath.Base(req.FilePath),
		RightsAttestationID: attestation.ID,
		CASPath:             casObj.Path,
		CreatedAt:           time.Now().UTC(),
	}
	if err := s.db.CreateSourceAsset(ctx, sa); err != nil {
		return nil, fmt.Errorf("persist source asset: %w", err)
	}

	// 8. Persist PreflightReport linked to the new SourceAsset
	report.AssetID = sa.ID
	if err := s.db.SavePreflightReport(ctx, *report); err != nil {
		return nil, fmt.Errorf("persist preflight report: %w", err)
	}

	return &IngestResult{
		Asset:           &sa,
		PreflightReport: report,
		Attestation:     attestation,
	}, nil
}
