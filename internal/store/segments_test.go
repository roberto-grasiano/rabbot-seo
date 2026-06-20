package store

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// segTestSite creates a site and returns its id.
func segTestSite(t *testing.T, db *DB, base string) int64 {
	t.Helper()
	id, err := db.AddSite(context.Background(), model.Site{BaseURL: base, Name: base, Enabled: true})
	if err != nil {
		t.Fatalf("AddSite(%q): %v", base, err)
	}
	return id
}

// segTestURL inserts a URL for a site and returns its id.
func segTestURL(t *testing.T, db *DB, siteID int64, url string) int64 {
	t.Helper()
	id, err := db.UpsertURL(context.Background(), model.URL{
		SiteID:      siteID,
		URL:         url,
		FirstSeen:   time.Now().UTC(),
		NextCheckAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", url, err)
	}
	return id
}

// segCountURLSegments returns the number of url_segments rows for a segment id.
func segCountURLSegments(t *testing.T, db *DB, segmentID int64) int {
	t.Helper()
	var n int
	if err := db.Read().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM url_segments WHERE segment_id = ?`, segmentID).Scan(&n); err != nil {
		t.Fatalf("count url_segments(segment=%d): %v", segmentID, err)
	}
	return n
}

// segNamesToIDs returns the (site_id, name) → id map straight from the table.
func segNamesToIDs(t *testing.T, db *DB, siteID int64) map[string]int64 {
	t.Helper()
	rows, err := db.Read().QueryContext(context.Background(),
		`SELECT name, id FROM segments WHERE site_id = ?`, siteID)
	if err != nil {
		t.Fatalf("read segments: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func TestSyncSiteSegmentsInsertsAndReturnsIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := segTestSite(t, db, "https://example.com")

	got, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/blog/"},
		{SiteID: siteID, Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("returned map len = %d, want 2", len(got))
	}
	// The returned map must match the persisted ids exactly.
	persisted := segNamesToIDs(t, db, siteID)
	for name, id := range got {
		if persisted[name] != id {
			t.Errorf("returned id for %q = %d, persisted = %d", name, id, persisted[name])
		}
	}
}

func TestSyncSiteSegmentsUpdatesMatchRuleAndKeepsID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := segTestSite(t, db, "https://example.com")

	first, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/blog/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments first: %v", err)
	}
	origID := first["content"]

	// Re-pattern the same name: id must be stable (upsert, not delete+insert)
	// and match_rule must be the new value.
	second, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/news/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments second: %v", err)
	}
	if second["content"] != origID {
		t.Errorf("id changed on re-pattern: was %d, now %d", origID, second["content"])
	}

	var rule string
	if err := db.Read().QueryRowContext(ctx,
		`SELECT match_rule FROM segments WHERE id = ?`, origID).Scan(&rule); err != nil {
		t.Fatalf("read match_rule: %v", err)
	}
	if rule != "^/news/" {
		t.Errorf("match_rule = %q, want ^/news/", rule)
	}
}

func TestSyncSiteSegmentsDeletesRemovedAndCascadesURLSegments(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := segTestSite(t, db, "https://example.com")
	urlID := segTestURL(t, db, siteID, "https://example.com/blog/x")

	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/blog/"},
		{SiteID: siteID, Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	contentID := ids["content"]

	// Give "content" a membership so we can assert the FK cascade clears it.
	if err := db.SetURLSegments(ctx, urlID, []int64{contentID}); err != nil {
		t.Fatalf("SetURLSegments: %v", err)
	}
	if got := segCountURLSegments(t, db, contentID); got != 1 {
		t.Fatalf("url_segments for content = %d, want 1 (precondition)", got)
	}

	// Remove "content" from config: it must be deleted, and the FK ON DELETE
	// CASCADE must clear its url_segments rows.
	after, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments after removal: %v", err)
	}
	if _, ok := after["content"]; ok {
		t.Errorf("removed segment %q still present in returned map", "content")
	}

	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE id = ?`, contentID).Scan(&n); err != nil {
		t.Fatalf("count content segment: %v", err)
	}
	if n != 0 {
		t.Errorf("removed segment row count = %d, want 0", n)
	}
	if got := segCountURLSegments(t, db, contentID); got != 0 {
		t.Errorf("orphaned url_segments after cascade = %d, want 0", got)
	}
}

func TestSetURLSegmentsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := segTestSite(t, db, "https://example.com")
	urlID := segTestURL(t, db, siteID, "https://example.com/blog/x")
	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/blog/"},
		{SiteID: siteID, Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}

	want := []int64{ids["content"], ids["product"]}
	// Apply twice — second call must not error or duplicate rows.
	for i := 0; i < 2; i++ {
		if err := db.SetURLSegments(ctx, urlID, want); err != nil {
			t.Fatalf("SetURLSegments call %d: %v", i, err)
		}
	}
	got := urlSegmentIDs(t, db, urlID)
	if len(got) != 2 {
		t.Errorf("memberships after double-apply = %v, want 2 rows", got)
	}

	// Rewrite to a smaller set: delete+insert semantics must drop the removed one.
	if err := db.SetURLSegments(ctx, urlID, []int64{ids["product"]}); err != nil {
		t.Fatalf("SetURLSegments shrink: %v", err)
	}
	got = urlSegmentIDs(t, db, urlID)
	if len(got) != 1 || got[0] != ids["product"] {
		t.Errorf("memberships after shrink = %v, want [product]", got)
	}

	// Empty set clears all memberships.
	if err := db.SetURLSegments(ctx, urlID, nil); err != nil {
		t.Fatalf("SetURLSegments clear: %v", err)
	}
	if got := urlSegmentIDs(t, db, urlID); len(got) != 0 {
		t.Errorf("memberships after clear = %v, want none", got)
	}
}

