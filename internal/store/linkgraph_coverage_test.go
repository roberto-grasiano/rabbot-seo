package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// This file extends linkgraph_test.go / linkgraph_export_test.go to cover the
// meaningful branches the original suite left at <100%: the cap/dedup fallback
// arms in syncOutEdges, the chunked write-back boundary in SweepGraphDepths, the
// unknown-site short circuit, and the export-query input-clamping + the two
// 0%-covered SiteHasSegments / SiteEdgesResolved reads.

// --- syncOutEdges: cap fallback + dedup ----------------------------------------

// TestSyncOutEdgesCapZeroFallsBackTo500: an explicit maxOutlinks <= 0 must NOT
// disable the bound — it falls back to 500. A 501-link page therefore still
// persists exactly 500 edges (the first 500 in extractor order). Mutating the
// `if maxOutlinks <= 0 { maxOutlinks = 500 }` fallback (e.g. to leave it 0)
// would make this insert all 501 and fail.
func TestSyncOutEdgesCapZeroFallsBackTo500(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := make([]string, 501)
	for i := range links {
		links[i] = fmt.Sprintf("https://example.com/p%04d", i)
	}
	// cap = 0 must behave exactly like the 500 default, not "unbounded".
	delta, err := db.SyncOutEdgesCapped(ctx, siteID, from, time.Now().UTC(), links, 0)
	if err != nil {
		t.Fatalf("SyncOutEdgesCapped(cap=0): %v", err)
	}
	if len(delta.Added) != 500 {
		t.Fatalf("Added = %d, want 500 (cap=0 falls back to 500, not unbounded)", len(delta.Added))
	}
	var n int
	if err := db.Read().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM link_edges WHERE from_url_id = ?", from).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 500 {
		t.Errorf("edge count = %d, want 500 (cap=0 fallback)", n)
	}
	// The overflow link (p0500) was dropped by the fallback cap.
	if scanOutEdgesHas(t, db, from, "https://example.com/p0500") {
		t.Errorf("p0500 persisted; cap=0 did not fall back to the 500-bound")
	}
}

// TestSyncOutEdgesNegativeCapFallsBackTo500 is the sibling arm: a negative cap is
// also clamped to 500. Distinct from the cap=0 case so the <= comparison (not ==)
// is exercised.
func TestSyncOutEdgesNegativeCapFallsBackTo500(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := make([]string, 600)
	for i := range links {
		links[i] = fmt.Sprintf("https://example.com/q%04d", i)
	}
	delta, err := db.SyncOutEdgesCapped(ctx, siteID, from, time.Now().UTC(), links, -7)
	if err != nil {
		t.Fatalf("SyncOutEdgesCapped(cap=-7): %v", err)
	}
	if len(delta.Added) != 500 {
		t.Errorf("Added = %d, want 500 (negative cap falls back to 500)", len(delta.Added))
	}
}

// TestSyncOutEdgesDedupsDuplicateLinks: a non-deduped input (the same to_url
// twice) must collapse to ONE edge. The PK would reject a second identical insert
// and fail the whole tx, so the in-Go dedup (`if _, dup := desiredSet[l]; dup {
// continue }`) is the only thing keeping the call from erroring — removing it
// would make this test fail with a UNIQUE violation.
func TestSyncOutEdgesDedupsDuplicateLinks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := []string{
		"https://example.com/a",
		"https://example.com/b",
		"https://example.com/a", // duplicate of the first
		"https://example.com/b", // duplicate of the second
		"https://example.com/a", // and again
	}
	delta, err := db.SyncOutEdges(ctx, siteID, from, time.Now().UTC(), links)
	if err != nil {
		t.Fatalf("SyncOutEdges with dups: %v (dedup arm broken?)", err)
	}
	sameSet(t, delta.Added, []string{"https://example.com/a", "https://example.com/b"})
	got, err := scanOutEdgesRead(t, db, from)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("edge count = %d %v, want 2 distinct (dups collapsed)", len(got), got)
	}
}

