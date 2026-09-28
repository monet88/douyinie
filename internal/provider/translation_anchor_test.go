package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestValidateCanonicalBatch(t *testing.T) {
	// Empty batch fails with ErrEmptyTranslationInput
	if err := ValidateCanonicalBatch(nil); !errors.Is(err, domain.ErrEmptyTranslationInput) {
		t.Fatalf("expected ErrEmptyTranslationInput, got %v", err)
	}

	// Repeated index fails
	dup := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "hello"},
		{Index: 0, SourceText: "world"},
	}
	if err := ValidateCanonicalBatch(dup); err == nil || !strings.Contains(err.Error(), "repeats index 0") {
		t.Fatalf("expected repeats index error, got %v", err)
	} else if !errors.Is(err, domain.ErrInvalidCanonicalBatch) {
		t.Fatalf("repeated index must be classified as domain.ErrInvalidCanonicalBatch, got %v", err)
	}

	// Over 512 segments fails
	oversize := make([]domain.TranslationInputSegment, 513)
	for i := range oversize {
		oversize[i] = domain.TranslationInputSegment{Index: i, SourceText: "a"}
	}
	if err := ValidateCanonicalBatch(oversize); err == nil || !strings.Contains(err.Error(), "exceeding the 512-segment bound") {
		t.Fatalf("expected segment count bound error, got %v", err)
	} else if !errors.Is(err, domain.ErrInvalidCanonicalBatch) {
		t.Fatalf("segment count bound must be classified as domain.ErrInvalidCanonicalBatch, got %v", err)
	}

	// Over 256 KiB source bytes fails
	hugeSource := make([]domain.TranslationInputSegment, 2)
	hugeSource[0] = domain.TranslationInputSegment{Index: 0, SourceText: strings.Repeat("a", (256<<10)+1)}
	hugeSource[1] = domain.TranslationInputSegment{Index: 1, SourceText: "b"}
	if err := ValidateCanonicalBatch(hugeSource); err == nil || !strings.Contains(err.Error(), "exceeding the 262144-byte batch bound") {
		t.Fatalf("expected byte count bound error, got %v", err)
	} else if !errors.Is(err, domain.ErrInvalidCanonicalBatch) {
		t.Fatalf("byte bound must be classified as domain.ErrInvalidCanonicalBatch, got %v", err)
	}

	// Empty batch stays classified as ErrEmptyTranslationInput, not as a generic invalid batch.
	if err := ValidateCanonicalBatch(nil); errors.Is(err, domain.ErrInvalidCanonicalBatch) {
		t.Fatalf("empty batch must not be classified as domain.ErrInvalidCanonicalBatch, got %v", err)
	}

	// Valid batch succeeds
	valid := []domain.TranslationInputSegment{
		{Index: 10, SourceText: "segment 10"},
		{Index: 20, SourceText: "segment 20"},
	}
	if err := ValidateCanonicalBatch(valid); err != nil {
		t.Fatalf("expected valid batch to pass, got %v", err)
	}
}

func TestNormalizeEchoText(t *testing.T) {
	// NFKC normalization, whitespace removal, punctuation removal, case folding
	cases := []struct {
		input    string
		expected string
	}{
		{"Hello, World!", "helloworld"},
		{"  你好，世界！  ", "你好世界"},
		{"SUPOR 500ml", "supor500ml"},
		{"A & B: C - D", "abcd"},
	}
	for _, c := range cases {
		got := normalizeEchoText(c.input)
		if got != c.expected {
			t.Errorf("normalizeEchoText(%q) = %q, expected %q", c.input, got, c.expected)
		}
	}
}

func TestDecodeGatewaySegments_DuplicatesAndExtras(t *testing.T) {
	expected := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "Hello"},
		{Index: 1, SourceText: "World"},
	}

	// Duplicate index reported as structural violation
	dupJSON := `{"segments": [
		{"index": 0, "source_text": "Hello", "target_text": "Xin chào"},
		{"index": 0, "source_text": "Hello", "target_text": "Chào bạn"},
		{"index": 1, "source_text": "World", "target_text": "Thế giới"}
	]}`
	decoded, structural, err := decodeGatewaySegments(dupJSON, expected)
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if len(structural) != 1 || !strings.Contains(structural[0], "repeated segment index 0") {
		t.Fatalf("expected repeated index violation, got: %+v", structural)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 unique decoded entries, got %d", len(decoded))
	}

	// Extra unrequested index reported as structural violation
	extraJSON := `{"segments": [
		{"index": 0, "source_text": "Hello", "target_text": "Xin chào"},
		{"index": 1, "source_text": "World", "target_text": "Thế giới"},
		{"index": 99, "source_text": "Extra", "target_text": "Thừa"}
	]}`
	decoded, structural, err = decodeGatewaySegments(extraJSON, expected)
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if len(structural) != 1 || !strings.Contains(structural[0], "unrequested segment index 99") {
		t.Fatalf("expected unrequested index violation, got: %+v", structural)
	}
	if _, has99 := decoded[99]; has99 {
		t.Fatalf("unrequested index must not be decoded into usable map")
	}
}

