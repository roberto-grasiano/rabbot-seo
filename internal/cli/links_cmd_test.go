package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// seedCLIGraphStore points the CLI's per-OS config/data resolution (XDG_*) at a
// temp dir, opens the store at the SAME path withStore/databasePath resolve to,
// and seeds a small site so a full `rabbot links`/`rabbot graph` invocation runs
// end to end (loadConfig -> withStore -> store.Open -> graphGrapher/runLinksFor).
//
// The seeded site (https://cli.test) has:
//   - home (root, importance 1.0) -> money edge   (money: 1 inlink, high-importance)
//   - blog (importance 0.5)        -> money edge   (money: 2 inlinks total)
//   - orphan (importance 0.9)      no inbound edge (the one orphan; root is excluded)
//
// It returns the open DB (cleanup closes it) and the site for id-bearing assertions.
// Tests using this CANNOT be t.Parallel (they mutate process env via t.Setenv).
func seedCLIGraphStore(t *testing.T) (*store.DB, model.Site) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	xdg.Reload()

	ctx := context.Background()
	// Resolve the exact DB path the CLI's withStore will open (DataDirPath under the
	// XDG_DATA_HOME we just set), so the seed and the command share one database.
	// ResolveDataDir (not DataDirPath) so the parent dir is created — the daemon
	// would have created it before writing the DB; in the test the seed stands in.
	if _, derr := config.ResolveDataDir(""); derr != nil {
		t.Fatalf("ResolveDataDir: %v", derr)
	}
	cfg := config.Defaults()
	dbPath := databasePath(&cfg)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	// base_url EQUALS the admitted root url (production seeds the root as base
	// verbatim — internal/cli/reconcile.go), so the orphan query's root exclusion
	// (u.url <> s.base_url) and the sweep's root anchor both line up.
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

	mk := func(u string, imp float64) int64 {
		id, uerr := db.UpsertURL(ctx, model.URL{
			SiteID: siteID, URL: u, FirstSeen: now, NextCheckAt: now,
			Interval: 600, Importance: imp, StatusType: model.StatusPage,
		})
		if uerr != nil {
			t.Fatalf("UpsertURL(%q): %v", u, uerr)
		}
		return id
	}
	homeID := mk("https://cli.test/", 1.0)
	blogID := mk("https://cli.test/blog", 0.5)
	mk("https://cli.test/money", 0.8)
	mk("https://cli.test/orphan", 0.9) // no inbound edge -> the sole orphan

	// home -> {blog, money} and blog -> money. money then has 2 inlinks (home +
	// blog), one of them high-importance (home at 1.0). blog has its own inlink
	// (home), so it is NOT an orphan — /orphan is the only orphan (root excluded).
	if _, err := db.SyncOutEdgesCapped(ctx, siteID, homeID, now, []string{"https://cli.test/blog", "https://cli.test/money"}, 500); err != nil {
		t.Fatalf("SyncOutEdgesCapped(home): %v", err)
	}
	if _, err := db.SyncOutEdgesCapped(ctx, siteID, blogID, now, []string{"https://cli.test/money"}, 500); err != nil {
		t.Fatalf("SyncOutEdgesCapped(blog): %v", err)
	}
	return db, site
}

// runRabbot executes the root command with args, capturing stdout. The store is
// already seeded at the XDG-resolved path by seedCLIGraphStore.
func runRabbot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestLinksFor_EndToEndTable drives `rabbot links <url>` through the full CLI path
// (siteIDForURL -> BlastRadiusCard -> renderLinksTable) over a seeded store. The
// money page has TWO inlinks, ONE high-importance (the homepage at 1.0), and both
// linkers must appear in the table. A regression in siteIDForURL (wrong site) or
// runLinksFor (wrong card) would drop these exact figures.
func TestLinksFor_EndToEndTable(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "https://cli.test/money")
	if err != nil {
		t.Fatalf("links: %v\n%s", err, out)
	}
	for _, want := range []string{
		"https://cli.test/money",
		"BLAST RADIUS",
		"inlinks 2",
		"high-importance 1",
		"TOP LINKERS (showing 2 of 2)",
		"https://cli.test/",
		"https://cli.test/blog",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("links table missing %q in:\n%s", want, out)
		}
	}
}

