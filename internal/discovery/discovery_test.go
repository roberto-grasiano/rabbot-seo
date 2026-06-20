package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/frontier"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

// countingGate is a frontierGate stub that records one Acquire per call and the
// host it gated, so a test can prove sitemap fetches flow through the per-host
// rate limiter. Release is a no-op; the stub never blocks.
type countingGate struct {
	mu       sync.Mutex
	acquires atomic.Int64
	releases atomic.Int64
	hosts    []string
}

func (g *countingGate) Acquire(_ context.Context, host string) (func(), error) {
	g.acquires.Add(1)
	g.mu.Lock()
	g.hosts = append(g.hosts, host)
	g.mu.Unlock()
	return func() { g.releases.Add(1) }, nil
}

func newDisc(t *testing.T, st *store.DB, caps Caps) (*Discoverer, func()) {
	t.Helper()
	f := fetcher.New(fetcher.Options{UserAgent: "kb/test", Timeout: 5 * time.Second, MaxBodyBytes: 50 << 20, AllowPrivate: true})
	d := &Discoverer{
		Store: st, Pages: f, Sitemaps: f,
		Robots:  frontier.NewRobotsCache(nil, "kb/test", time.Minute),
		Resolve: func(model.Site) Caps { return caps },
		Now:     func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	return d, func() {}
}

// TestEnqueueLinksClassifiesNewURLs guards A7 slice 3 discovery classification:
// a URL newly admitted by upsert is handed to the injected Classify seam (with
// its freshly assigned urlID and URL), so its segment memberships are written at
// URL entry — no wait for a reconcile. A deduped (already-present) URL is NOT
// re-classified (upsert never runs for it).
func TestEnqueueLinksClassifiesNewURLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: "https://ex.com", MinInterval: 600}
	parent := model.URL{ID: 1, SiteID: sid, URL: "https://ex.com/", Depth: 0}

	d, done := newDisc(t, st, Caps{FollowLinks: true, MaxDepth: 3, MaxPages: 100})
	defer done()

	var mu sync.Mutex
	classified := map[string]int64{} // url -> urlID seen by the seam
	d.Classify = func(_ context.Context, siteID, urlID int64, rawURL string) error {
		mu.Lock()
		defer mu.Unlock()
		if siteID != sid {
			t.Errorf("Classify siteID = %d, want %d", siteID, sid)
		}
		classified[rawURL] = urlID
		return nil
	}

	if _, err := d.EnqueueLinks(ctx, site, parent, []string{"https://ex.com/blog/a", "https://ex.com/blog/b"}); err != nil {
		t.Fatalf("EnqueueLinks: %v", err)
	}
	if len(classified) != 2 {
		t.Fatalf("Classify should run for each newly admitted URL, got %d: %v", len(classified), classified)
	}
	for _, raw := range []string{"https://ex.com/blog/a", "https://ex.com/blog/b"} {
		if id, ok := classified[raw]; !ok || id == 0 {
			t.Errorf("Classify(%q) not called with a real urlID (got id=%d ok=%v)", raw, id, ok)
		}
	}

	// Re-enqueue an existing URL: it is deduped before upsert, so Classify must
	// NOT run again for it.
	classified = map[string]int64{}
	if n, _ := d.EnqueueLinks(ctx, site, parent, []string{"https://ex.com/blog/a"}); n != 0 {
		t.Errorf("dedup: existing URL should not be re-added, got %d", n)
	}
	if len(classified) != 0 {
		t.Errorf("deduped URL must not be re-classified, got %v", classified)
	}
}

