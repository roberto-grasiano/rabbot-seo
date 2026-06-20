package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from this test file to the module root (the dir containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(here)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate module root (go.mod) above the test file")
	return ""
}

// TestMCPDocDocumentsFullConnection guards the load-bearing facts of the grown MCP
// surface in docs/claude-mcp.md (the README links to it but no longer enumerates the
// tools), so the docs cannot silently regress to the old "read-only, three resources,
// zero tools" description. Each anchor maps to a spec requirement: the safe-actions
// surface, the VPS-over-SSH recipe, the three-layer confirmation, and the "open once"
// service guidance.
func TestMCPDocDocumentsFullConnection(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "claude-mcp.md"))
	if err != nil {
		t.Fatalf("read MCP doc: %v", err)
	}
	doc := string(raw)

	anchors := []struct {
		what    string
		substrs []string // ALL must be present (case-insensitive)
	}{
		{"read + safe-actions framing", []string{"read", "safe", "action"}},
		{"VPS-over-SSH recipe", []string{"--connect-remote", "\"ssh\"", "loopback"}},
		{"three-layer confirmation", []string{"rabbot doctor", "get_status", "connected"}},
		{"open-once service guidance", []string{"service install", "open once"}},
		{"destructive ops excluded", []string{"shutdown", "reload"}},
	}
	low := strings.ToLower(doc)
	for _, a := range anchors {
		for _, s := range a.substrs {
			if !strings.Contains(low, strings.ToLower(s)) {
				t.Errorf("docs/claude-mcp.md missing %s: expected substring %q", a.what, s)
			}
		}
	}
	// The old read-only claim must be GONE — its presence means the doc was not updated.
	if strings.Contains(doc, "exposes three\nread-only resources") ||
		strings.Contains(low, "deliberately minimal read-only slice") {
		t.Errorf("docs/claude-mcp.md still describes the OLD read-only-only MCP slice; it must describe the grown read+actions surface")
	}
}

// TestConfigDocDocumentsReportCommand guards that the `rabbot report` activity-digest
// command stays documented in docs/configuration.md: the command itself, each
// load-bearing flag, and the "structured facts → Claude writes the prose" framing. If the
// command grows a flag the docs forget, or the section is dropped, this fails.
func TestConfigDocDocumentsReportCommand(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("read configuration doc: %v", err)
	}
	low := strings.ToLower(string(raw))

	for _, want := range []string{
		"rabbot report",    // the command
		"--since",          // the window flag
		"--site",           // the per-site scope flag
		"--limit",          // top-changed-URL bound
		"--json",           // pipe-to-Claude flag
		"168h",             // the default 7-day window
		"structured facts", // the binary-emits-facts framing
	} {
		if !strings.Contains(low, strings.ToLower(want)) {
			t.Errorf("docs/configuration.md missing report-command doc anchor %q", want)
		}
	}
}
