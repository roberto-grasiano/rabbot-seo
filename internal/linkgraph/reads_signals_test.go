package linkgraph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// errSink fails every Ingest/Resolve with a sentinel so the open/close arms'
// alert-dispatch error paths (grapher.ingest / grapher.resolve, currently
// partially covered) are exercised and surfaced — never swallowed.
type errSink struct{ err error }

func (s errSink) Ingest(context.Context, alerts.Event) error  { return s.err }
func (s errSink) Resolve(context.Context, alerts.Event) error { return s.err }

// --- BlastRadius (public read, was 0%) --------------------------------------

// TestBlastRadiusReturnsCountsAndHighImportance asserts the (inlinks,
// highImportance, ok) shape the Processor enrichment seam consumes: a target with
// inbound links reports the exact inbound count and how many sources clear the
// high-importance cutoff (>= 0.70).
func TestBlastRadiusReturnsCountsAndHighImportance(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	target := "https://example.com/target"
	addURL(t, db, siteID, target, 0.5)

	// Two high-importance sources (0.9, 0.7) and one low (0.2) link the target.
	_, hi1 := addURL(t, db, siteID, "https://example.com/hi1", 0.9)
	_, hi2 := addURL(t, db, siteID, "https://example.com/hi2", 0.7) // exactly at the 0.70 cutoff → counts
	_, lo := addURL(t, db, siteID, "https://example.com/lo", 0.2)

	g := NewGrapher(db)
	mustSync(t, g, ctx, site, hi1, target)
	mustSync(t, g, ctx, site, hi2, target)
	mustSync(t, g, ctx, site, lo, target)

	inlinks, high, ok := g.BlastRadius(ctx, siteID, target)
	if !ok {
		t.Fatalf("BlastRadius ok = false, want true for a linked target")
	}
	if inlinks != 3 {
		t.Fatalf("inlinks = %d, want 3", inlinks)
	}
	if high != 2 {
		t.Fatalf("highImportance = %d, want 2 (0.9 and 0.7 clear the 0.70 cutoff; 0.2 does not)", high)
	}
}

// TestBlastRadiusZeroInlinksNotOK asserts the documented contract: a URL with
// zero inlinks returns ok=false (nothing to enrich), distinct from a store error
// but the same no-op outcome for the caller.
func TestBlastRadiusZeroInlinksNotOK(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	addURL(t, db, siteID, "https://example.com/lonely", 0.5)

	inlinks, high, ok := g0(db).BlastRadius(ctx, siteID, "https://example.com/lonely")
	if ok {
		t.Fatalf("BlastRadius ok = true for a zero-inlink URL, want false")
	}
	if inlinks != 0 || high != 0 {
		t.Fatalf("zero-inlink BlastRadius = (%d,%d), want (0,0)", inlinks, high)
	}
}

// TestBlastRadiusStoreErrorNotOK asserts the store-error arm returns ok=false (a
// no-op enrichment, never a panic) — exercised by closing the DB so the read fails.
func TestBlastRadiusStoreErrorNotOK(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	addURL(t, db, siteID, "https://example.com/x", 0.5)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	inlinks, high, ok := g0(db).BlastRadius(ctx, siteID, "https://example.com/x")
	if ok || inlinks != 0 || high != 0 {
		t.Fatalf("BlastRadius on a closed DB = (%d,%d,%v), want (0,0,false)", inlinks, high, ok)
	}
}

// --- BlastRadiusCard (public read, was 0%) ----------------------------------

