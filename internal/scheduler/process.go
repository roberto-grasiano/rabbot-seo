package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/robotsmeta"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
)

// NewFinding identifies a rule finding the engine NEWLY opened this crawl (it was
// not already open on the prior crawl). ProcessFetch bridges these into the alert
// path (Feature A) so a finding that has no corresponding change-stream event —
// broken_links_spike (on the skipped internal_link_count field) or any rule that
// fires on a page broken on its FIRST crawl (no diff to ingest) — still reaches
// Slack. Field is the diff-field/change_type the finding maps to (so the bridge
// shares a change_type namespace with the change-stream loop and dedups cleanly).
type NewFinding struct {
	Field    string
	Severity model.Severity
	// Detail is the rule finding's Detail JSON (e.g. {"measured_px":906,
	// "budget_px":580,"chars":48} for a SERP-fit overflow, or {"old":N,"new":M} for
	// a count transition). It is passed through to the bridged alert's Event.After so
	// the Slack body can show the numbers behind the finding. Empty for findings that
	// carry no detail; the bridge then emits an event with an empty After, unchanged.
	Detail string
}

// overflowSourceField maps an A3 SERP-fit overflow rule's bridged change_type to
// the snapshot diff field whose value it measures. ProcessFetch's anti-stampede
// push gate consults it: a newly-opened overflow finding bridges to Slack ONLY when
// its source field actually changed THIS crawl. Without the gate, the first recheck
// after upgrading Rabbot would newly-open issues for every pre-existing long title
// and page the whole fleet at once (firstCrawl only covers brand-new URLs). Steady-
// state overflowers still open issues silently (visible on every pull/agent surface);
// they page only when an edit causes — or re-causes — the overflow. A finding whose
// field is NOT in this map (every non-overflow rule) is never gated by it.
var overflowSourceField = map[string]string{
	"title_pixel_overflow":            "title",
	"meta_description_pixel_overflow": "meta_description",
}

// ProcessorDeps is the narrow surface the M2 post-fetch processor needs. The new
// snapshot is persisted before ProcessFetch runs, so the prior snapshot is passed
// in explicitly (oldSnap) rather than re-read here — see the seam contract.
//
// ApplyRules returns the set of findings the rules engine NEWLY opened this crawl
// (NewFinding) so ProcessFetch can bridge them into the alert path; an already-open
// (refreshed) finding is NOT returned (it has already alerted on the crawl it opened).
// truncated is forwarded to the rules engine (rules.EvalContext.Truncated) so rules
// that read JSON-LD (the A4 rich-result family) self-suppress when the fetcher cut
// the body and may have severed a structured-data <script> mid-block.
type ProcessorDeps interface {
	RecordChanges(ctx context.Context, changes []model.Change) error
	ApplyRules(ctx context.Context, urlID int64, importance float64, newSnap, oldSnap model.Snapshot, changes []model.Change, truncated bool) ([]NewFinding, error)
	HandleFetchClass(ctx context.Context, ac alerts.AccessContext, seo []alerts.Event) (bool, error)
	IngestEvent(ctx context.Context, e alerts.Event) error
	ResolveEvent(ctx context.Context, e alerts.Event) error
	// RecordHealthScore (A6) recomputes and, when the score moved, persists the
	// health score for the URL's site and the segments containing the URL. It is
	// called once at the end of a successful ProcessFetch rules pass (after
	// ApplyRules succeeds), so a re-scored event reflects the issue state the pass
	// just settled. It is NOT called when ApplyRules errors (the issue state is
	// half-applied) nor on a non-ok / 304 fetch (no rules ran).
	RecordHealthScore(ctx context.Context, siteID, urlID int64) error
}

