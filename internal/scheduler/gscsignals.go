package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

// GSC W2 intelligence half: the signal evaluator over the rows the W1 puller
// stored. It runs DAILY (the GSC pull cadence), right after a site's GSC pull
// refreshes url_index_status — NOT on the crawl hot path (index status only
// changes when GSC is re-inspected). It reads Google's ground truth
// (url_index_status) and Rabbot's own indexability verdict (latest snapshot) and
// emits two per-URL signals through the EXISTING alert pipeline (the same incident
// dedup / Feature-B member-close machinery every other event uses):
//
//   1. index_status_discrepancy — Rabbot and Google disagree on whether a URL is
//      indexable (we say indexable, Google didn't index it — or the inverse).
//   2. google_canonical_mismatch — Google chose a different canonical than declared.
//
// The third signal (search_performance_shift) is NOT here: it is an ENRICHMENT on
// existing change records computed at the report/MCP read layer (SearchPerformanceShift
// below), never a standalone alert. Standalone raw-traffic/impression/ranking
// thresholds are a HARD non-goal (noise).
//
// Correctness invariants (every one is a falsifiable test in gscsignals_test.go):
//   - NEVER fire on missing GSC data: an un-inspected URL (LatestURLIndexStatus
//     ok=false) is skipped, not read as not-indexed (the single most important
//     guard — the spec's "no false discrepancy on missing data").
//   - NEVER fire on an ambiguous Google coverage state: gscIndexed returns known=false
//     and the URL is skipped (and any prior incident resolved, since we can no longer
//     confirm the discrepancy).
//   - Drive BOTH Ingest (currently discrepant) AND Resolve (now agreeing / data gone)
//     every tick — there is no crawl-driven resolve for these, so a cleared
//     discrepancy only auto-closes its incident because this evaluator resolves it.
//   - Canonicalize BOTH sides of the canonical comparison so trailing-slash / scheme
//     / host-case-equivalent forms do not false-positive.

const (
	// changeTypeIndexDiscrepancy is the alert change_type for signal 1. It flows
	// through the existing alerts/incidents/alert_members tables as a string — no
	// schema is needed (incident identity is hash(site, change_type, severity)).
	changeTypeIndexDiscrepancy = "index_status_discrepancy"
	// changeTypeCanonicalMismatch is the alert change_type for signal 2.
	changeTypeCanonicalMismatch = "google_canonical_mismatch"
)

// GSCVerdictReader is the narrow store read surface the evaluator needs: Rabbot's
// own indexability verdict for a URL (GetURL → urls row, LatestSnapshot → the
// extract-derived Indexable/IndexabilityReason) plus Google's latest ground-truth
// row (LatestURLIndexStatus). *store.DB satisfies it; tests substitute a mock.
type GSCVerdictReader interface {
	GetURL(ctx context.Context, siteID int64, normalizedURL string) (model.URL, error)
	LatestSnapshot(ctx context.Context, urlID int64) (model.Snapshot, error)
	LatestURLIndexStatus(ctx context.Context, siteID int64, url string) (model.URLIndexStatus, bool, error)
}

// GSCAlertSink is the alerts seam the evaluator drives — satisfied by *alerts.Pipeline
// (Ingest opens/updates an incident with dedup; Resolve closes it only when the last
// member URL recovers; HasOpenMember reports whether a URL is already a member of an
// open incident). A nil sink makes Evaluate a clean no-op (the GSC-disabled / test
// paths), exactly like SideTimers.Alerts.
//
// HasOpenMember is the daily-re-page anti-noise probe. These signals are re-evaluated
// on every daily pull tick, but the pipeline's per-event DedupWindow (~minutes) is far
// shorter than the day-long cadence, so a steady discrepancy/mismatch would re-notify
// an already-open incident EVERY day. The evaluator instead fires on STATE CHANGE only:
// it skips Ingest when this URL is already a member of an open incident for the same
// change_type (HasOpenMember=true), so the incident notifies once and stays quiet until
// it resolves and recurs. A NEW URL (not yet a member) still Ingests, preserving
// per-URL member tracking (Feature B) and the notify-on-newly-affected-URL behavior.
type GSCAlertSink interface {
	Ingest(ctx context.Context, e alerts.Event) error
	Resolve(ctx context.Context, e alerts.Event) error
	HasOpenMember(ctx context.Context, e alerts.Event) (bool, error)
}

