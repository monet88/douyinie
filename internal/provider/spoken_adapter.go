package provider

import (
	"context"
	"math"
	"regexp"
	"strings"
	"unicode"

	"github.com/monet88/douyinie/internal/domain"
)

// SpokenScriptAdapter adapts translated dialogue to fit immutable source timing
// while preserving source-relative cadence evidence for downstream TTS fitting.
type SpokenScriptAdapter interface {
	AdaptSpokenScript(ctx context.Context, req SpokenScriptAdaptationRequest) (*SpokenScriptAdaptationResult, error)
}

// SpokenScriptAdaptationRequest encapsulates inputs for spoken duration and cadence adaptation.
type SpokenScriptAdaptationRequest struct {
	SourceText            string
	SourceLanguage        string
	MeaningText           string
	TargetLanguage        string
	SlotDurationMs        int64
	SourceSpeakingRateCPS float64 // optional source syllable/unit-rate override for deterministic tests
	SourceGapAfterMs      int64   // immutable source silence from this turn EndMs to the next turn StartMs
	HasNextTurn           bool
	ProtectedTerms        []domain.GlossaryEntry
	MeasuredDurationMs    int64 // actual probed audio duration when invoked after synthesis overrun
	OverrunMs             int64 // actual measured overrun above PlaybackAllowanceMs (0 when none is proved)
	PlaybackAllowanceMs   int64 // accepted playback window budget (DubPlaybackEndMs - StartMs)
}

// SpokenScriptAdaptationResult represents the outcome of spoken dialogue adaptation.
type SpokenScriptAdaptationResult struct {
	SpokenText            string
	IsShortened           bool
	EstimatedDurationMs   int64
	TargetSpeakingRateCPS float64
	CadenceRatio          float64
	NaturalGapMs          int64 // predicted pause from estimated dub finish to the next source turn
	UsableSlotMs          int64 // immutable source speech slot; inter-turn source gap is outside this window
	TargetWordBudget      int
	RequiresReview        bool
	ReviewReason          string
}

// DefaultSpokenScriptAdapter provides generic linguistic concision and source-relative timing evidence.
type DefaultSpokenScriptAdapter struct{}

// NewDefaultSpokenScriptAdapter creates the default spoken-script adapter.
func NewDefaultSpokenScriptAdapter() *DefaultSpokenScriptAdapter {
	return &DefaultSpokenScriptAdapter{}
}

// Cadence thresholds are planning priors, not acceptance-fixture measurements.
// Ratios compare target syllables/sec with source syllables/sec.
const (
	cadenceTriggerMinRatio = 0.65
	cadenceTriggerMaxRatio = 1.20
	cadenceReviewMinRatio  = 0.50
	cadenceReviewMaxRatio  = 1.50
	phonationReserveMs     = 150.0
)

func msPerWordFor(lang string) float64 {
	switch strings.ToLower(lang) {
	case "vi":
		return 260.0
	case "en":
		return 310.0
	default:
		return 280.0
	}
}

// EstimateSpokenDurationMs is planning evidence only; measured synthesized audio
// remains authoritative in the downstream TTS fit controller.
func EstimateSpokenDurationMs(text, lang string) int64 {
	count := len(strings.Fields(strings.TrimSpace(text)))
	if count == 0 {
		return 0
	}
	return int64(math.Round(float64(count)*msPerWordFor(lang) + phonationReserveMs))
}

// EstimateSpokenRateCPS estimates target spoken cadence in syllables/sec so it
// can be compared dimensionlessly with source cadence.
func EstimateSpokenRateCPS(text, lang string) float64 {
	estimatedMs := EstimateSpokenDurationMs(text, lang)
	if estimatedMs <= 0 {
		return 0
	}
	syllables := estimateSyllableCount(text, lang)
	if syllables <= 0 {
		return 0
	}
	return syllables / (float64(estimatedMs) / 1000.0)
}

// EstimateSourceSpeakingRate estimates source cadence in syllable-like speech
// units/sec. Chinese Han characters map closely to spoken syllables; punctuation
// and whitespace are excluded. Non-Chinese sources reuse the target-language
// syllable estimator.
func EstimateSourceSpeakingRate(text, lang string, slotDurationMs int64) float64 {
	if slotDurationMs <= 0 {
		return 0
	}

	var units float64
	if strings.HasPrefix(strings.ToLower(lang), "zh") {
		units = countChineseSpeechUnits(text)
	} else {
		units = estimateSyllableCount(text, lang)
	}
	if units <= 0 {
		return 0
	}
	return units / (float64(slotDurationMs) / 1000.0)
}

