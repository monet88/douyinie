package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// AcquisitionService drives the canonical Douyin URL acquisition stage:
//
//	policy eligibility -> Router ladder (Jiji -> F2/parser fallback ->
//	browser-assisted auth last) where each candidate step probes the locator,
//	checks identity dedup, and acquires with per-candidate integrity
//	validation -> immutable SourceAsset
//
// Every acquisition attempt is policy-gated through the Router (policy
// eligibility runs before health/capability), fail-closed on policy/auth/
// license/content-unavailable/invalid-url/unsupported-media rejection, and
// deduplicated by content fingerprint so an equivalent reacquisition reuses
// the existing SourceAsset and its source-derived artifacts (Issue #28). A
// fallback-eligible probe failure (anti-bot, download, auth, session,
// captcha) advances the ladder to the next adapter; a candidate whose bytes
// fail integrity validation is likewise fallback-eligible. INTEGRITY_FAILED
// surfaces only after the whole ladder produced bad media (Issue #4).
type AcquisitionService struct {
	db       *storage.DB
	ingest   *IngestService
	router   *provider.Router
	tempRoot string
	nowFn    func() time.Time
}

// NewAcquisitionService wires the acquisition stage over the existing ingest
// (CAS + preflight + fingerprint dedup) and Router (policy-gated execution)
// seams. tempRoot is where adapter-produced media lands before CAS commit.
func NewAcquisitionService(db *storage.DB, ingest *IngestService, router *provider.Router, tempRoot string) *AcquisitionService {
	return &AcquisitionService{
		db:       db,
		ingest:   ingest,
		router:   router,
		tempRoot: tempRoot,
		nowFn:    time.Now,
	}
}

// AcquireRequest is the payload for one policy-gated URL acquisition.
type AcquireRequest struct {
	Locator domain.SourceLocator `json:"locator"`
	// Rights attestation is required before a durable SourceAsset is created
	// (Issue #17); it records the operator's rights basis, not service auth.
	AttestationID string                    `json:"attestation_id,omitempty"`
	Attestation   *domain.RightsAttestation `json:"attestation,omitempty"`
	// AuthorizedCredentials are safe CredentialRef ids/names backing the
	// REQUIRES_AUTHORIZATION acquisition adapters. Raw secrets never appear.
	AuthorizedCredentials []string `json:"authorized_credentials,omitempty"`
	ConsentGranted        bool     `json:"consent_granted,omitempty"`
	RunID                 string   `json:"run_id,omitempty"`
	// ForceRefresh re-acquires and may produce a new source-artifact revision;
	// it never overwrites an asset already referenced by a run.
	ForceRefresh bool `json:"force_refresh,omitempty"`
}

// AcquireResult reports the durable outcome of the acquisition stage.
type AcquireResult struct {
	Asset           *domain.SourceAsset           `json:"asset"`
	PreflightReport *domain.PreflightReport       `json:"preflight_report"`
	Attestation     *domain.RightsAttestation     `json:"attestation"`
	Provenance      *domain.AcquisitionProvenance `json:"provenance"`
	// Reused is true when the canonical source identity already resolved to
	// an existing content-fingerprinted SourceAsset (no re-download).
	Reused bool `json:"reused"`
}

// Probe resolves the locator through the policy-gated adapter ladder without
// downloading media or creating a durable asset. A fallback-eligible probe
// failure advances the ladder (Jiji -> F2 -> browser-assist); hard blocks
// (invalid url, unsupported media type, content unavailable) fail closed.
func (a *AcquisitionService) Probe(ctx context.Context, req AcquireRequest) (*domain.SourceDescriptor, error) {
	if err := a.validate(req); err != nil {
		return nil, err
	}
	var probed *domain.SourceDescriptor
	inputHash := sha256OfString(req.Locator.Location)
	err := a.router.ExecuteWithRetry(ctx, a.routeRequest(req, ""), inputHash, 1,
		func(p provider.Provider, attemptNumber int) error {
			cand, err := acquisitionAdapter(p)
			if err != nil {
				return err
			}
			desc, err := cand.Probe(ctx, req.Locator, firstCredential(req.AuthorizedCredentials))
			if err != nil {
				return err
			}
			if err := checkV1MediaPolicy(desc, cand.ID()); err != nil {
				return err
			}
			probed = desc
			return nil
		})
	if err != nil {
		return nil, err
	}
	return probed, nil
}

