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
	GatewayDeepSeekTranslationProviderID = "gateway_deepseek_v4_flash_vision_exp"
	WorkerQwenTranslationProviderID      = "qwen3_4b_translation"
)
// TextTranslationProvider is implemented by translation model/service adapters
// that translate source text segments into Vietnamese or English while preserving meaning.
type TextTranslationProvider interface {
	TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error)
}
