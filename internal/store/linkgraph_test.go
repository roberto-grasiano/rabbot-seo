package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// --- shared fixtures ---------------------------------------------------------

// lgSite adds a site and returns its id; base_url is the BFS root.
func lgSite(t *testing.T, db *DB, base string) int64 {
	t.Helper()
	id, err := db.AddSite(context.Background(), model.Site{BaseURL: base, Name: "t", Enabled: true})
	if err != nil {
		t.Fatalf("AddSite(%q): %v", base, err)
	}
	return id
}

// lgURL upserts a monitored page with the given importance and returns its id.
func lgURL(t *testing.T, db *DB, siteID int64, url string, importance float64) int64 {
	t.Helper()
	now := time.Now().UTC()
	id, err := db.UpsertURL(context.Background(), model.URL{
		SiteID:      siteID,
		URL:         url,
		FirstSeen:   now,
		NextCheckAt: now,
		Importance:  importance,
		StatusType:  model.StatusPage,
	})
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", url, err)
	}
	return id
}

// linkEdgesColumns returns (pk-position-by-column, all-column-names) from
// PRAGMA table_info(link_edges). Extracted so the rows close is a defer the
// sqlclosecheck linter accepts (a manual Close mid-function trips it).
func linkEdgesColumns(t *testing.T, db *DB) (pkCols map[string]int, allCols map[string]struct{}) {
	t.Helper()
	pkCols = map[string]int{}
	allCols = map[string]struct{}{}
	rows, err := db.Read().Query("PRAGMA table_info(link_edges)")
	if err != nil {
		t.Fatalf("table_info(link_edges): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			colName string
			typ     string
			notnull int
			dflt    *string
			pk      int
		)
		if err := rows.Scan(&cid, &colName, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		allCols[colName] = struct{}{}
		if pk > 0 {
			pkCols[colName] = pk
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return pkCols, allCols
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

func sameSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("set size = %d %v, want %d %v", len(got), got, len(want), want)
	}
	g, w := toSet(got), toSet(want)
	for k := range w {
		if _, ok := g[k]; !ok {
			t.Fatalf("missing %q in %v (want %v)", k, got, want)
		}
	}
}

// --- 1. Migration ------------------------------------------------------------

// TestMigration0010LinkEdges asserts a fresh DB migrates 0001→latest and the
// link_edges table exists with the composite PK + the (site_id,to_url) index,
// and urls.graph_depth is NULL-able (criterion 1).
func TestMigration0010LinkEdges(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Table exists.
	var name string
	if err := db.Read().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='link_edges'").Scan(&name); err != nil {
		t.Fatalf("link_edges table missing: %v", err)
	}

	// Composite PK on (from_url_id, to_url): PRAGMA table_info reports pk>0 for
	// both, and exactly those two.
	pkCols, allCols := linkEdgesColumns(t, db)
	for _, c := range []string{"site_id", "from_url_id", "to_url", "first_seen", "last_seen"} {
		if _, ok := allCols[c]; !ok {
			t.Errorf("link_edges missing column %q", c)
		}
	}
	if len(pkCols) != 2 || pkCols["from_url_id"] == 0 || pkCols["to_url"] == 0 {
		t.Errorf("link_edges PK = %v, want composite (from_url_id, to_url)", pkCols)
	}

	// Index exists.
	var idx string
	if err := db.Read().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_link_edges_site_to'").Scan(&idx); err != nil {
		t.Fatalf("idx_link_edges_site_to missing: %v", err)
	}

	// urls.graph_depth exists and is NULL-able (notnull=0, no default).
	uRows, err := db.Read().QueryContext(ctx, "PRAGMA table_info(urls)")
	if err != nil {
		t.Fatalf("table_info(urls): %v", err)
	}
	defer func() { _ = uRows.Close() }()
	foundDepth := false
	for uRows.Next() {
		var (
			cid     int
			colName string
			typ     string
			notnull int
			dflt    *string
			pk      int
		)
		if err := uRows.Scan(&cid, &colName, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan urls table_info: %v", err)
		}
		if colName == "graph_depth" {
			foundDepth = true
			if notnull != 0 {
				t.Errorf("urls.graph_depth notnull = %d, want 0 (NULL-able)", notnull)
			}
			if dflt != nil {
				t.Errorf("urls.graph_depth default = %q, want NULL", *dflt)
			}
			if typ != "INTEGER" {
				t.Errorf("urls.graph_depth type = %q, want INTEGER", typ)
			}
		}
	}
	if err := uRows.Err(); err != nil {
		t.Fatalf("urls table_info rows: %v", err)
	}
	if !foundDepth {
		t.Errorf("urls.graph_depth column missing")
	}

	// A fresh row reads graph_depth as NULL.
	siteID := lgSite(t, db, "https://example.com")
	urlID := lgURL(t, db, siteID, "https://example.com/p", 0.5)
	got, err := db.urlGraphDepth(ctx, urlID)
	if err != nil {
		t.Fatalf("urlGraphDepth: %v", err)
	}
	if got != nil {
		t.Errorf("fresh graph_depth = %d, want nil (NULL)", *got)
	}
}

