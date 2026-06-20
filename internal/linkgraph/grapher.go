// Package linkgraph is the A9 link-graph LITE service half: it maintains the
// internal-link edge set incrementally on the crawl path, derives the cross-URL
// monitor signals (page_orphaned, inlink_loss, click_depth_regression) from the
// edge delta and the periodic BFS sweep, and serves the bounded get_link_graph
// export. "Ship the questions, not the graph": Rabbot never renders — it answers
// and the agent draws.
//
// It imports store + alerts + model only; the crawl hook seam
// (scheduler.Crawler.Graph) is a structural interface this package's *Grapher
// satisfies, so scheduler never imports linkgraph (and linkgraph never imports
// scheduler). Signals are written through existing primitives — issues via
// store.UpsertIssue / CloseIssue (keyed UNIQUE(url_id, rule_id) so `rabbot
// issues` and the MCP list_issues tool surface them for free), alert events via
// the alerts.Pipeline (the SideTimers robots-bridge pattern).
package linkgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// Rule IDs for the three graph signals. These double as the issues.rule_id (the
// UNIQUE(url_id, rule_id) dedup key) and the alert Event.ChangeType (the incident
// fingerprint component). They are NOT in process.go's severityForField map (that
// map is process-local); the Grapher sets Event.Severity directly.
const (
	RulePageOrphaned         = "page_orphaned"
	RuleInlinkLoss           = "inlink_loss"
	RuleClickDepthRegression = "click_depth_regression"
)

// inlink_loss thresholds (documented constants, criterion 6). A target fires the
// signal at sync time when it both:
//   - had at least InlinkLossFloor inlinks BEFORE the sync (the floor — a page
//     dropping 4→2 is below the floor and never fires, even though it lost 50%),
//     AND
//   - dropped at least InlinkLossFraction of them in this sync (10→4 = 60% loss
//     fires; 10→6 = 40% loss does not).
const (
	InlinkLossFloor    = 5
	InlinkLossFraction = 0.50
)

// DepthRegressionThreshold is how many click-depth levels a page's FINITE depth
// must worsen across one BFS sweep to open click_depth_regression. A NULL prior
// depth (first sweep / newly reachable) never fires (criterion 7).
const DepthRegressionThreshold = 2

// Default export caps. The Grapher carries the config-sourced values; these are
// the fallbacks when a knob is zero/unset so a missing config never disables the
// bound. The HARD ceilings are enforced regardless of config (a hostile or
// fat-fingered config can never request a multi-MB export).
const (
	DefaultExportMaxNodes = 100
	DefaultExportMaxEdges = 300
	HardExportMaxNodes    = 250
	HardExportMaxEdges    = 750
	DefaultMaxOutlinks    = 500
)

// AlertSink is the incident pipeline subset the Grapher writes through (the
// SideTimers.EventIngestor pattern, extended with Resolve for the close arm).
// *alerts.Pipeline satisfies it. A nil sink disables alerting (issues still
// open/close): the severability the scope gate requires.
type AlertSink interface {
	Ingest(ctx context.Context, e alerts.Event) error
	Resolve(ctx context.Context, e alerts.Event) error
}

// Grapher maintains the link graph and derives its signals. Construct via
// NewGrapher; the zero value is not usable (it has no store).
type Grapher struct {
	db   *store.DB
	sink AlertSink        // nil = no alerting (issues still open/close)
	now  func() time.Time // injectable clock; defaults to time.Now().UTC()

	maxOutlinks    int
	exportMaxNodes int
	exportMaxEdges int

	// overviewScanCap hard-bounds the edge scan feeding the no-segments folder
	// fallback (see HardOverviewScanEdges). Defaults to HardOverviewScanEdges; a
	// test may lower it to exercise the truncation path without materializing tens
	// of thousands of rows. It is NOT operator-configurable — a fixed resource
	// bound, like the export hard ceilings.
	overviewScanCap int

	// inlinkBaseline tracks each target's reference inbound-edge count for the
	// inlink_loss signal. A single source page contributes at most one edge to a
	// target, so a per-SyncPage comparison of before-vs-after can only ever see a
	// ±1 swing — never a ≥50% drop. inlink_loss is therefore a CUMULATIVE-erosion
	// signal: the baseline is the target's HIGH-WATER inlink count (raised whenever
	// the live count exceeds it, never lowered by erosion), and the signal fires
	// when the live count falls to ≤ (1-InlinkLossFraction)·baseline while the
	// baseline was ≥ InlinkLossFloor. This is process-local LITE state (a daemon is
	// single-process); the periodic sweep / next crawl cycle re-seeds it, and an
	// open issue already records the finding durably (`rabbot issues`), so a restart
	// only loses the in-flight high-water mark, not the persisted finding.
	//
	// The keyspace is bounded by the number of distinct link targets — an
	// operator-controlled set the same size class as the alerts pipeline's lock map
	// — so it never grows with traffic and needs no eviction. Keyed by
	// (siteID,target).
	mu             sync.Mutex
	inlinkBaseline map[inlinkKey]int
}