func countChineseSpeechUnits(text string) float64 {
	var units float64
	inLatinToken := false
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			units++
			inLatinToken = false
		case unicode.IsDigit(r):
			units++
			inLatinToken = false
		case unicode.IsLetter(r):
			if !inLatinToken {
				units++
				inLatinToken = true
			}
		default:
			inLatinToken = false
		}
	}
	return units
}

func estimateSyllableCount(text, lang string) float64 {
	words := strings.Fields(strings.TrimSpace(text))
	if len(words) == 0 {
		return 0
	}

	switch strings.ToLower(lang) {
	case "vi":
		// Vietnamese orthographic spacing is predominantly syllable-based.
		return float64(len(words))
	case "en":
		var total int
		for _, word := range words {
			total += estimateEnglishWordSyllables(word)
		}
		return float64(total)
	default:
		return float64(len(words))
	}
}

func estimateEnglishWordSyllables(word string) int {
	letters := make([]rune, 0, len(word))
	for _, r := range strings.ToLower(word) {
		if unicode.IsLetter(r) {
			letters = append(letters, r)
		}
	}
	if len(letters) == 0 {
		return 0
	}

	isVowel := func(r rune) bool {
		return strings.ContainsRune("aeiouy", r)
	}
	groups := 0
	prevVowel := false
	for _, r := range letters {
		vowel := isVowel(r)
		if vowel && !prevVowel {
			groups++
		}
		prevVowel = vowel
	}
	if len(letters) > 2 && letters[len(letters)-1] == 'e' && groups > 1 && !isVowel(letters[len(letters)-2]) {
		groups--
	}
	if groups < 1 {
		return 1
	}
	return groups
}

// targetWordBudget combines the immutable duration budget with a source-relative
// cadence ceiling. Source cadence therefore changes the adaptation decision,
// rather than being metadata-only.
func targetWordBudget(slotDurationMs int64, targetLang string, sourceRate float64, meaningText string) int {
	msPerWord := msPerWordFor(targetLang)
	if slotDurationMs <= 0 || msPerWord <= 0 {
		return 0
	}

	durationBudget := int(math.Floor((float64(slotDurationMs) - phonationReserveMs) / msPerWord))
	if durationBudget < 0 {
		durationBudget = 0
	}
	if sourceRate <= 0 {
		return durationBudget
	}

	words := len(strings.Fields(strings.TrimSpace(meaningText)))
	if words == 0 {
		return 0
	}
	avgSyllablesPerWord := estimateSyllableCount(meaningText, targetLang) / float64(words)
	if avgSyllablesPerWord <= 0 {
		return durationBudget
	}

	cadenceSyllableBudget := sourceRate * cadenceTriggerMaxRatio * (float64(slotDurationMs) / 1000.0)
	cadenceBudget := int(math.Floor(cadenceSyllableBudget / avgSyllablesPerWord))
	if cadenceBudget < 0 {
		cadenceBudget = 0
	}
	if cadenceBudget < durationBudget {
		return cadenceBudget
	}
	return durationBudget
}

// EstimatePauseToNextTurnMs returns the predicted pause from the estimated dub
// finish to the next immutable source-turn start. SourceGapAfterMs is outside
// the current speech slot, so it must never be subtracted from SlotDurationMs.
func EstimatePauseToNextTurnMs(slotDurationMs, estimatedDurationMs, sourceGapAfterMs int64, hasNextTurn bool) int64 {
	if !hasNextTurn {
		return 0
	}
	if slotDurationMs < 0 {
		slotDurationMs = 0
	}
	if sourceGapAfterMs < 0 {
		sourceGapAfterMs = 0
	}
	pause := slotDurationMs + sourceGapAfterMs - estimatedDurationMs
	if pause < 0 {
		return 0
	}
	return pause
}

