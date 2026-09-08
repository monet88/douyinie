package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/monet88/douyinie/internal/domain"
)

// GatewayTranslationProvider is an OpenAI-compatible gateway translation adapter.
// It routes through approved OpenAI-compatible gateway endpoints (e.g. Gemini 3.8 Flash,
// DeepSeek v4 Flash Vision Exp) using an alias, without direct provider SDKs.
// Remote models are never represented as cryptographically pinned checkpoints.
type GatewayTranslationProvider struct {
	id                string
	alias             string
	serviceBaselineID string
	qualityScore      float64
	costPerUnit       float64
	policy            domain.PolicyState
	endpoint          string
	httpClient        *http.Client
	secretResolver    SecretResolver

	mu                sync.RWMutex
	lastObservedModel string
	lastFingerprint   string
}

var _ interface {
	Provider
	TextTranslationProvider
} = (*GatewayTranslationProvider)(nil)

var (
	// ErrGatewayEndpointRequired is returned when no explicit gateway endpoint is configured,
	// or when attempting to target a forbidden public provider endpoint.
	ErrGatewayEndpointRequired = errors.New("explicit gateway endpoint is required: remote translation lanes are gateway-only with no public provider fallback")

	// ErrServiceBaselineRequired is returned when service_baseline_id is missing or blank.
	ErrServiceBaselineRequired = errors.New("service_baseline_id is required: missing or blank service baseline cannot create reusable remote provenance")
)

// NewGatewayTranslationProvider constructs a production gateway translation provider.
func NewGatewayTranslationProvider(
	id string,
	alias string,
	serviceBaselineID string,
	qualityScore float64,
	opts ...any,
) (*GatewayTranslationProvider, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gateway translation provider requires id")
	}
	if strings.TrimSpace(alias) == "" {
		return nil, fmt.Errorf("gateway translation provider requires gateway alias")
	}
	serviceBaselineID = strings.TrimSpace(serviceBaselineID)
	if serviceBaselineID == "" {
		return nil, fmt.Errorf("%w: provider %q", ErrServiceBaselineRequired, id)
	}
	if qualityScore <= 0 {
		qualityScore = 0.95
	}

	endpoint := os.Getenv("DOUYINIE_GATEWAY_URL")
	if endpoint == "" {
		endpoint = os.Getenv("DOUYINIE_GATEWAY_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = os.Getenv("OPENAI_BASE_URL")
	}
	p := &GatewayTranslationProvider{
		id:                id,
		alias:             alias,
		serviceBaselineID: serviceBaselineID,
		qualityScore:      qualityScore,
		costPerUnit:       0.001,
		policy:            domain.PolicyRequiresExplicitConsent,
		endpoint:          endpoint,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		lastObservedModel: alias,
	}

	for _, opt := range opts {
		switch v := opt.(type) {
		case SecretResolver:
			p.secretResolver = v
		case func(context.Context, string, string) (string, error):
			p.secretResolver = v
		case *http.Client:
			if v != nil {
				p.httpClient = v
			}
		case string:
			if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
				p.endpoint = v
			}
		}
	}
	endpoint = strings.TrimSpace(p.endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: provider %q has no gateway endpoint configured", ErrGatewayEndpointRequired, id)
	}
	if isForbiddenDirectPublicEndpoint(endpoint) {
		return nil, fmt.Errorf("%w: provider %q cannot target direct public provider endpoint (%s); access strictly through local gateway", ErrGatewayEndpointRequired, id, endpoint)
	}
	p.endpoint = endpoint

	return p, nil
}

func (p *GatewayTranslationProvider) ID() string {
	return p.id
}

func (p *GatewayTranslationProvider) Type() ProviderType {
	return TypeTranslation
}

// ModelInfo returns an empty model name so that remote gateway providers
// are never treated as cryptographically pinned local checkpoints.
// The model version carries the gateway alias.
func (p *GatewayTranslationProvider) ModelInfo() (string, string) {
	return "", p.alias
}

func (p *GatewayTranslationProvider) PolicyState() domain.PolicyState {
	return p.policy
}

func (p *GatewayTranslationProvider) SetPolicyState(s domain.PolicyState) {
	p.policy = s
}

func (p *GatewayTranslationProvider) IsHealthy() bool {
	return true
}

func (p *GatewayTranslationProvider) RequiresSnapshot() bool {
	return false
}

func (p *GatewayTranslationProvider) ServiceBaselineID() string {
	return p.serviceBaselineID
}

func (p *GatewayTranslationProvider) ObservedModel() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.lastObservedModel != "" {
		return p.lastObservedModel
	}
	return p.alias
}