// GSCSignals evaluates the two per-URL GSC alert signals for a site after its daily
// pull. It owns only narrow interfaces so it is unit-testable against mocked GSC/store
// state with no live daemon, mirroring the W1 puller's test discipline. It is built
// in the daemon beside the GSCPuller (run.go) holding stack.Pipeline as Alerts.
type GSCSignals struct {
	// Reader resolves Google's ground truth and Rabbot's verdict for a URL.
	Reader GSCVerdictReader
	// Candidates yields the per-site URL set to evaluate (the same importance-ordered
	// inspection set the puller wrote statuses for). Reusing the puller's
	// URLCandidateSource keeps the evaluation set aligned with what was inspected.
	Candidates URLCandidateSource
	// Alerts is the incident pipeline. Nil = no-op.
	Alerts GSCAlertSink
	// Budget caps how many candidate URLs are evaluated per tick. <=0 uses
	// gscDefaultSignalBudget. It mirrors the inspection budget so the evaluation set
	// matches the set the puller refreshed.
	Budget int
	// Now is the clock used for the signal-1 staleness guard (how old is the GSC
	// inspection relative to wall time). Nil defaults to time.Now; tests inject a
	// fixed clock. It is read through now() so it never panics when unset.
	Now func() time.Time
}

// now returns the evaluator's clock, defaulting to time.Now when unset.
func (g *GSCSignals) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// gscMaxInspectionAge is the absolute staleness ceiling for a url_index_status row in
// the signal-1 discrepancy comparison. GSC URL-inspection is per-day quota-bounded, so
// a low-importance URL can carry an inspection that is days or weeks old; past this age
// the stored Google verdict is too stale to trust against Rabbot's CURRENT snapshot
// (Google may simply not have re-seen the page), so the discrepancy is skipped rather
// than fired. 30 days comfortably covers a slow re-inspection cadence while still
// retiring genuinely ancient ground truth. It only applies to a row with a real
// (non-zero) InspectedAt — a zero timestamp means "age unknown" and is not gated here.
const gscMaxInspectionAge = 30 * 24 * time.Hour

// gscDefaultSignalBudget bounds the per-tick evaluation set when Budget is unset. It
// matches gscDefaultDailyInspectBudget so the URLs evaluated are the URLs whose
// statuses the same-cadence pull just refreshed.
const gscDefaultSignalBudget = gscDefaultDailyInspectBudget

func (g *GSCSignals) budget() int {
	if g.Budget > 0 {
		return g.Budget
	}
	return gscDefaultSignalBudget
}

// Evaluate runs both per-URL GSC signals for one site over its candidate URL set.
// For each URL it drives EXACTLY ONE of Ingest (currently discrepant/mismatched) or
// Resolve (now agreeing, or the GSC data is gone) per signal, so a cleared signal
// auto-closes its incident on the next daily tick. A nil Alerts sink, a nil Reader,
// or a nil Candidates source makes it a clean no-op (the GSC-disabled paths).
//
// Failure policy mirrors the puller's best-effort-complete loop: a SYSTEMIC read
// error (GetURL/LatestSnapshot returning something other than ErrNotFound, or a
// pipeline Ingest/Resolve error) is joined and surfaced so the daemon logs it, but
// a single URL's not-found (no Rabbot row / never crawled / un-inspected) is a quiet
// per-URL skip that never poisons the rest of the site's evaluation.
func (g *GSCSignals) Evaluate(ctx context.Context, site model.Site) error {
	if g == nil || g.Alerts == nil || g.Reader == nil || g.Candidates == nil {
		return nil
	}
	cands, err := g.Candidates.InspectionCandidates(ctx, site.ID, g.budget())
	if err != nil {
		return fmt.Errorf("gsc signals: select candidates for site %q: %w", site.BaseURL, err)
	}

	var errs error
	for _, cand := range cands {
		if ctx.Err() != nil {
			break // cooperative shutdown
		}
		if e := g.evalSignal1(ctx, site, cand.URL); e != nil {
			errs = errors.Join(errs, e)
		}
		if e := g.evalSignal2(ctx, site, cand.URL); e != nil {
			errs = errors.Join(errs, e)
		}
	}
	return errs
}