func TestAuditAnchoredBatch_ShiftedAndMissingEcho(t *testing.T) {
	expected := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "First sentence"},
		{Index: 1, SourceText: "Second sentence"},
	}

	// Case 1: Shifted echo (index 0 returns echo of index 1)
	decodedShifted := map[int]gatewaySegment{
		0: {Index: 0, SourceText: "Second sentence", TargetText: "Câu thứ hai"},
		1: {Index: 1, SourceText: "First sentence", TargetText: "Câu thứ nhất"},
	}
	auditShifted := auditAnchoredBatch(expected, decodedShifted, "vi", domain.EffectiveGlossary{})
	if len(auditShifted.Structural) == 0 {
		t.Fatalf("expected structural violation for shifted echo, got none")
	}
	if !strings.Contains(auditShifted.Structural[0], "echoes the source of index 1") {
		t.Fatalf("expected shifted echo message, got: %v", auditShifted.Structural)
	}

	// Case 2: Missing echo entirely
	decodedMissingEcho := map[int]gatewaySegment{
		0: {Index: 0, SourceText: "", TargetText: "Câu thứ nhất"},
		1: {Index: 1, SourceText: "Second sentence", TargetText: "Câu thứ hai"},
	}
	auditMissingEcho := auditAnchoredBatch(expected, decodedMissingEcho, "vi", domain.EffectiveGlossary{})
	if len(auditMissingEcho.Failures) != 1 || auditMissingEcho.Failures[0].Kind != anchorMissingEcho {
		t.Fatalf("expected anchorMissingEcho failure, got: %+v", auditMissingEcho.Failures)
	}

	// Case 3: Empty target text
	decodedEmptyTarget := map[int]gatewaySegment{
		0: {Index: 0, SourceText: "First sentence", TargetText: "   "},
		1: {Index: 1, SourceText: "Second sentence", TargetText: "Câu thứ hai"},
	}
	auditEmptyTarget := auditAnchoredBatch(expected, decodedEmptyTarget, "vi", domain.EffectiveGlossary{})
	if len(auditEmptyTarget.Failures) != 1 || auditEmptyTarget.Failures[0].Kind != anchorEmptyTarget {
		t.Fatalf("expected anchorEmptyTarget failure, got: %+v", auditEmptyTarget.Failures)
	}

	// Case 4: Valid shuffled results restore order
	decodedShuffled := map[int]gatewaySegment{
		1: {Index: 1, SourceText: "Second sentence", TargetText: "Câu thứ hai"},
		0: {Index: 0, SourceText: "First sentence", TargetText: "Câu thứ nhất"},
	}
	auditShuffled := auditAnchoredBatch(expected, decodedShuffled, "vi", domain.EffectiveGlossary{})
	if !auditShuffled.usable() {
		t.Fatalf("expected valid shuffled output to be usable, got structural: %v failures: %v", auditShuffled.Structural, auditShuffled.Failures)
	}
	if len(auditShuffled.Entries) != 2 {
		t.Fatalf("expected 2 usable entries, got %d", len(auditShuffled.Entries))
	}
}