// Processor runs the M2 stage after a fetch: access gate -> diff -> rules -> alerts.
type Processor struct {
	deps             ProcessorDeps
	simhashThreshold int
	now              func() time.Time
	// segmentsFor is the A7 in-memory segment lookup: given a site id + URL it
	// returns the segment names the URL belongs to, annotating each emitted alert
	// Event so route matching can target match:{segment: <name>}. It is the
	// registry-backed hot-path seam — NO DB read. nil when segments are unwired
	// (events then carry no segments, identical to pre-A7 behavior).
	segmentsFor func(siteID int64, url string) []string

	// metrics is the daemon's self-observability layer: ProcessFetch adds
	// rabbot_changes_total{class} over the computed change set. A nil *Metrics
	// no-ops, so unmetered paths and unit tests need no wiring.
	metrics *obs.Metrics

	// blastRadius is the A9 link-graph enrichment seam: given a site + URL it
	// returns that page's one-hop inlink counts (total inlinks, of which
	// high-importance). When a CRITICAL http_status event is emitted for a page
	// whose new status is >= 400, ProcessFetch appends
	// " — linked from N pages (M high-importance)" to the event's After so the
	// operator sees the blast radius of the broken page — flowing through every
	// notifier via the existing After field (zero new notify.Alert fields). nil
	// (graph disabled / unwired) = no enrichment and no suffix; ok=false (e.g. the
	// page is not yet in the graph) = no suffix either. It is a bounded read; the
	// production impl is a single indexed query, safe enough for the alert path.
	blastRadius func(ctx context.Context, siteID int64, url string) (inlinks, highImportance int, ok bool)
}

// ProcessorOption configures a Processor at construction.
type ProcessorOption func(*Processor)

// WithSegmentsFor injects the A7 segment-name lookup used to annotate emitted
// alert events. The func must be safe for concurrent use (the registry's
// SegmentsFor is) and must not block on I/O — it is called on the crawl hot path.
func WithSegmentsFor(fn func(siteID int64, url string) []string) ProcessorOption {
	return func(p *Processor) { p.segmentsFor = fn }
}

// WithMetrics injects the self-observability layer that records
// rabbot_changes_total{class} per detected change. A nil *Metrics no-ops, so
// this is safe to omit.
func WithMetrics(m *obs.Metrics) ProcessorOption {
	return func(p *Processor) { p.metrics = m }
}

// WithBlastRadius injects the A9 link-graph enrichment lookup used to annotate a
// critical http_status alert (status >= 400) with the broken page's inlink blast
// radius. The func must be safe for concurrent use and must not block on heavy
// I/O — it is called on the crawl path. A nil func (or omitting this option, e.g.
// when the graph feature is disabled) means no enrichment: the alert After is
// unchanged. ok=false from the func likewise leaves After unchanged.
func WithBlastRadius(fn func(ctx context.Context, siteID int64, url string) (inlinks, highImportance int, ok bool)) ProcessorOption {
	return func(p *Processor) { p.blastRadius = fn }
}