// evalSignal1 evaluates index_status_discrepancy for one URL: compare Rabbot's
// stored indexability verdict against Google's ground truth, ingest on genuine
// disagreement, resolve on agreement. It SKIPS (and resolves nothing — there is
// nothing to retract on absent data) when the GSC row is missing (ok=false), when
// Google's state is ambiguous, or when Rabbot has no verdict (no urls row / never
// crawled). The skip-on-missing-data branch is the spec's hard no-false-positive
// guard.
func (g *GSCSignals) evalSignal1(ctx context.Context, site model.Site, rawURL string) error {
	idx, ok, err := g.Reader.LatestURLIndexStatus(ctx, site.ID, rawURL)
	if err != nil {
		return fmt.Errorf("gsc signals: read index status %q: %w", rawURL, err)
	}
	if !ok {
		return nil // no GSC data → never a discrepancy
	}
	googleIndexed, known := gscIndexed(idx)
	if !known {
		// Ambiguous Google state: we cannot confirm a discrepancy. Retract any prior
		// open discrepancy for this URL (we can no longer assert it) and stay quiet.
		return g.resolve(ctx, site, rawURL, changeTypeIndexDiscrepancy, 0)
	}

	// Rabbot's own verdict. A missing urls row or missing snapshot means we have no
	// engine verdict to validate against Google → skip (do not resolve: absent Rabbot
	// data is not "agreement").
	u, verr := g.Reader.GetURL(ctx, site.ID, rawURL)
	if errors.Is(verr, store.ErrNotFound) {
		return nil
	}
	if verr != nil {
		return fmt.Errorf("gsc signals: resolve url %q: %w", rawURL, verr)
	}
	snap, serr := g.Reader.LatestSnapshot(ctx, u.ID)
	if errors.Is(serr, store.ErrNotFound) {
		return nil
	}
	if serr != nil {
		return fmt.Errorf("gsc signals: latest snapshot for %q: %w", rawURL, serr)
	}

	// Staleness guard for the FIRE decision: a discrepancy is only credible when
	// Google's inspection is fresh enough to have seen Rabbot's CURRENT state. A
	// quota-bounded inspection can be days/weeks old; firing a discrepancy against a
	// freshly-changed Rabbot verdict then cries wolf about a divergence Google simply
	// hasn't re-evaluated yet. When the inspection is too stale to trust we SKIP the
	// fire (quietly — staleness defers judgment, it does not assert agreement, so we
	// do not Ingest). The AGREEMENT path is intentionally NOT gated: a stale row that
	// already agrees with Rabbot can still safely auto-close a prior incident, and the
	// resolve is what keeps incidents from stranding open across an aging-inspection gap.
	tooStale := gscInspectionTooStale(idx.InspectedAt, snap.FetchedAt, g.now())

	switch {
	case snap.Indexable && !googleIndexed:
		if tooStale {
			return nil // would fire, but Google's inspection predates / outlives trust → defer
		}
		// "We think it's fine, Google disagrees" — the high-signal case.
		after := fmt.Sprintf("Rabbot says indexable; Google: %s", googleStateLabel(idx))
		return g.ingestOnStateChange(ctx, site, rawURL, u.ID, changeTypeIndexDiscrepancy, "indexable", after)
	case !snap.Indexable && googleIndexed:
		if tooStale {
			return nil // inverse discrepancy, same staleness defer
		}
		// Inverse: we suppress it, Google indexed it anyway.
		before := "noindex"
		if snap.IndexabilityReason != "" {
			before = fmt.Sprintf("noindex (%s)", snap.IndexabilityReason)
		}
		after := fmt.Sprintf("Rabbot says %s; Google indexed it", before)
		return g.ingestOnStateChange(ctx, site, rawURL, u.ID, changeTypeIndexDiscrepancy, before, after)
	default:
		// Agreement (both indexable, or both not-indexable) → resolve any prior incident.
		return g.resolve(ctx, site, rawURL, changeTypeIndexDiscrepancy, u.ID)
	}
}

