package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

// Invariant Constants for Phase 1.1 Quality Gates (Spec #63 / Issue #72 / Issue #59)
const (
	MaxOverallCER            = 0.05 // <= 5% overall ASR CER
	MaxPerCategoryCER        = 0.10 // <= 10% per category ASR CER
	MaxSeriousErrorRate      = 0.01 // <= 1% serious error rate (deletions/hallucinations/critical words)
	MaxForcedAlignmentP95Ms  = 250  // <= 250 ms forced-alignment P95
	MinTimingWithin200MsRate = 0.95 // >= 95% of localized speech within ±200 ms of source slots
	MaxCumulativeDriftMs     = 100  // <= 100 ms whole-video cumulative drift
	MaxLocalRTF              = 5.0  // <= 5x RTF on Local 8 GB profile
)

// CaseQualityMetrics holds computed quantitative metrics for a single quality case execution.
type CaseQualityMetrics struct {
	CaseID                   string   `json:"case_id"`
	SourceVideoID            string   `json:"source_video_id"`
	PrimaryCategory          string   `json:"primary_category"`
	TargetLanguage           string   `json:"target_language"`
	Profile                  string   `json:"profile"` // "local", "hybrid"
	ASRCER                   float64  `json:"asr_cer"`
	ASRSeriousErrorRate      float64  `json:"asr_serious_error_rate"`
	ForcedAlignmentP95Ms     int64    `json:"forced_alignment_p95_ms"`
	DubbingTimingWithin200Ms float64  `json:"dubbing_timing_within_200ms_rate"`
	CumulativeDriftMs        int64    `json:"cumulative_drift_ms"`
	CriticalMeaningErrors    int      `json:"critical_meaning_errors"`
	MaxRTF                   float64  `json:"max_rtf"`
	PeakVRAMBytes            uint64   `json:"peak_vram_bytes"`
	Status                   string   `json:"status"` // "PASS", "REVIEW_REQUIRED", "FAIL"
	ReviewReasons            []string `json:"review_reasons,omitempty"`
	FailReasons              []string `json:"fail_reasons,omitempty"`

	// Numerators and denominators for denominator-correct corpus/category aggregation
	RefCharCount              int     `json:"ref_char_count,omitempty"`
	EditDistance              int     `json:"edit_distance,omitempty"`
	RefSpeechBlockCount       int     `json:"ref_speech_block_count,omitempty"`
	CorruptedSpeechBlockCount int     `json:"corrupted_speech_block_count,omitempty"`
	TotalDubbingBlocks        int     `json:"total_dubbing_blocks,omitempty"`
	CompliantDubbingBlocks    int     `json:"compliant_dubbing_blocks,omitempty"`
	AlignmentBoundaryErrors   []int64 `json:"alignment_boundary_errors,omitempty"`
}

// CategoryRollup summarizes metrics across all cases in a primary challenge category.
type CategoryRollup struct {
	Category                 QualityCategory `json:"category"`
	CaseCount                int             `json:"case_count"`
	AverageCER               float64         `json:"average_cer"`
	MaxCER                   float64         `json:"max_cer"`
	AverageSeriousErrorRate  float64         `json:"average_serious_error_rate"`
	ForcedAlignmentP95Ms     int64           `json:"forced_alignment_p95_ms"`
	DubbingTimingWithin200Ms float64         `json:"dubbing_timing_within_200ms_rate"`
	CumulativeDriftMs        int64           `json:"cumulative_drift_ms"`
	CriticalMeaningErrors    int             `json:"critical_meaning_errors"`
	PassCount                int             `json:"pass_count"`
	ReviewCount              int             `json:"review_count"`
	FailCount                int             `json:"fail_count"`
	GateSatisfied            bool            `json:"gate_satisfied"`
}

// ProfileRollup aggregates metrics across all 48 cases of a single profile (Local or Hybrid).
type ProfileRollup struct {
	Profile                  string                              `json:"profile"`     // "local", "hybrid"
	TotalCases               int                                 `json:"total_cases"` // Exactly 48
	OverallCER               float64                             `json:"overall_cer"`
	OverallSeriousErrorRate  float64                             `json:"overall_serious_error_rate"`
	ForcedAlignmentP95Ms     int64                               `json:"forced_alignment_p95_ms"`
	DubbingTimingWithin200Ms float64                             `json:"dubbing_timing_within_200ms_rate"`
	CumulativeDriftMs        int64                               `json:"cumulative_drift_ms"`
	CriticalMeaningErrors    int                                 `json:"critical_meaning_errors"`
	MaxRTF                   float64                             `json:"max_rtf"`
	PeakVRAMBytes            uint64                              `json:"peak_vram_bytes"`
	PassCount                int                                 `json:"pass_count"`
	ReviewCount              int                                 `json:"review_count"`
	FailCount                int                                 `json:"fail_count"`
	CategoryRollups          map[QualityCategory]*CategoryRollup `json:"category_rollups"`
	LanguageRollups          map[string]*LanguageRollup          `json:"language_rollups"`
	GateSatisfied            bool                                `json:"gate_satisfied"`
	FailReasons              []string                            `json:"fail_reasons,omitempty"`
}

// LanguageRollup breaks down profile performance by target language ("vi", "en").
type LanguageRollup struct {
	TargetLanguage           string  `json:"target_language"`
	CaseCount                int     `json:"case_count"`
	OverallCER               float64 `json:"overall_cer"`
	DubbingTimingWithin200Ms float64 `json:"dubbing_timing_within_200ms_rate"`
	PassCount                int     `json:"pass_count"`
	ReviewCount              int     `json:"review_count"`
	FailCount                int     `json:"fail_count"`
}