// inlinkKey identifies a target for the inlink_loss high-water baseline.
type inlinkKey struct {
	siteID int64
	url    string
}

// Option configures a Grapher (functional-options form).
type Option func(*Grapher)

// WithAlertSink wires the incident pipeline so the graph signals reach Slack /
// the configured notifiers. Nil leaves alerting off (issues still open/close).
func WithAlertSink(sink AlertSink) Option {
	return func(g *Grapher) { g.sink = sink }
}

// WithClock injects a deterministic clock (defaults to time.Now().UTC()).
func WithClock(now func() time.Time) Option {
	return func(g *Grapher) { g.now = now }
}

// WithMaxOutlinks threads graph.max_outlinks_per_page (the out-degree cap). <= 0
// falls back to DefaultMaxOutlinks.
func WithMaxOutlinks(n int) Option {
	return func(g *Grapher) { g.maxOutlinks = n }
}

// WithExportCaps threads graph.export_max_nodes / graph.export_max_edges. Each
// <= 0 falls back to its default; both are further bounded by the hard ceilings
// at export time regardless of the configured value.
func WithExportCaps(maxNodes, maxEdges int) Option {
	return func(g *Grapher) {
		g.exportMaxNodes = maxNodes
		g.exportMaxEdges = maxEdges
	}
}

// NewGrapher builds a Grapher over the store. Options wire the alert sink, clock,
// and config-sourced caps. Caps left unset use their package defaults.
func NewGrapher(db *store.DB, opts ...Option) *Grapher {
	g := &Grapher{
		db:              db,
		maxOutlinks:     DefaultMaxOutlinks,
		exportMaxNodes:  DefaultExportMaxNodes,
		exportMaxEdges:  DefaultExportMaxEdges,
		overviewScanCap: HardOverviewScanEdges,
		inlinkBaseline:  make(map[inlinkKey]int),
	}
	for _, opt := range opts {
		opt(g)
	}
	if g.now == nil {
		g.now = func() time.Time { return time.Now().UTC() }
	}
	if g.maxOutlinks <= 0 {
		g.maxOutlinks = DefaultMaxOutlinks
	}
	if g.exportMaxNodes <= 0 {
		g.exportMaxNodes = DefaultExportMaxNodes
	}
	if g.exportMaxEdges <= 0 {
		g.exportMaxEdges = DefaultExportMaxEdges
	}
	if g.overviewScanCap <= 0 {
		g.overviewScanCap = HardOverviewScanEdges
	}
	return g
}

