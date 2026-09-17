package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
)

// TranslationRequest represents the input to a translation provider.
type TranslationRequest struct {
	RunID                 string
	SourceLanguage        string
	TargetLanguage        string
	Segments              []domain.TranslationInputSegment
	AuthorizedCredentials []string
}

// TranslationResult represents the output from a translation provider.
type TranslationResult struct {
	ProviderID        string
	ModelName         string
	ModelVersion      string
	ObservedModel     string
	SystemFingerprint string
	ServiceBaselineID string
	Segments          []domain.TranslationSegment
}

// Frozen RC translation provider identities (Issue #66).
const (
	GatewayGeminiTranslationProviderID   = "gateway_gemini_3_8_flash"
	GatewayDeepSeekTranslationProviderID = "gateway_deepseek_v4_1_flash"
	WorkerQwenTranslationProviderID      = "qwen3_4b_translation"
)

// Gateway model aliases — the single switch for a production model rename.
// Aliases are whatever the configured gateway actually serves (cliproxy serves
// `deepseek-v4.1-flash` bare, with no `deepseek/` prefix); they are recorded as
// provenance at runtime, never pinned as checkpoint hashes.
// Renaming upstream means: these constants, the wire-assertion literals pinned in
// internal/provider/gateway_translation_test.go and test/seam1/translation_routing_seam1_test.go,
// the model-route lines in AGENTS.md / CONTEXT.md / PRODUCT.md / docs/architecture, and the
// operator's DOUYINIE_SERVICE_BASELINE_* value.
const (
	GatewayGeminiModelAlias   = "gemini-3.8-flash"
	GatewayDeepSeekModelAlias = "deepseek-v4.1-flash"
)

// TextTranslationProvider is implemented by translation model/service adapters
// that translate source text segments into Vietnamese or English while preserving meaning.
type TextTranslationProvider interface {
	TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error)
}