// TestLinksFor_EndToEndJSON drives `rabbot links <url> --json` end to end and
// asserts the exact wire numbers a downstream agent/jq consumes: 2 inlinks, 1
// high-importance, and two linker rows.
func TestLinksFor_EndToEndJSON(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "https://cli.test/money", "--json")
	if err != nil {
		t.Fatalf("links --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["url"] != "https://cli.test/money" {
		t.Fatalf("json url = %v", got["url"])
	}
	if got["inlinks"].(float64) != 2 {
		t.Fatalf("json inlinks = %v, want 2", got["inlinks"])
	}
	if got["high_importance"].(float64) != 1 {
		t.Fatalf("json high_importance = %v, want 1", got["high_importance"])
	}
	linkers, ok := got["linkers"].([]any)
	if !ok || len(linkers) != 2 {
		t.Fatalf("json linkers = %v, want 2 rows", got["linkers"])
	}
}

// TestLinksFor_LimitClipsLinkerRows proves --limit clips the listed linker rows
// while the blast-radius totals stay EXACT (the documented contract: a small limit
// keeps the table readable but never hides the true inlink mass). With --limit 1 the
// money page still reports 2 inlinks, but only the top linker is listed.
func TestLinksFor_LimitClipsLinkerRows(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "https://cli.test/money", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("links --limit 1 --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["inlinks"].(float64) != 2 {
		t.Fatalf("json inlinks = %v, want EXACT total 2 despite --limit 1", got["inlinks"])
	}
	linkers := got["linkers"].([]any)
	if len(linkers) != 1 {
		t.Fatalf("json linkers len = %d, want 1 (clipped by --limit 1)", len(linkers))
	}
	// The single retained row is the highest-importance source (the homepage).
	l0 := linkers[0].(map[string]any)
	if l0["url"] != "https://cli.test/" {
		t.Fatalf("clipped linker[0] = %v, want the homepage (top by importance)", l0["url"])
	}
}

// TestLinksFor_IslandPage drives a monitored-but-never-linked page (the orphan)
// end to end: it resolves to its site and reports zero inlinks as DATA (the island
// branch), not an error. This exercises the runLinksFor path for a page with an
// empty linker set.
func TestLinksFor_IslandPage(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "https://cli.test/orphan")
	if err != nil {
		t.Fatalf("links (island): %v\n%s", err, out)
	}
	if !strings.Contains(out, "inlinks 0") {
		t.Fatalf("island page should report inlinks 0:\n%s", out)
	}
	if !strings.Contains(out, "island") {
		t.Fatalf("island page should render the island note:\n%s", out)
	}
}

// TestLinksFor_NeverAdmittedTargetResolvesViaBaseURL exercises siteIDForURL's
// FALLBACK arm: a URL with no urls row (never admitted) but in-scope by base-URL
// prefix still resolves to its site and reports an honest "0 inlinks" island,
// rather than the no-owner error. This is the urlBelongsToSite fallback path.
func TestLinksFor_NeverAdmittedTargetResolvesViaBaseURL(t *testing.T) {
	seedCLIGraphStore(t)

	// /never-crawled is under https://cli.test but was never admitted (no urls row).
	out, err := runRabbot(t, "links", "https://cli.test/never-crawled")
	if err != nil {
		t.Fatalf("never-admitted in-scope url should resolve via base-URL fallback, got: %v\n%s", err, out)
	}
	if !strings.Contains(out, "inlinks 0") || !strings.Contains(out, "island") {
		t.Fatalf("never-admitted in-scope target should report a 0-inlink island:\n%s", out)
	}
}

// TestLinksFor_NoOwningSiteErrors exercises siteIDForURL's terminal no-owner arm:
// a URL whose host no monitored site owns is a clear caller error (not data).
func TestLinksFor_NoOwningSiteErrors(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "https://stranger.test/page")
	if err == nil {
		t.Fatalf("a URL no monitored site owns must error; out:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no monitored site owns") {
		t.Fatalf("error %q must explain no monitored site owns the URL", err)
	}
}

// TestLinksOrphans_EndToEndTable drives `rabbot links --orphans <base>` end to end:
// the site has exactly one orphan (/orphan; money has inbound edges, the root is
// excluded), so the inventory header reports (1) and lists that page.
func TestLinksOrphans_EndToEndTable(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "--orphans", "https://cli.test/")
	if err != nil {
		t.Fatalf("links --orphans: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ORPHANS (1)") {
		t.Fatalf("orphan inventory should report exactly 1 orphan:\n%s", out)
	}
	if !strings.Contains(out, "https://cli.test/orphan") {
		t.Fatalf("orphan inventory should list /orphan:\n%s", out)
	}
	if strings.Contains(out, "https://cli.test/money") {
		t.Fatalf("money has inbound edges and must NOT appear as an orphan:\n%s", out)
	}
}