// TestBlastRadiusCardRanksLinkers asserts the answer card carries the aggregate
// (inlinks, high-importance, weighted mass) AND the top inbound linkers ranked by
// source importance DESC, bounded by limit.
func TestBlastRadiusCardRanksLinkers(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	target := "https://example.com/target"
	addURL(t, db, siteID, target, 0.5)

	_, hi := addURL(t, db, siteID, "https://example.com/hi", 0.9)
	_, mid := addURL(t, db, siteID, "https://example.com/mid", 0.6)
	_, lo := addURL(t, db, siteID, "https://example.com/lo", 0.1)

	g := NewGrapher(db)
	mustSync(t, g, ctx, site, hi, target)
	mustSync(t, g, ctx, site, mid, target)
	mustSync(t, g, ctx, site, lo, target)

	card, err := g.BlastRadiusCard(ctx, siteID, target, 2)
	if err != nil {
		t.Fatalf("BlastRadiusCard: %v", err)
	}
	if card.URL != target {
		t.Fatalf("card.URL = %q, want %q", card.URL, target)
	}
	if card.Inlinks != 3 {
		t.Fatalf("card.Inlinks = %d, want 3 (aggregate ignores the linker limit)", card.Inlinks)
	}
	if card.HighImportance != 1 {
		t.Fatalf("card.HighImportance = %d, want 1 (only 0.9 clears 0.70)", card.HighImportance)
	}
	// limit=2 → only the two highest-importance linkers, in DESC order.
	if len(card.Linkers) != 2 {
		t.Fatalf("card.Linkers = %d, want 2 (limit honored)", len(card.Linkers))
	}
	if card.Linkers[0].URL != "https://example.com/hi" || card.Linkers[1].URL != "https://example.com/mid" {
		t.Fatalf("linkers not ranked by importance DESC: %+v", card.Linkers)
	}
	// Weighted mass = Σ(0.5 + 0.5·importance) = (0.5+0.45)+(0.5+0.3)+(0.5+0.05) = 2.3.
	if card.WeightedInlinks < 2.29 || card.WeightedInlinks > 2.31 {
		t.Fatalf("card.WeightedInlinks = %v, want ~2.3", card.WeightedInlinks)
	}
}

// TestBlastRadiusCardStoreError surfaces the store error (not a zero card) on a
// closed DB — the CLI/MCP surface must distinguish "no data" from "read failed".
func TestBlastRadiusCardStoreError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	addURL(t, db, siteID, "https://example.com/x", 0.5)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, err := g0(db).BlastRadiusCard(ctx, siteID, "https://example.com/x", 5); err == nil {
		t.Fatalf("BlastRadiusCard on a closed DB returned nil error, want a surfaced read error")
	}
}

// --- Orphans (public read, was 0%) ------------------------------------------

// TestOrphansInventory asserts the orphan inventory: monitored pages with zero
// inbound edges, root excluded, importance DESC, limit honored.
func TestOrphansInventory(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")

	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	addURL(t, db, siteID, "https://example.com/orphan-hi", 0.9)
	addURL(t, db, siteID, "https://example.com/orphan-lo", 0.3)
	addURL(t, db, siteID, "https://example.com/linked", 0.8)

	g := NewGrapher(db)
	// root links only /linked → the two orphans + root stay un-inbound; root is excluded.
	mustSync(t, g, ctx, site, root, "https://example.com/linked")

	all, err := g.Orphans(ctx, siteID, 0) // no limit
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	// /orphan-hi, /orphan-lo are orphans; /linked is not; root is excluded.
	if len(all) != 2 {
		t.Fatalf("orphans = %d (%+v), want 2", len(all), all)
	}
	if all[0].URL != "https://example.com/orphan-hi" {
		t.Fatalf("orphans not ordered importance DESC: first = %q, want orphan-hi", all[0].URL)
	}
	for _, o := range all {
		if o.URL == "https://example.com/" {
			t.Fatalf("site root reported as an orphan")
		}
		if o.URL == "https://example.com/linked" {
			t.Fatalf("a linked page reported as an orphan")
		}
	}

	// limit=1 → only the highest-importance orphan.
	one, err := g.Orphans(ctx, siteID, 1)
	if err != nil {
		t.Fatalf("Orphans(limit=1): %v", err)
	}
	if len(one) != 1 || one[0].URL != "https://example.com/orphan-hi" {
		t.Fatalf("Orphans(limit=1) = %+v, want exactly [orphan-hi]", one)
	}
}

// --- ingest / resolve error arms --------------------------------------------

// TestOpenArmSurfacesIngestError: when the wired sink fails, the open arm's error
// is wrapped and surfaced by SyncPage (never swallowed), while the issue is still
// persisted (the edge sync is the durable truth).
func TestOpenArmSurfacesIngestError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	moneyID, _ := addURL(t, db, siteID, "https://example.com/money", 0.9)

	boom := errors.New("sink down")
	g := NewGrapher(db, WithAlertSink(errSink{err: boom}),
		WithClock(func() time.Time { return time.Now().UTC() }))

	mustSync(t, g, ctx, site, root, "https://example.com/money")
	// Drop money → 1→0 orphan transition → openOrphan → ingest fails.
	err := g.SyncPage(ctx, site, root, nil)
	if err == nil {
		t.Fatalf("SyncPage swallowed the sink ingest error, want it surfaced")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("surfaced error = %v, want it to wrap the sink sentinel", err)
	}
	// The issue is still persisted despite the alert failure.
	if !hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("orphan issue not persisted when the alert sink failed (durable-truth violated)")
	}
}

