package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestJijiDiscoveryLookupNormalizesStableIdentity(t *testing.T) {
	adapter := NewJijiDiscoveryAdapter("test", "missing-python", "missing-script", nil)

	video, err := adapter.LookupVideo(context.Background(), "https://www.douyin.com/video/7659396126545619322?foo=1", "")
	if err != nil {
		t.Fatal(err)
	}
	if video.AwemeID != "7659396126545619322" || video.SourceID != "douyin:aweme:7659396126545619322" || video.CanonicalURL != "https://www.douyin.com/video/7659396126545619322" {
		t.Fatalf("unexpected normalized video: %+v", video)
	}
	if _, err := adapter.LookupVideo(context.Background(), "https://example.com/video/7659396126545619322", ""); err == nil {
		t.Fatal("non-Douyin host must not be normalized into a Douyin candidate")
	}

	creator, err := adapter.LookupCreator(context.Background(), "https://www.douyin.com/user/MS4wLjABAAAAstable?from_tab_name=main", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if creator.Creator.SecUID != "MS4wLjABAAAAstable" || creator.Creator.CanonicalURL != "https://www.douyin.com/user/MS4wLjABAAAAstable" {
		t.Fatalf("unexpected normalized creator: %+v", creator.Creator)
	}
	if creator.RecentLimit != 7 || creator.RecentViewAvailable || len(creator.RecentVideos) != 0 {
		t.Fatalf("Argus-gated recent view must stay truthfully unavailable: %+v", creator)
	}
	if _, err := adapter.LookupCreator(context.Background(), "https://example.com/?next=https://www.douyin.com/user/spoofed", 7, ""); !errors.Is(err, domain.ErrInvalidDiscoveryRequest) {
		t.Fatalf("wrapped non-Douyin creator URL must be rejected as invalid input, got %v", err)
	}
}

func TestJijiDiscoveryLookupResolvesDouyinShortVideoURL(t *testing.T) {
	canonical := "https://www.douyin.com/video/7659396126545619322"
	adapter := newShortLinkLookupAdapter(t, canonical)

	video, err := adapter.LookupVideo(context.Background(), "https://v.douyin.com/short-code/", "")
	if err != nil {
		t.Fatal(err)
	}
	if video.AwemeID != "7659396126545619322" || video.CanonicalURL != canonical {
		t.Fatalf("unexpected normalized short-link video: %+v", video)
	}
}

func TestJijiDiscoveryLookupResolvesDouyinShortCreatorURL(t *testing.T) {
	canonical := "https://www.douyin.com/user/MS4wLjABAAAAshort"
	adapter := newShortLinkLookupAdapter(t, canonical)

	creator, err := adapter.LookupCreator(context.Background(), "https://v.douyin.com/creator-code/", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if creator.Creator.SecUID != "MS4wLjABAAAAshort" || creator.Creator.CanonicalURL != canonical {
		t.Fatalf("unexpected normalized short-link creator: %+v", creator.Creator)
	}

	evil := newShortLinkLookupAdapter(t, "https://example.com/user/spoofed")
	if _, err := evil.LookupCreator(context.Background(), "https://v.douyin.com/evil-code/", 7, ""); !errors.Is(err, domain.ErrInvalidDiscoveryRequest) {
		t.Fatalf("short-link redirect to non-Douyin host must be rejected, got %v", err)
	}
}

func newShortLinkLookupAdapter(t *testing.T, redirectTo string) *JijiDiscoveryAdapter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTo, http.StatusFound)
	}))
	t.Cleanup(server.Close)

	adapter := NewJijiDiscoveryAdapter("test", "missing-python", "missing-script", nil)
	adapter.httpClient = server.Client()

	// Keep the production allowlist meaningful while substituting only the
	// transport target for this deterministic redirect test.
	originalTransport := adapter.httpClient.Transport
	adapter.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "v.douyin.com" {
			return originalTransport.RoundTrip(req)
		}
		rewritten := req.Clone(req.Context())
		target, _ := url.Parse(server.URL)
		rewritten.URL.Scheme = target.Scheme
		rewritten.URL.Host = target.Host
		return originalTransport.RoundTrip(rewritten)
	})
	return adapter
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestJijiDiscoveryConfigUsesAuthorizedCredentialOnly(t *testing.T) {
	t.Setenv("DOUYIN_COOKIE", "ambient=must-not-leak")
	resolverCalls := 0
	adapter := NewJijiDiscoveryAdapter("test", "missing-python", "missing-script", func(_ context.Context, ref, providerID string) (string, error) {
		resolverCalls++
		if ref != "cred-1" || providerID != "jiji_douyin_discovery" {
			t.Fatalf("resolver saw unexpected ref/provider %q/%q", ref, providerID)
		}
		return "session=authorized", nil
	})
	configBytes, err := adapter.discoveryConfig(context.Background(), "cred-1")
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
	var cfg map[string]any
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["cookie"] != "session=authorized" || cfg["auto_cookie"] != false {
		t.Fatalf("unexpected discovery config: %+v", cfg)
	}
	if strings.Contains(string(configBytes), "must-not-leak") {
		t.Fatal("ambient DOUYIN_COOKIE leaked into explicit discovery config")
	}
}

func TestNormalizeJijiSearchItemPreservesUnknownLikes(t *testing.T) {
	raw := map[string]any{
		"aweme_id": "7659396126545619322",
		"desc":     "candidate",
		"author": map[string]any{
			"sec_uid":  "creator-sec",
			"nickname": "Creator",
		},
	}
	video, ok := normalizeJijiSearchItem(raw)
	if !ok {
		t.Fatal("expected normalized item")
	}
	if video.LikeCount != nil {
		t.Fatalf("unknown likes were fabricated: %v", *video.LikeCount)
	}
	if video.Creator == nil || video.Creator.SecUID != "creator-sec" {
		t.Fatalf("missing creator identity: %+v", video.Creator)
	}
}

func TestDiscoveryContinuationIsOpaqueRoundTrip(t *testing.T) {
	token := encodeDiscoveryOffset(37)
	if token == "37" {
		t.Fatal("continuation leaked raw offset")
	}
	offset, err := decodeDiscoveryOffset(token)
	if err != nil || offset != 37 {
		t.Fatalf("round trip = %d, %v", offset, err)
	}
}

func TestJijiDiscoveryEnvDoesNotLeakAmbientCookie(t *testing.T) {
	t.Setenv("DOUYIN_COOKIE", "session=must-not-leak")
	for _, entry := range jijiDiscoveryEnv() {
		if strings.HasPrefix(strings.ToUpper(entry), "DOUYIN_COOKIE=") {
			t.Fatalf("ambient Douyin cookie leaked into discovery subprocess env: %q", entry)
		}
	}
}

func TestReadJijiSearchJSONLPreservesNumericAwemeID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search.jsonl")
	if err := os.WriteFile(path, []byte("{\"aweme_id\":7659396126545619322,\"desc\":\"candidate\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	videos, err := readJijiSearchJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 1 || videos[0].AwemeID != "7659396126545619322" {
		t.Fatalf("numeric aweme_id lost precision: %+v", videos)
	}
}
