package benchmark_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
)

// helper to generate 100 valid acquisition corpus entries
func makeValid100Entries() []benchmark.AcquisitionCorpusEntry {
	entries := make([]benchmark.AcquisitionCorpusEntry, 100)
	baseTime := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		awemeID := fmt.Sprintf("7400000000000000%02d", i)
		entries[i] = benchmark.AcquisitionCorpusEntry{
			EntryID:             awemeID,
			AwemeID:             awemeID,
			CanonicalURL:        fmt.Sprintf("https://www.douyin.com/video/%s", awemeID),
			ExpectedDurationMs:  15000 + int64(i*100),
			DurationToleranceMs: 500,
			RequiresAudio:       true,
			Oracle: benchmark.OracleValidation{
				Method:       "oracle_curl_probe",
				ValidatedAt:  baseTime,
				EvidenceHash: fmt.Sprintf("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
				Notes:        "verified active Douyin public video",
			},
			BaselineMediaSHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		}
	}
	return entries
}

func TestAcquisitionCorpus_Validation_RejectsInvalidCount(t *testing.T) {
	entries := makeValid100Entries()

	// 99 entries (under-quota)
	under := entries[:99]
	_, err := benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), under)
	if err == nil {
		t.Fatal("expected error for 99 entries, got nil")
	}
	if !strings.Contains(err.Error(), "exactly 100") {
		t.Errorf("expected error message mentioning exactly 100, got %v", err)
	}

	// 101 entries (over-quota)
	over := append(entries, entries[0])
	over[100].AwemeID = "7400000000000000999"
	over[100].EntryID = over[100].AwemeID
	over[100].CanonicalURL = "https://www.douyin.com/video/7400000000000000999"
	_, err = benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), over)
	if err == nil {
		t.Fatal("expected error for 101 entries, got nil")
	}
}

func TestAcquisitionCorpus_Validation_RejectsDuplicates(t *testing.T) {
	entries := makeValid100Entries()
	// Duplicate AwemeID
	entries[1].AwemeID = entries[0].AwemeID
	entries[1].EntryID = "unique_entry_1"
	entries[1].CanonicalURL = entries[0].CanonicalURL

	_, err := benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), entries)
	if err == nil || !strings.Contains(err.Error(), "duplicate aweme_id") {
		t.Fatalf("expected duplicate aweme_id error, got %v", err)
	}

	// Mismatched / Duplicate CanonicalURL
	entries = makeValid100Entries()
	entries[1].CanonicalURL = entries[0].CanonicalURL
	_, err = benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), entries)
	if err == nil {
		t.Fatal("expected error for duplicate/mismatched canonical_url, got nil")
	}
}

func TestAcquisitionCorpus_Validation_RejectsMalformedEntries(t *testing.T) {
	// Non-numeric aweme_id
	entries := makeValid100Entries()
	entries[0].AwemeID = "not-numeric-aweme"
	_, err := benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), entries)
	if err == nil || !strings.Contains(err.Error(), "aweme_id must be numeric") {
		t.Fatalf("expected non-numeric aweme_id error, got %v", err)
	}

	// Finding 5: Tight canonical URL validation rejections
	lookalikeCases := []struct {
		name string
		url  string
	}{
		{"non-douyin host", "https://www.tiktok.com/video/740000000000000000"},
		{"lookalike host evil-douyin", "https://evil-douyin.com/video/740000000000000000"},
		{"lookalike host attacker suffix", "https://www.douyin.com.attacker.com/video/740000000000000000"},
		{"http insecure scheme", "http://www.douyin.com/video/740000000000000000"},
		{"wrong path note", "https://www.douyin.com/note/740000000000000000"},
		{"mismatched aweme id in path", "https://www.douyin.com/video/740000000000000099"},
		{"with query params", "https://www.douyin.com/video/740000000000000000?share=1"},
		{"with fragment", "https://www.douyin.com/video/740000000000000000#anchor"},
	}
	for _, tc := range lookalikeCases {
		t.Run(tc.name, func(t *testing.T) {
			testEntries := makeValid100Entries()
			testEntries[0].CanonicalURL = tc.url
			_, err := benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), testEntries)
			if err == nil {
				t.Fatalf("%s: expected validation error for url %q, got nil", tc.name, tc.url)
			}
		})
	}

	// Zero duration
	entries = makeValid100Entries()
	entries[0].ExpectedDurationMs = 0
	_, err = benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), entries)
	if err == nil || !strings.Contains(err.Error(), "expected_duration_ms must be positive") {
		t.Fatalf("expected positive duration error, got %v", err)
	}

	// Incomplete oracle (missing evidence hash)
	entries = makeValid100Entries()
	entries[0].Oracle.EvidenceHash = "invalid-hash"
	_, err = benchmark.FreezeAcquisitionCorpus("test_acq", "1.1", time.Now().UTC(), entries)
	if err == nil || !strings.Contains(err.Error(), "oracle evidence_hash") {
		t.Fatalf("expected oracle evidence_hash error, got %v", err)
	}
}