// TestLinksOrphans_EndToEndJSON drives `rabbot links --orphans <base> --json` and
// asserts the wire shape: site, count 1, and the single orphan row.
func TestLinksOrphans_EndToEndJSON(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "--orphans", "https://cli.test/", "--json")
	if err != nil {
		t.Fatalf("links --orphans --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["site"] != "https://cli.test/" || got["count"].(float64) != 1 {
		t.Fatalf("json site/count = %v / %v, want https://cli.test/ / 1", got["site"], got["count"])
	}
	orphans := got["orphans"].([]any)
	if len(orphans) != 1 {
		t.Fatalf("json orphans len = %d, want 1", len(orphans))
	}
	o0 := orphans[0].(map[string]any)
	if o0["url"] != "https://cli.test/orphan" {
		t.Fatalf("json orphan[0].url = %v, want /orphan", o0["url"])
	}
}

// TestLinksOrphans_UnknownSiteErrors exercises runLinksOrphans's site-not-found
// arm: `--orphans` over a base URL no site monitors errors with the base in the
// message (the positional arg in --orphans mode IS the site base URL).
func TestLinksOrphans_UnknownSiteErrors(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "links", "--orphans", "https://nosuch.test")
	if err == nil {
		t.Fatalf("orphans for an unmonitored site must error; out:\n%s", out)
	}
	if !strings.Contains(err.Error(), "https://nosuch.test") {
		t.Fatalf("error %q must name the unknown base URL", err)
	}
}

// TestGraph_FocusEndToEndJSON drives `rabbot graph <base> --focus <url> --json`
// through graphGrapher (config-sourced caps) and the export. The money page's
// focus neighborhood must carry the mode + the two inbound edges (home->money and
// blog->money). A graphGrapher regression (wrong/zero caps) would change the
// node/edge set or truncate it.
func TestGraph_FocusEndToEndJSON(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "graph", "https://cli.test/", "--focus", "https://cli.test/money", "--json")
	if err != nil {
		t.Fatalf("graph --focus --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["mode"] != "focus" || got["focus"] != "https://cli.test/money" {
		t.Fatalf("json mode/focus = %v / %v, want focus / money", got["mode"], got["focus"])
	}
	edges, _ := got["edges"].([]any)
	var sawHome, sawBlog bool
	for _, e := range edges {
		m := e.(map[string]any)
		if m["from"] == "https://cli.test/" && m["to"] == "https://cli.test/money" {
			sawHome = true
		}
		if m["from"] == "https://cli.test/blog" && m["to"] == "https://cli.test/money" {
			sawBlog = true
		}
	}
	if !sawHome || !sawBlog {
		t.Fatalf("focus export missing inbound edges (home=%v blog=%v); edges=%v", sawHome, sawBlog, edges)
	}
}

// TestGraph_OverviewEndToEndJSON drives `rabbot graph <base> --json` (no --focus)
// through graphGrapher in OVERVIEW mode. With no segments the export folder-buckets
// edges by first path segment: home (/) links /blog and /money, and /blog links
// /money. /money has zero OUTBOUND edges so it is not a kept folder (the cap keeps
// folders by out-degree) and folds into "(other)". The deterministic, falsifiable
// observables: grouping=folder, a / -> /blog inter-folder edge, and at least one
// edge into "(other)" (the money fold). A graphGrapher regression that returned
// focus/empty output would carry none of these.
func TestGraph_OverviewEndToEndJSON(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "graph", "https://cli.test/", "--json")
	if err != nil {
		t.Fatalf("graph (overview) --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if got["mode"] != "overview" || got["grouping"] != "folder" {
		t.Fatalf("json mode/grouping = %v / %v, want overview / folder", got["mode"], got["grouping"])
	}
	ge, _ := got["group_edges"].([]any)
	var sawHomeToBlog, sawFoldedMoney bool
	for _, e := range ge {
		m := e.(map[string]any)
		if m["from"] == "/" && m["to"] == "/blog" {
			sawHomeToBlog = true
		}
		if m["to"] == "(other)" { // /money folded out by the out-degree cap
			sawFoldedMoney = true
		}
	}
	if !sawHomeToBlog {
		t.Fatalf("overview export missing the / -> /blog folder edge; group_edges=%v", ge)
	}
	if !sawFoldedMoney {
		t.Fatalf("overview export missing the folded (other) edge for /money; group_edges=%v", ge)
	}
}

