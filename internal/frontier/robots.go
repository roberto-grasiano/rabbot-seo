// Package frontier enforces per-host politeness (rate + concurrency) and
// robots.txt allow/disallow/crawl-delay (RFC 9309, crawl-delay honored).
package frontier

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
)

// defaultRobotsClient builds an HTTP client with explicit timeouts for fetching
// robots.txt. http.DefaultClient is never used here because its Timeout is 0
// (no timeout): a slow/malicious origin could otherwise hang the robots fetch
// — and the goroutine calling Allowed/CrawlDelay — indefinitely.
func defaultRobotsClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
		},
	}
}

// negativeTTL bounds how long an error-derived verdict (transport error, 4xx,
// or 5xx full-disallow) is cached. A short window keeps a transient blip from
// pinning a host into a wrong allow/disallow posture for the full recheck TTL.
const negativeTTL = 20 * time.Second

type robotsEntry struct {
	group       *robotstxt.Group
	disallowAll bool // 5xx => full-disallow per RFC-9309 (group carries no rules)
	fetchedAt   time.Time
	expiresAt   time.Time // fetchedAt + (ttl for success, negativeTTL for errors)
	raw         []byte
	status      int
	sitemaps    []string
}

// inflight coordinates a single robots.txt fetch for one origin so concurrent
// first-hits collapse into one request (single-flight) instead of a herd.
type inflight struct {
	done chan struct{}
}

// RobotsCache fetches and caches per-host robots.txt with a TTL (5-min recheck).
type RobotsCache struct {
	client    *http.Client
	userAgent string
	// userAgentFunc, when non-nil, computes the per-host robots.txt User-Agent
	// (keyed on the origin host) instead of the static userAgent. The daemon wires
	// it so a robots.txt fetch carries the same per-site trust signal the page and
	// sitemap fetches do; the matched group (FindGroup) follows the same UA so the
	// rules applied match the UA we present. Set once at startup via
	// SetUserAgentFunc, before any fetch — read-only thereafter, so it needs no lock.
	userAgentFunc func(host string) string
	ttl           time.Duration

	mu       sync.Mutex
	hosts    map[string]robotsEntry // keyed by scheme://host
	fetching map[string]*inflight   // in-flight robots.txt fetches, keyed by origin
}

// SetUserAgentFunc installs a per-host User-Agent resolver used for robots.txt
// fetches (and the matched robots group). It must be called once at startup,
// before the cache serves any request — it is not safe to call concurrently with
// Allowed/CrawlDelay/Sitemaps/Raw. A nil func leaves the static userAgent in use.
func (rc *RobotsCache) SetUserAgentFunc(f func(host string) string) {
	rc.userAgentFunc = f
}

// uaForHost resolves the User-Agent for a robots.txt request/match against host,
// preferring the per-host func when wired, else the static userAgent.
func (rc *RobotsCache) uaForHost(host string) string {
	if rc.userAgentFunc != nil {
		return rc.userAgentFunc(host)
	}
	return rc.userAgent
}

// NewRobotsCache builds a cache. ttl is the recheck window (default 5m per §2).
func NewRobotsCache(client *http.Client, userAgent string, ttl time.Duration) *RobotsCache {
	if client == nil {
		client = defaultRobotsClient()
	}
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	return &RobotsCache{
		client:    client,
		userAgent: userAgent,
		ttl:       ttl,
		hosts:     map[string]robotsEntry{},
		fetching:  map[string]*inflight{},
	}
}

func originOf(rawURL string) (string, *url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, err
	}
	return u.Scheme + "://" + u.Host, u, nil
}

// entryFor returns the (possibly freshly fetched) cached robots verdict for the
// origin of rawURL. Concurrent first-hits on one origin collapse into a single
// fetch (single-flight): the first goroutine fetches while the rest wait on the
// in-flight signal and then read the freshly-cached entry.
func (rc *RobotsCache) entryFor(ctx context.Context, rawURL string) (robotsEntry, *url.URL, error) {
	origin, u, err := originOf(rawURL)
	if err != nil {
		return robotsEntry{}, nil, err
	}

	for {
		rc.mu.Lock()
		entry, ok := rc.hosts[origin]
		if ok && time.Now().Before(entry.expiresAt) {
			rc.mu.Unlock()
			return entry, u, nil
		}
		// Stale or absent. If another goroutine is already fetching this
		// origin, wait for it and re-read the cache instead of piling on.
		if fl, busy := rc.fetching[origin]; busy {
			rc.mu.Unlock()
			select {
			case <-fl.done:
				continue // loop: read the entry the leader just stored
			case <-ctx.Done():
				return robotsEntry{}, nil, ctx.Err()
			}
		}
		// We are the leader: publish an in-flight marker and fetch unlocked.
		fl := &inflight{done: make(chan struct{})}
		rc.fetching[origin] = fl
		rc.mu.Unlock()

		entry = rc.fetchEntry(ctx, rawURL)

		rc.mu.Lock()
		rc.hosts[origin] = entry
		delete(rc.fetching, origin)
		rc.mu.Unlock()
		close(fl.done)
		return entry, u, nil
	}
}