// TestCloseArmSurfacesResolveError: a relink that closes an open orphan issue and
// hits a failing sink surfaces the resolve error while the issue is still closed.
func TestCloseArmSurfacesResolveError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	moneyID, _ := addURL(t, db, siteID, "https://example.com/money", 0.9)

	boom := errors.New("sink down")
	// Open the issue with a working sink first.
	ok := &recordSink{}
	g := NewGrapher(db, WithAlertSink(ok), WithClock(func() time.Time { return time.Now().UTC() }))
	mustSync(t, g, ctx, site, root, "https://example.com/money")
	mustSync(t, g, ctx, site, root) // drop → orphan opens
	if !hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("setup: orphan not opened")
	}

	// Now swap in a failing sink and relink → close arm fires Resolve → fails.
	g2 := NewGrapher(db, WithAlertSink(errSink{err: boom}),
		WithClock(func() time.Time { return time.Now().UTC() }))
	err := g2.SyncPage(ctx, site, root, []string{"https://example.com/money"})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("close-arm resolve error = %v, want it to wrap the sink sentinel", err)
	}
	// The issue is still closed (CloseIssue ran before the Resolve dispatch failed).
	if hasOpen(t, db, moneyID, RulePageOrphaned) {
		t.Fatalf("issue still open after a relink that failed only at Resolve dispatch")
	}
}

// --- closeOrphan / closeDepthRegression no-op (the !wasOpen early return) -----

// TestRelinkNeverOrphanedFiresNoResolve: adding an inlink to a page that was never
// orphaned (no open page_orphaned issue) must NOT dispatch a spurious Resolve —
// the !wasOpen guard in closeOrphan.
func TestRelinkNeverOrphanedFiresNoResolve(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	addURL(t, db, siteID, "https://example.com/fresh", 0.5)

	sink := &recordSink{}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return time.Now().UTC() }))

	// First-ever link to /fresh (an ADDED edge, but the target was never orphaned).
	mustSync(t, g, ctx, site, root, "https://example.com/fresh")

	if got := len(sink.resolvesFor(RulePageOrphaned)); got != 0 {
		t.Fatalf("page_orphaned Resolve fired %d times on a never-orphaned relink, want 0", got)
	}
}

// --- reconcileOrphans: already-open refresh + reconcile-driven close ----------

// TestReconcileAlreadyOpenDoesNotRePage covers the reconcile's already-open arm:
// a still-orphan page on a SECOND sweep refreshes its issue's last_seen WITHOUT
// re-ingesting an alert (no re-paging every 6h sweep).
func TestReconcileAlreadyOpenDoesNotRePage(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	addURL(t, db, siteID, "https://example.com/orphan", 0.5)

	sink := &recordSink{}
	clock := &fakeClock{t: time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	// root links elsewhere → /orphan has zero inbound; the live path never saw a
	// transition, so the FIRST sweep opens it (one ingest).
	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere")
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("first sweep ingested %d page_orphaned, want exactly 1", got)
	}

	// SECOND sweep, /orphan still orphaned → already-open arm: refresh, NO re-ingest.
	clock.advance(6 * time.Hour)
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("second sweep re-paged: ingests = %d, want still 1 (already-open refresh, no re-ingest)", got)
	}
}

