package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/extract"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/obs"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

func newPipeline(t *testing.T) (*Crawler, *fakeCrawlStore, func(urlID int64) (model.Snapshot, bool)) {
	t.Helper()
	saved := map[int64]model.Snapshot{}
	var latest = map[int64]model.Snapshot{}

	fc := &fakeCrawlStore{
		saved:             saved,
		latest:            latest,
		updated:           map[int64]model.FetchClass{},
		scheduledInterval: map[int64]int64{},
		scheduledNextAt:   map[int64]time.Time{},
		scheduledETag:     map[int64]string{},
	}
	c := &Crawler{
		Store:     fc,
		Fetcher:   fetcher.New(fetcher.Options{UserAgent: "Rabbot-SEO/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: true}),
		Extractor: extract.NewExtractor(),
		Robots:    frontier.NewRobotsCache(http.DefaultClient, "Rabbot-SEO/test", time.Minute),
		Frontier:  frontier.New(frontier.Options{PerHostRate: time.Microsecond, PerHostConcurrency: 4}),
		Now:       func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) },
	}
	return c, fc, func(urlID int64) (model.Snapshot, bool) {
		s, ok := saved[urlID]
		return s, ok
	}
}

func TestCrawlOKSavesSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte("<html><head><title>Hi</title></head><body><p>content words here today now</p></body></html>"))
	}))
	defer srv.Close()

	c, _, getSaved := newPipeline(t)
	u := model.URL{ID: 1, SiteID: 1, URL: srv.URL + "/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	snap, ok := getSaved(1)
	if !ok {
		t.Fatal("snapshot not saved for ok fetch")
	}
	if snap.Title != "Hi" {
		t.Errorf("Title = %q", snap.Title)
	}
}

func TestCrawlBlockedSkipsSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("Attention Required! | Cloudflare"))
	}))
	defer srv.Close()

	c, _, getSaved := newPipeline(t)
	u := model.URL{ID: 2, SiteID: 1, URL: srv.URL + "/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchHardBlock {
		t.Fatalf("FetchClass = %q, want hard_block", res.FetchClass)
	}
	if _, ok := getSaved(2); ok {
		t.Error("snapshot must be SKIPPED for non-ok fetch (§5A)")
	}
}

func TestCrawlRobotsDisallowedSkips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /page\n"))
			return
		}
		t.Errorf("disallowed URL must not be fetched")
	}))
	defer srv.Close()

	c, fc, getSaved := newPipeline(t)
	u := model.URL{ID: 3, SiteID: 1, URL: srv.URL + "/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.Skipped != "robots_disallowed" {
		t.Errorf("Skipped = %q, want robots_disallowed", res.Skipped)
	}
	if _, ok := getSaved(3); ok {
		t.Error("snapshot must not be saved for robots-disallowed URL")
	}
	// F5: a robots-disallowed URL must still have its schedule advanced strictly
	// past now, or PopDueURLs re-pops it every tick forever (busy loop).
	at, ok := fc.scheduledNextAt[3]
	if !ok {
		t.Fatal("robots-disallowed URL must still advance its schedule (UpdateURLSchedule)")
	}
	if !at.After(c.now()) {
		t.Errorf("next_check_at = %v, want strictly after now %v (else permanent re-pop)", at, c.now())
	}
}

// TestCrawlFrontierSkipAdvancesSchedule guards F5: a crawl that skips because the
// frontier Acquire fails (here: a cancelled context) must still advance the URL's
// schedule, or PopDueURLs re-pops it every tick forever. We cancel the context so
// robots fails open (Allowed==true) and Frontier.Acquire returns ctx.Err().
func TestCrawlFrontierSkipAdvancesSchedule(t *testing.T) {
	c, fc, _ := newPipeline(t)
	u := model.URL{ID: 7, SiteID: 1, URL: "https://e.com/page", Interval: 600}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := c.CrawlOne(ctx, u, 600, 86400, "")
	if res.Skipped != "frontier" {
		t.Fatalf("Skipped = %q, want frontier", res.Skipped)
	}
	at, ok := fc.scheduledNextAt[7]
	if !ok {
		t.Fatal("frontier-skipped URL must still advance its schedule (UpdateURLSchedule)")
	}
	if !at.After(c.now()) {
		t.Errorf("next_check_at = %v, want strictly after now %v (else permanent re-pop)", at, c.now())
	}
}

