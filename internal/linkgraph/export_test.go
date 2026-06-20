package linkgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// --- 8. Export bounds --------------------------------------------------------

// TestExportFocusBoundedOnHugeSite builds a 1,000-page synthetic hub-and-spoke
// site (one root linking 999 leaves, each leaf linking back to root) and asserts
// the focus export is bounded: node/edge counts ≤ caps, Truncated == true, the
// EXACT full totals are reported, and the serialized JSON is < 64 KiB
// (criterion 8). A hostile fan-out must never bloat the export to megabytes.
func TestExportFocusBoundedOnHugeSite(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)

	const leaves = 999
	links := make([]string, leaves)
	for i := 0; i < leaves; i++ {
		u := fmt.Sprintf("https://example.com/p%04d", i)
		addURL(t, db, siteID, u, 0.5)
		links[i] = u
	}
	g := NewGrapher(db, WithClock(func() time.Time { return time.Now().UTC() }))
	// root → all 999 leaves (capped at maxOutlinks=500, so root has 500 out-edges).
	if err := g.SyncPage(ctx, site, root, links); err != nil {
		t.Fatalf("sync root: %v", err)
	}
	// Each of the first 500 leaves links back to root (so root's neighborhood is
	// dense both directions).
	for i := 0; i < 500; i++ {
		lu, err := db.GetURL(ctx, siteID, links[i])
		if err != nil {
			t.Fatalf("GetURL leaf %d: %v", i, err)
		}
		if err := g.SyncPage(ctx, site, lu, []string{"https://example.com/"}); err != nil {
			t.Fatalf("sync leaf %d: %v", i, err)
		}
	}

	exp, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeFocus, Focus: "https://example.com/", Hops: 1})
	if err != nil {
		t.Fatalf("Export focus: %v", err)
	}

	// Node cap: default 100, never above the hard ceiling 250.
	if len(exp.Nodes) > HardExportMaxNodes {
		t.Fatalf("nodes = %d, exceeds hard ceiling %d", len(exp.Nodes), HardExportMaxNodes)
	}
	if len(exp.Nodes) > DefaultExportMaxNodes {
		t.Fatalf("nodes = %d, exceeds default cap %d (caps not enforced)", len(exp.Nodes), DefaultExportMaxNodes)
	}
	if len(exp.Edges) > HardExportMaxEdges {
		t.Fatalf("edges = %d, exceeds hard ceiling %d", len(exp.Edges), HardExportMaxEdges)
	}
	if !exp.Truncated {
		t.Fatalf("Truncated = false on a 1000-page site, want true")
	}
	// Exact full totals are reported and exceed the rendered set.
	if exp.TotalNodes <= len(exp.Nodes) {
		t.Fatalf("TotalNodes = %d, want > rendered %d (exact totals)", exp.TotalNodes, len(exp.Nodes))
	}

	// Serialized payload is tens of KB, never MB.
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const limit = 64 * 1024
	if len(raw) >= limit {
		t.Fatalf("export JSON = %d bytes, want < %d (64 KiB)", len(raw), limit)
	}
}

// TestExportOverviewFolderScanBounded is the regression guard for the
// folder-fallback unbounded-scan defect: the no-segments overview must hard-cap
// the intermediate edge scan it folds (every other export path is bounded; this
// one used to pass 0 → the store's 100k ceiling, materializing tens of MB of edge
// strings for a large legitimate site). With the cap, a site whose edge count
// exceeds the ceiling still folds to a bounded ≤51-group graph AND reports
// Truncated=true so the agent knows the weights are a sample. We lower the cap to
// a tiny value so the truncation path is exercised without materializing tens of
// thousands of rows.
func TestExportOverviewFolderScanBounded(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	// 30 edges from 30 source pages across a handful of folders, no segments.
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	_ = root
	g := NewGrapher(db)
	const sources = 30
	for i := 0; i < sources; i++ {
		u := fmt.Sprintf("https://example.com/f%d/page%02d", i%4, i)
		_, su := addURL(t, db, siteID, u, 0.5)
		if err := g.SyncPage(ctx, site, su, []string{"https://example.com/"}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	// Cap the scan well below the real edge count → the scan clips and Truncated
	// must flip, while the response stays a bounded folder graph.
	g.overviewScanCap = 5
	exp, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeOverview})
	if err != nil {
		t.Fatalf("Export overview: %v", err)
	}
	if exp.Grouping != "folder" {
		t.Fatalf("grouping = %q, want folder (no segments)", exp.Grouping)
	}
	if !exp.Truncated {
		t.Fatalf("Truncated = false despite a scan cap (%d) below the edge count (%d); the folder scan is not bounded/flagged", g.overviewScanCap, sources)
	}
	// The folded response is still small even when the scan clipped.
	if len(exp.Groups) > MaxOverviewGroups+1 {
		t.Fatalf("groups = %d, want <= %d even with truncation", len(exp.Groups), MaxOverviewGroups+1)
	}

	// And the opposite arm: an uncapped (default) scan over the same small site
	// does NOT report Truncated.
	g2 := NewGrapher(db)
	exp2, err := g2.Export(ctx, Query{SiteID: siteID, Mode: ModeOverview})
	if err != nil {
		t.Fatalf("Export overview (uncapped): %v", err)
	}
	if exp2.Truncated {
		t.Fatalf("Truncated = true on a 30-edge site under the default %d cap, want false", HardOverviewScanEdges)
	}
}

