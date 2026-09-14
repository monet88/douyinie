package server

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOperatorUIAppBehavior pins the Operator UI behaviors that string markers
// cannot: internal/server/ui/app.js is executed in a Node vm with a stubbed DOM
// and driven through its real event listeners. It fails if selection stops
// keeping video seek / active target / inspector tab / editor content in sync,
// if contextual voice audition is missing or untethered from the selected
// segment, or if non-404 artifact load failures are silently flattened to
// "no data".
//
// Requires the `node` binary used by the UI gate (`node --check
// internal/server/ui/app.js`); skipped when it is unavailable.
func TestOperatorUIAppBehavior(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run Operator UI behavior coverage")
	}

	appPath, err := filepath.Abs(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("resolve ui/app.js: %v", err)
	}
	harness := filepath.Join("testdata", "ui", "app_behavior.mjs")

	out, err := exec.Command(nodeBin, harness, appPath).CombinedOutput()
	if err != nil {
		t.Fatalf("Operator UI behavior coverage failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "operator UI behavior checks passed") {
		t.Fatalf("Operator UI behavior coverage produced no summary:\n%s", out)
	}
}
