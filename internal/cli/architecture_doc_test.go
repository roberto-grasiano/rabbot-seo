package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// architectureDoc reads ARCHITECTURE.md from the module root (reusing the
// repoRoot helper from readme_doc_test.go). Failing to read it fails the test:
// the one-page architecture doc is a launch-gate artifact, not an optional nicety.
func architectureDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "ARCHITECTURE.md"))
	if err != nil {
		t.Fatalf("read ARCHITECTURE.md at the module root: %v", err)
	}
	return string(raw)
}

// TestArchitectureDocExists pins the falsifiable "one page" promise:
// ARCHITECTURE.md exists at the module root, is non-empty, and stays at or
// under 250 lines (editorial target is 200; 250 is the hard ceiling).
func TestArchitectureDocExists(t *testing.T) {
	t.Parallel()

	doc := architectureDoc(t)
	if strings.TrimSpace(doc) == "" {
		t.Fatal("ARCHITECTURE.md is empty")
	}
	lines := strings.Count(doc, "\n")
	if !strings.HasSuffix(doc, "\n") {
		lines++ // count a final unterminated line too
	}
	if lines > 250 {
		t.Errorf("ARCHITECTURE.md is %d lines, want <= 250 (the one-page promise)", lines)
	}
}

// TestArchitectureDocHasRecheckDiagram guards the doc's centerpiece: exactly one
// fenced ```mermaid block, holding a flowchart that names the recheck-flow spine
// stages (fetch -> extract -> diff -> rules -> alerts). One diagram, one page —
// a second mermaid block means the doc grew past its charter.
func TestArchitectureDocHasRecheckDiagram(t *testing.T) {
	t.Parallel()

	doc := architectureDoc(t)
	if n := strings.Count(doc, "```mermaid"); n != 1 {
		t.Fatalf("ARCHITECTURE.md has %d fenced mermaid blocks, want exactly 1", n)
	}

	// Isolate the block: from the ```mermaid fence to the next closing fence.
	start := strings.Index(doc, "```mermaid")
	rest := doc[start+len("```mermaid"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatal("ARCHITECTURE.md mermaid block is never closed")
	}
	block := strings.ToLower(rest[:end])

	for _, want := range []string{
		"flowchart", // the diagram type GitHub renders natively
		"fetch",     // spine: fetcher
		"extract",   // spine: extractor
		"diff",      // spine: diff.Compare
		"rules",     // spine: rules.Engine.Apply
		"alerts",    // spine: alerts.Pipeline
	} {
		if !strings.Contains(block, want) {
			t.Errorf("ARCHITECTURE.md mermaid block missing recheck-flow stage %q", want)
		}
	}
}

// TestArchitectureDocCodemapCoversInternalPackages makes the codemap
// self-policing: every directory under internal/ must be named in the doc, driven
// from the filesystem — never from memory. Adding or removing a package fails
// here until the page is updated.
func TestArchitectureDocCodemapCoversInternalPackages(t *testing.T) {
	t.Parallel()

	doc := architectureDoc(t)
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal"))
	if err != nil {
		t.Fatalf("read internal/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.Contains(doc, "| `"+e.Name()+"`") {
			t.Errorf("ARCHITECTURE.md codemap has no table row for internal/%s", e.Name())
		}
	}
}

// TestArchitectureDocAnchors pins the load-bearing invariants the page must state
// (case-insensitive). Each anchor maps to a fact verified against the code: the
// adaptive schedule column, the static-binary rule, the loopback-only control
// plane, the SQLite write discipline, the migration policy, and the JS decision.
func TestArchitectureDocAnchors(t *testing.T) {
	t.Parallel()

	low := strings.ToLower(architectureDoc(t))
	for _, want := range []string{
		"next_check_at",         // the per-URL adaptive schedule
		"cgo_enabled=0",         // pure-Go static binary
		"127.0.0.1",             // loopback-only control plane
		"single writer",         // SQLite single-writer + WAL readers
		"forward-only",          // embedded migrations policy
		"recover, don't render", // the JS decision
	} {
		if !strings.Contains(low, want) {
			t.Errorf("ARCHITECTURE.md missing invariant anchor %q", want)
		}
	}
}

// TestArchitectureDocNoStaleClaims keeps drift from the removed pre-build design
// research out of the published page: project names, licenses, dependencies, and
// packages that were never built or were explicitly dropped must not appear.
func TestArchitectureDocNoStaleClaims(t *testing.T) {
	t.Parallel()

	low := strings.ToLower(architectureDoc(t))
	for _, banned := range []string{
		"argus",           // the pre-build project name
		"apache-2.0",      // the repo is AGPL-3.0
		"go-rod",          // no headless renderer dependency
		"chromedp",        // no headless renderer dependency
		"bbolt",           // storage is SQLite (modernc.org/sqlite)
		"internal/enrich", // never built
	} {
		if strings.Contains(low, banned) {
			t.Errorf("ARCHITECTURE.md contains stale pre-build claim %q", banned)
		}
	}
}

// TestReadmeLinksArchitectureDoc guards the link-in: the README must point
// readers at ARCHITECTURE.md, or the page is unreachable from the front door.
func TestReadmeLinksArchitectureDoc(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if !strings.Contains(string(raw), "ARCHITECTURE.md") {
		t.Error("README.md does not link ARCHITECTURE.md")
	}
}