// --- 2. SyncOutEdges ---------------------------------------------------------

// TestSyncOutEdgesFirstInsert: first sync inserts N edges with first_seen ==
// last_seen and returns them all as Added (criterion 2).
func TestSyncOutEdgesFirstInsert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	links := []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}
	delta, err := db.SyncOutEdges(ctx, siteID, from, now, links)
	if err != nil {
		t.Fatalf("SyncOutEdges: %v", err)
	}
	sameSet(t, delta.Added, links)
	if len(delta.Removed) != 0 {
		t.Errorf("Removed = %v, want empty", delta.Removed)
	}

	// first_seen == last_seen on every fresh edge.
	rows, err := db.Read().QueryContext(ctx,
		"SELECT to_url, first_seen, last_seen FROM link_edges WHERE from_url_id = ?", from)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var to string
		var fs, ls time.Time
		if err := rows.Scan(&to, &fs, &ls); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !fs.Equal(ls) {
			t.Errorf("edge %q first_seen=%v last_seen=%v, want equal on first insert", to, fs, ls)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n != 3 {
		t.Errorf("edge count = %d, want 3", n)
	}
}

// TestSyncOutEdgesIdempotent: an identical re-sync returns an empty delta but
// advances last_seen on every kept edge (first_seen stays put) (criterion 2).
func TestSyncOutEdgesIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	t0 := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	links := []string{"https://example.com/a", "https://example.com/b"}
	if _, err := db.SyncOutEdges(ctx, siteID, from, t0, links); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	t1 := t0.Add(time.Hour)
	delta, err := db.SyncOutEdges(ctx, siteID, from, t1, links)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if len(delta.Added) != 0 || len(delta.Removed) != 0 {
		t.Errorf("re-sync delta = %+v, want empty Added/Removed", delta)
	}

	// first_seen unchanged (t0), last_seen advanced (t1) on each kept edge.
	rows, err := db.Read().QueryContext(ctx,
		"SELECT to_url, first_seen, last_seen FROM link_edges WHERE from_url_id = ?", from)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var to string
		var fs, ls time.Time
		if err := rows.Scan(&to, &fs, &ls); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !fs.Equal(t0) {
			t.Errorf("edge %q first_seen=%v, want %v (unchanged)", to, fs, t0)
		}
		if !ls.Equal(t1) {
			t.Errorf("edge %q last_seen=%v, want %v (advanced)", to, ls, t1)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// TestSyncOutEdgesChangedSet: a changed out-set returns exact added/removed and
// the table converges (criterion 2). Both arms: an edge dropped AND added.
func TestSyncOutEdgesChangedSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	if _, err := db.SyncOutEdges(ctx, siteID, from, now,
		[]string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// drop b+c, keep a, add d+e.
	delta, err := db.SyncOutEdges(ctx, siteID, from, now.Add(time.Hour),
		[]string{"https://example.com/a", "https://example.com/d", "https://example.com/e"})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	sameSet(t, delta.Added, []string{"https://example.com/d", "https://example.com/e"})
	sameSet(t, delta.Removed, []string{"https://example.com/b", "https://example.com/c"})

	// Table now holds exactly a,d,e.
	got, err := scanOutEdgesRead(t, db, from)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sameSet(t, got, []string{"https://example.com/a", "https://example.com/d", "https://example.com/e"})
}

// scanOutEdgesRead reads a page's current out-set via the read pool (test helper).
func scanOutEdgesRead(t *testing.T, db *DB, fromURLID int64) ([]string, error) {
	t.Helper()
	rows, err := db.Read().Query("SELECT to_url FROM link_edges WHERE from_url_id = ?", fromURLID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			return nil, err
		}
		out = append(out, to)
	}
	return out, rows.Err()
}

// TestSyncOutEdgesOutDegreeCap: 501 links with cap 500 → exactly 500 edges,
// deterministically the first 500 in extractor order (criterion 2).
func TestSyncOutEdgesOutDegreeCap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := make([]string, 501)
	for i := range links {
		links[i] = fmt.Sprintf("https://example.com/p%04d", i)
	}
	delta, err := db.SyncOutEdgesCapped(ctx, siteID, from, time.Now().UTC(), links, 500)
	if err != nil {
		t.Fatalf("SyncOutEdgesCapped: %v", err)
	}
	if len(delta.Added) != 500 {
		t.Fatalf("Added = %d, want 500 (cap)", len(delta.Added))
	}

	var n int
	if err := db.Read().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM link_edges WHERE from_url_id = ?", from).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 500 {
		t.Errorf("edge count = %d, want 500", n)
	}

	// Deterministic FIRST-N: p0500 (the 501st) must be absent; p0499 present.
	var present int
	if err := db.Read().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM link_edges WHERE from_url_id = ? AND to_url = ?",
		from, "https://example.com/p0500").Scan(&present); err != nil {
		t.Fatalf("count overflow: %v", err)
	}
	if present != 0 {
		t.Errorf("the 501st link (p0500) was persisted; cap is not deterministic FIRST-N")
	}
	if err := db.Read().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM link_edges WHERE from_url_id = ? AND to_url = ?",
		from, "https://example.com/p0499").Scan(&present); err != nil {
		t.Fatalf("count last-kept: %v", err)
	}
	if present != 1 {
		t.Errorf("p0499 (the 500th) missing; cap dropped the wrong link")
	}
}

