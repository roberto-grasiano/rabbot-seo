package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestLinksRender_Table golden-tests the blast-radius card table render: the
// summary line carries the exact inlink/high-importance/weighted figures, and the
// linker rows list each source by importance.
func TestLinksRender_Table(t *testing.T) {
	t.Parallel()
	card := linkgraph.Card{
		URL:             "https://a.test/money",
		Inlinks:         3,
		HighImportance:  1,
		WeightedInlinks: 2.25,
		Linkers: []store.Linker{
			{URLID: 1, URL: "https://a.test/", Importance: 1.0},
			{URLID: 2, URL: "https://a.test/blog", Importance: 0.5},
		},
	}
	var buf bytes.Buffer
	if err := renderLinksTable(&buf, card); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"https://a.test/money",
		"BLAST RADIUS",
		"inlinks 3",
		"high-importance 1",
		"weighted 2.25",
		"TOP LINKERS (showing 2 of 3)",
		"https://a.test/",
		"https://a.test/blog",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q in:\n%s", want, out)
		}
	}
}

// TestLinksRender_TableIsland exercises the no-inlinks (island) branch so a page
// with zero inbound links renders the honest "island" line rather than an empty
// linker section.
func TestLinksRender_TableIsland(t *testing.T) {
	t.Parallel()
	card := linkgraph.Card{URL: "https://a.test/orphan"}
	var buf bytes.Buffer
	if err := renderLinksTable(&buf, card); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "inlinks 0") {
		t.Fatalf("island table missing inlinks 0:\n%s", out)
	}
	if !strings.Contains(out, "island") {
		t.Fatalf("island table missing island note:\n%s", out)
	}
}

// TestLinksRender_JSON golden-tests the blast-radius JSON: exact totals + the
// snake_case wire shape (high_importance, weighted_inlinks, linkers[].importance)
// that a downstream agent / jq consumes.
func TestLinksRender_JSON(t *testing.T) {
	t.Parallel()
	card := linkgraph.Card{
		URL:             "https://a.test/money",
		Inlinks:         3,
		HighImportance:  1,
		WeightedInlinks: 2.25,
		Linkers:         []store.Linker{{URLID: 7, URL: "https://a.test/", Importance: 1.0}},
	}
	var buf bytes.Buffer
	if err := renderLinksJSON(&buf, card); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if got["url"] != "https://a.test/money" {
		t.Fatalf("json url = %v", got["url"])
	}
	if got["inlinks"].(float64) != 3 {
		t.Fatalf("json inlinks = %v", got["inlinks"])
	}
	if got["high_importance"].(float64) != 1 {
		t.Fatalf("json high_importance = %v", got["high_importance"])
	}
	if got["weighted_inlinks"].(float64) != 2.25 {
		t.Fatalf("json weighted_inlinks = %v", got["weighted_inlinks"])
	}
	linkers, ok := got["linkers"].([]any)
	if !ok || len(linkers) != 1 {
		t.Fatalf("json linkers = %v", got["linkers"])
	}
	l0 := linkers[0].(map[string]any)
	if l0["url"] != "https://a.test/" || l0["importance"].(float64) != 1.0 {
		t.Fatalf("json linker[0] = %v", l0)
	}
}

// TestOrphansRender_Table golden-tests the orphan-inventory table: a count header
// plus an importance-ranked row per orphan.
func TestOrphansRender_Table(t *testing.T) {
	t.Parallel()
	pages := []store.OrphanPage{
		{URLID: 1, URL: "https://a.test/lost", Importance: 0.8},
		{URLID: 2, URL: "https://a.test/buried", Importance: 0.3},
	}
	var buf bytes.Buffer
	if err := renderOrphansTable(&buf, "https://a.test", pages); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ORPHANS (2)", "https://a.test/lost", "https://a.test/buried"} {
		if !strings.Contains(out, want) {
			t.Fatalf("orphans table missing %q in:\n%s", want, out)
		}
	}
}

// TestOrphansRender_TableEmpty exercises the "no orphans" branch — a healthy site
// must render the reassuring "none" line, not an empty body.
func TestOrphansRender_TableEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderOrphansTable(&buf, "https://a.test", nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ORPHANS (0)") || !strings.Contains(out, "none") {
		t.Fatalf("empty orphans table missing none line:\n%s", out)
	}
}

// TestOrphansRender_JSON golden-tests the orphan-inventory JSON shape.
func TestOrphansRender_JSON(t *testing.T) {
	t.Parallel()
	pages := []store.OrphanPage{{URLID: 9, URL: "https://a.test/lost", Importance: 0.8}}
	var buf bytes.Buffer
	if err := renderOrphansJSON(&buf, "https://a.test", pages); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if got["site"] != "https://a.test" || got["count"].(float64) != 1 {
		t.Fatalf("json site/count = %v / %v", got["site"], got["count"])
	}
	orphans := got["orphans"].([]any)
	if len(orphans) != 1 {
		t.Fatalf("json orphans len = %d", len(orphans))
	}
	o0 := orphans[0].(map[string]any)
	if o0["url"] != "https://a.test/lost" || o0["importance"].(float64) != 0.8 {
		t.Fatalf("json orphan[0] = %v", o0)
	}
}

// TestUrlBelongsToSite guards the fallback ownership check used to resolve a
// never-admitted (but in-scope) target to its site — an exact base match and a
// sub-path match are owned; an unrelated host is not, and a shorter string is not.
func TestUrlBelongsToSite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target, base string
		want         bool
	}{
		{"https://a.test", "https://a.test", true},
		{"https://a.test/blog/post", "https://a.test", true},
		{"https://a.test", "https://a.test/blog", false}, // target shorter than base
		{"https://b.test/x", "https://a.test", false},
		// Path-boundary safety: a hostname that merely EXTENDS the base host is not
		// owned by it (a bare prefix test would wrongly return true here).
		{"https://a.test.attacker.com/", "https://a.test", false},
		{"https://a.testing.test/x", "https://a.test", false},
		// A trailing slash on the base must behave identically to none.
		{"https://a.test/blog/post", "https://a.test/", true},
		{"https://a.test/", "https://a.test/", true},
	}
	for _, tc := range cases {
		if got := urlBelongsToSite(tc.target, tc.base); got != tc.want {
			t.Errorf("urlBelongsToSite(%q, %q) = %v, want %v", tc.target, tc.base, got, tc.want)
		}
	}
}

// TestLinksCmd_BadFlags asserts the CLI-layer validation rejects a non-positive
// --limit before any store work.
func TestLinksCmd_BadFlags(t *testing.T) {
	t.Parallel()
	cmd := NewRootCmd(BuildInfo{})
	cmd.SetArgs([]string{"links", "https://a.test/p", "--limit", "0"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --limit 0")
	}
	if !strings.Contains(err.Error(), "--limit") {
		t.Fatalf("error %q does not mention --limit (must fail at CLI validation)", err)
	}
}