// TestCrawlSoftBlockBacksOff verifies a 429 soft block reschedules the URL with
// an extended interval (>= prev*2) that honors Retry-After, rather than the
// normal growth curve (issue 8). The frontier-throttle backoff itself (routing
// soft/hard blocks through hadError) is covered by frontier_test.go.
func TestCrawlSoftBlockBacksOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	c, fc, _ := newPipeline(t)
	u := model.URL{ID: 9, SiteID: 1, URL: srv.URL + "/page", Interval: 600}

	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchSoftBlock {
		t.Fatalf("FetchClass = %q, want soft_block", res.FetchClass)
	}
	// Reschedule must extend at least to prev*2 (1200s); the normal stable curve
	// would only grow to 600*1.5 = 900s.
	if got := fc.scheduledInterval[9]; got < 1200 {
		t.Errorf("scheduled interval = %d, want >= 1200 (extended back-off)", got)
	}
	// Retry-After (3600s) pushes next_check_at out further than prev*2.
	wantAtLeast := c.now().Add(3500 * time.Second)
	if fc.scheduledNextAt[9].Before(wantAtLeast) {
		t.Errorf("next_check_at = %v, want >= %v (Retry-After honored)", fc.scheduledNextAt[9], wantAtLeast)
	}
}

// TestCrawlContentChangeShrinksInterval guards F24: when the M2 processor detects
// a substantive change, CrawlOne must feed changed=true into RecomputeNextCheck so
// the recheck interval SHRINKS (check changing pages more often), not grows. Before
// the fix CrawlOne hardcoded changed=false, so volatile pages were checked ever less
// often — the inverse of the adaptive feature.
func TestCrawlContentChangeShrinksInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte("<html><head><title>Fresh New Title</title></head><body><p>brand new content words here today</p></body></html>"))
	}))
	defer srv.Close()

	c, fc, _ := newPipeline(t)
	// Wire a Processor so the diff/rules/alerts stage runs and reports a change.
	deps := &fakeProcDeps{}
	c.Processor = NewProcessor(deps, 4, c.Now)

	// Seed a differing prior snapshot so diff.Compare detects a substantive change.
	fc.latest[8] = model.Snapshot{
		ID: 99, URLID: 8, Title: "Old Title", ContentSHA256: "oldhash", ContentSimhash: 0x01,
		Indexable: true, HTTPStatus: 200,
	}

	u := model.URL{ID: 8, SiteID: 1, URL: srv.URL + "/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.Err != nil {
		t.Fatalf("CrawlOne err = %v", res.Err)
	}
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	// changed=true => interval shrinks toward min (600/2 clamped to 600); changed=false
	// would have grown it to 600*1.5 = 900. Assert it did NOT grow.
	if got := fc.scheduledInterval[8]; got >= 900 {
		t.Errorf("scheduled interval = %d; a detected change must shrink (not grow to >=900)", got)
	}
}

// fakeFetcher returns a fixed Result, ignoring the request. Used by F11 to
// exercise the "FetchOK + 5xx body" path (a real fetcher's Classify would never
// hand back FetchOK alongside a 500, so the seam is faked here).
type fakeFetcher struct {
	res fetcher.Result
}

func (f *fakeFetcher) Fetch(ctx context.Context, req fetcher.Request) (fetcher.Result, error) {
	return f.res, nil
}
func (f *fakeFetcher) AllowsPrivate() bool { return true }

