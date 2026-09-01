package provider

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/monet88/douyinie/internal/domain"
)

// SecretResolver resolves an opaque CredentialRef id/name to the backing
// session secret at call time for one specific provider. The returned value is
// transient in-memory state: adapters write it only to a temp file for the
// subprocess and delete it immediately after. It must never be logged or
// persisted (Issue #17: session material is local-only secret state).
type SecretResolver func(ctx context.Context, authRef string, providerID string) (string, error)

// cliAcquisitionProvider implements AcquisitionProvider by driving an external
// documented CLI (Jiji-family downloader, F2 CLI, or a browser-assisted
// session-capture flow) as a subprocess. URL parsing, acquisition, and failure
// classification stay separate observable steps (docs/research guidance).
type cliAcquisitionProvider struct {
	id            string
	version       string
	policy        domain.PolicyState
	healthy       bool
	cap           domain.ProviderCapability
	binary        string
	args          []string // fixed prefix args before per-run args
	method        string   // "api" | "browser"
	requiresAuth  bool
	buildArgs     func(canonicalURL, destDir, cookieFile string) []string
	resolveSecret SecretResolver
	httpClient    *http.Client
}

func (a *cliAcquisitionProvider) ID() string                            { return a.id }
func (a *cliAcquisitionProvider) Type() ProviderType                    { return TypeAcquisition }
func (a *cliAcquisitionProvider) PolicyState() domain.PolicyState       { return a.policy }
func (a *cliAcquisitionProvider) IsHealthy() bool                       { return a.healthy }
func (a *cliAcquisitionProvider) Capability() domain.ProviderCapability { return a.cap }

// ModelInfo reports an empty model name: these adapters drive operator-
// installed external CLIs, not auto-downloaded checkpoints, so the Router's
// fail-closed license-manifest gate (which targets checkpoints) does not
// apply. Douyin service-terms obligations are enforced by the policy gate
// (REQUIRES_AUTHORIZATION + operator enablement, Issue #17).
func (a *cliAcquisitionProvider) ModelInfo() (string, string) { return "", a.version }

func newCLIAcquisitionProvider(id, version, binary string, baseArgs []string, tier string, quality float64, method string, requiresAuth bool, argv func(canonicalURL, destDir, cookieFile string) []string, resolver SecretResolver) *cliAcquisitionProvider {
	healthy := false
	if _, err := exec.LookPath(binary); err == nil {
		healthy = true
	}
	return &cliAcquisitionProvider{
		id:      id,
		version: version,
		// Fail-closed default (Issue #17): automated acquisition runs only
		// when the operator enables it for an authorized-use basis and a
		// credential reference backs the session.
		policy:        domain.PolicyRequiresAuthorization,
		healthy:       healthy,
		requiresAuth:  requiresAuth,
		resolveSecret: resolver,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		binary:        binary,
		args:          baseArgs,
		method:        method,
		cap: domain.ProviderCapability{
			Stage:          string(TypeAcquisition),
			ExecutionTier:  tier,
			QualityScore:   quality,
			CostPerUnit:    0.0,
			MaxConcurrency: 1,
			Features:       []string{"douyin", method},
		},
		buildArgs: argv,
	}
}