// AdaptSpokenScript implements SpokenScriptAdapter.
func (a *DefaultSpokenScriptAdapter) AdaptSpokenScript(_ context.Context, req SpokenScriptAdaptationRequest) (*SpokenScriptAdaptationResult, error) {
	meaningText := strings.TrimSpace(req.MeaningText)
	targetLang := strings.ToLower(strings.TrimSpace(req.TargetLanguage))
	sourceLang := strings.ToLower(strings.TrimSpace(req.SourceLanguage))
	slotMs := req.SlotDurationMs
	if req.PlaybackAllowanceMs > 0 {
		slotMs = req.PlaybackAllowanceMs
	}
	if slotMs <= 0 {
		slotMs = 1000
	}
	sourceRate := req.SourceSpeakingRateCPS
	if sourceRate <= 0 {
		sourceRate = EstimateSourceSpeakingRate(req.SourceText, sourceLang, slotMs)
	}

	estimatedMs := EstimateSpokenDurationMs(meaningText, targetLang)
	targetRate := EstimateSpokenRateCPS(meaningText, targetLang)
	cadenceRatio := ratio(targetRate, sourceRate)
	targetWords := len(strings.Fields(meaningText))
	wordBudget := targetWordBudget(slotMs, targetLang, sourceRate, meaningText)

	measuredOverrun := req.MeasuredDurationMs > slotMs || req.OverrunMs > 0
	shouldShorten := estimatedMs > slotMs || targetWords > wordBudget || measuredOverrun
	if cadenceRatio > 0 && (cadenceRatio < cadenceTriggerMinRatio || cadenceRatio > cadenceTriggerMaxRatio) {
		shouldShorten = true
	}
	spokenText := meaningText
	isShortened := false
	if shouldShorten {
		candidate := RewriteConciseSpokenText(meaningText, targetLang)
		if measuredOverrun {
			candidate = RewriteMeasuredOverrunSpokenText(candidate, targetLang)
		}
		for _, term := range req.ProtectedTerms {
			if domain.GlossaryTermMatches(meaningText, term.Target) && !domain.GlossaryTermMatches(candidate, term.Target) {
				candidate = meaningText
				break
			}
		}
		if len(strings.Fields(candidate)) > 0 && len(strings.Fields(candidate)) < targetWords {
			spokenText = candidate
			isShortened = true
		}
	}

	estimatedMs = EstimateSpokenDurationMs(spokenText, targetLang)
	targetRate = EstimateSpokenRateCPS(spokenText, targetLang)
	cadenceRatio = ratio(targetRate, sourceRate)
	naturalGapMs := EstimatePauseToNextTurnMs(slotMs, estimatedMs, req.SourceGapAfterMs, req.HasNextTurn)

	requiresReview := false
	reviewReason := ""
	switch {
	case estimatedMs > slotMs:
		requiresReview = true
		reviewReason = "DURATION_OVERRUN"
	case cadenceRatio > 0 && cadenceRatio < cadenceReviewMinRatio:
		requiresReview = true
		reviewReason = "CADENCE_TOO_SLOW"
	case cadenceRatio > cadenceReviewMaxRatio:
		requiresReview = true
		reviewReason = "CADENCE_TOO_FAST"
	}

	return &SpokenScriptAdaptationResult{
		SpokenText:            spokenText,
		IsShortened:           isShortened,
		EstimatedDurationMs:   estimatedMs,
		TargetSpeakingRateCPS: targetRate,
		CadenceRatio:          cadenceRatio,
		NaturalGapMs:          naturalGapMs,
		UsableSlotMs:          slotMs,
		TargetWordBudget:      wordBudget,
		RequiresReview:        requiresReview,
		ReviewReason:          reviewReason,
	}, nil
}

func ratio(target, source float64) float64 {
	if source <= 0 {
		return 0
	}
	return target / source
}

var (
	viGenericRules = []struct {
		Pattern *regexp.Regexp
		Replace string
	}{
		{regexp.MustCompile(`(?i)(?:^|\s+)(xin vui lòng|vui lòng|làm ơn)\s+`), " "},
		{regexp.MustCompile(`(?i)(?:^|\s+)(hãy nhớ|hãy)\s+`), " "},
		{regexp.MustCompile(`(?i)(?:^|\s+)chúng ta\s+`), " ta "},
		{regexp.MustCompile(`(?i)(?:^|\s+)điều chỉnh\s+`), " chỉnh "},
		{regexp.MustCompile(`(?i)(?:^|\s+)không được\s+`), " đừng "},
		{regexp.MustCompile(`(?i)(?:^|\s+)không phải\s+`), " chẳng phải "},
	}
	enGenericRules = []struct {
		Pattern *regexp.Regexp
		Replace string
	}{
		{regexp.MustCompile(`(?i)\b(please|kindly)\s+`), ""},
		{regexp.MustCompile(`(?i)\bdo not\s+`), "don't "},
		{regexp.MustCompile(`(?i)\bcannot\s+`), "can't "},
		{regexp.MustCompile(`(?i)\bwill not\s+`), "won't "},
		{regexp.MustCompile(`(?i)\bdid not\s+`), "didn't "},
		{regexp.MustCompile(`(?i)\bdoes not\s+`), "doesn't "},
		{regexp.MustCompile(`(?i)\bis not\s+`), "isn't "},
		{regexp.MustCompile(`(?i)\bare not\s+`), "aren't "},
	}
	multiSpaceRegex       = regexp.MustCompile(`\s+`)
	spaceBeforePunctRegex = regexp.MustCompile(`\s+([,.\?!])`)
)