// TestGraph_FocusEndToEndTable drives the human-facing table render through the
// full CLI path so the focus header + a node + an edge row all appear.
func TestGraph_FocusEndToEndTable(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "graph", "https://cli.test/", "--focus", "https://cli.test/money")
	if err != nil {
		t.Fatalf("graph --focus: %v\n%s", err, out)
	}
	for _, want := range []string{"mode", "focus", "https://cli.test/money", "NODES", "EDGES", "->"} {
		if !strings.Contains(out, want) {
			t.Fatalf("focus table missing %q in:\n%s", want, out)
		}
	}
}

// TestGraph_UnknownSiteErrors exercises the graph command's site-not-found arm:
// a base URL no site monitors errors with the base in the message.
func TestGraph_UnknownSiteErrors(t *testing.T) {
	seedCLIGraphStore(t)

	out, err := runRabbot(t, "graph", "https://nosuch.test")
	if err == nil {
		t.Fatalf("graph for an unmonitored site must error; out:\n%s", out)
	}
	if !strings.Contains(err.Error(), "https://nosuch.test") {
		t.Fatalf("error %q must name the unknown base URL", err)
	}
}

// TestGraphCmd_RejectsNegativeHops covers the hops<0 front-door guard (the sibling
// of the >2 ceiling check), which the existing ceiling test does not exercise.
func TestGraphCmd_RejectsNegativeHops(t *testing.T) {
	t.Parallel()
	out, err := runRabbotNoSeed(t, "graph", "https://cli.test", "--hops", "-1")
	if err == nil {
		t.Fatalf("--hops -1 must error; out:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--hops") || !strings.Contains(err.Error(), ">= 0") {
		t.Fatalf("error %q must say --hops must be >= 0", err)
	}
}

// runRabbotNoSeed runs the root command without seeding a store — for front-door
// validation tests that must fail BEFORE any store work.
func runRabbotNoSeed(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd(BuildInfo{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	return out.String(), cmd.Execute()
}

// TestGraphGrapher_ThreadsConfigCaps proves graphGrapher wires the config export
// caps into the Grapher (not the defaults): with a node cap of 1, a focus export
// over a multi-node neighborhood is truncated to a single node. A graphGrapher that
// ignored cfg.Graph.ExportMaxNodes (e.g. used NewGrapher with no caps) would return
// the full node set untruncated.
func TestGraphGrapher_ThreadsConfigCaps(t *testing.T) {
	t.Parallel()
	db, site := seedCLIGraphStoreDirect(t)
	ctx := context.Background()

	cfg := config.Defaults()
	cfg.Graph.ExportMaxNodes = 1 // force truncation
	cfg.Graph.ExportMaxEdges = 1
	g := graphGrapher(db, &cfg)

	exp, err := g.Export(ctx, linkgraph.Query{SiteID: site.ID, Focus: "https://cli.test/money", Hops: 2})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(exp.Nodes) != 1 {
		t.Fatalf("node cap not threaded: got %d nodes, want 1 (cap)", len(exp.Nodes))
	}
	if !exp.Truncated {
		t.Fatalf("export over a >1-node neighborhood with cap 1 must set Truncated; exp=%+v", exp)
	}
}

// seedCLIGraphStoreDirect builds a seeded site WITHOUT touching XDG env (so the
// caller can be t.Parallel and use the store directly). The base_url carries the
// trailing slash and EQUALS the admitted root url — the click-depth BFS sweep
// anchors on the urls row whose url == base_url (siteRootURL), so the root must
// match exactly for Sweep to walk the graph. The graph is root -> money and
// blog -> money, so the BFS reaches money at depth 1. Returns the DB and site.
func seedCLIGraphStoreDirect(t *testing.T) (*store.DB, model.Site) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "graphdirect.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
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
	mk := func(u string, imp float64) int64 {
		id, uerr := db.UpsertURL(ctx, model.URL{
			SiteID: siteID, URL: u, FirstSeen: now, NextCheckAt: now,
			Interval: 600, Importance: imp, StatusType: model.StatusPage,
		})
		if uerr != nil {
			t.Fatalf("UpsertURL(%q): %v", u, uerr)
		}
		return id
	}
	homeID := mk("https://cli.test/", 1.0)
	blogID := mk("https://cli.test/blog", 0.5)
	mk("https://cli.test/money", 0.8)
	if _, err := db.SyncOutEdgesCapped(ctx, siteID, homeID, now, []string{"https://cli.test/money"}, 500); err != nil {
		t.Fatalf("SyncOutEdgesCapped(home): %v", err)
	}
	if _, err := db.SyncOutEdgesCapped(ctx, siteID, blogID, now, []string{"https://cli.test/money"}, 500); err != nil {
		t.Fatalf("SyncOutEdgesCapped(blog): %v", err)
	}
	return db, site
}
