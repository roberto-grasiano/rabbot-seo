// Package discovery finds the pages of a site to monitor: recursive sitemap
// expansion (robots Sitemap: directive, index, gzip) and bounded same-host
// link-following. It is the single owner of every "don't over-fetch" bound.
package discovery

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/scheduler"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

const (
	maxSitemapDepth   = 3
	maxSitemapFetches = 50
)

// Caps is the resolved per-site discovery budget.
type Caps struct {
	FollowLinks bool
	Sitemap     bool
	MaxDepth    int
	MaxPages    int
}

// Store is the subset of *store.DB discovery needs.
type Store interface {
	UpsertURL(ctx context.Context, u model.URL) (int64, error)
	GetURL(ctx context.Context, siteID int64, url string) (model.URL, error)
	CountSiteURLs(ctx context.Context, siteID int64) (int, error)
}

// frontierGate is the subset of *frontier.Frontier discovery needs: the per-host
// rate + concurrency gate. The concrete *frontier.Frontier satisfies it. It is
// declared here (not consumed as *frontier.Frontier directly) so the optional
// Frontier field stays injectable for tests without a live frontier.
type frontierGate interface {
	Acquire(ctx context.Context, host string) (release func(), err error)
}

// Discoverer owns sitemap + link discovery and every bound. Pages is the page
// fetcher; Sitemaps is a fetcher with a larger body limit (sitemaps can be big).
type Discoverer struct {
	Store    Store
	Pages    fetcher.Fetcher
	Sitemaps fetcher.Fetcher
	Robots   *frontier.RobotsCache
	Resolve  func(model.Site) Caps
	Now      func() time.Time
	Logger   *slog.Logger

	// Classify, when set, is invoked once per NEWLY admitted URL (right after its
	// upsert assigns a urlID) to write that URL's segment memberships at entry —
	// the A7 classify seam. It is given the new urlID and URL. A deduped
	// (already-present) URL never reaches upsert, so it is not re-classified.
	// nil = no segment classification (back-compat for unit tests and pre-A7
	// callers). A classify error is logged at debug and never aborts admission:
	// reconcile re-classifies the whole site, so a transient miss self-heals.
	Classify func(ctx context.Context, siteID, urlID int64, url string) error

	// Frontier, when set, gates each sitemap fetch through the daemon's per-host
	// rate + concurrency budget (mirroring the page-crawl path) so a BFS over a
	// site's declared sitemaps cannot issue up to maxSitemapFetches ungated
	// requests to one host, ignoring its crawl-delay / per-host rate. Nil = no
	// gating (back-compat for unit tests and standalone discovery).
	Frontier frontierGate

	// siteLocks serializes the count->admit->upsert critical section per site so
	// concurrent same-site discovery (sitemap seeding racing link-following, or two
	// link-discoveries) cannot read a stale count and overshoot MaxPages. Keyed by
	// site.ID -> *sync.Mutex; lazily populated, zero-value ready (no constructor).
	siteLocks sync.Map
}