func (p *GatewayTranslationProvider) SystemFingerprint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastFingerprint
}

func (p *GatewayTranslationProvider) SetEndpoint(endpoint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpoint = endpoint
}

func (p *GatewayTranslationProvider) SetHTTPClient(client *http.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client != nil {
		p.httpClient = client
	}
}

func (p *GatewayTranslationProvider) SetSecretResolver(resolver SecretResolver) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secretResolver = resolver
}

func (p *GatewayTranslationProvider) Capability() domain.ProviderCapability {
	return domain.ProviderCapability{
		Stage:          string(TypeTranslation),
		Languages:      []string{"vi", "en"},
		ExecutionTier:  "cloud",
		CostPerUnit:    p.costPerUnit,
		QualityScore:   p.qualityScore,
		MaxConcurrency: 4,
		Features:       []string{"meaning_preservation", "gateway_openai_compat"},
	}
}

// TranslateText translates input segments under the meaning-first contract.
// Outbound content is strictly credential-gated; secrets are never logged or stored.
func (p *GatewayTranslationProvider) TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error) {
	if len(req.Segments) == 0 {
		return nil, domain.ErrEmptyTranslationInput
	}

	targetLang := strings.ToLower(strings.TrimSpace(req.TargetLanguage))
	if targetLang == "" {
		targetLang = "vi"
	}
	sourceLang := strings.ToLower(strings.TrimSpace(req.SourceLanguage))
	if sourceLang == "" {
		sourceLang = "zh"
	}

	// 1. Resolve credential (Fail-closed: REQUIRES_AUTHORIZATION)
	secret, err := p.resolveSecret(ctx, req.AuthorizedCredentials)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrAuthRequired, err)
	}

	// 2. Prepare OpenAI-compatible chat completion payload
	promptReq := p.buildChatRequest(sourceLang, targetLang, req.Segments)
	reqBytes, err := json.Marshal(promptReq)
	if err != nil {
		return nil, fmt.Errorf("marshal gateway translation request: %w", err)
	}
	endpointURL := p.resolveEndpointURL()
	if endpointURL == "" || isForbiddenDirectPublicEndpoint(endpointURL) {
		return nil, fmt.Errorf("%w: refusing to call unconfigured or direct public provider endpoint (%s); access strictly through local gateway", ErrGatewayEndpointRequired, endpointURL)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("create gateway request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+secret)

	// 3. Execute HTTP Call
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: gateway request failed: %v", domain.ErrQualityRejected, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: gateway authorization rejected (status %d)", domain.ErrAuthRequired, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w: gateway error (status %d): %s", domain.ErrQualityRejected, resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
	}

	// 4. Parse response
	var chatResp struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("%w: decode gateway response: %v", domain.ErrQualityRejected, err)
	}

	if len(chatResp.Choices) == 0 || strings.TrimSpace(chatResp.Choices[0].Message.Content) == "" {
		return nil, fmt.Errorf("%w: gateway returned empty choice content", domain.ErrQualityRejected)
	}

	// 5. Store provenance
	p.mu.Lock()
	if chatResp.Model != "" {
		p.lastObservedModel = chatResp.Model
	}
	p.lastFingerprint = chatResp.SystemFingerprint
	p.mu.Unlock()

	// 6. Parse translated segments from JSON output
	translatedSegments, err := p.parseTranslationOutput(chatResp.Choices[0].Message.Content, req.Segments)
	if err != nil {
		return nil, err
	}

	observedModel := chatResp.Model
	if observedModel == "" {
		observedModel = p.alias
	}

	return &TranslationResult{
		ProviderID:        p.id,
		ModelName:         "",
		ModelVersion:      p.alias,
		ObservedModel:     observedModel,
		SystemFingerprint: chatResp.SystemFingerprint,
		ServiceBaselineID: p.serviceBaselineID,
		Segments:          translatedSegments,
	}, nil
}

func (p *GatewayTranslationProvider) resolveSecret(ctx context.Context, authRefs []string) (string, error) {
	// Try credential resolver first if provided
	if p.secretResolver != nil && len(authRefs) > 0 {
		for _, ref := range authRefs {
			sec, err := p.secretResolver(ctx, ref, p.id)
			if err == nil && strings.TrimSpace(sec) != "" {
				return strings.TrimSpace(sec), nil
			}
		}
	}

	// Environment variable fallback if matching gateway key is set
	envKeys := []string{
		"DOUYINIE_GATEWAY_API_KEY",
		"OPENAI_API_KEY",
	}
	if strings.Contains(strings.ToLower(p.id), "gemini") {
		envKeys = append([]string{"GEMINI_GATEWAY_API_KEY"}, envKeys...)
	} else if strings.Contains(strings.ToLower(p.id), "deepseek") {
		envKeys = append([]string{"DEEPSEEK_GATEWAY_API_KEY"}, envKeys...)
	}

	for _, k := range envKeys {
		if val := strings.TrimSpace(os.Getenv(k)); val != "" {
			return val, nil
		}
	}

	return "", errors.New("no valid credential resolved for gateway provider")
}

