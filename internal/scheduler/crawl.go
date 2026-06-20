package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

// CrawlStore is the subset of store.Store the crawl pipeline needs. The daemon
// passes the concrete *store.DB, which satisfies this interface.
type CrawlStore interface {
	SaveSnapshot(ctx context.Context, snap model.Snapshot) (int64, error)
	LatestSnapshot(ctx context.Context, urlID int64) (model.Snapshot, error)
	RecordChanges(ctx context.Context, changes []model.Change) error
	UpdateURLSchedule(ctx context.Context, id int64, nextCheckAt time.Time, interval int64, lastFetch model.FetchClass, etag, lastModified string) error
	// GetSite resolves the owning site for the M2 post-fetch processor (alert
	// site labels / fingerprints). M1-owned method on *store.DB.
	GetSite(ctx context.Context, id int64) (model.Site, error)
}

// Crawler orchestrates a single-URL crawl: frontier gate -> robots -> fetch ->
// (ok only) extract + persist + reschedule. Non-ok fetches skip extraction and
// snapshot persistence entirely (§5A), recording only last_fetch_class.
type Crawler struct {
	Store     CrawlStore
	Fetcher   fetcher.Fetcher
	Extractor extract.Extractor
	Robots    *frontier.RobotsCache
	Frontier  *frontier.Frontier
	Now       func() time.Time

	// Hydration carries the A8 hydration-recovery knobs (crawler.hydration.*) into
	// the extractor. The daemon (run.go) builds it from config; its zero value
	// (Enabled=false) means "no payload recovery, extraction is byte-identical to
	// pre-A8" — exactly the M1-only / unit-test default, so those paths need no
	// wiring. CrawlOne folds it into the per-crawl extract.Options.
	Hydration extract.HydrationOptions

	// Logger emits one INFO heartbeat line per crawled URL on the success path.
	// Nil in unit tests / M1-only paths, where logging is skipped.
	Logger *slog.Logger

	// Metrics is the daemon's self-observability layer. CrawlOne records one
	// rabbot_fetches_total{class} + a fetch-duration sample per page fetch. A nil
	// *Metrics no-ops, so M1-only paths and unit tests need no wiring.
	Metrics *obs.Metrics

	// Processor runs the M2 post-fetch stage (access gate -> diff -> rules ->
	// alerts). Nil in M1-only paths and unit tests, where CrawlOne stops after
	// persisting the snapshot.
	Processor *Processor

	// Discoverer, when set, enqueues newly-discovered same-host links after a
	// successful extract (bounded link-following). Nil = no link discovery.
	Discoverer interface {
		EnqueueLinks(ctx context.Context, site model.Site, parent model.URL, links []string) (int, error)
	}

	// Graph, when set, replaces this page's out-edge set in the link graph after a
	// successful extract (A9 link-graph LITE), receiving the SAME deduped, absolute,
	// fragment-stripped same-host link slice the Discoverer gets. It is invoked at
	// the same post-extract point as Discoverer and on the FetchOK path ONLY — never
	// on a non-ok / 304 / extract-error fetch (no body means no current out-set).
	// Nil = the feature is OFF: not wiring Graph in run.go is the scope-gate
	// severability (a no-wiring decision, not a revert) — CrawlOne simply no-ops it.
	// The interface is structural so this package never imports internal/linkgraph.
	Graph interface {
		SyncPage(ctx context.Context, site model.Site, u model.URL, links []string) error
	}
}

// CrawlResult summarizes one crawl for the daemon loop and CLI crawl command.
type CrawlResult struct {
	URLID      int64
	FetchClass model.FetchClass
	HTTPStatus int
	Skipped    string // "" if crawled; otherwise "robots_disallowed" or "frontier"
	Detector   string
	Err        error
}

