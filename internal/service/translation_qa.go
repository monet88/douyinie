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

	// Regex for long Arabic digit sequences formatted as 3-digit groups (e.g. 909 999 520).
	threeDigitGroupRegex = regexp.MustCompile(`\b\d{3}(?:[ ]+\d{3})+\b`)

	// Regex for explicit clock ranges. Compact HMM/HHMM endpoints are accepted only
	// inside a range, so an arbitrary value such as "900" is never reinterpreted as
	// a time by itself.
	clockRangeRegex = regexp.MustCompile(`\b(\d{1,2}:\d{2}|\d{3,4})\s*[-–—~至到]\s*(\d{1,2}:\d{2}|\d{3,4})\b`)

	// Regex for ASCII proper names/brands (e.g. SUPOR, Matcha, iPhone, etc.)
	asciiNameRegex = regexp.MustCompile(`\b[A-Za-z0-9_-]{2,}\b`)

	// Regex for quantity/measurement unit suffixes attached to numbers (e.g. 3MINUTE, 20KG, 500ML).
	// These are quantities/units that should be naturally translated or preserved as numbers,
	// not proper names/brands that must remain verbatim.
	asciiQuantityTokenRegex = regexp.MustCompile(`^(?i)\d+(?:MINUTE|MINUTES|MIN|SEC|SECOND|SECONDS|HR|HOUR|HOURS|DAY|DAYS|WEEK|WEEKS|MONTH|MONTHS|YEAR|YEARS|KM|M|CM|MM|KG|G|ML|L|CUP|CUPS|TSP|TBSP|TEASPOON|TEASPOONS|TABLESPOON|TABLESPOONS)$`)
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

