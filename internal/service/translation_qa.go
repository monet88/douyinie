package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/monet88/douyinie/internal/domain"
)

// MeaningFirstQAGate validates that translations preserve facts, names, numbers, and negation polarity.
type MeaningFirstQAGate struct{}

// NewMeaningFirstQAGate creates a new meaning preservation QA validator.
func NewMeaningFirstQAGate() *MeaningFirstQAGate {
	return &MeaningFirstQAGate{}
}

// QAResult holds the outcome of meaning preservation evaluation for a segment.
type QAResult struct {
	Passed           bool
	Confidence       float64
	ExtractedFacts   []string
	NegationPolarity bool
	Violations       []string
	Err              error
}

var (
	// Regex for digits and floats.
	digitRegex = regexp.MustCompile(`\b\d+(?:\.\d+)?\b|\d+`)

	// Regex for ASCII proper names/brands (e.g. SUPOR, Matcha, iPhone, etc.)
	asciiNameRegex = regexp.MustCompile(`\b[A-Za-z0-9_-]{2,}\b`)
)

// Chinese number characters to integer value.
var zhNumMap = map[rune]int64{
	'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10,
	'百': 100, '千': 1000, '万': 10000,
}

// Words to numbers in Vietnamese. "không" is excluded because it is primarily a negation marker.
var viNumMap = map[string]int64{
	"một": 1, "mốt": 1, "hai": 2, "ba": 3, "bốn": 4, "tư": 4,
	"năm": 5, "lăm": 5, "sáu": 6, "bảy": 7, "tám": 8, "chín": 9, "mười": 10,
	"trăm": 100, "nghìn": 1000, "ngàn": 1000, "triệu": 1000000,
}

// Words to numbers in English.
var enNumMap = map[string]int64{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"hundred": 100, "thousand": 1000, "million": 1000000,
}

// Chinese negation markers.
var zhNegationMarkers = []string{
	"不", "没", "没有", "别", "未", "非", "无", "莫", "勿", "禁",
}

// Words containing negation characters that do not invert polarity.
var zhNegationExceptions = []string{
	"非常", "不仅", "不管", "特别", "非凡", "非洲", "无可挑剔",
}

// Vietnamese negation markers.
var viNegationMarkers = []string{
	"không", "chưa", "chớ", "đừng", "chẳng", "không phải", "không được", "phi", "vô", "cấm",
}

// English negation markers.
var enNegationMarkers = []string{
	"not", "no", "never", "don't", "didn't", "won't", "cannot", "can't",
	"neither", "nor", "none", "without", "prohibit", "forbidden", "shouldn't", "wouldn't",
}

// Known entity translations / transliterations across zh, vi, en.
type EntityMapping struct {
	ZH string
	VI string
	EN string
}

var knownEntities = []EntityMapping{
	{ZH: "张伟", VI: "Trương Vĩ", EN: "Zhang Wei"},
	{ZH: "李雷", VI: "Lý Lôi", EN: "Li Lei"},
	{ZH: "韩梅梅", VI: "Hàn Mai Mai", EN: "Han Meimei"},
	{ZH: "王芳", VI: "Vương Phương", EN: "Wang Fang"},
	{ZH: "SUPOR", VI: "SUPOR", EN: "SUPOR"},
	{ZH: "Matcha", VI: "Matcha", EN: "Matcha"},
	{ZH: "Douyin", VI: "Douyin", EN: "Douyin"},
	{ZH: "TikTok", VI: "TikTok", EN: "TikTok"},
	{ZH: "北京", VI: "Bắc Kinh", EN: "Beijing"},
	{ZH: "上海", VI: "Thượng Hải", EN: "Shanghai"},
}