func (c *Crawler) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// CrawlOne fetches one URL through the full M1 pipeline.
func (c *Crawler) CrawlOne(ctx context.Context, u model.URL, minInterval, maxInterval int64, contentSelector string) CrawlResult {
	res := CrawlResult{URLID: u.ID}

	if !c.Robots.Allowed(ctx, u.URL) {
		res.Skipped = "robots_disallowed"
		// F5: still advance the schedule, or this URL stays permanently due and is
		// re-popped every tick (busy loop). robots.txt is near-static, so a long
		// cadence (maxInterval) is appropriate — we re-check occasionally in case
		// the disallow is lifted, without spinning. Validators are carried forward.
		robotsAt := c.now().Add(time.Duration(maxInterval) * time.Second)
		c.advanceSchedule(ctx, u, maxInterval, robotsAt, u.LastFetchClass, &res)
		return res
	}

	host := urlx.Host(u.URL)
	// F28: honor robots.txt Crawl-delay. The site's advertised delay becomes a hard
	// per-host spacing floor applied BEFORE we acquire the slot, so Acquire blocks at
	// least that long between requests. SetMinInterval only ever raises the floor.
	if d := c.Robots.CrawlDelay(ctx, u.URL); d > 0 {
		c.Frontier.SetMinInterval(host, d)
	}
	release, err := c.Frontier.Acquire(ctx, host)
	if err != nil {
		res.Skipped = "frontier"
		res.Err = err
		// F5: a frontier Acquire failure (e.g. shutdown/cancel) is transient; still
		// advance the schedule on the normal growth curve so the URL is not re-popped
		// every tick while the condition persists.
		nextInterval, nextAt := RecomputeNextCheck(u.Interval, false, minInterval, maxInterval, c.now())
		c.advanceSchedule(ctx, u, nextInterval, nextAt, u.LastFetchClass, &res)
		return res
	}
	defer release()

	fres, _ := c.Fetcher.Fetch(ctx, fetcher.Request{URL: u.URL, ETag: u.ETag, LastMod: u.LastModified})
	// F11: a plain 5xx with a body classifies as FetchOK (so it IS snapshotted and the
	// status_regression rule can fire), but isBackoffClass(FetchOK) is false. Treat any
	// 5xx as a back-off signal so a failing origin is slowed down, not sped up.
	serverError := fres.HTTPStatus >= 500
	// §5A: unreachable AND block classes must back the host off. Soft/hard blocks
	// often return a tiny body fast, so latency alone never triggers the throttle —
	// signal them explicitly so 429/503/403 slow the crawler instead of speeding it up.
	c.Frontier.Report(host, fres.ResponseTime, isBackoffClass(fres.FetchClass) || serverError)

	// Self-observability: record this page fetch by its access class plus its
	// wall-clock duration (page fetches only in v1 — robots/sitemap fetches are
	// not instrumented). Closed-enum class label only; nil Metrics no-ops.
	c.Metrics.ObserveFetch(string(fres.FetchClass), fres.ResponseTime)

	res.FetchClass = fres.FetchClass
	res.HTTPStatus = fres.HTTPStatus
	res.Detector = fres.Detector

	// changed tracks whether the M2 processor detected a substantive change, so the
	// adaptive recheck cadence shrinks on change (and only grows while stable).
	var changed bool

	// §5A: only ok-class fetches with a body get extracted + snapshotted.
	if fres.FetchClass == model.FetchOK && !fres.NotModified && len(fres.Body) > 0 {
		snap, links, eerr := c.Extractor.ExtractWith(fres, extract.Options{
			ContentSelector: contentSelector,
			Hydration:       c.Hydration,
		})
		if eerr == nil {
			snap.URLID = u.ID
			// Capture the prior snapshot BEFORE persisting the new one: the M2
			// processor diffs new-vs-prior, but LatestSnapshot would return the
			// row we are about to save. A missing prior (first fetch) yields the
			// zero Snapshot, which diff.Compare treats as a baseline (no changes).
			old, lerr := c.Store.LatestSnapshot(ctx, u.ID)
			id, serr := c.Store.SaveSnapshot(ctx, snap)
			switch {
			case serr != nil:
				res.Err = serr
			case lerr != nil && !errors.Is(lerr, store.ErrNotFound):
				// A real read error (not the "no prior snapshot" sentinel): surface
				// it and SKIP the M2 processor. Diffing against the zero snapshot
				// would mis-record this fetch as a baseline and silently drop real
				// changes. (ErrNotFound is the legitimate first-fetch baseline.)
				res.Err = lerr
			default:
				snap.ID = id
				ch, perr := c.process(ctx, u, snap, old, fres)
				changed = ch
				if perr != nil && res.Err == nil {
					res.Err = perr
				}
				// Discoverer (link-following) and Graph (A9 link-graph) both consume
				// the SAME extracted same-host link slice on the FetchOK path. Resolve
				// the owning site once when either hook is wired (skip the GetSite when
				// neither is). Each hook is independently nil-able; a nil Graph is the
				// A9 scope-gate severability (CrawlOne no-ops it).
				if c.Discoverer != nil || c.Graph != nil {
					site, gerr := c.Store.GetSite(ctx, u.SiteID)
					if gerr == nil {
						if c.Discoverer != nil {
							if _, derr := c.Discoverer.EnqueueLinks(ctx, site, u, links); derr != nil && res.Err == nil {
								res.Err = derr
							}
						}
						if c.Graph != nil {
							if gserr := c.Graph.SyncPage(ctx, site, u, links); gserr != nil && res.Err == nil {
								res.Err = gserr
							}
						}
					}
				}
			}
		} else {
			res.Err = eerr
			// Degrade honestly on an unparseable DOM: x/net/html refuses HTML nested
			// deeper than its parser limit, so this page can never be extracted or
			// snapshotted. Without a distinct signal it would be an invisible blind
			// spot — fetched forever, no diff, no notice. Emit a WARN naming the URL
			// and a distinct reason so an operator can see it, rather than letting it
			// masquerade as a generic transient error.
			if errors.Is(eerr, extract.ErrDOMTooDeep) && c.Logger != nil {
				c.Logger.Warn("extract skipped: unparseable DOM",
					obs.KeyComponent, "scheduler",
					obs.KeyURL, u.URL,
					obs.KeyHTTPStatus, fres.HTTPStatus,
					"reason", "unparseable_dom",
					obs.KeyError, eerr.Error(),
				)
			}
		}
	} else if c.Processor != nil && (fres.FetchClass != model.FetchOK || fres.NotModified) {
		// A non-ok fetch persists no snapshot but still drives the access gate so
		// SEO emission is suppressed and the operational incident
		// (monitoring_blocked / monitoring_unreachable) is raised/maintained. A 304
		// (ok + NotModified, no body) also runs the gate so an open operational
		// incident auto-closes on a recovery delivered via 304 rather than waiting
		// for the 24h sweep; the processor diffs nothing for a zero snapshot.
		if _, perr := c.process(ctx, u, model.Snapshot{}, model.Snapshot{}, fres); perr != nil && res.Err == nil {
			res.Err = perr
		}
	}

	// Carry forward conditional-GET validators. On a 304 (NotModified) the server
	// sends no validators, so keep the ones we already had; otherwise adopt the
	// response's ETag/Last-Modified so the next crawl can issue a conditional GET.
	etag, lastMod := u.ETag, u.LastModified
	if fres.Header != nil && !fres.NotModified {
		if v := fres.Header.Get("ETag"); v != "" {
			etag = v
		}
		if v := fres.Header.Get("Last-Modified"); v != "" {
			lastMod = v
		}
	}

	// Reschedule. Blocked fetches (soft/hard) get an extended back-off instead of
	// the normal growth curve; soft blocks additionally honor any Retry-After. The
	// M2 processor reports whether a substantive change was detected (changed): on a
	// change the interval shrinks toward minInterval (check volatile pages more
	// often), while stable pages grow toward maxInterval.
	nextInterval, nextAt := RecomputeNextCheck(u.Interval, changed, minInterval, maxInterval, c.now())
	if isBackoffClass(fres.FetchClass) || serverError {
		nextInterval, nextAt = backoffSchedule(u.Interval, minInterval, maxInterval, fres, c.now())
	}
	if serr := c.Store.UpdateURLSchedule(ctx, u.ID, nextAt, nextInterval, fres.FetchClass, etag, lastMod); serr != nil && res.Err == nil {
		// Surface the schedule write error; if it is dropped, next_check_at stays
		// in the past and the URL is re-popped every tick (silent tight loop).
		res.Err = serr
	}
	if c.Logger != nil {
		c.Logger.Info("crawled",
			obs.KeyComponent, "scheduler",
			obs.KeyURL, u.URL,
			obs.KeyHTTPStatus, res.HTTPStatus,
			obs.KeyFetchClass, string(res.FetchClass),
			"not_modified", fres.NotModified,
			"changed", changed,
			obs.KeyDurationMS, fres.ResponseTime.Milliseconds(),
		)
	}
	return res
}