func TestClassifyCopy_And_PromotionRules(t *testing.T) {
	// A2: Copy detection classes:
	// - Excluded: numeric-only, URL/email, same-script text, authorized unchanged term
	// - Weak: short/proper-name/title (cross-script, rune count <= 10)
	// - Strong: meaningful cross-script unchanged copy (rune count > 10)
	glossary := domain.EffectiveGlossary{
		Entries: []domain.GlossaryEntry{
			{Source: "SUPOR", Target: "SUPOR"},
		},
	}

	// Numeric-only -> excluded
	if v := classifyCopy("12345", "12345", "vi", glossary); v != copyExcluded {
		t.Errorf("expected numeric copy to be excluded, got %v", v)
	}

	// URL -> excluded
	if v := classifyCopy("https://example.com/foo", "https://example.com/foo", "vi", glossary); v != copyExcluded {
		t.Errorf("expected URL copy to be excluded, got %v", v)
	}

	// Same-script (Latin to Latin) -> excluded
	if v := classifyCopy("Bonjour le monde", "Bonjour le monde", "vi", glossary); v != copyExcluded {
		t.Errorf("expected same-script Latin copy to be excluded, got %v", v)
	}

	// Authorized unchanged term -> excluded
	if v := classifyCopy("SUPOR", "SUPOR", "vi", glossary); v != copyExcluded {
		t.Errorf("expected authorized unchanged term to be excluded, got %v", v)
	}

	// Short cross-script copy (<=10 runes) -> weak
	// "你好世界" is 4 runes CJK -> target "你好世界" for targetLang "vi"
	if v := classifyCopy("你好世界", "你好世界", "vi", glossary); v != copyWeak {
		t.Errorf("expected short CJK copy to be weak, got %v", v)
	}

	// Long cross-script copy (>10 runes) -> strong
	// "这是一段非常长的主持人对话内容" is 15 runes CJK
	longCJK := "这是一段非常长的主持人对话内容"
	if v := classifyCopy(longCJK, longCJK, "vi", glossary); v != copyStrong {
		t.Errorf("expected long CJK copy to be strong, got %v", v)
	}
}

func TestAuditAnchoredBatch_CopyPromotionBoundary(t *testing.T) {
	// Promotion rule: promote weak exact cross-script copies ONLY after:
	// strong >= 2 AND strong * 3 > batch_size.
	// Weak copies NEVER increase the strong count.

	longCJK1 := "第一段非常长的主持人解说内容啊" // 15 runes -> strong
	longCJK2 := "第二段非常长的主持人解说内容啊" // 15 runes -> strong
	shortCJK := "短标题"             // 3 runes -> weak

	// Batch size 5:
	// If strong = 1, weak = 1 -> strong < 2 -> no promotion. Only strong is a failure.
	expected5 := []domain.TranslationInputSegment{
		{Index: 0, SourceText: longCJK1},
		{Index: 1, SourceText: shortCJK},
		{Index: 2, SourceText: "正常翻译段落一"},
		{Index: 3, SourceText: "正常翻译段落二"},
		{Index: 4, SourceText: "正常翻译段落三"},
	}
	decoded5_1 := map[int]gatewaySegment{
		0: {Index: 0, SourceText: longCJK1, TargetText: longCJK1}, // strong
		1: {Index: 1, SourceText: shortCJK, TargetText: shortCJK}, // weak
		2: {Index: 2, SourceText: "正常翻译段落一", TargetText: "Đoạn dịch bình thường một"},
		3: {Index: 3, SourceText: "正常翻译段落二", TargetText: "Đoạn dịch bình thường hai"},
		4: {Index: 4, SourceText: "正常翻译段落三", TargetText: "Đoạn dịch bình thường ba"},
	}
	audit5_1 := auditAnchoredBatch(expected5, decoded5_1, "vi", domain.EffectiveGlossary{})
	// Only segment 0 should fail (strong), segment 1 should NOT be promoted because strong=1 < 2
	if len(audit5_1.Failures) != 1 || audit5_1.Failures[0].Index != 0 {
		t.Fatalf("expected exactly 1 failure for segment 0 when strong=1, got: %+v", audit5_1.Failures)
	}

	// Now batch size 6 with strong = 2:
	// strong * 3 = 6. Condition is strong * 3 > batch_size (6 > 6 is false).
	// So weak copies should NOT be promoted!
	expected6 := []domain.TranslationInputSegment{
		{Index: 0, SourceText: longCJK1},
		{Index: 1, SourceText: longCJK2},
		{Index: 2, SourceText: shortCJK},
		{Index: 3, SourceText: "正常翻译段落一"},
		{Index: 4, SourceText: "正常翻译段落二"},
		{Index: 5, SourceText: "正常翻译段落三"},
	}
	decoded6 := map[int]gatewaySegment{
		0: {Index: 0, SourceText: longCJK1, TargetText: longCJK1}, // strong
		1: {Index: 1, SourceText: longCJK2, TargetText: longCJK2}, // strong
		2: {Index: 2, SourceText: shortCJK, TargetText: shortCJK}, // weak
		3: {Index: 3, SourceText: "正常翻译段落一", TargetText: "Đoạn dịch 1"},
		4: {Index: 4, SourceText: "正常翻译段落二", TargetText: "Đoạn dịch 2"},
		5: {Index: 5, SourceText: "正常翻译段落三", TargetText: "Đoạn dịch 3"},
	}
	audit6 := auditAnchoredBatch(expected6, decoded6, "vi", domain.EffectiveGlossary{})
	// strong = 2, strong*3 = 6 == len(expected6). Not strictly greater! Weak is NOT promoted.
	if len(audit6.Failures) != 2 {
		t.Fatalf("expected exactly 2 failures (strong only) when strong*3 == batch_size, got: %+v", audit6.Failures)
	}
	if audit6.Failures[0].Index != 0 || audit6.Failures[1].Index != 1 {
		t.Fatalf("expected failures on segments 0 and 1, got: %+v", audit6.Failures)
	}

	// Now batch size 5 with strong = 2:
	// strong = 2 >= 2 AND strong * 3 = 6 > 5 (true!).
	// Weak copy on segment 2 MUST be promoted!
	expected5_2 := []domain.TranslationInputSegment{
		{Index: 0, SourceText: longCJK1},
		{Index: 1, SourceText: longCJK2},
		{Index: 2, SourceText: shortCJK},
		{Index: 3, SourceText: "正常翻译段落一"},
		{Index: 4, SourceText: "正常翻译段落二"},
	}
	decoded5_2 := map[int]gatewaySegment{
		0: {Index: 0, SourceText: longCJK1, TargetText: longCJK1}, // strong
		1: {Index: 1, SourceText: longCJK2, TargetText: longCJK2}, // strong
		2: {Index: 2, SourceText: shortCJK, TargetText: shortCJK}, // weak -> promoted!
		3: {Index: 3, SourceText: "正常翻译段落一", TargetText: "Đoạn dịch 1"},
		4: {Index: 4, SourceText: "正常翻译段落二", TargetText: "Đoạn dịch 2"},
	}
	audit5_2 := auditAnchoredBatch(expected5_2, decoded5_2, "vi", domain.EffectiveGlossary{})
	if len(audit5_2.Failures) != 3 {
		t.Fatalf("expected 3 failures (2 strong + 1 promoted weak) when strong*3 > batch_size, got %d: %+v", len(audit5_2.Failures), audit5_2.Failures)
	}
}

