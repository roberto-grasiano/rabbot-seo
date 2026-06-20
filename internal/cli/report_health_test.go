package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestReportHealthSection_Table_SiteScoped asserts a site-scoped report renders a
// HEALTH section with the score and delta (A6 criterion 8).
func TestReportHealthSection_Table_SiteScoped(t *testing.T) {
	t.Parallel()
	ws := 100.0
	delta := -50.0
	res := store.ReportResult{
		Changes: store.ChangeSummary{Total: 1},
		Health: &store.HealthBlock{
			Current:     store.ScopeHealth{Defined: true, Score: 50.0, KnownURLs: 2, ProcessedURLs: 2, OpenCritical: 1},
			WindowStart: &ws,
			Delta:       &delta,
			Segments: []store.SegmentHealth{
				{Name: "content", ScopeHealth: store.ScopeHealth{Defined: true, Score: 0.0}},
				{Name: "blog", ScopeHealth: store.ScopeHealth{Defined: false}}, // below coverage floor
			},
			TopRules: []store.ContributingRule{{RuleID: "title-missing", Mass: 1000}},
		},
	}
	var buf bytes.Buffer
	siteID := int64(1)
	if err := renderReportTable(&buf, res, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), &siteID); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"HEALTH", "50.0", "title-missing", "content"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q in:\n%s", want, out)
		}
	}
	// Delta is rendered as a signed number.
	if !strings.Contains(out, "-50.0") {
		t.Fatalf("table missing delta -50.0 in:\n%s", out)
	}
	// The undefined "blog" segment renders the em-dash, never 100/0.
	if !strings.Contains(out, "—") {
		t.Fatalf("undefined segment must render em-dash; got:\n%s", out)
	}
	if strings.Contains(out, "100.0") {
		t.Fatalf("undefined scope must NOT render 100.0; got:\n%s", out)
	}
}

// TestReportHealthSection_Table_UndefinedSite asserts a site below the coverage
// floor renders "—" in HEALTH, never a fake 100/0.
func TestReportHealthSection_Table_UndefinedSite(t *testing.T) {
	t.Parallel()
	res := store.ReportResult{
		Health: &store.HealthBlock{
			Current: store.ScopeHealth{Defined: false, KnownURLs: 10, ProcessedURLs: 2},
		},
	}
	var buf bytes.Buffer
	siteID := int64(1)
	if err := renderReportTable(&buf, res, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), &siteID); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "HEALTH") || !strings.Contains(out, "—") {
		t.Fatalf("undefined site HEALTH must show em-dash; got:\n%s", out)
	}
	if strings.Contains(out, "100.0") || strings.Contains(out, "0.0") {
		t.Fatalf("undefined site must not render a fake number; got:\n%s", out)
	}
}

// TestReportHealthSection_AllSitesTable asserts each PER-SITE rollup row shows its
// own live score (or "—" when undefined).
func TestReportHealthSection_AllSitesTable(t *testing.T) {
	t.Parallel()
	res := store.ReportResult{
		Sites: []store.SiteRollup{
			{SiteID: 1, BaseURL: "https://a.test", Changes: 2, OpenIssues: 1, Health: store.ScopeHealth{Defined: true, Score: 75.0}},
			{SiteID: 2, BaseURL: "https://b.test", Changes: 1, Health: store.ScopeHealth{Defined: false}},
		},
	}
	var buf bytes.Buffer
	if err := renderReportTable(&buf, res, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), nil); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "75.0") {
		t.Fatalf("site A live score 75.0 missing:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Fatalf("undefined site B must render em-dash:\n%s", out)
	}
}

// TestReportHealthSection_JSON asserts the --json carries a `health` block with the
// current score, window-start, delta, per-segment scores, and top rules.
func TestReportHealthSection_JSON(t *testing.T) {
	t.Parallel()
	ws := 100.0
	delta := -50.0
	res := store.ReportResult{
		Health: &store.HealthBlock{
			Current:     store.ScopeHealth{Defined: true, Score: 50.0, ImpactMass: 1000, MaxMass: 2000, KnownURLs: 2, ProcessedURLs: 2, OpenCritical: 1},
			WindowStart: &ws,
			Delta:       &delta,
			Segments:    []store.SegmentHealth{{Name: "content", ScopeHealth: store.ScopeHealth{Defined: true, Score: 0.0}}},
			TopRules:    []store.ContributingRule{{RuleID: "title-missing", Mass: 1000}},
		},
	}
	var buf bytes.Buffer
	siteID := int64(1)
	if err := renderReportJSON(&buf, res, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), &siteID); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	health, ok := got["health"].(map[string]any)
	if !ok {
		t.Fatalf("json missing health block: %s", buf.String())
	}
	cur := health["current"].(map[string]any)
	if cur["defined"] != true || cur["score"].(float64) != 50.0 {
		t.Fatalf("health.current = %v", cur)
	}
	if health["window_start_score"].(float64) != 100.0 || health["delta"].(float64) != -50.0 {
		t.Fatalf("health window/delta = %v / %v", health["window_start_score"], health["delta"])
	}
	segs := health["segments"].([]any)
	if len(segs) != 1 {
		t.Fatalf("health.segments = %v", segs)
	}
	rules := health["top_rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["rule_id"] != "title-missing" {
		t.Fatalf("health.top_rules = %v", rules)
	}
}

// TestReportHealthSection_JSON_OmittedAllSites asserts an all-sites report omits the
// health block (per-site scores live on each rollup instead).
func TestReportHealthSection_JSON_OmittedAllSites(t *testing.T) {
	t.Parallel()
	res := store.ReportResult{
		Sites: []store.SiteRollup{{SiteID: 1, BaseURL: "https://a.test", Changes: 1, Health: store.ScopeHealth{Defined: true, Score: 90.0}}},
	}
	var buf bytes.Buffer
	if err := renderReportJSON(&buf, res, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), nil); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if _, present := got["health"]; present {
		t.Fatalf("all-sites report must omit the health block; got %v", got["health"])
	}
	sites := got["sites"].([]any)
	site0 := sites[0].(map[string]any)
	h := site0["health"].(map[string]any)
	if h["score"].(float64) != 90.0 {
		t.Fatalf("per-site rollup health.score = %v, want 90", h["score"])
	}
}
