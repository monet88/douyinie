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
// Note: GatewayDeepSeekTranslationProviderID was updated from "gateway_deepseek_v4_flash_vision_exp"
// to "gateway_deepseek_v4_1_flash" in fa182e7 when upstream decommissioned the v4-flash-vision-exp
// endpoint. Because CredentialService.ValidateCredentialRef enforces exact case-folded provider ID
// equality (ref.ProviderID != "" && !strings.EqualFold(ref.ProviderID, providerID)), any existing
// CredentialRef bound to the previous ID fails authorization until re-registered.
const (
	GatewayGeminiTranslationProviderID   = "gateway_gemini_3_8_flash"
	GatewayDeepSeekTranslationProviderID = "gateway_deepseek_v4_1_flash"
	WorkerQwenTranslationProviderID      = "qwen3_4b_translation"
)

// Gateway model aliases — the single switch for a production model rename.
// Aliases are whatever the configured gateway actually serves (cliproxy serves
// `deepseek-v4.1-flash` bare, with no `deepseek/` prefix); they are recorded as
// provenance at runtime, never pinned as checkpoint hashes.
// For alias-only updates: updating Gateway*ModelAlias leaves provider identities
// and registered credentials intact.
// Renaming an upstream provider ID means: these constants, the wire-assertion literals pinned in
// internal/provider/gateway_translation_test.go and test/seam1/translation_routing_seam1_test.go,
// the model-route lines in AGENTS.md / CONTEXT.md / PRODUCT.md / docs/architecture, the
// operator's DOUYINIE_SERVICE_BASELINE_* value, and re-registering any persisted CredentialRef
// bound to the old provider ID (otherwise ValidateCredentialRef fails closed with ErrAuthRequired).
const (
	GatewayGeminiModelAlias   = "gemini-3.8-flash"
	GatewayDeepSeekModelAlias = "deepseek-v4.1-flash"
)

// TextTranslationProvider is implemented by translation model/service adapters
// that translate source text segments into Vietnamese or English while preserving meaning.
type TextTranslationProvider interface {
	TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error)
}
