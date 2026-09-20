package server

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorUIFollowedCreatorSurface(t *testing.T) {
	index, err := operatorUIFS.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := operatorUIFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"Theo dõi kênh", `data-view="followed"`, `id="followed-creators"`, `id="followed-videos"`} {
		if !strings.Contains(string(index), marker) {
			t.Fatalf("followed creator UI missing %q", marker)
		}
	}
	for _, marker := range []string{"/api/v1/followed-creators", "/api/v1/discovery/videos", "data-load-older", "data-video-disposition", "data-request-download"} {
		if !strings.Contains(string(app), marker) {
			t.Fatalf("followed creator UI behavior missing %q", marker)
		}
	}
}

// TestOperatorUIAppBehavior pins the Operator UI behaviors that string markers
// cannot: internal/server/ui/app.js is executed in a Node vm with a stubbed DOM
// and driven through its real event listeners. It fails if selection stops
// keeping video seek / active target / inspector tab / editor content in sync,
// if contextual voice audition is missing or untethered from the selected
// segment, if non-404 artifact load failures are silently flattened to
// "no data", or if the direct-manipulation text-region overlay stops projecting
// canonical geometry, stops converting pointer deltas through the letterbox
// scale factor, or stops failing closed on invalid/refused geometry.
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
