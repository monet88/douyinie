package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
)

// TestMain doubles as the fake downloader subprocess: when
// DOUYINIE_CLI_HELPER is set, the test binary itself is re-executed by the
// CLI adapter and writes a media file into the -p destination, capturing the
// cookie-file path it was handed so the parent test can assert the transient
// session file was removed after the run.
func TestMain(m *testing.M) {
	if os.Getenv("DOUYINIE_CLI_HELPER") != "" {
		runCLIHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runCLIHelper() {
	args := os.Args
	dest := ""
	cookie := ""
	for i := range args {
		switch args[i] {
		case "-p":
			if i+1 < len(args) {
				dest = args[i+1]
			}
		case "--cookie-file", "-k":
			if i+1 < len(args) {
				cookie = args[i+1]
			}
		}
	}
	if cookie != "" {
		_ = os.WriteFile(filepath.Join(os.TempDir(), "douyinie-cli-helper-cookie-path"), []byte(cookie), 0600)
	}
	for i := range args {
		if args[i] == "--browser-fallback" {
			_ = os.WriteFile(filepath.Join(os.TempDir(), "douyinie-cli-helper-browser-fallback"), []byte("1"), 0600)
			break
		}
	}
	if dest == "" {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(dest, "helper-produced.mp4"), []byte("HELPER_MEDIA_BYTES"), 0644); err != nil {
		os.Exit(4)
	}
}

// TestCLIAdapterAcquireSuccessAndSecretCleanup proves the happy subprocess
// path produces the newest media file and that the transient cookie file
// materialized from the resolved secret is deleted after the run (Issue #17:
// session material is local-only and never lingers).
func TestCLIAdapterAcquireSuccessAndSecretCleanup(t *testing.T) {
	capture := filepath.Join(os.TempDir(), "douyinie-cli-helper-cookie-path")
	_ = os.Remove(capture)
	t.Cleanup(func() { _ = os.Remove(capture) })

	t.Setenv("DOUYINIE_CLI_HELPER", "1")
	resolverCalls := 0
	adapter := newCLIAcquisitionProvider("cli_test_jiji", "vtest", os.Args[0], nil, "hybrid", 0.95, "api", true,
		func(canonicalURL, destDir, cookieFile string) []string {
			out := []string{"-p", destDir}
			if cookieFile != "" {
				out = append(out, "--cookie-file", cookieFile)
			}
			return out
		},
		func(ctx context.Context, authRef, providerID string) (string, error) {
			resolverCalls++
			if authRef != "cred-ref-1" || providerID != "cli_test_jiji" {
				t.Errorf("resolver saw unexpected ref/provider %q/%q", authRef, providerID)
			}
			return "msToken=transient-secret", nil
		})

	dest := t.TempDir()
	media, err := adapter.Acquire(context.Background(), domain.SourceDescriptor{SourceID: "douyin:aweme:123456", CanonicalURL: "https://www.douyin.com/video/123456"}, dest, "cred-ref-1")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if media.Method != "api" || !media.Authenticated {
		t.Errorf("expected api+authenticated result, got %+v", media)
	}
	data, err := os.ReadFile(media.FilePath)
	if err != nil || string(data) != "HELPER_MEDIA_BYTES" {
		t.Fatalf("expected helper-produced media, got %q err=%v", data, err)
	}
	if resolverCalls != 1 {
		t.Errorf("secret resolver must run exactly once, got %d", resolverCalls)
	}

	cookiePath, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("helper did not capture a cookie file: %v", err)
	}
	if _, err := os.Stat(string(cookiePath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("transient session cookie file must be removed after the run: %s still exists", cookiePath)
	}
}

func TestResolveRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://www.douyin.com/video/7300000000000000000", http.StatusFound)
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	ctx := context.Background()

	final, status, err := resolveRedirect(ctx, ts.Client(), ts.URL+"/short")
	if err != nil {
		t.Fatalf("resolveRedirect: %v", err)
	}
	if status != http.StatusFound {
		t.Errorf("expected first-hop status 302, got %d", status)
	}
	if final != "https://www.douyin.com/video/7300000000000000000" {
		t.Errorf("expected redirect target as final url, got %s", final)
	}

	final, status, err = resolveRedirect(ctx, ts.Client(), ts.URL+"/gone")
	if err != nil {
		t.Fatalf("resolveRedirect gone: %v", err)
	}
	if status != http.StatusGone || final != ts.URL+"/gone" {
		t.Errorf("expected 410 passthrough, got %d %s", status, final)
	}
}

// TestCLIProbeStructuralStates covers the documented probe shapes: canonical
// 200 resolves the aweme id; 403/404 are hard CONTENT_UNAVAILABLE; a 200
// without an aweme id is the anti-bot/empty shape; malformed locators are
// INVALID_URL (never mislabeled CONTENT_UNAVAILABLE) without network.
func TestCLIProbeStructuralStates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/video/7300000000000000000", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	adapter := NewJijiAdapter("vtest", os.Args[0], "script", nil)
	adapter.httpClient = ts.Client()
	ctx := context.Background()

	desc, err := adapter.Probe(ctx, domain.SourceLocator{Type: "douyin_url", Location: ts.URL + "/video/7300000000000000000"}, "")
	if err != nil {
		t.Fatalf("canonical probe failed: %v", err)
	}
	if desc.SourceID != "douyin:aweme:7300000000000000000" || desc.Platform != "douyin" {
		t.Errorf("unexpected descriptor %+v", desc)
	}

	for _, tc := range []struct {
		name string
		path string
		want domain.AcquisitionState
	}{
		{"403", "/forbidden", domain.AcquisitionContentUnavailable},
		{"404", "/missing", domain.AcquisitionContentUnavailable},
		{"empty-200", "/empty", domain.AcquisitionAntiBotOrEmpty},
	} {
		_, err := adapter.Probe(ctx, domain.SourceLocator{Type: "douyin_url", Location: ts.URL + tc.path}, "")
		var acqErr *domain.AcquisitionError
		if !errors.As(err, &acqErr) || acqErr.State != tc.want {
			t.Errorf("%s: expected %s, got %v", tc.name, tc.want, err)
		}
	}

	// Malformed client input is INVALID_URL — a stable 4xx class — and must
	// never be mislabeled as content being gone (Issue #4 routing rules).
	_, err = adapter.Probe(ctx, domain.SourceLocator{Type: "douyin_url", Location: "not-a-url"}, "")
	var acqErr *domain.AcquisitionError
	if !errors.As(err, &acqErr) || acqErr.State != domain.AcquisitionInvalidURL {
		t.Errorf("malformed locator must be INVALID_URL without network, got %v", err)
	}
	if errors.Is(err, domain.ErrContentUnavailable) {
		t.Error("INVALID_URL must not match the CONTENT_UNAVAILABLE fail-closed sentinel")
	}
	_, err = adapter.Probe(ctx, domain.SourceLocator{Type: "local_file", Location: ts.URL + "/video/7300000000000000000"}, "")
	if !errors.As(err, &acqErr) || acqErr.State != domain.AcquisitionInvalidURL {
		t.Errorf("unsupported locator type must be INVALID_URL, got %v", err)
	}
}

