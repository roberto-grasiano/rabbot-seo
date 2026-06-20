package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// performanceDoc reads docs/PERFORMANCE.md from the module root. Failing to read
// it fails the test: the measured-capacity page is a launch-gate artifact whose
// result-table labels must stay honest, not an optional nicety.
func performanceDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "PERFORMANCE.md"))
	if err != nil {
		t.Fatalf("read docs/PERFORMANCE.md at the module root: %v", err)
	}
	return string(raw)
}

// benchResultRow matches a numeric result-table row in PERFORMANCE.md: a leading
// backtick-quoted benchmark label followed by three measurement columns
// (time/op | B/op | allocs/op), each of which begins with a digit (after the
// orchestrator fills the real `make bench` numbers in). Requiring all three value
// cells to be digit-led is what distinguishes a benchmark-result row from a
// descriptive table (e.g. the "what the server returns" scenario table, whose
// cells are prose / backtick-quoted) and from the hardware, size-class, and
// capacity tables (whose row labels are not backtick-quoted). This anchors on the
// PUBLISHED form, so the guard keeps holding the labels honest after the fill.
var benchResultRow = regexp.MustCompile(
	"(?m)^\\| `([^`]+)`(?:[^|]*)\\| *[0-9][^|]*\\| *[0-9][^|]*\\| *[0-9][^|]*\\|\\s*$",
)

// TestPerformanceDocLabelsMatchEmittedBenchmarks makes the docs/PERFORMANCE.md
// result tables self-policing against the actual benchmark identifiers the suite
// emits. Every numeric result row's label must name a benchmark that really runs,
// and every benchmark must have a row — so a label like `Extract/medium` (which
// resolves to NO benchmark, since the subtest is `Extract/typical`) fails here,
// and a benchmark added to code without a doc row fails here too.
//
// The expected set is the authoritative short-name list (the benchmark identifier
// minus the leading "Benchmark"), pinned the same way TestArchitectureDocAnchors
// pins its invariant anchors. It is grounded in the live emission of:
//
//	CGO_ENABLED=0 go test -run '^$' -bench . -benchtime=1x \
//	  ./internal/extract/... ./internal/diff/... ./internal/store/... ./internal/scheduler/...
//
// A reader who runs `make bench` and greps the doc's label MUST find a matching
// benchmark line — that round-trip is the falsifiable contract this test guards.
func TestPerformanceDocLabelsMatchEmittedBenchmarks(t *testing.T) {
	t.Parallel()

	doc := performanceDoc(t)

	// The benchmark identifiers the B3 suite emits, as they appear in the doc's
	// numeric result tables (the "Benchmark" prefix is dropped for the label).
	// The recheck-pipeline rows carry a trailing parenthetical describing the HTTP
	// shape (e.g. "`not_modified` (304)"); the regexp captures only the backtick
	// label, so the expected names are the bare scenario suffixes here.
	wantLabels := map[string]bool{
		// internal/extract — BenchmarkExtract/<class>
		"Extract/small":   false,
		"Extract/typical": false,
		"Extract/heavy":   false,
		// internal/extract — BenchmarkMainText/<branch>
		"MainText/readability": false,
		"MainText/selector":    false,
		// internal/extract — top-level hashing benches
		"ContentSHA256": false,
		"SimHash":       false,
		// internal/diff — BenchmarkCompare/<shape>
		"Compare/no_change":          false,
		"Compare/all_fields_changed": false,
		// internal/store — top-level store benches
		"SaveSnapshot":   false,
		"LatestSnapshot": false,
		"PopDueURLs":     false,
		// internal/scheduler — BenchmarkRecheckPipeline/<scenario>
		"not_modified": false,
		"unchanged":    false,
		"changed":      false,
	}

	matches := benchResultRow.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		t.Fatal("docs/PERFORMANCE.md: found no numeric benchmark-result rows (label | {{MEASURED}} x3); the result tables are missing or reshaped")
	}

	for _, m := range matches {
		label := m[1]
		if _, ok := wantLabels[label]; !ok {
			t.Errorf("docs/PERFORMANCE.md result row label %q resolves to no emitted benchmark "+
				"(known short-names: %s) — fix the doc label or the bench subtest name", label, sortedKeys(wantLabels))
			continue
		}
		wantLabels[label] = true
	}

	for label, seen := range wantLabels {
		if !seen {
			t.Errorf("docs/PERFORMANCE.md has no result row for benchmark %q (every emitted benchmark must be published)", label)
		}
	}
}

// sortedKeys renders the expected-label set deterministically for failure
// messages (a map iterates in random order, which would make the diagnostic
// noisy across runs).
func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort — tiny, dependency-free, deterministic
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ", ")
}