func TestGatewayTranslationProvider_RepairProtocol_MalformedAndNeighborEditRejected(t *testing.T) {
	ctx := context.Background()
	var callNum int32

	// 3 segments: 1 out of 3 missing (1*3 == 3, not > 1/3, so skips whole-batch repair and goes to targeted).
	// Targeted attempt 1: returns neighbor index 0 alongside index 1 (neighbor edit/merge -> rejected!).
	// Targeted attempt 2: returns malformed JSON -> rejected!
	// Targeted attempt 3: returns valid repair for index 1 -> succeeds!
	var capturedNeighbors []int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callNum, 1)
		var reqBody struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		var content string
		switch n {
		case 1:
			// Initial batch: segment 1 has empty target
			content = `{"segments":[
				{"index":0,"source_text":"第一句","target_text":"Câu một"},
				{"index":1,"source_text":"第二句","target_text":""},
				{"index":2,"source_text":"第三句","target_text":"Câu ba"}
			]}`
		case 2:
			// Targeted attempt 1: inspect neighbors in user payload, then return neighbor edit (extra index 0)
			var userPayload struct {
				Neighbors []struct {
					Index int `json:"index"`
				} `json:"read_only_context_neighbors"`
			}
			if len(reqBody.Messages) > 1 {
				_ = json.Unmarshal([]byte(reqBody.Messages[1].Content), &userPayload)
				for _, nb := range userPayload.Neighbors {
					capturedNeighbors = append(capturedNeighbors, nb.Index)
				}
			}
			content = `{"segments":[
				{"index":0,"source_text":"第一句","target_text":"Edited neighbor"},
				{"index":1,"source_text":"第二句","target_text":"Câu hai"}
			]}`
		case 3:
			// Targeted attempt 2: malformed JSON
			content = `{malformed json`
		case 4:
			// Targeted attempt 3: valid single entry
			content = `{"segments":[
				{"index":1,"source_text":"第二句","target_text":"Câu hai chuẩn"}
			]}`
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                 "chatcmpl-repair",
			"model":              "gemini-3.8-flash-001",
			"system_fingerprint": "fp_consistent",
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"},
			},
		})
	}))
	defer ts.Close()

	p, err := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-test",
		0.99,
		func(context.Context, string, string) (string, error) { return "tok", nil },
		ts.Client(),
		ts.URL,
	)
	if err != nil {
		t.Fatalf("NewGatewayTranslationProvider: %v", err)
	}

	res, err := p.TranslateText(ctx, TranslationRequest{
		RunID:                 "run-repair-1",
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		AuthorizedCredentials: []string{"cred"},
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "第一句", SpeakerID: "S0", StartMs: 100, EndMs: 200},
			{Index: 1, SourceText: "第二句", SpeakerID: "S1", StartMs: 200, EndMs: 300},
			{Index: 2, SourceText: "第三句", SpeakerID: "S0", StartMs: 300, EndMs: 400},
		},
	})
	if err != nil {
		t.Fatalf("expected repair to succeed on 3rd targeted attempt, got: %v", err)
	}
	if atomic.LoadInt32(&callNum) != 4 {
		t.Fatalf("expected 4 wire calls (1 initial + 3 targeted), got %d", callNum)
	}
	if len(capturedNeighbors) != 2 || capturedNeighbors[0] != 0 || capturedNeighbors[1] != 2 {
		t.Fatalf("expected neighbors [0, 2], got %+v", capturedNeighbors)
	}
	if res.Segments[1].TargetText != "Câu hai chuẩn" || res.Segments[1].SpeakerID != "S1" || res.Segments[1].StartMs != 200 {
		t.Fatalf("unexpected repaired segment 1: %+v", res.Segments[1])
	}
}