// TestSyncOutEdgesCapAfterDedupRebuildsSet exercises the cap-shrinks-the-desired-
// set rebuild (`if len(desired) < len(desiredSet) { ... }`). With duplicates the
// dedup map can hold MORE distinct keys than the capped `desired` slice keeps; if
// the membership set were not rebuilt to match the capped slice, an over-the-cap
// target would be classified "wanted" and never deleted. We seed an edge that the
// cap must drop and verify it is treated as removable, not kept.
//
// Setup: cap=2. First sync admits {a, b}. Second sync passes [a, a, c, d] — after
// dedup the distinct set is {a, c, d} (3 > cap 2), capped `desired` = [a, c].
// b must be DELETED (no longer wanted), c ADDED, d dropped by the cap and NOT
// inserted. The bug the rebuild guards: if desiredSet still held d, that is fine
// for deletes, but the rebuilt set must contain exactly {a, c} so b is removed.
func TestSyncOutEdgesCapAfterDedupRebuildsSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com")
	from := lgURL(t, db, siteID, "https://example.com/", 1.0)

	t0 := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	if _, err := db.SyncOutEdgesCapped(ctx, siteID, from, t0,
		[]string{"https://example.com/a", "https://example.com/b"}, 2); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Distinct-after-dedup = {a, c, d}; cap=2 keeps [a, c]; d is overflow.
	delta, err := db.SyncOutEdgesCapped(ctx, siteID, from, t0.Add(time.Hour),
		[]string{
			"https://example.com/a",
			"https://example.com/a", // dup
			"https://example.com/c",
			"https://example.com/d", // overflow beyond cap 2
		}, 2)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	// Only c is newly added (a kept, d dropped by cap, b removed).
	sameSet(t, delta.Added, []string{"https://example.com/c"})
	sameSet(t, delta.Removed, []string{"https://example.com/b"})

	got, err := scanOutEdgesRead(t, db, from)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Final table is exactly {a, c}: b removed, d never inserted (cap), no dup of a.
	sameSet(t, got, []string{"https://example.com/a", "https://example.com/c"})
}

// --- SweepGraphDepths: chunked write-back + unknown site -----------------------

// TestSweepGraphDepthsChunkedWriteBack drives the chunk-boundary flush: a chain of
// more nodes than one chunk forces SweepGraphDepths to flush a full batch
// mid-walk (`if len(batch) >= chunk { flush(); batch = batch[:0] }`) and then
// flush the remainder. We use chunk=2 on a 6-deep reachable chain so exactly 3
// chunk-flushes happen (and the batch reset is exercised). Every reachable depth
// must still be written and reported regardless of where the chunk boundary fell.
func TestSweepGraphDepthsChunkedWriteBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	// root=p0 → p1 → ... → p6 (7 nodes, all reachable, depths 0..6).
	const chainLen = 6
	ids := make([]int64, chainLen+1)
	urls := make([]string, chainLen+1)
	for i := 0; i <= chainLen; i++ {
		if i == 0 {
			urls[i] = "https://example.com/"
		} else {
			urls[i] = fmt.Sprintf("https://example.com/p%d", i)
		}
		ids[i] = lgURL(t, db, siteID, urls[i], 0.5)
	}
	now := time.Now().UTC()
	for i := 0; i < chainLen; i++ {
		if _, err := db.SyncOutEdges(ctx, siteID, ids[i], now, []string{urls[i+1]}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	// chunk=2 << 7 reachable nodes → multiple mid-walk flushes + a final flush.
	changes, err := db.SweepGraphDepths(ctx, siteID, now, 2)
	if err != nil {
		t.Fatalf("SweepGraphDepths(chunk=2): %v", err)
	}
	if len(changes) != chainLen+1 {
		t.Fatalf("changes = %d, want %d (all reachable nodes across chunks)", len(changes), chainLen+1)
	}
	gotDepth := map[int64]int{}
	for _, ch := range changes {
		gotDepth[ch.URLID] = ch.NewDepth
	}
	// Every node's depth was both reported AND persisted, across the chunk boundary.
	for i := 0; i <= chainLen; i++ {
		if got, ok := gotDepth[ids[i]]; !ok || got != i {
			t.Errorf("reported depth p%d = %d (present=%v), want %d", i, got, ok, i)
		}
		gd, err := db.urlGraphDepth(ctx, ids[i])
		if err != nil {
			t.Fatalf("urlGraphDepth(p%d): %v", i, err)
		}
		if gd == nil || *gd != i {
			t.Errorf("persisted depth p%d = %v, want %d (chunked write-back lost a node)", i, gd, i)
		}
	}
}

// TestSweepGraphDepthsUnknownSite: a siteID with no sites row has no base_url, so
// siteRootURL returns "" (the sql.ErrNoRows arm) and SweepGraphDepths
// short-circuits to (nil, nil) — nothing to walk. Mutating the `if root == "" {
// return nil, nil }` guard would surface an error or panic on the empty root.
func TestSweepGraphDepthsUnknownSite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	changes, err := db.SweepGraphDepths(ctx, 999999, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("SweepGraphDepths(unknown site): %v, want nil", err)
	}
	if changes != nil {
		t.Errorf("changes = %+v, want nil for an unknown site", changes)
	}
}