// TestCrawl5xxBacksOffHostF11 guards F11: a plain 5xx that Classify labels FetchOK
// (it has a body, so the status_regression rule must still fire) must nonetheless
// BACK THE HOST OFF — the reschedule must take the extended backoffSchedule (>= 2x
// prior interval), not the normal growth curve. Before the fix isBackoffClass(FetchOK)
// was false, so a 500 took the growth curve (600 -> 900) and the throttle DECREASED,
// i.e. a failing origin got crawled MORE often. The snapshot/process path must still
// run (the regression rule depends on it), so we also assert the snapshot was saved.
func TestCrawl5xxBacksOffHostF11(t *testing.T) {
	c, fc, getSaved := newPipeline(t)
	// Swap in a fake fetcher: FetchOK + HTTP 500 + non-empty body.
	c.Fetcher = &fakeFetcher{res: fetcher.Result{
		FetchClass: model.FetchOK,
		HTTPStatus: 500,
		Body:       []byte("<html><head><title>Oops</title></head><body><p>internal server error words here today now</p></body></html>"),
		Header:     http.Header{},
	}}

	// Use an unresolvable host so the real RobotsCache fails open (Allowed==true)
	// without a network round-trip (mirrors TestCrawlFrontierSkipAdvancesSchedule).
	u := model.URL{ID: 11, SiteID: 1, URL: "https://e.com/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")

	// (b) The snapshot/process path must still have run — the status_regression
	// rule depends on the FetchOK extract+snapshot branch staying intact.
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok (must not be reclassified)", res.FetchClass)
	}
	if _, ok := getSaved(11); !ok {
		t.Fatal("snapshot must still be saved for a FetchOK 5xx (status path intact)")
	}

	// (a) The host must be backed off: the reschedule must take backoffSchedule
	// (>= prev*2 = 1200s), not the growth curve (600*1.5 = 900s).
	if got := fc.scheduledInterval[11]; got < 1200 {
		t.Errorf("scheduled interval = %d, want >= 1200 (5xx must take extended back-off, not growth curve)", got)
	}
}

// TestCrawlHonorsCrawlDelayF28 guards F28: CrawlOne must install the robots.txt
// Crawl-delay as a per-host minimum spacing floor (via Frontier.SetMinInterval)
// BEFORE acquiring the slot. We seed a real RobotsCache from an httptest server
// advertising "Crawl-delay: 7" and use a real Frontier, then assert the frontier's
// effective spacing for the host is raised to >= 7s. effectiveInterval is package
// private to frontier, so the assertion lives in frontier_floor_test.go via an
// exported test helper; here we drive the wiring end-to-end.
func TestCrawlHonorsCrawlDelayF28(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nCrawl-delay: 7\n"))
			return
		}
		_, _ = w.Write([]byte("<html><head><title>Hi</title></head><body><p>content words here today now</p></body></html>"))
	}))
	defer srv.Close()

	c, _, _ := newPipeline(t)
	// Real RobotsCache (so CrawlDelay > 0) + real Frontier with a generous base so
	// the 7s floor is what raises the spacing (PerHostRate would otherwise dominate).
	c.Robots = frontier.NewRobotsCache(srv.Client(), "Rabbot-SEO/test", time.Minute)
	c.Frontier = frontier.New(frontier.Options{PerHostRate: time.Millisecond, PerHostConcurrency: 4})

	host := urlx.Host(srv.URL + "/page")
	u := model.URL{ID: 28, SiteID: 1, URL: srv.URL + "/page", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "")
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}

	// effectiveInterval is package private to frontier, so we assert the floor is
	// in effect through observable behavior: CrawlOne already consumed this host's
	// one rate token, so a fresh Acquire must now block on the 7s spacing floor.
	// With a 200ms-deadline context it must FAIL (DeadlineExceeded). Without the
	// floor (1ms base) the same Acquire would succeed almost instantly. This proves
	// CrawlOne installed the Crawl-delay via Frontier.SetMinInterval.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rel, err := c.Frontier.Acquire(ctx, host)
	rel()
	if err == nil {
		t.Errorf("second Acquire on a 7s-Crawl-delay host succeeded within 200ms; the floor was not applied")
	}
}

// fakeExtractor returns a fixed snapshot/error, ignoring its input. Used to drive
// the extract-failure path deterministically (e.g. ErrDOMTooDeep).
type fakeExtractor struct {
	snap  model.Snapshot
	links []string
	err   error
}

