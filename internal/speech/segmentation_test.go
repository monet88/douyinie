package speech

import (
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

// buildWords builds a word-timing list with the given per-word durations and
// inter-word gaps (gap is the pause AFTER each word, before the next).
func buildWords(t *testing.T, words []string, startMs, endMs, gapMs int64) []domain.WordTiming {
	t.Helper()
	out := make([]domain.WordTiming, len(words))
	cur := startMs
	for i, w := range words {
		out[i] = domain.WordTiming{Word: w, StartMs: cur, EndMs: cur + endMs, Confidence: 0.95}
		cur += endMs + gapMs
	}
	return out
}

func TestBuildSpeechBlocks_MinSpeechBlockMsEnforced(t *testing.T) {
	// A short sentence "好。" (100ms) followed by a long sentence "今天是星期一我们去公园散步"
	// (700ms). The punctuation split after "好。" would create a 100ms block,
	// which is < MinSpeechBlockMs (500ms). The gate must suppress this split.
	// The combined stream is 800ms >= 500ms, so one block is produced.
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
	words := []domain.WordTiming{
		{Word: "好。", StartMs: 0, EndMs: 100, Confidence: 0.95},
		{Word: "今天", StartMs: 120, EndMs: 300, Confidence: 0.95},
		{Word: "是", StartMs: 320, EndMs: 450, Confidence: 0.95},
		{Word: "星期一", StartMs: 470, EndMs: 600, Confidence: 0.95},
		{Word: "我们", StartMs: 620, EndMs: 700, Confidence: 0.95},
		{Word: "去", StartMs: 720, EndMs: 780, Confidence: 0.95},
		{Word: "公园", StartMs: 800, EndMs: 880, Confidence: 0.95},
		{Word: "散步。", StartMs: 900, EndMs: 1000, Confidence: 0.95},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	var speechBlocks []domain.SpeechBlock
	for _, b := range blocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speechBlocks = append(speechBlocks, b)
		}
	}

	// The short "好。" (100ms) must NOT be its own block; the whole stream
	// (1000ms) should be one block ≥ MinSpeechBlockMs.
	if len(speechBlocks) < 1 {
		t.Fatalf("expected >=1 speech block, got %d", len(speechBlocks))
	}
	for _, b := range speechBlocks {
		dur := b.EndMs - b.StartMs
		if dur < cfg.MinSpeechBlockMs {
			t.Errorf("speech block duration %d < MinSpeechBlockMs %d (punctuation split not gated)", dur, cfg.MinSpeechBlockMs)
		}
	}
}

func TestBuildSpeechBlocks_PunctuationSplitAllowedWhenNextIsSpeakerChange(t *testing.T) {
	// A short sentence followed by a speaker change: the punctuation split IS
	// allowed because the next boundary is a speaker change (semantic turn).
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
	words := []domain.WordTiming{
		{Word: "对。", StartMs: 0, EndMs: 100, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "不。", StartMs: 200, EndMs: 300, Confidence: 0.95, SpeakerID: "SPEAKER_01"},
		{Word: "继续。", StartMs: 400, EndMs: 600, Confidence: 0.95, SpeakerID: "SPEAKER_01"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	speechBlocks := 0
	for _, b := range blocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			speechBlocks++
		}
	}
	if speechBlocks < 2 {
		t.Fatalf("expected >=2 speech blocks (speaker change split must still occur), got %d", speechBlocks)
	}
}

func TestBuildSpeechBlocks_PreservesCompleteExplicitSilence(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
	// Word block 1: [0, 1000]. Gap of 5000ms (>= MinSilenceMs 300). Word block 2: [6000, 7000].
	words := []domain.WordTiming{
		{Word: "你好", StartMs: 0, EndMs: 1000, Confidence: 0.95},
		{Word: "再见", StartMs: 6000, EndMs: 7000, Confidence: 0.95},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	// Expect: speech block 1 [0,1000], silence block [1000,6000], speech block 2 [6000,7000].
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (speech, silence, speech), got %d: %+v", len(blocks), blocks)
	}
	sil := blocks[1]
	if sil.SegmentType != domain.SpeechBlockTypeSilence {
		t.Fatalf("expected silence block at index 1, got %s", sil.SegmentType)
	}
	if sil.StartMs != 1000 || sil.EndMs != 6000 {
		t.Errorf("silence block must span the COMPLETE explicit gap [1000,6000], got [%d,%d]", sil.StartMs, sil.EndMs)
	}
}