func TestAcquisitionCorpus_FreezeAndManifestDigest_Deterministic(t *testing.T) {
	entries := makeValid100Entries()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	m1, err := benchmark.FreezeAcquisitionCorpus("phase1_100", "1.1", frozenAt, entries)
	if err != nil {
		t.Fatalf("freeze m1: %v", err)
	}
	m2, err := benchmark.FreezeAcquisitionCorpus("phase1_100", "1.1", frozenAt, entries)
	if err != nil {
		t.Fatalf("freeze m2: %v", err)
	}

	if m1.ManifestDigest != m2.ManifestDigest {
		t.Fatalf("manifest digests must be byte-for-byte identical, got %s vs %s", m1.ManifestDigest, m2.ManifestDigest)
	}
	if err := m1.VerifyManifestDigest(); err != nil {
		t.Fatalf("verify m1: %v", err)
	}

	// Tamper detection: change an entry's duration
	m1.Entries[0].ExpectedDurationMs += 1000
	if err := m1.VerifyManifestDigest(); err == nil {
		t.Fatal("expected manifest digest mismatch on tampered entry, got nil")
	}
}

func TestAcquisitionCorpus_Scoring_EnforcesInvariants(t *testing.T) {
	entries := makeValid100Entries()
	targetEntry := entries[0]

	// Case 1: Prohibit local-file substitution in scored benchmark
	ev := &benchmark.AcquisitionEntryEvidence{
		EntryID:              targetEntry.EntryID,
		CanonicalURL:         targetEntry.CanonicalURL,
		ExpectedAwemeID:      targetEntry.AwemeID,
		ObservedAwemeID:      targetEntry.AwemeID,
		SourceAssetID:        "asset-123",
		SourceAssetCASHash:   targetEntry.BaselineMediaSHA256,
		IntegrityPassed:      true,
		AudioIntegrityPassed: true,
		ObservedDurationMs:   targetEntry.ExpectedDurationMs,
		IsLocalSubstitution:  true, // Prohibited!
	}
	status, passed, reason := benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "local-file substitution is prohibited") {
		t.Fatalf("local file substitution must fail scored benchmark, got %s (%s)", status, reason)
	}

	// Case 2: HTTP 403 or 429 is scored as FAIL, never CONTENT_UNAVAILABLE
	ev.IsLocalSubstitution = false
	ev.HTTPStatusCode = 403
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "HTTP 403") {
		t.Fatalf("HTTP 403 must be scored as FAIL, got %s (%s)", status, reason)
	}

	ev.HTTPStatusCode = 429
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "HTTP 429") {
		t.Fatalf("HTTP 429 must be scored as FAIL, got %s (%s)", status, reason)
	}

	// Case 3: Missing committed SourceAsset
	ev.HTTPStatusCode = 200
	ev.SourceAssetID = ""
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "missing committed SourceAssetID") {
		t.Fatalf("missing source asset must fail, got %s (%s)", status, reason)
	}

	// Case 4: AwemeID mismatch
	ev.SourceAssetID = "asset-123"
	ev.SourceAssetCASHash = targetEntry.BaselineMediaSHA256
	ev.ObservedAwemeID = "7400000000000000999"
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "aweme_id mismatch") {
		t.Fatalf("aweme_id mismatch must fail, got %s (%s)", status, reason)
	}

	// Finding 4: Invalid CAS hash format
	ev.ObservedAwemeID = targetEntry.AwemeID
	ev.SourceAssetCASHash = "not-64-hex"
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "invalid observed SourceAssetCASHash") {
		t.Fatalf("invalid CAS hash format must fail, got %s (%s)", status, reason)
	}

	// Finding 4: BaselineMediaSHA256 mismatch
	ev.SourceAssetCASHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "CAS hash mismatch") {
		t.Fatalf("baseline CAS hash mismatch must fail, got %s (%s)", status, reason)
	}

	// Finding 5: Canonical URL mismatch in evidence
	ev.SourceAssetCASHash = targetEntry.BaselineMediaSHA256
	ev.CanonicalURL = "https://www.douyin.com/video/740000000000000099"
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "canonical_url mismatch") {
		t.Fatalf("canonical_url mismatch must fail, got %s (%s)", status, reason)
	}
	ev.CanonicalURL = targetEntry.CanonicalURL

	// Finding 6: Anti-bot status without a real HTTP code cannot PASS
	antiBotEv := &benchmark.AcquisitionEntryEvidence{
		EntryID:            targetEntry.EntryID,
		CanonicalURL:       targetEntry.CanonicalURL,
		ExpectedAwemeID:    targetEntry.AwemeID,
		ObservedAwemeID:    targetEntry.AwemeID,
		SourceAssetID:      "asset-123",
		SourceAssetCASHash: targetEntry.BaselineMediaSHA256,
		IntegrityPassed:    true,
		HTTPStatusCode:     0, // No HTTP status code!
		Status:             "ANTI_BOT",
		ErrorMessage:       "anti-bot challenge detected",
	}
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, antiBotEv)
	if passed || status != "FAIL" {
		t.Fatalf("anti-bot without HTTP code must fail, got %s (%s)", status, reason)
	}

	// Finding 2 & 6: Missing preflight integrity failure cannot PASS
	missingPreflightEv := &benchmark.AcquisitionEntryEvidence{
		EntryID:            targetEntry.EntryID,
		CanonicalURL:       targetEntry.CanonicalURL,
		ExpectedAwemeID:    targetEntry.AwemeID,
		ObservedAwemeID:    targetEntry.AwemeID,
		SourceAssetID:      "asset-123",
		SourceAssetCASHash: targetEntry.BaselineMediaSHA256,
		IntegrityPassed:    false,
		Status:             "FAIL",
		ErrorMessage:       "preflight report missing from acquisition result",
	}
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, missingPreflightEv)
	if passed || status != "FAIL" {
		t.Fatalf("missing preflight must fail, got %s (%s)", status, reason)
	}

	// Case 5: Container/video integrity failure
	ev.ObservedAwemeID = targetEntry.AwemeID
	ev.IntegrityPassed = false
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "integrity validation failed") {
		t.Fatalf("integrity failure must fail, got %s (%s)", status, reason)
	}

	// Case 6: Missing required audio
	ev.IntegrityPassed = true
	ev.AudioIntegrityPassed = false
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "audio track missing") {
		t.Fatalf("missing required audio must fail, got %s (%s)", status, reason)
	}

	// Case 7: Duration delta > tolerance
	ev.AudioIntegrityPassed = true
	ev.ObservedDurationMs = targetEntry.ExpectedDurationMs + targetEntry.DurationToleranceMs + 100
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if passed || status != "FAIL" || !strings.Contains(reason, "duration tolerance exceeded") {
		t.Fatalf("duration tolerance exceeded must fail, got %s (%s)", status, reason)
	}

	// Case 8: All criteria met -> PASS
	ev.ObservedDurationMs = targetEntry.ExpectedDurationMs + 50 // Within tolerance (500ms)
	status, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if !passed || status != "PASS" {
		t.Fatalf("valid entry must pass, got status=%s passed=%v reason=%s", status, passed, reason)
	}
}