// NewProcessor builds the post-fetch processor.
func NewProcessor(deps ProcessorDeps, simhashThreshold int, now func() time.Time, opts ...ProcessorOption) *Processor {
	if now == nil {
		now = time.Now
	}
	if simhashThreshold == 0 {
		simhashThreshold = diff.DefaultSimhashThreshold
	}
	p := &Processor{deps: deps, simhashThreshold: simhashThreshold, now: now}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// segmentsForURL returns the segment names a URL belongs to via the injected
// seam, or nil when no seam is wired. Centralized so the two emit points stay in
// sync and a nil seam is handled in one place.
func (p *Processor) segmentsForURL(siteID int64, url string) []string {
	if p.segmentsFor == nil {
		return nil
	}
	return p.segmentsFor(siteID, url)
}

// enrichHTTPStatus appends the A9 blast-radius suffix to a critical http_status
// alert's After when the page is now broken (new status >= 400) and the graph
// enrichment seam is wired and has the page. It is centralized so BOTH emit
// points (the change-stream loop and the rule bridge) enrich identically. A nil
// seam, a non-http_status changeType, a status < 400, or an ok=false lookup all
// leave After unchanged (no suffix, no panic). status is the page's NEW HTTP
// status (newSnap.HTTPStatus) — gating on the integer, not on the textual After
// value, since the After text differs between the two emit sites.
func (p *Processor) enrichHTTPStatus(ctx context.Context, changeType string, status int, siteID int64, url, after string) string {
	if p.blastRadius == nil || changeType != "http_status" || status < 400 {
		return after
	}
	inlinks, highImportance, ok := p.blastRadius(ctx, siteID, url)
	if !ok {
		return after
	}
	return after + fmt.Sprintf("  — linked from %d pages (%d high-importance)", inlinks, highImportance)
}

// ProcessFetch is invoked once per completed fetch. On a non-ok fetch class it
// hands off to the access gate and emits NO SEO changes/issues/alerts. On ok it
// diffs the new snapshot against the prior stored one (oldSnap, the zero
// model.Snapshot{} when there is no prior), records changes, applies the rules
// engine, ingests substantive changes as alert events, and resolves incidents for
// critical signals that have recovered.
//
// It returns changed=true when at least one substantive (non-cosmetic) change was
// detected, so the caller can feed the adaptive recheck cadence (shrink toward the
// minimum on change, grow while stable). The error is best-effort-joined: every
// change is still attempted on a mid-loop IngestEvent failure (errors.Join), so a
// transient hiccup on one event does not silently drop the rest.
// truncated reports that the fetcher cut the response body at its size cap (§5A):
// the snapshot's content hash is then a shifted PREFIX of the real page, so diffing
// it against the prior full content would emit a spurious 'content' change (or hide a
// real one). When truncated is true the processor suppresses the content-field diff
// while still handling the other (head-derived) fields, which are recoverable from the
// bounded prefix.
func (p *Processor) ProcessFetch(ctx context.Context, site model.Site, u model.URL, newSnap, oldSnap model.Snapshot, fc model.FetchClass, detector string, truncated bool) (changed bool, err error) {
	now := p.now()

	ac := alerts.AccessContext{
		SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
		FetchClass: fc, Detector: detector, DeepLink: u.URL,
	}

	// A 304/no-body ok fetch carries no new content: newSnap is the zero value
	// (no snapshot was persisted). Drive only the access gate so an open
	// operational incident (monitoring_blocked/_unreachable) auto-closes on a
	// recovery delivered via 304, but skip diff/rules — there is nothing to
	// compare, and evaluating rules against a zero snapshot would open spurious
	// issues (a healthy 304 would read as Indexable=false, HTTPStatus=0, etc.).
	if fc == model.FetchOK && newSnap.ID == 0 {
		_, herr := p.deps.HandleFetchClass(ctx, ac, nil)
		return false, herr
	}

	handled, herr := p.deps.HandleFetchClass(ctx, ac, nil)
	if herr != nil {
		return false, herr
	}
	if handled {
		return false, nil // non-ok: SEO suppressed, operational incident maintained
	}

	changes := diff.Compare(newSnap, oldSnap, p.simhashThreshold, now)
	if truncated {
		// A truncated body's content hash is an incomplete prefix: drop any 'content'
		// change so it neither records a spurious diff nor hides a real one. Non-content
		// fields (title/meta/etc.) live in the recoverable <head> prefix and stay.
		changes = dropContentChanges(changes)
	}
	if len(changes) > 0 {
		if rerr := p.deps.RecordChanges(ctx, changes); rerr != nil {
			return false, rerr
		}
	}

	// Self-observability: tally rabbot_changes_total{class} over the same change
	// set just recorded (post-truncation drop), so the cosmetic/substantive ratio
	// dashboard mirrors the change stream exactly. Closed-enum class label only;
	// nil Metrics no-ops. Counted once per crawl after the changes are committed.
	var nCosmetic, nSubstantive int
	for _, c := range changes {
		if c.ChangeClass == model.ChangeCosmetic {
			nCosmetic++
		} else {
			nSubstantive++
		}
	}
	p.metrics.AddChanges(string(model.ChangeCosmetic), nCosmetic)
	p.metrics.AddChanges(string(model.ChangeSubstantive), nSubstantive)

	// A substantive (non-cosmetic) change drives the adaptive cadence shrink. This
	// is computed from the recorded diff, independent of which changes raise alerts.
	for _, c := range changes {
		if c.ChangeClass != model.ChangeCosmetic {
			changed = true
			break
		}
	}

	newFindings, aerr := p.deps.ApplyRules(ctx, u.ID, u.Importance, newSnap, oldSnap, changes, truncated)
	if aerr != nil {
		return changed, aerr
	}

	// Feature C: collapse the noindex triad. A page going noindex flips diff fields in
	// ONE crawl — indexable, meta_robots, x_robots_tag, indexability_reason — each mapped
	// Critical, which would otherwise fan out into multiple Slack alerts for one root
	// cause. When `indexable` is in this crawl's change set, emit ONLY the canonical
	// `indexable` event (folding the indexability_reason value into its body) and
	// suppress the standalone meta_robots / x_robots_tag / indexability_reason events.
	// Guarded precisely on `indexable` being present: a meta_robots / x_robots_tag change
	// that flips the noindex verdict WITHOUT an indexable flip this crawl still alerts on
	// its own — no over-suppression (a benign value change that does not flip the verdict
	// is separately gated below, fix #2). x_robots_tag is folded too (fix #4): a
	// header-driven noindex flips x_robots_tag alongside indexable, and omitting it
	// double-paged (indexable + x_robots_tag).
	//
	// Status fan-out collapse (fix #3): a 2xx -> 404/5xx flips http_status AND (usually)
	// indexable AND canonical in one crawl — the page is gone, one root cause. When
	// http_status is in the change set and the NEW status is non-2xx, fold the standalone
	// indexable and canonical events (and their bridged twins) into the single
	// http_status alert. Guarded precisely on http_status present + non-2xx new value, so
	// an indexable flip WITHOUT a status regression (a 2xx meta-noindex) still pages on
	// its own.
	indexableFlipped := false
	statusChanged := false
	statusRegressed := false
	indexabilityReason := ""
	// changedFields records which diff fields changed this crawl. The A3 overflow
	// push gate (below) consults it: an overflow finding pages only when its measured
	// source field (title / meta_description) actually changed — pre-existing long
	// titles must not stampede Slack on upgrade.
	changedFields := make(map[string]bool, len(changes))
	for _, c := range changes {
		changedFields[c.Field] = true
		if c.Field == "indexable" {
			indexableFlipped = true
		}
		if c.Field == "http_status" {
			statusChanged = true
		}
		if c.Field == "indexability_reason" {
			indexabilityReason = c.NewValue
		}
	}
	// An ERROR (4xx/5xx, or a degenerate 1xx) NEW status — a 3xx redirect is deliberately
	// EXCLUDED so a 200->301 still surfaces its canonical/indexability changes rather than
	// being swallowed by the collapse (gated on http_status actually being in the change set
	// so an unrelated field change on a steady error page does not trigger the collapse).
	statusRegressed = statusChanged && (newSnap.HTTPStatus < 200 || newSnap.HTTPStatus >= 400)

	// Emit alert events for substantive changes only (cosmetic churn is suppressed).
	// internal_link_count is recorded in the change log but does NOT raise a
	// standalone alert: a raw link-count delta is normal churn (pagination, nav
	// tweaks); meaningful link regressions surface via the broken_links_spike rule
	// (bridged into the alert path below — Feature A).
	//
	// The loop is best-effort-complete: a mid-loop IngestEvent failure does not skip
	// the remaining events. The change batch is committed up front (RecordChanges)
	// and the schedule advances regardless, so the next crawl re-diffs against the
	// now-stored snapshot and would emit NO change — meaning an un-ingested event is
	// never retried. Joining the per-event errors and continuing gives every change
	// one delivery attempt; the alerts pipeline's dedup makes a re-attempt safe.
	//
	// ingestedTypes tracks the change_types ingested THIS crawl so the rule bridge
	// (Feature A) below can skip any finding whose change_type already alerted — no
	// double-alerting.
	// A7: resolve this URL's segment names ONCE for the whole fetch (the URL is
	// the same for every per-URL event below) via the in-memory registry seam —
	// no DB read on the hot path. Site-level events come from the side-timers, not
	// here, so every event emitted in this function is per-URL and carries these.
	urlSegments := p.segmentsForURL(site.ID, u.URL)

	var ingestErr error
	ingestedTypes := make(map[string]bool, len(changes))
	for _, c := range changes {
		// internal_link_count and redirect_chain are recorded in the change log but do
		// NOT raise a standalone alert. internal_link_count delta is normal churn
		// (meaningful regressions surface via broken_links_spike). redirect_chain's
		// opaque before/after JSON is retired (A5, owner decision #4): chain churn that
		// neither grows nor loops is noise — the parsed redirect_chain_growth /
		// redirect_loop rules own redirect alerting and bridge via Feature A below.
		// render_mode (A8) is likewise recorded as history but raises no standalone
		// alert: a render-mode flip routes Info (severityForField default), and the
		// needs_rendering WARNING rule owns its alert (bridged to render_mode below).
		// Were render_mode ingested here, it would seed ingestedTypes["render_mode"]
		// and the bridge dedup would SWALLOW the needs_rendering warning — so skip it.
		if c.ChangeClass == model.ChangeCosmetic || c.Field == "internal_link_count" ||
			c.Field == "redirect_chain" || c.Field == "render_mode" {
			continue
		}
		// Feature C: when indexable flipped this crawl, fold the noindex triad into the
		// single canonical indexable alert — suppress the standalone meta_robots,
		// x_robots_tag, and indexability_reason events. Only collapses while indexable is
		// in the change set (a meta_robots/x_robots change WITHOUT an indexable flip still
		// alerts on its own — see the benign-verdict gate below).
		if indexableFlipped && (c.Field == "meta_robots" || c.Field == "x_robots_tag" || c.Field == "indexability_reason") {
			continue
		}
		// Status fan-out collapse (fix #3): on a 2xx -> non-2xx regression, fold the
		// indexable and canonical change-stream events into the single http_status alert
		// (the page is gone — one root cause). Gated on http_status present + non-2xx new
		// status, so an indexable flip on a still-2xx page is unaffected.
		if statusRegressed && (c.Field == "indexable" || c.Field == "canonical") {
			continue
		}
		// Benign robots value-change gate (fix #2): a meta_robots / x_robots_tag VALUE
		// change that does NOT flip the noindex verdict (adding max-snippet /
		// max-image-preview / nofollow — not a deindex) is not a critical event; the
		// metaRobotsNoindexRule correctly stays silent for it. Fire the standalone event
		// ONLY when robotsmeta.IsNoindex actually changed, so the change stream and the
		// rule agree. (A verdict flip that ALSO flips indexable is already folded above.)
		if c.Field == "meta_robots" && robotsmeta.IsNoindex(c.OldValue) == robotsmeta.IsNoindex(c.NewValue) {
			continue
		}
		if c.Field == "x_robots_tag" && robotsmeta.IsNoindex(c.OldValue) == robotsmeta.IsNoindex(c.NewValue) {
			continue
		}
		before, after := c.OldValue, c.NewValue
		switch c.Field {
		case "content":
			// The content field's before/after are raw SHA-256 hashes; never
			// surface them in the alert — they are meaningless to a human and
			// noisy in Slack. The SimHash-classified substantive flag is the signal.
			before, after = "", "main content changed (substantive)"
		case "indexable":
			// Canonical noindex alert: fold the indexability_reason into the body so the
			// root cause (e.g. "meta noindex") is preserved despite suppressing the
			// standalone indexability_reason event.
			if indexabilityReason != "" {
				after = after + " (reason: " + indexabilityReason + ")"
			}
		}
		// A9 enrichment: a critical http_status event for a now-broken page (>= 400)
		// gains a "linked from N pages (M high-importance)" suffix so the operator
		// sees the blast radius. No-op when the graph seam is unwired or the page is
		// not yet in the graph.
		after = p.enrichHTTPStatus(ctx, c.Field, newSnap.HTTPStatus, site.ID, u.URL, after)
		ev := alerts.Event{
			SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
			ChangeType: c.Field, Severity: severityForField(c.Field),
			Before: before, After: after, DeepLink: u.URL,
			Segments: urlSegments,
		}
		if ierr := p.deps.IngestEvent(ctx, ev); ierr != nil {
			ingestErr = errors.Join(ingestErr, ierr)
		}
		ingestedTypes[c.Field] = true
	}

	// Feature A: bridge newly-opened rule findings into the alert path. Rule findings
	// (e.g. broken_links_spike on the skipped internal_link_count field, or any rule
	// firing on a page broken on its FIRST crawl with no diff) open issues via
	// ApplyRules but never reach Slack through the change-stream loop above. Emit an
	// alert event for each newly-opened critical/warning finding, deduped against the
	// change_types already ingested this crawl so a field that trips BOTH a rule and a
	// change-stream event fires EXACTLY once.
	//
	// First-crawl guard: on a brand-new page (oldSnap.ID == 0) there is no prior
	// baseline, so the only findings worth paging are genuine fetch-level BREAKAGE
	// (a 4xx/5xx via status_regression -> http_status), not steady-state page-quality
	// hygiene findings (missing canonical/title/meta/h1, or a page that is noindex
	// from its very first observation). Those hygiene findings still OPEN an issue
	// (tracked + queryable via `rabbot issues`), but bridging them to Slack would page
	// the operator on essentially every newly-discovered page that lacks a self-
	// canonical — normal real-world pages, not regressions. This mirrors the
	// transition rules (indexability_flip, broken_links_spike) which already self-guard
	// Old.ID != 0: a steady state present from the first crawl is not a regression.
	// status_regression is the lone first-crawl exception because a 5xx is breakage
	// regardless of baseline (the rule fires on s >= 500 with no Old dependency).
	firstCrawl := oldSnap.ID == 0
	for _, f := range newFindings {
		if f.Severity != model.SeverityCritical && f.Severity != model.SeverityWarning {
			continue // info-tier findings are not pushed to Slack
		}
		if firstCrawl && f.Field != "http_status" {
			continue // no baseline: only genuine fetch breakage pages on a first crawl
		}
		if ingestedTypes[f.Field] {
			continue // already alerted via the change-stream loop — no double-alert
		}
		// Feature C interaction: if indexable collapsed the triad this crawl, a bridged
		// meta_robots / x_robots_tag / indexability_reason finding would re-introduce a
		// suppressed alert. Keep the collapse coherent across both paths (fix #4 adds
		// x_robots_tag, mirroring the change-stream fold above).
		if indexableFlipped && (f.Field == "meta_robots" || f.Field == "x_robots_tag" || f.Field == "indexability_reason") {
			continue
		}
		// Status fan-out collapse (fix #3), bridge twin: on a 2xx -> non-2xx regression,
		// suppress the bridged indexability_flip (-> indexable) and canonical_changed
		// (-> canonical) findings so they fold into the single http_status alert rather
		// than re-introducing the fan-out the change-stream loop just suppressed.
		if statusRegressed && (f.Field == "indexable" || f.Field == "canonical") {
			continue
		}
		// A3 anti-stampede push gate: an overflow finding (title_pixel_overflow /
		// meta_description_pixel_overflow) pages ONLY when its measured source field
		// changed this crawl. A pre-existing long title that did not change this crawl
		// — the upgrade case, where the rule newly opens an issue for every overflowing
		// page on the fleet — opens its issue silently (visible on pull/agent surfaces)
		// but does not page. A title/description edited INTO overflow still pages,
		// because its source field is in this crawl's change set. (title/meta diffs are
		// always substantive, so a real edit always lands in changedFields.)
		if src, isOverflow := overflowSourceField[f.Field]; isOverflow && !changedFields[src] {
			continue
		}
		// The finding's Detail JSON rides through to Event.After so the alert body can
		// show the numbers behind the finding (A3's measured-px detail, A5's old/new
		// counts). Findings with no detail leave After empty — identical to prior
		// behavior. This is a strict enrichment of the bridged alert surface (A3 open
		// question #2, resolved: accept the passthrough for all bridged findings).
		// A9 enrichment: a brand-new page broken on its FIRST crawl (a 4xx/5xx with
		// no prior snapshot) reaches Slack ONLY via this bridge — the change-stream
		// loop never ran for it. Enrich its critical http_status After with the
		// blast-radius suffix here too, so first-crawl breakage and steady-state
		// regressions carry the same context. (Both emit sites share enrichHTTPStatus.)
		after := p.enrichHTTPStatus(ctx, f.Field, newSnap.HTTPStatus, site.ID, u.URL, f.Detail)
		ev := alerts.Event{
			SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
			ChangeType: f.Field, Severity: f.Severity, After: after, DeepLink: u.URL,
			Segments: urlSegments,
		}
		if ierr := p.deps.IngestEvent(ctx, ev); ierr != nil {
			ingestErr = errors.Join(ingestErr, ierr)
		}
		ingestedTypes[f.Field] = true // guard against duplicate findings within the batch
	}

	// Resolve incidents for critical fields that reverted to a healthy value.
	// A failure to auto-resolve an incident on recovery must not be swallowed:
	// surface it (joined with any ingest error) so the crawl is recorded as failed.
	resolveErr := p.resolveHealthyFields(ctx, site, u, newSnap, oldSnap)

	// A6 compute-trigger seam: the rules pass succeeded (ApplyRules returned above
	// without error) and the issue state for this URL is settled, so recompute and
	// (on change) persist the health score for the site and the segments containing
	// this URL. Reached only on a successful rules pass — a non-ok / 304 fetch
	// returned earlier, and an ApplyRules error short-circuited above. A recompute
	// failure is surfaced (joined) rather than dropped, like the resolve error.
	scoreErr := p.deps.RecordHealthScore(ctx, site.ID, u.ID)
	return changed, errors.Join(ingestErr, resolveErr, scoreErr)
}

// resolveHealthyFields closes incidents for critical signals that have returned
// to a healthy state (e.g. indexable flipped back to true), so the incident
// auto-closes on recovery rather than waiting for the 24h sweep. It returns the
// FIRST ResolveEvent error so a failed auto-resolution is not silently lost.
func (p *Processor) resolveHealthyFields(ctx context.Context, site model.Site, u model.URL, newSnap, oldSnap model.Snapshot) error {
	if oldSnap.ID == 0 {
		return nil
	}
	// indexability recovered
	if !oldSnap.Indexable && newSnap.Indexable {
		if err := p.deps.ResolveEvent(ctx, alerts.Event{
			SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
			ChangeType: "indexable", Severity: model.SeverityCritical,
		}); err != nil {
			return err
		}
	}
	// status recovered to a true 2xx success. A 3xx is NOT a recovery: a dead URL
	// that now redirects elsewhere is still not serving its content (and a 200->301
	// is itself a regression). Mirror statusRegressionRule's 2xx-only notion of
	// healthy — only [200,300) clears an http_status incident.
	if oldSnap.HTTPStatus >= 400 && newSnap.HTTPStatus >= 200 && newSnap.HTTPStatus < 300 {
		if err := p.deps.ResolveEvent(ctx, alerts.Event{
			SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
			ChangeType: "http_status", Severity: model.SeverityCritical,
		}); err != nil {
			return err
		}
	}
	// redirect_loop recovered: the prior chain revisited a URL (a within-cap loop,
	// the redirect_loop rule's critical finding) and the new chain is clean. Close the
	// incident immediately on recovery rather than waiting for the auto-close sweep,
	// mirroring indexable / http_status. Both chains must PARSE (don't guess on garbage);
	// the shared rules.RedirectChainInfo is the single definition of "loops". Guarded on
	// old-looped so a chain that was never looping raises no spurious resolve.
	_, oldLoop, oldOK := rules.RedirectChainInfo(oldSnap.RedirectChain)
	_, newLoop, newOK := rules.RedirectChainInfo(newSnap.RedirectChain)
	if oldOK && newOK && oldLoop != "" && newLoop == "" {
		if err := p.deps.ResolveEvent(ctx, alerts.Event{
			SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
			ChangeType: "redirect_loop", Severity: model.SeverityCritical,
		}); err != nil {
			return err
		}
	}
	// needs_rendering (A8) recovered: the page left the shell states (the
	// needsRenderingRule's failing set, model.RenderMode.IsShell) for a monitorable
	// mode, so its content is visible without JS again. The rule passes and the
	// engine closes the issue row, but the bridged render_mode incident must resolve
	// NOW rather than linger until the 24h auto-close sweep — mirroring indexable /
	// http_status / redirect_loop. ChangeType render_mode + WARNING matches the
	// bridge key and the rule severity, so the group fingerprint lines up with the
	// open incident. (oldSnap.ID == 0 already returned above, so first crawl is safe.)
	if oldSnap.RenderMode.IsShell() && !newSnap.RenderMode.IsShell() {
		if err := p.deps.ResolveEvent(ctx, alerts.Event{
			SiteID: site.ID, Site: site.BaseURL, URL: u.URL, URLID: u.ID,
			ChangeType: "render_mode", Severity: model.SeverityWarning,
		}); err != nil {
			return err
		}
	}
	return nil
}

// dropContentChanges returns changes with any "content" field entry removed. Used
// when the fetched body was truncated at the size cap: the content hash is then a
// shifted prefix, so a content diff against the prior full page is unreliable.
func dropContentChanges(changes []model.Change) []model.Change {
	out := make([]model.Change, 0, len(changes))
	for _, c := range changes {
		if c.Field == "content" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// severityForField buckets a diffed field into an alert-routing tier (§5). Note:
// some fields here (e.g. internal_link_count) are intentionally NOT emitted as
// standalone alert events by ProcessFetch's ingest loop above — this map only
// classifies the severity for the fields that ARE emitted.
func severityForField(field string) model.Severity {
	switch field {
	// indexability_reason is the cause-of-deindexing companion to indexable: the
	// reason can change WITHOUT the indexable bool flipping (e.g. meta noindex ->
	// canonicalized away), so it is the sole signal of a root-cause regression and
	// must route at the same critical tier as indexable, not fall through to info.
	case "indexable", "indexability_reason", "canonical", "meta_robots", "x_robots_tag", "http_status":
		return model.SeverityCritical
	// robots_txt / robots_txt_status are site-level fields emitted by diff.CompareFile,
	// now wired through the robots side-timer (SideTimers.RefreshRobots, #8). A robots.txt
	// content change can ship a catastrophic "Disallow: /" deindex-the-whole-site rule,
	// and a status regression (robots.txt 200 -> 5xx/404) is itself an SEO emergency —
	// both route at the critical tier.
	case "robots_txt", "robots_txt_status":
		return model.SeverityCritical
	// sitemap_xml_status is the sitemap accessibility-regression signal emitted by
	// diff.CompareFile and wired through the sitemap side-timer (SideTimers.RefreshSitemap,
	// A2): a sitemap that breaks (200 -> 404/5xx/network-error) is the engine going blind
	// to a site's declared URL set — critical, same tier as a robots.txt status break.
	case "sitemap_xml_status":
		return model.SeverityCritical
	// content is the main-body change emitted by diff.Compare when the content hash
	// shifts and SimHash classifies it substantive. A rewritten/replaced page body is
	// a meaningful SEO signal, not info-tier noise, so it routes at the warning tier
	// alongside title/meta_description. (NOTE: a route configured match:{severity:
	// critical} still excludes warnings by design — the recommended route should be
	// critical+warning; that docs change is a separate follow-up.)
	// redirect_chain and internal_link_count stay classified here (the field remains a
	// known warning-tier field), but ProcessFetch's ingest loop SKIPS both as standalone
	// alerts: internal_link_count delta is churn (broken_links_spike owns it), and the
	// opaque redirect_chain alert is retired (A5) — the parsed redirect_chain_growth /
	// redirect_loop rules own redirect alerting. This map only sets severity for the
	// fields that ARE emitted; it is harmless to keep the classification for skipped ones.
	// sitemap_xml (set change) and sitemap_coverage_drift (declared-vs-crawled drift
	// grew) are the sitemap watch's warning-tier signals (A2): a URL set churn or a
	// growing coverage gap is a meaningful monitoring signal, not a critical outage —
	// it routes alongside the other site-level content-change warnings.
	// hreflang is a bare set-change detector with NO validity check: #16 killed its
	// critical RULE tier (hreflangInvalidRule emits WARNING). The change-stream severity
	// must AGREE — routing it critical here paged CRITICAL on any hreflang churn while
	// the rule stayed WARNING, and the bridge dedup then swallowed the hreflang_invalid
	// finding. It routes WARNING alongside title / meta_description / schema_types.
	// index_status_discrepancy / google_canonical_mismatch are the GSC W2 signals
	// (gscsignals.go). Both clear the "valid + worth-the-distraction" bar but are
	// ground-truth divergences to investigate, not confirmed live outages — Google's
	// coverage states lag/transition and a deliberate-canonical disagreement is rarely
	// an emergency. Warning keeps them visible on pull/agent surfaces and Slack
	// without paging at the critical tier (and lets an over-cap one buffer to the
	// digest — the desired anti-noise behavior). Extending the shared classifier (vs
	// hardcoding on the Event) keeps it the single source of truth for both the bridge
	// and any future change-stream.
	case "title", "meta_description", "headings", "schema_types", "hreflang", "redirect_chain", "internal_link_count", "content", "sitemap_xml", "sitemap_coverage_drift", changeTypeIndexDiscrepancy, changeTypeCanonicalMismatch:
		return model.SeverityWarning
	default:
		return model.SeverityInfo
	}
}