// TestGatewayTranslationProvider_WholeBatchRepairWireFailurePrefixes pins the observable prefix of a
// failed whole-batch repair wire call per recovery path, while both stay ErrQualityRejected-classified
// and consume exactly one repair budget call.
func TestGatewayTranslationProvider_WholeBatchRepairWireFailurePrefixes(t *testing.T) {
	envelope := func(content string) string {
		b, _ := json.Marshal(map[string]any{
			"model": "gemini-3.8-flash-001",
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"},
			},
		})
		return string(b)
	}
	cases := []struct {
		name        string
		initialBody func() string // raw initial response body
		wantPrefix  string
	}{
		{
			// Undecodable envelope -> repairable initial wire error -> "Initial request failed" recovery path.
			name:        "malformed initial response keeps the initial-request prefix",
			initialBody: func() string { return `{malformed json` },
			wantPrefix:  "whole-batch repair wire call failed",
		},
		{
			// Duplicate index -> batch-global structural failure -> anchoring recovery path.
			name: "anchoring failure keeps the anchoring prefix",
			initialBody: func() string {
				return envelope(`{"segments":[
					{"index":0,"source_text":"第一句","target_text":"Câu một"},
					{"index":0,"source_text":"第一句","target_text":"Câu một"}
				]}`)
			},
			wantPrefix: "whole-batch repair failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var callNum int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := atomic.AddInt32(&callNum, 1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					_, _ = w.Write([]byte(tc.initialBody()))
					return
				}
				// Repair wire call fails in the same repairable class (undecodable envelope).
				_, _ = w.Write([]byte(`{malformed json`))
			}))
			defer ts.Close()

			p, err := NewGatewayTranslationProvider(
				GatewayGeminiTranslationProviderID,
				GatewayGeminiModelAlias,
				"baseline-test",
				0.99,
				func(context.Context, string, string) (string, error) { return "tok", nil },
				ts.Client(),
				ts.URL,
			)
			if err != nil {
				t.Fatalf("NewGatewayTranslationProvider: %v", err)
			}

			_, err = p.TranslateText(context.Background(), TranslationRequest{
				RunID:                 "run-repair-prefix",
				SourceLanguage:        "zh",
				TargetLanguage:        "vi",
				AuthorizedCredentials: []string{"cred"},
				Segments: []domain.TranslationInputSegment{
					{Index: 0, SourceText: "第一句"},
					{Index: 1, SourceText: "第二句"},
				},
			})
			if err == nil {
				t.Fatalf("expected whole-batch repair failure to surface")
			}
			if !errors.Is(err, domain.ErrQualityRejected) {
				t.Fatalf("expected ErrQualityRejected classification, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantPrefix) {
				t.Fatalf("expected prefix %q, got %v", tc.wantPrefix, err)
			}
			if got := atomic.LoadInt32(&callNum); got != 2 {
				t.Fatalf("expected exactly 2 wire calls (1 initial + 1 whole-batch repair), got %d", got)
			}
		})
	}
}