// gscInspectionTooStale reports whether a url_index_status row is too stale to trust
// as ground truth for the index_status_discrepancy FIRE decision, given Rabbot's
// snapshot time and the current clock. It is true when EITHER:
//
//   - the GSC inspection PREDATES the Rabbot snapshot it is compared against
//     (inspectedAt < snapFetchedAt): Google last looked before Rabbot's current state
//     existed, so a "disagreement" is just Google not having re-seen the page yet; or
//   - the inspection is older than gscMaxInspectionAge in absolute terms
//     (now - inspectedAt > gscMaxInspectionAge): quota-bounded ancient ground truth.
//
// Both checks are skipped for a ZERO timestamp ("age unknown" — e.g. a fixture or a
// pre-W2 row with no InspectedAt), so an unset time never trips the guard. This keeps
// the guard a pure no-op for callers that do not populate the timestamps while fully
// protecting the production path (the puller always stamps InspectedAt, and a crawled
// URL always has FetchedAt).
func gscInspectionTooStale(inspectedAt, snapFetchedAt, now time.Time) bool {
	if !inspectedAt.IsZero() && !snapFetchedAt.IsZero() && inspectedAt.Before(snapFetchedAt) {
		return true
	}
	if !inspectedAt.IsZero() && now.Sub(inspectedAt) > gscMaxInspectionAge {
		return true
	}
	return false
}

// evalSignal2 evaluates google_canonical_mismatch for one URL: a pure GSC-internal
// comparison of the declared (userCanonical) vs Google-chosen (googleCanonical)
// canonical. It needs no Rabbot snapshot. It fires ONLY when both are non-empty and,
// after canonicalization, differ; it resolves when they realign; and it SKIPS (no
// ingest, no resolve) when the GSC row is missing or either canonical is empty
// (absent data, never a false mismatch).
func (g *GSCSignals) evalSignal2(ctx context.Context, site model.Site, rawURL string) error {
	idx, ok, err := g.Reader.LatestURLIndexStatus(ctx, site.ID, rawURL)
	if err != nil {
		return fmt.Errorf("gsc signals: read index status %q: %w", rawURL, err)
	}
	if !ok {
		return nil // no GSC data
	}
	if idx.UserCanonical == "" || idx.GoogleCanonical == "" {
		// Google hasn't reported one side (yet, or no longer). We can't assert a mismatch from
		// absent data — but if a mismatch incident was opened on a prior tick, clear it rather
		// than strand it. resolveIfOpen acts only when one is actually open, so the common
		// absent-data tick doesn't churn no-op resolves.
		return g.resolveIfOpen(ctx, site, rawURL, changeTypeCanonicalMismatch)
	}
	if canonicalEquivalent(idx.UserCanonical, idx.GoogleCanonical) {
		return g.resolve(ctx, site, rawURL, changeTypeCanonicalMismatch, 0)
	}
	// Genuine mismatch. Resolve the URLID best-effort (signal 2 doesn't need it; an
	// absent urls row yields 0, which is fine for member tracking by URL string).
	urlID := g.urlIDFor(ctx, site, rawURL)
	return g.ingestOnStateChange(ctx, site, rawURL, urlID, changeTypeCanonicalMismatch, idx.UserCanonical, idx.GoogleCanonical)
}