// ValidateSegment evaluates a single source/target translation pair.
func (g *MeaningFirstQAGate) ValidateSegment(source, target, srcLang, tgtLang string) QAResult {
	src := strings.TrimSpace(source)
	tgt := strings.TrimSpace(target)

	if src == "" {
		return QAResult{Passed: true, Confidence: 1.0}
	}

	// 1. Fact / Non-empty check
	if tgt == "" {
		return QAResult{
			Passed:     false,
			Confidence: 0.0,
			Violations: []string{"empty translation for non-empty source"},
			Err:        fmt.Errorf("%w: target text is empty", domain.ErrFactCorrupted),
		}
	}

	var facts []string
	var violations []string

	// 2. Number Preservation Check
	srcNums := extractNumbers(src, srcLang)
	tgtNums := extractNumbers(tgt, tgtLang)

	for numStr := range srcNums {
		facts = append(facts, "num:"+numStr)
		if !tgtNums[numStr] {
			// Number in source is missing from target
			violations = append(violations, fmt.Sprintf("number '%s' in source missing from target", numStr))
		}
	}

	// Check if target introduced extraneous contradictory numbers
	for numStr := range tgtNums {
		if !srcNums[numStr] && len(srcNums) > 0 {
			violations = append(violations, fmt.Sprintf("target contains unexpected number '%s' not in source", numStr))
		}
	}

	if len(violations) > 0 && len(srcNums) > 0 {
		return QAResult{
			Passed:         false,
			Confidence:     0.2,
			ExtractedFacts: facts,
			Violations:     violations,
			Err:            fmt.Errorf("%w: %s", domain.ErrNumberCorrupted, strings.Join(violations, "; ")),
		}
	}

	// 3. Negation Polarity Check
	srcNeg := detectNegation(src, srcLang)
	tgtNeg := detectNegation(tgt, tgtLang)

	if srcNeg != tgtNeg {
		status := "affirmative -> negative"
		if srcNeg {
			status = "negative -> affirmative"
		}
		violations = append(violations, fmt.Sprintf("negation polarity inverted (%s)", status))
		return QAResult{
			Passed:           false,
			Confidence:       0.1,
			ExtractedFacts:   facts,
			NegationPolarity: srcNeg,
			Violations:       violations,
			Err:              fmt.Errorf("%w: %s", domain.ErrNegationInverted, status),
		}
	}

	if srcNeg {
		facts = append(facts, "polarity:negative")
	} else {
		facts = append(facts, "polarity:affirmative")
	}

	// 4. Named Entities / Brand Preservation Check
	for _, ent := range knownEntities {
		if strings.Contains(src, ent.ZH) {
			facts = append(facts, "entity:"+ent.ZH)
			var expected string
			switch strings.ToLower(tgtLang) {
			case "vi":
				expected = ent.VI
			case "en":
				expected = ent.EN
			default:
				expected = ent.EN
			}
			// Check if target contains the expected translation or exact ZH brand/name
			if !strings.Contains(strings.ToLower(tgt), strings.ToLower(expected)) &&
				!strings.Contains(tgt, ent.ZH) {
				violations = append(violations, fmt.Sprintf("entity '%s' expected '%s' in target", ent.ZH, expected))
			}
		}
	}

	// Also check ASCII tokens in source (e.g. SUPOR, 4K, HD, etc.)
	srcTokens := asciiNameRegex.FindAllString(src, -1)
	for _, tok := range srcTokens {
		// Ignore pure numbers handled above
		if _, err := strconv.ParseFloat(tok, 64); err == nil {
			continue
		}
		if len(tok) >= 3 && unicode.IsUpper(rune(tok[0])) {
			facts = append(facts, "name:"+tok)
			if !strings.Contains(strings.ToLower(tgt), strings.ToLower(tok)) {
				// If not found in knownEntities either
				foundInKnown := false
				for _, ent := range knownEntities {
					if strings.EqualFold(ent.ZH, tok) {
						foundInKnown = true
						break
					}
				}
				if !foundInKnown {
					violations = append(violations, fmt.Sprintf("name/brand '%s' missing from target", tok))
				}
			}
		}
	}

	if len(violations) > 0 {
		return QAResult{
			Passed:           false,
			Confidence:       0.3,
			ExtractedFacts:   facts,
			NegationPolarity: srcNeg,
			Violations:       violations,
			Err:              fmt.Errorf("%w: %s", domain.ErrNameCorrupted, strings.Join(violations, "; ")),
		}
	}

	// High confidence pass
	return QAResult{
		Passed:           true,
		Confidence:       0.98,
		ExtractedFacts:   facts,
		NegationPolarity: srcNeg,
	}
}

// extractNumbers finds all numeric values in a string (both digits and language words).
func extractNumbers(text, lang string) map[string]bool {
	res := make(map[string]bool)

	// 1. Extract Arabic digits
	matches := digitRegex.FindAllString(text, -1)
	for _, m := range matches {
		res[m] = true
	}

	// 2. Language-specific number words
	lower := strings.ToLower(text)
	switch strings.ToLower(lang) {
	case "zh", "zh-cn", "zh-tw":
		for _, r := range text {
			if val, ok := zhNumMap[r]; ok {
				res[strconv.FormatInt(val, 10)] = true
			}
		}
	case "vi":
		words := strings.Fields(lower)
		for _, w := range words {
			w = strings.Trim(w, ",.?!;:'\"()[]{}")
			if val, ok := viNumMap[w]; ok {
				res[strconv.FormatInt(val, 10)] = true
			}
		}
	case "en":
		words := strings.Fields(lower)
		for _, w := range words {
			w = strings.Trim(w, ",.?!;:'\"()[]{}")
			if val, ok := enNumMap[w]; ok {
				res[strconv.FormatInt(val, 10)] = true
			}
		}
	}

	return res
}

// detectNegation checks if the text contains negation markers.
func detectNegation(text, lang string) bool {
	lower := strings.ToLower(text)
	switch strings.ToLower(lang) {
	case "zh", "zh-cn", "zh-tw":
		cleaned := text
		for _, exc := range zhNegationExceptions {
			cleaned = strings.ReplaceAll(cleaned, exc, "")
		}
		for _, marker := range zhNegationMarkers {
			if strings.Contains(cleaned, marker) {
				return true
			}
		}
	case "vi":
		words := strings.Fields(lower)
		for _, w := range words {
			w = strings.Trim(w, ",.?!;:'\"()[]{}")
			for _, marker := range viNegationMarkers {
				if w == marker || strings.Contains(lower, " "+marker+" ") || strings.HasPrefix(lower, marker+" ") {
					return true
				}
			}
		}
	case "en":
		words := strings.Fields(lower)
		for _, w := range words {
			w = strings.Trim(w, ",.?!;:'\"()[]{}")
			for _, marker := range enNegationMarkers {
				if w == marker {
					return true
				}
			}
		}
	}
	return false
}
