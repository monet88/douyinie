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
		out := map[string]any{
			"segments": []map[string]any{{
				"start_ms":      0,
				"end_ms":        500,
				"text":          "fake transcript",
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
		for _, tok := range strings.Fields(req.Text) {
			words = append(words, map[string]any{
				"word":       tok,
				"start_ms":   0,
				"end_ms":     300,
				"confidence": 0.9,
			})
		}
		if len(words) == 0 {
			fmt.Fprintf(os.Stderr, "no words to align\n")
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"word_timings": words})

	default:
		fmt.Fprintf(os.Stderr, "unknown fake model mode: %s\n", mode)
		os.Exit(1)
	}
}