// QualityBenchmarkSummary captures benchmark evaluation results.
// Invariant (Spec #63 / Issue #65 / Issue #72): Each benchmark session is bound to one
// hard execution profile executing 48 cases (24 primary sources x VI/EN).
// Preliminary counts and percentages for PASS, REVIEW_REQUIRED, FAIL.
type QualityBenchmarkSummary struct {
	SessionID        string               `json:"session_id"`
	ManifestDigest   string               `json:"manifest_digest"`
	AuditPlanDigest  string               `json:"audit_plan_digest"`
	TotalCases       int                  `json:"total_cases"` // 48 cases for bound profile session, or 96 for combined summary
	LocalRollup      *ProfileRollup       `json:"local_rollup"`
	HybridRollup     *ProfileRollup       `json:"hybrid_rollup"`
	TotalPassCount   int                  `json:"total_pass_count"`
	TotalReviewCount int                  `json:"total_review_count"`
	TotalFailCount   int                  `json:"total_fail_count"`
	OverallVerdict   string               `json:"overall_verdict"` // "PASS", "REVIEW_REQUIRED", "FAIL"
	ComputedAt       time.Time            `json:"computed_at"`
	SummaryDigest    string               `json:"summary_digest"`
	CaseMetrics      []CaseQualityMetrics `json:"case_metrics"`
}

// LevenshteinDistance computes edit distance between two rune slices.
func LevenshteinDistance(s1, s2 []rune) int {
	len1, len2 := len(s1), len(s2)
	dp := make([][]int, len1+1)
	for i := range dp {
		dp[i] = make([]int, len2+1)
		dp[i][0] = i
	}
	for j := 0; j <= len2; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= len1; i++ {
		for j := 1; j <= len2; j++ {
			cost := 1
			if s1[i-1] == s2[j-1] {
				cost = 0
			}
			dp[i][j] = min(
				dp[i-1][j]+1,      // deletion
				dp[i][j-1]+1,      // insertion
				dp[i-1][j-1]+cost, // substitution
			)
		}
	}
	return dp[len1][len2]
}

