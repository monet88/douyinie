package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestSnapshot_ValidateRelativePath(t *testing.T) {
	// 1. Valid relative paths
	validPaths := []string{
		"model.bin",
		"weights/model.safetensors",
		"config.json",
		"tokenizer/vocab.json",
		"deeply/nested/dir/asset.bin",
	}
	for _, p := range validPaths {
		norm, err := domain.ValidateRelativePath(p)
		if err != nil {
			t.Errorf("expected valid path %q to pass, got: %v", p, err)
		}
		if norm == "" || strings.Contains(norm, "\\") {
			t.Errorf("expected normalized forward slashes for %q, got %q", p, norm)
		}
	}

	// 2. Reject empty or whitespace
	for _, p := range []string{"", "   ", "\t\n"} {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected empty/whitespace %q to be rejected", p)
		}
	}

	// 3. Reject dot paths
	dotPaths := []string{".", "./", ".\\", "foo/.", "foo/./bar"}
	for _, p := range dotPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected dot path %q to be rejected", p)
		}
	}

	// 4. Reject parent traversal (..)
	traversalPaths := []string{
		"..",
		"../foo",
		"../../foo/bar",
		"foo/..",
		"foo/../bar",
		"foo/bar/../../baz",
		"a/b/../../../c",
	}
	for _, p := range traversalPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected parent traversal %q to be rejected", p)
		}
	}

	// 5. Reject absolute paths
	absPaths := []string{
		"/etc/passwd",
		"/model.bin",
		"\\Windows\\System32",
		"\\foo\\bar",
	}
	for _, p := range absPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected absolute path %q to be rejected", p)
		}
	}

	// 6. Reject volume-qualified paths
	volPaths := []string{
		"C:foo",
		"C:\\foo",
		"C:/foo",
		"D:weights.bin",
		"\\\\server\\share\\model.bin",
		"z:/model.bin",
	}
	for _, p := range volPaths {
		if _, err := domain.ValidateRelativePath(p); err == nil {
			t.Errorf("expected volume-qualified path %q to be rejected", p)
		}
	}
}

func TestSnapshot_ValidateSnapshotManifest_RejectDuplicatesAndInvalid(t *testing.T) {
	dummySHA := strings.Repeat("a", 64)

	// 1. Valid manifest passes
	validManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: 100},
			{RelativePath: "config.json", SHA256: dummySHA, SizeBytes: 200},
		},
	}
	if err := domain.ValidateSnapshotManifest(&validManifest); err != nil {
		t.Fatalf("expected valid manifest to pass, got: %v", err)
	}

	// 2. Duplicate normalized paths rejected
	dupManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: 100},
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&dupManifest); err == nil {
		t.Fatal("expected duplicate paths in manifest to be rejected")
	}

	// Duplicate with alternative slash/prefix
	dupManifest2 := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "subdir/weights.bin", SHA256: dummySHA, SizeBytes: 100},
			{RelativePath: "subdir\\weights.bin", SHA256: dummySHA, SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&dupManifest2); err == nil {
		t.Fatal("expected normalized duplicate paths to be rejected")
	}

	// 3. Traversal path in manifest rejected
	travManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "../secret.txt", SHA256: dummySHA, SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&travManifest); err == nil {
		t.Fatal("expected traversal in manifest to be rejected")
	}

	// 4. Invalid sha length rejected
	badSHAManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: "short_hash", SizeBytes: 100},
		},
	}
	if err := domain.ValidateSnapshotManifest(&badSHAManifest); err == nil {
		t.Fatal("expected short sha256 to be rejected")
	}

	// 5. Negative size rejected
	negSizeManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-model",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: dummySHA, SizeBytes: -5},
		},
	}
	if err := domain.ValidateSnapshotManifest(&negSizeManifest); err == nil {
		t.Fatal("expected negative size to be rejected")
	}
}