// TestSyncOutEdgesExactStringIdentity: /a, /a/, and /a?utm=x persist as three
// distinct to_url rows. These survive the #5 shared canonicalizer (urlx.Normalize)
// because they are genuinely distinct resources under RFC-3986 syntactic
// normalization: a trailing slash on a NON-root path can name a different resource,
// and the query string is identity-significant. (Host case, default port, %-escape
// case, and dot-segments DO collapse — see TestCanonicalIdentityCollapses; only the
// homepage's bare-host-vs-root-slash slash is folded, see
// TestHomepageNoTrailingSlashInlinks.) Fragments are still stripped.
func TestSyncOutEdgesExactStringIdentity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := []string{
		"https://example.com/a",
		"https://example.com/a/",
		"https://example.com/a?utm=x",
	}
	delta, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(), links)
	if err != nil {
		t.Fatalf("SyncOutEdges: %v", err)
	}
	if len(delta.Added) != 3 {
		t.Fatalf("Added = %d %v, want 3 distinct nodes", len(delta.Added), delta.Added)
	}
	got, err := scanOutEdgesRead(t, db, from)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sameSet(t, got, links)
}

// TestSyncOutEdgesEmptyClears: syncing an empty set removes all of a page's
// out-edges (CLOSE arm — a page that drops every link).
func TestSyncOutEdgesEmptyClears(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := []string{"https://example.com/a", "https://example.com/b"}
	if _, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(), links); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	delta, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("clear sync: %v", err)
	}
	sameSet(t, delta.Removed, links)
	if len(delta.Added) != 0 {
		t.Errorf("Added = %v, want empty", delta.Added)
	}
	got, err := scanOutEdgesRead(t, db, from)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("edges = %v, want empty after clear", got)
	}
}

// --- 4. Questions ------------------------------------------------------------

// TestWhatLinksToOrderingLimitTotal: linkers ordered importance DESC, limit
// honored, exact total ignores limit (criterion 4).
func TestWhatLinksToOrderingLimitTotal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	target := "https://example.com/money"

	// Three sources of differing importance all link to the target.
	srcHigh := lgURL(t, db, siteID, "https://example.com/high", 0.9)
	srcMid := lgURL(t, db, siteID, "https://example.com/mid", 0.5)
	srcLow := lgURL(t, db, siteID, "https://example.com/low", 0.1)
	for _, from := range []int64{srcLow, srcMid, srcHigh} { // insert low-first to defeat insertion-order luck
		if _, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(), []string{target}); err != nil {
			t.Fatalf("sync from %d: %v", from, err)
		}
	}

	// limit 2 → top 2 by importance, but total = 3.
	linkers, total, err := db.WhatLinksTo(ctx, siteID, target, 2)
	if err != nil {
		t.Fatalf("WhatLinksTo: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (exact, ignoring limit)", total)
	}
	if len(linkers) != 2 {
		t.Fatalf("returned %d linkers, want 2 (limit)", len(linkers))
	}
	if linkers[0].URL != "https://example.com/high" || linkers[1].URL != "https://example.com/mid" {
		t.Errorf("order = [%s, %s], want [high, mid] (importance DESC)", linkers[0].URL, linkers[1].URL)
	}

	// limit <= 0 → no rows, exact total still reported.
	none, total2, err := db.WhatLinksTo(ctx, siteID, target, 0)
	if err != nil {
		t.Fatalf("WhatLinksTo(limit=0): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("limit=0 returned %d linkers, want 0", len(none))
	}
	if total2 != 3 {
		t.Errorf("limit=0 total = %d, want 3", total2)
	}
}

// TestBlastRadiusBoundaryAndWeighted: importance 0.70 counts as high, 0.69 does
// not (inclusive boundary); weighted_inlinks == Σ(0.5 + 0.5·importance)
// (criterion 4).
func TestBlastRadiusBoundaryAndWeighted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	target := "https://example.com/target"

	// Two sources straddling the 0.70 boundary plus one clearly-high.
	s070 := lgURL(t, db, siteID, "https://example.com/s070", 0.70) // counts
	s069 := lgURL(t, db, siteID, "https://example.com/s069", 0.69) // does NOT
	s090 := lgURL(t, db, siteID, "https://example.com/s090", 0.90) // counts
	for _, from := range []int64{s070, s069, s090} {
		if _, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(), []string{target}); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}

	br, err := db.BlastRadius(ctx, siteID, target)
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if br.Inlinks != 3 {
		t.Errorf("Inlinks = %d, want 3", br.Inlinks)
	}
	if br.HighImportance != 2 {
		t.Errorf("HighImportance = %d, want 2 (0.70 in, 0.69 out)", br.HighImportance)
	}
	// Σ(0.5 + 0.5·imp) = (0.5+0.35) + (0.5+0.345) + (0.5+0.45) = 0.85+0.845+0.95 = 2.645
	want := (0.5 + 0.5*0.70) + (0.5 + 0.5*0.69) + (0.5 + 0.5*0.90)
	if diff := br.WeightedInlinks - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("WeightedInlinks = %v, want %v", br.WeightedInlinks, want)
	}
}