// Probe resolves the locator to a canonical Douyin descriptor without
// downloading media. Short links are resolved via a single redirect hop; the
// aweme id becomes the stable source identity used for dedup. Invalid client
// input is INVALID_URL (never mislabeled as content gone); gallery/live
// sources are classified before expensive work so the V1 media policy can
// reject them distinctly (resolved Issue #4).
func (a *cliAcquisitionProvider) Probe(ctx context.Context, locator domain.SourceLocator, authRef string) (*domain.SourceDescriptor, error) {
	if locator.Type != "douyin_url" {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionInvalidURL, ProviderID: a.id, Detail: "unsupported locator type for douyin adapter"}
	}
	raw := strings.TrimSpace(locator.Location)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionInvalidURL, ProviderID: a.id, Detail: "invalid url"}
	}

	// Live rooms are classified from the host before any network work.
	if isDouyinLiveHost(u.Host) {
		return &domain.SourceDescriptor{
			SourceID:     "douyin:live:" + strings.Trim(raw, "/"),
			Platform:     "douyin",
			CanonicalURL: raw,
			MediaType:    "live",
		}, nil
	}

	finalURL, status, err := resolveRedirect(ctx, a.httpClient, raw)
	if err != nil {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: a.id, Detail: "short link resolution failed"}
	}
	if status == http.StatusNotFound || status == http.StatusGone || status == http.StatusForbidden {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionContentUnavailable, ProviderID: a.id, Detail: fmt.Sprintf("canonical url returned http %d", status)}
	}

	mediaType := "video"
	if strings.Contains(finalURL, "/note/") {
		mediaType = "gallery"
	}
	awemeID := extractAwemeID(finalURL)
	if awemeID == "" {
		// A 200 page without a resolvable aweme id is the documented
		// anti-bot/empty-response shape (docs/research live test 2026-08-19).
		return nil, &domain.AcquisitionError{State: domain.AcquisitionAntiBotOrEmpty, ProviderID: a.id, Detail: "no aweme id in resolved url"}
	}

	canonical := "https://www.douyin.com/video/" + awemeID
	if mediaType == "gallery" {
		canonical = "https://www.douyin.com/note/" + awemeID
	}
	return &domain.SourceDescriptor{
		SourceID:     "douyin:aweme:" + awemeID,
		Platform:     "douyin",
		CanonicalURL: canonical,
		MediaType:    mediaType,
	}, nil
}

func isDouyinLiveHost(host string) bool {
	host = strings.ToLower(host)
	return host == "live.douyin.com" || strings.HasSuffix(host, ".live.douyin.com")
}

// Acquire runs the adapter CLI for one canonical descriptor and returns the
// durable local media file. Session secrets are materialized only as a
// transient temp cookie file passed to the subprocess and removed after.
func (a *cliAcquisitionProvider) Acquire(ctx context.Context, desc domain.SourceDescriptor, destDir string, authRef string) (*AcquiredMedia, error) {
	cookieFile := ""
	authenticated := false
	if a.requiresAuth {
		if a.resolveSecret == nil || strings.TrimSpace(authRef) == "" {
			return nil, &domain.AcquisitionError{State: domain.AcquisitionAuthRequired, ProviderID: a.id, Detail: "authorized session credential reference required"}
		}
		secret, err := a.resolveSecret(ctx, authRef, a.id)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(secret) == "" {
			return nil, &domain.AcquisitionError{State: domain.AcquisitionAuthRequired, ProviderID: a.id, Detail: "backing session secret unavailable for credential reference"}
		}
		f, err := os.CreateTemp("", "douyinie-session-*")
		if err != nil {
			return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: a.id, Detail: "session materialization failed"}
		}
		if _, err := f.WriteString(secret); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: a.id, Detail: "session materialization failed"}
		}
		f.Close()
		cookieFile = f.Name()
		authenticated = true
		defer os.Remove(cookieFile)
	}

	bin, err := exec.LookPath(a.binary)
	if err != nil {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: a.id, Detail: "adapter binary unavailable"}
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, append(append([]string{}, a.args...), a.buildArgs(desc.CanonicalURL, destDir, cookieFile)...)...)
	// Preserve UTF-8 process I/O on Windows (research doc constraint).
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		state := ClassifyAcquisitionFailure(string(out))
		if state == domain.AcquisitionContentUnavailable {
			return nil, &domain.AcquisitionError{State: state, ProviderID: a.id, Detail: "content unavailable"}
		}
		return nil, &domain.AcquisitionError{State: state, ProviderID: a.id, Detail: "adapter run failed"}
	}

	mediaPath, err := newestMediaFile(destDir)
	if err != nil {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: a.id, Detail: "no media produced"}
	}
	return &AcquiredMedia{
		FilePath:      mediaPath,
		MimeType:      mediaMimeType(mediaPath),
		Method:        a.method,
		Authenticated: authenticated,
	}, nil
}

// mediaMimeType reports the produced media file's type from its extension,
// never a hardcoded container (adapters may emit .mov/.mkv/.webm/.flv).
// Known media extensions map deterministically first; the platform registry
// (which varies per machine on Windows) is only a fallback.
func mediaMimeType(path string) string {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".flv":
		return "video/x-flv"
	default:
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
		return "application/octet-stream"
	}
}