// lockFor returns the per-site mutex for siteID, lazily creating it on first use.
// LoadOrStore makes concurrent first-callers converge on a single *sync.Mutex.
func (d *Discoverer) lockFor(siteID int64) *sync.Mutex {
	v, _ := d.siteLocks.LoadOrStore(siteID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (d *Discoverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *Discoverer) log(msg, site string, n int) {
	if d.Logger != nil {
		d.Logger.Info(msg, obs.KeyComponent, "discovery", "site", site, "count", n)
	}
}

func (d *Discoverer) debug(msg, site, url string) {
	if d.Logger != nil {
		d.Logger.Debug(msg, obs.KeyComponent, "discovery", "site", site, obs.KeyURL, url)
	}
}

// EnqueueLinks admits new same-host links discovered on parent (depth+1), bounded
// by FollowLinks, MaxDepth and MaxPages. Best-effort: per-link issues are skipped.
func (d *Discoverer) EnqueueLinks(ctx context.Context, site model.Site, parent model.URL, links []string) (int, error) {
	caps := d.Resolve(site)
	if !caps.FollowLinks || parent.Depth >= caps.MaxDepth {
		return 0, nil
	}

	// Hold the per-site lock around the entire count->admit->upsert loop: the cap
	// check reads CountSiteURLs and the upserts mutate it, so a concurrent
	// same-site discovery must not interleave between the two or both could admit
	// past MaxPages.
	mu := d.lockFor(site.ID)
	mu.Lock()
	defer mu.Unlock()

	count, err := d.Store.CountSiteURLs(ctx, site.ID)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, raw := range links {
		// MaxPages <= 0 means unlimited (mirrors the BFS guard below and
		// config's "0 = unlimited" contract): without the >0 guard, 0 >= 0
		// would break on the first iteration and admit nothing.
		if caps.MaxPages > 0 && count+added >= caps.MaxPages {
			d.log("link discovery hit page cap", site.BaseURL, caps.MaxPages)
			break
		}
		ok, aerr := d.admit(ctx, site, raw)
		if aerr != nil {
			return added, aerr
		}
		if !ok {
			continue
		}
		// Link-discovered pages carry no sitemap priority signal: pass 0 (not the
		// sitemap-protocol 0.5 default) so cold-start importance reflects depth alone.
		if d.upsert(ctx, site, raw, parent.Depth+1, false, 0) {
			added++
		}
	}
	return added, nil
}

// SeedSitemaps does a bounded BFS over the site's declared sitemaps (robots
// Sitemap: directive, else <base>/sitemap.xml), expanding indexes, gzip-aware.
// It is a thin back-compat wrapper over CollectAndSeed, returning only the count
// of newly-admitted URLs (the value it has always returned). New callers that
// also need the collected set / seed status / completeness (the sitemap watch)
// use CollectAndSeed directly.
func (d *Discoverer) SeedSitemaps(ctx context.Context, site model.Site) (int, error) {
	col, err := d.CollectAndSeed(ctx, site)
	return col.Admitted, err
}

// CollectAndSeed runs one bounded collection pass over the site's declared
// sitemaps and returns both the collected URL set and the admission result, so a
// single fetch pass feeds discovery AND the sitemap watch (snapshot/diff/
// reconcile). It is the same bounded BFS SeedSitemaps always ran — it builds
// pages []scheduler.SitemapEntry and admits the new ones — but it no longer
// discards the set: it surfaces it (plus the primary seed's identity/status and
// whether the pass was incomplete) in a scheduler.SitemapCollection.
//
// Bounds are unchanged: maxSitemapDepth, maxSitemapFetches, and ApplyBudget all
// still apply exactly as before. Incomplete is set whenever a document fetch
// fails or a body is truncated mid-BFS (the same continue branches), so a partial
// read can never masquerade downstream as a clean, complete set. SeedStatus is
// the HTTP status of the primary seed document (the first seed), with 0 meaning
// a network error (or that the primary seed was never fetched: filtered by
// validation/robots, or the collection was disabled).
func (d *Discoverer) CollectAndSeed(ctx context.Context, site model.Site) (scheduler.SitemapCollection, error) {
	caps := d.Resolve(site)
	if !caps.Sitemap {
		return scheduler.SitemapCollection{}, nil
	}
	seeds := d.Robots.Sitemaps(ctx, site.BaseURL)
	if len(seeds) == 0 {
		seeds = []string{strings.TrimRight(site.BaseURL, "/") + "/sitemap.xml"}
	}
	// The primary seed (criterion: per-seed status is fast-follow) is the first
	// declared seed; its fetched HTTP status is the watch's "sitemap broke" signal.
	primarySeed := seeds[0]
	col := scheduler.SitemapCollection{SeedURL: primarySeed}

	type item struct {
		url   string
		level int
	}
	queue := make([]item, 0, len(seeds))
	for _, s := range seeds {
		queue = append(queue, item{s, 0})
	}
	// visited dedups sitemap URLs across the BFS so a cyclic or self-referential
	// index (A -> B -> A) cannot spend a fetch on a document already processed.
	visited := make(map[string]struct{}, len(seeds))
	var pages []scheduler.SitemapEntry
	fetches := 0
	for len(queue) > 0 && fetches < maxSitemapFetches {
		// Stop the BFS the moment accumulation reaches the cap: transient memory
		// and the sort below must be bounded by MaxPages regardless of how large a
		// hostile sitemap set is. (MaxPages <= 0 means unbounded, so don't break.)
		if caps.MaxPages > 0 && len(pages) >= caps.MaxPages {
			break
		}
		it := queue[0]
		queue = queue[1:]
		if it.level > maxSitemapDepth || !d.inScope(it.url, site.BaseURL) {
			continue
		}
		if _, seen := visited[it.url]; seen {
			continue
		}
		visited[it.url] = struct{}{}
		if fetcher.ValidateSiteURL(it.url, d.Sitemaps.AllowsPrivate()) != nil || !d.Robots.Allowed(ctx, it.url) {
			continue
		}
		fetches++
		res, ferr := d.fetchSitemap(ctx, it.url)
		// Record the primary seed's HTTP status (0 = network error). A non-OK seed
		// (404/5xx) is the watch's accessibility-regression signal; capture it even
		// though the BFS then skips the body below. For a network error ferr != nil
		// and res.HTTPStatus is 0, which is exactly the 0-means-network-error contract.
		if it.url == primarySeed {
			col.SeedStatus = res.HTTPStatus
		}
		if ferr != nil || res.FetchClass != model.FetchOK || len(res.Body) == 0 {
			// A fetch that failed (or returned an empty/non-OK body) mid-BFS leaves the
			// collected set partial: mark it incomplete so a downstream reconcile never
			// reads a missing child as a mass URL drop.
			col.Incomplete = true
			continue
		}
		// A truncated body is a prefix of the real document: feeding it to the XML
		// parser would either fail or, worse, silently drop the entries past the cut.
		// Skip it rather than seed a partial/garbled set — and flag the whole pass
		// incomplete, since the missing tail is unknown.
		if res.Truncated {
			d.debug("sitemap fetch truncated, skipping", site.BaseURL, it.url)
			col.Incomplete = true
			continue
		}
		entries, isIndex, perr := scheduler.ParseSitemap(res.Body)
		if perr != nil {
			col.Incomplete = true
			continue
		}
		// Bound accumulation per document: never let one hostile urlset/index push
		// more than MaxPages entries into transient memory (and into the sort).
		entries = scheduler.ApplyBudget(entries, caps.MaxPages)
		if isIndex {
			for _, e := range entries {
				queue = append(queue, item{e.Loc, it.level + 1})
			}
			continue
		}
		pages = append(pages, entries...)
		pages = scheduler.ApplyBudget(pages, caps.MaxPages)
	}
	// The BFS exited with work still queued because it hit the fetch budget: the
	// declared set extends past what we fetched, so the collection is incomplete.
	if len(queue) > 0 && fetches >= maxSitemapFetches {
		col.Incomplete = true
	}
	sort.SliceStable(pages, func(i, j int) bool { return pages[i].Priority > pages[j].Priority })
	col.Entries = pages

	// Hold the per-site lock around the entire count->admit->upsert loop so a
	// concurrent same-site discovery (link-following, or a second sitemap seed)
	// cannot interleave between the cap check and the upserts and overshoot.
	mu := d.lockFor(site.ID)
	mu.Lock()
	defer mu.Unlock()

	count, err := d.Store.CountSiteURLs(ctx, site.ID)
	if err != nil {
		return col, err
	}
	added := 0
	for _, e := range pages {
		// MaxPages <= 0 means unlimited (mirrors the BFS guard above): without
		// the >0 guard, 0 >= 0 would break on the first iteration and seed nothing.
		if caps.MaxPages > 0 && count+added >= caps.MaxPages {
			d.log("sitemap discovery hit page cap", site.BaseURL, caps.MaxPages)
			break
		}
		ok, aerr := d.admit(ctx, site, e.Loc)
		if aerr != nil {
			col.Admitted = added
			return col, aerr
		}
		if !ok {
			continue
		}
		if d.upsert(ctx, site, e.Loc, 1, true, e.Priority) {
			added++
		}
	}
	col.Admitted = added
	return col, nil
}

// fetchSitemap fetches one sitemap document, gating it through the per-host
// frontier budget when one is wired so a BFS over a site's declared sitemaps
// respects the same per-host rate / crawl-delay as page crawls (it can otherwise
// issue up to maxSitemapFetches ungated requests to one host). The slot is
// acquired and released around this single fetch, mirroring the page-crawl path.
// A nil Frontier (standalone discovery / unit tests) fetches directly. An Acquire
// failure (e.g. ctx cancellation) returns the error so the BFS skips this URL.
func (d *Discoverer) fetchSitemap(ctx context.Context, rawURL string) (fetcher.Result, error) {
	if d.Frontier != nil {
		release, err := d.Frontier.Acquire(ctx, urlx.Host(rawURL))
		if err != nil {
			return fetcher.Result{}, err
		}
		defer release()
	}
	return d.Sitemaps.Fetch(ctx, fetcher.Request{URL: rawURL})
}

// inScope reports whether raw belongs to the crawl scope rooted at base. It is
// the single owner of discovery's scope question, shared by the link/loc admit
// gate and the sitemap-BFS document gate so both apply the SAME equivalence.
//
// The equivalence is urlx.SameSite, NOT urlx.SameHost: an apex host and its
// "www." sibling are the same site (#6). The fetcher already follows apex->www
// (and vice-versa), so a site configured at its apex whose canonical/links/
// sitemaps resolve to the www host has its homepage fetched — discovery must then
// keep those entries in scope rather than reject every one and report a 0-page
// site. SameSite is still NOT eTLD+1 collapsing (blog.example.com stays out), so
// an unrelated subdomain or a different registrable domain is rejected as before.
// (Exact-host SameHost remains the fetcher's contract for credential-stripping on
// cross-host redirects; that is a different question from crawl scope.)
func (d *Discoverer) inScope(raw, base string) bool {
	return urlx.SameSite(raw, base)
}

// admit applies the shared gauntlet (minus the cap, which callers track locally):
// in-scope (apex<->www site) -> SSRF -> dedup. Returns (false, nil) to skip,
// (false, err) on a real store error.
func (d *Discoverer) admit(ctx context.Context, site model.Site, raw string) (bool, error) {
	if !d.inScope(raw, site.BaseURL) {
		return false, nil
	}
	if fetcher.ValidateSiteURL(raw, d.Pages.AllowsPrivate()) != nil {
		return false, nil
	}
	_, err := d.Store.GetURL(ctx, site.ID, raw)
	switch {
	case err == nil:
		return false, nil // already in inventory — never re-upsert (would reset schedule)
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	default:
		return false, err
	}
}

func (d *Discoverer) upsert(ctx context.Context, site model.Site, raw string, depth int, inSitemap bool, pri float64) bool {
	urlID, err := d.Store.UpsertURL(ctx, model.URL{
		SiteID:         site.ID,
		URL:            raw,
		FirstSeen:      d.now(),
		NextCheckAt:    d.now(),
		Interval:       site.MinInterval,
		Importance:     scheduler.ColdStartImportance(false, depth, pri),
		Depth:          depth,
		InSitemap:      inSitemap,
		StatusType:     model.StatusPage,
		LastFetchClass: model.FetchOK,
	})
	if err != nil {
		return false
	}
	// A7: classify the newly admitted URL into its segments at entry. callers reach
	// upsert only for URLs that passed admit's dedup, so this runs once per new URL.
	// A classify miss is non-fatal — reconcile re-classifies the whole site — so the
	// URL still counts as admitted.
	if d.Classify != nil {
		if cerr := d.Classify(ctx, site.ID, urlID, raw); cerr != nil {
			d.debug("segment classify failed", site.BaseURL, raw)
		}
	}
	return true
}