// SyncPage replaces u's out-edge set to exactly `links` (the deduped, absolute,
// fragment-stripped, same-host slice the extractor produced), then evaluates the
// edge-delta signals on the affected targets. It satisfies the structural
// scheduler.Crawler.Graph interface, so it is invoked on the crawl path after a
// successful extract.
//
// The store does the single-transaction edge replacement (SyncOutEdges); this
// method then derives the per-target signals from the returned EdgeDelta:
//   - a REMOVED edge may leave its target orphaned (1+ -> 0 inlinks) or trip
//     inlink_loss (>= 50% drop from a >= 5 base);
//   - an ADDED edge may re-link a previously-orphaned target (close arm).
//
// Signal evaluation is best-effort-complete: a per-target error is joined and
// surfaced, never allowed to drop the remaining targets or unwind the
// already-persisted edge set (the edge sync is the durable truth; alerting is
// derived). A never-admitted target (no urls row) is skipped for signals (an
// issue is keyed by url_id), though its edge still persists for export.
func (g *Grapher) SyncPage(ctx context.Context, site model.Site, u model.URL, links []string) error {
	now := g.now()
	delta, err := g.db.SyncOutEdgesCapped(ctx, site.ID, u.ID, now, links, g.maxOutlinks)
	if err != nil {
		return fmt.Errorf("sync out-edges (site=%d from=%d): %w", site.ID, u.ID, err)
	}

	var signalErr error
	// REMOVED edges: a target that just lost an inlink may now be orphaned or have
	// crossed the inlink_loss thresholds. We read the post-sync inlink count once
	// per affected target.
	for _, target := range delta.Removed {
		if rerr := g.evaluateRemovedTarget(ctx, site, target, now); rerr != nil {
			signalErr = errors.Join(signalErr, rerr)
		}
	}
	// ADDED edges: a target that just gained an inlink, if it was previously
	// orphaned (an open page_orphaned issue), is relinked — close the issue and
	// resolve the incident (the close arm).
	for _, target := range delta.Added {
		if aerr := g.evaluateAddedTarget(ctx, site, target, now); aerr != nil {
			signalErr = errors.Join(signalErr, aerr)
		}
	}
	if signalErr != nil {
		return fmt.Errorf("graph signals (site=%d): %w", site.ID, signalErr)
	}
	return nil
}

// evaluateRemovedTarget handles the OPEN arm of page_orphaned and inlink_loss for
// a single target whose inbound edge was just removed. `after` is the target's
// post-sync inbound count (exact, ignoring limit).
//
// page_orphaned compares the 1+→0 transition: the pre-sync count is after+1
// (exactly one of THIS page's edges to the target was removed in this sync — the
// delta is per-source and the desired set is deduped, so a source links a target
// at most once). inlink_loss compares the post-sync count against the target's
// HIGH-WATER baseline (see Grapher.inlinkBaseline) because a single source's sync
// can only ever swing one target's count by ±1 — the ≥50% drop is cumulative.
func (g *Grapher) evaluateRemovedTarget(ctx context.Context, site model.Site, target string, now time.Time) error {
	tu, ok, err := g.lookupURL(ctx, site.ID, target)
	if err != nil {
		return err
	}
	if !ok {
		// A never-admitted target cannot carry a url_id-keyed issue; its edge still
		// persists for export. (It also cannot be a monitored "page" that went
		// orphan — orphan inventory is over admitted status_type='page' rows.)
		return nil
	}

	// Post-sync inbound count for the target (exact, ignoring limit).
	_, after, err := g.db.WhatLinksTo(ctx, site.ID, target, 0)
	if err != nil {
		return fmt.Errorf("count inlinks (to=%q): %w", target, err)
	}
	before := after + 1 // this sync removed exactly one of the source's edges to target

	// page_orphaned: the 1+ → 0 transition only. before >= 1 && after == 0. A
	// never-linked page (before would be 0) can never reach this branch from a
	// removal, mirroring the cold-start / first-crawl guard: the signal fires on a
	// page that HAD an inlink and just lost its last one — never on a page that was
	// never linked.
	//
	// COLD-START GATE (#83): during a PARTIAL first crawl, a target's inlinkers may
	// not have been crawled yet, so a removal that drops the live count to 0 is an
	// artifact of incomplete discovery, not a real orphaning. We suppress the eager
	// OPEN until the site is graph-warm (every admitted url fetched at least once).
	// The authoritative periodic sweep (reconcileOrphans) still opens any genuine
	// orphan once warm, so this loses no real signal — only the spurious first-crawl
	// WARNING burst. The CLOSE arm is NEVER gated (evaluateAddedTarget) so a relink
	// still clears a stale issue even mid-cold-window.
	if after == 0 && before >= 1 && tu.StatusType == model.StatusPage && target != site.BaseURL {
		warm, werr := g.db.GraphWarm(ctx, site.ID)
		if werr != nil {
			return fmt.Errorf("graph-warm gate (site=%d): %w", site.ID, werr)
		}
		if warm {
			if oerr := g.openOrphan(ctx, site, tu, now); oerr != nil {
				return oerr
			}
		}
	}

	// inlink_loss: a CUMULATIVE-erosion check against the target's high-water
	// baseline. The baseline is the largest inbound count we have ever observed for
	// this target; it is raised by growth (evaluateAddedTarget) and never lowered by
	// erosion, so a target that climbed to 10 then sheds inlinks one source at a time
	// trips the signal the moment its live count falls to ≤ (1-fraction)·10 = 4
	// (10→4 fires). A target whose high-water was below the floor never fires (4→2
	// stays silent). A 40% drop (10→6) is above the (1-fraction)·baseline=5 cutoff,
	// so it stays silent until it crosses 5.
	baseline := g.observeInlinks(site.ID, target, after)
	if baseline >= InlinkLossFloor {
		cutoff := (1.0 - InlinkLossFraction) * float64(baseline)
		if float64(after) <= cutoff {
			if ierr := g.openInlinkLoss(ctx, site, tu, baseline, after, now); ierr != nil {
				return ierr
			}
		}
	}
	return nil
}