// TestReconcileClosesRelinkedOrphan covers the reconcile CLOSE path (and urlByID,
// was 0%): an orphan issue opened by a sweep, then relinked by an edge inserted
// directly through the store (so the live SyncPage close arm never ran), is closed
// by the NEXT sweep's authoritative reconcile + one Resolve.
func TestReconcileClosesRelinkedOrphan(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	rootID, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	orphanID, _ := addURL(t, db, siteID, "https://example.com/orphan", 0.5)

	sink := &recordSink{}
	clock := &fakeClock{t: time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(clock.now))

	mustSync(t, g, ctx, site, root, "https://example.com/elsewhere")
	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if !hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("setup: orphan not opened by the first sweep")
	}

	// Relink /orphan by writing the edge DIRECTLY through the store (root → orphan),
	// bypassing SyncPage's live close arm. The issue stays OPEN until the sweep
	// reconciles it — this is exactly the "live path missed it" drift the reconcile
	// backstop exists for, and the only path that exercises urlByID.
	clock.advance(6 * time.Hour)
	if _, err := db.SyncOutEdges(ctx, siteID, rootID, clock.now(),
		[]string{"https://example.com/elsewhere", "https://example.com/orphan"}); err != nil {
		t.Fatalf("direct edge insert: %v", err)
	}
	if !hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("issue closed by the direct edge insert; expected it to stay open until the sweep")
	}

	if err := g.Sweep(ctx, siteID, 0); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if hasOpen(t, db, orphanID, RulePageOrphaned) {
		t.Fatalf("reconcile did not close the relinked orphan (close path / urlByID not exercised)")
	}
	if got := len(sink.resolvesFor(RulePageOrphaned)); got != 1 {
		t.Fatalf("reconcile-driven Resolve count = %d, want exactly 1", got)
	}
	_ = rootID
}

// --- evaluateRemovedTarget edge branches ------------------------------------

// TestRemovedNeverAdmittedTargetSkipsSignals: a removed edge to a target that was
// never admitted (no urls row) persists the edge but opens NO issue and fires NO
// alert — the !ok skip in evaluateRemovedTarget. (We add the source as a page so
// the sync runs, but the target is never UpsertURL'd.)
func TestRemovedNeverAdmittedTargetSkipsSignals(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)

	sink := &recordSink{}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return time.Now().UTC() }))

	// Link, then drop, a NEVER-admitted target. The edge exists, but no urls row →
	// no url_id-keyed issue can open.
	const phantom = "https://example.com/phantom"
	mustSync(t, g, ctx, site, root, phantom)
	mustSync(t, g, ctx, site, root) // drop phantom

	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 0 {
		t.Fatalf("page_orphaned fired for a never-admitted target, want 0 (got %d)", got)
	}
	if got := len(sink.ingestsFor(RuleInlinkLoss)); got != 0 {
		t.Fatalf("inlink_loss fired for a never-admitted target, want 0 (got %d)", got)
	}
}

// TestAddedNeverAdmittedTargetNoIssueNoAlert: an ADDED edge to a never-admitted
// target (the evaluateAddedTarget !ok branch — it still raises the high-water
// baseline so a later loss has a reference, but carries no url_id-keyed issue to
// close) opens no issue and fires no alert.
func TestAddedNeverAdmittedTargetNoIssueNoAlert(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)

	sink := &recordSink{}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return time.Now().UTC() }))

	// First-ever link to a NEVER-admitted target → the added-target !ok branch.
	mustSync(t, g, ctx, site, root, "https://example.com/phantom")

	if len(sink.ingested) != 0 {
		t.Fatalf("an alert fired for an added edge to a never-admitted target: %+v", sink.ingested)
	}
	if len(sink.resolved) != 0 {
		t.Fatalf("a resolve fired for an added edge to a never-admitted target: %+v", sink.resolved)
	}
}

// TestRootIsNeverOrphaned: the site root (base_url), even when it loses its last
// inbound edge, never opens page_orphaned — the target != site.BaseURL guard.
func TestRootIsNeverOrphaned(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	rootID, root := addURL(t, db, siteID, "https://example.com/", 1.0)
	_, leaf := addURL(t, db, siteID, "https://example.com/leaf", 0.5)

	sink := &recordSink{}
	g := NewGrapher(db, WithAlertSink(sink), WithClock(func() time.Time { return time.Now().UTC() }))

	// leaf links the root (root gains an inbound edge), then drops it (root's last
	// inbound edge removed → 1→0). The root must NOT orphan.
	mustSync(t, g, ctx, site, leaf, "https://example.com/")
	mustSync(t, g, ctx, site, leaf) // drop the link to root

	if hasOpen(t, db, rootID, RulePageOrphaned) {
		t.Fatalf("site root opened page_orphaned; the root-exclusion guard is broken")
	}
	if got := len(sink.ingestsFor(RulePageOrphaned)); got != 0 {
		t.Fatalf("page_orphaned fired for the site root, want 0 (got %d)", got)
	}
	_ = root
}