// TestSweepGraphDepthsRootNotYetAdmitted: the site exists (has a base_url) but the
// root url row has never been crawled, so bfsDepths anchors on a url string that
// joins no admitted row → the walk yields nothing. The sweep returns no changes
// and writes no depths (criterion 7: an unrooted graph is simply un-swept).
func TestSweepGraphDepthsRootNotYetAdmitted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	// Admit a NON-root page only; the root (base_url) is never upserted.
	lgURL(t, db, siteID, "https://example.com/loose", 0.5)

	changes, err := db.SweepGraphDepths(ctx, siteID, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("SweepGraphDepths(root not admitted): %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none (root not yet admitted → no walk)", changes)
	}
}

// --- SweepGraphDepths siteRootURL: the happy ErrNoRows is above; the non-ErrNoRows
// path of siteRootURL is exercised by every rooted sweep already. ---

// --- NeighborhoodURLs: input clamping ------------------------------------------

// TestNeighborhoodURLsClampsHops drives the three input-clamp arms: hops<0 → 0
// (focus only), hops>2 → 2, maxNodes<=0 → safe default. We build a 3-hop chain so
// the hops>2 clamp is observable (a node at hop 3 must NOT appear even when 5 is
// requested), and assert hops=-1 yields the focus alone.
func TestNeighborhoodURLsClampsHops(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.8)
	b := lgURL(t, db, siteID, "https://example.com/b", 0.7)
	c := lgURL(t, db, siteID, "https://example.com/c", 0.6) // 3 hops from root
	_, _, _ = a, b, c

	now := time.Now().UTC()
	mustSync := func(from int64, links ...string) {
		if _, err := db.SyncOutEdges(ctx, siteID, from, now, links); err != nil {
			t.Fatalf("sync from %d: %v", from, err)
		}
	}
	mustSync(root, "https://example.com/a")
	mustSync(a, "https://example.com/b")
	mustSync(b, "https://example.com/c") // c is 3 hops out

	// hops < 0 clamps to 0 → only the focus node.
	only, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/", -5, 250)
	if err != nil {
		t.Fatalf("NeighborhoodURLs(hops=-5): %v", err)
	}
	sameSet(t, only, []string{"https://example.com/"})

	// hops > 2 clamps to 2 → {root, a, b} but NOT c (which is 3 hops). A request of
	// 5 must behave exactly like 2.
	clamped, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/", 5, 250)
	if err != nil {
		t.Fatalf("NeighborhoodURLs(hops=5): %v", err)
	}
	sameSet(t, clamped, []string{
		"https://example.com/", "https://example.com/a", "https://example.com/b",
	})
	for _, u := range clamped {
		if u == "https://example.com/c" {
			t.Errorf("c (hop 3) returned with hops=5; the hops>2 clamp did not engage")
		}
	}
}