func TestAcquisitionCorpus_FixedDenominator_NeverShrinks(t *testing.T) {
	entries := makeValid100Entries()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeAcquisitionCorpus("phase1_100", "1.1", frozenAt, entries)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// Simulate 98 passing entries and 2 failed entries
	evidences := make(map[string]*benchmark.AcquisitionEntryEvidence, 100)
	for i := 0; i < 98; i++ {
		e := entries[i]
		evidences[e.EntryID] = &benchmark.AcquisitionEntryEvidence{
			EntryID:              e.EntryID,
			CanonicalURL:         e.CanonicalURL,
			ExpectedAwemeID:      e.AwemeID,
			ObservedAwemeID:      e.AwemeID,
			SourceAssetID:        fmt.Sprintf("asset-%d", i),
			SourceAssetCASHash:   e.BaselineMediaSHA256,
			IntegrityPassed:      true,
			AudioIntegrityPassed: true,
			ObservedDurationMs:   e.ExpectedDurationMs,
			Status:               "PASS",
		}
	}
	// 2 failed entries
	for i := 98; i < 100; i++ {
		e := entries[i]
		evidences[e.EntryID] = &benchmark.AcquisitionEntryEvidence{
			EntryID:         e.EntryID,
			CanonicalURL:    e.CanonicalURL,
			ExpectedAwemeID: e.AwemeID,
			HTTPStatusCode:  403,
			Status:          "FAIL",
			ErrorMessage:    "WAF challenge",
		}
	}

	summary, err := benchmark.ComputeAcquisitionSummary("session-1", manifest, evidences, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute summary: %v", err)
	}

	if summary.TotalEntries != 100 {
		t.Fatalf("denominator must be fixed at 100, got %d", summary.TotalEntries)
	}
	if summary.PassedEntries != 98 {
		t.Fatalf("expected 98 passed entries, got %d", summary.PassedEntries)
	}
	if summary.FailedEntries != 2 {
		t.Fatalf("expected 2 failed entries, got %d", summary.FailedEntries)
	}
	if summary.SuccessRate != 0.98 {
		t.Fatalf("expected success rate 0.98, got %f", summary.SuccessRate)
	}
	if !summary.GateSatisfied {
		t.Fatal("gate must be satisfied for 98/100 passes")
	}

	// Now simulate 97 passes (fails the >= 98 gate)
	evidences[entries[97].EntryID].Status = "FAIL"
	evidences[entries[97].EntryID].HTTPStatusCode = 502
	summary2, err := benchmark.ComputeAcquisitionSummary("session-1", manifest, evidences, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute summary 2: %v", err)
	}
	if summary2.PassedEntries != 97 {
		t.Fatalf("expected 97 passed entries, got %d", summary2.PassedEntries)
	}
	if summary2.GateSatisfied {
		t.Fatal("gate must not be satisfied for 97/100 passes")
	}
}

