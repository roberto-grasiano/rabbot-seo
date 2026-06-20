package cli

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// noCapResolver is a test capResolver that reports "unlimited" (cap 0).
func noCapResolver(_ context.Context, _ *store.DB, _ int64) int { return 0 }

// openSeededStore opens a fresh store and seeds one verified site with a homepage
// snapshot and one open issue. Returns the db and the seeded site id.
func openSeededStore(t *testing.T) (*store.DB, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/k.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	siteID, err := db.AddSite(ctx, model.Site{BaseURL: "https://a.test", Name: "A", Enabled: true, MinInterval: 600, MaxInterval: 86400})
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	urlID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://a.test", NextCheckAt: time.Now()})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{URLID: urlID, FetchedAt: now, HTTPStatus: 200, Title: "Home", Indexable: true}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if _, err := db.UpsertIssue(ctx, model.Issue{URLID: urlID, RuleID: "missing-meta", Status: model.IssueOpen, Severity: model.SeverityWarning, ImpactPoints: 5, OpenedAt: now, LastSeenAt: now, Detail: "{}"}); err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}
	return db, siteID
}

func TestListSitesHookResolvesTier(t *testing.T) {
	db, _ := openSeededStore(t)
	hook := listSitesHook(db)
	got, err := hook(context.Background())
	if err != nil {
		t.Fatalf("listSitesHook: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sites, want 1", len(got))
	}
	// Never verified -> throttled (the safe default).
	if got[0].VerificationState != "throttled" {
		t.Fatalf("VerificationState = %q, want throttled", got[0].VerificationState)
	}
	if got[0].URL != "https://a.test" || got[0].Name != "A" {
		t.Fatalf("unexpected summary: %+v", got[0])
	}
}

func TestSiteDetailHookSnapshotAndIssues(t *testing.T) {
	db, siteID := openSeededStore(t)
	hook := siteDetailHook(db, noCapResolver)
	got, found, err := hook(context.Background(), siteID)
	if err != nil {
		t.Fatalf("siteDetailHook: %v", err)
	}
	if !found {
		t.Fatal("found=false for a seeded site")
	}
	if got.OpenIssueCount != 1 {
		t.Fatalf("OpenIssueCount = %d, want 1", got.OpenIssueCount)
	}
	if !got.HasSnapshot || got.Title != "Home" || got.HTTPStatus != 200 {
		t.Fatalf("snapshot fields not surfaced: %+v", got)
	}
	if got.MinInterval != 600 {
		t.Fatalf("MinInterval = %d, want 600", got.MinInterval)
	}
}

func TestSiteDetailHookUnknownID(t *testing.T) {
	db, _ := openSeededStore(t)
	hook := siteDetailHook(db, noCapResolver)
	_, found, err := hook(context.Background(), 99999)
	if err != nil {
		t.Fatalf("siteDetailHook(unknown): unexpected err %v", err)
	}
	if found {
		t.Fatal("found=true for an unknown site id")
	}
}

func TestSiteDetailHookPopulatesCapFields(t *testing.T) {
	db, siteID := openSeededStore(t)
	// capResolver: a finite cap of 1 means the seeded single-URL site is capped.
	hook := siteDetailHook(db, func(_ context.Context, _ *store.DB, _ int64) int { return 1 })
	got, found, err := hook(context.Background(), siteID)
	if err != nil || !found {
		t.Fatalf("hook: found=%v err=%v", found, err)
	}
	if got.MonitoredPages != 1 {
		t.Fatalf("MonitoredPages = %d, want 1", got.MonitoredPages)
	}
	if got.MaxPages != 1 {
		t.Fatalf("MaxPages = %d, want 1", got.MaxPages)
	}
	if !got.Capped {
		t.Fatalf("Capped = false, want true (monitored 1 >= cap 1)")
	}
}

func TestSiteDetailHookUnlimitedCapNotCapped(t *testing.T) {
	db, siteID := openSeededStore(t)
	// cap 0 = unlimited: never capped, MaxPages stays 0.
	hook := siteDetailHook(db, func(_ context.Context, _ *store.DB, _ int64) int { return 0 })
	got, _, err := hook(context.Background(), siteID)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if got.MaxPages != 0 || got.Capped {
		t.Fatalf("unlimited cap should be MaxPages=0 Capped=false, got MaxPages=%d Capped=%v", got.MaxPages, got.Capped)
	}
	if got.MonitoredPages != 1 {
		t.Fatalf("MonitoredPages = %d, want 1", got.MonitoredPages)
	}
}