// evaluateAddedTarget handles the CLOSE arm of page_orphaned and raises the
// inlink_loss high-water baseline. A target that just gained an inlink and has an
// OPEN page_orphaned issue is relinked — close the issue and resolve the incident.
// inlink_loss has no per-sync close arm (a recovered inlink count is steady-state,
// not a transition the sync observes); it is a point-in-time warning the operator
// triages. We always update the high-water baseline on growth so the cumulative
// erosion check has a reference.
func (g *Grapher) evaluateAddedTarget(ctx context.Context, site model.Site, target string, now time.Time) error {
	tu, ok, err := g.lookupURL(ctx, site.ID, target)
	if err != nil {
		return err
	}
	if !ok {
		// A never-admitted target still raises its high-water (the edge exists), so a
		// later loss has a reference — but it carries no url_id-keyed issue to close.
		_, after, cerr := g.db.WhatLinksTo(ctx, site.ID, target, 0)
		if cerr != nil {
			return fmt.Errorf("count inlinks (to=%q): %w", target, cerr)
		}
		g.observeInlinks(site.ID, target, after)
		return nil
	}
	// Raise the high-water from the post-sync count.
	if _, after, cerr := g.db.WhatLinksTo(ctx, site.ID, target, 0); cerr == nil {
		g.observeInlinks(site.ID, target, after)
	} else {
		return fmt.Errorf("count inlinks (to=%q): %w", target, cerr)
	}
	return g.closeOrphan(ctx, site, tu, now)
}

// observeInlinks records `count` for (siteID,target) as a high-water observation
// and returns the resulting baseline (the max ever seen). The baseline is raised
// by growth and never lowered by erosion, so the inlink_loss check measures the
// cumulative drop from the target's peak. Concurrency-safe.
func (g *Grapher) observeInlinks(siteID int64, target string, count int) int {
	k := inlinkKey{siteID: siteID, url: target}
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.inlinkBaseline[k]; !ok || count > cur {
		g.inlinkBaseline[k] = count
		return count
	}
	return g.inlinkBaseline[k]
}

// openOrphan opens (idempotently, via the UNIQUE(url_id, rule_id) upsert) the
// page_orphaned issue for tu and ingests one alert event. UpsertIssue dedups, so a
// target that loses its last inlink on successive syncs opens the issue once and
// re-ingest is deduped by the incident pipeline.
func (g *Grapher) openOrphan(ctx context.Context, site model.Site, tu model.URL, now time.Time) error {
	iss := model.Issue{
		URLID:        tu.ID,
		RuleID:       RulePageOrphaned,
		Status:       model.IssueOpen,
		Severity:     model.SeverityWarning,
		ImpactPoints: orphanImpact(tu.Importance),
		OpenedAt:     now,
		LastSeenAt:   now,
		Detail:       mustDetail(map[string]any{"url": tu.URL, "importance": tu.Importance}),
	}
	if _, err := g.db.UpsertIssue(ctx, iss); err != nil {
		return fmt.Errorf("open page_orphaned issue (url=%d): %w", tu.ID, err)
	}
	return g.ingest(ctx, alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        tu.URL,
		URLID:      tu.ID,
		ChangeType: RulePageOrphaned,
		Severity:   model.SeverityWarning,
		After:      "page lost its last internal inlink (orphaned)",
		DeepLink:   tu.URL,
	})
}