// TestScopeAdmitsApexWWWFloor is the #6 regression: an apex-configured site whose
// canonical/links resolve to the "www." sibling (or vice-versa) must ADMIT those
// entries, not drop every one and report a 0-page site. The scope gate is an
// apex<->www-equivalent site check (urlx.SameSite), not an exact-host check
// (urlx.SameHost) — the fetcher already follows apex->www, so the homepage IS
// fetched; discovery must not then reject the whole site as off-scope. An
// unrelated host stays rejected (the floor is www-only, not eTLD+1 collapsing).
func TestScopeAdmitsApexWWWFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "floor.db"))
	defer func() { _ = st.Close() }()
	// Apex BaseURL; the discovered links are all on the www. sibling.
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: "https://example.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: "https://example.com", MinInterval: 600}
	parent := model.URL{ID: 1, SiteID: sid, URL: "https://example.com/", Depth: 0}

	// AllowPrivate is true so ValidateSiteURL accepts these public hostnames; no
	// network fetch happens — EnqueueLinks only runs the scope gate + dedup.
	d, done := newDisc(t, st, Caps{FollowLinks: true, MaxDepth: 3, MaxPages: 100})
	defer done()

	links := []string{
		"https://www.example.com/a", // www sibling of the apex base -> SAME site
		"https://www.example.com/b",
		"https://unrelated.com/x", // genuinely different site -> rejected
	}
	added, err := d.EnqueueLinks(ctx, site, parent, links)
	if err != nil {
		t.Fatalf("EnqueueLinks: %v", err)
	}
	if added != 2 {
		t.Fatalf("added=%d, want 2 (both www. links admitted under apex base; unrelated host excluded)", added)
	}
	for _, raw := range []string{"https://www.example.com/a", "https://www.example.com/b"} {
		if _, gerr := st.GetURL(ctx, sid, raw); gerr != nil {
			t.Errorf("www link %q not admitted under apex base: %v", raw, gerr)
		}
	}
	if _, gerr := st.GetURL(ctx, sid, "https://unrelated.com/x"); !errors.Is(gerr, store.ErrNotFound) {
		t.Errorf("unrelated host must stay rejected; GetURL=%v, want ErrNotFound", gerr)
	}

	// The mirror direction: a www-configured site admits an apex-host link.
	wsid, _ := st.AddSite(ctx, model.Site{BaseURL: "https://www.example.org", Name: "W", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	wsite := model.Site{ID: wsid, BaseURL: "https://www.example.org", MinInterval: 600}
	wparent := model.URL{ID: 2, SiteID: wsid, URL: "https://www.example.org/", Depth: 0}
	if n, werr := d.EnqueueLinks(ctx, wsite, wparent, []string{"https://example.org/apex"}); werr != nil || n != 1 {
		t.Fatalf("apex link under www base: added=%d err=%v, want 1, nil", n, werr)
	}
}

// TestInScopeApexWWWFloor pins the shared scope predicate both call sites use
// (the link/loc admit gate and the sitemap-BFS document gate): apex<->www are the
// same site, an unrelated host is not, and an unparseable host is rejected. This
// is the host-level unit that EnqueueLinks exercises end-to-end, asserted directly
// so the sitemap-BFS gate (which cannot use www hostnames against an IP-based
// httptest listener) is covered by the same predicate.
func TestInScopeApexWWWFloor(t *testing.T) {
	t.Parallel()
	d := &Discoverer{}
	base := "https://example.com/"
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://www.example.com/a", true},   // www sibling of apex base
		{"https://example.com/a", true},       // exact host
		{"http://www.example.com/a", true},    // scheme differs, host same site
		{"https://blog.example.com/a", false}, // unrelated subdomain (NOT eTLD+1 collapse)
		{"https://unrelated.com/x", false},    // different registrable domain
		{"://bad", false},                     // unparseable -> rejected
	}
	for _, c := range cases {
		if got := d.inScope(c.raw, base); got != c.want {
			t.Errorf("inScope(%q, %q) = %v, want %v", c.raw, base, got, c.want)
		}
	}
	// Mirror: a www base admits an apex-host loc.
	if !d.inScope("https://example.org/x", "https://www.example.org/") {
		t.Error("inScope: www base must admit apex-host loc")
	}
}

