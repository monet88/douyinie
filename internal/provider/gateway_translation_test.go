package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

func TestGatewayTranslationProvider_RequiresAuth_FailsClosedWithoutSecret(t *testing.T) {
	ctx := context.Background()
	p, err := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	// Invoking without authorized credentials must fail closed with ErrAuthRequired
	req := provider.TranslationRequest{
		RunID:          "run-1",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	}
	_, err = p.TranslateText(ctx, req)
	if !errors.Is(err, domain.ErrAuthRequired) {
		t.Fatalf("expected ErrAuthRequired, got: %v", err)
	}
}

func TestGatewayTranslationProvider_RemoteModelNeverRepresentedAsPinnedSnapshot(t *testing.T) {
	p, err := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	mName, mVer := p.ModelInfo()
	if mName != "" {
		t.Fatalf("remote gateway provider must return empty model name, got %q", mName)
	}
	if mVer != "gemini-3.8-flash" {
		t.Fatalf("model version must match gateway alias, got %q", mVer)
	}
	if p.RequiresSnapshot() {
		t.Fatalf("remote gateway provider must not require cryptographic snapshot verification")
	}
	if p.ServiceBaselineID() != "baseline-gemini-3.8-flash-2026-08" {
		t.Fatalf("unexpected service baseline id: %q", p.ServiceBaselineID())
	}
}

func TestGatewayTranslationProvider_NoConfiguredGateway_CannotHitOpenAI(t *testing.T) {
	// 1. Missing endpoint must fail with ErrGatewayEndpointRequired
	_, err := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
	)
	if !errors.Is(err, provider.ErrGatewayEndpointRequired) {
		t.Fatalf("expected ErrGatewayEndpointRequired when no gateway configured, got: %v", err)
	}

	// 2. Direct public endpoints must be rejected fail-closed (access strictly through local gateway)
	forbiddenEndpoints := []string{
		"https://api.openai.com/v1",
		"https://api.deepseek.com/v1",
		"https://generativelanguage.googleapis.com/v1beta",
		"https://ai.google.dev/api",
	}
	for _, ep := range forbiddenEndpoints {
		_, err = provider.NewGatewayTranslationProvider(
			"gateway_gemini_3_8_flash",
			"gemini-3.8-flash",
			"baseline-gemini-3.8-flash-2026-08",
			0.99,
			ep,
		)
		if !errors.Is(err, provider.ErrGatewayEndpointRequired) {
			t.Fatalf("expected ErrGatewayEndpointRequired for direct public endpoint %q, got: %v", ep, err)
		}
	}
}

func TestGatewayTranslationProvider_MissingOrBlankBaseline_FailsClosed(t *testing.T) {
	for _, blankBaseline := range []string{"", "   ", "\t\n"} {
		_, err := provider.NewGatewayTranslationProvider(
			"gateway_gemini_3_8_flash",
			"gemini-3.8-flash",
			blankBaseline,
			0.99,
			"http://127.0.0.1:8080",
		)
		if !errors.Is(err, provider.ErrServiceBaselineRequired) {
			t.Fatalf("expected ErrServiceBaselineRequired for blank baseline %q, got: %v", blankBaseline, err)
		}
	}
}

func TestGatewayTranslationProvider_SuccessfulTranslationAndProvenance(t *testing.T) {
	ctx := context.Background()

	// Mock OpenAI-compatible gateway server
	var capturedAuth string
	var capturedModel string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		capturedModel = body.Model

		resp := map[string]any{
			"id":                 "chatcmpl-test-123",
			"model":              "gemini-3.8-flash-001",
			"system_fingerprint": "fp_gemini_20260819",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role": "assistant",
						"content": `{"segments": [
							{
								"index": 0,
								"source_text": "SUPOR 500ml",
								"target_text": "Nồi SUPOR 500ml",
								"key_facts": ["SUPOR", "500"],
								"negation_polarity": false
							}
						]}`,
					},
					"finish_reason": "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	secretResolver := func(ctx context.Context, authRef, providerID string) (string, error) {
		if authRef == "cred_ref_gemini" {
			return "test-gateway-token-gemini-123", nil
		}
		return "", errors.New("unknown credential")
	}

	p, err := provider.NewGatewayTranslationProvider(
		"gateway_gemini_3_8_flash",
		"gemini-3.8-flash",
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		secretResolver,
		ts.Client(),
		ts.URL,
	)
	if err != nil {
		t.Fatalf("NewGatewayTranslationProvider: %v", err)
	}

	req := provider.TranslationRequest{
		RunID:                 "run-1",
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		AuthorizedCredentials: []string{"cred_ref_gemini"},
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "SUPOR 500ml"},
		},
	}

	res, err := p.TranslateText(ctx, req)
	if err != nil {
		t.Fatalf("TranslateText failed: %v", err)
	}

	if capturedAuth != "Bearer test-gateway-token-gemini-123" {
		t.Fatalf("expected Bearer token, got: %q", capturedAuth)
	}
	if capturedModel != "gemini-3.8-flash" {
		t.Fatalf("expected request model alias 'gemini-3.8-flash', got %q", capturedModel)
	}
	if res.ObservedModel != "gemini-3.8-flash-001" {
		t.Fatalf("expected observed model 'gemini-3.8-flash-001', got %q", res.ObservedModel)
	}
	if res.SystemFingerprint != "fp_gemini_20260819" {
		t.Fatalf("expected fingerprint 'fp_gemini_20260819', got %q", res.SystemFingerprint)
	}
	if res.ServiceBaselineID != "baseline-gemini-3.8-flash-2026-08" {
		t.Fatalf("expected baseline 'baseline-gemini-3.8-flash-2026-08', got %q", res.ServiceBaselineID)
	}
	if len(res.Segments) != 1 || res.Segments[0].TargetText != "Nồi SUPOR 500ml" {
		t.Fatalf("unexpected translated segments: %+v", res.Segments)
	}
}