// closeOrphan closes the page_orphaned issue for tu and resolves the incident.
// CloseIssue is idempotent (a 0-row update is a no-op success), so closing a page
// that was never orphaned is harmless. We only Resolve when there was an open
// issue to close, so a relink to a never-orphaned page does not dispatch a
// spurious resolve.
func (g *Grapher) closeOrphan(ctx context.Context, site model.Site, tu model.URL, now time.Time) error {
	wasOpen, err := g.hasOpenIssue(ctx, tu.ID, RulePageOrphaned)
	if err != nil {
		return err
	}
	if !wasOpen {
		return nil
	}
	if cerr := g.db.CloseIssue(ctx, tu.ID, RulePageOrphaned, now); cerr != nil {
		return fmt.Errorf("close page_orphaned issue (url=%d): %w", tu.ID, cerr)
	}
	return g.resolve(ctx, alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        tu.URL,
		URLID:      tu.ID,
		ChangeType: RulePageOrphaned,
		Severity:   model.SeverityWarning,
		DeepLink:   tu.URL,
	})
}

// openInlinkLoss opens the inlink_loss issue and ingests one alert event.
func (g *Grapher) openInlinkLoss(ctx context.Context, site model.Site, tu model.URL, before, after int, now time.Time) error {
	iss := model.Issue{
		URLID:        tu.ID,
		RuleID:       RuleInlinkLoss,
		Status:       model.IssueOpen,
		Severity:     model.SeverityWarning,
		ImpactPoints: orphanImpact(tu.Importance),
		OpenedAt:     now,
		LastSeenAt:   now,
		Detail:       mustDetail(map[string]any{"url": tu.URL, "before": before, "after": after}),
	}
	if _, err := g.db.UpsertIssue(ctx, iss); err != nil {
		return fmt.Errorf("open inlink_loss issue (url=%d): %w", tu.ID, err)
	}
	return g.ingest(ctx, alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        tu.URL,
		URLID:      tu.ID,
		ChangeType: RuleInlinkLoss,
		Severity:   model.SeverityWarning,
		Before:     fmt.Sprintf("%d inlinks", before),
		After:      fmt.Sprintf("%d inlinks (lost %d)", after, before-after),
		DeepLink:   tu.URL,
	})
}

// lookupURL resolves an exact-string target URL to its admitted urls row within
// siteID. ok=false (no error) when the target was never admitted (store.ErrNotFound).
func (g *Grapher) lookupURL(ctx context.Context, siteID int64, url string) (model.URL, bool, error) {
	u, err := g.db.GetURL(ctx, siteID, url)
	if errors.Is(err, store.ErrNotFound) {
		return model.URL{}, false, nil
	}
	if err != nil {
		return model.URL{}, false, fmt.Errorf("lookup url (site=%d url=%q): %w", siteID, url, err)
	}
	return u, true, nil
}

// hasOpenIssue reports whether an OPEN issue exists for (urlID, ruleID).
func (g *Grapher) hasOpenIssue(ctx context.Context, urlID int64, ruleID string) (bool, error) {
	open := model.IssueOpen
	issues, err := g.db.ListIssues(ctx, store.IssueFilter{URLID: &urlID, Status: &open})
	if err != nil {
		return false, fmt.Errorf("list open issues (url=%d): %w", urlID, err)
	}
	for _, iss := range issues {
		if iss.RuleID == ruleID {
			return true, nil
		}
	}
	return false, nil
}

// ingest forwards an event to the sink when one is wired (nil = no alerting).
func (g *Grapher) ingest(ctx context.Context, e alerts.Event) error {
	if g.sink == nil {
		return nil
	}
	if err := g.sink.Ingest(ctx, e); err != nil {
		return fmt.Errorf("ingest %s event (url=%q): %w", e.ChangeType, e.URL, err)
	}
	return nil
}

// resolve forwards a recovery to the sink when one is wired (nil = no alerting).
func (g *Grapher) resolve(ctx context.Context, e alerts.Event) error {
	if g.sink == nil {
		return nil
	}
	if err := g.sink.Resolve(ctx, e); err != nil {
		return fmt.Errorf("resolve %s event (url=%q): %w", e.ChangeType, e.URL, err)
	}
	return nil
}

