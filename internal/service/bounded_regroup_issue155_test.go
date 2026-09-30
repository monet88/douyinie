package service

import (
	"context"
	"errors"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

// Issue #155/R4: the bounded same-speaker group is resolved from the pinned canonical
// SpeechBlocks/AudioRolePlan, not from the filtered dub script. Routing refuses a run whose
// plan carries uncertain audio before the synthesis pass runs, so the uncertain-vocal
// boundary the selector must honour is only reachable here, at the selector itself.
func TestBoundedRegroupMembers_Issue155CanonicalAndProtectedBoundaries(t *testing.T) {
	scriptTurn := func(index int, startMs, endMs int64) domain.DubScriptSegment {
		return domain.DubScriptSegment{
			Index: index, SpeakerID: "SPEAKER_00", StartMs: startMs, EndMs: endMs,
			SourceText: "câu", SpokenText: "câu",
		}
	}
	speechBlock := func(index int, startMs, endMs int64) domain.SpeechBlock {
		return domain.SpeechBlock{
			Index: index, SegmentType: domain.SpeechBlockTypeSpeech,
			StartMs: startMs, EndMs: endMs, SpeakerID: "SPEAKER_00",
		}
	}

	const (
		firstEndMs     = 1000
		secondStartMs  = 1400
		secondEndMs    = 2400
		thirdStartMs   = 2800
		thirdEndMs     = 3800
		protectedStart = 1000
		protectedEnd   = 1400
		middleStartMs  = 1100
		middleEndMs    = 1300
	)

	cases := []struct {
		name string
		// gapRole is the audio role the pinned plan carries inside the gap.
		gapRole domain.AudioRole
		// middle pins an extra canonical speech block inside the gap, invisible to the dub
		// script because a dub-ineligible turn is filtered out of it.
		middle bool
		// divergent makes the script declare timing the canonical block does not have.
		divergent bool
		want      int
	}{
		{name: "DialogueGapMerges", gapRole: domain.AudioRoleNarrationDialogue, want: 3},
		{name: "BgmGapDoesNotBlock", gapRole: domain.AudioRoleInstrumentalBgm, want: 3},
		{name: "SfxGapDoesNotBlock", gapRole: domain.AudioRoleAmbienceSFX, want: 3},
		{name: "SingingVocalGapStops", gapRole: domain.AudioRoleSingingMusicVocal, want: 1},
		{name: "UncertainVocalGapStops", gapRole: domain.AudioRoleUncertain, want: 1},
		{name: "InterveningCanonicalBlockStops", gapRole: domain.AudioRoleInstrumentalBgm, middle: true, want: 1},
		{name: "DivergentScriptTimingStops", gapRole: domain.AudioRoleNarrationDialogue, divergent: true, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []domain.SpeechBlock{
				speechBlock(0, 0, firstEndMs),
				speechBlock(1, secondStartMs, secondEndMs),
				speechBlock(2, thirdStartMs, thirdEndMs),
			}
			if tc.middle {
				blocks = append(blocks, speechBlock(9, middleStartMs, middleEndMs))
			}
			transcript := &domain.TranscriptArtifact{SpeechBlocks: blocks}

			script := &domain.DubScriptVariant{}
			script.Segments = append(script.Segments, scriptTurn(0, 0, firstEndMs))
			if tc.divergent {
				script.Segments = append(script.Segments, scriptTurn(1, secondStartMs+50, secondEndMs))
			} else {
				script.Segments = append(script.Segments, scriptTurn(1, secondStartMs, secondEndMs))
			}
			script.Segments = append(script.Segments, scriptTurn(2, thirdStartMs, thirdEndMs))

			rolePlan := &domain.AudioRolePlan{Segments: []domain.AudioSegment{
				{StartMs: protectedStart, EndMs: protectedEnd, Role: tc.gapRole},
				{StartMs: protectedEnd, EndMs: thirdStartMs, Role: tc.gapRole},
			}}

			members := boundedRegroupMembers(script, transcript, rolePlan, 0, "SPEAKER_00")
			if len(members) != tc.want {
				t.Fatalf("expected %d canonical members, got %d (%+v)", tc.want, len(members), members)
			}
			for pos, member := range members {
				if member.Index != script.Segments[pos].Index {
					t.Fatalf("member %d must keep its canonical script position, got %+v", pos, members)
				}
			}
		})
	}
}

// The group never exceeds five canonical members even when every later turn is eligible.
func TestBoundedRegroupMembers_Issue155CapsAtFiveMembers(t *testing.T) {
	script := &domain.DubScriptVariant{}
	transcript := &domain.TranscriptArtifact{}
	for i := range 7 {
		start := int64(i) * 1100
		end := start + 1000
		script.Segments = append(script.Segments, domain.DubScriptSegment{
			Index: i, SpeakerID: "SPEAKER_00", StartMs: start, EndMs: end, SourceText: "câu", SpokenText: "câu",
		})
		transcript.SpeechBlocks = append(transcript.SpeechBlocks, domain.SpeechBlock{
			Index: i, SegmentType: domain.SpeechBlockTypeSpeech, StartMs: start, EndMs: end, SpeakerID: "SPEAKER_00",
		})
	}
	members := boundedRegroupMembers(script, transcript, &domain.AudioRolePlan{}, 0, "SPEAKER_00")
	if len(members) != 5 {
		t.Fatalf("expected the group to stop at 5 members, got %d", len(members))
	}
}

// A provider reporting success without a result must fail the pass closed at the boundary that
// owns provider invocation: every caller probes result.AudioData, so (nil, nil) returned as
// success would panic at the probe instead of failing the run.
func TestInvokeTTSWithFallback_NilResultFailsClosed(t *testing.T) {
	svc := &DubbingService{TTSInvoke: func(context.Context, provider.Provider, provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		return nil, nil
	}}
	res, _, err := svc.invokeTTSWithFallback(context.Background(), provider.TTSSynthesisRequest{}, domain.ExecutionProfileHybrid, nil)
	if err == nil {
		t.Fatalf("a nil synthesis result must fail closed, got result %+v", res)
	}
	if !errors.Is(err, errEmptyTTSResult) {
		t.Fatalf("expected the fail-closed nil-result error, got %v", err)
	}
}