func urlSegmentIDs(t *testing.T, db *DB, urlID int64) []int64 {
	t.Helper()
	rows, err := db.Read().QueryContext(context.Background(),
		`SELECT segment_id FROM url_segments WHERE url_id = ? ORDER BY segment_id`, urlID)
	if err != nil {
		t.Fatalf("read url_segments: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func TestReclassifySiteRewritesMemberships(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteID := segTestSite(t, db, "https://example.com")
	blogURL := segTestURL(t, db, siteID, "https://example.com/blog/post")
	prodURL := segTestURL(t, db, siteID, "https://example.com/product/widget")
	otherURL := segTestURL(t, db, siteID, "https://example.com/about")

	ids, err := db.SyncSiteSegments(ctx, siteID, []model.Segment{
		{SiteID: siteID, Name: "content", MatchRule: "^/blog/"},
		{SiteID: siteID, Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments: %v", err)
	}
	contentID := ids["content"]
	productID := ids["product"]

	// First match: blog→content, product→product, about→none.
	match := func(url string) []int64 {
		switch url {
		case "https://example.com/blog/post":
			return []int64{contentID}
		case "https://example.com/product/widget":
			return []int64{productID}
		default:
			return nil
		}
	}
	if err := db.ReclassifySite(ctx, siteID, match); err != nil {
		t.Fatalf("ReclassifySite: %v", err)
	}
	if got := urlSegmentIDs(t, db, blogURL); len(got) != 1 || got[0] != contentID {
		t.Errorf("blog memberships = %v, want [content]", got)
	}
	if got := urlSegmentIDs(t, db, prodURL); len(got) != 1 || got[0] != productID {
		t.Errorf("product memberships = %v, want [product]", got)
	}
	if got := urlSegmentIDs(t, db, otherURL); len(got) != 0 {
		t.Errorf("about memberships = %v, want none", got)
	}

	// Re-pattern: now blog matches BOTH content and product (multi-segment), and
	// about matches content. Reclassify must rewrite, not accumulate.
	match2 := func(url string) []int64 {
		switch url {
		case "https://example.com/blog/post":
			return []int64{contentID, productID}
		case "https://example.com/about":
			return []int64{contentID}
		default:
			return nil
		}
	}
	if err := db.ReclassifySite(ctx, siteID, match2); err != nil {
		t.Fatalf("ReclassifySite 2: %v", err)
	}
	blogGot := urlSegmentIDs(t, db, blogURL)
	sort.Slice(blogGot, func(i, j int) bool { return blogGot[i] < blogGot[j] })
	wantBlog := []int64{contentID, productID}
	sort.Slice(wantBlog, func(i, j int) bool { return wantBlog[i] < wantBlog[j] })
	if len(blogGot) != 2 || blogGot[0] != wantBlog[0] || blogGot[1] != wantBlog[1] {
		t.Errorf("blog memberships after re-pattern = %v, want %v", blogGot, wantBlog)
	}
	if got := urlSegmentIDs(t, db, prodURL); len(got) != 0 {
		t.Errorf("product URL memberships after re-pattern = %v, want none (rewritten)", got)
	}
	if got := urlSegmentIDs(t, db, otherURL); len(got) != 1 || got[0] != contentID {
		t.Errorf("about memberships after re-pattern = %v, want [content]", got)
	}
}

// TestReclassifySiteScopesToSite asserts the scan only touches the target
// site's URLs (no cross-site membership rewrites).
func TestReclassifySiteScopesToSite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteA := segTestSite(t, db, "https://a.example.com")
	siteB := segTestSite(t, db, "https://b.example.com")
	urlB := segTestURL(t, db, siteB, "https://b.example.com/blog/x")

	idsA, err := db.SyncSiteSegments(ctx, siteA, []model.Segment{
		{SiteID: siteA, Name: "content", MatchRule: "^/blog/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments A: %v", err)
	}
	idsB, err := db.SyncSiteSegments(ctx, siteB, []model.Segment{
		{SiteID: siteB, Name: "content", MatchRule: "^/blog/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments B: %v", err)
	}
	// Pre-set site B's URL membership.
	if err := db.SetURLSegments(ctx, urlB, []int64{idsB["content"]}); err != nil {
		t.Fatalf("SetURLSegments B: %v", err)
	}

	// Reclassify ONLY site A. A match func that would (wrongly) match site B's
	// URL must never be invoked for it — assert by leaving B untouched.
	called := map[string]bool{}
	match := func(url string) []int64 {
		called[url] = true
		return []int64{idsA["content"]}
	}
	if err := db.ReclassifySite(ctx, siteA, match); err != nil {
		t.Fatalf("ReclassifySite A: %v", err)
	}
	if called["https://b.example.com/blog/x"] {
		t.Errorf("ReclassifySite(siteA) invoked match for a siteB URL")
	}
	// Site B's membership is intact.
	if got := urlSegmentIDs(t, db, urlB); len(got) != 1 || got[0] != idsB["content"] {
		t.Errorf("siteB membership disturbed: %v", got)
	}
}

func TestListSegmentsCountsMembers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteA := segTestSite(t, db, "https://a.example.com")
	siteB := segTestSite(t, db, "https://b.example.com")

	idsA, err := db.SyncSiteSegments(ctx, siteA, []model.Segment{
		{SiteID: siteA, Name: "content", MatchRule: "^/blog/"},
		{SiteID: siteA, Name: "product", MatchRule: "^/product/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments A: %v", err)
	}
	_, err = db.SyncSiteSegments(ctx, siteB, []model.Segment{
		{SiteID: siteB, Name: "content", MatchRule: "^/blog/"},
	})
	if err != nil {
		t.Fatalf("SyncSiteSegments B: %v", err)
	}

	// Two URLs in site A → content, one → product, B has none.
	u1 := segTestURL(t, db, siteA, "https://a.example.com/blog/1")
	u2 := segTestURL(t, db, siteA, "https://a.example.com/blog/2")
	u3 := segTestURL(t, db, siteA, "https://a.example.com/product/1")
	for _, u := range []int64{u1, u2} {
		if err := db.SetURLSegments(ctx, u, []int64{idsA["content"]}); err != nil {
			t.Fatalf("SetURLSegments content: %v", err)
		}
	}
	if err := db.SetURLSegments(ctx, u3, []int64{idsA["product"]}); err != nil {
		t.Fatalf("SetURLSegments product: %v", err)
	}

	// Scoped to site A.
	got, err := db.ListSegments(ctx, &siteA)
	if err != nil {
		t.Fatalf("ListSegments(siteA): %v", err)
	}
	byName := map[string]SegmentWithCount{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["content"].MemberCount != 2 {
		t.Errorf("content member count = %d, want 2", byName["content"].MemberCount)
	}
	if byName["product"].MemberCount != 1 {
		t.Errorf("product member count = %d, want 1", byName["product"].MemberCount)
	}
	if byName["content"].MatchRule != "^/blog/" {
		t.Errorf("content match_rule = %q, want ^/blog/", byName["content"].MatchRule)
	}
	if byName["content"].SiteID != siteA {
		t.Errorf("content site_id = %d, want %d", byName["content"].SiteID, siteA)
	}

	// All-sites: both sites' segments appear; a segment with zero members
	// reports count 0 (LEFT JOIN, not INNER).
	all, err := db.ListSegments(ctx, nil)
	if err != nil {
		t.Fatalf("ListSegments(nil): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all-sites segment count = %d, want 3", len(all))
	}
	var zeroSeen bool
	for _, s := range all {
		if s.SiteID == siteB && s.Name == "content" {
			zeroSeen = true
			if s.MemberCount != 0 {
				t.Errorf("siteB content member count = %d, want 0", s.MemberCount)
			}
		}
	}
	if !zeroSeen {
		t.Errorf("siteB content segment missing from all-sites list")
	}
}

// TestSegmentIDByName covers the indexed point lookup (PR #76 review nit):
// hit, miss, and per-site scoping (same name in another site must not match).
func TestSegmentIDByName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	siteA := seedSite(t, db, "https://a.example")
	siteB := seedSite(t, db, "https://b.example")
	idsA, err := db.SyncSiteSegments(ctx, siteA, []model.Segment{{SiteID: siteA, Name: "content", MatchRule: "^/blog/"}})
	if err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if _, err := db.SyncSiteSegments(ctx, siteB, []model.Segment{{SiteID: siteB, Name: "content", MatchRule: "^/news/"}}); err != nil {
		t.Fatalf("sync B: %v", err)
	}
	id, ok, err := db.SegmentIDByName(ctx, siteA, "content")
	if err != nil || !ok || id != idsA["content"] {
		t.Fatalf("hit: got id=%d ok=%v err=%v, want id=%d ok=true", id, ok, err, idsA["content"])
	}
	if _, ok, err := db.SegmentIDByName(ctx, siteA, "missing"); err != nil || ok {
		t.Fatalf("miss: got ok=%v err=%v, want ok=false nil", ok, err)
	}
	idB, ok, err := db.SegmentIDByName(ctx, siteB, "content")
	if err != nil || !ok || idB == idsA["content"] {
		t.Fatalf("site scoping: got id=%d ok=%v err=%v, want different id than site A's", idB, ok, err)
	}
}