// Acquire runs the full ladder: each candidate step probes the locator,
// applies the V1 media policy, checks identity dedup, then acquires with
// integrity validation inside the same step; the winner's descriptor feeds
// provenance persistence.
func (a *AcquisitionService) Acquire(ctx context.Context, req AcquireRequest) (*AcquireResult, error) {
	if err := a.validate(req); err != nil {
		return nil, err
	}

	routeRes, err := a.router.Route(ctx, a.routeRequest(req, ""))
	if err != nil {
		return nil, err
	}
	authRef := firstCredential(req.AuthorizedCredentials)

	destDir, err := os.MkdirTemp(a.tempRoot, "douyinie-acquire-*")
	if err != nil {
		return nil, fmt.Errorf("create acquisition temp dir: %w", err)
	}
	defer os.RemoveAll(destDir)

	// Policy/auth/license rejection and the hard acquisition states
	// (CONTENT_UNAVAILABLE, INVALID_URL, UNSUPPORTED_MEDIA_TYPE) fail closed
	// inside ExecuteRoutedWithRetry (no retry past policy, no blind
	// fallback). Fallback-eligible probe/acquire states (transient, anti-bot,
	// auth, session, captcha, download) advance the ladder to the next
	// eligible adapter. Integrity validation runs per candidate: corrupt or
	// fingerprint-invalid media is wrapped with ErrQualityRejected so the
	// Router records a quality_failed attempt and advances the ladder
	// (Issue #4: INTEGRITY_FAILED is terminal only after every independent
	// media/provider option is exhausted). The AcquisitionError stays in the
	// error chain so the terminal state still surfaces structurally. The
	// closure captures the first candidate that passes validation.
	var probed *domain.SourceDescriptor
	var acquired *provider.AcquiredMedia
	var ingestRes *IngestResult
	var reusedRes *AcquireResult
	var usedAdapter string
	var usedVersion string

	inputHash := sha256OfString(req.Locator.Location)
	execErr := a.router.ExecuteRoutedWithRetry(ctx, a.routeRequest(req, ""), routeRes, inputHash, 1,
		func(p provider.Provider, attemptNumber int) error {
			cand, err := acquisitionAdapter(p)
			if err != nil {
				return err
			}
			// Probe participates in the ladder (Issue #4 routing rules): a
			// fallback-eligible probe failure on this adapter advances to the
			// next candidate instead of terminating the run.
			desc, err := cand.Probe(ctx, req.Locator, authRef)
			if err != nil {
				return err
			}
			// V1 media policy (resolved #4): ordinary video posts only.
			// Classify before expensive work; gallery/live reject distinctly.
			if err := checkV1MediaPolicy(desc, cand.ID()); err != nil {
				return err
			}
			// Identity dedup inside the candidate step: a previously acquired
			// canonical source resolves straight to its immutable SourceAsset;
			// source-derived artifacts stay reusable. Storage errors surface
			// fail-closed instead of silently re-downloading over an
			// inconsistent provenance binding (CODING_STANDARDS §10).
			if !req.ForceRefresh {
				prior, err := a.db.GetSourceAcquisitionBySourceID(ctx, desc.SourceID)
				if err == nil {
					res, err := a.resolveExisting(ctx, prior)
					if err != nil {
						return fmt.Errorf("%w: %w", domain.ErrInconsistentProvenance, err)
					}
					probed = desc
					reusedRes = res
					return nil
				}
				if !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("%w: lookup source acquisition: %w", domain.ErrInconsistentProvenance, err)
				}
			}
			// One directory per candidate so a failed adapter's bytes can
			// never be picked up by a later candidate's scan.
			candDir := filepath.Join(destDir, cand.ID())
			if err := os.MkdirAll(candDir, 0755); err != nil {
				return fmt.Errorf("create candidate acquisition dir: %w", err)
			}
			media, err := cand.Acquire(ctx, *desc, candDir, authRef)
			if err != nil {
				return err
			}
			// Integrity validation + CAS commit + fingerprint dedup via
			// ingest, inside the ladder candidate step.
			res, err := a.ingest.IngestLocalFile(ctx, IngestRequest{
				FilePath:      media.FilePath,
				AttestationID: req.AttestationID,
				Attestation:   req.Attestation,
			})
			if err != nil {
				if errors.Is(err, domain.ErrCorruptMedia) || errors.Is(err, domain.ErrFingerprintMismatch) {
					integrityErr := &domain.AcquisitionError{State: domain.AcquisitionIntegrityFailed, ProviderID: cand.ID(), Detail: "integrity validation failed"}
					return fmt.Errorf("%w: %w", domain.ErrQualityRejected, integrityErr)
				}
				return err
			}
			probed = desc
			acquired = media
			ingestRes = res
			usedAdapter = cand.ID()
			_, usedVersion = cand.ModelInfo()
			return nil
		})
	if execErr != nil {
		return nil, execErr
	}
	if reusedRes != nil {
		return reusedRes, nil
	}
	if acquired == nil || ingestRes == nil || probed == nil {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: usedAdapter, Detail: "no adapter produced validated media"}
	}

	prov := &domain.AcquisitionProvenance{
		ID:             uuid.NewString(),
		AssetID:        ingestRes.Asset.ID,
		SourceID:       probed.SourceID,
		Platform:       probed.Platform,
		CanonicalURL:   probed.CanonicalURL,
		Adapter:        usedAdapter,
		AdapterVersion: usedVersion,
		Method:         acquired.Method,
		Authenticated:  acquired.Authenticated,
		AcquiredAt:     a.nowFn().UTC(),
	}
	// Only the safe reference id is recorded; never the secret material.
	if authRef != "" {
		prov.AuthRefID = authRef
	}
	if err := a.db.SaveSourceAcquisition(ctx, *prov); err != nil {
		return nil, fmt.Errorf("persist acquisition provenance: %w", err)
	}

	return &AcquireResult{
		Asset:           ingestRes.Asset,
		PreflightReport: ingestRes.PreflightReport,
		Attestation:     ingestRes.Attestation,
		Provenance:      prov,
		Reused:          false,
	}, nil
}