// --- Export dispatch branches -----------------------------------------------

// TestExportDefaultModeOverviewWhenNoFocus: an empty Mode with no Focus defaults
// to overview (the mode-inference branch).
func TestExportDefaultModeOverviewWhenNoFocus(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	g := NewGrapher(db)

	exp, err := g.Export(ctx, Query{SiteID: siteID}) // no Mode, no Focus
	if err != nil {
		t.Fatalf("Export default mode: %v", err)
	}
	if exp.Mode != ModeOverview {
		t.Fatalf("inferred mode = %q, want overview when Focus is empty", exp.Mode)
	}
}

// TestExportDefaultModeFocusWhenFocusSet: an empty Mode WITH a Focus defaults to
// focus mode (the other inference branch).
func TestExportDefaultModeFocusWhenFocusSet(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	addURL(t, db, siteID, "https://example.com/", 1.0)
	g := NewGrapher(db)

	exp, err := g.Export(ctx, Query{SiteID: siteID, Focus: "https://example.com/"}) // Mode inferred → focus
	if err != nil {
		t.Fatalf("Export inferred focus: %v", err)
	}
	if exp.Mode != ModeFocus {
		t.Fatalf("inferred mode = %q, want focus when Focus is set", exp.Mode)
	}
}

// TestExportNegativeHopsRejected: a negative Hops is rejected clearly (the Hops<0
// guard, the sibling of the Hops>2 reject).
func TestExportNegativeHopsRejected(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	g := NewGrapher(db)

	if _, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeFocus, Focus: "https://example.com/", Hops: -1}); err == nil {
		t.Fatalf("hops=-1 accepted, want a clear rejection")
	}
}

// TestExportFocusRequiresURL: focus mode with an empty Focus is rejected.
func TestExportFocusRequiresURL(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	g := NewGrapher(db)

	if _, err := g.Export(ctx, Query{SiteID: siteID, Mode: ModeFocus, Focus: ""}); err == nil {
		t.Fatalf("focus mode with empty focus accepted, want rejection")
	}
}

// TestExportUnknownModeRejected: an unrecognized Mode is rejected (the default
// switch arm).
func TestExportUnknownModeRejected(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, _ := addSite(t, db, "https://example.com/")
	g := NewGrapher(db)

	if _, err := g.Export(ctx, Query{SiteID: siteID, Mode: ExportMode("galaxy")}); err == nil {
		t.Fatalf("unknown mode accepted, want rejection")
	}
}

// TestExportFocusLimitOverrideDownward: a Limit below the node cap shrinks the
// node set (the downward-override branch in exportFocus), while never raising it.
func TestExportFocusLimitOverrideDownward(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	siteID, site := addSite(t, db, "https://example.com/")
	_, root := addURL(t, db, siteID, "https://example.com/", 1.0)

	links := make([]string, 20)
	for i := 0; i < 20; i++ {
		u := "https://example.com/p" + itoa(i)
		addURL(t, db, siteID, u, 0.5)
		links[i] = u
	}
	g := NewGrapher(db)
	mustSync(t, g, ctx, site, root, links...)

	exp, err := g.Export(ctx, Query{
		SiteID: siteID, Mode: ModeFocus, Focus: "https://example.com/", Hops: 1, Limit: 5,
	})
	if err != nil {
		t.Fatalf("Export focus (limit=5): %v", err)
	}
	if len(exp.Nodes) > 5 {
		t.Fatalf("nodes = %d, want <= 5 (downward limit override ignored)", len(exp.Nodes))
	}
	// The neighborhood is bigger than the limit → Truncated, with the true total
	// reported above the rendered set.
	if !exp.Truncated {
		t.Fatalf("Truncated = false despite a node Limit below the neighborhood size")
	}
	if exp.TotalNodes <= len(exp.Nodes) {
		t.Fatalf("TotalNodes = %d, want > rendered %d", exp.TotalNodes, len(exp.Nodes))
	}
}

// g0 is a no-frills Grapher for the read-method tests (no sink, default clock).
func g0(db *store.DB) *Grapher { return NewGrapher(db) }

// _ keeps model imported even if a future edit drops its only direct use.
var _ = model.StatusPage