func min(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func normalizeASRText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func extractDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func computeP95(errors []int64) int64 {
	if len(errors) == 0 {
		return 0
	}
	sorted := make([]int64, len(errors))
	copy(sorted, errors)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ComputeCER calculates Character Error Rate: Levenshtein(hypothesis, reference) / len(reference).
// It normalizes Unicode whitespace and punctuation as defined by authoritative ASR evaluation references.
func ComputeCER(hyp, ref string) float64 {
	refClean := normalizeASRText(ref)
	hypClean := normalizeASRText(hyp)
	rRef := []rune(refClean)
	rHyp := []rune(hypClean)
	if len(rRef) == 0 {
		if len(rHyp) == 0 {
			return 0.0
		}
		return 1.0
	}
	dist := LevenshteinDistance(rHyp, rRef)
	return float64(dist) / float64(len(rRef))
}

// Canonicalize produces deterministic JSON bytes for QualityBenchmarkSummary.
func (s *QualityBenchmarkSummary) Canonicalize() ([]byte, error) {
	if s == nil {
		return nil, errors.New("summary is nil")
	}
	type canonStruct struct {
		AuditPlanDigest  string `json:"audit_plan_digest"`
		HybridPass       int    `json:"hybrid_pass"`
		HybridReview     int    `json:"hybrid_review"`
		HybridFail       int    `json:"hybrid_fail"`
		LocalPass        int    `json:"local_pass"`
		LocalReview      int    `json:"local_review"`
		LocalFail        int    `json:"local_fail"`
		ManifestDigest   string `json:"manifest_digest"`
		OverallVerdict   string `json:"overall_verdict"`
		SessionID        string `json:"session_id"`
		TotalCases       int    `json:"total_cases"`
		TotalFailCount   int    `json:"total_fail_count"`
		TotalPassCount   int    `json:"total_pass_count"`
		TotalReviewCount int    `json:"total_review_count"`
	}

	canon := canonStruct{
		AuditPlanDigest:  strings.ToLower(strings.TrimSpace(s.AuditPlanDigest)),
		ManifestDigest:   strings.ToLower(strings.TrimSpace(s.ManifestDigest)),
		OverallVerdict:   s.OverallVerdict,
		SessionID:        strings.TrimSpace(s.SessionID),
		TotalCases:       s.TotalCases,
		TotalFailCount:   s.TotalFailCount,
		TotalPassCount:   s.TotalPassCount,
		TotalReviewCount: s.TotalReviewCount,
	}
	if s.LocalRollup != nil {
		canon.LocalPass = s.LocalRollup.PassCount
		canon.LocalReview = s.LocalRollup.ReviewCount
		canon.LocalFail = s.LocalRollup.FailCount
	}
	if s.HybridRollup != nil {
		canon.HybridPass = s.HybridRollup.PassCount
		canon.HybridReview = s.HybridRollup.ReviewCount
		canon.HybridFail = s.HybridRollup.FailCount
	}
	return json.Marshal(canon)
}

// ComputeSummaryDigest computes the SHA-256 digest of canonical summary bytes.
func (s *QualityBenchmarkSummary) ComputeSummaryDigest() (string, error) {
	b, err := s.Canonicalize()
	if err != nil {
		return "", fmt.Errorf("canonicalize summary: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// VerifySummaryDigest validates the digest of the quality summary.
func (s *QualityBenchmarkSummary) VerifySummaryDigest() error {
	if s == nil {
		return errors.New("summary is nil")
	}
	if strings.TrimSpace(s.SummaryDigest) == "" {
		return errors.New("summary_digest is empty")
	}
	comp, err := s.ComputeSummaryDigest()
	if err != nil {
		return err
	}
	if !strings.EqualFold(s.SummaryDigest, comp) {
		return fmt.Errorf("summary digest mismatch: expected %s, computed %s", s.SummaryDigest, comp)
	}
	return nil
}

// EvaluateCaseQuality computes quantitative metrics for one QualityCaseEvidence
// against its frozen ReferenceAnnotationPack using real measured stage artifacts.
func EvaluateCaseQuality(caseEv *QualityCaseEvidence, pack *ReferenceAnnotationPack) CaseQualityMetrics {
	metrics := CaseQualityMetrics{
		CaseID:          caseEv.CaseID,
		SourceVideoID:   caseEv.SourceVideoID,
		PrimaryCategory: caseEv.PrimaryCategory,
		TargetLanguage:  caseEv.TargetLanguage,
		Profile:         caseEv.Profile,
		Status:          "PASS",
	}

	if pack == nil {
		metrics.Status = "FAIL"
		metrics.FailReasons = append(metrics.FailReasons, "missing reference annotation pack")
		return metrics
	}

	// 1. ASR CER and serious error rate calculation
	goldBuilder := strings.Builder{}
	for _, seg := range pack.ChineseTranscript {
		goldBuilder.WriteString(seg.ChineseText)
	}
	goldText := goldBuilder.String()

	hypText := ""
	if caseEv.StageArtifacts != nil && caseEv.StageArtifacts.Transcript != nil {
		t := caseEv.StageArtifacts.Transcript
		if len(t.SpeechBlocks) > 0 {
			var hb strings.Builder
			for _, sb := range t.SpeechBlocks {
				hb.WriteString(sb.SourceText)
			}
			hypText = hb.String()
		} else if len(t.WordTimings) > 0 {
			var hb strings.Builder
			for _, wt := range t.WordTimings {
				hb.WriteString(wt.Word)
			}
			hypText = hb.String()
		} else if len(t.RawSegments) > 0 {
			var hb strings.Builder
			for _, rs := range t.RawSegments {
				hb.WriteString(rs.Text)
			}
			hypText = hb.String()
		}
	}
	if len(goldText) > 0 {
		cer := ComputeCER(hypText, goldText)
		metrics.ASRCER = cer
		rRefClean := []rune(normalizeASRText(goldText))
		rHypClean := []rune(normalizeASRText(hypText))
		metrics.RefCharCount = len(rRefClean)
		metrics.EditDistance = LevenshteinDistance(rHypClean, rRefClean)
		if cer > MaxPerCategoryCER {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("ASR CER %f exceeds per-category ceiling %f", cer, MaxPerCategoryCER))
		}
	} else {
		if len(normalizeASRText(hypText)) > 0 {
			metrics.ASRCER = 1.0
			metrics.EditDistance = len([]rune(normalizeASRText(hypText)))
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, "hallucinated speech on no-dub video")
		} else {
			metrics.ASRCER = 0.0
		}
	}

	// Serious ASR error rate (denominator is frozen reference speech block count)
	totalRefBlocks := len(pack.ChineseTranscript)
	metrics.RefSpeechBlockCount = totalRefBlocks
	if totalRefBlocks > 0 {
		corruptedBlocks := 0
		cleanOverallHyp := normalizeASRText(hypText)

		for _, seg := range pack.ChineseTranscript {
			var segCrit []ReferenceCriticalItem
			for _, item := range pack.CriticalItems {
				if item.SegmentID == seg.SegmentID || (item.SegmentID == "" && strings.Contains(seg.ChineseText, item.SourceText)) {
					segCrit = append(segCrit, item)
				}
			}

			if len(segCrit) == 0 {
				continue
			}

			blockCorrupted := false
			blockIndeterminate := false
			for _, item := range segCrit {
				cleanItem := normalizeASRText(item.SourceText)
				if cleanItem == "" {
					continue
				}

				segHypText := ""
				if caseEv.StageArtifacts != nil && caseEv.StageArtifacts.Transcript != nil {
					for _, b := range caseEv.StageArtifacts.Transcript.SpeechBlocks {
						if b.StartMs < seg.EndMs && b.EndMs > seg.StartMs {
							segHypText += b.SourceText
						}
					}
				}
				cleanSegHyp := normalizeASRText(segHypText)

				// Conservative segment-scoped evaluation:
				// If the critical item is preserved in the segment hypothesis, it passes.
				// If not in segment, but present elsewhere in the full hypothesis:
				// - If single-occurrence in ref and present in full hyp, boundary shift -> indeterminate, review required.
				// - If absent from entire transcript -> definite corruption/loss -> corrupted.
				if cleanSegHyp != "" {
					if !strings.Contains(cleanSegHyp, cleanItem) {
						if strings.Contains(cleanOverallHyp, cleanItem) {
							blockIndeterminate = true
						} else {
							blockCorrupted = true
							break
						}
					}
				} else {
					if strings.Contains(cleanOverallHyp, cleanItem) {
						blockIndeterminate = true
					} else {
						blockCorrupted = true
						break
					}
				}
			}
			if blockCorrupted {
				corruptedBlocks++
			} else if blockIndeterminate && metrics.Status != "FAIL" {
				metrics.Status = "REVIEW_REQUIRED"
				metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("critical item in block %s displaced outside segment boundary", seg.SegmentID))
			}
		}

		metrics.CorruptedSpeechBlockCount = corruptedBlocks
		seriousRate := float64(corruptedBlocks) / float64(totalRefBlocks)
		metrics.ASRSeriousErrorRate = seriousRate
		if seriousRate > MaxSeriousErrorRate {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("ASR serious error rate %f exceeds ceiling %f", seriousRate, MaxSeriousErrorRate))
		}
	}

	// Forced alignment P95 (Ms)
	type charBoundary struct {
		char    rune
		startMs int64
		endMs   int64
	}
	var refChars []charBoundary
	for _, seg := range pack.ChineseTranscript {
		if len(seg.Words) > 0 {
			for _, w := range seg.Words {
				runes := []rune(w.Word)
				if len(runes) == 0 {
					continue
				}
				dur := (w.EndMs - w.StartMs) / int64(len(runes))
				for idx, ch := range runes {
					st := w.StartMs + int64(idx)*dur
					en := st + dur
					if idx == len(runes)-1 {
						en = w.EndMs
					}
					refChars = append(refChars, charBoundary{char: ch, startMs: st, endMs: en})
				}
			}
		} else {
			runes := []rune(seg.ChineseText)
			if len(runes) > 0 {
				dur := (seg.EndMs - seg.StartMs) / int64(len(runes))
				for idx, ch := range runes {
					st := seg.StartMs + int64(idx)*dur
					en := st + dur
					if idx == len(runes)-1 {
						en = seg.EndMs
					}
					refChars = append(refChars, charBoundary{char: ch, startMs: st, endMs: en})
				}
			}
		}
	}

	var hypChars []charBoundary
	if caseEv.StageArtifacts != nil && caseEv.StageArtifacts.Transcript != nil {
		t := caseEv.StageArtifacts.Transcript
		if len(t.WordTimings) > 0 {
			for _, wt := range t.WordTimings {
				runes := []rune(wt.Word)
				if len(runes) == 0 {
					continue
				}
				dur := (wt.EndMs - wt.StartMs) / int64(len(runes))
				for idx, ch := range runes {
					st := wt.StartMs + int64(idx)*dur
					en := st + dur
					if idx == len(runes)-1 {
						en = wt.EndMs
					}
					hypChars = append(hypChars, charBoundary{char: ch, startMs: st, endMs: en})
				}
			}
		} else if len(t.SpeechBlocks) > 0 {
			for _, sb := range t.SpeechBlocks {
				if len(sb.TokenTimings) > 0 {
					for _, tt := range sb.TokenTimings {
						runes := []rune(tt.Word)
						if len(runes) == 0 {
							continue
						}
						dur := (tt.EndMs - tt.StartMs) / int64(len(runes))
						for idx, ch := range runes {
							st := tt.StartMs + int64(idx)*dur
							en := st + dur
							if idx == len(runes)-1 {
								en = tt.EndMs
							}
							hypChars = append(hypChars, charBoundary{char: ch, startMs: st, endMs: en})
						}
					}
				} else {
					runes := []rune(sb.SourceText)
					if len(runes) > 0 {
						dur := (sb.EndMs - sb.StartMs) / int64(len(runes))
						for idx, ch := range runes {
							st := sb.StartMs + int64(idx)*dur
							en := st + dur
							if idx == len(runes)-1 {
								en = sb.EndMs
							}
							hypChars = append(hypChars, charBoundary{char: ch, startMs: st, endMs: en})
						}
					}
				}
			}
		}
	}

	cleanRefChars := make([]charBoundary, 0, len(refChars))
	for _, c := range refChars {
		if !unicode.IsSpace(c.char) && !unicode.IsPunct(c.char) {
			cleanRefChars = append(cleanRefChars, c)
		}
	}
	cleanHypChars := make([]charBoundary, 0, len(hypChars))
	for _, c := range hypChars {
		if !unicode.IsSpace(c.char) && !unicode.IsPunct(c.char) {
			cleanHypChars = append(cleanHypChars, c)
		}
	}

	if len(cleanRefChars) == 0 {
		metrics.ForcedAlignmentP95Ms = 0
	} else if len(cleanHypChars) == 0 {
		metrics.ForcedAlignmentP95Ms = 1000
		metrics.Status = "FAIL"
		metrics.FailReasons = append(metrics.FailReasons, "missing forced alignment timings")
	} else {
		var alignErrors []int64
		for i, rc := range cleanRefChars {
			if i < len(cleanHypChars) {
				hc := cleanHypChars[i]
				errStart := absInt64(hc.startMs - rc.startMs)
				errEnd := absInt64(hc.endMs - rc.endMs)
				bErr := errStart
				if errEnd > bErr {
					bErr = errEnd
				}
				if hc.char != rc.char {
					bErr += 200
				}
				alignErrors = append(alignErrors, bErr)
			} else {
				penalty := rc.endMs - rc.startMs + 300
				if penalty < 300 {
					penalty = 300
				}
				alignErrors = append(alignErrors, penalty)
			}
		}
		metrics.AlignmentBoundaryErrors = alignErrors
		p95 := computeP95(alignErrors)
		metrics.ForcedAlignmentP95Ms = p95
		if p95 > MaxForcedAlignmentP95Ms {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("forced alignment P95 %d ms exceeds ceiling %d ms", p95, MaxForcedAlignmentP95Ms))
		}
	}

	// 2. Dubbing timing and whole-video cumulative drift
	if caseEv.StageArtifacts != nil && caseEv.StageArtifacts.DubSegments != nil && len(caseEv.StageArtifacts.DubSegments.Segments) > 0 {
		segs := caseEv.StageArtifacts.DubSegments.Segments
		within200Count := 0
		has500MsOverrun := false
		var maxTimingErr int64

		for _, ds := range segs {
			finish := ds.StartMs + ds.MeasuredDurationMs
			sourceEnd := ds.EndMs
			diff := absInt64(finish - sourceEnd)
			if diff <= 200 {
				within200Count++
			}
			if diff > 500 {
				has500MsOverrun = true
			}
			if diff > maxTimingErr {
				maxTimingErr = diff
			}
		}

		metrics.TotalDubbingBlocks = len(segs)
		metrics.CompliantDubbingBlocks = within200Count
		timingRate := float64(within200Count) / float64(len(segs))
		metrics.DubbingTimingWithin200Ms = timingRate
		if timingRate < MinTimingWithin200MsRate {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("dubbing timing within ±200ms rate %f below required %f", timingRate, MinTimingWithin200MsRate))
		}
		if has500MsOverrun && metrics.Status != "FAIL" {
			metrics.Status = "REVIEW_REQUIRED"
			metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("dubbing block timing error %d ms exceeds 500ms threshold", maxTimingErr))
		}

		// Whole-video cumulative drift under immutable source anchor contract
		var cumDrift int64
		if len(segs) > 0 {
			lastSeg := segs[len(segs)-1]
			lastFinish := lastSeg.StartMs + lastSeg.MeasuredDurationMs
			lastSourceEnd := lastSeg.EndMs
			cumDrift = absInt64(lastFinish - lastSourceEnd)
		}
		metrics.CumulativeDriftMs = cumDrift
		if cumDrift > MaxCumulativeDriftMs {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("cumulative drift %d ms exceeds ceiling %d ms", cumDrift, MaxCumulativeDriftMs))
		}
		for _, ds := range segs {
			if ds.RequiresReview && strings.Contains(ds.ReviewReason, "anchor_drift") {
				if metrics.Status != "FAIL" {
					metrics.Status = "REVIEW_REQUIRED"
					metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("dub segment %d flagged for anchor drift: %s", ds.Index, ds.ReviewReason))
				}
			}
		}
	} else if len(pack.ChineseTranscript) > 0 && !caseEv.IsNoDub {
		if su, ok := caseEv.Stages["dub_synthesize"]; ok && su.Status == "COMPLETED" {
			metrics.DubbingTimingWithin200Ms = 0.0
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, "dub synthesize completed but produced 0 dub segments")
		} else {
			metrics.DubbingTimingWithin200Ms = 0.0
		}
		metrics.CumulativeDriftMs = 0
	} else {
		metrics.DubbingTimingWithin200Ms = 1.0
		metrics.CumulativeDriftMs = 0
	}

	// Critical meaning errors
	if caseEv.StageArtifacts != nil && caseEv.StageArtifacts.Translation != nil {
		tv := caseEv.StageArtifacts.Translation
		criticalErrors := 0
		hasUncertainty := false

		for _, tSeg := range tv.Segments {
			if !tSeg.PassedQAGate {
				criticalErrors++
			}
		}
		if caseEv.StageArtifacts.DubScript != nil {
			for _, dSeg := range caseEv.StageArtifacts.DubScript.Segments {
				if !dSeg.PassedQAGate {
					criticalErrors++
				}
				if dSeg.RequiresReview && (dSeg.ReviewReason == "meaning_corrupted" || dSeg.ReviewReason == "fact_corrupted") {
					criticalErrors++
				}
			}
		}

		if len(pack.CriticalItems) > 0 {
			for _, ci := range pack.CriticalItems {
				expectedRef := strings.ToLower(strings.TrimSpace(ci.TargetRefEN))
				if strings.EqualFold(caseEv.TargetLanguage, "vi") {
					expectedRef = strings.ToLower(strings.TrimSpace(ci.TargetRefVI))
				}
				if expectedRef == "" {
					continue
				}

				// Match frozen ReferenceCriticalItem SegmentID to its reference transcript segment
				var refSeg *ReferenceTranscriptSegment
				var refIdx = -1
				for i := range pack.ChineseTranscript {
					seg := &pack.ChineseTranscript[i]
					if ci.SegmentID != "" && seg.SegmentID == ci.SegmentID {
						refSeg = seg
						refIdx = i
						break
					}
					if ci.SegmentID == "" && strings.Contains(seg.ChineseText, ci.SourceText) {
						refSeg = seg
						refIdx = i
						break
					}
				}

				if refSeg == nil {
					hasUncertainty = true
					metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("critical item %s has no matching reference segment", ci.SourceText))
					continue
				}

				// Find corresponding TranslationSegment using index/timing/source mapping
				var matchedSeg *domain.TranslationSegment
				var matchCandidates []*domain.TranslationSegment

				cleanRefSource := normalizeASRText(refSeg.ChineseText)
				cleanCiSource := normalizeASRText(ci.SourceText)

				// 1. Exact source text match
				for i := range tv.Segments {
					ts := &tv.Segments[i]
					if normalizeASRText(ts.SourceText) == cleanRefSource {
						matchCandidates = append(matchCandidates, ts)
					}
				}

				// 2. Timing overlap or index match with source text containment
				if len(matchCandidates) == 0 {
					for i := range tv.Segments {
						ts := &tv.Segments[i]
						normTs := normalizeASRText(ts.SourceText)
						timingOverlap := false
						if refSeg.EndMs > refSeg.StartMs && ts.EndMs > ts.StartMs {
							if ts.StartMs < refSeg.EndMs && ts.EndMs > refSeg.StartMs {
								timingOverlap = true
							}
						}
						textContains := strings.Contains(normTs, cleanCiSource) || strings.Contains(cleanRefSource, normTs)
						if (timingOverlap && textContains) || (ts.Index == refIdx && textContains) {
							matchCandidates = append(matchCandidates, ts)
						}
					}
				}

				// 3. Direct 1:1 index alignment if segment counts match
				if len(matchCandidates) == 0 && len(tv.Segments) == len(pack.ChineseTranscript) && refIdx >= 0 && refIdx < len(tv.Segments) {
					matchCandidates = append(matchCandidates, &tv.Segments[refIdx])
				}

				if len(matchCandidates) == 1 {
					matchedSeg = matchCandidates[0]
				} else if len(matchCandidates) > 1 {
					for _, ts := range matchCandidates {
						if ts.Index == refIdx || (ts.StartMs == refSeg.StartMs && ts.EndMs == refSeg.EndMs) {
							matchedSeg = ts
							break
						}
					}
				}

				if matchedSeg == nil {
					// Ambiguous correspondence: project REVIEW_REQUIRED rather than global-pass
					hasUncertainty = true
					metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("critical item in segment %s has ambiguous translation segment correspondence", ci.SegmentID))
					continue
				}

				// Evaluate ONLY the corresponding TranslationSegment
				segTargetText := strings.ToLower(matchedSeg.TargetText)

				if ci.Type == "number" {
					srcNums := service.ExtractNumbers(ci.SourceText, "zh")
					tgtNums := service.ExtractNumbers(segTargetText, caseEv.TargetLanguage)
					missingAny := false
					for n := range srcNums {
						if !tgtNums[n] {
							missingAny = true
							break
						}
					}
					if missingAny {
						criticalErrors++
					}
				} else if ci.Type == "negation" {
					hasTgtNeg := service.DetectNegation(segTargetText, caseEv.TargetLanguage)
					if !hasTgtNeg && !strings.Contains(segTargetText, expectedRef) && !matchedSeg.NegationPolarity {
						criticalErrors++
					}
				} else {
					// Entity / fact / name / core_meaning
					if !strings.Contains(segTargetText, expectedRef) {
						if !matchedSeg.PassedQAGate || tv.OverallQAScore < 0.90 {
							criticalErrors++
						} else {
							hasUncertainty = true
							metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("critical item %s missing from target segment %d", expectedRef, matchedSeg.Index))
						}
					}
				}
			}
		}
		metrics.CriticalMeaningErrors = criticalErrors
		if criticalErrors > 0 {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("%d critical translation errors detected", criticalErrors))
		} else if hasUncertainty && metrics.Status != "FAIL" {
			metrics.Status = "REVIEW_REQUIRED"
			metrics.ReviewReasons = append(metrics.ReviewReasons, "critical item semantic preservation uncertain")
		}
	} else if len(pack.ChineseTranscript) > 0 && !caseEv.IsNoDub {
		if st, ok := caseEv.Stages["translate"]; ok && st.Status == "COMPLETED" {
			metrics.CriticalMeaningErrors = 1
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, "translation completed without artifact")
		}
	} else {
		metrics.CriticalMeaningErrors = 0
	}

	// 3. Resource Telemetry: Peak VRAM and Max RTF
	var maxRTF float64
	var peakVRAM uint64
	for _, sample := range caseEv.Telemetry {
		if sample.RTF > maxRTF {
			maxRTF = sample.RTF
		}
		if sample.DevicePeakVRAMBytes > peakVRAM {
			peakVRAM = sample.DevicePeakVRAMBytes
		}
	}
	metrics.MaxRTF = maxRTF
	metrics.PeakVRAMBytes = peakVRAM

	// Invariant (Spec #63 / Issue #72): Missing required Local telemetry cannot produce PASS-by-zero.
	if strings.EqualFold(caseEv.Profile, "local") {
		if len(caseEv.Telemetry) == 0 {
			if metrics.Status != "FAIL" {
				metrics.Status = "REVIEW_REQUIRED"
				metrics.ReviewReasons = append(metrics.ReviewReasons, "missing authentic local resource telemetry")
			}
		} else if maxRTF > MaxLocalRTF {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("RTF %f exceeds local profile ceiling %f", maxRTF, MaxLocalRTF))
		}
	}

	// 4. Evaluate against stage failures or relational QC issues
	if caseEv.Status == "FAILED" {
		metrics.Status = "FAIL"
		metrics.FailReasons = append(metrics.FailReasons, caseEv.ErrorMessage)
		return metrics
	}

	for _, st := range caseEv.Stages {
		if st.Status == "FAILED" {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("stage %s failed: %s", st.Stage, st.ErrorMessage))
		}
	}

	// Relational QC issues inspection
	for _, qr := range caseEv.RelationalQC.QualityResults {
		if qr.OverallStatus == domain.QualityStatusFail {
			metrics.Status = "FAIL"
			metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("QC %s FAIL", qr.Stage))
		} else if qr.OverallStatus == domain.QualityStatusReviewRequired && metrics.Status != "FAIL" {
			metrics.Status = "REVIEW_REQUIRED"
			metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("QC %s REVIEW_REQUIRED", qr.Stage))
		}
	}

	for _, ri := range caseEv.RelationalQC.ReviewItems {
		if ri.Status == domain.ReviewItemStatusPending {
			if ri.Severity == "blocker" || ri.Severity == "error" {
				metrics.Status = "FAIL"
				metrics.FailReasons = append(metrics.FailReasons, fmt.Sprintf("unresolved %s review item: %s", ri.Severity, ri.Reason))
			} else if metrics.Status != "FAIL" {
				metrics.Status = "REVIEW_REQUIRED"
				metrics.ReviewReasons = append(metrics.ReviewReasons, fmt.Sprintf("pending review item: %s", ri.Reason))
			}
		}
	}

	return metrics
}

