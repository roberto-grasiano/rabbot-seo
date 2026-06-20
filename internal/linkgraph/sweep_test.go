package linkgraph

import (
	"context"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// --- 7. click_depth_regression (signal layer) -------------------------------

// TestClickDepthRegressionOpensAndCloses drives the BFS sweep across three states
// and asserts the signal's BOTH ARMS plus the NULL-prior guard (criterion 7,
// signal half):
//   - sweep 1 (first sweep, NULL prior): money is 2 clicks deep → NEVER fires;
//   - sweep 2: money buried to 4 clicks (worsened by 2) → opens + one Ingest;
//   - sweep 3: money restored to 2 clicks → closes + one Resolve.
func TestClickDepthRegressionOpensAndCloses(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	_, a := addURL(t, db, siteID, "https://example.com/a", 0.9)
	_, b := addURL(t, db, siteID, "https://example.com/b", 0.9)
	_, c := addURL(t, db, siteID, "https://example.com/c", 0.9)
	moneyID, money := addURL(t, db, siteID, "https://example.com/money", 0.95)
	_ = money

	sink := &recordSink{}
	clock := &fakeClock{t: time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	// State 1: root → money directly (money at depth 1) and a chain root→a→b→c for
	// later burial. Use SyncPage so edges land via the real path.
	mustSync(t, g, ctx, site, root, "https://example.com/money", "https://example.com/a")
	mustSync(t, g, ctx, site, a, "https://example.com/b")
	mustSync(t, g, ctx, site, b, "https://example.com/c")

	// Sweep 1: first sweep, NULL prior depths → never fires.
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if got := len(sink.ingestsFor(RuleClickDepthRegression)); got != 0 {
		t.Fatalf("first sweep fired click_depth_regression %d times, want 0 (NULL prior)", got)
	}
	if hasOpen(t, db, moneyID, RuleClickDepthRegression) {
		t.Fatalf("first sweep opened click_depth_regression (NULL-prior guard broken)")
	}

	// State 2: bury money under the chain — root no longer links it directly; c→money.
	// money depth goes 1 → 4 (root→a→b→c→money), a +3 worsening (>= 2).
	clock.advance(time.Hour)
	mustSync(t, g, ctx, site, root, "https://example.com/a")  // drop the direct money link
	mustSync(t, g, ctx, site, c, "https://example.com/money") // c now links money
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if !hasOpen(t, db, moneyID, RuleClickDepthRegression) {
		t.Fatalf("sweep 2 did not open click_depth_regression (2→4 worsening)")
	}
	if got := len(sink.ingestsFor(RuleClickDepthRegression)); got != 1 {
		t.Fatalf("click_depth_regression Ingest count = %d, want exactly 1", got)
	}

	// State 3: restore money to depth 2 (root→money2hop). Re-link root→money directly
	// would make it depth 1; to land exactly the recovery arm, link a→money (depth 2).
	clock.advance(time.Hour)
	mustSync(t, g, ctx, site, a, "https://example.com/b", "https://example.com/money") // money now root→a→money = depth 2
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	if hasOpen(t, db, moneyID, RuleClickDepthRegression) {
		t.Fatalf("sweep 3 did not close click_depth_regression (recovery arm broken)")
	}
	if got := len(sink.resolvesFor(RuleClickDepthRegression)); got != 1 {
		t.Fatalf("click_depth_regression Resolve count = %d, want exactly 1", got)
	}
}

// TestSweepReconcilesOrphans asserts the sweep's authoritative orphan
// reconciliation: a page made orphan by a removal on a source page that is never
// re-crawled (so SyncPage never observed the transition live) is still opened by
// the periodic sweep, and one no longer orphan is closed.
func TestSweepReconcilesOrphans(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	orphanID, _ := addURL(t, db, siteID, "https://example.com/orphan", 0.5)

	sink := &recordSink{}
	clock := &fakeClock{t: time.Now().UTC()}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	// root links nothing — /orphan has zero inbound edges from the start (a partial
	// crawl that never linked it). The live SyncPage path never fired (cold start),
	// but the sweep reconciles it authoritatively.
	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere")

	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("sweep did not reconcile /orphan into a page_orphaned issue")
	}

	// Now link it and re-sweep: the sweep closes the no-longer-orphan issue.
	clock.advance(time.Hour)
	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere", "https://example.com/orphan")
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("sweep did not close the re-linked /orphan issue")
	}
}

