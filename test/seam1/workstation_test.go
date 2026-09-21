package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

func TestSeam1_WorkstationDownloadLifecycleAndMediaProjection(t *testing.T) {
	h := setupAcquisitionHarness(t)
	ctx := context.Background()
	attestationID := uuid.NewString()
	if err := h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attestationID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy: "seam1-workstation", TermsAccepted: true, ConfirmedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	credID := h.registerSessionCredential(t, "douyin_workstation", "DOUYINIE_TEST_WORKSTATION_SESSION", "workstation-secret")
	credByName := "douyin_workstation"

	const awemeID = "7600000000000000131"
	now := time.Now().UTC()
	retainedNew := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{AwemeID: awemeID, SourceID: "douyin:aweme:" + awemeID, CanonicalURL: "https://www.douyin.com/video/" + awemeID, Title: "queued download"},
		FirstObservedAt: now, LastObservedAt: now, Origin: domain.DiscoveryOriginMonitoring,
		Disposition: domain.DiscoveryDispositionNew, AcquisitionRequestState: domain.AcquisitionRequestNone,
		LibraryVisible: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.db.SaveDouyinVideoObservation(ctx, retainedNew); err != nil {
		t.Fatal(err)
	}
	video := domain.DiscoveredVideo{
		AwemeID: awemeID, SourceID: "douyin:aweme:" + awemeID,
		CanonicalURL: "https://www.douyin.com/video/" + awemeID, Title: "queued download",
	}
	body, _ := json.Marshal(service.DownloadRequest{
		AwemeIDs:      []string{awemeID, awemeID},
		AttestationID: attestationID, AuthorizedCredentials: []string{credByName},
	})
	resp, err := http.Post(h.server.URL+"/api/v1/library/media/downloads", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("queue download status=%d body=%s", resp.StatusCode, raw)
	}
	var queued struct {
		Videos []domain.DouyinVideo `json:"videos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	if len(queued.Videos) != 1 || queued.Videos[0].Disposition != domain.DiscoveryDispositionSeen || queued.Videos[0].AcquisitionRequestState != domain.AcquisitionRequestQueued {
		t.Fatalf("duplicate single/bulk request must converge to one queued row: %+v", queued.Videos)
	}

	waitForDownloadState(t, h, awemeID, domain.AcquisitionRequestDownloaded)
	stored, err := h.db.GetDouyinVideo(ctx, awemeID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RightsAttestationID != attestationID || stored.CredentialRef != credID {
		t.Fatalf("safe acquisition contract not retained: %+v", stored)
	}
	prov, err := h.db.GetSourceAcquisitionBySourceID(ctx, video.SourceID)
	if err != nil || prov.AssetID == "" {
		t.Fatalf("download did not pass through source_acquisitions: prov=%+v err=%v", prov, err)
	}
	// Same logical source request converges and cannot duplicate SourceAsset truth.
	repeatBody, _ := json.Marshal(service.DownloadRequest{AwemeIDs: []string{awemeID, awemeID}, AttestationID: attestationID, AuthorizedCredentials: []string{credID}})
	repeatResp, err := http.Post(h.server.URL+"/api/v1/library/media/downloads", "application/json", bytes.NewReader(repeatBody))
	if err != nil {
		t.Fatal(err)
	}
	repeatResp.Body.Close()
	if repeatResp.StatusCode != http.StatusAccepted || countRows(t, h.db, "source_assets") != 1 {
		t.Fatalf("same source request duplicated SourceAsset: status=%d", repeatResp.StatusCode)
	}

	// A different canonical source producing identical bytes reuses the same
	// content-fingerprinted SourceAsset through AcquisitionService.
	const awemeID2 = "7600000000000000133"
	video2 := domain.DiscoveredVideo{AwemeID: awemeID2, SourceID: "douyin:aweme:" + awemeID2, CanonicalURL: "https://www.douyin.com/video/" + awemeID2}
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", service.DownloadRequest{
		Videos: []domain.DiscoveredVideo{video2}, AttestationID: attestationID, AuthorizedCredentials: []string{credID},
	}, http.StatusAccepted)
	waitForDownloadState(t, h, awemeID2, domain.AcquisitionRequestDownloaded)
	prov2, err := h.db.GetSourceAcquisitionBySourceID(ctx, video2.SourceID)
	if err != nil || prov2.AssetID != prov.AssetID || countRows(t, h.db, "source_assets") != 1 {
		t.Fatalf("content fingerprint dedup failed: first=%+v second=%+v err=%v", prov, prov2, err)
	}

	jobID, runID := uuid.NewString(), uuid.NewString()
	if err := h.db.CreateJob(ctx, domain.LocalizationJob{ID: jobID, SourceAssetID: prov.AssetID, TargetLanguage: "vi", Status: "review_required", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CreateRun(ctx, domain.LocalizationRun{ID: runID, JobID: jobID, Status: domain.RunStatusQueued, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	mediaResp, err := http.Get(h.server.URL + "/api/v1/library/media")
	if err != nil {
		t.Fatal(err)
	}
	defer mediaResp.Body.Close()
	var library struct {
		Media []domain.MediaLibraryEntry `json:"media"`
	}
	if err := json.NewDecoder(mediaResp.Body).Decode(&library); err != nil {
		t.Fatal(err)
	}
	if len(library.Media) != 2 || library.Media[0].Asset == nil || !library.Media[0].Asset.LocalAvailable {
		t.Fatalf("media library must project authoritative local SourceAsset: %+v", library.Media)
	}
	var first *domain.MediaLibraryEntry
	for i := range library.Media {
		if library.Media[i].SourceID == video.SourceID {
			first = &library.Media[i]
		}
	}
	if first == nil || first.Thumbnail.Kind != "local_source_frame" || first.Thumbnail.AssetID != prov.AssetID || first.Thumbnail.URL != "/api/v1/assets/"+prov.AssetID+"/thumbnail" {
		t.Fatalf("downloaded media must expose regenerable local thumbnail recipe: %+v", first)
	}
	if len(first.Production) != 1 || first.Production[0].RunID != runID || first.Production[0].View != "inspector" {
		t.Fatalf("media library must hand off through existing production core: %+v", first.Production)
	}
}

func TestSeam1_WorkstationAtomicQueueRecoveryAndRequestGuards(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "workstation_atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	video := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{AwemeID: "7600000000000000140", SourceID: "douyin:aweme:7600000000000000140", CanonicalURL: "https://www.douyin.com/video/7600000000000000140"},
		FirstObservedAt: now, LastObservedAt: now, Origin: domain.DiscoveryOriginHistorical, Disposition: domain.DiscoveryDispositionSeen,
		AcquisitionRequestState: domain.AcquisitionRequestNone, LibraryVisible: true, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := db.QueueDouyinDownloads(ctx, []domain.DouyinVideo{video}, []string{video.AwemeID, "missing"}, "att-safe", "cred-safe", true, now); err == nil {
		t.Fatal("mixed invalid batch must fail closed")
	}
	if _, err := db.GetDouyinVideo(ctx, video.AwemeID); err == nil {
		t.Fatal("failed batch left partial transient persistence")
	}
	queued, err := db.QueueDouyinDownloads(ctx, []domain.DouyinVideo{video}, []string{video.AwemeID}, "att-safe", "cred-safe", true, now)
	if err != nil || len(queued) != 1 {
		t.Fatalf("atomic queue failed: %+v %v", queued, err)
	}
	claimed, err := db.ClaimNextDouyinDownload(ctx, now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.AcquisitionRequestState != domain.AcquisitionRequestDownloading || claimed.RightsAttestationID != "att-safe" || claimed.CredentialRef != "cred-safe" || !claimed.ConsentGranted {
		t.Fatalf("claim observed queued row without complete contract: %+v", claimed)
	}
	if n, err := db.RecoverDouyinDownloads(ctx, now.Add(2*time.Millisecond)); err != nil || n != 1 {
		t.Fatalf("restart recovery failed: n=%d err=%v", n, err)
	}
	recovered, err := db.GetDouyinVideo(ctx, video.AwemeID)
	if err != nil || recovered.AcquisitionRequestState != domain.AcquisitionRequestQueued || recovered.RightsAttestationID != "att-safe" {
		t.Fatalf("restart must preserve contract and requeue downloading work: %+v err=%v", recovered, err)
	}

	h := setupAcquisitionHarness(t)
	guardVideo := domain.DiscoveredVideo{AwemeID: "7600000000000000141", SourceID: "douyin:aweme:7600000000000000141", CanonicalURL: "https://www.douyin.com/video/7600000000000000141"}
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", service.DownloadRequest{Videos: []domain.DiscoveredVideo{guardVideo}}, http.StatusBadRequest)
	if _, err := h.db.GetDouyinVideo(ctx, guardVideo.AwemeID); err == nil {
		t.Fatal("missing rights contract persisted a download row")
	}
	attestationID := uuid.NewString()
	if err := h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{ID: attestationID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", DeclaredBy: "guard", TermsAccepted: true, ConfirmedAt: now}); err != nil {
		t.Fatal(err)
	}
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", service.DownloadRequest{
		Videos: []domain.DiscoveredVideo{guardVideo}, AttestationID: attestationID, AuthorizedCredentials: []string{"short-session-cookie"},
	}, http.StatusServiceUnavailable)
	if _, err := h.db.GetDouyinVideo(ctx, guardVideo.AwemeID); err == nil {
		t.Fatal("unresolved short credential value persisted a download row")
	}
	bulkNew := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{AwemeID: "7600000000000000144", SourceID: "douyin:aweme:7600000000000000144", CanonicalURL: "https://www.douyin.com/video/7600000000000000144"},
		FirstObservedAt: now, LastObservedAt: now, Origin: domain.DiscoveryOriginMonitoring, Disposition: domain.DiscoveryDispositionNew,
		AcquisitionRequestState: domain.AcquisitionRequestNone, LibraryVisible: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.db.SaveDouyinVideoObservation(ctx, bulkNew); err != nil {
		t.Fatal(err)
	}
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", service.DownloadRequest{
		AwemeIDs: []string{bulkNew.AwemeID, "7600000000000000999"}, AttestationID: attestationID,
	}, http.StatusNotFound)
	unmutated, err := h.db.GetDouyinVideo(ctx, bulkNew.AwemeID)
	if err != nil || unmutated.Disposition != domain.DiscoveryDispositionNew || unmutated.AcquisitionRequestState != domain.AcquisitionRequestNone {
		t.Fatalf("invalid bulk request must roll back without partial mutation: %+v err=%v", unmutated, err)
	}
	legacyCredID := h.registerSessionCredential(t, "douyin_legacy_queue", "DOUYINIE_TEST_LEGACY_QUEUE", "legacy-queue-secret")
	legacyQueued := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{AwemeID: "7600000000000000143", SourceID: "douyin:aweme:7600000000000000143", CanonicalURL: "https://www.douyin.com/video/7600000000000000143"},
		FirstObservedAt: now, LastObservedAt: now, Origin: domain.DiscoveryOriginHistorical, Disposition: domain.DiscoveryDispositionSeen,
		AcquisitionRequestState: domain.AcquisitionRequestQueued, LibraryVisible: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.db.SaveDouyinVideoObservation(ctx, legacyQueued); err != nil {
		t.Fatal(err)
	}
	startupSvc := service.NewWorkstationService(h.db, h.acq, h.cred)
	if err := startupSvc.Start(); err != nil {
		t.Fatal(err)
	}
	defer startupSvc.Stop()
	time.Sleep(50 * time.Millisecond)
	preBind, _ := h.db.GetDouyinVideo(ctx, legacyQueued.AwemeID)
	if preBind.AcquisitionRequestState != domain.AcquisitionRequestQueued || preBind.RightsAttestationID != "" {
		t.Fatalf("startup must leave pre-#131 unbound queued row untouched: %+v", preBind)
	}
	if _, err := startupSvc.QueueDownloads(ctx, service.DownloadRequest{
		AwemeIDs: []string{legacyQueued.AwemeID}, AttestationID: attestationID, AuthorizedCredentials: []string{legacyCredID},
	}); err != nil {
		t.Fatal(err)
	}
	waitForDownloadState(t, h, legacyQueued.AwemeID, domain.AcquisitionRequestDownloaded)
	bound, _ := h.db.GetDouyinVideo(ctx, legacyQueued.AwemeID)
	if bound.RightsAttestationID != attestationID || bound.CredentialRef != legacyCredID || bound.ConsentGranted {
		t.Fatalf("legacy queued row was not explicitly rebound without synthesized consent: %+v", bound)
	}
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", service.DownloadRequest{
		Videos: []domain.DiscoveredVideo{guardVideo}, AttestationID: attestationID, AuthorizedCredentials: []string{"sk-raw-secret"},
	}, http.StatusBadRequest)
	if _, err := h.db.GetDouyinVideo(ctx, guardVideo.AwemeID); err == nil {
		t.Fatal("raw credential material persisted a download row")
	}
	mismatch := guardVideo
	mismatch.AwemeID = "7600000000000000142"
	mismatch.SourceID = "douyin:aweme:7600000000000000998"
	mismatch.CanonicalURL = "https://www.douyin.com/video/7600000000000000142"
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", service.DownloadRequest{
		Videos: []domain.DiscoveredVideo{mismatch}, AttestationID: attestationID,
	}, http.StatusBadRequest)
	if _, err := h.db.GetDouyinVideo(ctx, mismatch.AwemeID); err == nil {
		t.Fatal("mismatched canonical identity persisted a download row")
	}
}

func TestSeam1_WorkstationFailureRetryAndLegacyProjection(t *testing.T) {
	h := setupAcquisitionHarness(t)
	ctx := context.Background()
	attestationID := uuid.NewString()
	if err := h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID: attestationID, AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy: "seam1-workstation", TermsAccepted: true, ConfirmedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	credID := h.registerSessionCredential(t, "douyin_workstation_retry", "DOUYINIE_TEST_WORKSTATION_RETRY", "retry-secret")
	neverRequested := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{AwemeID: "7600000000000000888", SourceID: "douyin:aweme:7600000000000000888", CanonicalURL: "https://www.douyin.com/video/7600000000000000888"},
		FirstObservedAt: time.Now().UTC(), LastObservedAt: time.Now().UTC(), Origin: domain.DiscoveryOriginBaseline,
		Disposition: domain.DiscoveryDispositionSeen, AcquisitionRequestState: domain.AcquisitionRequestNone,
		LibraryVisible: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := h.db.SaveDouyinVideoObservation(ctx, neverRequested); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"fake_jiji_douyin", "fake_f2_douyin", "fake_browser_assist"} {
		h.acquisitionProvider(t, id).AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: id, Detail: "cookie=session=SUPER_SECRET_TOKEN"}
	}
	const awemeID = "7600000000000000132"
	req := service.DownloadRequest{
		Videos:        []domain.DiscoveredVideo{{AwemeID: awemeID, SourceID: "douyin:aweme:" + awemeID, CanonicalURL: "https://www.douyin.com/video/" + awemeID}},
		AttestationID: attestationID, AuthorizedCredentials: []string{credID},
	}
	postJSON(t, h.server.URL+"/api/v1/library/media/downloads", req, http.StatusAccepted)
	waitForDownloadState(t, h, awemeID, domain.AcquisitionRequestFailed)
	failed, _ := h.db.GetDouyinVideo(ctx, awemeID)
	if failed.AcquisitionFailure == nil || failed.AcquisitionFailure.Code != string(domain.AcquisitionDownloadFailed) {
		t.Fatalf("structured failure missing: %+v", failed.AcquisitionFailure)
	}
	if strings.Contains(failed.AcquisitionFailure.Message, "SUPER_SECRET_TOKEN") {
		t.Fatalf("provider detail leaked into durable failure: %+v", failed.AcquisitionFailure)
	}

	for _, id := range []string{"fake_jiji_douyin", "fake_f2_douyin", "fake_browser_assist"} {
		h.acquisitionProvider(t, id).AcquireErr = nil
	}
	postJSON(t, h.server.URL+"/api/v1/library/media/"+awemeID+"/retry", map[string]any{}, http.StatusAccepted)
	waitForDownloadState(t, h, awemeID, domain.AcquisitionRequestDownloaded)

	legacyURL := "https://www.douyin.com/video/7600000000000000999"
	legacyResp, legacy := h.acquire(t, map[string]any{
		"locator":        map[string]string{"type": "douyin_url", "location": legacyURL},
		"attestation_id": attestationID, "authorized_credentials": []string{credID},
	})
	if legacyResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(legacyResp.Body)
		legacyResp.Body.Close()
		t.Fatalf("legacy direct acquisition failed: %d %s", legacyResp.StatusCode, raw)
	}
	legacyResp.Body.Close()
	if legacy.Asset == nil {
		t.Fatal("legacy direct acquisition missing SourceAsset")
	}
	mediaResp, err := http.Get(h.server.URL + "/api/v1/library/media")
	if err != nil {
		t.Fatal(err)
	}
	defer mediaResp.Body.Close()
	var library struct {
		Media []domain.MediaLibraryEntry `json:"media"`
	}
	if err := json.NewDecoder(mediaResp.Body).Decode(&library); err != nil {
		t.Fatal(err)
	}
	foundLegacy := false
	for _, item := range library.Media {
		if item.SourceID == neverRequested.SourceID {
			t.Fatalf("never-requested retained row leaked into Media Library: %+v", item)
		}
		if item.SourceID == legacy.Provenance.SourceID {
			foundLegacy = item.LegacyFallback && item.Asset != nil && item.AcquisitionState == domain.AcquisitionRequestDownloaded
		}
	}
	if !foundLegacy {
		t.Fatalf("pre-v20 source_acquisition missing truthful legacy projection: %+v", library.Media)
	}
}

func waitForDownloadState(t *testing.T, h *acquisitionHarness, awemeID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		video, err := h.db.GetDouyinVideo(context.Background(), awemeID)
		if err == nil && video.AcquisitionRequestState == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	video, _ := h.db.GetDouyinVideo(context.Background(), awemeID)
	t.Fatalf("download state=%q want %q", video.AcquisitionRequestState, want)
}

func postJSON(t *testing.T, url string, body any, wantStatus int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		message, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status=%d want=%d body=%s", url, resp.StatusCode, wantStatus, message)
	}
}
