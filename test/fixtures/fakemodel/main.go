// Command fakemodel is a deterministic fake Qwen3 model binary for testing the
// StageWorker adapter invocation seam. It is built twice by the Seam 2 tests as
// "qwen3-asr" and "qwen3-aligner" (the names the adapters LookPath), and it
// implements the stdin JSON request / stdout JSON response contract:
//
//	qwen3-asr:     {"audio_path": "...", "run_id": "...", "attempt_id": "..."}
//	              -> {"segments": [{"start_ms":0,"end_ms":500,"text":"...","confidence":0.9,"language_code":"zh"}]}
//	qwen3-aligner: {"audio_path": "...", "text": "...", ...}
//	              -> {"word_timings": [{"word":"...","start_ms":0,"end_ms":300,"confidence":0.9}]}
//
// It is a test fixture only: real model inference is never claimed by it. Set
// FAKEMODEL_GARBAGE=1 to emit non-JSON output for fail-closed output tests.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	mode := filepath.Base(os.Args[0])

	if os.Getenv("FAKEMODEL_GARBAGE") == "1" {
		fmt.Print("this is not json {{{")
		return
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
		os.Exit(1)
	}

	switch {
	case strings.HasPrefix(mode, "qwen3-asr"):
		var req struct {
			AudioPath string `json:"audio_path"`
			RunID     string `json:"run_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			fmt.Fprintf(os.Stderr, "bad asr request: %v\n", err)
			os.Exit(1)
		}
		if strings.TrimSpace(req.AudioPath) == "" {
			fmt.Fprintf(os.Stderr, "missing audio_path\n")
			os.Exit(1)
		}
		if os.Getenv("FAKEMODEL_EMPTY_SEGMENTS") == "1" {
			out := map[string]any{"segments": []any{}}
			_ = json.NewEncoder(os.Stdout).Encode(out)
			return
		}
		text := "今天天气很好。 我们去公园散步吧。 明天再继续工作。"
		if custom := os.Getenv("FAKEMODEL_ASR_TEXT"); custom != "" {
			text = custom
		}
		out := map[string]any{
			"segments": []map[string]any{{
				"start_ms":      0,
				"end_ms":        3200,
				"text":          text,
				"confidence":    0.9,
				"language_code": "zh",
			}},
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)

	case strings.HasPrefix(mode, "qwen3-aligner"):
		var req struct {
			AudioPath string `json:"audio_path"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			fmt.Fprintf(os.Stderr, "bad aligner request: %v\n", err)
			os.Exit(1)
		}
		if strings.TrimSpace(req.AudioPath) == "" || strings.TrimSpace(req.Text) == "" {
			fmt.Fprintf(os.Stderr, "missing audio_path or text\n")
			os.Exit(1)
		}
		words := []map[string]any{}
		start := int64(0)
		for _, tok := range strings.Fields(req.Text) {
			words = append(words, map[string]any{
				"word":       tok,
				"start_ms":   start,
				"end_ms":     start + 600,
				"confidence": 0.9,
			})
			start += 1600
		}
		if len(words) == 0 {
			fmt.Fprintf(os.Stderr, "no words to align\n")
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"word_timings": words})

	case strings.HasPrefix(mode, "3dspeaker-diarizer") || strings.HasPrefix(mode, "diarizer-3dspeaker") || strings.HasPrefix(mode, "campplus-diarizer") || strings.HasPrefix(mode, "qwen3-diarizer"):
		var req struct {
			Mode      string `json:"mode"`
			AudioPath string `json:"audio_path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			fmt.Fprintf(os.Stderr, "bad diarizer request: %v\n", err)
			os.Exit(1)
		}
		if strings.TrimSpace(req.AudioPath) == "" {
			fmt.Fprintf(os.Stderr, "missing audio_path\n")
			os.Exit(1)
		}
		if req.Mode == "evidence" {
			if os.Getenv("FAKEMODEL_DIARIZER_FAIL") == "1" {
				fmt.Fprintf(os.Stderr, "diarizer evidence probe failed\n")
				os.Exit(1)
			}
			if os.Getenv("FAKEMODEL_NO_SPEAKER_EVIDENCE") == "1" {
				out := map[string]any{
					"speaker_evidence": map[string]any{
						"has_multi_speaker_cues": false,
						"speaker_change_count":   0,
						"confidence":             0.3,
						"source":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0+iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4",
					},
					"model_name":        "iic/speech_campplus_sv_zh_en_16k-common_advanced",
					"model_version":     "v1.0.0",
					"vad_model_name":    "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
					"vad_model_version": "v2.0.4",
				}
				_ = json.NewEncoder(os.Stdout).Encode(out)
				return
			}
			out := map[string]any{
				"speaker_evidence": map[string]any{
					"has_multi_speaker_cues": true,
					"speaker_change_count":   2,
					"confidence":             0.95,
					"source":                 "iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0+iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4",
				},
				"model_name":        "iic/speech_campplus_sv_zh_en_16k-common_advanced",
				"model_version":     "v1.0.0",
				"vad_model_name":    "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
				"vad_model_version": "v2.0.4",
			}
			_ = json.NewEncoder(os.Stdout).Encode(out)
			return
		}
		out := map[string]any{
			"speaker_assignments": []map[string]any{
				{
					"speaker_id": "SPEAKER_00",
					"label":      "SPEAKER_00",
					"start_ms":   0,
					"end_ms":     1500,
					"confidence": 0.95,
				},
				{
					"speaker_id": "SPEAKER_01",
					"label":      "SPEAKER_01",
					"start_ms":   1501,
					"end_ms":     4000,
					"confidence": 0.92,
				},
			},
			"model_name":        "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			"model_version":     "v1.0.0",
			"vad_model_name":    "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
			"vad_model_version": "v2.0.4",
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
	}
}
