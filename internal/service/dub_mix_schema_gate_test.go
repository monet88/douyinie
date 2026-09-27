package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

// TestFreezeRenderPlan_DubMixSchemaGate locks the boundary that stops a legacy DubMix artifact
// (SchemaVersion 1) from supplying PASS evidence to rendering under the current playback/coverage
// acceptance contract (SchemaVersion 2). A legacy artifact stays readable as history, but it must be
// re-derived before it can be rendered: otherwise a pre-contract PASS would silently bypass the
// current acceptance rules.
func TestFreezeRenderPlan_DubMixSchemaGate(t *testing.T) {
	_, db, casStore, renderSvc, assetID := setupFullReviewHarnessWithRender(t)
	ctx := context.Background()

	preflight, err := db.GetPreflightReport(ctx, assetID)
	if err != nil {
		t.Fatalf("get preflight report: %v", err)
	}

	cues := []domain.SubtitleCue{{StartMs: 0, EndMs: 1000, Text: "Phụ đề", X: 100, Y: 200, Width: 400, Height: 80}}

	freeze := func(t *testing.T, schemaVersion int) error {
		t.Helper()
		artifact := domain.DubMixArtifact{
			ID:                  "dubmix-schema-" + time.Now().UTC().Format(time.RFC3339Nano),
			SchemaVersion:       schemaVersion,
			AssetID:             assetID,
			RunID:               "run-dubmix-schema-gate",
			TargetLanguage:      "vi",
			AudioCASHash:        preflight.NormalizedAudioSHA256,
			AudioCASPath:        preflight.NormalizedAudioCASPath,
			SampleRate:          16000,
			Channels:            1,
			Format:              "wav",
			DurationMs:          preflight.DurationMs,
			DialogueSuppressed:  true,
			SoundtrackPreserved: true,
			OverallStatus:       "PASS",
			CreatedAt:           time.Now().UTC(),
		}
		blob, err := json.Marshal(artifact)
		if err != nil {
			t.Fatalf("marshal dub mix: %v", err)
		}
		obj, err := casStore.Put(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("put dub mix: %v", err)
		}
		_, err = renderSvc.FreezeRenderPlan(ctx, service.RenderPlanInput{
			RunID:          "run-dubmix-schema-gate",
			AssetID:        assetID,
			TargetLanguage: "vi",
			DubMixCAS:      obj.SHA256,
			SubtitleCues:   cues,
		})
		return err
	}

	t.Run("legacy schema is refused", func(t *testing.T) {
		err := freeze(t, 1)
		if err == nil {
			t.Fatal("expected a legacy-schema dub mix to be refused, got nil error")
		}
		if !errors.Is(err, domain.ErrDubMixSchemaStale) {
			t.Fatalf("expected ErrDubMixSchemaStale, got %v", err)
		}
		if !errors.Is(err, domain.ErrDubMixNotRenderable) {
			t.Fatalf("expected the refusal to be classified as ErrDubMixNotRenderable, got %v", err)
		}
	})

	t.Run("current schema is still renderable", func(t *testing.T) {
		if err := freeze(t, domain.DubMixSchemaVersion); err != nil {
			t.Fatalf("current-schema dub mix must freeze a render plan, got %v", err)
		}
	})
}