// BlastRadius returns the one-hop inbound blast radius for url within siteID in
// the (inlinks, highImportance, ok) shape the Processor's WithBlastRadius
// enrichment seam consumes. ok=false on a store error (so the enrichment is a
// no-op rather than a panic) or when the url has zero inlinks (nothing to
// enrich). It also backs the CLI / MCP / control surfaces via Card.
func (g *Grapher) BlastRadius(ctx context.Context, siteID int64, url string) (inlinks, highImportance int, ok bool) {
	v, err := g.db.BlastRadius(ctx, siteID, url)
	if err != nil {
		return 0, 0, false
	}
	if v.Inlinks == 0 {
		return 0, 0, false
	}
	return v.Inlinks, v.HighImportance, true
}

// Card is the blast-radius answer card for the CLI / MCP / control surfaces: the
// full inbound blast radius plus the top inbound linkers ranked by source
// importance.
type Card struct {
	URL             string
	Inlinks         int
	HighImportance  int
	WeightedInlinks float64
	Linkers         []store.Linker
}

// BlastRadiusCard answers "how bad is this URL going dark, and who links it?" for
// a single URL: the blast-radius aggregate plus the top `limit` inbound linkers.
func (g *Grapher) BlastRadiusCard(ctx context.Context, siteID int64, url string, limit int) (Card, error) {
	br, err := g.db.BlastRadius(ctx, siteID, url)
	if err != nil {
		return Card{}, err
	}
	linkers, _, err := g.db.WhatLinksTo(ctx, siteID, url, limit)
	if err != nil {
		return Card{}, err
	}
	return Card{
		URL:             url,
		Inlinks:         br.Inlinks,
		HighImportance:  br.HighImportance,
		WeightedInlinks: br.WeightedInlinks,
		Linkers:         linkers,
	}, nil
}

// Orphans returns the orphan inventory for siteID (monitored pages with zero
// inbound edges, root excluded), limited to `limit` (<= 0 = no limit).
func (g *Grapher) Orphans(ctx context.Context, siteID int64, limit int) ([]store.OrphanPage, error) {
	return g.db.OrphanPages(ctx, siteID, limit)
}

// Sweep runs the per-site click-depth BFS sweep (the gocron job's body) and
// derives click_depth_regression from the depth transitions. It writes
// urls.graph_depth back (chunked) via the store, then for each page whose FINITE
// depth worsened by >= DepthRegressionThreshold opens the issue + ingests an
// event; a page whose depth RECOVERED (a previously-open regression now improved)
// closes the issue + resolves. A NULL prior depth (first sweep / newly reachable)
// never fires (criterion 7). The sweep also reconciles orphans authoritatively.
//
// chunk threads the write-back batch size (<= 0 = store default).
func (g *Grapher) Sweep(ctx context.Context, siteID int64, chunk int) error {
	now := g.now()
	site, err := g.db.GetSite(ctx, siteID)
	if err != nil {
		return fmt.Errorf("sweep: get site %d: %w", siteID, err)
	}

	changes, err := g.db.SweepGraphDepths(ctx, siteID, now, chunk)
	if err != nil {
		return fmt.Errorf("sweep: bfs depths (site=%d): %w", siteID, err)
	}

	var signalErr error
	for _, ch := range changes {
		if serr := g.evaluateDepthChange(ctx, site, ch, now); serr != nil {
			signalErr = errors.Join(signalErr, serr)
		}
	}
	if serr := g.reconcileOrphans(ctx, site, now); serr != nil {
		signalErr = errors.Join(signalErr, serr)
	}
	if signalErr != nil {
		return fmt.Errorf("sweep signals (site=%d): %w", siteID, signalErr)
	}
	return nil
}

