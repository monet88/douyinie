package service

import (
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

// TestDubVariantStatusRequiresExactEligibleCoverage pins the gate that authorizes a delivery
// rebuild: a variant may be PASS only when every dub-eligible canonical block of the run's pinned
// source is covered by exactly one accepted segment. The weaker "every unit is accepted and none
// is left in review" test this replaced would authorize a rebuild for a variant that silently
// dropped an eligible block, so an acceptance could publish an unearned PASS and only fail later
// inside the mix, instead of leaving the run in review (Issue #157).
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
