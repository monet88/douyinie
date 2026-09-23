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

func TestGlossaryRejectsConflictsAndLimits(t *testing.T) {
	if _, err := validateGlossaryEntries([]domain.GlossaryEntry{{Source: "SUPOR", Target: "A"}, {Source: "ＳＵＰＯＲ", Target: "B"}}); err == nil {
		t.Fatal("expected canonical duplicate conflict")
	}
	if _, err := validateGlossaryEntries([]domain.GlossaryEntry{{Source: strings.Repeat("界", 301), Target: "x"}}); err == nil {
		t.Fatal("expected source rune limit rejection")
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
