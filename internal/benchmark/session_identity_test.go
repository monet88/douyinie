package benchmark_test

import (
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
)

func sampleIdentityInput() benchmark.SessionIdentityInput {
	return benchmark.SessionIdentityInput{
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition_100_urls", Digest: "62ce8541701236f69fb31f39ee724473f09a42b424ea1fef73f323af46d58f2c", EntriesCount: 100},
			{Name: "quality_24_videos", Digest: "6bc654807f8868b2f0a03504975abfa4fe748f30426344eb959c33f6fcd51b54", EntriesCount: 24},
		},
		ExecutionProfile: "local",
		BuildIdentity:    "7bbac44969442444934338867db4abcb29a4d24b",
		ConfigSnapshot: map[string]any{
			"zero_overrun_strict": true,
			"profile":             "local",
			"max_retries":         1,
		},
		ProviderBaselines: []benchmark.ProviderBaseline{
			{
				Role:                   "translation",
				ProviderID:             "qwen3_4b_translator",
				ModelVersion:           "Qwen3-4B-Q4_K_M",
				SnapshotManifestDigest: "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
			},
			{
				Role:                   "tts_vi",
				ProviderID:             "vieneu_tts_vi",
				ModelVersion:           "v3.2.9",
				SnapshotManifestDigest: "1278db0090b98ccf23e56f2423857fc9d32a5118",
			},
		},
		Environment: benchmark.EnvironmentAttestation{
			OS:                   "windows",
			Arch:                 "amd64",
			GPUModel:             "NVIDIA GeForce RTX 2060 SUPER",
			TotalVRAMBytes:       8589934592,
			DriverVersion:        "560.94",
			CUDAOrRuntimeVersion: "12.6",
		},
		Operator: benchmark.OperatorMetadata{
			OperatorID:     "operator-01",
			SessionPurpose: "Phase 1.1 RC Verification",
			Notes:          "Baseline Local 8 GB hard gate run",
			StartedAt:      time.Now(),
		},
	}
}

func TestSessionIdentity_Deterministic(t *testing.T) {
	input1 := sampleIdentityInput()
	id1, digest1, err := benchmark.ComputeSessionID(input1)
	if err != nil {
		t.Fatalf("compute session ID 1: %v", err)
	}

	// 1. Verify prefix and length
	if !strings.HasPrefix(id1, "bms_") {
		t.Errorf("expected session ID to start with 'bms_', got %s", id1)
	}
	if len(digest1) != 64 {
		t.Errorf("expected 64-character hex digest, got %d (%s)", len(digest1), digest1)
	}
	if id1 != "bms_"+digest1 {
		t.Errorf("expected id to be bms_+digest, got %s vs %s", id1, digest1)
	}

	// 2. Same input computes identical ID
	input2 := sampleIdentityInput()
	id2, digest2, err := benchmark.ComputeSessionID(input2)
	if err != nil {
		t.Fatalf("compute session ID 2: %v", err)
	}
	if id1 != id2 || digest1 != digest2 {
		t.Errorf("expected deterministic identical ID, got %s != %s", id1, id2)
	}

	// 3. Permuted corpus order computes identical ID
	inputPermutedCorpus := sampleIdentityInput()
	inputPermutedCorpus.CorpusDigests[0], inputPermutedCorpus.CorpusDigests[1] = inputPermutedCorpus.CorpusDigests[1], inputPermutedCorpus.CorpusDigests[0]
	idPermCorpus, _, err := benchmark.ComputeSessionID(inputPermutedCorpus)
	if err != nil {
		t.Fatalf("compute permuted corpus ID: %v", err)
	}
	if id1 != idPermCorpus {
		t.Errorf("expected corpus order permutation invariance, got %s != %s", id1, idPermCorpus)
	}

	// 4. Permuted provider baselines computes identical ID
	inputPermutedBaselines := sampleIdentityInput()
	inputPermutedBaselines.ProviderBaselines[0], inputPermutedBaselines.ProviderBaselines[1] = inputPermutedBaselines.ProviderBaselines[1], inputPermutedBaselines.ProviderBaselines[0]
	idPermBaselines, _, err := benchmark.ComputeSessionID(inputPermutedBaselines)
	if err != nil {
		t.Fatalf("compute permuted baselines ID: %v", err)
	}
	if id1 != idPermBaselines {
		t.Errorf("expected provider baselines order permutation invariance, got %s != %s", id1, idPermBaselines)
	}

	// 5. Normalization: uppercase strings in profile / hex / arch should normalize
	inputNormalized := sampleIdentityInput()
	inputNormalized.ExecutionProfile = " LOCAL "
	inputNormalized.BuildIdentity = "  7BBAC44969442444934338867DB4ABCB29A4D24B "
	inputNormalized.Environment.OS = " WINDOWS "
	inputNormalized.Environment.Arch = " AMD64 "
	idNorm, _, err := benchmark.ComputeSessionID(inputNormalized)
	if err != nil {
		t.Fatalf("compute normalized ID: %v", err)
	}
	if id1 != idNorm {
		t.Errorf("expected casing/whitespace normalization invariance, got %s != %s", id1, idNorm)
	}

	// 6. JSON formatting invariance for ConfigSnapshot
	inputFormattedConfig := sampleIdentityInput()
	inputFormattedConfig.ConfigSnapshot = `{"max_retries":1,"profile":"local","zero_overrun_strict":true}`
	idFormattedConfig, _, err := benchmark.ComputeSessionID(inputFormattedConfig)
	if err != nil {
		t.Fatalf("compute formatted config ID: %v", err)
	}
	if id1 != idFormattedConfig {
		t.Errorf("expected config JSON equivalence, got %s != %s", id1, idFormattedConfig)
	}
}