// ClassifyAcquisitionFailure maps adapter output text to one of the
// structural acquisition states. Patterns come from the documented Jiji/F2
// failure shapes (empty 200 anti-bot, cookie/login required, session expiry,
// captcha, removed/private content). Pure function; unit-tested.
func ClassifyAcquisitionFailure(output string) domain.AcquisitionState {
	lower := strings.ToLower(output)
	switch {
	case containsAny(lower, "captcha", "verify safety", "safety check"):
		return domain.AcquisitionCaptchaRequired
	case containsAny(lower, "session expired", "cookie expired", "login expired", "re-login"):
		return domain.AcquisitionSessionExpired
	case containsAny(lower, "login", "cookie", "auth"):
		return domain.AcquisitionAuthRequired
	case containsAny(lower, "empty response", "anti-bot", "status_code=-1", `status_code": -1`):
		return domain.AcquisitionAntiBotOrEmpty
	case containsAny(lower, "removed", "not found", "private", "404"):
		return domain.AcquisitionContentUnavailable
	default:
		return domain.AcquisitionDownloadFailed
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var awemeIDRe = regexp.MustCompile(`/(?:video|note)/(\d{6,})|(?:aweme_id|modal_id)=(\d{6,})`)

// extractAwemeID pulls the stable numeric aweme id out of a canonical or
// redirect-target Douyin URL. Pure function; unit-tested.
func extractAwemeID(rawURL string) string {
	m := awemeIDRe.FindStringSubmatch(rawURL)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// resolveRedirect follows at most one redirect hop (short links) and returns
// the final URL plus the first response status. It never follows into an
// auth/captcha interstitial loop.
func resolveRedirect(ctx context.Context, client *http.Client, raw string) (string, int, error) {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 1 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	resp, err := c.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	final := resp.Request.URL.String()
	if loc, uerr := resp.Location(); uerr == nil && loc != nil {
		final = loc.String()
	}
	return final, resp.StatusCode, nil
}

var mediaExts = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".webm": true, ".flv": true}

func newestMediaFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !mediaExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestTime) {
			newest = filepath.Join(dir, e.Name())
			newestTime = info.ModTime()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no media file in %s", dir)
	}
	return newest, nil
}

// NewJijiAdapter builds the preferred Jiji-family Douyin adapter
// (jiji262/douyin-downloader CLI contract: `python run.py -u <url> -p <path>`).
func NewJijiAdapter(version string, pythonBin string, scriptPath string, resolver SecretResolver) *cliAcquisitionProvider {
	return newCLIAcquisitionProvider("jiji_douyin", version, pythonBin, []string{scriptPath}, "hybrid", 0.95, "api", true,
		func(canonicalURL, destDir, cookieFile string) []string {
			args := []string{"-u", canonicalURL, "-p", destDir}
			if cookieFile != "" {
				args = append(args, "--cookie-file", cookieFile)
			}
			return args
		}, resolver)
}

// NewF2Adapter builds the secondary F2 parser/CLI fallback adapter
// (`f2 dy -M one -u <url>`). All three adapters share one tier so the locked
// ladder order (Jiji -> F2 -> browser-assist) is decided by quality score
// alone and cannot be reordered by the execution profile.
func NewF2Adapter(version string, f2Bin string, resolver SecretResolver) *cliAcquisitionProvider {
	return newCLIAcquisitionProvider("f2_douyin", version, f2Bin, []string{"dy", "-M", "one"}, "hybrid", 0.60, "api", true,
		func(canonicalURL, destDir, cookieFile string) []string {
			args := []string{"-u", canonicalURL, "-p", destDir}
			if cookieFile != "" {
				args = append(args, "-k", cookieFile)
			}
			return args
		}, resolver)
}

// NewBrowserAssistAdapter builds the last-resort browser-assisted adapter:
// the Jiji flow driven with an operator-authorized captured session
// (documented cookie_fetcher path). No captcha/anti-bot bypass is attempted.
func NewBrowserAssistAdapter(version string, pythonBin string, scriptPath string, resolver SecretResolver) *cliAcquisitionProvider {
	return newCLIAcquisitionProvider("douyin_browser_assist", version, pythonBin, []string{scriptPath}, "hybrid", 0.50, "browser", true,
		func(canonicalURL, destDir, cookieFile string) []string {
			return []string{"-u", canonicalURL, "-p", destDir, "--cookie-file", cookieFile, "--browser-fallback"}
		}, resolver)
}