func TestBuildSpeechBlocks_ZeroDurationWordClampedToAvailableGap(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
	// Case from 7679392272936389915 segment 12: an isolated interjection word "啊"
	// where ASR/alignment output start_ms == end_ms == 29680.
	// Silence precedes it (ends at 27840), and silence follows it (next word starts at 32160).
	words := []domain.WordTiming{
		{Word: "喂", StartMs: 22480, EndMs: 22560, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "啊", StartMs: 29680, EndMs: 29680, Confidence: 0.90, SpeakerID: "SPEAKER_00"},
		{Word: "我", StartMs: 32160, EndMs: 32720, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	for _, b := range blocks {
		if b.SegmentType == domain.SpeechBlockTypeSpeech {
			dur := b.EndMs - b.StartMs
			if dur <= 0 {
				t.Fatalf("speech block %d (%q) has zero or negative duration: start_ms=%d end_ms=%d",
					b.Index, b.SourceText, b.StartMs, b.EndMs)
			}
		}
	}
}

func assertNoOverlapAndPositiveDuration(t *testing.T, blocks []domain.SpeechBlock) {
	t.Helper()
	for idx, b := range blocks {
		t.Logf("block %d (%s: %q) [%d, %d]", idx, b.SegmentType, b.SourceText, b.StartMs, b.EndMs)
		if b.EndMs <= b.StartMs {
			t.Fatalf("block %d (%s: %q) has zero or negative duration: [%d, %d]",
				idx, b.SegmentType, b.SourceText, b.StartMs, b.EndMs)
		}
		if idx > 0 {
			prev := blocks[idx-1]
			if prev.EndMs > b.StartMs {
				t.Fatalf("block %d [%d, %d] overlaps next block %d [%d, %d]",
					idx-1, prev.StartMs, prev.EndMs, idx, b.StartMs, b.EndMs)
			}
		}
	}
}

func TestBuildSpeechBlocks_DegenerateWord_ZeroNextGap(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 0,
		PunctuationSplit:   true,
	}
	// Word 0 ends at 1800. Gap of 1200ms. Word 1 is degenerate [3000, 3000].
	// Word 2 starts at 3000 with a speaker change (next gap = 0).
	words := []domain.WordTiming{
		{Word: "喂。", StartMs: 1000, EndMs: 1800, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "啊。", StartMs: 3000, EndMs: 3000, Confidence: 0.90, SpeakerID: "SPEAKER_00"},
		{Word: "走。", StartMs: 3000, EndMs: 4000, Confidence: 0.95, SpeakerID: "SPEAKER_01"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	assertNoOverlapAndPositiveDuration(t, blocks)
}

func TestBuildSpeechBlocks_DegenerateWord_ShortNextGap(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 0,
		PunctuationSplit:   true,
	}
	// Word 0 ends at 1800. Gap of 1200ms. Word 1 is degenerate [3000, 3000].
	// Word 2 starts at 3050 (next gap = 50ms < 200ms) with a speaker change.
	words := []domain.WordTiming{
		{Word: "喂。", StartMs: 1000, EndMs: 1800, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "啊。", StartMs: 3000, EndMs: 3000, Confidence: 0.90, SpeakerID: "SPEAKER_00"},
		{Word: "走。", StartMs: 3050, EndMs: 4000, Confidence: 0.95, SpeakerID: "SPEAKER_01"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	assertNoOverlapAndPositiveDuration(t, blocks)
}

func TestBuildSpeechBlocks_DegenerateWord_FinalWord(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
	// Word 0 ends at 1800. Word 1 is the final word and degenerate [3000, 3000].
	words := []domain.WordTiming{
		{Word: "喂。", StartMs: 1000, EndMs: 1800, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "啊。", StartMs: 3000, EndMs: 3000, Confidence: 0.90, SpeakerID: "SPEAKER_00"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	assertNoOverlapAndPositiveDuration(t, blocks)
}

func TestBuildSpeechBlocks_DegenerateWord_ZeroAvailableInterval(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 0,
		PunctuationSplit:   true,
	}
	// Previous block ends at 2000. Degenerate word is [2000, 2000]. Next word starts at 2000.
	// Both forwardGap <= 0 and backwardGap <= 0.
	words := []domain.WordTiming{
		{Word: "喂。", StartMs: 1000, EndMs: 2000, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "啊。", StartMs: 2000, EndMs: 2000, Confidence: 0.90, SpeakerID: "SPEAKER_01"},
		{Word: "走。", StartMs: 2000, EndMs: 3000, Confidence: 0.95, SpeakerID: "SPEAKER_02"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	assertNoOverlapAndPositiveDuration(t, blocks)
}

func TestBuildSpeechBlocks_DegenerateWord_FinalWordZeroBackwardGap(t *testing.T) {
	cfg := domain.SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 0,
		PunctuationSplit:   true,
	}
	// Word 0 ends at 2000. Word 1 is degenerate [2000, 2000] and final (backwardGap = 0, no next word).
	words := []domain.WordTiming{
		{Word: "喂。", StartMs: 1000, EndMs: 2000, Confidence: 0.95, SpeakerID: "SPEAKER_00"},
		{Word: "啊。", StartMs: 2000, EndMs: 2000, Confidence: 0.90, SpeakerID: "SPEAKER_01"},
	}

	blocks := BuildSpeechBlocks(words, cfg)
	assertNoOverlapAndPositiveDuration(t, blocks)
}