func TestGatewayTranslationProvider_CeilingsAndCancellation(t *testing.T) {
	// 1. Response > 1 MiB overflow fails closed
	tsOverflow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gemini-3.8-flash","choices":[{"message":{"content":"` + strings.Repeat("x", (1<<20)+100) + `"}}]}`))
	}))
	defer tsOverflow.Close()

	pOver, _ := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-test",
		0.99,
		func(context.Context, string, string) (string, error) { return "tok", nil },
		tsOverflow.Client(),
		tsOverflow.URL,
	)
	_, err := pOver.TranslateText(context.Background(), TranslationRequest{
		RunID:                 "run-over",
		AuthorizedCredentials: []string{"cred"},
		Segments:              []domain.TranslationInputSegment{{Index: 0, SourceText: "你好"}},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeded 1 MiB bound") {
		t.Fatalf("expected 1 MiB overflow error, got: %v", err)
	}

	// 2. Context cancellation stops immediately and returns context.Canceled without wrapping as ErrQualityRejected
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = pOver.TranslateText(ctxCancel, TranslationRequest{
		RunID:                 "run-cancel",
		AuthorizedCredentials: []string{"cred"},
		Segments:              []domain.TranslationInputSegment{{Index: 0, SourceText: "你好"}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if errors.Is(err, domain.ErrQualityRejected) {
		t.Fatalf("cancelled request must not wrap ErrQualityRejected: %v", err)
	}

	// 3. Budget exhaustion at 14 wire calls across retry re-entry
	var wireCount int32
	tsExhaust := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&wireCount, 1)
		w.Header().Set("Content-Type", "application/json")
		// Always return empty targets so whole-batch + targeted repairs keep firing until budget hits ceiling
		_, _ = w.Write([]byte(`{"model":"gemini-3.8-flash","choices":[{"message":{"content":"{\"segments\":[{\"index\":0,\"source_text\":\"s0\",\"target_text\":\"\"},{\"index\":1,\"source_text\":\"s1\",\"target_text\":\"\"},{\"index\":2,\"source_text\":\"s2\",\"target_text\":\"\"},{\"index\":3,\"source_text\":\"s3\",\"target_text\":\"\"},{\"index\":4,\"source_text\":\"s4\",\"target_text\":\"\"}]}"}}]}`))
	}))
	defer tsExhaust.Close()

	pEx, _ := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-test",
		0.99,
		func(context.Context, string, string) (string, error) { return "tok", nil },
		tsExhaust.Client(),
		tsExhaust.URL,
	)
	sharedBudget := NewCandidateWireBudget()
	// Pre-consume 10 wire calls (simulating prior transport-retry re-entry on same candidate)
	sharedBudget.TotalCalls = 10
	_, err = pEx.TranslateText(context.Background(), TranslationRequest{
		RunID:                 "run-exhaust",
		AuthorizedCredentials: []string{"cred"},
		Budget:                sharedBudget,
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "s0"},
			{Index: 1, SourceText: "s1"},
			{Index: 2, SourceText: "s2"},
			{Index: 3, SourceText: "s3"},
			{Index: 4, SourceText: "s4"},
		},
	})
	if !errors.Is(err, domain.ErrQualityRejected) || !strings.Contains(err.Error(), "ceiling reached") {
		t.Fatalf("expected ceiling reached ErrQualityRejected, got: %v", err)
	}
	if atomic.LoadInt32(&wireCount) != 4 || sharedBudget.TotalCalls != 14 {
		t.Fatalf("expected exactly 4 additional calls to reach 14 total, got wireCount=%d total=%d", wireCount, sharedBudget.TotalCalls)
	}
}