// TestNeighborhoodURLsMaxNodesFallback: maxNodes <= 0 must fall back to the safe
// ceiling (250), NOT request an unbounded result. With far fewer than 250 nodes
// in the graph a maxNodes=0 call returns the whole neighborhood (the fallback let
// it through) rather than erroring or clipping to zero rows.
func TestNeighborhoodURLsMaxNodesFallback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	lgURL(t, db, siteID, "https://example.com/a", 0.8)
	if _, err := db.SyncOutEdges(ctx, siteID, root, time.Now().UTC(),
		[]string{"https://example.com/a"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/", 1, 0)
	if err != nil {
		t.Fatalf("NeighborhoodURLs(maxNodes=0): %v", err)
	}
	sameSet(t, got, []string{"https://example.com/", "https://example.com/a"})
}

// TestNeighborhoodURLsIsolatedFocus: a focus on a page with NO edges still yields
// a one-node graph (hop 0 always includes the focus), even though the focus is not
// admitted as a urls row at all. This is the "draw this URL's blast radius before
// it's crawled" guarantee.
func TestNeighborhoodURLsIsolatedFocus(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	// No edges at all in the graph.
	got, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/lonely", 2, 250)
	if err != nil {
		t.Fatalf("NeighborhoodURLs(isolated): %v", err)
	}
	sameSet(t, got, []string{"https://example.com/lonely"})
}

// --- EdgesAmong: empty set + maxEdges fallback + clip --------------------------

// TestEdgesAmongEmptyNodeSet: an empty node set returns no edges and runs no query
// (the `if len(nodes) == 0 { return nil, nil }` short circuit). A non-nil return
// here would mean the guard was removed and an empty IN-list query ran.
func TestEdgesAmongEmptyNodeSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	lgURL(t, db, siteID, "https://example.com/a", 0.8)
	if _, err := db.SyncOutEdges(ctx, siteID, root, time.Now().UTC(),
		[]string{"https://example.com/a"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	edges, err := db.EdgesAmong(ctx, siteID, nil, 750)
	if err != nil {
		t.Fatalf("EdgesAmong(nil nodes): %v", err)
	}
	if edges != nil {
		t.Errorf("edges = %+v, want nil for empty node set", edges)
	}
}

// TestEdgesAmongMaxEdgesFallbackAndClip: maxEdges <= 0 falls back to the safe
// ceiling (so a missing cap returns rows, not zero), and an explicit small cap
// clips deterministically by (from, to) order. We make 3 induced edges and ask
// for 2 → the two lexicographically-first survive.
func TestEdgesAmongMaxEdgesFallbackAndClip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	lgURL(t, db, siteID, "https://example.com/a", 0.8)
	lgURL(t, db, siteID, "https://example.com/b", 0.7)
	lgURL(t, db, siteID, "https://example.com/c", 0.6)

	now := time.Now().UTC()
	if _, err := db.SyncOutEdges(ctx, siteID, root, now,
		[]string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	nodes := []string{
		"https://example.com/", "https://example.com/a",
		"https://example.com/b", "https://example.com/c",
	}

	// maxEdges=0 → fallback ceiling; all three induced edges returned.
	all, err := db.EdgesAmong(ctx, siteID, nodes, 0)
	if err != nil {
		t.Fatalf("EdgesAmong(maxEdges=0): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("maxEdges=0 returned %d edges, want 3 (fallback ceiling, not zero)", len(all))
	}

	// Explicit small cap clips to the (from,to)-ordered first 2: root→a, root→b.
	clipped, err := db.EdgesAmong(ctx, siteID, nodes, 2)
	if err != nil {
		t.Fatalf("EdgesAmong(maxEdges=2): %v", err)
	}
	if len(clipped) != 2 {
		t.Fatalf("clipped = %d edges, want 2", len(clipped))
	}
	if clipped[0].To != "https://example.com/a" || clipped[1].To != "https://example.com/b" {
		t.Errorf("clip order = [%s, %s], want [→a, →b] (deterministic by to)", clipped[0].To, clipped[1].To)
	}
}

// --- NodePayloads: empty set ---------------------------------------------------

// TestNodePayloadsEmptyNodeSet: an empty node set returns an empty (non-nil) map
// and runs no query (the `if len(nodes) == 0` short circuit after pre-seeding).
func TestNodePayloadsEmptyNodeSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	out, err := db.NodePayloads(ctx, siteID, nil)
	if err != nil {
		t.Fatalf("NodePayloads(nil): %v", err)
	}
	if out == nil {
		t.Fatalf("NodePayloads(nil) = nil map, want empty non-nil map")
	}
	if len(out) != 0 {
		t.Errorf("NodePayloads(nil) len = %d, want 0", len(out))
	}
}

// TestNodePayloadsGraphDepthAndSitemap: an admitted node with a swept graph_depth
// and in_sitemap set surfaces both (the depth.Valid and inSitemap != 0 scan arms).
// Original tests only covered the never-swept (NULL depth) admitted node.
func TestNodePayloadsGraphDepthAndSitemap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.8)
	_ = a

	now := time.Now().UTC()
	if _, err := db.SyncOutEdges(ctx, siteID, root, now, []string{"https://example.com/a"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Sweep to populate graph_depth (root=0, a=1).
	if _, err := db.SweepGraphDepths(ctx, siteID, now, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	payloads, err := db.NodePayloads(ctx, siteID, []string{"https://example.com/a"})
	if err != nil {
		t.Fatalf("NodePayloads: %v", err)
	}
	pa := payloads["https://example.com/a"]
	if !pa.Admitted {
		t.Fatalf("/a not admitted: %+v", pa)
	}
	if pa.GraphDepth == nil {
		t.Fatalf("/a GraphDepth = nil, want 1 (swept) — the depth.Valid arm did not fire")
	}
	if *pa.GraphDepth != 1 {
		t.Errorf("/a GraphDepth = %d, want 1", *pa.GraphDepth)
	}
}

// --- SiteHasSegments (was 0%) --------------------------------------------------

// TestSiteHasSegments covers both arms: a site with no segments reports false, and
// after defining one it reports true. The whole function was 0%-covered.
func TestSiteHasSegments(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	has, err := db.SiteHasSegments(ctx, siteID)
	if err != nil {
		t.Fatalf("SiteHasSegments(none): %v", err)
	}
	if has {
		t.Errorf("SiteHasSegments = true on a fresh site, want false")
	}

	if _, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{Name: "Blog", MatchRule: "/blog"},
	}); err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}

	has, err = db.SiteHasSegments(ctx, siteID)
	if err != nil {
		t.Fatalf("SiteHasSegments(one): %v", err)
	}
	if !has {
		t.Errorf("SiteHasSegments = false after defining a segment, want true")
	}
}

