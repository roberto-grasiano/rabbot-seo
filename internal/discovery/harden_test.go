package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// fakeFetcher is an in-memory fetcher.Fetcher: it serves canned bodies keyed by
// request URL with no network I/O, and reports a fixed AllowsPrivate() verdict.
// It lets the SSRF / page-cap / truncation paths be exercised deterministically —
// in particular with AllowsPrivate()==false, which cannot dial a loopback httptest
// server (the SSRF guard would reject 127.0.0.1), so a real server is unusable for
// those cases.
type fakeFetcher struct {
	allowPrivate bool
	bodies       map[string][]byte // url -> body
	truncated    map[string]bool   // url -> Truncated flag on the Result
}

func (f *fakeFetcher) AllowsPrivate() bool { return f.allowPrivate }

func (f *fakeFetcher) Fetch(_ context.Context, req fetcher.Request) (fetcher.Result, error) {
	body, ok := f.bodies[req.URL]
	if !ok {
		// Mimic a real miss: unreachable, empty body. SeedSitemaps skips these.
		return fetcher.Result{FetchClass: model.FetchUnreachable}, nil
	}
	return fetcher.Result{
		FetchClass: model.FetchOK,
		Body:       body,
		Truncated:  f.truncated[req.URL],
		HTTPStatus: http.StatusOK,
	}, nil
}

// errRoundTripper makes every robots.txt fetch fail instantly. The fake-fetcher
// tests use non-resolving hosts (foo.example), so a real RobotsCache client would
// attempt live DNS; this keeps them hermetic and fast. A robots transport error
// is "no rules / no Sitemap:" -> SeedSitemaps takes the <base>/sitemap.xml
// fallback and Allowed() returns true (fail-open), which is what these tests want.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("robots fetch disabled in test")
}

func noNetRobots() *frontier.RobotsCache {
	return frontier.NewRobotsCache(&http.Client{Transport: errRoundTripper{}}, "kb/test", time.Minute)
}

func urlset(locs ...string) []byte {
	var b strings.Builder
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, l := range locs {
		b.WriteString(`<url><loc>` + l + `</loc></url>`)
	}
	b.WriteString(`</urlset>`)
	return []byte(b.String())
}

// newFakeDisc builds a Discoverer whose Pages/Sitemaps are a single fakeFetcher
// and whose Robots cache (real, but never reached for fake-served hosts) returns
// no Sitemap: directive, forcing the <base>/sitemap.xml fallback.
func newFakeDisc(t *testing.T, st *store.DB, caps Caps, allowPrivate bool, bodies map[string][]byte) *Discoverer {
	t.Helper()
	f := &fakeFetcher{allowPrivate: allowPrivate, bodies: bodies, truncated: map[string]bool{}}
	return &Discoverer{
		Store: st, Pages: f, Sitemaps: f,
		Robots:  noNetRobots(),
		Resolve: func(model.Site) Caps { return caps },
		Now:     func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
}

// (a) SSRF reject through SeedSitemaps: with AllowsPrivate()==false, a urlset
// listing a benign same-host http URL plus a same-host non-http (file://) URL
// must seed only the benign one. The file:// loc shares the site host (so it
// passes admit's same-host gate) and is rejected by admit's ValidateSiteURL
// branch on its scheme — exactly the branch this test must execute.
//
// Note: a three-slash file:///etc/passwd has an EMPTY host, so it would be
// dropped one gate earlier (same-host), never reaching ValidateSiteURL. To prove
// the ValidateSiteURL branch fires we give the hostile loc the site's host.
func TestSeedSitemapsRejectsSSRFLoc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "ssrf.db"))
	defer func() { _ = st.Close() }()

	const base = "http://benign.example"
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: base, Name: "B", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: base, MinInterval: 600}

	benign := base + "/ok"
	hostile := "file://benign.example/etc/passwd" // same host, non-http scheme
	bodies := map[string][]byte{
		base + "/sitemap.xml": urlset(benign, hostile),
	}
	d := newFakeDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000}, false, bodies)
	if d.Sitemaps.AllowsPrivate() {
		t.Fatal("precondition: fetcher must report AllowsPrivate()==false")
	}

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 1 {
		t.Errorf("added=%d, want 1 (only the benign loc admitted)", added)
	}
	if _, err := st.GetURL(ctx, sid, benign); err != nil {
		t.Errorf("benign loc not seeded: %v", err)
	}
	if _, err := st.GetURL(ctx, sid, hostile); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("hostile file:// loc must be rejected (ValidateSiteURL); GetURL=%v, want ErrNotFound", err)
	}
}