// evaluateDepthChange opens/closes click_depth_regression for one page based on
// its depth transition this sweep. A NULL prior depth never fires (first sweep /
// newly reachable). A worsening of >= DepthRegressionThreshold opens; a recovery
// (depth improved back below the threshold delta, i.e. the page is no longer
// worse-by->=2 than where it was) closes any open regression.
func (g *Grapher) evaluateDepthChange(ctx context.Context, site model.Site, ch store.DepthChange, now time.Time) error {
	if ch.OldDepth == nil {
		// First sweep / newly reachable: no prior to regress against.
		return nil
	}
	worsenedBy := ch.NewDepth - *ch.OldDepth
	if worsenedBy >= DepthRegressionThreshold {
		return g.openDepthRegression(ctx, site, ch, now)
	}
	// Not a regression this sweep: if a regression was open (e.g. depth recovered
	// 5 -> 2), close it and resolve.
	return g.closeDepthRegression(ctx, site, ch, now)
}

func (g *Grapher) openDepthRegression(ctx context.Context, site model.Site, ch store.DepthChange, now time.Time) error {
	iss := model.Issue{
		URLID:        ch.URLID,
		RuleID:       RuleClickDepthRegression,
		Status:       model.IssueOpen,
		Severity:     model.SeverityWarning,
		ImpactPoints: depthImpact(ch.NewDepth, *ch.OldDepth),
		OpenedAt:     now,
		LastSeenAt:   now,
		Detail:       mustDetail(map[string]any{"url": ch.URL, "old_depth": *ch.OldDepth, "new_depth": ch.NewDepth}),
	}
	if _, err := g.db.UpsertIssue(ctx, iss); err != nil {
		return fmt.Errorf("open click_depth_regression issue (url=%d): %w", ch.URLID, err)
	}
	return g.ingest(ctx, alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        ch.URL,
		URLID:      ch.URLID,
		ChangeType: RuleClickDepthRegression,
		Severity:   model.SeverityWarning,
		Before:     fmt.Sprintf("%d clicks deep", *ch.OldDepth),
		After:      fmt.Sprintf("%d clicks deep (buried %d levels)", ch.NewDepth, ch.NewDepth-*ch.OldDepth),
		DeepLink:   ch.URL,
	})
}

func (g *Grapher) closeDepthRegression(ctx context.Context, site model.Site, ch store.DepthChange, now time.Time) error {
	wasOpen, err := g.hasOpenIssue(ctx, ch.URLID, RuleClickDepthRegression)
	if err != nil {
		return err
	}
	if !wasOpen {
		return nil
	}
	if cerr := g.db.CloseIssue(ctx, ch.URLID, RuleClickDepthRegression, now); cerr != nil {
		return fmt.Errorf("close click_depth_regression issue (url=%d): %w", ch.URLID, cerr)
	}
	return g.resolve(ctx, alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        ch.URL,
		URLID:      ch.URLID,
		ChangeType: RuleClickDepthRegression,
		Severity:   model.SeverityWarning,
		DeepLink:   ch.URL,
	})
}