// TestBlastRadiusNeverLinked: a never-linked target reports all zeros (no NULLs).
func TestBlastRadiusNeverLinked(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")

	br, err := db.BlastRadius(ctx, siteID, "https://example.com/never")
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if br.Inlinks != 0 || br.HighImportance != 0 || br.WeightedInlinks != 0 {
		t.Errorf("never-linked = %+v, want all zeros", br)
	}
}

// TestOrphanPagesExcludesRootAndLinked: only monitored pages with zero inbound
// edges are orphans; the site root is never an orphan even with no inlinks.
func TestOrphanPagesExcludesRootAndLinked(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0) // the root — never an orphan
	linked := lgURL(t, db, siteID, "https://example.com/linked", 0.8)
	orphan := lgURL(t, db, siteID, "https://example.com/orphan", 0.4)
	_ = root
	_ = orphan

	// root → linked, so linked is not an orphan; orphan has no inbound edge.
	if _, err := db.SyncOutEdges(ctx, siteID, root, time.Now().UTC(),
		[]string{"https://example.com/linked"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	_ = linked

	orphans, err := db.OrphanPages(ctx, siteID, 0)
	if err != nil {
		t.Fatalf("OrphanPages: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %+v, want exactly 1 (the /orphan page)", orphans)
	}
	if orphans[0].URL != "https://example.com/orphan" {
		t.Errorf("orphan = %q, want https://example.com/orphan", orphans[0].URL)
	}
}

// --- 7. BFS depth sweep ------------------------------------------------------

// TestSweepGraphDepthsDiamond: a diamond with paths of DIFFERING length to the
// same node must collapse to the SHORTEST. Edges: root→a, root→b, root→c (a
// 1-hop shortcut), a→c, b→c, c→d. Two paths reach c — root→c (depth 1) and
// root→a→c / root→b→c (depth 2) — so shortest-path depth of c is 1, not 2
// (this differing-length diamond is what makes the test fail under MAX instead
// of MIN). Final depths: root=0, a=1, b=1, c=1, d=2. The first sweep leaves
// unreachable pages at NULL and reports finite-depth pages with a nil OldDepth
// (criterion 7 BFS half).
func TestSweepGraphDepthsDiamond(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.9)
	b := lgURL(t, db, siteID, "https://example.com/b", 0.9)
	c := lgURL(t, db, siteID, "https://example.com/c", 0.8)
	d := lgURL(t, db, siteID, "https://example.com/d", 0.7)
	island := lgURL(t, db, siteID, "https://example.com/island", 0.5) // unreachable from root

	now := time.Now().UTC()
	mustSync := func(from int64, links ...string) {
		if _, err := db.SyncOutEdges(ctx, siteID, from, now, links); err != nil {
			t.Fatalf("sync from %d: %v", from, err)
		}
	}
	// root reaches c BOTH directly (1 hop) and via a/b (2 hops): MIN wins → c@1.
	mustSync(root, "https://example.com/a", "https://example.com/b", "https://example.com/c")
	mustSync(a, "https://example.com/c")
	mustSync(b, "https://example.com/c")
	mustSync(c, "https://example.com/d")

	changes, err := db.SweepGraphDepths(ctx, siteID, now, 0)
	if err != nil {
		t.Fatalf("SweepGraphDepths: %v", err)
	}

	wantDepth := map[int64]int{root: 0, a: 1, b: 1, c: 1, d: 2}
	gotDepth := map[int64]int{}
	for _, ch := range changes {
		gotDepth[ch.URLID] = ch.NewDepth
		if ch.OldDepth != nil {
			t.Errorf("url %d: first-sweep OldDepth = %d, want nil", ch.URLID, *ch.OldDepth)
		}
	}
	for id, want := range wantDepth {
		if got, ok := gotDepth[id]; !ok || got != want {
			t.Errorf("url %d depth = %d (present=%v), want %d", id, got, ok, want)
		}
	}
	// The island is unreachable from root → not returned, graph_depth stays NULL.
	if _, ok := gotDepth[island]; ok {
		t.Errorf("island url %d returned, want unreachable", island)
	}
	gd, err := db.urlGraphDepth(ctx, island)
	if err != nil {
		t.Fatalf("urlGraphDepth(island): %v", err)
	}
	if gd != nil {
		t.Errorf("island graph_depth = %d, want NULL (unreachable, untouched)", *gd)
	}

	// The reachable depths were written back.
	for id, want := range wantDepth {
		gd, err := db.urlGraphDepth(ctx, id)
		if err != nil {
			t.Fatalf("urlGraphDepth(%d): %v", id, err)
		}
		if gd == nil || *gd != want {
			t.Errorf("written graph_depth[%d] = %v, want %d", id, gd, want)
		}
	}
}

