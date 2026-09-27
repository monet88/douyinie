package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/monet88/douyinie/internal/domain"
	"golang.org/x/text/unicode/norm"
)

const (
	maxGlossaryEntries        = 1000
	maxEffectiveGlossaryTerms = 100
	maxGlossarySourceRunes    = 300
	maxGlossaryTargetRunes    = 600
	maxGlossaryNoteRunes      = 1000
	maxRawGlossaryBytes       = 256 << 10
	maxEffectiveGlossaryBytes = 64 << 10
)

func normalizeGlossarySource(s string) string {
	return domain.NormalizeGlossarySource(s)
}

// glossarySourceHasFormatRune reports whether s contains a Unicode format (Cf) rune other than the
// genuine script joiners ZWNJ/ZWJ. NFKC + TrimSpace leaves these runes intact, so a term such as
// "ke\u200byword" validates but can never match any source text: it would silently consume a glossary
// slot and never apply. Rejecting is the smallest deterministic contract that keeps provenance semantics.
func glossarySourceHasFormatRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		switch r {
		case '\u200c', '\u200d': // ZWNJ / ZWJ are meaningful joiners in Arabic/Persian/Indic scripts.
			return false
		}
		return unicode.In(r, unicode.Cf)
	})
}

func validateGlossaryEntriesWithReport(entries []domain.GlossaryEntry) ([]domain.GlossaryEntry, int, error) {
	normalized, err := normalizeGlossaryEntries(entries)
	if err != nil {
		return nil, 0, err
	}

	resolved := make([]domain.GlossaryEntry, 0, len(normalized))
	bySource := make(map[string]domain.GlossaryEntry, len(normalized))
	omittedConflicts := 0
	for _, e := range normalized {
		key := normalizeGlossarySource(e.Source)
		if prior, ok := bySource[key]; ok {
			if prior.Target != e.Target || prior.Note != e.Note {
				omittedConflicts++
			}
			continue
		}
		bySource[key] = e
		resolved = append(resolved, e)
	}
	return resolved, omittedConflicts, nil
}

// normalizeGlossaryEntries validates and normalizes an ordered request-local glossary while keeping
// every entry, including conflicting duplicates. First-wins conflict resolution happens later in
// validateGlossaryEntriesWithReport, so callers that freeze a request glossary persist this form and
// nothing about the request is lost. Invalid input returns an error and mutates nothing.
func normalizeGlossaryEntries(entries []domain.GlossaryEntry) ([]domain.GlossaryEntry, error) {
	if len(entries) > maxGlossaryEntries {
		return nil, fmt.Errorf("glossary has %d entries; maximum is %d", len(entries), maxGlossaryEntries)
	}
	if raw, err := json.Marshal(entries); err != nil {
		return nil, fmt.Errorf("marshal glossary: %w", err)
	} else if len(raw) > maxRawGlossaryBytes {
		return nil, fmt.Errorf("glossary JSON is %d bytes; maximum is %d", len(raw), maxRawGlossaryBytes)
	}

	normalized := make([]domain.GlossaryEntry, 0, len(entries))
	for i, raw := range entries {
		if utf8.RuneCountInString(raw.Source) > maxGlossarySourceRunes || utf8.RuneCountInString(raw.Target) > maxGlossaryTargetRunes || utf8.RuneCountInString(raw.Note) > maxGlossaryNoteRunes {
			return nil, fmt.Errorf("glossary entry %d exceeds source/target/note limits", i)
		}
		// Only source keys participate in NFKC matching. Target/note are operator-owned
		// output semantics: trim surrounding whitespace, but otherwise preserve exact
		// Unicode and case so a compatibility-character change invalidates provenance.
		e := domain.GlossaryEntry{Source: norm.NFKC.String(strings.TrimSpace(raw.Source)), Target: strings.TrimSpace(raw.Target), Note: strings.TrimSpace(raw.Note)}
		if e.Source == "" || e.Target == "" {
			return nil, fmt.Errorf("glossary entry %d requires non-empty source and target", i)
		}
		if glossarySourceHasFormatRune(e.Source) {
			return nil, fmt.Errorf("glossary entry %d source contains a disallowed format character", i)
		}
		if utf8.RuneCountInString(e.Source) > maxGlossarySourceRunes || utf8.RuneCountInString(e.Target) > maxGlossaryTargetRunes || utf8.RuneCountInString(e.Note) > maxGlossaryNoteRunes {
			return nil, fmt.Errorf("normalized glossary entry %d exceeds source/target/note limits", i)
		}
		normalized = append(normalized, e)
	}
	return normalized, nil
}

