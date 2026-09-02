package steering_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot returns the repository root by walking up from the test file
// until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found; cannot locate repo root")
		}
		dir = parent
	}
}

// steeringFiles are the always-loaded agent steering surface files.
var steeringFiles = []string{"AGENTS.md"}

// extractRepoRelativePaths extracts repository-relative file/directory
// pointers from markdown. It finds:
//   - Markdown link targets: [text](path)
//   - Inline code backtick paths: `path/with/extension` or `PATH.md`
//
// It skips URLs (http/https), gitnexus:// URIs, issue references,
// anchors (#), and paths starting with . (relative skill paths like
// .claude/skills/... which are external to this repo).
func extractRepoRelativePaths(content string) []string {
	var paths []string
	seen := map[string]bool{}

	// Markdown link targets: [text](path)
	linkRe := regexp.MustCompile(`\]\(([^)]+)\)`)
	for _, m := range linkRe.FindAllStringSubmatch(content, -1) {
		p := m[1]
		if isRepoRelativePath(p) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}

	// Backtick code spans referencing paths: `some/path.ext` or `SOME.md`
	codeRe := regexp.MustCompile("`([^`]+)`")
	for _, m := range codeRe.FindAllStringSubmatch(content, -1) {
		p := m[1]
		if isRepoRelativePath(p) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}

	return paths
}

func isRepoRelativePath(p string) bool {
	// Skip URLs
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return false
	}
	// Skip internal URIs
	if strings.Contains(p, "://") {
		return false
	}
	// Skip anchors
	if strings.HasPrefix(p, "#") {
		return false
	}
	// Skip external / home paths (e.g. ~/.agents/skills/..., .claude/skills/...)
	if strings.HasPrefix(p, ".") || strings.HasPrefix(p, "~") {
		return false
	}
	// Must look like a file or directory path (contains / or has an extension or ends with /)
	hasSlash := strings.Contains(p, "/")
	hasExt := strings.Contains(filepath.Base(p), ".")
	isDir := strings.HasSuffix(p, "/")
	if !hasSlash && !hasExt && !isDir {
		return false
	}
	// Skip things that look like code rather than paths
	if strings.ContainsAny(p, "{}()=,;") {
		return false
	}
	// Skip command-like content (contains spaces → likely a CLI invocation)
	if strings.Contains(p, " ") {
		return false
	}
	return true
}

// TestSteeringIntegrity_BrokenPointers validates that every
// repository-relative path referenced in the steering surface exists.
func TestSteeringIntegrity_BrokenPointers(t *testing.T) {
	root := repoRoot(t)

	for _, sf := range steeringFiles {
		content, err := os.ReadFile(filepath.Join(root, sf))
		if err != nil {
			t.Fatalf("cannot read steering file %s: %v", sf, err)
		}
		paths := extractRepoRelativePaths(string(content))
		for _, p := range paths {
			target := filepath.Join(root, filepath.FromSlash(p))
			if strings.HasSuffix(p, "/") {
				// Directory references may be lazily created (e.g. docs/adr/).
				// Validate only that the parent directory exists.
				parent := filepath.Dir(strings.TrimSuffix(target, string(filepath.Separator)))
				if _, err := os.Stat(parent); os.IsNotExist(err) {
					t.Errorf("%s: broken directory pointer %q — parent %q does not exist", sf, p, filepath.ToSlash(parent))
				}
			} else if _, err := os.Stat(target); os.IsNotExist(err) {
				t.Errorf("%s: broken pointer %q — file does not exist", sf, p)
			}
		}
	}
}

// dynamicStatsPattern matches hard-coded index statistics like
// "4165 symbols", "12,345 relationships", "500 execution flows".
// These are dynamic counts that should not appear in always-loaded
// steering guidance — they drift silently and mislead agents.
var dynamicStatsPattern = regexp.MustCompile(`\b\d[\d,]*\s+(symbols?|relationships?|execution flows?|nodes?|edges?|functions?|files?|modules?|processes)\b`)