// (a) SSRF reject through EnqueueLinks: with AllowsPrivate()==false, a same-host
// non-http link must be dropped by admit's ValidateSiteURL branch while a benign
// same-host http link is admitted. EnqueueLinks never dials, so the real fetcher
// (AllowPrivate:false) is fine here.
func TestEnqueueLinksDropsNonHTTPUnderNoPrivate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "ssrf_links.db"))
	defer func() { _ = st.Close() }()

	const base = "http://benign.example"
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: base, Name: "B", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: base, MinInterval: 600}
	parent := model.URL{ID: 1, SiteID: sid, URL: base + "/", Depth: 0}

	f := fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: false})
	d := &Discoverer{
		Store: st, Pages: f, Sitemaps: f,
		Robots:  noNetRobots(),
		Resolve: func(model.Site) Caps { return Caps{FollowLinks: true, MaxDepth: 3, MaxPages: 2000} },
		Now:     func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	if d.Pages.AllowsPrivate() {
		t.Fatal("precondition: fetcher must report AllowsPrivate()==false")
	}

	benign := base + "/ok"
	hostile := "file://benign.example/etc/passwd" // same host, non-http scheme
	added, err := d.EnqueueLinks(ctx, site, parent, []string{benign, hostile})
	if err != nil {
		t.Fatalf("EnqueueLinks: %v", err)
	}
	if added != 1 {
		t.Errorf("added=%d, want 1 (hostile non-http link dropped)", added)
	}
	if _, err := st.GetURL(ctx, sid, benign); err != nil {
		t.Errorf("benign link not added: %v", err)
	}
	if _, err := st.GetURL(ctx, sid, hostile); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("hostile non-http link must be dropped; GetURL=%v, want ErrNotFound", err)
	}
}

// (b) dedup must NOT reschedule: re-enqueueing an already-known URL adds nothing
// AND must not rewrite its row (which would reset next_check_at). MaxPages is set
// high so the cap cannot mask the dedup. We capture NextCheckAt of /a after the
// first seed and assert it is byte-identical after a redundant EnqueueLinks.
func TestEnqueueLinksDedupDoesNotReschedule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "dedup.db"))
	defer func() { _ = st.Close() }()

	const base = "http://benign.example"
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: base, Name: "B", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: base, MinInterval: 600}
	parent := model.URL{ID: 1, SiteID: sid, URL: base + "/", Depth: 0}

	// MaxPages deliberately high (1000) so the cap never short-circuits the dedup.
	f := fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: false})
	d := &Discoverer{
		Store: st, Pages: f, Sitemaps: f,
		Robots:  noNetRobots(),
		Resolve: func(model.Site) Caps { return Caps{FollowLinks: true, MaxDepth: 3, MaxPages: 1000} },
		Now:     func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}

	a := base + "/a"
	if n, err := d.EnqueueLinks(ctx, site, parent, []string{a}); err != nil || n != 1 {
		t.Fatalf("first EnqueueLinks: n=%d err=%v, want n=1", n, err)
	}
	first, err := st.GetURL(ctx, sid, a)
	if err != nil {
		t.Fatalf("GetURL /a after seed: %v", err)
	}
	nextBefore := first.NextCheckAt

	// Re-enqueue the same URL: must add 0 and must not touch the existing row.
	if n, err := d.EnqueueLinks(ctx, site, parent, []string{a}); err != nil || n != 0 {
		t.Fatalf("dedup EnqueueLinks: n=%d err=%v, want n=0", n, err)
	}
	after, err := st.GetURL(ctx, sid, a)
	if err != nil {
		t.Fatalf("GetURL /a after dedup: %v", err)
	}
	if !after.NextCheckAt.Equal(nextBefore) {
		t.Errorf("dedup rescheduled /a: NextCheckAt before=%v after=%v (must be unchanged)", nextBefore, after.NextCheckAt)
	}
}

