package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/monet88/douyinie/internal/domain"
)

// Bounded gateway translation work limits (issue #150 A4). They are explicit resource budgets,
// not calibrated language-quality thresholds: changing any of them changes the translation
// semantic contract and therefore the TranslationVariant contract identity.
const (
	// translationBatchMaxSegments bounds one outbound translation batch.
	translationBatchMaxSegments = 512
	// translationBatchMaxSourceBytes bounds the aggregate canonical source bytes of one batch.
	translationBatchMaxSourceBytes = 256 << 10
	// translationWireMaxBytes bounds both the marshaled outbound request and the read inbound
	// response body. An overflowing body is refused, never parsed as a truncated success.
	translationWireMaxBytes = 1 << 20
	// translationWireCallsPerCandidate bounds every gateway wire call of one Router provider
	// candidate, shared across all transport-retry re-entry of that candidate.
	translationWireCallsPerCandidate = 14
	// translationTargetedCallsPerCandidate bounds the targeted repair calls of one candidate.
	translationTargetedCallsPerCandidate = 12
	// translationTargetedAttemptsPerEntry bounds targeted repair attempts for one entry.
	translationTargetedAttemptsPerEntry = 3
	// translationNeighborContextSegments is the maximum number of canonical neighbors on either
	// side of a targeted repair entry. They are read-only context and are never merged.
	translationNeighborContextSegments = 2
	// translationShortCopyMaxRunes keeps short/proper-name/title-only copies weak: an exact
	// cross-script copy whose normalized source is no longer than this stays weak evidence and
	// never fails on its own.
	translationShortCopyMaxRunes = 10
)

// gatewaySegment is one raw translated entry decoded from a gateway payload.
type gatewaySegment struct {
	Index            int      `json:"index"`
	SourceText       string   `json:"source_text"`
	TargetText       string   `json:"target_text"`
	KeyFacts         []string `json:"key_facts"`
	NegationPolarity bool     `json:"negation_polarity"`
}

// anchorFailureKind classifies why one entry cannot be accepted as translated output.
type anchorFailureKind int

const (
	anchorMissing anchorFailureKind = iota + 1
	anchorEmptyTarget
	anchorMissingEcho
	anchorEchoMismatch
	anchorCopy
)

func (k anchorFailureKind) String() string {
	switch k {
	case anchorMissing:
		return "missing index"
	case anchorEmptyTarget:
		return "empty target"
	case anchorMissingEcho:
		return "missing source echo"
	case anchorEchoMismatch:
		return "source echo mismatch"
	case anchorCopy:
		return "untranslated source copy"
	default:
		return "unknown failure"
	}
}

// entryFailure is one addressable entry failure of a candidate batch.
type entryFailure struct {
	Index  int
	Kind   anchorFailureKind
	Reason string
}

// batchAudit is the anchoring verdict for one candidate batch.
type batchAudit struct {
	// Entries holds every decoded entry that passed echo and target checks, including entries
	// still failing copy detection. A successful (usable) audit means Entries is the accepted set.
	Entries map[int]gatewaySegment
	// Failures lists addressable per-entry failures that targeted repair may address.
	Failures []entryFailure
	// Structural lists batch-global anchor violations (duplicates, extra indices, shifted
	// echoes). A non-empty Structural verdict means the candidate is structurally unusable.
	Structural []string
}

// usable reports whether the batch is an exactly anchored, failure-free translation of the
// canonical input.
func (a batchAudit) usable() bool {
	return len(a.Structural) == 0 && len(a.Failures) == 0
}