// TestSweepGraphDepthsSecondSweepReportsPrior: a second sweep reports the prior
// depth (non-nil OldDepth), so the click_depth_regression layer can detect a
// 2→4 worsening. We deepen a page by rerouting its only inbound path.
func TestSweepGraphDepthsSecondSweepReportsPrior(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.9)
	b := lgURL(t, db, siteID, "https://example.com/b", 0.9)
	money := lgURL(t, db, siteID, "https://example.com/money", 0.95)

	now := time.Now().UTC()
	// Sweep 1: root→money directly (depth 1), plus a chain root→a→b for later.
	if _, err := db.SyncOutEdges(ctx, siteID, root, now,
		[]string{"https://example.com/money", "https://example.com/a"}); err != nil {
		t.Fatalf("sync root: %v", err)
	}
	if _, err := db.SyncOutEdges(ctx, siteID, a, now, []string{"https://example.com/b"}); err != nil {
		t.Fatalf("sync a: %v", err)
	}
	if _, err := db.SweepGraphDepths(ctx, siteID, now, 0); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}

	// Sweep 2: bury money under the chain (root no longer links money directly;
	// b→money). money depth goes 1 → 3.
	if _, err := db.SyncOutEdges(ctx, siteID, root, now.Add(time.Hour),
		[]string{"https://example.com/a"}); err != nil {
		t.Fatalf("reroute root: %v", err)
	}
	if _, err := db.SyncOutEdges(ctx, siteID, b, now.Add(time.Hour),
		[]string{"https://example.com/money"}); err != nil {
		t.Fatalf("link b->money: %v", err)
	}
	changes, err := db.SweepGraphDepths(ctx, siteID, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}

	var moneyChange *DepthChange
	for i := range changes {
		if changes[i].URLID == money {
			moneyChange = &changes[i]
		}
	}
	if moneyChange == nil {
		t.Fatalf("money not reported on sweep 2")
	}
	if moneyChange.OldDepth == nil {
		t.Fatalf("sweep 2 OldDepth = nil, want prior depth 1")
	}
	if *moneyChange.OldDepth != 1 {
		t.Errorf("OldDepth = %d, want 1", *moneyChange.OldDepth)
	}
	if moneyChange.NewDepth != 3 {
		t.Errorf("NewDepth = %d, want 3 (root→a→b→money)", moneyChange.NewDepth)
	}
}

