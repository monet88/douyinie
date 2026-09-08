// Package speech implements the canonical speech understanding pipeline:
// accepted transcript -> forced alignment -> conditional diarization ->
// canonical SpeechBlock segmentation (semantic/punctuation/pause rules).
//
// Recognizer (ASR/VAD) boundaries are NEVER canonical on their own: a long
// multi-sentence VAD turn is split into canonical SpeechBlock units only after
// forced alignment + speaker assignment, using punctuation, pause, and
// speaker-change rules (locked #16 §4 / #18 speech understanding lock).
package speech

import (
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

// sentenceFinalPunct is the set of sentence-final punctuation marks that can
// terminate a canonical SpeechBlock when PunctuationSplit is enabled.
var sentenceFinalPunct = map[rune]bool{
	'.': true, '!': true, '?': true,
	'。': true, '！': true, '？': true,
	'…': true,
}

// isSentenceFinal reports whether the given word text ends with sentence-final punctuation.
func isSentenceFinal(word string) bool {
	if word == "" {
		return false
	}
	r := []rune(word)
	return sentenceFinalPunct[r[len(r)-1]]
}

// splitWordsByPunctuation splits a single ASR raw segment's text into sentence
// units by sentence-final punctuation, returning the boundaries as word indexes.
// Punctuation-based splits never replace alignment-driven segmentation; they
// only seed candidate boundaries that must still satisfy timing rules.
func splitWordsByPunctuation(words []domain.WordTiming) []int {
	var boundaries []int
	for i, w := range words {
		if isSentenceFinal(w.Word) {
			boundaries = append(boundaries, i+1)
		}
	}
	return boundaries
}

// speakerChangeAt reports whether the speaker changes between word i-1 and word i.
func speakerChangeAt(words []domain.WordTiming, i int) bool {
	if i <= 0 || i >= len(words) {
		return false
	}
	prev := strings.TrimSpace(words[i-1].SpeakerID)
	cur := strings.TrimSpace(words[i].SpeakerID)
	return prev != "" && cur != "" && prev != cur
}

// splitPoint is a candidate canonical split boundary between words[wordIdx-1]
// and words[wordIdx]. priority: 3=speaker change, 2=pause, 1=punctuation.
type splitPoint struct {
	wordIdx  int // split AFTER words[wordIdx-1] (i.e. between i-1 and i)
	priority int // 3=speaker change, 2=pause, 1=punctuation
	gapMs    int64
}

// BuildSpeechBlocks performs canonical SpeechBlock segmentation over aligned,
// speaker-annotated word timings using semantic (punctuation), pause, and
// speaker-change rules. ASR/VAD raw boundaries are ignored entirely: only the
// word-level alignment timeline is canonical input.
//
// Rules (in priority order for a split point between word i-1 and word i):
//  1. Speaker change with a gap >= MinSpeakerChangeMs always splits.
//  2. A pause gap >= PauseSplitMs splits (source silence stays explicit).
//  3. Sentence-final punctuation splits when PunctuationSplit is enabled
//     (the block must still satisfy MinSpeechBlockMs unless the next gap is
//     a speaker change).
//  4. A block that would exceed MaxSpeechBlockMs splits at the largest
//     available boundary (speaker change > pause > punctuation).
//
// Silence between speech blocks is preserved as explicit silence SpeechBlocks
// (MinSilenceMs threshold). A silence block always spans the complete explicit
// source gap [prevEnd, next.StartMs] so source silence is never truncated.
func BuildSpeechBlocks(words []domain.WordTiming, cfg domain.SegmentRuleConfig) []domain.SpeechBlock {
	if len(words) == 0 {
		return nil
	}

	// Candidate split points with their priority and gap.
	var splits []splitPoint

	// Precompute punctuation boundaries (word indexes AFTER the punct word).
	punctSet := map[int]bool{}
	if cfg.PunctuationSplit {
		for _, b := range splitWordsByPunctuation(words) {
			punctSet[b] = true
		}
	}

	for i := 1; i < len(words); i++ {
		gap := words[i].StartMs - words[i-1].EndMs
		sp := speakerChangeAt(words, i)
		switch {
		case sp && gap >= cfg.MinSpeakerChangeMs:
			splits = append(splits, splitPoint{wordIdx: i, priority: 3, gapMs: gap})
		case gap >= cfg.PauseSplitMs:
			splits = append(splits, splitPoint{wordIdx: i, priority: 2, gapMs: gap})
		case punctSet[i] && gap >= 0:
			splits = append(splits, splitPoint{wordIdx: i, priority: 1, gapMs: gap})
		}
	}

	// Greedy segmentation respecting MaxSpeechBlockMs by forcing splits at the
	// best available candidate boundary when a block would otherwise exceed it.
	blocks := make([]domain.SpeechBlock, 0, len(splits)+1)
	blockStart := 0

	flushBlock := func(end int) {
		if end <= blockStart {
			return
		}
		seg := words[blockStart:end]
		text := joinWords(seg)
		speaker := dominantSpeaker(seg)
		conf := avgConfidence(seg)
		segType := domain.SpeechBlockTypeSpeech
		if domain.IsPathologicalRepetitionNoise(text) {
			segType = domain.SpeechBlockTypeNoise
		}
		startMs := seg[0].StartMs
		endMs := seg[len(seg)-1].EndMs
		if endMs <= startMs {
			// A degenerate aligned word (endMs <= startMs) must never create a
			// zero/negative dub slot, must never invent time beyond the actually
			// available source interval, and must never overlap adjacent words.
			nextStart := int64(-1)
			if end < len(words) {
				nextStart = words[end].StartMs
			}
			prevEnd := int64(0)
			if len(blocks) > 0 {
				prevEnd = blocks[len(blocks)-1].EndMs
			}

			backwardGap := startMs - prevEnd
			claimLimit := int64(200)
			if nextStart >= 0 {
				forwardGap := nextStart - startMs
				if forwardGap > 0 {
					claim := forwardGap
					if claim > claimLimit {
						claim = claimLimit
					}
					endMs = startMs + claim
				} else if backwardGap > 0 {
					borrow := backwardGap
					if borrow > claimLimit {
						borrow = claimLimit
					}
					startMs -= borrow
				} else {
					blockStart = end
					return
				}
			} else {
				if backwardGap > 0 {
					borrow := backwardGap
					if borrow > claimLimit {
						borrow = claimLimit
					}
					startMs -= borrow
				} else {
					blockStart = end
					return
				}
			}
		}
		blocks = append(blocks, domain.SpeechBlock{
			StartMs:           startMs,
			EndMs:             endMs,
			SpeakerID:         speaker,
			SourceText:        text,
			TokenTimings:      seg,
			SpeakerConfidence: conf,
			SegmentType:       segType,
		})
		blockStart = end
	}

	for i := 1; i <= len(words); i++ {
		// Force split when the block exceeds MaxSpeechBlockMs.
		if i > blockStart && words[i-1].EndMs-words[blockStart].StartMs > cfg.MaxSpeechBlockMs {
			// Find the best split point inside [blockStart, i).
			best := -1
			bestPri := 0
			for _, sp := range splits {
				if sp.wordIdx > blockStart && sp.wordIdx < i {
					if sp.priority > bestPri {
						best = sp.wordIdx
						bestPri = sp.priority
					}
				}
			}
			if best == -1 {
				// No candidate boundary: hard-split mid-word stream to honor the cap.
				best = i - 1
			}
			flushBlock(best)
			continue
		}

		// Normal split at a candidate boundary (highest priority wins at each index).
		// Punctuation-only splits must still satisfy MinSpeechBlockMs unless the
		// next gap is a speaker change (rule 3); pause and speaker-change splits
		// are semantic boundaries and stay exempt.
		for _, sp := range splits {
			if sp.wordIdx != i {
				continue
			}
			if sp.priority == 1 {
				blockDur := words[i-1].EndMs - words[blockStart].StartMs
				if blockDur < cfg.MinSpeechBlockMs && !nextSplitIsSpeakerChange(splits, i) {
					break // skip the undersized punctuation split; keep accumulating
				}
			}
			flushBlock(i)
			break
		}
	}
	// Flush the trailing block.
	flushBlock(len(words))

	// Insert explicit silence blocks between speech blocks when the gap is
	// meaningful (source silence stays explicit; never collapsed or pushed).
	var out []domain.SpeechBlock
	for idx, b := range blocks {
		if idx > 0 {
			prevEnd := blocks[idx-1].EndMs
			gap := b.StartMs - prevEnd
			if gap >= cfg.MinSilenceMs {
				// The silence block spans the COMPLETE explicit source gap
				// [prevEnd, b.StartMs]. No capping: truncating would drop part of
				// the source silence from the canonical timeline.
				out = append(out, domain.SpeechBlock{
					StartMs:     prevEnd,
					EndMs:       b.StartMs,
					SpeakerID:   "",
					SourceText:  "",
					SegmentType: domain.SpeechBlockTypeSilence,
				})
			}
		}
		b.Index = len(out)
		out = append(out, b)
	}
	return out
}

// joinWords concatenates word tokens into display text with spaces.
func joinWords(words []domain.WordTiming) string {
	var sb strings.Builder
	for i, w := range words {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strings.TrimSpace(w.Word))
	}
	return strings.TrimSpace(sb.String())
}

// dominantSpeaker returns the most frequent non-empty speaker across the words.
func dominantSpeaker(words []domain.WordTiming) string {
	counts := map[string]int{}
	for _, w := range words {
		s := strings.TrimSpace(w.SpeakerID)
		if s == "" {
			continue
		}
		counts[s]++
	}
	best := ""
	bestCount := 0
	for s, c := range counts {
		if c > bestCount {
			best = s
			bestCount = c
		}
	}
	return best
}

// avgConfidence returns the mean word confidence over the segment.
func avgConfidence(words []domain.WordTiming) float64 {
	if len(words) == 0 {
		return 0
	}
	var sum float64
	for _, w := range words {
		sum += w.Confidence
	}
	return sum / float64(len(words))
}

// nextSplitIsSpeakerChange returns true when the next split point after the
// given index (i) is a speaker-change boundary (priority 3). This is used by
// the MinSpeechBlockMs gate: a punctuation split that would create a short
// block is still permitted when the next boundary is a speaker change, because
// the short block is semantically justified (a speaker's short interjection).
func nextSplitIsSpeakerChange(splits []splitPoint, i int) bool {
	for _, sp := range splits {
		if sp.wordIdx > i {
			return sp.priority == 3
		}
	}
	return false
}