// (c) page-cap stop + log: a single child sitemap lists MORE locs than remaining
// budget. Exactly the budget seeds, the rest are ErrNotFound, and the "hit page
// cap" INFO line fires. A bytes.Buffer-backed slog.Logger captures the line.
func TestSeedSitemapsPageCapStopsAndLogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "cap.db"))
	defer func() { _ = st.Close() }()

	const base = "http://benign.example"
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: base, Name: "B", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: base, MinInterval: 600}

	// Six locs with descending priority so admit order is deterministic.
	locs := []string{base + "/p1", base + "/p2", base + "/p3", base + "/p4", base + "/p5", base + "/p6"}
	var sb strings.Builder
	sb.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for i, l := range locs {
		// priority 0.9, 0.8, ... so p1 is highest.
		sb.WriteString(`<url><loc>` + l + `</loc><priority>0.` + string(rune('9'-i)) + `</priority></url>`)
	}
	sb.WriteString(`</urlset>`)
	bodies := map[string][]byte{base + "/sitemap.xml": []byte(sb.String())}

	// Pre-seed the base URL so CountSiteURLs starts at 1. With MaxPages=3 the
	// remaining budget is 2, while the bounded `pages` slice still carries 3 entries
	// — so the admit loop fills 2 and then hits the cap with a surplus entry left,
	// which is exactly the condition that fires the "hit page cap" log line.
	if _, err := st.UpsertURL(ctx, model.URL{SiteID: sid, URL: base + "/", FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC(), Interval: 600}); err != nil {
		t.Fatalf("UpsertURL base: %v", err)
	}

	d := newFakeDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 3}, false, bodies)
	var buf bytes.Buffer
	d.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Errorf("added=%d, want 2 (base counts 1, MaxPages=3 → 2 remaining)", added)
	}
	for _, l := range locs[:2] {
		if _, err := st.GetURL(ctx, sid, l); err != nil {
			t.Errorf("%s should be seeded: %v", l, err)
		}
	}
	for _, l := range locs[2:] {
		if _, err := st.GetURL(ctx, sid, l); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s should be over-budget (ErrNotFound), got %v", l, err)
		}
	}
	if !strings.Contains(buf.String(), "hit page cap") {
		t.Errorf("expected a \"hit page cap\" log line; got: %s", buf.String())
	}
}

// (e) gzip THROUGH SeedSitemaps: the sitemap handler serves a gzip-compressed
// urlset as application/gzip with NO Content-Encoding (so the transport does not
// transparently inflate it). ParseSitemap's magic-byte gunzip must kick in and
// the inner locs must seed. This uses a real loopback server (AllowPrivate:true).
func TestSeedSitemapsGzipThroughPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	// No Sitemap: directive -> forces the <base>/sitemap.xml fallback.
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(urlset(srv.URL+"/g1", srv.URL+"/g2"))
		_ = zw.Close()
		_, _ = w.Write(gz.Bytes())
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "gz.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "G", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Errorf("added=%d, want 2 (inner gzip locs)", added)
	}
	if _, err := st.GetURL(ctx, sid, srv.URL+"/g1"); err != nil {
		t.Errorf("g1 not seeded from gzip sitemap: %v", err)
	}
	if _, err := st.GetURL(ctx, sid, srv.URL+"/g2"); err != nil {
		t.Errorf("g2 not seeded from gzip sitemap: %v", err)
	}
}

// (f) scope vs cap: an external (different-host) loc must be excluded by scope,
// NOT merely by the page cap. We set MaxPages high enough that the external loc
// IS reached during admit and rejected on host, asserting it lands in no store.
func TestSeedSitemapsExternalExcludedByScopeNotCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(urlset(srv.URL+"/in1", "https://external.example.com/x", srv.URL+"/in2"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "scope.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "S", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	// MaxPages=100: well above the 2 same-host locs, so the external loc is reached
	// in the admit loop and rejected on host — not skipped by an exhausted budget.
	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 100})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Errorf("added=%d, want 2 (both same-host locs; external excluded by scope)", added)
	}
	if _, err := st.GetURL(ctx, sid, "https://external.example.com/x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("external loc must be excluded by scope; GetURL=%v, want ErrNotFound", err)
	}
}
