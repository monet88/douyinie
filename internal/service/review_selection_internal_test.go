package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// TestDubVariantStatusRequiresExactEligibleCoverage pins the gate that authorizes a delivery
// rebuild: a variant may be PASS only when every dub-eligible canonical block of the run's pinned
// source is covered by exactly one accepted segment. The weaker predicate "every unit is accepted
// and none is left in review" would authorize a rebuild for a variant that silently dropped an
// eligible block, so an acceptance could publish an unearned PASS and only fail later inside the
// mix, instead of leaving the run in review.
func TestDubVariantStatusRequiresExactEligibleCoverage(t *testing.T) {
	eligible := map[int]domain.SpeechBlock{
		0: {Index: 0},
		1: {Index: 1},
	}
	accepted := func(index int, members ...int) domain.DubSegment {
		return domain.DubSegment{Index: index, SpeechBlockIndices: members, FitDecision: domain.FitActionAccept}
	}

	cases := []struct {
		name     string
		variant  *domain.DubSegmentsVariant
		eligible map[int]domain.SpeechBlock
		want     string
	}{
		{
			name:     "nil variant never passes",
			variant:  nil,
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name:     "a variant with no accepted segment never passes",
			variant:  &domain.DubSegmentsVariant{},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name:     "a source with no dub-eligible block never passes",
			variant:  &domain.DubSegmentsVariant{Segments: []domain.DubSegment{accepted(0, 0)}},
			eligible: map[int]domain.SpeechBlock{},
			want:     "REVIEW_REQUIRED",
		},
		{
			name: "an unresolved review unit never passes",
			variant: &domain.DubSegmentsVariant{
				Segments:       []domain.DubSegment{accepted(0, 0, 1)},
				ReviewSegments: []domain.DubSegmentReview{{Index: 1}},
			},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name: "a segment not accepted by its own fit never passes",
			variant: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{
					accepted(0, 0),
					{Index: 1, SpeechBlockIndices: []int{1}, FitDecision: domain.FitActionReview},
				},
			},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name: "an accepted segment still flagged for review never passes",
			variant: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{
					accepted(0, 0),
					{Index: 1, SpeechBlockIndices: []int{1}, FitDecision: domain.FitActionAccept, RequiresReview: true},
				},
			},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name:     "an eligible block no segment covers never passes",
			variant:  &domain.DubSegmentsVariant{Segments: []domain.DubSegment{accepted(0, 0)}},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name: "a member outside the eligible source never passes",
			variant: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{accepted(0, 0), accepted(1, 5)},
			},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name: "two segments covering one eligible block never pass",
			variant: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{accepted(0, 0, 1), accepted(1, 1)},
			},
			eligible: eligible,
			want:     "REVIEW_REQUIRED",
		},
		{
			name: "exact coverage by accepted segments passes",
			variant: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{accepted(0, 0), accepted(1, 1)},
			},
			eligible: eligible,
			want:     "PASS",
		},
		{
			name: "one accepted group covering every eligible block passes",
			variant: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{accepted(0, 0, 1)},
			},
			eligible: eligible,
			want:     "PASS",
		},
	}

	for _, tc := range cases {
		if got := dubVariantStatus(tc.variant, tc.eligible); got != tc.want {
			t.Errorf("%s: dubVariantStatus = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestInvalidateCorrectionStageStaysAppendOnly pins the invalidation contract every caller depends on:
// each call records its own queued row. A writer that skipped the row because the stage already held a
// queued one would silently drop a correction's invalidation whenever a pipeline pass had already
// queued that stage, and the pass would then promote the row it had read and publish an artifact the
// correction superseded. The pass is safe against that because executeRun re-reads the run's stage rows
// when ReconcileDeliveryLineage reports a superseded delivery, and rebuilds each stage on the row the
// reconcile made newest.
func TestInvalidateCorrectionStageStaysAppendOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}
	db, err := storage.Open(filepath.Join(dir, "stage_rows.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const assetID, jobID, runID = "asset-stage-rows", "job-stage-rows", "run-stage-rows"
	seedTempoUnitLineage(t, db, assetID, jobID, runID)
	svc := NewReviewService(db, casStore)

	for round := 1; round <= 2; round++ {
		if err := svc.invalidateCorrectionStage(ctx, runID, "render_preview"); err != nil {
			t.Fatalf("invalidation %d: %v", round, err)
		}
		rows, err := db.ListStageExecutions(ctx, runID)
		if err != nil {
			t.Fatalf("read run stage rows: %v", err)
		}
		queued := 0
		for _, se := range rows {
			if se.Stage == "render_preview" && se.Status == domain.StageStatusQueued {
				queued++
			}
		}
		if queued != round {
			t.Fatalf("invalidation %d must append its own queued row, got %d: %+v", round, queued, rows)
		}
	}
	// A reader resolves the newest row, which is the invalidation: the stage holds no artifact.
	if hash, err := db.GetStageArtifactHash(ctx, runID, "render_preview"); err != nil || hash != "" {
		t.Fatalf("expected an invalidated stage to resolve no artifact, got %q (err %v)", hash, err)
	}
}