// urlIDFor resolves a URL's urls.id for the alert event, returning 0 when Rabbot
// never admitted the URL (GetURL → ErrNotFound) — the alerts.Event then carries
// URLID 0, which member tracking (keyed by URL string) handles fine. A systemic
// read error is swallowed to a 0 id (signal 2 must still fire on the canonical
// facts; the id is a convenience, not load-bearing).
func (g *GSCSignals) urlIDFor(ctx context.Context, site model.Site, rawURL string) int64 {
	u, err := g.Reader.GetURL(ctx, site.ID, rawURL)
	if err != nil {
		return 0
	}
	return u.ID
}

// ingestOnStateChange dispatches a firing signal ONLY on a state change: if this URL
// is already a tracked member of an open incident for changeType, the signal is
// steady-state (already notified on a prior tick) and is SKIPPED — this is the
// daily-re-page anti-noise guard, making Evaluate idempotent across the daily pull
// ticks. A URL that is NOT yet a member (a freshly-affected URL, or a recurrence after
// the incident closed) falls through to ingest, which registers its membership and
// notifies. A HasOpenMember read error is surfaced (it gates a real alert decision);
// it never silently drops the alert.
func (g *GSCSignals) ingestOnStateChange(ctx context.Context, site model.Site, rawURL string, urlID int64, changeType, before, after string) error {
	probe := alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        rawURL,
		ChangeType: changeType,
		Severity:   severityForField(changeType),
	}
	already, err := g.Alerts.HasOpenMember(ctx, probe)
	if err != nil {
		return fmt.Errorf("gsc signals: open-member probe %s for %q: %w", changeType, rawURL, err)
	}
	if already {
		// Steady state: this URL is already a member of an open incident for this
		// change_type → do not re-notify. Fire-on-state-change only.
		return nil
	}
	return g.ingest(ctx, site, rawURL, urlID, changeType, before, after)
}

// ingest builds and dispatches the alerts.Event for a firing signal. Severity is
// derived from the shared severityForField classifier so the bridge and any future
// change-stream agree on the tier. The Event carries a non-empty URL so member
// tracking (Feature B) works — these are per-URL signals.
func (g *GSCSignals) ingest(ctx context.Context, site model.Site, rawURL string, urlID int64, changeType, before, after string) error {
	ev := alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        rawURL,
		URLID:      urlID,
		ChangeType: changeType,
		Severity:   severityForField(changeType),
		Before:     before,
		After:      after,
		DeepLink:   rawURL,
	}
	if err := g.Alerts.Ingest(ctx, ev); err != nil {
		return fmt.Errorf("gsc signals: ingest %s for %q: %w", changeType, rawURL, err)
	}
	return nil
}

// resolve retracts an open incident for this URL+signal (no-op in the pipeline when
// none is open). The Event must carry the SAME identity (site, change_type, severity,
// URL) the firing ingest used so the group fingerprint lines up and member tracking
// removes the right URL.
func (g *GSCSignals) resolve(ctx context.Context, site model.Site, rawURL string, changeType string, urlID int64) error {
	ev := alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        rawURL,
		URLID:      urlID,
		ChangeType: changeType,
		Severity:   severityForField(changeType),
		DeepLink:   rawURL,
	}
	if err := g.Alerts.Resolve(ctx, ev); err != nil {
		return fmt.Errorf("gsc signals: resolve %s for %q: %w", changeType, rawURL, err)
	}
	return nil
}

// resolveIfOpen resolves an incident for this URL+changeType only when this URL is actually a
// tracked member of an open one — so absent-data ticks (the common case) don't churn no-op
// resolves, while a prior mismatch incident is still cleared rather than stranded permanently.
func (g *GSCSignals) resolveIfOpen(ctx context.Context, site model.Site, rawURL, changeType string) error {
	probe := alerts.Event{
		SiteID:     site.ID,
		Site:       site.BaseURL,
		URL:        rawURL,
		ChangeType: changeType,
		Severity:   severityForField(changeType),
	}
	open, err := g.Alerts.HasOpenMember(ctx, probe)
	if err != nil {
		return fmt.Errorf("gsc signals: open-member probe %s for %q: %w", changeType, rawURL, err)
	}
	if !open {
		return nil
	}
	return g.resolve(ctx, site, rawURL, changeType, 0)
}

// ── index-state classification ───────────────────────────────────────────────