func validateGlossaryEntries(entries []domain.GlossaryEntry) ([]domain.GlossaryEntry, error) {
	resolved, _, err := validateGlossaryEntriesWithReport(entries)
	return resolved, err
}

// ValidateGlossaryEntries validates and normalizes an ordered request-local glossary.
// RuntimeHost uses it before persisting a run snapshot so invalid input causes zero mutation.
func ValidateGlossaryEntries(entries []domain.GlossaryEntry) ([]domain.GlossaryEntry, error) {
	return validateGlossaryEntries(entries)
}

// ValidateGlossaryEntriesWithReport is ValidateGlossaryEntries plus the number of entries dropped as
// conflicting duplicates (first-wins). The deduped list alone cannot reproduce that count, so an
// ingress that freezes a request glossary must persist it; otherwise EffectiveGlossary reports zero
// conflicts for the frozen run and the operator conflict feedback never fires.
func ValidateGlossaryEntriesWithReport(entries []domain.GlossaryEntry) ([]domain.GlossaryEntry, int, error) {
	return validateGlossaryEntriesWithReport(entries)
}

// NormalizeGlossaryEntries validates and normalizes an ordered request-local glossary while keeping
// conflicting duplicates; first-wins resolution is applied later, when the effective glossary for a
// concrete input is computed. Ingress that freezes or forwards a request glossary must use this form:
// a de-duplicated list cannot reproduce how many entries were dropped as conflicts, so the run's
// EffectiveGlossary would report 0 conflicts and operator conflict feedback would never fire.
func NormalizeGlossaryEntries(entries []domain.GlossaryEntry) ([]domain.GlossaryEntry, error) {
	return normalizeGlossaryEntries(entries)
}

// CanonicalGlossaryEqual reports whether two glossary entry lists are canonically equivalent:
// identical normalized entries, targets, notes, and ordering under ValidateGlossaryEntries.
func CanonicalGlossaryEqual(a, b []domain.GlossaryEntry) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	normA, errA := validateGlossaryEntries(a)
	normB, errB := validateGlossaryEntries(b)
	if errA != nil || errB != nil {
		return false
	}
	if len(normA) != len(normB) {
		return false
	}
	for i := range normA {
		if normalizeGlossarySource(normA[i].Source) != normalizeGlossarySource(normB[i].Source) ||
			normA[i].Target != normB[i].Target ||
			normA[i].Note != normB[i].Note {
			return false
		}
	}
	return true
}

func glossaryTermMatches(sourceText, term string) bool {
	return domain.GlossaryTermMatches(sourceText, term)
}

func effectiveGlossary(entries []domain.GlossaryEntry, segments []domain.TranslationInputSegment) (domain.EffectiveGlossary, error) {
	validated, omittedConflicts, err := validateGlossaryEntriesWithReport(entries)
	if err != nil {
		return domain.EffectiveGlossary{}, err
	}
	matched := make([]domain.GlossaryEntry, 0, min(len(validated), maxEffectiveGlossaryTerms))
	omitted := 0
	for _, e := range validated {
		isMatch := false
		for _, seg := range segments {
			if glossaryTermMatches(seg.SourceText, e.Source) {
				isMatch = true
				break
			}
		}
		if !isMatch {
			continue
		}
		if len(matched) >= maxEffectiveGlossaryTerms {
			omitted++
			continue
		}
		matched = append(matched, e)
	}
	hashEntries := make([]domain.GlossaryEntry, len(matched))
	for i, e := range matched {
		hashEntries[i] = domain.GlossaryEntry{Source: normalizeGlossarySource(e.Source), Target: e.Target, Note: e.Note}
	}
	payload, err := json.Marshal(hashEntries)
	if err != nil {
		return domain.EffectiveGlossary{}, fmt.Errorf("marshal effective glossary: %w", err)
	}
	h := sha256.Sum256(payload)
	effective := domain.EffectiveGlossary{Entries: matched, OmittedMatches: omitted, OmittedConflicts: omittedConflicts, Hash: hex.EncodeToString(h[:])}
	serialized, err := json.Marshal(effective)
	if err != nil {
		return domain.EffectiveGlossary{}, fmt.Errorf("marshal effective glossary envelope: %w", err)
	}
	if len(serialized) > maxEffectiveGlossaryBytes {
		return domain.EffectiveGlossary{}, fmt.Errorf("effective glossary is %d bytes; maximum is %d", len(serialized), maxEffectiveGlossaryBytes)
	}
	return effective, nil
}

func glossaryForSource(effective domain.EffectiveGlossary, sourceText string) []domain.GlossaryEntry {
	out := make([]domain.GlossaryEntry, 0)
	for _, e := range effective.Entries {
		if glossaryTermMatches(sourceText, e.Source) {
			out = append(out, e)
		}
	}
	return out
}
