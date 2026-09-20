package seam1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
	_ "modernc.org/sqlite"
)

func TestDouyinDiscoveryIsTransientThroughRuntimeHost(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "discovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := provider.NewRegistry()
	fake := provider.NewFakeDouyinDiscoveryProvider("fake_discovery", domain.PolicyAllowed)
	fake.ModelName = ""
	likes := int64(1234)
	published := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	fake.SearchPage = &domain.DiscoveryPage{Videos: []domain.DiscoveredVideo{
		{
			AwemeID: "7600000000000000001", SourceID: "douyin:aweme:7600000000000000001",
			CanonicalURL: "https://www.douyin.com/video/7600000000000000001",
			Title:        "candidate", PublishedAt: &published, LikeCount: &likes,
		},
		{
			AwemeID: "7600000000000000004", SourceID: "douyin:aweme:7600000000000000004",
			CanonicalURL: "https://www.douyin.com/video/7600000000000000004",
			Title:        "unknown likes stay unknown",
		},
	}, Continuation: "opaque-next", HasMore: true}
	fake.Video = &domain.DiscoveredVideo{AwemeID: "7600000000000000002", SourceID: "douyin:aweme:7600000000000000002", CanonicalURL: "https://www.douyin.com/video/7600000000000000002"}
	fake.Creator = &domain.CreatorLookup{
		Creator:      domain.DouyinCreator{SecUID: "MS4wLjABAAAAfake", CanonicalURL: "https://www.douyin.com/user/MS4wLjABAAAAfake"},
		RecentVideos: []domain.DiscoveredVideo{{AwemeID: "7600000000000000003", SourceID: "douyin:aweme:7600000000000000003", CanonicalURL: "https://www.douyin.com/video/7600000000000000003"}},
		RecentLimit:  5, RecentViewAvailable: true,
	}
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	pol := governance.NewPolicyService(db)
	lic := governance.NewLicenseService(db)
	cred := governance.NewCredentialService(db)
	router := provider.NewRouter(reg, pol, lic, cred, nil, db)
	discoveryRouter := provider.NewRouter(reg, pol, lic, cred, nil, nil)
	srv := server.New(server.Config{Addr: "127.0.0.1:0", DB: db, Registry: reg, PolicySvc: pol, LicenseSvc: lic, CredSvc: cred, Router: router, Discovery: service.NewDiscoveryService(discoveryRouter, db)})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	post := func(path string, body any, out any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}

	var search domain.DiscoveryPage
	post("/api/v1/douyin/search", map[string]any{"query": "áo nữ", "limit": 20}, &search)
	if len(search.Videos) != 2 || search.Continuation != "opaque-next" || search.Videos[0].LikeCount == nil || search.Videos[1].LikeCount != nil {
		t.Fatalf("unexpected search response: %+v", search)
	}
	var filtered domain.DiscoveryPage
	post("/api/v1/douyin/search", map[string]any{"query": "áo nữ", "limit": 20, "min_likes": 2000}, &filtered)
	if len(filtered.Videos) != 1 || filtered.Videos[0].AwemeID != "7600000000000000004" || filtered.Videos[0].LikeCount != nil {
		t.Fatalf("unknown likes must remain unknown and visible under min-like filter: %+v", filtered)
	}
	var video struct {
		Video domain.DiscoveredVideo `json:"video"`
	}
	post("/api/v1/douyin/lookup/video", map[string]any{"url": "https://www.douyin.com/video/7600000000000000002"}, &video)
	if video.Video.AwemeID != "7600000000000000002" {
		t.Fatalf("unexpected video lookup: %+v", video.Video)
	}
	var creator domain.CreatorLookup
	post("/api/v1/douyin/lookup/creator", map[string]any{"url": "https://www.douyin.com/user/MS4wLjABAAAAfake", "recent_limit": 5}, &creator)
	if creator.Creator.SecUID != "MS4wLjABAAAAfake" || len(creator.RecentVideos) != 1 {
		t.Fatalf("unexpected creator lookup: %+v", creator)
	}
	invalidBody, _ := json.Marshal(map[string]any{"query": ""})
	invalidResp, err := http.Post(ts.URL+"/api/v1/douyin/search", "application/json", bytes.NewReader(invalidBody))
	if err != nil {
		t.Fatal(err)
	}
	invalidResp.Body.Close()
	if invalidResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid discovery request status=%d, want %d", invalidResp.StatusCode, http.StatusBadRequest)
	}

	jobs, err := db.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("discovery persisted localization jobs: %d", len(jobs))
	}
	decisions, err := db.ListSelectionDecisions(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 {
		t.Fatalf("discovery persisted routing decisions: %d", len(decisions))
	}
	attempts, err := db.ListProviderAttempts(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("discovery persisted provider attempts: %d", len(attempts))
	}
	for _, sourceID := range []string{"douyin:aweme:7600000000000000001", "douyin:aweme:7600000000000000002", "douyin:aweme:7600000000000000003", "douyin:aweme:7600000000000000004"} {
		if _, err := db.GetSourceAcquisitionBySourceID(context.Background(), sourceID); err == nil {
			t.Fatalf("discovery persisted source acquisition for %s", sourceID)
		}
	}
	retained, err := db.ListDouyinVideos(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 0 {
		t.Fatalf("ordinary discovery persisted %d retained videos", len(retained))
	}
	if got := fake.Calls; len(got) != 4 || got[0] != "search" || got[1] != "search" || got[2] != "video" || got[3] != "creator" {
		t.Fatalf("unexpected provider calls: %v", got)
	}
}

func TestFollowedCreatorBaselineAndDispositionLifecycleThroughRuntimeHost(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "follow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := provider.NewRegistry()
	fake := provider.NewFakeDouyinDiscoveryProvider("fake_discovery", domain.PolicyAllowed)
	fake.ModelName = ""
	creator := domain.DouyinCreator{SecUID: "creator-129", CanonicalURL: "https://www.douyin.com/user/creator-129", DisplayName: "Baseline Creator"}
	fake.Creator = &domain.CreatorLookup{Creator: creator, RecentLimit: 2, RecentViewAvailable: true, RecentVideos: []domain.DiscoveredVideo{
		{AwemeID: "129001", SourceID: "douyin:aweme:129001", CanonicalURL: "https://www.douyin.com/video/129001", Title: "baseline one", Creator: &creator},
		{AwemeID: "129002", SourceID: "douyin:aweme:129002", CanonicalURL: "https://www.douyin.com/video/129002", Title: "baseline two", Creator: &creator},
		{AwemeID: "129099", SourceID: "douyin:aweme:129099", CanonicalURL: "https://www.douyin.com/video/129099", Title: "provider over-return must be bounded", Creator: &creator},
	}}
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	pol := governance.NewPolicyService(db)
	lic := governance.NewLicenseService(db)
	cred := governance.NewCredentialService(db)
	discoveryRouter := provider.NewRouter(reg, pol, lic, cred, nil, nil)
	discoverySvc := service.NewDiscoveryService(discoveryRouter, db)
	srv := server.New(server.Config{Addr: "127.0.0.1:0", DB: db, Registry: reg, PolicySvc: pol, LicenseSvc: lic, CredSvc: cred, Discovery: discoverySvc})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	doJSON := func(method, path string, body any, want int, out any) {
		t.Helper()
		var reader *bytes.Reader
		if body == nil {
			reader = bytes.NewReader(nil)
		} else {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, ts.URL+path, reader)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d", method, path, resp.StatusCode, want)
		}
		if out != nil && resp.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}

	// An unavailable recent view is not a successful empty baseline. Follow
	// must fail closed without durable state so a later first poll cannot
	// reannounce old posts as New.
	unavailableCreator := domain.DouyinCreator{SecUID: "creator-unavailable", CanonicalURL: "https://www.douyin.com/user/creator-unavailable"}
	fake.Creator = &domain.CreatorLookup{Creator: unavailableCreator, RecentLimit: 2, RecentViewAvailable: false, RecentVideos: []domain.DiscoveredVideo{}}
	doJSON(http.MethodPost, "/api/v1/followed-creators", map[string]any{"url": unavailableCreator.CanonicalURL, "recent_limit": 2}, http.StatusServiceUnavailable, nil)
	if _, err := db.GetFollowedCreator(context.Background(), unavailableCreator.SecUID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unavailable baseline persisted followed creator: err=%v", err)
	}

	// Follow plus its baseline is one durable operation. A malformed baseline
	// item must roll back the creator row and any earlier observations.
	atomicCreator := domain.DouyinCreator{SecUID: "creator-atomic", CanonicalURL: "https://www.douyin.com/user/creator-atomic"}
	fake.Creator = &domain.CreatorLookup{Creator: atomicCreator, RecentLimit: 2, RecentViewAvailable: true, RecentVideos: []domain.DiscoveredVideo{
		{AwemeID: "atomic-valid", SourceID: "douyin:aweme:atomic-valid", CanonicalURL: "https://www.douyin.com/video/atomic-valid"},
		{AwemeID: "atomic-invalid", CanonicalURL: "https://www.douyin.com/video/atomic-invalid"},
	}}
	doJSON(http.MethodPost, "/api/v1/followed-creators", map[string]any{"url": atomicCreator.CanonicalURL, "recent_limit": 2}, http.StatusBadRequest, nil)
	if _, err := db.GetFollowedCreator(context.Background(), atomicCreator.SecUID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed baseline persisted followed creator: err=%v", err)
	}
	if _, err := db.GetDouyinVideo(context.Background(), "atomic-valid"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed baseline persisted partial video state: err=%v", err)
	}

	var followed struct {
		Creator domain.FollowedCreator `json:"creator"`
	}
	fake.Creator = &domain.CreatorLookup{Creator: creator, RecentLimit: 2, RecentViewAvailable: true, RecentVideos: []domain.DiscoveredVideo{
		{AwemeID: "129001", SourceID: "douyin:aweme:129001", CanonicalURL: "https://www.douyin.com/video/129001", Title: "baseline one", Creator: &creator},
		{AwemeID: "129002", SourceID: "douyin:aweme:129002", CanonicalURL: "https://www.douyin.com/video/129002", Title: "baseline two", Creator: &creator},
		{AwemeID: "129099", SourceID: "douyin:aweme:129099", CanonicalURL: "https://www.douyin.com/video/129099", Title: "provider over-return must be bounded", Creator: &creator},
	}}
	doJSON(http.MethodPost, "/api/v1/followed-creators", map[string]any{"url": creator.CanonicalURL, "recent_limit": 2}, http.StatusCreated, &followed)
	if !followed.Creator.Followed || followed.Creator.SecUID != creator.SecUID {
		t.Fatalf("unexpected followed creator: %+v", followed.Creator)
	}
	token1 := followed.Creator.BaselineAt
	var listed struct {
		Videos []domain.DouyinVideo `json:"videos"`
	}
	doJSON(http.MethodGet, "/api/v1/discovery/videos?sec_uid="+creator.SecUID, nil, http.StatusOK, &listed)
	if len(listed.Videos) != 2 {
		t.Fatalf("baseline retained %d videos, want 2", len(listed.Videos))
	}
	for _, video := range listed.Videos {
		if video.Origin != domain.DiscoveryOriginBaseline || video.Disposition != domain.DiscoveryDispositionSeen {
			t.Fatalf("baseline video must be historical/seen: %+v", video)
		}
	}

	// Later successful monitoring is owned by #130; #129 exposes the retention
	// contract it will call. Unknown IDs become New while known IDs only refresh.
	likes := int64(9001)
	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token1, []domain.DiscoveredVideo{
		{AwemeID: "129001", SourceID: "douyin:aweme:129001", CanonicalURL: "https://www.douyin.com/video/129001", Title: "refreshed title", LikeCount: &likes},
		{AwemeID: "129003", SourceID: "douyin:aweme:129003", CanonicalURL: "https://www.douyin.com/video/129003", Title: "new monitored"},
	}); err != nil {
		t.Fatal(err)
	}
	doJSON(http.MethodGet, "/api/v1/discovery/videos?sec_uid="+creator.SecUID, nil, http.StatusOK, &listed)
	byID := map[string]domain.DouyinVideo{}
	for _, video := range listed.Videos {
		byID[video.AwemeID] = video
	}
	if byID["129001"].Disposition != domain.DiscoveryDispositionSeen || byID["129001"].Origin != domain.DiscoveryOriginBaseline || byID["129001"].Title != "refreshed title" || byID["129001"].LikeCount == nil || *byID["129001"].LikeCount != likes {
		t.Fatalf("known observation must refresh metadata without reannouncement: %+v", byID["129001"])
	}
	if byID["129003"].Disposition != domain.DiscoveryDispositionNew || byID["129003"].Origin != domain.DiscoveryOriginMonitoring {
		t.Fatalf("unknown monitoring observation must be new: %+v", byID["129003"])
	}

	var patched struct {
		Video domain.DouyinVideo `json:"video"`
	}
	doJSON(http.MethodPatch, "/api/v1/discovery/videos/129003", map[string]any{"action": "seen"}, http.StatusOK, &patched)
	if patched.Video.Disposition != domain.DiscoveryDispositionSeen {
		t.Fatalf("mark seen failed: %+v", patched.Video)
	}
	doJSON(http.MethodPatch, "/api/v1/discovery/videos/129003", map[string]any{"action": "ignore"}, http.StatusOK, &patched)
	if patched.Video.Disposition != domain.DiscoveryDispositionIgnored {
		t.Fatalf("ignore failed: %+v", patched.Video)
	}
	doJSON(http.MethodPatch, "/api/v1/discovery/videos/129003", map[string]any{"action": "unignore"}, http.StatusOK, &patched)
	if patched.Video.Disposition != domain.DiscoveryDispositionSeen {
		t.Fatalf("unignore must return to seen: %+v", patched.Video)
	}

	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token1, []domain.DiscoveredVideo{{AwemeID: "129004", SourceID: "douyin:aweme:129004", CanonicalURL: "https://www.douyin.com/video/129004"}}); err != nil {
		t.Fatal(err)
	}
	var downloads struct {
		Videos []domain.DouyinVideo `json:"videos"`
	}
	doJSON(http.MethodPost, "/api/v1/library/media/downloads", map[string]any{"aweme_ids": []string{"129004"}}, http.StatusAccepted, &downloads)
	if len(downloads.Videos) != 1 || downloads.Videos[0].Disposition != domain.DiscoveryDispositionSeen || downloads.Videos[0].AcquisitionRequestState != "queued" {
		t.Fatalf("download request must mark New seen and retain request state: %+v", downloads.Videos)
	}
	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token1, []domain.DiscoveredVideo{
		{AwemeID: "129090", SourceID: "douyin:aweme:129090", CanonicalURL: "https://www.douyin.com/video/129090", Title: "monitored new prior to load older"},
	}); err != nil {
		t.Fatal(err)
	}
	beforeLoadOlderNew, err := db.GetDouyinVideo(context.Background(), "129090")
	if err != nil {
		t.Fatalf("get 129090 before load older: %v", err)
	}
	if beforeLoadOlderNew.Disposition != domain.DiscoveryDispositionNew {
		t.Fatalf("129090 must be New prior to load older: %+v", beforeLoadOlderNew)
	}

	fake.Creator.RecentVideos = []domain.DiscoveredVideo{
		{AwemeID: "129001", SourceID: "douyin:aweme:129001", CanonicalURL: "https://www.douyin.com/video/129001", Title: "load older refresh"},
		{AwemeID: "129000", SourceID: "douyin:aweme:129000", CanonicalURL: "https://www.douyin.com/video/129000", Title: "older history"},
		{AwemeID: "129090", SourceID: "douyin:aweme:129090", CanonicalURL: "https://www.douyin.com/video/129090", Title: "load older overlap with known new"},
	}
	before, err := db.GetFollowedCreator(context.Background(), creator.SecUID)
	if err != nil {
		t.Fatalf("get creator before load older: %v", err)
	}
	doJSON(http.MethodPost, "/api/v1/followed-creators/"+creator.SecUID+"/load-older", map[string]any{"recent_limit": 10}, http.StatusOK, &listed)
	older, err := db.GetDouyinVideo(context.Background(), "129000")
	if err != nil {
		t.Fatalf("get older video: %v", err)
	}
	after, err := db.GetFollowedCreator(context.Background(), creator.SecUID)
	if err != nil {
		t.Fatalf("get creator after load older: %v", err)
	}
	if older.Origin != domain.DiscoveryOriginHistorical || older.Disposition != domain.DiscoveryDispositionSeen {
		t.Fatalf("load older must retain newly observed historical seen: %+v", older)
	}
	overlapNew, err := db.GetDouyinVideo(context.Background(), "129090")
	if err != nil {
		t.Fatalf("get overlap video: %v", err)
	}
	if overlapNew.Disposition != domain.DiscoveryDispositionNew {
		t.Fatalf("load older must NOT clear an already-known New lifecycle item: %+v", overlapNew)
	}
	if !sameOptionalTime(before.LastSuccessfulPollAt, after.LastSuccessfulPollAt) {
		t.Fatalf("load older advanced monitoring progress")
	}

	fake.Creator.RecentViewAvailable = false
	doJSON(http.MethodPost, "/api/v1/followed-creators/"+creator.SecUID+"/load-older", map[string]any{"recent_limit": 10}, http.StatusServiceUnavailable, nil)
	fake.Creator.RecentViewAvailable = true

	// Seed video 129006 as still-New and 129007 as Ignored prior to Unfollow -> Re-follow.
	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token1, []domain.DiscoveredVideo{
		{AwemeID: "129006", SourceID: "douyin:aweme:129006", CanonicalURL: "https://www.douyin.com/video/129006", Title: "still new before unfollow"},
		{AwemeID: "129007", SourceID: "douyin:aweme:129007", CanonicalURL: "https://www.douyin.com/video/129007", Title: "ignored before unfollow"},
	}); err != nil {
		t.Fatal(err)
	}
	doJSON(http.MethodPatch, "/api/v1/discovery/videos/129007", map[string]any{"action": "ignore"}, http.StatusOK, &patched)
	beforeRefollowNew, err := db.GetDouyinVideo(context.Background(), "129006")
	if err != nil {
		t.Fatalf("get 129006 before unfollow: %v", err)
	}
	if beforeRefollowNew.Disposition != domain.DiscoveryDispositionNew {
		t.Fatalf("129006 should be New before unfollow: %+v", beforeRefollowNew)
	}

	doJSON(http.MethodDelete, "/api/v1/followed-creators/"+creator.SecUID, nil, http.StatusNoContent, nil)
	unfollowed, err := db.GetFollowedCreator(context.Background(), creator.SecUID)
	if err != nil {
		t.Fatalf("get creator after unfollow: %v", err)
	}
	if unfollowed.Followed {
		t.Fatal("unfollow must stop follow state")
	}
	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token1, []domain.DiscoveredVideo{{AwemeID: "129005", SourceID: "douyin:aweme:129005", CanonicalURL: "https://www.douyin.com/video/129005"}}); err != nil {
		t.Fatalf("late monitoring result after unfollow should be discarded cleanly: %v", err)
	}
	if _, err := db.GetDouyinVideo(context.Background(), "129005"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unfollow must stop later monitoring retention: err=%v", err)
	}

	fake.Creator.RecentVideos = []domain.DiscoveredVideo{
		{AwemeID: "129003", SourceID: "douyin:aweme:129003", CanonicalURL: "https://www.douyin.com/video/129003"},
		{AwemeID: "129006", SourceID: "douyin:aweme:129006", CanonicalURL: "https://www.douyin.com/video/129006", Title: "baseline refreshed 129006"},
		{AwemeID: "129007", SourceID: "douyin:aweme:129007", CanonicalURL: "https://www.douyin.com/video/129007", Title: "baseline refreshed 129007"},
	}
	doJSON(http.MethodPost, "/api/v1/followed-creators", map[string]any{"url": creator.CanonicalURL, "recent_limit": 5}, http.StatusCreated, &followed)
	token2 := followed.Creator.BaselineAt

	// 1. Re-follow baseline correctness: still-New video must become Seen, Ignored stays Ignored, known identity preserved.
	refollowedNew, err := db.GetDouyinVideo(context.Background(), "129006")
	if err != nil {
		t.Fatalf("get 129006 after refollow: %v", err)
	}
	if refollowedNew.Disposition != domain.DiscoveryDispositionSeen {
		t.Fatalf("re-follow baseline must promote still-New video to Seen: %+v", refollowedNew)
	}
	if !refollowedNew.FirstObservedAt.Equal(beforeRefollowNew.FirstObservedAt) {
		t.Fatalf("re-follow baseline must preserve first_observed_at: got %v want %v", refollowedNew.FirstObservedAt, beforeRefollowNew.FirstObservedAt)
	}
	refollowedIgnored, err := db.GetDouyinVideo(context.Background(), "129007")
	if err != nil {
		t.Fatalf("get 129007 after refollow: %v", err)
	}
	if refollowedIgnored.Disposition != domain.DiscoveryDispositionIgnored {
		t.Fatalf("re-follow baseline must preserve Ignored disposition: %+v", refollowedIgnored)
	}
	// 2. Prevent stale monitoring results from previous generation crossing Unfollow -> Re-follow.
	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token1, []domain.DiscoveredVideo{
		{AwemeID: "129008", SourceID: "douyin:aweme:129008", CanonicalURL: "https://www.douyin.com/video/129008", Title: "stale gen1 late observation"},
	}); err != nil {
		t.Fatalf("stale generation observation must be discarded cleanly: %v", err)
	}
	if _, err := db.GetDouyinVideo(context.Background(), "129008"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale generation monitoring result must not be retained: err=%v", err)
	}

	// Current generation token commits successfully.
	if err := discoverySvc.RetainMonitoringVideos(context.Background(), creator.SecUID, token2, []domain.DiscoveredVideo{
		{AwemeID: "129009", SourceID: "douyin:aweme:129009", CanonicalURL: "https://www.douyin.com/video/129009", Title: "current gen2 observation"},
	}); err != nil {
		t.Fatalf("current generation monitoring observation must succeed: %v", err)
	}
	curGenVideo, err := db.GetDouyinVideo(context.Background(), "129009")
	if err != nil || curGenVideo.Disposition != domain.DiscoveryDispositionNew {
		t.Fatalf("current generation observation must be retained as New: %+v err=%v", curGenVideo, err)
	}

	// 3. Make bulk RequestDownloads atomic: batch containing missing ID must not partially mutate.
	doJSON(http.MethodPost, "/api/v1/library/media/downloads", map[string]any{"aweme_ids": []string{"129009", "129-nonexistent-missing"}}, http.StatusNotFound, nil)
	unmutated, err := db.GetDouyinVideo(context.Background(), "129009")
	if err != nil {
		t.Fatalf("get unmutated 129009: %v", err)
	}
	if unmutated.Disposition != domain.DiscoveryDispositionNew || unmutated.AcquisitionRequestState != "none" {
		t.Fatalf("bulk download request with invalid target must roll back without partial mutation: %+v", unmutated)
	}

	// Valid batch commits atomically.
	doJSON(http.MethodPost, "/api/v1/library/media/downloads", map[string]any{"aweme_ids": []string{"129009"}}, http.StatusAccepted, &downloads)
	queuedVideo, err := db.GetDouyinVideo(context.Background(), "129009")
	if err != nil {
		t.Fatalf("get queued 129009: %v", err)
	}
	if queuedVideo.Disposition != domain.DiscoveryDispositionSeen || queuedVideo.AcquisitionRequestState != "queued" {
		t.Fatalf("valid bulk download request must mark Seen and queued: %+v", queuedVideo)
	}
}
func TestV19ToV20MigrationPreservesAcquisitionTruth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration-v19.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	att := domain.RightsAttestation{ID: "att-v19", AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION", DeclaredBy: "test", TermsAccepted: true, ConfirmedAt: now}
	if err := db.CreateRightsAttestation(context.Background(), att); err != nil {
		t.Fatal(err)
	}
	asset := domain.SourceAsset{ID: "asset-v19", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ByteSize: 1, MimeType: "video/mp4", OriginalFilename: "legacy.mp4", RightsAttestationID: att.ID, CASPath: "legacy", CreatedAt: now}
	if err := db.CreateSourceAsset(context.Background(), asset); err != nil {
		t.Fatal(err)
	}
	prov := domain.AcquisitionProvenance{ID: "acq-v19", AssetID: asset.ID, SourceID: "douyin:aweme:legacy-v19", Platform: "douyin", CanonicalURL: "https://www.douyin.com/video/legacy-v19", Adapter: "legacy", AdapterVersion: "v1", Method: "api", AcquiredAt: now}
	if err := db.SaveSourceAcquisition(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	job := domain.LocalizationJob{ID: "job-v19", SourceAssetID: asset.ID, TargetLanguage: "vi", Status: "completed", CreatedAt: now, UpdatedAt: now}
	if err := db.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	run := domain.LocalizationRun{ID: "run-v19", JobID: job.ID, Status: domain.RunStatusCompleted, ConfigSnapshotJSON: `{}`, CreatedAt: now, CompletedAt: &now}
	if err := db.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	plan := storage.RenderPlanIndex{ID: "plan-v19", AssetID: asset.ID, RunID: run.ID, JobID: job.ID, TargetLanguage: "vi", CASHash: "plan-cas-v19", ProvenanceHash: "plan-prov-v19", CreatedAt: now}
	if err := db.SaveRenderPlanIndex(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	final := storage.RenderArtifactIndex{ID: "render-v19", AssetID: asset.ID, RunID: run.ID, JobID: job.ID, TargetLanguage: "vi", Kind: "final", PlanProvenance: plan.ProvenanceHash, PlanCASHash: plan.CASHash, OutputCASHash: "output-cas-v19", CASHash: "artifact-cas-v19", ProvenanceHash: "render-prov-v19", OverallStatus: "completed", CreatedAt: now}
	if err := db.SaveRenderArtifactIndex(context.Background(), final); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE douyin_videos; DROP TABLE followed_creators; DELETE FROM schema_migrations WHERE version = 20;`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	db, err = storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetSourceAcquisitionBySourceID(context.Background(), prov.SourceID)
	if err != nil || got.AssetID != asset.ID {
		t.Fatalf("pre-v20 acquisition truth changed: got=%+v err=%v", got, err)
	}
	if gotAsset, err := db.GetSourceAsset(context.Background(), asset.ID); err != nil || gotAsset.SHA256 != asset.SHA256 {
		t.Fatalf("pre-v20 SourceAsset truth changed: got=%+v err=%v", gotAsset, err)
	}
	if gotJob, err := db.GetJob(context.Background(), job.ID); err != nil || gotJob.SourceAssetID != asset.ID {
		t.Fatalf("pre-v20 Job truth changed: got=%+v err=%v", gotJob, err)
	}
	if gotRun, err := db.GetRun(context.Background(), run.ID); err != nil || gotRun.JobID != job.ID {
		t.Fatalf("pre-v20 Run truth changed: got=%+v err=%v", gotRun, err)
	}
	if gotRender, err := db.GetLatestRenderArtifactIndex(context.Background(), asset.ID, "vi", "final"); err != nil || gotRender.ProvenanceHash != final.ProvenanceHash || gotRender.CASHash != final.CASHash {
		t.Fatalf("pre-v20 render/CAS references changed: got=%+v err=%v", gotRender, err)
	}
	var count int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(*) FROM schema_migrations WHERE version=20`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("v20 migration missing: count=%d err=%v", count, err)
	}
	var fabricated int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(*) FROM douyin_videos`).Scan(&fabricated); err != nil || fabricated != 0 {
		t.Fatalf("migration fabricated browsing metadata: count=%d err=%v", fabricated, err)
	}

	// Creator identity is independent from Follow state. A downloaded/retained
	// video may carry a known sec_uid even when the operator has never followed
	// that creator; v20 must not require a synthetic followed_creators row.
	knownCreatorVideo := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{
			AwemeID:      "known-creator-not-followed",
			SourceID:     "douyin:aweme:known-creator-not-followed",
			CanonicalURL: "https://www.douyin.com/video/known-creator-not-followed",
		},
		SecUID:                  "known-not-followed",
		FirstObservedAt:         now,
		LastObservedAt:          now,
		Origin:                  domain.DiscoveryOriginHistorical,
		Disposition:             domain.DiscoveryDispositionSeen,
		AcquisitionRequestState: "queued",
		LibraryVisible:          true,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := db.SaveDouyinVideoObservation(context.Background(), knownCreatorVideo); err != nil {
		t.Fatalf("known creator identity must not require Follow state: %v", err)
	}
	if _, err := db.GetFollowedCreator(context.Background(), knownCreatorVideo.SecUID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("video creator identity fabricated Follow state: err=%v", err)
	}
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
