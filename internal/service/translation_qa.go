package service

import (
	"fmt"
	"regexp"
	"sort"
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

var zhDigitMap = map[rune]int64{
	'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '兩': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
}

var zhUnitMap = map[rune]int64{
	'十': 10,
	'百': 100,
	'千': 1000,
	'万': 10000,
	'萬': 10000,
}

var viNumberDigits = map[string]int64{
	"không": 0,
	"một":   1,
	"mốt":   1,
	"hai":   2,
	"ba":    3,
	"bốn":   4,
	"tư":    4,
	"năm":   5,
	"lăm":   5,
	"sáu":   6,
	"bảy":   7,
	"tám":   8,
	"chín":  9,
}

var viNumberUnits = map[string]int64{
	"mười":  10,
	"mươi":  10,
	"trăm":  100,
	"nghìn": 1000,
	"ngàn":  1000,
	"triệu": 1000000,
}

var enNumberValues = map[string]int64{
	"zero":      0,
	"one":       1,
	"two":       2,
	"three":     3,
	"four":      4,
	"five":      5,
	"six":       6,
	"seven":     7,
	"eight":     8,
	"nine":      9,
	"ten":       10,
	"eleven":    11,
	"twelve":    12,
	"thirteen":  13,
	"fourteen":  14,
	"fifteen":   15,
	"sixteen":   16,
	"seventeen": 17,
	"eighteen":  18,
	"nineteen":  19,
	"twenty":    20,
	"thirty":    30,
	"forty":     40,
	"fifty":     50,
	"sixty":     60,
	"seventy":   70,
	"eighty":    80,
	"ninety":    90,
}

var enNumberUnits = map[string]int64{
	"hundred":  100,
	"thousand": 1000,
	"million":  1000000,
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

// SemanticFactAnchor is a conservative multilingual lexical anchor for facts
// that can be validated deterministically without asking the translation
// provider to self-attest to its own output. It is intentionally a cheap gate:
// richer semantic reflection can add coverage later without weakening these
// deterministic corruption checks.
type SemanticFactAnchor struct {
	Key   string
	Value string
	ZH    []string
	VI    []string
	EN    []string
}

var semanticFactAnchors = []SemanticFactAnchor{
	{
		Key:   "weather_quality",
		Value: "good",
		ZH:    []string{"天气很好", "天气非常好", "好天气"},
		VI:    []string{"thời tiết rất tốt", "thời tiết tốt", "thời tiết đẹp"},
		EN:    []string{"weather is very good", "weather is good", "good weather"},
	},
	{
		Key:   "weather_quality",
		Value: "bad",
		ZH:    []string{"天气很差", "天气非常差", "天气不好"},
		VI:    []string{"thời tiết rất tệ", "thời tiết tệ", "thời tiết xấu"},
		EN:    []string{"weather is very bad", "weather is bad", "bad weather"},
	},
	{
		Key:   "window_action",
		Value: "open",
		ZH:    []string{"打开窗户", "开窗"},
		VI:    []string{"mở cửa sổ"},
		EN:    []string{"open the window", "open window"},
	},
	{
		Key:   "window_action",
		Value: "close",
		ZH:    []string{"关闭窗户", "关上窗户", "关窗"},
		VI:    []string{"đóng cửa sổ"},
		EN:    []string{"close the window", "shut the window", "close window"},
	},
	{
		Key:   "color",
		Value: "red",
		ZH:    []string{"红色", "红的"},
		VI:    []string{"màu đỏ"},
		EN:    []string{"red"},
	},
	{
		Key:   "color",
		Value: "blue",
		ZH:    []string{"蓝色", "藍色", "蓝的", "藍的"},
		VI:    []string{"màu xanh dương", "màu xanh lam"},
		EN:    []string{"blue"},
	},
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

	// 2. Semantic Fact Preservation Check
	srcSemanticFacts := extractSemanticFacts(src, srcLang)
	tgtSemanticFacts := extractSemanticFacts(tgt, tgtLang)
	semanticKeys := sortedStringKeys(srcSemanticFacts)
	for _, key := range semanticKeys {
		srcValue := srcSemanticFacts[key]
		facts = append(facts, "fact:"+key+"="+srcValue)
		tgtValue, ok := tgtSemanticFacts[key]
		if !ok {
			violations = append(violations, fmt.Sprintf("fact '%s=%s' in source missing from target", key, srcValue))
			continue
		}
		if tgtValue != srcValue {
			violations = append(violations, fmt.Sprintf("fact '%s' changed from '%s' to '%s'", key, srcValue, tgtValue))
		}
	}
	if len(violations) > 0 {
		return QAResult{
			Passed:         false,
			Confidence:     0.2,
			ExtractedFacts: facts,
			Violations:     violations,
			Err:            fmt.Errorf("%w: %s", domain.ErrFactCorrupted, strings.Join(violations, "; ")),
		}
	}

	// 3. Number Preservation Check
	srcNums := extractNumbers(src, srcLang)
	tgtNums := extractNumbers(tgt, tgtLang)

	for _, numStr := range sortedBoolKeys(srcNums) {
		facts = append(facts, "num:"+numStr)
		if !tgtNums[numStr] {
			// Number in source is missing from target
			violations = append(violations, fmt.Sprintf("number '%s' in source missing from target", numStr))
		}
	}

	// Check if target introduced extraneous contradictory numbers
	for _, numStr := range sortedBoolKeys(tgtNums) {
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

	// 4. Negation Polarity Check
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

	// 5. Named Entities / Brand Preservation Check
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
		for _, token := range extractChineseNumberTokens(text) {
			if val, ok := parseChineseNumberToken(token); ok {
				res[strconv.FormatInt(val, 10)] = true
			}
		}
	case "vi":
		for _, val := range extractVietnameseNumberWords(lower) {
			res[strconv.FormatInt(val, 10)] = true
		}
	case "en":
		for _, val := range extractEnglishNumberWords(lower) {
			res[strconv.FormatInt(val, 10)] = true
		}
	}

	return res
}

func extractSemanticFacts(text, lang string) map[string]string {
	res := make(map[string]string)
	lower := strings.ToLower(text)
	for _, anchor := range semanticFactAnchors {
		var phrases []string
		switch strings.ToLower(lang) {
		case "zh", "zh-cn", "zh-tw":
			phrases = anchor.ZH
		case "vi":
			phrases = anchor.VI
		case "en":
			phrases = anchor.EN
		default:
			continue
		}

		for _, phrase := range phrases {
			if strings.Contains(lower, strings.ToLower(phrase)) {
				res[anchor.Key] = anchor.Value
				break
			}
		}
	}
	return res
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func extractChineseNumberTokens(text string) []string {
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		tokens = append(tokens, string(current))
		current = current[:0]
	}

	for _, r := range text {
		if isChineseNumberRune(r) {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

func isChineseNumberRune(r rune) bool {
	if _, ok := zhDigitMap[r]; ok {
		return true
	}
	_, ok := zhUnitMap[r]
	return ok
}

func parseChineseNumberToken(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}

	hasUnit := false
	for _, r := range token {
		if _, ok := zhDigitMap[r]; ok {
			continue
		}
		if _, ok := zhUnitMap[r]; ok {
			hasUnit = true
			continue
		}
		return 0, false
	}

	if !hasUnit {
		var value int64
		for _, r := range token {
			value = value*10 + zhDigitMap[r]
		}
		return value, true
	}

	var total int64
	var section int64
	var number int64
	for _, r := range token {
		if digit, ok := zhDigitMap[r]; ok {
			number = digit
			continue
		}

		unit := zhUnitMap[r]
		if unit == 10000 {
			section += number
			if section == 0 {
				section = 1
			}
			total += section * unit
			section = 0
			number = 0
			continue
		}

		if number == 0 {
			number = 1
		}
		section += number * unit
		number = 0
	}

	return total + section + number, true
}

func tokenizeNumberWords(text string) []string {
	replacer := strings.NewReplacer(
		"-", " ",
		",", " ",
		".", " ",
		"?", " ",
		"!", " ",
		";", " ",
		":", " ",
		"'", " ",
		"\"", " ",
		"(", " ",
		")", " ",
		"[", " ",
		"]", " ",
		"{", " ",
		"}", " ",
	)
	return strings.Fields(replacer.Replace(strings.ToLower(text)))
}

func extractEnglishNumberWords(text string) []int64 {
	words := tokenizeNumberWords(text)
	var values []int64
	for i := 0; i < len(words); {
		value, consumed, ok := parseEnglishNumberAt(words, i)
		if !ok {
			i++
			continue
		}
		values = append(values, value)
		i += consumed
	}
	return values
}

func parseEnglishNumberAt(words []string, start int) (int64, int, bool) {
	var total int64
	var current int64
	consumed := 0
	for i := start; i < len(words); i++ {
		word := words[i]
		if value, ok := enNumberValues[word]; ok {
			current += value
			consumed++
			continue
		}

		unit, ok := enNumberUnits[word]
		if !ok {
			break
		}
		consumed++
		if unit == 100 {
			if current == 0 {
				current = 1
			}
			current *= unit
			continue
		}
		if current == 0 {
			current = 1
		}
		total += current * unit
		current = 0
	}
	if consumed == 0 {
		return 0, 0, false
	}
	return total + current, consumed, true
}

func extractVietnameseNumberWords(text string) []int64 {
	words := tokenizeNumberWords(text)
	var values []int64
	for i := 0; i < len(words); {
		value, consumed, ok := parseVietnameseNumberAt(words, i)
		if !ok {
			i++
			continue
		}
		values = append(values, value)
		i += consumed
	}
	return values
}

func parseVietnameseNumberAt(words []string, start int) (int64, int, bool) {
	// "không" is primarily a negation marker in Vietnamese. Do not start a
	// numeric sequence from it; it is accepted only as an internal zero/filler
	// once another numeric word has established numeric context.
	if start >= len(words) || words[start] == "không" || words[start] == "linh" || words[start] == "lẻ" {
		return 0, 0, false
	}

	var total int64
	var section int64
	var number int64
	consumed := 0
	hasNumber := false

	for i := start; i < len(words); i++ {
		word := words[i]
		if word == "linh" || word == "lẻ" {
			if !hasNumber {
				break
			}
			consumed++
			continue
		}
		if digit, ok := viNumberDigits[word]; ok {
			if word == "không" && !hasNumber {
				break
			}
			number = digit
			hasNumber = true
			consumed++
			continue
		}

		unit, ok := viNumberUnits[word]
		if !ok {
			break
		}
		hasNumber = true
		consumed++
		if unit >= 1000 {
			section += number
			if section == 0 {
				section = 1
			}
			total += section * unit
			section = 0
			number = 0
			continue
		}
		if number == 0 {
			number = 1
		}
		section += number * unit
		number = 0
	}

	if consumed == 0 || !hasNumber {
		return 0, 0, false
	}
	return total + section + number, consumed, true
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