// fetchEntry performs the unlocked robots.txt fetch + parse and classifies the
// verdict per RFC-9309 / Google's spec, choosing the cache lifetime:
//   - 2xx (and parse-ok): real rules, cached for the full recheck TTL.
//   - 5xx: temporary error => full-disallow, cached only for negativeTTL.
//   - transport error / 4xx (incl. 404/410): no rules => allow-all, cached only
//     for negativeTTL so a transient blip doesn't pin allow-all for the full TTL.
func (rc *RobotsCache) fetchEntry(ctx context.Context, rawURL string) robotsEntry {
	now := time.Now()
	raw, status, ferr := rc.Raw(ctx, rawURL)

	// Match the robots group against the SAME UA we present on the fetch (per-host
	// when wired, else static) so the applied rules correspond to our identity.
	ua := rc.userAgent
	if _, u, oerr := originOf(rawURL); oerr == nil && u != nil {
		ua = rc.uaForHost(u.Hostname())
	}

	switch {
	case ferr != nil:
		// Genuine transport error: fail open per polite-crawler convention,
		// but only for a short window (negative TTL).
		data, _ := robotstxt.FromBytes([]byte(""))
		return robotsEntry{
			group:     data.FindGroup(ua),
			fetchedAt: now,
			expiresAt: now.Add(negativeTTL),
			raw:       raw,
			status:    status,
		}
	case status >= 500 && status < 600:
		// 5xx is a temporary error => full-disallow (RFC-9309 / Google spec).
		// Cache only for the short negative TTL so a transient 503 doesn't pin
		// the host into disallow-all (or, previously, allow-all) for the full TTL.
		return robotsEntry{
			group:       &robotstxt.Group{},
			disallowAll: true,
			fetchedAt:   now,
			expiresAt:   now.Add(negativeTTL),
			raw:         raw,
			status:      status,
		}
	case status >= 400 && status < 500:
		// 4xx (incl. 404/410): no valid robots.txt => allow-all, but short TTL.
		data, _ := robotstxt.FromBytes([]byte(""))
		return robotsEntry{
			group:     data.FindGroup(ua),
			fetchedAt: now,
			expiresAt: now.Add(negativeTTL),
			raw:       raw,
			status:    status,
		}
	default:
		// Success (2xx) or any other non-error status: parse real rules and
		// cache for the full recheck TTL.
		parsed, perr := robotstxt.FromBytes(raw)
		if perr != nil {
			parsed, _ = robotstxt.FromBytes([]byte(""))
		}
		return robotsEntry{
			group:     parsed.FindGroup(ua),
			fetchedAt: now,
			expiresAt: now.Add(rc.ttl),
			raw:       raw,
			status:    status,
			sitemaps:  parsed.Sitemaps,
		}
	}
}

// Allowed reports whether rawURL may be fetched per robots.txt.
func (rc *RobotsCache) Allowed(ctx context.Context, rawURL string) bool {
	entry, u, err := rc.entryFor(ctx, rawURL)
	if err != nil {
		return true
	}
	if entry.disallowAll {
		return false
	}
	if entry.group == nil {
		return true
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return entry.group.Test(path)
}

// CrawlDelay returns the crawl-delay declared for our UA (0 if none).
func (rc *RobotsCache) CrawlDelay(ctx context.Context, rawURL string) time.Duration {
	entry, _, err := rc.entryFor(ctx, rawURL)
	if err != nil || entry.group == nil {
		return 0
	}
	return entry.group.CrawlDelay
}

// Sitemaps returns the Sitemap: URLs declared in the origin's robots.txt
// (empty if none / unreachable). Used by discovery to find the real sitemap,
// which is often a non-default path or a sitemap index.
func (rc *RobotsCache) Sitemaps(ctx context.Context, rawURL string) []string {
	entry, _, err := rc.entryFor(ctx, rawURL)
	if err != nil {
		return nil
	}
	return entry.sitemaps
}

// Raw fetches the raw robots.txt bytes + HTTP status for a site (used by file_snapshots).
func (rc *RobotsCache) Raw(ctx context.Context, rawURL string) ([]byte, int, error) {
	origin, u, err := originOf(rawURL)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", rc.uaForHost(u.Hostname()))
	resp, err := rc.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
