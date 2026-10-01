package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #94 review findings on the whole-speaker escalation path: an unreadable current
// assignment must fail safe, and an assignment scoped differently from the one the plan
// would mint must not be reused as that escalation.

func TestCurrentRunAssignmentFailsSafeOnUnreadableCurrentAssignment(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	casStore, err := cas.NewStore(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("open CAS store: %v", err)
	}
	svc := &DubbingService{db: db, cas: casStore}

	ctx := context.Background()
	in := domain.DubbingJobInput{AssetID: "asset-1", RunID: "run-1", TargetLanguage: "vi"}
	base := &domain.VoiceAssignment{CASHash: "sha256:base"}

	// No assignment stored for the run is not a move: the caller may mint the escalation.
	if current, movedOn := svc.currentRunAssignment(ctx, in, base); current != nil || movedOn {
		t.Fatalf("an absent current assignment must report no move, got current=%+v movedOn=%v", current, movedOn)
	}

	// A read failure is not "no assignment": report the move so the caller refuses to
	// supersede an assignment it could not read.
	if err := db.Close(); err != nil {
		t.Fatalf("close storage: %v", err)
	}
	if current, movedOn := svc.currentRunAssignment(ctx, in, base); current != nil || !movedOn {
		t.Fatalf("an unreadable current assignment must fail safe as moved, got current=%+v movedOn=%v", current, movedOn)
	}
}

func TestIsEscalationOfBaseRejectsDifferentSharedVoiceScope(t *testing.T) {
	fallback := domain.VoiceProfile{ProviderID: provider.CosyVoiceProviderID, VoiceID: "cosy-fallback"}
	base := &domain.VoiceAssignment{
		CASHash: "sha256:base",
		Assignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": {ProviderID: provider.ZeroTTSProviderID, VoiceID: "quangminh"},
			"SPEAKER_01": {ProviderID: provider.ZeroTTSProviderID, VoiceID: "maichi"},
		},
	}
	plan := &speakerEscalationPlan{
		target:      fallback,
		assignments: map[string]domain.VoiceProfile{"SPEAKER_00": fallback},
	}
	current := &domain.VoiceAssignment{
		CASHash:       "sha256:escalated",
		SupersedesCAS: base.CASHash,
		Assignments: map[string]domain.VoiceProfile{
			"SPEAKER_00": fallback,
			"SPEAKER_01": base.Assignments["SPEAKER_01"],
		},
	}

	if !isEscalationOfBase(current, base, plan) {
		t.Fatal("the escalation the plan would mint from base must be reusable on a retry")
	}
	// Same supersession and same profiles, but the run now pins one voice for every
	// speaker: that is not the assignment this plan would mint from base.
	current.UseSameVoiceForAll = true
	if isEscalationOfBase(current, base, plan) {
		t.Fatal("an assignment with a different shared-voice scope must not be reused as this escalation")
	}
}

// Issue #155 review finding: the FitController's recorded DURATION_OVERRUN verdict - not a
// measured-vs-slot millisecond comparison - is what makes a fixed-rate speaker's timing
// failure unresolved. A grouped candidate can fill its accepted playback window in floored
// milliseconds while its resampled frames do not, so an ms-only predicate would report
// Resolved=true for a failure the duration-controlled fallback never cleared. An unrelated
// review reason is not a timing failure and must not keep an escalation unresolved.
func TestResolveEscalationOutcomesKeepsFrameExactOverrunUnresolved(t *testing.T) {
	escalations := []domain.VoiceProviderEscalation{{SpeakerID: "SPEAKER_00", Reason: domain.VoiceEscalationReasonFixedRateOverrun}}
	frameExact := domain.DubSegmentReview{
		Index: 0, SpeechBlockIndices: []int{0, 1}, SpeakerID: "SPEAKER_00",
		// The probe fills the slot exactly while the frames placed inside the window exceed it.
		SlotDurationMs: 2005, MeasuredDurationMs: 2005,
		FitDecision: domain.FitActionReview, ReviewReason: "DURATION_OVERRUN",
	}

	resolved := resolveEscalationOutcomes(escalations, []domain.DubSegmentReview{frameExact})
	if len(resolved) != 1 || resolved[0].Resolved {
		t.Fatalf("a frame-exact overrun the fallback did not clear must stay unresolved, got %+v", resolved)
	}

	// A cleared speaker keeps its resolved outcome when no timing failure is left.
	if cleared := resolveEscalationOutcomes(escalations, nil); len(cleared) != 1 || !cleared[0].Resolved {
		t.Fatalf("a speaker with no unresolved timing failure must stay resolved, got %+v", cleared)
	}

	// A review the FitController did not classify as a duration overrun is not a timing
	// failure, even when its measurement exceeds the slot.
	unrelated := frameExact
	unrelated.ReviewReason = "MISSING_REPLACEMENT"
	unrelated.MeasuredDurationMs = 4000
	if other := resolveEscalationOutcomes(escalations, []domain.DubSegmentReview{unrelated}); !other[0].Resolved {
		t.Fatalf("an unrelated review reason must not resolve as an unresolved timing failure, got %+v", other)
	}
}