// gscIndexed interprets a stored url_index_status into Google's binary "is this URL
// in the index" ground truth. It returns (indexed, known): known=false means the
// state is ambiguous/unrecognized and the caller MUST treat it as no-signal (never
// force it to a verdict). The classification keys primarily on coverageState (the
// human-readable category Search Console surfaces), cross-checked with verdict —
// because Google's coverage strings are the authoritative, stable category names and
// new/unmodeled ones must fall through to known=false rather than be guessed.
func gscIndexed(idx model.URLIndexStatus) (indexed, known bool) {
	cov := normalizeState(idx.CoverageState)
	if cov == "" {
		// No coverage state at all (e.g. an inspection that only resolved a verdict).
		// Fall back to verdict only for an unambiguous PASS/FAIL; otherwise unknown.
		switch strings.ToUpper(strings.TrimSpace(idx.Verdict)) {
		case "PASS":
			return true, true
		case "FAIL":
			return false, true
		default:
			return false, false
		}
	}
	if _, ok := gscIndexedCoverage[cov]; ok {
		return true, true
	}
	if _, ok := gscNotIndexedCoverage[cov]; ok {
		return false, true
	}
	return false, false // unrecognized coverage state → no confident verdict
}

// normalizeState folds a GSC state string to a stable comparison key: trimmed,
// lower-cased, and with the curly/straight apostrophe variants unified (Google
// renders "Excluded by 'noindex' tag" with a typographic apostrophe).
func normalizeState(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "’", "'") // right single quote → ASCII apostrophe
	return s
}

// gscIndexedCoverage is the set of coverageState values that mean "Google has this
// URL indexed". Kept deliberately tight — only the unambiguous indexed categories.
var gscIndexedCoverage = map[string]struct{}{
	"submitted and indexed":             {},
	"indexed, not submitted in sitemap": {},
	"indexed, low interest":             {},
}

// gscNotIndexedCoverage is the set of coverageState values that mean "Google is NOT
// indexing this URL" — the discrepancy-worthy states when Rabbot believes the page
// is indexable. It enumerates the documented URL-Inspection coverage categories so an
// UNlisted (future/unknown) state falls through to known=false instead of being
// misread as not-indexed.
var gscNotIndexedCoverage = map[string]struct{}{
	"crawled - currently not indexed":                       {},
	"discovered - currently not indexed":                    {},
	"excluded by 'noindex' tag":                             {},
	"page with redirect":                                    {},
	"alternate page with proper canonical tag":              {},
	"duplicate without user-selected canonical":             {},
	"duplicate, google chose different canonical than user": {},
	"duplicate, submitted url not selected as canonical":    {},
	"blocked by robots.txt":                                 {},
	"indexed, though blocked by robots.txt":                 {},
	"blocked due to unauthorized request (401)":             {},
	"blocked due to access forbidden (403)":                 {},
	"not found (404)":                                       {},
	"soft 404":                                              {},
	"server error (5xx)":                                    {},
	"redirect error":                                        {},
	"url is unknown to google":                              {},
	"excluded by page removal tool":                         {},
	"blocked due to other 4xx issue":                        {},
	"crawl anomaly":                                         {},
	"submitted url blocked by robots.txt":                   {},
	"submitted url marked 'noindex'":                        {},
	"submitted url seems to be a soft 404":                  {},
	"submitted url not found (404)":                         {},
	"submitted url returned 403":                            {},
	"submitted url has crawl issue":                         {},
}

// googleStateLabel is the human-readable Google state for the discrepancy alert
// body: the coverage state if present, else the indexing state, else the verdict.
func googleStateLabel(idx model.URLIndexStatus) string {
	if s := strings.TrimSpace(idx.CoverageState); s != "" {
		return s
	}
	if s := strings.TrimSpace(idx.IndexingState); s != "" {
		return s
	}
	return strings.TrimSpace(idx.Verdict)
}

// ── canonical comparison ──────────────────────────────────────────────────────