// TestCLIProbeClassifiesGalleryAndLive proves probe classifies non-ordinary-
// video sources before expensive work (resolved Issue #4 V1 media policy):
// live rooms from the host without any network call, gallery/note posts from
// the resolved url. The descriptor carries the distinct media type so the
// service rejects it as UNSUPPORTED_MEDIA_TYPE, not CONTENT_UNAVAILABLE.
func TestCLIProbeClassifiesGalleryAndLive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/note/7300000000000000001", func(w http.ResponseWriter, r *http.Request) {})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	adapter := NewJijiAdapter("vtest", os.Args[0], "script", nil)
	adapter.httpClient = ts.Client()
	ctx := context.Background()

	desc, err := adapter.Probe(ctx, domain.SourceLocator{Type: "douyin_url", Location: ts.URL + "/note/7300000000000000001"}, "")
	if err != nil {
		t.Fatalf("gallery probe failed: %v", err)
	}
	if desc.MediaType != "gallery" {
		t.Errorf("expected gallery classification, got %q", desc.MediaType)
	}
	if !strings.Contains(desc.CanonicalURL, "/note/") {
		t.Errorf("gallery canonical url must keep the note path, got %s", desc.CanonicalURL)
	}

	// Live host: classified with no network (ts client would 404 the host).
	desc, err = adapter.Probe(ctx, domain.SourceLocator{Type: "douyin_url", Location: "https://live.douyin.com/123456789"}, "")
	if err != nil {
		t.Fatalf("live probe failed: %v", err)
	}
	if desc.MediaType != "live" || !strings.HasPrefix(desc.SourceID, "douyin:live:") {
		t.Errorf("expected live classification, got %+v", desc)
	}
}

