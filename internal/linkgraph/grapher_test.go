package linkgraph

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// --- harness -----------------------------------------------------------------

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func addSite(t *testing.T, db *store.DB, base string) (int64, model.Site) {
	t.Helper()
	s := model.Site{BaseURL: base, Name: "t", Enabled: true}
	id, err := db.AddSite(context.Background(), s)
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	s.ID = id
	return id, s
}

// addURL admits a CRAWLED url (last_checked set), so the site reads graph-warm by
// default. The #83 cold-start gate suppresses the eager page_orphaned open until
// every admitted url has been fetched at least once; the orphan/relink arms these
// tests exercise model the steady state AFTER the first full crawl, so the helper
// marks urls crawled. A test that needs an uncrawled (cold) url uses addUncrawledURL.
func addURL(t *testing.T, db *store.DB, siteID int64, url string, importance float64) (int64, model.URL) {
	t.Helper()
	now := time.Now().UTC()
	u := model.URL{
		SiteID: siteID, URL: url, FirstSeen: now, NextCheckAt: now, LastChecked: &now,
		Importance: importance, StatusType: model.StatusPage,
	}
	id, err := db.UpsertURL(context.Background(), u)
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", url, err)
	}
	u.ID = id
	return id, u
}

// addUncrawledURL admits a url that has NOT yet been fetched (last_checked NULL),
// so the site reads NOT graph-warm — the cold-start condition the #83 gate guards.
func addUncrawledURL(t *testing.T, db *store.DB, siteID int64, url string, importance float64) (int64, model.URL) {
	t.Helper()
	now := time.Now().UTC()
	u := model.URL{
		SiteID: siteID, URL: url, FirstSeen: now, NextCheckAt: now, // LastChecked nil → uncrawled
		Importance: importance, StatusType: model.StatusPage,
	}
	id, err := db.UpsertURL(context.Background(), u)
	if err != nil {
		t.Fatalf("UpsertURL(%q): %v", url, err)
	}
	u.ID = id
	return id, u
}

// markCrawled flips a url's last_checked to non-NULL (a fetch completed), warming
// the graph once every url is crawled. UpsertURL's ON CONFLICT path does NOT touch
// last_checked, so we use the schedule-advance path the crawl uses to mark a fetch
// complete.
func markCrawled(t *testing.T, db *store.DB, siteID int64, url string) {
	t.Helper()
	now := time.Now().UTC()
	u, err := db.GetURL(context.Background(), siteID, url)
	if err != nil {
		t.Fatalf("GetURL(%q): %v", url, err)
	}
	if err := db.UpdateURLSchedule(context.Background(), u.ID, now, 600, model.FetchOK, "", ""); err != nil {
		t.Fatalf("UpdateURLSchedule(%q): %v", url, err)
	}
}

// recordSink records every Ingest / Resolve call so a test can assert exactly one
// fired (or none did). Safe for concurrent use under -race.
type recordSink struct {
	mu       sync.Mutex
	ingested []alerts.Event
	resolved []alerts.Event
}

func (r *recordSink) Ingest(_ context.Context, e alerts.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingested = append(r.ingested, e)
	return nil
}

func (r *recordSink) Resolve(_ context.Context, e alerts.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved = append(r.resolved, e)
	return nil
}

func (r *recordSink) ingestsFor(changeType string) []alerts.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []alerts.Event
	for _, e := range r.ingested {
		if e.ChangeType == changeType {
			out = append(out, e)
		}
	}
	return out
}

func (r *recordSink) resolvesFor(changeType string) []alerts.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []alerts.Event
	for _, e := range r.resolved {
		if e.ChangeType == changeType {
			out = append(out, e)
		}
	}
	return out
}

// hasOpen reports whether (urlID, ruleID) has an open issue in the store.
func hasOpen(t *testing.T, db *store.DB, urlID int64, ruleID string) bool {
	t.Helper()
	open := model.IssueOpen
	issues, err := db.ListIssues(context.Background(), store.IssueFilter{URLID: &urlID, Status: &open})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	for _, iss := range issues {
		if iss.RuleID == ruleID {
			return true
		}
	}
	return false
}

// --- 5. page_orphaned --------------------------------------------------------