func (f *fakeExtractor) Extract(res fetcher.Result, contentSelector string) (model.Snapshot, []string, error) {
	return f.snap, f.links, f.err
}

// ExtractWith satisfies the A8 extract.Extractor seam; the fake ignores its
// Options (it returns a fixed snapshot/error to drive the failure path).
func (f *fakeExtractor) ExtractWith(res fetcher.Result, opts extract.Options) (model.Snapshot, []string, error) {
	return f.snap, f.links, f.err
}

// TestCrawlDeepDOMDegradesWithWarn guards the Med deep-DOM finding: an HTML page
// whose DOM nests deeper than the parser limit fails extraction with ErrDOMTooDeep.
// Before the fix the error was rescheduled as a generic transient error with no
// distinct signal, so the page became an invisible blind spot — fetched forever with
// no snapshot and no operator notice. CrawlOne must instead emit a distinct WARN
// (reason=unparseable_dom) and must not panic.
func TestCrawlDeepDOMDegradesWithWarn(t *testing.T) {
	c, _, getSaved := newPipeline(t)
	// A FetchOK 200 with a non-empty body so the extract branch runs.
	c.Fetcher = &fakeFetcher{res: fetcher.Result{
		FetchClass: model.FetchOK,
		HTTPStatus: 200,
		Body:       []byte("<html><head><title>Deep</title></head><body>too deep</body></html>"),
		Header:     http.Header{},
	}}
	// Extractor fails with a wrapped ErrDOMTooDeep (errors.Is-able, per Wave 1).
	c.Extractor = &fakeExtractor{err: fmt.Errorf("extract: parse failed: %w", extract.ErrDOMTooDeep)}

	var buf bytes.Buffer
	c.Logger = obs.NewLogger(&buf, "info")

	// Unresolvable host so the real RobotsCache fails open without a network hop.
	u := model.URL{ID: 66, SiteID: 1, URL: "https://e.com/deep", Interval: 600}
	res := c.CrawlOne(context.Background(), u, 600, 86400, "") // must not panic
	if res.FetchClass != model.FetchOK {
		t.Fatalf("FetchClass = %q, want ok", res.FetchClass)
	}
	if _, ok := getSaved(66); ok {
		t.Error("a deep-DOM extract failure must persist no snapshot")
	}
	out := buf.String()
	if !strings.Contains(out, "unparseable_dom") {
		t.Errorf("expected a distinct WARN with reason=unparseable_dom; got:\n%s", out)
	}
	if !strings.Contains(out, u.URL) {
		t.Errorf("the deep-DOM WARN must name the URL %q; got:\n%s", u.URL, out)
	}
}

type fakeCrawlStore struct {
	saved             map[int64]model.Snapshot
	latest            map[int64]model.Snapshot
	updated           map[int64]model.FetchClass
	scheduledInterval map[int64]int64
	scheduledNextAt   map[int64]time.Time
	scheduledETag     map[int64]string
}

func (f *fakeCrawlStore) SaveSnapshot(ctx context.Context, snap model.Snapshot) (int64, error) {
	f.saved[snap.URLID] = snap
	f.latest[snap.URLID] = snap
	return snap.URLID, nil
}
func (f *fakeCrawlStore) LatestSnapshot(ctx context.Context, urlID int64) (model.Snapshot, error) {
	s, ok := f.latest[urlID]
	if !ok {
		return model.Snapshot{}, nil
	}
	return s, nil
}
func (f *fakeCrawlStore) RecordChanges(ctx context.Context, changes []model.Change) error { return nil }
func (f *fakeCrawlStore) GetSite(ctx context.Context, id int64) (model.Site, error) {
	return model.Site{ID: id}, nil
}
func (f *fakeCrawlStore) UpdateURLSchedule(ctx context.Context, id int64, nextCheckAt time.Time, interval int64, lastFetch model.FetchClass, etag, lastModified string) error {
	f.updated[id] = lastFetch
	f.scheduledInterval[id] = interval
	f.scheduledNextAt[id] = nextCheckAt
	f.scheduledETag[id] = etag
	return nil
}