// normalizeEchoText canonicalizes a source echo for anchor comparison: NFKC, lowercased, with
// punctuation and whitespace removed (issue #150 A1). It is intentionally not a fuzzy match:
// only canonical equivalence counts as a valid echo.
func normalizeEchoText(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKC.String(s) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// ValidateCanonicalBatch refuses a canonical batch whose indices are not unique or whose bounded
// size exceeds the A4 ceilings. It runs before any gateway call: an invalid canonical input is a
// caller contract violation, never a quality verdict on a model response. Violations are wrapped
// around domain.ErrInvalidCanonicalBatch so transport layers can classify them as client errors.
func ValidateCanonicalBatch(segments []domain.TranslationInputSegment) error {
	if len(segments) == 0 {
		return domain.ErrEmptyTranslationInput
	}
	if len(segments) > translationBatchMaxSegments {
		return fmt.Errorf("%w: translation batch has %d segments, exceeding the %d-segment bound", domain.ErrInvalidCanonicalBatch, len(segments), translationBatchMaxSegments)
	}
	seen := make(map[int]bool, len(segments))
	var totalBytes int
	for _, seg := range segments {
		if seen[seg.Index] {
			return fmt.Errorf("%w: canonical translation input repeats index %d: batch indices must be unique", domain.ErrInvalidCanonicalBatch, seg.Index)
		}
		seen[seg.Index] = true
		totalBytes += len(seg.SourceText)
	}
	if totalBytes > translationBatchMaxSourceBytes {
		return fmt.Errorf("%w: canonical translation source is %d bytes, exceeding the %d-byte batch bound", domain.ErrInvalidCanonicalBatch, totalBytes, translationBatchMaxSourceBytes)
	}
	return nil
}

// stripMarkdownFence removes an optional ```json ... ``` wrapper from a gateway payload.
func stripMarkdownFence(content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "```") {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		return content
	}
	if strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "```") {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// decodeGatewaySegments decodes a gateway content payload into raw entries. Indices outside the
// canonical set and repeated indices are reported as structural violations instead of being
// silently ignored or overwritten.
func decodeGatewaySegments(content string, expected []domain.TranslationInputSegment) (map[int]gatewaySegment, []string, error) {
	var parsed struct {
		Segments []gatewaySegment `json:"segments"`
	}
	if err := json.Unmarshal([]byte(stripMarkdownFence(content)), &parsed); err != nil {
		return nil, nil, fmt.Errorf("%w: failed to parse translation JSON content: %v", domain.ErrQualityRejected, err)
	}
	if len(parsed.Segments) == 0 {
		return nil, nil, fmt.Errorf("%w: gateway returned 0 translated segments", domain.ErrQualityRejected)
	}

	expectedSet := make(map[int]bool, len(expected))
	for _, seg := range expected {
		expectedSet[seg.Index] = true
	}

	decoded := make(map[int]gatewaySegment, len(parsed.Segments))
	var structural []string
	for _, raw := range parsed.Segments {
		if _, dup := decoded[raw.Index]; dup {
			structural = append(structural, fmt.Sprintf("gateway repeated segment index %d", raw.Index))
			continue
		}
		if !expectedSet[raw.Index] {
			structural = append(structural, fmt.Sprintf("gateway returned unrequested segment index %d", raw.Index))
			continue
		}
		decoded[raw.Index] = raw
	}
	return decoded, structural, nil
}

// auditAnchoredBatch validates a decoded batch against the canonical input: exact index
// coverage, mandatory matching source echoes, nonempty targets and conservative copy detection.
func auditAnchoredBatch(expected []domain.TranslationInputSegment, decoded map[int]gatewaySegment, targetLang string, glossary domain.EffectiveGlossary) batchAudit {
	audit := batchAudit{Entries: make(map[int]gatewaySegment, len(expected))}

	normSource := make(map[int]string, len(expected))
	for _, seg := range expected {
		normSource[seg.Index] = normalizeEchoText(seg.SourceText)
	}

	strongCopies := make(map[int]bool)
	weakCopies := make(map[int]bool)

	for _, seg := range expected {
		raw, ok := decoded[seg.Index]
		if !ok {
			audit.Failures = append(audit.Failures, entryFailure{Index: seg.Index, Kind: anchorMissing, Reason: fmt.Sprintf("segment %d: %s", seg.Index, anchorMissing)})
			continue
		}
		echo := strings.TrimSpace(raw.SourceText)
		if echo == "" {
			audit.Failures = append(audit.Failures, entryFailure{Index: seg.Index, Kind: anchorMissingEcho, Reason: fmt.Sprintf("segment %d: %s", seg.Index, anchorMissingEcho)})
			continue
		}
		if normalizeEchoText(echo) != normSource[seg.Index] {
			if shiftedTo, shifted := shiftedEchoIndex(normalizeEchoText(echo), normSource, seg.Index); shifted {
				audit.Structural = append(audit.Structural, fmt.Sprintf("segment %d echoes the source of index %d", seg.Index, shiftedTo))
			} else {
				audit.Failures = append(audit.Failures, entryFailure{Index: seg.Index, Kind: anchorEchoMismatch, Reason: fmt.Sprintf("segment %d: %s", seg.Index, anchorEchoMismatch)})
			}
			continue
		}
		if strings.TrimSpace(raw.TargetText) == "" {
			audit.Failures = append(audit.Failures, entryFailure{Index: seg.Index, Kind: anchorEmptyTarget, Reason: fmt.Sprintf("segment %d: %s", seg.Index, anchorEmptyTarget)})
			continue
		}

		audit.Entries[seg.Index] = raw
		switch classifyCopy(seg.SourceText, raw.TargetText, targetLang, glossary) {
		case copyStrong:
			strongCopies[seg.Index] = true
		case copyWeak:
			weakCopies[seg.Index] = true
		}
	}

	// Weak copies never raise the strong-evidence count: the threshold is decided first, then
	// weak entries are promoted only when the batch already shows meaningful untranslated content.
	promoteWeak := len(strongCopies) >= 2 && len(strongCopies)*3 > len(expected)
	for index := range strongCopies {
		audit.Failures = append(audit.Failures, entryFailure{Index: index, Kind: anchorCopy, Reason: fmt.Sprintf("segment %d: %s", index, anchorCopy)})
	}
	if promoteWeak {
		for index := range weakCopies {
			if !strongCopies[index] {
				audit.Failures = append(audit.Failures, entryFailure{Index: index, Kind: anchorCopy, Reason: fmt.Sprintf("segment %d: %s promoted after strong>=2 and strong*3 > batch size", index, anchorCopy)})
			}
		}
	}
	sort.Slice(audit.Failures, func(i, j int) bool { return audit.Failures[i].Index < audit.Failures[j].Index })
	return audit
}

// shiftedEchoIndex reports whether a mismatching echo actually echoes another canonical index,
// which makes the response structurally shifted rather than individually mismatched.
func shiftedEchoIndex(echo string, normSource map[int]string, current int) (int, bool) {
	if echo == "" {
		return 0, false
	}
	for index, norm := range normSource {
		if index != current && norm != "" && norm == echo {
			return index, true
		}
	}
	return 0, false
}

// copyVerdict classifies an exact source copy of one segment.
type copyVerdict int

const (
	// copyDistinct is a real translation (target differs from source).
	copyDistinct copyVerdict = iota
	// copyExcluded is an exact copy that is never evidence of untranslated content.
	copyExcluded
	// copyWeak is an exact cross-script copy of short/proper-name/title-only content.
	copyWeak
	// copyStrong is a meaningful cross-script unchanged copy.
	copyStrong
)

// classifyCopy applies the conservative copy-detection contract (issue #150 A2): only meaningful
// cross-script unchanged content is strong evidence; numeric-only, URL/email, same-script text and
// explicitly authorized unchanged terminology are excluded, and short/proper-name/title-only
// copies stay weak.
func classifyCopy(source, target, targetLang string, glossary domain.EffectiveGlossary) copyVerdict {
	normSource := normalizeEchoText(source)
	if normSource == "" || normSource != normalizeEchoText(target) {
		return copyDistinct
	}
	if !hasLetter(target) || looksLikeURLOrEmail(target) {
		return copyExcluded
	}
	if authorizedUnchangedTerm(source, glossary) {
		return copyExcluded
	}
	if !isCrossScriptUntranslated(target, targetLang) {
		return copyExcluded
	}
	if utf8.RuneCountInString(normSource) <= translationShortCopyMaxRunes {
		return copyWeak
	}
	return copyStrong
}

// isCrossScriptUntranslated reports whether an exact copy left source-script content in a target
// that is expected to be Latin script (the production VI/EN target languages). Any other target
// language stays conservative and is never flagged.
func isCrossScriptUntranslated(target, targetLang string) bool {
	switch strings.ToLower(strings.TrimSpace(targetLang)) {
	case "vi", "en":
		return domain.HasCJK(target)
	default:
		return false
	}
}

// hasLetter reports whether s contains at least one letter.
func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// looksLikeURLOrEmail reports whether an exact copy is a URL or email address, which is legitimate
// unchanged content.
func looksLikeURLOrEmail(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(lower, "://") || strings.HasPrefix(lower, "www.") {
		return true
	}
	at := strings.Index(lower, "@")
	return at > 0 && strings.Contains(lower[at+1:], ".")
}

// authorizedUnchangedTerm reports whether the frozen request glossary explicitly authorizes the
// exact source term to stay unchanged in this segment.
func authorizedUnchangedTerm(source string, glossary domain.EffectiveGlossary) bool {
	normSource := normalizeEchoText(source)
	if normSource == "" {
		return false
	}
	for _, entry := range glossary.Entries {
		if strings.TrimSpace(entry.Source) == "" || strings.TrimSpace(entry.Target) == "" {
			continue
		}
		if domain.NormalizeGlossarySource(entry.Source) != domain.NormalizeGlossarySource(entry.Target) {
			continue
		}
		// Only a segment that IS the authorized unchanged term is exempt. A longer untranslated
		// sentence that merely contains the term stays copy evidence.
		if normalizeEchoText(entry.Source) == normSource {
			return true
		}
	}
	return false
}

// errRepairExhausted reports bounded-work exhaustion with the observed subrequest counts. The
// counts travel with the existing quality-failed attempt evidence (ErrorMessage), so repair
// outcomes are auditable without a new attempts store.
func errRepairExhausted(reason string, totalCalls, targetedCalls int) error {
	return fmt.Errorf("%w: %s after %d gateway wire calls (%d targeted) with a %d-call candidate budget",
		domain.ErrQualityRejected, reason, totalCalls, targetedCalls, translationWireCallsPerCandidate)
}

// succeededRepairEvidence reports the bounded subrequest work consumed by a successful invocation,
// in the same shape as errRepairExhausted, so success and exhaustion audit identically through the
// existing attempt evidence.
func succeededRepairEvidence(totalCalls, targetedCalls int) string {
	return fmt.Sprintf("succeeded after %d gateway wire calls (%d targeted) with a %d-call candidate budget",
		totalCalls, targetedCalls, translationWireCallsPerCandidate)
}