// normalizeSpokenSpacing is the shared final pass of every spoken-text rewrite:
// collapse whitespace runs and drop the space before sentence punctuation.
func normalizeSpokenSpacing(text string) string {
	text = multiSpaceRegex.ReplaceAllString(strings.TrimSpace(text), " ")
	return spaceBeforePunctRegex.ReplaceAllString(text, "$1")
}

// RewriteConciseSpokenText applies only generic politeness, pronoun, verb-form,
// and contraction reductions. It deliberately avoids corpus-specific sentence
// rewrites so unseen text follows the same production path as test fixtures.
func RewriteConciseSpokenText(text, tgtLang string) string {
	result := text
	switch strings.ToLower(tgtLang) {
	case "vi":
		for _, rule := range viGenericRules {
			result = rule.Pattern.ReplaceAllString(result, rule.Replace)
		}
	case "en":
		for _, rule := range enGenericRules {
			result = rule.Pattern.ReplaceAllString(result, rule.Replace)
		}
	}
	return normalizeSpokenSpacing(result)
}

// The measured-overrun tier may only drop qualifiers that cannot change negation scope.
// Vietnamese "rất"/"rất là"/"vô cùng" are intensifiers that carry no negation of their own, so on
// clearly safe text removing them leaves the claim intact. Qualifiers such as
// "hoàn toàn"/"thực sự"/"thật sự" instead scope the negation ("không hoàn toàn đồng ý" = does
// not entirely agree), so removing them silently strengthens the claim to "không đồng ý";
// MeaningFirstQAGate preserves polarity presence, not polarity scope, and cannot catch that.
// English has no scope-safe intensifier in this tier — "not very"/"not really"/"not actually"/
// "not truly"/"not extremely" all hedge the negation — so this tier defines no English rules and
// a negated English qualifier survives untouched instead of being shortened.
var viMeasuredOverrunRules = []struct {
	Pattern *regexp.Regexp
	Replace string
}{
	{regexp.MustCompile(`(?i)(?:^|\s+)(vô cùng|rất là)\s+`), " "},
	{regexp.MustCompile(`(?i)(?:^|\s+)rất\s+`), " "},
}

// viNegationMarkerRegexp matches a Vietnamese negation marker as a whole word. Whether an
// intensifier can lose its own text depends on the clause it sits in, not on the intensifier:
// the same "rất" is removable in "Đây là điều rất quan trọng" and load-bearing in
// "Tôi nói không rất rõ ràng" ("I said 'no', very clearly"), where deleting it moves the
// standalone "không" into scope of "rõ ràng" and turns the claim into "không rõ ràng"
// (I do not speak clearly). MeaningFirstQAGate preserves polarity presence, not polarity scope,
// so only the clause context can refuse that deletion.
var viNegationMarkerRegexp = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])(không|chẳng|chả|chưa|đừng|chớ|đâu)([^\p{L}\p{N}]|$)`)

// negationMarkerPrecedes reports whether a Vietnamese negation marker occurs in the clause that
// reaches up to prefix. Any marker in that clause may scope the intensifier that follows, or be
// re-read as a standalone particle beside it, so the tier refuses the deletion. Sentence and
// clause punctuation open a fresh clause, which keeps clearly safe text shortenable even when an
// earlier clause is negated.
func negationMarkerPrecedes(prefix string) bool {
	if cut := strings.LastIndexAny(prefix, ".!?;:,()\n"); cut >= 0 {
		prefix = prefix[cut+1:]
	}
	return viNegationMarkerRegexp.MatchString(prefix)
}

// replaceUnscopedIntensifier applies one measured-overrun rule, skipping every occurrence whose
// preceding clause carries a negation marker. Scope is decided per occurrence, so a negated
// clause keeps its intensifier while an unnegated one in the same text still shortens.
func replaceUnscopedIntensifier(text string, pattern *regexp.Regexp, replace string) string {
	locs := pattern.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, loc := range locs {
		if negationMarkerPrecedes(text[:loc[0]]) {
			continue
		}
		b.WriteString(text[last:loc[0]])
		b.WriteString(replace)
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// RewriteMeasuredOverrunSpokenText applies a second-tier concision pass when a synthesized
// waveform overruns its accepted playback budget. It removes only intensifiers that no
// Vietnamese negation can scope, so facts, names, numbers, glossary terms, and negation scope
// all survive the rewrite.
func RewriteMeasuredOverrunSpokenText(text, tgtLang string) string {
	result := text
	if strings.ToLower(tgtLang) == "vi" {
		for _, rule := range viMeasuredOverrunRules {
			result = replaceUnscopedIntensifier(result, rule.Pattern, rule.Replace)
		}
	}
	return normalizeSpokenSpacing(result)
}