// --- SiteEdgesResolved (was 0%) ------------------------------------------------

// TestSiteEdgesResolvedOrderingAndDefaultCap covers the folder-fallback edge read:
// it returns every admitted-source edge of the site, ordered (from, to), under the
// default cap (maxEdges<=0 → 100000). Edges from a non-admitted source never
// appear (the join is intrinsic). The whole function was 0%-covered.
func TestSiteEdgesResolvedOrderingAndDefaultCap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.8)
	_ = a

	now := time.Now().UTC()
	// root → {b, a} (insert b first to prove ORDER BY to_url sorts, not insert order),
	// a → c.
	if _, err := db.SyncOutEdges(ctx, siteID, root, now,
		[]string{"https://example.com/b", "https://example.com/a"}); err != nil {
		t.Fatalf("sync root: %v", err)
	}
	if _, err := db.SyncOutEdges(ctx, siteID, a, now,
		[]string{"https://example.com/c"}); err != nil {
		t.Fatalf("sync a: %v", err)
	}

	// maxEdges<=0 → default cap; all three edges returned, ordered (from, to).
	edges, err := db.SiteEdgesResolved(ctx, siteID, 0)
	if err != nil {
		t.Fatalf("SiteEdgesResolved(maxEdges=0): %v", err)
	}
	want := []GraphEdge{
		{From: "https://example.com/", To: "https://example.com/a"},
		{From: "https://example.com/", To: "https://example.com/b"},
		{From: "https://example.com/a", To: "https://example.com/c"},
	}
	if len(edges) != len(want) {
		t.Fatalf("edges = %+v, want %+v", edges, want)
	}
	for i := range want {
		if edges[i] != want[i] {
			t.Errorf("edges[%d] = %+v, want %+v (ordering broke)", i, edges[i], want[i])
		}
	}
}