// reconcileOrphans is the sweep's authoritative orphan reconciliation: it opens
// page_orphaned for every current orphan (UpsertIssue is idempotent) and closes
// any open page_orphaned issue whose page is no longer an orphan. The per-sync
// SyncPage path catches transitions live; this sweep is the periodic backstop
// that corrects drift (e.g. an orphan created by a removal on a page that was
// never re-crawled, or a relink the live path missed because the source page's
// crawl failed).
func (g *Grapher) reconcileOrphans(ctx context.Context, site model.Site, now time.Time) error {
	orphans, err := g.db.OrphanPages(ctx, site.ID, 0)
	if err != nil {
		return fmt.Errorf("reconcile orphans: list (site=%d): %w", site.ID, err)
	}

	// COLD-START GATE (#83): the sweep runs WithStartImmediately, so a freshly
	// (re)started daemon runs this before the first full crawl completes. While the
	// graph is not warm (a url still uncrawled), the orphan inventory is an artifact
	// of incomplete discovery — opening page_orphaned here would resurrect the same
	// spurious first-crawl burst the eager path gates. We still populate
	// currentOrphan (so the CLOSE arm below is correct) and still run the close arm
	// cold; only the OPEN/refresh writes wait for warm.
	warm, werr := g.db.GraphWarm(ctx, site.ID)
	if werr != nil {
		return fmt.Errorf("reconcile orphans: graph-warm gate (site=%d): %w", site.ID, werr)
	}

	currentOrphan := make(map[int64]model.URL, len(orphans))
	var rerr error
	for _, o := range orphans {
		currentOrphan[o.URLID] = model.URL{ID: o.URLID, URL: o.URL, Importance: o.Importance, StatusType: model.StatusPage}
		if !warm {
			// Cold start: record the orphan for the close-arm set, but do not open it.
			continue
		}
		// Open (idempotent) — a still-open orphan refreshes last_seen; a new orphan
		// opens. We do NOT re-ingest here unconditionally: the incident pipeline
		// dedups, but to avoid re-paging on every 6h sweep we open the issue (the
		// queryable record) and only ingest when the issue was not already open.
		alreadyOpen, herr := g.hasOpenIssue(ctx, o.URLID, RulePageOrphaned)
		if herr != nil {
			rerr = errors.Join(rerr, herr)
			continue
		}
		tu := model.URL{ID: o.URLID, URL: o.URL, Importance: o.Importance, StatusType: model.StatusPage}
		if alreadyOpen {
			// Refresh the issue record's last_seen without re-paging.
			if _, uerr := g.db.UpsertIssue(ctx, model.Issue{
				URLID: o.URLID, RuleID: RulePageOrphaned, Status: model.IssueOpen,
				Severity: model.SeverityWarning, ImpactPoints: orphanImpact(o.Importance),
				OpenedAt: now, LastSeenAt: now,
				Detail: mustDetail(map[string]any{"url": o.URL, "importance": o.Importance}),
			}); uerr != nil {
				rerr = errors.Join(rerr, uerr)
			}
			continue
		}
		if oerr := g.openOrphan(ctx, site, tu, now); oerr != nil {
			rerr = errors.Join(rerr, oerr)
		}
	}

	// Close any open page_orphaned issue whose page is no longer an orphan.
	open := model.IssueOpen
	openIssues, err := g.db.ListIssues(ctx, store.IssueFilter{SiteID: &site.ID, Status: &open})
	if err != nil {
		return errors.Join(rerr, fmt.Errorf("reconcile orphans: list open (site=%d): %w", site.ID, err))
	}
	for _, iss := range openIssues {
		if iss.RuleID != RulePageOrphaned {
			continue
		}
		if _, stillOrphan := currentOrphan[iss.URLID]; stillOrphan {
			continue
		}
		// No longer an orphan — close + resolve. We need the page's URL for the
		// resolve event; read it.
		u, uerr := g.urlByID(ctx, iss.URLID)
		if uerr != nil {
			rerr = errors.Join(rerr, uerr)
			continue
		}
		if cerr := g.closeOrphan(ctx, site, u, now); cerr != nil {
			rerr = errors.Join(rerr, cerr)
		}
	}
	return rerr
}

// urlByID reads a single urls row by id (for the reconcile close path, which has
// only the issue's url_id).
func (g *Grapher) urlByID(ctx context.Context, urlID int64) (model.URL, error) {
	var (
		u  model.URL
		st string
	)
	err := g.db.Read().QueryRowContext(ctx,
		`SELECT id, site_id, url, importance, status_type FROM urls WHERE id = ?`, urlID).
		Scan(&u.ID, &u.SiteID, &u.URL, &u.Importance, &st)
	if err != nil {
		return model.URL{}, fmt.Errorf("read url by id (%d): %w", urlID, err)
	}
	u.StatusType = model.StatusType(st)
	return u, nil
}

// orphanImpact scores a page_orphaned / inlink_loss issue by the page's
// importance so the highest-value orphans surface first in `rabbot issues`
// (impact_points DESC). Importance ∈ [0,1]; scaled to a small integer band.
func orphanImpact(importance float64) int {
	pts := int(importance*5) + 1
	if pts < 1 {
		pts = 1
	}
	return pts
}

// depthImpact scores a click_depth_regression by how many levels deeper the page
// was buried (a money page buried 1->10 is worse than 2->4).
func depthImpact(newDepth, oldDepth int) int {
	d := newDepth - oldDepth
	if d < 1 {
		d = 1
	}
	return d
}

// mustDetail marshals an issue detail map to JSON, falling back to "{}" on the
// (unreachable for these value types) marshal error so a detail is never broken.
func mustDetail(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