// zhQuantityMarkers establishes explicit quantity context for a standalone
// single-digit Chinese numeral: classifiers, containers/measures, time units,
// core frequency counts, and money. A lone digit rune is otherwise ambiguous
// in Chinese (aspectual 一+verb like 一照/一拉, adverbials like 一下/一直/
// 一样/一点, ordinals like 第一, labels like 话一) and must not become a hard
// numeric fact on its own. Multi-rune numerals (二十, 十二) stay unconditional,
// and Arabic digits are untouched. Deliberately excluded: action-count
// 下/刀/眼/手/口 (一下 aspectual dominates), 点 (一点 "a bit" dominates 八点
// o'clock), and 道/集/幕/封/场/顿/串 (naturalize to "a X"). 种/样/些/般 count
// with other digits (三种) but never with 一 (一样 "same", 一些 "some").
// Missing a real drop there fails open; requiring one there fails closed on
// valid translations, which is worse.
var zhQuantityMarkers = map[rune]bool{
	// General classifiers.
	'个': true, '個': true, '位': true, '只': true, '隻': true,
	'条': true, '條': true, '张': true, '張': true, '件': true,
	'本': true, '支': true, '块': true, '塊': true, '份': true,
	'套': true, '双': true, '雙': true, '对': true, '對': true,
	'匹': true, '头': true, '頭': true, '辆': true, '輛': true,
	'架': true, '台': true, '部': true, '座': true,
	'間': true, '颗': true, '顆': true, '粒': true, '篇': true,
	'章': true, '节': true, '節': true, '段': true, '层': true,
	'層': true, '首': true, '句': true, '格': true,
	// Containers and measures.
	'杯': true, '碗': true, '盘': true, '盤': true, '瓶': true,
	'罐': true, '袋': true, '包': true, '盒': true, '箱': true,
	'桶': true, '盆': true, '锅': true, '鍋': true,
	// Time units.
	'秒': true, '分': true, '时': true, '時': true, '天': true,
	'周': true, '月': true, '年': true, '季': true, '夜': true,
	'晚': true,
	// Kind markers: quantitative with other digits (三种), never with 一
	// (一样 "same", 一些 "some"); see zhYiOnlyNonQuantity.
	'种': true, '種': true, '样': true, '樣': true,
	'些': true, '般': true,
	// Core frequency counts (explicit "N times" almost always survives).
	'次': true, '遍': true, '趟': true, '回': true,
	// Money and weight.
	'元': true, '毛': true, '斤': true,
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
	"zero": 0, "one": 1, "two": 2,
	// Unambiguous frequency-count words only; the general gate still
	// rejects missing or contradictory numbers.
	"once":      1,
	"twice":     2,
	"thrice":    3,
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
// Note: "没" (U+6CA1, simplified) and "沒" (U+6C92, traditional) are both
// negators. Visual-track OCR and mixed-script sources may use either form;
// the gate must recognize both, otherwise correctly-negative translations are
// rejected and genuine inversions slip through.
var zhNegationMarkers = []string{
	"不", "没", "沒", "没有", "別", "别", "未", "非", "无", "無", "莫", "勿", "禁",
}

// Words containing negation characters that do not invert polarity.
var zhNegationExceptions = []string{
	// Non-negating words containing 别
	"告别", "别人", "区别", "类别", "性别", "级别", "个别", "辨别", "分别",
	"道别", "送别", "久别", "作别", "辞别", "阔别", "甄别", "判别", "离别", "永别", "惜别",
	// A-not-A question tags and affirmative-negative rhetorical questions
	"是不是", "可不可以", "能不能", "好不好", "行不行", "要不要", "会不会", "有没有", "对不对", "对不对吧", "对不", "对吧", "行不", "好不",
	"不得不", "不得不说", "不仅如此",
	// Non-negating words containing 无
	"无聊", "无论", "无论如何", "无奈", "无数", "无所谓", "无辜", "无端", "无暇", "无微不至", "无独有偶",
	// Non-negating words containing 不
	"不小心", "不过", "不管", "不仅", "不得了", "不由得", "不料", "不经意", "不良", "不断", "不愧", "不知不觉", "不在话下", "差不多", "并不",
	// Existing exceptions
	"非常", "不仅", "不管", "特别", "非凡", "非洲", "无可挑剔", "是非",
	// Non-negating words containing 没
	"沉没", "淹没", "埋没",
}

// Vietnamese grammatical negation markers.
// Note: Bound Sino-Vietnamese morphemes like "phi" or "vô" are derivational affixes
// (e.g. "phi pháp", "phi lý", "vô hiệu"), not standalone clausal negators. In isolation,
// "Phi" is frequently a proper noun / personal name ("Phi", "Huy Phi", "Đức Phi", "Phi Hùng")
// or verb ("bay/phi"). Standalone "phi" must NOT be treated as sentence-level negation.
var viNegationMarkers = []string{
	"không", "chưa", "chớ", "đừng", "chẳng", "không phải", "không được", "cấm",
}

// Bound morphological negation prefixes in Sino-Vietnamese compounds.
var viBoundNegationPrefixes = []string{
	"phi pháp", "phi lý", "phi nghĩa", "phi thực tế", "phi nhân", "phi chính phủ",
	"vô lý", "vô hiệu", "vô nghĩa", "vô vọng", "vô căn cứ", "vô đạo", "vô phương",
}

// Vietnamese phrases that do not invert polarity (rhetorical question particles, additives, question tags).
var viNegationExceptions = []string{
	"chẳng lẽ", "không lẽ", "không những", "không chỉ", "vô cùng", "vô số", "bất ngờ", "bất kể", "vô tư", "không ngừng",
	"có phải không", "được không", "phải không", "đúng không",
}

// English phrases that do not invert polarity.
var enNegationExceptions = []string{
	"not only", "no matter", "no wonder",
}

// Japanese negation markers for mixed-audio/song transcription.
var jaNegationMarkers = []string{
	"ありません", "ない", "なかった", "ません", "ず", "ぬ",
}

var enNegationMarkers = []string{
	"not", "no", "never", "don't", "didn't", "won't", "cannot", "can't",
	"neither", "nor", "none", "without", "prohibit", "forbidden", "shouldn't", "wouldn't",
	"nowhere", "nothing", "nobody", "hardly", "scarcely",
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
	}

	srcMissing := make(map[string]bool)
	for numStr := range srcNums {
		if !tgtNums[numStr] {
			srcMissing[numStr] = true
		}
	}
	tgtUnexpected := make(map[string]bool)
	for numStr := range tgtNums {
		if !srcNums[numStr] {
			tgtUnexpected[numStr] = true
		}
	}

	if len(srcMissing) > 0 || len(tgtUnexpected) > 0 {
		// Conservative equivalence only for long Arabic digit sequences (>= 6 digits) split or merged into 3-digit groups.
		for _, m := range threeDigitGroupRegex.FindAllString(tgt, -1) {
			parts := strings.Fields(m)
			merged := strings.Join(parts, "")
			if len(merged) >= 6 && srcMissing[merged] {
				allPartsPresent := true
				for _, p := range parts {
					if !tgtNums[p] {
						allPartsPresent = false
						break
					}
				}
				if allPartsPresent {
					delete(srcMissing, merged)
					for _, p := range parts {
						delete(tgtUnexpected, p)
					}
				}
			}
		}

		for _, m := range threeDigitGroupRegex.FindAllString(src, -1) {
			parts := strings.Fields(m)
			merged := strings.Join(parts, "")
			if len(merged) >= 6 && tgtUnexpected[merged] {
				allPartsPresent := true
				for _, p := range parts {
					if !srcNums[p] {
						allPartsPresent = false
						break
					}
				}
				if allPartsPresent {
					delete(tgtUnexpected, merged)
					for _, p := range parts {
						delete(srcMissing, p)
					}
				}
			}
		}

		// A provider may normalize compact clock notation while preserving the
		// exact time range, e.g. 900-17:00 -> 9:00-17:00. Reconcile only when a
		// valid canonical clock range exists on both sides; arbitrary numbers are
		// intentionally unaffected.
		reconcileEquivalentClockRanges(src, tgt, srcMissing, tgtUnexpected)
	}

	for _, numStr := range sortedBoolKeys(srcMissing) {
		violations = append(violations, fmt.Sprintf("number '%s' in source missing from target", numStr))
	}
	for _, numStr := range sortedBoolKeys(tgtUnexpected) {
		if len(srcNums) > 0 {
			// In natural speech (especially Vietnamese indefinite 'một' and English 'one'/'once'/'a'),
			// a singular indefinite marker is often naturally added (e.g. "một con lợn", "một đại mỹ nhân", "một chút")
			// without the source having a numeral '1'. If all source numbers are preserved (srcMissing is empty),
			// an unexpected singular/indefinite '1' does not constitute factual quantity corruption.
			if numStr == "1" && len(srcMissing) == 0 {
				continue
			}
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

	// Also check ASCII tokens in source (e.g. SUPOR, 4K, HD, CAE, etc.)
	srcTokens := asciiNameRegex.FindAllString(src, -1)
	for _, tok := range srcTokens {
		// Ignore pure numbers handled above
		if _, err := strconv.ParseFloat(tok, 64); err == nil {
			continue
		}
		if isProtectedASCIIToken(tok) {
			facts = append(facts, "name:"+tok)
			lowerTgt := strings.ToLower(tgt)
			lowerTok := strings.ToLower(tok)
			if !strings.Contains(lowerTgt, lowerTok) && !strings.Contains(stripDiacritics(lowerTgt), stripDiacritics(lowerTok)) {
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

func reconcileEquivalentClockRanges(source, target string, srcMissing, tgtUnexpected map[string]bool) {
	srcRanges := extractClockRangeNumberTokens(source)
	tgtRanges := extractClockRangeNumberTokens(target)
	for canonical, srcTokens := range srcRanges {
		tgtTokens, ok := tgtRanges[canonical]
		if !ok {
			continue
		}
		for token := range srcTokens {
			delete(srcMissing, token)
		}
		for token := range tgtTokens {
			delete(tgtUnexpected, token)
		}
	}
}

func extractClockRangeNumberTokens(text string) map[string]map[string]bool {
	ranges := make(map[string]map[string]bool)
	for _, match := range clockRangeRegex.FindAllStringSubmatch(text, -1) {
		if len(match) != 3 {
			continue
		}
		start, ok := canonicalClockEndpoint(match[1])
		if !ok {
			continue
		}
		end, ok := canonicalClockEndpoint(match[2])
		if !ok {
			continue
		}
		key := start + "-" + end
		tokens := ranges[key]
		if tokens == nil {
			tokens = make(map[string]bool)
			ranges[key] = tokens
		}
		for _, token := range digitRegex.FindAllString(match[0], -1) {
			tokens[token] = true
		}
	}
	return ranges
}

func canonicalClockEndpoint(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	var hourText, minuteText string
	if strings.Contains(raw, ":") {
		parts := strings.Split(raw, ":")
		if len(parts) != 2 || len(parts[0]) < 1 || len(parts[0]) > 2 || len(parts[1]) != 2 {
			return "", false
		}
		hourText, minuteText = parts[0], parts[1]
	} else {
		switch len(raw) {
		case 3:
			hourText, minuteText = raw[:1], raw[1:]
		case 4:
			hourText, minuteText = raw[:2], raw[2:]
		default:
			return "", false
		}
	}

	hour, err := strconv.Atoi(hourText)
	if err != nil || hour < 0 || hour > 23 {
		return "", false
	}
	minute, err := strconv.Atoi(minuteText)
	if err != nil || minute < 0 || minute > 59 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), true
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
	runes := []rune(text)
	var tokens []string
	var current []rune
	start := -1
	flush := func(end int) {
		if len(current) == 0 {
			return
		}
		// Ordinals (第一, 第十名, 第十二) never count, at any token length.
		if p := prevNonSpace(runes, start); p >= 0 && runes[p] == '第' {
			current = current[:0]
			start = -1
			return
		}
		if !isBareChineseDigit(current) || hasQuantityContext(runes, start, end) {
			tokens = append(tokens, string(current))
		}
		current = current[:0]
		start = -1
	}

	for i, r := range runes {
		if isChineseNumberRune(r) {
			if start < 0 {
				start = i
			}
			current = append(current, r)
			continue
		}
		if unicode.IsSpace(r) && start >= 0 && bridgesNumeralRun(current, runes, i) {
			// Transparent space inside one numeral (二 十 秒): keep the run open.
			continue
		}
		flush(i)
	}
	flush(len(runes))
	return tokens
}

// isBareChineseDigit reports a single digit rune with no unit (一, 两, 三...).
// Only these need quantity context; longer runs (二十) are unambiguous.
func isBareChineseDigit(token []rune) bool {
	if len(token) != 1 {
		return false
	}
	_, ok := zhDigitMap[token[0]]
	return ok
}

// bridgesNumeralRun reports whether spaces at pos sit inside one numeral: the
// open run is non-empty and a unit rune touches the gap on either side, so
// 二 十 秒 reads as 二十 while enumerations like 一 二 三 stay separate.
func bridgesNumeralRun(current []rune, runes []rune, pos int) bool {
	if len(current) == 0 {
		return false
	}
	next := nextNonSpace(runes, pos)
	if next < 0 || !isChineseNumberRune(runes[next]) {
		return false
	}
	if _, ok := zhUnitMap[runes[next]]; ok {
		return true
	}
	_, ok := zhUnitMap[current[len(current)-1]]
	return ok
}

// hasQuantityContext reports whether the bare digit at runes[start:end] heads
// an explicit quantity: the next non-space rune is a quantity marker, the
// digit is not an ordinal (第一), and the pair is not time/frequency 一X一Y
// distributive reduplication (一分一秒), which is adverbial, not counting.
// Classifier pairs like 一個一架 stay quantitative, and the reduplication
// guard is 一-specific: 三天三夜 counts twice, correctly.
func hasQuantityContext(runes []rune, start, end int) bool {
	next := nextNonSpace(runes, end)
	if next < 0 {
		return false
	}
	marked := zhQuantityMarkers[runes[next]]
	if !marked && runes[next] == '小' {
		// 小时/小時 ("hour") is a lexicalized time word: 八小时 counts,
		// while 一小打/六小黄 stay out.
		if after := nextNonSpace(runes, next+1); after >= 0 && (runes[after] == '时' || runes[after] == '時') {
			marked = true
		}
	}
	if !marked {
		return false
	}
	if runes[start] != '一' {
		return true
	}
	// 一种/一样/一些/一般 never count (三种/两样 do); other markers count.
	if zhYiOnlyNonQuantity[runes[next]] {
		return false
	}
	if m, ok := prevPairMarker(runes, start); ok && zhTimeFreqMarkers[m] {
		return false
	}
	if m, ok := nextPairMarker(runes, next); ok && zhTimeFreqMarkers[m] {
		return false
	}
	return true
}

// zhTimeFreqMarkers are markers whose 一X一Y reduplication is distributive
// (一分一秒, 一天一天), never counting.
var zhTimeFreqMarkers = map[rune]bool{
	'秒': true, '分': true, '时': true, '時': true, '天': true,
	'周': true, '月': true, '年': true, '季': true, '夜': true,
	'晚': true, '次': true, '遍': true, '趟': true, '回': true,
}

// zhYiOnlyNonQuantity are markers that never form quantities with 一
// (一种 "a kind of", 一样 "same", 一些 "some", 一般 "generally")
// but do count with other digits (三种 "three kinds", 两样 "two kinds").
var zhYiOnlyNonQuantity = map[rune]bool{
	'种': true, '種': true, '样': true, '樣': true,
	'些': true, '般': true,
}

// prevPairMarker returns the X of a 一X pair immediately before pos.
func prevPairMarker(runes []rune, pos int) (rune, bool) {
	a := prevNonSpace(runes, pos)
	if a < 0 {
		return 0, false
	}
	b := prevNonSpace(runes, a)
	if b >= 0 && runes[b] == '一' {
		return runes[a], true
	}
	return 0, false
}

// nextPairMarker returns the X of a 一X pair immediately after pos.
func nextPairMarker(runes []rune, pos int) (rune, bool) {
	a := nextNonSpace(runes, pos+1)
	if a < 0 {
		return 0, false
	}
	b := nextNonSpace(runes, a+1)
	if b >= 0 && runes[a] == '一' {
		return runes[b], true
	}
	return 0, false
}

func nextNonSpace(runes []rune, pos int) int {
	for i := pos; i < len(runes); i++ {
		if !unicode.IsSpace(runes[i]) {
			return i
		}
	}
	return -1
}

func prevNonSpace(runes []rune, pos int) int {
	for i := pos - 1; i >= 0; i-- {
		if !unicode.IsSpace(runes[i]) {
			return i
		}
	}
	return -1
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
		cjkClean := stripCJKSpaces(text)
		if containsJapaneseKana(cjkClean) {
			for _, marker := range jaNegationMarkers {
				if strings.Contains(cjkClean, marker) {
					return true
				}
			}
		}
		cleaned := cjkClean
		for _, exc := range zhNegationExceptions {
			cleaned = strings.ReplaceAll(cleaned, exc, "")
		}
		// A-not-A or interrogative question particles (e.g. "好不", "行不", "对不", "喜欢你不", "...不")
		// where "不" is at the end of a sentence or at a clause boundary before a new subject/discourse clause
		// (which frequently occurs without punctuation in ASR streams) functions as a confirmation /
		// tag-question request ("right?", "isn't it?") rather than semantic negation.
		// We identify these clause-final interrogative occurrences of "不" and strip them from negation evaluation.
		runes := []rune(cleaned)
		var filtered strings.Builder
		filtered.Grow(len(cleaned))
		n := len(runes)
		for i := 0; i < n; i++ {
			if runes[i] == '不' {
				// Check if this '不' is an interrogative clause-final tag particle:
				// 1. Preceded by a predicate/clause constituent (i > 0, e.g. 好, 行, 对, 来, 去, 喜欢你...)
				// 2. Either at the end of the text (ignoring trailing punctuation/spaces), OR
				//    followed by punctuation, OR
				//    followed immediately in unpunctuated ASR by a clause-starting pronoun/discourse marker:
				//    你, 我, 他, 她, 它, 咱, 咱们, 我们, 你们, 他们, 这, 那, 谁, 走, 如果, 要是, 既然...
				isSentenceEnd := true
				for j := i + 1; j < n; j++ {
					if !unicode.IsPunct(runes[j]) && !unicode.IsSpace(runes[j]) {
						isSentenceEnd = false
						break
					}
				}
				isClauseBoundary := isSentenceEnd
				if !isClauseBoundary && i > 0 && i+1 < n {
					// Check if next rune is punctuation or space
					if unicode.IsPunct(runes[i+1]) || unicode.IsSpace(runes[i+1]) {
						isClauseBoundary = true
					} else {
						// Unpunctuated ASR boundary: followed by new clause opener (pronoun/demonstrative/discourse starter)
						nextR := runes[i+1]
						switch nextR {
						case '你', '我', '他', '她', '它', '咱', '这', '那', '谁':
							isClauseBoundary = true
						}
					}
				}
				// An interrogative tag particle follows a non-negating predicate/pronoun,
				// not another negation marker (e.g. "并不", "决不", "绝不" keep normal negation).
				if isClauseBoundary && i > 0 && runes[i-1] != '并' && runes[i-1] != '绝' && runes[i-1] != '决' {
					// Omit this interrogative tag particle from negation detection
					continue
				}
			}
			filtered.WriteRune(runes[i])
		}
		cleaned = filtered.String()
		for _, marker := range zhNegationMarkers {
			if strings.Contains(cleaned, marker) {
				return true
			}
		}
	case "vi":
		cleaned := lower
		for _, exc := range viNegationExceptions {
			cleaned = strings.ReplaceAll(cleaned, exc, "")
		}
		for _, prefix := range viBoundNegationPrefixes {
			if strings.Contains(cleaned, prefix) {
				return true
			}
		}
		words := strings.Fields(cleaned)
		for _, w := range words {
			w = strings.Trim(w, ",.?!;:'\"()[]{}")
			for _, marker := range viNegationMarkers {
				if w == marker || strings.Contains(cleaned, " "+marker+" ") || strings.HasPrefix(cleaned, marker+" ") {
					return true
				}
			}
		}
	case "en":
		cleaned := lower
		for _, exc := range enNegationExceptions {
			cleaned = strings.ReplaceAll(cleaned, exc, "")
		}
		words := strings.Fields(cleaned)
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

func containsJapaneseKana(s string) bool {
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) {
			return true
		}
	}
	return false
}

func isCJKOrKana(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Extension A
		(r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0x3000 && r <= 0x303F) || // CJK Symbols and Punctuation
		(r >= 0xFF00 && r <= 0xFFEF) // Fullwidth forms
}

func stripCJKSpaces(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range runes {
		if unicode.IsSpace(r) {
			prevCJK := i > 0 && isCJKOrKana(runes[i-1])
			nextCJK := i+1 < len(runes) && isCJKOrKana(runes[i+1])
			if prevCJK && nextCJK {
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ExtractNumbers finds all numeric values in a string (both digits and language words) as canonical decimal string representations.
func ExtractNumbers(text, lang string) map[string]bool {
	return extractNumbers(text, lang)
}

// DetectNegation checks if the text contains language-appropriate negation markers.
func DetectNegation(text, lang string) bool {
	return detectNegation(text, lang)
}

func isProtectedASCIIToken(tok string) bool {
	if len(tok) < 2 {
		return false
	}

	// Known entities are always protected
	for _, ent := range knownEntities {
		if strings.EqualFold(ent.ZH, tok) {
			return true
		}
	}

	hasDigit := false
	hasUpper := false
	hasLower := false
	for _, r := range tok {
		if unicode.IsDigit(r) {
			hasDigit = true
		} else if unicode.IsUpper(r) {
			hasUpper = true
		} else if unicode.IsLower(r) {
			hasLower = true
		}
	}

	// Digit-bearing token with letters (e.g. 4K, MP4, H264).
	// Exclude pure quantity+unit compounds like 3MINUTE, 20KG, 500ML which are
	// measurements/durations rather than proprietary names/brands.
	if hasDigit && (hasUpper || hasLower) {
		if isQuantityToken(tok) {
			return false
		}
		return true
	}

	// Internal camel/mixed-case tokens with an uppercase letter after the first rune
	// (e.g. iPhone, YouTube, WeChat, DouYin).
	// Plain TitleCase (e.g. Shift, Total) where only the first rune is uppercase
	// is not protected solely by casing.
	if hasUpper && hasLower {
		upper := strings.ToUpper(tok)
		if strings.HasSuffix(upper, "WATER") ||
			strings.HasSuffix(upper, "COFFEE") ||
			strings.HasSuffix(upper, "TEA") ||
			strings.HasSuffix(upper, "JUICE") ||
			hasConsecutiveUpper(tok, 3) {
			return false
		}

		runes := []rune(tok)
		for _, r := range runes[1:] {
			if unicode.IsUpper(r) {
				return true
			}
		}
	}

	// All-caps tokens:
	// Short acronyms (2-3 chars, e.g. HD, CAE, AI, 4K, 5G) are protected.
	// Common lexical English words (e.g. SOY, TEA, ICE, RED, BIG, ONE) and ordinary
	// all-caps words (>= 4 chars like TOTAL, DAMAGE) are not protected
	// solely because of uppercase formatting unless listed in knownEntities (e.g. SUPOR).
	if hasUpper && !hasLower && !hasDigit {
		if len(tok) <= 3 {
			if isCommonEnglishLexicalWord(tok) {
				return false
			}
			return true
		}
	}

	return false
}

func hasConsecutiveUpper(s string, n int) bool {
	count := 0
	for _, r := range s {
		if unicode.IsUpper(r) {
			count++
			if count >= n {
				return true
			}
		} else {
			count = 0
		}
	}
	return false
}

func isCommonEnglishLexicalWord(tok string) bool {
	return commonLexicalWords3[strings.ToUpper(tok)]
}

var commonLexicalWords3 = map[string]bool{
	"SOY": true, "TEA": true, "EGG": true, "ICE": true, "HOT": true, "RED": true,
	"OIL": true, "PAN": true, "POT": true, "BOX": true, "CUP": true, "BAG": true,
	"BAR": true, "CAN": true, "JAM": true, "NUT": true, "PIE": true, "RAW": true,
	"BIG": true, "TOP": true, "FOR": true, "AND": true, "THE": true, "NEW": true,
	"BUY": true, "ONE": true, "TWO": true, "SIX": true, "TEN": true, "DAY": true,
	"CAT": true, "DOG": true, "PIG": true, "COW": true, "SUN": true, "AIR": true,
	"SEA": true, "CAR": true, "BUS": true, "BED": true, "BOY": true, "MAN": true,
	"WAR": true, "FIT": true, "GET": true, "LET": true, "RUN": true, "SIT": true,
	"WIN": true, "YES": true, "OFF": true, "OUT": true, "WAY": true, "KEY": true,
	"LAW": true, "EYE": true, "EAR": true, "ARM": true, "LEG": true, "LIP": true,
	"TOE": true, "GAP": true, "TIP": true, "LID": true, "CAP": true, "JAR": true,
	"PIN": true, "TIN": true, "DIP": true, "MIX": true, "FRY": true, "FAT": true,
	"DRY": true, "WET": true, "SPA": true, "PUB": true, "GYM": true, "ZOO": true,
	"VAN": true, "CAB": true, "JET": true, "MAP": true, "PEN": true, "TAG": true,
	"PAD": true, "SET": true, "BIT": true, "ROW": true, "DOT": true, "WEB": true,
	"APP": true, "SEE": true, "EAT": true, "OLD": true, "BAD": true, "FAR": true,
	"FEW": true, "LOW": true, "END": true, "ACT": true, "ADD": true, "AGE": true,
	"AGO": true, "AID": true, "AIM": true, "ALL": true, "ANY": true, "ARE": true,
	"ASK": true, "BEG": true, "BET": true, "BID": true, "BOW": true, "BUG": true,
	"BUT": true, "CRY": true, "CUE": true, "CUT": true, "DAM": true, "DIE": true,
	"DIG": true, "DIM": true, "DUE": true, "DYE": true, "EGO": true, "ELM": true,
	"ERA": true, "EVE": true, "FAN": true, "FAX": true, "FEE": true, "FIG": true,
	"FIX": true, "FLY": true, "FOG": true, "FOX": true, "FUN": true, "FUR": true,
	"GAS": true, "GEL": true, "GEM": true, "GOD": true, "GUN": true, "GUT": true,
	"GUY": true, "HAM": true, "HAT": true, "HAY": true, "HEN": true, "HIT": true,
	"HOG": true, "HOP": true, "HOW": true, "HUG": true, "HUT": true, "ILL": true,
	"INK": true, "INN": true, "ION": true, "IVY": true, "JAW": true, "JAY": true,
	"JEW": true, "JOB": true, "JOG": true, "JOY": true, "JUG": true, "KID": true,
	"KIT": true, "LAB": true, "LAP": true, "LAY": true, "LIE": true, "LOG": true,
	"LOT": true, "MAD": true, "MAT": true, "MUD": true, "MUG": true, "NET": true,
	"NOD": true, "NOT": true, "NOW": true, "OAK": true, "OAR": true, "ODD": true,
	"OPT": true, "ORB": true, "ORE": true, "OUR": true, "OWL": true, "OWN": true,
	"PAY": true, "PEA": true, "PEG": true, "PET": true, "PIT": true, "PLY": true,
	"POD": true, "POP": true, "PUN": true, "PUP": true, "RAG": true, "RAM": true,
	"RAN": true, "RAP": true, "RAT": true, "RAY": true, "RIB": true, "RID": true,
	"RIG": true, "RIM": true, "RIP": true, "ROB": true, "ROD": true, "ROT": true,
	"RUB": true, "RUG": true, "RUM": true, "RUT": true, "RYE": true, "SAG": true,
	"SAP": true, "SAT": true, "SAW": true, "SAY": true, "SEW": true, "SHE": true,
	"SHY": true, "SIN": true, "SIP": true, "SIR": true, "SKI": true, "SKY": true,
	"SLY": true, "SOB": true, "SOD": true, "SON": true, "SOW": true, "SPY": true,
	"STY": true, "SUE": true, "SUM": true, "TAB": true, "TAN": true, "TAP": true,
	"TAR": true, "TAX": true, "TIE": true, "TOY": true, "TRY": true, "TUB": true,
	"TUG": true, "URN": true, "USE": true, "VAT": true, "VET": true, "VIA": true,
	"VOW": true, "WAX": true, "WHO": true, "WHY": true, "WIG": true, "WIT": true,
	"WOE": true, "WOK": true, "WON": true, "YAK": true, "YAM": true, "YAP": true,
	"YAW": true, "YEA": true, "YEW": true, "YIP": true, "YOU": true, "ZEN": true,
	"ZIP": true,
	// 2-letter common English words:
	"AM": true, "AN": true, "AS": true, "AT": true, "BE": true, "BY": true,
	"DO": true, "GO": true, "HE": true, "IF": true, "IN": true, "IS": true,
	"IT": true, "ME": true, "MY": true, "NO": true, "OF": true, "ON": true,
	"OR": true, "SO": true, "TO": true, "UP": true, "US": true, "WE": true,
}

// Long known unit suffixes eligible for OCR-tolerant fuzzy matching (edit distance <= 1).
var longKnownUnitSuffixes = []string{
	"MINUTE", "MINUTES",
	"SECOND", "SECONDS",
	"HOUR", "HOURS",
	"DAY", "DAYS",
	"WEEK", "WEEKS",
	"MONTH", "MONTHS",
	"YEAR", "YEARS",
}

// isQuantityToken determines whether a token represents a quantity+unit compound (e.g. 3MINUTE, 20KG, 500ML)
// rather than a proprietary name/brand that must remain verbatim.
// It keeps exact quantity/unit matching and applies a conservative OCR-tolerant rule:
// only for digit-leading tokens, allow edit distance <= 1 against long known unit suffixes:
// MINUTE(S), SECOND(S), HOUR(S), DAY(S), WEEK(S), MONTH(S), YEAR(S).
// Short units and arbitrary alphanumerics are not fuzzy-matched.
func isQuantityToken(tok string) bool {
	if asciiQuantityTokenRegex.MatchString(tok) {
		return true
	}

	// Conservative OCR-tolerant rule: only for digit-leading tokens
	i := 0
	for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
		i++
	}
	if i == 0 || i == len(tok) {
		return false
	}
	suffix := tok[i:]
	// Suffix must be purely alphabetic ASCII.
	// Safety comes from comparing only against longKnownUnitSuffixes (edit distance <= 1).
	for j := range len(suffix) {
		c := suffix[j]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
			return false
		}
	}

	upperSuffix := strings.ToUpper(suffix)
	for _, unit := range longKnownUnitSuffixes {
		if isLevenshteinLeq1(upperSuffix, unit) {
			return true
		}
	}
	return false
}

// isLevenshteinLeq1 returns true if the Levenshtein edit distance between ASCII strings a and b is <= 1.
func isLevenshteinLeq1(a, b string) bool {
	diff := len(a) - len(b)
	if diff < -1 || diff > 1 {
		return false
	}
	if diff == 0 {
		mismatches := 0
		for i := range len(a) {
			if a[i] != b[i] {
				mismatches++
				if mismatches > 1 {
					return false
				}
			}
		}
		return true
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	// len(a) == len(b) + 1: check if b can be obtained by deleting 1 character from a
	i, j := 0, 0
	mismatches := 0
	for i < len(a) && j < len(b) {
		if a[i] != b[j] {
			mismatches++
			if mismatches > 1 {
				return false
			}
			i++ // skip the character in longer string a
		} else {
			i++
			j++
		}
	}
	return true
}

// stripDiacritics folds diacritical Latin variants (e.g. ö/ô/ơ/ò/ó/ọ/ỏ/õ -> o, é/è/ê/ë -> e, etc.)
// to plain ASCII characters so that diacritic-decorated latin transliterations are recognized
// against plain ASCII names.
func stripDiacritics(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'à', 'á', 'ả', 'ã', 'ạ', 'ă', 'ằ', 'ắ', 'ẳ', 'ẵ', 'ặ', 'â', 'ầ', 'ấ', 'ẩ', 'ẫ', 'ậ', 'ä', 'å', 'ā':
			b.WriteRune('a')
		case 'À', 'Á', 'Ả', 'Ã', 'Ạ', 'Ă', 'Ằ', 'Ắ', 'Ẳ', 'Ẵ', 'Ặ', 'Â', 'Ầ', 'Ấ', 'Ẩ', 'Ẫ', 'Ậ', 'Ä', 'Å', 'Ā':
			b.WriteRune('A')
		case 'è', 'é', 'ẻ', 'ẽ', 'ẹ', 'ê', 'ề', 'ế', 'ể', 'ễ', 'ệ', 'ë', 'ē', 'ė', 'ę':
			b.WriteRune('e')
		case 'È', 'É', 'Ẻ', 'Ẽ', 'Ẹ', 'Ê', 'Ề', 'Ế', 'Ể', 'Ễ', 'Ệ', 'Ë', 'Ē', 'Ė', 'Ę':
			b.WriteRune('E')
		case 'ì', 'í', 'ỉ', 'ĩ', 'ị', 'ï', 'î', 'ī', 'į':
			b.WriteRune('i')
		case 'Ì', 'Í', 'Ỉ', 'Ĩ', 'Ị', 'Ï', 'Î', 'Ī', 'Į':
			b.WriteRune('I')
		case 'ò', 'ó', 'ỏ', 'õ', 'ọ', 'ô', 'ồ', 'ố', 'ổ', 'ỗ', 'ộ', 'ơ', 'ờ', 'ớ', 'ở', 'ỡ', 'ợ', 'ö', 'ø', 'ō':
			b.WriteRune('o')
		case 'Ò', 'Ó', 'Ỏ', 'Õ', 'Ọ', 'Ô', 'Ồ', 'Ố', 'Ổ', 'Ỗ', 'Ộ', 'Ơ', 'Ờ', 'Ớ', 'Ở', 'Ỡ', 'Ợ', 'Ö', 'Ø', 'Ō':
			b.WriteRune('O')
		case 'ù', 'ú', 'ủ', 'ũ', 'ụ', 'ư', 'ừ', 'ứ', 'ử', 'ữ', 'ự', 'ü', 'û', 'ū', 'ů':
			b.WriteRune('u')
		case 'Ù', 'Ú', 'Ủ', 'Ũ', 'Ụ', 'Ư', 'Ừ', 'Ứ', 'Ử', 'Ữ', 'Ự', 'Ü', 'Û', 'Ū', 'Ů':
			b.WriteRune('U')
		case 'ỳ', 'ý', 'ỷ', 'ỹ', 'ỵ', 'ÿ':
			b.WriteRune('y')
		case 'Ỳ', 'Ý', 'Ỷ', 'Ỹ', 'Ỵ', 'Ÿ':
			b.WriteRune('Y')
		case 'đ':
			b.WriteRune('d')
		case 'Đ':
			b.WriteRune('D')
		case 'ç', 'ć', 'č':
			b.WriteRune('c')
		case 'Ç', 'Ć', 'Č':
			b.WriteRune('C')
		case 'ñ', 'ń', 'ň':
			b.WriteRune('n')
		case 'Ñ', 'Ń', 'Ň':
			b.WriteRune('N')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