func TestSessionIdentity_ChangesProduceDifferentIDs(t *testing.T) {
	baseInput := sampleIdentityInput()
	baseID, _, err := benchmark.ComputeSessionID(baseInput)
	if err != nil {
		t.Fatalf("compute base ID: %v", err)
	}

	testCases := []struct {
		name   string
		modify func(*benchmark.SessionIdentityInput)
	}{
		{
			name: "different execution profile",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.ExecutionProfile = "hybrid"
			},
		},
		{
			name: "different build commit",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.BuildIdentity = "0000000000000000000000000000000000000000"
			},
		},
		{
			name: "different corpus digest",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.CorpusDigests[0].Digest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			},
		},
		{
			name: "additional corpus entry",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.CorpusDigests = append(in.CorpusDigests, benchmark.CorpusDigest{
					Name:   "reserve_7_videos",
					Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				})
			},
		},
		{
			name: "different config snapshot option",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.ConfigSnapshot = map[string]any{
					"zero_overrun_strict": false,
					"profile":             "local",
					"max_retries":         2,
				}
			},
		},
		{
			name: "different provider snapshot manifest digest",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.ProviderBaselines[0].SnapshotManifestDigest = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			},
		},
		{
			name: "different provider service baseline id",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.ProviderBaselines = append(in.ProviderBaselines, benchmark.ProviderBaseline{
					Role:              "translation",
					ProviderID:        "deepseek_gateway_translator",
					ServiceBaselineID: "DeepSeek-V4-Flash-0731",
				})
			},
		},
		{
			name: "different GPU hardware attestation",
			modify: func(in *benchmark.SessionIdentityInput) {
				in.Environment.GPUModel = "NVIDIA GeForce RTX 4090"
				in.Environment.TotalVRAMBytes = 25769803776
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			modInput := sampleIdentityInput()
			tc.modify(&modInput)
			modID, _, err := benchmark.ComputeSessionID(modInput)
			if err != nil {
				t.Fatalf("compute modified ID: %v", err)
			}
			if baseID == modID {
				t.Errorf("expected different session ID for modification %q, but got identical ID %s", tc.name, baseID)
			}

			diffs := benchmark.CompareSessionIdentities(baseInput, modInput)
			if len(diffs) == 0 {
				t.Errorf("expected CompareSessionIdentities to return diffs for %q, got 0 diffs", tc.name)
			}
		})
	}
}

func TestSessionIdentity_CompareIdentical(t *testing.T) {
	in1 := sampleIdentityInput()
	in2 := sampleIdentityInput()
	diffs := benchmark.CompareSessionIdentities(in1, in2)
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs for identical inputs, got %v", diffs)
	}
}

func TestSessionIdentity_NormalizationOrderInvariance(t *testing.T) {
	in1 := sampleIdentityInput()

	// in2 has permuted ordering, whitespace, and case differences that are semantically identical
	in2 := sampleIdentityInput()
	// Permute corpus digests order and add whitespace/casing
	in2.CorpusDigests = []benchmark.CorpusDigest{
		{
			Name:         " quality_24_videos ",
			Digest:       "6BC654807F8868B2F0A03504975ABFA4FE748F30426344EB959C33F6FCD51B54",
			EntriesCount: 24,
		},
		{
			Name:         "acquisition_100_urls  ",
			Digest:       "  62ce8541701236f69fb31f39ee724473f09a42b424ea1fef73f323af46d58f2c ",
			EntriesCount: 100,
		},
	}
	// Permute provider baselines order and add whitespace/casing
	in2.ProviderBaselines = []benchmark.ProviderBaseline{
		{
			Role:                   "  TTS_VI ",
			ProviderID:             "vieneu_tts_vi  ",
			ModelVersion:           "v3.2.9",
			SnapshotManifestDigest: "1278DB0090B98CCF23E56F2423857FC9D32A5118",
		},
		{
			Role:                   "TRANSLATION",
			ProviderID:             " qwen3_4b_translator",
			ModelVersion:           "Qwen3-4B-Q4_K_M",
			SnapshotManifestDigest: "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
		},
	}

	id1, digest1, err := benchmark.ComputeSessionID(in1)
	if err != nil {
		t.Fatalf("compute session ID 1: %v", err)
	}

	id2, digest2, err := benchmark.ComputeSessionID(in2)
	if err != nil {
		t.Fatalf("compute session ID 2: %v", err)
	}

	if id1 != id2 {
		t.Errorf("expected identical session ID under normalized input permutation, got %s vs %s", id1, id2)
	}
	if digest1 != digest2 {
		t.Errorf("expected identical digest under normalized input permutation, got %s vs %s", digest1, digest2)
	}

	diffs := benchmark.CompareSessionIdentities(in1, in2)
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs for semantically normalized identical inputs, got %v", diffs)
	}
}