// TestPageOrphanedOpensOnceThenRelinkCloses covers the OPEN arm (a target's last
// inlink removed → opened once + one Ingest), the COLD-START guard (a never-linked
// page never fires), and the CLOSE arm (a re-link closes the issue + one Resolve)
// — criterion 5, BOTH ARMS.
func TestPageOrphanedOpensOnceThenRelinkCloses(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	rootID, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	moneyID, _ := addURL(t, db, siteID, "https://example.com/money", 0.9)
	_, _ = addURL(t, db, siteID, "https://example.com/never", 0.5) // never linked → never fires

	sink := &recordSink{}
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return now }))

	// root links money: money has 1 inlink.
	if err := g.SyncPage(ctx, site, root, []string{"https://example.com/money"}); err != nil {
		t.Fatalf("sync root (link money): %v", err)
	}
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("money orphaned while still linked")
	}

	// root drops money: money's last inlink removed → 1→0 transition → orphaned.
	if err := g.SyncPage(ctx, site, root, nil); err != nil {
		t.Fatalf("sync root (drop money): %v", err)
	}
	if !hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("money not orphaned after losing its last inlink (1→0)")
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("page_orphaned Ingest count = %d, want exactly 1", got)
	}

	// The never-linked page never fired (cold start / partial crawl guard).
	for _, e := range sink.ingestsFor(RulePageOrphaned) {
		if e.URL == "https://example.com/never" {
			t.Fatalf("never-linked page fired page_orphaned (cold-start guard broken)")
		}
	}

	// A second sync with money still unlinked must NOT re-open / double-fire (the
	// removal already happened; there is no further 1→0 transition).
	if err := g.SyncPage(ctx, site, root, nil); err != nil {
		t.Fatalf("sync root (idempotent drop): %v", err)
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("page_orphaned re-fired on a no-op re-sync: count = %d, want 1", got)
	}

	// Re-link money: an ADDED edge to an open-page_orphaned target closes the issue
	// + resolves the incident (CLOSE arm).
	if err := g.SyncPage(ctx, site, root, []string{"https://example.com/money"}); err != nil {
		t.Fatalf("sync root (relink money): %v", err)
	}
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("money still orphaned after re-link (close arm broken)")
	}
	if got := len(sink.resolvesFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("page_orphaned Resolve count = %d, want exactly 1", got)
	}
	_ = rootID
}

// TestPageOrphanedNeverFiresOnNeverLinked is the explicit cold-start arm: a page
// that has never had an inlink, when its (only, unrelated) source page syncs a set
// that never included it, never fires page_orphaned.
func TestPageOrphanedNeverFiresOnNeverLinked(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	neverID, _ := addURL(t, db, siteID, "https://example.com/never", 0.5)

	sink := &recordSink{}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return time.Now().UTC() }))

	// root links a DIFFERENT page, then drops it. /never is never in any edge set.
	if err := g.SyncPage(ctx, site, root, []string{"https://example.com/other"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := g.SyncPage(ctx, site, root, nil); err != nil {
		t.Fatalf("sync drop: %v", err)
	}
	if hasOpen(t, db, neverID, RulePageOrphaned) {
		t.Fatalf("never-linked page opened page_orphaned (cold-start guard broken)")
	}
	if len(sink.ingestsFor(RulePageOrphaned)) != 0 {
		t.Fatalf("page_orphaned ingested for a never-linked page")
	}
}

// TestPageOrphanedSuppressedUntilGraphWarm is the #83 cold-start gate (LESSONS
// 2+3). On a PARTIAL first crawl — at least one admitted url not yet fetched
// (last_checked NULL) — a real-looking 1+→0 inlink drop must NOT open
// page_orphaned (the inlinkers simply haven't been crawled yet). Once every url is
// crawled (graph-warm), the SAME drop DOES open it. This is the falsifiable bug
// repro: the pre-fix eager arm fires on the cold drop → the first assertion fails.
func TestPageOrphanedSuppressedUntilGraphWarm(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	// root is crawled; money is admitted but NOT yet crawled → site is NOT warm.
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	moneyID, _ := addUncrawledURL(t, db, siteID, "https://example.com/money", 0.9)

	sink := &recordSink{}
	clock := &fakeClock{t: time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	// Cold start: root links money (1 inlink), then drops it (1→0). Because the graph
	// is not warm, the eager open arm must be SUPPRESSED.
	mustSync(t, g, ctx, site, root, "https://example.com/money")
	mustSync(t, g, ctx, site, root) // drop → would be 1→0
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("page_orphaned opened during a PARTIAL crawl (cold-start gate not applied)")
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 0 {
		t.Fatalf("page_orphaned ingested %d times during cold start, want 0 (spurious WARNING burst)", got)
	}

	// Warm the graph: money is now crawled too → no url has last_checked NULL.
	markCrawled(t, db, siteID, "https://example.com/money")

	// Re-establish the inlink, then drop it again — now AFTER warm the 1→0 transition
	// MUST open page_orphaned (BOTH ARMS: the gate only suppresses cold, never warm).
	mustSync(t, g, ctx, site, root, "https://example.com/money")
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("relink left money orphaned")
	}
	mustSync(t, g, ctx, site, root) // drop again → 1→0, warm
	if !hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("page_orphaned did NOT open on a real 1→0 drop after the graph warmed (open arm broken)")
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("page_orphaned ingest count = %d after warm, want exactly 1", got)
	}

	// CLOSE arm still works after warm: relink closes the issue + one Resolve.
	mustSync(t, g, ctx, site, root, "https://example.com/money")
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("relink after warm did not close page_orphaned (close arm broken)")
	}
	if got := len(sink.resolvesFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("page_orphaned Resolve count = %d, want exactly 1", got)
	}
}