// TestMediaMimeTypeReflectsProducedFile proves AcquiredMedia.MimeType follows
// the produced media file's container instead of a hardcoded video/mp4.
func TestMediaMimeTypeReflectsProducedFile(t *testing.T) {
	cases := map[string]string{
		"a.mp4":  "video/mp4",
		"b.mov":  "video/quicktime",
		"c.mkv":  "video/x-matroska",
		"d.webm": "video/webm",
		"e.flv":  "video/x-flv",
		"f.bin":  "application/octet-stream",
	}
	for name, want := range cases {
		if got := mediaMimeType(filepath.Join(t.TempDir(), name)); got != want {
			t.Errorf("mediaMimeType(%s) = %s, want %s", name, got, want)
		}
	}
}

func TestNewestMediaFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := newestMediaFile(dir); err == nil {
		t.Fatal("empty dir must error")
	}
	old := filepath.Join(dir, "old.mkv")
	fresh := filepath.Join(dir, "fresh.mp4")
	noise := filepath.Join(dir, "notes.txt")
	for _, f := range []string{old, fresh, noise} {
		if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.webm"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := newestMediaFile(dir)
	if err != nil {
		t.Fatalf("newestMediaFile: %v", err)
	}
	if got != fresh {
		t.Errorf("expected newest media file %s, got %s", fresh, got)
	}
}

// TestNewBrowserAssistAdapterWiring proves the last-resort adapter's
// deterministic construction without any live Douyin/browser interaction:
// browser method reporting, required-auth gating (structural AUTH_REQUIRED
// when no credential reference is available), and argv carrying the
// authorized cookie reference path plus the --browser-fallback flag.
func TestNewBrowserAssistAdapterWiring(t *testing.T) {
	adapter := NewBrowserAssistAdapter("vtest", os.Args[0], "version", func(ctx context.Context, authRef, providerID string) (string, error) {
		if providerID != "douyin_browser_assist" {
			t.Errorf("resolver saw unexpected provider %q", providerID)
		}
		return "session=authorized-secret", nil
	})

	if adapter.ID() != "douyin_browser_assist" {
		t.Errorf("unexpected adapter id %q", adapter.ID())
	}
	if adapter.method != "browser" {
		t.Errorf("browser-assist must report browser method, got %q", adapter.method)
	}
	if !adapter.requiresAuth {
		t.Error("browser-assist must require an authorized credential reference")
	}
	if adapter.PolicyState() != domain.PolicyRequiresAuthorization {
		t.Errorf("browser-assist must be fail-closed REQUIRES_AUTHORIZATION, got %s", adapter.PolicyState())
	}

	// No auth reference -> structural AUTH_REQUIRED, no subprocess run.
	if _, err := adapter.Acquire(context.Background(), domain.SourceDescriptor{SourceID: "douyin:aweme:1", CanonicalURL: "https://www.douyin.com/video/1"}, t.TempDir(), ""); err == nil {
		t.Fatal("expected AUTH_REQUIRED without a credential reference")
	} else {
		var acqErr *domain.AcquisitionError
		if !errors.As(err, &acqErr) || acqErr.State != domain.AcquisitionAuthRequired {
			t.Fatalf("expected structural AUTH_REQUIRED, got %v", err)
		}
	}

	// With an authorized reference the argv must carry the transient cookie
	// file and the browser fallback flag (reuses the TestMain subprocess
	// helper; no network, no real browser).
	fbCapture := filepath.Join(os.TempDir(), "douyinie-cli-helper-browser-fallback")
	_ = os.Remove(fbCapture)
	t.Cleanup(func() { _ = os.Remove(fbCapture) })
	t.Setenv("DOUYINIE_CLI_HELPER", "1")

	dest := t.TempDir()
	media, err := adapter.Acquire(context.Background(), domain.SourceDescriptor{SourceID: "douyin:aweme:2", CanonicalURL: "https://www.douyin.com/video/2"}, dest, "cred-ref-browser")
	if err != nil {
		t.Fatalf("authorized browser-assist acquire failed: %v", err)
	}
	if media.Method != "browser" || !media.Authenticated {
		t.Errorf("expected browser+authenticated media, got %+v", media)
	}
	if flag, err := os.ReadFile(fbCapture); err != nil || string(flag) != "1" {
		t.Errorf("argv must include --browser-fallback, capture err=%v", err)
	}
	cookiePath, err := os.ReadFile(filepath.Join(os.TempDir(), "douyinie-cli-helper-cookie-path"))
	if err != nil {
		t.Fatalf("helper did not capture a cookie file: %v", err)
	}
	if _, err := os.Stat(string(cookiePath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("authorized cookie file must be transient: %s still exists", cookiePath)
	}
}
