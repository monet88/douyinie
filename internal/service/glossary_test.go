package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestGlossaryEffectiveMatchingUnicodeBoundariesAndIdentity(t *testing.T) {
	entries := []domain.GlossaryEntry{
		{Source: " SUPOR ", Target: "Nồi Supor", Note: "Brand"},
		{Source: "北京", Target: "Bắc Kinh"},
		{Source: "khỏe", Target: "healthy"},
		{Source: "unused", Target: "ignored"},
	}
	segments := []domain.TranslationInputSegment{{Index: 0, SourceText: "SUPOR 北京 rất khỏe_mạnh"}}
	eff, err := effectiveGlossary(entries, segments)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff.Entries) != 2 {
		t.Fatalf("effective entries=%d, want 2: %+v", len(eff.Entries), eff.Entries)
	}
	if eff.Entries[0].Target != "Nồi Supor" || eff.Entries[1].Target != "Bắc Kinh" {
		t.Fatalf("target case/order changed: %+v", eff.Entries)
	}

	// Unmatched additions and canonically equivalent source spelling must not change effective identity.
	eff2, err := effectiveGlossary([]domain.GlossaryEntry{
		{Source: "ｓｕｐｏｒ", Target: "Nồi Supor", Note: "Brand"},
		{Source: "北京", Target: "Bắc Kinh"},
		{Source: "other", Target: "other"},
	}, segments)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Hash != eff2.Hash {
		t.Fatalf("equivalent effective glossary hash changed: %s != %s", eff.Hash, eff2.Hash)
	}
}

func TestGlossaryDeterministicFirstWinnerAndLimits(t *testing.T) {
	// First-winner: keep the first canonical entry, omit later duplicates with conflict reporting
	res, conflicts, err := validateGlossaryEntriesWithReport([]domain.GlossaryEntry{
		{Source: "SUPOR", Target: "A", Note: "First"},
		{Source: "ＳＵＰＯＲ", Target: "B", Note: "Second"},
	})
	if err != nil {
		t.Fatalf("expected duplicate conflict to resolve via first-winner without error, got: %v", err)
	}
	if len(res) != 1 || res[0].Target != "A" || res[0].Note != "First" {
		t.Fatalf("expected first entry to win, got: %+v", res)
	}
	if conflicts != 1 {
		t.Fatalf("expected 1 omitted conflict, got %d", conflicts)
	}

	// Explicit NFKC example: first {"ﬁle","A"} then {"file","B"} => winner A, conflict diagnosed
	nfkcRes, nfkcConflicts, err := validateGlossaryEntriesWithReport([]domain.GlossaryEntry{
		{Source: "ﬁle", Target: "A", Note: "Winner"},
		{Source: "file", Target: "B", Note: "Omitted"},
	})
	if err != nil {
		t.Fatalf("expected NFKC duplicate conflict to resolve via first-winner without error, got: %v", err)
	}
	if len(nfkcRes) != 1 || nfkcRes[0].Target != "A" || nfkcRes[0].Note != "Winner" {
		t.Fatalf("expected first NFKC entry to win 'A', got: %+v", nfkcRes)
	}
	if nfkcConflicts != 1 {
		t.Fatalf("expected 1 omitted conflict for NFKC duplicate, got %d", nfkcConflicts)
	}

	// Effective glossary reports omitted conflicts and keeps provenance identical to winner-only
	segments := []domain.TranslationInputSegment{{Index: 0, SourceText: "ﬁle 测试"}}
	effWithConflict, err := effectiveGlossary([]domain.GlossaryEntry{
		{Source: "ﬁle", Target: "A", Note: "Winner"},
		{Source: "file", Target: "B", Note: "Omitted"},
	}, segments)
	if err != nil {
		t.Fatalf("effectiveGlossary failed: %v", err)
	}
	if effWithConflict.OmittedConflicts != 1 {
		t.Fatalf("expected 1 omitted conflict, got %d", effWithConflict.OmittedConflicts)
	}
	if len(effWithConflict.Entries) != 1 || effWithConflict.Entries[0].Target != "A" {
		t.Fatalf("expected first winner 'A', got %+v", effWithConflict.Entries)
	}

	effWinnerOnly, err := effectiveGlossary([]domain.GlossaryEntry{
		{Source: "ﬁle", Target: "A", Note: "Winner"},
	}, segments)
	if err != nil {
		t.Fatalf("effectiveGlossary winner-only failed: %v", err)
	}
	if effWithConflict.Hash != effWinnerOnly.Hash {
		t.Fatalf("semantic hash must be based only on winners: %s != %s", effWithConflict.Hash, effWinnerOnly.Hash)
	}

	// Genuinely invalid entries are strictly rejected
	if _, err := validateGlossaryEntries([]domain.GlossaryEntry{{Source: "", Target: "x"}}); err == nil {
		t.Fatal("expected empty source rejection")
	}
	if _, err := validateGlossaryEntries([]domain.GlossaryEntry{{Source: "x", Target: ""}}); err == nil {
		t.Fatal("expected empty target rejection")
	}
	if _, err := validateGlossaryEntries([]domain.GlossaryEntry{{Source: strings.Repeat("界", 301), Target: "x"}}); err == nil {
		t.Fatal("expected source rune limit rejection")
	}
}