// TestPageOrphanedCloseArmRunsDuringColdStart proves the gate is OPEN-arm-only:
// during a partial crawl a relink still CLOSES an already-open orphan issue (a
// stale issue from a prior warm window must not be stranded). Only the spurious
// OPEN is suppressed cold; the close arm is never gated (LESSON 3).
func TestPageOrphanedCloseArmRunsDuringColdStart(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	moneyID, _ := addURL(t, db, siteID, "https://example.com/money", 0.9)

	sink := &recordSink{}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return time.Now().UTC() }))

	// Warm window: open a genuine orphan issue (root links then drops money).
	mustSync(t, g, ctx, site, root, "https://example.com/money")
	mustSync(t, g, ctx, site, root)
	if !hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("setup: orphan not opened while warm")
	}

	// A NEW page is admitted but not yet crawled → the site goes cold again.
	addUncrawledURL(t, db, siteID, "https://example.com/fresh", 0.4)
	warm, err := db.GraphWarm(ctx, siteID)
	if err != nil {
		t.Fatalf("GraphWarm: %v", err)
	}
	if warm {
		t.Fatalf("setup: site should be cold after admitting an uncrawled url")
	}

	// Relink money during the cold window → the close arm must still fire.
	mustSync(t, g, ctx, site, root, "https://example.com/money")
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("close arm was gated during cold start; a stale orphan issue was stranded")
	}
	if got := len(sink.resolvesFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("page_orphaned Resolve count = %d during cold-start relink, want exactly 1", got)
	}
}

// TestNilSinkNoPanic: with no alert sink wired, signals still open/close issues
// without panicking (severability: alerting off).
func TestNilSinkNoPanic(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	moneyID, _ := addURL(t, db, siteID, "https://example.com/money", 0.9)

	g := NewGrapher(db) // no sink, default clock

	if err := g.SyncPage(ctx, site, root, []string{"https://example.com/money"}); err != nil {
		t.Fatalf("sync link: %v", err)
	}
	if err := g.SyncPage(ctx, site, root, nil); err != nil {
		t.Fatalf("sync drop: %v", err)
	}
	if !hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("orphan issue not opened with nil sink")
	}
}

// --- 6. inlink_loss ----------------------------------------------------------

// TestInlinkLossThresholds: 10→4 fires (floor 5 met, 60% loss); 4→2 does not
// (below floor); 10→6 does not (< 50% loss) — criterion 6.
func TestInlinkLossThresholds(t *testing.T) {
	cases := []struct {
		name      string
		before    int // number of source pages linking the target before
		removeN   int // how many of those sources drop the link in one sync
		wantFires bool
	}{
		{"10 to 4 fires (60% loss, floor met)", 10, 6, true},
		{"4 to 2 below floor (no fire)", 4, 2, false},
		{"10 to 6 only 40% loss (no fire)", 10, 4, false},
		{"5 to 2 floor met 60% loss fires", 5, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openDB(t)
			ctx := context.Background()
			siteID, site := addSite(t, db, "https://example.com/")
			target := "https://example.com/target"
			targetID, _ := addURL(t, db, siteID, target, 0.8)

			sink := &recordSink{}
			now := time.Now().UTC()
			g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return now }))

			// Build `before` source pages, each linking the target.
			sources := make([]model.URL, tc.before)
			for i := 0; i < tc.before; i++ {
				u := "https://example.com/src" + itoa(i)
				_, su := addURL(t, db, siteID, u, 0.5)
				sources[i] = su
				if err := g.SyncPage(ctx, site, su, []string{target}); err != nil {
					t.Fatalf("sync src %d: %v", i, err)
				}
			}
			// No inlink_loss should have fired during build-up (only growth).
			if len(sink.ingestsFor(RuleInlinkLoss)) != 0 {
				t.Fatalf("inlink_loss fired during inlink GROWTH")
			}

			// Drop the link from `removeN` sources. The LAST drop crosses the
			// threshold check against the count BEFORE that drop. To exercise the
			// documented before/after thresholds, drop all but the final one first
			// (no fire expected until the final crossing), then the final source's
			// drop is the decisive sync. Simpler + faithful: drop removeN sources one
			// at a time; assert the firing only by the FINAL state.
			for i := 0; i < tc.removeN; i++ {
				if err := g.SyncPage(ctx, site, sources[i], nil); err != nil {
					t.Fatalf("drop src %d: %v", i, err)
				}
			}

			fired := len(sink.ingestsFor(RuleInlinkLoss)) > 0
			openIssue := hasOpen(t, db, targetID, RuleInlinkLoss)
			if tc.wantFires && (!fired || !openIssue) {
				t.Fatalf("inlink_loss did not fire (ingested=%v open=%v), want fire", fired, openIssue)
			}
			if !tc.wantFires && (fired || openIssue) {
				t.Fatalf("inlink_loss fired (ingested=%v open=%v), want NO fire", fired, openIssue)
			}
		})
	}
}

// itoa is a tiny non-allocating-on-the-hot-path int formatter for test URLs.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