func TestAcquisitionCorpus_PostFreezeDisappearance_InvalidatesCorpus(t *testing.T) {
	entries := makeValid100Entries()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeAcquisitionCorpus("phase1_100", "1.1", frozenAt, entries)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}

	sessInput := benchmark.SessionIdentityInput{
		ExecutionProfile: "local",
		BuildIdentity:    "git-test",
		ConfigSnapshot:   map[string]any{"test": true},
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition", Digest: manifest.ManifestDigest},
		},
	}
	sess, err := benchmark.NewSession(sessInput)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := sess.RecordAcquisitionManifest(manifest); err != nil {
		t.Fatalf("record manifest: %v", err)
	}

	// Case 1: Reject fake disappearance proof with 403 (security challenge != disappearance)
	badProof := benchmark.OracleDisappearanceProof{
		EntryID:       entries[0].EntryID,
		AwemeID:       entries[0].AwemeID,
		CanonicalURL:  entries[0].CanonicalURL,
		HTTPStatus:    403, // Invalid!
		OracleMethod:  "independent_browser_audit",
		EvidenceHash:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		VerifiedAt:    time.Now().UTC(),
		ConfirmedDead: true,
	}
	if err := sess.InvalidateCorpus(entries[0].EntryID, badProof); err == nil {
		t.Fatal("expected error for HTTP 403 disappearance proof, got nil")
	}

	// Case 2: Valid oracle disappearance proof with HTTP 404
	validProof := benchmark.OracleDisappearanceProof{
		EntryID:       entries[0].EntryID,
		AwemeID:       entries[0].AwemeID,
		CanonicalURL:  entries[0].CanonicalURL,
		HTTPStatus:    404,
		OracleMethod:  "independent_browser_audit",
		EvidenceHash:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		VerifiedAt:    time.Now().UTC(),
		ConfirmedDead: true,
	}

	if err := sess.InvalidateCorpus(entries[0].EntryID, validProof); err != nil {
		t.Fatalf("invalidate corpus: %v", err)
	}

	if sess.Status != benchmark.SessionStatusCorpusInvalidated {
		t.Fatalf("expected session status %s, got %s", benchmark.SessionStatusCorpusInvalidated, sess.Status)
	}

	// Post-invalidation: Recording further manifest or summary is blocked fail-closed
	if err := sess.RecordAcquisitionManifest(manifest); err == nil {
		t.Fatal("expected error recording manifest on CORPUS_INVALIDATED session, got nil")
	}

	summary := &benchmark.AcquisitionBenchmarkSummary{
		SessionID:      sess.ID,
		ManifestDigest: manifest.ManifestDigest,
		TotalEntries:   100,
		SummaryDigest:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := sess.RecordAcquisitionSummary(summary); err == nil {
		t.Fatal("expected error recording summary on CORPUS_INVALIDATED session, got nil")
	}
}

func TestAcquisitionCorpus_SessionBinding_RejectsDigestMismatch(t *testing.T) {
	entries := makeValid100Entries()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeAcquisitionCorpus("phase1_100", "1.1", frozenAt, entries)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// Session bound to a different digest
	sessInput := benchmark.SessionIdentityInput{
		ExecutionProfile: "local",
		BuildIdentity:    "git-test",
		ConfigSnapshot:   map[string]any{"test": true},
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition", Digest: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"},
		},
	}
	sess, err := benchmark.NewSession(sessInput)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	err = sess.RecordAcquisitionManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "does not match any corpus digest in session identity") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}
