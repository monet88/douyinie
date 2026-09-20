package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monet88/douyinie/internal/domain"
)

// JijiDiscoveryAdapter uses the still-reachable metadata-only Jiji search lane.
// Video/creator lookup deliberately normalizes stable identities locally rather
// than invoking Argus-gated detail/post endpoints.
type JijiDiscoveryAdapter struct {
	version       string
	healthy       bool
	cap           domain.ProviderCapability
	python        string
	script        string
	resolveSecret SecretResolver
	httpClient    *http.Client
}

func NewJijiDiscoveryAdapter(version, python, script string, resolver SecretResolver) *JijiDiscoveryAdapter {
	healthy := false
	if _, err := exec.LookPath(python); err == nil {
		if st, statErr := os.Stat(script); statErr == nil && !st.IsDir() {
			healthy = true
		}
	}
	return &JijiDiscoveryAdapter{
		version: version,
		healthy: healthy,
		cap: domain.ProviderCapability{
			Stage:          string(TypeDouyinDiscovery),
			ExecutionTier:  "hybrid",
			QualityScore:   0.95,
			CostPerUnit:    0,
			MaxConcurrency: 1,
			Features:       []string{"douyin", "keyword_search", "video_lookup", "creator_lookup"},
		},
		python:        python,
		script:        script,
		resolveSecret: resolver,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *JijiDiscoveryAdapter) ID() string                            { return "jiji_douyin_discovery" }
func (a *JijiDiscoveryAdapter) Type() ProviderType                    { return TypeDouyinDiscovery }
func (a *JijiDiscoveryAdapter) PolicyState() domain.PolicyState       { return domain.PolicyAllowed }
func (a *JijiDiscoveryAdapter) IsHealthy() bool                       { return a.healthy }
func (a *JijiDiscoveryAdapter) Capability() domain.ProviderCapability { return a.cap }
func (a *JijiDiscoveryAdapter) ModelInfo() (string, string)           { return "", a.version }
func (a *JijiDiscoveryAdapter) PrimaryCheckpointRequired() bool       { return false }

func (a *JijiDiscoveryAdapter) Search(ctx context.Context, query, continuation string, limit int, authRef string) (*domain.DiscoveryPage, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("%w: search query is required", domain.ErrInvalidDiscoveryRequest)
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	offset, err := decodeDiscoveryOffset(continuation)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid discovery continuation", domain.ErrInvalidDiscoveryRequest)
	}
	maxItems := offset + limit + 1
	tmpDir, err := os.MkdirTemp("", "douyinie-discovery-*")
	if err != nil {
		return nil, fmt.Errorf("create discovery temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// Force an explicit per-run config so the upstream CLI cannot silently read
	// its own auto-cookie files. When the caller authorizes a CredentialRef, the
	// backing cookie is materialized only into this 0600 temp config and removed
	// with the temp directory after the subprocess exits.
	emptyConfig := filepath.Join(tmpDir, "discovery-config.yml")
	configBytes, err := a.discoveryConfig(ctx, authRef)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(emptyConfig, configBytes, 0o600); err != nil {
		return nil, fmt.Errorf("create discovery config: %w", err)
	}
	cmd := exec.CommandContext(runCtx, a.python, a.script,
		"--search", query,
		"--search-max", strconv.Itoa(maxItems),
		"-p", tmpDir,
		"-c", emptyConfig,
	)
	cmd.Env = jijiDiscoveryEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, classifyDiscoveryCommandFailure(string(out))
	}
	files, err := filepath.Glob(filepath.Join(tmpDir, "search", "*.jsonl"))
	if err != nil || len(files) == 0 {
		return &domain.DiscoveryPage{Videos: []domain.DiscoveredVideo{}}, nil
	}
	sort.Strings(files)
	items, err := readJijiSearchJSONL(files[len(files)-1])
	if err != nil {
		return nil, err
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	pageItems := append([]domain.DiscoveredVideo(nil), items[offset:end]...)
	hasMore := end < len(items)
	next := ""
	if hasMore {
		next = encodeDiscoveryOffset(end)
	}
	return &domain.DiscoveryPage{Videos: pageItems, Continuation: next, HasMore: hasMore}, nil
}

func (a *JijiDiscoveryAdapter) discoveryConfig(ctx context.Context, authRef string) ([]byte, error) {
	cookie := ""
	if strings.TrimSpace(authRef) != "" {
		if a.resolveSecret == nil {
			return nil, fmt.Errorf("%w: discovery credential resolver is unavailable", domain.ErrAuthRequired)
		}
		secret, err := a.resolveSecret(ctx, authRef, a.ID())
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(secret) == "" {
			return nil, fmt.Errorf("%w: backing discovery session is unavailable", domain.ErrAuthRequired)
		}
		cookie = secret
	}
	// JSON is valid YAML and safely escapes cookie contents without placing them
	// in argv, environment variables, logs, or durable application state.
	return json.Marshal(map[string]any{"auto_cookie": false, "cookie": cookie})
}

func jijiDiscoveryEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(entry), "DOUYIN_COOKIE=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "PYTHONUTF8=1")
}

func (a *JijiDiscoveryAdapter) LookupVideo(ctx context.Context, rawURL, _ string) (*domain.DiscoveredVideo, error) {
	rawURL, _, err := a.resolveLookupURL(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Douyin video URL", err)
	}
	awemeID := extractAwemeID(rawURL)
	if awemeID == "" {
		return nil, fmt.Errorf("%w: invalid Douyin video URL", domain.ErrInvalidDiscoveryRequest)
	}
	return &domain.DiscoveredVideo{
		AwemeID:      awemeID,
		SourceID:     "douyin:aweme:" + awemeID,
		CanonicalURL: "https://www.douyin.com/video/" + awemeID,
	}, nil
}

func (a *JijiDiscoveryAdapter) LookupCreator(ctx context.Context, rawURL string, recentLimit int, _ string) (*domain.CreatorLookup, error) {
	_, u, err := a.resolveLookupURL(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Douyin creator URL", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "user") || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("%w: invalid Douyin creator URL", domain.ErrInvalidDiscoveryRequest)
	}
	if recentLimit <= 0 || recentLimit > 50 {
		recentLimit = 12
	}
	secUID := parts[1]
	return &domain.CreatorLookup{
		Creator: domain.DouyinCreator{
			SecUID:       secUID,
			CanonicalURL: "https://www.douyin.com/user/" + secUID,
		},
		RecentVideos:        []domain.DiscoveredVideo{},
		RecentLimit:         recentLimit,
		RecentViewAvailable: false,
	}, nil
}

func (a *JijiDiscoveryAdapter) resolveLookupURL(ctx context.Context, rawURL string) (string, *url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", nil, domain.ErrInvalidDiscoveryRequest
	}
	host := strings.ToLower(u.Hostname())
	if host == "v.douyin.com" {
		finalURL, _, resolveErr := resolveRedirect(ctx, a.httpClient, rawURL)
		if resolveErr != nil {
			return "", nil, fmt.Errorf("resolve Douyin short URL: %w", resolveErr)
		}
		rawURL = finalURL
		u, err = url.Parse(rawURL)
		if err != nil {
			return "", nil, domain.ErrInvalidDiscoveryRequest
		}
		host = strings.ToLower(u.Hostname())
	}
	if host != "douyin.com" && host != "www.douyin.com" {
		return "", nil, domain.ErrInvalidDiscoveryRequest
	}
	return rawURL, u, nil
}