func (p *GatewayTranslationProvider) resolveEndpointURL() string {
	p.mu.RLock()
	ep := strings.TrimSpace(p.endpoint)
	p.mu.RUnlock()

	if ep == "" {
		return ""
	}
	ep = strings.TrimRight(ep, "/")
	if strings.HasSuffix(ep, "/chat/completions") {
		return ep
	}
	return ep + "/chat/completions"
}

func (p *GatewayTranslationProvider) buildChatRequest(sourceLang, targetLang string, segments []domain.TranslationInputSegment) map[string]any {
	systemPrompt := fmt.Sprintf(`You are a meaning-first translation engine for short-form video content.
Translate the input segments from %s to %s.
Strict Invariants:
1. One target language per request (%s).
2. Faithfully translate each segment into natural, idiomatic %s while strictly preserving meaning.
3. Protect and preserve all numbers, digits, quantities, proper names, entities, and negation polarity.
4. Do NOT compress or shorten duration (e.g. no vi_short duration adaptations). Shorten-first adaptation is performed downstream.
5. Respond ONLY with valid JSON conforming to:
{
  "segments": [
    {
      "index": 0,
      "source_text": "...",
      "target_text": "...",
      "key_facts": ["fact1", "fact2"],
      "negation_polarity": false
    }
  ]
}`, sourceLang, targetLang, targetLang, targetLang)

	type inputSeg struct {
		Index      int    `json:"index"`
		SourceText string `json:"source_text"`
	}
	var inputList []inputSeg
	for _, s := range segments {
		inputList = append(inputList, inputSeg{
			Index:      s.Index,
			SourceText: s.SourceText,
		})
	}
	userJSON, _ := json.Marshal(map[string]any{"segments": inputList})

	return map[string]any{
		"model": p.alias,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": string(userJSON)},
		},
		"temperature": 0.0,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}
}

func (p *GatewayTranslationProvider) parseTranslationOutput(content string, originalSegments []domain.TranslationInputSegment) ([]domain.TranslationSegment, error) {
	// Strip optional markdown ```json ... ``` wrapper if present
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) >= 2 {
			if strings.HasPrefix(lines[0], "```") {
				lines = lines[1:]
			}
			if len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "```") {
				lines = lines[:len(lines)-1]
			}
			content = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}

	var parsed struct {
		Segments []struct {
			Index            int      `json:"index"`
			SourceText       string   `json:"source_text"`
			TargetText       string   `json:"target_text"`
			KeyFacts         []string `json:"key_facts"`
			NegationPolarity bool     `json:"negation_polarity"`
		} `json:"segments"`
	}

	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("%w: failed to parse translation JSON content: %v", domain.ErrQualityRejected, err)
	}

	if len(parsed.Segments) == 0 {
		return nil, fmt.Errorf("%w: gateway returned 0 translated segments", domain.ErrQualityRejected)
	}

	resultMap := make(map[int]domain.TranslationSegment)
	for _, ps := range parsed.Segments {
		resultMap[ps.Index] = domain.TranslationSegment{
			Index:            ps.Index,
			SourceText:       ps.SourceText,
			TargetText:       ps.TargetText,
			KeyFacts:         ps.KeyFacts,
			NegationPolarity: ps.NegationPolarity,
		}
	}

	var out []domain.TranslationSegment
	for _, orig := range originalSegments {
		res, ok := resultMap[orig.Index]
		if !ok || strings.TrimSpace(res.TargetText) == "" {
			return nil, fmt.Errorf("%w: missing translation for segment index %d", domain.ErrQualityRejected, orig.Index)
		}
		if res.SourceText == "" {
			res.SourceText = orig.SourceText
		}
		res.SpeakerID = orig.SpeakerID
		res.StartMs = orig.StartMs
		res.EndMs = orig.EndMs
		out = append(out, res)
	}

	return out, nil
}

func isForbiddenDirectPublicEndpoint(endpoint string) bool {
	lower := strings.ToLower(endpoint)
	forbidden := []string{
		"api.openai.com",
		"api.deepseek.com",
		"generativelanguage.googleapis.com",
		"ai.google.dev",
	}
	for _, f := range forbidden {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}