func TestEnqueueLinksScopeDepthCapDedup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: "https://ex.com", MinInterval: 600}
	parent := model.URL{ID: 1, SiteID: sid, URL: "https://ex.com/", Depth: 0}

	d, done := newDisc(t, st, Caps{FollowLinks: true, MaxDepth: 3, MaxPages: 3})
	defer done()

	// same-host kept; external dropped; over-cap (MaxPages=3, base already counts) bounded.
	links := []string{"https://ex.com/a", "https://ex.com/b", "https://other.com/x", "https://ex.com/c", "https://ex.com/d"}
	// seed the base URL so CountSiteURLs starts at 1
	_, _ = st.UpsertURL(ctx, model.URL{SiteID: sid, URL: "https://ex.com/", FirstSeen: time.Now().UTC(), NextCheckAt: time.Now().UTC(), Interval: 600})
	added, err := d.EnqueueLinks(ctx, site, parent, links)
	if err != nil {
		t.Fatalf("EnqueueLinks: %v", err)
	}
	if added != 2 { // cap=3, base=1 already -> only 2 more admitted; external excluded
		t.Errorf("added=%d, want 2 (cap-bounded, external excluded)", added)
	}
	// depth cap: a parent at MaxDepth enqueues nothing
	deep := model.URL{ID: 2, SiteID: sid, URL: "https://ex.com/a", Depth: 3}
	if n, _ := d.EnqueueLinks(ctx, site, deep, []string{"https://ex.com/z"}); n != 0 {
		t.Errorf("depth-capped parent should enqueue 0, got %d", n)
	}
	// dedup: re-enqueue an existing URL adds nothing
	if n, _ := d.EnqueueLinks(ctx, site, parent, []string{"https://ex.com/a"}); n != 0 {
		t.Errorf("dedup: existing URL should not be re-added, got %d", n)
	}
}

// TestEnqueueLinksCapZeroUnlimited pins the "cap 0 = unlimited" contract at the
// discovery admission layer: with Caps{MaxPages: 0} EVERY same-host link must be
// admitted, not zero. The bug was "count+added >= caps.MaxPages" with no >0 guard
// (0 >= 0 breaks on the first iteration), so a cap-0 site admitted nothing.
func TestEnqueueLinksCapZeroUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "ul.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: "https://ex.com", Name: "Ex", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: "https://ex.com", MinInterval: 600}
	parent := model.URL{ID: 1, SiteID: sid, URL: "https://ex.com/", Depth: 0}

	d, done := newDisc(t, st, Caps{FollowLinks: true, MaxDepth: 3, MaxPages: 0})
	defer done()

	links := []string{"https://ex.com/a", "https://ex.com/b", "https://ex.com/c", "https://ex.com/d", "https://ex.com/e"}
	added, err := d.EnqueueLinks(ctx, site, parent, links)
	if err != nil {
		t.Fatalf("EnqueueLinks: %v", err)
	}
	if added != len(links) {
		t.Errorf("cap 0 = unlimited: added=%d, want %d (every same-host link admitted)", added, len(links))
	}
}

// TestSeedSitemapsCapZeroUnlimited pins "cap 0 = unlimited" for sitemap admission:
// a sitemap larger than any positive cap must seed EVERY page when MaxPages == 0.
func TestSeedSitemapsCapZeroUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	// A urlset of 5 pages — more than any small positive cap would admit.
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		body := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`
		for _, p := range []string{"/s1", "/s2", "/s3", "/s4", "/s5"} {
			body += `<url><loc>` + srv.URL + p + `</loc></url>`
		}
		body += `</urlset>`
		_, _ = w.Write([]byte(body))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "ulsm.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "U", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 0})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 5 {
		t.Errorf("cap 0 = unlimited: added=%d, want 5 (every sitemap page seeded)", added)
	}
}

// TestSeedSitemapsRecursiveIndexViaRobotsDirective exercises the robots Sitemap:
// directive path: robots.txt points at a sitemap INDEX, which expands to a child
// sitemap of two pages. (This is NOT the hardcoded /sitemap.xml fallback — that is
// covered by TestSeedSitemapsFallbackWhenNoRobotsSitemap below.)
func TestSeedSitemapsRecursiveIndexViaRobotsDirective(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap_index.xml\n"))
	})
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<sitemap><loc>` + srv.URL + `/sm1.xml</loc></sitemap></sitemapindex>`))
	})
	mux.HandleFunc("/sm1.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/p1</loc></url><url><loc>` + srv.URL + `/p2</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "S", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Errorf("recursive index should seed 2 pages, got %d", added)
	}
	if _, err := st.GetURL(ctx, sid, srv.URL+"/p1"); err != nil {
		t.Errorf("p1 not seeded: %v", err)
	}
}

