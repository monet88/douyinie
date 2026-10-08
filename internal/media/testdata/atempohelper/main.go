// Command atempohelper is a controllable ffmpeg stand-in for the Issue #156 cancellation
// contract. It signals that it has started by creating the file named by
// ATEMPO_HELPER_MARKER, then blocks until its parent kills it. Because it is a real
// subprocess, a context cancellation observed while it runs exercises the same
// exec.CommandContext kill path a real ffmpeg invocation takes — with no timing guesswork.
//
// It deliberately ignores its arguments: the caller only needs a process that is running.
package main

import (
	"os"
	"time"
)

func main() {
	if marker := os.Getenv("ATEMPO_HELPER_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("started"), 0o600)
	}
	// Block until killed. A bare `select {}` would trip the runtime's deadlock detector
	// because main is the only goroutine; sleeping in a loop blocks without panicking.
	for {
		time.Sleep(time.Hour)
	}
}