// advanceSchedule writes a next_check_at/interval for a crawl that ended on an
// early skip path (robots-disallowed / frontier) so the URL is not re-popped
// every tick. It carries forward the URL's existing conditional-GET validators
// and last fetch class, and surfaces any write error on res.Err if not already set.
func (c *Crawler) advanceSchedule(ctx context.Context, u model.URL, interval int64, nextAt time.Time, lastFetch model.FetchClass, res *CrawlResult) {
	if serr := c.Store.UpdateURLSchedule(ctx, u.ID, nextAt, interval, lastFetch, u.ETag, u.LastModified); serr != nil && res.Err == nil {
		res.Err = serr
	}
}

// process runs the M2 post-fetch stage when a Processor is wired. It resolves
// the owning site and hands off to ProcessFetch (access gate -> diff -> rules ->
// alerts), returning whether a substantive change was detected (to drive the
// adaptive cadence) plus any processing error for the caller to surface on res.Err
// (without aborting the crawl — the snapshot is already persisted and the
// schedule must still advance). A nil Processor (M1-only paths / unit tests)
// makes this a no-op that reports no change.
func (c *Crawler) process(ctx context.Context, u model.URL, newSnap, oldSnap model.Snapshot, fres fetcher.Result) (bool, error) {
	if c.Processor == nil {
		return false, nil
	}
	site, err := c.Store.GetSite(ctx, u.SiteID)
	if err != nil {
		return false, err
	}
	return c.Processor.ProcessFetch(ctx, site, u, newSnap, oldSnap, fres.FetchClass, fres.Detector, fres.Truncated)
}