func TestGatewayTranslationProvider_ProvenanceAggregationAndConcurrency(t *testing.T) {
	// 1. Model disagreement across repair subrequests fails closed with ErrInconsistentProvenance
	var step int32
	tsDisagree := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&step, 1)
		model := "gemini-3.8-flash-001"
		content := `{"segments":[{"index":0,"source_text":"你好","target_text":""}]}`
		if n == 2 {
			model = "gemini-3.8-flash-002-different"
			content = `{"segments":[{"index":0,"source_text":"你好","target_text":"Xin chào"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":              model,
			"system_fingerprint": "fp_1",
			"choices":            []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer tsDisagree.Close()

	pDis, _ := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-test",
		0.99,
		func(context.Context, string, string) (string, error) { return "tok", nil },
		tsDisagree.Client(),
		tsDisagree.URL,
	)
	_, err := pDis.TranslateText(context.Background(), TranslationRequest{
		RunID:                 "run-disagree",
		AuthorizedCredentials: []string{"cred"},
		Segments:              []domain.TranslationInputSegment{{Index: 0, SourceText: "你好"}},
	})
	if !errors.Is(err, domain.ErrInconsistentProvenance) {
		t.Fatalf("expected ErrInconsistentProvenance on model disagreement, got: %v", err)
	}

	// 2. Mixed fingerprints result in empty SystemFingerprint, while concurrent calls do not contaminate each other
	tsConcurrent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		userMsg := body.Messages[len(body.Messages)-1].Content
		model := "model-A"
		fp := "fp-A"
		content := `{"segments":[{"index":0,"source_text":"段A","target_text":"Đoạn A"}]}`
		if strings.Contains(userMsg, "段B") {
			model = "model-B"
			fp = "fp-B"
			content = `{"segments":[{"index":0,"source_text":"段B","target_text":"Đoạn B"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":              model,
			"system_fingerprint": fp,
			"choices":            []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer tsConcurrent.Close()

	pConc, _ := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-conc",
		0.99,
		func(context.Context, string, string) (string, error) { return "tok", nil },
		tsConcurrent.Client(),
		tsConcurrent.URL,
	)

	var wg sync.WaitGroup
	wg.Add(2)
	var resA, resB *TranslationResult
	var errA, errB error
	go func() {
		defer wg.Done()
		resA, errA = pConc.TranslateText(context.Background(), TranslationRequest{
			RunID:                 "run-A",
			AuthorizedCredentials: []string{"cred"},
			Segments:              []domain.TranslationInputSegment{{Index: 0, SourceText: "段A"}},
		})
	}()
	go func() {
		defer wg.Done()
		resB, errB = pConc.TranslateText(context.Background(), TranslationRequest{
			RunID:                 "run-B",
			AuthorizedCredentials: []string{"cred"},
			Segments:              []domain.TranslationInputSegment{{Index: 0, SourceText: "段B"}},
		})
	}()
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("concurrent calls failed: errA=%v errB=%v", errA, errB)
	}
	if resA.ObservedModel != "model-A" || resA.SystemFingerprint != "fp-A" {
		t.Fatalf("call A contaminated: %+v", resA)
	}
	if resB.ObservedModel != "model-B" || resB.SystemFingerprint != "fp-B" {
		t.Fatalf("call B contaminated: %+v", resB)
	}
}

func TestClassifyCopy_AuthorizedUnchangedTerm_ExactTermOnly(t *testing.T) {
	glossary := domain.EffectiveGlossary{
		Entries: []domain.GlossaryEntry{
			{Source: "SUPOR", Target: "SUPOR"},
		},
	}

	// 1. The exact authorized term is excluded from copy detection.
	if got := classifyCopy("SUPOR", "SUPOR", "vi", glossary); got != copyExcluded {
		t.Fatalf("expected exact authorized term to be copyExcluded, got: %v", got)
	}

	// 2. A larger untranslated sentence that merely contains the term is NOT exempt.
	longSentence := "今天我们深入评测这款SUPOR厨房神器"
	if got := classifyCopy(longSentence, longSentence, "vi", glossary); got != copyStrong {
		t.Fatalf("untranslated sentence containing a glossary term must be copyStrong, got: %v", got)
	}
}

func TestGatewayTranslationProvider_TargetedRepair_RejectsWeakCopy(t *testing.T) {
	var callNum int32
	var targetedCalls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callNum, 1)
		var content string
		switch n {
		case 1:
			// Initial batch: 1 empty target out of 4 stays below the >1/3 whole-batch threshold, so
			// segment 0 is addressed by targeted repair.
			content = `{"segments":[
				{"index":0,"source_text":"小王","target_text":""},
				{"index":1,"source_text":"第一句话","target_text":"Câu thứ nhất"},
				{"index":2,"source_text":"第二句话","target_text":"Câu thứ hai"},
				{"index":3,"source_text":"第三句话","target_text":"Câu thứ ba"}
			]}`
		case 2:
			// Targeted attempt 1: untranslated weak copy must be rejected.
			atomic.AddInt32(&targetedCalls, 1)
			content = `{"segments":[{"index":0,"source_text":"小王","target_text":"小王"}]}`
		default:
			// Targeted attempt 2: accepted translation.
			atomic.AddInt32(&targetedCalls, 1)
			content = `{"segments":[{"index":0,"source_text":"小王","target_text":"Tiểu Vương"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "gemini-3.8-flash-001",
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer ts.Close()

	p, _ := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-test",
		0.99,
		func(context.Context, string, string) (string, error) { return "tok", nil },
		ts.Client(),
		ts.URL,
	)

	res, err := p.TranslateText(context.Background(), TranslationRequest{
		RunID:                 "run-weak-copy-reject",
		TargetLanguage:        "vi",
		AuthorizedCredentials: []string{"cred"},
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "小王"},
			{Index: 1, SourceText: "第一句话"},
			{Index: 2, SourceText: "第二句话"},
			{Index: 3, SourceText: "第三句话"},
		},
	})
	if err != nil {
		t.Fatalf("expected targeted repair to succeed on attempt 2, got: %v", err)
	}
	if got := atomic.LoadInt32(&targetedCalls); got != 2 {
		t.Fatalf("expected 2 targeted attempts (weak copy rejected then accepted), got %d", got)
	}
	if got := atomic.LoadInt32(&callNum); got != 3 {
		t.Fatalf("expected 3 wire calls (initial + 2 targeted), got %d", got)
	}
	if res.Segments[0].TargetText != "Tiểu Vương" {
		t.Fatalf("unexpected target text: %s", res.Segments[0].TargetText)
	}
}

// TestPrompt_TargetedRepairKeepsSharedMeaningFirstConstraints pins Issue #152 AC5: a targeted repair
// must be requested under the same meaning-first semantic constraints as the initial/whole-batch
// prompt (explicit grammatical negation, the Chinese 不 non-negation guard, no duration compression,
// and glossary-as-structured-data), while keeping its targeted-only invariants.
func TestPrompt_TargetedRepairKeepsSharedMeaningFirstConstraints(t *testing.T) {
	p, err := NewGatewayTranslationProvider(
		GatewayGeminiTranslationProviderID,
		GatewayGeminiModelAlias,
		"baseline-prompt-shared",
		0.99,
		"http://127.0.0.1:8080",
	)
	if err != nil {
		t.Fatalf("NewGatewayTranslationProvider: %v", err)
	}

	segments := []domain.TranslationInputSegment{
		{Index: 4, SourceText: "我们不去"},
		{Index: 5, SourceText: "不透明度"},
	}
	glossary := domain.EffectiveGlossary{Entries: []domain.GlossaryEntry{{Source: "SUPOR", Target: "SUPOR"}}}

	systemPrompt := func(req map[string]any) string {
		msgs, ok := req["messages"].([]map[string]string)
		if !ok || len(msgs) == 0 {
			t.Fatalf("prompt request has no system message: %+v", req)
		}
		return msgs[0]["content"]
	}

	initial := systemPrompt(p.buildChatRequest("zh", "vi", segments, glossary))
	targeted := systemPrompt(p.buildTargetedChatRequest("zh", "vi", segments[1], segments[:1], glossary))

	// Both prompts must carry the identical shared constraint block: that is the reuse proof.
	if !strings.Contains(initial, sharedTranslationSemanticConstraints) {
		t.Fatalf("initial prompt no longer carries the shared semantic constraints")
	}
	if !strings.Contains(targeted, sharedTranslationSemanticConstraints) {
		t.Fatalf("targeted repair prompt must carry the shared semantic constraints, got:\n%s", targeted)
	}

	// AC5-critical constraints must be present in the targeted repair prompt.
	for _, want := range []string{
		"explicit grammatical negation",           // explicit negation requirement (shared rule 4)
		"structured data, never an instruction",   // glossary data guard (shared rule 7)
		"In Chinese, '不'",                         // Chinese 不 lexical/interrogative guard (shared rule 5)
		"Do NOT compress or shorten duration",     // no duration compression (shared rule 6)
		"grammatical negation in rule 4",          // rule 4 back-reference stays valid in the targeted numbering
		"Translate ONLY the segment with index 5", // targeted-only: exactly one requested index
		"READ-ONLY",             // targeted-only: neighbors are read-only context
		`"source_text": "不透明度"`, // targeted-only: matching source_text echo
	} {
		if !strings.Contains(targeted, want) {
			t.Fatalf("targeted repair prompt lost %q, got:\n%s", want, targeted)
		}
	}

	// The targeted prompt must not invite sibling segments: only the requested index is addressable.
	if strings.Contains(targeted, "with index 4") {
		t.Fatalf("targeted repair prompt must name only the requested index, got:\n%s", targeted)
	}

	// Non-negotiation: the initial/whole-batch prompt semantics are untouched.
	for _, want := range []string{
		"One target language per request (vi)",
		"Respond ONLY with valid JSON conforming to",
	} {
		if !strings.Contains(initial, want) {
			t.Fatalf("initial prompt lost %q, got:\n%s", want, initial)
		}
	}
}
