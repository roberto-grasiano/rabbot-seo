package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedCLIGSCStore points the CLI's per-OS config/data resolution at a temp dir,
// opens the store at the SAME path withStore resolves to, and seeds one site with
// GSC index-status + search-metrics rows so a full `rabbot gsc status|performance`
// invocation runs end to end (loadConfig -> withStore -> store.Open -> verb).
// Cannot be t.Parallel (mutates process env via t.Setenv).
func seedCLIGSCStore(t *testing.T) (*store.DB, model.Site) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	ctx := context.Background()
	if _, derr := config.ResolveDataDir(""); derr != nil {
		t.Fatalf("ResolveDataDir: %v", derr)
	}
	cfg := config.Defaults()
	db, err := store.Open(ctx, databasePath(&cfg))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	siteID, err := db.AddSite(ctx, model.Site{
		BaseURL: "https://cli.test/", Name: "CLI", Enabled: true,
		MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
	})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	site, err := db.GetSite(ctx, siteID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	// Admit the page so site resolution also works via the urls row (the hook path
	// uses host ownership, but the verb resolves the site the same way).
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://cli.test/p", FirstSeen: now, NextCheckAt: now, Interval: 600, Importance: 0.8, StatusType: model.StatusPage}); err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}

	lastCrawl := time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)
	if err := db.UpsertURLIndexStatus(ctx, model.URLIndexStatus{
		SiteID:          siteID,
		URL:             "https://cli.test/p",
		InspectedAt:     now,
		Verdict:         "PASS",
		CoverageState:   "Submitted and indexed",
		IndexingState:   "INDEXING_ALLOWED",
		RobotsTxtState:  "ALLOWED",
		PageFetchState:  "SUCCESSFUL",
		GoogleCanonical: "https://cli.test/p",
		UserCanonical:   "https://cli.test/p",
		CrawledAs:       "DESKTOP",
		LastCrawlTime:   &lastCrawl,
	}); err != nil {
		t.Fatalf("UpsertURLIndexStatus: %v", err)
	}
	if err := db.SaveSearchMetrics(ctx, []model.SearchMetric{
		{SiteID: siteID, URL: "https://cli.test/p", Query: "rabbit seo", Date: "2026-06-15", Clicks: 10, Impressions: 100, CTR: 0.1, Position: 4.2},
		{SiteID: siteID, URL: "https://cli.test/p", Query: "rabbit seo", Date: "2026-06-14", Clicks: 8, Impressions: 90, CTR: 0.089, Position: 4.6},
	}); err != nil {
		t.Fatalf("SaveSearchMetrics: %v", err)
	}
	return db, site
}

// TestGSCStatus_EndToEndTable drives `rabbot gsc status <url>` over a seeded store.
func TestGSCStatus_EndToEndTable(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "status", "https://cli.test/p")
	if err != nil {
		t.Fatalf("gsc status: %v\n%s", err, out)
	}
	for _, want := range []string{
		"https://cli.test/p",
		"PASS",
		"Submitted and indexed",
		"INDEXING_ALLOWED",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("gsc status table missing %q in:\n%s", want, out)
		}
	}
}

// TestGSCStatus_EndToEndJSON drives `rabbot gsc status <url> --json` and asserts
// the exact wire fields an agent/jq consumes.
func TestGSCStatus_EndToEndJSON(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "status", "https://cli.test/p", "--json")
	if err != nil {
		t.Fatalf("gsc status --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["url"] != "https://cli.test/p" {
		t.Fatalf("json url = %v", got["url"])
	}
	if got["has_status"] != true {
		t.Fatalf("json has_status = %v, want true", got["has_status"])
	}
	if got["verdict"] != "PASS" {
		t.Fatalf("json verdict = %v, want PASS", got["verdict"])
	}
	if got["google_canonical"] != "https://cli.test/p" {
		t.Fatalf("json google_canonical = %v", got["google_canonical"])
	}
}