// TestSweepGraphDepthsCapRespected: the BFS depth cap is pinned at exactly 20
// (criterion 7 — "cap 20 respected"). A 30-deep chain is walked only to depth
// 20; depth-21+ nodes stay NULL. Asserting against the literal 20 (not the
// MaxBFSDepth constant) makes the test fail if the cap drifts.
func TestSweepGraphDepthsCapRespected(t *testing.T) {
	if MaxBFSDepth != 20 {
		t.Fatalf("MaxBFSDepth = %d, want 20 (the spec-locked depth cap)", MaxBFSDepth)
	}
	const cap20 = 20
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	// Build a straight chain root=p0 → p1 → ... → p30 (deeper than the cap).
	const chainLen = 30
	ids := make([]int64, chainLen+1)
	urls := make([]string, chainLen+1)
	for i := 0; i <= chainLen; i++ {
		var u string
		if i == 0 {
			u = "https://example.com/"
		} else {
			u = fmt.Sprintf("https://example.com/p%d", i)
		}
		urls[i] = u
		ids[i] = lgURL(t, db, siteID, u, 0.5)
	}
	now := time.Now().UTC()
	for i := 0; i < chainLen; i++ {
		if _, err := db.SyncOutEdges(ctx, siteID, ids[i], now, []string{urls[i+1]}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	changes, err := db.SweepGraphDepths(ctx, siteID, now, 0)
	if err != nil {
		t.Fatalf("SweepGraphDepths: %v", err)
	}
	depthByID := map[int64]int{}
	for _, ch := range changes {
		depthByID[ch.URLID] = ch.NewDepth
	}
	// Nodes up to depth 20 are reachable; deeper ones are not written.
	for i := 0; i <= cap20; i++ {
		if got, ok := depthByID[ids[i]]; !ok || got != i {
			t.Errorf("p%d depth = %d (present=%v), want %d (within cap)", i, got, ok, i)
		}
	}
	for i := cap20 + 1; i <= chainLen; i++ {
		if got, ok := depthByID[ids[i]]; ok {
			t.Errorf("p%d depth = %d returned, want unreachable (beyond cap %d)", i, got, cap20)
		}
		gd, err := db.urlGraphDepth(ctx, ids[i])
		if err != nil {
			t.Fatalf("urlGraphDepth(p%d): %v", i, err)
		}
		if gd != nil {
			t.Errorf("p%d graph_depth = %d, want NULL (beyond cap)", i, *gd)
		}
	}
}

// TestSweepGraphDepthsDenseGraphTerminates is the regression guard for the
// UNION-ALL → UNION fix: a recursive CTE that uses UNION ALL enumerates every
// distinct PATH from the root, so on a densely cross-linked site (the common
// global-nav/footer shape — a near-complete subgraph) the frontier multiplies
// per level and the sweep never terminates inside the depth-20 cap, pinning a
// goroutine and stalling the single SQLite writer (a self-inflicted DoS). With
// UNION the walk dedups each (url, depth) row, bounding the frontier by
// nodes × depth, and MIN(depth) still yields correct shortest paths.
//
// A complete digraph of 24 mutually-linking pages plus a 3-cycle hangs for >15s
// under UNION ALL but completes in milliseconds under UNION. We assert the sweep
// returns within a tight deadline AND that depths are correct: in a complete
// digraph rooted at the home page every other node is depth 1 (one direct hop),
// and the cycle members are reachable too. The acyclic diamond/chain tests sail
// right past this — they are too sparse for the path count to ever explode.
func TestSweepGraphDepthsDenseGraphTerminates(t *testing.T) {
	db := openTestDB(t)
	siteID := lgSite(t, db, "https://example.com/")

	// 24 mutually-linking pages: a complete digraph (every page links every other,
	// including back to the root). This is the path-count-explosion shape.
	const n = 24
	urls := make([]string, n)
	ids := make([]int64, n)
	urls[0] = "https://example.com/"
	for i := 1; i < n; i++ {
		urls[i] = fmt.Sprintf("https://example.com/p%02d", i)
	}
	for i := 0; i < n; i++ {
		ids[i] = lgURL(t, db, siteID, urls[i], 0.5)
	}
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		out := make([]string, 0, n-1)
		for j := 0; j < n; j++ {
			if j != i {
				out = append(out, urls[j])
			}
		}
		if _, err := db.SyncOutEdges(context.Background(), siteID, ids[i], now, out); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	// A tight deadline that UNION completes well inside (ms) but UNION ALL blows
	// straight through (the live repro hung >15s). A failure here means the path
	// explosion regressed.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	done := make(chan struct{})
	var changes []DepthChange
	var sweepErr error
	go func() {
		changes, sweepErr = db.SweepGraphDepths(ctx, siteID, now, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("SweepGraphDepths did not complete on a 24-node complete digraph — path-count explosion (UNION ALL regressed?)")
	}
	if sweepErr != nil {
		t.Fatalf("SweepGraphDepths: %v", sweepErr)
	}

	// Correct shortest paths: root is 0, every other node is a single direct hop (1).
	depthByID := map[int64]int{}
	for _, ch := range changes {
		depthByID[ch.URLID] = ch.NewDepth
	}
	if got, ok := depthByID[ids[0]]; !ok || got != 0 {
		t.Errorf("root depth = %d (present=%v), want 0", got, ok)
	}
	for i := 1; i < n; i++ {
		if got, ok := depthByID[ids[i]]; !ok || got != 1 {
			t.Errorf("node %d depth = %d (present=%v), want 1 (direct hop in complete digraph)", i, got, ok)
		}
	}
}

// TestSweepGraphDepthsTightCycleTerminates: a tight cycle (root→a→b→root, plus a
// branch to c) must terminate and report correct MIN depths. A bare cycle has
// few paths so it can complete even under UNION ALL — this guards the depth
// correctness of the UNION fix on a cyclic graph specifically.
func TestSweepGraphDepthsTightCycleTerminates(t *testing.T) {
	db := openTestDB(t)
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.9)
	b := lgURL(t, db, siteID, "https://example.com/b", 0.8)
	c := lgURL(t, db, siteID, "https://example.com/c", 0.7)

	now := time.Now().UTC()
	mustSync := func(from int64, links ...string) {
		if _, err := db.SyncOutEdges(context.Background(), siteID, from, now, links); err != nil {
			t.Fatalf("sync from %d: %v", from, err)
		}
	}
	// root→a→b→root (a 3-cycle), b→c (a tail off the cycle).
	mustSync(root, "https://example.com/a")
	mustSync(a, "https://example.com/b")
	mustSync(b, "https://example.com/", "https://example.com/c")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	changes, err := db.SweepGraphDepths(ctx, siteID, now, 0)
	if err != nil {
		t.Fatalf("SweepGraphDepths: %v", err)
	}
	gotDepth := map[int64]int{}
	for _, ch := range changes {
		gotDepth[ch.URLID] = ch.NewDepth
	}
	// root=0, a=1, b=2, c=3 (root re-entry via b→root is depth 3 but MIN keeps 0).
	for id, want := range map[int64]int{root: 0, a: 1, b: 2, c: 3} {
		if got, ok := gotDepth[id]; !ok || got != want {
			t.Errorf("url %d depth = %d (present=%v), want %d", id, got, ok, want)
		}
	}
}

// TestSyncOutEdgesSurvivesRetention: link_edges are keyed by urls, not snapshots,
// so a snapshot-retention sweep must never erode the graph (the durable
// invariant: DeleteStaleSnapshots touches snapshots only).
func TestSyncOutEdgesSurvivesRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	if _, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(),
		[]string{"https://example.com/a", "https://example.com/b"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Aggressive retention sweep (delete everything old, keep 1) must not touch edges.
	if _, err := db.DeleteStaleSnapshots(ctx, time.Now().UTC().Add(time.Hour), 1, 100); err != nil {
		t.Fatalf("DeleteStaleSnapshots: %v", err)
	}
	got, err := scanOutEdgesRead(t, db, from)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	sameSet(t, got, []string{"https://example.com/a", "https://example.com/b"})
}

// --- #5 shared canonicalizer (MARQUEE bug) -----------------------------------

// TestHomepageNoTrailingSlashInlinks is the headline #5 regression: a homepage
// REGISTERED without a trailing slash (base_url "https://example.com", the literal
// value a user types) must still report its inbound links and blast radius, even
// though every href-resolved internal link to the homepage lands on
// "https://example.com/" (net/url's ResolveReference always emits at least "/").
// Before the fix, link_edges.to_url ("…/") never string-matched the urls.url
// homepage row ("…", no slash), so WhatLinksTo/BlastRadius reported ZERO. With the
// shared canonicalizer applied at BOTH write boundaries (to_url AND urls.url) the
// keyspace matches and the count is non-zero.
func TestHomepageNoTrailingSlashInlinks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Homepage registered WITHOUT a trailing slash (the bug trigger).
	siteID := lgSite(t, db, "https://example.com")
	_ = lgURL(t, db, siteID, "https://example.com", 1.0) // homepage row, no slash
	src := lgURL(t, db, siteID, "https://example.com/about", 0.8)

	// The source page links to the homepage the way a real page does: the resolved
	// absolute URL carries the root slash.
	if _, err := db.SyncOutEdges(ctx, siteID, src, time.Now().UTC(),
		[]string{"https://example.com/"}); err != nil {
		t.Fatalf("SyncOutEdges: %v", err)
	}

	// WhatLinksTo queried with the registered (no-slash) form must find the inlink.
	linkers, total, err := db.WhatLinksTo(ctx, siteID, "https://example.com", 10)
	if err != nil {
		t.Fatalf("WhatLinksTo: %v", err)
	}
	if total != 1 {
		t.Errorf("WhatLinksTo total = %d, want 1 (homepage no-slash must match resolved /)", total)
	}
	if len(linkers) != 1 || linkers[0].URL != "https://example.com/about" {
		t.Errorf("linkers = %+v, want exactly the /about source", linkers)
	}

	// Querying with the root-slash form must report the SAME inlink (one identity).
	if _, total2, err := db.WhatLinksTo(ctx, siteID, "https://example.com/", 10); err != nil {
		t.Fatalf("WhatLinksTo(/): %v", err)
	} else if total2 != 1 {
		t.Errorf("WhatLinksTo(/) total = %d, want 1 (same identity as no-slash)", total2)
	}

	// BlastRadius must likewise be non-zero for the homepage.
	br, err := db.BlastRadius(ctx, siteID, "https://example.com")
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if br.Inlinks != 1 {
		t.Errorf("BlastRadius.Inlinks = %d, want 1 (homepage no-slash)", br.Inlinks)
	}
	if br.HighImportance != 1 { // /about has importance 0.8 >= 0.70
		t.Errorf("BlastRadius.HighImportance = %d, want 1", br.HighImportance)
	}
}

// TestCanonicalIdentityCollapses asserts that host-case, default-port, %-escape
// case, and dot-segment divergences between a written to_url and a queried URL all
// collapse to ONE identity (the #5 acceptance matrix). Each case writes ONE edge
// with a divergent-but-equivalent to_url, then queries WhatLinksTo with the
// canonical form and expects exactly one inlink.
func TestCanonicalIdentityCollapses(t *testing.T) {
	cases := []struct {
		name        string
		writtenTo   string // the to_url as it would arrive from ResolveReference
		queriedForm string // the equivalent form a caller queries with
	}{
		{"host case", "https://Example.COM/b", "https://example.com/b"},
		{"default https port", "https://example.com:443/b", "https://example.com/b"},
		{"default http port", "http://example.com:80/b", "http://example.com/b"},
		{"percent-escape case", "https://example.com/%7Euser", "https://example.com/~user"},
		{"dot-segment", "https://example.com/x/../b", "https://example.com/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			siteID := lgSite(t, db, "https://example.com/")
			src := lgURL(t, db, siteID, "https://example.com/src", 0.9)

			if _, err := db.SyncOutEdges(ctx, siteID, src, time.Now().UTC(),
				[]string{tc.writtenTo}); err != nil {
				t.Fatalf("SyncOutEdges(%q): %v", tc.writtenTo, err)
			}
			_, total, err := db.WhatLinksTo(ctx, siteID, tc.queriedForm, 10)
			if err != nil {
				t.Fatalf("WhatLinksTo(%q): %v", tc.queriedForm, err)
			}
			if total != 1 {
				t.Errorf("to_url %q vs query %q: total = %d, want 1 (one identity)",
					tc.writtenTo, tc.queriedForm, total)
			}
		})
	}
}

