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