func TestSnapshot_ResolveTranslationGGUFEntrypoint(t *testing.T) {
	tmpDir := t.TempDir()

	// Helper to create dummy file in tmpDir
	createFile := func(relPath string, isDir bool) {
		full := filepath.Join(tmpDir, filepath.FromSlash(relPath))
		if isDir {
			_ = os.MkdirAll(full, 0755)
			return
		}
		_ = os.MkdirAll(filepath.Dir(full), 0755)
		_ = os.WriteFile(full, []byte("dummy gguf"), 0644)
	}

	createFile("qwen3-4b-q4_k_m.gguf", false)
	createFile("qwen3-4b-q8_0.gguf", false)
	createFile("other/model.gguf", false)
	createFile("qwen3-alt-q4_k_m.gguf", false)
	createFile("ambig1.gguf", false)
	createFile("ambig2.gguf", false)
	createFile("fake_dir.gguf", true) // is a directory

	// 1. Deterministic resolution of single Q4_K_M file
	m1 := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "qwen3-4b-q4_k_m.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
			{RelativePath: "config.json", SHA256: strings.Repeat("b", 64), SizeBytes: 5},
		},
	}
	ep1, err := domain.ResolveTranslationGGUFEntrypoint(m1, tmpDir, "Qwen3-4B-Q4_K_M")
	if err != nil {
		t.Fatalf("expected successful resolution, got: %v", err)
	}
	expected1 := filepath.Join(tmpDir, "qwen3-4b-q4_k_m.gguf")
	if ep1 != expected1 {
		t.Fatalf("expected entrypoint %q, got %q", expected1, ep1)
	}

	// 2. Disambiguation: Q4_K_M vs Q8_0 -> picks Q4_K_M deterministically
	m2 := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "qwen3-4b-q8_0.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 20},
			{RelativePath: "qwen3-4b-q4_k_m.gguf", SHA256: strings.Repeat("b", 64), SizeBytes: 10},
		},
	}
	ep2, err := domain.ResolveTranslationGGUFEntrypoint(m2, tmpDir, "Qwen3-4B-Q4_K_M")
	if err != nil {
		t.Fatalf("expected successful disambiguation, got: %v", err)
	}
	if ep2 != expected1 {
		t.Fatalf("expected entrypoint %q, got %q", expected1, ep2)
	}

	// 3. Single non-conflicting generic .gguf resolves successfully
	m3 := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "other/model.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
		},
	}
	ep3, err := domain.ResolveTranslationGGUFEntrypoint(m3, tmpDir, "Qwen3-4B-Q4_K_M")
	if err != nil {
		t.Fatalf("expected generic single gguf to resolve, got: %v", err)
	}
	expected3 := filepath.Join(tmpDir, "other", "model.gguf")
	if ep3 != expected3 {
		t.Fatalf("expected entrypoint %q, got %q", expected3, ep3)
	}

	// 4. Absent: No .gguf files declared
	mNoGGUF := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "config.json", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTranslationGGUFEntrypoint(mNoGGUF, tmpDir, "Qwen3-4B-Q4_K_M")
	if !errors.Is(err, domain.ErrSnapshotEntrypointAbsent) {
		t.Fatalf("expected ErrSnapshotEntrypointAbsent for no gguf files, got: %v", err)
	}

	// 5. Absent: Only conflicting quant files declared
	mConflictOnly := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "qwen3-4b-q8_0.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTranslationGGUFEntrypoint(mConflictOnly, tmpDir, "Qwen3-4B-Q4_K_M")
	if !errors.Is(err, domain.ErrSnapshotEntrypointAbsent) {
		t.Fatalf("expected ErrSnapshotEntrypointAbsent for conflicting quant only, got: %v", err)
	}

	// 6. Ambiguous: Multiple Q4_K_M files declared
	mMultiQ4KM := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "qwen3-4b-q4_k_m.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
			{RelativePath: "qwen3-alt-q4_k_m.gguf", SHA256: strings.Repeat("b", 64), SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTranslationGGUFEntrypoint(mMultiQ4KM, tmpDir, "Qwen3-4B-Q4_K_M")
	if !errors.Is(err, domain.ErrSnapshotEntrypointAmbiguous) {
		t.Fatalf("expected ErrSnapshotEntrypointAmbiguous for multiple Q4_K_M files, got: %v", err)
	}

	// 7. Ambiguous: Multiple generic gguf files without explicit Q4_K_M tag
	mMultiGeneric := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "ambig1.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
			{RelativePath: "ambig2.gguf", SHA256: strings.Repeat("b", 64), SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTranslationGGUFEntrypoint(mMultiGeneric, tmpDir, "Qwen3-4B-Q4_K_M")
	if !errors.Is(err, domain.ErrSnapshotEntrypointAmbiguous) {
		t.Fatalf("expected ErrSnapshotEntrypointAmbiguous for multiple generic gguf files, got: %v", err)
	}

	// 8. Corrupted: File missing on disk
	mMissingOnDisk := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "missing_on_disk_q4_k_m.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTranslationGGUFEntrypoint(mMissingOnDisk, tmpDir, "Qwen3-4B-Q4_K_M")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for missing file on disk, got: %v", err)
	}

	// 9. Corrupted: Entrypoint is a directory on disk
	mDirOnDisk := domain.SnapshotManifest{
		ModelID:      "qwen3_4b_translator",
		ModelVersion: "Qwen3-4B-Q4_K_M",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "fake_dir.gguf", SHA256: strings.Repeat("a", 64), SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTranslationGGUFEntrypoint(mDirOnDisk, tmpDir, "Qwen3-4B-Q4_K_M")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for directory on disk, got: %v", err)
	}
}
func TestSnapshot_ResolveTTSVoiceEntrypoint(t *testing.T) {
	tmpDir := t.TempDir()
	dummySHA := strings.Repeat("b", 64)

	// 1. Kokoro valid voice and checkpoint
	// 1. Kokoro valid voice, config, and checkpoint
	kokoroDir := filepath.Join(tmpDir, "kokoro")
	if err := os.MkdirAll(filepath.Join(kokoroDir, "voices"), 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(kokoroDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"model_type": "kokoro"}`), 0644); err != nil {
		t.Fatal(err)
	}
	ckptPath := filepath.Join(kokoroDir, "kokoro-v1_0.pth")
	if err := os.WriteFile(ckptPath, []byte("kokoro checkpoint"), 0644); err != nil {
		t.Fatal(err)
	}
	ckptSHA := "496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4"

	voicePath := filepath.Join(kokoroDir, "voices", "af_heart.pt")
	if err := os.WriteFile(voicePath, []byte("voice data"), 0644); err != nil {
		t.Fatal(err)
	}
	vH := sha256.Sum256([]byte("voice data"))
	voiceSHA := hex.EncodeToString(vH[:])

	mKokoroValid := domain.SnapshotManifest{
		ModelID:      "hexgrad/Kokoro-82M",
		ModelVersion: "v1.0",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "config.json", SHA256: dummySHA, SizeBytes: 24},
			{RelativePath: "kokoro-v1_0.pth", SHA256: ckptSHA, SizeBytes: 17},
			{RelativePath: "voices/af_heart.pt", SHA256: voiceSHA, SizeBytes: 10},
		},
	}
	res, err := domain.ResolveTTSVoiceEntrypoint(mKokoroValid, kokoroDir, "hexgrad/Kokoro-82M", "af_heart")
	if err != nil {
		t.Fatalf("expected valid Kokoro voice resolution, got: %v", err)
	}
	if res != voicePath {
		t.Fatalf("expected resolved path %s, got %s", voicePath, res)
	}
	// 2. Kokoro unverified voice rejected
	_, err = domain.ResolveTTSVoiceEntrypoint(mKokoroValid, kokoroDir, "hexgrad/Kokoro-82M", "unverified_voice")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for unverified voice, got: %v", err)
	}

	// 3. Kokoro voice missing in manifest rejected
	_, err = domain.ResolveTTSVoiceEntrypoint(mKokoroValid, kokoroDir, "hexgrad/Kokoro-82M", "am_michael")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for voice missing in manifest, got: %v", err)
	}

	// 4. Kokoro voice file missing on disk rejected
	mKokoroMissingDisk := domain.SnapshotManifest{
		ModelID:      "hexgrad/Kokoro-82M",
		ModelVersion: "v1.0",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "config.json", SHA256: dummySHA, SizeBytes: 24},
			{RelativePath: "kokoro-v1_0.pth", SHA256: ckptSHA, SizeBytes: 17},
			{RelativePath: "voices/am_michael.pt", SHA256: dummySHA, SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mKokoroMissingDisk, kokoroDir, "hexgrad/Kokoro-82M", "am_michael")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for voice missing on disk, got: %v", err)
	}

	// 5. Kokoro checkpoint SHA mismatch rejected
	mKokoroBadSHA := domain.SnapshotManifest{
		ModelID:      "hexgrad/Kokoro-82M",
		ModelVersion: "v1.0",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "config.json", SHA256: dummySHA, SizeBytes: 24},
			{RelativePath: "kokoro-v1_0.pth", SHA256: dummySHA, SizeBytes: 17},
			{RelativePath: "voices/af_heart.pt", SHA256: voiceSHA, SizeBytes: 10},
		},
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mKokoroBadSHA, kokoroDir, "hexgrad/Kokoro-82M", "af_heart")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for checkpoint sha mismatch, got: %v", err)
	}

	// 6. Fabricated VieNeu voices/*.pt without voices_v3_turbo.json rejected
	vieneuFabDir := filepath.Join(tmpDir, "vieneu_fab")
	if err := os.MkdirAll(filepath.Join(vieneuFabDir, "voices"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vieneuFabDir, "voices", "Trúc Ly.pt"), []byte("truc ly"), 0644); err != nil {
		t.Fatal(err)
	}
	mVieNeuFab := domain.SnapshotManifest{
		ModelID:      "pnnbao-ump/VieNeu-TTS-v3-Turbo",
		ModelVersion: "v3.8.1",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "voices/Trúc Ly.pt", SHA256: dummySHA, SizeBytes: 7},
		},
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuFab, vieneuFabDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for fabricated VieNeu snapshot without voices_v3_turbo.json, got: %v", err)
	}

	// 6b. VieNeu valid voice with authentic voices_v3_turbo.json and fixed in-root MOSS tokenizer
	// The lane's load-bearing digests are pinned, so the fixture declares the pinned digests for
	// every pinned asset (placeholder bytes: the resolver compares declared digests; registration
	// is what hashes the bytes against them).
	vieneuDir := filepath.Join(tmpDir, "vieneu")
	catDir := filepath.Join(vieneuDir, "src", "vieneu", "assets")
	if err := os.MkdirAll(catDir, 0755); err != nil {
		t.Fatal(err)
	}
	catRel := "src/vieneu/assets/voices_v3_turbo.json"
	catJSON := `{"presets": {"Trúc Ly": {"id": "Trúc Ly", "name": "Trúc Ly"}}}`
	catPath := filepath.Join(catDir, "voices_v3_turbo.json")
	if err := os.WriteFile(catPath, []byte(catJSON), 0644); err != nil {
		t.Fatal(err)
	}
	mossDir := filepath.Join(vieneuDir, "moss_tokenizer")
	if err := os.MkdirAll(mossDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mossDir, "tokenizer.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	vieneuFiles := declarePinnedAssets(t, vieneuDir, withoutPinnedRel(domain.PinnedVieNeuAssets, catRel))
	vieneuFiles = append([]domain.SnapshotFileEntry{
		{RelativePath: catRel, SHA256: pinnedSHA(t, domain.PinnedVieNeuAssets, catRel), SizeBytes: int64(len(catJSON))},
	}, vieneuFiles...)
	mVieNeuValid := domain.SnapshotManifest{
		ModelID:      "pnnbao-ump/VieNeu-TTS-v3-Turbo",
		ModelVersion: "v3.8.1",
		Files:        vieneuFiles,
	}
	resV, err := domain.ResolveTTSVoiceEntrypoint(mVieNeuValid, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if err != nil {
		t.Fatalf("expected valid VieNeu voice resolution, got: %v", err)
	}
	if resV != catPath {
		t.Fatalf("expected resolved catalog entrypoint path %s, got %s", catPath, resV)
	}

	// 6c. VieNeu snapshot that declares different bytes for a load-bearing weight under the pinned
	// revision is rejected: this is the weights-digest gate, not a revision-label check.
	mVieNeuSubstituted := mVieNeuValid
	mVieNeuSubstituted.Files = append([]domain.SnapshotFileEntry(nil), mVieNeuValid.Files...)
	for i := range mVieNeuSubstituted.Files {
		if mVieNeuSubstituted.Files[i].RelativePath == "update/model.safetensors" {
			mVieNeuSubstituted.Files[i].SHA256 = strings.Repeat("b", 64)
		}
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuSubstituted, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for substituted VieNeu weights, got: %v", err)
	}

	// 6d. A manifest that omits a load-bearing weight fails closed: an undeclared file was never
	// hashed by registration, so the pinned digest would be unchecked.
	mVieNeuUndeclared := mVieNeuValid
	mVieNeuUndeclared.Files = withoutFileRel(mVieNeuValid.Files, "update/model.safetensors")
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuUndeclared, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for undeclared VieNeu weights, got: %v", err)
	}

	// 7. VieNeu unverified voice rejected
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuValid, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "unverified_voice")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for unverified VieNeu voice, got: %v", err)
	}

	// 8. VieNeu voice missing in catalog rejected
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuValid, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Phạm Tuyên")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for VieNeu voice missing in catalog, got: %v", err)
	}

	// 9. Kokoro unmanifested config.json rejected
	mKokoroNoConfig := mKokoroValid
	mKokoroNoConfig.Files = mKokoroValid.Files[1:]
	_, err = domain.ResolveTTSVoiceEntrypoint(mKokoroNoConfig, kokoroDir, "hexgrad/Kokoro-82M", "af_heart")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for Kokoro unmanifested config.json, got: %v", err)
	}

	// 10. VieNeu unmanifested MOSS tokenizer rejected
	mVieNeuNoMoss := mVieNeuValid
	mVieNeuNoMoss.Files = mVieNeuValid.Files[:1]
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuNoMoss, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for VieNeu unmanifested MOSS tokenizer, got: %v", err)
	}

	// 11. VieNeu alternate catalog path rejected (only exact src/vieneu/assets/voices_v3_turbo.json accepted)
	mVieNeuAltCatalog := mVieNeuValid
	mVieNeuAltCatalog.Files = []domain.SnapshotFileEntry{
		{RelativePath: "assets/voices_v3_turbo.json", SHA256: dummySHA, SizeBytes: 100},
		{RelativePath: "moss_tokenizer/tokenizer.json", SHA256: dummySHA, SizeBytes: 2},
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuAltCatalog, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for alternate VieNeu catalog path assets/voices_v3_turbo.json, got: %v", err)
	}

	mVieNeuRootCatalog := mVieNeuValid
	mVieNeuRootCatalog.Files = []domain.SnapshotFileEntry{
		{RelativePath: "voices_v3_turbo.json", SHA256: dummySHA, SizeBytes: 100},
		{RelativePath: "moss_tokenizer/tokenizer.json", SHA256: dummySHA, SizeBytes: 2},
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mVieNeuRootCatalog, vieneuDir, "pnnbao-ump/VieNeu-TTS-v3-Turbo", "Trúc Ly")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for root VieNeu catalog path voices_v3_turbo.json, got: %v", err)
	}
}

// declarePinnedAssets writes a placeholder file for every pinned asset of a lane and returns
// manifest entries declaring the pinned digests. The resolver compares declared digests against the
// pinned constants and stats the file; registration is what hashes bytes against the declared
// digests, so a resolver fixture does not need byte-identical placeholders.
func declarePinnedAssets(t *testing.T, root string, assets []domain.PinnedAsset) []domain.SnapshotFileEntry {
	t.Helper()
	entries := make([]domain.SnapshotFileEntry, 0, len(assets))
	for _, asset := range assets {
		full := filepath.Join(root, filepath.FromSlash(asset.RelativePath))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		content := []byte("fixture:" + asset.RelativePath)
		if err := os.WriteFile(full, content, 0644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, domain.SnapshotFileEntry{
			RelativePath: asset.RelativePath,
			SHA256:       asset.SHA256,
			SizeBytes:    int64(len(content)),
		})
	}
	return entries
}

// pinnedSHA returns the pinned digest a lane declares for rel.
func pinnedSHA(t *testing.T, assets []domain.PinnedAsset, rel string) string {
	t.Helper()
	for _, asset := range assets {
		if asset.RelativePath == rel {
			return asset.SHA256
		}
	}
	t.Fatalf("no pinned asset %s", rel)
	return ""
}

// withoutPinnedRel returns assets minus the entry whose relative path equals rel.
func withoutPinnedRel(assets []domain.PinnedAsset, rel string) []domain.PinnedAsset {
	out := make([]domain.PinnedAsset, 0, len(assets))
	for _, asset := range assets {
		if asset.RelativePath != rel {
			out = append(out, asset)
		}
	}
	return out
}

// withoutFileRel returns manifest files minus the entry whose relative path equals rel.
func withoutFileRel(files []domain.SnapshotFileEntry, rel string) []domain.SnapshotFileEntry {
	out := make([]domain.SnapshotFileEntry, 0, len(files))
	for _, file := range files {
		if file.RelativePath != rel {
			out = append(out, file)
		}
	}
	return out
}

func TestSnapshot_ResolveTTSVoiceEntrypoint_ZeroTTSPinnedWeights(t *testing.T) {
	tmpDir := t.TempDir()
	zeroDir := filepath.Join(tmpDir, "zerotts")

	files := declarePinnedAssets(t, zeroDir, domain.PinnedZeroTTSAssets)
	files = append(files, declarePinnedAssets(t, zeroDir, domain.PinnedZeroTTSVoiceAssets)...)
	indexRel := "voices/index.json"
	indexPath := filepath.Join(zeroDir, "voices", "index.json")
	if err := os.WriteFile(indexPath, []byte(`{"voices": []}`), 0644); err != nil {
		t.Fatal(err)
	}
	files = append(files, domain.SnapshotFileEntry{
		RelativePath: indexRel, SHA256: strings.Repeat("a", 64), SizeBytes: 14,
	})
	mZero := domain.SnapshotManifest{
		ModelID:      "zeroweight-ai/ZeroTTS",
		ModelVersion: "c2bfbd67dc648cac455077333f7cf5c18a2e3bb4",
		Files:        files,
	}

	// 1. A snapshot declaring every pinned digest resolves the requested voice asset.
	ep, err := domain.ResolveTTSVoiceEntrypoint(mZero, zeroDir, "zeroweight-ai/ZeroTTS", "maichi")
	if err != nil {
		t.Fatalf("expected valid ZeroTTS voice resolution, got: %v", err)
	}
	expectedVoice := filepath.Join(zeroDir, "voices", "maichi", "voice.npz")
	if ep != expectedVoice {
		t.Fatalf("expected voice entrypoint %s, got %s", expectedVoice, ep)
	}

	// 2. Substituted graph bytes under the pinned revision are rejected (the gate is the digest,
	// not the revision label).
	mZeroSubstituted := mZero
	mZeroSubstituted.Files = append([]domain.SnapshotFileEntry(nil), mZero.Files...)
	for i := range mZeroSubstituted.Files {
		if mZeroSubstituted.Files[i].RelativePath == "onnx/text_encoder.onnx" {
			mZeroSubstituted.Files[i].SHA256 = strings.Repeat("b", 64)
		}
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mZeroSubstituted, zeroDir, "zeroweight-ai/ZeroTTS", "maichi")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for substituted ZeroTTS text encoder, got: %v", err)
	}

	// 3. A manifest that omits a load-bearing weight fails closed instead of skipping the check.
	mZeroUndeclared := mZero
	mZeroUndeclared.Files = withoutFileRel(mZero.Files, domain.PinnedZeroTTSAssets[0].RelativePath)
	_, err = domain.ResolveTTSVoiceEntrypoint(mZeroUndeclared, zeroDir, "zeroweight-ai/ZeroTTS", "maichi")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for undeclared ZeroTTS weight, got: %v", err)
	}

	// 4. Substituted voice conditioning tensors are rejected for the requested voice.
	mZeroVoiceSwapped := mZero
	mZeroVoiceSwapped.Files = append([]domain.SnapshotFileEntry(nil), mZero.Files...)
	for i := range mZeroVoiceSwapped.Files {
		if mZeroVoiceSwapped.Files[i].RelativePath == "voices/maichi/voice.npz" {
			mZeroVoiceSwapped.Files[i].SHA256 = strings.Repeat("c", 64)
		}
	}
	_, err = domain.ResolveTTSVoiceEntrypoint(mZeroVoiceSwapped, zeroDir, "zeroweight-ai/ZeroTTS", "maichi")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for substituted ZeroTTS voice asset, got: %v", err)
	}

	// 5. Unknown voice rejected before any asset check.
	_, err = domain.ResolveTTSVoiceEntrypoint(mZero, zeroDir, "zeroweight-ai/ZeroTTS", "not_a_preset")
	if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
		t.Fatalf("expected ErrTTSVoiceAssetMissing for unknown ZeroTTS voice, got: %v", err)
	}
}

func TestSnapshot_ResolveSeparatorEntrypoint(t *testing.T) {
	tmpDir := t.TempDir()
	dummySHA := strings.Repeat("a", 64)

	uvrDir := filepath.Join(tmpDir, "uvr")
	if err := os.MkdirAll(uvrDir, 0755); err != nil {
		t.Fatal(err)
	}
	uvrFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if err := os.WriteFile(uvrFile, []byte("onnx_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	mUVRValid := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "UVR-MDX-NET-Inst_HQ_4.onnx",
		ModelVersion:  "v3",
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx",
				SHA256:       domain.PinnedUVRArtifactSHA256,
				SizeBytes:    10,
			},
		},
	}

	// 1. Valid UVR resolves entrypoint
	ep, err := domain.ResolveSeparatorEntrypoint(mUVRValid, uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if err != nil {
		t.Fatalf("expected valid UVR entrypoint to resolve, got error: %v", err)
	}
	if ep != uvrFile {
		t.Fatalf("expected resolved entrypoint %s, got %s", uvrFile, ep)
	}

	// Also resolves with modelName containing "uvr"
	ep2, err := domain.ResolveSeparatorEntrypoint(mUVRValid, uvrDir, "uvr_separator")
	if err != nil || ep2 != uvrFile {
		t.Fatalf("expected uvr_separator to resolve %s, got %s (err: %v)", uvrFile, ep2, err)
	}

	// 2. UVR artifact not declared
	mUVRNoFile := mUVRValid
	mUVRNoFile.Files = []domain.SnapshotFileEntry{
		{RelativePath: "other.onnx", SHA256: domain.PinnedUVRArtifactSHA256, SizeBytes: 10},
	}
	_, err = domain.ResolveSeparatorEntrypoint(mUVRNoFile, uvrDir, "uvr_separator")
	if !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing when UVR onnx not declared, got: %v", err)
	}

	// 3. UVR artifact SHA mismatch
	mUVRBadSHA := mUVRValid
	mUVRBadSHA.Files = []domain.SnapshotFileEntry{
		{RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx", SHA256: dummySHA, SizeBytes: 10},
	}
	_, err = domain.ResolveSeparatorEntrypoint(mUVRBadSHA, uvrDir, "uvr_separator")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for bad UVR SHA, got: %v", err)
	}

	// 4. UVR artifact missing on disk
	mUVRMissingDisk := mUVRValid
	mUVRMissingDisk.Files = []domain.SnapshotFileEntry{
		{RelativePath: "missing_UVR-MDX-NET-Inst_HQ_4.onnx", SHA256: domain.PinnedUVRArtifactSHA256, SizeBytes: 10},
	}
	// Change relative path to end with UVR-MDX-NET-Inst_HQ_4.onnx
	mUVRMissingDisk.Files[0].RelativePath = "sub/UVR-MDX-NET-Inst_HQ_4.onnx"
	_, err = domain.ResolveSeparatorEntrypoint(mUVRMissingDisk, uvrDir, "uvr_separator")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for UVR missing on disk, got: %v", err)
	}

	// Demucs setup
	demucsDir := filepath.Join(tmpDir, "demucs")
	if err := os.MkdirAll(demucsDir, 0755); err != nil {
		t.Fatal(err)
	}
	demucsFile := filepath.Join(demucsDir, "955717e8-8726e21a.th")
	if err := os.WriteFile(demucsFile, []byte("demucs_bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	mDemucsValid := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "htdemucs",
		ModelVersion:  "v4",
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "955717e8-8726e21a.th",
				SHA256:       domain.PinnedDemucsCheckpointSHA,
				SizeBytes:    12,
			},
		},
	}

	// 5. Valid Demucs resolves entrypoint
	epD, err := domain.ResolveSeparatorEntrypoint(mDemucsValid, demucsDir, "htdemucs")
	if err != nil {
		t.Fatalf("expected valid Demucs entrypoint to resolve, got error: %v", err)
	}
	if epD != demucsFile {
		t.Fatalf("expected resolved entrypoint %s, got %s", demucsFile, epD)
	}

	// 6. Demucs rejects htdemucs_ft in modelName
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsValid, demucsDir, "htdemucs_ft")
	if !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing when htdemucs_ft requested, got: %v", err)
	}

	// 7. Demucs rejects htdemucs_ft in manifest ModelID
	mDemucsFT := mDemucsValid
	mDemucsFT.ModelID = "htdemucs_ft"
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsFT, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing when manifest ModelID is htdemucs_ft, got: %v", err)
	}

	// 8. Demucs rejects four-checkpoint htdemucs_ft bag files in manifest
	mDemucsBag := mDemucsValid
	mDemucsBag.Files = append(mDemucsBag.Files, domain.SnapshotFileEntry{
		RelativePath: "f7e0c4bc-ba3fe64a.th",
		SHA256:       dummySHA,
		SizeBytes:    10,
	})
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsBag, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing when 4-checkpoint bag files declared, got: %v", err)
	}

	// 9. Demucs checkpoint not declared
	mDemucsNoFile := mDemucsValid
	mDemucsNoFile.Files = []domain.SnapshotFileEntry{
		{RelativePath: "other.th", SHA256: domain.PinnedDemucsCheckpointSHA, SizeBytes: 10},
	}
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsNoFile, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing when demucs checkpoint not declared, got: %v", err)
	}

	// 10. Demucs checkpoint SHA mismatch
	mDemucsBadSHA := mDemucsValid
	mDemucsBadSHA.Files = []domain.SnapshotFileEntry{
		{RelativePath: "955717e8-8726e21a.th", SHA256: dummySHA, SizeBytes: 10},
	}
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsBadSHA, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for bad Demucs SHA, got: %v", err)
	}

	// 11. Demucs checkpoint missing on disk
	mDemucsMissingDisk := mDemucsValid
	mDemucsMissingDisk.Files = []domain.SnapshotFileEntry{
		{RelativePath: "sub/955717e8-8726e21a.th", SHA256: domain.PinnedDemucsCheckpointSHA, SizeBytes: 10},
	}
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsMissingDisk, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for Demucs missing on disk, got: %v", err)
	}
	// 12. Valid complete Demucs local repo with canonical htdemucs.yaml bag definition
	yamlFile := filepath.Join(demucsDir, "htdemucs.yaml")
	if err := os.WriteFile(yamlFile, []byte("models: ['955717e8']\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mDemucsComplete := mDemucsValid
	mDemucsComplete.Files = append(mDemucsComplete.Files, domain.SnapshotFileEntry{
		RelativePath: "htdemucs.yaml",
		SHA256:       domain.PinnedDemucsBagYAMLSHA256,
		SizeBytes:    21,
	})
	epComplete, err := domain.ResolveSeparatorEntrypoint(mDemucsComplete, demucsDir, "htdemucs")
	if err != nil {
		t.Fatalf("expected valid complete Demucs repo to resolve, got error: %v", err)
	}
	if epComplete != demucsFile {
		t.Fatalf("expected resolved entrypoint %s, got %s", demucsFile, epComplete)
	}

	// 13. Demucs bag YAML SHA mismatch rejected
	mDemucsBadYAML := mDemucsValid
	mDemucsBadYAML.Files = append(mDemucsBadYAML.Files, domain.SnapshotFileEntry{
		RelativePath: "htdemucs.yaml",
		SHA256:       dummySHA,
		SizeBytes:    21,
	})
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsBadYAML, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for bad htdemucs.yaml SHA, got: %v", err)
	}

	// 14. Demucs bag YAML declared but missing on disk rejected
	mDemucsMissingYAML := mDemucsValid
	mDemucsMissingYAML.Files = append(mDemucsMissingYAML.Files, domain.SnapshotFileEntry{
		RelativePath: "missing_sub/htdemucs.yaml",
		SHA256:       domain.PinnedDemucsBagYAMLSHA256,
		SizeBytes:    21,
	})
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsMissingYAML, demucsDir, "htdemucs")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for missing htdemucs.yaml on disk, got: %v", err)
	}

	// 15. Unknown model rejected
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsValid, demucsDir, "spleeter")
	if !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing for unknown model spleeter, got: %v", err)
	}

	// 13. Empty snapshot root rejected
	_, err = domain.ResolveSeparatorEntrypoint(mDemucsValid, "   ", "htdemucs")
	if !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted for empty snapshot root, got: %v", err)
	}
}

func TestSnapshot_ResolveSeparatorMetadataPath(t *testing.T) {
	tmpDir := t.TempDir()
	uvrDir := filepath.Join(tmpDir, "uvr_meta")
	if err := os.MkdirAll(uvrDir, 0755); err != nil {
		t.Fatal(err)
	}
	metaFile := filepath.Join(uvrDir, "mdx_model_data.json")
	if err := os.WriteFile(metaFile, []byte(`{"0ddfc0eb5792638ad5dc27850236c246": {}}`), 0644); err != nil {
		t.Fatal(err)
	}

	mValid := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "UVR-MDX-NET-Inst_HQ_4.onnx",
		ModelVersion:  "v3",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "mdx_model_data.json", SHA256: strings.Repeat("b", 64), SizeBytes: 40},
		},
	}

	// 1. Valid metadata resolves
	resolved, err := domain.ResolveSeparatorMetadataPath(mValid, uvrDir)
	if err != nil {
		t.Fatalf("expected valid metadata to resolve, got %v", err)
	}
	if resolved != metaFile {
		t.Fatalf("expected %s, got %s", metaFile, resolved)
	}

	// 2. Missing from manifest fails closed
	mMissing := mValid
	mMissing.Files = []domain.SnapshotFileEntry{
		{RelativePath: "other.json", SHA256: strings.Repeat("b", 64), SizeBytes: 40},
	}
	if _, err := domain.ResolveSeparatorMetadataPath(mMissing, uvrDir); !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected ErrSeparatorModelAssetMissing when metadata not declared, got %v", err)
	}

	// 3. Missing on disk fails closed
	mMissingDisk := mValid
	mMissingDisk.Files = []domain.SnapshotFileEntry{
		{RelativePath: "sub/mdx_model_data.json", SHA256: strings.Repeat("b", 64), SizeBytes: 40},
	}
	if _, err := domain.ResolveSeparatorMetadataPath(mMissingDisk, uvrDir); !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted when metadata missing on disk, got %v", err)
	}
}

func TestSnapshot_SeparatorRuntimeIdentityEvidence(t *testing.T) {
	snapSHA := strings.Repeat("c", 64)
	uvrRT := domain.NewUVRRuntimeIdentity(snapSHA, nil)
	if uvrRT.SourceRevision != domain.PinnedUVRSourceRevision {
		t.Fatalf("expected UVR source revision %s, got %s", domain.PinnedUVRSourceRevision, uvrRT.SourceRevision)
	}
	if uvrRT.RuntimeVersions["audio-separator"] != domain.PinnedUVRPackageVersion {
		t.Fatalf("expected audio-separator version %s, got %s", domain.PinnedUVRPackageVersion, uvrRT.RuntimeVersions["audio-separator"])
	}
	if uvrRT.RuntimeManifestSHA256 == "" {
		t.Fatal("expected non-empty RuntimeManifestSHA256 for UVR")
	}

	demucsRT := domain.NewDemucsRuntimeIdentity(snapSHA, nil)
	if demucsRT.SourceRevision != domain.PinnedDemucsSourceRevision {
		t.Fatalf("expected Demucs source revision %s, got %s", domain.PinnedDemucsSourceRevision, demucsRT.SourceRevision)
	}
	if demucsRT.RuntimeVersions["demucs"] != domain.PinnedDemucsPackageVersion {
		t.Fatalf("expected demucs version %s, got %s", domain.PinnedDemucsPackageVersion, demucsRT.RuntimeVersions["demucs"])
	}
	if demucsRT.RuntimeManifestSHA256 == "" {
		t.Fatal("expected non-empty RuntimeManifestSHA256 for Demucs")
	}
}
