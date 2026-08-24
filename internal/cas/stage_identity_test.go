package cas_test

import (
	"testing"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
)

func TestComputeStageCacheKey_DeterministicAndInvariance(t *testing.T) {
	input1 := domain.StageCacheIdentityInput{
		Stage:          "tts",
		InputHashes:    []string{"hash_a", "hash_b"},
		SemanticConfig: map[string]any{"voice_id": "v1", "speed": 1.0},
		ProviderID:     "fake_vieneu_tts_vi",
		ModelName:      "vieneu-v1",
		ModelVersion:   "1.0.0",
		Language:       "vi",
		SchemaVersion:  1,
	}

	key1, err := cas.ComputeStageCacheKey(input1)
	if err != nil {
		t.Fatalf("ComputeStageCacheKey failed: %v", err)
	}

	// Permuting input hashes order should produce identical key
	input2 := domain.StageCacheIdentityInput{
		Stage:          "TTS", // case insensitive
		InputHashes:    []string{"hash_b", "hash_a"},
		SemanticConfig: map[string]any{"speed": 1.0, "voice_id": "v1"}, // key order permuted
		ProviderID:     "fake_vieneu_tts_vi",
		ModelName:      "vieneu-v1",
		ModelVersion:   "1.0.0",
		Language:       "VI",
		SchemaVersion:  1,
	}

	key2, err := cas.ComputeStageCacheKey(input2)
	if err != nil {
		t.Fatalf("ComputeStageCacheKey failed: %v", err)
	}

	if key1 != key2 {
		t.Errorf("expected deterministic keys to match, got %s vs %s", key1, key2)
	}

	// Changing a semantic parameter MUST change the key
	input3 := input1
	input3.SemanticConfig = map[string]any{"voice_id": "v2", "speed": 1.0}
	key3, _ := cas.ComputeStageCacheKey(input3)
	if key1 == key3 {
		t.Errorf("expected different keys when config changes, got same: %s", key1)
	}

	// Changing language MUST change the key
	input4 := input1
	input4.Language = "en"
	key4, _ := cas.ComputeStageCacheKey(input4)
	if key1 == key4 {
		t.Errorf("expected different keys when language changes, got same: %s", key1)
	}
}
