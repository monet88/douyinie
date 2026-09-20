package seam1

import (
	"bytes"
	"context"
	"encoding/json"
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
	srv := server.New(server.Config{Addr: "127.0.0.1:0", DB: db, Registry: reg, PolicySvc: pol, LicenseSvc: lic, CredSvc: cred, Router: router, Discovery: service.NewDiscoveryService(discoveryRouter)})
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
	if got := fake.Calls; len(got) != 4 || got[0] != "search" || got[1] != "search" || got[2] != "video" || got[3] != "creator" {
		t.Fatalf("unexpected provider calls: %v", got)
	}
}