// isBackoffClass reports whether a fetch class should slow the host down: an
// unreachable error or an explicit soft/hard block (§5A).
func isBackoffClass(fc model.FetchClass) bool {
	switch fc {
	case model.FetchUnreachable, model.FetchSoftBlock, model.FetchHardBlock:
		return true
	default:
		return false
	}
}

// backoffSchedule computes an extended next-check for a blocked/unreachable URL.
// It at least doubles the interval (clamped to [min,max]); for a soft block it
// honors Retry-After when that pushes the next check out further.
func backoffSchedule(prevInterval, minInterval, maxInterval int64, fres fetcher.Result, now time.Time) (int64, time.Time) {
	if prevInterval <= 0 {
		prevInterval = minInterval
	}
	next := prevInterval * 2
	if next < minInterval {
		next = minInterval
	}
	if next > maxInterval {
		next = maxInterval
	}
	at := now.Add(time.Duration(next) * time.Second)
	if fres.FetchClass == model.FetchSoftBlock && fres.Header != nil {
		if d, ok := parseRetryAfter(fres.Header.Get("Retry-After"), now); ok {
			if ra := now.Add(d); ra.After(at) {
				at = ra
			}
		}
	}
	return next, at
}

// parseRetryAfter parses a Retry-After header value, which may be either a
// delay in seconds or an HTTP-date. It returns the delay from now.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true // a past date means "retry now", but still a valid signal
	}
	return 0, false
}