// TestSiteEdgesResolvedClipsToMaxEdges: an explicit small cap clips the scan
// deterministically by (from, to) so a hostile site cannot stream an unbounded
// result into memory. 3 edges, cap 1 → only the (from,to)-first edge survives.
func TestSiteEdgesResolvedClipsToMaxEdges(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)

	now := time.Now().UTC()
	if _, err := db.SyncOutEdges(ctx, siteID, root, now,
		[]string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	edges, err := db.SiteEdgesResolved(ctx, siteID, 1)
	if err != nil {
		t.Fatalf("SiteEdgesResolved(maxEdges=1): %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1 (clipped)", len(edges))
	}
	if edges[0].To != "https://example.com/a" {
		t.Errorf("clipped edge = %+v, want →a (first by to_url)", edges[0])
	}
}

// TestSiteEdgesResolvedEmptyGraph: a site with no edges returns no rows (the
// rows.Next never fires; out stays nil).
func TestSiteEdgesResolvedEmptyGraph(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	lgURL(t, db, siteID, "https://example.com/", 1.0)

	edges, err := db.SiteEdgesResolved(ctx, siteID, 100)
	if err != nil {
		t.Fatalf("SiteEdgesResolved(empty): %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("edges = %+v, want none for an edgeless site", edges)
	}
}

// --- OrphanPages: limit honored -----------------------------------------------

// TestOrphanPagesLimitHonored covers the `limit > 0 → LIMIT ?` arm (the original
// orphan test passed limit=0). Two orphan pages, limit 1 → the higher-importance
// one survives (ordering importance DESC).
func TestOrphanPagesLimitHonored(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	lgURL(t, db, siteID, "https://example.com/", 1.0) // root, never an orphan
	lgURL(t, db, siteID, "https://example.com/orphanHi", 0.9)
	lgURL(t, db, siteID, "https://example.com/orphanLo", 0.2)

	orphans, err := db.OrphanPages(ctx, siteID, 1)
	if err != nil {
		t.Fatalf("OrphanPages(limit=1): %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %+v, want exactly 1 (limit)", orphans)
	}
	if orphans[0].URL != "https://example.com/orphanHi" {
		t.Errorf("limited orphan = %q, want orphanHi (importance DESC)", orphans[0].URL)
	}
}

// --- WhatLinksTo: empty result -------------------------------------------------

// TestWhatLinksToNoInlinks: a target with no inbound edges returns an empty linker
// slice and total 0 (the limit>0 query runs but yields no rows). Distinct from the
// limit<=0 short circuit already covered.
func TestWhatLinksToNoInlinks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	linkers, total, err := db.WhatLinksTo(ctx, siteID, "https://example.com/never", 10)
	if err != nil {
		t.Fatalf("WhatLinksTo(no inlinks): %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(linkers) != 0 {
		t.Errorf("linkers = %+v, want empty", linkers)
	}
}

// scanOutEdgesHas reports whether fromURLID has an out-edge to `to`.
func scanOutEdgesHas(t *testing.T, db *DB, fromURLID int64, to string) bool {
	t.Helper()
	var n int
	if err := db.Read().QueryRow(
		"SELECT COUNT(*) FROM link_edges WHERE from_url_id = ? AND to_url = ?",
		fromURLID, to).Scan(&n); err != nil {
		t.Fatalf("scanOutEdgesHas(%q): %v", to, err)
	}
	return n > 0
}