// TestExportRejectsHops3 asserts hops > 2 is rejected clearly (criterion 8).
func TestExportRejectsHops3(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	g := NewGrapher(db)

	_, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeFocus, Focus: "https://example.com/", Hops: 3})
	if err == nil {
		t.Fatalf("hops=3 accepted, want a clear rejection error")
	}
}

// TestExportFocusHardCeilingOverridesConfig asserts the HARD ceiling bounds the
// export even when the configured cap is set absurdly high (a fat-fingered config
// can never request a multi-MB export).
func TestExportFocusHardCeilingOverridesConfig(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)

	const leaves = 600
	links := make([]string, leaves)
	for i := 0; i < leaves; i++ {
		u := fmt.Sprintf("https://example.com/p%04d", i)
		addURL(t, db, siteID, u, 0.5)
		links[i] = u
	}
	// Config asks for 100_000 nodes — the hard ceiling 250 must win.
	g := NewGrapher(db, WithExportCaps(100000, 100000), WithMaxOutlinks(1000))
	if err := g.SyncPage(ctx, site, root, links); err != nil {
		t.Fatalf("sync: %v", err)
	}

	exp, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeFocus, Focus: "https://example.com/", Hops: 1})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(exp.Nodes) > HardExportMaxNodes {
		t.Fatalf("nodes = %d, hard ceiling %d ignored despite huge config", len(exp.Nodes), HardExportMaxNodes)
	}
	if len(exp.Edges) > HardExportMaxEdges {
		t.Fatalf("edges = %d, hard ceiling %d ignored", len(exp.Edges), HardExportMaxEdges)
	}
}

// --- 9. Overview -------------------------------------------------------------

// TestExportOverviewSegmentGrouping: when a site has segments, overview groups by
// segment name with inter-segment edge weights (criterion 9).
func TestExportOverviewSegmentGrouping(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	blogID, blog := addURL(t, db, siteID, "https://example.com/blog/post", 0.5)
	_, _ = addURL(t, db, siteID, "https://example.com/money", 0.9)

	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{Name: "Blog", MatchRule: "/blog"},
		{Name: "Money", MatchRule: "/money"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	moneyRow, err := db.GetURL(ctx, siteID, "https://example.com/money")
	if err != nil {
		t.Fatalf("GetURL money: %v", err)
	}
	if err := db.SetURLSegments(ctx, blogID, []int64{ids["Blog"]}); err != nil {
		t.Fatalf("SetURLSegments blog: %v", err)
	}
	if err := db.SetURLSegments(ctx, moneyRow.ID, []int64{ids["Money"]}); err != nil {
		t.Fatalf("SetURLSegments money: %v", err)
	}

	g := NewGrapher(db)
	if err := g.SyncPage(ctx, site, blog, []string{"https://example.com/money"}); err != nil {
		t.Fatalf("sync blog→money: %v", err)
	}

	exp, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeOverview})
	if err != nil {
		t.Fatalf("Export overview: %v", err)
	}
	if exp.Grouping != "segment" {
		t.Fatalf("grouping = %q, want segment", exp.Grouping)
	}
	if len(exp.GroupEdges) != 1 {
		t.Fatalf("group edges = %+v, want one Blog→Money", exp.GroupEdges)
	}
	ge := exp.GroupEdges[0]
	if ge.From != "Blog" || ge.To != "Money" || ge.Weight != 1 {
		t.Errorf("group edge = %+v, want Blog→Money=1", ge)
	}
}

// TestExportOverviewFolderFallback: a site with NO segments folds into
// first-path-segment folders, capped at ≤ 50 groups + an "(other)" rollup
// (criterion 9).
func TestExportOverviewFolderFallback(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	// Build 60 distinct top-level folders, each with one page that links the root,
	// so there are > 50 folders → the (other) rollup must engage.
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	g := NewGrapher(db)
	for i := 0; i < 60; i++ {
		u := fmt.Sprintf("https://example.com/f%02d/page", i)
		_, su := addURL(t, db, siteID, u, 0.5)
		if err := g.SyncPage(ctx, site, su, []string{"https://example.com/"}); err != nil {
			t.Fatalf("sync folder %d: %v", i, err)
		}
	}
	_ = root
	exp, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeOverview})
	if err != nil {
		t.Fatalf("Export overview: %v", err)
	}
	if exp.Grouping != "folder" {
		t.Fatalf("grouping = %q, want folder (no segments)", exp.Grouping)
	}
	if len(exp.Groups) > MaxOverviewGroups+1 { // +1 for "(other)"
		t.Fatalf("groups = %d, want <= %d (50 + other)", len(exp.Groups), MaxOverviewGroups+1)
	}
	// "(other)" must be present given 60 > 50 distinct folders.
	foundOther := false
	for _, grp := range exp.Groups {
		if grp.Name == FolderOther {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("(other) rollup absent despite > 50 distinct folders; groups=%+v", exp.Groups)
	}
}
