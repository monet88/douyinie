package benchmark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestRuntimeHostClient_ProfileSensitiveStagesBindRouteOptions(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		Path string
		Body map[string]any
	}
	observed := make(chan observedRequest, 8)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode %s request body: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		observed <- observedRequest{Path: r.URL.Path, Body: body}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/assets/asset-1/translate":
			_, _ = w.Write([]byte(`{"translation_variant":{}}`))
		case "/api/v1/assets/asset-1/dub-script":
			_, _ = w.Write([]byte(`{"dub_script_variant":{}}`))
		case "/api/v1/assets/asset-1/voice-assignment":
			_, _ = w.Write([]byte(`{"voice_assignment":{}}`))
		case "/api/v1/assets/asset-1/dub-synthesize":
			_, _ = w.Write([]byte(`{"dub_segments_variant":{}}`))
		case "/api/v1/assets/asset-1/separate-stems":
			_, _ = w.Write([]byte(`{"audio_stems":{}}`))
		case "/api/v1/assets/asset-1/audio-mix":
			_, _ = w.Write([]byte(`{"dub_mix":{}}`))
		case "/api/v1/assets/asset-1/detect-text":
			_, _ = w.Write([]byte(`{"text_region_plan":{}}`))
		case "/api/v1/assets/asset-1/visual-track":
			_, _ = w.Write([]byte(`{"localized_visual_track":{},"localized_subtitle_track":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	client := NewRuntimeHostClient(ts.URL, ts.Client())
	opts := StageRouteOptions{
		ExecutionProfile:      domain.ExecutionProfileHybrid,
		ConsentGranted:        true,
		AuthorizedCredentials: []string{"cred_gateway"},
	}
	segments := []domain.TranslationInputSegment{{Index: 0, SourceText: "你好", StartMs: 0, EndMs: 1000}}

	ctx := context.Background()
	if _, err := client.RunTranslation(ctx, "asset-1", "run-1", "job-1", "vi", segments, opts); err != nil {
		t.Fatalf("RunTranslation: %v", err)
	}
	if _, err := client.RunDubScript(ctx, "asset-1", "run-1", "job-1", "vi", "trans-cas", segments, opts); err != nil {
		t.Fatalf("RunDubScript: %v", err)
	}
	if _, err := client.AssignVoices(ctx, "asset-1", "run-1", "job-1", "vi", opts); err != nil {
		t.Fatalf("AssignVoices: %v", err)
	}
	if _, err := client.RunDubSynthesize(ctx, "asset-1", "run-1", "job-1", "vi", "voice-cas", "dub-script-cas", opts); err != nil {
		t.Fatalf("RunDubSynthesize: %v", err)
	}
	if _, err := client.SeparateStems(ctx, "asset-1", "run-1", opts); err != nil {
		t.Fatalf("SeparateStems: %v", err)
	}
	if _, err := client.RunAudioMix(ctx, "asset-1", "run-1", "job-1", "vi", "dub-cas", "stems-cas", opts); err != nil {
		t.Fatalf("RunAudioMix: %v", err)
	}
	if _, err := client.DetectText(ctx, "asset-1", "run-1", opts); err != nil {
		t.Fatalf("DetectText: %v", err)
	}
	if _, _, err := client.LocalizeVisualTrack(ctx, "asset-1", "run-1", "job-1", "vi", "text-cas", "trans-cas", opts); err != nil {
		t.Fatalf("LocalizeVisualTrack: %v", err)
	}

	for i := 0; i < 8; i++ {
		req := <-observed
		if got := req.Body["execution_profile"]; got != string(domain.ExecutionProfileHybrid) {
			t.Errorf("%s execution_profile = %v, want hybrid", req.Path, got)
		}
		if req.Path == "/api/v1/assets/asset-1/translate" {
			if got := req.Body["consent_granted"]; got != true {
				t.Errorf("translation consent_granted = %v, want true", got)
			}
			creds, ok := req.Body["authorized_credentials"].([]any)
			if !ok || len(creds) != 1 || creds[0] != "cred_gateway" {
				t.Errorf("translation authorized_credentials = %#v, want [cred_gateway]", req.Body["authorized_credentials"])
			}
		}
		if req.Path == "/api/v1/assets/asset-1/visual-track" {
			if got := req.Body["consent_granted"]; got != true {
				t.Errorf("visual-track consent_granted = %v, want true", got)
			}
			creds, ok := req.Body["authorized_credentials"].([]any)
			if !ok || len(creds) != 1 || creds[0] != "cred_gateway" {
				t.Errorf("visual-track authorized_credentials = %#v, want [cred_gateway]", req.Body["authorized_credentials"])
			}
		}
	}
}