func TestCanonicalGlossaryEqual_FirstWinner(t *testing.T) {
	// Frozen glossary has winning entry; request has duplicates with conflict that normalize to same winner
	frozen := []domain.GlossaryEntry{{Source: "ﬁle", Target: "A", Note: "Winner"}}
	reqWithDuplicates := []domain.GlossaryEntry{
		{Source: "ﬁle", Target: "A", Note: "Winner"},
		{Source: "file", Target: "B", Note: "Omitted"},
	}
	if !CanonicalGlossaryEqual(frozen, reqWithDuplicates) {
		t.Fatal("expected canonically equivalent glossary under first-winner normalization to be equal")
	}
}

func TestCanonicalGlossaryEqual(t *testing.T) {
	a := []domain.GlossaryEntry{{Source: "SUPOR", Target: "Nồi Supor", Note: "Brand"}}
	b := []domain.GlossaryEntry{{Source: " ｓｕｐｏｒ ", Target: "Nồi Supor", Note: "Brand"}}
	c := []domain.GlossaryEntry{{Source: "SUPOR", Target: "Khác", Note: "Brand"}}

	if !CanonicalGlossaryEqual(a, b) {
		t.Fatal("expected equivalent glossaries to be equal")
	}
	if CanonicalGlossaryEqual(a, c) {
		t.Fatal("expected conflicting glossaries not to be equal")
	}
	if !CanonicalGlossaryEqual(nil, nil) {
		t.Fatal("expected empty glossaries to be equal")
	}
}

func TestEffectiveGlossaryEnforcesSerializedEnvelopeLimit(t *testing.T) {
	entries := make([]domain.GlossaryEntry, maxEffectiveGlossaryTerms)
	segments := make([]domain.TranslationInputSegment, maxEffectiveGlossaryTerms)
	for i := range entries {
		source := fmt.Sprintf("term_%03d", i)
		entries[i] = domain.GlossaryEntry{Source: source, Target: strings.Repeat("x", maxGlossaryTargetRunes), Note: strings.Repeat("n", 20)}
		segments[i] = domain.TranslationInputSegment{Index: i, SourceText: source}
	}

	// Tune the entries-only payload to just below the ceiling. Removing one ASCII
	// target byte removes one JSON byte and the target field is never omitted, so this creates the exact case where the
	// entries fit but the EffectiveGlossary envelope/hash metadata does not.
	targetEntriesBytes := maxEffectiveGlossaryBytes - 8
	encoded, _ := json.Marshal(entries)
	toRemove := len(encoded) - targetEntriesBytes
	if toRemove <= 0 {
		t.Fatalf("test setup expected oversized entries payload, got %d bytes", len(encoded))
	}
	for i := range entries {
		if toRemove == 0 {
			break
		}
		remove := min(toRemove, len(entries[i].Target)-1)
		entries[i].Target = entries[i].Target[:len(entries[i].Target)-remove]
		toRemove -= remove
	}
	if toRemove != 0 {
		t.Fatalf("test setup could not trim payload to boundary; %d bytes remain", toRemove)
	}
	hashEntries, _ := json.Marshal(entries)
	envelope, _ := json.Marshal(domain.EffectiveGlossary{Entries: entries, Hash: strings.Repeat("a", 64)})
	if len(hashEntries) > maxEffectiveGlossaryBytes || len(envelope) <= maxEffectiveGlossaryBytes {
		t.Fatalf("test setup did not straddle 64 KiB boundary: entries=%d envelope=%d", len(hashEntries), len(envelope))
	}
	if _, err := effectiveGlossary(entries, segments); err == nil {
		t.Fatalf("expected serialized effective glossary > %d bytes to be rejected (entries=%d envelope=%d)", maxEffectiveGlossaryBytes, len(hashEntries), len(envelope))
	}
}