// TestSeedSitemapsFallbackWhenNoRobotsSitemap proves the hardcoded
// <base>/sitemap.xml fallback: robots.txt declares NO Sitemap: line, yet a urlset
// served at /sitemap.xml must still be discovered and its locs seeded.
func TestSeedSitemapsFallbackWhenNoRobotsSitemap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	// robots.txt with NO Sitemap: directive -> d.Robots.Sitemaps() returns empty.
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	// The fallback target.
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/f1</loc></url><url><loc>` + srv.URL + `/f2</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "fb.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "F", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Errorf("fallback should seed 2 pages, got %d", added)
	}
	if _, err := st.GetURL(ctx, sid, srv.URL+"/f1"); err != nil {
		t.Errorf("f1 not seeded via fallback: %v", err)
	}
	if _, err := st.GetURL(ctx, sid, srv.URL+"/f2"); err != nil {
		t.Errorf("f2 not seeded via fallback: %v", err)
	}
}

// TestSeedSitemapsGatesEachFetchThroughFrontier pins the per-host rate-limit gate
// on sitemap fetches (Finding Low sitemap-rate): when a Frontier is wired, the BFS
// must Acquire (and release) the per-host budget once per sitemap document it
// fetches, mirroring the page-crawl path — not issue up to maxSitemapFetches
// ungated requests to a single host. The served index expands to one child
// sitemap, so the BFS fetches exactly two documents (index + child).
func TestSeedSitemapsGatesEachFetchThroughFrontier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap_index.xml\n"))
	})
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<sitemap><loc>` + srv.URL + `/sm1.xml</loc></sitemap></sitemapindex>`))
	})
	mux.HandleFunc("/sm1.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<url><loc>` + srv.URL + `/g1</loc></url><url><loc>` + srv.URL + `/g2</loc></url></urlset>`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "gate.db"))
	defer func() { _ = st.Close() }()
	sid, _ := st.AddSite(ctx, model.Site{BaseURL: srv.URL, Name: "G", Enabled: true, MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100})
	site := model.Site{ID: sid, BaseURL: srv.URL, MinInterval: 600}

	d, done := newDisc(t, st, Caps{Sitemap: true, MaxDepth: 3, MaxPages: 2000})
	defer done()
	d.Robots = frontier.NewRobotsCache(srv.Client(), "kb/test", time.Minute)
	gate := &countingGate{}
	d.Frontier = gate

	added, err := d.SeedSitemaps(ctx, site)
	if err != nil {
		t.Fatalf("SeedSitemaps: %v", err)
	}
	if added != 2 {
		t.Fatalf("index should seed 2 pages, got %d", added)
	}
	// index + child sitemap = exactly two fetched documents -> two Acquires, each released.
	if got := gate.acquires.Load(); got != 2 {
		t.Errorf("Acquire count = %d, want 2 (one per sitemap fetch)", got)
	}
	if got := gate.releases.Load(); got != 2 {
		t.Errorf("release count = %d, want 2 (every acquired slot released)", got)
	}
	wantHost := urlx.Host(srv.URL)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, h := range gate.hosts {
		if h != wantHost {
			t.Errorf("Acquire host = %q, want %q (gate keyed by site host)", h, wantHost)
		}
	}
}