// TestSteeringIntegrity_NoDynamicStats rejects hard-coded dynamic
// index statistics in the always-loaded steering surface.
func TestSteeringIntegrity_NoDynamicStats(t *testing.T) {
	root := repoRoot(t)

	for _, sf := range steeringFiles {
		content, err := os.ReadFile(filepath.Join(root, sf))
		if err != nil {
			t.Fatalf("cannot read %s: %v", sf, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(content)))
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if m := dynamicStatsPattern.FindString(line); m != "" {
				t.Errorf("%s:%d: hard-coded dynamic count %q — use a live resource URI instead", sf, lineNum, m)
			}
		}
	}
}

// TestSteeringIntegrity_NegativeFixtures proves each check fails with
// actionable diagnostics on known-bad input.
func TestSteeringIntegrity_NegativeFixtures(t *testing.T) {
	root := repoRoot(t)

	t.Run("broken_pointer", func(t *testing.T) {
		paths := extractRepoRelativePaths("[link](docs/nonexistent/phantom.md)")
		for _, p := range paths {
			target := filepath.Join(root, filepath.FromSlash(p))
			if _, err := os.Stat(target); err == nil {
				t.Fatal("expected broken pointer to not exist, but it does")
			}
		}
		if len(paths) == 0 {
			t.Fatal("fixture path was not extracted")
		}
		// Confirm: would produce an actionable error in the real check.
		p := paths[0]
		if p != "docs/nonexistent/phantom.md" {
			t.Fatalf("unexpected extracted path: %q", p)
		}
	})

	t.Run("dynamic_stats_detected", func(t *testing.T) {
		fixtures := []struct {
			line string
			want string
		}{
			{"The index contains 4165 symbols and growing.", "4165 symbols"},
			{"Covering 12,345 relationships across the codebase.", "12,345 relationships"},
			{"Tracks 89 execution flows.", "89 execution flows"},
		}
		for _, f := range fixtures {
			m := dynamicStatsPattern.FindString(f.line)
			if m == "" {
				t.Errorf("pattern should match %q", f.line)
			} else if m != f.want {
				t.Errorf("got %q, want %q", m, f.want)
			}
		}
	})

	t.Run("dynamic_stats_not_triggered_by_resource_uri", func(t *testing.T) {
		// Lines that reference a URI for stats should NOT match.
		clean := "read `gitnexus://repo/douyinie/context` for current index statistics (symbols, relationships, execution flows)"
		if m := dynamicStatsPattern.FindString(clean); m != "" {
			t.Errorf("should not match resource URI reference, but got %q", m)
		}
	})
}

// TestSteeringIntegrity_ExtractorSkipsNonPaths confirms the path
// extractor does not produce false positives on code references,
// URIs, anchors, and external tool paths.
func TestSteeringIntegrity_ExtractorSkipsNonPaths(t *testing.T) {
	cases := []string{
		"`impact({target: \"symbolName\", direction: \"upstream\"})`",
		"`detect_changes()`",
		"`query({search_query: \"concept\"})`",
		"`context({name: \"symbolName\"})`",
		"[link](https://github.com/example/repo)",
		"[link](gitnexus://repo/douyinie/context)",
		"[link](#section)",
		"`.claude/skills/gitnexus/SKILL.md`",
	}
	for _, c := range cases {
		paths := extractRepoRelativePaths(c)
		if len(paths) > 0 {
			t.Errorf("should not extract paths from %q, got %v", c, paths)
		}
	}
}

func TestSteeringIntegrity_Summary(t *testing.T) {
	root := repoRoot(t)
	fmt.Fprintf(os.Stderr, "steering-integrity gate: repo root=%s\n", root)

	// Quick existence check for steering files themselves.
	for _, sf := range steeringFiles {
		if _, err := os.Stat(filepath.Join(root, sf)); err != nil {
			t.Errorf("steering file %s missing: %v", sf, err)
		}
	}
}
