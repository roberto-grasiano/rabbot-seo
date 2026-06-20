package store

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// TestNeighborhoodURLsUndirectedHops asserts the focus-mode BFS reaches BOTH
// in- and out-neighbors within the hop cap and excludes nodes beyond it.
//
// Graph: root→a→b (out chain) and x→root (an in-edge to root). From focus=root
// at hops=1 the neighborhood is {root, a, x} (root's direct out a, root's direct
// in x). b is 2 hops out — excluded at hops=1, included at hops=2.
func TestNeighborhoodURLsUndirectedHops(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.8)
	b := lgURL(t, db, siteID, "https://example.com/b", 0.7)
	x := lgURL(t, db, siteID, "https://example.com/x", 0.6)

	now := time.Now().UTC()
	mustSync := func(from int64, links ...string) {
		if _, err := db.SyncOutEdges(ctx, siteID, from, now, links); err != nil {
			t.Fatalf("sync from %d: %v", from, err)
		}
	}
	mustSync(root, "https://example.com/a")
	mustSync(a, "https://example.com/b")
	mustSync(x, "https://example.com/") // x links root: an IN edge to root

	// hops=1 around root: {root, a, x}.
	got, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/", 1, 250)
	if err != nil {
		t.Fatalf("NeighborhoodURLs hops=1: %v", err)
	}
	sameSet(t, got, []string{"https://example.com/", "https://example.com/a", "https://example.com/x"})

	// hops=2 reaches b (root→a→b).
	got2, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/", 2, 250)
	if err != nil {
		t.Fatalf("NeighborhoodURLs hops=2: %v", err)
	}
	sameSet(t, got2, []string{
		"https://example.com/", "https://example.com/a",
		"https://example.com/b", "https://example.com/x",
	})
	_ = b
}

// TestNeighborhoodURLsNodeCap caps the returned node set deterministically
// (closest-first) so a hostile fan-out cannot blow the export budget.
func TestNeighborhoodURLsNodeCap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)

	links := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		u := "https://example.com/p" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		lgURL(t, db, siteID, u, 0.5)
		links = append(links, u)
	}
	if _, err := db.SyncOutEdges(ctx, siteID, root, time.Now().UTC(), links); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, err := db.NeighborhoodURLs(ctx, siteID, "https://example.com/", 1, 10)
	if err != nil {
		t.Fatalf("NeighborhoodURLs: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("capped node count = %d, want 10", len(got))
	}
	// The focus (depth 0) must always be present.
	foundRoot := false
	for _, u := range got {
		if u == "https://example.com/" {
			foundRoot = true
		}
	}
	if !foundRoot {
		t.Errorf("capped neighborhood missing the focus node")
	}
}

// TestEdgesAmongInducedSubgraph returns only edges with BOTH endpoints in the
// node set — no edge dangling outside the bounded neighborhood.
func TestEdgesAmongInducedSubgraph(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	root := lgURL(t, db, siteID, "https://example.com/", 1.0)
	a := lgURL(t, db, siteID, "https://example.com/a", 0.8)
	lgURL(t, db, siteID, "https://example.com/out", 0.4)

	now := time.Now().UTC()
	if _, err := db.SyncOutEdges(ctx, siteID, root, now,
		[]string{"https://example.com/a", "https://example.com/out"}); err != nil {
		t.Fatalf("sync root: %v", err)
	}
	_ = a

	nodes := []string{"https://example.com/", "https://example.com/a"}
	edges, err := db.EdgesAmong(ctx, siteID, nodes, 750)
	if err != nil {
		t.Fatalf("EdgesAmong: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges = %+v, want exactly root→a (out excluded)", edges)
	}
	if edges[0].From != "https://example.com/" || edges[0].To != "https://example.com/a" {
		t.Errorf("edge = %+v, want root→a", edges[0])
	}
}

// TestNodePayloadsAdmittedAndNot: an admitted node carries its payload, a
// never-admitted target carries URL + Admitted=false.
func TestNodePayloadsAdmittedAndNot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")
	lgURL(t, db, siteID, "https://example.com/a", 0.8)

	nodes := []string{"https://example.com/a", "https://example.com/ghost"}
	payloads, err := db.NodePayloads(ctx, siteID, nodes)
	if err != nil {
		t.Fatalf("NodePayloads: %v", err)
	}
	a, ok := payloads["https://example.com/a"]
	if !ok || !a.Admitted {
		t.Fatalf("admitted node /a missing or not admitted: %+v", a)
	}
	if a.Importance != 0.8 {
		t.Errorf("/a importance = %v, want 0.8", a.Importance)
	}
	ghost, ok := payloads["https://example.com/ghost"]
	if !ok {
		t.Fatalf("ghost node absent from payloads map")
	}
	if ghost.Admitted {
		t.Errorf("ghost node Admitted = true, want false (never crawled)")
	}
}

// TestSegmentEdgeWeights aggregates inter-segment link weights.
func TestSegmentEdgeWeights(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := lgSite(t, db, "https://example.com/")

	blog := lgURL(t, db, siteID, "https://example.com/blog/post", 0.5)
	money := lgURL(t, db, siteID, "https://example.com/money", 0.9)

	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{Name: "Blog", MatchRule: "/blog"},
		{Name: "Money", MatchRule: "/money"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	if err := db.SetURLSegments(ctx, blog, []int64{ids["Blog"]}); err != nil {
		t.Fatalf("SetURLSegments blog: %v", err)
	}
	if err := db.SetURLSegments(ctx, money, []int64{ids["Money"]}); err != nil {
		t.Fatalf("SetURLSegments money: %v", err)
	}
	if _, err := db.SyncOutEdges(ctx, siteID, blog, time.Now().UTC(),
		[]string{"https://example.com/money"}); err != nil {
		t.Fatalf("sync blog→money: %v", err)
	}

	weights, err := db.SegmentEdgeWeights(ctx, siteID)
	if err != nil {
		t.Fatalf("SegmentEdgeWeights: %v", err)
	}
	if len(weights) != 1 {
		t.Fatalf("weights = %+v, want one Blog→Money", weights)
	}
	if weights[0].From != "Blog" || weights[0].To != "Money" || weights[0].Weight != 1 {
		t.Errorf("weight = %+v, want Blog→Money=1", weights[0])
	}
}
