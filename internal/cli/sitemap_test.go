package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestSitemapCoverageRender_Table asserts the table render surfaces all four
// counts (the three drift buckets + seed status) and the has-sitemap flag.
func TestSitemapCoverageRender_Table(t *testing.T) {
	t.Parallel()
	res := store.SitemapCoverageResult{
		HasSitemap:           true,
		SeedStatus:           200,
		SitemappedUncrawled:  5,
		SitemappedUnadmitted: 2,
		CrawledNotInSitemap:  3,
		SampleUncrawled:      []string{"https://a.test/u1"},
		SampleNotInSitemap:   []string{"https://a.test/n1"},
	}
	var buf bytes.Buffer
	if err := renderCoverageTable(&buf, res, int64(7)); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"site 7",
		"seed status", "200",
		"sitemapped, uncrawled", "5",
		"sitemapped, unadmitted", "2",
		"crawled, not in sitemap", "3",
		"https://a.test/u1",
		"https://a.test/n1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q in:\n%s", want, out)
		}
	}
}

// TestSitemapCoverageRender_TableNoSitemap asserts a site without a watched
// sitemap renders the has_sitemap=false state, not bare zeros that look like a
// healthy, fully-reconciled site.
func TestSitemapCoverageRender_TableNoSitemap(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderCoverageTable(&buf, store.SitemapCoverageResult{HasSitemap: false}, int64(3)); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no sitemap") {
		t.Fatalf("no-sitemap render missing the no-sitemap notice:\n%s", out)
	}
}

// TestSitemapCoverageRender_JSON asserts --json round-trips the DTO field names
// (JSON-identical to the control + mcp DTOs).
func TestSitemapCoverageRender_JSON(t *testing.T) {
	t.Parallel()
	res := store.SitemapCoverageResult{
		HasSitemap:           true,
		SeedStatus:           404,
		SitemappedUncrawled:  4,
		SitemappedUnadmitted: 1,
		CrawledNotInSitemap:  2,
		SampleUncrawled:      []string{"https://a.test/u1"},
		SampleNotInSitemap:   []string{"https://a.test/n1", "https://a.test/n2"},
	}
	var buf bytes.Buffer
	if err := renderCoverageJSON(&buf, res); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if got["has_sitemap"] != true {
		t.Fatalf("has_sitemap = %v", got["has_sitemap"])
	}
	if got["seed_status"].(float64) != 404 {
		t.Fatalf("seed_status = %v", got["seed_status"])
	}
	if got["sitemapped_uncrawled"].(float64) != 4 ||
		got["sitemapped_unadmitted"].(float64) != 1 ||
		got["crawled_not_in_sitemap"].(float64) != 2 {
		t.Fatalf("counts wrong: %+v", got)
	}
	if len(got["sample_uncrawled"].([]any)) != 1 || len(got["sample_not_in_sitemap"].([]any)) != 2 {
		t.Fatalf("samples wrong: %+v", got)
	}
}

// TestSitemapParentListsCoverage asserts the bare `rabbot sitemap` parent command
// has no run function of its own and lists the `coverage` subcommand in its help.
func TestSitemapParentListsCoverage(t *testing.T) {
	t.Parallel()
	cmd := newSitemapCmd()
	if cmd.RunE != nil || cmd.Run != nil {
		t.Fatalf("sitemap parent must have no run function")
	}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	// A parent command with no run function prints help (and returns an error in
	// cobra >= 1.8 since no subcommand was given); we only care about the help text.
	_ = cmd.Help()
	out := buf.String()
	if !strings.Contains(out, "coverage") {
		t.Fatalf("`rabbot sitemap` help does not list the coverage subcommand:\n%s", out)
	}

	// The coverage subcommand must be registered under the parent.
	var found bool
	for _, c := range cmd.Commands() {
		if c.Name() == "coverage" {
			found = true
		}
	}
	if !found {
		t.Fatalf("coverage subcommand not registered under sitemap parent")
	}
}