// TestSweepDoesNotOpenOrphansUntilGraphWarm guards the SECOND cold-start vector
// (#83, LESSON 2): the periodic BFS sweep runs WithStartImmediately, so a freshly
// (re)started daemon runs it before the first full crawl completes. While the graph
// is not warm (a url still uncrawled), reconcileOrphans must NOT open page_orphaned
// — otherwise the spurious first-crawl burst returns through the sweep path. Once
// warm, the SAME sweep opens the genuine orphan (open arm preserved).
func TestSweepDoesNotOpenOrphansUntilGraphWarm(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	// root crawled; orphan admitted but NOT yet crawled → site is NOT warm.
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	orphanID, _ := addUncrawledURL(t, db, siteID, "https://example.com/orphan", 0.5)

	sink := &recordSink{}
	clock := &fakeClock{t: time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	// root links elsewhere → /orphan has zero inbound. A cold sweep must NOT open it.
	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere")
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("cold sweep: %v", err)
	}
	if hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("sweep opened page_orphaned during a PARTIAL crawl (sweep cold-start gate not applied)")
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 0 {
		t.Fatalf("sweep ingested %d page_orphaned cold, want 0 (burst returned through the sweep path)", got)
	}

	// Warm the graph, re-sweep: now the genuine orphan opens (open arm preserved).
	markCrawled(t, db, siteID, "https://example.com/orphan")
	clock.advance(6 * time.Hour)
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("warm sweep: %v", err)
	}
	if !hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("warm sweep did not open the genuine orphan (open arm broken by the gate)")
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("warm sweep ingest count = %d, want exactly 1", got)
	}
}

// TestSweepClosesRelinkedOrphanEvenWhileCold locks the close-arm invariant (LESSON
// 3): the cold-start gate suppresses only the OPEN side of reconcileOrphans. A
// genuine orphan opened while warm, then relinked, must still be CLOSED by a sweep
// even if the site has meanwhile gone cold again (a new uncrawled url admitted).
// This would FAIL if the fix had gated the whole reconcileOrphans cold.
func TestSweepClosesRelinkedOrphanEvenWhileCold(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	orphanID, _ := addURL(t, db, siteID, "https://example.com/orphan", 0.5)

	sink := &recordSink{}
	clock := &fakeClock{t: time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	// Warm: open a genuine orphan via the sweep (root links elsewhere).
	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere")
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("warm sweep open: %v", err)
	}
	if !hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("setup: orphan not opened by the warm sweep")
	}

	// Site goes cold: admit a new uncrawled url, and relink the orphan.
	addUncrawledURL(t, db, siteID, "https://example.com/new", 0.4)
	clock.advance(6 * time.Hour)
	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere", "https://example.com/orphan")
	// (The eager close arm above already closes it; force the issue back open so the
	// SWEEP close path is what's under test.)
	if hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("eager relink did not close the issue (precondition)")
	}
	reopen := model.Issue{
		URLID: orphanID, RuleID: RulePageOrphaned, Status: model.IssueOpen,
		Severity: model.SeverityWarning, ImpactPoints: 1, OpenedAt: clock.now(), LastSeenAt: clock.now(),
		Detail: "{}",
	}
	if _, err := db.UpsertIssue(ctx, reopen); err != nil {
		t.Fatalf("re-open issue for sweep-close test: %v", err)
	}

	// A cold sweep (site not warm) must still CLOSE the no-longer-orphan issue.
	warm, err := db.GraphWarm(ctx, siteID)
	if err != nil {
		t.Fatalf("GraphWarm: %v", err)
	}
	if warm {
		t.Fatalf("precondition: site should be cold")
	}
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("cold sweep close: %v", err)
	}
	if hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("cold sweep did not close a relinked orphan (close arm wrongly gated)")
	}
}

// fakeClock is a controllable monotonic clock for the sweep tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func mustSync(t *testing.T, g *Grapher, ctx context.Context, site model.Site, from model.URL, links ...string) {
	t.Helper()
	if err := g.SyncPage(ctx, site, from, links); err != nil {
		t.Fatalf("SyncPage from %q: %v", from.URL, err)
	}
}