// ComputeQualitySummary aggregates metrics across all 48 Local + 48 Hybrid executions.
func ComputeQualitySummary(
	sessionID string,
	manifest *QualityCorpusManifest,
	auditPlan *StratifiedAuditPlan,
	cases map[string]*QualityCaseEvidence,
	packs map[string]*ReferenceAnnotationPack,
	computedAt time.Time,
) (*QualityBenchmarkSummary, error) {
	if manifest == nil {
		return nil, errors.New("manifest is nil")
	}
	if err := manifest.VerifyManifestDigest(); err != nil {
		return nil, fmt.Errorf("verify manifest digest: %w", err)
	}
	if len(packs) == 0 {
		return nil, errors.New("missing reference annotation packs for scored corpus")
	}

	packMap := make(map[string]*ReferenceAnnotationPack)
	for _, p := range manifest.PrimaryAssets {
		pack := packs[p.AssetID]
		if pack == nil {
			pack = packs[p.SourceAssetID]
		}
		if pack == nil {
			return nil, fmt.Errorf("missing reference annotation pack for primary asset %q", p.AssetID)
		}
		computedDigest, err := pack.ComputePackDigest()
		if err != nil {
			return nil, fmt.Errorf("compute pack digest for asset %q: %w", p.AssetID, err)
		}
		if !strings.EqualFold(computedDigest, p.ReferencePackDigest) {
			return nil, fmt.Errorf("reference pack digest mismatch for asset %q: expected %s, got %s", p.AssetID, p.ReferencePackDigest, computedDigest)
		}
		packMap[p.SourceAssetID] = pack
		packMap[p.AssetID] = pack
	}

	// Sort case keys for determinism
	caseKeys := make([]string, 0, len(cases))
	for k := range cases {
		caseKeys = append(caseKeys, k)
	}
	sort.Strings(caseKeys)

	caseMetrics := make([]CaseQualityMetrics, 0, len(caseKeys))
	localMetrics := make([]CaseQualityMetrics, 0, 48)
	hybridMetrics := make([]CaseQualityMetrics, 0, 48)

	for _, k := range caseKeys {
		ev := cases[k]
		if ev == nil {
			continue
		}
		pack := packMap[ev.SourceAssetID]
		if pack == nil {
			pack = packMap[ev.SourceVideoID]
		}
		m := EvaluateCaseQuality(ev, pack)
		ev.Metrics = &m
		caseMetrics = append(caseMetrics, m)
		if strings.EqualFold(m.Profile, "local") {
			localMetrics = append(localMetrics, m)
		} else if strings.EqualFold(m.Profile, "hybrid") {
			hybridMetrics = append(hybridMetrics, m)
		}
	}

	var localRollup *ProfileRollup
	if len(localMetrics) > 0 {
		localRollup = buildProfileRollup("local", localMetrics)
	}
	var hybridRollup *ProfileRollup
	if len(hybridMetrics) > 0 {
		hybridRollup = buildProfileRollup("hybrid", hybridMetrics)
	}

	var totalPass, totalReview, totalFail int
	if localRollup != nil {
		totalPass += localRollup.PassCount
		totalReview += localRollup.ReviewCount
		totalFail += localRollup.FailCount
	}
	if hybridRollup != nil {
		totalPass += hybridRollup.PassCount
		totalReview += hybridRollup.ReviewCount
		totalFail += hybridRollup.FailCount
	}

	// Overall verdict precedence: FAIL > REVIEW_REQUIRED > PASS
	verdict := "PASS"
	profileGateFailed := (localRollup != nil && !localRollup.GateSatisfied) ||
		(hybridRollup != nil && !hybridRollup.GateSatisfied)
	if profileGateFailed || totalFail > 0 {
		verdict = "FAIL"
	} else if totalReview > 0 {
		verdict = "REVIEW_REQUIRED"
	}

	auditPlanDigest := ""
	if auditPlan != nil {
		auditPlanDigest = auditPlan.PlanDigest
	}

	summary := &QualityBenchmarkSummary{
		SessionID:        sessionID,
		ManifestDigest:   manifest.ManifestDigest,
		AuditPlanDigest:  auditPlanDigest,
		TotalCases:       len(caseMetrics),
		TotalPassCount:   totalPass,
		TotalReviewCount: totalReview,
		TotalFailCount:   totalFail,
		OverallVerdict:   verdict,
		LocalRollup:      localRollup,
		HybridRollup:     hybridRollup,
		CaseMetrics:      caseMetrics,
		ComputedAt:       computedAt,
	}

	dig, err := summary.ComputeSummaryDigest()
	if err != nil {
		return nil, fmt.Errorf("compute summary digest: %w", err)
	}
	summary.SummaryDigest = dig

	return summary, nil
}