func TestIssuesHookSeverityFilter(t *testing.T) {
	db, siteID := openSeededStore(t)
	hook := issuesHook(db)

	// warning matches the seeded issue.
	got, err := hook(context.Background(), control.IssueQuery{SiteID: &siteID, Severity: "warning"})
	if err != nil {
		t.Fatalf("issuesHook: %v", err)
	}
	if len(got) != 1 || got[0].RuleID != "missing-meta" || got[0].Severity != "warning" {
		t.Fatalf("warning filter got %+v", got)
	}

	// critical matches nothing.
	none, err := hook(context.Background(), control.IssueQuery{Severity: "critical"})
	if err != nil {
		t.Fatalf("issuesHook(critical): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("critical filter got %d, want 0", len(none))
	}

	// invalid severity -> ErrBadRequest (caller fault).
	if _, err := hook(context.Background(), control.IssueQuery{Severity: "nonsense"}); err == nil {
		t.Fatal("invalid severity: want ErrBadRequest, got nil")
	}
}

func TestHistoryHookResolvesURL(t *testing.T) {
	db, _ := openSeededStore(t)
	// Record a change against the homepage URL.
	ctx := context.Background()
	u, err := db.GetURL(ctx, 0, "https://a.test")
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	// changes.snapshot_id is NOT NULL REFERENCES snapshots(id); use the seeded
	// homepage snapshot id so the FK (PRAGMA foreign_keys = ON) resolves.
	snap, err := db.LatestSnapshot(ctx, u.ID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	now := time.Now().UTC()
	if err := db.WriteTx(ctx, func(tx store.Tx) error {
		_, e := tx.ExecContext(ctx,
			`INSERT INTO changes (url_id, snapshot_id, field, old_value, new_value, change_class, detected_at) VALUES (?,?,?,?,?,?,?)`,
			u.ID, snap.ID, "title", "Old", "Home", string(model.ChangeSubstantive), now)
		return e
	}); err != nil {
		t.Fatalf("insert change: %v", err)
	}

	hook := historyHook(db)
	got, err := hook(ctx, "https://a.test", time.Time{})
	if err != nil {
		t.Fatalf("historyHook: %v", err)
	}
	if got.NotFound {
		t.Fatal("NotFound=true for a known URL")
	}
	if len(got.Changes) != 1 || got.Changes[0].Field != "title" || got.Changes[0].ChangeClass != "substantive" {
		t.Fatalf("unexpected changes: %+v", got.Changes)
	}

	// Unknown URL -> structured not-found, no error.
	nf, err := hook(ctx, "https://nope.test/missing", time.Time{})
	if err != nil {
		t.Fatalf("historyHook(unknown): %v", err)
	}
	if !nf.NotFound {
		t.Fatalf("want NotFound=true for unknown url, got %+v", nf)
	}
}

func TestRichResultsHookValidatesLatestSnapshot(t *testing.T) {
	ctx := context.Background()
	db, siteID := openSeededStore(t)

	// A crawled product page with an eligible Product block.
	prodID, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://a.test/p", NextCheckAt: time.Now()})
	if err != nil {
		t.Fatalf("UpsertURL: %v", err)
	}
	if _, err := db.SaveSnapshot(ctx, model.Snapshot{
		URLID: prodID, FetchedAt: time.Now().UTC(), HTTPStatus: 200,
		JSONLD: `{"@type":"Product","name":"Widget","offers":{"price":"9"}}`,
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	hook := richResultsHook(db)

	got, err := hook(ctx, "https://a.test/p")
	if err != nil {
		t.Fatalf("richResultsHook: %v", err)
	}
	if got.NotFound {
		t.Fatal("NotFound=true for a known crawled URL")
	}
	if !got.HasSnapshot {
		t.Fatal("HasSnapshot=false for a crawled URL")
	}
	if got.Profile != "grr-2026.06" {
		t.Fatalf("profile = %q, want grr-2026.06", got.Profile)
	}
	if len(got.Entities) != 1 || got.Entities[0].Type != "Product" || !got.Entities[0].Eligible {
		t.Fatalf("entities = %+v, want one eligible Product", got.Entities)
	}

	// Unknown URL -> structured not-found, no error (handleHistory pattern).
	nf, err := hook(ctx, "https://nope.test/x")
	if err != nil {
		t.Fatalf("richResultsHook(unknown): %v", err)
	}
	if !nf.NotFound {
		t.Fatalf("want NotFound=true for unknown url, got %+v", nf)
	}

	// Monitored but never crawled -> HasSnapshot=false, NotFound=false, Profile set.
	if _, err := db.UpsertURL(ctx, model.URL{SiteID: siteID, URL: "https://a.test/fresh", NextCheckAt: time.Now()}); err != nil {
		t.Fatalf("UpsertURL fresh: %v", err)
	}
	fresh, err := hook(ctx, "https://a.test/fresh")
	if err != nil {
		t.Fatalf("richResultsHook(fresh): %v", err)
	}
	if fresh.NotFound {
		t.Fatal("NotFound=true for a monitored-but-uncrawled URL; want false")
	}
	if fresh.HasSnapshot {
		t.Fatal("HasSnapshot=true for an uncrawled URL")
	}
	if fresh.Profile != "grr-2026.06" {
		t.Fatalf("profile not set on no-snapshot response: %q", fresh.Profile)
	}
}