// TestOrphanPagesCanonicalTargetMatch: a non-root page registered with a
// divergent-but-equivalent URL (host case + default port) must NOT be reported as
// an orphan when an inbound edge exists whose to_url is the canonical form. Before
// the fix, the LEFT JOIN (link_edges.to_url = urls.url) missed across the keyspace
// gap and the linked page was wrongly flagged orphaned.
func TestOrphanPagesCanonicalTargetMatch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	// Linked page registered in a divergent form (uppercase host + explicit :443).
	_ = lgURL(t, db, siteID, "https://EXAMPLE.com:443/deep", 0.8)
	// A genuine orphan (no inbound edge).
	_ = lgURL(t, db, siteID, "https://example.com/lonely", 0.4)

	// Root links to the canonical form of the deep page.
	if _, err := db.SyncOutEdges(ctx, siteID, root, time.Now().UTC(),
		[]string{"https://example.com/deep"}); err != nil {
		t.Fatalf("SyncOutEdges: %v", err)
	}

	orphans, err := db.OrphanPages(ctx, siteID, 0)
	if err != nil {
		t.Fatalf("OrphanPages: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %+v, want exactly 1 (only /lonely)", orphans)
	}
	if orphans[0].URL != "https://example.com/lonely" {
		t.Errorf("orphan = %q, want https://example.com/lonely (the linked deep page must not be orphaned)", orphans[0].URL)
	}
}

