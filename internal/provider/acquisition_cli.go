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
	// acquireBlockedDetail makes an adapter probe-only for operations whose
	// execution lane is known to be structurally unavailable. The check runs
	// before credential materialization or subprocess execution so a
	// deterministic platform rejection cannot be retried inside the CLI.
	acquireBlockedDetail string
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

	// Canonical Douyin URLs already carry the stable source identity. Avoid a
	// redundant HTTP fetch here: a real-page acquisition helper must remain
	// viable even when ordinary HTTP to the canonical page is WAF/Argus-gated.
	if host := strings.ToLower(u.Hostname()); host == "douyin.com" || host == "www.douyin.com" {
		if awemeID := extractAwemeID(raw); awemeID != "" {
			mediaType := "video"
			canonical := "https://www.douyin.com/video/" + awemeID
			if strings.Contains(u.Path, "/note/") {
				mediaType = "gallery"
				canonical = "https://www.douyin.com/note/" + awemeID
			}
			return &domain.SourceDescriptor{
				SourceID:     "douyin:aweme:" + awemeID,
				Platform:     "douyin",
				CanonicalURL: canonical,
				MediaType:    mediaType,
			}, nil
		}
	}

	finalURL, status, err := resolveRedirect(ctx, a.httpClient, raw)
	if err != nil {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: a.id, Detail: "short link resolution failed"}
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return nil, &domain.AcquisitionError{State: domain.AcquisitionContentUnavailable, ProviderID: a.id, Detail: fmt.Sprintf("canonical url returned http %d", status)}
	}
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		// Invariant (Issue #70 / #63): HTTP 403 alone or 429 is a WAF/rate-limit/security challenge,
		// never genuine source disappearance (CONTENT_UNAVAILABLE). It scores as acquisition failure.
		return nil, &domain.AcquisitionError{State: domain.AcquisitionAntiBotOrEmpty, ProviderID: a.id, Detail: fmt.Sprintf("canonical url returned http %d (security/waf/rate-limit challenge, not content disappearance)", status)}
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
	if a.acquireBlockedDetail != "" {
		return nil, &domain.AcquisitionError{
			State:      domain.AcquisitionAntiBotOrEmpty,
			ProviderID: a.id,
			Detail:     a.acquireBlockedDetail,
		}
	}

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
		output := string(out)
		state := ClassifyAcquisitionFailure(output)
		if state == domain.AcquisitionContentUnavailable {
			return nil, &domain.AcquisitionError{State: state, ProviderID: a.id, Detail: "content unavailable"}
		}
		return nil, &domain.AcquisitionError{State: state, ProviderID: a.id, Detail: safeAcquisitionFailureDetail(output, state)}
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
	case containsAny(lower, "captcha", "verify safety", "safety check", "verify_check", "need_verify"):
		return domain.AcquisitionCaptchaRequired
	case containsAny(lower, "session expired", "cookie expired", "login expired", "re-login"):
		return domain.AcquisitionSessionExpired
	case containsAny(lower, "browser closed", "page closed", "target closed", "browser disconnected", "page disconnected", "browser runtime unavailable", "page runtime unavailable", "browser executable not found", "page helper unavailable"):
		return domain.AcquisitionDownloadFailed
	case containsAny(lower, "login", "cookie", "auth"):
		return domain.AcquisitionAuthRequired
	case containsAny(lower, "argussecurityplugin", "uifid not found", "403", "forbidden", "waf", "429", "too many requests", "rate limit", "empty response", "anti-bot", "status_code=-1", `status_code": -1`, "status_code=5", `status_code": 5`, "风控", "服务异常"):
		// Invariant (Issue #70 / #63): 403/429/WAF/security challenge/empty-response drift is
		// classified as AntiBotOrEmpty (fallback-eligible failure), not CONTENT_UNAVAILABLE.
		return domain.AcquisitionAntiBotOrEmpty
	case containsAny(lower, "removed", "not found", "private", "404", "410"):
		return domain.AcquisitionContentUnavailable
	default:
		return domain.AcquisitionDownloadFailed
	}
}

// safeAcquisitionFailureDetail preserves an operator-actionable structural
// reason without returning provider stdout/stderr, which may contain session
// material or signed request data. Keep this vocabulary deliberately small and
// stable; the detailed raw helper output is never persisted or surfaced.
func safeAcquisitionFailureDetail(output string, state domain.AcquisitionState) string {
	lower := strings.ToLower(output)
	switch {
	case containsAny(lower, "browser closed", "page closed", "target closed", "browser disconnected", "page disconnected"):
		return "authorized browser/page is unavailable"
	case containsAny(lower, "browser runtime unavailable", "page runtime unavailable", "browser executable not found", "page helper unavailable"):
		return "page-backed browser runtime is unavailable"
	case containsAny(lower, "argussecurityplugin", "uifid not found"):
		return "provider risk-control rejected the direct request"
	}
	switch state {
	case domain.AcquisitionAuthRequired:
		return "authorized session required"
	case domain.AcquisitionSessionExpired:
		return "authorized session expired"
	case domain.AcquisitionCaptchaRequired:
		return "manual verification required"
	case domain.AcquisitionAntiBotOrEmpty:
		return "provider risk-control or empty response"
	default:
		return "adapter run failed"
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

// NewJijiAdapter builds the direct Jiji-family Douyin adapter. Probe remains
// useful for canonical URL/identity resolution, but single-video acquisition
// is intentionally fail-closed: since 2026-09-14 Douyin's Argus gate rejects
// direct aweme/detail requests even with cookies. Running the CLI here would
// only repeat a deterministic rejection before a page-backed lane can run.
func NewJijiAdapter(version string, pythonBin string, scriptPath string, resolver SecretResolver) *cliAcquisitionProvider {
	adapter := newCLIAcquisitionProvider("jiji_douyin", version, pythonBin, []string{scriptPath}, "hybrid", 0.95, "api", true,
		func(canonicalURL, destDir, cookieFile string) []string {
			args := []string{"-u", canonicalURL, "-p", destDir}
			if cookieFile != "" {
				args = append(args, "--cookie-file", cookieFile)
			}
			return args
		}, resolver)
	adapter.acquireBlockedDetail = "direct Jiji aweme/detail is Argus-gated; an operator-authorized page-backed Douyin runtime is required"
	return adapter
}

// NewF2Adapter builds the secondary F2 parser/CLI fallback adapter
// (`f2 dy -M one -u <url>`). RuntimeHost registers it for Argus-gated
// acquisition only after explicit live verification of that operation; when
// present, normal Router scoring still orders it deterministically.
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

// NewPageBackedJijiAdapter builds the acquisition lane for a separately
// configured helper that executes gated requests inside an operator-authorized
// real Douyin page. Douyinie passes only a transient credential-file path and
// the canonical source URL; the helper owns page execution and must write the
// resulting media into destDir. Raw session material and page-generated
// security fields never enter argv, returned values, logs, or provenance.
func NewPageBackedJijiAdapter(version string, helperBin string, resolver SecretResolver) *cliAcquisitionProvider {
	return newCLIAcquisitionProvider("douyin_browser_assist", version, helperBin, nil, "hybrid", 0.99, "browser", true,
		func(canonicalURL, destDir, cookieFile string) []string {
			return []string{
				"acquire",
				"--url", canonicalURL,
				"--dest", destDir,
				"--credential-file", cookieFile,
			}
		}, resolver)
}