func encodeDiscoveryOffset(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeDiscoveryOffset(token string) (int, error) {
	if strings.TrimSpace(token) == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid offset")
	}
	return n, nil
}

func readJijiSearchJSONL(path string) ([]domain.DiscoveredVideo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open discovery output: %w", err)
	}
	defer f.Close()
	var videos []domain.DiscoveredVideo
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)
	for scanner.Scan() {
		var raw map[string]any
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err != nil {
			continue
		}
		if video, ok := normalizeJijiSearchItem(raw); ok {
			videos = append(videos, video)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read discovery output: %w", err)
	}
	return videos, nil
}

func normalizeJijiSearchItem(raw map[string]any) (domain.DiscoveredVideo, bool) {
	awemeID := stringValue(raw["aweme_id"])
	if awemeID == "" {
		if aweme, ok := raw["aweme_info"].(map[string]any); ok {
			raw = aweme
			awemeID = stringValue(raw["aweme_id"])
		}
	}
	if awemeID == "" {
		return domain.DiscoveredVideo{}, false
	}
	video := domain.DiscoveredVideo{
		AwemeID:      awemeID,
		SourceID:     "douyin:aweme:" + awemeID,
		CanonicalURL: "https://www.douyin.com/video/" + awemeID,
		Title:        firstString(raw, "desc", "title"),
	}
	if ts, ok := int64Value(raw["create_time"]); ok && ts > 0 {
		t := time.Unix(ts, 0).UTC()
		video.PublishedAt = &t
	}
	if stats, ok := raw["statistics"].(map[string]any); ok {
		if likes, ok := int64Value(stats["digg_count"]); ok {
			video.LikeCount = &likes
		}
	}
	if author, ok := raw["author"].(map[string]any); ok {
		secUID := stringValue(author["sec_uid"])
		if secUID != "" {
			video.Creator = &domain.DouyinCreator{
				SecUID:       secUID,
				CanonicalURL: "https://www.douyin.com/user/" + secUID,
				DisplayName:  firstString(author, "nickname", "name"),
				AvatarURL:    nestedURL(author["avatar_thumb"]),
			}
		}
	}
	if v, ok := raw["video"].(map[string]any); ok {
		video.CoverURL = nestedURL(v["cover"])
		if d, ok := int64Value(v["duration"]); ok {
			video.DurationMs = &d
		}
	}
	return video, true
}

func nestedURL(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if list, ok := m["url_list"].([]any); ok && len(list) > 0 {
		return stringValue(list[0])
	}
	return firstString(m, "url", "uri")
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := stringValue(m[key]); v != "" {
			return v
		}
	}
	return ""
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatInt(int64(v), 10)
	default:
		return ""
	}
}

func int64Value(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func safeDiscoveryFailure(output string) string {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "argussecurityplugin"), strings.Contains(lower, "403"), strings.Contains(lower, "uifid"):
		return "provider risk-control rejected the direct request"
	case strings.Contains(lower, "login"), strings.Contains(lower, "cookie"), strings.Contains(lower, "auth"):
		return "authorized session required or expired"
	default:
		return "provider command failed"
	}
}

func classifyDiscoveryCommandFailure(output string) error {
	safe := safeDiscoveryFailure(output)
	lower := strings.ToLower(output)
	if strings.Contains(lower, "login") || strings.Contains(lower, "cookie") || strings.Contains(lower, "auth") {
		return fmt.Errorf("%w: %s", domain.ErrAuthRequired, safe)
	}
	return fmt.Errorf("jiji discovery search failed: %s", safe)
}