// canonicalEquivalent reports whether two canonical URL strings denote the same
// resource for the purpose of the google_canonical_mismatch signal, so a benign
// variant (http↔https, www↔apex, trailing-slash, host-case, %-escape, default-port,
// dot-segment) does NOT false-positive a mismatch — the signal must fire only on a
// genuine cross-page difference (a different path or query, the forms a "Google chose
// a different canonical PAGE" alert is actually about). An unparseable string compares
// verbatim.
//
// urlx.Normalize alone is INSUFFICIENT here: it folds host-case / default-port /
// %-escapes / dot-segments, but it deliberately does NOT fold scheme (http vs https),
// the www↔apex host pair, or a trailing slash — by design, because for a crawl/link
// IDENTITY key those distinctions can matter. For a canonical-mismatch comparison they
// are noise: Google routinely reports the https/apex/slash-canonical form of a page
// whose declared canonical uses the http/www/slashless form, and that is the SAME page,
// not a mismatch. canonicalKey therefore layers the three extra folds on top of
// urlx.Normalize, while leaving the PATH (minus a trailing slash) and the QUERY
// identity-significant — so "/a" vs "/b", or "/p?utm=x" vs "/p", still fire.
func canonicalEquivalent(a, b string) bool {
	return canonicalKey(a) == canonicalKey(b)
}

// canonicalKey folds a canonical URL string to a comparison key that is invariant
// under the benign variants enumerated on canonicalEquivalent. It starts from
// urlx.Normalize (host-case, default-port, %-escapes, dot-segments) and then, for the
// mismatch comparison only, additionally folds:
//
//   - scheme: http and https collapse to one keyspace (a scheme flip is never a
//     different canonical page).
//   - host: a single leading "www." label is stripped when a registrable host remains
//     (apex↔www are the same site — the same rule urlx.SameSite/stripWWW applies), so
//     "www.x.com/p" and "x.com/p" key identically.
//   - path: a single trailing "/" is removed (but the root "/" is preserved), so
//     "/p" and "/p/" key identically. The query is left untouched and stays
//     identity-significant.
//
// On a parse failure it returns the raw string verbatim, so two identical raw strings
// still compare equal.
func canonicalKey(raw string) string {
	n, err := urlx.Normalize(raw)
	if err != nil {
		return raw
	}
	u, perr := url.Parse(n)
	if perr != nil {
		return n
	}
	// Scheme flip is not a different canonical page: collapse http/https to one key.
	switch u.Scheme {
	case "http", "https":
		u.Scheme = "https"
	}
	// apex↔www are the same site (mirrors urlx stripWWW: only strip when a registrable
	// host — one carrying a further dot — remains, so "www.com" is left intact).
	u.Host = stripCanonicalWWW(u.Host)
	// A trailing slash is not a different page; the root "/" is preserved. Operate on
	// the escaped path so an encoded %2F is never mistaken for a separator, then store
	// it back via RawPath so u.String() emits the trimmed form verbatim.
	escPath := u.EscapedPath()
	if len(escPath) > 1 && strings.HasSuffix(escPath, "/") {
		trimmed := strings.TrimRight(escPath, "/")
		if trimmed == "" {
			trimmed = "/"
		}
		if dec, derr := url.PathUnescape(trimmed); derr == nil {
			u.Path = dec
			u.RawPath = trimmed
		}
	} else if u.Path == "" {
		// Normalize leaves a slashless authority-only URL ("https://x.com") with an
		// empty path; fold it to root so it keys with "https://x.com/".
		u.Path = "/"
	}
	return u.String()
}

// stripCanonicalWWW removes a single leading "www." label from a (already lowercased
// by urlx.Normalize) host authority, but only when at least one more dot remains so a
// registrable host is left — mirroring urlx's stripWWW. The authority may carry a
// ":port"; the label check is a pure prefix test, which is safe because the port (if
// any) is a suffix.
func stripCanonicalWWW(host string) string {
	const p = "www."
	if strings.HasPrefix(host, p) {
		if rest := host[len(p):]; strings.Contains(rest, ".") {
			return rest
		}
	}
	return host
}