// TestGSCStatus_UnInspectedIsHonest drives the verb against a URL with no GSC
// inspection on record and asserts it prints an honest "no inspection on record"
// line, NOT an error (the LatestURLIndexStatus ok=false / quota-bounded contract).
func TestGSCStatus_UnInspectedIsHonest(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "status", "https://cli.test/never")
	if err != nil {
		t.Fatalf("gsc status un-inspected must not error: %v\n%s", err, out)
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "no") || !strings.Contains(low, "record") {
		t.Fatalf("want an honest 'no inspection on record' line, got:\n%s", out)
	}
}

// TestGSCStatus_UnInspectedJSONHasStatusFalse asserts the --json form reports
// has_status=false for an un-inspected URL (machine-honest absent data).
func TestGSCStatus_UnInspectedJSONHasStatusFalse(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "status", "https://cli.test/never", "--json")
	if err != nil {
		t.Fatalf("gsc status --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["has_status"] != false {
		t.Fatalf("json has_status = %v, want false for an un-inspected URL", got["has_status"])
	}
}

// TestGSCPerformance_EndToEndTable drives `rabbot gsc performance --url <url>`.
func TestGSCPerformance_EndToEndTable(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "performance", "--url", "https://cli.test/p")
	if err != nil {
		t.Fatalf("gsc performance: %v\n%s", err, out)
	}
	for _, want := range []string{
		"https://cli.test/p",
		"rabbit seo",
		"2026-06-15",
		"2026-06-14",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("gsc performance table missing %q in:\n%s", want, out)
		}
	}
}

// TestGSCPerformance_EndToEndJSON asserts the rows round-trip newest-first.
func TestGSCPerformance_EndToEndJSON(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "performance", "--url", "https://cli.test/p", "--json")
	if err != nil {
		t.Fatalf("gsc performance --json: %v\n%s", err, out)
	}
	var got struct {
		URL     string `json:"url"`
		HasData bool   `json:"has_data"`
		Rows    []struct {
			Query       string  `json:"query"`
			Date        string  `json:"date"`
			Impressions int64   `json:"impressions"`
			Position    float64 `json:"position"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if !got.HasData || len(got.Rows) != 2 {
		t.Fatalf("rows = %+v, want 2 with has_data=true", got)
	}
	if got.Rows[0].Date != "2026-06-15" || got.Rows[1].Date != "2026-06-14" {
		t.Fatalf("rows not newest-first: %+v", got.Rows)
	}
	if got.Rows[0].Impressions != 100 {
		t.Fatalf("row impressions = %d, want 100", got.Rows[0].Impressions)
	}
}

// TestGSCPerformance_NoDataIsHonest asserts a URL with no metrics prints an honest
// "no search data" line, not an error.
func TestGSCPerformance_NoDataIsHonest(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "performance", "--url", "https://cli.test/nometrics")
	if err != nil {
		t.Fatalf("gsc performance no-data must not error: %v\n%s", err, out)
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "no") {
		t.Fatalf("want an honest 'no search data' line, got:\n%s", out)
	}
}

// TestGSCPerformance_SinceWindowExcludesOldDays asserts --since (a Go duration
// from now) excludes days older than the window. The seeded days are in the past,
// so a tight default-relative window of "1h" excludes BOTH — proving the window is
// actually applied (an unfiltered read would still return the rows).
func TestGSCPerformance_SinceWindowExcludesOldDays(t *testing.T) {
	seedCLIGSCStore(t)

	out, err := runRabbot(t, "gsc", "performance", "--url", "https://cli.test/p", "--since", "1h", "--json")
	if err != nil {
		t.Fatalf("gsc performance --since 1h: %v\n%s", err, out)
	}
	var got struct {
		HasData bool `json:"has_data"`
		Rows    []struct {
			Date string `json:"date"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if len(got.Rows) != 0 || got.HasData {
		t.Fatalf("a 1h window must exclude the past seeded days, got %+v", got)
	}
}