func buildProfileRollup(profile string, metrics []CaseQualityMetrics) *ProfileRollup {
	rollup := &ProfileRollup{
		Profile:         profile,
		TotalCases:      len(metrics),
		CategoryRollups: make(map[QualityCategory]*CategoryRollup),
		LanguageRollups: make(map[string]*LanguageRollup),
		GateSatisfied:   true,
	}

	if len(metrics) == 0 {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, "no cases executed for profile")
		return rollup
	}

	var totalEditDistance, totalRefChars int
	var totalCorruptedBlocks, totalRefSpeechBlocks int
	var totalCompliantDubBlocks, totalDubBlocks int
	var allAlignmentErrors []int64
	var maxRTF float64
	var peakVRAM uint64
	var maxDrift int64
	var totalCriticalErrors int

	// Group by category and language
	byCategory := make(map[QualityCategory][]CaseQualityMetrics)
	byLanguage := make(map[string][]CaseQualityMetrics)

	for _, m := range metrics {
		cat := QualityCategory(m.PrimaryCategory)
		byCategory[cat] = append(byCategory[cat], m)
		byLanguage[m.TargetLanguage] = append(byLanguage[m.TargetLanguage], m)

		totalEditDistance += m.EditDistance
		totalRefChars += m.RefCharCount
		totalCorruptedBlocks += m.CorruptedSpeechBlockCount
		totalRefSpeechBlocks += m.RefSpeechBlockCount
		totalCompliantDubBlocks += m.CompliantDubbingBlocks
		totalDubBlocks += m.TotalDubbingBlocks
		allAlignmentErrors = append(allAlignmentErrors, m.AlignmentBoundaryErrors...)

		if m.MaxRTF > maxRTF {
			maxRTF = m.MaxRTF
		}
		if m.PeakVRAMBytes > peakVRAM {
			peakVRAM = m.PeakVRAMBytes
		}
		if m.CumulativeDriftMs > maxDrift {
			maxDrift = m.CumulativeDriftMs
		}
		totalCriticalErrors += m.CriticalMeaningErrors

		switch m.Status {
		case "PASS":
			rollup.PassCount++
		case "REVIEW_REQUIRED":
			rollup.ReviewCount++
		case "FAIL":
			rollup.FailCount++
		}
	}

	if totalRefChars > 0 {
		rollup.OverallCER = float64(totalEditDistance) / float64(totalRefChars)
	}
	if totalRefSpeechBlocks > 0 {
		rollup.OverallSeriousErrorRate = float64(totalCorruptedBlocks) / float64(totalRefSpeechBlocks)
	}
	if totalDubBlocks > 0 {
		rollup.DubbingTimingWithin200Ms = float64(totalCompliantDubBlocks) / float64(totalDubBlocks)
	} else {
		rollup.DubbingTimingWithin200Ms = 1.0
	}
	rollup.ForcedAlignmentP95Ms = computeP95(allAlignmentErrors)
	rollup.MaxRTF = maxRTF
	rollup.PeakVRAMBytes = peakVRAM
	rollup.CumulativeDriftMs = maxDrift
	rollup.CriticalMeaningErrors = totalCriticalErrors

	// Check overall CER gate (<= 5%)
	if rollup.OverallCER > MaxOverallCER {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("overall CER %f exceeds %f", rollup.OverallCER, MaxOverallCER))
	}
	// Check overall serious error rate gate (<= 1%)
	if rollup.OverallSeriousErrorRate > MaxSeriousErrorRate {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("overall serious ASR error rate %f exceeds %f", rollup.OverallSeriousErrorRate, MaxSeriousErrorRate))
	}
	if rollup.ForcedAlignmentP95Ms > MaxForcedAlignmentP95Ms {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("forced alignment P95 %d ms exceeds ceiling %d ms", rollup.ForcedAlignmentP95Ms, MaxForcedAlignmentP95Ms))
	}
	if rollup.DubbingTimingWithin200Ms < MinTimingWithin200MsRate {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("dubbing timing within ±200ms rate %f below %f", rollup.DubbingTimingWithin200Ms, MinTimingWithin200MsRate))
	}
	if rollup.CumulativeDriftMs > MaxCumulativeDriftMs {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("cumulative drift %d ms exceeds ceiling %d ms", rollup.CumulativeDriftMs, MaxCumulativeDriftMs))
	}
	if rollup.CriticalMeaningErrors > 0 {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("%d critical meaning errors present in profile", rollup.CriticalMeaningErrors))
	}

	// Build category rollups and enforce per-category CER <= 10%
	for cat, catMetrics := range byCategory {
		cRoll := &CategoryRollup{
			Category:      cat,
			CaseCount:     len(catMetrics),
			GateSatisfied: true,
		}
		var cEditDistance, cRefChars int
		var cCorruptedBlocks, cRefSpeechBlocks int
		var cCompliantDubBlocks, cDubBlocks int
		var cAlignErrors []int64
		var cMaxDrift int64
		var cCriticalErrors int

		for _, cm := range catMetrics {
			cEditDistance += cm.EditDistance
			cRefChars += cm.RefCharCount
			cCorruptedBlocks += cm.CorruptedSpeechBlockCount
			cRefSpeechBlocks += cm.RefSpeechBlockCount
			cCompliantDubBlocks += cm.CompliantDubbingBlocks
			cDubBlocks += cm.TotalDubbingBlocks
			cAlignErrors = append(cAlignErrors, cm.AlignmentBoundaryErrors...)

			if cm.ASRCER > cRoll.MaxCER {
				cRoll.MaxCER = cm.ASRCER
			}
			if cm.CumulativeDriftMs > cMaxDrift {
				cMaxDrift = cm.CumulativeDriftMs
			}
			cCriticalErrors += cm.CriticalMeaningErrors

			switch cm.Status {
			case "PASS":
				cRoll.PassCount++
			case "REVIEW_REQUIRED":
				cRoll.ReviewCount++
			case "FAIL":
				cRoll.FailCount++
			}
		}
		if cRefChars > 0 {
			cRoll.AverageCER = float64(cEditDistance) / float64(cRefChars)
		}
		if cRefSpeechBlocks > 0 {
			cRoll.AverageSeriousErrorRate = float64(cCorruptedBlocks) / float64(cRefSpeechBlocks)
		}
		if cDubBlocks > 0 {
			cRoll.DubbingTimingWithin200Ms = float64(cCompliantDubBlocks) / float64(cDubBlocks)
		} else {
			cRoll.DubbingTimingWithin200Ms = 1.0
		}
		cRoll.ForcedAlignmentP95Ms = computeP95(cAlignErrors)
		cRoll.CumulativeDriftMs = cMaxDrift
		cRoll.CriticalMeaningErrors = cCriticalErrors

		if cRoll.AverageCER > MaxPerCategoryCER {
			cRoll.GateSatisfied = false
			rollup.GateSatisfied = false
			rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("category %s CER %f exceeds %f", cat, cRoll.AverageCER, MaxPerCategoryCER))
		}
		if cRoll.ForcedAlignmentP95Ms > MaxForcedAlignmentP95Ms {
			cRoll.GateSatisfied = false
			rollup.GateSatisfied = false
			rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("category %s alignment P95 %d ms exceeds %d ms", cat, cRoll.ForcedAlignmentP95Ms, MaxForcedAlignmentP95Ms))
		}
		if cRoll.DubbingTimingWithin200Ms < MinTimingWithin200MsRate {
			cRoll.GateSatisfied = false
			rollup.GateSatisfied = false
			rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("category %s timing within 200ms rate %f below %f", cat, cRoll.DubbingTimingWithin200Ms, MinTimingWithin200MsRate))
		}
		if cRoll.CumulativeDriftMs > MaxCumulativeDriftMs {
			cRoll.GateSatisfied = false
			rollup.GateSatisfied = false
			rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("category %s drift %d ms exceeds %d ms", cat, cRoll.CumulativeDriftMs, MaxCumulativeDriftMs))
		}
		if cRoll.CriticalMeaningErrors > 0 {
			cRoll.GateSatisfied = false
			rollup.GateSatisfied = false
			rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("category %s has %d critical meaning errors", cat, cRoll.CriticalMeaningErrors))
		}
		if cRoll.AverageSeriousErrorRate > MaxSeriousErrorRate {
			cRoll.GateSatisfied = false
			rollup.GateSatisfied = false
			rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("category %s serious ASR error rate %f exceeds %f", cat, cRoll.AverageSeriousErrorRate, MaxSeriousErrorRate))
		}
		rollup.CategoryRollups[cat] = cRoll
	}

	// Build language rollups
	for lang, langMetrics := range byLanguage {
		lRoll := &LanguageRollup{
			TargetLanguage: lang,
			CaseCount:      len(langMetrics),
		}
		var lEditDistance, lRefChars int
		var lCompliantDubBlocks, lDubBlocks int
		for _, lm := range langMetrics {
			lEditDistance += lm.EditDistance
			lRefChars += lm.RefCharCount
			lCompliantDubBlocks += lm.CompliantDubbingBlocks
			lDubBlocks += lm.TotalDubbingBlocks
			switch lm.Status {
			case "PASS":
				lRoll.PassCount++
			case "REVIEW_REQUIRED":
				lRoll.ReviewCount++
			case "FAIL":
				lRoll.FailCount++
			}
		}
		if lRefChars > 0 {
			lRoll.OverallCER = float64(lEditDistance) / float64(lRefChars)
		}
		if lDubBlocks > 0 {
			lRoll.DubbingTimingWithin200Ms = float64(lCompliantDubBlocks) / float64(lDubBlocks)
		} else {
			lRoll.DubbingTimingWithin200Ms = 1.0
		}
		rollup.LanguageRollups[lang] = lRoll
	}

	// Enforce hard profile denominator: exactly 48 cases, >= 44 PASS, <= 4 REVIEW_REQUIRED, 0 FAIL.
	if rollup.TotalCases != 48 {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("total cases %d does not match hard profile denominator 48", rollup.TotalCases))
	}
	if rollup.PassCount < 44 {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("pass count %d below required 44", rollup.PassCount))
	}
	if rollup.ReviewCount > 4 {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("review count %d exceeds ceiling 4", rollup.ReviewCount))
	}
	if rollup.FailCount > 0 {
		rollup.GateSatisfied = false
		rollup.FailReasons = append(rollup.FailReasons, fmt.Sprintf("fail count %d exceeds ceiling 0", rollup.FailCount))
	}
	return rollup
}

// BuildProfileRollupForTest exposes buildProfileRollup for unit tests.
func BuildProfileRollupForTest(profile string, metrics []CaseQualityMetrics) *ProfileRollup {
	return buildProfileRollup(profile, metrics)
}