func TestGlossaryTargetAndNotePreserveUnicodeSemantics(t *testing.T) {
	segments := []domain.TranslationInputSegment{{Index: 0, SourceText: "SUPOR"}}
	a, err := effectiveGlossary([]domain.GlossaryEntry{{Source: "SUPOR", Target: "Ａ", Note: "①"}}, segments)
	if err != nil {
		t.Fatal(err)
	}
	b, err := effectiveGlossary([]domain.GlossaryEntry{{Source: "SUPOR", Target: "A", Note: "1"}}, segments)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash {
		t.Fatal("target/note compatibility-character changes must alter effective glossary identity")
	}
	if a.Entries[0].Target != "Ａ" || a.Entries[0].Note != "①" {
		t.Fatalf("target/note Unicode semantics were normalized away: %+v", a.Entries[0])
	}
}

func TestMeaningGateGlossaryEquivalenceIsScoped(t *testing.T) {
	gate := NewMeaningFirstQAGate()
	res := gate.ValidateSegment("SUPOR TikTok", "Nồi SuporVN", "zh", "vi", []domain.GlossaryEntry{{Source: "SUPOR", Target: "SuporVN"}})
	if res.Passed {
		t.Fatal("glossary equivalence for SUPOR must not waive unrelated TikTok protection")
	}

	res = gate.ValidateSegment("SUPOR", "Nồi SuporVN", "zh", "vi", []domain.GlossaryEntry{{Source: "SUPOR", Target: "SuporVN"}})
	if !res.Passed {
		t.Fatalf("expected scoped glossary equivalence to satisfy SUPOR: %+v", res.Violations)
	}
}

func TestGlossaryRejectsInvisibleFormatRunesExceptJoiners(t *testing.T) {
	// Bug premise: NFKC + TrimSpace is the only normalization applied to a glossary source and it
	// leaves zero-width formatters intact. A term carrying U+200B therefore validates as a legitimate
	// entry yet can never match the rendered source text, silently consuming a glossary slot forever —
	// rejection at ingress is the smallest deterministic contract that prevents that.
	zwsp := "ke\u200byword"
	if !strings.ContainsRune(domain.NormalizeGlossarySource(zwsp), '\u200b') {
		t.Fatalf("premise broken: normalization now strips U+200B from %q", zwsp)
	}
	if domain.GlossaryTermMatches("keyword", zwsp) {
		t.Fatalf("premise broken: %q unexpectedly matches its rendered form 'keyword'", zwsp)
	}

	entries := []domain.GlossaryEntry{{Source: zwsp, Target: "Từ khóa"}}
	resolved, conflicts, err := ValidateGlossaryEntriesWithReport(entries)
	if err == nil {
		t.Fatalf("expected U+200B source to be rejected, got %+v", resolved)
	}
	if resolved != nil || conflicts != 0 {
		t.Fatalf("rejected glossary must not be partially returned, got %+v/%d", resolved, conflicts)
	}
	if !strings.Contains(err.Error(), "disallowed format character") {
		t.Fatalf("expected disallowed-format-character error, got %v", err)
	}
	// The non-report variant is the ingress contract for a run snapshot: reject with zero mutation.
	if got, err := ValidateGlossaryEntries(entries); err == nil || got != nil {
		t.Fatalf("expected ValidateGlossaryEntries to reject U+200B with zero mutation, got %+v/%v", got, err)
	}

	// ZWNJ/ZWJ are genuine script joiners and must never be over-rejected.
	joiners := []domain.GlossaryEntry{
		{Source: "می\u200cخواهم", Target: "tôi muốn"},
		{Source: "👨\u200d👩", Target: "đôi"},
	}
	kept, err := ValidateGlossaryEntries(joiners)
	if err != nil {
		t.Fatalf("expected ZWNJ/ZWJ sources to be accepted, got %v", err)
	}
	if len(kept) != len(joiners) {
		t.Fatalf("expected both joiner entries kept, got %+v", kept)
	}
	for i, joiner := range []rune{'\u200c', '\u200d'} {
		if !strings.ContainsRune(kept[i].Source, joiner) {
			t.Fatalf("joiner entry %d lost U+%04X: %q", i, joiner, kept[i].Source)
		}
	}
}