// TestOrphanPagesRootExcludedWhenBaseHasNoSlash: a site whose base_url carries NO
// trailing slash ("https://example.com") still excludes its homepage from the
// orphan list, even though the homepage urls row is stored canonical
// ("https://example.com/") and has zero inbound edges. This pins the siteRootURL
// canonicalization in the root-exclusion path: base_url is verbatim, the root must
// be canonicalized to match the stored homepage identity.
func TestOrphanPagesRootExcludedWhenBaseHasNoSlash(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")       // base WITHOUT trailing slash
	_ = lgURL(t, db, siteID, "https://example.com", 1.0) // homepage row (stored canonical as …/)
	_ = lgURL(t, db, siteID, "https://example.com/orphan", 0.4)

	orphans, err := db.OrphanPages(ctx, siteID, 0)
	if err != nil {
		t.Fatalf("OrphanPages: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %+v, want exactly 1 (the homepage must be excluded as root)", orphans)
	}
	if orphans[0].URL != "https://example.com/orphan" {
		t.Errorf("orphan = %q, want https://example.com/orphan (homepage wrongly flagged?)", orphans[0].URL)
	}
}

// TestUpsertURLCanonicalKeyspace: UpsertURL/GetURL share the canonical keyspace, so
// two equivalent forms of the same URL are the SAME row (not two), and a GetURL
// lookup with a divergent form finds the canonically-stored row. This is the
// urls.url write/key boundary half of the #5 shared-canonicalizer decision (the
// discovery dedup path relies on GetURL matching the stored, canonical value).
func TestUpsertURLCanonicalKeyspace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	now := time.Now().UTC()
	mk := func(raw string) (int64, error) {
		return db.UpsertURL(ctx, model.URL{
			SiteID: siteID, URL: raw, FirstSeen: now, NextCheckAt: now,
			Importance: 0.5, StatusType: model.StatusPage,
		})
	}
	id1, err := mk("https://example.com/Page") // path case is preserved (significant)
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// Same resource via host-case + default port: must collapse onto the SAME row.
	id2, err := mk("https://EXAMPLE.com:443/Page")
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("UpsertURL canonical keyspace: got two ids %d and %d, want one row", id1, id2)
	}

	// GetURL with yet another equivalent form resolves the same row.
	got, err := db.GetURL(ctx, siteID, "https://example.com:443/Page")
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	if got.ID != id1 {
		t.Errorf("GetURL id = %d, want %d (canonical lookup)", got.ID, id1)
	}
	if got.URL != "https://example.com/Page" {
		t.Errorf("stored url = %q, want canonical https://example.com/Page", got.URL)
	}

	// A genuinely distinct path (different case) stays a separate row.
	id3, err := mk("https://example.com/page")
	if err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	if id3 == id1 {
		t.Errorf("path-case difference collapsed (id %d == %d); path case is identity-significant", id3, id1)
	}
}
