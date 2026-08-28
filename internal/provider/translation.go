package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
)

// TranslationRequest represents the input to a translation provider.
type TranslationRequest struct {
	RunID          string
	SourceLanguage string
	TargetLanguage string
	Segments       []domain.TranslationInputSegment
}

// TranslationResult represents the output from a translation provider.
type TranslationResult struct {
	ProviderID   string
	ModelName    string
	ModelVersion string
	Segments     []domain.TranslationSegment
}

// TextTranslationProvider is implemented by translation model/service adapters
// that translate source text segments into Vietnamese or English while preserving meaning.
type TextTranslationProvider interface {
	TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error)
}