func TestGlossaryOmittedConflictCountRequiresOriginalRequest(t *testing.T) {
	// Conflicting duplicates (same normalized source, different target/note): first-wins, count 1.
	conflicting := []domain.GlossaryEntry{
		{Source: "SUPOR", Target: "Nồi Supor", Note: "first"},
		{Source: "ｓｕｐｏｒ", Target: "Supor khác", Note: "second"},
	}
	deduped, conflicts, err := ValidateGlossaryEntriesWithReport(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if conflicts != 1 {
		t.Fatalf("expected 1 omitted conflict, got %d", conflicts)
	}
	if len(deduped) != 1 || deduped[0].Target != "Nồi Supor" || deduped[0].Note != "first" {
		t.Fatalf("expected deterministic first-wins entry, got %+v", deduped)
	}
	plain, err := ValidateGlossaryEntries(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if !CanonicalGlossaryEqual(plain, deduped) {
		t.Fatalf("count-dropping variant diverged from report variant: %+v vs %+v", plain, deduped)
	}

	// Identical duplicates are deduped but are NOT conflicts.
	identical, identicalConflicts, err := ValidateGlossaryEntriesWithReport([]domain.GlossaryEntry{
		{Source: "北京", Target: "Bắc Kinh", Note: "city"},
		{Source: "北京", Target: "Bắc Kinh", Note: "city"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if identicalConflicts != 0 {
		t.Fatalf("expected identical duplicates to report 0 conflicts, got %d", identicalConflicts)
	}
	if len(identical) != 1 {
		t.Fatalf("expected identical duplicates deduped to 1 entry, got %+v", identical)
	}

	// Bug premise: the deduped list alone cannot reproduce the count, and the semantic hash is
	// identical either way — so an ingress that freezes a run glossary MUST persist the count
	// returned alongside the list, or EffectiveGlossary reports 0 conflicts for the frozen run.
	segments := []domain.TranslationInputSegment{{Index: 0, SourceText: "SUPOR"}}
	fromDeduped, err := effectiveGlossary(deduped, segments)
	if err != nil {
		t.Fatal(err)
	}
	fromOriginal, err := effectiveGlossary(conflicting, segments)
	if err != nil {
		t.Fatal(err)
	}
	if fromDeduped.OmittedConflicts != 0 {
		t.Fatalf("expected deduped list to report 0 conflicts, got %d", fromDeduped.OmittedConflicts)
	}
	if fromOriginal.OmittedConflicts != 1 {
		t.Fatalf("expected original request to report 1 conflict, got %d", fromOriginal.OmittedConflicts)
	}
	if fromDeduped.Hash != fromOriginal.Hash {
		t.Fatalf("conflict count is not recoverable from semantics: hash %s != %s", fromDeduped.Hash, fromOriginal.Hash)
	}

	// Invalid input still fails with zero mutation (nil list, zero count).
	for _, bad := range [][]domain.GlossaryEntry{
		{{Source: "", Target: "x"}},
		{{Source: "x", Target: ""}},
		{{Source: strings.Repeat("界", maxGlossarySourceRunes+1), Target: "x"}},
	} {
		res, count, err := ValidateGlossaryEntriesWithReport(bad)
		if err == nil {
			t.Fatalf("expected error for %+v", bad)
		}
		if res != nil || count != 0 {
			t.Fatalf("invalid input must not mutate or return state, got %+v/%d", res, count)
		}
	}
}

func TestGlossaryMatchesLatinNextToCJK(t *testing.T) {
	entries := []domain.GlossaryEntry{
		{Source: "AI", Target: "Trí tuệ nhân tạo"},
	}
	// "AI" adjacent to CJK rune "模" must match because CJK runes are not Latin word runes
	segments := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "这是AI模型训练"},
	}
	eff, err := effectiveGlossary(entries, segments)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff.Entries) != 1 || eff.Entries[0].Target != "Trí tuệ nhân tạo" {
		t.Fatalf("expected 'AI' to match in 'AI模型', got %+v", eff.Entries)
	}

	// "AI" inside Latin word "AIR" must NOT match
	segmentsNeg := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "AIR condition"},
	}
	effNeg, err := effectiveGlossary(entries, segmentsNeg)
	if err != nil {
		t.Fatal(err)
	}
	if len(effNeg.Entries) != 0 {
		t.Fatalf("expected 'AI' NOT to match in 'AIR', got %+v", effNeg.Entries)
	}
}