// resolveExisting projects a prior acquisition provenance row back into the
// full durable result (asset + preflight + attestation) for dedup reuse.
func (a *AcquisitionService) resolveExisting(ctx context.Context, prior *domain.AcquisitionProvenance) (*AcquireResult, error) {
	asset, err := a.db.GetSourceAsset(ctx, prior.AssetID)
	if err != nil {
		return nil, fmt.Errorf("resolve acquired source asset %s: %w", prior.AssetID, err)
	}
	if strings.TrimSpace(asset.CASPath) == "" {
		return nil, fmt.Errorf("source asset %s has empty CAS path", asset.ID)
	}
	info, err := os.Stat(asset.CASPath)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("source asset %s missing media file in CAS at %s: %w", asset.ID, asset.CASPath, err)
	}
	report, err := a.db.GetPreflightReport(ctx, asset.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve acquired preflight report: %w", err)
	}
	ra, err := a.db.GetRightsAttestation(ctx, asset.RightsAttestationID)
	if err != nil {
		return nil, fmt.Errorf("resolve acquired rights attestation: %w", err)
	}
	return &AcquireResult{
		Asset:           asset,
		PreflightReport: report,
		Attestation:     ra,
		Provenance:      prior,
		Reused:          true,
	}, nil
}

// checkV1MediaPolicy enforces the resolved Issue #4 media policy: the V1
// localization pipeline accepts ordinary video posts only. Probe must
// classify the source before expensive work and reject gallery/live distinctly
// (UNSUPPORTED_MEDIA_TYPE, fail-closed — a different acquisition method
// cannot change what the source is).
func checkV1MediaPolicy(desc *domain.SourceDescriptor, adapterID string) error {
	if desc.MediaType == "" || desc.MediaType == "video" {
		return nil
	}
	return &domain.AcquisitionError{State: domain.AcquisitionUnsupportedMediaType, ProviderID: adapterID, Detail: fmt.Sprintf("V1 accepts ordinary video posts only, probe classified %q", desc.MediaType)}
}

func (a *AcquisitionService) validate(req AcquireRequest) error {
	if a.db == nil || a.ingest == nil || a.router == nil {
		return errors.New("acquisition service is not fully wired")
	}
	if req.Locator.Type != "douyin_url" {
		return fmt.Errorf("%w: acquisition supports douyin_url locators", domain.ErrInvalidURL)
	}
	raw := strings.TrimSpace(req.Locator.Location)
	if raw == "" {
		return fmt.Errorf("%w: locator.location is required", domain.ErrInvalidURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: locator.location is not a valid http(s) url", domain.ErrInvalidURL)
	}
	return nil
}

func (a *AcquisitionService) routeRequest(req AcquireRequest, preferred string) provider.RouteRequest {
	return provider.RouteRequest{
		RunID:                 req.RunID,
		Stage:                 provider.TypeAcquisition,
		ConsentGranted:        req.ConsentGranted,
		AuthorizedCredentials: req.AuthorizedCredentials,
		PreferredProviderID:   preferred,
	}
}

func acquisitionAdapter(p provider.Provider) (provider.AcquisitionProvider, error) {
	ap, ok := p.(provider.AcquisitionProvider)
	if !ok {
		return nil, fmt.Errorf("provider %s does not implement AcquisitionProvider", p.ID())
	}
	return ap, nil
}

func firstCredential(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0]
}

func sha256OfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
